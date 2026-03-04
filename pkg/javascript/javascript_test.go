package javascript

import (
	"runtime"
	"testing"
)

func init() {
	runtime.LockOSThread()
}

func TestJSInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if !r.initialized {
		t.Fatal("expected initialized=true")
	}
}

func TestJSDoubleInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if err := r.Initialize(); err == nil {
		t.Fatal("expected error on double initialize")
	}
}

func TestJSExecuteSimple(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("console.log('hello from v8')")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "hello from v8\n" {
		t.Fatalf("expected 'hello from v8\\n', got %q", result.Output)
	}
}

func TestJSExecuteExpression(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("console.log(2 + 2)")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "4\n" {
		t.Fatalf("expected '4\\n', got %q", result.Output)
	}
}

func TestJSExecuteError(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("throw new Error('test error')")
	if result.Err == nil {
		t.Fatal("expected error from throw")
	}
}

func TestJSExecuteSyntaxError(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("this is not valid javascript {{{")
	if result.Err == nil {
		t.Fatal("expected error from syntax error")
	}
}

func TestJSNotInitialized(t *testing.T) {
	r := New()
	result := r.Execute("console.log('hi')")
	if result.Err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func TestJSPump(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// Pump should not crash
	r.Pump()
}

func TestJSMultipleExecutions(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// State should persist between executions within the same context
	r.Execute("var x = 42")
	result := r.Execute("console.log(x)")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "42\n" {
		t.Fatalf("expected '42\\n', got %q", result.Output)
	}
}
