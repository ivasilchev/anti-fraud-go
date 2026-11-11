// Package sqlite implements store.Store on an SQLite file (pure Go driver,
// no cgo). The database runs in WAL mode with synchronous=FULL, so a commit
// survives a crash of the process or the machine.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store"
)

// seq is the rowid. Without AUTOINCREMENT a seq could be reused after the
// newest row is deleted, but the log is append-only.
const tableSQL = `
CREATE TABLE IF NOT EXISTS events (
  seq               INTEGER PRIMARY KEY,
  event_id          TEXT NOT NULL,
  player_id         TEXT NOT NULL,
  type              INTEGER NOT NULL,
  occurred_at       INTEGER NOT NULL, -- unix ms
  amount_e4         INTEGER,
  amount_eur_e4     INTEGER,
  currency          TEXT,
  device_id         TEXT,
  ip                TEXT,
  payment_method_id TEXT,
  voided_event_id   TEXT
) STRICT`

// The indexes are separate from the table so that Import can build them once,
// after the bulk load, instead of row by row.
var indexSQL = `
CREATE UNIQUE INDEX IF NOT EXISTS events_event_id ON events (event_id);
CREATE UNIQUE INDEX IF NOT EXISTS events_one_void_per_bet ON events (voided_event_id)
  WHERE type = ` + strconv.Itoa(int(state.EventBetVoided))

const columns = `event_id, player_id, type, occurred_at, amount_e4, amount_eur_e4,
  currency, device_id, ip, payment_method_id, voided_event_id`

// insertSQL skips a row that breaks any unique index: a known eventId or a
// second void of the same bet.
const insertSQL = `INSERT INTO events (` + columns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT DO NOTHING RETURNING seq`

const importSQL = `INSERT INTO events (seq, ` + columns + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

var lookupBetSQL = `SELECT seq, player_id, occurred_at, amount_eur_e4 FROM events
WHERE event_id = ? AND type = ` + strconv.Itoa(int(state.EventBet))

const streamSQL = `SELECT seq, ` + columns + ` FROM events WHERE seq > ? ORDER BY seq`

const (
	// importChunk is how many imported rows go in one transaction. It keeps
	// the WAL file small during a bulk load.
	importChunk = 100_000
	// maxConns allows a bet lookup while a stream holds a connection.
	maxConns = 4
)

// Store is an SQLite event log.
type Store struct {
	db                             *sql.DB
	insert, lookupBet, watermarkAt *sql.Stmt
}

var _ store.Store = (*Store)(nil)

// Open opens (or creates) the database file at path and creates the schema
// if it does not exist.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlite: empty database path")
	}
	q := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"busy_timeout(10000)",
		"cache_size(-65536)", // 64 MiB per connection
	} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	s := &Store{db: db}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

func (s *Store) init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, tableSQL+";"+indexSQL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	var err error
	if s.insert, err = s.db.PrepareContext(ctx, insertSQL); err != nil {
		return err
	}
	if s.lookupBet, err = s.db.PrepareContext(ctx, lookupBetSQL); err != nil {
		return err
	}
	s.watermarkAt, err = s.db.PrepareContext(ctx, `SELECT event_id, occurred_at FROM events WHERE seq = ?`)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying database, for tests that inspect the stored rows.
func (s *Store) DB() *sql.DB { return s.db }

// InsertEvents implements store.Store. All events go in one transaction.
func (s *Store) InsertEvents(ctx context.Context, events []state.Event) (_ []store.Inserted, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("insert events: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	stmt := tx.StmtContext(ctx, s.insert)
	out := make([]store.Inserted, 0, len(events))
	for i := range events {
		ev := &events[i]
		var seq int64
		err := stmt.QueryRowContext(ctx, values(ev)...).Scan(&seq)
		switch {
		case errors.Is(err, sql.ErrNoRows): // duplicate
		case err != nil:
			return nil, fmt.Errorf("insert events: %w", err)
		default:
			out = append(out, store.Inserted{Seq: seq, EventID: ev.EventID})
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("insert events: commit: %w", err)
	}
	return out, nil
}

// values returns the column values of ev, in the order of columns.
func values(ev *state.Event) []any {
	var amount, amountEUR, cur any
	if ev.Currency != money.NoCurrency {
		amount, amountEUR, cur = ev.AmountE4, ev.AmountEURE4, ev.Currency.String()
	}
	return []any{ev.EventID, ev.PlayerID, int64(ev.Type), ev.OccurredAt, amount, amountEUR, cur,
		null(ev.DeviceID), null(ev.IP), null(ev.PaymentMethodID), null(ev.VoidedEventID)}
}

// null maps "" to SQL NULL.
func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// LookupBets implements store.Store.
func (s *Store) LookupBets(ctx context.Context, ids []string) (map[string]state.BetRef, error) {
	out := make(map[string]state.BetRef, len(ids))
	for _, id := range ids {
		var ref state.BetRef
		err := s.lookupBet.QueryRowContext(ctx, id).Scan(&ref.Seq, &ref.PlayerID, &ref.T, &ref.AmountEUR)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("lookup bets: %w", err)
		default:
			out[id] = ref
		}
	}
	return out, nil
}

// StreamEvents implements store.Store.
func (s *Store) StreamEvents(ctx context.Context, afterSeq int64, fn func(seq int64, ev *state.Event) error) error {
	rows, err := s.db.QueryContext(ctx, streamSQL, afterSeq)
	if err != nil {
		return fmt.Errorf("stream events: %w", err)
	}
	defer rows.Close()
	var (
		seq, typ                    int64
		ev                          state.Event
		amount, amountEUR           sql.NullInt64
		cur, dev, ip, method, voids sql.NullString
	)
	for rows.Next() {
		if err := rows.Scan(&seq, &ev.EventID, &ev.PlayerID, &typ, &ev.OccurredAt, &amount, &amountEUR,
			&cur, &dev, &ip, &method, &voids); err != nil {
			return fmt.Errorf("stream events: scan after seq %d: %w", seq, err)
		}
		ev.Type = state.EventType(typ)
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

// WatermarkAt implements store.Store.
func (s *Store) WatermarkAt(ctx context.Context, seq int64) (mark state.Watermark, ok bool, err error) {
	err = s.watermarkAt.QueryRowContext(ctx, seq).Scan(&mark.EventID, &mark.OccurredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return state.Watermark{}, false, nil
	}
	if err != nil {
		return state.Watermark{}, false, fmt.Errorf("event at seq %d: %w", seq, err)
	}
	mark.Seq = seq
	return mark, true, nil
}

// CountEvents implements store.Store. It uses the eventId index: valid UTF-8
// never contains the byte 0xff, so prefix+"\xff" bounds every match.
func (s *Store) CountEvents(ctx context.Context, prefix string) (int64, error) {
	var n int64
	var err error
	if prefix == "" {
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE event_id >= ? AND event_id < ?`,
			prefix, prefix+"\xff").Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// Import implements store.Store. It drops the table, loads the rows in chunks
// of importChunk per transaction, then builds the indexes. If it fails, the
// log is incomplete and must be imported again.
func (s *Store) Import(ctx context.Context, fill func(add func(*state.Event) error) error) (n int64, err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("import: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS events; `+tableSQL); err != nil {
		return 0, fmt.Errorf("import: reset: %w", err)
	}

	var (
		tx   *sql.Tx
		stmt *sql.Stmt
	)
	begin := func() error {
		var err error
		if tx, err = conn.BeginTx(ctx, nil); err != nil {
			return err
		}
		stmt, err = tx.PrepareContext(ctx, importSQL)
		return err
	}
	commit := func() error {
		stmt.Close()
		return tx.Commit()
	}
	if err := begin(); err != nil {
		return 0, fmt.Errorf("import: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	err = fill(func(ev *state.Event) error {
		n++
		if _, err := stmt.ExecContext(ctx, append([]any{n}, values(ev)...)...); err != nil {
			return fmt.Errorf("event %s: %w", ev.EventID, err)
		}
		if n%importChunk == 0 {
			if err := commit(); err != nil {
				return err
			}
			return begin()
		}
		return nil
	})
	if err == nil {
		err = commit()
	}
	if err != nil {
		return n, fmt.Errorf("import: %w", err)
	}
	if _, err := conn.ExecContext(ctx, indexSQL+"; ANALYZE"); err != nil {
		return n, fmt.Errorf("import: build indexes: %w", err)
	}
	return n, nil
}
