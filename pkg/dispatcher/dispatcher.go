// Package dispatcher implements the Golden Thread channel dispatcher.
//
// All guest runtime interactions (Python, V8, JVM, Ruby) must happen on
// the main OS thread ("Golden Thread"). This dispatcher serializes those
// interactions via a Go channel, with deadline-aware pumping of each
// runtime's event loop between tasks.
package dispatcher

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// AsyncResult holds the return value and error from an async dispatch.
type AsyncResult struct {
	Value interface{}
	Err   error
}

// task is an internal envelope for work items sent to the Golden Thread.
type task struct {
	fn   func() error
	done chan error
}

// PumpFunc is a callback that pumps a guest runtime's event loop.
type PumpFunc func()

// Dispatcher is the core scheduler that serializes all guest-runtime
// work onto the Golden Thread via a channel.
type Dispatcher struct {
	taskChan chan task

	pumpMu    sync.RWMutex
	pumpFuncs map[string]PumpFunc

	// WatchdogTimeout is how long a single task can block the Golden Thread
	// before an alert is fired. Zero means no watchdog.
	WatchdogTimeout time.Duration

	// OnWatchdogAlert is called when a task exceeds WatchdogTimeout.
	OnWatchdogAlert func(duration time.Duration)

	shutdownOnce sync.Once
}

// New creates a new Dispatcher.
func New() *Dispatcher {
	return &Dispatcher{
		taskChan:  make(chan task, 256),
		pumpFuncs: make(map[string]PumpFunc),
	}
}

// RegisterPumpCallback adds a function that will be called on every
// pump cycle of the dispatcher loop. Use this to tick guest runtime
// event loops (V8 message loop, Python asyncio, JVM callback queue).
func (d *Dispatcher) RegisterPumpCallback(name string, fn PumpFunc) {
	d.pumpMu.Lock()
	defer d.pumpMu.Unlock()
	d.pumpFuncs[name] = fn
}

// UnregisterPumpCallback removes a previously registered pump callback.
func (d *Dispatcher) UnregisterPumpCallback(name string) {
	d.pumpMu.Lock()
	defer d.pumpMu.Unlock()
	delete(d.pumpFuncs, name)
}

// Run starts the dispatcher loop on the current goroutine. This MUST be
// called from a goroutine that has called runtime.LockOSThread() to pin
// it to the main OS thread.
//
// Run blocks until ctx is cancelled. On cancellation it drains remaining
// tasks from the channel before returning.
func (d *Dispatcher) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case t := <-d.taskChan:
			d.executeTask(t)
		case <-ticker.C:
			// Pump on timer even if no tasks arrived
		case <-ctx.Done():
			d.drain()
			return
		}
		d.pumpAll()
	}
}

// RunOnMain dispatches fn to the Golden Thread and blocks until it completes.
// If fn panics, the panic is recovered and returned as an error.
func (d *Dispatcher) RunOnMain(fn func() error) error {
	done := make(chan error, 1)
	d.taskChan <- task{fn: fn, done: done}
	return <-done
}

// RunAsync dispatches fn to the Golden Thread and returns a channel that
// will receive the result when the function completes.
func (d *Dispatcher) RunAsync(fn func() (interface{}, error)) <-chan AsyncResult {
	ch := make(chan AsyncResult, 1)
	d.taskChan <- task{
		fn: func() error {
			val, err := fn()
			ch <- AsyncResult{Value: val, Err: err}
			return err
		},
		done: make(chan error, 1),
	}
	return ch
}

// executeTask runs a single task with panic recovery and watchdog monitoring.
func (d *Dispatcher) executeTask(t task) {
	var watchdogDone chan struct{}

	if d.WatchdogTimeout > 0 && d.OnWatchdogAlert != nil {
		watchdogDone = make(chan struct{})
		go d.watchdog(watchdogDone)
	}

	err := d.safeExec(t.fn)

	if watchdogDone != nil {
		close(watchdogDone)
	}

	t.done <- err
}

// safeExec runs fn with panic recovery.
func (d *Dispatcher) safeExec(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic recovered on Golden Thread: %v", r)
		}
	}()
	return fn()
}

// watchdog monitors whether the Golden Thread is blocked for too long.
func (d *Dispatcher) watchdog(done chan struct{}) {
	timer := time.NewTimer(d.WatchdogTimeout)
	defer timer.Stop()

	select {
	case <-done:
		return
	case <-timer.C:
		if d.OnWatchdogAlert != nil {
			d.OnWatchdogAlert(d.WatchdogTimeout)
		}
	}
}

// pumpAll calls every registered pump callback.
func (d *Dispatcher) pumpAll() {
	d.pumpMu.RLock()
	defer d.pumpMu.RUnlock()
	for _, fn := range d.pumpFuncs {
		fn()
	}
}

// drain processes remaining tasks in the channel during shutdown.
func (d *Dispatcher) drain() {
	for {
		select {
		case t := <-d.taskChan:
			d.executeTask(t)
			d.pumpAll()
		default:
			return
		}
	}
}
