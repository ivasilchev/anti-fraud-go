#!/usr/bin/env bash
# Compares the unique events k6 saw accepted (load/results/run.env, written by
# handleSummary) with the events of that run in the event log. Any difference
# fails. The count runs inside the app container, next to the database file.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck disable=SC1091
source load/results/run.env
stored=$(docker compose exec -T app /usr/local/bin/server count-events "lt-${RUN_ID}-")
if [[ "$stored" == "$ACCEPTED" ]]; then
  echo "lost-event check: ok (k6 accepted ${ACCEPTED}, the log has ${stored} for run ${RUN_ID})"
else
  echo "lost-event check: FAIL (k6 accepted ${ACCEPTED}, the log has ${stored} for run ${RUN_ID})"
  exit 1
fi
