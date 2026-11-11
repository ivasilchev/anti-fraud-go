# Build: Anti-fraud decision service (Go)

You are building a take-home assignment end-to-end. Performance is the top priority after correctness. Work in phases, verify each phase before moving on, and do not stop until every acceptance criterion in section 12 passes. Use Context7 MCP for any library documentation (modernc.org/sqlite, k6, Docker Compose). Ask me only if something is truly blocking; otherwise follow the assumptions in section 11 and record any new ones in AI_NOTES.md.

## 1. Goal

A service that ingests player activity events and answers "is this payment action allowed?" with ALLOW / REVIEW / HOLD plus the reasons.

Hard limits (Docker): the app, with its embedded database, gets 2 CPUs and 2 GB RAM. With 1,000,000 players and 10,000,000 events pre-loaded, over a sustained 10-minute run, all of these must hold:

- Ingestion of 3,000 events/s with zero events lost.
- 500 decisions/s at the same time as that ingestion.
- Decision latency at the HTTP boundary: p95 ≤ 30 ms and p99 ≤ 80 ms.
- Fewer than 0.1% of requests return errors.
- An accepted event affects decisions within 1 s.

Durability: after a restart, nothing ingested is lost and the same input gets the same decision.

## 2. Tech stack (fixed)

- Language and HTTP: Go 1.25+ (container-aware GOMAXPROCS), stdlib net/http with ServeMux. JSON uses encoding/json; switch to github.com/goccy/go-json only if pprof shows JSON as the hot spot.
- Database: SQLite, embedded, via modernc.org/sqlite (pure Go, so CGO_ENABLED=0 and a distroless static image still work). WAL mode, synchronous=FULL. One file on the data volume.
- Storage abstraction: only internal/store/sqlite and the composition root (cmd/server, cmd/generate) name the engine. Everything else depends on the store.Store interface (section 4).
- Testing and load: integration tests use a temporary SQLite file per test (no Docker needed), k6 for load.
- Runtime env: GOMEMLIMIT=1500MiB, GOGC=200 (tune after measuring).
- No Redis or Kafka. The decision path must do zero I/O.

## 3. Architecture

A single stateful Go process. The event store (store.Store, backed by SQLite) is the durable log and the de-duplication authority. All rule state lives in memory.

Ingestion flow:

1. Handler parses and validates the whole request. Any invalid event rejects the entire request with 400 (all-or-nothing).
2. Valid events go onto a bounded channel (capacity 50,000 events). If the channel is full, return 503 OVERLOADED. Size it so this never happens under the target load.
3. A single writer goroutine flushes every 5 ms or every 2,000 events, whichever comes first. One transaction per flush (store.InsertEvents), with one prepared statement per row:
   INSERT INTO events (...) VALUES (?, ...) ON CONFLICT DO NOTHING RETURNING seq
   Note: ON CONFLICT DO NOTHING has no conflict target, so it also catches the partial unique index on voids (section 4).
4. The writer applies only the returned (new) rows to memory, in seq order.
5. Each waiting request is acknowledged only after its events are applied. This gives read-your-writes.
6. Response body: {"accepted": n, "duplicates": m}. Rows not returned by the insert count as duplicates.

Decision flow: parse, validate, look up player and device state under shard read-locks, evaluate rules R1–R5 with now = clock.Now(), and return the result. Nothing in this path may allocate beyond the response.

Startup flow:

1. Load the latest snapshot (if any).
2. Replay events WHERE seq > watermark ORDER BY seq, streaming rows (no full result set in memory).
3. /health/ready returns 503 until replay completes; /health/live always returns 200.

Snapshots: every 5 minutes and on SIGTERM, written to /data/snapshot.bin.tmp and then renamed atomically. The format is binary, carries a version header, and records the seq watermark. Snapshotting goes shard by shard and never blocks decisions for more than about 1 ms. The event log remains the source of truth; the snapshot is only an optimisation.

Endpoints: POST /events, POST /decisions, GET /health/live, GET /health/ready, GET /metrics (expvar or Prometheus text with counters for duplicates, late events >5 min, rejected requests, flush latency, queue depth). Expose pprof only when PPROF=1.

## 4. Event store

Interface (internal/store). The rest of the code sees only this:

    type Store interface {
      InsertEvents(ctx, events) ([]Inserted, error)  // atomic, durable, skips duplicates; first occurrence in a call wins
      LookupBets(ctx, ids) (map[eventId]BetRef, error)
      StreamEvents(ctx, afterSeq, fn) error          // seq order, streamed, fn may call LookupBets
      WatermarkAt(ctx, seq) (Watermark, ok, error)
      Import(ctx, fill) (n, error)                   // replaces the log, seq from 1 (generator)
      CountEvents(ctx, idPrefix) (n, error)          // lost-event check
      Close() error
    }

internal/store/storetest is a conformance suite. Every implementation must pass it.

SQLite schema (internal/store/sqlite, created on startup):

    CREATE TABLE events (
      seq               INTEGER PRIMARY KEY,
      event_id          TEXT NOT NULL,
      player_id         TEXT NOT NULL,
      type              INTEGER NOT NULL,
      occurred_at       INTEGER NOT NULL,  -- unix ms
      amount_e4         INTEGER,
      amount_eur_e4     INTEGER,
      currency          TEXT,
      device_id         TEXT,
      ip                TEXT,
      payment_method_id TEXT,
      voided_event_id   TEXT
    ) STRICT;
    CREATE UNIQUE INDEX events_event_id ON events (event_id);
    CREATE UNIQUE INDEX events_one_void_per_bet ON events (voided_event_id) WHERE type = <BET_VOIDED>;

Add no other indexes. The indexes are separate from the table so that Import builds them once, after the bulk load. Pragmas: journal_mode=WAL, synchronous=FULL, busy_timeout=10000, BEGIN IMMEDIATE for write transactions.

## 5. API contracts

POST /events accepts a single event object or an array of 1–500 events.

Event fields:

- Always required: eventId, playerId, occurredAt (ISO-8601 that must include an offset or Z), type.
- Optional: amount (decimal string), currency (EUR | USD | GBP), deviceId, ip, paymentMethodId, voidedEventId.

Required extra fields by type:

- LOGIN: deviceId
- PASSWORD_RESET: none
- DEPOSIT_ATTEMPT, DEPOSIT_APPROVED, DEPOSIT_DECLINED: amount, currency, paymentMethodId
- BET, WIN: amount, currency
- BET_VOIDED: voidedEventId
- PAYOUT_METHOD_ADDED: paymentMethodId
- WITHDRAWAL_REQUESTED: amount, currency, paymentMethodId

POST /decisions request:

    {"action": "DEPOSIT_ATTEMPT|WITHDRAWAL|BONUS_CLAIM", "playerId": "...", "amount": "...", "currency": "...", "deviceId": "...", "paymentMethodId": "..."}

- DEPOSIT_ATTEMPT and WITHDRAWAL require amount, currency and paymentMethodId.
- BONUS_CLAIM requires only playerId.
- deviceId is optional everywhere; rules that need it are skipped when it is absent.

Decision response (200):

    {"decision": "ALLOW|REVIEW|HOLD", "reasons": [{"rule": "R1_CARD_TESTING", "detail": "6 declined deposits with 3 payment methods on device in last 10m"}]}

- Strictness order is HOLD > REVIEW > ALLOW.
- Every rule that fired is listed, in rule order. If none fired, reasons is [].

Error response (400, or 503/500 where applicable):

    {"error": {"code": "VALIDATION_FAILED", "message": "...", "details": [{"index": 3, "field": "amount", "code": "INVALID_AMOUNT"}]}}

Error codes:

- INVALID_JSON
- EMPTY_BATCH
- BATCH_TOO_LARGE
- MISSING_FIELD
- INVALID_FIELD
- UNKNOWN_EVENT_TYPE
- UNKNOWN_ACTION
- INVALID_TIMESTAMP
- EVENT_IN_FUTURE (occurredAt more than 5 min after server now)
- INVALID_AMOUNT
- UNSUPPORTED_CURRENCY
- PAYLOAD_TOO_LARGE (body over 1 MB)
- OVERLOADED (503)

Field constraints:

- IDs are non-empty and at most 64 characters.
- Amounts must be > 0, have at most 2 decimals and at most 12 integer digits, and must match the regex ^\d{1,12}(\.\d{1,2})?$.

## 6. Money

- Never use float32 or float64 for money anywhere: not in parsing, storage, arithmetic or JSON.
- Parse the decimal string directly into an int64 count of 10^-4 units of its own currency.
- Convert to EUR with integer math: USD × 92 / 100, GBP × 117 / 100. This is exact at 10^-4 precision because the input has at most 2 decimals.
- All comparisons happen in EUR 10^-4 units (amount_eur_e4).

## 7. In-memory state

Interning:

- Map playerId, deviceId and paymentMethodId strings to uint32 once, in separate dictionaries (map[string]uint32 plus a reverse []string).
- Entity state lives in large slices indexed by that uint32.
- No per-entity maps and no pointer-heavy graphs. Small per-entity slices are allowed.

Sharding and concurrency:

- 256 shards for players and 256 for devices, each guarded by an RWMutex.
- The writer goroutine is the only mutator; decisions take read-locks only.

Time is stored as int64 unix milliseconds.

Player state:

- lastDepositT, lastDepositEUR, lastDepositSeq, turnoverEUR (R2)
- recentBets: entries (betEventIdHash uint64, t, amountEUR, voided bool), pruned to occurredAt ≥ now − 6 min (R2)
- declines: entries (t, pmIdx), pruned to t ≥ now − 15 min (R1)
- lastPasswordResetT, lastPayoutMethodAddedT, both max-of (R3)
- devices: entries (devIdx, firstSeenT, firstSeenIsLogin) (R3)
- approvedPMs: a small sorted []uint32 (R5)

Device state:

- declines: entries (t, pmIdx), pruned to t ≥ now − 15 min (R1)
- players: entries (playerIdx, lastSeenT), max-of, pruned beyond 30 days (R4)

Global state:

- pendingVoids map[betEventId]struct{t int64}, for voids that arrived before their bet; prune entries older than 1 h.

Memory budget target: under 1.2 GB RSS with the full data set. Measure it and report it.

Rules for applying an event (all of these must be order-independent within 5 minutes):

- Any event with deviceId:
  - Upsert player.devices: if the pair is new, firstSeenT = t and isLogin = (type == LOGIN).
  - If t < firstSeenT, replace firstSeenT and isLogin.
  - If t == firstSeenT and type == LOGIN, set isLogin = true.
  - Upsert device.players lastSeenT = max(lastSeenT, t).
- DEPOSIT_DECLINED: append (t, pm) to player.declines and, if deviceId is present, to device.declines.
- DEPOSIT_APPROVED:
  - Add pm to approvedPMs.
  - If (t, seq) > (lastDepositT, lastDepositSeq), it becomes the new last deposit. Then recompute turnoverEUR = sum of non-voided recentBets with t_b ≥ t. This is correct because, under the 5-minute lateness bound, every bet at or after t is still in recentBets.
- BET:
  - If its eventId is in pendingVoids, delete it from pendingVoids and treat the bet as voided: store it in recentBets with voided = true and do not count it.
  - Otherwise, if t ≥ lastDepositT, turnoverEUR += amount. Append it to recentBets either way.
- BET_VOIDED:
  - Find the bet in recentBets first. If it is not there, look it up in the event store by event_id, batching all voids of a flush into one LookupBets call (BET events only).
  - If the bet is found, belongs to this player, and t_b ≥ lastDepositT, subtract its amount and mark it voided.
  - If it is not found, add it to pendingVoids.
  - A second void of the same bet never reaches this point, because the partial unique index makes the insert a no-op.
- PASSWORD_RESET and PAYOUT_METHOD_ADDED: update their max-of timestamps.
- Events more than 5 minutes late (relative to server now at apply time): apply them best-effort and increment the late_events_total counter.

## 8. Rules

All windows are inclusive: an event counts when now − W ≤ t ≤ now.

R1_CARD_TESTING (applies to DEPOSIT_ATTEMPT, gives REVIEW):

- Within the last 10 min, the player has ≥ 5 declines with ≥ 3 distinct pmIdx among those same declines.
- Or, evaluated separately, the decision's deviceId has ≥ 5 declines with ≥ 3 distinct payment methods.

R2_CASHOUT_WITHOUT_PLAY (applies to WITHDRAWAL, gives HOLD):

- Fires when the player has an approved deposit and turnoverEUR < lastDepositEUR.
- No approved deposit means it does not fire.
- Only BET counts as turnover; WIN does not.

R3_ACCOUNT_TAKEOVER (applies to WITHDRAWAL, gives HOLD). All three must hold within [now − 60 min, now], in any order:

- lastPasswordResetT is in the window.
- lastPayoutMethodAddedT is in the window.
- Some device has firstSeenIsLogin == true and firstSeenT in the window.

R4_LINKED_ACCOUNTS (applies to BONUS_CLAIM and WITHDRAWAL, gives REVIEW):

- The decision's deviceId has ≥ 3 distinct players with lastSeenT ≥ now − 30 days.

R5_NEW_PAYOUT_METHOD (applies to WITHDRAWAL, gives REVIEW):

- The decision's paymentMethodId is not in player.approvedPMs.
- For an unknown player this fires.

Write the rule engine as pure functions: Evaluate(req, playerView, deviceView, now) -> Decision. Inject the clock. When TEST_MODE=true, the header X-Test-Now (RFC3339) overrides now for both ingestion and decisions; otherwise that header is ignored.

## 9. Data generator (cmd/generate)

Flags: --players=1000000 --events=10000000 --days=90 --seed=42 --db=/data/events.db

Behaviour:

- Replaces the log through store.Import with rows sorted by occurredAt, so seq order follows time order. The app must be stopped meanwhile.
- Must finish in a few minutes.
- Deterministic IDs: players p0000001…, devices d…, payment methods pm…, events g-<n>.

Realistic data:

- Player activity is Zipf-distributed.
- Sessions follow LOGIN → DEPOSIT_ATTEMPT → APPROVED/DECLINED → BET/WIN → occasional BET_VOIDED → WITHDRAWAL_REQUESTED.
- Most players use 1–2 devices and 1–3 payment methods.
- About 2% of devices are shared by 3–6 players within the last 30 days.
- Currencies are mixed.

Planted scenarios:

- For each rule, plant a firing case, an exact-threshold case and a one-below case, with timestamps relative to generation time so the short windows (R1, R3) are live.
- Also plant at least one player that fires several rules at once.
- Write manifest.json listing each planted player, the decision request to send, and the expected decision and rules.
- cmd/verify reads the manifest, calls /decisions, and exits non-zero on any mismatch.

## 10. Load test (load/k6.js) and one command

Scenarios (constant-arrival-rate, 10 min each, all three run concurrently):

- events, ~3,000 events/s total:
  - 1,500 req/s of single events plus 30 req/s of 50-event batches.
  - Use existing player IDs drawn from the generator's ID space.
  - occurredAt = now minus a random 0–300 s, to exercise late arrival.
  - 1% of events are resent duplicates.
  - Event IDs follow lt-<runId>-<vu>-<iter>-<i>.
- decisions, 500 req/s: 40% DEPOSIT_ATTEMPT, 40% WITHDRAWAL, 20% BONUS_CLAIM, for existing players and devices.
- visibility, 2 iterations/s:
  - Create a fresh player and post 5 declines with 3 payment methods, then immediately request a DEPOSIT_ATTEMPT decision.
  - Assert REVIEW and record the elapsed time in a Trend.

Thresholds:

- http_req_duration{scenario:decisions}: p(95) < 30 and p(99) < 80
- http_req_failed < 0.001
- the visibility check succeeds 100% of the time and its p(99) is < 1000 ms

Lost-event check: scripts/check-lost.sh compares the unique event IDs accepted, as reported by k6 in handleSummary, with the stored count from `server count-events lt-<runId>-`, run inside the app container. Any difference fails the run.

make load runs the k6 container (compose profile "load", with its own CPU limit), then the lost-event check, then prints a results table.

Makefile targets:

- make up: docker compose up -d app
- make seed: run the generator, then restart the app
- make verify
- make test
- make load
- make bench

## 11. Assumptions (record them in AI_NOTES.md)

- Decisions use the server clock as "now".
- A duplicate eventId with a different payload: the first one wins and the rest count as duplicates.
- Batches are all-or-nothing on validation.
- R2 does not fire without an approved deposit.
- R3 does not require order between the three events.
- "Seen device" means any event carrying that deviceId.
- R1 and R4 use the decision request's deviceId.
- Events more than 5 minutes late are accepted and applied best-effort.
- The field name for the referenced bet is voidedEventId.
- Decisions are not persisted (no audit log in scope).

## 12. Tests (make test must pass)

Unit tests of the rule engine (table-driven, fake clock).

R1:

- 5 declines with 3 payment methods fires; 4 declines does not.
- 5 declines with 2 payment methods does not fire.
- A decline exactly 10:00 old counts; one 10:00.001 old does not.
- The device path fires independently of the player path.

R2:

- turnover == deposit does not fire; turnover == deposit − 0.01 EUR fires.
- A voided bet is excluded.
- A void arriving before its bet is handled.
- A late deposit becomes the new last deposit and turnover is recomputed.
- A bet before the deposit is not counted.
- USD and GBP conversions are exact.
- No deposit means no fire.

R3:

- All three at exactly 60:00 fires; any one at 60:00.001 does not.
- Each component missing does not fire.
- A previously seen device does not fire.
- A late, earlier event for that device makes it no longer new.

R4:

- 3 players fires; 2 does not.
- A player last seen exactly 30 days ago counts; 30 days + 1 ms does not.

R5:

- A method used in an approved deposit does not fire.
- A method used only in declined deposits fires.

Combinations:

- When several rules fire, the strictest decision wins and all fired rules are listed.

Validation: every error code has a test.

Integration tests (a temporary SQLite file plus the real HTTP server):

- Duplicate in the same batch and across batches.
- Concurrent duplicate submissions.
- Out-of-order delivery within 5 minutes gives the same result as in-order delivery.
- Restart durability: ingest, kill the app (no graceful shutdown), restart, and get the same decision.
- Snapshot + tail replay produces state equal to a full replay.

Benchmarks:

- BenchmarkDecision: target < 20 µs and 0–2 allocs per op.
- BenchmarkApply.

## 13. Repository layout

    cmd/server, cmd/generate, cmd/verify
    internal/api (handlers, validation, errors)
    internal/ingest (queue, group-commit writer)
    internal/state (interning, shards, apply, snapshot)
    internal/rules (pure engine)
    internal/money
    internal/store (Store interface), internal/store/sqlite (implementation, schema migration on startup), internal/store/storetest (conformance suite)
    load/k6.js, scripts/, docker-compose.yml, Dockerfile (multi-stage, distroless), Makefile, README.md, AI_NOTES.md

docker-compose.yml:

- Service app, with deploy.resources.limits cpus "2" and memory 2g.
- A named volume "data" for the SQLite file and the snapshot. The generator mounts it too.
- A healthcheck on the app.
- Profiles "generate" and "load" for the one-shot containers.
- docker compose up must start everything with nothing else required.

## 14. Phases (verify each before continuing)

1. Money, validation and the rule engine with unit tests (all green).
2. Event store, group-commit writer, apply, and HTTP endpoints, with integration tests for duplicates, late events and voids.
3. Startup replay, snapshot, readiness, and the restart durability test.
4. Generator, manifest and verify. Load 10M events and report load time, replay time and RSS.
5. k6 load test. Profile with pprof and iterate until every threshold passes with margin. Usual culprits: allocations in JSON decode, lock contention, GC, flush size.
6. README with run instructions for the service, tests, generator and load test, plus a measured-results table (throughput, p50/p95/p99, error rate, visibility p99, RSS, replay time) and machine specs (CPU model, cores, RAM, OS, Docker version). AI_NOTES.md covering assumptions, trade-offs (single stateful instance; scale-out by partitioning player state by playerId and device state by deviceId behind a log), what you'd do with another day, and where AI tools helped.

Done means: make test, make verify and make load all pass on a fresh clone after docker compose up and make seed.
