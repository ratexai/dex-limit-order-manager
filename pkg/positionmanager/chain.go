package positionmanager

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// ChainClient abstracts blockchain interactions.
// go-ethereum's *ethclient.Client already satisfies this interface,
// so the host can pass it directly.
type ChainClient interface {
	// SendTransaction broadcasts a signed transaction.
	SendTransaction(ctx context.Context, tx *types.Transaction) error

	// SuggestGasPrice returns the suggested gas price.
	SuggestGasPrice(ctx context.Context) (*big.Int, error)

	// PendingNonceAt returns the next nonce for the account.
	PendingNonceAt(ctx context.Context, account common.Address) (uint64, error)

	// EstimateGas estimates the gas needed for a call.
	EstimateGas(ctx context.Context, call ethereum.CallMsg) (uint64, error)

	// CallContract executes a read-only contract call.
	CallContract(ctx context.Context, call ethereum.CallMsg, blockNumber *big.Int) ([]byte, error)

	// ChainID returns the chain ID.
	ChainID(ctx context.Context) (*big.Int, error)

	// TransactionReceipt returns the receipt for a mined transaction.
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error)
}
