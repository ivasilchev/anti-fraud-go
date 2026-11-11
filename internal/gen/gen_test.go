package gen

import (
	"encoding/json"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/api"
	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/rules"
	"github.com/devastion/anti-fraud-go/internal/state"
)

var testNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

func smallConfig() Config {
	return Config{Players: 2_000, Events: 30_000, Days: 90, Seed: 7, Now: testNow}
}

func collect(t *testing.T, cfg Config) ([]state.Event, *Manifest) {
	t.Helper()
	var out []state.Event
	m, err := Generate(cfg, func(ev *state.Event) error { out = append(out, *ev); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return out, m
}

func fingerprint(events []state.Event) uint64 {
	h := fnv.New64a()
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		h.Write(b)
	}
	return h.Sum64()
}

func TestGenerateIsExactSortedUniqueAndDeterministic(t *testing.T) {
	cfg := smallConfig()
	events, m := collect(t, cfg)
	if len(events) != cfg.Events {
		t.Fatalf("%d events, want %d", len(events), cfg.Events)
	}
	ids := make(map[string]bool, len(events))
	now := cfg.Now.UnixMilli()
	for i, ev := range events {
		if i > 0 && ev.OccurredAt < events[i-1].OccurredAt {
			t.Fatalf("event %d (%s) is out of order", i, ev.EventID)
		}
		if ev.OccurredAt > now || ev.OccurredAt < now-int64(cfg.Days)*day-hour {
			t.Fatalf("event %s at %v is outside the %d-day range", ev.EventID, time.UnixMilli(ev.OccurredAt), cfg.Days)
		}
		if ids[ev.EventID] {
			t.Fatalf("duplicate event id %s", ev.EventID)
		}
		ids[ev.EventID] = true
	}
	if len(m.Cases) < 15 {
		t.Fatalf("only %d planted cases", len(m.Cases))
	}

	again, _ := collect(t, cfg)
	if fingerprint(again) != fingerprint(events) {
		t.Fatal("same config gave different events")
	}
	cfg.Seed++
	other, _ := collect(t, cfg)
	if fingerprint(other) == fingerprint(events) {
		t.Fatal("different seed gave the same events")
	}
}

func TestIDFormats(t *testing.T) {
	for got, want := range map[string]string{
		PlayerID(0): "p0000001", PlayerID(999_999): "p1000000", DeviceID(41): "d0000042",
		MethodID(0): "pm0000001", EventID(12): "g-00000012", EventID(123_456_789): "g-123456789",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}

// wire renders an event as a POST /events JSON object.
func wire(ev *state.Event) map[string]any {
	o := map[string]any{"eventId": ev.EventID, "playerId": ev.PlayerID, "type": ev.Type.String(),
		"occurredAt": time.UnixMilli(ev.OccurredAt).UTC().Format(time.RFC3339Nano)}
	if ev.Currency != money.NoCurrency {
		o["amount"] = string(money.AppendE4(nil, ev.AmountE4))
		o["currency"] = ev.Currency.String()
	}
	for k, v := range map[string]string{"deviceId": ev.DeviceID, "ip": ev.IP,
		"paymentMethodId": ev.PaymentMethodID, "voidedEventId": ev.VoidedEventID} {
		if v != "" {
			o[k] = v
		}
	}
	return o
}

func TestEveryEventPassesAPIValidation(t *testing.T) {
	cfg := smallConfig()
	events, _ := collect(t, cfg)
	for start := 0; start < len(events); start += api.MaxBatch {
		batch := events[start:min(start+api.MaxBatch, len(events))]
		objs := make([]map[string]any, len(batch))
		for i := range batch {
			objs[i] = wire(&batch[i])
		}
		body, err := json.Marshal(objs)
		if err != nil {
			t.Fatal(err)
		}
		parsed, apiErr := api.ParseEvents(body, cfg.Now)
		if apiErr != nil {
			t.Fatalf("batch at %d rejected: %s %+v", start, apiErr.Message, apiErr.Details)
		}
		for i := range parsed {
			if parsed[i] != batch[i] {
				t.Fatalf("event %s does not round-trip:\n got %+v\nwant %+v", batch[i].EventID, parsed[i], batch[i])
			}
		}
	}
}

func TestBackgroundShape(t *testing.T) {
	cfg := smallConfig()
	events, m := collect(t, cfg)
	now := cfg.Now.UnixMilli()

	perPlayer := map[string]int{}
	devices := map[string]map[string]bool{}       // player -> devices
	recentOnDevice := map[string]map[string]bool{} // device -> players in the last 30 days
	methods := map[string]map[string]bool{}
	types := map[state.EventType]int{}
	currencies := map[money.Currency]int{}
	bets := map[string]string{} // bet id -> player
	for _, ev := range events {
		if strings.HasPrefix(ev.PlayerID, "pl-") {
			continue
		}
		perPlayer[ev.PlayerID]++
		types[ev.Type]++
		if ev.Currency != money.NoCurrency {
			currencies[ev.Currency]++
		}
		add := func(m map[string]map[string]bool, k, v string) {
			if m[k] == nil {
				m[k] = map[string]bool{}
			}
			m[k][v] = true
		}
		if ev.DeviceID != "" {
			add(devices, ev.PlayerID, ev.DeviceID)
			if ev.OccurredAt >= now-30*day {
				add(recentOnDevice, ev.DeviceID, ev.PlayerID)
			}
		}
		if ev.PaymentMethodID != "" {
			add(methods, ev.PlayerID, ev.PaymentMethodID)
		}
		switch ev.Type {
		case state.EventBet:
			bets[ev.EventID] = ev.PlayerID
		case state.EventBetVoided:
			if bets[ev.VoidedEventID] != ev.PlayerID {
				t.Fatalf("void %s references %s, not an earlier bet of %s", ev.EventID, ev.VoidedEventID, ev.PlayerID)
			}
		}
	}
	if len(perPlayer) != cfg.Players {
		t.Fatalf("%d players have events, want all %d", len(perPlayer), cfg.Players)
	}

	// Zipf: the busiest 1% of players produce far more than 1% of events.
	counts := slices.Sorted(func(yield func(int) bool) {
		for _, n := range perPlayer {
			if !yield(n) {
				return
			}
		}
	})
	slices.Reverse(counts)
	top, total := 0, 0
	for i, n := range counts {
		if i < len(counts)/100 {
			top += n
		}
		total += n
	}
	if share := float64(top) / float64(total); share < 0.15 {
		t.Errorf("top 1%% of players have %.1f%% of events; want a heavy head", 100*share)
	}

	// Devices: own devices are 1–2 per player; shared ones have 3–6 recent players.
	shared := 0
	for dev, players := range recentOnDevice {
		idx, _ := strconv.Atoi(dev[1:])
		if idx > 2*cfg.Players {
			shared++
			if len(players) < 3 || len(players) > 6 {
				t.Errorf("shared device %s has %d recent players, want 3–6", dev, len(players))
			}
		} else if len(players) != 1 {
			t.Errorf("own device %s has %d players", dev, len(players))
		}
	}
	if shared != m.SharedDevices || shared == 0 {
		t.Errorf("%d shared devices in use, manifest says %d", shared, m.SharedDevices)
	}
	used := map[string]bool{}
	for p, ds := range devices {
		own := 0
		for d := range ds {
			used[d] = true
			if idx, _ := strconv.Atoi(d[1:]); idx <= 2*cfg.Players {
				own++
			}
		}
		if own < 1 || own > 2 {
			t.Errorf("player %s uses %d own devices", p, own)
		}
	}
	if pct := 100 * float64(shared) / float64(len(used)); pct < 1.5 || pct > 2.5 {
		t.Errorf("%.2f%% of used devices are shared, want about 2%%", pct)
	}
	for p, ms := range methods {
		if len(ms) > 3 {
			t.Errorf("player %s uses %d payment methods", p, len(ms))
		}
	}
	for _, c := range []money.Currency{money.EUR, money.USD, money.GBP} {
		if currencies[c] == 0 {
			t.Errorf("no %s amounts", c)
		}
	}
	for typ := state.EventLogin; typ <= state.EventWithdrawalRequested; typ++ {
		if typ != state.EventWin && typ != state.EventDepositAttempt && types[typ] == 0 {
			t.Errorf("no %s events", typ)
		}
	}
	t.Logf("types: %v", types)
}

// TestPlantedCasesHoldAgainstTheRuleEngine applies the whole data set to an
// in-memory state, as the service's replay does, and evaluates every case.
func TestPlantedCasesHoldAgainstTheRuleEngine(t *testing.T) {
	cfg := smallConfig()
	events, m := collect(t, cfg)
	st := state.New()
	actions := make([]state.Action, len(events))
	for i := range events {
		actions[i] = state.Action{Seq: int64(i + 1), Event: &events[i]}
	}
	refs, missing := st.PrepareVoids(actions)
	if len(missing) > 0 {
		t.Fatalf("voids reference unknown bets: %v", missing)
	}
	st.Reduce(&state.Batch{Actions: actions, Bets: refs})

	now := m.GeneratedAt.Add(TestNowOffset).UnixMilli()
	for _, c := range m.Cases {
		action, ok := rules.ParseAction(c.Request.Action)
		if !ok {
			t.Fatalf("%s: bad action %q", c.Name, c.Request.Action)
		}
		v := st.Acquire(c.Request.PlayerID, c.Request.DeviceID, c.Request.PaymentMethodID)
		dec := rules.Evaluate(rules.Request{Action: action, PaymentMethod: v.PM, PaymentMethodKnown: v.PMKnown}, v.P, v.D, now)
		v.Release()
		got := Expected{Decision: dec.Outcome.String(), Rules: []string{}}
		for _, r := range dec.Reasons() {
			got.Rules = append(got.Rules, r.Rule.String())
		}
		if got.Decision != c.Expected.Decision || !slices.Equal(got.Rules, c.Expected.Rules) {
			t.Errorf("%s (%s): got %v, want %v", c.Name, c.About, got, c.Expected)
		}
	}
}

func TestTooFewEventsIsAnError(t *testing.T) {
	cfg := smallConfig()
	cfg.Events = 3 * cfg.Players
	if _, err := Generate(cfg, func(*state.Event) error { return nil }); err == nil {
		t.Fatal("want an error: planted events leave fewer than 3 per player")
	}
}
