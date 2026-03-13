#!/bin/bash
#
# OmniVM Manifest Test Suite
#
# Runs all manifest examples through the manifest-runner inside Docker
# and reports pass/fail status for each.
#
# Usage:
#   ./scripts/test-manifests.sh              # run all tests
#   ./scripts/test-manifests.sh --quick      # skip slow tests (express, pastebin)
#
set -uo pipefail

IMAGE="${OMNIVM_IMAGE:-omnivm:latest}"
RUNNER="manifest-runner"
EXAMPLES="/omnivm/examples"
QUICK=false

for arg in "$@"; do
  case "$arg" in
    --quick) QUICK=true ;;
  esac
done

passed=0
failed=0
skipped=0
errors=()

run() {
  local name="$1"
  local file="$2"
  local expect_error="${3:-false}"

  printf "  [TEST] %-45s " "$name"

  output=$(docker run --rm --entrypoint "$RUNNER" "$IMAGE" "$EXAMPLES/$file" 2>&1)
  exit_code=$?

  # Check for manifest completion marker
  if echo "$output" | grep -q "Manifest execution complete."; then
    printf "\033[32mPASS\033[0m\n"
    ((passed++))
  elif [ "$expect_error" = "true" ] && echo "$output" | grep -q "Manifest execution error:"; then
    # Some tests intentionally trigger errors but still complete
    if echo "$output" | grep -q "Shutting down..."; then
      printf "\033[32mPASS\033[0m (expected error)\n"
      ((passed++))
    else
      printf "\033[31mFAIL\033[0m (exit=%d)\n" "$exit_code"
      errors+=("$name: did not shut down cleanly")
      ((failed++))
    fi
  else
    printf "\033[31mFAIL\033[0m (exit=%d)\n" "$exit_code"
    # Extract the error line
    err_line=$(echo "$output" | grep -E "(error|Error|FAIL)" | head -1)
    errors+=("$name: ${err_line:-unknown error}")
    ((failed++))
  fi
}

skip() {
  local name="$1"
  printf "  [SKIP] %-45s \033[33mSKIPPED\033[0m\n" "$name"
  ((skipped++))
}

echo "=== OmniVM Manifest Test Suite ==="
echo "Image: $IMAGE"
echo

# ── Category 1: Basic Ops ──────────────────────────────────────
echo "── Basic Ops ──"
run "Polyglot eval/exec/import/concat"        manifest-test.json
run "Syntactic dominance (Python+JS pipeline)" syntactic-dominance.json

# ── Category 2: Control Flow ───────────────────────────────────
echo "── Control Flow ──"
run "If/else, while loops, recursion"          controlflow-test.json
run "Params, mutability, assign operators"     controlflow-manifest.json

# ── Category 3: Cross-Runtime Functions ────────────────────────
echo "── Cross-Runtime Functions ──"
run "Round-trip, accumulate, recursive chains" crossruntime-manifest.json

# ── Category 4: Advanced Patterns ──────────────────────────────
echo "── Advanced Patterns ──"
run "Foreach, try/catch, batch, large data"    stress-test-2.json    true
run "Async/await, parallel, channels, select"  stress-test-4.json
run "Channels, generators, spawn workers"      stress-test-5.json

# ── Category 5: Concurrency & Edge Cases ───────────────────────
echo "── Concurrency & Edge Cases ──"
run "Cursed concurrency (full channel+spawn)"  cursed-concurrency.json

# ── Category 6: Application Manifests ──────────────────────────
echo "── Application Manifests ──"
if [ "$QUICK" = "true" ]; then
  skip "Express.js + Python text processing"
  skip "Pastebin multi-shard API"
else
  run "Express.js + Python text processing"    express-manifest.json
  run "Pastebin multi-shard API"               pastebin-manifest.json  true
fi

# ── Summary ────────────────────────────────────────────────────
echo
total=$((passed + failed))
if [ "$skipped" -gt 0 ]; then
  printf "Results: \033[32m%d passed\033[0m, \033[31m%d failed\033[0m, \033[33m%d skipped\033[0m out of %d\n" \
    "$passed" "$failed" "$skipped" "$((total + skipped))"
else
  printf "Results: \033[32m%d passed\033[0m, \033[31m%d failed\033[0m out of %d\n" \
    "$passed" "$failed" "$total"
fi

if [ "${#errors[@]}" -gt 0 ]; then
  echo
  echo "Failures:"
  for err in "${errors[@]}"; do
    echo "  - $err"
  done
fi

echo
if [ "$failed" -gt 0 ]; then
  exit 1
fi
