package manifest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/handles"
)

// registerStubs installs callable stubs for a func_def in each available runtime.
// When guest code calls the function, the stub serializes the call as JSON and
// routes it through omnivm.call("__manifest", ...) back to HandleCall.
func (e *Executor) registerStubs(fd *FuncDef) error {
	paramNames := make([]string, len(fd.Params))
	for i, p := range fd.Params {
		paramNames[i] = p.Name
	}
	if e.javaStubFuncs == nil {
		e.javaStubFuncs = make(map[string]*FuncDef)
	}
	e.javaStubFuncs[fd.Name] = fd

	for name, rt := range e.runtimes {
		var code string
		switch name {
		case "javascript":
			code = jsStub(fd.Name, fd.Params)
		case "python":
			code = pythonStub(fd.Name, paramNames)
		case "ruby":
			code = rubyStub(fd.Name, paramNames)
		case "java":
			code = javaManifestStubs(e.javaStubFuncs)
		default:
			continue
		}

		result := rt.Execute(code)
		if result.Err != nil {
			return fmt.Errorf("register stub %q in %s: %w", fd.Name, name, result.Err)
		}
	}
	return nil
}

func javaManifestStubs(funcs map[string]*FuncDef) string {
	names := make([]string, 0, len(funcs))
	for name := range funcs {
		names = append(names, name)
	}
	sort.Strings(names)

	var methods []string
	for _, name := range names {
		fd := funcs[name]
		if !isJavaIdentifier(name) || isJavaReservedWord(name) {
			continue
		}
		params := make([]string, len(fd.Params))
		args := make([]string, len(fd.Params))
		used := make(map[string]int)
		for i, p := range fd.Params {
			paramName := javaSafeIdentifier(p.Name, i, used)
			params[i] = "Object " + paramName
			args[i] = paramName
		}
		callArgs := ""
		if len(args) > 0 {
			callArgs = ", " + strings.Join(args, ", ")
		}
		methods = append(methods, fmt.Sprintf(`  public static Object %s(%s) {
    return OmniVM.callManifest("%s"%s);
  }`, name, strings.Join(params, ", "), name, callArgs))
	}

	return `package omnivm;

public class OmniVMManifest {
  public static Object invoke(String func, Object... args) {
    return OmniVM.callManifest(func, args);
  }

` + strings.Join(methods, "\n\n") + `
}`
}

func javaSafeIdentifier(name string, index int, used map[string]int) string {
	if !isJavaIdentifier(name) || isJavaReservedWord(name) {
		name = fmt.Sprintf("__omnivm_arg_%d", index)
	}
	if used[name] == 0 {
		used[name] = 1
		return name
	}
	base := name
	for {
		suffix := used[base]
		used[base]++
		candidate := fmt.Sprintf("%s_%d", base, suffix)
		if used[candidate] == 0 {
			used[candidate] = 1
			return candidate
		}
	}
}

func isJavaIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !(r == '_' || r == '$' || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z')) {
				return false
			}
			continue
		}
		if !(r == '_' || r == '$' || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9')) {
			return false
		}
	}
	return true
}

func isJavaReservedWord(name string) bool {
	switch name {
	case "abstract", "assert", "boolean", "break", "byte", "case", "catch", "char", "class", "const",
		"continue", "default", "do", "double", "else", "enum", "extends", "final", "finally", "float",
		"for", "goto", "if", "implements", "import", "instanceof", "int", "interface", "long", "native",
		"new", "package", "private", "protected", "public", "return", "short", "static", "strictfp",
		"super", "switch", "synchronized", "this", "throw", "throws", "transient", "try", "void",
		"volatile", "while", "true", "false", "null", "_":
		return true
	default:
		return false
	}
}

func callableShapeForParams(params []*Param) *CallableShape {
	var shape CallableShape
	seenKeys := make(map[string]bool)
	for _, param := range params {
		if param == nil || param.CallableShape == nil {
			continue
		}
		if param.CallableShape.AcceptsKwargs {
			shape.AcceptsKwargs = true
		}
		if param.CallableShape.AcceptsOptionsObject {
			shape.AcceptsOptionsObject = true
		}
		for _, key := range param.CallableShape.DestructuredKeys {
			if key == "" || seenKeys[key] {
				continue
			}
			seenKeys[key] = true
			shape.DestructuredKeys = append(shape.DestructuredKeys, key)
		}
		for _, key := range param.CallableShape.ParameterNames {
			if key == "" || seenKeys[key] {
				continue
			}
			seenKeys[key] = true
			shape.ParameterNames = append(shape.ParameterNames, key)
		}
		if shape.Arity == nil && param.CallableShape.Arity != nil {
			arity := *param.CallableShape.Arity
			shape.Arity = &arity
		}
		if shape.JavaAdapter == nil && param.CallableShape.JavaAdapter != nil {
			adapter := *param.CallableShape.JavaAdapter
			if len(param.CallableShape.JavaAdapter.Keys) > 0 {
				adapter.Keys = append([]string(nil), param.CallableShape.JavaAdapter.Keys...)
			}
			shape.JavaAdapter = &adapter
		}
	}
	if !shape.AcceptsKwargs && !shape.AcceptsOptionsObject && len(shape.DestructuredKeys) == 0 && len(shape.ParameterNames) == 0 && shape.Arity == nil && shape.JavaAdapter == nil {
		return nil
	}
	return &shape
}

func callableShapeJSONForParams(params []*Param) string {
	shape := callableShapeForParams(params)
	if shape == nil {
		return ""
	}
	b, err := json.Marshal(shape)
	if err != nil {
		return ""
	}
	return string(b)
}

// jsStub generates a JavaScript function that calls back into the manifest executor.
// The bridge itself returns a string, but manifest return values are enveloped
// as JSON and decoded here so guest code receives native JS values.
func jsStub(funcName string, params []*Param) string {
	funcLiteral := strconv.Quote(funcName)
	shapeJSON := callableShapeJSONForParams(params)
	shapeAssignment := ""
	if shapeJSON != "" {
		shapeAssignment = fmt.Sprintf("\nglobalThis[%s].__omnivm_callable_shape__ = %s;", funcLiteral, shapeJSON)
	}

	return jsChannelMaterializer() + "\n" + fmt.Sprintf(`globalThis.__omnivm_decode_result = globalThis.__omnivm_decode_result || function(raw) {
  try {
    var env = JSON.parse(raw);
    if (env && env.__omnivm_result__ === true) return globalThis.__omnivm_materialize_capture(env.value);
  } catch (e) {}
  return raw;
};
globalThis.__omnivm_arg_refs = globalThis.__omnivm_arg_refs || {};
globalThis.__omnivm_arg_ref_counter = globalThis.__omnivm_arg_ref_counter || 0;
globalThis.__omnivm_encode_arg = globalThis.__omnivm_encode_arg || function(value) {
  if (value === null || value === undefined || typeof value === "string" || typeof value === "number" || typeof value === "boolean") return value;
  if (value && value.__omnivm_proxy__ === true && value.__omnivm_descriptor__) return value.__omnivm_descriptor__;
  var id = "arg_" + (++globalThis.__omnivm_arg_ref_counter);
  globalThis.__omnivm_arg_refs[id] = value;
  var descriptor = {__omnivm_runtime_ref__: true, runtime: "javascript", var: "__omnivm_arg_refs[" + JSON.stringify(id) + "]", callable: typeof value === "function"};
  if (typeof value === "function" && value.__omnivm_callable_shape__) descriptor.callable_shape = value.__omnivm_callable_shape__;
  return descriptor;
};
globalThis.__omnivm_manifest_invoke = globalThis.__omnivm_manifest_invoke || function(func, args) {
  var __req = JSON.stringify({func: func, args: args.map(globalThis.__omnivm_encode_arg)});
  return globalThis.__omnivm_decode_result(omnivm.call("__manifest", __req));
};
globalThis[%s] = function() {
  return globalThis.__omnivm_manifest_invoke(%s, Array.prototype.slice.call(arguments));
};%s`, funcLiteral, funcLiteral, shapeAssignment)
}

// pythonStub generates a Python function that calls back into the manifest executor.
// The bridge itself returns a string, but manifest return values are enveloped
// as JSON and decoded here so guest code receives native Python values.
func pythonStub(funcName string, params []string) string {
	_ = params
	funcLiteral := strconv.Quote(funcName)
	convenience := fmt.Sprintf(`globals()[%s] = lambda *__omnivm_args: __omnivm_manifest_invoke(%s, *__omnivm_args)
`, funcLiteral, funcLiteral)
	if isPythonIdentifier(funcName) && !isPythonReservedWord(funcName) {
		convenience = fmt.Sprintf(`def %s(*__omnivm_args):
    return __omnivm_manifest_invoke(%s, *__omnivm_args)
`, funcName, funcLiteral)
	}

	return pythonCaptureMaterializer() + "\n" + fmt.Sprintf(`def __omnivm_decode_result(raw):
    import json as __j
    try:
        env = __j.loads(raw)
        if isinstance(env, dict) and env.get('__omnivm_result__') is True:
            return globals()["__omnivm_materialize_capture"](env.get('value'))
    except Exception:
        pass
    return raw

__omnivm_arg_refs = globals().setdefault("__omnivm_arg_refs", {})
__omnivm_arg_ref_counter = globals().setdefault("__omnivm_arg_ref_counter", 0)

def __omnivm_encode_arg(value):
    global __omnivm_arg_ref_counter
    if value is None or isinstance(value, (bool, int, float, str)):
        return value
    descriptor = getattr(value, "_value", None)
    if isinstance(descriptor, dict) and (
        descriptor.get("__omnivm_resource__") is True
        or descriptor.get("__omnivm_table__") is True
        or descriptor.get("__omnivm_stream__") is True
        or descriptor.get("__omnivm_channel__") is True
        or descriptor.get("__omnivm_job__") is True
    ):
        return descriptor
    __omnivm_arg_ref_counter += 1
    __id = "arg_%%s" %% __omnivm_arg_ref_counter
    __omnivm_arg_refs[__id] = value
    return {"__omnivm_runtime_ref__": True, "runtime": "python", "var": "__omnivm_arg_refs[%%r]" %% __id, "callable": callable(value)}

def __omnivm_manifest_invoke(func, *__omnivm_args):
    import json as __j
    import omnivm
    return __omnivm_decode_result(omnivm.call('__manifest', __j.dumps({'func': func, 'args': [__omnivm_encode_arg(__arg) for __arg in __omnivm_args]})))

%s`, convenience)
}

func isPythonIdentifier(name string) bool {
	return isASCIIIdentifier(name, false)
}

func isPythonReservedWord(name string) bool {
	switch name {
	case "False", "None", "True", "and", "as", "assert", "async", "await", "break", "class", "continue",
		"def", "del", "elif", "else", "except", "finally", "for", "from", "global", "if", "import",
		"in", "is", "lambda", "nonlocal", "not", "or", "pass", "raise", "return", "try", "while",
		"with", "yield", "match", "case", "type":
		return true
	default:
		return false
	}
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
func rubyStub(funcName string, params []string) string {
	_ = params
	funcLiteral := strconv.Quote(funcName)
	convenience := fmt.Sprintf(`$__omnivm_manifest_funcs ||= {}
$__omnivm_manifest_funcs[%s] = proc { |*__omnivm_args| __omnivm_manifest_invoke(%s, *__omnivm_args) }
`, funcLiteral, funcLiteral)
	if isRubyIdentifier(funcName) && !rubyReserved[funcName] {
		convenience = fmt.Sprintf(`def %s(*__omnivm_args)
  __omnivm_manifest_invoke(%s, *__omnivm_args)
end
`, funcName, funcLiteral)
	}

	return "require 'json'\n" + rubyCaptureMaterializer() + "\n" + fmt.Sprintf(`def __omnivm_decode_result(raw)
  require 'json'
  begin
    env = JSON.parse(raw)
    return __omnivm_materialize_capture(env["value"]) if env.is_a?(Hash) && env["__omnivm_result__"] == true
  rescue
  end
  raw
end

$__omnivm_arg_refs ||= {}
$__omnivm_arg_ref_counter ||= 0

def __omnivm_encode_arg(value)
  return value if value.nil? || value.is_a?(String) || value.is_a?(Numeric) || value == true || value == false
  descriptor = value.instance_variable_get(:@value) if value.respond_to?(:instance_variable_get)
  if descriptor.is_a?(Hash) && (descriptor["__omnivm_resource__"] == true || descriptor["__omnivm_table__"] == true || descriptor["__omnivm_stream__"] == true || descriptor["__omnivm_channel__"] == true || descriptor["__omnivm_job__"] == true)
    return descriptor
  end
  $__omnivm_arg_ref_counter += 1
  id = "arg_#{$__omnivm_arg_ref_counter}"
  $__omnivm_arg_refs[id] = value
  {"__omnivm_runtime_ref__" => true, "runtime" => "ruby", "var" => "$__omnivm_arg_refs[#{id.inspect}]", "callable" => value.respond_to?(:call)}
end

def __omnivm_manifest_invoke(func, *__omnivm_args)
  require 'json'
  __omnivm_decode_result(OmniVM.call('__manifest', JSON.generate({func: func, args: __omnivm_args.map { |arg| __omnivm_encode_arg(arg) }})))
end

%s`, convenience)
}

func isRubyIdentifier(name string) bool {
	return isASCIIIdentifier(name, false)
}

func isASCIIIdentifier(name string, allowDollar bool) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 {
			if !(r == '_' || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z') || (allowDollar && r == '$')) {
				return false
			}
			continue
		}
		if !(r == '_' || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z') || ('0' <= r && r <= '9') || (allowDollar && r == '$')) {
			return false
		}
	}
	return true
}

// BridgeRequest is the JSON request shape accepted by runtime "__manifest".
type BridgeRequest struct {
	Func        string                 `json:"func"`
	Op          string                 `json:"op"`
	Args        []interface{}          `json:"args"`
	Kwargs      map[string]interface{} `json:"kwargs"`
	ID          interface{}            `json:"id"`
	From        interface{}            `json:"from"`
	To          interface{}            `json:"to"`
	Kind        string                 `json:"kind"`
	Mode        string                 `json:"mode"`
	Key         string                 `json:"key"`
	Value       interface{}            `json:"value"`
	Materialize bool                   `json:"materialize"`
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
	var req BridgeRequest
	if err := json.Unmarshal([]byte(code), &req); err != nil {
		return "", fmt.Errorf("manifest HandleCall: invalid request: %w", err)
	}

	if req.Op != "" {
		return e.handleInternalBridgeOp(req.Op, req)
	}
	req.Args = decodeRuntimeRefArgs(req.Args)

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

		return e.marshalReturnResult(collected)
	}

	// Execute the function body
	_, err = e.executeOps(fd.Body)
	if ret, ok := err.(ErrReturn); ok {
		return e.marshalReturnResult(ret.Value)
	}
	if err != nil {
		return "", err
	}

	return e.marshalReturnResult(nil)
}

func decodeRuntimeRefArgs(args []interface{}) []interface{} {
	out := make([]interface{}, len(args))
	for i, arg := range args {
		out[i] = decodeRuntimeRefArg(arg)
	}
	return out
}

func decodeRuntimeRefKwargs(kwargs map[string]interface{}) map[string]interface{} {
	if len(kwargs) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(kwargs))
	for key, value := range kwargs {
		out[key] = decodeRuntimeRefArg(value)
	}
	return out
}

func decodeRuntimeRefArg(arg interface{}) interface{} {
	switch v := arg.(type) {
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = decodeRuntimeRefArg(item)
		}
		return out
	case map[string]interface{}:
		if v["__omnivm_runtime_ref__"] == true {
			runtimeName, _ := v["runtime"].(string)
			varName, _ := v["var"].(string)
			if runtimeName != "" && varName != "" {
				ref := RuntimeRef{Runtime: runtimeName, VarName: varName}
				if callable, ok := v["callable"].(bool); ok {
					ref.CallableKnown = true
					ref.Callable = callable
				}
				ref.CallableShape = decodeCallableShape(v["callable_shape"])
				return ref
			}
		}
		out := make(map[string]interface{}, len(v))
		for key, item := range v {
			out[key] = decodeRuntimeRefArg(item)
		}
		return out
	default:
		return arg
	}
}

func decodeCallableShape(value interface{}) *CallableShape {
	m, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	var shape CallableShape
	if accepts, ok := m["acceptsKwargs"].(bool); ok {
		shape.AcceptsKwargs = accepts
	}
	if accepts, ok := m["acceptsOptionsObject"].(bool); ok {
		shape.AcceptsOptionsObject = accepts
	}
	if keys, ok := m["destructuredKeys"].([]interface{}); ok {
		for _, key := range keys {
			if s, ok := key.(string); ok && s != "" {
				shape.DestructuredKeys = append(shape.DestructuredKeys, s)
			}
		}
	}
	if keys, ok := m["parameterNames"].([]interface{}); ok {
		for _, key := range keys {
			if s, ok := key.(string); ok && s != "" {
				shape.ParameterNames = append(shape.ParameterNames, s)
			}
		}
	}
	if arity, ok := decodeInt(m["arity"]); ok {
		shape.Arity = &arity
	}
	if adapter := decodeJavaCallableAdapter(m["javaAdapter"]); adapter != nil {
		shape.JavaAdapter = adapter
	}
	if !shape.AcceptsKwargs && !shape.AcceptsOptionsObject && len(shape.DestructuredKeys) == 0 && len(shape.ParameterNames) == 0 && shape.Arity == nil && shape.JavaAdapter == nil {
		return nil
	}
	return &shape
}

func decodeInt(value interface{}) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
	}
}

func decodeJavaCallableAdapter(value interface{}) *JavaCallableAdapter {
	m, ok := value.(map[string]interface{})
	if !ok {
		return nil
	}
	var adapter JavaCallableAdapter
	if kind, ok := m["kind"].(string); ok {
		adapter.Kind = kind
	}
	if method, ok := m["method"].(string); ok {
		adapter.Method = method
	}
	if targetType, ok := m["targetType"].(string); ok {
		adapter.TargetType = targetType
	}
	if keys, ok := m["keys"].([]interface{}); ok {
		for _, key := range keys {
			if s, ok := key.(string); ok && s != "" {
				adapter.Keys = append(adapter.Keys, s)
			}
		}
	}
	if adapter.Kind == "" && adapter.Method == "" && adapter.TargetType == "" && len(adapter.Keys) == 0 {
		return nil
	}
	return &adapter
}

func (e *Executor) marshalReturnResult(val interface{}) (string, error) {
	normalized, err := e.bridgeReturnValue(val)
	if err != nil {
		return "", err
	}
	return marshalResult(normalized)
}

func (e *Executor) bridgeReturnValue(val interface{}) (interface{}, error) {
	if stream, ok, err := e.bridgeReturnStreamValue(val, "go"); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return stream, nil
	}
	if ref, ok, err := e.bridgeReturnBulkTableValue(val); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return transferTableProxyValue(ref), nil
	}
	if ref, ok, err := e.bridgeReturnResourceValue(val); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return ref, nil
	}
	switch v := val.(type) {
	case RuntimeRef:
		return e.bridgeReturnRuntimeRef(v)
	case *RuntimeRef:
		if v == nil {
			return nil, nil
		}
		return e.bridgeReturnRuntimeRef(*v)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			next, err := e.bridgeReturnValue(item)
			if err != nil {
				return nil, err
			}
			out = append(out, next)
		}
		return out, nil
	case *ResourceRef:
		if v != nil && v.ID != 0 {
			if err := e.ensureHandleTable().Escape(v.ID); err != nil {
				return nil, err
			}
			return transferResourceProxyValue(v), nil
		}
		return nil, nil
	case ResourceRef:
		if v.ID != 0 {
			if err := e.ensureHandleTable().Escape(v.ID); err != nil {
				return nil, err
			}
			return transferResourceProxyValue(&v), nil
		}
		return resourceProxyValue(&v), nil
	case *TableRef:
		if v != nil && v.ID != 0 {
			if err := e.ensureHandleTable().Escape(v.ID); err != nil {
				return nil, err
			}
			return transferTableProxyValue(v), nil
		}
		return nil, nil
	case TableRef:
		if v.ID != 0 {
			if err := e.ensureHandleTable().Escape(v.ID); err != nil {
				return nil, err
			}
			return transferTableProxyValue(&v), nil
		}
		return tableProxyValue(&v), nil
	case *JobHandle:
		return val, nil
	case JobHandle:
		return val, nil
	case map[string]interface{}:
		if isBridgeMarker(v) {
			if err := e.escapeBridgeReturnHandle(v); err != nil {
				return nil, err
			}
			return v, nil
		}
		out := make(map[string]interface{}, len(v))
		for key, item := range v {
			next, err := e.bridgeReturnValue(item)
			if err != nil {
				return nil, err
			}
			out[key] = next
		}
		return out, nil
	default:
		return val, nil
	}
}

func (e *Executor) bridgeReturnResourceValue(value interface{}) (map[string]interface{}, bool, error) {
	ref, ok, err := e.autoResourceRefForCapture(value)
	if err != nil || !ok {
		return nil, ok, err
	}
	if ref.ID != 0 {
		if err := e.ensureHandleTable().Escape(ref.ID); err != nil {
			return nil, true, err
		}
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.ResourceProxyCaptures++
	})
	return transferResourceProxyValue(ref), true, nil
}

func (e *Executor) bridgeReturnStreamValue(value interface{}, runtime string) (map[string]interface{}, bool, error) {
	kind := "channel"
	var id handles.ID
	var err error
	switch v := value.(type) {
	case *ChanRef:
		id, err = e.channelStreamHandle(v)
	default:
		if !isReceivableChannelValue(value) && !isReaderStreamValue(value) {
			return nil, false, nil
		}
		kind = streamKindForValue(value)
		id, err = e.genericStreamHandle(runtime, value)
	}
	if err != nil {
		return nil, true, err
	}
	if err := e.ensureHandleTable().Escape(id); err != nil {
		return nil, true, err
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.StreamProxyCaptures++
	})
	return transferStreamProxyValue(id, runtime, kind), true, nil
}

func (e *Executor) bridgeReturnBulkTableValue(val interface{}) (*TableRef, bool, error) {
	ref, ok, err := e.autoBulkTableRefForCapture(val)
	if err != nil || !ok {
		return nil, ok, err
	}
	if ref.ID != 0 {
		if err := e.ensureHandleTable().Escape(ref.ID); err != nil {
			return nil, true, err
		}
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.TableProxyCaptures++
		stats.ArrowTransfers++
	})
	return ref, true, nil
}

func (e *Executor) escapeBridgeReturnHandle(value map[string]interface{}) error {
	id, ok := bridgeMarkerHandleID(value)
	if !ok {
		return nil
	}
	if _, live := e.ensureHandleTable().Get(id); !live {
		return nil
	}
	if err := e.ensureHandleTable().Escape(id); err != nil {
		return err
	}
	value["transfer"] = true
	return nil
}

func (e *Executor) bridgeReturnRuntimeRef(ref RuntimeRef) (interface{}, error) {
	if jsonVal, ok, err := e.runtimeRefBulkTableCaptureJSON("", "", ref); ok || err != nil {
		if err != nil {
			return nil, err
		}
		var descriptor interface{}
		if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
			return nil, err
		}
		if value, ok := descriptor.(map[string]interface{}); ok {
			if err := e.escapeBridgeReturnHandle(value); err != nil {
				return nil, err
			}
		}
		return descriptor, nil
	}
	if stream, err := e.runtimeRefIsStream(ref); err != nil {
		return nil, err
	} else if stream {
		id, err := e.runtimeRefStreamHandle(ref)
		if err != nil {
			return nil, err
		}
		if err := e.ensureHandleTable().Escape(id); err != nil {
			return nil, err
		}
		e.addBoundaryStat(func(stats *BoundaryStats) {
			stats.StreamProxyCaptures++
		})
		return transferStreamProxyValue(id, ref.Runtime, "stream"), nil
	}
	if runtimeRefNeedsProxy(ref) {
		jsonVal, err := e.runtimeRefProxyCaptureJSON(ref)
		if err != nil {
			return nil, err
		}
		var descriptor interface{}
		if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
			return nil, err
		}
		if value, ok := descriptor.(map[string]interface{}); ok {
			if err := e.escapeBridgeReturnHandle(value); err != nil {
				return nil, err
			}
		}
		return descriptor, nil
	}
	return ref.Value, nil
}

func runtimeRefNeedsProxy(ref RuntimeRef) bool {
	if ref.Runtime == "" || ref.VarName == "" {
		return false
	}
	if ref.Opaque {
		return true
	}
	if ref.SnapshotKnown {
		return !isBridgePrimitive(ref.Value)
	}
	return ref.Value == nil || !isBridgePrimitive(ref.Value)
}

func (e *Executor) handleInternalBridgeOp(op string, req BridgeRequest) (string, error) {
	switch op {
	case "handle_retain":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		if err := e.ensureHandleTable().Retain(id); err != nil {
			return "", err
		}
		return marshalResult(true)
	case "handle_adopt":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		if err := e.ensureHandleTable().Retain(id); err != nil {
			return "", err
		}
		if err := e.ensureHandleTable().Release(id); err != nil {
			return "", err
		}
		return marshalResult(true)
	case "handle_access":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		report, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: req.Kind})
		if err != nil {
			return "", err
		}
		return marshalResult(map[string]interface{}{
			"id":                          uint64(report.ID),
			"runtime":                     report.Runtime,
			"kind":                        report.Kind,
			"access_kind":                 report.AccessKind,
			"accesses":                    report.Accesses,
			"kind_accesses":               report.KindAccesses,
			"chattiest_access_kind":       report.ChattiestAccessKind,
			"chattiest_access_kind_count": report.ChattiestAccessKindCount,
			"chatty":                      report.Chatty,
		})
	case "handle_reference":
		from, err := bridgeHandleID(req.From)
		if err != nil {
			return "", err
		}
		to, err := bridgeHandleID(req.To)
		if err != nil {
			return "", err
		}
		report, err := e.ensureHandleTable().RecordReference(from, to, req.Kind)
		if err != nil {
			return "", err
		}
		return marshalResult(map[string]interface{}{
			"from": uint64(report.From),
			"to":   uint64(report.To),
			"kind": report.Kind,
		})
	case "handle_drop_reference":
		from, err := bridgeHandleID(req.From)
		if err != nil {
			return "", err
		}
		to, err := bridgeHandleID(req.To)
		if err != nil {
			return "", err
		}
		e.ensureHandleTable().DropReference(from, to)
		return marshalResult(true)
	case "handle_release_finalizer":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		return marshalResult(e.ensureHandleTable().QueueReleaseFromFinalizer(id))
	case "handle_get":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		value, ok, err := e.handleProperty(id, req.Key)
		if err != nil {
			return "", err
		}
		if !ok {
			ok, err = e.handleCallable(id, req.Key)
			if err != nil {
				return "", err
			}
			if ok {
				descriptor := map[string]interface{}{
					"__omnivm_callable__": true,
					"key":                 req.Key,
				}
				zeroArg, err := e.handleZeroArgCallable(id, req.Key)
				if err != nil {
					return "", err
				}
				if zeroArg {
					descriptor["zeroArg"] = true
				}
				return marshalResult(descriptor)
			}
		}
		if !ok {
			return "", fmt.Errorf("manifest HandleCall: handle %d has no property %q", id, req.Key)
		}
		return e.marshalBridgeResult(id, value)
	case "handle_index":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		req.Value = decodeRuntimeRefArg(req.Value)
		value, ok, err := e.handleIndex(id, req.Value)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("manifest HandleCall: handle %d has no index %v", id, req.Value)
		}
		return e.marshalBridgeResult(id, value)
	case "handle_len":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		value, ok, err := e.handleLen(id)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("manifest HandleCall: handle %d has no length", id)
		}
		return marshalResult(value)
	case "handle_iter":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		values, ok, err := e.handleIter(id, req.Mode)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("manifest HandleCall: handle %d is not iterable", id)
		}
		if req.Materialize {
			e.addBoundaryStat(func(stats *BoundaryStats) {
				stats.ProxyMaterializations++
			})
		}
		return e.marshalBridgeIterResult(id, req.Mode, values)
	case "handle_contains":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		req.Value = decodeRuntimeRefArg(req.Value)
		ok, found, err := e.handleContains(id, req.Value)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("manifest HandleCall: handle %d does not support contains", id)
		}
		return marshalResult(found)
	case "stream_next":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		value, done, ok, err := e.handleStreamNext(id)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("manifest HandleCall: handle %d is not a stream", id)
		}
		if done {
			return marshalResult(map[string]interface{}{"done": true})
		}
		wrapped, err := e.bridgeResultValue(id, value)
		if err != nil {
			return "", err
		}
		return marshalResult(map[string]interface{}{
			"done":  false,
			"value": wrapped,
		})
	case "stream_cancel":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		if err := e.ensureHandleTable().ReleaseAllRefs(id); err != nil {
			return "", err
		}
		return marshalResult(true)
	case "handle_set":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		req.Value = decodeRuntimeRefArg(req.Value)
		result, err := e.handleSet(id, req.Key, req.Value)
		if err != nil {
			return "", err
		}
		if !result.OK {
			return "", fmt.Errorf("manifest HandleCall: handle %d has no writable property %q", id, req.Key)
		}
		if err := e.reconcileSetReferences(id, result.OldValue, result.HadOldValue, req.Value); err != nil {
			return "", err
		}
		return marshalResult(true)
	case "handle_call":
		id, err := bridgeHandleID(req.ID)
		if err != nil {
			return "", err
		}
		args := decodeRuntimeRefArgs(req.Args)
		kwargs := decodeRuntimeRefKwargs(req.Kwargs)
		if err := e.recordValueReferences(id, args, "call_arg"); err != nil {
			return "", err
		}
		if err := e.recordValueReferences(id, kwargs, "call_arg"); err != nil {
			return "", err
		}
		value, err := e.handleMethodCall(id, req.Key, args, kwargs)
		if err != nil {
			return "", err
		}
		return e.marshalBridgeResult(id, value)
	default:
		return "", fmt.Errorf("manifest HandleCall: unknown internal op %q", op)
	}
}

func (e *Executor) handleProperty(id handles.ID, key string) (interface{}, bool, error) {
	if key == "" {
		return nil, false, fmt.Errorf("manifest HandleCall: empty handle property")
	}
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return nil, false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "property"}); err != nil {
		return nil, false, err
	}

	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		return e.runtimeRefProperty(id, ref, key)
	}
	return genericProperty(entry.Value, key)
}

func (e *Executor) handleCallable(id handles.ID, key string) (bool, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		if key == "" {
			return e.runtimeRefTargetCallable(ref)
		}
		return e.runtimeRefCallable(ref, key)
	}
	return genericCallable(entry.Value, key), nil
}

func (e *Executor) handleZeroArgCallable(id handles.ID, key string) (bool, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		switch ref.Runtime {
		case "ruby":
			return e.runtimeRefRubyZeroArgMethod(ref, key)
		case "java":
			return e.runtimeRefJavaZeroArgMethod(ref, key)
		}
	}
	return genericZeroArgCallable(entry.Value, key), nil
}

func (e *Executor) marshalBridgeResult(parent handles.ID, value interface{}) (string, error) {
	wrapped, err := e.bridgeResultValue(parent, value)
	if err != nil {
		return "", err
	}
	return marshalResult(wrapped)
}

func (e *Executor) marshalBridgeListResult(parent handles.ID, values []interface{}) (string, error) {
	wrapped := make([]interface{}, 0, len(values))
	for _, value := range values {
		next, err := e.bridgeListValue(parent, value)
		if err != nil {
			return "", err
		}
		wrapped = append(wrapped, next)
	}
	return marshalResult(wrapped)
}

func (e *Executor) marshalBridgeIterResult(parent handles.ID, mode string, values []interface{}) (string, error) {
	wrapped := make([]interface{}, 0, len(values))
	for _, value := range values {
		if mode == "items" {
			pair, ok := value.([]interface{})
			if ok && len(pair) == 2 {
				key, err := e.bridgeResultValue(parent, pair[0])
				if err != nil {
					return "", err
				}
				itemValue, err := e.bridgeResultValue(parent, pair[1])
				if err != nil {
					return "", err
				}
				wrapped = append(wrapped, []interface{}{key, itemValue})
				continue
			}
		}
		next, err := e.bridgeResultValue(parent, value)
		if err != nil {
			return "", err
		}
		wrapped = append(wrapped, next)
	}
	return marshalResult(wrapped)
}

func (e *Executor) bridgeListValue(parent handles.ID, value interface{}) (interface{}, error) {
	if items, ok := value.([]interface{}); ok {
		out := make([]interface{}, 0, len(items))
		for _, item := range items {
			next, err := e.bridgeListValue(parent, item)
			if err != nil {
				return nil, err
			}
			out = append(out, next)
		}
		return out, nil
	}
	return e.bridgeResultValue(parent, value)
}

func (e *Executor) bridgeResultValue(parent handles.ID, value interface{}) (interface{}, error) {
	if isBridgePrimitive(value) {
		return value, nil
	}
	parentEntry, ok := e.ensureHandleTable().Get(parent)
	if !ok {
		return nil, fmt.Errorf("manifest HandleCall: unknown handle %d", parent)
	}
	if descriptor, ok := value.(map[string]interface{}); ok && isBridgeMarker(descriptor) {
		if id, ok := bridgeMarkerHandleID(descriptor); ok {
			if err := e.recordExistingHandleReference(parent, id, "property"); err != nil {
				return nil, err
			}
		}
		return value, nil
	}
	if ref, ok, err := e.autoBulkTableRefForCapture(value); ok || err != nil {
		if err != nil {
			return nil, err
		}
		if _, err := e.ensureHandleTable().RecordReference(parent, ref.ID, "property"); err != nil {
			return nil, err
		}
		e.addBoundaryStat(func(stats *BoundaryStats) {
			stats.TableProxyCaptures++
			stats.ArrowTransfers++
		})
		return tableProxyValue(ref), nil
	}
	switch v := value.(type) {
	case tableBufferRow:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			next, err := e.bridgeListValue(parent, item)
			if err != nil {
				return nil, err
			}
			out = append(out, next)
		}
		return out, nil
	case RuntimeRef:
		return e.bridgeResultRuntimeRef(parent, v)
	case *RuntimeRef:
		if v == nil {
			return nil, nil
		}
		return e.bridgeResultRuntimeRef(parent, *v)
	case *ResourceRef:
		if v == nil {
			return nil, nil
		}
		if err := e.recordExistingHandleReference(parent, v.ID, "property"); err != nil {
			return nil, err
		}
		return resourceProxyValue(v), nil
	case ResourceRef:
		if err := e.recordExistingHandleReference(parent, v.ID, "property"); err != nil {
			return nil, err
		}
		return resourceProxyValue(&v), nil
	case *TableRef:
		if v == nil {
			return nil, nil
		}
		if err := e.recordExistingHandleReference(parent, v.ID, "property"); err != nil {
			return nil, err
		}
		return tableProxyValue(v), nil
	case TableRef:
		if err := e.recordExistingHandleReference(parent, v.ID, "property"); err != nil {
			return nil, err
		}
		return tableProxyValue(&v), nil
	case *JobHandle:
		if v == nil {
			return nil, nil
		}
		if err := e.recordExistingHandleReference(parent, handles.ID(v.ID), "property"); err != nil {
			return nil, err
		}
		return jobProxyValue(v), nil
	case JobHandle:
		if err := e.recordExistingHandleReference(parent, handles.ID(v.ID), "property"); err != nil {
			return nil, err
		}
		return jobProxyValue(&v), nil
	}
	if id, ok := e.bridgeHandleForValue(parentEntry.Runtime, value); ok {
		if _, err := e.ensureHandleTable().RecordReference(parent, id, "property"); err != nil {
			return nil, err
		}
		return e.handleDescriptorValue(id)
	}

	if isReceivableChannelValue(value) || isReaderStreamValue(value) {
		id, err := e.genericStreamHandle(parentEntry.Runtime, value)
		if err != nil {
			return nil, err
		}
		if _, err := e.ensureHandleTable().RecordReference(parent, id, "property"); err != nil {
			return nil, err
		}
		e.addBoundaryStat(func(stats *BoundaryStats) {
			stats.StreamProxyCaptures++
		})
		return streamProxyValue(id, parentEntry.Runtime, streamKindForValue(value)), nil
	}

	ref := &ResourceRef{
		Runtime: parentEntry.Runtime,
		Kind:    bridgeResultKind(value),
		Value:   value,
	}
	var id handles.ID
	id, err := e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: parentEntry.Runtime,
		Kind:    ref.Kind,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			ref.Closed = true
			e.forgetReleasedHandle(id, ref)
			if proxy, ok := value.(*cSharedObjectProxy); ok {
				return proxy.Release()
			}
			if proxy, ok := value.(cSharedObjectProxy); ok {
				return proxy.Release()
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	ref.ID = id
	e.resources[id] = ref
	if ident, ok := bridgeIdentityForValue(value); ok {
		e.bridgeHandles[ident] = id
	}
	if err := e.recordValueReferences(id, value, "property"); err != nil {
		return nil, err
	}
	if _, err := e.ensureHandleTable().RecordReference(parent, id, "property"); err != nil {
		return nil, err
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.ResourceProxyCaptures++
	})
	return resourceProxyValue(ref), nil
}

func (e *Executor) bridgeResultRuntimeRef(parent handles.ID, ref RuntimeRef) (interface{}, error) {
	if ref.SnapshotKnown && !ref.Opaque && isBridgePrimitive(ref.Value) {
		return ref.Value, nil
	}
	if jsonVal, ok, err := e.runtimeRefBulkTableCaptureJSON("", "", ref); ok || err != nil {
		if err != nil {
			return nil, err
		}
		var descriptor map[string]interface{}
		if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
			return nil, err
		}
		if id, ok := bridgeMarkerHandleID(descriptor); ok {
			if err := e.recordExistingHandleReference(parent, id, "property"); err != nil {
				return nil, err
			}
		}
		return descriptor, nil
	}
	if jsonVal, ok, err := e.runtimeRefStreamCaptureJSON("", "", ref); ok || err != nil {
		if err != nil {
			return nil, err
		}
		var descriptor map[string]interface{}
		if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
			return nil, err
		}
		if id, ok := bridgeMarkerHandleID(descriptor); ok {
			if err := e.recordExistingHandleReference(parent, id, "property"); err != nil {
				return nil, err
			}
		}
		return descriptor, nil
	}
	if runtimeRefNeedsProxy(ref) {
		jsonVal, err := e.runtimeRefProxyCaptureJSON(ref)
		if err != nil {
			return nil, err
		}
		var descriptor map[string]interface{}
		if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
			return nil, err
		}
		if id, ok := bridgeMarkerHandleID(descriptor); ok {
			if err := e.recordExistingHandleReference(parent, id, "property"); err != nil {
				return nil, err
			}
		}
		return descriptor, nil
	}
	return e.bridgeResultValue(parent, ref.Value)
}

func (e *Executor) handleDescriptorValue(id handles.ID) (interface{}, error) {
	if ref := e.resources[id]; ref != nil {
		return resourceProxyValue(ref), nil
	}
	if ref := e.tables[id]; ref != nil {
		return tableProxyValue(ref), nil
	}
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return nil, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	switch v := entry.Value.(type) {
	case *ResourceRef:
		return resourceProxyValue(v), nil
	case ResourceRef:
		return resourceProxyValue(&v), nil
	case *TableRef:
		return tableProxyValue(v), nil
	case TableRef:
		return tableProxyValue(&v), nil
	case *JobHandle:
		return jobProxyValue(v), nil
	case JobHandle:
		return jobProxyValue(&v), nil
	}
	if entry.Kind == "channel" || entry.Kind == "stream" || entry.Kind == "reader" ||
		isReceivableChannelValue(entry.Value) || isReaderStreamValue(entry.Value) {
		return streamProxyValue(id, entry.Runtime, entry.Kind), nil
	}
	return resourceProxyValue(&ResourceRef{
		ID:      id,
		Runtime: entry.Runtime,
		Kind:    entry.Kind,
		Value:   entry.Value,
	}), nil
}

func (e *Executor) bridgeHandleForValue(runtime string, value interface{}) (handles.ID, bool) {
	ident, ok := bridgeIdentityForValue(value)
	if !ok {
		return 0, false
	}
	id, ok := e.bridgeHandles[ident]
	if !ok {
		return 0, false
	}
	entry, live := e.ensureHandleTable().Get(id)
	if !live || entry.Runtime != runtime {
		delete(e.bridgeHandles, ident)
		return 0, false
	}
	return id, true
}

func bridgeIdentityForValue(value interface{}) (bridgeIdentity, bool) {
	switch v := value.(type) {
	case *cSharedObjectProxy:
		if v == nil || v.objectID == "" {
			return bridgeIdentity{}, false
		}
		return bridgeIdentity{typ: "GoCSharedObject", key: strconv.FormatUint(uint64(v.handle), 10) + "\x00" + v.objectID}, true
	case cSharedObjectProxy:
		if v.objectID == "" {
			return bridgeIdentity{}, false
		}
		return bridgeIdentity{typ: "GoCSharedObject", key: strconv.FormatUint(uint64(v.handle), 10) + "\x00" + v.objectID}, true
	case RuntimeRef:
		if v.Runtime == "" || v.VarName == "" {
			return bridgeIdentity{}, false
		}
		return bridgeIdentity{typ: "RuntimeRef", key: v.Runtime + "\x00" + v.VarName}, true
	case *RuntimeRef:
		if v == nil || v.Runtime == "" || v.VarName == "" {
			return bridgeIdentity{}, false
		}
		return bridgeIdentity{typ: "RuntimeRef", key: v.Runtime + "\x00" + v.VarName}, true
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return bridgeIdentity{}, false
	}
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return bridgeIdentity{}, false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		if rv.IsNil() {
			return bridgeIdentity{}, false
		}
		ptr := rv.Pointer()
		if ptr == 0 {
			return bridgeIdentity{}, false
		}
		return bridgeIdentity{typ: rv.Type().String(), ptr: ptr}, true
	default:
		return bridgeIdentity{}, false
	}
}

func isBridgePrimitive(value interface{}) bool {
	if value == nil {
		return true
	}
	switch value.(type) {
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, json.Number:
		return true
	default:
		return false
	}
}

func isBridgeMarker(value interface{}) bool {
	v, ok := value.(map[string]interface{})
	if !ok {
		return false
	}
	return v["__omnivm_callable__"] == true ||
		v["__omnivm_resource__"] == true ||
		v["__omnivm_table__"] == true ||
		v["__omnivm_job__"] == true ||
		v["__omnivm_stream__"] == true ||
		v["__omnivm_channel__"] == true
}

func bridgeResultKind(value interface{}) string {
	switch v := value.(type) {
	case *cSharedObjectProxy:
		return v.Kind()
	case cSharedObjectProxy:
		return v.Kind()
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return "object"
	}
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "object"
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		return "map"
	case reflect.Slice, reflect.Array:
		return "sequence"
	case reflect.Func:
		return "callable"
	case reflect.Chan:
		return "channel"
	default:
		return "object"
	}
}

func (e *Executor) handleIndex(id handles.ID, key interface{}) (interface{}, bool, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return nil, false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "index"}); err != nil {
		return nil, false, err
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		return e.runtimeRefIndex(id, ref, key)
	}
	return genericIndex(entry.Value, key)
}

type handleSetResult struct {
	OK          bool
	OldValue    interface{}
	HadOldValue bool
}

func (e *Executor) handleSet(id handles.ID, key string, value interface{}) (handleSetResult, error) {
	if key == "" {
		return handleSetResult{}, fmt.Errorf("manifest HandleCall: empty handle property")
	}
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return handleSetResult{}, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "mutation"}); err != nil {
		return handleSetResult{}, err
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		ok, err := e.runtimeRefSet(ref, key, value)
		return handleSetResult{OK: ok}, err
	}
	oldValue, hadOldValue, err := oldValueForSet(entry.Value, key)
	if err != nil {
		return handleSetResult{}, err
	}
	ok, err = genericSet(entry.Value, key, value)
	if err != nil {
		return handleSetResult{}, err
	}
	return handleSetResult{
		OK:          ok,
		OldValue:    oldValue,
		HadOldValue: hadOldValue,
	}, nil
}

func (e *Executor) handleSetForProxy(id handles.ID, key string, value interface{}) (bool, error) {
	result, err := e.handleSet(id, key, value)
	if err != nil || !result.OK {
		return result.OK, err
	}
	if err := e.reconcileSetReferences(id, result.OldValue, result.HadOldValue, value); err != nil {
		return false, err
	}
	return true, nil
}

func oldValueForSet(value interface{}, key string) (interface{}, bool, error) {
	if oldValue, ok, err := genericProperty(value, key); err != nil || ok {
		return oldValue, ok, err
	}
	return genericIndex(value, key)
}

func (e *Executor) reconcileSetReferences(parent handles.ID, oldValue interface{}, hadOldValue bool, newValue interface{}) error {
	if hadOldValue {
		e.dropStaleSetReferences(parent, oldValue)
	}
	return e.recordSetReferences(parent, newValue)
}

func (e *Executor) dropStaleSetReferences(parent handles.ID, oldValue interface{}) {
	oldIDs := make(map[handles.ID]struct{})
	e.collectExistingValueReferenceIDs(oldValue, oldIDs)
	if len(oldIDs) == 0 {
		return
	}
	currentIDs := make(map[handles.ID]struct{})
	if entry, ok := e.ensureHandleTable().Get(parent); ok {
		e.collectContainedReferenceIDs(entry.Value, currentIDs)
	}
	for id := range oldIDs {
		if _, stillReferenced := currentIDs[id]; !stillReferenced {
			e.ensureHandleTable().DropReference(parent, id)
		}
	}
}

func (e *Executor) recordSetReferences(parent handles.ID, value interface{}) error {
	return e.recordValueReferences(parent, value, "mutation")
}

func (e *Executor) recordValueReferences(parent handles.ID, value interface{}, kind string) error {
	switch v := value.(type) {
	case RuntimeRef:
		return e.recordRuntimeRefReference(parent, v, kind)
	case *RuntimeRef:
		if v == nil {
			return nil
		}
		return e.recordRuntimeRefReference(parent, *v, kind)
	case *ResourceRef:
		if v == nil {
			return nil
		}
		return e.recordExistingHandleReference(parent, v.ID, kind)
	case ResourceRef:
		return e.recordExistingHandleReference(parent, v.ID, kind)
	case *TableRef:
		if v == nil {
			return nil
		}
		return e.recordExistingHandleReference(parent, v.ID, kind)
	case TableRef:
		return e.recordExistingHandleReference(parent, v.ID, kind)
	case *JobHandle:
		if v == nil {
			return nil
		}
		return e.recordExistingHandleReference(parent, handles.ID(v.ID), kind)
	case JobHandle:
		return e.recordExistingHandleReference(parent, handles.ID(v.ID), kind)
	case *GoHandleProxy:
		if v == nil {
			return nil
		}
		return e.recordExistingHandleReference(parent, v.id, kind)
	case map[string]interface{}:
		if id, ok := bridgeMarkerHandleID(v); ok {
			return e.recordExistingHandleReference(parent, id, kind)
		}
		for _, item := range v {
			if err := e.recordValueReferences(parent, item, kind); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, item := range v {
			if err := e.recordValueReferences(parent, item, kind); err != nil {
				return err
			}
		}
	default:
		return e.recordValueReferencesReflect(parent, reflect.ValueOf(value), kind, make(map[referenceVisit]struct{}), 0)
	}
	return nil
}

func (e *Executor) collectExistingValueReferenceIDs(value interface{}, out map[handles.ID]struct{}) {
	switch v := value.(type) {
	case RuntimeRef:
		if id, ok := e.bridgeHandleForValue(v.Runtime, v); ok {
			out[id] = struct{}{}
		}
	case *RuntimeRef:
		if v != nil {
			e.collectExistingValueReferenceIDs(*v, out)
		}
	case *ResourceRef:
		if v != nil && v.ID != 0 {
			out[v.ID] = struct{}{}
		}
	case ResourceRef:
		if v.ID != 0 {
			out[v.ID] = struct{}{}
		}
	case *TableRef:
		if v != nil && v.ID != 0 {
			out[v.ID] = struct{}{}
		}
	case TableRef:
		if v.ID != 0 {
			out[v.ID] = struct{}{}
		}
	case *JobHandle:
		if v != nil && v.ID != 0 {
			out[handles.ID(v.ID)] = struct{}{}
		}
	case JobHandle:
		if v.ID != 0 {
			out[handles.ID(v.ID)] = struct{}{}
		}
	case *GoHandleProxy:
		if v != nil && v.id != 0 {
			out[v.id] = struct{}{}
		}
	case map[string]interface{}:
		if id, ok := bridgeMarkerHandleID(v); ok {
			out[id] = struct{}{}
			return
		}
		for _, item := range v {
			e.collectExistingValueReferenceIDs(item, out)
		}
	case []interface{}:
		for _, item := range v {
			e.collectExistingValueReferenceIDs(item, out)
		}
	default:
		e.collectExistingValueReferenceIDsReflect(reflect.ValueOf(value), out, make(map[referenceVisit]struct{}), 0)
	}
}

func (e *Executor) collectContainedReferenceIDs(value interface{}, out map[handles.ID]struct{}) {
	switch v := value.(type) {
	case *ResourceRef:
		if v != nil {
			e.collectContainedReferenceIDs(v.Value, out)
		}
	case ResourceRef:
		e.collectContainedReferenceIDs(v.Value, out)
	case *TableRef:
		if v != nil {
			e.collectContainedReferenceIDs(v.Value, out)
		}
	case TableRef:
		e.collectContainedReferenceIDs(v.Value, out)
	case *JobHandle:
		if v != nil {
			e.collectContainedReferenceIDs(v.Payload, out)
			e.collectContainedReferenceIDs(v.Result, out)
		}
	case JobHandle:
		e.collectContainedReferenceIDs(v.Payload, out)
		e.collectContainedReferenceIDs(v.Result, out)
	default:
		e.collectExistingValueReferenceIDs(value, out)
	}
}

const referenceTraversalMaxDepth = 32

type referenceVisit struct {
	typ string
	ptr uintptr
}

func (e *Executor) recordValueReferencesReflect(parent handles.ID, rv reflect.Value, kind string, seen map[referenceVisit]struct{}, depth int) error {
	if depth > referenceTraversalMaxDepth {
		return nil
	}
	rv = dereferenceReferenceValue(rv)
	if !rv.IsValid() {
		return nil
	}
	if key, ok := referenceVisitKey(rv); ok {
		if _, visited := seen[key]; visited {
			return nil
		}
		seen[key] = struct{}{}
	}
	if rv.CanInterface() {
		switch v := rv.Interface().(type) {
		case RuntimeRef, *RuntimeRef, *ResourceRef, ResourceRef, *TableRef, TableRef, *JobHandle, JobHandle, *GoHandleProxy:
			return e.recordValueReferences(parent, v, kind)
		case map[string]interface{}:
			return e.recordValueReferences(parent, v, kind)
		case []interface{}:
			return e.recordValueReferences(parent, v, kind)
		}
	}
	switch rv.Kind() {
	case reflect.Pointer:
		return e.recordValueReferencesReflect(parent, rv.Elem(), kind, seen, depth+1)
	case reflect.Map:
		iter := rv.MapRange()
		for iter.Next() {
			if err := e.recordValueReferencesReflect(parent, iter.Key(), kind, seen, depth+1); err != nil {
				return err
			}
			if err := e.recordValueReferencesReflect(parent, iter.Value(), kind, seen, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			if err := e.recordValueReferencesReflect(parent, rv.Index(i), kind, seen, depth+1); err != nil {
				return err
			}
		}
	case reflect.Struct:
		rt := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			fieldInfo := rt.Field(i)
			if fieldInfo.PkgPath != "" && !fieldInfo.Anonymous {
				continue
			}
			field := rv.Field(i)
			if !field.CanInterface() {
				continue
			}
			if err := e.recordValueReferencesReflect(parent, field, kind, seen, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Executor) collectExistingValueReferenceIDsReflect(rv reflect.Value, out map[handles.ID]struct{}, seen map[referenceVisit]struct{}, depth int) {
	if depth > referenceTraversalMaxDepth {
		return
	}
	rv = dereferenceReferenceValue(rv)
	if !rv.IsValid() {
		return
	}
	if key, ok := referenceVisitKey(rv); ok {
		if _, visited := seen[key]; visited {
			return
		}
		seen[key] = struct{}{}
	}
	if rv.CanInterface() {
		switch v := rv.Interface().(type) {
		case RuntimeRef, *RuntimeRef, *ResourceRef, ResourceRef, *TableRef, TableRef, *JobHandle, JobHandle, *GoHandleProxy:
			e.collectExistingValueReferenceIDs(v, out)
			return
		case map[string]interface{}:
			e.collectExistingValueReferenceIDs(v, out)
			return
		case []interface{}:
			e.collectExistingValueReferenceIDs(v, out)
			return
		}
	}
	switch rv.Kind() {
	case reflect.Pointer:
		e.collectExistingValueReferenceIDsReflect(rv.Elem(), out, seen, depth+1)
	case reflect.Map:
		iter := rv.MapRange()
		for iter.Next() {
			e.collectExistingValueReferenceIDsReflect(iter.Key(), out, seen, depth+1)
			e.collectExistingValueReferenceIDsReflect(iter.Value(), out, seen, depth+1)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			e.collectExistingValueReferenceIDsReflect(rv.Index(i), out, seen, depth+1)
		}
	case reflect.Struct:
		rt := rv.Type()
		for i := 0; i < rv.NumField(); i++ {
			fieldInfo := rt.Field(i)
			if fieldInfo.PkgPath != "" && !fieldInfo.Anonymous {
				continue
			}
			field := rv.Field(i)
			if !field.CanInterface() {
				continue
			}
			e.collectExistingValueReferenceIDsReflect(field, out, seen, depth+1)
		}
	}
}

func dereferenceReferenceValue(rv reflect.Value) reflect.Value {
	for rv.IsValid() && rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return reflect.Value{}
		}
		rv = rv.Elem()
	}
	return rv
}

func referenceVisitKey(rv reflect.Value) (referenceVisit, bool) {
	switch rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice:
		if rv.IsNil() {
			return referenceVisit{}, false
		}
		return referenceVisit{typ: rv.Type().String(), ptr: rv.Pointer()}, true
	default:
		return referenceVisit{}, false
	}
}

func (e *Executor) recordRuntimeRefReference(parent handles.ID, ref RuntimeRef, kind string) error {
	if ref.Runtime == "" || ref.VarName == "" {
		return nil
	}
	jsonVal, err := e.runtimeRefProxyCaptureJSON(ref)
	if err != nil {
		return err
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		return err
	}
	id, ok := bridgeMarkerHandleID(descriptor)
	if !ok {
		return nil
	}
	return e.recordExistingHandleReference(parent, id, kind)
}

func (e *Executor) recordExistingHandleReference(parent, child handles.ID, kind string) error {
	if child == 0 {
		return nil
	}
	if _, ok := e.ensureHandleTable().Get(child); !ok {
		return nil
	}
	_, err := e.ensureHandleTable().RecordReference(parent, child, kind)
	return err
}

func (e *Executor) handleLen(id handles.ID) (int, bool, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return 0, false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "length"}); err != nil {
		return 0, false, err
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		return e.runtimeRefLen(ref)
	}
	return genericLen(entry.Value)
}

func (e *Executor) handleIter(id handles.ID, mode string) ([]interface{}, bool, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return nil, false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "iterate"}); err != nil {
		return nil, false, err
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		return e.runtimeRefIter(ref, mode)
	}
	return genericIter(entry.Value, mode)
}

func (e *Executor) handleContains(id handles.ID, key interface{}) (bool, bool, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return false, false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "contains"}); err != nil {
		return false, false, err
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		return e.runtimeRefContains(ref, key)
	}
	return genericContains(entry.Value, key)
}

func (e *Executor) handleStreamNext(id handles.ID) (interface{}, bool, bool, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return nil, false, false, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "stream"}); err != nil {
		return nil, false, false, err
	}
	switch v := entry.Value.(type) {
	case *ChanRef:
		value, done := v.recvStreamValue()
		if done {
			if err := e.ensureHandleTable().ReleaseAllRefs(id); err != nil {
				return nil, false, true, err
			}
		}
		return value, done, true, nil
	case RuntimeRef:
		value, done, err := e.runtimeRefStreamNext(id, v)
		return value, done, true, err
	case *RuntimeRef:
		if v == nil {
			return nil, false, false, nil
		}
		value, done, err := e.runtimeRefStreamNext(id, *v)
		return value, done, true, err
	default:
		value, done, ok := recvReflectStreamValue(v)
		if ok {
			if done {
				if err := e.ensureHandleTable().ReleaseAllRefs(id); err != nil {
					return nil, false, true, err
				}
			}
			return value, done, true, nil
		}
		value, done, ok, err := readGenericStreamValue(v)
		if err != nil {
			return nil, false, true, err
		}
		if !ok {
			return nil, false, false, nil
		}
		if done {
			if err := e.ensureHandleTable().ReleaseAllRefs(id); err != nil {
				return nil, false, true, err
			}
		}
		return value, done, true, nil
	}
}

func (e *Executor) runtimeRefIsStream(ref RuntimeRef) (bool, error) {
	expr, ok := runtimeRefStreamProbeExpr(ref)
	if !ok {
		return false, nil
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return false, err
	}
	stream, _ := value.(bool)
	return stream, nil
}

func (e *Executor) runtimeRefStreamNext(id handles.ID, ref RuntimeRef) (interface{}, bool, error) {
	valueVar := e.nextRuntimeRefVar(id, "stream_next")
	doneVar := valueVar + "_done"
	readyVar := valueVar + "_ready"
	errVar := valueVar + "_error"
	stateVar := runtimeRefStableVar(id, "stream_state", "each")
	useAsyncStep := ref.Runtime == "javascript"
	if ref.Runtime == "python" {
		asyncStream, err := e.runtimeRefIsPythonAsyncStream(ref)
		if err != nil {
			return nil, false, err
		}
		useAsyncStep = asyncStream
	}
	if useAsyncStep {
		code, ok := runtimeRefJSStreamNextStepCode(ref, valueVar, doneVar, readyVar, errVar, stateVar)
		if ref.Runtime == "python" {
			code, ok = runtimeRefPythonStreamNextStepCode(ref, valueVar, doneVar, readyVar, errVar, stateVar)
		}
		if !ok {
			return nil, false, fmt.Errorf("manifest HandleCall: runtime %q does not support stream_next", ref.Runtime)
		}
		rt, ok := e.runtimes[ref.Runtime]
		if !ok {
			return nil, false, fmt.Errorf("source runtime %q not found", ref.Runtime)
		}
		result := rt.Execute(code)
		if result.Err != nil {
			return nil, false, result.Err
		}
		readyValue := "true"
		if ref.Runtime == "python" {
			readyValue = "True"
		}
		if err := e.pumpUntilDone(func() bool {
			check := rt.Eval(runtimeVarRef(ref.Runtime, readyVar))
			return check.Value != nil && fmt.Sprintf("%v", check.Value) == readyValue
		}); err != nil {
			return nil, false, err
		}
		if ref.Runtime == "javascript" {
			if err := e.asyncJSError(rt, runtimeVarRef(ref.Runtime, errVar)); err != nil {
				return nil, false, err
			}
		} else if ref.Runtime == "python" {
			if err := e.asyncPythonError(rt, runtimeVarRef(ref.Runtime, errVar)); err != nil {
				return nil, false, err
			}
		}
		doneValue, err := e.runtimeRefEvalPrimitive(RuntimeRef{Runtime: ref.Runtime, VarName: doneVar}, runtimeVarRef(ref.Runtime, doneVar))
		if err != nil {
			return nil, false, err
		}
		done, _ := doneValue.(bool)
		if done {
			if err := e.ensureHandleTable().ReleaseAllRefs(id); err != nil {
				return nil, true, err
			}
			return nil, true, nil
		}
		value, ok, err := e.runtimeRefEvalExpr(ref, valueVar, runtimeVarRef(ref.Runtime, valueVar))
		return value, false, errOrNotOK(ok, err)
	}
	code, ok := runtimeRefStreamNextCode(ref, valueVar, doneVar, stateVar)
	if !ok {
		return nil, false, fmt.Errorf("manifest HandleCall: runtime %q does not support stream_next", ref.Runtime)
	}
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return nil, false, fmt.Errorf("source runtime %q not found", ref.Runtime)
	}
	result := rt.Execute(code)
	if result.Err != nil {
		return nil, false, result.Err
	}
	doneValue, err := e.runtimeRefEvalPrimitive(RuntimeRef{Runtime: ref.Runtime, VarName: doneVar}, runtimeVarRef(ref.Runtime, doneVar))
	if err != nil {
		return nil, false, err
	}
	done, _ := doneValue.(bool)
	if done {
		if err := e.ensureHandleTable().ReleaseAllRefs(id); err != nil {
			return nil, true, err
		}
		return nil, true, nil
	}
	value, ok, err := e.runtimeRefEvalExpr(ref, valueVar, runtimeVarRef(ref.Runtime, valueVar))
	return value, false, errOrNotOK(ok, err)
}

func (e *Executor) runtimeRefIsPythonAsyncStream(ref RuntimeRef) (bool, error) {
	if ref.Runtime != "python" {
		return false, nil
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	expr := fmt.Sprintf("(lambda __v: (lambda __omnivm_http_message: hasattr(__v, '__aiter__') and not hasattr(__v, '__len__') and not __omnivm_http_message and not isinstance(__v, (__import__('collections.abc', fromlist=['Mapping']).Mapping, __import__('collections.abc', fromlist=['Sequence']).Sequence, __import__('collections.abc', fromlist=['Set']).Set, memoryview)))(%s))(%s)", pythonHTTPMessageProbeExpr("__v"), base)
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return false, err
	}
	asyncStream, _ := value.(bool)
	return asyncStream, nil
}

func (e *Executor) closeRuntimeRefStream(id handles.ID, ref RuntimeRef) error {
	stateVar := runtimeRefStableVar(id, "stream_state", "each")
	code, ok := runtimeRefStreamCloseCode(ref, stateVar)
	if !ok {
		return nil
	}
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return nil
	}
	result := rt.Execute(code)
	if result.Err != nil {
		return result.Err
	}
	return nil
}

func errOrNotOK(ok bool, err error) error {
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("runtime stream_next did not return a value")
	}
	return nil
}

func (e *Executor) handleMethodCall(id handles.ID, key string, args []interface{}, kwargs map[string]interface{}) (interface{}, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if !ok {
		return nil, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
	}
	if _, err := e.ensureHandleTable().RecordAccess(id, handles.AccessOptions{Kind: "call"}); err != nil {
		return nil, err
	}
	if ref, ok := runtimeRefFromHandleValue(entry.Value); ok {
		value, ok, err := e.runtimeRefCall(id, ref, key, args, kwargs)
		if err != nil {
			return nil, err
		}
		if ok {
			return value, nil
		}
	}
	if len(kwargs) > 0 {
		return nil, fmt.Errorf("manifest HandleCall: handle %d method %q does not support keyword arguments", id, key)
	}
	value, ok, err := genericCall(entry.Value, key, args)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("manifest HandleCall: handle %d has no callable property %q", id, key)
	}
	return value, nil
}

func (e *Executor) handleMethodCallPositional(id handles.ID, key string, args []interface{}) (interface{}, error) {
	return e.handleMethodCall(id, key, args, nil)
}

func runtimeRefFromHandleValue(value interface{}) (RuntimeRef, bool) {
	switch v := value.(type) {
	case RuntimeRef:
		if v.Runtime != "" && v.VarName != "" {
			return v, true
		}
	case *RuntimeRef:
		if v != nil && v.Runtime != "" && v.VarName != "" {
			return *v, true
		}
	case *ResourceRef:
		if v != nil {
			return runtimeRefFromHandleValue(v.Value)
		}
	case ResourceRef:
		return runtimeRefFromHandleValue(v.Value)
	case *TableRef:
		if v != nil {
			return runtimeRefFromHandleValue(v.Value)
		}
	case TableRef:
		return runtimeRefFromHandleValue(v.Value)
	case *JobHandle:
		if v != nil {
			return runtimeRefFromHandleValue(v.Payload)
		}
	case JobHandle:
		return runtimeRefFromHandleValue(v.Payload)
	}
	return RuntimeRef{}, false
}

func (e *Executor) runtimeRefProperty(parent handles.ID, ref RuntimeRef, key string) (interface{}, bool, error) {
	if ref.Runtime == "ruby" {
		zeroArg, err := e.runtimeRefRubyZeroArgMethod(ref, key)
		if err != nil {
			return nil, false, err
		}
		if zeroArg {
			expr, ok, err := runtimeRefPropertyExpr(ref, key)
			if err != nil || !ok {
				return nil, ok, err
			}
			return e.runtimeRefEvalExpr(ref, runtimeRefStableVar(parent, "prop", key), expr)
		}
	}
	callable, err := e.runtimeRefCallable(ref, key)
	if err != nil {
		return nil, false, err
	}
	if callable {
		return nil, false, nil
	}
	if runtimeRefPropertyShouldConfirmPresence(key) {
		containsOK, found, err := e.runtimeRefContains(ref, key)
		if err != nil {
			return nil, false, err
		}
		if containsOK && !found {
			return nil, false, nil
		}
	}
	expr, ok, err := runtimeRefPropertyExpr(ref, key)
	if err != nil || !ok {
		return nil, ok, err
	}
	return e.runtimeRefEvalExpr(ref, runtimeRefStableVar(parent, "prop", key), expr)
}

func runtimeRefPropertyShouldConfirmPresence(key string) bool {
	switch key {
	case "get", "keys", "items", "values", "copy", "update", "to_json":
		return true
	default:
		return false
	}
}

func (e *Executor) runtimeRefRubyZeroArgMethod(ref RuntimeRef, key string) (bool, error) {
	if ref.Runtime != "ruby" {
		return false, nil
	}
	expr, ok, err := runtimeRefRubyZeroArgMethodExpr(ref, key)
	if err != nil || !ok {
		return false, err
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return false, err
	}
	zeroArg, _ := value.(bool)
	return zeroArg, nil
}

func (e *Executor) runtimeRefJavaZeroArgMethod(ref RuntimeRef, key string) (bool, error) {
	if ref.Runtime != "java" {
		return false, nil
	}
	if javaZeroArgCommandMethod(key) {
		return false, nil
	}
	expr, ok, err := runtimeRefJavaZeroArgMethodExpr(ref, key)
	if err != nil || !ok {
		return false, err
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return false, err
	}
	zeroArg, _ := value.(bool)
	return zeroArg, nil
}

func javaZeroArgCommandMethod(key string) bool {
	return rubyZeroArgCommandMethod(key)
}

func (e *Executor) runtimeRefTargetCallable(ref RuntimeRef) (bool, error) {
	expr, ok := runtimeRefTargetCallableExpr(ref)
	if !ok {
		return false, nil
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return false, err
	}
	callable, _ := value.(bool)
	return callable, nil
}

func (e *Executor) runtimeRefIndex(parent handles.ID, ref RuntimeRef, key interface{}) (interface{}, bool, error) {
	builder := &runtimeExprBuilder{executor: e, targetRuntime: ref.Runtime}
	expr, ok, err := runtimeRefIndexExprWithBuilder(ref, key, builder)
	if err != nil || !ok {
		return nil, ok, err
	}
	if err := e.executeRuntimeExprPrelude(ref.Runtime, builder); err != nil {
		return nil, false, err
	}
	return e.runtimeRefEvalExpr(ref, runtimeRefStableVar(parent, "index", key), expr)
}

func (e *Executor) runtimeRefSet(ref RuntimeRef, key string, value interface{}) (bool, error) {
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return false, nil
	}
	code, ok, err := e.runtimeRefSetCode(ref, key, value)
	if err != nil || !ok {
		return ok, err
	}
	result := rt.Execute(code)
	if result.Err != nil {
		return false, result.Err
	}
	return true, nil
}

func (e *Executor) runtimeRefLen(ref RuntimeRef) (int, bool, error) {
	expr, ok := runtimeRefLenExpr(ref)
	if !ok {
		return 0, false, nil
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return 0, false, err
	}
	idx, ok := numericIndex(value)
	return idx, ok, nil
}

func (e *Executor) runtimeRefIter(ref RuntimeRef, mode string) ([]interface{}, bool, error) {
	expr, ok, err := runtimeRefIterExpr(ref, mode)
	if err != nil || !ok {
		return nil, ok, err
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return nil, false, err
	}
	items, ok := value.([]interface{})
	return items, ok, nil
}

func (e *Executor) runtimeRefContains(ref RuntimeRef, key interface{}) (bool, bool, error) {
	builder := &runtimeExprBuilder{executor: e, targetRuntime: ref.Runtime}
	expr, ok, err := runtimeRefContainsExprWithBuilder(ref, key, builder)
	if err != nil || !ok {
		return ok, false, err
	}
	if err := e.executeRuntimeExprPrelude(ref.Runtime, builder); err != nil {
		return true, false, err
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return true, false, err
	}
	found, ok := value.(bool)
	return ok, found, nil
}

func (e *Executor) runtimeRefCallable(ref RuntimeRef, key string) (bool, error) {
	expr, ok, err := runtimeRefCallableExpr(ref, key)
	if err != nil || !ok {
		return false, err
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return false, err
	}
	callable, _ := value.(bool)
	return callable, nil
}

func (e *Executor) runtimeRefCall(parent handles.ID, ref RuntimeRef, key string, args []interface{}, kwargs map[string]interface{}) (interface{}, bool, error) {
	builder := &runtimeExprBuilder{executor: e, targetRuntime: ref.Runtime}
	expr, ok, err := runtimeRefCallExprWithBuilder(ref, key, args, kwargs, builder)
	if err != nil || !ok {
		return nil, ok, err
	}
	if err := e.executeRuntimeExprPrelude(ref.Runtime, builder); err != nil {
		return nil, false, err
	}
	valueVar := e.nextRuntimeRefVar(parent, "call")
	value, ok, err := e.runtimeRefEvalExpr(ref, valueVar, expr)
	if err != nil || !ok || ref.Runtime != "python" {
		return value, ok, err
	}
	awaitable, err := e.runtimeRefPythonValueIsAwaitable(valueVar)
	if err != nil {
		return nil, false, err
	}
	if !awaitable {
		return value, ok, nil
	}
	awaited, err := e.runtimeRefAwaitPythonValue(valueVar)
	if err != nil {
		return nil, false, err
	}
	if !awaited {
		return value, ok, nil
	}
	return e.runtimeRefEvalExpr(ref, valueVar, runtimeVarRef(ref.Runtime, valueVar))
}

func (e *Executor) runtimeRefPythonValueIsAwaitable(varName string) (bool, error) {
	value, err := e.runtimeRefEvalPrimitive(
		RuntimeRef{Runtime: "python", VarName: varName},
		fmt.Sprintf("(__import__('inspect').isawaitable(%s))", varName),
	)
	if err != nil {
		return false, nil
	}
	awaitable, _ := value.(bool)
	return awaitable, nil
}

func (e *Executor) runtimeRefAwaitPythonValue(varName string) (bool, error) {
	rt, ok := e.runtimes["python"]
	if !ok {
		return false, nil
	}
	readyVar := varName + "_await_ready"
	errVar := varName + "_await_error"
	awaitedVar := varName + "_awaited"
	code := fmt.Sprintf(`
import inspect as __omnivm_await_inspect
import asyncio as __omnivm_await_asyncio
%s = False
%s = None
%s = False
if __omnivm_await_inspect.isawaitable(%s):
    %s = True
    async def __omnivm_await_runtime_ref_call():
        global %s, %s, %s
        try:
            globals()[%q] = await %s
        except BaseException as e:
            %s = type(e).__name__ + ": " + str(e)
        finally:
            %s = True
    __omnivm_await_loop = globals().get('__omnivm_stream_loop')
    if __omnivm_await_loop is None or __omnivm_await_loop.is_closed():
        try:
            __omnivm_await_loop = __omnivm_await_asyncio.get_event_loop()
        except RuntimeError:
            __omnivm_await_loop = __omnivm_await_asyncio.new_event_loop()
            __omnivm_await_asyncio.set_event_loop(__omnivm_await_loop)
        globals()['__omnivm_stream_loop'] = __omnivm_await_loop
    __omnivm_await_asyncio.ensure_future(__omnivm_await_runtime_ref_call(), loop=__omnivm_await_loop)
else:
    %s = True
`, readyVar, errVar, awaitedVar, varName, awaitedVar, varName, errVar, readyVar, varName, varName, errVar, readyVar, readyVar)
	result := rt.Execute(code)
	if result.Err != nil {
		return false, fmt.Errorf("runtime ref awaitable setup [python]: %w", result.Err)
	}
	readyValue := "True"
	if err := e.pumpUntilDone(func() bool {
		check := rt.Eval(runtimeVarRef("python", readyVar))
		return check.Value != nil && fmt.Sprintf("%v", check.Value) == readyValue
	}); err != nil {
		return false, err
	}
	if err := e.asyncPythonError(rt, runtimeVarRef("python", errVar)); err != nil {
		return false, err
	}
	value, err := e.runtimeRefEvalPrimitive(RuntimeRef{Runtime: "python", VarName: awaitedVar}, runtimeVarRef("python", awaitedVar))
	if err != nil {
		return false, err
	}
	awaited, _ := value.(bool)
	return awaited, nil
}

type runtimeExprBuilder struct {
	executor              *Executor
	targetRuntime         string
	needsMaterializer     bool
	needsPythonTableBytes bool
}

func (b *runtimeExprBuilder) expr(value interface{}) (string, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return b.runtimeRefExpr(v)
	case *RuntimeRef:
		if v == nil {
			return runtimeValueLiteral(b.targetRuntime, nil)
		}
		return b.runtimeRefExpr(*v)
	case []interface{}:
		items := make([]string, 0, len(v))
		for _, item := range v {
			expr, err := b.expr(item)
			if err != nil {
				return "", err
			}
			items = append(items, expr)
		}
		return runtimeListExpr(b.targetRuntime, items)
	case map[string]interface{}:
		if isBridgeMarker(v) {
			return b.materializedDescriptorExpr(v)
		}
		items := make([]runtimeMapExprItem, 0, len(v))
		for key, item := range v {
			keyExpr, err := runtimeValueLiteral(b.targetRuntime, key)
			if err != nil {
				return "", err
			}
			valueExpr, err := b.expr(item)
			if err != nil {
				return "", err
			}
			items = append(items, runtimeMapExprItem{key: keyExpr, value: valueExpr})
		}
		return runtimeMapExpr(b.targetRuntime, items)
	default:
		return runtimeValueLiteral(b.targetRuntime, value)
	}
}

func (b *runtimeExprBuilder) runtimeRefExpr(ref RuntimeRef) (string, error) {
	if ref.Runtime == b.targetRuntime && ref.VarName != "" {
		return runtimeVarRef(b.targetRuntime, ref.VarName), nil
	}
	if ref.Runtime == "" || ref.VarName == "" {
		return runtimeValueLiteral(b.targetRuntime, ref.Value)
	}
	if b.targetRuntime == "python" && ref.Runtime == "javascript" {
		if jsonVal, ok, err := b.executor.runtimeRefBulkTableCaptureJSON("", b.targetRuntime, ref); ok || err != nil {
			if err != nil {
				return "", err
			}
			if runtimeRefTableJSONByteish(jsonVal) {
				b.needsPythonTableBytes = true
				return pythonTableBytesJSONExpr(jsonVal), nil
			}
			return b.materializedJSONExpr(jsonVal), nil
		}
	}
	jsonVal, err := b.executor.runtimeRefProxyCaptureJSON(ref)
	if err != nil {
		return "", err
	}
	return b.materializedJSONExpr(jsonVal), nil
}

func runtimeRefTableJSONByteish(jsonVal string) bool {
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		return false
	}
	if descriptor["__omnivm_table__"] != true {
		return false
	}
	metadata, _ := descriptor["metadata"].(map[string]interface{})
	rawDtype := descriptor["dtype"]
	if metadata != nil {
		if value, ok := metadata["dtype"]; ok {
			rawDtype = value
		}
	}
	dtype, ok := numericInt32(rawDtype)
	if !ok {
		return false
	}
	switch dtype {
	case arrow.DtypeBytes, arrow.DtypeI8, arrow.DtypeU8:
		return true
	default:
		return false
	}
}

func numericInt32(value interface{}) (int32, bool) {
	switch v := value.(type) {
	case int:
		return int32(v), true
	case int32:
		return v, true
	case int64:
		return int32(v), true
	case float64:
		if math.Trunc(v) != v {
			return 0, false
		}
		return int32(v), true
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, false
		}
		return int32(n), true
	default:
		return 0, false
	}
}

func (b *runtimeExprBuilder) materializedDescriptorExpr(value map[string]interface{}) (string, error) {
	jsonVal, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return b.materializedJSONExpr(string(jsonVal)), nil
}

func (b *runtimeExprBuilder) materializedJSONExpr(jsonVal string) string {
	b.needsMaterializer = true
	switch b.targetRuntime {
	case "javascript":
		return fmt.Sprintf("globalThis.__omnivm_materialize_capture(%s)", jsonVal)
	case "python":
		return fmt.Sprintf("__omnivm_materialize_capture(__import__('json').loads(%s))", jsonStringLiteral(jsonVal))
	case "ruby":
		return fmt.Sprintf("__omnivm_materialize_capture(JSON.parse(%s))", jsonStringLiteral(jsonVal))
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.materializeJsonCapture(%s)", jsonStringLiteral(jsonVal))
	default:
		return jsonVal
	}
}

func pythonTableBytesJSONExpr(jsonVal string) string {
	return fmt.Sprintf("__omnivm_table_bytes(__import__('json').loads(%s))", jsonStringLiteral(jsonVal))
}

func pythonTableBytesMaterializer() string {
	return `def __omnivm_table_bytes(value):
    if not isinstance(value, dict):
        return bytes(value)
    metadata = value.get("metadata") if isinstance(value.get("metadata"), dict) else {}
    buffer_name = value.get("buffer") or metadata.get("buffer") or value.get("value")
    if not buffer_name:
        return b""
    raw = __import__("omnivm").get_buffer(buffer_name)
    if raw is None:
        return b""
    data = bytes(raw)
    shape = metadata.get("shape") if isinstance(metadata.get("shape"), list) else []
    strides = metadata.get("strides") if isinstance(metadata.get("strides"), list) else []
    try:
        offset = int(metadata.get("offset") or 0)
    except Exception:
        offset = 0
    try:
        length = int(shape[0]) if shape else max(0, len(data) - offset)
    except Exception:
        length = max(0, len(data) - offset)
    try:
        stride = int(strides[0]) if strides else 1
    except Exception:
        stride = 1
    if offset < 0:
        offset = 0
    if length <= 0:
        return b""
    if stride == 0:
        stride = 1
    if stride == 1:
        return data[offset:offset + length]
    return bytes(data[offset + i * stride] for i in range(length) if 0 <= offset + i * stride < len(data))`
}

func (b *runtimeExprBuilder) prelude() string {
	if !b.needsMaterializer && !b.needsPythonTableBytes {
		return ""
	}
	switch b.targetRuntime {
	case "javascript":
		return jsChannelMaterializer()
	case "python":
		var parts []string
		if b.needsMaterializer {
			parts = append(parts, pythonCaptureMaterializer())
		}
		if b.needsPythonTableBytes {
			parts = append(parts, pythonTableBytesMaterializer())
		}
		return strings.Join(parts, "\n")
	case "ruby":
		return "require 'json'\n" + rubyCaptureMaterializer()
	default:
		return ""
	}
}

type runtimeMapExprItem struct {
	key   string
	value string
}

func runtimeListExpr(rtName string, items []string) (string, error) {
	switch rtName {
	case "javascript", "python", "ruby":
		return "[" + strings.Join(items, ", ") + "]", nil
	case "java":
		return "omnivm.OmniVM.listOf(new Object[]{" + strings.Join(items, ", ") + "})", nil
	default:
		return runtimeValueLiteral(rtName, []interface{}{})
	}
}

func runtimeMapExpr(rtName string, items []runtimeMapExprItem) (string, error) {
	parts := make([]string, 0, len(items))
	switch rtName {
	case "javascript":
		for _, item := range items {
			parts = append(parts, item.key+": "+item.value)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case "python":
		for _, item := range items {
			parts = append(parts, item.key+": "+item.value)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case "ruby":
		for _, item := range items {
			parts = append(parts, item.key+" => "+item.value)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	case "java":
		for _, item := range items {
			parts = append(parts, item.key, item.value)
		}
		return "omnivm.OmniVM.mapOf(new Object[]{" + strings.Join(parts, ", ") + "})", nil
	default:
		return runtimeValueLiteral(rtName, map[string]interface{}{})
	}
}

func prependRuntimeExprPrelude(builder *runtimeExprBuilder, expr string) string {
	prelude := builder.prelude()
	if prelude == "" {
		return expr
	}
	return prelude + "\n" + expr
}

func (e *Executor) executeRuntimeExprPrelude(rtName string, builder *runtimeExprBuilder) error {
	prelude := builder.prelude()
	if prelude == "" {
		return nil
	}
	rt, ok := e.runtimes[rtName]
	if !ok {
		return nil
	}
	result := rt.Execute(prelude)
	return result.Err
}

func (e *Executor) runtimeRefEvalExpr(ref RuntimeRef, varName, expr string) (interface{}, bool, error) {
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return nil, false, nil
	}
	nextRef := RuntimeRef{Runtime: ref.Runtime, VarName: varName}
	if _, exists := e.bridgeHandleForValue(ref.Runtime, nextRef); exists {
		return nextRef, true, nil
	}

	result := rt.Execute(runtimeAssign(ref.Runtime, varName, expr))
	if result.Err != nil {
		return nil, false, fmt.Errorf("runtime ref assign [%s]: %w (expr: %s)", ref.Runtime, result.Err, expr)
	}
	snapshotResult := rt.Eval(runtimePrimitiveSnapshotExpr(ref.Runtime, runtimeVarRef(ref.Runtime, varName)))
	if snapshot, ok, err := decodeRuntimePrimitiveSnapshot(ref.Runtime, snapshotResult); err == nil && ok {
		nextRef.SnapshotKnown = true
		if snapshot.Primitive {
			nextRef.Value = snapshot.Value
			return snapshot.Value, true, nil
		}
		nextRef.Opaque = true
		nextRef.CallableKnown = true
		nextRef.Callable = snapshot.Callable
		nextRef.CallableShape = snapshot.CallableShape
		return nextRef, true, nil
	}
	value, err := e.runtimeRefEvalPrimitive(RuntimeRef{Runtime: ref.Runtime, VarName: varName}, runtimeVarRef(ref.Runtime, varName))
	if err != nil {
		nextRef.Value = nil
		return nextRef, true, nil
	}
	if isBridgePrimitive(value) {
		return value, true, nil
	}
	nextRef.Value = value
	return nextRef, true, nil
}

func (e *Executor) runtimeRefEvalPrimitive(ref RuntimeRef, expr string) (interface{}, error) {
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return nil, fmt.Errorf("source runtime %q not found", ref.Runtime)
	}
	result := rt.Eval(runtimeSerializeExpr(ref.Runtime, expr))
	value, err := decodeRuntimeValue(ref.Runtime, result)
	if err != nil {
		return nil, fmt.Errorf("runtime ref eval [%s]: %w (expr: %s)", ref.Runtime, err, expr)
	}
	return value, nil
}

func runtimeRefStableVar(parent handles.ID, op string, key interface{}) string {
	payload, _ := json.Marshal([]interface{}{op, key})
	return fmt.Sprintf("__omnivm_ref_%d_%08x", parent, crc32.ChecksumIEEE(payload))
}

func (e *Executor) nextRuntimeRefVar(parent handles.ID, op string) string {
	e.nextRuntimeRefID++
	return fmt.Sprintf("__omnivm_ref_%d_%s_%d", parent, op, e.nextRuntimeRefID)
}

func runtimeRefPropertyExpr(ref RuntimeRef, key string) (string, bool, error) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("(%s)[%s]", base, keyLit), true, nil
	case "python":
		if pythonHTTPMessageAttributeKey(key) {
			return fmt.Sprintf("(lambda __o, __k: (getattr(__o, __k) if isinstance(__k, str) and %s and hasattr(__o, __k) else (__o[__k] if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and __k in __o else (getattr(__o, __k) if isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {}) else (getattr(__o, __k) if isinstance(__k, str) and hasattr(__o, __k) else (None if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) else __o[__k]))))))(%s, %s)", pythonHTTPMessageProbeExpr("__o"), base, keyLit), true, nil
		}
		return fmt.Sprintf("(lambda __o, __k: (__o[__k] if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and __k in __o else (getattr(__o, __k) if isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {}) else (getattr(__o, __k) if isinstance(__k, str) and hasattr(__o, __k) else (None if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) else __o[__k])))))(%s, %s)", base, keyLit), true, nil
	case "ruby":
		return fmt.Sprintf("(begin; __o = %s; __k = %s; (__o.respond_to?(:key?) && __o.key?(__k)) || (__o.respond_to?(:has_attribute?) && __o.has_attribute?(__k)) ? __o[__k] : (__o.respond_to?(__k) ? __o.public_send(__k) : __o[__k]); end)", base, keyLit), true, nil
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyGet(%s, %s)", base, keyLit), true, nil
	default:
		return "", false, nil
	}
}

func pythonHTTPMessageAttributeKey(key string) bool {
	switch key {
	case "body", "content", "headers", "META", "method", "path", "streaming_content", "status_code", "url":
		return true
	default:
		return false
	}
}

func runtimeRefIndexExpr(ref RuntimeRef, key interface{}) (string, bool, error) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	switch ref.Runtime {
	case "javascript", "python", "ruby":
		return fmt.Sprintf("(%s)[%s]", base, keyLit), true, nil
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyIndex(%s, %s)", base, keyLit), true, nil
	default:
		return "", false, nil
	}
}

func (e *Executor) runtimeRefIndexExpr(ref RuntimeRef, key interface{}) (string, bool, error) {
	builder := &runtimeExprBuilder{executor: e, targetRuntime: ref.Runtime}
	return runtimeRefIndexExprWithBuilder(ref, key, builder)
}

func runtimeRefIndexExprWithBuilder(ref RuntimeRef, key interface{}, builder *runtimeExprBuilder) (string, bool, error) {
	keyLit, err := builder.expr(key)
	if err != nil {
		return "", false, err
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	var expr string
	switch ref.Runtime {
	case "javascript", "python", "ruby":
		expr = fmt.Sprintf("(%s)[%s]", base, keyLit)
	case "java":
		expr = fmt.Sprintf("omnivm.OmniVM.proxyIndex(%s, %s)", base, keyLit)
	default:
		return "", false, nil
	}
	return expr, true, nil
}

func runtimeRefRubyZeroArgMethodExpr(ref RuntimeRef, key string) (string, bool, error) {
	if ref.Runtime != "ruby" {
		return "", false, nil
	}
	if rubyZeroArgCommandMethod(key) {
		return "false", true, nil
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("(begin; __o = %s; __k = %s; __o.respond_to?(__k) && __o.method(__k).arity == 0; rescue NameError; false; end)", base, keyLit), true, nil
}

func rubyZeroArgCommandMethod(key string) bool {
	switch key {
	case "close", "finish", "flush", "commit!", "rollback!", "cancel", "return", "dispose", "shutdown", "terminate":
		return true
	default:
		return false
	}
}

func runtimeRefJavaZeroArgMethodExpr(ref RuntimeRef, key string) (string, bool, error) {
	if ref.Runtime != "java" {
		return "", false, nil
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf("omnivm.OmniVM.proxyZeroArgCallable(%s, %s)", base, keyLit), true, nil
}

func runtimeRefStreamProbeExpr(ref RuntimeRef) (string, bool) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("(function(__v){ return !!(__v && !__v.__omnivm_proxy__ && !Array.isArray(__v) && !(typeof Map !== 'undefined' && __v instanceof Map) && !(typeof Set !== 'undefined' && __v instanceof Set) && !(typeof ArrayBuffer !== 'undefined' && (__v instanceof ArrayBuffer || ArrayBuffer.isView(__v))) && !(typeof __v === 'string' || __v instanceof String) && !(typeof __v.method !== 'undefined' && (__v.path || __v.url || __v.headers)) && ((typeof __v.next === 'function' && (typeof __v[Symbol.iterator] !== 'function' || __v[Symbol.iterator]() === __v)) || (typeof __v.getReader === 'function') || (typeof __v.next !== 'function' && typeof __v[Symbol.iterator] === 'function') || (typeof Symbol !== 'undefined' && typeof Symbol.asyncIterator !== 'undefined' && typeof __v[Symbol.asyncIterator] === 'function'))); })(%s)", base), true
	case "python":
		return fmt.Sprintf("(lambda __v: (getattr(type(__v), 'model_fields', None) is None and not (%s) and (((hasattr(__v, '__next__') and iter(__v) is __v) or (callable(getattr(__v, 'read', None)) and (isinstance(__v, __import__('io').IOBase) or (callable(getattr(__v, 'readable', None)) and __v.readable())))) or ((hasattr(__v, '__iter__') or hasattr(__v, '__aiter__')) and not hasattr(__v, '__len__') and not isinstance(__v, (__import__('collections.abc', fromlist=['Mapping']).Mapping, __import__('collections.abc', fromlist=['Sequence']).Sequence, __import__('collections.abc', fromlist=['Set']).Set, memoryview))))))(%s)", pythonHTTPMessageProbeExpr("__v"), base), true
	case "ruby":
		return fmt.Sprintf("(begin; __v = %s; __omnivm_http_message = %s; __omnivm_response_writer = __v.respond_to?(:write) && __v.respond_to?(:close) && __v.respond_to?(:closed?) && !__v.respond_to?(:read); !__omnivm_http_message && !__omnivm_response_writer && (__v.respond_to?(:next) || __v.respond_to?(:read) || (__v.respond_to?(:to_io) && __v.to_io.respond_to?(:read)) || (__v.respond_to?(:each) && !__v.is_a?(Array) && !__v.is_a?(Hash) && !__v.is_a?(String))); end)", base, rubyHTTPMessageProbeExpr("__v")), true
	case "java":
		httpMessage := javaHTTPMessageProbeExpr(base)
		return fmt.Sprintf("(!%s && ((%s instanceof java.util.Iterator) || (%s instanceof java.io.InputStream) || (%s instanceof java.nio.channels.ReadableByteChannel) || (%s instanceof java.io.Reader) || (%s instanceof java.util.stream.BaseStream) || %s || %s || ((%s instanceof java.lang.Iterable) && !(%s instanceof java.util.Collection) && !(%s instanceof java.util.Map) && !(%s instanceof java.lang.CharSequence))))", httpMessage, base, base, base, base, base, javaFlowPublisherProbeExpr(base), javaToStreamProbeExpr(base), base, base, base, base), true
	default:
		return "", false
	}
}

func javaToStreamProbeExpr(base string) string {
	return fmt.Sprintf("(%s != null && java.util.Arrays.stream(%s.getClass().getMethods()).anyMatch(__m -> (__m.getName().equals(\"toStream\") || __m.getName().equals(\"blockingStream\")) && __m.getParameterCount() == 0 && java.util.stream.BaseStream.class.isAssignableFrom(__m.getReturnType())))", base, base)
}

func javaFlowPublisherProbeExpr(base string) string {
	return fmt.Sprintf("(%s instanceof java.util.concurrent.Flow.Publisher)", base)
}

func pythonHTTPMessageProbeExpr(base string) string {
	return fmt.Sprintf("((hasattr(%s, 'method') and (hasattr(%s, 'path') or hasattr(%s, 'url') or hasattr(%s, 'headers') or hasattr(%s, 'META'))) or (hasattr(%s, 'status_code') and (hasattr(%s, 'headers') or hasattr(%s, 'content') or hasattr(%s, 'streaming_content'))))", base, base, base, base, base, base, base, base, base)
}

func rubyHTTPMessageProbeExpr(base string) string {
	return fmt.Sprintf("((begin; __omnivm_method_like = %s.respond_to?(:request_method) || (begin; %s.respond_to?(:method) && ![Kernel, Object, BasicObject].include?(%s.method(:method).owner); rescue; false; end); __omnivm_method_like && (%s.respond_to?(:path) || %s.respond_to?(:url) || %s.respond_to?(:headers) || %s.respond_to?(:env) || %s.respond_to?(:path_info)); end) || ((%s.respond_to?(:status) || %s.respond_to?(:status_code) || %s.respond_to?(:code)) && (%s.respond_to?(:headers) || %s.respond_to?(:get_header) || %s.respond_to?(:body) || %s.respond_to?(:each))))", base, base, base, base, base, base, base, base, base, base, base, base, base, base, base)
}

func javaHTTPMessageProbeExpr(base string) string {
	return fmt.Sprintf(`(new java.util.function.Predicate<Object>() {
  public boolean test(Object __v) {
    if (__v == null) return false;
    boolean __methodLike = false;
    boolean __targetLike = false;
    for (java.lang.reflect.Method __m : __v.getClass().getMethods()) {
      if (__m.getParameterCount() == 0) {
        String __n = __m.getName();
        if (__n.equals("getMethod") || __n.equals("method") || __n.equals("requestMethod") || __n.equals("getRequestMethod")) __methodLike = true;
        if (__n.equals("getPath") || __n.equals("path") || __n.equals("getUrl") || __n.equals("url") || __n.equals("getURI") || __n.equals("uri") || __n.equals("getHeaders") || __n.equals("headers") || __n.equals("getPathInfo") || __n.equals("pathInfo") || __n.equals("getEnv") || __n.equals("env")) __targetLike = true;
      }
    }
    for (java.lang.reflect.Field __f : __v.getClass().getFields()) {
      String __n = __f.getName();
      if (__n.equals("method") || __n.equals("requestMethod")) __methodLike = true;
      if (__n.equals("path") || __n.equals("url") || __n.equals("uri") || __n.equals("headers") || __n.equals("pathInfo") || __n.equals("env")) __targetLike = true;
    }
    return __methodLike && __targetLike;
  }
}).test(%s)`, base)
}

func runtimeRefJSStreamNextStepCode(ref RuntimeRef, valueVar, doneVar, readyVar, errVar, stateVar string) (string, bool) {
	if ref.Runtime != "javascript" {
		return "", false
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	stateRef := runtimeVarRef(ref.Runtime, stateVar)
	return fmt.Sprintf(`globalThis.%s = false;
globalThis.%s = undefined;
(function(){
  const __omnivm_stream_obj = %s;
  let __omnivm_step;
  if (typeof __omnivm_stream_obj.next === 'function') {
    __omnivm_step = __omnivm_stream_obj.next();
  } else if (typeof __omnivm_stream_obj.getReader === 'function') {
    const __omnivm_reader = %s || (%s = __omnivm_stream_obj.getReader());
    __omnivm_step = Promise.resolve(__omnivm_reader.read()).then(function(__omnivm_next) {
      if (__omnivm_next && __omnivm_next.done && typeof __omnivm_reader.releaseLock === 'function') {
        try { __omnivm_reader.releaseLock(); } catch (__omnivm_release_err) {}
        %s = undefined;
      }
      return __omnivm_next;
    });
  } else {
    const __omnivm_iter = %s || (%s = (typeof __omnivm_stream_obj[Symbol.asyncIterator] === 'function'
      ? __omnivm_stream_obj[Symbol.asyncIterator]()
      : __omnivm_stream_obj[Symbol.iterator]()));
    __omnivm_step = __omnivm_iter.next();
  }
  Promise.resolve(__omnivm_step).then(function(__omnivm_next) {
    globalThis.%s = __omnivm_next.value;
    globalThis.%s = !!__omnivm_next.done;
    globalThis.%s = true;
  }).catch(function(__omnivm_err) {
    globalThis.%s = __omnivm_err && __omnivm_err.message ? __omnivm_err.message : String(__omnivm_err);
    globalThis.%s = true;
  });
	})();`, readyVar, errVar, base, stateRef, stateRef, stateRef, stateRef, stateRef, valueVar, doneVar, readyVar, errVar, readyVar), true
}

func runtimeRefPythonStreamNextStepCode(ref RuntimeRef, valueVar, doneVar, readyVar, errVar, stateVar string) (string, bool) {
	if ref.Runtime != "python" {
		return "", false
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	stateRef := runtimeVarRef(ref.Runtime, stateVar)
	return fmt.Sprintf(`%s = False
%s = None
__omnivm_stream_obj = %s
__omnivm_http_message = %s
try:
    if not __omnivm_http_message and callable(getattr(__omnivm_stream_obj, '__next__', None)):
        %s = next(__omnivm_stream_obj)
        %s = False
        %s = True
    elif not __omnivm_http_message and callable(getattr(__omnivm_stream_obj, 'read', None)):
        try:
            %s = __omnivm_stream_obj.read(8192)
        except TypeError:
            %s = __omnivm_stream_obj.read()
        %s = (%s is None or %s == b'' or %s == '')
        %s = True
    elif not __omnivm_http_message and hasattr(__omnivm_stream_obj, '__iter__') and not hasattr(__omnivm_stream_obj, '__len__') and not isinstance(__omnivm_stream_obj, (__import__('collections.abc', fromlist=['Mapping']).Mapping, __import__('collections.abc', fromlist=['Sequence']).Sequence, __import__('collections.abc', fromlist=['Set']).Set, memoryview)):
        %s = globals().get(%q) or iter(__omnivm_stream_obj)
        %s = next(%s)
        %s = False
        %s = True
    elif not __omnivm_http_message and hasattr(__omnivm_stream_obj, '__aiter__') and not hasattr(__omnivm_stream_obj, '__len__') and not isinstance(__omnivm_stream_obj, (__import__('collections.abc', fromlist=['Mapping']).Mapping, __import__('collections.abc', fromlist=['Sequence']).Sequence, __import__('collections.abc', fromlist=['Set']).Set, memoryview)):
        import asyncio as __aio
        __omnivm_async_iter = globals().get(%q)
        if __omnivm_async_iter is None:
            __omnivm_async_iter = __omnivm_stream_obj.__aiter__()
            globals()[%q] = __omnivm_async_iter
        async def __omnivm_stream_next_task():
            try:
                try:
                    globals()[%q] = await __omnivm_async_iter.__anext__()
                    globals()[%q] = False
                except StopAsyncIteration:
                    globals()[%q] = None
                    globals()[%q] = True
                    globals()[%q] = None
            except BaseException as e:
                globals()[%q] = type(e).__name__ + ": " + str(e)
            finally:
                globals()[%q] = True
        __omnivm_loop = globals().get('__omnivm_stream_loop')
        if __omnivm_loop is None or __omnivm_loop.is_closed():
            __omnivm_loop = __aio.new_event_loop()
            globals()['__omnivm_stream_loop'] = __omnivm_loop
        __aio.ensure_future(__omnivm_stream_next_task(), loop=__omnivm_loop)
    else:
        %s = None
        %s = True
        %s = True
except StopIteration:
    %s = None
    %s = None
    %s = True
    %s = True
except BaseException as e:
    %s = type(e).__name__ + ": " + str(e)
    %s = True`, readyVar, errVar, base, pythonHTTPMessageProbeExpr("__omnivm_stream_obj"), valueVar, doneVar, readyVar, valueVar, valueVar, doneVar, valueVar, valueVar, valueVar, readyVar, stateRef, stateVar, valueVar, stateRef, doneVar, readyVar, stateVar, stateVar, valueVar, doneVar, valueVar, doneVar, stateVar, errVar, readyVar, valueVar, doneVar, readyVar, valueVar, stateRef, doneVar, readyVar, errVar, readyVar), true
}

func runtimeRefStreamNextCode(ref RuntimeRef, valueVar, doneVar, stateVar string) (string, bool) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	stateRef := runtimeVarRef(ref.Runtime, stateVar)
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("{ const __omnivm_stream_obj = %s; const __omnivm_iter = (typeof __omnivm_stream_obj.next === 'function') ? __omnivm_stream_obj : (%s || (%s = __omnivm_stream_obj[Symbol.iterator]())); const __omnivm_next = __omnivm_iter.next(); globalThis.%s = __omnivm_next.value; globalThis.%s = !!__omnivm_next.done; }", base, stateRef, stateRef, valueVar, doneVar), true
	case "python":
		return fmt.Sprintf("__omnivm_stream_obj = %s\n__omnivm_http_message = %s\ntry:\n    if not __omnivm_http_message and callable(getattr(__omnivm_stream_obj, '__next__', None)):\n        %s = next(__omnivm_stream_obj)\n        %s = False\n    elif not __omnivm_http_message and callable(getattr(__omnivm_stream_obj, 'read', None)):\n        try:\n            %s = __omnivm_stream_obj.read(8192)\n        except TypeError:\n            %s = __omnivm_stream_obj.read()\n        %s = (%s is None or %s == b'' or %s == '')\n    elif not __omnivm_http_message and hasattr(__omnivm_stream_obj, '__iter__') and not hasattr(__omnivm_stream_obj, '__len__') and not isinstance(__omnivm_stream_obj, (__import__('collections.abc', fromlist=['Mapping']).Mapping, __import__('collections.abc', fromlist=['Sequence']).Sequence, __import__('collections.abc', fromlist=['Set']).Set, memoryview)):\n        %s = globals().get(%q) or iter(__omnivm_stream_obj)\n        %s = next(%s)\n        %s = False\n    else:\n        %s = None\n        %s = True\nexcept StopIteration:\n    %s = None\n    %s = None\n    %s = True", base, pythonHTTPMessageProbeExpr("__omnivm_stream_obj"), valueVar, doneVar, valueVar, valueVar, doneVar, valueVar, valueVar, valueVar, stateRef, stateVar, valueVar, stateRef, doneVar, valueVar, doneVar, stateRef, valueVar, doneVar), true
	case "ruby":
		return fmt.Sprintf("begin; __omnivm_stream_obj = %s; __omnivm_method_like = __omnivm_stream_obj.respond_to?(:request_method) || (begin; __omnivm_stream_obj.respond_to?(:method) && ![Kernel, Object, BasicObject].include?(__omnivm_stream_obj.method(:method).owner); rescue; false; end); __omnivm_http_message = __omnivm_method_like && (__omnivm_stream_obj.respond_to?(:path) || __omnivm_stream_obj.respond_to?(:url) || __omnivm_stream_obj.respond_to?(:headers) || __omnivm_stream_obj.respond_to?(:env) || __omnivm_stream_obj.respond_to?(:path_info)); __omnivm_io = (!__omnivm_http_message && __omnivm_stream_obj.respond_to?(:to_io)) ? __omnivm_stream_obj.to_io : nil; if !__omnivm_http_message && __omnivm_stream_obj.respond_to?(:next); $%s = __omnivm_stream_obj.next; $%s = false; elsif !__omnivm_http_message && __omnivm_stream_obj.respond_to?(:read); $%s = __omnivm_stream_obj.read(8192); $%s = ($%s.nil? || $%s == \"\"); __omnivm_stream_obj.close if $%s && __omnivm_stream_obj.respond_to?(:close); elsif __omnivm_io.respond_to?(:read); $%s = __omnivm_io.read(8192); $%s = ($%s.nil? || $%s == \"\"); if $%s; if __omnivm_stream_obj.respond_to?(:close); __omnivm_stream_obj.close; elsif __omnivm_io.respond_to?(:close); __omnivm_io.close; end; end; elsif !__omnivm_http_message && __omnivm_stream_obj.respond_to?(:each) && !__omnivm_stream_obj.is_a?(Array) && !__omnivm_stream_obj.is_a?(Hash) && !__omnivm_stream_obj.is_a?(String); %s ||= __omnivm_stream_obj.each; $%s = %s.next; $%s = false; else; $%s = nil; $%s = true; end; rescue StopIteration, EOFError; %s = nil; $%s = nil; $%s = true; begin; __omnivm_stream_obj.close if defined?(__omnivm_stream_obj) && __omnivm_stream_obj.respond_to?(:close); rescue; end; end", base, valueVar, doneVar, valueVar, doneVar, valueVar, valueVar, doneVar, valueVar, doneVar, valueVar, valueVar, doneVar, stateRef, valueVar, stateRef, doneVar, valueVar, doneVar, stateRef, valueVar, doneVar), true
	case "java":
		return runtimeRefJavaStreamNextCode(base, valueVar, doneVar, stateVar), true
	case "__java_legacy":
		return fmt.Sprintf("Object __omnivm_stream_obj = %s; if (__omnivm_stream_obj instanceof java.util.Iterator) { java.util.Iterator __omnivm_next = (java.util.Iterator)(__omnivm_stream_obj); if (__omnivm_next.hasNext()) { omnivm.OmniVM.setCaptureObject(\"%s\", __omnivm_next.next()); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.FALSE); } else { omnivm.OmniVM.setCaptureObject(\"%s\", null); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.TRUE); } } else if (__omnivm_stream_obj instanceof java.io.InputStream) { try { byte[] __omnivm_buf = new byte[8192]; int __omnivm_n = ((java.io.InputStream)__omnivm_stream_obj).read(__omnivm_buf); if (__omnivm_n < 0) { omnivm.OmniVM.setCaptureObject(\"%s\", null); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.TRUE); } else { omnivm.OmniVM.setCaptureObject(\"%s\", java.util.Arrays.copyOf(__omnivm_buf, __omnivm_n)); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.FALSE); } } catch (java.io.IOException __omnivm_err) { throw new RuntimeException(__omnivm_err); } } else if (__omnivm_stream_obj instanceof java.nio.channels.ReadableByteChannel) { try { java.nio.ByteBuffer __omnivm_buf = java.nio.ByteBuffer.allocate(8192); int __omnivm_n = ((java.nio.channels.ReadableByteChannel)__omnivm_stream_obj).read(__omnivm_buf); if (__omnivm_n < 0) { omnivm.OmniVM.setCaptureObject(\"%s\", null); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.TRUE); } else { __omnivm_buf.flip(); byte[] __omnivm_out = new byte[__omnivm_buf.remaining()]; __omnivm_buf.get(__omnivm_out); omnivm.OmniVM.setCaptureObject(\"%s\", __omnivm_out); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.FALSE); } } catch (java.io.IOException __omnivm_err) { throw new RuntimeException(__omnivm_err); } } else if (__omnivm_stream_obj instanceof java.io.Reader) { try { char[] __omnivm_buf = new char[8192]; int __omnivm_n = ((java.io.Reader)__omnivm_stream_obj).read(__omnivm_buf); if (__omnivm_n < 0) { omnivm.OmniVM.setCaptureObject(\"%s\", null); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.TRUE); } else { omnivm.OmniVM.setCaptureObject(\"%s\", new String(__omnivm_buf, 0, __omnivm_n)); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.FALSE); } } catch (java.io.IOException __omnivm_err) { throw new RuntimeException(__omnivm_err); } } else if (__omnivm_stream_obj instanceof java.util.stream.BaseStream) { Object __omnivm_state = omnivm.OmniVM.getCapture(\"%s\"); java.util.Iterator __omnivm_next = (__omnivm_state instanceof java.util.Iterator) ? (java.util.Iterator)__omnivm_state : ((java.util.stream.BaseStream)__omnivm_stream_obj).iterator(); omnivm.OmniVM.setCaptureObject(\"%s\", __omnivm_next); if (__omnivm_next.hasNext()) { omnivm.OmniVM.setCaptureObject(\"%s\", __omnivm_next.next()); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.FALSE); } else { omnivm.OmniVM.setCaptureObject(\"%s\", null); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.TRUE); omnivm.OmniVM.setCaptureObject(\"%s\", null); } } else if ((__omnivm_stream_obj instanceof java.lang.Iterable) && !(__omnivm_stream_obj instanceof java.util.Collection) && !(__omnivm_stream_obj instanceof java.util.Map) && !(__omnivm_stream_obj instanceof java.lang.CharSequence)) { Object __omnivm_state = omnivm.OmniVM.getCapture(\"%s\"); java.util.Iterator __omnivm_next = (__omnivm_state instanceof java.util.Iterator) ? (java.util.Iterator)__omnivm_state : ((java.lang.Iterable)__omnivm_stream_obj).iterator(); omnivm.OmniVM.setCaptureObject(\"%s\", __omnivm_next); if (__omnivm_next.hasNext()) { omnivm.OmniVM.setCaptureObject(\"%s\", __omnivm_next.next()); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.FALSE); } else { omnivm.OmniVM.setCaptureObject(\"%s\", null); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.TRUE); omnivm.OmniVM.setCaptureObject(\"%s\", null); } } else { omnivm.OmniVM.setCaptureObject(\"%s\", null); omnivm.OmniVM.setCaptureObject(\"%s\", Boolean.TRUE); }", base, escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(stateVar), escapeJavaString(stateVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(stateVar), escapeJavaString(stateVar), escapeJavaString(stateVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(stateVar), escapeJavaString(valueVar), escapeJavaString(doneVar)), true
	default:
		return "", false
	}
}

func runtimeRefJavaStreamNextCode(base, valueVar, doneVar, stateVar string) string {
	return fmt.Sprintf(`Object __omnivm_stream_obj = %s;
try {
  if (__omnivm_stream_obj instanceof java.util.Iterator) {
    java.util.Iterator __omnivm_next = (java.util.Iterator)(__omnivm_stream_obj);
    if (__omnivm_next.hasNext()) {
      omnivm.OmniVM.setCaptureObject("%s", __omnivm_next.next());
      omnivm.OmniVM.setCaptureObject("%s", Boolean.FALSE);
    } else {
      omnivm.OmniVM.setCaptureObject("%s", null);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
    }
  } else if (__omnivm_stream_obj instanceof java.io.InputStream) {
    byte[] __omnivm_buf = new byte[8192];
    int __omnivm_n = ((java.io.InputStream)__omnivm_stream_obj).read(__omnivm_buf);
    if (__omnivm_n < 0) {
      omnivm.OmniVM.setCaptureObject("%s", null);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
    } else {
      omnivm.OmniVM.setCaptureObject("%s", java.util.Arrays.copyOf(__omnivm_buf, __omnivm_n));
      omnivm.OmniVM.setCaptureObject("%s", Boolean.FALSE);
    }
  } else if (__omnivm_stream_obj instanceof java.nio.channels.ReadableByteChannel) {
    java.nio.ByteBuffer __omnivm_buf = java.nio.ByteBuffer.allocate(8192);
    int __omnivm_n = ((java.nio.channels.ReadableByteChannel)__omnivm_stream_obj).read(__omnivm_buf);
    if (__omnivm_n < 0) {
      omnivm.OmniVM.setCaptureObject("%s", null);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
    } else {
      __omnivm_buf.flip();
      byte[] __omnivm_out = new byte[__omnivm_buf.remaining()];
      __omnivm_buf.get(__omnivm_out);
      omnivm.OmniVM.setCaptureObject("%s", __omnivm_out);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.FALSE);
    }
  } else if (__omnivm_stream_obj instanceof java.io.Reader) {
    char[] __omnivm_buf = new char[8192];
    int __omnivm_n = ((java.io.Reader)__omnivm_stream_obj).read(__omnivm_buf);
    if (__omnivm_n < 0) {
      omnivm.OmniVM.setCaptureObject("%s", null);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
    } else {
      omnivm.OmniVM.setCaptureObject("%s", new String(__omnivm_buf, 0, __omnivm_n));
      omnivm.OmniVM.setCaptureObject("%s", Boolean.FALSE);
    }
  } else if (%s) {
    Object __omnivm_state = omnivm.OmniVM.getCapture("%s");
    java.util.Iterator __omnivm_next = (__omnivm_state instanceof java.util.Iterator) ? (java.util.Iterator)__omnivm_state : null;
    if (__omnivm_next == null) {
      __omnivm_next = new omnivm.OmniVM.FlowPublisherIterator((java.util.concurrent.Flow.Publisher)__omnivm_stream_obj);
      omnivm.OmniVM.setCaptureObject("%s", __omnivm_next);
    }
    if (__omnivm_next.hasNext()) {
      omnivm.OmniVM.setCaptureObject("%s", __omnivm_next.next());
      omnivm.OmniVM.setCaptureObject("%s", Boolean.FALSE);
    } else {
      omnivm.OmniVM.setCaptureObject("%s", null);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
      omnivm.OmniVM.setCaptureObject("%s", null);
    }
  } else if (__omnivm_stream_obj instanceof java.util.stream.BaseStream || %s) {
    Object __omnivm_state = omnivm.OmniVM.getCapture("%s");
    java.util.stream.BaseStream __omnivm_base_stream = null;
    java.util.Iterator __omnivm_next = null;
    if (__omnivm_state instanceof Object[]) {
      Object[] __omnivm_pair = (Object[])__omnivm_state;
      if (__omnivm_pair.length > 0 && __omnivm_pair[0] instanceof java.util.stream.BaseStream) {
        __omnivm_base_stream = (java.util.stream.BaseStream)__omnivm_pair[0];
      }
      if (__omnivm_pair.length > 1 && __omnivm_pair[1] instanceof java.util.Iterator) {
        __omnivm_next = (java.util.Iterator)__omnivm_pair[1];
      }
    } else if (__omnivm_state instanceof java.util.Iterator) {
      __omnivm_next = (java.util.Iterator)__omnivm_state;
    }
    if (__omnivm_base_stream == null) {
      if (__omnivm_stream_obj instanceof java.util.stream.BaseStream) {
        __omnivm_base_stream = (java.util.stream.BaseStream)__omnivm_stream_obj;
      } else {
        for (java.lang.reflect.Method __omnivm_method : __omnivm_stream_obj.getClass().getMethods()) {
          if ((__omnivm_method.getName().equals("toStream") || __omnivm_method.getName().equals("blockingStream")) && __omnivm_method.getParameterCount() == 0 && java.util.stream.BaseStream.class.isAssignableFrom(__omnivm_method.getReturnType())) {
            Object __omnivm_converted = __omnivm_method.invoke(__omnivm_stream_obj);
            if (__omnivm_converted instanceof java.util.stream.BaseStream) {
              __omnivm_base_stream = (java.util.stream.BaseStream)__omnivm_converted;
            }
            break;
          }
        }
      }
    }
    if (__omnivm_next == null && __omnivm_base_stream != null) {
      __omnivm_next = __omnivm_base_stream.iterator();
      omnivm.OmniVM.setCaptureObject("%s", new Object[] { __omnivm_base_stream, __omnivm_next });
    }
    if (__omnivm_next != null && __omnivm_next.hasNext()) {
      omnivm.OmniVM.setCaptureObject("%s", __omnivm_next.next());
      omnivm.OmniVM.setCaptureObject("%s", Boolean.FALSE);
    } else {
      if (__omnivm_base_stream != null) {
        __omnivm_base_stream.close();
      }
      omnivm.OmniVM.setCaptureObject("%s", null);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
      omnivm.OmniVM.setCaptureObject("%s", null);
    }
  } else if ((__omnivm_stream_obj instanceof java.lang.Iterable) && !(__omnivm_stream_obj instanceof java.util.Collection) && !(__omnivm_stream_obj instanceof java.util.Map) && !(__omnivm_stream_obj instanceof java.lang.CharSequence)) {
    Object __omnivm_state = omnivm.OmniVM.getCapture("%s");
    java.util.Iterator __omnivm_next = (__omnivm_state instanceof java.util.Iterator) ? (java.util.Iterator)__omnivm_state : ((java.lang.Iterable)__omnivm_stream_obj).iterator();
    omnivm.OmniVM.setCaptureObject("%s", __omnivm_next);
    if (__omnivm_next.hasNext()) {
      omnivm.OmniVM.setCaptureObject("%s", __omnivm_next.next());
      omnivm.OmniVM.setCaptureObject("%s", Boolean.FALSE);
    } else {
      omnivm.OmniVM.setCaptureObject("%s", null);
      omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
      omnivm.OmniVM.setCaptureObject("%s", null);
    }
  } else {
    omnivm.OmniVM.setCaptureObject("%s", null);
    omnivm.OmniVM.setCaptureObject("%s", Boolean.TRUE);
  }
} catch (Exception __omnivm_err) {
  throw new RuntimeException(__omnivm_err);
}`, base,
		escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar),
		escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar),
		escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar),
		escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar),
		javaFlowPublisherProbeExpr("__omnivm_stream_obj"), escapeJavaString(stateVar), escapeJavaString(stateVar),
		escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(stateVar),
		javaToStreamProbeExpr("__omnivm_stream_obj"), escapeJavaString(stateVar), escapeJavaString(stateVar),
		escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(stateVar),
		escapeJavaString(stateVar), escapeJavaString(stateVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(valueVar), escapeJavaString(doneVar), escapeJavaString(stateVar),
		escapeJavaString(valueVar), escapeJavaString(doneVar))
}

func runtimeRefStreamCloseCode(ref RuntimeRef, stateVar string) (string, bool) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	stateRef := runtimeVarRef(ref.Runtime, stateVar)
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("{ const __omnivm_stream_obj = %s; const __omnivm_iter = %s; if (__omnivm_iter && typeof __omnivm_iter.return === 'function') __omnivm_iter.return(); else if (__omnivm_iter && typeof __omnivm_iter.cancel === 'function') __omnivm_iter.cancel(); else if (__omnivm_stream_obj && typeof __omnivm_stream_obj.return === 'function') __omnivm_stream_obj.return(); else if (__omnivm_stream_obj && typeof __omnivm_stream_obj.cancel === 'function') __omnivm_stream_obj.cancel(); if (__omnivm_iter && typeof __omnivm_iter.releaseLock === 'function') { try { __omnivm_iter.releaseLock(); } catch (__omnivm_release_err) {} } %s = undefined; }", base, stateRef, stateRef), true
	case "python":
		return fmt.Sprintf(`__omnivm_stream_obj = %s
__omnivm_stream_iter = globals().get(%q)
__omnivm_close_seen = set()
def __omnivm_maybe_await_close(__omnivm_result):
    import inspect as __inspect
    if not __inspect.isawaitable(__omnivm_result):
        return
    import asyncio as __aio
    __omnivm_loop = globals().get('__omnivm_stream_loop')
    if __omnivm_loop is None or __omnivm_loop.is_closed():
        try:
            __omnivm_loop = __aio.get_event_loop()
        except RuntimeError:
            __omnivm_loop = __aio.new_event_loop()
            __aio.set_event_loop(__omnivm_loop)
    if __omnivm_loop.is_running():
        __omnivm_close_loop = __aio.new_event_loop()
        try:
            __omnivm_close_loop.run_until_complete(__omnivm_result)
        finally:
            __omnivm_close_loop.close()
    else:
        __omnivm_loop.run_until_complete(__omnivm_result)
def __omnivm_close_one(__omnivm_target):
    if __omnivm_target is None:
        return
    __omnivm_target_id = id(__omnivm_target)
    if __omnivm_target_id in __omnivm_close_seen:
        return
    __omnivm_close_seen.add(__omnivm_target_id)
    __omnivm_target_aclose = getattr(__omnivm_target, 'aclose', None)
    __omnivm_target_close = getattr(__omnivm_target, 'close', None)
    if callable(__omnivm_target_aclose):
        __omnivm_maybe_await_close(__omnivm_target_aclose())
    elif callable(__omnivm_target_close):
        __omnivm_target_close()
def __omnivm_close_frame_iterators(__omnivm_target):
    for __omnivm_frame_attr in ('ag_frame', 'gi_frame'):
        __omnivm_frame = getattr(__omnivm_target, __omnivm_frame_attr, None)
        if __omnivm_frame is None:
            continue
        for __omnivm_local in list(getattr(__omnivm_frame, 'f_locals', {}).values()):
            if __omnivm_local is not __omnivm_target and (
                callable(getattr(__omnivm_local, 'aclose', None)) or
                callable(getattr(__omnivm_local, 'close', None))
            ):
                __omnivm_close_one(__omnivm_local)
__omnivm_close_frame_iterators(__omnivm_stream_iter)
__omnivm_close_frame_iterators(__omnivm_stream_obj)
__omnivm_stream_iter_aclose = getattr(__omnivm_stream_iter, 'aclose', None)
__omnivm_stream_iter_close = getattr(__omnivm_stream_iter, 'close', None)
if callable(__omnivm_stream_iter_aclose):
    __omnivm_maybe_await_close(__omnivm_stream_iter_aclose())
elif callable(__omnivm_stream_iter_close):
    __omnivm_stream_iter_close()
__omnivm_stream_aclose = getattr(__omnivm_stream_obj, 'aclose', None)
__omnivm_stream_close = getattr(__omnivm_stream_obj, 'close', None)
if __omnivm_stream_obj is not __omnivm_stream_iter:
    if callable(__omnivm_stream_aclose):
        __omnivm_maybe_await_close(__omnivm_stream_aclose())
    elif callable(__omnivm_stream_close):
        __omnivm_stream_close()
globals()[%q] = None`, base, stateVar, stateVar), true
	case "ruby":
		return fmt.Sprintf("begin; __omnivm_stream_obj = %s; __omnivm_io = __omnivm_stream_obj.respond_to?(:to_io) ? __omnivm_stream_obj.to_io : nil; if __omnivm_stream_obj.respond_to?(:close); __omnivm_stream_obj.close; elsif __omnivm_stream_obj.respond_to?(:return); __omnivm_stream_obj.return; elsif __omnivm_io.respond_to?(:close); __omnivm_io.close; end; %s = nil; end", base, stateRef), true
	case "java":
		return runtimeRefJavaStreamCloseCode(base, stateVar), true
	default:
		return "", false
	}
}

func runtimeRefJavaStreamCloseCode(base, stateVar string) string {
	return fmt.Sprintf(`Object __omnivm_stream_obj = %s;
Object __omnivm_state = omnivm.OmniVM.getCapture("%s");
try {
  if (__omnivm_state instanceof Object[]) {
    Object[] __omnivm_pair = (Object[])__omnivm_state;
    if (__omnivm_pair.length > 0 && __omnivm_pair[0] instanceof AutoCloseable) {
      ((AutoCloseable)__omnivm_pair[0]).close();
    }
  } else if (__omnivm_state instanceof AutoCloseable) {
    ((AutoCloseable)__omnivm_state).close();
  }
  if (__omnivm_stream_obj instanceof AutoCloseable && __omnivm_stream_obj != __omnivm_state) {
    ((AutoCloseable)__omnivm_stream_obj).close();
  }
  omnivm.OmniVM.setCaptureObject("%s", null);
} catch (Exception __omnivm_err) {
  throw new RuntimeException(__omnivm_err);
}`, base, escapeJavaString(stateVar), escapeJavaString(stateVar))
}

func pythonRuntimeRefSetCode(base, keyLit, valueLit string) string {
	return fmt.Sprintf("__o = %s\n__k = %s\n__v = %s\n__abc = __import__('collections.abc', fromlist=['MutableMapping', 'MutableSequence'])\nif isinstance(__o, __abc.MutableMapping):\n    __o[__k] = __v\nelif __k == 'length' and isinstance(__o, __abc.MutableSequence) and not isinstance(__o, (str, bytes, bytearray)):\n    __n = __import__('operator').index(__v)\n    if __n < 0:\n        raise ValueError('negative length')\n    del __o[__n:]\n    if __n > len(__o):\n        __o.extend([None] * (__n - len(__o)))\nelif isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {}):\n    setattr(__o, __k, __v)\nelif isinstance(__k, str) and __k.isdigit() and hasattr(__o, '__setitem__') and not hasattr(__o, __k):\n    try:\n        __o.__setitem__(int(__k), __v)\n    except (TypeError, IndexError, KeyError):\n        __o.__setitem__(__k, __v)\nelif hasattr(__o, __k):\n    setattr(__o, __k, __v)\nelse:\n    __o.__setitem__(__k, __v)", base, keyLit, valueLit)
}

func rubyRuntimeRefSetCode(base, keyLit, valueLit string) string {
	return fmt.Sprintf("begin; __o = %s; __k = %s; __v = %s; __setter = \"#{__k}=\"; __seq_index = __k.is_a?(String) && __k.match?(/\\A\\d+\\z/) && __o.respond_to?(:[]=) && __o.respond_to?(:each_with_index) && !__o.respond_to?(:key?); if __seq_index; begin; __o[__k.to_i] = __v; rescue TypeError, IndexError; __o[__k] = __v; end; elsif __k == \"length\" && __o.is_a?(Array); __n = Integer(__v); raise RangeError, \"negative length\" if __n < 0; if __n < __o.length; __o.slice!(__n, __o.length - __n); elsif __n > __o.length; __o.concat(Array.new(__n - __o.length)); end; elsif __o.respond_to?(:has_attribute?) && __o.has_attribute?(__k) && __o.respond_to?(:[]=); __o[__k] = __v; elsif __o.respond_to?(__setter); __o.public_send(__setter, __v); else; __o[__k] = __v; end; end", base, keyLit, valueLit)
}

func javaRuntimeRefSetCode(base, keyLit, valueLit string) string {
	return fmt.Sprintf("{ String __omnivm_set_key = String.valueOf(%s); if (!omnivm.OmniVM.proxySet(%s, __omnivm_set_key, %s)) throw new IllegalArgumentException(\"OmniVM Java proxy rejected set for key \" + __omnivm_set_key); }", keyLit, base, valueLit)
}

func runtimeRefSetCode(ref RuntimeRef, key string, value interface{}) (string, bool, error) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	valueLit, err := runtimeValueLiteral(ref.Runtime, value)
	if err != nil {
		return "", false, err
	}
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("(%s)[%s] = %s;", base, keyLit, valueLit), true, nil
	case "python":
		return pythonRuntimeRefSetCode(base, keyLit, valueLit), true, nil
	case "ruby":
		return rubyRuntimeRefSetCode(base, keyLit, valueLit), true, nil
	case "java":
		return javaRuntimeRefSetCode(base, keyLit, valueLit), true, nil
	default:
		return "", false, nil
	}
}

func (e *Executor) runtimeRefSetCode(ref RuntimeRef, key string, value interface{}) (string, bool, error) {
	builder := &runtimeExprBuilder{executor: e, targetRuntime: ref.Runtime}
	keyLit, err := builder.expr(key)
	if err != nil {
		return "", false, err
	}
	valueLit, err := builder.expr(value)
	if err != nil {
		return "", false, err
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	var code string
	switch ref.Runtime {
	case "javascript":
		code = fmt.Sprintf("(%s)[%s] = %s;", base, keyLit, valueLit)
	case "python":
		code = pythonRuntimeRefSetCode(base, keyLit, valueLit)
	case "ruby":
		code = rubyRuntimeRefSetCode(base, keyLit, valueLit)
	case "java":
		code = javaRuntimeRefSetCode(base, keyLit, valueLit)
	default:
		return "", false, nil
	}
	return prependRuntimeExprPrelude(builder, code), true, nil
}

func runtimeRefLenExpr(ref RuntimeRef) (string, bool) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("(%s).length", base), true
	case "python":
		return fmt.Sprintf("len(%s)", base), true
	case "ruby":
		return fmt.Sprintf("(%s).length", base), true
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyLen(%s)", base), true
	default:
		return "", false
	}
}

func runtimeRefIterExpr(ref RuntimeRef, mode string) (string, bool, error) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	switch ref.Runtime {
	case "javascript":
		switch mode {
		case "items":
			return fmt.Sprintf("(Array.isArray(%s) ? (%s).map(function(v, i){ return [i, v]; }) : Object.entries(%s))", base, base, base), true, nil
		case "keys":
			return fmt.Sprintf("(Array.isArray(%s) ? (%s).map(function(_, i){ return i; }) : Object.keys(%s))", base, base, base), true, nil
		default:
			return fmt.Sprintf("(Array.isArray(%s) ? Array.from(%s) : Object.values(%s))", base, base, base), true, nil
		}
	case "python":
		switch mode {
		case "items":
			return fmt.Sprintf("(lambda __o: (list(__o.items()) if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) else (list(enumerate(__o)) if hasattr(__o, '__iter__') and not isinstance(__o, (str, bytes, bytearray)) else [])))(%s)", base), true, nil
		case "keys":
			return fmt.Sprintf("(lambda __o: (list(__o.keys()) if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) else (list(range(len(__o))) if hasattr(__o, '__len__') and not isinstance(__o, (str, bytes, bytearray)) else [])))(%s)", base), true, nil
		default:
			return fmt.Sprintf("(lambda __o: (list(__o.values()) if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) else (list(__o) if hasattr(__o, '__iter__') and not isinstance(__o, (str, bytes, bytearray)) else [])))(%s)", base), true, nil
		}
	case "ruby":
		switch mode {
		case "items":
			return fmt.Sprintf("(begin; __o = %s; __o.respond_to?(:to_h) ? __o.to_h.to_a : __o.each_with_index.map { |v, i| [i, v] }; end)", base), true, nil
		case "keys":
			return fmt.Sprintf("(begin; __o = %s; __o.respond_to?(:keys) ? __o.keys : (0...__o.length).to_a; end)", base), true, nil
		default:
			return fmt.Sprintf("(begin; __o = %s; __o.respond_to?(:values) ? __o.values : __o.to_a; end)", base), true, nil
		}
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyIter(%s, %s)", base, jsonStringLiteral(mode)), true, nil
	default:
		return "", false, nil
	}
}

func runtimeRefContainsExpr(ref RuntimeRef, key interface{}) (string, bool, error) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("((Array.isArray(%s) && (%s).includes(%s)) || (%s in %s))", base, base, keyLit, keyLit, base), true, nil
	case "python":
		return fmt.Sprintf("(lambda __o, __k: (((isinstance(__k, int) or (isinstance(__k, str) and __k.isdigit())) and hasattr(__o, '__len__') and hasattr(__o, '__getitem__') and not isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and int(__k) >= 0 and int(__k) < len(__o)) or (isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and __k in __o) or (isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {})) or (isinstance(__k, str) and hasattr(__o, __k)) or (hasattr(__o, '__contains__') and __k in __o)))(%s, %s)", base, keyLit), true, nil
	case "ruby":
		return fmt.Sprintf("(begin; __o = %s; __k = %s; __idx = (__k.is_a?(Integer) || (__k.is_a?(String) && __k.match?(/\\A\\d+\\z/))) && __o.respond_to?(:length) && __o.respond_to?(:each_with_index) && !__o.respond_to?(:key?) && __k.to_i >= 0 && __k.to_i < __o.length; __idx || (__o.respond_to?(:key?) && __o.key?(__k)) || (__k.is_a?(String) && __o.respond_to?(__k)) || (__o.respond_to?(:include?) && __o.include?(__k)); end)", base, keyLit), true, nil
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyContains(%s, %s)", base, keyLit), true, nil
	default:
		return "", false, nil
	}
}

func (e *Executor) runtimeRefContainsExpr(ref RuntimeRef, key interface{}) (string, bool, error) {
	builder := &runtimeExprBuilder{executor: e, targetRuntime: ref.Runtime}
	return runtimeRefContainsExprWithBuilder(ref, key, builder)
}

func runtimeRefContainsExprWithBuilder(ref RuntimeRef, key interface{}, builder *runtimeExprBuilder) (string, bool, error) {
	keyLit, err := builder.expr(key)
	if err != nil {
		return "", false, err
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	var expr string
	switch ref.Runtime {
	case "javascript":
		expr = fmt.Sprintf("((Array.isArray(%s) && (%s).includes(%s)) || (%s in %s))", base, base, keyLit, keyLit, base)
	case "python":
		expr = fmt.Sprintf("(lambda __o, __k: (((isinstance(__k, int) or (isinstance(__k, str) and __k.isdigit())) and hasattr(__o, '__len__') and hasattr(__o, '__getitem__') and not isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and int(__k) >= 0 and int(__k) < len(__o)) or (isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and __k in __o) or (isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {})) or (isinstance(__k, str) and hasattr(__o, __k)) or (hasattr(__o, '__contains__') and __k in __o)))(%s, %s)", base, keyLit)
	case "ruby":
		expr = fmt.Sprintf("(begin; __o = %s; __k = %s; __idx = (__k.is_a?(Integer) || (__k.is_a?(String) && __k.match?(/\\A\\d+\\z/))) && __o.respond_to?(:length) && __o.respond_to?(:each_with_index) && !__o.respond_to?(:key?) && __k.to_i >= 0 && __k.to_i < __o.length; __idx || (__o.respond_to?(:key?) && __o.key?(__k)) || (__k.is_a?(String) && __o.respond_to?(__k)) || (__o.respond_to?(:include?) && __o.include?(__k)); end)", base, keyLit)
	case "java":
		expr = fmt.Sprintf("omnivm.OmniVM.proxyContains(%s, %s)", base, keyLit)
	default:
		return "", false, nil
	}
	return expr, true, nil
}

func runtimeRefCallableExpr(ref RuntimeRef, key string) (string, bool, error) {
	if key == "" {
		expr, ok := runtimeRefTargetCallableExpr(ref)
		return expr, ok, nil
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("(typeof (%s)[%s] === 'function')", base, keyLit), true, nil
	case "python":
		return fmt.Sprintf("(lambda __o, __k: (callable(__o[__k]) if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and __k in __o else (callable(getattr(__o, __k)) if isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {}) else (callable(getattr(__o, __k)) if isinstance(__k, str) and hasattr(__o, __k) else False))))(%s, %s)", base, keyLit), true, nil
	case "ruby":
		return fmt.Sprintf("(begin; __o = %s; __k = %s; (__o.respond_to?(:key?) && __o.key?(__k)) || (__o.respond_to?(:has_attribute?) && __o.has_attribute?(__k)) ? __o[__k].respond_to?(:call) : __o.respond_to?(__k); end)", base, keyLit), true, nil
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyCallable(%s, %s)", base, keyLit), true, nil
	default:
		return "", false, nil
	}
}

func runtimeRefTargetCallableExpr(ref RuntimeRef) (string, bool) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("(typeof %s === 'function')", base), true
	case "python":
		return fmt.Sprintf("callable(%s)", base), true
	case "ruby":
		return fmt.Sprintf("(begin; (%s).respond_to?(:call); end)", base), true
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyCallable(%s, \"\")", base), true
	default:
		return "", false
	}
}

func runtimeRefCallExpr(ref RuntimeRef, key string, args []interface{}) (string, bool, error) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	argsLit, err := runtimeValueLiteral(ref.Runtime, args)
	if err != nil {
		return "", false, err
	}
	if key == "" {
		switch ref.Runtime {
		case "javascript":
			return fmt.Sprintf("(%s).apply(undefined, %s)", base, argsLit), true, nil
		case "python":
			return fmt.Sprintf("(lambda __o, __args: __o(*__args))(%s, %s)", base, argsLit), true, nil
		case "ruby":
			return fmt.Sprintf("(begin; __o = %s; __args = %s; __o.call(*__args); end)", base, argsLit), true, nil
		case "java":
			return fmt.Sprintf("omnivm.OmniVM.proxyCall(%s, \"\", %s)", base, argsLit), true, nil
		default:
			return "", false, nil
		}
	}
	keyLit, err := runtimeValueLiteral(ref.Runtime, key)
	if err != nil {
		return "", false, err
	}
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf("(%s)[%s].apply(%s, %s)", base, keyLit, base, argsLit), true, nil
	case "python":
		return fmt.Sprintf("(lambda __o, __k, __args: (__o[__k] if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and __k in __o else (getattr(__o, __k) if isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {}) else (getattr(__o, __k) if isinstance(__k, str) and hasattr(__o, __k) else __o[__k])))(*__args))(%s, %s, %s)", base, keyLit, argsLit), true, nil
	case "ruby":
		return fmt.Sprintf("(begin; __o = %s; __k = %s; __args = %s; (__o.respond_to?(:key?) && __o.key?(__k)) ? __o[__k].call(*__args) : (__o.respond_to?(__k) ? __o.public_send(__k, *__args) : __o[__k].call(*__args)); end)", base, keyLit, argsLit), true, nil
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.proxyCall(%s, %s, %s)", base, keyLit, argsLit), true, nil
	default:
		return "", false, nil
	}
}

func (e *Executor) runtimeRefCallExpr(ref RuntimeRef, key string, args []interface{}) (string, bool, error) {
	builder := &runtimeExprBuilder{executor: e, targetRuntime: ref.Runtime}
	return runtimeRefCallExprWithBuilder(ref, key, args, nil, builder)
}

func runtimeRefCallExprWithBuilder(ref RuntimeRef, key string, args []interface{}, kwargs map[string]interface{}, builder *runtimeExprBuilder) (string, bool, error) {
	argsLit, err := builder.expr(args)
	if err != nil {
		return "", false, err
	}
	kwargsLit := "{}"
	if len(kwargs) > 0 {
		kwargsLit, err = builder.expr(kwargs)
		if err != nil {
			return "", false, err
		}
	}
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	var expr string
	if key == "" {
		switch ref.Runtime {
		case "javascript":
			if len(kwargs) > 0 {
				if !runtimeRefAcceptsJSOptionsKwargs(ref, kwargs) {
					return "", false, runtimeRefKeywordDiagnostic(ref, "", kwargs, builder, jsKwargsRejectReason(ref, kwargs, false), "declare callable_shape.acceptsOptionsObject/destructuredKeys for the captured function, or pass an explicit options object as the final positional argument")
				}
				expr = fmt.Sprintf("(function(__fn, __args, __kwargs) { return __fn.apply(undefined, __args.concat([__kwargs])); })(%s, %s, %s)", base, argsLit, kwargsLit)
			} else {
				expr = fmt.Sprintf("(%s).apply(undefined, %s)", base, argsLit)
			}
		case "python":
			expr = fmt.Sprintf("(lambda __o, __args, __kwargs: __o(*__args, **__kwargs))(%s, %s, %s)", base, argsLit, kwargsLit)
		case "ruby":
			if len(kwargs) > 0 {
				expr = fmt.Sprintf("(begin; __o = %s; __args = %s; __kwargs = (%s).transform_keys { |k| k.respond_to?(:to_sym) ? k.to_sym : k }; __o.call(*__args, **__kwargs); end)", base, argsLit, kwargsLit)
			} else {
				expr = fmt.Sprintf("(begin; __o = %s; __args = %s; __o.call(*__args); end)", base, argsLit)
			}
		case "java":
			if len(kwargs) > 0 {
				if !runtimeRefAcceptsJavaKwargs(ref, "", kwargs) {
					return "", false, runtimeRefKeywordDiagnostic(ref, "", kwargs, builder, javaKwargsRejectReason(ref, "", kwargs), "declare callable_shape.javaAdapter with kind map, record, or builder and matching keys, or pass an explicit Java options object positionally")
				}
				adapterArgs, err := javaAdapterArgs(ref, args, kwargs, builder)
				if err != nil {
					return "", false, err
				}
				adapterArgsLit, err := builder.expr(adapterArgs)
				if err != nil {
					return "", false, err
				}
				expr = fmt.Sprintf("omnivm.OmniVM.proxyCall(%s, \"\", %s)", base, adapterArgsLit)
			} else {
				expr = fmt.Sprintf("omnivm.OmniVM.proxyCall(%s, \"\", %s)", base, argsLit)
			}
		default:
			return "", false, nil
		}
		return expr, true, nil
	}
	keyLit, err := builder.expr(key)
	if err != nil {
		return "", false, err
	}
	switch ref.Runtime {
	case "javascript":
		if len(kwargs) > 0 {
			return "", false, runtimeRefKeywordDiagnostic(ref, key, kwargs, builder, jsKwargsRejectReason(ref, kwargs, true), "capture the JavaScript method as a function with callable_shape.acceptsOptionsObject/destructuredKeys, or pass an explicit options object as the final positional argument")
		}
		expr = fmt.Sprintf("(%s)[%s].apply(%s, %s)", base, keyLit, base, argsLit)
	case "python":
		expr = fmt.Sprintf("(lambda __o, __k, __args, __kwargs: (__o[__k] if isinstance(__o, __import__('collections.abc', fromlist=['Mapping']).Mapping) and __k in __o else (getattr(__o, __k) if isinstance(__k, str) and __k in getattr(type(__o), 'model_fields', {}) else (getattr(__o, __k) if isinstance(__k, str) and hasattr(__o, __k) else __o[__k])))(*__args, **__kwargs))(%s, %s, %s, %s)", base, keyLit, argsLit, kwargsLit)
	case "ruby":
		if len(kwargs) > 0 {
			expr = fmt.Sprintf("(begin; __o = %s; __k = %s; __args = %s; __kwargs = (%s).transform_keys { |k| k.respond_to?(:to_sym) ? k.to_sym : k }; (__o.respond_to?(:key?) && __o.key?(__k)) ? __o[__k].call(*__args, **__kwargs) : (__o.respond_to?(__k) ? __o.public_send(__k, *__args, **__kwargs) : __o[__k].call(*__args, **__kwargs)); end)", base, keyLit, argsLit, kwargsLit)
		} else {
			expr = fmt.Sprintf("(begin; __o = %s; __k = %s; __args = %s; (__o.respond_to?(:key?) && __o.key?(__k)) ? __o[__k].call(*__args) : (__o.respond_to?(__k) ? __o.public_send(__k, *__args) : __o[__k].call(*__args)); end)", base, keyLit, argsLit)
		}
	case "java":
		if len(kwargs) > 0 {
			if !runtimeRefAcceptsJavaKwargs(ref, key, kwargs) {
				return "", false, runtimeRefKeywordDiagnostic(ref, key, kwargs, builder, javaKwargsRejectReason(ref, key, kwargs), "declare callable_shape.javaAdapter with kind map, record, or builder, the matching method name, and accepted keys, or pass an explicit Java options object positionally")
			}
			adapterArgs, err := javaAdapterArgs(ref, args, kwargs, builder)
			if err != nil {
				return "", false, err
			}
			adapterArgsLit, err := builder.expr(adapterArgs)
			if err != nil {
				return "", false, err
			}
			expr = fmt.Sprintf("omnivm.OmniVM.proxyCall(%s, %s, %s)", base, keyLit, adapterArgsLit)
		} else {
			expr = fmt.Sprintf("omnivm.OmniVM.proxyCall(%s, %s, %s)", base, keyLit, argsLit)
		}
	default:
		return "", false, nil
	}
	return expr, true, nil
}

func appendRuntimeRefKwargsArg(args []interface{}, kwargs map[string]interface{}) []interface{} {
	out := make([]interface{}, 0, len(args)+1)
	out = append(out, args...)
	out = append(out, kwargs)
	return out
}

func javaAdapterArgs(ref RuntimeRef, args []interface{}, kwargs map[string]interface{}, builder *runtimeExprBuilder) ([]interface{}, error) {
	adapter := ref.CallableShape.JavaAdapter
	switch adapter.Kind {
	case "map":
		return appendRuntimeRefKwargsArg(args, kwargs), nil
	case "record":
		if adapter.TargetType == "" {
			return nil, fmt.Errorf("Java record adapter requires targetType")
		}
		kwargsExpr, err := builder.expr(kwargs)
		if err != nil {
			return nil, err
		}
		keysExpr, err := builder.expr(adapter.Keys)
		if err != nil {
			return nil, err
		}
		out := append([]interface{}{}, args...)
		out = append(out, RuntimeRef{
			Runtime: "java",
			VarName: fmt.Sprintf("omnivm.OmniVM.kwargsRecord(%q, %s, %s)",
				adapter.TargetType,
				kwargsExpr,
				keysExpr,
			),
		})
		return out, nil
	case "builder":
		if adapter.TargetType == "" {
			return nil, fmt.Errorf("Java builder adapter requires targetType")
		}
		kwargsExpr, err := builder.expr(kwargs)
		if err != nil {
			return nil, err
		}
		keysExpr, err := builder.expr(adapter.Keys)
		if err != nil {
			return nil, err
		}
		out := append([]interface{}{}, args...)
		out = append(out, RuntimeRef{
			Runtime: "java",
			VarName: fmt.Sprintf("omnivm.OmniVM.kwargsBuilder(%q, %s, %s)",
				adapter.TargetType,
				kwargsExpr,
				keysExpr,
			),
		})
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported Java adapter kind %q", adapter.Kind)
	}
}

func runtimeRefKeywordDiagnostic(ref RuntimeRef, key string, kwargs map[string]interface{}, builder *runtimeExprBuilder, reason, fix string) error {
	targetRuntime := ref.Runtime
	if builder != nil && builder.targetRuntime != "" {
		targetRuntime = builder.targetRuntime
	}
	callShape := "callable"
	if key != "" {
		callShape = fmt.Sprintf("method %q", key)
	}
	return fmt.Errorf("keyword proxy call rejected: source runtime=%s target runtime=%s call shape=%s kwargs=%s detected shape=%s reason=%s smallest fix=%s",
		ref.Runtime,
		targetRuntime,
		callShape,
		runtimeRefKeywordNames(kwargs),
		callableShapeSummary(ref.CallableShape),
		reason,
		fix,
	)
}

func runtimeRefKeywordNames(kwargs map[string]interface{}) string {
	if len(kwargs) == 0 {
		return "[]"
	}
	keys := make([]string, 0, len(kwargs))
	for key := range kwargs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return "[" + strings.Join(keys, ",") + "]"
}

func callableShapeSummary(shape *CallableShape) string {
	if shape == nil {
		return "none"
	}
	parts := []string{}
	if shape.AcceptsKwargs {
		parts = append(parts, "acceptsKwargs")
	}
	if shape.AcceptsOptionsObject {
		parts = append(parts, "acceptsOptionsObject")
	}
	if len(shape.DestructuredKeys) > 0 {
		parts = append(parts, "destructuredKeys="+runtimeRefSortedList(shape.DestructuredKeys))
	}
	if len(shape.ParameterNames) > 0 {
		parts = append(parts, "parameterNames="+runtimeRefSortedList(shape.ParameterNames))
	}
	if shape.Arity != nil {
		parts = append(parts, fmt.Sprintf("arity=%d", *shape.Arity))
	}
	if shape.JavaAdapter != nil {
		adapterParts := []string{}
		if shape.JavaAdapter.Kind != "" {
			adapterParts = append(adapterParts, "kind="+shape.JavaAdapter.Kind)
		}
		if shape.JavaAdapter.Method != "" {
			adapterParts = append(adapterParts, "method="+shape.JavaAdapter.Method)
		}
		if shape.JavaAdapter.TargetType != "" {
			adapterParts = append(adapterParts, "targetType="+shape.JavaAdapter.TargetType)
		}
		if len(shape.JavaAdapter.Keys) > 0 {
			adapterParts = append(adapterParts, "keys="+runtimeRefSortedList(shape.JavaAdapter.Keys))
		}
		parts = append(parts, "javaAdapter{"+strings.Join(adapterParts, ",")+"}")
	}
	if len(parts) == 0 {
		return "empty"
	}
	return strings.Join(parts, ";")
}

func runtimeRefSortedList(values []string) string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return "[" + strings.Join(out, ",") + "]"
}

func jsKwargsRejectReason(ref RuntimeRef, kwargs map[string]interface{}, methodCall bool) string {
	if methodCall {
		return "JavaScript method property has no proven options-object callable shape"
	}
	shape := ref.CallableShape
	if shape == nil {
		return "no callable_shape metadata was captured for the JavaScript function"
	}
	if !shape.AcceptsOptionsObject {
		return "callable_shape does not declare acceptsOptionsObject"
	}
	if unknown := firstUnknownKey(kwargs, shape.DestructuredKeys); unknown != "" {
		return fmt.Sprintf("keyword %q is not in destructuredKeys %s", unknown, runtimeRefSortedList(shape.DestructuredKeys))
	}
	return "callable_shape is incompatible with JavaScript options-object kwargs"
}

func javaKwargsRejectReason(ref RuntimeRef, method string, kwargs map[string]interface{}) string {
	shape := ref.CallableShape
	if shape == nil {
		return "no callable_shape metadata was captured for the Java target"
	}
	if shape.JavaAdapter == nil {
		return "callable_shape has no javaAdapter"
	}
	adapter := shape.JavaAdapter
	switch adapter.Kind {
	case "map", "record", "builder":
	default:
		return fmt.Sprintf("javaAdapter kind %q is unsupported", adapter.Kind)
	}
	if method != "" && adapter.Method != method {
		return fmt.Sprintf("javaAdapter method %q does not match method %q", adapter.Method, method)
	}
	if method == "" && adapter.Method != "" {
		return fmt.Sprintf("javaAdapter is bound to method %q, not the callable itself", adapter.Method)
	}
	allowedKeys := adapter.Keys
	if len(allowedKeys) == 0 {
		allowedKeys = shape.DestructuredKeys
	}
	if unknown := firstUnknownKey(kwargs, allowedKeys); unknown != "" {
		return fmt.Sprintf("keyword %q is not in javaAdapter keys %s", unknown, runtimeRefSortedList(allowedKeys))
	}
	return "javaAdapter is incompatible with keyword proxy call"
}

func firstUnknownKey(kwargs map[string]interface{}, allowedKeys []string) string {
	if len(allowedKeys) == 0 {
		return ""
	}
	allowed := make(map[string]bool, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[key] = true
	}
	keys := make([]string, 0, len(kwargs))
	for key := range kwargs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if !allowed[key] {
			return key
		}
	}
	return ""
}

func runtimeRefAcceptsJSOptionsKwargs(ref RuntimeRef, kwargs map[string]interface{}) bool {
	shape := ref.CallableShape
	if shape == nil || !shape.AcceptsOptionsObject {
		return false
	}
	if len(shape.DestructuredKeys) == 0 {
		return true
	}
	allowed := make(map[string]bool, len(shape.DestructuredKeys))
	for _, key := range shape.DestructuredKeys {
		allowed[key] = true
	}
	for key := range kwargs {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func runtimeRefAcceptsJavaKwargs(ref RuntimeRef, method string, kwargs map[string]interface{}) bool {
	shape := ref.CallableShape
	if shape == nil || shape.JavaAdapter == nil {
		return false
	}
	switch shape.JavaAdapter.Kind {
	case "map", "record", "builder":
	default:
		return false
	}
	if method != "" && shape.JavaAdapter.Method != method {
		return false
	}
	if method == "" && shape.JavaAdapter.Method != "" {
		return false
	}
	allowedKeys := shape.JavaAdapter.Keys
	if len(allowedKeys) == 0 {
		allowedKeys = shape.DestructuredKeys
	}
	if len(allowedKeys) == 0 {
		return true
	}
	allowed := make(map[string]bool, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[key] = true
	}
	for key := range kwargs {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func runtimeValueLiteral(rtName string, value interface{}) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	jsonText := string(b)
	switch rtName {
	case "javascript":
		return jsonText, nil
	case "python":
		return fmt.Sprintf("__import__('json').loads(%s)", jsonStringLiteral(jsonText)), nil
	case "ruby":
		return fmt.Sprintf("(begin; require 'json'; JSON.parse(%s); end)", jsonStringLiteral(jsonText)), nil
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.fromJson(%s)", jsonStringLiteral(jsonText)), nil
	default:
		return jsonText, nil
	}
}

func genericProperty(value interface{}, key string) (interface{}, bool, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return genericProperty(v.Value, key)
	case *RuntimeRef:
		if v == nil {
			return nil, false, nil
		}
		return genericProperty(v.Value, key)
	case *ResourceRef:
		return genericProperty(v.Value, key)
	case ResourceRef:
		return genericProperty(&v, key)
	case *TableRef:
		if field, ok := tableDescriptorProperty(v, key); ok {
			return field, true, nil
		}
		return genericProperty(v.Value, key)
	case TableRef:
		return genericProperty(&v, key)
	case *JobHandle:
		if field, ok := jobDescriptorProperty(v, key); ok {
			return field, true, nil
		}
		return genericProperty(v.Payload, key)
	case JobHandle:
		return genericProperty(&v, key)
	case *cSharedObjectProxy:
		return v.Get(key)
	case cSharedObjectProxy:
		return (&v).Get(key)
	case map[string]interface{}:
		out, ok := v[key]
		return out, ok, nil
	case map[string]string:
		out, ok := v[key]
		return out, ok, nil
	case map[string]int:
		out, ok := v[key]
		return out, ok, nil
	case map[string]int64:
		out, ok := v[key]
		return out, ok, nil
	case map[string]float64:
		out, ok := v[key]
		return out, ok, nil
	case map[string]bool:
		out, ok := v[key]
		return out, ok, nil
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		return genericProperty(rv.Elem().Interface(), key)
	}
	if rv.Kind() != reflect.Struct {
		return nil, false, nil
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if field.PkgPath != "" {
			continue
		}
		if field.Name == key || jsonFieldName(field.Tag.Get("json")) == key {
			return rv.Field(i).Interface(), true, nil
		}
	}
	return nil, false, nil
}

func genericCallable(value interface{}, key string) bool {
	switch v := value.(type) {
	case RuntimeRef:
		return genericCallable(v.Value, key)
	case *RuntimeRef:
		if v == nil {
			return false
		}
		return genericCallable(v.Value, key)
	case *ResourceRef:
		return genericCallable(v.Value, key)
	case ResourceRef:
		return genericCallable(&v, key)
	case *TableRef:
		return genericCallable(v.Value, key)
	case TableRef:
		return genericCallable(&v, key)
	case *JobHandle:
		return genericCallable(v.Payload, key)
	case JobHandle:
		return genericCallable(&v, key)
	case *cSharedObjectProxy:
		ok, err := v.Callable(key)
		return err == nil && ok
	case cSharedObjectProxy:
		ok, err := (&v).Callable(key)
		return err == nil && ok
	}

	if key == "" {
		rv := reflect.ValueOf(value)
		return rv.IsValid() && rv.Kind() == reflect.Func
	}
	if prop, ok, err := genericProperty(value, key); err == nil && ok {
		fn := reflect.ValueOf(prop)
		return fn.IsValid() && fn.Kind() == reflect.Func
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false
	}
	if rv.MethodByName(key).IsValid() {
		return true
	}
	if rv.Kind() != reflect.Pointer && rv.CanAddr() {
		return rv.Addr().MethodByName(key).IsValid()
	}
	return false
}

func genericZeroArgCallable(value interface{}, key string) bool {
	switch v := value.(type) {
	case RuntimeRef:
		return genericZeroArgCallable(v.Value, key)
	case *RuntimeRef:
		if v == nil {
			return false
		}
		return genericZeroArgCallable(v.Value, key)
	case *ResourceRef:
		return genericZeroArgCallable(v.Value, key)
	case ResourceRef:
		return genericZeroArgCallable(&v, key)
	case *TableRef:
		return genericZeroArgCallable(v.Value, key)
	case TableRef:
		return genericZeroArgCallable(&v, key)
	case *JobHandle:
		return genericZeroArgCallable(v.Payload, key)
	case JobHandle:
		return genericZeroArgCallable(&v, key)
	}

	if key == "" {
		rv := reflect.ValueOf(value)
		return rv.IsValid() && rv.Kind() == reflect.Func && rv.Type().NumIn() == 0
	}
	if prop, ok, err := genericProperty(value, key); err == nil && ok {
		fn := reflect.ValueOf(prop)
		return fn.IsValid() && fn.Kind() == reflect.Func && fn.Type().NumIn() == 0
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false
	}
	if method := rv.MethodByName(key); method.IsValid() {
		return method.Type().NumIn() == 0
	}
	if rv.Kind() != reflect.Pointer && rv.CanAddr() {
		if method := rv.Addr().MethodByName(key); method.IsValid() {
			return method.Type().NumIn() == 0
		}
	}
	return false
}

func genericIndex(value interface{}, key interface{}) (interface{}, bool, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return genericIndex(v.Value, key)
	case *RuntimeRef:
		if v == nil {
			return nil, false, nil
		}
		return genericIndex(v.Value, key)
	case *ResourceRef:
		return genericIndex(v.Value, key)
	case ResourceRef:
		return genericIndex(&v, key)
	case *TableRef:
		if keyStr, ok := key.(string); ok {
			if field, ok := tableDescriptorProperty(v, keyStr); ok {
				return field, true, nil
			}
		}
		if item, ok, err := tableBufferIndex(v, key); ok || err != nil {
			return item, ok, err
		}
		return genericIndex(v.Value, key)
	case TableRef:
		return genericIndex(&v, key)
	case *JobHandle:
		if keyStr, ok := key.(string); ok {
			if field, ok := jobDescriptorProperty(v, keyStr); ok {
				return field, true, nil
			}
		}
		return genericIndex(v.Payload, key)
	case JobHandle:
		return genericIndex(&v, key)
	case *cSharedObjectProxy:
		return v.Index(key)
	case cSharedObjectProxy:
		return (&v).Index(key)
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		mapKey, ok := convertReflectValue(key, rv.Type().Key())
		if !ok {
			return nil, false, nil
		}
		out := rv.MapIndex(mapKey)
		if !out.IsValid() {
			return nil, false, nil
		}
		return out.Interface(), true, nil
	case reflect.Slice, reflect.Array:
		idx, ok := numericIndex(key)
		if !ok || idx < 0 || idx >= rv.Len() {
			return nil, false, nil
		}
		return rv.Index(idx).Interface(), true, nil
	case reflect.String:
		idx, ok := numericIndex(key)
		if !ok || idx < 0 || idx >= rv.Len() {
			return nil, false, nil
		}
		return string(rv.String()[idx]), true, nil
	}
	return nil, false, nil
}

func genericSet(value interface{}, key string, next interface{}) (bool, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return genericSet(v.Value, key, next)
	case *RuntimeRef:
		if v == nil {
			return false, nil
		}
		return genericSet(v.Value, key, next)
	case *ResourceRef:
		return genericSet(v.Value, key, next)
	case ResourceRef:
		return genericSet(&v, key, next)
	case *TableRef:
		if key == "length" {
			return false, tableBufferResizeError(v, next)
		}
		return genericSet(v.Value, key, next)
	case TableRef:
		return genericSet(&v, key, next)
	case *JobHandle:
		return genericSet(v.Payload, key, next)
	case JobHandle:
		return genericSet(&v, key, next)
	case *cSharedObjectProxy:
		return v.Set(key, next)
	case cSharedObjectProxy:
		return (&v).Set(key, next)
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false, nil
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		mapKey, ok := convertReflectValue(key, rv.Type().Key())
		if !ok {
			return false, nil
		}
		mapValue, ok := convertReflectValue(next, rv.Type().Elem())
		if !ok {
			return false, fmt.Errorf("manifest HandleCall: cannot assign %T to %s", next, rv.Type().Elem())
		}
		rv.SetMapIndex(mapKey, mapValue)
		return true, nil
	case reflect.Slice, reflect.Array:
		idx, ok := numericIndex(key)
		if !ok || idx < 0 || idx >= rv.Len() {
			return false, nil
		}
		slot := rv.Index(idx)
		if !slot.CanSet() {
			return false, fmt.Errorf("manifest HandleCall: cannot assign to %s index %d", rv.Type(), idx)
		}
		slotValue, ok := convertReflectValue(next, slot.Type())
		if !ok {
			return false, fmt.Errorf("manifest HandleCall: cannot assign %T to %s", next, slot.Type())
		}
		slot.Set(slotValue)
		return true, nil
	case reflect.Struct:
		field := settableStructField(rv, key)
		if !field.IsValid() {
			return false, nil
		}
		fieldValue, ok := convertReflectValue(next, field.Type())
		if !ok {
			return false, fmt.Errorf("manifest HandleCall: cannot assign %T to %s", next, field.Type())
		}
		field.Set(fieldValue)
		return true, nil
	}
	return false, nil
}

func tableBufferResizeError(ref *TableRef, requested interface{}) error {
	if ref == nil {
		return fmt.Errorf("manifest HandleCall: cannot resize fixed-size table proxy requested=%v", requested)
	}
	name := tableBufferName(ref)
	if name == "" {
		return fmt.Errorf("manifest HandleCall: cannot resize fixed-size table proxy runtime=%s id=%d requested=%v", ref.Runtime, ref.ID, requested)
	}
	buf, err := arrow.GlobalStore().Get(name)
	if err != nil {
		return fmt.Errorf("manifest HandleCall: cannot resize fixed-size table proxy runtime=%s id=%d buffer=%q requested=%v: %w", ref.Runtime, ref.ID, name, requested, err)
	}
	length, _, lenErr := tableBufferLen(ref)
	if lenErr != nil {
		return fmt.Errorf("manifest HandleCall: cannot resize fixed-size table proxy runtime=%s id=%d buffer=%q dtype=%d format=%q shape=%v strides=%v offset=%d requested=%v: %w", ref.Runtime, ref.ID, name, buf.Dtype, buf.Format, tableBufferShape(ref, buf.Shape), tableBufferStrides(ref, buf.Strides), tableBufferOffset(ref), requested, lenErr)
	}
	return fmt.Errorf("manifest HandleCall: cannot resize fixed-size table proxy runtime=%s kind=table id=%d buffer=%q dtype=%d format=%q shape=%v strides=%v offset=%d length=%d requested=%v", ref.Runtime, ref.ID, name, buf.Dtype, buf.Format, tableBufferShape(ref, buf.Shape), tableBufferStrides(ref, buf.Strides), tableBufferOffset(ref), length, requested)
}

func genericLen(value interface{}) (int, bool, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return genericLen(v.Value)
	case *RuntimeRef:
		if v == nil {
			return 0, false, nil
		}
		return genericLen(v.Value)
	case *ResourceRef:
		return genericLen(v.Value)
	case ResourceRef:
		return genericLen(&v)
	case *TableRef:
		if length, ok, err := tableBufferLen(v); ok || err != nil {
			return length, ok, err
		}
		return genericLen(v.Value)
	case TableRef:
		return genericLen(&v)
	case *JobHandle:
		return genericLen(v.Payload)
	case JobHandle:
		return genericLen(&v)
	case *cSharedObjectProxy:
		return v.Len()
	case cSharedObjectProxy:
		return (&v).Len()
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return 0, false, nil
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return 0, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len(), true, nil
	default:
		return 0, false, nil
	}
}

func genericIter(value interface{}, mode string) ([]interface{}, bool, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return genericIter(v.Value, mode)
	case *RuntimeRef:
		if v == nil {
			return nil, false, nil
		}
		return genericIter(v.Value, mode)
	case *ResourceRef:
		return genericIter(v.Value, mode)
	case ResourceRef:
		return genericIter(&v, mode)
	case *TableRef:
		if values, ok, err := tableBufferIter(v, mode); ok || err != nil {
			return values, ok, err
		}
		return genericIter(v.Value, mode)
	case TableRef:
		return genericIter(&v, mode)
	case *JobHandle:
		return genericIter(v.Payload, mode)
	case JobHandle:
		return genericIter(&v, mode)
	case *cSharedObjectProxy:
		return v.Iter(mode)
	case cSharedObjectProxy:
		return (&v).Iter(mode)
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		keys := rv.MapKeys()
		out := make([]interface{}, 0, len(keys))
		for _, key := range keys {
			switch mode {
			case "values":
				out = append(out, rv.MapIndex(key).Interface())
			case "items":
				out = append(out, []interface{}{key.Interface(), rv.MapIndex(key).Interface()})
			default:
				out = append(out, key.Interface())
			}
		}
		return out, true, nil
	case reflect.Slice, reflect.Array:
		out := make([]interface{}, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			switch mode {
			case "items":
				out = append(out, []interface{}{i, rv.Index(i).Interface()})
			case "keys":
				out = append(out, i)
			default:
				out = append(out, rv.Index(i).Interface())
			}
		}
		return out, true, nil
	case reflect.String:
		out := make([]interface{}, 0, len(rv.String()))
		for i, r := range rv.String() {
			switch mode {
			case "items":
				out = append(out, []interface{}{i, string(r)})
			case "keys":
				out = append(out, i)
			default:
				out = append(out, string(r))
			}
		}
		return out, true, nil
	default:
		return nil, false, nil
	}
}

func genericContains(value interface{}, key interface{}) (bool, bool, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return genericContains(v.Value, key)
	case *RuntimeRef:
		if v == nil {
			return false, false, nil
		}
		return genericContains(v.Value, key)
	case *ResourceRef:
		return genericContains(v.Value, key)
	case ResourceRef:
		return genericContains(&v, key)
	case *TableRef:
		if ok, found, err := tableBufferContains(v, key); ok || err != nil {
			return ok, found, err
		}
		return genericContains(v.Value, key)
	case TableRef:
		return genericContains(&v, key)
	case *JobHandle:
		return genericContains(v.Payload, key)
	case JobHandle:
		return genericContains(&v, key)
	case *cSharedObjectProxy:
		return v.Contains(key)
	case cSharedObjectProxy:
		return (&v).Contains(key)
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false, false, nil
	}
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		mapKey, ok := convertReflectValue(key, rv.Type().Key())
		if !ok {
			return true, false, nil
		}
		return true, rv.MapIndex(mapKey).IsValid(), nil
	case reflect.Slice, reflect.Array:
		if idx, ok := numericIndex(key); ok && idx >= 0 && idx < rv.Len() {
			return true, true, nil
		}
		for i := 0; i < rv.Len(); i++ {
			if reflect.DeepEqual(rv.Index(i).Interface(), key) {
				return true, true, nil
			}
		}
		return true, false, nil
	case reflect.String:
		keyStr, ok := key.(string)
		if !ok {
			return true, false, nil
		}
		return true, strings.Contains(rv.String(), keyStr), nil
	case reflect.Struct:
		keyStr, ok := key.(string)
		if !ok {
			return true, false, nil
		}
		_, found, err := genericProperty(value, keyStr)
		return true, found, err
	default:
		return false, false, nil
	}
}

func genericCall(value interface{}, key string, args []interface{}) (interface{}, bool, error) {
	switch v := value.(type) {
	case RuntimeRef:
		return genericCall(v.Value, key, args)
	case *RuntimeRef:
		if v == nil {
			return nil, false, nil
		}
		return genericCall(v.Value, key, args)
	case *ResourceRef:
		return genericCall(v.Value, key, args)
	case ResourceRef:
		return genericCall(&v, key, args)
	case *TableRef:
		return genericCall(v.Value, key, args)
	case TableRef:
		return genericCall(&v, key, args)
	case *JobHandle:
		return genericCall(v.Payload, key, args)
	case JobHandle:
		return genericCall(&v, key, args)
	case *cSharedObjectProxy:
		return v.Call(key, args)
	case cSharedObjectProxy:
		return (&v).Call(key, args)
	}

	if key == "" {
		return callReflectCallable(reflect.ValueOf(value), args)
	}
	if prop, ok, err := genericProperty(value, key); err != nil {
		return nil, false, err
	} else if ok {
		if result, called, err := callReflectCallable(reflect.ValueOf(prop), args); called || err != nil {
			return result, called, err
		}
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	method := rv.MethodByName(key)
	if !method.IsValid() && rv.Kind() != reflect.Pointer && rv.CanAddr() {
		method = rv.Addr().MethodByName(key)
	}
	return callReflectCallable(method, args)
}

func jsonFieldName(tag string) string {
	if tag == "" || tag == "-" {
		return ""
	}
	if idx := strings.IndexByte(tag, ','); idx >= 0 {
		return tag[:idx]
	}
	return tag
}

func resourceDescriptorProperty(ref *ResourceRef, key string) (interface{}, bool) {
	switch key {
	case "id":
		return uint64(ref.ID), true
	case "runtime":
		return ref.Runtime, true
	case "kind":
		return ref.Kind, true
	case "disposer":
		return ref.Disposer, true
	case "closed":
		return ref.Closed, true
	default:
		return nil, false
	}
}

func numericIndex(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		if int64(int(v)) == v {
			return int(v), true
		}
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
	case json.Number:
		if i, err := v.Int64(); err == nil && int64(int(i)) == i {
			return int(i), true
		}
	case string:
		i, err := strconv.Atoi(v)
		return i, err == nil
	}
	return 0, false
}

func settableStructField(rv reflect.Value, key string) reflect.Value {
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if field.PkgPath != "" {
			continue
		}
		if field.Name == key || jsonFieldName(field.Tag.Get("json")) == key {
			out := rv.Field(i)
			if out.CanSet() {
				return out
			}
			return reflect.Value{}
		}
	}
	return reflect.Value{}
}

func convertReflectValue(value interface{}, target reflect.Type) (reflect.Value, bool) {
	if value == nil {
		switch target.Kind() {
		case reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.Func:
			return reflect.Zero(target), true
		default:
			return reflect.Value{}, false
		}
	}
	rv := reflect.ValueOf(value)
	if rv.Type().AssignableTo(target) {
		return rv, true
	}
	if rv.Type().ConvertibleTo(target) {
		return rv.Convert(target), true
	}
	switch target.Kind() {
	case reflect.Interface:
		if rv.Type().Implements(target) {
			return rv, true
		}
	case reflect.String:
		if s, ok := value.(string); ok {
			return reflect.ValueOf(s), true
		}
	case reflect.Bool:
		if b, ok := value.(bool); ok {
			return reflect.ValueOf(b).Convert(target), true
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n, ok := numericFloat(value); ok {
			i := int64(n)
			if n == float64(i) {
				out := reflect.New(target).Elem()
				if out.OverflowInt(i) {
					return reflect.Value{}, false
				}
				out.SetInt(i)
				return out, true
			}
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if n, ok := numericFloat(value); ok {
			u := uint64(n)
			if n == float64(u) {
				out := reflect.New(target).Elem()
				if out.OverflowUint(u) {
					return reflect.Value{}, false
				}
				out.SetUint(u)
				return out, true
			}
		}
	case reflect.Float32, reflect.Float64:
		if n, ok := numericFloat(value); ok {
			out := reflect.New(target).Elem()
			if out.OverflowFloat(n) {
				return reflect.Value{}, false
			}
			out.SetFloat(n)
			return out, true
		}
	}
	return reflect.Value{}, false
}

func numericFloat(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	}
	return 0, false
}

func callReflectCallable(fn reflect.Value, args []interface{}) (interface{}, bool, error) {
	if !fn.IsValid() {
		return nil, false, nil
	}
	if fn.Kind() != reflect.Func {
		return nil, false, nil
	}
	fnType := fn.Type()
	if !fnType.IsVariadic() && len(args) != fnType.NumIn() {
		return nil, false, fmt.Errorf("manifest HandleCall: callable expects %d args, got %d", fnType.NumIn(), len(args))
	}
	if fnType.IsVariadic() && len(args) < fnType.NumIn()-1 {
		return nil, false, fmt.Errorf("manifest HandleCall: callable expects at least %d args, got %d", fnType.NumIn()-1, len(args))
	}
	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		target := fnType.In(i)
		if fnType.IsVariadic() && i >= fnType.NumIn()-1 {
			target = target.Elem()
		}
		converted, ok := convertReflectValue(arg, target)
		if !ok {
			return nil, false, fmt.Errorf("manifest HandleCall: cannot pass %T as %s", arg, target)
		}
		in[i] = converted
	}
	out := fn.Call(in)
	if len(out) == 0 {
		return nil, true, nil
	}
	if last := out[len(out)-1]; last.IsValid() && last.Type().Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		if !last.IsNil() {
			return nil, true, last.Interface().(error)
		}
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return nil, true, nil
	}
	if len(out) == 1 {
		return out[0].Interface(), true, nil
	}
	values := make([]interface{}, len(out))
	for i, val := range out {
		values[i] = val.Interface()
	}
	return values, true, nil
}

func tableDescriptorProperty(ref *TableRef, key string) (interface{}, bool) {
	switch key {
	case "id":
		return uint64(ref.ID), true
	case "runtime":
		return ref.Runtime, true
	case "format":
		return ref.Format, true
	case "ownership":
		return ref.Ownership, true
	case "release":
		return ref.Release, true
	case "metadata":
		return ref.Metadata, true
	case "buffer":
		if name := tableBufferName(ref); name != "" {
			return name, true
		}
		return nil, true
	case "released":
		return ref.Released, true
	default:
		return nil, false
	}
}

func tableBufferName(ref *TableRef) string {
	if ref == nil {
		return ""
	}
	if ref.Metadata != nil && ref.Metadata.Buffer != "" {
		return ref.Metadata.Buffer
	}
	if name, ok := ref.Value.(string); ok {
		return name
	}
	return ""
}

func tableBufferLen(ref *TableRef) (int, bool, error) {
	name := tableBufferName(ref)
	if name == "" {
		return 0, false, nil
	}
	buf, err := arrow.GlobalStore().Get(name)
	if err != nil {
		return 0, false, err
	}
	elemSize, ok := tableBufferElementSize(buf.Dtype)
	if !ok {
		return 0, false, nil
	}
	layout, err := tableBufferLogicalLayout(ref, buf.Dtype, buf.Len, buf.Shape, buf.Strides)
	if err != nil {
		return 0, false, err
	}
	if layout.elemSize != elemSize {
		return 0, false, fmt.Errorf("manifest HandleCall: table buffer %q dtype metadata changed during layout", name)
	}
	return layout.length, true, nil
}

func tableBufferIndex(ref *TableRef, key interface{}) (interface{}, bool, error) {
	idx, ok := numericIndex(key)
	if !ok {
		return nil, false, nil
	}
	lease, data, ok, err := borrowTableBufferBytes(ref)
	if err != nil || !ok {
		return nil, ok, err
	}
	defer lease.Release()
	layout, err := tableBufferLogicalLayout(ref, lease.Dtype, len(data), lease.Metadata.Shape, lease.Metadata.Strides)
	if err != nil {
		return nil, false, err
	}
	if idx < 0 || idx >= layout.length {
		return nil, false, nil
	}
	if !layout.shaped {
		isNull, err := tableBufferValueIsNull(lease, int64(idx))
		if err != nil {
			return nil, false, err
		}
		if isNull {
			return nil, true, nil
		}
		offset := layout.offset + int64(idx)*int64(layout.elemSize)
		if len(layout.strides) == 1 {
			offset = layout.offset + int64(idx)*layout.strides[0]
		}
		value, ok, err := tableBufferValueAtByteOffset(lease.Dtype, data, offset)
		if err != nil || !ok {
			return nil, ok, err
		}
		return value, true, nil
	}
	value, ok, err := tableBufferSliceAt(lease.Dtype, data, layout.offset+int64(idx)*layout.strides[0], layout.shape[1:], layout.strides[1:])
	if err != nil || !ok {
		return nil, ok, err
	}
	return value, true, nil
}

func tableBufferIter(ref *TableRef, mode string) ([]interface{}, bool, error) {
	lease, data, ok, err := borrowTableBufferBytes(ref)
	if err != nil || !ok {
		return nil, ok, err
	}
	defer lease.Release()
	elemSize, ok := tableBufferElementSize(lease.Dtype)
	if !ok {
		return nil, false, nil
	}
	layout, err := tableBufferLogicalLayout(ref, lease.Dtype, len(data), lease.Metadata.Shape, lease.Metadata.Strides)
	if err != nil {
		return nil, false, err
	}
	if layout.elemSize != elemSize {
		return nil, false, fmt.Errorf("manifest HandleCall: table buffer %q dtype metadata changed during layout", tableBufferName(ref))
	}
	out := make([]interface{}, 0, layout.length)
	for i := 0; i < layout.length; i++ {
		var value interface{}
		if layout.shaped {
			value, ok, err = tableBufferSliceAt(lease.Dtype, data, layout.offset+int64(i)*layout.strides[0], layout.shape[1:], layout.strides[1:])
		} else {
			isNull, err := tableBufferValueIsNull(lease, int64(i))
			if err != nil {
				return nil, false, err
			}
			if isNull {
				value = nil
				ok = true
			} else {
				offset := layout.offset + int64(i)*int64(layout.elemSize)
				if len(layout.strides) == 1 {
					offset = layout.offset + int64(i)*layout.strides[0]
				}
				value, ok, err = tableBufferValueAtByteOffset(lease.Dtype, data, offset)
			}
		}
		if err != nil {
			return nil, false, err
		}
		if !ok {
			return nil, false, nil
		}
		switch mode {
		case "items":
			out = append(out, []interface{}{i, value})
		case "keys":
			out = append(out, i)
		default:
			out = append(out, value)
		}
	}
	return out, true, nil
}

type tableBufferRow []interface{}

type tableBufferLayout struct {
	elemSize int
	flatLen  int
	length   int
	shape    []int64
	strides  []int64
	offset   int64
	shaped   bool
}

func tableBufferLogicalLayout(ref *TableRef, dtype int32, byteLen int, fallbackShape []int64, fallbackStrides []int64) (tableBufferLayout, error) {
	elemSize, ok := tableBufferElementSize(dtype)
	if !ok {
		return tableBufferLayout{}, nil
	}
	layout := tableBufferLayout{elemSize: elemSize}
	if elemSize == 0 {
		return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q has zero-sized dtype %d", tableBufferName(ref), dtype)
	}
	shape := tableBufferShape(ref, fallbackShape)
	strides := tableBufferStrides(ref, fallbackStrides)
	offset := tableBufferOffset(ref)
	if len(shape) == 0 {
		if byteLen%elemSize != 0 {
			return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q length %d is not aligned to dtype %d", tableBufferName(ref), byteLen, dtype)
		}
		layout.flatLen = byteLen / elemSize
		layout.length = layout.flatLen
		layout.offset = offset
		return layout, nil
	}
	total, ok := tableShapeProduct(shape)
	if !ok {
		return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q has invalid shape %v", tableBufferName(ref), shape)
	}
	if int64(int(total)) != total || int64(int(shape[0])) != shape[0] {
		return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q shape %v overflows host indexes", tableBufferName(ref), shape)
	}
	if len(strides) > 0 {
		if len(strides) != len(shape) {
			return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q shape %v has mismatched strides %v", tableBufferName(ref), shape, strides)
		}
		minOffset, maxOffset, ok := tableStridedBounds(shape, strides, elemSize)
		if !ok {
			return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q shape %v strides %v overflow host indexes", tableBufferName(ref), shape, strides)
		}
		if offset+minOffset < 0 || offset+maxOffset > int64(byteLen) {
			return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q shape %v strides %v offset %d require byte range [%d,%d) but buffer has %d", tableBufferName(ref), shape, strides, offset, offset+minOffset, offset+maxOffset, byteLen)
		}
		layout.flatLen = int(total)
		layout.length = int(shape[0])
		layout.shape = append([]int64(nil), shape...)
		layout.strides = append([]int64(nil), strides...)
		layout.offset = offset
		layout.shaped = len(shape) > 1
		return layout, nil
	}
	requiredBytes := total * int64(elemSize)
	if offset < 0 || offset+requiredBytes > int64(byteLen) {
		return tableBufferLayout{}, fmt.Errorf("manifest HandleCall: table buffer %q shape %v offset %d describes byte range [%d,%d) but buffer has %d", tableBufferName(ref), shape, offset, offset, offset+requiredBytes, byteLen)
	}
	layout.flatLen = int(total)
	layout.length = int(shape[0])
	layout.shape = append([]int64(nil), shape...)
	layout.strides = defaultContiguousStrides(shape, elemSize)
	layout.offset = offset
	layout.shaped = len(shape) > 1
	return layout, nil
}

func tableBufferShape(ref *TableRef, fallback []int64) []int64 {
	if ref != nil && ref.Metadata != nil && len(ref.Metadata.Shape) > 0 {
		return ref.Metadata.Shape
	}
	return fallback
}

func tableBufferStrides(ref *TableRef, fallback []int64) []int64 {
	if ref != nil && ref.Metadata != nil && len(ref.Metadata.Strides) > 0 {
		return ref.Metadata.Strides
	}
	return fallback
}

func tableBufferOffset(ref *TableRef) int64 {
	if ref != nil && ref.Metadata != nil {
		return ref.Metadata.Offset
	}
	return 0
}

func tableShapeProduct(shape []int64) (int64, bool) {
	product := int64(1)
	for _, dim := range shape {
		if dim < 0 {
			return 0, false
		}
		if dim == 0 {
			return 0, true
		}
		if product > math.MaxInt64/dim {
			return 0, false
		}
		product *= dim
	}
	return product, true
}

func defaultContiguousStrides(shape []int64, elemSize int) []int64 {
	if len(shape) == 0 {
		return nil
	}
	strides := make([]int64, len(shape))
	stride := int64(elemSize)
	for i := len(shape) - 1; i >= 0; i-- {
		strides[i] = stride
		if shape[i] == 0 {
			stride = 0
			continue
		}
		stride *= shape[i]
	}
	return strides
}

func tableStridedBounds(shape []int64, strides []int64, elemSize int) (int64, int64, bool) {
	for _, dim := range shape {
		if dim < 0 {
			return 0, 0, false
		}
		if dim == 0 {
			return 0, 0, true
		}
	}
	minOffset := int64(0)
	maxOffset := int64(elemSize)
	for i, dim := range shape {
		stride := strides[i]
		if stride == 0 {
			return 0, 0, false
		}
		steps := dim - 1
		if steps <= 0 {
			continue
		}
		if stride > 0 {
			if stride > (math.MaxInt64-maxOffset)/steps {
				return 0, 0, false
			}
			maxOffset += steps * stride
		} else {
			if stride < math.MinInt64/steps {
				return 0, 0, false
			}
			delta := steps * stride
			if minOffset < math.MinInt64-delta {
				return 0, 0, false
			}
			minOffset += delta
		}
	}
	if maxOffset < minOffset {
		return 0, 0, false
	}
	return minOffset, maxOffset, true
}

func tableBufferSliceAt(dtype int32, data []byte, baseOffset int64, shape []int64, strides []int64) (interface{}, bool, error) {
	if len(shape) == 0 {
		return tableBufferValueAtByteOffset(dtype, data, baseOffset)
	}
	if len(strides) != len(shape) || int64(int(shape[0])) != shape[0] {
		return nil, false, fmt.Errorf("manifest HandleCall: table buffer slice shape %v overflows host indexes", shape)
	}
	out := make(tableBufferRow, 0, int(shape[0]))
	for i := 0; i < int(shape[0]); i++ {
		value, found, err := tableBufferSliceAt(dtype, data, baseOffset+int64(i)*strides[0], shape[1:], strides[1:])
		if err != nil {
			return nil, false, err
		}
		if !found {
			return nil, false, nil
		}
		out = append(out, value)
	}
	return out, true, nil
}

func tableBufferElementSize(dtype int32) (int, bool) {
	switch dtype {
	case arrow.DtypeBytes, arrow.DtypeUTF8, arrow.DtypeI8, arrow.DtypeU8:
		return 1, true
	case arrow.DtypeI16, arrow.DtypeU16:
		return 2, true
	case arrow.DtypeI32, arrow.DtypeF32:
		return 4, true
	case arrow.DtypeU32:
		return 4, true
	case arrow.DtypeI64, arrow.DtypeU64, arrow.DtypeF64:
		return 8, true
	default:
		return 0, false
	}
}

func tableBufferValueAt(dtype int32, data []byte, idx int) (interface{}, bool, error) {
	elemSize, ok := tableBufferElementSize(dtype)
	if !ok {
		return nil, false, nil
	}
	if elemSize == 0 || len(data)%elemSize != 0 {
		return nil, false, fmt.Errorf("manifest HandleCall: table buffer length %d is not aligned to dtype %d", len(data), dtype)
	}
	count := len(data) / elemSize
	if idx < 0 || idx >= count {
		return nil, false, nil
	}
	offset := idx * elemSize
	return tableBufferValueAtByteOffset(dtype, data, int64(offset))
}

func tableBufferValueAtByteOffset(dtype int32, data []byte, offset int64) (interface{}, bool, error) {
	elemSize, ok := tableBufferElementSize(dtype)
	if !ok {
		return nil, false, nil
	}
	if offset < 0 || offset > int64(len(data)-elemSize) {
		return nil, false, nil
	}
	switch dtype {
	case arrow.DtypeBytes, arrow.DtypeUTF8:
		return int(data[offset]), true, nil
	case arrow.DtypeI8:
		return int8(data[offset]), true, nil
	case arrow.DtypeU8:
		return uint8(data[offset]), true, nil
	case arrow.DtypeI16:
		return int16(binary.LittleEndian.Uint16(data[offset : offset+2])), true, nil
	case arrow.DtypeU16:
		return uint16(binary.LittleEndian.Uint16(data[offset : offset+2])), true, nil
	case arrow.DtypeI32:
		return int32(binary.LittleEndian.Uint32(data[offset : offset+4])), true, nil
	case arrow.DtypeU32:
		return uint32(binary.LittleEndian.Uint32(data[offset : offset+4])), true, nil
	case arrow.DtypeI64:
		return int64(binary.LittleEndian.Uint64(data[offset : offset+8])), true, nil
	case arrow.DtypeU64:
		return binary.LittleEndian.Uint64(data[offset : offset+8]), true, nil
	case arrow.DtypeF32:
		return float32(math.Float32frombits(binary.LittleEndian.Uint32(data[offset : offset+4]))), true, nil
	case arrow.DtypeF64:
		return math.Float64frombits(binary.LittleEndian.Uint64(data[offset : offset+8])), true, nil
	default:
		return nil, false, nil
	}
}

func tableBufferValueIsNull(lease *arrow.BorrowedBuffer, logicalIndex int64) (bool, error) {
	if lease == nil || lease.Metadata.NullCount <= 0 {
		return false, nil
	}
	if logicalIndex < 0 {
		return false, fmt.Errorf("manifest HandleCall: table buffer has negative logical index %d", logicalIndex)
	}
	if lease.Validity == nil || lease.ValidityLen <= 0 {
		return false, fmt.Errorf("manifest HandleCall: table buffer has null_count=%d but no validity bitmap", lease.Metadata.NullCount)
	}
	bit := lease.Metadata.ValidityBitOffset + logicalIndex
	if bit < 0 {
		return false, fmt.Errorf("manifest HandleCall: table buffer has negative validity bit offset %d", bit)
	}
	byteIndex := bit / 8
	if byteIndex >= lease.ValidityLen {
		return false, fmt.Errorf("manifest HandleCall: table buffer validity bit %d exceeds bitmap length %d", bit, lease.ValidityLen)
	}
	bitmap := unsafe.Slice((*byte)(lease.Validity), int(lease.ValidityLen))
	mask := byte(1 << uint(bit%8))
	return bitmap[byteIndex]&mask == 0, nil
}

func tableBufferContains(ref *TableRef, key interface{}) (bool, bool, error) {
	if idx, ok := numericIndex(key); ok {
		length, found, err := tableBufferLen(ref)
		if err != nil || !found {
			return found, false, err
		}
		return true, idx >= 0 && idx < length, nil
	}
	return false, false, nil
}

func borrowTableBufferBytes(ref *TableRef) (*arrow.BorrowedBuffer, []byte, bool, error) {
	name := tableBufferName(ref)
	if name == "" {
		return nil, nil, false, nil
	}
	lease, err := arrow.GlobalStore().Borrow(name)
	if err != nil {
		return nil, nil, false, err
	}
	if lease.Len <= 0 {
		return lease, nil, true, nil
	}
	if int64(int(lease.Len)) != lease.Len {
		lease.Release()
		return nil, nil, false, fmt.Errorf("manifest HandleCall: table buffer %q length %d overflows int", name, lease.Len)
	}
	if lease.Data == nil {
		lease.Release()
		return nil, nil, false, fmt.Errorf("manifest HandleCall: table buffer %q has no data pointer", name)
	}
	return lease, unsafe.Slice((*byte)(lease.Data), int(lease.Len)), true, nil
}

func jobDescriptorProperty(ref *JobHandle, key string) (interface{}, bool) {
	switch key {
	case "id":
		return ref.ID, true
	case "runtime":
		return ref.Runtime, true
	case "kind":
		return ref.Kind, true
	case "done":
		return ref.Done, true
	case "cancelled":
		return ref.Cancelled, true
	case "cancelReason":
		return ref.CancelReason, true
	case "payload":
		return ref.Payload, true
	case "result":
		return ref.Result, true
	default:
		return nil, false
	}
}

func bridgeHandleID(val interface{}) (handles.ID, error) {
	switch v := val.(type) {
	case float64:
		if v <= 0 || v != float64(uint64(v)) {
			return 0, fmt.Errorf("manifest HandleCall: invalid handle id %v", v)
		}
		return handles.ID(uint64(v)), nil
	case int:
		if v <= 0 {
			return 0, fmt.Errorf("manifest HandleCall: invalid handle id %v", v)
		}
		return handles.ID(v), nil
	case uint64:
		if v == 0 {
			return 0, fmt.Errorf("manifest HandleCall: invalid handle id %v", v)
		}
		return handles.ID(v), nil
	case handles.ID:
		if v == 0 {
			return 0, fmt.Errorf("manifest HandleCall: invalid handle id %v", v)
		}
		return v, nil
	case string:
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil || n == 0 {
			return 0, fmt.Errorf("manifest HandleCall: invalid handle id %q", v)
		}
		return handles.ID(n), nil
	default:
		return 0, fmt.Errorf("manifest HandleCall: invalid handle id %v", val)
	}
}

func bridgeMarkerHandleID(value map[string]interface{}) (handles.ID, bool) {
	if value["__omnivm_resource__"] != true &&
		value["__omnivm_table__"] != true &&
		value["__omnivm_job__"] != true &&
		value["__omnivm_stream__"] != true &&
		value["__omnivm_channel__"] != true {
		return 0, false
	}
	id, err := bridgeHandleID(value["id"])
	if err != nil {
		return 0, false
	}
	return id, true
}

// callGoFuncFromBridge invokes a Go plugin function from a bridge call.
// Includes panic recovery since plugin code may have type assertion failures.
func (e *Executor) callGoFuncFromBridge(name string, fn interface{}, args []interface{}) (result string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("go function %q panicked: %v", name, r)
		}
	}()

	// Normalize JSON number args and materialize manifest handles for Go.
	normalizedArgs := e.normalizeGoArgs(args)

	// Try func() interface{} (no args)
	if f, ok := fn.(func() interface{}); ok {
		res := f()
		return e.marshalGoBridgeResult(res)
	}

	// Try func(interface{}) interface{} (single arg)
	if f, ok := fn.(func(interface{}) interface{}); ok {
		var arg interface{}
		if len(normalizedArgs) > 0 {
			arg = normalizedArgs[0]
		}
		res := f(arg)
		return e.marshalGoBridgeResult(res)
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
		return e.marshalGoBridgeResult(res)
	}

	// Try func([]interface{}) (interface{}, error)
	if f, ok := fn.(func([]interface{}) (interface{}, error)); ok {
		res, ferr := f(normalizedArgs)
		if ferr != nil {
			return "", ferr
		}
		return e.marshalGoBridgeResult(res)
	}

	// Try func([]interface{}) interface{}
	if f, ok := fn.(func([]interface{}) interface{}); ok {
		res := f(normalizedArgs)
		return e.marshalGoBridgeResult(res)
	}

	return "", fmt.Errorf("go function %q has unsupported signature", name)
}

func (e *Executor) marshalGoBridgeResult(value interface{}) (string, error) {
	if jsonVal, ok, err := e.localStreamCaptureJSON(value, "go"); ok || err != nil {
		if err != nil {
			return "", err
		}
		var descriptor interface{}
		if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
			return "", err
		}
		if value, ok := descriptor.(map[string]interface{}); ok {
			if err := e.escapeBridgeReturnHandle(value); err != nil {
				return "", err
			}
		}
		return marshalResult(descriptor)
	}
	normalized, err := e.bridgeGoReturnValue(value)
	if err != nil {
		return "", err
	}
	return marshalResult(normalized)
}

func (e *Executor) bridgeGoReturnValue(value interface{}) (interface{}, error) {
	if ref, ok, err := e.bridgeReturnBulkTableValue(value); ok || err != nil {
		if err != nil {
			return nil, err
		}
		return transferTableProxyValue(ref), nil
	}
	if ref, ok, err := e.autoResourceRefForCapture(value); ok || err != nil {
		if err != nil {
			return nil, err
		}
		if ref.ID != 0 {
			if err := e.ensureHandleTable().Escape(ref.ID); err != nil {
				return nil, err
			}
		}
		e.addBoundaryStat(func(stats *BoundaryStats) {
			stats.ResourceProxyCaptures++
		})
		return transferResourceProxyValue(ref), nil
	}
	return e.bridgeReturnValue(value)
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
		if runtimeRefNeedsProxy(v) {
			return v
		}
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
	switch v := val.(type) {
	case *ResourceRef:
		val = resourceProxyValue(v)
	case ResourceRef:
		val = resourceProxyValue(&v)
	case *TableRef:
		val = tableProxyValue(v)
	case TableRef:
		val = tableProxyValue(&v)
	case *JobHandle:
		val = jobProxyValue(v)
	case JobHandle:
		val = jobProxyValue(&v)
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

	return "", fmt.Errorf("bridge result value %T is not JSON-marshalable; boundary classification must produce a primitive, descriptor, table, stream, or proxy", val)
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
