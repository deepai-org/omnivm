// Package polyglot provides the typed value bridge for cross-runtime calls.
//
// omni_value_t is a 32-byte tagged union that carries scalars (bool, int64,
// float64), strings, byte buffers, and opaque object references between
// runtimes without JSON serialization.
package polyglot

/*
#include "omni_bridge.h"
#include <stdlib.h>
#include <string.h>
*/
import "C"
import (
	"strconv"
	"unsafe"
)

// Tag constants matching omni_bridge.h.
const (
	TagNull   = 0
	TagBool   = 1
	TagI64    = 2
	TagF64    = 3
	TagString = 4
	TagBytes  = 5
	TagRef    = 6
	TagError  = 7
)

// Value is the Go representation of omni_value_t.
type Value struct {
	Tag    int64
	Int    int64   // TagBool (0/1), TagI64
	Float  float64 // TagF64
	Str    string  // TagString, TagError
	Bytes  []byte  // TagBytes
	Ref    uint64  // TagRef
}

// Null returns a null Value.
func Null() Value { return Value{Tag: TagNull} }

// Bool returns a boolean Value.
func Bool(b bool) Value {
	v := Value{Tag: TagBool}
	if b {
		v.Int = 1
	}
	return v
}

// I64 returns an int64 Value.
func I64(i int64) Value { return Value{Tag: TagI64, Int: i} }

// F64 returns a float64 Value.
func F64(f float64) Value { return Value{Tag: TagF64, Float: f} }

// String returns a string Value.
func String(s string) Value { return Value{Tag: TagString, Str: s} }

// Bytes returns a bytes Value.
func BytesVal(b []byte) Value { return Value{Tag: TagBytes, Bytes: b} }

// Error returns an error Value.
func Error(msg string) Value { return Value{Tag: TagError, Str: msg} }

// Ref returns an object reference Value.
func Ref(handle uint64) Value { return Value{Tag: TagRef, Ref: handle} }

// IsError returns true if this value represents an error.
func (v Value) IsError() bool { return v.Tag == TagError }

// IsNull returns true if this value is null.
func (v Value) IsNull() bool { return v.Tag == TagNull }

// CValueSize is the size of omni_value_t (24 bytes: 8-byte tag + 16-byte union).
const CValueSize = 24

// ToCValueRaw writes a Value into a 32-byte C omni_value_t at the given pointer.
// String/bytes data is copied into C-allocated memory (caller must free via FreeCValueRaw).
func (v Value) ToCValueRaw(ptr unsafe.Pointer) {
	// Zero the struct
	for i := 0; i < CValueSize; i++ {
		*(*byte)(unsafe.Pointer(uintptr(ptr) + uintptr(i))) = 0
	}
	// Write tag
	*(*int64)(ptr) = v.Tag
	valPtr := unsafe.Pointer(uintptr(ptr) + 8) // offset to union
	switch v.Tag {
	case TagNull:
		// already zeroed
	case TagBool, TagI64:
		*(*int64)(valPtr) = v.Int
	case TagF64:
		*(*float64)(valPtr) = v.Float
	case TagString, TagError:
		cstr := C.CString(v.Str)
		*(*uintptr)(valPtr) = uintptr(unsafe.Pointer(cstr))
		*(*int64)(unsafe.Pointer(uintptr(valPtr) + 8)) = int64(len(v.Str))
	case TagBytes:
		if len(v.Bytes) > 0 {
			cptr := C.malloc(C.size_t(len(v.Bytes)))
			C.memcpy(cptr, unsafe.Pointer(&v.Bytes[0]), C.size_t(len(v.Bytes)))
			*(*uintptr)(valPtr) = uintptr(cptr)
			*(*int64)(unsafe.Pointer(uintptr(valPtr) + 8)) = int64(len(v.Bytes))
		}
	case TagRef:
		*(*uint64)(valPtr) = v.Ref
	}
}

// FromCValueRaw reads a Value from a 32-byte C omni_value_t at the given pointer.
// String/bytes data is copied into Go-managed memory.
func FromCValueRaw(ptr unsafe.Pointer) Value {
	tag := *(*int64)(ptr)
	valPtr := unsafe.Pointer(uintptr(ptr) + 8)
	v := Value{Tag: tag}
	switch tag {
	case TagNull:
		// nothing
	case TagBool, TagI64:
		v.Int = *(*int64)(valPtr)
	case TagF64:
		v.Float = *(*float64)(valPtr)
	case TagString, TagError:
		sptr := (*[0]byte)(unsafe.Pointer(*(*uintptr)(valPtr)))
		length := *(*int64)(unsafe.Pointer(uintptr(valPtr) + 8))
		if sptr != nil && length > 0 {
			v.Str = C.GoStringN((*C.char)(unsafe.Pointer(sptr)), C.int(length))
		}
	case TagBytes:
		bptr := unsafe.Pointer(*(*uintptr)(valPtr))
		length := *(*int64)(unsafe.Pointer(uintptr(valPtr) + 8))
		if bptr != nil && length > 0 {
			v.Bytes = C.GoBytes(bptr, C.int(length))
		}
	case TagRef:
		v.Ref = *(*uint64)(valPtr)
	}
	return v
}

// FreeCValueRaw frees C-allocated memory in a omni_value_t at the given pointer.
func FreeCValueRaw(ptr unsafe.Pointer) {
	tag := *(*int64)(ptr)
	if tag == TagString || tag == TagError || tag == TagBytes {
		sptr := unsafe.Pointer(*(*uintptr)(unsafe.Pointer(uintptr(ptr) + 8)))
		if sptr != nil {
			C.free(sptr)
		}
	}
}

// Convenience wrappers using package-local C types (for tests and polyglot package internal use)

func (v Value) ToCValue() C.omni_value_t {
	var cv C.omni_value_t
	v.ToCValueRaw(unsafe.Pointer(&cv))
	return cv
}

func FromCValue(cv C.omni_value_t) Value {
	return FromCValueRaw(unsafe.Pointer(&cv))
}

func FreeCValue(cv C.omni_value_t) {
	FreeCValueRaw(unsafe.Pointer(&cv))
}

// ToGoString converts any Value to its string representation (for fallback).
func (v Value) ToGoString() string {
	switch v.Tag {
	case TagNull:
		return ""
	case TagBool:
		if v.Int != 0 {
			return "true"
		}
		return "false"
	case TagI64:
		return strconv.FormatInt(v.Int, 10)
	case TagF64:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case TagString:
		return v.Str
	case TagError:
		return "ERR:" + v.Str
	case TagBytes:
		return string(v.Bytes)
	case TagRef:
		return "<ref:" + strconv.FormatUint(v.Ref, 10) + ">"
	default:
		return ""
	}
}
