package positionmanager

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestComputeMinAmountOut_ETH_USDC(t *testing.T) {
	// 1 WETH (18 decimals) at $2000 (8 decimals) with 50 bps slippage.
	amountIn := big.NewInt(1e18)                 // 1 WETH
	triggerPrice := big.NewInt(200000000000)      // $2000.00 with 8 decimals
	slippageBps := uint16(50)                     // 0.5%
	decimalsIn := uint8(18)
	decimalsOut := uint8(6)

	result := computeMinAmountOut(triggerPrice, amountIn, slippageBps, decimalsIn, decimalsOut)

	// Expected: 1 * 2000 * (1 - 0.005) = 1990 USDC = 1990 * 1e6
	expected := big.NewInt(1990_000000)
	if result.Cmp(expected) != 0 {
		t.Errorf("ETH→USDC: expected %s, got %s", expected, result)
	}
}

func TestComputeMinAmountOut_USDC_ETH(t *testing.T) {
	// 2000 USDC (6 decimals) at price $2000 (8 decimals).
	// This represents buying WETH with USDC. The trigger price is quote/base.
	// expectedOut = 2000e6 * 2000e8 * 1e18 / (1e6 * 1e8)
	// That would give 2000 * 2000 * 1e18 = 4e6 * 1e18 — incorrect model.
	// The computeMinAmountOut function expects a direct price ratio.
	// For selling USDC to get ETH, the price should be 1/2000 = 0.0005
	// which in 8 decimals is 50000.
	amountIn := new(big.Int).Mul(big.NewInt(2000), big.NewInt(1e6)) // 2000 USDC
	triggerPrice := big.NewInt(50000)                                 // 0.0005 ETH per USDC in 8 decimals
	slippageBps := uint16(50)
	decimalsIn := uint8(6)
	decimalsOut := uint8(18)

	result := computeMinAmountOut(triggerPrice, amountIn, slippageBps, decimalsIn, decimalsOut)

	// expectedOut = 2000e6 * 50000 * 1e18 / (1e6 * 1e8)
	//             = 2000 * 50000 * 1e18 / 1e8
	//             = 100_000_000 * 1e18 / 1e8
	//             = 1e18 = 1 WETH
	// With 0.5% slippage: 0.995e18
	expected := big.NewInt(995_000_000_000_000_000) // 0.995 WETH
	if result.Cmp(expected) != 0 {
		t.Errorf("USDC→ETH: expected %s, got %s", expected, result)
	}
}

func TestComputeMinAmountOut_SameDecimals(t *testing.T) {
	// Both tokens 18 decimals, price 1:1 (1e8 in 8 decimal).
	amountIn := big.NewInt(1e18)
	triggerPrice := big.NewInt(1e8) // 1.0
	slippageBps := uint16(100)     // 1%

	result := computeMinAmountOut(triggerPrice, amountIn, slippageBps, 18, 18)

	// Expected: 1e18 * 0.99 = 0.99e18
	expected := big.NewInt(990_000_000_000_000_000)
	if result.Cmp(expected) != 0 {
		t.Errorf("same decimals: expected %s, got %s", expected, result)
	}
}

func TestComputeMinAmountOut_ZeroSlippage(t *testing.T) {
	amountIn := big.NewInt(1e18)
	triggerPrice := big.NewInt(200000000000)
	result := computeMinAmountOut(triggerPrice, amountIn, 0, 18, 6)

	expected := big.NewInt(2000_000000) // Exact: 2000 USDC
	if result.Cmp(expected) != 0 {
		t.Errorf("zero slippage: expected %s, got %s", expected, result)
	}
}

func TestComputeMinAmountOut_LargeSlippage(t *testing.T) {
	amountIn := big.NewInt(1e18)
	triggerPrice := big.NewInt(200000000000)
	result := computeMinAmountOut(triggerPrice, amountIn, 200, 18, 6) // 2% slippage

	expected := big.NewInt(1960_000000) // 2000 * 0.98 = 1960 USDC
	if result.Cmp(expected) != 0 {
		t.Errorf("2%% slippage: expected %s, got %s", expected, result)
	}
}

// --- Fee computation tests ---

func TestComputeFeeResult_NilConfig(t *testing.T) {
	result := computeFeeResult(big.NewInt(1e18), nil)
	if result.TotalFee.Sign() != 0 {
		t.Error("nil config should produce zero fee")
	}
}

func TestComputeFeeResult_ZeroFee(t *testing.T) {
	result := computeFeeResult(big.NewInt(1e18), &FeeConfig{FeeBps: 0})
	if result.TotalFee.Sign() != 0 {
		t.Error("zero FeeBps should produce zero fee")
	}
}

func TestComputeFeeResult_NoReferrer(t *testing.T) {
	amountIn := big.NewInt(10000)
	result := computeFeeResult(amountIn, &FeeConfig{FeeBps: 100}) // 1%

	expectedFee := big.NewInt(100) // 1% of 10000
	if result.TotalFee.Cmp(expectedFee) != 0 {
		t.Errorf("expected fee %s, got %s", expectedFee, result.TotalFee)
	}
	if result.PlatformShare.Cmp(expectedFee) != 0 {
		t.Errorf("platform share should equal total fee without referrer")
	}
	if result.ReferralShare.Sign() != 0 {
		t.Error("referral share should be zero without referrer")
	}
}

func TestComputeFeeResult_WithReferrer(t *testing.T) {
	amountIn := big.NewInt(10000)
	referrer := common.HexToAddress("0xABCDEF1234567890ABCDEF1234567890ABCDEF12")
	result := computeFeeResult(amountIn, &FeeConfig{
		FeeBps:        100,  // 1%
		ReferrerShare: 3000, // 30% of fee
		Referrer:      referrer,
	})

	expectedFee := big.NewInt(100)
	if result.TotalFee.Cmp(expectedFee) != 0 {
		t.Errorf("expected total fee %s, got %s", expectedFee, result.TotalFee)
	}

	expectedReferral := big.NewInt(30) // 30% of 100
	if result.ReferralShare.Cmp(expectedReferral) != 0 {
		t.Errorf("expected referral share %s, got %s", expectedReferral, result.ReferralShare)
	}

	expectedPlatform := big.NewInt(70) // 100 - 30
	if result.PlatformShare.Cmp(expectedPlatform) != 0 {
		t.Errorf("expected platform share %s, got %s", expectedPlatform, result.PlatformShare)
	}

	if result.Referrer != referrer {
		t.Error("referrer address mismatch")
	}
}

func TestComputeFeeResult_ReferrerShareWithZeroAddress(t *testing.T) {
	amountIn := big.NewInt(10000)
	result := computeFeeResult(amountIn, &FeeConfig{
		FeeBps:        100,
		ReferrerShare: 3000,
		Referrer:      common.Address{}, // Zero address.
	})

	// Even with ReferrerShare > 0, zero address means no referral payout.
	if result.ReferralShare.Sign() != 0 {
		t.Error("zero referrer address should mean no referral share")
	}
}

// --- mulBps tests ---

func TestMulBps(t *testing.T) {
	tests := []struct {
		amount   int64
		bps      uint16
		expected int64
	}{
		{10000, 100, 100},    // 1%
		{10000, 5000, 5000},  // 50%
		{10000, 10000, 10000}, // 100%
		{10000, 0, 0},        // 0%
		{10000, 1, 1},        // 0.01%
		{100, 5000, 50},      // 50% of 100
		{0, 5000, 0},         // 0 amount
	}
	for _, tt := range tests {
		result := mulBps(big.NewInt(tt.amount), tt.bps)
		expected := big.NewInt(tt.expected)
		if result.Cmp(expected) != 0 {
			t.Errorf("mulBps(%d, %d): expected %s, got %s", tt.amount, tt.bps, expected, result)
		}
	}
}

// --- Gas buffer tests ---

func TestApplyGasBuffer(t *testing.T) {
	exec := &executor{cfg: EthereumDefaults()}

	// SL: 1.5x
	sl := exec.applyGasBuffer(200000, PriorityCritical)
	if sl != 300000 {
		t.Errorf("SL gas buffer: expected 300000, got %d", sl)
	}

	// TP: 1.2x
	tp := exec.applyGasBuffer(200000, PriorityNormal)
	if tp != 240000 {
		t.Errorf("TP gas buffer: expected 240000, got %d", tp)
	}
}

// --- packSwapCalldata tests ---

func testSwapParams() executeSwapParams {
	return executeSwapParams{
		User:         common.HexToAddress("0xUser"),
		TokenIn:      common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		TokenOut:     common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		PoolFee:      3000,
		AmountIn:     big.NewInt(1e18),
		MinAmountOut: big.NewInt(1990_000000),
		FeeBps:       100,
		Priority:     PriorityNormal,
	}
}

func TestPackSwapCalldata_LegacyMode(t *testing.T) {
	exec := &executor{cfg: EthereumDefaults()}
	params := testSwapParams()
	params.Mode = ExecModeLegacy

	data, err := exec.packSwapCalldata(params)
	if err != nil {
		t.Fatalf("pack calldata: %v", err)
	}

	// Verify we got non-empty calldata.
	if len(data) < 4 {
		t.Fatal("calldata too short")
	}

	// First 4 bytes should be the executeSwap function selector.
	method, err := parsedSwapExecutorABI.MethodById(data[:4])
	if err != nil {
		t.Fatalf("method lookup: %v", err)
	}
	if method.Name != "executeSwap" {
		t.Errorf("method = %s, want executeSwap", method.Name)
	}
}

func TestPackSwapCalldata_Permit2AllowanceMode(t *testing.T) {
	exec := &executor{cfg: EthereumDefaults()}
	params := testSwapParams()
	params.Mode = ExecModePermit2Allowance

	data, err := exec.packSwapCalldata(params)
	if err != nil {
		t.Fatalf("pack calldata: %v", err)
	}

	if len(data) < 4 {
		t.Fatal("calldata too short")
	}

	method, err := parsedSwapExecutorV2ABI.MethodById(data[:4])
	if err != nil {
		t.Fatalf("method lookup: %v", err)
	}
	if method.Name != "executeSwapViaPermit2" {
		t.Errorf("method = %s, want executeSwapViaPermit2", method.Name)
	}
}

func TestPackSwapCalldata_Permit2SignatureMode(t *testing.T) {
	exec := &executor{cfg: EthereumDefaults()}
	params := testSwapParams()
	params.Mode = ExecModePermit2Signature
	params.PermitNonce = big.NewInt(0)
	params.PermitDeadline = big.NewInt(1714521600)
	params.PermitSignature = make([]byte, 65)

	data, err := exec.packSwapCalldata(params)
	if err != nil {
		t.Fatalf("pack calldata: %v", err)
	}

	if len(data) < 4 {
		t.Fatal("calldata too short")
	}

	method, err := parsedSwapExecutorV2ABI.MethodById(data[:4])
	if err != nil {
		t.Fatalf("method lookup: %v", err)
	}
	if method.Name != "executeSwapWithSignature" {
		t.Errorf("method = %s, want executeSwapWithSignature", method.Name)
	}
}

func TestBroadcastSignedApproveTx_InvalidRLP(t *testing.T) {
	exec := &executor{
		client: newMockChainClient(),
		cfg:    EthereumDefaults(),
	}

	_, err := exec.broadcastSignedApproveTx(nil, []byte{0xDE, 0xAD})
	if err == nil {
		t.Fatal("expected error for invalid RLP")
	}
}
