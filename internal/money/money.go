// Package money handles monetary amounts as integer counts of 10^-4 units.
// Floating point is never used.
package money

import (
	"errors"
	"strconv"
)

// Currency is a supported ISO-4217 currency.
type Currency uint8

const (
	NoCurrency Currency = iota
	EUR
	USD
	GBP
)

// String returns the ISO code, or "" for NoCurrency.
func (c Currency) String() string {
	switch c {
	case EUR:
		return "EUR"
	case USD:
		return "USD"
	case GBP:
		return "GBP"
	}
	return ""
}

// ParseCurrency accepts exactly "EUR", "USD" or "GBP".
func ParseCurrency(s string) (Currency, bool) {
	switch s {
	case "EUR":
		return EUR, true
	case "USD":
		return USD, true
	case "GBP":
		return GBP, true
	}
	return NoCurrency, false
}

// ErrInvalidAmount is returned for amounts that do not match
// ^\d{1,12}(\.\d{1,2})?$ or are not greater than zero.
var ErrInvalidAmount = errors.New("money: invalid amount")

const (
	maxIntDigits  = 12
	maxFracDigits = 2
)

// ParseE4 parses a decimal string into 10^-4 units of its own currency.
// The string must match ^\d{1,12}(\.\d{1,2})?$ and be greater than zero.
func ParseE4(s string) (int64, error) {
	n := len(s)
	i := 0
	var whole int64
	for i < n && i <= maxIntDigits && isDigit(s[i]) {
		whole = whole*10 + int64(s[i]-'0')
		i++
	}
	if i == 0 || i > maxIntDigits {
		return 0, ErrInvalidAmount
	}
	var frac int64
	if i < n {
		if s[i] != '.' {
			return 0, ErrInvalidAmount
		}
		i++
		start := i
		for i < n && i-start < maxFracDigits && isDigit(s[i]) {
			frac = frac*10 + int64(s[i]-'0')
			i++
		}
		digits := i - start
		if digits == 0 || i != n {
			return 0, ErrInvalidAmount
		}
		if digits == 1 {
			frac *= 10
		}
	}
	v := whole*10000 + frac*100
	if v <= 0 {
		return 0, ErrInvalidAmount
	}
	return v, nil
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// ToEUR converts an amount in 10^-4 units of c to 10^-4 EUR units:
// USD × 92 / 100, GBP × 117 / 100. The result is exact for any value
// produced by ParseE4, because such values are multiples of 100.
// It panics on NoCurrency, which validation rules out.
func ToEUR(e4 int64, c Currency) int64 {
	switch c {
	case EUR:
		return e4
	case USD:
		return e4 * 92 / 100
	case GBP:
		return e4 * 117 / 100
	}
	panic("money: ToEUR with unsupported currency")
}

// AppendE4 appends v (10^-4 units) as a decimal with 2 to 4 fractional
// digits, trimming trailing zeros beyond the second: 12.5 -> "12.50",
// 9.2092 -> "9.2092".
func AppendE4(dst []byte, v int64) []byte {
	u := uint64(v)
	if v < 0 {
		dst = append(dst, '-')
		u = -u
	}
	dst = strconv.AppendUint(dst, u/10000, 10)
	f := u % 10000
	frac := [4]byte{
		byte('0' + f/1000),
		byte('0' + f/100%10),
		byte('0' + f/10%10),
		byte('0' + f%10),
	}
	end := 4
	for end > 2 && frac[end-1] == '0' {
		end--
	}
	dst = append(dst, '.')
	return append(dst, frac[:end]...)
}
