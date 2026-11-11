package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store/sqlite"
)

// oldSchema is the events table before the uuid and timestamp columns.
const oldSchema = `
CREATE TABLE events (
  seq INTEGER PRIMARY KEY, event_id TEXT NOT NULL, player_id TEXT NOT NULL,
  type INTEGER NOT NULL, occurred_at INTEGER NOT NULL, amount_e4 INTEGER,
  amount_eur_e4 INTEGER, currency TEXT, device_id TEXT, ip TEXT,
  payment_method_id TEXT, voided_event_id TEXT
) STRICT;
CREATE UNIQUE INDEX events_event_id ON events (event_id);
INSERT INTO events (event_id, player_id, type, occurred_at) VALUES ('old1', 'p1', 1, 1000), ('old2', 'p1', 1, 2000);`

func TestOpenMigratesOldSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "events.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, oldSchema); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	s, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fresh := state.Event{EventID: "new1", PlayerID: "p1", Type: state.EventLogin, OccurredAt: 3000,
		UUID: [16]byte{0: 1, 15: 9}, ReceivedAt: 3001, CreatedAt: 3002, ProcessedAt: 3003}
	again := fresh
	again.EventID = "new2" // same uuid
	in, err := s.InsertEvents(ctx, []state.Event{fresh, again})
	if err != nil || len(in) != 1 || in[0].EventID != "new1" {
		t.Fatalf("insert after migration = %+v, %v; want only new1", in, err)
	}

	var got []state.Event
	if err := s.StreamEvents(ctx, 0, func(_ int64, ev *state.Event) error {
		got = append(got, *ev)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("streamed %d events, want 3", len(got))
	}
	for _, old := range got[:2] {
		if old.UUID != ([16]byte{}) || old.ReceivedAt != 0 || old.CreatedAt != 0 || old.ProcessedAt != 0 {
			t.Errorf("old row %s has metadata %+v, want zero", old.EventID, old)
		}
	}
	if got[2] != fresh {
		t.Errorf("new row = %+v, want %+v", got[2], fresh)
	}

	// Opening the migrated file again changes nothing.
	s2, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
}
