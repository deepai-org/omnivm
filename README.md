# OmniVM

A single Go binary that embeds Python (CPython), JavaScript (Node.js/V8), Java (JVM/JNI), and Ruby (MRI) — all running on the same OS thread.

```
$ docker run --rm omnivm -python "print(omnivm.call('javascript', '2 + 2'))"
4
```

## Architecture

All four runtimes share a single OS thread (the **Golden Thread**), orchestrated by a dispatcher that serializes execution and pumps event loops. Cross-runtime calls happen synchronously on the same call stack — Python can call JS, JS can call Ruby, Ruby can call Java, and any combination in between.

```
Go main goroutine (runtime.LockOSThread)
  └─ Dispatcher loop (1ms tick)
       ├─ Python (CPython 3.12)
       ├─ JavaScript (Node.js 18 / V8, ES2024+)
       ├─ Java (JVM 21 / JNI, javax.tools.JavaCompiler)
       └─ Ruby (MRI 3.2)
```

Node.js is embedded via the C++ Embedder API with manual libuv pumping — `uv_run(loop, UV_RUN_NOWAIT)` every 1ms gives JavaScript cooperative CPU time without starving other runtimes. This means `require()`, npm packages, `setTimeout`, Promises, and the full Node.js API all work.

The bridge function `omnivm.call(runtime, code)` is available from every runtime:

```python
# Python calling JavaScript
result = omnivm.call("javascript", "Math.sqrt(144)")

# JavaScript calling Ruby
var result = omnivm.call("ruby", "('hello' + ' world').upcase");

# Java calling Python
String result = omnivm.OmniVM.call("python", "2 ** 100");
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
         "python":"3.12.3","ruby":"3.2.3","java":"21.0.10",
         "engine":"Node.js v18.19.1"}

--- GET /compute ---
Status: 200 OK
Body:   {"fibonacci_50":"12586269025","ruby_reverse":"MVinmO"}
```

The call stack for a single HTTP request:

```
Golden Thread (OS tid=1)
  └─ dispatcher.pumpAll()
       └─ jsRuntime.Pump()
            └─ omnivm_v8_pump_message_loop()   [acquires v8::Locker]
                 └─ uv_run(UV_RUN_NOWAIT)       [libuv fires Express callback]
                      └─ Express route handler   [V8 JavaScript]
                           ├─ omnivm.call('python', ...) → CPython
                           ├─ omnivm.call('ruby', ...)   → MRI Ruby
                           ├─ omnivm.call('java', ...)   → JVM/JNI
                           └─ res.json(...)              [V8 → libuv write]
```

Five runtimes (Go, V8/Node.js, CPython, MRI Ruby, JVM) on one OS thread. No inter-thread synchronization, no message passing — direct function calls up and down the same C stack.

## Quick Start

```bash
# Build
docker build -t omnivm .

# Interactive REPL
docker run -it --rm omnivm

# Execute code
docker run --rm omnivm -python "print('hello from Python')"
docker run --rm omnivm -js "console.log('hello from JS')"
docker run --rm omnivm -java 'System.out.println("hello from Java");'
docker run --rm omnivm -ruby "puts 'hello from Ruby'"

# Node.js features (ES2024+, require, npm)
docker run --rm omnivm -js "const x = [1,2,3].map(n => n*2); console.log(x)"
docker run --rm omnivm -js "console.log(require('path').join('a','b'))"
docker run --rm omnivm -js "console.log(require('crypto').randomUUID())"

# Run a script file
docker run --rm -v $(pwd)/examples:/scripts omnivm -file /scripts/hello.py
```

## REPL Commands

```
:python, :py       Switch to Python
:javascript, :js   Switch to JavaScript
:java, :jvm        Switch to Java
:ruby, :rb         Switch to Ruby
:status            Show runtime status
:quit, :q          Exit
```

## Cross-Runtime Calls

Every runtime can call any other runtime via `omnivm.call(runtime, code)`:

```python
# Python → JS → Ruby → Java chain
omnivm.call("javascript",
  "omnivm.call('ruby'," +
  "  'omnivm.OmniVM.call(\"java\", \"1 + 1\")')")
```

All calls execute synchronously on the Golden Thread. No marshalling, no IPC, no serialization — just direct function calls through C on a single OS thread.

## Stress Tests

52 tests verify correctness under pressure:

```bash
docker run --rm --entrypoint stresstest omnivm
```

Tests cover cross-runtime stack mixing, generators across C boundaries, asyncio pumping with bridge callbacks, re-entrant calls (Python → JS → Python), signal handling (JVM SIGSEGV + Ruby + Python interrupts), GC interaction, 1MB string round-trips, Ruby Fiber cooperative bridging, 4-runtime mutual recursion (18 levels deep), a Golden Thread proof that verifies all runtimes report the same OS thread ID, and a `pthread_atfork` fork guard.

## Project Structure

```
cmd/
  omnivm/          Main binary (REPL + CLI)
  stresstest/      52-test stress suite
  express-demo/    Express + Python/Ruby/Java HTTP demo
  telephone/       Cross-runtime telephone game
pkg/
  python/          CPython embedding via cgo
  javascript/      Node.js/V8 embedding via cgo
  jvm/             JVM embedding via JNI/cgo
  ruby/            MRI Ruby embedding via cgo
  dispatcher/      Golden Thread task serializer (1ms tick)
  signals/         Signal handler management
  arrow/           Shared memory primitives
scripts/
  v8_bridge_node.cc  Node.js ↔ v8_bridge.h C++ adapter
runtime/
  java/            OmniVMRunner.java (in-memory compilation)
examples/          Sample scripts
```

## Key Design Decisions

- **Node.js over Duktape**: Duktape was ES5.1 — no `const`/`let`, no arrow functions, no `require()`, no npm. Node.js (via `libnode-dev`) gives full ES2024+, the npm ecosystem, and built-in modules, at the cost of ~50MB more in the image.
- **Manual libuv pump**: No `node::SpinEventLoop()`. We extract the `uv_loop_t` and tick it with `uv_run(UV_RUN_NOWAIT)` + `platform->DrainTasks()` every 1ms, giving Node.js cooperative CPU time without blocking the Golden Thread.
- **`v8::Locker` everywhere**: `MultiIsolatePlatform` spawns a V8 background thread for GC/compilation. Every entry point (execute, eval, pump) acquires the Locker to serialize access.
- **`node::CallbackScope`**: Required in every code evaluation and pump cycle so `process.nextTick` callbacks drain properly. Without it, Promises silently stall.
- **`Py_InitializeEx(0)`**: Skips Python signal handler registration so Go owns signals. Interrupt delivery uses a pipe-based mechanism instead.
- **`LD_PRELOAD=libjsig.so`**: JVM uses SIGSEGV for NullPointerException safepoints. Without signal chaining, this crashes Ruby. libjsig.so chains handlers properly.
- **Skip `ruby_cleanup()` and `V8::Dispose()`**: Both crash in a polyglot process (Ruby sends signals to JVM threads; V8 teardown has wrong init ordering). Process exit reclaims resources.
- **`pthread_atfork` fork guard**: Child processes after `fork()` have dead JVM threads holding mutexes. The guard `_exit(71)`s with a diagnostic message.
- **`javax.tools.JavaCompiler`**: Nashorn was removed in Java 15+. OmniVMRunner compiles Java source in-memory via the JDK compiler API.

## Building

Requires Docker. The multi-stage Dockerfile handles all dependencies:

```bash
docker build -t omnivm .
docker run --rm --entrypoint stresstest omnivm    # 52/52 tests
docker run --rm --entrypoint express-demo omnivm  # Express demo
docker run -it --rm omnivm                        # REPL
```
