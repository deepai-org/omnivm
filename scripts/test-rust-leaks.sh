#!/bin/bash
# Rust runtime leak gate (corpus level).
#
# Runs the leak-heavy corpus files — the C-Data pointer round trip, the
# spawned channel relay, and the Python-consumed Rust stream — under
# manifest-runner with OMNIVM_RUST_STATS_AT_EXIT=1 and asserts that the
# crate's liveness counters drained to zero before shutdown:
#
#   live_objects          exported object/stream handles released
#   live_cdata_shells     Arrow C-Data shells released after import
#   live_byte_buffers     pointer-lane byte buffers released by buffer_id
#   pending_futures       stored futures all driven/awaited
#   pending_bridge_calls  no bridge hop left half-completed
#
# Unlike the corpus runner this gate is absolute: drained or failed — there
# is no expectations ratchet. The in-process companion (one executor, every
# lane, 50 iterations) is pkg/manifest TestRustLeakGate.
set -u

CORPUS_DIR="${CORPUS_DIR:-/build/polyscript/examples/rust-ecosystem}"
LEAK_FILES="${LEAK_FILES:-rust-arrow-cdata-pointer.poly rust-channel-relay.poly rust-stream-python-iter.poly}"
COUNTERS="live_objects live_cdata_shells live_byte_buffers pending_futures pending_bridge_calls"
WORKDIR=$(mktemp -d /tmp/rust-leaks-XXXXXX)

overall=0
printf "%-40s %s\n" "FILE" "VERDICT"
for name in $LEAK_FILES; do
    poly="$CORPUS_DIR/$name"
    manifest="$WORKDIR/${name%.poly}.json"
    runlog="$WORKDIR/${name%.poly}.run.log"

    if [ ! -f "$poly" ]; then
        printf "%-40s %s\n" "$name" "FAIL (missing corpus file)"
        overall=1
        continue
    fi
    if ! polyc "$poly" > "$manifest" 2> "$WORKDIR/${name%.poly}.polyc.err"; then
        printf "%-40s %s\n" "$name" "FAIL (polyc)"
        tail -5 "$WORKDIR/${name%.poly}.polyc.err" | sed 's/^/    | /'
        overall=1
        continue
    fi
    if ! timeout 300 env OMNIVM_RUST_STATS_AT_EXIT=1 manifest-runner "$manifest" > "$runlog" 2>&1; then
        printf "%-40s %s\n" "$name" "FAIL (run)"
        tail -10 "$runlog" | sed 's/^/    | /'
        overall=1
        continue
    fi
    if grep -q "Manifest execution error" "$runlog"; then
        printf "%-40s %s\n" "$name" "FAIL (manifest error)"
        tail -10 "$runlog" | sed 's/^/    | /'
        overall=1
        continue
    fi

    stats_line=$(grep "^OMNIVM_RUST_STATS_AT_EXIT " "$runlog" | tail -1 | sed 's/^OMNIVM_RUST_STATS_AT_EXIT //')
    if [ -z "$stats_line" ]; then
        printf "%-40s %s\n" "$name" "FAIL (no rust stats at exit — manifest never crossed into rust?)"
        overall=1
        continue
    fi

    leaks=$(python3 - "$stats_line" $COUNTERS <<'PYEOF'
import json, sys
stats = json.loads(sys.argv[1])
bad = [f"{key}={stats.get(key)}" for key in sys.argv[2:] if stats.get(key) != 0]
print(" ".join(bad))
PYEOF
    )
    if [ -n "$leaks" ]; then
        printf "%-40s %s\n" "$name" "LEAK ($leaks)"
        printf "    | stats: %s\n" "$stats_line"
        overall=1
    else
        printf "%-40s %s\n" "$name" "ok (drained)"
    fi
done

exit $overall
