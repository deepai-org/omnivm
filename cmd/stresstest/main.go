// OmniVM Cross-Runtime Stack Mixing Stress Test
//
// Demonstrates true cross-runtime callbacks where runtime A calls into Go,
// Go calls runtime B, all on a single call stack on the Golden Thread.
//
// Test cases:
//   1. Simple re-entry (Python → JS)
//   2. Deep chain (Go → Py → JS → Ruby)
//   3. Closure-like capture (Python → JS → Ruby)
//   4. Fan-out (4 goroutines via dispatcher)
//   5. Recursive cross-runtime fibonacci (Python + JS)
//   6. Error propagation depth test
//
// Usage: docker run --rm --entrypoint stresstest omnivm:latest
package main

/*
#include <stdlib.h>

// Forward declarations of exported Go functions (cgo drops const qualifiers)
extern char* OmniCall(char* runtime, char* code);
extern void OmniFree(char* ptr);

// Get function pointers to pass to runtimes
static void* get_omni_call_ptr() { return (void*)OmniCall; }
static void* get_omni_free_ptr() { return (void*)OmniFree; }
*/
import "C"

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
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

// Global runtime registry for the bridge callback
var runtimes = make(map[string]pkg.Runtime)

// Allocation counter for memory leak detection
var allocCount int64

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
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
	fmt.Println("=== OmniVM Cross-Runtime Stack Mixing Stress Test ===")
	fmt.Println()

	// Create runtimes
	pyRuntime := python.New()
	jsRuntime := javascript.New()
	rbRuntime := ruby.New()
	jvmRuntime := jvm.New()

	runtimes["python"] = pyRuntime
	runtimes["javascript"] = jsRuntime
	runtimes["ruby"] = rbRuntime
	runtimes["java"] = jvmRuntime

	// Initialize all runtimes
	fmt.Println("Initializing runtimes...")
	for _, name := range []string{"python", "javascript", "ruby", "java"} {
		rt := runtimes[name]
		if err := rt.Initialize(); err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] FAILED: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "  [%s] OK\n", name)
	}

	// Set bridge callback AFTER all runtimes are initialized, BEFORE any guest code
	callPtr := uintptr(C.get_omni_call_ptr())
	freePtr := uintptr(C.get_omni_free_ptr())
	for _, rt := range runtimes {
		rt.SetBridgeCallback(callPtr, freePtr)
	}
	fmt.Println("Bridge callbacks installed.")
	fmt.Println()

	passed := 0
	failed := 0

	run := func(name string, fn func() error) {
		fmt.Printf("[TEST] %s... ", name)
		if err := fn(); err != nil {
			fmt.Printf("FAILED: %v\n", err)
			failed++
		} else {
			fmt.Println("PASSED")
			passed++
		}
	}

	// Reset allocation counter
	atomic.StoreInt64(&allocCount, 0)

	// Test 1: Simple re-entry
	run("Simple re-entry (Python calls JS)", func() error {
		result := pyRuntime.Eval(`omnivm.call("javascript", "2 + 2")`)
		if result.Err != nil {
			return result.Err
		}
		val := fmt.Sprintf("%v", result.Value)
		if val != "4" {
			return fmt.Errorf("expected '4', got %q", val)
		}
		return nil
	})

	// Test 2: Deep chain
	run("Deep chain (Py → JS → Ruby)", func() error {
		// Python starts with 10, asks JS to add 20, JS asks Ruby to add 30
		result := pyRuntime.Eval(`int(omnivm.call("javascript", "parseInt(omnivm.call('ruby', '30 + 20')) + 10"))`)
		if result.Err != nil {
			return result.Err
		}
		val := fmt.Sprintf("%v", result.Value)
		if val != "60" {
			return fmt.Errorf("expected '60', got %q", val)
		}
		return nil
	})

	// Test 3: Closure-like capture
	run("Closure-like capture (Py → JS → Ruby)", func() error {
		result := pyRuntime.Eval(`
base = 100
doubled = omnivm.call("javascript", str(base) + " * 2")
int(omnivm.call("ruby", doubled + " * 3"))
`)
		if result.Err != nil {
			return result.Err
		}
		val := fmt.Sprintf("%v", result.Value)
		if val != "600" {
			return fmt.Errorf("expected '600' (100*2*3), got %q", val)
		}
		return nil
	})

	// Test 4: Fan-out
	// The dispatcher must run on the main goroutine (locked to the main OS thread)
	// so runtime calls happen on the correct thread.
	run("Fan-out (4 goroutines via dispatcher)", func() error {
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

		var mu sync.Mutex
		var order []string
		var wg sync.WaitGroup

		tasks := []struct {
			name string
			rt   pkg.Runtime
			code string
		}{
			{"python", pyRuntime, "1 + 1"},
			{"javascript", jsRuntime, "2 + 2"},
			{"ruby", rbRuntime, "3 + 3"},
			{"python2", pyRuntime, "4 + 4"},
		}

		for _, t := range tasks {
			wg.Add(1)
			t := t
			go func() {
				defer wg.Done()
				disp.RunOnMain(func() error {
					result := t.rt.Eval(t.code)
					mu.Lock()
					order = append(order, t.name)
					mu.Unlock()
					if result.Err != nil {
						return result.Err
					}
					return nil
				})
			}()
		}

		// Wait in background goroutine, then cancel dispatcher
		done := make(chan struct{})
		go func() {
			wg.Wait()
			cancel()
			close(done)
		}()

		// Run dispatcher on main goroutine (main OS thread)
		disp.Run(ctx)
		<-done

		if len(order) != 4 {
			return fmt.Errorf("expected 4 tasks processed, got %d: %v", len(order), order)
		}
		fmt.Printf("(order: %s) ", strings.Join(order, "→"))
		return nil
	})

	// Test 5: Recursive cross-runtime fibonacci
	run("Recursive cross-runtime fibonacci(15)", func() error {
		start := time.Now()

		// Define fib in Python: even-indexed calls go through JS round-trip
		setupResult := pyRuntime.Execute(`
import omnivm
def fib(n):
    n = int(n)
    if n <= 1:
        return n
    if n % 2 == 0:
        a = int(omnivm.call("javascript", "parseInt(omnivm.call('python', 'fib(" + str(n-1) + ")'))"))
    else:
        a = fib(n - 1)
    b = fib(n - 2)
    return a + b
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		result := pyRuntime.Eval("fib(15)")
		elapsed := time.Since(start)

		if result.Err != nil {
			return result.Err
		}
		val := fmt.Sprintf("%v", result.Value)
		if val != "610" {
			return fmt.Errorf("fib(15) expected '610', got %q", val)
		}

		if elapsed > 5*time.Second {
			return fmt.Errorf("took %v (>5s timeout — possible deadlock)", elapsed)
		}
		fmt.Printf("(=%s, %v) ", val, elapsed.Round(time.Millisecond))
		return nil
	})

	// Test 6: Error propagation depth test
	// NOTE: We use JS throw (not Ruby raise) for deep error propagation because
	// Ruby's rb_exc_raise triggers SIGSEGV internally on ARM64, and the JVM's
	// signal handler (even with libjsig chaining) interferes with it.
	run("Error propagation (Py → JS throws)", func() error {
		result := pyRuntime.Eval(`
try:
    _r = omnivm.call("javascript", "throw new Error('deep error from JS')")
except RuntimeError as e:
    _r = str(e)
_r
`)
		if result.Err != nil {
			// If the error propagated as an error result, check the message
			if strings.Contains(result.Err.Error(), "deep error from JS") {
				return nil // Error propagated correctly
			}
			return fmt.Errorf("error propagated but wrong message: %v", result.Err)
		}
		val := fmt.Sprintf("%v", result.Value)
		if !strings.Contains(val, "deep error from JS") {
			return fmt.Errorf("expected error message containing 'deep error from JS', got %q", val)
		}
		return nil
	})

	// Check allocation counter
	fmt.Println()
	leaks := atomic.LoadInt64(&allocCount)
	if leaks != 0 {
		fmt.Printf("[WARN] Memory leak detected: %d unfreed C.CString allocations\n", leaks)
	} else {
		fmt.Println("[OK] No memory leaks detected (allocation counter = 0)")
	}

	// Summary
	fmt.Println()
	fmt.Printf("Results: %d passed, %d failed out of %d tests\n", passed, failed, passed+failed)

	// Flush stdout before shutdown messages
	os.Stdout.Sync()

	// Shutdown (LIFO)
	fmt.Fprintln(os.Stderr, "\nShutting down...")
	for _, name := range []string{"ruby", "java", "javascript", "python"} {
		runtimes[name].Shutdown()
	}

	if failed > 0 {
		os.Exit(1)
	}
}
