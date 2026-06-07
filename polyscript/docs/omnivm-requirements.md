# OmniVM Requirements for PolyScript

This document specifies what OmniVM must support for the PolyScript code generator to work end-to-end. It incorporates feedback from the OmniVM team on feasibility, priorities, and pushback items.

PolyScript's pipeline: `Source → Lexer → Parser → AST → Runtime Resolver → Code Generator → Dispatch Manifest`.

The code generator emits a **dispatch manifest** — a structured JSON IR that OmniVM interprets directly. **No language is "on top."** OmniVM is the orchestrator.

---

## 1. Dispatch Manifest Format

The code generator outputs a JSON document. OmniVM reads it and dispatches each operation to the appropriate runtime via the existing `pkg.Runtime` interface.

```json
{
  "version": 1,
  "defaultRuntime": "javascript",
  "ops": [
    { "op": "import", "runtime": "python", "path": "os", "bind": "os" },
    { "op": "eval", "runtime": "python", "code": "os.listdir('.')", "bind": "files" },
    { "op": "exec", "runtime": "javascript", "code": "console.log(files)", "captures": { "files": "files" } }
  ]
}
```

### Operation Types

| Op | Purpose | OmniVM Mapping | Effort |
|----|---------|----------------|--------|
| `exec` | Execute code, discard result | `runtime.Execute(code)` | Already have |
| `eval` | Execute code, bind result | `runtime.Eval(code)` → binding table | Already have |
| `declare` | Declare a variable in manifest scope | Store in `map[string]interface{}` | ~50 LOC |
| `assign` | Reassign an existing variable | Update binding table | ~20 LOC |
| `func_def` | Define a callable function | Store closure (body ops + scope) | ~100 LOC |
| `return` | Return from function | Sentinel error to unwind | ~30 LOC |
| `if` | Conditional branching | Eval test, pick branch | ~50 LOC |
| `loop` | While/for/infinite loops | Eval test, execute body, repeat | ~50 LOC |
| `concat` | Polyglot string interpolation | Iterate segments, eval, join | Trivial |
| `import` | Import a module in a runtime | `runtime.Execute("import ...")` | Already have |
| `native` | Pass-through code | `runtime.Execute(code)` | Already have |
| `parallel` | Cooperative concurrent execution | See Section 8 | Real work |
| `exec_compiled` | Compile Rust/C, execute | See Section 9 | Aspirational |
| `eval_compiled` | Compile Rust/C, bind result | See Section 9 | Aspirational |

---

## 2. Supported Runtimes

| Runtime | ID | Status | Notes |
|---------|-----|--------|-------|
| Python (CPython) | `"python"` | Full support | Async-capable (asyncio) |
| JavaScript (V8) | `"javascript"` | Full support | Async-capable (Promises) |
| Ruby (MRI) | `"ruby"` | Full support | Synchronous only |
| Java (JVM) | `"java"` | Full support | Synchronous only |
| Go | `"go"` | Limited | See Section 10 — pre-compiled functions only |
| Rust | `"rust"` | Aspirational | See Section 9 |
| C | `"c"` | Aspirational | See Section 9 |

---

## 3. Variable Binding Table + Captures Injection

**This is the biggest new piece.** Today cross-runtime data flows only as strings. The manifest needs:

- A `map[string]interface{}` binding table in Go
- Per-runtime injection before exec/eval
- Per-runtime extraction after eval

### Injection per runtime

| Runtime | Injection mechanism |
|---------|-------------------|
| Python | `PyObject_SetAttrString` on `__main__` module (or locals dict with `exec(code, globals, locals_dict)`) |
| JavaScript | `globalThis.varName = value` |
| Ruby | Global `$varName` or `binding.eval` |
| Java | `OmniVMRunner` constructor args |

### Manifest captures format

```json
{
  "op": "exec",
  "runtime": "python",
  "code": "process(x, count)",
  "captures": { "x": "x", "count": "total_count" }
}
```

Keys = names in the target runtime's namespace. Values = binding names in the manifest scope. OmniVM looks up each value in its binding table, marshals it into the target runtime, and sets it as a variable before executing the code.

### Scope isolation (P2)

Captures should ideally not leak as globals between calls:
- Python: use `exec(code, globals, locals_dict)` instead of `PyRun_SimpleString`
- JS: wrap in an IIFE
- Ruby: `binding.eval`
- Java: constructor args (already scoped)

This is deferred to P2 as the OmniVM team recommends.

---

## 4. Boundary Values Beyond Strings

The manifest boundary is no longer a string-only or JSON-only channel.
Primitive values can still copy directly, but library objects, lazy data,
streams, callbacks, and native memory must lower to the first-class boundary
forms OmniVM executes today: `copy`, `ref`, `stream`, `table`/Arrow,
`resource`, `job`, and `proxy_callable`.

### On `eval` (runtime → binding table)

Convert the runtime's native value to `interface{}` or a retained handle:
- primitives use runtime-native accessors, not string parsing;
- JSON-compatible literals copy only when small and ownership-free;
- framework, ORM, request, session, and transaction objects stay live resource
  proxies;
- iterators, result sets, response bodies, and readers stay lazy stream proxies
  with explicit EOF/cancel release;
- Arrow-compatible dataframes, buffers, typed arrays, and ByteBuffers export as
  table/Arrow descriptors when layout, ownership, and CPU addressability are
  proven;
- functions and closures retain as callable proxies with captured shape
  metadata when needed.

### On inject (binding table → runtime)

Convert `interface{}` to the runtime's native type:
- Python: `PyLong_FromLong`, `PyUnicode_FromString`, etc.
- JS: `v8::Number::New`, `v8::String::NewFromUtf8`, etc.
- Ruby: `rb_int_new`, `rb_str_new`, etc.
- Java: JNI `NewObject`, etc.

### Type mapping

| Manifest Type | Python | JavaScript | Ruby | Java |
|--------------|--------|------------|------|------|
| number (int) | `int` | `number` | `Integer` | `long` |
| number (float) | `float` | `number` | `Float` | `double` |
| string | `str` | `string` | `String` | `String` |
| boolean | `bool` | `boolean` | `true/false` | `boolean` |
| null | `None` | `null` | `nil` | `null` |
| array | `list` | `Array` | `Array` | `List<?>` |
| object | `dict` | `Object` | `Hash` | `Map<?,?>` |

JSON fallback is a diagnostic, not the main design. Garbage should emit bridge
metadata that lets OmniVM choose the most natural boundary automatically rather
than forcing users to annotate common framework and library values.

---

## 5. Control Flow — Tree-Walking Interpreter

The manifest's `if`, `loop`, `func_def`, and `return` ops require OmniVM to act as a tree-walking interpreter over the op list. Estimated ~500 lines of Go.

### `if`

```json
{
  "op": "if",
  "arms": [{ "test": { "kind": "expr", "runtime": "python", "code": "len(items) > 0" }, "body": [...] }],
  "elseBody": [...]
}
```

Eval the test condition in its runtime, truthy-check the result, execute the chosen branch's body ops.

### `loop`

```json
{ "op": "loop", "mode": "while", "test": { "kind": "expr", "runtime": "javascript", "code": "i < 10" }, "body": [...] }
```

Eval test, execute body, repeat. Modes: `while`, `for`, `infinite`, `foreach`.

### `func_def`

```json
{ "op": "func_def", "name": "greet", "params": [{ "name": "name" }], "bodyRuntime": "python", "body": [...] }
```

Store as a closure (body ops + captured scope) in the binding table. When called, create a new scope, bind params, execute body ops. If `bodyRuntime` is set, the entire body is a single runtime dispatch.

### `return`

```json
{ "op": "return", "from": { "op": "eval", "runtime": "python", "code": "result", "bind": "__ret" } }
```

Use a sentinel error or special value to unwind the op stack back to the enclosing `func_def`.

### `concat`

```json
{ "op": "concat", "bind": "msg", "segments": [{ "kind": "text", "value": "User: " }, { "kind": "eval", "runtime": "python", "code": "db.get_user().name" }] }
```

Iterate segments, eval each one, `strings.Join`. Trivial.

---

## 6. Runtime State Persistence

**Already works.** Each runtime maintains a single persistent interpreter. Python's `sys.modules`, JS globals, Ruby loaded gems, Java classloader state all persist across ops within a manifest execution.

---

## 7. Error Propagation

**Already works.** `Result.Err` surfaces structured errors. Requirements:

- Python `Exception` → structured error with traceback
- JS `Error` → structured error with stack
- Ruby `Exception` → structured error
- Java `Exception` → structured error with stack trace
- Syntax errors in code strings → structured error (not silent failure)

---

## 8. Parallel — Cooperative Concurrency

Per OmniVM team recommendation: **no true thread parallelism**. The Golden Thread model remains.

```json
{
  "op": "parallel",
  "branches": [
    { "runtime": "python", "code": "fetch_data()", "bind": "pyResult" },
    { "runtime": "javascript", "code": "fetch('/api')", "bind": "jsResult" },
    { "runtime": "ruby", "code": "compute()", "bind": "rbResult" }
  ]
}
```

**Recommended implementation**: "Start all async-capable branches, pump until all complete, run synchronous branches sequentially."

- **Python asyncio**: wrap in `asyncio.ensure_future()`, pump until done
- **JS Promises**: wrap in Promise resolution, pump until done
- **Ruby/Java**: run sequentially (they block the Golden Thread)

This is cooperative concurrency — the dispatcher interleaves async-capable runtimes across pump cycles. It's not parallelism, but it achieves the spirit for I/O-bound workloads.

No generic `async: true` flag on individual ops. Async behavior is runtime-specific and handled by the `parallel` op dispatcher.

---

## 9. Compiled Targets — Aspirational (P3)

**Per OmniVM team pushback**: Compiling arbitrary Rust/C at runtime is a massive security and complexity surface. This is deferred to P3/aspirational.

The manifest types define `exec_compiled` and `eval_compiled` for forward compatibility:

```json
{ "op": "exec_compiled", "lang": "rust", "code": "fn compute(x: i32) -> i32 { x * 2 }" }
```

If implemented, the path is: `rustc`/`gcc` → `.so` → `dlopen` → `dlsym`, with serious sandboxing.

---

## 10. Go as a Dispatch Target — Limited Scope

**Per OmniVM team pushback**: Go is the host, not a guest. Running arbitrary Go code at runtime requires a Go interpreter (Yaegi — limited) or plugin compilation (fragile).

**Scoped to**: Pre-compiled Go functions registered with the manifest, not arbitrary Go code strings. The PolyScript code generator can still resolve nodes as "Go runtime" but the manifest should either:
- Emit a `native` op that calls a registered Go function by name
- Fall back to a different runtime for truly arbitrary code

---

## 11. Callable Boundaries

OmniVM supports callable proxies, but Garbage should still be explicit about
shape. A Python function or JavaScript closure can cross as a callable handle;
keyword arguments, Java record/builders, and JavaScript options objects need
shape metadata so OmniVM can adapt calls without guessing.

```json
[
  { "op": "eval", "runtime": "python", "code": "lambda x: x * 2", "bind": "py_double" },
  { "op": "eval", "runtime": "javascript", "code": "py_double(21)", "captures": { "py_double": "py_double" }, "bind": "result" }
]
```

The second op crosses `py_double` as a callable proxy. Garbage should attach
callable-shape metadata when it knows argument names, destructured keys, keyword
support, or Java adapter forms. If the shape is unknown, OmniVM should reject
ambiguous keyword/options calls with a diagnostic instead of guessing.

---

## 12. Summary of Requirements

| Requirement | Priority | Status | Effort |
|-------------|----------|--------|--------|
| `exec` / `eval` ops | P0 | Have primitives | ~200 LOC (binding table) |
| Variable binding table | P0 | New | ~200 LOC |
| Captures injection (per-runtime) | P0 | New | Touches each cgo layer |
| Boundary value marshalling | P0 | Implemented in OmniVM; compiler emits hints | Keep bridge metadata aligned |
| Error propagation | P0 | Already works | — |
| Runtime state persistence | P0 | Already works | — |
| Multi-statement code strings | P0 | Already works | — |
| `import` / `native` ops | P0 | Trivial | ~50 LOC |
| `func_def` + control flow | P1 | New | ~500 LOC (interpreter) |
| `concat` op | P1 | New | Trivial |
| `parallel` (cooperative) | P1 | New | Real work |
| Scope isolation for captures | P2 | New | Per-runtime scoping |
| Go as dispatch target | P2 | Limited | Pre-compiled functions only |
| Compiled targets (Rust/C) | P3 | Aspirational | Major security surface |
| Callable proxy marshalling | P1 | Supported with shape metadata | Avoid ambiguous shapes |
