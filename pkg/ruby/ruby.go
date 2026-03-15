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
#include <pthread.h>
#include <unistd.h>

// Bridge callback pointer — set via omnivm_ruby_set_bridge_callback().
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

static int ruby_initialized = 0;

// Forward declarations (defined later in this file)
static char* omnivm_ruby_exec(const char* code);
static char* omnivm_ruby_eval(const char* code);
static int omnivm_ruby_init(void);
static void omnivm_ruby_register_bridge(void);

// Thread-local GVL ownership flag.
// 0 = does not hold GVL, 1 = holds GVL.
// Used to decide whether to call rb_thread_call_with_gvl or invoke directly.
static __thread int tls_holds_gvl = 0;

// Thread-local flag: 1 = this OS thread is "known" to Ruby (main thread or
// a thread created by Ruby). Foreign threads (JVM, Python, Go background)
// cannot use rb_thread_call_with_gvl — it crashes with "non-ruby thread".
static __thread int tls_is_ruby_thread = 0;

typedef struct { const char* code; char* result; } ruby_gvl_args;

static void* omnivm_ruby_exec_with_gvl(void* raw) {
    tls_holds_gvl = 1;
    ruby_gvl_args* a = (ruby_gvl_args*)raw;
    a->result = omnivm_ruby_exec(a->code);
    tls_holds_gvl = 0;
    return NULL;
}

static void* omnivm_ruby_eval_with_gvl(void* raw) {
    tls_holds_gvl = 1;
    ruby_gvl_args* a = (ruby_gvl_args*)raw;
    a->result = omnivm_ruby_eval(a->code);
    tls_holds_gvl = 0;
    return NULL;
}

// ---- Foreign thread proxy ----
// Ruby rejects rb_thread_call_with_gvl from threads it doesn't know about.
// For truly foreign threads (JVM, Python, Go goroutines), we route work
// through a dedicated Ruby-owned proxy thread using condvar signaling.

static pthread_mutex_t rb_fproxy_mtx = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t rb_fproxy_req_cv = PTHREAD_COND_INITIALIZER;
static pthread_cond_t rb_fproxy_res_cv = PTHREAD_COND_INITIALIZER;
static const char* rb_fproxy_code = NULL;
static char* rb_fproxy_result = NULL;
static int rb_fproxy_is_eval = 0;
static volatile int rb_fproxy_has_request = 0;
static volatile int rb_fproxy_shutdown = 0;
static volatile int rb_fproxy_ready = 0;

// Called WITHOUT the GVL — blocks waiting for work from foreign threads.
static void* rb_fproxy_wait(void* arg) {
    (void)arg;
    pthread_mutex_lock(&rb_fproxy_mtx);
    while (!rb_fproxy_has_request && !rb_fproxy_shutdown) {
        pthread_cond_wait(&rb_fproxy_req_cv, &rb_fproxy_mtx);
    }
    pthread_mutex_unlock(&rb_fproxy_mtx);
    return NULL;
}

// Proxy thread's pthread_t (stored for reference).
static pthread_t rb_fproxy_tid;

// Pipe-based interrupt mechanism (replaces SIGUSR1).
// The watchdog writes to the pipe; a Ruby watcher thread reads from it
// and calls Thread#raise(Interrupt) on the proxy thread.
static int ruby_interrupt_pipe[2] = {-1, -1};
static VALUE ruby_proxy_thread_val = Qnil;

// Ruby method: OmniVM._proxy_serve — runs in a dedicated Ruby Thread.
// Loops forever: releases GVL → waits for work → reacquires GVL → executes.
static VALUE rb_fproxy_serve(VALUE self) {
    rb_fproxy_tid = pthread_self();
    tls_holds_gvl = 1;
    tls_is_ruby_thread = 1;
    rb_fproxy_ready = 1;

    while (!rb_fproxy_shutdown) {
        tls_holds_gvl = 0;
        rb_thread_call_without_gvl(rb_fproxy_wait, NULL, RUBY_UBF_IO, NULL);
        tls_holds_gvl = 1;

        if (rb_fproxy_shutdown) break;

        // Execute the requested code (we hold GVL)
        if (rb_fproxy_is_eval) {
            rb_fproxy_result = omnivm_ruby_eval(rb_fproxy_code);
        } else {
            rb_fproxy_result = omnivm_ruby_exec(rb_fproxy_code);
        }

        // Signal completion
        pthread_mutex_lock(&rb_fproxy_mtx);
        rb_fproxy_has_request = 0;
        pthread_cond_signal(&rb_fproxy_res_cv);
        pthread_mutex_unlock(&rb_fproxy_mtx);
    }
    return Qnil;
}

// Submit work from a foreign (non-Ruby) thread. Blocks until completion.
// Multiple foreign threads are serialized by the mutex.
static char* rb_fproxy_submit(const char* code, int is_eval) {
    // Wait for proxy thread to be ready (handles init startup race)
    while (!rb_fproxy_ready) {
        usleep(1000);  // 1ms
    }

    pthread_mutex_lock(&rb_fproxy_mtx);
    rb_fproxy_code = code;
    rb_fproxy_is_eval = is_eval;
    rb_fproxy_has_request = 1;
    pthread_cond_signal(&rb_fproxy_req_cv);

    // Wait for result
    while (rb_fproxy_has_request) {
        pthread_cond_wait(&rb_fproxy_res_cv, &rb_fproxy_mtx);
    }
    char* result = rb_fproxy_result;
    rb_fproxy_result = NULL;
    pthread_mutex_unlock(&rb_fproxy_mtx);
    return result;
}

// Yield GVL briefly to let the proxy thread start up.
static void* rb_fproxy_yield_for_startup(void* arg) {
    (void)arg;
    while (!rb_fproxy_ready) {
        usleep(100);
    }
    return NULL;
}

// --- Ruby initialization on dedicated pthread ---
// Ruby is initialized on a background thread so the Golden Thread never
// holds the GVL. This prevents deadlocks when JVM/Python threads try to
// call Ruby while the Golden Thread is executing non-Ruby code.

static pthread_t ruby_init_pthread;
static volatile int ruby_init_done = 0;
static volatile int ruby_init_error = 0;

// Idle condvar — the Ruby main thread blocks here after releasing GVL.
static pthread_mutex_t ruby_idle_mtx = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t ruby_idle_cv = PTHREAD_COND_INITIALIZER;

// Callback for rb_thread_call_without_gvl — blocks until shutdown.
static void* ruby_main_idle(void* arg) {
    (void)arg;
    pthread_mutex_lock(&ruby_idle_mtx);
    while (!rb_fproxy_shutdown) {
        pthread_cond_wait(&ruby_idle_cv, &ruby_idle_mtx);
    }
    pthread_mutex_unlock(&ruby_idle_mtx);
    return NULL;
}

static void* ruby_init_thread_func(void* arg) {
    (void)arg;

    // Initialize Ruby on this thread
    if (omnivm_ruby_init() != 0) {
        ruby_init_error = 1;
        ruby_init_done = 1;
        return NULL;
    }

    // Register bridge module (includes _proxy_serve)
    omnivm_ruby_register_bridge();

    // Create interrupt pipe (before starting threads)
    if (pipe(ruby_interrupt_pipe) != 0) {
        ruby_init_error = 1;
        ruby_init_done = 1;
        return NULL;
    }

    // Start the proxy thread — store its Thread object for interrupt routing
    int state = 0;
    ruby_proxy_thread_val = rb_eval_string_protect(
        "Thread.new { OmniVM._proxy_serve }", &state);
    if (state) {
        ruby_init_error = 1;
        ruby_init_done = 1;
        return NULL;
    }
    rb_gc_register_mark_object(ruby_proxy_thread_val);
    rb_gv_set("$__omnivm_proxy", ruby_proxy_thread_val);

    // Yield GVL to let proxy thread start
    tls_holds_gvl = 0;
    rb_thread_call_without_gvl(rb_fproxy_yield_for_startup, NULL, NULL, NULL);
    tls_holds_gvl = 1;

    // Start the interrupt watcher thread — reads from pipe and raises
    // Interrupt on the proxy thread. Uses Thread#raise which enqueues
    // an interrupt that Ruby checks between opcodes.
    char watcher_code[512];
    snprintf(watcher_code, sizeof(watcher_code),
        "Thread.new {\n"
        "  r = IO.new(%d, autoclose: false)\n"
        "  loop {\n"
        "    r.read(1) or break\n"
        "    $__omnivm_proxy.raise(Interrupt) rescue nil\n"
        "  }\n"
        "}",
        ruby_interrupt_pipe[0]
    );
    rb_eval_string_protect(watcher_code, &state);

    // Install fork guard
    state = 0;
    rb_eval_string_protect(
        "module Process; def self.fork; raise RuntimeError, "
        "'fork() not safe in OmniVM'; end; end",
        &state
    );

    // Signal to Go that init is complete
    ruby_init_done = 1;

    // Release GVL permanently — the proxy thread handles all execution.
    tls_holds_gvl = 0;
    rb_thread_call_without_gvl(ruby_main_idle, NULL, NULL, NULL);
    tls_holds_gvl = 1;

    return NULL;
}

static int omnivm_ruby_init_on_thread(void) {
    if (ruby_initialized) return -1;
    pthread_create(&ruby_init_pthread, NULL, ruby_init_thread_func, NULL);
    while (!ruby_init_done) usleep(100);
    if (ruby_init_error) return -1;
    return 0;
}

// Write to the interrupt pipe — safe from any thread, no GVL needed.
// The Ruby watcher thread reads the byte and raises Interrupt on the proxy.
static void omnivm_ruby_interrupt(void) {
    char c = 1;
    if (ruby_interrupt_pipe[1] >= 0) {
        (void)write(ruby_interrupt_pipe[1], &c, 1);
    }
}

// Returns function pointer for the watchdog to call.
static void* omnivm_ruby_get_interrupt_ptr(void) {
    return (void*)omnivm_ruby_interrupt;
}

// Thread-safe exec: direct if we hold GVL, via rb_thread_call_with_gvl
// for Ruby-known threads, via proxy for foreign threads.
static char* omnivm_ruby_exec_safe(const char* code) {
    if (tls_holds_gvl) return omnivm_ruby_exec(code);
    if (tls_is_ruby_thread) {
        ruby_gvl_args args = { .code = code, .result = NULL };
        rb_thread_call_with_gvl(omnivm_ruby_exec_with_gvl, &args);
        return args.result;
    }
    // Foreign thread: route through proxy
    return rb_fproxy_submit(code, 0);
}

// Thread-safe eval: same three-tier dispatch as exec.
static char* omnivm_ruby_eval_safe(const char* code) {
    if (tls_holds_gvl) return omnivm_ruby_eval(code);
    if (tls_is_ruby_thread) {
        ruby_gvl_args args = { .code = code, .result = NULL };
        rb_thread_call_with_gvl(omnivm_ruby_eval_with_gvl, &args);
        return args.result;
    }
    // Foreign thread: route through proxy
    return rb_fproxy_submit(code, 1);
}

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
    // Calling thread holds the GVL after ruby_init()
    tls_holds_gvl = 1;
    tls_is_ruby_thread = 1;
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

    // Restore stdout (separate variable to preserve user code error state)
    int restore_state = 0;
    rb_eval_string_protect("$stdout = $__omnivm_old_stdout", &restore_state);

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

// Bridge args for rb_thread_call_without_gvl
typedef struct {
    const char* runtime;
    const char* code;
    char* result;
} ruby_bridge_args;

// Called from rb_thread_call_without_gvl — GVL is NOT held here.
static void* ruby_bridge_no_gvl(void* raw) {
    tls_holds_gvl = 0;  // We explicitly dropped the GVL
    ruby_bridge_args* a = (ruby_bridge_args*)raw;
    a->result = g_bridge_call(a->runtime, a->code);
    tls_holds_gvl = 1;  // GVL reacquired upon return from rb_thread_call_without_gvl
    return NULL;
}

// C implementation of OmniVM.call(runtime, code) for Ruby.
// Releases GVL during the cross-runtime call to prevent deadlock
// and allow other Ruby threads to run concurrently.
static VALUE rb_omnivm_call(VALUE self, VALUE rb_runtime, VALUE rb_code) {
    const char* runtime = StringValueCStr(rb_runtime);  // needs GVL
    const char* code = StringValueCStr(rb_code);        // needs GVL

    if (!g_bridge_call) {
        rb_raise(rb_eRuntimeError, "omnivm bridge not initialized");
        return Qnil;
    }

    ruby_bridge_args bargs = { .runtime = runtime, .code = code, .result = NULL };
    rb_thread_call_without_gvl(ruby_bridge_no_gvl, &bargs, RUBY_UBF_IO, NULL);
    char* result = bargs.result;

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

// Register OmniVM module with call() and _proxy_serve methods
static void omnivm_ruby_register_bridge() {
    VALUE mod = rb_define_module("OmniVM");
    rb_define_module_function(mod, "call", rb_omnivm_call, 2);
    rb_define_module_function(mod, "_proxy_serve", rb_fproxy_serve, 0);
}

static void omnivm_ruby_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

static void omnivm_ruby_shutdown(void) {
    if (ruby_initialized) {
        // Signal the foreign-thread proxy to shut down
        pthread_mutex_lock(&rb_fproxy_mtx);
        rb_fproxy_shutdown = 1;
        pthread_cond_signal(&rb_fproxy_req_cv);
        pthread_mutex_unlock(&rb_fproxy_mtx);

        // Close interrupt pipe to wake the watcher thread
        if (ruby_interrupt_pipe[0] >= 0) {
            close(ruby_interrupt_pipe[0]);
            close(ruby_interrupt_pipe[1]);
            ruby_interrupt_pipe[0] = -1;
            ruby_interrupt_pipe[1] = -1;
        }

        // Wake the Ruby main idle thread so it can exit
        pthread_mutex_lock(&ruby_idle_mtx);
        pthread_cond_signal(&ruby_idle_cv);
        pthread_mutex_unlock(&ruby_idle_mtx);

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

// Initialize starts the Ruby VM on a dedicated background pthread.
// The Golden Thread never holds the Ruby GVL — all Ruby calls from any
// thread are routed through a proxy thread.
func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("ruby: already initialized")
	}

	rc := C.omnivm_ruby_init_on_thread()
	if rc != 0 {
		return fmt.Errorf("ruby: initialization failed")
	}

	r.initialized = true
	return nil
}

// InterruptFuncPtr returns a C function pointer that the watchdog can call
// to interrupt Ruby execution. The function writes to a pipe; a Ruby watcher
// thread reads from it and raises Interrupt on the proxy thread.
func (r *Runtime) InterruptFuncPtr() unsafe.Pointer {
	return unsafe.Pointer(C.omnivm_ruby_get_interrupt_ptr())
}

// Execute runs Ruby code synchronously.
// Must be called on the Golden Thread.
func (r *Runtime) Execute(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("ruby: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_ruby_exec_safe(cCode)
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

	cOutput := C.omnivm_ruby_eval_safe(cCode)
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
