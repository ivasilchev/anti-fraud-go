# Anti Fraud API

## Quickstart

```bash
docker compose up
```

Or in the background, returning once the app is healthy (`make up` does the
same):

```bash
docker compose up -d --wait
```

The app listens on `http://localhost:8080`

```bash
curl http://localhost:8080/health/ready   # {"status":"ready"}
```

The app stores its event log in an SQLite file, `/data/events.db` on the
`data` volume, next to its snapshot. There is no separate database service.
The log starts empty. To load 1M players and 10M events, run `make seed`.

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

These results come from the Postgres version, before the switch to SQLite.
The first SQLite run (run 1794371451) used Docker Desktop 29.4.3 with a VM of
10 CPUs and 8 GB. It passed the latency, error, lost-event and visibility
checks: decision p95 / p99 were 0.99 / 2.21 ms. It failed two checks. k6
dropped 263 iterations, and the app peaked at 1,237 MiB RSS, over the 1.2 GB
budget. Seeding took 51 s, and a full replay of 10M events took 18 s.
