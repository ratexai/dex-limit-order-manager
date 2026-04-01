package positionmanager

import (
	"crypto/ecdsa"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// testPermit2Addr is the canonical Permit2 address used in tests.
var testPermit2Addr = common.HexToAddress(Permit2CanonicalAddress)

// generateTestKey creates a deterministic ECDSA key for testing.
func generateTestKey(t *testing.T) (*ecdsa.PrivateKey, common.Address) {
	t.Helper()
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	addr := crypto.PubkeyToAddress(key.PublicKey)
	return key, addr
}

// signPermitSingle signs a PermitSingleData with the given key, returning the 65-byte signature.
func signPermitSingle(t *testing.T, key *ecdsa.PrivateKey, data PermitSingleData, chainID uint64) []byte {
	t.Helper()
	hash := Permit2EIP712Hash(data, chainID, testPermit2Addr)
	sig, err := crypto.Sign(hash.Bytes(), key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// crypto.Sign returns v=0/1, Ethereum convention is v=27/28.
	sig[64] += 27
	return sig
}

func TestPermit2DomainSeparator_DeterministicPerChain(t *testing.T) {
	sep1 := permit2DomainSeparator(1, testPermit2Addr)
	sep56 := permit2DomainSeparator(56, testPermit2Addr)
	sep8453 := permit2DomainSeparator(8453, testPermit2Addr)

	// Different chains produce different domain separators.
	if sep1 == sep56 {
		t.Error("ETH and BSC domain separators should differ")
	}
	if sep1 == sep8453 {
		t.Error("ETH and Base domain separators should differ")
	}

	// Same chain produces same separator (deterministic).
	sep1Again := permit2DomainSeparator(1, testPermit2Addr)
	if sep1 != sep1Again {
		t.Error("same chain should produce same domain separator")
	}
}

func TestPermit2EIP712Hash_Deterministic(t *testing.T) {
	data := PermitSingleData{
		Token:       common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Amount:      big.NewInt(1e18),
		Expiration:  1714521600,
		Nonce:       0,
		Spender:     common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		SigDeadline: big.NewInt(1714521600),
	}

	hash1 := Permit2EIP712Hash(data, 1, testPermit2Addr)
	hash2 := Permit2EIP712Hash(data, 1, testPermit2Addr)
	if hash1 != hash2 {
		t.Error("same input should produce same hash")
	}

	// Different amount → different hash.
	data2 := data
	data2.Amount = big.NewInt(2e18)
	hash3 := Permit2EIP712Hash(data2, 1, testPermit2Addr)
	if hash1 == hash3 {
		t.Error("different amount should produce different hash")
	}

	// Different chain → different hash.
	hash4 := Permit2EIP712Hash(data, 56, testPermit2Addr)
	if hash1 == hash4 {
		t.Error("different chain should produce different hash")
	}
}

func TestRecoverPermitSigner_RoundTrip(t *testing.T) {
	key, addr := generateTestKey(t)

	data := PermitSingleData{
		Token:       common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Amount:      big.NewInt(1e18),
		Expiration:  1714521600,
		Nonce:       0,
		Spender:     common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		SigDeadline: big.NewInt(1714521600),
	}

	sig := signPermitSingle(t, key, data, 1)
	recovered, err := RecoverPermitSigner(data, 1, testPermit2Addr, sig)
	if err != nil {
		t.Fatalf("recover signer: %v", err)
	}
	if recovered != addr {
		t.Errorf("recovered %s, want %s", recovered.Hex(), addr.Hex())
	}
}

func TestRecoverPermitSigner_WrongChain(t *testing.T) {
	key, addr := generateTestKey(t)

	data := PermitSingleData{
		Token:       common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Amount:      big.NewInt(1e18),
		Expiration:  1714521600,
		Nonce:       0,
		Spender:     common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		SigDeadline: big.NewInt(1714521600),
	}

	// Sign for chain 1, recover for chain 56. Should NOT match.
	sig := signPermitSingle(t, key, data, 1)
	recovered, err := RecoverPermitSigner(data, 56, testPermit2Addr, sig)
	if err != nil {
		t.Fatalf("recover signer: %v", err)
	}
	if recovered == addr {
		t.Error("signature verified on wrong chain — domain separator is not effective")
	}
}

func TestRecoverPermitSigner_InvalidSignatureLength(t *testing.T) {
	data := PermitSingleData{
		Token:       common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Amount:      big.NewInt(1e18),
		Expiration:  1714521600,
		Nonce:       0,
		Spender:     common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		SigDeadline: big.NewInt(1714521600),
	}

	_, err := RecoverPermitSigner(data, 1, testPermit2Addr, []byte{0x01, 0x02})
	if err == nil {
		t.Error("expected error for short signature")
	}
}

func TestRecoverPermitSigner_TamperedData(t *testing.T) {
	key, addr := generateTestKey(t)

	data := PermitSingleData{
		Token:       common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Amount:      big.NewInt(1e18),
		Expiration:  1714521600,
		Nonce:       0,
		Spender:     common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"),
		SigDeadline: big.NewInt(1714521600),
	}

	sig := signPermitSingle(t, key, data, 1)

	// Tamper with the amount — recovery should produce a different address.
	tampered := data
	tampered.Amount = big.NewInt(999)
	recovered, err := RecoverPermitSigner(tampered, 1, testPermit2Addr, sig)
	if err != nil {
		t.Fatalf("recover signer: %v", err)
	}
	if recovered == addr {
		t.Error("tampered data should not recover the original signer")
	}
}

func TestValidatePermitForPosition_Valid(t *testing.T) {
	key, addr := generateTestKey(t)
	executor := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	token := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	amount := big.NewInt(1e18)
	expiration := uint64(time.Now().Add(48 * time.Hour).Unix())

	data := PermitSingleData{
		Token:       token,
		Amount:      amount,
		Expiration:  expiration,
		Nonce:       0,
		Spender:     executor,
		SigDeadline: new(big.Int).SetUint64(expiration),
	}

	sig := signPermitSingle(t, key, data, 1)

	err := ValidatePermitForPosition(addr, amount, token, data, 1, testPermit2Addr, executor, sig, 1*time.Hour)
	if err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidatePermitForPosition_SignerMismatch(t *testing.T) {
	key, _ := generateTestKey(t)
	_, otherAddr := generateTestKey(t)
	executor := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	token := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	amount := big.NewInt(1e18)
	expiration := uint64(time.Now().Add(48 * time.Hour).Unix())

	data := PermitSingleData{
		Token:       token,
		Amount:      amount,
		Expiration:  expiration,
		Nonce:       0,
		Spender:     executor,
		SigDeadline: new(big.Int).SetUint64(expiration),
	}

	sig := signPermitSingle(t, key, data, 1)

	// Validate with a different owner address — should fail.
	err := ValidatePermitForPosition(otherAddr, amount, token, data, 1, testPermit2Addr, executor, sig, 1*time.Hour)
	if err != ErrPermitSignerMismatch {
		t.Errorf("expected ErrPermitSignerMismatch, got: %v", err)
	}
}

func TestValidatePermitForPosition_AmountTooLow(t *testing.T) {
	key, addr := generateTestKey(t)
	executor := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	token := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	expiration := uint64(time.Now().Add(48 * time.Hour).Unix())

	permitAmount := big.NewInt(5e17) // 0.5 ETH
	positionSize := big.NewInt(1e18) // 1 ETH — larger than permit

	data := PermitSingleData{
		Token:       token,
		Amount:      permitAmount,
		Expiration:  expiration,
		Nonce:       0,
		Spender:     executor,
		SigDeadline: new(big.Int).SetUint64(expiration),
	}

	sig := signPermitSingle(t, key, data, 1)

	err := ValidatePermitForPosition(addr, positionSize, token, data, 1, testPermit2Addr, executor, sig, 1*time.Hour)
	if err != ErrPermitAmountTooLow {
		t.Errorf("expected ErrPermitAmountTooLow, got: %v", err)
	}
}

func TestValidatePermitForPosition_Expired(t *testing.T) {
	key, addr := generateTestKey(t)
	executor := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	token := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	amount := big.NewInt(1e18)
	expiration := uint64(time.Now().Add(-1 * time.Hour).Unix()) // Already expired

	data := PermitSingleData{
		Token:       token,
		Amount:      amount,
		Expiration:  expiration,
		Nonce:       0,
		Spender:     executor,
		SigDeadline: new(big.Int).SetUint64(expiration),
	}

	sig := signPermitSingle(t, key, data, 1)

	err := ValidatePermitForPosition(addr, amount, token, data, 1, testPermit2Addr, executor, sig, 1*time.Hour)
	if err != ErrPermitExpired {
		t.Errorf("expected ErrPermitExpired, got: %v", err)
	}
}

func TestValidatePermitForPosition_LifetimeTooShort(t *testing.T) {
	key, addr := generateTestKey(t)
	executor := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	token := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	amount := big.NewInt(1e18)
	expiration := uint64(time.Now().Add(30 * time.Minute).Unix()) // Only 30 min left

	data := PermitSingleData{
		Token:       token,
		Amount:      amount,
		Expiration:  expiration,
		Nonce:       0,
		Spender:     executor,
		SigDeadline: new(big.Int).SetUint64(expiration),
	}

	sig := signPermitSingle(t, key, data, 1)

	err := ValidatePermitForPosition(addr, amount, token, data, 1, testPermit2Addr, executor, sig, 1*time.Hour)
	if err != ErrPermitLifetimeTooShort {
		t.Errorf("expected ErrPermitLifetimeTooShort, got: %v", err)
	}
}

func TestValidatePermitForPosition_TokenMismatch(t *testing.T) {
	key, addr := generateTestKey(t)
	executor := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	permitToken := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	expectedToken := common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48") // Different token
	amount := big.NewInt(1e18)
	expiration := uint64(time.Now().Add(48 * time.Hour).Unix())

	data := PermitSingleData{
		Token:       permitToken,
		Amount:      amount,
		Expiration:  expiration,
		Nonce:       0,
		Spender:     executor,
		SigDeadline: new(big.Int).SetUint64(expiration),
	}

	sig := signPermitSingle(t, key, data, 1)

	err := ValidatePermitForPosition(addr, amount, expectedToken, data, 1, testPermit2Addr, executor, sig, 1*time.Hour)
	if err != ErrPermitTokenMismatch {
		t.Errorf("expected ErrPermitTokenMismatch, got: %v", err)
	}
}

func TestBuildPermitSingleTypedData_Structure(t *testing.T) {
	token := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")
	spender := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	amount := big.NewInt(1e18)
	expiration := uint64(1714521600)
	nonce := uint64(0)
	sigDeadline := big.NewInt(1714521600)

	td := BuildPermitSingleTypedData(token, amount, expiration, nonce, spender, sigDeadline, 1, testPermit2Addr)

	// Verify top-level keys.
	if td["primaryType"] != "PermitSingle" {
		t.Errorf("primaryType = %v, want PermitSingle", td["primaryType"])
	}

	domain, ok := td["domain"].(map[string]interface{})
	if !ok {
		t.Fatal("domain is not a map")
	}
	if domain["name"] != "Permit2" {
		t.Errorf("domain.name = %v, want Permit2", domain["name"])
	}
	if domain["chainId"] != uint64(1) {
		t.Errorf("domain.chainId = %v, want 1", domain["chainId"])
	}

	msg, ok := td["message"].(map[string]interface{})
	if !ok {
		t.Fatal("message is not a map")
	}
	if msg["spender"] != spender.Hex() {
		t.Errorf("message.spender = %v, want %s", msg["spender"], spender.Hex())
	}

	details, ok := msg["details"].(map[string]interface{})
	if !ok {
		t.Fatal("message.details is not a map")
	}
	if details["token"] != token.Hex() {
		t.Errorf("details.token = %v, want %s", details["token"], token.Hex())
	}
}

func TestHashPermitDetails_DifferentInputs(t *testing.T) {
	token := common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2")

	h1 := hashPermitDetails(token, big.NewInt(1e18), 1714521600, 0)
	h2 := hashPermitDetails(token, big.NewInt(2e18), 1714521600, 0)
	h3 := hashPermitDetails(token, big.NewInt(1e18), 1714521600, 1)

	if h1 == h2 {
		t.Error("different amounts should produce different hashes")
	}
	if h1 == h3 {
		t.Error("different nonces should produce different hashes")
	}
}
