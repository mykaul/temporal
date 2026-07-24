# temporal-benchmark

Run reproducible throughput benchmarks comparing temporal-server builds using
omes `throughput_stress` against ScyllaDB/Cassandra + Elasticsearch.

## Prerequisites

- Podman with compose support, Go 1.26+, `scylladb/scylla:2026.2` pulled
- `temporalio/omes` built (binary at `$OMES_DIR/omes-bin`)
- CPU lockdown script at `$PERF_SCRIPT` (save/restore freq, governor, turbo)
- tmpfs at `$TMPFS` (default: `/mnt/tmpfs-scylla`, 16G min, `chmod 777`)
- Temporal CLI binary at `$TEMPORAL_CLI` (`temporal` CLI, download from
  `https://github.com/temporalio/cli/releases/latest/download/temporal_cli_<ver>_linux_amd64.tar.gz`
  — check release API for exact asset name/version first)
- `cqlsh` on `PATH`, `temporal-cassandra-tool` and `temporal-elasticsearch-tool` built
- `docker-compose.*.pinned.yml` files overlaid onto base compose files
- `config/dynamicconfig/development-cass.yaml` must set
  `system.clusterMetadataRefreshInterval: 1s` and
  `system.forceSearchAttributesCacheRefreshOnRead: true`
  (without these overrides, workflow starts referencing a newly-added
  search attribute fail unpredictably after registration due to stale
  in-memory frontend cache)

## Configuration (set per-host, source before starting)

```bash
# Required: all must be set
REPO_ROOT=/home/ykaul/github/temporal       # this temporal checkout
TEMPORAL_CLI=$(command -v temporal)         # real CLI, not tctl (see Prerequisites)
OMES_DIR=/tmp/omes-bench                    # directory containing omes-bin
PERF_SCRIPT=~/perf_set.sh                   # CPU lockdown script
TMPFS=/mnt/tmpfs-scylla                     # tmpfs mount for ScyllaDB data

# Optional (host-specific CPU topology)
SCYLLA_CPUSET=0-2        # cores for ScyllaDB (e.g. P-cores on hybrid, any on non-hybrid)
SCYLLA_SMP=3             # --smp must equal CPU count in cpuset
SCYLLA_MEM=4G
SERVER_CPUSET=3-7        # cores for temporal-server
ES_CPUSET=8              # cores for Elasticsearch
OMES_CPUSET=9-15         # cores for omes + worker

# Host-specific CPU lock frequencies (kHz) — pick a value each core sustains
# without thermal throttling. Example below (2.2GHz) is for this host only;
# check `cpupower frequency-info` per core type on a new host.
CPU_LOCK_FREQ_KHZ=2200000

# C-states with exit latency >= this (us) get disabled during the lock window
# (POLL=0us, C1E=2us typically safe to keep; deeper states like C6/C8/C10
# have 170-230us+ latency and cause frequency-ramp variance). Check with
# `cpupower idle-info` per host — state latencies vary by CPU model.
CSTATE_LATENCY_THRESHOLD_US=4

# Benchmark run shape
BENCH_DURATION=3m           # wall-clock time to start new iterations for
BENCH_DURATION_SECONDS=180  # same value in seconds, for iter/s math below

# Re-calibrate per host: not portable. Double from a guess until short
# 1m runs start showing timeout cascades ("context deadline exceeded"),
# then back off. This host (16 cores): 200 clean, 300 couldn't drain in 90s.
MAX_CONCURRENT=200
LABEL=main                  # binary label for this run, e.g. main/pr1/pr2
CYCLE=1                     # cycle number, for log/result file naming
```

For non-hybrid CPUs: Omit HT siblings after dividing. The 4/4/1/7
(ScyllaDB/server/ES/omes) split above is unvalidated as an ideal ratio —
it's just what this host used. Actual measurement at that split: ScyllaDB
43-72% busy, server 82-86% busy (see Known Limitations) — ScyllaDB had
headroom to spare even at only 4 of 16 cores, so there's no evidence it
needs anywhere near half. Start proportional to core count and adjust
based on per-core utilization sampling (Known Limitations), not by guessing.

## Procedure

All steps below run with CWD=`$REPO_ROOT` unless otherwise noted.

In each cycle, run every binary exactly once. Interleave to cancel drift:  
`B → PR1 → PR2 → B → PR1 → PR2 → B → PR1 → PR2`

**IMPORTANT:** ALL containers (not just ScyllaDB/ES — everything) are torn
down AFTER EACH CYCLE (every 3 binaries), via `podman rm -f -a`.
Between binaries within a cycle, only the temporal-server is killed and replaced.
**CPU/swap state is saved and locked at the START of each cycle, restored at
the END of each cycle** — save right before benchmarking, restore right after.
This gives full speed for recompiling binaries between cycles (e.g. testing a
new PR revision) without leaving the machine locked while idle.

### 1. Lock CPU + disable swap (repeat at the START of EVERY cycle)

```bash
source "$PERF_SCRIPT"
cpu_save_state
cpu_enable_performance_cpupower_state
cpu_disable_intel_turbo_boost
cpu_set_min_frequencies "$CPU_LOCK_FREQ_KHZ"
cpu_set_max_frequencies "$CPU_LOCK_FREQ_KHZ"

# min=max is only a ceiling under intel_pstate active/HWP — idle cores still
# sag toward the frequency floor between bursts of work, adding run-to-run
# variance. Disable deep C-states (keep POLL + C1E; C1E's exit latency is
# low enough not to matter) to hold cores closer to the locked frequency.
sudo cpupower idle-set -D "$CSTATE_LATENCY_THRESHOLD_US" 2>/dev/null || true

# Save swap state and disable. swapoff alone is NOT enough: podman's
# container teardown (rm -fa) triggers a systemd slice/generator reload that
# re-arms zram-generator swap units. Mask them too.
SWAP_STATE=$(swapon --show --noheadings 2>/dev/null | wc -l)
echo "$SWAP_STATE" > /tmp/bench-swap-state
systemctl list-units --type=swap --no-legend 2>/dev/null | awk '{print $1}' > /tmp/bench-swap-units
sudo swapoff -a 2>/dev/null || true
sudo systemctl mask --now $(cat /tmp/bench-swap-units) 2>/dev/null || true
```

### 2. Start infrastructure

```bash
# Clean + start ScyllaDB
sudo rm -rf "$TMPFS"/*
sudo chmod 777 "$TMPFS"
podman compose -f develop/docker-compose/docker-compose.scylla.yml \
              -f develop/docker-compose/docker-compose.scylla.pinned.yml up -d scylladb
for i in $(seq 45); do
  cqlsh 127.0.0.1 9042 -e 'describe cluster' 2>/dev/null && break
  sleep 2
done

# Clean + start ES
podman rm -f temporal-dev-elasticsearch 2>/dev/null || true
podman compose -f develop/docker-compose/docker-compose.yml \
              -f develop/docker-compose/docker-compose.pinned.yml \
              -f develop/docker-compose/docker-compose.es.yml up -d elasticsearch
for i in $(seq 45); do
  curl -sf http://127.0.0.1:9200 >/dev/null && break
  sleep 2
done
```

### 3. Install schemas

```bash
cqlsh 127.0.0.1 9042 -e "DROP KEYSPACE IF EXISTS temporal;"
cqlsh 127.0.0.1 9042 -e \
  "CREATE KEYSPACE temporal WITH replication = {'class': 'NetworkTopologyStrategy', 'replication_factor': '1'};"
./temporal-cassandra-tool setup-schema -f schema/cassandra/temporal/schema.cql --disable-versioning
./temporal-cassandra-tool setup-schema -v 0.0
cqlsh 127.0.0.1 9042 -e \
  "UPDATE temporal.schema_version SET curr_version = '1.13', min_compatible_version = '1.13' WHERE keyspace_name = 'temporal';"
./temporal-elasticsearch-tool --endpoint http://127.0.0.1:9200 setup-schema
./temporal-elasticsearch-tool --endpoint http://127.0.0.1:9200 create-index --index temporal_visibility_v1_dev
./temporal-elasticsearch-tool --endpoint http://127.0.0.1:9200 update-schema --index temporal_visibility_v1_dev
```

### 4. Run one binary (repeat for each binary in the cycle)

Set `LABEL` (e.g. `main`/`pr1`/`pr2`) and `CYCLE` (1/2/3) for this run before
starting — used only for log/result file naming.

```bash
# Server should already be dead here (killed at the end of the previous
# binary's run, or never started this cycle) — belt-and-suspenders only.
pkill -f "temporal-server-" 2>/dev/null || true
sleep 2

# Start server (--config works with relative path; absolute triggers path.Join bug)
cd "$REPO_ROOT"
nohup taskset -c "$SERVER_CPUSET" "/tmp/temporal-server-$LABEL" \
  --config config --env development-cass-es --allow-no-auth start \
  > "/tmp/server-$LABEL-c$CYCLE.log" 2>&1 &
disown
for i in $(seq 30); do
  ss -tlnp 2>/dev/null | grep -q 7233 && break
  sleep 1
done
sleep 3

# Register namespace (idempotent)
$TEMPORAL_CLI operator namespace create --namespace default --address 127.0.0.1:7233 2>&1 || true

# MANDATORY: register search attribute used by throughput_stress scenario.
# See Prerequisites for the root cause (cluster-metadata cache refresh) this
# dynamic config override fixes. Poll-verify kept as defense-in-depth in case
# the override isn't applied on a given host.
$TEMPORAL_CLI operator search-attribute create --name OmesExecutionID --type Keyword \
  --namespace default --address 127.0.0.1:7233 2>&1 || true
for i in $(seq 30); do
  $TEMPORAL_CLI operator search-attribute list --namespace default \
    --address 127.0.0.1:7233 2>/dev/null | grep -q OmesExecutionID && break
  sleep 1
done
sleep 10

# Benchmark run
"$OMES_DIR/omes-bin" run-scenario-with-worker \
  --scenario throughput_stress --language go \
  --duration "$BENCH_DURATION" --max-concurrent "$MAX_CONCURRENT" \
  --option internal-iterations=10 \
  --option continue-as-new-after-iterations=3 \
  --option sleep-time=1ms 2>&1 | tee "/tmp/bench-$LABEL-c$CYCLE.log"
# iter/s = "Total iterations completed" / $BENCH_DURATION_SECONDS

# Kill server before next binary
pkill -f "temporal-server-" 2>/dev/null || true
sleep 3
```

### 5. End of cycle: teardown containers + restore CPU/swap (repeat after EVERY cycle)

```bash
pkill -f "temporal-server-" 2>/dev/null || true
sleep 2
# Nuke ALL containers and their networks — clean slate between cycles
podman rm -f -a 2>/dev/null || true
podman network prune -f 2>/dev/null || true
sudo rm -rf "$TMPFS"/*
sudo chmod 777 "$TMPFS"

# Restore CPU FIRST — always undo the performance lock before anything else
cpu_restore_state
sudo cpupower idle-set -E 2>/dev/null || true

# Unmask + re-enable swap if it was on
sudo systemctl unmask $(cat /tmp/bench-swap-units) 2>/dev/null || true
if [ "$(cat /tmp/bench-swap-state 2>/dev/null)" -gt 0 ] 2>/dev/null; then sudo swapon -a 2>/dev/null || true; fi
```

Loop back to step 1 for the next cycle (recompile binaries here if needed —
CPU is at full speed since it was just restored).

## Important Rules

1. **MUST nuke ALL containers after every cycle** — `podman rm -f -a` (not just ScyllaDB/ES by name) and `rm -rf "$TMPFS"/*`. Residual data/compaction skews results
2. **Server must use `nohup` + `disown`** to survive shell exit
3. **Always verify port 7233 before omes** — loop until `ss -tlnp | grep 7233`
4. **Interleave binaries** within each cycle to cancel temporal drift
5. **3 cycles minimum** for standard error estimation
6. **CPU/swap: save+lock at the start of every cycle, restore at the end of every cycle** — never left locked while idle between cycles; allows full-speed recompilation between cycles
7. **Do NOT use `podman compose ... down -v`** — hangs with podman; use `podman rm -f` + `podman network rm -f` instead
8. **Always `sudo rm -rf "$TMPFS"/*`** — files owned by scylla user (UID inside container), regular `rm` fails
9. **`swapoff -a` alone is insufficient** — mask zram-generator swap units too, or podman's container teardown silently re-arms them mid-run
10. **Disable deep C-states, not just frequency limits** — `cpupower idle-set -D <latency>` alongside the min=max frequency lock; skipping this leaves a real source of run-to-run variance (see Known Limitations)
11. **`MAX_CONCURRENT` must be re-calibrated per host** — see Configuration section; it is a measured capacity limit for one specific machine, not a portable constant

## Known Limitations

- **`intel_pstate` in `active` mode**: min=max frequency is only a ceiling — HWP autonomous idle behavior lets `scaling_cur_freq` sag toward the floor between bursts of work even with the ceiling set, independent of C-states. Disabling deep C-states (Rule 10) mitigates but may not eliminate this.
- **Bottleneck not isolated (this host)**: at `MAX_CONCURRENT=200`, server cores hit 82-86% busy, ScyllaDB 43-72%, ES/omes 32-42% — none saturated, yet higher concurrency collapses (timeouts) instead of degrading gracefully. Likely queueing/lock contention, not raw CPU/ScyllaDB capacity, but unconfirmed.
- **Duration/iteration-count untuned**: `3m` duration + `internal-iterations=10` are working defaults, not validated as enough to detect small per-write latency wins over run-to-run noise. If binaries converge, try longer duration or higher `internal-iterations` before concluding "no difference."
- **Single-node ScyllaDB**: optimizations needing multiple nodes (e.g. hedged/speculative reads racing a second host) can't be validated here — the query plan only ever has one host, regardless of workload tuning.

## Binaries (build once)

Each `/tmp/temporal-build-{main,pr1,pr2}` directory is a separate git
worktree/checkout at the revision under test.

```bash
mkdir -p /tmp/temporal-build-{main,pr1,pr2}
cd /tmp/temporal-build-main && go build -tags disable_grpc_modules -o /tmp/temporal-server-main ./cmd/server
cd /tmp/temporal-build-pr1  && go build -tags disable_grpc_modules -o /tmp/temporal-server-pr1  ./cmd/server
cd /tmp/temporal-build-pr2  && go build -tags disable_grpc_modules -o /tmp/temporal-server-pr2  ./cmd/server
```

Management tools (from repo root):
```bash
make temporal-cassandra-tool temporal-elasticsearch-tool
```

## Pinned Compose Files

- `docker-compose.scylla.pinned.yml`: add `cpuset: "$SCYLLA_CPUSET"`, args `--smp $SCYLLA_SMP --memory $SCYLLA_MEM`
- `docker-compose.pinned.yml`: add `cpuset: "$ES_CPUSET"` under `elasticsearch.deploy.resources`

## Results Table

Generic template — one column per binary under test, one row per cycle.

| Cycle | Binary A | Binary B | Binary C |
|-------|:--------:|:--------:|:--------:|
| 1 | | | |
| 2 | | | |
| 3 | | | |
| **Avg** | | | |

Record as `iterations (iter/s)` per cell. Discard/re-run any cycle recorded
before a methodology fix (e.g. dynamic config or C-state changes above) —
mixing pre- and post-fix cycles in one average is invalid.
