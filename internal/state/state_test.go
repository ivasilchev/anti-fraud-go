package state

import (
	"math"
	"testing"
	"time"
)

var now = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli()

const minute = int64(60 * 1000)

func TestEventTypeRoundTrip(t *testing.T) {
	for et := EventLogin; et <= EventWithdrawalRequested; et++ {
		got, ok := ParseEventType(et.String())
		if !ok || got != et {
			t.Errorf("ParseEventType(%q) = %v, %v; want %v", et.String(), got, ok, et)
		}
	}
	if _, ok := ParseEventType("login"); ok {
		t.Error("ParseEventType is case-sensitive")
	}
}

func TestEventTypeCodesAreStable(t *testing.T) {
	// Persisted as smallint: changing these breaks existing data.
	want := map[EventType]uint8{
		EventLogin: 1, EventPasswordReset: 2, EventDepositAttempt: 3, EventDepositApproved: 4,
		EventDepositDeclined: 5, EventBet: 6, EventWin: 7, EventBetVoided: 8,
		EventPayoutMethodAdded: 9, EventWithdrawalRequested: 10,
	}
	for et, code := range want {
		if uint8(et) != code {
			t.Errorf("%s = %d, want %d", et, et, code)
		}
	}
}

func TestHashIDIsFNV1a(t *testing.T) {
	// Reference values for 64-bit FNV-1a.
	if got := HashID(""); got != 0xcbf29ce484222325 {
		t.Errorf("HashID(\"\") = %#x", got)
	}
	if got := HashID("a"); got != 0xaf63dc4c8601ec8c {
		t.Errorf("HashID(\"a\") = %#x", got)
	}
}

func TestDeclinesArePrunedBeyond15Minutes(t *testing.T) {
	var p Player
	p.AddDecline(now-16*minute, 1, now)
	p.AddDecline(now-15*minute, 2, now)
	p.AddDecline(now, 3, now)
	if len(p.Declines) != 2 || p.Declines[0].PM != 2 {
		t.Fatalf("declines = %+v, want the 15m-old and new ones", p.Declines)
	}
}

func TestRecentBetsArePrunedBeyond6Minutes(t *testing.T) {
	var p Player
	p.ApplyBet(1, now-7*minute, 100, false, now)
	p.ApplyBet(2, now-6*minute, 100, false, now)
	p.ApplyBet(3, now, 100, false, now)
	if len(p.RecentBets) != 2 || p.RecentBets[0].IDHash != 2 {
		t.Fatalf("bets = %+v, want the 6m-old and new ones", p.RecentBets)
	}
	if p.TurnoverEUR != 300 {
		t.Fatalf("turnover = %d, want 300: pruning must not change turnover", p.TurnoverEUR)
	}
}

func TestVoidIsIdempotent(t *testing.T) {
	var p Player
	p.ApplyBet(1, now, 100, false, now)
	p.ApplyVoid(1)
	p.ApplyVoid(1)
	if p.TurnoverEUR != 0 {
		t.Fatalf("turnover = %d, want 0", p.TurnoverEUR)
	}
	if p.ApplyVoid(2) {
		t.Fatal("ApplyVoid found a bet that does not exist")
	}
}

func TestVoidOfBetBeforeLastDepositKeepsTurnover(t *testing.T) {
	var p Player
	p.ApplyBet(1, now-3*minute, 100, false, now)
	p.ApplyDepositApproved(now-2*minute, 1, 500, 1)
	p.ApplyBet(2, now-1*minute, 200, false, now)
	if !p.ApplyVoid(1) {
		t.Fatal("bet 1 not found")
	}
	if p.TurnoverEUR != 200 {
		t.Fatalf("turnover = %d, want 200", p.TurnoverEUR)
	}
	// Marked voided anyway, so no later recompute can count it.
	if !p.RecentBets[0].Voided {
		t.Fatal("bet before the last deposit was not marked voided")
	}
}

func TestTurnoverSaturatesInsteadOfWrapping(t *testing.T) {
	const maxBetEUR = 11699999999999883 // 999999999999.99 GBP in 10^-4 EUR
	var p Player
	p.ApplyDepositApproved(now-5*minute, 1, maxBetEUR, 1)
	for i := range 1000 {
		p.ApplyBet(uint64(i), now-4*minute, maxBetEUR, false, now)
	}
	if p.TurnoverEUR != math.MaxInt64 {
		t.Fatalf("turnover = %d, want saturation at MaxInt64", p.TurnoverEUR)
	}
	p.ApplyDepositApproved(now-5*minute, 2, maxBetEUR, 1) // recompute path
	if p.TurnoverEUR != math.MaxInt64 {
		t.Fatalf("recomputed turnover = %d, want saturation at MaxInt64", p.TurnoverEUR)
	}
}

func TestDevicePlayersPrunedBeyond30Days(t *testing.T) {
	var d Device
	d.SeePlayer(1, now-DevicePlayersRetentionMs-1, now)
	d.SeePlayer(2, now-DevicePlayersRetentionMs, now)
	d.SeePlayer(3, now, now)
	if len(d.Players) != 2 || d.Players[0].Player != 2 {
		t.Fatalf("players = %+v, want players 2 and 3", d.Players)
	}
}

func TestPendingVoids(t *testing.T) {
	pv := NewPendingVoids()
	pv.Add("old", now-61*minute)
	pv.Add("edge", now-60*minute)
	pv.Add("new", now)
	pv.Prune(now)
	if pv.Len() != 2 || pv.Take("old") {
		t.Fatalf("len = %d; void older than 1h should be pruned", pv.Len())
	}
	if !pv.Take("edge") || pv.Take("edge") {
		t.Fatal("Take must return true once")
	}
}
