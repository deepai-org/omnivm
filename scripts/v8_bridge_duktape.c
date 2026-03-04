#include "duktape.h"
#include "v8_bridge.h"
#include <stdlib.h>
#include <string.h>

struct omnivm_v8_isolate {
    int dummy;
};

struct omnivm_v8_context {
    duk_context* ctx;
};

static int initialized = 0;

/* Bridge callback pointers — set via omnivm_v8_set_bridge_callback() */
static omnivm_bridge_call_fn g_bridge_call = NULL;
static omnivm_bridge_free_fn g_bridge_free = NULL;

int omnivm_v8_init(void) {
    initialized = 1;
    return 0;
}

omnivm_v8_isolate* omnivm_v8_isolate_new(void) {
    omnivm_v8_isolate* iso = (omnivm_v8_isolate*)calloc(1, sizeof(omnivm_v8_isolate));
    return iso;
}

omnivm_v8_context* omnivm_v8_context_new(omnivm_v8_isolate* isolate) {
    omnivm_v8_context* ctx_w = (omnivm_v8_context*)calloc(1, sizeof(omnivm_v8_context));
    ctx_w->ctx = duk_create_heap_default();
    if (!ctx_w->ctx) {
        free(ctx_w);
        return NULL;
    }
    return ctx_w;
}

/* Custom print function for console.log */
static duk_ret_t native_print(duk_context *ctx) {
    duk_push_string(ctx, " ");
    duk_insert(ctx, 0);
    duk_join(ctx, duk_get_top(ctx) - 1);
    /* Append to __omnivm_output */
    duk_get_global_string(ctx, "__omnivm_output");
    if (duk_is_array(ctx, -1)) {
        duk_size_t len = duk_get_length(ctx, -1);
        duk_dup(ctx, -2);
        duk_put_prop_index(ctx, -2, (duk_uarridx_t)len);
    }
    duk_pop(ctx);
    return 0;
}

/* omnivm.call(runtime, code) — calls back into Go via function pointer */
static duk_ret_t native_omnivm_call(duk_context *ctx) {
    const char* runtime = duk_require_string(ctx, 0);
    const char* code = duk_require_string(ctx, 1);

    if (!g_bridge_call) {
        duk_error(ctx, DUK_ERR_ERROR, "omnivm bridge not initialized");
        return 0;
    }

    char* result = g_bridge_call(runtime, code);
    if (!result) {
        duk_error(ctx, DUK_ERR_ERROR, "omnivm.call returned NULL");
        return 0;
    }

    /* Check for error prefix */
    if (strncmp(result, "ERR:", 4) == 0) {
        char* err_msg = strdup(result + 4);
        if (g_bridge_free) g_bridge_free(result);
        duk_error(ctx, DUK_ERR_ERROR, "%s", err_msg);
        free(err_msg);
        return 0;
    }

    duk_push_string(ctx, result);
    if (g_bridge_free) g_bridge_free(result);
    return 1; /* return the string */
}

omnivm_v8_result omnivm_v8_execute(omnivm_v8_context* ctx_w, const char* code) {
    omnivm_v8_result result = {NULL, NULL};
    duk_context* ctx = ctx_w->ctx;

    /* Set up output capture array */
    duk_push_array(ctx);
    duk_put_global_string(ctx, "__omnivm_output");

    /* Set up console.log */
    duk_push_object(ctx);
    duk_push_c_function(ctx, native_print, DUK_VARARGS);
    duk_put_prop_string(ctx, -2, "log");
    duk_push_c_function(ctx, native_print, DUK_VARARGS);
    duk_put_prop_string(ctx, -2, "error");
    duk_push_c_function(ctx, native_print, DUK_VARARGS);
    duk_put_prop_string(ctx, -2, "warn");
    duk_put_global_string(ctx, "console");

    /* Set up omnivm.call */
    duk_push_object(ctx);
    duk_push_c_function(ctx, native_omnivm_call, 2);
    duk_put_prop_string(ctx, -2, "call");
    duk_put_global_string(ctx, "omnivm");

    /* Execute user code */
    if (duk_peval_string(ctx, code) != 0) {
        const char* err = duk_safe_to_string(ctx, -1);
        result.error = strdup(err ? err : "unknown error");
        duk_pop(ctx);
        return result;
    }
    duk_pop(ctx);

    /* Retrieve captured output */
    duk_get_global_string(ctx, "__omnivm_output");
    if (duk_is_array(ctx, -1)) {
        duk_size_t len = duk_get_length(ctx, -1);
        if (len == 0) {
            result.value = strdup("");
        } else {
            size_t total = 0;
            char** parts = (char**)malloc(sizeof(char*) * len);
            duk_size_t i;
            for (i = 0; i < len; i++) {
                duk_get_prop_index(ctx, -1, (duk_uarridx_t)i);
                parts[i] = strdup(duk_safe_to_string(ctx, -1));
                total += strlen(parts[i]) + 1;
                duk_pop(ctx);
            }
            char* output = (char*)malloc(total + 1);
            output[0] = '\0';
            for (i = 0; i < len; i++) {
                strcat(output, parts[i]);
                strcat(output, "\n");
                free(parts[i]);
            }
            free(parts);
            result.value = output;
        }
    } else {
        result.value = strdup("");
    }
    duk_pop(ctx);

    return result;
}

/* Eval — returns the expression value directly, not stdout */
omnivm_v8_result omnivm_v8_eval(omnivm_v8_context* ctx_w, const char* code) {
    omnivm_v8_result result = {NULL, NULL};
    duk_context* ctx = ctx_w->ctx;

    /* Set up omnivm.call (needed for re-entrant calls during eval) */
    duk_push_object(ctx);
    duk_push_c_function(ctx, native_omnivm_call, 2);
    duk_put_prop_string(ctx, -2, "call");
    duk_put_global_string(ctx, "omnivm");

    /* Evaluate the expression */
    if (duk_peval_string(ctx, code) != 0) {
        const char* err = duk_safe_to_string(ctx, -1);
        result.error = strdup(err ? err : "unknown error");
        duk_pop(ctx);
        return result;
    }

    /* Convert result to string */
    if (duk_is_undefined(ctx, -1)) {
        result.value = strdup("undefined");
    } else {
        const char* val = duk_safe_to_string(ctx, -1);
        result.value = strdup(val ? val : "undefined");
    }
    duk_pop(ctx);

    return result;
}

void omnivm_v8_set_bridge_callback(omnivm_bridge_call_fn call_fn, omnivm_bridge_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

void omnivm_v8_pump_message_loop(omnivm_v8_isolate* isolate) {
    (void)isolate;
}

void omnivm_v8_context_free(omnivm_v8_context* ctx_w) {
    if (ctx_w) {
        if (ctx_w->ctx) {
            duk_destroy_heap(ctx_w->ctx);
            ctx_w->ctx = NULL;
        }
        free(ctx_w);
    }
}

void omnivm_v8_isolate_free(omnivm_v8_isolate* iso) {
    if (iso) free(iso);
}

void omnivm_v8_shutdown(void) {
    initialized = 0;
}

void omnivm_v8_free_string(char* s) {
    free(s);
}
