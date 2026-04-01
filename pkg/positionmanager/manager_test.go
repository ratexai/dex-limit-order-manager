package positionmanager

import (
	"context"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// testHarness bundles all mocks and the Manager for test convenience.
type testHarness struct {
	store     *mockStore
	priceFeed *mockPriceFeed
	feeProvider *mockFeeProvider
	chainClient *mockChainClient
	manager   *Manager
	execEvents []ExecutionEvent
	errEvents  []ErrorEvent
	mu         sync.Mutex
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	store := newMockStore()
	pf := newMockPriceFeed()
	cc := newMockChainClient()

	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}

	h := &testHarness{
		store:       store,
		priceFeed:   pf,
		feeProvider: &mockFeeProvider{fee: &FeeConfig{FeeBps: 100}},
		chainClient: cc,
	}

	mgr, err := New(Config{
		Store:       store,
		PriceFeed:   pf,
		FeeProvider: h.feeProvider,
		Chains: map[uint64]ChainInstance{
			1: {
				Client:          cc,
				KeeperKey:       key,
				ExecutorAddress: common.HexToAddress("0x1234"),
				ChainConfig:     EthereumDefaults(),
			},
		},
		OnExecution: func(evt ExecutionEvent) {
			h.mu.Lock()
			h.execEvents = append(h.execEvents, evt)
			h.mu.Unlock()
		},
		OnError: func(evt ErrorEvent) {
			h.mu.Lock()
			h.errEvents = append(h.errEvents, evt)
			h.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	h.manager = mgr
	return h
}

func testPair() TokenPair {
	return TokenPair{
		Base:    common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"), // WETH
		Quote:   common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"), // USDC
		ChainID: 1,
	}
}

func testOpenParams() OpenParams {
	pair := testPair()
	return OpenParams{
		Owner:         common.HexToAddress("0xUser"),
		TokenBase:     pair.Base,
		TokenQuote:    pair.Quote,
		Direction:     Long,
		Size:          big.NewInt(1e18), // 1 WETH
		EntryPrice:    big.NewInt(200000000000), // $2000, 8 decimals
		ChainID:       1,
		PoolFee:       3000,
		DecimalsBase:  18,
		DecimalsQuote: 6,
		Levels: []LevelParams{
			{
				Type:         LevelTypeSL,
				TriggerPrice: big.NewInt(180000000000), // $1800
				PortionBps:   10000,                     // 100%
			},
			{
				Type:         LevelTypeTP,
				TriggerPrice: big.NewInt(220000000000), // $2200
				PortionBps:   5000,                      // 50%
			},
		},
	}
}

// --- New() validation tests ---

func TestNew_MissingStore(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("expected error for missing Store")
	}
}

func TestNew_MissingPriceFeed(t *testing.T) {
	_, err := New(Config{Store: newMockStore()})
	if err == nil {
		t.Fatal("expected error for missing PriceFeed")
	}
}

func TestNew_MissingFeeProvider(t *testing.T) {
	_, err := New(Config{
		Store:     newMockStore(),
		PriceFeed: newMockPriceFeed(),
	})
	if err == nil {
		t.Fatal("expected error for missing FeeProvider")
	}
}

func TestNew_NoChains(t *testing.T) {
	_, err := New(Config{
		Store:       newMockStore(),
		PriceFeed:   newMockPriceFeed(),
		FeeProvider: &mockFeeProvider{},
	})
	if err == nil {
		t.Fatal("expected error for no chains")
	}
}

// --- OpenPosition tests ---

func TestOpenPosition_Success(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()

	pos, err := h.manager.OpenPosition(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	if pos.State != StateActive {
		t.Errorf("expected StateActive, got %v", pos.State)
	}
	if pos.TotalSize.Cmp(params.Size) != 0 {
		t.Errorf("size mismatch")
	}
	if pos.RemainingSize.Cmp(params.Size) != 0 {
		t.Errorf("remaining size should equal total size")
	}
	if len(pos.Levels) != 2 {
		t.Errorf("expected 2 levels, got %d", len(pos.Levels))
	}
	if pos.DecimalsBase != 18 || pos.DecimalsQuote != 6 {
		t.Errorf("decimals not set correctly")
	}

	// Verify stored.
	stored, err := h.store.Get(ctx, pos.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ID != pos.ID {
		t.Error("stored position ID mismatch")
	}

	// Verify triggers registered.
	if h.manager.trigger.Count() != 2 {
		t.Errorf("expected 2 triggers, got %d", h.manager.trigger.Count())
	}
}

func TestOpenPosition_InvalidSize(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.Size = big.NewInt(0)

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for zero size")
	}
}

func TestOpenPosition_NilSize(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.Size = nil

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for nil size")
	}
}

func TestOpenPosition_NoLevels(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.Levels = nil

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for no levels")
	}
}

func TestOpenPosition_UnsupportedChain(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.ChainID = 999

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for unsupported chain")
	}
}

func TestOpenPosition_PortionBpsExceeds10000(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.Levels = []LevelParams{
		{Type: LevelTypeSL, TriggerPrice: big.NewInt(180000000000), PortionBps: 6000},
		{Type: LevelTypeTP, TriggerPrice: big.NewInt(220000000000), PortionBps: 5000},
	}

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for PortionBps > 10000")
	}
}

func TestOpenPosition_InvalidTriggerPrice(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.Levels[0].TriggerPrice = big.NewInt(0) // Zero

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for zero trigger price")
	}
}

func TestOpenPosition_InvalidPortionBps(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.Levels[0].PortionBps = 0

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for zero PortionBps")
	}
}

func TestOpenPosition_CancelOnFireOutOfRange(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()
	params := testOpenParams()
	params.Levels[0].CancelOnFire = []int{5} // Out of range

	_, err := h.manager.OpenPosition(ctx, params)
	if err == nil {
		t.Fatal("expected error for CancelOnFire out of range")
	}
}

// --- CancelPosition tests ---

func TestCancelPosition_Success(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, err := h.manager.OpenPosition(ctx, testOpenParams())
	if err != nil {
		t.Fatal(err)
	}

	err = h.manager.CancelPosition(ctx, pos.ID)
	if err != nil {
		t.Fatal(err)
	}

	updated, _ := h.store.Get(ctx, pos.ID)
	if updated.State != StateCancelled {
		t.Errorf("expected StateCancelled, got %v", updated.State)
	}
	for i, l := range updated.Levels {
		if l.Status != LevelCancelled {
			t.Errorf("level %d: expected LevelCancelled, got %v", i, l.Status)
		}
	}

	// All triggers should be unregistered.
	if h.manager.trigger.Count() != 0 {
		t.Errorf("expected 0 triggers after cancel, got %d", h.manager.trigger.Count())
	}
}

func TestCancelPosition_AlreadyCancelled(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	_ = h.manager.CancelPosition(ctx, pos.ID)

	err := h.manager.CancelPosition(ctx, pos.ID)
	if err == nil {
		t.Fatal("expected error cancelling already-cancelled position")
	}
}

func TestCancelPosition_NotFound(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	err := h.manager.CancelPosition(ctx, [16]byte{0xff})
	if err == nil {
		t.Fatal("expected error for non-existent position")
	}
}

// --- UpdateLevel tests ---

func TestUpdateLevel_Success(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	newPrice := big.NewInt(175000000000) // $1750

	err := h.manager.UpdateLevel(ctx, pos.ID, 0, newPrice)
	if err != nil {
		t.Fatal(err)
	}

	updated, _ := h.store.Get(ctx, pos.ID)
	if updated.Levels[0].TriggerPrice.Cmp(newPrice) != 0 {
		t.Errorf("trigger price not updated: got %s", updated.Levels[0].TriggerPrice)
	}
}

func TestUpdateLevel_InvalidPrice(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())

	err := h.manager.UpdateLevel(ctx, pos.ID, 0, big.NewInt(0))
	if err == nil {
		t.Fatal("expected error for zero price")
	}

	err = h.manager.UpdateLevel(ctx, pos.ID, 0, nil)
	if err == nil {
		t.Fatal("expected error for nil price")
	}
}

func TestUpdateLevel_InvalidIndex(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())

	err := h.manager.UpdateLevel(ctx, pos.ID, 99, big.NewInt(100))
	if err == nil {
		t.Fatal("expected error for invalid index")
	}
}

func TestUpdateLevel_TerminalPosition(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	_ = h.manager.CancelPosition(ctx, pos.ID)

	err := h.manager.UpdateLevel(ctx, pos.ID, 0, big.NewInt(100))
	if err == nil {
		t.Fatal("expected error updating cancelled position")
	}
}

// --- AddLevel tests ---

func TestAddLevel_Success(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())

	err := h.manager.AddLevel(ctx, pos.ID, LevelParams{
		Type:         LevelTypeTP,
		TriggerPrice: big.NewInt(250000000000), // $2500
		PortionBps:   3000,
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, _ := h.store.Get(ctx, pos.ID)
	if len(updated.Levels) != 3 {
		t.Errorf("expected 3 levels, got %d", len(updated.Levels))
	}
	if updated.Levels[2].Index != 2 {
		t.Errorf("new level index should be 2, got %d", updated.Levels[2].Index)
	}
	if h.manager.trigger.Count() != 3 {
		t.Errorf("expected 3 triggers, got %d", h.manager.trigger.Count())
	}
}

func TestAddLevel_TerminalPosition(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	_ = h.manager.CancelPosition(ctx, pos.ID)

	err := h.manager.AddLevel(ctx, pos.ID, LevelParams{
		Type:         LevelTypeTP,
		TriggerPrice: big.NewInt(250000000000),
		PortionBps:   3000,
	})
	if err == nil {
		t.Fatal("expected error adding level to cancelled position")
	}
}

// --- RemoveLevel tests ---

func TestRemoveLevel_Success(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())

	err := h.manager.RemoveLevel(ctx, pos.ID, 1)
	if err != nil {
		t.Fatal(err)
	}

	updated, _ := h.store.Get(ctx, pos.ID)
	if updated.Levels[1].Status != LevelCancelled {
		t.Errorf("expected LevelCancelled, got %v", updated.Levels[1].Status)
	}
	// Only SL trigger should remain.
	if h.manager.trigger.Count() != 1 {
		t.Errorf("expected 1 trigger, got %d", h.manager.trigger.Count())
	}
}

func TestRemoveLevel_InvalidIndex(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())

	err := h.manager.RemoveLevel(ctx, pos.ID, 99)
	if err == nil {
		t.Fatal("expected error for invalid index")
	}
}

// --- GetPosition / ListPositions tests ---

func TestGetPosition(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())

	got, err := h.manager.GetPosition(ctx, pos.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != pos.ID {
		t.Error("ID mismatch")
	}
}

func TestListPositions_ByOwner(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	owner := common.HexToAddress("0xUser")
	params := testOpenParams()
	params.Owner = owner
	_, _ = h.manager.OpenPosition(ctx, params)
	_, _ = h.manager.OpenPosition(ctx, params)

	positions, err := h.manager.ListPositions(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 2 {
		t.Errorf("expected 2 positions, got %d", len(positions))
	}
}

func TestListPositions_FilterByState(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	owner := common.HexToAddress("0xUser")
	params := testOpenParams()
	params.Owner = owner
	pos1, _ := h.manager.OpenPosition(ctx, params)
	_, _ = h.manager.OpenPosition(ctx, params)

	_ = h.manager.CancelPosition(ctx, pos1.ID)

	active, _ := h.manager.ListPositions(ctx, owner, StateActive)
	if len(active) != 1 {
		t.Errorf("expected 1 active, got %d", len(active))
	}

	cancelled, _ := h.manager.ListPositions(ctx, owner, StateCancelled)
	if len(cancelled) != 1 {
		t.Errorf("expected 1 cancelled, got %d", len(cancelled))
	}
}

// --- Position helper tests ---

func TestPosition_Pair(t *testing.T) {
	pos := &Position{
		TokenBase:  common.HexToAddress("0xBase"),
		TokenQuote: common.HexToAddress("0xQuote"),
		ChainID:    1,
	}
	pair := pos.Pair()
	if pair.Base != pos.TokenBase || pair.Quote != pos.TokenQuote || pair.ChainID != 1 {
		t.Error("Pair() returned wrong values")
	}
}

func TestPosition_ActiveLevels(t *testing.T) {
	pos := &Position{
		Levels: []Level{
			{Index: 0, Status: LevelActive},
			{Index: 1, Status: LevelTriggered},
			{Index: 2, Status: LevelActive},
			{Index: 3, Status: LevelCancelled},
		},
	}
	active := pos.ActiveLevels()
	if len(active) != 2 {
		t.Errorf("expected 2 active levels, got %d", len(active))
	}
}

func TestPosition_ActiveSL(t *testing.T) {
	pos := &Position{
		Levels: []Level{
			{Index: 0, Type: LevelTypeSL, Status: LevelActive},
			{Index: 1, Type: LevelTypeTP, Status: LevelActive},
		},
	}
	sl := pos.ActiveSL()
	if sl == nil || sl.Index != 0 {
		t.Error("ActiveSL should return level 0")
	}

	pos.Levels[0].Status = LevelCancelled
	if pos.ActiveSL() != nil {
		t.Error("ActiveSL should return nil when SL is cancelled")
	}
}

// --- PositionState tests ---

func TestPositionState_IsTerminal(t *testing.T) {
	if StateActive.IsTerminal() {
		t.Error("Active should not be terminal")
	}
	if StatePartialClosed.IsTerminal() {
		t.Error("PartialClosed should not be terminal")
	}
	if !StateClosed.IsTerminal() {
		t.Error("Closed should be terminal")
	}
	if !StateCancelled.IsTerminal() {
		t.Error("Cancelled should be terminal")
	}
}

// --- executeTrigger integration test ---

func TestExecuteTrigger_SLFires(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	params := testOpenParams()
	pos, err := h.manager.OpenPosition(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate SL trigger event (price dropped to $1800).
	slPrice := big.NewInt(180000000000)
	evt := TriggerEvent{
		PositionID: pos.ID,
		LevelIndex: 0, // SL
		LevelType:  LevelTypeSL,
		Price:      slPrice,
		ChainID:    1,
	}

	h.manager.executeTrigger(ctx, evt)

	// Give time for callback.
	time.Sleep(10 * time.Millisecond)

	// Position should be closed.
	updated, _ := h.store.Get(ctx, pos.ID)
	if updated.State != StateClosed {
		t.Errorf("expected StateClosed after SL, got %v", updated.State)
	}

	// SL level should be triggered.
	if updated.Levels[0].Status != LevelTriggered {
		t.Errorf("SL level should be triggered, got %v", updated.Levels[0].Status)
	}

	// All TP levels should be cancelled.
	if updated.Levels[1].Status != LevelCancelled {
		t.Errorf("TP level should be cancelled after SL, got %v", updated.Levels[1].Status)
	}

	// RemainingSize should be reduced.
	if updated.RemainingSize.Sign() > 0 {
		// For 100% SL on full remaining, should be zero.
		t.Errorf("remaining size should be 0 after 100%% SL, got %s", updated.RemainingSize)
	}

	// Verify execution event was emitted.
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.execEvents) != 1 {
		t.Fatalf("expected 1 execution event, got %d", len(h.execEvents))
	}
	if h.execEvents[0].LevelType != LevelTypeSL {
		t.Errorf("execution event should be SL")
	}
	if h.execEvents[0].PositionState != StateClosed {
		t.Errorf("execution event should show Closed state")
	}

	// Verify a TX was sent.
	h.chainClient.mu.Lock()
	txCount := len(h.chainClient.sentTxs)
	h.chainClient.mu.Unlock()
	if txCount != 1 {
		t.Errorf("expected 1 sent tx, got %d", txCount)
	}
}

func TestExecuteTrigger_TPFires(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	params := testOpenParams()
	// TP at 50%, SL at 100%.
	pos, err := h.manager.OpenPosition(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	tpPrice := big.NewInt(220000000000) // $2200
	evt := TriggerEvent{
		PositionID: pos.ID,
		LevelIndex: 1, // TP
		LevelType:  LevelTypeTP,
		Price:      tpPrice,
		ChainID:    1,
	}

	h.manager.executeTrigger(ctx, evt)

	updated, _ := h.store.Get(ctx, pos.ID)

	// TP level should be triggered.
	if updated.Levels[1].Status != LevelTriggered {
		t.Errorf("TP level should be triggered, got %v", updated.Levels[1].Status)
	}

	// SL should still be active.
	if updated.Levels[0].Status != LevelActive {
		t.Errorf("SL should remain active after partial TP, got %v", updated.Levels[0].Status)
	}

	// Position should be partial closed (SL still active).
	if updated.State != StatePartialClosed {
		t.Errorf("expected StatePartialClosed, got %v", updated.State)
	}

	// Remaining size should be reduced by 50%.
	expectedRemaining := new(big.Int).Div(params.Size, big.NewInt(2))
	if updated.RemainingSize.Cmp(expectedRemaining) != 0 {
		t.Errorf("remaining size: expected %s, got %s", expectedRemaining, updated.RemainingSize)
	}
}

func TestExecuteTrigger_TPWithMoveSL(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	params := testOpenParams()
	newSLPrice := big.NewInt(195000000000) // Move SL to $1950 (breakeven-ish)
	params.Levels[1].MoveSLTo = newSLPrice

	pos, err := h.manager.OpenPosition(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	// Fire TP.
	evt := TriggerEvent{
		PositionID: pos.ID,
		LevelIndex: 1,
		LevelType:  LevelTypeTP,
		Price:      big.NewInt(220000000000),
		ChainID:    1,
	}
	h.manager.executeTrigger(ctx, evt)

	updated, _ := h.store.Get(ctx, pos.ID)

	// SL price should have been moved.
	if updated.Levels[0].TriggerPrice.Cmp(newSLPrice) != 0 {
		t.Errorf("SL not moved: expected %s, got %s", newSLPrice, updated.Levels[0].TriggerPrice)
	}
}

func TestExecuteTrigger_TPWithCancelOnFire(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	params := testOpenParams()
	// Add a second TP at index 2.
	params.Levels = append(params.Levels, LevelParams{
		Type:         LevelTypeTP,
		TriggerPrice: big.NewInt(250000000000), // $2500
		PortionBps:   3000,
	})
	// First TP (index 1) cancels the second TP (index 2) on fire.
	params.Levels[1].CancelOnFire = []int{2}

	pos, _ := h.manager.OpenPosition(ctx, params)

	// Fire first TP.
	evt := TriggerEvent{
		PositionID: pos.ID,
		LevelIndex: 1,
		LevelType:  LevelTypeTP,
		Price:      big.NewInt(220000000000),
		ChainID:    1,
	}
	h.manager.executeTrigger(ctx, evt)

	updated, _ := h.store.Get(ctx, pos.ID)

	// Second TP should be cancelled.
	if updated.Levels[2].Status != LevelCancelled {
		t.Errorf("level 2 should be cancelled by CancelOnFire, got %v", updated.Levels[2].Status)
	}
}

func TestExecuteTrigger_TerminalPositionSkipped(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	_ = h.manager.CancelPosition(ctx, pos.ID)

	// Try to execute trigger on cancelled position — should be no-op.
	evt := TriggerEvent{
		PositionID: pos.ID,
		LevelIndex: 0,
		LevelType:  LevelTypeSL,
		Price:      big.NewInt(180000000000),
		ChainID:    1,
	}
	h.manager.executeTrigger(ctx, evt)

	h.chainClient.mu.Lock()
	txCount := len(h.chainClient.sentTxs)
	h.chainClient.mu.Unlock()
	if txCount != 0 {
		t.Error("should not send TX for cancelled position")
	}
}

// --- swapTokens tests ---

func TestSwapTokens_Long(t *testing.T) {
	h := newTestHarness(t)
	pos := &Position{
		Direction:  Long,
		TokenBase:  common.HexToAddress("0xBase"),
		TokenQuote: common.HexToAddress("0xQuote"),
	}
	level := &Level{Type: LevelTypeSL}

	tokenIn, tokenOut := h.manager.swapTokens(pos, level)
	if tokenIn != pos.TokenBase {
		t.Error("Long should sell base (tokenIn = base)")
	}
	if tokenOut != pos.TokenQuote {
		t.Error("Long should buy quote (tokenOut = quote)")
	}
}

func TestSwapTokens_Short(t *testing.T) {
	h := newTestHarness(t)
	pos := &Position{
		Direction:  Short,
		TokenBase:  common.HexToAddress("0xBase"),
		TokenQuote: common.HexToAddress("0xQuote"),
	}
	level := &Level{Type: LevelTypeSL}

	tokenIn, tokenOut := h.manager.swapTokens(pos, level)
	if tokenIn != pos.TokenQuote {
		t.Error("Short should sell quote (tokenIn = quote)")
	}
	if tokenOut != pos.TokenBase {
		t.Error("Short should buy base (tokenOut = base)")
	}
}

// --- computeAmount tests ---

func TestComputeAmount(t *testing.T) {
	h := newTestHarness(t)
	pos := &Position{RemainingSize: big.NewInt(10000)}
	level := &Level{PortionBps: 5000} // 50%

	amount := h.manager.computeAmount(pos, level)
	if amount.Cmp(big.NewInt(5000)) != 0 {
		t.Errorf("expected 5000, got %s", amount)
	}
}

func TestComputeAmount_Full(t *testing.T) {
	h := newTestHarness(t)
	pos := &Position{RemainingSize: big.NewInt(10000)}
	level := &Level{PortionBps: 10000} // 100%

	amount := h.manager.computeAmount(pos, level)
	if amount.Cmp(big.NewInt(10000)) != 0 {
		t.Errorf("expected 10000, got %s", amount)
	}
}
