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
	"github.com/omnivm/omnivm/pkg/arrow"
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

// ExportBuffer publishes a JavaScript ArrayBuffer or typed-array view into
// OmniVM's shared data plane without copying.
func (r *Runtime) ExportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	if !r.initialized {
		return pkg.ExportedBuffer{}, false, fmt.Errorf("javascript: not initialized")
	}
	cExpr := C.CString(expr)
	defer C.free(unsafe.Pointer(cExpr))

	var exported C.omnivm_v8_exported_buffer_t
	rc := C.omnivm_v8_export_buffer(r.context, cExpr, &exported)
	if rc < 0 {
		return pkg.ExportedBuffer{}, false, fmt.Errorf("javascript: export buffer failed")
	}
	if rc > 0 {
		return pkg.ExportedBuffer{}, false, nil
	}

	byteLen := int64(exported.len)
	if byteLen < 0 {
		C.omnivm_v8_release_exported_buffer(exported.handle)
		return pkg.ExportedBuffer{}, false, nil
	}
	elements := int64(exported.elements)
	if elements < 0 {
		C.omnivm_v8_release_exported_buffer(exported.handle)
		return pkg.ExportedBuffer{}, false, nil
	}
	dtype := int32(exported.dtype)
	arrowFormat := C.GoString(exported.arrow_format)
	readOnly := exported.read_only != 0
	if byteLen > 0 && exported.data == nil {
		C.omnivm_v8_release_exported_buffer(exported.handle)
		return pkg.ExportedBuffer{}, false, fmt.Errorf("javascript: exported buffer %q has nil data", name)
	}
	if _, err := arrow.GlobalStore().SetExternalWithMetadata(name, unsafe.Pointer(exported.data), byteLen, arrow.BufferMetadata{
		Dtype:     dtype,
		Format:    arrowFormat,
		Shape:     []int64{elements},
		ReadOnly:  readOnly,
		Ownership: "producer",
	}, func() error {
		C.omnivm_v8_release_exported_buffer(exported.handle)
		return nil
	}); err != nil {
		C.omnivm_v8_release_exported_buffer(exported.handle)
		return pkg.ExportedBuffer{}, false, err
	}
	return pkg.ExportedBuffer{
		Name:        name,
		Dtype:       dtype,
		ArrowFormat: arrowFormat,
		Elements:    elements,
		Shape:       []int64{elements},
		ReadOnly:    readOnly,
	}, true, nil
}

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_v8_set_bridge_callback(
		C.omnivm_bridge_call_fn(unsafe.Pointer(callPtr)),
		C.omnivm_bridge_free_fn(unsafe.Pointer(freePtr)),
	)
}

// SetBufCallbacks installs the buffer bridge function pointers.
func (r *Runtime) SetBufCallbacks(getPtr, setPtr, releasePtr, freePtr, statusPtr uintptr) {
	C.omnivm_v8_set_buf_callbacks(
		C.omni_buf_get_fn(unsafe.Pointer(getPtr)),
		C.omni_buf_set_fn(unsafe.Pointer(setPtr)),
		C.omni_buf_release_fn(unsafe.Pointer(releasePtr)),
		C.omni_buf_free_fn(unsafe.Pointer(freePtr)),
		C.omni_buf_status_fn(unsafe.Pointer(statusPtr)),
	)
}

// Pump processes pending V8 microtasks and platform messages.
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
