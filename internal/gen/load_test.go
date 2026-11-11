package gen

import (
	"math/rand/v2"
	"testing"
)

func TestUUIDV7(t *testing.T) {
	const ms = 1_700_000_000_123
	u := uuidV7(ms, rand.New(rand.NewPCG(1, 2)))
	if got := int64(u[0])<<40 | int64(u[1])<<32 | int64(u[2])<<24 | int64(u[3])<<16 | int64(u[4])<<8 | int64(u[5]); got != ms {
		t.Errorf("timestamp = %d, want %d", got, ms)
	}
	if u[6]>>4 != 7 || u[8]>>6 != 0b10 {
		t.Errorf("version/variant bits wrong: %x", u)
	}
	if u != uuidV7(ms, rand.New(rand.NewPCG(1, 2))) {
		t.Error("same seed gave a different uuid")
	}
	if u == uuidV7(ms, rand.New(rand.NewPCG(1, 3))) {
		t.Error("another seed gave the same uuid")
	}
}
