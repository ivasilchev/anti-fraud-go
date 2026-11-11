# Notes

The full record of decisions, measurements and profiles, phase by phase, is in
[docs/implementation-log.md](docs/implementation-log.md).

## Assumptions

The task leaves these open. I chose the following, without confirmation from the task authors.

API and input:

- Decisions use the server clock as "now". With `TEST_MODE=true`, the
  `X-Test-Now` header overrides it for ingestion and decisions; `make verify`
  uses this.
- `BET_VOIDED` names the voided bet in `voidedEventId`.
- A duplicate `eventId` counts once. If the payload differs, the first one wins.
- Batches (1–500 events) are all-or-nothing. Any invalid input is a 400 with a
  code, including a body over 1 MiB (`PAYLOAD_TOO_LARGE`). Other status codes:
  - 503 when the ingest queue is full (`OVERLOADED`) or startup replay has not
    finished (`NOT_READY`).
  - 500 when the database write fails (`INTERNAL_ERROR`).
- `occurredAt` must be RFC 3339 with `Z` or `±hh:mm`; other ISO-8601 forms are
  rejected. Events up to 5 min in the future are accepted.
- Events more than 5 min late are accepted and applied best-effort, and
  counted in `late_events_total`.
- IDs are 1–64 characters with no control characters. Amounts are decimal
  strings with at most 12 integer digits and 2 decimals. `amount` and
  `currency` come together.
- Decisions are not persisted (no audit log).

Rules:

- All windows are inclusive: `now − W ≤ t ≤ now`.
- R1 checks the player's declines and, separately, the declines on the
  decision's device. It is one reason even when both fire.
- R2 does not fire without an approved deposit. Voided bets are not turnover.
- R3 needs no order between its three events. A device is "new" when the
  first event ever seen with it is a LOGIN inside the hour.
- R4 uses the decision's device and counts only players seen on it in events.
  The requesting player is not counted unless their own events used that device.
- R5 fires when the method was never used in an approved deposit, including
  for an unknown player.

Storage (since the switch from Postgres to SQLite):

- The event log is one SQLite file (`data/events.db`) that the app process
  opens itself. Only one process writes to it. `make seed` stops the app
  before the generator replaces the log.
- SQLite runs in WAL mode with `synchronous=FULL`. An acknowledged event
  survives a crash of the process and a power loss.
- The code outside `internal/store/sqlite` and the two `cmd` entry points
  sees only the `store.Store` interface. A new engine must pass the
  `internal/store/storetest` conformance suite.
- Each event row has a `uuid` (UUIDv7, 16-byte BLOB, unique) and three times:
  `received_at` (HTTP handler, `X-Test-Now` in test mode), `created_at`
  (writer took the request off the queue) and `processed_at` (flush start,
  right before the insert and the reducers). `processed_at` is written in the
  same INSERT, so it costs no extra commit but does not prove the reducers ran.
  In test mode `received_at` can be later than `created_at`.
- Startup adds these columns to an older file with `ALTER TABLE`, so they are
  nullable. Rows from before the change have no uuid and no times. The first
  start on a 10M-row file builds the partial `events_uuid` index with one scan.
- `POST /events` returns the `eventId` and `uuid` of each stored event, in
  request order. A duplicate gets no uuid; returning the stored one would
  need a read per duplicate.
- Seqs have no gaps for duplicates. Postgres used one seq for each skipped
  row. Nothing depends on gaps or on their absence.
- The lost-event check counts stored events with `server count-events
  <prefix>`, on the host, while the app runs. WAL mode lets this second
  process read the file.

HTTP measurement:

- A middleware records each request by route (the matched ServeMux pattern)
  and status code. Requests that match no route go to the route `other`.
  `/metrics` shows `http_requests_total{route,code}` and the histogram
  `http_request_duration_seconds{route}`. The histogram has bounds at 30 ms
  and 80 ms, the SPEC's p95 and p99 limits.
- The latency is measured from the start of the handler to its return. It
  does not include the time in the kernel socket queues, so it is lower than
  the latency that k6 measures.
- Every `HTTP_LOG_INTERVAL` (default `10s`, `0` disables it), the server
  logs one `http summary` line for each route that served requests in that
  interval: rate, 2xx/4xx/5xx counts, mean, p50, p95, p99 and max. The
  percentiles are histogram bucket bounds, capped at the max, so they are
  upper estimates.
- `ACCESS_LOG=1` logs one line for each request. It is off by default,
  because at 3,500 requests/s the log grows by about 1 MB/s.
- The middleware makes no allocations per request. A test compares the
  allocations of an instrumented decision with a plain one.

Runtime (since Docker was dropped):

- The app runs as a host process, started by `scripts/app.sh`. Nothing
  restarts it after a crash. The load test fails if its pid changes.
- `GOMAXPROCS=2` applies the 2-CPU limit to Go code. Without a cgroup, the OS
  does not cap memory and cannot OOM-kill the app. `make load` samples the
  RSS every 5 s with `ps` and fails if the peak goes over 2 GiB. A peak
  shorter than 5 s can be missed.
- `process_resident_memory_bytes` in `/metrics` reads `/proc`, so it is
  missing on macOS. The scripts use `ps` instead.
- k6 runs on the same host without a CPU limit, so it competes with the app
  for CPU. The old k6 container was limited to 4 CPUs.
- A bare-bones `Dockerfile` (no compose file) runs the same binary with
  `--cpus 2 --memory 2g` through `scripts/docker.sh`. It mounts `data/`, so
  it shares the event log with the host process. Only one of them may run.
  `make verify` and `make load` support only the host process.

## Trade-offs

- **One stateful instance.** All rule state is in memory in one process
  (slices indexed by interned IDs, behind sharded read-write locks).
  Decisions do no I/O, so p99 is about 2 ms.
  The cost: it scales up, not out, and state is limited by RAM (about 1 GB
  RSS for 1M players). Scale-out would partition player state by `playerId`
  and device state by `deviceId` behind a log. Each partition consumes its
  slice of the log. A decision that needs both a player view and a device
  view (R1, R4) then makes one extra hop.
- **An embedded SQLite file is the log, not a query store.** An event is
  acknowledged only after it is committed and applied to memory. Group commit
  (one transaction per flush of up to 2,000 events or 5 ms) keeps that durable
  at 3,000 events/s. The mean flush took 1.8 ms. Restarts rebuild memory from
  a snapshot plus the log tail in 0.4 s. A full replay of 10M events takes
  18 s (7 s with Postgres). The cost: SQLite has one writer, and only the app
  process can open the file. A shared log for scale-out needs another
  `store.Store` implementation.
- **Pruning follows the log clock, not the wall clock.** Live state, a full
  replay and snapshot + tail are byte-identical, and the tests check this.
  The cost is a little extra memory when traffic stops.
- **Only rule state is kept.** Per-entity state holds only what the five rules
  read: windows, max-of timestamps and turnover. A new rule that needs other
  history means a code change and a replay.
- **Strict validation.** Anything ambiguous (formats, field sizes, control
  characters) is rejected rather than guessed, so a bad event cannot poison a
  whole flush.
- **Snapshot pause.** A snapshot pauses the writer for about 100 ms at 1M
  players. Event acks wait for it; decisions do not.
- **Redux-shaped state, updated in place.** Each stored event is an action.
  The writer and replay dispatch batches of actions through a
  `state.Dispatcher`. It is not called a store, because `store.Store` is the
  event log.
  Middleware (`CountLate`, `ResolveVoids`) can read the state and do I/O.
  Only the reducers (`Reduce`, then `reduceDevice` and `reducePlayer`)
  change the state, and they do no I/O and take no locks. Decisions read
  through `State.Select`, and the rule functions act as selectors. Unlike
  Redux, reducers change entities in place and do not return a new state.
  Copies for 3,000 events/s over 1M players cost GC time and memory, and no
  rule needs old versions. `BenchmarkApply` did not change, and each
  flush makes one more allocation (the batch).

## With another day

- Copy-on-write or incremental snapshots, to remove the 100 ms writer pause.
- Fix the max-of edge case: an event dated in the future currently hides
  earlier in-window R3/R4 timestamps until `now` catches up. Keep the last
  two values, or clamp to `now`.
- Close the writer's catch-up gap: track the seqs of failed insert statements
  instead of relying on a single catch-up query.
- Periodically sweep idle entities' slices (they are pruned only on append),
  and hash payment-method IDs instead of interning them (about 70 MB saved).
- Store an explicit log identity (an epoch written by the generator) in the
  snapshot, instead of comparing the watermark event's ID and time.
- A decision audit log, and a load test of the scale-out design.

## Where AI tools helped

> Draft from what the repo shows; edit to match what actually happened.

- SPEC.md is a brief I wrote for an AI coding agent from the task PDF. The
  agent (pi, with Go and Docker skills in `.agents/skills`) implemented it
  phase by phase. I reviewed every phase, and the per-phase acceptance checks
  had to pass before moving on.
- Library docs (pgx v5, k6, testcontainers-go, Docker Compose) came from
  Context7, not from model memory.
- AI review agents audited the result against the task and the SPEC. They
  found a stale-snapshot bug after a reseed, a data race on a metrics hook,
  the 413-vs-400 mismatch and a missing benchmark, and these were fixed.
- What I did not delegate: the architecture, the assumptions above, and
  checking the measured numbers against the thresholds.
