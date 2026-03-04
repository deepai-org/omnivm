// Package ruby embeds MRI Ruby via cgo.
//
// Build requires: ruby-dev headers and libruby.
package ruby

/*
#cgo pkg-config: ruby
#include <ruby.h>
#include <ruby/version.h>
#include <stdlib.h>
#include <string.h>

// Bridge callback pointer — set via omnivm_ruby_set_bridge_callback().
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

static int ruby_initialized = 0;

// rb_protect callback: calls exception.message
static VALUE call_exception_message(VALUE exception) {
    return rb_funcall(exception, rb_intern("message"), 0);
}

// rb_protect callback: calls exception.class.to_s
static VALUE call_exception_class_name(VALUE exception) {
    return rb_funcall(rb_funcall(exception, rb_intern("class"), 0),
                      rb_intern("to_s"), 0);
}

// Safe helper to extract exception message using rb_protect.
// Catches any secondary exceptions during message extraction (e.g., rb_exc_raise
// in rb_funcall which crashes on ARM64 when JVM is active).
// Returns a malloc'd string like "RubyError: ClassName: message" (caller must free).
static char* omnivm_ruby_safe_error_msg(VALUE exception) {
    int inner_state = 0;
    const char* klass_cstr = "UnknownError";
    const char* msg_cstr = "unknown error";

    // Try to get class name safely
    VALUE klass = rb_protect(call_exception_class_name, exception, &inner_state);
    if (!inner_state && klass != Qnil) {
        klass_cstr = StringValueCStr(klass);
    } else if (inner_state) {
        rb_set_errinfo(Qnil); // Clear secondary error
    }

    // Try to get message safely
    inner_state = 0;
    VALUE msg = rb_protect(call_exception_message, exception, &inner_state);
    if (!inner_state && msg != Qnil) {
        msg_cstr = StringValueCStr(msg);
    } else if (inner_state) {
        rb_set_errinfo(Qnil); // Clear secondary error
    }

    size_t len = strlen(klass_cstr) + strlen(msg_cstr) + 20;
    char* err = (char*)malloc(len);
    snprintf(err, len, "RubyError: %s: %s", klass_cstr, msg_cstr);
    return err;
}

static int omnivm_ruby_init(void) {
    if (ruby_initialized) return -1;

    // RUBY_INIT_STACK must be called from the stack frame that will
    // persist for the lifetime of the Ruby VM.
    RUBY_INIT_STACK;

    ruby_init();
    ruby_init_loadpath();
    ruby_initialized = 1;
    return 0;
}

// Execute Ruby code and capture stdout.
// Returns captured output (caller must free) or NULL on error.
static char* omnivm_ruby_exec(const char* code) {
    if (!ruby_initialized) return strdup("RubyError: not initialized");

    int state = 0;

    // Set up stdout capture:
    // $__omnivm_out = StringIO.new
    // $__omnivm_old_stdout = $stdout
    // $stdout = $__omnivm_out
    rb_eval_string_protect(
        "require 'stringio'\n"
        "$__omnivm_out = StringIO.new\n"
        "$__omnivm_old_stdout = $stdout\n"
        "$stdout = $__omnivm_out",
        &state
    );
    if (state) {
        rb_eval_string_protect("$stdout = $__omnivm_old_stdout if $__omnivm_old_stdout", &state);
        return strdup("RubyError: failed to set up output capture");
    }

    // Execute user code
    VALUE result = rb_eval_string_protect(code, &state);

    // Restore stdout
    rb_eval_string_protect("$stdout = $__omnivm_old_stdout", &state);

    if (state) {
        VALUE exception = rb_errinfo();
        rb_set_errinfo(Qnil);
        if (exception != Qnil) {
            return omnivm_ruby_safe_error_msg(exception);
        }
        return strdup("RubyError: unknown error");
    }

    // Get captured output
    VALUE captured = rb_eval_string_protect(
        "$__omnivm_out.string",
        &state
    );

    if (state || captured == Qnil) {
        return strdup("");
    }

    const char* output = StringValueCStr(captured);
    return strdup(output);
}

// Eval: evaluate expression and return its value as a string.
// Returns result string (caller must free) or NULL on error.
static char* omnivm_ruby_eval(const char* code) {
    if (!ruby_initialized) return NULL;

    int state = 0;
    VALUE result = rb_eval_string_protect(code, &state);

    if (state) {
        VALUE exception = rb_errinfo();
        rb_set_errinfo(Qnil);
        if (exception != Qnil) {
            return omnivm_ruby_safe_error_msg(exception);
        }
        return NULL;
    }

    // Convert result to string via to_s
    VALUE str_result = rb_funcall(result, rb_intern("to_s"), 0);
    const char* cstr = StringValueCStr(str_result);
    return strdup(cstr);
}

// C implementation of OmniVM.call(runtime, code) for Ruby
static VALUE rb_omnivm_call(VALUE self, VALUE rb_runtime, VALUE rb_code) {
    const char* runtime = StringValueCStr(rb_runtime);
    const char* code = StringValueCStr(rb_code);

    if (!g_bridge_call) {
        rb_raise(rb_eRuntimeError, "omnivm bridge not initialized");
        return Qnil;
    }

    char* result = g_bridge_call(runtime, code);
    if (!result) {
        rb_raise(rb_eRuntimeError, "OmniVM.call returned NULL");
        return Qnil;
    }

    // Check for error prefix
    if (strncmp(result, "ERR:", 4) == 0) {
        VALUE err_msg = rb_str_new_cstr(result + 4);
        if (g_bridge_free) g_bridge_free(result);
        rb_raise(rb_eRuntimeError, "%s", StringValueCStr(err_msg));
        return Qnil;
    }

    VALUE rb_result = rb_str_new_cstr(result);
    if (g_bridge_free) g_bridge_free(result);
    return rb_result;
}

// Register OmniVM module with call() method
static void omnivm_ruby_register_bridge() {
    VALUE mod = rb_define_module("OmniVM");
    rb_define_module_function(mod, "call", rb_omnivm_call, 2);
}

static void omnivm_ruby_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

static void omnivm_ruby_shutdown(void) {
    if (ruby_initialized) {
        // NOTE: We intentionally do NOT call ruby_cleanup() here.
        // In a polyglot process, ruby_cleanup() calls rb_thread_terminate_all()
        // which sends signals to terminate threads. When the JVM is also running,
        // its threads interfere with Ruby's signal-based cleanup and cause a
        // segfault. Since the process is exiting, the OS will reclaim all
        // resources. This is safe and matches how other embedded Ruby hosts
        // (e.g., Apache mod_ruby) handle shutdown.
        ruby_initialized = 0;
    }
}
*/
import "C"
import (
	"fmt"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
)

// Runtime implements pkg.Runtime for MRI Ruby.
type Runtime struct {
	initialized bool
}

// New creates a new Ruby runtime (not yet initialized).
func New() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Name() string { return "ruby" }

// Initialize starts the Ruby VM.
// Must be called on the Golden Thread.
func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("ruby: already initialized")
	}

	rc := C.omnivm_ruby_init()
	if rc != 0 {
		return fmt.Errorf("ruby: initialization failed")
	}

	// Register the OmniVM module
	C.omnivm_ruby_register_bridge()

	r.initialized = true
	return nil
}

// Execute runs Ruby code synchronously.
// Must be called on the Golden Thread.
func (r *Runtime) Execute(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("ruby: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_ruby_exec(cCode)
	if cOutput == nil {
		return pkg.Result{Err: fmt.Errorf("ruby: execution returned nil")}
	}

	output := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))

	if strings.HasPrefix(output, "RubyError: ") {
		return pkg.Result{Err: fmt.Errorf("ruby: %s", strings.TrimPrefix(output, "RubyError: "))}
	}

	return pkg.Result{Output: output}
}

// Eval evaluates a Ruby expression and returns its value directly.
func (r *Runtime) Eval(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("ruby: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_ruby_eval(cCode)
	if cOutput == nil {
		return pkg.Result{Err: fmt.Errorf("ruby: eval failed")}
	}

	output := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))

	if strings.HasPrefix(output, "RubyError: ") {
		return pkg.Result{Err: fmt.Errorf("ruby: %s", strings.TrimPrefix(output, "RubyError: "))}
	}

	return pkg.Result{Value: output, Output: output}
}

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_ruby_set_bridge_callback(
		C.omni_call_fn(unsafe.Pointer(callPtr)),
		C.omni_free_fn(unsafe.Pointer(freePtr)),
	)
}

// Pump is a no-op for Ruby (no cooperative event loop).
func (r *Runtime) Pump() {}

// Shutdown finalizes the Ruby VM.
func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false
	C.omnivm_ruby_shutdown()
	return nil
}
