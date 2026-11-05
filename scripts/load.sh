#!/usr/bin/env bash
# make load: runs the k6 scenarios against the running app, samples the app's
# memory meanwhile, runs the lost-event check, checks that app and db were not
# restarted or OOM-killed during the run, and prints a results table.
# Env: DURATION (default 10m), PLAYERS (default 1000000).
set -uo pipefail
cd "$(dirname "$0")/.."
export RUN_ID=${RUN_ID:-$(date +%s)}
export DURATION=${DURATION:-10m}
results=load/results
mkdir -p "$results" && chmod 777 "$results"
rm -f "$results"/run.env "$results"/table.txt

metric() { curl -fsS http://localhost:8080/metrics | awk -v m="$1" '$1 == m {print $2}'; }

# One line per container: name, restart count, start time and OOM flag. Any
# change between before and after the run means a crash or restart (the
# restart policy would otherwise hide it).
read -ra containers <<< "$(docker compose ps -q app db | tr '\n' ' ')"
if [[ ${#containers[@]} != 2 ]]; then
  echo "app and db must be running first (make up)" >&2
  exit 1
fi
lifecycle() {
  docker inspect -f '{{.Name}} restarts={{.RestartCount}} started={{.State.StartedAt}} oom={{.State.OOMKilled}}' \
    "${containers[@]}"
}

# Sample RSS and queue depth every 5 s; keep the maxima.
(
  peak_rss=0; peak_queue=0
  while sleep 5; do
    rss=$(metric process_resident_memory_bytes 2>/dev/null || echo 0)
    q=$(metric ingest_queue_depth 2>/dev/null || echo 0)
    (( ${rss%.*} > peak_rss )) && peak_rss=${rss%.*}
    (( ${q%.*} > peak_queue )) && peak_queue=${q%.*}
    printf 'PEAK_RSS=%s\nPEAK_QUEUE=%s\n' "$peak_rss" "$peak_queue" > "$results/peaks.env"
  done
) &
sampler=$!
trap 'kill $sampler 2>/dev/null' EXIT

before_late=$(metric late_events_total); before_dup=$(metric duplicate_events_total)
before_snap=$(metric snapshots_total)
before_lifecycle=$(lifecycle)
docker compose --profile load run --rm k6
k6_status=$?
kill $sampler 2>/dev/null

scripts/check-lost.sh
lost_status=$?

after_lifecycle=$(lifecycle)
if [[ "$before_lifecycle" == "$after_lifecycle" && "$after_lifecycle" != *oom=true* ]]; then
  lifecycle_status=0
else
  lifecycle_status=1
  printf 'container check: FAIL\nbefore:\n%s\nafter:\n%s\n' "$before_lifecycle" "$after_lifecycle"
fi

# shellcheck disable=SC1091
source "$results/peaks.env" 2>/dev/null || true
flush_sum=$(metric flush_duration_seconds_sum); flush_count=$(metric flush_duration_seconds_count)
{
  echo
  echo "== results (run ${RUN_ID}, ${DURATION}) =="
  cat "$results/table.txt" 2>/dev/null
  printf '%-44s  %s\n' "app RSS now / peak" "$(( $(metric process_resident_memory_bytes) / 1048576 )) MiB / $(( ${PEAK_RSS:-0} / 1048576 )) MiB"
  printf '%-44s  %s\n' "ingest queue depth peak" "${PEAK_QUEUE:-?} events"
  printf '%-44s  %s\n' "flushes / mean flush latency" "${flush_count} / $(awk -v s="$flush_sum" -v c="$flush_count" 'BEGIN{printf "%.2f ms", 1000*s/c}')"
  printf '%-44s  %s\n' "late events (>5 min) during run" "$(( $(metric late_events_total) - before_late ))"
  printf '%-44s  %s\n' "server-side duplicates during run" "$(( $(metric duplicate_events_total) - before_dup ))"
  printf '%-44s  %s\n' "snapshots during run / last writer pause" "$(( $(metric snapshots_total) - before_snap )) / $(awk -v s="$(metric snapshot_pause_seconds)" 'BEGIN{printf "%.0f ms", 1000*s}')"
  printf '%-44s  %s\n' "k6 thresholds" "$([[ $k6_status == 0 ]] && echo pass || echo "FAIL (exit $k6_status)")"
  printf '%-44s  %s\n' "lost-event check" "$([[ $lost_status == 0 ]] && echo pass || echo FAIL)"
  printf '%-44s  %s\n' "no restart / OOM kill (app, db)" "$([[ $lifecycle_status == 0 ]] && echo pass || echo FAIL)"
} | tee "$results/results.txt"

[[ $k6_status == 0 && $lost_status == 0 && $lifecycle_status == 0 ]]
