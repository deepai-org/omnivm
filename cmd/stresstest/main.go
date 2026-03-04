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

	// Test 7: Python generator consumed cross-runtime
	// Generator frames live on the heap. The global reference keeps the PyObject
	// alive across 100 round-trips through Go → JS → Ruby C boundary crossings.
	run("Python generator consumed cross-runtime (100 iterations)", func() error {
		// Step 1: Create generator with finally block for cleanup verification
		setupResult := pyRuntime.Execute(`
_t7_cleanup = False

def _t7_counting_gen():
    global _t7_cleanup
    try:
        i = 1
        while True:
            yield i
            i += 1
    finally:
        _t7_cleanup = True

_t7_gen = _t7_counting_gen()
`)
		if setupResult.Err != nil {
			return fmt.Errorf("generator setup: %v", setupResult.Err)
		}

		// Step 2: Initialize Ruby accumulator
		rbSetup := rbRuntime.Eval("$_t7_acc = 0")
		if rbSetup.Err != nil {
			return fmt.Errorf("ruby accumulator setup: %v", rbSetup.Err)
		}

		// Step 3: Loop 100 times: Python next(gen) → JS multiply by 2 → Ruby accumulate
		for i := 0; i < 100; i++ {
			pyResult := pyRuntime.Eval("next(_t7_gen)")
			if pyResult.Err != nil {
				return fmt.Errorf("iteration %d: python next(): %v", i, pyResult.Err)
			}
			pyVal := fmt.Sprintf("%v", pyResult.Value)

			jsResult := jsRuntime.Eval(pyVal + " * 2")
			if jsResult.Err != nil {
				return fmt.Errorf("iteration %d: js multiply: %v", i, jsResult.Err)
			}
			jsVal := fmt.Sprintf("%v", jsResult.Value)

			rbResult := rbRuntime.Eval("$_t7_acc += " + jsVal)
			if rbResult.Err != nil {
				return fmt.Errorf("iteration %d: ruby accumulate: %v", i, rbResult.Err)
			}
		}

		// Step 4: Verify accumulated result = 2 * sum(1..100) = 2 * 5050 = 10100
		finalResult := rbRuntime.Eval("$_t7_acc")
		if finalResult.Err != nil {
			return fmt.Errorf("ruby final read: %v", finalResult.Err)
		}
		if fmt.Sprintf("%v", finalResult.Value) != "10100" {
			return fmt.Errorf("expected '10100', got %q", finalResult.Value)
		}

		// Step 5: Close generator and verify finally block ran
		pyRuntime.Execute("_t7_gen.close()")
		cleanupResult := pyRuntime.Eval("_t7_cleanup")
		if cleanupResult.Err != nil {
			return fmt.Errorf("cleanup check: %v", cleanupResult.Err)
		}
		if fmt.Sprintf("%v", cleanupResult.Value) != "True" {
			return fmt.Errorf("expected cleanup flag True, got %q", cleanupResult.Value)
		}

		return nil
	})

	// Test 8: Cross-runtime async with dispatcher pumping
	// Python asyncio tasks are driven by dispatcher pump callbacks. The pump
	// manually steps the event loop. task_c does two awaits, forcing multiple
	// pump cycles before all tasks complete.
	run("Cross-runtime async with dispatcher pumping", func() error {
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

		// Register pump callback that manually steps asyncio event loop
		disp.RegisterPumpCallback("python_async", func() {
			pyRuntime.Execute(`
try:
    _t8_loop.stop()
    _t8_loop.run_forever()
except:
    pass
`)
		})
		defer disp.UnregisterPumpCallback("python_async")

		var testErr error
		pollCount := 0

		go func() {
			defer cancel()

			// Phase 1: Setup async tasks on Golden Thread
			err := disp.RunOnMain(func() error {
				result := pyRuntime.Execute(`
import asyncio

_t8_results = []

async def _t8_task_a():
    val = omnivm.call("javascript", "7 * 6")
    _t8_results.append("task_a:" + val)

async def _t8_task_b():
    val = omnivm.call("ruby", "11 * 4")
    _t8_results.append("task_b:" + val)

async def _t8_task_c():
    await asyncio.sleep(0)
    await asyncio.sleep(0)
    _t8_results.append("task_c:done")

_t8_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_t8_loop)
_t8_loop.create_task(_t8_task_a())
_t8_loop.create_task(_t8_task_b())
_t8_loop.create_task(_t8_task_c())
`)
				return result.Err
			})
			if err != nil {
				testErr = fmt.Errorf("setup: %v", err)
				return
			}

			// Phase 2: Poll for completion (pump fires between RunOnMain calls)
			for i := 0; i < 100; i++ {
				time.Sleep(5 * time.Millisecond)
				var count string
				disp.RunOnMain(func() error {
					r := pyRuntime.Eval("len(_t8_results)")
					if r.Err == nil {
						count = fmt.Sprintf("%v", r.Value)
					}
					return nil
				})
				pollCount++
				if count == "3" {
					break
				}
				if i == 99 {
					testErr = fmt.Errorf("tasks did not complete after 100 pump cycles (count=%s)", count)
					return
				}
			}

			// Phase 3: Verify results
			disp.RunOnMain(func() error {
				r := pyRuntime.Eval("sorted(_t8_results)")
				if r.Err != nil {
					testErr = fmt.Errorf("verify: %v", r.Err)
					return nil
				}
				val := fmt.Sprintf("%v", r.Value)
				expected := "['task_a:42', 'task_b:44', 'task_c:done']"
				if val != expected {
					testErr = fmt.Errorf("expected %q, got %q", expected, val)
				}
				return nil
			})

			// Cleanup
			disp.RunOnMain(func() error {
				pyRuntime.Execute("_t8_loop.close()")
				return nil
			})
		}()

		disp.Run(ctx)
		fmt.Printf("(%d pump cycles) ", pollCount)
		return testErr
	})

	// Test 9: Re-entrant async — callback during pump calls back into Python
	// During loop.run_forever(), an async callback calls JS, which calls back
	// into Python. This tests CPython re-entrancy: PyRun_SimpleString (outer)
	// → asyncio → omnivm.call("javascript",...) → JS → omnivm.call("python",...)
	// → PyRun_String (inner). The inner expression is a read-only variable
	// lookup that doesn't mutate __main__'s dict.
	run("Re-entrant async: Py pump -> JS -> back into Py", func() error {
		// Phase 1: Set up the inner value (read-only access from re-entrant call)
		setupResult := pyRuntime.Execute(`
import asyncio

_t9_inner_value = "SENTINEL_42"
_t9_result = None

async def _t9_reentrant_task():
    global _t9_result
    # Python -> JS -> Python (re-entry during event loop)
    val = omnivm.call("javascript",
        "omnivm.call('python', '_t9_inner_value')")
    _t9_result = "reentrant:" + val

_t9_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_t9_loop)
_t9_loop.create_task(_t9_reentrant_task())
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Phase 2: Drive the event loop (triggers the re-entrant callback chain)
		driveResult := pyRuntime.Execute(`
_t9_loop.stop()
_t9_loop.run_forever()
`)
		if driveResult.Err != nil {
			return fmt.Errorf("drive loop: %v", driveResult.Err)
		}

		// Phase 3: Verify the re-entrant result
		verifyResult := pyRuntime.Eval("_t9_result")
		if verifyResult.Err != nil {
			return fmt.Errorf("verify: %v", verifyResult.Err)
		}
		val := fmt.Sprintf("%v", verifyResult.Value)
		if val != "reentrant:SENTINEL_42" {
			return fmt.Errorf("expected 'reentrant:SENTINEL_42', got %q", val)
		}

		// Phase 4: Prove event loop survived re-entry by running a new task
		// that calls a DIFFERENT runtime (Ruby, not JS) to exercise both
		// the event loop machinery and a different bridge path
		postResult := pyRuntime.Execute(`
_t9_post_result = None

async def _t9_post_task():
    global _t9_post_result
    val = omnivm.call("ruby", "7 * 7")
    _t9_post_result = "post:" + val

_t9_loop.create_task(_t9_post_task())
_t9_loop.stop()
_t9_loop.run_forever()
`)
		if postResult.Err != nil {
			return fmt.Errorf("post re-entry task: %v", postResult.Err)
		}

		postVerify := pyRuntime.Eval("_t9_post_result")
		if postVerify.Err != nil {
			return fmt.Errorf("post verify: %v", postVerify.Err)
		}
		postVal := fmt.Sprintf("%v", postVerify.Value)
		if postVal != "post:49" {
			return fmt.Errorf("expected 'post:49', got %q", postVal)
		}

		// Cleanup
		pyRuntime.Execute("_t9_loop.close()")

		return nil
	})

	// Test 10: Cross-runtime exception through suspended coroutine
	// gen.throw() injects an exception into a suspended generator frame.
	// The generator has a while True loop with try/except so it survives
	// the throw and can be advanced again.
	run("Exception through suspended Python generator", func() error {
		// Phase 1: Create generator with try/except structure
		setupResult := pyRuntime.Execute(`
def _t10_gen_func():
    while True:
        try:
            value = yield "ready"
        except RuntimeError as e:
            yield "caught:" + str(e)
        except GeneratorExit:
            return

_t10_gen = _t10_gen_func()
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Phase 2: Advance to first yield
		advResult := pyRuntime.Eval("next(_t10_gen)")
		if advResult.Err != nil {
			return fmt.Errorf("advance: %v", advResult.Err)
		}
		if fmt.Sprintf("%v", advResult.Value) != "ready" {
			return fmt.Errorf("expected 'ready', got %q", advResult.Value)
		}

		// Phase 3: Throw exception from Go
		throwResult := pyRuntime.Eval(`_t10_gen.throw(RuntimeError("injected from Go"))`)
		if throwResult.Err != nil {
			return fmt.Errorf("throw from Go: %v", throwResult.Err)
		}
		throwVal := fmt.Sprintf("%v", throwResult.Value)
		if throwVal != "caught:injected from Go" {
			return fmt.Errorf("expected 'caught:injected from Go', got %q", throwVal)
		}

		// Phase 4: Generator should loop back to yield "ready"
		nextResult := pyRuntime.Eval("next(_t10_gen)")
		if nextResult.Err != nil {
			return fmt.Errorf("next after throw: %v", nextResult.Err)
		}
		if fmt.Sprintf("%v", nextResult.Value) != "ready" {
			return fmt.Errorf("expected 'ready' after throw, got %q", nextResult.Value)
		}

		// Phase 5: Throw exception triggered cross-runtime via JS → bridge → Python
		jsThrowResult := jsRuntime.Eval(
			`omnivm.call("python", "_t10_gen.throw(RuntimeError('from JS'))")`)
		if jsThrowResult.Err != nil {
			return fmt.Errorf("throw from JS: %v", jsThrowResult.Err)
		}
		jsThrowVal := fmt.Sprintf("%v", jsThrowResult.Value)
		if jsThrowVal != "caught:from JS" {
			return fmt.Errorf("expected 'caught:from JS', got %q", jsThrowVal)
		}

		// Phase 6: Verify generator still alive
		aliveResult := pyRuntime.Eval("next(_t10_gen)")
		if aliveResult.Err != nil {
			return fmt.Errorf("generator died after cross-runtime throw: %v", aliveResult.Err)
		}
		if fmt.Sprintf("%v", aliveResult.Value) != "ready" {
			return fmt.Errorf("expected 'ready' after JS throw, got %q", aliveResult.Value)
		}

		// Phase 7: Clean shutdown via GeneratorExit
		pyRuntime.Execute("_t10_gen.close()")

		return nil
	})

	// Test 11: Object pinning and GC interaction with weakref verification
	// Objects stored in a dict (handle table pattern) must survive gc.collect().
	// After unpinning, weakref.ref() must return None — proving objects were
	// actually reclaimed by GC, not just removed from the dict but leaked
	// through a C bridge reference.
	run("Object pinning and GC interaction", func() error {
		// Phase 1: Create handle table with weakref tracking
		setupResult := pyRuntime.Execute(`
import gc
import weakref

class _T11Obj:
    def __init__(self, value):
        self.value = value

_t11_handles = {}
_t11_weak_refs = {}
_t11_next_id = 0

def _t11_pin(value):
    global _t11_next_id
    obj = _T11Obj(value)
    hid = _t11_next_id
    _t11_next_id += 1
    _t11_handles[hid] = obj
    _t11_weak_refs[hid] = weakref.ref(obj)
    return hid

def _t11_get(hid):
    return _t11_handles[int(hid)].value

def _t11_unpin(hid):
    del _t11_handles[int(hid)]
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Phase 2: Pin 10 objects
		ids := make([]string, 10)
		for i := 0; i < 10; i++ {
			result := pyRuntime.Eval(fmt.Sprintf("_t11_pin('value_%d')", i))
			if result.Err != nil {
				return fmt.Errorf("pin %d: %v", i, result.Err)
			}
			ids[i] = fmt.Sprintf("%v", result.Value)
		}

		// Phase 3: Cross-runtime round-trip (JS → Ruby → Python → returns value)
		for i := 0; i < 10; i++ {
			jsResult := jsRuntime.Eval(fmt.Sprintf(
				`omnivm.call("ruby", "OmniVM.call('python', '_t11_get(%s)')")`,
				ids[i]))
			if jsResult.Err != nil {
				return fmt.Errorf("cross-runtime lookup %d: %v", i, jsResult.Err)
			}
			expected := fmt.Sprintf("value_%d", i)
			if fmt.Sprintf("%v", jsResult.Value) != expected {
				return fmt.Errorf("ID %s: expected %q, got %q", ids[i], expected, jsResult.Value)
			}
		}

		// Phase 4: Force GC, verify objects survive (rooted in _t11_handles)
		pyRuntime.Execute("gc.collect(); gc.collect(); gc.collect()")

		for i := 0; i < 10; i++ {
			result := pyRuntime.Eval(fmt.Sprintf("_t11_get(%s)", ids[i]))
			if result.Err != nil {
				return fmt.Errorf("post-GC access %d: %v (object collected!)", i, result.Err)
			}
			expected := fmt.Sprintf("value_%d", i)
			if fmt.Sprintf("%v", result.Value) != expected {
				return fmt.Errorf("post-GC value %d: expected %q, got %q", i, expected, result.Value)
			}
		}

		// Phase 5: Unpin all objects
		for i := 0; i < 10; i++ {
			unpinResult := pyRuntime.Execute(fmt.Sprintf("_t11_unpin(%s)", ids[i]))
			if unpinResult.Err != nil {
				return fmt.Errorf("unpin %d: %v", i, unpinResult.Err)
			}
		}

		// Phase 6: Force GC to reclaim unpinned objects
		pyRuntime.Execute("gc.collect(); gc.collect(); gc.collect()")

		// Phase 7: Verify retrieval fails with KeyError (not segfault)
		for i := 0; i < 10; i++ {
			result := pyRuntime.Eval(fmt.Sprintf("_t11_get(%s)", ids[i]))
			if result.Err == nil {
				return fmt.Errorf("expected error for unpinned ID %s, got value: %v",
					ids[i], result.Value)
			}
		}

		// Phase 8: Weakref check — verify objects were actually reclaimed
		for i := 0; i < 10; i++ {
			result := pyRuntime.Eval(fmt.Sprintf("_t11_weak_refs[%s]() is None", ids[i]))
			if result.Err != nil {
				return fmt.Errorf("weakref check %d: %v", i, result.Err)
			}
			if fmt.Sprintf("%v", result.Value) != "True" {
				return fmt.Errorf("weakref for ID %s still alive — object leaked through C bridge", ids[i])
			}
		}

		// Phase 9: Verify handle table is empty
		countResult := pyRuntime.Eval("len(_t11_handles)")
		if countResult.Err != nil {
			return fmt.Errorf("count: %v", countResult.Err)
		}
		if fmt.Sprintf("%v", countResult.Value) != "0" {
			return fmt.Errorf("expected 0 handles, got %v", countResult.Value)
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
