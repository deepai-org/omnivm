# Rust Runtime Design: Compiled Peer with Golden-Thread Async

This document is the plan of record for adding Rust as a sixth peer runtime,
with support parity targeted at the Go lane (compiled peer), not the
interpreter lane. It covers the PolyScript syntax level, the runtime/bridge
level, the events/async model, and ecosystem compatibility. The async design
went through several revisions; only the converged result is recorded here,
with the rejected alternatives noted where the reasoning matters.

Status: agreed design, pre-implementation.

## Positioning

Rust slots in structurally next to Go: source compiles to a shared object,
loads in-process, and calls other runtimes through the bridge. It improves on
the Go lane on three axes:

- **One build mode.** Rust always compiles to a `cdylib` with a stable C ABI,
  loaded via `dlopen`/`dlsym`. The same artifact works in the OmniVM binary and
  in `libomnivm.so` c-shared deployments — no `-buildmode=plugin` /
  `-buildmode=c-shared` split.
- **Cancellable async.** Tokio futures can be cancelled at `.await` points
  (`JoinHandle::abort()`, `tokio::time::timeout`). The watchdog gets a real
  preemption row for async Rust where Go has none.
- **Native Arrow.** `arrow-rs` and `polars` implement the Arrow C Data
  Interface directly (`arrow::ffi`), which is the Tier 2 path in
  [`bridge-performance-plan.md`](bridge-performance-plan.md) and the existing
  `table` op. Rust gets the best zero-copy story of any guest with no adapter
  code.

The honest gap: there is no string eval. `omnivm.call("rust", "6 * 7")` means
compiling a crate. With a warm cache it is a `dlopen`; cold, it is a cargo
build. "Equally good support" for Rust means parity at the `.poly` /
manifest / `func_def` level, with inline-string calls supported but slow on
first compile — the same deal Go already has.

## Syntax Level (PolyScript)

PolyScript's model is an unrestricted syntactic union with evidence-based
runtime inference — no `rs { }` blocks. `OmniRuntime.Rust` already exists in
`polyscript/src/runtime-resolver/types.ts`; this section activates it.

### Definite Rust evidence (Pass 1)

- `fn` keyword — unique among all six languages (Go `func`, JS `function`,
  Python/Ruby `def`).
- Macro invocations `ident!(` — `println!`, `vec!`, `format!`, `json!`. No
  other donor language has this form.
- `.await` postfix (JS `await` is prefix).
- `match { pat => ... }`, `impl`, `trait`, `pub`, `let mut`, `&mut`,
  `#[derive(...)]`, lifetimes `'a`, `use path::path;`.
- Builtin/method affinity entries: `Some`/`None`/`Ok`/`Err`, `.unwrap()`,
  `.expect()`, `Vec::new()`, `tokio`, `serde_json`.

Import provenance extends cleanly: each language keeps its native import form,
and `use tokio::time::sleep;` is unambiguously Rust.

### Ambiguity resolutions

| Token | Conflict | Resolution |
|---|---|---|
| `=>` | JS arrow (currently definite-JS) | Demote to contextual: `=>` inside a `match { }` body is Rust; elsewhere JS. The `match` keyword is the anchor. |
| `let` | JS `let` | Bare `let x =` stays ambiguous; `let mut` and `let x: i32 =` are definite Rust. Tie-break via Pass 2 propagation. |
| `::` | Ruby `Foo::Bar` | Ruby paths are Constant-cased; `lowercase::lowercase` is Rust-leaning evidence. |
| `?` postfix | JS `?.` and ternary | Weak evidence only; resolve via the type system when the receiver is `Result`-typed. |
| `struct` | Go `type X struct` | Keyword order: `struct X {` is Rust. |
| `panic` | Go-affine builtin today | `panic!(` (macro) is Rust; bare `panic(` stays Go. |

The `=>` demotion touches inference for existing JS-heavy `.poly` files and
must be validated against the full compiler test corpus before/after.

### Mechanical touchpoints

- Add `"rs"` to the `RuntimeTag` union (`ast.ts`) and the parser tag list
  (`expr-prefix.ts`) so `@rs(expr)` is the escape hatch.
- Include Rust in `isCompiledRuntime()` alongside Go.
- Emit `func_def` ops with `bodyRuntime: "rust"` and a complete compilation
  unit in `source`, parallel to `emitGoFuncDef`.

### Typed boundaries

Rust func_defs map PolyScript canonical types directly instead of erasing to
`interface{}` as Go does: `int → i64`, `float → f64`, `string → String`,
`bytes → Vec<u8>`, `list<T> → Vec<T>`, untyped → `omnivm::Value` (a
serde-enabled enum mirroring the planned `omni_value_t` tagged union from
Tier 3 of the bridge plan). `Result<T, E>` at an exported boundary compiles to
the structured error envelope; `Option<T>` maps to null.

## Runtime Level (`pkg/rust/`)

Follows `pkg/golang/` structurally.

### Compilation and caching

- Always `cdylib`; never a Go-style plugin.
- Cache key: SHA256 of (source + resolved `Cargo.lock`) into the existing
  `/tmp/omnivm-plugins/<hash>.so` scheme.
- Dependency inference: `use` statement crate roots map to a generated
  `Cargo.toml` (with the crates.io hyphen/underscore mapping), versions pinned
  to an in-image lockfile. Mirrors Go import inference in `emitGoFuncDef`.
- Error enhancement extends naturally:
  `error[E0432]: unresolved import 'serde'` → a "add the crate" hint.

### Bridge ABI

Every compiled unit exports the Rust analog of the Go `SetBridge` symbol
contract, and depends on an `omnivm` crate that wraps it:

```rust
#[no_mangle]
pub extern "C" fn omnivm_set_bridge(
    call: extern "C" fn(*const c_char, *const c_char) -> *mut c_char,
    free: extern "C" fn(*mut c_char),
) { ... }

// user-facing surface in the omnivm crate
let v: String = omnivm::call("python", "2 ** 100")?;
let users = omnivm::call_typed::<Vec<User>>("python", "get_users()")?;
```

### Panic and error envelope

Compiled units build with `panic = "unwind"`. Every exported entry point wraps
the body in `std::panic::catch_unwind`, converting panics into the structured
error envelope with `RUST_BACKTRACE` frames. A stray `.unwrap()` must become a
Python `omnivm.RuntimeError`, never a worker abort. `anyhow`/`thiserror`
errors walk `source()` chains into the existing `cause_chain` field.

### Registration touchpoints

`.rs` in `extToLang` (`pkg/cli/cli.go`), `-rust` legacy flag, the runtime map
and init loop in `cmd/omnivm/main.go`, `RuntimeID` in `pkg/engine/engine.go`,
a watchdog runtime ID, `:rust`/`:rs` REPL commands, shutdown order, and
`init_runtimes(["rust"])` in the Python package.

## Events / Async: Golden-Thread-First Tokio

Rust is the only guest without a GIL/GVL/isolate lock, so the temptation is to
give it a background multi-threaded executor. The agreed design rejects that
as the default: Rust behaves like every other guest, with threads available
only as lazy or explicit escalations.

**Unifying principle: the golden thread parks in exactly one reactor at a
time, and that reactor watches the other reactors' fds.** This is what the
epoll dispatcher already does with the libuv backend fd; during a Rust await,
ownership of the wait transfers temporarily to tokio.

### Default executor

One lazily-initialized **current-thread** tokio `Runtime` plus a `LocalSet`,
owned by the Rust peer, living on the golden thread.

- Pumped like libuv on dispatcher cycles:
  `local_set.block_on(&rt, async { tokio::task::yield_now().await })` drives
  ready tasks, fires expired timers, and polls the I/O reactor — inline, zero
  additional threads.
- `spawn_local` permits `!Send` futures, which is friendlier for generated
  glue than multi-thread `Send + 'static` bounds.
- Fork safety is trivial: no worker threads exist, so the runtime is dropped
  and recreated post-fork with no `pthread_atfork` choreography.
- Tokio exposes neither its driver fd nor its next-timer deadline as public
  API. Pumped-mode tokio timers therefore ride the dispatcher heartbeat;
  precise wakeups come from the parked-await path below.

### The await op: a re-park loop

A manifest `await` on a Rust future parks the golden thread inside tokio's
reactor via `block_on(select!(...))`. The select arms vary by mode:

| Arm | c-shared mode | Binary mode | Purpose |
|---|---|---|---|
| `timeout(deadline, fut)` | yes | yes | The awaited future, watchdog deadline composed inline. |
| Heartbeat pump | yes | yes | Periodically pump libuv/asyncio so cross-runtime-entangled futures (e.g. a Rust future awaiting a manifest channel fed by a JS interval) make progress. Required in **both** modes: while parked, no call boundaries occur, so c-shared cooperative pumping would otherwise stop entirely. |
| `taskFD` readable | no | yes | Dispatcher work arrived — exit the park, let the dispatcher run it (including fast-channel calls), then re-park. Without this arm, a parked await starves `CallFast` head-of-line. |
| Outbound bridge queue | step 2c | step 2c | See "async bridge hop" below. |

Because the park can exit and resume, the pending future cannot be a local: it
is a stored `Pin<Box<dyn Future>>` keyed by the await handle, polled across
multiple `block_on` entries. Abandonment (watchdog timeout, scope cleanup,
manifest error between parks) releases the boxed future through the same
handle-table discipline as every other runtime-owned value — quiet,
idempotent, recorded in cleanup details. Dropping the box is tokio
cancellation, so no bespoke teardown exists.

For the JS pump arm, use `uv_backend_timeout()` (public libuv API) to compute
`min(heartbeat_floor, uv_backend_timeout(loop))` instead of a fixed interval —
a JS `setInterval(…, 7)` feeding a Rust channel gets pumped at 7ms cadence
rather than riding a 10ms heartbeat with jitter. asyncio publishes no
equivalent (digging into `loop._scheduled` is private API); Python stays on
the fixed heartbeat.

### taskFD arm: edge-triggered, two consumers

Tokio's `AsyncFd` is edge-triggered, and the dispatcher's own epoll also
watches the task eventfd. The tokio arm must observe readiness and exit
**without reading the fd** — the dispatcher drains it as normal — with
`clear_ready` placed so that re-parking after a drain re-arms correctly. Two
epoll instances on one eventfd is fine; mixing edge-triggered observation with
another consumer's reads is demo-works/stress-fails territory and carries
explicit required tests (below).

### Reentrancy: guard first, async hop later

A Rust future that calls `omnivm::call("python", ...)` synchronously from
inside `block_on` re-enters Rust when Python calls back, and
`Runtime::block_on` panics when nested. The stress suite's 18-level 4-runtime
mutual recursion makes this path non-optional.

- **Ships first (2a): reentrancy guard.** On entry, `Handle::try_current()`;
  if already inside the runtime, drive the future via `spawn_local` + the pump
  loop instead of `block_on`.
- **End state (2c): async bridge hop.** Outbound bridge calls from async
  context become an async hop: the future suspends on a oneshot, the select
  loop's outbound-bridge arm fires, the park **exits**, the Python call runs
  on the golden thread as plain dispatcher work with no active runtime
  context, the oneshot completes, the loop re-parks. The correctness argument:
  because the park exits before the bridge call runs, an inner Rust entry
  calls the driver loop fresh — a second `block_on` that is nested on the OS
  call stack but **sequential** in tokio's view, which is exactly the
  distinction that does not panic. Mutual recursion becomes alternating layers
  of suspended boxed futures and plain synchronous frames. The guard's
  drive-via-pump branch becomes dead code once all outbound calls hop; the two
  coexist during the transition.

### Threads as lazy or explicit escalations

- **`spawn_blocking` / blocking I/O:** tokio's blocking pool is already lazy
  (threads created on first use, reaped after idle). Cap with
  `max_blocking_threads`. Zero threads until user code asks.
- **rayon:** the legitimate "GIL released, CPU-bound" parallelism story. Its
  global pool initializes lazily on first `par_iter`; a `.poly` file that
  never touches rayon never creates it. Documented as the explicit
  parallelism escalation.
- **Long-lived servers (axum/actix):** an accept loop on a pumped
  current-thread runtime only progresses during ticks — fine in binary mode,
  degraded in c-shared mode. Gated behind an explicit knob
  (`runtime.rust.executor = "multi"` or `#[omnivm::executor(multi)]`). When
  set: multi-thread tokio runtime, eventfd-based completion delivery into the
  dispatcher epoll, background progress in c-shared mode, and a `rust_tokio`
  owner-dispatch target that can genuinely report `supported: true`
  (tokio `Waker` is `Send + Sync`; `Handle::spawn` works from any thread).
- README caveat: some crates spawn threads internally regardless (TLS, DNS
  resolver paths). "Zero threads" is a default posture, not an enforced
  guarantee.

### Watchdog matrix row

*Cooperatively cancellable when async; Go-equivalent when compute-bound on the
default executor; thread-isolated under `executor = "multi"`.*

- Async Rust: deadline enforced inline via `timeout(...)` in the park;
  `JoinHandle::abort()` cancels at the next `.await` point. No taint.
- CPU-bound Rust between awaits: blocks the golden thread like any CPU-bound
  host call — deadline detection + `worker_tainted()` + recycle, same as Go
  plugins.

### Manifest contract mapping

- `async fn` / `.await` in `.poly` → the existing `await` op via the re-park
  loop above.
- `go expr` spawn syntax works on Rust funcs unchanged → `tokio::spawn`
  (`spawn_local` on the default executor; `spawn_blocking` for sync fns),
  resolving the standard `SpawnHandle`.
- Manifest channels ↔ `tokio::sync::mpsc` via thin forwarding adapters; the
  `omnivm` crate exposes `omnivm::chan::<T>("name")` returning
  `Sender`/`Receiver`, the receiver implementing `futures::Stream`.
- `futures::Stream` / `AsyncRead` values crossing a boundary map onto the
  existing stream-proxy protocol (`stream_next` / `stream_cancel`).

## Ecosystem Compatibility

Consistent with the bridge plan's rule: no library-specific fast paths.
Popular crates work because their values expose ordinary protocols.

- **serde / serde_json:** is the value boundary. `copy` semantics =
  `Serialize`/`Deserialize` against the JSON envelope; when Tier 3
  `omni_value_t` lands, serde targets it directly and skips JSON.
- **tokio / reqwest / hyper / axum / actix:** covered by the async design.
  Route handlers call `omnivm::call(...)` through the bridge (hopping to the
  host thread in c-shared mode).
- **rayon:** works as the parallel CPU escalation; pure Rust work violates no
  runtime locks.
- **arrow-rs / polars / ndarray:** Arrow C Data Interface via `arrow::ffi`
  against the `table` op. polars DataFrame ↔ pandas crossing is zero-copy
  with no adapter code.
- **anyhow / thiserror:** `source()` chains → `cause_chain`. **tracing:** a
  subscriber in the `omnivm` crate forwards spans into `CallMetrics` /
  `SetOnCallDone`.
- **pyo3-based crates:** unsupported initially (could in principle attach to
  the embedded CPython; explicitly out of scope).

### Compile-time mitigation

1. **Prelude workspace baked into the Docker image:** top ~50 crates (tokio,
   serde, reqwest, rayon, polars, anyhow, itertools, regex, chrono, …)
   pre-compiled with a pinned `Cargo.lock`, vendored registry for offline
   builds, shared `target/` dir baked in. A `.poly` file using only prelude
   crates compiles user code only.
2. **sccache** plus the SHA256 plugin cache for everything else.

## Build Order

1. **Peer runtime:** `pkg/rust/` cdylib compile/cache/dlopen, bridge ABI,
   `omnivm` crate, panic→envelope. Gets `omnivm run main.rs`, `-rust 'code'`,
   manifest `func_def`.
2. **Async, staged:**
   - **2a (smallest correct ship, covers production):** c-shared
     `block_on(select!(timeout(fut), heartbeat-pump))` + reentrancy guard +
     boxed re-park loop with handle-table-managed future storage.
   - **2b (binary mode):** taskFD arm as edge-observed `AsyncFd` (no reads),
     `uv_backend_timeout()`-aware JS pump cadence, fixed heartbeat for
     asyncio.
   - **2c (escalation):** async bridge hop (outbound arm + oneshot, retires
     the guard); `executor = "multi"` knob; eventfd completion delivery;
     `rust_tokio` owner-dispatch target.
3. **PolyScript:** Rust evidence in Pass 1 + method tables, `@rs` tag, the
   `=>`/`let` disambiguation (full corpus run before/after), `emitRustFuncDef`
   with typed signatures.
4. **Package layer:** prelude workspace image, `use`→`Cargo.toml` inference,
   Arrow FFI for the `table` op.

## Required Tests

- Assert `Handle::try_current().is_err()` at every bridge entry (the
  sequential-not-nested `block_on` invariant).
- Eventfd two-consumer pair: park → flood taskFD → drain → re-park → write →
  assert wakeup; **and** the inverse — write during the drained-but-not-yet-
  re-parked window, re-park, assert the wakeup still arrives (the
  edge-triggered re-arm race).
- Re-run the 18-level mutual-recursion stress with Rust in the chain, under
  both the guard (2a) and the hop (2c).
- Entangled-future regression: Rust future awaiting a manifest channel fed by
  a JS interval, in both binary and c-shared modes.
- Abandoned-await release: watchdog timeout mid-park leaves no leaked boxed
  future (handle-table diagnostics clean).
