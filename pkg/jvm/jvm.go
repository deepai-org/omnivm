// Package jvm embeds the Java Virtual Machine via JNI through cgo.
//
// Build requires: JDK development headers (jni.h).
package jvm

/*
#cgo CFLAGS: -I${JAVA_HOME}/include -I${JAVA_HOME}/include/linux
#cgo LDFLAGS: -L${JAVA_HOME}/lib/server -ljvm -Wl,-rpath,${JAVA_HOME}/lib/server

#include <jni.h>
#include <stdlib.h>
#include <string.h>

// Bridge callback pointer
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

static JavaVM* jvm = NULL;
static JNIEnv* env = NULL;

// Initialize the JVM with reduced signal handling (-Xrs)
static int omnivm_jvm_init(void) {
    JavaVMInitArgs vm_args;
    JavaVMOption options[3];

    options[0].optionString = (char*)"-Xrs";  // Reduce signal usage
    options[1].optionString = (char*)"-Xmx256m";  // Default heap size
    options[2].optionString = (char*)"-Djava.class.path=.";

    vm_args.version = JNI_VERSION_10;
    vm_args.nOptions = 3;
    vm_args.options = options;
    vm_args.ignoreUnrecognized = JNI_TRUE;

    return JNI_CreateJavaVM(&jvm, (void**)&env, &vm_args);
}

// Execute a Java expression via ScriptEngine
static char* omnivm_jvm_exec(const char* code) {
    if (!env) return strdup("JVM not initialized");

    jclass se_mgr_class = (*env)->FindClass(env, "javax/script/ScriptEngineManager");

    if (se_mgr_class) {
        jmethodID mgr_init = (*env)->GetMethodID(env, se_mgr_class, "<init>", "()V");
        jobject mgr = (*env)->NewObject(env, se_mgr_class, mgr_init);

        jmethodID get_engine = (*env)->GetMethodID(env, se_mgr_class, "getEngineByName",
            "(Ljava/lang/String;)Ljavax/script/ScriptEngine;");

        jstring engine_name = (*env)->NewStringUTF(env, "js");
        jobject engine = (*env)->CallObjectMethod(env, mgr, get_engine, engine_name);

        if (engine) {
            jclass engine_class = (*env)->GetObjectClass(env, engine);
            jmethodID eval_method = (*env)->GetMethodID(env, engine_class, "eval",
                "(Ljava/lang/String;)Ljava/lang/Object;");

            jstring jcode = (*env)->NewStringUTF(env, code);
            jobject result = (*env)->CallObjectMethod(env, engine, eval_method, jcode);

            if ((*env)->ExceptionCheck(env)) {
                jthrowable exc = (*env)->ExceptionOccurred(env);
                (*env)->ExceptionClear(env);
                jclass throwable_class = (*env)->FindClass(env, "java/lang/Throwable");
                jmethodID get_msg = (*env)->GetMethodID(env, throwable_class, "getMessage",
                    "()Ljava/lang/String;");
                jstring msg = (jstring)(*env)->CallObjectMethod(env, exc, get_msg);
                if (msg) {
                    const char* msg_utf = (*env)->GetStringUTFChars(env, msg, NULL);
                    char* err = (char*)malloc(strlen(msg_utf) + 20);
                    sprintf(err, "JavaError: %s", msg_utf);
                    (*env)->ReleaseStringUTFChars(env, msg, msg_utf);
                    return err;
                }
            }

            if (result) {
                jclass obj_class = (*env)->GetObjectClass(env, result);
                jmethodID to_string = (*env)->GetMethodID(env, obj_class, "toString",
                    "()Ljava/lang/String;");
                jstring str = (jstring)(*env)->CallObjectMethod(env, result, to_string);
                if (str) {
                    const char* utf = (*env)->GetStringUTFChars(env, str, NULL);
                    char* output = strdup(utf);
                    (*env)->ReleaseStringUTFChars(env, str, utf);
                    return output;
                }
            }

            return strdup("");
        } else {
            return strdup("JavaError: No script engine available. Install GraalJS or Nashorn.");
        }
    } else {
        (*env)->ExceptionClear(env);
        return strdup("JavaError: javax.script not available");
    }
}

static char* omnivm_jvm_eval(const char* code) {
    return omnivm_jvm_exec(code);
}

static void omnivm_jvm_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}

static void omnivm_jvm_shutdown(void) {
    if (jvm) {
        (*jvm)->DestroyJavaVM(jvm);
        jvm = NULL;
        env = NULL;
    }
}

static int omnivm_jvm_is_initialized(void) {
    return jvm != NULL ? 1 : 0;
}
*/
import "C"
import (
	"fmt"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
)

// Runtime implements pkg.Runtime for the JVM.
type Runtime struct {
	initialized bool
}

// New creates a new JVM runtime (not yet initialized).
func New() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Name() string { return "jvm" }

// Initialize creates the JVM with reduced signal handling.
// Must be called on the Golden Thread.
func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("jvm: already initialized")
	}

	rc := C.omnivm_jvm_init()
	if rc != 0 {
		return fmt.Errorf("jvm: JNI_CreateJavaVM failed (rc=%d)", rc)
	}

	r.initialized = true
	return nil
}

// Execute runs code via the JVM's ScriptEngine.
// Must be called on the Golden Thread.
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

// Pump is a no-op for JVM (no cooperative event loop to tick).
func (r *Runtime) Pump() {}

// Shutdown destroys the JVM.
func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false
	C.omnivm_jvm_shutdown()
	return nil
}
