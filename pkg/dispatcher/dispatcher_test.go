package dispatcher

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewDispatcher(t *testing.T) {
	d := New()
	if d == nil {
		t.Fatal("New() returned nil")
	}
	if d.taskChan == nil {
		t.Fatal("taskChan not initialized")
	}
}

func TestDispatcherRunOnMain(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	// RunOnMain should execute the function and return the result
	var executed bool
	err := d.RunOnMain(func() error {
		executed = true
		return nil
	})
	if err != nil {
		t.Fatalf("RunOnMain returned error: %v", err)
	}
	if !executed {
		t.Fatal("function was not executed")
	}
}

func TestDispatcherRunOnMainError(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	expectedErr := fmt.Errorf("test error")
	err := d.RunOnMain(func() error {
		return expectedErr
	})
	if err != expectedErr {
		t.Fatalf("expected %v, got %v", expectedErr, err)
	}
}

func TestDispatcherRunAsync(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	// RunAsync should return a channel that receives the result
	ch := d.RunAsync(func() (interface{}, error) {
		return 42, nil
	})

	result := <-ch
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}
	if result.Value.(int) != 42 {
		t.Fatalf("expected 42, got %v", result.Value)
	}
}

func TestDispatcherRunAsyncError(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	expectedErr := fmt.Errorf("async error")
	ch := d.RunAsync(func() (interface{}, error) {
		return nil, expectedErr
	})

	result := <-ch
	if result.Err != expectedErr {
		t.Fatalf("expected %v, got %v", expectedErr, result.Err)
	}
}

func TestDispatcherSerialization(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	// Prove that tasks are serialized: run 100 tasks that increment a counter
	// If they were concurrent, we'd see races (run with -race)
	var counter int
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.RunOnMain(func() error {
				counter++
				return nil
			})
		}()
	}

	wg.Wait()
	if counter != 100 {
		t.Fatalf("expected counter=100, got %d", counter)
	}
}

func TestDispatcherPumpCallbacks(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Register a pump callback
	var pumpCount atomic.Int64
	d.RegisterPumpCallback("test", func() {
		pumpCount.Add(1)
	})

	go d.Run(ctx)

	// Submit a task; after it runs, pump should have been called
	d.RunOnMain(func() error { return nil })

	// Give the pump loop a few cycles
	time.Sleep(10 * time.Millisecond)

	if pumpCount.Load() == 0 {
		t.Fatal("pump callback was never called")
	}
}

func TestDispatcherShutdown(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()

	// Submit a task to prove it's running
	d.RunOnMain(func() error { return nil })

	// Cancel context to trigger shutdown
	cancel()

	select {
	case <-done:
		// Good - dispatcher exited
	case <-time.After(2 * time.Second):
		t.Fatal("dispatcher did not shut down within timeout")
	}
}

func TestDispatcherWatchdog(t *testing.T) {
	d := New()
	d.WatchdogTimeout = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var warnings atomic.Int64
	d.OnWatchdogAlert = func(duration time.Duration) {
		warnings.Add(1)
	}

	go d.Run(ctx)

	// Submit a task that blocks for longer than the watchdog timeout
	d.RunOnMain(func() error {
		time.Sleep(100 * time.Millisecond)
		return nil
	})

	time.Sleep(50 * time.Millisecond)

	if warnings.Load() == 0 {
		t.Fatal("watchdog alert was never fired")
	}
}

func TestDispatcherPanicRecovery(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	// A panicking task should not crash the dispatcher
	err := d.RunOnMain(func() error {
		panic("test panic")
	})
	if err == nil {
		t.Fatal("expected error from panic, got nil")
	}

	// Dispatcher should still work after the panic
	var executed bool
	err = d.RunOnMain(func() error {
		executed = true
		return nil
	})
	if err != nil {
		t.Fatalf("post-panic task failed: %v", err)
	}
	if !executed {
		t.Fatal("post-panic task was not executed")
	}
}

func TestDispatcherFastPriority(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// We'll track execution order
	var order []string
	var mu sync.Mutex
	record := func(label string) {
		mu.Lock()
		order = append(order, label)
		mu.Unlock()
	}

	go d.Run(ctx)

	// Block the dispatcher with a slow task
	started := make(chan struct{})
	release := make(chan struct{})
	go d.RunOnMain(func() error {
		close(started)
		<-release
		record("slow")
		return nil
	})
	<-started

	// While blocked, queue a normal task and a fast task
	go d.RunOnMain(func() error {
		record("normal")
		return nil
	})
	time.Sleep(2 * time.Millisecond) // ensure normal is queued first
	go d.RunOnMainFast(func() error {
		record("fast")
		return nil
	})
	time.Sleep(2 * time.Millisecond) // ensure fast is queued

	// Release the blocker — fast should run before normal
	close(release)
	time.Sleep(20 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 {
		t.Fatalf("expected 3 tasks, got %v", order)
	}
	if order[0] != "slow" {
		t.Errorf("first should be slow, got %v", order)
	}
	if order[1] != "fast" {
		t.Errorf("second should be fast, got %v", order)
	}
	if order[2] != "normal" {
		t.Errorf("third should be normal, got %v", order)
	}
}

func TestDispatcherRunAsyncFast(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go d.Run(ctx)

	ch := d.RunAsyncFast(func() (interface{}, error) {
		return "priority", nil
	})

	result := <-ch
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.Value.(string) != "priority" {
		t.Errorf("got %v, want priority", result.Value)
	}
}

func TestDispatcherFastShutdown(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())

	go d.Run(ctx)
	d.RunOnMain(func() error { return nil }) // ensure running

	cancel()
	d.WaitForStop()

	// Fast path should also return ErrShutdown after stop
	err := d.RunOnMainFast(func() error { return nil })
	if err != ErrShutdown {
		t.Errorf("expected ErrShutdown, got %v", err)
	}
}

func TestDispatcherShutdownDrainsQueue(t *testing.T) {
	d := New()
	ctx, cancel := context.WithCancel(context.Background())

	go d.Run(ctx)

	// Queue up several tasks
	var completed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.RunOnMain(func() error {
				time.Sleep(1 * time.Millisecond)
				completed.Add(1)
				return nil
			})
		}()
	}

	// Let some tasks get queued
	time.Sleep(5 * time.Millisecond)
	cancel()

	wg.Wait()

	// All submitted tasks that were accepted should complete
	if completed.Load() == 0 {
		t.Fatal("no tasks completed during shutdown")
	}
}
