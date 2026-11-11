#!/usr/bin/env bash
# Runs the server as a background process on the host.
# Usage: scripts/app.sh start|stop|restart|status|pid|rss
#
# The server keeps its event log, snapshot, pid file and log in $DATA_DIR
# (default ./data). GOMAXPROCS=2 applies the SPEC's 2-CPU limit. Memory is not
# capped by the OS: make load measures the peak RSS and fails above 2 GiB.
# Env: PORT (8080), TEST_MODE, PPROF, GOMAXPROCS, GOMEMLIMIT, GOGC,
# SNAPSHOT_PATH (set to an empty string to disable snapshots),
# HTTP_LOG_INTERVAL (10s, 0 disables the per-route summary log),
# ACCESS_LOG (1 logs every request).
set -euo pipefail
cd "$(dirname "$0")/.."
data=${DATA_DIR:-$PWD/data}
pidfile=$data/server.pid
log=$data/server.log
port=${PORT:-8080}

# pid prints the server's pid and fails when it is not running.
pid() {
  [[ -f $pidfile ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null && cat "$pidfile"
}

start() {
  local p
  if p=$(pid); then
    echo "app is already running (pid $p)"
    return
  fi
  # Otherwise the health check below can reach another process on the port.
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "port $port is already in use (the container? make docker-down)" >&2
    return 1
  fi
  mkdir -p "$data"
  GOMAXPROCS=${GOMAXPROCS:-2} GOMEMLIMIT=${GOMEMLIMIT:-1100MiB} GOGC=${GOGC:-200} \
    DB_PATH=$data/events.db SNAPSHOT_PATH=${SNAPSHOT_PATH-$data/snapshot.bin} \
    ADDR=:$port TEST_MODE=${TEST_MODE:-false} PPROF=${PPROF:-0} \
    nohup bin/server >>"$log" 2>&1 &
  p=$!
  echo "$p" >"$pidfile"
  # A full replay of 10M events takes about 20 s. Wait up to 10 min.
  for ((i = 0; i < 1200; i++)); do
    if ! kill -0 "$p" 2>/dev/null; then
      rm -f "$pidfile"
      echo "app exited during startup, last lines of $log:" >&2
      tail -n 20 "$log" >&2
      return 1
    fi
    if ADDR=:$port bin/server healthcheck 2>/dev/null; then
      echo "app is ready on http://localhost:$port (pid $p, log $log, TEST_MODE=${TEST_MODE:-false})"
      return
    fi
    sleep 0.5
  done
  echo "app is not ready after 10 min, see $log" >&2
  return 1
}

# stop sends SIGTERM, which makes the server write a snapshot before it exits.
stop() {
  local p
  if ! p=$(pid); then
    rm -f "$pidfile"
    return
  fi
  kill -TERM "$p"
  for ((i = 0; i < 120; i++)); do
    if ! kill -0 "$p" 2>/dev/null; then
      rm -f "$pidfile"
      echo "app stopped"
      return
    fi
    sleep 0.5
  done
  echo "app did not stop within 60 s, sending SIGKILL" >&2
  kill -KILL "$p"
  rm -f "$pidfile"
}

case ${1:-} in
start) start ;;
stop) stop ;;
restart) stop && start ;;
status) if p=$(pid); then echo "app is running (pid $p)"; else echo "app is not running"; exit 1; fi ;;
pid) pid ;;
# rss prints the resident set size in bytes. ps works on Linux and macOS.
rss) p=$(pid) && echo $(($(ps -o rss= -p "$p") * 1024)) ;;
*)
  echo "usage: $0 start|stop|restart|status|pid|rss" >&2
  exit 2
  ;;
esac
