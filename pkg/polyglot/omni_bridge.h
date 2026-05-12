// omni_bridge.h — Cross-runtime bridge function pointer types.
//
// Each runtime includes this header and stores the callback pointers
// set via its SetBridgeCallback(). No import cycles.
#ifndef OMNIVM_BRIDGE_H
#define OMNIVM_BRIDGE_H

#include <stdint.h>

// ---- String-based bridge (existing) ----

// Function pointer type for cross-runtime calls.
// runtime: target runtime name ("python", "javascript", "ruby", "java")
// code:    expression to evaluate in the target runtime
// Returns: result string (caller must free via omni_free_fn), or
//          error string prefixed with "ERR:" (caller must free).
typedef char* (*omni_call_fn)(const char* runtime, const char* code);

// Function pointer type for freeing strings returned by omni_call_fn.
typedef void (*omni_free_fn)(char* ptr);

// Error prefix convention. All error returns start with this.
#define OMNI_ERR_PREFIX "ERR:"

// ---- Zero-copy buffer sharing (Tier 1) ----

// Data type tags for shared buffers.
#define OMNI_DTYPE_BYTES  0
#define OMNI_DTYPE_I32    1
#define OMNI_DTYPE_I64    2
#define OMNI_DTYPE_F32    3
#define OMNI_DTYPE_F64    4
#define OMNI_DTYPE_UTF8   5

// A shared memory buffer that can be passed between runtimes without copying.
// When owned=0 (borrowed), the source runtime retains ownership and the
// receiver must not free or hold the pointer beyond the current call.
// When owned=1, ownership transfers to the receiver.
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
} omni_buffer_t;

// Function pointer types for buffer operations.
// get: retrieve a named shared buffer. Returns 0 on success, -1 on error.
// set: register a buffer under a name. Returns 0 on success, -1 on error.
// release: schedule a buffer for deferred release (safe from GC threads).
typedef int (*omni_buf_get_fn)(const char* name, omni_buffer_t* out);
typedef int (*omni_buf_set_fn)(const char* name, omni_buffer_t buf);
typedef void (*omni_buf_release_fn)(const char* name);

// ---- Typed value bridge (Tier 2) ----

// Value type tags. int64_t so union is always at offset 8.
#define OMNI_TAG_NULL    0
#define OMNI_TAG_BOOL    1
#define OMNI_TAG_I64     2
#define OMNI_TAG_F64     3
#define OMNI_TAG_STRING  4
#define OMNI_TAG_BYTES   5
#define OMNI_TAG_REF     6
#define OMNI_TAG_ERROR   7

// Tagged value type for cross-runtime function calls without serialization.
// Layout: 8-byte tag + 24-byte union = 32 bytes total.
typedef struct {
    int64_t tag;
    union {
        int64_t  i;       // TAG_BOOL (0/1), TAG_I64
        double   f;       // TAG_F64
        struct { char* ptr; int64_t len; } s;  // TAG_STRING, TAG_BYTES, TAG_ERROR
        uint64_t ref;     // TAG_REF: opaque handle into HandleTable
    } v;
} omni_value_t;

// Typed call: invoke a named function in another runtime with typed args.
// Returns a typed value (TAG_ERROR on failure).
typedef omni_value_t (*omni_call_typed_fn)(
    const char* runtime,
    const char* func_name,
    omni_value_t* args,
    int32_t nargs
);

// Convenience constructors (inline for C/C++ compatibility)
static inline omni_value_t omni_null(void) {
    omni_value_t v; v.tag = OMNI_TAG_NULL; v.v.i = 0; return v;
}
static inline omni_value_t omni_bool(int b) {
    omni_value_t v; v.tag = OMNI_TAG_BOOL; v.v.i = b ? 1 : 0; return v;
}
static inline omni_value_t omni_i64(int64_t i) {
    omni_value_t v; v.tag = OMNI_TAG_I64; v.v.i = i; return v;
}
static inline omni_value_t omni_f64(double f) {
    omni_value_t v; v.tag = OMNI_TAG_F64; v.v.f = f; return v;
}
static inline omni_value_t omni_string(char* ptr, int64_t len) {
    omni_value_t v; v.tag = OMNI_TAG_STRING; v.v.s.ptr = ptr; v.v.s.len = len; return v;
}
static inline omni_value_t omni_error(char* ptr, int64_t len) {
    omni_value_t v; v.tag = OMNI_TAG_ERROR; v.v.s.ptr = ptr; v.v.s.len = len; return v;
}

#endif // OMNIVM_BRIDGE_H
