#!/usr/bin/env bash
# Go -> guest callbacks + auto-marshal: compile each callback .poly and assert the
# result. Covers direct (Golden Thread) and goroutine (auto-marshaled) invocation,
# Python + JS callbacks, nested cross-runtime, concurrent goroutines, error
# propagation, and fire-and-forget (must not hang). Runs INSIDE the container.
set -uo pipefail

POLY="${POLYSCRIPT_DIR:-/build/polyscript}"
PY="${PYTHON_BIN:-python3.14}"
RUN="${GO_CB_RUNNER:-/build/test/go-callbacks/run.py}"

# poly-name | expect-substring | mode(ok|err|nohang)
CASES=(
  "go-guest-callback-direct|result=42|ok"
  "auto-marshal-callback-proof|result=42|ok"
  "go-callback-python|direct=15 spawned=21|ok"
  "go-callback-nested|callback 105|ok"
  "go-callback-multi|multi callbacks|ok"
  "go-callback-error|callback boom|err"
  "go-callback-fire-forget|fire-and-forget submitted|nohang"
)

fail=0
for entry in "${CASES[@]}"; do
  IFS='|' read -r name expect mode <<<"$entry"
  manifest="/tmp/gocb-${name}.json"
  if ! ( cd "$POLY" && node dist/cli-manifest.js "examples/${name}.poly" -o "$manifest" >/dev/null 2>&1 ); then
    echo "FAIL ${name}: compile error"; fail=1; continue
  fi
  out=$(timeout -s KILL 60 "$PY" "$RUN" "$manifest" 2>&1); rc=$?
  case "$mode" in
    ok)
      if [ $rc -eq 0 ] && printf '%s' "$out" | grep -qF "$expect"; then
        echo "PASS ${name}: ${expect}"
      else
        echo "FAIL ${name} (rc=$rc): want '${expect}', got: $(printf '%s' "$out" | tail -1 | cut -c1-80)"; fail=1
      fi ;;
    err)
      # the guest callback raises; the error must surface (nonzero rc) and mention it
      if [ $rc -ne 0 ] && printf '%s' "$out" | grep -qF "$expect"; then
        echo "PASS ${name}: error surfaced (${expect})"
      else
        echo "FAIL ${name} (rc=$rc): expected surfaced error '${expect}'"; fail=1
      fi ;;
    nohang)
      # must complete (not be SIGKILLed by timeout) and print its marker
      if [ $rc -eq 0 ] && printf '%s' "$out" | grep -qF "$expect"; then
        echo "PASS ${name}: completed without hang"
      else
        echo "FAIL ${name} (rc=$rc, 137=hang): ${expect}"; fail=1
      fi ;;
  esac
done

echo
if [ "$fail" -ne 0 ]; then echo "GO CALLBACKS: FAIL"; exit 1; fi
echo "GO CALLBACKS: PASS"
