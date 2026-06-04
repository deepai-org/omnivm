package python

import (
	"encoding/binary"
	"math"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/omnivm/omnivm/pkg/arrow"
)

func init() {
	// Pin test goroutine to main OS thread (required by CPython)
	runtime.LockOSThread()
}

func TestPythonInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if !r.initialized {
		t.Fatal("expected initialized=true")
	}
}

func TestPythonDoubleInitialize(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if err := r.Initialize(); err == nil {
		t.Fatal("expected error on double initialize")
	}
}

func TestPythonExecuteSimple(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("print('hello from python')")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "hello from python\n" {
		t.Fatalf("expected 'hello from python\\n', got %q", result.Output)
	}
}

func TestPythonExecuteExpression(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("print(2 + 2)")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "4\n" {
		t.Fatalf("expected '4\\n', got %q", result.Output)
	}
}

func TestPythonExecuteError(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("raise ValueError('test error')")
	if result.Err == nil {
		t.Fatal("expected error from invalid code")
	}
}

func TestPythonRuntimeErrorPreludeStructuredEnvelopeSource(t *testing.T) {
	for _, want := range []string{
		"self.origin_runtime = parsed['origin_runtime']",
		"self._stack_frames = _copy_json_value(parsed['stack_frames'])",
		"def stack_frames(self):",
		"return _copy_json_value(self._stack_frames)",
		"def cause_chain(self):",
		"return _copy_json_value(self._cause_chain)",
		"def details(self):",
		"return _copy_json_value(self._details)",
		"'origin_runtime': self.origin_runtime",
		"'stack_frames': _copy_json_value(self.stack_frames)",
		"def as_dict(self):",
		"return self.to_dict()",
		"def _parse_runtime_error_envelope",
		"if body.startswith('ERR:')",
		"origin_runtime = text_field(field('origin_runtime', 'originRuntime'), runtime_name)",
		"def details_field(source):",
		"raw_details = source.get('details_json')",
		"raw_details = source.get('detailsJson')",
		"return __omnivm_json.loads(raw_details)",
		"return raw_details",
		"stack_frames = field('stack_frames', 'stackFrames')",
		"cause_traceback = cause.get('traceback')",
		"item['stack_frames'] = list(cause_stack_frames)",
		"cause_details = details_field(cause)",
		"item['details'] = cause_details",
		"details': details_field(envelope)",
		"wrapped_boundary = ' > '.join(boundary_parts) or (f'call[{source_runtime}]' if source_runtime and source_runtime != runtime else boundary_path)",
		"envelope = _parse_runtime_error_envelope(body, runtime=source_runtime, boundary_path=wrapped_boundary)",
		"'origin_runtime': source_runtime",
		"'stack_frames': _runtime_error_stack_frames(traceback)",
		"return value",
	} {
		if !strings.Contains(runtimeErrorPreludeForTest(), want) {
			t.Fatalf("embedded Python RuntimeError prelude missing %q", want)
		}
	}
}

func TestPythonExecuteMultiline(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	code := `
x = 10
y = 20
print(x + y)
`
	result := r.Execute(code)
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	if result.Output != "30\n" {
		t.Fatalf("expected '30\\n', got %q", result.Output)
	}
}

func TestPythonNotInitialized(t *testing.T) {
	r := New()
	result := r.Execute("print('hi')")
	if result.Err == nil {
		t.Fatal("expected error when not initialized")
	}
}

func TestPythonImportStdlib(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute("import json; print(json.dumps({'key': 'value'}))")
	if result.Err != nil {
		t.Fatalf("Execute failed: %v", result.Err)
	}
	expected := `{"key": "value"}` + "\n"
	if result.Output != expected {
		t.Fatalf("expected %q, got %q", expected, result.Output)
	}
}

func TestPythonPump(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	// Pump should not crash even with no event loop
	r.Pump()
}

func TestPythonPumpCompletesScheduledCoroutine(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	result := r.Execute(`
import asyncio
__omnivm_pump_done = False
async def __omnivm_pump_task():
    global __omnivm_pump_done
    __omnivm_pump_done = True
__omnivm_pump_loop = asyncio.new_event_loop()
asyncio.set_event_loop(__omnivm_pump_loop)
asyncio.ensure_future(__omnivm_pump_task(), loop=__omnivm_pump_loop)
`)
	if result.Err != nil {
		t.Fatalf("schedule coroutine: %v", result.Err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		r.Pump()
		check := r.Eval("__omnivm_pump_done")
		if check.Err == nil && check.Value == "True" {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("scheduled coroutine did not complete after pumping")
}

func TestPythonExportBufferUsesLiveBufferProtocolMemory(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute("payload = bytearray(b'abc')"); result.Err != nil {
		t.Fatalf("create payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-bytearray", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("bytearray should export through the generic buffer protocol")
	}
	if exported.Dtype != arrow.DtypeU8 || exported.ArrowFormat != "C" || exported.Elements != 3 || exported.ReadOnly {
		t.Fatalf("bad exported metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-bytearray")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	data[1] = 'z'
	lease.Release()

	result := r.Eval("payload[1]")
	if result.Err != nil {
		t.Fatalf("eval mutated payload: %v", result.Err)
	}
	if result.Value != "122" {
		t.Fatalf("exported buffer was not live zero-copy memory, payload[1]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-bytearray"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferUsesEmptyBufferProtocolMemory(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute("payload = bytearray()"); result.Err != nil {
		t.Fatalf("create empty payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-empty-bytearray", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("empty bytearray should export through the generic buffer protocol")
	}
	if exported.Dtype != arrow.DtypeU8 || exported.ArrowFormat != "C" || exported.Elements != 0 || len(exported.Shape) != 1 || exported.Shape[0] != 0 {
		t.Fatalf("bad exported empty metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-empty-bytearray")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 0 || lease.Data != nil || len(lease.Metadata.Shape) != 1 || lease.Metadata.Shape[0] != 0 {
		t.Fatalf("bad borrowed empty metadata: len=%d data=%v metadata=%+v", lease.Len, lease.Data, lease.Metadata)
	}
	lease.Release()
	if err := arrow.GlobalStore().Free("python-export-empty-bytearray"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferPreservesShapeAndStrides(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute("payload = memoryview(bytearray(b'abcdef')).cast('B', shape=[2, 3])"); result.Err != nil {
		t.Fatalf("create shaped payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-shaped", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("shaped memoryview should export through the generic buffer protocol")
	}
	if len(exported.Shape) != 2 || exported.Shape[0] != 2 || exported.Shape[1] != 3 {
		t.Fatalf("bad exported shape: %+v", exported)
	}
	if len(exported.Strides) != 2 || exported.Strides[0] != 3 || exported.Strides[1] != 1 {
		t.Fatalf("bad exported strides: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-shaped")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if len(lease.Metadata.Shape) != 2 || lease.Metadata.Shape[0] != 2 || lease.Metadata.Shape[1] != 3 {
		t.Fatalf("bad borrowed shape metadata: %+v", lease.Metadata)
	}
	if len(lease.Metadata.Strides) != 2 || lease.Metadata.Strides[0] != 3 || lease.Metadata.Strides[1] != 1 {
		t.Fatalf("bad borrowed stride metadata: %+v", lease.Metadata)
	}
	lease.Release()
	if err := arrow.GlobalStore().Free("python-export-shaped"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferRejectsWrongEndianBufferProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import numpy as np
payload = np.array([258, 772, 1286], dtype=">u2" if __import__("sys").byteorder == "little" else "<u2")
`); result.Err != nil {
		t.Fatalf("create wrong-endian buffer payload: %v", result.Err)
	}
	if exported, ok, err := r.ExportBuffer("python-export-wrong-endian-buffer-protocol", "payload"); err != nil || ok {
		t.Fatalf("wrong-endian buffer export = (%+v,%v,%v), want no zero-copy export and no error", exported, ok, err)
	}
}

func TestPythonExportBufferUsesStridedMemoryview(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute("source = bytearray(b'abcdef')\npayload = memoryview(source)[::2]"); result.Err != nil {
		t.Fatalf("create strided memoryview payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-strided-memoryview", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("positive-stride memoryview should export through the generic buffer protocol")
	}
	if exported.Dtype != arrow.DtypeU8 || exported.ArrowFormat != "C" || exported.Elements != 3 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || len(exported.Strides) != 1 || exported.Strides[0] != 2 {
		t.Fatalf("bad exported strided memoryview metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-strided-memoryview")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 5 || len(lease.Metadata.Strides) != 1 || lease.Metadata.Strides[0] != 2 {
		t.Fatalf("bad borrowed strided memoryview metadata: len=%d metadata=%+v", lease.Len, lease.Metadata)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	data[2] = 'Z'
	lease.Release()

	result := r.Eval("source[2]")
	if result.Err != nil {
		t.Fatalf("eval mutated strided memoryview payload: %v", result.Err)
	}
	if result.Value != "90" {
		t.Fatalf("strided memoryview export was not live zero-copy memory, source[2]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-strided-memoryview"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferUsesNegativeStridedMemoryview(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute("source = bytearray(b'abcdef')\npayload = memoryview(source)[::-2]"); result.Err != nil {
		t.Fatalf("create negative-strided memoryview payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-negative-strided-memoryview", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("negative-stride memoryview should export through the generic buffer protocol")
	}
	if exported.Dtype != arrow.DtypeU8 || exported.ArrowFormat != "C" || exported.Elements != 3 || exported.Offset != 4 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || len(exported.Strides) != 1 || exported.Strides[0] != -2 {
		t.Fatalf("bad exported negative-strided memoryview metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-negative-strided-memoryview")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 5 || lease.Metadata.Offset != 4 || len(lease.Metadata.Strides) != 1 || lease.Metadata.Strides[0] != -2 {
		t.Fatalf("bad borrowed negative-strided memoryview metadata: len=%d metadata=%+v", lease.Len, lease.Metadata)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	data[2] = 'Z'
	lease.Release()

	result := r.Eval("source[3]")
	if result.Err != nil {
		t.Fatalf("eval mutated negative-strided memoryview payload: %v", result.Err)
	}
	if result.Value != "90" {
		t.Fatalf("negative-strided memoryview export was not live zero-copy memory, source[3]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-negative-strided-memoryview"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferSupportsUnsigned16(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute("from array import array\npayload = array('H', [513, 1027])"); result.Err != nil {
		t.Fatalf("create uint16 payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-u16", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("array('H') should export through the generic buffer protocol")
	}
	if exported.Dtype != arrow.DtypeU16 || exported.ArrowFormat != "S" || exported.Elements != 2 {
		t.Fatalf("bad exported uint16 metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-u16")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Metadata.Dtype != arrow.DtypeU16 || lease.Metadata.Format != "S" {
		t.Fatalf("bad borrowed uint16 metadata: %+v", lease.Metadata)
	}
	lease.Release()
	if err := arrow.GlobalStore().Free("python-export-u16"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferSupportsUnsigned64(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute("from array import array\npayload = array('Q', [9223372036854775808, 9223372036854775813])"); result.Err != nil {
		t.Fatalf("create uint64 payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-u64", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("array('Q') should export through the generic buffer protocol")
	}
	if exported.Dtype != arrow.DtypeU64 || exported.ArrowFormat != "L" || exported.Elements != 2 {
		t.Fatalf("bad exported uint64 metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-u64")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Metadata.Dtype != arrow.DtypeU64 || lease.Metadata.Format != "L" {
		t.Fatalf("bad borrowed uint64 metadata: %+v", lease.Metadata)
	}
	lease.Release()
	if err := arrow.GlobalStore().Free("python-export-u64"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferUsesArrayInterface(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes
class ArrayInterfaceOnly:
    def __init__(self):
        self.backing = (ctypes.c_uint16 * 3)(258, 772, 1286)
    @property
    def __array_interface__(self):
        return {
            "data": (ctypes.addressof(self.backing), False),
            "shape": (3,),
            "typestr": "<u2",
            "version": 3,
        }
payload = ArrayInterfaceOnly()
`); result.Err != nil {
		t.Fatalf("create array-interface payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-array-interface", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("__array_interface__ object should export through the generic array protocol")
	}
	if exported.Dtype != arrow.DtypeU16 || exported.ArrowFormat != "S" || exported.Elements != 3 || len(exported.Shape) != 1 || exported.Shape[0] != 3 {
		t.Fatalf("bad exported array-interface metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-array-interface")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	binary.LittleEndian.PutUint16(data[2:4], 2049)
	lease.Release()

	result := r.Eval("payload.backing[1]")
	if result.Err != nil {
		t.Fatalf("eval mutated array-interface payload: %v", result.Err)
	}
	if result.Value != "2049" {
		t.Fatalf("array-interface export was not live zero-copy memory, payload.backing[1]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-array-interface"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferRejectsWrongEndianArrayInterface(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes
class WrongEndianArrayInterfaceOnly:
    def __init__(self):
        self.backing = (ctypes.c_uint16 * 3)(258, 772, 1286)
    @property
    def __array_interface__(self):
        return {
            "data": (ctypes.addressof(self.backing), False),
            "shape": (3,),
            "typestr": ">u2",
            "version": 3,
        }
payload = WrongEndianArrayInterfaceOnly()
`); result.Err != nil {
		t.Fatalf("create wrong-endian array-interface payload: %v", result.Err)
	}
	if exported, ok, err := r.ExportBuffer("python-export-wrong-endian-array-interface", "payload"); err != nil || ok {
		t.Fatalf("wrong-endian __array_interface__ export = (%+v,%v,%v), want no zero-copy export and no error", exported, ok, err)
	}
}

func TestPythonExportBufferUsesArrayMethodProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import numpy as np
class ArrayMethodOnly:
    def __init__(self):
        self.backing = np.arange(6, dtype=np.int16)
        self.view = self.backing[::2]
    def __array__(self, dtype=None, copy=None):
        if dtype is not None:
            return self.view.astype(dtype, copy=False)
        return self.view
payload = ArrayMethodOnly()
`); result.Err != nil {
		t.Fatalf("create __array__ payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-array-method", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("__array__ object should export through the generic array protocol")
	}
	if exported.Dtype != arrow.DtypeI16 || exported.ArrowFormat != "s" || exported.Elements != 3 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || len(exported.Strides) != 1 || exported.Strides[0] != 4 {
		t.Fatalf("bad exported __array__ metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-array-method")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 10 || len(lease.Metadata.Strides) != 1 || lease.Metadata.Strides[0] != 4 {
		t.Fatalf("bad borrowed __array__ metadata: len=%d metadata=%+v", lease.Len, lease.Metadata)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	binary.LittleEndian.PutUint16(data[4:6], 99)
	lease.Release()

	result := r.Eval("int(payload.view[1])")
	if result.Err != nil {
		t.Fatalf("eval mutated __array__ payload: %v", result.Err)
	}
	if result.Value != "99" {
		t.Fatalf("__array__ export was not live zero-copy memory, payload.view[1]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-array-method"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferUsesDLPackProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import numpy as np
class DLPackOnly:
    def __init__(self):
        self.backing = np.arange(6, dtype=np.int32).reshape(2, 3)
        self.view = self.backing[:, ::2]
    def __dlpack__(self, stream=None):
        return self.view.__dlpack__(stream=stream)
payload = DLPackOnly()
`); result.Err != nil {
		t.Fatalf("create __dlpack__ payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-dlpack", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("__dlpack__ object should export through the generic DLPack protocol")
	}
	if exported.Dtype != arrow.DtypeI32 || exported.ArrowFormat != "i" || exported.Elements != 4 || len(exported.Shape) != 2 || exported.Shape[0] != 2 || exported.Shape[1] != 2 || len(exported.Strides) != 2 || exported.Strides[0] != 12 || exported.Strides[1] != 8 {
		t.Fatalf("bad exported __dlpack__ metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-dlpack")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 24 || len(lease.Metadata.Strides) != 2 || lease.Metadata.Strides[0] != 12 || lease.Metadata.Strides[1] != 8 {
		t.Fatalf("bad borrowed __dlpack__ metadata: len=%d metadata=%+v", lease.Len, lease.Metadata)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	binary.LittleEndian.PutUint32(data[20:24], 77)
	lease.Release()

	result := r.Eval("int(payload.view[1, 1])")
	if result.Err != nil {
		t.Fatalf("eval mutated __dlpack__ payload: %v", result.Err)
	}
	if result.Value != "77" {
		t.Fatalf("__dlpack__ export was not live zero-copy memory, payload.view[1,1]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-dlpack"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferReleasesDLPackManagedTensor(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes
released = ctypes.c_int(0)

class DLDevice(ctypes.Structure):
    _fields_ = [("device_type", ctypes.c_int32), ("device_id", ctypes.c_int32)]

class DLDataType(ctypes.Structure):
    _fields_ = [("code", ctypes.c_uint8), ("bits", ctypes.c_uint8), ("lanes", ctypes.c_uint16)]

class DLTensor(ctypes.Structure):
    _fields_ = [
        ("data", ctypes.c_void_p),
        ("device", DLDevice),
        ("ndim", ctypes.c_int32),
        ("dtype", DLDataType),
        ("shape", ctypes.POINTER(ctypes.c_int64)),
        ("strides", ctypes.POINTER(ctypes.c_int64)),
        ("byte_offset", ctypes.c_uint64),
    ]

class DLManagedTensor(ctypes.Structure):
    pass

Deleter = ctypes.CFUNCTYPE(None, ctypes.POINTER(DLManagedTensor))
DLManagedTensor._fields_ = [("dl_tensor", DLTensor), ("manager_ctx", ctypes.c_void_p), ("deleter", Deleter)]

@Deleter
def managed_deleter(ptr):
    released.value += 1

class DLPackOwned:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 2)(11, 22)
        self.shape = (ctypes.c_int64 * 1)(2)
        self.strides = (ctypes.c_int64 * 1)(1)
        self.deleter = managed_deleter
        self.managed = DLManagedTensor(
            DLTensor(
                ctypes.cast(self.backing, ctypes.c_void_p),
                DLDevice(1, 0),
                1,
                DLDataType(0, 32, 1),
                self.shape,
                self.strides,
                0,
            ),
            None,
            self.deleter,
        )

    def __dlpack__(self, stream=None):
        ctypes.pythonapi.PyCapsule_New.restype = ctypes.py_object
        ctypes.pythonapi.PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
        return ctypes.pythonapi.PyCapsule_New(ctypes.cast(ctypes.pointer(self.managed), ctypes.c_void_p), b"dltensor", None)

payload = DLPackOwned()
`); result.Err != nil {
		t.Fatalf("create owned __dlpack__ payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-dlpack-owned", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("__dlpack__ owned capsule should export through DLPack")
	}
	if exported.Dtype != arrow.DtypeI32 || exported.Elements != 2 {
		t.Fatalf("bad owned __dlpack__ metadata: %+v", exported)
	}
	if result := r.Eval("released.value"); result.Err != nil || result.Value != "0" {
		t.Fatalf("DLPack deleter ran before buffer release: value=%v err=%v", result.Value, result.Err)
	}
	if err := arrow.GlobalStore().Free("python-export-dlpack-owned"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
	if result := r.Eval("released.value"); result.Err != nil || result.Value != "1" {
		t.Fatalf("DLPack deleter did not run exactly once after release: value=%v err=%v", result.Value, result.Err)
	}
}

func TestPythonExportBufferRejectsNonCPUDLPackDevice(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
class GPUOnlyDLPack:
    def __init__(self):
        self.called = False
    def __dlpack_device__(self):
        return (2, 0)
    def __dlpack__(self, stream=None):
        self.called = True
        raise RuntimeError("non-CPU __dlpack__ should not be called")
payload = GPUOnlyDLPack()
`); result.Err != nil {
		t.Fatalf("create non-CPU __dlpack__ payload: %v", result.Err)
	}
	if exported, ok, err := r.ExportBuffer("python-export-dlpack-gpu", "payload"); err != nil || ok {
		t.Fatalf("non-CPU __dlpack__ export = (%+v,%v,%v), want no export and no error", exported, ok, err)
	}
	if result := r.Eval("payload.called"); result.Err != nil || result.Value != "False" {
		t.Fatalf("non-CPU __dlpack__ was called: value=%v err=%v", result.Value, result.Err)
	}
}

func TestPythonExportBufferUsesSingleColumnDataFrameInterchange(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

class InterchangeBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (1, None)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.backing)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.backing)

class InterchangeColumn:
    def __init__(self, owner):
        self.owner = owner
    def size(self):
        return 3
    @property
    def offset(self):
        return 0
    @property
    def dtype(self):
        return (2, 64, "g", "<")
    def num_chunks(self):
        return 1
    def get_buffers(self):
        return {
            "data": (InterchangeBuffer(self.owner), self.dtype),
            "validity": None,
            "offsets": None,
        }

class InterchangeFrame:
    def __init__(self, owner):
        self.owner = owner
    def num_columns(self):
        return 1
    def num_rows(self):
        return 3
    def num_chunks(self):
        return 1
    def get_column(self, i):
        if i != 0:
            raise IndexError(i)
        return InterchangeColumn(self.owner)

class DataFrameInterchangeOnly:
    def __init__(self):
        self.backing = (ctypes.c_double * 3)(1.5, 2.5, 3.5)
        self.allow_copy_seen = None
    def __dataframe__(self, *, allow_copy=True):
        self.allow_copy_seen = allow_copy
        return InterchangeFrame(self)

payload = DataFrameInterchangeOnly()
`); result.Err != nil {
		t.Fatalf("create dataframe-interchange payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-dataframe-interchange", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("__dataframe__ object should export through the generic dataframe interchange protocol")
	}
	if exported.Dtype != arrow.DtypeF64 || exported.ArrowFormat != "g" || exported.Elements != 3 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || len(exported.Strides) != 1 || exported.Strides[0] != 8 || !exported.ReadOnly {
		t.Fatalf("bad exported dataframe-interchange metadata: %+v", exported)
	}
	if result := r.Eval("payload.allow_copy_seen"); result.Err != nil || result.Value != "False" {
		t.Fatalf("__dataframe__ should be asked for a no-copy export: value=%v err=%v", result.Value, result.Err)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-dataframe-interchange")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	binary.LittleEndian.PutUint64(data[8:16], math.Float64bits(9.25))
	lease.Release()

	result := r.Eval("payload.backing[1]")
	if result.Err != nil {
		t.Fatalf("eval mutated dataframe-interchange payload: %v", result.Err)
	}
	if result.Value != "9.25" {
		t.Fatalf("dataframe-interchange export was not live zero-copy memory, payload.backing[1]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-dataframe-interchange"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferUsesNullableDataFrameInterchange(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

class InterchangeDataBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (1, None)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.backing)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.backing)

class InterchangeValidityBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (1, None)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.validity)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.validity)

class InterchangeColumn:
    def __init__(self, owner):
        self.owner = owner
    def size(self):
        return 3
    @property
    def offset(self):
        return 0
    @property
    def dtype(self):
        return (2, 64, "g", "<")
    def num_chunks(self):
        return 1
    def get_buffers(self):
        return {
            "data": (InterchangeDataBuffer(self.owner), self.dtype),
            "validity": (InterchangeValidityBuffer(self.owner), (0, 8, "C", "|")),
            "offsets": None,
        }

class InterchangeFrame:
    def __init__(self, owner):
        self.owner = owner
    def num_columns(self):
        return 1
    def num_rows(self):
        return 3
    def num_chunks(self):
        return 1
    def get_column(self, i):
        if i != 0:
            raise IndexError(i)
        return InterchangeColumn(self.owner)

class NullableDataFrameInterchangeOnly:
    def __init__(self):
        self.backing = (ctypes.c_double * 3)(1.5, 2.5, 3.5)
        self.validity = (ctypes.c_uint8 * 1)(0b00000101)
        self.allow_copy_seen = None
    def __dataframe__(self, *, allow_copy=True):
        self.allow_copy_seen = allow_copy
        return InterchangeFrame(self)

payload = NullableDataFrameInterchangeOnly()
`); result.Err != nil {
		t.Fatalf("create nullable dataframe-interchange payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-nullable-dataframe-interchange", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("nullable __dataframe__ object should export through the generic dataframe interchange protocol")
	}
	if exported.Dtype != arrow.DtypeF64 || exported.ArrowFormat != "g" || exported.Elements != 3 || exported.NullCount != 1 {
		t.Fatalf("bad exported nullable dataframe-interchange metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-nullable-dataframe-interchange")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Metadata.NullCount != 1 || lease.Metadata.ValidityBytes != 1 || lease.Metadata.ValidityBitOffset != 0 || lease.Validity == nil {
		t.Fatalf("bad nullable dataframe-interchange borrow metadata: %+v validity=%v", lease.Metadata, lease.Validity)
	}
	validity := unsafe.Slice((*byte)(lease.Validity), int(lease.ValidityLen))
	if validity[0] != 0b00000101 {
		t.Fatalf("validity byte = %08b, want 00000101", validity[0])
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	binary.LittleEndian.PutUint64(data[16:24], math.Float64bits(7.75))
	lease.Release()

	result := r.Eval("payload.backing[2]")
	if result.Err != nil {
		t.Fatalf("eval mutated nullable dataframe-interchange payload: %v", result.Err)
	}
	if result.Value != "7.75" {
		t.Fatalf("nullable dataframe-interchange export was not live zero-copy memory, payload.backing[2]=%v", result.Value)
	}
	if result := r.Eval("payload.allow_copy_seen"); result.Err != nil || result.Value != "False" {
		t.Fatalf("__dataframe__ should be asked for a no-copy export: value=%v err=%v", result.Value, result.Err)
	}
	if err := arrow.GlobalStore().Free("python-export-nullable-dataframe-interchange"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferRejectsMultiColumnDataFrameInterchange(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
class MultiColumnFrame:
    def num_columns(self):
        return 2
    def num_chunks(self):
        return 1

class MultiColumnDataFrame:
    def __dataframe__(self, *, allow_copy=True):
        return MultiColumnFrame()

payload = MultiColumnDataFrame()
`); result.Err != nil {
		t.Fatalf("create multi-column dataframe-interchange payload: %v", result.Err)
	}
	if exported, ok, err := r.ExportBuffer("python-export-dataframe-multi-column", "payload"); err != nil || ok {
		t.Fatalf("multi-column dataframe export = (%+v,%v,%v), want no one-buffer export and no error", exported, ok, err)
	}
}

func TestPythonExportBufferRejectsNonCPUDataFrameInterchangeBuffer(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

class GPUInterchangeBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (2, 0)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.backing)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.backing)

class GPUInterchangeColumn:
    def __init__(self, owner):
        self.owner = owner
    def size(self):
        return 2
    @property
    def offset(self):
        return 0
    @property
    def dtype(self):
        return (0, 32, "i", "<")
    def num_chunks(self):
        return 1
    def get_buffers(self):
        return {"data": (GPUInterchangeBuffer(self.owner), self.dtype), "validity": None, "offsets": None}

class GPUInterchangeFrame:
    def __init__(self, owner):
        self.owner = owner
    def num_columns(self):
        return 1
    def num_chunks(self):
        return 1
    def get_column(self, i):
        return GPUInterchangeColumn(self.owner)

class GPUDataFrameInterchange:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 2)(11, 22)
    def __dataframe__(self, *, allow_copy=True):
        return GPUInterchangeFrame(self)

payload = GPUDataFrameInterchange()
`); result.Err != nil {
		t.Fatalf("create non-CPU dataframe-interchange payload: %v", result.Err)
	}
	if exported, ok, err := r.ExportBuffer("python-export-dataframe-gpu", "payload"); err != nil || ok {
		t.Fatalf("non-CPU dataframe export = (%+v,%v,%v), want no export and no error", exported, ok, err)
	}
}

func TestPythonExportBufferRejectsWrongEndianDataFrameInterchangeBuffer(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

class BigEndianInterchangeBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (1, None)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.backing)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.backing)

class BigEndianInterchangeColumn:
    def __init__(self, owner):
        self.owner = owner
    def size(self):
        return 2
    @property
    def offset(self):
        return 0
    @property
    def dtype(self):
        return (0, 32, "i", ">")
    def num_chunks(self):
        return 1
    def get_buffers(self):
        return {"data": (BigEndianInterchangeBuffer(self.owner), self.dtype), "validity": None, "offsets": None}

class BigEndianInterchangeFrame:
    def __init__(self, owner):
        self.owner = owner
    def num_columns(self):
        return 1
    def num_chunks(self):
        return 1
    def get_column(self, i):
        return BigEndianInterchangeColumn(self.owner)

class BigEndianDataFrameInterchange:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 2)(11, 22)
    def __dataframe__(self, *, allow_copy=True):
        return BigEndianInterchangeFrame(self)

payload = BigEndianDataFrameInterchange()
`); result.Err != nil {
		t.Fatalf("create wrong-endian dataframe-interchange payload: %v", result.Err)
	}
	if exported, ok, err := r.ExportBuffer("python-export-dataframe-wrong-endian", "payload"); err != nil || ok {
		t.Fatalf("wrong-endian dataframe export = (%+v,%v,%v), want no export and no error", exported, ok, err)
	}
}

func TestPythonExportBufferUsesArrowCapsuleProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

released_schema = ctypes.c_int(0)
released_array = ctypes.c_int(0)

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))

@ArrowSchemaRelease
def schema_release(schema):
    released_schema.value += 1

@ArrowArrayRelease
def array_release(array):
    released_array.value += 1

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object

class ArrowCapsuleArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 4)(111, 4, 5, 6)
        self.schema = ArrowSchema()
        self.array = ArrowArray()
        self.buffers = (ctypes.c_void_p * 2)()
        self.schema.format = b"i"
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.array.length = 3
        self.array.offset = 1
        self.array.null_count = 0
        self.array.n_buffers = 2
        self.array.buffers = self.buffers
        self.array.release = ctypes.cast(array_release, ctypes.c_void_p).value
        self.buffers[0] = None
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p)

    def __arrow_c_array__(self, requested_schema=None):
        return (
            PyCapsule_New(ctypes.addressof(self.schema), b"arrow_schema", None),
            PyCapsule_New(ctypes.addressof(self.array), b"arrow_array", None),
        )

payload = ArrowCapsuleArray()
`); result.Err != nil {
		t.Fatalf("create Arrow capsule payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-arrow-capsule", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("__arrow_c_array__ object should export through the generic Arrow PyCapsule protocol")
	}
	if exported.Dtype != arrow.DtypeI32 || exported.ArrowFormat != "i" || exported.Elements != 3 || exported.Offset != 4 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || !exported.ReadOnly {
		t.Fatalf("bad exported Arrow capsule metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-arrow-capsule")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 16 || lease.Metadata.Offset != 4 {
		t.Fatalf("bad borrowed Arrow capsule metadata: len=%d metadata=%+v", lease.Len, lease.Metadata)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	if got := int32(binary.LittleEndian.Uint32(data[4:8])); got != 4 {
		t.Fatalf("Arrow capsule logical first value = %d, want 4", got)
	}
	lease.Release()
	if result := r.Eval("(released_schema.value, released_array.value)"); result.Err != nil || result.Value != "(0, 0)" {
		t.Fatalf("Arrow capsule released early: value=%v err=%v", result.Value, result.Err)
	}
	if err := arrow.GlobalStore().Free("python-export-arrow-capsule"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
	if result := r.Eval("(released_schema.value, released_array.value)"); result.Err != nil || result.Value != "(1, 1)" {
		t.Fatalf("Arrow capsule descriptors not released exactly once: value=%v err=%v", result.Value, result.Err)
	}
}

func TestPythonExportBufferUsesNullableArrowCapsuleProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))

@ArrowSchemaRelease
def schema_release(schema):
    pass

@ArrowArrayRelease
def array_release(array):
    pass

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object

class NullableArrowCapsuleArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 4)(111, 4, 5, 6)
        self.validity = (ctypes.c_uint8 * 1)(0b00001011)
        self.schema = ArrowSchema()
        self.array = ArrowArray()
        self.buffers = (ctypes.c_void_p * 2)()
        self.schema.format = b"i"
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.array.length = 3
        self.array.offset = 1
        self.array.null_count = 1
        self.array.n_buffers = 2
        self.array.buffers = self.buffers
        self.array.release = ctypes.cast(array_release, ctypes.c_void_p).value
        self.buffers[0] = ctypes.cast(self.validity, ctypes.c_void_p)
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p)

    def __arrow_c_array__(self, requested_schema=None):
        return (
            PyCapsule_New(ctypes.addressof(self.schema), b"arrow_schema", None),
            PyCapsule_New(ctypes.addressof(self.array), b"arrow_array", None),
        )

payload = NullableArrowCapsuleArray()
`); result.Err != nil {
		t.Fatalf("create nullable Arrow capsule payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-nullable-arrow-capsule", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("nullable __arrow_c_array__ object should export through the generic Arrow PyCapsule protocol")
	}
	if exported.NullCount != 1 || exported.Offset != 4 {
		t.Fatalf("bad nullable Arrow capsule metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-nullable-arrow-capsule")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Metadata.NullCount != 1 || lease.Metadata.ValidityBytes != 1 || lease.Metadata.ValidityBitOffset != 1 || lease.Validity == nil {
		t.Fatalf("bad nullable borrow metadata: %+v validity=%v", lease.Metadata, lease.Validity)
	}
	validity := unsafe.Slice((*byte)(lease.Validity), int(lease.ValidityLen))
	if validity[0] != 0b00001011 {
		t.Fatalf("validity byte = %08b, want 00001011", validity[0])
	}
	lease.Release()
	if err := arrow.GlobalStore().Free("python-export-nullable-arrow-capsule"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferKeepsTemporaryArrowCapsuleOwnerAlive(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

deleted = ctypes.c_int(0)

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))

@ArrowSchemaRelease
def schema_release(schema):
    pass

@ArrowArrayRelease
def array_release(array):
    pass

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object

class TemporaryArrowCapsuleArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 2)(7, 9)
        self.schema = ArrowSchema()
        self.array = ArrowArray()
        self.buffers = (ctypes.c_void_p * 2)()
        self.schema.format = b"i"
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.array.length = 2
        self.array.null_count = 0
        self.array.n_buffers = 2
        self.array.buffers = self.buffers
        self.array.release = ctypes.cast(array_release, ctypes.c_void_p).value
        self.buffers[0] = None
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p)

    def __arrow_c_array__(self, requested_schema=None):
        return (
            PyCapsule_New(ctypes.addressof(self.schema), b"arrow_schema", None),
            PyCapsule_New(ctypes.addressof(self.array), b"arrow_array", None),
        )

    def __del__(self):
        deleted.value += 1
`); result.Err != nil {
		t.Fatalf("create temporary Arrow capsule class: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-temp-arrow-capsule", "TemporaryArrowCapsuleArray()")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("temporary __arrow_c_array__ object should export through the generic Arrow PyCapsule protocol")
	}
	if exported.Dtype != arrow.DtypeI32 || exported.Elements != 2 {
		t.Fatalf("bad temporary Arrow capsule metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-temp-arrow-capsule")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	if got := int32(binary.LittleEndian.Uint32(data[0:4])); got != 7 {
		t.Fatalf("temporary Arrow capsule first value = %d, want 7", got)
	}
	lease.Release()
	if result := r.Eval("(__import__('gc').collect(), deleted.value)[1]"); result.Err != nil || result.Value != "0" {
		t.Fatalf("temporary Arrow capsule owner released early: value=%v err=%v", result.Value, result.Err)
	}
	if err := arrow.GlobalStore().Free("python-export-temp-arrow-capsule"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
	if result := r.Eval("(__import__('gc').collect(), deleted.value)[1]"); result.Err != nil || result.Value != "1" {
		t.Fatalf("temporary Arrow capsule owner not released with buffer: value=%v err=%v", result.Value, result.Err)
	}
}

func TestPythonExportBufferRejectsNestedArrowCapsuleProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object

schema_release = ArrowSchemaRelease(lambda schema: None)
array_release = ArrowArrayRelease(lambda array: None)

class NestedArrowCapsuleArray:
    def __init__(self):
        self.kind = "nested-arrow-capsule"
        self.backing = (ctypes.c_int32 * 2)(1, 2)
        self.child_schema = ArrowSchema()
        self.child_array = ArrowArray()
        self.schema = ArrowSchema()
        self.array = ArrowArray()
        self.buffers = (ctypes.c_void_p * 2)()
        self.buffers[0] = None
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p)
        self.schema.format = b"+l"
        self.schema.n_children = 1
        self.schema.children = ctypes.cast(ctypes.pointer(ctypes.pointer(self.child_schema)), ctypes.c_void_p)
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.array.length = 2
        self.array.null_count = 0
        self.array.n_buffers = 2
        self.array.n_children = 1
        self.array.buffers = self.buffers
        self.array.children = ctypes.cast(ctypes.pointer(ctypes.pointer(self.child_array)), ctypes.c_void_p)
        self.array.release = ctypes.cast(array_release, ctypes.c_void_p).value

    def __arrow_c_array__(self, requested_schema=None):
        return (
            PyCapsule_New(ctypes.addressof(self.schema), b"arrow_schema", None),
            PyCapsule_New(ctypes.addressof(self.array), b"arrow_array", None),
        )

payload = NestedArrowCapsuleArray()
`); result.Err != nil {
		t.Fatalf("create nested Arrow capsule payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-nested-arrow-capsule", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if ok {
		t.Fatalf("nested Arrow capsule should not lower to one-buffer Arrow metadata: %+v", exported)
	}
}

func TestPythonExportBufferUsesSingleChunkArrowStreamProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

released_schema = ctypes.c_int(0)
released_array = ctypes.c_int(0)
released_stream = ctypes.c_int(0)

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

class ArrowArrayStream(ctypes.Structure):
    pass

SchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
GetSchema = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowSchema))
GetNext = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowArray))
GetLastError = ctypes.CFUNCTYPE(ctypes.c_char_p, ctypes.POINTER(ArrowArrayStream))
StreamRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArrayStream))

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArrayStream._fields_ = [
    ("get_schema", GetSchema),
    ("get_next", GetNext),
    ("get_last_error", GetLastError),
    ("release", StreamRelease),
    ("private_data", ctypes.c_void_p),
]

@SchemaRelease
def schema_release(schema):
    released_schema.value += 1

@ArrayRelease
def array_release(array):
    released_array.value += 1

class ArrowStreamArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 4)(111, 4, 5, 6)
        self.schema = ArrowSchema()
        self.schema.format = b"i"
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.buffers = (ctypes.c_void_p * 2)()
        self.buffers[0] = None
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p)
        self.calls = 0

        @GetSchema
        def get_schema(stream, out):
            out.contents.format = self.schema.format
            out.contents.name = None
            out.contents.metadata = None
            out.contents.flags = 0
            out.contents.n_children = 0
            out.contents.children = None
            out.contents.dictionary = None
            out.contents.release = self.schema.release
            out.contents.private_data = None
            return 0

        @GetNext
        def get_next(stream, out):
            if self.calls == 0:
                self.calls += 1
                out.contents.length = 3
                out.contents.null_count = 0
                out.contents.offset = 1
                out.contents.n_buffers = 2
                out.contents.n_children = 0
                out.contents.buffers = self.buffers
                out.contents.children = None
                out.contents.dictionary = None
                out.contents.release = ctypes.cast(array_release, ctypes.c_void_p).value
                out.contents.private_data = None
                return 0
            out.contents.release = None
            return 0

        @GetLastError
        def get_last_error(stream):
            return None

        @StreamRelease
        def stream_release(stream):
            released_stream.value += 1

        self.get_schema = get_schema
        self.get_next = get_next
        self.get_last_error = get_last_error
        self.stream_release = stream_release
        self.stream = ArrowArrayStream(get_schema, get_next, get_last_error, stream_release, None)

    def __arrow_c_stream__(self, requested_schema=None):
        ctypes.pythonapi.PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
        ctypes.pythonapi.PyCapsule_New.restype = ctypes.py_object
        return ctypes.pythonapi.PyCapsule_New(ctypes.addressof(self.stream), b"arrow_array_stream", None)

payload = ArrowStreamArray()
`); result.Err != nil {
		t.Fatalf("create Arrow stream payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-arrow-stream", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("__arrow_c_stream__ object should export through the generic Arrow stream PyCapsule protocol")
	}
	if exported.Dtype != arrow.DtypeI32 || exported.ArrowFormat != "i" || exported.Elements != 3 || exported.Offset != 4 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || !exported.ReadOnly {
		t.Fatalf("bad exported Arrow stream metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-arrow-stream")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	if got := int32(binary.LittleEndian.Uint32(data[4:8])); got != 4 {
		t.Fatalf("Arrow stream logical first value = %d, want 4", got)
	}
	lease.Release()
	if result := r.Eval("(released_schema.value, released_array.value, released_stream.value)"); result.Err != nil || result.Value != "(0, 0, 0)" {
		t.Fatalf("Arrow stream released early: value=%v err=%v", result.Value, result.Err)
	}
	if err := arrow.GlobalStore().Free("python-export-arrow-stream"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
	if result := r.Eval("(released_schema.value, released_array.value, released_stream.value)"); result.Err != nil || result.Value != "(1, 1, 1)" {
		t.Fatalf("Arrow stream descriptors not released exactly once: value=%v err=%v", result.Value, result.Err)
	}
}

func TestPythonExportBufferUsesNullableSingleChunkArrowStreamProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

class ArrowArrayStream(ctypes.Structure):
    pass

SchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
GetSchema = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowSchema))
GetNext = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowArray))
GetLastError = ctypes.CFUNCTYPE(ctypes.c_char_p, ctypes.POINTER(ArrowArrayStream))
StreamRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArrayStream))

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArrayStream._fields_ = [
    ("get_schema", GetSchema),
    ("get_next", GetNext),
    ("get_last_error", GetLastError),
    ("release", StreamRelease),
    ("private_data", ctypes.c_void_p),
]

@SchemaRelease
def schema_release(schema):
    pass

@ArrayRelease
def array_release(array):
    pass

class NullableArrowStreamArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 4)(111, 4, 5, 6)
        self.validity = (ctypes.c_uint8 * 1)(0b00001011)
        self.schema = ArrowSchema()
        self.schema.format = b"i"
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.buffers = (ctypes.c_void_p * 2)()
        self.buffers[0] = ctypes.cast(self.validity, ctypes.c_void_p)
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p)
        self.calls = 0

        @GetSchema
        def get_schema(stream, out):
            out.contents.format = self.schema.format
            out.contents.name = None
            out.contents.metadata = None
            out.contents.flags = 0
            out.contents.n_children = 0
            out.contents.children = None
            out.contents.dictionary = None
            out.contents.release = self.schema.release
            out.contents.private_data = None
            return 0

        @GetNext
        def get_next(stream, out):
            if self.calls == 0:
                self.calls += 1
                out.contents.length = 3
                out.contents.null_count = 1
                out.contents.offset = 1
                out.contents.n_buffers = 2
                out.contents.n_children = 0
                out.contents.buffers = self.buffers
                out.contents.children = None
                out.contents.dictionary = None
                out.contents.release = ctypes.cast(array_release, ctypes.c_void_p).value
                out.contents.private_data = None
                return 0
            out.contents.release = None
            return 0

        @GetLastError
        def get_last_error(stream):
            return None

        @StreamRelease
        def stream_release(stream):
            pass

        self.get_schema = get_schema
        self.get_next = get_next
        self.get_last_error = get_last_error
        self.stream_release = stream_release
        self.stream = ArrowArrayStream(get_schema, get_next, get_last_error, stream_release, None)

    def __arrow_c_stream__(self, requested_schema=None):
        ctypes.pythonapi.PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
        ctypes.pythonapi.PyCapsule_New.restype = ctypes.py_object
        return ctypes.pythonapi.PyCapsule_New(ctypes.addressof(self.stream), b"arrow_array_stream", None)

payload = NullableArrowStreamArray()
`); result.Err != nil {
		t.Fatalf("create nullable Arrow stream payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-nullable-arrow-stream", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("nullable __arrow_c_stream__ object should export through the generic Arrow stream PyCapsule protocol")
	}
	if exported.NullCount != 1 || exported.Offset != 4 {
		t.Fatalf("bad nullable Arrow stream metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-nullable-arrow-stream")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Metadata.NullCount != 1 || lease.Metadata.ValidityBytes != 1 || lease.Metadata.ValidityBitOffset != 1 || lease.Validity == nil {
		t.Fatalf("bad nullable stream borrow metadata: %+v validity=%v", lease.Metadata, lease.Validity)
	}
	lease.Release()
	if err := arrow.GlobalStore().Free("python-export-nullable-arrow-stream"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferRejectsMultiChunkArrowStreamProtocol(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes

released_schema = ctypes.c_int(0)
released_array = ctypes.c_int(0)
released_stream = ctypes.c_int(0)

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

class ArrowArrayStream(ctypes.Structure):
    pass

SchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
GetSchema = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowSchema))
GetNext = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowArray))
GetLastError = ctypes.CFUNCTYPE(ctypes.c_char_p, ctypes.POINTER(ArrowArrayStream))
StreamRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArrayStream))

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p),
    ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]

ArrowArrayStream._fields_ = [
    ("get_schema", GetSchema),
    ("get_next", GetNext),
    ("get_last_error", GetLastError),
    ("release", StreamRelease),
    ("private_data", ctypes.c_void_p),
]

@SchemaRelease
def schema_release(schema):
    released_schema.value += 1

@ArrayRelease
def array_release(array):
    released_array.value += 1

class MultiChunkArrowStream:
    def __init__(self):
        self.first = (ctypes.c_int32 * 2)(1, 2)
        self.second = (ctypes.c_int32 * 1)(3)
        self.first_buffers = (ctypes.c_void_p * 2)()
        self.first_buffers[0] = None
        self.first_buffers[1] = ctypes.cast(self.first, ctypes.c_void_p)
        self.second_buffers = (ctypes.c_void_p * 2)()
        self.second_buffers[0] = None
        self.second_buffers[1] = ctypes.cast(self.second, ctypes.c_void_p)
        self.calls = 0

        @GetSchema
        def get_schema(stream, out):
            out.contents.format = b"i"
            out.contents.name = None
            out.contents.metadata = None
            out.contents.flags = 0
            out.contents.n_children = 0
            out.contents.children = None
            out.contents.dictionary = None
            out.contents.release = ctypes.cast(schema_release, ctypes.c_void_p).value
            out.contents.private_data = None
            return 0

        @GetNext
        def get_next(stream, out):
            self.calls += 1
            if self.calls == 1:
                out.contents.length = 2
                out.contents.buffers = self.first_buffers
            elif self.calls == 2:
                out.contents.length = 1
                out.contents.buffers = self.second_buffers
            else:
                out.contents.release = None
                return 0
            out.contents.null_count = 0
            out.contents.offset = 0
            out.contents.n_buffers = 2
            out.contents.n_children = 0
            out.contents.children = None
            out.contents.dictionary = None
            out.contents.release = ctypes.cast(array_release, ctypes.c_void_p).value
            out.contents.private_data = None
            return 0

        @GetLastError
        def get_last_error(stream):
            return None

        @StreamRelease
        def stream_release(stream):
            released_stream.value += 1

        self.get_schema = get_schema
        self.get_next = get_next
        self.get_last_error = get_last_error
        self.stream_release = stream_release
        self.stream = ArrowArrayStream(get_schema, get_next, get_last_error, stream_release, None)

    def __arrow_c_stream__(self, requested_schema=None):
        ctypes.pythonapi.PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
        ctypes.pythonapi.PyCapsule_New.restype = ctypes.py_object
        return ctypes.pythonapi.PyCapsule_New(ctypes.addressof(self.stream), b"arrow_array_stream", None)

payload = MultiChunkArrowStream()
`); result.Err != nil {
		t.Fatalf("create multi-chunk Arrow stream payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-arrow-stream-multichunk", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if ok {
		t.Fatalf("multi-chunk Arrow stream should not lower to one-buffer Arrow metadata: %+v", exported)
	}
	if result := r.Eval("(released_schema.value, released_array.value, released_stream.value)"); result.Err != nil || result.Value != "(1, 2, 1)" {
		t.Fatalf("multi-chunk Arrow stream descriptors not released exactly once: value=%v err=%v", result.Value, result.Err)
	}
}

func TestPythonExportBufferUsesStridedArrayInterface(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes
class StridedArrayInterfaceOnly:
    def __init__(self):
        self.backing = (ctypes.c_uint16 * 5)(258, 999, 772, 999, 1286)
    @property
    def __array_interface__(self):
        return {
            "data": (ctypes.addressof(self.backing), False),
            "shape": (3,),
            "strides": (4,),
            "typestr": "<u2",
            "version": 3,
        }
payload = StridedArrayInterfaceOnly()
`); result.Err != nil {
		t.Fatalf("create strided array-interface payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-strided-array-interface", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("strided __array_interface__ object should export through the generic array protocol")
	}
	if exported.Dtype != arrow.DtypeU16 || exported.ArrowFormat != "S" || exported.Elements != 3 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || len(exported.Strides) != 1 || exported.Strides[0] != 4 {
		t.Fatalf("bad exported strided array-interface metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-strided-array-interface")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 10 || len(lease.Metadata.Strides) != 1 || lease.Metadata.Strides[0] != 4 {
		t.Fatalf("bad borrowed strided metadata: len=%d metadata=%+v", lease.Len, lease.Metadata)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	binary.LittleEndian.PutUint16(data[4:6], 2049)
	lease.Release()

	result := r.Eval("payload.backing[2]")
	if result.Err != nil {
		t.Fatalf("eval mutated strided array-interface payload: %v", result.Err)
	}
	if result.Value != "2049" {
		t.Fatalf("strided array-interface export was not live zero-copy memory, payload.backing[2]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-strided-array-interface"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}

func TestPythonExportBufferUsesNegativeStridedArrayInterface(t *testing.T) {
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	defer r.Shutdown()

	if result := r.Execute(`
import ctypes
class NegativeStridedArrayInterfaceOnly:
    def __init__(self):
        self.backing = (ctypes.c_uint16 * 5)(258, 999, 772, 999, 1286)
    @property
    def __array_interface__(self):
        return {
            "data": (ctypes.addressof(self.backing) + 8, False),
            "shape": (3,),
            "strides": (-4,),
            "typestr": "<u2",
            "version": 3,
        }
payload = NegativeStridedArrayInterfaceOnly()
`); result.Err != nil {
		t.Fatalf("create negative-strided array-interface payload: %v", result.Err)
	}
	exported, ok, err := r.ExportBuffer("python-export-negative-strided-array-interface", "payload")
	if err != nil {
		t.Fatalf("ExportBuffer failed: %v", err)
	}
	if !ok {
		t.Fatal("negative-strided __array_interface__ object should export through the generic array protocol")
	}
	if exported.Dtype != arrow.DtypeU16 || exported.ArrowFormat != "S" || exported.Elements != 3 || exported.Offset != 8 || len(exported.Shape) != 1 || exported.Shape[0] != 3 || len(exported.Strides) != 1 || exported.Strides[0] != -4 {
		t.Fatalf("bad exported negative-strided array-interface metadata: %+v", exported)
	}
	lease, err := arrow.GlobalStore().Borrow("python-export-negative-strided-array-interface")
	if err != nil {
		t.Fatalf("Borrow exported buffer: %v", err)
	}
	if lease.Len != 10 || lease.Metadata.Offset != 8 || len(lease.Metadata.Strides) != 1 || lease.Metadata.Strides[0] != -4 {
		t.Fatalf("bad borrowed negative-strided metadata: len=%d metadata=%+v", lease.Len, lease.Metadata)
	}
	data := unsafe.Slice((*byte)(lease.Data), int(lease.Len))
	binary.LittleEndian.PutUint16(data[4:6], 2049)
	lease.Release()

	result := r.Eval("payload.backing[2]")
	if result.Err != nil {
		t.Fatalf("eval mutated negative-strided array-interface payload: %v", result.Err)
	}
	if result.Value != "2049" {
		t.Fatalf("negative-strided array-interface export was not live zero-copy memory, payload.backing[2]=%v", result.Value)
	}
	if err := arrow.GlobalStore().Free("python-export-negative-strided-array-interface"); err != nil {
		t.Fatalf("Free exported buffer: %v", err)
	}
}
