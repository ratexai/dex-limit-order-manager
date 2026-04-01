package positionmanager

import "math/big"

// mulBps multiplies amount by bps basis points: result = amount * bps / 10000.
func mulBps(amount *big.Int, bps uint16) *big.Int {
	result := new(big.Int).Mul(amount, big.NewInt(int64(bps)))
	return result.Div(result, big.NewInt(10000))
}
