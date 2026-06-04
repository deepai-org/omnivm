// Package jvm embeds the Java Virtual Machine via JNI through cgo.
//
// Uses javax.tools.JavaCompiler for in-memory compilation of Java source,
// with a helper class (OmniVMRunner) that handles compilation, classloading,
// and output capture. Requires a full JDK (not just JRE).
package jvm

/*
#include <jni.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <stdio.h>
#include <pthread.h>

// Bridge callback pointer
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
} jvm_omni_buffer_t;
typedef int (*jvm_buf_get_fn)(const char* name, jvm_omni_buffer_t* out);
typedef int (*jvm_buf_set_fn)(const char* name, jvm_omni_buffer_t buf);
typedef void (*jvm_buf_release_fn)(const char* name);
typedef int (*jvm_buf_free_fn)(const char* name);
typedef char* (*jvm_buf_status_fn)(const char* name);
static jvm_buf_get_fn g_buf_get = NULL;
static jvm_buf_set_fn g_buf_set = NULL;
static jvm_buf_release_fn g_buf_release = NULL;
static jvm_buf_free_fn g_buf_free = NULL;
static jvm_buf_status_fn g_buf_status = NULL;

// Typed value bridge
typedef struct {
    int64_t tag;
    union {
        int64_t  i;
        double   f;
        struct { char* ptr; int64_t len; } s;
        uint64_t ref;
    } v;
} jvm_omni_value_t;

#define JVM_OMNI_TAG_NULL    0
#define JVM_OMNI_TAG_BOOL    1
#define JVM_OMNI_TAG_I64     2
#define JVM_OMNI_TAG_F64     3
#define JVM_OMNI_TAG_STRING  4
#define JVM_OMNI_TAG_BYTES   5
#define JVM_OMNI_TAG_REF     6
#define JVM_OMNI_TAG_ERROR   7

typedef jvm_omni_value_t (*jvm_call_typed_fn)(const char* runtime,
                                               const char* func_name,
                                               jvm_omni_value_t* args,
                                               int32_t nargs);
static jvm_call_typed_fn g_call_typed = NULL;

typedef struct {
    jobject object;
    void* critical;
    int array_kind;
} jvm_exported_buffer_handle_t;

typedef struct {
    void* data;
    int64_t len;
    int32_t dtype;
    int64_t elements;
    const char* arrow_format;
    int8_t read_only;
    void* handle;
} jvm_omnivm_exported_buffer_t;

#define JVM_EXPORT_DIRECT 0
#define JVM_EXPORT_BYTE_ARRAY 1
#define JVM_EXPORT_INT_ARRAY 2
#define JVM_EXPORT_LONG_ARRAY 3
#define JVM_EXPORT_FLOAT_ARRAY 4
#define JVM_EXPORT_DOUBLE_ARRAY 5
#define JVM_EXPORT_SHORT_ARRAY 6

static JavaVM* jvm_ptr = NULL;
static JNIEnv* env_ptr = NULL;  // Initial thread's env (used during init only)
static jclass runner_class = NULL;
static jmethodID execute_method = NULL;
static jmethodID eval_method_id = NULL;
static jmethodID eval_object_method_id = NULL;
static jmethodID exec_file_method_id = NULL;
static pthread_mutex_t active_java_thread_mutex = PTHREAD_MUTEX_INITIALIZER;
static jobject active_java_thread = NULL;

// Get a JNIEnv for the current thread. If the thread is not attached to the
// JVM, attach it as a daemon thread. Sets *did_attach=1 if newly attached
// (caller must call omnivm_jvm_maybe_detach).
static JNIEnv* omnivm_jvm_get_env(int* did_attach) {
    JNIEnv* env = NULL;
    *did_attach = 0;
    if (!jvm_ptr) return NULL;
    jint rc = (*jvm_ptr)->GetEnv(jvm_ptr, (void**)&env, JNI_VERSION_10);
    if (rc == JNI_OK) return env;
    rc = (*jvm_ptr)->AttachCurrentThreadAsDaemon(jvm_ptr, (void**)&env, NULL);
    if (rc != JNI_OK) return NULL;
    *did_attach = 1;
    return env;
}

static void omnivm_jvm_maybe_detach(int did_attach) {
    if (did_attach && jvm_ptr) (*jvm_ptr)->DetachCurrentThread(jvm_ptr);
}

static void omnivm_jvm_mark_active_thread(JNIEnv* env) {
    if (!env) return;
    jclass thread_class = (*env)->FindClass(env, "java/lang/Thread");
    if (!thread_class) {
        (*env)->ExceptionClear(env);
        return;
    }
    jmethodID current_thread = (*env)->GetStaticMethodID(
        env, thread_class, "currentThread", "()Ljava/lang/Thread;");
    if (!current_thread) {
        (*env)->ExceptionClear(env);
        (*env)->DeleteLocalRef(env, thread_class);
        return;
    }
    jobject thread = (*env)->CallStaticObjectMethod(env, thread_class, current_thread);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        (*env)->DeleteLocalRef(env, thread_class);
        return;
    }
    jobject global_thread = thread ? (*env)->NewGlobalRef(env, thread) : NULL;
    pthread_mutex_lock(&active_java_thread_mutex);
    if (active_java_thread) {
        (*env)->DeleteGlobalRef(env, active_java_thread);
    }
    active_java_thread = global_thread;
    pthread_mutex_unlock(&active_java_thread_mutex);
    if (thread) (*env)->DeleteLocalRef(env, thread);
    (*env)->DeleteLocalRef(env, thread_class);
}

static void omnivm_jvm_clear_active_thread(JNIEnv* env) {
    if (!env) return;
    pthread_mutex_lock(&active_java_thread_mutex);
    jobject thread = active_java_thread;
    active_java_thread = NULL;
    pthread_mutex_unlock(&active_java_thread_mutex);
    if (thread) {
        (*env)->DeleteGlobalRef(env, thread);
    }
}

static void omnivm_jvm_interrupt_active_thread(void) {
    int did_attach;
    JNIEnv* env = omnivm_jvm_get_env(&did_attach);
    if (!env) return;

    pthread_mutex_lock(&active_java_thread_mutex);
    jobject thread = active_java_thread;
    if (thread) {
        thread = (*env)->NewLocalRef(env, thread);
    }
    pthread_mutex_unlock(&active_java_thread_mutex);

    if (thread) {
        jclass thread_class = (*env)->GetObjectClass(env, thread);
        if (thread_class) {
            jmethodID interrupt = (*env)->GetMethodID(env, thread_class, "interrupt", "()V");
            if (interrupt) {
                (*env)->CallVoidMethod(env, thread, interrupt);
            }
            if ((*env)->ExceptionCheck(env)) {
                (*env)->ExceptionClear(env);
            }
            (*env)->DeleteLocalRef(env, thread_class);
        } else if ((*env)->ExceptionCheck(env)) {
            (*env)->ExceptionClear(env);
        }
        (*env)->DeleteLocalRef(env, thread);
    }

    omnivm_jvm_maybe_detach(did_attach);
}

static void* omnivm_jvm_interrupt_active_thread_ptr(void) {
    return (void*)omnivm_jvm_interrupt_active_thread;
}

// JNI native implementation of OmniVM.call(runtime, code)
static jstring JNICALL Java_omnivm_OmniVM_nativeCall(JNIEnv* env, jclass cls,
                                                       jstring j_runtime, jstring j_code) {
    if (!g_bridge_call) {
        jclass exc_class = (*env)->FindClass(env, "java/lang/RuntimeException");
        (*env)->ThrowNew(env, exc_class, "omnivm bridge not initialized");
        (*env)->DeleteLocalRef(env, exc_class);
        return NULL;
    }

    // Use PushLocalFrame/PopLocalFrame for JNI local ref management
    if ((*env)->PushLocalFrame(env, 16) < 0) {
        return NULL;
    }

    const char* runtime = (*env)->GetStringUTFChars(env, j_runtime, NULL);
    const char* code = (*env)->GetStringUTFChars(env, j_code, NULL);

    char* result = g_bridge_call(runtime, code);

    (*env)->ReleaseStringUTFChars(env, j_runtime, runtime);
    (*env)->ReleaseStringUTFChars(env, j_code, code);

    if (!result) {
        (*env)->PopLocalFrame(env, NULL);
        jclass exc_class = (*env)->FindClass(env, "java/lang/RuntimeException");
        (*env)->ThrowNew(env, exc_class, "OmniVM.call returned NULL");
        return NULL;
    }

    // Check for error prefix
    if (strncmp(result, "ERR:", 4) == 0) {
        jclass exc_class = (*env)->FindClass(env, "java/lang/RuntimeException");
        (*env)->ThrowNew(env, exc_class, result + 4);
        if (g_bridge_free) g_bridge_free(result);
        (*env)->PopLocalFrame(env, NULL);
        return NULL;
    }

    jstring j_result = (*env)->NewStringUTF(env, result);
    if (g_bridge_free) g_bridge_free(result);

    // PopLocalFrame keeps j_result alive by returning it
    return (jstring)(*env)->PopLocalFrame(env, j_result);
}

// JNI: OmniVM.nativeGetBuffer(name) -> byte[] or null
static jbyteArray JNICALL Java_omnivm_OmniVM_nativeGetBuffer(JNIEnv* env, jclass cls,
                                                               jstring j_name) {
    if (!g_buf_get) return NULL;
    const char* name = (*env)->GetStringUTFChars(env, j_name, NULL);
    jvm_omni_buffer_t buf;
    memset(&buf, 0, sizeof(buf));
    int rc = g_buf_get(name, &buf);
    if (rc != 0 || !buf.data || buf.len <= 0) {
        if (rc == 0 && g_buf_release) g_buf_release(name);
        (*env)->ReleaseStringUTFChars(env, j_name, name);
        return NULL;
    }

    jbyteArray arr = (*env)->NewByteArray(env, (jsize)buf.len);
    if (arr) {
        (*env)->SetByteArrayRegion(env, arr, 0, (jsize)buf.len, (jbyte*)buf.data);
    }
    if (g_buf_release) g_buf_release(name);
    (*env)->ReleaseStringUTFChars(env, j_name, name);
    return arr;
}

// JNI: OmniVM.nativeGetBufferDtype(name) -> int
static jint JNICALL Java_omnivm_OmniVM_nativeGetBufferDtype(JNIEnv* env, jclass cls,
                                                              jstring j_name) {
    if (!g_buf_get) return -1;
    const char* name = (*env)->GetStringUTFChars(env, j_name, NULL);
    jvm_omni_buffer_t buf;
    memset(&buf, 0, sizeof(buf));
    int rc = g_buf_get(name, &buf);
    if (rc != 0) {
        (*env)->ReleaseStringUTFChars(env, j_name, name);
        return -1;
    }
    jint dtype = (jint)buf.dtype;
    if (g_buf_release) g_buf_release(name);
    (*env)->ReleaseStringUTFChars(env, j_name, name);
    return dtype;
}

// JNI: OmniVM.nativeSetBuffer(name, data, dtype) -> void
static void JNICALL Java_omnivm_OmniVM_nativeSetBuffer(JNIEnv* env, jclass cls,
                                                         jstring j_name, jbyteArray j_data,
                                                         jint j_dtype) {
    if (!g_buf_set) {
        jclass exc = (*env)->FindClass(env, "java/lang/RuntimeException");
        (*env)->ThrowNew(env, exc, "buffer bridge not initialized");
        return;
    }
    const char* name = (*env)->GetStringUTFChars(env, j_name, NULL);
    jsize len = (*env)->GetArrayLength(env, j_data);
    jbyte* data = (*env)->GetByteArrayElements(env, j_data, NULL);

    jvm_omni_buffer_t buf;
    buf.data = (void*)data;
    buf.len = (int64_t)len;
    buf.dtype = (int32_t)j_dtype;
    buf.owned = 0;
    buf.read_only = 0;
    g_buf_set(name, buf);

    (*env)->ReleaseByteArrayElements(env, j_data, data, JNI_ABORT);
    (*env)->ReleaseStringUTFChars(env, j_name, name);
}

// JNI: OmniVM.nativeReleaseBuffer(name) -> void
static void JNICALL Java_omnivm_OmniVM_nativeReleaseBuffer(JNIEnv* env, jclass cls,
                                                              jstring j_name) {
    if (!g_buf_free) return;
    const char* name = (*env)->GetStringUTFChars(env, j_name, NULL);
    if (g_buf_free(name) != 0) {
        jclass exc = (*env)->FindClass(env, "java/lang/RuntimeException");
        if (exc) {
            char* raw = g_buf_status ? g_buf_status(name) : NULL;
            if (raw && raw[0] != '\0') {
                size_t msg_len = strlen("OmniVM.releaseBuffer failed: ") + strlen(raw) + 1;
                char* msg = (char*)malloc(msg_len);
                if (msg) {
                    snprintf(msg, msg_len, "OmniVM.releaseBuffer failed: %s", raw);
                    (*env)->ThrowNew(env, exc, msg);
                    free(msg);
                } else {
                    (*env)->ThrowNew(env, exc, "OmniVM.releaseBuffer failed");
                }
            } else {
                (*env)->ThrowNew(env, exc, "OmniVM.releaseBuffer failed");
            }
            if (raw && g_bridge_free) {
                g_bridge_free(raw);
            }
        }
    }
    (*env)->ReleaseStringUTFChars(env, j_name, name);
}

// JNI: OmniVM.nativeBufferStatus(name) -> JSON string
static jstring JNICALL Java_omnivm_OmniVM_nativeBufferStatus(JNIEnv* env, jclass cls,
                                                               jstring j_name) {
    if (!g_buf_status) {
        jclass exc = (*env)->FindClass(env, "java/lang/RuntimeException");
        if (exc) {
            (*env)->ThrowNew(env, exc, "OmniVM.bufferStatus bridge not initialized");
        }
        return NULL;
    }
    const char* name = (*env)->GetStringUTFChars(env, j_name, NULL);
    char* raw = g_buf_status(name);
    (*env)->ReleaseStringUTFChars(env, j_name, name);
    if (!raw) {
        jclass exc = (*env)->FindClass(env, "java/lang/RuntimeException");
        if (exc) {
            (*env)->ThrowNew(env, exc, "OmniVM.bufferStatus failed");
        }
        return NULL;
    }
    jstring out = (*env)->NewStringUTF(env, raw);
    if (g_bridge_free) {
        g_bridge_free(raw);
    }
    return out;
}

// Convert a Java Object to jvm_omni_value_t
static jvm_omni_value_t java_to_omni_value(JNIEnv* env, jobject obj) {
    jvm_omni_value_t val;
    memset(&val, 0, sizeof(val));

    if (!obj) {
        val.tag = JVM_OMNI_TAG_NULL;
        return val;
    }

    // Check types: Integer, Long, Double, Float, Boolean, String
    jclass int_class = (*env)->FindClass(env, "java/lang/Integer");
    jclass long_class = (*env)->FindClass(env, "java/lang/Long");
    jclass double_class = (*env)->FindClass(env, "java/lang/Double");
    jclass float_class = (*env)->FindClass(env, "java/lang/Float");
    jclass bool_class = (*env)->FindClass(env, "java/lang/Boolean");
    jclass str_class = (*env)->FindClass(env, "java/lang/String");

    if ((*env)->IsInstanceOf(env, obj, bool_class)) {
        jmethodID m = (*env)->GetMethodID(env, bool_class, "booleanValue", "()Z");
        val.tag = JVM_OMNI_TAG_BOOL;
        val.v.i = (*env)->CallBooleanMethod(env, obj, m) ? 1 : 0;
    } else if ((*env)->IsInstanceOf(env, obj, int_class)) {
        jmethodID m = (*env)->GetMethodID(env, int_class, "intValue", "()I");
        val.tag = JVM_OMNI_TAG_I64;
        val.v.i = (int64_t)(*env)->CallIntMethod(env, obj, m);
    } else if ((*env)->IsInstanceOf(env, obj, long_class)) {
        jmethodID m = (*env)->GetMethodID(env, long_class, "longValue", "()J");
        val.tag = JVM_OMNI_TAG_I64;
        val.v.i = (int64_t)(*env)->CallLongMethod(env, obj, m);
    } else if ((*env)->IsInstanceOf(env, obj, double_class)) {
        jmethodID m = (*env)->GetMethodID(env, double_class, "doubleValue", "()D");
        val.tag = JVM_OMNI_TAG_F64;
        val.v.f = (*env)->CallDoubleMethod(env, obj, m);
    } else if ((*env)->IsInstanceOf(env, obj, float_class)) {
        jmethodID m = (*env)->GetMethodID(env, float_class, "floatValue", "()F");
        val.tag = JVM_OMNI_TAG_F64;
        val.v.f = (double)(*env)->CallFloatMethod(env, obj, m);
    } else if ((*env)->IsInstanceOf(env, obj, str_class)) {
        const char* utf = (*env)->GetStringUTFChars(env, (jstring)obj, NULL);
        val.tag = JVM_OMNI_TAG_STRING;
        val.v.s.len = strlen(utf);
        val.v.s.ptr = strdup(utf);
        (*env)->ReleaseStringUTFChars(env, (jstring)obj, utf);
    } else {
        const char* msg = "unsupported typed bridge argument; complex values must cross through the manifest proxy/Arrow boundary, not implicit stringification";
        val.tag = JVM_OMNI_TAG_ERROR;
        val.v.s.len = strlen(msg);
        val.v.s.ptr = strdup(msg);
    }

    (*env)->DeleteLocalRef(env, int_class);
    (*env)->DeleteLocalRef(env, long_class);
    (*env)->DeleteLocalRef(env, double_class);
    (*env)->DeleteLocalRef(env, float_class);
    (*env)->DeleteLocalRef(env, bool_class);
    (*env)->DeleteLocalRef(env, str_class);
    return val;
}

// Convert jvm_omni_value_t to Java Object
static jobject omni_value_to_java(JNIEnv* env, jvm_omni_value_t val) {
    switch (val.tag) {
    case JVM_OMNI_TAG_NULL:
        return NULL;
    case JVM_OMNI_TAG_BOOL: {
        jclass cls = (*env)->FindClass(env, "java/lang/Boolean");
        jmethodID m = (*env)->GetStaticMethodID(env, cls, "valueOf", "(Z)Ljava/lang/Boolean;");
        jobject r = (*env)->CallStaticObjectMethod(env, cls, m, val.v.i ? JNI_TRUE : JNI_FALSE);
        (*env)->DeleteLocalRef(env, cls);
        return r;
    }
    case JVM_OMNI_TAG_I64: {
        jclass cls = (*env)->FindClass(env, "java/lang/Long");
        jmethodID m = (*env)->GetStaticMethodID(env, cls, "valueOf", "(J)Ljava/lang/Long;");
        jobject r = (*env)->CallStaticObjectMethod(env, cls, m, (jlong)val.v.i);
        (*env)->DeleteLocalRef(env, cls);
        return r;
    }
    case JVM_OMNI_TAG_F64: {
        jclass cls = (*env)->FindClass(env, "java/lang/Double");
        jmethodID m = (*env)->GetStaticMethodID(env, cls, "valueOf", "(D)Ljava/lang/Double;");
        jobject r = (*env)->CallStaticObjectMethod(env, cls, m, val.v.f);
        (*env)->DeleteLocalRef(env, cls);
        return r;
    }
    case JVM_OMNI_TAG_STRING:
        if (val.v.s.ptr)
            return (*env)->NewStringUTF(env, val.v.s.ptr);
        return (*env)->NewStringUTF(env, "");
    case JVM_OMNI_TAG_BYTES:
        if (val.v.s.ptr && val.v.s.len > 0) {
            jbyteArray arr = (*env)->NewByteArray(env, (jsize)val.v.s.len);
            (*env)->SetByteArrayRegion(env, arr, 0, (jsize)val.v.s.len, (jbyte*)val.v.s.ptr);
            return arr;
        }
        return (*env)->NewByteArray(env, 0);
    case JVM_OMNI_TAG_ERROR: {
        jclass exc = (*env)->FindClass(env, "java/lang/RuntimeException");
        (*env)->ThrowNew(env, exc, val.v.s.ptr ? val.v.s.ptr : "unknown error");
        return NULL;
    }
    default:
        return NULL;
    }
}

static void jvm_free_omni_value(jvm_omni_value_t* val) {
    if (val->tag == JVM_OMNI_TAG_STRING || val->tag == JVM_OMNI_TAG_BYTES ||
        val->tag == JVM_OMNI_TAG_ERROR) {
        free(val->v.s.ptr);
        val->v.s.ptr = NULL;
    }
}

// JNI: OmniVM.nativeCallTyped(runtime, funcName, args) -> Object
static jobject JNICALL Java_omnivm_OmniVM_nativeCallTyped(JNIEnv* env, jclass cls,
                                                            jstring j_runtime,
                                                            jstring j_func,
                                                            jobjectArray j_args) {
    if (!g_call_typed) {
        jclass exc = (*env)->FindClass(env, "java/lang/RuntimeException");
        (*env)->ThrowNew(env, exc, "typed bridge not initialized");
        return NULL;
    }

    const char* runtime = (*env)->GetStringUTFChars(env, j_runtime, NULL);
    const char* func_name = (*env)->GetStringUTFChars(env, j_func, NULL);

    int32_t nargs = j_args ? (int32_t)(*env)->GetArrayLength(env, j_args) : 0;
    jvm_omni_value_t* c_args = NULL;
    if (nargs > 0) {
        c_args = (jvm_omni_value_t*)calloc(nargs, sizeof(jvm_omni_value_t));
        for (int32_t i = 0; i < nargs; i++) {
            jobject item = (*env)->GetObjectArrayElement(env, j_args, i);
            c_args[i] = java_to_omni_value(env, item);
            if (item) (*env)->DeleteLocalRef(env, item);
            if (c_args[i].tag == JVM_OMNI_TAG_ERROR) {
                jclass exc = (*env)->FindClass(env, "java/lang/IllegalArgumentException");
                (*env)->ThrowNew(env, exc, c_args[i].v.s.ptr ? c_args[i].v.s.ptr : "unsupported typed bridge argument");
                (*env)->ReleaseStringUTFChars(env, j_runtime, runtime);
                (*env)->ReleaseStringUTFChars(env, j_func, func_name);
                for (int32_t j = 0; j <= i; j++) {
                    jvm_free_omni_value(&c_args[j]);
                }
                free(c_args);
                return NULL;
            }
        }
    }

    jvm_omni_value_t result = g_call_typed(runtime, func_name, c_args, nargs);

    (*env)->ReleaseStringUTFChars(env, j_runtime, runtime);
    (*env)->ReleaseStringUTFChars(env, j_func, func_name);

    if (c_args) {
        for (int32_t i = 0; i < nargs; i++) {
            jvm_free_omni_value(&c_args[i]);
        }
        free(c_args);
    }

    jobject j_result = omni_value_to_java(env, result);
    jvm_free_omni_value(&result);
    return j_result;
}

// Typed eval: evaluate Java code and return omni_value_t
static jvm_omni_value_t omnivm_jvm_eval_typed(const char* code) {
    jvm_omni_value_t null_val;
    memset(&null_val, 0, sizeof(null_val));

    int did_attach;
    JNIEnv* env = omnivm_jvm_get_env(&did_attach);
    if (!env || !runner_class || !eval_method_id) {
        jvm_omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = JVM_OMNI_TAG_ERROR;
        err.v.s.ptr = strdup("JVM not available");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }

    if ((*env)->PushLocalFrame(env, 16) < 0) {
        omnivm_jvm_maybe_detach(did_attach);
        jvm_omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = JVM_OMNI_TAG_ERROR;
        err.v.s.ptr = strdup("PushLocalFrame failed");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }

    jstring jcode = (*env)->NewStringUTF(env, code);
    if (!jcode) {
        (*env)->ExceptionClear(env);
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        jvm_omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = JVM_OMNI_TAG_ERROR;
        err.v.s.ptr = strdup("Failed to create Java string");
        err.v.s.len = strlen(err.v.s.ptr);
        return err;
    }

    omnivm_jvm_mark_active_thread(env);
    jstring result = (jstring)(*env)->CallStaticObjectMethod(
        env, runner_class, eval_method_id, jcode);
    omnivm_jvm_clear_active_thread(env);

    if ((*env)->ExceptionCheck(env)) {
        jthrowable exc = (*env)->ExceptionOccurred(env);
        (*env)->ExceptionClear(env);
        jclass throwable_class = (*env)->FindClass(env, "java/lang/Throwable");
        jmethodID to_string = (*env)->GetMethodID(env, throwable_class, "toString", "()Ljava/lang/String;");
        jstring msg = (jstring)(*env)->CallObjectMethod(env, exc, to_string);

        jvm_omni_value_t err;
        memset(&err, 0, sizeof(err));
        err.tag = JVM_OMNI_TAG_ERROR;
        if (msg) {
            const char* utf = (*env)->GetStringUTFChars(env, msg, NULL);
            err.v.s.ptr = strdup(utf);
            err.v.s.len = strlen(err.v.s.ptr);
            (*env)->ReleaseStringUTFChars(env, msg, utf);
        } else {
            err.v.s.ptr = strdup("Unknown JNI exception");
            err.v.s.len = strlen(err.v.s.ptr);
        }
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return err;
    }

    if (!result) {
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return null_val;
    }

    // OmniVMRunner.eval returns String — try to parse as typed
    const char* utf = (*env)->GetStringUTFChars(env, result, NULL);
    jvm_omni_value_t typed;
    memset(&typed, 0, sizeof(typed));

    // Try parsing as integer
    char* endptr;
    long long ll = strtoll(utf, &endptr, 10);
    if (*endptr == '\0' && endptr != utf) {
        typed.tag = JVM_OMNI_TAG_I64;
        typed.v.i = (int64_t)ll;
    } else {
        // Try parsing as double
        double d = strtod(utf, &endptr);
        if (*endptr == '\0' && endptr != utf) {
            typed.tag = JVM_OMNI_TAG_F64;
            typed.v.f = d;
        } else if (strcmp(utf, "true") == 0) {
            typed.tag = JVM_OMNI_TAG_BOOL;
            typed.v.i = 1;
        } else if (strcmp(utf, "false") == 0) {
            typed.tag = JVM_OMNI_TAG_BOOL;
            typed.v.i = 0;
        } else if (strcmp(utf, "null") == 0) {
            typed.tag = JVM_OMNI_TAG_NULL;
        } else {
            typed.tag = JVM_OMNI_TAG_STRING;
            typed.v.s.len = strlen(utf);
            typed.v.s.ptr = strdup(utf);
        }
    }

    (*env)->ReleaseStringUTFChars(env, result, utf);
    (*env)->PopLocalFrame(env, NULL);
    omnivm_jvm_maybe_detach(did_attach);
    return typed;
}

static int omnivm_jvm_export_array_buffer(JNIEnv* env,
                                          jobject obj,
                                          int array_kind,
                                          int32_t dtype,
                                          const char* arrow_format,
                                          int64_t item_size,
                                          jvm_omnivm_exported_buffer_t* out) {
    jarray arr = (jarray)obj;
    jsize elements = (*env)->GetArrayLength(env, arr);
    if (elements == 0) {
        jvm_exported_buffer_handle_t* handle =
            (jvm_exported_buffer_handle_t*)calloc(1, sizeof(jvm_exported_buffer_handle_t));
        if (!handle) return -1;
        handle->object = (*env)->NewGlobalRef(env, obj);
        if (!handle->object) {
            free(handle);
            return -1;
        }
        handle->array_kind = array_kind;
        out->data = NULL;
        out->len = 0;
        out->dtype = dtype;
        out->elements = 0;
        out->arrow_format = arrow_format;
        out->read_only = 0;
        out->handle = handle;
        return 0;
    }
    jboolean is_copy = JNI_FALSE;
    void* data = (*env)->GetPrimitiveArrayCritical(env, arr, &is_copy);
    if (!data) {
        if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);
        return -1;
    }
    if (is_copy == JNI_TRUE) {
        (*env)->ReleasePrimitiveArrayCritical(env, arr, data, JNI_ABORT);
        return 1;
    }

    jvm_exported_buffer_handle_t* handle =
        (jvm_exported_buffer_handle_t*)calloc(1, sizeof(jvm_exported_buffer_handle_t));
    if (!handle) {
        (*env)->ReleasePrimitiveArrayCritical(env, arr, data, JNI_ABORT);
        return -1;
    }
    handle->object = (*env)->NewGlobalRef(env, obj);
    if (!handle->object) {
        (*env)->ReleasePrimitiveArrayCritical(env, arr, data, JNI_ABORT);
        free(handle);
        return -1;
    }
    handle->critical = data;
    handle->array_kind = array_kind;

    out->data = data;
    out->len = (int64_t)elements * item_size;
    out->dtype = dtype;
    out->elements = (int64_t)elements;
    out->arrow_format = arrow_format;
    out->read_only = 0;
    out->handle = handle;
    return 0;
}

static int omnivm_jvm_export_nio_buffer(JNIEnv* env,
                                        jobject obj,
                                        jclass buffer_class,
                                        const char* array_signature,
                                        int array_kind,
                                        int32_t dtype,
                                        const char* arrow_format,
                                        int64_t item_size,
                                        jvm_omnivm_exported_buffer_t* out) {
    jmethodID position_method = (*env)->GetMethodID(env, buffer_class, "position", "()I");
    jmethodID remaining_method = (*env)->GetMethodID(env, buffer_class, "remaining", "()I");
    jmethodID capacity_method = (*env)->GetMethodID(env, buffer_class, "capacity", "()I");
    jmethodID read_only_method = (*env)->GetMethodID(env, buffer_class, "isReadOnly", "()Z");
    if (!position_method || !remaining_method || !capacity_method) {
        if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);
        return -1;
    }

    jint position = (*env)->CallIntMethod(env, obj, position_method);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        return -1;
    }
    jint remaining = (*env)->CallIntMethod(env, obj, remaining_method);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        return -1;
    }
    jint capacity = (*env)->CallIntMethod(env, obj, capacity_method);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        return -1;
    }
    jboolean read_only = JNI_FALSE;
    if (read_only_method) {
        read_only = (*env)->CallBooleanMethod(env, obj, read_only_method);
        if ((*env)->ExceptionCheck(env)) {
            (*env)->ExceptionClear(env);
            return -1;
        }
    } else if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
    }
    if (position < 0 || remaining < 0 || capacity < 0 || position + remaining > capacity) {
        return -1;
    }
    if (remaining == 0) {
        jvm_exported_buffer_handle_t* handle =
            (jvm_exported_buffer_handle_t*)calloc(1, sizeof(jvm_exported_buffer_handle_t));
        if (!handle) return -1;
        handle->object = (*env)->NewGlobalRef(env, obj);
        if (!handle->object) {
            free(handle);
            return -1;
        }
        handle->array_kind = JVM_EXPORT_DIRECT;
        out->data = NULL;
        out->len = 0;
        out->dtype = dtype;
        out->elements = 0;
        out->arrow_format = arrow_format;
        out->read_only = read_only ? 1 : 0;
        out->handle = handle;
        return 0;
    }

    void* direct_data = (*env)->GetDirectBufferAddress(env, obj);
    if (direct_data) {
        if (item_size > 1) {
            jmethodID order_method = (*env)->GetMethodID(env, buffer_class, "order", "()Ljava/nio/ByteOrder;");
            jclass byte_order_class = (*env)->FindClass(env, "java/nio/ByteOrder");
            jmethodID native_order_method = byte_order_class ? (*env)->GetStaticMethodID(env, byte_order_class, "nativeOrder", "()Ljava/nio/ByteOrder;") : NULL;
            if (!order_method || !byte_order_class || !native_order_method) {
                if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);
                if (byte_order_class) (*env)->DeleteLocalRef(env, byte_order_class);
                return 1;
            }
            jobject order_obj = (*env)->CallObjectMethod(env, obj, order_method);
            if ((*env)->ExceptionCheck(env)) {
                (*env)->ExceptionClear(env);
                (*env)->DeleteLocalRef(env, byte_order_class);
                return 1;
            }
            jobject native_order_obj = (*env)->CallStaticObjectMethod(env, byte_order_class, native_order_method);
            if ((*env)->ExceptionCheck(env)) {
                (*env)->ExceptionClear(env);
                if (order_obj) (*env)->DeleteLocalRef(env, order_obj);
                (*env)->DeleteLocalRef(env, byte_order_class);
                return 1;
            }
            jboolean native_order = order_obj && native_order_obj && (*env)->IsSameObject(env, order_obj, native_order_obj);
            if (order_obj) (*env)->DeleteLocalRef(env, order_obj);
            if (native_order_obj) (*env)->DeleteLocalRef(env, native_order_obj);
            (*env)->DeleteLocalRef(env, byte_order_class);
            if (!native_order) return 1;
        }
        jvm_exported_buffer_handle_t* handle =
            (jvm_exported_buffer_handle_t*)calloc(1, sizeof(jvm_exported_buffer_handle_t));
        if (!handle) return -1;
        handle->object = (*env)->NewGlobalRef(env, obj);
        if (!handle->object) {
            free(handle);
            return -1;
        }
        handle->array_kind = JVM_EXPORT_DIRECT;
        out->data = (void*)((char*)direct_data + ((int64_t)position * item_size));
        out->len = (int64_t)remaining * item_size;
        out->dtype = dtype;
        out->elements = (int64_t)remaining;
        out->arrow_format = arrow_format;
        out->read_only = read_only ? 1 : 0;
        out->handle = handle;
        return 0;
    }
    if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);

    jmethodID has_array_method = (*env)->GetMethodID(env, buffer_class, "hasArray", "()Z");
    jmethodID array_method = (*env)->GetMethodID(env, buffer_class, "array", array_signature);
    jmethodID array_offset_method = (*env)->GetMethodID(env, buffer_class, "arrayOffset", "()I");
    if (!has_array_method || !array_method || !array_offset_method) {
        if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);
        return 1;
    }
    jboolean has_array = (*env)->CallBooleanMethod(env, obj, has_array_method);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        return 1;
    }
    if (!has_array) return 1;

    jobject array_obj = (*env)->CallObjectMethod(env, obj, array_method);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        return -1;
    }
    if (!array_obj) return 1;

    jint array_offset = (*env)->CallIntMethod(env, obj, array_offset_method);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        (*env)->DeleteLocalRef(env, array_obj);
        return -1;
    }
    if (array_offset < 0) {
        (*env)->DeleteLocalRef(env, array_obj);
        return -1;
    }

    jarray arr = (jarray)array_obj;
    jsize array_len = (*env)->GetArrayLength(env, arr);
    if ((int64_t)array_offset + (int64_t)position + (int64_t)remaining > (int64_t)array_len) {
        (*env)->DeleteLocalRef(env, array_obj);
        return -1;
    }
    jboolean is_copy = JNI_FALSE;
    void* base = (*env)->GetPrimitiveArrayCritical(env, arr, &is_copy);
    if (!base) {
        if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);
        (*env)->DeleteLocalRef(env, array_obj);
        return -1;
    }
    if (is_copy == JNI_TRUE) {
        (*env)->ReleasePrimitiveArrayCritical(env, arr, base, JNI_ABORT);
        (*env)->DeleteLocalRef(env, array_obj);
        return 1;
    }

    jvm_exported_buffer_handle_t* handle =
        (jvm_exported_buffer_handle_t*)calloc(1, sizeof(jvm_exported_buffer_handle_t));
    if (!handle) {
        (*env)->ReleasePrimitiveArrayCritical(env, arr, base, JNI_ABORT);
        (*env)->DeleteLocalRef(env, array_obj);
        return -1;
    }
    handle->object = (*env)->NewGlobalRef(env, arr);
    if (!handle->object) {
        (*env)->ReleasePrimitiveArrayCritical(env, arr, base, JNI_ABORT);
        (*env)->DeleteLocalRef(env, array_obj);
        free(handle);
        return -1;
    }
    handle->critical = base;
    handle->array_kind = array_kind;

    int64_t start = ((int64_t)array_offset + (int64_t)position) * item_size;
    out->data = (void*)((char*)base + start);
    out->len = (int64_t)remaining * item_size;
    out->dtype = dtype;
    out->elements = (int64_t)remaining;
    out->arrow_format = arrow_format;
    out->read_only = read_only ? 1 : 0;
    out->handle = handle;
    (*env)->DeleteLocalRef(env, array_obj);
    return 0;
}

static int omnivm_jvm_export_buffer(const char* code, jvm_omnivm_exported_buffer_t* out) {
    if (!out) return -1;
    memset(out, 0, sizeof(*out));

    int did_attach;
    JNIEnv* env = omnivm_jvm_get_env(&did_attach);
    if (!env || !runner_class || !eval_object_method_id) {
        omnivm_jvm_maybe_detach(did_attach);
        return -1;
    }

    if ((*env)->PushLocalFrame(env, 32) < 0) {
        omnivm_jvm_maybe_detach(did_attach);
        return -1;
    }

    jstring jcode = (*env)->NewStringUTF(env, code);
    if (!jcode) {
        (*env)->ExceptionClear(env);
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return -1;
    }

    omnivm_jvm_mark_active_thread(env);
    jobject obj = (*env)->CallStaticObjectMethod(env, runner_class, eval_object_method_id, jcode);
    omnivm_jvm_clear_active_thread(env);
    if ((*env)->ExceptionCheck(env)) {
        (*env)->ExceptionClear(env);
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return -1;
    }
    if (!obj) {
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return 1;
    }

    jclass throwable_class = (*env)->FindClass(env, "java/lang/Throwable");
    if (throwable_class && (*env)->IsInstanceOf(env, obj, throwable_class)) {
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return -1;
    }
    if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);

    struct nio_export_case {
        const char* class_name;
        const char* array_signature;
        int array_kind;
        int32_t dtype;
        const char* arrow_format;
        int64_t item_size;
    };
    struct nio_export_case nio_cases[] = {
        {"java/nio/ByteBuffer", "()[B", JVM_EXPORT_BYTE_ARRAY, 0, "C", 1},
        {"java/nio/ShortBuffer", "()[S", JVM_EXPORT_SHORT_ARRAY, 6, "s", 2},
        {"java/nio/IntBuffer", "()[I", JVM_EXPORT_INT_ARRAY, 1, "i", 4},
        {"java/nio/LongBuffer", "()[J", JVM_EXPORT_LONG_ARRAY, 2, "l", 8},
        {"java/nio/FloatBuffer", "()[F", JVM_EXPORT_FLOAT_ARRAY, 3, "f", 4},
        {"java/nio/DoubleBuffer", "()[D", JVM_EXPORT_DOUBLE_ARRAY, 4, "g", 8},
    };
    for (size_t i = 0; i < sizeof(nio_cases)/sizeof(nio_cases[0]); i++) {
        jclass buffer_class = (*env)->FindClass(env, nio_cases[i].class_name);
        if (!buffer_class) {
            if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);
            continue;
        }
        if ((*env)->IsInstanceOf(env, obj, buffer_class)) {
            int rc = omnivm_jvm_export_nio_buffer(env, obj, buffer_class,
                                                  nio_cases[i].array_signature,
                                                  nio_cases[i].array_kind,
                                                  nio_cases[i].dtype,
                                                  nio_cases[i].arrow_format,
                                                  nio_cases[i].item_size, out);
            (*env)->DeleteLocalRef(env, buffer_class);
            (*env)->PopLocalFrame(env, NULL);
            omnivm_jvm_maybe_detach(did_attach);
            return rc;
        }
        (*env)->DeleteLocalRef(env, buffer_class);
    }
    if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);

    struct array_export_case {
        const char* class_name;
        int array_kind;
        int32_t dtype;
        const char* arrow_format;
        int64_t item_size;
    };
    struct array_export_case cases[] = {
		{"[B", JVM_EXPORT_BYTE_ARRAY, 10, "c", 1},
        {"[S", JVM_EXPORT_SHORT_ARRAY, 6, "s", 2},
        {"[I", JVM_EXPORT_INT_ARRAY, 1, "i", 4},
        {"[J", JVM_EXPORT_LONG_ARRAY, 2, "l", 8},
        {"[F", JVM_EXPORT_FLOAT_ARRAY, 3, "f", 4},
        {"[D", JVM_EXPORT_DOUBLE_ARRAY, 4, "g", 8},
    };
    for (size_t i = 0; i < sizeof(cases)/sizeof(cases[0]); i++) {
        jclass arr_class = (*env)->FindClass(env, cases[i].class_name);
        if (!arr_class) {
            if ((*env)->ExceptionCheck(env)) (*env)->ExceptionClear(env);
            continue;
        }
        if ((*env)->IsInstanceOf(env, obj, arr_class)) {
            int rc = omnivm_jvm_export_array_buffer(env, obj, cases[i].array_kind,
                                                    cases[i].dtype, cases[i].arrow_format,
                                                    cases[i].item_size, out);
            (*env)->DeleteLocalRef(env, arr_class);
            (*env)->PopLocalFrame(env, NULL);
            omnivm_jvm_maybe_detach(did_attach);
            return rc;
        }
        (*env)->DeleteLocalRef(env, arr_class);
    }

    (*env)->PopLocalFrame(env, NULL);
    omnivm_jvm_maybe_detach(did_attach);
    return 1;
}

static void omnivm_jvm_release_exported_buffer(void* raw) {
    jvm_exported_buffer_handle_t* handle = (jvm_exported_buffer_handle_t*)raw;
    if (!handle) return;
    int did_attach;
    JNIEnv* env = omnivm_jvm_get_env(&did_attach);
    if (env && handle->object) {
        if (handle->critical && handle->array_kind != JVM_EXPORT_DIRECT) {
            (*env)->ReleasePrimitiveArrayCritical(env, (jarray)handle->object, handle->critical, JNI_ABORT);
        }
        (*env)->DeleteGlobalRef(env, handle->object);
    }
    omnivm_jvm_maybe_detach(did_attach);
    free(handle);
}

static void omnivm_jvm_set_buf_callbacks(jvm_buf_get_fn get_fn,
                                          jvm_buf_set_fn set_fn,
                                          jvm_buf_release_fn release_fn,
                                          jvm_buf_free_fn free_fn,
                                          jvm_buf_status_fn status_fn) {
    g_buf_get = get_fn;
    g_buf_set = set_fn;
    g_buf_release = release_fn;
    g_buf_free = free_fn;
    g_buf_status = status_fn;
}

static void omnivm_jvm_set_typed_callback(jvm_call_typed_fn fn) {
    g_call_typed = fn;
}

static int omnivm_jvm_init(const char* classpath) {
    if (jvm_ptr && env_ptr) return 0;

    JavaVMInitArgs vm_args;
    JavaVMOption options[4];

    // Build classpath option: include our OmniVMRunner + user libs
    char cp_option[4096];
    snprintf(cp_option, sizeof(cp_option),
        "-Djava.class.path=%s:/omnivm/libs/*", classpath);

    options[0].optionString = (char*)"-Xrs";           // Reduce signal usage
    options[1].optionString = (char*)"-Xmx512m";       // Heap size
    options[2].optionString = cp_option;
    options[3].optionString = (char*)"-Djava.awt.headless=true";

    vm_args.version = JNI_VERSION_10;
    vm_args.nOptions = 4;
    vm_args.options = options;
    vm_args.ignoreUnrecognized = JNI_TRUE;

    int rc = JNI_CreateJavaVM(&jvm_ptr, (void**)&env_ptr, &vm_args);
    if (rc != 0) return rc;

    // Find OmniVMRunner class
    runner_class = (*env_ptr)->FindClass(env_ptr, "omnivm/OmniVMRunner");
    if (!runner_class) {
        fprintf(stderr, "[jvm] WARNING: OmniVMRunner class not found on classpath: %s\n", classpath);
        // Clear exception so JVM stays usable
        (*env_ptr)->ExceptionClear(env_ptr);
        return 0; // JVM initialized, but runner not available
    }

    // Make it a global ref so it survives GC
    runner_class = (jclass)(*env_ptr)->NewGlobalRef(env_ptr, runner_class);

    // Cache the execute method
    execute_method = (*env_ptr)->GetStaticMethodID(env_ptr, runner_class,
        "execute", "(Ljava/lang/String;)Ljava/lang/String;");
    if (!execute_method) {
        (*env_ptr)->ExceptionClear(env_ptr);
        fprintf(stderr, "[jvm] WARNING: OmniVMRunner.execute() method not found\n");
    }

    // Cache the eval method
    eval_method_id = (*env_ptr)->GetStaticMethodID(env_ptr, runner_class,
        "eval", "(Ljava/lang/String;)Ljava/lang/String;");
    if (!eval_method_id) {
        (*env_ptr)->ExceptionClear(env_ptr);
        // eval not available; will fall back to execute
    }

    // Cache the raw object-returning eval method used for automatic exports.
    eval_object_method_id = (*env_ptr)->GetStaticMethodID(env_ptr, runner_class,
        "evalObject", "(Ljava/lang/String;)Ljava/lang/Object;");
    if (!eval_object_method_id) {
        (*env_ptr)->ExceptionClear(env_ptr);
    }

    // Cache the executeFile method
    exec_file_method_id = (*env_ptr)->GetStaticMethodID(env_ptr, runner_class,
        "executeFile", "(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;");
    if (!exec_file_method_id) {
        (*env_ptr)->ExceptionClear(env_ptr);
    }

    // Register native methods for OmniVM
    jclass omnivm_class = (*env_ptr)->FindClass(env_ptr, "omnivm/OmniVM");
    if (omnivm_class) {
        JNINativeMethod methods[] = {
            {"nativeCall", "(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;",
             (void*)Java_omnivm_OmniVM_nativeCall},
            {"nativeGetBuffer", "(Ljava/lang/String;)[B",
             (void*)Java_omnivm_OmniVM_nativeGetBuffer},
            {"nativeGetBufferDtype", "(Ljava/lang/String;)I",
             (void*)Java_omnivm_OmniVM_nativeGetBufferDtype},
            {"nativeSetBuffer", "(Ljava/lang/String;[BI)V",
             (void*)Java_omnivm_OmniVM_nativeSetBuffer},
            {"nativeReleaseBuffer", "(Ljava/lang/String;)V",
             (void*)Java_omnivm_OmniVM_nativeReleaseBuffer},
            {"nativeBufferStatus", "(Ljava/lang/String;)Ljava/lang/String;",
             (void*)Java_omnivm_OmniVM_nativeBufferStatus},
            {"nativeCallTyped", "(Ljava/lang/String;Ljava/lang/String;[Ljava/lang/Object;)Ljava/lang/Object;",
             (void*)Java_omnivm_OmniVM_nativeCallTyped},
        };
        (*env_ptr)->RegisterNatives(env_ptr, omnivm_class, methods, 7);
        (*env_ptr)->ExceptionClear(env_ptr);
        (*env_ptr)->DeleteLocalRef(env_ptr, omnivm_class);
    } else {
        (*env_ptr)->ExceptionClear(env_ptr);
        fprintf(stderr, "[jvm] NOTE: omnivm/OmniVM class not found (bridge available after compilation)\n");
    }

    return 0;
}

// Execute Java code via OmniVMRunner.execute()
// Thread-safe: attaches current thread to JVM if needed.
// Returns output string (caller must free) or error prefixed with "JavaError: "
static char* omnivm_jvm_exec(const char* code) {
    int did_attach;
    JNIEnv* env = omnivm_jvm_get_env(&did_attach);
    if (!env) return strdup("JavaError: JVM not available");
    if (!runner_class || !execute_method) {
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: OmniVMRunner not available (check classpath)");
    }

    if ((*env)->PushLocalFrame(env, 16) < 0) {
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: PushLocalFrame failed");
    }

    jstring jcode = (*env)->NewStringUTF(env, code);
    if (!jcode) {
        (*env)->ExceptionClear(env);
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: Failed to create Java string");
    }

    omnivm_jvm_mark_active_thread(env);
    jstring result = (jstring)(*env)->CallStaticObjectMethod(
        env, runner_class, execute_method, jcode);
    omnivm_jvm_clear_active_thread(env);

    if ((*env)->ExceptionCheck(env)) {
        jthrowable exc = (*env)->ExceptionOccurred(env);
        (*env)->ExceptionClear(env);

        jclass throwable_class = (*env)->FindClass(env, "java/lang/Throwable");
        jmethodID to_string = (*env)->GetMethodID(env, throwable_class,
            "toString", "()Ljava/lang/String;");
        jstring msg = (jstring)(*env)->CallObjectMethod(env, exc, to_string);

        char* err = strdup("JavaError: Unknown JNI exception");
        if (msg) {
            const char* utf = (*env)->GetStringUTFChars(env, msg, NULL);
            size_t len = strlen(utf) + 20;
            char* formatted = (char*)malloc(len);
            if (formatted) {
                snprintf(formatted, len, "JavaError: %s", utf);
                free(err);
                err = formatted;
            }
            (*env)->ReleaseStringUTFChars(env, msg, utf);
        }
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return err;
    }

    if (!result) {
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: execute() returned null");
    }

    const char* utf = (*env)->GetStringUTFChars(env, result, NULL);
    char* output = strdup(utf);
    (*env)->ReleaseStringUTFChars(env, result, utf);

    (*env)->PopLocalFrame(env, NULL);
    omnivm_jvm_maybe_detach(did_attach);
    return output;
}

// Eval Java code via OmniVMRunner.eval() — returns expression value.
// Thread-safe: attaches current thread to JVM if needed.
static char* omnivm_jvm_eval(const char* code) {
    int did_attach;
    JNIEnv* env = omnivm_jvm_get_env(&did_attach);
    if (!env) return strdup("JavaError: JVM not available");

    // If eval method is available, use it
    if (runner_class && eval_method_id) {
        if ((*env)->PushLocalFrame(env, 16) < 0) {
            omnivm_jvm_maybe_detach(did_attach);
            return strdup("JavaError: PushLocalFrame failed");
        }

        jstring jcode = (*env)->NewStringUTF(env, code);
        if (!jcode) {
            (*env)->ExceptionClear(env);
            (*env)->PopLocalFrame(env, NULL);
            omnivm_jvm_maybe_detach(did_attach);
            return strdup("JavaError: Failed to create Java string");
        }

        omnivm_jvm_mark_active_thread(env);
        jstring result = (jstring)(*env)->CallStaticObjectMethod(
            env, runner_class, eval_method_id, jcode);
        omnivm_jvm_clear_active_thread(env);

        if ((*env)->ExceptionCheck(env)) {
            jthrowable exc = (*env)->ExceptionOccurred(env);
            (*env)->ExceptionClear(env);

            jclass throwable_class = (*env)->FindClass(env, "java/lang/Throwable");
            jmethodID to_string = (*env)->GetMethodID(env, throwable_class,
                "toString", "()Ljava/lang/String;");
            jstring msg = (jstring)(*env)->CallObjectMethod(env, exc, to_string);

            char* err = strdup("JavaError: Unknown JNI exception");
            if (msg) {
                const char* utf = (*env)->GetStringUTFChars(env, msg, NULL);
                size_t len = strlen(utf) + 20;
                char* formatted = (char*)malloc(len);
                if (formatted) {
                    snprintf(formatted, len, "JavaError: %s", utf);
                    free(err);
                    err = formatted;
                }
                (*env)->ReleaseStringUTFChars(env, msg, utf);
            }
            (*env)->PopLocalFrame(env, NULL);
            omnivm_jvm_maybe_detach(did_attach);
            return err;
        }

        if (!result) {
            (*env)->PopLocalFrame(env, NULL);
            omnivm_jvm_maybe_detach(did_attach);
            return strdup("null");
        }

        const char* utf = (*env)->GetStringUTFChars(env, result, NULL);
        char* output = strdup(utf);
        (*env)->ReleaseStringUTFChars(env, result, utf);

        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return output;
    }

    // Fall back to execute (which handles its own attach/detach)
    omnivm_jvm_maybe_detach(did_attach);
    return omnivm_jvm_exec(code);
}

// Execute a .java, .class, or .jar file via OmniVMRunner.executeFile().
// Thread-safe. Returns result string (caller must free):
//   "0"            - success (exit code 0)
//   "N"            - System.exit(N)
//   "JavaError: …" - compilation/runtime error
static char* omnivm_jvm_exec_file(const char* path, const char* args_joined) {
    int did_attach;
    JNIEnv* env = omnivm_jvm_get_env(&did_attach);
    if (!env) return strdup("JavaError: JVM not available");
    if (!runner_class || !exec_file_method_id) {
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: OmniVMRunner.executeFile() not available");
    }

    if ((*env)->PushLocalFrame(env, 16) < 0) {
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: PushLocalFrame failed");
    }

    jstring jpath = (*env)->NewStringUTF(env, path);
    jstring jargs = (*env)->NewStringUTF(env, args_joined ? args_joined : "");
    if (!jpath || !jargs) {
        (*env)->ExceptionClear(env);
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: Failed to create Java strings");
    }

    omnivm_jvm_mark_active_thread(env);
    jstring result = (jstring)(*env)->CallStaticObjectMethod(
        env, runner_class, exec_file_method_id, jpath, jargs);
    omnivm_jvm_clear_active_thread(env);

    if ((*env)->ExceptionCheck(env)) {
        jthrowable exc = (*env)->ExceptionOccurred(env);
        (*env)->ExceptionClear(env);

        jclass throwable_class = (*env)->FindClass(env, "java/lang/Throwable");
        jmethodID to_string = (*env)->GetMethodID(env, throwable_class,
            "toString", "()Ljava/lang/String;");
        jstring msg = (jstring)(*env)->CallObjectMethod(env, exc, to_string);

        char* err = strdup("JavaError: Unknown JNI exception");
        if (msg) {
            const char* utf = (*env)->GetStringUTFChars(env, msg, NULL);
            size_t len = strlen(utf) + 20;
            char* formatted = (char*)malloc(len);
            if (formatted) {
                snprintf(formatted, len, "JavaError: %s", utf);
                free(err);
                err = formatted;
            }
            (*env)->ReleaseStringUTFChars(env, msg, utf);
        }
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return err;
    }

    if (!result) {
        (*env)->PopLocalFrame(env, NULL);
        omnivm_jvm_maybe_detach(did_attach);
        return strdup("JavaError: executeFile() returned null");
    }

    const char* utf = (*env)->GetStringUTFChars(env, result, NULL);
    char* output = strdup(utf);
    (*env)->ReleaseStringUTFChars(env, result, utf);

    (*env)->PopLocalFrame(env, NULL);
    omnivm_jvm_maybe_detach(did_attach);
    return output;
}

static void omnivm_jvm_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

static void omnivm_jvm_shutdown(void) {
    // DestroyJavaVM can block indefinitely in embedded, multi-runtime hosts
    // when JVM-managed threads or JNI-attached native threads are still live.
    // Treat the JVM as process-lifetime and let OS teardown reclaim it.
}
*/
import "C"
import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/polyglot"
)

// ClasspathDir is where OmniVMRunner.class lives (set during Docker build).
var ClasspathDir = "/omnivm/java"

// LibDir is where user JARs can be placed.
var LibDir = "/omnivm/libs"

type Runtime struct {
	initialized bool
}

func New() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Name() string { return "java" }

func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("jvm: already initialized")
	}

	cClasspath := C.CString(ClasspathDir)
	defer C.free(unsafe.Pointer(cClasspath))

	rc := C.omnivm_jvm_init(cClasspath)
	if rc != 0 {
		return fmt.Errorf("jvm: JNI_CreateJavaVM failed (rc=%d)", rc)
	}

	r.initialized = true
	return nil
}

func (r *Runtime) Execute(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("jvm: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_jvm_exec(cCode)
	if cOutput == nil {
		return pkg.Result{Err: fmt.Errorf("jvm: execution returned nil")}
	}

	output := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))

	if strings.HasPrefix(output, "JavaError: ") {
		return pkg.Result{Err: fmt.Errorf("jvm: %s", strings.TrimPrefix(output, "JavaError: "))}
	}

	return pkg.Result{Output: output}
}

// Eval evaluates a Java expression and returns its value directly.
func (r *Runtime) Eval(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("jvm: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_jvm_eval(cCode)
	if cOutput == nil {
		return pkg.Result{Err: fmt.Errorf("jvm: eval failed")}
	}

	output := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))

	if strings.HasPrefix(output, "JavaError: ") {
		return pkg.Result{Err: fmt.Errorf("jvm: %s", strings.TrimPrefix(output, "JavaError: "))}
	}

	return pkg.Result{Value: output, Output: output}
}

// ExportBuffer publishes Java direct or array-backed NIO buffers and primitive
// arrays into OmniVM's shared data plane without a user-visible bridge API.
func (r *Runtime) ExportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	if !r.initialized {
		return pkg.ExportedBuffer{}, false, fmt.Errorf("jvm: not initialized")
	}

	cExpr := C.CString(expr)
	defer C.free(unsafe.Pointer(cExpr))

	var out C.jvm_omnivm_exported_buffer_t
	rc := C.omnivm_jvm_export_buffer(cExpr, &out)
	if rc < 0 {
		return pkg.ExportedBuffer{}, false, fmt.Errorf("jvm: export buffer failed")
	}
	if rc > 0 {
		return pkg.ExportedBuffer{}, false, nil
	}

	byteLen := int64(out.len)
	elements := int64(out.elements)
	if byteLen < 0 || elements < 0 || (byteLen > 0 && out.data == nil) || out.handle == nil {
		C.omnivm_jvm_release_exported_buffer(out.handle)
		return pkg.ExportedBuffer{}, false, fmt.Errorf("jvm: invalid exported buffer")
	}
	dtype := int32(out.dtype)
	arrowFormat := C.GoString(out.arrow_format)
	meta := arrow.BufferMetadata{
		Dtype:       dtype,
		Format:      arrowFormat,
		Shape:       []int64{elements},
		ReadOnly:    out.read_only != 0,
		Ownership:   "producer",
		MemorySpace: "host",
	}
	if _, err := arrow.GlobalStore().SetExternalWithMetadata(name, unsafe.Pointer(out.data), byteLen, meta, func() error {
		C.omnivm_jvm_release_exported_buffer(out.handle)
		return nil
	}); err != nil {
		C.omnivm_jvm_release_exported_buffer(out.handle)
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

// ExecuteFile runs a .java, .class, or .jar file with arguments.
// stdout/stderr go directly to the process streams (not captured).
// Implements pkg.FileExecutor.
func (r *Runtime) ExecuteFile(path string, args []string, stdin io.Reader) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("jvm: not initialized"), ExitCode: 1}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("jvm: %v", err), ExitCode: 1}
	}

	argsJoined := strings.Join(args, "\n")

	cPath := C.CString(absPath)
	defer C.free(unsafe.Pointer(cPath))
	cArgs := C.CString(argsJoined)
	defer C.free(unsafe.Pointer(cArgs))

	cResult := C.omnivm_jvm_exec_file(cPath, cArgs)
	if cResult == nil {
		return pkg.Result{Err: fmt.Errorf("jvm: executeFile returned nil"), ExitCode: 1}
	}

	result := C.GoString(cResult)
	C.free(unsafe.Pointer(cResult))

	if strings.HasPrefix(result, "JavaError: ") {
		return pkg.Result{
			Err:      fmt.Errorf("%s", strings.TrimPrefix(result, "JavaError: ")),
			ExitCode: 1,
		}
	}

	// Result is the exit code as a string
	exitCode, _ := strconv.Atoi(result)
	if exitCode != 0 {
		return pkg.Result{ExitCode: exitCode, Err: fmt.Errorf("exit status %d", exitCode)}
	}
	return pkg.Result{}
}

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_jvm_set_bridge_callback(
		C.omni_call_fn(unsafe.Pointer(callPtr)),
		C.omni_free_fn(unsafe.Pointer(freePtr)),
	)
}

// SetBufCallbacks installs the buffer bridge function pointers.
func (r *Runtime) SetBufCallbacks(getPtr, setPtr, releasePtr, freePtr, statusPtr uintptr) {
	C.omnivm_jvm_set_buf_callbacks(
		C.jvm_buf_get_fn(unsafe.Pointer(getPtr)),
		C.jvm_buf_set_fn(unsafe.Pointer(setPtr)),
		C.jvm_buf_release_fn(unsafe.Pointer(releasePtr)),
		C.jvm_buf_free_fn(unsafe.Pointer(freePtr)),
		C.jvm_buf_status_fn(unsafe.Pointer(statusPtr)),
	)
}

// SetTypedCallback installs the typed call bridge function pointer.
func (r *Runtime) SetTypedCallback(ptr uintptr) {
	C.omnivm_jvm_set_typed_callback(C.jvm_call_typed_fn(unsafe.Pointer(ptr)))
}

// InterruptFuncPtr returns the JNI interrupt hook used by the watchdog.
func (r *Runtime) InterruptFuncPtr() unsafe.Pointer {
	return C.omnivm_jvm_interrupt_active_thread_ptr()
}

// EvalTyped evaluates Java code and returns a typed polyglot.Value.
func (r *Runtime) EvalTyped(code string) polyglot.Value {
	if !r.initialized {
		return polyglot.Error("jvm: not initialized")
	}
	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cResult := C.omnivm_jvm_eval_typed(cCode)
	ptr := unsafe.Pointer(&cResult)
	val := polyglot.FromCValueRaw(ptr)
	polyglot.FreeCValueRaw(ptr)
	return val
}

func (r *Runtime) Pump() {}

func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false
	C.omnivm_jvm_shutdown()
	return nil
}
