package omnivm

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestRuntimeError_Error(t *testing.T) {
	e := &RuntimeError{
		Runtime: "python",
		Type:    "SyntaxError",
		Message: "invalid syntax",
	}
	got := e.Error()
	want := "python: SyntaxError: invalid syntax"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRuntimeError_WithTraceback(t *testing.T) {
	e := &RuntimeError{
		Runtime:   "python",
		Type:      "NameError",
		Message:   "name 'x' is not defined",
		Traceback: "  File \"<string>\", line 1, in <module>",
	}
	got := e.Error()
	if got == "" {
		t.Fatal("empty error string")
	}
	// Should contain the traceback
	if !contains(got, "File \"<string>\"") {
		t.Errorf("error string should contain traceback, got: %s", got)
	}
	// Should still contain the message
	if !contains(got, "name 'x' is not defined") {
		t.Errorf("error string should contain message, got: %s", got)
	}
}

func TestRuntimeError_Is(t *testing.T) {
	e := &RuntimeError{Runtime: "python", Type: "SyntaxError", Message: "bad"}
	var re *RuntimeError
	if !errors.As(e, &re) {
		t.Error("errors.As should match *RuntimeError")
	}
}

func TestRuntimeError_ToMapReturnsStructuredEnvelope(t *testing.T) {
	e := &RuntimeError{
		Runtime:             "javascript",
		Type:                "Error",
		Message:             "outer",
		Traceback:           "    at <anonymous>:1:7\nCaused by: TypeError: inner",
		StackFrames:         []string{"at <anonymous>:1:7"},
		CauseChain:          []RuntimeErrorCause{{Type: "TypeError", Message: "inner", StackFrames: []string{"at cause:1:1"}, Details: map[string]interface{}{"code": "E_INNER"}}},
		BoundaryPath:        "call[javascript]",
		OriginalErrorHandle: "js-error-42",
		Details:             map[string]interface{}{"code": "E_JS"},
	}
	got := e.ToMap()
	want := map[string]interface{}{
		"runtime":               "javascript",
		"origin_runtime":        "javascript",
		"type":                  "Error",
		"message":               "outer",
		"traceback":             "    at <anonymous>:1:7\nCaused by: TypeError: inner",
		"stack_frames":          []string{"at <anonymous>:1:7"},
		"cause_chain":           []map[string]interface{}{{"type": "TypeError", "message": "inner", "stack_frames": []string{"at cause:1:1"}, "details": map[string]interface{}{"code": "E_INNER"}}},
		"boundary_path":         "call[javascript]",
		"original_error_handle": "js-error-42",
		"details":               map[string]interface{}{"code": "E_JS"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ToMap = %#v, want %#v", got, want)
	}
}

func TestRuntimeError_ToMapCopiesMutableEnvelopeSlices(t *testing.T) {
	e := &RuntimeError{
		Runtime:     "python",
		StackFrames: []string{"File \"<string>\", line 1"},
		CauseChain: []RuntimeErrorCause{{
			Type:        "ValueError",
			Message:     "bad",
			StackFrames: []string{"cause frame"},
			Details:     map[string]interface{}{"items": []interface{}{map[string]interface{}{"path": "cause.path"}}},
		}},
		Details: map[string]interface{}{
			"code":       "E_PY",
			"items":      []interface{}{map[string]interface{}{"path": "user.age"}},
			"labels":     map[string]string{"field": "age"},
			"frames":     []string{"handler"},
			"typedCause": []map[string]string{{"type": "TypeError", "message": "inner"}},
			"nested":     map[string][]string{"loc": {"user", "age"}},
			"objects":    []map[string]interface{}{{"path": []string{"user", "age"}}},
			"mixed":      map[string][]interface{}{"items": {map[string]interface{}{"code": "too_small"}}},
		},
	}
	envelope := e.ToMap()
	envelope["stack_frames"].([]string)[0] = "changed"
	envelopeCause := envelope["cause_chain"].([]map[string]interface{})[0]
	envelopeCause["message"] = "changed"
	envelopeCause["stack_frames"].([]string)[0] = "changed"
	envelopeCause["details"].(map[string]interface{})["items"].([]interface{})[0].(map[string]interface{})["path"] = "changed"
	details := envelope["details"].(map[string]interface{})
	details["code"] = "changed"
	details["items"].([]interface{})[0].(map[string]interface{})["path"] = "changed"
	details["labels"].(map[string]string)["field"] = "changed"
	details["frames"].([]string)[0] = "changed"
	details["typedCause"].([]map[string]string)[0]["message"] = "changed"
	details["nested"].(map[string][]string)["loc"][0] = "changed"
	details["objects"].([]map[string]interface{})[0]["path"].([]string)[0] = "changed"
	details["mixed"].(map[string][]interface{})["items"][0].(map[string]interface{})["code"] = "changed"

	if e.StackFrames[0] != "File \"<string>\", line 1" {
		t.Fatalf("ToMap exposed StackFrames backing storage: %#v", e.StackFrames)
	}
	if e.CauseChain[0].Message != "bad" {
		t.Fatalf("ToMap exposed CauseChain backing storage: %#v", e.CauseChain)
	}
	if e.CauseChain[0].StackFrames[0] != "cause frame" {
		t.Fatalf("ToMap exposed CauseChain stack frame backing storage: %#v", e.CauseChain)
	}
	if e.CauseChain[0].Details.(map[string]interface{})["items"].([]interface{})[0].(map[string]interface{})["path"] != "cause.path" {
		t.Fatalf("ToMap exposed CauseChain details backing storage: %#v", e.CauseChain)
	}
	originalDetails := e.Details.(map[string]interface{})
	if originalDetails["code"] != "E_PY" {
		t.Fatalf("ToMap exposed Details map backing storage: %#v", e.Details)
	}
	if originalDetails["items"].([]interface{})[0].(map[string]interface{})["path"] != "user.age" {
		t.Fatalf("ToMap exposed nested Details backing storage: %#v", e.Details)
	}
	if originalDetails["labels"].(map[string]string)["field"] != "age" {
		t.Fatalf("ToMap exposed typed Details map backing storage: %#v", e.Details)
	}
	if originalDetails["frames"].([]string)[0] != "handler" {
		t.Fatalf("ToMap exposed typed Details slice backing storage: %#v", e.Details)
	}
	if originalDetails["typedCause"].([]map[string]string)[0]["message"] != "inner" {
		t.Fatalf("ToMap exposed typed Details map slice backing storage: %#v", e.Details)
	}
	if originalDetails["nested"].(map[string][]string)["loc"][0] != "user" {
		t.Fatalf("ToMap exposed nested typed Details map backing storage: %#v", e.Details)
	}
	if originalDetails["objects"].([]map[string]interface{})[0]["path"].([]string)[0] != "user" {
		t.Fatalf("ToMap exposed nested typed Details object slice backing storage: %#v", e.Details)
	}
	if originalDetails["mixed"].(map[string][]interface{})["items"][0].(map[string]interface{})["code"] != "too_small" {
		t.Fatalf("ToMap exposed nested mixed Details backing storage: %#v", e.Details)
	}
}

func TestParseError_Simple(t *testing.T) {
	re := ParseError("python", "ERR:SyntaxError: invalid syntax")
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Runtime != "python" {
		t.Errorf("Runtime = %q, want python", re.Runtime)
	}
	if re.OriginRuntime != "python" {
		t.Errorf("OriginRuntime = %q, want python", re.OriginRuntime)
	}
	if re.Type != "SyntaxError" {
		t.Errorf("Type = %q, want SyntaxError", re.Type)
	}
	if re.Message != "invalid syntax" {
		t.Errorf("Message = %q, want 'invalid syntax'", re.Message)
	}
}

func TestParseError_NoType(t *testing.T) {
	re := ParseError("javascript", "ERR:something went wrong")
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Type != "" {
		t.Errorf("Type = %q, want empty", re.Type)
	}
	if re.Message != "something went wrong" {
		t.Errorf("Message = %q, want 'something went wrong'", re.Message)
	}
}

func TestParseError_NotAnError(t *testing.T) {
	re := ParseError("python", "42")
	if re != nil {
		t.Errorf("expected nil for non-error string, got %+v", re)
	}
}

func TestParseError_WithTraceback(t *testing.T) {
	raw := "ERR:NameError: name 'x' is not defined\nTraceback (most recent call last):\n  File \"<string>\", line 1"
	re := ParseError("python", raw)
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Type != "NameError" {
		t.Errorf("Type = %q, want NameError", re.Type)
	}
	if re.Traceback == "" {
		t.Error("expected non-empty traceback")
	}
	if len(re.StackFrames) != 2 || re.StackFrames[0] != "Traceback (most recent call last):" || re.StackFrames[1] != "File \"<string>\", line 1" {
		t.Errorf("StackFrames = %#v, want normalized traceback lines", re.StackFrames)
	}
	if re.BoundaryPath != "call[python]" {
		t.Errorf("BoundaryPath = %q, want call[python]", re.BoundaryPath)
	}
}

func TestParseError_RuntimePrefixedWithoutERR(t *testing.T) {
	re := ParseError("python", "python: ZeroDivisionError: division by zero")
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Runtime != "python" {
		t.Errorf("Runtime = %q, want python", re.Runtime)
	}
	if re.Type != "ZeroDivisionError" {
		t.Errorf("Type = %q, want ZeroDivisionError", re.Type)
	}
	if re.Message != "division by zero" {
		t.Errorf("Message = %q, want division by zero", re.Message)
	}
	if re.BoundaryPath != "call[python]" {
		t.Errorf("BoundaryPath = %q, want call[python]", re.BoundaryPath)
	}
}

func TestParseError_RuntimeRefAssignPreservesOwnerRuntime(t *testing.T) {
	raw := "ERR:runtime ref assign [python]: Traceback (most recent call last):\n" +
		"  File \"<string>\", line 1, in <module>\n" +
		"OSError: [Errno 9] Bad file descriptor\n" +
		" (expr: (lambda __o, __args, __kwargs: __o(*__args, **__kwargs))(...))"
	re := ParseError("__manifest", raw)
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Runtime != "python" {
		t.Errorf("Runtime = %q, want python", re.Runtime)
	}
	if re.Type != "OSError" {
		t.Errorf("Type = %q, want OSError", re.Type)
	}
	if re.Message != "[Errno 9] Bad file descriptor" {
		t.Errorf("Message = %q, want [Errno 9] Bad file descriptor", re.Message)
	}
	if re.BoundaryPath != "call[python]" {
		t.Errorf("BoundaryPath = %q, want call[python]", re.BoundaryPath)
	}
	if !contains(re.Traceback, "Traceback") || !contains(re.Traceback, "(expr:") {
		t.Errorf("Traceback should retain source stack and expression metadata, got: %q", re.Traceback)
	}
}

func TestParseError_ManifestBoundaryCauseAndHandle(t *testing.T) {
	raw := "ERR:execute manifest: exec [java]: java: java.lang.RuntimeException: outer\n" +
		"\tat OmniVMEval.run(OmniVMEval.java:3)\n" +
		"Caused by: java.lang.IllegalArgumentException: inner\n" +
		"Original error handle: java-error-42"
	re := ParseError("", raw)
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Runtime != "java" {
		t.Errorf("Runtime = %q, want java", re.Runtime)
	}
	if re.Type != "java.lang.RuntimeException" {
		t.Errorf("Type = %q, want java.lang.RuntimeException", re.Type)
	}
	if re.Message != "outer" {
		t.Errorf("Message = %q, want outer", re.Message)
	}
	if re.BoundaryPath != "execute manifest > exec[java]" {
		t.Errorf("BoundaryPath = %q, want execute manifest > exec[java]", re.BoundaryPath)
	}
	if re.OriginalErrorHandle != "java-error-42" {
		t.Errorf("OriginalErrorHandle = %q, want java-error-42", re.OriginalErrorHandle)
	}
	if len(re.CauseChain) != 1 {
		t.Fatalf("CauseChain len = %d, want 1: %#v", len(re.CauseChain), re.CauseChain)
	}
	if len(re.StackFrames) != 1 || re.StackFrames[0] != "at OmniVMEval.run(OmniVMEval.java:3)" {
		t.Errorf("StackFrames = %#v, want Java stack line without cause metadata", re.StackFrames)
	}
	if re.CauseChain[0].Type != "java.lang.IllegalArgumentException" || re.CauseChain[0].Message != "inner" {
		t.Errorf("CauseChain[0] = %#v, want java.lang.IllegalArgumentException: inner", re.CauseChain[0])
	}
}

func TestParseError_PythonTracebackIgnoresMetadataLines(t *testing.T) {
	raw := "ERR:python: Traceback (most recent call last):\n" +
		"  File \"<string>\", line 1, in <module>\n" +
		"sqlalchemy.exc.IntegrityError: UNIQUE constraint failed: users.name\n" +
		"[SQL: INSERT INTO users (name) VALUES (?)]\n" +
		"[parameters: ('ada',)]\n" +
		"Details: {\"errors\":[{\"loc\":[\"age\"],\"type\":\"greater_than\"}]}\n"
	re := ParseError("python", raw)
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Type != "IntegrityError" {
		t.Errorf("Type = %q, want IntegrityError", re.Type)
	}
	if re.Message != "UNIQUE constraint failed: users.name" {
		t.Errorf("Message = %q, want UNIQUE constraint failed: users.name", re.Message)
	}
	if !contains(re.Traceback, "[parameters:") {
		t.Errorf("Traceback should retain metadata lines, got: %q", re.Traceback)
	}
	if re.Details == nil {
		t.Fatal("expected parsed details")
	}
	details, ok := re.Details.(map[string]interface{})
	if !ok {
		t.Fatalf("Details = %T, want object", re.Details)
	}
	errors, ok := details["errors"].([]interface{})
	if !ok || len(errors) != 1 {
		t.Fatalf("Details[errors] = %#v, want one error", details["errors"])
	}
}

func TestParseError_DetailsPreservesNonObjectJSON(t *testing.T) {
	re := ParseError("javascript", `ERR:javascript: AggregateError: invalid
Details: [{"path":["user","age"],"code":"too_small"}]`)
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	details, ok := re.Details.([]interface{})
	if !ok || len(details) != 1 {
		t.Fatalf("Details = %#v, want one-element array", re.Details)
	}
	envelope := re.ToMap()
	envelope["details"].([]interface{})[0] = "changed"
	if _, ok := re.Details.([]interface{})[0].(map[string]interface{}); !ok {
		t.Fatalf("ToMap exposed Details array backing storage: %#v", re.Details)
	}
}

func TestParseError_StructuredJSONEnvelope(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"runtime":               "javascript",
		"origin_runtime":        "python",
		"type":                  "AggregateError",
		"message":               "invalid",
		"traceback":             "synthetic fallback frame",
		"stack_frames":          []interface{}{"at parse (<anonymous>:1:2)"},
		"cause_chain":           []interface{}{map[string]interface{}{"type": "TypeError", "message": "inner"}},
		"boundary_path":         "call[javascript] > callback[python]",
		"original_error_handle": "js-error-7",
		"details":               []interface{}{map[string]interface{}{"path": []interface{}{"user", "age"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	re := ParseError("go", "ERR:"+string(raw))
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Runtime != "javascript" || re.OriginRuntime != "python" {
		t.Fatalf("runtime/origin = %q/%q, want javascript/python", re.Runtime, re.OriginRuntime)
	}
	if re.Type != "AggregateError" || re.Message != "invalid" {
		t.Fatalf("type/message = %q/%q, want AggregateError/invalid", re.Type, re.Message)
	}
	if !reflect.DeepEqual(re.StackFrames, []string{"at parse (<anonymous>:1:2)"}) {
		t.Fatalf("StackFrames = %#v", re.StackFrames)
	}
	if len(re.CauseChain) != 1 || re.CauseChain[0].Type != "TypeError" || re.CauseChain[0].Message != "inner" {
		t.Fatalf("CauseChain = %#v", re.CauseChain)
	}
	if re.BoundaryPath != "call[javascript] > callback[python]" || re.OriginalErrorHandle != "js-error-7" {
		t.Fatalf("boundary/handle = %q/%q", re.BoundaryPath, re.OriginalErrorHandle)
	}
	details, ok := re.Details.([]interface{})
	if !ok || len(details) != 1 {
		t.Fatalf("Details = %#v, want array", re.Details)
	}
}

func TestParseError_StructuredCausePreservesBoundaryMetadata(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"runtime":        "javascript",
		"origin_runtime": "python",
		"type":           "AggregateError",
		"message":        "outer",
		"cause_chain": []interface{}{map[string]interface{}{
			"runtime":               "java",
			"origin_runtime":        "ruby",
			"type":                  "java.lang.IllegalStateException",
			"message":               "inner",
			"traceback":             "java.lang.IllegalStateException: inner\n\tat Example.call(Example.java:7)",
			"stack_frames":          []interface{}{"at Example.call(Example.java:7)"},
			"boundary_path":         "call[javascript] > callback[java]",
			"original_error_handle": "java-error-3",
			"details":               map[string]interface{}{"code": "E_JAVA", "retryable": false},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	re := ParseError("go", "ERR:"+string(raw))
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if len(re.CauseChain) != 1 {
		t.Fatalf("CauseChain = %#v, want one cause", re.CauseChain)
	}
	cause := re.CauseChain[0]
	if cause.Runtime != "java" || cause.OriginRuntime != "ruby" {
		t.Fatalf("cause runtime/origin = %q/%q, want java/ruby", cause.Runtime, cause.OriginRuntime)
	}
	if cause.Type != "java.lang.IllegalStateException" || cause.Message != "inner" {
		t.Fatalf("cause type/message = %q/%q, want java.lang.IllegalStateException/inner", cause.Type, cause.Message)
	}
	if cause.Traceback == "" || len(cause.StackFrames) != 1 || cause.StackFrames[0] != "at Example.call(Example.java:7)" {
		t.Fatalf("cause traceback/frames = %q/%#v", cause.Traceback, cause.StackFrames)
	}
	if cause.BoundaryPath != "call[javascript] > callback[java]" || cause.OriginalErrorHandle != "java-error-3" {
		t.Fatalf("cause boundary/handle = %q/%q", cause.BoundaryPath, cause.OriginalErrorHandle)
	}
	details, ok := cause.Details.(map[string]interface{})
	if !ok || details["code"] != "E_JAVA" || details["retryable"] != false {
		t.Fatalf("cause details = %#v", cause.Details)
	}
	causes, ok := re.ToMap()["cause_chain"].([]map[string]interface{})
	if !ok || len(causes) != 1 {
		t.Fatalf("mapped cause_chain = %#v, want one mapped cause", re.ToMap()["cause_chain"])
	}
	if causes[0]["runtime"] != "java" || causes[0]["origin_runtime"] != "ruby" ||
		causes[0]["boundary_path"] != "call[javascript] > callback[java]" ||
		causes[0]["original_error_handle"] != "java-error-3" {
		t.Fatalf("mapped cause metadata = %#v", causes[0])
	}
	if causes[0]["traceback"] == "" || !reflect.DeepEqual(causes[0]["stack_frames"], []string{"at Example.call(Example.java:7)"}) {
		t.Fatalf("mapped cause traceback/frames = %#v", causes[0])
	}
	if mappedDetails, ok := causes[0]["details"].(map[string]interface{}); !ok || mappedDetails["code"] != "E_JAVA" {
		t.Fatalf("mapped cause details = %#v", causes[0]["details"])
	}
}

func TestParseError_StructuredJSONEnvelopeAcceptsCamelCase(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"runtime":             "javascript",
		"originRuntime":       "python",
		"type":                "AggregateError",
		"message":             "outer",
		"traceback":           "Error: outer\n    at parse (<anonymous>:1:2)",
		"stackFrames":         []interface{}{"at parse (<anonymous>:1:2)"},
		"boundaryPath":        "call[javascript] > callback[python]",
		"originalErrorHandle": "js-error-7",
		"causeChain": []interface{}{map[string]interface{}{
			"runtime":             "java",
			"originRuntime":       "ruby",
			"type":                "java.lang.IllegalStateException",
			"message":             "inner",
			"traceback":           "java.lang.IllegalStateException: inner\n\tat Example.call(Example.java:7)",
			"stackFrames":         []interface{}{"at Example.call(Example.java:7)"},
			"boundaryPath":        "call[javascript] > callback[java]",
			"originalErrorHandle": "java-error-3",
			"details":             map[string]interface{}{"code": "E_JAVA"},
		}},
		"details": map[string]interface{}{"code": "E_JS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	re := ParseError("go", "ERR:"+string(raw))
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Runtime != "javascript" || re.OriginRuntime != "python" {
		t.Fatalf("runtime/origin = %q/%q, want javascript/python", re.Runtime, re.OriginRuntime)
	}
	if re.BoundaryPath != "call[javascript] > callback[python]" || re.OriginalErrorHandle != "js-error-7" {
		t.Fatalf("boundary/handle = %q/%q", re.BoundaryPath, re.OriginalErrorHandle)
	}
	if !reflect.DeepEqual(re.StackFrames, []string{"at parse (<anonymous>:1:2)"}) {
		t.Fatalf("StackFrames = %#v", re.StackFrames)
	}
	if len(re.CauseChain) != 1 {
		t.Fatalf("CauseChain = %#v, want one cause", re.CauseChain)
	}
	cause := re.CauseChain[0]
	if cause.Runtime != "java" || cause.OriginRuntime != "ruby" {
		t.Fatalf("cause runtime/origin = %q/%q, want java/ruby", cause.Runtime, cause.OriginRuntime)
	}
	if cause.BoundaryPath != "call[javascript] > callback[java]" || cause.OriginalErrorHandle != "java-error-3" {
		t.Fatalf("cause boundary/handle = %q/%q", cause.BoundaryPath, cause.OriginalErrorHandle)
	}
	if !reflect.DeepEqual(cause.StackFrames, []string{"at Example.call(Example.java:7)"}) {
		t.Fatalf("cause StackFrames = %#v", cause.StackFrames)
	}
	envelope := re.ToMap()
	if envelope["origin_runtime"] != "python" || envelope["boundary_path"] != "call[javascript] > callback[python]" ||
		envelope["original_error_handle"] != "js-error-7" {
		t.Fatalf("normalized envelope metadata = %#v", envelope)
	}
	causes, ok := envelope["cause_chain"].([]map[string]interface{})
	if !ok || len(causes) != 1 || causes[0]["origin_runtime"] != "ruby" ||
		causes[0]["boundary_path"] != "call[javascript] > callback[java]" ||
		causes[0]["original_error_handle"] != "java-error-3" {
		t.Fatalf("normalized cause metadata = %#v", envelope["cause_chain"])
	}
}

func TestParseError_WrappedStructuredJSONEnvelopePreservesFields(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"runtime":        "javascript",
		"origin_runtime": "python",
		"type":           "AggregateError",
		"message":        "invalid",
		"stack_frames":   []interface{}{"at parse (<anonymous>:1:2)"},
		"cause_chain":    []interface{}{map[string]interface{}{"type": "TypeError", "message": "inner"}},
		"details":        []interface{}{map[string]interface{}{"path": []interface{}{"user", "age"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	re := ParseError("", "ERR:execute manifest: call [javascript]: "+string(raw))
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.Runtime != "javascript" || re.OriginRuntime != "python" {
		t.Fatalf("runtime/origin = %q/%q, want javascript/python", re.Runtime, re.OriginRuntime)
	}
	if re.Type != "AggregateError" || re.Message != "invalid" {
		t.Fatalf("type/message = %q/%q, want AggregateError/invalid", re.Type, re.Message)
	}
	if re.BoundaryPath != "execute manifest > call[javascript]" {
		t.Fatalf("BoundaryPath = %q, want outer manifest/call boundary", re.BoundaryPath)
	}
	if !reflect.DeepEqual(re.StackFrames, []string{"at parse (<anonymous>:1:2)"}) {
		t.Fatalf("StackFrames = %#v", re.StackFrames)
	}
	if len(re.CauseChain) != 1 || re.CauseChain[0].Type != "TypeError" || re.CauseChain[0].Message != "inner" {
		t.Fatalf("CauseChain = %#v", re.CauseChain)
	}
	if details, ok := re.Details.([]interface{}); !ok || len(details) != 1 {
		t.Fatalf("Details = %#v, want one structured detail", re.Details)
	}
}

func TestParseError_WrappedStructuredJSONEnvelopeKeepsExplicitBoundary(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"runtime":       "ruby",
		"type":          "RuntimeError",
		"message":       "failed",
		"boundary_path": "call[ruby] > callback[python]",
	})
	if err != nil {
		t.Fatal(err)
	}
	re := ParseError("", "ERR:execute manifest: "+string(raw))
	if re == nil {
		t.Fatal("expected non-nil RuntimeError")
	}
	if re.BoundaryPath != "call[ruby] > callback[python]" {
		t.Fatalf("BoundaryPath = %q, want explicit envelope boundary", re.BoundaryPath)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
