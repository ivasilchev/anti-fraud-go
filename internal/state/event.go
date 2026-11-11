// Package state holds the in-memory rule state, the dispatcher and the reducers
// that apply stored events to it.
package state

import "github.com/devastion/anti-fraud-go/internal/money"

// EventType is persisted as an integer in the event log. Values are stable; never renumber.
type EventType uint8

const (
	EventUnknown EventType = iota
	EventLogin
	EventPasswordReset
	EventDepositAttempt
	EventDepositApproved
	EventDepositDeclined
	EventBet
	EventWin
	EventBetVoided
	EventPayoutMethodAdded
	EventWithdrawalRequested
)

var eventTypeNames = [...]string{
	EventUnknown:             "",
	EventLogin:               "LOGIN",
	EventPasswordReset:       "PASSWORD_RESET",
	EventDepositAttempt:      "DEPOSIT_ATTEMPT",
	EventDepositApproved:     "DEPOSIT_APPROVED",
	EventDepositDeclined:     "DEPOSIT_DECLINED",
	EventBet:                 "BET",
	EventWin:                 "WIN",
	EventBetVoided:           "BET_VOIDED",
	EventPayoutMethodAdded:   "PAYOUT_METHOD_ADDED",
	EventWithdrawalRequested: "WITHDRAWAL_REQUESTED",
}

// String returns the wire name of the type, or "" for unknown values.
func (t EventType) String() string {
	if int(t) < len(eventTypeNames) {
		return eventTypeNames[t]
	}
	return ""
}

// ParseEventType maps a wire name to its EventType.
func ParseEventType(s string) (EventType, bool) {
	switch s {
	case "LOGIN":
		return EventLogin, true
	case "PASSWORD_RESET":
		return EventPasswordReset, true
	case "DEPOSIT_ATTEMPT":
		return EventDepositAttempt, true
	case "DEPOSIT_APPROVED":
		return EventDepositApproved, true
	case "DEPOSIT_DECLINED":
		return EventDepositDeclined, true
	case "BET":
		return EventBet, true
	case "WIN":
		return EventWin, true
	case "BET_VOIDED":
		return EventBetVoided, true
	case "PAYOUT_METHOD_ADDED":
		return EventPayoutMethodAdded, true
	case "WITHDRAWAL_REQUESTED":
		return EventWithdrawalRequested, true
	}
	return EventUnknown, false
}

// Event is a validated player activity event.
type Event struct {
	EventID    string
	PlayerID   string
	Type       EventType
	OccurredAt int64 // unix milliseconds

	// AmountE4 and AmountEURE4 are 10^-4 units; both are 0 when Currency is NoCurrency.
	AmountE4    int64
	AmountEURE4 int64
	Currency    money.Currency

	DeviceID        string
	IP              string
	PaymentMethodID string
	VoidedEventID   string

	// Log metadata. The reducers never read it. Zero means unset: the store
	// writes NULL and reads NULL back as zero (rows from before these columns).

	// UUID is a server-assigned UUIDv7, unique in the log.
	UUID [16]byte
	// ReceivedAt is when the HTTP handler received the request (unix ms,
	// X-Test-Now in test mode). CreatedAt is when the writer took the request
	// off the queue, and ProcessedAt is when its flush began, right before
	// the insert and the reducers (both unix ms, server clock).
	ReceivedAt  int64
	CreatedAt   int64
	ProcessedAt int64
}

// HashID is the 64-bit FNV-1a hash used to key bets in Player.RecentBets.
// It is stable across processes, so it is safe to persist in snapshots.
func HashID(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}
