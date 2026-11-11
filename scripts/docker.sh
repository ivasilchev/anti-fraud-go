#!/usr/bin/env bash
# Builds the image and runs the server in a container with the SPEC's limits
# (2 CPUs, 2 GB). The container mounts the same data directory as
# scripts/app.sh, so `make seed` fills it for both. Run one of them at a time.
# Usage: scripts/docker.sh build|up|down|restart|status|logs
# Env: PORT (8080), IMAGE (anti-fraud-go), CONTAINER (anti-fraud-go),
# DATA_DIR (./data), TEST_MODE, PPROF, GOMEMLIMIT, GOGC.
set -euo pipefail
cd "$(dirname "$0")/.."
image=${IMAGE:-anti-fraud-go}
name=${CONTAINER:-anti-fraud-go}
data=${DATA_DIR:-$PWD/data}
port=${PORT:-8080}

running() {
  [[ "$(docker container inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" == true ]]
}

build() {
  docker build -t "$image" .
}

up() {
  if running; then
    echo "container $name is already running"
    return
  fi
  if scripts/app.sh status >/dev/null 2>&1; then
    echo "the host app uses $data, stop it first (make down)" >&2
    return 1
  fi
  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    echo "port $port is already in use" >&2
    return 1
  fi
  docker rm "$name" >/dev/null 2>&1 || true
  mkdir -p "$data"
  # --user keeps the files in data/ owned by you, so the host app can use them.
  # --stop-timeout gives the shutdown snapshot time to finish.
  docker run -d --name "$name" --cpus 2 --memory 2g --stop-timeout 60 \
    --user "$(id -u):$(id -g)" -p "$port:8080" -v "$data:/data" \
    -e GOMEMLIMIT="${GOMEMLIMIT:-1100MiB}" -e GOGC="${GOGC:-200}" \
    -e TEST_MODE="${TEST_MODE:-false}" -e PPROF="${PPROF:-0}" \
    "$image" >/dev/null
  # A full replay of 10M events takes about 20 s. Wait up to 10 min.
  for ((i = 0; i < 1200; i++)); do
    if ! running; then
      echo "container $name exited during startup, last log lines:" >&2
      docker logs --tail 20 "$name" >&2
      return 1
    fi
    if curl -fsS "http://localhost:$port/health/ready" >/dev/null 2>&1; then
      echo "container $name is ready on http://localhost:$port (TEST_MODE=${TEST_MODE:-false})"
      return
    fi
    sleep 0.5
  done
  echo "container $name is not ready after 10 min, see: docker logs $name" >&2
  return 1
}

# down stops the container (SIGTERM, so the server writes a snapshot) and
# removes it. The data directory stays.
down() {
  if docker container inspect "$name" >/dev/null 2>&1; then
    docker stop "$name" >/dev/null
    docker rm "$name" >/dev/null
    echo "container $name stopped"
  fi
}

case ${1:-} in
build) build ;;
up) up ;;
down) down ;;
restart) down && up ;;
status) if running; then echo "container $name is running"; else echo "container $name is not running"; exit 1; fi ;;
logs) docker logs -f "$name" ;;
*)
  echo "usage: $0 build|up|down|restart|status|logs" >&2
  exit 2
  ;;
esac
