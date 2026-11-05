// Package rules is the pure decision engine. Evaluate does no I/O and does not allocate.
package rules

import "github.com/devastion/anti-fraud-go/internal/state"

// Action is the payment action being decided.
type Action uint8

const (
	ActionUnknown Action = iota
	DepositAttempt
	Withdrawal
	BonusClaim
)

// ParseAction maps a wire name to its Action.
func ParseAction(s string) (Action, bool) {
	switch s {
	case "DEPOSIT_ATTEMPT":
		return DepositAttempt, true
	case "WITHDRAWAL":
		return Withdrawal, true
	case "BONUS_CLAIM":
		return BonusClaim, true
	}
	return ActionUnknown, false
}

// Outcome is the decision. Higher values are stricter.
type Outcome uint8

const (
	Allow Outcome = iota
	Review
	Hold
)

func (o Outcome) String() string {
	switch o {
	case Review:
		return "REVIEW"
	case Hold:
		return "HOLD"
	}
	return "ALLOW"
}

// Rule identifies a fraud rule. Values follow rule order.
type Rule uint8

const (
	R1CardTesting Rule = iota + 1
	R2CashoutWithoutPlay
	R3AccountTakeover
	R4LinkedAccounts
	R5NewPayoutMethod
)

func (r Rule) String() string {
	switch r {
	case R1CardTesting:
		return "R1_CARD_TESTING"
	case R2CashoutWithoutPlay:
		return "R2_CASHOUT_WITHOUT_PLAY"
	case R3AccountTakeover:
		return "R3_ACCOUNT_TAKEOVER"
	case R4LinkedAccounts:
		return "R4_LINKED_ACCOUNTS"
	case R5NewPayoutMethod:
		return "R5_NEW_PAYOUT_METHOD"
	}
	return ""
}

// Outcome returns the decision the rule gives when it fires.
func (r Rule) Outcome() Outcome {
	switch r {
	case R2CashoutWithoutPlay, R3AccountTakeover:
		return Hold
	case R1CardTesting, R4LinkedAccounts, R5NewPayoutMethod:
		return Review
	}
	return Allow
}

// Rule thresholds. Windows are in milliseconds and inclusive: an event at t
// counts when now-W <= t <= now.
const (
	CardTestingWindowMs    = 10 * 60 * 1000
	CardTestingMinDeclines = 5
	CardTestingMinMethods  = 3
	TakeoverWindowMs       = 60 * 60 * 1000
	LinkedWindowMs         = 30 * 24 * 60 * 60 * 1000
	LinkedMinPlayers       = 3
)

// Request is a validated decision request with IDs resolved to interned indexes.
// Player and device state are passed to Evaluate separately.
type Request struct {
	Action Action
	// PaymentMethod is meaningful only when PaymentMethodKnown is true.
	// An unknown method has never been seen in any event.
	PaymentMethod      uint32
	PaymentMethodKnown bool
}

// Reason describes one fired rule. Only the fields of its Rule are set.
type Reason struct {
	Rule Rule

	// R1: counts for each path that fired; zero for a path that did not.
	PlayerDeclines, PlayerMethods int32
	DeviceDeclines, DeviceMethods int32

	// R2: amounts in 10^-4 EUR.
	TurnoverEUR, DepositEUR int64

	// R4: distinct players seen on the device in the window.
	LinkedPlayers int32
}

const ruleCount = 5

// Decision is the engine result. The zero value is ALLOW with no reasons.
type Decision struct {
	Outcome Outcome
	reasons [ruleCount]Reason
	n       uint8
}

// Reasons returns the fired rules in rule order.
func (d *Decision) Reasons() []Reason { return d.reasons[:d.n] }

func (d *Decision) fire(r Reason) {
	d.reasons[d.n] = r
	d.n++
	d.Outcome = max(d.Outcome, r.Rule.Outcome())
}

// Evaluate applies the rules for req.Action. p is nil for an unknown player;
// d is nil when the request has no deviceId or the device is unknown.
// now is unix milliseconds.
func Evaluate(req Request, p *state.Player, d *state.Device, now int64) Decision {
	var dec Decision
	switch req.Action {
	case DepositAttempt:
		cardTesting(&dec, p, d, now)
	case Withdrawal:
		cashoutWithoutPlay(&dec, p)
		accountTakeover(&dec, p, now)
		linkedAccounts(&dec, d, now)
		newPayoutMethod(&dec, req, p)
	case BonusClaim:
		linkedAccounts(&dec, d, now)
	}
	return dec
}

func inWindow(t, now, w int64) bool { return now-w <= t && t <= now }

// cardTesting is R1: >= 5 declines with >= 3 distinct payment methods within
// 10 minutes, on the player or (separately) on the request's device.
func cardTesting(dec *Decision, p *state.Player, d *state.Device, now int64) {
	r := Reason{Rule: R1CardTesting}
	fired := false
	if p != nil {
		if n, m := countDeclines(p.Declines, now); n >= CardTestingMinDeclines && m >= CardTestingMinMethods {
			r.PlayerDeclines, r.PlayerMethods = n, m
			fired = true
		}
	}
	if d != nil {
		if n, m := countDeclines(d.Declines, now); n >= CardTestingMinDeclines && m >= CardTestingMinMethods {
			r.DeviceDeclines, r.DeviceMethods = n, m
			fired = true
		}
	}
	if fired {
		dec.fire(r)
	}
}

// countDeclines returns the number of declines in the R1 window and the number
// of distinct payment methods among them. Decline lists are short (pruned to
// 15 minutes), so the quadratic distinct count is cheap and allocation-free.
func countDeclines(ds []state.Decline, now int64) (n, methods int32) {
	for i, x := range ds {
		if !inWindow(x.T, now, CardTestingWindowMs) {
			continue
		}
		n++
		seen := false
		for _, y := range ds[:i] {
			if y.PM == x.PM && inWindow(y.T, now, CardTestingWindowMs) {
				seen = true
				break
			}
		}
		if !seen {
			methods++
		}
	}
	return n, methods
}

// cashoutWithoutPlay is R2: turnover since the last approved deposit is below
// that deposit's amount.
func cashoutWithoutPlay(dec *Decision, p *state.Player) {
	if p == nil || !p.HasDeposit() || p.TurnoverEUR >= p.LastDepositEUR {
		return
	}
	dec.fire(Reason{Rule: R2CashoutWithoutPlay, TurnoverEUR: p.TurnoverEUR, DepositEUR: p.LastDepositEUR})
}

// accountTakeover is R3: password reset, payout method added and a device first
// seen via LOGIN, all within the last 60 minutes, in any order.
func accountTakeover(dec *Decision, p *state.Player, now int64) {
	if p == nil ||
		!inWindow(p.LastPasswordResetT, now, TakeoverWindowMs) ||
		!inWindow(p.LastPayoutMethodAddedT, now, TakeoverWindowMs) {
		return
	}
	for _, dev := range p.Devices {
		if dev.FirstSeenIsLogin && inWindow(dev.FirstSeenT, now, TakeoverWindowMs) {
			dec.fire(Reason{Rule: R3AccountTakeover})
			return
		}
	}
}

// linkedAccounts is R4: the request's device was used by >= 3 distinct players
// within the last 30 days.
func linkedAccounts(dec *Decision, d *state.Device, now int64) {
	if d == nil {
		return
	}
	var n int32
	for _, e := range d.Players {
		if inWindow(e.LastSeenT, now, LinkedWindowMs) {
			n++
		}
	}
	if n >= LinkedMinPlayers {
		dec.fire(Reason{Rule: R4LinkedAccounts, LinkedPlayers: n})
	}
}

// newPayoutMethod is R5: the withdrawal's payment method was never used in an
// approved deposit. Fires for unknown players and unknown methods.
func newPayoutMethod(dec *Decision, req Request, p *state.Player) {
	if p != nil && req.PaymentMethodKnown && p.HasApprovedPM(req.PaymentMethod) {
		return
	}
	dec.fire(Reason{Rule: R5NewPayoutMethod})
}
