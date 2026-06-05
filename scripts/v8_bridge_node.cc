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

#include <algorithm>
#include <cctype>
#include <cmath>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <regex>
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

// Typed call bridge callback pointer
static omni_call_typed_fn g_call_typed = nullptr;

// Buffer bridge callback pointers (types from v8_bridge.h)
static omni_buf_get_fn g_buf_get = nullptr;
static omni_buf_set_fn g_buf_set = nullptr;
static omni_buf_release_fn g_buf_release = nullptr;
static omni_buf_free_fn g_buf_free = nullptr;
static omni_buf_status_fn g_buf_status = nullptr;

struct OmniExternalBufferLease {
    char* name;
};

struct OmniExportedBufferHandle {
    std::shared_ptr<v8::BackingStore> backing;
};

static std::string omnivm_v8_value_string(v8::Isolate* isolate,
                                          v8::Local<v8::Context> context,
                                          v8::Local<v8::Value> value) {
    if (value.IsEmpty()) {
        return "";
    }
    v8::Local<v8::String> str;
    if (!value->ToString(context).ToLocal(&str)) {
        return "";
    }
    v8::String::Utf8Value utf8(isolate, str);
    return (*utf8 && **utf8) ? std::string(*utf8) : "";
}

static std::string omnivm_v8_json_escape_string(const std::string& value) {
    static const char hex[] = "0123456789abcdef";
    std::string out;
    out.reserve(value.size() + 2);
    out += '"';
    for (unsigned char ch : value) {
        switch (ch) {
        case '"':
            out += "\\\"";
            break;
        case '\\':
            out += "\\\\";
            break;
        case '\b':
            out += "\\b";
            break;
        case '\f':
            out += "\\f";
            break;
        case '\n':
            out += "\\n";
            break;
        case '\r':
            out += "\\r";
            break;
        case '\t':
            out += "\\t";
            break;
        default:
            if (ch < 0x20) {
                out += "\\u00";
                out += hex[(ch >> 4) & 0x0f];
                out += hex[ch & 0x0f];
            } else {
                out += static_cast<char>(ch);
            }
        }
    }
    out += '"';
    return out;
}

static bool omnivm_v8_json_fallback_stringify(v8::Isolate* isolate,
                                              v8::Local<v8::Context> context,
                                              v8::Local<v8::Value> value,
                                              std::string& out,
                                              std::vector<v8::Local<v8::Object>>& seen,
                                              int depth) {
    if (value.IsEmpty() || value->IsNull() || value->IsUndefined()) {
        out += "null";
        return true;
    }
    if (value->IsBoolean()) {
        out += value->BooleanValue(isolate) ? "true" : "false";
        return true;
    }
    if (value->IsNumber()) {
        double number = value.As<v8::Number>()->Value();
        if (!std::isfinite(number)) {
            out += "null";
            return true;
        }
        char buf[64];
        std::snprintf(buf, sizeof(buf), "%.17g", number);
        out += buf;
        return true;
    }
    if (value->IsBigInt()) {
        out += omnivm_v8_json_escape_string(omnivm_v8_value_string(isolate, context, value));
        return true;
    }
    if (value->IsString()) {
        out += omnivm_v8_json_escape_string(omnivm_v8_value_string(isolate, context, value));
        return true;
    }
    if (value->IsSymbol() || value->IsFunction()) {
        return false;
    }
    if (!value->IsObject()) {
        out += "null";
        return true;
    }
    if (depth >= 16) {
        out += omnivm_v8_json_escape_string("[MaxDepth]");
        return true;
    }

    v8::Local<v8::Object> object = value.As<v8::Object>();
    for (v8::Local<v8::Object> prior : seen) {
        if (prior->StrictEquals(object)) {
            out += omnivm_v8_json_escape_string("[Circular]");
            return true;
        }
    }
    seen.push_back(object);

    if (value->IsArray()) {
        v8::Local<v8::Array> array = value.As<v8::Array>();
        out += '[';
        uint32_t length = array->Length();
        for (uint32_t i = 0; i < length; ++i) {
            if (i > 0) {
                out += ',';
            }
            v8::Local<v8::Value> item;
            std::string item_json;
            if (!array->Get(context, i).ToLocal(&item) ||
                !omnivm_v8_json_fallback_stringify(isolate, context, item, item_json, seen, depth + 1)) {
                out += "null";
            } else {
                out += item_json;
            }
        }
        out += ']';
        seen.pop_back();
        return true;
    }

    v8::Local<v8::Array> names;
    if (!object->GetOwnPropertyNames(context).ToLocal(&names)) {
        seen.pop_back();
        return false;
    }
    out += '{';
    bool first = true;
    for (uint32_t i = 0; i < names->Length(); ++i) {
        v8::Local<v8::Value> key_value;
        if (!names->Get(context, i).ToLocal(&key_value) || key_value.IsEmpty()) {
            continue;
        }
        v8::Local<v8::Value> item;
        if (!object->Get(context, key_value).ToLocal(&item) || item.IsEmpty() ||
            item->IsUndefined() || item->IsFunction() || item->IsSymbol()) {
            continue;
        }
        std::string item_json;
        if (!omnivm_v8_json_fallback_stringify(isolate, context, item, item_json, seen, depth + 1)) {
            continue;
        }
        if (!first) {
            out += ',';
        }
        first = false;
        out += omnivm_v8_json_escape_string(omnivm_v8_value_string(isolate, context, key_value));
        out += ':';
        out += item_json;
    }
    out += '}';
    seen.pop_back();
    return true;
}

static std::string omnivm_v8_get_string_prop(v8::Isolate* isolate,
                                             v8::Local<v8::Context> context,
                                             v8::Local<v8::Object> object,
                                             const char* key) {
    v8::Local<v8::Value> value;
    if (!object->Get(
            context,
            v8::String::NewFromUtf8(isolate, key).ToLocalChecked()
        ).ToLocal(&value) || value.IsEmpty() || value->IsNullOrUndefined()) {
        return "";
    }
    v8::String::Utf8Value text(isolate, value);
    return (*text && **text) ? std::string(*text) : "";
}

static std::string omnivm_v8_json_stringify(v8::Isolate* isolate,
                                            v8::Local<v8::Context> context,
                                            v8::Local<v8::Value> value) {
    if (value.IsEmpty() || value->IsNullOrUndefined()) {
        return "";
    }
    v8::Local<v8::String> json;
    {
        v8::TryCatch try_catch(isolate);
        if (v8::JSON::Stringify(context, value).ToLocal(&json) && !json.IsEmpty()) {
            v8::String::Utf8Value text(isolate, json);
            return (*text && **text) ? std::string(*text) : "";
        }
        if (try_catch.HasTerminated()) {
            return "";
        }
    }
    std::string fallback;
    std::vector<v8::Local<v8::Object>> seen;
    if (omnivm_v8_json_fallback_stringify(isolate, context, value, fallback, seen, 0)) {
        return fallback;
    }
    return "";
}

static std::string omnivm_v8_json_stringify_prop(v8::Isolate* isolate,
                                                 v8::Local<v8::Context> context,
                                                 v8::Local<v8::Object> object,
                                                 const char* key) {
    v8::Local<v8::Value> value;
    if (!object->Get(
            context,
            v8::String::NewFromUtf8(isolate, key).ToLocalChecked()
        ).ToLocal(&value) || value.IsEmpty() || value->IsNullOrUndefined()) {
        return "";
    }
    return omnivm_v8_json_stringify(isolate, context, value);
}

static std::string omnivm_v8_details_json_prop_fallback(v8::Isolate* isolate,
                                                        v8::Local<v8::Context> context,
                                                        v8::Local<v8::Object> object) {
    std::string details = omnivm_v8_json_stringify_prop(isolate, context, object, "details");
    if (!details.empty()) {
        return details;
    }
    const char* keys[] = {"details_json", "detailsJson"};
    for (const char* key : keys) {
        v8::Local<v8::Value> value;
        if (!object->Get(
                context,
                v8::String::NewFromUtf8(isolate, key).ToLocalChecked()
            ).ToLocal(&value) || value.IsEmpty() || value->IsNullOrUndefined()) {
            continue;
        }
        if (value->IsString()) {
            return omnivm_v8_value_string(isolate, context, value);
        }
        std::string json = omnivm_v8_json_stringify(isolate, context, value);
        if (!json.empty()) {
            return json;
        }
    }
    std::string issues = omnivm_v8_json_stringify_prop(isolate, context, object, "issues");
    if (!issues.empty()) {
        return "{\"issues\":" + issues + "}";
    }
    std::string errors = omnivm_v8_json_stringify_prop(isolate, context, object, "errors");
    if (!errors.empty()) {
        return "{\"errors\":" + errors + "}";
    }
    return "";
}

static std::string omnivm_v8_error_stack(v8::Isolate* isolate,
                                         v8::Local<v8::Context> context,
                                         v8::Local<v8::Value> value) {
    if (value.IsEmpty()) {
        return "";
    }
    if (value->IsObject()) {
        v8::Local<v8::Object> obj = value.As<v8::Object>();
        v8::Local<v8::String> stack_key =
            v8::String::NewFromUtf8Literal(isolate, "stack");
        v8::Local<v8::Value> stack;
        if (obj->Get(context, stack_key).ToLocal(&stack) && !stack.IsEmpty()) {
            std::string stack_text = omnivm_v8_value_string(isolate, context, stack);
            if (!stack_text.empty()) {
                return stack_text;
            }
        }
    }
    return omnivm_v8_value_string(isolate, context, value);
}

static void omnivm_v8_append_error_causes(v8::Isolate* isolate,
                                          v8::Local<v8::Context> context,
                                          v8::Local<v8::Value> value,
                                          std::string& out,
                                          int depth) {
    if (depth >= 16 || value.IsEmpty() || !value->IsObject()) {
        return;
    }
    v8::Local<v8::Object> obj = value.As<v8::Object>();
    v8::Local<v8::String> cause_key =
        v8::String::NewFromUtf8Literal(isolate, "cause");
    v8::Local<v8::Value> cause;
    if (!obj->Get(context, cause_key).ToLocal(&cause) ||
        cause.IsEmpty() ||
        cause->IsUndefined() ||
        cause->IsNull()) {
        return;
    }
    std::string cause_text = omnivm_v8_error_stack(isolate, context, cause);
    if (cause_text.empty()) {
        return;
    }
    out += "\nCaused by: ";
    out += cause_text;
    omnivm_v8_append_error_causes(isolate, context, cause, out, depth + 1);
}

static void omnivm_v8_append_aggregate_errors(v8::Isolate* isolate,
                                              v8::Local<v8::Context> context,
                                              v8::Local<v8::Value> value,
                                              std::string& out,
                                              int depth) {
    if (depth >= 4 || value.IsEmpty() || !value->IsObject()) {
        return;
    }
    v8::Local<v8::Object> obj = value.As<v8::Object>();
    v8::Local<v8::Value> errors_value;
    if (!obj->Get(
            context,
            v8::String::NewFromUtf8(isolate, "errors").ToLocalChecked()
        ).ToLocal(&errors_value) || errors_value.IsEmpty() || !errors_value->IsArray()) {
        return;
    }
    v8::Local<v8::Array> errors = errors_value.As<v8::Array>();
    uint32_t length = errors->Length();
    uint32_t limit = length < 64 ? length : 64;
    for (uint32_t i = 0; i < limit; ++i) {
        v8::Local<v8::Value> item;
        if (!errors->Get(context, i).ToLocal(&item) || item.IsEmpty()) {
            continue;
        }
        std::string item_text = omnivm_v8_error_stack(isolate, context, item);
        if (item_text.empty()) {
            continue;
        }
        out += "\nCaused by: ";
        out += item_text;
        omnivm_v8_append_error_causes(isolate, context, item, out, 0);
        omnivm_v8_append_aggregate_errors(isolate, context, item, out, depth + 1);
    }
    if (length > limit) {
        out += "\nCaused by: AggregateError: additional aggregate errors truncated";
    }
}

static void omnivm_v8_append_original_error_handle(v8::Isolate* isolate,
                                                  v8::Local<v8::Context> context,
                                                  v8::Local<v8::Value> value,
                                                  std::string& out) {
    if (value.IsEmpty() || !value->IsObject()) {
        return;
    }
    v8::Local<v8::Object> obj = value.As<v8::Object>();
    std::string handle = omnivm_v8_get_string_prop(isolate, context, obj, "originalErrorHandle");
    if (handle.empty()) {
        handle = omnivm_v8_get_string_prop(isolate, context, obj, "original_error_handle");
    }
    if (handle.empty()) {
        return;
    }
    out += "\nOriginal error handle: ";
    out += handle;
}

static void omnivm_v8_append_error_details(v8::Isolate* isolate,
                                           v8::Local<v8::Context> context,
                                           v8::Local<v8::Value> value,
                                           std::string& out) {
    if (value.IsEmpty() || !value->IsObject()) {
        return;
    }
    v8::Local<v8::Object> obj = value.As<v8::Object>();
    std::string details = omnivm_v8_details_json_prop_fallback(isolate, context, obj);
    if (details.empty()) {
        return;
    }
    out += "\nDetails: ";
    out += details;
}

static std::string omnivm_v8_format_runtime_error_object(v8::Isolate* isolate,
                                                        v8::Local<v8::Context> context,
                                                        v8::Local<v8::Value> value);

static char* omnivm_v8_format_exception(v8::Isolate* isolate,
                                        v8::Local<v8::Context> context,
                                        v8::TryCatch& try_catch,
                                        const char* fallback) {
    if (try_catch.HasTerminated()) {
        isolate->CancelTerminateExecution();
        return strdup("execution terminated (timeout)");
    }

    v8::Local<v8::Value> exception = try_catch.Exception();
    std::string runtime_error_text = omnivm_v8_format_runtime_error_object(isolate, context, exception);
    if (!runtime_error_text.empty()) {
        return strdup(runtime_error_text.c_str());
    }

    std::string text = omnivm_v8_error_stack(isolate, context, exception);
    if (!text.empty()) {
        omnivm_v8_append_error_causes(isolate, context, exception, text, 0);
        omnivm_v8_append_aggregate_errors(isolate, context, exception, text, 0);
        omnivm_v8_append_error_details(isolate, context, exception, text);
        omnivm_v8_append_original_error_handle(isolate, context, exception, text);
        return strdup(text.c_str());
    }

    if (!exception.IsEmpty()) {
        v8::String::Utf8Value err_str(isolate, exception);
        if (*err_str && **err_str) {
            return strdup(*err_str);
        }
    }
    return strdup(fallback);
}

struct OmniRuntimeErrorCause {
    std::string runtime;
    std::string origin_runtime;
    std::string type;
    std::string message;
    std::string traceback;
    std::vector<std::string> stack_frames;
    std::string boundary_path;
    std::string original_error_handle;
    std::string details_json;
};

struct OmniRuntimeErrorEnvelope {
    std::string runtime;
    std::string origin_runtime;
    std::string type;
    std::string message;
    std::string traceback;
    std::vector<std::string> stack_frames;
    std::vector<OmniRuntimeErrorCause> cause_chain;
    std::string boundary_path;
    std::string original_error_handle;
    std::string details_json;
};

static std::vector<std::string> omnivm_runtime_error_stack_frames(const std::string& traceback);

static bool omnivm_is_error_type_candidate(const std::string& value) {
    if (value.empty()) {
        return false;
    }
    unsigned char first = static_cast<unsigned char>(value[0]);
    if (!(std::isalpha(first) || value[0] == '_')) {
        return false;
    }
    for (char c : value) {
        unsigned char uc = static_cast<unsigned char>(c);
        if (!(std::isalnum(uc) || c == '_' || c == '.' || c == '$' || c == ':')) {
            return false;
        }
    }
    return true;
}

static std::string omnivm_trim(const std::string& value) {
    size_t start = value.find_first_not_of(" \t\r\n");
    if (start == std::string::npos) {
        return "";
    }
    size_t end = value.find_last_not_of(" \t\r\n");
    return value.substr(start, end - start + 1);
}

static std::string omnivm_v8_get_string_prop_fallback(v8::Isolate* isolate,
                                                      v8::Local<v8::Context> context,
                                                      v8::Local<v8::Object> object,
                                                      const char* preferred_key,
                                                      const char* fallback_key) {
    std::string value = omnivm_v8_get_string_prop(isolate, context, object, preferred_key);
    if (!value.empty()) {
        return value;
    }
    return omnivm_v8_get_string_prop(isolate, context, object, fallback_key);
}

static std::vector<std::string> omnivm_v8_get_string_array_prop_fallback(v8::Isolate* isolate,
                                                                         v8::Local<v8::Context> context,
                                                                         v8::Local<v8::Object> object,
                                                                         const char* preferred_key,
                                                                         const char* fallback_key) {
    v8::Local<v8::Value> value;
    if (!object->Get(
            context,
            v8::String::NewFromUtf8(isolate, preferred_key).ToLocalChecked()
        ).ToLocal(&value) || value.IsEmpty() || value->IsUndefined()) {
        if (!object->Get(
                context,
                v8::String::NewFromUtf8(isolate, fallback_key).ToLocalChecked()
            ).ToLocal(&value) || value.IsEmpty()) {
            return {};
        }
    }
    if (!value->IsArray()) {
        return {};
    }
    std::vector<std::string> out;
    v8::Local<v8::Array> array = value.As<v8::Array>();
    uint32_t length = array->Length();
    for (uint32_t i = 0; i < length; ++i) {
        v8::Local<v8::Value> item;
        if (!array->Get(context, i).ToLocal(&item) || item.IsEmpty() || !item->IsString()) {
            return {};
        }
        out.push_back(omnivm_v8_value_string(isolate, context, item));
    }
    return out;
}

static bool omnivm_v8_parse_runtime_error_envelope_object(v8::Isolate* isolate,
                                                          v8::Local<v8::Context> context,
                                                          v8::Local<v8::Object> object,
                                                          const std::string& fallback_runtime,
                                                          const std::string& fallback_boundary,
                                                          OmniRuntimeErrorEnvelope& env) {
    env.runtime = omnivm_v8_get_string_prop(isolate, context, object, "runtime");
    if (env.runtime.empty()) {
        env.runtime = fallback_runtime;
    }
    env.origin_runtime = omnivm_v8_get_string_prop_fallback(isolate, context, object, "origin_runtime", "originRuntime");
    if (env.origin_runtime.empty()) {
        env.origin_runtime = env.runtime;
    }
    env.type = omnivm_v8_get_string_prop_fallback(isolate, context, object, "type", "name");
    env.message = omnivm_v8_get_string_prop(isolate, context, object, "message");
    env.traceback = omnivm_v8_get_string_prop_fallback(isolate, context, object, "traceback", "stack");
    if (env.runtime.empty() && env.type.empty() && env.message.empty() && env.traceback.empty()) {
        return false;
    }
    env.stack_frames = omnivm_v8_get_string_array_prop_fallback(isolate, context, object, "stack_frames", "stackFrames");
    if (env.stack_frames.empty() && !env.traceback.empty()) {
        env.stack_frames = omnivm_runtime_error_stack_frames(env.traceback);
    }
    env.boundary_path = omnivm_v8_get_string_prop_fallback(isolate, context, object, "boundary_path", "boundaryPath");
    if (env.boundary_path.empty()) {
        env.boundary_path = fallback_boundary;
    }
    env.original_error_handle = omnivm_v8_get_string_prop_fallback(isolate, context, object, "original_error_handle", "originalErrorHandle");
    env.details_json = omnivm_v8_details_json_prop_fallback(isolate, context, object);

    v8::Local<v8::Value> causes_value;
    if (!object->Get(
            context,
            v8::String::NewFromUtf8Literal(isolate, "cause_chain")
        ).ToLocal(&causes_value) || causes_value.IsEmpty() || causes_value->IsUndefined()) {
        object->Get(
            context,
            v8::String::NewFromUtf8Literal(isolate, "causeChain")
        ).ToLocal(&causes_value);
    }
    if (causes_value.IsEmpty() || !causes_value->IsArray()) {
        return true;
    }
    v8::Local<v8::Array> causes = causes_value.As<v8::Array>();
    uint32_t length = causes->Length();
    for (uint32_t i = 0; i < length; ++i) {
        v8::Local<v8::Value> cause_value;
        if (!causes->Get(context, i).ToLocal(&cause_value) || cause_value.IsEmpty() || !cause_value->IsObject()) {
            continue;
        }
        v8::Local<v8::Object> cause_object = cause_value.As<v8::Object>();
        OmniRuntimeErrorCause cause;
        cause.runtime = omnivm_v8_get_string_prop(isolate, context, cause_object, "runtime");
        if (cause.runtime.empty()) {
            cause.runtime = env.runtime;
        }
        cause.origin_runtime = omnivm_v8_get_string_prop_fallback(isolate, context, cause_object, "origin_runtime", "originRuntime");
        if (cause.origin_runtime.empty()) {
            cause.origin_runtime = cause.runtime;
        }
        cause.type = omnivm_v8_get_string_prop_fallback(isolate, context, cause_object, "type", "name");
        cause.message = omnivm_v8_get_string_prop(isolate, context, cause_object, "message");
        cause.traceback = omnivm_v8_get_string_prop_fallback(isolate, context, cause_object, "traceback", "stack");
        cause.stack_frames = omnivm_v8_get_string_array_prop_fallback(isolate, context, cause_object, "stack_frames", "stackFrames");
        if (cause.stack_frames.empty() && !cause.traceback.empty()) {
            cause.stack_frames = omnivm_runtime_error_stack_frames(cause.traceback);
        }
        cause.boundary_path = omnivm_v8_get_string_prop_fallback(isolate, context, cause_object, "boundary_path", "boundaryPath");
        cause.original_error_handle = omnivm_v8_get_string_prop_fallback(isolate, context, cause_object, "original_error_handle", "originalErrorHandle");
        cause.details_json = omnivm_v8_details_json_prop_fallback(isolate, context, cause_object);
        env.cause_chain.push_back(cause);
    }
    return true;
}

static std::string omnivm_v8_format_runtime_error_object(v8::Isolate* isolate,
                                                        v8::Local<v8::Context> context,
                                                        v8::Local<v8::Value> value) {
    if (value.IsEmpty() || !value->IsObject()) {
        return "";
    }
    v8::Local<v8::Object> object = value.As<v8::Object>();
    std::string runtime = omnivm_v8_get_string_prop(isolate, context, object, "runtime");
    if (runtime.empty()) {
        return "";
    }
    std::string type = omnivm_v8_get_string_prop_fallback(isolate, context, object, "type", "name");
    std::string message = omnivm_v8_get_string_prop(isolate, context, object, "message");
    std::string traceback = omnivm_v8_get_string_prop_fallback(isolate, context, object, "traceback", "stack");
    std::string handle = omnivm_v8_get_string_prop_fallback(isolate, context, object, "original_error_handle", "originalErrorHandle");

    std::string out = runtime + ": ";
    if (!type.empty()) {
        out += type + ": ";
    }
    out += message;
    if (!traceback.empty()) {
        out += "\n" + traceback;
    }

    v8::Local<v8::Value> causes_value;
    if (object->Get(
            context,
            v8::String::NewFromUtf8Literal(isolate, "cause_chain")
        ).ToLocal(&causes_value) && causes_value->IsArray()) {
        // handled below
    } else if (object->Get(
            context,
            v8::String::NewFromUtf8Literal(isolate, "causeChain")
        ).ToLocal(&causes_value) && causes_value->IsArray()) {
        // handled below
    }
    if (!causes_value.IsEmpty() && causes_value->IsArray()) {
        v8::Local<v8::Array> causes = causes_value.As<v8::Array>();
        uint32_t length = causes->Length();
        for (uint32_t i = 0; i < length; ++i) {
            v8::Local<v8::Value> cause_value;
            if (!causes->Get(context, i).ToLocal(&cause_value) || !cause_value->IsObject()) {
                continue;
            }
            v8::Local<v8::Object> cause = cause_value.As<v8::Object>();
            std::string cause_type = omnivm_v8_get_string_prop_fallback(isolate, context, cause, "type", "name");
            std::string cause_message = omnivm_v8_get_string_prop(isolate, context, cause, "message");
            out += "\nCaused by: ";
            if (!cause_type.empty()) {
                out += cause_type + ": ";
            }
            out += cause_message;
        }
    }
    std::string details = omnivm_v8_details_json_prop_fallback(isolate, context, object);
    if (!details.empty()) {
        out += "\nDetails: " + details;
    }
    if (!handle.empty()) {
        out += "\nOriginal error handle: " + handle;
    }
    return out;
}

static bool omnivm_starts_with(const std::string& value, const char* prefix) {
    size_t len = strlen(prefix);
    return value.size() >= len && value.compare(0, len, prefix) == 0;
}

static bool omnivm_is_runtime_error_metadata_line(const std::string& line) {
    std::string lower = line;
    std::transform(lower.begin(), lower.end(), lower.begin(), [](unsigned char c) {
        return static_cast<char>(std::tolower(c));
    });
    return omnivm_starts_with(lower, "caused by:") ||
           omnivm_starts_with(lower, "details:") ||
           omnivm_starts_with(lower, "original_error_handle:") ||
           omnivm_starts_with(lower, "original error handle:") ||
           omnivm_starts_with(lower, "original-error-handle:");
}

static std::vector<std::string> omnivm_runtime_error_stack_frames(const std::string& traceback) {
    std::vector<std::string> frames;
    size_t offset = 0;
    while (offset <= traceback.size()) {
        size_t next = traceback.find('\n', offset);
        std::string line = next == std::string::npos
            ? traceback.substr(offset)
            : traceback.substr(offset, next - offset);
        std::string stripped = omnivm_trim(line);
        if (!stripped.empty() && !omnivm_is_runtime_error_metadata_line(stripped)) {
            frames.push_back(stripped);
        }
        if (next == std::string::npos) {
            break;
        }
        offset = next + 1;
    }
    return frames;
}

static void omnivm_parse_runtime_prefix(std::string& body, std::string& runtime) {
    struct RuntimePrefix {
        const char* prefix;
        const char* runtime;
    };
    static const RuntimePrefix prefixes[] = {
        {"javascript: ", "javascript"},
        {"python: ", "python"},
        {"ruby: ", "ruby"},
        {"jvm: ", "java"},
        {"java: ", "java"},
        {"go: ", "go"},
    };
    bool changed = true;
    while (changed) {
        changed = false;
        for (const auto& entry : prefixes) {
            if (omnivm_starts_with(body, entry.prefix)) {
                runtime = entry.runtime;
                body = body.substr(strlen(entry.prefix));
                changed = true;
                break;
            }
        }
    }
}

static OmniRuntimeErrorEnvelope omnivm_parse_runtime_error_text(
    const std::string& text,
    const std::string& runtime_hint) {
    OmniRuntimeErrorEnvelope env;
    env.runtime = runtime_hint;
    env.boundary_path = runtime_hint.empty() ? "" : "call[" + runtime_hint + "]";

    std::string body = text;
    std::vector<std::string> boundary_parts;

    struct BoundaryPrefix {
        const char* prefix;
        const char* label;
    };
    static const BoundaryPrefix boundary_prefixes[] = {
        {"execute manifest: ", "execute manifest"},
        {"load manifest module: ", "load manifest module"},
        {"manifest module call: ", "manifest module call"},
    };
    for (const auto& entry : boundary_prefixes) {
        if (omnivm_starts_with(body, entry.prefix)) {
            boundary_parts.emplace_back(entry.label);
            body = body.substr(strlen(entry.prefix));
            break;
        }
    }

    std::smatch op_match;
    static const std::regex op_re(
        R"(^([A-Za-z_][A-Za-z0-9_]*) \[([A-Za-z0-9_-]+)\]: ([\s\S]*))");
    if (std::regex_match(body, op_match, op_re)) {
        std::string op_name = op_match[1].str();
        std::string op_runtime = op_match[2].str();
        boundary_parts.push_back(op_name + "[" + op_runtime + "]");
        env.runtime = op_runtime;
        body = op_match[3].str();
    }

    std::smatch runtime_ref_assign_match;
    static const std::regex runtime_ref_assign_re(
        R"(^runtime ref assign \[([A-Za-z0-9_-]+)\]: ([\s\S]*))");
    if (std::regex_match(body, runtime_ref_assign_match, runtime_ref_assign_re)) {
        env.runtime = runtime_ref_assign_match[1].str();
        body = runtime_ref_assign_match[2].str();
    }

    omnivm_parse_runtime_prefix(body, env.runtime);
    env.origin_runtime = env.runtime;

    size_t newline = body.find('\n');
    std::string first_line = newline == std::string::npos ? body : body.substr(0, newline);
    std::string rest = newline == std::string::npos ? "" : body.substr(newline + 1);
    std::string parse_line = first_line;
    env.traceback = rest;
    env.message = first_line;

    static const std::regex handle_re(
        R"((^|\n)\s*(Original[- ]error[- ]handle|original_error_handle):\s*(\S+)\s*($|\n))",
        std::regex_constants::icase);
    std::smatch handle_match;
    if (std::regex_search(body, handle_match, handle_re)) {
        env.original_error_handle = handle_match[3].str();
    }
    static const std::regex details_re(
        R"((^|\n)\s*Details:\s*([^\n\r]+)\s*($|\n))");
    std::smatch details_match;
    if (std::regex_search(body, details_match, details_re)) {
        env.details_json = details_match[2].str();
    }

    if (omnivm_starts_with(first_line, "Traceback ")) {
        env.traceback = body;
        size_t pos = body.size();
        while (pos > 0) {
            size_t prev = body.rfind('\n', pos - 1);
            size_t line_start = prev == std::string::npos ? 0 : prev + 1;
            std::string line = omnivm_trim(body.substr(line_start, pos - line_start));
            if (!line.empty() && !omnivm_is_runtime_error_metadata_line(line)) {
                size_t sep = line.find(": ");
                if (sep != std::string::npos &&
                    omnivm_is_error_type_candidate(line.substr(0, sep))) {
                    parse_line = line;
                    break;
                }
            }
            if (prev == std::string::npos) {
                break;
            }
            pos = prev;
        }
    }

    size_t sep = parse_line.find(": ");
    if (sep != std::string::npos) {
        std::string candidate = parse_line.substr(0, sep);
        std::string detail = parse_line.substr(sep + 2);
        if (omnivm_is_error_type_candidate(candidate)) {
            if (env.runtime == "python") {
                size_t dot = candidate.rfind('.');
                if (dot != std::string::npos) {
                    candidate = candidate.substr(dot + 1);
                }
            }
            env.type = candidate;
            env.message = detail;
        }
    }

    size_t offset = 0;
    while (offset <= rest.size()) {
        size_t next = rest.find('\n', offset);
        std::string line = next == std::string::npos ? rest.substr(offset) : rest.substr(offset, next - offset);
        std::string stripped = omnivm_trim(line);
        const char* cause_prefix = "Caused by: ";
        if (omnivm_starts_with(stripped, cause_prefix)) {
            std::string cause_text = stripped.substr(strlen(cause_prefix));
            OmniRuntimeErrorCause cause;
            cause.runtime = env.runtime;
            cause.origin_runtime = env.runtime;
            cause.message = cause_text;
            size_t cause_sep = cause_text.find(": ");
            if (cause_sep != std::string::npos) {
                std::string candidate = cause_text.substr(0, cause_sep);
                if (omnivm_is_error_type_candidate(candidate)) {
                    cause.type = candidate;
                    cause.message = cause_text.substr(cause_sep + 2);
                }
            }
            env.cause_chain.push_back(cause);
        }
        if (next == std::string::npos) {
            break;
        }
        offset = next + 1;
    }

    env.stack_frames = omnivm_runtime_error_stack_frames(env.traceback);

    if (!boundary_parts.empty()) {
        env.boundary_path.clear();
        for (size_t i = 0; i < boundary_parts.size(); ++i) {
            if (i > 0) {
                env.boundary_path += " > ";
            }
            env.boundary_path += boundary_parts[i];
        }
    } else if (!env.runtime.empty() && env.runtime != runtime_hint) {
        env.boundary_path = "call[" + env.runtime + "]";
    }

    return env;
}

static bool omnivm_v8_parse_runtime_error_envelope_text(v8::Isolate* isolate,
                                                        v8::Local<v8::Context> context,
                                                        const std::string& text,
                                                        const std::string& runtime_hint,
                                                        OmniRuntimeErrorEnvelope& env) {
    std::string body = omnivm_trim(text);
    std::string source_runtime = runtime_hint;
    std::vector<std::string> boundary_parts;

    struct BoundaryPrefix {
        const char* prefix;
        const char* label;
    };
    static const BoundaryPrefix boundary_prefixes[] = {
        {"execute manifest: ", "execute manifest"},
        {"load manifest module: ", "load manifest module"},
        {"manifest module call: ", "manifest module call"},
    };
    for (const auto& entry : boundary_prefixes) {
        if (omnivm_starts_with(body, entry.prefix)) {
            boundary_parts.emplace_back(entry.label);
            body = body.substr(strlen(entry.prefix));
            break;
        }
    }

    std::smatch op_match;
    static const std::regex op_re(
        R"(^([A-Za-z_][A-Za-z0-9_]*) \[([A-Za-z0-9_-]+)\]: ([\s\S]*))");
    if (std::regex_match(body, op_match, op_re)) {
        std::string op_name = op_match[1].str();
        std::string op_runtime = op_match[2].str();
        boundary_parts.push_back(op_name + "[" + op_runtime + "]");
        source_runtime = op_runtime;
        body = op_match[3].str();
    }

    std::smatch runtime_ref_assign_match;
    static const std::regex runtime_ref_assign_re(
        R"(^runtime ref assign \[([A-Za-z0-9_-]+)\]: ([\s\S]*))");
    if (std::regex_match(body, runtime_ref_assign_match, runtime_ref_assign_re)) {
        source_runtime = runtime_ref_assign_match[1].str();
        body = runtime_ref_assign_match[2].str();
    }

    omnivm_parse_runtime_prefix(body, source_runtime);
    body = omnivm_trim(body);
    if (!omnivm_starts_with(body, "{")) {
        return false;
    }

    std::string boundary_path;
    if (!boundary_parts.empty()) {
        for (size_t i = 0; i < boundary_parts.size(); ++i) {
            if (i > 0) {
                boundary_path += " > ";
            }
            boundary_path += boundary_parts[i];
        }
    } else if (!source_runtime.empty()) {
        boundary_path = "call[" + source_runtime + "]";
    }

    v8::Local<v8::String> json_text =
        v8::String::NewFromUtf8(isolate, body.c_str()).ToLocalChecked();
    v8::Local<v8::Value> parsed;
    if (!v8::JSON::Parse(context, json_text).ToLocal(&parsed) || parsed.IsEmpty() || !parsed->IsObject()) {
        return false;
    }
    return omnivm_v8_parse_runtime_error_envelope_object(
        isolate,
        context,
        parsed.As<v8::Object>(),
        source_runtime,
        boundary_path,
        env);
}

static void omnivm_v8_set_string_prop(v8::Isolate* isolate,
                                      v8::Local<v8::Context> context,
                                      v8::Local<v8::Object> object,
                                      const char* key,
                                      const std::string& value) {
    object->Set(
        context,
        v8::String::NewFromUtf8(isolate, key).ToLocalChecked(),
        v8::String::NewFromUtf8(isolate, value.c_str()).ToLocalChecked()
    ).ToChecked();
}

static void omnivm_v8_set_string_array_prop(v8::Isolate* isolate,
                                            v8::Local<v8::Context> context,
                                            v8::Local<v8::Object> object,
                                            const char* key,
                                            const std::vector<std::string>& values) {
    v8::Local<v8::Array> array = v8::Array::New(isolate, static_cast<int>(values.size()));
    for (uint32_t i = 0; i < values.size(); ++i) {
        array->Set(
            context,
            i,
            v8::String::NewFromUtf8(isolate, values[i].c_str()).ToLocalChecked()
        ).ToChecked();
    }
    object->Set(
        context,
        v8::String::NewFromUtf8(isolate, key).ToLocalChecked(),
        array
    ).ToChecked();
}

static v8::Local<v8::Value> omnivm_v8_json_clone_value(v8::Isolate* isolate,
                                                       v8::Local<v8::Context> context,
                                                       v8::Local<v8::Value> value) {
    if (value.IsEmpty()) {
        return v8::Null(isolate);
    }
    if (!value->IsObject()) {
        return value;
    }
    std::string json = omnivm_v8_json_stringify(isolate, context, value);
    if (json.empty()) {
        return value;
    }
    v8::Local<v8::String> json_text =
        v8::String::NewFromUtf8(isolate, json.c_str()).ToLocalChecked();
    v8::Local<v8::Value> parsed;
    if (!v8::JSON::Parse(context, json_text).ToLocal(&parsed) || parsed.IsEmpty()) {
        return value;
    }
    return parsed;
}

static void omnivm_v8_copy_prop(v8::Isolate* isolate,
                                v8::Local<v8::Context> context,
                                v8::Local<v8::Object> source,
                                v8::Local<v8::Object> target,
                                const char* source_key,
                                const char* target_key) {
    v8::Local<v8::Value> value;
    if (!source->Get(
            context,
            v8::String::NewFromUtf8(isolate, source_key).ToLocalChecked()
        ).ToLocal(&value)) {
        value = v8::Null(isolate);
    }
    value = omnivm_v8_json_clone_value(isolate, context, value);
    target->Set(
        context,
        v8::String::NewFromUtf8(isolate, target_key).ToLocalChecked(),
        value
    ).ToChecked();
}

static void omnivm_v8_copy_prop_fallback(v8::Isolate* isolate,
                                         v8::Local<v8::Context> context,
                                         v8::Local<v8::Object> source,
                                         v8::Local<v8::Object> target,
                                         const char* preferred_key,
                                         const char* fallback_key,
                                         const char* target_key) {
    v8::Local<v8::Value> value;
    if (!source->Get(
            context,
            v8::String::NewFromUtf8(isolate, preferred_key).ToLocalChecked()
        ).ToLocal(&value) ||
        value->IsUndefined()) {
        if (!source->Get(
                context,
                v8::String::NewFromUtf8(isolate, fallback_key).ToLocalChecked()
            ).ToLocal(&value)) {
            value = v8::Null(isolate);
        }
    }
    value = omnivm_v8_json_clone_value(isolate, context, value);
    target->Set(
        context,
        v8::String::NewFromUtf8(isolate, target_key).ToLocalChecked(),
        value
    ).ToChecked();
}

static void omnivm_v8_runtime_error_to_json(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();
    v8::Local<v8::Context> context = isolate->GetCurrentContext();
    v8::Local<v8::Object> out = v8::Object::New(isolate);
    if (!info.This().IsEmpty() && info.This()->IsObject()) {
        v8::Local<v8::Object> error = info.This();
        omnivm_v8_copy_prop(isolate, context, error, out, "runtime", "runtime");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "origin_runtime", "originRuntime", "origin_runtime");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "type", "name", "type");
        omnivm_v8_copy_prop(isolate, context, error, out, "message", "message");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "traceback", "stack", "traceback");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "stack_frames", "stackFrames", "stack_frames");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "cause_chain", "causeChain", "cause_chain");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "boundary_path", "boundaryPath", "boundary_path");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "original_error_handle", "originalErrorHandle", "original_error_handle");
        omnivm_v8_copy_prop(isolate, context, error, out, "details", "details");
        omnivm_v8_copy_prop_fallback(isolate, context, error, out, "details_json", "detailsJson", "details_json");
    }
    info.GetReturnValue().Set(out);
}

static void omnivm_v8_set_runtime_error_props(v8::Isolate* isolate,
                                              v8::Local<v8::Context> context,
                                              v8::Local<v8::Value> error_value,
                                              const OmniRuntimeErrorEnvelope& env) {
    if (error_value.IsEmpty() || !error_value->IsObject()) {
        return;
    }
    v8::Local<v8::Object> error = error_value.As<v8::Object>();
    std::string origin_runtime = env.origin_runtime.empty() ? env.runtime : env.origin_runtime;
    omnivm_v8_set_string_prop(isolate, context, error, "runtime", env.runtime);
    omnivm_v8_set_string_prop(isolate, context, error, "originRuntime", origin_runtime);
    omnivm_v8_set_string_prop(isolate, context, error, "origin_runtime", origin_runtime);
    omnivm_v8_set_string_prop(isolate, context, error, "type", env.type);
    omnivm_v8_set_string_prop(isolate, context, error, "traceback", env.traceback);
    omnivm_v8_set_string_array_prop(isolate, context, error, "stackFrames", env.stack_frames);
    omnivm_v8_set_string_array_prop(isolate, context, error, "stack_frames", env.stack_frames);
    omnivm_v8_set_string_prop(isolate, context, error, "boundaryPath", env.boundary_path);
    omnivm_v8_set_string_prop(isolate, context, error, "boundary_path", env.boundary_path);
    if (env.original_error_handle.empty()) {
        error->Set(
            context,
            v8::String::NewFromUtf8Literal(isolate, "originalErrorHandle"),
            v8::Null(isolate)
        ).ToChecked();
        error->Set(
            context,
            v8::String::NewFromUtf8Literal(isolate, "original_error_handle"),
            v8::Null(isolate)
        ).ToChecked();
    } else {
        omnivm_v8_set_string_prop(isolate, context, error, "originalErrorHandle", env.original_error_handle);
        omnivm_v8_set_string_prop(isolate, context, error, "original_error_handle", env.original_error_handle);
    }

    v8::Local<v8::Array> causes = v8::Array::New(isolate, static_cast<int>(env.cause_chain.size()));
    for (uint32_t i = 0; i < env.cause_chain.size(); ++i) {
        v8::Local<v8::Object> cause = v8::Object::New(isolate);
        if (!env.cause_chain[i].runtime.empty()) {
            omnivm_v8_set_string_prop(isolate, context, cause, "runtime", env.cause_chain[i].runtime);
        }
        if (!env.cause_chain[i].origin_runtime.empty()) {
            omnivm_v8_set_string_prop(isolate, context, cause, "originRuntime", env.cause_chain[i].origin_runtime);
            omnivm_v8_set_string_prop(isolate, context, cause, "origin_runtime", env.cause_chain[i].origin_runtime);
        }
        omnivm_v8_set_string_prop(isolate, context, cause, "type", env.cause_chain[i].type);
        omnivm_v8_set_string_prop(isolate, context, cause, "message", env.cause_chain[i].message);
        if (!env.cause_chain[i].traceback.empty()) {
            omnivm_v8_set_string_prop(isolate, context, cause, "traceback", env.cause_chain[i].traceback);
        }
        if (!env.cause_chain[i].stack_frames.empty()) {
            omnivm_v8_set_string_array_prop(isolate, context, cause, "stackFrames", env.cause_chain[i].stack_frames);
            omnivm_v8_set_string_array_prop(isolate, context, cause, "stack_frames", env.cause_chain[i].stack_frames);
        }
        if (!env.cause_chain[i].boundary_path.empty()) {
            omnivm_v8_set_string_prop(isolate, context, cause, "boundaryPath", env.cause_chain[i].boundary_path);
            omnivm_v8_set_string_prop(isolate, context, cause, "boundary_path", env.cause_chain[i].boundary_path);
        }
        if (!env.cause_chain[i].original_error_handle.empty()) {
            omnivm_v8_set_string_prop(isolate, context, cause, "originalErrorHandle", env.cause_chain[i].original_error_handle);
            omnivm_v8_set_string_prop(isolate, context, cause, "original_error_handle", env.cause_chain[i].original_error_handle);
        }
        if (!env.cause_chain[i].details_json.empty()) {
            v8::Local<v8::String> details_text =
                v8::String::NewFromUtf8(isolate, env.cause_chain[i].details_json.c_str()).ToLocalChecked();
            cause->Set(
                context,
                v8::String::NewFromUtf8Literal(isolate, "detailsJson"),
                details_text
            ).ToChecked();
            cause->Set(
                context,
                v8::String::NewFromUtf8Literal(isolate, "details_json"),
                details_text
            ).ToChecked();
            v8::Local<v8::Value> details;
            if (v8::JSON::Parse(context, details_text).ToLocal(&details) && !details.IsEmpty()) {
                cause->Set(
                    context,
                    v8::String::NewFromUtf8Literal(isolate, "details"),
                    details
                ).ToChecked();
            } else {
                cause->Set(
                    context,
                    v8::String::NewFromUtf8Literal(isolate, "details"),
                    details_text
                ).ToChecked();
            }
        }
        causes->Set(context, i, cause).ToChecked();
    }
    error->Set(
        context,
        v8::String::NewFromUtf8Literal(isolate, "causeChain"),
        causes
    ).ToChecked();
    error->Set(
        context,
        v8::String::NewFromUtf8Literal(isolate, "cause_chain"),
        causes
    ).ToChecked();

    v8::Local<v8::Value> details = v8::Null(isolate);
    if (!env.details_json.empty()) {
        v8::Local<v8::String> details_text =
            v8::String::NewFromUtf8(isolate, env.details_json.c_str()).ToLocalChecked();
        v8::Local<v8::Value> parsed;
        if (v8::JSON::Parse(context, details_text).ToLocal(&parsed) && !parsed.IsEmpty()) {
            details = parsed;
        } else {
            details = details_text;
        }
    }
    error->Set(
        context,
        v8::String::NewFromUtf8Literal(isolate, "details"),
        details
    ).ToChecked();
    if (env.details_json.empty()) {
        error->Set(
            context,
            v8::String::NewFromUtf8Literal(isolate, "detailsJson"),
            v8::Null(isolate)
        ).ToChecked();
        error->Set(
            context,
            v8::String::NewFromUtf8Literal(isolate, "details_json"),
            v8::Null(isolate)
        ).ToChecked();
    } else {
        omnivm_v8_set_string_prop(isolate, context, error, "detailsJson", env.details_json);
        omnivm_v8_set_string_prop(isolate, context, error, "details_json", env.details_json);
    }
    v8::Local<v8::Function> to_json;
    if (v8::Function::New(context, omnivm_v8_runtime_error_to_json).ToLocal(&to_json)) {
        error->Set(
            context,
            v8::String::NewFromUtf8Literal(isolate, "toJSON"),
            to_json
        ).ToChecked();
    }
}

static void OmniExternalBufferDeleter(void* data, size_t length, void* deleter_data) {
    (void)data;
    (void)length;
    auto* lease = static_cast<OmniExternalBufferLease*>(deleter_data);
    if (lease) {
        if (g_buf_release && lease->name) {
            g_buf_release(lease->name);
        }
        free(lease->name);
        delete lease;
    }
}

static bool OmniTypedArrayMetadata(v8::Local<v8::Value> value,
                                   int32_t* dtype,
                                   const char** arrow_format,
                                   size_t* elem_size) {
    if (value->IsArrayBuffer()) {
        *dtype = 0;
        *arrow_format = "C";
        *elem_size = 1;
        return true;
    }
    if (value->IsDataView()) {
        *dtype = 0;
        *arrow_format = "C";
        *elem_size = 1;
        return true;
    }
    if (value->IsInt8Array()) {
        *dtype = 10;
        *arrow_format = "c";
        *elem_size = 1;
        return true;
    }
    if (value->IsUint8Array() || value->IsUint8ClampedArray()) {
        *dtype = 11;
        *arrow_format = "C";
        *elem_size = 1;
        return true;
    }
    if (value->IsInt16Array()) {
        *dtype = 6;
        *arrow_format = "s";
        *elem_size = 2;
        return true;
    }
    if (value->IsUint16Array()) {
        *dtype = 7;
        *arrow_format = "S";
        *elem_size = 2;
        return true;
    }
    if (value->IsInt32Array()) {
        *dtype = 1;
        *arrow_format = "i";
        *elem_size = 4;
        return true;
    }
    if (value->IsUint32Array()) {
        *dtype = 8;
        *arrow_format = "I";
        *elem_size = 4;
        return true;
    }
    if (value->IsBigInt64Array()) {
        *dtype = 2;
        *arrow_format = "l";
        *elem_size = 8;
        return true;
    }
    if (value->IsBigUint64Array()) {
        *dtype = 9;
        *arrow_format = "L";
        *elem_size = 8;
        return true;
    }
    if (value->IsFloat32Array()) {
        *dtype = 3;
        *arrow_format = "f";
        *elem_size = 4;
        return true;
    }
    if (value->IsFloat64Array()) {
        *dtype = 4;
        *arrow_format = "g";
        *elem_size = 8;
        return true;
    }
    return false;
}

// Node.js per-process init result (shared_ptr since Node 24)
static std::shared_ptr<node::InitializationResult> init_result;

// ---- omnivm.call() V8 native function ----

static void OmnivmCallCallback(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();
    v8::Local<v8::Context> context = isolate->GetCurrentContext();

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

    char* result;
    {
        // Release V8 Isolate lock so other threads (Golden Thread pump,
        // other JVM threads) can enter V8 while we wait in another runtime.
        v8::Unlocker unlocker(isolate);
        result = g_bridge_call(*runtime, *code);
    }
    // Locker automatically reacquired when unlocker goes out of scope

    if (!result) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "omnivm.call returned NULL")));
        return;
    }

    // Check for error prefix — same protocol as Duktape/v8_bridge.cc
    if (strncmp(result, "ERR:", 4) == 0) {
        std::string err_msg(result + 4);
        std::string runtime_hint(*runtime && **runtime ? *runtime : "");
        OmniRuntimeErrorEnvelope envelope;
        if (!omnivm_v8_parse_runtime_error_envelope_text(isolate, context, err_msg, runtime_hint, envelope)) {
            envelope = omnivm_parse_runtime_error_text(err_msg, runtime_hint);
        }
        if (g_bridge_free) g_bridge_free(result);
        std::string display_message = envelope.message.empty() ? err_msg : envelope.message;
        v8::Local<v8::Value> error_value = v8::Exception::Error(
            v8::String::NewFromUtf8(isolate, display_message.c_str()).ToLocalChecked());
        omnivm_v8_set_runtime_error_props(isolate, context, error_value, envelope);
        isolate->ThrowException(error_value);
        return;
    }

    v8::Local<v8::String> ret =
        v8::String::NewFromUtf8(isolate, result).ToLocalChecked();
    if (g_bridge_free) g_bridge_free(result);
    info.GetReturnValue().Set(ret);
}

// omnivm.getBuffer(name) -> ArrayBuffer or null
static void OmnivmGetBufferCallback(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();
    if (info.Length() < 1 || !info[0]->IsString()) {
        isolate->ThrowException(v8::Exception::TypeError(
            v8::String::NewFromUtf8Literal(isolate, "getBuffer requires a string name")));
        return;
    }
    if (!g_buf_get) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "buffer bridge not initialized")));
        return;
    }

    v8::String::Utf8Value name(isolate, info[0]);
    omni_buffer_t buf;
    memset(&buf, 0, sizeof(buf));

    int rc;
    {
        v8::Unlocker unlocker(isolate);
        rc = g_buf_get(*name, &buf);
    }
    if (rc != 0) {
        info.GetReturnValue().Set(v8::Null(isolate));
        return;
    }

    if (buf.data == nullptr || buf.len <= 0) {
        if (g_buf_release) {
            g_buf_release(*name);
        }
        info.GetReturnValue().Set(v8::Null(isolate));
        return;
    }

    if (buf.read_only != 0) {
        auto backing = v8::ArrayBuffer::NewBackingStore(isolate, buf.len);
        memcpy(backing->Data(), buf.data, buf.len);
        if (g_buf_release) {
            g_buf_release(*name);
        }
        auto ab = v8::ArrayBuffer::New(isolate, std::move(backing));
        info.GetReturnValue().Set(ab);
        return;
    }

    auto* lease = new OmniExternalBufferLease();
    size_t name_len = strlen(*name);
    lease->name = static_cast<char*>(malloc(name_len + 1));
    if (!lease->name) {
        delete lease;
        if (g_buf_release) {
            g_buf_release(*name);
        }
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "getBuffer allocation failed")));
        return;
    }
    memcpy(lease->name, *name, name_len + 1);

    auto backing = v8::ArrayBuffer::NewBackingStore(
        buf.data,
        static_cast<size_t>(buf.len),
        OmniExternalBufferDeleter,
        lease);
    auto ab = v8::ArrayBuffer::New(isolate, std::move(backing));
    info.GetReturnValue().Set(ab);
}

// omnivm.setBuffer(name, arrayBuffer, dtype=0)
static void OmnivmSetBufferCallback(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();
    if (info.Length() < 2 || !info[0]->IsString() || !info[1]->IsArrayBuffer()) {
        isolate->ThrowException(v8::Exception::TypeError(
            v8::String::NewFromUtf8Literal(isolate, "setBuffer(name, arrayBuffer[, dtype])")));
        return;
    }
    if (!g_buf_set) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "buffer bridge not initialized")));
        return;
    }

    v8::String::Utf8Value name(isolate, info[0]);
    v8::Local<v8::ArrayBuffer> ab = info[1].As<v8::ArrayBuffer>();
    int32_t dtype = 0;
    if (info.Length() > 2 && info[2]->IsInt32()) {
        dtype = info[2].As<v8::Int32>()->Value();
    }

    omni_buffer_t buf;
    buf.data = ab->GetBackingStore()->Data();
    buf.len = static_cast<int64_t>(ab->ByteLength());
    buf.dtype = dtype;
    buf.owned = 0;
    buf.read_only = 0;

    int rc;
    {
        v8::Unlocker unlocker(isolate);
        rc = g_buf_set(*name, buf);
    }

    if (rc != 0) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "setBuffer failed")));
    }
}

// omnivm.releaseBuffer(name)
static void OmnivmReleaseBufferCallback(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();
    v8::Local<v8::Context> context = isolate->GetCurrentContext();
    if (info.Length() < 1 || !info[0]->IsString()) {
        isolate->ThrowException(v8::Exception::TypeError(
            v8::String::NewFromUtf8Literal(isolate, "releaseBuffer requires a string name")));
        return;
    }
    if (!g_buf_free) return;

    v8::String::Utf8Value name(isolate, info[0]);
    if (g_buf_free(*name) != 0) {
        std::string status_json;
        if (g_buf_status) {
            char* raw_status = g_buf_status(*name);
            if (raw_status) {
                status_json = raw_status;
                if (g_bridge_free) g_bridge_free(raw_status);
            }
        }
        std::string message = "releaseBuffer failed";
        if (!status_json.empty()) {
            message += ": ";
            message += status_json;
        }
        v8::Local<v8::Value> error = v8::Exception::Error(
            v8::String::NewFromUtf8(isolate, message.c_str()).ToLocalChecked());
        if (!status_json.empty() && error->IsObject()) {
            v8::Local<v8::Value> parsed_status;
            v8::Local<v8::String> json =
                v8::String::NewFromUtf8(isolate, status_json.c_str()).ToLocalChecked();
            if (v8::JSON::Parse(context, json).ToLocal(&parsed_status)) {
                error.As<v8::Object>()->Set(context,
                    v8::String::NewFromUtf8Literal(isolate, "omnivmBufferStatus"),
                    parsed_status).Check();
            }
        }
        isolate->ThrowException(error);
    }
}

// omnivm.bufferStatus(name) -> object
static void OmnivmBufferStatusCallback(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();
    v8::Local<v8::Context> context = isolate->GetCurrentContext();
    if (info.Length() < 1 || !info[0]->IsString()) {
        isolate->ThrowException(v8::Exception::TypeError(
            v8::String::NewFromUtf8Literal(isolate, "bufferStatus requires a string name")));
        return;
    }
    if (!g_buf_status) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "buffer status bridge not initialized")));
        return;
    }

    v8::String::Utf8Value name(isolate, info[0]);
    char* raw = nullptr;
    {
        v8::Unlocker unlocker(isolate);
        raw = g_buf_status(*name);
    }
    if (!raw) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "bufferStatus failed")));
        return;
    }
    v8::Local<v8::String> json = v8::String::NewFromUtf8(isolate, raw).ToLocalChecked();
    if (g_bridge_free) g_bridge_free(raw);
    v8::Local<v8::Value> parsed;
    if (!v8::JSON::Parse(context, json).ToLocal(&parsed)) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate, "bufferStatus returned invalid JSON")));
        return;
    }
    info.GetReturnValue().Set(parsed);
}

// Convert a JS value to omni_value_t
static omni_value_t js_to_omni_value(v8::Isolate* isolate,
                                      v8::Local<v8::Context> context,
                                      v8::Local<v8::Value> val) {
    omni_value_t out;
    memset(&out, 0, sizeof(out));

    if (val->IsNullOrUndefined()) {
        out.tag = OMNI_TAG_NULL;
    } else if (val->IsBoolean()) {
        out.tag = OMNI_TAG_BOOL;
        out.v.i = val->BooleanValue(isolate) ? 1 : 0;
    } else if (val->IsInt32()) {
        out.tag = OMNI_TAG_I64;
        out.v.i = val.As<v8::Int32>()->Value();
    } else if (val->IsNumber()) {
        double d = val.As<v8::Number>()->Value();
        // Use I64 for exact integers
        if (d == static_cast<double>(static_cast<int64_t>(d)) &&
            d >= -9007199254740992.0 && d <= 9007199254740992.0) {
            out.tag = OMNI_TAG_I64;
            out.v.i = static_cast<int64_t>(d);
        } else {
            out.tag = OMNI_TAG_F64;
            out.v.f = d;
        }
    } else if (val->IsString()) {
        v8::String::Utf8Value utf8(isolate, val);
        out.tag = OMNI_TAG_STRING;
        out.v.s.len = utf8.length();
        out.v.s.ptr = static_cast<char*>(malloc(out.v.s.len + 1));
        memcpy(out.v.s.ptr, *utf8, out.v.s.len);
        out.v.s.ptr[out.v.s.len] = '\0';
    } else if (val->IsArrayBuffer()) {
        auto ab = val.As<v8::ArrayBuffer>();
        out.tag = OMNI_TAG_BYTES;
        out.v.s.len = static_cast<int64_t>(ab->ByteLength());
        out.v.s.ptr = static_cast<char*>(malloc(out.v.s.len));
        memcpy(out.v.s.ptr, ab->GetBackingStore()->Data(), out.v.s.len);
    } else {
        const char* msg = "unsupported typed bridge argument; complex values must cross through the manifest proxy/Arrow boundary, not implicit stringification";
        out.tag = OMNI_TAG_ERROR;
        out.v.s.len = static_cast<int64_t>(strlen(msg));
        out.v.s.ptr = static_cast<char*>(malloc(out.v.s.len + 1));
        memcpy(out.v.s.ptr, msg, out.v.s.len + 1);
    }
    return out;
}

// Convert omni_value_t to a JS value
static v8::Local<v8::Value> omni_value_to_js(v8::Isolate* isolate,
                                               const omni_value_t& val) {
    switch (val.tag) {
    case OMNI_TAG_NULL:
        return v8::Null(isolate);
    case OMNI_TAG_BOOL:
        return v8::Boolean::New(isolate, val.v.i != 0);
    case OMNI_TAG_I64:
        return v8::Number::New(isolate, static_cast<double>(val.v.i));
    case OMNI_TAG_F64:
        return v8::Number::New(isolate, val.v.f);
    case OMNI_TAG_STRING:
        if (val.v.s.ptr && val.v.s.len > 0)
            return v8::String::NewFromUtf8(isolate, val.v.s.ptr,
                       v8::NewStringType::kNormal,
                       static_cast<int>(val.v.s.len)).ToLocalChecked();
        return v8::String::Empty(isolate);
    case OMNI_TAG_BYTES:
        if (val.v.s.ptr && val.v.s.len > 0) {
            auto backing = v8::ArrayBuffer::NewBackingStore(isolate, val.v.s.len);
            memcpy(backing->Data(), val.v.s.ptr, val.v.s.len);
            return v8::ArrayBuffer::New(isolate, std::move(backing));
        }
        return v8::ArrayBuffer::New(isolate, 0);
    case OMNI_TAG_ERROR:
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8(isolate,
                val.v.s.ptr ? val.v.s.ptr : "unknown error").ToLocalChecked()));
        return v8::Undefined(isolate);
    default:
        return v8::Null(isolate);
    }
}

static void free_omni_value(omni_value_t* val) {
    if (val->tag == OMNI_TAG_STRING || val->tag == OMNI_TAG_BYTES ||
        val->tag == OMNI_TAG_ERROR) {
        free(val->v.s.ptr);
        val->v.s.ptr = nullptr;
    }
}

// omnivm.callTyped(runtime, funcName, ...args) -> typed value
static void OmnivmCallTypedCallback(const v8::FunctionCallbackInfo<v8::Value>& info) {
    v8::Isolate* isolate = info.GetIsolate();
    v8::Local<v8::Context> context = isolate->GetCurrentContext();

    if (info.Length() < 2 || !info[0]->IsString() || !info[1]->IsString()) {
        isolate->ThrowException(v8::Exception::TypeError(
            v8::String::NewFromUtf8Literal(isolate,
                "omnivm.callTyped requires (runtime, funcName, ...args)")));
        return;
    }

    if (!g_call_typed) {
        isolate->ThrowException(v8::Exception::Error(
            v8::String::NewFromUtf8Literal(isolate,
                "omnivm typed bridge not initialized")));
        return;
    }

    v8::String::Utf8Value runtime(isolate, info[0]);
    v8::String::Utf8Value func_name(isolate, info[1]);

    int32_t nargs = info.Length() - 2;
    omni_value_t* c_args = nullptr;
    if (nargs > 0) {
        c_args = static_cast<omni_value_t*>(calloc(nargs, sizeof(omni_value_t)));
        for (int32_t i = 0; i < nargs; i++) {
            c_args[i] = js_to_omni_value(isolate, context, info[i + 2]);
            if (c_args[i].tag == OMNI_TAG_ERROR) {
                isolate->ThrowException(v8::Exception::TypeError(
                    v8::String::NewFromUtf8(isolate,
                        c_args[i].v.s.ptr ? c_args[i].v.s.ptr : "unsupported typed bridge argument").ToLocalChecked()));
                for (int32_t j = 0; j <= i; j++) {
                    free_omni_value(&c_args[j]);
                }
                free(c_args);
                return;
            }
        }
    }

    omni_value_t result;
    {
        v8::Unlocker unlocker(isolate);
        result = g_call_typed(*runtime, *func_name, c_args, nargs);
    }

    // Free args
    if (c_args) {
        for (int32_t i = 0; i < nargs; i++) {
            free_omni_value(&c_args[i]);
        }
        free(c_args);
    }

    v8::Local<v8::Value> js_result = omni_value_to_js(isolate, result);
    free_omni_value(&result);

    // If omni_value_to_js threw (TAG_ERROR), don't set return value
    if (!isolate->IsExecutionTerminating()) {
        info.GetReturnValue().Set(js_result);
    }
}

static void register_omnivm_proxy_helpers(v8::Isolate* isolate,
                                          v8::Local<v8::Context> context) {
    const char* source = R"JS(
(function() {
  if (typeof globalThis.__omnivm_actual_public_method !== 'function') {
    Object.defineProperty(globalThis, "__omnivm_actual_public_method", {
      configurable: true,
      value: function(value, name) {
        if (value == null) return null;
        var cursor = Object(value);
        var depth = 0;
        while (cursor != null && depth++ < 64) {
          var descriptor = null;
          try {
            descriptor = Object.getOwnPropertyDescriptor(cursor, name);
          } catch (_descriptorError) {
            return null;
          }
          if (descriptor) {
            return typeof descriptor.value === 'function' ? descriptor.value.bind(value) : null;
          }
          cursor = Object.getPrototypeOf(cursor);
        }
        return null;
      }
    });
  }
  if (typeof globalThis.omnivm !== 'undefined' && globalThis.omnivm && typeof globalThis.omnivm.proxyClose !== 'function') {
    Object.defineProperty(globalThis.omnivm, "proxyClose", {
      configurable: true,
      value: function(value) {
        var omnivmClose = null;
        try {
          omnivmClose = globalThis.__omnivm_actual_public_method(value, "__omnivm_close");
        } catch (_omnivmCloseLookupError) {}
        if (typeof omnivmClose === 'function') return omnivmClose.call(value);
        if (typeof Symbol !== 'undefined') {
          var symbolDispose = null;
          try {
            symbolDispose = Symbol.dispose ? globalThis.__omnivm_actual_public_method(value, Symbol.dispose) : null;
          } catch (_symbolDisposeLookupError) {}
          if (typeof symbolDispose === 'function') {
            var symbolDisposeResult = symbolDispose.call(value);
            return symbolDisposeResult === undefined ? true : symbolDisposeResult;
          }
          var symbolAsyncDispose = null;
          try {
            symbolAsyncDispose = Symbol.asyncDispose ? globalThis.__omnivm_actual_public_method(value, Symbol.asyncDispose) : null;
          } catch (_symbolAsyncDisposeLookupError) {}
          if (typeof symbolAsyncDispose === 'function') {
            var symbolAsyncDisposeResult = symbolAsyncDispose.call(value);
            return symbolAsyncDisposeResult === undefined ? true : symbolAsyncDisposeResult;
          }
        }
        var close = globalThis.__omnivm_actual_public_method(value, "close");
        if (close) {
          var result = close.call(value);
          return result === undefined ? true : result;
        }
        var dispose = globalThis.__omnivm_actual_public_method(value, "dispose");
        if (dispose) {
          var disposeResult = dispose.call(value);
          return disposeResult === undefined ? true : disposeResult;
        }
        var destroy = globalThis.__omnivm_actual_public_method(value, "destroy");
        if (destroy) {
          var destroyResult = destroy.call(value);
          return destroyResult === undefined ? true : destroyResult;
        }
        return false;
      }
    });
  }
  if (typeof globalThis.omnivm !== 'undefined' && globalThis.omnivm && typeof globalThis.omnivm.omnivmClose !== 'function') {
    Object.defineProperty(globalThis.omnivm, "omnivmClose", {
      configurable: true,
      value: function(value) { return globalThis.omnivm.proxyClose(value); }
    });
  }
  if (typeof globalThis.omnivm !== 'undefined' && globalThis.omnivm && typeof globalThis.omnivm.cleanupErrors !== 'function') {
    Object.defineProperty(globalThis.omnivm, "cleanupErrors", {
      configurable: true,
      value: function(error) {
        var errors = error && error.omnivmCleanupErrors;
        return Array.isArray(errors) ? errors.slice() : [];
      }
    });
  }
  if (typeof globalThis.omnivm !== 'undefined' && globalThis.omnivm) {
    globalThis.__omnivm_clone_json = globalThis.__omnivm_clone_json || function(value) {
      if (value == null) return value;
      return JSON.parse(JSON.stringify(value));
    };
    globalThis.__omnivm_owner_dispatch_contract = globalThis.__omnivm_owner_dispatch_contract || function() {
      return globalThis.__omnivm_clone_json({
        mode: "diagnostic_only",
        owner_dispatch_supported: false,
        foreign_thread_behavior: "reject_runtime_calls",
        reason: "owner dispatch is unsupported in this mode, so OmniVM will not route calls onto foreign owner loops",
        owner_dispatch_targets: {
          python_asyncio: {
            supported: false,
            owner_kind: "python_asyncio_loop",
            required_capability: "run callback on owning asyncio loop",
            current_behavior: "Python async stream pulls and close have narrow pump-owned paths; general callbacks are not migrated back to the owner loop",
            diagnostic: "Python async streams have narrow pump-owned pull/close paths, but general callbacks are not migrated back to the owner loop",
            narrow_capabilities: ["python_async_stream_pull", "python_async_stream_close"]
          },
          javascript_event_loop: {
            supported: false,
            owner_kind: "javascript_event_loop",
            required_capability: "run callback on the owning JavaScript event loop",
            current_behavior: "JavaScript promises and timers are pumped at OmniVM call boundaries; foreign owner-loop callback dispatch is not available",
            diagnostic: "OmniVM does not currently route arbitrary callbacks back onto a JavaScript event loop owner"
          },
          java_executor: {
            supported: false,
            owner_kind: "java_executor",
            required_capability: "run callback on the owning Java Executor",
            current_behavior: "Java futures and reactive handles expose cancellation/status, but arbitrary callbacks are not migrated to a captured Executor",
            diagnostic: "OmniVM does not currently route arbitrary callbacks back onto a Java Executor owner"
          },
          ruby_fiber_thread: {
            supported: false,
            owner_kind: "ruby_fiber_thread",
            required_capability: "run callback on the owning Ruby Fiber or native Thread",
            current_behavior: "Ruby runs on the single VM thread with native Ruby thread scheduling disabled",
            diagnostic: "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported"
          }
        }
      });
    };
    globalThis.__omnivm_ruby_threading_contract = globalThis.__omnivm_ruby_threading_contract || function() {
      return globalThis.__omnivm_clone_json({
        mode: "single_vm_thread",
        native_threads_supported: false,
        ruby_vm_thread: "single_vm_thread",
        thread_new_behavior: "unsupported_diagnostic",
        diagnostic: "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported",
        app_server_boundary: "Use Fiber/Async or single-thread Rack servers in process; run native-threaded Ruby app servers such as Puma out of process."
      });
    };
    globalThis.__omnivm_owner_dispatch_target_name = globalThis.__omnivm_owner_dispatch_target_name || function(target) {
      var raw = String(target == null ? "" : target);
      var normalized = raw.trim().toLowerCase().replace(/[-\s]+/g, "_");
      var aliases = {
        asyncio: "python_asyncio",
        python: "python_asyncio",
        python_loop: "python_asyncio",
        py: "python_asyncio",
        js: "javascript_event_loop",
        javascript: "javascript_event_loop",
        javascript_loop: "javascript_event_loop",
        node: "javascript_event_loop",
        java: "java_executor",
        jvm: "java_executor",
        executor: "java_executor",
        ruby: "ruby_fiber_thread",
        ruby_fiber: "ruby_fiber_thread",
        ruby_thread: "ruby_fiber_thread"
      };
      return aliases[normalized] || normalized;
    };
    if (typeof globalThis.omnivm.ownerDispatchStatus !== 'function') {
      Object.defineProperty(globalThis.omnivm, "ownerDispatchStatus", {
        configurable: true,
        value: function() { return globalThis.__omnivm_owner_dispatch_contract(); }
      });
    }
    if (typeof globalThis.omnivm.rubyThreadingStatus !== 'function') {
      Object.defineProperty(globalThis.omnivm, "rubyThreadingStatus", {
        configurable: true,
        value: function() { return globalThis.__omnivm_ruby_threading_contract(); }
      });
    }
    if (typeof globalThis.omnivm.ownerDispatchTargetStatus !== 'function') {
      Object.defineProperty(globalThis.omnivm, "ownerDispatchTargetStatus", {
        configurable: true,
        value: function(target) {
          var requested = String(target == null ? "" : target);
          var name = globalThis.__omnivm_owner_dispatch_target_name(requested);
          var status = globalThis.omnivm.ownerDispatchStatus();
          var info = status.owner_dispatch_targets[name];
          if (!info) {
            var unknownTarget = {
              target: name,
              requested_target: requested,
              known_targets: Object.keys(status.owner_dispatch_targets || {}).sort(),
              owner_dispatch_targets: status.owner_dispatch_targets || {}
            };
            throw globalThis.__omnivm_owner_dispatch_error("unknown owner dispatch target: " + requested, "owner_dispatch_target", {
              target: unknownTarget.target,
              requested_target: unknownTarget.requested_target,
              known_targets: unknownTarget.known_targets,
              owner_dispatch_targets: unknownTarget.owner_dispatch_targets,
              owner_dispatch_target: unknownTarget
            });
          }
          info.requested_target = requested;
          info.target = name;
          return info;
        }
      });
    }
    globalThis.__omnivm_owner_dispatch_error = globalThis.__omnivm_owner_dispatch_error || function(message, boundaryPath, details) {
      var err = new Error(message);
      var stackFrames;
      var causeChain = [];
      var detailsSnapshot = globalThis.__omnivm_clone_json(details);
      var detailsJson = JSON.stringify(detailsSnapshot);
      err.name = "OmniVMRuntimeError";
      err.runtime = "javascript";
      err.origin_runtime = "javascript";
      err.type = "RuntimeError";
      err.boundary_path = boundaryPath;
      err.boundaryPath = boundaryPath;
      err.original_error_handle = null;
      err.originalErrorHandle = null;
      err.traceback = err.stack || "";
      stackFrames = String(err.traceback).split("\n").filter(function(frame) { return frame.length > 0; });
      err.stack_frames = stackFrames.slice();
      err.stackFrames = stackFrames.slice();
      err.cause_chain = causeChain.slice();
      err.causeChain = causeChain.slice();
      err.details = globalThis.__omnivm_clone_json(detailsSnapshot);
      err.details_json = detailsJson;
      err.detailsJson = detailsJson;
      err.toJSON = function() {
        return {
          runtime: err.runtime,
          origin_runtime: err.origin_runtime,
          type: err.type,
          message: err.message,
          traceback: err.traceback,
          stack_frames: stackFrames.slice(),
          cause_chain: globalThis.__omnivm_clone_json(causeChain),
          boundary_path: err.boundary_path,
          original_error_handle: err.original_error_handle,
          details: globalThis.__omnivm_clone_json(detailsSnapshot),
          details_json: detailsJson
        };
      };
      return err;
    };
    if (typeof globalThis.omnivm.assertOwnerDispatchSupported !== 'function') {
      Object.defineProperty(globalThis.omnivm, "assertOwnerDispatchSupported", {
        configurable: true,
        value: function(label) {
          var info = globalThis.omnivm.ownerDispatchStatus();
          if (info.owner_dispatch_supported === true) return true;
          var prefix = label == null || String(label) === "" ? "" : String(label) + ": ";
          throw globalThis.__omnivm_owner_dispatch_error(prefix + "owner dispatch unsupported: " + info.reason, "owner_dispatch", {owner_dispatch: info});
        }
      });
    }
    if (typeof globalThis.omnivm.assertRubyNativeThreadsSupported !== 'function') {
      Object.defineProperty(globalThis.omnivm, "assertRubyNativeThreadsSupported", {
        configurable: true,
        value: function(label) {
          var info = globalThis.omnivm.rubyThreadingStatus();
          if (info.native_threads_supported === true) return true;
          var prefix = label == null || String(label) === "" ? "" : String(label) + ": ";
          throw globalThis.__omnivm_owner_dispatch_error(prefix + "native Ruby threads unsupported: mode=" + info.mode + ": " + info.diagnostic, "ruby_threading", {ruby_threading: info});
        }
      });
    }
    if (typeof globalThis.omnivm.assertOwnerDispatchTargetSupported !== 'function') {
      Object.defineProperty(globalThis.omnivm, "assertOwnerDispatchTargetSupported", {
        configurable: true,
        value: function(target, label) {
          var info = globalThis.omnivm.ownerDispatchTargetStatus(target);
          if (info.supported === true) return true;
          var prefix = label == null || String(label) === "" ? "" : String(label) + ": ";
          throw globalThis.__omnivm_owner_dispatch_error(prefix + "owner dispatch target unsupported: " + info.target + ": " + info.diagnostic, "owner_dispatch_target", {target: info.target, requested_target: info.requested_target, owner_dispatch_target: info});
        }
      });
    }
  }
  if (typeof globalThis.omnivm !== 'undefined' && globalThis.omnivm) {
    globalThis.__omnivm_buffer_owner_unset = globalThis.__omnivm_buffer_owner_unset || {};
    globalThis.__omnivm_buffer_status_released = globalThis.__omnivm_buffer_status_released || function(status) {
      return !!(status && typeof status === 'object' && (status.released === true || status.state === "released" || status.state === "released_detached"));
    };
    globalThis.__omnivm_release_error_released_buffer = globalThis.__omnivm_release_error_released_buffer || function(error) {
      var details = error && error.details;
      if (details == null && error && (typeof error.details_json === 'string' || typeof error.detailsJson === 'string')) {
        try {
          details = JSON.parse(error.details_json || error.detailsJson);
        } catch (_parseError) {}
      }
      return globalThis.__omnivm_buffer_status_released(details) ||
        !!(details && typeof details === 'object' && globalThis.__omnivm_buffer_status_released(details.buffer));
    };
    if (typeof globalThis.__omnivm_BufferOwner !== 'function') {
      Object.defineProperty(globalThis, "__omnivm_BufferOwner", {
        configurable: true,
        value: function(name, data, dtype) {
          this.name = String(name);
          this.__omnivm_data = data;
          this.__omnivm_dtype = dtype == null ? 0 : dtype;
          this.released = false;
          this.__omnivm_entered = false;
        }
      });
      globalThis.__omnivm_BufferOwner.prototype.enter = function() {
        if (this.released === true) throw new Error("omnivm.bufferOwner " + JSON.stringify(this.name) + " cannot be re-entered after release");
        if (this.__omnivm_entered) throw new Error("omnivm.bufferOwner " + JSON.stringify(this.name) + " is already active");
        if (this.__omnivm_data !== globalThis.__omnivm_buffer_owner_unset) {
          globalThis.omnivm.setBuffer(this.name, this.__omnivm_data, this.__omnivm_dtype);
        }
        this.__omnivm_entered = true;
        return this;
      };
      globalThis.__omnivm_BufferOwner.prototype.release = function() {
        if (this.released === true) return false;
        try {
          globalThis.omnivm.releaseBuffer(this.name);
        } catch (err) {
          if (globalThis.__omnivm_release_error_released_buffer(err)) {
            this.released = true;
            this.__omnivm_entered = false;
          }
          throw err;
        }
        this.released = true;
        this.__omnivm_entered = false;
        return true;
      };
      globalThis.__omnivm_BufferOwner.prototype.close = function() {
        return this.release();
      };
      globalThis.__omnivm_BufferOwner.prototype.status = function() {
        if (typeof globalThis.omnivm.bufferStatus !== 'function') {
          throw new Error("buffer status bridge not initialized");
        }
        return globalThis.omnivm.bufferStatus(this.name);
      };
      if (typeof Symbol !== 'undefined' && Symbol.dispose) {
        globalThis.__omnivm_BufferOwner.prototype[Symbol.dispose] = function() {
          return this.release();
        };
      }
      if (typeof Symbol !== 'undefined' && Symbol.asyncDispose) {
        globalThis.__omnivm_BufferOwner.prototype[Symbol.asyncDispose] = function() {
          return this.release();
        };
      }
    }
    if (typeof globalThis.omnivm.bufferOwner !== 'function') {
      Object.defineProperty(globalThis.omnivm, "bufferOwner", {
        configurable: true,
        value: function(name, data, dtype, body) {
          var unset = globalThis.__omnivm_buffer_owner_unset;
          var ownerData = unset;
          var ownerDtype = 0;
          var callback = null;
          if (arguments.length >= 2 && typeof data !== 'function') {
            ownerData = data;
          }
          if (typeof data === 'function') {
            callback = data;
          } else if (typeof dtype === 'function') {
            callback = dtype;
          } else if (typeof body === 'function') {
            callback = body;
          }
          if (typeof dtype !== 'function' && dtype != null) ownerDtype = dtype;
          var owner = new globalThis.__omnivm_BufferOwner(name, ownerData, ownerDtype).enter();
          if (callback === null) return owner;
          var finishSuccess = function(value) {
            owner.release();
            return value;
          };
          var finishError = function(bodyError) {
            try {
              owner.release();
            } catch (cleanupError) {
              try {
                bodyError.omnivmCleanupErrors = (bodyError.omnivmCleanupErrors || []).concat([cleanupError]);
              } catch (_cleanupRecordError) {}
            }
            throw bodyError;
          };
          try {
            var result = callback(owner);
            if (typeof Promise !== 'undefined' && result != null && typeof result.then === 'function') {
              return Promise.resolve(result).then(finishSuccess, finishError);
            }
            return finishSuccess(result);
          } catch (bodyError) {
            return finishError(bodyError);
          }
        }
      });
    }
  }
})();
)JS";
    v8::Local<v8::String> src =
        v8::String::NewFromUtf8(isolate, source).ToLocalChecked();
    v8::Local<v8::Script> script;
    if (!v8::Script::Compile(context, src).ToLocal(&script)) {
        return;
    }
    v8::Local<v8::Value> ignored;
    (void)script->Run(context).ToLocal(&ignored);
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

    // Typed call bridge
    omnivm_obj->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "callTyped"),
        v8::FunctionTemplate::New(isolate, OmnivmCallTypedCallback)
            ->GetFunction(context).ToLocalChecked()).Check();

    // Buffer bridge methods
    omnivm_obj->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "getBuffer"),
        v8::FunctionTemplate::New(isolate, OmnivmGetBufferCallback)
            ->GetFunction(context).ToLocalChecked()).Check();
    omnivm_obj->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "setBuffer"),
        v8::FunctionTemplate::New(isolate, OmnivmSetBufferCallback)
            ->GetFunction(context).ToLocalChecked()).Check();
    omnivm_obj->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "releaseBuffer"),
        v8::FunctionTemplate::New(isolate, OmnivmReleaseBufferCallback)
            ->GetFunction(context).ToLocalChecked()).Check();
    omnivm_obj->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "bufferStatus"),
        v8::FunctionTemplate::New(isolate, OmnivmBufferStatusCallback)
            ->GetFunction(context).ToLocalChecked()).Check();

    global->Set(context,
        v8::String::NewFromUtf8Literal(isolate, "omnivm"),
        omnivm_obj).Check();
    register_omnivm_proxy_helpers(isolate, context);
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
        result.error = omnivm_v8_format_exception(ctx_w->isolate, context, try_catch, "compilation error");

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
        result.error = omnivm_v8_format_exception(ctx_w->isolate, context, try_catch, "runtime error");

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

    // Drain microtasks so Promises triggered by this call resolve immediately.
    // Safe even on the Golden Thread (idempotent — already-drained queues are a no-op).
    ctx_w->isolate->PerformMicrotaskCheckpoint();

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
        result.error = omnivm_v8_format_exception(ctx_w->isolate, context, try_catch, "compilation error");
        return result;
    }

    v8::MaybeLocal<v8::Value> maybe_result =
        maybe_script.ToLocalChecked()->Run(context);

    if (try_catch.HasCaught()) {
        result.error = omnivm_v8_format_exception(ctx_w->isolate, context, try_catch, "runtime error");
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

    // Drain microtasks so Promises triggered by this eval resolve immediately
    ctx_w->isolate->PerformMicrotaskCheckpoint();

    return result;
}

int omnivm_v8_export_buffer(omnivm_v8_context* ctx_w,
                             const char* code,
                             omnivm_v8_exported_buffer_t* out) {
    if (!ctx_w || !ctx_w->isolate || !out) {
        return -1;
    }
    memset(out, 0, sizeof(*out));

    v8::Locker locker(ctx_w->isolate);
    v8::Isolate::Scope isolate_scope(ctx_w->isolate);
    v8::HandleScope handle_scope(ctx_w->isolate);
    v8::Local<v8::Context> context =
        v8::Local<v8::Context>::New(ctx_w->isolate, ctx_w->context);
    v8::Context::Scope context_scope(context);
    node::CallbackScope callback_scope(ctx_w->isolate,
        v8::Object::New(ctx_w->isolate), {0, 0});

    ctx_w->isolate->CancelTerminateExecution();
    register_omnivm_bridge(ctx_w->isolate, context);

    v8::TryCatch try_catch(ctx_w->isolate);
    v8::Local<v8::String> source =
        v8::String::NewFromUtf8(ctx_w->isolate, code).ToLocalChecked();
    v8::MaybeLocal<v8::Script> maybe_script =
        v8::Script::Compile(context, source);
    if (maybe_script.IsEmpty()) {
        return 1;
    }
    v8::MaybeLocal<v8::Value> maybe_result =
        maybe_script.ToLocalChecked()->Run(context);
    if (maybe_result.IsEmpty() || try_catch.HasCaught()) {
        if (try_catch.HasTerminated()) {
            ctx_w->isolate->CancelTerminateExecution();
            return -1;
        }
        return 1;
    }

    v8::Local<v8::Value> value = maybe_result.ToLocalChecked();
    int32_t dtype = 0;
    const char* arrow_format = nullptr;
    size_t elem_size = 0;
    if (!OmniTypedArrayMetadata(value, &dtype, &arrow_format, &elem_size)) {
        return 1;
    }

    std::shared_ptr<v8::BackingStore> backing;
    size_t byte_offset = 0;
    size_t byte_len = 0;
    if (value->IsArrayBuffer()) {
        auto ab = value.As<v8::ArrayBuffer>();
        backing = ab->GetBackingStore();
        byte_len = ab->ByteLength();
    } else if (value->IsArrayBufferView()) {
        auto view = value.As<v8::ArrayBufferView>();
        backing = view->Buffer()->GetBackingStore();
        byte_offset = view->ByteOffset();
        byte_len = view->ByteLength();
    } else {
        return 1;
    }
    if (elem_size == 0 || byte_len % elem_size != 0 || byte_offset > backing->ByteLength() ||
        byte_len > backing->ByteLength() - byte_offset) {
        return 1;
    }

    auto* handle = new OmniExportedBufferHandle();
    handle->backing = backing;
    out->data = static_cast<char*>(backing->Data()) + byte_offset;
    out->len = static_cast<int64_t>(byte_len);
    out->dtype = dtype;
    out->elements = static_cast<int64_t>(byte_len / elem_size);
    out->arrow_format = arrow_format;
    out->read_only = 0;
    out->handle = handle;
    ctx_w->isolate->PerformMicrotaskCheckpoint();
    return 0;
}

void omnivm_v8_release_exported_buffer(void* handle) {
    delete static_cast<OmniExportedBufferHandle*>(handle);
}

omni_value_t omnivm_v8_eval_typed(omnivm_v8_context* ctx_w, const char* code) {
    omni_value_t null_val;
    memset(&null_val, 0, sizeof(null_val));

    if (!ctx_w || !ctx_w->isolate) {
        omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = OMNI_TAG_ERROR;
        err.v.s.ptr = strdup("JS context not initialized");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
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
    register_omnivm_bridge(ctx_w->isolate, context);

    v8::TryCatch try_catch(ctx_w->isolate);
    v8::Local<v8::String> source =
        v8::String::NewFromUtf8(ctx_w->isolate, code).ToLocalChecked();

    v8::MaybeLocal<v8::Script> maybe_script =
        v8::Script::Compile(context, source);

    if (maybe_script.IsEmpty()) {
        omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = OMNI_TAG_ERROR;
        err.v.s.ptr = omnivm_v8_format_exception(ctx_w->isolate, context, try_catch, "compilation error");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }

    v8::MaybeLocal<v8::Value> maybe_result =
        maybe_script.ToLocalChecked()->Run(context);

    if (try_catch.HasCaught()) {
        omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = OMNI_TAG_ERROR;
        err.v.s.ptr = omnivm_v8_format_exception(ctx_w->isolate, context, try_catch, "runtime error");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }

    if (maybe_result.IsEmpty()) {
        return null_val;
    }

    v8::Local<v8::Value> val = maybe_result.ToLocalChecked();
    omni_value_t typed_result = js_to_omni_value(ctx_w->isolate, context, val);

    ctx_w->isolate->PerformMicrotaskCheckpoint();
    return typed_result;
}

void omnivm_v8_set_bridge_callback(omnivm_bridge_call_fn call_fn,
                                    omnivm_bridge_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

void omnivm_v8_set_buf_callbacks(omni_buf_get_fn get_fn,
                                  omni_buf_set_fn set_fn,
                                  omni_buf_release_fn release_fn,
                                  omni_buf_free_fn free_fn,
                                  omni_buf_status_fn status_fn) {
    g_buf_get = get_fn;
    g_buf_set = set_fn;
    g_buf_release = release_fn;
    g_buf_free = free_fn;
    g_buf_status = status_fn;
}

void omnivm_v8_set_typed_callback(omni_call_typed_fn fn) {
    g_call_typed = fn;
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
