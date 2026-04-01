package positionmanager

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// ExecutionEvent is emitted to the host after a level is executed on-chain.
// The host uses this for logging, analytics, notifications, and referral tracking.
type ExecutionEvent struct {
	// Position context.
	PositionID [16]byte
	Owner      common.Address
	ChainID    uint64

	// What was executed.
	LevelIndex int
	LevelType  LevelType
	Direction  Direction

	// Swap details.
	TokenIn    common.Address
	TokenOut   common.Address
	AmountIn   *big.Int
	AmountOut  *big.Int
	ExecPrice  *big.Int // Actual execution price (8 decimals).
	TxHash     common.Hash

	// Fee breakdown for accounting and referral payouts.
	Fee FeeResult

	// Post-execution state.
	RemainingSize *big.Int
	PositionState PositionState
	SLMovedTo     *big.Int // New SL trigger price after this TP (zero if N/A).
}

// ErrorEvent is emitted when execution fails.
type ErrorEvent struct {
	PositionID [16]byte
	LevelIndex int
	ChainID    uint64
	Err        error
	Retryable  bool
}

// PermitExpiryEvent is emitted when a position's Permit2 permit is approaching expiry.
// The host should notify the user to renew the permit via the /renew endpoint.
type PermitExpiryEvent struct {
	PositionID     [16]byte
	Owner          common.Address
	ChainID        uint64
	PermitDeadline int64  // Unix timestamp when permit expires.
	HoursRemaining int    // Hours until expiry.
	ActiveLevels   int    // Number of active levels that will be suspended if not renewed.
}
