# temporal-benchmark

Run reproducible throughput benchmarks comparing temporal-server builds using
omes `throughput_stress` against ScyllaDB + Elasticsearch.

## Prerequisites

- Podman with compose support, Go 1.26+, `scylladb/scylla:2026.2` pulled
- `temporalio/omes` at `$OMES_DIR` (default: `/home/ykaul/github/omes`)
- CPU lockdown script at `$PERF_SCRIPT` (save/restore freq, governor, turbo)
- tmpfs at `$TMPFS` (default: `/mnt/tmpfs-scylla`, 16G min, `chmod 777`)
- Temporal CLI binary at `$TEMPORAL_CLI`
- `cqlsh` on `PATH`, `temporal-cassandra-tool` and `temporal-elasticsearch-tool` built
- `docker-compose.*.pinned.yml` files overlaid onto base compose files

## Configuration (set per-host)

```bash
SCYLLA_CPUSET=0-3        # cores for ScyllaDB (e.g. P-cores on hybrid, any on non-hybrid)
SCYLLA_SMP=4             # --smp must equal CPU count in cpuset
SCYLLA_MEM=4G
SERVER_CPUSET=4-7        # cores for temporal-server
ES_CPUSET=8              # cores for Elasticsearch
OMES_CPUSET=9-15         # cores for omes + worker
```

For non-hybrid CPUs: Omit HT siblings after dividing. If all cores are equal,
give ScyllaDB half the cores and split the rest between server, ES, omes.

## Dynamic Config

Add these to `config/dynamicconfig/development-cass.yaml`:

```yaml
history.shardIOConcurrency:
  - value: 4096
matching.rps:
  - value: 100000
    constraints: {}
matching.namespaceRPS:
  - value: 100000
    constraints: {}
```

**IMPORTANT:** The YAML list items (`- value:`) and their continuation lines
(`constraints: {}`) must be at the **same indentation level**. Bad indentation
causes the entire file to silently fall back to defaults.

```yaml
# BAD — constraints at wrong level, invalid YAML:
matching.rps:
  - value: 100000
  constraints: {}

# GOOD — value and constraints are siblings:
matching.rps:
  - value: 100000
    constraints: {}
```

If the server is started from a **copy** of the repo (e.g. `/tmp/perf-merge/`),
the config at `$COPY_DIR/config/dynamicconfig/development-cass.yaml` is used,
NOT the source repo. Always `diff` or `cp` to keep them in sync.

## Procedure

In each cycle, run every binary exactly once. Interleave to cancel drift:  
`B → PR1 → PR2 → B → PR1 → PR2 → B → PR1 → PR2`

**IMPORTANT:** ScyllaDB + ES are torn down AFTER EACH CYCLE (every 3 binaries).
Between binaries within a cycle, only the temporal-server is killed and replaced.

### 0. Lock CPU

```bash
source "$PERF_SCRIPT"
cpu_save_state
cpu_enable_performance_cpupower_state
cpu_disable_intel_turbo_boost
cpu_set_min_frequencies 2200000
cpu_set_max_frequencies 2200000
```

### 1. Start infrastructure

```bash
# Clean + start ScyllaDB
rm -rf "$TMPFS"/*
podman compose -f develop/docker-compose/docker-compose.scylla.yml \
              -f develop/docker-compose/docker-compose.scylla.pinned.yml up -d scylladb
# Wait: cqlsh 127.0.0.1 9042 -e 'describe cluster' (2s loops, 90s max)

# Clean + start ES
podman compose -f develop/docker-compose/docker-compose.yml \
              -f develop/docker-compose/docker-compose.pinned.yml \
              -f develop/docker-compose/docker-compose.es.yml \
              down -v
podman compose -f develop/docker-compose/docker-compose.yml \
              -f develop/docker-compose/docker-compose.pinned.yml \
              -f develop/docker-compose/docker-compose.es.yml up -d elasticsearch
# Wait: curl -s http://127.0.0.1:9200 (2s loops, 90s max)
```

### 2. Install schemas

```bash
cqlsh 127.0.0.1 9042 -e "DROP KEYSPACE IF EXISTS temporal;"
cqlsh 127.0.0.1 9042 -e \
  "CREATE KEYSPACE temporal WITH replication = {'class': 'NetworkTopologyStrategy', 'replication_factor': '1'} AND tablets = {'enabled': false};"
./temporal-cassandra-tool setup-schema -f schema/cassandra/temporal/schema.cql --disable-versioning
./temporal-cassandra-tool setup-schema -v 0.0
cqlsh 127.0.0.1 9042 -e \
  "UPDATE temporal.schema_version SET curr_version = '1.13', min_compatible_version = '1.13' WHERE keyspace_name = 'temporal';"
./temporal-elasticsearch-tool --endpoint http://127.0.0.1:9200 setup-schema
./temporal-elasticsearch-tool --endpoint http://127.0.0.1:9200 create-index --index temporal_visibility_v1_dev
./temporal-elasticsearch-tool --endpoint http://127.0.0.1:9200 update-schema --index temporal_visibility_v1_dev
```

### 3. Run one binary (repeat for each binary in the cycle)

```bash
# Start server (--config works with relative path; absolute triggers path.Join bug)
cd /home/ykaul/github/temporal
taskset -c "$SERVER_CPUSET" /path/to/server-binary \
  --config config --env development-cass-es --allow-no-auth start &
# Wait: ss -tlnp | grep 7233 (1s loops, 30s max)

# Register namespace + search attribute (idempotent)
$TEMPORAL_CLI operator namespace create default --retention 24h
$TEMPORAL_CLI operator search-attribute create --name OmesExecutionID --type Keyword

# Smoke-test that omes can reach server
$OMES_DIR/omes-bin run-scenario-with-worker \
  --scenario throughput_stress --language go --duration 1m \
  --max-concurrent 1 --option internal-iterations=1 \
  --option continue-as-new-after-iterations=0 --option sleep-time=1ms 2>&1 \
  | grep -q "Total iterations completed"

# 3-minute benchmark
$OMES_DIR/omes-bin run-scenario-with-worker \
  --scenario throughput_stress --language go \
  --duration 3m --max-concurrent 200 \
  --option internal-iterations=10 \
  --option continue-as-new-after-iterations=3 \
  --option sleep-time=1ms 2>&1 | tee /tmp/bench-{LABEL}-c{CYCLE}.log
# iter/s = "iterations completed" / 180

# Teardown
pkill -f "temporal-server-.*--env" 2>/dev/null || true
sleep 3
```

To collect CPU/block/mutex profiles during a benchmark run, fetch from the
pprof port (configured in `development-cass-es.yaml` under `pprof.port`, default
7936):

```bash
curl -s -o /tmp/cpu.pprof "http://localhost:7936/debug/pprof/profile?seconds=25"
curl -s -o /tmp/block.pprof "http://localhost:7936/debug/pprof/block?seconds=25"
curl -s -o /tmp/mutex.pprof "http://localhost:7936/debug/pprof/mutex?seconds=25"
```
(Note: path is `/debug/pprof/`, not `/pprof/`.)

### 4. End-of-cycle teardown (ScyllaDB + ES, run after each full cycle)

```bash
pkill -f "temporal-server-" 2>/dev/null || true
podman compose -f develop/docker-compose/docker-compose.scylla.yml \
              -f develop/docker-compose/docker-compose.scylla.pinned.yml down -v
podman compose -f develop/docker-compose/docker-compose.yml \
              -f develop/docker-compose/docker-compose.pinned.yml \
              -f develop/docker-compose/docker-compose.es.yml down -v
rm -rf "$TMPFS"/*

# Restore CPU to free state (undo the benchmark lock)
cpu_restore_state
```

## Important Rules

1. **MUST teardown ScyllaDB + ES after every cycle** — always `down -v` and re-create between cycles. Never skip this; residual data/compaction skews results
2. **Server must use `nohup` + `disown` or `setsid`** to survive shell exit
3. **Always verify port 7233 before omes** — `ss -tlnp | grep 7233`
4. **Interleave binaries** within each cycle to cancel temporal drift
5. **3 cycles minimum** for standard error estimation
6. CPU restore: `cpu_restore_state` at the very end
7. **Validate the dynamic config YAML** before starting the server; use a linter or read the file. A single indentation error silences all overrides and falls back to defaults.

## Binaries (build once)

```bash
mkdir -p /tmp/temporal-build-{main,pr1,pr2}
cd /tmp/temporal-build-main && go build -tags disable_grpc_modules, -o /tmp/temporal-server-main  ./cmd/server
cd /tmp/temporal-build-pr1  && go build -tags disable_grpc_modules, -o /tmp/temporal-server-pr1  ./cmd/server
cd /tmp/temporal-build-pr2  && go build -tags disable_grpc_modules, -o /tmp/temporal-server-pr2  ./cmd/server
```

Management tools (from repo root):
```bash
make temporal-cassandra-tool temporal-elasticsearch-tool
```

## Pinned Compose Files

- `docker-compose.scylla.pinned.yml`: add `cpuset: "$SCYLLA_CPUSET"`, args `--smp $SCYLLA_SMP --memory $SCYLLA_MEM`
- `docker-compose.pinned.yml`: add `cpuset: "$ES_CPUSET"` under `elasticsearch.deploy.resources`

## Diagnosis and Key Findings

### `history.shardIOConcurrency` default (1) is a universal bottleneck

The default value of `history.shardIOConcurrency` is **1** — every shard processes
persistence operations serially. This is a universal bottleneck affecting **all
branches equally** (main, PR1, PR2). With `shardIOConcurrency=1`, throughput
caps at ~32 iter/s (ii=1). Raising it to 4096 raises throughput to **~43 iter/s**
(+33%).

The results in the table below were all collected with `shardIOConcurrency=1`
(the default). No branch-specific config changes were made. The 15–24% variation
between Baseline (2.22) and the PRs (2.69–2.75) reflects genuine per-iteration
differences (internal-iterations=10 obscures workflow-level throughput), but all
share the same ~32 iter/s ceiling at the workflow-execution level.

### GC overhead is ~30%

CPU profiles consistently show GC consuming ~30% of CPU time. This is a
cross-cutting concern independent of the PRs. Reducing allocation or improving
GC efficiency would benefit all branches.

### No lock contention

Block and mutex profiles are empty — zero contention on any lock, mutex, or
semaphore during the steady-state workload.

### ScyllaDB is lightly loaded

ScyllaDB uses ~8% of its allocated CPU. The slow path is not the database.

### Interceptor chain overhead

The gRPC interceptor chain (TelemetryInterceptor,
NamespaceRateLimitInterceptorImpl, RateLimitInterceptor, etc.) accounts for
~26% cumulative CPU. While this is wrapper overhead, significant portions are
inherent to the gRPC framework.

## Results Table

| Cycle | Baseline (main) | PR1 (pipeline-histo) | PR2 (speculative-exec) |
|-------|:---------------:|:--------------------:|:----------------------:|
| 1 | 2.22 iter/s | 2.75 iter/s | 2.69 iter/s |
| 2 | | | |
| 3 | | | |
| **Avg** | | | |
