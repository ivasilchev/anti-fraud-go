package integration

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestEventsGetUUIDAndTimestamps checks that POST /events returns the uuid of
// each stored event, and that the row holds that uuid and the three times.
func TestEventsGetUUIDAndTimestamps(t *testing.T) {
	a := newApp(t)
	now := t0
	before := time.Now().UnixMilli()
	var res struct {
		Accepted   int `json:"accepted"`
		Duplicates int `json:"duplicates"`
		Events     []struct {
			EventID string `json:"eventId"`
			UUID    string `json:"uuid"`
		} `json:"events"`
	}
	batch := []ev{
		decline("m1", "p1", "pm1", now.Add(-time.Minute)),
		decline("m1", "p1", "pm2", now.Add(-time.Minute)), // duplicate in the batch
		decline("m2", "p1", "pm2", now.Add(-time.Minute)),
	}
	if code := a.post("/events", now, batch, &res); code != http.StatusOK {
		t.Fatalf("POST /events = %d", code)
	}
	if res.Accepted != 2 || res.Duplicates != 1 || len(res.Events) != 2 ||
		res.Events[0].EventID != "m1" || res.Events[1].EventID != "m2" {
		t.Fatalf("response = %+v, want m1 and m2 stored, one duplicate", res)
	}
	after := time.Now().UnixMilli()

	for _, e := range res.Events {
		var (
			id                         []byte
			received, created, applied int64
		)
		err := a.db.DB().QueryRow(`SELECT uuid, received_at, created_at, processed_at FROM events WHERE event_id = ?`,
			e.EventID).Scan(&id, &received, &created, &applied)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(id); got != strings.ReplaceAll(e.UUID, "-", "") {
			t.Errorf("%s: stored uuid %s, response %s", e.EventID, got, e.UUID)
		}
		if e.UUID[14] != '7' {
			t.Errorf("%s: uuid %s is not version 7", e.EventID, e.UUID)
		}
		// received_at follows X-Test-Now; the writer stamps the other two
		// with the server clock, created before processed.
		if received != now.UnixMilli() {
			t.Errorf("%s: received_at %d, want X-Test-Now %d", e.EventID, received, now.UnixMilli())
		}
		if created < before || created > applied || applied > after {
			t.Errorf("%s: created_at %d, processed_at %d, want %d <= created <= processed <= %d",
				e.EventID, created, applied, before, after)
		}
	}

	// A resend of a stored event returns no uuid.
	if code := a.post("/events", now, batch[2], &res); code != http.StatusOK || len(res.Events) != 0 || res.Duplicates != 1 {
		t.Fatalf("resend = %d %+v, want a duplicate with no events", code, res)
	}
}
