# watch-latency-sim: buildFarmSim-style Benchmark

This benchmark reproduces the traffic pattern from
[buildFarmSim](https://github.com/mcornea/misc/tree/main/buildFarmSim) that
triggers the memory regression reported in
[#21355](https://github.com/etcd-io/etcd/issues/21355). The regression was
introduced between v3.6.0-alpha.0 and v3.6.0-rc.0 by PR #17563's event
reuse/caching in `kvsToEvents`, triggered during watcher reconnection catch-up.

## Building

### 1. Build the benchmark tool (from repo root)

```bash
go build -o bin/benchmark ./tools/benchmark
```

### 2. Build etcd binaries for the two versions of interest

The regression was introduced between these two tags:

```bash
# Create worktrees for each tag
git worktree add /tmp/etcd-v3.6.0-alpha.0 v3.6.0-alpha.0
git worktree add /tmp/etcd-v3.6.0-rc.0    v3.6.0-rc.0

# Build each
(cd /tmp/etcd-v3.6.0-alpha.0 && make build)
(cd /tmp/etcd-v3.6.0-rc.0    && make build)
```

Binaries will be at:
- `/tmp/etcd-v3.6.0-alpha.0/bin/etcd` — before the regression
- `/tmp/etcd-v3.6.0-rc.0/bin/etcd` — after the regression

To clean up worktrees later:

```bash
git worktree remove /tmp/etcd-v3.6.0-alpha.0
git worktree remove /tmp/etcd-v3.6.0-rc.0
```

## Running the Benchmark

### Quick smoke test (~1 minute)

```bash
# Terminal 1: start etcd
/tmp/etcd-v3.6.0-rc.0/bin/etcd --data-dir /tmp/etcd-bench-data

# Terminal 2: run benchmark
bin/benchmark --endpoints=127.0.0.1:2379 watch-latency-sim \
  --namespaces=2 \
  --jobs-per-ns=5 \
  --secrets-per-ns=10 \
  --run-duration=1m
```

### Full test matching buildFarmSim defaults (~3.5 minutes)

```bash
bin/benchmark --endpoints=127.0.0.1:2379 watch-latency-sim \
  --namespaces=10 \
  --jobs-per-ns=10 \
  --secrets-per-ns=50 \
  --controllers=3 \
  --watchers-per-ctrl=6 \
  --watch-buckets=32 \
  --prevkv \
  --reconnect-interval=2m \
  --reconnect-sleep=30s \
  --put-rate=500 \
  --run-duration=3m30s
```

### Comparing the two versions

```bash
# --- Run against v3.6.0-alpha.0 (baseline) ---
rm -rf /tmp/etcd-bench-data
/tmp/etcd-v3.6.0-alpha.0/bin/etcd --data-dir /tmp/etcd-bench-data &
ETCD_PID=$!

bin/benchmark --endpoints=127.0.0.1:2379 watch-latency-sim \
  --namespaces=10 --jobs-per-ns=10 --secrets-per-ns=50 \
  --run-duration=3m30s \
  --metrics-output=metrics-alpha.csv

kill $ETCD_PID
wait $ETCD_PID 2>/dev/null

# --- Run against v3.6.0-rc.0 (regression) ---
rm -rf /tmp/etcd-bench-data
/tmp/etcd-v3.6.0-rc.0/bin/etcd --data-dir /tmp/etcd-bench-data &
ETCD_PID=$!

bin/benchmark --endpoints=127.0.0.1:2379 watch-latency-sim \
  --namespaces=10 --jobs-per-ns=10 --secrets-per-ns=50 \
  --run-duration=3m30s \
  --metrics-output=metrics-rc.csv

kill $ETCD_PID
wait $ETCD_PID 2>/dev/null
```

## Memory Usage Monitoring

The benchmark automatically scrapes etcd's Prometheus `/metrics` endpoint
during the run. No extra tools are needed.

### What gets reported

At the end of each run, the benchmark prints a metrics summary:

```
Metrics summary (42 samples over 3m30s):
  RSS  - Initial:    45.2 MiB, Peak:   312.7 MiB, Final:   298.1 MiB, Growth: +252.9 MiB (+560%)
  DB   - Initial:     0.1 MiB, Peak:    52.3 MiB, Final:    52.3 MiB
  DB in-use      :    48.7 MiB (final)
```

During the run, periodic snapshots are logged to stderr:

```
[metrics] phase=secrets    RSS=   48.3MiB DB=   12.1MiB watchers=18
[metrics] phase=jobs       RSS=  102.5MiB DB=   35.7MiB watchers=18
[metrics] phase=hold       RSS=  298.1MiB DB=   52.3MiB watchers=12
```

### Metrics tracked

| Metric | Prometheus name | Description |
|--------|----------------|-------------|
| RSS | `process_resident_memory_bytes` | etcd process resident memory |
| DB size | `etcd_mvcc_db_total_size_in_bytes` | Total bbolt DB file size |
| DB in-use | `etcd_mvcc_db_total_size_in_use_in_bytes` | Used portion of bbolt DB |
| Watchers | `etcd_debugging_mvcc_watcher_total` | Active watcher count |
| Events | `etcd_debugging_mvcc_events_total` | Cumulative events processed |

### CSV export for analysis

Use `--metrics-output` to write all snapshots to a CSV file:

```bash
bin/benchmark --endpoints=127.0.0.1:2379 watch-latency-sim \
  --run-duration=3m30s \
  --metrics-output=metrics.csv
```

The CSV has columns: `timestamp, phase, rss_bytes, db_size_bytes,
db_in_use_bytes, watchers, events`. You can plot it with any tool:

```bash
# Quick gnuplot one-liner to plot RSS over time
gnuplot -p -e "
  set datafile separator ',';
  set xlabel 'Sample';
  set ylabel 'RSS (MiB)';
  plot 'metrics.csv' skip 1 using 0:(\$3/1048576) with lines title 'RSS'
"
```

### Manual memory check (alternative)

If you prefer to check memory outside the benchmark, you can query etcd's
metrics endpoint directly:

```bash
# One-shot RSS check
curl -s http://127.0.0.1:2379/metrics | grep process_resident_memory_bytes

# Watch RSS every 5 seconds
watch -n5 "curl -s http://127.0.0.1:2379/metrics | grep process_resident_memory_bytes"
```

## Flag Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--namespaces` | 10 | Number of simulated namespaces |
| `--jobs-per-ns` | 10 | Jobs per namespace |
| `--job-size` | 21504 (21KB) | Job value size in bytes |
| `--pod-size` | 10240 (10KB) | Pod value size in bytes |
| `--event-size` | 1024 (1KB) | Event value size in bytes |
| `--secrets-per-ns` | 50 | Secrets per namespace (DB inflation) |
| `--secret-size` | 102400 (100KB) | Secret value size in bytes |
| `--watch-buckets` | 32 | Number of watch prefix buckets |
| `--controllers` | 3 | Simulated controllers |
| `--watchers-per-ctrl` | 6 | Watchers per controller |
| `--prevkv` | true | Enable WithPrevKV on watches |
| `--reconnect-interval` | 2m | Watch duration before disconnect |
| `--reconnect-sleep` | 30s | Sleep between disconnect/reconnect |
| `--watch-start-delay` | 0 | Delay before starting watchers |
| `--put-rate` | 500 | Max PUTs per second |
| `--run-duration` | 3m30s | Total benchmark duration |
| `--metadata-iterations` | 2 | Metadata patch iterations per job |
| `--metrics-interval` | 5s | Metrics scrape interval |
| `--metrics-output` | (none) | CSV output path for metrics |
