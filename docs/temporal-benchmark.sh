#!/usr/bin/env bash
set -euo pipefail

# temporal-benchmark.sh — Single-cycle throughput benchmark for temporal-server builds.
# One invocation = one complete cycle: lock CPU → start infra → install schemas →
# run all binaries (interleaved) → teardown → restore CPU → print summary.
#
# Run multiple times for multiple cycles. Between invocations, verify clean state
# (no containers, no stray processes) and review cumulative results.
#
# Usage:
#   ./temporal-benchmark.sh [cycle_number] [binary1,binary2,...]
#
#   cycle_number — cycle index for log naming (default: 1)
#   binaries     — comma-separated list of labels (default: main,pr1)
#
# Prerequisites: see docs/benchmark.md "Prerequisites" section.
# Config vars below must be set in the environment before running.

# ── Configuration (override via environment) ──────────────────────────────
REPO_ROOT="${REPO_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
TEMPORAL_CLI="${TEMPORAL_CLI:-temporal}"
OMES_DIR="${OMES_DIR:-/tmp/omes-bench}"
PERF_SCRIPT="${PERF_SCRIPT:-$HOME/perf_set.sh}"
TMPFS="${TMPFS:-/mnt/tmpfs-scylla}"

SCYLLA_CPUSET="${SCYLLA_CPUSET:-0-2}"
SCYLLA_SMP="${SCYLLA_SMP:-3}"
SCYLLA_MEM="${SCYLLA_MEM:-4G}"
SERVER_CPUSET="${SERVER_CPUSET:-3-7}"
ES_CPUSET="${ES_CPUSET:-8}"
OMES_CPUSET="${OMES_CPUSET:-9-15}"

CPU_LOCK_FREQ_KHZ="${CPU_LOCK_FREQ_KHZ:-2200000}"
CSTATE_LATENCY_THRESHOLD_US="${CSTATE_LATENCY_THRESHOLD_US:-4}"

BENCH_DURATION="${BENCH_DURATION:-3m}"
BENCH_DURATION_SECONDS="${BENCH_DURATION_SECONDS:-180}"
MAX_CONCURRENT="${MAX_CONCURRENT:-200}"

# Podman command timeout (seconds) — prevents indefinite hangs on stuck containers
PODMAN_TIMEOUT="${PODMAN_TIMEOUT:-30}"

# ── Parse arguments ───────────────────────────────────────────────────────
CYCLE="${1:-1}"
BINARIES="${2:-main,pr1}"

IFS=',' read -ra BINARY_LIST <<< "$BINARIES"

# ── Compose file sets ─────────────────────────────────────────────────────
SCYLLA_COMPOSE=(
    -f "$REPO_ROOT/develop/docker-compose/docker-compose.scylla.yml"
    -f "$REPO_ROOT/develop/docker-compose/docker-compose.scylla.pinned.yml"
)
ES_COMPOSE=(
    -f "$REPO_ROOT/develop/docker-compose/docker-compose.yml"
    -f "$REPO_ROOT/develop/docker-compose/docker-compose.pinned.yml"
    -f "$REPO_ROOT/develop/docker-compose/docker-compose.es.yml"
)

# ── Helpers ───────────────────────────────────────────────────────────────
log()  { echo "[$(date '+%H:%M:%S')] $*"; }
warn() { log "WARN: $*"; }
die()  { log "ERROR: $*"; exit 1; }

# Run a command, warn on failure but do not abort. For cleanup/idempotent ops.
try() {
    "$@" || warn "command failed (rc=$?): $*"
}

wait_for_port() {
    local port=$1 timeout=${2:-90}
    for i in $(seq "$timeout"); do
        ss -tlnp 2>/dev/null | grep -q ":$port " && return 0
        sleep 1
    done
    return 1
}

wait_for_cql() {
    local timeout=${1:-90}
    for i in $(seq "$timeout"); do
        cqlsh 127.0.0.1 9042 -e 'describe cluster' 2>/dev/null && return 0
        sleep 2
    done
    return 1
}

wait_for_es() {
    local timeout=${1:-90}
    for i in $(seq "$timeout"); do
        curl -sf http://127.0.0.1:9200 >/dev/null && return 0
        sleep 2
    done
    return 1
}

# Extract iteration count from a bench log and print iter/s
report_result() {
    local label=$1 cycle=$2
    local logfile="/tmp/bench-$label-c$cycle.log"
    local line iters rate
    line=$(grep "Total iterations completed:" "$logfile" 2>/dev/null | head -1)
    iters=$(echo "$line" | sed 's/.*Total iterations completed: \([0-9]*\).*/\1/')
    if [ -n "$iters" ] && [[ "$iters" =~ ^[0-9]+$ ]]; then
        rate=$(python3 -c "print(f'{$iters/$BENCH_DURATION_SECONDS:.2f}')")
        log "RESULT: $label c$cycle: $iters iters → ${rate} iter/s"
        echo "$label $cycle $iters $rate" >> /tmp/benchmark-results.tsv
    else
        warn "no result found for $label c$cycle"
        echo "$label $cycle 0 0.00" >> /tmp/benchmark-results.tsv
    fi
}

# Print cumulative summary from all cycles so far
print_summary() {
    log "=== CUMULATIVE RESULTS ==="
    if [ ! -f /tmp/benchmark-results.tsv ]; then
        echo "  (no results file)"
        return
    fi
    # Print per-binary averages across all cycles
    python3 -c "
import collections, sys
data = collections.defaultdict(list)
with open('/tmp/benchmark-results.tsv') as f:
    for line in f:
        parts = line.strip().split()
        if len(parts) == 4:
            label, cycle, iters, rate = parts
            data[label].append(float(rate))
print(f'{\"Binary\":<20} {\"Cycles\":>6} {\"Avg iter/s\":>10} {\"Min\":>8} {\"Max\":>8}')
print('-' * 56)
for label in sorted(data):
    rates = data[label]
    avg = sum(rates) / len(rates)
    print(f'{label:<20} {len(rates):>6} {avg:>10.2f} {min(rates):>8.2f} {max(rates):>8.2f}')
"
}

# ── Step 1: Lock CPU + disable swap ───────────────────────────────────────
lock_cpu_and_swap() {
    log "Locking CPU + disabling swap..."
    source "$PERF_SCRIPT"
    cpu_save_state
    cpu_enable_performance_cpupower_state
    cpu_disable_intel_turbo_boost
    cpu_set_min_frequencies "$CPU_LOCK_FREQ_KHZ"
    cpu_set_max_frequencies "$CPU_LOCK_FREQ_KHZ"
    sudo cpupower idle-set -D "$CSTATE_LATENCY_THRESHOLD_US" \
        || die "failed to disable C-states (latency threshold ${CSTATE_LATENCY_THRESHOLD_US}us)"

    local swap_state
    swap_state=$(swapon --show --noheadings 2>/dev/null | wc -l)
    echo "$swap_state" > /tmp/bench-swap-state
    systemctl list-units --type=swap --no-legend 2>/dev/null | awk '{print $1}' > /tmp/bench-swap-units
    sudo swapoff -a || die "failed to disable swap"
    if [ -s /tmp/bench-swap-units ]; then
        sudo systemctl mask --now $(cat /tmp/bench-swap-units) \
            || die "failed to mask swap units"
    fi
}

# ── Restore CPU + swap ────────────────────────────────────────────────────
restore_cpu_and_swap() {
    log "Restoring CPU + swap..."
    cpu_restore_state
    sudo cpupower idle-set -E 2>/dev/null || warn "failed to re-enable C-states"
    if [ -s /tmp/bench-swap-units ]; then
        sudo systemctl unmask $(cat /tmp/bench-swap-units) 2>/dev/null \
            || warn "failed to unmask swap units"
    fi
    if [ "$(cat /tmp/bench-swap-state 2>/dev/null)" -gt 0 ] 2>/dev/null; then
        sudo swapon -a 2>/dev/null || warn "failed to re-enable swap"
    fi
}

# ── Step 2: Start infrastructure ───────────────────────────────────────────
start_infra() {
    log "Starting ScyllaDB..."
    # Clean any leftover ScyllaDB containers + volumes before starting
    try timeout "$PODMAN_TIMEOUT" podman compose "${SCYLLA_COMPOSE[@]}" down -v
    sudo rm -rf "$TMPFS"/*
    sudo chmod 777 "$TMPFS"
    timeout "$PODMAN_TIMEOUT" podman compose "${SCYLLA_COMPOSE[@]}" up -d scylladb \
        || die "failed to start ScyllaDB"
    wait_for_cql 90 || die "ScyllaDB failed to start (CQL not ready)"

    log "Starting Elasticsearch..."
    # Clean any leftover ES containers + volumes before starting
    try timeout "$PODMAN_TIMEOUT" podman compose "${ES_COMPOSE[@]}" down -v
    timeout "$PODMAN_TIMEOUT" podman compose "${ES_COMPOSE[@]}" up -d elasticsearch \
        || die "failed to start Elasticsearch"
    wait_for_es 90 || die "Elasticsearch failed to start (HTTP not ready)"
}

# ── Step 3: Install schemas ───────────────────────────────────────────────
install_schemas() {
    log "Installing schemas..."
    cqlsh 127.0.0.1 9042 -e "DROP KEYSPACE IF EXISTS temporal;"
    cqlsh 127.0.0.1 9042 -e \
      "CREATE KEYSPACE temporal WITH replication = {'class': 'NetworkTopologyStrategy', 'replication_factor': '1'};"
    "$REPO_ROOT/temporal-cassandra-tool" setup-schema -f "$REPO_ROOT/schema/cassandra/temporal/schema.cql" --disable-versioning
    "$REPO_ROOT/temporal-cassandra-tool" setup-schema -v 0.0
    cqlsh 127.0.0.1 9042 -e \
      "UPDATE temporal.schema_version SET curr_version = '1.13', min_compatible_version = '1.13' WHERE keyspace_name = 'temporal';"
    "$REPO_ROOT/temporal-elasticsearch-tool" --endpoint http://127.0.0.1:9200 setup-schema
    "$REPO_ROOT/temporal-elasticsearch-tool" --endpoint http://127.0.0.1:9200 create-index --index temporal_visibility_v1_dev
    "$REPO_ROOT/temporal-elasticsearch-tool" --endpoint http://127.0.0.1:9200 update-schema --index temporal_visibility_v1_dev
}

# ── Step 4: Run one binary ────────────────────────────────────────────────
run_binary() {
    local label=$1 cycle=$2

    log "Running binary: $label (cycle $cycle)"

    pkill -f "temporal-server-" 2>/dev/null || true
    sleep 2

    cd "$REPO_ROOT"
    nohup env GODEBUG=gctrace=1 taskset -c "$SERVER_CPUSET" "/tmp/temporal-server-$label" \
      --config config --env development-cass-es --allow-no-auth start \
      > "/tmp/server-$label-c$cycle.log" 2>&1 &
    disown

    wait_for_port 7233 30 || die "server $label failed to start on port 7233"
    sleep 3

    # Register namespace (idempotent — already exists after first binary)
    "$TEMPORAL_CLI" operator namespace create --namespace default \
      --address 127.0.0.1:7233 2>&1 || true

    # Register search attribute (idempotent — already exists after first binary)
    "$TEMPORAL_CLI" operator search-attribute create --name OmesExecutionID --type Keyword \
      --namespace default --address 127.0.0.1:7233 2>&1 || true
    for i in $(seq 30); do
        "$TEMPORAL_CLI" operator search-attribute list --namespace default \
          --address 127.0.0.1:7233 2>/dev/null | grep -q OmesExecutionID && break
        sleep 1
    done
    sleep 10

    # Smoke-test that omes can reach server (1 iteration, 1 concurrent, 1 minute)
    log "Smoke-testing $label..."
    "$OMES_DIR/omes-bin" run-scenario-with-worker \
      --scenario throughput_stress --language go --duration 1m \
      --max-concurrent 1 --option internal-iterations=1 \
      --option continue-as-new-after-iterations=0 --option sleep-time=1ms \
      > "/tmp/smoke-$label-c$cycle.log" 2>&1 \
      || die "smoke test failed for $label (rc=$?)"
    grep -q "Total iterations completed" "/tmp/smoke-$label-c$cycle.log" \
      || die "smoke test output missing 'Total iterations completed' for $label"

    # Run benchmark with host profiling
    pidstat -dur -h 1 2>/dev/null > "/tmp/pidstat-$label-c$cycle.log" &
    local pidstat_pid=$!
    mpstat -P ALL 1 2>/dev/null > "/tmp/mpstat-$label-c$cycle.log" &
    local mpstat_pid=$!

    "$OMES_DIR/omes-bin" run-scenario-with-worker \
      --scenario throughput_stress --language go \
      --duration "$BENCH_DURATION" --max-concurrent "$MAX_CONCURRENT" \
      --option internal-iterations=10 \
      --option continue-as-new-after-iterations=3 \
      --option sleep-time=1ms 2>&1 | tee "/tmp/bench-$label-c$cycle.log"

    kill "$pidstat_pid" "$mpstat_pid" 2>/dev/null || warn "failed to stop profiling"

    # Kill server before next binary
    pkill -f "temporal-server-" 2>/dev/null || true
    sleep 3

    # Report immediately
    report_result "$label" "$cycle"
}

# ── Step 5: Teardown ──────────────────────────────────────────────────────
teardown() {
    log "Tearing down containers..."
    pkill -f "temporal-server-" 2>/dev/null || true
    sleep 2
    # Properly stop compose services + remove volumes
    timeout "$PODMAN_TIMEOUT" podman compose "${SCYLLA_COMPOSE[@]}" down -v \
        || die "failed to tear down ScyllaDB (compose down -v)"
    timeout "$PODMAN_TIMEOUT" podman compose "${ES_COMPOSE[@]}" down -v \
        || die "failed to tear down Elasticsearch (compose down -v)"
    # Fallback: kill any stragglers by name
    try timeout 15 podman rm -f temporal-dev-scylladb temporal-dev-elasticsearch
    try timeout 10 podman network prune -f
    sudo rm -rf "$TMPFS"/*
    sudo chmod 777 "$TMPFS"
}

# ── Main (single cycle) ───────────────────────────────────────────────────
main() {
    log "Starting cycle $CYCLE, binaries: ${BINARY_LIST[*]}"
    log "REPO_ROOT=$REPO_ROOT"
    log "MAX_CONCURRENT=$MAX_CONCURRENT  BENCH_DURATION=$BENCH_DURATION"

    lock_cpu_and_swap
    start_infra
    install_schemas

    # Interleave: run each binary once
    for label in "${BINARY_LIST[@]}"; do
        run_binary "$label" "$CYCLE"
    done

    teardown
    restore_cpu_and_swap

    log "Cycle $CYCLE complete."
    print_summary
}

main
