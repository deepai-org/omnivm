// libomnivm — OmniVM as a C shared library (-buildmode=c-shared).
//
// This is the pip-installable variant of OmniVM. Python loads it via
// ctypes.CDLL("libomnivm.so") in omnivm.init_runtimes(), AFTER fork.
// The Go runtime starts fresh in each worker — no stale goroutines,
// no dead GC threads, no scheduler corruption.
//
// Build:
//
//	go build -buildmode=c-shared -o libomnivm.so ./cmd/libomnivm/
//
// Exports:
//
//	OmniInit(runtimes *C.char) *C.char
//	OmniCall(runtime, code *C.char) *C.char
//	OmniExec(runtime, code *C.char) *C.char
//	OmniLoadPlugin(runtime, path *C.char) *C.char
//	OmniShutdown()
//	OmniFree(ptr *C.char)
//
// All 5 runtimes are supported: Python (host — already running),
// JavaScript, Java (JVM), Ruby, and Go (via dlopen-based plugins).
//
// Go plugins: Since -buildmode=plugin is incompatible with a c-shared host,
// Go plugins must be built as c-shared libraries themselves
// (go build -buildmode=c-shared). They are loaded via dlopen/dlsym.
package main

/*
#include <stdlib.h>
#include <dlfcn.h>
#include <string.h>
#include <unistd.h>
#include <sys/syscall.h>
#include <stdint.h>

// Forward declarations of exported Go functions
extern char* OmniCall(char* runtime, char* code);
extern void OmniFree(char* ptr);

// Buffer bridge exports
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
} omni_buffer_t;
extern int OmniBufGet(char* name, omni_buffer_t* out);
extern int OmniBufSet(char* name, omni_buffer_t buf);
extern void OmniBufRelease(char* name);

// Typed value bridge
typedef struct {
    int64_t tag;
    union {
        int64_t  i;
        double   f;
        struct { char* ptr; int64_t len; } s;
        uint64_t ref;
    } v;
} omni_value_t;
extern omni_value_t OmniCallTyped(char* runtime, char* func_name,
                                   omni_value_t* args, int32_t nargs);

static void* get_omni_call_ptr() { return (void*)OmniCall; }
static void* get_omni_free_ptr() { return (void*)OmniFree; }
static void* get_omni_buf_get_ptr()     { return (void*)OmniBufGet; }
static void* get_omni_buf_set_ptr()     { return (void*)OmniBufSet; }
static void* get_omni_buf_release_ptr() { return (void*)OmniBufRelease; }
static void* get_omni_call_typed_ptr() { return (void*)OmniCallTyped; }

// Get the current OS thread ID (Linux-specific).
static long get_thread_id() { return syscall(SYS_gettid); }

// dlopen-based Go plugin loading (replaces plugin.Open for c-shared hosts).
// Each Go plugin is built as -buildmode=c-shared and exports C functions.
typedef char* (*go_plugin_func_s_s)(char*);  // func(string) string
typedef char* (*go_plugin_func_s)(void);     // func() string

static void* omni_dlopen(const char* path) {
    return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

static void* omni_dlsym(void* handle, const char* name) {
    return dlsym(handle, name);
}

static const char* omni_dlerror(void) {
    return dlerror();
}

static char* omni_call_plugin_s_s(void* fn, const char* arg) {
    return ((go_plugin_func_s_s)fn)((char*)arg);
}

static char* omni_call_plugin_s(void* fn) {
    return ((go_plugin_func_s)fn)();
}
*/
import "C"

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/engine"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/manifest"
	"github.com/omnivm/omnivm/pkg/polyglot"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
)

func init() {
	// Pin the main goroutine to the current OS thread — this becomes the
	// Golden Thread. In the prefork model, this is the Gunicorn worker
	// thread that called dlopen + OmniInit.
	runtime.LockOSThread()
}

// eng is the shared engine managing all runtimes.
var eng *engine.Engine
var manifestExecutor *manifest.Executor

// goPlugins maps plugin names to dlopen handles for c-shared Go plugins.
type goPlugin struct {
	handle unsafe.Pointer
	name   string
}

var goPlugins = make(map[string]*goPlugin)

// initialized tracks whether OmniInit has been called.
var initialized bool

//export OmniInit
func OmniInit(cList *C.char) *C.char {
	if initialized {
		return C.CString("ERR:already initialized")
	}

	list := C.GoString(cList)
	names := strings.Split(list, ",")

	eng = engine.New()
	eng.GoldenThreadID = int64(C.get_thread_id())
	arrow.SetGlobalStore(arrow.NewSharedStore())

	// Python is the host process — mark CPython as already initialized
	// so the python.Runtime wraps it instead of calling Py_Initialize.
	python.MarkCPythonInitialized()

	// Runtime creators — Python is special (host, no Initialize needed)
	creators := map[string]func() pkg.Runtime{
		"javascript": func() pkg.Runtime { return javascript.New() },
		"java":       func() pkg.Runtime { return jvm.New() },
		"ruby":       func() pkg.Runtime { return ruby.New() },
	}

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name == "go" || name == "python" {
			// "go" is handled via dlopen-based plugins
			// "python" is the host — already running
			continue
		}

		creator, ok := creators[name]
		if !ok {
			return C.CString("ERR:unknown runtime: " + name)
		}

		rt := creator()
		if err := rt.Initialize(); err != nil {
			return C.CString("ERR:" + name + ": " + err.Error())
		}
		eng.Runtimes[name] = rt
	}

	// Add Python as a runtime for cross-runtime bridge (wraps host CPython).
	// Initialize() detects cpythonInitialized and just sets r.initialized = true.
	pyRT := python.New()
	if err := pyRT.Initialize(); err != nil {
		return C.CString("ERR:python: " + err.Error())
	}
	eng.Runtimes["python"] = pyRT

	// Set up watchdog, bridge, dispatcher
	eng.SetupWatchdog()

	callPtr := uintptr(C.get_omni_call_ptr())
	freePtr := uintptr(C.get_omni_free_ptr())
	eng.SetupBridge(callPtr, freePtr)

	// Buffer bridge
	bufGetPtr := uintptr(C.get_omni_buf_get_ptr())
	bufSetPtr := uintptr(C.get_omni_buf_set_ptr())
	bufReleasePtr := uintptr(C.get_omni_buf_release_ptr())
	eng.SetupBufCallbacks(bufGetPtr, bufSetPtr, bufReleasePtr)

	// Typed call bridge
	typedPtr := uintptr(C.get_omni_call_typed_ptr())
	eng.SetupTypedCallback(typedPtr)
	pyRT.SetTypedCallback(typedPtr)

	// Go-backed typed functions
	polyglot.RegisterBuiltins()

	eng.ActivateForkGuard()
	// Do not start the background dispatcher in c-shared mode. CPython is the
	// host runtime here; pumping it from a Go-created background thread can
	// violate Python thread-state ownership while the caller is executing Java
	// or Ruby. Manifest execution and direct calls run on the calling Python
	// worker thread and pump async runtimes cooperatively where needed.

	initialized = true
	return C.CString("OK")
}

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
	if !initialized {
		return C.CString("ERR:not initialized — call OmniInit first")
	}

	rtName := C.GoString(cRuntime)
	code := C.GoString(cCode)

	if rtName == "__manifest" {
		if manifestExecutor == nil {
			return C.CString("ERR:manifest executor not active")
		}
		res, err := manifestExecutor.HandleCall(code)
		if err != nil {
			return C.CString("ERR:" + err.Error())
		}
		return C.CString(res)
	}

	// Go plugins use dlopen/dlsym (not the standard runtime interface)
	if rtName == "go" {
		return callGoPlugin(code)
	}

	threadID := int64(C.get_thread_id())
	val, err := eng.Call(rtName, code, threadID)
	if err != nil {
		return C.CString("ERR:" + err.Error())
	}
	return C.CString(val)
}

// callGoPlugin dispatches a "plugin.Func(arg)" call to a dlopen'd Go plugin.
func callGoPlugin(code string) *C.char {
	code = strings.TrimSpace(code)

	// Parse "pluginname.FuncName(args)"
	dot := strings.IndexByte(code, '.')
	if dot <= 0 {
		return C.CString("ERR:go plugin call must be 'plugin.Func(args)', got: " + code)
	}
	pluginName := code[:dot]
	rest := code[dot+1:]

	paren := strings.IndexByte(rest, '(')
	if paren <= 0 || code[len(code)-1] != ')' {
		return C.CString("ERR:go plugin call must be 'plugin.Func(args)', got: " + code)
	}
	funcName := rest[:paren]
	args := rest[paren+1 : len(rest)-1]

	// Strip surrounding quotes from single string argument
	args = strings.TrimSpace(args)
	if len(args) >= 2 && args[0] == '"' && args[len(args)-1] == '"' {
		args = args[1 : len(args)-1]
		args = strings.ReplaceAll(args, `\"`, `"`)
		args = strings.ReplaceAll(args, `\\`, `\`)
	}

	plug, ok := goPlugins[pluginName]
	if !ok {
		return C.CString("ERR:go plugin '" + pluginName + "' not loaded")
	}

	// Look up the function via dlsym
	cFuncName := C.CString(funcName)
	defer C.free(unsafe.Pointer(cFuncName))
	sym := C.omni_dlsym(plug.handle, cFuncName)
	if sym == nil {
		errMsg := C.GoString(C.omni_dlerror())
		return C.CString("ERR:" + pluginName + "." + funcName + ": " + errMsg)
	}

	// Call the function. Convention: func(char*) char* or func() char*
	var result *C.char
	if args != "" {
		cArgs := C.CString(args)
		defer C.free(unsafe.Pointer(cArgs))
		result = C.omni_call_plugin_s_s(sym, cArgs)
	} else {
		result = C.omni_call_plugin_s(sym)
	}

	if result == nil {
		return C.CString("")
	}
	// The result is malloc'd by the plugin's Go runtime — copy it and free
	goStr := C.GoString(result)
	C.free(unsafe.Pointer(result))
	return C.CString(goStr)
}

//export OmniExec
func OmniExec(cRuntime *C.char, cCode *C.char) *C.char {
	if !initialized {
		return C.CString("ERR:not initialized — call OmniInit first")
	}

	rtName := C.GoString(cRuntime)
	code := C.GoString(cCode)

	threadID := int64(C.get_thread_id())
	out, err := eng.Exec(rtName, code, threadID)
	if err != nil {
		return C.CString("ERR:" + err.Error())
	}
	return C.CString(out)
}

//export OmniRunManifestFile
func OmniRunManifestFile(cPath *C.char) *C.char {
	if !initialized {
		return C.CString("ERR:not initialized — call OmniInit first")
	}

	path := C.GoString(cPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return C.CString("ERR:read manifest: " + err.Error())
	}

	m, err := manifest.ParseManifest(data)
	if err != nil {
		return C.CString("ERR:parse manifest: " + err.Error())
	}

	executor := manifest.NewExecutor(eng.Runtimes)
	manifestExecutor = executor
	prevGoSourceFallback := manifest.UseGoSourceFallback
	manifest.UseGoSourceFallback = true
	defer func() {
		manifestExecutor = nil
		manifest.UseGoSourceFallback = prevGoSourceFallback
	}()

	if err := executor.Execute(m); err != nil {
		return C.CString("ERR:execute manifest: " + err.Error())
	}

	return C.CString("OK")
}

//export OmniLoadPlugin
func OmniLoadPlugin(cRuntime *C.char, cPath *C.char) *C.char {
	rtName := C.GoString(cRuntime)
	path := C.GoString(cPath)

	if rtName != "go" {
		return C.CString("ERR:load_plugin only supported for 'go' runtime")
	}

	cPath2 := C.CString(path)
	defer C.free(unsafe.Pointer(cPath2))

	handle := C.omni_dlopen(cPath2)
	if handle == nil {
		errMsg := C.GoString(C.omni_dlerror())
		return C.CString("ERR:dlopen " + path + ": " + errMsg)
	}

	// Derive plugin name from filename
	base := filepath.Base(path)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	// Strip "lib" prefix if present (libsessvalidator.so → sessvalidator)
	name = strings.TrimPrefix(name, "lib")

	goPlugins[name] = &goPlugin{handle: handle, name: name}

	return C.CString("OK")
}

//export OmniShutdown
func OmniShutdown() {
	if !initialized {
		return
	}
	eng.Shutdown()
	// Go plugins are c-shared libs — dlclose not needed, process exit cleans up
	initialized = false
}

//export OmniFree
func OmniFree(ptr *C.char) {
	if ptr != nil {
		C.free(unsafe.Pointer(ptr))
	}
}

//export OmniBufGet
func OmniBufGet(cName *C.char, out *C.omni_buffer_t) C.int {
	name := C.GoString(cName)
	var data unsafe.Pointer
	var length int64
	var dtype int32
	rc := arrow.BufGet(name, &data, &length, &dtype)
	if rc != 0 {
		return -1
	}
	out.data = data
	out.len = C.int64_t(length)
	out.dtype = C.int32_t(dtype)
	out.owned = 0
	return 0
}

//export OmniBufSet
func OmniBufSet(cName *C.char, buf C.omni_buffer_t) C.int {
	name := C.GoString(cName)
	return C.int(arrow.BufSet(name, buf.data, int64(buf.len), int32(buf.dtype)))
}

//export OmniBufRelease
func OmniBufRelease(cName *C.char) {
	arrow.BufRelease(C.GoString(cName))
}

//export OmniCallTyped
func OmniCallTyped(cRuntime *C.char, cFuncName *C.char, cArgs *C.omni_value_t, nargs C.int32_t) C.omni_value_t {
	rtName := C.GoString(cRuntime)
	funcName := C.GoString(cFuncName)

	n := int(nargs)
	goArgs := make([]polyglot.Value, n)
	if n > 0 && cArgs != nil {
		for i := 0; i < n; i++ {
			argPtr := unsafe.Pointer(uintptr(unsafe.Pointer(cArgs)) + uintptr(i)*polyglot.CValueSize)
			goArgs[i] = polyglot.FromCValueRaw(argPtr)
		}
	}

	result := eng.CallTyped(rtName, funcName, goArgs)
	var cv C.omni_value_t
	result.ToCValueRaw(unsafe.Pointer(&cv))
	return cv
}

// main is required for c-shared but never called.
func main() {}

// Ensure fmt is used (for error formatting in callGoPlugin).
var _ = fmt.Sprintf
