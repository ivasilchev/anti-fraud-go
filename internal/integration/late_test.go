package integration

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

// scenario returns a player's history within the 5 minutes before t0. IDs are
// prefixed so that several copies can live in one service.
func scenario(prefix string) []ev {
	p := prefix + "p"
	d1, d2 := prefix+"d1", prefix+"d2"
	at := func(d time.Duration) time.Time { return t0.Add(-d) }
	withDevice := func(e ev, dev string) ev { e["deviceId"] = dev; return e }
	return []ev{
		{"eventId": prefix + "login", "playerId": p, "type": "LOGIN", "occurredAt": ts(at(4 * time.Minute)), "deviceId": d1},
		{"eventId": prefix + "reset", "playerId": p, "type": "PASSWORD_RESET", "occurredAt": ts(at(3 * time.Minute))},
		{"eventId": prefix + "payout", "playerId": p, "type": "PAYOUT_METHOD_ADDED", "occurredAt": ts(at(2 * time.Minute)), "paymentMethodId": prefix + "pm3"},
		// d1 seen again later, not via LOGIN: the earliest sighting (LOGIN) must win.
		withDevice(bet(prefix+"b0", p, "1.00", at(3*time.Minute)), d1),
		deposit(prefix+"dep1", p, "100.00", prefix+"pm1", at(270*time.Second)),
		deposit(prefix+"dep2", p, "50.00", prefix+"pm1", at(time.Minute)),
		bet(prefix+"b1", p, "30.00", at(4*time.Minute)), // before dep2: not turnover
		bet(prefix+"b2", p, "20.00", at(50*time.Second)),
		bet(prefix+"b3", p, "25.00", at(30*time.Second)),
		bet(prefix+"b4", p, "10.00", at(20*time.Second)),
		void(prefix+"v3", p, prefix+"b3", at(10*time.Second)),
		withDevice(decline(prefix+"x1", p, prefix+"pm1", at(3*time.Minute)), d2),
		withDevice(decline(prefix+"x2", p, prefix+"pm2", at(150*time.Second)), d2),
		withDevice(decline(prefix+"x3", p, prefix+"pm4", at(2*time.Minute)), d2),
		withDevice(decline(prefix+"x4", p, prefix+"pm1", at(90*time.Second)), d2),
		withDevice(decline(prefix+"x5", p, prefix+"pm2", at(time.Minute)), d2),
		{"eventId": prefix + "q1", "playerId": prefix + "q1", "type": "LOGIN", "occurredAt": ts(at(time.Minute)), "deviceId": d2},
		{"eventId": prefix + "q2", "playerId": prefix + "q2", "type": "LOGIN", "occurredAt": ts(at(4 * time.Minute)), "deviceId": d2},
	}
}

// scenarioDecisions asks every decision that depends on the scenario state.
func scenarioDecisions(a *app, prefix string) []string {
	p, d2 := prefix+"p", prefix+"d2"
	reqs := []map[string]any{
		withdrawal(p, prefix+"pm3"),
		withdrawal(p, prefix+"pm1"),
		depositAttempt(p, prefix+"pm9"),
		depositAttempt(prefix+"q1", prefix+"pm9"),
		{"action": "BONUS_CLAIM", "playerId": prefix + "q1"},
	}
	reqs[0]["deviceId"] = d2
	reqs[3]["deviceId"] = d2
	reqs[4]["deviceId"] = d2
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = a.decide(t0, r).String()
	}
	return out
}

func TestOutOfOrderDeliveryMatchesInOrder(t *testing.T) {
	a := newApp(t)

	for _, e := range scenario("in-") {
		a.ingest(t0, e)
	}
	want := scenarioDecisions(a, "in-")
	expected := []string{
		"HOLD R2_CASHOUT_WITHOUT_PLAY R3_ACCOUNT_TAKEOVER R4_LINKED_ACCOUNTS R5_NEW_PAYOUT_METHOD",
		"HOLD R2_CASHOUT_WITHOUT_PLAY R3_ACCOUNT_TAKEOVER",
		"REVIEW R1_CARD_TESTING",
		"REVIEW R1_CARD_TESTING",
		"REVIEW R4_LINKED_ACCOUNTS",
	}
	if !slices.Equal(want, expected) {
		t.Fatalf("in-order decisions:\n got %q\nwant %q", want, expected)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	orders := map[string]func([]ev) []ev{
		"reversed": func(es []ev) []ev { slices.Reverse(es); return es },
	}
	for _, name := range []string{"shuffle1", "shuffle2", "shuffle3", "shuffle4"} {
		orders[name] = func(es []ev) []ev {
			rng.Shuffle(len(es), func(i, j int) { es[i], es[j] = es[j], es[i] })
			return es
		}
	}
	for name, reorder := range orders {
		t.Run(name, func(t *testing.T) {
			prefix := name + "-"
			for _, e := range reorder(scenario(prefix)) {
				a.ingest(t0, e)
			}
			if got := scenarioDecisions(a, prefix); !slices.Equal(got, want) {
				t.Fatalf("decisions:\n got %q\nwant %q", got, want)
			}
		})
	}
	t.Run("one reversed batch", func(t *testing.T) {
		es := scenario("batch-")
		slices.Reverse(es)
		if res := a.ingest(t0, es); res.Accepted != len(es) {
			t.Fatalf("got %+v", res)
		}
		if got := scenarioDecisions(a, "batch-"); !slices.Equal(got, want) {
			t.Fatalf("decisions:\n got %q\nwant %q", got, want)
		}
	})
	if late := a.metrics.LateEvents.Load(); late != 0 {
		t.Fatalf("late_events_total = %d, want 0 (all events within 5 minutes)", late)
	}
}

func TestLateEventIsAcceptedCountedAndApplied(t *testing.T) {
	a := newApp(t)
	now := t0
	res := a.ingest(now, []ev{
		{"eventId": "reset", "playerId": "p1", "type": "PASSWORD_RESET", "occurredAt": ts(now.Add(-10 * time.Minute))},
		{"eventId": "payout", "playerId": "p1", "type": "PAYOUT_METHOD_ADDED", "occurredAt": ts(now.Add(-5 * time.Minute)), "paymentMethodId": "pm1"},
		{"eventId": "login", "playerId": "p1", "type": "LOGIN", "occurredAt": ts(now.Add(-time.Minute)), "deviceId": "d1"},
	})
	if res.Accepted != 3 {
		t.Fatalf("got %+v, want 3 accepted", res)
	}
	// Only the reset is more than 5 minutes late; exactly 5 minutes is not late.
	if late := a.metrics.LateEvents.Load(); late != 1 {
		t.Fatalf("late_events_total = %d, want 1", late)
	}
	a.expectDecision(now, withdrawal("p1", "pm1"), "HOLD R3_ACCOUNT_TAKEOVER R5_NEW_PAYOUT_METHOD")
}

func TestLateEarlierSightingMakesDeviceNotNew(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, []ev{
		{"eventId": "reset", "playerId": "p1", "type": "PASSWORD_RESET", "occurredAt": ts(now.Add(-10 * time.Minute))},
		{"eventId": "payout", "playerId": "p1", "type": "PAYOUT_METHOD_ADDED", "occurredAt": ts(now.Add(-10 * time.Minute)), "paymentMethodId": "pm1"},
		{"eventId": "login", "playerId": "p1", "type": "LOGIN", "occurredAt": ts(now.Add(-time.Minute)), "deviceId": "d1"},
	})
	a.expectDecision(now, withdrawal("p1", "pm1"), "HOLD R3_ACCOUNT_TAKEOVER R5_NEW_PAYOUT_METHOD")

	// A very late event shows d1 was already used two hours ago.
	old := bet("old-bet", "p1", "1.00", now.Add(-2*time.Hour))
	old["deviceId"] = "d1"
	a.ingest(now, old)
	a.expectDecision(now, withdrawal("p1", "pm1"), "REVIEW R5_NEW_PAYOUT_METHOD")
}
