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
