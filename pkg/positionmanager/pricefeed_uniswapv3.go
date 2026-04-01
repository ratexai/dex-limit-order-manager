package positionmanager

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// Package-level cached constants to avoid recomputing on every call.
var (
	poolABI     abi.ABI
	q192        *big.Int // 2^192
	priceScale8 *big.Int // 10^8
)

func init() {
	poolABI, _ = abi.JSON(strings.NewReader(`[
		{"inputs":[],"name":"slot0","outputs":[{"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},{"name":"observationIndex","type":"uint16"},{"name":"observationCardinality","type":"uint16"},{"name":"observationCardinalityNext","type":"uint16"},{"name":"feeProtocol","type":"uint8"},{"name":"unlocked","type":"bool"}],"stateMutability":"view","type":"function"},
		{"inputs":[{"name":"secondsAgos","type":"uint32[]"}],"name":"observe","outputs":[{"name":"tickCumulatives","type":"int56[]"},{"name":"secondsPerLiquidityCumulativeX128s","type":"uint160[]"}],"stateMutability":"view","type":"function"}
	]`))
	q192 = new(big.Int).Exp(big.NewInt(2), big.NewInt(192), nil)
	priceScale8 = new(big.Int).Exp(big.NewInt(10), big.NewInt(8), nil)
}

// UniswapV3PriceFeed is a reference PriceFeed implementation that reads
// prices from Uniswap V3 pools. Supports both spot (slot0) and TWAP (observe) modes.
type UniswapV3PriceFeed struct {
	client    ChainClient
	pools     map[TokenPair]poolConfig
	twapSecs  uint32 // TWAP window in seconds. 0 = use spot price (slot0).

	mu      sync.RWMutex
	prices  map[TokenPair]cachedPrice
	subs    map[TokenPair][]chan PriceUpdate
	polling map[TokenPair]context.CancelFunc // Cancel func per poll loop.
	pollWg  sync.WaitGroup                   // Tracks running poll loops for graceful shutdown.
}

type poolConfig struct {
	Address        common.Address
	Token0Decimals uint8
	Token1Decimals uint8
	Token0IsBase   bool // true if token0 is the base token.
}

type cachedPrice struct {
	price     *big.Int
	timestamp int64
}

// UniswapV3PoolDef defines a Uniswap V3 pool for price tracking.
type UniswapV3PoolDef struct {
	Pair           TokenPair
	PoolAddress    common.Address
	Token0Decimals uint8
	Token1Decimals uint8
	Token0IsBase   bool
}

// NewUniswapV3PriceFeed creates a reference price feed.
// twapSeconds: TWAP window in seconds. Use 0 for spot price (slot0 only).
// Recommended: 30-120 seconds for anti-manipulation protection.
func NewUniswapV3PriceFeed(client ChainClient, pools []UniswapV3PoolDef, twapSeconds uint32) *UniswapV3PriceFeed {
	poolMap := make(map[TokenPair]poolConfig)
	for _, p := range pools {
		poolMap[p.Pair] = poolConfig{
			Address:        p.PoolAddress,
			Token0Decimals: p.Token0Decimals,
			Token1Decimals: p.Token1Decimals,
			Token0IsBase:   p.Token0IsBase,
		}
	}
	return &UniswapV3PriceFeed{
		client:   client,
		pools:    poolMap,
		twapSecs: twapSeconds,
		prices:   make(map[TokenPair]cachedPrice),
		subs:     make(map[TokenPair][]chan PriceUpdate),
		polling:  make(map[TokenPair]context.CancelFunc),
	}
}

// Subscribe returns a channel that receives price updates for a pair.
// The poll loop uses its own internal context — it stops only when all
// subscribers are gone or Close() is called.
func (f *UniswapV3PriceFeed) Subscribe(ctx context.Context, pair TokenPair) (<-chan PriceUpdate, error) {
	if _, ok := f.pools[pair]; !ok {
		return nil, fmt.Errorf("no pool configured for pair %v", pair)
	}

	ch := make(chan PriceUpdate, 16)

	f.mu.Lock()
	f.subs[pair] = append(f.subs[pair], ch)
	_, alreadyPolling := f.polling[pair]
	if !alreadyPolling {
		// Start poll loop with its own context, independent of subscriber ctx.
		pollCtx, cancel := context.WithCancel(context.Background())
		f.polling[pair] = cancel
		f.pollWg.Add(1)
		go func() {
			defer f.pollWg.Done()
			f.pollLoop(pollCtx, pair)
		}()
	}
	f.mu.Unlock()

	// When the subscriber's context is cancelled, remove this channel.
	go func() {
		<-ctx.Done()
		f.removeSub(pair, ch)
	}()

	return ch, nil
}

// removeSub removes a subscriber channel and stops the poll loop if no subscribers remain.
func (f *UniswapV3PriceFeed) removeSub(pair TokenPair, ch chan PriceUpdate) {
	f.mu.Lock()
	defer f.mu.Unlock()

	subs := f.subs[pair]
	for i, s := range subs {
		if s == ch {
			f.subs[pair] = append(subs[:i], subs[i+1:]...)
			close(ch)
			break
		}
	}

	// If no subscribers left, cancel the poll loop.
	if len(f.subs[pair]) == 0 {
		if cancel, ok := f.polling[pair]; ok {
			cancel()
			delete(f.polling, pair)
		}
		delete(f.subs, pair)
	}
}

// Latest returns the most recent known price.
func (f *UniswapV3PriceFeed) Latest(pair TokenPair) (*big.Int, int64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	cached, ok := f.prices[pair]
	if !ok {
		return nil, 0, fmt.Errorf("no price available for pair")
	}
	return new(big.Int).Set(cached.price), cached.timestamp, nil
}

// Close stops all poll loops and waits for them to finish.
func (f *UniswapV3PriceFeed) Close() {
	f.mu.Lock()
	for pair, cancel := range f.polling {
		cancel()
		for _, ch := range f.subs[pair] {
			close(ch)
		}
		delete(f.subs, pair)
		delete(f.polling, pair)
	}
	f.mu.Unlock()

	// Wait for all poll loops to exit.
	f.pollWg.Wait()
}

// Shutdown gracefully stops the price feed, respecting the context deadline.
// If ctx expires before all poll loops finish, returns ctx.Err().
func (f *UniswapV3PriceFeed) Shutdown(ctx context.Context) error {
	// Signal all poll loops to stop.
	f.mu.Lock()
	for pair, cancel := range f.polling {
		cancel()
		for _, ch := range f.subs[pair] {
			close(ch)
		}
		delete(f.subs, pair)
		delete(f.polling, pair)
	}
	f.mu.Unlock()

	// Wait for poll loops with deadline.
	done := make(chan struct{})
	go func() {
		f.pollWg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// pollLoop polls at regular intervals and publishes price updates.
func (f *UniswapV3PriceFeed) pollLoop(ctx context.Context, pair TokenPair) {
	pool := f.pools[pair]

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var price *big.Int
			var err error

			if f.twapSecs > 0 {
				price, err = f.readTWAP(ctx, pool, f.twapSecs)
			} else {
				price, err = f.readSlot0(ctx, pool)
			}
			if err != nil {
				continue
			}

			now := time.Now().Unix()

			f.mu.Lock()
			// Skip if price unchanged.
			if cached, ok := f.prices[pair]; ok && cached.price.Cmp(price) == 0 {
				f.mu.Unlock()
				continue
			}
			f.prices[pair] = cachedPrice{price: price, timestamp: now}
			// Copy subscriber slice under lock to avoid races.
			subs := make([]chan PriceUpdate, len(f.subs[pair]))
			copy(subs, f.subs[pair])
			f.mu.Unlock()

			update := PriceUpdate{
				Pair:      pair,
				Price:     price,
				Timestamp: now,
			}

			for _, ch := range subs {
				select {
				case ch <- update:
				default:
				}
			}
		}
	}
}

// readSlot0 calls the Uniswap V3 pool's slot0() function and converts
// sqrtPriceX96 to a human-readable price with 8 decimals.
func (f *UniswapV3PriceFeed) readSlot0(ctx context.Context, pool poolConfig) (*big.Int, error) {
	calldata, err := poolABI.Pack("slot0")
	if err != nil {
		return nil, err
	}

	result, err := f.client.CallContract(ctx, ethereum.CallMsg{
		To:   &pool.Address,
		Data: calldata,
	}, nil)
	if err != nil {
		return nil, err
	}

	outputs, err := poolABI.Unpack("slot0", result)
	if err != nil {
		return nil, err
	}

	sqrtPriceX96 := outputs[0].(*big.Int)
	return sqrtPriceX96ToPrice(sqrtPriceX96, pool.Token0Decimals, pool.Token1Decimals, pool.Token0IsBase), nil
}

// readTWAP computes a time-weighted average price using pool.observe().
// This is resistant to flash-loan manipulation unlike raw slot0().
func (f *UniswapV3PriceFeed) readTWAP(ctx context.Context, pool poolConfig, windowSecs uint32) (*big.Int, error) {
	// observe([windowSecs, 0]) returns cumulative ticks at [now-window, now].
	secondsAgos := []uint32{windowSecs, 0}
	calldata, err := poolABI.Pack("observe", secondsAgos)
	if err != nil {
		return nil, err
	}

	result, err := f.client.CallContract(ctx, ethereum.CallMsg{
		To:   &pool.Address,
		Data: calldata,
	}, nil)
	if err != nil {
		return nil, err
	}

	outputs, err := poolABI.Unpack("observe", result)
	if err != nil {
		return nil, err
	}

	tickCumulatives := outputs[0].([]*big.Int)
	if len(tickCumulatives) < 2 {
		return nil, fmt.Errorf("observe returned %d values, need 2", len(tickCumulatives))
	}

	// avgTick = (tickCumulatives[1] - tickCumulatives[0]) / windowSecs
	tickDiff := new(big.Int).Sub(tickCumulatives[1], tickCumulatives[0])
	avgTick := tickDiff.Div(tickDiff, new(big.Int).SetUint64(uint64(windowSecs)))

	return tickToPrice(avgTick.Int64(), pool.Token0Decimals, pool.Token1Decimals, pool.Token0IsBase), nil
}

// tickToPrice converts a Uniswap V3 tick to a price with 8 decimals.
// price = 1.0001^tick, adjusted for token decimals and direction.
func tickToPrice(tick int64, decimals0, decimals1 uint8, token0IsBase bool) *big.Int {
	// price0 = 1.0001^tick (token1 per token0)
	priceFloat := math.Pow(1.0001, float64(tick))

	// Adjust for decimals: raw price is in token1/token0 with implicit decimal shift.
	// Actual price = priceFloat * 10^decimals0 / 10^decimals1
	decimalAdjust := math.Pow(10, float64(decimals0)-float64(decimals1))
	adjusted := priceFloat * decimalAdjust

	if !token0IsBase {
		// If token0 is quote, invert.
		if adjusted == 0 {
			return new(big.Int)
		}
		adjusted = 1.0 / adjusted
	}

	// Scale to 8 decimals.
	scaled := adjusted * 1e8
	return new(big.Int).SetUint64(uint64(scaled))
}

// sqrtPriceX96ToPrice converts Uniswap V3 sqrtPriceX96 to a price with 8 decimals.
func sqrtPriceX96ToPrice(sqrtPriceX96 *big.Int, decimals0, decimals1 uint8, token0IsBase bool) *big.Int {
	sq := new(big.Int).Mul(sqrtPriceX96, sqrtPriceX96)

	if token0IsBase {
		dec0 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals0)), nil)
		dec1 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals1)), nil)

		num := new(big.Int).Mul(sq, priceScale8)
		num.Mul(num, dec0)

		denom := new(big.Int).Mul(q192, dec1)
		return num.Div(num, denom)
	}

	dec0 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals0)), nil)
	dec1 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals1)), nil)

	num := new(big.Int).Mul(q192, priceScale8)
	num.Mul(num, dec1)

	denom := new(big.Int).Mul(sq, dec0)
	return num.Div(num, denom)
}
