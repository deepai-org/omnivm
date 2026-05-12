// Package engine provides the shared runtime management core used by both
// the omnivm binary (cmd/omnivm) and the c-shared library (cmd/libomnivm).
//
// It handles runtime lifecycle, cross-runtime bridge wiring, watchdog setup,
// dispatcher management, and the Call/Exec entry points. Each binary provides
// thin wrappers (e.g. //export OmniCall) that delegate here.
package engine

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/dispatcher"
	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
	"github.com/omnivm/omnivm/pkg/watchdog"
)

// Engine manages the lifecycle of all OmniVM runtimes.
type Engine struct {
	Runtimes       map[string]pkg.Runtime
	GoldenThreadID int64
	Disp           *dispatcher.Dispatcher
	TaskTimeoutMS  int

	ctx               context.Context
	cancel            context.CancelFunc
	dispatcherStarted bool
}

// New creates an Engine with sensible defaults.
func New() *Engine {
	ctx, cancel := context.WithCancel(context.Background())
	disp := dispatcher.New()
	disp.TaskTimeout = 30 * time.Second
	disp.WatchdogTimeout = 5 * time.Second

	return &Engine{
		Runtimes:      make(map[string]pkg.Runtime),
		Disp:          disp,
		TaskTimeoutMS: int(disp.TaskTimeout / time.Millisecond),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// SetupBridge installs cross-runtime bridge callbacks on all initialized
// runtimes so any runtime can call any other via omnivm.call().
func (e *Engine) SetupBridge(callPtr, freePtr uintptr) {
	for _, rt := range e.Runtimes {
		rt.SetBridgeCallback(callPtr, freePtr)
	}
}

// SetupBufCallbacks installs buffer bridge callbacks on runtimes that support them.
func (e *Engine) SetupBufCallbacks(getPtr, setPtr, releasePtr uintptr) {
	if rt, ok := e.Runtimes["python"]; ok {
		if pyRT, ok := rt.(*python.Runtime); ok {
			pyRT.SetBufCallbacks(getPtr, setPtr, releasePtr)
		}
	}
	if rt, ok := e.Runtimes["javascript"]; ok {
		if jsRT, ok := rt.(*javascript.Runtime); ok {
			jsRT.SetBufCallbacks(getPtr, setPtr, releasePtr)
		}
	}
	if rt, ok := e.Runtimes["ruby"]; ok {
		if rbRT, ok := rt.(*ruby.Runtime); ok {
			rbRT.SetBufCallbacks(getPtr, setPtr, releasePtr)
		}
	}
	// TODO: Add Java buffer callbacks here when implemented
}

// SetupWatchdog registers each runtime's interrupt function pointer with the
// watchdog so it can terminate runaway code.
func (e *Engine) SetupWatchdog() {
	watchdog.Init()

	if rt, ok := e.Runtimes["python"]; ok {
		if pyRT, ok := rt.(*python.Runtime); ok {
			if ptr := pyRT.InterruptFuncPtr(); ptr != nil {
				watchdog.SetPythonInterrupt(ptr)
			}
		}
	}
	if rt, ok := e.Runtimes["javascript"]; ok {
		if jsRT, ok := rt.(*javascript.Runtime); ok {
			if ptr := jsRT.TerminateFuncPtr(); ptr != nil {
				watchdog.SetV8Terminate(ptr)
			}
		}
	}
	if rt, ok := e.Runtimes["ruby"]; ok {
		if rbRT, ok := rt.(*ruby.Runtime); ok {
			if ptr := rbRT.InterruptFuncPtr(); ptr != nil {
				watchdog.SetRubyInterrupt(ptr)
			}
		}
	}

	// Dispatcher hooks for arm/disarm around tasks
	e.Disp.OnTaskStart = func() {
		if e.Disp.TaskTimeout > 0 {
			watchdog.Arm(int(e.Disp.TaskTimeout / time.Millisecond))
		}
	}
	e.Disp.OnTaskEnd = func() {
		watchdog.Disarm()
	}
}

// StartDispatcher launches the epoll-based dispatcher on a background goroutine.
// It handles JS event loop pumping and async task scheduling.
func (e *Engine) StartDispatcher() {
	// Register pump callbacks for all runtimes
	for name, rt := range e.Runtimes {
		e.Disp.RegisterPumpCallback(name, rt.Pump)
	}

	uvFD := -1
	if rt, ok := e.Runtimes["javascript"]; ok {
		if jsRT, ok := rt.(*javascript.Runtime); ok {
			uvFD = jsRT.GetUVBackendFD()
		}
	}

	e.dispatcherStarted = true
	go e.Disp.RunEpoll(e.ctx, uvFD)
}

// RuntimeID maps a language name to a watchdog runtime constant.
func RuntimeID(lang string) int {
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

// Call evaluates an expression in a runtime, managing watchdog state.
// Returns the string result or an error.
func (e *Engine) Call(rtName, code string, threadID int64) (string, error) {
	rt, ok := e.Runtimes[rtName]
	if !ok {
		return "", fmt.Errorf("unknown runtime: %s", rtName)
	}

	// Only manage watchdog for Golden Thread tasks
	isGolden := threadID == e.GoldenThreadID
	var prevRT int
	if isGolden {
		prevRT = watchdog.GetActiveRuntime()
		watchdog.SetActiveRuntime(RuntimeID(rtName))
	}

	result := rt.Eval(code)

	if isGolden {
		watchdog.SetActiveRuntime(prevRT)
	}

	if result.Err != nil {
		return "", result.Err
	}
	if result.Value != nil {
		return fmt.Sprintf("%v", result.Value), nil
	}
	return result.Output, nil
}

// Exec executes code in a runtime (for side effects), managing watchdog state.
// Returns captured stdout or an error.
func (e *Engine) Exec(rtName, code string, threadID int64) (string, error) {
	rt, ok := e.Runtimes[rtName]
	if !ok {
		return "", fmt.Errorf("unknown runtime: %s", rtName)
	}

	isGolden := threadID == e.GoldenThreadID
	var prevRT int
	if isGolden {
		prevRT = watchdog.GetActiveRuntime()
		watchdog.SetActiveRuntime(RuntimeID(rtName))
	}

	result := rt.Execute(code)

	if isGolden {
		watchdog.SetActiveRuntime(prevRT)
	}

	if result.Err != nil {
		return "", result.Err
	}
	return result.Output, nil
}

// ExecWithWatchdog arms the watchdog, sets the active runtime, executes fn,
// then disarms. Used for CLI/file execution paths.
func (e *Engine) ExecWithWatchdog(rtID int, fn func() pkg.Result) pkg.Result {
	watchdog.SetActiveRuntime(rtID)
	if e.TaskTimeoutMS > 0 {
		watchdog.Arm(e.TaskTimeoutMS)
	}
	result := fn()
	watchdog.Disarm()
	watchdog.SetActiveRuntime(watchdog.RuntimeNone)
	return result
}

// ActivateForkGuard enables the fork guard if JVM or Ruby are loaded.
// In a forked child, these runtimes' threads don't survive — the guard
// kills the child with a diagnostic message instead of deadlocking.
func (e *Engine) ActivateForkGuard() {
	for name := range e.Runtimes {
		if name == "java" || name == "ruby" {
			python.ActivateForkGuard()
			return
		}
	}
}

// Shutdown performs an ordered teardown of all runtimes, dispatcher, and watchdog.
func (e *Engine) Shutdown() {
	e.cancel()
	if e.dispatcherStarted {
		e.Disp.WaitForStop()
	}

	watchdog.Shutdown()

	// Reverse initialization order: Go, Ruby, Java, JavaScript, Python
	for _, name := range []string{"go", "ruby", "java", "javascript", "python"} {
		if r, ok := e.Runtimes[name]; ok {
			r.Shutdown()
			delete(e.Runtimes, name)
		}
	}
}

// SetupPythonInterruptTimeout wires a Python interrupt to fire when tasks
// exceed the dispatcher's TaskTimeout.
func (e *Engine) SetupPythonInterruptTimeout() {
	if rt, ok := e.Runtimes["python"]; ok {
		if pyRT, ok := rt.(*python.Runtime); ok {
			e.Disp.OnTaskTimeout = func() {
				pyRT.Interrupt()
			}
		}
	}
}

// SetupWatchdogAlert installs a stderr warning for when the Golden Thread
// is blocked too long.
func (e *Engine) SetupWatchdogAlert() {
	e.Disp.WatchdogTimeout = 5 * time.Second
	e.Disp.OnWatchdogAlert = func(d time.Duration) {
		fmt.Fprintf(os.Stderr, "[watchdog] Golden Thread blocked for >%v\n", d)
	}
}

// Context returns the engine's context.
func (e *Engine) Context() context.Context { return e.ctx }

// Cancel cancels the engine's context.
func (e *Engine) Cancel() { e.cancel() }

