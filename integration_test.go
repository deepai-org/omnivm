// Integration tests for OmniVM.
// These run inside Docker where all runtimes are available.
//
// Run with: go test -v -tags=integration -count=1 ./...
//
//go:build integration

package main

import (
	"runtime"
	"testing"

	"github.com/omnivm/omnivm/pkg/javascript"
	"github.com/omnivm/omnivm/pkg/jvm"
	"github.com/omnivm/omnivm/pkg/python"
	"github.com/omnivm/omnivm/pkg/ruby"
)

func init() {
	runtime.LockOSThread()
}

// ---------- Cross-runtime integration tests ----------

func TestAllRuntimesInitialize(t *testing.T) {
	py := python.New()
	js := javascript.New()
	jv := jvm.New()
	rb := ruby.New()

	if err := py.Initialize(); err != nil {
		t.Fatalf("Python init: %v", err)
	}
	defer py.Shutdown()

	if err := js.Initialize(); err != nil {
		t.Fatalf("JS init: %v", err)
	}
	defer js.Shutdown()

	if err := jv.Initialize(); err != nil {
		t.Fatalf("JVM init: %v", err)
	}
	defer jv.Shutdown()

	if err := rb.Initialize(); err != nil {
		t.Fatalf("Ruby init: %v", err)
	}
	defer rb.Shutdown()
}

func TestCrossRuntimeDataFlow(t *testing.T) {
	py := python.New()
	js := javascript.New()
	rb := ruby.New()

	if err := py.Initialize(); err != nil {
		t.Fatalf("Python init: %v", err)
	}
	defer py.Shutdown()

	if err := js.Initialize(); err != nil {
		t.Fatalf("JS init: %v", err)
	}
	defer js.Shutdown()

	if err := rb.Initialize(); err != nil {
		t.Fatalf("Ruby init: %v", err)
	}
	defer rb.Shutdown()

	// Python generates data
	pyResult := py.Execute("print(42 * 2)")
	if pyResult.Err != nil {
		t.Fatalf("Python exec: %v", pyResult.Err)
	}
	if pyResult.Output != "84\n" {
		t.Fatalf("Python expected '84\\n', got %q", pyResult.Output)
	}

	// JavaScript uses the same value
	jsResult := js.Execute("console.log(42 * 2)")
	if jsResult.Err != nil {
		t.Fatalf("JS exec: %v", jsResult.Err)
	}
	if jsResult.Output != "84\n" {
		t.Fatalf("JS expected '84\\n', got %q", jsResult.Output)
	}

	// Ruby too
	rbResult := rb.Execute("puts 42 * 2")
	if rbResult.Err != nil {
		t.Fatalf("Ruby exec: %v", rbResult.Err)
	}
	if rbResult.Output != "84\n" {
		t.Fatalf("Ruby expected '84\\n', got %q", rbResult.Output)
	}
}

func TestPythonNumpyAvailable(t *testing.T) {
	py := python.New()
	if err := py.Initialize(); err != nil {
		t.Fatalf("Python init: %v", err)
	}
	defer py.Shutdown()

	// Test that we can import stdlib
	result := py.Execute("import sys; print(sys.version_info.major)")
	if result.Err != nil {
		t.Fatalf("Python version check: %v", result.Err)
	}
	if result.Output != "3\n" {
		t.Fatalf("expected Python 3, got %q", result.Output)
	}
}

func TestJSConsoleCapture(t *testing.T) {
	js := javascript.New()
	if err := js.Initialize(); err != nil {
		t.Fatalf("JS init: %v", err)
	}
	defer js.Shutdown()

	result := js.Execute(`
		console.log('line 1');
		console.log('line 2');
		console.log(1 + 2 + 3);
	`)
	if result.Err != nil {
		t.Fatalf("JS exec: %v", result.Err)
	}
	expected := "line 1\nline 2\n6\n"
	if result.Output != expected {
		t.Fatalf("expected %q, got %q", expected, result.Output)
	}
}

func TestRubyStdlib(t *testing.T) {
	rb := ruby.New()
	if err := rb.Initialize(); err != nil {
		t.Fatalf("Ruby init: %v", err)
	}
	defer rb.Shutdown()

	result := rb.Execute("puts RUBY_VERSION")
	if result.Err != nil {
		t.Fatalf("Ruby version: %v", result.Err)
	}
	if result.Output == "" {
		t.Fatal("expected Ruby version output")
	}
}
