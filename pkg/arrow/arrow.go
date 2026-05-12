// Package arrow provides zero-copy data sharing between guest runtimes
// using Apache Arrow columnar format.
//
// Data flows: Go creates Arrow buffers, then passes raw pointers to
// guest runtimes (Python via pyarrow, JS via C++ bindings, etc.)
// for zero-copy access.
package arrow

import (
	"fmt"
	"sync"
	"unsafe"
)

// Buffer is a named, reference-counted Arrow-compatible memory buffer
// that can be shared across runtimes via raw pointer.
type Buffer struct {
	Name  string
	Data  []byte
	Len   int
	Dtype int32 // element type (DtypeBytes, DtypeI32, DtypeF64, etc.)
	refs  int
	mu    sync.Mutex
}

// SharedStore manages named Arrow buffers accessible to all runtimes.
type SharedStore struct {
	mu      sync.RWMutex
	buffers map[string]*Buffer
}

// NewSharedStore creates an empty shared buffer store.
func NewSharedStore() *SharedStore {
	return &SharedStore{
		buffers: make(map[string]*Buffer),
	}
}

// Allocate creates a new named buffer of the given size.
func (s *SharedStore) Allocate(name string, size int) (*Buffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.buffers[name]; exists {
		return nil, fmt.Errorf("arrow: buffer %q already exists", name)
	}

	buf := &Buffer{
		Name: name,
		Data: make([]byte, size),
		Len:  size,
		refs: 1,
	}
	s.buffers[name] = buf
	return buf, nil
}

// Get retrieves a named buffer.
func (s *SharedStore) Get(name string) (*Buffer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	buf, ok := s.buffers[name]
	if !ok {
		return nil, fmt.Errorf("arrow: buffer %q not found", name)
	}
	return buf, nil
}

// Free releases a named buffer.
func (s *SharedStore) Free(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok := s.buffers[name]
	if !ok {
		return fmt.Errorf("arrow: buffer %q not found", name)
	}

	buf.mu.Lock()
	buf.refs--
	refs := buf.refs
	buf.mu.Unlock()

	if refs <= 0 {
		delete(s.buffers, name)
	}
	return nil
}

// List returns the names of all buffers in the store.
func (s *SharedStore) List() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.buffers))
	for name := range s.buffers {
		names = append(names, name)
	}
	return names
}

// Pointer returns an unsafe.Pointer to the buffer's underlying data,
// suitable for passing to C code in guest runtimes. The caller must
// ensure the buffer is not freed while the pointer is in use.
func (b *Buffer) Pointer() unsafe.Pointer {
	if len(b.Data) == 0 {
		return nil
	}
	return unsafe.Pointer(&b.Data[0])
}

// Retain increments the reference count.
func (b *Buffer) Retain() {
	b.mu.Lock()
	b.refs++
	b.mu.Unlock()
}

// Release decrements the reference count.
func (b *Buffer) Release() int {
	b.mu.Lock()
	b.refs--
	r := b.refs
	b.mu.Unlock()
	return r
}
