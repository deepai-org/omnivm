#!/bin/bash
# Runs example manifests through libomnivm.so from a CPython host.

set -uo pipefail

IMAGE="${OMNIVM_IMAGE:-omnivm:latest}"
PYTHON_BIN="${OMNIVM_PYTHON_BIN:-python3.14}"
SCRIPT="/build/scripts/run-manifest-libomnivm.py"
PASS=0
FAIL=0

pass() { PASS=$((PASS+1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL+1)); echo "  FAIL: $1 — $2"; }

run() {
  local name="$1"
  local manifest="$2"
  local expected="${3:-}"

  echo "--- libomnivm: $name ---"
  local output
  output=$(docker run --rm --entrypoint "$PYTHON_BIN" "$IMAGE" "$SCRIPT" "/build/examples/$manifest" 2>&1)
  local code=$?
  if [ "$code" -ne 0 ]; then
    fail "$name" "exit=$code output=$output"
    return
  fi
  if [ -n "$expected" ] && ! echo "$output" | grep -q "$expected"; then
    fail "$name" "missing '$expected' in output: $output"
    return
  fi
  pass "$name"
}

echo "=== libomnivm Manifest Tests ==="

run "Polyglot eval/exec/import/concat" manifest-test.json
run "Syntactic dominance" syntactic-dominance.json
run "If/else, while loops, recursion" controlflow-test.json
run "Params, mutability, assign operators" controlflow-manifest.json
run "Round-trip, accumulate, recursive chains" crossruntime-manifest.json
run "Foreach, try/catch, batch, large data" stress-test-2.json
run "Async/await, parallel, channels, select" stress-test-4.json
run "Channels, generators, spawn workers" stress-test-5.json
run "Spawn handles + channel capture contract" spawn-channel-contract.json
run "Cursed concurrency" cursed-concurrency.json "Processed"
run "Express.js + Python text processing" express-manifest.json
run "Pastebin multi-shard API" pastebin-manifest.json
run "BeautifulSoup + Cheerio + Go cache" beautifulsoup-cheerio-go-cache.json
run "Buffer sharing" buffer-sharing-manifest.json "Buffers released"
run "Data pipeline" data-pipeline-manifest.json
run "Django + Zod + Go HMAC endpoint" django-zod-go-crypto.json
run "Resource/job handles" edge-resource-job-handles.json
run "Stream proxy bridge" edge-stream-proxy.json
run "Table copy and validation error bridges" edge-table-validation-bridges.json
run "Express + NumPy + Go rate limit" express-numpy-go-rate-limit.json
run "Express + Pandas + Go workers" express-pandas-go-workers.json
run "FizzBuzz polyglot functions" fizzbuzz-polyglot-manifest.json
run "HTTPX + URL + Go retry workers" httpx-url-go-retry.json
run "Java Commons CSV + Pydantic + Go" java-commons-csv-pydantic-go-batching.json
run "Java Gson + Pandas + Zod + Express" java-gson-pandas-zod-express.json
run "Java Jsoup + BeautifulSoup + Cheerio" java-jsoup-bs4-cheerio.json
run "Java OkHttp + HTTPX + Go retry" java-okhttp-httpx-go-retry.json
run "Jinja2 + Marked + Go docs" jinja2-marked-go-docs.json
run "Nokogiri + Pandas + JS formatting" nokogiri-pandas-js-format.json
run "NumPy + Pandas + D3 + Go channels" numpy-pandas-d3-go-channels.json
run "Polyglot pipeline" polyglot-pipeline-manifest.json
run "Pydantic + Zod + Go events" pydantic-zod-go-events.json
run "Garbage generated resource/job manifest" runnable-resource-job-boundary.json "resource/job boundary result receipt-ok"
run "Garbage generated zero-copy table manifest" runnable-zero-copy-table-boundary.json "zero-copy table boundary"
run "SQLAlchemy + Lodash + Go batching" sqlalchemy-lodash-go-batching.json

echo ""
echo "=== libomnivm Results: $PASS passed, $FAIL failed ==="
if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
