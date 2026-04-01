package positionmanager

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"
	"time"

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
	circuitBreaker  *CircuitBreaker

	nonceMu   sync.Mutex
	nextNonce uint64
	nonceInit bool
}

func newExecutor(client ChainClient, keeperKey *ecdsa.PrivateKey, executorAddr common.Address, chainID uint64, cfg ChainConfig) *executor {
	var cb *CircuitBreaker
	if cfg.CircuitBreaker.MaxFailures > 0 {
		cb = NewCircuitBreaker(cfg.CircuitBreaker)
	} else {
		cb = NewCircuitBreaker(DefaultCircuitBreakerConfig())
	}
	return &executor{
		client:         client,
		keeperKey:      keeperKey,
		keeperAddr:     crypto.PubkeyToAddress(keeperKey.PublicKey),
		executorAddr:   executorAddr,
		chainID:        new(big.Int).SetUint64(chainID),
		cfg:            cfg,
		circuitBreaker: cb,
	}
}

// executeSwapParams holds the parameters for a single swap execution.
type executeSwapParams struct {
	User        common.Address
	TokenIn     common.Address
	TokenOut    common.Address
	PoolFee     uint32
	AmountIn    *big.Int
	MinAmountOut *big.Int
	FeeBps      uint16
	Priority    Priority
}

// executeSwap calls SwapExecutor.executeSwap on-chain, waits for the transaction
// to be mined, and verifies the receipt status. Returns the tx hash, receipt, and error.
func (e *executor) executeSwap(ctx context.Context, params executeSwapParams) (common.Hash, *types.Receipt, error) {
	// ABI expects uint24 for poolFee and uint16 for feeBps — go-ethereum
	// maps sub-32-bit uints to *big.Int, so we convert here.
	poolFeeBig := new(big.Int).SetUint64(uint64(params.PoolFee))
	feeBpsBig := new(big.Int).SetUint64(uint64(params.FeeBps))

	calldata, err := parsedSwapExecutorABI.Pack(
		"executeSwap",
		params.User,
		params.TokenIn,
		params.TokenOut,
		poolFeeBig,
		params.AmountIn,
		params.MinAmountOut,
		feeBpsBig,
	)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("pack calldata: %w", err)
	}

	gasTipCap, maxFeeCap, err := e.suggestGasFees(ctx, params.Priority)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("suggest gas fees: %w", err)
	}

	gasLimit, err := e.client.EstimateGas(ctx, ethereum.CallMsg{
		From:      e.keeperAddr,
		To:        &e.executorAddr,
		GasFeeCap: maxFeeCap,
		GasTipCap: gasTipCap,
		Data:      calldata,
	})
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("estimate gas: %w", err)
	}

	// Add buffer to gas limit.
	gasLimit = e.applyGasBuffer(gasLimit, params.Priority)

	nonce, err := e.acquireNonce(ctx)
	if err != nil {
		return common.Hash{}, nil, fmt.Errorf("acquire nonce: %w", err)
	}

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   e.chainID,
		Nonce:     nonce,
		GasTipCap: gasTipCap,
		GasFeeCap: maxFeeCap,
		Gas:       gasLimit,
		To:        &e.executorAddr,
		Value:     big.NewInt(0),
		Data:      calldata,
	})

	signer := types.LatestSignerForChainID(e.chainID)
	signedTx, err := types.SignTx(tx, signer, e.keeperKey)
	if err != nil {
		e.rollbackNonce(nonce)
		return common.Hash{}, nil, fmt.Errorf("sign tx: %w", err)
	}

	if err := e.client.SendTransaction(ctx, signedTx); err != nil {
		e.rollbackNonce(nonce)
		return common.Hash{}, nil, fmt.Errorf("send tx: %w", err)
	}

	txHash := signedTx.Hash()

	// Wait for the transaction to be mined and verify success.
	receipt, err := e.waitForReceipt(ctx, txHash)
	if err != nil {
		return txHash, nil, fmt.Errorf("wait for receipt: %w", err)
	}
	if receipt.Status == 0 {
		return txHash, receipt, fmt.Errorf("tx reverted: %s", txHash.Hex())
	}

	return txHash, receipt, nil
}

// waitForReceipt waits for a transaction to be mined and returns the receipt.
func (e *executor) waitForReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	ticker := time.NewTicker(e.cfg.BlockTime / 2)
	defer ticker.Stop()

	for {
		receipt, err := e.client.TransactionReceipt(ctx, txHash)
		if err == nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// suggestGasFees returns EIP-1559 gas parameters (gasTipCap, gasFeeCap) with priority multiplier.
func (e *executor) suggestGasFees(ctx context.Context, priority Priority) (gasTipCap, gasFeeCap *big.Int, err error) {
	baseFee, err := e.client.SuggestGasPrice(ctx)
	if err != nil {
		return nil, nil, err
	}
	tipCap, err := e.client.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, err
	}

	multiplier := e.cfg.TPGasMultiplier
	if priority == PriorityCritical {
		multiplier = e.cfg.SLGasMultiplier
	}

	// Apply multiplier to tip for priority escalation.
	mult := int64(multiplier * 100)
	gasTipCap = new(big.Int).Mul(tipCap, big.NewInt(mult))
	gasTipCap.Div(gasTipCap, big.NewInt(100))

	// maxFee = baseFee * multiplier + gasTipCap
	gasFeeCap = new(big.Int).Mul(baseFee, big.NewInt(mult))
	gasFeeCap.Div(gasFeeCap, big.NewInt(100))
	gasFeeCap.Add(gasFeeCap, gasTipCap)

	// Enforce max gas price cap on gasFeeCap.
	if e.cfg.MaxGasPrice != nil && gasFeeCap.Cmp(e.cfg.MaxGasPrice) > 0 {
		gasFeeCap.Set(e.cfg.MaxGasPrice)
	}

	return gasTipCap, gasFeeCap, nil
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

// rollbackNonce decrements nonce after a failed sign or send, so the next call reuses it.
func (e *executor) rollbackNonce(nonce uint64) {
	e.nonceMu.Lock()
	e.nextNonce = nonce
	e.nonceMu.Unlock()
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

// parseAmountOutFromReceipt extracts the actual amountOut from the SwapExecuted event
// in the transaction receipt. Returns nil if the event is not found.
func parseAmountOutFromReceipt(receipt *types.Receipt, executorAddr common.Address) *big.Int {
	swapExecutedID := parsedSwapExecutorABI.Events["SwapExecuted"].ID
	for _, log := range receipt.Logs {
		if log.Address != executorAddr || len(log.Topics) == 0 || log.Topics[0] != swapExecutedID {
			continue
		}
		outputs, err := parsedSwapExecutorABI.Events["SwapExecuted"].Inputs.NonIndexed().Unpack(log.Data)
		if err != nil || len(outputs) < 2 {
			continue
		}
		// SwapExecuted non-indexed fields: amountIn, amountOut, feeAmount, feeBps
		if amountOut, ok := outputs[1].(*big.Int); ok {
			return new(big.Int).Set(amountOut)
		}
	}
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
