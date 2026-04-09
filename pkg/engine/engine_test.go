package engine

import (
	"fmt"
	"sync"
	"testing"

	pkg "github.com/omnivm/omnivm/pkg"
)

// mockRuntime implements pkg.Runtime for testing without cgo.
type mockRuntime struct {
	name       string
	initErr    error
	execResult pkg.Result
	evalResult pkg.Result

	mu         sync.Mutex
	initCalled int
	execCalls  []string
	evalCalls  []string
	shutCalled int
	bridgeSet  bool
}

func newMock(name string) *mockRuntime {
	return &mockRuntime{name: name, evalResult: pkg.Result{Value: "ok"}}
}

func (m *mockRuntime) Name() string { return m.name }

func (m *mockRuntime) Initialize() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initCalled++
	return m.initErr
}

func (m *mockRuntime) Execute(code string) pkg.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCalls = append(m.execCalls, code)
	return m.execResult
}

func (m *mockRuntime) Eval(code string) pkg.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evalCalls = append(m.evalCalls, code)
	return m.evalResult
}

func (m *mockRuntime) SetBridgeCallback(callPtr, freePtr uintptr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bridgeSet = true
}

func (m *mockRuntime) Pump() {}

func (m *mockRuntime) Shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutCalled++
	return nil
}

func TestNew(t *testing.T) {
	e := New()
	if e == nil {
		t.Fatal("New() returned nil")
	}
	if e.Runtimes == nil {
		t.Fatal("Runtimes map is nil")
	}
	if e.Disp == nil {
		t.Fatal("Dispatcher is nil")
	}
	if e.TaskTimeoutMS <= 0 {
		t.Fatalf("TaskTimeoutMS should be positive, got %d", e.TaskTimeoutMS)
	}
}

func TestCallUnknownRuntime(t *testing.T) {
	e := New()
	_, err := e.Call("nonexistent", "code", 0)
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
	if err.Error() != "unknown runtime: nonexistent" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExecUnknownRuntime(t *testing.T) {
	e := New()
	_, err := e.Exec("nonexistent", "code", 0)
	if err == nil {
		t.Fatal("expected error for unknown runtime")
	}
}

func TestCallSuccess(t *testing.T) {
	e := New()
	m := newMock("python")
	m.evalResult = pkg.Result{Value: "42"}
	e.Runtimes["python"] = m

	result, err := e.Call("python", "21 * 2", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "42" {
		t.Fatalf("expected '42', got %q", result)
	}
}

func TestCallError(t *testing.T) {
	e := New()
	m := newMock("python")
	m.evalResult = pkg.Result{Err: fmt.Errorf("syntax error")}
	e.Runtimes["python"] = m

	_, err := e.Call("python", "bad code", 0)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "syntax error" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCallReturnsOutput(t *testing.T) {
	e := New()
	m := newMock("js")
	m.evalResult = pkg.Result{Output: "hello"}
	e.Runtimes["js"] = m

	result, err := e.Call("js", "code", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello" {
		t.Fatalf("expected 'hello', got %q", result)
	}
}

func TestExecSuccess(t *testing.T) {
	e := New()
	m := newMock("ruby")
	m.execResult = pkg.Result{Output: "hello from ruby"}
	e.Runtimes["ruby"] = m

	result, err := e.Exec("ruby", "puts 'hello'", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "hello from ruby" {
		t.Fatalf("expected 'hello from ruby', got %q", result)
	}
}

func TestExecError(t *testing.T) {
	e := New()
	m := newMock("ruby")
	m.execResult = pkg.Result{Err: fmt.Errorf("name error")}
	e.Runtimes["ruby"] = m

	_, err := e.Exec("ruby", "bad", 0)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSetupBridge(t *testing.T) {
	e := New()
	m1 := newMock("python")
	m2 := newMock("javascript")
	e.Runtimes["python"] = m1
	e.Runtimes["javascript"] = m2

	e.SetupBridge(0x1234, 0x5678)

	if !m1.bridgeSet {
		t.Fatal("python bridge not set")
	}
	if !m2.bridgeSet {
		t.Fatal("javascript bridge not set")
	}
}

func TestRuntimeID(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{"python", 1},
		{"javascript", 2},
		{"ruby", 3},
		{"java", 4},
		{"unknown", 0},
	}
	for _, tc := range tests {
		got := RuntimeID(tc.name)
		if got != tc.want {
			t.Errorf("RuntimeID(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestShutdown(t *testing.T) {
	e := New()
	m1 := newMock("python")
	m2 := newMock("javascript")
	e.Runtimes["python"] = m1
	e.Runtimes["javascript"] = m2

	e.Shutdown()

	if m1.shutCalled != 1 {
		t.Fatalf("python shutdown called %d times, want 1", m1.shutCalled)
	}
	if m2.shutCalled != 1 {
		t.Fatalf("javascript shutdown called %d times, want 1", m2.shutCalled)
	}
	if len(e.Runtimes) != 0 {
		t.Fatalf("runtimes not cleared after shutdown: %d remain", len(e.Runtimes))
	}
}

func TestShutdownOrder(t *testing.T) {
	// Shutdown should go in reverse init order: go, ruby, java, javascript, python
	e := New()
	var order []string
	for _, name := range []string{"python", "javascript", "java", "ruby"} {
		m := newMock(name)
		m.evalResult = pkg.Result{Value: "ok"}
		origShutdown := m.Shutdown
		_ = origShutdown
		// Track shutdown order via a closure
		n := name
		e.Runtimes[name] = &orderTrackingRuntime{mockRuntime: m, name: n, order: &order}
	}

	e.Shutdown()

	// Expected order based on the hardcoded list in Shutdown()
	expected := []string{"ruby", "java", "javascript", "python"}
	if len(order) != len(expected) {
		t.Fatalf("shutdown order length %d, want %d: %v", len(order), len(expected), order)
	}
	for i, name := range expected {
		if order[i] != name {
			t.Fatalf("shutdown[%d] = %q, want %q (full: %v)", i, order[i], name, order)
		}
	}
}

// orderTrackingRuntime wraps mockRuntime to track shutdown order.
type orderTrackingRuntime struct {
	*mockRuntime
	name  string
	order *[]string
}

func (o *orderTrackingRuntime) Shutdown() error {
	*o.order = append(*o.order, o.name)
	return o.mockRuntime.Shutdown()
}

func TestExecWithWatchdog(t *testing.T) {
	e := New()
	called := false
	result := e.ExecWithWatchdog(1, func() pkg.Result {
		called = true
		return pkg.Result{Output: "done"}
	})
	if !called {
		t.Fatal("function not called")
	}
	if result.Output != "done" {
		t.Fatalf("expected 'done', got %q", result.Output)
	}
}

func TestActivateForkGuard(t *testing.T) {
	// Just verify it doesn't panic with no runtimes
	e := New()
	e.ActivateForkGuard() // no-op, no java/ruby

	e.Runtimes["python"] = newMock("python")
	e.ActivateForkGuard() // still no-op, no java/ruby

	// With java, should activate (calls python.ActivateForkGuard which is C)
	// Can't test the C call on macOS, but at least verify the logic path
}
