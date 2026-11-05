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
- `pendingVoids` entries age out by the void's occurredAt (older than 1 h before now).
- Pruning of per-entity slices is done on append, relative to the `now` the caller passes in.
  Open for phase 3: which "now" to use during replay. Wall-clock now would
  prune everything from history; the max applied occurredAt is the likely choice.
- Event type smallint codes are fixed: LOGIN=1 … WITHDRAWAL_REQUESTED=10 (see `internal/state/event.go`).
- Module path is `github.com/devastion/anti-fraud-go` (the repo has no remote yet).
- Phase 1 already includes the per-entity apply methods from SPEC §7
  (`state.Player`, `state.Device`, `state.PendingVoids`), because several §12
  rule tests (voids, late deposits, device novelty, declined-only methods) are
  about apply semantics. Interning, sharding, the writer and snapshots come in phase 2/3.
