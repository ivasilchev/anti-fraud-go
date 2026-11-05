package rules

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
)

const (
	ms     = int64(1)
	second = 1000 * ms
	minute = 60 * second
	hour   = 60 * minute
	day    = 24 * hour
)

// now is the fake clock shared by every test.
var now = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC).UnixMilli()

func amountEUR(t *testing.T, amount string, c money.Currency) int64 {
	t.Helper()
	e4, err := money.ParseE4(amount)
	if err != nil {
		t.Fatalf("ParseE4(%q): %v", amount, err)
	}
	return money.ToEUR(e4, c)
}

func eur(t *testing.T, amount string) int64 { return amountEUR(t, amount, money.EUR) }

func rulesOf(d Decision) []Rule {
	out := []Rule{}
	for _, r := range d.Reasons() {
		out = append(out, r.Rule)
	}
	return out
}

func fired(d Decision, rule Rule) bool { return slices.Contains(rulesOf(d), rule) }

func reasonFor(t *testing.T, d Decision, rule Rule) Reason {
	t.Helper()
	for _, r := range d.Reasons() {
		if r.Rule == rule {
			return r
		}
	}
	t.Fatalf("rule %s did not fire; fired %v", rule, rulesOf(d))
	return Reason{}
}

// ledger drives a Player through money events the way the ingest writer will:
// deposits get increasing seq numbers, and voids that miss RecentBets wait in
// PendingVoids for their bet.
type ledger struct {
	p   state.Player
	pv  *state.PendingVoids
	seq int64
}

func newLedger() *ledger { return &ledger{pv: state.NewPendingVoids()} }

func (l *ledger) deposit(t, amountEUR int64, pm uint32) {
	l.seq++
	l.p.ApplyDepositApproved(t, l.seq, amountEUR, pm)
}

func (l *ledger) depositWithSeq(t, seq, amountEUR int64, pm uint32) {
	l.p.ApplyDepositApproved(t, seq, amountEUR, pm)
}

func (l *ledger) bet(id string, t, amountEUR int64) {
	l.p.ApplyBet(state.HashID(id), t, amountEUR, l.pv.Take(id), now)
}

func (l *ledger) void(betID string, t int64) {
	if !l.p.ApplyVoid(state.HashID(betID)) {
		l.pv.Add(betID, t)
	}
}

func declinesAt(pms []uint32, ts ...int64) []state.Decline {
	out := make([]state.Decline, len(ts))
	for i, t := range ts {
		out[i] = state.Decline{T: t, PM: pms[i%len(pms)]}
	}
	return out
}

func TestR1CardTesting(t *testing.T) {
	recent := []int64{now - 1*minute, now - 2*minute, now - 3*minute, now - 4*minute, now - 5*minute}
	tests := []struct {
		name       string
		player     []state.Decline
		device     []state.Decline
		want       bool
		wantPlayer [2]int32 // declines, methods
		wantDevice [2]int32
		nilPlayer  bool
		noDevice   bool
	}{
		{
			name:       "5 declines with 3 methods fires",
			player:     declinesAt([]uint32{1, 2, 3}, recent...),
			want:       true,
			wantPlayer: [2]int32{5, 3},
		},
		{
			name:   "4 declines with 3 methods does not fire",
			player: declinesAt([]uint32{1, 2, 3}, recent[:4]...),
		},
		{
			name:   "5 declines with 2 methods does not fire",
			player: declinesAt([]uint32{1, 2}, recent...),
		},
		{
			name:       "decline exactly 10:00 old counts",
			player:     declinesAt([]uint32{1, 2, 3}, append(recent[:4:4], now-10*minute)...),
			want:       true,
			wantPlayer: [2]int32{5, 3},
		},
		{
			name:   "decline 10:00.001 old does not count",
			player: declinesAt([]uint32{1, 2, 3}, append(recent[:4:4], now-10*minute-1*ms)...),
		},
		{
			name:   "decline after now does not count",
			player: declinesAt([]uint32{1, 2, 3}, append(recent[:4:4], now+1*ms)...),
		},
		{
			name:   "only declines in window count towards distinct methods",
			player: append(declinesAt([]uint32{1, 2}, recent...), state.Decline{T: now - 11*minute, PM: 3}),
		},
		{
			name:       "device path fires with no player history",
			device:     declinesAt([]uint32{7, 8, 9}, recent...),
			want:       true,
			wantDevice: [2]int32{5, 3},
		},
		{
			name:       "device path fires for unknown player",
			device:     declinesAt([]uint32{7, 8, 9}, recent...),
			nilPlayer:  true,
			want:       true,
			wantDevice: [2]int32{5, 3},
		},
		{
			name:       "player path fires when device is below threshold",
			player:     declinesAt([]uint32{1, 2, 3}, recent...),
			device:     declinesAt([]uint32{7, 8}, recent...),
			want:       true,
			wantPlayer: [2]int32{5, 3},
		},
		{
			name:       "both paths fire and are both reported",
			player:     declinesAt([]uint32{1, 2, 3}, append(recent, now-6*minute)...),
			device:     declinesAt([]uint32{7, 8, 9, 10}, recent...),
			want:       true,
			wantPlayer: [2]int32{6, 3},
			wantDevice: [2]int32{5, 4},
		},
		{
			name:   "paths are evaluated separately, not summed",
			player: declinesAt([]uint32{1, 2, 3}, recent[:4]...),
			device: declinesAt([]uint32{4, 5, 6}, recent[:4]...),
		},
		{
			name:     "no device in request skips device path",
			player:   declinesAt([]uint32{1, 2}, recent...),
			noDevice: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p *state.Player
			if !tt.nilPlayer {
				p = &state.Player{Declines: tt.player}
			}
			var d *state.Device
			if !tt.noDevice {
				d = &state.Device{Declines: tt.device}
			}
			dec := Evaluate(Request{Action: DepositAttempt}, p, d, now)
			if got := fired(dec, R1CardTesting); got != tt.want {
				t.Fatalf("R1 fired = %v, want %v", got, tt.want)
			}
			if !tt.want {
				if dec.Outcome != Allow {
					t.Fatalf("outcome = %s, want ALLOW", dec.Outcome)
				}
				return
			}
			if dec.Outcome != Review {
				t.Fatalf("outcome = %s, want REVIEW", dec.Outcome)
			}
			r := reasonFor(t, dec, R1CardTesting)
			if got := [2]int32{r.PlayerDeclines, r.PlayerMethods}; got != tt.wantPlayer {
				t.Errorf("player path = %v, want %v", got, tt.wantPlayer)
			}
			if got := [2]int32{r.DeviceDeclines, r.DeviceMethods}; got != tt.wantDevice {
				t.Errorf("device path = %v, want %v", got, tt.wantDevice)
			}
		})
	}
}

func TestR1CountsDeclinesRecordedThroughApply(t *testing.T) {
	var p state.Player
	var d state.Device
	for i, pm := range []uint32{1, 2, 3, 1, 2} {
		ts := now - int64(i)*minute
		p.AddDecline(ts, pm, now)
		d.AddDecline(ts, pm, now)
	}
	p.AddDecline(now-20*minute, 9, now) // stale, outside the window
	dec := Evaluate(Request{Action: DepositAttempt}, &p, &d, now)
	r := reasonFor(t, dec, R1CardTesting)
	if r.PlayerDeclines != 5 || r.PlayerMethods != 3 || r.DeviceDeclines != 5 || r.DeviceMethods != 3 {
		t.Fatalf("reason = %+v, want 5/3 on both paths", r)
	}
}

func evalWithdrawal(p *state.Player) Decision {
	return Evaluate(Request{Action: Withdrawal}, p, nil, now)
}

func TestR2CashoutWithoutPlay(t *testing.T) {
	tests := []struct {
		name         string
		build        func(t *testing.T, l *ledger)
		want         bool
		wantTurnover string // expected turnover in EUR when firing
	}{
		{
			name: "turnover equal to deposit does not fire",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-30*minute, eur(t, "50.00"), 1)
				l.bet("b1", now-20*minute, eur(t, "20.00"))
				l.bet("b2", now-10*minute, eur(t, "30.00"))
			},
		},
		{
			name: "turnover 0.01 EUR below deposit fires",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-30*minute, eur(t, "50.00"), 1)
				l.bet("b1", now-20*minute, eur(t, "20.00"))
				l.bet("b2", now-10*minute, eur(t, "29.99"))
			},
			want:         true,
			wantTurnover: "49.99",
		},
		{
			name: "voided bet is excluded",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-5*minute, eur(t, "50.00"), 1)
				l.bet("b1", now-4*minute, eur(t, "30.00"))
				l.bet("b2", now-3*minute, eur(t, "20.00"))
				l.void("b2", now-2*minute)
			},
			want:         true,
			wantTurnover: "30.00",
		},
		{
			name: "void arriving before its bet is handled",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-5*minute, eur(t, "50.00"), 1)
				l.void("b2", now-1*minute)
				l.bet("b1", now-4*minute, eur(t, "30.00"))
				l.bet("b2", now-3*minute, eur(t, "20.00"))
				if l.pv.Len() != 0 {
					t.Fatalf("pending voids = %d, want 0", l.pv.Len())
				}
			},
			want:         true,
			wantTurnover: "30.00",
		},
		{
			name: "late deposit becomes the last deposit and turnover is recomputed",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-20*minute, eur(t, "50.00"), 1)
				l.bet("b1", now-4*minute, eur(t, "30.00"))
				l.bet("b2", now-2*minute, eur(t, "30.00"))
				// Late deposit between the two bets: only b2 now counts.
				l.deposit(now-3*minute, eur(t, "50.00"), 1)
			},
			want:         true,
			wantTurnover: "30.00",
		},
		{
			name: "late deposit older than the last deposit is ignored",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, eur(t, "50.00"), 1)
				l.bet("b1", now-3*minute, eur(t, "50.00"))
				l.deposit(now-5*minute, eur(t, "500.00"), 1)
			},
		},
		{
			name: "deposit at the same instant with a later seq wins",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, eur(t, "10.00"), 1)
				l.bet("b1", now-3*minute, eur(t, "20.00"))
				l.deposit(now-4*minute, eur(t, "25.00"), 1)
			},
			want:         true,
			wantTurnover: "20.00",
		},
		{
			name: "bet before the deposit is not counted",
			build: func(t *testing.T, l *ledger) {
				l.bet("b1", now-5*minute, eur(t, "100.00"))
				l.deposit(now-4*minute, eur(t, "50.00"), 1)
			},
			want:         true,
			wantTurnover: "0.00",
		},
		{
			name: "late bet timestamped before the deposit is not counted",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, eur(t, "50.00"), 1)
				l.bet("b1", now-5*minute, eur(t, "100.00"))
			},
			want:         true,
			wantTurnover: "0.00",
		},
		{
			name: "bet at the deposit instant counts",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, eur(t, "50.00"), 1)
				l.bet("b1", now-4*minute, eur(t, "50.00"))
			},
		},
		{
			name: "USD deposit converts exactly: 100.00 USD = 92.00 EUR, matched",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, amountEUR(t, "100.00", money.USD), 1)
				l.bet("b1", now-3*minute, eur(t, "92.00"))
			},
		},
		{
			name: "USD deposit converts exactly: 0.01 EUR short fires",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, amountEUR(t, "100.00", money.USD), 1)
				l.bet("b1", now-3*minute, eur(t, "91.99"))
			},
			want:         true,
			wantTurnover: "91.99",
		},
		{
			name: "GBP deposit converts exactly: 10.01 GBP = 11.7117 EUR, matched by GBP bet",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, amountEUR(t, "10.01", money.GBP), 1)
				l.bet("b1", now-3*minute, amountEUR(t, "10.01", money.GBP))
			},
		},
		{
			name: "GBP deposit converts exactly: 11.71 EUR is below 11.7117 EUR",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, amountEUR(t, "10.01", money.GBP), 1)
				l.bet("b1", now-3*minute, eur(t, "11.71"))
			},
			want:         true,
			wantTurnover: "11.71",
		},
		{
			name: "USD bet against GBP deposit: 12.73 USD = 11.7116 EUR is below 11.7117 EUR",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, amountEUR(t, "10.01", money.GBP), 1)
				l.bet("b1", now-3*minute, amountEUR(t, "12.73", money.USD))
			},
			want:         true,
			wantTurnover: "11.7116",
		},
		{
			name: "USD bet against GBP deposit: 12.74 USD = 11.7208 EUR covers 11.7117 EUR",
			build: func(t *testing.T, l *ledger) {
				l.deposit(now-4*minute, amountEUR(t, "10.01", money.GBP), 1)
				l.bet("b1", now-3*minute, amountEUR(t, "12.74", money.USD))
			},
		},
		{
			name: "no deposit does not fire",
			build: func(t *testing.T, l *ledger) {
				l.bet("b1", now-3*minute, eur(t, "1.00"))
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLedger()
			tt.build(t, l)
			dec := evalWithdrawal(&l.p)
			if got := fired(dec, R2CashoutWithoutPlay); got != tt.want {
				t.Fatalf("R2 fired = %v, want %v (turnover %d, deposit %d)",
					got, tt.want, l.p.TurnoverEUR, l.p.LastDepositEUR)
			}
			if !tt.want {
				return
			}
			if dec.Outcome != Hold {
				t.Fatalf("outcome = %s, want HOLD", dec.Outcome)
			}
			r := reasonFor(t, dec, R2CashoutWithoutPlay)
			if want := expectE4(t, tt.wantTurnover); r.TurnoverEUR != want {
				t.Errorf("turnover = %d, want %d", r.TurnoverEUR, want)
			}
			if r.DepositEUR != l.p.LastDepositEUR {
				t.Errorf("deposit = %d, want %d", r.DepositEUR, l.p.LastDepositEUR)
			}
		})
	}
}

// expectE4 parses an expected EUR value with up to 4 decimals ("0.00", "11.7116").
func expectE4(t *testing.T, s string) int64 {
	t.Helper()
	whole, frac, _ := strings.Cut(s, ".")
	w, err1 := strconv.ParseInt(whole, 10, 64)
	f, err2 := strconv.ParseInt((frac + "0000")[:4], 10, 64)
	if err1 != nil || err2 != nil || len(frac) > 4 {
		t.Fatalf("bad expected amount %q", s)
	}
	return w*10000 + f
}

// TestR2OrderIndependence applies random deposits, bets and voids within a
// 5-minute span in many orders and checks the result against a brute-force
// oracle: last deposit = latest deposit; turnover = sum of non-voided bets at
// or after it.
func TestR2OrderIndependence(t *testing.T) {
	type dep struct{ t, seq, eur int64 }
	type bet struct {
		id     string
		t, eur int64
		voided bool
	}
	rng := rand.New(rand.NewPCG(42, 0))
	span := 5 * minute
	for scenario := range 300 {
		// Deposits get distinct timestamps: ties at equal t are broken by seq,
		// which is assigned in arrival order and so is order-dependent by design.
		var deps []dep
		for i, off := range rng.Perm(int(span / second))[:1+rng.IntN(3)] {
			deps = append(deps, dep{t: now - int64(off)*second, seq: int64(i + 1), eur: (1 + rng.Int64N(200)) * 10000})
		}
		var bets []bet
		for i := range rng.IntN(8) {
			bets = append(bets, bet{
				id:     "b" + strconv.Itoa(i),
				t:      now - rng.Int64N(span/second+1)*second, // whole seconds, so bets often tie with deposits
				eur:    (1 + rng.Int64N(100)) * 10000,
				voided: rng.IntN(3) == 0,
			})
		}

		last := deps[0]
		for _, d := range deps[1:] {
			if d.t > last.t {
				last = d
			}
		}
		var wantTurnover int64
		for _, b := range bets {
			if !b.voided && b.t >= last.t {
				wantTurnover += b.eur
			}
		}

		var ops []func(*ledger)
		for _, d := range deps {
			ops = append(ops, func(l *ledger) { l.depositWithSeq(d.t, d.seq, d.eur, 1) })
		}
		for _, b := range bets {
			ops = append(ops, func(l *ledger) { l.bet(b.id, b.t, b.eur) })
			if b.voided {
				ops = append(ops, func(l *ledger) { l.void(b.id, b.t+second) })
			}
		}

		for perm := range 20 {
			l := newLedger()
			for _, i := range rng.Perm(len(ops)) {
				ops[i](l)
			}
			if l.p.TurnoverEUR != wantTurnover || l.p.LastDepositT != last.t || l.p.LastDepositEUR != last.eur {
				t.Fatalf("scenario %d perm %d: turnover %d, last deposit (%d, %d); want %d, (%d, %d)\ndeposits %+v\nbets %+v",
					scenario, perm, l.p.TurnoverEUR, l.p.LastDepositT, l.p.LastDepositEUR,
					wantTurnover, last.t, last.eur, deps, bets)
			}
			if l.pv.Len() != 0 {
				t.Fatalf("scenario %d perm %d: %d voids left pending", scenario, perm, l.pv.Len())
			}
			if got, want := fired(evalWithdrawal(&l.p), R2CashoutWithoutPlay), wantTurnover < last.eur; got != want {
				t.Fatalf("scenario %d perm %d: R2 fired = %v, want %v", scenario, perm, got, want)
			}
		}
	}
}

func TestR2UnknownPlayerDoesNotFire(t *testing.T) {
	if fired(evalWithdrawal(nil), R2CashoutWithoutPlay) {
		t.Fatal("R2 fired for unknown player")
	}
}

type takeover struct {
	reset, payout, login int64 // 0 means the event never happened
}

func (tk takeover) player() *state.Player {
	p := &state.Player{}
	if tk.reset != 0 {
		p.ApplyPasswordReset(tk.reset)
	}
	if tk.payout != 0 {
		p.ApplyPayoutMethodAdded(tk.payout)
	}
	if tk.login != 0 {
		p.SeeDevice(42, tk.login, true)
	}
	return p
}

func TestR3AccountTakeover(t *testing.T) {
	edge := now - 60*minute
	past := edge - 1*ms
	tests := []struct {
		name string
		tk   takeover
		want bool
	}{
		{"all three recent fires", takeover{now - 5*minute, now - 3*minute, now - 1*minute}, true},
		{"all three at exactly 60:00 fires", takeover{edge, edge, edge}, true},
		{"reset at 60:00.001 does not fire", takeover{past, edge, edge}, false},
		{"payout method at 60:00.001 does not fire", takeover{edge, past, edge}, false},
		{"new device login at 60:00.001 does not fire", takeover{edge, edge, past}, false},
		{"missing reset does not fire", takeover{0, now - minute, now - minute}, false},
		{"missing payout method does not fire", takeover{now - minute, 0, now - minute}, false},
		{"missing new device does not fire", takeover{now - minute, now - minute, 0}, false},
		{"event after now does not count", takeover{now + 1*ms, now - minute, now - minute}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dec := evalWithdrawal(tt.tk.player())
			if got := fired(dec, R3AccountTakeover); got != tt.want {
				t.Fatalf("R3 fired = %v, want %v", got, tt.want)
			}
			if tt.want && dec.Outcome != Hold {
				t.Fatalf("outcome = %s, want HOLD", dec.Outcome)
			}
		})
	}
}

func TestR3MaxOfKeepsLatestTimestamp(t *testing.T) {
	p := takeover{now - 5*minute, now - 5*minute, now - 5*minute}.player()
	// Older, late-arriving events must not move the timestamps back.
	p.ApplyPasswordReset(now - 2*day)
	p.ApplyPayoutMethodAdded(now - 2*day)
	if !fired(evalWithdrawal(p), R3AccountTakeover) {
		t.Fatal("R3 did not fire after older late events")
	}
}

func TestR3DeviceNovelty(t *testing.T) {
	const dev = 42
	tests := []struct {
		name   string
		seeDev func(p *state.Player)
		want   bool
	}{
		{
			name: "previously seen device does not fire",
			seeDev: func(p *state.Player) {
				p.SeeDevice(dev, now-2*day, false) // e.g. a deposit from this device
				p.SeeDevice(dev, now-10*minute, true)
			},
		},
		{
			name: "device first seen via a non-login event does not fire",
			seeDev: func(p *state.Player) {
				p.SeeDevice(dev, now-10*minute, false)
			},
		},
		{
			name: "late, earlier event for the device makes it no longer new",
			seeDev: func(p *state.Player) {
				p.SeeDevice(dev, now-10*minute, true)
				p.SeeDevice(dev, now-12*minute, false)
			},
		},
		{
			name: "late, earlier login replaces a non-login first sighting",
			seeDev: func(p *state.Player) {
				p.SeeDevice(dev, now-10*minute, false)
				p.SeeDevice(dev, now-12*minute, true)
			},
			want: true,
		},
		{
			name: "login at the same instant as the first sighting counts as login",
			seeDev: func(p *state.Player) {
				p.SeeDevice(dev, now-10*minute, false)
				p.SeeDevice(dev, now-10*minute, true)
			},
			want: true,
		},
		{
			name: "a later login on an old device does not help, another new device does",
			seeDev: func(p *state.Player) {
				p.SeeDevice(dev, now-2*day, false)
				p.SeeDevice(dev, now-1*minute, true)
				p.SeeDevice(dev+1, now-1*minute, true)
			},
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := takeover{reset: now - 5*minute, payout: now - 5*minute}.player()
			tt.seeDev(p)
			if got := fired(evalWithdrawal(p), R3AccountTakeover); got != tt.want {
				t.Fatalf("R3 fired = %v, want %v (devices %+v)", got, tt.want, p.Devices)
			}
		})
	}
}

func TestR4LinkedAccounts(t *testing.T) {
	edge := now - 30*day
	tests := []struct {
		name     string
		lastSeen []int64 // one entry per player
		want     bool
		wantN    int32
	}{
		{"3 players fires", []int64{now - day, now - hour, now - minute}, true, 3},
		{"2 players does not fire", []int64{now - day, now - minute}, false, 0},
		{"player last seen exactly 30 days ago counts", []int64{edge, now - hour, now - minute}, true, 3},
		{"player last seen 30 days + 1 ms ago does not count", []int64{edge - 1*ms, now - hour, now - minute}, false, 0},
		{"5 players reports 5", []int64{now - 1, now - 2, now - 3, now - 4, now - 5}, true, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &state.Device{}
			for i, ts := range tt.lastSeen {
				d.Players = append(d.Players, state.DevicePlayer{Player: uint32(i), LastSeenT: ts})
			}
			for _, action := range []Action{Withdrawal, BonusClaim} {
				dec := Evaluate(Request{Action: action}, &state.Player{}, d, now)
				if got := fired(dec, R4LinkedAccounts); got != tt.want {
					t.Fatalf("action %d: R4 fired = %v, want %v", action, got, tt.want)
				}
				if tt.want {
					if r := reasonFor(t, dec, R4LinkedAccounts); r.LinkedPlayers != tt.wantN {
						t.Fatalf("linked players = %d, want %d", r.LinkedPlayers, tt.wantN)
					}
				}
			}
		})
	}
}

func TestR4SeePlayerCountsDistinctPlayers(t *testing.T) {
	var d state.Device
	d.SeePlayer(1, now-2*minute, now)
	d.SeePlayer(1, now-1*minute, now)
	d.SeePlayer(2, now-1*minute, now)
	dec := Evaluate(Request{Action: BonusClaim}, nil, &d, now)
	if fired(dec, R4LinkedAccounts) {
		t.Fatal("R4 fired for 2 distinct players seen 3 times")
	}
	d.SeePlayer(2, now-31*day, now) // older sighting must not move lastSeen back
	d.SeePlayer(3, now-1*minute, now)
	if !fired(Evaluate(Request{Action: BonusClaim}, nil, &d, now), R4LinkedAccounts) {
		t.Fatal("R4 did not fire for 3 distinct players")
	}
}

func TestR4SkippedWithoutDevice(t *testing.T) {
	if dec := Evaluate(Request{Action: BonusClaim}, &state.Player{}, nil, now); len(dec.Reasons()) != 0 {
		t.Fatalf("reasons = %v, want none", rulesOf(dec))
	}
}

func TestR5NewPayoutMethod(t *testing.T) {
	const pm = 7
	tests := []struct {
		name  string
		build func(p *state.Player)
		req   Request
		nilP  bool
		want  bool
	}{
		{
			name:  "method used in an approved deposit does not fire",
			build: func(p *state.Player) { p.ApplyDepositApproved(now-day, 1, 10000, pm) },
			req:   Request{Action: Withdrawal, PaymentMethod: pm, PaymentMethodKnown: true},
		},
		{
			name:  "method used only in declined deposits fires",
			build: func(p *state.Player) { p.AddDecline(now-minute, pm, now) },
			req:   Request{Action: Withdrawal, PaymentMethod: pm, PaymentMethodKnown: true},
			want:  true,
		},
		{
			name:  "approved deposit with a different method fires",
			build: func(p *state.Player) { p.ApplyDepositApproved(now-day, 1, 10000, pm+1) },
			req:   Request{Action: Withdrawal, PaymentMethod: pm, PaymentMethodKnown: true},
			want:  true,
		},
		{
			name: "unknown player fires",
			nilP: true,
			req:  Request{Action: Withdrawal, PaymentMethod: pm, PaymentMethodKnown: true},
			want: true,
		},
		{
			name:  "never-seen payment method fires",
			build: func(p *state.Player) { p.ApplyDepositApproved(now-day, 1, 10000, 0) },
			req:   Request{Action: Withdrawal, PaymentMethodKnown: false},
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p *state.Player
			if !tt.nilP {
				p = &state.Player{}
				tt.build(p)
			}
			dec := Evaluate(tt.req, p, nil, now)
			if got := fired(dec, R5NewPayoutMethod); got != tt.want {
				t.Fatalf("R5 fired = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApprovedPMsStaySortedAndUnique(t *testing.T) {
	var p state.Player
	for i, pm := range []uint32{5, 1, 9, 1, 5, 3} {
		p.ApplyDepositApproved(now-int64(i)*minute, int64(i+1), 100, pm)
	}
	if want := []uint32{1, 3, 5, 9}; !slices.Equal(p.ApprovedPMs, want) {
		t.Fatalf("ApprovedPMs = %v, want %v", p.ApprovedPMs, want)
	}
}

// fraudster fires R2, R3, R4 and R5 on WITHDRAWAL and R1 on DEPOSIT_ATTEMPT.
func fraudster(t *testing.T) (*state.Player, *state.Device) {
	t.Helper()
	l := newLedger()
	l.deposit(now-10*minute, eur(t, "100.00"), 1)
	l.bet("b1", now-9*minute, eur(t, "10.00"))
	p := &l.p
	p.ApplyPasswordReset(now - 8*minute)
	p.ApplyPayoutMethodAdded(now - 7*minute)
	p.SeeDevice(42, now-6*minute, true)
	for i, pm := range []uint32{2, 3, 4, 2, 3} {
		p.AddDecline(now-int64(i)*minute, pm, now)
	}
	d := &state.Device{}
	for i := range uint32(3) {
		d.SeePlayer(i, now-minute, now)
	}
	return p, d
}

func TestCombinations(t *testing.T) {
	t.Run("strictest outcome wins and all fired rules are listed in order", func(t *testing.T) {
		p, d := fraudster(t)
		dec := Evaluate(Request{Action: Withdrawal, PaymentMethod: 99, PaymentMethodKnown: true}, p, d, now)
		if dec.Outcome != Hold {
			t.Fatalf("outcome = %s, want HOLD", dec.Outcome)
		}
		want := []Rule{R2CashoutWithoutPlay, R3AccountTakeover, R4LinkedAccounts, R5NewPayoutMethod}
		if got := rulesOf(dec); !slices.Equal(got, want) {
			t.Fatalf("rules = %v, want %v", got, want)
		}
	})
	t.Run("several REVIEW rules give REVIEW", func(t *testing.T) {
		_, d := fraudster(t)
		dec := Evaluate(Request{Action: Withdrawal, PaymentMethodKnown: false}, &state.Player{}, d, now)
		if dec.Outcome != Review {
			t.Fatalf("outcome = %s, want REVIEW", dec.Outcome)
		}
		if got, want := rulesOf(dec), []Rule{R4LinkedAccounts, R5NewPayoutMethod}; !slices.Equal(got, want) {
			t.Fatalf("rules = %v, want %v", got, want)
		}
	})
	t.Run("no rule fired gives ALLOW with empty reasons", func(t *testing.T) {
		p := &state.Player{}
		p.ApplyDepositApproved(now-day, 1, 10000, 1)
		p.ApplyBet(state.HashID("b"), now-hour, 10000, false, now)
		dec := Evaluate(Request{Action: Withdrawal, PaymentMethod: 1, PaymentMethodKnown: true}, p, nil, now)
		if dec.Outcome != Allow || len(dec.Reasons()) != 0 {
			t.Fatalf("got %s %v, want ALLOW []", dec.Outcome, rulesOf(dec))
		}
	})
	t.Run("DEPOSIT_ATTEMPT evaluates only R1", func(t *testing.T) {
		p, d := fraudster(t)
		dec := Evaluate(Request{Action: DepositAttempt, PaymentMethod: 99, PaymentMethodKnown: true}, p, d, now)
		if got, want := rulesOf(dec), []Rule{R1CardTesting}; !slices.Equal(got, want) || dec.Outcome != Review {
			t.Fatalf("got %s %v, want REVIEW %v", dec.Outcome, got, want)
		}
	})
	t.Run("BONUS_CLAIM evaluates only R4", func(t *testing.T) {
		p, d := fraudster(t)
		dec := Evaluate(Request{Action: BonusClaim}, p, d, now)
		if got, want := rulesOf(dec), []Rule{R4LinkedAccounts}; !slices.Equal(got, want) || dec.Outcome != Review {
			t.Fatalf("got %s %v, want REVIEW %v", dec.Outcome, got, want)
		}
	})
}

func TestReasonDetail(t *testing.T) {
	tests := []struct {
		r    Reason
		want string
	}{
		{Reason{Rule: R1CardTesting, DeviceDeclines: 6, DeviceMethods: 3},
			"6 declined deposits with 3 payment methods on device in last 10m"},
		{Reason{Rule: R1CardTesting, PlayerDeclines: 5, PlayerMethods: 3, DeviceDeclines: 6, DeviceMethods: 4},
			"5 declined deposits with 3 payment methods on player in last 10m; 6 declined deposits with 4 payment methods on device in last 10m"},
		{Reason{Rule: R2CashoutWithoutPlay, TurnoverEUR: 300000, DepositEUR: 920000},
			"turnover 30.00 EUR below last deposit 92.00 EUR"},
		{Reason{Rule: R3AccountTakeover},
			"password reset, payout method added and login from new device in last 60m"},
		{Reason{Rule: R4LinkedAccounts, LinkedPlayers: 4}, "device used by 4 players in last 30d"},
		{Reason{Rule: R5NewPayoutMethod}, "payment method not used in any approved deposit"},
	}
	for _, tt := range tests {
		if got := string(tt.r.AppendDetail(nil)); got != tt.want {
			t.Errorf("%s detail = %q, want %q", tt.r.Rule, got, tt.want)
		}
	}
}

func TestParseAction(t *testing.T) {
	for s, want := range map[string]Action{"DEPOSIT_ATTEMPT": DepositAttempt, "WITHDRAWAL": Withdrawal, "BONUS_CLAIM": BonusClaim} {
		if got, ok := ParseAction(s); !ok || got != want {
			t.Errorf("ParseAction(%q) = %v, %v", s, got, ok)
		}
	}
	for _, s := range []string{"", "withdrawal", "DEPOSIT", "BONUS"} {
		if _, ok := ParseAction(s); ok {
			t.Errorf("ParseAction(%q) accepted", s)
		}
	}
}

func TestEvaluateDoesNotAllocate(t *testing.T) {
	p, d := fraudster(t)
	for i, pm := range []uint32{5, 6, 7, 5, 6} {
		d.AddDecline(now-int64(i)*minute, pm, now)
	}
	for _, req := range []Request{
		{Action: DepositAttempt},
		{Action: Withdrawal, PaymentMethod: 99, PaymentMethodKnown: true},
		{Action: BonusClaim},
	} {
		dec := Evaluate(req, p, d, now)
		if len(dec.Reasons()) == 0 {
			t.Fatalf("action %d: fixture fires no rule", req.Action)
		}
		if allocs := testing.AllocsPerRun(100, func() { _ = Evaluate(req, p, d, now) }); allocs != 0 {
			t.Fatalf("action %d: Evaluate allocates %.0f times per call, want 0", req.Action, allocs)
		}
	}
}
