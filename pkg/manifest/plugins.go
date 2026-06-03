package manifest

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"plugin"
	"regexp"
	"runtime"
	"strings"
)

const pluginCacheDir = "/tmp/omnivm-plugins"

// UseGoSourceFallback avoids Go's plugin.Open path for c-shared hosts. A Go
// shared library cannot safely use the normal Go plugin loader; libomnivm sets
// this and compiles manifest Go func_defs as c-shared libraries instead.
var UseGoSourceFallback bool

// loadedPlugins tracks plugins already opened in this process.
// Go's plugin.Open panics/errors if the same .so is opened twice.
var loadedPlugins = map[string]*plugin.Plugin{}

var loadedCSharedPlugins = map[string]cSharedPluginHandle{}

// compileGoPlugin handles func_def ops with bodyRuntime:"go" and a source field.
// It compiles the Go source as a plugin, loads exports, and registers them
// in the executor's goFuncs registry.
func (e *Executor) compileGoPlugin(op *Op) (interface{}, error) {
	if UseGoSourceFallback {
		if e.registerGoChannelWorkerFallback(op) {
			return nil, nil
		}
		if err := e.compileGoCSharedPlugin(op); err != nil {
			return nil, err
		}
		return nil, nil
	}

	hash := sha256Hash(op.Source)
	soPath := filepath.Join(pluginCacheDir, hash+".so")

	// Check if already loaded in this process
	plug, alreadyLoaded := loadedPlugins[soPath]

	if !alreadyLoaded {
		// Check compile cache
		if _, err := os.Stat(soPath); os.IsNotExist(err) {
			if err := compilePlugin(op.Source, soPath); err != nil {
				return nil, fmt.Errorf("go plugin compile: %w", err)
			}
		}

		// Load the plugin
		var err error
		plug, err = plugin.Open(soPath)
		if err != nil {
			return nil, fmt.Errorf("go plugin open: %w", err)
		}
		loadedPlugins[soPath] = plug
	}

	// If the plugin has an Init function and requires dependencies, call it
	if len(op.Requires) > 0 {
		initSym, err := plug.Lookup("Init")
		if err == nil {
			if initFn, ok := initSym.(func(map[string]interface{})); ok {
				deps := make(map[string]interface{})
				for _, req := range op.Requires {
					if fn, ok := e.goFuncs[req]; ok {
						deps[req] = fn
					}
				}
				initFn(deps)
			}
		}
	}

	// Register exported symbols under both their Go name and the manifest func name
	for _, name := range op.Exports {
		sym, err := plug.Lookup(name)
		if err != nil {
			return nil, fmt.Errorf("go plugin: export %q not found: %w", name, err)
		}
		e.goFuncs[name] = sym
	}

	// Also register the manifest function name → first export mapping
	// so HandleCall can find it by the manifest name (e.g. "shard_for" → ShardFor)
	if op.Name != "" && len(op.Exports) > 0 {
		if _, exists := e.goFuncs[op.Name]; !exists {
			e.goFuncs[op.Name] = e.goFuncs[op.Exports[0]]
		}
	}

	// Register stubs in each runtime so guest code can call this function
	if op.Name != "" {
		fd := &FuncDef{Name: op.Name, Params: op.Params}
		if err := e.registerStubs(fd); err != nil {
			return nil, fmt.Errorf("go plugin stubs: %w", err)
		}
	}

	return nil, nil
}

func (e *Executor) compileGoCSharedPlugin(op *Op) error {
	if op.Name == "" && len(op.Exports) == 0 {
		return fmt.Errorf("go c-shared plugin: missing function name/export")
	}
	if op.Source == "" {
		return fmt.Errorf("go c-shared plugin: missing source")
	}
	for _, exportName := range op.Exports {
		if !goIdentifierRE.MatchString(exportName) {
			return fmt.Errorf("go c-shared plugin: invalid export %q", exportName)
		}
	}

	hash := sha256Hash("cshared:" + op.Source + ":" + strings.Join(op.Exports, ","))
	soPath := filepath.Join(pluginCacheDir, hash+".so")

	handle, alreadyLoaded := loadedCSharedPlugins[soPath]
	if !alreadyLoaded {
		if _, err := os.Stat(soPath); os.IsNotExist(err) {
			if err := compileCSharedPlugin(op, soPath); err != nil {
				return fmt.Errorf("go c-shared plugin compile: %w", err)
			}
		}
		var err error
		handle, err = openCSharedGoPlugin(soPath)
		if err != nil {
			return fmt.Errorf("go c-shared plugin open: %w", err)
		}
		loadedCSharedPlugins[soPath] = handle
	}

	for _, exportName := range op.Exports {
		exportName := exportName
		symbol := cSharedWrapperSymbol(exportName)
		fn := func(args []interface{}) interface{} {
			encodedArgs, leases, err := e.encodeCSharedGoArgs(args)
			if err != nil {
				panic(err)
			}
			defer func() {
				for _, lease := range leases {
					lease.release()
				}
			}()
			value, err := callCSharedGoPlugin(handle, symbol, encodedArgs)
			if err != nil {
				panic(err)
			}
			return normalizeArg(value)
		}
		e.goFuncs[exportName] = fn
	}

	if op.Name != "" && len(op.Exports) > 0 {
		if _, exists := e.goFuncs[op.Name]; !exists {
			e.goFuncs[op.Name] = e.goFuncs[op.Exports[0]]
		}
	}

	if op.Name != "" {
		fd := &FuncDef{Name: op.Name, Params: op.Params}
		if err := e.registerStubs(fd); err != nil {
			return fmt.Errorf("go c-shared plugin stubs: %w", err)
		}
	}

	return nil
}

func compileCSharedPlugin(op *Op, outputPath string) error {
	if err := os.MkdirAll(pluginCacheDir, 0o755); err != nil {
		return err
	}

	tmpDir, err := os.MkdirTemp("", "omnivm-cshared-plugin-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	pkgRe := regexp.MustCompile(`(?m)^package\s+\w+`)
	source := pkgRe.ReplaceAllString(op.Source, "package main")

	if err := os.WriteFile(filepath.Join(tmpDir, "plugin.go"), []byte(source), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "omnivm_wrappers.go"), []byte(goCSharedWrapperSource(op.Exports)), 0o644); err != nil {
		return err
	}

	modName := fmt.Sprintf("omnivm-cshared-plugin-%s", filepath.Base(outputPath[:len(outputPath)-3]))
	goVer := strings.TrimPrefix(runtime.Version(), "go")
	if parts := strings.SplitN(goVer, ".", 3); len(parts) >= 2 {
		goVer = parts[0] + "." + parts[1]
	}
	modContent := fmt.Sprintf("module %s\n\ngo %s\n", modName, goVer)
	if err := os.WriteFile(filepath.Join(tmpDir, "go.mod"), []byte(modContent), 0o644); err != nil {
		return err
	}

	goTool, err := goToolPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(goTool, "build", "-buildmode=c-shared", "-o", outputPath, ".")
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %s: %w", string(out), err)
	}

	return nil
}

func goCSharedWrapperSource(exports []string) string {
	var b strings.Builder
	b.WriteString(`package main

/*
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

type __omnivmEnvelope struct {
	OK    bool        ` + "`json:\"ok\"`" + `
	Boundary string   ` + "`json:\"boundary,omitempty\"`" + `
	Dtype string      ` + "`json:\"dtype,omitempty\"`" + `
	Format string     ` + "`json:\"format,omitempty\"`" + `
	HandleID string  ` + "`json:\"handle_id,omitempty\"`" + `
	Kind string      ` + "`json:\"kind,omitempty\"`" + `
	Pointer string    ` + "`json:\"pointer,omitempty\"`" + `
	BufferID string   ` + "`json:\"buffer_id,omitempty\"`" + `
	BytesLen int64    ` + "`json:\"bytes_len,omitempty\"`" + `
	Elements int64    ` + "`json:\"elements,omitempty\"`" + `
	Shape []int64     ` + "`json:\"shape,omitempty\"`" + `
	Strides []int64   ` + "`json:\"strides,omitempty\"`" + `
	Found bool        ` + "`json:\"found,omitempty\"`" + `
	Value interface{} ` + "`json:\"value,omitempty\"`" + `
	Error string      ` + "`json:\"error,omitempty\"`" + `
}

var __omnivmOwnedBuffers sync.Map
var __omnivmOwnedObjects sync.Map
var __omnivmObjectIdentities sync.Map
var __omnivmObjectCounter uint64

func __omnivmCStringEnvelope(value interface{}, err error) *C.char {
	env := __omnivmEnvelope{OK: err == nil}
	if err != nil {
		env.Error = err.Error()
	} else {
		env = __omnivmEncodeReturn(value)
	}
	payload, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		payload, _ = json.Marshal(__omnivmEnvelope{OK: false, Error: marshalErr.Error()})
	}
	return C.CString(string(payload))
}

func __omnivmEncodeReturn(value interface{}) __omnivmEnvelope {
	env := __omnivmEnvelope{OK: true, Value: value}
	dtype, format, ptr, bufferID, bytesLen, elements, shape, strides, ok, err := __omnivmOwnedTypedSequence(value)
	if err != nil {
		return __omnivmEnvelope{OK: false, Error: err.Error()}
	}
	if ok {
		env.Boundary = "owned_buffer"
		env.Dtype = dtype
		env.Format = format
		env.BufferID = bufferID
		env.BytesLen = bytesLen
		env.Elements = elements
		env.Shape = shape
		env.Strides = strides
		env.Value = nil
		if ptr != nil {
			env.Pointer = strconv.FormatUint(uint64(uintptr(ptr)), 10)
		}
	}
	if env.Boundary == "" {
		if handleID, kind, ok := __omnivmOwnedObjectHandle(value); ok {
			env.Boundary = "owned_handle"
			env.HandleID = handleID
			env.Kind = kind
			env.Value = nil
		}
	}
	return env
}

func __omnivmOwnedObjectHandle(value interface{}) (string, string, bool) {
	if !__omnivmShouldProxyObject(value) {
		return "", "", false
	}
	if identity, ok := __omnivmObjectIdentity(value); ok {
		if existing, found := __omnivmObjectIdentities.Load(identity); found {
			return existing.(string), __omnivmObjectKind(value), true
		}
		id := strconv.FormatUint(atomic.AddUint64(&__omnivmObjectCounter, 1), 10)
		__omnivmOwnedObjects.Store(id, value)
		__omnivmObjectIdentities.Store(identity, id)
		return id, __omnivmObjectKind(value), true
	}
	id := strconv.FormatUint(atomic.AddUint64(&__omnivmObjectCounter, 1), 10)
	__omnivmOwnedObjects.Store(id, value)
	return id, __omnivmObjectKind(value), true
}

func __omnivmObjectIdentity(value interface{}) (string, bool) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return "", false
	}
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "", false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Pointer, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if rv.IsNil() {
			return "", false
		}
		ptr := rv.Pointer()
		if ptr == 0 {
			return "", false
		}
		return rv.Type().String() + ":" + strconv.FormatUint(uint64(ptr), 10), true
	default:
		return "", false
	}
}

func __omnivmShouldProxyObject(value interface{}) bool {
	if value == nil {
		return false
	}
	if __omnivmIsBridgeMarker(value) {
		return false
	}
	switch value.(type) {
	case string, bool,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64:
		return false
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false
	}
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Pointer, reflect.Func:
		if (rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Func) && rv.IsNil() {
			return false
		}
		return true
	default:
		return false
	}
}

func __omnivmIsBridgeMarker(value interface{}) bool {
	descriptor, ok := value.(map[string]interface{})
	if !ok {
		return false
	}
	return descriptor["__omnivm_resource__"] == true ||
		descriptor["__omnivm_table__"] == true ||
		descriptor["__omnivm_job__"] == true ||
		descriptor["__omnivm_stream__"] == true ||
		descriptor["__omnivm_channel__"] == true ||
		descriptor["__omnivm_callable__"] == true ||
		descriptor["__omnivm_runtime_ref__"] == true ||
		descriptor["__omnivm_cshared_boundary__"] == true
}

func __omnivmObjectKind(value interface{}) string {
	if __omnivmIsReaderStream(value) {
		return "reader"
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return "object"
	}
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return "object"
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		return "map"
	case reflect.Slice, reflect.Array:
		return "sequence"
	case reflect.Func:
		return "callable"
	default:
		return "object"
	}
}

func __omnivmIsReaderStream(value interface{}) bool {
	if __omnivmIsHTTPMessageShape(value) {
		return false
	}
	_, ok := value.(io.Reader)
	return ok
}

func __omnivmIsHTTPMessageShape(value interface{}) bool {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	rt := rv.Type()
	methodLike := false
	targetLike := false
	if rt.Kind() == reflect.Struct {
		for _, name := range []string{"Method", "RequestMethod"} {
			if _, ok := rt.FieldByName(name); ok {
				methodLike = true
				break
			}
		}
		for _, name := range []string{"Path", "URL", "Url", "URI", "Headers", "Header", "Env", "PathInfo", "RequestURI"} {
			if _, ok := rt.FieldByName(name); ok {
				targetLike = true
				break
			}
		}
	}
	for i := 0; i < rt.NumMethod(); i++ {
		method := rt.Method(i)
		if method.Type.NumIn() != 1 || method.Type.NumOut() == 0 {
			continue
		}
		switch method.Name {
		case "Method", "GetMethod", "RequestMethod", "GetRequestMethod":
			methodLike = true
		case "Path", "GetPath", "URL", "Url", "GetURL", "GetUrl", "URI", "GetURI", "Headers", "GetHeaders", "Header", "GetHeader", "Env", "GetEnv", "PathInfo", "GetPathInfo", "RequestURI", "GetRequestURI":
			targetLike = true
		}
	}
	return methodLike && targetLike
}

func __omnivmOwnedTypedSequence(value interface{}) (string, string, unsafe.Pointer, string, int64, int64, []int64, []int64, bool, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return "", "", nil, "", 0, 0, nil, nil, false, nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "", "", nil, "", 0, 0, nil, nil, false, nil
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice {
		return "", "", nil, "", 0, 0, nil, nil, false, nil
	}
	shape, elem, ok := __omnivmTypedSequenceShape(rv)
	if !ok {
		return "", "", nil, "", 0, 0, nil, nil, false, nil
	}
	dtype, format, elemSize, ok := __omnivmTypedSequenceFormat(elem)
	if !ok {
		return "", "", nil, "", 0, 0, nil, nil, false, nil
	}
	elements, ok := __omnivmShapeProduct(shape)
	if !ok {
		return "", "", nil, "", 0, 0, nil, nil, false, fmt.Errorf("invalid typed sequence shape %v", shape)
	}
	bytesLen := elements * elemSize
	strides := __omnivmContiguousStrides(shape, elemSize)
	if bytesLen == 0 {
		return dtype, format, nil, "", 0, elements, shape, strides, true, nil
	}
	mem := C.malloc(C.size_t(bytesLen))
	if mem == nil {
		return "", "", nil, "", 0, 0, nil, nil, false, fmt.Errorf("allocate %d-byte owned buffer", bytesLen)
	}
	buf := unsafe.Slice((*byte)(mem), bytesLen)
	offset := 0
	__omnivmCopyTypedSequenceBytes(buf, rv, elem.Kind(), int(elemSize), &offset)
	bufferID := strconv.FormatUint(uint64(uintptr(mem)), 10)
	__omnivmOwnedBuffers.Store(bufferID, unsafe.Pointer(mem))
	return dtype, format, unsafe.Pointer(mem), bufferID, bytesLen, elements, shape, strides, true, nil
}

func __omnivmTypedSequenceShape(rv reflect.Value) ([]int64, reflect.Type, bool) {
	if rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice {
		return nil, nil, false
	}
	elem, ok := __omnivmTypedSequenceScalarType(rv.Type())
	if !ok {
		return nil, nil, false
	}
	shape, ok := __omnivmTypedSequenceShapeDims(rv)
	if !ok || len(shape) == 0 {
		return nil, nil, false
	}
	return shape, elem, true
}

func __omnivmTypedSequenceScalarType(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Array || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() == reflect.Array || t.Kind() == reflect.Slice {
		return nil, false
	}
	return t, true
}

func __omnivmTypedSequenceShapeDims(rv reflect.Value) ([]int64, bool) {
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice {
		return nil, true
	}
	shape := []int64{int64(rv.Len())}
	if rv.Len() == 0 {
		inner, ok := __omnivmTypedSequenceTypeFixedShape(rv.Type().Elem())
		if !ok {
			return shape, true
		}
		return append(shape, inner...), true
	}
	var expected []int64
	for i := 0; i < rv.Len(); i++ {
		inner, ok := __omnivmTypedSequenceShapeDims(rv.Index(i))
		if !ok {
			return nil, false
		}
		if i == 0 {
			expected = append([]int64(nil), inner...)
		} else if !__omnivmInt64SlicesEqual(expected, inner) {
			return nil, false
		}
	}
	return append(shape, expected...), true
}

func __omnivmTypedSequenceTypeFixedShape(t reflect.Type) ([]int64, bool) {
	switch t.Kind() {
	case reflect.Array:
		inner, ok := __omnivmTypedSequenceTypeFixedShape(t.Elem())
		if !ok {
			return nil, false
		}
		return append([]int64{int64(t.Len())}, inner...), true
	case reflect.Slice:
		return nil, false
	default:
		return nil, true
	}
}

func __omnivmInt64SlicesEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func __omnivmShapeProduct(shape []int64) (int64, bool) {
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

func __omnivmContiguousStrides(shape []int64, elemSize int64) []int64 {
	if len(shape) == 0 {
		return nil
	}
	strides := make([]int64, len(shape))
	stride := elemSize
	for i := len(shape) - 1; i >= 0; i-- {
		strides[i] = stride
		if shape[i] == 0 {
			stride = 0
			continue
		}
		if stride <= math.MaxInt64/shape[i] {
			stride *= shape[i]
		}
	}
	return strides
}

func __omnivmTypedSequenceFormat(elem reflect.Type) (string, string, int64, bool) {
	switch elem.Kind() {
	case reflect.Int8:
		return "i8", "c", 1, true
	case reflect.Uint8:
		return "u8", "C", 1, true
	case reflect.Int16:
		return "i16", "s", 2, true
	case reflect.Uint16:
		return "u16", "S", 2, true
	case reflect.Int32:
		return "i32", "i", 4, true
	case reflect.Uint32:
		return "u32", "I", 4, true
	case reflect.Int64:
		return "i64", "l", 8, true
	case reflect.Uint64:
		return "u64", "L", 8, true
	case reflect.Float32:
		return "f32", "f", 4, true
	case reflect.Float64:
		return "f64", "g", 8, true
	case reflect.Int:
		if elem.Size() == 4 {
			return "i32", "i", 4, true
		}
		if elem.Size() == 8 {
			return "i64", "l", 8, true
		}
	case reflect.Uint:
		if elem.Size() == 4 {
			return "u32", "I", 4, true
		}
		if elem.Size() == 8 {
			return "u64", "L", 8, true
		}
	}
	return "", "", 0, false
}

func __omnivmCopyTypedSequenceBytes(out []byte, rv reflect.Value, kind reflect.Kind, elemSize int, offset *int) {
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Array || rv.Kind() == reflect.Slice {
		for i := 0; i < rv.Len(); i++ {
			__omnivmCopyTypedSequenceBytes(out, rv.Index(i), kind, elemSize, offset)
		}
		return
	}
	if *offset < 0 || *offset+elemSize > len(out) {
		return
	}
	slot := out[*offset : *offset+elemSize]
	defer func() { *offset += elemSize }()
	item := rv
	switch kind {
	case reflect.Int8:
		slot[0] = byte(int8(item.Int()))
	case reflect.Uint8:
		slot[0] = byte(item.Uint())
	case reflect.Int16:
		binary.LittleEndian.PutUint16(slot, uint16(int16(item.Int())))
	case reflect.Uint16:
		binary.LittleEndian.PutUint16(slot, uint16(item.Uint()))
	case reflect.Int32:
		binary.LittleEndian.PutUint32(slot, uint32(int32(item.Int())))
	case reflect.Uint32:
		binary.LittleEndian.PutUint32(slot, uint32(item.Uint()))
	case reflect.Int64, reflect.Int:
		if elemSize == 4 {
			binary.LittleEndian.PutUint32(slot, uint32(int32(item.Int())))
		} else {
			binary.LittleEndian.PutUint64(slot, uint64(item.Int()))
		}
	case reflect.Uint64, reflect.Uint:
		if elemSize == 4 {
			binary.LittleEndian.PutUint32(slot, uint32(item.Uint()))
		} else {
			binary.LittleEndian.PutUint64(slot, item.Uint())
		}
	case reflect.Float32:
		binary.LittleEndian.PutUint32(slot, math.Float32bits(float32(item.Float())))
	case reflect.Float64:
		binary.LittleEndian.PutUint64(slot, math.Float64bits(item.Float()))
	}
}

func __omnivmCopyFlatTypedSequenceBytes(out []byte, rv reflect.Value, kind reflect.Kind, elemSize int) {
	for i := 0; i < rv.Len(); i++ {
		item := rv.Index(i)
		offset := i * elemSize
		switch kind {
		case reflect.Int8:
			out[offset] = byte(int8(item.Int()))
		case reflect.Uint8:
			out[offset] = byte(item.Uint())
		case reflect.Int16:
			binary.LittleEndian.PutUint16(out[offset:offset+2], uint16(int16(item.Int())))
		case reflect.Uint16:
			binary.LittleEndian.PutUint16(out[offset:offset+2], uint16(item.Uint()))
		case reflect.Int32:
			binary.LittleEndian.PutUint32(out[offset:offset+4], uint32(int32(item.Int())))
		case reflect.Uint32:
			binary.LittleEndian.PutUint32(out[offset:offset+4], uint32(item.Uint()))
		case reflect.Int64, reflect.Int:
			if elemSize == 4 {
				binary.LittleEndian.PutUint32(out[offset:offset+4], uint32(int32(item.Int())))
			} else {
				binary.LittleEndian.PutUint64(out[offset:offset+8], uint64(item.Int()))
			}
		case reflect.Uint64, reflect.Uint:
			if elemSize == 4 {
				binary.LittleEndian.PutUint32(out[offset:offset+4], uint32(item.Uint()))
			} else {
				binary.LittleEndian.PutUint64(out[offset:offset+8], item.Uint())
			}
		case reflect.Float32:
			binary.LittleEndian.PutUint32(out[offset:offset+4], math.Float32bits(float32(item.Float())))
		case reflect.Float64:
			binary.LittleEndian.PutUint64(out[offset:offset+8], math.Float64bits(item.Float()))
		}
	}
}

func __omnivmInvoke(fn interface{}, argsJSON *C.char) (out *C.char) {
	defer func() {
		if r := recover(); r != nil {
			out = __omnivmCStringEnvelope(nil, fmt.Errorf("%v\n%s", r, string(debug.Stack())))
		}
	}()
	var args []interface{}
	if argsJSON != nil {
		if err := json.Unmarshal([]byte(C.GoString(argsJSON)), &args); err != nil {
			return __omnivmCStringEnvelope(nil, err)
		}
	}
	value, err := __omnivmReflectCall(fn, args)
	return __omnivmCStringEnvelope(value, err)
}

//export OmniVMReleaseBuffer
func OmniVMReleaseBuffer(bufferID *C.char) *C.char {
	id := C.GoString(bufferID)
	if ptr, ok := __omnivmOwnedBuffers.LoadAndDelete(id); ok {
		C.free(ptr.(unsafe.Pointer))
	}
	return __omnivmCStringEnvelope(nil, nil)
}

//export OmniVMReleaseObject
func OmniVMReleaseObject(objectID *C.char) *C.char {
	id := C.GoString(objectID)
	if value, ok := __omnivmOwnedObjects.LoadAndDelete(id); ok {
		if identity, found := __omnivmObjectIdentity(value); found {
			__omnivmObjectIdentities.Delete(identity)
		}
	}
	return __omnivmCStringEnvelope(nil, nil)
}

//export OmniVMHandleOp
func OmniVMHandleOp(argsJSON *C.char) (out *C.char) {
	defer func() {
		if r := recover(); r != nil {
			out = __omnivmCStringEnvelope(nil, fmt.Errorf("%v\n%s", r, string(debug.Stack())))
		}
	}()
	var req map[string]interface{}
	if argsJSON != nil {
		if err := json.Unmarshal([]byte(C.GoString(argsJSON)), &req); err != nil {
			return __omnivmCStringEnvelope(nil, err)
		}
	}
	id, _ := req["handle_id"].(string)
	op, _ := req["op"].(string)
	value, ok := __omnivmOwnedObjects.Load(id)
	if !ok {
		return __omnivmCStringEnvelope(nil, fmt.Errorf("unknown Go c-shared object handle %q", id))
	}
	env, err := __omnivmHandleOp(value, op, req)
	if err != nil {
		return __omnivmCStringEnvelope(nil, err)
	}
	payload, marshalErr := json.Marshal(env)
	if marshalErr != nil {
		payload, _ = json.Marshal(__omnivmEnvelope{OK: false, Error: marshalErr.Error()})
	}
	return C.CString(string(payload))
}

func __omnivmHandleOp(value interface{}, op string, req map[string]interface{}) (__omnivmEnvelope, error) {
	switch op {
	case "read":
		reader, ok := value.(io.Reader)
		if !ok || __omnivmIsHTTPMessageShape(value) {
			return __omnivmEnvelope{OK: true, Found: false}, nil
		}
		size := 8192
		if n, ok := __omnivmInt64(req["size"]); ok && n > 0 {
			size = int(n)
		}
		if size > 1048576 {
			size = 1048576
		}
		buf := make([]byte, size)
		n, err := reader.Read(buf)
		if n > 0 {
			return __omnivmEnvelope{OK: true, Found: true, Value: map[string]interface{}{"done": false, "chunk": buf[:n]}}, nil
		}
		if err == nil || err == io.EOF {
			return __omnivmEnvelope{OK: true, Found: true, Value: map[string]interface{}{"done": true}}, nil
		}
		return __omnivmEnvelope{OK: false, Found: true, Error: err.Error()}, nil
	case "close":
		closer, ok := value.(io.Closer)
		if !ok || __omnivmIsHTTPMessageShape(value) {
			return __omnivmEnvelope{OK: true, Found: false}, nil
		}
		err := closer.Close()
		return __omnivmEnvelope{OK: err == nil, Found: true, Value: true, Error: __omnivmErrorString(err)}, nil
	case "get":
		key, _ := req["key"].(string)
		next, found, err := __omnivmGenericProperty(value, key)
		return __omnivmFoundEnvelope(next, found, err), nil
	case "callable":
		key, _ := req["key"].(string)
		return __omnivmEnvelope{OK: true, Found: true, Value: __omnivmGenericCallable(value, key)}, nil
	case "index":
		next, found, err := __omnivmGenericIndex(value, req["value"])
		return __omnivmFoundEnvelope(next, found, err), nil
	case "set":
		key, _ := req["key"].(string)
		ok, err := __omnivmGenericSet(value, key, req["value"])
		return __omnivmEnvelope{OK: err == nil, Found: ok, Value: ok, Error: __omnivmErrorString(err)}, nil
	case "len":
		n, found, err := __omnivmGenericLen(value)
		return __omnivmEnvelope{OK: err == nil, Found: found, Value: n, Error: __omnivmErrorString(err)}, nil
	case "iter":
		mode, _ := req["mode"].(string)
		values, found, err := __omnivmGenericIter(value, mode)
		return __omnivmEnvelope{OK: err == nil, Found: found, Value: values, Error: __omnivmErrorString(err)}, nil
	case "contains":
		ok, found, err := __omnivmGenericContains(value, req["value"])
		return __omnivmEnvelope{OK: err == nil, Found: ok, Value: found, Error: __omnivmErrorString(err)}, nil
	case "call":
		key, _ := req["key"].(string)
		rawArgs, _ := req["args"].([]interface{})
		next, found, err := __omnivmGenericCall(value, key, rawArgs)
		return __omnivmFoundEnvelope(next, found, err), nil
	default:
		return __omnivmEnvelope{}, fmt.Errorf("unknown Go c-shared object op %q", op)
	}
}

func __omnivmFoundEnvelope(value interface{}, found bool, err error) __omnivmEnvelope {
	if err != nil {
		return __omnivmEnvelope{OK: false, Found: found, Error: err.Error()}
	}
	env := __omnivmEncodeReturn(value)
	env.Found = found
	return env
}

func __omnivmBoundaryValue(value interface{}) interface{} {
	env := __omnivmEncodeReturn(value)
	switch env.Boundary {
	case "owned_handle":
		return map[string]interface{}{
			"__omnivm_cshared_boundary__": true,
			"boundary":                    env.Boundary,
			"handle_id":                   env.HandleID,
			"kind":                        env.Kind,
		}
	case "owned_buffer":
		return map[string]interface{}{
			"__omnivm_cshared_boundary__": true,
			"boundary":                    env.Boundary,
			"dtype":                       env.Dtype,
			"format":                      env.Format,
			"pointer":                     env.Pointer,
			"buffer_id":                   env.BufferID,
			"bytes_len":                   env.BytesLen,
			"elements":                    env.Elements,
			"shape":                       env.Shape,
			"strides":                     env.Strides,
		}
	default:
		return env.Value
	}
}

func __omnivmErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func __omnivmGenericProperty(value interface{}, key string) (interface{}, bool, error) {
	if key == "" {
		return nil, false, nil
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		mapKey, ok := __omnivmConvertReflectValue(key, rv.Type().Key())
		if !ok {
			return nil, false, nil
		}
		out := rv.MapIndex(mapKey)
		if !out.IsValid() {
			return nil, false, nil
		}
		return out.Interface(), true, nil
	case reflect.Struct:
		rt := rv.Type()
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			if field.PkgPath != "" {
				continue
			}
			if field.Name == key || __omnivmJSONFieldName(field.Tag.Get("json")) == key {
				return rv.Field(i).Interface(), true, nil
			}
		}
	}
	return nil, false, nil
}

func __omnivmGenericCallable(value interface{}, key string) bool {
	if key == "" {
		fn := reflect.ValueOf(value)
		return fn.IsValid() && fn.Kind() == reflect.Func
	}
	if prop, ok, err := __omnivmGenericProperty(value, key); err == nil && ok {
		fn := reflect.ValueOf(prop)
		return fn.IsValid() && fn.Kind() == reflect.Func
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false
	}
	if rv.MethodByName(key).IsValid() {
		return true
	}
	if rv.Kind() != reflect.Pointer && rv.CanAddr() {
		return rv.Addr().MethodByName(key).IsValid()
	}
	return false
}

func __omnivmGenericIndex(value interface{}, key interface{}) (interface{}, bool, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		mapKey, ok := __omnivmConvertReflectValue(key, rv.Type().Key())
		if !ok {
			return nil, false, nil
		}
		out := rv.MapIndex(mapKey)
		if !out.IsValid() {
			return nil, false, nil
		}
		return out.Interface(), true, nil
	case reflect.Slice, reflect.Array:
		idx, ok := __omnivmNumericIndex(key)
		if !ok || idx < 0 || idx >= rv.Len() {
			return nil, false, nil
		}
		return rv.Index(idx).Interface(), true, nil
	case reflect.String:
		idx, ok := __omnivmNumericIndex(key)
		if !ok || idx < 0 || idx >= rv.Len() {
			return nil, false, nil
		}
		return string(rv.String()[idx]), true, nil
	}
	return nil, false, nil
}

func __omnivmGenericSet(value interface{}, key string, next interface{}) (bool, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false, nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		mapKey, ok := __omnivmConvertReflectValue(key, rv.Type().Key())
		if !ok {
			return false, nil
		}
		mapValue, ok := __omnivmConvertReflectValue(next, rv.Type().Elem())
		if !ok {
			return false, fmt.Errorf("cannot assign %T to %s", next, rv.Type().Elem())
		}
		rv.SetMapIndex(mapKey, mapValue)
		return true, nil
	case reflect.Slice, reflect.Array:
		idx, ok := __omnivmNumericIndex(key)
		if !ok || idx < 0 || idx >= rv.Len() {
			return false, nil
		}
		slot := rv.Index(idx)
		if !slot.CanSet() {
			return false, fmt.Errorf("cannot assign to %s index %d", rv.Type(), idx)
		}
		slotValue, ok := __omnivmConvertReflectValue(next, slot.Type())
		if !ok {
			return false, fmt.Errorf("cannot assign %T to %s", next, slot.Type())
		}
		slot.Set(slotValue)
		return true, nil
	case reflect.Struct:
		field := __omnivmSettableStructField(rv, key)
		if !field.IsValid() {
			return false, nil
		}
		fieldValue, ok := __omnivmConvertReflectValue(next, field.Type())
		if !ok {
			return false, fmt.Errorf("cannot assign %T to %s", next, field.Type())
		}
		field.Set(fieldValue)
		return true, nil
	}
	return false, nil
}

func __omnivmGenericLen(value interface{}) (int, bool, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return 0, false, nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return 0, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len(), true, nil
	default:
		return 0, false, nil
	}
}

func __omnivmGenericIter(value interface{}, mode string) ([]interface{}, bool, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		keys := rv.MapKeys()
		out := make([]interface{}, 0, len(keys))
		for _, key := range keys {
			switch mode {
			case "values":
				out = append(out, __omnivmBoundaryValue(rv.MapIndex(key).Interface()))
			case "items":
				out = append(out, []interface{}{key.Interface(), __omnivmBoundaryValue(rv.MapIndex(key).Interface())})
			default:
				out = append(out, key.Interface())
			}
		}
		return out, true, nil
	case reflect.Slice, reflect.Array:
		out := make([]interface{}, 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			switch mode {
			case "items":
				out = append(out, []interface{}{i, __omnivmBoundaryValue(rv.Index(i).Interface())})
			case "keys":
				out = append(out, i)
			default:
				out = append(out, __omnivmBoundaryValue(rv.Index(i).Interface()))
			}
		}
		return out, true, nil
	case reflect.String:
		out := make([]interface{}, 0, len(rv.String()))
		for i, r := range rv.String() {
			switch mode {
			case "items":
				out = append(out, []interface{}{i, string(r)})
			case "keys":
				out = append(out, i)
			default:
				out = append(out, string(r))
			}
		}
		return out, true, nil
	default:
		return nil, false, nil
	}
}

func __omnivmGenericContains(value interface{}, key interface{}) (bool, bool, error) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false, false, nil
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false, false, nil
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map:
		mapKey, ok := __omnivmConvertReflectValue(key, rv.Type().Key())
		if !ok {
			return true, false, nil
		}
		return true, rv.MapIndex(mapKey).IsValid(), nil
	case reflect.Slice, reflect.Array:
		if idx, ok := __omnivmNumericIndex(key); ok && idx >= 0 && idx < rv.Len() {
			return true, true, nil
		}
		for i := 0; i < rv.Len(); i++ {
			if reflect.DeepEqual(rv.Index(i).Interface(), key) {
				return true, true, nil
			}
		}
		return true, false, nil
	case reflect.String:
		keyStr, ok := key.(string)
		if !ok {
			return true, false, nil
		}
		return true, strings.Contains(rv.String(), keyStr), nil
	case reflect.Struct:
		keyStr, ok := key.(string)
		if !ok {
			return true, false, nil
		}
		_, found, err := __omnivmGenericProperty(value, keyStr)
		return true, found, err
	default:
		return false, false, nil
	}
}

func __omnivmGenericCall(value interface{}, key string, args []interface{}) (interface{}, bool, error) {
	if key == "" {
		return __omnivmCallReflectCallable(reflect.ValueOf(value), args)
	}
	if prop, ok, err := __omnivmGenericProperty(value, key); err != nil {
		return nil, false, err
	} else if ok {
		if result, called, err := __omnivmCallReflectCallable(reflect.ValueOf(prop), args); called || err != nil {
			return result, called, err
		}
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return nil, false, nil
	}
	method := rv.MethodByName(key)
	if !method.IsValid() && rv.Kind() != reflect.Pointer && rv.CanAddr() {
		method = rv.Addr().MethodByName(key)
	}
	return __omnivmCallReflectCallable(method, args)
}

func __omnivmCallReflectCallable(fn reflect.Value, args []interface{}) (interface{}, bool, error) {
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		return nil, false, nil
	}
	ft := fn.Type()
	if !ft.IsVariadic() && len(args) != ft.NumIn() {
		return nil, true, fmt.Errorf("expected %d args, got %d", ft.NumIn(), len(args))
	}
	if ft.IsVariadic() && len(args) < ft.NumIn()-1 {
		return nil, true, fmt.Errorf("expected at least %d args, got %d", ft.NumIn()-1, len(args))
	}
	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		target := ft.In(i)
		if ft.IsVariadic() && i >= ft.NumIn()-1 {
			target = ft.In(ft.NumIn() - 1).Elem()
		}
		converted, ok := __omnivmConvertReflectValue(__omnivmNormalize(arg), target)
		if !ok {
			return nil, true, fmt.Errorf("arg %d: cannot use %T as %s", i, arg, target)
		}
		in[i] = converted
	}
	outs := fn.Call(in)
	if len(outs) == 0 {
		return nil, true, nil
	}
	if len(outs) == 2 && outs[1].Type().Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		if !outs[1].IsNil() {
			return nil, true, outs[1].Interface().(error)
		}
		return outs[0].Interface(), true, nil
	}
	if len(outs) == 1 {
		return outs[0].Interface(), true, nil
	}
	values := make([]interface{}, 0, len(outs))
	for _, out := range outs {
		values = append(values, out.Interface())
	}
	return values, true, nil
}

func __omnivmConvertReflectValue(value interface{}, target reflect.Type) (reflect.Value, bool) {
	if value == nil {
		switch target.Kind() {
		case reflect.Interface, reflect.Map, reflect.Slice, reflect.Pointer, reflect.Func:
			return reflect.Zero(target), true
		default:
			return reflect.Value{}, false
		}
	}
	if target.Kind() == reflect.Interface {
		val := reflect.ValueOf(value)
		if val.Type().AssignableTo(target) {
			return val, true
		}
		return val, true
	}
	switch target.Kind() {
	case reflect.String:
		if s, ok := value.(string); ok {
			return reflect.ValueOf(s).Convert(target), true
		}
	case reflect.Bool:
		if v, ok := value.(bool); ok {
			return reflect.ValueOf(v).Convert(target), true
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := __omnivmInt64(value)
		if ok {
			v := reflect.New(target).Elem()
			if v.OverflowInt(n) {
				return reflect.Value{}, false
			}
			v.SetInt(n)
			return v, true
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, ok := __omnivmUint64(value)
		if ok {
			v := reflect.New(target).Elem()
			if v.OverflowUint(n) {
				return reflect.Value{}, false
			}
			v.SetUint(n)
			return v, true
		}
	case reflect.Float32, reflect.Float64:
		n, ok := __omnivmFloat64(value)
		if ok {
			v := reflect.New(target).Elem()
			if v.OverflowFloat(n) {
				return reflect.Value{}, false
			}
			v.SetFloat(n)
			return v, true
		}
	}
	val := reflect.ValueOf(value)
	if val.Type().AssignableTo(target) {
		return val, true
	}
	if val.Type().ConvertibleTo(target) {
		return val.Convert(target), true
	}
	return reflect.Value{}, false
}

func __omnivmNumericIndex(value interface{}) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), int64(int(v)) == v
	case float64:
		if math.Trunc(v) == v {
			return int(v), true
		}
	case string:
		n, err := strconv.Atoi(v)
		return n, err == nil
	}
	return 0, false
}

func __omnivmSettableStructField(rv reflect.Value, key string) reflect.Value {
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return reflect.Value{}
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		field := rt.Field(i)
		if field.PkgPath != "" {
			continue
		}
		if field.Name == key || __omnivmJSONFieldName(field.Tag.Get("json")) == key {
			slot := rv.Field(i)
			if slot.CanSet() {
				return slot
			}
		}
	}
	return reflect.Value{}
}

func __omnivmJSONFieldName(tag string) string {
	if tag == "" || tag == "-" {
		return ""
	}
	if idx := strings.IndexByte(tag, ','); idx >= 0 {
		tag = tag[:idx]
	}
	return tag
}

func __omnivmReflectCall(fn interface{}, args []interface{}) (interface{}, error) {
	fv := reflect.ValueOf(fn)
	ft := fv.Type()
	if ft.Kind() != reflect.Func {
		return nil, fmt.Errorf("target is not callable: %s", ft)
	}
	if !ft.IsVariadic() && len(args) != ft.NumIn() {
		return nil, fmt.Errorf("expected %d args, got %d", ft.NumIn(), len(args))
	}
	if ft.IsVariadic() && len(args) < ft.NumIn()-1 {
		return nil, fmt.Errorf("expected at least %d args, got %d", ft.NumIn()-1, len(args))
	}
	in := make([]reflect.Value, len(args))
	for i, arg := range args {
		target := ft.In(i)
		if ft.IsVariadic() && i >= ft.NumIn()-1 {
			target = ft.In(ft.NumIn() - 1).Elem()
		}
		converted, err := __omnivmConvertArg(arg, target)
		if err != nil {
			return nil, fmt.Errorf("arg %d: %w", i, err)
		}
		in[i] = converted
	}
	outs := fv.Call(in)
	if len(outs) == 0 {
		return nil, nil
	}
	if len(outs) == 2 && outs[1].Type().Implements(reflect.TypeOf((*error)(nil)).Elem()) {
		if !outs[1].IsNil() {
			return nil, outs[1].Interface().(error)
		}
		return outs[0].Interface(), nil
	}
	if len(outs) == 1 {
		return outs[0].Interface(), nil
	}
	values := make([]interface{}, 0, len(outs))
	for _, out := range outs {
		values = append(values, out.Interface())
	}
	return values, nil
}

func __omnivmConvertArg(arg interface{}, target reflect.Type) (reflect.Value, error) {
	arg = __omnivmNormalize(arg)
	if converted, ok, err := __omnivmConvertBufferArg(arg, target); ok || err != nil {
		return converted, err
	}
	if arg == nil {
		switch target.Kind() {
		case reflect.Interface, reflect.Map, reflect.Slice, reflect.Pointer, reflect.Func:
			return reflect.Zero(target), nil
		default:
			return reflect.Zero(target), nil
		}
	}
	if target.Kind() == reflect.Interface {
		val := reflect.ValueOf(arg)
		if val.Type().AssignableTo(target) {
			return val, nil
		}
		return val, nil
	}
	switch target.Kind() {
	case reflect.String:
		if s, ok := arg.(string); ok {
			return reflect.ValueOf(s).Convert(target), nil
		}
	case reflect.Bool:
		if v, ok := arg.(bool); ok {
			return reflect.ValueOf(v).Convert(target), nil
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := __omnivmInt64(arg)
		if ok {
			v := reflect.New(target).Elem()
			if v.OverflowInt(n) {
				return reflect.Value{}, fmt.Errorf("%v overflows %s", n, target)
			}
			v.SetInt(n)
			return v, nil
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		n, ok := __omnivmUint64(arg)
		if ok {
			v := reflect.New(target).Elem()
			if v.OverflowUint(n) {
				return reflect.Value{}, fmt.Errorf("%v overflows %s", n, target)
			}
			v.SetUint(n)
			return v, nil
		}
	case reflect.Float32, reflect.Float64:
		n, ok := __omnivmFloat64(arg)
		if ok {
			v := reflect.New(target).Elem()
			if v.OverflowFloat(n) {
				return reflect.Value{}, fmt.Errorf("%v overflows %s", n, target)
			}
			v.SetFloat(n)
			return v, nil
		}
	}
	val := reflect.ValueOf(arg)
	if val.Type().AssignableTo(target) {
		return val, nil
	}
	if val.Type().ConvertibleTo(target) {
		return val.Convert(target), nil
	}
	return reflect.Value{}, fmt.Errorf("cannot use %T as %s", arg, target)
}

func __omnivmConvertBufferArg(arg interface{}, target reflect.Type) (reflect.Value, bool, error) {
	descriptor, ok := arg.(map[string]interface{})
	if !ok || descriptor["__omnivm_cshared_buffer__"] != true || descriptor["boundary"] != "borrowed_buffer" {
		return reflect.Value{}, false, nil
	}
	targetShape, scalarElem, ok := __omnivmTargetBufferShape(target)
	if !ok {
		return reflect.Value{}, true, fmt.Errorf("cannot use c-shared buffer as %s", target)
	}
	dtype, _, elemSize, ok := __omnivmTypedSequenceFormat(scalarElem)
	if !ok {
		return reflect.Value{}, true, fmt.Errorf("cannot use c-shared buffer as %s", target)
	}
	if got, _ := descriptor["dtype"].(string); got != dtype {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer dtype %q cannot be used as %s", got, target)
	}
	bytesLen, ok := __omnivmInt64(descriptor["bytes_len"])
	if !ok || bytesLen < 0 || bytesLen%elemSize != 0 {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer byte length %v is not aligned to %s", descriptor["bytes_len"], target)
	}
	physicalElements := bytesLen / elemSize
	bufferShape := __omnivmInt64Slice(descriptor["shape"])
	if len(bufferShape) == 0 {
		bufferShape = []int64{physicalElements}
	}
	logicalElements, ok := __omnivmShapeProduct(bufferShape)
	if !ok {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer shape %v is invalid for %s", bufferShape, target)
	}
	if explicitElements, ok := __omnivmInt64(descriptor["elements"]); ok && explicitElements != logicalElements {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer elements %d do not match shape %v for %s", explicitElements, bufferShape, target)
	}
	if !__omnivmTargetShapeCompatible(targetShape, bufferShape) {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer shape %v cannot be used as %s", bufferShape, target)
	}
	if int64(int(logicalElements)) != logicalElements {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer element count %d overflows host indexes", logicalElements)
	}
	bufferStrides := __omnivmInt64Slice(descriptor["strides"])
	if len(bufferStrides) == 0 {
		bufferStrides = __omnivmContiguousStrides(bufferShape, elemSize)
	}
	bufferOffset, ok := __omnivmInt64(descriptor["offset"])
	if !ok {
		bufferOffset = 0
	}
	if bufferOffset%elemSize != 0 {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer offset %d is not aligned to %s", bufferOffset, target)
	}
	minOffset, maxOffset, ok := __omnivmStridedBounds(bufferShape, bufferStrides, elemSize)
	if !ok || bufferOffset+minOffset < 0 || bufferOffset+maxOffset > bytesLen {
		return reflect.Value{}, true, fmt.Errorf("c-shared buffer shape %v strides %v offset %d require byte range [%d,%d) but buffer has %d", bufferShape, bufferStrides, bufferOffset, bufferOffset+minOffset, bufferOffset+maxOffset, bytesLen)
	}
	if bytesLen == 0 {
		switch target.Kind() {
		case reflect.Slice:
			if len(bufferShape) > 1 || target.Elem().Kind() == reflect.Slice || target.Elem().Kind() == reflect.Array {
				empty := reflect.MakeSlice(reflect.SliceOf(scalarElem), 0, 0)
				return __omnivmCompositeFromFlat(empty, target, bufferShape)
			}
			return reflect.MakeSlice(target, int(bufferShape[0]), int(bufferShape[0])), true, nil
		case reflect.Array:
			empty := reflect.MakeSlice(reflect.SliceOf(scalarElem), 0, 0)
			return __omnivmCompositeFromFlat(empty, target, bufferShape)
		default:
			return reflect.Value{}, true, fmt.Errorf("cannot use empty c-shared buffer as %s", target)
		}
	}
	ptrString, _ := descriptor["pointer"].(string)
	ptr, err := strconv.ParseUint(ptrString, 10, 64)
	if err != nil || ptr == 0 {
		return reflect.Value{}, true, fmt.Errorf("invalid c-shared buffer pointer %q", ptrString)
	}
	contiguous := __omnivmStridesAreContiguous(bufferShape, bufferStrides, elemSize)
	if !contiguous {
		return __omnivmCompositeFromStrided(uintptr(ptr), target, bufferShape, bufferStrides, bufferOffset)
	}
	sliceValue, err := __omnivmBorrowedBufferSliceValue(unsafe.Pointer(uintptr(ptr)+uintptr(bufferOffset)), int(logicalElements), scalarElem)
	if err != nil {
		return reflect.Value{}, true, err
	}
	switch target.Kind() {
	case reflect.Slice:
		if len(targetShape) > 1 || target.Elem().Kind() == reflect.Slice || target.Elem().Kind() == reflect.Array {
			return __omnivmCompositeFromFlat(sliceValue, target, bufferShape)
		}
		if sliceValue.Type().AssignableTo(target) {
			return sliceValue, true, nil
		}
		if sliceValue.Type().ConvertibleTo(target) {
			return sliceValue.Convert(target), true, nil
		}
		case reflect.Array:
			return __omnivmCompositeFromFlat(sliceValue, target, bufferShape)
		}
		return reflect.Value{}, true, fmt.Errorf("cannot use c-shared buffer as %s", target)
	}

func __omnivmTargetBufferShape(target reflect.Type) ([]int64, reflect.Type, bool) {
	shape := []int64{}
	for target.Kind() == reflect.Slice || target.Kind() == reflect.Array {
		if target.Kind() == reflect.Slice {
			shape = append(shape, -1)
		} else {
			shape = append(shape, int64(target.Len()))
		}
		target = target.Elem()
	}
	if len(shape) == 0 {
		return nil, nil, false
	}
	return shape, target, true
}

func __omnivmTargetShapeCompatible(targetShape, bufferShape []int64) bool {
	if len(targetShape) != len(bufferShape) {
		return false
	}
	for i, want := range targetShape {
		if want >= 0 && want != bufferShape[i] {
			return false
		}
	}
	return true
}

func __omnivmInt64Slice(value interface{}) []int64 {
	raw, ok := value.([]interface{})
	if !ok {
		return nil
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		n, ok := __omnivmInt64(item)
		if !ok {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func __omnivmStridesAreContiguous(shape, strides []int64, elemSize int64) bool {
	if len(strides) != len(shape) {
		return false
	}
	expected := __omnivmContiguousStrides(shape, elemSize)
	for i := range strides {
		if strides[i] != expected[i] {
			return false
		}
	}
	return true
}

func __omnivmStridedBounds(shape []int64, strides []int64, elemSize int64) (int64, int64, bool) {
	if len(shape) != len(strides) || elemSize <= 0 {
		return 0, 0, false
	}
	minOffset := int64(0)
	maxOffset := elemSize
	for i, dim := range shape {
		if dim < 0 {
			return 0, 0, false
		}
		if dim == 0 {
			return 0, 0, true
		}
		steps := dim - 1
		stride := strides[i]
		if stride >= 0 {
			if steps != 0 && stride > (math.MaxInt64-maxOffset)/steps {
				return 0, 0, false
			}
			maxOffset += steps * stride
			continue
		}
		if steps != 0 && stride < math.MinInt64/steps {
			return 0, 0, false
		}
		delta := steps * stride
		if minOffset < math.MinInt64-delta {
			return 0, 0, false
		}
		minOffset += delta
	}
	if maxOffset < minOffset {
		return 0, 0, false
	}
	return minOffset, maxOffset, true
}

func __omnivmNestedSliceFromFlat(flat reflect.Value, target reflect.Type, shape []int64) (reflect.Value, bool, error) {
	index := 0
	out, err := __omnivmCompositeFromFlatAt(flat, target, shape, &index)
	return out, true, err
}

func __omnivmCompositeFromFlat(flat reflect.Value, target reflect.Type, shape []int64) (reflect.Value, bool, error) {
	index := 0
	out, err := __omnivmCompositeFromFlatAt(flat, target, shape, &index)
	return out, true, err
}

func __omnivmCompositeFromFlatAt(flat reflect.Value, target reflect.Type, shape []int64, index *int) (reflect.Value, error) {
	switch target.Kind() {
	case reflect.Slice:
		if len(shape) == 0 || shape[0] < 0 || int64(int(shape[0])) != shape[0] {
			return reflect.Value{}, fmt.Errorf("cannot materialize c-shared buffer shape %v as %s", shape, target)
		}
		out := reflect.MakeSlice(target, int(shape[0]), int(shape[0]))
		for i := 0; i < out.Len(); i++ {
			next, err := __omnivmCompositeFromFlatAt(flat, target.Elem(), shape[1:], index)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(next)
		}
		return out, nil
	case reflect.Array:
		if len(shape) == 0 || shape[0] != int64(target.Len()) {
			return reflect.Value{}, fmt.Errorf("cannot materialize c-shared buffer shape %v as %s", shape, target)
		}
		out := reflect.New(target).Elem()
		for i := 0; i < out.Len(); i++ {
			next, err := __omnivmCompositeFromFlatAt(flat, target.Elem(), shape[1:], index)
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(next)
		}
		return out, nil
	default:
		if *index < 0 || *index >= flat.Len() {
			return reflect.Value{}, fmt.Errorf("c-shared buffer ended before filling %s", target)
		}
		value := flat.Index(*index)
		(*index)++
		if value.Type().AssignableTo(target) {
			return value, nil
		}
		if value.Type().ConvertibleTo(target) {
			return value.Convert(target), nil
		}
		return reflect.Value{}, fmt.Errorf("cannot assign c-shared buffer element %s to %s", value.Type(), target)
	}
}

func __omnivmCompositeFromStrided(ptr uintptr, target reflect.Type, shape []int64, strides []int64, offset int64) (reflect.Value, bool, error) {
	out, err := __omnivmCompositeFromStridedAt(ptr, target, shape, strides, offset)
	return out, true, err
}

func __omnivmCompositeFromStridedAt(ptr uintptr, target reflect.Type, shape []int64, strides []int64, offset int64) (reflect.Value, error) {
	switch target.Kind() {
	case reflect.Slice:
		if len(shape) == 0 || shape[0] < 0 || int64(int(shape[0])) != shape[0] || len(strides) != len(shape) {
			return reflect.Value{}, fmt.Errorf("cannot materialize c-shared buffer shape %v strides %v as %s", shape, strides, target)
		}
		out := reflect.MakeSlice(target, int(shape[0]), int(shape[0]))
		for i := 0; i < out.Len(); i++ {
			next, err := __omnivmCompositeFromStridedAt(ptr, target.Elem(), shape[1:], strides[1:], offset+int64(i)*strides[0])
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(next)
		}
		return out, nil
	case reflect.Array:
		if len(shape) == 0 || shape[0] != int64(target.Len()) || len(strides) != len(shape) {
			return reflect.Value{}, fmt.Errorf("cannot materialize c-shared buffer shape %v strides %v as %s", shape, strides, target)
		}
		out := reflect.New(target).Elem()
		for i := 0; i < out.Len(); i++ {
			next, err := __omnivmCompositeFromStridedAt(ptr, target.Elem(), shape[1:], strides[1:], offset+int64(i)*strides[0])
			if err != nil {
				return reflect.Value{}, err
			}
			out.Index(i).Set(next)
		}
		return out, nil
	default:
		if offset < 0 {
			return reflect.Value{}, fmt.Errorf("negative c-shared buffer element offset %d for %s", offset, target)
		}
		return reflect.NewAt(target, unsafe.Pointer(ptr+uintptr(offset))).Elem(), nil
	}
}

func __omnivmFillArrayFromFlat(out reflect.Value, flat reflect.Value, index *int) error {
	if out.Kind() == reflect.Array {
		for i := 0; i < out.Len(); i++ {
			if err := __omnivmFillArrayFromFlat(out.Index(i), flat, index); err != nil {
				return err
			}
		}
		return nil
	}
	if *index < 0 || *index >= flat.Len() {
		return fmt.Errorf("c-shared buffer ended before filling %s", out.Type())
	}
	value := flat.Index(*index)
	(*index)++
	if value.Type().AssignableTo(out.Type()) {
		out.Set(value)
		return nil
	}
	if value.Type().ConvertibleTo(out.Type()) {
		out.Set(value.Convert(out.Type()))
		return nil
	}
	return fmt.Errorf("cannot assign c-shared buffer element %s to %s", value.Type(), out.Type())
}

func __omnivmBorrowedBufferSliceValue(ptr unsafe.Pointer, elements int, elem reflect.Type) (reflect.Value, error) {
	switch elem.Kind() {
	case reflect.Int8:
		return reflect.ValueOf(unsafe.Slice((*int8)(ptr), elements)), nil
	case reflect.Uint8:
		return reflect.ValueOf(unsafe.Slice((*uint8)(ptr), elements)), nil
	case reflect.Int16:
		return reflect.ValueOf(unsafe.Slice((*int16)(ptr), elements)), nil
	case reflect.Uint16:
		return reflect.ValueOf(unsafe.Slice((*uint16)(ptr), elements)), nil
	case reflect.Int32:
		return reflect.ValueOf(unsafe.Slice((*int32)(ptr), elements)), nil
	case reflect.Uint32:
		return reflect.ValueOf(unsafe.Slice((*uint32)(ptr), elements)), nil
	case reflect.Int64:
		return reflect.ValueOf(unsafe.Slice((*int64)(ptr), elements)), nil
	case reflect.Uint64:
		return reflect.ValueOf(unsafe.Slice((*uint64)(ptr), elements)), nil
	case reflect.Float32:
		return reflect.ValueOf(unsafe.Slice((*float32)(ptr), elements)), nil
	case reflect.Float64:
		return reflect.ValueOf(unsafe.Slice((*float64)(ptr), elements)), nil
	case reflect.Int:
		return reflect.ValueOf(unsafe.Slice((*int)(ptr), elements)), nil
	case reflect.Uint:
		return reflect.ValueOf(unsafe.Slice((*uint)(ptr), elements)), nil
	}
	return reflect.Value{}, fmt.Errorf("unsupported c-shared buffer element type %s", elem)
}

func __omnivmNormalize(value interface{}) interface{} {
	switch v := value.(type) {
	case float64:
		if math.Trunc(v) == v && v >= float64(__omnivmMinInt) && v <= float64(__omnivmMaxInt) {
			return int(v)
		}
		return v
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, __omnivmNormalize(item))
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, item := range v {
			out[key] = __omnivmNormalize(item)
		}
		return out
	default:
		return value
	}
}

func __omnivmInt64(value interface{}) (int64, bool) {
	switch v := value.(type) {
	case int:
		return int64(v), true
	case int8:
		return int64(v), true
	case int16:
		return int64(v), true
	case int32:
		return int64(v), true
	case int64:
		return v, true
	case float64:
		if math.Trunc(v) == v {
			return int64(v), true
		}
	}
	return 0, false
}

func __omnivmUint64(value interface{}) (uint64, bool) {
	switch v := value.(type) {
	case uint:
		return uint64(v), true
	case uint8:
		return uint64(v), true
	case uint16:
		return uint64(v), true
	case uint32:
		return uint64(v), true
	case uint64:
		return v, true
	case int:
		if v >= 0 {
			return uint64(v), true
		}
	case float64:
		if math.Trunc(v) == v && v >= 0 {
			return uint64(v), true
		}
	}
	return 0, false
}

func __omnivmFloat64(value interface{}) (float64, bool) {
	switch v := value.(type) {
	case float32:
		return float64(v), true
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}

const __omnivmMaxInt = int(^uint(0) >> 1)
const __omnivmMinInt = -__omnivmMaxInt - 1

`)
	for _, exportName := range exports {
		if !goIdentifierRE.MatchString(exportName) {
			continue
		}
		symbol := cSharedWrapperSymbol(exportName)
		b.WriteString(fmt.Sprintf(`
//export %s
func %s(argsJSON *C.char) *C.char {
	return __omnivmInvoke(%s, argsJSON)
}
`, symbol, symbol, exportName))
	}
	b.WriteString(`
func main() {}
`)
	return b.String()
}

var goIdentifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func cSharedWrapperSymbol(exportName string) string {
	return "OmniVMCall_" + exportName
}

func (e *Executor) registerGoChannelWorkerFallback(op *Op) bool {
	if !requiresGoChannelBuiltins(op) {
		return false
	}
	fn := e.channelWorkerFallback(op.Source)
	if fn == nil {
		return false
	}

	if op.Name != "" {
		e.goFuncs[op.Name] = fn
	}
	for _, exportName := range op.Exports {
		e.goFuncs[exportName] = fn
	}

	params := make([]string, len(op.Params))
	for i, p := range op.Params {
		params[i] = p.Name
	}
	fd := &FuncDef{Name: op.Name, Params: op.Params}
	if err := e.registerStubs(fd); err != nil {
		fmt.Fprintf(os.Stderr, "go fallback stubs %q: %v\n", op.Name, err)
	}
	return true
}

func requiresGoChannelBuiltins(op *Op) bool {
	if len(op.Requires) == 0 {
		return false
	}
	needsRecv := false
	needsSend := false
	for _, req := range op.Requires {
		if req == "recv" {
			needsRecv = true
		}
		if req == "send" {
			needsSend = true
		}
	}
	return needsRecv && needsSend
}

func (e *Executor) channelWorkerFallback(source string) func([]interface{}) interface{} {
	recvName := firstStringLiteralAfter(source, "recv(")
	sendName := firstStringLiteralAfter(source, "send(")
	if recvName == "" || sendName == "" {
		return nil
	}
	return func(args []interface{}) interface{} {
		id := firstArg(args)
		for {
			item := e.goFuncs["recv"].(func(interface{}) interface{})(recvName)
			if item == nil {
				return id
			}
			e.goFuncs["send"].(func(interface{}, interface{}) interface{})(sendName, item)
		}
	}
}

func firstStringLiteralAfter(source, marker string) string {
	idx := strings.Index(source, marker)
	if idx < 0 {
		return ""
	}
	rest := source[idx+len(marker):]
	start := strings.Index(rest, "\"")
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, "\"")
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func firstArg(args []interface{}) interface{} {
	if len(args) == 0 {
		return nil
	}
	return normalizeArg(args[0])
}

// compilePlugin writes Go source to a temp directory and builds it as a plugin.
func compilePlugin(source, outputPath string) error {
	if err := os.MkdirAll(pluginCacheDir, 0o755); err != nil {
		return err
	}

	// Create temp directory for compilation
	tmpDir, err := os.MkdirTemp("", "omnivm-plugin-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	// Rewrite package declaration to "main" (Go plugins require package main)
	pkgRe := regexp.MustCompile(`(?m)^package\s+\w+`)
	source = pkgRe.ReplaceAllString(source, "package main")

	// Write source
	srcPath := filepath.Join(tmpDir, "plugin.go")
	if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
		return err
	}

	// Write go.mod — each plugin needs a unique module name
	// so Go's plugin system treats them as distinct packages.
	modName := fmt.Sprintf("omnivm-plugin-%s", filepath.Base(outputPath[:len(outputPath)-3]))
	goVer := strings.TrimPrefix(runtime.Version(), "go")
	if parts := strings.SplitN(goVer, ".", 3); len(parts) >= 2 {
		goVer = parts[0] + "." + parts[1]
	}
	modContent := fmt.Sprintf("module %s\n\ngo %s\n", modName, goVer)
	modPath := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(modPath, []byte(modContent), 0o644); err != nil {
		return err
	}

	// Build plugin
	goTool, err := goToolPath()
	if err != nil {
		return err
	}
	cmd := exec.Command(goTool, "build", "-buildmode=plugin", "-o", outputPath, ".")
	cmd.Dir = tmpDir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %s: %w", string(out), err)
	}

	return nil
}

func sha256Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", h)
}

func goToolPath() (string, error) {
	if path, err := exec.LookPath("go"); err == nil {
		return path, nil
	}
	if goroot := runtime.GOROOT(); goroot != "" {
		path := filepath.Join(goroot, "bin", "go")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("go toolchain not found")
}
