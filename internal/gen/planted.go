package gen

import (
	"strconv"
	"time"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
)

const (
	r1 = "R1_CARD_TESTING"
	r2 = "R2_CASHOUT_WITHOUT_PLAY"
	r3 = "R3_ACCOUNT_TAKEOVER"
	r4 = "R4_LINKED_ACCOUNTS"
	r5 = "R5_NEW_PAYOUT_METHOD"
)

// Timing: R1 events are at most 60 s old and R3 events at most 50 min old at
// generation time, so their windows (10 and 60 min) stay live for about 9
// minutes after generation. Verify pins its clock to generation time + 1 min
// when the service runs with TEST_MODE=true.

// planter collects planted events and the manifest cases.
type planter struct {
	g      *generator
	events []state.Event
	cases  []Case
}

func (p *planter) add(player string, typ state.EventType, ago time.Duration, fill func(*state.Event)) string {
	ev := state.Event{EventID: p.g.nextID(), PlayerID: player, Type: typ, OccurredAt: p.g.now - ago.Milliseconds()}
	if fill != nil {
		fill(&ev)
	}
	p.events = append(p.events, ev)
	return ev.EventID
}

func withAmount(cents int64, c money.Currency) func(*state.Event) {
	return func(ev *state.Event) {
		ev.AmountE4 = cents * 100
		ev.AmountEURE4 = money.ToEUR(ev.AmountE4, c)
		ev.Currency = c
	}
}

func (p *planter) login(player, dev string, ago time.Duration) {
	p.add(player, state.EventLogin, ago, func(ev *state.Event) { ev.DeviceID = dev })
}

func (p *planter) decline(player, pm, dev string, ago time.Duration) {
	p.add(player, state.EventDepositDeclined, ago, func(ev *state.Event) {
		withAmount(1_000, money.EUR)(ev)
		ev.PaymentMethodID, ev.DeviceID = pm, dev
	})
}

func (p *planter) approve(player, pm string, cents int64, c money.Currency, ago time.Duration) {
	p.add(player, state.EventDepositApproved, ago, func(ev *state.Event) {
		withAmount(cents, c)(ev)
		ev.PaymentMethodID = pm
	})
}

func (p *planter) bet(player string, cents int64, c money.Currency, ago time.Duration) string {
	return p.add(player, state.EventBet, ago, withAmount(cents, c))
}

func (p *planter) voidBet(player, betID string, ago time.Duration) {
	p.add(player, state.EventBetVoided, ago, func(ev *state.Event) { ev.VoidedEventID = betID })
}

func (p *planter) reset(player string, ago time.Duration) {
	p.add(player, state.EventPasswordReset, ago, nil)
}

func (p *planter) payout(player, pm string, ago time.Duration) {
	p.add(player, state.EventPayoutMethodAdded, ago, func(ev *state.Event) { ev.PaymentMethodID = pm })
}

// played gives player an approved deposit long ago, fully turned over, so
// that neither R2 nor R5 (for pm) fires.
func (p *planter) played(player, pm string) {
	p.approve(player, pm, 5_000, money.EUR, 10*24*time.Hour)
	p.bet(player, 5_000, money.EUR, 10*24*time.Hour-time.Hour)
}

func (p *planter) expect(name, rule, kind, about string, req DecisionRequest, decision string, rules ...string) {
	if rules == nil {
		rules = []string{}
	}
	p.cases = append(p.cases, Case{Name: name, Rule: rule, Kind: kind, About: about, PlayerID: req.PlayerID,
		Request: req, Expected: Expected{Decision: decision, Rules: rules}})
}

func depositAttempt(player, pm, dev string) DecisionRequest {
	return DecisionRequest{Action: "DEPOSIT_ATTEMPT", PlayerID: player, Amount: "10.00", Currency: "EUR",
		PaymentMethodID: pm, DeviceID: dev}
}

func withdrawal(player, pm, dev string) DecisionRequest {
	return DecisionRequest{Action: "WITHDRAWAL", PlayerID: player, Amount: "20.00", Currency: "EUR",
		PaymentMethodID: pm, DeviceID: dev}
}

func bonusClaim(player, dev string) DecisionRequest {
	return DecisionRequest{Action: "BONUS_CLAIM", PlayerID: player, DeviceID: dev}
}

// declines plants n declines for player over the last minute, cycling through
// methods distinct payment methods.
func (p *planter) declines(player, dev string, n, methods int) {
	for k := range n {
		p.decline(player, player+"-pm"+strconv.Itoa(k%methods), dev, time.Duration(60-5*k)*time.Second)
	}
}

// plant builds every planted scenario. Each uses its own "pl-" IDs.
func plant(g *generator) ([]state.Event, *Manifest) {
	p := &planter{g: g}
	const sec, min, hr, dy = time.Second, time.Minute, time.Hour, 24 * time.Hour

	// R1: ≥5 declines with ≥3 distinct methods in the last 10 minutes, per
	// player or per decision device.
	p.declines("pl-r1-fire", "", 7, 4)
	p.expect("r1-fire", r1, "fires", "7 declines with 4 methods in the last minute",
		depositAttempt("pl-r1-fire", "pl-r1-fire-pm9", ""), "REVIEW", r1)
	p.declines("pl-r1-exact", "", 5, 3)
	p.expect("r1-exact", r1, "exact-threshold", "exactly 5 declines with exactly 3 methods",
		depositAttempt("pl-r1-exact", "pl-r1-exact-pm9", ""), "REVIEW", r1)
	p.declines("pl-r1-below", "", 4, 3)
	p.expect("r1-below", r1, "one-below", "4 declines with 3 methods",
		depositAttempt("pl-r1-below", "pl-r1-below-pm9", ""), "ALLOW")
	p.declines("pl-r1-below-methods", "", 5, 2)
	p.expect("r1-below-methods", r1, "one-below", "5 declines with only 2 methods",
		depositAttempt("pl-r1-below-methods", "pl-r1-below-methods-pm9", ""), "ALLOW")
	for k := range 6 {
		p.decline("pl-r1-stale", "pl-r1-stale-pm"+strconv.Itoa(k%3), "", 20*min-time.Duration(k)*min)
	}
	p.expect("r1-stale", r1, "edge", "6 declines with 3 methods, all 15–20 minutes old",
		depositAttempt("pl-r1-stale", "pl-r1-stale-pm9", ""), "ALLOW")
	for k := range 5 {
		player := "pl-r1-dev-" + strconv.Itoa(k)
		p.decline(player, "pl-r1-dev-pm"+strconv.Itoa(k%3), "dpl-r1-dev", time.Duration(60-5*k)*sec)
	}
	p.expect("r1-device-exact", r1, "exact-threshold",
		"a new player on a device with exactly 5 declines (5 players, 3 methods); device path only",
		depositAttempt("pl-r1-dev-new", "pl-r1-dev-new-pm", "dpl-r1-dev"), "REVIEW", r1)
	for k := range 4 {
		player := "pl-r1-devlow-" + strconv.Itoa(k)
		p.decline(player, "pl-r1-devlow-pm"+strconv.Itoa(k%3), "dpl-r1-devlow", time.Duration(60-5*k)*sec)
	}
	p.expect("r1-device-below", r1, "one-below", "a new player on a device with 4 declines",
		depositAttempt("pl-r1-devlow-new", "pl-r1-devlow-new-pm", "dpl-r1-devlow"), "ALLOW")

	// R2: turnover since the last approved deposit below its amount.
	p.approve("pl-r2-fire", "pl-r2-fire-pm", 10_000, money.EUR, 2*hr)
	p.bet("pl-r2-fire", 4_000, money.EUR, 90*min)
	p.expect("r2-fire", r2, "fires", "deposit 100.00 EUR, bets 40.00",
		withdrawal("pl-r2-fire", "pl-r2-fire-pm", ""), "HOLD", r2)
	p.approve("pl-r2-exact", "pl-r2-exact-pm", 10_000, money.EUR, 2*hr)
	p.bet("pl-r2-exact", 6_000, money.EUR, 90*min)
	p.bet("pl-r2-exact", 4_000, money.EUR, 80*min)
	p.expect("r2-exact", r2, "exact-threshold", "deposit 100.00 EUR, bets 60.00 + 40.00",
		withdrawal("pl-r2-exact", "pl-r2-exact-pm", ""), "ALLOW")
	p.approve("pl-r2-below", "pl-r2-below-pm", 10_000, money.EUR, 2*hr)
	p.bet("pl-r2-below", 6_000, money.EUR, 90*min)
	p.bet("pl-r2-below", 3_999, money.EUR, 80*min)
	p.expect("r2-below", r2, "one-below", "deposit 100.00 EUR, bets 99.99",
		withdrawal("pl-r2-below", "pl-r2-below-pm", ""), "HOLD", r2)
	p.approve("pl-r2-fx", "pl-r2-fx-pm", 11_700, money.USD, 2*hr) // 107.64 EUR
	p.bet("pl-r2-fx", 9_200, money.GBP, 90*min)                   // 107.64 EUR
	p.expect("r2-fx-exact", r2, "exact-threshold", "deposit 117.00 USD = bet 92.00 GBP = 107.64 EUR",
		withdrawal("pl-r2-fx", "pl-r2-fx-pm", ""), "ALLOW")
	p.approve("pl-r2-void", "pl-r2-void-pm", 10_000, money.EUR, 2*hr)
	p.bet("pl-r2-void", 6_000, money.EUR, 90*min)
	b := p.bet("pl-r2-void", 4_000, money.EUR, 80*min)
	p.voidBet("pl-r2-void", b, 79*min)
	p.expect("r2-void", r2, "edge", "deposit 100.00, bets 60.00 + 40.00, the 40.00 voided",
		withdrawal("pl-r2-void", "pl-r2-void-pm", ""), "HOLD", r2)
	p.bet("pl-r2-early", 10_000, money.EUR, 3*hr)
	p.approve("pl-r2-early", "pl-r2-early-pm", 10_000, money.EUR, 2*hr)
	p.expect("r2-bet-before-deposit", r2, "edge", "bet 100.00 an hour before the 100.00 deposit",
		withdrawal("pl-r2-early", "pl-r2-early-pm", ""), "HOLD", r2)
	p.bet("pl-r2-nodep", 1_000, money.EUR, 2*hr)
	p.expect("r2-no-deposit", r2, "edge", "no approved deposit: R2 cannot fire (R5 does)",
		withdrawal("pl-r2-nodep", "pl-r2-nodep-pm", ""), "REVIEW", r5)

	// R3: password reset, payout method added and a new device (first seen
	// via LOGIN), all in the last 60 minutes.
	for _, c := range []struct {
		name, kind, about       string
		reset, payout, newLogin time.Duration // 0 = absent
		deviceSeenBefore        bool
		decision                string
	}{
		{"r3-fire", "fires", "reset 30m, payout 20m, new-device login 10m ago", 30 * min, 20 * min, 10 * min, false, "HOLD"},
		{"r3-exact", "exact-threshold", "the three components once each, the oldest 50 minutes ago", 50 * min, 45 * min, 40 * min, false, "HOLD"},
		{"r3-below", "one-below", "reset and payout, but the device was first seen 2 hours ago", 30 * min, 20 * min, 10 * min, true, "ALLOW"},
		{"r3-no-reset", "one-below", "payout and new device, no password reset", 0, 20 * min, 10 * min, false, "ALLOW"},
	} {
		player, dev := "pl-"+c.name, "dpl-"+c.name
		p.played(player, player+"-pm")
		p.login(player, "dpl-"+c.name+"-old", 10*dy)
		if c.reset > 0 {
			p.reset(player, c.reset)
		}
		p.payout(player, player+"-payout", c.payout)
		if c.deviceSeenBefore {
			p.add(player, state.EventBet, 2*hr, func(ev *state.Event) { withAmount(100, money.EUR)(ev); ev.DeviceID = dev })
		}
		p.login(player, dev, c.newLogin)
		rules := []string{}
		if c.decision == "HOLD" {
			rules = []string{r3}
		}
		p.expect(c.name, r3, c.kind, c.about, withdrawal(player, player+"-pm", ""), c.decision, rules...)
	}

	// R4: ≥3 distinct players seen on the decision's device in the last 30 days.
	for _, c := range []struct {
		name, kind, about string
		ages              []time.Duration
		decision          string
	}{
		{"r4-fire", "fires", "4 players on the device within 30 days", []time.Duration{dy, 2 * dy, 5 * dy, 20 * dy}, "REVIEW"},
		{"r4-exact", "exact-threshold", "exactly 3 players on the device within 30 days", []time.Duration{dy, 2 * dy, 5 * dy}, "REVIEW"},
		{"r4-below", "one-below", "2 players on the device", []time.Duration{dy, 2 * dy}, "ALLOW"},
		{"r4-stale", "edge", "3 players, but one last seen 31 days ago", []time.Duration{dy, 2 * dy, 31 * dy}, "ALLOW"},
	} {
		dev := "dpl-" + c.name
		for k, age := range c.ages {
			p.login("pl-"+c.name+"-"+strconv.Itoa(k), dev, age)
		}
		rules := []string{}
		if c.decision == "REVIEW" {
			rules = []string{r4}
		}
		p.expect(c.name, r4, c.kind, c.about, bonusClaim("pl-"+c.name+"-0", dev), c.decision, rules...)
	}

	// R5: the withdrawal's method was never used in an approved deposit.
	p.played("pl-r5", "pl-r5-pm-ok")
	p.decline("pl-r5", "pl-r5-pm-declined", "", 3*dy)
	p.expect("r5-fire", r5, "fires", "method used only in a declined deposit",
		withdrawal("pl-r5", "pl-r5-pm-declined", ""), "REVIEW", r5)
	p.expect("r5-approved", r5, "one-below", "method used in an approved deposit",
		withdrawal("pl-r5", "pl-r5-pm-ok", ""), "ALLOW")
	p.expect("r5-never-seen", r5, "edge", "method never seen at all",
		withdrawal("pl-r5", "pl-r5-pm-new", ""), "REVIEW", r5)
	p.expect("r5-unknown-player", r5, "edge", "player with no events",
		withdrawal("pl-r5-nobody", "pl-r5-pm-ok", ""), "REVIEW", r5)

	// Several rules at once.
	p.approve("pl-multi", "pl-multi-pm", 10_000, money.EUR, dy) // no bets: R2
	p.reset("pl-multi", 30*min)
	p.payout("pl-multi", "pl-multi-payout", 20*min)
	p.login("pl-multi", "dpl-multi", 10*min) // new device: R3
	p.login("pl-multi-friend-0", "dpl-multi", dy)
	p.login("pl-multi-friend-1", "dpl-multi", 2*dy) // 3 players on the device: R4
	p.expect("multi-hold", "MULTI", "fires", "R2, R3, R4 and R5 together; HOLD wins",
		withdrawal("pl-multi", "pl-multi-payout", "dpl-multi"), "HOLD", r2, r3, r4, r5)
	p.played("pl-multi2", "pl-multi2-pm")
	for k := range 3 {
		p.login("pl-multi2-"+strconv.Itoa(k), "dpl-multi2", time.Duration(k+1)*dy)
	}
	p.login("pl-multi2", "dpl-multi2", 3*hr)
	p.expect("multi-review", "MULTI", "fires", "R4 and R5 together: REVIEW, both listed",
		withdrawal("pl-multi2", "pl-multi2-new", "dpl-multi2"), "REVIEW", r4, r5)

	m := &Manifest{
		GeneratedAt: g.cfg.Now.UTC().Truncate(time.Millisecond),
		Seed:        g.cfg.Seed,
		Players:     g.cfg.Players,
		Events:      g.cfg.Events,
		Days:        g.cfg.Days,
		IDFormats: map[string]string{
			"player": "p%07d, 1..players", "device": "d%07d, 1..devices (1..players are every player's primary device)",
			"paymentMethod": "pm%07d", "event": "g-%08d",
		},
		Cases: p.cases,
	}
	return p.events, m
}
