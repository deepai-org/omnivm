// v8_bridge.h - C bridge for V8 embedding
// V8 is C++ but cgo needs C linkage. This header provides the C API.
#ifndef OMNIVM_V8_BRIDGE_H
#define OMNIVM_V8_BRIDGE_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// Opaque handles
typedef struct omnivm_v8_isolate omnivm_v8_isolate;
typedef struct omnivm_v8_context omnivm_v8_context;

typedef struct {
    char* value;   // output string (caller frees), NULL on error
    char* error;   // error string (caller frees), NULL on success
} omnivm_v8_result;

// Bridge callback type (matches omni_bridge.h)
typedef char* (*omnivm_bridge_call_fn)(const char* runtime, const char* code);
typedef void (*omnivm_bridge_free_fn)(char* ptr);

// Lifecycle
int omnivm_v8_init(void);
omnivm_v8_isolate* omnivm_v8_isolate_new(void);
omnivm_v8_context* omnivm_v8_context_new(omnivm_v8_isolate* isolate);

// Execution
omnivm_v8_result omnivm_v8_execute(omnivm_v8_context* ctx, const char* code);

// Eval — returns expression value directly (not stdout)
omnivm_v8_result omnivm_v8_eval(omnivm_v8_context* ctx, const char* code);

// Bridge callback registration
void omnivm_v8_set_bridge_callback(omnivm_bridge_call_fn call_fn, omnivm_bridge_free_fn free_fn);

// Event loop
void omnivm_v8_pump_message_loop(omnivm_v8_isolate* isolate);

// Cleanup
void omnivm_v8_context_free(omnivm_v8_context* ctx);
void omnivm_v8_isolate_free(omnivm_v8_isolate* isolate);
void omnivm_v8_shutdown(void);

// Helpers
void omnivm_v8_free_string(char* s);

// Epoll integration: get libuv's backend fd for epoll monitoring
int omnivm_v8_get_uv_backend_fd(omnivm_v8_context* ctx);

// Thread-safe execution termination (for watchdog)
void omnivm_v8_terminate_execution(omnivm_v8_context* ctx);

// Watchdog support: store context and get a void(void) terminate function pointer
void omnivm_v8_set_terminate_context(omnivm_v8_context* ctx);
void* omnivm_v8_get_terminate_ptr(void);

// Typed value bridge
typedef struct {
    int64_t tag;
    union {
        int64_t  i;
        double   f;
        struct { char* ptr; int64_t len; } s;
        uint64_t ref;
    } v;
} omni_value_t;

#define OMNI_TAG_NULL    0
#define OMNI_TAG_BOOL    1
#define OMNI_TAG_I64     2
#define OMNI_TAG_F64     3
#define OMNI_TAG_STRING  4
#define OMNI_TAG_BYTES   5
#define OMNI_TAG_REF     6
#define OMNI_TAG_ERROR   7

typedef omni_value_t (*omni_call_typed_fn)(const char* runtime,
                                            const char* func_name,
                                            omni_value_t* args,
                                            int32_t nargs);
void omnivm_v8_set_typed_callback(omni_call_typed_fn fn);

// Eval typed — returns expression as omni_value_t preserving native JS types
omni_value_t omnivm_v8_eval_typed(omnivm_v8_context* ctx, const char* code);

// Buffer bridge callback types and registration
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
    int8_t  read_only;
} omni_buffer_t;
typedef int (*omni_buf_get_fn)(const char* name, omni_buffer_t* out);
typedef int (*omni_buf_set_fn)(const char* name, omni_buffer_t buf);
typedef void (*omni_buf_release_fn)(const char* name);
void omnivm_v8_set_buf_callbacks(omni_buf_get_fn get_fn,
                                  omni_buf_set_fn set_fn,
                                  omni_buf_release_fn release_fn);

typedef struct {
    void* data;
    int64_t len;
    int32_t dtype;
    int64_t elements;
    const char* arrow_format;
    int8_t read_only;
    void* handle;
} omnivm_v8_exported_buffer_t;

int omnivm_v8_export_buffer(omnivm_v8_context* ctx,
                             const char* code,
                             omnivm_v8_exported_buffer_t* out);
void omnivm_v8_release_exported_buffer(void* handle);

#ifdef __cplusplus
}
#endif

#endif // OMNIVM_V8_BRIDGE_H
