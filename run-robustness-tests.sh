#!/bin/bash
set -o pipefail

RUNS=${1:-5}

TARGETS=(
  "test-robustness-issue20221-fix20229-resumable"
  "test-robustness-issue20221-fix20229-resumable-peer-partition"
  "test-robustness-issue20221-fix20229-bookmarkable"
  "test-robustness-issue20221-fix20229-bookmarkable-peer-partition"
  "test-robustness-issue20221-fix20281"
  "test-robustness-issue20221-fix20281-peer-partition"
)

# Arrays to collect summary data
declare -a SUMMARY_TARGETS
declare -a SUMMARY_REPRO
declare -a SUMMARY_RUNS
declare -a SUMMARY_AVG
declare -a SUMMARY_MIN
declare -a SUMMARY_MAX
declare -a SUMMARY_DURATIONS
declare -a SUMMARY_ITERS
declare -a SUMMARY_BH

echo "=============================================="
echo "Running 6 targets x $RUNS runs each (count=30)"
echo "Started at $(date)"
echo "=============================================="
echo ""

IDX=0
for TARGET in "${TARGETS[@]}"; do
  echo "======================================================================"
  echo "TARGET: $TARGET"
  echo "======================================================================"

  REPRO_COUNT=0
  TOTAL_REPRO_TIME=0
  MIN_TIME=999999
  MAX_TIME=0
  DURATIONS=""
  TOTAL_ITERS=0
  TOTAL_BH_TIME=0
  TOTAL_BH_RUNS=0

  for i in $(seq 1 $RUNS); do
    TMPFILE=$(mktemp /tmp/robustness-${TARGET}-run${i}-XXXXXX.log)
    echo "  Run $i: log -> $TMPFILE"
    START=$(date +%s)

    make -B "$TARGET" > "$TMPFILE" 2>&1
    EXIT_CODE=$?

    END=$(date +%s)
    DUR=$((END - START))

    # Check for watch guarantee violations
    MATCHED_ERR=$(grep -oE "broke (Bookmarkable|Ordered|Unique|Atomic|Reliable|Resumable)|incorrect event (prevValue|IsCreate)|event not matching watch filter" "$TMPFILE" | head -1)

    # Extract iteration count (which --count iteration triggered the failure)
    # Note: log lines may have ANSI escape codes before "=== RUN", so no ^ anchor.
    # grep -c exits 1 on zero matches but still prints "0"; || true prevents that
    # from triggering error handling.
    COUNT_ITERS=$(grep -c '=== RUN   TestRobustnessRegression$' "$TMPFILE" 2>/dev/null || true)
    : "${COUNT_ITERS:=0}"

    # Extract total blackhole wait time (sum of Triggering->Finished failpoint durations)
    BH_SECS=$(python3 -c "
import re, sys
from datetime import datetime
starts, ends = [], []
for line in open(sys.argv[1]):
    if 'Triggering failpoint' in line:
        m = re.search(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3})', line)
        if m: starts.append(datetime.strptime(m.group(1), '%Y-%m-%dT%H:%M:%S.%f'))
    elif 'Finished triggering failpoint' in line:
        m = re.search(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3})', line)
        if m: ends.append(datetime.strptime(m.group(1), '%Y-%m-%dT%H:%M:%S.%f'))
total = sum((e-s).total_seconds() for s, e in zip(starts, ends))
print(int(total))
" "$TMPFILE" 2>/dev/null)
    # If python3 failed or produced non-integer output, fall back to "?"
    if ! [[ "$BH_SECS" =~ ^[0-9]+$ ]]; then
      BH_SECS="?"
    fi

    if [ -n "$MATCHED_ERR" ]; then
      echo "  Run $i: REPRODUCED in ${DUR}s ($MATCHED_ERR), iter=${COUNT_ITERS}/30, blackhole=${BH_SECS}s/${DUR}s"
      REPRO_COUNT=$((REPRO_COUNT + 1))
      TOTAL_REPRO_TIME=$((TOTAL_REPRO_TIME + DUR))
      TOTAL_ITERS=$((TOTAL_ITERS + COUNT_ITERS))
      if [ "$BH_SECS" != "?" ]; then
        TOTAL_BH_TIME=$((TOTAL_BH_TIME + BH_SECS))
        TOTAL_BH_RUNS=$((TOTAL_BH_RUNS + 1))
      fi
      [ $DUR -lt $MIN_TIME ] && MIN_TIME=$DUR
      [ $DUR -gt $MAX_TIME ] && MAX_TIME=$DUR
      DURATIONS="${DURATIONS}${DUR}s "
    elif grep -q "Failed to reproduce" "$TMPFILE"; then
      echo "  Run $i: NOT REPRODUCED (${DUR}s) - all 30 runs passed, blackhole=${BH_SECS}s/${DUR}s"
      DURATIONS="${DURATIONS}-  "
    elif grep -q "Successful reproduction" "$TMPFILE"; then
      echo "  Run $i: FAILED in ${DUR}s but no watch guarantee violation"
      grep -E "(FAIL|Error|error)" "$TMPFILE" | tail -3 | sed 's/^/    /'
      DURATIONS="${DURATIONS}?  "
    else
      echo "  Run $i: OTHER (${DUR}s, exit=$EXIT_CODE)"
      tail -5 "$TMPFILE" | sed 's/^/    /'
      DURATIONS="${DURATIONS}?  "
    fi

    # rm -f "$TMPFILE"
  done

  if [ $REPRO_COUNT -gt 0 ]; then
    AVG_TIME=$((TOTAL_REPRO_TIME / REPRO_COUNT))
    AVG_ITER=$((TOTAL_ITERS / REPRO_COUNT))
    if [ $TOTAL_BH_RUNS -gt 0 ]; then
      AVG_BH=$((TOTAL_BH_TIME / TOTAL_BH_RUNS))
    else
      AVG_BH=0
    fi
    echo "  => Result: $REPRO_COUNT/$RUNS reproduced, avg ${AVG_TIME}s"
  else
    AVG_TIME=0
    AVG_ITER=0
    AVG_BH=0
    MIN_TIME=0
    MAX_TIME=0
    echo "  => Result: 0/$RUNS reproduced"
  fi
  echo ""

  # Save for summary table
  SUMMARY_TARGETS[$IDX]="$TARGET"
  SUMMARY_REPRO[$IDX]="$REPRO_COUNT"
  SUMMARY_RUNS[$IDX]="$RUNS"
  SUMMARY_AVG[$IDX]="$AVG_TIME"
  SUMMARY_MIN[$IDX]="$MIN_TIME"
  SUMMARY_MAX[$IDX]="$MAX_TIME"
  SUMMARY_DURATIONS[$IDX]="$DURATIONS"
  SUMMARY_ITERS[$IDX]="$AVG_ITER"
  SUMMARY_BH[$IDX]="$AVG_BH"
  IDX=$((IDX + 1))
done

echo "=============================================="
echo "SUMMARY TABLE"
echo "=============================================="
echo ""
printf "%-64s | %5s | %7s | %5s | %5s | %5s | %9s | %s\n" "Target" "Repro" "AvgIter" "Avg" "Min" "Max" "BH/Run" "Durations"
printf "%-64s-+-%5s-+-%7s-+-%5s-+-%5s-+-%5s-+-%9s-+-%s\n" "----------------------------------------------------------------" "-----" "-------" "-----" "-----" "-----" "---------" "----------"
for i in $(seq 0 $((IDX - 1))); do
  if [ "${SUMMARY_REPRO[$i]}" -gt 0 ]; then
    printf "%-64s | %s/%s   | %4s/30 | %4ss | %4ss | %4ss | %3ss/%3ss | %s\n" \
      "${SUMMARY_TARGETS[$i]}" "${SUMMARY_REPRO[$i]}" "${SUMMARY_RUNS[$i]}" \
      "${SUMMARY_ITERS[$i]}" \
      "${SUMMARY_AVG[$i]}" "${SUMMARY_MIN[$i]}" "${SUMMARY_MAX[$i]}" \
      "${SUMMARY_BH[$i]}" "${SUMMARY_AVG[$i]}" \
      "${SUMMARY_DURATIONS[$i]}"
  else
    printf "%-64s | %s/%s   |    N/A  |   N/A |   N/A |   N/A |       N/A | %s\n" \
      "${SUMMARY_TARGETS[$i]}" "${SUMMARY_REPRO[$i]}" "${SUMMARY_RUNS[$i]}" \
      "${SUMMARY_DURATIONS[$i]}"
  fi
done
echo ""
echo "Finished at $(date)"
echo "=============================================="
