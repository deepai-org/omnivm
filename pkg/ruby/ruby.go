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
#include <stdio.h>
#include <string.h>
#include <pthread.h>
#include <unistd.h>
#include <signal.h>
#include <time.h>
#include <glob.h>

// Bridge callback pointer — set via omnivm_ruby_set_bridge_callback().
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;
static pthread_mutex_t g_ruby_bridge_call_mu = PTHREAD_MUTEX_INITIALIZER;

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
typedef int (*omni_buf_free_fn)(const char* name);

static omni_buf_get_fn g_buf_get = NULL;
static omni_buf_set_fn g_buf_set = NULL;
static omni_buf_release_fn g_buf_release = NULL;
static omni_buf_free_fn g_buf_free = NULL;

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
                                           omni_buf_release_fn release_fn,
                                           omni_buf_free_fn free_fn) {
    g_buf_get = get_fn;
    g_buf_set = set_fn;
    g_buf_release = release_fn;
    g_buf_free = free_fn;
}

static int ruby_initialized = 0;

static VALUE omnivm_ruby_object_class(VALUE self) {
    return rb_obj_class(self);
}

static VALUE omnivm_ruby_object_clone(VALUE self) {
    return rb_obj_clone(self);
}

static VALUE omnivm_ruby_object_frozen_p(VALUE self) {
    return rb_obj_frozen_p(self);
}

static VALUE omnivm_ruby_time_now(VALUE klass) {
    return rb_time_new(time(NULL), 0);
}

static VALUE omnivm_ruby_time_from_args(int argc, VALUE* argv, int utc) {
    if (argc == 0) {
        return rb_time_new(time(NULL), 0);
    }
    if (rb_obj_is_kind_of(argv[0], rb_cTime)) {
        return argv[0];
    }
    if (!rb_obj_is_kind_of(argv[0], rb_cInteger)) {
        return rb_time_new(0, 0);
    }

    struct tm tmv;
    memset(&tmv, 0, sizeof(tmv));
    tmv.tm_year = NUM2INT(argv[0]) - 1900;
    tmv.tm_mon = argc > 1 && rb_obj_is_kind_of(argv[1], rb_cInteger) ? NUM2INT(argv[1]) - 1 : 0;
    tmv.tm_mday = argc > 2 && rb_obj_is_kind_of(argv[2], rb_cInteger) ? NUM2INT(argv[2]) : 1;
    tmv.tm_hour = argc > 3 && rb_obj_is_kind_of(argv[3], rb_cInteger) ? NUM2INT(argv[3]) : 0;
    tmv.tm_min = argc > 4 && rb_obj_is_kind_of(argv[4], rb_cInteger) ? NUM2INT(argv[4]) : 0;
    tmv.tm_sec = argc > 5 && rb_obj_is_kind_of(argv[5], rb_cInteger) ? NUM2INT(argv[5]) : 0;
    tmv.tm_isdst = -1;

    time_t sec;
#ifdef _GNU_SOURCE
    sec = utc ? timegm(&tmv) : mktime(&tmv);
#else
    sec = mktime(&tmv);
#endif
    return rb_time_new(sec, 0);
}

static VALUE omnivm_ruby_time_new(int argc, VALUE* argv, VALUE klass) {
    return omnivm_ruby_time_from_args(argc, argv, 0);
}

static VALUE omnivm_ruby_time_at(int argc, VALUE* argv, VALUE klass) {
    time_t sec = 0;
    long usec = 0;
    if (argc > 0 && rb_obj_is_kind_of(argv[0], rb_cTime)) {
        return argv[0];
    }
    if (argc > 0) sec = NUM2LONG(argv[0]);
    if (argc > 1) usec = NUM2LONG(argv[1]);
    return rb_time_new(sec, usec);
}

static VALUE omnivm_ruby_time_utc(int argc, VALUE* argv, VALUE klass) {
    return omnivm_ruby_time_from_args(argc, argv, 1);
}

static VALUE omnivm_ruby_dir_glob(int argc, VALUE* argv, VALUE klass) {
    VALUE ary = rb_ary_new();
    if (argc == 0) return ary;

    const char* pattern = StringValueCStr(argv[0]);
    glob_t matches;
    memset(&matches, 0, sizeof(matches));
    int rc = glob(pattern, 0, NULL, &matches);
    if (rc == 0) {
        for (size_t i = 0; i < matches.gl_pathc; i++) {
            rb_ary_push(ary, rb_str_new_cstr(matches.gl_pathv[i]));
        }
    }
    globfree(&matches);
    return ary;
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

// rb_protect callback: returns the first backtrace frame, if available.
static VALUE call_exception_backtrace_first(VALUE exception) {
    VALUE backtrace = rb_funcall(exception, rb_intern("backtrace"), 0);
    if (backtrace == Qnil || !RB_TYPE_P(backtrace, T_ARRAY) || RARRAY_LEN(backtrace) == 0) {
        return Qnil;
    }
    return rb_obj_as_string(rb_ary_entry(backtrace, 0));
}

// rb_protect callback: formats the complete Ruby backtrace for cross-runtime parsers.
static VALUE call_exception_backtrace_text(VALUE exception) {
    VALUE backtrace = rb_funcall(exception, rb_intern("backtrace"), 0);
    if (backtrace == Qnil || !RB_TYPE_P(backtrace, T_ARRAY) || RARRAY_LEN(backtrace) == 0) {
        return Qnil;
    }

    VALUE text = rb_str_new_cstr("Traceback (most recent call last):\n");
    long count = RARRAY_LEN(backtrace);
    for (long i = 0; i < count; i++) {
        VALUE frame = rb_obj_as_string(rb_ary_entry(backtrace, i));
        rb_str_cat_cstr(text, "  from ");
        rb_str_append(text, frame);
        rb_str_cat_cstr(text, "\n");
    }

    rb_str_append(text, call_exception_class_name(exception));
    rb_str_cat_cstr(text, ": ");
    rb_str_append(text, rb_obj_as_string(call_exception_message(exception)));
    return text;
}

static VALUE call_exception_omnivm_bridge_error_text(VALUE exception) {
    ID method = rb_intern("__omnivm_bridge_error_text");
    if (!rb_respond_to(exception, method)) {
        return Qnil;
    }
    return rb_obj_as_string(rb_funcall(exception, method, 0));
}

static VALUE call_exception_original_error_handle(VALUE exception) {
    ID method = rb_intern("original_error_handle");
    if (rb_respond_to(exception, method)) {
        VALUE value = rb_funcall(exception, method, 0);
        if (value != Qnil) {
            return rb_obj_as_string(value);
        }
    }

    VALUE value = rb_ivar_get(exception, rb_intern("@original_error_handle"));
    if (value == Qnil) {
        return Qnil;
    }
    return rb_obj_as_string(value);
}

static VALUE call_exception_error_details_json(VALUE exception) {
    ID mod_id = rb_intern("OmniVM");
    if (!rb_const_defined(rb_cObject, mod_id)) {
        return Qnil;
    }
    VALUE mod = rb_const_get(rb_cObject, mod_id);
    ID method = rb_intern("__error_details_json");
    if (!rb_respond_to(mod, method)) {
        return Qnil;
    }
    VALUE value = rb_funcall(mod, method, 1, exception);
    if (value == Qnil) {
        return Qnil;
    }
    return rb_obj_as_string(value);
}

// Safe helper to extract exception message using rb_protect.
// Catches any secondary exceptions during message extraction (e.g., rb_exc_raise
// in rb_funcall which crashes on ARM64 when JVM is active).
// Returns a malloc'd string like "RubyError: ClassName: message" (caller must free).
static char* omnivm_ruby_safe_error_msg(VALUE exception) {
    int inner_state = 0;
    const char* klass_cstr = "UnknownError";
    const char* msg_cstr = "unknown error";
    const char* frame_cstr = NULL;
    const char* traceback_cstr = NULL;
    const char* handle_cstr = NULL;
    const char* details_cstr = NULL;

    VALUE bridge_text = rb_protect(call_exception_omnivm_bridge_error_text, exception, &inner_state);
    if (!inner_state && bridge_text != Qnil) {
        const char* bridge_cstr = StringValueCStr(bridge_text);
        size_t len = strlen(bridge_cstr) + 12;
        char* err = (char*)malloc(len);
        snprintf(err, len, "RubyError: %s", bridge_cstr);
        return err;
    } else if (inner_state) {
        rb_set_errinfo(Qnil);
    }

    // Try to get class name safely
    inner_state = 0;
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

    inner_state = 0;
    VALUE frame = rb_protect(call_exception_backtrace_first, exception, &inner_state);
    if (!inner_state && frame != Qnil) {
        frame_cstr = StringValueCStr(frame);
    } else if (inner_state) {
        rb_set_errinfo(Qnil); // Clear secondary error
    }

    inner_state = 0;
    VALUE traceback = rb_protect(call_exception_backtrace_text, exception, &inner_state);
    if (!inner_state && traceback != Qnil) {
        traceback_cstr = StringValueCStr(traceback);
    } else if (inner_state) {
        rb_set_errinfo(Qnil); // Clear secondary error
    }

    inner_state = 0;
    VALUE handle = rb_protect(call_exception_original_error_handle, exception, &inner_state);
    if (!inner_state && handle != Qnil) {
        handle_cstr = StringValueCStr(handle);
    } else if (inner_state) {
        rb_set_errinfo(Qnil); // Clear secondary error
    }

    inner_state = 0;
    VALUE details = rb_protect(call_exception_error_details_json, exception, &inner_state);
    if (!inner_state && details != Qnil) {
        details_cstr = StringValueCStr(details);
    } else if (inner_state) {
        rb_set_errinfo(Qnil); // Clear secondary error
    }

    size_t len = strlen("RubyError: ") + strlen(klass_cstr) + strlen(": ") + strlen(msg_cstr) + 1;
    if (frame_cstr) len += strlen(" (at )") + strlen(frame_cstr);
    if (traceback_cstr) len += strlen("\n") + strlen(traceback_cstr);
    if (details_cstr) len += strlen("\nDetails: ") + strlen(details_cstr);
    if (handle_cstr) len += strlen("\nOriginal error handle: ") + strlen(handle_cstr);
    char* err = (char*)malloc(len);
    if (frame_cstr && traceback_cstr) {
        snprintf(err, len, "RubyError: %s: %s (at %s)\n%s", klass_cstr, msg_cstr, frame_cstr, traceback_cstr);
    } else if (frame_cstr) {
        snprintf(err, len, "RubyError: %s: %s (at %s)", klass_cstr, msg_cstr, frame_cstr);
    } else if (traceback_cstr) {
        snprintf(err, len, "RubyError: %s: %s\n%s", klass_cstr, msg_cstr, traceback_cstr);
    } else {
        snprintf(err, len, "RubyError: %s: %s", klass_cstr, msg_cstr);
    }
    if (handle_cstr) {
        if (details_cstr) {
            strcat(err, "\nDetails: ");
            strcat(err, details_cstr);
        }
        strcat(err, "\nOriginal error handle: ");
        strcat(err, handle_cstr);
    } else if (details_cstr) {
        strcat(err, "\nDetails: ");
        strcat(err, details_cstr);
    }
    return err;
}

static int omnivm_ruby_eval_bootstrap(const char* label, const char* code) {
    int state = 0;
    VALUE result = rb_eval_string_protect(code, &state);
    (void)result;
    if (!state) return 0;

    VALUE exception = rb_errinfo();
    rb_set_errinfo(Qnil);
    char* msg = NULL;
    if (exception != Qnil) {
        msg = omnivm_ruby_safe_error_msg(exception);
    } else {
        msg = strdup("RubyError: unknown error");
    }
    fprintf(stderr, "OmniVM Ruby bootstrap failed in %s: %s\n", label, msg);
    free(msg);
    return -1;
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
    rb_define_method(rb_cObject, "frozen?", omnivm_ruby_object_frozen_p, 0);
    rb_define_singleton_method(rb_cTime, "now", omnivm_ruby_time_now, 0);
    rb_define_singleton_method(rb_cTime, "new", omnivm_ruby_time_new, -1);
    rb_define_singleton_method(rb_cTime, "at", omnivm_ruby_time_at, -1);
    rb_define_singleton_method(rb_cTime, "utc", omnivm_ruby_time_utc, -1);
    rb_define_singleton_method(rb_cTime, "local", omnivm_ruby_time_new, -1);
    rb_define_singleton_method(rb_cDir, "glob", omnivm_ruby_dir_glob, -1);
    rb_define_singleton_method(rb_cDir, "[]", omnivm_ruby_dir_glob, -1);

    // Load internal preludes so core methods like Integer#times exist.
    // Ruby 3.3 defines many core methods in <internal:numeric> etc.
    // ruby_init() alone doesn't load them. ruby_options() loads them but
    // breaks the watchdog interrupt mechanism. Instead, polyfill the
    // methods we need using rb_eval_string_protect.
    if (omnivm_ruby_eval_bootstrap("core prelude",
        "class Integer\n"
        "  def to_i\n"
        "    self\n"
        "  end\n"
        "  def to_f\n"
        "    Float(self)\n"
        "  end unless method_defined?(:to_f)\n"
        "  def size\n"
        "    8\n"
        "  end\n"
        "  def ~\n"
        "    -self - 1\n"
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
        "class NilClass\n"
        "  def to_i\n"
        "    0\n"
        "  end\n"
        "end\n"
        "class Float\n"
        "  def to_f\n"
        "    self\n"
        "  end unless method_defined?(:to_f)\n"
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
        "class String\n"
        "  def __omnivm_unpack_hex(format, offset)\n"
        "    source = offset && offset > 0 ? self[offset..-1].to_s : self\n"
        "    out = ''\n"
        "    source.each_byte do |byte|\n"
        "      hi = (byte >> 4).to_s(16)\n"
        "      lo = (byte & 15).to_s(16)\n"
        "      out << (format.start_with?('h') ? lo + hi : hi + lo)\n"
        "    end\n"
        "    out\n"
        "  end unless method_defined?(:__omnivm_unpack_hex)\n"
        "  def unpack(format, offset: 0)\n"
        "    if format == 'H*' || format == 'h*'\n"
        "      [__omnivm_unpack_hex(format, offset)]\n"
        "    else\n"
        "      raise NotImplementedError, \"String#unpack format #{format.inspect} is not available\"\n"
        "    end\n"
        "  end unless method_defined?(:unpack)\n"
        "  def unpack1(format, offset: 0)\n"
        "    unpack(format, offset: offset).first\n"
        "  end unless method_defined?(:unpack1)\n"
        "end\n"
        "class Dir\n"
        "  class << self\n"
        "    alias __omnivm_native_glob glob unless method_defined?(:__omnivm_native_glob)\n"
        "    def glob(pattern, flags = 0, base: nil, sort: true)\n"
        "      if base\n"
        "        expanded_base = File.expand_path(base)\n"
        "        prefix = expanded_base.end_with?('/') ? expanded_base : expanded_base + '/'\n"
        "        __omnivm_native_glob(File.join(expanded_base, pattern), flags).map do |path|\n"
        "          expanded_path = File.expand_path(path)\n"
        "          expanded_path.start_with?(prefix) ? expanded_path[prefix.length..-1] : expanded_path\n"
        "        end\n"
        "      else\n"
        "        __omnivm_native_glob(pattern, flags)\n"
        "      end\n"
        "    end\n"
        "  end\n"
        "  def self.[](pattern, *flags)\n"
        "    glob(pattern, *flags)\n"
        "  end\n"
        "end\n"
        "class IO\n"
        "  def readline(*args)\n"
        "    line = gets(*args)\n"
        "    raise EOFError, 'end of file reached' if line.nil?\n"
        "    line\n"
        "  end unless public_method_defined?(:readline)\n"
        "  public :readline if private_method_defined?(:readline)\n"
        "end\n"
        "if defined?(Ractor)\n"
        "  class Ractor\n"
        "    def self.current\n"
        "      @__omnivm_current ||= {}\n"
        "    end unless respond_to?(:current)\n"
        "    def self.make_shareable(obj, copy: false)\n"
        "      obj\n"
        "    end\n"
        "  end\n"
        "end\n"
        "if defined?(::Thread) && defined?(::Thread::Queue) && defined?(::Thread::SizedQueue)\n"
        "  module OmniVMRubyCompat\n"
        "    def self.queue_storage(queue)\n"
        "      queue.instance_variable_get(:@__omnivm_queue_items) || queue.instance_variable_set(:@__omnivm_queue_items, [])\n"
        "    end\n"
        "  end\n"
        "  class ::Thread::Queue\n"
        "    def initialize\n"
        "      @__omnivm_queue_items = []\n"
        "      @__omnivm_queue_closed = false\n"
        "    end\n"
        "    def push(value)\n"
        "      raise ClosedQueueError, 'queue closed' if @__omnivm_queue_closed && defined?(ClosedQueueError)\n"
        "      raise ThreadError, 'queue closed' if @__omnivm_queue_closed\n"
        "      OmniVMRubyCompat.queue_storage(self) << value\n"
        "      self\n"
        "    end\n"
        "    alias << push\n"
        "    alias enq push\n"
        "    def pop(non_block = false)\n"
        "      items = OmniVMRubyCompat.queue_storage(self)\n"
        "      return items.shift unless items.empty?\n"
        "      return nil if @__omnivm_queue_closed\n"
        "      raise ThreadError, 'queue empty' if non_block\n"
        "      raise ThreadError, 'Ruby Queue#pop would block in OmniVM embedded Ruby; Ruby executes on a single VM-owned thread'\n"
        "    end\n"
        "    alias deq pop\n"
        "    alias shift pop\n"
        "    def clear\n"
        "      OmniVMRubyCompat.queue_storage(self).clear\n"
        "      self\n"
        "    end\n"
        "    def close\n"
        "      @__omnivm_queue_closed = true\n"
        "      self\n"
        "    end\n"
        "    def closed?\n"
        "      !!@__omnivm_queue_closed\n"
        "    end\n"
        "    def empty?\n"
        "      OmniVMRubyCompat.queue_storage(self).empty?\n"
        "    end\n"
        "    def length\n"
        "      OmniVMRubyCompat.queue_storage(self).length\n"
        "    end\n"
        "    alias size length\n"
        "    def num_waiting\n"
        "      0\n"
        "    end\n"
        "    def marshal_dump\n"
        "      raise TypeError, \"can't dump Thread::Queue\"\n"
        "    end\n"
        "  end\n"
        "  class ::Thread::SizedQueue < ::Thread::Queue\n"
        "    def initialize(max)\n"
        "      super()\n"
        "      @__omnivm_queue_max = Integer(max)\n"
        "      raise ArgumentError, 'queue size must be positive' if @__omnivm_queue_max <= 0\n"
        "    end\n"
        "    def push(value)\n"
        "      raise ClosedQueueError, 'queue closed' if @__omnivm_queue_closed && defined?(ClosedQueueError)\n"
        "      raise ThreadError, 'queue closed' if @__omnivm_queue_closed\n"
        "      if OmniVMRubyCompat.queue_storage(self).length >= @__omnivm_queue_max\n"
        "        raise ThreadError, 'Ruby SizedQueue#push would block in OmniVM embedded Ruby; Ruby executes on a single VM-owned thread'\n"
        "      end\n"
        "      OmniVMRubyCompat.queue_storage(self) << value\n"
        "      self\n"
        "    end\n"
        "    alias << push\n"
        "    alias enq push\n"
        "    def clear\n"
        "      OmniVMRubyCompat.queue_storage(self).clear\n"
        "      self\n"
        "    end\n"
        "    def close\n"
        "      @__omnivm_queue_closed = true\n"
        "      self\n"
        "    end\n"
        "    def empty?\n"
        "      OmniVMRubyCompat.queue_storage(self).empty?\n"
        "    end\n"
        "    def length\n"
        "      OmniVMRubyCompat.queue_storage(self).length\n"
        "    end\n"
        "    alias size length\n"
        "    def num_waiting\n"
        "      0\n"
        "    end\n"
        "    def max\n"
        "      @__omnivm_queue_max\n"
        "    end\n"
        "    def max=(value)\n"
        "      next_max = Integer(value)\n"
        "      raise ArgumentError, 'queue size must be positive' if next_max <= 0\n"
        "      @__omnivm_queue_max = next_max\n"
        "    end\n"
        "  end\n"
        "end\n"
        "module GC\n"
        "  def self.stat(hash = nil)\n"
        "    stats = {heap_live_slots: 0, total_allocated_objects: 0, total_freed_objects: 0}\n"
        "    if hash\n"
        "      stats.each { |k, v| hash[k] = v }\n"
        "      hash\n"
        "    else\n"
        "      stats\n"
        "    end\n"
        "  end unless respond_to?(:stat)\n"
        "end\n"
        "RUBY_DESCRIPTION = \"ruby #{RUBY_VERSION}\" unless defined?(RUBY_DESCRIPTION)\n"
        "module Kernel\n"
        "  def tap\n"
        "    yield self if block_given?\n"
        "    self\n"
        "  end\n"
        "  def then\n"
        "    return to_enum(:then) unless block_given?\n"
        "    yield self\n"
        "  end unless method_defined?(:then)\n"
        "  alias yield_self then unless method_defined?(:yield_self)\n"
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
        "  def Integer(value, base = nil, exception: true)\n"
        "    begin\n"
        "      return value if value.is_a?(::Integer)\n"
        "      if value.respond_to?(:to_int)\n"
        "        converted = value.to_int\n"
        "        return converted if converted.is_a?(::Integer)\n"
        "      end\n"
        "      if value.is_a?(::String)\n"
        "        text = value.strip\n"
        "        if base.nil? || base == 10\n"
        "          raise ArgumentError, \"invalid value for Integer(): #{value.inspect}\" unless text =~ /\\A[+-]?[0-9]+\\z/\n"
        "          return text.to_i\n"
        "        end\n"
        "        converted = text.to_i(base)\n"
        "        raise ArgumentError, \"invalid value for Integer(): #{value.inspect}\" if converted == 0 && !(text =~ /\\A[+-]?0+\\z/)\n"
        "        return converted\n"
        "      end\n"
        "      if value.respond_to?(:to_i)\n"
        "        converted = value.to_i\n"
        "        return converted if converted.is_a?(::Integer)\n"
        "      end\n"
        "      raise TypeError, \"can't convert #{value.class} into Integer\"\n"
        "    rescue Exception => e\n"
        "      return nil unless exception\n"
        "      raise e\n"
        "    end\n"
        "  end unless method_defined?(:Integer)\n"
        "  module_function :Integer\n"
        "  def Float(value, exception: true)\n"
        "    begin\n"
        "      return value if value.is_a?(::Float)\n"
        "      if value.is_a?(::String)\n"
        "        text = value.strip\n"
        "        unless text =~ /\\A[+-]?(?:[0-9]+(?:\\.[0-9]*)?|\\.[0-9]+)(?:[eE][+-]?[0-9]+)?\\z/ || text =~ /\\A[+-]?(?:Infinity|NaN)\\z/i\n"
        "          raise ArgumentError, \"invalid value for Float(): #{value.inspect}\"\n"
        "        end\n"
        "        return text.to_f\n"
        "      end\n"
        "      if value.respond_to?(:to_f)\n"
        "        converted = value.to_f\n"
        "        return converted if converted.is_a?(::Float)\n"
        "      end\n"
        "      raise TypeError, \"can't convert #{value.class} into Float\"\n"
        "    rescue Exception => e\n"
        "      return nil unless exception\n"
        "      raise e\n"
        "    end\n"
        "  end unless method_defined?(:Float)\n"
        "  module_function :Float\n"
        "  def warn(*messages)\n"
        "    return nil if messages.empty?\n"
        "    $stderr.puts(*messages)\n"
        "    nil\n"
        "  end unless method_defined?(:warn)\n"
        "  module_function :warn\n"
        "end\n") != 0) {
        return -1;
    }

    if (omnivm_ruby_eval_bootstrap("dup compatibility",
        "if [1].freeze.dup.frozen?\n"
        "  class Array\n"
        "    alias __omnivm_native_dup dup unless method_defined?(:__omnivm_native_dup)\n"
        "    def dup\n"
        "      copy = []\n"
        "      each { |item| copy << item }\n"
        "      copy\n"
        "    end\n"
        "  end\n"
        "end\n"
        "if ({a: 1}.freeze.dup.frozen?)\n"
        "  class Hash\n"
        "    alias __omnivm_native_dup dup unless method_defined?(:__omnivm_native_dup)\n"
        "    def dup\n"
        "      copy = self.class.new\n"
        "      each { |key, value| copy[key] = value }\n"
        "      copy\n"
        "    end\n"
        "  end\n"
        "end\n") != 0) {
        return -1;
    }

    if (omnivm_ruby_eval_bootstrap("stdlib prelude",
        "begin\n"
        "  require 'enc/encdb'\n"
        "  require 'rubygems'\n"
        "  require 'json'\n"
        "  require 'set'\n"
        "rescue LoadError\n"
        "end\n") != 0) {
        return -1;
    }

    if (omnivm_ruby_eval_bootstrap("runtime error prelude",
        "module OmniVM\n"
        "  class RuntimeError < ::RuntimeError\n"
        "    attr_reader :runtime, :origin_runtime, :type, :traceback, :boundary_path, :original_error_handle\n"
        "    def initialize(message, runtime: nil, boundary_path: nil)\n"
        "      super(message)\n"
        "      parsed = OmniVM.__parse_runtime_error_text(message.to_s, runtime, boundary_path)\n"
        "      @runtime = parsed[:runtime]\n"
        "      @origin_runtime = parsed[:origin_runtime] || @runtime\n"
        "      @type = parsed[:type]\n"
        "      @message = parsed[:message]\n"
        "      @traceback = parsed[:traceback]\n"
        "      @stack_frames = parsed[:stack_frames]\n"
        "      @cause_chain = parsed[:cause_chain]\n"
        "      @boundary_path = parsed[:boundary_path]\n"
        "      @original_error_handle = parsed[:original_error_handle]\n"
        "      @details = parsed[:details]\n"
        "    end\n"
        "    def message\n"
        "      @message || super\n"
        "    end\n"
        "    def stack_frames\n"
        "      OmniVM.__copy_json_value(@stack_frames)\n"
        "    end\n"
        "    def cause_chain\n"
        "      OmniVM.__copy_json_value(@cause_chain)\n"
        "    end\n"
        "    def details\n"
        "      OmniVM.__copy_json_value(@details)\n"
        "    end\n"
        "    def to_h\n"
        "      {runtime: @runtime, origin_runtime: @origin_runtime, type: @type, message: message, traceback: @traceback, stack_frames: OmniVM.__copy_json_value(@stack_frames), cause_chain: OmniVM.__copy_json_value(@cause_chain), boundary_path: @boundary_path, original_error_handle: @original_error_handle, details: OmniVM.__copy_json_value(@details)}\n"
        "    end\n"
        "    alias to_dict to_h\n"
        "    def as_json(*_args)\n"
        "      to_h\n"
        "    end\n"
        "    def to_json(*args)\n"
        "      JSON.generate(to_h, *args)\n"
        "    end\n"
        "    def __omnivm_bridge_error_text\n"
        "      text = ''\n"
        "      text << \"#{@runtime}: \" if @runtime && !@runtime.empty?\n"
        "      text << \"#{@type}: \" if @type && !@type.empty?\n"
        "      text << message.to_s\n"
        "      text << \"\\n#{@traceback}\" if @traceback && !@traceback.empty?\n"
        "      Array(@cause_chain).each do |cause|\n"
        "        text << \"\\nCaused by: \"\n"
        "        ctype = cause[:type] || cause['type']\n"
        "        cmsg = cause[:message] || cause['message']\n"
        "        text << \"#{ctype}: \" if ctype && !ctype.to_s.empty?\n"
        "        text << cmsg.to_s\n"
        "      end\n"
        "      text << \"\\nDetails: #{JSON.generate(@details)}\" if defined?(JSON) && @details\n"
        "      text << \"\\nOriginal error handle: #{@original_error_handle}\" if @original_error_handle && !@original_error_handle.empty?\n"
        "      text\n"
        "    end\n"
        "  end\n"
        "  def self.__method_without_required_args?(object, name)\n"
        "    return false unless object.respond_to?(name)\n"
        "    arity = object.method(name).arity\n"
        "    arity == 0 || arity == -1\n"
        "  rescue StandardError\n"
        "    false\n"
        "  end\n"
        "  def self.__active_model_errors_details(errors)\n"
        "    out = {}\n"
        "    out[:errors] = errors.details if __method_without_required_args?(errors, :details)\n"
        "    out[:messages] = errors.to_hash if __method_without_required_args?(errors, :to_hash)\n"
        "    out[:full_messages] = errors.full_messages if __method_without_required_args?(errors, :full_messages)\n"
        "    out.empty? ? nil : out\n"
        "  rescue StandardError\n"
        "    nil\n"
        "  end\n"
        "  def self.__error_details_value(error)\n"
        "    return error.details if __method_without_required_args?(error, :details) && !error.details.nil?\n"
        "    [:record, :model].each do |holder|\n"
        "      if __method_without_required_args?(error, holder) && (record = error.public_send(holder)) && record.respond_to?(:errors)\n"
        "        details = __active_model_errors_details(record.errors)\n"
        "        return details if details\n"
        "      end\n"
        "    end\n"
        "    if __method_without_required_args?(error, :errors)\n"
        "      errors = error.errors\n"
        "      details = __active_model_errors_details(errors)\n"
        "      return details if details\n"
        "      return({errors: errors}) unless errors.nil?\n"
        "    end\n"
        "    return error.to_hash if __method_without_required_args?(error, :to_hash) && !error.to_hash.nil?\n"
        "    return error.to_h if __method_without_required_args?(error, :to_h) && !error.to_h.nil?\n"
        "    nil\n"
        "  rescue StandardError\n"
        "    nil\n"
        "  end\n"
        "  def self.__error_details_json(error)\n"
        "    require 'json' unless defined?(JSON)\n"
        "    value = __error_details_value(error)\n"
        "    return nil if value.nil?\n"
        "    JSON.generate(value)\n"
        "  rescue Exception\n"
        "    nil\n"
        "  end\n"
        "  def self.__runtime_error_from_bridge(message, runtime)\n"
        "    RuntimeError.new(message, runtime: runtime, boundary_path: runtime && \"call[#{runtime}]\")\n"
        "  end\n"
        "  def self.__copy_json_value(value)\n"
        "    case value\n"
        "    when Hash\n"
        "      value.each_with_object({}) { |(key, item), out| out[key] = __copy_json_value(item) }\n"
        "    when Array\n"
        "      value.map { |item| __copy_json_value(item) }\n"
        "    else\n"
        "      value\n"
        "    end\n"
        "  end\n"
        "  def self.__parse_runtime_error_envelope(text, runtime = nil, boundary_path = nil)\n"
        "    body = text.to_s.strip\n"
        "    body = body[4..-1].to_s.strip if body.start_with?(\"ERR:\")\n"
        "    return nil unless body.start_with?(\"{\")\n"
        "    begin\n"
        "      envelope = JSON.parse(body)\n"
        "    rescue StandardError\n"
        "      return nil\n"
        "    end\n"
        "    return nil unless envelope.is_a?(Hash)\n"
        "    read_field = ->(hash, preferred, fallback = nil) do\n"
        "      preferred_sym = preferred.to_sym\n"
        "      return hash[preferred] if hash.key?(preferred)\n"
        "      return hash[preferred_sym] if hash.key?(preferred_sym)\n"
        "      if fallback\n"
        "        fallback_sym = fallback.to_sym\n"
        "        return hash[fallback] if hash.key?(fallback)\n"
        "        return hash[fallback_sym] if hash.key?(fallback_sym)\n"
        "      end\n"
        "      nil\n"
        "    end\n"
        "    field = ->(preferred, fallback) { read_field.call(envelope, preferred, fallback) }\n"
        "    text_field = ->(value, fallback = \"\") { value.nil? ? fallback : value.to_s }\n"
        "    details_field = ->(source) do\n"
        "      return nil unless source.is_a?(Hash)\n"
        "      details = read_field.call(source, \"details\")\n"
        "      return __copy_json_value(details) unless details.nil?\n"
        "      raw_details = read_field.call(source, \"details_json\", \"detailsJson\")\n"
        "      if raw_details.is_a?(String)\n"
        "        begin\n"
        "          return JSON.parse(raw_details)\n"
        "        rescue StandardError\n"
        "          return raw_details\n"
        "        end\n"
        "      end\n"
        "      raw_details.nil? ? nil : __copy_json_value(raw_details)\n"
        "    end\n"
        "    runtime_name = text_field.call(field.call(\"runtime\", \"runtime\"), runtime)\n"
        "    origin_runtime = text_field.call(field.call(\"origin_runtime\", \"originRuntime\"), runtime_name)\n"
        "    err_type = text_field.call(field.call(\"type\", \"type\"))\n"
        "    detail = text_field.call(field.call(\"message\", \"message\"))\n"
        "    traceback = text_field.call(field.call(\"traceback\", \"traceback\"))\n"
        "    return nil unless runtime_name || !err_type.to_s.empty? || !detail.to_s.empty? || !traceback.to_s.empty?\n"
        "    stack_frames = field.call(\"stack_frames\", \"stackFrames\")\n"
        "    stack_frames = __runtime_error_stack_frames(traceback) unless stack_frames.is_a?(Array) && stack_frames.all? { |frame| frame.is_a?(String) }\n"
        "    cause_chain = field.call(\"cause_chain\", \"causeChain\")\n"
        "    cause_chain = [] unless cause_chain.is_a?(Array)\n"
        "    cause_chain = cause_chain.each_with_object([]) do |cause, out|\n"
        "      next unless cause.is_a?(Hash)\n"
        "      item = {type: (read_field.call(cause, \"type\") || \"\").to_s, message: (read_field.call(cause, \"message\") || \"\").to_s}\n"
        "      cause_traceback = read_field.call(cause, \"traceback\")\n"
        "      item[:traceback] = cause_traceback if cause_traceback.is_a?(String)\n"
        "      cause_stack_frames = read_field.call(cause, \"stack_frames\", \"stackFrames\")\n"
        "      if cause_stack_frames.is_a?(Array) && cause_stack_frames.all? { |frame| frame.is_a?(String) }\n"
        "        item[:stack_frames] = cause_stack_frames.dup\n"
        "      elsif cause_traceback.is_a?(String)\n"
        "        item[:stack_frames] = __runtime_error_stack_frames(cause_traceback)\n"
        "      end\n"
        "      {\"runtime\" => \"runtime\", \"origin_runtime\" => \"originRuntime\", \"boundary_path\" => \"boundaryPath\", \"original_error_handle\" => \"originalErrorHandle\"}.each do |key, fallback|\n"
        "        value = read_field.call(cause, key, fallback)\n"
        "        item[key.to_sym] = value.to_s if value\n"
        "      end\n"
        "      item[:runtime] = runtime_name if !item[:runtime] && runtime_name && !runtime_name.to_s.empty?\n"
        "      item[:origin_runtime] = item[:runtime] if item[:runtime] && !item[:origin_runtime]\n"
        "      cause_details = details_field.call(cause)\n"
        "      item[:details] = cause_details unless cause_details.nil?\n"
        "      out << item\n"
        "    end\n"
        "    {runtime: runtime_name, origin_runtime: origin_runtime, type: err_type, message: detail, traceback: traceback, stack_frames: stack_frames, cause_chain: cause_chain, boundary_path: text_field.call(field.call(\"boundary_path\", \"boundaryPath\"), boundary_path), original_error_handle: text_field.call(field.call(\"original_error_handle\", \"originalErrorHandle\"), nil), details: details_field.call(envelope)}\n"
        "  end\n"
        "  def self.__parse_runtime_error_text(text, runtime = nil, boundary_path = nil)\n"
        "    envelope = __parse_runtime_error_envelope(text, runtime, boundary_path)\n"
        "    return envelope if envelope\n"
        "    source_runtime = runtime\n"
        "    body = text.to_s.lstrip\n"
        "    body = body[4..-1].to_s if body.start_with?(\"ERR:\")\n"
        "    boundary_parts = []\n"
        "    [[\"execute manifest: \", \"execute manifest\"], [\"load manifest module: \", \"load manifest module\"], [\"manifest module call: \", \"manifest module call\"]].each do |prefix, label|\n"
        "      if body.start_with?(prefix)\n"
        "        boundary_parts << label\n"
        "        body = body[prefix.length..-1].to_s\n"
        "        break\n"
        "      end\n"
        "    end\n"
        "    if (m = body.match(/\\A([A-Za-z_][A-Za-z0-9_]*) \\[([A-Za-z0-9_-]+)\\]: ([\\s\\S]*)\\z/))\n"
        "      boundary_parts << \"#{m[1]}[#{m[2]}]\"\n"
        "      source_runtime = m[2]\n"
        "      body = m[3]\n"
        "    end\n"
        "    if (m = body.match(/\\Aruntime ref assign \\[([A-Za-z0-9_-]+)\\]: ([\\s\\S]*)\\z/))\n"
        "      source_runtime = m[1]\n"
        "      body = m[2]\n"
        "    end\n"
        "    prefixes = [[\"javascript: \", \"javascript\"], [\"python: \", \"python\"], [\"ruby: \", \"ruby\"], [\"jvm: \", \"java\"], [\"java: \", \"java\"], [\"go: \", \"go\"]]\n"
        "    changed = true\n"
        "    while changed\n"
        "      changed = false\n"
        "      prefixes.each do |prefix, canonical|\n"
        "        if body.start_with?(prefix)\n"
        "          source_runtime = canonical\n"
        "          body = body[prefix.length..-1].to_s.lstrip\n"
        "          changed = true\n"
        "          break\n"
        "        end\n"
        "      end\n"
        "    end\n"
        "    fallback_boundary = source_runtime && source_runtime != runtime ? \"call[#{source_runtime}]\" : boundary_path\n"
        "    wrapped_boundary = boundary_parts.empty? ? fallback_boundary : boundary_parts.join(\" > \")\n"
        "    envelope = __parse_runtime_error_envelope(body, source_runtime, wrapped_boundary)\n"
        "    return envelope if envelope\n"
        "    first_line, rest = body.split(\"\\n\", 2)\n"
        "    first_line ||= \"\"\n"
        "    rest ||= \"\"\n"
        "    parse_line = first_line\n"
        "    traceback = rest\n"
        "    if (hm = body.match(/^\\s*(?:Original[- ]error[- ]handle|original_error_handle):\\s*(\\S+)\\s*$/i))\n"
        "      original_error_handle = hm[1]\n"
        "    else\n"
        "      original_error_handle = nil\n"
        "    end\n"
        "    if first_line.start_with?(\"Traceback \")\n"
        "      traceback = body\n"
        "      body.lines.reverse_each do |line|\n"
        "        stripped = line.strip\n"
        "        next if OmniVM.__runtime_error_metadata_line?(stripped)\n"
        "        next unless stripped.include?(\": \")\n"
        "        candidate = stripped.split(\": \", 2).first\n"
        "        if candidate.match?(/\\A[A-Za-z_][A-Za-z0-9_.$:]*\\z/)\n"
        "          parse_line = stripped\n"
        "          break\n"
        "        end\n"
        "      end\n"
        "    end\n"
        "    err_type = \"\"\n"
        "    detail = first_line\n"
        "    if parse_line.include?(\": \")\n"
        "      candidate, tail = parse_line.split(\": \", 2)\n"
        "      if candidate.match?(/\\A[A-Za-z_][A-Za-z0-9_.$:]*\\z/)\n"
        "        candidate = candidate.split('.').last if source_runtime == \"python\"\n"
        "        err_type = candidate\n"
        "        detail = tail\n"
        "      end\n"
        "    end\n"
        "    causes = []\n"
        "    rest.each_line do |line|\n"
        "      stripped = line.strip\n"
        "      next unless stripped.start_with?(\"Caused by: \")\n"
        "      cause_text = stripped[\"Caused by: \".length..-1].to_s\n"
        "      cause_type = \"\"\n"
        "      cause_message = cause_text\n"
        "      if cause_text.include?(\": \")\n"
        "        candidate, tail = cause_text.split(\": \", 2)\n"
        "        if candidate.match?(/\\A[A-Za-z_][A-Za-z0-9_.$:]*\\z/)\n"
        "          cause_type = candidate\n"
        "          cause_message = tail\n"
        "        end\n"
        "      end\n"
        "      causes << {type: cause_type, message: cause_message, runtime: source_runtime, origin_runtime: source_runtime}\n"
        "    end\n"
        "    {runtime: source_runtime, origin_runtime: source_runtime, type: err_type, message: detail, traceback: traceback, stack_frames: OmniVM.__runtime_error_stack_frames(traceback), cause_chain: causes, boundary_path: wrapped_boundary, original_error_handle: original_error_handle, details: OmniVM.__parse_runtime_error_details(body)}\n"
        "  end\n"
        "  def self.__runtime_error_metadata_line?(line)\n"
        "    lower = line.to_s.strip.downcase\n"
        "    lower.start_with?(\"caused by:\") || lower.start_with?(\"details:\") || lower.start_with?(\"original_error_handle:\") || lower.start_with?(\"original error handle:\") || lower.start_with?(\"original-error-handle:\")\n"
        "  end\n"
        "  def self.__runtime_error_stack_frames(traceback)\n"
        "    traceback.to_s.each_line.map(&:strip).reject { |line| line.empty? || __runtime_error_metadata_line?(line) }\n"
        "  end\n"
        "  def self.__parse_runtime_error_details(text)\n"
        "    text.to_s.each_line do |line|\n"
        "      stripped = line.strip\n"
        "      next unless stripped.start_with?(\"Details: \")\n"
        "      begin\n"
        "        return JSON.parse(stripped[\"Details: \".length..-1].to_s) if defined?(JSON)\n"
        "      rescue StandardError\n"
        "        return nil\n"
        "      end\n"
        "    end\n"
        "    nil\n"
        "  end\n"
        "end\n") != 0) {
        return -1;
    }

    if (omnivm_ruby_eval_bootstrap("ecosystem compatibility prelude",
        "module OmniVMRubyCompat\n"
        "  def self.patch_active_record_reaper\n"
        "    return unless defined?(::ActiveRecord::ConnectionAdapters::ConnectionPool::Reaper)\n"
        "    reaper = ::ActiveRecord::ConnectionAdapters::ConnectionPool::Reaper\n"
        "    return if reaper.method_defined?(:__omnivm_reaper_run)\n"
        "    reaper.class_eval do\n"
        "      alias __omnivm_reaper_run run\n"
        "      def run\n"
        "        nil\n"
        "      end\n"
        "    end\n"
        "  end\n"
        "end\n"
        "if defined?(::Thread)\n"
        "  class << ::Thread\n"
        "    alias __omnivm_native_new new unless method_defined?(:__omnivm_native_new)\n"
        "    alias __omnivm_native_start start unless method_defined?(:__omnivm_native_start)\n"
        "    alias __omnivm_native_fork fork unless method_defined?(:__omnivm_native_fork)\n"
        "    def __omnivm_unsupported_new(*args, &block)\n"
        "      raise RuntimeError, \"Ruby Thread.new is not supported in OmniVM embedded Ruby; Ruby executes on a single VM-owned thread, so use Fiber/Async or run threaded Ruby app servers out of process\"\n"
        "    end\n"
        "    alias new __omnivm_unsupported_new\n"
        "    alias start __omnivm_unsupported_new\n"
        "    alias fork __omnivm_unsupported_new\n"
        "  end\n"
        "end\n"
        "module Kernel\n"
        "  alias __omnivm_native_require require unless private_method_defined?(:__omnivm_native_require) || method_defined?(:__omnivm_native_require)\n"
        "  def require(path)\n"
        "    loaded = __omnivm_native_require(path)\n"
        "    path_text = path.to_s\n"
        "    if path_text == 'active_record' || path_text.start_with?('active_record/')\n"
        "      OmniVMRubyCompat.patch_active_record_reaper\n"
        "    end\n"
        "    loaded\n"
        "  end\n"
        "  module_function :require\n"
        "end\n") != 0) {
        return -1;
    }

    if (omnivm_ruby_eval_bootstrap("bootstrap validation",
        "missing = []\n"
        "missing << 'Integer#times' unless 3.respond_to?(:times)\n"
        "missing << 'Integer#upto' unless 1.respond_to?(:upto)\n"
        "missing << 'Integer#~' unless (~0) == -1\n"
        "missing << 'Kernel.Integer' unless Integer('42') == 42 && Integer('bad', exception: false).nil?\n"
        "missing << 'Kernel.Float' unless Float('4.25') == 4.25 && Float('bad', exception: false).nil?\n"
        "missing << 'Numeric#to_f' unless 1.to_f == 1.0 && 0.2.to_f == 0.2\n"
        "missing << 'NilClass#to_i' unless nil.to_i == 0\n"
        "missing << 'Object#then' unless Object.new.then { |o| o }.is_a?(Object)\n"
        "missing << 'Array#last' unless [1, 2, 3].last(2) == [2, 3]\n"
        "missing << 'Object#frozen?' unless Object.new.respond_to?(:frozen?)\n"
        "missing << 'String#unpack1' unless \"A\".unpack1('H*') == '41'\n"
        "missing << 'Dir.glob' unless Dir.respond_to?(:glob) && Dir.respond_to?(:[])\n"
        "missing << 'Dir.glob base keyword' unless Dir.glob('*.rb', base: '/usr/lib/ruby/vendor_ruby/rubygems').include?('util.rb')\n"
        "missing << 'File#readline public' unless File.open('/usr/lib/ruby/vendor_ruby/rubygems/util.rb') { |file| file.respond_to?(:readline) && file.readline.is_a?(String) }\n"
        "missing << 'Time.new' unless Time.new(2000, 1, 1, 0, 0, 0).respond_to?(:yday)\n"
        "missing << 'GC.stat' unless GC.stat({}).is_a?(Hash)\n"
        "missing << 'Ractor.current' if defined?(Ractor) && !Ractor.respond_to?(:current)\n"
        "missing << 'Thread.current' unless Thread.current.is_a?(Thread)\n"
        "missing << 'Thread::Queue pop' unless Thread::Queue.new.tap { |q| q.push(:ok) }.pop == :ok\n"
        "missing << 'Thread::SizedQueue push/pop' unless Thread::SizedQueue.new(1).tap { |q| q.push(:ok) }.pop == :ok\n"
        "missing << 'Thread::Queue blocking diagnostic' unless begin Thread::Queue.new.pop; false; rescue ThreadError => e; e.message.include?('OmniVM embedded Ruby'); end\n"
        "begin\n"
        "  Thread.new { nil }\n"
        "  missing << 'Thread.new diagnostic'\n"
        "rescue RuntimeError => e\n"
        "  missing << 'Thread.new diagnostic' unless e.message.include?('Ruby Thread.new is not supported in OmniVM embedded Ruby')\n"
        "end\n"
        "begin\n"
        "  Thread.start { nil }\n"
        "  missing << 'Thread.start diagnostic'\n"
        "rescue RuntimeError => e\n"
        "  missing << 'Thread.start diagnostic' unless e.message.include?('Ruby Thread.new is not supported in OmniVM embedded Ruby')\n"
        "end\n"
        "begin\n"
        "  Thread.fork { nil }\n"
        "  missing << 'Thread.fork diagnostic'\n"
        "rescue RuntimeError => e\n"
        "  missing << 'Thread.fork diagnostic' unless e.message.include?('Ruby Thread.new is not supported in OmniVM embedded Ruby')\n"
        "end\n"
        "missing << 'Kernel.warn' unless Kernel.respond_to?(:warn)\n"
        "missing << 'Kernel.require' unless Kernel.respond_to?(:require)\n"
        "missing << 'Array#dup unfrozen' if [1].freeze.dup.frozen?\n"
        "missing << 'Hash#dup unfrozen' if ({a: 1}.freeze.dup.frozen?)\n"
        "raise \"missing Ruby bootstrap surface: #{missing.join(', ')}\" unless missing.empty?\n"
        "true\n") != 0) {
        return -1;
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
    pthread_mutex_lock(&g_ruby_bridge_call_mu);
    a->result = g_bridge_call(a->runtime, a->code);
    pthread_mutex_unlock(&g_ruby_bridge_call_mu);
    tls_holds_gvl = 1;  // GVL reacquired upon return from rb_thread_call_without_gvl
    return NULL;
}

static int omnivm_ruby_in_nonblocking_fiber(void) {
    ID blocking_id = rb_intern("blocking?");
    VALUE fiber = rb_fiber_current();
    if (NIL_P(fiber) || !rb_respond_to(fiber, blocking_id)) {
        return 0;
    }
    return rb_funcall(fiber, blocking_id, 0) == Qfalse;
}

static int omnivm_ruby_runtime_requires_blocking_fiber(const char* runtime) {
    return strcmp(runtime, "javascript") == 0 || strcmp(runtime, "js") == 0 ||
           strcmp(runtime, "java") == 0 || strcmp(runtime, "jvm") == 0;
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

    if (omnivm_ruby_runtime_requires_blocking_fiber(runtime) && omnivm_ruby_in_nonblocking_fiber()) {
        rb_raise(rb_eRuntimeError, "Ruby non-blocking Fiber cannot enter %s runtime directly; run OmniVM.call from a blocking/root Ruby fiber or move the bridge call outside the Async task", runtime);
        return Qnil;
    }

    char* result = NULL;
    if (strcmp(runtime, "python") == 0) {
        pthread_mutex_lock(&g_ruby_bridge_call_mu);
        result = g_bridge_call(runtime, code);
        pthread_mutex_unlock(&g_ruby_bridge_call_mu);
    } else {
        ruby_bridge_args bargs = { .runtime = runtime, .code = code, .result = NULL };
        rb_thread_call_without_gvl(ruby_bridge_no_gvl, &bargs, RUBY_UBF_IO, NULL);
        result = bargs.result;
    }

    if (!result) {
        rb_raise(rb_eRuntimeError, "OmniVM.call returned NULL");
        return Qnil;
    }

    // Check for error prefix
    if (strncmp(result, "ERR:", 4) == 0) {
        VALUE err_msg = rb_str_new_cstr(result + 4);
        VALUE runtime_hint = rb_str_new_cstr(runtime);
        if (g_bridge_free) g_bridge_free(result);
        VALUE mod = rb_const_get(rb_cObject, rb_intern("OmniVM"));
        VALUE exc = rb_funcall(mod, rb_intern("__runtime_error_from_bridge"), 2, err_msg, runtime_hint);
        rb_exc_raise(exc);
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
    pthread_mutex_lock(&g_ruby_bridge_call_mu);
    a->result = g_call_typed(a->runtime, a->func_name, a->args, a->nargs);
    pthread_mutex_unlock(&g_ruby_bridge_call_mu);
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

    if (omnivm_ruby_runtime_requires_blocking_fiber(runtime) && omnivm_ruby_in_nonblocking_fiber()) {
        rb_raise(rb_eRuntimeError, "Ruby non-blocking Fiber cannot enter %s runtime directly; run OmniVM.call_typed from a blocking/root Ruby fiber or move the bridge call outside the Async task", runtime);
        return Qnil;
    }

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

    if (strcmp(runtime, "python") == 0) {
        pthread_mutex_lock(&g_ruby_bridge_call_mu);
        bargs.result = g_call_typed(bargs.runtime, bargs.func_name, bargs.args, bargs.nargs);
        pthread_mutex_unlock(&g_ruby_bridge_call_mu);
    } else {
        rb_thread_call_without_gvl(ruby_typed_bridge_no_gvl, &bargs, RUBY_UBF_IO, NULL);
    }

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
    if (!g_buf_free) {
        rb_raise(rb_eRuntimeError, "omnivm buffer bridge not initialized");
        return Qnil;
    }
    const char* name = StringValueCStr(rb_name);
    if (g_buf_free(name) != 0) {
        rb_raise(rb_eRuntimeError, "OmniVM.release_buffer failed");
    }
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
func (r *Runtime) SetBufCallbacks(getPtr, setPtr, releasePtr, freePtr uintptr) {
	C.omnivm_ruby_set_buf_callbacks(
		C.omni_buf_get_fn(unsafe.Pointer(getPtr)),
		C.omni_buf_set_fn(unsafe.Pointer(setPtr)),
		C.omni_buf_release_fn(unsafe.Pointer(releasePtr)),
		C.omni_buf_free_fn(unsafe.Pointer(freePtr)),
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
