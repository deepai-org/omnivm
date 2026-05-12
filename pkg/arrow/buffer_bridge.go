package arrow

/*
#include <stdint.h>
#include <string.h>

// omni_buffer_t matches the definition in omni_bridge.h.
// Duplicated here to avoid cgo include path issues across packages.
typedef struct {
    void*   data;
    int64_t len;
    int32_t dtype;
    int8_t  owned;
} omni_buffer_t;
*/
import "C"
import (
	"unsafe"
)

// Dtype constants matching omni_bridge.h.
const (
	DtypeBytes = 0
	DtypeI32   = 1
	DtypeI64   = 2
	DtypeF32   = 3
	DtypeF64   = 4
	DtypeUTF8  = 5
)

// BufferInfo is the Go-side representation of a shared buffer with type info.
type BufferInfo struct {
	Name  string
	Dtype int32
}

// SetWithDtype stores a buffer with explicit dtype metadata.
func (s *SharedStore) SetWithDtype(name string, data []byte, dtype int32) (*Buffer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Replace existing buffer if present
	if existing, ok := s.buffers[name]; ok {
		existing.mu.Lock()
		existing.Data = data
		existing.Len = len(data)
		existing.refs = 1
		existing.Dtype = dtype
		existing.mu.Unlock()
		return existing, nil
	}

	buf := &Buffer{
		Name:  name,
		Data:  data,
		Len:   len(data),
		refs:  1,
		Dtype: dtype,
	}
	s.buffers[name] = buf
	return buf, nil
}

// ToCBuffer fills an omni_buffer_t from a named buffer.
// Returns true on success, false if buffer not found.
func (s *SharedStore) ToCBuffer(name string, out *C.omni_buffer_t) bool {
	s.mu.RLock()
	buf, ok := s.buffers[name]
	s.mu.RUnlock()
	if !ok {
		return false
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

	if len(buf.Data) == 0 {
		out.data = nil
		out.len = 0
	} else {
		out.data = unsafe.Pointer(&buf.Data[0])
		out.len = C.int64_t(buf.Len)
	}
	out.dtype = C.int32_t(buf.Dtype)
	out.owned = 0 // borrowed by default
	return true
}

// FromCBuffer creates or replaces a named buffer from an omni_buffer_t.
func (s *SharedStore) FromCBuffer(name string, in *C.omni_buffer_t) {
	size := int(in.len)
	data := make([]byte, size)
	if size > 0 && in.data != nil {
		C.memcpy(unsafe.Pointer(&data[0]), in.data, C.size_t(size))
	}
	s.SetWithDtype(name, data, int32(in.dtype))
}

// DeferredRelease is a channel for buffer release requests from GC threads.
// The Golden Thread drains this on each pump cycle.
var DeferredRelease = make(chan string, 256)

// DrainDeferred processes all pending deferred buffer releases.
// Must be called from the Golden Thread (or any single-threaded context).
func (s *SharedStore) DrainDeferred() {
	for {
		select {
		case name := <-DeferredRelease:
			s.Free(name)
		default:
			return
		}
	}
}
