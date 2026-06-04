package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/handles"
)

type captureInjection struct {
	setup            string
	javaCaptureNames []string
	err              error
}

func (e *Executor) findRuntimeGlobalCapture(name, targetRuntime string) (RuntimeRef, bool, error) {
	for _, rtName := range runtimeCaptureProbeOrder(targetRuntime) {
		if _, ok := e.runtimes[rtName]; !ok {
			continue
		}
		exists, err := e.runtimeGlobalBindingExists(rtName, name)
		if err != nil || !exists {
			continue
		}
		ref, _, err := e.boundRuntimeRefSnapshot(rtName, name)
		if err != nil {
			return RuntimeRef{}, false, err
		}
		return ref, true, nil
	}
	return RuntimeRef{}, false, nil
}

func runtimeCaptureProbeOrder(targetRuntime string) []string {
	all := []string{"python", "javascript", "ruby", "java"}
	out := make([]string, 0, len(all))
	if targetRuntime != "" {
		out = append(out, targetRuntime)
	}
	for _, rtName := range all {
		if rtName != targetRuntime {
			out = append(out, rtName)
		}
	}
	return out
}

func (e *Executor) runtimeGlobalBindingExists(rtName, name string) (bool, error) {
	rt, ok := e.runtimes[rtName]
	if !ok {
		return false, nil
	}
	expr, ok := runtimeGlobalBindingExistsExpr(rtName, name)
	if !ok {
		return false, nil
	}
	result := rt.Eval(expr)
	if result.Err != nil {
		return false, nil
	}
	value, err := decodeRuntimeValue(rtName, result)
	if err != nil {
		return false, nil
	}
	exists, _ := value.(bool)
	return exists, nil
}

func runtimeGlobalBindingExistsExpr(rtName, name string) (string, bool) {
	switch rtName {
	case "javascript":
		return fmt.Sprintf(`JSON.stringify(Object.prototype.hasOwnProperty.call(globalThis, %s))`, strconv.Quote(name)), true
	case "python":
		return fmt.Sprintf(`__import__("json").dumps(%s in globals())`, strconv.Quote(name)), true
	case "ruby":
		return fmt.Sprintf(`begin; require 'json'; JSON.generate(defined?($%s) != nil || (($omnivm_bindings ||= {}).key?(%s))); end`, name, strconv.Quote(name)), isRubyIdentifier(name) && !rubyReserved[name]
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.toJson(omnivm.OmniVM.getCapture(\"%s\") != null)", escapeJavaString(name)), true
	default:
		return "", false
	}
}

func (injection captureInjection) javaCleanupCode(excludeNames ...string) string {
	if len(injection.javaCaptureNames) == 0 {
		return ""
	}
	exclude := make(map[string]bool, len(excludeNames))
	for _, name := range excludeNames {
		if name != "" {
			exclude[name] = true
		}
	}
	var lines []string
	for _, name := range injection.javaCaptureNames {
		if exclude[name] {
			continue
		}
		lines = append(lines, fmt.Sprintf("omnivm.OmniVM.clearCapture(\"%s\");", escapeJavaString(name)))
	}
	return strings.Join(lines, "\n")
}

// drainChannel performs a non-blocking drain of a channel into a slice.
// Returns all currently buffered values without blocking.
// Handles closed channels safely by checking the ok flag.
func drainChannel(ch *ChanRef) []interface{} {
	result := make([]interface{}, 0)
	for {
		select {
		case v, ok := <-ch.ch:
			if !ok {
				return result
			}
			result = append(result, v)
		default:
			return result
		}
	}
}

// wrapWithCaptures generates runtime-specific code that injects capture values
// into the execution scope, then runs the user's code. Each runtime uses
// scope isolation to prevent captures from leaking as persistent globals.
//
// ImportRef values for the same runtime are skipped (module already in scope).
// ImportRef values for a different runtime are skipped (can't transfer modules).
func (e *Executor) wrapWithCaptures(rtName, code string, captures map[string]string) (string, error) {
	// Resolve capture values from the binding stack
	resolved := make(map[string]string) // varname → JSON string
	for varName, bindingName := range captures {
		val, ok := e.getBinding(bindingName)
		if !ok {
			ref, found, err := e.findRuntimeGlobalCapture(bindingName, rtName)
			if err != nil {
				return "", fmt.Errorf("capture %q: runtime global %q: %w", varName, bindingName, err)
			}
			if !found {
				return "", fmt.Errorf("capture %q: undefined binding %q", varName, bindingName)
			}
			if ref.Runtime == rtName {
				continue
			}
			jsonVal, err := e.resolveRuntimeRefCapture(bindingName, rtName, ref)
			if err != nil {
				return "", fmt.Errorf("capture %q: RuntimeRef: %w", varName, err)
			}
			resolved[varName] = jsonVal
			continue
		}

		if _, ok := val.(*SpawnHandle); ok {
			continue
		}

		// ImportRef: module is already in scope for the owning runtime
		if ref, ok := val.(ImportRef); ok {
			if ref.Runtime == rtName {
				continue
			}
			continue
		}

		// RuntimeRef: variable lives in a runtime's global scope
		if ref, ok := val.(RuntimeRef); ok {
			if ref.Runtime == rtName {
				// Same runtime and same symbol — variable already in scope.
				// Same runtime but hidden arg-ref/derived var still needs the
				// requested capture name to be bound for user code.
				if ref.VarName == bindingName {
					continue
				}
			}
			jsonVal, err := e.resolveRuntimeRefCapture(bindingName, rtName, ref)
			if err != nil {
				return "", fmt.Errorf("capture %q: RuntimeRef: %w", varName, err)
			}
			resolved[varName] = jsonVal
			continue
		}

		// Stream-like values carry lazy descriptors instead of draining to JSON.
		if jsonVal, ok, err := e.localStreamCaptureJSON(val, "go"); ok || err != nil {
			if err != nil {
				return "", fmt.Errorf("capture %q: stream: %w", varName, err)
			}
			resolved[varName] = jsonVal
			continue
		}

		jsonVal, err := e.marshalForCapture(val)
		if err != nil {
			return "", fmt.Errorf("capture %q: marshal: %w", varName, err)
		}
		resolved[varName] = jsonVal
	}

	// If all captures were skipped (e.g. all ImportRefs), return code as-is
	if len(resolved) == 0 {
		return code, nil
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.CaptureInjections++
	})

	switch rtName {
	case "python":
		return wrapPythonCaptures(code, resolved), nil
	case "javascript":
		return wrapJavaScriptCaptures(code, resolved), nil
	case "ruby":
		return wrapRubyCaptures(code, resolved), nil
	case "java":
		return wrapJavaCaptures(code, resolved), nil
	default:
		return "", fmt.Errorf("captures not supported for runtime %q", rtName)
	}
}

// wrapPythonCaptures wraps code in a scope where captures are local variables.
// Uses JSON.loads for deserialization. The code runs in its own locals dict.
func wrapPythonCaptures(code string, captures map[string]string) string {
	if len(captures) == 0 {
		return code
	}

	var lines []string
	lines = append(lines, "import json as __json")
	lines = append(lines, pythonCaptureMaterializer())

	// Build locals dict with captures
	var pairs []string
	for varName, jsonVal := range captures {
		pairs = append(pairs, fmt.Sprintf("'%s': __omnivm_materialize_capture(__json.loads('%s'))",
			escapePythonString(varName),
			escapePythonString(jsonVal)))
	}
	lines = append(lines, fmt.Sprintf("__captures = {%s}", strings.Join(pairs, ", ")))

	// Inject captures as local variables, then run the user's code.
	// We assign each capture into the global scope so eval() can see it.
	for varName := range captures {
		lines = append(lines, runtimeAssign("python", varName, fmt.Sprintf("__captures['%s']", escapePythonString(varName))))
	}

	lines = append(lines, code)
	return strings.Join(lines, "\n")
}

// wrapJavaScriptCaptures wraps code in an IIFE with captures as parameters.
func wrapJavaScriptCaptures(code string, captures map[string]string) string {
	if len(captures) == 0 {
		return code
	}

	var paramVals []string
	var assignments []string
	i := 0
	for varName, jsonVal := range captures {
		paramVals = append(paramVals, jsonVal)
		if isJSIdentifier(varName) && !isJSReservedWord(varName) {
			assignments = append(assignments, fmt.Sprintf("const %s = __omnivm_captures[%d];", varName, i))
		} else {
			assignments = append(assignments, fmt.Sprintf("globalThis[%s] = __omnivm_captures[%d];", strconv.Quote(varName), i))
		}
		i++
	}

	return fmt.Sprintf("%s\n(function() { const __omnivm_captures = [%s]; %s %s\n})()",
		jsChannelMaterializer(),
		strings.Join(materializeJSCaptures(paramVals), ", "),
		strings.Join(assignments, " "),
		code)
}

// wrapRubyCaptures wraps code with global variable assignments from JSON.
// Uses $globals so captures persist across Execute/Eval boundaries and
// are accessible in the user's code regardless of scope.
func wrapRubyCaptures(code string, captures map[string]string) string {
	if len(captures) == 0 {
		return code
	}

	var lines []string
	lines = append(lines, "require 'json'")
	lines = append(lines, rubyCaptureMaterializer())
	for varName, jsonVal := range captures {
		lines = append(lines, runtimeAssign("ruby", varName, fmt.Sprintf("__omnivm_materialize_capture(JSON.parse('%s'))", escapeRubyString(jsonVal))))
	}
	// Also assign to local aliases so user code can reference without $
	for varName := range captures {
		if isRubyIdentifier(varName) && !rubyReserved[varName] {
			lines = append(lines, fmt.Sprintf("%s = $%s", varName, varName))
		}
	}
	lines = append(lines, code)
	return strings.Join(lines, "\n")
}

// wrapJavaCaptures injects captures via OmniVM.setCapture() calls before the code.
// The Java runtime materializes descriptor captures into generic handle proxies.
func wrapJavaCaptures(code string, captures map[string]string) string {
	if len(captures) == 0 {
		return code
	}

	var body []string
	injection := javaCaptureInjection(captures)
	imports, bodyCode := splitJavaImports(code)
	body = append(body, imports...)
	body = append(body, injection.setup)
	body = append(body, "try {")
	body = append(body, bodyCode)
	body = append(body, "} finally {")
	body = append(body, injection.javaCleanupCode())
	body = append(body, "}")
	return strings.Join(body, "\n")
}

func splitJavaImports(code string) ([]string, string) {
	var imports []string
	var body []string
	for _, line := range strings.Split(code, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "import ") {
			imports = append(imports, line)
			continue
		}
		body = append(body, line)
	}
	return imports, strings.Join(body, "\n")
}

// autoInjectScope injects all serializable bindings from the current scope
// into the given runtime. Used for conditions without explicit captures.
func (e *Executor) autoInjectScope(rtName string) string {
	return e.autoInjectScopePlan(rtName).setup
}

func (e *Executor) autoInjectScopePlan(rtName string) captureInjection {
	return e.autoInjectScopePlanExcluding(rtName, nil)
}

func captureBindingExclusions(captures map[string]string) map[string]bool {
	if len(captures) == 0 {
		return nil
	}
	exclude := make(map[string]bool, len(captures))
	for _, bindingName := range captures {
		if bindingName != "" {
			exclude[bindingName] = true
		}
	}
	return exclude
}

func (e *Executor) autoInjectScopePlanExcluding(rtName string, exclude map[string]bool) captureInjection {
	resolved := make(map[string]string)
	// Walk the scope stack top-down, collecting serializable bindings
	for i := len(e.scopes) - 1; i >= 0; i-- {
		for varName, val := range e.scopes[i] {
			if exclude[varName] {
				continue
			}
			if _, already := resolved[varName]; already {
				continue // shadowed by higher scope
			}
			if _, ok := val.(*SpawnHandle); ok {
				continue
			}
			if _, ok := val.(ImportRef); ok {
				continue
			}
			if ref, ok := val.(RuntimeRef); ok {
				if ref.Runtime == rtName {
					continue // already in scope
				}
				jsonVal, err := e.resolveRuntimeRefCapture(varName, rtName, ref)
				if err != nil {
					continue
				}
				resolved[varName] = jsonVal
				continue
			}
			if jsonVal, ok, err := e.localStreamCaptureJSON(val, "go"); ok || err != nil {
				if err != nil {
					continue
				}
				resolved[varName] = jsonVal
				continue
			}
			jsonVal, err := e.marshalForCapture(val)
			if err != nil {
				continue
			}
			resolved[varName] = jsonVal
		}
	}
	if len(resolved) == 0 {
		return captureInjection{}
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.CaptureInjections++
	})
	switch rtName {
	case "python":
		return captureInjection{setup: injectPythonCaptures(resolved)}
	case "javascript":
		return captureInjection{setup: injectJSCaptures(resolved)}
	case "ruby":
		return captureInjection{setup: injectRubyCaptures(resolved)}
	case "java":
		return javaCaptureInjection(resolved)
	default:
		return captureInjection{}
	}
}

// buildCaptureInjection generates capture setup code without appending the user's code.
// Used by opEval which needs captures and assignment as separate steps.
func (e *Executor) buildCaptureInjection(rtName string, captures map[string]string) string {
	return e.buildCaptureInjectionPlan(rtName, captures).setup
}

func (e *Executor) buildCaptureInjectionPlan(rtName string, captures map[string]string) captureInjection {
	resolved := make(map[string]string)
	for varName, bindingName := range captures {
		val, ok := e.getBinding(bindingName)
		if !ok {
			ref, found, err := e.findRuntimeGlobalCapture(bindingName, rtName)
			if err != nil {
				return captureInjection{err: fmt.Errorf("capture %q: runtime global %q: %w", varName, bindingName, err)}
			}
			if !found {
				return captureInjection{err: fmt.Errorf("capture %q: undefined binding %q", varName, bindingName)}
			}
			if ref.Runtime == rtName {
				continue
			}
			jsonVal, err := e.resolveRuntimeRefCapture(bindingName, rtName, ref)
			if err != nil {
				return captureInjection{err: fmt.Errorf("capture %q: RuntimeRef: %w", varName, err)}
			}
			resolved[varName] = jsonVal
			continue
		}
		if _, ok := val.(*SpawnHandle); ok {
			continue
		}
		if _, ok := val.(ImportRef); ok {
			continue
		}
		if ref, ok := val.(RuntimeRef); ok {
			if ref.Runtime == rtName {
				if ref.VarName == bindingName {
					continue
				}
			}
			jsonVal, err := e.resolveRuntimeRefCapture(bindingName, rtName, ref)
			if err != nil {
				return captureInjection{err: fmt.Errorf("capture %q: RuntimeRef: %w", varName, err)}
			}
			resolved[varName] = jsonVal
			continue
		}
		if jsonVal, ok, err := e.localStreamCaptureJSON(val, "go"); ok || err != nil {
			if err != nil {
				return captureInjection{err: fmt.Errorf("capture %q: stream: %w", varName, err)}
			}
			resolved[varName] = jsonVal
			continue
		}
		jsonVal, err := e.marshalForCapture(val)
		if err != nil {
			return captureInjection{err: fmt.Errorf("capture %q: marshal: %w", varName, err)}
		}
		resolved[varName] = jsonVal
	}

	if len(resolved) == 0 {
		return captureInjection{}
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.CaptureInjections++
	})

	switch rtName {
	case "python":
		return captureInjection{setup: injectPythonCaptures(resolved)}
	case "javascript":
		return captureInjection{setup: injectJSCaptures(resolved)}
	case "ruby":
		return captureInjection{setup: injectRubyCaptures(resolved)}
	case "java":
		return javaCaptureInjection(resolved)
	default:
		return captureInjection{}
	}
}

// injectPythonCaptures generates Python code to set capture variables (no user code).
func injectPythonCaptures(captures map[string]string) string {
	var lines []string
	lines = append(lines, "import json as __json")
	lines = append(lines, pythonCaptureMaterializer())
	for varName, jsonVal := range captures {
		lines = append(lines, runtimeAssign("python", varName, fmt.Sprintf("__omnivm_materialize_capture(__json.loads('%s'))", escapePythonString(jsonVal))))
	}
	return strings.Join(lines, "\n")
}

func pythonCaptureMaterializer() string {
	return `__omnivm_arg_refs = globals().setdefault("__omnivm_arg_refs", {})
__omnivm_arg_ref_counter = globals().setdefault("__omnivm_arg_ref_counter", 0)
import weakref as __omnivm_weakref
import collections.abc as __omnivm_collections_abc
__omnivm_proxy_cache = globals().setdefault("__omnivm_proxy_cache", __omnivm_weakref.WeakValueDictionary())

class _OmniVMBridgeMissing(Exception):
    pass

def _omnivm_is_missing_bridge_error(exc):
    text = str(exc)
    return (
        " has no property " in text
        or " has no index " in text
        or " has no length" in text
        or " is not iterable" in text
        or " does not support contains" in text
        or " has no writable property " in text
    )

def __omnivm_bridge_module():
    caller = globals().get("omnivm")
    if caller is not None and hasattr(caller, "call"):
        return caller
    try:
        import omnivm as __omnivm_mod
        if hasattr(__omnivm_mod, "call"):
            globals()["omnivm"] = __omnivm_mod
            return __omnivm_mod
    except Exception:
        pass
    return None

def __omnivm_release_handle_id(handle_id):
    try:
        caller = __omnivm_bridge_module()
        if caller is not None and hasattr(caller, "call"):
            import json as __j
            caller.call("__manifest", __j.dumps({"op": "handle_release_finalizer", "id": handle_id}))
    except Exception:
        pass

def __omnivm_retain_handle_id(handle_id):
    try:
        caller = __omnivm_bridge_module()
        if caller is not None and hasattr(caller, "call"):
            import json as __j
            raw = caller.call("__manifest", __j.dumps({"op": "handle_retain", "id": handle_id}))
            env = __j.loads(raw)
            return isinstance(env, dict) and env.get("__omnivm_result__") is True and env.get("value") is True
    except Exception:
        pass
    return False

def __omnivm_adopt_handle_id(handle_id):
    try:
        caller = __omnivm_bridge_module()
        if caller is not None and hasattr(caller, "call"):
            import json as __j
            raw = caller.call("__manifest", __j.dumps({"op": "handle_adopt", "id": handle_id}))
            env = __j.loads(raw)
            return isinstance(env, dict) and env.get("__omnivm_result__") is True and env.get("value") is True
    except Exception:
        pass
    return False

def _omnivm_encode_arg(value):
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
    __id = "arg_%s" % __omnivm_arg_ref_counter
    __omnivm_arg_refs[__id] = value
    return {"__omnivm_runtime_ref__": True, "runtime": "python", "var": "__omnivm_arg_refs[%r]" % __id, "callable": callable(value)}

class __OmniVMHandleProxy:
    _omnivm_chatty_warned = {}
    _omnivm_chatty_warned_limit = 4096

    def __init__(self, value):
        object.__setattr__(self, "_value", value)
        handle_id = value.get("id") if isinstance(value, dict) else None
        if handle_id is not None:
            if value.get("transfer") is True:
                globals()["__omnivm_adopt_handle_id"](handle_id)
            else:
                globals()["__omnivm_retain_handle_id"](handle_id)
            import weakref as __w
            object.__setattr__(self, "_finalizer", __w.finalize(self, globals()["__omnivm_release_handle_id"], handle_id))
        if isinstance(value, dict) and value.get("kind") == "mapping":
            self._sync_mapping_cache()

    def _sync_mapping_cache(self):
        try:
            if not isinstance(self._value, dict) or self._value.get("kind") != "mapping":
                return
            dict.clear(self)
            for key, item in self._value.items():
                if not self._is_internal_descriptor_key(key) and key != "__omnivm_materialized__":
                    dict.__setitem__(self, key, item)
        except Exception:
            pass

    def _warn_chatty(self, report):
        try:
            handle_id = report.get("id")
            warned = type(self)._omnivm_chatty_warned
            if handle_id in warned:
                return
            warned[handle_id] = True
            limit = type(self)._omnivm_chatty_warned_limit
            if len(warned) > limit:
                for stale_id in list(warned)[:len(warned) - limit]:
                    warned.pop(stale_id, None)
            access_kind = report.get("chattiest_access_kind") or report.get("access_kind") or "access"
            import sys as __s
            print("omnivm: chatty cross-runtime proxy access detected for handle %s (%s); consider runtime-local iteration or bulk materialization" % (handle_id, access_kind), file=__s.stderr)
        except Exception:
            pass

    def _record(self, kind="property"):
        try:
            caller = globals()["__omnivm_bridge_module"]()
            if caller is not None and hasattr(caller, "call"):
                import json as __j
                raw = caller.call("__manifest", __j.dumps({"op": "handle_access", "id": self._value.get("id"), "kind": kind}))
                env = __j.loads(raw)
                if isinstance(env, dict) and env.get("__omnivm_result__") is True:
                    report = env.get("value")
                    if isinstance(report, dict) and report.get("chatty") is True:
                        self._warn_chatty(report)
                    return report if isinstance(report, dict) else None
        except Exception:
            pass
        return None

    def _materialize_chatty(self):
        try:
            if self._value.get("__omnivm_materialized__") is True:
                return
            items = self._bridge({"op": "handle_iter", "mode": "items", "materialize": True})
            if not isinstance(items, list):
                return
            for pair in items:
                if not isinstance(pair, (list, tuple)) or len(pair) < 2:
                    continue
                key = str(pair[0])
                if key not in self._value:
                    self._value[key] = self._materialize_bridge_value(pair[1])
            self._value["__omnivm_materialized__"] = True
            self._sync_mapping_cache()
        except Exception:
            pass

    def _is_internal_descriptor_key(self, key):
        if self._value.get("__omnivm_resource__") is not True:
            return False
        return str(key) in ("__omnivm_resource__", "__omnivm_materialized__", "id", "runtime", "kind", "closed", "transfer", "disposer")

    def _has_local_value(self, key):
        return key in self._value and not self._is_internal_descriptor_key(key)

    def _has_local_text_value(self, key):
        text_key = str(key)
        return text_key in self._value and not self._is_internal_descriptor_key(text_key)

    def _local_value(self, key):
        if self._has_local_value(key):
            return self._value[key]
        text_key = str(key)
        if self._has_local_text_value(text_key):
            return self._value[text_key]
        raise KeyError(key)

    def _bridge(self, payload):
        caller = globals()["__omnivm_bridge_module"]()
        if caller is None or not hasattr(caller, "call"):
            raise AttributeError(payload.get("key"))
        import json as __j
        payload["id"] = self._value.get("id")
        try:
            raw = caller.call("__manifest", __j.dumps(payload))
        except Exception as exc:
            if globals()["_omnivm_is_missing_bridge_error"](exc):
                raise globals()["_OmniVMBridgeMissing"](str(exc))
            raise
        env = __j.loads(raw)
        if isinstance(env, dict) and env.get("__omnivm_result__") is True:
            return self._materialize_bridge_value(env.get("value"))
        return raw

    def _materialize_bridge_value(self, value):
        if isinstance(value, dict) and value.get("__omnivm_callable__") is True:
            key = value.get("key")
            if value.get("zeroArg") is True:
                return self._bridge_call(key, (), {})
            return lambda *args, **kwargs: self._bridge_call(key, args, kwargs)
        if isinstance(value, dict) and (
            value.get("__omnivm_resource__") is True
            or value.get("__omnivm_table__") is True
            or value.get("__omnivm_job__") is True
            or value.get("__omnivm_stream__") is True
            or value.get("__omnivm_channel__") is True
        ):
            return globals()["__omnivm_materialize_capture"](value)
        if isinstance(value, list):
            return [self._materialize_bridge_value(item) for item in value]
        if isinstance(value, dict):
            return {key: self._materialize_bridge_value(item) for key, item in value.items()}
        return value

    def _bridge_get(self, key):
        return self._bridge({"op": "handle_get", "key": key})

    def _bridge_index(self, key):
        return self._bridge({"op": "handle_index", "value": key})

    def _bridge_set(self, key, value):
        return self._bridge({"op": "handle_set", "key": key, "value": _omnivm_encode_arg(value)})

    def _bridge_call(self, key, args, kwargs=None):
        payload = {"op": "handle_call", "key": key, "args": [_omnivm_encode_arg(arg) for arg in args]}
        if kwargs:
            payload["kwargs"] = {str(k): _omnivm_encode_arg(v) for k, v in kwargs.items()}
        return self._bridge(payload)

    def _bridge_len(self):
        return self._bridge({"op": "handle_len"})

    def _bridge_iter(self, mode="values"):
        value = self._bridge({"op": "handle_iter", "mode": mode})
        if isinstance(value, list):
            return value
        return []

    def _bridge_contains(self, key):
        return self._bridge({"op": "handle_contains", "value": key})

    def _is_indexed_descriptor(self):
        return self._value.get("__omnivm_table__") is True or self._value.get("kind") == "sequence"

    def _numeric_index(self, key):
        if isinstance(key, bool):
            return None
        if isinstance(key, int):
            return key
        if isinstance(key, str):
            try:
                if key == str(int(key)):
                    return int(key)
            except Exception:
                return None
        return None

    def _is_proxy_method_key(self, key):
        return key in ("get", "keys", "items", "values", "copy", "update", "to_json")

    def _release_from_finalizer(self):
        try:
            finalizer = object.__getattribute__(self, "_finalizer")
        except AttributeError:
            finalizer = None
        if finalizer is not None and finalizer.alive:
            finalizer()

    def __getitem__(self, key):
        try:
            return self._local_value(key)
        except KeyError:
            pass
        report = self._record("index")
        if isinstance(report, dict) and report.get("chatty") is True:
            self._materialize_chatty()
            try:
                return self._local_value(key)
            except KeyError:
                pass
        return self._bridge_index(key)

    def __setitem__(self, key, value):
        result = self._bridge_set(str(key), value)
        text_key = str(key)
        if self._has_local_text_value(text_key):
            self._value[text_key] = value
        if isinstance(self._value, dict) and self._value.get("kind") == "mapping":
            dict.__setitem__(self, text_key, value)
        return result

    def __delitem__(self, key):
        raise TypeError("cross-runtime proxy items cannot be deleted implicitly")

    def __setattr__(self, key, value):
        if key.startswith("_"):
            object.__setattr__(self, key, value)
            return None
        result = self._bridge_set(key, value)
        if self._has_local_text_value(key):
            self._value[key] = value
        return result

    def __getattribute__(self, key):
        if key.startswith("_") or key in ("__class__", "__dict__", "__weakref__", "__module__"):
            return object.__getattribute__(self, key)
        try:
            return object.__getattribute__(self, "__getattr__")(key)
        except AttributeError:
            return object.__getattribute__(self, key)

    def __getattr__(self, key):
        try:
            return self._local_value(key)
        except KeyError:
            pass
        report = self._record("property")
        if isinstance(report, dict) and report.get("chatty") is True:
            self._materialize_chatty()
            try:
                return self._local_value(key)
            except KeyError:
                pass
        try:
            return self._bridge_get(key)
        except _OmniVMBridgeMissing:
            pass
        if self._is_proxy_method_key(key):
            raise AttributeError(key)
        try:
            return self._bridge_index(key)
        except _OmniVMBridgeMissing:
            pass
        raise AttributeError(key)

    def __contains__(self, key):
        try:
            return bool(self._bridge_contains(key))
        except _OmniVMBridgeMissing:
            self._record("property")
            return self._has_local_value(key) or self._has_local_text_value(key)

    def __iter__(self):
        try:
            if self._value.get("kind") == "mapping":
                self._materialize_chatty()
                self._sync_mapping_cache()
            mode = "values" if self._value.get("kind") == "sequence" or self._value.get("__omnivm_table__") is True else "keys"
            return iter(self._bridge_iter(mode))
        except _OmniVMBridgeMissing:
            self._record("iterate")
            return iter(self._value)

    def __len__(self):
        try:
            value = self._bridge_len()
            if isinstance(value, int):
                return value
        except _OmniVMBridgeMissing:
            pass
        self._record("property")
        return len(self._value)

    def get(self, key, default=None):
        text_key = str(key)
        try:
            return self._local_value(key)
        except KeyError:
            pass
        idx = self._numeric_index(key)
        if self._is_indexed_descriptor() and idx is not None:
            try:
                return self._bridge_index(idx)
            except _OmniVMBridgeMissing:
                return default
        report = self._record("property")
        if isinstance(report, dict) and report.get("chatty") is True:
            self._materialize_chatty()
            try:
                return self._local_value(key)
            except KeyError:
                pass
        try:
            if not self._bridge_contains(text_key):
                return default
        except _OmniVMBridgeMissing:
            pass
        try:
            return self._bridge_get(text_key)
        except _OmniVMBridgeMissing:
            return default

    def keys(self):
        self._materialize_chatty()
        self._sync_mapping_cache()
        try:
            return self._bridge_iter("keys")
        except _OmniVMBridgeMissing:
            self._record("iterate")
            return self._value.keys()

    def items(self):
        self._materialize_chatty()
        self._sync_mapping_cache()
        try:
            return [tuple(item) if isinstance(item, list) else item for item in self._bridge_iter("items")]
        except _OmniVMBridgeMissing:
            self._record("iterate")
            return self._value.items()

    def values(self):
        self._materialize_chatty()
        self._sync_mapping_cache()
        try:
            return self._bridge_iter("values")
        except _OmniVMBridgeMissing:
            self._record("iterate")
            return self._value.values()

    def to_json(self):
        self._materialize_chatty()
        self._sync_mapping_cache()
        return dict(self._value)

    def __repr__(self):
        return repr(self._value)

class __OmniVMCallableHandleProxy(__OmniVMHandleProxy):
    def __call__(self, *args, **kwargs):
        return self._bridge_call("", args, kwargs)

class __OmniVMMappingHandleProxy(__OmniVMHandleProxy, dict):
    pass

class __OmniVMStreamProxy:
    def __init__(self, value):
        self._value = value
        self._cache = []
        self._cursor = 0
        self._exhausted = False
        handle_id = value.get("id") if isinstance(value, dict) else None
        if handle_id is not None:
            if value.get("transfer") is True:
                globals()["__omnivm_adopt_handle_id"](handle_id)
            else:
                globals()["__omnivm_retain_handle_id"](handle_id)
            import weakref as __w
            self._finalizer = __w.finalize(self, globals()["__omnivm_release_handle_id"], handle_id)

    def _next_envelope(self):
        caller = globals()["__omnivm_bridge_module"]()
        if caller is None or not hasattr(caller, "call"):
            return {"done": True}
        import json as __j
        raw = caller.call("__manifest", __j.dumps({"op": "stream_next", "id": self._value.get("id")}))
        env = __j.loads(raw)
        if isinstance(env, dict) and env.get("__omnivm_result__") is True:
            return env.get("value") or {"done": True}
        return {"done": True}

    def _pull_next(self):
        if self._exhausted:
            return False
        item = self._next_envelope()
        if item.get("done") is True:
            self._exhausted = True
            return False
        self._cache.append(globals()["__omnivm_materialize_capture"](item.get("value")))
        return True

    def _materialize_all(self):
        while self._pull_next():
            pass
        return self._cache

    def __iter__(self):
        return self

    def __next__(self):
        if self._cursor >= len(self._cache) and not self._pull_next():
            raise StopIteration
        value = self._cache[self._cursor]
        self._cursor += 1
        return value

    def __len__(self):
        return len(self._materialize_all())

    def __getitem__(self, key):
        return self._materialize_all()[key]

    def __bool__(self):
        return len(self) != 0

    def close(self):
        try:
            caller = globals()["__omnivm_bridge_module"]()
            if caller is not None and hasattr(caller, "call"):
                import json as __j
                caller.call("__manifest", __j.dumps({"op": "stream_cancel", "id": self._value.get("id")}))
        except Exception:
            pass

def __omnivm_materialize_capture(value):
    if isinstance(value, dict) and (
        value.get("__omnivm_stream__") is True
        or value.get("__omnivm_channel__") is True
    ):
        handle_id = value.get("id")
        cache_key = ("stream", handle_id)
        if handle_id is not None:
            cached = __omnivm_proxy_cache.get(cache_key)
            if cached is not None:
                return cached
        proxy = __OmniVMStreamProxy(value)
        if handle_id is not None:
            __omnivm_proxy_cache[cache_key] = proxy
        return proxy
    if isinstance(value, dict) and (
        value.get("__omnivm_resource__") is True
        or value.get("__omnivm_table__") is True
        or value.get("__omnivm_job__") is True
    ):
        handle_id = value.get("id")
        cache_key = ("handle", handle_id)
        if handle_id is not None:
            cached = __omnivm_proxy_cache.get(cache_key)
            if cached is not None:
                return cached
        proxy_cls = __OmniVMCallableHandleProxy if value.get("kind") == "callable" else (__OmniVMMappingHandleProxy if value.get("kind") == "mapping" else __OmniVMHandleProxy)
        proxy = proxy_cls(value)
        if handle_id is not None:
            __omnivm_proxy_cache[cache_key] = proxy
        return proxy
    if isinstance(value, list):
        return [__omnivm_materialize_capture(item) for item in value]
    if isinstance(value, dict):
        return {key: __omnivm_materialize_capture(item) for key, item in value.items()}
    return value`
}

// injectJSCaptures generates JS code to set capture variables as globals.
func injectJSCaptures(captures map[string]string) string {
	var lines []string
	lines = append(lines, jsChannelMaterializer())
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("globalThis[%s] = globalThis.__omnivm_materialize_capture(%s);", strconv.Quote(varName), jsonVal))
	}
	return strings.Join(lines, "\n")
}

func isJSIdentifier(name string) bool {
	return isASCIIIdentifier(name, true)
}

func isJSReservedWord(name string) bool {
	switch name {
	case "await", "break", "case", "catch", "class", "const", "continue", "debugger", "default", "delete",
		"do", "else", "enum", "export", "extends", "false", "finally", "for", "function", "if", "import",
		"in", "instanceof", "new", "null", "return", "super", "switch", "this", "throw", "true", "try",
		"typeof", "var", "void", "while", "with", "yield", "let", "static", "implements", "interface",
		"package", "private", "protected", "public":
		return true
	default:
		return false
	}
}

func streamCaptureJSON(id handles.ID, runtime, kind string) string {
	b, err := json.Marshal(streamProxyValue(id, runtime, kind))
	if err != nil {
		return `{"__omnivm_stream__":true}`
	}
	return string(b)
}

func streamProxyValue(id handles.ID, runtime, kind string) map[string]interface{} {
	return map[string]interface{}{
		"__omnivm_stream__": true,
		"id":                uint64(id),
		"runtime":           runtime,
		"kind":              kind,
	}
}

func transferStreamProxyValue(id handles.ID, runtime, kind string) map[string]interface{} {
	value := streamProxyValue(id, runtime, kind)
	value["transfer"] = true
	return value
}

func jsonStringLiteral(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

func materializeJSCaptures(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := make([]string, 0, len(values)+1)
	for _, val := range values {
		out = append(out, fmt.Sprintf("globalThis.__omnivm_materialize_capture(%s)", val))
	}
	return out
}

func jsChannelMaterializer() string {
	return `globalThis.__omnivm_chatty_proxy_warned = globalThis.__omnivm_chatty_proxy_warned || {};
globalThis.__omnivm_chatty_proxy_warned_order = globalThis.__omnivm_chatty_proxy_warned_order || [];
globalThis.__omnivm_chatty_proxy_warned_limit = globalThis.__omnivm_chatty_proxy_warned_limit || 4096;
globalThis.__omnivm_warn_chatty_proxy = globalThis.__omnivm_warn_chatty_proxy || function(report) {
  try {
    var id = report && report.id;
    if (id == null || globalThis.__omnivm_chatty_proxy_warned[id]) return;
    globalThis.__omnivm_chatty_proxy_warned[id] = true;
    globalThis.__omnivm_chatty_proxy_warned_order.push(id);
    while (globalThis.__omnivm_chatty_proxy_warned_order.length > globalThis.__omnivm_chatty_proxy_warned_limit) {
      var staleId = globalThis.__omnivm_chatty_proxy_warned_order.shift();
      delete globalThis.__omnivm_chatty_proxy_warned[staleId];
    }
    var accessKind = (report && (report.chattiest_access_kind || report.access_kind)) || "access";
    if (typeof console !== 'undefined' && console && typeof console.warn === 'function') {
      console.warn("omnivm: chatty cross-runtime proxy access detected for handle " + id + " (" + accessKind + "); consider runtime-local iteration or bulk materialization");
    }
  } catch (_e) {}
};
globalThis.__omnivm_record_handle_access = globalThis.__omnivm_record_handle_access || function(id, kind) {
  try {
    if (typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
      var raw = omnivm.call("__manifest", JSON.stringify({op: "handle_access", id: id, kind: kind || "property"}));
      var env = JSON.parse(raw);
      if (env && env.__omnivm_result__ === true && env.value && env.value.chatty === true) {
        globalThis.__omnivm_warn_chatty_proxy(env.value);
      }
      return env && env.__omnivm_result__ === true ? env.value : null;
    }
  } catch (_e) {}
  return null;
};
globalThis.__omnivm_materialize_chatty_proxy = globalThis.__omnivm_materialize_chatty_proxy || function(obj) {
  try {
    var handleId = globalThis.__omnivm_proxy_handle_id(obj);
    if (!obj || obj.__omnivm_materialized__ === true || handleId == null) return;
    if (typeof omnivm === 'undefined' || !omnivm || typeof omnivm.call !== 'function') return;
    var raw = omnivm.call("__manifest", JSON.stringify({op: "handle_iter", id: handleId, mode: "items", materialize: true}));
    var env = JSON.parse(raw);
    if (!env || env.__omnivm_result__ !== true || !Array.isArray(env.value)) return;
    for (var i = 0; i < env.value.length; i++) {
      var pair = env.value[i];
      if (!Array.isArray(pair) || pair.length < 2) continue;
      var key = String(pair[0]);
      if (!(key in obj)) obj[key] = globalThis.__omnivm_materialize_capture(pair[1]);
    }
    obj.__omnivm_materialized__ = true;
  } catch (_e) {}
};
globalThis.__omnivm_record_handle_release_finalizer = globalThis.__omnivm_record_handle_release_finalizer || function(id) {
  try {
    if (typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
      omnivm.call("__manifest", JSON.stringify({op: "handle_release_finalizer", id: id}));
    }
  } catch (_e) {}
};
globalThis.__omnivm_release_handle_explicit = globalThis.__omnivm_release_handle_explicit || function(id) {
  if (typeof omnivm === 'undefined' || !omnivm || typeof omnivm.call !== 'function') return false;
  var raw = omnivm.call("__manifest", JSON.stringify({op: "handle_release_finalizer", id: id}));
  var env = JSON.parse(raw);
  return !!(env && env.__omnivm_result__ === true && env.value === true);
};
globalThis.__omnivm_retain_handle = globalThis.__omnivm_retain_handle || function(id) {
  try {
    if (typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
      var raw = omnivm.call("__manifest", JSON.stringify({op: "handle_retain", id: id}));
      var env = JSON.parse(raw);
      return !!(env && env.__omnivm_result__ === true && env.value === true);
    }
  } catch (_e) {}
  return false;
};
globalThis.__omnivm_adopt_handle = globalThis.__omnivm_adopt_handle || function(id) {
  try {
    if (typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
      var raw = omnivm.call("__manifest", JSON.stringify({op: "handle_adopt", id: id}));
      var env = JSON.parse(raw);
      return !!(env && env.__omnivm_result__ === true && env.value === true);
    }
  } catch (_e) {}
  return false;
};
globalThis.__omnivm_handle_finalizers = globalThis.__omnivm_handle_finalizers || (typeof FinalizationRegistry !== 'undefined' ? new FinalizationRegistry(function(id) {
  globalThis.__omnivm_record_handle_release_finalizer(id);
}) : null);
globalThis.__omnivm_proxy_cache = globalThis.__omnivm_proxy_cache || (typeof WeakRef !== 'undefined' ? new Map() : null);
globalThis.__omnivm_prune_proxy_cache = globalThis.__omnivm_prune_proxy_cache || function(force) {
  var cache = globalThis.__omnivm_proxy_cache;
  if (!cache || typeof WeakRef === 'undefined') return;
  if (!force && cache.size <= 4096) return;
  cache.forEach(function(ref, key) {
    if (!ref || typeof ref.deref !== 'function' || !ref.deref()) cache.delete(key);
  });
};
globalThis.__omnivm_cached_proxy = globalThis.__omnivm_cached_proxy || function(kind, id, makeProxy, descriptor) {
  if (id == null || !globalThis.__omnivm_proxy_cache || typeof WeakRef === 'undefined') return makeProxy();
  var key = kind + ":" + id;
  var existing = globalThis.__omnivm_proxy_cache.get(key);
  if (existing && typeof existing.deref === 'function') {
    var cached = existing.deref();
    if (cached) {
      if (descriptor) cached.__omnivm_descriptor__ = descriptor;
      return cached;
    }
    globalThis.__omnivm_proxy_cache.delete(key);
  }
  var proxy = makeProxy();
  globalThis.__omnivm_proxy_cache.set(key, new WeakRef(proxy));
  globalThis.__omnivm_prune_proxy_cache(false);
  return proxy;
};
globalThis.__omnivm_proxy_handle_id = globalThis.__omnivm_proxy_handle_id || function(obj) {
  var descriptor = obj && (obj.__omnivm_descriptor__ || obj);
  if (descriptor && descriptor.id != null) return descriptor.id;
  return obj && obj.id;
};
if (typeof omnivm !== 'undefined' && omnivm) {
  globalThis.__omnivm_proxy_length_symbol = globalThis.__omnivm_proxy_length_symbol ||
    (typeof Symbol !== 'undefined' ? Symbol.for("omnivm.proxy.length") : null);
  if (globalThis.__omnivm_proxy_length_symbol && typeof omnivm.proxyLength === 'undefined') {
    Object.defineProperty(omnivm, "proxyLength", {
      configurable: true,
      value: globalThis.__omnivm_proxy_length_symbol
    });
  }
  if (typeof omnivm.proxyGet !== 'function') {
    Object.defineProperty(omnivm, "proxyGet", {
      configurable: true,
      value: function(value, key, defaultValue) {
        if (value && typeof value.__omnivm_get === 'function') return value.__omnivm_get(key, defaultValue);
        if (value != null && Object.prototype.hasOwnProperty.call(Object(value), key)) return value[key];
        return defaultValue;
      }
    });
  }
  if (typeof omnivm.proxySet !== 'function') {
    Object.defineProperty(omnivm, "proxySet", {
      configurable: true,
      value: function(value, key, nextValue) {
        if (value && typeof value.__omnivm_set === 'function') return value.__omnivm_set(key, nextValue);
        if (value == null) return false;
        value[key] = nextValue;
        return true;
      }
    });
  }
  if (typeof omnivm.proxyCall !== 'function') {
    Object.defineProperty(omnivm, "proxyCall", {
      configurable: true,
      value: function(value, key, args) {
        var callArgs = Array.isArray(args) ? args : [];
        if (value && typeof value.__omnivm_call === 'function') return value.__omnivm_call(key, callArgs);
        if (value == null) throw new TypeError("OmniVM cannot call a method on null or undefined");
        if (key === null || key === undefined || key === "") {
          if (typeof value !== 'function') throw new TypeError("OmniVM target is not callable");
          return value.apply(undefined, callArgs);
        }
        var member = value[key];
        if (typeof member !== 'function') throw new TypeError("OmniVM member is not callable: " + String(key));
        return member.apply(value, callArgs);
      }
    });
  }
  if (typeof omnivm.proxyLen !== 'function') {
    Object.defineProperty(omnivm, "proxyLen", {
      configurable: true,
      value: function(value, defaultValue) {
        if (value && typeof value.__omnivm_len === 'function') return value.__omnivm_len(defaultValue);
        if (value != null && typeof value.length === 'number') return value.length;
        return defaultValue;
      }
    });
  }
  if (typeof omnivm.proxyIter !== 'function') {
    Object.defineProperty(omnivm, "proxyIter", {
      configurable: true,
      value: function(value, mode) {
        var iterMode = mode || "values";
        if (value && typeof value.__omnivm_iter === 'function') return value.__omnivm_iter(iterMode);
        if (value == null) return [];
        if (value instanceof Map) {
          if (iterMode === "keys") return Array.from(value.keys());
          if (iterMode === "items") return Array.from(value.entries());
          return Array.from(value.values());
        }
        if (iterMode === "keys") return Object.keys(Object(value));
        if (iterMode === "items") return Object.keys(Object(value)).map(function(key) { return [key, value[key]]; });
        if (typeof Symbol !== 'undefined' && typeof value[Symbol.iterator] === 'function' && !Array.isArray(value)) return Array.from(value);
        return Object.keys(Object(value)).map(function(key) { return value[key]; });
      }
    });
  }
  if (typeof omnivm.proxyKeys !== 'function') {
    Object.defineProperty(omnivm, "proxyKeys", {
      configurable: true,
      value: function(value) { return omnivm.proxyIter(value, "keys"); }
    });
  }
  if (typeof omnivm.proxyValues !== 'function') {
    Object.defineProperty(omnivm, "proxyValues", {
      configurable: true,
      value: function(value) { return omnivm.proxyIter(value, "values"); }
    });
  }
  if (typeof omnivm.proxyItems !== 'function') {
    Object.defineProperty(omnivm, "proxyItems", {
      configurable: true,
      value: function(value) { return omnivm.proxyIter(value, "items"); }
    });
  }
  if (typeof omnivm.proxyContains !== 'function') {
    Object.defineProperty(omnivm, "proxyContains", {
      configurable: true,
      value: function(value, key) {
        if (value && typeof value.__omnivm_contains === 'function') return value.__omnivm_contains(key);
        if (value == null) return false;
        if (value instanceof Map || value instanceof Set) return value.has(key);
        if (Array.isArray(value)) return value.indexOf(key) !== -1;
        return Object.prototype.hasOwnProperty.call(Object(value), key);
      }
    });
  }
  if (typeof omnivm.proxyClose !== 'function') {
    Object.defineProperty(omnivm, "proxyClose", {
      configurable: true,
      value: function(value) {
        if (value && typeof value.__omnivm_close === 'function') return value.__omnivm_close();
        if (value && typeof value.close === 'function') {
          value.close();
          return true;
        }
        return false;
      }
    });
  }
}
globalThis.__omnivm_arg_refs = globalThis.__omnivm_arg_refs || {};
globalThis.__omnivm_arg_ref_counter = globalThis.__omnivm_arg_ref_counter || 0;
globalThis.__omnivm_is_missing_bridge_error = globalThis.__omnivm_is_missing_bridge_error || function(error) {
  var text = String(error && (error.message || error));
  return text.indexOf(" has no property ") >= 0 ||
    text.indexOf(" has no index ") >= 0 ||
    text.indexOf(" has no length") >= 0 ||
    text.indexOf(" is not iterable") >= 0 ||
    text.indexOf(" does not support contains") >= 0 ||
    text.indexOf(" has no writable property ") >= 0;
};
globalThis.__omnivm_encode_arg = globalThis.__omnivm_encode_arg || function(value) {
  if (value === null || value === undefined || typeof value === "string" || typeof value === "number" || typeof value === "boolean") return value;
  if (value && value.__omnivm_proxy__ === true && value.__omnivm_descriptor__) return value.__omnivm_descriptor__;
  var id = "arg_" + (++globalThis.__omnivm_arg_ref_counter);
  globalThis.__omnivm_arg_refs[id] = value;
  return {__omnivm_runtime_ref__: true, runtime: "javascript", var: "__omnivm_arg_refs[" + JSON.stringify(id) + "]", callable: typeof value === "function"};
};
globalThis.__omnivm_make_handle_proxy = globalThis.__omnivm_make_handle_proxy || function(target, jsonShape) {
  if (typeof Proxy === 'undefined') return target;
  var decode = function(raw, options) {
    try {
      var env = JSON.parse(raw);
      if (env && env.__omnivm_result__ === true) {
        if (env.value && env.value.__omnivm_callable__ === true) {
          if (env.value.zeroArg === true && !(options && options.preserveCallable)) {
            return bridge({op: "handle_call", key: env.value.key, args: []});
          }
          return function() {
            return bridge({op: "handle_call", key: env.value.key, args: Array.prototype.slice.call(arguments).map(globalThis.__omnivm_encode_arg)});
          };
        }
        if (env.value && (env.value.__omnivm_resource__ === true || env.value.__omnivm_table__ === true || env.value.__omnivm_job__ === true)) {
          return globalThis.__omnivm_materialize_capture(env.value);
        }
        return globalThis.__omnivm_materialize_capture(env.value);
      }
    } catch (_e) {}
    return raw;
  };
  var bridge = function(payload, options) {
    payload.id = globalThis.__omnivm_proxy_handle_id(target);
    return decode(omnivm.call("__manifest", JSON.stringify(payload)), options);
  };
  var descriptor = target && (target.__omnivm_descriptor__ || target);
  var isRuntimeRefFunctionTarget = function() {
    return typeof target === 'function' && descriptor && descriptor.kind === "runtime_ref";
  };
  var isFunctionIntrinsic = function(prop) {
    return prop === 'length' || prop === 'name' || prop === 'prototype' || prop === 'caller' || prop === 'arguments';
  };
  var hasLocalProp = function(obj, prop) {
    return Object.prototype.hasOwnProperty.call(obj, prop) && !(isRuntimeRefFunctionTarget() && isFunctionIntrinsic(prop));
  };
  var isProxyBookkeepingProp = function(prop) {
    return prop === "__omnivm_proxy__" || prop === "__omnivm_descriptor__" || prop === "__omnivm_materialized__" || prop === "__omnivm_get" || prop === "__omnivm_set" || prop === "__omnivm_call" || prop === "__omnivm_len" || prop === "__omnivm_iter" || prop === "__omnivm_contains" || prop === "toJSON";
  };
  var isIndexedDescriptor = function() {
    return descriptor && (descriptor.__omnivm_table__ === true || descriptor.kind === "sequence");
  };
  var remoteDescription = function() {
    if (!descriptor) return "";
    var parts = [];
    if (descriptor.runtime != null) parts.push("runtime=" + descriptor.runtime);
    if (descriptor.kind != null) parts.push("kind=" + descriptor.kind);
    else if (descriptor.__omnivm_table__ === true) parts.push("kind=table");
    if (descriptor.id != null) parts.push("id=" + descriptor.id);
    return parts.length ? " (" + parts.join(", ") + ")" : "";
  };
  var lengthSetDiagnostic = function(reason, cause) {
    var message = "OmniVM cannot resize remote indexed proxy" + remoteDescription() + ": " + reason;
    if (cause && cause.message) message += ": " + cause.message;
    var err = new TypeError(message);
    if (cause) {
      try { err.cause = cause; } catch (_causeError) {}
    }
    return err;
  };
  var numericIndex = function(value) {
    if (typeof value === "number" && Number.isInteger(value)) return value;
    if (typeof value === "string" && /^(0|-?[1-9][0-9]*)$/.test(value)) return Number(value);
    return null;
  };
  var bridgeGet = function(key, defaultValue) {
    if (hasLocalProp(target, key)) return target[key];
    var textKey = String(key);
    if (hasLocalProp(target, textKey)) return target[textKey];
    var idx = numericIndex(key);
    if (isIndexedDescriptor() && idx !== null) {
      try {
        return bridge({op: "handle_index", value: idx});
      } catch (_e) {
        if (!globalThis.__omnivm_is_missing_bridge_error(_e)) throw _e;
        return defaultValue;
      }
    }
    try {
      return bridge({op: "handle_get", key: textKey});
    } catch (_getError) {
      if (!globalThis.__omnivm_is_missing_bridge_error(_getError)) throw _getError;
      try {
        return bridge({op: "handle_index", value: key});
      } catch (_indexError) {
        if (!globalThis.__omnivm_is_missing_bridge_error(_indexError)) throw _indexError;
        return defaultValue;
      }
    }
  };
  var bridgeLen = function(defaultValue) {
    try {
      return bridge({op: "handle_len"});
    } catch (_e) {
      if (!globalThis.__omnivm_is_missing_bridge_error(_e)) throw _e;
      return defaultValue;
    }
  };
  var bridgeSet = function(key, value) {
    var textKey = String(key);
    if (textKey === 'length' && isIndexedDescriptor()) {
      var lengthValue = Number(value);
      if (!Number.isInteger(lengthValue) || lengthValue < 0) {
        throw new RangeError("OmniVM cannot set remote length" + remoteDescription() + ": length must be a non-negative integer");
      }
      try {
        bridge({op: "handle_set", key: textKey, value: globalThis.__omnivm_encode_arg(lengthValue)});
        return true;
      } catch (_lengthSetError) {
        throw lengthSetDiagnostic("source runtime rejected length write", _lengthSetError);
      }
    }
    try {
      bridge({op: "handle_set", key: textKey, value: globalThis.__omnivm_encode_arg(value)});
      return true;
    } catch (_setError) {
      if (!globalThis.__omnivm_is_missing_bridge_error(_setError)) throw _setError;
      return false;
    }
  };
  var bridgeCall = function(key, args) {
    var callArgs = Array.isArray(args) ? args : [];
    return bridge({op: "handle_call", key: key == null ? "" : String(key), args: callArgs.map(globalThis.__omnivm_encode_arg)});
  };
  var bridgeIter = function(mode) {
    return bridge({op: "handle_iter", mode: mode || "values"});
  };
  var bridgeContains = function(key) {
    return !!bridge({op: "handle_contains", value: globalThis.__omnivm_encode_arg(key)});
  };
  var releaseProxyLease = function() {
    var handleId = globalThis.__omnivm_proxy_handle_id(target);
    if (handleId == null || target.__omnivm_closed__ === true) return false;
    var released = globalThis.__omnivm_release_handle_explicit(handleId);
    target.__omnivm_closed__ = true;
    if (globalThis.__omnivm_handle_finalizers && typeof globalThis.__omnivm_handle_finalizers.unregister === 'function') {
      globalThis.__omnivm_handle_finalizers.unregister(target);
    }
    return released;
  };
  var proxy = new Proxy(target, {
    get: function(obj, prop, receiver) {
      if (prop === "__omnivm_get") return function(key, defaultValue) { return bridgeGet(key, defaultValue); };
      if (prop === "__omnivm_set") return function(key, value) { return bridgeSet(key, value); };
      if (prop === "__omnivm_call") return function(key, args) { return bridgeCall(key, args); };
      if (prop === "__omnivm_len") return function(defaultValue) { return bridgeLen(defaultValue); };
      if (prop === "__omnivm_iter") return function(mode) { return bridgeIter(mode); };
      if (prop === "__omnivm_contains") return function(key) { return bridgeContains(key); };
      if (prop === "__omnivm_close") return releaseProxyLease;
      if (globalThis.__omnivm_proxy_length_symbol && prop === globalThis.__omnivm_proxy_length_symbol) {
        return bridgeLen(Reflect.get(obj, 'length', receiver));
      }
      if (prop === 'then' && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        try {
          var thenValue = bridge({op: "handle_get", key: "then"}, {preserveCallable: true});
          return typeof thenValue === 'function' ? undefined : thenValue;
        } catch (_thenError) {
          if (!globalThis.__omnivm_is_missing_bridge_error(_thenError)) throw _thenError;
          return Reflect.get(obj, prop, receiver);
        }
      }
      if (prop === 'length' && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        if (!(descriptor && descriptor.__omnivm_table__ === true)) {
          try {
            if (bridge({op: "handle_contains", value: "length"})) return bridge({op: "handle_get", key: "length"});
          } catch (_fieldLengthError) {
            if (!globalThis.__omnivm_is_missing_bridge_error(_fieldLengthError)) throw _fieldLengthError;
          }
        }
        return bridgeLen(Reflect.get(obj, prop, receiver));
      }
      if (prop === 'name' && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        try {
          if (bridge({op: "handle_contains", value: "name"})) return bridge({op: "handle_get", key: "name"});
        } catch (_fieldNameError) {
          if (!globalThis.__omnivm_is_missing_bridge_error(_fieldNameError)) throw _fieldNameError;
        }
        return Reflect.get(obj, prop, receiver);
      }
      if (hasLocalProp(obj, prop)) return Reflect.get(obj, prop, receiver);
      if (prop !== 'toJSON' && prop !== Symbol.toStringTag && prop !== Symbol.iterator) {
        var report = globalThis.__omnivm_record_handle_access(globalThis.__omnivm_proxy_handle_id(obj), "property");
        if (report && report.chatty === true) {
          globalThis.__omnivm_materialize_chatty_proxy(obj);
          if (hasLocalProp(obj, prop)) return Reflect.get(obj, prop, receiver);
        }
      }
      if (prop === Symbol.iterator && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        return function() {
          try {
            var values = bridge({op: "handle_iter", mode: "values"});
            if (Array.isArray(values)) return values[Symbol.iterator]();
          } catch (_e) {
            if (!globalThis.__omnivm_is_missing_bridge_error(_e)) throw _e;
          }
          return [][Symbol.iterator]();
        };
      }
      if (typeof prop === 'string' && !(prop in obj) && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        if (isIndexedDescriptor() && /^(0|[1-9][0-9]*)$/.test(prop)) {
          try {
            return bridge({op: "handle_index", value: Number(prop)});
          } catch (_indexedPropError) {
            if (!globalThis.__omnivm_is_missing_bridge_error(_indexedPropError)) throw _indexedPropError;
          }
        }
        try {
          return bridge({op: "handle_get", key: prop});
        } catch (_e) {
          if (!globalThis.__omnivm_is_missing_bridge_error(_e)) throw _e;
          if (prop === 'get') {
            return bridgeGet;
          }
          if (/^(0|[1-9][0-9]*)$/.test(prop)) {
            try {
              return bridge({op: "handle_index", value: Number(prop)});
            } catch (_ignored) {
              if (!globalThis.__omnivm_is_missing_bridge_error(_ignored)) throw _ignored;
            }
          }
        }
      }
      if (typeof prop === 'string' && !isProxyBookkeepingProp(prop) && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        try {
          if (bridge({op: "handle_contains", value: prop})) return bridge({op: "handle_get", key: prop});
        } catch (_inheritedFieldError) {
          if (!globalThis.__omnivm_is_missing_bridge_error(_inheritedFieldError)) throw _inheritedFieldError;
        }
      }
      return Reflect.get(obj, prop, receiver);
    },
    set: function(obj, prop, value, receiver) {
      if (typeof prop === 'string' && !isProxyBookkeepingProp(prop) && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        if (bridgeSet(prop, value)) {
          if (hasLocalProp(obj, prop)) Reflect.set(obj, prop, value, receiver);
          return true;
        }
      }
      return Reflect.set(obj, prop, value, receiver);
    },
    apply: function(obj, thisArg, args) {
      return bridgeCall("", Array.prototype.slice.call(args));
    },
    has: function(obj, prop) {
      if (typeof prop === 'string' && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        try {
          return !!bridge({op: "handle_contains", value: prop});
        } catch (_e) {
          if (!globalThis.__omnivm_is_missing_bridge_error(_e)) throw _e;
        }
      }
      globalThis.__omnivm_record_handle_access(globalThis.__omnivm_proxy_handle_id(obj), "property");
      return Reflect.has(obj, prop);
    },
    getOwnPropertyDescriptor: function(obj, prop) {
      var local = Object.getOwnPropertyDescriptor(obj, prop);
      if (local) return local;
      if (typeof prop === 'string' && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        try {
          if (bridge({op: "handle_contains", value: prop})) {
            return {enumerable: true, configurable: true};
          }
        } catch (_e) {
          if (!globalThis.__omnivm_is_missing_bridge_error(_e)) throw _e;
        }
      }
      return undefined;
    },
    ownKeys: function(obj) {
      if (typeof omnivm !== 'undefined' && omnivm && typeof omnivm.call === 'function') {
        try {
          var keys = bridge({op: "handle_iter", mode: "keys"});
          if (Array.isArray(keys)) {
            var out = keys.map(function(key) { return String(key); });
            if (Array.isArray(obj) && out.indexOf("length") < 0) out.push("length");
            return out;
          }
        } catch (_e) {
          if (!globalThis.__omnivm_is_missing_bridge_error(_e)) throw _e;
        }
      }
      globalThis.__omnivm_record_handle_access(globalThis.__omnivm_proxy_handle_id(obj), "iterate");
      return Reflect.ownKeys(obj);
    }
  });
  var finalizerHandleId = globalThis.__omnivm_proxy_handle_id(target);
  if (globalThis.__omnivm_handle_finalizers && target && finalizerHandleId != null) {
    var descriptor = target.__omnivm_descriptor__ || target;
    if (descriptor && descriptor.transfer === true) {
      globalThis.__omnivm_adopt_handle(finalizerHandleId);
    } else {
      globalThis.__omnivm_retain_handle(finalizerHandleId);
    }
    globalThis.__omnivm_handle_finalizers.register(proxy, finalizerHandleId, target);
  }
  return proxy;
};
globalThis.__omnivm_make_stream_proxy = globalThis.__omnivm_make_stream_proxy || function(value) {
  var localValues = Array.isArray(value && value.values) ? value.values.map(function(v) {
    return globalThis.__omnivm_materialize_capture(v);
  }) : null;
  var localIndex = 0;
  var remoteClosed = false;
  var markRemoteClosed = function() {
    if (remoteClosed) return false;
    remoteClosed = true;
    if (stream) stream.__omnivm_closed__ = true;
    if (globalThis.__omnivm_handle_finalizers && typeof globalThis.__omnivm_handle_finalizers.unregister === 'function' && stream) {
      globalThis.__omnivm_handle_finalizers.unregister(stream);
    }
    return true;
  };
  var cancelRemote = function() {
    if (remoteClosed) return false;
    if (typeof omnivm === 'undefined' || !omnivm || typeof omnivm.call !== 'function') return false;
    omnivm.call("__manifest", JSON.stringify({op: "stream_cancel", id: value.id}));
    return markRemoteClosed();
  };
  var closeRemote = function() {
    markRemoteClosed();
  };
  var nextValue = function() {
    if (localValues) {
      if (localIndex >= localValues.length) return {done: true};
      return {done: false, value: localValues[localIndex++]};
    }
    try {
      if (typeof omnivm === 'undefined' || !omnivm || typeof omnivm.call !== 'function') return {done: true};
      var raw = omnivm.call("__manifest", JSON.stringify({op: "stream_next", id: value.id}));
      var env = JSON.parse(raw);
      if (env && env.__omnivm_result__ === true && env.value) {
        if (env.value.done === true) {
          closeRemote();
          return {done: true};
        }
        return {done: false, value: globalThis.__omnivm_stream_chunk_value(env.value.value)};
      }
    } catch (_e) {
      throw _e;
    }
    return {done: true};
  };
    var stream = {
    runtime: value.runtime,
    kind: value.kind,
    cancel: function(reason) {
      this.cancelled = reason || true;
      return cancelRemote();
    },
    toArray: function() {
      var out = [];
      for (var item of this) out.push(item);
      return out;
    },
    toNodeReadable: function(options) {
      if (typeof require !== 'function') {
        throw new Error("Node.js Readable streams are unavailable in this JavaScript runtime");
      }
      var streamModule = require('node:stream');
      if (!streamModule || typeof streamModule.Readable !== 'function') {
        throw new Error("Node.js Readable streams are unavailable in this JavaScript runtime");
      }
      var source = this;
      var iterator = source[Symbol.asyncIterator]();
      var closed = false;
      var closeIterator = function(reason) {
        if (closed) return Promise.resolve();
        closed = true;
        if (iterator && typeof iterator.return === 'function') {
          return Promise.resolve(iterator.return(reason)).then(function() {});
        }
        if (source && typeof source.cancel === 'function') {
          return Promise.resolve(source.cancel(reason)).then(function() {});
        }
        return Promise.resolve();
      };
      var opts = Object.assign({}, options || {});
      opts.read = function() {
        var target = this;
        Promise.resolve(iterator.next()).then(function(item) {
          if (item && item.done) {
            closed = true;
            target.push(null);
            return;
          }
          target.push(item ? item.value : undefined);
        }, function(err) {
          target.destroy(err);
        });
      };
      opts.destroy = function(err, cb) {
        closeIterator(err).then(function() {
          cb(err);
        }, function(closeErr) {
          cb(err || closeErr);
        });
      };
      return new streamModule.Readable(opts);
    },
    __omnivm_close: function() {
      return cancelRemote();
    },
    [Symbol.iterator]: function() {
      var owner = this;
      return {
        next: nextValue,
        return: function(reason) {
          owner.cancel(reason);
          return {done: true};
        }
      };
    },
    [Symbol.asyncIterator]: function() {
      var owner = this;
      return {
        next: function() {
          return Promise.resolve(nextValue());
        },
        return: function(reason) {
          owner.cancel(reason);
          return Promise.resolve({done: true});
        }
      };
    }
  };
  if (globalThis.__omnivm_handle_finalizers && value && value.id != null) {
    if (value.transfer === true) {
      globalThis.__omnivm_adopt_handle(value.id);
    } else {
      globalThis.__omnivm_retain_handle(value.id);
    }
    globalThis.__omnivm_handle_finalizers.register(stream, value.id, stream);
  }
  return stream;
};
globalThis.__omnivm_stream_chunk_value = globalThis.__omnivm_stream_chunk_value || function(value) {
  if (value && value.__omnivm_table__ === true) {
    var metadata = value.metadata || {};
    var dtype = metadata.dtype;
    var bufferName = value.buffer || metadata.buffer || null;
    var byteDtype = dtype === 0 || dtype === 5 || dtype === 10 || dtype === 11;
    if (byteDtype && bufferName && typeof omnivm !== 'undefined' && omnivm && typeof omnivm.getBuffer === 'function') {
      var shape = Array.isArray(metadata.shape) ? metadata.shape : [];
      var length = shape.length > 0 ? Number(shape[0]) : 0;
      var offset = Number(metadata.offset || 0);
      var strides = Array.isArray(metadata.strides) ? metadata.strides : [];
      var stride = strides.length > 0 ? Number(strides[0]) : 1;
      if (!Number.isFinite(length) || length < 0) length = 0;
      if (!Number.isFinite(offset) || offset < 0) offset = 0;
      if (!Number.isFinite(stride) || stride === 0) stride = 1;
      if (length === 0) return new Uint8Array(0);
      var raw = omnivm.getBuffer(bufferName);
      if (raw instanceof ArrayBuffer) {
        var bytes = new Uint8Array(raw);
        if (stride === 1 && offset + length <= bytes.byteLength) {
          return new Uint8Array(raw, offset, length);
        }
        var out = new Uint8Array(length);
        for (var i = 0; i < length; i++) {
          var src = offset + i * stride;
          if (src >= 0 && src < bytes.byteLength) out[i] = bytes[src];
        }
        return out;
      }
    }
  }
  return globalThis.__omnivm_materialize_capture(value);
};
globalThis.__omnivm_materialize_capture = globalThis.__omnivm_materialize_capture || function(value) {
  if (value && (value.__omnivm_stream__ === true || value.__omnivm_channel__ === true)) {
    return globalThis.__omnivm_cached_proxy("stream", value.id, function() {
      return globalThis.__omnivm_make_stream_proxy(value);
    }, value);
  }
  if (value && value.__omnivm_resource__ === true) {
    return globalThis.__omnivm_cached_proxy("resource", value.id, function() {
      var target = value.kind === "callable" ? function() {} : (value.kind === "sequence" ? [] : {});
      target.__omnivm_proxy__ = true;
      target.__omnivm_descriptor__ = value;
      target.toJSON = function() { var descriptor = target.__omnivm_descriptor__ || value; return {id: descriptor.id, runtime: descriptor.runtime, kind: descriptor.kind, closed: descriptor.closed === true}; };
      return globalThis.__omnivm_make_handle_proxy(target);
    }, value);
  }
  if (value && value.__omnivm_table__ === true) {
    return globalThis.__omnivm_cached_proxy("table", value.id, function() {
      return globalThis.__omnivm_make_handle_proxy({
        __omnivm_proxy__: true,
        __omnivm_descriptor__: value,
        id: value.id,
        runtime: value.runtime,
        format: value.format,
        ownership: value.ownership,
        buffer: value.buffer || (value.metadata && value.metadata.buffer) || null,
        metadata: value.metadata || null,
        released: value.released === true,
        toJSON: function() { var descriptor = this.__omnivm_descriptor__ || value; return {id: descriptor.id, runtime: descriptor.runtime, format: descriptor.format, ownership: descriptor.ownership, buffer: descriptor.buffer || (descriptor.metadata && descriptor.metadata.buffer) || null, metadata: descriptor.metadata || null, released: descriptor.released === true}; }
      });
    }, value);
  }
  if (value && value.__omnivm_job__ === true) {
    return globalThis.__omnivm_cached_proxy("job", value.id, function() {
      return globalThis.__omnivm_make_handle_proxy({
        __omnivm_proxy__: true,
        __omnivm_descriptor__: value,
        id: value.id,
        runtime: value.runtime,
        kind: value.kind,
        done: value.done === true,
        cancelled: value.cancelled === true,
        cancelReason: value.cancelReason,
        payload: value.payload,
        result: value.result,
        toJSON: function() { var descriptor = this.__omnivm_descriptor__ || value; return {id: descriptor.id, runtime: descriptor.runtime, kind: descriptor.kind, done: descriptor.done === true, cancelled: descriptor.cancelled === true, cancelReason: descriptor.cancelReason, payload: descriptor.payload, result: descriptor.result}; }
      });
    }, value);
  }
  if (value && (value.__omnivm_proxy__ === true || value.__omnivm_disposable__ === true)) {
    return value;
  }
  if (Array.isArray(value)) {
    return value.map(function(item) { return globalThis.__omnivm_materialize_capture(item); });
  }
  if (value && typeof value === 'object') {
    var mapped = {};
    Object.keys(value).forEach(function(key) {
      mapped[key] = globalThis.__omnivm_materialize_capture(value[key]);
    });
    return mapped;
  }
  return value;
};`
}

// injectRubyCaptures generates Ruby code to set capture variables as globals.
// Ruby local variables are scoped to the eval context and don't persist across
// separate Execute()/Eval() calls. We use global variables ($var) so they
// persist, matching how Python uses module globals and JS uses globalThis.
func injectRubyCaptures(captures map[string]string) string {
	var lines []string
	lines = append(lines, "require 'json'")
	lines = append(lines, rubyCaptureMaterializer())
	for varName, jsonVal := range captures {
		lines = append(lines, runtimeAssign("ruby", varName, fmt.Sprintf("__omnivm_materialize_capture(JSON.parse('%s'))", escapeRubyString(jsonVal))))
	}
	return strings.Join(lines, "\n")
}

func rubyCaptureMaterializer() string {
	return `require 'weakref'
$__omnivm_proxy_cache ||= {}
$__omnivm_proxy_cache_limit ||= 4096

def __omnivm_prune_proxy_cache(force = false)
  return unless force || $__omnivm_proxy_cache.length > $__omnivm_proxy_cache_limit
  $__omnivm_proxy_cache.delete_if do |_key, ref|
    begin
      ref.__getobj__.nil?
    rescue WeakRef::RefError
      true
    rescue
      true
    end
  end
end

def __omnivm_cached_proxy(kind, value)
  id = value["id"] if value.is_a?(Hash)
  return yield if id.nil?
  key = [kind, id]
  begin
    ref = $__omnivm_proxy_cache[key]
    if ref
      cached = ref.__getobj__
      return cached unless cached.nil?
    end
  rescue WeakRef::RefError
    $__omnivm_proxy_cache.delete(key)
  rescue
    $__omnivm_proxy_cache.delete(key)
  end
  proxy = yield
  $__omnivm_proxy_cache[key] = WeakRef.new(proxy)
  __omnivm_prune_proxy_cache(false)
  proxy
end

class OmniVMHandleProxy
  OMNIVM_MISSING = Object.new unless const_defined?(:OMNIVM_MISSING, false)

  def self.__omnivm_missing_bridge_error?(error)
    text = error.message.to_s
    text.include?(" has no property ") ||
      text.include?(" has no index ") ||
      text.include?(" has no length") ||
      text.include?(" is not iterable") ||
      text.include?(" does not support contains") ||
      text.include?(" has no writable property ")
  end

  def __omnivm_missing_bridge_error?(error)
    self.class.__omnivm_missing_bridge_error?(error)
  end

  def initialize(value)
    @value = value
    @__omnivm_closed = false
    id = @value["id"]
    if !id.nil?
      @value["transfer"] == true ? self.class.omnivm_adopt(id) : self.class.omnivm_retain(id)
    end
    ObjectSpace.define_finalizer(self, self.class.omnivm_finalizer(id)) unless id.nil?
  end

  def self.omnivm_retain(id)
    begin
      if defined?(OmniVM) && OmniVM.respond_to?(:call)
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_retain", id: id}))
        env = JSON.parse(raw)
        return env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"] == true
      end
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    false
  end

  def self.omnivm_adopt(id)
    begin
      if defined?(OmniVM) && OmniVM.respond_to?(:call)
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_adopt", id: id}))
        env = JSON.parse(raw)
        return env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"] == true
      end
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    false
  end

  def self.omnivm_finalizer(id)
    proc do
      begin
        if defined?(OmniVM) && OmniVM.respond_to?(:call)
          OmniVM.call("__manifest", JSON.generate({op: "handle_release_finalizer", id: id}))
        end
      rescue
      end
    end
  end

  @@__omnivm_chatty_warned = {}
  @@__omnivm_chatty_warned_limit = 4096

  def __omnivm_warn_chatty(report)
    begin
      id = report["id"]
      return if id.nil? || @@__omnivm_chatty_warned[id]
      @@__omnivm_chatty_warned[id] = true
      if @@__omnivm_chatty_warned.size > @@__omnivm_chatty_warned_limit
        @@__omnivm_chatty_warned.keys.first(@@__omnivm_chatty_warned.size - @@__omnivm_chatty_warned_limit).each { |stale_id| @@__omnivm_chatty_warned.delete(stale_id) }
      end
      access_kind = report["chattiest_access_kind"] || report["access_kind"] || "access"
      warn("omnivm: chatty cross-runtime proxy access detected for handle #{id} (#{access_kind}); consider runtime-local iteration or bulk materialization")
    rescue
    end
  end

  def __omnivm_record(kind = "property")
    begin
      if defined?(OmniVM) && OmniVM.respond_to?(:call)
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_access", id: @value["id"], kind: kind}))
        env = JSON.parse(raw)
        if env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"].is_a?(Hash) && env["value"]["chatty"] == true
          __omnivm_warn_chatty(env["value"])
        end
        return env["value"] if env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"].is_a?(Hash)
      end
    rescue
    end
    nil
  end

  def __omnivm_data_key?(key)
    text_key = key.to_s
    return true if __omnivm_local_key?(key)
    begin
      if defined?(OmniVM) && OmniVM.respond_to?(:call)
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_contains", id: @value["id"], value: text_key}))
        env = JSON.parse(raw)
        return !!env["value"] if env.is_a?(Hash) && env["__omnivm_result__"] == true
      end
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    false
  end

  def __omnivm_internal_descriptor_key?(key)
    @value["__omnivm_resource__"] == true &&
      ["__omnivm_resource__", "__omnivm_materialized__", "id", "runtime", "kind", "closed", "transfer", "disposer"].include?(key.to_s)
  end

  def __omnivm_local_key?(key)
    text_key = key.to_s
    (@value.key?(key) && !__omnivm_internal_descriptor_key?(key)) ||
      (@value.key?(text_key) && !__omnivm_internal_descriptor_key?(text_key))
  end

  def __omnivm_local_value(key)
    text_key = key.to_s
    return @value[key] if @value.key?(key) && !__omnivm_internal_descriptor_key?(key)
    return @value[text_key] if @value.key?(text_key) && !__omnivm_internal_descriptor_key?(text_key)
    OMNIVM_MISSING
  end

  def __omnivm_data_key_value(key, default = OMNIVM_MISSING)
    text_key = key.to_s
    local = __omnivm_local_value(key)
    return local unless local.equal?(OMNIVM_MISSING)
    report = __omnivm_record("property")
    if report.is_a?(Hash) && report["chatty"] == true
      __omnivm_materialize_chatty
      local = __omnivm_local_value(key)
      return local unless local.equal?(OMNIVM_MISSING)
    end
    if __omnivm_data_key?(text_key)
      begin
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_index", id: @value["id"], value: text_key}))
        env = JSON.parse(raw)
        return __omnivm_materialize_bridge_value(env["value"]) if env.is_a?(Hash) && env["__omnivm_result__"] == true
      rescue => e
        raise unless __omnivm_missing_bridge_error?(e)
      end
      begin
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_get", id: @value["id"], key: text_key}))
        env = JSON.parse(raw)
        return __omnivm_materialize_bridge_value(env["value"]) if env.is_a?(Hash) && env["__omnivm_result__"] == true
      rescue => e
        raise unless __omnivm_missing_bridge_error?(e)
      end
    end
    return default unless default.equal?(OMNIVM_MISSING)
    raise NameError, text_key
  end

  def __omnivm_materialize_chatty
    begin
      return if @value["__omnivm_materialized__"] == true
      raw = OmniVM.call("__manifest", JSON.generate({op: "handle_iter", id: @value["id"], mode: "items", materialize: true}))
      env = JSON.parse(raw)
      return unless env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"].is_a?(Array)
      env["value"].each do |pair|
        next unless pair.is_a?(Array) && pair.length >= 2
        key = pair[0].to_s
        @value[key] = __omnivm_materialize_bridge_value(pair[1]) unless @value.key?(key)
      end
      @value["__omnivm_materialized__"] = true
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
  end

  def __omnivm_encode_arg(value)
    return value if value.nil? || value.is_a?(String) || value.is_a?(Numeric) || value == true || value == false
    descriptor = value.instance_variable_get(:@value) if value.respond_to?(:instance_variable_get)
    if descriptor.is_a?(Hash) && (
      descriptor["__omnivm_resource__"] == true ||
      descriptor["__omnivm_table__"] == true ||
      descriptor["__omnivm_stream__"] == true ||
      descriptor["__omnivm_channel__"] == true ||
      descriptor["__omnivm_job__"] == true
    )
      return descriptor
    end
    $__omnivm_arg_refs ||= {}
    $__omnivm_arg_ref_counter ||= 0
    $__omnivm_arg_ref_counter += 1
    id = "arg_#{$__omnivm_arg_ref_counter}"
    $__omnivm_arg_refs[id] = value
    {"__omnivm_runtime_ref__" => true, "runtime" => "ruby", "var" => "$__omnivm_arg_refs[#{id.inspect}]", "callable" => value.respond_to?(:call)}
  end

  def __omnivm_materialize_bridge_value(value)
    if value.is_a?(Hash) && value["__omnivm_callable__"] == true
      if value["zeroArg"] == true
        raw_call = OmniVM.call("__manifest", JSON.generate({op: "handle_call", id: @value["id"], key: value["key"], args: []}))
        env_call = JSON.parse(raw_call)
        return env_call.is_a?(Hash) && env_call["__omnivm_result__"] == true ? __omnivm_materialize_bridge_value(env_call["value"]) : raw_call
      end
      return proc do |*call_args|
        raw_call = OmniVM.call("__manifest", JSON.generate({op: "handle_call", id: @value["id"], key: value["key"], args: call_args.map { |arg| __omnivm_encode_arg(arg) }}))
        env_call = JSON.parse(raw_call)
        env_call.is_a?(Hash) && env_call["__omnivm_result__"] == true ? __omnivm_materialize_bridge_value(env_call["value"]) : raw_call
      end
    end
    if value.is_a?(Hash) && (
      value["__omnivm_resource__"] == true ||
      value["__omnivm_table__"] == true ||
      value["__omnivm_job__"] == true ||
      value["__omnivm_stream__"] == true ||
      value["__omnivm_channel__"] == true
    )
      return __omnivm_materialize_capture(value)
    end
    if value.is_a?(Array)
      return value.map { |item| __omnivm_materialize_bridge_value(item) }
    end
    if value.is_a?(Hash)
      return value.transform_values { |item| __omnivm_materialize_bridge_value(item) }
    end
    value
  end

  def [](key)
    local = __omnivm_local_value(key)
    return local unless local.equal?(OMNIVM_MISSING)
    report = __omnivm_record("index")
    if report.is_a?(Hash) && report["chatty"] == true
      __omnivm_materialize_chatty
      local = __omnivm_local_value(key)
      return local unless local.equal?(OMNIVM_MISSING)
    end
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_index", id: @value["id"], value: key}))
    env = JSON.parse(raw)
    env.is_a?(Hash) && env["__omnivm_result__"] == true ? __omnivm_materialize_bridge_value(env["value"]) : raw
  end

  def fetch(key, default = OMNIVM_MISSING, &block)
    marker = Object.new
    value = __omnivm_data_key_value(key, marker)
    return value unless value.equal?(marker)
    return block.call(key) if block_given?
    return default unless default.equal?(OMNIVM_MISSING)
    raise KeyError, "key not found: #{key.inspect}"
  end

  def omnivm_get(key)
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_get", id: @value["id"], key: key.to_s}))
    env = JSON.parse(raw)
    env.is_a?(Hash) && env["__omnivm_result__"] == true ? __omnivm_materialize_bridge_value(env["value"]) : raw
  end

  def omnivm_set(key, value)
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_set", id: @value["id"], key: key.to_s, value: __omnivm_encode_arg(value)}))
    env = JSON.parse(raw)
    if env.is_a?(Hash) && env["__omnivm_result__"] == true
      text_key = key.to_s
      @value[text_key] = value if __omnivm_local_key?(text_key)
      env["value"]
    else
      raw
    end
  end

  def omnivm_call(key, *args)
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_call", id: @value["id"], key: key.to_s, args: args.map { |arg| __omnivm_encode_arg(arg) }}))
    env = JSON.parse(raw)
    env.is_a?(Hash) && env["__omnivm_result__"] == true ? __omnivm_materialize_bridge_value(env["value"]) : raw
  end

  def omnivm_len
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_len", id: @value["id"]}))
    env = JSON.parse(raw)
    env.is_a?(Hash) && env["__omnivm_result__"] == true ? env["value"] : raw
  end

  def omnivm_iter(mode = "values")
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_iter", id: @value["id"], mode: mode.to_s}))
    env = JSON.parse(raw)
    env.is_a?(Hash) && env["__omnivm_result__"] == true ? __omnivm_materialize_bridge_value(env["value"]) : raw
  end

  def omnivm_keys
    omnivm_iter("keys")
  end

  def omnivm_values
    omnivm_iter("values")
  end

  def omnivm_items
    omnivm_iter("items")
  end

  def omnivm_contains(key)
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_contains", id: @value["id"], value: __omnivm_encode_arg(key)}))
    env = JSON.parse(raw)
    env.is_a?(Hash) && env["__omnivm_result__"] == true ? !!env["value"] : false
  end

  def omnivm_close
    return false if @__omnivm_closed == true
    OmniVM.call("__manifest", JSON.generate({op: "handle_release_finalizer", id: @value["id"]}))
    @__omnivm_closed = true
    begin
      ObjectSpace.undefine_finalizer(self)
    rescue
    end
    true
  end

  def []=(key, value)
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_set", id: @value["id"], key: key.to_s, value: __omnivm_encode_arg(value)}))
    env = JSON.parse(raw)
    if env.is_a?(Hash) && env["__omnivm_result__"] == true
      text_key = key.to_s
      @value[text_key] = value if __omnivm_local_key?(text_key)
      env["value"]
    else
      raw
    end
  end

  def key?(key)
    begin
      raw = OmniVM.call("__manifest", JSON.generate({op: "handle_contains", id: @value["id"], value: key}))
      env = JSON.parse(raw)
      return !!env["value"] if env.is_a?(Hash) && env["__omnivm_result__"] == true
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    __omnivm_record("property")
    __omnivm_local_key?(key)
  end

  alias include? key?

  def each(&block)
    return __omnivm_data_key_value("each") if !block_given? && __omnivm_data_key?("each")
    begin
      raw = OmniVM.call("__manifest", JSON.generate({op: "handle_iter", id: @value["id"], mode: "values"}))
      env = JSON.parse(raw)
      if env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"].is_a?(Array)
        return __omnivm_materialize_bridge_value(env["value"]).each(&block)
      end
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    __omnivm_record("iterate")
    @value.each(&block)
  end

  def keys
    return __omnivm_data_key_value("keys") if __omnivm_data_key?("keys")
    begin
      raw = OmniVM.call("__manifest", JSON.generate({op: "handle_iter", id: @value["id"], mode: "keys"}))
      env = JSON.parse(raw)
      return __omnivm_materialize_bridge_value(env["value"]) if env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"].is_a?(Array)
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    __omnivm_record("iterate")
    @value.keys
  end

  def values
    return __omnivm_data_key_value("values") if __omnivm_data_key?("values")
    begin
      raw = OmniVM.call("__manifest", JSON.generate({op: "handle_iter", id: @value["id"], mode: "values"}))
      env = JSON.parse(raw)
      return __omnivm_materialize_bridge_value(env["value"]) if env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"].is_a?(Array)
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    __omnivm_record("iterate")
    @value.values
  end

  def length
    return __omnivm_data_key_value("length") if __omnivm_data_key?("length")
    begin
      raw = OmniVM.call("__manifest", JSON.generate({op: "handle_len", id: @value["id"]}))
      env = JSON.parse(raw)
      return env["value"] if env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"].is_a?(Numeric)
    rescue => e
      raise unless __omnivm_missing_bridge_error?(e)
    end
    __omnivm_record("property")
    @value.length
  end

  def size
    return __omnivm_data_key_value("size") if __omnivm_data_key?("size")
    length
  end

  def then(*args, &block)
    return __omnivm_data_key_value("then") if args.empty? && !block_given? && __omnivm_data_key?("then")
    super
  end

  def to_h
    return __omnivm_data_key_value("to_h") if __omnivm_data_key?("to_h")
    @value.dup
  end

  def to_a
    return __omnivm_data_key_value("to_a") if __omnivm_data_key?("to_a")
    values
  end

  def to_json(*args)
    return __omnivm_data_key_value("to_json") if args.empty? && __omnivm_data_key?("to_json")
    @value.to_json(*args)
  end

  def method_missing(name, *args, &block)
    key = name.to_s
    if key.end_with?("=") && args.length == 1 && defined?(OmniVM) && OmniVM.respond_to?(:call)
      begin
        target_key = key[0...-1]
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_set", id: @value["id"], key: target_key, value: __omnivm_encode_arg(args[0])}))
        env = JSON.parse(raw)
        if env.is_a?(Hash) && env["__omnivm_result__"] == true
          @value[target_key] = args[0] if __omnivm_local_key?(target_key)
          return args[0]
        end
      rescue => e
        raise unless __omnivm_missing_bridge_error?(e)
      end
      super
    end
    if args.empty?
      marker = Object.new
      value = __omnivm_data_key_value(key, marker)
      return value unless value.equal?(marker)
    end
    if args.empty? && __omnivm_local_key?(key)
      __omnivm_local_value(key)
    elsif args.empty? && defined?(OmniVM) && OmniVM.respond_to?(:call)
      begin
        report = __omnivm_record("property")
        if report.is_a?(Hash) && report["chatty"] == true
          __omnivm_materialize_chatty
          local = __omnivm_local_value(key)
          return local unless local.equal?(OMNIVM_MISSING)
        end
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_get", id: @value["id"], key: key}))
        env = JSON.parse(raw)
        if env.is_a?(Hash) && env["__omnivm_result__"] == true
          return __omnivm_materialize_bridge_value(env["value"])
        end
      rescue => e
        raise unless __omnivm_missing_bridge_error?(e)
      end
      super
    elsif defined?(OmniVM) && OmniVM.respond_to?(:call)
      begin
        __omnivm_record("call")
        raw = OmniVM.call("__manifest", JSON.generate({op: "handle_call", id: @value["id"], key: key, args: args.map { |arg| __omnivm_encode_arg(arg) }}))
        env = JSON.parse(raw)
        return __omnivm_materialize_bridge_value(env["value"]) if env.is_a?(Hash) && env["__omnivm_result__"] == true
      rescue => e
        raise unless __omnivm_missing_bridge_error?(e)
      end
      super
    else
      super
    end
  end

  def respond_to_missing?(name, include_private = false)
    key = name.to_s
    key.end_with?("=") || __omnivm_local_key?(key) || super
  end
end

class OmniVMCallableHandleProxy < OmniVMHandleProxy
  def call(*args)
    raw = OmniVM.call("__manifest", JSON.generate({op: "handle_call", id: @value["id"], key: "", args: args.map { |arg| __omnivm_encode_arg(arg) }}))
    env = JSON.parse(raw)
    env.is_a?(Hash) && env["__omnivm_result__"] == true ? __omnivm_materialize_bridge_value(env["value"]) : raw
  end
end

def __omnivm_stream_chunk_value(value)
  if value.is_a?(Hash) && value["__omnivm_table__"] == true
    metadata = value["metadata"].is_a?(Hash) ? value["metadata"] : {}
    dtype = metadata.key?("dtype") ? metadata["dtype"] : value["dtype"]
    buffer_name = value["buffer"] || metadata["buffer"]
    byte_dtype = !dtype.nil? && [0, 5, 10, 11].include?(dtype.to_i)
    if byte_dtype && buffer_name && defined?(OmniVM) && OmniVM.respond_to?(:get_buffer)
      raw = OmniVM.get_buffer(buffer_name)
      if raw.is_a?(String)
        shape = metadata["shape"].is_a?(Array) ? metadata["shape"] : []
        length = shape.empty? ? raw.bytesize : shape[0].to_i
        raw_offset = metadata.key?("offset") ? metadata["offset"] : value["offset"]
        offset = raw_offset.nil? ? 0 : raw_offset.to_i
        strides = metadata["strides"].is_a?(Array) ? metadata["strides"] : []
        stride = strides.empty? ? 1 : strides[0].to_i
        length = 0 if length < 0
        offset = 0 if offset < 0
        stride = 1 if stride == 0
        return "".b if length == 0
        if stride == 1
          return (raw.byteslice(offset, length) || "".b).b
        end
        bytes = raw.bytes
        out = []
        length.times do |i|
          src = offset + i * stride
          out << bytes[src] if src >= 0 && src < bytes.length
        end
        return out.pack("C*").b
      end
    end
  end
  __omnivm_materialize_capture(value)
end

class OmniVMStreamProxy
  include Enumerable

  def initialize(value)
    @value = value
    @__omnivm_closed = false
    id = @value["id"]
    if !id.nil?
      @value["transfer"] == true ? OmniVMHandleProxy.omnivm_adopt(id) : OmniVMHandleProxy.omnivm_retain(id)
    end
    ObjectSpace.define_finalizer(self, OmniVMHandleProxy.omnivm_finalizer(id)) unless id.nil?
  end

  def __omnivm_mark_closed
    return false if @__omnivm_closed == true
    @__omnivm_closed = true
    begin
      ObjectSpace.undefine_finalizer(self)
    rescue
    end
    true
  end

  def each
    return enum_for(:each) unless block_given?
    loop do
      raw = OmniVM.call("__manifest", JSON.generate({op: "stream_next", id: @value["id"]}))
      env = JSON.parse(raw)
      item = env.is_a?(Hash) && env["__omnivm_result__"] == true ? env["value"] : {"done" => true}
      if item.nil? || item["done"] == true
        __omnivm_mark_closed
        break
      end
      yield __omnivm_stream_chunk_value(item["value"])
    end
  end

  def close
    return false if @__omnivm_closed == true
    OmniVM.call("__manifest", JSON.generate({op: "stream_cancel", id: @value["id"]}))
    __omnivm_mark_closed
  end

  def omnivm_close
    close
  end
end

def __omnivm_materialize_capture(value)
  if value.is_a?(Hash) && (
    value["__omnivm_stream__"] == true ||
    value["__omnivm_channel__"] == true
  )
    return __omnivm_cached_proxy("stream", value) { OmniVMStreamProxy.new(value) }
  end
  if value.is_a?(Hash) && (
    value["__omnivm_resource__"] == true ||
    value["__omnivm_table__"] == true ||
    value["__omnivm_job__"] == true
  )
    return __omnivm_cached_proxy("handle", value) { value["kind"] == "callable" ? OmniVMCallableHandleProxy.new(value) : OmniVMHandleProxy.new(value) }
  end
  if value.is_a?(Array)
    return value.map { |item| __omnivm_materialize_capture(item) }
  end
  if value.is_a?(Hash)
    return value.transform_values { |item| __omnivm_materialize_capture(item) }
  end
  value
end`
}

// injectJavaCaptures generates Java code to set captures via OmniVM.
func injectJavaCaptures(captures map[string]string) string {
	return javaCaptureInjection(captures).setup
}

func javaCaptureInjection(captures map[string]string) captureInjection {
	var lines []string
	var names []string
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("omnivm.OmniVM.setCapture(\"%s\", \"%s\");",
			escapeJavaString(varName), escapeJavaString(jsonVal)))
		names = append(names, varName)
	}
	sort.Strings(names)
	return captureInjection{setup: strings.Join(lines, "\n"), javaCaptureNames: names}
}

// crossRuntimeSerialize asks the source runtime to JSON-serialize a variable
// for explicit bridge transforms. Ordinary complex values should have already
// selected a proxy, stream, or Arrow boundary before this path.
func (e *Executor) crossRuntimeSerialize(ref RuntimeRef) (string, error) {
	srcRT, ok := e.runtimes[ref.Runtime]
	if !ok {
		return "", fmt.Errorf("source runtime %q not found", ref.Runtime)
	}

	var jsonCode string
	switch ref.Runtime {
	case "python":
		jsonCode = fmt.Sprintf("__import__('json').dumps(%s)", runtimeVarRef(ref.Runtime, ref.VarName))
	case "javascript":
		jsonCode = fmt.Sprintf("JSON.stringify(%s)", runtimeVarRef(ref.Runtime, ref.VarName))
	case "ruby":
		jsonCode = fmt.Sprintf("require 'json'; JSON.generate(%s)", runtimeVarRef(ref.Runtime, ref.VarName))
	case "java":
		jsonCode = runtimeSerializeExpr(ref.Runtime, runtimeVarRef(ref.Runtime, ref.VarName))
	default:
		return "", fmt.Errorf("cross-runtime serialize not supported for %q", ref.Runtime)
	}

	result := srcRT.Eval(jsonCode)
	if result.Err != nil {
		return "", result.Err
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.RuntimeSerializations++
	})

	val := result.Value
	if val == nil {
		if result.Output != "" {
			return strings.TrimRight(result.Output, "\n"), nil
		}
		return "", fmt.Errorf("no value returned")
	}

	// The eval result is the JSON string itself — return it directly
	// (strip surrounding quotes if the eval wrapped it as a string)
	s := fmt.Sprintf("%v", val)
	if s == "" || s == "undefined" {
		return "", fmt.Errorf("source runtime returned no JSON")
	}
	return s, nil
}

func (e *Executor) runtimeRefProxyCaptureJSON(ref RuntimeRef) (string, error) {
	if id, ok := e.bridgeHandleForValue(ref.Runtime, ref); ok {
		descriptor, err := e.handleDescriptorValue(id)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(descriptor)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}

	kind := "runtime_ref"
	if ref.CallableKnown && ref.Callable {
		kind = "callable"
	} else if collectionKind, ok := e.runtimeRefCollectionKind(ref); ok {
		kind = collectionKind
	}
	resource := &ResourceRef{
		Runtime: ref.Runtime,
		Kind:    kind,
		Value:   ref,
	}
	var id handles.ID
	id, err := e.ensureHandleTable().Register(resource, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    resource.Kind,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			resource.Closed = true
			e.forgetReleasedHandle(id, resource)
			return e.releaseRuntimeArgRef(ref)
		},
	})
	if err != nil {
		return "", err
	}
	resource.ID = id
	e.resources[id] = resource
	if ident, ok := bridgeIdentityForValue(ref); ok {
		e.bridgeHandles[ident] = id
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.ResourceProxyCaptures++
	})
	return marshalResourceProxy(resource)
}

func (e *Executor) runtimeRefCollectionKind(ref RuntimeRef) (string, bool) {
	if ref.Runtime == "" || ref.VarName == "" {
		return "", false
	}
	expr, ok := runtimeRefCollectionKindExpr(ref)
	if !ok {
		return "", false
	}
	value, err := e.runtimeRefEvalPrimitive(ref, expr)
	if err != nil {
		return "", false
	}
	kind, _ := value.(string)
	switch kind {
	case "sequence", "mapping":
		return kind, true
	default:
		return "", false
	}
}

func runtimeRefCollectionKindExpr(ref RuntimeRef) (string, bool) {
	base := runtimeVarRef(ref.Runtime, ref.VarName)
	switch ref.Runtime {
	case "javascript":
		return fmt.Sprintf(`(function(__v){ if (Array.isArray(__v) || (typeof Set !== "undefined" && __v instanceof Set)) return "sequence"; if (__v && (typeof Map !== "undefined" && __v instanceof Map)) return "mapping"; if (__v && typeof __v === "object" && !(typeof ArrayBuffer !== "undefined" && (__v instanceof ArrayBuffer || ArrayBuffer.isView(__v))) && (Object.getPrototypeOf(__v) === Object.prototype || Object.getPrototypeOf(__v) === null)) { var __keys = Object.keys(__v); for (var __i = 0; __i < __keys.length; __i++) { if (typeof __v[__keys[__i]] === "function") return ""; } return "mapping"; } return ""; })(%s)`, base), true
	case "python":
		return fmt.Sprintf(`(lambda __v, __abc: "mapping" if isinstance(__v, __abc.Mapping) else ("sequence" if (isinstance(__v, (__abc.Sequence, __abc.Set)) and not isinstance(__v, (str, bytes, bytearray, memoryview))) else ""))(%s, __import__("collections.abc", fromlist=["Mapping", "Sequence", "Set"]))`, base), true
	case "ruby":
		return fmt.Sprintf(`(begin; __v = %s; __v.is_a?(Hash) ? "mapping" : (__v.is_a?(Array) || (defined?(Set) && __v.is_a?(Set)) ? "sequence" : ""); end)`, base), true
	case "java":
		return fmt.Sprintf(`((%s instanceof java.util.Map) ? "mapping" : (((%s instanceof java.util.Collection) || (%s != null && %s.getClass().isArray())) ? "sequence" : ""))`, base, base, base, base), true
	default:
		return "", false
	}
}

func (e *Executor) releaseRuntimeArgRef(ref RuntimeRef) error {
	key, ok := runtimeArgRefKey(ref.VarName)
	if !ok {
		return nil
	}
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return nil
	}
	code, ok := runtimeArgRefReleaseCode(ref.Runtime, key)
	if !ok {
		return nil
	}
	result := rt.Execute(code)
	if result.Err != nil {
		return result.Err
	}
	return nil
}

func runtimeArgRefKey(varName string) (string, bool) {
	varName = strings.TrimPrefix(varName, "globalThis.")
	for _, prefix := range []string{"__omnivm_arg_refs[", "$__omnivm_arg_refs["} {
		if !strings.HasPrefix(varName, prefix) || !strings.HasSuffix(varName, "]") {
			continue
		}
		lit := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(varName, prefix), "]"))
		if lit == "" {
			return "", false
		}
		if strings.HasPrefix(lit, `"`) {
			key, err := strconv.Unquote(lit)
			return key, err == nil
		}
		if strings.HasPrefix(lit, "'") && strings.HasSuffix(lit, "'") && len(lit) >= 2 {
			key := strings.TrimSuffix(strings.TrimPrefix(lit, "'"), "'")
			key = strings.ReplaceAll(key, `\'`, `'`)
			key = strings.ReplaceAll(key, `\\`, `\`)
			return key, true
		}
	}
	return "", false
}

func runtimeArgRefReleaseCode(runtimeName, key string) (string, bool) {
	keyLit := jsonStringLiteral(key)
	switch runtimeName {
	case "javascript":
		return fmt.Sprintf("if (globalThis.__omnivm_arg_refs) { delete globalThis.__omnivm_arg_refs[%s]; }", keyLit), true
	case "python":
		return fmt.Sprintf(`globals().get("__omnivm_arg_refs", {}).pop(%s, None)`, keyLit), true
	case "ruby":
		return fmt.Sprintf(`($__omnivm_arg_refs || {}).delete(%s)`, keyLit), true
	case "java":
		return fmt.Sprintf(`omnivm.OmniVM.releaseArgRef(%s);`, keyLit), true
	default:
		return "", false
	}
}

func (e *Executor) resolveRuntimeRefCapture(binding, targetRuntime string, ref RuntimeRef) (string, error) {
	if ref.SnapshotKnown && !ref.Opaque && isBridgePrimitive(ref.Value) {
		if _, rubyString := ref.Value.(string); !(ref.Runtime == "ruby" && rubyString) {
			if jsonVal, ok, err := e.knownPrimitiveRuntimeRefCaptureJSON(binding, targetRuntime, ref); ok || err != nil {
				return jsonVal, err
			}
		}
	}
	if jsonVal, ok, err := e.runtimeRefBulkTableCaptureJSON(binding, targetRuntime, ref); ok || err != nil {
		return jsonVal, err
	}
	if _, rubyString := ref.Value.(string); !(ref.Runtime == "ruby" && rubyString) {
		if jsonVal, ok, err := e.knownPrimitiveRuntimeRefCaptureJSON(binding, targetRuntime, ref); ok || err != nil {
			return jsonVal, err
		}
	}
	if jsonVal, ok, err := e.runtimeRefStreamCaptureJSON(binding, targetRuntime, ref); ok || err != nil {
		return jsonVal, err
	}
	if e.shouldProxyRuntimeRefCapture(binding, targetRuntime, ref) {
		return e.runtimeRefProxyCaptureJSON(ref)
	}
	if jsonVal, ok, err := e.unknownRuntimeRefPrimitiveCaptureJSON(binding, targetRuntime, ref); ok || err != nil {
		return jsonVal, err
	}

	jsonVal, err := e.crossRuntimeSerialize(ref)
	if err != nil {
		if ref.Runtime != "" && ref.VarName != "" {
			return e.runtimeRefProxyCaptureJSON(ref)
		}
		if ref.Value == nil || !isBridgePrimitive(ref.Value) {
			return e.runtimeRefProxyCaptureJSON(ref)
		}
		fallback, fallbackErr := e.marshalForCapture(ref.Value)
		if fallbackErr != nil {
			return "", fmt.Errorf("marshal fallback after serialize error %v: %w", err, fallbackErr)
		}
		jsonVal = fallback
	} else if e.isAmbiguousBoundary(binding, ref.Runtime, targetRuntime, jsonVal) {
		if !e.hasBridgeOps(binding, ref.Runtime, targetRuntime) {
			return e.runtimeRefProxyCaptureJSON(ref)
		}
	}

	jsonVal, err = e.applyBridgeOpsJSON(binding, ref.Runtime, targetRuntime, jsonVal)
	if err != nil {
		return "", fmt.Errorf("bridge: %w", err)
	}
	return jsonVal, nil
}

func (e *Executor) runtimeRefStreamCaptureJSON(binding, targetRuntime string, ref RuntimeRef) (string, bool, error) {
	if ref.Runtime == "" || ref.VarName == "" || e.hasBridgeOps(binding, ref.Runtime, targetRuntime) {
		return "", false, nil
	}
	stream, err := e.runtimeRefIsStream(ref)
	if err != nil || !stream {
		return "", false, nil
	}
	id, err := e.runtimeRefStreamHandle(ref)
	if err != nil {
		return "", true, err
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.StreamProxyCaptures++
	})
	return streamCaptureJSON(id, ref.Runtime, "stream"), true, nil
}

func (e *Executor) unknownRuntimeRefPrimitiveCaptureJSON(binding, targetRuntime string, ref RuntimeRef) (string, bool, error) {
	if ref.Runtime == "" || ref.VarName == "" || ref.SnapshotKnown || e.hasBridgeOps(binding, ref.Runtime, targetRuntime) {
		return "", false, nil
	}
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return "", false, nil
	}
	result := rt.Eval(runtimePrimitiveSnapshotExpr(ref.Runtime, runtimeVarRef(ref.Runtime, ref.VarName)))
	if result.Err != nil {
		jsonVal, proxyErr := e.runtimeRefProxyCaptureJSON(ref)
		return jsonVal, true, proxyErr
	}
	raw := result.Output
	if result.Value != nil {
		raw = fmt.Sprintf("%v", result.Value)
	}
	var snapshot runtimePrimitiveSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		jsonVal, proxyErr := e.runtimeRefProxyCaptureJSON(ref)
		return jsonVal, true, proxyErr
	}
	if !snapshot.Primitive {
		ref.CallableKnown = true
		ref.Callable = snapshot.Callable
		ref.CallableShape = snapshot.CallableShape
		jsonVal, err := e.runtimeRefProxyCaptureJSON(ref)
		return jsonVal, true, err
	}
	jsonVal, err := e.marshalForCapture(snapshot.Value)
	if err != nil {
		return "", true, err
	}
	return jsonVal, true, nil
}

func (e *Executor) runtimeRefStreamHandle(ref RuntimeRef) (handles.ID, error) {
	if id, ok := e.bridgeHandleForValue(ref.Runtime, ref); ok {
		return id, nil
	}
	var id handles.ID
	id, err := e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    "stream",
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			if err := e.closeRuntimeRefStream(id, ref); err != nil {
				return err
			}
			e.rememberReleasedStream(id, ref.Runtime, "stream")
			e.forgetReleasedHandle(id, ref)
			return nil
		},
	})
	if err != nil {
		return 0, err
	}
	if ident, ok := bridgeIdentityForValue(ref); ok {
		e.bridgeHandles[ident] = id
	}
	return id, nil
}

func (e *Executor) runtimeRefBulkTableCaptureJSON(binding, targetRuntime string, ref RuntimeRef) (string, bool, error) {
	if ref.Runtime == "" || ref.VarName == "" || e.hasBridgeOps(binding, ref.Runtime, targetRuntime) {
		return "", false, nil
	}
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return "", false, nil
	}
	exporter, ok := rt.(pkg.BufferExporter)
	if !ok {
		return "", false, nil
	}

	e.nextRuntimeRefID++
	name := fmt.Sprintf("__omnivm_auto_runtime_buffer_%p_%d", e, e.nextRuntimeRefID)
	exported, ok, err := exporter.ExportBuffer(name, runtimeVarRef(ref.Runtime, ref.VarName))
	if err != nil || !ok {
		return "", ok, err
	}

	dtype := exported.Dtype
	var nullCount *int64
	if exported.NullCount > 0 {
		value := exported.NullCount
		nullCount = &value
	}
	shape := append([]int64(nil), exported.Shape...)
	if len(shape) == 0 {
		shape = []int64{exported.Elements}
	}
	table := &TableRef{
		Runtime:   ref.Runtime,
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: exported.ArrowFormat,
			Buffer:      exported.Name,
			Shape:       shape,
			Strides:     append([]int64(nil), exported.Strides...),
			Offset:      exported.Offset,
			NullCount:   nullCount,
			ReadOnly:    exported.ReadOnly,
		},
		Value: exported.Name,
	}
	var id handles.ID
	id, err = e.ensureHandleTable().Register(table, handles.RegisterOptions{
		Runtime: table.Runtime,
		Kind:    "table:" + table.Format,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			table.Released = true
			e.forgetReleasedHandle(id, table)
			if err := arrow.GlobalStore().Free(exported.Name); err != nil {
				return err
			}
			return e.releaseRuntimeArgRef(ref)
		},
	})
	if err != nil {
		_ = arrow.GlobalStore().Free(exported.Name)
		return "", true, err
	}
	table.ID = id
	e.tables[id] = table
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.TableProxyCaptures++
		stats.ArrowTransfers++
	})
	jsonVal, err := marshalTableProxy(table)
	if err != nil {
		_ = e.ensureHandleTable().Release(id)
		return "", true, err
	}
	return jsonVal, true, nil
}

func (e *Executor) knownPrimitiveRuntimeRefCaptureJSON(binding, targetRuntime string, ref RuntimeRef) (string, bool, error) {
	if !ref.SnapshotKnown || ref.Opaque || !isBridgePrimitive(ref.Value) {
		return "", false, nil
	}
	jsonVal, err := e.marshalForCapture(ref.Value)
	if err != nil {
		return "", true, err
	}
	jsonVal, err = e.applyBridgeOpsJSON(binding, ref.Runtime, targetRuntime, jsonVal)
	if err != nil {
		return "", true, fmt.Errorf("bridge: %w", err)
	}
	return jsonVal, true, nil
}

func (e *Executor) shouldProxyRuntimeRefCapture(binding, targetRuntime string, ref RuntimeRef) bool {
	if ref.Runtime == "" || ref.VarName == "" || e.hasBridgeOps(binding, ref.Runtime, targetRuntime) {
		return false
	}
	if ref.Opaque {
		return true
	}
	if ref.Value == nil || isBridgePrimitive(ref.Value) {
		return false
	}
	return true
}

func (e *Executor) isAmbiguousBoundary(binding, from, to, jsonVal string) bool {
	return e.serializedBoundaryDecision(binding, from, to, jsonVal).Fallback
}

func (e *Executor) hasBridgeOps(binding, from, to string) bool {
	if len(e.bridgeOps) == 0 {
		return false
	}
	ops := e.bridgeOps[bridgeKey(binding, from, to)]
	return len(ops) > 0
}

func (e *Executor) serializedBoundaryDecision(binding, from, to, jsonVal string) boundaryDecision {
	var ops []*BridgeOp
	if len(e.bridgeOps) > 0 {
		ops = e.bridgeOps[bridgeKey(binding, from, to)]
	}
	return classifySerializedBoundary(jsonVal, ops)
}

func (e *Executor) warnBoundaryFallback(binding, from, to, reason string) {
	key := binding + "|" + from + "|" + to + "|" + reason
	if e.boundaryWarnings == nil {
		e.boundaryWarnings = make(map[string]struct{})
	}
	if _, seen := e.boundaryWarnings[key]; seen {
		return
	}
	e.boundaryWarnings[key] = struct{}{}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.BoundaryWarnings++
	})
	fmt.Fprintf(os.Stderr, "warning: cross-runtime capture %q from %s to %s has ambiguous boundary semantics: %s. Add an explicit bridge op or type metadata to make the contract enforceable.\n", binding, from, to, reason)
}

func (e *Executor) recordJSONFallback(reason string) {
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.JSONFallbacks++
		stats.LastJSONFallbackReason = reason
	})
}

// applyBridgeOpsJSON looks up bridge ops for a binding crossing from→to,
// applies them to the JSON-encoded value, and returns the transformed JSON.
// If no bridge ops exist for this crossing, returns the value unchanged.
func (e *Executor) applyBridgeOpsJSON(binding, from, to, jsonVal string) (string, error) {
	if len(e.bridgeOps) == 0 {
		return jsonVal, nil
	}

	key := bridgeKey(binding, from, to)
	ops, ok := e.bridgeOps[key]
	if !ok || len(ops) == 0 {
		return jsonVal, nil
	}

	// Deserialize, apply bridges, re-serialize
	var val interface{}
	if err := json.Unmarshal([]byte(jsonVal), &val); err != nil {
		return "", fmt.Errorf("bridge input for %q from %s to %s is not JSON: %w", binding, from, to, err)
	}

	for _, op := range ops {
		var err error
		val, err = applyBridge(op, val)
		if err != nil {
			return "", err
		}
		switch op.Op {
		case "share_memory":
			e.addBoundaryStat(func(stats *BoundaryStats) {
				stats.ArrowTransfers++
			})
		case "stream_proxy", "channel_bridge":
			e.addBoundaryStat(func(stats *BoundaryStats) {
				stats.StreamProxyCaptures++
			})
		case "proxy_with_finalizer", "attach_disposer", "proxy_callable":
			e.addBoundaryStat(func(stats *BoundaryStats) {
				stats.ProxyCaptures++
			})
		}
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.BridgeTransforms += int64(len(ops))
	})

	result, err := json.Marshal(val)
	if err != nil {
		return "", fmt.Errorf("bridge re-serialize: %w", err)
	}
	return string(result), nil
}

// String escaping helpers

func escapePythonString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	return s
}

func escapeRubyString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "\\'")
	return s
}

func escapeJavaString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}
