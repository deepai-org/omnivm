package arrow

import "unsafe"

// BufGet fills output params from a named shared buffer.
// Returns 0 on success, -1 if not found.
func BufGet(name string, dataOut *unsafe.Pointer, lenOut *int64, dtypeOut *int32, readOnlyOut *bool) int {
	store := GlobalStore()
	lease, err := store.borrowNamed(name)
	if err != nil {
		return -1
	}

	*dataOut = lease.Data
	*lenOut = lease.Len
	*dtypeOut = lease.Dtype
	if readOnlyOut != nil {
		*readOnlyOut = lease.Metadata.ReadOnly
	}
	return 0
}

// BufSet stores data into the shared store under the given name.
// The data is copied from the provided pointer.
func BufSet(name string, data unsafe.Pointer, length int64, dtype int32, readOnly bool) int {
	store := GlobalStore()
	goData := make([]byte, length)
	if length > 0 && data != nil {
		copy(goData, unsafe.Slice((*byte)(data), length))
	}
	_, err := store.SetWithMetadata(name, goData, BufferMetadata{
		Dtype:    dtype,
		ReadOnly: readOnly,
	})
	if err != nil {
		return -1
	}
	store.recordCopy(length)
	return 0
}

// BufRelease schedules a deferred buffer release (safe from any thread).
func BufRelease(name string) {
	select {
	case DeferredRelease <- name:
	default:
		queueDeferredReleaseOverflow(name)
	}
}
