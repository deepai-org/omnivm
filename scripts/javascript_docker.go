// Package javascript embeds a JavaScript engine via cgo.
//
// In Docker: uses a Duktape-based shim implementing the V8 bridge C API.
package javascript

/*
#include "v8_bridge.h"
#include <stdlib.h>
*/
import "C"
import (
	"fmt"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
)

// Runtime implements pkg.Runtime for JavaScript.
type Runtime struct {
	initialized bool
	isolate     *C.omnivm_v8_isolate
	context     *C.omnivm_v8_context
}

func New() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Name() string { return "javascript" }

func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("javascript: already initialized")
	}

	if rc := C.omnivm_v8_init(); rc != 0 {
		return fmt.Errorf("javascript: initialization failed (rc=%d)", rc)
	}

	r.isolate = C.omnivm_v8_isolate_new()
	if r.isolate == nil {
		return fmt.Errorf("javascript: failed to create isolate")
	}

	r.context = C.omnivm_v8_context_new(r.isolate)
	if r.context == nil {
		C.omnivm_v8_isolate_free(r.isolate)
		return fmt.Errorf("javascript: failed to create context")
	}

	r.initialized = true
	return nil
}

func (r *Runtime) Execute(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("javascript: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	result := C.omnivm_v8_execute(r.context, cCode)

	if result.error != nil {
		errStr := C.GoString(result.error)
		C.omnivm_v8_free_string(result.error)
		return pkg.Result{Err: fmt.Errorf("javascript: %s", errStr)}
	}

	var output string
	if result.value != nil {
		output = C.GoString(result.value)
		C.omnivm_v8_free_string(result.value)
	}

	return pkg.Result{Output: output}
}

// Eval evaluates a JS expression and returns its value directly (not stdout).
func (r *Runtime) Eval(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("javascript: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	result := C.omnivm_v8_eval(r.context, cCode)

	if result.error != nil {
		errStr := C.GoString(result.error)
		C.omnivm_v8_free_string(result.error)
		return pkg.Result{Err: fmt.Errorf("javascript: %s", errStr)}
	}

	var value string
	if result.value != nil {
		value = C.GoString(result.value)
		C.omnivm_v8_free_string(result.value)
	}

	return pkg.Result{Value: value, Output: value}
}

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_v8_set_bridge_callback(
		C.omnivm_bridge_call_fn(unsafe.Pointer(callPtr)),
		C.omnivm_bridge_free_fn(unsafe.Pointer(freePtr)),
	)
}

func (r *Runtime) Pump() {
	if !r.initialized || r.isolate == nil {
		return
	}
	C.omnivm_v8_pump_message_loop(r.isolate)
}

// GetUVBackendFD returns libuv's backend fd for epoll integration.
// Returns -1 if the runtime is not initialized.
func (r *Runtime) GetUVBackendFD() int {
	if !r.initialized || r.context == nil {
		return -1
	}
	return int(C.omnivm_v8_get_uv_backend_fd(r.context))
}

// TerminateExecution triggers V8's thread-safe execution termination.
// Safe to call from any thread (e.g. watchdog pthread).
func (r *Runtime) TerminateExecution() {
	if r.initialized && r.context != nil {
		C.omnivm_v8_terminate_execution(r.context)
	}
}

// TerminateFuncPtr returns a C function pointer that terminates V8 execution.
// Since omnivm_v8_terminate_execution takes a context parameter, we store
// the context in a global and provide a void(void) wrapper for the watchdog.
// This is safe because there's only one V8 context in the process.
func (r *Runtime) TerminateFuncPtr() unsafe.Pointer {
	if !r.initialized || r.context == nil {
		return nil
	}
	C.omnivm_v8_set_terminate_context(r.context)
	return unsafe.Pointer(C.omnivm_v8_get_terminate_ptr())
}

func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false

	if r.context != nil {
		C.omnivm_v8_context_free(r.context)
		r.context = nil
	}
	if r.isolate != nil {
		C.omnivm_v8_isolate_free(r.isolate)
		r.isolate = nil
	}
	C.omnivm_v8_shutdown()
	return nil
}
