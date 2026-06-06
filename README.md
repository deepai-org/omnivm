# OmniVM

A single Go binary that embeds Python (CPython), JavaScript (Node.js/V8), Java (JVM/JNI), Ruby (MRI), and Go (plugins) — five peer runtimes in one process.

```bash
$ omnivm run hello.py
hello from python

$ omnivm run App.java arg1 arg2
Hello! Args: arg1, arg2

$ omnivm run app.jar
server started on :8080

$ omnivm run main.go --flag value
args: [--flag value]

$ cat data.csv | omnivm run process.py
processed 1000 rows

$ omnivm run app.js --port 3000
listening on :3000
```

## Quick Start

```bash
# Build
docker build -t omnivm .

# Run scripts (language detected by extension)
docker run --rm omnivm run /omnivm/examples/hello.py
docker run --rm omnivm run /omnivm/examples/hello.js
docker run --rm omnivm run /omnivm/examples/hello.rb

# Run Java files (.java compiled in-memory, .class and .jar supported)
docker run --rm omnivm run /omnivm/examples/Hello.java
docker run --rm omnivm run /omnivm/examples/GsonDemo.java hello world

# Run Go programs (compiled as plugins, loaded in-process)
docker run --rm -v $(pwd)/main.go:/app/main.go omnivm run /app/main.go

# Pass arguments (all runtimes — goes to main(String[] args), sys.argv, etc.)
docker run --rm omnivm run /omnivm/examples/hello.py arg1 arg2

# Pipe stdin
echo "hello" | docker run -i --rm -v $(pwd)/upper.py:/app/upper.py omnivm run /app/upper.py

# Shebang support (inside container)
#!/usr/bin/env omnivm run
print("works as a script interpreter")

# Interactive REPL (all runtimes)
docker run -it --rm omnivm

# Inline execution (legacy syntax, still supported)
docker run --rm omnivm -python "print('hello')"
docker run --rm omnivm -js "console.log('hello')"
docker run --rm omnivm -java 'System.out.println("hello");'
docker run --rm omnivm -ruby "puts 'hello'"
docker run --rm omnivm -go 'fmt.Println("hello")'
```

## Python Interpreter Mode

OmniVM can act as a **drop-in Python interpreter**. When the binary is symlinked as `python3` (or invoked as `omnivm python`), it delegates to CPython's `Py_BytesMain()` — the same code path as the stock `python3` binary. Everything works: `-m`, `-c`, script files, pip, interactive REPL, `PYTHONSTARTUP`, `-u`, `-W`, stdin piping.

The difference: `import omnivm` is always available, giving Python code zero-overhead access to Go and JavaScript runtimes.

```bash
# Use as Python interpreter
docker run --rm --entrypoint python3-omnivm omnivm -c "print('I am Python')"

# Or via subcommand
docker run --rm omnivm python -c "print('I am Python')"

# Full CPython CLI works
docker run --rm --entrypoint python3-omnivm omnivm -m site
docker run --rm --entrypoint python3-omnivm omnivm -c "import sys; print(sys.version)"
```

### PolyScript Python Mode

`python3-polyscript` is the progressive-migration entrypoint for existing Python applications. It is a small wrapper around stock `python3`, so the process starts as real CPython rather than the Go-hosted OmniVM binary. That matters for Passenger and Gunicorn prefork modes: the Go runtime is not loaded in the master process.

The wrapper preserves normal CPython behavior for `.py` code, preloads the `polyscript` package, and automatically installs a `.poly` import hook. A Passenger or Django app can swap its Python command first, then convert individual modules or call sites to PolyScript over time.

```bash
docker run --rm --entrypoint python3-polyscript omnivm \
  -c "import polyscript, sys; print(polyscript.is_enabled(), sys.version)"
```

The hook compiles `.poly` files with `POLYSCRIPT_COMPILER` (default: `polyc`). Under `python3-polyscript`, imported `.poly` modules run the generated manifest in-process through CPython-hosted `libomnivm` by default; setting `POLYSCRIPT_MANIFEST_RUNNER` explicitly switches back to an external manifest runner. Existing Python imports keep using CPython; only `.poly` files enter PolyScript. This keeps `python3-polyscript` suitable for Passenger/Gunicorn: the master remains ordinary CPython and each worker loads `libomnivm.so` lazily after it has forked.

```bash
export POLYSCRIPT_COMPILER="polyc"
export POLYSCRIPT_CACHE_DIR="/tmp/polyscript-cache"
python3-polyscript manage.py runserver
```

For prefork servers, keep the master process clean and initialize runtime-heavy work in each worker after fork. Passenger can use `python3-polyscript` as the Python interpreter while `passenger_wsgi.py` remains ordinary Python during the first migration step. `make test-libomnivm-stress` covers this shape with master-import/worker-init, multi-worker, recycled-worker, and `python3-polyscript` WSGI smoke tests.

```python
# passenger_wsgi.py
import os

os.environ.setdefault("POLYSCRIPT_CACHE_DIR", "/tmp/polyscript-cache")

from mysite.wsgi import application
```

As modules are converted, `import billing_rules` can resolve `billing_rules.poly` automatically, and retained manifest functions are exposed as Python callables. That means Django code can use normal imports such as `from billing_rules import rank_user` while the module body still executes through CPython-hosted `libomnivm` inside the worker.

`python3-omnivm` is still available for single-process tools and development. It is Go-hosted CPython and therefore loads the Go runtime before Python starts; use `python3-polyscript` for prefork deployments.

### Calling Go and JavaScript from Python

```python
import omnivm

# Initialize only the runtimes you need (call in Gunicorn post_fork hook)
omnivm.init_runtimes(["go", "javascript"])

# Go — same thread, no IPC, no serialization, GIL released during execution
result = omnivm.call("go", "6 * 7")   # "42"

# JavaScript — full Node.js with require() and npm packages
html = omnivm.call("javascript", "JSON.stringify({status: 'ok'})")

# Errors become Python exceptions, not process crashes
try:
    omnivm.call("go", "invalid!!!")
except RuntimeError as e:
    print(f"Caught: {e}")  # Go compilation error, not a segfault
```

### Pre-compiled Go Plugins

Write Go as Go, not as strings. Compile plugins ahead of time and call exported functions directly:

```python
# Load pre-compiled Go plugin
omnivm.load_plugin("go", "/app/plugins/sessvalidator.so")

# Call exported function — no compilation, no overhead
user_id = omnivm.call("go", 'sessvalidator.ValidateSession("session_key")')
```

### Django / WSGI Integration (Prefork-Safe)

For Gunicorn/Passenger prefork servers, use `libomnivm.so` — a c-shared library loaded post-fork. The Go runtime starts fresh in each worker, avoiding the fatal "Go runtime doesn't survive fork()" problem.

```dockerfile
FROM python:3.14-slim

# Install omnivm Python package (pure Python wrapper + libomnivm.so)
COPY --from=omnivm /usr/local/lib/libomnivm.so /usr/local/lib/
COPY --from=omnivm /usr/local/lib/python3.14/dist-packages/omnivm/ \
     /usr/local/lib/python3.14/dist-packages/omnivm/
RUN ldconfig

# Build Go plugins as c-shared libraries (not -buildmode=plugin)
COPY go_plugins/ /tmp/go_plugins/
RUN cd /tmp/go_plugins/sessvalidator && \
    go build -buildmode=c-shared -o /app/plugins/sessvalidator.so .

# Everything else is standard Django
COPY . /app
RUN pip install -r requirements.txt
CMD ["gunicorn", "myapp.wsgi:application", "--config", "gunicorn.conf.py"]
```

```python
# gunicorn.conf.py
preload_app = True  # Django preloads in master — safe, no Go loaded yet

def post_fork(server, worker):
    """Each worker loads the Go runtime fresh after fork."""
    import omnivm
    omnivm.init_runtimes(["go", "javascript", "java", "ruby"])  # dlopen("libomnivm.so")
    omnivm.set_task_timeout(5000)  # watchdog for direct JS/Ruby calls
    omnivm.load_plugin("go", "/app/plugins/sessvalidator.so")
    omnivm.execute("javascript", "global.marked = require('marked')")
    omnivm.execute("ruby", "require 'json'")

def worker_exit(server, worker):
    """Optional: release live OmniVM handles before an app-server worker exits."""
    import omnivm
    omnivm.drain_worker_hook(server, worker)

# middleware.py
from omnivm import call

class GoSessionMiddleware:
    def __init__(self, get_response):
        self.get_response = get_response

    def __call__(self, request):
        session_key = request.COOKIES.get("sessionid")
        if session_key:
            user_id = call("go", f'sessvalidator.ValidateSession({session_key!r})')
            if user_id:
                request._go_validated_user_id = user_id
        return self.get_response(request)

# views.py
from omnivm import call
import json

def my_view(request):
    # Go for CPU-bound work (GIL released — other threads aren't blocked)
    hash_result = call("go", f'hasher.ComputeHash({request.path!r})')

    # Node.js for npm ecosystem
    html = call("javascript", f'marked.parse({json.dumps(markdown_text)})')

    return JsonResponse({"hash": hash_result, "html": html})
```

**Why c-shared instead of the OmniVM binary?** Go's runtime (GC, scheduler, goroutines) doesn't survive `fork()`. A Go binary is a running Go runtime from the moment it starts — forked children inherit corrupted state. `libomnivm.so` sidesteps this: the master process is pure CPython (no Go), and each worker `dlopen`s the library post-fork, starting a fresh Go runtime.

**Go plugins use `-buildmode=c-shared`**, not `-buildmode=plugin`. Go's plugin system requires the host binary to be a regular executable with plugin metadata tables; a c-shared host can't load plugins. Instead, Go plugins are built as c-shared libraries themselves and loaded via `dlopen`/`dlsym`.

**Direct-call watchdog semantics in c-shared mode** are intentionally explicit:

| Runtime | Timeout behavior |
|---------|------------------|
| JavaScript | `omnivm.set_task_timeout(ms)` can preempt direct calls and nested bridge calls with V8 termination. |
| Ruby | `omnivm.set_task_timeout(ms)` can preempt direct calls and nested bridge calls through Ruby's interrupt hook. |
| Python | Host CPython is interrupted with CPython-native mechanisms; direct libomnivm watchdog arming is not used for Python code. |
| Java | `omnivm.set_task_timeout(ms)` calls `Thread.interrupt()` on the active Java thread. This stops interruptible calls such as `Thread.sleep()` and blocking Java APIs; CPU-bound Java code must check interruption cooperatively. |
| Go plugins | `omnivm.set_task_timeout(ms)` applies a host-call deadline and returns control to CPython. Arbitrary in-process Go plugin code cannot be force-preempted; recycle the worker after a plugin deadline. |

You can inspect the current matrix and worker health at runtime:

```python
import omnivm
omnivm.init_runtimes(["javascript", "java", "ruby"])
print(omnivm.watchdog_capabilities())
print(omnivm.status())

if omnivm.worker_tainted():
    # Let your process manager recycle this worker after the request.
    print(omnivm.worker_taint_reason())
```

`worker_tainted()` is intentionally conservative. It is set after a Go plugin deadline because libomnivm can return control to CPython, but arbitrary in-process Go plugin code may still be running and cannot be safely force-preempted.

In c-shared mode there is no Go-owned background dispatcher thread. Direct calls cooperatively pump async runtimes on the pinned CPython worker thread, so Node/libuv timers such as `setTimeout()` advance on subsequent `omnivm.call()` / `omnivm.execute()` boundaries without violating CPython thread-state ownership.

### Observability

Thread-local call timing for Django middleware:

```python
from omnivm import call, thread_local_total_ms, thread_local_reset

class OmniVMMetricsMiddleware:
    def __init__(self, get_response):
        self.get_response = get_response

    def __call__(self, request):
        thread_local_reset()
        response = self.get_response(request)
        response["X-OmniVM-Time-Ms"] = f"{thread_local_total_ms():.2f}"
        return response
```

Structured cross-runtime errors for application logs:

```python
import logging
import omnivm

logger = logging.getLogger(__name__)

try:
    omnivm.call("javascript", "throw new Error('boom')")
except omnivm.RuntimeError as exc:
    logger.exception(
        "omnivm runtime error",
        extra={"omnivm_error": exc.to_dict()},
    )
```

`RuntimeError.to_dict()` and its `as_dict()` alias return a JSON-serializable envelope with `runtime`,
`origin_runtime`, `type`, `message`, `traceback`, `stack_frames`, `cause_chain`,
`boundary_path`, `original_error_handle`, and `details`. The handle is only populated when
the source runtime reports one; callers should treat it as optional diagnostic
metadata. Structured cause entries preserve their own optional `runtime`,
`origin_runtime`, `boundary_path`, and `original_error_handle` fields when a
source envelope provides them. Native JavaScript `Error` objects created by OmniVM expose the same
envelope through `toJSON()`, so `JSON.stringify(error)` emits matching
snake_case fields. Python, JavaScript, and Ruby runtime error helpers keep
`details`, `details_json`, and `detailsJson` synchronized when code assigns a
new structured details object or a raw JSON details string.

### Two Modes: Interpreter vs Library

| | OmniVM as Python interpreter | `libomnivm.so` (c-shared) |
|---|---|---|
| **How** | Symlink `python3` → `omnivm` | `import omnivm` + `init_runtimes()` |
| **Prefork (Gunicorn)** | Not compatible (Go runtime dies on fork) | Works — Go loads post-fork |
| **Single process** | Works (`gunicorn --workers 1 --threads N`) | Works |
| **`pip install`** | Not needed — it's the interpreter | `pip install omnivm` |
| **All 5 runtimes** | Yes (Python, JS, Go, Java, Ruby) | Yes (Python host + JS, Go, Java, Ruby) |
| **Go plugins** | `-buildmode=plugin` (standard) | `-buildmode=c-shared` (dlopen) |

For most Django deployments (Gunicorn prefork), use the c-shared library.

### Python API Reference

| Function | Description |
|----------|-------------|
| `omnivm.init_runtimes(["go", "javascript", "java", "ruby"])` | Initialize specified runtimes after fork; the caller becomes the c-shared host thread |
| `omnivm.call(runtime, code)` | Eval expression, return result as string (GIL released) |
| `omnivm.execute(runtime, code)` | Execute code (for side effects) |
| `omnivm.load_plugin("go", path)` | Load pre-compiled Go plugin `.so` |
| `omnivm.set_task_timeout(ms)` | Set direct-call watchdog timeout for supported runtimes (`0` disables) |
| `omnivm.watchdog_capabilities()` | Return the runtime timeout/preemption support matrix |
| `omnivm.host_thread_id()` | Return the OS thread id pinned by libomnivm |
| `omnivm.affinity_status()` | Return current Python thread/asyncio-loop affinity diagnostics relative to the libomnivm host thread |
| `omnivm.owner_dispatch_status()` | Return the machine-readable owner-dispatch/thread-affinity capability contract |
| `omnivm.owner_dispatch_target_status(target)` | Return the owner-dispatch capability for `python_asyncio`, `javascript_event_loop`, `java_executor`, or `ruby_fiber_thread`; common aliases such as `asyncio`, `js`, `java`, and `ruby` are accepted |
| `omnivm.assert_owner_dispatch_supported(label="")` | Raise a structured `RuntimeError` when an integration requires universal owner-loop/executor dispatch |
| `omnivm.assert_owner_dispatch_target_supported(target, label="")` | Raise a structured `RuntimeError` when an integration requires one specific owner-loop/executor target |
| `omnivm.ruby_threading_status()` | Return the embedded Ruby threading capability contract |
| `omnivm.assert_ruby_native_threads_supported(label="")` | Raise a structured `RuntimeError` when an integration requires native Ruby threads |
| `omnivm.assert_host_thread(label="")` | Raise a structured `RuntimeError` if a lifecycle callback is running off the libomnivm host thread |
| `omnivm.status()` | Return worker status JSON as a Python dict (`pid`, loaded runtimes, timeout counters, taint state, thread-affinity/Ruby threading boundaries, handle/boundary diagnostics) |
| `omnivm.drain_worker()` | Release live process handles and retained manifest modules before worker drain/reload hooks |
| `omnivm.drain_worker_hook(*args, **kwargs)` | App-server-compatible worker exit/reload hook that drains initialized workers and no-ops for workers that never loaded OmniVM |
| `omnivm.install_worker_drain_hook()` | Register `drain_worker_hook()` with `atexit` as an idempotent process-exit fallback |
| `omnivm.drain_finalizer_releases(max_releases=0)` | Best-effort request/job cleanup hook for queued proxy-finalizer releases (`0` drains all queued releases) |
| `omnivm.lifecycle_scope(max_finalizer_releases=0)` | Sync/async context manager for request/job bodies that drains queued proxy-finalizer cleanup on exit without suppressing body errors |
| `omnivm.worker_tainted()` | Return whether this worker should be recycled after a non-recoverable timeout |
| `omnivm.worker_taint_reason()` | Return the recycle reason for diagnostics |
| `omnivm.last_timeout_runtime()` | Return the runtime that caused the last non-recoverable timeout |
| `omnivm.shutdown()` | Tear down runtimes (optional — process exit works too) |
| `omnivm.RuntimeError.to_dict()` / `as_dict()` | Return a structured runtime error envelope for logging, middleware, and JSON diagnostics |
| `omnivm.cleanup_errors(error)` | Return cleanup exceptions recorded while preserving a body exception |
| `omnivm.get_buffer(name)` | Return a borrowed shared buffer view, or `None` when the name is absent |
| `omnivm.set_buffer(name, data, dtype=0)` | Publish bytes-like data into the shared buffer store |
| `omnivm.release_buffer(name)` | Explicitly release a named shared buffer owner |
| `omnivm.buffer_owner(name[, data], dtype=0)` | Single-active-use sync/async context object for named buffer ownership; publishes optional data on entry, exposes `owner.status()`, and releases the owner name on exit |
| `omnivm.buffer_status(name)` | Return per-name buffer lifecycle diagnostics (`state`, `lease_state`, shape/stride/nullability metadata, and `memory_space`, currently `host` for zero-copy buffers) |

The named-buffer functions are explicit ownership tools for integrations that
publish or borrow shared host memory by name. Ordinary `.poly` snippets that
move buffers, tensors, Arrow arrays, ByteBuffers, memory views, or dataframe
columns across a runtime boundary should keep using native object access; they
should not need manual materialize/release helpers.

Guard failures such as `assert_owner_dispatch_supported()`,
`assert_owner_dispatch_target_supported()`, `assert_host_thread()`, and
`assert_ruby_native_threads_supported()` attach the relevant status block to
`RuntimeError.details` so startup code can log or branch on structured fields
without parsing the message.

### Lazy Initialization

Only the runtime you need is loaded. Running a Go file skips all embedded runtimes entirely (~60ms). Running a Python script only initializes CPython — no JVM, no Ruby, no V8 startup penalty.

### Error Messages

OmniVM enhances errors with actionable suggestions:

```
$ omnivm run app.py
Traceback (most recent call last):
  File "<string>", line 1, in <module>
ModuleNotFoundError: No module named 'requests'

  Hint: pip install requests
```

```
$ omnivm run app.js
Error: Cannot find module 'express'

  Hint: npm install express
```

```
$ omnivm run App.java
JavaError: Class not found: com.example.HttpClient

  Hint: Ensure com.example.HttpClient is on the classpath.
  Place JARs in ./lib/, ./libs/, or /omnivm/libs/
  Maven: mvn dependency:copy-dependencies
  Gradle: gradle copyDependencies
```

```
$ omnivm run main.go
./main.go:5:2: undefined: fmt.Prntln

  Did you mean: fmt.Println?
```

### Exit Codes

Programs' exit codes propagate to the shell:

```bash
$ omnivm run exit42.py; echo $?
42
```

## Architecture

All five runtimes are equal peers orchestrated by a **Golden Thread** dispatcher on the main OS thread. Cross-runtime calls happen synchronously on the same call stack. Go is the host only because its runtime was the pickiest about embedding — not because it has special status. Java files are compiled in-memory via `javax.tools.JavaCompiler` and executed on the embedded JVM — supporting `.java`, `.class`, and `.jar` files with auto-detected classpath. Go files are compiled as plugins (`-buildmode=plugin`), loaded in-process, and can call other runtimes via the bridge.

```
Go main goroutine (runtime.LockOSThread)
  └─ Epoll dispatcher (Linux: eventfd + timerfd + libuv fd)
       ├─ Python (CPython 3.14)  — GIL-wrapped entry, pipe-based interrupt
       ├─ JavaScript (Node.js 22 / V8) — v8::Locker, TerminateExecution
       ├─ Java (JVM 21 / JNI)   — AttachCurrentThreadAsDaemon
       ├─ Ruby (MRI 3.3)        — single VM thread, native Thread.new disabled
       └─ Go (plugins)          — compiled as .so, loaded via plugin.Open

C pthread watchdog (independent of Go scheduler)
  └─ Temporal signal routing: active_runtime → per-runtime interrupt
```

Node.js is embedded via the C++ Embedder API with manual libuv pumping — `uv_run(loop, UV_RUN_NOWAIT)` gives JavaScript cooperative CPU time without starving other runtimes. This means `require()`, npm packages, `setTimeout`, Promises, and the full Node.js API all work.

On Linux, the dispatcher uses **epoll** with eventfd (task wakeup), timerfd (heartbeat), and the libuv backend fd (V8 I/O) — replacing the 1ms polling ticker with event-driven wakeups. A **C pthread watchdog** independently monitors task execution time and dispatches runtime-specific interrupts (Python pipe write, `v8::Isolate::TerminateExecution()`, Ruby trace hook interrupt, Java `Thread.interrupt()`).

### How a Cross-Runtime Call Works (Internals)

When Python calls `omnivm.call("javascript", "Math.sqrt(144)")`:

1. **Python bridge** (`py_omnivm_call` in `pkg/python/python.go`): releases the GIL via `PyEval_SaveThread`, calls the C function pointer `g_bridge_call("javascript", "Math.sqrt(144)")`.
2. **Bridge gateway** (`OmniCall` in the main binary): receives the call on whatever thread invoked it. Looks up the target runtime and calls `jsRuntime.Eval(code)` directly — no dispatcher round-trip for bridge calls.
3. **V8 entry** (`omnivm_v8_eval` in `scripts/v8_bridge_node.cc`): acquires `v8::Locker`, enters the isolate/context, compiles and runs the code, returns the result as a C string.
4. **Return path**: `OmniCall` returns the result string. Python bridge re-acquires the GIL via `PyEval_RestoreThread`, converts the C string to a Python object.

No thread ever holds two runtime locks simultaneously — the source lock is always released before acquiring the target lock. This makes deadlocks impossible by construction.

### Thread Model

```
Main OS thread (Golden Thread):
  runtime.LockOSThread() — pinned for lifetime of process
  Runs: dispatcher loop, all scheduled tasks, V8/Python/Java direct calls

Ruby VM execution lane:
  One Ruby execution lane is active inside OmniVM
  Native Ruby Thread.new/Thread.start/Thread.fork are diagnostic-only
  Puma-style in-process native thread ownership must run out of process

Foreign threads (JVM threads, Python threads, Go goroutines):
  In binary bridge mode, can call runtimes through thread-safe bridge entry points
  In c-shared Python-hosted mode, runtime entrypoints stay pinned to the host worker thread
  Watchdog timeout protection only applies to Golden Thread tasks
```

### Watchdog

The C pthread watchdog (`pkg/watchdog/`) runs independently of Go's scheduler using `pthread_cond_timedwait` with `CLOCK_MONOTONIC`. When armed:

1. The dispatcher sets `active_runtime` before each task
2. If the task exceeds the timeout, the watchdog fires the runtime-specific interrupt
3. The interrupt fires once for the timed-out task; callers re-arm the watchdog for the next task
4. A generation counter prevents stale timeouts after rapid arm/disarm cycles

## Cross-Runtime Calls

The bridge function `omnivm.call(runtime, code)` is available from every runtime.
In the OmniVM binary/REPL bridge it is thread-safe; in c-shared Python-hosted
deployments, direct runtime, manifest, plugin, and typed-call entrypoints are
pinned to the initialized CPython worker thread and expose
`omnivm.owner_dispatch_status()` / `omnivm.assert_host_thread(label)` for
startup and callback checks.

```python
# Python calling JavaScript
result = omnivm.call("javascript", "Math.sqrt(144)")

# JavaScript calling Ruby
var result = omnivm.call("ruby", "('hello' + ' world').upcase");

# Java calling Python (JVM-spawned threads are supported in binary bridge mode)
String result = omnivm.OmniVM.call("python", "2 ** 100");

# Go calling Python (via plugin bridge)
result := OmniVM.Call("python", "2 ** 100")
```

All calls execute synchronously — no marshalling, no IPC, no serialization.
Golden Thread calls are direct C function calls. Binary bridge foreign-thread
calls acquire the target runtime's entry lock where that mode supports them;
c-shared host calls fail fast from non-host threads instead of trying to migrate
work onto an owner loop.

## REPL Commands

```
:python, :py       Switch to Python
:javascript, :js   Switch to JavaScript
:java, :jvm        Switch to Java
:ruby, :rb         Switch to Ruby
:go                Switch to Go
:status            Show runtime status
:quit, :q          Exit
```

## Express + Python Demo

An Express.js HTTP server where route handlers call Python, Ruby, and Java — all on the same thread:

```bash
docker run --rm --entrypoint express-demo omnivm
```

```
Starting Express server...
Express listening on :3000

--- GET / ---
Status: 200 OK
Body:   {"message":"Hello from Express inside OmniVM!",
         "python":"3.14.3","ruby":"3.2.3","java":"21.0.10",
         "engine":"Node.js v22.22.2"}

--- GET /compute ---
Status: 200 OK
Body:   {"fibonacci_50":"12586269025","ruby_reverse":"MVinmO"}
```

## Manifest Executor

The manifest executor runs structured JSON programs that dispatch ops across all five runtimes. A manifest is the IR target produced by the PolyScript compiler — each op specifies a runtime, code, captures, bindings, and control flow.

Manifest proxies are designed so generated `.poly` code can use normal field
reads, method calls, indexing, iteration, membership, length, and lifecycle
cleanup even when owner objects have fields named `then`, `items`, `keys`,
`get`, `close`, `length`, or `count`. Runtime-specific proxy helpers such as
`omnivm.proxyGet`, Python `proxy_get`, Ruby `omnivm_get`, and Java
`OmniVM.proxyGet` remain available for diagnostics and extremely rare manual
escape-hatch debugging, but `.poly` user code should not need them in normal
use. If it does, treat that as a bug in the automatic proxy/codegen behavior.
The same rule applies to native-ish values: generated `.poly` code should pass
and inspect buffers, tensors, Arrow arrays, Java `ByteBuffer`s, Python
`memoryview`s, and similar host-memory objects through the manifest boundary
without rewriting snippets into `get_buffer`, `to_buffer`, `to_arrow`, or
manual release calls.

JavaScript handle and stream proxies also expose `Symbol.dispose` and
`Symbol.asyncDispose` when available for generated cleanup paths. Embedded
JavaScript also provides
`omnivm.bufferOwner(name[, data], dtype[, callback])` for scoped named-buffer
ownership with single-active-use entry and idempotent release. Owners expose `status()`, and
`omnivm.bufferStatus(name)` returns the same per-name lifecycle diagnostics.
JavaScript code that needs owner-loop routing can preflight the current
diagnostic-only boundary with `omnivm.ownerDispatchStatus()`,
`omnivm.ownerDispatchTargetStatus(target)`,
`omnivm.assertOwnerDispatchSupported(label)`, or
`omnivm.assertOwnerDispatchTargetSupported(target, label)`.
JavaScript integrations that may load native-threaded Ruby hosts can
preflight that separate boundary with `omnivm.rubyThreadingStatus()` or
`omnivm.assertRubyNativeThreadsSupported("ruby host startup")`.
Owner fields named `close` or `dispose` remain ordinary fields on retained
Python proxies; generated cleanup uses the proxy lifecycle path instead of
treating those names as lifecycle methods. Python retained handle proxies, stream
iterators, and embedded local stream proxies also support `with` and
`async with` for deterministic release/cancel, and Python stream proxies support
`async for` with the same early-break cancellation as sync iteration. For
ordinary local objects, generated lifecycle helpers honor descriptor-defined
`close()` and `dispose()` without triggering dynamic attribute lookup and await
async close, `aclose()`, and dispose results when needed.
Ruby manifest proxies likewise keep owner fields such as `then`, `class`,
`inspect`, `hash`, `to_s`, `to_h`, `to_a`, `to_json`, `close`, and `dispose`
as ordinary fields when present. Embedded Ruby also
provides `OmniVM.buffer_owner(name[, data], dtype: 0)` for scoped named-buffer
ownership with single-active-use entry and idempotent release. Owners expose
`status()`, and `OmniVM.buffer_status(name)` returns the same per-name lifecycle
diagnostics.
Ruby code that may load native-threaded app servers can call
`OmniVM.ruby_threading_status` or
`OmniVM.assert_ruby_native_threads_supported("puma startup")` before invoking
framework startup. Ruby code that needs owner-loop routing can also preflight
the diagnostic-only boundary with `OmniVM.owner_dispatch_status`,
`OmniVM.owner_dispatch_target_status(target)`,
`OmniVM.assert_owner_dispatch_supported(label)`, or
`OmniVM.assert_owner_dispatch_target_supported(target, label)`.
Generated Java manifest code also treats proxy collisions as automatic owner
shape decisions rather than a reason for user `.poly` code to call static proxy
helpers. Java also provides `OmniVM.bufferOwner(name[, data], dtype)` as an `AutoCloseable`
single-active-use named-buffer owner for try-with-resources cleanup.
`OmniVM.bufferStatus(name)` and `owner.status()` return JSON lifecycle
diagnostics from the shared buffer store.
Java code that needs owner-executor routing can preflight the current
diagnostic-only boundary with `OmniVM.ownerDispatchStatus()`,
`OmniVM.ownerDispatchTargetStatus(target)`,
`OmniVM.assertOwnerDispatchSupported(label)`, or
`OmniVM.assertOwnerDispatchTargetSupported(target, label)`.
Java integrations that may load native-threaded Ruby app servers can preflight
that separate boundary with `OmniVM.rubyThreadingStatus()` or
`OmniVM.assertRubyNativeThreadsSupported("puma startup")`.
Go manifest callers can use `manifest.ProxyClose(value)` or the idiomatic alias
`manifest.OmniVMClose(value)` to close handle proxies, stream proxies, or
ordinary Go `Close() error` values through the same collision-safe lifecycle
path; the compatibility alias `manifest.OmnivmClose(value)` remains available
for generated-helper naming parity. The helpers return `(closed, error)` so
failed owner cleanup is not mistaken for a successful close.
Go integrations that need owner-loop/executor routing can preflight the same
diagnostic-only boundary with `manifest.OwnerDispatchStatus()`,
`manifest.OwnerDispatchTargetStatus(target)`,
`manifest.AssertOwnerDispatchSupported(label)`, or
`manifest.AssertOwnerDispatchTargetSupported(target, label)`. Go code that may
load native-threaded Ruby app servers can also call
`manifest.RubyThreadingStatus()` or
`manifest.AssertRubyNativeThreadsSupported(label)`.

```bash
# Run a single manifest
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/cursed-concurrency.json

# Verify boundary decisions without starting guest runtimes
docker run --rm --entrypoint manifest-runner omnivm --doctor /omnivm/examples/cursed-concurrency.json

# Run the focused spawn/channel contract regression
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/spawn-channel-contract.json

# Showcase examples
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/fizzbuzz-polyglot-manifest.json
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/data-pipeline-manifest.json
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/polyglot-pipeline-manifest.json

# Run the full manifest test suite
make test-manifests
```

### Supported Ops

| Op | Description |
|----|-------------|
| `exec` | Execute code (side effects, stdout capture) |
| `eval` | Evaluate expression (returns value) |
| `import` | Runtime-specific module import |
| `func_def` | Define a manifest function (with optional generator, Go plugin source) |
| `return` | Return from function |
| `if` | Conditional branching with arms + else |
| `loop` | While/for/foreach/infinite loops |
| `declare` / `assign` | Variable binding and mutation |
| `concat` | String interpolation with cross-runtime eval segments |
| `try` / `throw` | Error handling with catch/finally |
| `parallel` | Concurrent branch execution |
| `chan` | Go channel operations (make/send/recv/close) |
| `select` | Go-style select on channels |
| `spawn` | Launch Go functions or manifest func_defs, optionally binding a spawn handle |
| `resource` | Open/close opaque runtime-owned resources with explicit disposer metadata |
| `table` | Export/release runtime-owned table or buffer handles, preferably Arrow C Data Interface |
| `job` | Enqueue, complete, and wait on manifest-visible background job handles |
| `yield` | Generator yield (with delegate support) |
| `await` | Async/await semantics |

Channel, spawn, and `wait()` semantics are defined in
[`docs/manifest-channel-contract.md`](docs/manifest-channel-contract.md).
Cross-runtime value movement is specified in
[`docs/boundary-semantics.md`](docs/boundary-semantics.md), with the staged
performance plan in
[`docs/bridge-performance-plan.md`](docs/bridge-performance-plan.md).

Manifests are validated when parsed. The validator checks the stable executor contract: supported runtimes and op names, required fields for ops such as `spawn`, `chan`, `select`, `func_def`, and nested control-flow bodies. It intentionally does not reject dynamic binding-liveness cases that only execution can know.

### Channels and Spawn Handles

Manifest channels are shared executor values, not runtime-local queues. `chan` ops create them, send/recv/close ops mutate them, and captures inject iterable/readable wrappers into runtimes such as JavaScript and Python. The Go manifest helpers `recv(ch)` and `send(ch, value)` expose the same channel values to compiled Go worker functions.

`spawn` returns a manifest-visible handle when the op has a `bind` field:

```json
{ "op": "spawn", "runtime": "go", "code": "worker(1)", "bind": "w1" }
```

The `wait` helper has three forms:

| Form | Result |
|------|--------|
| `wait()` | Waits for every spawned worker and returns the total spawn count |
| `wait(handle)` | Waits for one handle and returns that worker's result |
| `wait(h1, h2, ...)` | Waits in argument order and returns an array of worker results |

This is what lets a `.poly` source file express real worker joins:

```polyscript
const w1 = go worker(1)
const w2 = go worker(2)
const joined = wait(w1, w2)
```

The `spawn-channel-contract.json` example is the small regression manifest for this behavior. `cursed-concurrency.json` is the larger end-to-end example that combines Go workers, shared channels, JavaScript channel iteration, and Python aggregation.

## Stress Tests

The stress suite verifies correctness under pressure:

```bash
docker run --rm --entrypoint stresstest omnivm
```

Tests cover cross-runtime stack mixing, generators across C boundaries, asyncio pumping with bridge callbacks, re-entrant calls (Python → JS → Python), signal handling (JVM SIGSEGV + Ruby + Python interrupts), GC interaction, 1MB string round-trips, Ruby Fiber cooperative bridging, 4-runtime mutual recursion (18 levels deep), Golden Thread verification, `pthread_atfork` fork guard, watchdog-driven preemption of infinite loops across all runtimes, binary-mode foreign-thread bridge calls (JVM threads → Python/JS/Ruby), c-shared host-thread affinity diagnostics, concurrent multi-thread bridge contention, and nested foreign-thread cross-runtime chains.

## Library API

The `pkg/omnivm` package lets you embed OmniVM as a Go library — no CLI required. This is designed for production use cases like a Go HTTP server calling Django's ORM:

```go
package main

import (
    "context"
    "fmt"
    "log"
    "net/http"
    "os/signal"
    "runtime"
    "syscall"
    "time"

    "github.com/omnivm/omnivm/pkg/omnivm"
    "github.com/omnivm/omnivm/pkg/python"
)

func init() { runtime.LockOSThread() }

func main() {
    vm := omnivm.New(omnivm.Config{
        TaskTimeout:  30 * time.Second,
        DrainTimeout: 25 * time.Second,
    })

    // Only load what you need — no JVM, no Ruby, no V8 overhead
    vm.Register("python", python.New())

    if err := vm.Start(); err != nil {
        log.Fatal(err)
    }

    // Django setup — runs once, state persists across all calls
    vm.Execute("python", `
        import os, django
        os.environ['DJANGO_SETTINGS_MODULE'] = 'myapp.settings'
        django.setup()
    `)

    // DB cleanup after every call (runs even on error, like defer)
    vm.SetAfterCall("python",
        "from django.db import close_old_connections; close_old_connections()")

    ctx, cancel := signal.NotifyContext(context.Background(),
        syscall.SIGTERM, syscall.SIGINT)
    defer cancel()

    go func() {
        http.HandleFunc("/api/user", func(w http.ResponseWriter, r *http.Request) {
            // Per-request context — cancels if client disconnects
            result, err := vm.CallWithContext(r.Context(), "python", fmt.Sprintf(
                `from apps.models import User; User.objects.get(id=%%q).to_json()`,
                r.URL.Query().Get("id"),
            ))
            if err != nil {
                http.Error(w, "internal error", 500)
                return
            }
            w.Header().Set("Content-Type", "application/json")
            w.Write([]byte(result))
        })
        log.Fatal(http.ListenAndServe(":8080", nil))
    }()

    vm.Run(ctx)  // blocks on Golden Thread
    vm.Shutdown() // drain hooks → runtime teardown (reverse order)
}
```

### Library API Reference

| Method | Description |
|--------|-------------|
| `New(Config)` | Create a VM instance |
| `Register(name, runtime)` | Add a runtime (selective — only load what you need) |
| `Start()` | Initialize runtimes on Golden Thread |
| `Run(ctx)` | Block running the dispatcher (returns on context cancel) |
| `Call(runtime, code)` | Eval code, return result as string (goroutine-safe) |
| `CallWithContext(ctx, runtime, code)` | Call with per-request deadline/cancellation |
| `CallWithRequestID(ctx, runtime, code, id)` | Call with request ID for metrics correlation |
| `CallFast(runtime, code)` | Priority eval — skips ahead of queued normal calls |
| `CallFastWithContext(ctx, runtime, code)` | Priority eval with deadline/cancellation |
| `CallFastWithRequestID(ctx, runtime, code, id)` | Priority eval with request ID |
| `Execute(runtime, code)` | Run code, return captured stdout (goroutine-safe) |
| `ExecuteWithContext(ctx, runtime, code)` | Execute with per-request deadline/cancellation |
| `LoadFile(runtime, path)` | Execute a file's contents (define helpers from .py files) |
| `SetAfterCall(runtime, code)` | Cleanup code that runs after every call (like `defer`) |
| `SetOnCallDone(fn)` | Observe-only callback with `CallMetrics` (duration, queue wait, fast/normal, request ID) |
| `CallBatch(runtime, items)` | Execute multiple independent snippets in one Golden Thread dispatch |
| `CallBatchWithContext(ctx, runtime, items, requestID)` | Batch call with context and request ID |
| `RegisterDrainHook(fn)` | Shutdown hook — runs on Golden Thread, can call `drainExecute()` |
| `Shutdown()` | Graceful stop: drain hooks (on Golden Thread) → reverse-order runtime teardown |

### Helper Function Pattern

The interpreter is persistent — variables and functions survive across calls. The recommended pattern is to define Python helper functions at startup (from files), then call them with one-liners per request:

```python
# helpers/user.py
import json
from django.contrib.auth.models import User

def get_user_json(user_id):
    u = User.objects.get(id=int(user_id))
    return json.dumps({"email": u.email, "active": u.is_active})

def validate_session(session_key):
    from django.contrib.sessions.backends.db import SessionStore
    s = SessionStore(session_key=session_key)
    return s.get("_auth_user_id", "")
```

```go
// At startup — load helpers from files (not inline strings)
vm.LoadFile("python", "helpers/user.py")

// Per request — clean one-liner
result, err := vm.Call("python", fmt.Sprintf(`get_user_json(%q)`, userID))
sessionUID, err := vm.Call("python", fmt.Sprintf(`validate_session(%q)`, sessionKey))
```

The string-returning `Call` API is the compatibility surface, so helpers that
produce HTTP JSON can still return JSON directly. That is not the long-term
boundary model for PolyScript or manifest execution: framework objects, ORM
models, buffers, streams, and other complex values should cross automatically
through the generic `copy`, `ref`, `stream`, or Arrow boundary selected from the
value's protocol shape. The runtime must not rely on special cases for Django,
Pandas, PIL, or any other package name.

### Priority Dispatch

The dispatcher has two channels: a **fast channel** (64 slots) and a **normal channel** (256 slots). Fast tasks are always drained before normal tasks on every dispatcher cycle, reducing head-of-line blocking.

Use `CallFast` for latency-sensitive operations (auth checks, session validation) and `Call` for heavier business logic (report generation, batch processing). A slow Python query in the normal queue won't block fast auth checks queued behind it:

```go
// Auth middleware — uses priority channel, skips ahead of slow queries
userID, err := vm.CallFast("python", fmt.Sprintf(`validate_session(%q)`, sessionKey))

// Business logic — normal priority
report, err := vm.Call("python", fmt.Sprintf(`generate_report(%q)`, reportID))
```

### Batch Calls

When a single HTTP handler needs multiple independent pieces of data, `CallBatch` executes them all in one Golden Thread dispatch — avoiding N round-trips through the dispatcher queue:

```go
results := vm.CallBatch("python", []omnivm.BatchItem{
    {Code: "get_subscription_state(123)"},
    {Code: "get_usage_totals(123)"},
    {Code: "get_lock_status(123)"},
})
// results[0].Value, results[0].Err — independent per item
// results[1].Value, results[1].Err
// results[2].Value, results[2].Err
```

Each item gets independent error handling — a failure in item 1 does not prevent item 2 from executing. `AfterCall` runs once after all items complete (not per-item). Use `CallBatchWithContext` for context cancellation and request ID correlation.

### Observability

`SetOnCallDone` receives a `CallMetrics` struct with production-grade telemetry:

```go
vm.SetOnCallDone(func(m omnivm.CallMetrics) {
    histogram.WithLabelValues(m.Runtime, fmt.Sprint(m.Fast)).
        Observe(m.Duration.Seconds())
    if m.QueueWait > 50*time.Millisecond {
        log.Warn("high dispatcher queue wait",
            "request_id", m.RequestID,
            "queue_wait", m.QueueWait,
            "exec_duration", m.Duration)
    }
})
```

| Field | Type | Description |
|-------|------|-------------|
| `Runtime` | `string` | Which runtime was called (`"python"`, `"javascript"`, etc.) |
| `Result` | `string` | String result (empty on error) |
| `Err` | `error` | `nil` on success |
| `Duration` | `time.Duration` | Wall-clock execution time on the Golden Thread |
| `QueueWait` | `time.Duration` | Time spent waiting in the dispatcher queue |
| `Fast` | `bool` | `true` if dispatched via the high-priority channel |
| `RequestID` | `string` | Caller-provided correlation ID (via `CallWithRequestID` / `CallFastWithRequestID`) |

### Concurrency Model

All runtime calls serialize through the Golden Thread — Python and JavaScript cannot overlap. This is inherent to cgo and the GIL/GVL. The performance model is: Go handles HTTP routing and concurrency (fast), Python/JS/Ruby handle business logic (serialized but short). `CallWithContext` provides caller-side cancellation, but the Golden Thread task runs to completion (cgo cannot be interrupted mid-call).

### AfterCall Performance

`SetAfterCall` cleanup code (e.g., `close_old_connections()`) uses the lightweight `Eval` path internally, skipping the stdout/stderr capture overhead that `Execute` requires. This saves ~100μs per request compared to routing cleanup through `Execute`.

## Project Structure

```
cmd/
  omnivm/            Main binary (REPL + CLI + `run` subcommand + Python interpreter mode)
  libomnivm/         c-shared library for pip-installable Python package (prefork-safe)
  manifest-runner/   JSON manifest executor
  stresstest/        71-test stress suite
  express-demo/      Express + Python/Ruby/Java HTTP demo
  telephone/         Cross-runtime telephone game
pkg/
  engine/            Shared runtime management core (used by both omnivm and libomnivm)
  omnivm/            Library API (VM, Config, Call, Execute, Shutdown)
  cli/               CLI parsing, language detection, shebang handling
  errmsg/            Error enhancement (hints, traceback formatting)
  golang/            Go runtime (plugin-based, in-process compilation + execution)
  python/            CPython embedding via cgo
  javascript/        Node.js/V8 embedding via cgo
  jvm/               JVM embedding via JNI/cgo
  ruby/              MRI Ruby embedding via cgo
  manifest/          Manifest IR executor (ops, captures, channels, stubs)
  dispatcher/        Golden Thread task serializer (epoll on Linux)
  watchdog/          C pthread watchdog with temporal signal routing
  signals/           Signal handler management
  arrow/             Shared memory primitives
scripts/
  v8_bridge_node.cc    Node.js ↔ v8_bridge.h C++ adapter
  test-manifests.sh    Manifest test suite runner
  test-cli.sh          CLI integration tests (29 tests)
  test-libomnivm-*.sh  CPython-hosted libomnivm manifest/stress tests
  test-poly-libomnivm-smoke.sh  Compile sibling PolyScript examples and run via CPython + libomnivm
runtime/
  java/              OmniVMRunner.java (in-memory compilation, file/jar/class execution)
examples/            Manifest JSON files and sample scripts
docs/                Manifest contracts and design notes
```

## Building & Testing

Requires Docker. The multi-stage Dockerfile handles all dependencies (Go, CPython, Node.js, JVM, Ruby). Unit tests run during the build. Integration tests run against the final image.

```bash
# Build (runs unit + integration tests during build)
docker build -t omnivm .

# Run ALL tests against the built image
docker run --rm --entrypoint /bin/bash omnivm /omnivm/scripts/test-cli.sh  # 29 CLI tests
docker run --rm --entrypoint stresstest omnivm                             # 71 stress tests
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/manifest-test.json

# REPL
docker run -it --rm omnivm
```

The Docker build runs three test tiers: (1) unit tests with race detector for pure Go packages, (2) cgo-linked runtime tests for Python/JS/Ruby/Engine, and (3) cross-runtime integration tests that verify all runtimes initialize and interoperate correctly. CLI, stress, and manifest tests run against the final image.

Make targets:

```bash
make build                # Build Docker image
make test-all             # Canonical local/CI gate: local, Docker, manifest, stress, and libomnivm tests
make test-cli             # CLI integration tests (29 tests in Docker)
make test-manifests       # Run manifest examples and edge contract fixtures
make test-libomnivm-manifests # Run all example JSON manifests via CPython + libomnivm
make test-libomnivm-stress    # Run CPython-hosted libomnivm stress checks
make test-libomnivm-stress STRESS_ARGS="--category proxy --name materializes" # Filter stress checks
make test-poly-libomnivm-smoke # Compile selected PolyScript examples, then run via CPython + libomnivm
make test-stress          # Run 71 stress tests
```

The cross-repo `.poly` smoke expects a sibling PolyScript compiler checkout at `../polyscript-compiler` by default. Prefer `POLYSCRIPT_DIR` for explicit paths; the older `GARBAGE_DIR` variable remains accepted for existing scripts:

```bash
POLYSCRIPT_DIR=/path/to/polyscript-compiler make test-poly-libomnivm-smoke
```

The README-level CI parity sequence is:

```bash
# PolyScript compiler checkout
npm test -- --runInBand
npm run build
node scripts/audit-manifests.js

# omnivm
make test-all
make test-poly-libomnivm-smoke
make test-libomnivm-manifests
make test-libomnivm-stress
```

## Key Design Decisions

- **Lazy runtime initialization**: Only the runtime needed for the target file is started. `omnivm run main.go` skips all embedded runtimes. `omnivm run script.py` only starts CPython.
- **Java file execution**: `omnivm run App.java` compiles in-memory via `javax.tools.JavaCompiler` and runs on the embedded JVM with real `main(String[] args)` and direct stdout/stderr. Supports `.class` and `.jar` files. Classpath auto-detects Maven (`target/dependency/`), Gradle (`build/libs/`), and `lib/`/`libs/` directories — downloaded JARs just work.
- **Go as equal peer**: Go files are compiled as plugins (`-buildmode=plugin`), loaded in-process, and executed — not via subprocess. `func main()` is transformed to an exported `func Main()` via the Go AST, compiled, and called via `plugin.Open`/`Lookup`. Go plugins can call other runtimes through the bridge (`OmniVM.Call("python", "...")`) and participate in the REPL and inline execution (`omnivm -go 'code'`). Go is the host because its runtime was the pickiest about embedding, not because it has special status.
- **Bridge gateway affinity**: The OmniVM binary bridge supports foreign-thread calls by entering the target runtime through the appropriate lock: `PyGILState_Ensure` (Python), `v8::Locker` (V8), the Ruby VM execution lane, or `AttachCurrentThreadAsDaemon` (JVM). In c-shared Python-hosted mode, CPython owns the worker thread state, so runtime, manifest, plugin, and typed-call entrypoints reject non-host threads with structured `thread_affinity` diagnostics instead of attempting universal owner-loop dispatch.
- **Ruby single-VM-thread boundary**: Ruby runs through one OmniVM-owned VM execution lane. Ruby 3.3's M:N threading breaks `Thread.new` and `rb_thread_call_without_gvl` in this embedded shape, so native Ruby thread creation raises an explicit diagnostic instead of hanging. Ruby `fork`, `Kernel.fork`, `Process.fork`, `Process.daemon`, `Process.spawn`, `Kernel.spawn`, `Kernel.system`, `Kernel.exec`, backticks, and `IO.popen` also raise explicit diagnostics after OmniVM initializes; Ruby code that needs preforking, daemonization, or subprocess launch must do so before loading OmniVM or run that component out of process. `omnivm.status()["ruby_threading"]` and `omnivm.ruby_threading_status()` report this boundary (`mode=single_vm_thread`, native threads unsupported), and `omnivm.assert_ruby_native_threads_supported(label)` is the fail-fast guard for host apps that need an out-of-process Puma deployment before loading a threaded Ruby app server.
- **Epoll dispatcher (Linux)**: eventfd for task wakeup, timerfd for heartbeat, libuv backend fd for V8 I/O. Replaces the 1ms polling ticker with event-driven wakeups — zero CPU when idle.
- **C pthread watchdog**: Independent of the Go scheduler. `pthread_cond_timedwait` with `CLOCK_MONOTONIC` (immune to NTP jumps). Temporal signal routing dispatches runtime-specific interrupts: Python pipe write, `v8::Isolate::TerminateExecution()`, Ruby trace hook interrupt, and Java `Thread.interrupt()`.
- **Error enhancement**: Missing module errors get "pip install" / "npm install" / "gem install" hints. Python tracebacks are reformatted with `file:line` references. Go compile errors get "Did you mean?" suggestions.
- **Node.js over Duktape**: Duktape was ES5.1 — no `const`/`let`, no arrow functions, no `require()`, no npm. Node.js (via `libnode-dev` / `libnode127`) gives full ES2024+, the npm ecosystem, and built-in modules.
- **Skip `Py_FinalizeEx`, `ruby_cleanup()`, `V8::Dispose()`**: All crash in a polyglot process. Process exit reclaims resources.
- **`LD_PRELOAD=libjsig.so`**: JVM uses SIGSEGV for NullPointerException safepoints. Without signal chaining, this crashes Ruby. libjsig.so chains handlers properly.
- **`pthread_atfork` fork guard**: Child processes after `fork()` have dead JVM threads holding mutexes. The guard `_exit(71)`s with a diagnostic stack trace — both the C backtrace (via glibc `backtrace_symbols_fd`) and the Python traceback (via `faulthandler.dump_traceback`) are logged to stderr, identifying exactly which dependency triggered the fork. Python forced to `multiprocessing.set_start_method('spawn')`. The fork guard is **conditional** — it only fires when JVM or Ruby are loaded. Go+JS-only configurations are fork-safe when runtimes are initialized post-fork (the Gunicorn/Passenger pattern).
- **Python interpreter mode**: When symlinked as `python3`, OmniVM calls `Py_BytesMain()` — CPython's own entry point. `PyImport_AppendInittab("omnivm", ...)` registers the `omnivm` module before CPython initializes, so `import omnivm` works in any Python code. Best for single-process deployments (dev, `gunicorn --workers 1 --threads N`, uvicorn). Not compatible with prefork — Go's runtime doesn't survive `fork()`.
- **c-shared library mode (`libomnivm.so`)**: For prefork servers (Gunicorn, Passenger, uWSGI). Built with `go build -buildmode=c-shared`. All 5 runtimes are supported: JavaScript, Java, Ruby, Go (via dlopen plugins), and Python (host - cross-runtime bridge calls back into the already-running CPython). The master process is pure CPython - no Go runtime loaded. Each worker calls `omnivm.init_runtimes()` post-fork, which `dlopen`s `libomnivm.so`, starts a fresh Go runtime, and pins the calling Python worker thread as the c-shared host thread. Direct calls and manifest execution must enter from that host thread; non-host callers fail fast with structured `thread_affinity` diagnostics. The background epoll dispatcher is intentionally not started in c-shared mode because CPython owns the process and thread state. Async runtimes are pumped cooperatively at host call boundaries, so Node/libuv timers progress without a Go-owned dispatcher thread. The watchdog, buffer bridge, cross-runtime bridge, and fork guard are active. Direct-call watchdog support is runtime-specific: JavaScript and Ruby can be preempted, Java receives `Thread.interrupt()`, Go plugin calls get a host-call deadline, and host Python uses CPython-native interruption. Workers expose `omnivm.status()`, `omnivm.owner_dispatch_status()`, and conservative taint flags so servers can recycle after a non-recoverable Go plugin deadline and can fail fast when they require universal owner-loop/executor dispatch. PolyScript examples are compiled and executed through this path by `make test-poly-libomnivm-smoke`; all example JSON manifests are covered by `make test-libomnivm-manifests`, and CPython-hosted nested callback/buffer/fork/prefork lifecycle/watchdog checks are covered by `make test-libomnivm-stress`. See `docs/passenger-django-polyscript.md` for the Passenger/Django migration shape and `docs/example-suite.md` for example-suite coverage. Both binaries share the `pkg/engine` package for runtime lifecycle, bridge wiring, watchdog setup, and shutdown - the `//export` C wrappers are thin. Go plugins must be built as `-buildmode=c-shared` (not `-buildmode=plugin`) and are loaded via `dlopen`/`dlsym`.
