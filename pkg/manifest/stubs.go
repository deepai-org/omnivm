package manifest

import (
	"encoding/json"
	"fmt"
	"strconv"
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
// The bridge itself returns a string, but manifest return values are enveloped
// as JSON and decoded here so guest code receives native JS values.
func jsStub(funcName string, params []string) string {
	paramList := strings.Join(params, ", ")

	var argEntries []string
	for _, p := range params {
		argEntries = append(argEntries, p)
	}
	argsArray := "[" + strings.Join(argEntries, ", ") + "]"

	return fmt.Sprintf(`globalThis.__omnivm_decode_result = globalThis.__omnivm_decode_result || function(raw) {
  try {
    var env = JSON.parse(raw);
    if (env && env.__omnivm_result__ === true) return env.value;
  } catch (e) {}
  return raw;
};
globalThis.%s = function(%s) {
  var __req = JSON.stringify({func: "%s", args: %s});
  return globalThis.__omnivm_decode_result(omnivm.call("__manifest", __req));
};`, funcName, paramList, funcName, argsArray)
}

// pythonStub generates a Python function that calls back into the manifest executor.
// The bridge itself returns a string, but manifest return values are enveloped
// as JSON and decoded here so guest code receives native Python values.
func pythonStub(funcName string, params []string) string {
	paramList := strings.Join(params, ", ")

	var argEntries []string
	for _, p := range params {
		argEntries = append(argEntries, p)
	}
	argsArray := "[" + strings.Join(argEntries, ", ") + "]"

	return fmt.Sprintf(`def __omnivm_decode_result(raw):
    import json as __j
    try:
        env = __j.loads(raw)
        if isinstance(env, dict) and env.get('__omnivm_result__') is True:
            return env.get('value')
    except Exception:
        pass
    return raw

def %s(%s):
    import json as __j
    return __omnivm_decode_result(omnivm.call('__manifest', __j.dumps({'func': '%s', 'args': %s})))`, funcName, paramList, funcName, argsArray)
}

// rubyReserved is the set of Ruby keywords that cannot be used as parameter names.
var rubyReserved = map[string]bool{
	"end": true, "begin": true, "class": true, "def": true, "do": true,
	"if": true, "unless": true, "while": true, "until": true, "for": true,
	"case": true, "when": true, "module": true, "then": true, "else": true,
	"elsif": true, "ensure": true, "rescue": true, "retry": true, "return": true,
	"yield": true, "super": true, "self": true, "nil": true, "true": true,
	"false": true, "and": true, "or": true, "not": true, "in": true,
}

// rubyStub generates a Ruby function that calls back into the manifest executor.
// The bridge itself returns a string, but manifest return values are enveloped
// as JSON and decoded here so guest code receives native Ruby values.
// Parameter names that collide with Ruby keywords are suffixed with _ in the
// function signature, then mapped back to the original names in the args array.
func rubyStub(funcName string, params []string) string {
	// Build safe param names for the Ruby signature
	safeParams := make([]string, len(params))
	for i, p := range params {
		if rubyReserved[p] {
			safeParams[i] = p + "_"
		} else {
			safeParams[i] = p
		}
	}

	paramList := strings.Join(safeParams, ", ")
	argsArray := "[" + strings.Join(safeParams, ", ") + "]"

	return fmt.Sprintf(`def __omnivm_decode_result(raw)
  require 'json'
  begin
    env = JSON.parse(raw)
    return env["value"] if env.is_a?(Hash) && env["__omnivm_result__"] == true
  rescue
  end
  raw
end

def %s(%s)
  require 'json'
  __omnivm_decode_result(OmniVM.call('__manifest', JSON.generate({func: "%s", args: %s})))
end`, funcName, paramList, funcName, argsArray)
}

// HandleCall is invoked when the bridge receives a call to runtime "__manifest".
// It deserializes {func, args}, pushes a new scope, binds args to params,
// executes the func_def body, pops the scope, and returns the result.
func (e *Executor) HandleCall(code string) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("manifest HandleCall panic: %v", r)
		}
	}()

	// Deserialize the call request
	var req struct {
		Func string        `json:"func"`
		Args []interface{} `json:"args"`
	}
	if err := json.Unmarshal([]byte(code), &req); err != nil {
		return "", fmt.Errorf("manifest HandleCall: invalid request: %w", err)
	}

	// Check Go function registry first (Go plugins)
	if goFn, ok := e.goFuncs[req.Func]; ok {
		return e.callGoFuncFromBridge(req.Func, goFn, req.Args)
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

	// For generators, push a yield collector, run body, return collected values
	if fd.Generator {
		e.yieldCollectors = append(e.yieldCollectors, []interface{}{})
		_, bodyErr := e.executeOps(fd.Body)

		top := len(e.yieldCollectors) - 1
		collected := e.yieldCollectors[top]
		e.yieldCollectors = e.yieldCollectors[:top]

		// ErrReturn in a generator means "stop generating"
		if bodyErr != nil {
			if _, isReturn := bodyErr.(ErrReturn); !isReturn {
				return "", bodyErr
			}
		}

		return marshalResult(collected)
	}

	// Execute the function body
	_, err = e.executeOps(fd.Body)
	if ret, ok := err.(ErrReturn); ok {
		return marshalResult(ret.Value)
	}
	if err != nil {
		return "", err
	}

	return marshalResult(nil)
}

// callGoFuncFromBridge invokes a Go plugin function from a bridge call.
// Includes panic recovery since plugin code may have type assertion failures.
func (e *Executor) callGoFuncFromBridge(name string, fn interface{}, args []interface{}) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("go function %q panicked: %v", name, r)
		}
	}()

	// Normalize JSON number args: float64 → int where possible
	normalizedArgs := normalizeArgs(args)

	// Try func(interface{}) interface{} (single arg)
	if f, ok := fn.(func(interface{}) interface{}); ok {
		var arg interface{}
		if len(normalizedArgs) > 0 {
			arg = normalizedArgs[0]
		}
		res := f(arg)
		return marshalResult(res)
	}

	// Try func(interface{}, interface{}) interface{} (two args)
	if f, ok := fn.(func(interface{}, interface{}) interface{}); ok {
		var a, b interface{}
		if len(normalizedArgs) > 0 {
			a = normalizedArgs[0]
		}
		if len(normalizedArgs) > 1 {
			b = normalizedArgs[1]
		}
		res := f(a, b)
		return marshalResult(res)
	}

	// Try func([]interface{}) (interface{}, error)
	if f, ok := fn.(func([]interface{}) (interface{}, error)); ok {
		res, ferr := f(normalizedArgs)
		if ferr != nil {
			return "", ferr
		}
		return marshalResult(res)
	}

	// Try func([]interface{}) interface{}
	if f, ok := fn.(func([]interface{}) interface{}); ok {
		res := f(normalizedArgs)
		return marshalResult(res)
	}

	return "", fmt.Errorf("go function %q has unsupported signature", name)
}

// normalizeArgs converts values to Go-friendly types for plugin calls:
// - float64 with no fractional part → int
// - numeric strings → int or float64
// - RuntimeRef → unwrapped value (recursively normalized)
func normalizeArgs(args []interface{}) []interface{} {
	out := make([]interface{}, len(args))
	for i, arg := range args {
		out[i] = normalizeArg(arg)
	}
	return out
}

func normalizeArg(arg interface{}) interface{} {
	switch v := arg.(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v)
		}
		return v
	case string:
		// Try parsing numeric strings as numbers
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			if f == float64(int(f)) {
				return int(f)
			}
			return f
		}
		return v
	case RuntimeRef:
		return normalizeArg(v.Value)
	default:
		return arg
	}
}

type bridgeResultEnvelope struct {
	Marker bool        `json:"__omnivm_result__"`
	Kind   string      `json:"kind"`
	Value  interface{} `json:"value"`
}

// marshalResult converts a value to an enveloped JSON string suitable for a
// bridge return. Runtime stubs decode the envelope back into native values,
// which keeps JSON serialization out of user-level PolyScript code.
func marshalResult(val interface{}) (string, error) {
	// Unwrap RuntimeRef to get the actual value
	if ref, ok := val.(RuntimeRef); ok {
		val = ref.Value
	}
	kind := resultKind(val)
	env := bridgeResultEnvelope{
		Marker: true,
		Kind:   kind,
		Value:  val,
	}
	b, err := json.Marshal(env)
	if err == nil {
		return string(b), nil
	}

	// Last-resort fallback for non-JSON-marshalable values.
	env.Kind = "string"
	env.Value = fmt.Sprintf("%v", val)
	b, err = json.Marshal(env)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func resultKind(val interface{}) string {
	if val == nil {
		return "null"
	}
	switch val.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return "number"
	default:
		return "json"
	}
}
