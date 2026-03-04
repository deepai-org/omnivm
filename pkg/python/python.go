// Package python embeds CPython via cgo.
//
// Build requires: python3-dev headers and libpython3.
// The cgo directives use pkg-config to find the correct flags.
package python

/*
#cgo pkg-config: python3-embed
#include <Python.h>
#include <stdlib.h>

// Bridge callback pointer — set via omnivm_py_set_bridge_callback().
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

// Helper to run Python code and capture output via a StringIO redirect.
// Returns the output string (caller must free) or NULL on error.
static char* omnivm_py_exec(const char* code) {
    // Set up output capture
    PyObject* io_module = PyImport_ImportModule("io");
    if (!io_module) return NULL;

    PyObject* string_io_class = PyObject_GetAttrString(io_module, "StringIO");
    Py_DECREF(io_module);
    if (!string_io_class) return NULL;

    PyObject* string_io = PyObject_CallObject(string_io_class, NULL);
    Py_DECREF(string_io_class);
    if (!string_io) return NULL;

    // Redirect sys.stdout
    PyObject* sys_module = PyImport_ImportModule("sys");
    if (!sys_module) { Py_DECREF(string_io); return NULL; }

    PyObject* old_stdout = PyObject_GetAttrString(sys_module, "stdout");
    PyObject_SetAttrString(sys_module, "stdout", string_io);

    // Execute code
    int result = PyRun_SimpleString(code);

    // Restore stdout
    if (old_stdout) {
        PyObject_SetAttrString(sys_module, "stdout", old_stdout);
        Py_DECREF(old_stdout);
    }

    char* output = NULL;
    if (result == 0) {
        // Get captured output
        PyObject* getvalue = PyObject_GetAttrString(string_io, "getvalue");
        if (getvalue) {
            PyObject* py_output = PyObject_CallObject(getvalue, NULL);
            Py_DECREF(getvalue);
            if (py_output) {
                const char* utf8 = PyUnicode_AsUTF8(py_output);
                if (utf8) {
                    output = strdup(utf8);
                }
                Py_DECREF(py_output);
            }
        }
    }

    Py_DECREF(string_io);
    Py_DECREF(sys_module);

    return output;
}

// Helper to convert a PyObject to a strdup'd string.
static char* py_obj_to_str(PyObject* obj) {
    if (!obj || obj == Py_None) return strdup("None");
    PyObject* str_result = PyObject_Str(obj);
    Py_DECREF(obj);
    if (!str_result) return strdup("None");
    const char* utf8 = PyUnicode_AsUTF8(str_result);
    char* output = strdup(utf8 ? utf8 : "None");
    Py_DECREF(str_result);
    return output;
}

// Eval: try Py_eval_input first, then multi-line split (like Jupyter).
// Returns expression value as string (caller must free) or NULL on error.
static char* omnivm_py_eval(const char* code) {
    PyObject* main_module = PyImport_AddModule("__main__");
    if (!main_module) return NULL;
    PyObject* globals = PyModule_GetDict(main_module);

    // Try as single expression first (Py_eval_input)
    PyObject* result = PyRun_String(code, Py_eval_input, globals, globals);
    if (result) {
        return py_obj_to_str(result);
    }
    PyErr_Clear();

    // Multi-line: run all-but-last line as statements, eval last line as expression.
    // Find the last non-blank line.
    const char* end = code + strlen(code);
    while (end > code && (end[-1] == '\n' || end[-1] == ' ' || end[-1] == '\t' || end[-1] == '\r'))
        end--;
    if (end == code) {
        // Empty code
        return strdup("None");
    }

    // Find start of last line
    const char* last_start = end;
    while (last_start > code && last_start[-1] != '\n')
        last_start--;

    // If last line is indented, it's part of a block — run everything as statements
    if (last_start[0] == ' ' || last_start[0] == '\t') {
        result = PyRun_String(code, Py_file_input, globals, globals);
        if (!result) return NULL;
        Py_DECREF(result);
        return strdup("None");
    }

    // If there's no preceding code, just run as statement
    if (last_start == code) {
        result = PyRun_String(code, Py_file_input, globals, globals);
        if (!result) return NULL;
        Py_DECREF(result);
        return strdup("None");
    }

    // Split: head = statements, tail = last expression
    size_t head_len = last_start - code;
    size_t tail_len = end - last_start;

    char* head = (char*)malloc(head_len + 1);
    memcpy(head, code, head_len);
    head[head_len] = '\0';

    char* tail = (char*)malloc(tail_len + 1);
    memcpy(tail, last_start, tail_len);
    tail[tail_len] = '\0';

    // Run head as statements
    result = PyRun_String(head, Py_file_input, globals, globals);
    free(head);
    if (!result) {
        free(tail);
        return NULL;
    }
    Py_DECREF(result);

    // Try tail as expression
    result = PyRun_String(tail, Py_eval_input, globals, globals);
    if (result) {
        free(tail);
        return py_obj_to_str(result);
    }
    PyErr_Clear();

    // Last line isn't an expression, run as statement
    result = PyRun_String(tail, Py_file_input, globals, globals);
    free(tail);
    if (!result) return NULL;
    Py_DECREF(result);
    return strdup("None");
}

// Fetch the current Python error as a string. Caller must free.
static char* omnivm_py_fetch_error() {
    PyObject *type, *value, *traceback;
    PyErr_Fetch(&type, &value, &traceback);
    if (!value) return NULL;

    PyObject* str = PyObject_Str(value);
    char* result = NULL;
    if (str) {
        const char* utf8 = PyUnicode_AsUTF8(str);
        if (utf8) result = strdup(utf8);
        Py_DECREF(str);
    }

    Py_XDECREF(type);
    Py_XDECREF(value);
    Py_XDECREF(traceback);
    PyErr_Clear();
    return result;
}

// C implementation of omnivm.call(runtime, code) for Python
static PyObject* py_omnivm_call(PyObject* self, PyObject* args) {
    const char* runtime;
    const char* code;
    if (!PyArg_ParseTuple(args, "ss", &runtime, &code)) {
        return NULL;
    }

    if (!g_bridge_call) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm bridge not initialized");
        return NULL;
    }

    char* result = g_bridge_call(runtime, code);
    if (!result) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm.call returned NULL");
        return NULL;
    }

    // Check for error prefix
    if (strncmp(result, "ERR:", 4) == 0) {
        PyErr_SetString(PyExc_RuntimeError, result + 4);
        if (g_bridge_free) g_bridge_free(result);
        return NULL;
    }

    PyObject* py_result = PyUnicode_FromString(result);
    if (g_bridge_free) g_bridge_free(result);
    return py_result;
}

// Method table for the omnivm module
static PyMethodDef omnivm_methods[] = {
    {"call", py_omnivm_call, METH_VARARGS, "Call another runtime: omnivm.call(runtime, code)"},
    {NULL, NULL, 0, NULL}
};

// Module definition
static struct PyModuleDef omnivm_module_def = {
    PyModuleDef_HEAD_INIT,
    "omnivm",
    "OmniVM cross-runtime bridge",
    -1,
    omnivm_methods
};

// Register the omnivm module
static void omnivm_py_register_bridge() {
    PyObject* module = PyModule_Create(&omnivm_module_def);
    if (module) {
        PyObject* sys_modules = PySys_GetObject("modules");
        if (sys_modules) {
            PyDict_SetItemString(sys_modules, "omnivm", module);
        }
        Py_DECREF(module);
    }
}

static void omnivm_py_set_bridge_callback(omni_call_fn call_fn, omni_free_fn free_fn) {
    g_bridge_call = call_fn;
    g_bridge_free = free_fn;
}
*/
import "C"
import (
	"fmt"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
)

// Runtime implements pkg.Runtime for CPython.
type Runtime struct {
	initialized bool
}

// New creates a new Python runtime (not yet initialized).
func New() *Runtime {
	return &Runtime{}
}

func (r *Runtime) Name() string { return "python" }

// Initialize starts CPython with signal handlers disabled.
// Must be called on the Golden Thread.
func (r *Runtime) Initialize() error {
	if r.initialized {
		return fmt.Errorf("python: already initialized")
	}

	// 0 = skip signal handler registration (Go owns signals)
	C.Py_InitializeEx(0)

	if C.Py_IsInitialized() == 0 {
		return fmt.Errorf("python: Py_InitializeEx failed")
	}

	// Initialize the GIL state for multi-thread usage
	// (even though we primarily use the Golden Thread)
	if C.PyEval_ThreadsInitialized() == 0 {
		C.PyEval_InitThreads()
	}

	// Register the omnivm Python module and import it into __main__
	C.omnivm_py_register_bridge()
	C.PyRun_SimpleString(C.CString("import omnivm"))

	r.initialized = true
	return nil
}

// Execute runs Python code synchronously on the current thread.
// Must be called on the Golden Thread.
func (r *Runtime) Execute(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("python: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_py_exec(cCode)

	if cOutput == nil {
		// Check for Python error
		cErr := C.omnivm_py_fetch_error()
		if cErr != nil {
			errStr := C.GoString(cErr)
			C.free(unsafe.Pointer(cErr))
			return pkg.Result{Err: fmt.Errorf("python: %s", errStr)}
		}
		return pkg.Result{Err: fmt.Errorf("python: execution failed")}
	}

	output := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))
	return pkg.Result{Output: output}
}

// Eval evaluates a Python expression and returns its value directly.
// Uses two-pass: try Py_eval_input first, fall back to Py_file_input.
func (r *Runtime) Eval(code string) pkg.Result {
	if !r.initialized {
		return pkg.Result{Err: fmt.Errorf("python: not initialized")}
	}

	cCode := C.CString(code)
	defer C.free(unsafe.Pointer(cCode))

	cOutput := C.omnivm_py_eval(cCode)

	if cOutput == nil {
		cErr := C.omnivm_py_fetch_error()
		if cErr != nil {
			errStr := C.GoString(cErr)
			C.free(unsafe.Pointer(cErr))
			return pkg.Result{Err: fmt.Errorf("python: %s", errStr)}
		}
		return pkg.Result{Err: fmt.Errorf("python: eval failed")}
	}

	value := C.GoString(cOutput)
	C.free(unsafe.Pointer(cOutput))
	return pkg.Result{Value: value, Output: value}
}

// SetBridgeCallback installs the cross-runtime callback function pointer.
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	C.omnivm_py_set_bridge_callback(
		C.omni_call_fn(unsafe.Pointer(callPtr)),
		C.omni_free_fn(unsafe.Pointer(freePtr)),
	)
}

// ExecuteOffThread runs Python code on a separate OS thread with proper
// GIL management. Use this for CPU-bound work to avoid blocking the
// Golden Thread.
func (r *Runtime) ExecuteOffThread(code string) <-chan pkg.Result {
	ch := make(chan pkg.Result, 1)
	go func() {
		// Acquire the GIL from this (non-main) thread
		gstate := C.PyGILState_Ensure()
		result := r.Execute(code)
		C.PyGILState_Release(gstate)
		ch <- result
	}()
	return ch
}

// Pump runs pending asyncio events. Called by the dispatcher on every cycle.
func (r *Runtime) Pump() {
	if !r.initialized {
		return
	}
	// Run one iteration of the asyncio event loop if one is running
	C.PyRun_SimpleString(C.CString(`
import asyncio
try:
    loop = asyncio.get_event_loop()
    if loop.is_running():
        loop.call_soon(loop.stop)
        loop.run_forever()
except RuntimeError:
    pass
`))
}

// Shutdown finalizes CPython.
func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false
	C.Py_FinalizeEx()
	return nil
}
