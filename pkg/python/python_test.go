package python

import (
	"runtime"
	"testing"
	"time"
)

func init() {
	// Pin test goroutine to main OS thread (required by CPython)
	runtime.LockOSThread()
}

func TestPythonInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if !r.initialized {
		t.Fatal("expected initialized=true")
	}
}

func TestPythonDoubleInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if err := r.Initialize(); err == nil {
		t.Fatal("expected error on double initialize")
	}
}

func TestPythonExecuteSimple(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("print('hello from python')")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "hello from python\n" {
		t.Fatalf("expected 'hello from python\\n', got %q", result.Output)
	}
}

func TestPythonExecuteExpression(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("print(2 + 2)")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "4\n" {
		t.Fatalf("expected '4\\n', got %q", result.Output)
	}
}

func TestPythonExecuteError(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("raise ValueError('test error')")
	if result.Err == nil {
		t.Fatal("expected error from invalid code")
	}
}

func TestPythonExecuteMultiline(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	code := `
x = 10
y = 20
print(x + y)
`
	result := r.Execute(code)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "30\n" {
		t.Fatalf("expected '30\\n', got %q", result.Output)
	}
}

func TestPythonNotInitialized(t *testing.T) {
	r := New()
	result := r.Execute("print('hi')")
	if result.Err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func TestPythonImportStdlib(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("import json; print(json.dumps({'key': 'value'}))")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	expected := `{"key": "value"}` + "\n"
	if result.Output != expected {
		t.Fatalf("expected %q, got %q", expected, result.Output)
	}
}

func TestPythonPump(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// Pump should not crash even with no event loop
	r.Pump()
}

func TestPythonPumpCompletesScheduledCoroutine(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
import asyncio
__omnivm_pump_done = False
async def __omnivm_pump_task():
    global __omnivm_pump_done
    __omnivm_pump_done = True
__omnivm_pump_loop = asyncio.new_event_loop()
asyncio.set_event_loop(__omnivm_pump_loop)
asyncio.ensure_future(__omnivm_pump_task(), loop=__omnivm_pump_loop)
`)
	if result.Err != nil {
		t.Fatalf("schedule coroutine: %v", result.Err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.Pump()
		check := r.Eval("__omnivm_pump_done")
		if check.Err == nil && check.Value == "True" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scheduled coroutine did not complete after pumping")
}
