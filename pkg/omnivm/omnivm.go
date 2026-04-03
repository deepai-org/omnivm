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
	onCallDone func(runtime, result string, err error)

	ctx      context.Context
	cancel   context.CancelFunc
	started  bool
	shutdown bool
	draining bool
}

// New creates a new VM with the given configuration.
func New(cfg Config) *VM {
	return &VM{
		cfg:       cfg,
		disp:      dispatcher.New(),
		runtimes:  make(map[string]pkg.Runtime),
		afterCall: make(map[string]string),
	}
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
	if err := vm.checkReady(runtime); err != nil {
		return "", err
	}

	// Check context before dispatching
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	rt := vm.runtimes[runtime]
	ch := vm.disp.RunAsync(func() (interface{}, error) {
		result := rt.Eval(code)

		// Run afterCall regardless of error
		afterCode, hasAfter := vm.afterCall[runtime]
		if hasAfter {
			afterResult := rt.Execute(afterCode)
			if afterResult.Err != nil {
				log.Printf("omnivm: afterCall error for %s: %v", runtime, afterResult.Err)
			}
		}

		// Fire onCallDone callback
		if vm.onCallDone != nil {
			resultStr := ""
			if result.Value != nil {
				resultStr = fmt.Sprintf("%v", result.Value)
			}
			vm.onCallDone(runtime, resultStr, result.Err)
		}

		if result.Err != nil {
			// Try to parse structured error
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

	rt := vm.runtimes[runtime]
	ch := vm.disp.RunAsync(func() (interface{}, error) {
		result := rt.Execute(code)

		afterCode, hasAfter := vm.afterCall[runtime]
		if hasAfter {
			afterResult := rt.Execute(afterCode)
			if afterResult.Err != nil {
				log.Printf("omnivm: afterCall error for %s: %v", runtime, afterResult.Err)
			}
		}

		if vm.onCallDone != nil {
			vm.onCallDone(runtime, result.Output, result.Err)
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
func (vm *VM) SetOnCallDone(fn func(runtime, result string, err error)) {
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
	vm.disp.WaitForStop()

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
