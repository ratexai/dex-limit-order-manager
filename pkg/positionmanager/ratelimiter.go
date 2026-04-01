package positionmanager

import (
	"context"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// RPCRateLimiter is a token-bucket rate limiter for RPC calls.
// Attach it to a ChainInstance to throttle outgoing RPC requests.
type RPCRateLimiter struct {
	mu       sync.Mutex
	tokens   float64
	maxBurst float64
	rate     float64 // tokens per second
	lastTime time.Time
}

// NewRPCRateLimiter creates a rate limiter.
// ratePerSec: sustained requests/second. burst: max burst size.
func NewRPCRateLimiter(ratePerSec float64, burst int) *RPCRateLimiter {
	return &RPCRateLimiter{
		tokens:   float64(burst),
		maxBurst: float64(burst),
		rate:     ratePerSec,
		lastTime: time.Now(),
	}
}

// Wait blocks until a token is available or ctx is cancelled.
func (rl *RPCRateLimiter) Wait(ctx context.Context) error {
	for {
		rl.mu.Lock()
		now := time.Now()
		elapsed := now.Sub(rl.lastTime).Seconds()
		rl.tokens += elapsed * rl.rate
		if rl.tokens > rl.maxBurst {
			rl.tokens = rl.maxBurst
		}
		rl.lastTime = now

		if rl.tokens >= 1.0 {
			rl.tokens -= 1.0
			rl.mu.Unlock()
			return nil
		}

		// Calculate wait time for next token.
		deficit := 1.0 - rl.tokens
		waitDur := time.Duration(deficit / rl.rate * float64(time.Second))
		rl.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitDur):
		}
	}
}

// Allow returns true if a token is available (non-blocking).
func (rl *RPCRateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastTime).Seconds()
	rl.tokens += elapsed * rl.rate
	if rl.tokens > rl.maxBurst {
		rl.tokens = rl.maxBurst
	}
	rl.lastTime = now

	if rl.tokens >= 1.0 {
		rl.tokens -= 1.0
		return true
	}
	return false
}

// RateLimitedClient wraps a ChainClient with rate limiting.
type RateLimitedClient struct {
	inner   ChainClient
	limiter *RPCRateLimiter
}

// NewRateLimitedClient wraps an existing ChainClient with rate limiting.
func NewRateLimitedClient(client ChainClient, limiter *RPCRateLimiter) *RateLimitedClient {
	return &RateLimitedClient{inner: client, limiter: limiter}
}

func (c *RateLimitedClient) wait(ctx context.Context) error {
	return c.limiter.Wait(ctx)
}

func (c *RateLimitedClient) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	if err := c.wait(ctx); err != nil {
		return err
	}
	return c.inner.SendTransaction(ctx, tx)
}

func (c *RateLimitedClient) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.SuggestGasPrice(ctx)
}

func (c *RateLimitedClient) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.SuggestGasTipCap(ctx)
}

func (c *RateLimitedClient) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	if err := c.wait(ctx); err != nil {
		return 0, err
	}
	return c.inner.PendingNonceAt(ctx, account)
}

func (c *RateLimitedClient) EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error) {
	if err := c.wait(ctx); err != nil {
		return 0, err
	}
	return c.inner.EstimateGas(ctx, call)
}

func (c *RateLimitedClient) CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.CallContract(ctx, call, blockNumber)
}

func (c *RateLimitedClient) ChainID(ctx context.Context) (*big.Int, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.ChainID(ctx)
}

func (c *RateLimitedClient) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	if err := c.wait(ctx); err != nil {
		return nil, err
	}
	return c.inner.TransactionReceipt(ctx, txHash)
}
