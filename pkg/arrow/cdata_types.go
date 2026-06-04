package arrow

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct ArrowSchema {
    const char* format;
    const char* name;
    const char* metadata;
    int64_t flags;
    int64_t n_children;
    struct ArrowSchema** children;
    struct ArrowSchema* dictionary;
    void (*release)(struct ArrowSchema*);
    void* private_data;
} ArrowSchema;

typedef struct ArrowArray {
    int64_t length;
    int64_t null_count;
    int64_t offset;
    int64_t n_buffers;
    int64_t n_children;
    const void** buffers;
    struct ArrowArray** children;
    struct ArrowArray* dictionary;
    void (*release)(struct ArrowArray*);
    void* private_data;
} ArrowArray;

extern void omniArrowReleaseSchemaHandle(void* handle);
extern void omniArrowReleaseArrayHandle(void* handle);

static ArrowSchema* omni_arrow_alloc_schema(void) {
    return (ArrowSchema*)calloc(1, sizeof(ArrowSchema));
}

static ArrowArray* omni_arrow_alloc_array(void) {
    return (ArrowArray*)calloc(1, sizeof(ArrowArray));
}

static const void** omni_arrow_alloc_buffers(int64_t n) {
    return (const void**)calloc((size_t)n, sizeof(void*));
}

static uintptr_t* omni_arrow_alloc_handle(uintptr_t handle) {
    uintptr_t* out = (uintptr_t*)malloc(sizeof(uintptr_t));
    if (!out) return NULL;
    *out = handle;
    return out;
}

static void omni_arrow_release_schema(ArrowSchema* schema) {
    if (!schema || !schema->release) return;
    void* handle = schema->private_data;
    schema->release = NULL;
    schema->private_data = NULL;
    omniArrowReleaseSchemaHandle(handle);
}

static void omni_arrow_release_array(ArrowArray* array) {
    if (!array || !array->release) return;
    void* handle = array->private_data;
    array->release = NULL;
    array->private_data = NULL;
    omniArrowReleaseArrayHandle(handle);
}

void omni_arrow_schema_release_callback(ArrowSchema* schema) {
    omni_arrow_release_schema(schema);
}

void omni_arrow_array_release_callback(ArrowArray* array) {
    omni_arrow_release_array(array);
}

static void omni_arrow_release_schema_if_live(ArrowSchema* schema) {
    if (schema && schema->release) schema->release(schema);
}

static void omni_arrow_release_array_if_live(ArrowArray* array) {
    if (array && array->release) array->release(array);
}

static const void* omni_arrow_array_buffer(ArrowArray* array, int64_t i) {
    if (!array || !array->buffers || i < 0 || i >= array->n_buffers) return NULL;
    return array->buffers[i];
}

static void omni_arrow_array_set_buffer(ArrowArray* array, int64_t i, const void* buffer) {
    if (!array || !array->buffers || i < 0 || i >= array->n_buffers) return;
    array->buffers[i] = buffer;
}

static void omni_arrow_copy_schema(ArrowSchema* dst, const ArrowSchema* src) {
    if (!dst || !src) return;
    *dst = *src;
}

static void omni_arrow_copy_array(ArrowArray* dst, const ArrowArray* src) {
    if (!dst || !src) return;
    *dst = *src;
}
*/
import "C"

import (
	"fmt"
	"math"
	"runtime/cgo"
	"sync"
	"unsafe"
)

// ArrowCData is an adapter-facing lease exported through the Arrow C Data
// Interface. It is intentionally not a user-visible .poly API.
type ArrowCData struct {
	Schema *C.ArrowSchema
	Array  *C.ArrowArray

	once sync.Once
}

type arrowSchemaState struct {
	format *C.char
	name   *C.char
}

type arrowArrayState struct {
	lease   *BorrowedBuffer
	buffers unsafe.Pointer
}

// BorrowCArrowArray exports a named contiguous primitive buffer as Arrow C Data.
func (s *SharedStore) BorrowCArrowArray(name string) (*ArrowCData, error) {
	lease, err := s.Borrow(name)
	if err != nil {
		return nil, err
	}

	format, elemSize, err := arrowPrimitiveFormat(lease.Dtype, lease.Metadata.Format)
	if err != nil {
		lease.Release()
		return nil, err
	}
	if elemSize <= 0 || lease.Len%int64(elemSize) != 0 {
		lease.Release()
		return nil, fmt.Errorf("arrow: buffer %q length %d is not aligned to element size %d", name, lease.Len, elemSize)
	}
	if lease.Metadata.NullCount > 0 && lease.Validity == nil {
		lease.Release()
		return nil, fmt.Errorf("arrow: buffer %q has null_count=%d but no validity bitmap", name, lease.Metadata.NullCount)
	}
	logicalLength, logicalOffset, err := cArrowLogicalPrimitiveView(name, lease.Metadata, lease.Len, elemSize)
	if err != nil {
		lease.Release()
		return nil, err
	}

	schema := C.omni_arrow_alloc_schema()
	array := C.omni_arrow_alloc_array()
	buffers := C.omni_arrow_alloc_buffers(2)
	if schema == nil || array == nil || buffers == nil {
		if schema != nil {
			C.free(unsafe.Pointer(schema))
		}
		if array != nil {
			C.free(unsafe.Pointer(array))
		}
		if buffers != nil {
			C.free(unsafe.Pointer(buffers))
		}
		lease.Release()
		return nil, fmt.Errorf("arrow: failed to allocate Arrow C Data descriptors")
	}

	schemaState := &arrowSchemaState{
		format: C.CString(format),
		name:   C.CString(name),
	}
	if schemaState.format == nil || schemaState.name == nil {
		if schemaState.format != nil {
			C.free(unsafe.Pointer(schemaState.format))
		}
		if schemaState.name != nil {
			C.free(unsafe.Pointer(schemaState.name))
		}
		C.free(unsafe.Pointer(schema))
		C.free(unsafe.Pointer(array))
		C.free(unsafe.Pointer(buffers))
		lease.Release()
		return nil, fmt.Errorf("arrow: failed to allocate Arrow C Data schema strings")
	}

	arrayState := &arrowArrayState{
		lease:   lease,
		buffers: unsafe.Pointer(buffers),
	}
	schemaHandle := cgo.NewHandle(schemaState)
	arrayHandle := cgo.NewHandle(arrayState)
	schemaHandlePtr := C.omni_arrow_alloc_handle(C.uintptr_t(schemaHandle))
	arrayHandlePtr := C.omni_arrow_alloc_handle(C.uintptr_t(arrayHandle))
	if schemaHandlePtr == nil || arrayHandlePtr == nil {
		if schemaHandlePtr != nil {
			C.free(unsafe.Pointer(schemaHandlePtr))
		}
		if arrayHandlePtr != nil {
			C.free(unsafe.Pointer(arrayHandlePtr))
		}
		schemaHandle.Delete()
		arrayHandle.Delete()
		C.free(unsafe.Pointer(schemaState.format))
		C.free(unsafe.Pointer(schemaState.name))
		C.free(unsafe.Pointer(schema))
		C.free(unsafe.Pointer(array))
		C.free(unsafe.Pointer(buffers))
		lease.Release()
		return nil, fmt.Errorf("arrow: failed to allocate Arrow C Data private handles")
	}

	schema.format = schemaState.format
	schema.name = schemaState.name
	schema.metadata = nil
	schema.flags = 0
	schema.n_children = 0
	schema.children = nil
	schema.dictionary = nil
	schema.release = (*[0]byte)(C.omni_arrow_schema_release_callback)
	schema.private_data = unsafe.Pointer(schemaHandlePtr)

	array.length = C.int64_t(logicalLength)
	array.null_count = C.int64_t(lease.Metadata.NullCount)
	array.offset = C.int64_t(logicalOffset / int64(elemSize))
	array.n_buffers = 2
	array.n_children = 0
	array.buffers = buffers
	array.children = nil
	array.dictionary = nil
	array.release = (*[0]byte)(C.omni_arrow_array_release_callback)
	array.private_data = unsafe.Pointer(arrayHandlePtr)
	C.omni_arrow_array_set_buffer(array, 0, nil)
	if lease.Validity != nil && lease.Metadata.NullCount > 0 {
		C.omni_arrow_array_set_buffer(array, 0, lease.Validity)
	}
	C.omni_arrow_array_set_buffer(array, 1, lease.Data)

	return &ArrowCData{Schema: schema, Array: array}, nil
}

func cArrowLogicalPrimitiveView(name string, meta BufferMetadata, byteLen int64, elemSize int) (int64, int64, error) {
	elemBytes := int64(elemSize)
	if elemBytes <= 0 {
		return 0, 0, fmt.Errorf("arrow: buffer %q has invalid element size %d", name, elemSize)
	}
	if meta.Offset < 0 || meta.Offset%elemBytes != 0 {
		return 0, 0, fmt.Errorf("arrow: buffer %q offset %d is not aligned to element size %d", name, meta.Offset, elemSize)
	}
	if meta.Offset > byteLen {
		return 0, 0, fmt.Errorf("arrow: buffer %q offset %d exceeds length %d", name, meta.Offset, byteLen)
	}

	if len(meta.Shape) == 0 {
		remaining := byteLen - meta.Offset
		if remaining%elemBytes != 0 {
			return 0, 0, fmt.Errorf("arrow: buffer %q remaining length %d is not aligned to element size %d", name, remaining, elemSize)
		}
		if len(meta.Strides) != 0 {
			return 0, 0, fmt.Errorf("arrow: buffer %q has strides without shape", name)
		}
		return remaining / elemBytes, meta.Offset, nil
	}

	length, ok := cArrowShapeProduct(meta.Shape)
	if !ok {
		return 0, 0, fmt.Errorf("arrow: buffer %q has invalid shape %v", name, meta.Shape)
	}
	required := length * elemBytes
	if length != 0 && required/length != elemBytes {
		return 0, 0, fmt.Errorf("arrow: buffer %q shape %v overflows byte length", name, meta.Shape)
	}
	if meta.Offset > byteLen || required > byteLen-meta.Offset {
		return 0, 0, fmt.Errorf("arrow: buffer %q shape %v offset %d describes %d bytes but buffer has %d", name, meta.Shape, meta.Offset, required, byteLen)
	}
	if len(meta.Strides) > 0 && !cArrowStridesAreContiguous(meta.Shape, meta.Strides, elemSize) {
		return 0, 0, fmt.Errorf("arrow: buffer %q has non-contiguous strides %v for shape %v; Arrow C Data primitive export requires contiguous layout", name, meta.Strides, meta.Shape)
	}
	return length, meta.Offset, nil
}

func cArrowShapeProduct(shape []int64) (int64, bool) {
	product := int64(1)
	for _, dim := range shape {
		if dim < 0 {
			return 0, false
		}
		if dim == 0 {
			return 0, true
		}
		if product > math.MaxInt64/dim {
			return 0, false
		}
		product *= dim
	}
	return product, true
}

func cArrowStridesAreContiguous(shape []int64, strides []int64, elemSize int) bool {
	if len(strides) != len(shape) || elemSize <= 0 {
		return false
	}
	for _, dim := range shape {
		if dim < 0 {
			return false
		}
		if dim == 0 {
			return true
		}
	}
	stride := int64(elemSize)
	for i := len(shape) - 1; i >= 0; i-- {
		if strides[i] != stride {
			return false
		}
		if i > 0 && shape[i] > math.MaxInt64/stride {
			return false
		}
		stride *= shape[i]
	}
	return true
}

// ImportCArrowArray imports a primitive Arrow C Data array into the shared
// store. It accepts flat primitive arrays with optional validity bitmaps;
// richer nested layouts can be added without changing user-facing .poly code.
func (s *SharedStore) ImportCArrowArray(name string, schemaPtr, arrayPtr unsafe.Pointer) error {
	if name == "" {
		return fmt.Errorf("arrow: buffer name is required")
	}
	if schemaPtr == nil || arrayPtr == nil {
		return fmt.Errorf("arrow: nil Arrow C Data descriptor")
	}
	schema := (*C.ArrowSchema)(schemaPtr)
	array := (*C.ArrowArray)(arrayPtr)
	releaseOriginal := true
	defer func() {
		if releaseOriginal {
			C.omni_arrow_release_array_if_live(array)
			C.omni_arrow_release_schema_if_live(schema)
		}
	}()

	if schema.format == nil {
		return fmt.Errorf("arrow: Arrow schema format is required")
	}
	format := C.GoString(schema.format)
	elemSize := arrowElementSize(format)
	if elemSize <= 0 {
		return fmt.Errorf("arrow: unsupported Arrow format %q", format)
	}
	if schema.n_children != 0 || schema.dictionary != nil || array.n_children != 0 || array.dictionary != nil {
		return fmt.Errorf("arrow: only flat primitive Arrow arrays are supported")
	}
	if array.length < 0 || array.offset < 0 {
		return fmt.Errorf("arrow: invalid Arrow array length/offset")
	}
	if array.n_buffers < 2 {
		return fmt.Errorf("arrow: primitive Arrow array requires validity and value buffers")
	}
	if array.null_count < 0 {
		return fmt.Errorf("arrow: invalid Arrow null count")
	}
	validity := C.omni_arrow_array_buffer(array, 0)
	if array.null_count > 0 && validity == nil {
		return fmt.Errorf("arrow: nullable Arrow array has no validity bitmap")
	}
	values := C.omni_arrow_array_buffer(array, 1)
	if values == nil && array.length > 0 {
		return fmt.Errorf("arrow: primitive Arrow array has no value buffer")
	}

	if int64(array.length) > math.MaxInt64/int64(elemSize) {
		return fmt.Errorf("arrow: Arrow array length %d overflows byte length for element size %d", int64(array.length), elemSize)
	}
	byteLen := int64(array.length) * int64(elemSize)
	offsetBytes := int64(array.offset) * int64(elemSize)
	validityBytes := arrowValidityByteLen(int64(array.offset), int64(array.length), int64(array.null_count))
	if int64(int(validityBytes)) != validityBytes {
		return fmt.Errorf("arrow: validity bitmap length %d overflows host int", validityBytes)
	}
	meta := BufferMetadata{
		Dtype:             arrowDtypeForFormat(format),
		Format:            format,
		NullCount:         int64(array.null_count),
		ValidityBytes:     validityBytes,
		ValidityBitOffset: int64(array.offset),
	}
	if array.release != nil {
		meta.Ownership = "external"
		schemaCopy := C.omni_arrow_alloc_schema()
		arrayCopy := C.omni_arrow_alloc_array()
		if schemaCopy == nil || arrayCopy == nil {
			if schemaCopy != nil {
				C.free(unsafe.Pointer(schemaCopy))
			}
			if arrayCopy != nil {
				C.free(unsafe.Pointer(arrayCopy))
			}
			return fmt.Errorf("arrow: failed to allocate imported Arrow C Data descriptors")
		}
		C.omni_arrow_copy_schema(schemaCopy, schema)
		C.omni_arrow_copy_array(arrayCopy, array)
		array.release = nil
		array.private_data = nil
		schema.release = nil
		schema.private_data = nil
		releaseOriginal = false

		data := unsafe.Pointer(values)
		if offsetBytes > 0 {
			data = unsafe.Add(data, offsetBytes)
		}
		_, err := s.SetExternalArrowWithMetadata(name, data, byteLen, unsafe.Pointer(validity), validityBytes, meta, func() error {
			C.omni_arrow_release_array_if_live(arrayCopy)
			C.omni_arrow_release_schema_if_live(schemaCopy)
			C.free(unsafe.Pointer(arrayCopy))
			C.free(unsafe.Pointer(schemaCopy))
			return nil
		})
		if err != nil {
			return err
		}
		return nil
	}

	data := make([]byte, byteLen)
	if byteLen > 0 {
		copy(data, unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(values), offsetBytes)), byteLen))
	}
	var validityData []byte
	if validityBytes > 0 {
		validityData = make([]byte, validityBytes)
		copy(validityData, unsafe.Slice((*byte)(unsafe.Pointer(validity)), int(validityBytes)))
	}
	meta.Ownership = "omnivm"
	_, err := s.SetWithValidityMetadata(name, data, validityData, meta)
	if err != nil {
		return err
	}
	s.recordCopy(byteLen + validityBytes)
	return nil
}

func arrowValidityByteLen(offset, length, nullCount int64) int64 {
	if nullCount <= 0 || length <= 0 {
		return 0
	}
	bits := offset + length
	if bits <= 0 {
		return 0
	}
	return (bits + 7) / 8
}

// Release releases both C Data descriptors and the underlying buffer borrow.
func (a *ArrowCData) Release() {
	if a == nil {
		return
	}
	a.once.Do(func() {
		if a.Schema != nil {
			C.omni_arrow_release_schema_if_live(a.Schema)
			C.free(unsafe.Pointer(a.Schema))
			a.Schema = nil
		}
		if a.Array != nil {
			C.omni_arrow_release_array_if_live(a.Array)
			C.free(unsafe.Pointer(a.Array))
			a.Array = nil
		}
	})
}

// DetachTo copies the C Data descriptors into caller-owned Arrow C Data
// structs. The caller becomes responsible for invoking the embedded release
// callbacks. The temporary descriptor shells allocated by BorrowCArrowArray are
// freed without releasing the transferred private state.
func (a *ArrowCData) DetachTo(schemaOut, arrayOut unsafe.Pointer) error {
	if a == nil || a.Schema == nil || a.Array == nil {
		return fmt.Errorf("arrow: no Arrow C Data descriptors to detach")
	}
	if schemaOut == nil || arrayOut == nil {
		return fmt.Errorf("arrow: nil Arrow C Data output descriptor")
	}
	C.omni_arrow_copy_schema((*C.ArrowSchema)(schemaOut), a.Schema)
	C.omni_arrow_copy_array((*C.ArrowArray)(arrayOut), a.Array)
	C.free(unsafe.Pointer(a.Schema))
	C.free(unsafe.Pointer(a.Array))
	a.Schema = nil
	a.Array = nil
	return nil
}

// Format returns the Arrow C Data format string.
func (a *ArrowCData) Format() string {
	if a == nil || a.Schema == nil || a.Schema.format == nil {
		return ""
	}
	return C.GoString(a.Schema.format)
}

// Length returns the logical Arrow array length.
func (a *ArrowCData) Length() int64 {
	if a == nil || a.Array == nil {
		return 0
	}
	return int64(a.Array.length)
}

// BufferPointer returns an Arrow buffer pointer by index.
func (a *ArrowCData) BufferPointer(i int) unsafe.Pointer {
	if a == nil || a.Array == nil {
		return nil
	}
	return unsafe.Pointer(C.omni_arrow_array_buffer(a.Array, C.int64_t(i)))
}

func (a *ArrowCData) setBufferPointer(i int, ptr unsafe.Pointer) {
	if a == nil || a.Array == nil {
		return
	}
	C.omni_arrow_array_set_buffer(a.Array, C.int64_t(i), ptr)
}

func arrowPrimitiveFormat(dtype int32, explicit string) (string, int, error) {
	if explicit != "" {
		if size := arrowElementSize(explicit); size > 0 {
			return explicit, size, nil
		}
		return "", 0, fmt.Errorf("arrow: unsupported Arrow format %q", explicit)
	}
	switch dtype {
	case DtypeBytes:
		return "C", 1, nil
	case DtypeI8:
		return "c", 1, nil
	case DtypeU8:
		return "C", 1, nil
	case DtypeI16:
		return "s", 2, nil
	case DtypeU16:
		return "S", 2, nil
	case DtypeI32:
		return "i", 4, nil
	case DtypeU32:
		return "I", 4, nil
	case DtypeI64:
		return "l", 8, nil
	case DtypeU64:
		return "L", 8, nil
	case DtypeF32:
		return "f", 4, nil
	case DtypeF64:
		return "g", 8, nil
	default:
		return "", 0, fmt.Errorf("arrow: dtype %d is not a contiguous primitive Arrow buffer", dtype)
	}
}

func arrowElementSize(format string) int {
	switch format {
	case "c", "C":
		return 1
	case "s", "S":
		return 2
	case "i", "I", "f":
		return 4
	case "l", "L", "g":
		return 8
	default:
		return 0
	}
}

func arrowDtypeForFormat(format string) int32 {
	switch format {
	case "c":
		return DtypeI8
	case "C":
		return DtypeU8
	case "s":
		return DtypeI16
	case "S":
		return DtypeU16
	case "i":
		return DtypeI32
	case "I":
		return DtypeU32
	case "l":
		return DtypeI64
	case "L":
		return DtypeU64
	case "f":
		return DtypeF32
	case "g":
		return DtypeF64
	default:
		return DtypeBytes
	}
}
