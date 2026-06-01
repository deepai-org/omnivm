package ruby

import (
	"runtime"
	"testing"
)

func init() {
	runtime.LockOSThread()
}

func TestRubyInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if !r.initialized {
		t.Fatal("expected initialized=true")
	}
}

func TestRubyDoubleInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if err := r.Initialize(); err == nil {
		t.Fatal("expected error on double initialize")
	}
}

func TestRubyExecuteSimple(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("puts 'hello from ruby'")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "hello from ruby\n" {
		t.Fatalf("expected 'hello from ruby\\n', got %q", result.Output)
	}
}

func TestRubyExecuteExpression(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("puts 2 + 2")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "4\n" {
		t.Fatalf("expected '4\\n', got %q", result.Output)
	}
}

func TestRubyExecuteError(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("raise 'test error'")
	if result.Err == nil {
		t.Fatal("expected error from raise")
	}
}

func TestRubyExecuteMultiline(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	code := `
x = 10
y = 20
puts x + y
`
	result := r.Execute(code)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "30\n" {
		t.Fatalf("expected '30\\n', got %q", result.Output)
	}
}

func TestRubyNotInitialized(t *testing.T) {
	r := New()
	result := r.Execute("puts 'hi'")
	if result.Err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func TestRubyImportStdlib(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("require 'json'; puts JSON.generate({key: 'value'})")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	expected := "{\"key\":\"value\"}\n"
	if result.Output != expected {
		t.Fatalf("expected %q, got %q", expected, result.Output)
	}
}

func TestRubyEncodingDatabaseLoaded(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("puts Encoding.find('binary').name")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "ASCII-8BIT\n" {
		t.Fatalf("expected ASCII-8BIT, got %q", result.Output)
	}
}

func TestRubyPump(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// Pump should not crash
	r.Pump()
}
