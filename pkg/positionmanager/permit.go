package positionmanager

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
)

// Permit2 EIP-712 type hashes (precomputed).
var (
	// keccak256("EIP712Domain(string name,uint256 chainId,address verifyingContract)")
	eip712DomainTypeHash = crypto.Keccak256Hash([]byte(
		"EIP712Domain(string name,uint256 chainId,address verifyingContract)",
	))

	// keccak256("Permit2")
	permit2NameHash = crypto.Keccak256Hash([]byte("Permit2"))

	// keccak256("PermitDetails(address token,uint160 amount,uint48 expiration,uint48 nonce)")
	permitDetailsTypeHash = crypto.Keccak256Hash([]byte(
		"PermitDetails(address token,uint160 amount,uint48 expiration,uint48 nonce)",
	))

	// keccak256("PermitSingle(PermitDetails details,address spender,uint256 sigDeadline)PermitDetails(address token,uint160 amount,uint48 expiration,uint48 nonce)")
	permitSingleTypeHash = crypto.Keccak256Hash([]byte(
		"PermitSingle(PermitDetails details,address spender,uint256 sigDeadline)PermitDetails(address token,uint160 amount,uint48 expiration,uint48 nonce)",
	))
)

// PermitSingleData holds the data for a Permit2 AllowanceTransfer PermitSingle.
type PermitSingleData struct {
	Token      common.Address
	Amount     *big.Int // uint160 max
	Expiration uint64   // uint48 unix timestamp
	Nonce      uint64   // uint48
	Spender    common.Address
	SigDeadline *big.Int
}

// permit2DomainSeparator computes the EIP-712 domain separator for Permit2 on a given chain.
func permit2DomainSeparator(chainID uint64, permit2Address common.Address) common.Hash {
	chainIDBig := new(big.Int).SetUint64(chainID)
	return crypto.Keccak256Hash(
		eip712DomainTypeHash.Bytes(),
		permit2NameHash.Bytes(),
		math.U256Bytes(chainIDBig),
		common.LeftPadBytes(permit2Address.Bytes(), 32),
	)
}

// hashPermitDetails computes the struct hash for PermitDetails.
func hashPermitDetails(token common.Address, amount *big.Int, expiration, nonce uint64) common.Hash {
	return crypto.Keccak256Hash(
		permitDetailsTypeHash.Bytes(),
		common.LeftPadBytes(token.Bytes(), 32),
		math.U256Bytes(amount),
		math.U256Bytes(new(big.Int).SetUint64(expiration)),
		math.U256Bytes(new(big.Int).SetUint64(nonce)),
	)
}

// hashPermitSingle computes the struct hash for PermitSingle.
func hashPermitSingle(data PermitSingleData) common.Hash {
	detailsHash := hashPermitDetails(data.Token, data.Amount, data.Expiration, data.Nonce)
	return crypto.Keccak256Hash(
		permitSingleTypeHash.Bytes(),
		detailsHash.Bytes(),
		common.LeftPadBytes(data.Spender.Bytes(), 32),
		math.U256Bytes(data.SigDeadline),
	)
}

// Permit2EIP712Hash computes the final EIP-712 hash for a PermitSingle message.
// This is the hash that the user signs: keccak256("\x19\x01" || domainSeparator || structHash).
func Permit2EIP712Hash(data PermitSingleData, chainID uint64, permit2Address common.Address) common.Hash {
	domainSeparator := permit2DomainSeparator(chainID, permit2Address)
	structHash := hashPermitSingle(data)

	return crypto.Keccak256Hash(
		[]byte{0x19, 0x01},
		domainSeparator.Bytes(),
		structHash.Bytes(),
	)
}

// RecoverPermitSigner recovers the signer address from a Permit2 EIP-712 signature.
// Returns the recovered address or an error if recovery fails.
// secp256k1N is the order of the secp256k1 curve.
var secp256k1N, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)

// secp256k1HalfN is N/2, used for low-s signature malleability check.
var secp256k1HalfN = new(big.Int).Rsh(secp256k1N, 1)

func RecoverPermitSigner(data PermitSingleData, chainID uint64, permit2Address common.Address, signature []byte) (common.Address, error) {
	if len(signature) != 65 {
		return common.Address{}, fmt.Errorf("invalid signature length: %d (expected 65)", len(signature))
	}

	hash := Permit2EIP712Hash(data, chainID, permit2Address)

	// Ethereum signatures use v = 27 or 28; crypto.Ecrecover expects v = 0 or 1.
	sig := make([]byte, 65)
	copy(sig, signature)
	if sig[64] >= 27 {
		sig[64] -= 27
	}

	// Enforce canonical form (low-s) to prevent signature malleability.
	// EIP-2: s must be in the lower half of the curve order.
	s := new(big.Int).SetBytes(sig[32:64])
	if s.Cmp(secp256k1HalfN) > 0 {
		return common.Address{}, fmt.Errorf("signature malleability: s value is not in lower half of curve order")
	}

	pubKey, err := crypto.Ecrecover(hash.Bytes(), sig)
	if err != nil {
		return common.Address{}, fmt.Errorf("ecrecover failed: %w", err)
	}

	pubKeyECDSA, err := crypto.UnmarshalPubkey(pubKey)
	if err != nil {
		return common.Address{}, fmt.Errorf("unmarshal pubkey: %w", err)
	}

	return crypto.PubkeyToAddress(*pubKeyECDSA), nil
}

// Errors for permit validation.
var (
	ErrPermitSignerMismatch = errors.New("permit signer does not match position owner")
	ErrPermitAmountTooLow   = errors.New("permit amount is less than position size")
	ErrPermitExpired        = errors.New("permit has expired")
	ErrPermitLifetimeTooShort = errors.New("permit remaining lifetime is below minimum")
	ErrPermitTokenMismatch  = errors.New("permit token does not match expected tokenIn")
)

// ValidatePermitForPosition checks that a signed Permit2 permit is valid for a position.
// This is called at position creation time (OpenPosition).
func ValidatePermitForPosition(
	owner common.Address,
	size *big.Int,
	expectedTokenIn common.Address,
	permitData PermitSingleData,
	chainID uint64,
	permit2Address common.Address,
	executorAddress common.Address,
	signature []byte,
	minLifetime time.Duration,
) error {
	// 0. Nil safety — prevent panics from uninitialized big.Int fields.
	if size == nil || size.Sign() <= 0 {
		return fmt.Errorf("position size is nil or zero")
	}
	if permitData.Amount == nil {
		return fmt.Errorf("permit amount is nil")
	}
	if permitData.SigDeadline == nil {
		return fmt.Errorf("permit sigDeadline is nil")
	}

	// 1. Recover signer and verify it matches the owner.
	signer, err := RecoverPermitSigner(permitData, chainID, permit2Address, signature)
	if err != nil {
		return fmt.Errorf("recover signer: %w", err)
	}
	if signer != owner {
		return ErrPermitSignerMismatch
	}

	// 2. Verify the permit amount covers the position size.
	if permitData.Amount.Cmp(size) < 0 {
		return ErrPermitAmountTooLow
	}

	// 3. Verify the permit is not expired.
	now := time.Now().Unix()
	if int64(permitData.Expiration) <= now {
		return ErrPermitExpired
	}

	// 4. Verify minimum remaining lifetime.
	remaining := time.Duration(int64(permitData.Expiration)-now) * time.Second
	if remaining < minLifetime {
		return ErrPermitLifetimeTooShort
	}

	// 5. Verify the permit token matches expected tokenIn.
	if permitData.Token != expectedTokenIn {
		return ErrPermitTokenMismatch
	}

	// 6. Verify spender is our executor contract.
	if permitData.Spender != executorAddress {
		return fmt.Errorf("permit spender %s does not match executor %s", permitData.Spender.Hex(), executorAddress.Hex())
	}

	// 7. Verify sigDeadline is not in the past.
	if permitData.SigDeadline.Int64() <= now {
		return fmt.Errorf("permit sigDeadline %d has passed (now=%d)", permitData.SigDeadline.Int64(), now)
	}

	return nil
}

// BuildPermitSingleTypedData builds the EIP-712 typed data structure that the frontend
// needs to present to the user for signing. Returns a map suitable for JSON serialization.
func BuildPermitSingleTypedData(
	token common.Address,
	amount *big.Int,
	expiration uint64,
	nonce uint64,
	spender common.Address,
	sigDeadline *big.Int,
	chainID uint64,
	permit2Address common.Address,
) map[string]interface{} {
	return map[string]interface{}{
		"types": map[string]interface{}{
			"EIP712Domain": []map[string]string{
				{"name": "name", "type": "string"},
				{"name": "chainId", "type": "uint256"},
				{"name": "verifyingContract", "type": "address"},
			},
			"PermitSingle": []map[string]string{
				{"name": "details", "type": "PermitDetails"},
				{"name": "spender", "type": "address"},
				{"name": "sigDeadline", "type": "uint256"},
			},
			"PermitDetails": []map[string]string{
				{"name": "token", "type": "address"},
				{"name": "amount", "type": "uint160"},
				{"name": "expiration", "type": "uint48"},
				{"name": "nonce", "type": "uint48"},
			},
		},
		"primaryType": "PermitSingle",
		"domain": map[string]interface{}{
			"name":              "Permit2",
			"chainId":           chainID,
			"verifyingContract": permit2Address.Hex(),
		},
		"message": map[string]interface{}{
			"details": map[string]interface{}{
				"token":      token.Hex(),
				"amount":     amount.String(),
				"expiration": expiration,
				"nonce":      nonce,
			},
			"spender":     spender.Hex(),
			"sigDeadline": sigDeadline.String(),
		},
	}
}
