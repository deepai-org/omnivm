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
    omnivm.load_plugin("go", "/app/plugins/sessvalidator.so")
    omnivm.execute("javascript", "global.marked = require('marked')")
    omnivm.execute("ruby", "require 'json'")

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
| `omnivm.init_runtimes(["go", "javascript", "java", "ruby"])` | Initialize specified runtimes (call post-fork) |
| `omnivm.call(runtime, code)` | Eval expression, return result as string (GIL released) |
| `omnivm.execute(runtime, code)` | Execute code (for side effects) |
| `omnivm.load_plugin("go", path)` | Load pre-compiled Go plugin `.so` |
| `omnivm.shutdown()` | Tear down runtimes (optional — process exit works too) |

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
       ├─ Ruby (MRI 3.3)        — proxy pthread, pipe-based interrupt
       └─ Go (plugins)          — compiled as .so, loaded via plugin.Open

C pthread watchdog (independent of Go scheduler)
  └─ Temporal signal routing: active_runtime → per-runtime interrupt
```

Node.js is embedded via the C++ Embedder API with manual libuv pumping — `uv_run(loop, UV_RUN_NOWAIT)` gives JavaScript cooperative CPU time without starving other runtimes. This means `require()`, npm packages, `setTimeout`, Promises, and the full Node.js API all work.

On Linux, the dispatcher uses **epoll** with eventfd (task wakeup), timerfd (heartbeat), and the libuv backend fd (V8 I/O) — replacing the 1ms polling ticker with event-driven wakeups. A **C pthread watchdog** independently monitors task execution time and dispatches runtime-specific interrupts (Python pipe write, `v8::Isolate::TerminateExecution()`, Ruby proxy `Thread#raise`).

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

Ruby proxy pthread:
  Holds GVL permanently, processes requests via condvar
  Ruby 3.3's M:N scheduler can't schedule threads on non-main pthreads
  All Ruby calls (Golden Thread or foreign) route through this proxy

Foreign threads (JVM threads, Python threads, Go goroutines):
  Can call any runtime directly via thread-safe entry points
  Each runtime entry acquires its own lock (GIL/Locker/GVL-proxy/JNI attach)
  Watchdog timeout protection only applies to Golden Thread tasks
```

### Watchdog

The C pthread watchdog (`pkg/watchdog/`) runs independently of Go's scheduler using `pthread_cond_timedwait` with `CLOCK_MONOTONIC`. When armed:

1. The dispatcher sets `active_runtime` before each task
2. If the task exceeds the timeout, the watchdog fires the runtime-specific interrupt
3. The interrupt fires repeatedly until the task completes or the watchdog is disarmed
4. A generation counter prevents stale timeouts after rapid arm/disarm cycles

## Cross-Runtime Calls

The bridge function `omnivm.call(runtime, code)` is available from every runtime and from any thread:

```python
# Python calling JavaScript
result = omnivm.call("javascript", "Math.sqrt(144)")

# JavaScript calling Ruby
var result = omnivm.call("ruby", "('hello' + ' world').upcase");

# Java calling Python (works from JVM-spawned threads too)
String result = omnivm.OmniVM.call("python", "2 ** 100");

# Go calling Python (via plugin bridge)
result := OmniVM.Call("python", "2 ** 100")
```

All calls execute synchronously — no marshalling, no IPC, no serialization. Golden Thread calls are direct C function calls. Foreign thread calls automatically acquire the target runtime's lock (GIL, GVL, v8::Locker, or JNI AttachCurrentThread).

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

The manifest executor runs structured JSON programs that dispatch ops across all five runtimes. A manifest is the IR target for a hypothetical PolyScript compiler — each op specifies a runtime, code, captures, and control flow.

```bash
# Run a single manifest
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/cursed-concurrency.json

# Run the full manifest test suite (11 tests, 6 categories)
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
| `spawn` | Launch Go functions or manifest func_defs |
| `yield` | Generator yield (with delegate support) |
| `await` | Async/await semantics |

## Stress Tests

71 tests verify correctness under pressure:

```bash
docker run --rm --entrypoint stresstest omnivm
```

Tests cover cross-runtime stack mixing, generators across C boundaries, asyncio pumping with bridge callbacks, re-entrant calls (Python → JS → Python), signal handling (JVM SIGSEGV + Ruby + Python interrupts), GC interaction, 1MB string round-trips, Ruby Fiber cooperative bridging, 4-runtime mutual recursion (18 levels deep), Golden Thread verification, `pthread_atfork` fork guard, watchdog-driven preemption of infinite loops across all runtimes, foreign thread bridge calls (JVM threads → Python/JS/Ruby), concurrent multi-thread bridge contention, and nested foreign-thread cross-runtime chains.

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

ORM objects can't cross the bridge (everything is a string), so helpers should serialize their return values (JSON). This matches the typical Django view pattern where the output is already `JsonResponse`.

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
  test-manifests.sh    Manifest test suite runner (11 tests)
  test-cli.sh          CLI integration tests (27 tests)
runtime/
  java/              OmniVMRunner.java (in-memory compilation, file/jar/class execution)
examples/            Manifest JSON files and sample scripts
```

## Building & Testing

Requires Docker. The multi-stage Dockerfile handles all dependencies (Go, CPython, Node.js, JVM, Ruby). Unit tests run during the build. Integration tests run against the final image.

```bash
# Build (runs unit tests during build)
docker build -t omnivm .

# Run ALL tests against the built image
docker run --rm --entrypoint /bin/bash omnivm /omnivm/scripts/test-cli.sh  # 27 CLI tests
docker run --rm --entrypoint stresstest omnivm                             # 71 stress tests
docker run --rm --entrypoint manifest-runner omnivm /omnivm/examples/manifest-test.json

# REPL
docker run -it --rm omnivm
```

Always run the full test suite after a Docker build — unit tests in the build verify compilation, but CLI, stress, and manifest tests verify end-to-end behavior in the final image.

Make targets:

```bash
make build                # Build Docker image
make test-cli             # CLI integration tests (27 tests in Docker)
make test-manifests       # Run 11 manifest tests
make test-stress          # Run 71 stress tests
make test-all             # Everything: build + CLI + stress + manifests
```

## Key Design Decisions

- **Lazy runtime initialization**: Only the runtime needed for the target file is started. `omnivm run main.go` skips all embedded runtimes. `omnivm run script.py` only starts CPython.
- **Java file execution**: `omnivm run App.java` compiles in-memory via `javax.tools.JavaCompiler` and runs on the embedded JVM with real `main(String[] args)` and direct stdout/stderr. Supports `.class` and `.jar` files. Classpath auto-detects Maven (`target/dependency/`), Gradle (`build/libs/`), and `lib/`/`libs/` directories — downloaded JARs just work.
- **Go as equal peer**: Go files are compiled as plugins (`-buildmode=plugin`), loaded in-process, and executed — not via subprocess. `func main()` is transformed to an exported `func Main()` via the Go AST, compiled, and called via `plugin.Open`/`Lookup`. Go plugins can call other runtimes through the bridge (`OmniVM.Call("python", "...")`) and participate in the REPL and inline execution (`omnivm -go 'code'`). Go is the host because its runtime was the pickiest about embedding, not because it has special status.
- **Thread-safe bridge gateway**: Any thread can call `omnivm.call()` — not just the Golden Thread. Each runtime's entry point acquires the appropriate lock: `PyGILState_Ensure` (Python), `v8::Locker` (V8), `rb_thread_call_with_gvl` or proxy submit (Ruby), `AttachCurrentThreadAsDaemon` (JVM). Bridge hops release the source lock so no thread ever holds two runtime locks simultaneously — deadlock-free by construction.
- **Ruby proxy pthread**: Ruby is initialized on a dedicated pthread that doubles as the execution thread. All Ruby calls route through condvar-based dispatch to this pthread, which holds the GVL permanently. Ruby 3.3's M:N threading breaks `Thread.new` and `rb_thread_call_without_gvl` on non-main pthreads, so we avoid Ruby threads entirely — the pthread runs a simple request loop.
- **Epoll dispatcher (Linux)**: eventfd for task wakeup, timerfd for heartbeat, libuv backend fd for V8 I/O. Replaces the 1ms polling ticker with event-driven wakeups — zero CPU when idle.
- **C pthread watchdog**: Independent of the Go scheduler. `pthread_cond_timedwait` with `CLOCK_MONOTONIC` (immune to NTP jumps). Temporal signal routing dispatches runtime-specific interrupts: Python pipe write, `v8::Isolate::TerminateExecution()`, Ruby proxy `Thread#raise`.
- **Error enhancement**: Missing module errors get "pip install" / "npm install" / "gem install" hints. Python tracebacks are reformatted with `file:line` references. Go compile errors get "Did you mean?" suggestions.
- **Node.js over Duktape**: Duktape was ES5.1 — no `const`/`let`, no arrow functions, no `require()`, no npm. Node.js (via `libnode-dev` / `libnode127`) gives full ES2024+, the npm ecosystem, and built-in modules.
- **Skip `Py_FinalizeEx`, `ruby_cleanup()`, `V8::Dispose()`**: All crash in a polyglot process. Process exit reclaims resources.
- **`LD_PRELOAD=libjsig.so`**: JVM uses SIGSEGV for NullPointerException safepoints. Without signal chaining, this crashes Ruby. libjsig.so chains handlers properly.
- **`pthread_atfork` fork guard**: Child processes after `fork()` have dead JVM threads holding mutexes. The guard `_exit(71)`s with a diagnostic stack trace — both the C backtrace (via glibc `backtrace_symbols_fd`) and the Python traceback (via `faulthandler.dump_traceback`) are logged to stderr, identifying exactly which dependency triggered the fork. Python forced to `multiprocessing.set_start_method('spawn')`. The fork guard is **conditional** — it only fires when JVM or Ruby are loaded. Go+JS-only configurations are fork-safe when runtimes are initialized post-fork (the Gunicorn/Passenger pattern).
- **Python interpreter mode**: When symlinked as `python3`, OmniVM calls `Py_BytesMain()` — CPython's own entry point. `PyImport_AppendInittab("omnivm", ...)` registers the `omnivm` module before CPython initializes, so `import omnivm` works in any Python code. Best for single-process deployments (dev, `gunicorn --workers 1 --threads N`, uvicorn). Not compatible with prefork — Go's runtime doesn't survive `fork()`.
- **c-shared library mode (`libomnivm.so`)**: For prefork servers (Gunicorn, Passenger, uWSGI). Built with `go build -buildmode=c-shared`. All 5 runtimes are supported: JavaScript, Java, Ruby, Go (via dlopen plugins), and Python (host — cross-runtime bridge calls back into the already-running CPython). Full epoll dispatcher, watchdog, and cross-runtime bridge — feature parity with the main binary. The master process is pure CPython — no Go runtime loaded. Each worker calls `omnivm.init_runtimes()` post-fork, which `dlopen`s `libomnivm.so` and starts a fresh Go runtime. Both binaries share the `pkg/engine` package for runtime lifecycle, bridge wiring, watchdog setup, and shutdown — the `//export` C wrappers are thin. Go plugins must be built as `-buildmode=c-shared` (not `-buildmode=plugin`) and are loaded via `dlopen`/`dlsym`.
