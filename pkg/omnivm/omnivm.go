// Package omnivm provides a Go library for hosting polyglot runtimes.
//
// The VM serializes all guest runtime calls through a single OS thread
// (the "Golden Thread") via a dispatcher. Callers from any goroutine
// can use Call/Execute to interact with registered runtimes.
package omnivm

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	pkg "github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/dispatcher"
)

// Config controls VM behavior.
type Config struct {
	// TaskTimeout is the global default per-call timeout. Zero means no timeout.
	TaskTimeout time.Duration
	// DrainTimeout is the maximum time Shutdown() waits for in-flight calls.
	DrainTimeout time.Duration
}

// CallMetrics is passed to the OnCallDone callback with observability data.
type CallMetrics struct {
	Runtime   string        // which runtime was called
	Result    string        // string result (empty on error)
	Err       error         // nil on success
	Duration  time.Duration // wall-clock time of the Python/JS/etc execution
	QueueWait time.Duration // time spent waiting in the dispatcher queue
	Fast      bool          // true if dispatched via the high-priority channel
	RequestID string        // caller-provided correlation ID (empty if not set)
}

// BatchItem is one element in a CallBatch request.
type BatchItem struct {
	Code string // code to evaluate
}

// BatchResult is one element in a CallBatch response.
type BatchResult struct {
	Value string // result string on success
	Err   error  // non-nil on failure
}

// VM is the main entry point for the OmniVM library.
type VM struct {
	cfg Config

	disp *dispatcher.Dispatcher

	mu        sync.Mutex
	runtimes  map[string]pkg.Runtime
	regOrder  []string // registration order
	initOrder []string // tracks what was successfully initialized

	afterCall  map[string]string // runtime name → cleanup code
	drainHooks []func()
	onCallDone func(CallMetrics)

	activeMu        sync.Mutex
	activeInterrupt func()

	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
	shutdown bool
	draining bool
}

// New creates a new VM with the given configuration.
func New(cfg Config) *VM {
	disp := dispatcher.New()
	disp.TaskTimeout = cfg.TaskTimeout
	vm := &VM{
		cfg:       cfg,
		disp:      disp,
		runtimes:  make(map[string]pkg.Runtime),
		afterCall: make(map[string]string),
	}
	disp.OnTaskTimeout = vm.interruptActiveRuntime
	return vm
}

// Register adds a runtime to the VM. Must be called before Start().
// Runtimes are initialized in registration order.
func (vm *VM) Register(name string, rt pkg.Runtime) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.runtimes[name] = rt
	vm.regOrder = append(vm.regOrder, name)
}

// Start initializes all registered runtimes in registration order.
// Must be called on the Golden Thread (main goroutine with runtime.LockOSThread).
// If any runtime fails to initialize, already-initialized runtimes are shut down.
func (vm *VM) Start() error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.started {
		return ErrAlreadyStarted
	}

	for _, name := range vm.regOrder {
		rt := vm.runtimes[name]
		if err := rt.Initialize(); err != nil {
			// Cleanup already-initialized runtimes in reverse order
			for i := len(vm.initOrder) - 1; i >= 0; i-- {
				vm.runtimes[vm.initOrder[i]].Shutdown()
			}
			return fmt.Errorf("omnivm: failed to initialize %s: %w", name, err)
		}
		vm.initOrder = append(vm.initOrder, name)
		vm.disp.RegisterPumpCallback(name, rt.Pump)
	}

	vm.started = true
	vm.ctx, vm.cancel = context.WithCancel(context.Background())
	return nil
}

// Run blocks running the dispatcher loop on the current goroutine.
// This must be called on the Golden Thread. Returns when ctx is cancelled.
func (vm *VM) Run(ctx context.Context) {
	// Merge the external context with our internal context
	mergedCtx, mergedCancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-vm.ctx.Done():
			mergedCancel()
		case <-mergedCtx.Done():
		}
	}()
	vm.disp.Run(mergedCtx)
	mergedCancel() // prevent goroutine leak
}

// Call evaluates code in the named runtime and returns the result as a string.
// Safe to call from any goroutine.
func (vm *VM) Call(runtime, code string) (string, error) {
	return vm.CallWithContext(context.Background(), runtime, code)
}

// CallWithContext evaluates code with a request-scoped context.
// If ctx is cancelled before the call completes, returns ctx.Err().
// Note: the Golden Thread task still runs to completion (cgo cannot be interrupted).
func (vm *VM) CallWithContext(ctx context.Context, runtime, code string) (string, error) {
	return vm.callInternal(ctx, runtime, code, "", false)
}

// CallWithRequestID is like CallWithContext but attaches a request ID for
// correlation in the OnCallDone metrics callback.
func (vm *VM) CallWithRequestID(ctx context.Context, runtime, code, requestID string) (string, error) {
	return vm.callInternal(ctx, runtime, code, requestID, false)
}

// callInternal is the shared implementation for Call, CallFast, and their variants.
func (vm *VM) callInternal(ctx context.Context, runtime, code, requestID string, fast bool) (string, error) {
	if err := vm.checkReady(runtime); err != nil {
		return "", err
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	enqueueTime := time.Now()
	rt := vm.runtimes[runtime]

	dispatchFn := func() (interface{}, error) {
		vm.setActiveRuntime(rt)
		defer vm.clearActiveRuntime()

		execStart := time.Now()
		queueWait := execStart.Sub(enqueueTime)

		result := rt.Eval(code)

		// Run afterCall regardless of error.
		afterCode, hasAfter, onCallDone := vm.callHooks(runtime)
		if hasAfter {
			afterResult := rt.Eval(afterCode)
			if afterResult.Err != nil {
				log.Printf("omnivm: afterCall error for %s: %v", runtime, afterResult.Err)
			}
		}

		execDuration := time.Since(execStart)

		// Fire onCallDone callback with metrics
		if onCallDone != nil {
			resultStr := ""
			if result.Value != nil {
				resultStr = fmt.Sprintf("%v", result.Value)
			}
			onCallDone(CallMetrics{
				Runtime:   runtime,
				Result:    resultStr,
				Err:       result.Err,
				Duration:  execDuration,
				QueueWait: queueWait,
				Fast:      fast,
				RequestID: requestID,
			})
		}

		if result.Err != nil {
			errMsg := result.Err.Error()
			if re := ParseError(runtime, errMsg); re != nil {
				return nil, re
			}
			return nil, result.Err
		}

		val := ""
		if result.Value != nil {
			val = fmt.Sprintf("%v", result.Value)
		}
		return val, nil
	}

	var ch <-chan dispatcher.AsyncResult
	if fast {
		ch = vm.disp.RunAsyncFast(dispatchFn)
	} else {
		ch = vm.disp.RunAsync(dispatchFn)
	}

	select {
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		if res.Value == nil {
			return "", nil
		}
		return res.Value.(string), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// CallFast is like Call but uses the high-priority dispatch channel.
// Fast calls are always processed before normal calls, reducing head-of-line
// blocking for latency-sensitive operations (e.g., auth checks).
func (vm *VM) CallFast(runtime, code string) (string, error) {
	return vm.CallFastWithContext(context.Background(), runtime, code)
}

// CallFastWithContext is like CallWithContext but uses high-priority dispatch.
func (vm *VM) CallFastWithContext(ctx context.Context, runtime, code string) (string, error) {
	return vm.callInternal(ctx, runtime, code, "", true)
}

// CallFastWithRequestID is like CallFastWithContext with a request ID for metrics.
func (vm *VM) CallFastWithRequestID(ctx context.Context, runtime, code, requestID string) (string, error) {
	return vm.callInternal(ctx, runtime, code, requestID, true)
}

// CallBatch evaluates multiple independent code snippets in a single Golden
// Thread dispatch. All items execute sequentially within one task, avoiding
// N round-trips through the dispatcher queue. AfterCall runs once after all
// items complete (not per-item). Each item gets independent error handling —
// a failure in item[1] does not prevent item[2] from executing.
//
// This is ideal when a single HTTP handler needs multiple independent pieces
// of data (e.g., subscription state + usage totals + lock status).
func (vm *VM) CallBatch(runtime string, items []BatchItem) []BatchResult {
	return vm.CallBatchWithContext(context.Background(), runtime, items, "")
}

// CallBatchWithContext is like CallBatch with context cancellation and request ID.
func (vm *VM) CallBatchWithContext(ctx context.Context, runtime string, items []BatchItem, requestID string) []BatchResult {
	results := make([]BatchResult, len(items))
	if err := vm.checkReady(runtime); err != nil {
		for i := range results {
			results[i].Err = err
		}
		return results
	}

	select {
	case <-ctx.Done():
		for i := range results {
			results[i].Err = ctx.Err()
		}
		return results
	default:
	}

	enqueueTime := time.Now()
	rt := vm.runtimes[runtime]

	ch := vm.disp.RunAsync(func() (interface{}, error) {
		vm.setActiveRuntime(rt)
		defer vm.clearActiveRuntime()

		execStart := time.Now()
		queueWait := execStart.Sub(enqueueTime)

		for i, item := range items {
			result := rt.Eval(item.Code)
			if result.Err != nil {
				errMsg := result.Err.Error()
				if re := ParseError(runtime, errMsg); re != nil {
					results[i].Err = re
				} else {
					results[i].Err = result.Err
				}
			} else if result.Value != nil {
				results[i].Value = fmt.Sprintf("%v", result.Value)
			}
		}

		// AfterCall once for the whole batch
		afterCode, hasAfter, onCallDone := vm.callHooks(runtime)
		if hasAfter {
			afterResult := rt.Eval(afterCode)
			if afterResult.Err != nil {
				log.Printf("omnivm: afterCall error for %s: %v", runtime, afterResult.Err)
			}
		}

		execDuration := time.Since(execStart)

		if onCallDone != nil {
			onCallDone(CallMetrics{
				Runtime:   runtime,
				Duration:  execDuration,
				QueueWait: queueWait,
				RequestID: requestID,
			})
		}

		return results, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			for i := range results {
				results[i].Err = res.Err
			}
		}
		return results
	case <-ctx.Done():
		for i := range results {
			if results[i].Err == nil && results[i].Value == "" {
				results[i].Err = ctx.Err()
			}
		}
		return results
	}
}

// Execute runs code in the named runtime and returns captured stdout.
// Safe to call from any goroutine.
func (vm *VM) Execute(runtime, code string) (string, error) {
	return vm.ExecuteWithContext(context.Background(), runtime, code)
}

// ExecuteWithContext runs code with a request-scoped context.
func (vm *VM) ExecuteWithContext(ctx context.Context, runtime, code string) (string, error) {
	if err := vm.checkReady(runtime); err != nil {
		return "", err
	}

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	enqueueTime := time.Now()
	rt := vm.runtimes[runtime]
	ch := vm.disp.RunAsync(func() (interface{}, error) {
		vm.setActiveRuntime(rt)
		defer vm.clearActiveRuntime()

		execStart := time.Now()
		queueWait := execStart.Sub(enqueueTime)

		result := rt.Execute(code)

		afterCode, hasAfter, onCallDone := vm.callHooks(runtime)
		if hasAfter {
			afterResult := rt.Execute(afterCode)
			if afterResult.Err != nil {
				log.Printf("omnivm: afterCall error for %s: %v", runtime, afterResult.Err)
			}
		}

		execDuration := time.Since(execStart)

		if onCallDone != nil {
			onCallDone(CallMetrics{
				Runtime:   runtime,
				Result:    result.Output,
				Err:       result.Err,
				Duration:  execDuration,
				QueueWait: queueWait,
				Fast:      false,
			})
		}

		if result.Err != nil {
			errMsg := result.Err.Error()
			if re := ParseError(runtime, errMsg); re != nil {
				return nil, re
			}
			return nil, result.Err
		}
		return result.Output, nil
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		if res.Value == nil {
			return "", nil
		}
		return res.Value.(string), nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// LoadFile reads a file and executes its contents in the named runtime.
// Use this to define helper functions from .py/.js/.rb files instead of
// inline string literals in Go code.
func (vm *VM) LoadFile(runtime, path string) error {
	if err := vm.checkReady(runtime); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("omnivm: load file: %w", err)
	}
	_, execErr := vm.Execute(runtime, string(data))
	return execErr
}

// SetAfterCall registers cleanup code that runs after every Call/Execute
// to the named runtime. Runs on the Golden Thread within the same dispatch,
// even if the main call errors (like defer/finally). Errors from afterCall
// are logged but don't mask the original error.
func (vm *VM) SetAfterCall(runtime, code string) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.afterCall[runtime] = code
}

// SetOnCallDone registers an observe-only callback that fires after each
// Call/Execute completes (including afterCall). Must not call back into the VM.
// The CallMetrics struct includes duration, queue wait time, channel type,
// and caller-provided request ID for production observability.
func (vm *VM) SetOnCallDone(fn func(CallMetrics)) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.onCallDone = fn
}

// RegisterDrainHook adds a function called during Shutdown before runtime teardown.
// Use for flushing DB connections, Sentry, etc.
func (vm *VM) RegisterDrainHook(fn func()) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	vm.drainHooks = append(vm.drainHooks, fn)
}

// Shutdown gracefully stops the VM. Must be called on the Golden Thread
// (after Run returns). Runs drain hooks directly on the current goroutine
// (so they can call into runtimes), then shuts down runtimes in reverse
// initialization order. Safe to call multiple times (idempotent).
//
// Drain hooks run on the Golden Thread without the dispatcher — they can
// call runtime methods directly via drainExecute(). This is safe because
// after Run() returns, no other goroutine is accessing the runtimes.
func (vm *VM) Shutdown() error {
	vm.mu.Lock()
	if vm.shutdown {
		vm.mu.Unlock()
		return nil
	}
	vm.shutdown = true
	vm.draining = true
	hooks := make([]func(), len(vm.drainHooks))
	copy(hooks, vm.drainHooks)
	order := make([]string, len(vm.initOrder))
	copy(order, vm.initOrder)
	vm.mu.Unlock()

	// Cancel dispatcher and wait for it to stop
	if vm.cancel != nil {
		vm.cancel()
	}
	if !vm.disp.WaitForStopTimeout(vm.cfg.DrainTimeout) {
		return ErrDrainTimeout
	}

	// Run drain hooks directly on the Golden Thread.
	// The dispatcher is stopped, so we call runtimes directly — no dispatch needed.
	// Hooks can use vm.drainExecute() for runtime calls during this phase.
	for _, fn := range hooks {
		fn()
	}

	vm.draining = false

	// Shutdown runtimes in reverse init order
	for i := len(order) - 1; i >= 0; i-- {
		vm.runtimes[order[i]].Shutdown()
	}

	return nil
}

func (vm *VM) callHooks(runtime string) (afterCode string, hasAfter bool, onCallDone func(CallMetrics)) {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	afterCode, hasAfter = vm.afterCall[runtime]
	onCallDone = vm.onCallDone
	return afterCode, hasAfter, onCallDone
}

type interruptibleRuntime interface {
	Interrupt()
}

func (vm *VM) setActiveRuntime(rt pkg.Runtime) {
	vm.activeMu.Lock()
	defer vm.activeMu.Unlock()
	if interruptible, ok := rt.(interruptibleRuntime); ok {
		vm.activeInterrupt = interruptible.Interrupt
	} else {
		vm.activeInterrupt = nil
	}
}

func (vm *VM) clearActiveRuntime() {
	vm.activeMu.Lock()
	defer vm.activeMu.Unlock()
	vm.activeInterrupt = nil
}

func (vm *VM) interruptActiveRuntime() {
	vm.activeMu.Lock()
	fn := vm.activeInterrupt
	vm.activeMu.Unlock()
	if fn != nil {
		fn()
	}
}

// drainExecute runs code on a runtime during the drain phase.
// Called directly on the Golden Thread (no dispatcher). Only valid
// inside drain hooks during Shutdown().
func (vm *VM) drainExecute(runtime, code string) (string, error) {
	rt, ok := vm.runtimes[runtime]
	if !ok {
		return "", &ErrUnknownRuntime{Name: runtime}
	}

	result := rt.Execute(code)
	if result.Err != nil {
		return "", result.Err
	}
	return result.Output, nil
}

func (vm *VM) checkReady(runtime string) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if vm.shutdown {
		return ErrShutdown
	}
	if !vm.started {
		return ErrNotStarted
	}
	if _, ok := vm.runtimes[runtime]; !ok {
		return &ErrUnknownRuntime{Name: runtime}
	}
	return nil
}
