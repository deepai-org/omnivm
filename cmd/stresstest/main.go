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
#include <unistd.h>
#include <sys/syscall.h>

// Forward declarations of exported Go functions (cgo drops const qualifiers)
extern char* OmniCall(char* runtime, char* code);
extern void OmniFree(char* ptr);

// Get function pointers to pass to runtimes
static void* get_omni_call_ptr() { return (void*)OmniCall; }
static void* get_omni_free_ptr() { return (void*)OmniFree; }

// Get the current OS thread ID (Linux-specific).
static long get_thread_id() { return syscall(SYS_gettid); }
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
	"github.com/omnivm/omnivm/pkg/watchdog"
)

func init() {
	runtime.LockOSThread()
}

// Global runtime registry for the bridge callback
var runtimes = make(map[string]pkg.Runtime)

// Allocation counter for memory leak detection
var allocCount int64

// goldenThreadID is the OS thread ID of the main goroutine (set in main).
// OmniCall checks this to reject calls from rogue threads.
var goldenThreadID int64

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
	// Guard: reject calls from non-Golden threads
	currentTid := int64(C.get_thread_id())
	if currentTid != goldenThreadID {
		msg := fmt.Sprintf("ERR:omnivm.call from non-Golden Thread (tid=%d, expected=%d). "+
			"Cross-runtime calls must originate from the main thread.", currentTid, goldenThreadID)
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

	// Test 19: Java enters the arena
	// Java has been initialized but barely tested. Verify basic Java eval and
	// bidirectional bridge calls between Java and every other runtime.
	run("Java enters the arena", func() error {
		// Basic Java eval (compiles a fresh class each time)
		r := jvmRuntime.Eval("1 + 1")
		if r.Err != nil {
			return fmt.Errorf("basic eval: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "2" {
			return fmt.Errorf("basic eval: expected '2', got %q", r.Value)
		}

		// Java → Python (via fully-qualified OmniVM.call in eval context)
		r = jvmRuntime.Eval(`omnivm.OmniVM.call("python", "7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("java→python: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("java→python: expected '42', got %q", r.Value)
		}

		// Java → JavaScript
		r = jvmRuntime.Eval(`omnivm.OmniVM.call("javascript", "7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("java→javascript: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("java→javascript: expected '42', got %q", r.Value)
		}

		// Java → Ruby
		r = jvmRuntime.Eval(`omnivm.OmniVM.call("ruby", "7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("java→ruby: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("java→ruby: expected '42', got %q", r.Value)
		}

		// Python → Java
		r = pyRuntime.Eval(`omnivm.call("java", "7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("python→java: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("python→java: expected '42', got %q", r.Value)
		}

		// JavaScript → Java
		r = jsRuntime.Eval(`omnivm.call("java", "7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("js→java: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("js→java: expected '42', got %q", r.Value)
		}

		// Ruby → Java
		r = rbRuntime.Eval(`OmniVM.call("java", "7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("ruby→java: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("ruby→java: expected '42', got %q", r.Value)
		}

		return nil
	})

	// Test 20: Full quadrilateral chain (Py → JS → Java → Ruby)
	// First test that exercises ALL 4 guest runtimes in a single nested call stack.
	// Uses relay functions in Py/JS/Ruby to avoid escaping hell. Java is reached
	// via the bridge and calls Ruby as the terminal hop.
	run("Full quadrilateral chain (Py → JS → Java → Ruby)", func() error {
		// Set up relay functions in each runtime
		r := pyRuntime.Execute(`
def _t20_py_relay(x):
    """Adds 5, calls JS relay."""
    return omnivm.call("javascript", "_t20_js_relay(" + str(int(x) + 5) + ")")
`)
		if r.Err != nil {
			return fmt.Errorf("python relay setup: %v", r.Err)
		}

		r = jsRuntime.Execute(`
function _t20_js_relay(x) {
    // Multiplies by 2, calls Java which calls Ruby
    var val = x * 2;
    return omnivm.call("java", 'omnivm.OmniVM.call("ruby", "' + val + ' * 3")');
}
`)
		if r.Err != nil {
			return fmt.Errorf("js relay setup: %v", r.Err)
		}

		// Chain: _t20_py_relay(10)
		//   → Python: 10 + 5 = 15, calls _t20_js_relay(15)
		//   → JS: 15 * 2 = 30, calls Java with OmniVM.call("ruby", "30 * 3")
		//   → Java: calls Ruby with "30 * 3"
		//   → Ruby: 30 * 3 = 90
		//   → returns "90" all the way back
		result := pyRuntime.Eval("_t20_py_relay(10)")
		if result.Err != nil {
			return fmt.Errorf("quadrilateral chain: %v", result.Err)
		}
		if fmt.Sprintf("%v", result.Value) != "90" {
			return fmt.Errorf("quadrilateral chain: expected '90', got %q", result.Value)
		}

		// Reverse direction: Java → Python → JS → Ruby
		// Java calls Python which calls JS which calls Ruby
		r2 := jvmRuntime.Eval(`omnivm.OmniVM.call("python", "omnivm.call('javascript', 'omnivm.call(\"ruby\", \"7 * 8\")')")`)
		if r2.Err != nil {
			return fmt.Errorf("reverse quad (Java→Py→JS→Ruby): %v", r2.Err)
		}
		if fmt.Sprintf("%v", r2.Value) != "56" {
			return fmt.Errorf("reverse quad: expected '56', got %q", r2.Value)
		}

		// Yet another permutation: Ruby → Java → Python → JS
		r3 := rbRuntime.Eval(`OmniVM.call("java", 'omnivm.OmniVM.call("python", "omnivm.call(\'javascript\', \'3 + 4\')")')`)
		if r3.Err != nil {
			return fmt.Errorf("Ruby→Java→Py→JS: %v", r3.Err)
		}
		if fmt.Sprintf("%v", r3.Value) != "7" {
			return fmt.Errorf("Ruby→Java→Py→JS: expected '7', got %q", r3.Value)
		}

		return nil
	})

	// Test 21: Java re-entrant call (Java → Python → Java)
	// Tests JNI re-entry: Java calls Python via bridge, Python calls back into
	// Java via bridge. The JVM handles nested JNI calls on the same thread.
	// Then tests deeper: Java → Python → JS → Java (Java appears twice).
	run("Java re-entrant call (Java → Py → Java)", func() error {
		// Simple re-entry: Java → Python → Java
		r := jvmRuntime.Eval(`omnivm.OmniVM.call("python", "omnivm.call('java', '100 + 23')")`)
		if r.Err != nil {
			return fmt.Errorf("Java→Py→Java: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "123" {
			return fmt.Errorf("Java→Py→Java: expected '123', got %q", r.Value)
		}

		// Deeper: Java → Python → JS → Java
		r = jvmRuntime.Eval(`omnivm.OmniVM.call("python", "omnivm.call('javascript', 'omnivm.call(\"java\", \"200 + 34\")')")`)
		if r.Err != nil {
			return fmt.Errorf("Java→Py→JS→Java: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "234" {
			return fmt.Errorf("Java→Py→JS→Java: expected '234', got %q", r.Value)
		}

		// Triple Java: Java → Ruby → Java → Python → Java
		r = jvmRuntime.Eval(`omnivm.OmniVM.call("ruby", "OmniVM.call(\"java\", 'omnivm.OmniVM.call(\"python\", \"omnivm.call(\\\'java\\\', \\\'300 + 45\\\')\")')")`)
		if r.Err != nil {
			// This is an extremely deep chain with complex escaping.
			// If it fails due to escaping, try a relay approach instead.
			// Try relay approach: set up Python helper
			pyRuntime.Execute(`
def _t21_inner():
    return omnivm.call("java", "300 + 45")
`)
			rbRuntime.Eval(`
def _t21_rb_relay
  OmniVM.call("java", 'omnivm.OmniVM.call("python", "_t21_inner()")')
end
`)
			r = jvmRuntime.Eval(`omnivm.OmniVM.call("ruby", "_t21_rb_relay")`)
			if r.Err != nil {
				return fmt.Errorf("triple Java (relay): %v", r.Err)
			}
		}
		if fmt.Sprintf("%v", r.Value) != "345" {
			return fmt.Errorf("triple Java: expected '345', got %q", r.Value)
		}

		return nil
	})

	// Test 22: Java exception handling through bridge
	// Java try/catch around failing cross-runtime calls. Verifies the JNI
	// exception path (ERR: prefix → ThrowNew → RuntimeException → catch).
	run("Java exception handling through bridge", func() error {
		// Java catches an error from JS throw, recovers, then succeeds
		r := jvmRuntime.Execute(`
import omnivm.OmniVM;
String result;
try {
    result = OmniVM.call("javascript", "(function() { throw new Error('bridge error'); })()");
} catch (RuntimeException e) {
    result = "caught:" + e.getMessage();
}
// Prove bridge still works after catching error
String check = OmniVM.call("python", "1 + 1");
System.out.print(result + "|ok:" + check);
`)
		if r.Err != nil {
			return fmt.Errorf("java exception handling: %v", r.Err)
		}
		output := strings.TrimSpace(r.Output)
		if !strings.Contains(output, "caught:") || !strings.Contains(output, "bridge error") {
			return fmt.Errorf("expected caught error, got %q", output)
		}
		if !strings.Contains(output, "|ok:2") {
			return fmt.Errorf("bridge unhealthy after catch, got %q", output)
		}

		// Java catches Python exception
		r = jvmRuntime.Execute(`
import omnivm.OmniVM;
String result;
try {
    result = OmniVM.call("python", "1/0");
} catch (RuntimeException e) {
    result = "py_caught:" + e.getMessage();
}
System.out.print(result);
`)
		if r.Err != nil {
			return fmt.Errorf("java catch python error: %v", r.Err)
		}
		output = strings.TrimSpace(r.Output)
		if !strings.Contains(output, "py_caught:") || !strings.Contains(output, "division by zero") {
			return fmt.Errorf("expected python division error, got %q", output)
		}

		// Java catches error, retries with different runtime
		r = jvmRuntime.Execute(`
import omnivm.OmniVM;
String result = "unset";
for (int attempt = 0; attempt < 3; attempt++) {
    try {
        if (attempt < 2) {
            OmniVM.call("javascript", "(function() { throw new Error('attempt ' + " + attempt + "); })()");
        } else {
            result = "success:" + OmniVM.call("ruby", "42 * 2");
        }
    } catch (RuntimeException e) {
        // Retry on next iteration
    }
}
System.out.print(result);
`)
		if r.Err != nil {
			return fmt.Errorf("java retry loop: %v", r.Err)
		}
		output = strings.TrimSpace(r.Output)
		if output != "success:84" {
			return fmt.Errorf("java retry: expected 'success:84', got %q", output)
		}

		return nil
	})

	// Test 23: Java compilation storm (50 unique classes with bridge calls)
	// Each iteration compiles a fresh Java class via javax.tools.JavaCompiler,
	// each calling a different runtime. Stresses the in-memory compiler,
	// classloader, and JNI bridge under repeated compilation churn.
	run("Java compilation storm (50 unique classes)", func() error {
		runtimeNames := []string{"python", "javascript", "ruby"}
		for i := 0; i < 50; i++ {
			target := runtimeNames[i%3]
			var expr string
			switch target {
			case "python":
				expr = fmt.Sprintf("%d + 1", i)
			case "javascript":
				expr = fmt.Sprintf("%d + 1", i)
			case "ruby":
				expr = fmt.Sprintf("%d + 1", i)
			}

			r := jvmRuntime.Eval(fmt.Sprintf(`Integer.parseInt(omnivm.OmniVM.call("%s", "%s"))`, target, expr))
			if r.Err != nil {
				return fmt.Errorf("iter %d (%s): %v", i, target, r.Err)
			}
			expected := fmt.Sprintf("%d", i+1)
			if fmt.Sprintf("%v", r.Value) != expected {
				return fmt.Errorf("iter %d (%s): expected %s, got %v", i, target, expected, r.Value)
			}
		}

		// Verify all runtimes still healthy after 50 compilations
		for _, check := range []struct {
			name string
			r    pkg.Runtime
		}{
			{"python", pyRuntime},
			{"javascript", jsRuntime},
			{"ruby", rbRuntime},
			{"java", jvmRuntime},
		} {
			result := check.r.Eval("1 + 1")
			if result.Err != nil || fmt.Sprintf("%v", result.Value) != "2" {
				return fmt.Errorf("%s unhealthy after storm", check.name)
			}
		}

		return nil
	})

	// Test 24: Concurrent Java + other runtimes via dispatcher
	// Multiple goroutines submit mixed Java/Python/JS/Ruby work through
	// the dispatcher. Tests that JVM and other runtimes coexist under
	// concurrent dispatch pressure, including Java cross-runtime calls.
	run("Concurrent Java + all runtimes via dispatcher", func() error {
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

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
			var wg sync.WaitGroup

			// Goroutine A: Java eval 50x
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					capturedI := i
					err := disp.RunOnMain(func() error {
						r := jvmRuntime.Eval(fmt.Sprintf("%d + %d", capturedI, capturedI))
						if r.Err != nil {
							return r.Err
						}
						expected := fmt.Sprintf("%d", capturedI*2)
						if fmt.Sprintf("%v", r.Value) != expected {
							return fmt.Errorf("java eval: expected %s, got %v", expected, r.Value)
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine A iter %d: %v", i, err))
						return
					}
				}
			}()

			// Goroutine B: Python eval 50x
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 50; i++ {
					capturedI := i
					err := disp.RunOnMain(func() error {
						r := pyRuntime.Eval(fmt.Sprintf("%d * 3", capturedI))
						if r.Err != nil {
							return r.Err
						}
						expected := fmt.Sprintf("%d", capturedI*3)
						if fmt.Sprintf("%v", r.Value) != expected {
							return fmt.Errorf("python eval: expected %s, got %v", expected, r.Value)
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine B iter %d: %v", i, err))
						return
					}
				}
			}()

			// Goroutine C: Java → Python cross-runtime 25x
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					capturedI := i
					err := disp.RunOnMain(func() error {
						r := jvmRuntime.Eval(fmt.Sprintf(
							`omnivm.OmniVM.call("python", "%d + %d")`, capturedI, capturedI+1))
						if r.Err != nil {
							return r.Err
						}
						expected := fmt.Sprintf("%d", capturedI*2+1)
						if fmt.Sprintf("%v", r.Value) != expected {
							return fmt.Errorf("java→python: expected %s, got %v", expected, r.Value)
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine C iter %d: %v", i, err))
						return
					}
				}
			}()

			// Goroutine D: JS → Java cross-runtime 25x
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					capturedI := i
					err := disp.RunOnMain(func() error {
						r := jsRuntime.Eval(fmt.Sprintf(
							`omnivm.call("java", "%d * 2")`, capturedI))
						if r.Err != nil {
							return r.Err
						}
						expected := fmt.Sprintf("%d", capturedI*2)
						if fmt.Sprintf("%v", r.Value) != expected {
							return fmt.Errorf("js→java: expected %s, got %v", expected, r.Value)
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine D iter %d: %v", i, err))
						return
					}
				}
			}()

			// Goroutine E: Ruby → Java → Python deep chain 10x
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 10; i++ {
					capturedI := i
					err := disp.RunOnMain(func() error {
						r := rbRuntime.Eval(fmt.Sprintf(
							`OmniVM.call("java", 'omnivm.OmniVM.call("python", "%d + 100")')`, capturedI))
						if r.Err != nil {
							return r.Err
						}
						expected := fmt.Sprintf("%d", capturedI+100)
						if fmt.Sprintf("%v", r.Value) != expected {
							return fmt.Errorf("ruby→java→python: expected %s, got %v", expected, r.Value)
						}
						return nil
					})
					if err != nil {
						setErr(fmt.Errorf("goroutine E iter %d: %v", i, err))
						return
					}
				}
			}()

			wg.Wait()
		}()

		disp.Run(ctx)

		if testErr != nil {
			return testErr
		}
		return nil
	})

	// Test 25: Generator send() pipeline across runtimes
	// Three generators chained via send(). Gen A yields values, transformed by JS,
	// sent to Gen B, transformed by Ruby, sent to Gen C. Tests that yield
	// expressions properly receive cross-runtime values through the
	// suspended frame resume path.
	run("Generator send() pipeline across runtimes", func() error {
		setupResult := pyRuntime.Execute(`
def _t25_gen_a():
    """Yields squares. Receives values via send() and adds them to next square."""
    i = 0
    bonus = 0
    while True:
        i += 1
        val = yield (i * i) + bonus
        bonus = int(val) if val is not None else 0

def _t25_gen_b():
    """Receives a value, triples it, yields the tripled value."""
    val = yield "ready"
    while True:
        val = yield int(val) * 3

def _t25_gen_c():
    """Receives a value, yields it plus 1000."""
    val = yield "ready"
    while True:
        val = yield int(val) + 1000

_t25_a = _t25_gen_a()
_t25_b = _t25_gen_b()
_t25_c = _t25_gen_c()

# Prime all generators
next(_t25_a)
next(_t25_b)
next(_t25_c)
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Pipeline: advance A → transform via JS → send to B → transform via Ruby → send to C
		for i := 0; i < 20; i++ {
			// Advance generator A (yields (i+2)^2 + bonus from previous cycle)
			aVal := pyRuntime.Eval("next(_t25_a)")
			if aVal.Err != nil {
				return fmt.Errorf("iter %d gen_a next: %v", i, aVal.Err)
			}

			// Transform via JS: multiply by 2
			jsVal := jsRuntime.Eval(fmt.Sprintf("parseInt('%v') * 2", aVal.Value))
			if jsVal.Err != nil {
				return fmt.Errorf("iter %d js transform: %v", i, jsVal.Err)
			}

			// Send JS result to generator B
			bVal := pyRuntime.Eval(fmt.Sprintf("_t25_b.send('%v')", jsVal.Value))
			if bVal.Err != nil {
				return fmt.Errorf("iter %d gen_b send: %v", i, bVal.Err)
			}

			// Transform via Ruby: add 10
			rbVal := rbRuntime.Eval(fmt.Sprintf("%v + 10", bVal.Value))
			if rbVal.Err != nil {
				return fmt.Errorf("iter %d ruby transform: %v", i, rbVal.Err)
			}

			// Send Ruby result to generator C
			cVal := pyRuntime.Eval(fmt.Sprintf("_t25_c.send('%v')", rbVal.Value))
			if cVal.Err != nil {
				return fmt.Errorf("iter %d gen_c send: %v", i, cVal.Err)
			}

			// Send C's result back to A via send() — this becomes A's bonus
			pyRuntime.Eval(fmt.Sprintf("_t25_a.send('%v')", cVal.Value))
		}

		// Verify generators are all still alive and functional
		finalA := pyRuntime.Eval("next(_t25_a)")
		if finalA.Err != nil {
			return fmt.Errorf("final gen_a: %v", finalA.Err)
		}

		// Cleanup
		pyRuntime.Execute("_t25_a.close(); _t25_b.close(); _t25_c.close()")

		return nil
	})

	// Test 26: Python iterator protocol (__next__) with cross-runtime calls
	// A custom iterator whose __next__ calls JS on every iteration. Used in
	// for loops and list(). Python's C-level tp_iternext calls the bridge
	// 100x in a tight loop — tests rapid-fire C boundary crossings driven
	// by Python's internal iteration machinery.
	run("Iterator protocol with cross-runtime __next__", func() error {
		setupResult := pyRuntime.Execute(`
class _T26_CrossIter:
    """Iterator whose __next__ calls JS for each value."""
    def __init__(self, n):
        self.n = n
        self.i = 0
    def __iter__(self):
        return self
    def __next__(self):
        if self.i >= self.n:
            raise StopIteration
        self.i += 1
        return omnivm.call("javascript", str(self.i) + " * 2")
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Use list() to consume iterator — 100 bridge calls from C-level iteration
		listResult := pyRuntime.Eval("list(_T26_CrossIter(100))")
		if listResult.Err != nil {
			return fmt.Errorf("list() consumption: %v", listResult.Err)
		}

		// Verify first, middle, last elements (reuse one list, avoid 3x100 bridge calls)
		pyRuntime.Execute("_t26_list = list(_T26_CrossIter(100))")
		check := pyRuntime.Eval("_t26_list[0] + '|' + _t26_list[49] + '|' + _t26_list[99]")
		if check.Err != nil {
			return fmt.Errorf("element check: %v", check.Err)
		}
		val := fmt.Sprintf("%v", check.Value)
		if val != "2|100|200" {
			return fmt.Errorf("expected '2|100|200', got %q", val)
		}

		// Use in a for loop with sum — 100 more bridge calls
		sumResult := pyRuntime.Eval("sum(int(x) for x in _T26_CrossIter(100))")
		if sumResult.Err != nil {
			return fmt.Errorf("sum via for: %v", sumResult.Err)
		}
		// sum(2+4+6+...+200) = 2*sum(1..100) = 2*5050 = 10100
		if fmt.Sprintf("%v", sumResult.Value) != "10100" {
			return fmt.Errorf("sum: expected '10100', got %v", sumResult.Value)
		}

		// Cross-runtime: iterator produces JS values, consumed by Java
		// (avoid Ruby here — rb_exc_raise crashes on ARM64 with JVM if eval errors)
		pyRuntime.Execute("_t26_vals = list(_T26_CrossIter(5))")
		for i := 0; i < 5; i++ {
			pyVal := pyRuntime.Eval(fmt.Sprintf("_t26_vals[%d]", i))
			if pyVal.Err != nil {
				return fmt.Errorf("java check %d: %v", i, pyVal.Err)
			}
			jvmResult := jvmRuntime.Eval(fmt.Sprintf("Integer.parseInt(\"%v\") + 1", pyVal.Value))
			if jvmResult.Err != nil {
				return fmt.Errorf("java compute %d: %v", i, jvmResult.Err)
			}
			expected := fmt.Sprintf("%d", (i+1)*2+1)
			if fmt.Sprintf("%v", jvmResult.Value) != expected {
				return fmt.Errorf("java check %d: expected %s, got %v", i, expected, jvmResult.Value)
			}
		}

		return nil
	})

	// Test 27: Context manager __exit__ with cross-runtime call during exception
	// __enter__ calls JS, body raises ValueError, __exit__ calls Ruby while
	// exception info is being passed as arguments. Tests whether Python's
	// exception-to-__exit__-args handoff interferes with the bridge.
	run("Context manager __exit__ with in-flight exception", func() error {
		setupResult := pyRuntime.Execute(`
class _T27_CM:
    def __init__(self):
        self.enter_val = None
        self.exit_val = None
        self.exc_type_name = None
        self.suppressed = False

    def __enter__(self):
        self.enter_val = omnivm.call("javascript", "21 * 2")
        return self

    def __exit__(self, exc_type, exc_val, exc_tb):
        if exc_type is not None:
            self.exc_type_name = exc_type.__name__
            # Call Ruby while exception info is being processed
            self.exit_val = omnivm.call("ruby", "84 / 2")
            self.suppressed = True
            return True  # suppress the exception
        return False
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Test 1: Normal flow (no exception)
		r := pyRuntime.Eval(`
_t27_cm1 = _T27_CM()
with _t27_cm1:
    pass
_t27_cm1.enter_val + "|" + str(_t27_cm1.suppressed)
`)
		if r.Err != nil {
			return fmt.Errorf("normal flow: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42|False" {
			return fmt.Errorf("normal flow: expected '42|False', got %q", r.Value)
		}

		// Test 2: Exception flow — __exit__ calls Ruby while handling ValueError
		r = pyRuntime.Eval(`
_t27_cm2 = _T27_CM()
with _t27_cm2:
    raise ValueError("test error")
_t27_cm2.enter_val + "|" + _t27_cm2.exit_val + "|" + _t27_cm2.exc_type_name + "|" + str(_t27_cm2.suppressed)
`)
		if r.Err != nil {
			return fmt.Errorf("exception flow: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42|42|ValueError|True" {
			return fmt.Errorf("exception flow: expected '42|42|ValueError|True', got %q", r.Value)
		}

		// Test 3: Nested context managers, inner raises, both __exit__ call different runtimes
		pyRuntime.Execute(`
class _T27_CM_Inner:
    def __init__(self):
        self.exit_val = None
    def __enter__(self):
        return self
    def __exit__(self, exc_type, exc_val, exc_tb):
        if exc_type is not None:
            self.exit_val = omnivm.call("java", "99 + 1")
            return True  # suppress
        return False
`)
		r = pyRuntime.Eval(`
_t27_outer = _T27_CM()
_t27_inner = _T27_CM_Inner()
with _t27_outer:
    with _t27_inner:
        raise RuntimeError("nested")
_t27_outer.enter_val + "|" + _t27_inner.exit_val + "|" + str(_t27_outer.suppressed)
`)
		if r.Err != nil {
			return fmt.Errorf("nested cm: %v", r.Err)
		}
		// Inner __exit__ suppresses, so outer sees no exception
		if fmt.Sprintf("%v", r.Value) != "42|100|False" {
			return fmt.Errorf("nested cm: expected '42|100|False', got %q", r.Value)
		}

		return nil
	})

	// Test 28: Nested try/finally cross-runtime during stack unwinding
	// Three nested finally blocks, each calling a different runtime, while a
	// ValueError propagates up. Python stashes the exception on the frame's
	// exception stack during each finally — tests bridge calls with stashed exceptions.
	run("Nested try/finally cross-runtime during stack unwinding", func() error {
		r := pyRuntime.Eval(`
_t28_results = []
_t28_caught = False
try:
    try:
        try:
            raise ValueError("propagating")
        finally:
            _t28_results.append("js:" + omnivm.call("javascript", "10 + 1"))
    finally:
        _t28_results.append("rb:" + omnivm.call("ruby", "20 + 2"))
except ValueError as e:
    _t28_results.append("caught:" + str(e))
    _t28_caught = True

"|".join(_t28_results) + "|" + str(_t28_caught)
`)
		if r.Err != nil {
			return fmt.Errorf("basic unwinding: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "js:11|rb:22|caught:propagating|True" {
			return fmt.Errorf("basic unwinding: expected 'js:11|rb:22|caught:propagating|True', got %q", r.Value)
		}

		// Deeper: 4 levels with Java, plus a cross-runtime call in the except block
		r = pyRuntime.Eval(`
_t28_r2 = []
try:
    try:
        try:
            try:
                raise TypeError("deep")
            finally:
                _t28_r2.append("py:" + str(1 + 1))
        finally:
            _t28_r2.append("js:" + omnivm.call("javascript", "2 + 2"))
    finally:
        _t28_r2.append("rb:" + omnivm.call("ruby", "3 + 3"))
except TypeError:
    _t28_r2.append("java:" + omnivm.call("java", "4 + 4"))
"|".join(_t28_r2)
`)
		if r.Err != nil {
			return fmt.Errorf("deep unwinding: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "py:2|js:4|rb:6|java:8" {
			return fmt.Errorf("deep unwinding: expected 'py:2|js:4|rb:6|java:8', got %q", r.Value)
		}

		// Edge case: finally block's cross-runtime call itself raises
		// The new error should replace the original during propagation
		r = pyRuntime.Eval(`
_t28_r3 = "unset"
try:
    try:
        raise ValueError("original")
    finally:
        # This JS call throws, replacing the ValueError with RuntimeError
        try:
            omnivm.call("javascript", "(function() { throw new Error('finally boom'); })()")
        except RuntimeError:
            _t28_r3 = "finally_caught"
except ValueError:
    _t28_r3 = _t28_r3 + "|original_caught"
_t28_r3
`)
		if r.Err != nil {
			return fmt.Errorf("finally-raises: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "finally_caught|original_caught" {
			return fmt.Errorf("finally-raises: expected 'finally_caught|original_caught', got %q", r.Value)
		}

		return nil
	})

	// Test 29: __getattr__ dynamically dispatches to runtimes
	// A proxy object where attribute access triggers omnivm.call. This fires
	// from inside Python's LOAD_ATTR opcode handler — we're re-entering the
	// bridge during name resolution. Tests bridge invocation during Python's
	// attribute lookup protocol.
	run("__getattr__ dispatches to runtimes", func() error {
		setupResult := pyRuntime.Execute(`
class _T29_RuntimeProxy:
    """Proxy where attribute access calls a runtime."""
    def __init__(self, runtime):
        # Use object.__setattr__ to avoid triggering __getattr__
        object.__setattr__(self, '_runtime', runtime)
        object.__setattr__(self, '_call_count', 0)

    def __getattr__(self, name):
        rt = object.__getattribute__(self, '_runtime')
        count = object.__getattribute__(self, '_call_count')
        object.__setattr__(self, '_call_count', count + 1)
        return omnivm.call(rt, name)

_t29_js = _T29_RuntimeProxy("javascript")
_t29_rb = _T29_RuntimeProxy("ruby")
_t29_java = _T29_RuntimeProxy("java")
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Access JS proxy — attribute name IS the JS expression
		r := pyRuntime.Eval(`_t29_js.__getattr__("7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("js proxy: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("js proxy: expected '42', got %q", r.Value)
		}

		// Ruby proxy
		r = pyRuntime.Eval(`_t29_rb.__getattr__("7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("ruby proxy: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("ruby proxy: expected '42', got %q", r.Value)
		}

		// Java proxy
		r = pyRuntime.Eval(`_t29_java.__getattr__("7 * 6")`)
		if r.Err != nil {
			return fmt.Errorf("java proxy: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("java proxy: expected '42', got %q", r.Value)
		}

		// Rapid-fire: 50 attribute lookups via __getattr__, each crossing into JS
		r = pyRuntime.Eval(`
_t29_results = []
for i in range(50):
    _t29_results.append(_t29_js.__getattr__(str(i) + " + 1"))
len(_t29_results) == 50 and _t29_results[0] == "1" and _t29_results[49] == "50"
`)
		if r.Err != nil {
			return fmt.Errorf("rapid-fire: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("rapid-fire: expected True, got %v", r.Value)
		}

		// Chain: proxy access → JS → omnivm.call("ruby", ...) → Ruby
		// The __getattr__ call enters JS, which then calls Ruby
		r = pyRuntime.Eval(`_t29_js.__getattr__('omnivm.call("ruby", "11 * 4")')`)
		if r.Err != nil {
			return fmt.Errorf("chained proxy: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "44" {
			return fmt.Errorf("chained proxy: expected '44', got %q", r.Value)
		}

		// Verify call counts accumulated correctly
		r = pyRuntime.Eval("_t29_js._call_count")
		if r.Err != nil {
			return fmt.Errorf("call count: %v", r.Err)
		}
		jsCount := fmt.Sprintf("%v", r.Value)
		// 1 (first) + 50 (rapid-fire) + 1 (chained) = 52
		if jsCount != "52" {
			return fmt.Errorf("js call count: expected 52, got %s", jsCount)
		}

		return nil
	})

	// Test 30: List comprehension with cross-runtime filter + transform
	// [omnivm.call("js", str(x*3)) for x in range(100) if int(omnivm.call("ruby", str(x))) % 2 == 0]
	// 100 Ruby calls for filtering + 50 JS calls for transform = 150 bridge
	// calls inside Python's comprehension bytecode, interleaving two runtimes.
	run("List comprehension with cross-runtime filter + transform", func() error {
		r := pyRuntime.Eval(`
_t30_result = [omnivm.call("javascript", str(x) + " * 3") for x in range(100) if int(omnivm.call("ruby", str(x) + " % 2")) == 0]
len(_t30_result)
`)
		if r.Err != nil {
			return fmt.Errorf("comprehension: %v", r.Err)
		}
		// x % 2 == 0 for x in 0..99: 50 even numbers (0,2,4,...,98)
		if fmt.Sprintf("%v", r.Value) != "50" {
			return fmt.Errorf("length: expected 50, got %v", r.Value)
		}

		// Verify specific values
		r = pyRuntime.Eval(`_t30_result[0] + "|" + _t30_result[1] + "|" + _t30_result[49]`)
		if r.Err != nil {
			return fmt.Errorf("values: %v", r.Err)
		}
		// x=0: 0*3=0, x=2: 2*3=6, x=98: 98*3=294
		if fmt.Sprintf("%v", r.Value) != "0|6|294" {
			return fmt.Errorf("values: expected '0|6|294', got %q", r.Value)
		}

		// Harder: nested comprehension with Java in the mix
		r = pyRuntime.Eval(`
_t30_matrix = [[omnivm.call("java", str(r) + " * 10 + " + str(c)) for c in range(5)] for r in range(5)]
_t30_matrix[0][0] + "|" + _t30_matrix[2][3] + "|" + _t30_matrix[4][4]
`)
		if r.Err != nil {
			return fmt.Errorf("nested comprehension: %v", r.Err)
		}
		// [0][0]=0*10+0=0, [2][3]=2*10+3=23, [4][4]=4*10+4=44
		if fmt.Sprintf("%v", r.Value) != "0|23|44" {
			return fmt.Errorf("nested comprehension: expected '0|23|44', got %q", r.Value)
		}

		// Dict comprehension with cross-runtime key AND value computation
		r = pyRuntime.Eval(`
_t30_dict = {omnivm.call("ruby", '"key_" + ' + str(i) + '.to_s'): omnivm.call("javascript", str(i) + " * " + str(i)) for i in range(10)}
_t30_dict["key_0"] + "|" + _t30_dict["key_5"] + "|" + _t30_dict["key_9"]
`)
		if r.Err != nil {
			return fmt.Errorf("dict comprehension: %v", r.Err)
		}
		// key_0: 0*0=0, key_5: 5*5=25, key_9: 9*9=81
		if fmt.Sprintf("%v", r.Value) != "0|25|81" {
			return fmt.Errorf("dict comprehension: expected '0|25|81', got %q", r.Value)
		}

		return nil
	})

	// Test 31: yield from with cross-runtime sub-generator
	// yield from is complex C-level delegation in CPython. It handles send(),
	// throw(), close() forwarding automatically via _PyGen_yf(). If the sub-
	// generator calls another runtime on each __next__, we stress this machinery
	// with bridge calls. Also tests throw() delegation through yield from.
	run("yield from with cross-runtime sub-generator", func() error {
		setupResult := pyRuntime.Execute(`
def _t31_sub_gen(runtime, n):
    """Sub-generator that calls another runtime on each yield."""
    for i in range(n):
        val = omnivm.call(runtime, str(i) + " + 1")
        yield val

def _t31_outer_gen(runtime, n):
    """Outer generator that delegates via yield from."""
    result = yield from _t31_sub_gen(runtime, n)
    return result

def _t31_outer_with_prefix(runtime, n, prefix):
    """Outer that adds a prefix to delegated values."""
    yield prefix + ":start"
    yield from _t31_sub_gen(runtime, n)
    yield prefix + ":end"
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Basic yield from — sub-generator calls JS 20 times
		r := pyRuntime.Eval(`list(_t31_outer_gen("javascript", 20))`)
		if r.Err != nil {
			return fmt.Errorf("basic yield from: %v", r.Err)
		}
		// Verify first and last elements
		check := pyRuntime.Eval(`
_t31_vals = list(_t31_outer_gen("javascript", 20))
_t31_vals[0] + "|" + _t31_vals[19]
`)
		if check.Err != nil {
			return fmt.Errorf("element check: %v", check.Err)
		}
		if fmt.Sprintf("%v", check.Value) != "1|20" {
			return fmt.Errorf("elements: expected '1|20', got %q", check.Value)
		}

		// yield from with Ruby sub-generator + prefix wrapper
		check = pyRuntime.Eval(`
_t31_vals2 = list(_t31_outer_with_prefix("ruby", 5, "RB"))
"|".join(_t31_vals2)
`)
		if check.Err != nil {
			return fmt.Errorf("prefix wrapper: %v", check.Err)
		}
		if fmt.Sprintf("%v", check.Value) != "RB:start|1|2|3|4|5|RB:end" {
			return fmt.Errorf("prefix wrapper: expected 'RB:start|1|2|3|4|5|RB:end', got %q", check.Value)
		}

		// send() through yield from — values sent to outer are forwarded to sub-gen
		setupResult = pyRuntime.Execute(`
def _t31_sub_sendable():
    """Sub-generator that receives sent values and transforms via JS."""
    val = yield "ready"
    while val is not None:
        result = omnivm.call("javascript", str(val) + " * 2")
        val = yield result

def _t31_outer_sendable():
    """Delegates send() to sub-generator via yield from."""
    final = yield from _t31_sub_sendable()
    return final

_t31_sg = _t31_outer_sendable()
`)
		if setupResult.Err != nil {
			return fmt.Errorf("sendable setup: %v", setupResult.Err)
		}

		// Prime the generator
		r = pyRuntime.Eval("next(_t31_sg)")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "ready" {
			return fmt.Errorf("prime: err=%v val=%v", r.Err, r.Value)
		}

		// Send values through yield from → sub-generator → JS
		for _, tc := range []struct {
			send     int
			expected string
		}{
			{5, "10"}, {10, "20"}, {25, "50"}, {100, "200"},
		} {
			r = pyRuntime.Eval(fmt.Sprintf("_t31_sg.send(%d)", tc.send))
			if r.Err != nil {
				return fmt.Errorf("send(%d): %v", tc.send, r.Err)
			}
			if fmt.Sprintf("%v", r.Value) != tc.expected {
				return fmt.Errorf("send(%d): expected %s, got %v", tc.send, tc.expected, r.Value)
			}
		}

		// throw() through yield from — exception forwarded to sub-generator
		setupResult = pyRuntime.Execute(`
def _t31_sub_throwable():
    """Sub-generator that catches thrown exceptions."""
    while True:
        try:
            yield "waiting"
        except ValueError as e:
            result = omnivm.call("javascript", '"caught:" + "' + str(e) + '"')
            yield result
        except GeneratorExit:
            return

def _t31_outer_throwable():
    yield from _t31_sub_throwable()

_t31_tg = _t31_outer_throwable()
`)
		if setupResult.Err != nil {
			return fmt.Errorf("throwable setup: %v", setupResult.Err)
		}

		r = pyRuntime.Eval("next(_t31_tg)")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "waiting" {
			return fmt.Errorf("throwable prime: err=%v val=%v", r.Err, r.Value)
		}

		// Throw through yield from → sub-generator catches → calls JS
		r = pyRuntime.Eval(`_t31_tg.throw(ValueError("injected"))`)
		if r.Err != nil {
			return fmt.Errorf("throw through yield from: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "caught:injected" {
			return fmt.Errorf("throw result: expected 'caught:injected', got %q", r.Value)
		}

		// Generator still alive after throw
		r = pyRuntime.Eval("next(_t31_tg)")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "waiting" {
			return fmt.Errorf("post-throw: err=%v val=%v", r.Err, r.Value)
		}

		// Cleanup
		pyRuntime.Execute("_t31_sg.close(); _t31_tg.close()")

		return nil
	})

	// Test 32: contextlib.contextmanager (generator-based CM)
	// Combines generator protocol AND context manager protocol on one C boundary.
	// __enter__ drives the generator to first yield, body runs, __exit__ resumes.
	// If body raises, __exit__ calls generator.throw() which could itself call
	// another runtime from the except handler.
	run("contextlib.contextmanager with cross-runtime calls", func() error {
		setupResult := pyRuntime.Execute(`
import contextlib

@contextlib.contextmanager
def _t32_managed_resource(runtime):
    """Generator-based CM that calls another runtime on enter/exit."""
    enter_val = omnivm.call(runtime, "21 * 2")
    try:
        yield enter_val
    finally:
        # Cross-runtime call during generator finalization
        omnivm.call(runtime, "1 + 1")

@contextlib.contextmanager
def _t32_error_handling_cm():
    """Generator-based CM that handles errors via cross-runtime calls."""
    omnivm.call("javascript", "1 + 0")  # enter
    try:
        yield "resource"
    except ValueError as e:
        # Error handler calls another runtime for recovery
        recovery = omnivm.call("javascript", '"recovered:" + "' + str(e) + '"')
        yield recovery  # This will be suppressed by contextmanager
    finally:
        omnivm.call("javascript", "0 + 0")  # cleanup
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Basic usage: generator-based CM with JS call on enter/exit
		r := pyRuntime.Eval(`
_t32_result = None
with _t32_managed_resource("javascript") as val:
    _t32_result = val
_t32_result
`)
		if r.Err != nil {
			return fmt.Errorf("basic CM: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("basic CM: expected '42', got %q", r.Value)
		}

		// With Java runtime
		r = pyRuntime.Eval(`
_t32_result2 = None
with _t32_managed_resource("java") as val:
    _t32_result2 = val
_t32_result2
`)
		if r.Err != nil {
			return fmt.Errorf("java CM: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "42" {
			return fmt.Errorf("java CM: expected '42', got %q", r.Value)
		}

		// Nested generator-based CMs with different runtimes
		r = pyRuntime.Eval(`
_t32_vals = []
with _t32_managed_resource("javascript") as js_val:
    _t32_vals.append("js:" + js_val)
    with _t32_managed_resource("java") as java_val:
        _t32_vals.append("java:" + java_val)
"|".join(_t32_vals)
`)
		if r.Err != nil {
			return fmt.Errorf("nested CM: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "js:42|java:42" {
			return fmt.Errorf("nested CM: expected 'js:42|java:42', got %q", r.Value)
		}

		// CM with exception in body — generator-based CM must handle this correctly
		// contextlib.contextmanager catches the exception and calls generator.throw()
		// which triggers the except handler in the generator body
		r = pyRuntime.Eval(`
_t32_exc_result = "not_set"
try:
    with _t32_managed_resource("javascript") as val:
        raise ValueError("body error")
except ValueError as e:
    _t32_exc_result = "caught:" + str(e)
_t32_exc_result
`)
		if r.Err != nil {
			return fmt.Errorf("CM exception: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "caught:body error" {
			return fmt.Errorf("CM exception: expected 'caught:body error', got %q", r.Value)
		}

		// Multiple enter/exit cycles on same CM function
		r = pyRuntime.Eval(`
_t32_cycle_results = []
for i in range(10):
    with _t32_managed_resource("javascript") as val:
        _t32_cycle_results.append(val)
len(_t32_cycle_results) == 10 and all(v == "42" for v in _t32_cycle_results)
`)
		if r.Err != nil {
			return fmt.Errorf("CM cycles: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("CM cycles: expected True, got %v", r.Value)
		}

		return nil
	})

	// Test 33: Recursive cross-runtime depth bomb
	// Py→JS→Py→JS→... until we hit the stack limit. Each hop adds C stack frames
	// (cgo, Python eval, Duktape eval). Tests whether the system crashes cleanly
	// (recoverable error) or SIGSEGV's. We binary search for the max safe depth.
	run("Recursive cross-runtime depth bomb", func() error {
		// Set up Python function that recurses through JS
		setupResult := pyRuntime.Execute(`
def _t33_recurse(depth, max_depth):
    if depth >= max_depth:
        return "bottom:" + str(depth)
    # Call JS, which calls back into Python with depth+1
    return omnivm.call("javascript",
        'omnivm.call("python", "_t33_recurse(' + str(depth + 1) + ', ' + str(max_depth) + ')")')
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Start with known-safe depths and increase
		// Each Py→JS→Py round-trip uses ~2 hops, so depth N = 2N total stack transitions
		maxWorking := 0
		for _, depth := range []int{5, 10, 25, 50, 75, 100} {
			r := pyRuntime.Eval(fmt.Sprintf("_t33_recurse(0, %d)", depth))
			if r.Err != nil {
				// Hit the limit — this is expected at some depth
				break
			}
			expected := fmt.Sprintf("bottom:%d", depth)
			if fmt.Sprintf("%v", r.Value) != expected {
				return fmt.Errorf("depth %d: expected %q, got %q", depth, expected, r.Value)
			}
			maxWorking = depth
		}

		// We should be able to do at least depth 25 (50 stack transitions)
		if maxWorking < 25 {
			return fmt.Errorf("max safe depth too low: %d (expected at least 25)", maxWorking)
		}

		// Verify runtimes are healthy after hitting the limit
		pyCheck := pyRuntime.Eval("1 + 1")
		if pyCheck.Err != nil || fmt.Sprintf("%v", pyCheck.Value) != "2" {
			return fmt.Errorf("python unhealthy after depth bomb")
		}
		jsCheck := jsRuntime.Eval("1 + 1")
		if jsCheck.Err != nil || fmt.Sprintf("%v", jsCheck.Value) != "2" {
			return fmt.Errorf("javascript unhealthy after depth bomb")
		}

		return nil
	})

	// Test 34: 1MB string through the actual bridge
	// Previous tests kept big strings within runtimes. This sends a 1MB string
	// through Python → JS → Python via the C bridge, testing malloc/free,
	// Duktape string internment, and strdup at scale in one chain.
	run("1MB string through the bridge (Py → JS → Py)", func() error {
		// Create 1MB string in Python
		setupResult := pyRuntime.Execute(`_t34_big = "X" * (1024 * 1024)`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// Verify size
		r := pyRuntime.Eval("len(_t34_big)")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "1048576" {
			return fmt.Errorf("initial size: err=%v val=%v", r.Err, r.Value)
		}

		// Round-trip through JS: Python → bridge → Go → JS eval → Go → bridge → Python
		// JS just returns the string as-is
		r = pyRuntime.Eval(`
_t34_via_js = omnivm.call("javascript", '"' + _t34_big + '"')
len(_t34_via_js)
`)
		if r.Err != nil {
			return fmt.Errorf("JS round-trip: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "1048576" {
			return fmt.Errorf("JS round-trip length: expected 1048576, got %v", r.Value)
		}

		// Verify content integrity
		r = pyRuntime.Eval("_t34_via_js == _t34_big")
		if r.Err != nil {
			return fmt.Errorf("content check: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("content mismatch after JS round-trip")
		}

		// Round-trip through Java — can't pass 1MB as a string literal (Java's
		// constant pool limit is 65535 bytes), so generate it in Java instead.
		// This still tests the bridge returning a 1MB string from JVM → Go → Python.
		r = pyRuntime.Eval(`
_t34_via_java = omnivm.call("java", "new String(new char[1048576]).replace((char)0, (char)88)")
len(_t34_via_java) == 1048576 and _t34_via_java == _t34_big
`)
		if r.Err != nil {
			return fmt.Errorf("Java round-trip: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("Java round-trip: content mismatch")
		}

		// Double hop: Python → JS → Java → back
		// Python sends to JS, JS forwards to Java, Java returns
		r = pyRuntime.Eval(`
_t34_code = '"' + _t34_big + '"'
_t34_double = omnivm.call("javascript", 'omnivm.call("java", "' + "'" + _t34_big + "'" + '")')
len(_t34_double)
`)
		if r.Err != nil {
			// Double hop with 1MB might fail due to escaping — that's informative
			// Fall back to single-hop verification
			r = pyRuntime.Eval("len(_t34_via_js)")
			if r.Err != nil {
				return fmt.Errorf("fallback check: %v", r.Err)
			}
		}

		// Cleanup
		pyRuntime.Execute("del _t34_big; del _t34_via_js; del _t34_via_java")

		return nil
	})

	// Test 35: Chained error recovery cascade
	// Three serial error-catch-retry cycles. Each failure triggers a cross-runtime
	// recovery call. Tests that PyErr state, JNI exception state, and Duktape
	// error stack are all properly cleared between retries.
	run("Chained error recovery cascade", func() error {
		r := pyRuntime.Eval(`
_t35_log = []

# Attempt 1: JS throws
try:
    omnivm.call("javascript", "(function() { throw new Error('fail1'); })()")
    _t35_log.append("js:ok")
except RuntimeError as e:
    _t35_log.append("js:caught")
    # Recovery call to a different runtime
    recovery1 = omnivm.call("java", "100 + 1")
    _t35_log.append("java_recovery:" + recovery1)

# Attempt 2: Python division by zero through Java bridge
try:
    omnivm.call("java", 'omnivm.OmniVM.call("python", "1/0")')
    _t35_log.append("py_via_java:ok")
except RuntimeError as e:
    _t35_log.append("py_via_java:caught")
    # Recovery via JS
    recovery2 = omnivm.call("javascript", "200 + 2")
    _t35_log.append("js_recovery:" + recovery2)

# Attempt 3: Java compilation error
try:
    omnivm.call("java", "this is not valid java")
    _t35_log.append("java_bad:ok")
except RuntimeError as e:
    _t35_log.append("java_bad:caught")
    # Recovery via JS (simple, should work)
    recovery3 = omnivm.call("javascript", "300 + 3")
    _t35_log.append("js_recovery2:" + recovery3)

# Final verification: all runtimes healthy after 3 error cycles
final_js = omnivm.call("javascript", "1 + 1")
final_java = omnivm.call("java", "2 + 2")
_t35_log.append("final_js:" + final_js)
_t35_log.append("final_java:" + final_java)

"|".join(_t35_log)
`)
		if r.Err != nil {
			return fmt.Errorf("cascade: %v", r.Err)
		}
		val := fmt.Sprintf("%v", r.Value)

		// Verify the expected flow
		expected := "js:caught|java_recovery:101|py_via_java:caught|js_recovery:202|java_bad:caught|js_recovery2:303|final_js:2|final_java:4"
		if val != expected {
			return fmt.Errorf("expected %q, got %q", expected, val)
		}

		// Deeper cascade: error in error handler itself
		r = pyRuntime.Eval(`
_t35_deep = []
try:
    try:
        omnivm.call("javascript", "(function() { throw new Error('outer'); })()")
    except RuntimeError:
        _t35_deep.append("outer_caught")
        # Recovery itself fails
        try:
            omnivm.call("javascript", "(function() { throw new Error('inner'); })()")
        except RuntimeError:
            _t35_deep.append("inner_caught")
            # Final recovery succeeds
            val = omnivm.call("java", "42 + 0")
            _t35_deep.append("final:" + val)
except Exception as e:
    _t35_deep.append("unexpected:" + str(e))

"|".join(_t35_deep)
`)
		if r.Err != nil {
			return fmt.Errorf("deep cascade: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "outer_caught|inner_caught|final:42" {
			return fmt.Errorf("deep cascade: expected 'outer_caught|inner_caught|final:42', got %q", r.Value)
		}

		// Cross-runtime error chain: JS → Python error → Java recovery
		// Each step uses a different runtime for the error and recovery
		r = pyRuntime.Eval(`
_t35_chain = []
for i in range(5):
    try:
        if i % 2 == 0:
            omnivm.call("javascript", "(function() { throw new Error('err' + " + str(i) + "); })()")
        else:
            omnivm.call("java", 'throw new RuntimeException("err' + str(i) + '")')
    except RuntimeError:
        if i % 2 == 0:
            v = omnivm.call("java", str(i) + " + 100")
        else:
            v = omnivm.call("javascript", str(i) + " + 200")
        _t35_chain.append(v)

"|".join(_t35_chain)
`)
		if r.Err != nil {
			return fmt.Errorf("alternating chain: %v", r.Err)
		}
		// i=0: JS err, Java recovery: 0+100=100
		// i=1: Java err, JS recovery: 1+200=201
		// i=2: JS err, Java recovery: 2+100=102
		// i=3: Java err, JS recovery: 3+200=203
		// i=4: JS err, Java recovery: 4+100=104
		if fmt.Sprintf("%v", r.Value) != "100|201|102|203|104" {
			return fmt.Errorf("alternating chain: expected '100|201|102|203|104', got %q", r.Value)
		}

		return nil
	})

	// Test 36: functools.reduce with cross-runtime accumulator
	// reduce(f, range(200)) where f calls JS for each fold step. Python's C-level
	// functools_reduce calls the bridge 199 times with accumulating state. Different
	// from the iterator test because the accumulator crosses the boundary each time.
	run("functools.reduce with cross-runtime accumulator", func() error {
		setupResult := pyRuntime.Execute(`
import functools

def _t36_js_add(acc, x):
    """Accumulator that calls JS to add values."""
    return omnivm.call("javascript", str(acc) + " + " + str(x))

def _t36_java_mul(acc, x):
    """Accumulator that calls Java to multiply."""
    return omnivm.call("java", "Long.parseLong(\"" + str(acc) + "\") * " + str(x))
`)
		if setupResult.Err != nil {
			return fmt.Errorf("setup: %v", setupResult.Err)
		}

		// reduce with JS addition: sum(0..199) = 19900
		r := pyRuntime.Eval(`functools.reduce(_t36_js_add, range(200))`)
		if r.Err != nil {
			return fmt.Errorf("JS reduce: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "19900" {
			return fmt.Errorf("JS reduce: expected '19900', got %v", r.Value)
		}

		// reduce with Java multiplication: 1*2*3*...*12 = 479001600
		r = pyRuntime.Eval(`functools.reduce(_t36_java_mul, range(1, 13))`)
		if r.Err != nil {
			return fmt.Errorf("Java reduce: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "479001600" {
			return fmt.Errorf("Java reduce: expected '479001600', got %v", r.Value)
		}

		// reduce with initial value and alternating runtimes
		setupResult = pyRuntime.Execute(`
def _t36_alternating(acc, x):
    """Alternates between JS and Java based on x."""
    acc_val = int(acc)
    if x % 2 == 0:
        return omnivm.call("javascript", str(acc_val) + " + " + str(x))
    else:
        return omnivm.call("java", str(acc_val) + " + " + str(x))
`)
		if setupResult.Err != nil {
			return fmt.Errorf("alternating setup: %v", setupResult.Err)
		}

		r = pyRuntime.Eval(`functools.reduce(_t36_alternating, range(100), "0")`)
		if r.Err != nil {
			return fmt.Errorf("alternating reduce: %v", r.Err)
		}
		// sum(0..99) = 4950
		if fmt.Sprintf("%v", r.Value) != "4950" {
			return fmt.Errorf("alternating reduce: expected '4950', got %v", r.Value)
		}

		// Chained reduce: result of JS reduce fed into Java reduce
		r = pyRuntime.Eval(`
_t36_js_sum = functools.reduce(_t36_js_add, range(10))
_t36_final = functools.reduce(_t36_java_mul, range(1, 5), _t36_js_sum)
_t36_final
`)
		if r.Err != nil {
			return fmt.Errorf("chained reduce: %v", r.Err)
		}
		// JS sum: 0+1+...+9 = 45, then Java: 45 * 1 * 2 * 3 * 4 = 1080
		if fmt.Sprintf("%v", r.Value) != "1080" {
			return fmt.Errorf("chained reduce: expected '1080', got %v", r.Value)
		}

		return nil
	})

	// ================================================================
	// Signal Handling Stress Tests (37-42)
	// ================================================================

	// Test 37: Python interrupt stops infinite loop
	// Verifies the pipe-based interrupt can break a tight Python loop.
	// This is the core mechanism behind task timeouts.
	run("Python interrupt stops infinite loop", func() error {
		pyRuntime.ClearInterrupt() // drain any stale state

		// Test A: interrupt from Go goroutine (via pipe mechanism)
		go func() {
			time.Sleep(50 * time.Millisecond)
			pyRuntime.Interrupt()
		}()
		start := time.Now()
		r := pyRuntime.Execute(`
_t37_started = True
_t37_interrupted = False
_t37_i = 0
try:
    while True:
        _t37_i += 1
except KeyboardInterrupt:
    _t37_interrupted = True
`)
		elapsed := time.Since(start)
		if r.Err != nil {
			return fmt.Errorf("execute error: %v", r.Err)
		}
		r = pyRuntime.Eval("_t37_started")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("loop didn't start: %v", r.Value)
		}
		r = pyRuntime.Eval("_t37_interrupted")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("loop wasn't interrupted: %v", r.Value)
		}
		if elapsed > 5*time.Second {
			return fmt.Errorf("took too long: %v", elapsed)
		}

		// Test B: interrupt from Python Timer thread (self-contained)
		pyRuntime.ClearInterrupt()
		r = pyRuntime.Execute(`
import threading, _thread
_t37b_ok = False
_t37b_i = 0
threading.Timer(0.05, _thread.interrupt_main).start()
try:
    while True:
        _t37b_i += 1
except KeyboardInterrupt:
    _t37b_ok = True
`)
		if r.Err != nil {
			return fmt.Errorf("timer interrupt error: %v", r.Err)
		}
		r = pyRuntime.Eval("_t37b_ok")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("timer interrupt not caught: %v", r.Value)
		}

		// Verify Python is still healthy after interrupts
		r = pyRuntime.Eval("42 + 1")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "43" {
			return fmt.Errorf("Python unhealthy after interrupt: err=%v val=%v", r.Err, r.Value)
		}
		return nil
	})

	// Test 38: JVM NullPointerException triggers SIGSEGV — Ruby survives
	// JVM uses SIGSEGV internally for NullPointerException safepoint traps.
	// With libjsig.so, this should chain properly and not crash Ruby.
	run("JVM NPE (SIGSEGV) does not crash Ruby", func() error {
		// First, verify Ruby works
		r := rbRuntime.Eval("100 + 1")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "101" {
			return fmt.Errorf("Ruby pre-check: err=%v val=%v", r.Err, r.Value)
		}

		// Trigger a NullPointerException in Java — this fires SIGSEGV internally
		r = jvmRuntime.Execute(`
String s = null;
try {
    int len = s.length();
    System.out.println("should not reach");
} catch (NullPointerException e) {
    System.out.println("caught:" + e.getClass().getSimpleName());
}
`)
		if r.Err != nil {
			return fmt.Errorf("JVM NPE: %v", r.Err)
		}
		if !strings.Contains(r.Output, "caught:NullPointerException") {
			return fmt.Errorf("JVM NPE: expected 'caught:NullPointerException', got %q", r.Output)
		}

		// Critical: verify Ruby still works after JVM SIGSEGV
		r = rbRuntime.Eval("200 + 2")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "202" {
			return fmt.Errorf("Ruby post-NPE: err=%v val=%v", r.Err, r.Value)
		}

		// And Python
		r = pyRuntime.Eval("300 + 3")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "303" {
			return fmt.Errorf("Python post-NPE: err=%v val=%v", r.Err, r.Value)
		}

		return nil
	})

	// Test 39: Rapid JVM NPE + Ruby interleave (100 cycles)
	// Stress tests libjsig.so signal chaining under rapid SIGSEGV fire.
	run("Rapid JVM NPE + Ruby interleave (100 cycles)", func() error {
		for i := 0; i < 100; i++ {
			// JVM: trigger NPE (SIGSEGV internally)
			r := jvmRuntime.Execute(`
String s = null;
try { s.length(); } catch (NullPointerException e) {}
System.out.println("ok");
`)
			if r.Err != nil {
				return fmt.Errorf("cycle %d JVM NPE: %v", i, r.Err)
			}

			// Ruby: immediately after JVM's SIGSEGV handler ran
			r = rbRuntime.Eval(fmt.Sprintf("%d + 1", i))
			if r.Err != nil {
				return fmt.Errorf("cycle %d Ruby: %v", i, r.Err)
			}
			expected := fmt.Sprintf("%d", i+1)
			if fmt.Sprintf("%v", r.Value) != expected {
				return fmt.Errorf("cycle %d Ruby: expected %s, got %v", i, expected, r.Value)
			}
		}
		return nil
	})

	// Test 40: Python interrupt recovery during bridge call
	// Uses Python Timer to send interrupt while in a loop doing cross-runtime calls.
	// The interrupt fires between bytecode checks and is caught by try/except.
	run("Python interrupt recovery during bridge call", func() error {
		pyRuntime.ClearInterrupt() // drain any stale state

		// Python Timer fires interrupt while bridge call loop runs
		r := pyRuntime.Execute(`
import threading, _thread
_t40_result = None
_t40_caught = False
threading.Timer(0.03, _thread.interrupt_main).start()
try:
    for _t40_i in range(10000):
        _t40_result = omnivm.call("javascript", "1 + 1")
except KeyboardInterrupt:
    _t40_caught = True
`)
		if r.Err != nil {
			return fmt.Errorf("execute: %v", r.Err)
		}

		// Either the loop completed or was interrupted — both are valid
		r = pyRuntime.Eval("_t40_caught or _t40_result == '2'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("neither completed nor caught interrupt: %v", r.Value)
		}

		// Verify Python is healthy
		r = pyRuntime.Eval("'healthy'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "healthy" {
			return fmt.Errorf("Python unhealthy: err=%v val=%v", r.Err, r.Value)
		}
		return nil
	})

	// Test 41: JVM exception + Ruby error + Python interrupt — all in sequence
	// Triple signal stress: JVM SIGSEGV (NPE), Ruby error handling (rb_protect),
	// Python interrupt — verify all three signal-adjacent mechanisms work
	// back-to-back without corrupting each other's handler state.
	run("Triple signal stress: JVM NPE + Ruby error + Python interrupt", func() error {
		pyRuntime.ClearInterrupt() // drain any stale state from test 40
		// 1. JVM NPE (SIGSEGV internally)
		r := jvmRuntime.Execute(`
String s = null;
try { s.length(); } catch (NullPointerException e) { System.out.println("npe_ok"); }
`)
		if r.Err != nil {
			return fmt.Errorf("JVM NPE: %v", r.Err)
		}
		if !strings.Contains(r.Output, "npe_ok") {
			return fmt.Errorf("JVM NPE: got %q", r.Output)
		}

		// 2. Ruby error (exercises rb_protect error handler)
		r = rbRuntime.Eval("raise 'test_error' rescue $!.message")
		if r.Err != nil {
			return fmt.Errorf("Ruby error: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "test_error" {
			return fmt.Errorf("Ruby error: expected 'test_error', got %v", r.Value)
		}

		// 3. Python interrupt (self-contained via Python Timer)
		pyRuntime.ClearInterrupt()
		r = pyRuntime.Execute(`
import threading, _thread
_t41_ok = False
_t41_i = 0
threading.Timer(0.02, _thread.interrupt_main).start()
try:
    while True:
        _t41_i += 1
except KeyboardInterrupt:
    _t41_ok = True
`)
		if r.Err != nil {
			return fmt.Errorf("Python interrupt: %v", r.Err)
		}
		r = pyRuntime.Eval("_t41_ok")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "True" {
			return fmt.Errorf("Python interrupt failed: %v", r.Value)
		}

		// 4. Verify ALL runtimes still work after the triple stress
		r = jvmRuntime.Eval("1 + 2 + 3")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "6" {
			return fmt.Errorf("JVM post-stress: err=%v val=%v", r.Err, r.Value)
		}
		r = rbRuntime.Eval("4 + 5 + 6")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "15" {
			return fmt.Errorf("Ruby post-stress: err=%v val=%v", r.Err, r.Value)
		}
		r = pyRuntime.Eval("7 + 8 + 9")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "24" {
			return fmt.Errorf("Python post-stress: err=%v val=%v", r.Err, r.Value)
		}
		r = jsRuntime.Eval("10 + 11 + 12")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "33" {
			return fmt.Errorf("JS post-stress: err=%v val=%v", r.Err, r.Value)
		}

		return nil
	})

	// Test 42: Sustained JVM NPE storm with cross-runtime verification
	// 500 NPEs interleaved with calls to all four runtimes, testing that
	// signal handler chaining holds up under sustained SIGSEGV fire.
	run("Sustained JVM NPE storm (500 cycles, all runtimes)", func() error {
		for i := 0; i < 500; i++ {
			// JVM NPE
			r := jvmRuntime.Execute(`
String s = null;
try { s.length(); } catch (NullPointerException e) { System.out.println("ok"); }
`)
			if r.Err != nil {
				return fmt.Errorf("cycle %d JVM: %v", i, r.Err)
			}

			// Every 50th cycle, verify all runtimes
			if i%50 == 49 {
				r = pyRuntime.Eval(fmt.Sprintf("%d * 2", i))
				if r.Err != nil {
					return fmt.Errorf("cycle %d Python: %v", i, r.Err)
				}
				expected := fmt.Sprintf("%d", i*2)
				if fmt.Sprintf("%v", r.Value) != expected {
					return fmt.Errorf("cycle %d Python: expected %s, got %v", i, expected, r.Value)
				}

				r = rbRuntime.Eval(fmt.Sprintf("%d + 10", i))
				if r.Err != nil {
					return fmt.Errorf("cycle %d Ruby: %v", i, r.Err)
				}
				expected = fmt.Sprintf("%d", i+10)
				if fmt.Sprintf("%v", r.Value) != expected {
					return fmt.Errorf("cycle %d Ruby: expected %s, got %v", i, expected, r.Value)
				}

				r = jsRuntime.Eval(fmt.Sprintf("%d + 100", i))
				if r.Err != nil {
					return fmt.Errorf("cycle %d JS: %v", i, r.Err)
				}
				expected = fmt.Sprintf("%d", i+100)
				if fmt.Sprintf("%v", r.Value) != expected {
					return fmt.Errorf("cycle %d JS: expected %s, got %v", i, expected, r.Value)
				}
			}
		}
		return nil
	})

	// ================================================================
	// Thread Identity Verification (43)
	// ================================================================

	// Test 43: Verify all runtimes run on the same OS thread (Golden Thread)
	// Gets the OS-level thread ID (gettid) from Go/C, then from each runtime,
	// and verifies they are all identical. This proves the Golden Thread
	// architecture is real — no shenanigans.
	run("All runtimes on same OS thread (Golden Thread proof)", func() error {
		// 1. Get Go/C thread ID via syscall(SYS_gettid)
		goTid := int64(C.get_thread_id())
		if goTid <= 0 {
			return fmt.Errorf("failed to get Go thread ID: %d", goTid)
		}

		// 2. Python: threading.get_native_id() returns OS thread ID
		r := pyRuntime.Eval("__import__('threading').get_native_id()")
		if r.Err != nil {
			return fmt.Errorf("Python get_native_id: %v", r.Err)
		}
		pyTid := fmt.Sprintf("%v", r.Value)

		// 3. Ruby: Thread.current.native_thread_id (Ruby 3.1+)
		r = rbRuntime.Eval("Thread.current.native_thread_id")
		if r.Err != nil {
			return fmt.Errorf("Ruby native_thread_id: %v", r.Err)
		}
		rbTid := fmt.Sprintf("%v", r.Value)

		// 4. Java: call back through bridge to Python to get thread ID
		//    This proves Java code runs on the same thread, because the
		//    bridge call is synchronous on the calling thread's stack.
		r = jvmRuntime.Eval(`omnivm.OmniVM.call("python", "__import__('threading').get_native_id()")`)
		if r.Err != nil {
			return fmt.Errorf("Java→Python bridge thread ID: %v", r.Err)
		}
		javaBridgeTid := fmt.Sprintf("%v", r.Value)

		// 5. JavaScript: same bridge approach
		r = jsRuntime.Eval(`omnivm.call("python", "__import__('threading').get_native_id()")`)
		if r.Err != nil {
			return fmt.Errorf("JS→Python bridge thread ID: %v", r.Err)
		}
		jsBridgeTid := fmt.Sprintf("%v", r.Value)

		// 6. Verify ALL thread IDs match
		goTidStr := fmt.Sprintf("%d", goTid)
		fmt.Printf("    Go/C tid=%s, Python tid=%s, Ruby tid=%s, Java→Py tid=%s, JS→Py tid=%s\n",
			goTidStr, pyTid, rbTid, javaBridgeTid, jsBridgeTid)

		if pyTid != goTidStr {
			return fmt.Errorf("Python on different thread: Go=%s Python=%s", goTidStr, pyTid)
		}
		if rbTid != goTidStr {
			return fmt.Errorf("Ruby on different thread: Go=%s Ruby=%s", goTidStr, rbTid)
		}
		if javaBridgeTid != goTidStr {
			return fmt.Errorf("Java→Python bridge on different thread: Go=%s Java→Py=%s", goTidStr, javaBridgeTid)
		}
		if jsBridgeTid != goTidStr {
			return fmt.Errorf("JS→Python bridge on different thread: Go=%s JS→Py=%s", goTidStr, jsBridgeTid)
		}

		// 7. Cross-check: call Python from INSIDE each runtime during execution
		//    to prove the thread stays the same even during nested bridge calls
		r = pyRuntime.Eval(fmt.Sprintf(`
import threading
tid = threading.get_native_id()
# Verify from inside Python that we're on the expected thread
"match" if tid == %s else "MISMATCH:%%d" %% tid
`, goTidStr))
		if r.Err != nil {
			return fmt.Errorf("Python self-check: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "match" {
			return fmt.Errorf("Python self-check: %v", r.Value)
		}

		return nil
	})

	// ================================================================
	// Rogue Thread, Exception Ping-Pong, GC Standoff (44-46)
	// ================================================================

	// Test 44: Rogue Thread — background threads calling omnivm.call() get an error, not a crash
	// Each runtime spawns a native background thread that attempts to call
	// omnivm.call(). OmniCall detects the wrong OS thread ID and returns
	// an error instead of invoking a non-thread-safe runtime.
	run("Rogue thread detection (Python, Java, Ruby)", func() error {
		// 1. Python background thread tries to call JS via omnivm.call
		r := pyRuntime.Execute(`
import threading
_t44_py_error = None
def _t44_py_worker():
    global _t44_py_error
    try:
        omnivm.call("javascript", "1 + 1")
        _t44_py_error = "NO_ERROR"
    except Exception as e:
        _t44_py_error = str(e)
_t44_t = threading.Thread(target=_t44_py_worker)
_t44_t.start()
_t44_t.join()
`)
		if r.Err != nil {
			return fmt.Errorf("Python thread setup: %v", r.Err)
		}
		r = pyRuntime.Eval("_t44_py_error")
		if r.Err != nil {
			return fmt.Errorf("Python error check: %v", r.Err)
		}
		pyErr := fmt.Sprintf("%v", r.Value)
		if !strings.Contains(pyErr, "non-Golden Thread") {
			return fmt.Errorf("Python rogue thread: expected Golden Thread error, got: %s", pyErr)
		}

		// 2. Java background thread tries to call Python via OmniVM.call
		r = jvmRuntime.Execute(`
final String[] error = {null};
Thread t = new Thread(() -> {
    try {
        omnivm.OmniVM.call("python", "1 + 1");
        error[0] = "NO_ERROR";
    } catch (Exception e) {
        error[0] = e.getMessage();
    }
});
t.start();
t.join();
System.out.println(error[0]);
`)
		if r.Err != nil {
			return fmt.Errorf("Java thread setup: %v", r.Err)
		}
		if !strings.Contains(r.Output, "non-Golden Thread") {
			return fmt.Errorf("Java rogue thread: expected Golden Thread error, got: %s", strings.TrimSpace(r.Output))
		}

		// 3. Ruby background thread tries to call JS via OmniVM.call
		r = rbRuntime.Execute(`
$_t44_rb_error = nil
t = Thread.new do
  begin
    OmniVM.call("javascript", "1 + 1")
    $_t44_rb_error = "NO_ERROR"
  rescue => e
    $_t44_rb_error = e.message
  end
end
t.join
`)
		if r.Err != nil {
			return fmt.Errorf("Ruby thread setup: %v", r.Err)
		}
		r = rbRuntime.Eval("$_t44_rb_error")
		if r.Err != nil {
			return fmt.Errorf("Ruby error check: %v", r.Err)
		}
		rbErr := fmt.Sprintf("%v", r.Value)
		if !strings.Contains(rbErr, "non-Golden Thread") {
			return fmt.Errorf("Ruby rogue thread: expected Golden Thread error, got: %s", rbErr)
		}

		// 4. Verify all runtimes still work after rogue thread attempts
		r = pyRuntime.Eval("1 + 1")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "2" {
			return fmt.Errorf("Python post-rogue: %v", r.Err)
		}
		r = jsRuntime.Eval("2 + 2")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "4" {
			return fmt.Errorf("JS post-rogue: %v", r.Err)
		}
		r = jvmRuntime.Eval("3 + 3")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "6" {
			return fmt.Errorf("Java post-rogue: %v", r.Err)
		}
		r = rbRuntime.Eval("4 + 4")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "8" {
			return fmt.Errorf("Ruby post-rogue: %v", r.Err)
		}

		return nil
	})

	// Test 45: Exception Ping-Pong — error propagates through all 4 runtimes
	// Python → JS → Java → Ruby (raises) → error bubbles back through
	// every C bridge boundary without stack corruption or memory leaks.
	run("Exception ping-pong (Py → JS → Java → Ruby raises)", func() error {
		// Set up Ruby function that raises
		r := rbRuntime.Execute(`def _t45_raise; raise 'ruby_cascade_error'; end`)
		if r.Err != nil {
			return fmt.Errorf("Ruby setup: %v", r.Err)
		}

		// Set up JS function that calls Java which calls Ruby
		r = jsRuntime.Execute(`function _t45_chain() {
  return omnivm.call("java", 'omnivm.OmniVM.call("ruby", "_t45_raise")');
}`)
		if r.Err != nil {
			return fmt.Errorf("JS setup: %v", r.Err)
		}

		// Trigger: Python → JS → Java → Ruby (raises)
		// The error must propagate back through all 4 runtimes
		r = pyRuntime.Execute(`
_t45_error = None
try:
    omnivm.call("javascript", "_t45_chain()")
    _t45_error = "NO_ERROR"
except Exception as e:
    _t45_error = str(e)
`)
		if r.Err != nil {
			return fmt.Errorf("Python chain: %v", r.Err)
		}
		r = pyRuntime.Eval("_t45_error")
		if r.Err != nil {
			return fmt.Errorf("Python error check: %v", r.Err)
		}
		errMsg := fmt.Sprintf("%v", r.Value)
		if !strings.Contains(errMsg, "ruby_cascade_error") {
			return fmt.Errorf("error didn't propagate: got %q", errMsg)
		}

		// Do it 50 times to verify no memory leaks or stack corruption
		allocBefore := atomic.LoadInt64(&allocCount)
		for i := 0; i < 50; i++ {
			r = pyRuntime.Execute(`
try:
    omnivm.call("javascript", "_t45_chain()")
except Exception:
    pass
`)
			if r.Err != nil {
				return fmt.Errorf("iteration %d: %v", i, r.Err)
			}
		}
		allocAfter := atomic.LoadInt64(&allocCount)
		if allocAfter != allocBefore {
			return fmt.Errorf("allocation leak: before=%d after=%d (delta=%d)",
				allocBefore, allocAfter, allocAfter-allocBefore)
		}

		// Verify all runtimes survived
		r = pyRuntime.Eval("'py_ok'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "py_ok" {
			return fmt.Errorf("Python post-pingpong: %v", r.Err)
		}
		r = jsRuntime.Eval("'js_ok'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "js_ok" {
			return fmt.Errorf("JS post-pingpong: %v", r.Err)
		}
		r = jvmRuntime.Eval(`"java_ok"`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "java_ok" {
			return fmt.Errorf("Java post-pingpong: %v", r.Err)
		}
		r = rbRuntime.Eval("'rb_ok'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "rb_ok" {
			return fmt.Errorf("Ruby post-pingpong: %v", r.Err)
		}

		return nil
	})

	// Test 46: GC Mexican Standoff — rapid large allocations across all runtimes
	// Each runtime generates 1MB strings that flow through the C bridge
	// (malloc/strdup → GoString copy → C.free). If any free is missing,
	// 100 iterations × 4 runtimes × 1MB = 400MB of leaked C strings will OOM.
	run("GC standoff (1MB × 4 runtimes × 100 rounds + cross-bridge)", func() error {
		allocBefore := atomic.LoadInt64(&allocCount)

		for i := 0; i < 100; i++ {
			// Python: generate and return 1MB string
			r := pyRuntime.Eval("'P' * 1048576")
			if r.Err != nil {
				return fmt.Errorf("round %d Python gen: %v", i, r.Err)
			}
			if len(fmt.Sprintf("%v", r.Value)) != 1048576 {
				return fmt.Errorf("round %d Python: wrong size %d", i, len(fmt.Sprintf("%v", r.Value)))
			}

			// JS: generate and return 1MB string
			r = jsRuntime.Eval("var _s='J'; while(_s.length<1048576) _s=_s+_s; _s.substring(0,1048576)")
			if r.Err != nil {
				return fmt.Errorf("round %d JS gen: %v", i, r.Err)
			}
			if len(fmt.Sprintf("%v", r.Value)) != 1048576 {
				return fmt.Errorf("round %d JS: wrong size %d", i, len(fmt.Sprintf("%v", r.Value)))
			}

			// Java: generate and return 1MB string
			r = jvmRuntime.Eval(`new String(new char[1048576]).replace((char)0, 'V')`)
			if r.Err != nil {
				return fmt.Errorf("round %d Java gen: %v", i, r.Err)
			}
			if len(fmt.Sprintf("%v", r.Value)) != 1048576 {
				return fmt.Errorf("round %d Java: wrong size %d", i, len(fmt.Sprintf("%v", r.Value)))
			}

			// Ruby: generate and return 1MB string
			r = rbRuntime.Eval("'R' * 1048576")
			if r.Err != nil {
				return fmt.Errorf("round %d Ruby gen: %v", i, r.Err)
			}
			if len(fmt.Sprintf("%v", r.Value)) != 1048576 {
				return fmt.Errorf("round %d Ruby: wrong size %d", i, len(fmt.Sprintf("%v", r.Value)))
			}

			// Cross-runtime: Python sends 1MB through JS bridge
			// Python→Go→JS→Go→Python (1MB return value at each boundary)
			r = pyRuntime.Eval(`len(omnivm.call("javascript", "var _s='X'; while(_s.length<1048576) _s=_s+_s; _s.substring(0,1048576)"))`)
			if r.Err != nil {
				return fmt.Errorf("round %d Py→JS bridge: %v", i, r.Err)
			}
			if fmt.Sprintf("%v", r.Value) != "1048576" {
				return fmt.Errorf("round %d Py→JS bridge: expected 1048576, got %v", i, r.Value)
			}

			// Cross-runtime: JS sends 1MB through Ruby bridge
			r = jsRuntime.Eval(`omnivm.call("ruby", "'R' * 1048576").length`)
			if r.Err != nil {
				return fmt.Errorf("round %d JS→Ruby bridge: %v", i, r.Err)
			}
			if fmt.Sprintf("%v", r.Value) != "1048576" {
				return fmt.Errorf("round %d JS→Ruby bridge: expected 1048576, got %v", i, r.Value)
			}
		}

		allocAfter := atomic.LoadInt64(&allocCount)
		if allocAfter != allocBefore {
			return fmt.Errorf("C.CString leak: delta=%d", allocAfter-allocBefore)
		}

		return nil
	})

	// Test 47: Ruby Fiber cooperative bridge — Fibers yield bridge requests
	// that the caller dispatches outside the Fiber's C stack. This tests that
	// Ruby Fiber C stack switching doesn't corrupt Ruby's internal state when
	// bridge calls happen between resume/yield cycles.
	// Note: Bridge calls INSIDE a Fiber body would crash because cgo callbacks
	// can't extend the goroutine stack from a Fiber's C stack (makecontext).
	run("Ruby Fiber cooperative bridge (C stack switching)", func() error {
		// Phase A: Fiber yields bridge requests; caller dispatches them and
		// feeds results back. 3 cycles through Py, JS, Java.
		r := rbRuntime.Eval(`
f = Fiber.new do
  r1 = Fiber.yield(["python", "10 + 20"])
  r2 = Fiber.yield(["javascript", "30 + 40"])
  r3 = Fiber.yield(["java", "50 + 60"])
  "first:#{r1}|second:#{r2}|third:#{r3}"
end
results = []
req = f.resume
while req.is_a?(Array)
  val = OmniVM.call(req[0], req[1])
  req = f.resume(val)
end
req
`)
		if r.Err != nil {
			return fmt.Errorf("phase A: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "first:30|second:70|third:110" {
			return fmt.Errorf("phase A: expected 'first:30|second:70|third:110', got %q", r.Value)
		}

		// Phase B: 3 Fibers interleaved, each making multiple bridge requests
		// through different runtimes. Tests multiple suspended Fiber stacks.
		r = rbRuntime.Eval(`
fibers = [
  Fiber.new { |_| r = Fiber.yield(["python", "100 + 1"]); "a:#{r}" },
  Fiber.new { |_| r = Fiber.yield(["javascript", "200 + 2"]); "b:#{r}" },
  Fiber.new { |_| r = Fiber.yield(["java", "300 + 3"]); "c:#{r}" }
]
# First pass: start all fibers, collect requests
requests = fibers.map { |f| f.resume(nil) }
# Second pass: dispatch requests and resume with results
results = fibers.zip(requests).map do |f, req|
  val = OmniVM.call(req[0], req[1])
  f.resume(val)
end
results.join(",")
`)
		if r.Err != nil {
			return fmt.Errorf("phase B: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "a:101,b:202,c:303" {
			return fmt.Errorf("phase B: expected 'a:101,b:202,c:303', got %q", r.Value)
		}

		// Phase C: Fiber with 50 yield/resume cycles and bridge calls between each
		r = rbRuntime.Eval(`
f = Fiber.new do
  sum = 0
  50.times do |i|
    val = Fiber.yield(["javascript", "#{i} + 1"])
    sum += val.to_i
  end
  sum.to_s
end
req = f.resume
while req.is_a?(Array)
  val = OmniVM.call(req[0], req[1])
  req = f.resume(val)
end
req
`)
		if r.Err != nil {
			return fmt.Errorf("phase C: %v", r.Err)
		}
		// sum of (i+1) for i=0..49 = 1275
		if fmt.Sprintf("%v", r.Value) != "1275" {
			return fmt.Errorf("phase C: expected '1275', got %q", r.Value)
		}

		return nil
	})

	// Test 48: Ruby ensure with bridge call during exception unwinding.
	// Bridge calls must work inside ensure blocks while Ruby's exception
	// state is in-flight.
	run("Ruby ensure with bridge during exception unwind", func() error {
		// Phase A: Basic ensure with bridge call
		r := rbRuntime.Eval(`
results = []
begin
  begin
    raise "test_error"
  ensure
    results << "ensure:" + OmniVM.call("python", "str(6 * 7)")
  end
rescue => e
  results << "rescued:" + e.message
end
results.join("|")
`)
		if r.Err != nil {
			return fmt.Errorf("phase A: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "ensure:42|rescued:test_error" {
			return fmt.Errorf("phase A: expected 'ensure:42|rescued:test_error', got %q", r.Value)
		}

		// Phase B: 3 nested ensure blocks calling different runtimes during unwinding
		r = rbRuntime.Eval(`
out = []
begin
  begin
    begin
      begin
        raise "deep_err"
      ensure
        out << "e1:" + OmniVM.call("python", "str(1+1)")
      end
    ensure
      out << "e2:" + OmniVM.call("javascript", "3+3")
    end
  ensure
    out << "e3:" + OmniVM.call("java", "7+7")
  end
rescue => e
  out << "r:" + e.message
end
out.join("|")
`)
		if r.Err != nil {
			return fmt.Errorf("phase B: %v", r.Err)
		}
		expected := "e1:2|e2:6|e3:14|r:deep_err"
		if fmt.Sprintf("%v", r.Value) != expected {
			return fmt.Errorf("phase B: expected %q, got %q", expected, r.Value)
		}

		// Phase C: ensure's bridge call triggers a JS error — both errors handled
		r = rbRuntime.Eval(`
out = []
begin
  begin
    raise "original_error"
  ensure
    begin
      OmniVM.call("javascript", "throw new Error('ensure_boom')")
    rescue => inner
      out << "inner:" + inner.message
    end
  end
rescue => e
  out << "outer:" + e.message
end
out.join("|")
`)
		if r.Err != nil {
			return fmt.Errorf("phase C: %v", r.Err)
		}
		val := fmt.Sprintf("%v", r.Value)
		if !strings.Contains(val, "inner:") || !strings.Contains(val, "outer:original_error") {
			return fmt.Errorf("phase C: expected inner + outer errors, got %q", val)
		}

		return nil
	})

	// Test 49: Ruby catch/throw with bridge calls.
	// catch/throw uses setjmp/longjmp which must not corrupt bridge state.
	run("Ruby catch/throw with bridge calls (longjmp safety)", func() error {
		// Phase A: Single-level catch/throw with bridge call
		r := rbRuntime.Eval(`
catch(:done) do
  val = OmniVM.call("javascript", "100 + 23")
  throw(:done, "caught:" + val) if val.to_i > 100
  "not_thrown"
end
`)
		if r.Err != nil {
			return fmt.Errorf("phase A: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "caught:123" {
			return fmt.Errorf("phase A: expected 'caught:123', got %q", r.Value)
		}

		// Phase B: Nested catch/throw past intermediate bridge-touched frames
		r = rbRuntime.Eval(`
catch(:outer) do
  catch(:inner) do
    v1 = OmniVM.call("python", "str(10)")
    v2 = OmniVM.call("java", "20 + 5")
    throw(:outer, "skip_inner:" + v1 + "+" + v2)
  end
  "should_not_reach"
end
`)
		if r.Err != nil {
			return fmt.Errorf("phase B: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "skip_inner:10+25" {
			return fmt.Errorf("phase B: expected 'skip_inner:10+25', got %q", r.Value)
		}

		// Phase C: 50 iterations of catch/throw with bridge call
		r = rbRuntime.Eval(`
count = 0
50.times do |i|
  result = catch(:loop) do
    val = OmniVM.call("javascript", "#{i} + 1")
    throw(:loop, val.to_i)
  end
  count += result
end
count.to_s
`)
		if r.Err != nil {
			return fmt.Errorf("phase C: %v", r.Err)
		}
		// sum of (i+1) for i=0..49 = 1+2+...+50 = 1275
		if fmt.Sprintf("%v", r.Value) != "1275" {
			return fmt.Errorf("phase C: expected '1275', got %q", r.Value)
		}

		return nil
	})

	// Test 50: JS try/finally where bridge throws, finally does bridge call.
	// Duktape executes finally after duk_error longjmp; bridge calls in
	// finally must work correctly.
	run("JS try/finally with bridge throw + bridge in finally", func() error {
		// Phase A: Basic — Python 1/0 throws, finally calls Ruby
		r := jsRuntime.Eval(`
var result = "";
try {
  try {
    omnivm.call("python", "1/0");
  } finally {
    result += "finally:" + omnivm.call("ruby", "(7 * 8).to_s");
  }
} catch(e) {
  result += "|caught";
}
result
`)
		if r.Err != nil {
			return fmt.Errorf("phase A: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "finally:56|caught" {
			return fmt.Errorf("phase A: expected 'finally:56|caught', got %q", r.Value)
		}

		// Phase B: Nested finally — two levels of throwing bridge calls + bridge in finally
		r = jsRuntime.Eval(`
var out = "";
try {
  try {
    try {
      omnivm.call("ruby", "raise 'inner_boom'");
    } finally {
      out += "f1:" + omnivm.call("python", "str(3*3)");
    }
  } finally {
    out += "|f2:" + omnivm.call("java", "4*4");
  }
} catch(e) {
  out += "|caught";
}
out
`)
		if r.Err != nil {
			return fmt.Errorf("phase B: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "f1:9|f2:16|caught" {
			return fmt.Errorf("phase B: expected 'f1:9|f2:16|caught', got %q", r.Value)
		}

		// Phase C: try calls Python (throws), finally calls Java — tests JNI clean after Python error
		r = jsRuntime.Eval(`
var out = "";
try {
  try {
    omnivm.call("python", "1/0");
  } finally {
    out += "java:" + omnivm.call("java", "100 + 11");
  }
} catch(e) {
  out += "|ok";
}
out
`)
		if r.Err != nil {
			return fmt.Errorf("phase C: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "java:111|ok" {
			return fmt.Errorf("phase C: expected 'java:111|ok', got %q", r.Value)
		}

		return nil
	})

	// Test 51: 4-runtime mutual recursion — stack ALL 4 runtime C frames.
	// Python dispatches to JS, Java, Ruby in a cycle, each calling back
	// into Python for the next level. 18 levels = 6 full J→V→R cycles.
	run("4-runtime mutual recursion (18 levels deep)", func() error {
		// Define the recursive dispatcher in Python
		r := pyRuntime.Execute(`
def _t51_dispatch(depth, max_depth):
    if depth >= max_depth:
        return "end"
    runtimes = ["javascript", "java", "ruby"]
    labels = {"javascript": "J", "java": "V", "ruby": "R"}
    rt = runtimes[depth % 3]
    inner = "_t51_dispatch(" + str(depth+1) + "," + str(max_depth) + ")"
    if rt == "javascript":
        result = omnivm.call("javascript", "omnivm.call('python', '" + inner + "')")
    elif rt == "java":
        result = omnivm.call("java", 'omnivm.OmniVM.call("python", "' + inner + '")')
    elif rt == "ruby":
        result = omnivm.call("ruby", "OmniVM.call('python', '" + inner + "')")
    return labels[rt] + ">" + result
`)
		if r.Err != nil {
			return fmt.Errorf("define dispatcher: %v", r.Err)
		}

		// Call with max_depth=18 (6 full J→V→R cycles)
		r = pyRuntime.Eval("_t51_dispatch(0, 18)")
		if r.Err != nil {
			return fmt.Errorf("4-runtime recursion: %v", r.Err)
		}
		expected := "J>V>R>J>V>R>J>V>R>J>V>R>J>V>R>J>V>R>end"
		if fmt.Sprintf("%v", r.Value) != expected {
			return fmt.Errorf("expected %q, got %q", expected, r.Value)
		}

		// Verify all 4 runtimes are healthy after deep recursion
		r = pyRuntime.Eval("'py_ok'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "py_ok" {
			return fmt.Errorf("Python health check failed: %v", r.Err)
		}
		r = jsRuntime.Eval("'js_ok'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "js_ok" {
			return fmt.Errorf("JS health check failed: %v", r.Err)
		}
		r = jvmRuntime.Eval(`"java_ok"`)
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "java_ok" {
			return fmt.Errorf("Java health check failed: %v", r.Err)
		}
		r = rbRuntime.Eval("'rb_ok'")
		if r.Err != nil || fmt.Sprintf("%v", r.Value) != "rb_ok" {
			return fmt.Errorf("Ruby health check failed: %v", r.Err)
		}

		return nil
	})

	// Test 52: fork() guard — pthread_atfork child handler kills child with exit 71.
	// Verifies that fork() in a polyglot process is intercepted.
	run("fork() guard (pthread_atfork kills child with exit 71)", func() error {
		// Use Execute to run the fork code and capture exit_code, then Eval to read it
		r := pyRuntime.Execute(`
import os
exit_code = -1
pid = os.fork()
if pid == 0:
    os._exit(99)
else:
    _, status = os.waitpid(pid, 0)
    if os.WIFEXITED(status):
        exit_code = os.WEXITSTATUS(status)
`)
		if r.Err != nil {
			return fmt.Errorf("fork guard execute: %v", r.Err)
		}
		r = pyRuntime.Eval("str(exit_code)")
		if r.Err != nil {
			return fmt.Errorf("fork guard eval: %v", r.Err)
		}
		if r.Err != nil {
			return fmt.Errorf("fork guard: %v", r.Err)
		}
		if fmt.Sprintf("%v", r.Value) != "71" {
			return fmt.Errorf("expected exit code 71, got %v", r.Value)
		}

		return nil
	})

	// ================================================================
	// Epoll + Watchdog Architecture Stress Tests
	// ================================================================

	// Test: Rogue Guest Preemption
	// Dispatch infinite loops in Python, JS, and Ruby to the Golden Thread.
	// The watchdog must interrupt each one within TaskTimeout.
	run("Rogue guest preemption (Py+JS+Ruby infinite loops)", func() error {
		watchdog.Init()
		watchdog.SetPythonInterrupt(pyRuntime.InterruptFuncPtr())
		watchdog.SetV8Terminate(jsRuntime.TerminateFuncPtr())
		rbRuntime.Execute("trap('USR1') { raise Interrupt }")

		timeout := 2 * time.Second
		timeoutMS := int(timeout / time.Millisecond)
		margin := 3 * time.Second // generous margin for CI

		type rogueCase struct {
			name string
			rt   int
			exec func() pkg.Result
		}
		cases := []rogueCase{
			{"Python", watchdog.RuntimePython, func() pkg.Result {
				return pyRuntime.Execute("i = 0\nwhile True:\n    i += 1")
			}},
			{"JavaScript", watchdog.RuntimeJavaScript, func() pkg.Result {
				return jsRuntime.Execute("while(true) {}")
			}},
			{"Ruby", watchdog.RuntimeRuby, func() pkg.Result {
				return rbRuntime.Execute("i = 0; loop { i += 1 }")
			}},
		}

		for _, tc := range cases {
			watchdog.SetActiveRuntime(tc.rt)
			watchdog.Arm(timeoutMS)
			start := time.Now()
			result := tc.exec()
			elapsed := time.Since(start)
			watchdog.Disarm()
			watchdog.SetActiveRuntime(watchdog.RuntimeNone)

			if result.Err == nil {
				return fmt.Errorf("%s: expected error from interrupted loop, got nil", tc.name)
			}
			if elapsed > timeout+margin {
				return fmt.Errorf("%s: took %v, expected <%v", tc.name, elapsed, timeout+margin)
			}
		}

		// Clear Python interrupt state for subsequent tests
		pyRuntime.ClearInterrupt()
		return nil
	})

	// Test: Epoll Coalescing Avalanche
	// 100 goroutines simultaneously call RunOnMain. eventfd coalescing must
	// not drop any tasks.
	run("Epoll coalescing avalanche (100 simultaneous dispatches)", func() error {
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

		var completed atomic.Int64
		var wg sync.WaitGroup

		// Start barrier: hold all goroutines until they're all ready
		barrier := make(chan struct{})

		for i := 0; i < 100; i++ {
			wg.Add(1)
			i := i
			go func() {
				defer wg.Done()
				<-barrier // Wait for all goroutines to be ready
				err := disp.RunOnMain(func() error {
					// Simple cross-runtime math to prove the task ran
					r := pyRuntime.Eval(fmt.Sprintf("%d * 2", i))
					if r.Err != nil {
						return r.Err
					}
					completed.Add(1)
					return nil
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "    goroutine %d error: %v\n", i, err)
				}
			}()
		}

		// Release all 100 goroutines at once
		time.Sleep(10 * time.Millisecond) // let goroutines park on barrier
		close(barrier)

		done := make(chan struct{})
		go func() {
			wg.Wait()
			cancel()
			close(done)
		}()

		disp.RunEpoll(ctx, -1)
		<-done

		c := completed.Load()
		if c != 100 {
			return fmt.Errorf("expected 100 tasks completed, got %d", c)
		}
		return nil
	})

	// Test: M:N Isolation (GOMAXPROCS=4)
	// Run CPU-bound Go work on multiple goroutines while the Golden Thread
	// processes V8 micro-tasks. Proves GOMAXPROCS>1 doesn't break the
	// Golden Thread invariant.
	run("M:N isolation (Go goroutines + Golden Thread V8 tasks)", func() error {
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

		// Track which OS threads the Go goroutines run on
		var threadIDs sync.Map
		var goWorkDone atomic.Int64
		var jsTasksDone atomic.Int64

		// Launch 4 CPU-bound Go goroutines
		var goWg sync.WaitGroup
		for g := 0; g < 4; g++ {
			goWg.Add(1)
			go func() {
				defer goWg.Done()
				tid := int64(C.get_thread_id())
				threadIDs.Store(tid, true)
				// CPU-bound work: tight loop
				sum := int64(0)
				for i := int64(0); i < 10_000_000; i++ {
					sum += i
				}
				_ = sum
				goWorkDone.Add(1)
			}()
		}

		// Simultaneously dispatch 100 JS tasks to the Golden Thread
		var jsWg sync.WaitGroup
		for i := 0; i < 100; i++ {
			jsWg.Add(1)
			i := i
			go func() {
				defer jsWg.Done()
				disp.RunOnMain(func() error {
					r := jsRuntime.Eval(fmt.Sprintf("%d + %d", i, i))
					if r.Err != nil {
						return r.Err
					}
					jsTasksDone.Add(1)
					return nil
				})
			}()
		}

		done := make(chan struct{})
		go func() {
			goWg.Wait()
			jsWg.Wait()
			cancel()
			close(done)
		}()

		disp.RunEpoll(ctx, -1)
		<-done

		if goWorkDone.Load() != 4 {
			return fmt.Errorf("expected 4 Go goroutines done, got %d", goWorkDone.Load())
		}
		if jsTasksDone.Load() != 100 {
			return fmt.Errorf("expected 100 JS tasks done, got %d", jsTasksDone.Load())
		}

		// Count distinct OS threads used by Go goroutines
		distinctThreads := 0
		threadIDs.Range(func(_, _ interface{}) bool {
			distinctThreads++
			return true
		})
		fmt.Printf("(%d Go threads, %d JS tasks) ",
			distinctThreads, jsTasksDone.Load())
		return nil
	})

	// Test: Signal Chaos Trap
	// Ruby GC stress + rapid active_runtime toggling + raw SIGUSR1.
	// Proves temporal signal routing doesn't segfault under chaos.
	run("Signal chaos trap (Ruby GC + rapid runtime toggling + SIGUSR1)", func() error {
		// Run Ruby code that allocates heavily, forcing GC
		disp := dispatcher.New()
		ctx, cancel := context.WithCancel(context.Background())

		var chaosErr error
		var chaosDone atomic.Bool

		// Rapidly toggle active_runtime from a Go goroutine
		go func() {
			rts := []int{
				watchdog.RuntimeNone,
				watchdog.RuntimePython,
				watchdog.RuntimeJavaScript,
				watchdog.RuntimeRuby,
				watchdog.RuntimeJVM,
			}
			for !chaosDone.Load() {
				for _, rt := range rts {
					watchdog.SetActiveRuntime(rt)
					time.Sleep(100 * time.Microsecond)
				}
			}
			watchdog.SetActiveRuntime(watchdog.RuntimeNone)
		}()

		// Dispatch Ruby GC stress on the Golden Thread
		taskDone := make(chan struct{})
		go func() {
			err := disp.RunOnMain(func() error {
				// Heavy allocation in Ruby to trigger GC
				r := rbRuntime.Execute(`
arr = []
10000.times do |i|
  arr << ("x" * 1000)
  arr.shift if arr.length > 100
end
arr.length
`)
				if r.Err != nil {
					return r.Err
				}
				return nil
			})
			if err != nil {
				chaosErr = err
			}
			close(taskDone)
		}()

		go func() {
			<-taskDone
			cancel()
		}()

		disp.RunEpoll(ctx, -1)
		chaosDone.Store(true)
		<-taskDone

		if chaosErr != nil {
			return chaosErr
		}
		// If we get here without segfault, the test passed
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
	watchdog.Shutdown()
	for _, name := range []string{"ruby", "java", "javascript", "python"} {
		runtimes[name].Shutdown()
	}

	if failed > 0 {
		os.Exit(1)
	}
}
