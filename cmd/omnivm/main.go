// OmniVM - The Go-Hosted Polyglot Runtime
//
// A single binary that embeds Python (CPython), JavaScript (V8),
// Java (JVM/JNI), Ruby (MRI), and Go runtimes.
//
// Usage:
//
//	omnivm                          Start the interactive REPL
//	omnivm run script.py [args...]  Run a file (language detected by extension)
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
extern char* OmniInitRuntimes(char* list);
extern char* OmniLoadPlugin(char* runtime, char* path);
extern void OmniShutdownRuntimes(void);

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
extern int OmniBufFree(char* name);

// Typed value bridge exports
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

// Get function pointers to pass to runtimes
static void* get_omni_call_ptr() { return (void*)OmniCall; }
static void* get_omni_free_ptr() { return (void*)OmniFree; }
static void* get_omni_buf_get_ptr()     { return (void*)OmniBufGet; }
static void* get_omni_buf_set_ptr()     { return (void*)OmniBufSet; }
static void* get_omni_buf_release_ptr() { return (void*)OmniBufRelease; }
static void* get_omni_buf_free_ptr()    { return (void*)OmniBufFree; }
static void* get_omni_call_typed_ptr()  { return (void*)OmniCallTyped; }

// Get function pointers for Python interpreter mode callbacks
static void* get_omni_init_runtimes_ptr() { return (void*)OmniInitRuntimes; }
static void* get_omni_load_plugin_ptr()   { return (void*)OmniLoadPlugin; }
static void* get_omni_shutdown_ptr()      { return (void*)OmniShutdownRuntimes; }

// Get the current OS thread ID (Linux-specific).
static long get_thread_id() { return syscall(SYS_gettid); }
*/
import "C"

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/cli"
	"github.com/omnivm/omnivm/pkg/engine"
	"github.com/omnivm/omnivm/pkg/errmsg"
	golangrt "github.com/omnivm/omnivm/pkg/golang"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/polyglot"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
	"github.com/omnivm/omnivm/pkg/signals"
)

func init() {
	// Pin the main goroutine to the main OS thread — the "Golden Thread".
	// All guest runtime interactions (V8, CPython, JVM, Ruby) must happen here.
	runtime.LockOSThread()
}

// eng is the shared engine managing all runtimes.
var eng *engine.Engine

// goRuntime is the Go plugin runtime, initialized alongside other runtimes.
var goRuntime = golangrt.New()

//export OmniCall
func OmniCall(cRuntime *C.char, cCode *C.char) *C.char {
	rtName := C.GoString(cRuntime)
	code := C.GoString(cCode)

	threadID := int64(C.get_thread_id())
	val, err := eng.Call(rtName, code, threadID)
	if err != nil {
		return C.CString("ERR:" + err.Error())
	}
	return C.CString(val)
}

//export OmniFree
func OmniFree(ptr *C.char) {
	if ptr != nil {
		C.free(unsafe.Pointer(ptr))
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
	if eng != nil && int64(C.get_thread_id()) == eng.GoldenThreadID {
		arrow.GlobalStore().DrainDeferred()
	}
}

//export OmniBufFree
func OmniBufFree(cName *C.char) C.int {
	if err := arrow.BufFree(C.GoString(cName)); err != nil {
		return -1
	}
	return 0
}

//export OmniCallTyped
func OmniCallTyped(cRuntime *C.char, cFuncName *C.char, cArgs *C.omni_value_t, nargs C.int32_t) C.omni_value_t {
	rtName := C.GoString(cRuntime)
	funcName := C.GoString(cFuncName)

	// Convert C args to Go values
	n := int(nargs)
	goArgs := make([]polyglot.Value, n)
	if n > 0 && cArgs != nil {
		for i := 0; i < n; i++ {
			argPtr := unsafe.Pointer(uintptr(unsafe.Pointer(cArgs)) + uintptr(i)*polyglot.CValueSize)
			goArgs[i] = polyglot.FromCValueRaw(argPtr)
		}
	}

	result := eng.CallTyped(rtName, funcName, goArgs)
	var cv C.omni_value_t
	result.ToCValueRaw(unsafe.Pointer(&cv))
	return cv
}

func main() {
	// Python interpreter mode: if invoked as "python3" (symlink) or "omnivm python ...",
	// delegate to Py_BytesMain() with the omnivm module pre-registered.
	if isPythonMode() {
		os.Exit(runPythonInterpreter())
	}

	// Parse CLI arguments
	cmd, err := cli.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Determine which runtimes to initialize (lazy init)
	needed := cli.RequiredRuntimes(cmd)

	// Set up signal handling
	sigMgr := signals.NewManager()

	// Create the engine
	eng = engine.New()
	eng.GoldenThreadID = int64(C.get_thread_id())
	eng.SetupWatchdogAlert()

	// Create shared memory store (process-wide singleton)
	arrow.SetGlobalStore(arrow.NewSharedStore())

	// Create runtimes — only the ones we need
	allRuntimes := map[string]pkg.Runtime{
		"python":     python.New(),
		"javascript": javascript.New(),
		"java":       jvm.New(),
		"ruby":       ruby.New(),
		"go":         goRuntime,
	}

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
	for _, name := range []string{"python", "javascript", "java", "ruby", "go"} {
		if !needSet[name] {
			continue
		}
		r := allRuntimes[name]
		if err := r.Initialize(); err != nil {
			fmt.Fprintf(os.Stderr, "  [%s] FAILED: %v\n", name, err)
		} else {
			fmt.Fprintf(os.Stderr, "  [%s] OK\n", name)
			eng.Runtimes[name] = r
		}
	}

	// Set up watchdog, bridge, dispatcher
	eng.SetupWatchdog()
	eng.SetupPythonInterruptTimeout()

	callPtr := uintptr(C.get_omni_call_ptr())
	freePtr := uintptr(C.get_omni_free_ptr())
	eng.SetupBridge(callPtr, freePtr)

	// Install buffer bridge callbacks on Python runtime
	eng.SetupBufCallbacks(
		uintptr(C.get_omni_buf_get_ptr()),
		uintptr(C.get_omni_buf_set_ptr()),
		uintptr(C.get_omni_buf_release_ptr()),
		uintptr(C.get_omni_buf_free_ptr()),
	)

	// Install typed call bridge
	eng.SetupTypedCallback(uintptr(C.get_omni_call_typed_ptr()))

	// Set up Go bridge: Go plugins call back via a Go closure (no C needed)
	if _, ok := eng.Runtimes["go"]; ok {
		goRuntime.BridgeFn = func(rtName, code string) string {
			val, err := eng.Call(rtName, code, eng.GoldenThreadID)
			if err != nil {
				return "ERR:" + err.Error()
			}
			return val
		}
	}

	// Signal handler
	sigMgr.RegisterShutdown("engine", func() {
		eng.Cancel()
	})

	fmt.Fprintln(os.Stderr, "Ready.")

	// Start signal handler in background
	go sigMgr.Wait(eng.Context())

	// Determine execution mode
	dispatcherRunning := false
	switch cmd.Mode {
	case ModeExec:
		r, ok := eng.Runtimes[cmd.Language]
		if !ok {
			fmt.Fprintf(os.Stderr, "Runtime %q not available\n", cmd.Language)
			os.Exit(1)
		}
		result := eng.ExecWithWatchdog(engine.RuntimeID(cmd.Language), func() pkg.Result {
			return r.Execute(cmd.Code)
		})
		printResult(cmd.Language, result)

	case ModeRun:
		executeFileNew(cmd)

	default:
		// REPL mode — start the dispatcher
		dispatcherRunning = true
		eng.StartDispatcher()
		repl(eng.Context())
	}

	// Flush stdout before shutdown messages on stderr
	os.Stdout.Sync()

	// Graceful shutdown
	fmt.Fprintln(os.Stderr, "\n[shutdown] Shutting down runtimes...")
	if !dispatcherRunning {
		// If dispatcher wasn't started, cancel context before shutdown
		eng.Cancel()
	}
	eng.Shutdown()
}

// Alias cli.Mode constants for use in this file
const (
	ModeREPL = cli.ModeREPL
	ModeRun  = cli.ModeRun
	ModeExec = cli.ModeExec
)

// executeFileNew runs a script file with argv, stdin, and shebang support.
func executeFileNew(cmd cli.Command) {
	r, ok := eng.Runtimes[cmd.Language]
	if !ok {
		fmt.Fprintf(os.Stderr, "Runtime %q not available\n", cmd.Language)
		os.Exit(1)
	}

	// Prefer FileExecutor: real args to main(), real stdout/stderr, exit codes
	if fe, ok := r.(pkg.FileExecutor); ok {
		result := eng.ExecWithWatchdog(engine.RuntimeID(cmd.Language), func() pkg.Result {
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

	result := eng.ExecWithWatchdog(engine.RuntimeID(cmd.Language), func() pkg.Result {
		return r.Execute(code)
	})
	printResult(cmd.Language, result)
}

// setupArgv configures sys.argv / process.argv / ARGV etc. for the target runtime.
func setupArgv(lang, file string, args []string) {
	r, ok := eng.Runtimes[lang]
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

func escapeJavaString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
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
			r, ok := eng.Runtimes[currentLang]
			if !ok {
				fmt.Fprintf(os.Stderr, "Runtime %q not available\n", currentLang)
			} else {
				result := eng.ExecWithWatchdog(engine.RuntimeID(currentLang), func() pkg.Result {
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
	for name, r := range eng.Runtimes {
		result := r.Execute("1")
		status := "OK"
		if result.Err != nil {
			status = "ERROR"
		}
		fmt.Printf("  [%s] %s\n", name, status)
	}
}

// ---------------------------------------------------------------------------
// Python interpreter mode
// ---------------------------------------------------------------------------

// isPythonMode returns true if OmniVM should act as a Python interpreter.
// Triggered when the binary is symlinked as "python", "python3", "python3.14",
// or when invoked as "omnivm python [args...]".
func isPythonMode() bool {
	base := filepath.Base(os.Args[0])
	if strings.HasPrefix(base, "python") {
		return true
	}
	if len(os.Args) > 1 && os.Args[1] == "python" {
		return true
	}
	return false
}

// runPythonInterpreter runs CPython's full CLI via Py_BytesMain().
// The "omnivm" module is pre-registered so "import omnivm" works.
func runPythonInterpreter() int {
	// Register the omnivm built-in module BEFORE Py_BytesMain initializes CPython.
	python.RegisterAppendInittab()

	// Install Go callbacks so omnivm.init_runtimes() etc. can call back into Go.
	python.SetPyModeCallbacks(
		unsafe.Pointer(C.get_omni_init_runtimes_ptr()),
		unsafe.Pointer(C.get_omni_load_plugin_ptr()),
		unsafe.Pointer(C.get_omni_shutdown_ptr()),
		unsafe.Pointer(C.get_omni_free_ptr()),
	)

	// Build argv for Py_BytesMain. If invoked as "omnivm python -m pytest",
	// strip "omnivm" and "python" so CPython sees "-m pytest".
	args := os.Args
	base := filepath.Base(args[0])
	if !strings.HasPrefix(base, "python") {
		// Invoked as "omnivm python [args...]" - shift past the subcommand.
		if len(args) > 1 && args[1] == "python" {
			args = append([]string{args[0]}, args[2:]...)
		}
	}

	return python.BytesMain(args)
}

//export OmniInitRuntimes
func OmniInitRuntimes(cList *C.char) *C.char {
	list := C.GoString(cList)
	names := strings.Split(list, ",")

	// Mark CPython as already initialized (Py_BytesMain did it)
	python.MarkCPythonInitialized()

	// Create and configure the engine
	eng = engine.New()
	eng.GoldenThreadID = int64(C.get_thread_id())

	// Runtime creators (Python is already running, "go" uses plugin system)
	allCreators := map[string]func() pkg.Runtime{
		"go":         func() pkg.Runtime { return goRuntime },
		"javascript": func() pkg.Runtime { return javascript.New() },
		"java":       func() pkg.Runtime { return jvm.New() },
		"ruby":       func() pkg.Runtime { return ruby.New() },
	}

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || name == "python" {
			continue // Python is already running
		}

		creator, ok := allCreators[name]
		if !ok {
			return C.CString("ERR:unknown runtime: " + name)
		}
		rt := creator()
		if err := rt.Initialize(); err != nil {
			return C.CString("ERR:" + name + ": " + err.Error())
		}
		eng.Runtimes[name] = rt
	}

	// Set up watchdog, bridge, dispatcher
	eng.SetupWatchdog()
	eng.ActivateForkGuard()

	callPtr := uintptr(C.get_omni_call_ptr())
	freePtr := uintptr(C.get_omni_free_ptr())
	eng.SetupBridge(callPtr, freePtr)

	// Install buffer bridge callbacks
	eng.SetupBufCallbacks(
		uintptr(C.get_omni_buf_get_ptr()),
		uintptr(C.get_omni_buf_set_ptr()),
		uintptr(C.get_omni_buf_release_ptr()),
		uintptr(C.get_omni_buf_free_ptr()),
	)

	// Install typed call bridge
	eng.SetupTypedCallback(uintptr(C.get_omni_call_typed_ptr()))

	// Also set the bridge on the Python side so omnivm.call() works
	// (Python is the host, not in the runtimes map, but needs the callback)
	pyRT := python.New()
	pyRT.SetBridgeCallback(callPtr, freePtr)
	pyRT.SetTypedCallback(uintptr(C.get_omni_call_typed_ptr()))
	pyRT.SetBufCallbacks(
		uintptr(C.get_omni_buf_get_ptr()),
		uintptr(C.get_omni_buf_set_ptr()),
		uintptr(C.get_omni_buf_release_ptr()),
		uintptr(C.get_omni_buf_free_ptr()),
	)

	// Set up Go bridge closure
	if _, ok := eng.Runtimes["go"]; ok {
		goRuntime.BridgeFn = func(rtName, code string) string {
			val, err := eng.Call(rtName, code, eng.GoldenThreadID)
			if err != nil {
				return "ERR:" + err.Error()
			}
			return val
		}
	}

	eng.StartDispatcher()

	return C.CString("OK")
}

//export OmniLoadPlugin
func OmniLoadPlugin(cRuntime *C.char, cPath *C.char) *C.char {
	rtName := C.GoString(cRuntime)
	path := C.GoString(cPath)

	if rtName != "go" {
		return C.CString("ERR:load_plugin only supported for 'go' runtime")
	}

	if err := goRuntime.LoadPlugin(path); err != nil {
		return C.CString("ERR:" + err.Error())
	}

	return C.CString("OK")
}

//export OmniShutdownRuntimes
func OmniShutdownRuntimes() {
	if eng != nil {
		eng.Shutdown()
	}
}
