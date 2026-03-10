# CQL Persistence Benchmarking

Reproducible benchmarks for the Cassandra/ScyllaDB persistence layer using
[omes](https://github.com/temporalio/omes) `throughput_stress` scenario.

## Prerequisites

- Docker or Podman with compose support
- Go (version per `go.mod`)
- Built `temporal-server`, `temporal-cassandra-tool`, and `temporal-elasticsearch-tool` binaries

Build omes:

```bash
git clone https://github.com/temporalio/omes.git /tmp/omes-bench/omes
cd /tmp/omes-bench/omes
go build -o /tmp/omes-bench/omes-bin ./cmd/omes
```

## CPU-Pinned Containers

The `docker-compose.pinned.yml` and `docker-compose.scylla.pinned.yml` overlay
files pin database and Elasticsearch containers to dedicated CPU cores via
`cpuset`. This eliminates OS scheduler noise and makes results comparable across
runs. Without pinning, container CPU time fluctuates with host load and produces
unreliable numbers.

Default pinning (adjust to match your hardware):

| Service       | cpuset | File                                  |
|---------------|--------|---------------------------------------|
| Cassandra     | 0,1    | `docker-compose.pinned.yml`           |
| Elasticsearch | 6,7    | `docker-compose.pinned.yml`           |
| ScyllaDB      | 0,1    | `docker-compose.scylla.pinned.yml`    |

## Running a Benchmark

### 1. Start containers

**Cassandra + Elasticsearch:**

```bash
cd develop/docker-compose
podman compose \
  -f docker-compose.yml \
  -f docker-compose.pinned.yml \
  up -d cassandra elasticsearch
```

**ScyllaDB + Elasticsearch:**

ScyllaDB uses host networking so the gocql shard-aware port is directly
reachable without NAT (port-mapping breaks shard-aware connection routing).
Elasticsearch still runs on the bridge network.

```bash
cd develop/docker-compose
podman compose -f docker-compose.scylla.yml -f docker-compose.scylla.pinned.yml up -d scylladb
podman compose -f docker-compose.yml -f docker-compose.pinned.yml up -d elasticsearch
```

Wait for the database to become healthy before proceeding:

```bash
# Cassandra
until docker exec temporal-dev-cassandra cqlsh -e 'describe cluster' 2>/dev/null; do sleep 2; done

# ScyllaDB (host networking, CQL on default port 9042, shard-aware on 19042)
until cqlsh 127.0.0.1 9042 -e 'describe cluster' 2>/dev/null; do sleep 2; done
```

### 2. Install CQL schema

**Cassandra** can use the standard Makefile target:

```bash
make install-schema-cass-es
```

**ScyllaDB** requires the versioned migration path — the `setup-schema -v 0.0`
plus `update-schema -d versioned/` sequence. Do **not** use `setup-schema -f
schema.cql` because that creates the full schema at version 0.0, and then the
versioned migrations fail with column-already-exists errors.

```bash
make install-schema-scylla-es
```

### 3. Install Elasticsearch schema

If setting up ES manually (outside the Makefile targets), the full three-step
sequence is required. Skipping `setup-schema` causes visibility queries to
return zero results.

```bash
./temporal-elasticsearch-tool setup-schema --version v1 \
  --url http://127.0.0.1:9200

./temporal-elasticsearch-tool create-index \
  --url http://127.0.0.1:9200

./temporal-elasticsearch-tool update-schema --index temporal_visibility_v1 \
  --url http://127.0.0.1:9200 \
  --schema-dir schema/elasticsearch/visibility
```

### 4. Start the server

```bash
# Cassandra
./temporal-server --env development-cass-es --allow-no-auth start

# ScyllaDB (--root / so the absolute config path is resolved correctly)
./temporal-server --root / --env development-scylla-es \
  --config /path/to/temporal/config start
```

### 5. Register namespace and search attributes

The `default` namespace must exist, and the `OmesExecutionID` search attribute
must be registered before omes workflows can run.

```bash
# Register namespace (via Temporal CLI or gRPC helper)
temporal operator namespace create -n default

# Register the OmesExecutionID search attribute
temporal operator search-attribute create \
  -n default --name OmesExecutionID --type Keyword
```

**Important:** After registering the search attribute, wait at least 90 seconds
before starting the benchmark. The namespace cache needs time to propagate the
new attribute. Without this delay, workflows fail in a tight loop with
"OmesExecutionID is not defined", which can overload the database with retries
and cause cascading CQL timeouts — especially on ScyllaDB.

### 6. Run the benchmark

```bash
/tmp/omes-bench/omes-bin run-scenario-with-worker \
  --scenario throughput_stress \
  --language go \
  --server-address 127.0.0.1:7233 \
  --duration 10m \
  --run-id "$(date +%Y%m%d-%H%M%S)" \
  --max-concurrent 5 \
  --option internal-iterations=10
```

Redirect server and omes output to separate log files for post-run analysis.
Add `--log-level debug` to the omes command to see per-iteration progress;
iteration logs use `Debugf` and are invisible at the default `info` level.

### 7. Teardown

```bash
cd develop/docker-compose

# Cassandra
podman compose -f docker-compose.yml -f docker-compose.pinned.yml down -v

# ScyllaDB (separate commands since ScyllaDB uses host networking)
podman compose -f docker-compose.scylla.yml -f docker-compose.scylla.pinned.yml down -v
podman compose -f docker-compose.yml -f docker-compose.pinned.yml down -v
```

Always use `-v` to remove volumes so the next run starts with a clean database.

## Interpreting Results

Key metrics from omes output and server logs:

| Metric | Where | What it means |
|--------|-------|---------------|
| Iterations completed | omes stdout | Primary throughput number; higher is better |
| Total workflows | omes stdout | iterations × workflows-per-iteration |
| "Workflow task not found" | omes stderr | Stale workflow tasks due to slow persistence writes |
| Server warn/error count | server log | `grep -c '"level":"warn\|error"'`; lower is better |

When comparing configurations, run each at least twice to confirm the numbers
are stable within ±5%.

## Benchmark Results

Results from `throughput_stress` 10-minute runs with `--max-concurrent 5
--option internal-iterations=10`, databases pinned to CPUs 0-1, ES on 6-7:

| Configuration | Iterations | Total workflows | Server errors |
|---|---|---|---|
| gocql + Cassandra 5.0 (baseline) | 150 | 2,100 | 92 |
| scylladb/gocql + Cassandra 5.0 | 150 | 2,100 | 70 |
| scylladb/gocql + ScyllaDB 2025.4 (no fixes) | 65–73 | 910–1,022 | ~2,000 |
| scylladb/gocql + ScyllaDB 2026.1-rc3 (no fixes) | 68–71 | 952–994 | ~2,000 |
| Cassandra 5.0 + idempotency fix | 145 | 2,030 | 18 |
| ScyllaDB + idempotency fix only | 78 | 1,092 | ~215 |
| **ScyllaDB + all fixes + speedup flags** | **150** | **2,100** | **0** |

The "all fixes" configuration includes: idempotent query annotations for
driver-level retry, explicit `RetryPolicy{NumRetries: 5}`, 30s write timeout,
shard-aware connections (host networking), and ScyllaDB speedup flags
(`unsafe-bypass-fsync`, `kernel-page-cache`, `commitlog-use-o-dsync=0`).

The speedup flags trade durability for throughput and are appropriate for
benchmarks and development. In production, evaluate each flag against your
durability requirements.

## Known Issues

- **ES visibility verification hangs:** The omes post-run verification step that
  counts workflows via ES visibility may hang at zero. This is an ES
  configuration issue in the dev setup. Kill the process after the "Run
  completed" line appears in stdout.

- **ScyllaDB LWT/Paxos overhead:** Every mutable-state write goes through
  `MapExecuteBatchCAS` with mixed LWT and non-LWT statements, forcing the entire
  batch through Paxos. Without the speedup flags, ScyllaDB achieves ~50% of
  Cassandra 5.0 throughput. The speedup flags eliminate the I/O bottleneck
  (fsync, commitlog sync) and bring ScyllaDB to full parity.
