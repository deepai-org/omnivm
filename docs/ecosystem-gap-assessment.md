# Ecosystem Gap Assessment

This document assesses likely OmniVM gaps for popular ecosystem libraries. It is
not a claim that every library works today; it separates current evidence from
open test targets.

## Summary

| Area | Current confidence | Covered evidence | Remaining high-value gap |
| --- | --- | --- | --- |
| Lazy data: querysets, streams, iterators, result sets | Medium | Generic Python/JS/Ruby/Java iterators, readers, async iterables, JS ReadableStream, Java `InputStream`/`Reader`/`ReadableByteChannel`/`BaseStream`, channels | Real Django `QuerySet`, SQLAlchemy result, JDBC `ResultSet`, ActiveRecord `Relation`, Prisma cursor/result set |
| Lifecycle-owned objects: requests, responses, sessions, transactions | Medium-low | Django/FastAPI request objects cross as live proxies; resource/job manifest ops model transaction-like handles | Real request-body close/read-after-close behavior, Django `transaction.atomic()` rollback across foreign-runtime errors, live response writer objects |
| Thread/event-loop affinity: Ruby fiber/thread, JS loop, Java executor, Python async loop | Medium | Ruby Fiber cooperative bridge, JS timer/async starvation, JVM thread bridge calls, Python asyncio starvation, watchdog/interrupt stress | Framework executor ownership: ASGI cancellation, Reactor scheduler callbacks, Java `CompletableFuture` callbacks re-entering Python/JS under cancellation |
| Native memory: Arrow, buffers, tensors, direct ByteBuffers, GPU memory | Medium | Python buffers, NumPy/Pandas/Polars/dataframe interchange, DLPack CPU, JS typed arrays, Java direct/read-only/sliced ByteBuffers, non-CPU dataframe interchange stays proxy | Real PyTorch/CuPy/JAX tensors, GPU DLPack/device transfer policy, multi-buffer/nested/chunked Arrow dictionaries and strings |
| Cancellation/teardown: request aborts, worker reloads, timeouts | Medium | Watchdog timeout/interrupt stress, stream EOF/cancel release, finalizer/scope release, prefork worker lifecycle fixture | End-to-end request abort propagation, worker reload with live stream/resource handles, cross-runtime cancellation status attached to handles |
| Method/key collisions: `items`, `keys`, `count`, `then`, `length` | Medium-high | RuntimeRef mapping keys beat methods; descriptor fields do not shadow runtime object fields; Java unsafe function invoke fallback | Promise-like `then`, array-like `length`, framework model fields colliding with proxy metadata across all runtimes |
| Error fidelity: stack/type/cause across boundaries | Medium-low | Java runner preserves cause text/stack internally; stress has exception ping-pong and stack-unwinding tests | Structured error envelope with runtime, class/type, message, stack, cause chain, boundary path, and original error handle |

## Assessment By Gap Class

### Lazy Data

OmniVM has good generic stream evidence. The CPython-hosted stress harness
covers Python generators, Python sync/async iterable bodies, JavaScript
iterables/async iterables/ReadableStream, Ruby `each`, Java `Iterable`,
`BaseStream`, `InputStream`, `Reader`, and `ReadableByteChannel`. These tests
assert stream proxy captures, no JSON fallback, recorded stream accesses, and
release on EOF or cancellation.

The weak point is ecosystem-owned lazy data whose validity depends on a
database session or framework lifecycle. A Django `QuerySet`, SQLAlchemy
`Result`, JDBC `ResultSet`, ActiveRecord `Relation`, and Prisma cursor can all
look iterable while still needing an open transaction/session/connection. The
next test should prove that a Django `QuerySet` crossing into JavaScript, Ruby,
and Java remains lazy, does not JSON fallback, and only iterates while its
owning Django connection/session is still valid.

### Lifecycle-Owned Objects

Current request-shape tests cover classification: Django `HttpRequest` and
FastAPI/Starlette request objects cross as live resource proxies, not JSON,
stream, or Arrow. Manifest resource/job examples cover explicit opaque
transaction-like handles.

That is not the same as full lifecycle safety. Real framework objects have
owner phases: request body read windows, response write/close windows,
transaction commit/rollback windows, and worker shutdown cleanup. The next
Django app fixture should keep `HttpRequest`, response objects, DB connections,
sessions, and `transaction.atomic()` blocks local to Python unless an explicit
DTO or resource handle crosses. It should verify rollback after a foreign
runtime error and verify that a closed request body cannot be read from another
runtime unless it was intentionally materialized before close.

### Thread And Event-Loop Affinity

The stress suite already probes several hard embedding paths: Ruby Fiber stack
switching, nested JS/Ruby/Python bridge calls, JVM thread callbacks into
Python/JS/Ruby, Python asyncio starvation, JS timer starvation, watchdog
preemption, and timeout recovery.

The remaining risk is framework-owned scheduling. Java `CompletableFuture` and
Reactor callbacks, ASGI task cancellation, Node stream promises, and Ruby
Fiber-local state can invoke work from threads or loops OmniVM does not own.
Tests should distinguish direct same-stack calls from callback or scheduler
re-entry and should assert either safe dispatch to the Golden Thread or a clear
diagnostic rejection.

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

Production cancellation is more demanding. Request aborts, worker reloads,
transaction rollback, stream cancellation, Java future cancellation, Go
`context.Context`, and ASGI disconnects should all produce observable cleanup.
The next fixture should assert handle counts before/after an aborted request and
should expose cancellation status rather than hiding it in logs.

### Method And Key Collisions

Current tests cover method-colliding mapping keys such as `items`, `keys`, and
`count`, descriptor-field shadowing, unsafe Java manifest function names, live
setter values, and method arguments staying behind proxies.

Remaining targets are library objects where collision names carry special host
semantics: JavaScript `then` on promise-like objects, `length` on array-like
objects, ORM/model fields named like collection methods, and proxy metadata
fields colliding with application fields. These should be tested with mutable
objects so wrong dispatch is visible.

### Error Fidelity

OmniVM has strong crash/stability stress around exception propagation and stack
unwinding, but the user-facing error contract is still weaker than production
frameworks need. Popular libraries often branch on error type: Django
`ValidationError`, Pydantic/Zod validation errors, SQLAlchemy exceptions, Java
causes, Ruby ActiveRecord exceptions, and Go wrapped errors.

The target error envelope should preserve:

- source runtime;
- source class/type;
- message;
- stack/traceback;
- cause chain;
- manifest op or boundary path;
- whether an original runtime error handle is still available.

Until that is implemented, cross-runtime error handling should be considered
good enough for diagnostics and fail-fast behavior, but not enough for precise
application recovery logic.

## Priority Test Plan

1. Add a Django ORM stress fixture with a real model, in-memory SQLite,
   `QuerySet` capture into JavaScript/Ruby/Java, no JSON fallback, and
   `transaction.atomic()` rollback after a foreign-runtime error.
2. Add a Django/FastAPI request lifecycle fixture that closes or consumes a body
   in Python, then verifies foreign runtimes cannot read it except through a
   pre-materialized DTO.
3. Add Node stream and Go `io.Reader` cancellation/backpressure tests that stop
   early and assert owner-side close/cancel signals.
4. Add Java `CompletableFuture` and Reactor cancellation tests where callbacks
   attempt to re-enter Python/JS from executor-owned threads.
5. Add ActiveRecord `Relation` tests for lazy iteration while connection state
   is valid and for method/key collision fields on model-like hashes.
6. Add PyTorch/CuPy/JAX tests as optional dependency groups: CPU tensors must
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
