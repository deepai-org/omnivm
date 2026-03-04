// Package jvm embeds the Java Virtual Machine via JNI through cgo.
//
// Uses javax.tools.JavaCompiler for in-memory compilation of Java source,
// with a helper class (OmniVMRunner) that handles compilation, classloading,
// and output capture. Requires a full JDK (not just JRE).
package jvm

/*
#include <jni.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

// Bridge callback pointer
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

static JavaVM* jvm_ptr = NULL;
static JNIEnv* env_ptr = NULL;
static jclass runner_class = NULL;
static jmethodID execute_method = NULL;
static jmethodID eval_method_id = NULL;

// JNI native implementation of OmniVM.call(runtime, code)
static jstring JNICALL Java_omnivm_OmniVM_nativeCall(JNIEnv* env, jclass cls,
                                                       jstring j_runtime, jstring j_code) {
    if (!g_bridge_call) {
        jclass exc_class = (*env)->FindClass(env, "java/lang/RuntimeException");
        (*env)->ThrowNew(env, exc_class, "omnivm bridge not initialized");
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

static int omnivm_jvm_init(const char* classpath) {
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

    // Register native method for OmniVM.call
    jclass omnivm_class = (*env_ptr)->FindClass(env_ptr, "omnivm/OmniVM");
    if (omnivm_class) {
        JNINativeMethod methods[] = {
            {"nativeCall", "(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;",
             (void*)Java_omnivm_OmniVM_nativeCall}
        };
        (*env_ptr)->RegisterNatives(env_ptr, omnivm_class, methods, 1);
        (*env_ptr)->ExceptionClear(env_ptr); // OK if OmniVM class not yet compiled
    } else {
        (*env_ptr)->ExceptionClear(env_ptr);
        fprintf(stderr, "[jvm] NOTE: omnivm/OmniVM class not found (bridge available after compilation)\n");
    }

    return 0;
}

// Execute Java code via OmniVMRunner.execute()
// Returns output string (caller must free) or error prefixed with "JavaError: "
static char* omnivm_jvm_exec(const char* code) {
    if (!env_ptr) return strdup("JavaError: JVM not initialized");
    if (!runner_class || !execute_method) {
        return strdup("JavaError: OmniVMRunner not available (check classpath)");
    }

    // Convert code to Java string
    jstring jcode = (*env_ptr)->NewStringUTF(env_ptr, code);
    if (!jcode) {
        (*env_ptr)->ExceptionClear(env_ptr);
        return strdup("JavaError: Failed to create Java string");
    }

    // Call OmniVMRunner.execute(code)
    jstring result = (jstring)(*env_ptr)->CallStaticObjectMethod(
        env_ptr, runner_class, execute_method, jcode);

    // Check for JNI-level exceptions
    if ((*env_ptr)->ExceptionCheck(env_ptr)) {
        jthrowable exc = (*env_ptr)->ExceptionOccurred(env_ptr);
        (*env_ptr)->ExceptionClear(env_ptr);

        jclass throwable_class = (*env_ptr)->FindClass(env_ptr, "java/lang/Throwable");
        jmethodID to_string = (*env_ptr)->GetMethodID(env_ptr, throwable_class,
            "toString", "()Ljava/lang/String;");
        jstring msg = (jstring)(*env_ptr)->CallObjectMethod(env_ptr, exc, to_string);

        if (msg) {
            const char* utf = (*env_ptr)->GetStringUTFChars(env_ptr, msg, NULL);
            size_t len = strlen(utf) + 20;
            char* err = (char*)malloc(len);
            snprintf(err, len, "JavaError: %s", utf);
            (*env_ptr)->ReleaseStringUTFChars(env_ptr, msg, utf);
            (*env_ptr)->DeleteLocalRef(env_ptr, msg);
            (*env_ptr)->DeleteLocalRef(env_ptr, exc);
            return err;
        }
        (*env_ptr)->DeleteLocalRef(env_ptr, exc);
        return strdup("JavaError: Unknown JNI exception");
    }

    if (!result) {
        (*env_ptr)->DeleteLocalRef(env_ptr, jcode);
        return strdup("JavaError: execute() returned null");
    }

    const char* utf = (*env_ptr)->GetStringUTFChars(env_ptr, result, NULL);
    char* output = strdup(utf);
    (*env_ptr)->ReleaseStringUTFChars(env_ptr, result, utf);
    (*env_ptr)->DeleteLocalRef(env_ptr, result);
    (*env_ptr)->DeleteLocalRef(env_ptr, jcode);

    return output;
}

// Eval Java code via OmniVMRunner.eval() — returns expression value
static char* omnivm_jvm_eval(const char* code) {
    if (!env_ptr) return strdup("JavaError: JVM not initialized");

    // If eval method is available, use it
    if (runner_class && eval_method_id) {
        jstring jcode = (*env_ptr)->NewStringUTF(env_ptr, code);
        if (!jcode) {
            (*env_ptr)->ExceptionClear(env_ptr);
            return strdup("JavaError: Failed to create Java string");
        }

        jstring result = (jstring)(*env_ptr)->CallStaticObjectMethod(
            env_ptr, runner_class, eval_method_id, jcode);

        if ((*env_ptr)->ExceptionCheck(env_ptr)) {
            jthrowable exc = (*env_ptr)->ExceptionOccurred(env_ptr);
            (*env_ptr)->ExceptionClear(env_ptr);

            jclass throwable_class = (*env_ptr)->FindClass(env_ptr, "java/lang/Throwable");
            jmethodID to_string = (*env_ptr)->GetMethodID(env_ptr, throwable_class,
                "toString", "()Ljava/lang/String;");
            jstring msg = (jstring)(*env_ptr)->CallObjectMethod(env_ptr, exc, to_string);

            if (msg) {
                const char* utf = (*env_ptr)->GetStringUTFChars(env_ptr, msg, NULL);
                size_t len = strlen(utf) + 20;
                char* err = (char*)malloc(len);
                snprintf(err, len, "JavaError: %s", utf);
                (*env_ptr)->ReleaseStringUTFChars(env_ptr, msg, utf);
                (*env_ptr)->DeleteLocalRef(env_ptr, msg);
                (*env_ptr)->DeleteLocalRef(env_ptr, exc);
                return err;
            }
            (*env_ptr)->DeleteLocalRef(env_ptr, exc);
            return strdup("JavaError: Unknown JNI exception");
        }

        if (!result) {
            (*env_ptr)->DeleteLocalRef(env_ptr, jcode);
            return strdup("null");
        }

        const char* utf = (*env_ptr)->GetStringUTFChars(env_ptr, result, NULL);
        char* output = strdup(utf);
        (*env_ptr)->ReleaseStringUTFChars(env_ptr, result, utf);
        (*env_ptr)->DeleteLocalRef(env_ptr, result);
        (*env_ptr)->DeleteLocalRef(env_ptr, jcode);
        return output;
    }

    // Fall back to execute
    return omnivm_jvm_exec(code);
}

static void omnivm_jvm_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

static void omnivm_jvm_shutdown(void) {
    if (env_ptr && runner_class) {
        (*env_ptr)->DeleteGlobalRef(env_ptr, runner_class);
        runner_class = NULL;
        execute_method = NULL;
        eval_method_id = NULL;
    }
    if (jvm_ptr) {
        (*jvm_ptr)->DestroyJavaVM(jvm_ptr);
        jvm_ptr = NULL;
        env_ptr = NULL;
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

func (r *Runtime) Name() string { return "jvm" }

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

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_jvm_set_bridge_callback(
		C.omni_call_fn(unsafe.Pointer(callPtr)),
		C.omni_free_fn(unsafe.Pointer(freePtr)),
	)
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
