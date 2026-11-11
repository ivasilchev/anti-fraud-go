#!/usr/bin/env bash
# Compares the unique events k6 saw accepted (load/results/run.env, written by
# handleSummary) with the events of that run in the event log. Any difference
# fails. The count opens the SQLite file next to the running server (WAL mode
# allows a second reader).
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck disable=SC1091
source load/results/run.env
stored=$(DB_PATH=${DATA_DIR:-$PWD/data}/events.db bin/server count-events "lt-${RUN_ID}-")
if [[ "$stored" == "$ACCEPTED" ]]; then
  echo "lost-event check: ok (k6 accepted ${ACCEPTED}, the log has ${stored} for run ${RUN_ID})"
else
  echo "lost-event check: FAIL (k6 accepted ${ACCEPTED}, the log has ${stored} for run ${RUN_ID})"
  exit 1
fi
