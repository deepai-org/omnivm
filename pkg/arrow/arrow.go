// Package arrow provides zero-copy data sharing between guest runtimes
// using Apache Arrow columnar format.
//
// Data flows: Go creates Arrow buffers, then passes raw pointers to
// guest runtimes (Python via pyarrow, JS via C++ bindings, etc.)
// for zero-copy access.
package arrow

import (
	"fmt"
	"strconv"
	"sync"
	"unsafe"
)

// BufferMetadata describes a generic Arrow-compatible buffer without naming a
// specific producer library. Shape/strides are optional evidence for tensors,
// images, and array-like values; Arrow format/nullability describe columnar
// values when known.
type BufferMetadata struct {
	Dtype             int32   `json:"dtype"`
	Format            string  `json:"format,omitempty"`
	Shape             []int64 `json:"shape,omitempty"`
	Strides           []int64 `json:"strides,omitempty"`
	Offset            int64   `json:"offset,omitempty"`
	NullCount         int64   `json:"null_count,omitempty"`
	ValidityBytes     int64   `json:"validity_bytes,omitempty"`
	ValidityBitOffset int64   `json:"validity_bit_offset,omitempty"`
	ReadOnly          bool    `json:"read_only"`
	Ownership         string  `json:"ownership,omitempty"`
}

// Buffer is a named, reference-counted Arrow-compatible memory buffer
// that can be shared across runtimes via raw pointer.
type Buffer struct {
	Name              string
	Data              []byte
	ExternalData      unsafe.Pointer
	Len               int
	Validity          []byte
	ExternalValidity  unsafe.Pointer
	ValidityLen       int
	Dtype             int32 // element type (DtypeBytes, DtypeI32, DtypeF64, etc.)
	Format            string
	Shape             []int64
	Strides           []int64
	Offset            int64
	NullCount         int64
	ValidityBitOffset int64
	ReadOnly          bool
	Ownership         string
	refs              int
	release           func() error
	mu                sync.Mutex
}

// BorrowedBuffer is a zero-copy lease over a shared buffer. Runtime adapters
// use this when they can expose a stable memory view directly instead of
// materializing through JSON or another copied format.
type BorrowedBuffer struct {
	store       *SharedStore
	name        string
	Buffer      *Buffer
	Data        unsafe.Pointer
	Len         int64
	Validity    unsafe.Pointer
	ValidityLen int64
	Dtype       int32
	Metadata    BufferMetadata
	once        sync.Once
}

// Stats is a process-level snapshot for bulk data diagnostics.
type Stats struct {
	LiveBuffers       int            `json:"live_buffers"`
	LiveBytes         int64          `json:"live_bytes"`
	BuffersByDtype    map[string]int `json:"buffers_by_dtype"`
	BuffersByFormat   map[string]int `json:"buffers_by_format"`
	Allocations       int64          `json:"allocations"`
	Sets              int64          `json:"sets"`
	Gets              int64          `json:"gets"`
	Releases          int64          `json:"releases"`
	CopiedBytes       int64          `json:"copied_bytes"`
	ZeroCopyBorrows   int64          `json:"zero_copy_borrows"`
	ZeroCopyImports   int64          `json:"zero_copy_imports"`
	ActiveBorrows     int64          `json:"active_borrows"`
	DeferredDrops     int64          `json:"deferred_release_drops"`
	DeferredQueueLen  int            `json:"deferred_release_queue_len"`
	DeferredOverflow  int            `json:"deferred_release_overflow_names"`
	LargestBufferName string         `json:"largest_buffer_name,omitempty"`
	LargestBufferSize int64          `json:"largest_buffer_size"`
}

// SharedStore manages named Arrow buffers accessible to all runtimes.
type SharedStore struct {
	mu           sync.RWMutex
	buffers      map[string]*Buffer
	namedBorrows map[string][]*Buffer

	allocations     int64
	sets            int64
	gets            int64
	releases        int64
	copiedBytes     int64
	zeroCopyBorrows int64
	zeroCopyImports int64
	deferredDrops   int64
}

// NewSharedStore creates an empty shared buffer store.
func NewSharedStore() *SharedStore {
	return &SharedStore{
		buffers:      make(map[string]*Buffer),
		namedBorrows: make(map[string][]*Buffer),
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
		Name:      name,
		Data:      make([]byte, size),
		Len:       size,
		refs:      1,
		Ownership: "omnivm",
	}
	s.buffers[name] = buf
	s.allocations++
	return buf, nil
}

// Get retrieves a named buffer.
func (s *SharedStore) Get(name string) (*Buffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok := s.buffers[name]
	if !ok {
		return nil, fmt.Errorf("arrow: buffer %q not found", name)
	}
	s.gets++
	return buf, nil
}

// Borrow returns a zero-copy lease for a named buffer. The lease keeps the
// backing memory alive until Release is called.
func (s *SharedStore) Borrow(name string) (*BorrowedBuffer, error) {
	return s.borrow(name, false)
}

func (s *SharedStore) borrowNamed(name string) (*BorrowedBuffer, error) {
	return s.borrow(name, true)
}

func (s *SharedStore) borrow(name string, trackNameRelease bool) (*BorrowedBuffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	buf, ok := s.buffers[name]
	if !ok {
		return nil, fmt.Errorf("arrow: buffer %q not found", name)
	}

	buf.mu.Lock()
	buf.refs++
	lease := &BorrowedBuffer{
		store:  s,
		name:   name,
		Buffer: buf,
		Len:    int64(buf.Len),
		Dtype:  buf.Dtype,
		Metadata: BufferMetadata{
			Dtype:             buf.Dtype,
			Format:            buf.Format,
			Shape:             append([]int64(nil), buf.Shape...),
			Strides:           append([]int64(nil), buf.Strides...),
			Offset:            buf.Offset,
			NullCount:         buf.NullCount,
			ValidityBytes:     int64(buf.ValidityLen),
			ValidityBitOffset: buf.ValidityBitOffset,
			ReadOnly:          buf.ReadOnly,
			Ownership:         buf.Ownership,
		},
	}
	if len(buf.Data) > 0 {
		lease.Data = unsafe.Pointer(&buf.Data[0])
	} else if buf.ExternalData != nil && buf.Len > 0 {
		lease.Data = buf.ExternalData
	}
	if len(buf.Validity) > 0 {
		lease.Validity = unsafe.Pointer(&buf.Validity[0])
		lease.ValidityLen = int64(len(buf.Validity))
	} else if buf.ExternalValidity != nil && buf.ValidityLen > 0 {
		lease.Validity = buf.ExternalValidity
		lease.ValidityLen = int64(buf.ValidityLen)
	}
	buf.mu.Unlock()

	if trackNameRelease {
		s.namedBorrows[name] = append(s.namedBorrows[name], buf)
	}
	s.gets++
	s.zeroCopyBorrows++
	return lease, nil
}

// Release ends the zero-copy lease. It is safe to call more than once.
func (b *BorrowedBuffer) Release() {
	if b == nil || b.store == nil {
		return
	}
	b.once.Do(func() {
		b.store.releaseBorrow(b.name, b.Buffer)
	})
}

func (s *SharedStore) releaseBorrow(name string, buf *Buffer) {
	s.mu.Lock()
	release := s.releaseBufferLocked(name, buf)
	s.mu.Unlock()
	_ = callBufferRelease(release)
}

func (s *SharedStore) releaseNamedBorrow(name string) error {
	s.mu.Lock()

	var buf *Buffer
	if queue := s.namedBorrows[name]; len(queue) > 0 {
		buf = queue[0]
		if len(queue) == 1 {
			delete(s.namedBorrows, name)
		} else {
			s.namedBorrows[name] = queue[1:]
		}
	} else {
		var ok bool
		buf, ok = s.buffers[name]
		if !ok {
			s.mu.Unlock()
			return fmt.Errorf("arrow: buffer %q not found", name)
		}
	}

	release := s.releaseBufferLocked(name, buf)
	s.mu.Unlock()
	return callBufferRelease(release)
}

func (s *SharedStore) releaseBufferLocked(name string, buf *Buffer) func() error {
	if buf == nil {
		return nil
	}

	buf.mu.Lock()
	buf.refs--
	refs := buf.refs
	release := buf.release
	buf.mu.Unlock()

	if refs <= 0 {
		if current, ok := s.buffers[name]; ok && current == buf {
			delete(s.buffers, name)
		}
		s.releases++
		return release
	}
	s.releases++
	return nil
}

// Free releases a named buffer.
func (s *SharedStore) Free(name string) error {
	s.mu.Lock()

	buf, ok := s.buffers[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("arrow: buffer %q not found", name)
	}

	buf.mu.Lock()
	buf.refs--
	refs := buf.refs
	release := buf.release
	buf.mu.Unlock()

	if refs <= 0 {
		delete(s.buffers, name)
	}
	s.releases++
	s.mu.Unlock()
	if refs <= 0 {
		return callBufferRelease(release)
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

// Metadata returns a stable metadata snapshot for the buffer.
func (b *Buffer) Metadata() BufferMetadata {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BufferMetadata{
		Dtype:             b.Dtype,
		Format:            b.Format,
		Shape:             append([]int64(nil), b.Shape...),
		Strides:           append([]int64(nil), b.Strides...),
		Offset:            b.Offset,
		NullCount:         b.NullCount,
		ValidityBytes:     int64(b.ValidityLen),
		ValidityBitOffset: b.ValidityBitOffset,
		ReadOnly:          b.ReadOnly,
		Ownership:         b.Ownership,
	}
}

// Stats returns a process-level diagnostics snapshot.
func (s *SharedStore) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	deferredLen, deferredOverflow := deferredReleaseStats()

	stats := Stats{
		LiveBuffers:      len(s.buffers),
		BuffersByDtype:   make(map[string]int),
		BuffersByFormat:  make(map[string]int),
		Allocations:      s.allocations,
		Sets:             s.sets,
		Gets:             s.gets,
		Releases:         s.releases,
		CopiedBytes:      s.copiedBytes,
		ZeroCopyBorrows:  s.zeroCopyBorrows,
		ZeroCopyImports:  s.zeroCopyImports,
		DeferredDrops:    s.deferredDrops,
		DeferredQueueLen: deferredLen,
		DeferredOverflow: deferredOverflow,
	}
	for name, buf := range s.buffers {
		buf.mu.Lock()
		size := int64(buf.Len)
		dtype := buf.Dtype
		format := buf.Format
		refs := buf.refs
		buf.mu.Unlock()

		stats.LiveBytes += size
		if refs > 1 {
			stats.ActiveBorrows += int64(refs - 1)
		}
		stats.BuffersByDtype[strconv.FormatInt(int64(dtype), 10)]++
		if format != "" {
			stats.BuffersByFormat[format]++
		}
		if size > stats.LargestBufferSize {
			stats.LargestBufferSize = size
			stats.LargestBufferName = name
		}
	}
	return stats
}

func (s *SharedStore) recordCopy(bytes int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes > 0 {
		s.copiedBytes += bytes
	}
}

func (s *SharedStore) recordBorrow() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zeroCopyBorrows++
}

func (s *SharedStore) recordZeroCopyImport() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.zeroCopyImports++
}

func (s *SharedStore) recordDeferredDrop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deferredDrops++
}

// Pointer returns an unsafe.Pointer to the buffer's underlying data,
// suitable for passing to C code in guest runtimes. The caller must
// ensure the buffer is not freed while the pointer is in use.
func (b *Buffer) Pointer() unsafe.Pointer {
	if len(b.Data) > 0 {
		return unsafe.Pointer(&b.Data[0])
	}
	return b.ExternalData
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

func callBufferRelease(release func() error) error {
	if release == nil {
		return nil
	}
	return release()
}
