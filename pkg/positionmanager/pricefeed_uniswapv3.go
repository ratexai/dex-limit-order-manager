package positionmanager

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// UniswapV3PriceFeed is a reference PriceFeed implementation that reads
// prices from Uniswap V3 pools via slot0() polling.
//
// Usage:
//
//	feed := NewUniswapV3PriceFeed(client, map[TokenPair]common.Address{
//	    {Base: WETH, Quote: USDC, ChainID: 1}: poolAddress,
//	})
//	mgr, _ := positionmanager.New(positionmanager.Config{PriceFeed: feed, ...})
type UniswapV3PriceFeed struct {
	client ChainClient
	pools  map[TokenPair]poolConfig

	mu      sync.RWMutex
	prices  map[TokenPair]cachedPrice
	subs    map[TokenPair][]chan PriceUpdate
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
func NewUniswapV3PriceFeed(client ChainClient, pools []UniswapV3PoolDef) *UniswapV3PriceFeed {
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
		client: client,
		pools:  poolMap,
		prices: make(map[TokenPair]cachedPrice),
		subs:   make(map[TokenPair][]chan PriceUpdate),
	}
}

// Subscribe returns a channel that receives price updates for a pair.
func (f *UniswapV3PriceFeed) Subscribe(ctx context.Context, pair TokenPair) (<-chan PriceUpdate, error) {
	if _, ok := f.pools[pair]; !ok {
		return nil, fmt.Errorf("no pool configured for pair %v", pair)
	}

	ch := make(chan PriceUpdate, 16)

	f.mu.Lock()
	f.subs[pair] = append(f.subs[pair], ch)
	f.mu.Unlock()

	// Start polling if this is the first subscriber for this pair.
	go f.pollLoop(ctx, pair)

	return ch, nil
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

// pollLoop polls slot0() at regular intervals and publishes price updates.
func (f *UniswapV3PriceFeed) pollLoop(ctx context.Context, pair TokenPair) {
	pool := f.pools[pair]

	// Poll every 2 seconds (suitable for both Ethereum 12s and Base 2s blocks).
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			f.closeSubs(pair)
			return
		case <-ticker.C:
			price, err := f.readSlot0(ctx, pool)
			if err != nil {
				continue // Skip this tick, try again next time.
			}

			now := time.Now().Unix()
			update := PriceUpdate{
				Pair:      pair,
				Price:     price,
				Timestamp: now,
			}

			f.mu.Lock()
			f.prices[pair] = cachedPrice{price: price, timestamp: now}
			subs := f.subs[pair]
			f.mu.Unlock()

			for _, ch := range subs {
				select {
				case ch <- update:
				default:
					// Subscriber is slow, drop update.
				}
			}
		}
	}
}

// readSlot0 calls the Uniswap V3 pool's slot0() function and converts
// sqrtPriceX96 to a human-readable price with 8 decimals.
func (f *UniswapV3PriceFeed) readSlot0(ctx context.Context, pool poolConfig) (*big.Int, error) {
	slot0ABI, _ := abi.JSON(strings.NewReader(`[{"inputs":[],"name":"slot0","outputs":[{"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},{"name":"observationIndex","type":"uint16"},{"name":"observationCardinality","type":"uint16"},{"name":"observationCardinalityNext","type":"uint16"},{"name":"feeProtocol","type":"uint8"},{"name":"unlocked","type":"bool"}],"stateMutability":"view","type":"function"}]`))

	calldata, err := slot0ABI.Pack("slot0")
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

	outputs, err := slot0ABI.Unpack("slot0", result)
	if err != nil {
		return nil, err
	}

	sqrtPriceX96 := outputs[0].(*big.Int)
	return sqrtPriceX96ToPrice(sqrtPriceX96, pool.Token0Decimals, pool.Token1Decimals, pool.Token0IsBase), nil
}

// sqrtPriceX96ToPrice converts Uniswap V3 sqrtPriceX96 to a price with 8 decimals.
//
//	price = (sqrtPriceX96 / 2^96)^2, adjusted for token decimals.
//
// If token0IsBase is true, price = quote/base = token1/token0.
// Otherwise, price = token0/token1.
func sqrtPriceX96ToPrice(sqrtPriceX96 *big.Int, decimals0, decimals1 uint8, token0IsBase bool) *big.Int {
	// price_raw = sqrtPriceX96^2 / 2^192
	// With 8 output decimals: price = sqrtPriceX96^2 * 10^8 * 10^decimals0 / (2^192 * 10^decimals1)
	// Or inversely if token0 is NOT base.

	sq := new(big.Int).Mul(sqrtPriceX96, sqrtPriceX96)

	// 2^192
	q192 := new(big.Int).Exp(big.NewInt(2), big.NewInt(192), nil)

	priceDecimals := new(big.Int).Exp(big.NewInt(10), big.NewInt(8), nil) // 10^8 for output

	if token0IsBase {
		// price (quote per base) = sq * 10^8 * 10^decimals0 / (2^192 * 10^decimals1)
		dec0 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals0)), nil)
		dec1 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals1)), nil)

		num := new(big.Int).Mul(sq, priceDecimals)
		num.Mul(num, dec0)

		denom := new(big.Int).Mul(q192, dec1)
		return num.Div(num, denom)
	}

	// price (quote per base) = 2^192 * 10^8 * 10^decimals1 / (sq * 10^decimals0)
	dec0 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals0)), nil)
	dec1 := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals1)), nil)

	num := new(big.Int).Mul(q192, priceDecimals)
	num.Mul(num, dec1)

	denom := new(big.Int).Mul(sq, dec0)
	return num.Div(num, denom)
}

func (f *UniswapV3PriceFeed) closeSubs(pair TokenPair) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, ch := range f.subs[pair] {
		close(ch)
	}
	delete(f.subs, pair)
}
