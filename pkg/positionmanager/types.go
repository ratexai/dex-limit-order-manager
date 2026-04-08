// Package positionmanager provides a CEX-like position management library
// with automatic stop-loss and take-profit execution on EVM DEXes.
//
// It is designed as a library that integrates into a host application
// (e.g. RateXAI finance layer) via dependency injection through interfaces.
package positionmanager

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// Direction of the position.
type Direction uint8

const (
	Long  Direction = iota // Buy base, sell on SL/TP.
	Short                  // Sell base, buy on SL/TP.
)

func (d Direction) String() string {
	switch d {
	case Long:
		return "LONG"
	case Short:
		return "SHORT"
	default:
		return "UNKNOWN"
	}
}

// PositionState tracks the lifecycle of a position.
type PositionState uint8

const (
	StateActive        PositionState = iota // All levels live.
	StatePartialClosed                      // At least one TP fired, position still open.
	StateClosed                             // Fully closed (final TP or SL).
	StateCancelled                          // User-cancelled.
)

func (s PositionState) String() string {
	switch s {
	case StateActive:
		return "ACTIVE"
	case StatePartialClosed:
		return "PARTIAL_CLOSED"
	case StateClosed:
		return "CLOSED"
	case StateCancelled:
		return "CANCELLED"
	default:
		return "UNKNOWN"
	}
}

// IsTerminal returns true if the position cannot transition further.
func (s PositionState) IsTerminal() bool {
	return s == StateClosed || s == StateCancelled
}

// LevelType distinguishes stop-loss from take-profit.
type LevelType uint8

const (
	LevelTypeSL LevelType = iota // Stop-Loss.
	LevelTypeTP                  // Take-Profit.
)

func (t LevelType) String() string {
	if t == LevelTypeSL {
		return "SL"
	}
	return "TP"
}

// LevelStatus tracks execution state of a single level.
type LevelStatus uint8

const (
	LevelActive    LevelStatus = iota // Waiting for trigger.
	LevelTriggered                    // Executed on-chain.
	LevelCancelled                    // Cancelled (by SL fire or user).
	LevelSuspended                    // Permit expired, waiting for renewal.
)

// Priority determines gas strategy for execution.
type Priority uint8

const (
	PriorityCritical Priority = iota // SL — aggressive gas.
	PriorityNormal                   // TP — standard gas.
)

// TokenPair identifies a trading pair.
type TokenPair struct {
	Base    common.Address // e.g. WETH
	Quote   common.Address // e.g. USDC
	ChainID uint64
}

// Position represents a user's trading position with SL/TP levels.
type Position struct {
	ID            [16]byte       // UUID.
	Owner         common.Address // User wallet address.
	TokenBase     common.Address // Base token (e.g. WETH).
	TokenQuote    common.Address // Quote token (e.g. USDC).
	Direction     Direction
	TotalSize     *big.Int // Initial size in base token (wei).
	RemainingSize *big.Int // Current remaining size (wei).
	EntryPrice    *big.Int // Price at entry (quote per base, 8 decimals).
	State         PositionState
	ChainID       uint64
	PoolFee       uint32 // Uniswap V3 fee tier (500, 3000, 10000).
	DecimalsBase  uint8  // Token decimals for base (e.g. 18 for WETH).
	DecimalsQuote uint8  // Token decimals for quote (e.g. 6 for USDC).
	Levels        []Level
	CreatedAt     int64 // Unix timestamp.
	UpdatedAt     int64

	// Permit2 authorization data (filled at position creation).
	PermitSignature []byte         // EIP-712 signature (65 bytes: r || s || v).
	PermitNonce     *big.Int       // Permit2 nonce used for this position.
	PermitDeadline  int64          // Unix timestamp expiry of the permit.
	PermitAmount    *big.Int       // Amount authorized (= TotalSize at creation).
	PermitToken     common.Address // Token authorized (tokenIn for direction).
	PermitActivated bool           // Whether the on-chain Permit2 allowance has been set.
}

// Pair returns the TokenPair for this position.
func (p *Position) Pair() TokenPair {
	return TokenPair{Base: p.TokenBase, Quote: p.TokenQuote, ChainID: p.ChainID}
}

// ActiveLevels returns levels that are still active (not triggered or cancelled).
func (p *Position) ActiveLevels() []Level {
	var result []Level
	for _, l := range p.Levels {
		if l.Status == LevelActive {
			result = append(result, l)
		}
	}
	return result
}

// ActiveSL returns the currently active stop-loss level, or nil.
func (p *Position) ActiveSL() *Level {
	for i := range p.Levels {
		if p.Levels[i].Type == LevelTypeSL && p.Levels[i].Status == LevelActive {
			return &p.Levels[i]
		}
	}
	return nil
}

// Level represents a single SL or TP trigger level.
type Level struct {
	Index        int        // Position within the levels slice.
	Type         LevelType  // SL or TP.
	TriggerPrice *big.Int   // Quote per base, 8 decimals.
	PortionBps   uint16     // Basis points of remaining size (10000 = 100%).
	Status       LevelStatus
	MoveSLTo     *big.Int // After this TP triggers, move SL to this price. Zero = don't move.
	CancelOnFire []int    // Level indices to cancel when this fires.

	// Execution results (filled after trigger).
	ExecTxHash common.Hash
	ExecPrice  *big.Int // Actual execution price.
	ExecAmount *big.Int // Actual amount swapped.
	ExecAt     int64    // Unix timestamp.
}

// OpenParams are the parameters for opening a new position.
type OpenParams struct {
	Owner      common.Address
	TokenBase  common.Address
	TokenQuote common.Address
	Direction  Direction
	Size       *big.Int // Total size in base token (wei).
	EntryPrice *big.Int // Price at entry (8 decimals).
	ChainID       uint64
	PoolFee       uint32 // Uniswap V3 fee tier.
	DecimalsBase  uint8  // Token decimals for base (e.g. 18 for WETH).
	DecimalsQuote uint8  // Token decimals for quote (e.g. 6 for USDC).
	Levels        []LevelParams

	// Permit2 authorization (signed by user at position creation).
	PermitSignature []byte   // EIP-712 signature (65 bytes).
	PermitNonce     *big.Int // Permit2 nonce.
	PermitDeadline  int64    // Unix timestamp expiry.
	PermitAmount    *big.Int // Amount the user signed for (must be >= Size).

	// Optional: signed approve TX for one-click flow.
	// If present, keeper broadcasts this before activating permit.
	// Frontend silently signs token.approve(Permit2, MAX) with user's local key.
	SignedApproveTx []byte // RLP-encoded signed transaction, or nil.
}

// LevelParams defines a level at position creation time.
type LevelParams struct {
	Type         LevelType
	TriggerPrice *big.Int // Quote per base, 8 decimals.
	PortionBps   uint16   // Basis points of remaining size.
	MoveSLTo     *big.Int // After TP: move SL to this price. Zero = don't move.
	CancelOnFire []int    // Level indices to cancel.
}

// MarketSwapParams are the parameters for an immediate market swap.
type MarketSwapParams struct {
	Owner       common.Address
	TokenIn     common.Address
	TokenOut    common.Address
	AmountIn    *big.Int
	ChainID     uint64
	PoolFee     uint32
	DecimalsIn  uint8  // Token decimals for input token.
	DecimalsOut uint8  // Token decimals for output token.
	SlippageBps uint16 // Max slippage in bps (50 = 0.5%).

	// Permit2 SignatureTransfer (signed by user for this market swap).
	PermitSignature []byte   // EIP-712 signature (65 bytes).
	PermitNonce     *big.Int // Permit2 nonce.
	PermitDeadline  int64    // Unix timestamp (short, e.g. 5 minutes).

	// Optional: signed approve TX for one-click flow.
	SignedApproveTx []byte // RLP-encoded signed transaction, or nil.

	// Native ETH: if true, user sends raw ETH and SwapExecutorV2 wraps to WETH.
	NativeETH bool
}

// SwapResult is the outcome of a market swap.
type SwapResult struct {
	TxHash    common.Hash
	AmountIn  *big.Int
	AmountOut *big.Int
	Fee       FeeResult
}
