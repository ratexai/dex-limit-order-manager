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
	trigger   *TriggerEngine
	executors map[uint64]*executor // chainID → executor
	posLocks  positionLockMap      // Per-position mutex to serialize executions.
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

	return &Manager{
		cfg:       cfg,
		log:       logger,
		trigger:   NewTriggerEngine(),
		executors: executors,
	}, nil
}

// Run starts the keeper loop. It blocks until ctx is cancelled.
// It loads active positions, subscribes to price feeds, and dispatches triggers.
func (m *Manager) Run(ctx context.Context) error {
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
	}

	var wg sync.WaitGroup
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

	// Subscribe to each pair's price feed.
	for _, pair := range pairs {
		ch, err := m.cfg.PriceFeed.Subscribe(ctx, pair)
		if err != nil {
			m.emitError(ErrorEvent{ChainID: chainID, Err: fmt.Errorf("subscribe %v: %w", pair, err)})
			continue
		}

		wg.Add(1)
		go func(pair TokenPair, ch <-chan PriceUpdate) {
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
		}(pair, ch)
	}

	// Wait for all goroutines to finish.
	wg.Wait()
	close(execCh)
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
	m.posLocks.Lock(evt.PositionID)
	defer m.posLocks.Unlock(evt.PositionID)

	pos, err := m.cfg.Store.Get(ctx, evt.PositionID)
	if err != nil {
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
	})
	if err != nil {
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

	// Parse actual amountOut from receipt logs.
	actualAmountOut := parseAmountOutFromReceipt(receipt, exec.executorAddr)
	if actualAmountOut == nil {
		actualAmountOut = minAmountOut // Fallback if event parsing fails.
	}

	// Update level execution results.
	now := time.Now().Unix()
	level.Status = LevelTriggered
	level.ExecTxHash = txHash
	level.ExecPrice = new(big.Int).Set(evt.Price)
	level.ExecAmount = amountIn
	level.ExecAt = now

	// Update remaining size.
	pos.RemainingSize = new(big.Int).Sub(pos.RemainingSize, amountIn)
	if pos.RemainingSize.Sign() <= 0 {
		pos.RemainingSize = new(big.Int)
	}
	pos.UpdatedAt = now

	// Handle post-trigger actions.
	pair := pos.Pair()

	// Cancel linked levels.
	if level.Type == LevelTypeSL {
		m.cancelActiveLevels(pos, pair, evt.LevelIndex)
		pos.State = StateClosed
	} else {
		// TP fired.
		// Cancel specified levels.
		for _, cancelIdx := range level.CancelOnFire {
			if cancelIdx < len(pos.Levels) && pos.Levels[cancelIdx].Status == LevelActive {
				pos.Levels[cancelIdx].Status = LevelCancelled
				m.trigger.Unregister(pair, pos.ID, cancelIdx)
			}
		}

		// Move SL if configured.
		if level.MoveSLTo != nil && level.MoveSLTo.Sign() > 0 {
			if sl := pos.ActiveSL(); sl != nil {
				sl.TriggerPrice = new(big.Int).Set(level.MoveSLTo)
				m.trigger.UpdateTriggerPrice(pair, pos.ID, sl.Index, sl.Type, pos.Direction, sl.TriggerPrice)
			}
		}

		// Check if position is fully closed.
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

	// Persist.
	if err := m.cfg.Store.Update(ctx, pos); err != nil {
		m.emitError(ErrorEvent{PositionID: pos.ID, ChainID: pos.ChainID, Err: fmt.Errorf("store update: %w", err)})
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

	if err := m.cfg.Store.Save(ctx, pos); err != nil {
		return nil, fmt.Errorf("save position: %w", err)
	}

	m.registerPositionTriggers(pos)
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
func (m *Manager) MarketSwap(ctx context.Context, params MarketSwapParams) (*SwapResult, error) {
	exec, ok := m.executors[params.ChainID]
	if !ok {
		return nil, fmt.Errorf("unsupported chain %d", params.ChainID)
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

	txHash, receipt, err := exec.executeSwap(ctx, executeSwapParams{
		User:         params.Owner,
		TokenIn:      params.TokenIn,
		TokenOut:     params.TokenOut,
		PoolFee:      params.PoolFee,
		AmountIn:     params.AmountIn,
		MinAmountOut: minAmountOut,
		FeeBps:       feeBps,
		Priority:     PriorityNormal,
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

func (m *Manager) emitError(evt ErrorEvent) {
	if m.cfg.OnError != nil {
		m.cfg.OnError(evt)
	}
}
