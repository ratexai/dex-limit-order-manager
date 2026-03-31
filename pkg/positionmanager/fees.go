package positionmanager

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// FeeProvider resolves the fee configuration for a user.
// The host application implements this interface — it knows each user's
// tariff tier, referral relationships, and any dynamic fee logic.
type FeeProvider interface {
	// GetFee returns the fee config for a specific user at the time of a swap.
	// The host determines the tier based on user tariff, volume, referrals, etc.
	GetFee(ctx context.Context, user common.Address) (*FeeConfig, error)
}

// FeeConfig describes the fee structure for a single swap.
type FeeConfig struct {
	// FeeBps is the platform fee in basis points (100 = 1%).
	// Zero means no fee. Max is enforced by the on-chain contract (MAX_FEE_BPS = 500).
	FeeBps uint16

	// ReferrerShare is the portion of the platform fee that goes to the referrer,
	// expressed in basis points (3000 = 30% of the fee).
	// Zero means no referral payout.
	ReferrerShare uint16

	// Referrer is the address of the referrer. Zero address means no referrer.
	Referrer common.Address
}

// FeeResult is computed after a swap and delivered to the host via OnExecution callback.
// The host uses this for P&L accounting and referral payout scheduling.
type FeeResult struct {
	// TotalFee is the total fee deducted from amountIn (in tokenIn units).
	TotalFee *big.Int

	// PlatformShare is the platform's portion of the fee (totalFee - referralShare).
	PlatformShare *big.Int

	// ReferralShare is the referrer's portion of the fee.
	ReferralShare *big.Int

	// Referrer is the referrer's address (zero if none).
	Referrer common.Address

	// FeeBps that was applied.
	FeeBps uint16
}

// computeFeeResult calculates the fee breakdown after a swap.
func computeFeeResult(amountIn *big.Int, cfg *FeeConfig) FeeResult {
	if cfg == nil || cfg.FeeBps == 0 {
		return FeeResult{
			TotalFee:      new(big.Int),
			PlatformShare: new(big.Int),
			ReferralShare: new(big.Int),
		}
	}

	totalFee := new(big.Int).Mul(amountIn, big.NewInt(int64(cfg.FeeBps)))
	totalFee.Div(totalFee, big.NewInt(10000))

	var referralShare *big.Int
	if cfg.ReferrerShare > 0 && cfg.Referrer != (common.Address{}) {
		referralShare = new(big.Int).Mul(totalFee, big.NewInt(int64(cfg.ReferrerShare)))
		referralShare.Div(referralShare, big.NewInt(10000))
	} else {
		referralShare = new(big.Int)
	}

	platformShare := new(big.Int).Sub(totalFee, referralShare)

	return FeeResult{
		TotalFee:      totalFee,
		PlatformShare: platformShare,
		ReferralShare: referralShare,
		Referrer:      cfg.Referrer,
		FeeBps:        cfg.FeeBps,
	}
}
