package omnivm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pkg "github.com/omnivm/omnivm/pkg"
)

// startedVM creates a VM with the given mocks, starts it, and runs the dispatcher.
// Returns the VM and a cancel func. Call cancel + vm.Shutdown() when done.
func startedVM(t *testing.T, mocks ...*MockRuntime) (*VM, context.CancelFunc) {
	t.Helper()
	vm := New(Config{})
	for _, m := range mocks {
		vm.Register(m.name, m)
	}
	if err := vm.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go vm.Run(ctx)
	// Give dispatcher a moment to start
	time.Sleep(5 * time.Millisecond)
	return vm, cancel
}

// --- Phase 3: Lifecycle ---

func TestNew_Defaults(t *testing.T) {
	vm := New(Config{})
	if vm == nil {
		t.Fatal("New returned nil")
	}
}

func TestRegister_AddsRuntime(t *testing.T) {
	vm := New(Config{})
	m := newMock("python")
	vm.Register("python", m)
	if _, ok := vm.runtimes["python"]; !ok {
		t.Error("runtime not registered")
	}
}

func TestStart_InitializesRegisteredOnly(t *testing.T) {
	vm := New(Config{})
	py := newMock("python")
	vm.Register("python", py)
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	if py.getInitCalled() != 1 {
		t.Errorf("init called %d times, want 1", py.getInitCalled())
	}
}

func TestStart_InitializesInRegistrationOrder(t *testing.T) {
	var order []string
	vm := New(Config{})
	py := newMock("python")
	py.initOrder = &order
	js := newMock("javascript")
	js.initOrder = &order
	rb := newMock("ruby")
	rb.initOrder = &order

	vm.Register("python", py)
	vm.Register("javascript", js)
	vm.Register("ruby", rb)

	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 {
		t.Fatalf("expected 3 inits, got %d", len(order))
	}
	if order[0] != "python" || order[1] != "javascript" || order[2] != "ruby" {
		t.Errorf("init order = %v, want [python javascript ruby]", order)
	}
}

func TestStart_InitFailure_CleansUp(t *testing.T) {
	var order []string
	vm := New(Config{})
	py := newMock("python")
	py.initOrder = &order
	js := failingMock("javascript", &order)
	rb := newMock("ruby")
	rb.initOrder = &order

	vm.Register("python", py)
	vm.Register("javascript", js)
	vm.Register("ruby", rb)

	err := vm.Start()
	if err == nil {
		t.Fatal("expected error from failing init")
	}
	// Python was inited, should be shut down
	if py.getShutCalled() != 1 {
		t.Errorf("python shutdown called %d times, want 1", py.getShutCalled())
	}
	// Ruby should never have been initialized
	if rb.getInitCalled() != 0 {
		t.Error("ruby should not have been initialized")
	}
}

func TestStart_AlreadyStarted(t *testing.T) {
	vm := New(Config{})
	vm.Register("python", newMock("python"))
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}
	err := vm.Start()
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("expected ErrAlreadyStarted, got %v", err)
	}
}

// --- Phase 4: Dispatch ---

func TestCall_ReturnsResult(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Value: "42"}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	result, err := vm.Call("python", "21 + 21")
	if err != nil {
		t.Fatal(err)
	}
	if result != "42" {
		t.Errorf("result = %q, want 42", result)
	}
}

func TestExecute_ReturnsOutput(t *testing.T) {
	py := newMock("python")
	py.execResult = pkg.Result{Output: "hello world\n"}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	output, err := vm.Execute("python", "print('hello world')")
	if err != nil {
		t.Fatal(err)
	}
	if output != "hello world\n" {
		t.Errorf("output = %q, want 'hello world\\n'", output)
	}
}

func TestCall_UnknownRuntime(t *testing.T) {
	vm, cancel := startedVM(t, newMock("python"))
	defer func() { cancel(); vm.Shutdown() }()

	_, err := vm.Call("ruby", "puts 'hi'")
	var unk *ErrUnknownRuntime
	if !errors.As(err, &unk) {
		t.Errorf("expected ErrUnknownRuntime, got %v", err)
	}
}

func TestCall_BeforeStart(t *testing.T) {
	vm := New(Config{})
	vm.Register("python", newMock("python"))
	_, err := vm.Call("python", "1+1")
	if !errors.Is(err, ErrNotStarted) {
		t.Errorf("expected ErrNotStarted, got %v", err)
	}
}

func TestCall_RuntimeError_Structured(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{
		Value: "ERR:SyntaxError: invalid syntax",
		Err:   errors.New("ERR:SyntaxError: invalid syntax"),
	}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	_, err := vm.Call("python", "def")
	if err == nil {
		t.Fatal("expected error")
	}
	var re *RuntimeError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RuntimeError, got %T: %v", err, err)
	}
	if re.Type != "SyntaxError" {
		t.Errorf("Type = %q, want SyntaxError", re.Type)
	}
	if re.Runtime != "python" {
		t.Errorf("Runtime = %q, want python", re.Runtime)
	}
}

// --- Phase 5: AfterCall & Hooks ---

func TestSetAfterCall_RunsCleanupCode(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Value: "ok"}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	vm.SetAfterCall("python", "cleanup()")
	_, err := vm.Call("python", "do_work()")
	if err != nil {
		t.Fatal(err)
	}

	// The afterCall should have triggered an Execute with the cleanup code
	execs := py.getExecCalls()
	if len(execs) != 1 || execs[0] != "cleanup()" {
		t.Errorf("exec calls = %v, want [cleanup()]", execs)
	}
}

func TestSetAfterCall_RunsEvenOnError(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Err: errors.New("ERR:DoesNotExist: not found")}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	vm.SetAfterCall("python", "close_connections()")
	_, err := vm.Call("python", "get_user()")
	if err == nil {
		t.Fatal("expected error")
	}

	// AfterCall should still have run
	execs := py.getExecCalls()
	if len(execs) != 1 || execs[0] != "close_connections()" {
		t.Errorf("exec calls = %v, want [close_connections()]", execs)
	}
}

func TestSetAfterCall_ErrorLogged(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Err: errors.New("ERR:NameError: x")}
	// afterCall will also fail (execResult has an error)
	py.execResult = pkg.Result{Err: errors.New("cleanup failed")}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	vm.SetAfterCall("python", "cleanup()")
	_, err := vm.Call("python", "code")

	// Original error should be returned, not the afterCall error
	var re *RuntimeError
	if !errors.As(err, &re) {
		t.Fatalf("expected *RuntimeError from original call, got %T: %v", err, err)
	}
	if re.Type != "NameError" {
		t.Errorf("Type = %q, want NameError (original error)", re.Type)
	}
}

func TestOnCallDone_Fires(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Value: "result123"}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	var gotRuntime, gotResult string
	var gotErr error
	var mu sync.Mutex
	vm.SetOnCallDone(func(runtime, result string, err error) {
		mu.Lock()
		defer mu.Unlock()
		gotRuntime = runtime
		gotResult = result
		gotErr = err
	})

	result, err := vm.Call("python", "code")
	if err != nil {
		t.Fatal(err)
	}
	if result != "result123" {
		t.Errorf("result = %q", result)
	}

	// Give callback a moment (it runs synchronously in dispatch, should be immediate)
	time.Sleep(10 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if gotRuntime != "python" {
		t.Errorf("callback runtime = %q, want python", gotRuntime)
	}
	if gotResult != "result123" {
		t.Errorf("callback result = %q, want result123", gotResult)
	}
	if gotErr != nil {
		t.Errorf("callback err = %v, want nil", gotErr)
	}
}

// --- Phase 6: Context & Cancellation ---

func TestCallWithContext_Cancelled(t *testing.T) {
	py := newMock("python")
	// Make eval block until we're ready
	py.evalResult = pkg.Result{Value: "ok"}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	ctx, ctxCancel := context.WithCancel(context.Background())
	ctxCancel() // cancel immediately

	_, err := vm.CallWithContext(ctx, "python", "code")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestCallWithContext_Deadline(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Value: "ok"}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer ctxCancel()
	time.Sleep(1 * time.Millisecond) // ensure deadline passes

	_, err := vm.CallWithContext(ctx, "python", "code")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}
}

func TestCall_ConcurrentSafe(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Value: "ok"}
	vm, cancel := startedVM(t, py)
	defer func() { cancel(); vm.Shutdown() }()

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := vm.Call("python", "1+1")
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call failed: %v", err)
	}
}

// --- Phase 7: Shutdown ---

func TestShutdown_ReverseOrder(t *testing.T) {
	var order []string
	py := newMock("python")
	py.initOrder = &order
	js := newMock("javascript")
	js.initOrder = &order
	rb := newMock("ruby")
	rb.initOrder = &order

	vm := New(Config{})
	vm.Register("python", py)
	vm.Register("javascript", js)
	vm.Register("ruby", rb)
	if err := vm.Start(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go vm.Run(ctx)
	time.Sleep(5 * time.Millisecond)

	cancel()
	vm.Shutdown()

	// order should be: python, javascript, ruby (init), then shutdown:ruby, shutdown:javascript, shutdown:python
	shutdowns := []string{}
	for _, s := range order {
		if len(s) > 9 && s[:9] == "shutdown:" {
			shutdowns = append(shutdowns, s)
		}
	}
	if len(shutdowns) != 3 {
		t.Fatalf("expected 3 shutdowns, got %v", shutdowns)
	}
	if shutdowns[0] != "shutdown:ruby" || shutdowns[1] != "shutdown:javascript" || shutdowns[2] != "shutdown:python" {
		t.Errorf("shutdown order = %v, want [shutdown:ruby shutdown:javascript shutdown:python]", shutdowns)
	}
}

func TestShutdown_DrainHooksFirst(t *testing.T) {
	py := newMock("python")
	vm, cancel := startedVM(t, py)

	var order []string
	var mu sync.Mutex
	vm.RegisterDrainHook(func() {
		mu.Lock()
		order = append(order, "drain")
		mu.Unlock()
	})

	cancel()
	vm.Shutdown()

	mu.Lock()
	defer mu.Unlock()
	if len(order) == 0 || order[0] != "drain" {
		t.Errorf("drain hook should have fired, order = %v", order)
	}
	if py.getShutCalled() < 1 {
		t.Error("runtime shutdown should have been called")
	}
}

func TestShutdown_DrainHookCanExecute(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Value: "ok"}
	py.execResult = pkg.Result{Output: "flushed"}
	vm, cancel := startedVM(t, py)

	var drainResult string
	var drainErr error
	vm.RegisterDrainHook(func() {
		// Drain hooks must be able to call into the runtime
		// (e.g., connections.close_all(), sentry_sdk.flush())
		drainResult, drainErr = vm.drainExecute("python", "flush_all()")
	})

	cancel()
	vm.Shutdown()

	if drainErr != nil {
		t.Errorf("drain Execute failed: %v", drainErr)
	}
	if drainResult != "flushed" {
		t.Errorf("drain result = %q, want 'flushed'", drainResult)
	}

	execs := py.getExecCalls()
	found := false
	for _, c := range execs {
		if c == "flush_all()" {
			found = true
		}
	}
	if !found {
		t.Errorf("drain hook Execute not dispatched, execs = %v", execs)
	}
}

func TestShutdown_Idempotent(t *testing.T) {
	py := newMock("python")
	vm, cancel := startedVM(t, py)
	cancel()

	vm.Shutdown()
	vm.Shutdown() // second call should be no-op

	if py.getShutCalled() != 1 {
		t.Errorf("shutdown called %d times, want 1", py.getShutCalled())
	}
}

func TestShutdown_CancelsInFlightCalls(t *testing.T) {
	py := newMock("python")
	py.evalResult = pkg.Result{Value: "ok"}
	vm, cancel := startedVM(t, py)

	// Shutdown immediately
	cancel()
	vm.Shutdown()

	// Calls after shutdown should fail
	_, err := vm.Call("python", "1+1")
	if err == nil {
		t.Error("expected error after shutdown")
	}
}
