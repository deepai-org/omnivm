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

#endif // OMNIVM_BRIDGE_H
