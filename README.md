# OmniVM

A single Go binary that embeds Python (CPython), JavaScript (Duktape), Java (JVM/JNI), and Ruby (MRI) — all running on the same OS thread.

```
$ docker run --rm omnivm -python "print(omnivm.call('javascript', '2 + 2'))"
4
```

## Architecture

All four runtimes share a single OS thread (the **Golden Thread**), orchestrated by a dispatcher that serializes execution and pumps event loops. Cross-runtime calls happen synchronously on the same call stack — Python can call JS, JS can call Ruby, Ruby can call Java, and any combination in between.

```
Go main goroutine (runtime.LockOSThread)
  └─ Dispatcher loop
       ├─ Python (CPython, Py_InitializeEx)
       ├─ JavaScript (Duktape, ES5.1+)
       ├─ Java (JVM/JNI, javax.tools.JavaCompiler)
       └─ Ruby (MRI, rb_eval_string)
```

The bridge function `omnivm.call(runtime, code)` is available from every runtime:

```python
# Python calling JavaScript
result = omnivm.call("javascript", "Math.sqrt(144)")

# JavaScript calling Ruby
var result = omnivm.call("ruby", "('hello' + ' world').upcase");

# Java calling Python
String result = omnivm.OmniVM.call("python", "2 ** 100");
```

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

43 tests verify correctness under pressure:

```bash
docker run --rm --entrypoint stresstest omnivm
```

Tests cover cross-runtime stack mixing, generators across C boundaries, asyncio pumping with bridge callbacks, re-entrant calls (Python → JS → Python), signal handling (JVM SIGSEGV + Ruby + Python interrupts), GC interaction, 1MB string round-trips, and a Golden Thread proof that verifies all runtimes report the same OS thread ID.

## Project Structure

```
cmd/
  omnivm/        Main binary (REPL + CLI)
  stresstest/    43-test stress suite
pkg/
  python/        CPython embedding via cgo
  javascript/    Duktape embedding via cgo
  jvm/           JVM embedding via JNI/cgo
  ruby/          MRI Ruby embedding via cgo
  dispatcher/    Golden Thread task serializer
  signals/       Signal handler management
  arrow/         Shared memory primitives
runtime/
  java/          OmniVMRunner.java (in-memory compilation)
examples/        Sample scripts
```

## Key Design Decisions

- **Duktape over V8**: V8 is painful to build from source. Duktape is a single C file, ES5.1+ compliant, and implements the same bridge API.
- **`Py_InitializeEx(0)`**: Skips Python signal handler registration so Go owns signals. Interrupt delivery uses a pipe-based mechanism instead.
- **`LD_PRELOAD=libjsig.so`**: JVM uses SIGSEGV for NullPointerException safepoints. Without signal chaining, this crashes Ruby. libjsig.so chains handlers properly.
- **Skip `ruby_cleanup()`**: Ruby's cleanup sends signals to threads, crashing when JVM threads exist. Process exit reclaims resources.
- **`javax.tools.JavaCompiler`**: Nashorn was removed in Java 15+. OmniVMRunner compiles Java source in-memory via the JDK compiler API.

## Building

Requires Docker. The multi-stage Dockerfile handles all dependencies:

```bash
make build    # Build the Docker image
make test     # Run local + Docker tests
make run      # Start the REPL
```
