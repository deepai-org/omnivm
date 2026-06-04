package arrow

import (
	"encoding/json"
	"unsafe"
)

// BufGet fills output params from a named shared buffer.
// Returns 0 on success, -1 if not found.
func BufGet(name string, dataOut *unsafe.Pointer, lenOut *int64, dtypeOut *int32, readOnlyOut *bool) int {
	clearBufGetOutputs(dataOut, lenOut, dtypeOut, readOnlyOut)
	if dataOut == nil || lenOut == nil || dtypeOut == nil {
		return -1
	}
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
	if length < 0 || int64(int(length)) != length {
		return -1
	}
	if length > 0 && data == nil {
		return -1
	}
	store := GlobalStore()
	goData := make([]byte, length)
	if length > 0 {
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

func clearBufGetOutputs(dataOut *unsafe.Pointer, lenOut *int64, dtypeOut *int32, readOnlyOut *bool) {
	if dataOut != nil {
		*dataOut = nil
	}
	if lenOut != nil {
		*lenOut = 0
	}
	if dtypeOut != nil {
		*dtypeOut = 0
	}
	if readOnlyOut != nil {
		*readOnlyOut = false
	}
}

// BufRelease schedules deferred cleanup for a named borrow (safe from any
// thread). It does not release the named owner reference; use BufFree for
// explicit owner release.
func BufRelease(name string) {
	select {
	case DeferredRelease <- name:
	default:
		if !queueDeferredReleaseOverflow(name) {
			GlobalStore().recordDeferredDrop()
		}
	}
}

// BufFree releases the named owner reference immediately. Active borrowed
// views stay valid until their own BufRelease calls arrive.
func BufFree(name string) error {
	return GlobalStore().Free(name)
}

// BufStatusJSON returns a JSON lifecycle diagnostic for a named buffer.
func BufStatusJSON(name string) string {
	status := GlobalStore().Status(name)
	data, err := json.Marshal(status)
	if err != nil {
		return `{"name":` + jsonString(name) + `,"state":"error","live":false,"released":false}`
	}
	return string(data)
}

func jsonString(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(data)
}
