package arrow

import "unsafe"

// BufGet fills output params from a named shared buffer.
// Returns 0 on success, -1 if not found.
func BufGet(name string, dataOut *unsafe.Pointer, lenOut *int64, dtypeOut *int32) int {
	store := GlobalStore()
	buf, err := store.Get(name)
	if err != nil {
		return -1
	}

	buf.Retain() // caller borrows; must release when done
	if len(buf.Data) == 0 {
		*dataOut = nil
		*lenOut = 0
	} else {
		*dataOut = unsafe.Pointer(&buf.Data[0])
		*lenOut = int64(buf.Len)
	}
	*dtypeOut = buf.Dtype
	return 0
}

// BufSet stores data into the shared store under the given name.
// The data is copied from the provided pointer.
func BufSet(name string, data unsafe.Pointer, length int64, dtype int32) int {
	store := GlobalStore()
	goData := make([]byte, length)
	if length > 0 && data != nil {
		copy(goData, unsafe.Slice((*byte)(data), length))
	}
	_, err := store.SetWithDtype(name, goData, dtype)
	if err != nil {
		return -1
	}
	return 0
}

// BufRelease schedules a deferred buffer release (safe from any thread).
func BufRelease(name string) {
	select {
	case DeferredRelease <- name:
	default:
		// Channel full — drop. Buffer will leak but won't crash.
	}
}
