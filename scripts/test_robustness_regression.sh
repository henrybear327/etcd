#!/usr/bin/env bash
# Copyright 2025 The etcd Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Script to run all robustness regression tests
# Usage: ./scripts/test_robustness_regression.sh [OPTIONS]
#
# This script ensures that refactoring doesn't break the ability to reproduce
# previously discovered bugs, as documented in tests/robustness/README.md
# section "Maintaining Bug Reproducibility During Refactoring"

set -uo pipefail

# Configuration
ETCD_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
source "${ETCD_ROOT}/scripts/test_utils.sh"

RESULTS_DIR="${RESULTS_DIR:-/tmp/robustness-regression-results}"
PARALLEL_JOBS="${PARALLEL_JOBS:-1}"
TIMEOUT="${TIMEOUT:-45m}"

# Detect timeout command
TIMEOUT_CMD=""
if command -v timeout >/dev/null 2>&1; then
  TIMEOUT_CMD="timeout"
elif command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_CMD="gtimeout"
fi

# Show usage
show_usage() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Run all robustness regression tests to ensure refactoring doesn't break
bug reproducibility.

Options:
  --parallel=N         Number of tests to run in parallel (default: 1)
  --results-dir=PATH   Directory to store test results (default: /tmp/robustness-regression-results)
  --timeout=DURATION   Timeout per test, e.g., 45m, 1h (default: 45m)
  --help, -h           Show this help message

Examples:
  # Sequential execution
  ./$(basename "$0")

  # Parallel execution with 4 jobs
  ./$(basename "$0") --parallel=4

  # Custom results directory
  ./$(basename "$0") --results-dir=/tmp/my-results

Environment Variables:
  RESULTS_DIR          Alternative to --results-dir flag
  PARALLEL_JOBS        Alternative to --parallel flag
  TIMEOUT              Alternative to --timeout flag
EOF
}

# Parse arguments
for arg in "$@"; do
  case $arg in
    --parallel=*)
      PARALLEL_JOBS="${arg#*=}"
      ;;
    --results-dir=*)
      RESULTS_DIR="${arg#*=}"
      ;;
    --timeout=*)
      TIMEOUT="${arg#*=}"
      ;;
    --help|-h)
      show_usage
      exit 0
      ;;
    *)
      log_error "Unknown option: $arg"
      show_usage
      exit 2
      ;;
  esac
done

# Discover test targets from Makefile
discover_tests() {
  make -C "${ETCD_ROOT}/tests/robustness" -qp 2>/dev/null | \
    grep -E '^test-robustness-issue[0-9]+:' | \
    cut -d: -f1 | \
    sort
}

# Run a single test
run_test() {
  local target=$1
  local log="${RESULTS_DIR}/${target}.log"
  local start_time=$(date +%s)

  log_callout "Running: ${target}"

  local exit_code=0
  if [ -n "$TIMEOUT_CMD" ]; then
    $TIMEOUT_CMD "$TIMEOUT" make -C "$ETCD_ROOT" -f "${ETCD_ROOT}/tests/robustness/Makefile" "$target" > "$log" 2>&1 || exit_code=$?
  else
    make -C "$ETCD_ROOT" -f "${ETCD_ROOT}/tests/robustness/Makefile" "$target" > "$log" 2>&1 || exit_code=$?
  fi

  local end_time=$(date +%s)
  local duration=$((end_time - start_time))
  echo "$duration" > "${RESULTS_DIR}/${target}.duration"

  if [ $exit_code -eq 0 ]; then
    touch "${RESULTS_DIR}/${target}.passed"
    log_success "PASSED: ${target} (${duration}s)"
  else
    touch "${RESULTS_DIR}/${target}.failed"
    log_error "FAILED: ${target} (see ${log})"
  fi
}

# Export function for xargs subshells
export -f run_test
export -f log_callout
export -f log_success
export -f log_error
export ETCD_ROOT RESULTS_DIR TIMEOUT TIMEOUT_CMD
export COLOR_LIGHTCYAN COLOR_GREEN COLOR_RED COLOR_BOLD COLOR_NONE

# Generate summary
generate_summary() {
  local passed=$(find "$RESULTS_DIR" -name "*.passed" 2>/dev/null | wc -l | tr -d ' ')
  local failed=$(find "$RESULTS_DIR" -name "*.failed" 2>/dev/null | wc -l | tr -d ' ')
  local total=$((passed + failed))

  echo ""
  log_callout "=========================================="
  log_callout "  Robustness Regression Test Results"
  log_callout "=========================================="
  echo "Total: ${total}  Passed: ${passed}  Failed: ${failed}"

  if [ "$failed" -gt 0 ]; then
    echo ""
    log_error "Failed Tests:"
    for marker in "$RESULTS_DIR"/*.failed; do
      [ -f "$marker" ] || continue
      local test_name=$(basename "$marker" .failed)
      log_error "  - ${test_name}: ${RESULTS_DIR}/${test_name}.log"
    done

    # Save summary to file
    {
      echo "=========================================="
      echo "  Robustness Regression Test Results"
      echo "=========================================="
      echo "Total: ${total}  Passed: ${passed}  Failed: ${failed}"
      echo ""
      echo "Failed Tests:"
      for marker in "$RESULTS_DIR"/*.failed; do
        [ -f "$marker" ] || continue
        local test_name=$(basename "$marker" .failed)
        echo "  - ${test_name}: ${RESULTS_DIR}/${test_name}.log"
      done
    } > "${RESULTS_DIR}/summary.txt"

    return 1
  else
    log_success "✓ All tests passed!"

    # Save summary to file
    {
      echo "=========================================="
      echo "  Robustness Regression Test Results"
      echo "=========================================="
      echo "Total: ${total}  Passed: ${passed}  Failed: ${failed}"
      echo ""
      echo "✓ All tests passed!"
    } > "${RESULTS_DIR}/summary.txt"

    return 0
  fi
}

# Main execution
main() {
  # Create results directory
  if ! mkdir -p "$RESULTS_DIR"; then
    log_error "Failed to create results directory: $RESULTS_DIR"
    exit 2
  fi

  # Clean up old results
  rm -f "$RESULTS_DIR"/*.passed "$RESULTS_DIR"/*.failed "$RESULTS_DIR"/*.log "$RESULTS_DIR"/*.duration "$RESULTS_DIR"/summary.txt

  # Discover tests
  log_callout "Discovering test targets from Makefile..."
  local targets
  targets=$(discover_tests)

  if [ -z "$targets" ]; then
    log_error "No test targets found in Makefile"
    exit 2
  fi

  local count=$(echo "$targets" | wc -l | tr -d ' ')
  log_callout "Found ${count} test(s), running with parallelism=${PARALLEL_JOBS}"
  echo ""

  # Run tests
  echo "$targets" | xargs -P "$PARALLEL_JOBS" -I {} bash -c 'run_test "$@"' _ {}

  # Generate summary
  generate_summary
}

main "$@"
