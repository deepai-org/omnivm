#!/usr/bin/env bash
# Cooperative-boundary proof: runs the asyncio + gevent harnesses in blocking and
# cooperative modes and asserts the contrast. Runs INSIDE the omnivm-dev container
# (or the tester image). Expects the PolyScript compiler at /build/polyscript-dev
# and pyomnivm at /build/pyomnivm.
set -euo pipefail

POLY="${POLYSCRIPT_DIR:-/build/polyscript}"
PY="${PYTHON_BIN:-python3.14}"
HARNESS_DIR="${HARNESS_DIR:-/build/test/cooperative}"

BLOCK_MAX="${BLOCK_MAX:-2}"     # blocking mode must starve the loop/hub
COOP_MIN="${COOP_MIN:-20}"      # cooperative mode must interleave heavily

# Each proof: a .poly whose work the host should be able to interleave with.
#   cooperative-boundary-proof : many cross-runtime ops on the Golden Thread
#   cooperative-goroutine-join : a pure-Go goroutine runs in parallel; the host
#                                stays responsive across the wait() join
PROOFS="cooperative-boundary-proof cooperative-goroutine-join"

ticks_of() { # parse "RESULT ... ticks=N ..." from a harness run
  sed -n 's/.*RESULT .*ticks=\([0-9][0-9]*\).*/\1/p' <<<"$1" | tail -1
}

fail=0
for proof in $PROOFS; do
  manifest="/tmp/${proof}.json"
  echo
  echo "##### proof: ${proof} #####"
  ( cd "$POLY" && node dist/cli-manifest.js "examples/${proof}.poly" -o "$manifest" >/dev/null )
  for host in asyncio gevent; do
    harness="$HARNESS_DIR/proof_${host}.py"
    bout="$($PY "$harness" "$manifest" blocking 2>&1)" || { echo "$bout"; echo "FAIL: ${proof}/${host} blocking errored"; fail=1; continue; }
    bt="$(ticks_of "$bout")"; bt="${bt:-unknown}"
    cout="$($PY "$harness" "$manifest" cooperative 2>&1)" || { echo "$cout"; echo "FAIL: ${proof}/${host} cooperative errored"; fail=1; continue; }
    ct="$(ticks_of "$cout")"; ct="${ct:-unknown}"
    echo "$bout" | grep -E '^\['
    echo "$cout" | grep -E '^\['

    if [ "$bt" = unknown ] || [ "$ct" = unknown ]; then
      echo "FAIL: ${proof}/${host}: could not parse ticks (blocking=$bt cooperative=$ct)"; fail=1; continue
    fi
    if [ "$bt" -gt "$BLOCK_MAX" ]; then
      echo "FAIL: ${proof}/${host}: blocking did not starve (ticks=$bt > $BLOCK_MAX)"; fail=1
    elif [ "$ct" -lt "$COOP_MIN" ]; then
      echo "FAIL: ${proof}/${host}: cooperative did not interleave (ticks=$ct < $COOP_MIN)"; fail=1
    else
      echo "PASS: ${proof}/${host}: blocking ticks=$bt -> cooperative ticks=$ct"
    fi
  done
done

echo
if [ "$fail" -ne 0 ]; then echo "COOPERATIVE PROOF: FAIL"; exit 1; fi
echo "COOPERATIVE PROOF: PASS"
