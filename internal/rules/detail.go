package rules

import (
	"strconv"

	"github.com/devastion/anti-fraud-go/internal/money"
)

// AppendDetail appends the human-readable detail for r to dst.
func (r Reason) AppendDetail(dst []byte) []byte {
	switch r.Rule {
	case R1CardTesting:
		if r.PlayerDeclines > 0 {
			dst = appendDeclines(dst, r.PlayerDeclines, r.PlayerMethods, "player")
		}
		if r.DeviceDeclines > 0 {
			if r.PlayerDeclines > 0 {
				dst = append(dst, "; "...)
			}
			dst = appendDeclines(dst, r.DeviceDeclines, r.DeviceMethods, "device")
		}
	case R2CashoutWithoutPlay:
		dst = append(dst, "turnover "...)
		dst = money.AppendE4(dst, r.TurnoverEUR)
		dst = append(dst, " EUR below last deposit "...)
		dst = money.AppendE4(dst, r.DepositEUR)
		dst = append(dst, " EUR"...)
	case R3AccountTakeover:
		dst = append(dst, "password reset, payout method added and login from new device in last 60m"...)
	case R4LinkedAccounts:
		dst = append(dst, "device used by "...)
		dst = strconv.AppendInt(dst, int64(r.LinkedPlayers), 10)
		dst = append(dst, " players in last 30d"...)
	case R5NewPayoutMethod:
		dst = append(dst, "payment method not used in any approved deposit"...)
	}
	return dst
}

func appendDeclines(dst []byte, declines, methods int32, subject string) []byte {
	dst = strconv.AppendInt(dst, int64(declines), 10)
	dst = append(dst, " declined deposits with "...)
	dst = strconv.AppendInt(dst, int64(methods), 10)
	dst = append(dst, " payment methods on "...)
	dst = append(dst, subject...)
	return append(dst, " in last 10m"...)
}
