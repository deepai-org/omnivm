// OmniVM - The Go-Hosted Polyglot Runtime
//
// A single binary that embeds Python (CPython), JavaScript (V8),
// Java (JVM/JNI), Ruby (MRI), and Go runtimes.
//
// Usage:
//
//	omnivm                          Start the interactive REPL
//	omnivm run script.py [args...]  Run a file (language detected by extension)
//	omnivm run main.go [args...]    Compile and run Go code
//	omnivm -python "code"           Execute Python code (legacy)
//	omnivm -js "code"               Execute JavaScript code (legacy)
//	omnivm -java "code"             Execute Java code (legacy)
//	omnivm -ruby "code"             Execute Ruby code (legacy)
//	omnivm -file script.py          Execute a file (legacy)
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
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/cli"
	"github.com/omnivm/omnivm/pkg/dispatcher"
	"github.com/omnivm/omnivm/pkg/errmsg"
	golangrt "github.com/omnivm/omnivm/pkg/golang"
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

// goRuntime handles Go file execution via "go run".
var goRuntime = golangrt.New()

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
	rtName := C.GoString(cRuntime)
	code := C.GoString(cCode)

	rt, ok := runtimes[rtName]
	if !ok {
		return C.CString("ERR:unknown runtime: " + rtName)
	}

	// Only manage watchdog for Golden Thread tasks.
	isGolden := int64(C.get_thread_id()) == goldenThreadID
	var prevRT int
	if isGolden {
		prevRT = watchdog.GetActiveRuntime()
		watchdog.SetActiveRuntime(runtimeID(rtName))
	}

	evalResult := rt.Eval(code)

	if isGolden {
		watchdog.SetActiveRuntime(prevRT)
	}

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

	// Parse CLI arguments
	cmd, err := cli.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Go runtime is handled externally — no Golden Thread needed
	if cmd.Language == "go" {
		runGoFile(cmd)
		return
	}

	// Determine which runtimes to initialize (lazy init)
	needed := cli.RequiredRuntimes(cmd)

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

	// Create runtimes — only the ones we need
	pyRuntime := python.New()
	jsRuntime := javascript.New()
	jvmRuntime := jvm.New()
	rbRuntime := ruby.New()

	allRuntimes := map[string]pkg.Runtime{
		"python":     pyRuntime,
		"javascript": jsRuntime,
		"java":       jvmRuntime,
		"ruby":       rbRuntime,
	}

	// Task timeout callback
	disp.OnTaskTimeout = func() {
		fmt.Fprintf(os.Stderr, "[timeout] Task exceeded %v, interrupting Python...\n",
			disp.TaskTimeout)
		pyRuntime.Interrupt()
	}

	// Watchdog arm/disarm hooks
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

	sigMgr.RegisterShutdown("dispatcher", func() {
		cancel()
	})

	// Initialize only needed runtimes (lazy init)
	needSet := make(map[string]bool)
	for _, n := range needed {
		needSet[n] = true
	}

	fmt.Fprintln(os.Stderr, "OmniVM - Go-Hosted Polyglot Runtime")
	if cmd.Mode == ModeREPL {
		fmt.Fprintln(os.Stderr, "Initializing runtimes...")
	} else {
		fmt.Fprintf(os.Stderr, "Initializing %s runtime...\n", cmd.Language)
	}

	// Initialize runtimes in dependency order (Python first for bridge)
	for _, name := range []string{"python", "javascript", "java", "ruby"} {
		if !needSet[name] {
			continue
		}
		r := allRuntimes[name]
		if err := r.Initialize(); err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] FAILED: %v\n", name, err)
		} else {
			fmt.Fprintf(os.Stderr, "  [%s] OK\n", name)
			runtimes[name] = r
			disp.RegisterPumpCallback(name, r.Pump)
		}
	}

	// Install cross-runtime bridge callbacks on initialized runtimes
	callPtr := uintptr(C.get_omni_call_ptr())
	freePtr := uintptr(C.get_omni_free_ptr())
	for _, rt := range runtimes {
		rt.SetBridgeCallback(callPtr, freePtr)
	}

	// Initialize watchdog
	watchdog.Init()
	if _, ok := runtimes["python"]; ok {
		if ptr := pyRuntime.InterruptFuncPtr(); ptr != nil {
			watchdog.SetPythonInterrupt(ptr)
		}
	}
	if _, ok := runtimes["javascript"]; ok {
		if ptr := jsRuntime.TerminateFuncPtr(); ptr != nil {
			watchdog.SetV8Terminate(ptr)
		}
	}
	if _, ok := runtimes["ruby"]; ok {
		if ptr := rbRuntime.InterruptFuncPtr(); ptr != nil {
			watchdog.SetRubyInterrupt(ptr)
		}
	}

	fmt.Fprintln(os.Stderr, "Ready.")

	// Start signal handler in background
	go sigMgr.Wait(ctx)

	// Determine execution mode
	dispatcherRunning := false
	switch cmd.Mode {
	case ModeExec:
		r, ok := runtimes[cmd.Language]
		if !ok {
			fmt.Fprintf(os.Stderr, "Runtime %q not available\n", cmd.Language)
			os.Exit(1)
		}
		result := executeWithWatchdog(runtimeID(cmd.Language), func() pkg.Result {
			return r.Execute(cmd.Code)
		})
		printResult(cmd.Language, result)

	case ModeRun:
		executeFileNew(cmd)

	default:
		// REPL mode
		dispatcherRunning = true
		if jsRT, ok := runtimes["javascript"]; ok {
			uvFD := jsRT.(*javascript.Runtime).GetUVBackendFD()
			go func() {
				disp.RunEpoll(ctx, uvFD)
			}()
		} else {
			go func() {
				disp.RunEpoll(ctx, -1)
			}()
		}
		repl(ctx)
	}

	// Flush stdout before shutdown messages on stderr
	os.Stdout.Sync()

	// Graceful shutdown
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

// Alias cli.Mode constants for use in this file
const (
	ModeREPL = cli.ModeREPL
	ModeRun  = cli.ModeRun
	ModeExec = cli.ModeExec
)

// runGoFile handles Go file execution — no Golden Thread or embedded runtimes needed.
func runGoFile(cmd cli.Command) {
	result := goRuntime.ExecuteFile(cmd.File, cmd.Args, os.Stdin)
	if result.Err != nil {
		enhanced := errmsg.Enhance("go", result.Err.Error())
		fmt.Fprintln(os.Stderr, enhanced)
		exitCode := result.ExitCode
		if exitCode == 0 {
			exitCode = 1
		}
		os.Exit(exitCode)
	}
	if result.Output != "" {
		fmt.Print(result.Output)
	}
}

// executeFileNew runs a script file with argv, stdin, and shebang support.
// If the runtime implements FileExecutor, uses that (real args, real stdout).
// Otherwise falls back to reading the file and calling Execute(code).
func executeFileNew(cmd cli.Command) {
	r, ok := runtimes[cmd.Language]
	if !ok {
		fmt.Fprintf(os.Stderr, "Runtime %q not available\n", cmd.Language)
		os.Exit(1)
	}

	// Prefer FileExecutor: real args to main(), real stdout/stderr, exit codes
	if fe, ok := r.(pkg.FileExecutor); ok {
		result := executeWithWatchdog(runtimeID(cmd.Language), func() pkg.Result {
			return fe.ExecuteFile(cmd.File, cmd.Args, os.Stdin)
		})
		printResult(cmd.Language, result)
		return
	}

	// Fallback: read file, set up argv manually, Execute(code)
	data, err := os.ReadFile(cmd.File)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading file: %v\n", err)
		os.Exit(1)
	}

	code := cli.StripShebang(string(data))

	// Set up argv for the target runtime before executing
	setupArgv(cmd.Language, cmd.File, cmd.Args)

	// Set up stdin forwarding for the target runtime
	setupStdin(cmd.Language)

	result := executeWithWatchdog(runtimeID(cmd.Language), func() pkg.Result {
		return r.Execute(code)
	})
	printResult(cmd.Language, result)
}

// setupArgv configures sys.argv / process.argv / ARGV etc. for the target runtime.
func setupArgv(lang, file string, args []string) {
	r, ok := runtimes[lang]
	if !ok {
		return
	}

	switch lang {
	case "python":
		// Build sys.argv = [file, arg1, arg2, ...]
		parts := []string{fmt.Sprintf("%q", file)}
		for _, a := range args {
			parts = append(parts, fmt.Sprintf("%q", a))
		}
		code := fmt.Sprintf("import sys; sys.argv = [%s]", strings.Join(parts, ", "))
		r.Execute(code)

	case "javascript":
		// Build process.argv = [node, file, arg1, arg2, ...]
		parts := []string{`"node"`, fmt.Sprintf("%q", file)}
		for _, a := range args {
			parts = append(parts, fmt.Sprintf("%q", a))
		}
		code := fmt.Sprintf("process.argv = [%s]", strings.Join(parts, ", "))
		r.Execute(code)

	case "ruby":
		// ARGV = [arg1, arg2, ...], $0 = file, $PROGRAM_NAME = file
		parts := []string{}
		for _, a := range args {
			parts = append(parts, fmt.Sprintf("%q", a))
		}
		code := fmt.Sprintf("ARGV.replace([%s]); $0 = %q; $PROGRAM_NAME = %q",
			strings.Join(parts, ", "), file, file)
		r.Execute(code)

	case "java":
		// Store args in a system property for retrieval
		joined := strings.Join(args, "\x00")
		code := fmt.Sprintf(`System.setProperty("omnivm.argv", "%s")`, escapeJavaString(joined))
		r.Execute(code)
	}
}

// setupStdin configures stdin forwarding for embedded runtimes.
// For Python/Ruby/JS, stdin is already available via the process's fd 0.
// No special setup needed — the embedded runtimes inherit the process stdin.
func setupStdin(lang string) {
	// Python: sys.stdin reads from fd 0 (inherited from process)
	// JavaScript: process.stdin reads from fd 0
	// Ruby: $stdin / STDIN reads from fd 0
	// Java: System.in reads from fd 0
	// All inherited automatically — nothing to do.
}

func escapeJavaString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// executeWithWatchdog arms the watchdog, sets the active runtime, executes
// the function, then disarms and clears.
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
		errMsg := result.Err.Error()
		enhanced := errmsg.Enhance(lang, errMsg)
		formatted := errmsg.FormatTraceback(lang, enhanced)
		fmt.Fprintln(os.Stderr, formatted)
		exitCode := result.ExitCode
		if exitCode == 0 {
			exitCode = 1
		}
		os.Exit(exitCode)
	}
	if result.Output != "" {
		fmt.Print(result.Output)
	}
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
		case line == ":go":
			currentLang = "go"
			fmt.Println("Switched to Go")
		case line == ":help":
			printHelp()
		case line == ":status":
			printStatus()
		default:
			r, ok := runtimes[currentLang]
			if !ok {
				fmt.Fprintf(os.Stderr, "Runtime %q not available\n", currentLang)
			} else {
				result := executeWithWatchdog(runtimeID(currentLang), func() pkg.Result {
					return r.Execute(line)
				})
				if result.Err != nil {
					enhanced := errmsg.Enhance(currentLang, result.Err.Error())
					fmt.Fprintln(os.Stderr, enhanced)
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
  :go                Switch to Go
  :status            Show runtime status
  :help              Show this help
  :quit, :q          Exit

CLI Usage:
  omnivm run <file> [args...]   Run a script file
  omnivm -python "code"         Execute Python code
  omnivm -js "code"             Execute JavaScript code`)
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
