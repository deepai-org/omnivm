// Manifest Runner — executes OmniVM JSON dispatch manifests.
//
// A manifest is a structured IR where a PolyScript compiler emits ops that
// OmniVM dispatches to Python, JavaScript, Ruby, Java, and Go runtimes.
//
// Usage: docker run --rm --entrypoint manifest-runner omnivm manifest.json
package main

/*
#include <stdlib.h>
#include <unistd.h>
#include <sys/syscall.h>

extern char* OmniCall(char* runtime, char* code);
extern void OmniFree(char* ptr);

// Buffer bridge exports
#include <stdint.h>
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
    int8_t  read_only;
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
static void* get_omni_call_typed_ptr()  { return (void*)OmniCallTyped; }
static long get_thread_id() { return syscall(SYS_gettid); }
*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/dispatcher"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/manifest"
	"github.com/omnivm/omnivm/pkg/polyglot"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
)

func init() {
	runtime.LockOSThread()
}

var runtimes = make(map[string]pkg.Runtime)
var executor *manifest.Executor
var allocCount int64
var goldenThreadID int64

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
	currentTid := int64(C.get_thread_id())
	if currentTid != goldenThreadID {
		msg := fmt.Sprintf("ERR:omnivm.call from non-Golden Thread (tid=%d, expected=%d)",
			currentTid, goldenThreadID)
		result := C.CString(msg)
		atomic.AddInt64(&allocCount, 1)
		return result
	}

	rtName := C.GoString(cRuntime)
	code := C.GoString(cCode)

	// Route __manifest calls to the executor
	if rtName == "__manifest" {
		res, err := executor.HandleCall(code)
		if err != nil {
			result := C.CString("ERR:" + err.Error())
			atomic.AddInt64(&allocCount, 1)
			return result
		}
		result := C.CString(res)
		atomic.AddInt64(&allocCount, 1)
		return result
	}

	rt, ok := runtimes[rtName]
	if !ok {
		result := C.CString("ERR:unknown runtime: " + rtName)
		atomic.AddInt64(&allocCount, 1)
		return result
	}

	evalResult := rt.Eval(code)
	if evalResult.Err != nil {
		result := C.CString("ERR:" + evalResult.Err.Error())
		atomic.AddInt64(&allocCount, 1)
		return result
	}

	var val string
	if evalResult.Value != nil {
		val = fmt.Sprintf("%v", evalResult.Value)
	} else {
		val = evalResult.Output
	}

	result := C.CString(val)
	atomic.AddInt64(&allocCount, 1)
	return result
}

//export OmniFree
func OmniFree(ptr *C.char) {
	if ptr != nil {
		C.free(unsafe.Pointer(ptr))
		atomic.AddInt64(&allocCount, -1)
	}
}

//export OmniBufGet
func OmniBufGet(cName *C.char, out *C.omni_buffer_t) C.int {
	name := C.GoString(cName)
	var data unsafe.Pointer
	var length int64
	var dtype int32
	var readOnly bool
	rc := arrow.BufGet(name, &data, &length, &dtype, &readOnly)
	if rc != 0 {
		return -1
	}
	out.data = data
	out.len = C.int64_t(length)
	out.dtype = C.int32_t(dtype)
	out.owned = 0
	if readOnly {
		out.read_only = 1
	} else {
		out.read_only = 0
	}
	return 0
}

//export OmniBufSet
func OmniBufSet(cName *C.char, buf C.omni_buffer_t) C.int {
	name := C.GoString(cName)
	return C.int(arrow.BufSet(name, buf.data, int64(buf.len), int32(buf.dtype), buf.read_only != 0))
}

//export OmniBufRelease
func OmniBufRelease(cName *C.char) {
	arrow.BufRelease(C.GoString(cName))
	if int64(C.get_thread_id()) == goldenThreadID {
		arrow.GlobalStore().DrainDeferred()
	}
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

	// Try typed registry first, fall back to eval
	result := polyglot.GlobalRegistry.Call(rtName, funcName, goArgs)
	if result.IsError() {
		rt, ok := runtimes[rtName]
		if !ok {
			var cv C.omni_value_t
			polyglot.Error("unknown runtime: " + rtName).ToCValueRaw(unsafe.Pointer(&cv))
			return cv
		}
		code := funcName + "("
		for i, arg := range goArgs {
			if i > 0 {
				code += ", "
			}
			code += arg.ToGoString()
		}
		code += ")"

		// Use typed eval if the runtime supports it
		type typedEvaler interface {
			EvalTyped(code string) polyglot.Value
		}
		if te, ok := rt.(typedEvaler); ok {
			result = te.EvalTyped(code)
			var cv C.omni_value_t
			result.ToCValueRaw(unsafe.Pointer(&cv))
			return cv
		}

		evalResult := rt.Eval(code)
		if evalResult.Err != nil {
			var cv C.omni_value_t
			polyglot.Error(evalResult.Err.Error()).ToCValueRaw(unsafe.Pointer(&cv))
			return cv
		}
		s := ""
		if evalResult.Value != nil {
			s = fmt.Sprintf("%v", evalResult.Value)
		} else {
			s = evalResult.Output
		}
		var cv C.omni_value_t
		polyglot.String(s).ToCValueRaw(unsafe.Pointer(&cv))
		return cv
	}
	var cv C.omni_value_t
	result.ToCValueRaw(unsafe.Pointer(&cv))
	return cv
}

func main() {
	goldenThreadID = int64(C.get_thread_id())
	arrow.SetGlobalStore(arrow.NewSharedStore())
	polyglot.RegisterBuiltins()

	// Parse args: manifest-runner <manifest.json>
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: manifest-runner <manifest.json>\n")
		os.Exit(1)
	}
	manifestPath := os.Args[1]

	// Read and parse manifest
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading manifest: %v\n", err)
		os.Exit(1)
	}

	m, err := manifest.ParseManifest(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing manifest: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "=== OmniVM Manifest Runner (v%d) ===\n", m.Version)

	// Create and initialize runtimes
	pyRuntime := python.New()
	jsRuntime := javascript.New()
	rbRuntime := ruby.New()
	jvmRuntime := jvm.New()

	runtimes["python"] = pyRuntime
	runtimes["javascript"] = jsRuntime
	runtimes["ruby"] = rbRuntime
	runtimes["java"] = jvmRuntime

	fmt.Fprintln(os.Stderr, "Initializing runtimes...")
	for _, name := range []string{"python", "javascript", "ruby", "java"} {
		if err := runtimes[name].Initialize(); err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] FAILED: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "  [%s] OK\n", name)
	}

	// Install bridge callbacks
	callPtr := uintptr(C.get_omni_call_ptr())
	freePtr := uintptr(C.get_omni_free_ptr())
	for _, rt := range runtimes {
		rt.SetBridgeCallback(callPtr, freePtr)
	}

	// Install buffer bridge callbacks
	bufGetPtr := uintptr(C.get_omni_buf_get_ptr())
	bufSetPtr := uintptr(C.get_omni_buf_set_ptr())
	bufReleasePtr := uintptr(C.get_omni_buf_release_ptr())
	pyRuntime.SetBufCallbacks(bufGetPtr, bufSetPtr, bufReleasePtr)
	jsRuntime.SetBufCallbacks(bufGetPtr, bufSetPtr, bufReleasePtr)
	rbRuntime.SetBufCallbacks(bufGetPtr, bufSetPtr, bufReleasePtr)
	jvmRuntime.SetBufCallbacks(bufGetPtr, bufSetPtr, bufReleasePtr)

	// Install typed call bridge
	typedPtr := uintptr(C.get_omni_call_typed_ptr())
	pyRuntime.SetTypedCallback(typedPtr)
	jsRuntime.SetTypedCallback(typedPtr)
	rbRuntime.SetTypedCallback(typedPtr)
	jvmRuntime.SetTypedCallback(typedPtr)

	// Create executor
	executor = manifest.NewExecutor(runtimes)

	// Create dispatcher
	ctx, cancel := context.WithCancel(context.Background())
	disp := dispatcher.New()
	disp.RegisterPumpCallback("javascript", jsRuntime.Pump)
	disp.RegisterPumpCallback("python", pyRuntime.Pump)

	// Handle SIGINT/SIGTERM for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Run manifest in background goroutine; dispatcher runs on Golden Thread
	go func() {
		err := disp.RunOnMain(func() error {
			return executor.Execute(m)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nManifest execution error: %v\n", err)
			cancel()
			return
		}
		fmt.Fprintln(os.Stderr, "\nManifest execution complete.")
		cancel()
	}()

	// Run dispatcher on main goroutine (Golden Thread)
	disp.Run(ctx)
	disp.WaitForStop()

	// Flush stdout before shutdown messages
	os.Stdout.Sync()

	// Shutdown in reverse order
	fmt.Fprintln(os.Stderr, "Shutting down...")
	for _, name := range []string{"ruby", "java", "javascript", "python"} {
		runtimes[name].Shutdown()
	}
}
