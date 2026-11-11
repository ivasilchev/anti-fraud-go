// Package store defines the durable event log. Callers depend only on the
// Store interface; the storage engine lives in a subpackage (store/sqlite).
package store

import (
	"context"

	"github.com/devastion/anti-fraud-go/internal/state"
)

// Store is the durable, de-duplicating event log. Every event gets a seq when
// it is stored. Seqs increase in insertion order and are never reused while
// the log exists. The log is the source of truth: memory is rebuilt from it.
//
// Implementations must be safe for one writer plus concurrent readers, and
// must allow LookupBets to be called from inside a StreamEvents callback.
type Store interface {
	// InsertEvents stores events atomically and durably, skipping any event
	// whose eventId is already stored and any second BET_VOIDED for the same
	// voidedEventId. Within one call, the first occurrence of a duplicated
	// eventId is the one stored. It returns the stored rows, in any order.
	InsertEvents(ctx context.Context, events []state.Event) ([]Inserted, error)

	// LookupBets returns the stored BET events among ids, keyed by eventId.
	LookupBets(ctx context.Context, ids []string) (map[string]state.BetRef, error)

	// StreamEvents calls fn for every event with seq > afterSeq, in seq
	// order, without holding all of them in memory. ev is reused between
	// calls; fn must copy it to keep it. An error from fn stops the stream
	// and is returned.
	StreamEvents(ctx context.Context, afterSeq int64, fn func(seq int64, ev *state.Event) error) error

	// WatermarkAt returns the event stored at seq, or ok == false if there
	// is none.
	WatermarkAt(ctx context.Context, seq int64) (mark state.Watermark, ok bool, err error)

	// Import replaces the whole log with the events that fill passes to add,
	// numbered from seq 1 in the order given. It is for bulk loads while no
	// writer is running. It returns the number of events stored.
	Import(ctx context.Context, fill func(add func(*state.Event) error) error) (int64, error)

	// CountEvents counts the stored events whose eventId starts with prefix
	// ("" counts all of them).
	CountEvents(ctx context.Context, prefix string) (int64, error)

	Close() error
}

// Inserted is a row that was new to the log.
type Inserted struct {
	Seq     int64
	EventID string
}
