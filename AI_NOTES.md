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

## Trade-offs

- **One stateful instance.** All rule state is in memory in one process
  (slices indexed by interned IDs, behind sharded read-write locks).
  Decisions do no I/O, so p99 is about 2 ms.
  The cost: it scales up, not out, and state is limited by RAM (about 1 GB
  RSS for 1M players). Scale-out would partition player state by `playerId`
  and device state by `deviceId` behind a log. Each partition consumes its
  slice of the log. A decision that needs both a player view and a device
  view (R1, R4) then makes one extra hop.
- **Postgres is the log, not a query store.** An event is acknowledged only
  after it is committed and applied to memory. Group commit (one `INSERT …
  unnest` per flush of up to 2,000 events or 5 ms) keeps that durable at
  3,000 events/s with a 2–3 ms flush. Restarts rebuild memory from a snapshot
  plus the log tail: 0.4 s, or 7 s for a full replay of 10M events.
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
