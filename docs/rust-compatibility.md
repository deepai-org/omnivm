# Rust Compatibility & Mixing Report

Status: measured 2026-06-11, on the gates that run in CI. Companion to
[`rust-runtime-design.md`](rust-runtime-design.md) (the architecture) and the
README's Rust Runtime section (the user-facing summary). Every number below
is produced by a ratcheted CI gate, not by assertion.

## The three evidence layers

| Layer | Gate | What it proves | Current state |
|---|---|---|---|
| Syntax | `polyscript/scripts/rust-registry-sweep.js` (ratchet: `scripts/rust-registry-sweep-expectations.txt`) | Real crate source files compile through the PolyScript pipeline with every top-level item **byte-identical** in the emitted unit and zero leakage into other runtimes | **98.5%** of 782 files across 26 crates (floor 98, 12 diagnosed known-fails) |
| Semantics | `scripts/test-rust-corpus.sh` (ratchet: `scripts/rust-corpus-expectations.txt`) | Idiomatic crate usage *runs* end-to-end mixed with Python/JS, with output assertions | **11/11** pass |
| Acceptance | `polyscript/examples/rust-review-service.poly` (e2e suite) | A four-language service runs unchanged under both the binary and `libomnivm.so` hosts | green |

## Syntax: most existing Rust parses unchanged

The two-regime front end (items sliced verbatim by a Rust-aware scanner;
expression-level mixing handled by the union grammar) was measured against
every `.rs` file of the pinned crates — including the crates' own internals,
which use far gnarlier Rust than user code typically does:

| Crate | Files clean | Notes |
|---|---|---|
| regex, csv-core, chrono, once_cell, itertools, base64, anyhow\*, thiserror | 100% (anyhow 91.7%) | lifetimes, generics, where-clauses, attribute forms |
| futures-util | 99.5% | dominated by `pin_project!` macro items — now scanner anchors |
| serde, serde_json, rayon, uuid, memchr, log, url, sha2, bytes | 95–100% | |

What this covers: lifetimes everywhere, generic fns/impls with where-clauses,
turbofish, `unsafe impl ... where`, data-carrying enums with attributes
between variants, multi-line `#[cfg_attr(...)]` / `#![inner]` attributes,
declarative-macro invocation items (`pin_project! { ... }`, `cfg_if! { ... }`),
raw strings at any hash depth, doc comments, `let-else`, async closures.

The 12 known-fails are diagnosed in the expectations file; the notable
classes: `const _: () = { ... };` blocks (runaway-guard tension),
paren-form `macro_rules!(...)` bodies, and a lone re-export type alias that
earns no Rust affinity. None affect typical application code.

## Semantics: what idiomatic crate usage looks like when mixed

The ecosystem corpus exercises each claim with another runtime in the loop
(four-state classification: parse/infer/compile/runtime-fail/pass):

- **tokio + reqwest** — timers, `join!`, a `LazyLock<Client>` static reused
  across calls. `use tokio::...` resolves against the omnivm re-export (one
  runtime per process; the version is pinned by the image).
- **sqlx** — an async in-memory SQLite pool with typed row mapping, awaited
  through the golden-thread re-park loop.
- **axum** — handlers as plain async fns + the routing DSL (served sockets
  need `executor = "multi"`; the default executor is golden-thread pumped).
- **rayon** — the CPU-parallel escalation (its pool initializes on first
  `par_iter`; the only default-posture thread exception besides crates'
  internal threads).
- **regex / chrono / itertools / anyhow / thiserror** — lifetimes, timezone
  arithmetic, generic combinators, and error chains that surface in Python
  as catchable `RuntimeError`s with full anyhow context in `err.message`.
- **Arrow C Data Interface** — pointer identity asserted in BOTH directions:
  the buffer Python exports is the buffer the Rust `DataFrame` reads, and
  the buffer Rust allocates is the buffer the returned Python polars frame
  reads. Large `Vec<u8>` returns cross by pointer (`omnivm::Bytes`), never
  base64.
- **Concurrency shapes** — a spawned Rust task relaying between manifest
  channels fed after the spawn (live `pending` semantics, not snapshot), and
  Python lazily iterating a Rust-produced stream.

## The boundary contract (the rules that matter when mixing)

1. **Exported fns take owned, concrete params.** `&str`/`&[T]` params get
   mechanical owned-data adapters generated under the original symbol;
   generic fns cannot cross (call through a concrete wrapper — the compiler
   emits a diagnostic). Internal helpers are unrestricted: lifetimes,
   borrows, generics, non-serde types all fine, because only fns referenced
   from outside the unit are exported.
2. **serde is the codec.** The author's declared shape is the one canonical
   projection: `#[serde(tag = "type")]` enums arrive in JS as discriminated
   unions and in Python as tagged dicts. `Result<T, E>` becomes the
   structured error envelope; panics become catchable `RuntimeError`s;
   aborts taint the worker (the honest path).
3. **DataFrames cross as Arrow** (C-Data pointers when host and consumer
   support it, IPC otherwise); declare `fn f(frame: DataFrame) -> DataFrame`
   and the shims handle it (`(df, json)` typed-kind extraction).
4. **Channels/streams/callables** use the universal manifest protocols:
   `omnivm::Channel` (live `recv_async`, `send_async`), `export_stream`
   (peers pull via `stream_next`), `omnivm::Callback` (peer functions as
   arguments, invoked over the async bridge hop).
5. **Threading**: sync Rust = golden thread, same stack, tokio never
   created. Async = current-thread runtime, golden thread parks in the
   reactor. Threads only via `go expr` on sync fns (blocking pool),
   `executor = "multi"`, or crates that spawn internally.

## Known limits (deliberate)

- **proc-macros are not expanded by the front end** — they're carried
  verbatim and expanded by rustc inside the unit (i.e. they work; the
  compiler just treats them as opaque text). Declarative macro *bodies* are
  likewise opaque balanced-delimiter groups.
- **pyo3-based crates**: unsupported (could in principle attach to the
  embedded CPython; explicitly out of scope).
- **native-tls / openssl-sys-style build.rs probing**: the prelude policy is
  rustls throughout; crates that hard-require system OpenSSL may fail to
  build in minimal images.
- **Unknown crates resolve online once** per image lifetime (resolution
  recorded next to the artifact); offline images fail closed with a hint —
  pin the crate in the prelude for reproducible offline builds.
- **`omni_value_t` typed crossing (Tier 3)** remains staged: scalar/value
  traffic rides the JSON value model today; tabular and byte traffic already
  bypass it via the pointer lanes.
- **Multi-file Rust (`mod x;` across files)** is not a `.poly` concept — a
  compilation unit is what one `.poly` file declares (plus the prelude).
  Bring multi-file crates in as dependencies instead.

## Reproducing the numbers

```bash
# syntax sweep (inside the image)
cd polyscript && node scripts/rust-registry-sweep.js \
  --ratchet ../scripts/rust-registry-sweep-expectations.txt

# semantics corpus
bash scripts/test-rust-corpus.sh

# both run automatically in the Docker tester stage
docker build --target tester .
```
