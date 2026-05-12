package polyglot

import (
	"fmt"
	"sync"
)

// TypedFunc is a function that accepts typed values and returns a typed value.
type TypedFunc func(args []Value) Value

// Registry holds named typed functions for each runtime.
type Registry struct {
	mu    sync.RWMutex
	funcs map[string]map[string]TypedFunc // runtime -> func_name -> func
}

// NewRegistry creates an empty typed function registry.
func NewRegistry() *Registry {
	return &Registry{
		funcs: make(map[string]map[string]TypedFunc),
	}
}

// Register adds a typed function to a runtime.
func (r *Registry) Register(runtime, name string, fn TypedFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.funcs[runtime] == nil {
		r.funcs[runtime] = make(map[string]TypedFunc)
	}
	r.funcs[runtime][name] = fn
}

// Call invokes a named function in the given runtime with typed args.
func (r *Registry) Call(runtime, name string, args []Value) Value {
	r.mu.RLock()
	fns, ok := r.funcs[runtime]
	if !ok {
		r.mu.RUnlock()
		return Error(fmt.Sprintf("runtime %q has no typed functions", runtime))
	}
	fn, ok := fns[name]
	r.mu.RUnlock()
	if !ok {
		return Error(fmt.Sprintf("function %q not found in runtime %q", name, runtime))
	}
	return fn(args)
}

// GlobalRegistry is the process-wide typed function registry.
var GlobalRegistry = NewRegistry()
