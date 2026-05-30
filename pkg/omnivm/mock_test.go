package omnivm

import (
	"fmt"
	"sync"

	pkg "github.com/omnivm/omnivm/pkg"
)

// MockRuntime implements pkg.Runtime for testing without cgo.
type MockRuntime struct {
	name       string
	initErr    error
	execResult pkg.Result
	evalResult pkg.Result

	mu         sync.Mutex
	initCalled int
	execCalls  []string
	evalCalls  []string
	shutCalled int
	pumpCalled int

	// Shared slice to record init order across multiple mocks.
	initOrder *[]string
}

func newMock(name string) *MockRuntime {
	return &MockRuntime{name: name}
}

func (m *MockRuntime) Name() string { return m.name }

func (m *MockRuntime) Initialize() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.initCalled++
	if m.initOrder != nil {
		*m.initOrder = append(*m.initOrder, m.name)
	}
	return m.initErr
}

func (m *MockRuntime) Execute(code string) pkg.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.execCalls = append(m.execCalls, code)
	return m.execResult
}

func (m *MockRuntime) Eval(code string) pkg.Result {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evalCalls = append(m.evalCalls, code)
	return m.evalResult
}

func (m *MockRuntime) SetBridgeCallback(callPtr, freePtr uintptr) {}

func (m *MockRuntime) Pump() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pumpCalled++
}

func (m *MockRuntime) Shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shutCalled++
	if m.initOrder != nil {
		*m.initOrder = append(*m.initOrder, "shutdown:"+m.name)
	}
	return nil
}

func (m *MockRuntime) getInitCalled() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.initCalled
}

func (m *MockRuntime) getShutCalled() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shutCalled
}

func (m *MockRuntime) getExecCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]string, len(m.execCalls))
	copy(cp, m.execCalls)
	return cp
}

func (m *MockRuntime) getEvalCalls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]string, len(m.evalCalls))
	copy(cp, m.evalCalls)
	return cp
}

// failingMock returns a mock that fails Initialize with the given error.
func failingMock(name string, order *[]string) *MockRuntime {
	m := newMock(name)
	m.initErr = fmt.Errorf("init failed: %s", name)
	m.initOrder = order
	return m
}
