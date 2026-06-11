# Donor-runtime lane parity

Status of the "fast lane" machinery per donor runtime, after the Rust lane
work (typed outbound calls, source-batched streams) was spread to the python
and JS guest proxies. "Lane" here means the set of boundary optimizations a
guest can use instead of the baseline one-JSON-envelope-per-operation bridge:

- **typed eval / typed callbacks** — scalars cross the FFI as tagged unions
  (polyglot.Value), not JSON strings,
- **batched stream pulls** — `{"op":"stream_next","max_n":64}` moves up to 64
  values per bridge hop, answered in the plural `{"done","values":[...]}`
  shape with `done` riding WITH the final values,
- **bytes / Arrow C-Data** — byte tables and dataframes cross by buffer
  pointer, never JSON-escaped.

## Per-donor status

| Lane                  | rust | python | javascript | ruby | java |
|-----------------------|------|--------|------------|------|------|
| typed eval/callbacks  | full | yes    | yes (Docker runtime) | yes | FFI wired, unused by guest code |
| batched stream pulls  | full (source-batched) | yes | yes | no (single pull) | no (single pull) |
| bytes / Arrow C-Data  | full | yes (C-Data + table bytes) | yes (byte tables via getBuffer) | partial | byte tables → `byte[]` |
| capture injection     | native values | JSON+materializer | JSON+materializer | JSON+materializer | JSON strings (`setCapture`) |

- **rust: full.** Typed outbound calls, `Dyn` iteration sugar, and
  source-batched streams (the rust `next` object method accepts `{"n": K}`,
  so one OBJECT call moves up to 64 values — see
  `rustStreamNextBatch`/`rust_stream_batch_contract_test.go`). The rust
  consumer also sends `"pending":true` so live channels can report
  open-but-empty without being closed out from under it.
- **python: C-Data + bytes + batched.** The stream proxy
  (`__OmniVMStreamProxy` in `pkg/manifest/captures.go`) now requests
  `max_n=64` and drains a client-side raw buffer; a 200-value stream costs 4
  bridge hops instead of 201 (~50x). Materialization stays per-`__next__` so
  mid-stream materialization errors surface at the same consumption point as
  the singular protocol.
- **javascript: batched** (this change) with the same buffer design as
  python (`__omnivm_make_stream_proxy`, same file). Byte-table chunks still
  materialize through `__omnivm_stream_chunk_value` per value.
- **ruby: single-pull.** The ruby proxy (captures.go ~4731) still sends one
  `stream_next` per value and has no client buffer. The host already answers
  `max_n` for every handle kind, so closing this gap is guest-side only:
  mirror the python buffer (raw buffer + `remote_done` + cancel-skip after
  done), ~60 lines of generated ruby plus golden-test updates.
- **java: single-pull, JSON-everywhere guest code.** See below.

## How the host batches (and when it refuses to)

`handleStreamNextBatch` (pkg/manifest/stubs.go) services `max_n > 1`:

- `RustStreamRef` → source-batched: one object call moves the whole chunk.
- `ChanRef`, reflect channels, readers, materialized local iterators → the
  generic loop pulls eagerly up to `max_n`; an open-but-empty channel without
  `"pending":true` reads as done (snapshot semantics, unchanged).
- `RuntimeRef`-backed streams (JS/python generators, ruby enumerators) are
  **clamped to one value per envelope** — still in the plural shape — because
  generators run user code per pulled value: eager pulls would make
  per-value side effects and finally-block timing observable (a consumer
  that breaks after 2 values must not have driven the generator 64 steps;
  see `polyscript/examples/javascript-generator-python-consume.poly` and
  `TestGuestGeneratorBatchPullStaysLazy`).

The python/JS proxies never send `"pending":true` (that flag is the rust
live-channel consumer's); their snapshot semantics are byte-identical to the
pre-batch protocol. When `done` rides with final values the host has already
released the handle, so the proxies detach finalizers / skip the
`stream_cancel` wire call while draining the buffered tail; an explicit
close mid-drain drops the unconsumed tail (consumed values stay consumed).

## Java lane: what exists, what closing the gap costs

The REAL JVM runtime used in Docker images is `scripts/jvm_docker.go`
(`pkg/jvm/jvm.go` is a local shim whose `SetTypedCallback` is a no-op).
Findings:

- **Typed infrastructure exists at the FFI layer.** `jvm_docker.go`
  implements `SetTypedCallback` (`omnivm_jvm_set_typed_callback`) and
  `EvalTyped` (typed polyglot.Value results), and the engine wires both when
  present (pkg/engine/engine.go). Guest-side, `OmniVM.callTyped()` does
  typed outbound calls. What still rides JSON strings: capture injection
  (`omnivm.OmniVM.setCapture(name, jsonString)` + `materializeJsonCapture`)
  and every `__manifest` handle op (`bridgeManifestOp` builds JSON by hand).
  Scalar typed eval is therefore mostly plumbed; the JSON cost is in the
  guest helper (`runtime/java/OmniVM.java`), not the Go side.
- **Java HAS a stream proxy** (`OmniVM.StreamProxy`, OmniVM.java ~3003) with
  the same shape as the JS one: single `{"op":"stream_next","id":N}` pull
  per value inside `iterator().load()`, per-value `materializeStreamChunk`,
  `cancel()` → `stream_cancel`, Cleaner-based finalizers.

Cost to close, ranked by benefit/effort:

1. **Java batched stream pulls — cheap (~50-70 lines, contained).** Add an
   `ArrayDeque<Object> remoteBuffer` + `hostReleased` flag to `StreamProxy`,
   send `max_n:64`, accept the plural `"values"` shape with the singular
   fallback, skip the `stream_cancel` wire call once `done` has been seen
   (the host released the handle). The state machine to respect:
   `released`/`releaseReason` (`markReleased` gates `load()` at the top, so
   buffered-tail delivery must be checked BEFORE the released gate) and
   `cancelAfterLoadFailure`. Validation is already in place: the
   `javac`-based harness in `pkg/manifest/manifest_test.go`
   (TestJavaRemoteStream*) compiles `runtime/java/OmniVM.java` with a mocked
   `nativeCall`, so contract tests mirroring the python/JS ones run without
   a Docker image rebuild. Not done here (Java path was investigation-only
   for this change).
2. **Ruby batched stream pulls — cheap (~60 lines of generated ruby).**
   Same design as python; host side already done.
3. **Java typed capture injection — moderate.** Replace
   `setCapture(JSON)` with typed `setCaptureObject` for scalars/byte[];
   needs jvm_docker C-side work plus OmniVM.java changes, and the win is
   small unless captures are large scalar arrays (tables already cross as
   buffers).
4. **Source-batched guest generators — not worth it / semantics-bound.**
   Batching RuntimeRef-backed streams at the source would change observable
   per-value side-effect timing; the clamp is deliberate (see above). The
   remaining cost for guest-generator streams is the per-value
   Execute/Eval dance in `runtimeRefStreamNext`, which could be reduced
   (fewer round-trips per value) without changing arrival timing — an
   orthogonal optimization.

## Measured hop reductions (200-value stream, max_n=64)

| consumer | before | after | test |
|----------|--------|-------|------|
| host service count (ChanRef source) | 201 | 4 | TestGuestStreamBatchHostServiceCount |
| python guest bridge calls | 201 | 4 | TestPythonGuestStreamBatchedPull |
| JS guest bridge calls | 201 | 4 | TestJSGuestStreamBatchedPull |
| rust consumer object calls | 200 | <20 | TestRustStreamBatchedPull (pre-existing) |
| guest-generator source (any consumer) | 1/value | 1/value (deliberate) | TestGuestGeneratorBatchPullStaysLazy |
