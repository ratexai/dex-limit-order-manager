package positionmanager

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// executor handles on-chain swap execution via the SwapExecutor contract.
type executor struct {
	client          ChainClient
	keeperKey       *ecdsa.PrivateKey
	keeperAddr      common.Address
	executorAddr    common.Address
	chainID         *big.Int
	cfg             ChainConfig

	nonceMu   sync.Mutex
	nextNonce uint64
	nonceInit bool
}

func newExecutor(client ChainClient, keeperKey *ecdsa.PrivateKey, executorAddr common.Address, chainID uint64, cfg ChainConfig) *executor {
	return &executor{
		client:       client,
		keeperKey:    keeperKey,
		keeperAddr:   crypto.PubkeyToAddress(keeperKey.PublicKey),
		executorAddr: executorAddr,
		chainID:      new(big.Int).SetUint64(chainID),
		cfg:          cfg,
	}
}

// executeSwapParams holds the parameters for a single swap execution.
type executeSwapParams struct {
	User        common.Address
	TokenIn     common.Address
	TokenOut    common.Address
	PoolFee     uint24
	AmountIn    *big.Int
	MinAmountOut *big.Int
	FeeBps      uint16
	Priority    Priority
}

// executeSwap calls SwapExecutor.executeSwap on-chain.
// Returns the tx hash on success. The caller is responsible for waiting for the receipt.
func (e *executor) executeSwap(ctx context.Context, params executeSwapParams) (common.Hash, error) {
	calldata, err := parsedSwapExecutorABI.Pack(
		"executeSwap",
		params.User,
		params.TokenIn,
		params.TokenOut,
		params.PoolFee,
		params.AmountIn,
		params.MinAmountOut,
		params.FeeBps,
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("pack calldata: %w", err)
	}

	gasPrice, err := e.suggestGasPrice(ctx, params.Priority)
	if err != nil {
		return common.Hash{}, fmt.Errorf("suggest gas price: %w", err)
	}

	gasLimit, err := e.client.EstimateGas(ctx, ethereum.CallMsg{
		From:     e.keeperAddr,
		To:       &e.executorAddr,
		GasPrice: gasPrice,
		Data:     calldata,
	})
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimate gas: %w", err)
	}

	// Add buffer to gas limit.
	gasLimit = e.applyGasBuffer(gasLimit, params.Priority)

	nonce, err := e.acquireNonce(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("acquire nonce: %w", err)
	}

	tx := types.NewTransaction(
		nonce,
		e.executorAddr,
		big.NewInt(0), // No ETH value.
		gasLimit,
		gasPrice,
		calldata,
	)

	signer := types.NewEIP155Signer(e.chainID)
	signedTx, err := types.SignTx(tx, signer, e.keeperKey)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign tx: %w", err)
	}

	if err := e.client.SendTransaction(ctx, signedTx); err != nil {
		return common.Hash{}, fmt.Errorf("send tx: %w", err)
	}

	return signedTx.Hash(), nil
}

// waitForReceipt waits for a transaction to be mined and returns the receipt.
func (e *executor) waitForReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	for {
		receipt, err := e.client.TransactionReceipt(ctx, txHash)
		if err == nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			// Poll again on next iteration.
		}
	}
}

// suggestGasPrice returns the gas price with the appropriate multiplier.
func (e *executor) suggestGasPrice(ctx context.Context, priority Priority) (*big.Int, error) {
	base, err := e.client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, err
	}

	multiplier := e.cfg.TPGasMultiplier
	if priority == PriorityCritical {
		multiplier = e.cfg.SLGasMultiplier
	}

	// Multiply: gasPrice = base * multiplier.
	// Use integer math: multiply by (multiplier * 100), divide by 100.
	mult := int64(multiplier * 100)
	gasPrice := new(big.Int).Mul(base, big.NewInt(mult))
	gasPrice.Div(gasPrice, big.NewInt(100))

	// Enforce max gas price cap.
	if e.cfg.MaxGasPrice != nil && gasPrice.Cmp(e.cfg.MaxGasPrice) > 0 {
		gasPrice.Set(e.cfg.MaxGasPrice)
	}

	return gasPrice, nil
}

// applyGasBuffer adds a safety buffer to the gas estimate.
func (e *executor) applyGasBuffer(estimate uint64, priority Priority) uint64 {
	if priority == PriorityCritical {
		return estimate * 3 / 2 // 1.5x for SL.
	}
	return estimate * 6 / 5 // 1.2x for TP.
}

// acquireNonce returns the next nonce and increments the counter.
func (e *executor) acquireNonce(ctx context.Context) (uint64, error) {
	e.nonceMu.Lock()
	defer e.nonceMu.Unlock()

	if !e.nonceInit {
		nonce, err := e.client.PendingNonceAt(ctx, e.keeperAddr)
		if err != nil {
			return 0, err
		}
		e.nextNonce = nonce
		e.nonceInit = true
	}

	nonce := e.nextNonce
	e.nextNonce++
	return nonce, nil
}

// resyncNonce resets the nonce from chain state (call after "nonce too low" error).
func (e *executor) resyncNonce(ctx context.Context) error {
	e.nonceMu.Lock()
	defer e.nonceMu.Unlock()

	nonce, err := e.client.PendingNonceAt(ctx, e.keeperAddr)
	if err != nil {
		return err
	}
	e.nextNonce = nonce
	e.nonceInit = true
	return nil
}

// computeMinAmountOut calculates the minimum acceptable output given a trigger price,
// an amount, and a slippage tolerance.
func computeMinAmountOut(triggerPrice *big.Int, amountIn *big.Int, slippageBps uint16, decimalsIn, decimalsOut uint8) *big.Int {
	// expectedOut = amountIn * triggerPrice / 10^8 (price has 8 decimals)
	// Adjust for token decimals: expectedOut = amountIn * triggerPrice * 10^decimalsOut / (10^8 * 10^decimalsIn)
	num := new(big.Int).Mul(amountIn, triggerPrice)

	decOut := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimalsOut)), nil)
	num.Mul(num, decOut)

	decIn := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimalsIn)), nil)
	priceDecimals := new(big.Int).Exp(big.NewInt(10), big.NewInt(8), nil)
	denom := new(big.Int).Mul(decIn, priceDecimals)

	expectedOut := new(big.Int).Div(num, denom)

	// Apply slippage: minOut = expectedOut * (10000 - slippageBps) / 10000
	minOut := new(big.Int).Mul(expectedOut, big.NewInt(int64(10000-slippageBps)))
	minOut.Div(minOut, big.NewInt(10000))

	return minOut
}
