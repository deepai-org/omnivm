# OmniVM Boundary Semantics

This document defines how values cross OmniVM runtime boundaries. It is the
contract for manifest execution, compiler lowering, bridge validation, runtime
inference, and user-visible `.poly` semantics.

The design goal is automatic crossing. Normal `.poly` code should not need
manual `dispose()`, `to_arrow()`, `to_buffer()`, runtime annotations, or
JSON encode/decode glue to move values between Python, JavaScript, Java, Ruby,
and Go. Lowering and runtime adapters choose the crossing mode from the value's
kind and how the target runtime uses it.

## Next-Stage Goal

OmniVM should make CPython-hosted `libomnivm` feel like an ordinary Python
process that can progressively run `.poly` code, while preserving normal object
identity, lifetime, streaming, and bulk-data behavior across every loaded
runtime. A framework handler, request object, model, image-like value, typed
array, dataframe, iterator, file-like value, channel, callback, or native
runtime object should cross the boundary by the same automatic rules as any
other value with the same protocol shape.

This means the long-term contract is protocol-driven, not package-driven:

- no `.poly` source helper API for handles, buffers, Arrow exports, or
  materialization;
- no classifier allowlists for specific libraries, frameworks, or object
  types;
- no implicit JSON encode/decode path for complex objects or bulk data;
- no manual lifetime calls in normal `.poly` code;
- diagnostics and status counters expose the chosen boundary form, but source
  syntax stays ordinary.

The implementation may contain runtime-specific protocol adapters, because each
host has different reflection, buffer, stream, and finalizer APIs. Those
adapters must recognize generic language/runtime protocols rather than named
ecosystem packages.

## Implementation Status

Implemented today:

- CPython-hosted `libomnivm` can run manifests with Python as the parent
  process and load JavaScript, Java, Ruby, and Go after fork.
- Runtime refs preserve identity for complex objects through manifest proxy
  descriptors, scoped handle tables, finalizer queues, and deterministic scope
  cleanup.
- Generic buffer, Arrow, stream, channel, resource, table, job, callback, and
  function-return crossings are visible in manifest/status diagnostics.
- The Arrow/shared store reports live buffers, copied bytes, zero-copy borrows,
  dtype/format metadata, shape, strides, and release counters.
- The handle table records generic access kinds and bounded chatty-proxy
  auto-materialization for repeated item access.
- Request/framework-shaped values and ORM/model-shaped values are tested as
  refs/proxies based on protocol shape, not package-specific allowlists.

Still future or intentionally conservative:

- Arbitrary compiler loop rewriting and deep batching of property/index access
  beyond the currently instrumented repeated-item paths.
- Full multi-buffer, nested, chunked, dictionary, and string-offset Arrow table
  transfer everywhere. Unsupported shapes stay refs or use diagnosed fallback
  paths instead of pretending to be zero-copy.
- Distributed cycle collection across runtimes. Scope cleanup and finalizer
  release bound common cases; retained cycles are observable through diagnostics.
- Returning import-time Python symbols from `.poly` modules as normal Python
  module exports. Side-effect `.poly` imports work; exported symbol semantics
  need a separate contract.

## Boundary Model

Every value crossing a runtime boundary is represented as one of four forms:

- `copy`: an immutable value copied into the target runtime.
- `ref`: an opaque runtime-owned handle with identity, scoped lifetime, and
  finalizer-backed release.
- `stream`: a sequenced value source, such as a channel or iterator, that is
  consumed according to an explicit materialization rule.
- `arrow`: Arrow C Data Interface arrays/schemas for bulk array, tensor, and
  tabular values.

Serialization is not the default boundary model. It is used only when requested
by a manifest bridge operation or when a runtime cannot expose a usable `ref`.

The automatic classifier is:

1. primitives and small immutable scalars use `copy`;
2. Arrow-compatible arrays/tables/images/tensors use `arrow`;
3. streams, bodies, channels, iterators, and generators use `stream`;
4. complex objects with identity, methods, lazy fields, sessions, sockets, or
   framework ownership use `ref`;
5. serialization is the diagnosed fallback, not the default.

## Value Matrix

| Value kind | Default crossing | Ownership | Mutation visibility | Notes |
| --- | --- | --- | --- | --- |
| `null` / `nil` / `undefined` | `copy` | none | none | Lowered to the nearest target-runtime null value. |
| booleans | `copy` | none | none | No identity is preserved. |
| integers | `copy` | none | none | Narrowing must be explicit or validated by bridge rules. |
| floats | `copy` | none | none | Precision loss must be explicit or diagnosed. |
| strings | `copy` | none | none | UTF-8 text; runtimes may store internally however they need. |
| bytes | `arrow` when buffer-like, otherwise `copy` | producer owns until release | read-only by default | Simple bytes are a one-buffer Arrow view when zero-copy is safe. |
| columnar tables | `arrow` | producer owns until release | schema-dependent | Arrow C Data Interface in-process; Arrow IPC only for out-of-process fallback. |
| arrays/lists | `arrow`, `stream`, or `ref` when bulk/foreign; `copy` only for small immutable literals | scope owns handle or target owns copy | depends on mode | Tight loops over foreign arrays should materialize or batch automatically. |
| maps/objects/structs | `ref` when identity/mutation/laziness matters; `copy` only for plain data records | source runtime | yes for refs, no for copies | Framework objects, ORM models, clients, modules, and native objects default to refs. |
| functions/callbacks | `ref` | defining runtime | yes, via calls | Calls marshal arguments/results through this contract. |
| runtime objects/classes/modules | `ref` | source runtime | yes, via methods | Target receives an opaque handle or generated stub. |
| errors/exceptions | `copy` summary plus optional `ref` | source runtime | no | Structured error data should include runtime, origin runtime, type, message, traceback, stack frames, cause chain, boundary path, details. |
| channels | `stream` | OmniVM manifest scope | consumption-dependent | See channel rules below. |
| iterators/generators | `stream` or `ref` | defining runtime | consumption-dependent | Must declare whether crossing drains or proxies. |

## Automatic Boundary Selection

Compiler lowering and runtime adapters must keep boundary mechanics out of
normal `.poly` source. The source language should look like ordinary Python,
JavaScript, Java, Ruby, or Go unless the user deliberately asks for
serialization.

Automatic selection uses evidence from:

- static type information from `.poly` inference;
- runtime adapter type tests such as Python buffer protocol, Arrow exporters,
  Java `Buffer`/Arrow vectors, JS `ArrayBuffer`/TypedArray, and Ruby strings;
- operation shape, for example index-heavy loops, method calls, iteration,
  stream consumption, or property reads;
- generic object protocol evidence such as callability, mutability, stream/body
  interfaces, buffer export support, and stable identity;
- manifest bridge metadata from compiled `.poly` output.

Named packages and frameworks are not classifier inputs. A value crosses through
the same path whenever it exposes the same protocol shape, regardless of which
ecosystem package produced it.

The lowering phase must emit explicit boundary intent into IR/manifest output
even when the user did not write a bridge API. The automatic decision is visible
to tooling and diagnostics, not to the source syntax.

## Runtime Refs

A runtime ref is an opaque handle to a value owned by one runtime.

- The source runtime owns allocation and object identity.
- Other runtimes access the value through generated proxies, bridge calls, or
  manifest operations.
- Runtime refs must not be silently serialized just because the target runtime
  cannot inspect them.
- Ref lifetime is request/scope-bound by default and may be retained only when
  compiler/runtime evidence proves the value escapes.
- Release is automatic through scope cleanup and guest-runtime finalizers.
- Manual release/dispose may exist for internals and debugging, but normal
  `.poly` code must not require it.
- Release must be idempotent and safe after source-runtime shutdown.

The handle table entry must include:

- stable handle id;
- origin runtime;
- source object pointer/reference;
- kind and optional type hint;
- scope id;
- strong reference count;
- weak/finalizer registration state;
- release callback;
- creation site, last access, and generic access counters for diagnostics.

Guest proxies register finalizers:

- JavaScript uses `FinalizationRegistry`;
- Python uses `weakref.finalize`;
- Java uses `Cleaner`;
- Ruby uses finalizers cautiously and should prefer scope cleanup where
  available;
- Go uses runtime finalizers only for adapter-owned proxy wrappers.

Finalizers are best effort. Request/scope cleanup is the deterministic safety
net for web workloads. Guest runtime finalizers enqueue release events; OmniVM
drains those events from safe host-owned contexts so release callbacks do not
run on arbitrary GC finalizer threads.
In the CPython-hosted `libomnivm` path, ordinary host call boundaries drain the
queue automatically when the golden host thread is idle; nested runtime bridge
calls only enqueue.
The finalizer queue has a fixed in-memory spill limit for distinct handle ids;
overflow is counted under handle diagnostics instead of growing without bound.
Stale proxy operations that are initiated by user code, such as get, set, call,
retain, adopt, access, stream next/cancel, and reference creation, must report
the owner-side lifecycle error with runtime/kind context. Cleanup-only paths are
different: `handle_release_finalizer` returns `false` for an already released
handle without queueing work, and `handle_drop_reference` is an idempotent
no-op when either side of the edge is already gone. This keeps GC/finalizer and
scope cleanup races quiet without hiding ordinary stale-proxy use.
Explicit proxy-close helpers route through the user-initiated
`handle_release_explicit` operation instead of the quiet finalizer queue: after
a successful release, Python detaches the
`weakref.finalize` hook, JavaScript unregisters the `FinalizationRegistry`
token, Ruby undefines the `ObjectSpace` finalizer, and Java marks the shared
`Cleaner` state released before running `Cleanable.clean()` so later GC does
not enqueue redundant cleanup for that proxy.

### Cross-Runtime Cycles

Cross-runtime cycles are not visible to any single runtime GC. For example, a
Python object may hold a JS proxy that holds a Python proxy. OmniVM must bound
these cycles with scope ownership and diagnostics.

Policy:

- handles created inside a request/manifest scope are released at scope end
  unless explicitly retained by escape analysis or runtime adapter evidence;
- retained handle graphs are tracked by origin runtime and scope;
- cycle detection may be conservative, but leak diagnostics must report retained
  handles, handles by origin, oldest live handles, and repeated access patterns;
- finalizer release breaks non-cyclic stale proxies opportunistically;
- serialization must not be used to avoid solving identity/lifetime semantics.

## Callable Shape Metadata

Callable shape metadata is the boundary evidence OmniVM uses before turning a
host-language keyword call into another runtime's callable form. It can come
from compiler-emitted manifest params, a runtime-ref descriptor, or a
conservative runtime probe when the value is visible.

The shape fields are intentionally small:

- `acceptsKwargs` means native keyword splatting is known to be valid, as with
  Python `**kwargs` or Ruby keyword parameters.
- `acceptsOptionsObject` means JavaScript keyword calls can append one final
  options object. `destructuredKeys` restricts accepted keywords when known.
- `arity` and `parameterNames` are diagnostic/probing evidence. They document
  the callable shape but do not by themselves authorize keyword adaptation.
- `javaAdapter` names a Java adapter kind, optional method, target type, and
  allowed keys.

Keyword adaptation is automatic only when the target runtime has a proven
shape. Python and Ruby calls use native `**kwargs` dispatch. JavaScript keyword
calls require `acceptsOptionsObject`; unknown keyword names are rejected when
`destructuredKeys` is present. Java keyword calls require `javaAdapter`; OmniVM
must reject keyword calls without it rather than guessing from argument names.

Current Java adapter kinds:

- `map`: append a `Map<String,Object>` argument to the Java method/callable.
- `record`: construct the target record from keyed values, then pass it as the
  Java argument.
- `builder`: construct the target builder, call matching setter-style methods,
  then pass `build()` when present or the builder itself.

Planned Java adapter kinds:

- `namedParameters`: Java parameter-name dispatch when bytecode was compiled
  with reliable `-parameters` metadata and overload resolution is unambiguous.

Runtime probes are conservative. JavaScript probes function arity and attached
or destructured options-object metadata. Java probes functional methods,
records, builders, `Map` parameters, and one-argument adapter methods by
reflection. Python uses `inspect.signature` where available, and Ruby uses
`parameters`/`arity`. Probe failure leaves shape absent, which keeps unsupported
keyword calls explicit.

## Copies

Copied values are detached from the source runtime.

- Mutating a copied array, map, object, or struct in the target runtime does not
  mutate the source value.
- Copy operations must be deterministic and JSON-compatible only when the bridge
  operation says JSON is the representation.
- Unsupported nested values must fail with a boundary error unless a fallback
  bridge is explicitly configured.

## Channels And Streams

Channels are OmniVM-owned manifest resources, not native runtime objects.

- `chan make` creates a manifest-scoped channel.
- `chan send` copies the sent value into the channel unless the value is already
  a runtime ref.
- `chan recv` consumes one item and returns either the item or the runtime's null
  value when the channel is closed and empty.
- Capturing a channel injects a scoped stream descriptor. The target runtime
  pulls values lazily with `stream_next`; strict arrays/lists are materialized
  only when user code asks for them, such as `Array.from(channel)` or
  `list(channel)`.
- Global `wait(...)` returns spawn results, not channel contents.
- Channel draining must be explicit in the lowered IR or manifest operation.

Iterators and generators need an explicit lowered crossing mode:

- `stream`: target pulls values lazily through a bridge.
- `copy`: target drains the iterator into an array/list.
- `ref`: target receives an opaque iterator handle.

`stream_proxy` bridge ops carry an explicit stream marker into captures. The
materialized target value follows the host runtime's normal iteration protocol
and pulls with `stream_next`. This is a proxy contract, not an implicit JSON
array contract.

Stream handles release automatically when `stream_next` reaches end-of-stream.
Read errors from owner stream protocols are terminal for the stream lease: the
owner is closed/released before the error crosses the boundary, and later use
of the same stream handle reports the closed-stream lifecycle diagnostic.
Generated stream proxies also mark themselves closed when a pull raises, detach
their fallback finalizer, and rethrow the original error.
Targets may cancel abandoned streams; collision-safe stream close helpers route
to `stream_cancel` so partial consumption closes the owner deterministically.
Request/manifest scope cleanup and proxy finalizers remain fallback release
paths.

HTTP bodies, request/response streams, file handles, sockets, and
generator-like library objects use this same lazy stream contract. They must
not be materialized unless the target operation requires a strict value and the
size policy allows it.

Runtime adapters recognize generic reader protocols as stream sources: Python
objects with `read`, unsized non-collection `__iter__`, or unsized
non-collection `__aiter__`, Ruby objects with
`read`, `to_io`, or non-collection `each`, JavaScript iterator objects,
non-collection sync iterables, async iterables, or `getReader` streams, Java
`InputStream`, `ReadableByteChannel`, `Reader`, `BaseStream`,
`Flow.Publisher`, or non-collection `Iterable` values, and Go `io.Reader`
values. The bridge pulls bounded chunks with `stream_next` and releases the
stream handle at EOF or owner read error.
Closeable stream sources are closed through their host
protocol on EOF, read error, cancellation, or scope/finalizer release: Python and Ruby
`close`, Java `AutoCloseable`, JavaScript iterator `return`, and Go `io.Closer`.
Go stream proxies expose `Next()` and `ValuesWithError()` when callers need the
terminal owner error; the older `Recv()` and `Values()` helpers remain
EOF-shaped compatibility wrappers.
Java `StreamProxy` marks itself released before rethrowing terminal owner stream
errors, so later `cancel()` or Cleaner cleanup stays idempotent.
Binary chunks continue through the same bulk-data classifier, so byte chunks can
become Arrow/shared-buffer table descriptors without a user-visible helper.

HTTP message-shaped values with public method/path/url/header metadata stay
identity-preserving `ref` values even when they expose body iteration methods.
The request or response object is the complex resource; its body stream can
cross lazily when accessed as a separate body value.

## Opaque Resources And Jobs

Runtime-owned handles such as transactions, request/response objects, database
connections, and job scheduler internals should not cross as JSON copies.

- `resource open` creates a manifest-owned opaque handle with runtime, kind, and
  disposer metadata.
- `resource close` marks that handle closed and is intended for `finallyBody`
  cleanup paths.
- Capturing a resource into another runtime injects a proxy descriptor, not the
  live object.
- `job enqueue` creates a delayed-work handle; `job complete` records its
  eventual result; `job wait` materializes that result into a normal binding.
- `job cancel` runs optional runtime cleanup code, records `cancelled` and
  `cancelReason` descriptor state, and makes later `job wait`/`job complete`
  fail with a cancellation diagnostic.

## Arrow Data Plane

The preferred bulk-data boundary is the Arrow C Data Interface, not a parallel
OmniVM-specific buffer protocol. It carries schema, buffers, offsets, child
arrays, validity bitmaps, and release callbacks without copying column data.
Arrow IPC is the portable fallback for out-of-process runtimes or runtimes that
cannot safely consume C pointers.

Simple byte buffers, typed arrays, image pixels, tensors, and one-dimensional
numeric arrays are represented as degenerate Arrow arrays. Higher-dimensional
values carry shape/stride metadata when the source library exposes it.

The long-term manifest shape should distinguish the logical table from the
transport:

```json
{
  "op": "table",
  "action": "export",
  "runtime": "python",
  "bind": "orders_view",
  "format": "arrow_c_data",
  "source": { "kind": "ref", "name": "orders" },
  "ownership": "borrowed",
  "release": "producer",
  "metadata": {
    "dtype": 4,
    "arrow_format": "g",
    "shape": [1024],
    "strides": [8],
    "null_count": 0,
    "read_only": true
  }
}
```

Implementation requirements:

- `owned` handles transfer release responsibility to OmniVM; `borrowed` handles
  must keep the producer alive until all consumers release the view.
- Mutable buffers require an explicit `mutable: true` contract. The default is
  read-only sharing.
- A table handle must include schema identity and nullability, not infer it from
  target-runtime objects.
- JSON row materialization should remain an explicit user action or fallback
  bridge with diagnostics, never the default table boundary.
- DataFrame libraries should lower to this table handle when they expose Arrow
  memory directly; otherwise they should lower to Arrow IPC or a diagnosed copy.
- The shared Arrow store carries primitive value buffers plus Arrow validity
  bitmaps for nullable flat arrays. Until it carries full child-array and
  multi-buffer table descriptors, dataframe interchange imports may only lower
  single-column, single-chunk numeric data and validity buffers. Wider,
  chunked, string, or offset-backed frames must remain refs or use an explicit
  fallback rather than pretending to be one-buffer Arrow data.
- Dataframe interchange buffers must prove CPU-addressable memory through the
  protocol device hook before OmniVM treats their `ptr` value as host memory.
- Dataframe interchange dtype endianness must match the host byte order or be
  endian-irrelevant; byte-swapping is a diagnosed copy/fallback operation, not a
  zero-copy import.
- Python `__arrow_c_array__` exports and one-chunk `__arrow_c_stream__` exports
  lower flat primitive nullable arrays by preserving the standard Arrow validity
  bitmap. Chunked, nested, dictionary, or multi-buffer stream shapes stay refs or
  fall back until the Python adapter can pass their full descriptors through
  without lying. Invalid elements surface as native null values through table
  proxies.
- Generated Go `c-shared` manifest functions use the same contract for
  primitive numeric slices and arrays. Returns export an owned C data buffer
  with dtype, Arrow format, shape, and release callback metadata, then the host
  imports that memory into the shared Arrow store. Parameters receive borrowed
  table buffers through the same dtype/format descriptor for the duration of the
  call. The rule is based on value shape and element type, not producer package
  names.

Runtime adapters should target generic protocols instead of named-library
branches:

- Python: buffer protocol, `memoryview`, `__arrow_c_array__`,
  `__arrow_c_stream__`, `__dlpack__`, sync/async iterables, and dataframe
  interchange protocols;
- JavaScript: `ArrayBuffer`, TypedArray, `DataView`, sync/async iterables,
  `getReader` streams, and Arrow C Data compatible vectors when exposed;
- Java: `ByteBuffer`, `DirectByteBuffer`, `InputStream`, `ReadableByteChannel`,
  `Reader`, `BaseStream`, `Flow.Publisher`, `AutoCloseable` ownership, and
  Arrow C Data compatible vectors when exposed;
- Go: slices, `io.Reader`/`io.Writer`, `io.Closer`, and Arrow C Data compatible
  values when exposed;
- Ruby: `to_io`, `each`, frozen/binary strings, Fiddle-backed views, and Arrow C
  Data compatible values when exposed.

The manifest executor keeps the same model internally: serialized captures are
classified as `copy`, `ref`, `stream`, `arrow`, or diagnosed `json_fallback`
from bridge metadata and generic handle/table shapes. It does not inspect
producer library names.

## Chatty Proxy Control

Refs preserve identity, but they can hide expensive boundary traffic. The
runtime must detect and mitigate chatty access patterns without requiring new
`.poly` syntax.

Examples:

- repeated foreign index access inside a loop;
- repeated property reads on the same proxy;
- `len(proxy)` followed by `proxy[i]`;
- map/filter/reduce over a foreign collection.

Mitigations:

- automatically batch known property/index reads when the adapter can prove
  stability;
- automatically materialize small immutable collections into typed values;
- switch bulk array access to Arrow when possible;
- stream large iterables lazily instead of indexing them;
- emit diagnostics when the runtime cannot safely optimize;
- expose counters through status/diagnostics: proxy calls, batched calls,
  materializations, Arrow transfers, and JSON fallbacks.

Current manifest/libomnivm diagnostics expose process-level movement counters
under `omnivm.status()["boundary"]`:

- `capture_injections`;
- `runtime_serializations`;
- `json_fallbacks`;
- `arrow_transfers`;
- `bridge_transforms`;
- `boundary_warnings`;
- `proxy_materializations`;
- `proxy_captures`;
- `channel_materializations`;
- `stream_proxy_captures`;
- `resource_proxy_captures`;
- `table_proxy_captures`;
- `job_proxy_captures`.

The central handle table also exposes generic access diagnostics under
`omnivm.status()["handles"]`:

- `handle_accesses`;
- `handle_accesses_by_kind`;
- `finalizer_queued`;
- `finalizer_queue_drains`;
- `finalizer_queue_drops`;
- `finalizer_queue_len`;
- `finalizer_overflow_handles`;
- `strong_refs`;
- `retained_refs`;
- `retained_handles`;
- `retained_by_runtime`;
- `max_strong_refs`;
- `max_strong_ref_handle_id`;
- `chatty_proxy_warnings`;
- `chatty_by_runtime`;
- `chattiest_handle_id`;
- `chattiest_accesses`;
- `chattiest_handle_kind`;
- `reference_edges`;
- `reference_edges_by_kind`;
- `reference_edges_by_runtime`;
- `suspected_cycles`;
- `cyclic_handles`;
- `largest_cycle`;
- `cycle_sample`.

Runtime adapters should report proxy behavior through the internal handle ABI:
retain/escape/release for lifetime, finalizer release enqueueing for GC-owned
threads, access recording for chatty proxy detection, and reference/drop-edge
events for cross-runtime cycle observability. These hooks are adapter plumbing,
not `.poly` language APIs.

Request-scoped host calls that are cancelled before they start executing on the
golden thread must be rejected without running the queued guest-runtime task.
Once guest code has started, it remains on the golden thread until it returns or
a runtime-specific interrupt/timeout hook stops it; cancellation must not move
running runtime work onto a foreign owner loop or cleanup thread.

`omnivm.status()["ruby_threading"]` exposes the embedded Ruby deployment
boundary as structured data. The current mode is `single_vm_thread`; native Ruby
threads are intentionally unsupported in process, and native-threaded app
servers such as Puma should run out of process or be guarded by a startup check.
`omnivm.ruby_threading_status()` returns the same block, and
`omnivm.assert_ruby_native_threads_supported(label)` is the fail-fast form for
integrations that require native Ruby thread scheduling.

For CPython-hosted app servers, `omnivm.drain_worker_hook(*args, **kwargs)` is
the lifecycle-hook form of `omnivm.drain_worker()`. It accepts server callback
arguments, drains initialized workers, and no-ops when a worker exits without
ever loading OmniVM. `omnivm.install_worker_drain_hook()` registers the same
hook with `atexit` as an idempotent process-exit fallback.

`omnivm.affinity_status()` reports the current Python native thread id, the
libomnivm host thread id, whether the call is on the host thread, and any
currently running asyncio loop id. `omnivm.assert_host_thread(label)` raises a
structured `RuntimeError` with boundary path `thread_affinity` when a framework
integration or lifecycle callback is unexpectedly running on a foreign thread.
`omnivm.owner_dispatch_status()` returns
`omnivm.status()["thread_affinity"]`, a machine-readable capability block for
startup checks. In c-shared mode the block currently reports
`mode=diagnostic_only` and `owner_dispatch_supported=false`: OmniVM exposes
host-thread/asyncio diagnostics and pumps async runtimes at host call
boundaries, but it does not export a universal owner-loop, executor, or VM
thread dispatcher. The nested `owner_dispatch_targets` map breaks that down for
`python_asyncio`, `javascript_event_loop`, `java_executor`, and
`ruby_fiber_thread`, with `supported=false` and a diagnostic for each owner
kind. `omnivm.owner_dispatch_target_status(target)` returns one target block,
and `omnivm.assert_owner_dispatch_target_supported(target, label)` is the
target-specific fail-fast guard. `omnivm.assert_owner_dispatch_supported(label)`
remains the fail-fast form for integrations that require universal dispatch.

JavaScript manifest proxies keep natural `.length` semantics for remote data
fields on non-indexed objects and collection length for indexed sequence/table
proxies. When user code needs an unambiguous operation, it can use
`omnivm.proxyGet(proxy, key)`, `omnivm.proxySet(proxy, key, value)`,
`omnivm.proxyCall(proxy, key, args)`, `omnivm.proxyLen(proxy)`,
`omnivm.proxyKeys(proxy)`, `omnivm.proxyValues(proxy)`,
`omnivm.proxyItems(proxy)`, `omnivm.proxyContains(proxy, key)`,
`omnivm.proxyClose(proxy)`, or the collision-free symbol property
`proxy[omnivm.proxyLength]`; data fields remain available through
`omnivm.proxyGet(proxy, "length")`.
Python retained manifest proxies provide matching helpers:
`omnivm.proxy_get(proxy, key)`, `omnivm.proxy_set(proxy, key, value)`,
`omnivm.proxy_call(proxy, key, args=(), kwargs=None)`, and
`omnivm.proxy_len(proxy)`, plus `omnivm.proxy_keys(proxy)`,
`omnivm.proxy_values(proxy)`, `omnivm.proxy_items(proxy)`, and
`omnivm.proxy_contains(proxy, key)`, and `omnivm.proxy_close(proxy)`.
Generated Python manifest capture code also injects `omnivm_close(value)` so
guest Python snippets can explicitly release a handle proxy or cancel a stream
proxy even when the owner object has a real `close` field or method.
Ruby manifest proxies provide `proxy.omnivm_get(key)`,
`proxy.omnivm_set(key, value)`, `proxy.omnivm_call(key, *args)`, and
`proxy.omnivm_len`, plus `proxy.omnivm_keys`, `proxy.omnivm_values`,
`proxy.omnivm_items`, `proxy.omnivm_contains(key)`, and
`proxy.omnivm_close`; generated snippets also provide
`OmniVM.proxy_close(proxy)` and `omnivm_close(proxy)` as collision-safe close
helpers.
The close helpers are idempotent. For ordinary handle proxies they release the
proxy lease; for stream/channel proxies they cancel the lazy stream owner. In
runtimes with explicit finalizer unregistration, close also detaches the
fallback GC cleanup hook after the release or cancellation succeeds.
Java manifest proxies provide the static helpers
`OmniVM.proxyGet(proxy, key)`, `OmniVM.proxySet(proxy, key, value)`,
`OmniVM.proxyCall(proxy, key, args)`, `OmniVM.proxyLen(proxy)`,
`OmniVM.proxyIter(proxy, mode)`, `OmniVM.proxyKeys(proxy)`,
`OmniVM.proxyValues(proxy)`, `OmniVM.proxyItems(proxy)`, and
`OmniVM.proxyContains(proxy, key)`, and `OmniVM.proxyClose(proxy)` for the same
remote get/set/call/length/iteration/membership/proxy-release escape hatches.

The shared Arrow data plane exposes generic bulk-data diagnostics under
`omnivm.status()["arrow"]`:

- `live_buffers`;
- `live_bytes`;
- `buffers_by_dtype`;
- `buffers_by_format`;
- `allocations`;
- `sets`;
- `gets`;
- `releases`;
- `copied_bytes`;
- `zero_copy_borrows`;
- `active_borrows`;
- `active_borrowed_bytes`;
- `active_named_borrows`;
- `named_borrow_queues`;
- `max_named_borrow_queue`;
- `detached_buffers`;
- `detached_bytes`;
- `deferred_release_drops`;
- `largest_buffer_name`;
- `largest_buffer_size`.

Explicit named buffer release tombstones the public name immediately. Active
borrowed views keep the backing memory alive until their own borrow release or
finalizer arrives, and those unnamed leases remain visible through
`active_borrows`, `active_borrowed_bytes`, `detached_buffers`, and
`detached_bytes`.
Go `BorrowedBuffer.Release()` remains a quiet, idempotent finalizer-compatible
lease release; explicit callers that need producer release callback failures can
use `ReleaseWithError()`.
Use `omnivm.buffer_status(name)` for a per-name lifecycle check. It reports
`live` with dtype/format/ownership metadata, `released` after explicit release,
`released_detached` while active borrowed views keep released memory alive, and
`missing` for names that are not known to the store. Released buffer tombstones
retain their dtype/format/read-only/ownership metadata until the bounded
tombstone entry expires or the name is reused. Python
`omnivm.release_buffer(name)` failures include the same status fields when the
loaded library exposes `OmniBufStatus`.
Named borrow queue counters expose runtime buffer views that can only release
by public buffer name. A `max_named_borrow_queue` greater than one means more
than one active view shares that release name, so finalizer-order issues are
observable in diagnostics instead of hidden as a memory leak.

Internal debug helpers such as materialize-to-value or materialize-to-Arrow may
exist, but normal `.poly` code should not need to call them.

## Callbacks

Callbacks cross as refs to callable values.

- The defining runtime owns the callback and its closure.
- Arguments are marshalled using this same boundary matrix.
- Return values are marshalled back to the caller using this same matrix.
- Exceptions propagate as structured boundary errors and must preserve the source
  runtime.
- Callback refs must remain alive at least as long as any generated stub can call
  them.

## Serialization

Serialization is an explicit bridge operation, not an implicit fallback.

Allowed serialization triggers:

- a manifest `bridge` op such as `serialize_json` or `deserialize_json`;
- a user-authored encode/decode call in source code;
- an explicit compiler-lowered fallback marked in diagnostics.

Implicit serialization is forbidden for:

- runtime refs;
- callbacks;
- channels;
- iterators/generators;
- objects with unsupported identity or mutation semantics.

## Boundary Errors

Boundary failures must identify:

- source runtime;
- target runtime;
- value kind;
- attempted bridge operation;
- reason the crossing was rejected;
- suggested explicit bridge operation when one exists.

The manifest runner should prefer typed boundary errors over runtime-specific
string failures.

## Lowering Requirements

Compiler lowering must make boundary intent explicit before manifest emission:

- value copy: `BridgeValue` with copy semantics;
- runtime ref: `BridgeValue` with ref semantics;
- callback: `BridgeValue` with callback semantics;
- channel materialization: channel-specific IR, not hidden capture behavior;
- serialization: explicit serialize/deserialize bridge operation.

Manifest emission should not infer boundary behavior from raw source strings.
