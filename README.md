# Anti Fraud API

## Quickstart

You need Go 1.26 or later. The load test also needs k6 (`brew install k6`).
Docker is optional (see below).

```bash
make up      # build bin/ and start the app in the background
make down    # stop the app (it writes a snapshot first)
make help    # list all targets with a short description
```

`make up` returns when the app is ready on `http://localhost:8080`. To use
another port, set `PORT`, for example `make up PORT=18080`.

```bash
curl http://localhost:8080/health/ready   # {"status":"ready"}
```

`scripts/app.sh` runs the server process. It sets `GOMAXPROCS=2`, which
applies the 2-CPU limit. The OS does not cap memory, so `make load` measures
the peak RSS and fails above 2 GiB.

The app keeps its files in `data/`: the SQLite event log `events.db`, the
snapshot, the manifest, `server.pid` and `server.log`. There is no separate
database service. The log starts empty.

```bash
make seed     # stop the app, load 1M players and 10M events, start the app
make verify   # check the planted cases in data/manifest.json
make test
make load     # 10-minute k6 run, lost-event check, results table
```

## Build and run with Docker

Docker is optional. The `Dockerfile` builds the three binaries in a Go image
and copies them into a distroless image of about 64 MB.

```bash
make docker-build   # docker build -t anti-fraud-go .
make docker-up      # build, start the container, wait until it is ready
make docker-logs    # follow the server log
make docker-down    # stop the container (it writes a snapshot first)
```

`make docker-up` runs the container with the SPEC limits of 2 CPUs and 2 GB,
on port 8080 (or `PORT`). It mounts `data/` at `/data`, so the container and
the host process use the same event log. Run one of them at a time.
`make docker-up` refuses to start while the host app runs.

If you seed while the container runs, stop the container first. The
generator replaces the event log.

```bash
make docker-down seed down docker-up
```

You can also run the binaries without make:

```bash
docker build -t anti-fraud-go .
docker run -d --name anti-fraud-go --cpus 2 --memory 2g -p 8080:8080 \
  --user "$(id -u):$(id -g)" -v "$PWD/data:/data" anti-fraud-go
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/data:/data" \
  --entrypoint generate anti-fraud-go --db=/data/events.db --manifest=/data/manifest.json
```

`make verify` and `make load` drive the host process only.

## Load test results

`make load` with the default 10-minute duration (run 1793859146), against
1,000,000 players and 10,000,000 pre-loaded events (`make seed`). app and db
are each limited to 2 CPUs and 2 GB; k6 runs in its own container (4 CPUs).

| Criterion                                       | Target                   | Measured                                                                     | Result |
| ----------------------------------------------- | ------------------------ | ---------------------------------------------------------------------------- | ------ |
| Duration                                        | 10 min sustained         | 600 s                                                                        | pass   |
| Ingestion                                       | 3,000 events/s           | 2,987 unique events/s (1,792,487); 13,570 resends all reported as duplicates | pass   |
| Decisions, concurrently                         | 500/s                    | 500/s (300,001)                                                              | pass   |
| Dropped iterations (k6 could not keep the rate) | 0                        | 0                                                                            | pass   |
| Decision latency p50 / p95 / p99 / max          | p95 ≤ 30 ms, p99 ≤ 80 ms | 0.24 / 0.97 / 1.83 / 53.19 ms                                                | pass   |
| All requests p50 / p95 / p99                    | —                        | 7.95 / 14.75 / 17.61 ms                                                      | —      |
| Error rate                                      | < 0.1%                   | 0.0000% (1,220,406 requests)                                                 | pass   |
| Lost events                                     | 0                        | 0 (k6 accepted count = rows in Postgres)                                     | pass   |
| Visibility: accepted event affects decisions    | 100%, p99 < 1 s          | 100%; p50 / p99 / max 11 / 19 / 36 ms                                        | pass   |
| app or db restarted / OOM-killed during run     | none                     | none                                                                         | pass   |
| App RSS, end / peak                             | < 1.2 GB                 | 1,011 / 1,037 MiB                                                            | pass   |
| Ingest queue depth, peak                        | < 50,000                 | 70 events                                                                    | —      |
| Flushes / mean flush latency                    | —                        | 69,219 / 2.89 ms                                                             | —      |
| Snapshots during run / writer pause             | —                        | 2 / 111 ms                                                                   | —      |

Machine: Apple M5, 10 cores, 32 GB RAM, macOS 27.2, OrbStack with Docker
29.8.2 (VM: 10 CPUs, 16 GB).

These results are stale. They come from the Postgres version, which ran in
Docker containers, before the switch to SQLite and to a host process.
The first SQLite run (run 1794371451) used Docker Desktop 29.4.3 with a VM of
10 CPUs and 8 GB. It passed the latency, error, lost-event and visibility
checks: decision p95 / p99 were 0.99 / 2.21 ms. It failed two checks. k6
dropped 263 iterations, and the app peaked at 1,237 MiB RSS, over the 1.2 GB
budget. Seeding took 51 s, and a full replay of 10M events took 18 s.
