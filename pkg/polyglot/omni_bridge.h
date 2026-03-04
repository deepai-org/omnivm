// omni_bridge.h — Cross-runtime bridge function pointer type.
//
// Each runtime includes this header and stores the callback pointer
// set via its SetBridgeCallback(). No import cycles.
#ifndef OMNIVM_BRIDGE_H
#define OMNIVM_BRIDGE_H

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

#endif // OMNIVM_BRIDGE_H
