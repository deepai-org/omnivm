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

	// TaskTimeout is the maximum time a single task may run before
	// OnTaskTimeout is called. Zero means no timeout.
	TaskTimeout time.Duration

	// OnTaskTimeout is called from a background goroutine when a task
	// exceeds TaskTimeout. The callback should attempt to interrupt the
	// running code (e.g. PyErr_SetInterrupt for Python). It may be called
	// multiple times (once per TaskTimeout interval) if the task doesn't stop.
	OnTaskTimeout func()

	// OnTaskStart is called at the beginning of each task execution.
	// Used by the watchdog to arm its timer.
	OnTaskStart func()

	// OnTaskEnd is called after each task execution completes (in defer).
	// Used by the watchdog to disarm its timer.
	OnTaskEnd func()

	// wakeupFunc is called after enqueuing a task to wake the epoll loop.
	// Set by RunEpoll during init; nil when using ticker-based Run().
	wakeupFunc func()

	shutdownOnce sync.Once

	// stopped is closed when Run() returns, signaling all pumping has ceased.
	stopped chan struct{}
}

// New creates a new Dispatcher.
func New() *Dispatcher {
	return &Dispatcher{
		taskChan:  make(chan task, 256),
		pumpFuncs: make(map[string]PumpFunc),
		stopped:   make(chan struct{}),
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
	defer close(d.stopped)

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

// WaitForStop blocks until Run() has fully returned. Call this after
// cancelling the context to ensure no pump callbacks are in flight
// before shutting down runtimes.
func (d *Dispatcher) WaitForStop() {
	<-d.stopped
}

// RunOnMain dispatches fn to the Golden Thread and blocks until it completes.
// If fn panics, the panic is recovered and returned as an error.
func (d *Dispatcher) RunOnMain(fn func() error) error {
	done := make(chan error, 1)
	d.taskChan <- task{fn: fn, done: done}
	if d.wakeupFunc != nil {
		d.wakeupFunc()
	}
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
	if d.wakeupFunc != nil {
		d.wakeupFunc()
	}
	return ch
}

// executeTask runs a single task with panic recovery and watchdog monitoring.
func (d *Dispatcher) executeTask(t task) {
	done := make(chan struct{})

	if d.OnTaskStart != nil {
		d.OnTaskStart()
	}
	defer func() {
		close(done)
		if d.OnTaskEnd != nil {
			d.OnTaskEnd()
		}
	}()

	if d.WatchdogTimeout > 0 && d.OnWatchdogAlert != nil {
		go d.watchdog(done)
	}

	if d.TaskTimeout > 0 && d.OnTaskTimeout != nil {
		go d.taskTimeoutLoop(done)
	}

	err := d.safeExec(t.fn)

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

// taskTimeoutLoop fires OnTaskTimeout repeatedly until the task completes.
// This allows the callback to inject interrupts that the runtime may need
// multiple attempts to process (e.g. Python checks pending calls periodically).
func (d *Dispatcher) taskTimeoutLoop(done chan struct{}) {
	timer := time.NewTimer(d.TaskTimeout)
	defer timer.Stop()

	for {
		select {
		case <-done:
			return
		case <-timer.C:
			if d.OnTaskTimeout != nil {
				d.OnTaskTimeout()
			}
			// Reset to fire again if task is still stuck
			timer.Reset(d.TaskTimeout)
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

// pumpNamed calls a single named pump callback.
func (d *Dispatcher) pumpNamed(name string) {
	d.pumpMu.RLock()
	defer d.pumpMu.RUnlock()
	if fn, ok := d.pumpFuncs[name]; ok {
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
