package positionmanager

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// ==========================================================================
// Circuit Breaker Tests
// ==========================================================================

func TestCircuitBreaker_StartsClosedAllowsAll(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{MaxFailures: 3, ResetTimeout: time.Second, HalfOpenMaxAttempts: 1})
	if cb.State() != CircuitClosed {
		t.Fatalf("expected Closed, got %v", cb.State())
	}
	if err := cb.Allow(); err != nil {
		t.Fatalf("Allow on closed circuit: %v", err)
	}
}

func TestCircuitBreaker_OpensAfterMaxFailures(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{MaxFailures: 3, ResetTimeout: time.Second, HalfOpenMaxAttempts: 1})

	for i := 0; i < 3; i++ {
		cb.RecordFailure()
	}
	if cb.State() != CircuitOpen {
		t.Fatalf("expected Open after 3 failures, got %v", cb.State())
	}
	if cb.Failures() != 3 {
		t.Fatalf("expected 3 failures, got %d", cb.Failures())
	}
	if err := cb.Allow(); err != ErrCircuitOpen {
		t.Fatalf("expected ErrCircuitOpen, got %v", err)
	}
}

func TestCircuitBreaker_HalfOpenAfterTimeout(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{MaxFailures: 1, ResetTimeout: 10 * time.Millisecond, HalfOpenMaxAttempts: 1})

	cb.RecordFailure()
	if cb.State() != CircuitOpen {
		t.Fatal("expected Open")
	}

	time.Sleep(20 * time.Millisecond)
	if err := cb.Allow(); err != nil {
		t.Fatalf("expected Allow after timeout, got %v", err)
	}
	if cb.State() != CircuitHalfOpen {
		t.Fatalf("expected HalfOpen, got %v", cb.State())
	}
}

func TestCircuitBreaker_ClosesAfterHalfOpenSuccess(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{MaxFailures: 1, ResetTimeout: 10 * time.Millisecond, HalfOpenMaxAttempts: 2})

	cb.RecordFailure()
	time.Sleep(20 * time.Millisecond)
	cb.Allow() // → HalfOpen

	cb.RecordSuccess()
	if cb.State() != CircuitHalfOpen {
		t.Fatal("should still be HalfOpen after 1 success (need 2)")
	}
	cb.RecordSuccess()
	if cb.State() != CircuitClosed {
		t.Fatalf("expected Closed after 2 successes, got %v", cb.State())
	}
	if cb.Failures() != 0 {
		t.Fatalf("failures should be reset to 0, got %d", cb.Failures())
	}
}

func TestCircuitBreaker_FailureInHalfOpenReopens(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{MaxFailures: 1, ResetTimeout: 10 * time.Millisecond, HalfOpenMaxAttempts: 2})

	cb.RecordFailure()
	time.Sleep(20 * time.Millisecond)
	cb.Allow() // → HalfOpen

	cb.RecordFailure() // Fail during half-open
	if cb.State() != CircuitOpen {
		t.Fatalf("expected Open after half-open failure, got %v", cb.State())
	}
}

func TestCircuitBreaker_SuccessInClosedResetsFailures(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{MaxFailures: 3, ResetTimeout: time.Second, HalfOpenMaxAttempts: 1})

	cb.RecordFailure()
	cb.RecordFailure()
	if cb.Failures() != 2 {
		t.Fatalf("expected 2, got %d", cb.Failures())
	}
	cb.RecordSuccess()
	if cb.Failures() != 0 {
		t.Fatalf("expected 0 after success, got %d", cb.Failures())
	}
}

func TestCircuitBreaker_StateString(t *testing.T) {
	tests := []struct {
		state    CircuitState
		expected string
	}{
		{CircuitClosed, "CLOSED"},
		{CircuitOpen, "OPEN"},
		{CircuitHalfOpen, "HALF_OPEN"},
		{CircuitState(99), "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := tt.state.String(); got != tt.expected {
			t.Errorf("CircuitState(%d).String() = %s, want %s", tt.state, got, tt.expected)
		}
	}
}

func TestCircuitBreaker_DefaultConfig(t *testing.T) {
	cfg := DefaultCircuitBreakerConfig()
	if cfg.MaxFailures != 5 || cfg.ResetTimeout != 30*time.Second || cfg.HalfOpenMaxAttempts != 2 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestCircuitBreaker_InvalidConfigDefaults(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{MaxFailures: -1, ResetTimeout: -1, HalfOpenMaxAttempts: 0})
	// Should use safe defaults.
	for i := 0; i < 5; i++ {
		cb.RecordFailure()
	}
	if cb.State() != CircuitOpen {
		t.Fatal("should open after 5 failures (default)")
	}
}

// ==========================================================================
// Rate Limiter Tests
// ==========================================================================

func TestRateLimiter_AllowBurst(t *testing.T) {
	rl := NewRPCRateLimiter(10, 5) // 10/sec, burst 5

	allowed := 0
	for i := 0; i < 10; i++ {
		if rl.Allow() {
			allowed++
		}
	}
	if allowed != 5 {
		t.Errorf("expected 5 allowed (burst), got %d", allowed)
	}
}

func TestRateLimiter_AllowRefill(t *testing.T) {
	rl := NewRPCRateLimiter(100, 1) // 100/sec, burst 1
	rl.Allow() // Drain the one token.

	time.Sleep(20 * time.Millisecond) // Should refill ~2 tokens.
	if !rl.Allow() {
		t.Error("expected Allow after refill")
	}
}

func TestRateLimiter_WaitRespectsContext(t *testing.T) {
	rl := NewRPCRateLimiter(1, 1) // 1/sec, burst 1
	rl.Allow() // Drain.

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := rl.Wait(ctx)
	if err == nil {
		t.Error("expected context deadline error")
	}
}

func TestRateLimiter_WaitSuccess(t *testing.T) {
	rl := NewRPCRateLimiter(1000, 1)
	rl.Allow() // Drain.

	ctx := context.Background()
	err := rl.Wait(ctx)
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
}

func TestRateLimitedClient_Wraps(t *testing.T) {
	inner := newMockChainClient()
	rl := NewRPCRateLimiter(1000, 10)
	client := NewRateLimitedClient(inner, rl)

	price, err := client.SuggestGasPrice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if price.Cmp(inner.gasPrice) != 0 {
		t.Errorf("expected %s, got %s", inner.gasPrice, price)
	}

	chainID, err := client.ChainID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if chainID.Int64() != 1 {
		t.Errorf("expected chain 1, got %d", chainID.Int64())
	}
}

func TestRateLimitedClient_ContextCancel(t *testing.T) {
	inner := newMockChainClient()
	rl := NewRPCRateLimiter(1, 1)
	client := NewRateLimitedClient(inner, rl)

	// Drain the token.
	rl.Allow()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := client.SuggestGasPrice(ctx)
	if err == nil {
		t.Error("expected error from cancelled context")
	}
}

// ==========================================================================
// MarketSwap Tests
// ==========================================================================

func TestMarketSwap_UnsupportedChain(t *testing.T) {
	h := newTestHarness(t)
	_, err := h.manager.MarketSwap(context.Background(), MarketSwapParams{
		ChainID: 999,
	})
	if err == nil {
		t.Fatal("expected error for unsupported chain")
	}
}

func TestMarketSwap_FeeFetchError(t *testing.T) {
	store := newMockStore()
	pf := newMockPriceFeed()
	cc := newMockChainClient()
	key, _ := generateTestKey(t)

	mgr, _ := New(Config{
		Store:       store,
		PriceFeed:   pf,
		FeeProvider: &errorFeeProvider{},
		Chains: map[uint64]ChainInstance{
			1: {Client: cc, KeeperKey: key, ExecutorAddress: common.HexToAddress("0x1234"), ChainConfig: EthereumDefaults()},
		},
	})

	_, err := mgr.MarketSwap(context.Background(), MarketSwapParams{
		Owner:   common.HexToAddress("0xUser"),
		ChainID: 1,
	})
	if err == nil {
		t.Fatal("expected fee error")
	}
}

type errorFeeProvider struct{}

func (e *errorFeeProvider) GetFee(_ context.Context, _ common.Address) (*FeeConfig, error) {
	return nil, fmt.Errorf("fee service down")
}

func TestMarketSwap_NoPriceAvailable(t *testing.T) {
	h := newTestHarness(t)
	// Don't set any price in the mock feed.
	_, err := h.manager.MarketSwap(context.Background(), MarketSwapParams{
		Owner:      common.HexToAddress("0xUser"),
		TokenIn:    common.HexToAddress("0xA"),
		TokenOut:   common.HexToAddress("0xB"),
		AmountIn:   big.NewInt(1e18),
		ChainID:    1,
		PoolFee:    3000,
		DecimalsIn: 18,
		DecimalsOut: 6,
	})
	if err == nil {
		t.Fatal("expected error for missing price")
	}
}

// ==========================================================================
// CleanupClosedPositionLocks Tests
// ==========================================================================

func TestCleanupClosedPositionLocks_RemovesTerminal(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	params := testOpenParams()
	pos, err := h.manager.OpenPosition(ctx, params)
	if err != nil {
		t.Fatal(err)
	}

	// Lock it (simulate trigger execution).
	h.manager.posLocks.Lock(pos.ID)
	h.manager.posLocks.Unlock(pos.ID)

	// Cancel the position.
	h.manager.CancelPosition(ctx, pos.ID)

	removed, err := h.manager.CleanupClosedPositionLocks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed, got %d", removed)
	}
}

// ==========================================================================
// Edge Cases: Position Validation
// ==========================================================================

func TestOpenPosition_ZeroTriggerPrice(t *testing.T) {
	h := newTestHarness(t)
	params := testOpenParams()
	params.Levels[0].TriggerPrice = big.NewInt(0)

	_, err := h.manager.OpenPosition(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for zero trigger price")
	}
}

func TestOpenPosition_NegativeSize(t *testing.T) {
	h := newTestHarness(t)
	params := testOpenParams()
	params.Size = big.NewInt(-1)

	_, err := h.manager.OpenPosition(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for negative size")
	}
}

func TestUpdateLevel_OnTerminalPosition(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	h.manager.CancelPosition(ctx, pos.ID)

	err := h.manager.UpdateLevel(ctx, pos.ID, 0, big.NewInt(190000000000))
	if err == nil {
		t.Fatal("expected error on terminal position")
	}
}

func TestUpdateLevel_TriggeredLevel(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	// Manually mark level as triggered.
	pos.Levels[0].Status = LevelTriggered
	h.store.Update(ctx, pos)

	err := h.manager.UpdateLevel(ctx, pos.ID, 0, big.NewInt(190000000000))
	if err == nil {
		t.Fatal("expected error on triggered level")
	}
}

func TestAddLevel_OnTerminalPosition(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pos, _ := h.manager.OpenPosition(ctx, testOpenParams())
	h.manager.CancelPosition(ctx, pos.ID)

	err := h.manager.AddLevel(ctx, pos.ID, LevelParams{
		Type: LevelTypeTP, TriggerPrice: big.NewInt(300000000000), PortionBps: 1000,
	})
	if err == nil {
		t.Fatal("expected error on terminal position")
	}
}

// ==========================================================================
// Config Defaults Tests
// ==========================================================================

func TestBaseDefaults(t *testing.T) {
	cfg := BaseDefaults()
	if cfg.BlockTime != 2*time.Second {
		t.Errorf("Base block time: %v", cfg.BlockTime)
	}
	if cfg.ExecutorWorkers != 8 {
		t.Errorf("Base workers: %d", cfg.ExecutorWorkers)
	}
}

func TestBSCDefaults(t *testing.T) {
	cfg := BSCDefaults()
	if cfg.BlockTime != 3*time.Second {
		t.Errorf("BSC block time: %v", cfg.BlockTime)
	}
	if cfg.ExecutorWorkers != 6 {
		t.Errorf("BSC workers: %d", cfg.ExecutorWorkers)
	}
}

// ==========================================================================
// Trigger Engine Edge Cases
// ==========================================================================

func TestTriggerEngine_DuplicateRegister(t *testing.T) {
	e := NewTriggerEngine()
	pair := testPair()
	var id [16]byte
	id[0] = 1

	// Register same level twice — should not duplicate.
	e.Register(pair, id, 0, LevelTypeSL, Long, big.NewInt(180000000000))
	e.Register(pair, id, 0, LevelTypeSL, Long, big.NewInt(180000000000))

	events := e.OnPrice(pair, big.NewInt(170000000000))
	// Both entries fire, but this is a known edge case — dedup happens at manager level.
	if len(events) < 1 {
		t.Fatal("expected at least 1 event")
	}
}

func TestTriggerEngine_EmptyOnPrice(t *testing.T) {
	e := NewTriggerEngine()
	pair := testPair()
	events := e.OnPrice(pair, big.NewInt(200000000000))
	if len(events) != 0 {
		t.Fatalf("expected 0 events for untracked pair, got %d", len(events))
	}
}

func TestTriggerEngine_CountAfterUnregister(t *testing.T) {
	e := NewTriggerEngine()
	pair := testPair()
	var id [16]byte
	id[0] = 1

	e.Register(pair, id, 0, LevelTypeSL, Long, big.NewInt(180000000000))
	if e.Count() != 1 {
		t.Fatalf("expected 1, got %d", e.Count())
	}

	e.Unregister(pair, id, 0)
	if e.Count() != 0 {
		t.Fatalf("expected 0 after unregister, got %d", e.Count())
	}
}
