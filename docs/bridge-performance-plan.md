# Bridge Performance Plan: Automatic Handles, Arrow Data, Native C-ABI

## Current State

Many current cross-runtime values still go through:
```
Source value → toString()/JSON.stringify → char* (malloc) → C.GoString → Go string → C.CString → target runtime parses string
```

This is correct but ~1000x slower than a direct C function call for hot paths.
It also loses object identity for complex values. The plan adds progressively
better bridge paths **without removing the existing one**. JSON becomes the
diagnosed fallback when typed values, handles, streams, and Arrow data do not
apply.

Normal `.poly` source should not expose this machinery. The compiler/runtime
chooses the crossing path automatically from value kind and use shape.

The plan intentionally does not add library-specific fast paths. Popular
ecosystem libraries should work because their values expose ordinary host
protocols: reflection/property/call surfaces for objects, iterator/body
protocols for streams, buffer/Arrow/dataframe protocols for bulk data, and host
finalizers/scope cleanup for lifetime. If two values expose the same protocol
shape, OmniVM should choose the same crossing mode regardless of the package
that created them.

## Implementation Boundary

This plan is partly implemented and partly architectural direction.

Implemented now:

- scoped runtime handles with status counters, finalizer queues, retained-handle
  diagnostics, reference-edge diagnostics, and cycle samples;
- generic manifest proxy descriptors for complex objects, callbacks, functions,
  request-shaped values, model-shaped values, collections, maps, sets, and
  runtime-owned objects;
- generic Arrow/shared-buffer diagnostics for buffer protocol, `__array_interface__`,
  dataframe interchange, Arrow capsule/stream, JS typed arrays, Ruby binary
  strings, Java buffers/primitive arrays, and Go c-shared slices;
- lazy stream descriptors for channels, iterators, generators, readers, and
  body-shaped values;
- bounded repeated-item materialization and warning counters for chatty proxies.

Future work:

- compiler-driven loop rewriting and adapter-wide batching for arbitrary
  property/index/method access patterns;
- complete Arrow C Data Interface coverage for multi-buffer, nested, chunked,
  dictionary, and string-offset table shapes across all runtimes;
- distributed cycle collection for long-lived cross-runtime object graphs.

---

## Tier 1: Automatic Object Handles

Complex values cross as scoped runtime-owned handles when identity, laziness,
mutation, or native ownership matters. This is selected from generic protocol
evidence such as callability, mutability, stream/body interfaces, close/finalize
semantics, buffer export support, and stable identity. The rule is deliberately
not a catalog of library names.

### Handle table

```go
type HandleID uint64

type HandleEntry struct {
    ID          HandleID
    Runtime     string
    Kind        string
    TypeHint    string
    ScopeID     uint64
    StrongRefs  int64
    Finalizers  int64
    Accesses    int64
    Value       any
    Release     func() error
    CreatedAt   time.Time
    LastAccess  time.Time
}
```

The handle table is process-local in libomnivm and Go-hosted OmniVM. Handles are
not portable across processes.

### Lifetime model

- Handles are scope-bound by default. Request/manifest cleanup is the
  deterministic release path.
- Guest proxies register finalizers:
  - JS: `FinalizationRegistry`
  - Python: `weakref.finalize`
  - Java: `Cleaner`
  - Ruby: finalizers where safe, scope cleanup otherwise
  - Go: runtime finalizers for proxy wrappers
- Finalizers enqueue release events; they must not call into Go/runtime internals
  directly from arbitrary GC threads.
- Release is idempotent.
- Explicit release/dispose remains an internal/debug escape hatch, not a normal
  `.poly` API.

### Cross-runtime cycles

Distributed cycle collection is not phase-one scope. Instead:

- request/manifest scopes release non-retained handles at scope end;
- retained handle graphs are observable through status diagnostics;
- oldest-live-handle and handles-by-origin counters make leaks visible;
- generic handle access counters make repeated proxy traffic visible by access
  kind before adapter-level batching lands;
- compiler/runtime escape evidence is required before a handle outlives its
  creating scope.

### Chatty proxy mitigation

Proxies preserve identity but can hide FFI costs. The runtime must detect:

- repeated index access;
- repeated property reads;
- `len(proxy)` followed by indexed loops;
- map/filter/reduce over foreign collections.

Mitigations are automatic:

- batch reads when stable;
- materialize small immutable values;
- switch array-like values to Arrow;
- stream large iterables;
- warn when no safe optimization exists.

The first concrete layer is generic telemetry in the central handle table:
runtime adapters record access kinds such as property/index/call/iterate/buffer,
and `omnivm.status()["handles"]` reports total accesses, accesses by kind,
chatty warning counts, chatty origins, and the chattiest live handle. Runtime
adapters can also record generic handle-reference edges, which lets status
report live graph edges, runtime pairs, suspected cycles, cyclic handle counts,
and a sample cycle. Manifest boundary status separately reports automatic proxy
materializations so batched chatty-proxy mitigation is observable without a
user-facing API. This keeps the optimization and leak-diagnostic triggers
protocol-shaped rather than tied to particular libraries.

---

## Tier 2: Arrow C Data Interface

**Builds on:** existing `pkg/arrow/arrow.go`, but replaces the long-term
OmniVM-specific buffer contract with Arrow C Data Interface handles.

### C-level data types (`omni_bridge.h`)

```c
typedef struct {
    const char* format;
    const char* name;
    const char* metadata;
    int64_t flags;
    int64_t n_children;
    struct ArrowSchema** children;
    struct ArrowSchema* dictionary;
    void (*release)(struct ArrowSchema*);
    void* private_data;
} ArrowSchema;

typedef struct {
    int64_t length;
    int64_t null_count;
    int64_t offset;
    int64_t n_buffers;
    int64_t n_children;
    const void** buffers;
    struct ArrowArray** children;
    struct ArrowArray* dictionary;
    void (*release)(struct ArrowArray*);
    void* private_data;
} ArrowArray;
```

Simple bytes and typed arrays are represented as one-buffer Arrow arrays. Bulk
tabular, image-like, tensor-like, and nested values use normal Arrow schemas,
child arrays, shape metadata, and release callbacks when they expose the
required protocols.

The current shared store records generic metadata slots for dtype, Arrow format,
shape, strides, null count, read-only state, and ownership. Status reports live
buffers, bytes, formats, copied bytes, and zero-copy borrows so runtime adapters
can prove when a bulk crossing stayed on the shared-memory path and when it
fell back to a copy.

### Per-runtime Arrow access

| Runtime | Zero-copy target | Mechanism |
|---------|------------------|-----------|
| Python | buffer/Arrow/DLPack/dataframe interchange exporters, `memoryview` | Arrow C Data Interface / buffer protocol |
| JS/V8 | `ArrayBuffer`, TypedArray, `DataView`, Arrow-compatible vectors | external backing stores plus schema metadata |
| Ruby | `to_io`, `each`, frozen/binary strings, Fiddle-backed views | borrowed pointer with scope pinning |
| Java | `ByteBuffer`, `DirectByteBuffer`, Arrow-compatible vectors | C Data import / `NewDirectByteBuffer` |
| Go | slices, `io.Reader`/`io.Writer`, Arrow-compatible values | cgo pointer with release callback |

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

### Wire Arrow bridge op

The manifest compiler emits Arrow/share-memory bridge intent and OmniVM skips
serialization entirely, passing Arrow schema/array handles through the data path.
The user still writes ordinary `.poly`; this is a lowered boundary decision.
Manifest table handles carry generic metadata (`dtype`, `arrow_format`, shape,
strides, null count, and read-only state) so target runtimes can import the view
without guessing from runtime-specific objects.

---

## Tier 3: Native C-ABI Bridge (Typed Values)

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
    ScopeID uint64
    Kind    string
}
```

The native value bridge carries handle ids for complex values, but lifecycle is
owned by Tier 1. Handles are scope-bound and finalizer-backed by default, not
weak by default.

### PolyScript integration

When the type system knows both sides are scalar types, emit a `typed_call`
bridge op instead of `serialize`/`deserialize`. The manifest executor checks for
`call_typed` availability and falls back only with diagnostics if not present.

---

## Tier 4: Compile-Time Binding (Optional)

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
to call back into a host-owned Python object, it goes through `call_typed`.
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
Phase 1 (Tier 1): Object handles and scopes
   ├─ Central HandleTable with scope ids and release callbacks
   ├─ Python and JS proxy wrappers first
   ├─ Finalizer release queue drained on safe OmniVM threads
   ├─ Request/manifest scope cleanup
   ├─ Status counters for live handles, origins, retained handles, releases
   └─ Tests: identity, scope release, finalizer release, cycle diagnostics

Phase 2 (Tier 2): Arrow data plane
   ├─ Add ArrowSchema/ArrowArray C definitions
   ├─ Adapt existing SharedStore around Arrow handles
   ├─ Borrowed buffer auto-pinning per runtime
   ├─ Wire Arrow/share_memory bridge op in manifest executor
   └─ Tests: generic buffer/Arrow/dataframe/typed-array zero-copy where supported

Phase 3 (Tier 3): Typed value bridge
   ├─ Define omni_value_t with alignment guarantees
   ├─ Implement call_typed in OmniCall
   ├─ Python typed extraction (PyLong/PyFloat direct)
   ├─ V8 typed extraction (v8::Integer/Number direct)
   ├─ PolyScript emits typed bridge ops when types are known
   └─ Tests: scalar hot paths, fallback diagnostics

Phase 4 (Tier 4): Compile-time binding
   ├─ polybind code generator
   ├─ CPython extension module generation
   ├─ Rust FFI wrapper generation
   ├─ Integration with exec_compiled
   └─ Tests: ordinary framework handler calling compiled native function
```

## What Stays the Same

- The JSON string bridge (`omni_call_fn`) remains available as a compatibility
  fallback
- The manifest executor works unchanged — bridge ops apply at the same injection points
- The Golden Thread / dispatcher architecture is untouched
- The watchdog / timeout system works across all tiers
- PolyScript's type system and runtime adapters drive which tier to use:
  primitives → typed/copy, complex objects → handles, bulk data → Arrow,
  streams → stream handles, unsupported values → diagnosed fallback

Each tier is additive. Handles can ship before Arrow. Arrow can ship before
compile-time binding. The JSON path remains available for compatibility, but it
is not the target architecture for complex or bulk values.
