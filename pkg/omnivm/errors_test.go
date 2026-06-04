package omnivm

import (
	"errors"
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
	errors, ok := re.Details["errors"].([]interface{})
	if !ok || len(errors) != 1 {
		t.Fatalf("Details[errors] = %#v, want one error", re.Details["errors"])
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
