#!/usr/bin/env bash
# scripts/run-robustness-local-arm64.sh
#
# Mirrors the pull-etcd-robustness-arm64 Prow job for local Raspberry Pi execution.
# Handles system dependency installation, lazyfs build, and runs test-robustness-main.
#
# Usage:
#   ./scripts/run-robustness-local-arm64.sh                              # defaults
#   COUNT=1 TIMEOUT=30m ./scripts/run-robustness-local-arm64.sh          # quick smoke test
#   STOP_ON_MEMORY_ERROR=true ./scripts/run-robustness-local-arm64.sh    # stop on first memory error

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_ROOT}"

# --- Configurable defaults (override via env vars) ---
CPU="${CPU:-4}"
COUNT="${COUNT:-10}"
TIMEOUT="${TIMEOUT:-200m}"
TEST_PATTERN="${TEST_PATTERN:-TestRobustnessExploratorySingleCase}"
RESULTS_DIR="${RESULTS_DIR:-/tmp/robustness-results}"

# --- Memory management ---
MEMORY_LIMIT="${MEMORY_LIMIT:-}"        # e.g. "6G" - cgroup hard limit via systemd-run
GOMEMLIMIT="${GOMEMLIMIT:-}"            # e.g. "5GiB" - Go runtime soft limit (aggressive GC)
MEMORY_PROFILE="${MEMORY_PROFILE:-1}"   # "1" to enable periodic heap profiling (default: on)
RSS_THRESHOLD_KB="${RSS_THRESHOLD_KB:-1048576}"  # per-process RSS limit in KB (default: 1GB)
STOP_ON_MEMORY_ERROR="${STOP_ON_MEMORY_ERROR:-false}"  # "true" to stop on memory errors, "false" to continue

if [[ "${MEMORY_PROFILE}" == "1" ]]; then
    MEMORY_PROFILE_DIR="${RESULTS_DIR}/heap-profiles"
    mkdir -p "${MEMORY_PROFILE_DIR}"
    export MEMORY_PROFILE_DIR
fi

if [[ -n "${GOMEMLIMIT}" ]]; then
    export GOMEMLIMIT
fi

# --- Step 1: Check & install system dependencies ---
echo "=== Step 1: Checking system dependencies ==="
missing_deps=()
command -v cmake >/dev/null 2>&1 || missing_deps+=(cmake)
dpkg -s libfuse3-dev >/dev/null 2>&1 || missing_deps+=(libfuse3-dev)
dpkg -s fuse3 >/dev/null 2>&1 || missing_deps+=(fuse3)

if [ ${#missing_deps[@]} -gt 0 ]; then
    echo "Installing missing packages: ${missing_deps[*]}"
    sudo apt-get update -qq && sudo apt-get install -y "${missing_deps[@]}"
else
    echo "All system dependencies installed."
fi

# --- Step 2: Enable user_allow_other in fuse.conf ---
if grep -q '^#user_allow_other' /etc/fuse.conf 2>/dev/null; then
    echo "=== Step 2: Enabling user_allow_other in /etc/fuse.conf ==="
    sudo sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
else
    echo "=== Step 2: user_allow_other already enabled ==="
fi

# --- Step 3: Build lazyfs ---
echo "=== Step 3: Building lazyfs ==="
if [ ! -f bin/lazyfs ]; then
    make install-lazyfs
else
    echo "lazyfs already built (delete bin/lazyfs to rebuild)"
fi

# --- Step 4: Run test-robustness-main ---
echo "=== Step 4: Running test-robustness-main ==="
echo "  CPU=${CPU} COUNT=${COUNT} TIMEOUT=${TIMEOUT}"
echo "  Pattern: ${TEST_PATTERN}"
echo "  Results: ${RESULTS_DIR}"
if [[ -n "${MEMORY_LIMIT}" ]]; then
    echo "  Memory limit: ${MEMORY_LIMIT} (cgroup via systemd-run)"
fi
if [[ -n "${GOMEMLIMIT}" ]]; then
    echo "  GOMEMLIMIT: ${GOMEMLIMIT} (Go runtime soft limit)"
fi
if [[ "${MEMORY_PROFILE}" == "1" ]]; then
    echo "  Memory profiling: enabled (${MEMORY_PROFILE_DIR})"
fi
echo "  RSS threshold: ${RSS_THRESHOLD_KB} KB per process"
echo "  Stop on memory error: ${STOP_ON_MEMORY_ERROR}"

mkdir -p "${RESULTS_DIR}"
memory_leak_marker="${RESULTS_DIR}/.memory-leak-detected"

# --- RSS monitor ---
RSS_MONITOR_PID=""
rss_monitor_log="${RESULTS_DIR}/rss-monitor.log"

start_rss_monitor() {
    local threshold="${RSS_THRESHOLD_KB}"
    local marker="${memory_leak_marker}"
    (
        echo "timestamp,process,pid,rss_kb" > "${rss_monitor_log}"
        while true; do
            ts="$(date '+%Y-%m-%d %H:%M:%S')"
            # Log RSS of robustness.test and etcd processes, check threshold
            ps -eo pid,comm,rss --no-headers 2>/dev/null | \
                awk '/robustness\.test|etcd/' | while read -r pid comm rss; do
                    echo "${ts},${comm},${pid},${rss}" >> "${rss_monitor_log}"
                    if [[ "${rss}" -gt "${threshold}" ]]; then
                        echo "${ts} ${comm} (pid=${pid}) RSS=${rss}KB exceeded threshold=${threshold}KB" > "${marker}"
                    fi
                done
            sleep 10
        done
    ) &
    RSS_MONITOR_PID=$!
}

stop_rss_monitor() {
    if [[ -n "${RSS_MONITOR_PID}" ]]; then
        kill "${RSS_MONITOR_PID}" 2>/dev/null || true
        wait "${RSS_MONITOR_PID}" 2>/dev/null || true
        RSS_MONITOR_PID=""
    fi
}

trap stop_rss_monitor EXIT

start_rss_monitor

# --- Build make command (once, reused each iteration) ---
make_cmd=(
    make -C tests/robustness test-robustness-main
)

# Wrap with systemd-run if MEMORY_LIMIT is set
if [[ -n "${MEMORY_LIMIT}" ]]; then
    if command -v systemd-run >/dev/null 2>&1; then
        make_cmd=(
            systemd-run --user --scope -p MemoryMax="${MEMORY_LIMIT}" -p MemorySwapMax=0 --
            "${make_cmd[@]}"
        )
    else
        echo "WARNING: systemd-run not found. MEMORY_LIMIT will not be enforced."
        echo "  Install systemd or run without MEMORY_LIMIT."
    fi
fi

# --- Continuous execution loop ---
echo "=== Starting continuous execution (Ctrl-C to stop) ==="

iteration=0
result=0

while true; do
    iteration=$((iteration + 1))
    iter_log="${RESULTS_DIR}/iteration-${iteration}.log"

    echo ""
    echo "=== Iteration ${iteration} started at $(date) ==="

    rm -f "${memory_leak_marker}"
    go clean -testcache

    # Record boot-relative timestamp for OOM detection
    test_start_seconds="$(awk '{print int($1)}' /proc/uptime)"

    result=0
    set +o pipefail
    GO_TEST_FLAGS="-v --count ${COUNT} --timeout ${TIMEOUT} --run ${TEST_PATTERN}" \
      VERBOSE=1 GOOS=linux GOARCH=arm64 CPU="${CPU}" EXPECT_DEBUG=true \
      RESULTS_DIR="${RESULTS_DIR}" \
      "${make_cmd[@]}" 2>&1 | tee "${iter_log}"
    result=${PIPESTATUS[0]}
    set -o pipefail

    if [[ $result -eq 0 ]]; then
        echo "=== Iteration ${iteration}: PASSED ==="
        continue
    fi

    # Check for memory leak (RSS threshold exceeded)
    if [[ -f "${memory_leak_marker}" ]]; then
        echo ""
        echo "=== Iteration ${iteration}: MEMORY LEAK DETECTED ==="
        cat "${memory_leak_marker}"
        echo ""
        echo "Check heap profiles and RSS log for analysis."

        if [[ "${STOP_ON_MEMORY_ERROR}" == "true" ]]; then
            break
        else
            echo "Continuing execution (STOP_ON_MEMORY_ERROR=false)..."
            continue
        fi
    fi

    # Check for benign QPS failure
    if grep -q "Requiring minimal.*qps before failpoint injection" "${iter_log}"; then
        echo "=== Iteration ${iteration}: SKIPPED (insufficient QPS — not a real failure) ==="
        continue
    fi

    # --- Real failure: OOM detection ---
    if oom_lines="$(dmesg --raw 2>/dev/null | awk -v start="${test_start_seconds}" -F'[],[]' '{if ($2+0 >= start) print}' | grep -i 'out of memory\|oom-kill\|killed process' 2>/dev/null)"; then
        echo ""
        echo "=== WARNING: OOM kill detected ==="
        echo "${oom_lines}"
        echo ""
        echo "Suggestions:"
        echo "  - Set MEMORY_LIMIT=6G to contain memory usage via cgroup"
        echo "  - Set GOMEMLIMIT=5GiB to trigger aggressive Go GC before the limit"
        echo "  - Lower COUNT to reduce cumulative memory pressure"

        if [[ "${STOP_ON_MEMORY_ERROR}" == "true" ]]; then
            echo ""
            echo "Stopping execution (STOP_ON_MEMORY_ERROR=true)"
            echo "=== Iteration ${iteration}: FAILED ==="
            break
        fi
    fi

    echo "=== Iteration ${iteration}: FAILED ==="
    break
done

stop_rss_monitor

echo ""
echo "=== Results after ${iteration} iterations ==="

# --- On failure: copy results to working directory ---
if [[ $result -ne 0 ]]; then
    failure_dir="${REPO_ROOT}/failure-results-$(date '+%Y%m%d-%H%M%S')"
    mkdir -p "${failure_dir}"

    # Copy the failing iteration log
    cp -f "${iter_log}" "${failure_dir}/" 2>/dev/null || true

    # Copy RSS monitor log
    cp -f "${rss_monitor_log}" "${failure_dir}/" 2>/dev/null || true

    # Copy memory leak marker if present
    cp -f "${memory_leak_marker}" "${failure_dir}/" 2>/dev/null || true

    # Copy heap profiles
    if [[ "${MEMORY_PROFILE}" == "1" ]] && [ -d "${MEMORY_PROFILE_DIR}" ]; then
        cp -r "${MEMORY_PROFILE_DIR}" "${failure_dir}/heap-profiles" 2>/dev/null || true
    fi

    echo "  Failure results copied to: ${failure_dir}"
    echo ""

    # --- Find the most useful profile files ---
    # With --count N, each subtest invocation creates its own timestamp directory.
    # Find the latest profile and its matching baseline from the same run.
    last_profile=""
    baseline_profile=""
    last_run_dir=""
    if [ -d "${failure_dir}/heap-profiles" ]; then
        last_profile="$(find "${failure_dir}/heap-profiles" -name '*.pb.gz' -printf '%T@ %p\n' 2>/dev/null | sort -rn | head -1 | cut -d' ' -f2-)"
        if [[ -n "${last_profile}" ]]; then
            last_run_dir="$(dirname "${last_profile}")"
            baseline_profile="$(find "${last_run_dir}" -name 'heap_0000_baseline_*.pb.gz' 2>/dev/null | head -1)"
        fi
    fi

    echo "=== Next steps ==="
    echo ""
    echo "# View test output:"
    echo "  less ${failure_dir}/iteration-${iteration}.log"
    echo ""
    echo "# View RSS growth over time:"
    echo "  column -t -s, ${failure_dir}/rss-monitor.log | less"
    echo ""

    if [[ -n "${last_profile}" ]]; then
        echo "# Top memory consumers (last snapshot before failure):"
        echo "  go tool pprof -top -inuse_space ${last_profile}"
        echo ""

        if [[ -n "${baseline_profile}" ]]; then
            echo "# Find memory leaks (diff baseline vs last snapshot):"
            echo "  go tool pprof -diff_base=${baseline_profile} ${last_profile}"
            echo ""
        fi

        echo "# Interactive web UI (flame graph, source view, call graph):"
        echo "  go tool pprof -http=:8080 ${last_profile}"
        echo ""
        echo "# Generate flame graph as SVG (no server needed):"
        echo "  go tool pprof -svg ${last_profile} > ${failure_dir}/flamegraph.svg"
        echo ""
        echo "# Generate call graph as PNG (needs graphviz):"
        echo "  go tool pprof -png ${last_profile} > ${failure_dir}/callgraph.png"
        echo ""
        echo "# List all heap profiles:"
        echo "  ls -lht ${failure_dir}/heap-profiles/*/*/"
        echo ""
        echo "# Latest run directory (profiles above are from this run):"
        echo "  ${last_run_dir}"
        echo ""
        echo "# List all runs (newest first):"
        echo "  ls -td ${failure_dir}/heap-profiles/*/*/"
    fi

    if [[ -f "${failure_dir}/.memory-leak-detected" ]]; then
        echo ""
        echo "# Memory leak details:"
        echo "  cat ${failure_dir}/.memory-leak-detected"
    fi
else
    echo "  All iterations passed."
fi

exit $result
