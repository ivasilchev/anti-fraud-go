package money

import (
	"errors"
	"math"
	"testing"
)

func TestParseE4Valid(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"1", 10000},
		{"0.01", 100},
		{"0.1", 1000},
		{"12.5", 125000},
		{"12.34", 123400},
		{"00012", 120000},
		{"999999999999", 9999999999990000},
		{"999999999999.99", 9999999999999900},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseE4(tt.in)
			if err != nil {
				t.Fatalf("ParseE4(%q) error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("ParseE4(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseE4Invalid(t *testing.T) {
	for _, in := range []string{
		"", "0", "0.0", "0.00", "000.00",
		"1.234", "1.", ".5", "1..2", "1.2.3",
		"-1", "+1", "1e3", " 1", "1 ", "1,00", "１",
		"1234567890123", "1234567890123.00",
		"NaN", "Infinity", "0x10",
	} {
		t.Run(in, func(t *testing.T) {
			if v, err := ParseE4(in); !errors.Is(err, ErrInvalidAmount) {
				t.Fatalf("ParseE4(%q) = %d, %v; want ErrInvalidAmount", in, v, err)
			}
		})
	}
}

func TestToEURExact(t *testing.T) {
	tests := []struct {
		amount string
		cur    Currency
		want   int64
	}{
		{"10.00", EUR, 100000},
		{"100.00", USD, 920000},
		{"10.01", USD, 92092},
		{"0.01", USD, 92},
		{"100.00", GBP, 1170000},
		{"10.01", GBP, 117117},
		{"0.01", GBP, 117},
		{"999999999999.99", USD, 9199999999999908},
		{"999999999999.99", GBP, 11699999999999883},
	}
	for _, tt := range tests {
		t.Run(tt.amount+tt.cur.String(), func(t *testing.T) {
			e4, err := ParseE4(tt.amount)
			if err != nil {
				t.Fatal(err)
			}
			if got := ToEUR(e4, tt.cur); got != tt.want {
				t.Fatalf("ToEUR(%d, %s) = %d, want %d", e4, tt.cur, got, tt.want)
			}
		})
	}
}

func TestToEURNoOverflowAtMax(t *testing.T) {
	maxE4, err := ParseE4("999999999999.99")
	if err != nil {
		t.Fatal(err)
	}
	if maxE4 > math.MaxInt64/117 {
		t.Fatalf("max amount %d would overflow GBP conversion", maxE4)
	}
}

func TestParseCurrency(t *testing.T) {
	for _, s := range []string{"EUR", "USD", "GBP"} {
		c, ok := ParseCurrency(s)
		if !ok || c.String() != s {
			t.Fatalf("ParseCurrency(%q) = %v, %v", s, c, ok)
		}
	}
	for _, s := range []string{"", "eur", "JPY", "EURO", " EUR"} {
		if _, ok := ParseCurrency(s); ok {
			t.Fatalf("ParseCurrency(%q) accepted", s)
		}
	}
}

func TestAppendE4(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0.00"},
		{100, "0.01"},
		{125000, "12.50"},
		{92092, "9.2092"},
		{92090, "9.209"},
		{-100, "-0.01"},
		{9999999999999900, "999999999999.99"},
	}
	for _, tt := range tests {
		if got := string(AppendE4(nil, tt.in)); got != tt.want {
			t.Errorf("AppendE4(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
