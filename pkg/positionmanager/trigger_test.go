package positionmanager

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func triggerTestPair() TokenPair {
	return TokenPair{
		Base:    common.HexToAddress("0x1111111111111111111111111111111111111111"),
		Quote:   common.HexToAddress("0x2222222222222222222222222222222222222222"),
		ChainID: 1,
	}
}

func posID(b byte) [16]byte {
	var id [16]byte
	id[0] = b
	return id
}

func TestTriggerEngine_LongTP(t *testing.T) {
	e := NewTriggerEngine()
	pair := triggerTestPair()

	// Register TP at 2200 for a Long position.
	e.Register(pair, posID(1), 1, LevelTypeTP, Long, big.NewInt(220000000000)) // 2200e8

	// Price below TP — no trigger.
	events := e.OnPrice(pair, big.NewInt(200000000000)) // 2000e8
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}

	// Price reaches TP — trigger fires.
	events = e.OnPrice(pair, big.NewInt(220000000000)) // 2200e8
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].LevelIndex != 1 || events[0].LevelType != LevelTypeTP {
		t.Fatalf("unexpected event: %+v", events[0])
	}

	// Trigger was consumed — no more events.
	events = e.OnPrice(pair, big.NewInt(230000000000))
	if len(events) != 0 {
		t.Fatalf("expected 0 events after consumption, got %d", len(events))
	}
}

func TestTriggerEngine_LongSL(t *testing.T) {
	e := NewTriggerEngine()
	pair := triggerTestPair()

	// Register SL at 1800 for a Long position.
	e.Register(pair, posID(1), 0, LevelTypeSL, Long, big.NewInt(180000000000))

	// Price above SL — no trigger.
	events := e.OnPrice(pair, big.NewInt(200000000000))
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}

	// Price drops to SL — trigger fires.
	events = e.OnPrice(pair, big.NewInt(180000000000))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].LevelType != LevelTypeSL {
		t.Fatalf("expected SL, got %v", events[0].LevelType)
	}
}

func TestTriggerEngine_MultipleTPTrigger(t *testing.T) {
	e := NewTriggerEngine()
	pair := triggerTestPair()

	// Register 3 TPs for a Long position.
	e.Register(pair, posID(1), 1, LevelTypeTP, Long, big.NewInt(220000000000)) // 2200
	e.Register(pair, posID(1), 2, LevelTypeTP, Long, big.NewInt(250000000000)) // 2500
	e.Register(pair, posID(1), 3, LevelTypeTP, Long, big.NewInt(300000000000)) // 3000

	// Price jumps to 2600 — triggers TP1 and TP2, but not TP3.
	events := e.OnPrice(pair, big.NewInt(260000000000))
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Only TP3 remains.
	if e.Count() != 1 {
		t.Fatalf("expected 1 remaining trigger, got %d", e.Count())
	}
}

func TestTriggerEngine_UpdateTriggerPrice(t *testing.T) {
	e := NewTriggerEngine()
	pair := triggerTestPair()

	// SL at 1800.
	e.Register(pair, posID(1), 0, LevelTypeSL, Long, big.NewInt(180000000000))

	// Move SL to 2000.
	e.UpdateTriggerPrice(pair, posID(1), 0, LevelTypeSL, Long, big.NewInt(200000000000))

	// Price at 2100 — should NOT trigger (SL at 2000, Long SL fires when price <= trigger).
	events := e.OnPrice(pair, big.NewInt(210000000000))
	if len(events) != 0 {
		t.Fatalf("expected 0 events at price above SL, got %d", len(events))
	}

	// Price drops to 1900 — should trigger (1900 <= 2000).
	events = e.OnPrice(pair, big.NewInt(190000000000))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
}

func TestTriggerEngine_UnregisterPosition(t *testing.T) {
	e := NewTriggerEngine()
	pair := triggerTestPair()

	e.Register(pair, posID(1), 0, LevelTypeSL, Long, big.NewInt(180000000000))
	e.Register(pair, posID(1), 1, LevelTypeTP, Long, big.NewInt(220000000000))
	e.Register(pair, posID(1), 2, LevelTypeTP, Long, big.NewInt(250000000000))

	if e.Count() != 3 {
		t.Fatalf("expected 3 triggers, got %d", e.Count())
	}

	e.UnregisterPosition(pair, posID(1))

	if e.Count() != 0 {
		t.Fatalf("expected 0 triggers after unregister, got %d", e.Count())
	}
}

func TestTriggerEngine_ShortPosition(t *testing.T) {
	e := NewTriggerEngine()
	pair := triggerTestPair()

	// Short: SL fires when price goes UP, TP fires when price goes DOWN.
	e.Register(pair, posID(1), 0, LevelTypeSL, Short, big.NewInt(220000000000)) // SL at 2200
	e.Register(pair, posID(1), 1, LevelTypeTP, Short, big.NewInt(180000000000)) // TP at 1800

	// Price at 2000 — nothing fires.
	events := e.OnPrice(pair, big.NewInt(200000000000))
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}

	// Price rises to 2200 — SL fires.
	events = e.OnPrice(pair, big.NewInt(220000000000))
	if len(events) != 1 || events[0].LevelType != LevelTypeSL {
		t.Fatalf("expected SL trigger, got %+v", events)
	}
}

func TestFeeResult(t *testing.T) {
	amountIn := big.NewInt(1000000000000000000) // 1 ETH

	cfg := &FeeConfig{
		FeeBps:        100, // 1%
		ReferrerShare: 3000, // 30% of fee
		Referrer:      common.HexToAddress("0xABCD"),
	}

	result := computeFeeResult(amountIn, cfg)

	expectedFee := big.NewInt(10000000000000000) // 0.01 ETH
	if result.TotalFee.Cmp(expectedFee) != 0 {
		t.Fatalf("expected total fee %s, got %s", expectedFee, result.TotalFee)
	}

	expectedRefShare := big.NewInt(3000000000000000) // 30% of 0.01 = 0.003 ETH
	if result.ReferralShare.Cmp(expectedRefShare) != 0 {
		t.Fatalf("expected referral share %s, got %s", expectedRefShare, result.ReferralShare)
	}

	expectedPlatform := big.NewInt(7000000000000000) // 70% of 0.01 = 0.007 ETH
	if result.PlatformShare.Cmp(expectedPlatform) != 0 {
		t.Fatalf("expected platform share %s, got %s", expectedPlatform, result.PlatformShare)
	}
}

func TestFeeResult_NoFee(t *testing.T) {
	result := computeFeeResult(big.NewInt(1e18), nil)
	if result.TotalFee.Sign() != 0 {
		t.Fatalf("expected zero fee, got %s", result.TotalFee)
	}
}

func TestFeeResult_NoReferrer(t *testing.T) {
	cfg := &FeeConfig{FeeBps: 50} // 0.5%, no referrer
	result := computeFeeResult(big.NewInt(1e18), cfg)

	if result.ReferralShare.Sign() != 0 {
		t.Fatalf("expected zero referral share, got %s", result.ReferralShare)
	}
	if result.PlatformShare.Cmp(result.TotalFee) != 0 {
		t.Fatalf("platform share should equal total fee when no referrer")
	}
}
