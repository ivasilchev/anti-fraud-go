SHELL := /bin/bash
COMPOSE := docker compose

# testcontainers needs DOCKER_HOST when the socket is not at /var/run/docker.sock
# (OrbStack, Colima, Docker Desktop on some setups).
DOCKER_HOST ?= $(shell docker context inspect -f '{{.Endpoints.docker.Host}}' 2>/dev/null)
export DOCKER_HOST

.PHONY: up down build seed recovery verify test bench manifest load

build:
	$(COMPOSE) build app

up: build
	$(COMPOSE) up -d --wait db app

down:
	$(COMPOSE) down

# Generate and load the data set, then restart the app so it replays it.
seed: build
	$(COMPOSE) up -d --wait db
	$(COMPOSE) stop app
	time $(COMPOSE) --profile generate run --rm generate
	time $(COMPOSE) up -d --wait app
	@$(MAKE) --no-print-directory recovery

# Print how the app recovered (from /metrics).
recovery:
	@curl -fsS http://localhost:8080/metrics | grep -E '^(startup_|process_resident|go_memstats_heap)'

verify:
	$(COMPOSE) --profile generate run --rm verify

# Copy the manifest out of its volume, e.g. for running verify on the host.
manifest:
	docker run --rm -v anti-fraud-go_manifest:/m -v $(CURDIR):/host busybox cp /m/manifest.json /host/manifest.json

test:
	go test -race ./...

bench:
	go test -run '^$$' -bench . -benchmem ./internal/...

# k6 load test (DURATION=10m by default), lost-event check, results table.
load:
	scripts/load.sh
