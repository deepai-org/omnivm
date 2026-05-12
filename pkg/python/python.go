// Package python embeds CPython via cgo.
//
// Build requires: python3-dev headers and libpython3.
// The cgo directives use pkg-config to find the correct flags.
package python

/*
#cgo pkg-config: python-3.14-embed
#include <Python.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <pthread.h>
#ifdef __GLIBC__
#include <execinfo.h>
#endif

// Bridge callback pointer — set via omnivm_py_set_bridge_callback().
typedef char* (*omni_call_fn)(const char* runtime, const char* code);
typedef void (*omni_free_fn)(char* ptr);
static omni_call_fn g_bridge_call = NULL;
static omni_free_fn g_bridge_free = NULL;

// Buffer bridge function pointers — set via omnivm_py_set_buf_callbacks().
// get: fill an omni_buffer_t from a named shared buffer. Returns 0 on success.
// set: register a buffer under a name. Returns 0 on success.
// release: schedule deferred release (safe from GC threads).
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
} py_omni_buffer_t;

typedef int (*omni_buf_get_fn)(const char* name, py_omni_buffer_t* out);
typedef int (*omni_buf_set_fn)(const char* name, py_omni_buffer_t buf);
typedef void (*omni_buf_release_fn)(const char* name);

static omni_buf_get_fn g_buf_get = NULL;
static omni_buf_set_fn g_buf_set = NULL;
static omni_buf_release_fn g_buf_release = NULL;

static void omnivm_py_set_buf_callbacks(omni_buf_get_fn get_fn,
                                         omni_buf_set_fn set_fn,
                                         omni_buf_release_fn release_fn) {
    g_buf_get = get_fn;
    g_buf_set = set_fn;
    g_buf_release = release_fn;
}

// Helper to get the value from a StringIO object as a strdup'd C string.
static char* get_stringio_value(PyObject* sio) {
    PyObject* getvalue = PyObject_GetAttrString(sio, "getvalue");
    if (!getvalue) return NULL;
    PyObject* py_val = PyObject_CallObject(getvalue, NULL);
    Py_DECREF(getvalue);
    if (!py_val) return NULL;
    const char* utf8 = PyUnicode_AsUTF8(py_val);
    char* result = utf8 ? strdup(utf8) : NULL;
    Py_DECREF(py_val);
    return result;
}

// Inner helper: runs Python code and captures stdout/stderr via StringIO redirect.
// Must be called with GIL held. Returns output (caller must free) or NULL on error.
// On error, returns "ERR:<traceback>" so the Go side can extract the error message.
static char* omnivm_py_exec_inner(const char* code) {
    // Set up output capture
    PyObject* io_module = PyImport_ImportModule("io");
    if (!io_module) return NULL;

    PyObject* string_io_class = PyObject_GetAttrString(io_module, "StringIO");
    Py_DECREF(io_module);
    if (!string_io_class) return NULL;

    PyObject* stdout_io = PyObject_CallObject(string_io_class, NULL);
    if (!stdout_io) { Py_DECREF(string_io_class); return NULL; }

    PyObject* stderr_io = PyObject_CallObject(string_io_class, NULL);
    Py_DECREF(string_io_class);
    if (!stderr_io) { Py_DECREF(stdout_io); return NULL; }

    // Redirect sys.stdout and sys.stderr
    PyObject* sys_module = PyImport_ImportModule("sys");
    if (!sys_module) { Py_DECREF(stdout_io); Py_DECREF(stderr_io); return NULL; }

    PyObject* old_stdout = PyObject_GetAttrString(sys_module, "stdout");
    PyObject* old_stderr = PyObject_GetAttrString(sys_module, "stderr");
    PyObject_SetAttrString(sys_module, "stdout", stdout_io);
    PyObject_SetAttrString(sys_module, "stderr", stderr_io);

    // Execute code
    int result = PyRun_SimpleString(code);

    // Restore stdout and stderr
    if (old_stdout) {
        PyObject_SetAttrString(sys_module, "stdout", old_stdout);
        Py_DECREF(old_stdout);
    }
    if (old_stderr) {
        PyObject_SetAttrString(sys_module, "stderr", old_stderr);
        Py_DECREF(old_stderr);
    }

    char* output = NULL;
    if (result == 0) {
        output = get_stringio_value(stdout_io);
    } else {
        // On error, return the captured traceback from stderr as ERR: prefix
        char* err_text = get_stringio_value(stderr_io);
        if (err_text && strlen(err_text) > 0) {
            size_t len = strlen(err_text) + 5; // "ERR:" + null
            output = (char*)malloc(len);
            if (output) {
                snprintf(output, len, "ERR:%s", err_text);
            }
            free(err_text);
        }
        // If no stderr captured, output stays NULL
    }

    Py_DECREF(stdout_io);
    Py_DECREF(stderr_io);
    Py_DECREF(sys_module);

    return output;
}

// Thread-safe exec: acquires GIL, delegates to inner, releases GIL.
// PyGILState_Ensure is recursive-safe — no-op if GIL already held.
static char* omnivm_py_exec(const char* code) {
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_exec_inner(code);
    PyGILState_Release(gstate);
    return result;
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

// Inner eval: try Py_eval_input first, then multi-line split (like Jupyter).
// Must be called with GIL held. Returns expression value as string (caller must free) or NULL on error.
static char* omnivm_py_eval_inner(const char* code) {
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
    if (!head) return NULL;
    memcpy(head, code, head_len);
    head[head_len] = '\0';

    char* tail = (char*)malloc(tail_len + 1);
    if (!tail) { free(head); return NULL; }
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

// Thread-safe eval: acquires GIL, delegates to inner, releases GIL.
static char* omnivm_py_eval(const char* code) {
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_eval_inner(code);
    PyGILState_Release(gstate);
    return result;
}

// Inner fetch error: retrieves current Python error as a string.
// Must be called with GIL held. Caller must free.
static char* omnivm_py_fetch_error_inner() {
    PyObject *type, *value, *traceback;
    PyErr_Fetch(&type, &value, &traceback);
    if (!value) {
        Py_XDECREF(type);
        Py_XDECREF(traceback);
        return NULL;
    }

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

// Thread-safe fetch error: acquires GIL, delegates to inner, releases GIL.
static char* omnivm_py_fetch_error() {
    PyGILState_STATE gstate = PyGILState_Ensure();
    char* result = omnivm_py_fetch_error_inner();
    PyGILState_Release(gstate);
    return result;
}

// C implementation of omnivm.call(runtime, code) for Python.
// Releases GIL during the cross-runtime call so other threads can run Python
// concurrently. This also prevents deadlock when a foreign thread holds the
// GIL and calls into a runtime that needs to pump Python.
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

    // Release GIL while calling into other runtime.
    // Between SaveThread and RestoreThread, NO Python C-API calls are allowed.
    PyThreadState* _save = PyEval_SaveThread();
    char* result = g_bridge_call(runtime, code);
    PyEval_RestoreThread(_save);

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

// py_omnivm_get_buffer(name) -> memoryview or None
// Returns a memoryview wrapping the shared buffer's data (zero-copy).
static PyObject* py_omnivm_get_buffer(PyObject* self, PyObject* args) {
    const char* name;
    if (!PyArg_ParseTuple(args, "s", &name)) return NULL;

    if (!g_buf_get) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm buffer bridge not initialized");
        return NULL;
    }

    py_omni_buffer_t buf;
    memset(&buf, 0, sizeof(buf));
    int rc = g_buf_get(name, &buf);
    if (rc != 0) {
        Py_RETURN_NONE;
    }

    if (buf.data == NULL || buf.len <= 0) {
        // Return empty bytes
        return PyBytes_FromStringAndSize("", 0);
    }

    // Return a memoryview over the shared buffer (zero-copy, read-only).
    // The buffer must remain valid while the memoryview is in use.
    return PyMemoryView_FromMemory((char*)buf.data, (Py_ssize_t)buf.len, PyBUF_READ);
}

// py_omnivm_set_buffer(name, data, dtype=0) -> None
// Copies the buffer-protocol object into the shared store.
static PyObject* py_omnivm_set_buffer(PyObject* self, PyObject* args) {
    const char* name;
    Py_buffer view;
    int dtype = 0; // default: bytes
    if (!PyArg_ParseTuple(args, "sy*|i", &name, &view, &dtype)) return NULL;

    if (!g_buf_set) {
        PyBuffer_Release(&view);
        PyErr_SetString(PyExc_RuntimeError, "omnivm buffer bridge not initialized");
        return NULL;
    }

    py_omni_buffer_t buf;
    buf.data = view.buf;
    buf.len = (int64_t)view.len;
    buf.dtype = (int32_t)dtype;
    buf.owned = 0; // Go side will copy

    int rc = g_buf_set(name, buf);
    PyBuffer_Release(&view);

    if (rc != 0) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm.set_buffer failed");
        return NULL;
    }
    Py_RETURN_NONE;
}

// py_omnivm_release_buffer(name) -> None
// Schedule a deferred release of a named buffer.
static PyObject* py_omnivm_release_buffer(PyObject* self, PyObject* args) {
    const char* name;
    if (!PyArg_ParseTuple(args, "s", &name)) return NULL;

    if (!g_buf_release) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm buffer bridge not initialized");
        return NULL;
    }

    g_buf_release(name);
    Py_RETURN_NONE;
}

// Method table for the omnivm module
static PyMethodDef omnivm_methods[] = {
    {"call", py_omnivm_call, METH_VARARGS, "Call another runtime: omnivm.call(runtime, code)"},
    {"get_buffer", py_omnivm_get_buffer, METH_VARARGS, "Get a shared buffer: omnivm.get_buffer(name) -> memoryview|None"},
    {"set_buffer", py_omnivm_set_buffer, METH_VARARGS, "Set a shared buffer: omnivm.set_buffer(name, data, dtype=0)"},
    {"release_buffer", py_omnivm_release_buffer, METH_VARARGS, "Release a shared buffer: omnivm.release_buffer(name)"},
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

static void omnivm_py_set_bridge_free(omni_free_fn free_fn) {
    g_bridge_free = free_fn;
}

// Pipe-based interrupt mechanism.
// PyErr_SetInterrupt() from a non-Python thread (cgo) fails because
// _PyRuntimeState_GetThreadState() returns NULL, so eval_breaker is
// never set. Instead, we use a pipe: Go writes a byte, a Python daemon
// thread reads it and calls _thread.interrupt_main() which has a proper
// thread state and correctly triggers eval_breaker.
static int interrupt_pipe[2] = {-1, -1};

// Create the interrupt pipe and start a Python daemon thread that waits
// for bytes and calls _thread.interrupt_main(). Must be called after
// Py_InitializeEx and signal handler setup.
static void omnivm_py_setup_interrupt(void) {
    if (pipe(interrupt_pipe) != 0) return;
    char code[512];
    snprintf(code, sizeof(code),
        "import threading, os, _thread\n"
        "def _omnivm_interrupt_watcher():\n"
        "    while True:\n"
        "        os.read(%d, 1)\n"
        "        _thread.interrupt_main()\n"
        "_t = threading.Thread(target=_omnivm_interrupt_watcher, daemon=True)\n"
        "_t.start()\n",
        interrupt_pipe[0]);
    PyRun_SimpleString(code);
}

// Signal the Python interrupt watcher thread. Safe from any thread,
// no GIL or thread state required — just a write() to a pipe fd.
static void omnivm_py_interrupt(void) {
    if (interrupt_pipe[1] >= 0) {
        char c = 1;
        (void)write(interrupt_pipe[1], &c, 1);
    }
}

// Drain any stale interrupt: empty the pipe, wait for the watcher thread
// to finish processing any byte it already read, then absorb any pending
// KeyboardInterrupt. Call this before code that must not be interrupted
// by a prior test's leftover interrupt.
static void omnivm_py_clear_interrupt(void) {
    // Drain unread bytes from the pipe (non-blocking) — no GIL needed
    if (interrupt_pipe[0] >= 0) {
        char buf[16];
        int flags = fcntl(interrupt_pipe[0], F_GETFL, 0);
        fcntl(interrupt_pipe[0], F_SETFL, flags | O_NONBLOCK);
        while (read(interrupt_pipe[0], buf, sizeof(buf)) > 0) {}
        fcntl(interrupt_pipe[0], F_SETFL, flags);
    }
    // Wait for watcher thread to process any byte it already read
    usleep(10000); // 10ms
    // Absorb any KeyboardInterrupt already queued by the watcher thread
    // Needs GIL since we call Python C-API.
    PyGILState_STATE gstate = PyGILState_Ensure();
    PyRun_SimpleString("try:\n pass\nexcept KeyboardInterrupt:\n pass");
    PyErr_Clear();
    PyGILState_Release(gstate);
}

// Return a function pointer to omnivm_py_interrupt for the watchdog.
static void* omnivm_py_get_interrupt_ptr(void) {
	return (void*)omnivm_py_interrupt;
}

// Fork guard: fork() after JVM/Ruby init leaves dead threads holding mutexes.
// Install a pthread_atfork child handler that kills the child immediately.
// Conditional: only fires if JVM or Ruby were loaded (Go+JS are fork-safe
// when initialized post-fork).
static int fork_guard_active = 0;

static void omnivm_fork_child_handler(void) {
    if (!fork_guard_active) return;

    const char* msg = "FATAL: fork() in OmniVM polyglot process. "
                      "JVM/Ruby threads do not survive fork(). "
                      "Use multiprocessing.set_start_method('spawn').\n";
    write(STDERR_FILENO, msg, strlen(msg));

    // Log C call stack to help identify the offending fork() call site.
    // backtrace() is async-signal-safe enough for a dying child process.
#ifdef __GLIBC__
    const char* hdr = "Fork call stack (child process, pre-_exit):\n";
    write(STDERR_FILENO, hdr, strlen(hdr));
    void* frames[32];
    int n = backtrace(frames, 32);
    backtrace_symbols_fd(frames, n, STDERR_FILENO);
#endif

    // Also dump Python stack if possible — this is the most likely culprit.
    // We're in the forked child so the GIL state is undefined, but
    // Py_IsInitialized() and faulthandler are safe enough pre-_exit.
    if (Py_IsInitialized()) {
        const char* py_hdr = "Python stack at fork:\n";
        write(STDERR_FILENO, py_hdr, strlen(py_hdr));
        // faulthandler.dump_traceback writes directly to fd, no GIL needed
        PyRun_SimpleString(
            "import faulthandler; faulthandler.dump_traceback(open(2,'w'))");
    }

    _exit(71);
}

static void omnivm_install_fork_guard(void) {
    pthread_atfork(NULL, NULL, omnivm_fork_child_handler);
}

static void omnivm_activate_fork_guard(void) {
    fork_guard_active = 1;
}

// -------------------------------------------------------------------
// Python interpreter mode: _omnivm built-in C extension module
// -------------------------------------------------------------------
// Registered via PyImport_AppendInittab BEFORE Py_BytesMain() so that
// "import omnivm" works in any Python code run by the interpreter.
// Defers actual runtime init to omnivm.init_runtimes().

// Callbacks into Go (set by the main binary via OmniSetPythonModeCallbacks)
typedef char* (*omni_init_runtimes_fn)(const char* list);
typedef char* (*omni_load_plugin_fn)(const char* runtime, const char* path);
typedef void  (*omni_shutdown_fn)(void);

static omni_init_runtimes_fn  g_init_runtimes = NULL;
static omni_load_plugin_fn    g_load_plugin   = NULL;
static omni_shutdown_fn       g_shutdown      = NULL;

static void omnivm_set_pymode_callbacks(
    omni_init_runtimes_fn init_fn,
    omni_load_plugin_fn   load_fn,
    omni_shutdown_fn      shut_fn
) {
    g_init_runtimes = init_fn;
    g_load_plugin   = load_fn;
    g_shutdown      = shut_fn;
}

// omnivm.init_runtimes(["go", "javascript"])
static PyObject* pymode_init_runtimes(PyObject* self, PyObject* args) {
    PyObject* list;
    if (!PyArg_ParseTuple(args, "O!", &PyList_Type, &list)) return NULL;

    if (!g_init_runtimes) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm: not running in OmniVM interpreter mode");
        return NULL;
    }

    // Build comma-separated string from list
    Py_ssize_t n = PyList_Size(list);
    char buf[256] = {0};
    int pos = 0;
    for (Py_ssize_t i = 0; i < n && pos < 250; i++) {
        PyObject* item = PyList_GetItem(list, i);
        const char* s = PyUnicode_AsUTF8(item);
        if (!s) return NULL;
        if (i > 0) buf[pos++] = ',';
        int len = strlen(s);
        if (pos + len >= 255) break;
        memcpy(buf + pos, s, len);
        pos += len;
    }
    buf[pos] = '\0';

    // Release GIL during Go runtime initialization
    PyThreadState* _save = PyEval_SaveThread();
    char* result = g_init_runtimes(buf);
    PyEval_RestoreThread(_save);

    if (result && strncmp(result, "ERR:", 4) == 0) {
        PyErr_SetString(PyExc_RuntimeError, result + 4);
        if (g_bridge_free) g_bridge_free(result);
        return NULL;
    }
    if (result && g_bridge_free) g_bridge_free(result);
    Py_RETURN_NONE;
}

// omnivm.call(runtime, code)
// Reuses g_bridge_call which is set by init_runtimes via SetBridgeCallback.
static PyObject* pymode_call(PyObject* self, PyObject* args) {
    return py_omnivm_call(self, args);
}

// omnivm.load_plugin(runtime, path)
static PyObject* pymode_load_plugin(PyObject* self, PyObject* args) {
    const char* runtime;
    const char* path;
    if (!PyArg_ParseTuple(args, "ss", &runtime, &path)) return NULL;

    if (!g_load_plugin) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm: not running in OmniVM interpreter mode");
        return NULL;
    }

    PyThreadState* _save = PyEval_SaveThread();
    char* result = g_load_plugin(runtime, path);
    PyEval_RestoreThread(_save);

    if (result && strncmp(result, "ERR:", 4) == 0) {
        PyErr_SetString(PyExc_RuntimeError, result + 4);
        if (g_bridge_free) g_bridge_free(result);
        return NULL;
    }
    if (result && g_bridge_free) g_bridge_free(result);
    Py_RETURN_NONE;
}

// omnivm.shutdown()
static PyObject* pymode_shutdown(PyObject* self, PyObject* args) {
    if (g_shutdown) {
        PyThreadState* _save = PyEval_SaveThread();
        g_shutdown();
        PyEval_RestoreThread(_save);
    }
    Py_RETURN_NONE;
}

// omnivm.execute(runtime, code) — runs code, returns captured stdout
static PyObject* pymode_execute(PyObject* self, PyObject* args) {
    const char* runtime;
    const char* code;
    if (!PyArg_ParseTuple(args, "ss", &runtime, &code)) return NULL;

    if (!g_bridge_call) {
        PyErr_SetString(PyExc_RuntimeError, "omnivm bridge not initialized — call init_runtimes() first");
        return NULL;
    }

    // For execute, we use the same bridge but with a different Go-side handler.
    // The bridge's OmniCall uses Eval(); for stdout capture we'd need Execute().
    // For now, delegate to the same call path — execute vs eval distinction
    // is handled at the Go level by the caller's code string.
    return py_omnivm_call(self, args);
}

static PyMethodDef omnivm_pymode_methods[] = {
    {"init_runtimes", pymode_init_runtimes, METH_VARARGS, "Initialize runtimes: omnivm.init_runtimes(['go', 'javascript'])"},
    {"call",          pymode_call,          METH_VARARGS, "Call a runtime: omnivm.call('go', 'expr')"},
    {"load_plugin",   pymode_load_plugin,   METH_VARARGS, "Load a plugin: omnivm.load_plugin('go', '/path/to/plugin.so')"},
    {"shutdown",      pymode_shutdown,       METH_NOARGS,  "Shut down runtimes"},
    {"execute",       pymode_execute,        METH_VARARGS, "Execute code: omnivm.execute('javascript', 'code')"},
    {NULL, NULL, 0, NULL}
};

static struct PyModuleDef omnivm_pymode_module_def = {
    PyModuleDef_HEAD_INIT,
    "omnivm",
    "OmniVM polyglot runtime — call Go, JavaScript, and other runtimes from Python",
    -1,
    omnivm_pymode_methods
};

// Called by PyImport_AppendInittab registration. This is the init function
// for "import omnivm" when running in Python interpreter mode.
PyMODINIT_FUNC PyInit_omnivm(void) {
    PyObject* mod = PyModule_Create(&omnivm_pymode_module_def);
    if (!mod) return NULL;

    // Add RuntimeError subclass for omnivm-specific errors
    PyObject* base = PyExc_RuntimeError;
    PyObject* exc = PyErr_NewException("omnivm.RuntimeError", base, NULL);
    if (exc) {
        PyModule_AddObject(mod, "RuntimeError", exc);
    }

    return mod;
}
*/
import "C"
import (
	"fmt"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
)

// Pre-allocated C strings to avoid repeated malloc in hot paths.
// These are allocated once and never freed (process-lifetime).
var (
	cImportOmnivm    *C.char
	cSetupInterrupt  *C.char
	cPumpCode        *C.char
	cForceSpawnMode  *C.char
)

func init() {
	cImportOmnivm = C.CString("import omnivm")
	// Install Python's default SIGINT handler so _thread.interrupt_main() works.
	// Py_InitializeEx(0) skips signal handler setup, leaving the handler table
	// empty. Without this, _thread.interrupt_main() has no handler to invoke.
	cSetupInterrupt = C.CString("import signal; signal.signal(signal.SIGINT, signal.default_int_handler)")
	cForceSpawnMode = C.CString("import multiprocessing; multiprocessing.set_start_method('spawn', force=True)")
	cPumpCode = C.CString(`
import asyncio
try:
    loop = asyncio.get_event_loop()
    if loop.is_running():
        loop.call_soon(loop.stop)
        loop.run_forever()
except RuntimeError:
    pass
`)
}

// cpythonInitialized guards against double CPython init across Runtime instances.
// CPython can only be initialized once per process; a second call crashes on 3.14+.
var cpythonInitialized bool

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

	if cpythonInitialized {
		// CPython was already initialized (and never truly finalized).
		// Just mark this Runtime as initialized — the interpreter is still live.
		r.initialized = true
		return nil
	}

	// 0 = skip signal handler registration (Go owns signals)
	C.Py_InitializeEx(0)

	if C.Py_IsInitialized() == 0 {
		return fmt.Errorf("python: Py_InitializeEx failed")
	}

	// Register the omnivm Python module and import it into __main__
	C.omnivm_py_register_bridge()
	C.PyRun_SimpleString(cImportOmnivm)

	// Install Python's default SIGINT handler so _thread.interrupt_main() works.
	C.PyRun_SimpleString(cSetupInterrupt)

	// Set up pipe-based interrupt: a daemon thread reads from a pipe and
	// calls _thread.interrupt_main(). This lets Go's Interrupt() work from
	// any goroutine without needing the GIL or a Python thread state.
	C.omnivm_py_setup_interrupt()

	// Install fork guard: child processes created by fork() in a polyglot
	// process with JVM threads will deadlock. Kill them immediately.
	// In Go-hosted mode, activate immediately (JVM/Ruby may be loaded later).
	C.omnivm_install_fork_guard()
	C.omnivm_activate_fork_guard()

	// Force multiprocessing to use 'spawn' instead of 'fork'.
	// fork() after JVM init leaves dead threads holding mutexes.
	C.PyRun_SimpleString(cForceSpawnMode)

	// Release the GIL so it's available for all threads (including the
	// Golden Thread). Every subsequent Python call acquires/releases the
	// GIL via PyGILState_Ensure/Release. Without this, the main thread
	// holds the GIL forever and foreign threads deadlock on Ensure().
	C.PyEval_SaveThread()

	r.initialized = true
	cpythonInitialized = true
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

	// Check for ERR: prefix (captured traceback from stderr)
	if len(output) > 4 && output[:4] == "ERR:" {
		return pkg.Result{Err: fmt.Errorf("%s", output[4:])}
	}

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

// SetBufCallbacks installs the buffer bridge function pointers.
func (r *Runtime) SetBufCallbacks(getPtr, setPtr, releasePtr uintptr) {
	C.omnivm_py_set_buf_callbacks(
		C.omni_buf_get_fn(unsafe.Pointer(getPtr)),
		C.omni_buf_set_fn(unsafe.Pointer(setPtr)),
		C.omni_buf_release_fn(unsafe.Pointer(releasePtr)),
	)
}

// ExecuteOffThread runs Python code on a separate OS thread.
// GIL acquisition is handled automatically by omnivm_py_exec (Phase 1A).
func (r *Runtime) ExecuteOffThread(code string) <-chan pkg.Result {
	ch := make(chan pkg.Result, 1)
	go func() {
		result := r.Execute(code)
		ch <- result
	}()
	return ch
}

// Pump runs pending asyncio events. Called by the dispatcher on every cycle.
// Acquires the GIL since the main thread no longer holds it persistently.
func (r *Runtime) Pump() {
	if !r.initialized {
		return
	}
	gstate := C.PyGILState_Ensure()
	C.PyRun_SimpleString(cPumpCode)
	C.PyGILState_Release(gstate)
}

// Interrupt raises a KeyboardInterrupt in Python at the next bytecode check.
// Writes a byte to the interrupt pipe; a Python daemon thread reads it and
// calls _thread.interrupt_main(). Safe from any goroutine — no GIL needed.
func (r *Runtime) Interrupt() {
	if r.initialized {
		C.omnivm_py_interrupt()
	}
}

// InterruptFuncPtr returns a C function pointer to omnivm_py_interrupt().
// This is safe to call from any thread (including the watchdog pthread)
// because it only performs a write() to the interrupt pipe.
func (r *Runtime) InterruptFuncPtr() unsafe.Pointer {
	return unsafe.Pointer(C.omnivm_py_get_interrupt_ptr())
}

// ClearInterrupt drains any stale interrupt from the pipe and absorbs any
// pending KeyboardInterrupt. Use between tests or after timed interrupts
// where the interrupt goroutine may fire after the target code returns.
func (r *Runtime) ClearInterrupt() {
	if r.initialized {
		C.omnivm_py_clear_interrupt()
	}
}

// Shutdown finalizes CPython.
// In polyglot mode, Py_FinalizeEx can crash when other runtime threads
// (JVM, Ruby proxy) are still active. When running standalone, it can also
// crash due to signal handler teardown conflicts with libjsig.so.
// Since we're exiting the process anyway, skip finalization — same strategy
// as Ruby shutdown.
func (r *Runtime) Shutdown() error {
	if !r.initialized {
		return nil
	}
	r.initialized = false
	// Skip Py_FinalizeEx — process exit reclaims all resources.
	// See Ruby shutdown strategy in MEMORY.md.
	return nil
}

// ActivateForkGuard enables the fork guard (for JVM/Ruby).
// When only Go+JS are loaded, fork is safe if runtimes are initialized post-fork.
func ActivateForkGuard() {
	C.omnivm_activate_fork_guard()
}

// RegisterAppendInittab registers the "omnivm" built-in module with CPython
// via PyImport_AppendInittab. Must be called BEFORE Py_Initialize/Py_BytesMain.
// This is used in Python interpreter mode so "import omnivm" works.
func RegisterAppendInittab() {
	cName := C.CString("omnivm")
	C.PyImport_AppendInittab(cName, (*[0]byte)(C.PyInit_omnivm))
	// Intentionally leak cName — AppendInittab stores the pointer.
}

// BytesMain calls Py_BytesMain with the given arguments, running CPython's
// full CLI (handles -m, -c, script files, interactive REPL, etc.).
// Returns the exit code from CPython.
func BytesMain(args []string) int {
	argc := C.int(len(args))
	argv := make([]*C.char, len(args))
	for i, a := range args {
		argv[i] = C.CString(a)
	}
	// Py_BytesMain takes char** — pass pointer to first element.
	return int(C.Py_BytesMain(argc, &argv[0]))
}

// SetPyModeCallbacks installs the Go callback function pointers used by the
// omnivm Python module in interpreter mode. Called from the main binary.
func SetPyModeCallbacks(initPtr, loadPtr, shutdownPtr, freePtr unsafe.Pointer) {
	C.omnivm_set_pymode_callbacks(
		C.omni_init_runtimes_fn(initPtr),
		C.omni_load_plugin_fn(loadPtr),
		C.omni_shutdown_fn(shutdownPtr),
	)
	// Also set the bridge free function so pymode can free Go-allocated strings
	C.omnivm_py_set_bridge_free(C.omni_free_fn(freePtr))
}

// MarkCPythonInitialized marks CPython as already initialized (because
// Py_BytesMain did it). This prevents double-init if the Go-hosted
// python.Runtime.Initialize() is called later.
func MarkCPythonInitialized() {
	cpythonInitialized = true
}
