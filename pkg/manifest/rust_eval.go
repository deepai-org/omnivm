package manifest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const arrowIPCMarkerKey = "__omnivm_arrow_ipc_b64__"

// evalRustCode handles eval/exec ops with runtime:"rust": a call expression
// like `heavy_stats(df)` or `classify(df.to_dict("records"))` dispatches to
// the unit export registered by the rust func_def. Arguments may be manifest
// bindings, literals, or expressions rooted in another runtime's binding —
// those evaluate in their owner runtime first, with DataFrames crossing as
// Arrow IPC tables.
func (e *Executor) evalRustCode(op *Op) (interface{}, error) {
	code := strings.TrimSpace(op.Code)
	parenIdx := strings.Index(code, "(")
	if parenIdx <= 0 || !strings.HasSuffix(code, ")") {
		return nil, fmt.Errorf("eval rust: cannot parse expression %q (only registered-function calls are supported at the top level)", code)
	}
	funcName := strings.TrimSpace(code[:parenIdx])
	if _, ok := e.goFuncs[funcName]; !ok {
		return nil, fmt.Errorf("eval rust: unknown function %q (no rust func_def registered it)", funcName)
	}

	argsStr := code[parenIdx+1 : len(code)-1]
	var args []interface{}
	for _, expr := range splitTopLevelArgs(argsStr) {
		value, err := e.resolveRustArg(expr)
		if err != nil {
			return nil, fmt.Errorf("eval rust %s: argument %q: %w", funcName, expr, err)
		}
		args = append(args, value)
	}

	value, err := e.callGoFunc(funcName, args, "")
	if err != nil {
		return nil, err
	}
	return e.bindRustResult(op.Bind, value)
}

// bindRustResult stores the result; Arrow IPC table results materialize as a
// polars DataFrame in Python (the tabular consumer) and bind as a runtime ref
// so later Python code uses them natively (e.g. `stats.height`).
func (e *Executor) bindRustResult(bind string, value interface{}) (interface{}, error) {
	if marker, ok := arrowIPCPayload(value); ok {
		if pyRT, hasPy := e.runtimes["python"]; hasPy && bind != "" {
			setup := fmt.Sprintf(`
import base64 as __omnivm_b64, io as __omnivm_io, polars as __omnivm_pl
%s = __omnivm_pl.read_ipc_stream(__omnivm_io.BytesIO(__omnivm_b64.b64decode(%q)))
`, bind, marker)
			result := pyRT.Execute(setup)
			if result.Err != nil {
				return nil, fmt.Errorf("eval rust: materializing table %q in python: %w", bind, result.Err)
			}
			ref, _, err := e.boundRuntimeRefSnapshot("python", bind)
			if err != nil {
				return nil, fmt.Errorf("eval rust: snapshot of %q: %w", bind, err)
			}
			e.setBinding(bind, ref)
			return ref, nil
		}
	}
	if bind != "" {
		e.setBinding(bind, value)
	}
	return value, nil
}

func arrowIPCPayload(value interface{}) (string, bool) {
	m, ok := value.(map[string]interface{})
	if !ok {
		return "", false
	}
	payload, ok := m[arrowIPCMarkerKey].(string)
	return payload, ok && payload != ""
}

// resolveRustArg evaluates one argument expression.
func (e *Executor) resolveRustArg(expr string) (interface{}, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, nil
	}
	// Literals.
	switch expr {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null", "None", "nil":
		return nil, nil
	}
	if f, err := strconv.ParseFloat(expr, 64); err == nil {
		return f, nil
	}
	if len(expr) >= 2 && (expr[0] == '"' || expr[0] == '\'') && expr[len(expr)-1] == expr[0] {
		return expr[1 : len(expr)-1], nil
	}
	if expr[0] == '[' || expr[0] == '{' {
		var out interface{}
		if err := json.Unmarshal([]byte(expr), &out); err == nil {
			return out, nil
		}
	}

	// Bare identifier bound in the manifest scope.
	root := identifierRoot(expr)
	binding, bound := e.getBinding(root)
	if root == expr && bound {
		if ref, isRef := binding.(RuntimeRef); isRef {
			// Callables cross as runtime-ref markers (Rust wraps them as
			// omnivm::Callback); data refs evaluate in their owner runtime.
			if ref.CallableKnown && ref.Callable {
				return encodeCSharedHandlePayloadValue(ref), nil
			}
			return e.evalForeignRustArg(ref.Runtime, expr)
		}
		// Manifest channels cross as descriptors (Rust wraps them as
		// omnivm::Channel riding stream_next + the send builtin).
		if ch, isChan := binding.(*ChanRef); isChan {
			id, err := e.genericStreamHandle("go", ch)
			if err != nil {
				return nil, fmt.Errorf("channel handle for %q: %w", root, err)
			}
			return map[string]interface{}{
				"__omnivm_channel__": true,
				"id":                 int64(id),
				"kind":               "channel",
			}, nil
		}
		return binding, nil
	}

	// Expression rooted at a foreign-runtime binding (e.g. df.to_dict(...)):
	// evaluate in the owner runtime.
	if bound {
		if ref, isRef := binding.(RuntimeRef); isRef {
			return e.evalForeignRustArg(ref.Runtime, expr)
		}
	}
	// Unbound roots: try python as the expression host if available.
	if _, hasPy := e.runtimes["python"]; hasPy && root != expr {
		return e.evalForeignRustArg("python", expr)
	}
	return nil, fmt.Errorf("cannot resolve argument (no binding for %q)", root)
}

func identifierRoot(expr string) string {
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (i > 0 && c >= '0' && c <= '9') {
			continue
		}
		return expr[:i]
	}
	return expr
}

// evalForeignRustArg evaluates an expression in its owner runtime and decodes
// the result for the Rust boundary. DataFrame-like values (pandas/polars)
// cross as Arrow IPC stream markers — Arrow is the canonical tabular
// crossing; everything else crosses through the JSON value model.
func (e *Executor) evalForeignRustArg(rtName, expr string) (interface{}, error) {
	rt, ok := e.runtimes[rtName]
	if !ok {
		return nil, fmt.Errorf("runtime %q is not initialized", rtName)
	}
	var wrapped string
	switch rtName {
	case "python":
		wrapped = fmt.Sprintf(`(lambda __omnivm_v: __import__("json").dumps({%q: __import__("base64").b64encode(__omnivm_arrow_ipc(__omnivm_v)).decode()}) if (hasattr(__omnivm_v, "to_arrow") or hasattr(__omnivm_v, "to_parquet")) else __import__("json").dumps(__omnivm_v))(%s)`, arrowIPCMarkerKey, expr)
		setup := `
def __omnivm_arrow_ipc(__omnivm_df):
    import io as __io, pyarrow as __pa
    if hasattr(__omnivm_df, "to_arrow"):
        __tbl = __omnivm_df.to_arrow()
    else:
        __tbl = __pa.Table.from_pandas(__omnivm_df, preserve_index=False)
    __sink = __io.BytesIO()
    with __pa.ipc.new_stream(__sink, __tbl.schema) as __w:
        __w.write_table(__tbl)
    return __sink.getvalue()
`
		if result := rt.Execute(setup); result.Err != nil {
			return nil, fmt.Errorf("python arrow helper: %w", result.Err)
		}
	case "javascript":
		wrapped = "JSON.stringify(" + expr + ")"
	case "ruby":
		wrapped = "require 'json'; (" + expr + ").to_json"
	default:
		return nil, fmt.Errorf("cannot evaluate rust argument in runtime %q", rtName)
	}
	result := rt.Eval(wrapped)
	if result.Err != nil {
		return nil, fmt.Errorf("[%s] %s: %w", rtName, expr, result.Err)
	}
	raw := ""
	if result.Value != nil {
		raw = fmt.Sprintf("%v", result.Value)
	} else {
		raw = strings.TrimSpace(result.Output)
	}
	var out interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("[%s] %s returned non-JSON %.120q: %w", rtName, expr, raw, err)
	}
	return out, nil
}

// splitTopLevelArgs splits a call argument list on commas that are not inside
// parens, brackets, braces, or string literals.
func splitTopLevelArgs(argsStr string) []string {
	var parts []string
	depth := 0
	inString := byte(0)
	escaped := false
	start := 0
	for i := 0; i < len(argsStr); i++ {
		c := argsStr[i]
		if inString != 0 {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == inString {
				inString = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inString = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(argsStr[start:i]))
				start = i + 1
			}
		}
	}
	if last := strings.TrimSpace(argsStr[start:]); last != "" {
		parts = append(parts, last)
	}
	return parts
}
