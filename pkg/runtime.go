// Package pkg defines the common interfaces for guest runtimes.
package pkg

// Result represents the outcome of executing code in a guest runtime.
type Result struct {
	Value  interface{}
	Output string
	Err    error
}

// Runtime is the interface that all guest language runtimes must implement.
type Runtime interface {
	// Name returns the runtime name (e.g. "python", "javascript").
	Name() string

	// Initialize sets up the runtime. Must be called on the Golden Thread.
	Initialize() error

	// Execute runs code synchronously. Must be called on the Golden Thread.
	// Captures stdout output.
	Execute(code string) Result

	// Eval evaluates an expression and returns its value as a string
	// via the C API (not via stdout). Used by the cross-runtime bridge.
	Eval(code string) Result

	// SetBridgeCallback installs the cross-runtime callback function pointer.
	// Must be called after all runtimes are initialized, before guest code.
	SetBridgeCallback(callPtr, freePtr uintptr)

	// Pump ticks the runtime's internal event loop. Called on every
	// dispatcher cycle to process pending async events.
	Pump()

	// Shutdown gracefully tears down the runtime. Must be called on
	// the Golden Thread.
	Shutdown() error
}
