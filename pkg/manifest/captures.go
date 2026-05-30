package manifest

import (
	"encoding/json"
	"fmt"
	"strings"
)

// drainChannel performs a non-blocking drain of a channel into a slice.
// Returns all currently buffered values without blocking.
// Handles closed channels safely by checking the ok flag.
func drainChannel(ch *ChanRef) []interface{} {
	result := make([]interface{}, 0)
	if ch.closed {
		return result
	}
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
			return "", fmt.Errorf("capture %q: undefined binding %q", varName, bindingName)
		}

		// ChanRef: drain buffered values into an array
		if ch, ok := val.(*ChanRef); ok {
			drained := drainChannel(ch)
			jsonVal, err := json.Marshal(drained)
			if err != nil {
				return "", fmt.Errorf("capture %q: marshal ChanRef: %w", varName, err)
			}
			resolved[varName] = string(jsonVal)
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
				// Same runtime — variable already in scope, skip injection
				continue
			}
			// Cross-runtime: ask the source runtime to JSON-serialize the value
			// so complex types (lists, dicts, arrays) transfer correctly.
			jsonVal, err := e.crossRuntimeSerialize(ref)
			if err != nil {
				// Fallback to cached value
				jsonVal, err = marshalForCapture(ref.Value)
				if err != nil {
					return "", fmt.Errorf("capture %q: marshal RuntimeRef: %w", varName, err)
				}
			}
			// Apply bridge ops for this crossing
			jsonVal, err = e.applyBridgeOpsJSON(bindingName, ref.Runtime, rtName, jsonVal)
			if err != nil {
				return "", fmt.Errorf("capture %q: bridge: %w", varName, err)
			}
			resolved[varName] = jsonVal
			continue
		}

		jsonVal, err := marshalForCapture(val)
		if err != nil {
			return "", fmt.Errorf("capture %q: marshal: %w", varName, err)
		}
		resolved[varName] = jsonVal
	}

	// If all captures were skipped (e.g. all ImportRefs), return code as-is
	if len(resolved) == 0 {
		return code, nil
	}

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

	// Build locals dict with captures
	var pairs []string
	for varName, jsonVal := range captures {
		pairs = append(pairs, fmt.Sprintf("'%s': __json.loads('%s')",
			escapePythonString(varName),
			escapePythonString(jsonVal)))
	}
	lines = append(lines, fmt.Sprintf("__captures = {%s}", strings.Join(pairs, ", ")))

	// Inject captures as local variables, then run the user's code.
	// We assign each capture into the global scope so eval() can see it.
	for varName := range captures {
		lines = append(lines, fmt.Sprintf("%s = __captures['%s']", varName, escapePythonString(varName)))
	}

	lines = append(lines, code)
	return strings.Join(lines, "\n")
}

// wrapJavaScriptCaptures wraps code in an IIFE with captures as parameters.
func wrapJavaScriptCaptures(code string, captures map[string]string) string {
	if len(captures) == 0 {
		return code
	}

	var paramNames []string
	var paramVals []string
	for varName, jsonVal := range captures {
		paramNames = append(paramNames, varName)
		paramVals = append(paramVals, jsonVal)
	}

	return fmt.Sprintf("(function(%s) { %s\n})(%s)",
		strings.Join(paramNames, ", "),
		code,
		strings.Join(paramVals, ", "))
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
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("$%s = JSON.parse('%s')",
			varName,
			escapeRubyString(jsonVal)))
	}
	// Also assign to local aliases so user code can reference without $
	for varName := range captures {
		lines = append(lines, fmt.Sprintf("%s = $%s", varName, varName))
	}
	lines = append(lines, code)
	return strings.Join(lines, "\n")
}

// wrapJavaCaptures injects captures via OmniVM.setCapture() calls before the code.
// Java code accesses captures via omnivm.OmniVM.getCapture("name").
func wrapJavaCaptures(code string, captures map[string]string) string {
	if len(captures) == 0 {
		return code
	}

	var lines []string
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("omnivm.OmniVM.setCapture(\"%s\", \"%s\");",
			escapeJavaString(varName),
			escapeJavaString(jsonVal)))
	}
	lines = append(lines, code)
	return strings.Join(lines, "\n")
}

// autoInjectScope injects all serializable bindings from the current scope
// into the given runtime. Used for conditions without explicit captures.
func (e *Executor) autoInjectScope(rtName string) string {
	resolved := make(map[string]string)
	// Walk the scope stack top-down, collecting serializable bindings
	for i := len(e.scopes) - 1; i >= 0; i-- {
		for varName, val := range e.scopes[i] {
			if _, already := resolved[varName]; already {
				continue // shadowed by higher scope
			}
			if _, ok := val.(*ChanRef); ok {
				continue // Auto-inject skips channels; use explicit captures to drain
			}
			if _, ok := val.(ImportRef); ok {
				continue
			}
			if ref, ok := val.(RuntimeRef); ok {
				if ref.Runtime == rtName {
					continue // already in scope
				}
				jsonVal, err := e.crossRuntimeSerialize(ref)
				if err != nil {
					jsonVal, err = marshalForCapture(ref.Value)
					if err != nil {
						continue
					}
				}
				// Apply bridge ops for this crossing
				jsonVal, err = e.applyBridgeOpsJSON(varName, ref.Runtime, rtName, jsonVal)
				if err != nil {
					continue
				}
				resolved[varName] = jsonVal
				continue
			}
			jsonVal, err := marshalForCapture(val)
			if err != nil {
				continue
			}
			resolved[varName] = jsonVal
		}
	}
	if len(resolved) == 0 {
		return ""
	}
	switch rtName {
	case "python":
		return injectPythonCaptures(resolved)
	case "javascript":
		return injectJSCaptures(resolved)
	case "ruby":
		return injectRubyCaptures(resolved)
	case "java":
		return injectJavaCaptures(resolved)
	default:
		return ""
	}
}

// buildCaptureInjection generates capture setup code without appending the user's code.
// Used by opEval which needs captures and assignment as separate steps.
func (e *Executor) buildCaptureInjection(rtName string, captures map[string]string) string {
	resolved := make(map[string]string)
	for varName, bindingName := range captures {
		val, ok := e.getBinding(bindingName)
		if !ok {
			continue
		}
		if ch, ok := val.(*ChanRef); ok {
			drained := drainChannel(ch)
			jsonVal, merr := json.Marshal(drained)
			if merr != nil {
				continue
			}
			resolved[varName] = string(jsonVal)
			continue
		}
		if _, ok := val.(ImportRef); ok {
			continue
		}
		if ref, ok := val.(RuntimeRef); ok {
			if ref.Runtime == rtName {
				continue
			}
			jsonVal, err := e.crossRuntimeSerialize(ref)
			if err != nil {
				jsonVal, err = marshalForCapture(ref.Value)
				if err != nil {
					continue
				}
			}
			// Apply bridge ops for this crossing
			jsonVal, err = e.applyBridgeOpsJSON(bindingName, ref.Runtime, rtName, jsonVal)
			if err != nil {
				continue
			}
			resolved[varName] = jsonVal
			continue
		}
		jsonVal, err := marshalForCapture(val)
		if err != nil {
			continue
		}
		resolved[varName] = jsonVal
	}

	if len(resolved) == 0 {
		return ""
	}

	switch rtName {
	case "python":
		return injectPythonCaptures(resolved)
	case "javascript":
		return injectJSCaptures(resolved)
	case "ruby":
		return injectRubyCaptures(resolved)
	case "java":
		return injectJavaCaptures(resolved)
	default:
		return ""
	}
}

// injectPythonCaptures generates Python code to set capture variables (no user code).
func injectPythonCaptures(captures map[string]string) string {
	var lines []string
	lines = append(lines, "import json as __json")
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("%s = __json.loads('%s')",
			varName, escapePythonString(jsonVal)))
	}
	return strings.Join(lines, "\n")
}

// injectJSCaptures generates JS code to set capture variables as globals.
func injectJSCaptures(captures map[string]string) string {
	var lines []string
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("globalThis.%s = %s;", varName, jsonVal))
	}
	return strings.Join(lines, "\n")
}

// injectRubyCaptures generates Ruby code to set capture variables as globals.
// Ruby local variables are scoped to the eval context and don't persist across
// separate Execute()/Eval() calls. We use global variables ($var) so they
// persist, matching how Python uses module globals and JS uses globalThis.
func injectRubyCaptures(captures map[string]string) string {
	var lines []string
	lines = append(lines, "require 'json'")
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("$%s = JSON.parse('%s')",
			varName, escapeRubyString(jsonVal)))
	}
	return strings.Join(lines, "\n")
}

// injectJavaCaptures generates Java code to set captures via OmniVM.
func injectJavaCaptures(captures map[string]string) string {
	var lines []string
	for varName, jsonVal := range captures {
		lines = append(lines, fmt.Sprintf("omnivm.OmniVM.setCapture(\"%s\", \"%s\");",
			escapeJavaString(varName), escapeJavaString(jsonVal)))
	}
	return strings.Join(lines, "\n")
}

// crossRuntimeSerialize asks the source runtime to JSON-serialize a variable
// so complex types (lists, dicts, arrays, objects) transfer correctly across runtimes.
func (e *Executor) crossRuntimeSerialize(ref RuntimeRef) (string, error) {
	srcRT, ok := e.runtimes[ref.Runtime]
	if !ok {
		return "", fmt.Errorf("source runtime %q not found", ref.Runtime)
	}

	var jsonCode string
	switch ref.Runtime {
	case "python":
		jsonCode = fmt.Sprintf("__import__('json').dumps(%s)", ref.VarName)
	case "javascript":
		jsonCode = fmt.Sprintf("JSON.stringify(%s)", ref.VarName)
	case "ruby":
		jsonCode = fmt.Sprintf("require 'json'; JSON.generate(%s)", ref.VarName)
	default:
		return "", fmt.Errorf("cross-runtime serialize not supported for %q", ref.Runtime)
	}

	result := srcRT.Eval(jsonCode)
	if result.Err != nil {
		return "", result.Err
	}

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
		return jsonVal, nil // can't parse — pass through
	}

	for _, op := range ops {
		var err error
		val, err = applyBridge(op, val)
		if err != nil {
			return "", err
		}
	}

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
