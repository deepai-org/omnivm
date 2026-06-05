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
    int8_t  read_only;
} omni_buffer_t;
*/
import "C"
import (
	"errors"
	"fmt"
	"sync"
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
	DtypeI16   = 6
	DtypeU16   = 7
	DtypeU32   = 8
	DtypeU64   = 9
	DtypeI8    = 10
	DtypeU8    = 11
)

// BufferInfo is the Go-side representation of a shared buffer with type info.
type BufferInfo struct {
	Name  string
	Dtype int32
}

// BorrowCBuffer fills an omni_buffer_t from a named buffer and returns a lease
// that must be released when the adapter is finished with the pointer.
func (s *SharedStore) BorrowCBuffer(name string, out *C.omni_buffer_t) (*BorrowedBuffer, bool) {
	clearCBuffer(out)
	if out == nil {
		return nil, false
	}
	lease, err := s.Borrow(name)
	if err != nil {
		return nil, false
	}
	out.data = lease.Data
	out.len = C.int64_t(lease.Len)
	out.dtype = C.int32_t(lease.Dtype)
	out.owned = 0
	if lease.Metadata.ReadOnly {
		out.read_only = 1
	} else {
		out.read_only = 0
	}
	return lease, true
}

// SetWithDtype stores a buffer with explicit dtype metadata.
func (s *SharedStore) SetWithDtype(name string, data []byte, dtype int32) (*Buffer, error) {
	return s.SetWithMetadata(name, data, BufferMetadata{Dtype: dtype})
}

// SetWithMetadata stores a buffer with generic Arrow-compatible metadata.
func (s *SharedStore) SetWithMetadata(name string, data []byte, meta BufferMetadata) (*Buffer, error) {
	return s.SetWithValidityMetadata(name, data, nil, meta)
}

// SetWithValidityMetadata stores a buffer and optional Arrow validity bitmap
// with generic Arrow-compatible metadata.
func (s *SharedStore) SetWithValidityMetadata(name string, data []byte, validity []byte, meta BufferMetadata) (buf *Buffer, err error) {
	if err := validateBufferMetadata(name, meta); err != nil {
		return nil, err
	}
	s.mu.Lock()
	var release func() error
	defer func() {
		s.mu.Unlock()
		if releaseErr := callBufferRelease(release); releaseErr != nil {
			s.recordReleaseFailure(name, releaseErr)
			err = errors.Join(err, releaseErr)
		}
	}()

	s.forgetReleasedLocked(name)

	// Replace existing buffer if present
	if existing, ok := s.buffers[name]; ok {
		existing.mu.Lock()
		if existing.refs > 1 {
			existing.ownerRef = false
			if existing.refs > 0 {
				existing.refs--
			}
			refs := existing.refs
			existing.mu.Unlock()
			if refs > 0 {
				s.detached[existing] = struct{}{}
			}
			buf = &Buffer{
				Name:     name,
				Data:     data,
				Len:      len(data),
				Validity: validity,
				refs:     1,
				ownerRef: true,
				store:    s,
			}
			applyMetadataLocked(buf, meta)
			s.buffers[name] = buf
			s.sets++
			return buf, nil
		}
		release = existing.release
		existing.Data = data
		existing.ExternalData = nil
		existing.Len = len(data)
		existing.Validity = validity
		existing.ExternalValidity = nil
		existing.ValidityLen = len(validity)
		existing.refs = 1
		existing.borrowRefs = 0
		existing.ownerRef = true
		existing.store = s
		existing.release = nil
		applyMetadataLocked(existing, meta)
		existing.mu.Unlock()
		s.sets++
		return existing, nil
	}

	buf = &Buffer{
		Name:     name,
		Data:     data,
		Len:      len(data),
		Validity: validity,
		refs:     1,
		ownerRef: true,
		store:    s,
	}
	applyMetadataLocked(buf, meta)
	s.buffers[name] = buf
	s.sets++
	return buf, nil
}

// SetExternalWithMetadata stores a producer-owned memory region with explicit
// release ownership. Runtime adapters use this for Arrow C Data imports where
// the producer has transferred lifetime control to OmniVM.
func (s *SharedStore) SetExternalWithMetadata(name string, data unsafe.Pointer, length int64, meta BufferMetadata, release func() error) (*Buffer, error) {
	return s.SetExternalArrowWithMetadata(name, data, length, nil, 0, meta, release)
}

// SetExternalArrowWithMetadata stores producer-owned value and validity buffers
// under one release callback. Runtime adapters use this for Arrow C Data imports
// where the producer has transferred lifetime control to OmniVM.
func (s *SharedStore) SetExternalArrowWithMetadata(name string, data unsafe.Pointer, length int64, validity unsafe.Pointer, validityLength int64, meta BufferMetadata, release func() error) (buf *Buffer, err error) {
	fail := func(err error) (*Buffer, error) {
		return nil, errors.Join(err, callBufferRelease(release))
	}
	if err := validateBufferMetadata(name, meta); err != nil {
		return fail(err)
	}
	if length < 0 {
		return fail(fmt.Errorf("arrow: external buffer %q has negative length", name))
	}
	if int64(int(length)) != length {
		return fail(fmt.Errorf("arrow: external buffer %q length %d overflows int", name, length))
	}
	if length > 0 && data == nil {
		return fail(fmt.Errorf("arrow: external buffer %q has nil data", name))
	}
	if validityLength < 0 {
		return fail(fmt.Errorf("arrow: external buffer %q has negative validity length", name))
	}
	if int64(int(validityLength)) != validityLength {
		return fail(fmt.Errorf("arrow: external buffer %q validity length %d overflows int", name, validityLength))
	}
	if validityLength > 0 && validity == nil {
		return fail(fmt.Errorf("arrow: external buffer %q has nil validity bitmap", name))
	}

	s.mu.Lock()
	var oldRelease func() error
	defer func() {
		s.mu.Unlock()
		if releaseErr := callBufferRelease(oldRelease); releaseErr != nil {
			s.recordReleaseFailure(name, releaseErr)
			err = errors.Join(err, releaseErr)
		}
	}()

	s.forgetReleasedLocked(name)

	if existing, ok := s.buffers[name]; ok {
		existing.mu.Lock()
		if existing.refs > 1 {
			existing.ownerRef = false
			if existing.refs > 0 {
				existing.refs--
			}
			refs := existing.refs
			existing.mu.Unlock()
			if refs > 0 {
				s.detached[existing] = struct{}{}
			}
			buf := &Buffer{
				Name:             name,
				ExternalData:     data,
				Len:              int(length),
				ExternalValidity: validity,
				ValidityLen:      int(validityLength),
				refs:             1,
				ownerRef:         true,
				store:            s,
				release:          release,
			}
			applyMetadataLocked(buf, meta)
			s.buffers[name] = buf
			s.sets++
			s.zeroCopyImports++
			return buf, nil
		}
		oldRelease = existing.release
		existing.Data = nil
		existing.ExternalData = data
		existing.Len = int(length)
		existing.Validity = nil
		existing.ExternalValidity = validity
		existing.ValidityLen = int(validityLength)
		existing.refs = 1
		existing.borrowRefs = 0
		existing.ownerRef = true
		existing.store = s
		existing.release = release
		applyMetadataLocked(existing, meta)
		existing.mu.Unlock()
		s.sets++
		s.zeroCopyImports++
		return existing, nil
	}

	buf = &Buffer{
		Name:             name,
		ExternalData:     data,
		Len:              int(length),
		ExternalValidity: validity,
		ValidityLen:      int(validityLength),
		refs:             1,
		ownerRef:         true,
		store:            s,
		release:          release,
	}
	applyMetadataLocked(buf, meta)
	s.buffers[name] = buf
	s.sets++
	s.zeroCopyImports++
	return buf, nil
}

// ToCBuffer fills an omni_buffer_t from a named buffer without retaining it.
// New adapters should use BorrowCBuffer so the pointer has an explicit lease.
// Returns true on success, false if buffer not found.
func (s *SharedStore) ToCBuffer(name string, out *C.omni_buffer_t) bool {
	clearCBuffer(out)
	if out == nil {
		return false
	}
	s.mu.RLock()
	buf, ok := s.buffers[name]
	s.mu.RUnlock()
	if !ok {
		return false
	}

	buf.mu.Lock()
	defer buf.mu.Unlock()

	if ptr := buf.Pointer(); ptr == nil || buf.Len == 0 {
		out.data = nil
		out.len = 0
	} else {
		out.data = ptr
		out.len = C.int64_t(buf.Len)
	}
	out.dtype = C.int32_t(buf.Dtype)
	out.owned = 0 // borrowed by default
	if buf.ReadOnly {
		out.read_only = 1
	} else {
		out.read_only = 0
	}
	return true
}

// FromCBuffer creates or replaces a named buffer from an omni_buffer_t.
func (s *SharedStore) FromCBuffer(name string, in *C.omni_buffer_t) {
	if in == nil || in.len < 0 || int64(int(in.len)) != int64(in.len) || (in.len > 0 && in.data == nil) {
		return
	}
	size := int(in.len)
	data := make([]byte, size)
	if size > 0 && in.data != nil {
		C.memcpy(unsafe.Pointer(&data[0]), in.data, C.size_t(size))
	}
	s.SetWithMetadata(name, data, BufferMetadata{
		Dtype:    int32(in.dtype),
		ReadOnly: in.read_only != 0,
	})
}

func clearCBuffer(out *C.omni_buffer_t) {
	if out == nil {
		return
	}
	out.data = nil
	out.len = 0
	out.dtype = 0
	out.owned = 0
	out.read_only = 0
}

func validateBufferMetadata(name string, meta BufferMetadata) error {
	if meta.MemorySpace != "" && meta.MemorySpace != "host" {
		return fmt.Errorf("arrow: buffer %q memory_space %q is not host-accessible", name, meta.MemorySpace)
	}
	if meta.Offset < 0 {
		return fmt.Errorf("arrow: buffer %q offset %d is negative", name, meta.Offset)
	}
	if meta.NullCount < -1 {
		return fmt.Errorf("arrow: buffer %q null_count %d is invalid", name, meta.NullCount)
	}
	if meta.ValidityBytes < 0 {
		return fmt.Errorf("arrow: buffer %q validity_bytes %d is negative", name, meta.ValidityBytes)
	}
	if meta.ValidityBitOffset < 0 {
		return fmt.Errorf("arrow: buffer %q validity_bit_offset %d is negative", name, meta.ValidityBitOffset)
	}
	if len(meta.Strides) > 0 && len(meta.Shape) == 0 {
		return fmt.Errorf("arrow: buffer %q has strides without shape", name)
	}
	if len(meta.Strides) > 0 && len(meta.Strides) != len(meta.Shape) {
		return fmt.Errorf("arrow: buffer %q shape %v has mismatched strides %v", name, meta.Shape, meta.Strides)
	}
	product := int64(1)
	for _, dim := range meta.Shape {
		if dim < 0 {
			return fmt.Errorf("arrow: buffer %q has negative shape dimension in %v", name, meta.Shape)
		}
		if dim == 0 {
			product = 0
			continue
		}
		if product != 0 && dim > (1<<63-1)/product {
			return fmt.Errorf("arrow: buffer %q shape %v overflows logical element count", name, meta.Shape)
		}
		product *= dim
	}
	return nil
}

func applyMetadataLocked(buf *Buffer, meta BufferMetadata) {
	buf.Dtype = meta.Dtype
	buf.Format = meta.Format
	buf.Shape = append([]int64(nil), meta.Shape...)
	buf.Strides = append([]int64(nil), meta.Strides...)
	buf.Offset = meta.Offset
	buf.NullCount = meta.NullCount
	if len(buf.Validity) > 0 {
		buf.ValidityLen = len(buf.Validity)
	} else if meta.ValidityBytes > 0 {
		buf.ValidityLen = int(meta.ValidityBytes)
	}
	buf.ValidityBitOffset = meta.ValidityBitOffset
	buf.ReadOnly = meta.ReadOnly
	buf.Ownership = meta.Ownership
	if buf.Ownership == "" {
		buf.Ownership = "omnivm"
	}
	buf.MemorySpace = meta.MemorySpace
	if buf.MemorySpace == "" {
		buf.MemorySpace = "host"
	}
}

// DeferredRelease is the fast path for named-borrow cleanup requests from GC
// threads. The Golden Thread drains this on each pump cycle. Overflow releases
// are kept in deferredReleaseOverflow so a saturated channel cannot silently
// leak a borrowed buffer. Distinct overflow names are bounded so finalizer
// storms cannot grow memory without limit; pressure is reported in store stats.
var DeferredRelease = make(chan string, 256)

const maxDeferredReleaseOverflowNames = 4096

var deferredReleaseOverflow struct {
	sync.Mutex
	counts map[string]int64
	order  []string
	total  int
}

// DrainDeferred processes all pending deferred named-borrow releases.
// Must be called from the Golden Thread (or any single-threaded context).
func (s *SharedStore) DrainDeferred() {
	for {
		drained := false
		select {
		case name := <-DeferredRelease:
			_ = s.releaseNamedBorrow(name)
			drained = true
		default:
		}
		if drained {
			continue
		}

		name, ok := nextDeferredReleaseOverflow()
		if !ok {
			return
		}
		_ = s.releaseNamedBorrow(name)
	}
}

func queueDeferredReleaseOverflow(name string) bool {
	deferredReleaseOverflow.Lock()
	defer deferredReleaseOverflow.Unlock()
	if deferredReleaseOverflow.counts == nil {
		deferredReleaseOverflow.counts = make(map[string]int64)
	}
	if deferredReleaseOverflow.counts[name] == 0 {
		if len(deferredReleaseOverflow.counts) >= maxDeferredReleaseOverflowNames {
			return false
		}
		deferredReleaseOverflow.order = append(deferredReleaseOverflow.order, name)
	}
	deferredReleaseOverflow.counts[name]++
	deferredReleaseOverflow.total++
	return true
}

func nextDeferredReleaseOverflow() (string, bool) {
	deferredReleaseOverflow.Lock()
	defer deferredReleaseOverflow.Unlock()
	if len(deferredReleaseOverflow.order) == 0 {
		return "", false
	}
	name := deferredReleaseOverflow.order[0]
	deferredReleaseOverflow.counts[name]--
	deferredReleaseOverflow.total--
	if deferredReleaseOverflow.counts[name] <= 0 {
		delete(deferredReleaseOverflow.counts, name)
		copy(deferredReleaseOverflow.order, deferredReleaseOverflow.order[1:])
		deferredReleaseOverflow.order[len(deferredReleaseOverflow.order)-1] = ""
		deferredReleaseOverflow.order = deferredReleaseOverflow.order[:len(deferredReleaseOverflow.order)-1]
	}
	return name, true
}

func deferredReleaseStats() (queueLen int, overflowNames int) {
	deferredReleaseOverflow.Lock()
	defer deferredReleaseOverflow.Unlock()
	return len(DeferredRelease) + deferredReleaseOverflow.total, len(deferredReleaseOverflow.counts)
}
