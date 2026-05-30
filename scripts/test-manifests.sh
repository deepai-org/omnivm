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
  local expect_pattern="${4:-}"

  printf "  [TEST] %-45s " "$name"

  output=$(docker run --rm --entrypoint "$RUNNER" "$IMAGE" "$EXAMPLES/$file" 2>&1)
  exit_code=$?

  # Check for manifest completion marker
  if echo "$output" | grep -q "Manifest execution complete."; then
    if [ -n "$expect_pattern" ] && ! echo "$output" | grep -Eq "$expect_pattern"; then
      printf "\033[31mFAIL\033[0m (output mismatch)\n"
      errors+=("$name: output did not match /$expect_pattern/")
      ((failed++))
      return
    fi
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
run "Spawn handles + channel capture contract" spawn-channel-contract.json false "Channel contract total=3 workers=2 delivered=1"

# ── Category 5: Concurrency & Edge Cases ───────────────────────
echo "── Concurrency & Edge Cases ──"
run "Cursed concurrency (full channel+spawn)"  cursed-concurrency.json false "Processed [1-9][0-9]* items across 3 runtimes; workers 4; delivered 1 report"

# ── Category 6: Application Manifests ──────────────────────────
echo "── Application Manifests ──"
if [ "$QUICK" = "true" ]; then
  skip "Express.js + Python text processing"
  skip "Pastebin multi-shard API"
else
  run "Express.js + Python text processing"    express-manifest.json
  run "Pastebin multi-shard API"               pastebin-manifest.json  true
fi

# ── Category 7: Popular Ecosystem Libraries ────────────────────
echo "── Popular Ecosystem Libraries ──"
run "Django + Zod + Go HMAC endpoint"       django-zod-go-crypto.json false "Django/Zod/Go secure endpoint 200 user-42 /secure/orders"
run "Express + Pandas + Go workers"         express-pandas-go-workers.json false "Express/Pandas/Go report 3 regions top=west workers=2"
run "BeautifulSoup + Cheerio + Go cache"    beautifulsoup-cheerio-go-cache.json false "BeautifulSoup/Cheerio/Go scrape POLYGLOT RUNTIME links=2 workers=2"
run "Pydantic + Zod + Go events"            pydantic-zod-go-events.json false "Pydantic/Zod/Go event .* amount=42"
run "SQLAlchemy + Lodash + Go batching"     sqlalchemy-lodash-go-batching.json false "SQLAlchemy/Lodash/Go admin endpoint roles=2 batches=2 sql=true"
run "NumPy + Pandas + D3 + Go channels"     numpy-pandas-d3-go-channels.json false "NumPy/Pandas/D3/Go chart points=4 series=4 workers=2"
run "Jinja2 + Marked + Go docs"             jinja2-marked-go-docs.json false "Jinja2/Marked/Go docs key=poly-docs-[0-9a-f]{12} length=[1-9][0-9]*"
run "Express + NumPy + Go rate limit"       express-numpy-go-rate-limit.json false "Express/NumPy/Go scoring allowed=true bucket=3 score=2.2"
run "HTTPX + URL + Go retry workers"        httpx-url-go-retry.json false "HTTPX/URL/Go aggregator endpoint=https://api.example.test/v1/items attempts=2 workers=2 backoff=50ms"
run "Nokogiri + Pandas + JS formatting"     nokogiri-pandas-js-format.json false "Nokogiri/Pandas/JS legacy feed Ada:2\\|Grace:3"
run "Java Gson + Pandas + Zod + Express"    java-gson-pandas-zod-express.json false "Java/Gson/Pandas/Zod/Express regions=2 top=west amount=40"
run "Java Commons CSV + Pydantic + Go"       java-commons-csv-pydantic-go-batching.json false "Java/CommonsCSV/Pydantic/Go rows=3 javaRecords=3 batches=2"
run "Java Jsoup + BeautifulSoup + Cheerio"   java-jsoup-bs4-cheerio.json false "Java/Jsoup/BeautifulSoup/Cheerio title=Poly Feed javaLinks=2 pyLinks=2 js=ALPHA\\|BETA"
run "Java OkHttp + HTTPX + Go retry"         java-okhttp-httpx-go-retry.json false "Java/OkHttp/HTTPX/Go host=api.example.test path=/v1/items attempts=2 workers=2 backoff=75ms"

# ── Category 8: Edge Runtime Contracts ───────────────────────────
echo "── Edge Runtime Contracts ──"
run "Stream proxy bridge"                    edge-stream-proxy.json false "Edge stream proxy labels=ALPHA\\|BETA\\|GAMMA length=3"
run "Opaque resource and job handles"        edge-resource-job-handles.json false "Edge resource closed=true result=receipt-ok"
run "Garbage generated resource/job manifest" runnable-resource-job-boundary.json false "resource/job boundary result receipt-ok"
run "Table copy and validation error bridges" edge-table-validation-bridges.json false "Edge table bridge west=30 errors=1"

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
