# Benchmarking Temporal with Cassandra / ScyllaDB

Use the [omes](https://github.com/temporalio/omes) `throughput_stress` scenario to
measure end-to-end Temporal throughput against Cassandra or ScyllaDB.

## Prerequisites

- 16 logical CPU cores (the pinning layout below uses all cores on i7-1270P)
- `podman compose` (or `docker compose`)
- Go toolchain (see `go.mod` for required version — if your system Go is older,
  prefix all Go commands with `GOTOOLCHAIN=go<version>`, e.g. `GOTOOLCHAIN=go1.26.0`)
- [omes](https://github.com/temporalio/omes) cloned and built locally:
  ```bash
  cd /path/to/omes
  GOTOOLCHAIN=go1.26.0 go build -o omes ./cmd
  ```

## Core pinning layout

All containers use host networking to eliminate NAT/bridge overhead.

| Component        | Cores | Type             | Mechanism                                      |
|------------------|-------|------------------|-------------------------------------------------|
| Temporal server  | 0-3   | P-cores 0+1 (HT)| `taskset -c 0,1,2,3` (fastest cores for the bottleneck) |
| DB (Cass/Scylla) | 4-7   | P-cores 2+3 (HT)| `cpuset` in compose overlay (+ `--cpuset` for ScyllaDB) |
| Elasticsearch    | 8-11  | E-core cluster 4 | `cpuset` in compose overlay                    |
| Omes workers     | 12-15 | E-core cluster 5 | `taskset -c 12,13,14,15`                       |

## Full benchmark procedure

The complete sequence below incorporates all lessons learned from running these
benchmarks. Follow it exactly for reproducible results.

### 0. Clean slate

Reboot the machine for best results. Otherwise, ensure nothing is running:

```bash
pkill -9 -f temporal-server || true
podman kill -a 2>/dev/null; podman pod rm -f -a 2>/dev/null; podman rm -f -a 2>/dev/null
# Verify:
podman ps -a; podman pod ls
```

**Note:** `podman pod rm -f -a` sometimes fails to remove pods on the first try.
Run the kill/rm sequence repeatedly until `podman ps -a` and `podman pod ls` both
show nothing.

### 1. Start infrastructure

ScyllaDB and Cassandra both bind port 9042 via host networking — they cannot run
simultaneously.

#### Cassandra + Elasticsearch

```bash
podman compose \
  -f develop/docker-compose/docker-compose.cass.yml \
  -f develop/docker-compose/docker-compose.cass.bench.yml \
  -f develop/docker-compose/docker-compose.es.yml \
  -f develop/docker-compose/docker-compose.es.bench.yml \
  up -d
```

#### ScyllaDB + Elasticsearch

```bash
podman compose \
  -f develop/docker-compose/docker-compose.scylla.yml \
  -f develop/docker-compose/docker-compose.scylla.bench.yml \
  -f develop/docker-compose/docker-compose.es.yml \
  -f develop/docker-compose/docker-compose.es.bench.yml \
  up -d
```

#### Wait for readiness

Cassandra is usually ready in seconds; ScyllaDB takes 30-60 seconds.

```bash
# Cassandra:
for i in $(seq 1 60); do
  podman exec temporal-dev-cassandra cqlsh -e "SELECT now() FROM system.local" > /dev/null 2>&1 && echo "Cassandra ready" && break
  sleep 2
done

# ScyllaDB:
for i in $(seq 1 60); do
  podman exec temporal-dev-scylladb cqlsh -e "SELECT now() FROM system.local" > /dev/null 2>&1 && echo "ScyllaDB ready" && break
  sleep 2
done

# Elasticsearch:
for i in $(seq 1 30); do
  curl -sf http://127.0.0.1:9200/_cluster/health > /dev/null 2>&1 && echo "ES ready" && break
  sleep 2
done
```

### 2. Install schema

Wait for **both** DB and ES to be ready before running these.

```bash
# Cassandra:
make install-schema-cass-es

# ScyllaDB:
make install-schema-scylla-es
```

Both targets use `temporal-cassandra-tool` (ScyllaDB is CQL-compatible).

### 3. Build and start Temporal server

```bash
GOTOOLCHAIN=go1.26.0 go build -o temporal-server ./cmd/server
```

```bash
# Cassandra:
GOMAXPROCS=4 GOGC=200 GOTOOLCHAIN=go1.26.0 nohup taskset -c 0,1,2,3 \
  ./temporal-server --env development-cass-es start > /tmp/temporal-bench.log 2>&1 &

# ScyllaDB:
GOMAXPROCS=4 GOGC=200 GOTOOLCHAIN=go1.26.0 nohup taskset -c 0,1,2,3 \
  ./temporal-server --env development-scylla-es start > /tmp/temporal-bench.log 2>&1 &
```

Wait for the server to be ready. The HTTP API is on port **7243** (not 7233 which is gRPC):

```bash
for i in $(seq 1 30); do
  curl -sf http://127.0.0.1:7243/api/v1/namespaces > /dev/null 2>&1 && echo "Server ready" && break
  sleep 1
done
```

### 4. Register namespace and search attributes

```bash
# Register namespace (retention must use protobuf duration format, e.g. "86400s" not "1d"):
curl -sf http://127.0.0.1:7243/api/v1/namespaces -X POST \
  -H 'Content-Type: application/json' \
  -d '{"namespace":"default","workflowExecutionRetentionPeriod":"86400s"}'
```

Wait 5 seconds for namespace propagation, then register search attributes via the
gRPC OperatorService. **Do not use `tdbg cluster`** — it does not have this subcommand.
Use the helper script:

```bash
sleep 5
GOTOOLCHAIN=go1.26.0 go run tools/benchmark/register_sa.go
```

#### CRITICAL: Restart server after SA registration

The history service has its own namespace/SA cache that is separate from the
frontend's. After registering search attributes, the history shard caches may
take several seconds to refresh. This causes `BadSearchAttributes` errors on
`StartChildWorkflowCommand` during the first few seconds of a benchmark run.

**To eliminate SA propagation errors entirely**, restart the server after
registering search attributes. This forces all history shards to load fresh
SA data from the database on startup:

```bash
pkill -f temporal-server
sleep 3

# Restart (same command as step 3):
GOMAXPROCS=4 GOGC=200 GOTOOLCHAIN=go1.26.0 nohup taskset -c 0,1,2,3 \
  ./temporal-server --env development-cass-es start > /tmp/temporal-bench.log 2>&1 &

# Wait for ready:
for i in $(seq 1 30); do
  curl -sf http://127.0.0.1:7243/api/v1/namespaces > /dev/null 2>&1 && echo "Server ready" && break
  sleep 1
done
```

Verify SAs are usable (this tests the frontend; the server restart ensures
history shards are also fresh):

```bash
sleep 5
GOTOOLCHAIN=go1.26.0 go run tools/benchmark/verify_sa.go
```

### 5. Run omes throughput_stress

#### Quick before/after comparison (5 minutes, mc150)

```bash
GOTOOLCHAIN=go1.26.0 taskset -c 12,13,14,15 /path/to/omes run-scenario-with-worker \
  --language go \
  --scenario throughput_stress \
  --max-concurrent 150 \
  --duration 5m \
  --option internal-iterations=25 \
  --option internal-iterations-timeout=2h30m \
  --option continue-as-new-after-iterations=5 \
  --option sleep-time=100ms \
  --do-not-register-search-attributes
```

**Important flags:**
- `--language go` is required (not optional)
- `--do-not-register-search-attributes` means omes will NOT register SAs itself —
  they must be pre-registered (step 4)

The result is in the `[Scenario completion summary]` line, e.g.:
```
Total iterations completed: 202
```

#### Concurrency scaling test

Run with `--max-concurrent` set to 50, 100, 150, 200, and 250 to find the
throughput ceiling.

#### Nightly configuration

```bash
GOTOOLCHAIN=go1.26.0 taskset -c 12,13,14,15 /path/to/omes run-scenario-with-worker \
  --language go \
  --scenario throughput_stress \
  --max-concurrent 50 \
  --duration 2h5m0s \
  --option internal-iterations=25 \
  --option internal-iterations-timeout=2h30m \
  --option continue-as-new-after-iterations=5 \
  --option sleep-time=100ms \
  --do-not-register-search-attributes
```

#### Weekly configuration

Same as nightly but with `--duration 24h`.

### 6. Teardown

```bash
pkill -f temporal-server || true
sleep 2
podman kill -a 2>/dev/null
podman pod rm -f -a 2>/dev/null
podman rm -f -a 2>/dev/null
```

### 7. Before/after benchmark workflow

To benchmark a specific commit's impact:

1. Check out the baseline (without the commit)
2. Build server, run Cassandra benchmark (steps 0-6), record result
3. Teardown (step 6)
4. Run ScyllaDB benchmark (steps 1-6), record result
5. Teardown (step 6)
6. Apply the commit, rebuild server
7. Run Cassandra benchmark (steps 1-6), record result
8. Teardown (step 6)
9. Run ScyllaDB benchmark (steps 1-6), record result
10. Teardown (step 6)

Each run must start from a clean database — never reuse data from a previous
benchmark run, as leftover data can skew results.

## Collecting profiles

The server exposes pprof on `127.0.0.1:7936` (configurable). Block and mutex
profiling are enabled via `blockProfileRate` and `mutexProfileFraction` in the
development config files.

Capture profiles during a benchmark run:

```bash
# CPU (30 seconds)
curl -o cpu.pb.gz "http://localhost:7936/debug/pprof/profile?seconds=30"
# Heap / allocations
curl -o heap.pb.gz "http://localhost:7936/debug/pprof/heap"
curl -o allocs.pb.gz "http://localhost:7936/debug/pprof/allocs"
# Block (I/O and channel waits)
curl -o block.pb.gz "http://localhost:7936/debug/pprof/block"
# Mutex contention
curl -o mutex.pb.gz "http://localhost:7936/debug/pprof/mutex"
# Goroutine dump
curl -o goroutine.txt "http://localhost:7936/debug/pprof/goroutine?debug=2"
```

Analyze with `go tool pprof`:

```bash
go tool pprof -http=:8080 cpu.pb.gz
```

## Collecting results

Omes prints throughput metrics to stdout. Temporal server exposes Prometheus
metrics on `127.0.0.1:8000` (configurable in the development YAML).

Key metrics to compare:

- `persistence_latency` — per-operation Cassandra/ScyllaDB latency
- `history_size` — workflow history event throughput
- omes reported workflows/second

Scrape Prometheus metrics for offline analysis:

```bash
curl -s http://127.0.0.1:8000/metrics > bench_results.txt
```

## Troubleshooting

### BadSearchAttributes errors at benchmark start

If you see `BadSearchAttributes: search attribute OmesExecutionID is not defined`
errors on `StartChildWorkflowCommand` during the first few seconds:

- **Cause:** History service shard caches haven't refreshed with the new SA data.
  The `verify_sa.go` tool only validates the frontend service cache — history
  shards have separate caches.
- **Fix:** Restart the Temporal server after SA registration (see step 4).
  This forces all shards to load fresh data on startup.
- **Note:** This is more pronounced with ScyllaDB than Cassandra, likely due to
  slower namespace data reads during cache population.

### SA payload encoding

Search attribute payloads must use `json/plain` encoding, not `plain/plain`.
The data must be JSON-marshaled (e.g. `json.Marshal("value")`), and the
metadata encoding must be `json/plain`. The helper scripts in `tools/benchmark/`
handle this correctly.

### Podman cleanup issues

`podman pod rm -f -a` sometimes fails silently. Always verify with `podman ps -a`
and `podman pod ls` after cleanup. Run the kill/rm commands multiple times if
needed.

### Typical throughput numbers

On i7-1270P with the pinning layout above, server CPUs (0-3) run >80% utilized
while DB CPUs (4-7) are <65% — the server is the throughput bottleneck.

Prior results with mc150/5min for reference (your numbers may vary):
- Cassandra: ~190-200 iterations
- ScyllaDB: ~170-250 iterations
