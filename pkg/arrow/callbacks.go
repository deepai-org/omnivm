package arrow

import (
	"encoding/json"
	"unsafe"
)

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

// BufRelease schedules deferred cleanup for a named borrow (safe from any
// thread). It does not release the named owner reference; use BufFree for
// explicit owner release.
func BufRelease(name string) {
	select {
	case DeferredRelease <- name:
	default:
		queueDeferredReleaseOverflow(name)
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
