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

// RateLimitedClient wraps a ChainClient with a token-bucket rate limiter
// to prevent RPC throttling. The host configures the rate when constructing
// the ChainInstance.
//
// Usage:
//
//	client := positionmanager.NewRateLimitedClient(ethclient, 50) // 50 RPC/sec
//	cfg.Chains[1] = ChainInstance{Client: client, ...}
type RateLimitedClient struct {
	inner   ChainClient
	tokens  chan struct{}
	done    chan struct{}
	closeOnce sync.Once
}

// NewRateLimitedClient creates a rate-limited wrapper around a ChainClient.
// rps is the maximum requests per second (0 = unlimited).
func NewRateLimitedClient(inner ChainClient, rps int) *RateLimitedClient {
	if rps <= 0 {
		return &RateLimitedClient{inner: inner, done: make(chan struct{})}
	}

	rl := &RateLimitedClient{
		inner:  inner,
		tokens: make(chan struct{}, rps),
		done:   make(chan struct{}),
	}

	// Fill the bucket initially.
	for i := 0; i < rps; i++ {
		rl.tokens <- struct{}{}
	}

	// Refill at a steady rate.
	go func() {
		interval := time.Second / time.Duration(rps)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-rl.done:
				return
			case <-ticker.C:
				select {
				case rl.tokens <- struct{}{}:
				default: // Bucket full.
				}
			}
		}
	}()

	return rl
}

// Close stops the refill goroutine.
func (rl *RateLimitedClient) Close() {
	rl.closeOnce.Do(func() { close(rl.done) })
}

func (rl *RateLimitedClient) acquire(ctx context.Context) error {
	if rl.tokens == nil {
		return nil // Unlimited.
	}
	select {
	case <-rl.tokens:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (rl *RateLimitedClient) SendTransaction(ctx context.Context, tx *types.Transaction) error {
	if err := rl.acquire(ctx); err != nil {
		return err
	}
	return rl.inner.SendTransaction(ctx, tx)
}

func (rl *RateLimitedClient) SuggestGasPrice(ctx context.Context) (*big.Int, error) {
	if err := rl.acquire(ctx); err != nil {
		return nil, err
	}
	return rl.inner.SuggestGasPrice(ctx)
}

func (rl *RateLimitedClient) SuggestGasTipCap(ctx context.Context) (*big.Int, error) {
	if err := rl.acquire(ctx); err != nil {
		return nil, err
	}
	return rl.inner.SuggestGasTipCap(ctx)
}

func (rl *RateLimitedClient) PendingNonceAt(ctx context.Context, account common.Address) (uint64, error) {
	if err := rl.acquire(ctx); err != nil {
		return 0, err
	}
	return rl.inner.PendingNonceAt(ctx, account)
}

func (rl *RateLimitedClient) EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error) {
	if err := rl.acquire(ctx); err != nil {
		return 0, err
	}
	return rl.inner.EstimateGas(ctx, call)
}

func (rl *RateLimitedClient) CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error) {
	if err := rl.acquire(ctx); err != nil {
		return nil, err
	}
	return rl.inner.CallContract(ctx, call, blockNumber)
}

func (rl *RateLimitedClient) ChainID(ctx context.Context) (*big.Int, error) {
	if err := rl.acquire(ctx); err != nil {
		return nil, err
	}
	return rl.inner.ChainID(ctx)
}

func (rl *RateLimitedClient) TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	if err := rl.acquire(ctx); err != nil {
		return nil, err
	}
	return rl.inner.TransactionReceipt(ctx, txHash)
}
