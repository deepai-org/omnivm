# Bridge Performance Plan: Zero-Copy, Native C-ABI, Compile-Time Binding

## Current State

Every cross-runtime value goes through:
```
Source value → toString()/JSON.stringify → char* (malloc) → C.GoString → Go string → C.CString → target runtime parses string
```

This is correct but ~1000x slower than a direct C function call for hot paths.
The plan adds three tiers of progressively faster bridge paths **without removing
the existing one** — the JSON path becomes the fallback when the others don't apply.

---

## Tier 1: Zero-Copy Buffer Passing

**Builds on:** existing `pkg/arrow/arrow.go` (SharedStore, Buffer, ref counting)

### New C-level buffer type (`omni_bridge.h`)

```c
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;  // 0=bytes, 1=i32, 2=f64, 3=utf8, ...
    int8_t  owned;  // 1=receiver owns (must free), 0=borrowed
} omni_buffer_t;

typedef char* (*omni_call_buf_fn)(const char* runtime, const char* code,
                                   omni_buffer_t* bufs, int32_t nbuf);
```

### Per-runtime buffer access

| Runtime | Zero-copy API | Mechanism |
|---------|--------------|-----------|
| Python | `omnivm.get_buffer("name")` → `memoryview` | `PyMemoryView_FromMemory` |
| JS/V8 | `omnivm.getBuffer("name")` → `ArrayBuffer` | `v8::ArrayBuffer::NewBackingStore` (no-op deleter) |
| Ruby | `omnivm.get_buffer("name")` → frozen `String` | `rb_str_new_static` |
| Java | `omnivm.getBuffer("name")` → `ByteBuffer` | `NewDirectByteBuffer` |

### Constraints

- **Finalizer thread safety:** Runtime GC finalizers (JS `FinalizationRegistry`,
  Java `Cleaner`, Python `weakref`) run on background GC threads. `Buffer.Release()`
  must NOT call Go directly from these threads. Instead, write to an eventfd/pipe
  that the Golden Thread drains on the next pump cycle.

- **Borrowed buffer pinning:** When `owned=0`, the source runtime must prevent GC
  of the underlying object for the duration of the call:
  - Python: `Py_INCREF` before, `Py_DECREF` after
  - V8: `v8::Persistent` handle
  - Ruby: `RB_GC_GUARD`
  - Java: `NewGlobalRef`
  Automatic in the per-runtime `get_buffer` implementation, not caller's responsibility.

### Wire share_memory bridge op

The manifest compiler emits `share_memory{ownership: "borrowed"}` and OmniVM
skips serialization entirely, passing pointer+length through the buffer path.

---

## Tier 2: Native C-ABI Bridge (Typed Values)

### Tagged value type

```c
typedef struct {
    int64_t tag;  // 0=null, 1=bool, 2=i64, 3=f64, 4=string, 5=bytes, 6=object_ref
    union {
        int64_t  i;
        double   f;
        struct { char* ptr; int64_t len; } s;
        struct { void* ptr; int64_t len; int32_t dtype; } buf;
        uint64_t ref;  // opaque handle for complex objects
    } v;
} omni_value_t;
```

**Alignment:** Use `__attribute__((aligned(8)))` in C, `#[repr(C)]` in Rust.
Static assert `sizeof(omni_value_t)` at compile time in each language. Tag is
`int64_t` (not `int8_t`) so the union is always at offset 8, naturally aligned.

### New bridge signature (coexists with existing)

```c
typedef omni_value_t (*omni_call_typed_fn)(
    const char* runtime,
    const char* func_name,
    omni_value_t* args,
    int32_t nargs
);

struct omni_bridge {
    omni_call_fn       call;        // existing: string → string
    omni_free_fn       free;        // existing: free string
    omni_call_typed_fn call_typed;  // new: value → value
    omni_call_buf_fn   call_buf;    // new: with buffers
};
```

Each runtime checks: if `call_typed` is set, use it for function calls with
known signatures. Otherwise fall back to `call`. Old code keeps working.

### Per-runtime typed extraction

| Runtime | i64 | f64 | string |
|---------|-----|-----|--------|
| Python | `PyLong_FromLongLong` / `PyLong_AsLongLong` | `PyFloat_FromDouble` / `PyFloat_AsDouble` | `PyUnicode_FromStringAndSize` |
| V8 | `v8::Integer::New` / `Int32Value` | `v8::Number::New` / `NumberValue` | `v8::String::NewFromUtf8` |
| Ruby | `LONG2FIX` / `FIX2LONG` | `DBL2NUM` / `NUM2DBL` | `rb_str_new` |
| Java | `CallLongMethod` | `CallDoubleMethod` | `NewStringUTF` |

### Object handle table

```go
type HandleTable struct {
    mu      sync.RWMutex
    handles map[uint64]HandleEntry
    next    uint64
}

type HandleEntry struct {
    Value   interface{}
    Runtime string
    Weak    bool  // if true, source runtime can GC the object
}
```

**Cross-runtime GC cycles:** If Python holds a handle to a JS object and JS holds
a handle to a Python object, neither GC can see the other's roots → memory leak.

**Policy for Tier 2:** Handles are weak by default (dereference returns null/error
if source GC'd). Strong handles are opt-in and require explicit `Release()`.
Distributed cycle collection is out of scope — document the limitation.

### PolyScript integration

When the type system knows both sides are scalar types, emit a `typed_call` bridge
op instead of `serialize`/`deserialize`. The manifest executor checks for `call_typed`
availability and falls back to JSON if not present.

---

## Tier 3: Compile-Time Binding (Optional)

### `polybind` code generator

```
polybind generate --from rust --to python src/math.rs
```

Produces:
- `math_py.c` — CPython extension module with `PyMethodDef` entries
- `math_py.rs` — Rust FFI wrappers
- `Makefile` — compiles to `math.cpython-314-x86_64-linux-gnu.so`

### PolyScript drives codegen

The compiler already knows types. For:
```
fn fast_compute(data: Array<f64>) -> f64:
    // Rust body
```

It emits either:
- **Manifest mode** (existing): `{"op": "eval", "runtime": "rust", ...}` with bridge ops
- **Compiled mode** (new): generates a `.so` that Python imports directly

### Callback escape hatch

The compiled binding uses `omni_value_t` internally. If the compiled module needs
to call back into Python (e.g., Django ORM), it goes through `call_typed`.
PyO3-speed for the hot path, bridge-speed for callbacks.

### Extends existing `exec_compiled`

`pkg/manifest/compiled.go` already compiles C and Rust to `.so`. Extend to generate
typed entry points instead of just `run()`:
```
dlsym("fast_compute") → call with omni_value_t args
```

---

## Implementation Order

```
Phase 1 (Tier 1): Zero-copy buffers
   ├─ Extend omni_bridge.h with omni_buffer_t
   ├─ SharedStore ↔ runtime bindings (Python memoryview, V8 ArrayBuffer)
   ├─ Eventfd-based release hook (not direct Go call from GC threads)
   ├─ Borrowed buffer auto-pinning per runtime
   ├─ Wire share_memory bridge op in manifest executor
   └─ Tests: pass 1MB buffer Python→JS without copy

Phase 2 (Tier 2): Typed value bridge
   ├─ Define omni_value_t with alignment guarantees
   ├─ Implement call_typed in OmniCall
   ├─ Python typed extraction (PyLong/PyFloat direct)
   ├─ V8 typed extraction (v8::Integer/Number direct)
   ├─ Object handle table (weak by default)
   ├─ PolyScript emits typed bridge ops when types are known
   └─ Tests: 1M scalar calls Python→Rust, benchmark vs JSON path

Phase 3 (Tier 3): Compile-time binding
   ├─ polybind code generator
   ├─ CPython extension module generation
   ├─ Rust FFI wrapper generation
   ├─ Integration with exec_compiled
   └─ Tests: Django view calling compiled Rust function
```

## What Stays the Same

- The JSON string bridge (`omni_call_fn`) remains the **default and fallback**
- The manifest executor works unchanged — bridge ops apply at the same injection points
- The Golden Thread / dispatcher architecture is untouched
- The watchdog / timeout system works across all tiers
- PolyScript's type system drives which tier to use:
  unknown types → JSON, known scalars → typed, annotated hot paths → compiled

Each tier is additive. You can use zero-copy buffers without typed calls.
You can use typed calls without compile-time binding. And you can always
fall back to the JSON path that works today.
