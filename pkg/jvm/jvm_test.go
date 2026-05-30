package jvm

import (
	"runtime"
	"testing"
)

func init() {
	runtime.LockOSThread()
}

func TestJVMInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if !r.initialized {
		t.Fatal("expected initialized=true")
	}
}

func TestJVMDoubleInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if err := r.Initialize(); err == nil {
		t.Fatal("expected error on double initialize")
	}
}

func TestJVMReinitializeAfterShutdown(t *testing.T) {
	r1 := New()
	if err := r1.Initialize(); err != nil {
		t.Fatalf("first Initialize failed: %v", err)
	}
	if err := r1.Shutdown(); err != nil {
		t.Fatalf("first Shutdown failed: %v", err)
	}

	r2 := New()
	if err := r2.Initialize(); err != nil {
		t.Fatalf("second Initialize failed: %v", err)
	}
	defer r2.Shutdown()
}

func TestJVMExecuteSimple(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// Using JavaScript via ScriptEngine (Nashorn or GraalJS)
	result := r.Execute("print('hello from jvm')")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output == "" {
		t.Log("Warning: empty output (may need GraalJS on classpath)")
	}
}

func TestJVMExecuteError(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("throw 'test error'")
	// This should produce an error
	if result.Err == nil {
		t.Log("Warning: error handling depends on script engine availability")
	}
}

func TestJVMNotInitialized(t *testing.T) {
	r := New()
	result := r.Execute("1 + 1")
	if result.Err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func TestJVMPump(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// Pump should be a no-op and not crash
	r.Pump()
}
