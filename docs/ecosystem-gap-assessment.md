# Ecosystem Gap Assessment

This document assesses likely OmniVM gaps for popular ecosystem libraries. It is
not a claim that every library works today; it separates current evidence from
open test targets.

## Summary

| Area | Current confidence | Covered evidence | Remaining high-value gap |
| --- | --- | --- | --- |
| Lazy data: querysets, streams, iterators, result sets | Medium-high | Django `QuerySet`, SQLAlchemy sync `Result`/session rollback plus large-result early cancellation, SQLAlchemy async `AsyncResult`/`AsyncSession` rollback over asyncpg, psycopg/asyncpg cursors, PyMongo cursor batches/killCursors, Redis `scan_iter`, boto3/botocore S3 paginator windows, Prisma cursor-style `findMany` page windows, JDBC/H2 `ResultSet`, ActiveRecord relation/SQLite adapter, generic Python/JS/Ruby/Java iterators, readers, async iterables, Python `requests`/`httpx`/`aiohttp` response streams, JS ReadableStream, Java `InputStream`/`Reader`/`ReadableByteChannel`/`BaseStream`, OkHttp response body streams, channels | Other driver-specific pagination windows, large live DB result backpressure under cancellation |
| Lifecycle-owned objects: requests, responses, sessions, transactions | Medium | Django/FastAPI/Starlette/aiohttp/Flask/Werkzeug/Rack/Rails/Express request and response objects cross as live proxies; Django async `StreamingHttpResponse` body cancellation, FastAPI and Starlette ASGI app disconnect checks, Starlette direct-request checks, Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP client-abort fixtures, Rack/Rails socket-abort response-body owner close, Rails ActionDispatch response stream writer post-close diagnostics, Express abort lifecycle checks stay live, and prefork worker reload can drain retained handles before recycle through `omnivm.drain_worker()`; failed retained manifest calls release request-like Python argument refs; Django closed-body diagnostics; Django/SQLAlchemy sync and async/ActiveRecord rollback after foreign-runtime errors; resource/job manifest ops model transaction-like handles | Real Rack/Rails/Puma app-server abort propagation, worker reload with live request/stream/resource handles outside retained manifest modules in more server hooks, response writers after owner close in more servers |
| Thread/event-loop affinity: Ruby fiber/thread, JS loop, Java executor, Python async loop | Medium-high | Ruby Fiber and Async gem callbacks, Ruby `Thread.new` unsupported diagnostics instead of deadlock, Ruby `Thread.current` and fiber-local state through nested JavaScript/Python re-entry, single-threaded Ruby `Queue`/`SizedQueue` compatibility for Rackup/WEBrick initialization, JS timer/promise pumping, JVM thread bridge calls, Python asyncio and TaskGroup, Starlette ASGI app disconnect loop, Uvicorn event-loop re-entry during a streaming response, Express event-loop re-entry during a TCP client abort, Node Web Stream `pipeTo` abort re-entry into Python, undici fetch response-body cancellation, undici request-body cancellation, undici fetch upload abort with a foreign-owned body stream, Java `CompletableFuture` default/custom executor callbacks, `FutureTask`/`ScheduledFuture`, Reactor scheduler/Disposable, RxJava executor/Disposable, scheduled Reactor/RxJava stream cancellation, Kotlin coroutine callback affinity as safe or diagnostic plus `Job` cancellation status; Java cancellation status crosses runtimes for covered future/reactive handles | Deeper Node event-loop ownership from undici internals beyond covered request/response body streams, Ruby app servers that require native Ruby thread scheduling such as Puma, additional ASGI server variants |
| Native memory: Arrow, buffers, tensors, direct ByteBuffers, GPU memory | Medium | Python buffers, NumPy/Pandas/Polars/dataframe interchange, Arrow PyCapsule/stream, DLPack CPU, JS typed arrays/DataView/ArrayBuffer, byte-table stream chunks materialize as JS `Uint8Array` for Web body consumers, Java primitive arrays and direct/read-only/sliced ByteBuffers, non-CPU dataframe interchange stays proxy | Real PyTorch/CuPy/JAX tensors, GPU DLPack/device transfer policy, multi-buffer/nested/chunked Arrow dictionaries and strings |
| Cancellation/teardown: request aborts, worker reloads, timeouts | Medium-high | Watchdog timeout/interrupt stress, stream EOF/cancel release, finalizer/scope release, prefork worker lifecycle, retained-handle worker-reload drain, and failed manifest-call argument cleanup fixtures, Django async `StreamingHttpResponse` body cancel, FastAPI and Starlette ASGI app disconnect, Starlette direct-request disconnect, real Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP client aborts, Rack/Rails socket-abort response-body owner close, Express request abort state, Starlette/requests/aiohttp/httpx/undici/Node Web Stream/Node Web `pipeTo`/undici fetch body early cancel, OkHttp response body early cancel, undici request-body stream cancel, undici fetch upload abort, Go c-shared context-owned reader cancel, resource-close cancellation, and manifest job cancellation, Java `CompletableFuture`/`FutureTask`/`ScheduledFuture` cancellation status, Kotlin `Job` cancellation status, Reactor/RxJava Disposable status, and scheduled Reactor/RxJava stream scheduler teardown | More app-server abort propagation across Rack/Rails/Puma and additional ASGI/Node servers, worker reload while non-manifest handles are live in real server hooks, more library-specific cancellation status attached to handles |
| Method/key collisions: `items`, `keys`, `count`, `then`, `length`, `get`, `close` | Medium-high | RuntimeRef mapping keys beat methods; Python HTTP message attributes such as `headers` beat raw scope mapping keys; descriptor fields do not shadow runtime object fields; SQLAlchemy rows, ActiveRecord rows/models, Python mappings, Java JDBC/H2 rows, Ruby materialized Java zero-arg methods stay natural, non-callable `then` fields do not become JS thenables, callable `then` requires explicit `omnivm.proxyGet`, data fields named `get` and `close` stay user data across Python and JavaScript object proxies, indexed proxy `length` writes resize Python/Ruby/Java mutable sequences or fail with runtime/kind diagnostics and no local shadows, Java fixed arrays, ByteBuffer table proxies, and tensor-shaped NumPy table proxies reject JS `length` writes without changing owner state, and `omnivm.proxyGet`/`proxyLen` provide explicit access when names collide | More framework model fields colliding with proxy metadata |
| Error fidelity: stack/type/cause across boundaries | High | Pydantic, Zod, Django forms, SQLAlchemy, Java cause chains, JavaScript `Error.cause`, Ruby ActiveRecord errors, and Go c-shared wrapped errors preserve runtime/type/message/stack/cause/boundary path in Python-facing errors; direct `call[...]` and typed `call_typed[...]` failures include API boundary labels; embedded Python guest callers and `omnivm python` interpreter-mode callers catch `omnivm.RuntimeError` with runtime/type/message/traceback/cause-chain/boundary-path fields for cross-runtime `omnivm.call` and `omnivm.call_typed` failures; native JavaScript `catch` receives normal `Error` objects with the same OmniVM fields, including Python tracebacks; native Ruby `rescue` receives `OmniVM::RuntimeError` with matching fields, `to_h`/`to_dict` envelopes, Java cause-chain coverage, and Python traceback coverage; native Java callers catch `OmniVM.RuntimeError` with matching getters and `toMap()` envelopes for JavaScript cause chains and Python failures; Python, JavaScript, Ruby, and Java native catch/rethrow paths preserve the source runtime/type/message instead of relabeling the error as the rethrowing runtime; original-error-handle markers cross native JavaScript, Python, Ruby, and Java catch envelopes; Go API callers receive `*omnivm.RuntimeError` with runtime/type/message/traceback/cause-chain/boundary-path/original-handle fields | General original runtime error handle protocol across every guest |

## Assessment By Gap Class

### Lazy Data

OmniVM has good generic stream evidence. The CPython-hosted stress harness
covers Python generators, Python sync/async iterable bodies, JavaScript
iterables/async iterables/ReadableStream, Ruby `each`, Java `Iterable`,
`BaseStream`, `InputStream`, `Reader`, and `ReadableByteChannel`, plus OkHttp
response body streams. These tests assert stream proxy captures, no JSON
fallback, recorded stream accesses, and release on EOF or cancellation.

The database-backed evidence is now concrete for common relational stacks:
Django `QuerySet`, SQLAlchemy sync `Result` including a large result that is
cancelled after a single row with owner-side result/connection close,
SQLAlchemy async `AsyncResult`/`AsyncSession` over asyncpg, psycopg and asyncpg cursors, PyMongo
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
boundary. FastAPI and Starlette ASGI app-call fixtures now observe
`http.disconnect` and keep the captured request live across the boundary.
Rack/Rails request objects are also covered under a socket-level response abort
that closes the Rack body owner. Ruby `Queue`/`SizedQueue` now has enough
single-threaded compatibility for Rackup/WEBrick server initialization, so
WEBrick no longer fails before reaching app-server ownership checks. Rails
`ActionDispatch::Response#stream` writer objects now stay live as resource
proxies, preserve `write`/`close`, and report the source `closed stream` error
when a foreign runtime writes after owner close.
Closed Django request bodies fail clearly when read from foreign runtimes, and
transaction rollback is covered for Django, SQLAlchemy sync and async sessions,
and ActiveRecord after foreign-runtime errors.

That is still not the same as full production lifecycle safety. Real app
servers have owner phases: client disconnect, response writer close, transaction
commit/rollback, worker drain, and worker reload. The suite now invokes a real
FastAPI and Starlette ASGI app callables with an `http.disconnect` receive event
and runs real Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP
listener/client-abort fixtures, plus a Rack/Rails socket-abort response-body
owner fixture, Rackup/WEBrick initialization fixture, and Rails response-writer
post-close fixture. Django async
`StreamingHttpResponse.streaming_content` also now closes wrapper-owned async
body iterators on foreign-runtime cancellation.
Prefork worker reload has an explicit `omnivm.drain_worker()` hook, with
handle-count assertions proving leaked retained proxies are released before
worker exit. `omnivm.unload_manifest_modules()` remains as a compatibility
alias for code that used the original manifest-specific name. The remaining
tests should cover real Rack/Rails/Puma server processes and additional server
hook reload paths with observable cancellation after abort/reload events.

### Thread And Event-Loop Affinity

The stress suite already probes several hard embedding paths: Ruby Fiber stack
switching, nested JS/Ruby/Python bridge calls, JVM thread callbacks into
Python/JS/Ruby, Python asyncio starvation, JS timer starvation, watchdog
preemption, and timeout recovery.

The remaining risk is framework-owned scheduling. Starlette ASGI app-call
disconnect, Uvicorn event-loop re-entry during streaming response cancellation,
Express event-loop re-entry during a real client abort, Node Web Stream
`pipeTo` abort with underlying-source re-entry into Python, undici fetch
response-body cancellation through a real local HTTP server, undici
request-body cancellation of a foreign-owned iterable, and undici `fetch()`
upload abort against a real local HTTP server are covered. Deeper undici
internals beyond these request/response abort paths and Ruby app servers such
as Puma can still invoke work from threads or loops OmniVM does not own. Ruby
`Thread.current` and fiber-local framework-style state now survives nested
JavaScript/Python re-entry on the VM-owned Ruby thread. Puma also exposed
a smaller embedded-Ruby
surface gap:
standard `Float(...)` coercion now exists in the Ruby bootstrap alongside
`Integer(...)`, and guest Ruby `Thread.new`/`Thread.start`/`Thread.fork` now
raise an explicit unsupported diagnostic rather than deadlocking the embedded
runtime. Rackup/WEBrick server objects now initialize because Ruby `Queue` and
`SizedQueue` expose single-threaded FIFO behavior, but Puma itself still depends
on native Ruby thread scheduling, so true in-process Puma support remains open.
Java `CompletableFuture` callback affinity
is covered for both default async dispatch and explicit custom executors, and
`CompletableFuture`/`FutureTask`/`ScheduledFuture` cancellation status plus
Reactor/RxJava Disposable status are covered. Scheduler-specific
future/reactive semantics should still be expanded where libraries attach
cancellation to custom schedulers. Tests should distinguish direct same-stack calls from callback or
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
abort/disconnect coverage for FastAPI, Starlette, Django async streaming
responses, and Express, FastAPI and Starlette ASGI app-call disconnect fixtures,
real Uvicorn/Starlette, aiohttp, and Express TCP client-abort streaming
fixtures, a Flask/Werkzeug client-abort request-body fixture with post-abort
handle checks, Rack/Rails socket-abort response-body owner close, undici fetch
response-body cancellation against a real local HTTP server, Python `requests`
response `iter_lines()` early cancellation with `response.close()`, OkHttp
response-body `byteStream()` early cancellation closing the owner-side source,
retained handle drain during prefork worker recycle, Go c-shared stream
cancellation reaching a context-owned reader's `ctx.Done()`, Go c-shared
resource close reaching a context-owned handle's `ctx.Done()`, Go c-shared
manifest job cancellation reaching a context-owned handle's `ctx.Done()`, plus
Java `CompletableFuture`
cancellation status, `FutureTask`/`ScheduledFuture` cancellation status, and Reactor/RxJava
Disposable status crossing the JS boundary. Scheduled Reactor/RxJava stream
cancellation now also proves owner-side scheduler/executor teardown after an
early JS cancel. Undici request-body cancellation now also exercises a
foreign-owned Python iterable through Node's Web `Request` body conversion, Node
Web Stream `pipeTo` abort reaches owner-side Python cancellation from the
underlying source, and undici `fetch()` upload abort exercises the real client
stack against a local HTTP server, so async-iterator cancellation releases the
owner. Additional Rack/Rails/Puma app-server aborts, worker reloads,
transaction rollback, and broader library-specific stream cancellation status
should all produce observable cleanup. The next fixture should assert handle counts
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
message, traceback/stack, cause chain, direct `call[...]` API boundary, manifest
boundary path, and a JSON-serializable `RuntimeError.to_dict()` envelope for
common library errors: Django `ValidationError`,
Pydantic/Zod validation errors, SQLAlchemy exceptions, Java causes, JavaScript
`Error.cause`, Ruby ActiveRecord exceptions, and Go c-shared errors that wrap
causes with `errors.Unwrap`. Native JavaScript callers can also catch the normal
`Error` thrown by `omnivm.call(...)` and read `runtime`, `type`, `traceback`,
`causeChain`, `boundaryPath`, and `originalErrorHandle` fields without parsing
the message string. Embedded Python guest callers now catch `omnivm.RuntimeError`
as a normal `RuntimeError` and read the equivalent `runtime`, `type`, `message`,
`traceback`, `cause_chain`, `boundary_path`, `original_error_handle`, and
`to_dict()` envelope fields when JavaScript or Java failures cross through
`omnivm.call(...)`; typed bridge failures through `omnivm.call_typed(...)` use
the same class and the `call_typed[...]` boundary path. The same `call_typed`
API is exposed from `omnivm python` interpreter mode, so Python app entrypoints
see the same typed success path and structured typed failure envelope. Native
Ruby callers can rescue `OmniVM::RuntimeError` as a
normal `RuntimeError` and read equivalent `runtime`, `type`, `traceback`,
`cause_chain`, `boundary_path`, `original_error_handle`, and `to_h`/`to_dict`
envelope fields when the source runtime reports those details. Java cause
chains and Python traceback errors are covered for native Ruby rescue paths.
Native Java callers can catch
`OmniVM.RuntimeError` as a normal `RuntimeException` and read equivalent
`getRuntime()`, `getType()`, `getTraceback()`, `getCauseChain()`,
`getBoundaryPath()`, `getOriginalErrorHandle()`, and `toMap()` envelope fields.
Python, JavaScript, Ruby, and Java rethrow paths now serialize the structured
OmniVM error envelope again when a guest catches and rethrows it, so the next
boundary preserves the source runtime, type, message, traceback, and causes
instead of reducing the error to the rethrowing runtime's local exception class.
Go API callers can use `errors.As` to recover `*omnivm.RuntimeError` with the
same runtime, type, message, traceback, cause-chain, boundary-path, and
original-error-handle fields, including runtime-prefixed errors that do not use
the transport `ERR:` marker.

The target error envelope should preserve:

- source runtime;
- source class/type;
- message;
- stack/traceback;
- cause chain;
- manifest op or boundary path;
- whether an original runtime error handle is still available.

The remaining production gap is preserving an original runtime error handle when
the guest can safely expose it. Python, JavaScript, Ruby, and Java now parse and
propagate an optional original-error-handle marker when a runtime reports one,
but there is not yet a general guest-native error handle protocol.

## Priority Test Plan

1. Add more real app-server abort/reload fixtures for Rack/Rails/Puma and worker
   drains, with handle-count assertions after client disconnect or worker drain.
   Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express TCP client-abort
   coverage is now in the stress suite; Rack/Rails currently has socket-level
   response-owner abort coverage, Rackup/WEBrick initialization coverage, and
   retained handles now have a natural `omnivm.drain_worker()` hook for prefork
   worker reloads, but real app-server propagation remains open. Puma requires a
   Ruby-thread scheduling strategy before it can run inside the embedded Ruby
   runtime.
2. Add additional SDK pager/cursor tests for lazy result windows, owner close,
   and backpressure. Prisma cursor-style page windows are now covered.
3. Add more library-specific reactive cancellation status assertions beyond
   covered `CompletableFuture`/`FutureTask`/`ScheduledFuture`, Kotlin `Job`, Reactor/RxJava
   Disposable object status, scheduled Reactor/RxJava stream teardown, and
   callback-affinity diagnostics.
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
