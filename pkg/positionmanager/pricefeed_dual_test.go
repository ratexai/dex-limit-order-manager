package positionmanager

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// --- mockWSClient extends mockChainClient with log subscription ---

type mockWSClient struct {
	*mockChainClient
	logSubs []chan<- types.Log
}

func newMockWSClient() *mockWSClient {
	return &mockWSClient{mockChainClient: newMockChainClient()}
}

type mockLogSubscription struct {
	errCh chan error
}

func (s *mockLogSubscription) Err() <-chan error { return s.errCh }
func (s *mockLogSubscription) Unsubscribe()     { close(s.errCh) }

func (c *mockWSClient) SubscribeFilterLogs(_ context.Context, _ ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error) {
	c.mu.Lock()
	c.logSubs = append(c.logSubs, ch)
	c.mu.Unlock()
	return &mockLogSubscription{errCh: make(chan error, 1)}, nil
}

// pushSwapLog simulates a Swap event by pushing a log to all subscribers.
func (c *mockWSClient) pushSwapLog(poolAddr common.Address, sqrtPriceX96 *big.Int) {
	// Build Swap event log data: amount0(int256), amount1(int256), sqrtPriceX96(uint160), liquidity(uint128), tick(int24)
	data := make([]byte, 160) // 5 × 32 bytes

	// amount0 = 0 (offset 0)
	// amount1 = 0 (offset 32)
	// sqrtPriceX96 at offset 64
	sqrtBytes := sqrtPriceX96.Bytes()
	copy(data[96-len(sqrtBytes):96], sqrtBytes)
	// liquidity = 0 (offset 96)
	// tick = 0 (offset 128)

	swapEventID := swapEventABI.Events["Swap"].ID

	log := types.Log{
		Address: poolAddr,
		Topics:  []common.Hash{swapEventID, {}, {}}, // event ID + 2 indexed (sender, recipient)
		Data:    data,
	}

	c.mu.Lock()
	subs := make([]chan<- types.Log, len(c.logSubs))
	copy(subs, c.logSubs)
	c.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- log:
		default:
		}
	}
}

func testDualPair() TokenPair {
	return TokenPair{
		Base:    common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Quote:   common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		ChainID: 1,
	}
}

func testDualPoolDef() UniswapV3PoolDef {
	return UniswapV3PoolDef{
		Pair:           testDualPair(),
		PoolAddress:    common.HexToAddress("0x8ad599c3a0ff1de082011efddc58f1908eb6e6d8"),
		Token0Decimals: 6,  // USDC
		Token1Decimals: 18, // WETH
		Token0IsBase:   false,
	}
}

func TestNewDualPriceFeed_Validation(t *testing.T) {
	_, err := NewDualPriceFeed(DualPriceFeedConfig{})
	if err == nil {
		t.Error("expected error for nil WSClient")
	}

	_, err = NewDualPriceFeed(DualPriceFeedConfig{
		WSClient: newMockWSClient(),
	})
	if err == nil {
		t.Error("expected error for empty pools")
	}
}

func TestNewDualPriceFeed_DefaultConfig(t *testing.T) {
	ws := newMockWSClient()
	feed, err := NewDualPriceFeed(DualPriceFeedConfig{
		WSClient: ws,
		Pools:    []UniswapV3PoolDef{testDualPoolDef()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if feed.cfg.MaxDeviation != 200 {
		t.Errorf("MaxDeviation = %d, want 200", feed.cfg.MaxDeviation)
	}
	if feed.cfg.StaleThreshold != 30*time.Second {
		t.Errorf("StaleThreshold = %v, want 30s", feed.cfg.StaleThreshold)
	}
}

func TestDualPriceFeed_LatestWithoutSubscribe(t *testing.T) {
	ws := newMockWSClient()
	feed, _ := NewDualPriceFeed(DualPriceFeedConfig{
		WSClient: ws,
		Pools:    []UniswapV3PoolDef{testDualPoolDef()},
	})
	defer feed.Close()

	_, _, err := feed.Latest(testDualPair())
	if err == nil {
		t.Error("expected error for untracked pair")
	}
}

func TestComputeDeviationBps(t *testing.T) {
	feed := &DualPriceFeed{}

	tests := []struct {
		a, b     int64
		expected uint16
	}{
		{100, 100, 0},
		{100, 102, 200},  // 2%
		{100, 98, 200},   // 2%
		{100, 105, 500},  // 5%
		{100, 0, 10000},  // max deviation
		{0, 100, 10000},  // max deviation
	}

	for _, tt := range tests {
		got := feed.computeDeviationBps(big.NewInt(tt.a), big.NewInt(tt.b))
		if got != tt.expected {
			t.Errorf("deviation(%d, %d) = %d bps, want %d bps", tt.a, tt.b, got, tt.expected)
		}
	}
}

func TestParseFloatPrice(t *testing.T) {
	tests := []struct {
		input    string
		expected int64
	}{
		{"2000.50", 200050000000},
		{"0.0005", 50000},
		{"1.0", 100000000},
		{"100000", 10000000000000},
	}

	for _, tt := range tests {
		result, err := parseFloatPrice(tt.input)
		if err != nil {
			t.Errorf("parseFloatPrice(%q): %v", tt.input, err)
			continue
		}
		if result.Int64() != tt.expected {
			t.Errorf("parseFloatPrice(%q) = %d, want %d", tt.input, result.Int64(), tt.expected)
		}
	}
}

func TestParseFloatPrice_Invalid(t *testing.T) {
	_, err := parseFloatPrice("not-a-number")
	if err == nil {
		t.Error("expected error for invalid input")
	}
}

func TestMockWSClient_PushSwapLog(t *testing.T) {
	ws := newMockWSClient()
	pool := testDualPoolDef()

	// Subscribe to capture the log.
	logCh := make(chan types.Log, 1)
	ws.SubscribeFilterLogs(context.Background(), ethereum.FilterQuery{}, logCh)

	// Push a swap log with a known sqrtPriceX96.
	sqrtPrice := new(big.Int).Mul(big.NewInt(1), new(big.Int).Exp(big.NewInt(2), big.NewInt(96), nil)) // 2^96 = price 1.0
	ws.pushSwapLog(pool.PoolAddress, sqrtPrice)

	select {
	case log := <-logCh:
		if log.Address != pool.PoolAddress {
			t.Errorf("expected pool address %s, got %s", pool.PoolAddress.Hex(), log.Address.Hex())
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timeout waiting for swap log")
	}
}

func TestDualPriceFeed_Close(t *testing.T) {
	ws := newMockWSClient()
	feed, _ := NewDualPriceFeed(DualPriceFeedConfig{
		WSClient: ws,
		Pools:    []UniswapV3PoolDef{testDualPoolDef()},
	})

	ctx, cancel := context.WithCancel(context.Background())
	_, err := feed.Subscribe(ctx, testDualPair())
	if err != nil {
		t.Fatal(err)
	}

	cancel()
	time.Sleep(50 * time.Millisecond)
	feed.Close() // Should not hang.
}
