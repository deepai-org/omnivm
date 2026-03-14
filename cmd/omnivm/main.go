// OmniVM - The Go-Hosted Polyglot Runtime
//
// A single Go binary that embeds Python (CPython), JavaScript (V8),
// Java (JVM/JNI), and Ruby (MRI) runtimes, orchestrated via a Golden
// Thread dispatcher pattern.
//
// Usage:
//
//	omnivm                     Start the interactive REPL
//	omnivm -python "code"      Execute Python code
//	omnivm -js "code"          Execute JavaScript code
//	omnivm -java "code"        Execute Java code (via ScriptEngine)
//	omnivm -ruby "code"        Execute Ruby code
//	omnivm -file script.py     Execute a file (detected by extension)
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
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/dispatcher"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
	"github.com/omnivm/omnivm/pkg/signals"
	"github.com/omnivm/omnivm/pkg/watchdog"
)

func init() {
	// Pin the main goroutine to the main OS thread — the "Golden Thread".
	// All guest runtime interactions (V8, CPython, JVM, Ruby) must happen here.
	runtime.LockOSThread()
}

// runtimes maps language names to their Runtime implementations.
var runtimes = make(map[string]pkg.Runtime)

// goldenThreadID is the OS thread ID of the main goroutine.
var goldenThreadID int64

// taskTimeoutMS is the task timeout in milliseconds, used by the watchdog
// for CLI and REPL paths that bypass the dispatcher.
var taskTimeoutMS int

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
	// Guard: reject calls from non-Golden threads to prevent crashes
	currentTid := int64(C.get_thread_id())
	if currentTid != goldenThreadID {
		return C.CString(fmt.Sprintf("ERR:omnivm.call from non-Golden Thread (tid=%d, expected=%d). "+
			"Cross-runtime calls must originate from the main thread.", currentTid, goldenThreadID))
	}

	rtName := C.GoString(cRuntime)
	code := C.GoString(cCode)

	rt, ok := runtimes[rtName]
	if !ok {
		return C.CString("ERR:unknown runtime: " + rtName)
	}

	evalResult := rt.Eval(code)
	if evalResult.Err != nil {
		return C.CString("ERR:" + evalResult.Err.Error())
	}

	var val string
	if evalResult.Value != nil {
		val = fmt.Sprintf("%v", evalResult.Value)
	} else {
		val = evalResult.Output
	}

	return C.CString(val)
}

//export OmniFree
func OmniFree(ptr *C.char) {
	if ptr != nil {
		C.free(unsafe.Pointer(ptr))
	}
}

func main() {
	goldenThreadID = int64(C.get_thread_id())

	// Parse flags
	pyCode := flag.String("python", "", "Execute Python code")
	jsCode := flag.String("js", "", "Execute JavaScript code")
	javaCode := flag.String("java", "", "Execute Java code")
	rubyCode := flag.String("ruby", "", "Execute Ruby code")
	filePath := flag.String("file", "", "Execute a script file (detected by extension)")
	flag.Parse()

	// Set up signal handling
	sigMgr := signals.NewManager()

	// Create the dispatcher
	disp := dispatcher.New()
	disp.WatchdogTimeout = 5 * time.Second
	disp.OnWatchdogAlert = func(d time.Duration) {
		fmt.Fprintf(os.Stderr, "[watchdog] Golden Thread blocked for >%v\n", d)
	}
	disp.TaskTimeout = 30 * time.Second
	taskTimeoutMS = int(disp.TaskTimeout / time.Millisecond)

	// Create shared memory store
	_ = arrow.NewSharedStore()

	// Create runtimes
	pyRuntime := python.New()
	jsRuntime := javascript.New()
	jvmRuntime := jvm.New()
	rbRuntime := ruby.New()

	runtimes["python"] = pyRuntime
	runtimes["javascript"] = jsRuntime
	runtimes["java"] = jvmRuntime
	runtimes["ruby"] = rbRuntime

	// Task timeout: interrupt Python (other runtimes lack safe interrupt APIs).
	// This is the fallback when no watchdog is available (non-Linux).
	disp.OnTaskTimeout = func() {
		fmt.Fprintf(os.Stderr, "[timeout] Task exceeded %v, interrupting Python...\n",
			disp.TaskTimeout)
		pyRuntime.Interrupt()
	}

	// Watchdog arm/disarm hooks: called around each task execution
	disp.OnTaskStart = func() {
		if disp.TaskTimeout > 0 {
			watchdog.Arm(int(disp.TaskTimeout / time.Millisecond))
		}
	}
	disp.OnTaskEnd = func() {
		watchdog.Disarm()
	}

	// Context for lifecycle management
	ctx, cancel := context.WithCancel(context.Background())

	// On signal, just cancel context; explicit shutdown happens at end of main
	sigMgr.RegisterShutdown("dispatcher", func() {
		cancel()
	})

	// Initialize all runtimes on the Golden Thread
	fmt.Fprintln(os.Stderr, "OmniVM - Go-Hosted Polyglot Runtime")
	fmt.Fprintln(os.Stderr, "Initializing runtimes...")

	initRuntime := func(r pkg.Runtime) {
		if err := r.Initialize(); err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] FAILED: %v\n", r.Name(), err)
		} else {
			fmt.Fprintf(os.Stderr, "  [%s] OK\n", r.Name())
			disp.RegisterPumpCallback(r.Name(), r.Pump)
		}
	}

	initRuntime(pyRuntime)
	initRuntime(jsRuntime)
	initRuntime(jvmRuntime)
	initRuntime(rbRuntime)

	// Install cross-runtime bridge callbacks
	callPtr := uintptr(C.get_omni_call_ptr())
	freePtr := uintptr(C.get_omni_free_ptr())
	for _, rt := range runtimes {
		rt.SetBridgeCallback(callPtr, freePtr)
	}

	// Initialize watchdog thread for runtime-specific preemption
	watchdog.Init()
	if pyInterruptPtr := pyRuntime.InterruptFuncPtr(); pyInterruptPtr != nil {
		watchdog.SetPythonInterrupt(pyInterruptPtr)
	}
	if v8TermPtr := jsRuntime.TerminateFuncPtr(); v8TermPtr != nil {
		watchdog.SetV8Terminate(v8TermPtr)
	}

	// Ruby SIGUSR1 trap: MRI intercepts the signal between opcodes
	// and raises Interrupt. The watchdog sends pthread_kill(golden_tid, SIGUSR1).
	rbRuntime.Execute("trap('USR1') { raise Interrupt }")

	fmt.Fprintln(os.Stderr, "Ready.")

	// Start signal handler in background
	go sigMgr.Wait(ctx)

	// Determine execution mode
	dispatcherRunning := false
	switch {
	case *pyCode != "":
		result := executeWithWatchdog(watchdog.RuntimePython, func() pkg.Result {
			return pyRuntime.Execute(*pyCode)
		})
		printResult("python", result)
	case *jsCode != "":
		result := executeWithWatchdog(watchdog.RuntimeJavaScript, func() pkg.Result {
			return jsRuntime.Execute(*jsCode)
		})
		printResult("javascript", result)
	case *javaCode != "":
		result := executeWithWatchdog(watchdog.RuntimeJVM, func() pkg.Result {
			return jvmRuntime.Execute(*javaCode)
		})
		printResult("java", result)
	case *rubyCode != "":
		result := executeWithWatchdog(watchdog.RuntimeRuby, func() pkg.Result {
			return rbRuntime.Execute(*rubyCode)
		})
		printResult("ruby", result)
	case *filePath != "":
		executeFile(*filePath)
	default:
		// Start the dispatcher with epoll (Linux) or ticker fallback,
		// run REPL on main thread (already on the Golden Thread)
		dispatcherRunning = true
		uvFD := jsRuntime.GetUVBackendFD()
		go func() {
			disp.RunEpoll(ctx, uvFD)
		}()
		repl(ctx)
	}

	// Flush stdout before shutdown messages on stderr
	os.Stdout.Sync()

	// Graceful shutdown: signal dispatcher to stop and wait for it
	cancel()
	if dispatcherRunning {
		disp.WaitForStop()
	}
	fmt.Fprintln(os.Stderr, "\n[shutdown] Shutting down runtimes...")
	watchdog.Shutdown()
	for _, name := range []string{"ruby", "java", "javascript", "python"} {
		if r, ok := runtimes[name]; ok {
			fmt.Fprintf(os.Stderr, "[shutdown] %s...\n", name)
			r.Shutdown()
		}
	}
}

// executeWithWatchdog arms the watchdog, sets the active runtime, executes
// the function, then disarms and clears. Used by CLI one-shots and REPL
// paths that bypass the dispatcher.
func executeWithWatchdog(rt int, fn func() pkg.Result) pkg.Result {
	watchdog.SetActiveRuntime(rt)
	if taskTimeoutMS > 0 {
		watchdog.Arm(taskTimeoutMS)
	}
	result := fn()
	watchdog.Disarm()
	watchdog.SetActiveRuntime(watchdog.RuntimeNone)
	return result
}

// runtimeID maps a language name to a watchdog runtime constant.
func runtimeID(lang string) int {
	switch lang {
	case "python":
		return watchdog.RuntimePython
	case "javascript":
		return watchdog.RuntimeJavaScript
	case "ruby":
		return watchdog.RuntimeRuby
	case "java":
		return watchdog.RuntimeJVM
	default:
		return watchdog.RuntimeNone
	}
}

func printResult(lang string, result pkg.Result) {
	if result.Err != nil {
		fmt.Fprintf(os.Stderr, "[%s] Error: %v\n", lang, result.Err)
		os.Exit(1)
	}
	if result.Output != "" {
		fmt.Print(result.Output)
	}
}

func executeFile(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	code := string(data)
	ext := filepath.Ext(path)

	var lang string
	var r pkg.Runtime
	switch ext {
	case ".py":
		lang = "python"
		r = runtimes["python"]
	case ".js":
		lang = "javascript"
		r = runtimes["javascript"]
	case ".java":
		lang = "java"
		r = runtimes["java"]
	case ".rb":
		lang = "ruby"
		r = runtimes["ruby"]
	default:
		fmt.Fprintf(os.Stderr, "Unknown file extension: %s\n", ext)
		os.Exit(1)
		return
	}

	result := executeWithWatchdog(runtimeID(lang), func() pkg.Result {
		return r.Execute(code)
	})
	printResult(lang, result)
}

func repl(ctx context.Context) {
	scanner := bufio.NewScanner(os.Stdin)
	currentLang := "python" // default

	fmt.Printf("[%s]> ", currentLang)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())

		if line == "" {
			fmt.Printf("[%s]> ", currentLang)
			continue
		}

		// Meta-commands
		switch {
		case line == ":quit" || line == ":q":
			return
		case line == ":python" || line == ":py":
			currentLang = "python"
			fmt.Println("Switched to Python")
		case line == ":javascript" || line == ":js":
			currentLang = "javascript"
			fmt.Println("Switched to JavaScript")
		case line == ":java" || line == ":jvm":
			currentLang = "java"
			fmt.Println("Switched to Java")
		case line == ":ruby" || line == ":rb":
			currentLang = "ruby"
			fmt.Println("Switched to Ruby")
		case line == ":help":
			printHelp()
		case line == ":status":
			printStatus()
		default:
			// Execute code in current language
			r, ok := runtimes[currentLang]
			if !ok {
				fmt.Fprintf(os.Stderr, "Runtime %q not available\n", currentLang)
			} else {
				result := executeWithWatchdog(runtimeID(currentLang), func() pkg.Result {
					return r.Execute(line)
				})
				if result.Err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", result.Err)
				} else if result.Output != "" {
					fmt.Print(result.Output)
				}
			}
		}

		fmt.Printf("[%s]> ", currentLang)
	}
}

func printHelp() {
	fmt.Println(`OmniVM REPL Commands:
  :python, :py       Switch to Python
  :javascript, :js   Switch to JavaScript
  :java, :jvm        Switch to Java
  :ruby, :rb         Switch to Ruby
  :status            Show runtime status
  :help              Show this help
  :quit, :q          Exit`)
}

func printStatus() {
	for name, r := range runtimes {
		result := r.Execute("1")
		status := "OK"
		if result.Err != nil {
			status = "ERROR"
		}
		fmt.Printf("  [%s] %s\n", name, status)
	}
}
