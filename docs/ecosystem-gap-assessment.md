# Ecosystem Gap Assessment

This document assesses likely OmniVM gaps for popular ecosystem libraries. It is
not a claim that every library works today; it separates current evidence from
open test targets.

## Summary

| Area | Current confidence | Covered evidence | Remaining high-value gap |
| --- | --- | --- | --- |
| Lazy data: querysets, streams, iterators, result sets | Medium-high | Django `QuerySet`, SQLAlchemy `Result`/session rollback, psycopg/asyncpg cursors, PyMongo cursor batches/killCursors, Redis `scan_iter`, boto3/botocore S3 paginator windows, Prisma cursor-style `findMany` page windows, JDBC/H2 `ResultSet`, ActiveRecord relation/SQLite adapter, generic Python/JS/Ruby/Java iterators, readers, async iterables, JS ReadableStream, Java `InputStream`/`Reader`/`ReadableByteChannel`/`BaseStream`, channels | Other driver-specific pagination windows, large live DB result backpressure under cancellation |
| Lifecycle-owned objects: requests, responses, sessions, transactions | Medium | Django/FastAPI/Starlette/aiohttp/Flask/Werkzeug/Rack/Rails/Express request and response objects cross as live proxies; Starlette direct-request and ASGI app disconnect checks, Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP client-abort fixtures, Rack/Rails socket-abort response-body owner close, and Express abort lifecycle checks stay live; Django closed-body diagnostics; Django/SQLAlchemy/ActiveRecord rollback after foreign-runtime errors; resource/job manifest ops model transaction-like handles | Real Rack/Rails/Puma app-server abort propagation, worker reload with live request/stream/resource handles, response writers after owner close in more servers |
| Thread/event-loop affinity: Ruby fiber/thread, JS loop, Java executor, Python async loop | Medium-high | Ruby Fiber and Async gem callbacks, Ruby `Thread.new` unsupported diagnostics instead of deadlock, JS timer/promise pumping, JVM thread bridge calls, Python asyncio and TaskGroup, Starlette ASGI app disconnect loop, Uvicorn event-loop re-entry during a streaming response, Express event-loop re-entry during a TCP client abort, Java `CompletableFuture`, `FutureTask`, Reactor scheduler/Disposable, RxJava executor/Disposable, Kotlin coroutine callback affinity as safe or diagnostic; Java cancellation status crosses runtimes for covered future/reactive handles | Node event-loop ownership from undici internals, Ruby app servers that require native Ruby thread scheduling such as Puma, Ruby thread-local/fiber-local framework state under nested callbacks, additional ASGI server variants |
| Native memory: Arrow, buffers, tensors, direct ByteBuffers, GPU memory | Medium | Python buffers, NumPy/Pandas/Polars/dataframe interchange, Arrow PyCapsule/stream, DLPack CPU, JS typed arrays/DataView/ArrayBuffer, Java primitive arrays and direct/read-only/sliced ByteBuffers, non-CPU dataframe interchange stays proxy | Real PyTorch/CuPy/JAX tensors, GPU DLPack/device transfer policy, multi-buffer/nested/chunked Arrow dictionaries and strings |
| Cancellation/teardown: request aborts, worker reloads, timeouts | Medium-high | Watchdog timeout/interrupt stress, stream EOF/cancel release, finalizer/scope release, prefork worker lifecycle fixture, Starlette direct-request and ASGI app disconnect, real Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP client aborts, Rack/Rails socket-abort response-body owner close, Express request abort state, Starlette/aiohttp/httpx/undici/Node Web Stream early cancel, Go c-shared context-owned reader cancel, resource-close cancellation, and manifest job cancellation, Java `CompletableFuture`/`FutureTask` cancellation status, Reactor/RxJava Disposable status | More app-server abort propagation across Rack/Rails/Puma and additional ASGI/Node servers, worker reload while handles are live, scheduler- or library-specific cancellation status attached to handles |
| Method/key collisions: `items`, `keys`, `count`, `then`, `length` | Medium-high | RuntimeRef mapping keys beat methods; Python HTTP message attributes such as `headers` beat raw scope mapping keys; descriptor fields do not shadow runtime object fields; SQLAlchemy rows, ActiveRecord rows/models, Python mappings, Java JDBC/H2 rows, Ruby materialized Java zero-arg methods stay natural, non-callable `then` fields do not become JS thenables, callable `then` requires explicit `omnivm.proxyGet`, indexed proxy `length` writes resize Python/Ruby/Java mutable sequences or fail with runtime/kind diagnostics and no local shadows, Java fixed arrays, ByteBuffer table proxies, and tensor-shaped NumPy table proxies reject JS `length` writes without changing owner state, and `omnivm.proxyGet`/`proxyLen` provide explicit access when names collide | More framework model fields colliding with proxy metadata |
| Error fidelity: stack/type/cause across boundaries | Medium-high | Pydantic, Zod, Django forms, SQLAlchemy, Java cause chains, JavaScript `Error.cause`, Ruby ActiveRecord errors, and Go c-shared wrapped errors preserve runtime/type/message/stack/cause/boundary path in Python-facing errors | Original runtime error handles and language-native catch/rethrow semantics across every guest |

## Assessment By Gap Class

### Lazy Data

OmniVM has good generic stream evidence. The CPython-hosted stress harness
covers Python generators, Python sync/async iterable bodies, JavaScript
iterables/async iterables/ReadableStream, Ruby `each`, Java `Iterable`,
`BaseStream`, `InputStream`, `Reader`, and `ReadableByteChannel`. These tests
assert stream proxy captures, no JSON fallback, recorded stream accesses, and
release on EOF or cancellation.

The database-backed evidence is now concrete for common relational stacks:
Django `QuerySet`, SQLAlchemy `Result`, psycopg and asyncpg cursors, PyMongo
cursor batches with `killCursors`, Redis `scan_iter`, boto3/botocore S3
paginator windows, Prisma cursor-style `findMany` page windows, JDBC/H2
`ResultSet`, and ActiveRecord relation/SQLite adapter cases all stay behind
lazy proxies or streams and preserve rollback/close behavior. The remaining weak
point is cursor families with their own pagination or network backpressure
contracts, such as additional database clients and cloud SDK pagers.

### Lifecycle-Owned Objects

Request and response classification is covered across Django, FastAPI,
Starlette, aiohttp, Rack, Rails ActionDispatch, and Express. Framework objects
cross as live resource proxies, not JSON, stream, or Arrow, unless the body
itself is intentionally exposed as a stream. Starlette disconnect checks,
Express abort lifecycle checks, and Uvicorn/Starlette, aiohttp, Flask/Werkzeug,
and Express localhost server fixtures force TCP client aborts during streaming
responses or request-body reads and preserve live owner state across the
boundary. Rack/Rails request objects are also covered under a socket-level
response abort that closes the Rack body owner. Closed Django request bodies
fail clearly when read from foreign runtimes, and transaction rollback is
covered for Django, SQLAlchemy, and ActiveRecord after foreign-runtime errors.

That is still not the same as full production lifecycle safety. Real app
servers have owner phases: client disconnect, response writer close, transaction
commit/rollback, worker drain, and worker reload. The suite now invokes a real
Starlette ASGI app callable with an `http.disconnect` receive event and runs
real Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP
listener/client-abort fixtures, plus a Rack/Rails socket-abort response-body
owner fixture, but the remaining tests should cover real Rack/Rails/Puma server
processes, worker drain, and reload paths with handle counts and observable
cancellation after abort/reload events.

### Thread And Event-Loop Affinity

The stress suite already probes several hard embedding paths: Ruby Fiber stack
switching, nested JS/Ruby/Python bridge calls, JVM thread callbacks into
Python/JS/Ruby, Python asyncio starvation, JS timer starvation, watchdog
preemption, and timeout recovery.

The remaining risk is framework-owned scheduling. Starlette ASGI app-call
disconnect, Uvicorn event-loop re-entry during streaming response cancellation,
and Express event-loop re-entry during a real client abort are covered, but Node
stream promises inside undici internals, Ruby app servers such as Puma, and Ruby
thread-local or fiber-local framework state can invoke work from threads or
loops OmniVM does not own. Puma also exposed a smaller embedded-Ruby surface gap:
standard `Float(...)` coercion now exists in the Ruby bootstrap alongside
`Integer(...)`, and guest Ruby `Thread.new`/`Thread.start`/`Thread.fork` now
raise an explicit unsupported diagnostic rather than deadlocking the embedded
runtime. Puma itself still depends on native Ruby thread scheduling, so true
in-process Puma support remains open. Java
`CompletableFuture`/`FutureTask` cancellation status and Reactor/RxJava
Disposable status are covered, but scheduler-specific future/reactive semantics
should still be checked where libraries attach cancellation to custom
schedulers. Tests should distinguish direct same-stack calls from callback or
scheduler re-entry and should assert either safe dispatch to the Golden Thread
or a clear diagnostic rejection.

### Native Memory

The current data-plane evidence is broad for CPU data: Python buffer protocol,
array interface, dataframe interchange, Arrow PyCapsule, DLPack CPU, NumPy
views, Pandas/Polars dataframes, JS typed arrays/DataView/ArrayBuffer, Java
primitive arrays and direct/read-only/sliced ByteBuffers. Non-CPU dataframe
interchange is intentionally conservative and stays a resource proxy instead of
claiming Arrow/shared memory.

The hard gap is real tensor libraries. PyTorch, CuPy, and JAX expose CPU, pinned
CPU, GPU, and accelerator buffers with distinct ownership and synchronization
rules. CPU tensors can eventually follow the buffer/DLPack/Arrow path when
layout, strides, dtype, and lifetime are proven. GPU tensors should remain
opaque handles unless an explicit device transfer or device-aware bridge exists.

### Cancellation And Teardown

There is meaningful teardown coverage for stream EOF/cancel, proxy finalizers,
scope cleanup, watchdog timeouts, prefork worker lifecycle, and post-timeout
status. This covers many isolated runtime hazards.

Production cancellation is more demanding. The suite now has framework-object
abort/disconnect coverage for Starlette and Express, a Starlette ASGI app-call
disconnect fixture, real Uvicorn/Starlette, aiohttp, and Express TCP
client-abort streaming fixtures, a Flask/Werkzeug client-abort request-body
fixture with post-abort handle checks, Rack/Rails socket-abort response-body
owner close, Go c-shared stream cancellation reaching a context-owned reader's
`ctx.Done()`, Go c-shared resource close reaching a context-owned handle's
`ctx.Done()`, Go c-shared manifest job cancellation reaching a context-owned
handle's `ctx.Done()`, plus Java `CompletableFuture`
cancellation status, `FutureTask` cancellation status, and Reactor/RxJava
Disposable status crossing the JS boundary. Additional Rack/Rails/Puma app-server
aborts, worker reloads, transaction rollback, broader stream cancellation
status, and scheduler-specific Java reactive cancellation status should all
produce observable cleanup. The next fixture should assert handle counts
before/after an aborted request or worker reload and should expose cancellation
status rather than hiding it in logs.

### Method And Key Collisions

Current tests cover method-colliding mapping keys such as `items`, `keys`,
`count`, `then`, `get`, and `length`, descriptor-field shadowing, unsafe Java
manifest function names, SQLAlchemy and ActiveRecord field collisions, live
setter values, and method arguments staying behind proxies. Ruby materialization
also reads Java zero-arg methods naturally when users access them as fields.
Python HTTP request/response objects get an attribute-first path for natural
fields like `headers`, so Starlette `request.headers.get(...)` does not fall
through to the raw ASGI scope list.

Remaining targets are library objects where collision names carry special host
semantics: additional framework model fields colliding with proxy metadata.
Non-callable `then` data fields are covered so `Promise.resolve(proxy)` resolves
to the proxy instead of treating the field as a thenable. Callable `then` fields are
deliberately hidden from natural `.then` access so promise resolution cannot
accidentally call a foreign-runtime field; users can still call them explicitly
with `omnivm.proxyGet(proxy, "then")`. Indexed proxy `length` writes now either
resize mutable remote Python, Ruby, and Java sequences, or fail with
runtime/kind/id context instead of creating a local-only array length shadow.
Java fixed arrays, ByteBuffer table proxies, and tensor-shaped NumPy table
proxies now reject JS `.length` writes with source-runtime, dtype, shape,
stride, and owner context, and indexed Java array properties route through
remote index access before generic property lookup.
JavaScript keeps remote `.get`
behavior natural when a library object exposes a real method or field named
`get`, and also exposes
`omnivm.proxyGet(proxy, key)` and `omnivm.proxyLen(proxy)` so users can explicitly
choose data-key access or collection length when method names or `.length` would
be ambiguous.

### Error Fidelity

OmniVM has strong crash/stability stress around exception propagation and stack
unwinding, and the Python-facing error wrapper now preserves runtime, type,
message, traceback/stack, cause chain, and manifest boundary path for common
library errors: Django `ValidationError`, Pydantic/Zod validation errors,
SQLAlchemy exceptions, Java causes, JavaScript `Error.cause`, Ruby
ActiveRecord exceptions, and Go c-shared errors that wrap causes with
`errors.Unwrap`.

The target error envelope should preserve:

- source runtime;
- source class/type;
- message;
- stack/traceback;
- cause chain;
- manifest op or boundary path;
- whether an original runtime error handle is still available.

The remaining production gap is native recovery logic: preserving an original
runtime error handle and letting foreign runtimes catch/rethrow with
language-native semantics instead of only receiving a host-side diagnostic
envelope.

## Priority Test Plan

1. Add more real app-server abort/reload fixtures for Rack/Rails/Puma and worker
   drains, with handle-count assertions after client disconnect or worker drain.
   Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP client-abort
   coverage is now in the stress suite; Rack/Rails currently has socket-level
   response-owner abort coverage but still needs real app-server propagation,
   and Puma requires a Ruby-thread scheduling strategy before it can run inside
   the embedded Ruby runtime.
2. Add additional SDK pager/cursor tests for lazy result windows, owner close,
   and backpressure. Prisma cursor-style page windows are now covered.
3. Add scheduler-specific Java reactive cancellation status assertions beyond
   covered `CompletableFuture`/`FutureTask` and Reactor/RxJava Disposable object
   status plus callback-affinity diagnostics.
4. Add PyTorch/CuPy/JAX tests as optional dependency groups: CPU tensors must
   prove dtype/shape/stride/lifetime before Arrow/buffer crossing; GPU tensors
   must stay opaque unless an explicit transfer is requested.

## Policy

- Framework-owned objects cross as opaque handles, not JSON.
- Lazy collections cross as lazy stream proxies with a clear owner runtime.
- DB/session/transaction objects never become plain maps.
- Native memory crosses only when layout, device, and ownership are proven.
- Cancellation is explicit and observable in status counters or error details.
- Errors preserve at least runtime, class/type, message, stack, cause chain, and
  boundary path.
