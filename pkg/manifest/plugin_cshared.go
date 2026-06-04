package manifest

/*
#include <stdlib.h>
#include <dlfcn.h>

typedef char* (*omnivm_cshared_go_func)(char*);

static void* omnivm_manifest_dlopen(const char* path) {
	return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}

static void* omnivm_manifest_dlsym(void* handle, const char* name) {
	return dlsym(handle, name);
}

static const char* omnivm_manifest_dlerror(void) {
	return dlerror();
}

static char* omnivm_manifest_call_cshared_go(void* fn, const char* args_json) {
	return ((omnivm_cshared_go_func)fn)((char*)args_json);
}
*/
import "C"

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"sync"
	"unsafe"

	"github.com/omnivm/omnivm/pkg/arrow"
)

type cSharedPluginHandle uintptr

type cSharedPluginEnvelope struct {
	OK          bool        `json:"ok"`
	Boundary    string      `json:"boundary,omitempty"`
	Dtype       string      `json:"dtype,omitempty"`
	Format      string      `json:"format,omitempty"`
	MemorySpace string      `json:"memory_space,omitempty"`
	Ownership   string      `json:"ownership,omitempty"`
	ReadOnly    bool        `json:"read_only,omitempty"`
	HandleID    string      `json:"handle_id,omitempty"`
	Kind        string      `json:"kind,omitempty"`
	Pointer     string      `json:"pointer,omitempty"`
	BufferID    string      `json:"buffer_id,omitempty"`
	BytesLen    int64       `json:"bytes_len,omitempty"`
	Elements    int64       `json:"elements,omitempty"`
	Shape       []int64     `json:"shape,omitempty"`
	Strides     []int64     `json:"strides,omitempty"`
	Found       bool        `json:"found,omitempty"`
	Value       interface{} `json:"value,omitempty"`
	Error       string      `json:"error,omitempty"`
}

type cSharedOwnedBuffer struct {
	ptr         unsafe.Pointer
	bytesLen    int64
	elements    int64
	shape       []int64
	strides     []int64
	dtype       int32
	format      string
	memorySpace string
	release     func() error
	once        *sync.Once
}

type cSharedArgLease struct {
	lease *arrow.BorrowedBuffer
}

type cSharedObjectProxy struct {
	handle   cSharedPluginHandle
	objectID string
	kind     string
	state    *cSharedObjectState
}

type cSharedObjectState struct {
	mu       sync.Mutex
	released bool
}

func newCSharedObjectProxy(handle cSharedPluginHandle, objectID, kind string) *cSharedObjectProxy {
	return &cSharedObjectProxy{
		handle:   handle,
		objectID: objectID,
		kind:     kind,
		state:    &cSharedObjectState{},
	}
}

func openCSharedGoPlugin(path string) (cSharedPluginHandle, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.omnivm_manifest_dlopen(cPath)
	if handle == nil {
		return 0, fmt.Errorf("dlopen %s: %s", path, C.GoString(C.omnivm_manifest_dlerror()))
	}
	return cSharedPluginHandle(uintptr(handle)), nil
}

func callCSharedGoPlugin(handle cSharedPluginHandle, symbol string, args []interface{}) (interface{}, error) {
	cSymbol := C.CString(symbol)
	defer C.free(unsafe.Pointer(cSymbol))

	fn := C.omnivm_manifest_dlsym(unsafe.Pointer(uintptr(handle)), cSymbol)
	if fn == nil {
		return nil, fmt.Errorf("%s: %s", symbol, C.GoString(C.omnivm_manifest_dlerror()))
	}

	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshal Go c-shared args: %w", err)
	}
	cArgs := C.CString(string(argsJSON))
	defer C.free(unsafe.Pointer(cArgs))

	cResult := C.omnivm_manifest_call_cshared_go(fn, cArgs)
	if cResult == nil {
		return nil, nil
	}
	defer C.free(unsafe.Pointer(cResult))

	var env cSharedPluginEnvelope
	if err := json.Unmarshal([]byte(C.GoString(cResult)), &env); err != nil {
		return nil, fmt.Errorf("decode Go c-shared result: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("%s", env.Error)
	}
	return decodeCSharedEnvelopeValue(handle, env)
}

func decodeCSharedEnvelopeValue(handle cSharedPluginHandle, env cSharedPluginEnvelope) (interface{}, error) {
	if env.Boundary == "owned_buffer" {
		return decodeCSharedOwnedBuffer(handle, env)
	}
	if env.Boundary == "owned_handle" {
		if env.HandleID == "" {
			return nil, fmt.Errorf("decode Go c-shared object handle: missing handle_id")
		}
		return newCSharedObjectProxy(handle, env.HandleID, env.Kind), nil
	}
	if env.Boundary == "typed_slice" {
		return decodeCSharedTypedSlice(env.Dtype, env.Value)
	}
	if ref, ok := decodeCSharedRuntimeRefValue(env.Value); ok {
		return ref, nil
	}
	return env.Value, nil
}

func (p *cSharedObjectProxy) Kind() string {
	if p == nil || p.kind == "" {
		return "object"
	}
	return p.kind
}

func (p *cSharedObjectProxy) Get(key string) (interface{}, bool, error) {
	env, err := p.call("get", map[string]interface{}{"key": key})
	if err != nil || !env.Found {
		return nil, false, err
	}
	value, err := decodeCSharedEnvelopeValue(p.handle, env)
	return value, err == nil, err
}

func (p *cSharedObjectProxy) Callable(key string) (bool, error) {
	env, err := p.call("callable", map[string]interface{}{"key": key})
	if err != nil {
		return false, err
	}
	callable, _ := env.Value.(bool)
	return env.Found && callable, nil
}

func (p *cSharedObjectProxy) Index(key interface{}) (interface{}, bool, error) {
	env, err := p.call("index", map[string]interface{}{"value": encodeCSharedHandlePayloadValue(key)})
	if err != nil || !env.Found {
		return nil, false, err
	}
	value, err := decodeCSharedEnvelopeValue(p.handle, env)
	return value, err == nil, err
}

func (p *cSharedObjectProxy) Set(key string, value interface{}) (bool, error) {
	env, err := p.call("set", map[string]interface{}{"key": key, "value": encodeCSharedHandlePayloadValue(value)})
	if err != nil {
		return false, err
	}
	ok, _ := env.Value.(bool)
	return env.Found && ok, nil
}

func (p *cSharedObjectProxy) Len() (int, bool, error) {
	env, err := p.call("len", nil)
	if err != nil || !env.Found {
		return 0, false, err
	}
	switch v := env.Value.(type) {
	case float64:
		return int(v), true, nil
	case int:
		return v, true, nil
	default:
		return 0, false, fmt.Errorf("Go c-shared object len returned %T", env.Value)
	}
}

func (p *cSharedObjectProxy) Iter(mode string) ([]interface{}, bool, error) {
	env, err := p.call("iter", map[string]interface{}{"mode": mode})
	if err != nil || !env.Found {
		return nil, false, err
	}
	raw, ok := env.Value.([]interface{})
	if !ok {
		return nil, false, fmt.Errorf("Go c-shared object iter returned %T", env.Value)
	}
	out := make([]interface{}, 0, len(raw))
	for _, item := range raw {
		value, err := p.decodeInlineValue(item)
		if err != nil {
			return nil, false, err
		}
		out = append(out, value)
	}
	return out, true, nil
}

func (p *cSharedObjectProxy) Contains(key interface{}) (bool, bool, error) {
	env, err := p.call("contains", map[string]interface{}{"value": encodeCSharedHandlePayloadValue(key)})
	if err != nil || !env.Found {
		return false, false, err
	}
	found, _ := env.Value.(bool)
	return true, found, nil
}

func (p *cSharedObjectProxy) Call(key string, args []interface{}) (interface{}, bool, error) {
	env, err := p.call("call", map[string]interface{}{"key": key, "args": encodeCSharedHandlePayloadValue(args)})
	if err != nil || !env.Found {
		return nil, false, err
	}
	value, err := decodeCSharedEnvelopeValue(p.handle, env)
	return value, err == nil, err
}

func (p *cSharedObjectProxy) Read(dst []byte) (int, error) {
	if p == nil || p.Kind() != "reader" {
		return 0, fmt.Errorf("Go c-shared object is not readable")
	}
	if len(dst) == 0 {
		return 0, nil
	}
	env, err := p.call("read", map[string]interface{}{"size": len(dst)})
	if err != nil {
		return 0, err
	}
	if !env.Found {
		return 0, fmt.Errorf("Go c-shared object is not readable")
	}
	payload, ok := env.Value.(map[string]interface{})
	if !ok {
		return 0, fmt.Errorf("Go c-shared reader returned %T", env.Value)
	}
	done, _ := payload["done"].(bool)
	if done {
		return 0, io.EOF
	}
	chunk, err := cSharedByteChunk(payload["chunk"])
	if err != nil {
		return 0, err
	}
	if len(chunk) == 0 {
		return 0, nil
	}
	return copy(dst, chunk), nil
}

func (p *cSharedObjectProxy) Close() error {
	if p == nil || p.Kind() != "reader" {
		return nil
	}
	if p.isReleased() {
		return nil
	}
	env, err := p.call("close", nil)
	if err != nil {
		return err
	}
	if !env.Found {
		return nil
	}
	return nil
}

func (p *cSharedObjectProxy) Release() error {
	if p == nil || p.objectID == "" {
		return nil
	}
	if p.state == nil {
		p.state = &cSharedObjectState{}
	}
	p.state.mu.Lock()
	if p.state.released {
		p.state.mu.Unlock()
		return nil
	}
	p.state.released = true
	p.state.mu.Unlock()
	return releaseCSharedGoPluginObject(p.handle, p.objectID)
}

func (p *cSharedObjectProxy) isReleased() bool {
	if p == nil || p.state == nil {
		return false
	}
	p.state.mu.Lock()
	defer p.state.mu.Unlock()
	return p.state.released
}

func (p *cSharedObjectProxy) call(op string, payload map[string]interface{}) (cSharedPluginEnvelope, error) {
	if p == nil || p.objectID == "" {
		return cSharedPluginEnvelope{}, fmt.Errorf("Go c-shared object proxy is nil")
	}
	if payload == nil {
		payload = map[string]interface{}{}
	}
	payload["op"] = op
	payload["handle_id"] = p.objectID
	return callCSharedGoHandleOp(p.handle, payload)
}

func (p *cSharedObjectProxy) decodeInlineValue(value interface{}) (interface{}, error) {
	if pair, ok := value.([]interface{}); ok {
		out := make([]interface{}, 0, len(pair))
		for _, item := range pair {
			next, err := p.decodeInlineValue(item)
			if err != nil {
				return nil, err
			}
			out = append(out, next)
		}
		return out, nil
	}
	descriptor, ok := value.(map[string]interface{})
	if !ok || descriptor["__omnivm_cshared_boundary__"] != true {
		if ref, ok := decodeCSharedRuntimeRefValue(value); ok {
			return ref, nil
		}
		return value, nil
	}
	env := cSharedPluginEnvelope{OK: true}
	env.Boundary, _ = descriptor["boundary"].(string)
	env.Dtype, _ = descriptor["dtype"].(string)
	env.Format, _ = descriptor["format"].(string)
	env.MemorySpace, _ = descriptor["memory_space"].(string)
	env.Ownership, _ = descriptor["ownership"].(string)
	env.ReadOnly, _ = descriptor["read_only"].(bool)
	env.HandleID, _ = descriptor["handle_id"].(string)
	env.Kind, _ = descriptor["kind"].(string)
	env.Pointer, _ = descriptor["pointer"].(string)
	env.BufferID, _ = descriptor["buffer_id"].(string)
	env.BytesLen, _ = cSharedInt64OrZero(descriptor["bytes_len"])
	env.Elements, _ = cSharedInt64OrZero(descriptor["elements"])
	env.Shape = cSharedInt64SliceOrNil(descriptor["shape"])
	env.Strides = cSharedInt64SliceOrNil(descriptor["strides"])
	return decodeCSharedEnvelopeValue(p.handle, env)
}

func decodeCSharedRuntimeRefValue(value interface{}) (interface{}, bool) {
	descriptor, ok := value.(map[string]interface{})
	if !ok || descriptor["__omnivm_runtime_ref__"] != true {
		return nil, false
	}
	return decodeRuntimeRefArg(descriptor), true
}

func cSharedByteChunk(value interface{}) ([]byte, error) {
	switch v := value.(type) {
	case string:
		out, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("decode Go c-shared reader chunk: %w", err)
		}
		return out, nil
	case []interface{}:
		out := make([]byte, len(v))
		for i, item := range v {
			n, err := cSharedInt64(item)
			if err != nil || n < 0 || n > 255 {
				return nil, fmt.Errorf("decode Go c-shared reader chunk byte %d: %v", i, item)
			}
			out[i] = byte(n)
		}
		return out, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("Go c-shared reader chunk returned %T", value)
	}
}

func encodeCSharedHandlePayloadValue(value interface{}) interface{} {
	switch v := value.(type) {
	case RuntimeRef:
		out := map[string]interface{}{
			"__omnivm_runtime_ref__": true,
			"runtime":                v.Runtime,
			"var":                    v.VarName,
		}
		if v.CallableKnown {
			out["callable"] = v.Callable
		}
		if v.CallableShape != nil {
			out["callable_shape"] = v.CallableShape
		}
		return out
	case *RuntimeRef:
		if v == nil {
			return nil
		}
		return encodeCSharedHandlePayloadValue(*v)
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = encodeCSharedHandlePayloadValue(item)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, item := range v {
			out[key] = encodeCSharedHandlePayloadValue(item)
		}
		return out
	default:
		return value
	}
}

func cSharedInt64OrZero(value interface{}) (int64, bool) {
	if value == nil {
		return 0, false
	}
	n, err := cSharedInt64(value)
	return n, err == nil
}

func (e *Executor) encodeCSharedGoArgs(args []interface{}) ([]interface{}, []*cSharedArgLease, error) {
	encoded := make([]interface{}, 0, len(args))
	leases := make([]*cSharedArgLease, 0)
	for _, arg := range args {
		next, lease, err := e.encodeCSharedGoArg(arg)
		if err != nil {
			for _, held := range leases {
				held.release()
			}
			return nil, nil, err
		}
		encoded = append(encoded, next)
		if lease != nil {
			leases = append(leases, lease)
		}
	}
	return encoded, leases, nil
}

func (e *Executor) encodeCSharedGoArg(arg interface{}) (interface{}, *cSharedArgLease, error) {
	switch v := arg.(type) {
	case *TableRef:
		return e.encodeCSharedGoTableArg(v)
	case TableRef:
		return e.encodeCSharedGoTableArg(&v)
	case *GoHandleProxy:
		if v == nil {
			return nil, nil, nil
		}
		if v.Kind() != "table" || v.id == 0 {
			return v.payload, nil, nil
		}
		if v.id == 0 {
			return arg, nil, nil
		}
		ref, ok := e.tables[v.id]
		if !ok {
			entry, live := e.ensureHandleTable().Get(v.id)
			if !live {
				return nil, nil, fmt.Errorf("Go c-shared arg: table handle %d is not live", v.id)
			}
			var refOK bool
			ref, refOK = entry.Value.(*TableRef)
			if !refOK {
				return nil, nil, fmt.Errorf("Go c-shared arg: handle %d is %T, want table", v.id, entry.Value)
			}
		}
		return e.encodeCSharedGoTableArg(ref)
	default:
		return arg, nil, nil
	}
}

func (e *Executor) encodeCSharedGoTableArg(ref *TableRef) (interface{}, *cSharedArgLease, error) {
	if ref == nil {
		return nil, nil, nil
	}
	lease, _, found, err := borrowTableBufferBytes(ref)
	if err != nil || !found {
		return nil, nil, err
	}
	dtype, ok := cSharedDtypeString(lease.Dtype)
	if !ok {
		lease.Release()
		return nil, nil, fmt.Errorf("Go c-shared arg: unsupported Arrow dtype %d", lease.Dtype)
	}
	elemSize, ok := tableBufferElementSize(lease.Dtype)
	if !ok || elemSize <= 0 || lease.Len%int64(elemSize) != 0 {
		lease.Release()
		return nil, nil, fmt.Errorf("Go c-shared arg: buffer length %d is not aligned to dtype %d", lease.Len, lease.Dtype)
	}
	elements := lease.Len / int64(elemSize)
	if len(lease.Metadata.Shape) > 0 {
		shapeElements, ok := cSharedShapeProduct(lease.Metadata.Shape)
		if !ok {
			lease.Release()
			return nil, nil, fmt.Errorf("Go c-shared arg: invalid Arrow shape %v", lease.Metadata.Shape)
		}
		elements = shapeElements
	}
	payload := map[string]interface{}{
		"__omnivm_cshared_buffer__": true,
		"boundary":                  "borrowed_buffer",
		"dtype":                     dtype,
		"format":                    lease.Metadata.Format,
		"memory_space":              nonEmpty(lease.Metadata.MemorySpace, "host"),
		"ownership":                 nonEmpty(lease.Metadata.Ownership, "omnivm"),
		"read_only":                 lease.Metadata.ReadOnly,
		"bytes_len":                 lease.Len,
		"elements":                  elements,
	}
	if len(lease.Metadata.Shape) > 0 {
		payload["shape"] = append([]int64(nil), lease.Metadata.Shape...)
	} else {
		payload["shape"] = []int64{elements}
	}
	if len(lease.Metadata.Strides) > 0 {
		payload["strides"] = append([]int64(nil), lease.Metadata.Strides...)
	}
	if lease.Metadata.Offset != 0 {
		payload["offset"] = lease.Metadata.Offset
	}
	if lease.Data != nil && lease.Len > 0 {
		payload["pointer"] = strconv.FormatUint(uint64(uintptr(lease.Data)), 10)
	}
	return payload, &cSharedArgLease{lease: lease}, nil
}

func (l *cSharedArgLease) release() {
	if l != nil && l.lease != nil {
		l.lease.Release()
	}
}

func cSharedDtypeString(dtype int32) (string, bool) {
	switch dtype {
	case arrow.DtypeBytes, arrow.DtypeUTF8:
		return "u8", true
	case arrow.DtypeI8:
		return "i8", true
	case arrow.DtypeU8:
		return "u8", true
	case arrow.DtypeI16:
		return "i16", true
	case arrow.DtypeU16:
		return "u16", true
	case arrow.DtypeI32:
		return "i32", true
	case arrow.DtypeU32:
		return "u32", true
	case arrow.DtypeI64:
		return "i64", true
	case arrow.DtypeU64:
		return "u64", true
	case arrow.DtypeF32:
		return "f32", true
	case arrow.DtypeF64:
		return "f64", true
	default:
		return "", false
	}
}

func decodeCSharedOwnedBuffer(handle cSharedPluginHandle, env cSharedPluginEnvelope) (*cSharedOwnedBuffer, error) {
	dtype, format, elemSize, ok := cSharedArrowDtype(env.Dtype)
	if !ok {
		return nil, fmt.Errorf("decode Go c-shared owned buffer: unknown dtype %q", env.Dtype)
	}
	if env.Format != "" && env.Format != format {
		return nil, fmt.Errorf("decode Go c-shared owned buffer: dtype %q has format %q, got %q", env.Dtype, format, env.Format)
	}
	memorySpace := nonEmpty(env.MemorySpace, "host")
	if memorySpace != "host" {
		return nil, fmt.Errorf("decode Go c-shared owned buffer: memory_space %q is not host-accessible", memorySpace)
	}
	if env.BytesLen < 0 || env.Elements < 0 {
		return nil, fmt.Errorf("decode Go c-shared owned buffer: negative length bytes=%d elements=%d", env.BytesLen, env.Elements)
	}
	if env.BytesLen != env.Elements*elemSize {
		return nil, fmt.Errorf("decode Go c-shared owned buffer: byte length %d does not match %d %s elements", env.BytesLen, env.Elements, env.Dtype)
	}
	shape := append([]int64(nil), env.Shape...)
	if len(shape) == 0 {
		shape = []int64{env.Elements}
	}
	elements, ok := cSharedShapeProduct(shape)
	if !ok || elements != env.Elements {
		return nil, fmt.Errorf("decode Go c-shared owned buffer: shape %v does not match %d elements", shape, env.Elements)
	}
	strides := append([]int64(nil), env.Strides...)
	if len(strides) > 0 && len(strides) != len(shape) {
		return nil, fmt.Errorf("decode Go c-shared owned buffer: shape %v has mismatched strides %v", shape, strides)
	}
	var ptr unsafe.Pointer
	if env.BytesLen > 0 {
		if env.Pointer == "" {
			return nil, fmt.Errorf("decode Go c-shared owned buffer: missing pointer")
		}
		addr, err := strconv.ParseUint(env.Pointer, 10, 64)
		if err != nil || addr == 0 {
			return nil, fmt.Errorf("decode Go c-shared owned buffer: invalid pointer %q", env.Pointer)
		}
		ptr = unsafe.Pointer(uintptr(addr))
	}
	buf := &cSharedOwnedBuffer{
		ptr:         ptr,
		bytesLen:    env.BytesLen,
		elements:    env.Elements,
		shape:       shape,
		strides:     strides,
		dtype:       dtype,
		format:      format,
		memorySpace: memorySpace,
		once:        &sync.Once{},
	}
	if env.BufferID != "" {
		bufferID := env.BufferID
		buf.release = func() error {
			var err error
			buf.once.Do(func() {
				err = releaseCSharedGoPluginBuffer(handle, bufferID)
			})
			return err
		}
	}
	return buf, nil
}

func cSharedInt64SliceOrNil(value interface{}) []int64 {
	raw, ok := value.([]interface{})
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		n, err := cSharedInt64(item)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func cSharedShapeProduct(shape []int64) (int64, bool) {
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

func releaseCSharedGoPluginBuffer(handle cSharedPluginHandle, bufferID string) error {
	cSymbol := C.CString("OmniVMReleaseBuffer")
	defer C.free(unsafe.Pointer(cSymbol))

	fn := C.omnivm_manifest_dlsym(unsafe.Pointer(uintptr(handle)), cSymbol)
	if fn == nil {
		return fmt.Errorf("OmniVMReleaseBuffer: %s", C.GoString(C.omnivm_manifest_dlerror()))
	}

	cID := C.CString(bufferID)
	defer C.free(unsafe.Pointer(cID))

	cResult := C.omnivm_manifest_call_cshared_go(fn, cID)
	if cResult == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(cResult))

	var env cSharedPluginEnvelope
	if err := json.Unmarshal([]byte(C.GoString(cResult)), &env); err != nil {
		return fmt.Errorf("decode Go c-shared buffer release: %w", err)
	}
	if !env.OK {
		return fmt.Errorf("%s", env.Error)
	}
	return nil
}

func callCSharedGoHandleOp(handle cSharedPluginHandle, payload map[string]interface{}) (cSharedPluginEnvelope, error) {
	cSymbol := C.CString("OmniVMHandleOp")
	defer C.free(unsafe.Pointer(cSymbol))

	fn := C.omnivm_manifest_dlsym(unsafe.Pointer(uintptr(handle)), cSymbol)
	if fn == nil {
		return cSharedPluginEnvelope{}, fmt.Errorf("OmniVMHandleOp: %s", C.GoString(C.omnivm_manifest_dlerror()))
	}

	argsJSON, err := json.Marshal(payload)
	if err != nil {
		return cSharedPluginEnvelope{}, fmt.Errorf("marshal Go c-shared handle op: %w", err)
	}
	cArgs := C.CString(string(argsJSON))
	defer C.free(unsafe.Pointer(cArgs))

	cResult := C.omnivm_manifest_call_cshared_go(fn, cArgs)
	if cResult == nil {
		return cSharedPluginEnvelope{}, nil
	}
	defer C.free(unsafe.Pointer(cResult))

	var env cSharedPluginEnvelope
	if err := json.Unmarshal([]byte(C.GoString(cResult)), &env); err != nil {
		return cSharedPluginEnvelope{}, fmt.Errorf("decode Go c-shared handle op result: %w", err)
	}
	if !env.OK {
		return cSharedPluginEnvelope{}, fmt.Errorf("%s", env.Error)
	}
	return env, nil
}

func releaseCSharedGoPluginObject(handle cSharedPluginHandle, objectID string) error {
	cSymbol := C.CString("OmniVMReleaseObject")
	defer C.free(unsafe.Pointer(cSymbol))

	fn := C.omnivm_manifest_dlsym(unsafe.Pointer(uintptr(handle)), cSymbol)
	if fn == nil {
		return fmt.Errorf("OmniVMReleaseObject: %s", C.GoString(C.omnivm_manifest_dlerror()))
	}

	cID := C.CString(objectID)
	defer C.free(unsafe.Pointer(cID))

	cResult := C.omnivm_manifest_call_cshared_go(fn, cID)
	if cResult == nil {
		return nil
	}
	defer C.free(unsafe.Pointer(cResult))

	var env cSharedPluginEnvelope
	if err := json.Unmarshal([]byte(C.GoString(cResult)), &env); err != nil {
		return fmt.Errorf("decode Go c-shared object release: %w", err)
	}
	if !env.OK {
		return fmt.Errorf("%s", env.Error)
	}
	return nil
}

func cSharedArrowDtype(dtype string) (int32, string, int64, bool) {
	switch dtype {
	case "i8":
		return arrow.DtypeI8, "c", 1, true
	case "u8":
		return arrow.DtypeU8, "C", 1, true
	case "i16":
		return arrow.DtypeI16, "s", 2, true
	case "u16":
		return arrow.DtypeU16, "S", 2, true
	case "i32":
		return arrow.DtypeI32, "i", 4, true
	case "u32":
		return arrow.DtypeU32, "I", 4, true
	case "i64":
		return arrow.DtypeI64, "l", 8, true
	case "u64":
		return arrow.DtypeU64, "L", 8, true
	case "f32":
		return arrow.DtypeF32, "f", 4, true
	case "f64":
		return arrow.DtypeF64, "g", 8, true
	default:
		return 0, "", 0, false
	}
}

func decodeCSharedTypedSlice(dtype string, raw interface{}) (interface{}, error) {
	values, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("decode Go c-shared typed slice: value is %T, want array", raw)
	}
	switch dtype {
	case "i8":
		return decodeCSharedSignedSlice[int8](values, dtype)
	case "u8":
		return decodeCSharedUnsignedSlice[uint8](values, dtype)
	case "i16":
		return decodeCSharedSignedSlice[int16](values, dtype)
	case "u16":
		return decodeCSharedUnsignedSlice[uint16](values, dtype)
	case "i32":
		return decodeCSharedSignedSlice[int32](values, dtype)
	case "u32":
		return decodeCSharedUnsignedSlice[uint32](values, dtype)
	case "i64":
		return decodeCSharedSignedSlice[int64](values, dtype)
	case "u64":
		return decodeCSharedUnsignedSlice[uint64](values, dtype)
	case "f32":
		return decodeCSharedFloatSlice[float32](values, dtype)
	case "f64":
		return decodeCSharedFloatSlice[float64](values, dtype)
	default:
		return nil, fmt.Errorf("decode Go c-shared typed slice: unknown dtype %q", dtype)
	}
}

func decodeCSharedSignedSlice[T ~int8 | ~int16 | ~int32 | ~int64](values []interface{}, dtype string) ([]T, error) {
	out := make([]T, 0, len(values))
	for _, value := range values {
		n, err := cSharedInt64(value)
		if err != nil {
			return nil, fmt.Errorf("%s element: %w", dtype, err)
		}
		out = append(out, T(n))
	}
	return out, nil
}

func decodeCSharedUnsignedSlice[T ~uint8 | ~uint16 | ~uint32 | ~uint64](values []interface{}, dtype string) ([]T, error) {
	out := make([]T, 0, len(values))
	for _, value := range values {
		n, err := cSharedUint64(value)
		if err != nil {
			return nil, fmt.Errorf("%s element: %w", dtype, err)
		}
		out = append(out, T(n))
	}
	return out, nil
}

func decodeCSharedFloatSlice[T ~float32 | ~float64](values []interface{}, dtype string) ([]T, error) {
	out := make([]T, 0, len(values))
	for _, value := range values {
		n, err := cSharedFloat64(value)
		if err != nil {
			return nil, fmt.Errorf("%s element: %w", dtype, err)
		}
		out = append(out, T(n))
	}
	return out, nil
}

func cSharedInt64(value interface{}) (int64, error) {
	switch v := value.(type) {
	case string:
		return strconv.ParseInt(v, 10, 64)
	case float64:
		if math.Trunc(v) != v {
			return 0, fmt.Errorf("%v is not an integer", v)
		}
		return int64(v), nil
	default:
		return 0, fmt.Errorf("cannot decode %T as signed integer", value)
	}
}

func cSharedUint64(value interface{}) (uint64, error) {
	switch v := value.(type) {
	case string:
		return strconv.ParseUint(v, 10, 64)
	case float64:
		if math.Trunc(v) != v || v < 0 {
			return 0, fmt.Errorf("%v is not an unsigned integer", v)
		}
		return uint64(v), nil
	default:
		return 0, fmt.Errorf("cannot decode %T as unsigned integer", value)
	}
}

func cSharedFloat64(value interface{}) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case string:
		return strconv.ParseFloat(v, 64)
	default:
		return 0, fmt.Errorf("cannot decode %T as float", value)
	}
}
