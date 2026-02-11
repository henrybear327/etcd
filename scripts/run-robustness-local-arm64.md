# Local Robustness Test Environment for Raspberry Pi

## Overview

The `run-robustness-local-arm64.sh` script replicates the `pull-etcd-robustness-arm64` Prow job for local execution on Raspberry Pi 5 hardware. This enables nightly or on-demand robustness testing without requiring CI infrastructure.

**What it does:**
1. Checks and installs system dependencies (cmake, libfuse3-dev, fuse3)
2. Configures FUSE for lazyfs (enables `user_allow_other`)
3. Builds lazyfs if not already present
4. Runs `test-robustness-main` in a continuous loop until a real failure occurs

**Test execution workflow:**
- The script uses the existing `test-robustness-main` Makefile target
- This target builds etcd from `main` and `release-3.6` branches with failpoints enabled
- It then runs the robustness tests with those binaries to verify cross-version compatibility

**Prow job equivalent:**
```bash
GO_TEST_FLAGS="-v --count 120 --timeout '200m' --run TestRobustnessExploratory"
VERBOSE=1 GOOS=linux GOARCH=amd64 CPU=8 EXPECT_DEBUG=true \
  GO_TEST_FLAGS=${GO_TEST_FLAGS} RESULTS_DIR=/data/results make test-robustness-main
```

## Resource Adjustments: Prow vs Pi 5

| Parameter | Prow | Pi 5 (default) | Notes |
|-----------|------|----------------|-------|
| CPU | 8 | 4 | Pi 5 has 4 cores |
| Memory | 14Gi | ~8Gi | Pi's limit, should be sufficient |
| `--count` | 120 | 10 | Fewer iterations due to slower hardware |
| `--timeout` | 200m | 200m | Keep same; individual runs are slower |
| Test pattern | `TestRobustnessExploratory` | `TestRobustnessExploratorySingleCase` | Pi runs single-case variant for resource efficiency |

All values are overridable via environment variables.

## System Dependencies

The script automatically checks for and installs missing dependencies:

| Dependency | Purpose |
|------------|---------|
| `cmake` | Required to build lazyfs |
| `libfuse3-dev` | FUSE3 development headers for lazyfs |
| `fuse3` | FUSE3 runtime |

It also enables `user_allow_other` in `/etc/fuse.conf` (required by lazyfs). This requires sudo, so the script will prompt for your password if needed.

## Configuration

All behavior is controlled via environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `CPU` | `4` | Number of CPUs for test parallelism |
| `COUNT` | `10` | Number of test iterations per run |
| `TIMEOUT` | `200m` | Go test timeout |
| `TEST_PATTERN` | `TestRobustnessExploratorySingleCase` | Test pattern to run |
| `RESULTS_DIR` | `/tmp/robustness-results` | Directory for test output files |
| `MEMORY_LIMIT` | *(unset)* | Cgroup hard memory limit via `systemd-run` (e.g., `6G`). Requires systemd-run. |
| `GOMEMLIMIT` | *(unset)* | Go runtime soft memory limit for aggressive GC (e.g., `5GiB`) |
| `MEMORY_PROFILE` | `1` | Periodic heap profiling: `1` to enable, `0` to disable |
| `RSS_THRESHOLD_KB` | `1048576` | Per-process RSS limit in KB (default: 1GB). Creates marker file when exceeded. |
| `STOP_ON_MEMORY_ERROR` | `false` | Stop script on memory errors (`true`) or continue collecting failures (`false`) |

## Memory Management

The Pi 5 has ~8GB RAM. Long test runs can leak memory, potentially freezing the system. Multiple mechanisms help prevent and diagnose this:

### Memory Protection Mechanisms

```
MEMORY_LIMIT (cgroup hard kill)
  ↓ ~1GB headroom
GOMEMLIMIT (Go GC soft limit)
  ↓ monitor independently
RSS_THRESHOLD_KB (per-process detection)
```

**1. Cgroup Memory Limit (`MEMORY_LIMIT`)**
- Uses `systemd-run --user --scope` to hard-limit the entire process tree (test binary + child etcd processes)
- The `--user` flag runs in the user's systemd session (no sudo required)
- The kernel OOM-kills processes in the scope instead of freezing the whole system
- `MemorySwapMax=0` prevents swap thrashing
- Example: `MEMORY_LIMIT=6G`

**2. Go Runtime Soft Limit (`GOMEMLIMIT`)**
- Triggers aggressive garbage collection as the Go heap approaches the limit
- Set this ~1GB below `MEMORY_LIMIT` to give the GC room to work before the cgroup kills the process
- Example: `GOMEMLIMIT=5GiB`

**3. RSS Threshold Watchdog (`RSS_THRESHOLD_KB`)**
- The RSS monitor checks each `robustness.test` and `etcd` process against the threshold every 10 seconds
- When a process exceeds the limit, the event is logged to a marker file (`.memory-leak-detected`)
- The test continues running normally (it may OOM or finish naturally)
- After the iteration completes, the loop behavior depends on `STOP_ON_MEMORY_ERROR`:
  - If `true`: Stops immediately with "MEMORY LEAK DETECTED" message
  - If `false` (default): Continues to next iteration after copying failure results
- Default threshold: 1GB (1048576 KB)

**4. RSS Monitor (always active)**
- Logs RSS of `robustness.test` and `etcd` processes every 10 seconds
- Saved to `${RESULTS_DIR}/rss-monitor.log` in CSV format: `timestamp,process,pid,rss_kb`
- Use this to analyze memory growth over time

**5. OOM Detection**
- On test failure, the script checks `dmesg` for OOM-kill messages
- Records the boot-relative uptime at test start and only reports OOM messages that appeared during the test run
- Avoids false positives from stale kernel messages
- Behavior controlled by `STOP_ON_MEMORY_ERROR` flag

### Memory Management Examples

```bash
# Default run (profiling enabled, no hard limits)
./scripts/run-robustness-local-arm64.sh

# With cgroup memory protection (recommended for overnight runs)
MEMORY_LIMIT=6G GOMEMLIMIT=5GiB ./scripts/run-robustness-local-arm64.sh

# Disable profiling if disk space is limited
MEMORY_PROFILE=0 ./scripts/run-robustness-local-arm64.sh

# Stop on first memory error (debugging mode)
STOP_ON_MEMORY_ERROR=true ./scripts/run-robustness-local-arm64.sh

# Continue through memory errors (nightly continuous mode)
STOP_ON_MEMORY_ERROR=false ./scripts/run-robustness-local-arm64.sh  # or just omit it
```

## Running the Script

### Quick Start

```bash
# Default run (10 iterations, 200m timeout)
./scripts/run-robustness-local-arm64.sh

# Quick smoke test (1 iteration, 30 min timeout)
COUNT=1 TIMEOUT=30m ./scripts/run-robustness-local-arm64.sh

# Full run with memory protection
MEMORY_LIMIT=6G GOMEMLIMIT=5GiB ./scripts/run-robustness-local-arm64.sh
```

### Advanced Examples

```bash
# Override CPU count for testing
CPU=2 COUNT=5 ./scripts/run-robustness-local-arm64.sh

# Run different test pattern
TEST_PATTERN=TestRobustnessExploratory ./scripts/run-robustness-local-arm64.sh

# Custom results directory
RESULTS_DIR=/data/robustness-results ./scripts/run-robustness-local-arm64.sh
```

### Continuous Execution Behavior

The script runs tests in an infinite loop until a real failure is detected or you press Ctrl-C.

After each iteration:
- **PASSED (exit 0)**: Logs "PASSED" and starts the next iteration
- **SKIPPED (insufficient QPS)**: If the only failure is "Requiring minimal qps before failpoint injection", the iteration is logged as "SKIPPED" and continues. This is an environmental fluke (the Pi couldn't sustain enough traffic), not a real bug.
- **FAILED (any other error)**: Logs "FAILED", copies results to a timestamped failure directory, runs OOM detection, and exits (unless `STOP_ON_MEMORY_ERROR=false` and the failure was memory-related)

Each iteration's full output is saved to `${RESULTS_DIR}/iteration-N.log` for post-mortem analysis. The test cache is cleared between iterations to ensure fresh runs.

## Understanding the Output

During execution, the following files are created in `${RESULTS_DIR}`:

```
/tmp/robustness-results/  (or your custom RESULTS_DIR)
├── iteration-1.log              # Full output of iteration 1
├── iteration-2.log              # Full output of iteration 2
├── iteration-N.log              # Full output of iteration N
├── rss-monitor.log              # CSV: timestamp,process,pid,rss_kb
├── .memory-leak-detected        # Marker file (only if RSS threshold exceeded)
└── heap-profiles/               # Memory snapshots (if MEMORY_PROFILE=1)
    └── TestRobustnessExploratorySingleCase_<scenario>/
        ├── <unix_nanos_run1>/       # run 1 (with --count N, each invocation gets its own dir)
        │   ├── heap_0000_baseline_<ts>.pb.gz     # at test start
        │   ├── heap_0001_periodic_<ts>.pb.gz     # t+1s
        │   ├── heap_0002_periodic_<ts>.pb.gz     # t+2s
        │   ├── ...
        │   └── heap_NNNN_final_<ts>.pb.gz        # at test end (missing on OOM)
        ├── <unix_nanos_run2>/       # run 2
        │   └── ...
        └── <unix_nanos_runN>/       # run N
            └── ...
```

**File details:**
- **iteration-N.log**: Complete stdout/stderr from that iteration
- **rss-monitor.log**: CSV format with headers, sampled every 10 seconds
- **.memory-leak-detected**: Contains timestamp and process info when RSS exceeded threshold
- **heap-profiles/**: One directory per scenario, with a subdirectory per `--count` invocation containing timestamped snapshots every second

## Analyzing Failures

When a test fails, the script automatically copies relevant files to a timestamped directory in your working directory for easy analysis.

### Failure Results Directory Structure

```
failure-results-YYYYMMDD-HHMMSS/
├── iteration-N.log              # The failing iteration's complete output
├── rss-monitor.log              # RSS tracking over time
├── .memory-leak-detected        # Present if RSS threshold was exceeded
└── heap-profiles/               # Memory snapshots (if profiling was enabled)
    └── TestRobustnessExploratorySingleCase_*/
        ├── <unix_nanos_run1>/       # one directory per --count invocation
        │   ├── heap_0000_baseline_*.pb.gz
        │   ├── heap_0001_periodic_*.pb.gz
        │   └── ...
        ├── <unix_nanos_run2>/
        │   └── ...
        └── ...
```

### Failure Types

**1. Real Test Failure**
- Loop stops, results copied to `failure-results-YYYYMMDD-HHMMSS/`
- Check iteration log for test errors

**2. Memory Leak (RSS Threshold Exceeded)**
- Marker file created: `.memory-leak-detected`
- Behavior depends on `STOP_ON_MEMORY_ERROR`:
  - `true`: Loop stops immediately
  - `false`: Results copied, loop continues to collect more failures
- Check RSS log and heap profiles for leak source

**3. OOM Kill**
- Detected via dmesg timestamps
- Shows suggestions for memory limits
- If `STOP_ON_MEMORY_ERROR=true`, loop stops

**4. Benign QPS Failure**
- Loop continues, iteration logged as "SKIPPED"
- Not a real failure, just insufficient system load

### Next Steps After Failure

The script provides helpful commands at the end. Here's the complete workflow:

#### 1. View Test Output
```bash
less failure-results-YYYYMMDD-HHMMSS/iteration-N.log
```

#### 2. View RSS Growth Over Time
```bash
column -t -s, failure-results-YYYYMMDD-HHMMSS/rss-monitor.log | less
```
This shows memory usage trends. Look for processes with steadily increasing RSS.

#### 3. Check Memory Leak Marker (if present)
```bash
cat failure-results-YYYYMMDD-HHMMSS/.memory-leak-detected
```
Shows which process exceeded the threshold and when.

#### 4. Analyze Memory Profiles (if available)

With `--count N`, each subtest invocation creates its own timestamp directory. First, identify which run to analyze:

```bash
# List all runs (newest first)
ls -td failure-results-*/heap-profiles/*/*/

# Set LATEST_RUN to the most recent run directory
LATEST_RUN="$(ls -td failure-results-*/heap-profiles/*/* | head -1)"
```

**Top memory consumers (last snapshot before failure):**
```bash
go tool pprof -top -inuse_space "$(ls -t ${LATEST_RUN}/heap_*.pb.gz | head -1)"
```

**Find memory leaks (diff baseline vs last snapshot):**
```bash
go tool pprof -diff_base="${LATEST_RUN}"/heap_0000_baseline_*.pb.gz "$(ls -t ${LATEST_RUN}/heap_*_periodic_*.pb.gz | head -1)"
```
Inside pprof, use `top` to see what grew the most.

**Interactive web UI (flame graph, source view, call graph):**
```bash
go tool pprof -http=:8080 "$(ls -t ${LATEST_RUN}/heap_*.pb.gz | head -1)"
```

**Generate flame graph as SVG (no server needed):**
```bash
go tool pprof -svg "$(ls -t ${LATEST_RUN}/heap_*.pb.gz | head -1)" > failure-results-*/flamegraph.svg
```

**Generate call graph as PNG (requires graphviz):**
```bash
go tool pprof -png "$(ls -t ${LATEST_RUN}/heap_*.pb.gz | head -1)" > failure-results-*/callgraph.png
```

**List all heap profiles:**
```bash
ls -lht failure-results-*/heap-profiles/*/*/
```

## Advanced Topics

### Memory Profiling Deep-Dive

Heap profiles are saved in the standard Go pprof format (`.pb.gz`). Each subtest gets its own directory with timestamped snapshots.

#### Profile Types

Each test run produces:
- **Baseline snapshot**: Taken at test start (`heap_0000_baseline_*.pb.gz`)
- **Periodic snapshots**: Taken every 1 second during execution (`heap_*_periodic_*.pb.gz`)
- **Final snapshot**: Taken at test end (`heap_NNNN_final_*.pb.gz`) — missing if the process was OOM-killed

Since snapshots are written incrementally to disk, they survive OOM kills (only the final snapshot is lost).

#### Selecting a Run

With `--count N`, each subtest invocation creates its own timestamp directory. First pick the run to analyze:

```bash
# List all runs (newest first)
ls -td /tmp/robustness-results/heap-profiles/*/*/

# Use the latest run
RUN="$(ls -td /tmp/robustness-results/heap-profiles/*/* | head -1)"
```

#### Interactive Exploration

```bash
# Open the last periodic snapshot (closest to OOM)
go tool pprof "$(ls -t ${RUN}/heap_*_periodic_*.pb.gz | head -1)"

# Inside pprof:
#   top 20          — show top 20 memory consumers
#   list <func>     — show annotated source for a function
#   web             — open flame graph in browser (needs graphviz)
#   exit            — quit pprof
```

#### Finding Memory Leaks

Compare baseline to the last periodic snapshot:
```bash
go tool pprof -diff_base="${RUN}"/heap_0000_baseline_*.pb.gz "$(ls -t ${RUN}/heap_*_periodic_*.pb.gz | head -1)"

# Inside pprof, use `top` to see what grew the most
```

#### Tracking Memory Growth Over Time

Compare two periodic snapshots (e.g., early vs late):
```bash
EARLY="$(ls ${RUN}/heap_*_periodic_*.pb.gz | head -1)"
LATE="$(ls ${RUN}/heap_*_periodic_*.pb.gz | tail -1)"
go tool pprof -diff_base="${EARLY}" "${LATE}"
```

#### Non-Interactive Analysis

Print top 10 in-use memory consumers:
```bash
go tool pprof -top -inuse_space "$(ls -t ${RUN}/*.pb.gz | head -1)"
```

#### Understanding inuse_space vs alloc_space

- **`-inuse_space`** (default): Memory currently held — shows what's alive now. Use this to find leaks.
- **`-alloc_space`**: Total memory ever allocated — shows allocation hotspots. Use this to find churn that pressures GC.

### Systemd-Run Requirement

The `MEMORY_LIMIT` feature requires `systemd-run` to be available. If it's not found, the script prints a warning and continues without memory limits.

Check if systemd-run is available:
```bash
command -v systemd-run
```

If missing on your system, you can still use `GOMEMLIMIT` and `RSS_THRESHOLD_KB` for memory management.

### Adjusting for Different Hardware

The defaults are tuned for Raspberry Pi 5. For other arm64 systems:

```bash
# For systems with more cores
CPU=8 COUNT=20 ./scripts/run-robustness-local-arm64.sh

# For systems with less memory
MEMORY_LIMIT=4G GOMEMLIMIT=3GiB RSS_THRESHOLD_KB=524288 ./scripts/run-robustness-local-arm64.sh

# For faster hardware (shorter timeout)
TIMEOUT=100m ./scripts/run-robustness-local-arm64.sh
```

## Troubleshooting

**Issue: "user_allow_other" permission denied**
- Solution: Run the script once with sudo to enable the FUSE config, then run without sudo afterward

**Issue: Out of disk space**
- Heap profiles can consume significant space
- Solution: Disable profiling with `MEMORY_PROFILE=0` or use a custom `RESULTS_DIR` on a larger disk

**Issue: Test fails with "insufficient QPS"**
- This is expected on the Pi occasionally
- The script automatically skips these and continues
- Not a real failure

**Issue: System becomes unresponsive**
- Likely hitting OOM without limits
- Solution: Use `MEMORY_LIMIT=6G GOMEMLIMIT=5GiB` to prevent system-wide OOM

**Issue: "systemd-run not found" warning**
- `MEMORY_LIMIT` won't work
- Solution: Install systemd, or use `GOMEMLIMIT` and `RSS_THRESHOLD_KB` instead
