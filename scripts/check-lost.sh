#!/usr/bin/env bash
# Compares the unique events k6 saw accepted (load/results/run.env, written by
# handleSummary) with the rows of that run in Postgres. Any difference fails.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck disable=SC1091
source load/results/run.env
stored=$(docker compose exec -T db psql -U "${POSTGRES_USER:-antifraud}" -d antifraud -tAc \
  "SELECT count(*) FROM events WHERE event_id LIKE 'lt-${RUN_ID}-%'")
if [[ "$stored" == "$ACCEPTED" ]]; then
  echo "lost-event check: ok (k6 accepted ${ACCEPTED}, Postgres has ${stored} for run ${RUN_ID})"
else
  echo "lost-event check: FAIL (k6 accepted ${ACCEPTED}, Postgres has ${stored} for run ${RUN_ID})"
  exit 1
fi
