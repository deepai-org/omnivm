// libomnivm — OmniVM as a C shared library (-buildmode=c-shared).
//
// This is the pip-installable variant of OmniVM. Python loads it via
// ctypes.CDLL("libomnivm.so") in omnivm.init_runtimes(), AFTER fork.
// The Go runtime starts fresh in each worker — no stale goroutines,
// no dead GC threads, no scheduler corruption.
//
// Build:
//   go build -buildmode=c-shared -o libomnivm.so ./cmd/libomnivm/
//
// Exports:
//   OmniInit(runtimes *C.char) *C.char
//   OmniCall(runtime, code *C.char) *C.char
//   OmniExec(runtime, code *C.char) *C.char
//   OmniLoadPlugin(runtime, path *C.char) *C.char
//   OmniShutdown()
//   OmniFree(ptr *C.char)
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

// Forward declarations of exported Go functions
extern char* OmniCall(char* runtime, char* code);
extern void OmniFree(char* ptr);

static void* get_omni_call_ptr() { return (void*)OmniCall; }
static void* get_omni_free_ptr() { return (void*)OmniFree; }

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
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/engine"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
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

	eng.ActivateForkGuard()
	eng.StartDispatcher()

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

// main is required for c-shared but never called.
func main() {}

// Ensure fmt is used (for error formatting in callGoPlugin).
var _ = fmt.Sprintf
