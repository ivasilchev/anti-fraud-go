package rules

import (
	"strconv"
	"testing"

	"github.com/devastion/anti-fraud-go/internal/state"
)

// benchState builds a busy player and a shared device: 50 recent bets,
// 10 declines, 4 devices, 3 approved methods, 6 players on the device.
func benchState() (*state.Player, *state.Device) {
	p := &state.Player{}
	for i := range 3 {
		p.ApplyDepositApproved(now-int64(10-i)*minute, int64(i+1), 500000, uint32(i))
	}
	for i := range 50 {
		p.ApplyBet(state.HashID("bet-"+strconv.Itoa(i)), now-int64(i)*second, 10000, false, now)
	}
	d := &state.Device{}
	for i := range 10 {
		p.AddDecline(now-int64(i)*minute, uint32(i%4), now)
		d.AddDecline(now-int64(i)*minute, uint32(i%4), now)
	}
	for i := range 4 {
		p.SeeDevice(uint32(i), now-int64(i)*hour, i%2 == 0)
	}
	p.ApplyPasswordReset(now - 30*minute)
	p.ApplyPayoutMethodAdded(now - 20*minute)
	for i := range 6 {
		d.SeePlayer(uint32(i), now-int64(i)*day, now)
	}
	return p, d
}

func BenchmarkDecision(b *testing.B) {
	p, d := benchState()
	reqs := []Request{
		{Action: DepositAttempt},
		{Action: Withdrawal, PaymentMethod: 1, PaymentMethodKnown: true},
		{Action: BonusClaim},
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		dec := Evaluate(reqs[i%len(reqs)], p, d, now)
		if dec.Outcome > Hold {
			b.Fatal("impossible outcome")
		}
		i++
	}
}
