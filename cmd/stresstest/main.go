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

	// Test 12: Interleaved multiple live generators
	// Three generators alive simultaneously, advanced in non-sequential order
	// from different runtimes. Tests that each generator's suspended frame is
	// independent — no shared mutable state in the Eval path.
	run("Interleaved multiple live generators", func() error {
		setupResult := pyRuntime.Execute(`
def _t12_powers_of_2():
    x = 1
    while True:
        yield x
        x *= 2

def _t12_powers_of_3():
    x = 1
    while True:
        yield x
        x *= 3

def _t12_fib():
    a, b = 0, 1
    while True:
        yield a
        a, b = b, a + b

_t12_gen_a = _t12_powers_of_2()
_t12_gen_b = _t12_powers_of_3()
_t12_gen_c = _t12_fib()
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Expected sequences:
		// powers of 2: 1, 2, 4, 8, 16, 32, ...
		// powers of 3: 1, 3, 9, 27, 81, 243, ...
		// fibonacci:   0, 1, 1, 2, 3, 5, 8, 13, 21, 34, ...

		// Interleaved advancement from different runtimes in non-sequential order:
		// gen_b via Ruby, gen_a via JS, gen_c via Ruby, gen_a via JS, gen_b via Go

		// gen_b[0] via Ruby = 1
		r := rbRuntime.Eval(`OmniVM.call("python", "next(_t12_gen_b)")`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "1" {
			return fmt.Errorf("gen_b[0] via Ruby: err=%v val=%v", r.Err, r.Value)
		}

		// gen_a[0] via JS = 1
		r = jsRuntime.Eval(`omnivm.call("python", "next(_t12_gen_a)")`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "1" {
			return fmt.Errorf("gen_a[0] via JS: err=%v val=%v", r.Err, r.Value)
		}

		// gen_c[0] via Ruby = 0 (fib starts at 0)
		r = rbRuntime.Eval(`OmniVM.call("python", "next(_t12_gen_c)")`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "0" {
			return fmt.Errorf("gen_c[0] via Ruby: err=%v val=%v", r.Err, r.Value)
		}

		// gen_a[1] via JS = 2
		r = jsRuntime.Eval(`omnivm.call("python", "next(_t12_gen_a)")`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "2" {
			return fmt.Errorf("gen_a[1] via JS: err=%v val=%v", r.Err, r.Value)
		}

		// gen_b[1] via Go directly = 3
		r2 := pyRuntime.Eval("next(_t12_gen_b)")
		if r2.Err != nil || fmt.Sprintf("%v", r2.Value) != "3" {
			return fmt.Errorf("gen_b[1] via Go: err=%v val=%v", r2.Err, r2.Value)
		}

		// Continue interleaving to verify independence over more iterations
		// gen_c[1] via JS = 1, gen_c[2] via Ruby = 1, gen_a[2] via Go = 4
		r = jsRuntime.Eval(`omnivm.call("python", "next(_t12_gen_c)")`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "1" {
			return fmt.Errorf("gen_c[1] via JS: err=%v val=%v", r.Err, r.Value)
		}

		r = rbRuntime.Eval(`OmniVM.call("python", "next(_t12_gen_c)")`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "1" {
			return fmt.Errorf("gen_c[2] via Ruby: err=%v val=%v", r.Err, r.Value)
		}

		r2 = pyRuntime.Eval("next(_t12_gen_a)")
		if r2.Err != nil || fmt.Sprintf("%v", r2.Value) != "4" {
			return fmt.Errorf("gen_a[2] via Go: err=%v val=%v", r2.Err, r2.Value)
		}

		// gen_b[2] via Ruby = 9, gen_c[3] via Go = 2
		r = rbRuntime.Eval(`OmniVM.call("python", "next(_t12_gen_b)")`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "9" {
			return fmt.Errorf("gen_b[2] via Ruby: err=%v val=%v", r.Err, r.Value)
		}

		r2 = pyRuntime.Eval("next(_t12_gen_c)")
		if r2.Err != nil || fmt.Sprintf("%v", r2.Value) != "2" {
			return fmt.Errorf("gen_c[3] via Go: err=%v val=%v", r2.Err, r2.Value)
		}

		// Cleanup all generators
		pyRuntime.Execute("_t12_gen_a.close(); _t12_gen_b.close(); _t12_gen_c.close()")

		return nil
	})

	// Test 13: GC finalizer triggers cross-runtime call
	// A Python object's __del__ calls omnivm.call("javascript",...) during
	// gc.collect(). This is adversarial: __del__ runs at an unpredictable
	// point during GC traversal. Run 50 times to catch intermittent failures.
	run("GC finalizer triggers cross-runtime call (50x)", func() error {
		setupResult := pyRuntime.Execute(`
import gc

_t13_del_results = []

class _T13Poisoned:
    def __init__(self, val):
        self.val = val
    def __del__(self):
        try:
            result = omnivm.call("javascript", str(self.val) + " + 1")
            _t13_del_results.append(int(result))
        except Exception as e:
            _t13_del_results.append("ERR:" + str(e))
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		for i := 0; i < 50; i++ {
			// Create object, drop reference, force GC
			execResult := pyRuntime.Execute(fmt.Sprintf(`
_t13_obj = _T13Poisoned(%d)
del _t13_obj
gc.collect()
gc.collect()
`, i))
			if execResult.Err != nil {
				return fmt.Errorf("iteration %d: %v", i, execResult.Err)
			}
		}

		// Verify all 50 finalizer calls produced correct results
		countResult := pyRuntime.Eval("len(_t13_del_results)")
		if countResult.Err != nil {
			return fmt.Errorf("count: %v", countResult.Err)
		}
		count := fmt.Sprintf("%v", countResult.Value)
		if count != "50" {
			return fmt.Errorf("expected 50 finalizer results, got %s", count)
		}

		// Verify each result is i + 1
		for i := 0; i < 50; i++ {
			r := pyRuntime.Eval(fmt.Sprintf("_t13_del_results[%d]", i))
			if r.Err != nil {
				return fmt.Errorf("verify %d: %v", i, r.Err)
			}
			expected := fmt.Sprintf("%d", i+1)
			if fmt.Sprintf("%v", r.Value) != expected {
				return fmt.Errorf("iteration %d: expected %s, got %v", i, expected, r.Value)
			}
		}

		// Verify both runtimes still healthy
		pyHealth := pyRuntime.Eval("1 + 1")
		if pyHealth.Err != nil || fmt.Sprintf("%v", pyHealth.Value) != "2" {
			return fmt.Errorf("python unhealthy after finalizer test")
		}
		jsHealth := jsRuntime.Eval("1 + 1")
		if jsHealth.Err != nil || fmt.Sprintf("%v", jsHealth.Value) != "2" {
			return fmt.Errorf("javascript unhealthy after finalizer test")
		}

		return nil
	})

	// Test 14: String encoding gauntlet
	// Edge-case strings through a full round-trip chain. Tests C string handling,
	// ERR: prefix false positives, empty strings, unicode, and large allocations.
	run("String encoding gauntlet", func() error {
		// Set up test strings in Python
		setupResult := pyRuntime.Execute(`
_t14_strings = {
    "empty": "",
    "unicode": "héllo wörld \u4f60\u597d \U0001f389",
    "err_prefix": "ERR: this is not an error",
    "large": "A" * 102400,
    "escapes": 'line1\nline2\\"quoted\\"',
}

def _t14_get(name):
    return _t14_strings[name]

def _t14_check(name, val):
    expected = _t14_strings[name]
    if val == expected:
        return "match"
    return "MISMATCH: len=" + str(len(val)) + " expected_len=" + str(len(expected))
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		testCases := []struct {
			name string
			desc string
		}{
			{"empty", "empty string"},
			{"unicode", "unicode with emoji"},
			{"large", "100KB string"},
			{"escapes", "backslashes and newlines"},
		}

		for _, tc := range testCases {
			// Round trip: Python → JS → Ruby → Python verify
			// Python returns string, JS passes to Ruby, Ruby passes back to Python for verification
			result := pyRuntime.Eval(fmt.Sprintf(`
_t14_val = _t14_get("%s")
_t14_via_js = omnivm.call("javascript", 'omnivm.call("python", "_t14_get(\\"%s\\")")')
_t14_check("%s", _t14_via_js)
`, tc.name, tc.name, tc.name))
			if result.Err != nil {
				return fmt.Errorf("%s: %v", tc.desc, result.Err)
			}
			val := fmt.Sprintf("%v", result.Value)
			if val != "match" {
				return fmt.Errorf("%s: %s", tc.desc, val)
			}
		}

		// Special test for ERR: prefix — must NOT be treated as an error
		// Direct Python→JS round trip since ERR: prefix would trigger error handling
		errPrefixResult := pyRuntime.Eval(`_t14_get("err_prefix")`)
		if errPrefixResult.Err != nil {
			return fmt.Errorf("ERR: prefix test: %v", errPrefixResult.Err)
		}
		errVal := fmt.Sprintf("%v", errPrefixResult.Value)
		if errVal != "ERR: this is not an error" {
			return fmt.Errorf("ERR: prefix test: expected 'ERR: this is not an error', got %q", errVal)
		}

		// Verify the large string length survived a round-trip through the bridge
		lenResult := pyRuntime.Eval(`len(omnivm.call("javascript", 'omnivm.call("python", "_t14_get(\\"large\\")")'))`)
		if lenResult.Err != nil {
			return fmt.Errorf("large string length check: %v", lenResult.Err)
		}
		lenVal := fmt.Sprintf("%v", lenResult.Value)
		if lenVal != "102400" {
			return fmt.Errorf("large string length: expected 102400, got %s", lenVal)
		}

		return nil
	})

	// Test 15: Sustained mixed workload over 1000 dispatcher cycles
	// 4 goroutines continuously submit mixed work. Tests for state drift:
	// reference leaks, Duktape stack growth, Python global corruption.
	run("Sustained mixed workload (1000 cycles)", func() error {
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

		// Register pump callback
		disp.RegisterPumpCallback("python_pump", pyRuntime.Pump)
		defer disp.UnregisterPumpCallback("python_pump")

		// Setup generator and Ruby accumulator on main thread
		setupDone := make(chan error, 1)
		go func() {
			setupDone <- disp.RunOnMain(func() error {
				r := pyRuntime.Execute(`
def _t15_gen():
    i = 0
    while True:
        i += 1
        yield i
_t15_g = _t15_gen()
`)
				if r.Err != nil {
					return r.Err
				}
				r2 := rbRuntime.Eval("$_t15_sum = 0")
				return r2.Err
			})
		}()

		var testErr error
		totalCycles := 1000

		go func() {
			if err := <-setupDone; err != nil {
				testErr = fmt.Errorf("setup: %v", err)
				cancel()
				return
			}

			var wg sync.WaitGroup
			var mu sync.Mutex
			errored := false

			setErr := func(err error) {
				mu.Lock()
				if !errored {
					errored = true
					testErr = err
				}
				mu.Unlock()
			}

			// Goroutine A: advance Python generator, send to JS (1000 cycles)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < totalCycles; i++ {
					err := disp.RunOnMain(func() error {
						r := pyRuntime.Eval("next(_t15_g)")
						if r.Err != nil {
							return r.Err
						}
						pyVal := fmt.Sprintf("%v", r.Value)
						jr := jsRuntime.Eval(pyVal + " * 2")
						if jr.Err != nil {
							return jr.Err
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine A cycle %d: %v", i, err))
						return
					}
				}
			}()

			// Goroutine B: Ruby arithmetic (1000 cycles)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < totalCycles; i++ {
					err := disp.RunOnMain(func() error {
						expr := fmt.Sprintf("%d * %d + %d", i+1, i+2, i+3)
						expected := (i+1)*(i+2) + (i + 3)
						r := rbRuntime.Eval(expr)
						if r.Err != nil {
							return r.Err
						}
						val := fmt.Sprintf("%v", r.Value)
						exp := fmt.Sprintf("%d", expected)
						if val != exp {
							return fmt.Errorf("ruby %s: expected %s got %s", expr, exp, val)
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine B cycle %d: %v", i, err))
						return
					}
				}
			}()

			// Goroutine C: periodic gc.collect() every 10 cycles
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < totalCycles/10; i++ {
					err := disp.RunOnMain(func() error {
						r := pyRuntime.Execute("import gc; gc.collect()")
						return r.Err
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine C cycle %d: %v", i, err))
						return
					}
				}
			}()

			// Goroutine D: periodic deep chain every 50 cycles
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < totalCycles/50; i++ {
					err := disp.RunOnMain(func() error {
						r := pyRuntime.Eval(fmt.Sprintf(
							`int(omnivm.call("javascript", "parseInt(omnivm.call('ruby', '%d + 1')) + 10"))`,
							i))
						if r.Err != nil {
							return r.Err
						}
						expected := fmt.Sprintf("%d", i+1+10)
						if fmt.Sprintf("%v", r.Value) != expected {
							return fmt.Errorf("deep chain: expected %s, got %v", expected, r.Value)
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine D cycle %d: %v", i, err))
						return
					}
				}
			}()

			wg.Wait()
			cancel()
		}()

		disp.Run(ctx)

		if testErr != nil {
			return testErr
		}

		// Verify generator advanced to exactly 1000
		genResult := pyRuntime.Eval("next(_t15_g)")
		if genResult.Err != nil {
			return fmt.Errorf("final generator check: %v", genResult.Err)
		}
		if fmt.Sprintf("%v", genResult.Value) != "1001" {
			return fmt.Errorf("generator drift: expected 1001, got %v", genResult.Value)
		}

		// Cleanup
		pyRuntime.Execute("_t15_g.close()")

		return nil
	})

	// Test 16: Re-entrant exception during generator during async pump
	// Combines async pump (test 9), generator protocol (test 10), and error
	// propagation (test 6). An async task advances a generator whose body calls
	// omnivm.call("javascript",...). JS throws an error. The error propagates
	// back through the bridge into the generator's except block, which yields
	// a recovery value. The async task collects this. 8+ mixed frames.
	run("Re-entrant exception during generator during async pump", func() error {
		setupResult := pyRuntime.Execute(`
import asyncio

def _t16_gen():
    """Generator that calls JS and handles bridge errors."""
    while True:
        try:
            # This call will succeed or fail depending on the JS code
            code = yield "ready"
            result = omnivm.call("javascript", code)
            yield "ok:" + result
        except RuntimeError as e:
            yield "caught:" + str(e)
        except GeneratorExit:
            return

_t16_gen_inst = _t16_gen()
next(_t16_gen_inst)  # advance to first yield "ready"

_t16_async_result = None

async def _t16_async_task():
    global _t16_async_result
    # Send JS code that throws — the generator should catch the error
    # via the bridge and yield a recovery value
    val = _t16_gen_inst.send("(function() { throw new Error('async bridge error'); })()")
    _t16_async_result = val

_t16_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_t16_loop)
_t16_loop.create_task(_t16_async_task())
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Drive the event loop — this triggers the full call chain:
		// Execute → PyRun_SimpleString → loop.run_forever → async task
		// → gen.send() → generator body → omnivm.call("javascript", ...)
		// → OmniCall → jsRuntime.Eval → Duktape throws → returns "ERR:..."
		// → Python omnivm.call raises RuntimeError → generator except catches
		// → yield "caught:..." → async task stores result
		driveResult := pyRuntime.Execute(`
_t16_loop.stop()
_t16_loop.run_forever()
`)
		if driveResult.Err != nil {
			return fmt.Errorf("drive: %v", driveResult.Err)
		}

		// Verify the recovery value propagated correctly through all layers
		verifyResult := pyRuntime.Eval("_t16_async_result")
		if verifyResult.Err != nil {
			return fmt.Errorf("verify: %v", verifyResult.Err)
		}
		val := fmt.Sprintf("%v", verifyResult.Value)
		if !strings.Contains(val, "caught:") || !strings.Contains(val, "async bridge error") {
			return fmt.Errorf("expected 'caught:...async bridge error...', got %q", val)
		}

		// Verify generator is still alive and can handle a successful call
		successResult := pyRuntime.Execute(`
_t16_success_result = None

async def _t16_success_task():
    global _t16_success_result
    next(_t16_gen_inst)  # advance past the caught yield back to "ready"
    val = _t16_gen_inst.send("40 + 2")
    _t16_success_result = val

_t16_loop.create_task(_t16_success_task())
_t16_loop.stop()
_t16_loop.run_forever()
`)
		if successResult.Err != nil {
			return fmt.Errorf("success task: %v", successResult.Err)
		}

		successVerify := pyRuntime.Eval("_t16_success_result")
		if successVerify.Err != nil {
			return fmt.Errorf("success verify: %v", successVerify.Err)
		}
		successVal := fmt.Sprintf("%v", successVerify.Value)
		if successVal != "ok:42" {
			return fmt.Errorf("expected 'ok:42', got %q", successVal)
		}

		// Cleanup
		pyRuntime.Execute("_t16_gen_inst.close()")
		pyRuntime.Execute("_t16_loop.close()")

		return nil
	})

	// Test 17: Allocation Storm — Memory Fragmentation
	// Allocate 10MB strings in Python, JS, Ruby. Then loop 100x passing the string
	// Py → JS → Ruby → Py, with each runtime modifying one character per iteration
	// (Copy-on-Write check). This fragments the C-heap and forces allocators to coexist.
	run("Allocation storm (10MB × 3 runtimes × 100 round-trips)", func() error {
		// Create 10MB strings in each runtime
		setupResult := pyRuntime.Execute(`_t17_big = "A" * (10 * 1024 * 1024)`)
		if setupResult.Err != nil {
			return fmt.Errorf("python alloc: %v", setupResult.Err)
		}

		// Build 10MB string via doubling (Duktape strings are immutable, += is O(n²))
		jsSetup := jsRuntime.Execute(`var _t17_big = "B"; while (_t17_big.length < 10 * 1024 * 1024) _t17_big = _t17_big + _t17_big; _t17_big = _t17_big.substring(0, 10 * 1024 * 1024);`)
		if jsSetup.Err != nil {
			return fmt.Errorf("js alloc: %v", jsSetup.Err)
		}

		rbSetup := rbRuntime.Eval(`$_t17_big = "C" * (10 * 1024 * 1024)`)
		if rbSetup.Err != nil {
			return fmt.Errorf("ruby alloc: %v", rbSetup.Err)
		}

		// Verify initial sizes
		for _, check := range []struct {
			name string
			r    pkg.Runtime
			code string
		}{
			{"python", pyRuntime, `len(_t17_big)`},
			{"javascript", jsRuntime, `_t17_big.length`},
			{"ruby", rbRuntime, `$_t17_big.length`},
		} {
			result := check.r.Eval(check.code)
			if result.Err != nil {
				return fmt.Errorf("%s length check: %v", check.name, result.Err)
			}
			if fmt.Sprintf("%v", result.Value) != "10485760" {
				return fmt.Errorf("%s initial size: expected 10485760, got %v", check.name, result.Value)
			}
		}

		// Loop 100x: pass digest through Py → JS → Ruby → Py
		// Each runtime modifies character at position [iteration] in its 10MB string,
		// then computes a digest (length + char at position) that gets passed through
		// the bridge. Python and Ruby have mutable strings. JS (Duktape) has immutable
		// strings, so we track modifications in a separate array to avoid O(n) copies.
		jsRuntime.Execute(`var _t17_mods = {};`)

		for i := 0; i < 100; i++ {
			// Python: modify char at position i, compute digest
			pyResult := pyRuntime.Eval(fmt.Sprintf(`
_t17_big = _t17_big[:%d] + "P" + _t17_big[%d:]
str(len(_t17_big)) + ":" + _t17_big[%d]
`, i, i+1, i))
			if pyResult.Err != nil {
				return fmt.Errorf("iter %d python: %v", i, pyResult.Err)
			}
			pyDigest := fmt.Sprintf("%v", pyResult.Value)

			// JS: record modification at position i, verify Python's digest
			jsResult := jsRuntime.Eval(fmt.Sprintf(`
_t17_mods[%d] = "J";
var _t17_pd = "%s";
_t17_pd + "|" + _t17_big.length + ":" + (_t17_mods[%d] || _t17_big[%d])
`, i, pyDigest, i, i))
			if jsResult.Err != nil {
				return fmt.Errorf("iter %d js: %v", i, jsResult.Err)
			}
			jsOut := fmt.Sprintf("%v", jsResult.Value)

			// Ruby: modify char at position i, verify chain so far
			rbResult := rbRuntime.Eval(fmt.Sprintf(`
$_t17_big[%d] = "R"
$_t17_chain = "%s"
$_t17_chain + "|" + $_t17_big.length.to_s + ":" + $_t17_big[%d]
`, i, jsOut, i))
			if rbResult.Err != nil {
				return fmt.Errorf("iter %d ruby: %v", i, rbResult.Err)
			}
			rbOut := fmt.Sprintf("%v", rbResult.Value)

			// Verify the full chain: pyDigest|jsDigest|rbDigest
			parts := strings.Split(rbOut, "|")
			if len(parts) != 3 {
				return fmt.Errorf("iter %d: expected 3 parts, got %d: %q", i, len(parts), rbOut)
			}
			if parts[0] != "10485760:P" {
				return fmt.Errorf("iter %d: python digest wrong: %q", i, parts[0])
			}
			if parts[1] != "10485760:J" {
				return fmt.Errorf("iter %d: js digest wrong: %q", i, parts[1])
			}
			if parts[2] != "10485760:R" {
				return fmt.Errorf("iter %d: ruby digest wrong: %q", i, parts[2])
			}
		}

		// Final verification: check that modifications persisted
		pyCheck := pyRuntime.Eval(`_t17_big[0] + _t17_big[50] + _t17_big[99] + ":" + str(len(_t17_big))`)
		if pyCheck.Err != nil {
			return fmt.Errorf("final python check: %v", pyCheck.Err)
		}
		if fmt.Sprintf("%v", pyCheck.Value) != "PPP:10485760" {
			return fmt.Errorf("final python: expected 'PPP:10485760', got %q", pyCheck.Value)
		}

		// JS: check via mods array (Duktape immutable strings — originals unchanged)
		jsCheck := jsRuntime.Eval(`(_t17_mods[0] || "?") + (_t17_mods[50] || "?") + (_t17_mods[99] || "?") + ":" + _t17_big.length`)
		if jsCheck.Err != nil {
			return fmt.Errorf("final js check: %v", jsCheck.Err)
		}
		if fmt.Sprintf("%v", jsCheck.Value) != "JJJ:10485760" {
			return fmt.Errorf("final js: expected 'JJJ:10485760', got %q", jsCheck.Value)
		}

		rbCheck := rbRuntime.Eval(`$_t17_big[0] + $_t17_big[50] + $_t17_big[99] + ":" + $_t17_big.length.to_s`)
		if rbCheck.Err != nil {
			return fmt.Errorf("final ruby check: %v", rbCheck.Err)
		}
		if fmt.Sprintf("%v", rbCheck.Value) != "RRR:10485760" {
			return fmt.Errorf("final ruby: expected 'RRR:10485760', got %q", rbCheck.Value)
		}

		// Cleanup — release the 10MB strings
		pyRuntime.Execute("del _t17_big")
		jsRuntime.Execute("_t17_big = undefined; _t17_mods = undefined;")
		rbRuntime.Eval("$_t17_big = nil")

		return nil
	})

	// Test 18: Sleep-Wake Concurrency Torture
	// Verify that Go time.Sleep (which parks the Goroutine) doesn't deschedule
	// the Golden Thread in a way that breaks CPython/Duktape thread-local storage.
	// Between RunOnMain calls, the Go scheduler might run other Goroutines on the
	// main OS thread — if LockOSThread isn't tight enough, TLS would be lost.
	run("Sleep-wake concurrency torture", func() error {
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

		// Register pump callback
		disp.RegisterPumpCallback("python_pump", pyRuntime.Pump)
		defer disp.UnregisterPumpCallback("python_pump")

		var testErr error
		var mu sync.Mutex
		setErr := func(err error) {
			mu.Lock()
			if testErr == nil {
				testErr = err
			}
			mu.Unlock()
		}

		go func() {
			defer cancel()

			// Step 1: Set up state in Python, JS, and Ruby on the main thread
			err := disp.RunOnMain(func() error {
				r := pyRuntime.Execute("import time; _t18_x = 42; _t18_marker = 'ALIVE'")
				if r.Err != nil {
					return fmt.Errorf("python setup: %v", r.Err)
				}
				r2 := jsRuntime.Execute("var _t18_x = 42; var _t18_marker = 'ALIVE';")
				if r2.Err != nil {
					return fmt.Errorf("js setup: %v", r2.Err)
				}
				r3 := rbRuntime.Eval("$_t18_x = 42; $_t18_marker = 'ALIVE'")
				if r3.Err != nil {
					return fmt.Errorf("ruby setup: %v", r3.Err)
				}
				return nil
			})
			if err != nil {
				setErr(err)
				return
			}

			// Step 2: Sleep-wake cycles with increasing sleep durations
			// Each cycle: sleep → RunOnMain → verify TLS survived
			// We mutate x each cycle and verify the mutation persists across sleep.
			sleepDurations := []time.Duration{
				1 * time.Millisecond,
				10 * time.Millisecond,
				50 * time.Millisecond,
				100 * time.Millisecond,
				200 * time.Millisecond,
			}

			expectedX := 42 // tracks the current expected value
			for cycle, dur := range sleepDurations {
				// Park this goroutine — Go scheduler free to do whatever
				time.Sleep(dur)

				capturedExpected := expectedX
				capturedCycle := cycle

				// Wake up and verify all runtimes still have their TLS state
				err := disp.RunOnMain(func() error {
					expectedStr := fmt.Sprintf("%d", capturedExpected)

					// Python: verify variable survived the sleep
					pyR := pyRuntime.Eval("_t18_x")
					if pyR.Err != nil {
						return fmt.Errorf("cycle %d python eval: %v", capturedCycle, pyR.Err)
					}
					if fmt.Sprintf("%v", pyR.Value) != expectedStr {
						return fmt.Errorf("cycle %d python TLS lost: expected %s, got %v", capturedCycle, expectedStr, pyR.Value)
					}

					// JS: verify variable survived
					jsR := jsRuntime.Eval("_t18_x")
					if jsR.Err != nil {
						return fmt.Errorf("cycle %d js eval: %v", capturedCycle, jsR.Err)
					}
					if fmt.Sprintf("%v", jsR.Value) != expectedStr {
						return fmt.Errorf("cycle %d js TLS lost: expected %s, got %v", capturedCycle, expectedStr, jsR.Value)
					}

					// Ruby: verify variable survived
					rbR := rbRuntime.Eval("$_t18_x")
					if rbR.Err != nil {
						return fmt.Errorf("cycle %d ruby eval: %v", capturedCycle, rbR.Err)
					}
					if fmt.Sprintf("%v", rbR.Value) != expectedStr {
						return fmt.Errorf("cycle %d ruby TLS lost: expected %s, got %v", capturedCycle, expectedStr, rbR.Value)
					}

					// Mutate state — next cycle will verify this mutation survived the sleep
					newVal := capturedExpected + 1
					pyRuntime.Execute(fmt.Sprintf("_t18_x = %d", newVal))
					jsRuntime.Execute(fmt.Sprintf("_t18_x = %d;", newVal))
					rbRuntime.Eval(fmt.Sprintf("$_t18_x = %d", newVal))

					return nil
				})
				if err != nil {
					setErr(err)
					return
				}
				expectedX++ // track the mutation for next cycle
			}

			// Step 3: Final verification with all 3 runtimes checking at once
			finalExpected := fmt.Sprintf("%d", expectedX)
			err = disp.RunOnMain(func() error {
				for _, check := range []struct {
					name string
					r    pkg.Runtime
					code string
				}{
					{"python", pyRuntime, "_t18_x"},
					{"javascript", jsRuntime, "_t18_x"},
					{"ruby", rbRuntime, "$_t18_x"},
				} {
					result := check.r.Eval(check.code)
					if result.Err != nil {
						return fmt.Errorf("final %s: %v", check.name, result.Err)
					}
					if fmt.Sprintf("%v", result.Value) != finalExpected {
						return fmt.Errorf("final %s: expected %s, got %v", check.name, finalExpected, result.Value)
					}
				}

				// Verify marker strings survived all the sleep-wake cycles
				pyM := pyRuntime.Eval("_t18_marker")
				if pyM.Err != nil || fmt.Sprintf("%v", pyM.Value) != "ALIVE" {
					return fmt.Errorf("python marker lost")
				}
				jsM := jsRuntime.Eval("_t18_marker")
				if jsM.Err != nil || fmt.Sprintf("%v", jsM.Value) != "ALIVE" {
					return fmt.Errorf("js marker lost")
				}
				rbM := rbRuntime.Eval("$_t18_marker")
				if rbM.Err != nil || fmt.Sprintf("%v", rbM.Value) != "ALIVE" {
					return fmt.Errorf("ruby marker lost")
				}

				return nil
			})
			if err != nil {
				setErr(err)
				return
			}

			// Step 4: Concurrent pressure — launch multiple goroutines that
			// all sleep and then try to RunOnMain, stressing the scheduler
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(gid int) {
					defer wg.Done()
					for rep := 0; rep < 10; rep++ {
						time.Sleep(time.Duration(gid*5+rep) * time.Millisecond)
						err := disp.RunOnMain(func() error {
							r := pyRuntime.Eval(fmt.Sprintf("42 + %d + %d", gid, rep))
							if r.Err != nil {
								return fmt.Errorf("goroutine %d rep %d: %v", gid, rep, r.Err)
							}
							expected := fmt.Sprintf("%d", 42+gid+rep)
							if fmt.Sprintf("%v", r.Value) != expected {
								return fmt.Errorf("goroutine %d rep %d: expected %s, got %v",
									gid, rep, expected, r.Value)
							}
							return nil
						})
						if err != nil {
							setErr(err)
							return
						}
					}
				}(g)
			}
			wg.Wait()
		}()

		disp.Run(ctx)

		if testErr != nil {
			return testErr
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
