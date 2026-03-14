// v8_bridge_node.cc - Node.js Embedder API implementation of v8_bridge.h
//
// Replaces Duktape with full Node.js (V8 + libuv + npm ecosystem).
// The C API surface (v8_bridge.h) is unchanged — Go code doesn't know
// or care that the backend switched.
//
// Key design choices:
//   - No node::SpinEventLoop(). We pump libuv manually via uv_run(UV_RUN_NOWAIT)
//     in omnivm_v8_pump_message_loop(), called every 1ms by the dispatcher.
//   - v8::Locker is acquired in every entry point (execute, eval, pump)
//     because MultiIsolatePlatform spawns a V8 background thread.
//   - node::CallbackScope wraps every code execution and pump cycle
//     so process.nextTick callbacks are drained properly.
//   - kNoDefaultSignalHandling prevents Node from stealing signals from
//     Go and JVM. --no-wasm-trap-handler prevents V8 SIGSEGV conflicts with JVM.

#include "v8_bridge.h"

#include <node.h>
#include <uv.h>
#include <v8.h>

#include <cstdlib>
#include <cstring>
#include <memory>
#include <string>
#include <vector>

// Internal structures behind opaque C handles
struct omnivm_v8_isolate {
    node::MultiIsolatePlatform* platform;  // borrowed from init_result
    omnivm_v8_context* active_context;     // back-pointer for pump
};

struct omnivm_v8_context {
    std::unique_ptr<node::CommonEnvironmentSetup> setup;
    v8::Isolate* isolate;           // borrowed from setup
    node::Environment* env;         // borrowed from setup
    uv_loop_t* event_loop;          // borrowed from setup
    v8::Global<v8::Context> context;
};

// Bridge callback pointers — same pattern as Duktape shim
static omnivm_bridge_call_fn g_bridge_call = nullptr;
static omnivm_bridge_free_fn g_bridge_free = nullptr;

// Node.js per-process init result
static std::unique_ptr<node::InitializationResult> init_result;

// ---- omnivm.call() V8 native function ----

static void OmnivmCallCallback(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();

    if (info.Length() < 2 || !info[0]->IsString() || !info[1]->IsString()) {
        isolate->ThrowException(v8::Exception::TypeError(
            v8::String::NewFromUtf8Literal(isolate, "omnivm.call requires (runtime, code)")));
        return;
    }

    v8::String::Utf8Value runtime(isolate, info[0]);
    v8::String::Utf8Value code(isolate, info[1]);

    if (!g_bridge_call) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "omnivm bridge not initialized")));
        return;
    }

    char* result = g_bridge_call(*runtime, *code);
    if (!result) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "omnivm.call returned NULL")));
        return;
    }

    // Check for error prefix — same protocol as Duktape/v8_bridge.cc
    if (strncmp(result, "ERR:", 4) == 0) {
        std::string err_msg(result + 4);
        if (g_bridge_free) g_bridge_free(result);
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8(isolate, err_msg.c_str()).ToLocalChecked()));
        return;
    }

    v8::Local<v8::String> ret =
        v8::String::NewFromUtf8(isolate, result).ToLocalChecked();
    if (g_bridge_free) g_bridge_free(result);
    info.GetReturnValue().Set(ret);
}

// Register globalThis.omnivm.call() on the given context
static void register_omnivm_bridge(v8::Isolate* isolate,
                                    v8::Local<v8::Context> context) {
    v8::Local<v8::Object> global = context->Global();

    v8::Local<v8::Object> omnivm_obj = v8::Object::New(isolate);
    v8::Local<v8::FunctionTemplate> call_tmpl =
        v8::FunctionTemplate::New(isolate, OmnivmCallCallback);
    omnivm_obj->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "call"),
        call_tmpl->GetFunction(context).ToLocalChecked()).Check();

    global->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "omnivm"),
        omnivm_obj).Check();
}

extern "C" {

int omnivm_v8_init(void) {
    // argv[0] is required by Node.js internals
    std::vector<std::string> args = {"omnivm"};
    std::vector<std::string> exec_args;
    std::vector<std::string> errors;

    auto flags = static_cast<node::ProcessInitializationFlags::Flags>(
        node::ProcessInitializationFlags::kNoDefaultSignalHandling |
        node::ProcessInitializationFlags::kNoStdioInitialization |
        node::ProcessInitializationFlags::kNoPrintHelpOrVersionOutput);

    init_result = node::InitializeOncePerProcess(args, flags);

    if (!init_result->errors().empty()) {
        return -1;
    }
    return 0;
}

omnivm_v8_isolate* omnivm_v8_isolate_new(void) {
    if (!init_result) return nullptr;
    auto* iso = new omnivm_v8_isolate();
    iso->platform = init_result->platform();
    iso->active_context = nullptr;
    return iso;
}

omnivm_v8_context* omnivm_v8_context_new(omnivm_v8_isolate* isolate_w) {
    if (!isolate_w || !isolate_w->platform) return nullptr;

    auto* ctx_w = new omnivm_v8_context();

    // Args passed to the Node.js environment
    std::vector<std::string> args = {"omnivm"};
    std::vector<std::string> exec_args;

    // Create the Node.js environment setup
    std::vector<std::string> errors;
    ctx_w->setup = node::CommonEnvironmentSetup::Create(
        isolate_w->platform,
        &errors,
        args,
        exec_args);

    if (!ctx_w->setup) {
        delete ctx_w;
        return nullptr;
    }

    ctx_w->isolate = ctx_w->setup->isolate();
    ctx_w->env = ctx_w->setup->env();
    ctx_w->event_loop = ctx_w->setup->event_loop();

    // Enter isolate + context scopes for bootstrap
    {
        v8::Locker locker(ctx_w->isolate);
        v8::Isolate::Scope isolate_scope(ctx_w->isolate);
        v8::HandleScope handle_scope(ctx_w->isolate);
        v8::Local<v8::Context> context = ctx_w->setup->context();
        v8::Context::Scope context_scope(context);

        // Bootstrap: set up require(), disable process.exit()
        // Note: use require('module'), not require('node:module') —
        // the 'node:' prefix isn't available in the embedder bootstrap context.
        v8::MaybeLocal<v8::Value> bootstrap_result = node::LoadEnvironment(
            ctx_w->env,
            "const publicRequire = require('module').createRequire(process.cwd() + '/');\n"
            "globalThis.require = publicRequire;\n"
            "process.exit = function() {\n"
            "  throw new Error('process.exit is not allowed in OmniVM');\n"
            "};\n"
        );

        if (bootstrap_result.IsEmpty()) {
            delete ctx_w;
            return nullptr;
        }

        // Register omnivm.call()
        register_omnivm_bridge(ctx_w->isolate, context);

        // Store context in persistent handle
        ctx_w->context.Reset(ctx_w->isolate, context);
    }

    // Set back-pointer for pump
    isolate_w->active_context = ctx_w;

    return ctx_w;
}

omnivm_v8_result omnivm_v8_execute(omnivm_v8_context* ctx_w, const char* code) {
    omnivm_v8_result result = {nullptr, nullptr};
    if (!ctx_w || !ctx_w->isolate) {
        result.error = strdup("JS context not initialized");
        return result;
    }

    v8::Locker locker(ctx_w->isolate);
    v8::Isolate::Scope isolate_scope(ctx_w->isolate);
    v8::HandleScope handle_scope(ctx_w->isolate);
    v8::Local<v8::Context> context =
        v8::Local<v8::Context>::New(ctx_w->isolate, ctx_w->context);
    v8::Context::Scope context_scope(context);
    node::CallbackScope callback_scope(ctx_w->isolate,
        v8::Object::New(ctx_w->isolate), {0, 0});

    // Clear any pending V8 termination from a previous watchdog timeout.
    // TerminateExecution() persists across calls until explicitly cancelled;
    // without this, the setup code below crashes with FromJust on Nothing.
    ctx_w->isolate->CancelTerminateExecution();

    v8::Local<v8::Object> global = context->Global();

    // Save original console (Node.js has a real one)
    v8::Local<v8::Value> orig_console;
    (void)global->Get(context,
        v8::String::NewFromUtf8Literal(ctx_w->isolate, "console"))
        .ToLocal(&orig_console);

    // Set up output capture array
    const char* setup_code =
        "var __omnivm_output = [];\n"
        "var console = {\n"
        "  log: function() { __omnivm_output.push(Array.prototype.slice.call(arguments).join(' ')); },\n"
        "  error: function() { __omnivm_output.push(Array.prototype.slice.call(arguments).join(' ')); },\n"
        "  warn: function() { __omnivm_output.push(Array.prototype.slice.call(arguments).join(' ')); },\n"
        "  info: function() { __omnivm_output.push(Array.prototype.slice.call(arguments).join(' ')); }\n"
        "};\n";

    {
        v8::Local<v8::String> setup_src =
            v8::String::NewFromUtf8(ctx_w->isolate, setup_code).ToLocalChecked();
        (void)v8::Script::Compile(context, setup_src).ToLocalChecked()->Run(context);
    }

    // Also register omnivm.call each time (may have been overwritten by user code)
    register_omnivm_bridge(ctx_w->isolate, context);

    // Compile and run user code
    v8::TryCatch try_catch(ctx_w->isolate);
    v8::Local<v8::String> source =
        v8::String::NewFromUtf8(ctx_w->isolate, code).ToLocalChecked();

    v8::MaybeLocal<v8::Script> maybe_script =
        v8::Script::Compile(context, source);

    if (maybe_script.IsEmpty()) {
        if (try_catch.HasTerminated()) {
            ctx_w->isolate->CancelTerminateExecution();
            result.error = strdup("execution terminated (timeout)");
        } else {
            v8::Local<v8::Value> exception = try_catch.Exception();
            v8::String::Utf8Value err_str(ctx_w->isolate, exception);
            result.error = strdup(*err_str ? *err_str : "compilation error");
        }

        // Restore console (safe now that termination is cleared)
        if (!orig_console.IsEmpty()) {
            global->Set(context,
                v8::String::NewFromUtf8Literal(ctx_w->isolate, "console"),
                orig_console).Check();
        }
        return result;
    }

    v8::MaybeLocal<v8::Value> maybe_result =
        maybe_script.ToLocalChecked()->Run(context);

    if (try_catch.HasCaught()) {
        if (try_catch.HasTerminated()) {
            ctx_w->isolate->CancelTerminateExecution();
            result.error = strdup("execution terminated (timeout)");
        } else {
            v8::Local<v8::Value> exception = try_catch.Exception();
            v8::String::Utf8Value err_str(ctx_w->isolate, exception);
            result.error = strdup(*err_str ? *err_str : "runtime error");
        }

        // Restore console (safe now that termination is cleared)
        if (!orig_console.IsEmpty()) {
            global->Set(context,
                v8::String::NewFromUtf8Literal(ctx_w->isolate, "console"),
                orig_console).Check();
        }
        return result;
    }

    // Retrieve captured console output
    const char* get_output =
        "__omnivm_output.join('\\n') + (__omnivm_output.length ? '\\n' : '')";
    v8::Local<v8::String> get_src =
        v8::String::NewFromUtf8(ctx_w->isolate, get_output).ToLocalChecked();
    v8::Local<v8::Value> output_val =
        v8::Script::Compile(context, get_src)
            .ToLocalChecked()->Run(context).ToLocalChecked();
    v8::String::Utf8Value output_str(ctx_w->isolate, output_val);
    result.value = strdup(*output_str ? *output_str : "");

    // Restore original console
    if (!orig_console.IsEmpty()) {
        global->Set(context,
            v8::String::NewFromUtf8Literal(ctx_w->isolate, "console"),
            orig_console).Check();
    }

    return result;
}

omnivm_v8_result omnivm_v8_eval(omnivm_v8_context* ctx_w, const char* code) {
    omnivm_v8_result result = {nullptr, nullptr};
    if (!ctx_w || !ctx_w->isolate) {
        result.error = strdup("JS context not initialized");
        return result;
    }

    v8::Locker locker(ctx_w->isolate);
    v8::Isolate::Scope isolate_scope(ctx_w->isolate);
    v8::HandleScope handle_scope(ctx_w->isolate);
    v8::Local<v8::Context> context =
        v8::Local<v8::Context>::New(ctx_w->isolate, ctx_w->context);
    v8::Context::Scope context_scope(context);
    node::CallbackScope callback_scope(ctx_w->isolate,
        v8::Object::New(ctx_w->isolate), {0, 0});

    ctx_w->isolate->CancelTerminateExecution();

    // Ensure omnivm.call is registered
    register_omnivm_bridge(ctx_w->isolate, context);

    v8::TryCatch try_catch(ctx_w->isolate);
    v8::Local<v8::String> source =
        v8::String::NewFromUtf8(ctx_w->isolate, code).ToLocalChecked();

    v8::MaybeLocal<v8::Script> maybe_script =
        v8::Script::Compile(context, source);

    if (maybe_script.IsEmpty()) {
        if (try_catch.HasTerminated()) {
            ctx_w->isolate->CancelTerminateExecution();
            result.error = strdup("execution terminated (timeout)");
        } else {
            v8::Local<v8::Value> exception = try_catch.Exception();
            v8::String::Utf8Value err_str(ctx_w->isolate, exception);
            result.error = strdup(*err_str ? *err_str : "compilation error");
        }
        return result;
    }

    v8::MaybeLocal<v8::Value> maybe_result =
        maybe_script.ToLocalChecked()->Run(context);

    if (try_catch.HasCaught()) {
        if (try_catch.HasTerminated()) {
            ctx_w->isolate->CancelTerminateExecution();
            result.error = strdup("execution terminated (timeout)");
        } else {
            v8::Local<v8::Value> exception = try_catch.Exception();
            v8::String::Utf8Value err_str(ctx_w->isolate, exception);
            result.error = strdup(*err_str ? *err_str : "runtime error");
        }
        return result;
    }

    if (maybe_result.IsEmpty()) {
        result.value = strdup("undefined");
    } else {
        v8::Local<v8::Value> val = maybe_result.ToLocalChecked();
        if (val->IsUndefined()) {
            result.value = strdup("undefined");
        } else {
            v8::String::Utf8Value val_str(ctx_w->isolate, val);
            result.value = strdup(*val_str ? *val_str : "undefined");
        }
    }

    return result;
}

void omnivm_v8_set_bridge_callback(omnivm_bridge_call_fn call_fn,
                                    omnivm_bridge_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

void omnivm_v8_pump_message_loop(omnivm_v8_isolate* iso_w) {
    if (!iso_w || !iso_w->active_context) return;
    auto* ctx = iso_w->active_context;
    if (!ctx->isolate) return;

    v8::Locker locker(ctx->isolate);
    v8::Isolate::Scope isolate_scope(ctx->isolate);
    v8::HandleScope handle_scope(ctx->isolate);
    v8::Local<v8::Context> context =
        v8::Local<v8::Context>::New(ctx->isolate, ctx->context);
    v8::Context::Scope context_scope(context);

    // CallbackScope ensures process.nextTick queue is drained
    node::CallbackScope callback_scope(ctx->isolate,
        v8::Object::New(ctx->isolate), {0, 0});

    // 1. Pump libuv I/O — processes ready I/O, fires expired timers,
    //    queues up JavaScript callbacks. Non-blocking.
    uv_run(ctx->event_loop, UV_RUN_NOWAIT);

    // 2. Drain V8 background tasks (GC, concurrent compiles) and
    //    microtasks (Promises resolved by I/O callbacks above).
    iso_w->platform->DrainTasks(ctx->isolate);
}

void omnivm_v8_context_free(omnivm_v8_context* ctx_w) {
    if (!ctx_w) return;

    if (ctx_w->isolate && ctx_w->env) {
        v8::Locker locker(ctx_w->isolate);
        v8::Isolate::Scope isolate_scope(ctx_w->isolate);

        // Stop the environment (cancels pending handles)
        node::Stop(ctx_w->env);

        ctx_w->context.Reset();
    }

    // CommonEnvironmentSetup destructor handles isolate + event loop cleanup
    ctx_w->setup.reset();
    delete ctx_w;
}

void omnivm_v8_isolate_free(omnivm_v8_isolate* iso_w) {
    if (!iso_w) return;
    iso_w->active_context = nullptr;
    // Platform is owned by init_result, not by us
    iso_w->platform = nullptr;
    delete iso_w;
}

void omnivm_v8_shutdown(void) {
    // Note: We intentionally skip v8::V8::Dispose() and
    // node::TearDownOncePerProcess() here. In a polyglot process,
    // V8's shutdown sequencing conflicts with already-torn-down
    // subsystems (wrong init order assertion). Since the process is
    // exiting, the OS reclaims all resources. This mirrors our Ruby
    // shutdown strategy (skip ruby_cleanup for the same reason).
    init_result.reset();
}

int omnivm_v8_get_uv_backend_fd(omnivm_v8_context* ctx_w) {
    if (!ctx_w || !ctx_w->event_loop) return -1;
    return uv_backend_fd(ctx_w->event_loop);
}

void omnivm_v8_terminate_execution(omnivm_v8_context* ctx_w) {
    if (!ctx_w || !ctx_w->isolate) return;
    ctx_w->isolate->TerminateExecution();
}

// Watchdog support: void(void) wrapper for terminate_execution.
// Stores the context globally (only one V8 context per process).
static omnivm_v8_context* g_terminate_ctx = nullptr;

void omnivm_v8_set_terminate_context(omnivm_v8_context* ctx_w) {
    g_terminate_ctx = ctx_w;
}

static void omnivm_v8_terminate_thunk(void) {
    omnivm_v8_terminate_execution(g_terminate_ctx);
}

void* omnivm_v8_get_terminate_ptr(void) {
    return (void*)omnivm_v8_terminate_thunk;
}

void omnivm_v8_free_string(char* s) {
    free(s);
}

} // extern "C"
