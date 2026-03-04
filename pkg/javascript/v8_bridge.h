// v8_bridge.h - C bridge for V8 embedding
// V8 is C++ but cgo needs C linkage. This header provides the C API.
#ifndef OMNIVM_V8_BRIDGE_H
#define OMNIVM_V8_BRIDGE_H

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

#ifdef __cplusplus
}
#endif

#endif // OMNIVM_V8_BRIDGE_H
