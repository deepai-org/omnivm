package arrow

/*
#include <stdint.h>
#include <stdlib.h>
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

//export omniArrowReleaseSchemaHandle
func omniArrowReleaseSchemaHandle(ptr unsafe.Pointer) {
	if ptr == nil {
		return
	}
	handle := cgo.Handle(*(*C.uintptr_t)(ptr))
	C.free(ptr)
	state, ok := handle.Value().(*arrowSchemaState)
	if ok && state != nil {
		if state.format != nil {
			C.free(unsafe.Pointer(state.format))
			state.format = nil
		}
		if state.name != nil {
			C.free(unsafe.Pointer(state.name))
			state.name = nil
		}
	}
	handle.Delete()
}

//export omniArrowReleaseArrayHandle
func omniArrowReleaseArrayHandle(ptr unsafe.Pointer) {
	if ptr == nil {
		return
	}
	handle := cgo.Handle(*(*C.uintptr_t)(ptr))
	C.free(ptr)
	state, ok := handle.Value().(*arrowArrayState)
	if ok && state != nil {
		if state.buffers != nil {
			C.free(state.buffers)
			state.buffers = nil
		}
		if state.lease != nil {
			state.lease.Release()
			state.lease = nil
		}
	}
	handle.Delete()
}
