#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GARBAGE_DIR="${GARBAGE_DIR:-"$ROOT/../garbage"}"
IMAGE="${OMNIVM_IMAGE:-omnivm:latest}"
PYTHON_BIN="${PYTHON_BIN:-python3.14}"
RUNNER="${LIBOMNIVM_MANIFEST_RUNNER:-/build/scripts/run-manifest-libomnivm.py}"

if [ ! -d "$GARBAGE_DIR" ]; then
  echo "Garbage repo not found at $GARBAGE_DIR; set GARBAGE_DIR=/path/to/garbage" >&2
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
)

echo "=== .poly -> manifest -> CPython-hosted libomnivm smoke ==="
for example in "${examples[@]}"; do
  name="${example%.poly}"
  manifest="$TMP/manifests/$name.json"

  echo "compile $example"
  (cd "$GARBAGE_DIR" && npm run polyc -- "examples/$example" -o "$manifest" >/dev/null)

  echo "run $example"
  docker run --rm \
    --entrypoint "$PYTHON_BIN" \
    -v "$TMP/manifests":/tmp/poly-libomnivm-smoke:ro \
    -v "$TMP/var-data":/var/data:ro \
    "$IMAGE" \
    "$RUNNER" "/tmp/poly-libomnivm-smoke/$name.json"
done

echo "poly libomnivm smoke passed (${#examples[@]} examples)"
