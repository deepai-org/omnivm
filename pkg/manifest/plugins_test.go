package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/handles"
)

func TestGoToolPathFallsBackToGOROOT(t *testing.T) {
	goroot := runtime.GOROOT()
	if goroot == "" {
		t.Skip("GOROOT unavailable")
	}
	if _, err := os.Stat(filepath.Join(goroot, "bin", "go")); err != nil {
		t.Skip("GOROOT go tool unavailable")
	}
	t.Setenv("PATH", "")
	path, err := goToolPath()
	if err != nil {
		t.Fatalf("goToolPath with empty PATH: %v", err)
	}
	if path == "" {
		t.Fatal("goToolPath returned empty path")
	}
}

func TestGoCSharedBorrowedTableArgCarriesMemoryOwnershipMetadata(t *testing.T) {
	e := NewExecutor(nil)
	ref, ok, err := e.autoBulkTableRefForCapture([2][2]float32{{1, 2}, {3, 4}})
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}

	payloadValue, lease, err := e.encodeCSharedGoTableArg(ref)
	if err != nil {
		t.Fatalf("encodeCSharedGoTableArg: %v", err)
	}
	if lease == nil {
		t.Fatal("encodeCSharedGoTableArg did not retain a borrowed buffer lease")
	}
	defer lease.release()
	t.Cleanup(func() {
		if err := e.releaseAllHandleScopes(); err != nil {
			t.Fatalf("releaseAllHandleScopes: %v", err)
		}
	})

	payload, ok := payloadValue.(map[string]interface{})
	if !ok {
		t.Fatalf("payload = %T, want map", payloadValue)
	}
	if payload["memory_space"] != "host" || payload["ownership"] != "producer" || payload["read_only"] != true {
		t.Fatalf("borrowed c-shared payload memory ownership metadata = %#v", payload)
	}
	if payload["boundary"] != "borrowed_buffer" || payload["dtype"] != "f32" || payload["format"] != "f" {
		t.Fatalf("borrowed c-shared payload core metadata = %#v", payload)
	}
}

func TestGoCSharedOwnedBufferRejectsNonHostMemorySpace(t *testing.T) {
	_, err := decodeCSharedOwnedBuffer(0, cSharedPluginEnvelope{
		OK:          true,
		Boundary:    "owned_buffer",
		Dtype:       "u8",
		Format:      "C",
		MemorySpace: "cuda",
		BytesLen:    0,
		Elements:    0,
	})
	if err == nil {
		t.Fatal("decodeCSharedOwnedBuffer accepted non-host memory_space")
	}
	if !strings.Contains(err.Error(), `memory_space "cuda" is not host-accessible`) {
		t.Fatalf("decodeCSharedOwnedBuffer non-host memory error = %v", err)
	}
}

func TestGoCSharedOwnedBufferCarriesHostMemorySpace(t *testing.T) {
	buf, err := decodeCSharedOwnedBuffer(0, cSharedPluginEnvelope{
		OK:          true,
		Boundary:    "owned_buffer",
		Dtype:       "u8",
		Format:      "C",
		MemorySpace: "host",
		BytesLen:    0,
		Elements:    0,
	})
	if err != nil {
		t.Fatalf("decodeCSharedOwnedBuffer: %v", err)
	}

	e := NewExecutor(nil)
	ref, ok, err := e.autoBulkTableRefForCapture(buf)
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}
	t.Cleanup(func() {
		if err := e.releaseAllHandleScopes(); err != nil {
			t.Fatalf("releaseAllHandleScopes: %v", err)
		}
	})

	if ref.Metadata == nil || ref.Metadata.MemorySpace != "host" {
		t.Fatalf("owned c-shared table memory_space = %#v, want host", ref.Metadata)
	}
	status := arrow.GlobalStore().Status(ref.Metadata.Buffer)
	if status.MemorySpace != "host" {
		t.Fatalf("owned c-shared store memory_space = %q, want host", status.MemorySpace)
	}
}

func TestGoCSharedSourceFallbackCompilesGenericFunction(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "blend_score",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "a"}, {Name: "b"}},
		Exports:     []string{"BlendScore"},
		Source: `package polyfunc

func BlendScore(a interface{}, b interface{}) interface{} {
	return a.(int)*10 + b.(int)
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile c-shared Go func_def: %v", err)
	}

	got, err := e.callGoFunc("blend_score", []interface{}{float64(6), float64(7)}, "")
	if err != nil {
		t.Fatalf("call c-shared Go func_def: %v", err)
	}
	if got != 67 {
		t.Fatalf("blend_score = %#v, want 67", got)
	}
}

func TestGoCSharedSourceFallbackSupportsTypedParams(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "typed_product",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "a"}, {Name: "b"}},
		Exports:     []string{"TypedProduct"},
		Source: `package polyfunc

func TypedProduct(a int, b int) int {
	return a * b
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile typed c-shared Go func_def: %v", err)
	}

	got, err := e.callGoFunc("typed_product", []interface{}{float64(6), float64(7)}, "")
	if err != nil {
		t.Fatalf("call typed c-shared Go func_def: %v", err)
	}
	if got != 42 {
		t.Fatalf("typed_product = %#v, want 42", got)
	}
}

func TestGoCSharedSourceFallbackReturnsTypedSliceAsArrowTable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "scores",
		BodyRuntime: "go",
		Exports:     []string{"Scores"},
		Source: `package polyfunc

func Scores() []int32 {
	return []int32{4, 5, 6}
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile typed-slice c-shared Go func_def: %v", err)
	}

	beforeArrow := arrow.GlobalStore().Stats()
	result, err := e.HandleCall(`{"func":"scores","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall scores: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_table__"] != true || descriptor["format"] != "arrow_c_data" {
		t.Fatalf("c-shared typed slice result = %#v, want Arrow table descriptor", env)
	}
	metadata, ok := descriptor["metadata"].(map[string]interface{})
	if !ok || metadata["dtype"] != float64(arrow.DtypeI32) || metadata["arrow_format"] != "i" {
		t.Fatalf("c-shared typed slice metadata = %#v, want int32 Arrow metadata", descriptor["metadata"])
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("table id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared typed slice bridge stats = %+v, want Arrow table without JSON fallback", stats)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyImports <= beforeArrow.ZeroCopyImports {
		t.Fatalf("c-shared typed slice did not enter Arrow as an owned zero-copy import: before=%+v after=%+v", beforeArrow, afterArrow)
	}
}

func TestGoCSharedSourceFallbackReturnsNestedArrayAsShapedArrowTable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "matrix",
		BodyRuntime: "go",
		Exports:     []string{"Matrix"},
		Source: `package polyfunc

func Matrix() [2][3]float64 {
	return [2][3]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}}
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile nested-array c-shared Go func_def: %v", err)
	}

	beforeArrow := arrow.GlobalStore().Stats()
	result, err := e.HandleCall(`{"func":"matrix","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall matrix: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_table__"] != true || descriptor["format"] != "arrow_c_data" {
		t.Fatalf("c-shared nested array result = %#v, want Arrow table descriptor", env)
	}
	metadata, ok := descriptor["metadata"].(map[string]interface{})
	shape, shapeOK := metadata["shape"].([]interface{})
	strides, stridesOK := metadata["strides"].([]interface{})
	if !ok || metadata["dtype"] != float64(arrow.DtypeF64) || metadata["arrow_format"] != "g" ||
		!shapeOK || len(shape) != 2 || shape[0] != float64(2) || shape[1] != float64(3) ||
		!stridesOK || len(strides) != 2 || strides[0] != float64(24) || strides[1] != float64(8) {
		t.Fatalf("c-shared nested array metadata = %#v, want shaped float64 Arrow metadata", descriptor["metadata"])
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("table id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	rowResult, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall row: %v", err)
	}
	rowEnv := decodeResultEnvelopeForTest(t, rowResult)
	row, ok := rowEnv.Value.([]interface{})
	if rowEnv.Kind != "json" || !ok || len(row) != 3 || row[0] != 4.5 || row[2] != 6.5 {
		t.Fatalf("c-shared nested array row = %#v, want second row", rowEnv)
	}

	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared nested array bridge stats = %+v, want shaped Arrow table without proxy/JSON fallback", stats)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyImports <= beforeArrow.ZeroCopyImports {
		t.Fatalf("c-shared nested array did not enter Arrow as an owned zero-copy import: before=%+v after=%+v", beforeArrow, afterArrow)
	}
}

func TestGoCSharedSourceFallbackReturnsNestedSliceAsShapedArrowTable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "matrix",
		BodyRuntime: "go",
		Exports:     []string{"Matrix"},
		Source: `package polyfunc

func Matrix() [][]float64 {
	return [][]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}}
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile nested-slice c-shared Go func_def: %v", err)
	}

	beforeArrow := arrow.GlobalStore().Stats()
	result, err := e.HandleCall(`{"func":"matrix","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall matrix: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_table__"] != true || descriptor["format"] != "arrow_c_data" {
		t.Fatalf("c-shared nested slice result = %#v, want Arrow table descriptor", env)
	}
	metadata, ok := descriptor["metadata"].(map[string]interface{})
	shape, shapeOK := metadata["shape"].([]interface{})
	strides, stridesOK := metadata["strides"].([]interface{})
	if !ok || metadata["dtype"] != float64(arrow.DtypeF64) || metadata["arrow_format"] != "g" ||
		!shapeOK || len(shape) != 2 || shape[0] != float64(2) || shape[1] != float64(3) ||
		!stridesOK || len(strides) != 2 || strides[0] != float64(24) || strides[1] != float64(8) {
		t.Fatalf("c-shared nested slice metadata = %#v, want shaped float64 Arrow metadata", descriptor["metadata"])
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("table id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	rowResult, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall row: %v", err)
	}
	rowEnv := decodeResultEnvelopeForTest(t, rowResult)
	row, ok := rowEnv.Value.([]interface{})
	if rowEnv.Kind != "json" || !ok || len(row) != 3 || row[0] != 4.5 || row[2] != 6.5 {
		t.Fatalf("c-shared nested slice row = %#v, want second row", rowEnv)
	}

	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared nested slice bridge stats = %+v, want shaped Arrow table without proxy/JSON fallback", stats)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyImports <= beforeArrow.ZeroCopyImports {
		t.Fatalf("c-shared nested slice did not enter Arrow as an owned zero-copy import: before=%+v after=%+v", beforeArrow, afterArrow)
	}
}

func TestGoCSharedSourceFallbackReturnsByteSliceAsArrowTable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "payload",
		BodyRuntime: "go",
		Exports:     []string{"Payload"},
		Source: `package polyfunc

func Payload() []byte {
	return []byte{2, 3, 5}
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile byte-slice c-shared Go func_def: %v", err)
	}

	beforeArrow := arrow.GlobalStore().Stats()
	result, err := e.HandleCall(`{"func":"payload","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall payload: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_table__"] != true || descriptor["format"] != "arrow_c_data" {
		t.Fatalf("c-shared byte slice result = %#v, want Arrow table descriptor", env)
	}
	metadata, ok := descriptor["metadata"].(map[string]interface{})
	if !ok || metadata["dtype"] != float64(arrow.DtypeU8) || metadata["arrow_format"] != "C" {
		t.Fatalf("c-shared byte slice metadata = %#v, want uint8 Arrow metadata", descriptor["metadata"])
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("table id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared byte slice bridge stats = %+v, want Arrow table without JSON fallback", stats)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyImports <= beforeArrow.ZeroCopyImports {
		t.Fatalf("c-shared byte slice did not enter Arrow as an owned zero-copy import: before=%+v after=%+v", beforeArrow, afterArrow)
	}
}

func TestGoCSharedSourceFallbackReturnsComplexObjectAsIdentityProxy(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "request",
		BodyRuntime: "go",
		Exports:     []string{"Request"},
		Source: `package polyfunc

var store = map[string]interface{}{
	"path": "/go-cshared-return",
	"items": []interface{}{"first", "second"},
}

func Request() map[string]interface{} {
	return store
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile complex c-shared Go func_def: %v", err)
	}

	result, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_resource__"] != true || descriptor["runtime"] != "go" || descriptor["kind"] != "map" {
		t.Fatalf("c-shared complex result = %#v, want Go resource proxy descriptor", env)
	}
	if strings.Contains(result, `"/go-cshared-return"`) || strings.Contains(result, `"first"`) {
		t.Fatalf("c-shared complex object should not be JSON-copied into descriptor: %s", result)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	path, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"path"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get path: %v", err)
	}
	pathEnv := decodeResultEnvelopeForTest(t, path)
	if pathEnv.Kind != "string" || pathEnv.Value != "/go-cshared-return" {
		t.Fatalf("c-shared complex path = %#v, want /go-cshared-return", pathEnv)
	}

	items, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"items"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get items: %v", err)
	}
	itemsEnv := decodeResultEnvelopeForTest(t, items)
	itemsDescriptor, ok := itemsEnv.Value.(map[string]interface{})
	if itemsEnv.Kind != "json" || !ok || itemsDescriptor["__omnivm_resource__"] != true || itemsDescriptor["kind"] != "sequence" {
		t.Fatalf("c-shared nested items = %#v, want sequence proxy descriptor", itemsEnv)
	}
	itemsID, err := bridgeHandleID(itemsDescriptor["id"])
	if err != nil {
		t.Fatalf("items id: %v", err)
	}
	defer e.ensureHandleTable().Release(itemsID)

	if _, err := e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"key":"0","value":"changed"}`); err != nil {
		t.Fatalf("HandleCall handle_set items: %v", err)
	}
	indexed, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"value":0}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index items: %v", err)
	}
	indexEnv := decodeResultEnvelopeForTest(t, indexed)
	if indexEnv.Kind != "string" || indexEnv.Value != "changed" {
		t.Fatalf("c-shared nested mutation = %#v, want changed", indexEnv)
	}

	again, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request again: %v", err)
	}
	againEnv := decodeResultEnvelopeForTest(t, again)
	againDescriptor, ok := againEnv.Value.(map[string]interface{})
	if !ok {
		t.Fatalf("second c-shared complex result = %#v, want descriptor", againEnv)
	}
	againID, err := bridgeHandleID(againDescriptor["id"])
	if err != nil {
		t.Fatalf("second resource id: %v", err)
	}
	if againID != id {
		t.Fatalf("c-shared Go complex identity cache returned handle %d, want existing handle %d", againID, id)
	}

	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures < 2 || stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared complex bridge stats = %+v, want resource proxy without JSON fallback", stats)
	}
}

func TestGoCSharedSourceFallbackStructMembersAsGenericProxy(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "request",
		BodyRuntime: "go",
		Exports:     []string{"Request"},
		Source: `package polyfunc

type RequestData struct {
	Path string ` + "`json:\"path\"`" + `
	Name string ` + "`json:\"name\"`" + `
	Count int ` + "`json:\"count\"`" + `
	Label string ` + "`json:\"label\"`" + `
}

func (r *RequestData) Join(suffix string) string {
	return r.Path + ":" + r.Name + ":" + suffix
}

var store = &RequestData{Path: "/go-cshared-struct", Name: "initial", Count: 2, Label: "alpha"}

func Request() *RequestData {
	return store
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile struct c-shared Go func_def: %v", err)
	}

	result, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_resource__"] != true || descriptor["runtime"] != "go" || descriptor["kind"] != "object" {
		t.Fatalf("c-shared struct result = %#v, want Go object proxy descriptor", env)
	}
	if strings.Contains(result, `"/go-cshared-struct"`) || strings.Contains(result, `"initial"`) {
		t.Fatalf("c-shared struct should not be JSON-copied into descriptor: %s", result)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	for key, want := range map[string]interface{}{
		"path":  "/go-cshared-struct",
		"name":  "initial",
		"count": float64(2),
		"label": "alpha",
	} {
		got, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"` + key + `"}`)
		if err != nil {
			t.Fatalf("HandleCall handle_get %s: %v", key, err)
		}
		gotEnv := decodeResultEnvelopeForTest(t, got)
		if gotEnv.Value != want {
			t.Fatalf("c-shared struct %s = %#v, want %#v", key, gotEnv, want)
		}
	}

	contains, err := e.HandleCall(`{"op":"handle_contains","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":"label"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_contains label: %v", err)
	}
	containsEnv := decodeResultEnvelopeForTest(t, contains)
	if containsEnv.Kind != "bool" || containsEnv.Value != true {
		t.Fatalf("c-shared struct contains label = %#v, want true", containsEnv)
	}

	if _, err := e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"name","value":"changed"}`); err != nil {
		t.Fatalf("HandleCall handle_set name: %v", err)
	}
	callJSON, err := json.Marshal(map[string]interface{}{
		"op":   "handle_call",
		"id":   uint64(id),
		"key":  "Join",
		"args": []interface{}{"tail"},
	})
	if err != nil {
		t.Fatalf("marshal handle_call Join: %v", err)
	}
	joined, err := e.HandleCall(string(callJSON))
	if err != nil {
		t.Fatalf("HandleCall handle_call Join: %v", err)
	}
	joinedEnv := decodeResultEnvelopeForTest(t, joined)
	if joinedEnv.Kind != "string" || joinedEnv.Value != "/go-cshared-struct:changed:tail" {
		t.Fatalf("c-shared struct Join = %#v, want changed join", joinedEnv)
	}

	again, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request again: %v", err)
	}
	againEnv := decodeResultEnvelopeForTest(t, again)
	againDescriptor, ok := againEnv.Value.(map[string]interface{})
	if !ok {
		t.Fatalf("second c-shared struct result = %#v, want descriptor", againEnv)
	}
	againID, err := bridgeHandleID(againDescriptor["id"])
	if err != nil {
		t.Fatalf("second resource id: %v", err)
	}
	if againID != id {
		t.Fatalf("c-shared Go struct identity cache returned handle %d, want existing handle %d", againID, id)
	}

	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures < 2 || stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared struct bridge stats = %+v, want resource proxy without JSON fallback", stats)
	}
}

func TestGoCSharedObjectReleaseDropsPluginHandle(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "request",
		BodyRuntime: "go",
		Exports:     []string{"Request"},
		Source: `package polyfunc

var store = map[string]interface{}{
	"path": "/go-cshared-release",
}

func Request() map[string]interface{} {
	return store
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile release c-shared Go func_def: %v", err)
	}

	result, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("c-shared release result = %#v, want resource descriptor", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	entry, live := e.ensureHandleTable().Get(id)
	if !live {
		t.Fatalf("resource handle %d is not live", id)
	}
	ref, ok := entry.Value.(*ResourceRef)
	if !ok {
		t.Fatalf("resource handle value = %T, want *ResourceRef", entry.Value)
	}
	proxy, ok := ref.Value.(*cSharedObjectProxy)
	if !ok {
		t.Fatalf("resource value = %T, want *cSharedObjectProxy", ref.Value)
	}
	if value, found, err := proxy.Get("path"); err != nil || !found || value != "/go-cshared-release" {
		t.Fatalf("pre-release proxy.Get path = (%#v, %v, %v), want live value", value, found, err)
	}

	if err := e.ensureHandleTable().Release(id); err != nil {
		t.Fatalf("release c-shared resource handle: %v", err)
	}
	if err := proxy.Release(); err != nil {
		t.Fatalf("second c-shared proxy release should be idempotent: %v", err)
	}
	if _, _, err := proxy.Get("path"); err == nil {
		t.Fatal("old c-shared proxy handle remained usable after host release")
	}

	again, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request again: %v", err)
	}
	againEnv := decodeResultEnvelopeForTest(t, again)
	againDescriptor, ok := againEnv.Value.(map[string]interface{})
	if againEnv.Kind != "json" || !ok || againDescriptor["__omnivm_resource__"] != true {
		t.Fatalf("second c-shared release result = %#v, want resource descriptor", againEnv)
	}
	againID, err := bridgeHandleID(againDescriptor["id"])
	if err != nil {
		t.Fatalf("second resource id: %v", err)
	}
	defer e.ensureHandleTable().Release(againID)
	if againID == id {
		t.Fatalf("reacquired c-shared object reused released host handle %d", id)
	}
	path, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(againID), 10) + `,"key":"path"}`)
	if err != nil {
		t.Fatalf("HandleCall reacquired path: %v", err)
	}
	pathEnv := decodeResultEnvelopeForTest(t, path)
	if pathEnv.Kind != "string" || pathEnv.Value != "/go-cshared-release" {
		t.Fatalf("reacquired c-shared object path = %#v, want live value", pathEnv)
	}
}

func TestGoCSharedObjectShutdownCleanupDropsPluginHandle(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "request",
		BodyRuntime: "go",
		Exports:     []string{"Request"},
		Source: `package polyfunc

var store = map[string]interface{}{
	"path": "/go-cshared-shutdown-release",
}

func Request() map[string]interface{} {
	return store
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile shutdown release c-shared Go func_def: %v", err)
	}

	result, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("c-shared shutdown release result = %#v, want resource descriptor", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	entry, live := e.ensureHandleTable().Get(id)
	if !live {
		t.Fatalf("resource handle %d is not live", id)
	}
	ref, ok := entry.Value.(*ResourceRef)
	if !ok {
		t.Fatalf("resource handle value = %T, want *ResourceRef", entry.Value)
	}
	proxy, ok := ref.Value.(*cSharedObjectProxy)
	if !ok {
		t.Fatalf("resource value = %T, want *cSharedObjectProxy", ref.Value)
	}
	if value, found, err := proxy.Get("path"); err != nil || !found || value != "/go-cshared-shutdown-release" {
		t.Fatalf("pre-shutdown-release proxy.Get path = (%#v, %v, %v), want live value", value, found, err)
	}

	statsBefore := e.ensureHandleTable().Stats(time.Now())
	if err := e.ensureHandleTable().ReleaseAll(); err != nil {
		t.Fatalf("release all c-shared resource handles: %v", err)
	}
	statsAfter := e.ensureHandleTable().Stats(time.Now())
	if statsAfter.ScopeReleases <= statsBefore.ScopeReleases {
		t.Fatalf("scope release count = %d before, %d after; want increment", statsBefore.ScopeReleases, statsAfter.ScopeReleases)
	}
	if statsAfter.Live >= statsBefore.Live {
		t.Fatalf("live handles = %d before, %d after; want shutdown cleanup to reduce live handles", statsBefore.Live, statsAfter.Live)
	}
	if _, live := e.ensureHandleTable().Get(id); live {
		t.Fatalf("resource handle %d remained live after shutdown cleanup", id)
	}
	if _, _, err := proxy.Get("path"); err == nil {
		t.Fatal("old c-shared proxy handle remained usable after shutdown cleanup")
	}

	again, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request again: %v", err)
	}
	againEnv := decodeResultEnvelopeForTest(t, again)
	againDescriptor, ok := againEnv.Value.(map[string]interface{})
	if againEnv.Kind != "json" || !ok || againDescriptor["__omnivm_resource__"] != true {
		t.Fatalf("second c-shared shutdown release result = %#v, want resource descriptor", againEnv)
	}
	againID, err := bridgeHandleID(againDescriptor["id"])
	if err != nil {
		t.Fatalf("second resource id: %v", err)
	}
	defer e.ensureHandleTable().Release(againID)
	if againID == id {
		t.Fatalf("reacquired c-shared object reused shutdown-released host handle %d", id)
	}
	path, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(againID), 10) + `,"key":"path"}`)
	if err != nil {
		t.Fatalf("HandleCall reacquired path: %v", err)
	}
	pathEnv := decodeResultEnvelopeForTest(t, path)
	if pathEnv.Kind != "string" || pathEnv.Value != "/go-cshared-shutdown-release" {
		t.Fatalf("reacquired c-shared object path = %#v, want live value", pathEnv)
	}
}

func TestGoCSharedObjectIterPreservesOwnedBufferShape(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "request",
		BodyRuntime: "go",
		Exports:     []string{"Request"},
		Source: `package polyfunc

var store = map[string]interface{}{
	"matrix": [2][3]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}},
}

func Request() map[string]interface{} {
	return store
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile c-shared shaped iter func_def: %v", err)
	}

	result, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("c-shared request result = %#v, want resource descriptor", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	values, err := e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(id), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter values: %v", err)
	}
	valuesEnv := decodeResultEnvelopeForTest(t, values)
	items, ok := valuesEnv.Value.([]interface{})
	if valuesEnv.Kind != "json" || !ok || len(items) != 1 {
		t.Fatalf("c-shared iter values = %#v, want one table descriptor", valuesEnv)
	}
	tableDescriptor, ok := items[0].(map[string]interface{})
	if !ok || tableDescriptor["__omnivm_table__"] != true || tableDescriptor["format"] != "arrow_c_data" {
		t.Fatalf("c-shared iter value = %#v, want Arrow table descriptor", items[0])
	}
	metadata, ok := tableDescriptor["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("c-shared iter table metadata = %#v, want object", tableDescriptor["metadata"])
	}
	shape, ok := metadata["shape"].([]interface{})
	if !ok || len(shape) != 2 || shape[0] != float64(2) || shape[1] != float64(3) {
		t.Fatalf("c-shared iter table shape = %#v, want [2 3]", metadata["shape"])
	}
	strides, ok := metadata["strides"].([]interface{})
	if !ok || len(strides) != 2 || strides[0] != float64(24) || strides[1] != float64(8) {
		t.Fatalf("c-shared iter table strides = %#v, want [24 8]", metadata["strides"])
	}

	tableID, ok := bridgeMarkerHandleID(tableDescriptor)
	if !ok {
		t.Fatalf("c-shared iter table descriptor missing handle id: %#v", tableDescriptor)
	}
	row, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall table index: %v", err)
	}
	rowEnv := decodeResultEnvelopeForTest(t, row)
	rowValues, ok := rowEnv.Value.([]interface{})
	if rowEnv.Kind != "json" || !ok || len(rowValues) != 3 || rowValues[0] != float64(4.5) || rowValues[2] != float64(6.5) {
		t.Fatalf("c-shared iter shaped table row = %#v, want second row", rowEnv)
	}

	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared iter shaped table stats = %+v, want Arrow table without JSON fallback", stats)
	}
}

func TestGoCSharedSourceFallbackPreservesHostProxyArguments(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "echo_request",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "req"}},
		Exports:     []string{"EchoRequest"},
		Source: `package polyfunc

func EchoRequest(req interface{}) interface{} {
	return req
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile proxy-arg c-shared Go func_def: %v", err)
	}

	ref, ok, err := e.autoResourceRefForCapture(map[string]interface{}{"path": "/host-proxy"})
	if err != nil || !ok {
		t.Fatalf("autoResourceRefForCapture = (%v, %v)", ok, err)
	}
	got, err := e.callGoFunc("echo_request", []interface{}{ref}, "")
	if err != nil {
		t.Fatalf("call echo_request: %v", err)
	}
	descriptor, ok := got.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true || descriptor["runtime"] != "go" {
		t.Fatalf("c-shared host proxy arg result = %#v, want original resource descriptor", got)
	}
	gotID, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}
	if gotID != ref.ID {
		t.Fatalf("c-shared host proxy arg id = %d, want original handle %d", gotID, ref.ID)
	}
	if strings.Contains(fmt.Sprint(got), "/host-proxy") {
		t.Fatalf("c-shared host proxy arg should not JSON-copy object contents: %#v", got)
	}
}

func TestGoCSharedObjectSetPreservesRuntimeRefValues(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "request",
		BodyRuntime: "go",
		Exports:     []string{"Request"},
		Source: `package polyfunc

var store = map[string]interface{}{}

func Request() map[string]interface{} {
	return store
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile complex c-shared Go func_def: %v", err)
	}

	result, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("c-shared request result = %#v, want resource descriptor", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	payload := RuntimeRef{Runtime: "javascript", VarName: "__omnivm_arg_refs[\"payload\"]", CallableKnown: true}
	setJSON, err := json.Marshal(map[string]interface{}{
		"op":  "handle_set",
		"id":  uint64(id),
		"key": "payload",
		"value": map[string]interface{}{
			"__omnivm_runtime_ref__": true,
			"runtime":                payload.Runtime,
			"var":                    payload.VarName,
			"callable":               payload.Callable,
		},
	})
	if err != nil {
		t.Fatalf("marshal handle_set: %v", err)
	}
	if _, err := e.HandleCall(string(setJSON)); err != nil {
		t.Fatalf("HandleCall handle_set payload: %v", err)
	}

	got, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"payload"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get payload: %v", err)
	}
	gotEnv := decodeResultEnvelopeForTest(t, got)
	gotDescriptor, ok := gotEnv.Value.(map[string]interface{})
	if gotEnv.Kind != "json" || !ok || gotDescriptor["__omnivm_resource__"] != true || gotDescriptor["runtime"] != "javascript" {
		t.Fatalf("c-shared set RuntimeRef result = %#v, want live javascript proxy descriptor", gotEnv)
	}
	if strings.Contains(got, "VarName") || strings.Contains(got, "__omnivm_runtime_ref__") {
		t.Fatalf("c-shared set RuntimeRef should not round-trip as copied marker JSON: %s", got)
	}
	childID, err := bridgeHandleID(gotDescriptor["id"])
	if err != nil {
		t.Fatalf("child resource id: %v", err)
	}
	entry, live := e.ensureHandleTable().Get(childID)
	if !live {
		t.Fatalf("child handle %d is not live", childID)
	}
	ref, ok := runtimeRefFromHandleValue(entry.Value)
	if !ok || ref.Runtime != payload.Runtime || ref.VarName != payload.VarName || !ref.CallableKnown || ref.Callable {
		t.Fatalf("child handle value = %#v, want preserved RuntimeRef %#v", entry.Value, payload)
	}
}

func TestGoCSharedObjectCallPreservesRuntimeRefArguments(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "request",
		BodyRuntime: "go",
		Exports:     []string{"Request"},
		Source: `package polyfunc

var store = map[string]interface{}{}

func init() {
	store["take"] = func(value interface{}) interface{} {
		store["payload"] = value
		return value
	}
}

func Request() map[string]interface{} {
	return store
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile callable c-shared Go func_def: %v", err)
	}

	result, err := e.HandleCall(`{"func":"request","args":[]}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("c-shared request result = %#v, want resource descriptor", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)

	payload := RuntimeRef{Runtime: "javascript", VarName: "__omnivm_arg_refs[\"call_payload\"]", CallableKnown: true}
	callJSON, err := json.Marshal(map[string]interface{}{
		"op":  "handle_call",
		"id":  uint64(id),
		"key": "take",
		"args": []interface{}{map[string]interface{}{
			"__omnivm_runtime_ref__": true,
			"runtime":                payload.Runtime,
			"var":                    payload.VarName,
			"callable":               payload.Callable,
		}},
	})
	if err != nil {
		t.Fatalf("marshal handle_call: %v", err)
	}
	got, err := e.HandleCall(string(callJSON))
	if err != nil {
		t.Fatalf("HandleCall handle_call take: %v", err)
	}
	gotEnv := decodeResultEnvelopeForTest(t, got)
	gotDescriptor, ok := gotEnv.Value.(map[string]interface{})
	if gotEnv.Kind != "json" || !ok || gotDescriptor["__omnivm_resource__"] != true || gotDescriptor["runtime"] != "javascript" {
		t.Fatalf("c-shared call RuntimeRef result = %#v, want live javascript proxy descriptor", gotEnv)
	}
	if strings.Contains(got, "__omnivm_runtime_ref__") {
		t.Fatalf("c-shared call RuntimeRef should not round-trip as copied marker JSON: %s", got)
	}

	stored, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"payload"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get payload: %v", err)
	}
	storedEnv := decodeResultEnvelopeForTest(t, stored)
	storedDescriptor, ok := storedEnv.Value.(map[string]interface{})
	if storedEnv.Kind != "json" || !ok || storedDescriptor["__omnivm_resource__"] != true || storedDescriptor["runtime"] != "javascript" {
		t.Fatalf("c-shared stored RuntimeRef result = %#v, want live javascript proxy descriptor", storedEnv)
	}
}

func TestGoCSharedSourceFallbackConsumesArrowTableAsTypedSlice(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "sum_scores",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "scores"}},
		Exports:     []string{"SumScores"},
		Source: `package polyfunc

func SumScores(scores []int32) int32 {
	var total int32
	for _, score := range scores {
		total += score
	}
	return total
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile typed-slice c-shared Go func_def: %v", err)
	}
	ref, ok, err := e.autoBulkTableRefForCapture([]int32{4, 5, 6})
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}
	beforeArrow := arrow.GlobalStore().Stats()
	got, err := e.callGoFunc("sum_scores", []interface{}{ref}, "")
	if err != nil {
		t.Fatalf("call sum_scores: %v", err)
	}
	if got != 15 {
		t.Fatalf("sum_scores = %#v, want 15", got)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyBorrows <= beforeArrow.ZeroCopyBorrows {
		t.Fatalf("c-shared typed slice arg did not borrow the Arrow buffer: before=%+v after=%+v", beforeArrow, afterArrow)
	}
	stats := e.BoundaryStats()
	if stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared typed slice arg used JSON fallback: %+v", stats)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func registerCSharedTableArgForTest(t *testing.T, e *Executor, ref *TableRef) {
	t.Helper()
	id, err := e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    "table:" + ref.Format,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register c-shared table arg: %v", err)
	}
	ref.ID = id
	e.tables[id] = ref
}

func TestGoCSharedSourceFallbackConsumesShapedArrowTableAsFixedArray(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "sum_matrix",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "matrix"}},
		Exports:     []string{"SumMatrix"},
		Source: `package polyfunc

func SumMatrix(matrix [2][3]float64) float64 {
	total := 0.0
	for _, row := range matrix {
		for _, value := range row {
			total += value
		}
	}
	return total
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile shaped-array c-shared Go func_def: %v", err)
	}
	ref, ok, err := e.autoBulkTableRefForCapture([2][3]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}})
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}
	beforeArrow := arrow.GlobalStore().Stats()
	got, err := e.callGoFunc("sum_matrix", []interface{}{ref}, "")
	if err != nil {
		t.Fatalf("call sum_matrix: %v", err)
	}
	total, ok := numericFloat(got)
	if !ok || total != 24.0 {
		t.Fatalf("sum_matrix = %#v, want 24", got)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyBorrows <= beforeArrow.ZeroCopyBorrows {
		t.Fatalf("c-shared shaped array arg did not borrow the Arrow buffer: before=%+v after=%+v", beforeArrow, afterArrow)
	}
	stats := e.BoundaryStats()
	if stats.JSONFallbacks != 0 || stats.ResourceProxyCaptures != 0 {
		t.Fatalf("c-shared shaped array arg used fallback/proxy: %+v", stats)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoCSharedSourceFallbackConsumesShapedArrowTableAsNestedSlice(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "sum_matrix",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "matrix"}},
		Exports:     []string{"SumMatrix"},
		Source: `package polyfunc

func SumMatrix(matrix [][]float64) float64 {
	total := 0.0
	for _, row := range matrix {
		for _, value := range row {
			total += value
		}
	}
	return total
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile nested-slice c-shared Go func_def: %v", err)
	}
	ref, ok, err := e.autoBulkTableRefForCapture([2][3]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}})
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}
	beforeArrow := arrow.GlobalStore().Stats()
	got, err := e.callGoFunc("sum_matrix", []interface{}{ref}, "")
	if err != nil {
		t.Fatalf("call sum_matrix: %v", err)
	}
	total, ok := numericFloat(got)
	if !ok || total != 24.0 {
		t.Fatalf("sum_matrix = %#v, want 24", got)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyBorrows <= beforeArrow.ZeroCopyBorrows {
		t.Fatalf("c-shared shaped nested-slice arg did not borrow the Arrow buffer: before=%+v after=%+v", beforeArrow, afterArrow)
	}
	stats := e.BoundaryStats()
	if stats.JSONFallbacks != 0 || stats.ResourceProxyCaptures != 0 {
		t.Fatalf("c-shared shaped nested-slice arg used fallback/proxy: %+v", stats)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoCSharedSourceFallbackConsumesStridedArrowTableAsTypedSlice(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "sum_scores",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "scores"}},
		Exports:     []string{"SumScores"},
		Source: `package polyfunc

func SumScores(scores []uint16) uint16 {
	var total uint16
	for _, score := range scores {
		total += score
	}
	return total
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile strided typed-slice c-shared Go func_def: %v", err)
	}
	name := "test_go_cshared_strided_arg"
	if _, err := arrow.GlobalStore().SetWithMetadata(name, []byte{
		4, 0, 99, 0,
		5, 0, 99, 0,
		6, 0,
	}, arrow.BufferMetadata{
		Dtype:   arrow.DtypeU16,
		Format:  "S",
		Shape:   []int64{3},
		Strides: []int64{4},
	}); err != nil {
		t.Fatalf("SetWithMetadata: %v", err)
	}
	t.Cleanup(func() { _ = arrow.GlobalStore().Free(name) })
	dtype := int32(arrow.DtypeU16)
	ref := &TableRef{
		Runtime:   "python",
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: "S",
			Buffer:      name,
			Shape:       []int64{3},
			Strides:     []int64{4},
			ReadOnly:    true,
		},
		Value: name,
	}
	registerCSharedTableArgForTest(t, e, ref)
	beforeArrow := arrow.GlobalStore().Stats()
	got, err := e.callGoFunc("sum_scores", []interface{}{ref}, "")
	if err != nil {
		t.Fatalf("call sum_scores with strided Arrow table: %v", err)
	}
	if got != 15 {
		t.Fatalf("sum_scores = %#v, want 15", got)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyBorrows <= beforeArrow.ZeroCopyBorrows {
		t.Fatalf("c-shared strided typed-slice arg did not borrow the Arrow buffer: before=%+v after=%+v", beforeArrow, afterArrow)
	}
	stats := e.BoundaryStats()
	if stats.JSONFallbacks != 0 || stats.ResourceProxyCaptures != 0 {
		t.Fatalf("c-shared strided typed-slice arg used fallback/proxy: %+v", stats)
	}
}

func TestGoCSharedSourceFallbackConsumesNegativeStridedArrowTableAsTypedSlice(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "weighted",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "scores"}},
		Exports:     []string{"Weighted"},
		Source: `package polyfunc

func Weighted(scores []uint16) uint16 {
	var total uint16
	for i, score := range scores {
		total += uint16(i + 1) * score
	}
	return total
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile negative-strided typed-slice c-shared Go func_def: %v", err)
	}
	name := "test_go_cshared_negative_strided_arg"
	if _, err := arrow.GlobalStore().SetWithMetadata(name, []byte{
		4, 0, 99, 0,
		5, 0, 99, 0,
		6, 0,
	}, arrow.BufferMetadata{
		Dtype:   arrow.DtypeU16,
		Format:  "S",
		Shape:   []int64{3},
		Strides: []int64{-4},
		Offset:  8,
	}); err != nil {
		t.Fatalf("SetWithMetadata: %v", err)
	}
	t.Cleanup(func() { _ = arrow.GlobalStore().Free(name) })
	dtype := int32(arrow.DtypeU16)
	ref := &TableRef{
		Runtime:   "python",
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: "S",
			Buffer:      name,
			Shape:       []int64{3},
			Strides:     []int64{-4},
			Offset:      8,
			ReadOnly:    true,
		},
		Value: name,
	}
	registerCSharedTableArgForTest(t, e, ref)
	beforeArrow := arrow.GlobalStore().Stats()
	got, err := e.callGoFunc("weighted", []interface{}{ref}, "")
	if err != nil {
		t.Fatalf("call weighted with negative-strided Arrow table: %v", err)
	}
	if got != 28 {
		t.Fatalf("weighted = %#v, want 28", got)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyBorrows <= beforeArrow.ZeroCopyBorrows {
		t.Fatalf("c-shared negative-strided typed-slice arg did not borrow the Arrow buffer: before=%+v after=%+v", beforeArrow, afterArrow)
	}
	stats := e.BoundaryStats()
	if stats.JSONFallbacks != 0 || stats.ResourceProxyCaptures != 0 {
		t.Fatalf("c-shared negative-strided typed-slice arg used fallback/proxy: %+v", stats)
	}
}

func TestGoCSharedSourceFallbackConsumesByteBufferAsByteSlice(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available")
	}

	prev := UseGoSourceFallback
	UseGoSourceFallback = true
	t.Cleanup(func() { UseGoSourceFallback = prev })

	e := NewExecutor(nil)
	op := &Op{
		OpType:      "func_def",
		Name:        "checksum",
		BodyRuntime: "go",
		Params:      []*Param{{Name: "payload"}},
		Exports:     []string{"Checksum"},
		Source: `package polyfunc

func Checksum(payload []byte) int {
	total := 0
	for _, b := range payload {
		total += int(b)
	}
	return total
}
`,
	}

	if _, err := e.compileGoPlugin(op); err != nil {
		t.Fatalf("compile byte-slice c-shared Go func_def: %v", err)
	}
	ref, ok, err := e.autoBulkTableRefForCapture([]byte{2, 3, 5})
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}
	beforeArrow := arrow.GlobalStore().Stats()
	got, err := e.callGoFunc("checksum", []interface{}{ref}, "")
	if err != nil {
		t.Fatalf("call checksum: %v", err)
	}
	if got != 10 {
		t.Fatalf("checksum = %#v, want 10", got)
	}
	afterArrow := arrow.GlobalStore().Stats()
	if afterArrow.ZeroCopyBorrows <= beforeArrow.ZeroCopyBorrows {
		t.Fatalf("c-shared byte slice arg did not borrow the Arrow buffer: before=%+v after=%+v", beforeArrow, afterArrow)
	}
	stats := e.BoundaryStats()
	if stats.JSONFallbacks != 0 {
		t.Fatalf("c-shared byte slice arg used JSON fallback: %+v", stats)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}
