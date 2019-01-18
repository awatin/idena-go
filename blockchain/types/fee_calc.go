package types

import (
	"github.com/shopspring/decimal"
	"idena-go/common"
	"idena-go/common/math"
	"math/big"
)

const (
	InvitationCoef = 11000
)

func CalculateFee(networkSize int, tx *Transaction) *big.Int {
	if tx.Type == KillTx || tx.Type == NewEpochTx {
		return big.NewInt(0)
	}
	if networkSize == 0 {
		return big.NewInt(0)
	}
	feePerByte := new(big.Int).Div(common.DnaBase, big.NewInt(int64(networkSize)))

	return new(big.Int).Mul(feePerByte, big.NewInt(int64(tx.Size())))
}

func CalculateCost(networkSize int, tx *Transaction) *big.Int {
	result := big.NewInt(0)

	result.Add(result, tx.AmountOrZero())

	fee := CalculateFee(networkSize, tx)
	result.Add(result, fee)

	if tx.Type == InviteTx && networkSize > 0 {

		invitationCost := decimal.NewFromFloat(InvitationCoef / float64(networkSize))
		coinsPerInvitation := invitationCost.Mul(decimal.NewFromBigInt(common.DnaBase, 0))

		result.Add(result, math.ToInt(&coinsPerInvitation))
	}

	return result
}
