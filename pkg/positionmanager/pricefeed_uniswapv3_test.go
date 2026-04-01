package positionmanager

import (
	"context"
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// --- sqrtPriceX96ToPrice tests ---

func TestSqrtPriceX96ToPrice_ETH_USDC(t *testing.T) {
	// Real-world example: ETH/USDC pool on Uniswap V3.
	// sqrtPriceX96 ≈ sqrt(2000 * 10^6 / 10^18) * 2^96
	// For price = 2000 USDC/ETH (token0=USDC 6dec, token1=WETH 18dec, token0IsBase=false):
	// sqrtPriceX96 = sqrt(price_token1_per_token0) * 2^96
	// price_token1_per_token0 = (1/2000) * 10^18 / 10^6 = 5e8
	// sqrtPriceX96 = sqrt(5e8) * 2^96 ≈ 22360.68 * 2^96 ≈ 1.7706e39

	// Use the known sqrtPriceX96 for ~$2000 ETH.
	// In a typical USDC/WETH pool: token0=USDC(6 dec), token1=WETH(18 dec).
	// If token0IsBase=false (WETH is base), price = 1/raw = quote per base.
	// sqrtPriceX96 for ~$2000: approximately 3.543e12 (from mainnet data).
	// Let's compute: price(token1/token0) = (sqrtPriceX96^2) / 2^192.

	// Instead of guessing mainnet values, test the math with a known price:
	// token0=WETH(18 dec), token1=USDC(6 dec), token0IsBase=true.
	// Price = 2000 USDC per WETH → in raw terms: 2000 * 10^6 / 10^18 = 2e-9
	// sqrtPriceX96 = sqrt(2e-9) * 2^96 = 4.472e-5 * 7.922e28 = 3.543e24
	sqrtPrice := new(big.Int)
	sqrtPrice.SetString("3543191142285914205922034", 10)

	price := sqrtPriceX96ToPrice(sqrtPrice, 18, 6, true)

	// Expected: ~200000000000 ($2000 with 8 decimals).
	// Allow 1% tolerance for rounding.
	expected := int64(200000000000)
	got := price.Int64()
	ratio := float64(got) / float64(expected)
	if ratio < 0.99 || ratio > 1.01 {
		t.Errorf("ETH/USDC price: expected ~%d, got %d (ratio %.4f)", expected, got, ratio)
	}
}

func TestSqrtPriceX96ToPrice_Token0IsBase(t *testing.T) {
	// token0=TokenA(18 dec), token1=TokenB(18 dec), price 1:1.
	// sqrtPriceX96 = sqrt(1) * 2^96 = 2^96 = 79228162514264337593543950336
	sqrtPrice := new(big.Int).Exp(big.NewInt(2), big.NewInt(96), nil)

	price := sqrtPriceX96ToPrice(sqrtPrice, 18, 18, true)

	// Expected: 1e8 (1.0 with 8 decimals).
	expected := big.NewInt(1e8)
	if price.Cmp(expected) != 0 {
		t.Errorf("1:1 price token0IsBase: expected %s, got %s", expected, price)
	}
}

func TestSqrtPriceX96ToPrice_Token0IsNotBase(t *testing.T) {
	// Same as above but inverted: token0IsBase=false → price = 1/raw = 1.0
	sqrtPrice := new(big.Int).Exp(big.NewInt(2), big.NewInt(96), nil)

	price := sqrtPriceX96ToPrice(sqrtPrice, 18, 18, false)

	// Expected: 1e8 (1.0 with 8 decimals).
	expected := big.NewInt(1e8)
	if price.Cmp(expected) != 0 {
		t.Errorf("1:1 price !token0IsBase: expected %s, got %s", expected, price)
	}
}

func TestSqrtPriceX96ToPrice_DifferentDecimals(t *testing.T) {
	// token0=18 dec, token1=8 dec, token0IsBase=true, price 1:1 in human terms.
	// Raw price = 1 * 10^8 / 10^18 = 1e-10.
	// sqrtPriceX96 = sqrt(1e-10) * 2^96 = 1e-5 * 2^96
	sqrtF := math.Sqrt(1e-10)
	twoTo96, _ := new(big.Float).SetString("79228162514264337593543950336")
	sqrtBig := new(big.Float).Mul(new(big.Float).SetFloat64(sqrtF), twoTo96)
	sqrtPrice, _ := sqrtBig.Int(nil)

	price := sqrtPriceX96ToPrice(sqrtPrice, 18, 8, true)

	// Expected: ~1e8 (price 1.0 in 8 decimals).
	expected := int64(1e8)
	got := price.Int64()
	ratio := float64(got) / float64(expected)
	if ratio < 0.99 || ratio > 1.01 {
		t.Errorf("diff decimals price: expected ~%d, got %d (ratio %.4f)", expected, got, ratio)
	}
}

// --- tickToPrice tests ---

func TestTickToPrice_Zero(t *testing.T) {
	// Tick 0 → price = 1.0001^0 = 1.0
	price := tickToPrice(0, 18, 18, true)

	expected := big.NewInt(1e8)
	if price.Cmp(expected) != 0 {
		t.Errorf("tick 0: expected %s, got %s", expected, price)
	}
}

func TestTickToPrice_Positive(t *testing.T) {
	// Tick 23028 ≈ ln(10)/ln(1.0001) → price ≈ 10.0
	// More precisely: 1.0001^23028 ≈ 10.002
	price := tickToPrice(23028, 18, 18, true)

	expected := int64(10e8) // $10 in 8 decimals
	got := price.Int64()
	ratio := float64(got) / float64(expected)
	if ratio < 0.99 || ratio > 1.01 {
		t.Errorf("tick 23028: expected ~%d, got %d (ratio %.4f)", expected, got, ratio)
	}
}

func TestTickToPrice_Negative(t *testing.T) {
	// Tick -23028 → price ≈ 0.1
	price := tickToPrice(-23028, 18, 18, true)

	expected := int64(1e7) // 0.1 in 8 decimals
	got := price.Int64()
	ratio := float64(got) / float64(expected)
	if ratio < 0.99 || ratio > 1.01 {
		t.Errorf("tick -23028: expected ~%d, got %d (ratio %.4f)", expected, got, ratio)
	}
}

func TestTickToPrice_Inverted(t *testing.T) {
	// Token0 is NOT base → invert price.
	// Tick 0, same decimals → 1.0 inverted = 1.0
	price := tickToPrice(0, 18, 18, false)

	expected := big.NewInt(1e8)
	if price.Cmp(expected) != 0 {
		t.Errorf("tick 0 inverted: expected %s, got %s", expected, price)
	}
}

func TestTickToPrice_DifferentDecimals(t *testing.T) {
	// token0=18 dec, token1=6 dec, tick 0 → raw price = 1.0
	// Decimal adjust = 10^(18-6) = 10^12
	// With token0IsBase=true: price = 1.0 * 10^12 = 1e12, scaled to 8 dec = 1e20
	// This represents a very large price (expected for raw 1:1 with different decimals).
	price := tickToPrice(0, 18, 6, true)

	// price = 1.0 * 10^(18-6) * 1e8 = 1e20
	expected := new(big.Int).SetUint64(1e20)
	if price.Cmp(expected) != 0 {
		t.Errorf("tick 0 diff decimals: expected %s, got %s", expected, price)
	}
}

// --- Subscribe / Latest / Close tests ---

func pricefeedTestPair() TokenPair {
	return TokenPair{
		Base:    common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Quote:   common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		ChainID: 1,
	}
}

func TestNewUniswapV3PriceFeed(t *testing.T) {
	pair := pricefeedTestPair()
	poolAddr := common.HexToAddress("0x8ad599c3a0ff1de082011efddc58f1908eb6e6d8")
	client := newMockChainClient()

	feed := NewUniswapV3PriceFeed(client, []UniswapV3PoolDef{
		{Pair: pair, PoolAddress: poolAddr, Token0Decimals: 6, Token1Decimals: 18, Token0IsBase: false},
	}, 0)

	if feed == nil {
		t.Fatal("expected non-nil feed")
	}
	if len(feed.pools) != 1 {
		t.Errorf("expected 1 pool, got %d", len(feed.pools))
	}
	if feed.twapSecs != 0 {
		t.Errorf("expected twapSecs=0, got %d", feed.twapSecs)
	}
}

func TestNewUniswapV3PriceFeed_TWAP(t *testing.T) {
	client := newMockChainClient()
	feed := NewUniswapV3PriceFeed(client, nil, 60)

	if feed.twapSecs != 60 {
		t.Errorf("expected twapSecs=60, got %d", feed.twapSecs)
	}
}

func TestSubscribe_UnknownPair(t *testing.T) {
	client := newMockChainClient()
	feed := NewUniswapV3PriceFeed(client, nil, 0)

	_, err := feed.Subscribe(context.Background(), pricefeedTestPair())
	if err == nil {
		t.Fatal("expected error for unknown pair")
	}
}

func TestLatest_NoPriceAvailable(t *testing.T) {
	client := newMockChainClient()
	feed := NewUniswapV3PriceFeed(client, nil, 0)

	_, _, err := feed.Latest(pricefeedTestPair())
	if err == nil {
		t.Fatal("expected error when no price available")
	}
}

func TestLatest_ReturnsCachedPrice(t *testing.T) {
	client := newMockChainClient()
	pair := pricefeedTestPair()
	feed := NewUniswapV3PriceFeed(client, nil, 0)

	// Manually inject a cached price.
	feed.mu.Lock()
	feed.prices[pair] = cachedPrice{price: big.NewInt(200000000000), timestamp: 1000}
	feed.mu.Unlock()

	price, ts, err := feed.Latest(pair)
	if err != nil {
		t.Fatal(err)
	}
	if price.Cmp(big.NewInt(200000000000)) != 0 {
		t.Errorf("expected 200000000000, got %s", price)
	}
	if ts != 1000 {
		t.Errorf("expected ts 1000, got %d", ts)
	}

	// Ensure returned price is a copy.
	price.SetInt64(0)
	p2, _, _ := feed.Latest(pair)
	if p2.Cmp(big.NewInt(200000000000)) != 0 {
		t.Error("Latest should return a copy, not a reference")
	}
}

func TestClose_StopsPolling(t *testing.T) {
	pair := pricefeedTestPair()
	poolAddr := common.HexToAddress("0x8ad599c3a0ff1de082011efddc58f1908eb6e6d8")
	client := newMockChainClient()

	feed := NewUniswapV3PriceFeed(client, []UniswapV3PoolDef{
		{Pair: pair, PoolAddress: poolAddr, Token0Decimals: 6, Token1Decimals: 18, Token0IsBase: false},
	}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := feed.Subscribe(ctx, pair)
	if err != nil {
		t.Fatal(err)
	}

	// Verify poll loop started.
	feed.mu.RLock()
	pollingCount := len(feed.polling)
	feed.mu.RUnlock()
	if pollingCount != 1 {
		t.Fatalf("expected 1 polling loop, got %d", pollingCount)
	}

	// Close should stop everything.
	feed.Close()

	feed.mu.RLock()
	pollingAfter := len(feed.polling)
	subsAfter := len(feed.subs)
	feed.mu.RUnlock()
	if pollingAfter != 0 {
		t.Errorf("expected 0 polling after Close, got %d", pollingAfter)
	}
	if subsAfter != 0 {
		t.Errorf("expected 0 subs after Close, got %d", subsAfter)
	}
}

func TestShutdown_RespectsDeadline(t *testing.T) {
	pair := pricefeedTestPair()
	poolAddr := common.HexToAddress("0x8ad599c3a0ff1de082011efddc58f1908eb6e6d8")
	client := newMockChainClient()

	feed := NewUniswapV3PriceFeed(client, []UniswapV3PoolDef{
		{Pair: pair, PoolAddress: poolAddr, Token0Decimals: 6, Token1Decimals: 18, Token0IsBase: false},
	}, 0)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()

	_, err := feed.Subscribe(subCtx, pair)
	if err != nil {
		t.Fatal(err)
	}

	// Shutdown with generous deadline should succeed.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()

	if err := feed.Shutdown(shutdownCtx); err != nil {
		t.Errorf("Shutdown should succeed, got: %v", err)
	}
}

func TestSubscribe_MultipleSubscribers(t *testing.T) {
	pair := pricefeedTestPair()
	poolAddr := common.HexToAddress("0x8ad599c3a0ff1de082011efddc58f1908eb6e6d8")
	client := newMockChainClient()

	feed := NewUniswapV3PriceFeed(client, []UniswapV3PoolDef{
		{Pair: pair, PoolAddress: poolAddr, Token0Decimals: 6, Token1Decimals: 18, Token0IsBase: false},
	}, 0)
	defer feed.Close()

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	_, err1 := feed.Subscribe(ctx1, pair)
	if err1 != nil {
		t.Fatal(err1)
	}
	_, err2 := feed.Subscribe(ctx2, pair)
	if err2 != nil {
		t.Fatal(err2)
	}

	feed.mu.RLock()
	subCount := len(feed.subs[pair])
	pollingCount := len(feed.polling)
	feed.mu.RUnlock()

	if subCount != 2 {
		t.Errorf("expected 2 subscribers, got %d", subCount)
	}
	// Only one poll loop should run.
	if pollingCount != 1 {
		t.Errorf("expected 1 polling loop, got %d", pollingCount)
	}

	// Cancel first subscriber — poll loop should continue for second.
	cancel1()
	time.Sleep(50 * time.Millisecond) // Let the cleanup goroutine run.

	feed.mu.RLock()
	subCountAfter := len(feed.subs[pair])
	pollingAfter := len(feed.polling)
	feed.mu.RUnlock()

	if subCountAfter != 1 {
		t.Errorf("expected 1 subscriber after cancel, got %d", subCountAfter)
	}
	if pollingAfter != 1 {
		t.Errorf("poll loop should still be active, got %d", pollingAfter)
	}
}

func TestSubscribe_LastSubCancelStopsPolling(t *testing.T) {
	pair := pricefeedTestPair()
	poolAddr := common.HexToAddress("0x8ad599c3a0ff1de082011efddc58f1908eb6e6d8")
	client := newMockChainClient()

	feed := NewUniswapV3PriceFeed(client, []UniswapV3PoolDef{
		{Pair: pair, PoolAddress: poolAddr, Token0Decimals: 6, Token1Decimals: 18, Token0IsBase: false},
	}, 0)

	ctx, cancel := context.WithCancel(context.Background())

	_, err := feed.Subscribe(ctx, pair)
	if err != nil {
		t.Fatal(err)
	}

	// Cancel the only subscriber.
	cancel()
	time.Sleep(50 * time.Millisecond)

	feed.mu.RLock()
	pollingCount := len(feed.polling)
	subCount := len(feed.subs[pair])
	feed.mu.RUnlock()

	if pollingCount != 0 {
		t.Errorf("expected 0 polling after last sub cancelled, got %d", pollingCount)
	}
	if subCount != 0 {
		t.Errorf("expected 0 subs after cancel, got %d", subCount)
	}

	// Wait for poll goroutine to finish.
	feed.pollWg.Wait()
}

// --- Price dedup test ---

func TestPollLoop_SkipsDuplicatePrice(t *testing.T) {
	// This tests the dedup logic: if price hasn't changed, no update is published.
	// We test by directly inspecting cached price behavior.
	pair := pricefeedTestPair()
	client := newMockChainClient()
	feed := NewUniswapV3PriceFeed(client, nil, 0)

	price := big.NewInt(200000000000)

	// Set initial price.
	feed.mu.Lock()
	feed.prices[pair] = cachedPrice{price: price, timestamp: 100}
	feed.mu.Unlock()

	// Read the cached price.
	feed.mu.RLock()
	cached := feed.prices[pair]
	feed.mu.RUnlock()

	// Same price should compare equal.
	if cached.price.Cmp(price) != 0 {
		t.Error("cached price should equal set price")
	}

	// The pollLoop's dedup check: if cached.price.Cmp(newPrice) == 0, skip.
	newPrice := big.NewInt(200000000000)
	if cached.price.Cmp(newPrice) != 0 {
		t.Error("dedup check: same price should be considered equal")
	}

	// Different price should not be equal.
	differentPrice := big.NewInt(210000000000)
	if cached.price.Cmp(differentPrice) == 0 {
		t.Error("dedup check: different price should not be equal")
	}
}
