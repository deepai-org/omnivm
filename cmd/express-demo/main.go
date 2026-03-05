// Express + Python Mixing Demo
//
// Starts an Express.js HTTP server inside OmniVM where route handlers
// call Python, Ruby, and Java through the cross-runtime bridge, then
// makes HTTP requests to prove it works.
//
// Usage: docker run --rm --entrypoint express-demo omnivm:latest
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
	"io"
	"net/http"
	"os"
	"runtime"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/dispatcher"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
)

func init() {
	runtime.LockOSThread()
}

var runtimes = make(map[string]pkg.Runtime)
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

	fmt.Println("=== OmniVM Express + Python Mixing Demo ===")
	fmt.Println()

	// Create and initialize runtimes
	pyRuntime := python.New()
	jsRuntime := javascript.New()
	rbRuntime := ruby.New()
	jvmRuntime := jvm.New()

	runtimes["python"] = pyRuntime
	runtimes["javascript"] = jsRuntime
	runtimes["ruby"] = rbRuntime
	runtimes["java"] = jvmRuntime

	fmt.Println("Initializing runtimes...")
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

	// Create dispatcher
	ctx, cancel := context.WithCancel(context.Background())
	disp := dispatcher.New()

	// Register JS pump so libuv processes Express I/O every tick
	disp.RegisterPumpCallback("javascript", jsRuntime.Pump)

	// Run the demo in a background goroutine; dispatcher runs on main thread
	go func() {
		defer cancel()

		// Step 1: Set up Express server via the Golden Thread
		fmt.Println("\nStarting Express server...")
		err := disp.RunOnMain(func() error {
			result := jsRuntime.Execute(`
				var express = require('express');
				var app = express();

				app.get('/', function(req, res) {
					var py = omnivm.call('python', '__import__("platform").python_version()');
					var rb = omnivm.call('ruby', 'RUBY_VERSION');
					var jv = omnivm.call('java', 'System.getProperty("java.version")');
					res.json({
						message: 'Hello from Express inside OmniVM!',
						python: py,
						ruby: rb,
						java: jv,
						engine: 'Node.js ' + process.version
					});
				});

				app.get('/compute', function(req, res) {
					var fib = omnivm.call('python',
						'def fib(n):\n' +
						'    a, b = 0, 1\n' +
						'    for _ in range(n): a, b = b, a+b\n' +
						'    return str(a)\n' +
						'fib(50)');
					var rb_rev = omnivm.call('ruby', '"OmniVM".reverse');
					res.json({
						fibonacci_50: fib,
						ruby_reverse: rb_rev
					});
				});

				app.listen(3000, function() {
					console.log('Express listening on :3000');
				});
			`)
			if result.Err != nil {
				return result.Err
			}
			if result.Output != "" {
				fmt.Print(result.Output)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start Express: %v\n", err)
			return
		}

		// Give the server a moment to bind
		time.Sleep(500 * time.Millisecond)

		// Step 2: Make HTTP requests
		fmt.Println("\n--- GET / ---")
		fetch("http://localhost:3000/")

		fmt.Println("\n--- GET /compute ---")
		fetch("http://localhost:3000/compute")

		// Step 3: Hit it a few more times to prove stability
		fmt.Println("\n--- 10 rapid requests ---")
		for i := 0; i < 10; i++ {
			resp, err := http.Get("http://localhost:3000/")
			if err != nil {
				fmt.Fprintf(os.Stderr, "  request %d: %v\n", i+1, err)
				continue
			}
			resp.Body.Close()
			fmt.Printf("  request %d: %s\n", i+1, resp.Status)
		}

		fmt.Println("\nDone!")
	}()

	// Run dispatcher on main goroutine (Golden Thread)
	disp.Run(ctx)
	disp.WaitForStop()

	// Shutdown
	fmt.Fprintln(os.Stderr, "\nShutting down...")
	for _, name := range []string{"ruby", "java", "javascript", "python"} {
		runtimes[name].Shutdown()
	}
}

func fetch(url string) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("Status: %s\nBody:   %s\n", resp.Status, string(body))
}
