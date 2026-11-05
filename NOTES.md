# Notes

## Assumptions (SPEC §11)

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

## Additional assumptions (phase 1)

Validation and errors:

- Field-level problems are returned as top-level `VALIDATION_FAILED` with one
  `details` entry per problem (as in the SPEC §5 example). `INVALID_JSON`,
  `EMPTY_BATCH`, `BATCH_TOO_LARGE`, `PAYLOAD_TOO_LARGE` and `OVERLOADED` are
  top-level codes with empty `details`.
- `details[].index` is the event position (0 for a single-object body). It is
  omitted for decision requests.
- `PAYLOAD_TOO_LARGE` returns HTTP 413. Body limit is 1 MiB (1,048,576 bytes); exactly 1 MiB is accepted.
- A missing field or JSON `null` is `MISSING_FIELD`. A present but empty, too
  long or non-string ID is `INVALID_FIELD`. "64 characters" counts Unicode code points.
- `ip` gets the same non-empty and length check as IDs. Its format is not validated.
- Some IDs and `ip` values are `INVALID_FIELD`:
  - Values containing control characters (`unicode.IsControl`: C0, DEL, C1,
    including NUL). Postgres `text` rejects NUL, so one such row would fail a
    whole group-commit flush. Control characters also invite log injection.
  - Any string field with invalid UTF-8 or an unpaired `\uD800`–`\uDFFF`
    escape. encoding/json would silently turn these into U+FFFD, so distinct
    IDs could collapse into one. A literal U+FFFD or `\ufffd` is accepted.
- Arrays are split with a streaming decoder that stops at element 501. An
  oversized batch of tiny values costs at most 501 element decodes before
  `BATCH_TOO_LARGE`. If an array has more than 500 elements and also contains a
  syntax error after the 500th, the response is `BATCH_TOO_LARGE`, not `INVALID_JSON`.
- A non-string `amount` (e.g. a JSON number) is `INVALID_AMOUNT`. A non-string
  `currency` is `INVALID_FIELD`. A string outside EUR/USD/GBP (case-sensitive) is `UNSUPPORTED_CURRENCY`.
- `amount` and `currency` are optional on types that don't need them. If one
  of the two is sent, the other is required (`MISSING_FIELD`), so that every
  stored amount has an EUR value.
- `occurredAt` must be RFC 3339 (`time.RFC3339Nano`, fractional seconds
  optional, `.` separator only) with uppercase `Z` or a `±hh:mm` offset
  (hours 00–23, minutes 00–59). Other ISO-8601 forms (basic format, `+hhmm`,
  no seconds, `,` decimal separator) are rejected with `INVALID_TIMESTAMP`.
  Exactly 5 min ahead of now is accepted; 5 min + 1 ms is `EVENT_IN_FUTURE`.
- Times are kept as unix milliseconds; sub-millisecond precision is dropped.
- encoding/json matches field names case-insensitively, so `EventId` is accepted as `eventId`. Unknown fields are ignored.

Rules and state:

- Windows are applied literally (`now − W ≤ t ≤ now`), including the upper
  bound. An event dated up to 5 min in the future therefore doesn't count until
  `now` reaches it. For max-of fields (R3 timestamps, R4 lastSeenT), a
  future-dated event also hides an earlier in-window value until then.
- R1: one reason is reported even when both the player and device paths fire.
  The detail then lists both, separated by "; ".
- R5 fires when the paymentMethodId has never been seen in any event (not interned).
- Timestamps use 0 as "never". Events at or before the epoch can never fall in
  a window relative to the server clock, so this is unambiguous in practice.
- `lastDepositSeq == 0` means "no approved deposit". Postgres `bigserial` starts at 1.
- Bets in `recentBets` are keyed by a 64-bit FNV-1a hash of the eventId. That
  hash is stable across restarts, so it is safe in snapshots. A collision
  within one player's 6-minute bet window is treated as negligible.
- `TurnoverEUR` uses saturating addition at `math.MaxInt64` (~9.2e14 EUR), so a
  flood of maximum-size bets can't wrap it negative and fire R2. Once saturated,
  voids subtract from the cap, so turnover is no longer exact. It stays far above
  any single deposit (max ~1.17e12 EUR) unless roughly 790 maximum-size bets are voided.
- `ApplyVoid` marks a bet voided even when it is older than the last deposit.
  Turnover is unchanged in that case. Marking it means no later recompute can
  count it. SPEC §7 only describes the `t_b ≥ lastDepositT` case.
- `pendingVoids` entries age out by the void's occurredAt (older than 1 h before
  the pruning clock; see phase 3).
- Pruning of per-entity slices is done on append, relative to the `now` the
  caller passes in. Phase 3 settled which `now`: the log-derived pruning clock.
- Event type smallint codes are fixed: LOGIN=1 … WITHDRAWAL_REQUESTED=10 (see `internal/state/event.go`).
- Module path is `github.com/devastion/anti-fraud-go` (the repo has no remote yet).
- Phase 1 already includes the per-entity apply methods from SPEC §7
  (`state.Player`, `state.Device`, `state.PendingVoids`), because several §12
  rule tests (voids, late deposits, device novelty, declined-only methods) are
  about apply semantics. Interning, sharding, the writer and snapshots come in phase 2/3.

## Additional assumptions (phase 2)

Ingestion and storage:

- `POST /events` returns 200 with `{"accepted": n, "duplicates": m}`.
- A flush closes at 2,000 events or 5 ms after its first request, whichever
  comes first. Requests are never split across flushes, so one flush can hold up
  to 2,499 events.
- Within one flush, the first occurrence of a duplicated eventId wins. This
  relies on Postgres inserting `unnest` rows in array order, which also
  assigns `seq` in that order.
- Late detection uses the `now` of the request that carried the event (the
  receive time, or `X-Test-Now` in test mode). It differs from the exact apply
  instant by at most one flush interval plus the insert latency. Pruning does
  not use it (phase 3).
- If the insert fails, every request in the flush gets 500 `INTERNAL_ERROR`
  (a new top-level code), and nothing from that flush was committed. Clients
  retry; de-duplication makes retries safe. If the commit succeeded but the
  reply was lost, the writer catches up from Postgres before its next flush (phase 3).
- If the bet lookup for voids fails after the insert has committed, it is
  retried 5 times, 100 ms apart. If it still fails, those voids go to
  `pendingVoids` and an error is logged.
- When TEST_MODE=true, an unparseable `X-Test-Now` header returns 400
  `VALIDATION_FAILED` with detail `{field: "X-Test-Now", code: "INVALID_TIMESTAMP"}`.
- Server env: `DATABASE_URL` (required), `ADDR` (default `:8080`), `TEST_MODE`, `PPROF`.
  Pool size is 4 connections: the writer uses one; the rest are for lookups and replay.
- Testcontainers can't find the Docker socket when it is not at
  `/var/run/docker.sock` (e.g. OrbStack, Colima). Integration tests then need
  `DOCKER_HOST=$(docker context inspect -f '{{.Endpoints.docker.Host}}')`; `make test` will export it.

Voids:

- A void is resolved, in order, from the player's `recentBets`, from bets in
  the same flush, and from one batched Postgres lookup per flush. If the bet is
  later in the same flush (higher seq), the void goes to `pendingVoids` and the
  bet arrives voided.
- A void that references another player's bet is ignored: no state change, and
  not pending. `pendingVoids` is keyed by bet eventId only (SPEC §7), so a
  void that arrives before its bet is not checked against the bet's player.
- A void found via Postgres (bet no longer in `recentBets`) subtracts the
  amount if `t_b ≥ lastDepositT`. It can't be marked voided in memory, because
  the bet is not there.

State layout:

- Players, devices and payment methods each use 256 shards. The shard is
  `FNV-1a(id) & 255`; index = `position << 8 | shard`. Each shard owns its
  dictionary (`map[string]uint32` plus a reverse `[]string`) and its entity slice.
- Decisions read-lock the payment-method shard (released right away), then
  the player shard, then the device shard. The writer never holds two shard
  locks at once, so this cannot deadlock.
- Payment methods are interned only from DEPOSIT_APPROVED and DEPOSIT_DECLINED,
  the only events whose method feeds a rule.
- Metrics: `events_accepted_total`, `duplicate_events_total`, `late_events_total`,
  `decisions_total`, `rejected_requests_total{code}`, `flush_duration_seconds`
  (histogram), `ingest_queue_depth`.

## Additional assumptions (phase 3)

Pruning clock:

- Per-entity slices and pending voids are pruned relative to
  `pruneNow = max applied occurredAt − 5 min`, not the receive time.
  - An accepted event is at most 5 min in the future (`EVENT_IN_FUTURE`), so
    `pruneNow` never runs ahead of the real clock. Retention is therefore at
    least what SPEC §7 asks for, measured from server now.
  - It depends only on the events applied, in seq order. Live state, a full
    replay and snapshot + tail are therefore byte-identical, which the tests
    check. Receive-time pruning would make replayed state depend on when each
    event happened to arrive.
  - If no events arrive, nothing is pruned. That costs only memory: rules
    always check their windows against the decision's `now`.
- `api.MaxFutureSkew` and `state.MaxFutureMs` must stay equal. A test enforces it.
- Pending voids are pruned whenever `pruneNow` enters a new second. That makes
  pruning deterministic too.
- Replayed events are never counted in `late_events_total`.

Replay:

- Replay streams `SELECT … WHERE seq > watermark ORDER BY seq`. It applies
  rows in chunks of 8,192, and voids in a chunk share one bet lookup.
  - The lookup returns the bet's seq. If the bet's seq is after the void's,
    the bet is treated as not yet arrived, so the void goes to `pendingVoids`,
    exactly as it did live.
  - A lookup error fails the replay; it never falls back to pending.
- To check in phase 4: on 10M rows, whether Postgres reads in PK index order
  or does a seq scan plus sort.
- While replay runs, `/health/live` is 200 and `/health/ready` is 503.
  `POST /events` and `POST /decisions` return 503 with a new top-level code,
  `NOT_READY`. The ingest writer starts only after replay, so it never races replay.
- On SIGTERM, `/health/ready` turns 503. Requests on open connections are
  still served while the HTTP server drains.
- A startup recovery error (Postgres unreachable mid-replay) exits the
  process; the container restart policy retries.

Writer catch-up:

- After a failed insert, the writer replays `seq > watermark` from Postgres
  before its next flush or snapshot. This applies anything the failed
  statement committed anyway.
  - If the catch-up fails, the next flush fails too. Nothing newer is applied
    while memory might be missing older events.
  - Residual gap: a statement that commits after the catch-up query ran (e.g.
    the client timed out but the server kept going). Its events are durable,
    but they are missing from memory until the next restart. If a snapshot
    covers their seq, that restart won't replay them either. Treated as
    negligible; a fix would track the seqs of failed statements.

Snapshots:

- The format is binary with varints: magic `AFGOSNAP`, a uint32 version, then
  the watermark (seq and eventId), the pruning clock, the three sharded
  tables in index order, and the pending voids sorted by bet ID. A CRC-32C
  trailer covers everything before it. See `internal/state/snapshot.go`.
- The snapshot is encoded inside the writer goroutine between two flushes. It
  is a consistent cut at the watermark.
  - It reads one shard at a time under that shard's read lock. Decisions take
    only read locks, and the writer is paused, so decisions are never blocked.
  - Ingestion acks wait for the pause; events keep queueing meanwhile.
  - fsync, rename and the directory fsync happen after the writer resumes.
  - The pause length is exported as `snapshot_pause_seconds`. Phase 4
    measures it at 1M players.
- A snapshot is skipped when nothing was applied since the last one written
  or loaded.
- At startup, the snapshot is used only if `event_id` at its watermark seq
  matches the one it recorded.
  - This detects a truncated or reseeded log (`make seed`, even with
    `RESTART IDENTITY`). A corrupt or wrong-version file is also ignored.
  - An ignored snapshot means a full replay, logged as a warning.
- Loading reads the whole file into memory once (transient, about the
  snapshot size).
- Env: `SNAPSHOT_PATH` (default `/data/snapshot.bin`; set it to empty to
  disable) and `SNAPSHOT_INTERVAL` (default `5m`). If the snapshot directory
  does not exist at startup, snapshots are disabled with a warning.
- New metrics: `startup_replayed_events`, `startup_recovery_seconds`,
  `startup_snapshot_seq`, `snapshots_total`, `snapshot_failures_total`,
  `snapshot_seq`, `snapshot_bytes`, `snapshot_pause_seconds`.

Tests:

- The restart tests run the real `cmd/server` binary, built once per test
  run, as a child process and kill it with SIGKILL. They check that the same
  decisions come back after snapshot + tail, after a full replay, and after a
  periodic snapshot.
- Another restart test kills the server mid-ingestion and checks that every
  acknowledged event is in Postgres and in memory.
- `ON CONFLICT DO NOTHING` uses up a sequence value even for a skipped
  duplicate, so seqs have gaps. Counts must use `count(*)`, never seq arithmetic.

## Additional assumptions (phase 4)

Generator (`cmd/generate`, logic in `internal/gen`):

- `--events` is the exact total, planted events included. Every player gets a
  registration session (3–5 events), and the remaining events go to sessions
  of Zipf-chosen players (s = 1.1, v = 20, ranks mapped through a fixed
  permutation so that the busy players are spread across the ID space).
  Session lengths are planned up front so the total is exact.
  `--events` must leave at least 3 background events per player.
- Sessions follow LOGIN → DEPOSIT_ATTEMPT → APPROVED (88%) / DECLINED (and,
  half the time, a retry) → BET (60%) / WIN, with an occasional BET_VOIDED (3%)
  of the player's last bet. 15% of sessions end with WITHDRAWAL_REQUESTED. 2% of
  sessions add a PASSWORD_RESET, and 2% add a PAYOUT_METHOD_ADDED. Events in a
  session are 5–90 s apart. Every event in a session carries its device, and
  LOGIN also carries an IP.
- Every per-player attribute is a pure function of the player index and the
  seed, so nothing per player is stored:
  - Currency: 60% EUR, 25% USD, 15% GBP.
  - Payment methods: 1–3, `pm<3i+k>`.
  - Devices: a primary device `d<i>`, and for 30% of players a second one `d<n+i>`.
- Shared devices: `d<2n+g>`, with groups of 3, 4, 5, 6 players in turn. They
  are sized to be 2% of the devices in use.
  - A shared player's registration session falls within the last 29 days. Its
    events after the LOGIN use the shared device, so every shared device has
    3–6 players within the last 30 days.
  - The test checks both the 2% share and the 3–6 range.
- Background sessions start between `now − days` and `now − 45 min` and last
  under 36 min, so no background event is in the future.
- The events are produced in occurredAt order by a min-heap merge, with
  sessions sorted by start. Planted events are merged into the same stream.
  One COPY then loads them, so seq order is exactly occurredAt order.
- `TRUNCATE events RESTART IDENTITY` runs before the load. The service's
  snapshot check (phase 3) then discards a stale snapshot.
- `VACUUM (ANALYZE) events` runs after the load. It sets hint bits and the
  visibility map now, instead of during the service's first replay, and gives
  the planner statistics.
- Event IDs are zero-padded (`g-00000001`) so that the `event_id` index fills
  roughly in order. Background IDs use `p%07d`, `d%07d`, `pm%07d`, which caps
  players at 3,333,333.

Planted scenarios:

- Planted IDs use a `pl-` / `dpl-` prefix, outside the background ID space
  that the load test draws from. Load traffic therefore can't change their outcome.
- Some rules have no numeric threshold. For those, "exact-threshold" and
  "one-below" mean:
  - R3 (time window): exact = the three components once each, the oldest 50
    min old; one-below = two of three (device seen before, or no reset).
  - R5 (set membership): the firing case uses a method seen only in a
    declined deposit; the non-firing case uses one from an approved deposit.
    Unknown-player and never-seen-method cases are added.
  - The manifest's `kind` field says which is which.
- Timing: R1 events are at most 60 s old and R3 events at most 50 min old at
  generation time, so `make verify` must run within about 9 minutes of `make seed`.
  Planting future-dated events would extend that, but would push the
  service's pruning clock ahead of real time.
  - verify always sends `X-Test-Now = generatedAt + 1 min`. With
    TEST_MODE=true the result is therefore independent of when it runs;
    otherwise the header is ignored, and verify warns if the data is over 9
    minutes old.
- The manifest is written to the `manifest` volume. `make verify` runs verify
  in a container against `http://app:8080`; `make manifest` copies the file
  out to the working directory.

Packaging (needed to measure phase 4 under the SPEC limits):

- One distroless image holds `server`, `generate` and `verify`. The app
  healthcheck is `server healthcheck`, because distroless has no curl. It has
  `start_period: 10m` to cover a full replay.
- The compose project is named `anti-fraud-go`. A project named `anti-fraud`
  already exists on the dev machine with its own `pgdata` volume.
- The app also exports `process_resident_memory_bytes` (from `/proc/self/statm`)
  and Go heap gauges.
- `GOMEMLIMIT` was tuned from the SPEC's 1500MiB to 1100MiB (`GOGC=200`
  unchanged); see the measurements below. Both can be overridden from the
  environment. `SNAPSHOT_PATH=` (empty) disables snapshots, which forces a full replay.
- After recovery the server calls `State.Trim()` and then
  `debug.FreeOSMemory()`. Trim shrinks every slice to its length (capacity
  only, no state change; a test checks this).

## Phase 4 measurements

Machine: Apple M5, 10 cores, 32 GB RAM, macOS 27.2, OrbStack (Docker
29.8.2, 10 CPUs and 16 GB for the VM). app and db are limited to 2 CPUs and
2 GB each. The data set is `--players=1000000 --events=10000000 --days=90 --seed=42`.

| What | Result |
|---|---|
| Generate + COPY 10M events (one stream, indexes in place) | 40.5 s (247k events/s) |
| `VACUUM (ANALYZE)` after the load | 6.5 s |
| `make seed` end to end (load + app restart with full replay) | 48 s + 8 s |
| Table size | 1,053 MB heap, 1,690 MB with indexes |
| State | 1,000,045 players, 1,044,360 devices, 1,081,217 payment methods |
| Full replay of 10M events (2 CPUs) | 7.1 s (~1.4M events/s) |
| Snapshot file | 95 MB |
| Snapshot writer pause (encode) / total with fsync and rename | 89 ms / 115 ms |
| Recovery from the snapshot (0 tail events) | 0.36 s |
| Live heap after full replay, before / after `Trim` | 618 MB / 541 MB |
| RSS after full replay, before / after `Trim` + `FreeOSMemory` | 1,334 MB / 663 MB |
| RSS after recovery from the snapshot | 557 MB |
| `make verify` (TEST_MODE off, server clock) | 28/28 cases pass, after the full replay and after snapshot recovery |

- The replay query uses `Index Scan using events_pkey` with no sort. The
  load is in seq order, so the correlation is 1.
- Where the memory goes (heap profile after loading the snapshot, 511 MB):
  - Player table: 144 B per player, plus its dictionary: ~200 MB.
  - Per-player slices: ~65 MB.
  - Device table and its dictionary: ~126 MB.
  - Payment-method dictionary: ~74 MB.
  - ID strings: ~29 MB.
- Before the fixes, RSS was 1.33 GB. Replay garbage was never returned to
  the OS, because the scavenger keeps memory up to the GC goal.
- With ~540 MB live, `GOGC=200` lets the heap grow toward 3× live (capped by
  `GOMEMLIMIT`). At the SPEC's 1500MiB that would break the 1.2 GB RSS budget
  under load. At 1100MiB, the GC holds total memory around the limit. Phase 5
  must confirm RSS and latency under load.
- Not done, possible if memory is tight:
  - Hash payment methods to uint64 instead of interning them. This saves the
    ~74 MB dictionary, but deviates from SPEC §7.
  - A periodic sweep of stale entries. Per-entity slices are pruned only on
    append, so an inactive player keeps the bets and declines of its last
    session (bounded, a few tens of MB).
