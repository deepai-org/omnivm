# Rust dogfood: porting tokio-rs/mini-redis into one .poly file

Status: written 2026-06-11 against branch `rust-runtime-design` (verified green
twice: once at the tree's morning state, once after the day's
stubs.go/loader.go churn). The example is
[`polyscript/examples/mini-redis-tokio.poly`](../polyscript/examples/mini-redis-tokio.poly).
It is deliberately **not** in `polyscript/examples/rust-ecosystem/`, so it does
not participate in the corpus ratchet.

This document is the honest field report the experiment was run for: what a
real port feels like, with every piece of friction kept. The headline is a
split verdict — **the Rust regime is genuinely excellent** (≈1,000 lines of
unmodified real-world tokio code parsed, compiled and ran on the first try
that the *Rust* was at fault for nothing), while **the mixing layer is where
all the blood is** (nine of the ten friction findings are about routing
Python/JS around the Rust, not about the Rust).

## What was ported

[tokio-rs/mini-redis](https://github.com/tokio-rs/mini-redis) (MIT), the
Tokio project's canonical teaching server, at `master` = `3d93b42bc363`
(fetched 2026-06-11). Ported files:

| upstream file | what | how |
|---|---|---|
| `src/db.rs` | `Db`/`DbDropGuard`/`Shared`/`State`/`Entry`, the BTreeSet expiration index, the spawned purge task (`tokio::select!` over `sleep_until` + `Notify`) | **whole file verbatim** |
| `src/parse.rs` | `Parse` cursor + `ParseError` | verbatim except 6 signatures (see below) |
| `src/frame.rs` | `Frame` enum, `array`/`push_bulk`/`push_int`/`to_error`, `PartialEq<&str>`, `Display` | verbatim subset (the RESP byte decoder is the socket edge; not ported) |
| `src/cmd/{get,set,publish,ping,unknown}.rs` | command structs, `parse_frames`, `into_frame` | verbatim; `apply` adapted (the I/O edge) |
| `src/cmd/mod.rs` | `Command` enum + `from_frame` dispatch | adapted subset (no `Subscribe`/`Unsubscribe` — welded to `Connection`/`Shutdown`) |
| `src/lib.rs` | the boxed `Error`/`Result` aliases | verbatim |

The replaced I/O edge: in mini-redis, `Cmd::apply(self, db, dst: &mut
Connection)` writes the response frame to a TCP socket framed by
`src/connection.rs`. In the port, `apply` **returns** the `Frame` and the
polyglot boundary is the wire: Python is the redis client (drives a script of
inline commands, asserts every reply), JavaScript is the reporting layer, and
~80 lines of clearly-marked NEW Rust play `src/server.rs` (a `OnceLock`-held
`DbDropGuard`, one request/response wrapper, pub/sub open/drain wrappers, a
clock-advance wrapper, and a live-entry census used to prove the background
purge task actually evicts keys).

## How unchanged is "as unchanged as possible"? (measured)

Counting non-blank lines in the ported region (everything from the lib.rs
aliases through `cmd/mod.rs`, excluding the NEW-CODE wrappers and harness):

- **1,002 non-blank ported lines, 962 byte-identical to upstream = 96.0%.**
- The 40 changed lines decompose as:
  - **22 lines: the port itself** (the `apply` adaptation any port would make:
    6 signatures, 5 `Ok(response)` returns, 4 `#[instrument(skip(...))]`
    attrs that lost their `dst` param, 7 doc-comment lines).
  - **12 lines: OmniVM-friction workarounds** — 6 parse.rs signatures
    qualified to `std::result::Result<T, ParseError>` (upstream's module-local
    two-arg `Result` collides with lib.rs's crate-root one-arg `Result<T>`
    alias once everything is one module — a single-file-flattening cost, not
    strictly OmniVM's fault), and 6 lines from the dependency-inference bug
    below (`use Command::*;` / `use ParseError::EndOfStream;` replaced by
    qualified variants).
  - 6 explanatory comment lines marking those workarounds.
- Structural flattening (not counted as line changes): per-file `use` headers
  merged into one 7-line block (duplicate `use` is E0252 in one module), and
  `mod`/`use crate::{Connection, ...}` plumbing dropped.

Everything load-bearing — the mutex discipline, the expiration BTreeSet
dance, `tokio::spawn` + `select!` + `Notify` purge loop, the broadcast
pub/sub map, every `parse_frames`, the `From` conversion chains, `Display` —
is byte-identical upstream code.

## The run

```text
$ polyc mini-redis-tokio.poly > m.json   # 0.15s
$ manifest-runner m.json                 # cold ~3.0s total, warm skips rustc
mini-redis port: server=ready checks=11/11 failures=[] fanout=1,1,0 \
  feed=RUST-ON-OMNIVM | SHIP-IT flash=gone-soon->(nil) live_keys=2->1
```

The 11 checks: PING, PING msg, SET/GET, GET missing → `(nil)`, overwrite,
unknown command → `ERR unknown command 'gput'` (the `Frame::Error` path),
three protocol-error replies straight from upstream's `ParseError` strings,
and the `SET ... XX` unsupported-option error. `fanout=1,1,0` is
`Frame::Integer` subscriber counts through `Db::publish`;
`feed=...` is two messages drained from a real `broadcast::Receiver`;
`flash=gone-soon->(nil)` and `live_keys=2->1` prove **the spawned purge task
ran while the golden thread was parked in a 250 ms await** — mini-redis's
`Db::get` never checks `expires_at`, so only the background task can make
that key disappear. Cold Rust unit compile for the ~1,200-line unit: **1.4 s**
(the prebuilt workspace/feature-union architecture pays off hard).
Deterministic across repeated runs, cold and warm.

## Friction log (chronological, complete)

Everything below was hit for real; the workaround for each is documented
inline in the .poly file too.

1. **`polyc` parse errors print no location.** First compile died with
   `Parse errors:\n  Unexpected token in expression` — nothing else, on a
   1,310-line file. `ParseError` already carries the token (line/column);
   `cli-manifest.ts` just doesn't print it. I had to write a private harness
   against `dist/` internals to learn the error was at line 1266.
2. **Other languages' reserved words poison Python identifiers.** The
   location turned out to be `for case in script:` — `case` (Ruby/Go/TS
   keyword) cannot be a Python variable name in the union grammar, and the
   message says "Unexpected token in expression" rather than anything about
   reserved words.
3. **Cargo dependency inference treats enum-variant imports as crates.**
   Upstream's idiomatic `use Command::*;` (cmd/mod.rs) and
   `use ParseError::EndOfStream;` (cmd/set.rs) became
   `Command = "*"` / `ParseError = "*"` in the generated Cargo.toml; failure
   surfaces minutes later as cargo's `no matching package found: Command`.
   The item scanner already knows every type the unit defines; first-segment
   matches against unit-local items should be skipped. Until then: verbatim
   mini-redis source does not build.
4. **Mentioning a fn name in a comment exports it — and can fail the build.**
   The export-set analysis is documented as conservative ("even in a
   comment"), but the consequence is brutal: my file-header comment said
   `purge_expired_tasks`, so that internal task fn got an export shim, and
   the shim demands serde on its params — `Arc<Shared>` isn't Deserialize,
   so the **whole unit failed to compile** with a trait error pointing into
   generated glue. The .poly now contains a comment carefully *not* naming
   the fn it describes, which is absurd in a nice way.
5. **A Python `def` fell back to the JavaScript runtime.** The first harness
   wrapped per-command checking in `def run_case(c):`. The resolver left that
   func_def runtime-less, the runner fell back to manifest
   `defaultRuntime: "javascript"`, and V8 was handed Python source —
   `SyntaxError: Unexpected identifier 'run_case'` at runtime. `def` is
   Python-definite syntax; this should be impossible.
6. **Top-level `for` loops bind the loop variable at manifest level only.**
   `for c in script:` + `got = handle_request(c[0])` evaluated the *argument
   expression* `c[0]` in JavaScript → `ReferenceError: c is not defined`.
   Variants fared no better: `req = c[0]` and even Python-only
   `req, want = c` tuple unpacking inside the loop body were routed to *Rust*
   (`cannot parse expression "(req , want = c)"`), and a plain
   `if got == want:` block inside the loop body is a parse error
   ("Expected 'else' in Python ternary"). A loop that calls a Rust fn with a
   loop-var argument — surely the most common shape in any driver script —
   currently has exactly one working spelling (a list comprehension).
7. **Python tuples were silently rebuilt as JavaScript comma expressions.**
   The worst find. `script = [(["PING"], "PONG"), ...]` was routed to the JS
   runtime, and source-reconstruct emitted `([\"PING\"] , \"PONG\")` — valid
   JS that evaluates to `"PONG"`. No error anywhere; the data was just
   wrong (`requests` became `"P"` slices downstream). Tuple syntax wasn't
   treated as Python evidence, and reconstruction didn't refuse the
   semantics-changing translation.
8. **Python→Rust calls cannot pass lists by value.** With the comprehension
   form working, `handle_request(req.split(" "))` died with
   `invalid type: map, expected a sequence`: the in-runtime bridge
   (`__omnivm_encode_arg`) passes scalars by value but wraps **any**
   non-scalar — including a plain `["SET", "color", "blue"]` — as an
   `__omnivm_runtime_ref__` descriptor map, which serde then rejects against
   `Vec<String>`. Boundary docs say "exported fns take owned, concrete
   params", but a `Vec<String>` param is only callable from top-level
   orchestration, not from inside Python code. The port's `handle_request`
   now takes one space-delimited `String` (real Redis calls these "inline
   commands", so the adaptation has a fig leaf — but it was forced, not
   chosen).
9. **JS string proxies leak through Python comprehensions.** Because the
   `requests` list literal was inferred as JavaScript (its twin
   `expectations`, same shape, went to Python — routing is unpredictable for
   plain data), its elements arrived in the Python comprehension as JS
   proxies; `.split(" ")` on one returns a JS array proxy that crosses into
   Rust as a map again. `str(req)` is the rescue spell.
10. **The Python ternary came back as JS.** The pass/fail comprehension
    `["ok" if got == want else f"FAIL ..." for ...]` was emitted as
    `[((got == want) ? "ok" : \`FAIL want=${want}...\`) for ...]` *inside a
    python eval op* — JS ternary and template literal in CPython. Same class
    as #7 (reconstruction across dialects), but at least this one would have
    failed loudly. Also: `[fan1, fan2, fan3].join(",")` — written as
    JavaScript — was routed to **Rust** because its operands were bound by
    rust evals (`unknown function "[fan_news_1, ...].join"`); a template
    literal pinned it to JS.

Minor notes, not counted: `#[instrument]` spans print as
`[rust:info] tracing::span: apply;` on every call with no apparent way to set
a level filter (harmless, noisy); rustc's mapped diagnostics show .poly
coordinates in the arrow line but unit-relative line numbers in the snippet
gutter of the same diagnostic, which is briefly disorienting.

## What delighted

- **The Rust front end did not miss once.** 962 verbatim lines of real tokio
  project code — doc comments, multi-line attrs, `tokio::select!`,
  `impl Drop`/`Display`/`PartialEq<&str>`, `From` chains, `pub(crate)`
  everywhere, a function-local `use std::collections::hash_map::Entry`
  shadowing a struct named `Entry` — parsed, sliced and carried
  byte-identical. Every single parse fight in this port was in the
  Python/JS/mixing layer. The registry-sweep investment shows.
- **The async model held under a genuinely stateful workload.** One spawned
  background task outlived every statement boundary, raced `sleep_until`
  against `Notify` correctly, and its observable effect (entry eviction)
  landed exactly when the golden thread parked. Pub/sub broadcast receivers
  buffered across separate exported calls. Nothing about "one re-exported
  tokio, golden-thread parks in the reactor" leaked into user-visible
  semantics.
- **Unknown-crate resolution is invisible.** `tracing` (with the
  `#[instrument]` proc-macro!) and `atoi` were not in the pin set; cargo
  resolved them online once and the unit built. The only tell was the log
  line.
- **Compile times are a non-event.** 0.15 s polyc, 1.4 s cold rustc for a
  ~1,200-line unit with tokio/bytes/tracing in the graph, 0 s warm. The
  iterate-in-Docker loop was bottlenecked by *my* debugging, never by builds.
- **Source-mapped rustc errors.** When the Result-alias collision hit, the
  E0107s pointed at the .poly lines with the real alias definition site
  shown. Error quality on the Rust side generally beat error quality on the
  routing side by a wide margin.
- **`crate::` semantics just work in the unit** — `crate::Result`,
  `crate::Error` from the flattened lib.rs aliases resolved fine, so
  upstream's `crate::Result<Set>` signatures stayed verbatim.

## Ranked improvement suggestions

1. **Plain data must cross Python→Rust calls by value.** Lists/dicts of
   JSON-able values should serialize, with runtime-refs reserved for
   genuinely stateful/unserializable objects (friction #8). This is the
   single biggest "the documented boundary contract isn't the real contract"
   gap, and it bites the most natural calling code first.
2. **Forbid semantics-changing source reconstruction.** If a statement is
   routed to runtime X, emitting it requires a dialect-faithful translation
   (tuples ≠ comma expressions, `a if c else b` ≠ `c ? a : b` left inside a
   python eval) — otherwise fail compilation with the routing decision in the
   message (friction #7, #10). #7 is the only *silent corruption* found all
   day and deserves priority for that reason alone.
3. **Never default-runtime a func_def whose declKeyword is
   language-definite** (`def` → python, `fn` → rust, `func` → go); make
   unresolved-runtime a compile error, not a runtime SyntaxError in the wrong
   VM (friction #5).
4. **Dependency inference: skip `use` roots that name unit-local items**
   (friction #3). The scanner already has the item table; this makes
   `use Enum::Variant;` — ubiquitous real Rust — work verbatim.
5. **Export-set analysis: comment mentions shouldn't break builds.** Either
   stop counting comments as references, or when a comment-only-referenced fn
   has non-exportable params, demote to internal with a warning instead of
   failing the unit with serde-trait errors in generated glue (friction #4).
6. **Print line:column in polyc parse errors** — the token is already on the
   error object (friction #1). Cheapest fix on this list; would have saved
   the most wall-clock per dollar.
7. **Make one looping idiom work end-to-end** — either real per-iteration
   binding for manifest-level `for` loops, or keep statements inside a
   Python-syntax loop in Python (friction #6). Document the blessed shape in
   the meantime (today it's: list comprehension, `str()` your proxies).
8. **A `--explain-routing` polyc mode** that dumps each statement's resolved
   runtime and *why*. Most of this log was me reverse-engineering routing
   decisions from runtime crashes; the resolver clearly has the information
   (its capture-boundary info diagnostics were the breadcrumb that cracked
   #6).
9. **Reserved-word diagnostics** ("`case` is reserved by the union grammar,
   rename the variable") instead of "Unexpected token in expression"
   (friction #2).
10. **Runtime log-level control for the Rust unit** (`tracing` span spam from
    `#[instrument]`; cosmetic).

## Reproducing

```bash
docker run --rm --entrypoint sh \
  -v "$PWD":/src -v omnivm-gopath:/go -v omnivm-gocache:/root/.cache \
  omnivm-builder:rust -c '
    (cd /src/runtime/rust && tar --exclude=./target --exclude="./units/u*" -cf - .) | tar -C /opt/omnivm-rust -xf -
    rm -rf /build && mkdir /build
    (cd /src && tar --exclude=./polyscript/node_modules --exclude=./polyscript/dist --exclude=./.git --exclude=./runtime/rust/target -cf - .) | tar -C /build -xf -
    cd /build && cp scripts/javascript_docker.go pkg/javascript/javascript.go && rm -f pkg/javascript/v8_bridge.cc
    cp scripts/jvm_docker.go pkg/jvm/jvm.go && bash scripts/build.sh
    cp -R /src/polyscript/node_modules polyscript/node_modules && cd polyscript && npm run build
    node dist/cli-manifest.js /src/polyscript/examples/mini-redis-tokio.poly > /tmp/m.json
    PATH=/build/bin:$PATH LD_LIBRARY_PATH=/usr/local/lib manifest-runner /tmp/m.json'
# expect: mini-redis port: server=ready checks=11/11 failures=[] fanout=1,1,0 \
#         feed=RUST-ON-OMNIVM | SHIP-IT flash=gone-soon->(nil) live_keys=2->1
```
