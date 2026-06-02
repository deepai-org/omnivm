#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GARBAGE_DIR="${GARBAGE_DIR:-"$ROOT/../garbage"}"
PASSENGER_FIXTURE="$ROOT/test/fixtures/passenger-django-polyscript"
IMAGE="${OMNIVM_IMAGE:-omnivm:latest}"
PYTHON_BIN="${PYTHON_BIN:-python3.14}"
RUNNER="${LIBOMNIVM_MANIFEST_RUNNER:-/build/scripts/run-manifest-libomnivm.py}"

if [ ! -d "$GARBAGE_DIR" ]; then
  echo "Garbage repo not found at $GARBAGE_DIR; set GARBAGE_DIR=/path/to/garbage" >&2
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
  "go-docs-popular-packages.poly"
  "python-fastapi-sqlalchemy-polars-docs.poly"
  "javascript-react-jsx-docs.poly"
  "go-http-handler-docs.poly"
  "compat-python-service.py"
  "compat-go-status.go"
  "compat-order-schema.ts"
)

echo "=== .poly -> manifest -> CPython-hosted libomnivm smoke ==="
for example in "${examples[@]}"; do
  name="${example%.poly}"
  manifest="$TMP/manifests/$name.json"

  echo "compile $example"
  (cd "$GARBAGE_DIR" && npm run polyc -- "examples/$example" -o "$manifest" >/dev/null)

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
done

fixture="$TMP/passenger-django"
cp -R "$PASSENGER_FIXTURE" "$fixture"

echo "run Passenger-style Django .poly import fixture across fresh workers"
for worker in 1 2 3; do
  docker run --rm \
    --entrypoint python3-polyscript \
    -e POLYSCRIPT_COMPILER="node /garbage/dist/cli-manifest.js" \
    -e POLYSCRIPT_CACHE_DIR=/tmp/polyscript-cache \
    -v "$fixture":/tmp/passenger-django:ro \
    -v "$GARBAGE_DIR":/garbage:ro \
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
}
body = b"".join(application(environ, start_response)).decode()
assert captured["status"].startswith("200"), captured
assert captured["headers"].get("X-Poly-Fixture") == "middleware", captured
assert body == "poly-feature-ok:GET:/poly/:u-42:poly", body
print(body)'
done

echo "poly libomnivm smoke passed (${#examples[@]} examples + Passenger-style Django import fixture x3)"
