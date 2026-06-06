#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLYSCRIPT_DIR="${POLYSCRIPT_DIR:-${GARBAGE_DIR:-"$ROOT/../polyscript-compiler"}}"
PASSENGER_FIXTURE="$ROOT/test/fixtures/passenger-django-polyscript"
IMAGE="${OMNIVM_IMAGE:-omnivm:latest}"
PYTHON_BIN="${PYTHON_BIN:-python3.14}"
RUNNER="${LIBOMNIVM_MANIFEST_RUNNER:-/usr/local/bin/run-manifest-libomnivm.py}"

if [ ! -d "$POLYSCRIPT_DIR" ]; then
  echo "PolyScript compiler checkout not found at $POLYSCRIPT_DIR; set POLYSCRIPT_DIR=/path/to/polyscript-compiler" >&2
  exit 2
fi
if [ ! -d "$PASSENGER_FIXTURE" ]; then
  echo "Passenger/Django fixture not found at $PASSENGER_FIXTURE" >&2
  exit 2
fi

TMP="$(mktemp -d)"
cleanup() {
  rm -rf "$TMP"
}
trap cleanup EXIT

mkdir -p "$TMP/manifests" "$TMP/var-data"
touch \
  "$TMP/var-data/alpha.py" \
  "$TMP/var-data/beta.txt" \
  "$TMP/var-data/a.log" \
  "$TMP/var-data/.DS_STORE"

examples=(
  "syntactic-dominance.poly"
  "cursed-polyglot.poly"
  "python-docs-popular-packages.poly"
  "javascript-docs-popular-packages.poly"
  "javascript-error-fields.poly"
  "python-error-js-catch.poly"
  "javascript-error-cause-details.poly"
  "ruby-java-error-fields.poly"
  "go-docs-popular-packages.poly"
  "beautifulsoup-cheerio-go-cache.poly"
  "python-map-collision-docs.poly"
  "python-fastapi-sqlalchemy-polars-docs.poly"
  "javascript-react-jsx-docs.poly"
  "go-http-handler-docs.poly"
  "java-map-collision-docs.poly"
  "ruby-map-collision-docs.poly"
  "javascript-map-collision-docs.poly"
  "native-memory-docs.poly"
  "python-lifecycle-docs.poly"
  "python-executor-docs.poly"
  "python-generator-js-consume.poly"
  "python-generator-js-cancel.poly"
  "javascript-generator-python-consume.poly"
  "python-async-generator-js-consume.poly"
  "vertical-order-review-app.poly"
  "compat-python-service.py"
  "compat-go-status.go"
  "compat-order-schema.ts"
)

echo "=== .poly -> manifest -> CPython-hosted libomnivm smoke ==="
for example in "${examples[@]}"; do
  name="${example%.poly}"
  manifest="$TMP/manifests/$name.json"

  echo "compile $example"
  (cd "$POLYSCRIPT_DIR" && npm run polyc -- "examples/$example" -o "$manifest" >/dev/null)

  echo "run $example"
  if ! output=$(docker run --rm \
      --entrypoint "$PYTHON_BIN" \
      -v "$TMP/manifests":/tmp/poly-libomnivm-smoke:ro \
      -v "$TMP/var-data":/var/data:ro \
      "$IMAGE" \
      "$RUNNER" "/tmp/poly-libomnivm-smoke/$name.json" 2>&1); then
    printf '%s\n' "$output"
    exit 1
  fi
  printf '%s\n' "$output"
  if [ "$example" = "compat-go-status.go" ] && [[ "$output" != *"ok:200"* ]]; then
    echo "expected compat-go-status.go main() output to contain ok:200, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "java-map-collision-docs.poly" ] && [[ "$output" != *'Java map collision docs {"items":2,"firstItem":"alpha","keys":2,"firstKey":"id","then":"field-then","get":"field-get","close":"field-close","length":2,"count":7}'* ]]; then
    echo "expected Java map collision natural access output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "ruby-map-collision-docs.poly" ] && [[ "$output" != *'Ruby map collision docs {"items":2,"firstItem":"alpha","keys":2,"firstKey":"id","then":"field-then","get":"field-get","close":"field-close","length":2,"count":7}'* ]]; then
    echo "expected Ruby map collision natural access output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "javascript-map-collision-docs.poly" ] && [[ "$output" != *"JavaScript map collision docs py=2:alpha:2:id:field-then:field-get:field-close:2:7 ruby=2:alpha:2:id:field-then:field-get:field-close:2:7 java=7:2:field-then"* ]]; then
    echo "expected JavaScript-owned map collision natural access output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "native-memory-docs.poly" ] && [[ "$output" != *"Native memory docs py=4:1:4 js=4:97:100 java=4:7:8"* ]]; then
    echo "expected native memory docs output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "python-lifecycle-docs.poly" ] && [[ "$output" != *"Lifecycle docs inside alpha:1:2:1:2:field-close"* || "$output" != *"Lifecycle docs closed True events=enter,exit"* ]]; then
    echo "expected Python lifecycle docs context-manager cleanup output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "python-executor-docs.poly" ] && [[ "$output" != *"Python executor docs rows alpha:5:row-close|beta:4:row-close"* || "$output" != *"Python executor docs shutdown True"* ]]; then
    echo "expected Python ThreadPoolExecutor docs output with shutdown, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "python-generator-js-consume.poly" ] && [[ "$output" != *"Python generator JS consume 0:0:1:row-close|1:1:2:row-close"* ]]; then
    echo "expected Python generator lazy JS consumption output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "javascript-error-fields.poly" ] && [[ "$output" != *"JavaScript error fields Error:field-check:true:exec[javascript]"* ]]; then
    echo "expected JavaScript native error identity fields, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "python-error-js-catch.poly" ] && [[ "$output" != *"Python error JS catch python:python:ValueError:bad order:true:exec[python]"* ]]; then
    echo "expected Python error caught naturally in JavaScript with fidelity fields, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "javascript-error-cause-details.poly" ] && [[ "$output" != *"JavaScript error Python details javascript:javascript:TypeError:outer type:True:exec[javascript]:E_OUTER:order.id:Error:inner cause"* ]]; then
    echo "expected JavaScript error details and cause caught naturally in Python, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "ruby-java-error-fields.poly" ] && [[ "$output" != *"Ruby Java error fields ruby=ruby:ruby:RuntimeError:bad ruby:true:exec[ruby] java=java:java:IllegalStateException:bad java:true:exec[java]"* ]]; then
    echo "expected Ruby/Java error fields with concrete Java exception type, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "beautifulsoup-cheerio-go-cache.poly" ]; then
    if [[ "$output" != *"BeautifulSoup/Cheerio/Go scrape POLYGLOT RUNTIME links=2 workers=2 key=https://example.test/articles/poly hrefs=/docs,/api"* ]]; then
      echo "expected BeautifulSoup/Cheerio/Go cache output, got: $output" >&2
      exit 1
    fi
    if [[ "$output" == *"panic"* || "$output" == *"panicked"* ]]; then
      echo "expected BeautifulSoup/Cheerio/Go cache output without Go plugin panic text, got: $output" >&2
      exit 1
    fi
  fi
  if [ "$example" = "python-map-collision-docs.poly" ] && [[ "$output" != *'Python map collision docs object={"then":"called:manual","items":2,"firstItem":"alpha","keys":2,"firstKey":"id","get":"field-get","close":"field-close","length":2,"count":7} dict={"items":2,"firstItem":"alpha","keys":2,"firstKey":"id","then":"field-then","get":"field-get","close":"field-close","length":2,"count":7}'* ]]; then
    echo "expected Python map collision natural access output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "python-generator-js-cancel.poly" ] && [[ "$output" != *"Python generator JS cancel 0:break|1:break errors=0:throw|stop-stream"* || "$output" != *"Python generator JS closed ['break', 'throw']"* ]]; then
    echo "expected Python generator JS cancellation/error release output, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "javascript-generator-python-consume.poly" ] && [[ "$output" != *"JavaScript generator Python consume 0:break|1:break"* || "$output" != *"JavaScript generator Python closed break"* || "$output" != *"JavaScript generator Python produced 0|1"* ]]; then
    echo "expected JavaScript generator Python consumption output with stable yielded rows, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "python-async-generator-js-consume.poly" ] && [[ "$output" != *"Python async generator JS consume 0:break|1:break"* || "$output" != *"Python async generator JS closed ['break']"* ]]; then
    echo "expected Python async generator JS consumption output with early cleanup, got: $output" >&2
    exit 1
  fi
  if [ "$example" = "vertical-order-review-app.poly" ] && [[ "$output" != *"Vertical order app order=ord-42"* || "$output" != *"ruby=fiber-active"* ]]; then
    echo "expected vertical order app output with Ruby lifecycle text, got: $output" >&2
    exit 1
  fi
done

fixture="$TMP/passenger-django"
cp -R "$PASSENGER_FIXTURE" "$fixture"

echo "run Passenger-style Django .poly import fixture across fresh workers"
for worker in 1 2 3; do
  docker run --rm \
    --entrypoint python3-polyscript \
    -e POLYSCRIPT_COMPILER="node /polyscript-compiler/dist/cli-manifest.js" \
    -e POLYSCRIPT_CACHE_DIR=/tmp/polyscript-cache \
    -v "$fixture":/tmp/passenger-django:ro \
    -v "$POLYSCRIPT_DIR":/polyscript-compiler:ro \
    "$IMAGE" \
    -c 'import io, sys
sys.path.insert(0, "/tmp/passenger-django")
from passenger_wsgi import application
captured = {}
def start_response(status, headers):
    captured["status"] = status
    captured["headers"] = dict(headers)
environ = {
    "REQUEST_METHOD": "GET",
    "PATH_INFO": "/poly/",
    "SCRIPT_NAME": "",
    "SERVER_NAME": "testserver",
    "SERVER_PORT": "80",
    "SERVER_PROTOCOL": "HTTP/1.1",
    "wsgi.version": (1, 0),
    "wsgi.url_scheme": "http",
    "wsgi.input": io.BytesIO(b""),
    "wsgi.errors": sys.stderr,
    "wsgi.multithread": False,
    "wsgi.multiprocess": True,
    "wsgi.run_once": False,
    "HTTP_X_REQUEST_ID": "req-42",
}
body = b"".join(application(environ, start_response)).decode()
assert captured["status"].startswith("200"), captured
assert captured["headers"].get("X-Poly-Fixture") == "middleware", captured
import json
payload = json.loads(body)
assert payload["status"] == "poly-feature-ok", payload
assert payload["method"] == "GET", payload
assert payload["path"] == "/poly/", payload
assert payload["user_id"] == "u-42", payload
assert payload["feature"] == "poly", payload
assert payload["visits"] == 3, payload
assert payload["request_id"] == "req-42", payload
assert payload["meta_request_id"] == "req-42", payload
assert payload["items"] == [
    {"kind": "feature", "value": "poly"},
    {"kind": "request", "value": "req-42"},
], payload
print(body)'
done

echo "poly libomnivm smoke passed (${#examples[@]} examples + Passenger-style Django import fixture x3)"
