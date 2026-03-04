package signals

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestManagerRegistersCallbacks(t *testing.T) {
	m := NewManager()
	var called atomic.Bool
	m.RegisterShutdown("test", func() {
		called.Store(true)
	})

	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	m.Wait(ctx)

	if !called.Load() {
		t.Fatal("shutdown callback was not called")
	}
}

func TestManagerLIFOOrder(t *testing.T) {
	m := NewManager()
	var order []string

	m.RegisterShutdown("first", func() {
		order = append(order, "first")
	})
	m.RegisterShutdown("second", func() {
		order = append(order, "second")
	})
	m.RegisterShutdown("third", func() {
		order = append(order, "third")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Immediate cancel

	m.Wait(ctx)

	if len(order) != 3 {
		t.Fatalf("expected 3 callbacks, got %d", len(order))
	}
	if order[0] != "third" || order[1] != "second" || order[2] != "first" {
		t.Fatalf("expected LIFO order [third second first], got %v", order)
	}
}
