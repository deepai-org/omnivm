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

static void* get_omni_call_ptr() { return (void*)OmniCall; }
static void* get_omni_free_ptr() { return (void*)OmniFree; }
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
	"github.com/omnivm/omnivm/pkg/dispatcher"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/manifest"
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

func main() {
	goldenThreadID = int64(C.get_thread_id())

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
		fmt.Fprintln(os.Stderr, "\nManifest execution complete. Pumping event loops (Ctrl+C to stop)...")

		// Keep running (pump loop stays active for servers, etc.)
		// Wait for signal to shut down
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "\nReceived signal, shutting down...")
			cancel()
		case <-ctx.Done():
		}
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
