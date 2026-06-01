// Package pkg defines the common interfaces for guest runtimes.
package pkg

import "io"

// Result represents the outcome of executing code in a guest runtime.
type Result struct {
	Value    interface{}
	Output   string
	Err      error
	ExitCode int // Non-zero exit code from the program (for file execution)
}

// ExportedBuffer describes runtime-owned buffer memory published into
// OmniVM's shared data plane. Shape/stride metadata describes the logical view
// when the underlying memory is strided.
type ExportedBuffer struct {
	Name        string
	Dtype       int32
	ArrowFormat string
	Elements    int64
	Shape       []int64
	Strides     []int64
	Offset      int64
	NullCount   int64
	ReadOnly    bool
}

// BufferExporter is implemented by runtimes that can expose a live expression
// through a generic buffer protocol without a user-visible bridge API.
type BufferExporter interface {
	ExportBuffer(name, expr string) (ExportedBuffer, bool, error)
}

// FileExecutor is an optional interface for runtimes that support file execution
// with arguments, stdin, and environment passthrough.
type FileExecutor interface {
	ExecuteFile(path string, args []string, stdin io.Reader) Result
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
