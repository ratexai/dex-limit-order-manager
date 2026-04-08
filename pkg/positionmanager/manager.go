package positionmanager

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Manager is the main entry point of the position manager library.
// Create one via New(), then call Run() in a goroutine to start the keeper loop.
type Manager struct {
	cfg       Config
	log       *slog.Logger
	metrics   MetricsCollector
	trigger   *TriggerEngine
	executors map[uint64]*executor // chainID → executor
	posLocks  positionLockMap      // Per-position mutex to serialize executions.

	// Dynamic pair subscription state (populated by Run, used by OpenPosition).
	runCtx     context.Context    // Set when Run starts; nil before.
	pairSubsMu sync.Mutex
	pairSubs   map[uint64]map[TokenPair]bool       // chainID → set of subscribed pairs.
	execChs    map[uint64]chan TriggerEvent          // chainID → execution channel.
	subWg      map[uint64]*sync.WaitGroup            // chainID → WaitGroup for subscription goroutines.
}

// positionLockMap provides per-position mutual exclusion so that concurrent
// trigger executions for the same position are serialized.
type positionLockMap struct {
	mu    sync.Mutex
	locks map[[16]byte]*sync.Mutex
}

func (m *positionLockMap) Lock(id [16]byte) {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[[16]byte]*sync.Mutex)
	}
	lk, ok := m.locks[id]
	if !ok {
		lk = &sync.Mutex{}
		m.locks[id] = lk
	}
	m.mu.Unlock()
	lk.Lock()
}

func (m *positionLockMap) Unlock(id [16]byte) {
	m.mu.Lock()
	lk := m.locks[id]
	m.mu.Unlock()
	lk.Unlock()
}

// Cleanup removes mutexes for the given position IDs (e.g. closed/cancelled).
// Only call when no goroutine holds the lock for these IDs.
func (m *positionLockMap) Cleanup(ids [][16]byte) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for _, id := range ids {
		if _, ok := m.locks[id]; ok {
			delete(m.locks, id)
			removed++
		}
	}
	return removed
}

// Len returns the number of tracked position locks.
func (m *positionLockMap) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.locks)
}

// New creates a new Manager with the given configuration.
// The caller must provide Store, PriceFeed, FeeProvider, and at least one chain.
func New(cfg Config) (*Manager, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("positionmanager: Store is required")
	}
	if cfg.PriceFeed == nil {
		return nil, fmt.Errorf("positionmanager: PriceFeed is required")
	}
	if cfg.FeeProvider == nil {
		return nil, fmt.Errorf("positionmanager: FeeProvider is required")
	}
	if len(cfg.Chains) == 0 {
		return nil, fmt.Errorf("positionmanager: at least one chain is required")
	}

	executors := make(map[uint64]*executor)
	for chainID, ci := range cfg.Chains {
		if ci.Client == nil {
			return nil, fmt.Errorf("positionmanager: chain %d: Client is required", chainID)
		}
		if ci.KeeperKey == nil {
			return nil, fmt.Errorf("positionmanager: chain %d: KeeperKey is required", chainID)
		}
		executors[chainID] = newExecutor(ci.Client, ci.KeeperKey, ci.ExecutorAddress, chainID, ci.ChainConfig)
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	metrics := cfg.Metrics
	if metrics == nil {
		metrics = noopMetrics{}
	}

	return &Manager{
		cfg:       cfg,
		log:       logger,
		metrics:   metrics,
		trigger:   NewTriggerEngine(),
		executors: executors,
	}, nil
}

// Run starts the keeper loop. It blocks until ctx is cancelled.
// It loads active positions, subscribes to price feeds, and dispatches triggers.
func (m *Manager) Run(ctx context.Context) error {
	m.runCtx = ctx
	m.pairSubs = make(map[uint64]map[TokenPair]bool)
	m.execChs = make(map[uint64]chan TriggerEvent)
	m.subWg = make(map[uint64]*sync.WaitGroup)

	// Load active positions, register triggers, and collect pairs per chain.
	chainPairs := make(map[uint64][]TokenPair)
	for chainID := range m.cfg.Chains {
		positions, err := m.cfg.Store.ListActive(ctx, chainID)
		if err != nil {
			return fmt.Errorf("load active positions for chain %d: %w", chainID, err)
		}
		for _, pos := range positions {
			m.registerPositionTriggers(pos)
		}
		chainPairs[chainID] = uniquePairs(positions, chainID)

		// Initialize per-chain subscription tracking.
		m.pairSubs[chainID] = make(map[TokenPair]bool)
		for _, pair := range chainPairs[chainID] {
			m.pairSubs[chainID][pair] = true
		}
	}

	var wg sync.WaitGroup

	// Start permit expiry monitor.
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.runPermitExpiryMonitor(ctx)
	}()

	for chainID, ci := range m.cfg.Chains {
		wg.Add(1)
		go func(chainID uint64, ci ChainInstance) {
			defer wg.Done()
			m.runChain(ctx, chainID, ci, chainPairs[chainID])
		}(chainID, ci)
	}

	wg.Wait()
	return ctx.Err()
}

// runChain runs the keeper loop for a single chain with a bounded worker pool.
func (m *Manager) runChain(ctx context.Context, chainID uint64, ci ChainInstance, pairs []TokenPair) {
	workers := ci.ExecutorWorkers
	if workers <= 0 {
		workers = 4
	}

	// Buffered channel for trigger events awaiting execution.
	execCh := make(chan TriggerEvent, workers*4)

	// Store references for dynamic pair subscription from OpenPosition.
	var subWg sync.WaitGroup
	m.pairSubsMu.Lock()
	m.execChs[chainID] = execCh
	m.subWg[chainID] = &subWg
	m.pairSubsMu.Unlock()

	var wg sync.WaitGroup

	// Start execution workers.
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case evt, ok := <-execCh:
					if !ok {
						return
					}
					m.executeTrigger(ctx, evt)
				}
			}
		}()
	}

	// Subscribe to initial pairs.
	for _, pair := range pairs {
		m.subscribePair(ctx, chainID, pair, execCh, &subWg)
	}

	// Wait for all goroutines to finish.
	wg.Wait()
	subWg.Wait()
	close(execCh)
}

// subscribePair subscribes to a price feed for a pair and routes updates to execCh.
func (m *Manager) subscribePair(ctx context.Context, chainID uint64, pair TokenPair, execCh chan<- TriggerEvent, wg *sync.WaitGroup) {
	ch, err := m.cfg.PriceFeed.Subscribe(ctx, pair)
	if err != nil {
		m.emitError(ErrorEvent{ChainID: chainID, Err: fmt.Errorf("subscribe %v: %w", pair, err)})
		return
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-ch:
				if !ok {
					return
				}
				m.handlePriceUpdate(ctx, update, execCh)
			}
		}
	}()
}

// ensurePairSubscribed checks if a pair is already subscribed on a chain.
// If not, subscribes dynamically. Safe to call from OpenPosition after Run has started.
func (m *Manager) ensurePairSubscribed(chainID uint64, pair TokenPair) {
	m.pairSubsMu.Lock()
	defer m.pairSubsMu.Unlock()

	// Run hasn't started yet — triggers will be picked up when Run initializes.
	if m.pairSubs == nil {
		return
	}

	subs, ok := m.pairSubs[chainID]
	if !ok {
		return // Unknown chain.
	}
	if subs[pair] {
		return // Already subscribed.
	}

	execCh := m.execChs[chainID]
	wg := m.subWg[chainID]
	if execCh == nil || wg == nil {
		return // Chain not initialized yet.
	}

	// Mark subscribed before releasing lock to prevent race with another OpenPosition.
	subs[pair] = true

	m.log.Info("dynamic pair subscription",
		"chain", chainID,
		"base", pair.Base.Hex(),
		"quote", pair.Quote.Hex(),
	)

	// Subscribe outside the lock would be cleaner, but the PriceFeed.Subscribe
	// call should be fast. The goroutine is spawned inside subscribePair.
	m.subscribePair(m.runCtx, chainID, pair, execCh, wg)
}

// handlePriceUpdate processes a single price update by dispatching
// triggered events to the execution channel.
func (m *Manager) handlePriceUpdate(ctx context.Context, update PriceUpdate, execCh chan<- TriggerEvent) {
	events := m.trigger.OnPrice(update.Pair, update.Price)
	for _, evt := range events {
		select {
		case execCh <- evt:
		case <-ctx.Done():
			return
		}
	}
}

// executeTrigger handles a single trigger event: execute swap, update state.
// Per-position locking ensures concurrent triggers for the same position are serialized.
func (m *Manager) executeTrigger(ctx context.Context, evt TriggerEvent) {
	execStart := time.Now()

	m.posLocks.Lock(evt.PositionID)
	defer m.posLocks.Unlock(evt.PositionID)

	pos, err := m.cfg.Store.Get(ctx, evt.PositionID)
	if err != nil {
		m.metrics.IncErrorTotal(evt.ChainID, "store_get")
		m.emitError(ErrorEvent{PositionID: evt.PositionID, LevelIndex: evt.LevelIndex, ChainID: evt.ChainID, Err: err})
		return
	}

	if pos.State.IsTerminal() {
		return // Position already closed.
	}

	if evt.LevelIndex >= len(pos.Levels) {
		return
	}
	level := &pos.Levels[evt.LevelIndex]
	if level.Status != LevelActive {
		return // Already triggered or cancelled.
	}

	exec, ok := m.executors[pos.ChainID]
	if !ok {
		m.emitError(ErrorEvent{PositionID: evt.PositionID, ChainID: evt.ChainID, Err: fmt.Errorf("no executor for chain %d", pos.ChainID)})
		return
	}

	// Determine swap parameters.
	tokenIn, tokenOut := m.swapTokens(pos, level)
	amountIn := m.computeAmount(pos, level)

	// Get fee config.
	feeCfg, err := m.cfg.FeeProvider.GetFee(ctx, pos.Owner)
	if err != nil {
		m.metrics.IncErrorTotal(pos.ChainID, "get_fee")
		m.emitError(ErrorEvent{PositionID: evt.PositionID, LevelIndex: evt.LevelIndex, ChainID: evt.ChainID, Err: fmt.Errorf("get fee: %w", err)})
		return
	}

	// Determine slippage.
	slippageBps := m.cfg.Chains[pos.ChainID].TPSlippageBps
	priority := PriorityNormal
	if level.Type == LevelTypeSL {
		slippageBps = m.cfg.Chains[pos.ChainID].SLSlippageBps
		priority = PriorityCritical
	}

	// Determine token decimals based on swap direction.
	decimalsIn, decimalsOut := pos.DecimalsBase, pos.DecimalsQuote
	if pos.Direction == Short {
		decimalsIn, decimalsOut = pos.DecimalsQuote, pos.DecimalsBase
	}
	minAmountOut := computeMinAmountOut(level.TriggerPrice, amountIn, slippageBps, decimalsIn, decimalsOut)

	var feeBps uint16
	if feeCfg != nil {
		feeBps = feeCfg.FeeBps
	}

	// Check circuit breaker before executing.
	if cb := exec.circuitBreaker; cb != nil {
		if err := cb.Allow(); err != nil {
			m.metrics.IncErrorTotal(pos.ChainID, "circuit_open")
			// Re-register trigger for retry after circuit resets.
			pair := pos.Pair()
			m.trigger.Register(pair, pos.ID, evt.LevelIndex, level.Type, pos.Direction, level.TriggerPrice)
			m.emitError(ErrorEvent{PositionID: evt.PositionID, LevelIndex: evt.LevelIndex, ChainID: evt.ChainID, Err: err, Retryable: true})
			return
		}
	}

	// Determine execution mode.
	execMode := ExecModeLegacy
	if len(pos.PermitSignature) > 0 {
		// Check permit not expired.
		if pos.PermitDeadline > 0 && pos.PermitDeadline <= time.Now().Unix() {
			// Permit expired — suspend levels and notify host.
			m.suspendPositionLevels(pos)
			if err := m.cfg.Store.Update(ctx, pos); err != nil {
				m.emitError(ErrorEvent{PositionID: pos.ID, ChainID: pos.ChainID, Err: fmt.Errorf("store update: %w", err)})
			}
			m.emitError(ErrorEvent{
				PositionID: evt.PositionID,
				LevelIndex: evt.LevelIndex,
				ChainID:    evt.ChainID,
				Err:        fmt.Errorf("permit expired at %d", pos.PermitDeadline),
				Retryable:  false,
			})
			return
		}

		execMode = ExecModePermit2Allowance

		// Activate Permit2 allowance on-chain if not yet done.
		if !pos.PermitActivated {
			ci := m.cfg.Chains[pos.ChainID]
			_, err := exec.activatePermit(ctx, activatePermitParams{
				User:        pos.Owner,
				Token:       pos.PermitToken,
				Amount:      pos.PermitAmount,
				Expiration:  uint64(pos.PermitDeadline),
				Nonce:       pos.PermitNonce.Uint64(),
				Spender:     ci.ExecutorAddress,
				SigDeadline: new(big.Int).SetInt64(pos.PermitDeadline),
				Signature:   pos.PermitSignature,
				Priority:    priority,
			})
			if err != nil {
				m.metrics.IncErrorTotal(pos.ChainID, "activate_permit")
				pair := pos.Pair()
				m.trigger.Register(pair, pos.ID, evt.LevelIndex, level.Type, pos.Direction, level.TriggerPrice)
				m.emitError(ErrorEvent{
					PositionID: evt.PositionID,
					LevelIndex: evt.LevelIndex,
					ChainID:    evt.ChainID,
					Err:        fmt.Errorf("activate permit: %w", err),
					Retryable:  true,
				})
				return
			}
			pos.PermitActivated = true
			pos.UpdatedAt = time.Now().Unix()
			if err := m.cfg.Store.Update(ctx, pos); err != nil {
				m.log.Error("failed to persist permit activation", "error", err)
			}
		}
	}

	// Execute on-chain swap and wait for receipt.
	txHash, receipt, err := exec.executeSwap(ctx, executeSwapParams{
		User:         pos.Owner,
		TokenIn:      tokenIn,
		TokenOut:     tokenOut,
		PoolFee:      pos.PoolFee,
		AmountIn:     amountIn,
		MinAmountOut: minAmountOut,
		FeeBps:       feeBps,
		Priority:     priority,
		Mode:         execMode,
	})
	if err != nil {
		if exec.circuitBreaker != nil {
			exec.circuitBreaker.RecordFailure()
		}
		m.metrics.IncErrorTotal(pos.ChainID, "execute_swap")
		m.log.Error("swap execution failed",
			"position", fmt.Sprintf("%x", evt.PositionID[:8]),
			"level", evt.LevelIndex,
			"type", level.Type.String(),
			"chain", pos.ChainID,
			"error", err,
		)

		// Resync nonce on "nonce too low" type errors.
		if strings.Contains(err.Error(), "nonce") {
			if resyncErr := exec.resyncNonce(ctx); resyncErr != nil {
				m.log.Error("nonce resync failed", "chain", pos.ChainID, "error", resyncErr)
			}
		}

		// Re-register the trigger so it fires again on the next price update.
		pair := pos.Pair()
		m.trigger.Register(pair, pos.ID, evt.LevelIndex, level.Type, pos.Direction, level.TriggerPrice)

		m.emitError(ErrorEvent{
			PositionID: evt.PositionID,
			LevelIndex: evt.LevelIndex,
			ChainID:    evt.ChainID,
			Err:        fmt.Errorf("execute swap: %w", err),
			Retryable:  true,
		})
		return
	}

	if exec.circuitBreaker != nil {
		exec.circuitBreaker.RecordSuccess()
	}

	// Record metrics for successful execution.
	m.metrics.IncTriggerCount(pos.ChainID, level.Type.String(), pos.Direction.String())
	m.metrics.ObserveExecutionLatency(pos.ChainID, level.Type.String(), time.Since(execStart))
	if receipt != nil {
		gasPrice := uint64(0)
		if receipt.EffectiveGasPrice != nil {
			gasPrice = receipt.EffectiveGasPrice.Uint64()
		}
		m.metrics.ObserveGasSpent(pos.ChainID, level.Type.String(), receipt.GasUsed, gasPrice)
	}

	// Parse actual amountOut from receipt logs.
	actualAmountOut := parseAmountOutFromReceipt(receipt, exec.executorAddr)
	if actualAmountOut == nil {
		actualAmountOut = minAmountOut // Fallback if event parsing fails.
	}

	// Build new state in memory (but do NOT touch trigger engine yet).
	now := time.Now().Unix()
	level.Status = LevelTriggered
	level.ExecTxHash = txHash
	level.ExecPrice = new(big.Int).Set(evt.Price)
	level.ExecAmount = amountIn
	level.ExecAt = now

	pos.RemainingSize = new(big.Int).Sub(pos.RemainingSize, amountIn)
	if pos.RemainingSize.Sign() <= 0 {
		pos.RemainingSize = new(big.Int)
	}
	pos.UpdatedAt = now

	// Compute new position state and which levels to cancel.
	pair := pos.Pair()
	var levelsToCancelInTrigger []int // Indices to unregister from trigger engine AFTER persist.
	var slToMove *struct {            // SL price update to apply AFTER persist.
		index     int
		levelType LevelType
		newPrice  *big.Int
	}

	if level.Type == LevelTypeSL {
		// SL fired → cancel all other active levels, close position.
		for i := range pos.Levels {
			if i != evt.LevelIndex && pos.Levels[i].Status == LevelActive {
				pos.Levels[i].Status = LevelCancelled
				levelsToCancelInTrigger = append(levelsToCancelInTrigger, i)
			}
		}
		pos.State = StateClosed
	} else {
		// TP fired.
		for _, cancelIdx := range level.CancelOnFire {
			if cancelIdx < len(pos.Levels) && pos.Levels[cancelIdx].Status == LevelActive {
				pos.Levels[cancelIdx].Status = LevelCancelled
				levelsToCancelInTrigger = append(levelsToCancelInTrigger, cancelIdx)
			}
		}

		if level.MoveSLTo != nil && level.MoveSLTo.Sign() > 0 {
			if sl := pos.ActiveSL(); sl != nil {
				sl.TriggerPrice = new(big.Int).Set(level.MoveSLTo)
				slToMove = &struct {
					index     int
					levelType LevelType
					newPrice  *big.Int
				}{sl.Index, sl.Type, new(big.Int).Set(level.MoveSLTo)}
			}
		}

		hasActive := false
		for _, l := range pos.Levels {
			if l.Status == LevelActive {
				hasActive = true
				break
			}
		}
		if !hasActive || pos.RemainingSize.Sign() <= 0 {
			pos.State = StateClosed
		} else {
			pos.State = StatePartialClosed
		}
	}

	// Persist BEFORE modifying trigger engine. Retry on failure because the
	// on-chain swap already happened and the state MUST be recorded.
	if err := m.storeUpdateWithRetry(ctx, pos, 3); err != nil {
		m.metrics.IncErrorTotal(pos.ChainID, "store_update")
		m.log.Error("CRITICAL: store update failed after on-chain swap — manual intervention may be needed",
			"position", fmt.Sprintf("%x", pos.ID[:8]),
			"tx", txHash.Hex(),
			"error", err,
		)
		m.emitError(ErrorEvent{PositionID: pos.ID, ChainID: pos.ChainID, Err: fmt.Errorf("store update after swap: %w", err)})
		// Do NOT update trigger engine — next Run restart will re-load from DB
		// and the swap will fail on-chain (tokens already moved), which is safe.
		return
	}

	// Persist succeeded — now safe to update in-memory trigger engine.
	for _, idx := range levelsToCancelInTrigger {
		m.trigger.Unregister(pair, pos.ID, idx)
	}
	if slToMove != nil {
		m.trigger.UpdateTriggerPrice(pair, pos.ID, slToMove.index, slToMove.levelType, pos.Direction, slToMove.newPrice)
	}

	m.log.Info("level executed",
		"position", fmt.Sprintf("%x", pos.ID[:8]),
		"level", evt.LevelIndex,
		"type", level.Type.String(),
		"chain", pos.ChainID,
		"tx", txHash.Hex(),
		"amountIn", amountIn.String(),
		"amountOut", actualAmountOut.String(),
		"state", pos.State.String(),
	)

	// Emit execution event to host.
	if m.cfg.OnExecution != nil {
		slMovedTo := new(big.Int)
		if level.MoveSLTo != nil {
			slMovedTo = level.MoveSLTo
		}
		m.cfg.OnExecution(ExecutionEvent{
			PositionID:    pos.ID,
			Owner:         pos.Owner,
			ChainID:       pos.ChainID,
			LevelIndex:    evt.LevelIndex,
			LevelType:     level.Type,
			Direction:     pos.Direction,
			TokenIn:       tokenIn,
			TokenOut:      tokenOut,
			AmountIn:      amountIn,
			AmountOut:     actualAmountOut,
			ExecPrice:     evt.Price,
			TxHash:        txHash,
			Fee:           computeFeeResult(amountIn, feeCfg),
			RemainingSize: new(big.Int).Set(pos.RemainingSize),
			PositionState: pos.State,
			SLMovedTo:     slMovedTo,
		})
	}
}

// --- CRUD operations ---

// OpenPosition creates a new position with the given parameters.
func (m *Manager) OpenPosition(ctx context.Context, params OpenParams) (*Position, error) {
	if params.Size == nil || params.Size.Sign() <= 0 {
		return nil, fmt.Errorf("invalid size")
	}
	if len(params.Levels) == 0 {
		return nil, fmt.Errorf("at least one level is required")
	}
	if _, ok := m.cfg.Chains[params.ChainID]; !ok {
		return nil, fmt.Errorf("unsupported chain %d", params.ChainID)
	}

	// Validate levels.
	var totalPortionBps uint32
	for i, lp := range params.Levels {
		if lp.TriggerPrice == nil || lp.TriggerPrice.Sign() <= 0 {
			return nil, fmt.Errorf("level %d: invalid trigger price", i)
		}
		if lp.PortionBps == 0 || lp.PortionBps > 10000 {
			return nil, fmt.Errorf("level %d: invalid portion %d bps", i, lp.PortionBps)
		}
		totalPortionBps += uint32(lp.PortionBps)
		for _, idx := range lp.CancelOnFire {
			if idx < 0 || idx >= len(params.Levels) {
				return nil, fmt.Errorf("level %d: CancelOnFire index %d out of range", i, idx)
			}
		}
	}
	if totalPortionBps > 10000 {
		return nil, fmt.Errorf("total PortionBps %d exceeds 10000", totalPortionBps)
	}

	// Validate Permit2 authorization if provided.
	var permitToken common.Address
	if len(params.PermitSignature) > 0 {
		ci := m.cfg.Chains[params.ChainID]

		// Determine expected tokenIn based on direction.
		if params.Direction == Long {
			permitToken = params.TokenBase
		} else {
			permitToken = params.TokenQuote
		}

		permitData := PermitSingleData{
			Token:       permitToken,
			Amount:      params.Size,
			Expiration:  uint64(params.PermitDeadline),
			Nonce:       0,
			Spender:     ci.ExecutorAddress,
			SigDeadline: new(big.Int).SetInt64(params.PermitDeadline),
		}
		if params.PermitNonce != nil {
			permitData.Nonce = params.PermitNonce.Uint64()
		}

		if err := ValidatePermitForPosition(
			params.Owner,
			params.Size,
			permitToken,
			permitData,
			params.ChainID,
			ci.ChainConfig.Permit2Address,
			ci.ExecutorAddress,
			params.PermitSignature,
			ci.ChainConfig.MinPermitLifetime,
		); err != nil {
			return nil, fmt.Errorf("permit validation: %w", err)
		}
	}

	// Broadcast signed approve TX if provided (one-click flow).
	if len(params.SignedApproveTx) > 0 {
		exec, ok := m.executors[params.ChainID]
		if !ok {
			return nil, fmt.Errorf("unsupported chain %d", params.ChainID)
		}
		if _, err := exec.broadcastSignedApproveTx(ctx, params.SignedApproveTx); err != nil {
			return nil, fmt.Errorf("broadcast approve tx: %w", err)
		}
	}

	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, fmt.Errorf("generate ID: %w", err)
	}

	now := time.Now().Unix()
	pos := &Position{
		ID:            id,
		Owner:         params.Owner,
		TokenBase:     params.TokenBase,
		TokenQuote:    params.TokenQuote,
		Direction:     params.Direction,
		TotalSize:     new(big.Int).Set(params.Size),
		RemainingSize: new(big.Int).Set(params.Size),
		EntryPrice:    new(big.Int).Set(params.EntryPrice),
		State:         StateActive,
		ChainID:       params.ChainID,
		PoolFee:       params.PoolFee,
		DecimalsBase:  params.DecimalsBase,
		DecimalsQuote: params.DecimalsQuote,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	for i, lp := range params.Levels {
		level := Level{
			Index:        i,
			Type:         lp.Type,
			TriggerPrice: new(big.Int).Set(lp.TriggerPrice),
			PortionBps:   lp.PortionBps,
			Status:       LevelActive,
			CancelOnFire: lp.CancelOnFire,
		}
		if lp.MoveSLTo != nil {
			level.MoveSLTo = new(big.Int).Set(lp.MoveSLTo)
		}
		pos.Levels = append(pos.Levels, level)
	}

	// Store Permit2 data on position if provided.
	if len(params.PermitSignature) > 0 {
		pos.PermitSignature = params.PermitSignature
		pos.PermitDeadline = params.PermitDeadline
		pos.PermitAmount = new(big.Int).Set(params.Size)
		pos.PermitToken = permitToken
		if params.PermitNonce != nil {
			pos.PermitNonce = new(big.Int).Set(params.PermitNonce)
		} else {
			pos.PermitNonce = new(big.Int)
		}
	}

	if err := m.cfg.Store.Save(ctx, pos); err != nil {
		return nil, fmt.Errorf("save position: %w", err)
	}

	m.registerPositionTriggers(pos)

	// Subscribe to the pair's price feed if not already subscribed (dynamic subscription).
	m.ensurePairSubscribed(pos.ChainID, pos.Pair())

	return pos, nil
}

// GetPosition returns a position by ID.
func (m *Manager) GetPosition(ctx context.Context, id [16]byte) (*Position, error) {
	return m.cfg.Store.Get(ctx, id)
}

// ListPositions returns positions for an owner, optionally filtered by state.
func (m *Manager) ListPositions(ctx context.Context, owner common.Address, states ...PositionState) ([]*Position, error) {
	return m.cfg.Store.GetByOwner(ctx, owner, states...)
}

// CancelPosition cancels a position and all its active levels.
// No on-chain transaction needed — just update the DB.
func (m *Manager) CancelPosition(ctx context.Context, id [16]byte) error {
	pos, err := m.cfg.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	if pos.State.IsTerminal() {
		return fmt.Errorf("position already %s", pos.State)
	}

	pair := pos.Pair()
	m.cancelActiveLevels(pos, pair, -1)
	pos.State = StateCancelled
	pos.UpdatedAt = time.Now().Unix()
	return m.cfg.Store.Update(ctx, pos)
}

// UpdateLevel changes the trigger price of a level. Zero gas — off-chain only.
func (m *Manager) UpdateLevel(ctx context.Context, posID [16]byte, levelIdx int, newTriggerPrice *big.Int) error {
	if newTriggerPrice == nil || newTriggerPrice.Sign() <= 0 {
		return fmt.Errorf("invalid trigger price")
	}
	pos, err := m.cfg.Store.Get(ctx, posID)
	if err != nil {
		return err
	}
	if pos.State.IsTerminal() {
		return fmt.Errorf("position is %s", pos.State)
	}
	if levelIdx < 0 || levelIdx >= len(pos.Levels) {
		return fmt.Errorf("invalid level index %d", levelIdx)
	}
	level := &pos.Levels[levelIdx]
	if level.Status != LevelActive {
		return fmt.Errorf("level %d is %v, not active", levelIdx, level.Status)
	}

	pair := pos.Pair()
	level.TriggerPrice = new(big.Int).Set(newTriggerPrice)
	pos.UpdatedAt = time.Now().Unix()

	m.trigger.UpdateTriggerPrice(pair, pos.ID, levelIdx, level.Type, pos.Direction, newTriggerPrice)
	return m.cfg.Store.Update(ctx, pos)
}

// AddLevel adds a new level to an existing position.
func (m *Manager) AddLevel(ctx context.Context, posID [16]byte, lp LevelParams) error {
	pos, err := m.cfg.Store.Get(ctx, posID)
	if err != nil {
		return err
	}
	if pos.State.IsTerminal() {
		return fmt.Errorf("position is %s", pos.State)
	}

	level := Level{
		Index:        len(pos.Levels),
		Type:         lp.Type,
		TriggerPrice: new(big.Int).Set(lp.TriggerPrice),
		PortionBps:   lp.PortionBps,
		Status:       LevelActive,
		CancelOnFire: lp.CancelOnFire,
	}
	if lp.MoveSLTo != nil {
		level.MoveSLTo = new(big.Int).Set(lp.MoveSLTo)
	}
	pos.Levels = append(pos.Levels, level)
	pos.UpdatedAt = time.Now().Unix()

	pair := pos.Pair()
	m.trigger.Register(pair, pos.ID, level.Index, level.Type, pos.Direction, level.TriggerPrice)
	return m.cfg.Store.Update(ctx, pos)
}

// RemoveLevel cancels and removes a level from a position.
func (m *Manager) RemoveLevel(ctx context.Context, posID [16]byte, levelIdx int) error {
	pos, err := m.cfg.Store.Get(ctx, posID)
	if err != nil {
		return err
	}
	if pos.State.IsTerminal() {
		return fmt.Errorf("position is %s", pos.State)
	}
	if levelIdx < 0 || levelIdx >= len(pos.Levels) {
		return fmt.Errorf("invalid level index %d", levelIdx)
	}

	pos.Levels[levelIdx].Status = LevelCancelled
	pos.UpdatedAt = time.Now().Unix()

	pair := pos.Pair()
	m.trigger.Unregister(pair, pos.ID, levelIdx)
	return m.cfg.Store.Update(ctx, pos)
}

// MarketSwap executes an immediate market swap (no trigger engine).
// Supports legacy (direct approve), Permit2 SignatureTransfer, and native ETH modes.
func (m *Manager) MarketSwap(ctx context.Context, params MarketSwapParams) (*SwapResult, error) {
	exec, ok := m.executors[params.ChainID]
	if !ok {
		return nil, fmt.Errorf("unsupported chain %d", params.ChainID)
	}

	// Broadcast signed approve TX if provided (one-click flow).
	if len(params.SignedApproveTx) > 0 {
		if _, err := exec.broadcastSignedApproveTx(ctx, params.SignedApproveTx); err != nil {
			return nil, fmt.Errorf("broadcast approve tx: %w", err)
		}
	}

	feeCfg, err := m.cfg.FeeProvider.GetFee(ctx, params.Owner)
	if err != nil {
		return nil, fmt.Errorf("get fee: %w", err)
	}

	// Get current price for minAmountOut estimation.
	pair := TokenPair{Base: params.TokenIn, Quote: params.TokenOut, ChainID: params.ChainID}
	price, _, err := m.cfg.PriceFeed.Latest(pair)
	if err != nil {
		return nil, fmt.Errorf("get price: %w", err)
	}

	minAmountOut := computeMinAmountOut(price, params.AmountIn, params.SlippageBps, params.DecimalsIn, params.DecimalsOut)

	var feeBps uint16
	if feeCfg != nil {
		feeBps = feeCfg.FeeBps
	}

	// Determine execution mode.
	execMode := ExecModeLegacy
	var permitNonce, permitDeadline *big.Int
	var permitSig []byte
	if len(params.PermitSignature) > 0 {
		execMode = ExecModePermit2Signature
		permitSig = params.PermitSignature
		permitNonce = params.PermitNonce
		permitDeadline = new(big.Int).SetInt64(params.PermitDeadline)
	}

	txHash, receipt, err := exec.executeSwap(ctx, executeSwapParams{
		User:            params.Owner,
		TokenIn:         params.TokenIn,
		TokenOut:        params.TokenOut,
		PoolFee:         params.PoolFee,
		AmountIn:        params.AmountIn,
		MinAmountOut:    minAmountOut,
		FeeBps:          feeBps,
		Priority:        PriorityNormal,
		Mode:            execMode,
		PermitNonce:     permitNonce,
		PermitDeadline:  permitDeadline,
		PermitSignature: permitSig,
	})
	if err != nil {
		return nil, err
	}

	actualAmountOut := parseAmountOutFromReceipt(receipt, exec.executorAddr)
	if actualAmountOut == nil {
		actualAmountOut = minAmountOut
	}

	return &SwapResult{
		TxHash:    txHash,
		AmountIn:  params.AmountIn,
		AmountOut: actualAmountOut,
		Fee:       computeFeeResult(params.AmountIn, feeCfg),
	}, nil
}

// --- Internal helpers ---

// registerPositionTriggers registers all active levels of a position with the trigger engine.
func (m *Manager) registerPositionTriggers(pos *Position) {
	pair := pos.Pair()
	for _, level := range pos.Levels {
		if level.Status == LevelActive {
			m.trigger.Register(pair, pos.ID, level.Index, level.Type, pos.Direction, level.TriggerPrice)
		}
	}
}

// swapTokens returns tokenIn and tokenOut based on position direction and level type.
func (m *Manager) swapTokens(pos *Position, level *Level) (tokenIn, tokenOut common.Address) {
	// Long + SL/TP: sell base for quote (sell WETH for USDC).
	// Short + SL/TP: sell quote for base (sell USDC for WETH).
	if pos.Direction == Long {
		return pos.TokenBase, pos.TokenQuote
	}
	return pos.TokenQuote, pos.TokenBase
}

// computeAmount calculates the swap amount for a level based on remaining size and portion.
func (m *Manager) computeAmount(pos *Position, level *Level) *big.Int {
	return mulBps(pos.RemainingSize, level.PortionBps)
}

// uniquePairs extracts unique token pairs from a list of positions.
func uniquePairs(positions []*Position, _ uint64) []TokenPair {
	seen := make(map[TokenPair]bool)
	var pairs []TokenPair
	for _, pos := range positions {
		pair := pos.Pair()
		if !seen[pair] {
			seen[pair] = true
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

// cancelActiveLevels cancels all active levels on a position and unregisters them from the trigger engine.
func (m *Manager) cancelActiveLevels(pos *Position, pair TokenPair, exceptIdx int) {
	for i := range pos.Levels {
		if i != exceptIdx && pos.Levels[i].Status == LevelActive {
			pos.Levels[i].Status = LevelCancelled
			m.trigger.Unregister(pair, pos.ID, i)
		}
	}
}

// CleanupClosedPositionLocks removes mutexes for positions in terminal states.
// Call periodically (e.g. every few minutes) to prevent unbounded lock map growth.
// Safe to call concurrently with Run().
func (m *Manager) CleanupClosedPositionLocks(ctx context.Context) (int, error) {
	var terminalIDs [][16]byte
	for chainID := range m.cfg.Chains {
		positions, err := m.cfg.Store.ListActive(ctx, chainID)
		if err != nil {
			return 0, fmt.Errorf("list active for chain %d: %w", chainID, err)
		}
		activeSet := make(map[[16]byte]bool, len(positions))
		for _, p := range positions {
			activeSet[p.ID] = true
		}

		m.posLocks.mu.Lock()
		for id := range m.posLocks.locks {
			if !activeSet[id] {
				terminalIDs = append(terminalIDs, id)
			}
		}
		m.posLocks.mu.Unlock()
	}

	if len(terminalIDs) == 0 {
		return 0, nil
	}
	removed := m.posLocks.Cleanup(terminalIDs)
	m.log.Info("cleaned up position locks", "removed", removed, "remaining", m.posLocks.Len())
	return removed, nil
}

// suspendPositionLevels marks all active levels as suspended (permit expired).
func (m *Manager) suspendPositionLevels(pos *Position) {
	pair := pos.Pair()
	for i := range pos.Levels {
		if pos.Levels[i].Status == LevelActive {
			pos.Levels[i].Status = LevelSuspended
			m.trigger.Unregister(pair, pos.ID, i)
		}
	}
	pos.UpdatedAt = time.Now().Unix()
}

// RenewPermit updates a position's permit data after the user signs a new one.
// Reactivates any suspended levels.
func (m *Manager) RenewPermit(ctx context.Context, posID [16]byte, signature []byte, nonce *big.Int, deadline int64, amount *big.Int) error {
	pos, err := m.cfg.Store.Get(ctx, posID)
	if err != nil {
		return err
	}
	if pos.State.IsTerminal() {
		return fmt.Errorf("position is %s", pos.State)
	}

	pos.PermitSignature = signature
	if nonce != nil {
		pos.PermitNonce = new(big.Int).Set(nonce)
	}
	pos.PermitDeadline = deadline
	if amount != nil {
		pos.PermitAmount = new(big.Int).Set(amount)
	}
	pos.PermitActivated = false // Needs re-activation on-chain.
	pos.UpdatedAt = time.Now().Unix()

	// Reactivate suspended levels.
	pair := pos.Pair()
	reactivated := 0
	for i := range pos.Levels {
		if pos.Levels[i].Status == LevelSuspended {
			pos.Levels[i].Status = LevelActive
			m.trigger.Register(pair, pos.ID, i, pos.Levels[i].Type, pos.Direction, pos.Levels[i].TriggerPrice)
			reactivated++
		}
	}

	m.log.Info("permit renewed",
		"position", fmt.Sprintf("%x", pos.ID[:8]),
		"reactivated_levels", reactivated,
		"new_deadline", deadline,
	)

	return m.cfg.Store.Update(ctx, pos)
}

// runPermitExpiryMonitor periodically checks for positions with expiring permits
// and notifies the host via OnPermitExpiring callback.
func (m *Manager) runPermitExpiryMonitor(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkPermitExpiry(ctx)
		}
	}
}

func (m *Manager) checkPermitExpiry(ctx context.Context) {
	if m.cfg.OnPermitExpiring == nil {
		return
	}

	now := time.Now().Unix()

	for chainID, ci := range m.cfg.Chains {
		positions, err := m.cfg.Store.ListActive(ctx, chainID)
		if err != nil {
			m.log.Error("permit expiry check: list active", "chain", chainID, "error", err)
			continue
		}

		warningThreshold := ci.ChainConfig.PermitExpiryWarning
		if warningThreshold == 0 {
			warningThreshold = 48 * time.Hour
		}

		for _, pos := range positions {
			if len(pos.PermitSignature) == 0 || pos.PermitDeadline == 0 {
				continue
			}

			remaining := time.Duration(pos.PermitDeadline-now) * time.Second
			if remaining <= 0 {
				// Already expired — suspend if not already done.
				hasActive := false
				for _, l := range pos.Levels {
					if l.Status == LevelActive {
						hasActive = true
						break
					}
				}
				if hasActive {
					m.suspendPositionLevels(pos)
					_ = m.cfg.Store.Update(ctx, pos)
				}
			} else if remaining <= warningThreshold {
				activeLevels := 0
				for _, l := range pos.Levels {
					if l.Status == LevelActive {
						activeLevels++
					}
				}
				m.cfg.OnPermitExpiring(PermitExpiryEvent{
					PositionID:     pos.ID,
					Owner:          pos.Owner,
					ChainID:        pos.ChainID,
					PermitDeadline: pos.PermitDeadline,
					HoursRemaining: int(remaining.Hours()),
					ActiveLevels:   activeLevels,
				})
			}
		}
	}
}

// storeUpdateWithRetry attempts Store.Update up to maxRetries times with exponential backoff.
// Used after on-chain swaps where the state MUST be persisted.
func (m *Manager) storeUpdateWithRetry(ctx context.Context, pos *Position, maxRetries int) error {
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err = m.cfg.Store.Update(ctx, pos)
		if err == nil {
			return nil
		}
		if attempt < maxRetries {
			backoff := time.Duration(1<<uint(attempt)) * time.Second // 1s, 2s, 4s
			m.log.Warn("store update failed, retrying",
				"position", fmt.Sprintf("%x", pos.ID[:8]),
				"attempt", attempt+1,
				"backoff", backoff,
				"error", err,
			)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return err
}

func (m *Manager) emitError(evt ErrorEvent) {
	if m.cfg.OnError != nil {
		m.cfg.OnError(evt)
	}
}
