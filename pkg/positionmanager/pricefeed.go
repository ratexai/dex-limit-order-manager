package positionmanager

import (
	"context"
	"math/big"
)

// PriceFeed provides real-time token prices.
// The host application can use the reference Uniswap V3 implementation
// (pricefeed_uniswapv3.go) or provide its own (e.g. if it already
// aggregates prices from multiple sources).
type PriceFeed interface {
	// Subscribe returns a channel that receives price updates for a pair.
	// The channel is closed when ctx is cancelled.
	Subscribe(ctx context.Context, pair TokenPair) (<-chan PriceUpdate, error)

	// Latest returns the most recent known price for a pair.
	// Returns error if the pair is not being tracked.
	Latest(pair TokenPair) (price *big.Int, timestamp int64, err error)
}

// PriceUpdate is a single price observation.
type PriceUpdate struct {
	Pair      TokenPair
	Price     *big.Int // Quote per base, 8 decimals.
	Block     uint64
	Timestamp int64
}
