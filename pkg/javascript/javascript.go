// Package javascript embeds V8 via cgo using a C bridge.
//
// Build requires: V8 development headers and libv8.
package javascript

/*
#cgo CXXFLAGS: -std=c++17 -I/usr/include/v8
#cgo LDFLAGS: -lv8 -lv8_libplatform -lv8_libbase -lpthread -ldl
#include "v8_bridge.h"
#include <stdlib.h>
*/
import "C"
import (
	"fmt"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
)

// Runtime implements pkg.Runtime for V8 JavaScript.
type Runtime struct {
	initialized bool
	isolate     *C.omnivm_v8_isolate
	context     *C.omnivm_v8_context
}

// New creates a new JavaScript runtime (not yet initialized).
func New() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Name() string { return "javascript" }

// Initialize starts V8 and creates an isolate + context.
// Must be called on the Golden Thread.
func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("javascript: already initialized")
	}

	if rc := C.omnivm_v8_init(); rc != 0 {
		return fmt.Errorf("javascript: V8 initialization failed (rc=%d)", rc)
	}

	r.isolate = C.omnivm_v8_isolate_new()
	if r.isolate == nil {
		return fmt.Errorf("javascript: failed to create V8 isolate")
	}

	r.context = C.omnivm_v8_context_new(r.isolate)
	if r.context == nil {
		C.omnivm_v8_isolate_free(r.isolate)
		return fmt.Errorf("javascript: failed to create V8 context")
	}

	r.initialized = true
	return nil
}

// Execute runs JavaScript code synchronously.
// Must be called on the Golden Thread.
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

// Pump processes pending V8 microtasks and platform messages.
func (r *Runtime) Pump() {
	if !r.initialized || r.isolate == nil {
		return
	}
	C.omnivm_v8_pump_message_loop(r.isolate)
}

// Shutdown tears down V8.
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
