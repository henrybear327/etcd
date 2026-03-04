#!/usr/bin/env bash
#
# bench-memory-21355.sh — Benchmark etcd memory regression in kvsToEvents (#21355)
#
# Usage: ./bench-memory-21355.sh [--dry-run] [label=gitref ...]
#
# v3.6.0-alpha.0 and v3.6.0-rc.0 are always included as baselines.
# Up to 3 extra branches can be specified as label=gitref pairs.
# If no '=' is given, the sanitized gitref is used as the label.
#
set -euo pipefail

# ---------- argument parsing ----------
DRY_RUN=false
EXTRA_SPECS=()

for arg in "$@"; do
    if [[ "$arg" == "--dry-run" ]]; then
        DRY_RUN=true
        continue
    fi
    EXTRA_SPECS+=("$arg")
done

if (( ${#EXTRA_SPECS[@]} > 3 )); then
    echo "ERROR: at most 3 extra branches allowed (got ${#EXTRA_SPECS[@]})" >&2
    exit 1
fi

if [[ "$DRY_RUN" == true ]]; then
    echo "*** DRY-RUN MODE: skipping build & benchmark, generating synthetic data ***"
fi

# Build the full spec list: baselines first, then extras
LABELS=()
GITREFS=()

# Baselines (always included)
LABELS+=("alpha0");  GITREFS+=("v3.6.0-alpha.0")
LABELS+=("rc0");     GITREFS+=("v3.6.0-rc.0")

# Extra specs
for spec in "${EXTRA_SPECS[@]}"; do
    if [[ "$spec" == *=* ]]; then
        LABELS+=("${spec%%=*}")
        GITREFS+=("${spec#*=}")
    else
        # Sanitize gitref to make a safe label (replace non-alphanumeric with -)
        sanitized=$(echo "$spec" | sed 's/[^a-zA-Z0-9]/-/g; s/--*/-/g; s/^-//; s/-$//')
        LABELS+=("$sanitized")
        GITREFS+=("$spec")
    fi
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export OUTDIR="${OUTDIR:-${SCRIPT_DIR}/bench-21355-results}"
ENDPOINT="http://127.0.0.1:2379"

# Benchmark parameters
STREAMS=3
WATCHERS=6
PUT_TOTAL=3000
PUT_RATE=20
VAL_SIZE=21504
BG_KEY_COUNT=3000
BG_KEY_SIZE=102400
RECONNECT_INTERVAL=10s
RECONNECT_SLEEP=3s

mkdir -p "$OUTDIR"

# ---------- cleanup trap ----------
ETCD_PID=""
MONITOR_PID=""

cleanup() {
    [[ -n "$MONITOR_PID" ]] && kill "$MONITOR_PID" 2>/dev/null || true
    [[ -n "$ETCD_PID" ]]   && kill "$ETCD_PID"   2>/dev/null || true
    wait 2>/dev/null || true
}
trap cleanup EXIT

if [[ "$DRY_RUN" == false ]]; then
# ---------- build phase ----------
BENCHMARK="$OUTDIR/benchmark"

build_etcd() {
    local label=$1 git_ref=$2 output_bin=$3 force=${4:-false}
    if [[ "$force" != "true" && -x "$output_bin" ]]; then
        echo "=== ${label} binary already exists, skipping build ==="
        return
    fi
    echo "=== Building etcd from ${git_ref} ==="
    local worktree="$OUTDIR/.worktree-${label}"
    git -C "$SCRIPT_DIR" worktree remove "$worktree" --force 2>/dev/null || true
    git -C "$SCRIPT_DIR" worktree prune
    git -C "$SCRIPT_DIR" worktree add --detach "$worktree" "$git_ref" --quiet
    (cd "$worktree/server" && CGO_ENABLED=0 GOWORK=off go build -o "$output_bin" .)
    git -C "$SCRIPT_DIR" worktree remove "$worktree" --force
}

for idx in "${!LABELS[@]}"; do
    label="${LABELS[$idx]}"
    gitref="${GITREFS[$idx]}"
    build_etcd "$label" "$gitref" "$OUTDIR/etcd-${label}" true
done

if [[ -x "$BENCHMARK" ]]; then
    echo "=== benchmark binary already exists, skipping build ==="
else
    echo "=== Building benchmark tool ==="
    (cd "$SCRIPT_DIR" && go build -o "$BENCHMARK" ./tools/benchmark)
fi
fi

# ---------- python venv ----------
VENV="$OUTDIR/.venv"
if ! "$VENV/bin/python3" -c "import matplotlib" 2>/dev/null; then
    rm -rf "$VENV"
    python3 -m venv "$VENV"
    "$VENV/bin/pip" install --quiet matplotlib
fi

# ---------- helper: monitor RSS ----------
monitor_rss() {
    local pid=$1 csv=$2
    echo "time_s,rss_kib" > "$csv"
    local t=0.0
    while kill -0 "$pid" 2>/dev/null; do
        local rss
        rss=$(awk '/^VmRSS:/ {print $2}' /proc/"$pid"/status 2>/dev/null) || break
        echo "${t},${rss}" >> "$csv"
        sleep 0.1
        t=$(awk "BEGIN {printf \"%.1f\", $t + 0.1}")
    done
}

# ---------- helper: generate synthetic CSV for dry-run ----------
generate_fake_csv() {
    local index=$1 csv=$2
    echo "time_s,rss_kib" > "$csv"

    # Each index gets a distinct synthetic pattern
    local base_rss=$(( 180000 + index * 30000 ))
    local growth=$(( 50 + index * 60 ))
    local noise=$(( 2000 + index * 1000 ))
    local plateau_point=$(( 300 + index * 150 ))

    for i in $(seq 0 2999); do
        local t_s rss_kib
        t_s=$(awk "BEGIN {printf \"%.1f\", $i * 0.1}")
        if (( i < plateau_point )); then
            rss_kib=$(( base_rss + i * growth + RANDOM % noise ))
        else
            rss_kib=$(( base_rss + plateau_point * growth + RANDOM % noise ))
        fi
        echo "${t_s},${rss_kib}" >> "$csv"
    done
    echo "Generated synthetic data: $csv (3000 points)"
}

# ---------- helper: run one version ----------
run_version() {
    local label=$1 etcd_bin=$2 csv=$3 etcd_log=$4 bench_log=$5
    local datadir="$OUTDIR/data-${label}"

    echo ""
    echo "========================================"
    echo "  Running: $label"
    echo "========================================"

    rm -rf "$datadir"
    mkdir -p "$datadir"

    # Start etcd
    "$etcd_bin" \
        --data-dir "$datadir" \
        --listen-client-urls "$ENDPOINT" \
        --advertise-client-urls "$ENDPOINT" \
        --listen-peer-urls http://127.0.0.1:2380 \
        > "$etcd_log" 2>&1 &
    ETCD_PID=$!

    # Wait for health
    echo "Waiting for etcd ($label) to be healthy..."
    for i in $(seq 1 30); do
        if curl -sf "$ENDPOINT/health" > /dev/null 2>&1; then
            echo "etcd ($label) is healthy (pid=$ETCD_PID)"
            break
        fi
        if [[ $i -eq 30 ]]; then
            echo "ERROR: etcd ($label) failed to start" >&2
            cat "$etcd_log" >&2
            exit 1
        fi
        sleep 1
    done

    # Start RSS monitor
    monitor_rss "$ETCD_PID" "$csv" &
    MONITOR_PID=$!

    # Run benchmark
    echo "Starting benchmark for $label..."
    "$OUTDIR/benchmark" watch-latency \
        --endpoints="$ENDPOINT" \
        --streams="$STREAMS" --watchers-per-stream="$WATCHERS" \
        --put-total="$PUT_TOTAL" --put-rate="$PUT_RATE" \
        --val-size="$VAL_SIZE" \
        --bg-key-count="$BG_KEY_COUNT" --bg-key-size="$BG_KEY_SIZE" \
        --reconnect-interval="$RECONNECT_INTERVAL" --reconnect-sleep="$RECONNECT_SLEEP" \
        --watch-from-rev=1 --prevkv \
        > "$bench_log" 2>&1 || true

    echo "Benchmark for $label complete."

    # Stop monitor & etcd
    kill "$MONITOR_PID" 2>/dev/null || true
    wait "$MONITOR_PID" 2>/dev/null || true
    MONITOR_PID=""

    kill "$ETCD_PID" 2>/dev/null || true
    wait "$ETCD_PID" 2>/dev/null || true
    ETCD_PID=""

    rm -rf "$datadir"
}

# ---------- benchmark phase ----------
for idx in "${!LABELS[@]}"; do
    label="${LABELS[$idx]}"
    csv="$OUTDIR/rss-${label}.csv"
    if [[ "$DRY_RUN" == true ]]; then
        generate_fake_csv "$idx" "$csv"
    else
        run_version "$label" "$OUTDIR/etcd-${label}" \
            "$csv" "$OUTDIR/etcd-${label}.log" "$OUTDIR/bench-${label}.log"
    fi
done

# ---------- plot phase ----------
echo ""
echo "=== Generating memory comparison plot ==="

# Write datasets config for Python
COLORS=("green" "red" "blue" "orange" "purple")
DATASETS_CFG="$OUTDIR/.datasets.cfg"
echo "label,csv_path,color" > "$DATASETS_CFG"
for idx in "${!LABELS[@]}"; do
    label="${LABELS[$idx]}"
    gitref="${GITREFS[$idx]}"
    color="${COLORS[$idx]}"
    echo "${gitref},${OUTDIR}/rss-${label}.csv,${color}" >> "$DATASETS_CFG"
done
export DATASETS_CFG

"$VENV/bin/python3" - <<'PYEOF'
import csv
import sys
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt

import os
outdir = os.environ["OUTDIR"]
datasets_cfg = os.environ["DATASETS_CFG"]

# Read dataset configuration
datasets = []
with open(datasets_cfg) as f:
    reader = csv.DictReader(f)
    for row in reader:
        datasets.append((row["label"], row["csv_path"], row["color"]))

fig, ax = plt.subplots(figsize=(12, 6))

for label, path, color in datasets:
    try:
        with open(path) as f:
            reader = csv.DictReader(f)
            times, rss = [], []
            for row in reader:
                times.append(float(row["time_s"]))
                rss.append(int(row["rss_kib"]) / 1024.0)  # KiB -> MiB
        ax.plot(times, rss, label=label, color=color, linewidth=1.5)
    except Exception as e:
        print(f"Warning: could not read {path}: {e}", file=sys.stderr)

ax.set_xlabel("Time (seconds)")
ax.set_ylabel("RSS (MiB)")
ax.set_title("etcd Memory Usage During watch-latency Benchmark (#21355)")
ax.legend()
ax.grid(True, alpha=0.3)

import datetime
timestamp = datetime.datetime.now().strftime("%Y%m%d_%H%M%S")
out_png = os.path.join(outdir, f"memory-comparison_{timestamp}.png")
fig.savefig(out_png, dpi=150, bbox_inches="tight")
print(f"Plot saved to {out_png}")
PYEOF

echo ""
echo "=== Done ==="
echo "Results in: $OUTDIR"
ls -lh "$OUTDIR"/*.csv "$OUTDIR"/*.png "$OUTDIR"/*.log 2>/dev/null || true
