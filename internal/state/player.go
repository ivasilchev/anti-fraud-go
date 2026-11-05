package state

import (
	"math"
	"slices"
)

// Retention windows, in milliseconds, for pruned per-entity slices.
const (
	RecentBetsRetentionMs    = 6 * 60 * 1000
	DeclinesRetentionMs      = 15 * 60 * 1000
	DevicePlayersRetentionMs = 30 * 24 * 60 * 60 * 1000
)

// Bet is a recent bet kept for turnover recomputation and void matching.
type Bet struct {
	IDHash    uint64
	T         int64
	AmountEUR int64
	Voided    bool
}

// Decline is a declined deposit.
type Decline struct {
	T  int64
	PM uint32
}

// PlayerDevice records when a player first used a device.
type PlayerDevice struct {
	Device           uint32
	FirstSeenT       int64
	FirstSeenIsLogin bool
}

// Player is the rule state for one player. Timestamps are unix ms; 0 means never.
// The zero value is a player with no history.
type Player struct {
	LastDepositT   int64
	LastDepositEUR int64
	LastDepositSeq int64 // 0 means no approved deposit
	// TurnoverEUR is the sum of non-voided bets with T >= LastDepositT.
	TurnoverEUR int64

	LastPasswordResetT     int64
	LastPayoutMethodAddedT int64

	RecentBets  []Bet
	Declines    []Decline
	Devices     []PlayerDevice
	ApprovedPMs []uint32 // sorted ascending, unique
}

// HasDeposit reports whether the player has an approved deposit.
func (p *Player) HasDeposit() bool { return p.LastDepositSeq > 0 }

// HasApprovedPM reports whether pm was used in an approved deposit.
func (p *Player) HasApprovedPM(pm uint32) bool {
	_, ok := slices.BinarySearch(p.ApprovedPMs, pm)
	return ok
}

// SeeDevice records any event carrying a deviceId. The earliest sighting wins;
// on a tie, a LOGIN marks the sighting as a login.
func (p *Player) SeeDevice(dev uint32, t int64, isLogin bool) {
	for i := range p.Devices {
		d := &p.Devices[i]
		if d.Device != dev {
			continue
		}
		switch {
		case t < d.FirstSeenT:
			d.FirstSeenT, d.FirstSeenIsLogin = t, isLogin
		case t == d.FirstSeenT && isLogin:
			d.FirstSeenIsLogin = true
		}
		return
	}
	p.Devices = append(p.Devices, PlayerDevice{Device: dev, FirstSeenT: t, FirstSeenIsLogin: isLogin})
}

// AddDecline records a DEPOSIT_DECLINED, pruning declines older than
// DeclinesRetentionMs relative to now.
func (p *Player) AddDecline(t int64, pm uint32, now int64) {
	p.Declines = append(pruneDeclines(p.Declines, now-DeclinesRetentionMs), Decline{T: t, PM: pm})
}

// ApplyDepositApproved records an approved deposit. If (t, seq) is newer than
// the current last deposit, it becomes the last deposit and turnover is
// recomputed from RecentBets.
func (p *Player) ApplyDepositApproved(t, seq, amountEUR int64, pm uint32) {
	if i, ok := slices.BinarySearch(p.ApprovedPMs, pm); !ok {
		p.ApprovedPMs = slices.Insert(p.ApprovedPMs, i, pm)
	}
	if p.HasDeposit() && (t < p.LastDepositT || (t == p.LastDepositT && seq <= p.LastDepositSeq)) {
		return
	}
	p.LastDepositT, p.LastDepositEUR, p.LastDepositSeq = t, amountEUR, seq
	var sum int64
	for _, b := range p.RecentBets {
		if !b.Voided && b.T >= t {
			sum = addSat(sum, b.AmountEUR)
		}
	}
	p.TurnoverEUR = sum
}

// ApplyBet records a BET. voided is true when a void for this bet arrived
// first; such a bet is stored but never counted as turnover.
func (p *Player) ApplyBet(idHash uint64, t, amountEUR int64, voided bool, now int64) {
	if !voided && t >= p.LastDepositT {
		p.TurnoverEUR = addSat(p.TurnoverEUR, amountEUR)
	}
	p.RecentBets = append(pruneBets(p.RecentBets, now-RecentBetsRetentionMs),
		Bet{IDHash: idHash, T: t, AmountEUR: amountEUR, Voided: voided})
}

// ApplyVoid marks the bet with idHash as voided and removes it from turnover
// if it was counted. It returns false when the bet is not in RecentBets.
func (p *Player) ApplyVoid(idHash uint64) bool {
	for i := range p.RecentBets {
		b := &p.RecentBets[i]
		if b.IDHash != idHash {
			continue
		}
		if !b.Voided {
			b.Voided = true
			if b.T >= p.LastDepositT {
				p.TurnoverEUR -= b.AmountEUR
			}
		}
		return true
	}
	return false
}

// ApplyVoidOutOfWindow handles a void whose bet (at t, amountEUR) is no longer
// in RecentBets: it is removed from turnover if it was counted.
func (p *Player) ApplyVoidOutOfWindow(t, amountEUR int64) {
	if t >= p.LastDepositT {
		p.TurnoverEUR -= amountEUR
	}
}

// FindBet returns the bet with idHash from RecentBets.
func (p *Player) FindBet(idHash uint64) (Bet, bool) {
	for _, b := range p.RecentBets {
		if b.IDHash == idHash {
			return b, true
		}
	}
	return Bet{}, false
}

// ApplyPasswordReset records a PASSWORD_RESET (max-of).
func (p *Player) ApplyPasswordReset(t int64) {
	p.LastPasswordResetT = max(p.LastPasswordResetT, t)
}

// ApplyPayoutMethodAdded records a PAYOUT_METHOD_ADDED (max-of).
func (p *Player) ApplyPayoutMethodAdded(t int64) {
	p.LastPayoutMethodAddedT = max(p.LastPayoutMethodAddedT, t)
}

// addSat adds a non-negative amount, saturating at math.MaxInt64 so that huge
// turnovers (above ~9.2e14 EUR) can never wrap negative and fire R2.
func addSat(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func pruneDeclines(s []Decline, cutoff int64) []Decline {
	return slices.DeleteFunc(s, func(d Decline) bool { return d.T < cutoff })
}

func pruneBets(s []Bet, cutoff int64) []Bet {
	return slices.DeleteFunc(s, func(b Bet) bool { return b.T < cutoff })
}
