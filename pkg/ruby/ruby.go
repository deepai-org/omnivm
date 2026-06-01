// Package ruby embeds MRI Ruby via cgo.
//
// Build requires: ruby-dev headers and libruby.
package ruby

/*
#cgo pkg-config: ruby
#include <ruby.h>
#include <ruby/version.h>
#include <ruby/thread.h>
#include <ruby/debug.h>
#include <ruby/encoding.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>
#include <unistd.h>
#include <signal.h>

// Bridge callback pointer — set via omnivm_ruby_set_bridge_callback().
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

// Buffer bridge function pointers
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
    int8_t  read_only;
} rb_omni_buffer_t;

typedef int (*omni_buf_get_fn)(const char* name, rb_omni_buffer_t* out);
typedef int (*omni_buf_set_fn)(const char* name, rb_omni_buffer_t buf);
typedef void (*omni_buf_release_fn)(const char* name);

static omni_buf_get_fn g_buf_get = NULL;
static omni_buf_set_fn g_buf_set = NULL;
static omni_buf_release_fn g_buf_release = NULL;

// Typed value bridge
typedef struct {
    int64_t tag;
    union {
        int64_t  i;
        double   f;
        struct { char* ptr; int64_t len; } s;
        uint64_t ref;
    } v;
} rb_omni_value_t;

#define RB_OMNI_TAG_NULL    0
#define RB_OMNI_TAG_BOOL    1
#define RB_OMNI_TAG_I64     2
#define RB_OMNI_TAG_F64     3
#define RB_OMNI_TAG_STRING  4
#define RB_OMNI_TAG_BYTES   5
#define RB_OMNI_TAG_REF     6
#define RB_OMNI_TAG_ERROR   7

typedef rb_omni_value_t (*rb_omni_call_typed_fn)(const char* runtime,
                                                  const char* func_name,
                                                  rb_omni_value_t* args,
                                                  int32_t nargs);
static rb_omni_call_typed_fn g_call_typed = NULL;

static void omnivm_ruby_set_typed_callback(rb_omni_call_typed_fn fn) {
    g_call_typed = fn;
}

// Convert a Ruby VALUE to rb_omni_value_t
static rb_omni_value_t rb_to_omni_value(VALUE val) {
    rb_omni_value_t out;
    memset(&out, 0, sizeof(out));

    if (NIL_P(val)) {
        out.tag = RB_OMNI_TAG_NULL;
    } else if (val == Qtrue) {
        out.tag = RB_OMNI_TAG_BOOL;
        out.v.i = 1;
    } else if (val == Qfalse) {
        out.tag = RB_OMNI_TAG_BOOL;
        out.v.i = 0;
    } else if (RB_FIXNUM_P(val)) {
        out.tag = RB_OMNI_TAG_I64;
        out.v.i = FIX2LONG(val);
    } else if (RB_TYPE_P(val, T_BIGNUM)) {
        out.tag = RB_OMNI_TAG_I64;
        out.v.i = NUM2LL(val);
    } else if (RB_FLOAT_TYPE_P(val)) {
        out.tag = RB_OMNI_TAG_F64;
        out.v.f = RFLOAT_VALUE(val);
    } else if (RB_TYPE_P(val, T_STRING)) {
        out.tag = RB_OMNI_TAG_STRING;
        out.v.s.len = RSTRING_LEN(val);
        out.v.s.ptr = (char*)malloc(out.v.s.len + 1);
        memcpy(out.v.s.ptr, RSTRING_PTR(val), out.v.s.len);
        out.v.s.ptr[out.v.s.len] = '\0';
    } else {
        const char* msg = "unsupported typed bridge argument; complex values must cross through the manifest proxy/Arrow boundary, not implicit stringification";
        out.tag = RB_OMNI_TAG_ERROR;
        out.v.s.len = strlen(msg);
        out.v.s.ptr = (char*)malloc(out.v.s.len + 1);
        memcpy(out.v.s.ptr, msg, out.v.s.len + 1);
    }
    return out;
}

// Convert rb_omni_value_t to a Ruby VALUE
static VALUE omni_value_to_rb(rb_omni_value_t val) {
    switch (val.tag) {
    case RB_OMNI_TAG_NULL:
        return Qnil;
    case RB_OMNI_TAG_BOOL:
        return val.v.i ? Qtrue : Qfalse;
    case RB_OMNI_TAG_I64:
        return LL2NUM(val.v.i);
    case RB_OMNI_TAG_F64:
        return DBL2NUM(val.v.f);
    case RB_OMNI_TAG_STRING:
        if (val.v.s.ptr)
            return rb_str_new(val.v.s.ptr, (long)val.v.s.len);
        return rb_str_new("", 0);
    case RB_OMNI_TAG_BYTES:
        if (val.v.s.ptr) {
            VALUE str = rb_str_new(val.v.s.ptr, (long)val.v.s.len);
            rb_enc_associate(str, rb_ascii8bit_encoding());
            return str;
        }
        return rb_str_new("", 0);
    case RB_OMNI_TAG_ERROR:
        rb_raise(rb_eRuntimeError, "%s", val.v.s.ptr ? val.v.s.ptr : "unknown error");
        return Qnil;
    default:
        return Qnil;
    }
}

static void rb_free_omni_value(rb_omni_value_t* val) {
    if (val->tag == RB_OMNI_TAG_STRING || val->tag == RB_OMNI_TAG_BYTES ||
        val->tag == RB_OMNI_TAG_ERROR) {
        free(val->v.s.ptr);
        val->v.s.ptr = NULL;
    }
}

static void omnivm_ruby_set_buf_callbacks(omni_buf_get_fn get_fn,
                                           omni_buf_set_fn set_fn,
                                           omni_buf_release_fn release_fn) {
    g_buf_get = get_fn;
    g_buf_set = set_fn;
    g_buf_release = release_fn;
}

static int ruby_initialized = 0;

static VALUE omnivm_ruby_object_class(VALUE self) {
    return rb_obj_class(self);
}

static VALUE omnivm_ruby_object_clone(VALUE self) {
    return rb_obj_clone(self);
}

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

// Proxy thread's pthread_t (stored for reference).
static pthread_t rb_fproxy_tid;

// Proxy thread's Ruby thread object (for GC pinning).
static VALUE ruby_proxy_thread_val = Qnil;

// NOTE: rb_fproxy_serve (Ruby Thread-based proxy) is no longer used.
// Ruby 3.3's M:N threading breaks Thread.new and rb_thread_call_without_gvl
// on non-main pthreads. The proxy now runs as a plain C condvar loop in
// ruby_init_thread_func instead.

// Submit work from a foreign (non-Ruby) thread. Blocks until completion.
// Multiple foreign threads are serialized by the mutex.
typedef void* (*rb_fproxy_callback_fn)(void*);
static rb_fproxy_callback_fn rb_fproxy_callback = NULL;
static void* rb_fproxy_callback_arg = NULL;

static char* rb_fproxy_submit(const char* code, int is_eval) {
    // Reject if the proxy is shutting down / already dead
    if (rb_fproxy_shutdown) {
        return strdup("RubyError: runtime is shutting down");
    }

    // Wait for proxy thread to be ready (handles init startup race)
    while (!rb_fproxy_ready && !rb_fproxy_shutdown) {
        usleep(1000);  // 1ms
    }
    if (rb_fproxy_shutdown) {
        return strdup("RubyError: runtime is shutting down");
    }

    pthread_mutex_lock(&rb_fproxy_mtx);
    if (rb_fproxy_shutdown) {
        pthread_mutex_unlock(&rb_fproxy_mtx);
        return strdup("RubyError: runtime is shutting down");
    }
    rb_fproxy_code = code;
    rb_fproxy_is_eval = is_eval;
    rb_fproxy_callback = NULL;
    rb_fproxy_callback_arg = NULL;
    rb_fproxy_has_request = 1;
    pthread_cond_signal(&rb_fproxy_req_cv);

    // Wait for result — also bail if the proxy shuts down mid-request
    while (rb_fproxy_has_request && !rb_fproxy_shutdown) {
        pthread_cond_wait(&rb_fproxy_res_cv, &rb_fproxy_mtx);
    }
    if (rb_fproxy_shutdown && rb_fproxy_has_request) {
        // Proxy died before completing our request
        rb_fproxy_has_request = 0;
        pthread_mutex_unlock(&rb_fproxy_mtx);
        return strdup("RubyError: runtime shut down during execution");
    }
    char* result = rb_fproxy_result;
    rb_fproxy_result = NULL;
    pthread_mutex_unlock(&rb_fproxy_mtx);
    return result;
}

static void rb_fproxy_submit_callback(rb_fproxy_callback_fn fn, void* arg) {
    if (rb_fproxy_shutdown || !fn) return;
    while (!rb_fproxy_ready && !rb_fproxy_shutdown) {
        usleep(1000);
    }
    if (rb_fproxy_shutdown) return;

    pthread_mutex_lock(&rb_fproxy_mtx);
    if (rb_fproxy_shutdown) {
        pthread_mutex_unlock(&rb_fproxy_mtx);
        return;
    }
    rb_fproxy_code = NULL;
    rb_fproxy_is_eval = 0;
    rb_fproxy_callback = fn;
    rb_fproxy_callback_arg = arg;
    rb_fproxy_has_request = 1;
    pthread_cond_signal(&rb_fproxy_req_cv);

    while (rb_fproxy_has_request && !rb_fproxy_shutdown) {
        pthread_cond_wait(&rb_fproxy_res_cv, &rb_fproxy_mtx);
    }
    pthread_mutex_unlock(&rb_fproxy_mtx);
}


// --- Ruby initialization on dedicated pthread ---
// Ruby is initialized on a background pthread that stays alive (anchoring
// RUBY_INIT_STACK). The pthread also serves as the execution thread —
// all Ruby code runs on this pthread via condvar-based request dispatch.
//
// Ruby 3.3 M:N threading note: Thread.new and rb_thread_call_without_gvl
// don't properly schedule on non-main pthreads. So we avoid Ruby threads
// entirely — the init pthread holds the GVL permanently and executes
// requests in a simple condvar loop.

static pthread_t ruby_init_pthread;
static volatile int ruby_init_done = 0;
static volatile int ruby_init_error = 0;

// Set up an alternate signal stack on the current thread.
static void setup_sigaltstack(void) {
    stack_t ss;
    ss.ss_sp = malloc(SIGSTKSZ);
    if (ss.ss_sp == NULL) return;
    ss.ss_size = SIGSTKSZ;
    ss.ss_flags = 0;
    sigaltstack(&ss, NULL);
}

// Fix Ruby's signal handlers to include SA_ONSTACK.
// Go requires all signal handlers to use SA_ONSTACK. Ruby 3.3 installs
// handlers (SIGCHLD, SIGPIPE, etc.) without this flag, which causes
// "non-Go code set up signal handler without SA_ONSTACK flag" crashes.
static void fix_signal_handlers_sa_onstack(void) {
    for (int sig = 1; sig < NSIG; sig++) {
        struct sigaction sa;
        if (sigaction(sig, NULL, &sa) == 0) {
            if (sa.sa_handler != SIG_DFL && sa.sa_handler != SIG_IGN &&
                !(sa.sa_flags & SA_ONSTACK)) {
                sa.sa_flags |= SA_ONSTACK;
                sigaction(sig, &sa, NULL);
            }
        }
    }
}

static void* ruby_init_thread_func(void* arg) {
    (void)arg;

    // Set up alternate signal stack BEFORE Ruby installs signal handlers.
    setup_sigaltstack();

    if (!ruby_initialized) {
        // First-time init: set up Ruby VM
        if (omnivm_ruby_init() != 0) {
            ruby_init_error = 1;
            ruby_init_done = 1;
            return NULL;
        }

        omnivm_ruby_register_bridge();

        // Install fork guard
        int state = 0;
        rb_eval_string_protect(
            "module Process; def self.fork; raise RuntimeError, "
            "'fork() not safe in OmniVM'; end; end",
            &state
        );
    }

    // Store the proxy's Ruby thread object for interrupt delivery
    ruby_proxy_thread_val = rb_thread_current();
    rb_gc_register_address(&ruby_proxy_thread_val);

    // Signal to Go that init/restart is complete
    ruby_init_done = 1;

    // Enter the request-serving loop. This pthread holds the GVL
    // permanently and processes requests from any thread via condvar.
    // No Ruby threads are used — avoiding Ruby 3.3 M:N scheduling issues.
    rb_fproxy_ready = 1;
    while (!rb_fproxy_shutdown) {
        pthread_mutex_lock(&rb_fproxy_mtx);
        while (!rb_fproxy_has_request && !rb_fproxy_shutdown) {
            pthread_cond_wait(&rb_fproxy_req_cv, &rb_fproxy_mtx);
        }
        pthread_mutex_unlock(&rb_fproxy_mtx);

        if (rb_fproxy_shutdown) break;

        // Execute the requested code/callback (we hold GVL)
        if (rb_fproxy_callback) {
            rb_fproxy_callback(rb_fproxy_callback_arg);
            rb_fproxy_callback = NULL;
            rb_fproxy_callback_arg = NULL;
            rb_fproxy_result = NULL;
        } else if (rb_fproxy_is_eval) {
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

    return NULL;
}

static int omnivm_ruby_init_on_thread(void) {
    if (ruby_initialized) return -1;
    pthread_create(&ruby_init_pthread, NULL, ruby_init_thread_func, NULL);
    while (!ruby_init_done) usleep(100);
    if (ruby_init_error) return -1;
    return 0;
}

// Volatile interrupt flag: set by watchdog (any thread), checked by
// Ruby trace hook (runs with GVL on proxy pthread).
static volatile int rb_interrupt_requested = 0;

// Trace hook: fires on every Ruby line event. If the interrupt flag
// is set, raises Interrupt. This runs with the GVL held on the proxy.
static void rb_interrupt_trace_hook(rb_event_flag_t event, VALUE data,
                                     VALUE self, ID mid, VALUE klass) {
    (void)event; (void)data; (void)self; (void)mid; (void)klass;
    if (rb_interrupt_requested) {
        rb_interrupt_requested = 0;
        rb_raise(rb_eInterrupt, "execution expired");
    }
}

// Interrupt Ruby execution — safe from any thread, no GVL needed.
// Sets a volatile flag that the trace hook checks on every Ruby line.
static void omnivm_ruby_interrupt(void) {
    rb_interrupt_requested = 1;
}

// Returns function pointer for the watchdog to call.
static void* omnivm_ruby_get_interrupt_ptr(void) {
    return (void*)omnivm_ruby_interrupt;
}

// Thread-safe exec: direct if we hold GVL, via rb_thread_call_with_gvl
// for Ruby-known threads, via proxy for foreign threads.
//
// In inline init mode (Ruby 3.3+), the calling Go thread IS the Ruby
// main thread and always holds the GVL. Foreign thread proxy is available
// if omnivm_ruby_start_proxy() was called.
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

typedef struct {
    VALUE object;
    int locked;
} rb_omni_exported_buffer_handle_t;

typedef struct {
    void* data;
    int64_t len;
    int32_t dtype;
    int64_t elements;
    const char* arrow_format;
    int8_t read_only;
    void* handle;
} rb_omni_exported_buffer_t;

typedef struct {
    const char* code;
    rb_omni_exported_buffer_t* out;
    int rc;
} ruby_export_buffer_args;

static VALUE ruby_export_eval_cb(VALUE raw) {
    ruby_export_buffer_args* args = (ruby_export_buffer_args*)raw;
    return rb_eval_string(args->code);
}

static VALUE ruby_lock_tmp_cb(VALUE value) {
    return rb_str_locktmp(value);
}

static VALUE ruby_unlock_tmp_cb(VALUE raw) {
    rb_omni_exported_buffer_handle_t* handle = (rb_omni_exported_buffer_handle_t*)raw;
    if (handle && handle->locked) rb_str_unlocktmp(handle->object);
    return Qnil;
}

static void* omnivm_ruby_export_buffer_run(void* raw) {
    ruby_export_buffer_args* args = (ruby_export_buffer_args*)raw;
    if (!args) return NULL;
    args->rc = 1;
    if (!args->out || !args->code) return NULL;
    memset(args->out, 0, sizeof(*args->out));

    int state = 0;
    VALUE obj = rb_protect(ruby_export_eval_cb, (VALUE)args, &state);
    if (state) {
        rb_set_errinfo(Qnil);
        return NULL;
    }

    VALUE str = rb_check_string_type(obj);
    if (NIL_P(str)) return NULL;
    if (rb_enc_get(str) != rb_ascii8bit_encoding()) return NULL;

    rb_omni_exported_buffer_handle_t* handle =
        (rb_omni_exported_buffer_handle_t*)calloc(1, sizeof(rb_omni_exported_buffer_handle_t));
    if (!handle) {
        args->rc = -1;
        return NULL;
    }
    handle->object = Qnil;
    rb_gc_register_address(&handle->object);
    handle->object = str;

    state = 0;
    rb_protect(ruby_lock_tmp_cb, str, &state);
    if (state) {
        rb_set_errinfo(Qnil);
        rb_gc_unregister_address(&handle->object);
        free(handle);
        return NULL;
    }
    handle->locked = 1;

    args->out->data = (void*)RSTRING_PTR(str);
    args->out->len = (int64_t)RSTRING_LEN(str);
    args->out->dtype = 0;
    args->out->elements = (int64_t)RSTRING_LEN(str);
    args->out->arrow_format = "C";
    args->out->read_only = 1;
    args->out->handle = handle;
    args->rc = 0;
    return NULL;
}

static int omnivm_ruby_export_buffer_safe(const char* code, rb_omni_exported_buffer_t* out) {
    ruby_export_buffer_args args;
    args.code = code;
    args.out = out;
    args.rc = 1;
    if (tls_holds_gvl) {
        omnivm_ruby_export_buffer_run(&args);
    } else if (tls_is_ruby_thread) {
        rb_thread_call_with_gvl(omnivm_ruby_export_buffer_run, &args);
    } else {
        rb_fproxy_submit_callback(omnivm_ruby_export_buffer_run, &args);
    }
    return args.rc;
}

static void* omnivm_ruby_release_exported_buffer_run(void* raw) {
    rb_omni_exported_buffer_handle_t* handle = (rb_omni_exported_buffer_handle_t*)raw;
    if (!handle) return NULL;
    int state = 0;
    rb_protect(ruby_unlock_tmp_cb, (VALUE)handle, &state);
    if (state) rb_set_errinfo(Qnil);
    rb_gc_unregister_address(&handle->object);
    free(handle);
    return NULL;
}

static void omnivm_ruby_release_exported_buffer_safe(void* handle) {
    if (!handle) return;
    if (tls_holds_gvl) {
        omnivm_ruby_release_exported_buffer_run(handle);
    } else if (tls_is_ruby_thread) {
        rb_thread_call_with_gvl(omnivm_ruby_release_exported_buffer_run, handle);
    } else {
        rb_fproxy_submit_callback(omnivm_ruby_release_exported_buffer_run, handle);
    }
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
    rb_define_method(rb_mKernel, "class", omnivm_ruby_object_class, 0);
    rb_define_method(rb_mKernel, "clone", omnivm_ruby_object_clone, 0);
    rb_define_method(rb_mKernel, "dup", omnivm_ruby_object_clone, 0);

    // Load internal preludes so core methods like Integer#times exist.
    // Ruby 3.3 defines many core methods in <internal:numeric> etc.
    // ruby_init() alone doesn't load them. ruby_options() loads them but
    // breaks the watchdog interrupt mechanism. Instead, polyfill the
    // methods we need using rb_eval_string_protect.
    {
        int state = 0;
        rb_eval_string_protect(
            "class Integer\n"
            "  def to_i\n"
            "    self\n"
            "  end\n"
            "  def times\n"
            "    return to_enum(:times) { self } unless block_given?\n"
            "    i = 0\n"
            "    while i < self\n"
            "      yield i\n"
            "      i += 1\n"
            "    end\n"
            "    self\n"
            "  end\n"
            "  def upto(max)\n"
            "    return to_enum(:upto, max) unless block_given?\n"
            "    i = self\n"
            "    while i <= max\n"
            "      yield i\n"
            "      i += 1\n"
            "    end\n"
            "    self\n"
            "  end\n"
            "  def downto(min)\n"
            "    return to_enum(:downto, min) unless block_given?\n"
            "    i = self\n"
            "    while i >= min\n"
            "      yield i\n"
            "      i -= 1\n"
            "    end\n"
            "    self\n"
            "  end\n"
            "end\n"
            "class Array\n"
            "  def last(n = nil)\n"
            "    if n.nil?\n"
            "      self[length - 1]\n"
            "    else\n"
            "      start = length - n\n"
            "      start = 0 if start < 0\n"
            "      self[start, n]\n"
            "    end\n"
            "  end\n"
            "end\n"
            "class Set\n"
            "  def initialize(enum = [])\n"
            "    @values = []\n"
            "    i = 0\n"
            "    while i < enum.length\n"
            "      @values << enum[i]\n"
            "      i += 1\n"
            "    end\n"
            "  end\n"
            "  def include?(value)\n"
            "    i = 0\n"
            "    while i < @values.length\n"
            "      return true if @values[i] == value\n"
            "      i += 1\n"
            "    end\n"
            "    false\n"
            "  end\n"
            "end unless defined?(Set)\n"
            "class Symbol\n"
            "  def to_sym\n"
            "    self\n"
            "  end\n"
            "end\n"
            "if defined?(Ractor)\n"
            "  class Ractor\n"
            "    def self.make_shareable(obj, copy: false)\n"
            "      obj\n"
            "    end\n"
            "  end\n"
            "end\n"
            "RUBY_DESCRIPTION = \"ruby #{RUBY_VERSION}\" unless defined?(RUBY_DESCRIPTION)\n"
            "module Kernel\n"
            "  def tap\n"
            "    yield self if block_given?\n"
            "    self\n"
            "  end\n"
            "  def loop\n"
            "    return to_enum(:loop) unless block_given?\n"
            "    begin\n"
            "      while true\n"
            "        yield\n"
            "      end\n"
            "    rescue StopIteration => e\n"
            "      e.result\n"
            "    end\n"
            "  end\n"
            "  module_function :loop\n"
            "end\n",
            &state
        );
    }

    {
        int state = 0;
        rb_eval_string_protect(
            "begin\n"
            "  require 'enc/encdb'\n"
            "  require 'rubygems'\n"
            "  require 'set'\n"
            "rescue LoadError\n"
            "end\n",
            &state
        );
    }

    // Install trace hook for interrupt delivery. The hook fires on every
    // Ruby line event and checks the volatile interrupt flag.
    rb_add_event_hook(rb_interrupt_trace_hook, RUBY_EVENT_LINE, Qnil);

    // Fix Ruby's signal handlers to include SA_ONSTACK (required by Go)
    fix_signal_handlers_sa_onstack();
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

// Typed eval: returns rb_omni_value_t preserving native Ruby types.
static rb_omni_value_t omnivm_ruby_eval_typed(const char* code) {
    rb_omni_value_t null_val;
    memset(&null_val, 0, sizeof(null_val));

    if (!ruby_initialized) {
        rb_omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = RB_OMNI_TAG_ERROR;
        err.v.s.ptr = strdup("ruby not initialized");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }

    int state = 0;
    VALUE result = rb_eval_string_protect(code, &state);

    if (state) {
        VALUE exception = rb_errinfo();
        rb_set_errinfo(Qnil);
        rb_omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = RB_OMNI_TAG_ERROR;
        if (exception != Qnil) {
            char* msg = omnivm_ruby_safe_error_msg(exception);
            err.v.s.ptr = msg ? msg : strdup("ruby error");
        } else {
            err.v.s.ptr = strdup("ruby eval error");
        }
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }

    return rb_to_omni_value(result);
}

// GVL wrapper for typed eval
typedef struct {
    const char* code;
    rb_omni_value_t result;
} ruby_typed_eval_args;

static void* omnivm_ruby_eval_typed_with_gvl(void* raw) {
    ruby_typed_eval_args* a = (ruby_typed_eval_args*)raw;
    a->result = omnivm_ruby_eval_typed(a->code);
    return NULL;
}

static rb_omni_value_t omnivm_ruby_eval_typed_safe(const char* code) {
    if (tls_holds_gvl) return omnivm_ruby_eval_typed(code);
    ruby_typed_eval_args args = { .code = code };
    memset(&args.result, 0, sizeof(args.result));
    rb_thread_call_with_gvl(omnivm_ruby_eval_typed_with_gvl, &args);
    return args.result;
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

// Typed call bridge args for GVL release
typedef struct {
    const char* runtime;
    const char* func_name;
    rb_omni_value_t* args;
    int32_t nargs;
    rb_omni_value_t result;
} ruby_typed_bridge_args;

static void* ruby_typed_bridge_no_gvl(void* raw) {
    tls_holds_gvl = 0;
    ruby_typed_bridge_args* a = (ruby_typed_bridge_args*)raw;
    a->result = g_call_typed(a->runtime, a->func_name, a->args, a->nargs);
    tls_holds_gvl = 1;
    return NULL;
}

// OmniVM.call_typed(runtime, func_name, *args) -> typed value
static VALUE rb_omnivm_call_typed(int argc, VALUE* argv, VALUE self) {
    if (argc < 2) {
        rb_raise(rb_eArgError, "call_typed requires at least 2 arguments (runtime, func_name)");
        return Qnil;
    }
    if (!g_call_typed) {
        rb_raise(rb_eRuntimeError, "omnivm typed bridge not initialized");
        return Qnil;
    }

    const char* runtime = StringValueCStr(argv[0]);
    const char* func_name = StringValueCStr(argv[1]);

    int32_t nargs = argc - 2;
    rb_omni_value_t* c_args = NULL;
    if (nargs > 0) {
        c_args = (rb_omni_value_t*)calloc(nargs, sizeof(rb_omni_value_t));
        for (int32_t i = 0; i < nargs; i++) {
            c_args[i] = rb_to_omni_value(argv[i + 2]);
            if (c_args[i].tag == RB_OMNI_TAG_ERROR) {
                for (int32_t j = 0; j <= i; j++) {
                    rb_free_omni_value(&c_args[j]);
                }
                free(c_args);
                rb_raise(rb_eTypeError, "unsupported typed bridge argument; complex values must cross through the manifest proxy/Arrow boundary, not implicit stringification");
                return Qnil;
            }
        }
    }

    ruby_typed_bridge_args bargs;
    bargs.runtime = runtime;
    bargs.func_name = func_name;
    bargs.args = c_args;
    bargs.nargs = nargs;
    memset(&bargs.result, 0, sizeof(bargs.result));

    rb_thread_call_without_gvl(ruby_typed_bridge_no_gvl, &bargs, RUBY_UBF_IO, NULL);

    // Free args
    if (c_args) {
        for (int32_t i = 0; i < nargs; i++) {
            rb_free_omni_value(&c_args[i]);
        }
        free(c_args);
    }

    VALUE result = omni_value_to_rb(bargs.result);
    rb_free_omni_value(&bargs.result);
    return result;
}

// OmniVM.get_buffer(name) -> String or nil
static VALUE rb_omnivm_get_buffer(VALUE self, VALUE rb_name) {
    const char* name = StringValueCStr(rb_name);
    if (!g_buf_get) {
        rb_raise(rb_eRuntimeError, "omnivm buffer bridge not initialized");
        return Qnil;
    }

    rb_omni_buffer_t buf;
    memset(&buf, 0, sizeof(buf));
    int rc = g_buf_get(name, &buf);
    if (rc != 0) return Qnil;
    if (buf.data == NULL || buf.len <= 0) {
        if (g_buf_release) g_buf_release(name);
        return rb_str_new("", 0);
    }
    // Return a frozen binary string (copy from shared memory)
    VALUE str = rb_str_new((const char*)buf.data, (long)buf.len);
    rb_enc_associate(str, rb_ascii8bit_encoding());
    if (g_buf_release) g_buf_release(name);
    return str;
}

// OmniVM.set_buffer(name, data, dtype=0)
static VALUE rb_omnivm_set_buffer(int argc, VALUE* argv, VALUE self) {
    VALUE rb_name, rb_data, rb_dtype;
    rb_scan_args(argc, argv, "21", &rb_name, &rb_data, &rb_dtype);

    if (!g_buf_set) {
        rb_raise(rb_eRuntimeError, "omnivm buffer bridge not initialized");
        return Qnil;
    }

    const char* name = StringValueCStr(rb_name);
    StringValue(rb_data);

    rb_omni_buffer_t buf;
    buf.data = (void*)RSTRING_PTR(rb_data);
    buf.len = (int64_t)RSTRING_LEN(rb_data);
    buf.dtype = NIL_P(rb_dtype) ? 0 : (int32_t)NUM2INT(rb_dtype);
    buf.owned = 0;
    buf.read_only = 0;

    int rc = g_buf_set(name, buf);
    if (rc != 0) {
        rb_raise(rb_eRuntimeError, "OmniVM.set_buffer failed");
    }
    return Qnil;
}

// OmniVM.release_buffer(name)
static VALUE rb_omnivm_release_buffer(VALUE self, VALUE rb_name) {
    if (!g_buf_release) return Qnil;
    const char* name = StringValueCStr(rb_name);
    g_buf_release(name);
    return Qnil;
}

// Register OmniVM module with call() and buffer methods
static void omnivm_ruby_register_bridge() {
    VALUE mod = rb_define_module("OmniVM");
    rb_define_module_function(mod, "call", rb_omnivm_call, 2);
    rb_define_module_function(mod, "call_typed", rb_omnivm_call_typed, -1);
    rb_define_module_function(mod, "get_buffer", rb_omnivm_get_buffer, 1);
    rb_define_module_function(mod, "set_buffer", rb_omnivm_set_buffer, -1);
    rb_define_module_function(mod, "release_buffer", rb_omnivm_release_buffer, 1);
}

static void omnivm_ruby_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

static void omnivm_ruby_shutdown(void) {
    if (ruby_initialized) {
        // Signal the foreign-thread proxy to shut down and wake any
        // foreign thread blocked in rb_fproxy_submit's wait loop.
        pthread_mutex_lock(&rb_fproxy_mtx);
        rb_fproxy_shutdown = 1;
        pthread_cond_signal(&rb_fproxy_req_cv);
        pthread_cond_broadcast(&rb_fproxy_res_cv);
        pthread_mutex_unlock(&rb_fproxy_mtx);

        // NOTE: We intentionally do NOT call ruby_cleanup() here.
        // In a polyglot process, ruby_cleanup() calls rb_thread_terminate_all()
        // which sends signals to terminate threads. When the JVM is also running,
        // its threads interfere with Ruby's signal-based cleanup and cause a
        // segfault. Since the process is exiting, the OS will reclaim all
        // resources. This is safe and matches how other embedded Ruby hosts
        // (e.g., Apache mod_ruby) handle shutdown.
        // NOTE: We keep ruby_initialized = 1 because the Ruby VM stays
        // alive (no ruby_cleanup). The Go-level rubyInitialized guard
        // handles reattach on next Initialize() call.
    }
}
*/
import "C"
import (
	"fmt"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/polyglot"
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

// rubyInitialized guards against double Ruby init per process.
// ruby_init() cannot be called twice — the proxy thread and GVL state
// become corrupted. Since Shutdown() skips ruby_cleanup(), the VM stays live.
var rubyInitialized bool

// Initialize starts the Ruby VM on the calling thread.
// The calling goroutine must be locked to an OS thread (runtime.LockOSThread).
// After init, the calling thread holds the GVL and direct Ruby calls work.
func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("ruby: already initialized")
	}

	if rubyInitialized {
		// Ruby VM still alive from previous Runtime — just reattach.
		// The proxy pthread is still running (shutdown is a no-op).
		r.initialized = true
		return nil
	}

	rc := C.omnivm_ruby_init_on_thread()
	if rc != 0 {
		return fmt.Errorf("ruby: initialization failed")
	}

	r.initialized = true
	rubyInitialized = true
	return nil
}

// InterruptFuncPtr returns a C function pointer that the watchdog can call
// to interrupt Ruby execution. Sets a volatile flag that a Ruby trace hook
// checks on every line event, raising Interrupt when set.
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

// ExportBuffer publishes a Ruby String-compatible value into OmniVM's shared
// data plane without copying. It uses Ruby's generic to_str protocol, pins the
// backing object against GC movement, and exposes the memory as read-only.
func (r *Runtime) ExportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	if !r.initialized {
		return pkg.ExportedBuffer{}, false, fmt.Errorf("ruby: not initialized")
	}

	cExpr := C.CString(expr)
	defer C.free(unsafe.Pointer(cExpr))

	var out C.rb_omni_exported_buffer_t
	rc := C.omnivm_ruby_export_buffer_safe(cExpr, &out)
	if rc < 0 {
		return pkg.ExportedBuffer{}, false, fmt.Errorf("ruby: export buffer failed")
	}
	if rc > 0 {
		return pkg.ExportedBuffer{}, false, nil
	}

	byteLen := int64(out.len)
	elements := int64(out.elements)
	if byteLen < 0 || elements < 0 || (byteLen > 0 && out.data == nil) || out.handle == nil {
		C.omnivm_ruby_release_exported_buffer_safe(out.handle)
		return pkg.ExportedBuffer{}, false, fmt.Errorf("ruby: invalid exported buffer")
	}
	dtype := int32(out.dtype)
	arrowFormat := C.GoString(out.arrow_format)
	meta := arrow.BufferMetadata{
		Dtype:     dtype,
		Format:    arrowFormat,
		Shape:     []int64{elements},
		ReadOnly:  out.read_only != 0,
		Ownership: "producer",
	}
	if _, err := arrow.GlobalStore().SetExternalWithMetadata(name, unsafe.Pointer(out.data), byteLen, meta, func() error {
		C.omnivm_ruby_release_exported_buffer_safe(out.handle)
		return nil
	}); err != nil {
		C.omnivm_ruby_release_exported_buffer_safe(out.handle)
		return pkg.ExportedBuffer{}, false, err
	}
	return pkg.ExportedBuffer{
		Name:        name,
		Dtype:       dtype,
		ArrowFormat: arrowFormat,
		Elements:    elements,
		Shape:       []int64{elements},
		ReadOnly:    meta.ReadOnly,
	}, true, nil
}

// EvalTyped evaluates Ruby code and returns a typed polyglot.Value.
func (r *Runtime) EvalTyped(code string) polyglot.Value {
	if !r.initialized {
		return polyglot.Error("ruby: not initialized")
	}
	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cResult := C.omnivm_ruby_eval_typed_safe(cCode)
	ptr := unsafe.Pointer(&cResult)
	val := polyglot.FromCValueRaw(ptr)
	polyglot.FreeCValueRaw(ptr)
	return val
}

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_ruby_set_bridge_callback(
		C.omni_call_fn(unsafe.Pointer(callPtr)),
		C.omni_free_fn(unsafe.Pointer(freePtr)),
	)
}

// SetBufCallbacks installs the buffer bridge function pointers.
func (r *Runtime) SetBufCallbacks(getPtr, setPtr, releasePtr uintptr) {
	C.omnivm_ruby_set_buf_callbacks(
		C.omni_buf_get_fn(unsafe.Pointer(getPtr)),
		C.omni_buf_set_fn(unsafe.Pointer(setPtr)),
		C.omni_buf_release_fn(unsafe.Pointer(releasePtr)),
	)
}

// SetTypedCallback installs the typed call bridge function pointer.
func (r *Runtime) SetTypedCallback(ptr uintptr) {
	C.omnivm_ruby_set_typed_callback(C.rb_omni_call_typed_fn(unsafe.Pointer(ptr)))
}

// Pump is a no-op for Ruby (no cooperative event loop).
func (r *Runtime) Pump() {}

// Shutdown marks the Ruby runtime as inactive.
// The Ruby VM and proxy pthread stay alive — ruby_cleanup() is unsafe
// in a polyglot process, and the pthread can't be cleanly restarted.
// The proxy thread idles harmlessly when no requests are pending.
func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false
	// Don't call C.omnivm_ruby_shutdown() — the proxy pthread can't be
	// restarted without GVL, and ruby_cleanup crashes in polyglot process.
	return nil
}
