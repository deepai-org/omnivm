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

// Forward declarations of exported Go functions (cgo drops const qualifiers)
extern char* OmniCall(char* runtime, char* code);
extern void OmniFree(char* ptr);

// Get function pointers to pass to runtimes
static void* get_omni_call_ptr() { return (void*)OmniCall; }
static void* get_omni_free_ptr() { return (void*)OmniFree; }
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
)

func init() {
	// Pin the main goroutine to the main OS thread — the "Golden Thread".
	// All guest runtime interactions (V8, CPython, JVM, Ruby) must happen here.
	runtime.LockOSThread()
}

// runtimes maps language names to their Runtime implementations.
var runtimes = make(map[string]pkg.Runtime)

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
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

	// Task timeout: interrupt Python (other runtimes lack safe interrupt APIs)
	disp.OnTaskTimeout = func() {
		fmt.Fprintf(os.Stderr, "[timeout] Task exceeded %v, interrupting Python...\n",
			disp.TaskTimeout)
		pyRuntime.Interrupt()
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

	fmt.Fprintln(os.Stderr, "Ready.")

	// Start signal handler in background
	go sigMgr.Wait(ctx)

	// Determine execution mode
	dispatcherRunning := false
	switch {
	case *pyCode != "":
		result := pyRuntime.Execute(*pyCode)
		printResult("python", result)
	case *jsCode != "":
		result := jsRuntime.Execute(*jsCode)
		printResult("javascript", result)
	case *javaCode != "":
		result := jvmRuntime.Execute(*javaCode)
		printResult("java", result)
	case *rubyCode != "":
		result := rbRuntime.Execute(*rubyCode)
		printResult("ruby", result)
	case *filePath != "":
		executeFile(*filePath)
	default:
		// Start the dispatcher in background, run REPL on main thread
		// (In this case we run the REPL directly since we're already
		// on the Golden Thread)
		dispatcherRunning = true
		go func() {
			disp.Run(ctx)
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
	for _, name := range []string{"ruby", "java", "javascript", "python"} {
		if r, ok := runtimes[name]; ok {
			fmt.Fprintf(os.Stderr, "[shutdown] %s...\n", name)
			r.Shutdown()
		}
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

	result := r.Execute(code)
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
				result := r.Execute(line)
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
