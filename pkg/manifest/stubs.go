package manifest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// registerStubs installs callable stubs for a func_def in each available runtime.
// When guest code calls the function, the stub serializes the call as JSON and
// routes it through omnivm.call("__manifest", ...) back to HandleCall.
func (e *Executor) registerStubs(fd *FuncDef) error {
	paramNames := make([]string, len(fd.Params))
	for i, p := range fd.Params {
		paramNames[i] = p.Name
	}

	for name, rt := range e.runtimes {
		var code string
		switch name {
		case "javascript":
			code = jsStub(fd.Name, paramNames)
		case "python":
			code = pythonStub(fd.Name, paramNames)
		case "ruby":
			code = rubyStub(fd.Name, paramNames)
		default:
			continue // Java stubs deferred (complex compilation model)
		}

		result := rt.Execute(code)
		if result.Err != nil {
			return fmt.Errorf("register stub %q in %s: %w", fd.Name, name, result.Err)
		}
	}
	return nil
}

// jsStub generates a JavaScript function that calls back into the manifest executor.
// Returns the raw string result — caller decides whether to JSON.parse.
func jsStub(funcName string, params []string) string {
	paramList := strings.Join(params, ", ")

	var argEntries []string
	for _, p := range params {
		argEntries = append(argEntries, p)
	}
	argsArray := "[" + strings.Join(argEntries, ", ") + "]"

	return fmt.Sprintf(`globalThis.%s = function(%s) {
  var __req = JSON.stringify({func: "%s", args: %s});
  return omnivm.call("__manifest", __req);
};`, funcName, paramList, funcName, argsArray)
}

// pythonStub generates a Python function that calls back into the manifest executor.
// Returns the raw string result — caller decides whether to json.loads.
func pythonStub(funcName string, params []string) string {
	paramList := strings.Join(params, ", ")

	var argEntries []string
	for _, p := range params {
		argEntries = append(argEntries, p)
	}
	argsArray := "[" + strings.Join(argEntries, ", ") + "]"

	return fmt.Sprintf(`def %s(%s):
    import json as __j
    return omnivm.call('__manifest', __j.dumps({'func': '%s', 'args': %s}))`, funcName, paramList, funcName, argsArray)
}

// rubyStub generates a Ruby function that calls back into the manifest executor.
// Returns the raw string result — caller decides whether to JSON.parse.
func rubyStub(funcName string, params []string) string {
	paramList := strings.Join(params, ", ")

	var argEntries []string
	for _, p := range params {
		argEntries = append(argEntries, p)
	}
	argsArray := "[" + strings.Join(argEntries, ", ") + "]"

	return fmt.Sprintf(`def %s(%s)
  require 'json'
  OmniVM.call('__manifest', JSON.generate({func: "%s", args: %s}))
end`, funcName, paramList, funcName, argsArray)
}

// HandleCall is invoked when the bridge receives a call to runtime "__manifest".
// It deserializes {func, args}, pushes a new scope, binds args to params,
// executes the func_def body, pops the scope, and returns the result.
func (e *Executor) HandleCall(code string) (string, error) {
	// Deserialize the call request
	var req struct {
		Func string        `json:"func"`
		Args []interface{} `json:"args"`
	}
	if err := json.Unmarshal([]byte(code), &req); err != nil {
		return "", fmt.Errorf("manifest HandleCall: invalid request: %w", err)
	}

	fd, ok := e.funcs[req.Func]
	if !ok {
		return "", fmt.Errorf("manifest HandleCall: undefined function %q", req.Func)
	}

	// Push new scope for this invocation
	e.pushScope()
	defer e.popScope()

	// Bind args to params
	for i, param := range fd.Params {
		if param.Spread {
			// Spread: collect remaining args into a slice
			if i < len(req.Args) {
				e.setBinding(param.Name, req.Args[i:])
			} else {
				e.setBinding(param.Name, []interface{}{})
			}
			break
		}

		if i < len(req.Args) {
			e.setBinding(param.Name, req.Args[i])
		} else if param.DefaultValue != nil {
			e.setBinding(param.Name, param.DefaultValue)
		} else {
			e.setBinding(param.Name, nil)
		}
	}

	// Execute the function body
	_, err := e.executeOps(fd.Body)
	if ret, ok := err.(ErrReturn); ok {
		return marshalResult(ret.Value)
	}
	if err != nil {
		return "", err
	}

	return "", nil
}

// marshalResult converts a value to a string suitable for bridge return.
// The bridge always returns strings, so we format the value as a string.
func marshalResult(val interface{}) (string, error) {
	if val == nil {
		return "", nil
	}
	return fmt.Sprintf("%v", val), nil
}
