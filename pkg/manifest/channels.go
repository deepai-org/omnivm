package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// ChanRef wraps a Go channel for use as a manifest binding.
type ChanRef struct {
	ch     chan interface{}
	closed bool
}

// registerChannelBuiltins registers Go channel builtins in the goFuncs registry.
func (e *Executor) registerChannelBuiltins() {
	e.goFuncs["make"] = func(arg interface{}) interface{} {
		size := int(toFloat(arg))
		if size < 0 {
			size = 0
		}
		return &ChanRef{ch: make(chan interface{}, size)}
	}
}

// opChan handles channel make/send/recv/close operations.
func (e *Executor) opChan(op *Op) (interface{}, error) {
	// make action creates a new channel — no existing channel to resolve
	if op.Action == "make" {
		size := 0
		if op.Size != nil {
			size = int(toFloat(op.Size))
		}
		if size < 0 {
			size = 0
		}
		ch := &ChanRef{ch: make(chan interface{}, size)}
		if op.Bind != "" {
			e.setBinding(op.Bind, ch)
		}
		return ch, nil
	}

	chVal, ok := e.getBinding(op.Channel)
	if !ok {
		return nil, fmt.Errorf("chan %s: undefined channel %q", op.Action, op.Channel)
	}
	chRef, ok := chVal.(*ChanRef)
	if !ok {
		return nil, fmt.Errorf("chan %s: %q is not a channel (got %T)", op.Action, op.Channel, chVal)
	}

	switch op.Action {
	case "send":
		val, err := e.resolveValueExpr(op.Value)
		if err != nil {
			return nil, fmt.Errorf("chan send: %w", err)
		}
		// Non-blocking send to prevent deadlocks in single-threaded executor.
		// Buffered channels with capacity accept the value; full/unbuffered channels drop it.
		select {
		case chRef.ch <- val:
		default:
		}
		return nil, nil

	case "recv":
		// Non-blocking recv to prevent deadlocks in single-threaded executor.
		// Buffered channels with data return immediately; empty channels return nil.
		var val interface{}
		select {
		case v := <-chRef.ch:
			val = v
		default:
		}
		if op.Bind != "" {
			e.setBinding(op.Bind, val)
		}
		return val, nil

	case "close":
		if chRef.closed {
			return nil, fmt.Errorf("chan close: channel %q already closed", op.Channel)
		}
		close(chRef.ch)
		chRef.closed = true
		return nil, nil

	default:
		return nil, fmt.Errorf("chan: unknown action %q", op.Action)
	}
}

// opSelect implements Go-style select on channels using reflect.Select.
func (e *Executor) opSelect(op *Op) (interface{}, error) {
	var cases []reflect.SelectCase

	for _, sc := range op.Cases {
		chVal, ok := e.getBinding(sc.Channel)
		if !ok {
			return nil, fmt.Errorf("select: undefined channel %q", sc.Channel)
		}
		chRef, ok := chVal.(*ChanRef)
		if !ok {
			return nil, fmt.Errorf("select: %q is not a channel (got %T)", sc.Channel, chVal)
		}

		switch sc.Action {
		case "recv":
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(chRef.ch),
			})
		case "send":
			val, err := e.resolveValueExpr(sc.Value)
			if err != nil {
				return nil, fmt.Errorf("select send: %w", err)
			}
			var sendVal reflect.Value
			if val == nil {
				sendVal = reflect.Zero(reflect.TypeOf(chRef.ch).Elem())
			} else {
				sendVal = reflect.ValueOf(val)
			}
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectSend,
				Chan: reflect.ValueOf(chRef.ch),
				Send: sendVal,
			})
		default:
			return nil, fmt.Errorf("select: unknown action %q", sc.Action)
		}
	}

	// Only add default case when defaultBody is present (standard blocking otherwise)
	hasDefault := len(op.DefaultBody) > 0
	if hasDefault {
		cases = append(cases, reflect.SelectCase{
			Dir: reflect.SelectDefault,
		})
	}

	chosen, _, _ := reflect.Select(cases)

	if hasDefault && chosen == len(op.Cases) {
		return e.executeOps(op.DefaultBody)
	}

	return e.executeOps(op.Cases[chosen].Body)
}

// opSpawn launches a Go function in a new goroutine (best-effort).
func (e *Executor) opSpawn(op *Op) (interface{}, error) {
	code := strings.TrimSpace(op.Code)

	// Check for closure/arrow syntax — not supported
	if strings.HasPrefix(code, "()") {
		fmt.Fprintf(os.Stderr, "spawn: closure syntax not supported, skipping\n")
		return nil, nil
	}

	parenIdx := strings.Index(code, "(")
	if parenIdx < 0 {
		fmt.Fprintf(os.Stderr, "spawn: cannot parse %q\n", code)
		return nil, nil
	}

	funcName := strings.TrimSpace(code[:parenIdx])

	fn, ok := e.goFuncs[funcName]
	if !ok {
		// Check if funcName is a manifest func_def
		if _, isFuncDef := e.funcs[funcName]; isFuncDef {
			return e.spawnFuncDef(funcName, code[parenIdx+1:len(code)-1])
		}
		fmt.Fprintf(os.Stderr, "spawn: undefined function %q\n", funcName)
		return nil, nil
	}

	// Parse arguments
	argsStr := strings.TrimSpace(code[parenIdx+1 : len(code)-1])
	var args []interface{}
	if argsStr != "" {
		for _, part := range strings.Split(argsStr, ",") {
			part = strings.TrimSpace(part)
			if val, ok := e.getBinding(part); ok {
				if ref, ok := val.(RuntimeRef); ok {
					args = append(args, ref.Value)
				} else {
					args = append(args, val)
				}
			} else if f, err := strconv.ParseFloat(part, 64); err == nil {
				if f == float64(int(f)) {
					args = append(args, int(f))
				} else {
					args = append(args, f)
				}
			} else {
				part = strings.Trim(part, "\"'")
				args = append(args, part)
			}
		}
	}

	normalizedArgs := normalizeArgs(args)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "spawn: %q panicked: %v\n", funcName, r)
			}
		}()
		callGoFuncDirect(fn, normalizedArgs)
	}()

	return nil, nil
}

// spawnFuncDef executes a manifest func_def inline (single-threaded executor).
// Parses args from the call expression string, builds a HandleCall JSON request,
// and invokes the function body via HandleCall.
func (e *Executor) spawnFuncDef(funcName, argsStr string) (interface{}, error) {
	argsStr = strings.TrimSpace(argsStr)
	var args []interface{}
	if argsStr != "" {
		for _, part := range strings.Split(argsStr, ",") {
			part = strings.TrimSpace(part)
			if val, ok := e.getBinding(part); ok {
				if ref, ok := val.(RuntimeRef); ok {
					args = append(args, ref.Value)
				} else {
					args = append(args, val)
				}
			} else if f, err := strconv.ParseFloat(part, 64); err == nil {
				if f == float64(int(f)) {
					args = append(args, int(f))
				} else {
					args = append(args, f)
				}
			} else {
				part = strings.Trim(part, "\"'")
				args = append(args, part)
			}
		}
	}

	reqJSON, err := json.Marshal(map[string]interface{}{"func": funcName, "args": args})
	if err != nil {
		return nil, fmt.Errorf("spawn func_def: marshal request: %w", err)
	}
	_, err = e.HandleCall(string(reqJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: func_def %q error: %v\n", funcName, err)
	}
	return nil, nil
}

// callGoFuncDirect calls a Go function with the given args, ignoring the return value.
func callGoFuncDirect(fn interface{}, args []interface{}) {
	if f, ok := fn.(func(interface{}) interface{}); ok {
		var arg interface{}
		if len(args) > 0 {
			arg = args[0]
		}
		f(arg)
		return
	}
	if f, ok := fn.(func(interface{}, interface{}) interface{}); ok {
		var a, b interface{}
		if len(args) > 0 {
			a = args[0]
		}
		if len(args) > 1 {
			b = args[1]
		}
		f(a, b)
		return
	}
	if f, ok := fn.(func([]interface{}) (interface{}, error)); ok {
		f(args)
		return
	}
	if f, ok := fn.(func([]interface{}) interface{}); ok {
		f(args)
		return
	}
}
