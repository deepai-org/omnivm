// v8_bridge.cc - C++ implementation of the V8 C bridge
#include "v8_bridge.h"

#include <libplatform/libplatform.h>
#include <v8.h>
#include <string.h>
#include <stdlib.h>

// Internal structures behind opaque handles
struct omnivm_v8_isolate {
    v8::Isolate* isolate;
    v8::Isolate::CreateParams create_params;
};

struct omnivm_v8_context {
    v8::Global<v8::Context> context;
    omnivm_v8_isolate* isolate_wrapper;
};

static std::unique_ptr<v8::Platform> platform;

// Bridge callback pointers
static omnivm_bridge_call_fn g_bridge_call = nullptr;
static omnivm_bridge_free_fn g_bridge_free = nullptr;

extern "C" {

int omnivm_v8_init(void) {
    v8::V8::InitializeICUDefaultLocation("");
    v8::V8::InitializeExternalStartupData("");
    platform = v8::platform::NewDefaultPlatform();
    v8::V8::InitializePlatform(platform.get());
    return v8::V8::Initialize() ? 0 : -1;
}

omnivm_v8_isolate* omnivm_v8_isolate_new(void) {
    auto* w = new omnivm_v8_isolate();
    w->create_params.array_buffer_allocator =
        v8::ArrayBuffer::Allocator::NewDefaultAllocator();
    w->isolate = v8::Isolate::New(w->create_params);
    return w;
}

omnivm_v8_context* omnivm_v8_context_new(omnivm_v8_isolate* isolate_w) {
    auto* ctx_w = new omnivm_v8_context();
    ctx_w->isolate_wrapper = isolate_w;

    v8::Isolate::Scope isolate_scope(isolate_w->isolate);
    v8::HandleScope handle_scope(isolate_w->isolate);

    // Create a new context with a console.log shim
    v8::Local<v8::ObjectTemplate> global = v8::ObjectTemplate::New(isolate_w->isolate);
    v8::Local<v8::Context> context = v8::Context::New(isolate_w->isolate, nullptr, global);

    ctx_w->context.Reset(isolate_w->isolate, context);
    return ctx_w;
}

omnivm_v8_result omnivm_v8_execute(omnivm_v8_context* ctx_w, const char* code) {
    omnivm_v8_result result = {nullptr, nullptr};
    v8::Isolate* isolate = ctx_w->isolate_wrapper->isolate;

    v8::Isolate::Scope isolate_scope(isolate);
    v8::HandleScope handle_scope(isolate);
    v8::Local<v8::Context> context =
        v8::Local<v8::Context>::New(isolate, ctx_w->context);
    v8::Context::Scope context_scope(context);

    // Set up a console.log capture array
    const char* setup =
        "var __omnivm_output = [];"
        "var console = { log: function() {"
        "  __omnivm_output.push(Array.prototype.slice.call(arguments).join(' '));"
        "}};\n";

    // Compile and run setup
    v8::Local<v8::String> setup_src =
        v8::String::NewFromUtf8(isolate, setup).ToLocalChecked();
    v8::Script::Compile(context, setup_src).ToLocalChecked()->Run(context);

    // Compile the user code
    v8::Local<v8::String> source =
        v8::String::NewFromUtf8(isolate, code).ToLocalChecked();

    v8::TryCatch try_catch(isolate);

    v8::MaybeLocal<v8::Script> maybe_script =
        v8::Script::Compile(context, source);

    if (maybe_script.IsEmpty()) {
        v8::Local<v8::Value> exception = try_catch.Exception();
        v8::String::Utf8Value err_str(isolate, exception);
        result.error = strdup(*err_str ? *err_str : "compilation error");
        return result;
    }

    v8::MaybeLocal<v8::Value> maybe_result =
        maybe_script.ToLocalChecked()->Run(context);

    if (try_catch.HasCaught()) {
        v8::Local<v8::Value> exception = try_catch.Exception();
        v8::String::Utf8Value err_str(isolate, exception);
        result.error = strdup(*err_str ? *err_str : "runtime error");
        return result;
    }

    // Retrieve captured console output
    const char* get_output =
        "__omnivm_output.join('\\n') + (__omnivm_output.length ? '\\n' : '')";
    v8::Local<v8::String> get_src =
        v8::String::NewFromUtf8(isolate, get_output).ToLocalChecked();
    v8::Local<v8::Value> output_val =
        v8::Script::Compile(context, get_src).ToLocalChecked()->Run(context).ToLocalChecked();
    v8::String::Utf8Value output_str(isolate, output_val);
    result.value = strdup(*output_str ? *output_str : "");

    return result;
}

// Eval — returns expression value directly (not stdout capture)
omnivm_v8_result omnivm_v8_eval(omnivm_v8_context* ctx_w, const char* code) {
    omnivm_v8_result result = {nullptr, nullptr};
    v8::Isolate* isolate = ctx_w->isolate_wrapper->isolate;

    v8::Isolate::Scope isolate_scope(isolate);
    v8::HandleScope handle_scope(isolate);
    v8::Local<v8::Context> context =
        v8::Local<v8::Context>::New(isolate, ctx_w->context);
    v8::Context::Scope context_scope(context);

    v8::Local<v8::String> source =
        v8::String::NewFromUtf8(isolate, code).ToLocalChecked();

    v8::TryCatch try_catch(isolate);

    v8::MaybeLocal<v8::Script> maybe_script =
        v8::Script::Compile(context, source);

    if (maybe_script.IsEmpty()) {
        v8::Local<v8::Value> exception = try_catch.Exception();
        v8::String::Utf8Value err_str(isolate, exception);
        result.error = strdup(*err_str ? *err_str : "compilation error");
        return result;
    }

    v8::MaybeLocal<v8::Value> maybe_result =
        maybe_script.ToLocalChecked()->Run(context);

    if (try_catch.HasCaught()) {
        v8::Local<v8::Value> exception = try_catch.Exception();
        v8::String::Utf8Value err_str(isolate, exception);
        result.error = strdup(*err_str ? *err_str : "runtime error");
        return result;
    }

    if (maybe_result.IsEmpty()) {
        result.value = strdup("undefined");
    } else {
        v8::Local<v8::Value> val = maybe_result.ToLocalChecked();
        v8::String::Utf8Value val_str(isolate, val);
        result.value = strdup(*val_str ? *val_str : "undefined");
    }

    return result;
}

void omnivm_v8_set_bridge_callback(omnivm_bridge_call_fn call_fn, omnivm_bridge_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

void omnivm_v8_pump_message_loop(omnivm_v8_isolate* isolate_w) {
    if (platform && isolate_w && isolate_w->isolate) {
        while (v8::platform::PumpMessageLoop(
                   platform.get(), isolate_w->isolate)) {
            // Keep pumping until no more messages
        }
    }
}

void omnivm_v8_context_free(omnivm_v8_context* ctx_w) {
    if (ctx_w) {
        ctx_w->context.Reset();
        delete ctx_w;
    }
}

void omnivm_v8_isolate_free(omnivm_v8_isolate* isolate_w) {
    if (isolate_w) {
        isolate_w->isolate->Dispose();
        delete isolate_w->create_params.array_buffer_allocator;
        delete isolate_w;
    }
}

void omnivm_v8_shutdown(void) {
    v8::V8::Dispose();
    v8::V8::DisposePlatform();
}

void omnivm_v8_free_string(char* s) {
    free(s);
}

} // extern "C"
