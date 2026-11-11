SHELL := /bin/bash
DATA := $(or $(DATA_DIR),$(CURDIR)/data)
export DATA_DIR := $(DATA)
URL := http://localhost:$(or $(PORT),8080)

.PHONY: help build up down seed recovery verify test bench load \
	docker-build docker-up docker-down docker-logs

# Static binaries in bin/: server, generate, verify.
build: ## Build the server, generate and verify binaries into bin/
	@mkdir -p bin
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/server ./cmd/generate ./cmd/verify

# Lists each target that has a "## description" on its rule line.
help: ## Show this list of targets
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-13s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

up: build ## Build, start the app in the background, wait until it is ready
	scripts/app.sh start

down: ## Stop the app (SIGTERM, so it writes a snapshot)
	scripts/app.sh stop

# The app is stopped first: the generator replaces its event log.
seed: build ## Stop the app, generate the data set, restart the app to replay it
	scripts/app.sh stop
	@mkdir -p $(DATA)
	time bin/generate --players=$(or $(PLAYERS),1000000) --events=$(or $(EVENTS),10000000) \
		--days=$(or $(DAYS),90) --seed=$(or $(SEED),42) \
		--db=$(DATA)/events.db --manifest=$(DATA)/manifest.json
	time scripts/app.sh start
	@$(MAKE) --no-print-directory recovery

recovery: ## Print how the app recovered (from /metrics) and its resident memory
	@curl -fsS $(URL)/metrics | grep -E '^(startup_|go_memstats_heap)'
	@echo "process_resident_memory_bytes $$(scripts/app.sh rss)"

# Restarts the app with TEST_MODE=true so verify can pin the clock to the
# seed time (X-Test-Now): the planted R1/R3 cases then pass at any time.
# The app is restarted with TEST_MODE=false afterwards, even on failure.
verify: build ## Check the planted cases in data/manifest.json against the app
	TEST_MODE=true scripts/app.sh restart
	bin/verify --manifest=$(DATA)/manifest.json --url=$(URL); status=$$?; \
		TEST_MODE=false scripts/app.sh restart && exit $$status

test: ## Run all tests with the race detector
	go test -race ./...

bench: ## Run the benchmarks in internal/
	go test -run '^$$' -bench . -benchmem ./internal/...

# Needs k6 on the PATH (brew install k6).
load: build ## Run the k6 load test (DURATION=10m), lost-event check, results table
	scripts/load.sh

# The same server in a container (Dockerfile, scripts/docker.sh), limited to
# 2 CPUs and 2 GB. It mounts data/, so seed with `make docker-down seed down`
# first. make verify and make load drive the host process only.
docker-build: ## Build the anti-fraud-go container image
	scripts/docker.sh build

docker-up: docker-build ## Build and start the container, wait until it is ready
	scripts/docker.sh up

docker-down: ## Stop the container (it writes a snapshot first)
	scripts/docker.sh down

docker-logs: ## Follow the server log in the container
	scripts/docker.sh logs
