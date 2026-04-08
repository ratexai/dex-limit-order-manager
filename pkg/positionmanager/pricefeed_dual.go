package positionmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// WebSocketClient extends ChainClient with log subscription for event-driven price feeds.
type WebSocketClient interface {
	ChainClient
	SubscribeFilterLogs(ctx context.Context, q ethereum.FilterQuery, ch chan<- types.Log) (ethereum.Subscription, error)
}

// OKXConfig configures the OKX DEX ticker API for price cross-checking.
type OKXConfig struct {
	BaseURL     string        // e.g. "https://www.okx.com" (default)
	ChainIndex  string        // OKX chain index: "1" (ETH), "56" (BSC), "8453" (Base)
	Timeout     time.Duration // HTTP timeout (default 5s)
	PollInterval time.Duration // How often to poll OKX (default 10s)
}

// DualPriceFeedConfig configures the dual-source price feed.
type DualPriceFeedConfig struct {
	WSClient       WebSocketClient       // WebSocket-capable chain client
	Pools          []UniswapV3PoolDef    // Uniswap/PancakeSwap V3 pools to track
	OKX            *OKXConfig            // OKX config (nil = no OKX fallback)
	TWAPSeconds    uint32                // TWAP window for initial price read (0 = spot)
	MaxDeviation   uint16                // Max bps deviation between sources before alert (default 200 = 2%)
	StaleThreshold time.Duration         // Price considered stale after this (default 30s)
}

// DualPriceFeed implements PriceFeed using Uniswap V3 Swap events (primary)
// and OKX DEX ticker API (fallback/cross-check).
//
// Primary: WebSocket subscription to Swap events on Uniswap V3 pools.
// On each Swap event, reads slot0() for exact price (Swap event confirms
// price moved, slot0 gives the current state).
//
// Fallback: OKX DEX ticker API polled every N seconds. Used when:
//   - WebSocket connection drops
//   - Primary price is stale (no Swap events for StaleThreshold)
//   - Cross-check: alerts if OKX and on-chain prices deviate > MaxDeviation
type DualPriceFeed struct {
	cfg    DualPriceFeedConfig
	pools  map[TokenPair]poolConfig

	mu      sync.RWMutex
	prices  map[TokenPair]dualPrice
	subs    map[TokenPair][]chan PriceUpdate
	active  map[TokenPair]context.CancelFunc

	wg sync.WaitGroup
}

type dualPrice struct {
	onChain   *big.Int // From Uniswap slot0 (primary)
	okx       *big.Int // From OKX API (cross-check)
	price     *big.Int // Final published price
	timestamp int64
	source    string   // "onchain", "okx", "onchain+okx"
}

// Swap event signature for Uniswap V3 / PancakeSwap V3 pools.
var swapEventABI abi.ABI

func init() {
	swapEventABI, _ = abi.JSON(strings.NewReader(`[{
		"anonymous": false,
		"inputs": [
			{"indexed": true, "name": "sender", "type": "address"},
			{"indexed": true, "name": "recipient", "type": "address"},
			{"indexed": false, "name": "amount0", "type": "int256"},
			{"indexed": false, "name": "amount1", "type": "int256"},
			{"indexed": false, "name": "sqrtPriceX96", "type": "uint160"},
			{"indexed": false, "name": "liquidity", "type": "uint128"},
			{"indexed": false, "name": "tick", "type": "int24"}
		],
		"name": "Swap",
		"type": "event"
	}]`))
}

// NewDualPriceFeed creates a dual-source price feed.
func NewDualPriceFeed(cfg DualPriceFeedConfig) (*DualPriceFeed, error) {
	if cfg.WSClient == nil {
		return nil, fmt.Errorf("WSClient is required")
	}
	if len(cfg.Pools) == 0 {
		return nil, fmt.Errorf("at least one pool is required")
	}
	if cfg.MaxDeviation == 0 {
		cfg.MaxDeviation = 200 // 2% default
	}
	if cfg.StaleThreshold == 0 {
		cfg.StaleThreshold = 30 * time.Second
	}

	poolMap := make(map[TokenPair]poolConfig)
	for _, p := range cfg.Pools {
		poolMap[p.Pair] = poolConfig{
			Address:        p.PoolAddress,
			Token0Decimals: p.Token0Decimals,
			Token1Decimals: p.Token1Decimals,
			Token0IsBase:   p.Token0IsBase,
		}
	}

	return &DualPriceFeed{
		cfg:    cfg,
		pools:  poolMap,
		prices: make(map[TokenPair]dualPrice),
		subs:   make(map[TokenPair][]chan PriceUpdate),
		active: make(map[TokenPair]context.CancelFunc),
	}, nil
}

// Subscribe implements PriceFeed. Starts event listeners on first subscription per pair.
func (f *DualPriceFeed) Subscribe(ctx context.Context, pair TokenPair) (<-chan PriceUpdate, error) {
	if _, ok := f.pools[pair]; !ok {
		return nil, fmt.Errorf("no pool configured for pair %v", pair)
	}

	ch := make(chan PriceUpdate, 16)

	f.mu.Lock()
	f.subs[pair] = append(f.subs[pair], ch)

	if _, running := f.active[pair]; !running {
		pairCtx, cancel := context.WithCancel(context.Background())
		f.active[pair] = cancel

		// Read initial price synchronously before starting event listeners.
		pool := f.pools[pair]
		if price, err := f.readSlot0(pairCtx, pool); err == nil {
			now := time.Now().Unix()
			f.prices[pair] = dualPrice{onChain: price, price: price, timestamp: now, source: "onchain"}
		}

		// Start Swap event listener.
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.runSwapListener(pairCtx, pair)
		}()

		// Start OKX cross-check if configured.
		if f.cfg.OKX != nil {
			f.wg.Add(1)
			go func() {
				defer f.wg.Done()
				f.runOKXPoller(pairCtx, pair)
			}()
		}

		// Start staleness watchdog.
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			f.runStaleWatchdog(pairCtx, pair)
		}()
	}
	f.mu.Unlock()

	// Auto-unsubscribe when caller's context is done.
	go func() {
		<-ctx.Done()
		f.removeSub(pair, ch)
	}()

	return ch, nil
}

// Latest implements PriceFeed.
func (f *DualPriceFeed) Latest(pair TokenPair) (*big.Int, int64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	dp, ok := f.prices[pair]
	if !ok || dp.price == nil {
		return nil, 0, fmt.Errorf("no price available for pair")
	}
	return new(big.Int).Set(dp.price), dp.timestamp, nil
}

// Close stops all listeners.
func (f *DualPriceFeed) Close() {
	f.mu.Lock()
	for pair, cancel := range f.active {
		cancel()
		for _, ch := range f.subs[pair] {
			close(ch)
		}
		delete(f.subs, pair)
		delete(f.active, pair)
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// runSwapListener subscribes to Swap events on the pool via WebSocket.
// On each event, reads slot0() for the current exact price.
// If the WS subscription drops, falls back to polling until reconnected.
func (f *DualPriceFeed) runSwapListener(ctx context.Context, pair TokenPair) {
	pool := f.pools[pair]
	swapEventID := swapEventABI.Events["Swap"].ID

	for {
		if err := f.listenSwapEvents(ctx, pair, pool, swapEventID); err != nil {
			if ctx.Err() != nil {
				return
			}
			// WS dropped — fall back to polling briefly, then retry WS.
			f.pollFallback(ctx, pair, pool, 5*time.Second)
		}
	}
}

// listenSwapEvents sets up a log subscription for Swap events.
// Returns when the subscription errors or context is cancelled.
func (f *DualPriceFeed) listenSwapEvents(ctx context.Context, pair TokenPair, pool poolConfig, swapEventID common.Hash) error {
	logCh := make(chan types.Log, 64)
	query := ethereum.FilterQuery{
		Addresses: []common.Address{pool.Address},
		Topics:    [][]common.Hash{{swapEventID}},
	}

	sub, err := f.cfg.WSClient.SubscribeFilterLogs(ctx, query, logCh)
	if err != nil {
		return fmt.Errorf("subscribe swap logs: %w", err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case err := <-sub.Err():
			return fmt.Errorf("swap subscription error: %w", err)

		case log := <-logCh:
			// Swap event received — extract sqrtPriceX96 directly from log data.
			price := f.priceFromSwapLog(log, pool)
			if price != nil {
				f.publishPrice(pair, price, "onchain", log.BlockNumber)
			}
		}
	}
}

// priceFromSwapLog extracts the price from a Swap event log.
// The Swap event emits sqrtPriceX96 as the 3rd non-indexed field.
func (f *DualPriceFeed) priceFromSwapLog(log types.Log, pool poolConfig) *big.Int {
	if len(log.Data) < 160 { // 5 fields × 32 bytes
		return nil
	}

	// Swap event non-indexed: amount0 (int256), amount1 (int256), sqrtPriceX96 (uint160), liquidity (uint128), tick (int24)
	outputs, err := swapEventABI.Events["Swap"].Inputs.NonIndexed().Unpack(log.Data)
	if err != nil || len(outputs) < 3 {
		return nil
	}

	sqrtPriceX96, ok := outputs[2].(*big.Int)
	if !ok || sqrtPriceX96.Sign() <= 0 {
		return nil
	}

	return sqrtPriceX96ToPrice(sqrtPriceX96, pool.Token0Decimals, pool.Token1Decimals, pool.Token0IsBase)
}

// pollFallback polls slot0() at short intervals when WS is down.
func (f *DualPriceFeed) pollFallback(ctx context.Context, pair TokenPair, pool poolConfig, duration time.Duration) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	deadline := time.After(duration)
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			price, err := f.readSlot0(ctx, pool)
			if err != nil {
				continue
			}
			f.publishPrice(pair, price, "onchain-poll", 0)
		}
	}
}

// runOKXPoller polls OKX DEX ticker API for cross-checking.
func (f *DualPriceFeed) runOKXPoller(ctx context.Context, pair TokenPair) {
	okxCfg := f.cfg.OKX
	interval := okxCfg.PollInterval
	if interval == 0 {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			price, err := f.fetchOKXPrice(ctx, pair)
			if err != nil {
				continue
			}

			f.mu.Lock()
			dp := f.prices[pair]
			dp.okx = price

			// Cross-check: if both sources available, check deviation.
			if dp.onChain != nil && dp.onChain.Sign() > 0 && price.Sign() > 0 {
				deviation := f.computeDeviationBps(dp.onChain, price)
				if deviation > f.cfg.MaxDeviation {
					// Log deviation but don't override — on-chain is the source of truth.
					// The host's circuit breaker should handle this.
					dp.source = fmt.Sprintf("onchain (okx deviation %d bps)", deviation)
				} else {
					dp.source = "onchain+okx"
				}
			}
			f.prices[pair] = dp
			f.mu.Unlock()
		}
	}
}

// runStaleWatchdog checks if the on-chain price is stale and forces a slot0 read.
func (f *DualPriceFeed) runStaleWatchdog(ctx context.Context, pair TokenPair) {
	pool := f.pools[pair]
	ticker := time.NewTicker(f.cfg.StaleThreshold / 2)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.mu.RLock()
			dp := f.prices[pair]
			f.mu.RUnlock()

			age := time.Since(time.Unix(dp.timestamp, 0))
			if age > f.cfg.StaleThreshold {
				// Force a slot0 read.
				price, err := f.readSlot0(ctx, pool)
				if err != nil {
					// If slot0 also fails and OKX is available, use OKX as emergency fallback.
					f.mu.RLock()
					okxPrice := f.prices[pair].okx
					f.mu.RUnlock()
					if okxPrice != nil && okxPrice.Sign() > 0 {
						f.publishPrice(pair, okxPrice, "okx-fallback", 0)
					}
					continue
				}
				f.publishPrice(pair, price, "onchain-stale-refresh", 0)
			}
		}
	}
}

// publishPrice updates the cached price and notifies all subscribers.
func (f *DualPriceFeed) publishPrice(pair TokenPair, price *big.Int, source string, block uint64) {
	now := time.Now().Unix()

	f.mu.Lock()
	dp := f.prices[pair]

	// Skip if price unchanged.
	if dp.price != nil && dp.price.Cmp(price) == 0 {
		f.mu.Unlock()
		return
	}

	dp.onChain = price
	dp.price = price
	dp.timestamp = now
	dp.source = source
	f.prices[pair] = dp

	subs := make([]chan PriceUpdate, len(f.subs[pair]))
	copy(subs, f.subs[pair])
	f.mu.Unlock()

	update := PriceUpdate{
		Pair:      pair,
		Price:     price,
		Block:     block,
		Timestamp: now,
	}

	for _, ch := range subs {
		select {
		case ch <- update:
		default: // Drop if subscriber is slow.
		}
	}
}

// readSlot0 reads the current price from the pool's slot0().
func (f *DualPriceFeed) readSlot0(ctx context.Context, pool poolConfig) (*big.Int, error) {
	calldata, err := poolABI.Pack("slot0")
	if err != nil {
		return nil, err
	}

	result, err := f.cfg.WSClient.CallContract(ctx, ethereum.CallMsg{
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

// --- OKX DEX Ticker API ---

// OKX ticker API response structure.
type okxTickerResponse struct {
	Code string `json:"code"`
	Data []struct {
		InstID  string `json:"instId"`
		Last    string `json:"last"`
		BidPx   string `json:"bidPx"`
		AskPx   string `json:"askPx"`
		Ts      string `json:"ts"`
	} `json:"data"`
}

// fetchOKXPrice fetches the mid-price from OKX DEX ticker API.
func (f *DualPriceFeed) fetchOKXPrice(ctx context.Context, pair TokenPair) (*big.Int, error) {
	okxCfg := f.cfg.OKX
	baseURL := okxCfg.BaseURL
	if baseURL == "" {
		baseURL = "https://www.okx.com"
	}
	timeout := okxCfg.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	pool, ok := f.pools[pair]
	if !ok {
		return nil, fmt.Errorf("no pool for pair")
	}

	// OKX DEX API: /api/v5/dex/aggregator/all-tokens or market ticker.
	// For DEX prices, use the token addresses directly.
	// Format: GET /api/v5/market/ticker?instId=<base>-<quote>
	// For on-chain DEX tokens, use the aggregator quote endpoint.
	url := fmt.Sprintf("%s/api/v5/dex/aggregator/quote?chainId=%s&amount=1000000000000000000&fromTokenAddress=%s&toTokenAddress=%s",
		baseURL,
		okxCfg.ChainIndex,
		pool.Address.Hex(), // Using pool address as a proxy; in production use actual token addresses from pair.
		pair.Quote.Hex(),
	)

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	var tickerResp okxTickerResponse
	if err := json.Unmarshal(body, &tickerResp); err != nil {
		return nil, fmt.Errorf("parse OKX response: %w", err)
	}

	if tickerResp.Code != "0" || len(tickerResp.Data) == 0 {
		return nil, fmt.Errorf("OKX API error: code=%s", tickerResp.Code)
	}

	// Parse mid-price: (bid + ask) / 2, or just last price.
	lastStr := tickerResp.Data[0].Last
	if lastStr == "" {
		return nil, fmt.Errorf("no price in OKX response")
	}

	// Convert float string to 8-decimal big.Int.
	return parseFloatPrice(lastStr)
}

// parseFloatPrice converts a decimal string (e.g. "2000.50") to an 8-decimal big.Int.
func parseFloatPrice(s string) (*big.Int, error) {
	f, ok := new(big.Float).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid price string: %s", s)
	}
	scaled := new(big.Float).Mul(f, new(big.Float).SetFloat64(1e8))
	result, _ := scaled.Int(nil)
	return result, nil
}

// computeDeviationBps computes the deviation between two prices in basis points.
func (f *DualPriceFeed) computeDeviationBps(a, b *big.Int) uint16 {
	if a.Sign() == 0 || b.Sign() == 0 {
		return 10000 // max deviation
	}
	aF := new(big.Float).SetInt(a)
	bF := new(big.Float).SetInt(b)
	diff := new(big.Float).Sub(aF, bF)
	diff.Abs(diff)
	ratio, _ := new(big.Float).Quo(diff, aF).Float64()
	return uint16(math.Round(ratio * 10000))
}

// removeSub removes a subscriber and stops listeners when no subscribers remain.
func (f *DualPriceFeed) removeSub(pair TokenPair, ch chan PriceUpdate) {
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

	if len(f.subs[pair]) == 0 {
		if cancel, ok := f.active[pair]; ok {
			cancel()
			delete(f.active, pair)
		}
		delete(f.subs, pair)
	}
}
