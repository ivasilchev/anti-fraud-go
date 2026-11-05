// Package store is the Postgres event log: schema migration, group-commit
// inserts with de-duplication, and bet lookups for voids.
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
)

var schema = `
CREATE TABLE IF NOT EXISTS events (
  seq               bigserial PRIMARY KEY,
  event_id          text NOT NULL UNIQUE,
  player_id         text NOT NULL,
  type              smallint NOT NULL,
  occurred_at       timestamptz NOT NULL,
  amount_e4         bigint,
  amount_eur_e4     bigint,
  currency          char(3),
  device_id         text,
  ip                text,
  payment_method_id text,
  voided_event_id   text
);
CREATE UNIQUE INDEX IF NOT EXISTS events_one_void_per_bet ON events (voided_event_id)
  WHERE type = ` + strconv.Itoa(int(state.EventBetVoided)) + `;`

// insertSQL inserts a whole flush in one statement. ON CONFLICT DO NOTHING has
// no conflict target, so it also skips a second void of the same bet.
const insertSQL = `
INSERT INTO events (event_id, player_id, type, occurred_at, amount_e4, amount_eur_e4,
  currency, device_id, ip, payment_method_id, voided_event_id)
SELECT * FROM unnest($1::text[], $2::text[], $3::smallint[], $4::timestamptz[], $5::bigint[],
  $6::bigint[], $7::text[], $8::text[], $9::text[], $10::text[], $11::text[])
ON CONFLICT DO NOTHING
RETURNING seq, event_id`

var lookupBetsSQL = `
SELECT seq, event_id, player_id, occurred_at, amount_eur_e4 FROM events
WHERE event_id = ANY($1) AND type = ` + strconv.Itoa(int(state.EventBet))

// streamSQL reads the log in seq order through the primary key index.
const streamSQL = `
SELECT seq, event_id, player_id, type, occurred_at, amount_e4, amount_eur_e4,
  currency, device_id, ip, payment_method_id, voided_event_id
FROM events WHERE seq > $1 ORDER BY seq`

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and creates the schema if it does not exist.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close closes the pool.
func (s *Store) Close() { s.pool.Close() }

// Pool returns the underlying pool.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Inserted is a row that was new to the log.
type Inserted struct {
	Seq     int64
	EventID string
}

var currencyCodes = [...]string{money.EUR: "EUR", money.USD: "USD", money.GBP: "GBP"}

// InsertEvents inserts events in one statement, skipping duplicates. It returns
// the rows that were inserted, in no particular order. Within one call, the
// first occurrence of a duplicated eventId is the one inserted.
func (s *Store) InsertEvents(ctx context.Context, events []state.Event) ([]Inserted, error) {
	n := len(events)
	var (
		ids       = make([]string, n)
		players   = make([]string, n)
		types     = make([]int16, n)
		times     = make([]time.Time, n)
		amounts   = make([]*int64, n)
		amountsEU = make([]*int64, n)
		curs      = make([]*string, n)
		devices   = make([]*string, n)
		ips       = make([]*string, n)
		methods   = make([]*string, n)
		voided    = make([]*string, n)
	)
	for i := range events {
		ev := &events[i]
		ids[i], players[i], types[i] = ev.EventID, ev.PlayerID, int16(ev.Type)
		times[i] = time.UnixMilli(ev.OccurredAt)
		if ev.Currency != money.NoCurrency {
			amounts[i], amountsEU[i], curs[i] = &ev.AmountE4, &ev.AmountEURE4, &currencyCodes[ev.Currency]
		}
		devices[i] = optional(&ev.DeviceID)
		ips[i] = optional(&ev.IP)
		methods[i] = optional(&ev.PaymentMethodID)
		voided[i] = optional(&ev.VoidedEventID)
	}
	rows, err := s.pool.Query(ctx, insertSQL,
		ids, players, types, times, amounts, amountsEU, curs, devices, ips, methods, voided)
	if err != nil {
		return nil, fmt.Errorf("insert events: %w", err)
	}
	defer rows.Close()
	out := make([]Inserted, 0, n)
	for rows.Next() {
		var r Inserted
		if err := rows.Scan(&r.Seq, &r.EventID); err != nil {
			return nil, fmt.Errorf("insert events: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("insert events: %w", err)
	}
	return out, nil
}

// optional maps "" to SQL NULL.
func optional(s *string) *string {
	if *s == "" {
		return nil
	}
	return s
}

// LookupBets returns the BET events among ids, keyed by eventId, with their seq.
func (s *Store) LookupBets(ctx context.Context, ids []string) (map[string]state.BetRef, error) {
	rows, err := s.pool.Query(ctx, lookupBetsSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("lookup bets: %w", err)
	}
	defer rows.Close()
	out := make(map[string]state.BetRef, len(ids))
	for rows.Next() {
		var (
			id  string
			ref state.BetRef
			t   time.Time
		)
		if err := rows.Scan(&ref.Seq, &id, &ref.PlayerID, &t, &ref.AmountEUR); err != nil {
			return nil, fmt.Errorf("lookup bets: scan: %w", err)
		}
		ref.T = t.UnixMilli()
		out[id] = ref
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lookup bets: %w", err)
	}
	return out, nil
}

// StreamEvents calls fn for every event with seq > afterSeq, in seq order.
// Rows are streamed from the server, never held in memory all at once. ev is
// reused between calls; fn must copy it to keep it. An error from fn stops
// the stream and is returned.
func (s *Store) StreamEvents(ctx context.Context, afterSeq int64, fn func(seq int64, ev *state.Event) error) error {
	rows, err := s.pool.Query(ctx, streamSQL, afterSeq)
	if err != nil {
		return fmt.Errorf("stream events: %w", err)
	}
	defer rows.Close()
	var (
		seq                         int64
		ev                          state.Event
		typ                         int16
		at                          time.Time
		amount, amountEUR           pgtype.Int8
		cur, dev, ip, method, voids pgtype.Text
	)
	for rows.Next() {
		if err := rows.Scan(&seq, &ev.EventID, &ev.PlayerID, &typ, &at, &amount, &amountEUR,
			&cur, &dev, &ip, &method, &voids); err != nil {
			return fmt.Errorf("stream events: scan seq %d: %w", seq, err)
		}
		ev.Type = state.EventType(typ)
		ev.OccurredAt = at.UnixMilli()
		ev.AmountE4, ev.AmountEURE4, ev.Currency = 0, 0, money.NoCurrency
		if cur.Valid {
			c, ok := money.ParseCurrency(cur.String)
			if !ok || !amount.Valid || !amountEUR.Valid {
				return fmt.Errorf("stream events: seq %d: bad amount or currency %q", seq, cur.String)
			}
			ev.AmountE4, ev.AmountEURE4, ev.Currency = amount.Int64, amountEUR.Int64, c
		}
		ev.DeviceID, ev.IP, ev.PaymentMethodID, ev.VoidedEventID = dev.String, ip.String, method.String, voids.String
		if err := fn(seq, &ev); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("stream events: %w", err)
	}
	return nil
}

// EventIDAt returns the eventId stored at seq, or ok == false if there is none.
func (s *Store) EventIDAt(ctx context.Context, seq int64) (id string, ok bool, err error) {
	err = s.pool.QueryRow(ctx, `SELECT event_id FROM events WHERE seq = $1`, seq).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("event at seq %d: %w", seq, err)
	}
	return id, true, nil
}
