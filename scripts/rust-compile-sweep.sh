#!/bin/bash
# Level-2 registry COMPILE sweep (the compile-truth oracle for Rust support).
#
# Level 1 (polyscript/scripts/rust-registry-sweep.js) proves real crate files
# SLICE byte-exactly through the PolyScript front end. THIS sweep proves a
# deterministic sample of the same registry files also COMPILE as real Rust
# units through the production toolchain — pkg/rust Toolchain.BuildUnit, i.e.
# the same cargo workspace, dependency inference, runtime-alias injection,
# export-shim completion, and artifact cache the manifest executor uses.
#
# Per registry file:
#   1. wrap as
#          <content>
#
#          fn __omnivm_probe() -> i64 { 42 }
#
#          print(__omnivm_probe())
#      The probe fn is load-bearing: the codegen export-set analysis only
#      emits the Rust unit into the manifest when at least one fn is
#      referenced OUTSIDE the unit — a bare `print("done")` tail classifies
#      every file fn as internal-only and produces a manifest with NO rust
#      ops at all. The probe forces exactly one (serde-trivial) export, so
#      the func_def carries the full unit: every real item of the file plus
#      our generated glue.
#   2. compile with cli-manifest (polyc) -> manifest JSON       [parse-fail]
#   3. extract every rust func_def unit and build it through the REAL Go
#      toolchain via scripts/rust-unit-compile — ONE process for the whole
#      sweep, so the workspace flock and artifact/dedup caches are shared.
#
# Classes:
#   compile-ok    the unit built as a real cdylib
#   compile-fail  rustc/cargo rejected it (first error line captured)
#   parse-fail    polyc could not compile the wrapped file (level 1 gates this)
#   no-unit       manifest produced but no rust func_def landed (front-end bug)
#   skip          out of scope: empty, >--max-kb (default 200KB), or item-free
#                 file (unit contains only the injected probe)
#
# Every sampled file is additionally tagged "crate-private" when it
# references `crate::` / `super::` paths: a registry file is a module of a
# larger crate, not a standalone unit, so those paths cannot resolve in
# isolation. The report shows the pass-rate both including and excluding
# those files; the ratchet floor applies to the headline (including) rate.
#
# Usage:
#   scripts/rust-compile-sweep.sh [--sample N | --all] [--ratchet FILE]
#       [--registry DIR] [--crates a,b,c] [--max-kb N] [--out FILE]
#
#   --sample N   deterministic selection: every k-th file of the sorted
#                eligible list, k = floor(total/N). Default 150.
#   --all        sweep every eligible file (cargo-build heavy; measure first).
#   --ratchet F  compare against expectations file F (same scheme as the
#                level-1 sweep: "min-pass-rate <pct>" then sorted
#                "<class> <path> — <first error line>" known-fail lines).
#                Exit nonzero on regressions; print a ratchet-update reminder
#                on improvements.
#   --out F      write the current failure list to F (for regenerating the
#                expectations file).
#
# Runs in the tester stage (registry at /opt/cargo/registry/src, workspace at
# /opt/omnivm-rust) and in the iteration container with the repo at /src.
set -u

# ── Defaults ────────────────────────────────────────────────────────────────
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT="${REPO_ROOT:-$(dirname "$SCRIPT_DIR")}"
REGISTRY="${OMNIVM_RUST_REGISTRY:-}"
SAMPLE=150
ALL=0
MAX_KB=200
RATCHET=""
OUT=""
CRATES=""

# Curated crate set — keep in sync with DEFAULT_CRATES in
# polyscript/scripts/rust-registry-sweep.js (the level-1 sweep).
DEFAULT_CRATES="anyhow base64 bytes chrono csv csv-core futures-core futures-util itertools itoa log memchr once_cell rayon rayon-core regex regex-automata regex-syntax ryu serde serde_core serde_json sha2 thiserror url uuid"

while [ $# -gt 0 ]; do
    case "$1" in
        --sample) SAMPLE="$2"; shift 2 ;;
        --sample=*) SAMPLE="${1#*=}"; shift ;;
        --all) ALL=1; shift ;;
        --ratchet) RATCHET="$2"; shift 2 ;;
        --ratchet=*) RATCHET="${1#*=}"; shift ;;
        --registry) REGISTRY="$2"; shift 2 ;;
        --registry=*) REGISTRY="${1#*=}"; shift ;;
        --crates) CRATES="$2"; shift 2 ;;
        --crates=*) CRATES="${1#*=}"; shift ;;
        --max-kb) MAX_KB="$2"; shift 2 ;;
        --max-kb=*) MAX_KB="${1#*=}"; shift ;;
        --out) OUT="$2"; shift 2 ;;
        --out=*) OUT="${1#*=}"; shift ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

# ── Locate registry, compiler, helper ───────────────────────────────────────
if [ -z "$REGISTRY" ]; then
    base=/opt/cargo/registry/src
    if [ ! -d "$base" ]; then
        echo "registry not found at $base; pass --registry DIR (run inside omnivm-builder)" >&2
        exit 2
    fi
    REGISTRY="$base/$(ls "$base" | sort | head -1)"
fi
if [ ! -d "$REGISTRY" ]; then
    echo "registry dir not found: $REGISTRY" >&2
    exit 2
fi

CLI="$REPO_ROOT/polyscript/dist/cli-manifest.js"
if [ -f "$CLI" ]; then
    POLYC=(node "$CLI")
elif command -v polyc >/dev/null; then
    POLYC=(polyc)
else
    echo "no PolyScript compiler: $CLI missing and polyc not in PATH (npm run build first)" >&2
    exit 2
fi

WORK=$(mktemp -d /tmp/rust-compile-sweep-XXXXXX)
echo "[compile-sweep] building scripts/rust-unit-compile (the BuildUnit driver)..."
if ! (cd "$REPO_ROOT" && go build -o "$WORK/rust-unit-compile" ./scripts/rust-unit-compile); then
    echo "failed to build scripts/rust-unit-compile" >&2
    exit 2
fi

# ── Enumerate + sample ──────────────────────────────────────────────────────
crates="${CRATES:-$DEFAULT_CRATES}"
crates=$(echo "$crates" | tr ',' ' ')

: > "$WORK/all.tsv"      # rel<TAB>abs, every .rs file of the crate set
skip_empty=0
skip_large=0
for crate in $(echo "$crates" | tr ' ' '\n' | sort); do
    found=0
    for dir in "$REGISTRY/$crate"-[0-9]*; do
        [ -d "$dir/src" ] || continue
        found=1
        find "$dir/src" -name '*.rs' -type f | sort | while read -r abs; do
            printf '%s\t%s\n' "${abs#"$REGISTRY"/}" "$abs"
        done >> "$WORK/all.tsv"
    done
    [ "$found" = 1 ] || echo "note: crate not pinned in registry, skipping: $crate" >&2
done
sort -o "$WORK/all.tsv" "$WORK/all.tsv"
total=$(wc -l < "$WORK/all.tsv")

: > "$WORK/eligible.tsv"
while IFS=$'\t' read -r rel abs; do
    size=$(stat -c %s "$abs" 2>/dev/null || echo 0)
    if [ "$size" -eq 0 ]; then
        skip_empty=$((skip_empty + 1))
    elif [ "$size" -gt $((MAX_KB * 1024)) ]; then
        skip_large=$((skip_large + 1))
    else
        printf '%s\t%s\n' "$rel" "$abs" >> "$WORK/eligible.tsv"
    fi
done < "$WORK/all.tsv"
eligible=$(wc -l < "$WORK/eligible.tsv")

if [ "$ALL" = 1 ] || [ "$SAMPLE" -ge "$eligible" ]; then
    cp "$WORK/eligible.tsv" "$WORK/sample.tsv"
    stride=1
else
    stride=$((eligible / SAMPLE))
    [ "$stride" -lt 1 ] && stride=1
    awk -F'\t' -v k="$stride" -v n="$SAMPLE" \
        '(NR - 1) % k == 0 && picked < n { print; picked++ }' \
        "$WORK/eligible.tsv" > "$WORK/sample.tsv"
fi
sampled=$(wc -l < "$WORK/sample.tsv")
echo "[compile-sweep] registry: $REGISTRY"
echo "[compile-sweep] files: $total total, $eligible eligible (skips: empty=$skip_empty too-large=$skip_large), sampled $sampled (stride $stride)"

# ── Phase 1: wrap + polyc, tag crate-private ────────────────────────────────
# results.tsv: class<TAB>rel<TAB>private(0|1)<TAB>message
: > "$WORK/results.tsv"
: > "$WORK/manifests.tsv"   # rel<TAB>manifest-path for the compile phase
declare -A PRIVATE
parse_start=$(date +%s)
while IFS=$'\t' read -r rel abs; do
    priv=0
    # Crate-private markers — the file is a module of a larger crate, not a
    # standalone unit: bare crate::/super:: paths (`$crate::` resolves to the
    # unit itself and does NOT count), pub(crate)/pub(super)/pub(in ...)
    # visibility (at unit root: "too many leading `super` keywords"), and
    # file-backed `mod x;` declarations (sibling files don't exist standalone).
    if grep -Pq '(?<![\$A-Za-z0-9_])(crate|super)::' "$abs" 2>/dev/null \
        || grep -Eq '(^|[^$A-Za-z0-9_])(crate|super)::' "$abs" \
        || grep -Eq 'pub[[:space:]]*\([[:space:]]*(crate|super|in[[:space:]])' "$abs" \
        || grep -Eq '^[[:space:]]*(pub([[:space:]]*\([^)]*\))?[[:space:]]+)?mod[[:space:]]+[A-Za-z_][A-Za-z0-9_]*[[:space:]]*;' "$abs"; then
        priv=1
    fi
    PRIVATE[$rel]=$priv

    poly="$WORK/poly/$rel.poly"
    mf="$WORK/manifest/$rel.json"
    mkdir -p "$(dirname "$poly")" "$(dirname "$mf")"
    { cat "$abs"; printf '\n\nfn __omnivm_probe() -> i64 { 42 }\n\nprint(__omnivm_probe())\n'; } > "$poly"
    if timeout 60 "${POLYC[@]}" "$poly" > "$mf" 2> "$mf.err"; then
        printf '%s\t%s\n' "$rel" "$mf" >> "$WORK/manifests.tsv"
    else
        msg=$(grep -v '^[[:space:]]*$' "$mf.err" | grep -v '^Parse errors:$' | head -1 \
              | sed 's/^[[:space:]]*//' | tr '\t' ' ' | cut -c1-300)
        printf 'parse-fail\t%s\t%s\t%s\n' "$rel" "$priv" "${msg:-polyc failed}" >> "$WORK/results.tsv"
    fi
done < "$WORK/sample.tsv"
parse_secs=$(( $(date +%s) - parse_start ))
echo "[compile-sweep] front-end phase: $(wc -l < "$WORK/manifests.tsv") manifests in ${parse_secs}s"

# ── Phase 2: compile every unit through the real toolchain ─────────────────
compile_start=$(date +%s)
"$WORK/rust-unit-compile" < "$WORK/manifests.tsv" > "$WORK/compile.tsv"
helper_rc=$?
compile_secs=$(( $(date +%s) - compile_start ))
if [ "$helper_rc" -ne 0 ]; then
    echo "rust-unit-compile exited $helper_rc" >&2
    exit 2
fi
while IFS=$'\t' read -r status rel msg; do
    priv="${PRIVATE[$rel]:-0}"
    case "$status" in
        ok)      printf 'compile-ok\t%s\t%s\t%s\n'   "$rel" "$priv" "$msg" ;;
        fail)    printf 'compile-fail\t%s\t%s\t%s\n' "$rel" "$priv" "$msg" ;;
        trivial) printf 'skip\t%s\t%s\tno-items: %s\n' "$rel" "$priv" "$msg" ;;
        no-unit) printf 'no-unit\t%s\t%s\t%s\n'      "$rel" "$priv" "$msg" ;;
        *)       printf 'no-unit\t%s\t%s\tdriver: %s\n' "$rel" "$priv" "$msg" ;;
    esac >> "$WORK/results.tsv"
done < "$WORK/compile.tsv"
sort -t$'\t' -k2,2 -o "$WORK/results.tsv" "$WORK/results.tsv"
echo "[compile-sweep] compile phase: ${compile_secs}s"

# ── Report ──────────────────────────────────────────────────────────────────
awk -F'\t' '
{ count[$1]++; if ($3 == 1) priv[$1]++; n++ }
END {
    judged = n - count["skip"]
    ok = count["compile-ok"] + 0
    printf "\nresults over %d sampled files (judged %d, skip %d):\n", n, judged, count["skip"] + 0
    for (c in count) printf "  %-13s %4d  (crate-private: %d)\n", c, count[c], priv[c] + 0
    rate = judged > 0 ? 100 * ok / judged : 100
    printf "\nheadline pass-rate: %.1f%% (%d/%d judged)\n", rate, ok, judged

    privJudged = 0; privOk = 0
    for (c in count) if (c != "skip") privJudged += priv[c] + 0
    privOk = priv["compile-ok"] + 0
    pubJudged = judged - privJudged
    pubOk = ok - privOk
    printf "crate-private files (reference crate::/super:: — modules of a larger crate,\n"
    printf "not standalone units): %d of %d judged\n", privJudged, judged
    if (privJudged > 0)
        printf "  pass-rate over crate-private files only: %.1f%% (%d/%d)\n", 100 * privOk / privJudged, privOk, privJudged
    if (pubJudged > 0)
        printf "  pass-rate excluding crate-private files: %.1f%% (%d/%d)\n", 100 * pubOk / pubJudged, pubOk, pubJudged
}' "$WORK/results.tsv"

# Root-cause histogram for compile failures.
awk -F'\t' '$1 == "compile-fail" {
    msg = $4
    if (msg ~ /failed to get|network|Unable to update registry|no matching package/)
        cls = "dep-resolution (crate not pinned / unknown root inferred as a dep)"
    else if (msg ~ /E0583/)
        cls = "crate-private: mod declaration points at a sibling file"
    else if ((msg ~ /E0432|E0433/) && ($3 == 1 || msg ~ /`(crate|super)`|crate::|super::/))
        cls = "crate-private path (crate::/super:: — out of standalone scope)"
    else if (msg ~ /E0432|E0433/)
        cls = "unresolved external import (dependency inference miss)"
    else if (msg ~ /E0658|feature/)
        cls = "feature-gated code (#[cfg(feature)] / nightly)"
    else if ($3 == 1)
        cls = "other, in a crate-private-tagged file"
    else
        cls = "other (INSPECT: candidate shim/glue bug)"
    count[cls]++; nfail++
}
END {
    if (nfail == 0) { print "\nno compile failures."; exit }
    print "\ncompile-fail root-cause histogram:"
    for (c in count) printf "  %4d  %s\n", count[c], c
}' "$WORK/results.tsv"

failures=$(awk -F'\t' '$1 != "compile-ok" && $1 != "skip"' "$WORK/results.tsv" | sort -t$'\t' -k2,2)
if [ -n "$failures" ]; then
    echo ""
    echo "failures:"
    echo "$failures" | awk -F'\t' '{ tag = ($3 == 1) ? " [crate-private]" : ""; printf "%s %s — %s%s\n", $1, $2, $4, tag }'
fi

if [ -n "$OUT" ]; then
    echo "$failures" | awk -F'\t' 'NF { printf "%s %s — %s\n", $1, $2, $4 }' > "$OUT"
    echo ""
    echo "failure list written to $OUT"
fi

echo ""
echo "workdir: $WORK (manifests + per-file logs)"

# ── Ratchet ─────────────────────────────────────────────────────────────────
if [ -z "$RATCHET" ]; then
    exit 0
fi
if [ ! -f "$RATCHET" ]; then
    echo "expectations file not found: $RATCHET" >&2
    exit 2
fi

awk -F'\t' -v expfile="$RATCHET" '
BEGIN {
    floor = -1
    nknown = 0
    while ((getline line < expfile) > 0) {
        sub(/^[ \t]+/, "", line)
        if (line == "" || line ~ /^#/) continue
        split(line, parts, /[ \t]+/)
        if (floor < 0) {
            if (parts[1] != "min-pass-rate" || parts[2] + 0 != parts[2]) {
                printf "expectations file %s: first line must be \"min-pass-rate <percent>\"\n", expfile > "/dev/stderr"
                fatal = 1
                exit 2
            }
            floor = parts[2] + 0
            continue
        }
        known[parts[2]] = parts[1]
        nknown++
    }
    if (floor < 0) {
        printf "expectations file %s: missing min-pass-rate line\n", expfile > "/dev/stderr"
        fatal = 1
        exit 2
    }
}
{
    n++
    if ($1 == "skip") { skips++; next }
    if ($1 == "compile-ok") { ok++; next }
    failing[$2] = $1
    failmsg[$2] = $4
}
END {
    if (fatal) exit 2
    judged = n - skips
    rate = judged > 0 ? 100 * ok / judged : 100
    newf = 0; fixed = 0
    for (p in failing) if (!(p in known)) newf++
    for (p in known) if (!(p in failing)) fixed++
    printf "\nratchet: floor %s%% | measured %.1f%% | known-fail %d | new %d | fixed %d\n", floor, rate, nknown, newf, fixed

    bad = 0
    if (rate < floor) {
        printf "RATCHET FAIL: pass-rate %.1f%% dropped below floor %s%%\n", rate, floor > "/dev/stderr"
        bad = 1
    }
    if (newf > 0) {
        print "RATCHET FAIL: files failing that are NOT in the known-fail list:" > "/dev/stderr"
        for (p in failing) if (!(p in known))
            printf "  %s %s — %s\n", failing[p], p, failmsg[p] > "/dev/stderr"
        bad = 1
    }
    changed = 0
    for (p in failing) if ((p in known) && known[p] != failing[p]) {
        if (!changed) { print "note: known-fail files changed class (still failing, not a regression):"; changed = 1 }
        printf "  %s (%s -> %s)\n", p, known[p], failing[p]
    }
    if (!bad && fixed > 0) {
        printf "ratchet-update reminder: these known-fail files now PASS; remove them from %s (and consider raising the floor):\n", expfile
        for (p in known) if (!(p in failing)) printf "  %s\n", p
    }
    exit bad
}' "$WORK/results.tsv"
exit $?
