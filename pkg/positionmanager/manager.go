package positionmanager

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// Manager is the main entry point of the position manager library.
// Create one via New(), then call Run() in a goroutine to start the keeper loop.
type Manager struct {
	cfg       Config
	trigger   *TriggerEngine
	executors map[uint64]*executor // chainID → executor
	mu        sync.RWMutex
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

	return &Manager{
		cfg:       cfg,
		trigger:   NewTriggerEngine(),
		executors: executors,
	}, nil
}

// Run starts the keeper loop. It blocks until ctx is cancelled.
// It loads active positions, subscribes to price feeds, and dispatches triggers.
func (m *Manager) Run(ctx context.Context) error {
	// Load active positions and register triggers.
	for chainID := range m.cfg.Chains {
		positions, err := m.cfg.Store.ListActive(ctx, chainID)
		if err != nil {
			return fmt.Errorf("load active positions for chain %d: %w", chainID, err)
		}
		for _, pos := range positions {
			m.registerPositionTriggers(pos)
		}
	}

	// Subscribe to price feeds and dispatch triggers.
	// One goroutine per chain, each with a worker pool for execution.
	var wg sync.WaitGroup
	for chainID, ci := range m.cfg.Chains {
		wg.Add(1)
		go func(chainID uint64, ci ChainInstance) {
			defer wg.Done()
			m.runChain(ctx, chainID, ci)
		}(chainID, ci)
	}

	wg.Wait()
	return ctx.Err()
}

// runChain runs the keeper loop for a single chain.
func (m *Manager) runChain(ctx context.Context, chainID uint64, ci ChainInstance) {
	// Collect all pairs for this chain.
	pairs := m.collectPairs(ctx, chainID)

	// Subscribe to each pair's price feed.
	for _, pair := range pairs {
		ch, err := m.cfg.PriceFeed.Subscribe(ctx, pair)
		if err != nil {
			m.emitError(ErrorEvent{ChainID: chainID, Err: fmt.Errorf("subscribe %v: %w", pair, err)})
			continue
		}

		go func(pair TokenPair, ch <-chan PriceUpdate) {
			for {
				select {
				case <-ctx.Done():
					return
				case update, ok := <-ch:
					if !ok {
						return
					}
					m.handlePriceUpdate(ctx, update)
				}
			}
		}(pair, ch)
	}

	// Block until context cancelled.
	<-ctx.Done()
}

// handlePriceUpdate processes a single price update.
func (m *Manager) handlePriceUpdate(ctx context.Context, update PriceUpdate) {
	events := m.trigger.OnPrice(update.Pair, update.Price)
	for _, evt := range events {
		// Execute in the current goroutine for simplicity.
		// Production: use a worker pool with ci.ExecutorWorkers goroutines.
		m.executeTrigger(ctx, evt)
	}
}

// executeTrigger handles a single trigger event: execute swap, update state.
func (m *Manager) executeTrigger(ctx context.Context, evt TriggerEvent) {
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

	// Compute minAmountOut (simplified — assumes 18/6 decimals for base/quote).
	// TODO: fetch actual decimals from chain.
	minAmountOut := computeMinAmountOut(level.TriggerPrice, amountIn, slippageBps, 18, 6)

	var feeBps uint16
	if feeCfg != nil {
		feeBps = feeCfg.FeeBps
	}

	// Execute on-chain swap.
	txHash, err := exec.executeSwap(ctx, executeSwapParams{
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
		m.emitError(ErrorEvent{
			PositionID: evt.PositionID,
			LevelIndex: evt.LevelIndex,
			ChainID:    evt.ChainID,
			Err:        fmt.Errorf("execute swap: %w", err),
			Retryable:  true,
		})
		return
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
	pair := TokenPair{Base: pos.TokenBase, Quote: pos.TokenQuote, ChainID: pos.ChainID}

	// Cancel linked levels.
	if level.Type == LevelTypeSL {
		// SL fired: cancel all remaining levels.
		for i := range pos.Levels {
			if i != evt.LevelIndex && pos.Levels[i].Status == LevelActive {
				pos.Levels[i].Status = LevelCancelled
				m.trigger.Unregister(pair, pos.ID, i)
			}
		}
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
			AmountOut:     minAmountOut, // Approximate; actual comes from receipt.
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

	pair := TokenPair{Base: pos.TokenBase, Quote: pos.TokenQuote, ChainID: pos.ChainID}

	for i := range pos.Levels {
		if pos.Levels[i].Status == LevelActive {
			pos.Levels[i].Status = LevelCancelled
		}
	}
	pos.State = StateCancelled
	pos.UpdatedAt = time.Now().Unix()

	m.trigger.UnregisterPosition(pair, pos.ID)
	return m.cfg.Store.Update(ctx, pos)
}

// UpdateLevel changes the trigger price of a level. Zero gas — off-chain only.
func (m *Manager) UpdateLevel(ctx context.Context, posID [16]byte, levelIdx int, newTriggerPrice *big.Int) error {
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

	pair := TokenPair{Base: pos.TokenBase, Quote: pos.TokenQuote, ChainID: pos.ChainID}
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

	pair := TokenPair{Base: pos.TokenBase, Quote: pos.TokenQuote, ChainID: pos.ChainID}
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

	pair := TokenPair{Base: pos.TokenBase, Quote: pos.TokenQuote, ChainID: pos.ChainID}
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

	minAmountOut := computeMinAmountOut(price, params.AmountIn, params.SlippageBps, 18, 6)

	var feeBps uint16
	if feeCfg != nil {
		feeBps = feeCfg.FeeBps
	}

	txHash, err := exec.executeSwap(ctx, executeSwapParams{
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

	return &SwapResult{
		TxHash:    txHash,
		AmountIn:  params.AmountIn,
		AmountOut: minAmountOut,
		Fee:       computeFeeResult(params.AmountIn, feeCfg),
	}, nil
}

// --- Internal helpers ---

// registerPositionTriggers registers all active levels of a position with the trigger engine.
func (m *Manager) registerPositionTriggers(pos *Position) {
	pair := TokenPair{Base: pos.TokenBase, Quote: pos.TokenQuote, ChainID: pos.ChainID}
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
	amount := new(big.Int).Mul(pos.RemainingSize, big.NewInt(int64(level.PortionBps)))
	amount.Div(amount, big.NewInt(10000))
	return amount
}

// collectPairs returns all unique token pairs for active positions on a chain.
func (m *Manager) collectPairs(ctx context.Context, chainID uint64) []TokenPair {
	positions, err := m.cfg.Store.ListActive(ctx, chainID)
	if err != nil {
		return nil
	}
	seen := make(map[TokenPair]bool)
	var pairs []TokenPair
	for _, pos := range positions {
		pair := TokenPair{Base: pos.TokenBase, Quote: pos.TokenQuote, ChainID: chainID}
		if !seen[pair] {
			seen[pair] = true
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

func (m *Manager) emitError(evt ErrorEvent) {
	if m.cfg.OnError != nil {
		m.cfg.OnError(evt)
	}
}
