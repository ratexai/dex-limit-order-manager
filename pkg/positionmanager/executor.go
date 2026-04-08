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

// ExecutionMode determines how tokens are pulled from the user.
type ExecutionMode uint8

const (
	ExecModeLegacy          ExecutionMode = iota // Direct ERC20 transferFrom.
	ExecModePermit2Allowance                     // Permit2 AllowanceTransfer (multi-level positions).
	ExecModePermit2Signature                     // Permit2 SignatureTransfer (single-use market swaps).
)

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
	Mode        ExecutionMode

	// Permit2 SignatureTransfer fields (Mode == ExecModePermit2Signature).
	PermitNonce    *big.Int
	PermitDeadline *big.Int
	PermitSignature []byte
}

// activatePermitParams holds the data needed to activate a Permit2 allowance on-chain.
type activatePermitParams struct {
	User        common.Address
	Token       common.Address
	Amount      *big.Int // uint160
	Expiration  uint64   // uint48
	Nonce       uint64   // uint48
	Spender     common.Address
	SigDeadline *big.Int
	Signature   []byte
	Priority    Priority
}

// executeSwap calls the appropriate SwapExecutor function on-chain based on the
// execution mode, waits for the transaction to be mined, and verifies the receipt.
func (e *executor) executeSwap(ctx context.Context, params executeSwapParams) (common.Hash, *types.Receipt, error) {
	calldata, err := e.packSwapCalldata(params)
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

// packSwapCalldata builds the ABI-encoded calldata for the appropriate swap function
// based on the execution mode.
func (e *executor) packSwapCalldata(params executeSwapParams) ([]byte, error) {
	// go-ethereum ABI packer expects native Go types matching Solidity types:
	// uint24 (poolFee) → *big.Int, uint16 (feeBps) → uint16
	poolFeeBig := new(big.Int).SetUint64(uint64(params.PoolFee))

	switch params.Mode {
	case ExecModeLegacy:
		// V1 contract: executeSwap(user, tokenIn, tokenOut, poolFee, amountIn, minAmountOut, feeBps)
		return parsedSwapExecutorABI.Pack(
			"executeSwap",
			params.User,
			params.TokenIn,
			params.TokenOut,
			poolFeeBig,
			params.AmountIn,
			params.MinAmountOut,
			params.FeeBps,
		)

	case ExecModePermit2Allowance:
		// V2 contract: executeSwapViaPermit2(user, tokenIn, tokenOut, poolFee, amountIn, minAmountOut, feeBps)
		return parsedSwapExecutorV2ABI.Pack(
			"executeSwapViaPermit2",
			params.User,
			params.TokenIn,
			params.TokenOut,
			poolFeeBig,
			params.AmountIn,
			params.MinAmountOut,
			params.FeeBps,
		)

	case ExecModePermit2Signature:
		// V2 contract: executeSwapWithSignature(user, tokenOut, poolFee, minAmountOut, feeBps, permitTransfer, signature)
		// The permitTransfer struct is encoded as a tuple: ((token, amount), nonce, deadline)
		type tokenPermissions struct {
			Token  common.Address
			Amount *big.Int
		}
		type permitTransferFrom struct {
			Permitted tokenPermissions
			Nonce     *big.Int
			Deadline  *big.Int
		}
		permit := permitTransferFrom{
			Permitted: tokenPermissions{
				Token:  params.TokenIn,
				Amount: params.AmountIn,
			},
			Nonce:    params.PermitNonce,
			Deadline: params.PermitDeadline,
		}
		return parsedSwapExecutorV2ABI.Pack(
			"executeSwapWithSignature",
			params.User,
			params.TokenOut,
			poolFeeBig,
			params.MinAmountOut,
			params.FeeBps,
			permit,
			params.PermitSignature,
		)

	default:
		return nil, fmt.Errorf("unknown execution mode: %d", params.Mode)
	}
}

// activatePermit submits a transaction to activate a Permit2 allowance on-chain.
// This is called once per position before the first level execution.
// Permit2 type bounds: prevent silent ABI truncation.
var maxUint48 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 48), big.NewInt(1))   // 2^48 - 1
var maxUint160 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1)) // 2^160 - 1

func (e *executor) activatePermit(ctx context.Context, params activatePermitParams) (common.Hash, error) {
	// Validate Permit2 type bounds to prevent silent ABI truncation.
	if params.Amount != nil && params.Amount.Cmp(maxUint160) > 0 {
		return common.Hash{}, fmt.Errorf("permit amount exceeds uint160 max")
	}
	if params.Expiration > maxUint48.Uint64() {
		return common.Hash{}, fmt.Errorf("permit expiration exceeds uint48 max")
	}
	if params.Nonce > maxUint48.Uint64() {
		return common.Hash{}, fmt.Errorf("permit nonce exceeds uint48 max")
	}

	// Build the PermitSingle struct for the ABI call.
	type permitDetails struct {
		Token      common.Address
		Amount     *big.Int
		Expiration *big.Int
		Nonce      *big.Int
	}
	type permitSingle struct {
		Details     permitDetails
		Spender     common.Address
		SigDeadline *big.Int
	}

	ps := permitSingle{
		Details: permitDetails{
			Token:      params.Token,
			Amount:     params.Amount,
			Expiration: new(big.Int).SetUint64(params.Expiration),
			Nonce:      new(big.Int).SetUint64(params.Nonce),
		},
		Spender:     params.Spender,
		SigDeadline: params.SigDeadline,
	}

	calldata, err := parsedSwapExecutorV2ABI.Pack(
		"activatePermit",
		params.User,
		ps,
		params.Signature,
	)
	if err != nil {
		return common.Hash{}, fmt.Errorf("pack activatePermit calldata: %w", err)
	}

	gasTipCap, maxFeeCap, err := e.suggestGasFees(ctx, params.Priority)
	if err != nil {
		return common.Hash{}, fmt.Errorf("suggest gas fees: %w", err)
	}

	gasLimit, err := e.client.EstimateGas(ctx, ethereum.CallMsg{
		From:      e.keeperAddr,
		To:        &e.executorAddr,
		GasFeeCap: maxFeeCap,
		GasTipCap: gasTipCap,
		Data:      calldata,
	})
	if err != nil {
		return common.Hash{}, fmt.Errorf("estimate gas: %w", err)
	}
	gasLimit = e.applyGasBuffer(gasLimit, params.Priority)

	nonce, err := e.acquireNonce(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("acquire nonce: %w", err)
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
		return common.Hash{}, fmt.Errorf("sign tx: %w", err)
	}

	if err := e.client.SendTransaction(ctx, signedTx); err != nil {
		e.rollbackNonce(nonce)
		return common.Hash{}, fmt.Errorf("send tx: %w", err)
	}

	txHash := signedTx.Hash()
	receipt, err := e.waitForReceipt(ctx, txHash)
	if err != nil {
		return txHash, fmt.Errorf("wait for receipt: %w", err)
	}
	if receipt.Status == 0 {
		return txHash, fmt.Errorf("activatePermit tx reverted: %s", txHash.Hex())
	}

	return txHash, nil
}

// broadcastSignedApproveTx broadcasts a user-signed approve TX (for one-click flow).
// The frontend silently signs token.approve(Permit2, MAX) and sends the signed TX bytes.
// Keeper broadcasts it and waits for confirmation before proceeding with the swap.
//
// Security: validates that the TX calldata is an ERC20 approve() call (selector 0x095ea7b3)
// targeting the Permit2 canonical address. Rejects any other function or spender.
func (e *executor) broadcastSignedApproveTx(ctx context.Context, signedTxBytes []byte) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(signedTxBytes); err != nil {
		return common.Hash{}, fmt.Errorf("decode signed approve tx: %w", err)
	}

	// Validate: must be a call (not a contract creation).
	if tx.To() == nil {
		return common.Hash{}, fmt.Errorf("approve tx has no 'to' address (contract creation not allowed)")
	}

	// Validate: calldata must be an ERC20 approve(address spender, uint256 amount) call.
	// Function selector: 0x095ea7b3
	data := tx.Data()
	if len(data) < 4+32+32 { // 4-byte selector + address + uint256
		return common.Hash{}, fmt.Errorf("approve tx calldata too short (%d bytes)", len(data))
	}
	approveSelector := [4]byte{0x09, 0x5e, 0xa7, 0xb3}
	if data[0] != approveSelector[0] || data[1] != approveSelector[1] ||
		data[2] != approveSelector[2] || data[3] != approveSelector[3] {
		return common.Hash{}, fmt.Errorf("approve tx has wrong function selector (expected approve)")
	}

	// Validate: spender must be Permit2 canonical address.
	// The spender is the first argument (bytes 4-36, right-padded address at bytes 16-36).
	spender := common.BytesToAddress(data[4:36])
	permit2Addr := common.HexToAddress(Permit2CanonicalAddress)
	if spender != permit2Addr {
		return common.Hash{}, fmt.Errorf("approve tx spender is %s, expected Permit2 %s", spender.Hex(), permit2Addr.Hex())
	}

	// Validate: TX must not send ETH value.
	if tx.Value() != nil && tx.Value().Sign() > 0 {
		return common.Hash{}, fmt.Errorf("approve tx must not send ETH value")
	}

	if err := e.client.SendTransaction(ctx, tx); err != nil {
		return common.Hash{}, fmt.Errorf("broadcast approve tx: %w", err)
	}

	txHash := tx.Hash()
	receipt, err := e.waitForReceipt(ctx, txHash)
	if err != nil {
		return txHash, fmt.Errorf("wait for approve receipt: %w", err)
	}
	if receipt.Status == 0 {
		return txHash, fmt.Errorf("approve tx reverted: %s", txHash.Hex())
	}

	return txHash, nil
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
