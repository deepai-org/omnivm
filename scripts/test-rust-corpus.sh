#!/bin/bash
# Rust ecosystem corpus runner (Phase A of the Rust hardening plan).
#
# For each .poly in polyscript/examples/rust-ecosystem/, classify into a
# four-state result and ratchet against scripts/rust-corpus-expectations.txt:
#
#   parse-fail    polyc cannot compile the file at all
#   infer-fail    a manifest is produced but no Rust func_def/eval landed
#   compile-fail  the Rust unit failed to build (cargo/rustc error)
#   runtime-fail  the manifest ran but errored
#   pass          manifest execution completed (and, when an expectation
#                 substring is recorded, stdout contains it)
#
# Exit nonzero if any file regresses below its recorded expectation.
# Improvements are reported so the expectations file can be ratcheted up.
set -u

CORPUS_DIR="${CORPUS_DIR:-/build/polyscript/examples/rust-ecosystem}"
EXPECTATIONS="${EXPECTATIONS:-/build/scripts/rust-corpus-expectations.txt}"
WORKDIR=$(mktemp -d /tmp/rust-corpus-XXXXXX)

rank() {
    case "$1" in
        parse-fail) echo 0 ;;
        infer-fail) echo 1 ;;
        compile-fail) echo 2 ;;
        runtime-fail) echo 3 ;;
        pass) echo 4 ;;
        *) echo -1 ;;
    esac
}

classify() {
    local poly="$1"
    local name
    name=$(basename "$poly")
    local manifest="$WORKDIR/${name%.poly}.json"
    local runlog="$WORKDIR/${name%.poly}.run.log"

    if ! polyc "$poly" > "$manifest" 2> "$WORKDIR/${name%.poly}.polyc.err"; then
        echo "parse-fail"
        return
    fi
    if ! python3 - "$manifest" <<'PYEOF'
import json, sys
m = json.load(open(sys.argv[1]))
def rust_ops(ops):
    for op in ops:
        if op.get("bodyRuntime") == "rust" or op.get("runtime") == "rust":
            yield op
        for child in op.get("body", []) or []:
            yield from rust_ops([child])
sys.exit(0 if any(True for _ in rust_ops(m.get("ops", []))) else 1)
PYEOF
    then
        echo "infer-fail"
        return
    fi
    if ! timeout 300 manifest-runner "$manifest" > "$runlog" 2>&1; then
        if grep -q "rust compilation failed" "$runlog"; then
            echo "compile-fail"
        else
            echo "runtime-fail"
        fi
        return
    fi
    if grep -q "rust compilation failed" "$runlog"; then
        echo "compile-fail"
        return
    fi
    if grep -q "Manifest execution error" "$runlog"; then
        echo "runtime-fail"
        return
    fi
    echo "pass"
}

overall=0
printf "%-40s %-13s %-13s %s\n" "FILE" "EXPECTED" "ACTUAL" "VERDICT"
for poly in "$CORPUS_DIR"/*.poly; do
    name=$(basename "$poly")
    expected=$(awk -v f="$name" '$1 == f { print $2 }' "$EXPECTATIONS" 2>/dev/null)
    expected=${expected:-pass}
    want=$(awk -v f="$name" '$1 == f { $1=""; $2=""; sub(/^  */, ""); print }' "$EXPECTATIONS" 2>/dev/null)

    actual=$(classify "$poly")
    verdict="ok"
    if [ "$actual" = "pass" ] && [ -n "$want" ]; then
        runlog="$WORKDIR/${name%.poly}.run.log"
        if ! grep -qF "$want" "$runlog"; then
            actual="runtime-fail"
            verdict="WRONG-OUTPUT (wanted: $want)"
        fi
    fi
    if [ "$(rank "$actual")" -lt "$(rank "$expected")" ]; then
        verdict="REGRESSION"
        overall=1
        tail -5 "$WORKDIR/${name%.poly}".*.err "$WORKDIR/${name%.poly}".run.log 2>/dev/null | sed 's/^/    | /'
    elif [ "$(rank "$actual")" -gt "$(rank "$expected")" ]; then
        verdict="IMPROVED (ratchet the expectations file)"
    fi
    printf "%-40s %-13s %-13s %s\n" "$name" "$expected" "$actual" "$verdict"
done

exit $overall
