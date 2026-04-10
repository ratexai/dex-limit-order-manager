package positionmanager

import (
	"crypto/ecdsa"
	"log/slog"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// Config is the top-level configuration for the position manager library.
type Config struct {
	// Store is the persistence backend (host provides).
	Store Store

	// PriceFeed provides real-time prices (host provides or use reference impl).
	PriceFeed PriceFeed

	// FeeProvider resolves per-user fee tiers and referral info (host provides).
	FeeProvider FeeProvider

	// Chains maps chainID to chain-specific configuration and client.
	Chains map[uint64]ChainInstance

	// Logger for structured logging. If nil, a no-op logger is used.
	// Host provides (e.g. slog.Default() or custom handler).
	Logger *slog.Logger

	// Metrics collects operational metrics (trigger counts, latency, gas, errors).
	// Host provides a Prometheus-backed implementation. Nil = no-op.
	Metrics MetricsCollector

	// OnExecution is called after each successful level execution.
	// Host uses this for logging, notifications, referral tracking.
	OnExecution func(ExecutionEvent)

	// OnError is called when an execution fails.
	OnError func(ErrorEvent)

	// OnPermitExpiring is called when a position's permit is approaching expiry.
	// Host uses this to notify the user to renew via the /renew endpoint.
	OnPermitExpiring func(PermitExpiryEvent)
}

// ChainInstance bundles the chain client with chain-specific configuration.
type ChainInstance struct {
	// Client is the blockchain RPC client (go-ethereum *ethclient.Client works).
	Client ChainClient

	// KeeperKey is the private key of the keeper EOA for this chain.
	KeeperKey *ecdsa.PrivateKey

	// ExecutorAddress is the deployed SwapExecutor contract address.
	ExecutorAddress common.Address

	// Chain-specific parameters.
	ChainConfig
}

// ChainConfig holds chain-specific parameters for gas, slippage, and timing.
type ChainConfig struct {
	ChainID   uint64
	Name      string
	BlockTime time.Duration

	// Execution.
	ExecutorWorkers int // Concurrent executor goroutines.

	// Gas strategy.
	SLGasMultiplier float64  // Base fee multiplier for SL (aggressive).
	TPGasMultiplier float64  // Base fee multiplier for TP (normal).
	MaxGasPrice     *big.Int // Hard cap on gas price (wei).

	// Slippage.
	SLSlippageBps uint16 // Max slippage for SL execution (bps).
	TPSlippageBps uint16 // Max slippage for TP execution (bps).

	// MEV protection.
	UseFlashbots   bool
	FlashbotsRelay string

	// Retry.
	MaxRetries         int
	RetryGasEscalation float64 // Gas multiplier on each retry (e.g. 1.5).

	// CircuitBreaker config for executor resilience. Zero value = use defaults.
	CircuitBreaker CircuitBreakerConfig

	// Permit2.
	Permit2Address      common.Address // Canonical: 0x000000000022D473030F116dDEE9F6B43aC78BA3.
	MinPermitLifetime   time.Duration  // Min remaining lifetime for new permits (default: 1h).
	PermitExpiryWarning time.Duration  // Warn host this long before permit expiry (default: 48h).
}

// EthereumDefaults returns sensible defaults for Ethereum mainnet.
func EthereumDefaults() ChainConfig {
	return ChainConfig{
		ChainID:             1,
		Name:                "ethereum",
		BlockTime:           12 * time.Second,
		ExecutorWorkers:     4,
		SLGasMultiplier:     2.5,
		TPGasMultiplier:     1.3,
		MaxGasPrice:         new(big.Int).Mul(big.NewInt(200), big.NewInt(1e9)), // 200 gwei
		SLSlippageBps:       200,
		TPSlippageBps:       50,
		UseFlashbots:        true,
		FlashbotsRelay:      "https://relay.flashbots.net",
		MaxRetries:          3,
		RetryGasEscalation:  1.5,
		Permit2Address:      common.HexToAddress("0x000000000022D473030F116dDEE9F6B43aC78BA3"),
		MinPermitLifetime:   1 * time.Hour,
		PermitExpiryWarning: 48 * time.Hour,
	}
}

// BaseDefaults returns sensible defaults for Base mainnet.
func BaseDefaults() ChainConfig {
	return ChainConfig{
		ChainID:             8453,
		Name:                "base",
		BlockTime:           2 * time.Second,
		ExecutorWorkers:     8,
		SLGasMultiplier:     2.0,
		TPGasMultiplier:     1.5,
		MaxGasPrice:         new(big.Int).Mul(big.NewInt(1), big.NewInt(1e9)), // 1 gwei
		SLSlippageBps:       200,
		TPSlippageBps:       50,
		UseFlashbots:        false,
		MaxRetries:          3,
		RetryGasEscalation:  1.5,
		Permit2Address:      common.HexToAddress("0x000000000022D473030F116dDEE9F6B43aC78BA3"),
		MinPermitLifetime:   1 * time.Hour,
		PermitExpiryWarning: 48 * time.Hour,
	}
}

// BSCDefaults returns sensible defaults for BSC mainnet (PancakeSwap V3).
func BSCDefaults() ChainConfig {
	return ChainConfig{
		ChainID:             56,
		Name:                "bsc",
		BlockTime:           3 * time.Second,
		ExecutorWorkers:     6,
		SLGasMultiplier:     2.0,
		TPGasMultiplier:     1.5,
		MaxGasPrice:         new(big.Int).Mul(big.NewInt(5), big.NewInt(1e9)), // 5 gwei
		SLSlippageBps:       200,
		TPSlippageBps:       50,
		UseFlashbots:        false, // No Flashbots on BSC, use private RPCs if needed.
		MaxRetries:          3,
		RetryGasEscalation:  1.5,
		Permit2Address:      common.HexToAddress("0x31c2F6fcFf4F8759b3Bd5Bf0e1084A055615c768"),
		MinPermitLifetime:   1 * time.Hour,
		PermitExpiryWarning: 48 * time.Hour,
	}
}
