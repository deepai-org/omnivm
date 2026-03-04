// Package signals provides safe signal handler management for the
// polyglot runtime. Go must own process signals; guest runtimes
// (Python, Node, JVM) must not install their own handlers.
package signals

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// ShutdownFunc is called during graceful shutdown.
type ShutdownFunc func()

// Manager coordinates signal handling and graceful shutdown across
// all guest runtimes.
type Manager struct {
	mu        sync.Mutex
	callbacks []namedCallback
	sigChan   chan os.Signal
}

type namedCallback struct {
	name string
	fn   ShutdownFunc
}

// NewManager creates a signal manager that intercepts SIGINT and SIGTERM.
func NewManager() *Manager {
	m := &Manager{
		sigChan: make(chan os.Signal, 1),
	}
	signal.Notify(m.sigChan, syscall.SIGINT, syscall.SIGTERM)
	return m
}

// RegisterShutdown adds a named shutdown callback. Callbacks are executed
// in reverse registration order (LIFO) during shutdown.
func (m *Manager) RegisterShutdown(name string, fn ShutdownFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callbacks = append(m.callbacks, namedCallback{name: name, fn: fn})
}

// Wait blocks until a signal is received or ctx is cancelled.
// It then calls all registered shutdown callbacks in reverse order.
// Returns the signal that triggered shutdown (nil if ctx cancelled).
func (m *Manager) Wait(ctx context.Context) os.Signal {
	var sig os.Signal
	select {
	case sig = <-m.sigChan:
	case <-ctx.Done():
	}

	m.mu.Lock()
	cbs := make([]namedCallback, len(m.callbacks))
	copy(cbs, m.callbacks)
	m.mu.Unlock()

	// LIFO order: last registered shuts down first
	for i := len(cbs) - 1; i >= 0; i-- {
		cbs[i].fn()
	}

	signal.Stop(m.sigChan)
	return sig
}
