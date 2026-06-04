# Ecosystem Gap Assessment

This document assesses likely OmniVM gaps for popular ecosystem libraries. It is
not a claim that every library works today; it separates current evidence from
open test targets.

## Summary

| Area | Current confidence | Covered evidence | Remaining high-value gap |
| --- | --- | --- | --- |
| Lazy data: querysets, streams, iterators, result sets | Medium-high | Django `QuerySet` plus chunked `QuerySet.iterator()` cancellation, SQLAlchemy sync `Result`/session rollback plus large-result early cancellation, owner-closed SQLAlchemy `Result` stream errors crossing JS/Ruby/Java, ORM `yield_per`/`scalars()` early cancellation, lazy ORM relationship collections, and `Result.partitions()` batch-window cancellation, SQLAlchemy async `AsyncResult` cancelled from JS/Ruby/Java plus `AsyncSession` rollback over asyncpg, psycopg cursors, asyncpg cursors cancelled from JS/Ruby/Java, asyncpg connection lifecycle proxying, psycopg server-side cursor early cancellation from JS/Ruby/Java, PyMongo cursor batches/killCursors, Redis `scan_iter`, boto3/botocore S3, DynamoDB, and CloudWatch Logs paginator windows, Google API Core pagers, Prisma cursor-style `findMany` page windows plus transaction rollback after a foreign-runtime error, JDBC/H2 `ResultSet` lifecycle/rollback plus early close without draining owner rows, ActiveRecord relation/SQLite adapter plus PostgreSQL adapter relation cancellation/rollback, generic Python/JS/Ruby/Java iterators, readers, async iterables, Python `requests`/`httpx`/`aiohttp` response streams, JS ReadableStream, Node classic `Readable` plus `toNodeReadable()` adapter, Java `InputStream`/`Reader`/`ReadableByteChannel`/`BaseStream`/`Flow.Publisher`, OkHttp response body streams, channels | Other driver-specific pagination windows, large live DB result backpressure under cancellation |
| Lifecycle-owned objects: requests, responses, sessions, transactions | Medium | Django/FastAPI/Starlette/aiohttp/Flask/Werkzeug/Rack/Rails/Express/Fastify request and response objects cross as live proxies; Django `SessionMiddleware` signed-cookie sessions and Flask `LocalProxy` sessions stay live and mutate naturally across runtimes; stale Python, JavaScript, Ruby, and Java resource proxies report owner-side manifest lifecycle errors after resource close instead of falling back to local proxy defaults, and released table/stream proxies report owner-side released/closed lifecycle errors instead of unknown handles; Django async `StreamingHttpResponse` body cancellation, real Django ASGI/Uvicorn client-abort cancellation, FastAPI ASGI disconnect and `StreamingResponse` body cancellation, Starlette direct-request and ASGI disconnect checks, Uvicorn/Starlette, aiohttp, Flask/Werkzeug, Express, and Fastify TCP client-abort fixtures, Express/Multer multipart upload abort cancellation, Express/body-parser JSON request abort cancellation, Koa/bodyparser JSON request abort cancellation, Fastify request/reply lifecycle after a real TCP request plus late `reply.raw.write()` rejection after end, Koa raw response write-after-end rejection after a real TCP request, Rack/Rails socket-abort response-body owner close, Rackup/WEBrick parser/service client-abort fixture with a live Rack request proxy and callable response writer cleanup, Rails ActionDispatch response stream writer post-close diagnostics, aiohttp `StreamResponse.write()` after `write_eof()` reports the owner-side error through a JS caller, Werkzeug WSGI `start_response` writers report the owner-side closed-socket `OSError` through a JS caller, Express abort lifecycle checks stay live, Express and Node core `http.ServerResponse` writes after `end()` report owner-side `ERR_STREAM_WRITE_AFTER_END`, raw asyncpg connections stay live across JS/Java calls and reflect owner-side close state, and prefork worker reload can drain live resource and stream handles, including retained and escaped process handles, before recycle through `omnivm.drain_worker()` or the app-server-compatible `omnivm.drain_worker_hook(*args, **kwargs)`; failed retained manifest calls release request-like Python argument refs; Django closed-body diagnostics; Django/SQLAlchemy sync and async/ActiveRecord SQLite and PostgreSQL/Prisma rollback after foreign-runtime errors; resource/job manifest ops model transaction-like handles | True Puma/native-threaded Ruby app-server abort propagation, additional server-specific worker reload hook coverage, response writers after owner close in more servers |
| Thread/event-loop affinity: Ruby fiber/thread, JS loop, Java executor, Python async loop | Medium-high | Ruby Fiber and Async gem callbacks, Ruby Async task cancellation/status across JavaScript/Python/Java/Ruby, Ruby `Thread.new` unsupported diagnostics instead of deadlock, Puma server startup reaches that explicit diagnostic rather than hanging, `omnivm.status()["ruby_threading"]` exposes the single-VM-thread/Puma-out-of-process boundary for host startup checks, Ruby `Thread.current` and fiber-local state through nested JavaScript/Python re-entry, single-threaded Ruby `Queue`/`SizedQueue` compatibility for Rackup/WEBrick initialization, Rackup/WEBrick parser/service handling through a single accepted socket with timeout threads disabled, JS timer/promise pumping, Python `affinity_status()` and `assert_host_thread()` expose host-thread/current-thread/running-asyncio-loop diagnostics with structured `thread_affinity` violations for callback/lifecycle guards, JVM thread bridge calls, Python raw threads, `ThreadPoolExecutor` future callbacks, asyncio and TaskGroup, AnyIO task-group cancellation, Python awaitable method results returned through JS proxy calls are pumped on the owner loop, Starlette ASGI app disconnect loop, Uvicorn event-loop re-entry during a streaming response, Express event-loop re-entry during a TCP client abort, Express/Multer multipart upload abort through undici and a Python-owned body, Express/body-parser JSON abort through undici and a Python-owned body, Koa/bodyparser JSON abort through undici and a Python-owned body, Node Web Stream `pipeTo` abort re-entry into Python, Node classic `stream.pipeline()` abort cancellation of a Python-owned source, Busboy multipart upload abort cancellation of a Python-owned request body, undici fetch response-body cancellation, undici request-body cancellation, undici fetch upload abort with a foreign-owned body stream, lower-level undici `request()`, `Client#request()`, `Pool#request()`, and `Agent#request()` upload aborts with foreign-owned body streams, custom undici `Dispatcher` upload-body error handling with a foreign-owned body stream, Java `CompletableFuture` default/custom executor callbacks, `ExecutorService` future cancellation/interrupt teardown, `FutureTask`/`ScheduledFuture`, Guava `ListenableFuture`, Java `Flow.Publisher` demand/cancel, Reactor scheduler/Disposable, RxJava executor/Disposable, scheduled Reactor/RxJava stream cancellation, Kotlin coroutine callback affinity as safe or diagnostic plus `Job` cancellation status; Java cancellation status crosses runtimes for covered future/reactive handles, including Reactor/RxJava `Disposable` and `FutureTask` status from JavaScript, Python, and Ruby callers | True in-process support for Ruby app servers that require native Ruby thread scheduling such as Puma, additional ASGI/Node server variants, universal owner-executor/event-loop callback dispatch |
| Native memory: Arrow, buffers, tensors, direct ByteBuffers, GPU memory | Medium | Python buffers, NumPy/Pandas/Polars/dataframe interchange, Arrow PyCapsule/stream, DLPack CPU including real JAX CPU tensors, JS typed arrays/DataView/ArrayBuffer including byte-oriented JS buffer args materialized as Python `bytes` for Python writer APIs, byte-table stream chunks materialize as JS `Uint8Array` for Web body consumers, Java primitive arrays and direct/read-only/sliced ByteBuffers, non-CPU dataframe interchange and non-CPU DLPack stay proxy, and nested/multi-chunk/dictionary/string-offset Arrow shapes stay proxy instead of pretending one-buffer Arrow | Real PyTorch/CuPy tensors, JAX accelerator tensors, explicit GPU DLPack/device transfer policy, broader multi-buffer Arrow arrays and full dictionary/string materialization |
| Cancellation/teardown: request aborts, worker reloads, timeouts | Medium-high | Watchdog timeout/interrupt stress, stream EOF/cancel release, finalizer/scope release, prefork worker lifecycle, live resource/stream handle worker-reload drain through direct and app-server-compatible Python hook paths, and failed manifest-call argument cleanup fixtures, Django async `StreamingHttpResponse` body cancel, real Django ASGI/Uvicorn async streaming abort cleanup, FastAPI and Starlette ASGI app disconnect, Starlette direct-request disconnect, real Uvicorn/Starlette, aiohttp, Flask/Werkzeug, Express, and Fastify TCP client aborts, Rack/Rails socket-abort response-body owner close, Rackup/WEBrick callable response writer cleanup after client abort, Express request abort state, Starlette/requests/aiohttp/httpx/undici/Node Web Stream/Node classic `Readable`/Node classic `stream.pipeline()`/Busboy multipart upload abort/Express Multer upload abort/Express body-parser JSON abort/Koa bodyparser JSON abort/Node Web `pipeTo`/undici fetch body early cancel, OkHttp response body early cancel, Java `InputStream`/`Reader`/`ReadableByteChannel`/`Flow.Publisher` early cancel, undici request-body stream cancel, undici fetch upload abort, undici `request()`/`Client#request()`/`Pool#request()`/`Agent#request()` upload aborts, custom undici `Dispatcher` upload-body errors, Go c-shared `io.Reader` cancellation from JS/Ruby/Java, Go c-shared context-owned reader cancel, resource-close cancellation, and manifest job cancellation, Java `CompletableFuture`/`ExecutorService` Future/`FutureTask`/`ScheduledFuture`/Guava `ListenableFuture` cancellation status, Kotlin `Job` cancellation status, Reactor/RxJava Disposable status from JavaScript, Python, and Ruby callers, and scheduled Reactor/RxJava stream scheduler teardown | More app-server abort propagation across Puma/native-threaded Ruby servers and additional ASGI/Node servers, additional server-specific worker reload hook coverage, more library-specific cancellation status attached to handles |
| Method/key collisions: `items`, `keys`, `count`, `then`, `length`, `get`, `close` | Medium-high | RuntimeRef mapping keys beat methods; Python key-addressable session-like objects that are not registered `Mapping`s, including Django `SessionStore`, prefer contained keys before methods; lazy Python objects with unsafe `__contains__` methods do not have membership probes called while resolving method-colliding attributes; Python HTTP message attributes such as `headers` beat raw scope mapping keys; descriptor fields do not shadow runtime object fields; Pydantic `BaseModel` fields, Protobuf message fields, Zod parsed object fields, attrs/dataclass model fields, Django model fields, SQLAlchemy ORM mapped fields and Core `Row` fields, ActiveRecord rows/models, Python mappings, Java JDBC/H2 rows, Java value-like zero-arg methods stay natural in JavaScript/Python/Ruby guest proxies, non-callable `then` fields do not become JS thenables, callable `then` requires explicit `omnivm.proxyGet`, data fields named `get` and `close` stay user data across Python and JavaScript object proxies, owner fields named `keys`/`length` beat inherited JS array helper/length surfaces on sequence-like resource proxies when the owner proves those fields exist, indexed proxy `length` writes resize Python/Ruby/Java mutable sequences or fail with runtime/kind diagnostics and no local shadows, Java fixed arrays, ByteBuffer table proxies, and tensor-shaped NumPy table proxies reject JS `length` writes without changing owner state, JavaScript `omnivm.proxyGet`/`proxySet`/`proxyCall`/`proxyLen`/`proxyKeys`/`proxyItems`/`proxyContains`/`proxyClose` plus `proxy[omnivm.proxyLength]`, Python `omnivm.proxy_get`/`proxy_set`/`proxy_call`/`proxy_len`/`proxy_keys`/`proxy_items`/`proxy_contains`/`proxy_close`, Ruby `omnivm_get`/`omnivm_set`/`omnivm_call`/`omnivm_len`/`omnivm_keys`/`omnivm_items`/`omnivm_contains`/`omnivm_close`, and Java `OmniVM.proxyGet`/`proxySet`/`proxyCall`/`proxyLen`/`proxyKeys`/`proxyItems`/`proxyContains`/`proxyClose` provide explicit access when names collide | More less-common framework model fields colliding with proxy metadata |
| Error fidelity: stack/type/cause across boundaries | High | Pydantic, Marshmallow, jsonschema, Zod, Django forms, SQLAlchemy, Java cause chains, JavaScript `Error.cause`, Ruby ActiveRecord errors, and Go c-shared wrapped errors preserve runtime/type/message/stack/cause/boundary path in Python-facing errors; Pydantic validation `errors()`, Marshmallow normalized messages, jsonschema validator/path attributes, Django form JSON error fields, SQLAlchemy DBAPI statement/params/original error fields, Python exception `details` plus `to_dict()`/`as_dict()`/JSON envelope methods, Ruby exception `details`/`errors`/`to_h`/`to_hash` methods plus ActiveModel/ActiveRecord `record`/`model.errors`, ordinary Java throwable `getDetails()`/`details()`/`getErrors()`/`errors()`/path/location/original-message accessors or fields, and Zod `issues` now cross as structured `details` payloads instead of message-only text; JavaScript native `catch` preserves SQLAlchemy `IntegrityError`, Django form `ValidationError`, and Pydantic/generic Python structured details from Python calls, ActiveRecord error fields and ActiveRecord validation details from Ruby calls, plus ordinary Java throwable details from Java calls; direct `call[...]` and typed `call_typed[...]` failures include API boundary labels; embedded Python guest callers and `omnivm python` interpreter-mode callers catch `omnivm.RuntimeError` with runtime/type/message/traceback/cause-chain/boundary-path/details fields for cross-runtime `omnivm.call` and `omnivm.call_typed` failures; native JavaScript `catch` receives normal `Error` objects with the same OmniVM fields, including SQLAlchemy, Django form, Pydantic details, ActiveRecord, Ruby ActiveRecord validation details, Java throwable details, and Python traceback details; native Ruby `rescue` receives `OmniVM::RuntimeError` with matching fields, `to_h`/`to_dict` envelopes, Java cause-chain coverage, Python traceback coverage, Pydantic/Marshmallow/jsonschema/Zod validation details, SQLAlchemy `IntegrityError` SQL details, and Django form error details; native Java callers catch `OmniVM.RuntimeError` with matching getters and `toMap()` envelopes for JavaScript cause chains, Python failures, Pydantic/Marshmallow/jsonschema/Zod validation details, SQLAlchemy `IntegrityError` SQL details, Django form error details, and ActiveRecord error details; Python, JavaScript, Ruby, and Java native catch/rethrow paths preserve the source runtime/type/message instead of relabeling the error as the rethrowing runtime; original-error-handle markers cross native JavaScript, Python, Ruby, and Java catch envelopes; plain JavaScript `Error.originalErrorHandle`, ordinary Python exception `original_error_handle`, ordinary Ruby exception `@original_error_handle`, and ordinary Java throwable original-handle fields/methods cross into native catch envelopes; Go API callers receive `*omnivm.RuntimeError` with runtime/type/message/traceback/cause-chain/boundary-path/original-handle/details fields | More library-specific structured payload extractors beyond the covered validation, DBAPI/ActiveModel, and generic Java accessor/field shapes |

## Assessment By Gap Class

### Lazy Data

OmniVM has good generic stream evidence. The CPython-hosted stress harness
covers Python generators, Python sync/async iterable bodies, JavaScript
iterables/async iterables/ReadableStream, Node classic `Readable` and the
JS stream proxy `toNodeReadable()` adapter, Ruby `each`, Java `Iterable`,
`BaseStream`, `Flow.Publisher`, `InputStream`, `Reader`, and
`ReadableByteChannel`, plus OkHttp
response body streams. These tests assert stream proxy captures, no JSON
fallback, recorded stream accesses, and release on EOF or cancellation.

The database-backed evidence is now concrete for common relational stacks:
Django `QuerySet`, chunked `QuerySet.iterator()` streams that cancel after a
single row without draining the owner iterator, SQLAlchemy sync `Result`
including a large result that is cancelled after a single row with owner-side
result/connection close, owner-closed SQLAlchemy `Result` stream errors that
cross into JS/Ruby/Java instead of becoming silent EOF, ORM
`yield_per`/`scalars()` streams that return mapped objects as live proxies and
close the `ScalarResult`/session identity map on early cancellation, lazy ORM
relationship collections that stay unloaded until foreign access and then cross
as live sequence proxies with mapped child objects, and `Result.partitions()`
batch windows cancelled after one partition,
SQLAlchemy async `AsyncResult` cancellation from JS/Ruby/Java plus
`AsyncSession` rollback over asyncpg, psycopg cursors,
asyncpg cursors cancelled from JS/Ruby/Java, raw asyncpg connections that stay
live as resource proxies and reflect owner-side close state, psycopg server-side cursors that
close and roll back after early JS/Ruby/Java cancellation, PyMongo cursor
batches with `killCursors`, Redis `scan_iter`, boto3/botocore S3, DynamoDB, and
CloudWatch Logs paginator windows, Google API Core page iterators, Prisma
cursor-style `findMany` page windows plus transaction rollback after a
foreign-runtime error,
JDBC/H2 `ResultSet` lifecycle/rollback plus an early-close fixture that reads
one row and verifies owner-side `ResultSet`/`Statement`/`Connection` close
without draining the remaining rows, and ActiveRecord relation/SQLite/PostgreSQL adapter cases all stay
behind lazy proxies or streams and preserve rollback/close behavior. The remaining weak
point is cursor families with their own pagination or network backpressure
contracts, such as additional database clients and cloud SDK pagers.

### Lifecycle-Owned Objects

Request and response classification is covered across Django, FastAPI,
Starlette, aiohttp, Rack, Rails ActionDispatch, Express, and Fastify. Framework objects
cross as live resource proxies, not JSON, stream, or Arrow, unless the body
itself is intentionally exposed as a stream. Starlette disconnect checks,
Express abort lifecycle checks, and Uvicorn/Starlette, aiohttp, Flask/Werkzeug,
Express, and Fastify localhost server fixtures force TCP client aborts during streaming
responses or request-body reads and preserve live owner state across the
boundary. Django `SessionMiddleware` signed-cookie sessions and Flask
`LocalProxy` sessions stay as live framework-owned proxies across foreign
runtime reads and writes, including session keys named like proxy methods such
as `items`, `keys`, `get`, and `close`. FastAPI and Starlette ASGI app-call fixtures now observe
`http.disconnect` and keep the captured request live across the boundary.
Stale Python, JavaScript, Ruby, and Java resource proxies now distinguish
ordinary missing remote fields from owner lifecycle failures: after a manifest
resource close, natural property access reports a source `closed resource
handle` lifecycle diagnostic with runtime/kind context instead of falling back
to local descriptor data, `undefined`, or runtime-local missing-field defaults.
Released table and closed stream proxies now use the same tombstone path, so
post-release table index/length and post-cancel stream reads report
owner-side lifecycle errors instead of generic unknown-handle failures.
Fastify now runs a real listener path and keeps request/reply wrappers live
after a Python-driven HTTP request, including owner-side reply status/header
inspection and clean server shutdown. It also rejects Python writes through
`reply.raw` after the reply has ended without mutating the owner response body,
and validates any owner-side late-write error if the Node stream emits one. A
Fastify hijacked-reply fixture now also
forces a TCP reset after the first chunk and verifies request close/abort events,
reply owner close, timer cancellation, Python-side request/reply access, and
clean server shutdown.
Koa now has the same real-listener post-close response-writer check: a captured
context crosses to Python, `ctx.res.write()` after Koa has ended the response
returns Node's rejected write status, and the owner body is not mutated.
Rack/Rails request objects are also covered under a socket-level response abort
that closes the Rack body owner. Rackup/WEBrick now reaches a real
parser/service path for a single accepted socket with a client abort: the Rack
request crosses to JavaScript as a live proxy, the callable response writer
observes the abort, and writer cleanup runs. Ruby `Queue`/`SizedQueue` plus
`NilClass#to_i` provide the core compatibility Rackup/WEBrick expects on that
path. Werkzeug WSGI `start_response` writers are captured from a completed
localhost request and then called from JavaScript after socket close; the
closed-socket `OSError` remains a Python owner error with traceback and boundary
metadata. Rails `ActionDispatch::Response#stream` writer objects now stay live as
resource proxies, preserve `write`/`close`, and report the source `closed
stream` error when a foreign runtime writes after owner close. Express response
writers also stay live across Python calls after `end()`, return Node's
late-write status, and surface the owner-side `ERR_STREAM_WRITE_AFTER_END`
event. The same post-close behavior is now covered for a bare Node core
`http.ServerResponse`, including Python-side status assignment and method calls
against the live owner. aiohttp `StreamResponse` writers also stay live after capture, and a
JavaScript caller writing after Python owner-side `write_eof()` now receives the
source `Cannot call write() after write_eof()` error rather than a generic proxy
or unawaited coroutine result.
Closed Django request bodies fail clearly when read from foreign runtimes, and
transaction rollback is covered for Django, SQLAlchemy sync and async sessions,
ActiveRecord SQLite/PostgreSQL adapters, and Prisma Client transactions after
foreign-runtime errors.

That is still not the same as full production lifecycle safety. Real app
servers have owner phases: client disconnect, response writer close, transaction
commit/rollback, worker drain, and worker reload. The suite now invokes a real
FastAPI and Starlette ASGI app callables with an `http.disconnect` receive event,
including FastAPI `StreamingResponse` body cancellation after the first chunk,
and runs real Django ASGI/Uvicorn, Uvicorn/Starlette, aiohttp, Flask/Werkzeug,
Express, and Fastify TCP listener/client-abort fixtures, plus a Rack/Rails socket-abort
response-body owner fixture, Rackup/WEBrick parser/service client-abort fixture,
and Rails response-writer post-close fixture. Django async
`StreamingHttpResponse.streaming_content` closes wrapper-owned async body
iterators on foreign-runtime cancellation, and a real Django ASGI streaming view
now cleans up its async body when the TCP client aborts after the first chunk.
Prefork worker reload has an explicit `omnivm.drain_worker()` hook, with
handle-count assertions proving live resource and stream proxies are released
before worker exit, including retained and escaped process handles.
`omnivm.drain_worker_hook(*args, **kwargs)` gives Gunicorn/Passenger-style
worker-exit callbacks a natural hook target; it drains initialized workers,
no-ops for workers that never loaded OmniVM, and has a prefork fixture proving
escaped live app-object handles are released through the hook path.
`omnivm.unload_manifest_modules()` remains as a
compatibility alias for code that used the original manifest-specific name. The
Python retained-manifest wrapper also keeps app-owned complex arguments alive
when a retained function returns them as live proxies, and releases those
argument refs when the proxy closes or is finalized.
The remaining tests should cover Puma and other native-threaded Ruby server
processes plus additional server hook reload paths with observable cancellation
after abort/reload events.

### Thread And Event-Loop Affinity

The stress suite already probes several hard embedding paths: Ruby Fiber stack
switching, Ruby Async callbacks and task cancellation/status, nested
JS/Ruby/Python bridge calls, JVM thread callbacks into Python/JS/Ruby, Python raw
threads and `ThreadPoolExecutor` future callbacks, Python asyncio starvation,
asyncio `TaskGroup` cancellation, AnyIO task-group cancellation, JS timer
starvation, watchdog preemption, and timeout recovery.

Python awaitable method results returned through foreign-runtime proxy calls are
now pumped on the owner loop before the call result crosses back. That covers
async writer methods such as aiohttp `StreamResponse.write()`, including
owner-side exceptions raised only when the coroutine is awaited.
Python app integrations can also call `omnivm.affinity_status()` to inspect the
libomnivm host thread, current native thread, and active asyncio loop, or
`omnivm.assert_host_thread(label)` to fail with a structured
`thread_affinity` boundary diagnostic when a lifecycle hook or callback runs on
an unexpected foreign thread. This is a guardrail for framework integration; it
does not pretend to provide universal owner-executor dispatch.
`omnivm.status()["thread_affinity"]` and
`omnivm.owner_dispatch_status()` now expose that boundary as a startup-check
contract: `mode=diagnostic_only`, `owner_dispatch_supported=false`, the pinned
host thread id, and the per-runtime diagnostics/dispatch limitations. Apps that
require callback migration onto a framework-owned loop, executor, or Ruby VM
thread can call `omnivm.assert_owner_dispatch_supported(label)` to reject the
in-process integration before serving traffic with a structured
`thread_affinity` diagnostic.

The remaining risk is framework-owned scheduling. Starlette ASGI app-call
disconnect, Uvicorn event-loop re-entry during streaming response cancellation,
Express event-loop re-entry during a real client abort, Node Web Stream
`pipeTo` abort with underlying-source re-entry into Python, Node classic
`stream.pipeline()` abort cancellation of a Python-owned source, Busboy
multipart upload abort cancellation of a Python-owned request body, an
Express/Multer upload abort through undici and a Python-owned body,
Express/body-parser JSON abort through undici and a Python-owned body,
Koa/bodyparser JSON abort through undici and a Python-owned body, undici
fetch response-body cancellation through a real local HTTP server, undici
request-body cancellation of a foreign-owned iterable, undici `fetch()` upload
abort against a real local HTTP server, and lower-level undici
`request()`, `Client#request()`, `Pool#request()`, and `Agent#request()` upload
aborts with foreign-owned bodies are covered. A custom undici `Dispatcher` that
consumes a foreign-owned upload body and reports a dispatcher-side error also
releases the owner. Ruby app servers such as Puma can still invoke work from
threads or loops OmniVM does not own. Ruby
`Thread.current` and fiber-local framework-style state now survives nested
JavaScript/Python re-entry on the VM-owned Ruby thread. Standard `Float(...)`
coercion now exists in the Ruby bootstrap alongside `Integer(...)`, and guest
Ruby `Thread.new`/`Thread.start`/`Thread.fork` now raise an explicit unsupported
diagnostic rather than deadlocking the embedded runtime. Rackup/WEBrick server
objects now initialize because Ruby `Queue` and `SizedQueue` expose
single-threaded FIFO behavior, and a single accepted socket can pass through
WEBrick request parsing, Rack service dispatch, and response sending when
WEBrick timeout threads are disabled. The Docker stress image also exposes the
Puma and nio4r gems, and `Puma::Server#run` now reaches the same explicit
unsupported-thread diagnostic instead of hanging. Puma itself still depends on
native Ruby thread scheduling, so true in-process Puma support remains open.
`omnivm.status()["ruby_threading"]` now exposes the same boundary as structured
data (`mode=single_vm_thread`, native threads unsupported, native-threaded app
servers such as Puma should run out of process), and
`omnivm.ruby_threading_status()` plus
`omnivm.assert_ruby_native_threads_supported(label)` give host integrations a
startup guard without first invoking `Thread.new`.
Java `CompletableFuture` callback affinity
is covered for both default async dispatch and explicit custom executors, and
`CompletableFuture`/`ExecutorService` Future/`FutureTask`/`ScheduledFuture`/Guava
`ListenableFuture` cancellation status plus
Java `Flow.Publisher` demand/cancel and Reactor/RxJava Disposable status are
covered across JavaScript, Python, and Ruby callers. Scheduler-specific
future/reactive semantics should still be expanded where libraries attach
cancellation to custom schedulers. Tests should distinguish direct same-stack calls from callback or
scheduler re-entry and should assert either safe dispatch to the Golden Thread
or a clear diagnostic rejection.

### Native Memory

The current data-plane evidence is broad for CPU data: Python buffer protocol,
array interface, dataframe interchange, Arrow PyCapsule, DLPack CPU, NumPy
views, Pandas/Polars dataframes, JS typed arrays/DataView/ArrayBuffer, Java
primitive arrays and direct/read-only/sliced ByteBuffers. Non-CPU dataframe
interchange and non-CPU DLPack exporters are intentionally conservative and stay
resource proxies instead of claiming Arrow/shared memory. Nested Arrow C Data,
string-offset Arrow C Data, multi-chunk Arrow streams, and dictionary-encoded
Arrow streams also stay live proxies unless OmniVM can prove a faithful
one-buffer primitive view.

JS `ArrayBuffer`, `Uint8Array`, `Uint8ClampedArray`, and `DataView` runtime refs
can now export through the Arrow/shared buffer path when they are passed as
arguments into Python-owned methods, and byte-oriented tables are unwrapped to
Python `bytes` for APIs that require byte-like input. Named captures still stay
table proxies so shape, dtype, and metadata remain inspectable.

The hard gap is real tensor libraries. JAX CPU tensors now exercise the
DLPack/Arrow path with real dtype, shape, stride, and table contents. PyTorch,
CuPy, and JAX accelerator tensors still expose CPU, pinned CPU, GPU, and
accelerator buffers with distinct ownership and synchronization rules. Additional
CPU tensor libraries can follow the buffer/DLPack/Arrow path when layout,
strides, dtype, and lifetime are proven. GPU tensors currently remain opaque
handles unless an explicit device transfer or device-aware bridge exists.

### Cancellation And Teardown

There is meaningful teardown coverage for stream EOF/cancel, proxy finalizers,
scope cleanup, watchdog timeouts, prefork worker lifecycle, and post-timeout
status. This covers many isolated runtime hazards.

Production cancellation is more demanding. The suite now has framework-object
abort/disconnect coverage for FastAPI, including `StreamingResponse` body
cancellation, Starlette, Django async streaming responses, and Express, FastAPI
and Starlette ASGI app-call disconnect fixtures, real Django ASGI/Uvicorn,
Uvicorn/Starlette, aiohttp, and Express TCP
client-abort streaming fixtures, a Flask/Werkzeug client-abort request-body
fixture with post-abort handle checks, Rack/Rails socket-abort response-body
owner close, Rackup/WEBrick callable response writer cleanup after client
abort, undici fetch response-body cancellation against a real local HTTP
server, Python `requests` response `iter_lines()` early cancellation with
`response.close()`, OkHttp response-body `byteStream()` early cancellation
closing the owner-side source, Java `InputStream`/`Reader`/
`ReadableByteChannel` early cancellation reaching owner-side `close()`,
retained handle drain during prefork worker
recycle, Go c-shared `io.Reader` owner close after JS/Ruby/Java cancellation,
Go c-shared stream cancellation reaching a context-owned reader's
`ctx.Done()`, Go c-shared resource close reaching a context-owned handle's
`ctx.Done()`, Go c-shared manifest job cancellation reaching a context-owned
handle's `ctx.Done()`, plus
Java `CompletableFuture`
cancellation status, `ExecutorService` Future interruption/teardown,
`FutureTask`/`ScheduledFuture`/Guava `ListenableFuture` cancellation status, and Reactor/RxJava
Disposable status crossing JavaScript, Python, and Ruby boundaries. Scheduled Reactor/RxJava stream
cancellation now also proves owner-side scheduler/executor teardown after an
early JS cancel. Undici request-body cancellation now also exercises a
foreign-owned Python iterable through Node's Web `Request` body conversion, Node
Web Stream `pipeTo` abort reaches owner-side Python cancellation from the
underlying source, and undici `fetch()` upload abort exercises the real client
stack against a local HTTP server. Lower-level undici `request()`,
`Client#request()`, `Pool#request()`, and `Agent#request()` upload aborts now
exercise the same foreign-owned body stream path without going through Fetch,
so async-iterator cancellation releases the owner. A custom undici `Dispatcher`
body-consumption error now proves dispatcher-side failures release the same
foreign-owned upload stream. Additional Puma/native-threaded Ruby server aborts,
worker reloads, transaction rollback, and broader library-specific stream
cancellation status should all produce observable cleanup. The next fixture
should assert handle counts before/after an aborted request or worker reload and
should expose cancellation status rather than hiding it in logs.

### Method And Key Collisions

Current tests cover method-colliding mapping keys such as `items`, `keys`,
`count`, `then`, `get`, and `length`, descriptor-field shadowing, unsafe Java
manifest function names, Django model fields, SQLAlchemy ORM mapped fields and
row fields, ActiveRecord field collisions, live setter values, and method
arguments staying behind proxies.
Java value-like zero-arg methods such as `status()`, `count()`, and
`isClosed()` now read naturally as fields through JavaScript, Python, and Ruby
guest proxies, while command-style methods such as `close()` remain explicit
calls.
Python HTTP request/response objects get an attribute-first path for natural
fields like `headers`, so Starlette `request.headers.get(...)` does not fall
through to the raw ASGI scope list.
For Python session-like objects that expose `keys()`/`__getitem__` but are not
registered `Mapping`s, existing contained keys now win over same-named proxy or
framework methods. That path deliberately avoids broad `__contains__` probes, so
lazy result/query/stream-like objects whose membership checks are expensive,
side-effectful, or type-restricted still resolve ordinary attributes and methods
without triggering owner-side membership logic.
Pydantic v2 `BaseModel` objects and dynamic Protobuf message objects with fields
named `items`, `keys`, `count`, `then`, `length`, `get`, and `close` now stay as
resource proxies rather than being mistaken for streams, and field reads/writes
beat same-named model methods while real model methods such as `model_dump()` and
`SerializeToString()` remain callable.
Zod parsed output objects with the same collision-heavy field names stay live
through Python, Ruby, and Java guest access and preserve owner-side JavaScript
state after foreign-runtime mutations.
SQLAlchemy Core `Row` objects are covered for the same collision names,
including sequence-like rows whose owner columns named `keys` or `length` must
beat inherited JavaScript array helpers and length surfaces.
SQLAlchemy lazy relationship child objects use the same collision-heavy mapped
field names and preserve live owner-side mutations through JS/Ruby/Java access.
attrs model objects with the same collision-heavy field names also stay live
behind resource proxies, preserve natural JS/Ruby/Java/Python field syntax, and
round-trip owner-side `attrs.asdict()` state after foreign-runtime mutations.
Python dataclass DTOs with the same collision-heavy fields are covered as live
resource proxies too, including owner-side `dataclasses.asdict()` verification
after JS/Ruby/Java mutations.

Remaining targets are library objects where collision names carry special host
semantics: additional framework model fields and proxy metadata beyond the
common ORM shapes already covered.
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
JavaScript keeps remote `.get` behavior natural when a library object exposes a
real method or field named `get`, and also exposes
`omnivm.proxyGet(proxy, key)`, `omnivm.proxySet(proxy, key, value)`,
`omnivm.proxyCall(proxy, key, args)`, `omnivm.proxyLen(proxy)`,
`omnivm.proxyKeys(proxy)`, `omnivm.proxyValues(proxy)`,
`omnivm.proxyItems(proxy)`, `omnivm.proxyContains(proxy, key)`,
`omnivm.proxyClose(proxy)`, and the collision-free symbol property
`proxy[omnivm.proxyLength]` so users can explicitly choose data-key access,
mutation, method calls, collection length, iteration, membership, or proxy
lease release when method names or `.length` would be ambiguous.
Python retained manifest proxies expose the same explicit operations as
`omnivm.proxy_get`, `omnivm.proxy_set`, `omnivm.proxy_call`,
`omnivm.proxy_len`, `omnivm.proxy_keys`, `omnivm.proxy_values`,
`omnivm.proxy_items`, `omnivm.proxy_contains`, and `omnivm.proxy_close`; Ruby proxies expose
`omnivm_get`, `omnivm_set`, `omnivm_call`, `omnivm_len`, `omnivm_keys`,
`omnivm_values`, `omnivm_items`, `omnivm_contains`, and `omnivm_close`.
Java manifest callers can use `OmniVM.proxyGet`, `OmniVM.proxySet`,
`OmniVM.proxyCall`, `OmniVM.proxyLen`, `OmniVM.proxyIter`,
`OmniVM.proxyKeys`, `OmniVM.proxyValues`, `OmniVM.proxyItems`,
`OmniVM.proxyContains`, and `OmniVM.proxyClose`; for runtime-owned
`HandleProxy` values these helpers now force the remote handle operation before
Java `Map` or reflection behavior can collide with keys such as `get`, `set`, `call`,
`close`, or `length`.

### Error Fidelity

OmniVM has strong crash/stability stress around exception propagation and stack
unwinding, and the Python-facing error wrapper now preserves runtime, type,
message, traceback/stack, cause chain, direct `call[...]` API boundary, manifest
boundary path, and a JSON-serializable `RuntimeError.to_dict()` envelope for
common library errors: Django `ValidationError`,
Pydantic/Marshmallow/jsonschema/Zod validation errors, SQLAlchemy exceptions, Java causes, JavaScript
`Error.cause`, Ruby ActiveRecord exceptions, and Go c-shared errors that wrap
causes with `errors.Unwrap`. Runtime-ref proxy-call failures now strip the
internal `runtime ref assign [...]` wrapper before surfacing the envelope, so a
foreign call into a Python-owned writer still reports the Python owner runtime,
source exception type, traceback, and `call[python]` boundary. Native
JavaScript callers can also catch the normal `Error` thrown by
`omnivm.call(...)` and read `runtime`, `type`, `traceback`, `causeChain`,
`boundaryPath`, and `originalErrorHandle` fields without parsing the message
string; this now includes SQLAlchemy `IntegrityError` structured
`details.statement`/`details.params` payloads from Python database calls,
Pydantic structured `details.errors` payloads from Python validation failures,
generic Python exception `details`/`to_dict()`/`as_dict()`/JSON envelope
methods, Django form JSON error fields under `details.fields`, and ActiveRecord
exception fields plus ActiveRecord validation `record.errors` details from Ruby
calls.
Embedded Python guest callers now catch `omnivm.RuntimeError`
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
chains, Python traceback errors, Pydantic/Marshmallow/jsonschema/Zod validation-library details,
SQLAlchemy `IntegrityError` SQL details, and Django form `ValidationError`
details are covered for native Ruby rescue paths.
Native Ruby exceptions also get source-side structured detail extraction for
explicit `details`, `errors`, ActiveModel/ActiveRecord `record`/`model.errors`,
and no-required-arg `to_h`/`to_hash` methods before the error crosses into
Python/JavaScript/Java.
Native Java throwables also get source-side structured detail extraction for
public no-argument `getDetails()`/`details()`, `getErrors()`/`errors()`,
path/location/original-message accessors or fields, and `toMap()` methods. Extracted
maps, lists, arrays, primitives, and strings are normalized to JSON-safe
`Details:` payloads before the error crosses into Python/JavaScript/Ruby.
Native Java callers can catch
`OmniVM.RuntimeError` as a normal `RuntimeException` and read equivalent
`getRuntime()`, `getType()`, `getTraceback()`, `getCauseChain()`,
`getBoundaryPath()`, `getOriginalErrorHandle()`, and `toMap()` envelope fields;
this includes Pydantic, Marshmallow, jsonschema, and Zod validation-library errors, SQLAlchemy
`IntegrityError` SQL details, Django form `ValidationError` details, and Ruby
ActiveRecord errors crossing into Java callers.
Python, JavaScript, Ruby, and Java rethrow paths now serialize the structured
OmniVM error envelope again when a guest catches and rethrows it, so the next
boundary preserves the source runtime, type, message, traceback, and causes
instead of reducing the error to the rethrowing runtime's local exception class.
Go API callers can use `errors.As` to recover `*omnivm.RuntimeError` with the
same runtime, type, message, traceback, cause-chain, boundary-path, and
original-error-handle/details fields, including runtime-prefixed errors that do
not use the transport `ERR:` marker.

The target error envelope should preserve:

- source runtime;
- source class/type;
- message;
- stack/traceback;
- cause chain;
- manifest op or boundary path;
- library-specific detail payloads when the source exception exposes them;
- whether an original runtime error handle is still available.

The remaining production gap is broadening library-specific payload extraction
past the validation and DBAPI shapes covered here, especially libraries that
store structured diagnostics behind custom properties or non-JSON-native
objects.
Python, JavaScript, Ruby, and Java now parse and propagate an optional
original-error-handle marker when a runtime reports one;
plain JavaScript `Error` objects can expose `originalErrorHandle`, ordinary
Python exceptions can expose `original_error_handle`, ordinary Ruby exceptions
can expose `@original_error_handle`, and ordinary Java throwables can expose
`getOriginalErrorHandle()`, `originalErrorHandle()`, `originalErrorHandle`, or
`original_error_handle`.

## Priority Test Plan

1. Add more real app-server abort/reload fixtures for Puma/native-threaded Ruby servers and worker
   drains, with handle-count assertions after client disconnect or worker drain.
   Django ASGI/Uvicorn, Uvicorn/Starlette, aiohttp, Flask/Werkzeug, and Express
   TCP client-abort coverage is now in the stress suite; Rack/Rails currently has socket-level
   response-owner abort coverage, Rackup/WEBrick parser/service client-abort
   coverage, and
   live resource and stream handles now have a natural
   `omnivm.drain_worker()` hook for prefork worker reloads, but native-threaded
   Ruby app-server propagation remains open. Puma now
   loads in the Docker stress image and reports the embedded Ruby thread
   diagnostic at server startup; it still requires a Ruby-thread scheduling
   strategy before it can run inside the embedded Ruby runtime.
2. Add additional SDK pager/cursor tests for lazy result windows, owner close,
   and backpressure. Prisma cursor-style page windows plus transaction rollback,
   boto3/botocore S3, DynamoDB, CloudWatch Logs, and Google API Core paginator
   windows, and psycopg server-side cursor early cancellation from JS/Ruby/Java
   are now covered.
3. Add more library-specific reactive cancellation status assertions beyond
   covered `CompletableFuture`/`ExecutorService` Future/`FutureTask`/`ScheduledFuture`, Guava `ListenableFuture`, Java `Flow.Publisher`, Kotlin `Job`, Reactor/RxJava
   Disposable object status, scheduled Reactor/RxJava stream teardown, and
   callback-affinity diagnostics.
4. Add more PyTorch/CuPy/JAX tests as optional dependency groups: JAX CPU tensors
   now prove dtype/shape/stride/table contents before Arrow/buffer crossing;
   PyTorch/CuPy CPU tensors and GPU tensors still need coverage, and GPU tensors
   must stay opaque unless an explicit transfer is requested.

## Policy

- Framework-owned objects cross as opaque handles, not JSON.
- Lazy collections cross as lazy stream proxies with a clear owner runtime.
- DB/session/transaction objects never become plain maps.
- Native memory crosses only when layout, device, and ownership are proven.
- Cancellation is explicit and observable in status counters or error details.
- Errors preserve at least runtime, class/type, message, stack, cause chain, and
  boundary path.
