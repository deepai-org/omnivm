package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/handles"
)

// mockRuntime is a minimal mock of pkg.Runtime for testing the manifest executor
// without real runtimes (no cgo dependency).
type mockRuntime struct {
	name      string
	execFn    func(code string) pkg.Result
	evalFn    func(code string) pkg.Result
	exportFn  func(name, expr string) (pkg.ExportedBuffer, bool, error)
	pumpFn    func()
	execCalls []string
	evalCalls []string
	exports   []string
	pumpCalls int
}

type closeTrackingReader struct {
	*strings.Reader
	closed bool
}

func (r *closeTrackingReader) Close() error {
	r.closed = true
	return nil
}

type goProxyTestCloser struct {
	calls int
	err   error
}

func (c *goProxyTestCloser) Close() error {
	c.calls++
	return c.err
}

type closeErrorReader struct {
	*strings.Reader
}

func (r *closeErrorReader) Close() error {
	return errors.New("close failed")
}

type errorAfterChunkReader struct {
	chunk  string
	reads  int
	closed bool
}

func (r *errorAfterChunkReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads == 1 {
		return copy(p, r.chunk), nil
	}
	return 0, errors.New("owner read failed")
}

func (r *errorAfterChunkReader) Close() error {
	r.closed = true
	return nil
}

type errorAfterChunkCloseErrorReader struct {
	errorAfterChunkReader
	closeErr error
}

func (r *errorAfterChunkCloseErrorReader) Close() error {
	r.closed = true
	if r.closeErr != nil {
		return r.closeErr
	}
	return errors.New("close failed")
}

type goHTTPMessageReaderShape struct {
	Method  string
	Path    string
	Headers map[string]string
	reads   int
}

func (r *goHTTPMessageReaderShape) Read(p []byte) (int, error) {
	r.reads++
	return copy(p, "not-the-request"), nil
}

type goHTTPResponseReaderShape struct {
	RequestMethod string
	Header        map[string]string
	StatusCode    int
	reads         int
}

func (r *goHTTPResponseReaderShape) Read(p []byte) (int, error) {
	r.reads++
	return copy(p, "not-the-response"), nil
}

func newMockRuntime(name string) *mockRuntime {
	return &mockRuntime{
		name: name,
		execFn: func(code string) pkg.Result {
			return pkg.Result{Output: ""}
		},
		evalFn: func(code string) pkg.Result {
			return pkg.Result{Value: "mock"}
		},
	}
}

func (m *mockRuntime) Name() string                               { return m.name }
func (m *mockRuntime) Initialize() error                          { return nil }
func (m *mockRuntime) SetBridgeCallback(callPtr, freePtr uintptr) {}
func (m *mockRuntime) Shutdown() error                            { return nil }

func (m *mockRuntime) Pump() {
	m.pumpCalls++
	if m.pumpFn != nil {
		m.pumpFn()
	}
}

func (m *mockRuntime) Execute(code string) pkg.Result {
	m.execCalls = append(m.execCalls, code)
	if m.execFn != nil {
		return m.execFn(code)
	}
	return pkg.Result{}
}

func (m *mockRuntime) Eval(code string) pkg.Result {
	m.evalCalls = append(m.evalCalls, code)
	if m.evalFn != nil {
		return m.evalFn(code)
	}
	return pkg.Result{}
}

func (m *mockRuntime) ExportBuffer(name, expr string) (pkg.ExportedBuffer, bool, error) {
	m.exports = append(m.exports, expr)
	if m.exportFn != nil {
		return m.exportFn(name, expr)
	}
	return pkg.ExportedBuffer{}, false, nil
}

func makeExecutor(runtimes ...string) (*Executor, map[string]*mockRuntime) {
	mocks := make(map[string]*mockRuntime)
	rts := make(map[string]pkg.Runtime)
	for _, name := range runtimes {
		m := newMockRuntime(name)
		mocks[name] = m
		rts[name] = m
	}
	return NewExecutor(rts), mocks
}

// --- ParseManifest tests ---

func TestParseManifest(t *testing.T) {
	data := `{"version": 1, "defaultRuntime": "javascript", "ops": [{"op": "declare", "bind": "x", "value": {"kind": "literal", "value": 42}}]}`
	m, err := ParseManifest([]byte(data))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	if m.Version != 1 {
		t.Errorf("version = %d, want 1", m.Version)
	}
	if m.DefaultRuntime != "javascript" {
		t.Errorf("defaultRuntime = %q, want javascript", m.DefaultRuntime)
	}
	if len(m.Ops) != 1 {
		t.Fatalf("ops len = %d, want 1", len(m.Ops))
	}
	if m.Ops[0].OpType != "declare" {
		t.Errorf("op type = %q, want declare", m.Ops[0].OpType)
	}
}

func TestParseManifestInvalid(t *testing.T) {
	_, err := ParseManifest([]byte("not json"))
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestParseManifestValidationUnknownOp(t *testing.T) {
	data := `{"version":1,"defaultRuntime":"javascript","ops":[{"op":"bogus"}]}`
	_, err := ParseManifest([]byte(data))
	if err == nil {
		t.Fatal("expected validation error for unknown op")
	}
}

func TestParseManifestValidationSpawnRequiresCode(t *testing.T) {
	data := `{"version":1,"defaultRuntime":"javascript","ops":[{"op":"spawn","runtime":"go","bind":"h"}]}`
	_, err := ParseManifest([]byte(data))
	if err == nil {
		t.Fatal("expected validation error for spawn without code")
	}
}

func TestParseManifestValidationChanSendRequiresValue(t *testing.T) {
	data := `{"version":1,"defaultRuntime":"javascript","ops":[{"op":"chan","action":"send","runtime":"go","channel":"ch"}]}`
	_, err := ParseManifest([]byte(data))
	if err == nil {
		t.Fatal("expected validation error for chan send without value")
	}
}

// --- Scope tests ---

func TestScopeBasic(t *testing.T) {
	e, _ := makeExecutor()
	e.setBinding("x", 42)
	val, ok := e.getBinding("x")
	if !ok || val != 42 {
		t.Errorf("getBinding(x) = %v, %v; want 42, true", val, ok)
	}
}

func TestScopeShadowing(t *testing.T) {
	e, _ := makeExecutor()
	e.setBinding("x", "outer")
	e.pushScope()
	e.setBinding("x", "inner")
	val, _ := e.getBinding("x")
	if val != "inner" {
		t.Errorf("inner scope: got %v, want inner", val)
	}
	e.popScope()
	val, _ = e.getBinding("x")
	if val != "outer" {
		t.Errorf("after pop: got %v, want outer", val)
	}
}

func TestScopeUndefined(t *testing.T) {
	e, _ := makeExecutor()
	_, ok := e.getBinding("nope")
	if ok {
		t.Error("expected undefined binding")
	}
}

// --- isTruthy tests ---

func TestIsTruthy(t *testing.T) {
	cases := []struct {
		val  interface{}
		want bool
	}{
		{nil, false},
		{true, true},
		{false, false},
		{"hello", true},
		{"", false},
		{"false", false},
		{"none", false},
		{"null", false},
		{"nil", false},
		{"0", false},
		{"undefined", false},
		{float64(1), true},
		{float64(0), false},
		{int(0), false},
		{int(1), true},
		{json.Number("0"), false},
		{json.Number("42"), true},
		{[]int{1, 2}, true}, // non-nil, non-basic type
	}
	for _, tc := range cases {
		got := isTruthy(tc.val)
		if got != tc.want {
			t.Errorf("isTruthy(%v) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// --- applyOperator tests ---

func TestApplyOperator(t *testing.T) {
	cases := []struct {
		existing interface{}
		op       string
		newVal   interface{}
		want     interface{}
	}{
		{10, "+=", 5, 15},
		{10, "-=", 3, 7},
		{4, "*=", 5, 20},
		{10, "/=", 4, 2.5},
		{float64(1.5), "+=", float64(0.5), 2},
	}
	for _, tc := range cases {
		got, err := applyOperator(tc.existing, tc.op, tc.newVal)
		if err != nil {
			t.Errorf("applyOperator(%v, %q, %v) error: %v", tc.existing, tc.op, tc.newVal, err)
			continue
		}
		if got != tc.want {
			t.Errorf("applyOperator(%v, %q, %v) = %v, want %v", tc.existing, tc.op, tc.newVal, got, tc.want)
		}
	}
}

func TestApplyOperatorDivZero(t *testing.T) {
	_, err := applyOperator(10, "/=", 0)
	if err == nil {
		t.Error("expected division by zero error")
	}
}

func TestApplyOperatorUnknown(t *testing.T) {
	_, err := applyOperator(10, "^=", 2)
	if err == nil {
		t.Error("expected unknown operator error")
	}
}

// --- toFloat tests ---

func TestToFloat(t *testing.T) {
	cases := []struct {
		val  interface{}
		want float64
	}{
		{float64(3.14), 3.14},
		{int(42), 42.0},
		{"3.14", 3.14},
		{"not a number", 0.0},
		{json.Number("99"), 99.0},
		{RuntimeRef{Value: int(7)}, 7.0},
		{nil, 0.0},
	}
	for _, tc := range cases {
		got := toFloat(tc.val)
		if got != tc.want {
			t.Errorf("toFloat(%v) = %v, want %v", tc.val, got, tc.want)
		}
	}
}

// --- convertFStringToTemplateLiteral tests ---

func TestConvertFString(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{`f"hello {name}"`, "`hello ${name}`"},
		{`f'count: {n}'`, "`count: ${n}`"},
		{`no f-string here`, `no f-string here`},
		{`f"a {x} b {y} c"`, "`a ${x} b ${y} c`"},
	}
	for _, tc := range cases {
		got := convertFStringToTemplateLiteral(tc.input)
		if got != tc.want {
			t.Errorf("convertFString(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// --- Op execution tests (with mock runtimes) ---

func TestOpDeclare(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	op := &Op{
		OpType: "declare",
		Bind:   "greeting",
		Value:  &ValueExpr{Kind: "literal", Value: "hello"},
	}
	_, err := e.executeOp(op)
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	val, ok := e.getBinding("greeting")
	if !ok || val != "hello" {
		t.Errorf("greeting = %v, want hello", val)
	}
}

func TestOpAssignSimple(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"
	e.setBinding("counter", 10)

	op := &Op{
		OpType:   "assign",
		Target:   "counter",
		Operator: "+=",
		Value:    &ValueExpr{Kind: "literal", Value: float64(5)},
	}
	_, err := e.executeOp(op)
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	val, _ := e.getBinding("counter")
	if val != 15 {
		t.Errorf("counter = %v, want 15", val)
	}
}

func TestOpConcat(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"
	e.setBinding("name", "world")

	op := &Op{
		OpType: "concat",
		Bind:   "result",
		Segments: []*ConcatSegment{
			{Kind: "text", Value: "Hello, "},
			{Kind: "ref", Name: "name"},
			{Kind: "text", Value: "!"},
		},
	}
	_, err := e.executeOp(op)
	if err != nil {
		t.Fatalf("concat: %v", err)
	}
	val, _ := e.getBinding("result")
	if val != "Hello, world!" {
		t.Errorf("result = %q, want %q", val, "Hello, world!")
	}
}

func TestOpConcatUndefinedRef(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	op := &Op{
		OpType:   "concat",
		Segments: []*ConcatSegment{{Kind: "ref", Name: "missing"}},
	}
	_, err := e.executeOp(op)
	if err == nil {
		t.Error("expected error for undefined ref")
	}
}

func TestOpIfTruthy(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	op := &Op{
		OpType: "if",
		Arms: []*IfArm{
			{
				Test: &CondExpr{Kind: "literal", Value: true},
				Body: []*Op{
					{OpType: "declare", Bind: "hit", Value: &ValueExpr{Kind: "literal", Value: "yes"}},
				},
			},
		},
	}
	_, err := e.executeOp(op)
	if err != nil {
		t.Fatalf("if: %v", err)
	}
	val, ok := e.getBinding("hit")
	if !ok || val != "yes" {
		t.Errorf("hit = %v, want yes", val)
	}
}

func TestOpIfFalsy(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	op := &Op{
		OpType: "if",
		Arms: []*IfArm{
			{
				Test: &CondExpr{Kind: "literal", Value: false},
				Body: []*Op{
					{OpType: "declare", Bind: "hit", Value: &ValueExpr{Kind: "literal", Value: "yes"}},
				},
			},
		},
		ElseBody: []*Op{
			{OpType: "declare", Bind: "hit", Value: &ValueExpr{Kind: "literal", Value: "no"}},
		},
	}
	_, err := e.executeOp(op)
	if err != nil {
		t.Fatalf("if: %v", err)
	}
	val, _ := e.getBinding("hit")
	if val != "no" {
		t.Errorf("hit = %v, want no", val)
	}
}

func TestOpThrowAndTry(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	// The catch body runs in a child scope that gets popped,
	// so we use return to propagate the caught value out.
	op := &Op{
		OpType: "try",
		Body: []*Op{
			{OpType: "throw", Value: &ValueExpr{Kind: "literal", Value: "boom"}},
		},
		Catches: []*CatchClause{
			{
				Param: "err",
				Body: []*Op{
					{OpType: "return", Value: &ValueExpr{Kind: "ref", Name: "err"}},
				},
			},
		},
	}
	_, err := e.executeOp(op)
	// The return from catch propagates as ErrReturn
	ret, ok := err.(ErrReturn)
	if !ok {
		t.Fatalf("expected ErrReturn from catch, got %v", err)
	}
	if ret.Value != "boom" {
		t.Errorf("caught = %v, want boom", ret.Value)
	}
}

func TestOpTryFinally(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	op := &Op{
		OpType: "try",
		Body: []*Op{
			{OpType: "declare", Bind: "x", Value: &ValueExpr{Kind: "literal", Value: 1}},
		},
		FinallyBody: []*Op{
			{OpType: "declare", Bind: "cleaned", Value: &ValueExpr{Kind: "literal", Value: true}},
		},
	}
	_, err := e.executeOp(op)
	if err != nil {
		t.Fatalf("try/finally: %v", err)
	}
	val, _ := e.getBinding("cleaned")
	if val != true {
		t.Errorf("cleaned = %v, want true", val)
	}
}

func TestOpReturn(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	op := &Op{
		OpType: "return",
		Value:  &ValueExpr{Kind: "literal", Value: 42},
	}
	_, err := e.executeOp(op)
	ret, ok := err.(ErrReturn)
	if !ok {
		t.Fatalf("expected ErrReturn, got %v", err)
	}
	if ret.Value != 42 {
		t.Errorf("return value = %v, want 42", ret.Value)
	}
}

func TestOpLoopWhile(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"
	e.setBinding("i", 0)

	op := &Op{
		OpType: "loop",
		Mode:   "while",
		Test:   &CondExpr{Kind: "ref", Name: "done"},
		Body: []*Op{
			{OpType: "declare", Bind: "x", Value: &ValueExpr{Kind: "literal", Value: 1}},
		},
	}
	// "done" is not bound, so ref condition returns false → loop doesn't execute
	_, err := e.executeOp(op)
	if err != nil {
		t.Fatalf("loop: %v", err)
	}
}

func TestOpForeach(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"
	e.setBinding("items", []interface{}{"a", "b", "c"})
	e.setBinding("collected", "")

	op := &Op{
		OpType:   "loop",
		Mode:     "foreach",
		Variable: "item",
		Iterable: &ValueExpr{Kind: "ref", Name: "items"},
		Body: []*Op{
			{
				OpType:   "assign",
				Target:   "collected",
				Operator: "+=",
				Value:    &ValueExpr{Kind: "ref", Name: "item"},
			},
		},
	}
	// String += string won't work numerically, but the assignment still sets the binding
	_, err := e.executeOp(op)
	// The applyOperator will try to convert "a" to float (0) and add — that's fine for this test
	if err != nil {
		t.Fatalf("foreach: %v", err)
	}
}

func TestOpUnknownType(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"
	_, err := e.executeOp(&Op{OpType: "nonsense"})
	if err == nil {
		t.Error("expected error for unknown op type")
	}
}

func TestAwaitExecutesFromOpAndBindsResult(t *testing.T) {
	e, _ := makeExecutor()
	val, err := e.executeOp(&Op{
		OpType: "await",
		Bind:   "answer",
		From: &Op{
			OpType:  "declare",
			Bind:    "__inner",
			Mutable: false,
			Value:   &ValueExpr{Kind: "literal", Value: 42},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if val != 42 {
		t.Fatalf("await value = %v, want 42", val)
	}
	if got, _ := e.getBinding("answer"); got != 42 {
		t.Fatalf("await binding = %v, want 42", got)
	}
}

func TestPumpUntilDoneTimeoutReturnsError(t *testing.T) {
	oldTimeout := asyncPumpTimeout
	oldInterval := asyncPumpInterval
	asyncPumpTimeout = 5 * time.Millisecond
	asyncPumpInterval = time.Millisecond
	defer func() {
		asyncPumpTimeout = oldTimeout
		asyncPumpInterval = oldInterval
	}()

	e, _ := makeExecutor("javascript")
	err := e.pumpUntilDone(func() bool { return false })
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("pumpUntilDone error = %v, want timeout", err)
	}
}

func TestEvalAsyncJSReturnsPromiseError(t *testing.T) {
	e, mocks := makeExecutor("javascript")
	js := mocks["javascript"]
	js.evalFn = func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "__omni_async_done"):
			return pkg.Result{Value: true}
		case strings.Contains(code, "__omni_async_error"):
			return pkg.Result{Value: "boom"}
		default:
			return pkg.Result{Value: nil}
		}
	}
	_, err := e.evalAsyncJS(&Op{OpType: "eval", Runtime: "javascript", Async: true, Code: "Promise.reject(new Error('boom'))"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("evalAsyncJS error = %v, want boom", err)
	}
}

func TestParallelAsyncBranchErrorPropagates(t *testing.T) {
	e, mocks := makeExecutor("javascript")
	js := mocks["javascript"]
	js.evalFn = func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "__omni_parallel_0_done"):
			return pkg.Result{Value: true}
		case strings.Contains(code, "__omni_parallel_0_error"):
			return pkg.Result{Value: "branch failed"}
		default:
			return pkg.Result{Value: nil}
		}
	}
	_, err := e.opParallel(&Op{
		OpType: "parallel",
		Branches: []*Op{{
			Runtime: "javascript",
			Code:    "Promise.reject(new Error('branch failed'))",
			Bind:    "result",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "branch failed") {
		t.Fatalf("parallel error = %v, want branch failed", err)
	}
}

// --- Channel tests ---

func TestChanMakeSendRecv(t *testing.T) {
	e, _ := makeExecutor()

	// make
	_, err := e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(2)})
	if err != nil {
		t.Fatalf("chan make: %v", err)
	}

	// send
	_, err = e.executeOp(&Op{OpType: "chan", Action: "send", Channel: "ch", Value: &ValueExpr{Kind: "literal", Value: "hello"}})
	if err != nil {
		t.Fatalf("chan send: %v", err)
	}

	// recv
	_, err = e.executeOp(&Op{OpType: "chan", Action: "recv", Channel: "ch", Bind: "msg"})
	if err != nil {
		t.Fatalf("chan recv: %v", err)
	}
	val, _ := e.getBinding("msg")
	if val != "hello" {
		t.Errorf("recv = %v, want hello", val)
	}
}

func TestResourceOpenCloseAndCaptureProxy(t *testing.T) {
	e, _ := makeExecutor()
	_, err := e.executeOp(&Op{
		OpType:   "resource",
		Action:   "open",
		Runtime:  "python",
		Bind:     "tx",
		Kind:     "sqlalchemy.transaction",
		Disposer: "rollback",
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, ok := e.getBinding("tx")
	if !ok {
		t.Fatal("tx binding missing")
	}
	ref, ok := val.(*ResourceRef)
	if !ok {
		t.Fatalf("tx = %T, want ResourceRef", val)
	}
	if ref.Closed {
		t.Fatal("new resource should be open")
	}
	if stats := e.handleTable.Stats(time.Now()); stats.Live != 1 {
		t.Fatalf("resource open should register one live handle, stats=%+v", stats)
	}
	jsonVal, err := marshalForCapture(ref)
	if err != nil {
		t.Fatalf("marshal resource: %v", err)
	}
	if !strings.Contains(jsonVal, `"__omnivm_resource__":true`) {
		t.Fatalf("resource proxy missing marker: %s", jsonVal)
	}
	valueJSON, err := marshalForCapture(*ref)
	if err != nil {
		t.Fatalf("marshal resource value: %v", err)
	}
	if !strings.Contains(valueJSON, `"__omnivm_resource__":true`) || strings.Contains(valueJSON, `"Value"`) {
		t.Fatalf("resource value should marshal as a proxy descriptor, got %s", valueJSON)
	}
	if _, err := e.executeOp(&Op{OpType: "resource", Action: "close", Target: "tx"}); err != nil {
		t.Fatalf("resource close: %v", err)
	}
	if !ref.Closed {
		t.Fatal("resource should be closed")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("resource close should release handle explicitly, stats=%+v", stats)
	}
}

func TestBoundaryStatsCountsResourceAndTableProxyCaptures(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	resource := &ResourceRef{ID: 1, Runtime: "python", Kind: "request"}
	table := &TableRef{ID: 2, Runtime: "python", Format: "arrow_c_data", Ownership: "borrowed"}
	e.setBinding("req", resource)
	e.setBinding("orders", table)

	if _, err := e.wrapWithCaptures("javascript", "use(req, orders)", map[string]string{
		"req":    "req",
		"orders": "orders",
	}); err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures != 1 || stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.CaptureInjections != 1 {
		t.Fatalf("unexpected boundary stats: %+v", stats)
	}
}

func TestByteSliceCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	payload := []byte("automatic-buffer")
	e.setBinding("payload", payload)

	code, err := e.wrapWithCaptures("javascript", "use(payload)", map[string]string{"payload": "payload"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"arrow_c_data"`) || !strings.Contains(code, `"buffer"`) || !strings.Contains(code, `"memory_space":"host"`) {
		t.Fatalf("byte slice capture should inject an Arrow table descriptor, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("byte slice boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.BuffersByFormat["C"] < before.BuffersByFormat["C"]+1 || after.ZeroCopyImports < before.ZeroCopyImports+1 {
		t.Fatalf("byte slice capture did not register zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}
	payload[1] = 'Z'
	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	if tableID == 0 {
		t.Fatalf("byte slice capture did not register a table handle")
	}
	if meta := e.tables[tableID].Metadata; meta == nil || meta.MemorySpace != "host" {
		t.Fatalf("byte slice table metadata memory_space = %+v, want host", meta)
	}
	result, err := e.HandleCall(`{"op":"handle_len","id":` + strconv.FormatUint(uint64(tableID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_len: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(len(payload)) {
		t.Fatalf("byte buffer len envelope = %#v, want %d", env, len(payload))
	}
	result, err = e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(payload[1]) {
		t.Fatalf("byte buffer index envelope = %#v, want %d", env, payload[1])
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	values, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(values) != len(payload) || values[0] != float64(payload[0]) {
		t.Fatalf("byte buffer iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("scope release did not free automatic Arrow buffer: before=%+v after=%+v", before, released)
	}
}

func TestEmptyByteSliceCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	payload := []byte{}
	e.setBinding("payload", payload)

	code, err := e.wrapWithCaptures("javascript", "use(payload)", map[string]string{"payload": "payload"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"arrow_c_data"`) || !strings.Contains(code, `"shape":[0]`) {
		t.Fatalf("empty byte slice capture should inject a zero-length Arrow table descriptor, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("empty byte slice boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	if tableID == 0 {
		t.Fatalf("empty byte slice capture did not register a table handle")
	}
	result, err := e.HandleCall(`{"op":"handle_len","id":` + strconv.FormatUint(uint64(tableID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_len: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(0) {
		t.Fatalf("empty buffer len envelope = %#v, want 0", env)
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	values, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(values) != 0 {
		t.Fatalf("empty buffer iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("scope release did not free empty Arrow buffer: before=%+v after=%+v", before, released)
	}
}

func TestNumericSliceCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	values := []float64{1.25, 2.5, 3.75}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"arrow_c_data"`) || !strings.Contains(code, `"dtype":4`) || !strings.Contains(code, `"arrow_format":"g"`) {
		t.Fatalf("numeric slice capture should inject a float64 Arrow table descriptor, got %q", code)
	}
	if strings.Contains(code, "1.25") || strings.Contains(code, "2.5") || strings.Contains(code, "3.75") {
		t.Fatalf("numeric slice capture should not materialize values into capture JSON, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("numeric slice boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.BuffersByFormat["g"] < before.BuffersByFormat["g"]+1 || after.ZeroCopyImports < before.ZeroCopyImports+1 {
		t.Fatalf("numeric slice capture did not register zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}

	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	if tableID == 0 {
		t.Fatalf("numeric slice capture did not register a table handle")
	}
	result, err := e.HandleCall(`{"op":"handle_len","id":` + strconv.FormatUint(uint64(tableID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_len: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(len(values)) {
		t.Fatalf("numeric buffer len envelope = %#v, want %d", env, len(values))
	}
	result, err = e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != values[1] {
		t.Fatalf("numeric buffer index envelope = %#v, want %v", env, values[1])
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	iter, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(iter) != len(values) || iter[2] != values[2] {
		t.Fatalf("numeric buffer iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("scope release did not free automatic Arrow buffer: before=%+v after=%+v", before, released)
	}
}

func TestUnsignedSliceCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	values := []uint16{258, 772, 1286}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"dtype":7`) || !strings.Contains(code, `"arrow_format":"S"`) {
		t.Fatalf("unsigned slice capture should inject a uint16 Arrow table descriptor, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("unsigned slice boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.BuffersByFormat["S"] < before.BuffersByFormat["S"]+1 || after.ZeroCopyImports < before.ZeroCopyImports+1 {
		t.Fatalf("unsigned slice capture did not register zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}
	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	if tableID == 0 {
		t.Fatalf("unsigned slice capture did not register a table handle")
	}
	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(values[1]) {
		t.Fatalf("unsigned buffer index envelope = %#v, want %v", env, values[1])
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestSigned8SliceCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	values := []int8{-1, 0, 2}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"dtype":10`) || !strings.Contains(code, `"arrow_format":"c"`) {
		t.Fatalf("signed int8 slice should inject an int8 Arrow table descriptor, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("signed int8 slice boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.BuffersByFormat["c"] < before.BuffersByFormat["c"]+1 || after.ZeroCopyImports < before.ZeroCopyImports+1 {
		t.Fatalf("signed int8 slice did not register zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}
	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":0}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(-1) {
		t.Fatalf("signed int8 table index envelope = %#v, want -1", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestUnsigned64SliceCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	values := []uint64{9223372036854775808, 9223372036854775813}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"dtype":9`) || !strings.Contains(code, `"arrow_format":"L"`) {
		t.Fatalf("uint64 slice capture should inject a uint64 Arrow table descriptor, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("uint64 slice boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.BuffersByFormat["L"] < before.BuffersByFormat["L"]+1 || after.ZeroCopyImports < before.ZeroCopyImports+1 {
		t.Fatalf("uint64 slice capture did not register zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}
	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	if tableID == 0 {
		t.Fatalf("uint64 slice capture did not register a table handle")
	}
	value, ok, err := tableBufferValueAt(arrow.DtypeU64, unsafe.Slice((*byte)(unsafe.Pointer(&values[0])), len(values)*8), 1)
	if err != nil || !ok || value != values[1] {
		t.Fatalf("tableBufferValueAt uint64 = (%#v, %v, %v), want %d", value, ok, err, values[1])
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoIntAliasSliceCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	type scores []int
	values := scores{4, 5, 6}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	wantFormat := "l"
	wantDtype := int32(arrow.DtypeI64)
	if strconv.IntSize == 32 {
		wantFormat = "i"
		wantDtype = int32(arrow.DtypeI32)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"arrow_format":"`+wantFormat+`"`) {
		t.Fatalf("Go int alias slice should inject an Arrow table descriptor, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("Go int alias slice boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.ZeroCopyImports < before.ZeroCopyImports+1 {
		t.Fatalf("Go int alias slice did not register zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}
	values[1] = 42
	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(values[1]) {
		t.Fatalf("Go int alias table index envelope = %#v, want %d", env, values[1])
	}
	if got := e.tables[tableID].Metadata.Dtype; got == nil || *got != wantDtype {
		t.Fatalf("Go int alias dtype = %v, want %d", got, wantDtype)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoFixedArrayCaptureBecomesArrowTableHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	values := [3]uint32{10, 20, 30}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_table__":true`) || !strings.Contains(code, `"dtype":8`) || !strings.Contains(code, `"arrow_format":"I"`) {
		t.Fatalf("Go fixed array should inject a uint32 Arrow table descriptor, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("Go fixed array boundary stats = %+v, want Arrow table without JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 {
		t.Fatalf("Go fixed array did not register an Arrow buffer: before=%+v after=%+v", before, after)
	}
	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	result, err := e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	iter, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(iter) != len(values) || iter[2] != float64(values[2]) {
		t.Fatalf("Go fixed array iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoNestedFixedArrayCapturePreservesArrowShape(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	values := [2][3]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}}
	e.setBinding("values", &values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	for _, want := range []string{`"__omnivm_table__":true`, `"dtype":4`, `"arrow_format":"g"`, `"shape":[2,3]`, `"strides":[24,8]`} {
		if !strings.Contains(code, want) {
			t.Fatalf("Go nested fixed array should inject shaped Arrow metadata %s, got %q", want, code)
		}
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("Go nested fixed array boundary stats = %+v, want Arrow table without proxy/JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.ZeroCopyImports < before.ZeroCopyImports+1 {
		t.Fatalf("Go nested fixed array did not register a zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}

	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	row, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(row) != 3 || row[0] != 4.5 || row[2] != 6.5 {
		t.Fatalf("Go nested fixed array row envelope = %#v", env)
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	rows, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(rows) != 2 {
		t.Fatalf("Go nested fixed array iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoRectangularNestedSliceCapturePreservesArrowShape(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	before := arrow.GlobalStore().Stats()
	values := [][]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	for _, want := range []string{`"__omnivm_table__":true`, `"dtype":4`, `"arrow_format":"g"`, `"shape":[2,3]`, `"strides":[24,8]`} {
		if !strings.Contains(code, want) {
			t.Fatalf("Go rectangular nested slice should inject shaped Arrow metadata %s, got %q", want, code)
		}
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("Go rectangular nested slice boundary stats = %+v, want Arrow table without proxy/JSON fallback", stats)
	}
	after := arrow.GlobalStore().Stats()
	if after.LiveBuffers < before.LiveBuffers+1 || after.ZeroCopyImports != before.ZeroCopyImports {
		t.Fatalf("Go rectangular nested slice did not register a non-zero-copy Arrow buffer: before=%+v after=%+v", before, after)
	}

	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	row, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(row) != 3 || row[0] != 4.5 || row[2] != 6.5 {
		t.Fatalf("Go rectangular nested slice row envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoJaggedNestedSliceCaptureDoesNotPretendArrowShape(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	values := [][]float64{{1.5, 2.5}, {3.5}}
	e.setBinding("values", values)

	code, err := e.wrapWithCaptures("javascript", "use(values)", map[string]string{"values": "values"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if strings.Contains(code, `"__omnivm_table__":true`) {
		t.Fatalf("Go jagged nested slice should not be treated as a shaped Arrow buffer: %q", code)
	}
}

func TestScopedResourceAutoReleasesOnPop(t *testing.T) {
	e, _ := makeExecutor()
	e.pushScope()
	_, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "tx",
		Kind:    "sqlalchemy.transaction",
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, ok := e.getBinding("tx")
	if !ok {
		t.Fatal("tx binding missing")
	}
	ref := val.(*ResourceRef)
	if stats := e.handleTable.Stats(time.Now()); stats.Live != 1 {
		t.Fatalf("resource open should register one live handle, stats=%+v", stats)
	}
	e.popScope()
	if !ref.Closed {
		t.Fatal("resource should be closed by scope cleanup")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ScopeReleases != 1 {
		t.Fatalf("scope cleanup should release resource handle, stats=%+v", stats)
	}
}

func TestManifestScopeReleasesTopLevelResourceOnExecuteEnd(t *testing.T) {
	e, _ := makeExecutor()
	m := &Manifest{
		Version:        1,
		DefaultRuntime: "python",
		Ops: []*Op{
			{OpType: "resource", Action: "open", Runtime: "python", Bind: "tx", Kind: "sqlalchemy.transaction"},
		},
	}
	if err := e.Execute(m); err != nil {
		t.Fatalf("execute: %v", err)
	}
	val, ok := e.getBinding("tx")
	if !ok {
		t.Fatal("tx binding missing")
	}
	ref := val.(*ResourceRef)
	if !ref.Closed {
		t.Fatal("top-level resource should be closed by manifest-scope cleanup")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ScopeReleases != 1 {
		t.Fatalf("manifest-scope cleanup should release resource handle, stats=%+v", stats)
	}
}

func TestManifestScopeReleasesTopLevelTableOnExecuteEnd(t *testing.T) {
	e, _ := makeExecutor("python")
	e.setBinding("orders", RuntimeRef{Runtime: "python", VarName: "orders", Value: "arrow-array"})
	m := &Manifest{
		Version:        1,
		DefaultRuntime: "python",
		Ops: []*Op{
			{
				OpType:    "table",
				Action:    "export",
				Runtime:   "python",
				Bind:      "orders_view",
				Format:    "arrow_c_data",
				Ownership: "borrowed",
				Value:     &ValueExpr{Kind: "ref", Name: "orders"},
			},
		},
	}
	if err := e.Execute(m); err != nil {
		t.Fatalf("execute: %v", err)
	}
	val, ok := e.getBinding("orders_view")
	if !ok {
		t.Fatal("orders_view binding missing")
	}
	ref := val.(*TableRef)
	if !ref.Released {
		t.Fatal("top-level table should be released by manifest-scope cleanup")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ScopeReleases != 1 {
		t.Fatalf("manifest-scope cleanup should release table handle, stats=%+v", stats)
	}
}

func TestResourceCloseExecutesCleanupHook(t *testing.T) {
	e, mocks := makeExecutor("python")
	_, err := e.executeOp(&Op{
		OpType:   "resource",
		Action:   "open",
		Runtime:  "python",
		Bind:     "tx",
		Kind:     "sqlalchemy.transaction",
		Disposer: "rollback",
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	if _, err := e.executeOp(&Op{
		OpType: "resource",
		Action: "close",
		Target: "tx",
		Code:   "cleanup_log.append('rollback')",
	}); err != nil {
		t.Fatalf("resource close: %v", err)
	}
	if !containsExecCall(mocks["python"].execCalls, "cleanup_log.append('rollback')") {
		t.Fatalf("cleanup hook was not executed; calls=%q", mocks["python"].execCalls)
	}
}

func TestResourceCloseReleasesRetainedProxyRefs(t *testing.T) {
	e, _ := makeExecutor("python")
	value, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "tx",
		Kind:    "sqlalchemy.transaction",
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	ref := value.(*ResourceRef)
	if err := e.ensureHandleTable().Retain(ref.ID); err != nil {
		t.Fatalf("retain resource proxy ref: %v", err)
	}
	if _, err := e.executeOp(&Op{OpType: "resource", Action: "close", Target: "tx"}); err != nil {
		t.Fatalf("resource close: %v", err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("resource close should release all retained refs explicitly, stats=%+v", stats)
	}
}

func TestResourceCloseRunsFromFinallyBody(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	m := &Manifest{
		Version:        1,
		DefaultRuntime: "javascript",
		Ops: []*Op{
			{
				OpType: "try",
				Body: []*Op{
					{OpType: "resource", Action: "open", Runtime: "python", Bind: "tx", Kind: "sqlalchemy.transaction"},
				},
				FinallyBody: []*Op{
					{OpType: "resource", Action: "close", Target: "tx", Code: "cleanup_log.append('finally')"},
				},
			},
		},
	}
	if err := e.Execute(m); err != nil {
		t.Fatalf("execute: %v", err)
	}
	val, ok := e.getBinding("tx")
	if !ok {
		t.Fatal("tx binding missing")
	}
	ref, ok := val.(*ResourceRef)
	if !ok {
		t.Fatalf("tx = %T, want ResourceRef", val)
	}
	if !ref.Closed {
		t.Fatal("resource should be closed by finallyBody")
	}
	if !containsExecCall(mocks["python"].execCalls, "cleanup_log.append('finally')") {
		t.Fatalf("finally cleanup hook was not executed; calls=%q", mocks["python"].execCalls)
	}
}

func TestTableExportReleaseAndCaptureProxy(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	name := "test_table_export_release_memory_space"
	_ = arrow.GlobalStore().Free(name)
	if _, err := arrow.GlobalStore().SetWithMetadata(name, []byte{1, 2, 3, 4}, arrow.BufferMetadata{
		Dtype:     arrow.DtypeF64,
		Format:    "g",
		Shape:     []int64{2},
		ReadOnly:  true,
		Ownership: "producer",
	}); err != nil {
		t.Fatalf("SetWithMetadata: %v", err)
	}
	defer arrow.GlobalStore().Free(name)
	e.setBinding("orders", RuntimeRef{Runtime: "python", VarName: "orders", Value: "arrow-array"})
	dtype := int32(4)
	nullCount := int64(0)

	_, err := e.executeOp(&Op{
		OpType:    "table",
		Action:    "export",
		Runtime:   "python",
		Bind:      "orders_view",
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Release:   "producer",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: "g",
			Buffer:      name,
			Shape:       []int64{10, 3},
			Strides:     []int64{24, 8},
			NullCount:   &nullCount,
			ReadOnly:    true,
		},
		Value: &ValueExpr{Kind: "ref", Name: "orders"},
	})
	if err != nil {
		t.Fatalf("table export: %v", err)
	}
	val, ok := e.getBinding("orders_view")
	if !ok {
		t.Fatal("orders_view binding missing")
	}
	ref, ok := val.(*TableRef)
	if !ok {
		t.Fatalf("orders_view = %T, want TableRef", val)
	}
	if ref.Format != "arrow_c_data" || ref.Ownership != "borrowed" || ref.Released {
		t.Fatalf("unexpected table ref: %+v", ref)
	}
	if ref.Metadata == nil || ref.Metadata.Dtype == nil || *ref.Metadata.Dtype != dtype || ref.Metadata.ArrowFormat != "g" {
		t.Fatalf("table metadata not preserved: %+v", ref.Metadata)
	}
	if len(ref.Metadata.Shape) != 2 || ref.Metadata.Shape[0] != 10 || len(ref.Metadata.Strides) != 2 || ref.Metadata.Strides[1] != 8 {
		t.Fatalf("table shape/stride metadata not preserved: %+v", ref.Metadata)
	}
	if ref.Metadata.MemorySpace != "host" {
		t.Fatalf("table memory_space = %q, want host", ref.Metadata.MemorySpace)
	}
	if stats := e.handleTable.Stats(time.Now()); stats.Live != 1 {
		t.Fatalf("table export should register one live handle, stats=%+v", stats)
	}
	jsonVal, err := marshalForCapture(ref)
	if err != nil {
		t.Fatalf("marshal table: %v", err)
	}
	if !strings.Contains(jsonVal, `"__omnivm_table__":true`) {
		t.Fatalf("table proxy missing marker: %s", jsonVal)
	}
	if !strings.Contains(jsonVal, `"metadata"`) || !strings.Contains(jsonVal, `"arrow_format":"g"`) || !strings.Contains(jsonVal, `"memory_space":"host"`) {
		t.Fatalf("table proxy missing metadata: %s", jsonVal)
	}
	valueJSON, err := marshalForCapture(*ref)
	if err != nil {
		t.Fatalf("marshal table value: %v", err)
	}
	if !strings.Contains(valueJSON, `"__omnivm_table__":true`) || strings.Contains(valueJSON, `"Value"`) {
		t.Fatalf("table value should marshal as a proxy descriptor, got %s", valueJSON)
	}
	if _, err := e.HandleCall(`{"op":"handle_retain","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`); err != nil {
		t.Fatalf("retain table proxy handle before owner release: %v", err)
	}
	if _, err := e.executeOp(&Op{
		OpType: "table",
		Action: "release",
		Target: "orders_view",
		Code:   "release_log.append('orders_view')",
	}); err != nil {
		t.Fatalf("table release: %v", err)
	}
	if !ref.Released {
		t.Fatal("table should be released")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("table release should release handle explicitly, stats=%+v", stats)
	}
	parentID, err := e.ensureHandleTable().Register(map[string]interface{}{"owner": "table-parent"}, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "resource",
	})
	if err != nil {
		t.Fatalf("register table parent handle: %v", err)
	}
	for _, call := range []string{
		`{"op":"handle_retain","id":%d}`,
		`{"op":"handle_adopt","id":%d}`,
		`{"op":"handle_access","id":%d,"kind":"property"}`,
		`{"op":"handle_release_explicit","id":%d}`,
		`{"op":"handle_len","id":%d}`,
		`{"op":"handle_index","id":%d,"value":0}`,
	} {
		_, err := e.HandleCall(fmt.Sprintf(call, ref.ID))
		if err == nil {
			t.Fatalf("released table call %s did not fail", call)
		}
		got := err.Error()
		for _, want := range []string{"closed table handle", "runtime=python", "format=arrow_c_data", "owner-side lifecycle is released"} {
			if !strings.Contains(got, want) {
				t.Fatalf("released table call %s diagnostic missing %q: %s", call, want, got)
			}
		}
	}
	for _, call := range []string{
		fmt.Sprintf(`{"op":"handle_reference","from":%d,"to":%d,"kind":"property"}`, ref.ID, parentID),
		fmt.Sprintf(`{"op":"handle_reference","from":%d,"to":%d,"kind":"property"}`, parentID, ref.ID),
	} {
		_, err := e.HandleCall(call)
		if err == nil {
			t.Fatalf("released table reference call %s did not fail", call)
		}
		got := err.Error()
		for _, want := range []string{"closed table handle", "runtime=python", "format=arrow_c_data", "owner-side lifecycle is released"} {
			if !strings.Contains(got, want) {
				t.Fatalf("released table reference call %s diagnostic missing %q: %s", call, want, got)
			}
		}
		if strings.Contains(got, "unknown source handle") || strings.Contains(got, "unknown target handle") {
			t.Fatalf("released table reference call %s used generic handle-table diagnostic: %s", call, got)
		}
	}
	beforeCleanup := e.handleTable.Stats(time.Now())
	result, err := e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`)
	if err != nil {
		t.Fatalf("released table handle_release_finalizer should remain idempotent: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != false {
		t.Fatalf("released table handle_release_finalizer envelope = %#v, want false", env)
	}
	afterCleanup := e.handleTable.Stats(time.Now())
	if afterCleanup.FinalizerQueued != beforeCleanup.FinalizerQueued || afterCleanup.FinalizerQueueLen != beforeCleanup.FinalizerQueueLen || afterCleanup.FinalizerReleases != beforeCleanup.FinalizerReleases {
		t.Fatalf("released table finalizer cleanup changed finalizer stats: before=%+v after=%+v", beforeCleanup, afterCleanup)
	}
	for _, call := range []string{
		fmt.Sprintf(`{"op":"handle_drop_reference","from":%d,"to":%d}`, ref.ID, parentID),
		fmt.Sprintf(`{"op":"handle_drop_reference","from":%d,"to":%d}`, parentID, ref.ID),
	} {
		result, err := e.HandleCall(call)
		if err != nil {
			t.Fatalf("released table handle_drop_reference cleanup %s should remain idempotent: %v", call, err)
		}
		env := decodeResultEnvelopeForTest(t, result)
		if env.Kind != "bool" || env.Value != true {
			t.Fatalf("released table handle_drop_reference envelope = %#v, want true", env)
		}
	}
	if !containsExecCall(mocks["python"].execCalls, "release_log.append('orders_view')") {
		t.Fatalf("release hook was not executed; calls=%q", mocks["python"].execCalls)
	}
}

func containsExecCall(calls []string, want string) bool {
	for _, call := range calls {
		if strings.Contains(call, want) {
			return true
		}
	}
	return false
}

func TestJobEnqueueCompleteWait(t *testing.T) {
	e, _ := makeExecutor()
	_, err := e.executeOp(&Op{
		OpType:  "job",
		Action:  "enqueue",
		Runtime: "ruby",
		Kind:    "sidekiq",
		Bind:    "job",
		Payload: &ValueExpr{Kind: "literal", Value: map[string]interface{}{"user": "ada"}},
	})
	if err != nil {
		t.Fatalf("job enqueue: %v", err)
	}
	if _, err := e.executeOp(&Op{
		OpType: "job",
		Action: "complete",
		Target: "job",
		Value:  &ValueExpr{Kind: "literal", Value: "ok"},
	}); err != nil {
		t.Fatalf("job complete: %v", err)
	}
	result, err := e.executeOp(&Op{OpType: "job", Action: "wait", Target: "job", Bind: "job_result"})
	if err != nil {
		t.Fatalf("job wait: %v", err)
	}
	if result != "ok" {
		t.Fatalf("job result = %#v, want ok", result)
	}
	bound, _ := e.getBinding("job_result")
	if bound != "ok" {
		t.Fatalf("job_result binding = %#v", bound)
	}
}

func TestJobCancelRunsCleanupAndExposesState(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	_, err := e.executeOp(&Op{
		OpType:  "job",
		Action:  "enqueue",
		Runtime: "python",
		Kind:    "celery.task",
		Bind:    "job",
		Payload: &ValueExpr{Kind: "literal", Value: map[string]interface{}{"task": "receipt"}},
	})
	if err != nil {
		t.Fatalf("job enqueue: %v", err)
	}
	cancelled, err := e.executeOp(&Op{
		OpType:  "job",
		Action:  "cancel",
		Target:  "job",
		Runtime: "python",
		Value:   &ValueExpr{Kind: "literal", Value: "client-abort"},
		Code:    "cleanup_log.append('cancelled')",
	})
	if err != nil {
		t.Fatalf("job cancel: %v", err)
	}
	job := cancelled.(*JobHandle)
	if !job.Done || !job.Cancelled || job.CancelReason != "client-abort" {
		t.Fatalf("job cancel state = done=%v cancelled=%v reason=%#v", job.Done, job.Cancelled, job.CancelReason)
	}
	if !containsExecCall(mocks["python"].execCalls, "cleanup_log.append('cancelled')") {
		t.Fatalf("cancel cleanup hook was not executed; calls=%q", mocks["python"].execCalls)
	}
	descriptor := jobProxyValue(job)
	if descriptor["done"] != true || descriptor["cancelled"] != true || descriptor["cancelReason"] != "client-abort" {
		t.Fatalf("job descriptor = %#v, want cancelled state", descriptor)
	}
	if _, err := e.executeOp(&Op{OpType: "job", Action: "wait", Target: "job"}); err == nil || !strings.Contains(err.Error(), "was cancelled") {
		t.Fatalf("job wait after cancel err = %v, want cancellation diagnostic", err)
	}
	if _, err := e.executeOp(&Op{OpType: "job", Action: "complete", Target: "job", Value: &ValueExpr{Kind: "literal", Value: "late"}}); err == nil || !strings.Contains(err.Error(), "was cancelled") {
		t.Fatalf("job complete after cancel err = %v, want cancellation diagnostic", err)
	}
}

func TestChanClose(t *testing.T) {
	e, _ := makeExecutor()
	e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(1)})
	_, err := e.executeOp(&Op{OpType: "chan", Action: "close", Channel: "ch"})
	if err != nil {
		t.Fatalf("chan close: %v", err)
	}
	// Double close should error
	_, err = e.executeOp(&Op{OpType: "chan", Action: "close", Channel: "ch"})
	if err == nil {
		t.Error("expected error on double close")
	}
}

func TestChanRecvEmpty(t *testing.T) {
	e, _ := makeExecutor()
	e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(1)})
	_, err := e.executeOp(&Op{OpType: "chan", Action: "recv", Channel: "ch", Bind: "val"})
	if err != nil {
		t.Fatalf("recv empty: %v", err)
	}
	val, _ := e.getBinding("val")
	if val != nil {
		t.Errorf("recv empty = %v, want nil", val)
	}
}

func TestChanUndefined(t *testing.T) {
	e, _ := makeExecutor()
	_, err := e.executeOp(&Op{OpType: "chan", Action: "send", Channel: "nope"})
	if err == nil {
		t.Error("expected error for undefined channel")
	}
}

func TestChanSendFullBufferedDropsWithoutBlocking(t *testing.T) {
	e, _ := makeExecutor()
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "send", Channel: "ch", Value: &ValueExpr{Kind: "literal", Value: "first"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "send", Channel: "ch", Value: &ValueExpr{Kind: "literal", Value: "second"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "recv", Channel: "ch", Bind: "one"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.getBinding("one"); got != "first" {
		t.Fatalf("first recv = %v, want first", got)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "recv", Channel: "ch", Bind: "two"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.getBinding("two"); got != nil {
		t.Fatalf("second recv = %v, want nil dropped send", got)
	}
}

func TestChanBuiltinSendUnbufferedDoesNotBlock(t *testing.T) {
	e, _ := makeExecutor()
	ch := &ChanRef{ch: make(chan interface{})}
	done := make(chan interface{}, 1)
	go func() {
		done <- e.goFuncs["send"].(func(interface{}, interface{}) interface{})(ch, "value")
	}()
	select {
	case got := <-done:
		if got != false {
			t.Fatalf("unbuffered helper send = %v, want false", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("unbuffered helper send blocked")
	}
}

func TestChanSendAfterCloseDoesNotPanic(t *testing.T) {
	e, _ := makeExecutor()
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "close", Channel: "ch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "send", Channel: "ch", Value: &ValueExpr{Kind: "literal", Value: "late"}}); err != nil {
		t.Fatalf("send after close should be a dropped no-op, got %v", err)
	}
}

func TestChanConcurrentHelperSendCloseNoPanic(t *testing.T) {
	e, _ := makeExecutor()
	for i := 0; i < 100; i++ {
		ch := &ChanRef{ch: make(chan interface{}, 1)}
		start := make(chan struct{})
		done := make(chan interface{}, 1)
		go func() {
			<-start
			done <- e.goFuncs["send"].(func(interface{}, interface{}) interface{})(ch, "value")
		}()
		close(start)
		_ = ch.close()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
			t.Fatal("helper send racing close blocked")
		}
	}
}

func TestSelectWithoutDefaultTimesOut(t *testing.T) {
	e, _ := makeExecutor()
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(0)}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := e.executeOp(&Op{
		OpType: "select",
		Cases:  []*SelectCase{{Action: "recv", Channel: "ch"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no case ready") {
		t.Fatalf("select error = %v, want no case ready", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("select took %s, expected bounded timeout", elapsed)
	}
}

func TestSelectSendOnClosedChannelErrors(t *testing.T) {
	e, _ := makeExecutor()
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "close", Channel: "ch"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.executeOp(&Op{
		OpType: "select",
		Cases: []*SelectCase{{
			Action:  "send",
			Channel: "ch",
			Value:   &ValueExpr{Kind: "literal", Value: "late"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("select send closed error = %v, want closed", err)
	}
}

func TestSelectClosedChannelRecvRunsCase(t *testing.T) {
	e, _ := makeExecutor()
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "make", Bind: "ch", Size: float64(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.executeOp(&Op{OpType: "chan", Action: "close", Channel: "ch"}); err != nil {
		t.Fatal(err)
	}
	_, err := e.executeOp(&Op{
		OpType: "select",
		Cases: []*SelectCase{{
			Action:  "recv",
			Channel: "ch",
			Body: []*Op{{
				OpType:  "declare",
				Bind:    "selected",
				Mutable: false,
				Value:   &ValueExpr{Kind: "literal", Value: "closed"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := e.getBinding("selected"); got != "closed" {
		t.Fatalf("selected = %v, want closed", got)
	}
}

// --- GoFunc tests ---

func TestCallGoFunc(t *testing.T) {
	e, _ := makeExecutor()
	e.goFuncs["double"] = func(n interface{}) interface{} {
		return n.(int) * 2
	}

	val, err := e.callGoFunc("double", []interface{}{float64(21)}, "result")
	if err != nil {
		t.Fatalf("callGoFunc: %v", err)
	}
	if val != 42 {
		t.Errorf("double(21) = %v, want 42", val)
	}
	bound, _ := e.getBinding("result")
	if bound != 42 {
		t.Errorf("bound = %v, want 42", bound)
	}
}

func TestCallGoFuncTwoArgs(t *testing.T) {
	e, _ := makeExecutor()
	e.goFuncs["add"] = func(a, b interface{}) interface{} {
		return a.(int) + b.(int)
	}

	val, err := e.callGoFunc("add", []interface{}{float64(3), float64(4)}, "")
	if err != nil {
		t.Fatalf("callGoFunc: %v", err)
	}
	if val != 7 {
		t.Errorf("add(3,4) = %v, want 7", val)
	}
}

func TestCallGoFuncZeroArgs(t *testing.T) {
	e, _ := makeExecutor()
	e.goFuncs["answer"] = func() interface{} {
		return 42
	}

	val, err := e.callGoFunc("answer", nil, "result")
	if err != nil {
		t.Fatalf("callGoFunc: %v", err)
	}
	if val != 42 {
		t.Errorf("answer() = %v, want 42", val)
	}
	bound, _ := e.getBinding("result")
	if bound != 42 {
		t.Errorf("bound = %v, want 42", bound)
	}
}

func TestCallGoFuncUndefined(t *testing.T) {
	e, _ := makeExecutor()
	_, err := e.callGoFunc("nope", nil, "")
	if err == nil {
		t.Error("expected error for undefined function")
	}
}

func TestCallGoFuncPanic(t *testing.T) {
	e, _ := makeExecutor()
	e.goFuncs["boom"] = func(n interface{}) interface{} {
		panic("kaboom")
	}
	_, err := e.callGoFunc("boom", []interface{}{1}, "")
	if err == nil {
		t.Error("expected error from panicking function")
	}
}

func TestCallGoFuncMaterializesResourceHandleProxy(t *testing.T) {
	e, _ := makeExecutor("python", "go")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value: &ValueExpr{Kind: "literal", Value: map[string]interface{}{
			"path":  "/cart",
			"items": []interface{}{"one", "two", "three"},
		}},
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	e.goFuncs["inspect"] = func(arg interface{}) interface{} {
		proxy, ok := arg.(*GoHandleProxy)
		if !ok {
			t.Fatalf("arg = %T, want *GoHandleProxy", arg)
		}
		if proxy.Kind() != "resource" || proxy.Runtime() != "python" || proxy.ResourceKind() != "request" {
			t.Fatalf("unexpected Go handle proxy: %#v", proxy.AsMap())
		}
		if proxy.Get("path") != "/cart" {
			t.Fatalf("Go handle proxy did not fetch generic resource field: %#v", proxy.Get("path"))
		}
		if !proxy.Contains("path") || proxy.Contains("missing") {
			t.Fatalf("Go handle proxy contains returned wrong values")
		}
		if proxy.Len() != 2 {
			t.Fatalf("Go handle proxy length = %d, want 2", proxy.Len())
		}
		keys := proxy.Keys()
		if !containsInterface(keys, "path") || !containsInterface(keys, "items") {
			t.Fatalf("Go handle proxy keys = %#v, want path/items", keys)
		}
		if values := proxy.Values(); len(values) != 2 {
			t.Fatalf("Go handle proxy values len = %d, want 2: %#v", len(values), values)
		}
		items := proxy.Items()
		if !containsGoProxyItem(items, "path", "/cart") {
			t.Fatalf("Go handle proxy items = %#v, want path=/cart", items)
		}
		asMap := proxy.AsMap()
		if asMap["path"] != "/cart" {
			t.Fatalf("Go handle proxy AsMap path = %#v, want /cart; map=%#v", asMap["path"], asMap)
		}
		seq, ok := asMap["items"].(*GoHandleProxy)
		if !ok {
			t.Fatalf("Go handle proxy AsMap items = %T, want *GoHandleProxy", asMap["items"])
		}
		if seq.Len() != 3 || !containsInterface(seq.Keys(), 0) || !containsInterface(seq.Values(), "two") {
			t.Fatalf("Go sequence proxy shape bad: len=%d keys=%#v values=%#v", seq.Len(), seq.Keys(), seq.Values())
		}
		if !seq.Set("0", "zero") || seq.Index(0) != "zero" {
			t.Fatalf("Go sequence proxy index mutation failed: first=%#v", seq.Index(0))
		}
		return uint64(proxy.ID())
	}
	refVal, _ := e.getBinding("req")
	ref := refVal.(*ResourceRef)
	val, err := e.callGoFunc("inspect", []interface{}{ref}, "req_id")
	if err != nil {
		t.Fatalf("callGoFunc: %v", err)
	}
	if val != uint64(ref.ID) {
		t.Fatalf("inspect returned %v, want %d", val, ref.ID)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccesses == 0 || stats.HandleAccessesByKind["property"] == 0 || stats.HandleAccessesByKind["contains"] == 0 || stats.HandleAccessesByKind["length"] == 0 {
		t.Fatalf("Go handle proxy did not record access: %+v", stats)
	}
}

func containsInterface(values []interface{}, want interface{}) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsGoProxyItem(items []GoProxyItem, key, value interface{}) bool {
	for _, item := range items {
		if item.Key == key && item.Value == value {
			return true
		}
	}
	return false
}

func TestGoHandleProxyAutoMaterializesChattyItems(t *testing.T) {
	table := handles.NewTable()
	id, err := table.Register(map[string]interface{}{"path": "/go-batched"}, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "request",
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	getCalls := 0
	iterCalls := 0
	materializations := 0
	proxy := newGoHandleProxy(
		id,
		table,
		"resource",
		map[string]interface{}{
			"__omnivm_resource__": true,
			"id":                  uint64(id),
			"runtime":             "python",
			"kind":                "request",
		},
		func(handles.ID, string) (interface{}, bool, error) {
			getCalls++
			return "/go-batched", true, nil
		},
		nil,
		nil,
		nil,
		func(handles.ID, string) ([]interface{}, bool, error) {
			iterCalls++
			return []interface{}{[]interface{}{"path", "/go-batched"}}, true, nil
		},
		nil,
		nil,
		func(value interface{}) interface{} { return value },
		nil,
		func() { materializations++ },
	)

	for i := 0; i < 90; i++ {
		if got := proxy.Get("path"); got != "/go-batched" {
			t.Fatalf("proxy.Get path = %#v, want /go-batched", got)
		}
	}
	if getCalls >= 90 {
		t.Fatalf("chatty Go proxy did not stop repeated bridge gets: getCalls=%d", getCalls)
	}
	if iterCalls == 0 {
		t.Fatalf("chatty Go proxy did not batch materialize items")
	}
	if materializations != 1 {
		t.Fatalf("chatty Go proxy materializations = %d, want 1", materializations)
	}
	stats := table.Stats(time.Now())
	if stats.ChattyProxyWarnings != 1 || stats.HandleAccessesByKind["property"] < 64 {
		t.Fatalf("chatty Go proxy did not record warning/access stats: %+v", stats)
	}
}

func TestHandleIterMaterializationUpdatesBoundaryStats(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	ref := &ResourceRef{
		Runtime: "python",
		Kind:    "request",
		Value:   map[string]interface{}{"path": "/batched"},
	}
	id, err := e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "request",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	ref.ID = id
	e.resources[id] = ref

	result, err := e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(id), 10) + `,"mode":"items","materialize":true}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter materialize: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" {
		t.Fatalf("handle_iter materialize envelope = %#v, want json", env)
	}
	stats := e.BoundaryStats()
	if stats.ProxyMaterializations != 1 {
		t.Fatalf("proxy materialization stats = %+v, want one materialization", stats)
	}
}

func TestNormalizeGoArgMaterializesStreamDescriptor(t *testing.T) {
	e, _ := makeExecutor("go")
	ch := &ChanRef{ch: make(chan interface{}, 2)}
	ch.ch <- "first"
	ch.ch <- "second"
	if err := ch.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		t.Fatalf("channelStreamHandle: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "channel")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "channel")))
	}
	if value, ok := stream.Recv(); !ok || value != "first" {
		t.Fatalf("stream Recv first = (%#v, %v), want first,true", value, ok)
	}
	if value, ok := stream.Recv(); !ok || value != "second" {
		t.Fatalf("stream Recv second = (%#v, %v), want second,true", value, ok)
	}
	if value, ok := stream.Recv(); ok || value != nil {
		t.Fatalf("stream Recv done = (%#v, %v), want nil,false", value, ok)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 3 || stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("Go stream proxy stats = %+v, want 3 stream reads and release on EOF", stats)
	}
}

func TestNormalizeGoArgMaterializesLocalStreamValues(t *testing.T) {
	e, _ := makeExecutor("go")
	stream, ok := e.normalizeGoArg(map[string]interface{}{
		"__omnivm_stream__": true,
		"values": []interface{}{
			"first",
			map[string]interface{}{"nested": "second"},
		},
	}).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg local stream = %T, want *GoStreamProxy", e.normalizeGoArg(map[string]interface{}{"__omnivm_stream__": true, "values": []interface{}{}}))
	}
	if value, ok := stream.Recv(); !ok || value != "first" {
		t.Fatalf("local stream first = (%#v, %v), want first,true", value, ok)
	}
	value, ok := stream.Recv()
	nested, isMap := value.(map[string]interface{})
	if !ok || !isMap || nested["nested"] != "second" {
		t.Fatalf("local stream second = (%#v, %v), want nested map", value, ok)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("local stream Close: %v", err)
	}
	if value, ok := stream.Recv(); ok || value != nil {
		t.Fatalf("local stream after Close = (%#v, %v), want nil,false", value, ok)
	}
	cleanup, ok := e.normalizeGoArg(map[string]interface{}{
		"__omnivm_stream__": true,
		"values":            []interface{}{"cleanup"},
	}).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg cleanup local stream = %T, want *GoStreamProxy", e.normalizeGoArg(map[string]interface{}{"__omnivm_stream__": true, "values": []interface{}{}}))
	}
	cleanup.ReleaseFromFinalizer()
	if value, ok := cleanup.Recv(); ok || value != nil {
		t.Fatalf("local stream after finalizer cleanup = (%#v, %v), want nil,false", value, ok)
	}
	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 0 || stats.FinalizerQueued != 0 {
		t.Fatalf("local stream should not touch handle table: %+v", stats)
	}
}

func TestGoStreamProxyCloseCancelsWithoutDraining(t *testing.T) {
	e, _ := makeExecutor("go")
	ch := &ChanRef{ch: make(chan interface{}, 2)}
	ch.ch <- "first"
	ch.ch <- "second"
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		t.Fatalf("channelStreamHandle: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "channel")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "channel")))
	}
	if value, ok := stream.Recv(); !ok || value != "first" {
		t.Fatalf("stream Recv first = (%#v, %v), want first,true", value, ok)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close: %v", err)
	}
	if value, ok := stream.Recv(); ok || value != nil {
		t.Fatalf("stream Recv after Close = (%#v, %v), want nil,false", value, ok)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("second stream Close: %v", err)
	}
	stream.ReleaseFromFinalizer()
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 1 || stats.Live != 0 || stats.ExplicitReleases != 1 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go stream proxy close stats = %+v, want one read, one explicit release, no finalizer queue", stats)
	}
	if got := len(ch.ch); got != 1 {
		t.Fatalf("remaining channel values = %d, want 1", got)
	}
}

func TestGoStreamProxyFinalizerReleaseClosesProxy(t *testing.T) {
	e, _ := makeExecutor("go")
	ch := &ChanRef{ch: make(chan interface{}, 1)}
	ch.ch <- "first"
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		t.Fatalf("channelStreamHandle: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "channel")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "channel")))
	}
	stream.ReleaseFromFinalizer()
	if value, ok := stream.Recv(); ok || value != nil {
		t.Fatalf("stream Recv after finalizer cleanup = (%#v, %v), want nil,false", value, ok)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close after finalizer cleanup: %v", err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.FinalizerQueued != 1 || stats.FinalizerQueueLen != 1 || stats.ExplicitReleases != 0 {
		t.Fatalf("Go stream proxy finalizer queue stats = %+v, want one queued cleanup and no explicit release", stats)
	}
	if err := e.handleTable.DrainFinalizerReleases(0); err != nil {
		t.Fatalf("DrainFinalizerReleases: %v", err)
	}
	after := e.handleTable.Stats(time.Now())
	if after.Live != 1 || after.StrongRefs != 1 || after.RetainedRefs != 0 || after.ExplicitReleases != 0 {
		t.Fatalf("Go stream proxy finalizer cleanup stats = %+v, want only finalizer retain released", after)
	}
}

func TestGoStreamProxyCloseReportsExternallyClosedOwner(t *testing.T) {
	e, _ := makeExecutor("go")
	ch := &ChanRef{ch: make(chan interface{}, 1)}
	ch.ch <- "first"
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		t.Fatalf("channelStreamHandle: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "channel")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "channel")))
	}
	if _, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err != nil {
		t.Fatalf("HandleCall stream_cancel: %v", err)
	}
	err = stream.Close()
	if err == nil {
		t.Fatal("Go stream proxy Close after owner cancel did not fail")
	}
	got := err.Error()
	for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Go stream proxy stale Close diagnostic missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "unknown handle") {
		t.Fatalf("Go stream proxy stale Close used generic handle-table diagnostic: %s", got)
	}
	if err := stream.Close(); err == nil {
		t.Fatal("second stale Go stream proxy Close should keep reporting the owner lifecycle error")
	}
	stream.ReleaseFromFinalizer()
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 1 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go stream proxy stale close stats = %+v, want one external explicit release and no finalizer queue", stats)
	}
}

func TestGoStreamProxyCloseErrorRemainsRetryable(t *testing.T) {
	e, _ := makeExecutor("go")
	id, err := e.genericStreamHandle("go", &closeErrorReader{Reader: strings.NewReader("reader-body")})
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "reader")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "reader")))
	}

	if err := stream.Close(); err == nil {
		t.Fatal("Go stream proxy Close with owner close error did not fail")
	} else if !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("Go stream proxy Close error = %v, want close failure", err)
	}
	err = stream.Close()
	if err == nil {
		t.Fatal("Go stream proxy Close after failed close did not keep reporting owner lifecycle failure")
	}
	got := err.Error()
	for _, want := range []string{"closed stream handle", "runtime=go", "kind=reader", "owner-side lifecycle is closed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Go stream proxy retry close diagnostic missing %q: %s", want, got)
		}
	}
	stream.ReleaseFromFinalizer()
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go stream proxy failed close cleanup stats = %+v, want no live handle and no queued finalizer cleanup", stats)
	}
}

func TestGoStreamProxyNextReportsOwnerReadError(t *testing.T) {
	e, _ := makeExecutor("go")
	reader := &errorAfterChunkReader{chunk: "first"}
	id, err := e.genericStreamHandle("go", reader)
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "reader")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "reader")))
	}
	value, ok, err := stream.Next()
	if err != nil || !ok || value == nil {
		t.Fatalf("stream Next first = (%#v, %v, %v), want value,true,nil", value, ok, err)
	}
	if proxy, ok := value.(*GoHandleProxy); ok {
		if err := proxy.Close(); err != nil {
			t.Fatalf("close first chunk proxy: %v", err)
		}
	}
	value, ok, err = stream.Next()
	if err == nil || !strings.Contains(err.Error(), "owner read failed") || ok || value != nil {
		t.Fatalf("stream Next error = (%#v, %v, %v), want nil,false,owner read failure", value, ok, err)
	}
	if !reader.closed {
		t.Fatal("reader was not closed after Go stream proxy read error")
	}
	if value, ok := stream.Recv(); ok || value != nil {
		t.Fatalf("stream Recv after read error = (%#v, %v), want nil,false", value, ok)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("stream Close after read error: %v", err)
	}
	stream.ReleaseFromFinalizer()
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 2 || stats.Live != 0 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go stream proxy read error stats = %+v, want two reads, no live handles, no finalizer queue", stats)
	}
}

func TestGoStreamProxyNextPreservesReadErrorWhenCloseFails(t *testing.T) {
	e, _ := makeExecutor("go")
	closeErr := errors.New("close failed")
	reader := &errorAfterChunkCloseErrorReader{
		errorAfterChunkReader: errorAfterChunkReader{chunk: "first"},
		closeErr:              closeErr,
	}
	id, err := e.genericStreamHandle("go", reader)
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "reader")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "reader")))
	}
	value, ok, err := stream.Next()
	if err != nil || !ok || value == nil {
		t.Fatalf("stream Next first = (%#v, %v, %v), want value,true,nil", value, ok, err)
	}
	if proxy, ok := value.(*GoHandleProxy); ok {
		if err := proxy.Close(); err != nil {
			t.Fatalf("close first chunk proxy: %v", err)
		}
	}
	value, ok, err = stream.Next()
	if err == nil || ok || value != nil {
		t.Fatalf("stream Next error = (%#v, %v, %v), want nil,false,error", value, ok, err)
	}
	got := err.Error()
	for _, want := range []string{"owner read failed", "additionally failed to close stream after read error", "close failed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream Next combined error missing %q: %s", want, got)
		}
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("stream Next combined error should wrap close failure: %v", err)
	}
	if !reader.closed {
		t.Fatal("reader close was not attempted after read error")
	}
	if value, ok, err := stream.Next(); err != nil || ok || value != nil {
		t.Fatalf("stream Next after combined error = (%#v, %v, %v), want nil,false,nil", value, ok, err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 2 || stats.Live != 0 || stats.ReleaseErrors != 1 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go stream proxy combined read/close error stats = %+v, want two reads, one release error, no live handles/finalizers", stats)
	}
}

func TestGoStreamProxyValuesWithErrorReturnsPartialValues(t *testing.T) {
	e, _ := makeExecutor("go")
	reader := &errorAfterChunkReader{chunk: "first"}
	id, err := e.genericStreamHandle("go", reader)
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "reader")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "reader")))
	}
	values, err := stream.ValuesWithError()
	if err == nil || !strings.Contains(err.Error(), "owner read failed") {
		t.Fatalf("stream ValuesWithError err = %v, want owner read failure", err)
	}
	if len(values) != 1 {
		t.Fatalf("stream ValuesWithError values = %#v, want one partial value", values)
	}
	if proxy, ok := values[0].(*GoHandleProxy); ok {
		if err := proxy.Close(); err != nil {
			t.Fatalf("close partial chunk proxy: %v", err)
		}
	}
	if value, ok, err := stream.Next(); err != nil || ok || value != nil {
		t.Fatalf("stream Next after ValuesWithError = (%#v, %v, %v), want nil,false,nil", value, ok, err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 2 || stats.Live != 0 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go stream proxy ValuesWithError stats = %+v, want two reads, no live handles, no finalizer queue", stats)
	}
}

func TestGoHandleProxyKeepsResourceDescriptorFieldsPrivate(t *testing.T) {
	table := handles.NewTable()
	id, err := table.Register(map[string]interface{}{
		"id":       7,
		"runtime":  "app",
		"kind":     "user",
		"closed":   false,
		"disposer": "domain",
		"name":     "Ada",
	}, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "object",
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	proxy := newGoHandleProxy(
		id,
		table,
		"resource",
		map[string]interface{}{
			"__omnivm_resource__": true,
			"id":                  uint64(id),
			"runtime":             "python",
			"kind":                "object",
			"closed":              false,
			"disposer":            "cleanup()",
		},
		func(_ handles.ID, key string) (interface{}, bool, error) {
			values := map[string]interface{}{
				"id":       7,
				"runtime":  "app",
				"kind":     "user",
				"closed":   false,
				"disposer": "domain",
				"name":     "Ada",
			}
			value, ok := values[key]
			return value, ok, nil
		},
		nil,
		nil,
		nil,
		func(handles.ID, string) ([]interface{}, bool, error) {
			return []interface{}{
				[]interface{}{"id", 7},
				[]interface{}{"runtime", "app"},
				[]interface{}{"kind", "user"},
				[]interface{}{"closed", false},
				[]interface{}{"disposer", "domain"},
				[]interface{}{"name", "Ada"},
			}, true, nil
		},
		func(_ handles.ID, key interface{}) (bool, bool, error) {
			_, ok := map[interface{}]bool{
				"id": true, "runtime": true, "kind": true, "closed": true, "disposer": true, "name": true,
			}[key]
			return true, ok, nil
		},
		nil,
		func(value interface{}) interface{} { return value },
		nil,
		nil,
	)
	if proxy.Runtime() != "python" || proxy.ResourceKind() != "object" {
		t.Fatalf("internal metadata accessors changed: runtime=%q kind=%q", proxy.Runtime(), proxy.ResourceKind())
	}
	for key, want := range map[string]interface{}{
		"id": 7, "runtime": "app", "kind": "user", "closed": false, "disposer": "domain", "name": "Ada",
	} {
		if got := proxy.Get(key); got != want {
			t.Fatalf("GoHandleProxy.Get(%q) = %#v, want %#v", key, got, want)
		}
		if !proxy.Contains(key) {
			t.Fatalf("GoHandleProxy.Contains(%q) = false, want true", key)
		}
	}
	asMap := proxy.AsMap()
	if asMap["id"] != 7 || asMap["runtime"] != "app" || asMap["kind"] != "user" || asMap["disposer"] != "domain" {
		t.Fatalf("GoHandleProxy.AsMap exposed descriptor metadata instead of remote fields: %#v", asMap)
	}

	offline := newGoHandleProxy(0, nil, "resource", map[string]interface{}{
		"__omnivm_resource__": true,
		"id":                  uint64(id),
		"runtime":             "python",
		"kind":                "object",
		"closed":              false,
		"disposer":            "cleanup()",
		"name":                "Ada",
	}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if offline.Get("id") != nil || offline.Get("runtime") != nil || offline.Get("kind") != nil || offline.Get("disposer") != nil {
		t.Fatalf("offline GoHandleProxy exposed descriptor metadata through Get: %#v", offline.AsMap())
	}
	if offline.Contains("id") || offline.Contains("runtime") || offline.Contains("kind") || offline.Contains("disposer") {
		t.Fatalf("offline GoHandleProxy exposed descriptor metadata through Contains")
	}
	if asMap := offline.AsMap(); len(asMap) != 1 || asMap["name"] != "Ada" {
		t.Fatalf("offline GoHandleProxy.AsMap = %#v, want only user payload", asMap)
	}
}

func TestGoHandleProxyKeepsTableAndJobDescriptorFieldsPrivate(t *testing.T) {
	tableProxy := newGoHandleProxy(0, nil, "table", map[string]interface{}{
		"__omnivm_table__": true,
		"id":               uint64(7),
		"runtime":          "python",
		"format":           "arrow_c_data",
		"ownership":        "borrowed",
		"metadata":         map[string]interface{}{"dtype": 4},
		"buffer":           "descriptor-buffer",
		"released":         false,
		"name":             "orders",
	}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, key := range []string{"id", "runtime", "format", "ownership", "metadata", "buffer", "released"} {
		if got := tableProxy.Get(key); got != nil {
			t.Fatalf("table GoHandleProxy.Get(%q) = %#v, want nil descriptor-private field", key, got)
		}
		if tableProxy.Contains(key) {
			t.Fatalf("table GoHandleProxy.Contains(%q) = true, want descriptor-private field", key)
		}
	}
	if asMap := tableProxy.AsMap(); len(asMap) != 1 || asMap["name"] != "orders" {
		t.Fatalf("table GoHandleProxy.AsMap = %#v, want only user payload", asMap)
	}

	jobProxy := newGoHandleProxy(0, nil, "job", map[string]interface{}{
		"__omnivm_job__": true,
		"id":             uint64(8),
		"runtime":        "javascript",
		"kind":           "job",
		"done":           true,
		"cancelled":      false,
		"cancelReason":   "descriptor-reason",
		"payload":        "descriptor-payload",
		"result":         "descriptor-result",
		"name":           "import",
	}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	for _, key := range []string{"id", "runtime", "kind", "done", "cancelled", "cancelReason", "payload", "result"} {
		if got := jobProxy.Get(key); got != nil {
			t.Fatalf("job GoHandleProxy.Get(%q) = %#v, want nil descriptor-private field", key, got)
		}
		if jobProxy.Contains(key) {
			t.Fatalf("job GoHandleProxy.Contains(%q) = true, want descriptor-private field", key)
		}
	}
	if asMap := jobProxy.AsMap(); len(asMap) != 1 || asMap["name"] != "import" {
		t.Fatalf("job GoHandleProxy.AsMap = %#v, want only user payload", asMap)
	}
}

func TestGoHandleProxyMetadataAccessorsAreNilSafe(t *testing.T) {
	var proxy *GoHandleProxy

	if proxy.ID() != 0 {
		t.Fatalf("nil GoHandleProxy.ID() = %d, want 0", proxy.ID())
	}
	if proxy.Kind() != "" {
		t.Fatalf("nil GoHandleProxy.Kind() = %q, want empty", proxy.Kind())
	}
	if proxy.Runtime() != "" {
		t.Fatalf("nil GoHandleProxy.Runtime() = %q, want empty", proxy.Runtime())
	}
	if proxy.ResourceKind() != "" {
		t.Fatalf("nil GoHandleProxy.ResourceKind() = %q, want empty", proxy.ResourceKind())
	}
}

func TestGoHandleProxyMaterializesReturnedStreamDescriptor(t *testing.T) {
	e, _ := makeExecutor("go")
	ch := make(chan interface{}, 2)
	ch <- "first"
	ch <- "second"
	close(ch)
	parent := &ResourceRef{Runtime: "go", Kind: "holder", Value: map[string]interface{}{"stream": ch}}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	proxy, ok := e.normalizeGoArg(parent).(*GoHandleProxy)
	if !ok {
		t.Fatalf("normalizeGoArg parent = %T, want *GoHandleProxy", e.normalizeGoArg(parent))
	}
	stream, ok := proxy.Get("stream").(*GoStreamProxy)
	if !ok {
		t.Fatalf("proxy.Get(stream) = %T, want *GoStreamProxy", proxy.Get("stream"))
	}
	values := stream.Values()
	if len(values) != 2 || values[0] != "first" || values[1] != "second" {
		t.Fatalf("stream Values = %#v, want [first second]", values)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 3 || stats.ExplicitReleases != 1 {
		t.Fatalf("Go proxy returned stream stats = %+v, want stream reads and release on EOF", stats)
	}
}

func TestGoHandleProxyQueuesFinalizerRelease(t *testing.T) {
	e, _ := makeExecutor("python", "go")
	if _, err := e.executeOp(&Op{
		OpType:  "table",
		Action:  "export",
		Runtime: "python",
		Bind:    "orders",
		Format:  "arrow_c_data",
		Value:   &ValueExpr{Kind: "literal", Value: "arrow-array"},
	}); err != nil {
		t.Fatalf("table export: %v", err)
	}
	val, _ := e.getBinding("orders")
	proxy := e.normalizeGoArg(val).(*GoHandleProxy)
	proxy.ReleaseFromFinalizer()
	if value := proxy.Get("format"); value != nil {
		t.Fatalf("Go handle proxy finalizer cleanup left proxy usable: %#v", value)
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("Go handle proxy Close after finalizer cleanup: %v", err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.FinalizerQueued != 1 || stats.FinalizerQueueLen != 1 {
		t.Fatalf("Go handle proxy did not queue finalizer release: %+v", stats)
	}
	if err := e.handleTable.DrainFinalizerReleases(0); err != nil {
		t.Fatalf("DrainFinalizerReleases: %v", err)
	}
	if _, ok := e.handleTable.Get(proxy.id); !ok {
		t.Fatal("Go proxy finalizer release consumed the scope owner reference")
	}
	after := e.handleTable.Stats(time.Now())
	if after.Live != 1 || after.StrongRefs != 1 || after.RetainedRefs != 0 || after.ExplicitReleases != 0 {
		t.Fatalf("Go proxy finalizer cleanup stats = %+v, want only finalizer retain released", after)
	}
}

func TestGoHandleProxyCloseReleasesProxyRetain(t *testing.T) {
	e, _ := makeExecutor("python", "go")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/scoped"}},
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)
	proxy := e.normalizeGoArg(ref).(*GoHandleProxy)
	before := e.handleTable.Stats(time.Now())
	if before.Live != 1 || before.StrongRefs != 2 || before.RetainedRefs != 1 {
		t.Fatalf("Go proxy retain stats = %+v, want one live handle with one proxy retain", before)
	}

	if err := proxy.Close(); err != nil {
		t.Fatalf("Go handle proxy Close: %v", err)
	}
	if ref.Closed {
		t.Fatal("Go handle proxy Close consumed the scope owner reference")
	}
	if err := proxy.Close(); err != nil {
		t.Fatalf("second Go handle proxy Close: %v", err)
	}
	proxy.ReleaseFromFinalizer()
	after := e.handleTable.Stats(time.Now())
	if after.Live != 1 || after.StrongRefs != 1 || after.RetainedRefs != 0 || after.ExplicitReleases != before.ExplicitReleases {
		t.Fatalf("Go proxy close stats = before=%+v after=%+v, want proxy retain released without owner close", before, after)
	}
	if after.FinalizerQueued != before.FinalizerQueued || after.FinalizerQueueLen != before.FinalizerQueueLen {
		t.Fatalf("Go proxy close left finalizer cleanup active: before=%+v after=%+v", before, after)
	}
	if value := proxy.Get("path"); value != nil {
		t.Fatalf("closed Go proxy Get(path) = %#v, want nil", value)
	}
	afterClosedAccess := e.handleTable.Stats(time.Now())
	if afterClosedAccess.HandleAccesses != after.HandleAccesses {
		t.Fatalf("closed Go proxy recorded access: before=%+v after=%+v", after, afterClosedAccess)
	}
}

func TestGoProxyCloseHelperClosesHandleStreamsAndCloseables(t *testing.T) {
	if closed, err := ProxyClose(nil); closed || err != nil {
		t.Fatalf("ProxyClose(nil) = (%v, %v), want false,nil", closed, err)
	}
	var nilHandle *GoHandleProxy
	if closed, err := ProxyClose(nilHandle); closed || err != nil {
		t.Fatalf("ProxyClose(typed nil handle) = (%v, %v), want false,nil", closed, err)
	}
	if closed, err := ProxyClose(struct{}{}); closed || err != nil {
		t.Fatalf("ProxyClose(no close) = (%v, %v), want false,nil", closed, err)
	}

	closeable := &goProxyTestCloser{}
	if closed, err := ProxyClose(closeable); !closed || err != nil {
		t.Fatalf("ProxyClose(closeable) = (%v, %v), want true,nil", closed, err)
	}
	if closeable.calls != 1 {
		t.Fatalf("closeable calls = %d, want 1", closeable.calls)
	}
	closeErr := errors.New("close failed")
	failingCloseable := &goProxyTestCloser{err: closeErr}
	if closed, err := OmniVMClose(failingCloseable); closed || !errors.Is(err, closeErr) {
		t.Fatalf("OmniVMClose(failing closeable) = (%v, %v), want false,closeErr", closed, err)
	}

	localStream := newGoLocalStreamProxy([]interface{}{"first"}, nil)
	if closed, err := ProxyClose(localStream); !closed || err != nil {
		t.Fatalf("ProxyClose(local stream) = (%v, %v), want true,nil", closed, err)
	}
	if closed, err := ProxyClose(localStream); closed || err != nil {
		t.Fatalf("second ProxyClose(local stream) = (%v, %v), want false,nil", closed, err)
	}

	e, _ := makeExecutor("python", "go")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/scoped"}},
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)
	proxy := e.normalizeGoArg(ref).(*GoHandleProxy)
	if closed, err := ProxyClose(proxy); !closed || err != nil {
		t.Fatalf("ProxyClose(handle proxy) = (%v, %v), want true,nil", closed, err)
	}
	if ref.Closed {
		t.Fatal("ProxyClose(handle proxy) consumed the scope owner reference")
	}
	if closed, err := OmniVMClose(proxy); closed || err != nil {
		t.Fatalf("OmniVMClose(closed handle proxy) = (%v, %v), want false,nil", closed, err)
	}
	if closed, err := OmnivmClose(proxy); closed || err != nil {
		t.Fatalf("OmnivmClose compatibility alias = (%v, %v), want false,nil", closed, err)
	}
}

func TestGoProxyCloseHelperPreservesRetryableErrors(t *testing.T) {
	table := handles.NewTable()
	releaseErr := errors.New("release boom")
	id, err := table.Register("value", handles.RegisterOptions{
		Runtime: "go",
		Kind:    "resource",
		Release: func(any) error {
			return releaseErr
		},
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	proxy := newGoHandleProxy(id, table, "resource", map[string]interface{}{
		"__omnivm_resource__": true,
		"id":                  uint64(id),
		"runtime":             "go",
		"kind":                "resource",
	}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if err := table.Release(id); err != nil {
		t.Fatalf("release owner ref: %v", err)
	}
	if closed, err := ProxyClose(proxy); closed || !errors.Is(err, releaseErr) {
		t.Fatalf("ProxyClose(failing handle proxy) = (%v, %v), want false,releaseErr", closed, err)
	}
	if closed, err := ProxyClose(proxy); closed || !errors.Is(err, releaseErr) {
		t.Fatalf("second ProxyClose(failing handle proxy) = (%v, %v), want false,releaseErr", closed, err)
	}
}

func TestGoProxyCloseHelperReportsStreamCloseFailureAsNotClosed(t *testing.T) {
	e, _ := makeExecutor("go")
	id, err := e.genericStreamHandle("go", &closeErrorReader{Reader: strings.NewReader("reader-body")})
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}
	stream, ok := e.normalizeGoArg(streamProxyValue(id, "go", "reader")).(*GoStreamProxy)
	if !ok {
		t.Fatalf("normalizeGoArg stream = %T, want *GoStreamProxy", e.normalizeGoArg(streamProxyValue(id, "go", "reader")))
	}

	if closed, err := ProxyClose(stream); closed || err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("ProxyClose(failing stream) = (%v, %v), want false,close failure", closed, err)
	}
	if stream.closed {
		t.Fatal("ProxyClose marked stream closed after failed owner close")
	}
	if closed, err := ProxyClose(stream); closed || err == nil || !strings.Contains(err.Error(), "closed stream handle") {
		t.Fatalf("second ProxyClose(failing stream) = (%v, %v), want false,lifecycle error", closed, err)
	}
}

func TestGoHandleProxyCloseClosesLastOwnedHandle(t *testing.T) {
	e, _ := makeExecutor("python", "go")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)
	proxy := e.normalizeGoArg(ref).(*GoHandleProxy)
	if err := e.ensureHandleTable().Release(ref.ID); err != nil {
		t.Fatalf("release scope owner ref: %v", err)
	}
	if ref.Closed {
		t.Fatal("scope owner release closed resource while proxy retain was still live")
	}

	if err := proxy.Close(); err != nil {
		t.Fatalf("Go handle proxy Close: %v", err)
	}
	if !ref.Closed {
		t.Fatal("Go handle proxy Close did not close last owned resource")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 1 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go proxy last-owner close stats = %+v, want closed explicit release without finalizer queue", stats)
	}
}

func TestGoHandleProxyCloseErrorRemainsObservable(t *testing.T) {
	table := handles.NewTable()
	releaseErr := errors.New("release boom")
	id, err := table.Register("value", handles.RegisterOptions{
		Runtime: "go",
		Kind:    "resource",
		Release: func(any) error {
			return releaseErr
		},
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	proxy := newGoHandleProxy(id, table, "resource", map[string]interface{}{
		"__omnivm_resource__": true,
		"id":                  uint64(id),
		"runtime":             "go",
		"kind":                "resource",
	}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	if err := table.Release(id); err != nil {
		t.Fatalf("release owner ref: %v", err)
	}

	if err := proxy.Close(); err == nil {
		t.Fatal("Go handle proxy Close with owner release error did not fail")
	} else if !strings.Contains(err.Error(), "release boom") {
		t.Fatalf("Go handle proxy Close error = %v, want release boom", err)
	}
	if err := proxy.Close(); err == nil {
		t.Fatal("second Go handle proxy Close should keep reporting the release error")
	} else if !strings.Contains(err.Error(), "release boom") {
		t.Fatalf("second Go handle proxy Close error = %v, want release boom", err)
	}
	proxy.ReleaseFromFinalizer()
	stats := table.Stats(time.Now())
	if stats.Live != 0 || stats.ReleaseErrors != 1 || stats.FinalizerQueued != 0 {
		t.Fatalf("Go handle proxy failed close stats = %+v, want one release error and no queued finalizer cleanup", stats)
	}
}

func TestGoHandleProxyWithErrorMethodsReportOwnerFailures(t *testing.T) {
	ownerErr := errors.New("owner lifecycle failed")
	proxy := newGoHandleProxy(
		7,
		nil,
		"resource",
		map[string]interface{}{"runtime": "python", "kind": "request"},
		func(handles.ID, string) (interface{}, bool, error) {
			return nil, false, ownerErr
		},
		func(handles.ID, interface{}) (interface{}, bool, error) {
			return nil, false, ownerErr
		},
		func(handles.ID, string, interface{}) (bool, error) {
			return false, ownerErr
		},
		func(handles.ID) (int, bool, error) {
			return 0, false, ownerErr
		},
		func(handles.ID, string) ([]interface{}, bool, error) {
			return nil, false, ownerErr
		},
		func(handles.ID, interface{}) (bool, bool, error) {
			return false, false, ownerErr
		},
		func(handles.ID, string, []interface{}) (interface{}, error) {
			return nil, ownerErr
		},
		nil,
		nil,
		nil,
	)

	if value, err := proxy.GetWithError("path"); !errors.Is(err, ownerErr) || value != nil {
		t.Fatalf("GetWithError = (%#v, %v), want owner error", value, err)
	}
	if value, err := proxy.IndexWithError("path"); !errors.Is(err, ownerErr) || value != nil {
		t.Fatalf("IndexWithError = (%#v, %v), want owner error", value, err)
	}
	if value, err := proxy.ValuesWithError(); !errors.Is(err, ownerErr) || value != nil {
		t.Fatalf("ValuesWithError = (%#v, %v), want owner error", value, err)
	}
	if value, err := proxy.KeysWithError(); !errors.Is(err, ownerErr) || value != nil {
		t.Fatalf("KeysWithError = (%#v, %v), want owner error", value, err)
	}
	if value, err := proxy.ItemsWithError(); !errors.Is(err, ownerErr) || value != nil {
		t.Fatalf("ItemsWithError = (%#v, %v), want owner error", value, err)
	}
	if value, err := proxy.ContainsWithError("path"); !errors.Is(err, ownerErr) || value {
		t.Fatalf("ContainsWithError = (%v, %v), want owner error", value, err)
	}
	if value, err := proxy.LenWithError(); !errors.Is(err, ownerErr) || value != 0 {
		t.Fatalf("LenWithError = (%d, %v), want owner error", value, err)
	}
	if value, err := proxy.SetWithError("path", "/cart"); !errors.Is(err, ownerErr) || value {
		t.Fatalf("SetWithError = (%v, %v), want owner error", value, err)
	}
	if value, err := proxy.CallWithError("close"); !errors.Is(err, ownerErr) || value != nil {
		t.Fatalf("CallWithError = (%#v, %v), want owner error", value, err)
	}
	if value, err := proxy.AsMapWithError(); !errors.Is(err, ownerErr) || value != nil {
		t.Fatalf("AsMapWithError = (%#v, %v), want owner error", value, err)
	}

	if proxy.Get("path") != nil || proxy.Index("path") != nil || proxy.Values() != nil || proxy.Keys() != nil || proxy.Items() != nil ||
		proxy.Contains("path") || proxy.Len() != 0 || proxy.Set("path", "/cart") || proxy.Call("close") != nil || proxy.AsMap() != nil {
		t.Fatal("legacy Go proxy helpers should preserve nil/false fallback behavior")
	}

	proxy.closed = true
	if _, err := proxy.GetWithError("path"); err == nil || !strings.Contains(err.Error(), "closed resource handle #7") {
		t.Fatalf("closed GetWithError diagnostic = %v", err)
	}
}

func TestNormalizeGoArgMaterializesTableProxy(t *testing.T) {
	e, _ := makeExecutor("go")
	ref, ok, err := e.autoBulkTableRefForCapture([]uint16{258, 772, 1286})
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}
	proxy, ok := e.normalizeGoArg(ref).(*GoHandleProxy)
	if !ok {
		t.Fatalf("normalizeGoArg table = %T, want *GoHandleProxy", e.normalizeGoArg(ref))
	}
	if proxy.Kind() != "table" || proxy.Len() != 3 {
		t.Fatalf("Go table proxy kind/len = (%q, %d), want table/3", proxy.Kind(), proxy.Len())
	}
	if got := proxy.Index(1); got != uint16(772) {
		t.Fatalf("Go table proxy Index(1) = %#v, want 772", got)
	}
	values := proxy.Values()
	if len(values) != 3 || values[0] != uint16(258) || values[2] != uint16(1286) {
		t.Fatalf("Go table proxy Values = %#v, want uint16 data", values)
	}
	items := proxy.Items()
	if len(items) != 3 || items[1].Key != 1 || items[1].Value != uint16(772) {
		t.Fatalf("Go table proxy Items = %#v, want indexed uint16 data", items)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["iterate"] < 2 || stats.HandleAccessesByKind["index"] < 1 {
		t.Fatalf("Go table proxy access stats = %+v, want indexed and batched iteration", stats)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestGoFunctionConsumesTableProxy(t *testing.T) {
	e, _ := makeExecutor("go")
	ref, ok, err := e.autoBulkTableRefForCapture([]int32{4, 5, 6})
	if err != nil || !ok {
		t.Fatalf("autoBulkTableRefForCapture = (%v, %v)", ok, err)
	}
	e.goFuncs["sumTable"] = func(arg interface{}) interface{} {
		proxy, ok := arg.(*GoHandleProxy)
		if !ok {
			return fmt.Sprintf("bad arg %T", arg)
		}
		total := int32(0)
		for _, value := range proxy.Values() {
			total += value.(int32)
		}
		return total
	}
	got, err := e.callGoFunc("sumTable", []interface{}{ref}, "total")
	if err != nil {
		t.Fatalf("callGoFunc sumTable: %v", err)
	}
	if got != int32(15) {
		t.Fatalf("sumTable = %#v, want 15", got)
	}
	bound, ok := e.getBinding("total")
	if !ok || bound != int32(15) {
		t.Fatalf("bound total = %#v, %v; want 15,true", bound, ok)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["iterate"] < 1 {
		t.Fatalf("Go function did not consume table through batched iteration: %+v", stats)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestSpawnBindAndSelectiveWait(t *testing.T) {
	e, _ := makeExecutor()
	e.goFuncs["identity"] = func(n interface{}) interface{} {
		return n
	}

	if _, err := e.executeOp(&Op{OpType: "spawn", Runtime: "go", Code: "identity(1)", Bind: "h1"}); err != nil {
		t.Fatalf("spawn h1: %v", err)
	}
	if _, err := e.executeOp(&Op{OpType: "spawn", Runtime: "go", Code: "identity(2)", Bind: "h2"}); err != nil {
		t.Fatalf("spawn h2: %v", err)
	}
	val, err := e.executeOp(&Op{OpType: "eval", Runtime: "go", Code: "wait(h1, h2)", Bind: "joined"})
	if err != nil {
		t.Fatalf("wait handles: %v", err)
	}
	results, ok := val.([]interface{})
	if !ok {
		t.Fatalf("wait(h1, h2) = %T, want []interface{}", val)
	}
	if len(results) != 2 || results[0] != 1 || results[1] != 2 {
		t.Fatalf("wait(h1, h2) = %#v, want [1 2]", results)
	}
	if bound, _ := e.getBinding("joined"); bound == nil {
		t.Fatal("joined was not bound")
	}
}

func TestGlobalWaitReturnsSpawnCount(t *testing.T) {
	e, _ := makeExecutor()
	e.goFuncs["identity"] = func(n interface{}) interface{} {
		return n
	}

	if _, err := e.executeOp(&Op{OpType: "spawn", Runtime: "go", Code: "identity(1)", Bind: "h1"}); err != nil {
		t.Fatalf("spawn h1: %v", err)
	}
	if _, err := e.executeOp(&Op{OpType: "spawn", Runtime: "go", Code: "identity(2)", Bind: "h2"}); err != nil {
		t.Fatalf("spawn h2: %v", err)
	}
	val, err := e.executeOp(&Op{OpType: "eval", Runtime: "go", Code: "wait()"})
	if err != nil {
		t.Fatalf("wait all: %v", err)
	}
	if val != 2 {
		t.Fatalf("wait() = %v, want 2", val)
	}
}

// --- normalizeArgs tests ---

func TestNormalizeArgs(t *testing.T) {
	args := []interface{}{float64(42), float64(3.14), "7", "hello", RuntimeRef{Value: int(5)}}
	normalized := normalizeArgs(args)
	if normalized[0] != 42 {
		t.Errorf("[0] = %v (%T), want 42 (int)", normalized[0], normalized[0])
	}
	if normalized[1] != 3.14 {
		t.Errorf("[1] = %v, want 3.14", normalized[1])
	}
	if normalized[2] != 7 {
		t.Errorf("[2] = %v (%T), want 7 (int)", normalized[2], normalized[2])
	}
	if normalized[3] != "hello" {
		t.Errorf("[3] = %v, want hello", normalized[3])
	}
	if normalized[4] != 5 {
		t.Errorf("[4] = %v, want 5", normalized[4])
	}
}

func TestDecodeRuntimeRefArgsPreservesComplexStubArguments(t *testing.T) {
	args := decodeRuntimeRefArgs([]interface{}{
		map[string]interface{}{
			"__omnivm_runtime_ref__": true,
			"runtime":                "python",
			"var":                    `__omnivm_arg_refs["arg_1"]`,
		},
		map[string]interface{}{
			"nested": map[string]interface{}{
				"__omnivm_runtime_ref__": true,
				"runtime":                "javascript",
				"var":                    `__omnivm_arg_refs["arg_2"]`,
			},
		},
	})

	ref, ok := args[0].(RuntimeRef)
	if !ok || ref.Runtime != "python" || ref.VarName != `__omnivm_arg_refs["arg_1"]` {
		t.Fatalf("top-level runtime ref arg = %#v", args[0])
	}
	nested := args[1].(map[string]interface{})["nested"].(RuntimeRef)
	if nested.Runtime != "javascript" || nested.VarName != `__omnivm_arg_refs["arg_2"]` {
		t.Fatalf("nested runtime ref arg = %#v", nested)
	}
}

func TestNormalizeGoArgMaterializesRuntimeRefAsHandleProxy(t *testing.T) {
	e, _ := makeExecutor("python", "go")
	arg := e.normalizeGoArg(RuntimeRef{
		Runtime: "python",
		VarName: `__omnivm_arg_refs["arg_1"]`,
	})
	proxy, ok := arg.(*GoHandleProxy)
	if !ok {
		t.Fatalf("runtime ref Go arg = %T, want *GoHandleProxy", arg)
	}
	if proxy.Kind() != "resource" || proxy.Runtime() != "python" || proxy.ResourceKind() != "runtime_ref" {
		t.Fatalf("unexpected runtime ref proxy: %#v", proxy.AsMap())
	}
	handleStats := e.handleTable.Stats(time.Now())
	boundaryStats := e.BoundaryStats()
	if handleStats.Live != 1 || boundaryStats.ResourceProxyCaptures != 1 {
		t.Fatalf("runtime ref proxy stats = handles %+v boundary %+v, want one live resource proxy", handleStats, boundaryStats)
	}
}

func TestNormalizeGoArgMaterializesRuntimeRefBufferAsTableProxy(t *testing.T) {
	e, mocks := makeExecutor("python", "go")
	payload := []int32{4, 5, 6}
	mocks["python"].exportFn = func(name, expr string) (pkg.ExportedBuffer, bool, error) {
		if expr != `__omnivm_arg_refs["arg_1"]` {
			t.Fatalf("ExportBuffer expr = %q", expr)
		}
		view, ok := bulkCaptureViewForValue(payload)
		if !ok {
			t.Fatal("bulkCaptureViewForValue rejected int32 payload")
		}
		if _, err := arrow.GlobalStore().SetExternalWithMetadata(name, view.ptr, view.bytesLen, arrow.BufferMetadata{
			Dtype:     view.dtype,
			Format:    view.format,
			Shape:     []int64{view.elements},
			ReadOnly:  true,
			Ownership: "producer",
		}, view.release); err != nil {
			return pkg.ExportedBuffer{}, false, err
		}
		return pkg.ExportedBuffer{
			Name:        name,
			Dtype:       view.dtype,
			ArrowFormat: view.format,
			Elements:    view.elements,
			Shape:       []int64{view.elements},
			ReadOnly:    true,
		}, true, nil
	}
	arg := e.normalizeGoArg(RuntimeRef{
		Runtime: "python",
		VarName: `__omnivm_arg_refs["arg_1"]`,
	})
	proxy, ok := arg.(*GoHandleProxy)
	if !ok {
		t.Fatalf("runtime ref Go arg = %T, want *GoHandleProxy", arg)
	}
	if proxy.Kind() != "table" || proxy.Len() != 3 || proxy.Index(2) != int32(6) {
		t.Fatalf("runtime ref table proxy = kind %q len %d index2 %#v", proxy.Kind(), proxy.Len(), proxy.Index(2))
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("runtime ref buffer Go arg stats = %+v, want Arrow table without proxy fallback", stats)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
}

func TestLocalComplexCaptureBecomesResourceHandle(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	data := map[string]interface{}{
		"path":  "/local",
		"items": []interface{}{"one", "two"},
	}
	e.setBinding("req", data)

	code, err := e.wrapWithCaptures("javascript", "use(req)", map[string]string{"req": "req"})
	if err != nil {
		t.Fatalf("wrapWithCaptures: %v", err)
	}
	if !strings.Contains(code, `"__omnivm_resource__":true`) || !strings.Contains(code, `"runtime":"go"`) {
		t.Fatalf("local complex capture should inject a Go resource descriptor, got %q", code)
	}
	if strings.Contains(code, `"path":"/local"`) || strings.Contains(code, `"items":["`) {
		t.Fatalf("local complex capture should not cross as JSON payload, got %q", code)
	}
	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("local complex capture stats = %+v, want resource proxy without JSON fallback", stats)
	}
	var id handles.ID
	for resourceID := range e.resources {
		id = resourceID
	}
	if id == 0 {
		t.Fatalf("local complex capture did not register a resource handle")
	}
	got, ok, err := e.handleProperty(id, "path")
	if err != nil || !ok || got != "/local" {
		t.Fatalf("local complex proxy path = (%#v,%v,%v), want /local,true,nil", got, ok, err)
	}
	result, err := e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"status","value":"accepted"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_set: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("handle_set envelope = %#v, want true", env)
	}
	if data["status"] != "accepted" {
		t.Fatalf("local complex proxy mutation did not preserve identity: %#v", data)
	}
}

func TestHandleSetTableLengthRejectsWithTensorContext(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	name := fmt.Sprintf("test-tensor-length-%d", time.Now().UnixNano())
	payload := []byte{0, 0, 1, 0, 2, 0, 3, 0, 4, 0, 5, 0}
	if _, err := arrow.GlobalStore().SetWithMetadata(name, payload, arrow.BufferMetadata{
		Dtype:   arrow.DtypeI16,
		Format:  "s",
		Shape:   []int64{3, 2},
		Strides: []int64{4, 2},
	}); err != nil {
		t.Fatalf("SetWithMetadata: %v", err)
	}
	defer func() {
		arrow.BufRelease(name)
		arrow.GlobalStore().DrainDeferred()
	}()

	dtype := int32(arrow.DtypeI16)
	table := &TableRef{
		Runtime:   "python",
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: "s",
			Buffer:      name,
			Shape:       []int64{3, 2},
			Strides:     []int64{4, 2},
		},
		Value: name,
	}
	id, err := e.ensureHandleTable().Register(table, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "table",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register table: %v", err)
	}
	table.ID = id
	defer e.ensureHandleTable().ReleaseAllRefs(id)

	_, err = e.handleSet(id, "length", float64(1))
	if err == nil {
		t.Fatal("table length write should fail")
	}
	message := err.Error()
	for _, want := range []string{
		"cannot resize fixed-size table proxy",
		"runtime=python",
		"kind=table",
		fmt.Sprintf("id=%d", id),
		"buffer=\"" + name + "\"",
		"dtype=6",
		"format=\"s\"",
		"shape=[3 2]",
		"strides=[4 2]",
		"length=3",
		"requested=1",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("table length write error %q missing %q", message, want)
		}
	}
	length, ok, err := tableBufferLen(table)
	if err != nil || !ok || length != 3 {
		t.Fatalf("table length after rejected write = (%d,%v,%v), want 3,true,nil", length, ok, err)
	}
}

func TestHandleResultTypedSliceBecomesArrowTable(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	data := map[string]interface{}{
		"scores": []int32{10, 20, 30},
	}
	ref, ok, err := e.autoResourceRefForCapture(data)
	if err != nil || !ok {
		t.Fatalf("autoResourceRefForCapture = (%v, %v)", ok, err)
	}

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `,"key":"scores"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get scores: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	value, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || value["__omnivm_table__"] != true {
		t.Fatalf("typed slice handle result should cross as table descriptor, got %#v", env)
	}
	if value["runtime"] != "go" || value["format"] != "arrow_c_data" {
		t.Fatalf("typed slice table descriptor = %#v, want go arrow_c_data", value)
	}

	tableID, ok := bridgeMarkerHandleID(value)
	if !ok {
		t.Fatalf("typed slice table descriptor missing handle id: %#v", value)
	}
	indexed, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall table index: %v", err)
	}
	indexEnv := decodeResultEnvelopeForTest(t, indexed)
	if indexEnv.Kind != "number" || indexEnv.Value != float64(20) {
		t.Fatalf("typed slice table index envelope = %#v, want 20", indexEnv)
	}

	handleStats := e.ensureHandleTable().Stats(time.Now())
	if handleStats.ReferenceEdges != 1 || handleStats.ReferenceEdgesByKind["property"] != 1 {
		t.Fatalf("typed slice table result should be referenced from parent handle: %+v", handleStats)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("typed slice handle result stats = %+v, want Arrow table without proxy/JSON fallback", stats)
	}
}

// --- marshalResult tests ---

func decodeResultEnvelopeForTest(t *testing.T, raw string) bridgeResultEnvelope {
	t.Helper()
	var env bridgeResultEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("result envelope is not JSON: %v; raw=%q", err, raw)
	}
	if !env.Marker {
		t.Fatalf("result envelope missing marker: %#v", env)
	}
	return env
}

func jsonEqual(a, b interface{}) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	return aerr == nil && berr == nil && string(ab) == string(bb)
}

func TestMarshalResult(t *testing.T) {
	cases := []struct {
		val      interface{}
		want     interface{}
		wantKind string
	}{
		{nil, nil, "null"},
		{42, float64(42), "number"},
		{"hello", "hello", "string"},
		{map[string]interface{}{"ok": true}, map[string]interface{}{"ok": true}, "json"},
		{RuntimeRef{Value: "unwrapped"}, "unwrapped", "string"},
		{RuntimeRef{Value: nil}, nil, "null"},
	}
	for _, tc := range cases {
		got, err := marshalResult(tc.val)
		if err != nil {
			t.Errorf("marshalResult(%v) error: %v", tc.val, err)
			continue
		}
		env := decodeResultEnvelopeForTest(t, got)
		if env.Kind != tc.wantKind {
			t.Errorf("marshalResult(%v) kind = %q, want %q", tc.val, env.Kind, tc.wantKind)
		}
		if !jsonEqual(env.Value, tc.want) {
			t.Errorf("marshalResult(%v) value = %#v, want %#v", tc.val, env.Value, tc.want)
		}
	}
}

func TestMarshalResultConvertsHandlesToProxyDescriptors(t *testing.T) {
	got, err := marshalResult(&ResourceRef{ID: 7, Runtime: "python", Kind: "request"})
	if err != nil {
		t.Fatalf("marshalResult resource: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, got)
	value, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || value["__omnivm_resource__"] != true || value["id"] != float64(7) {
		t.Fatalf("resource result envelope = %#v", env)
	}

	got, err = marshalResult(&TableRef{ID: 9, Runtime: "python", Format: "arrow_c_data", Ownership: "borrowed"})
	if err != nil {
		t.Fatalf("marshalResult table: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, got)
	value, ok = env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || value["__omnivm_table__"] != true || value["format"] != "arrow_c_data" {
		t.Fatalf("table result envelope = %#v", env)
	}
}

func TestMarshalResultRejectsUnclassifiedComplexValues(t *testing.T) {
	_, err := marshalResult(map[string]interface{}{"callback": func() {}})
	if err == nil {
		t.Fatal("expected non-marshalable bridge result to fail instead of stringifying")
	}
	if !strings.Contains(err.Error(), "boundary classification") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMarshalForCaptureRejectsUnclassifiedComplexValues(t *testing.T) {
	_, err := marshalForCapture(map[string]interface{}{"items": []interface{}{1, 2, 3}})
	if err == nil {
		t.Fatal("expected direct complex capture marshaling to fail instead of JSON-copying")
	}
	if !strings.Contains(err.Error(), "boundary classification") {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := marshalForCapture([]byte("binary")); err == nil {
		t.Fatal("expected direct byte-slice marshaling to fail instead of base64-copying")
	}
}

func TestEvalExplicitCaptureReportsMissingBinding(t *testing.T) {
	e, mocks := makeExecutor("javascript")

	_, err := e.opEval(&Op{
		Runtime:  "javascript",
		Code:     "payload.items.length",
		Captures: map[string]string{"payload": "missing_payload"},
	})
	if err == nil {
		t.Fatal("expected explicit eval capture to fail instead of silently dropping the missing capture")
	}
	if !strings.Contains(err.Error(), `eval captures [javascript]: capture "payload": undefined binding "missing_payload"`) {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mocks["javascript"].evalCalls) != 1 || !strings.Contains(mocks["javascript"].evalCalls[0], "hasOwnProperty.call") {
		t.Fatalf("eval should only run the runtime-global capture probe, calls=%q", mocks["javascript"].evalCalls)
	}
}

func TestMarshalReturnResultPreservesComplexRuntimeRefAsProxy(t *testing.T) {
	e, _ := makeExecutor("python")
	got, err := e.marshalReturnResult(RuntimeRef{
		Runtime: "python",
		VarName: `__omnivm_arg_refs["arg_1"]`,
	})
	if err != nil {
		t.Fatalf("marshalReturnResult runtime ref: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, got)
	value, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || value["__omnivm_resource__"] != true || value["runtime"] != "python" || value["kind"] != "runtime_ref" {
		t.Fatalf("runtime ref return envelope = %#v", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 1 || stats.EscapedByRuntime["python"] != 1 {
		t.Fatalf("runtime ref return should register an escaped proxy handle, stats=%+v", stats)
	}
}

func TestMarshalReturnResultExportsRuntimeRefBufferAsArrowTable(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	before := arrow.GlobalStore().Stats()
	payload := []byte("abcdef")
	mocks["python"].evalFn = func(code string) pkg.Result {
		t.Fatalf("runtime-ref return should not JSON-serialize source runtime, got eval %q", code)
		return pkg.Result{}
	}
	mocks["python"].exportFn = func(name, expr string) (pkg.ExportedBuffer, bool, error) {
		if expr != "payload" {
			t.Fatalf("ExportBuffer expr = %q, want payload", expr)
		}
		if _, err := arrow.GlobalStore().SetWithMetadata(name, payload, arrow.BufferMetadata{
			Dtype:     arrow.DtypeBytes,
			Format:    "C",
			Shape:     []int64{int64(len(payload))},
			ReadOnly:  true,
			Ownership: "producer",
		}); err != nil {
			return pkg.ExportedBuffer{}, false, err
		}
		return pkg.ExportedBuffer{
			Name:        name,
			Dtype:       arrow.DtypeBytes,
			ArrowFormat: "C",
			Elements:    int64(len(payload)),
			Shape:       []int64{int64(len(payload))},
			ReadOnly:    true,
		}, true, nil
	}

	got, err := e.marshalReturnResult(RuntimeRef{
		Runtime: "python",
		VarName: "payload",
		Opaque:  true,
	})
	if err != nil {
		t.Fatalf("marshalReturnResult runtime-ref buffer: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, got)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_table__"] != true || descriptor["format"] != "arrow_c_data" {
		t.Fatalf("runtime-ref buffer return envelope = %#v, want Arrow table", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("table id: %v", err)
	}
	indexed, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":2}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	indexEnv := decodeResultEnvelopeForTest(t, indexed)
	if indexEnv.Kind != "number" || indexEnv.Value != float64(payload[2]) {
		t.Fatalf("runtime-ref buffer index = %#v, want %d", indexEnv, payload[2])
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("runtime-ref buffer return stats = %+v, want Arrow table without proxy or JSON fallback", stats)
	}
	if len(mocks["python"].exports) != 1 {
		t.Fatalf("ExportBuffer calls = %d, want 1", len(mocks["python"].exports))
	}
	if err := e.ensureHandleTable().Release(id); err != nil {
		t.Fatalf("release returned table: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("returned runtime buffer was not released: before=%+v after=%+v", before, released)
	}
}

func TestMarshalReturnResultExportsLocalComplexAsTransferProxy(t *testing.T) {
	e, _ := makeExecutor("python")
	payload := map[string]interface{}{
		"name":  "initial",
		"items": []interface{}{"a", "b"},
	}

	got, err := e.marshalReturnResult(payload)
	if err != nil {
		t.Fatalf("marshalReturnResult local complex: %v", err)
	}
	if strings.Contains(got, `"name":"initial"`) || strings.Contains(got, `"items":["`) {
		t.Fatalf("returned local complex value should not cross as JSON payload: %s", got)
	}
	env := decodeResultEnvelopeForTest(t, got)
	descriptor, ok := env.Value.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true || descriptor["runtime"] != "go" || descriptor["transfer"] != true {
		t.Fatalf("returned local complex descriptor = %#v, want transfer-marked Go resource proxy", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	path, ok, err := e.handleProperty(id, "name")
	if err != nil || !ok || path != "initial" {
		t.Fatalf("returned local complex property = (%#v,%v,%v), want initial,true,nil", path, ok, err)
	}
	if _, err := e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"name","value":"changed"}`); err != nil {
		t.Fatalf("HandleCall handle_set: %v", err)
	}
	if payload["name"] != "changed" {
		t.Fatalf("returned proxy mutation did not preserve local identity: %#v", payload)
	}
	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("local complex return stats = %+v, want resource proxy without JSON fallback", stats)
	}
	if _, err := e.HandleCall(`{"op":"handle_adopt","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err != nil {
		t.Fatalf("HandleCall handle_adopt: %v", err)
	}
	if _, err := e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err != nil {
		t.Fatalf("HandleCall handle_release_finalizer: %v", err)
	}
	if err := e.ensureHandleTable().DrainFinalizerReleases(0); err != nil {
		t.Fatalf("DrainFinalizerReleases: %v", err)
	}
	if _, live := e.ensureHandleTable().Get(id); live {
		t.Fatalf("adopted returned local complex handle %d remained live after proxy finalizer", id)
	}
}

func TestHandleCallReturnsComplexRuntimeRefProxyPastFunctionScope(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.funcs["echo"] = &FuncDef{
		Name:   "echo",
		Params: []*Param{{Name: "value"}},
		Body: []*Op{
			{OpType: "return", Value: &ValueExpr{Kind: "ref", Name: "value"}},
		},
	}

	result, err := e.HandleCall(`{"func":"echo","args":[{"__omnivm_runtime_ref__":true,"runtime":"javascript","var":"__omnivm_arg_refs[\"arg_1\"]"}]}`)
	if err != nil {
		t.Fatalf("HandleCall echo: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	value, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || value["__omnivm_resource__"] != true || value["runtime"] != "javascript" || value["kind"] != "runtime_ref" {
		t.Fatalf("echo runtime ref envelope = %#v", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 1 || stats.EscapedByRuntime["javascript"] != 1 || stats.ScopeReleases != 0 {
		t.Fatalf("returned proxy should survive function scope as escaped handle, stats=%+v", stats)
	}
}

// --- HandleCall tests ---

func TestHandleCallUndefined(t *testing.T) {
	e, _ := makeExecutor("javascript")
	_, err := e.HandleCall(`{"func": "nope", "args": []}`)
	if err == nil {
		t.Error("expected error for undefined function")
	}
}

func TestHandleCallInvalidJSON(t *testing.T) {
	e, _ := makeExecutor("javascript")
	_, err := e.HandleCall("not json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestHandleCallInternalHandleAccessRecordsStats(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	_, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)

	result, err := e.HandleCall(`{"op":"handle_access","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `,"kind":"property"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_access: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" {
		t.Fatalf("handle_access envelope = %#v", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccesses == 0 || stats.HandleAccessesByKind["property"] != 1 {
		t.Fatalf("handle access stats not recorded: %+v", stats)
	}
}

func TestHandleCallInternalHandleGetReadsGenericResourceValue(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	_, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value: &ValueExpr{Kind: "literal", Value: map[string]interface{}{
			"path":   "/orders/42",
			"method": "POST",
			"headers": map[string]interface{}{
				"x-request-id": "abc",
			},
		}},
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `,"key":"path"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "/orders/42" {
		t.Fatalf("handle_get envelope = %#v, want path", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["property"] == 0 {
		t.Fatalf("handle_get did not record property access: %+v", stats)
	}
}

type bridgeObjectForTest struct {
	Name string `json:"name"`
}

func (o *bridgeObjectForTest) Greet(prefix string) string {
	return prefix + " " + o.Name
}

type runtimeRefArgReceiver struct {
	Last interface{}
}

func (r *runtimeRefArgReceiver) Take(arg interface{}) string {
	r.Last = arg
	return "ok"
}

func (r *runtimeRefArgReceiver) TakeAndFail(arg interface{}) (string, error) {
	r.Last = arg
	return "", errors.New("boom")
}

func TestHandleCallInternalHandleIndexSetAndCallGenericResourceValue(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	_, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value: &ValueExpr{Kind: "literal", Value: map[string]interface{}{
			"path":  "/orders/42",
			"items": []interface{}{"first", "second"},
		}},
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)

	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `,"value":"items"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	itemsProxy, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || itemsProxy["__omnivm_resource__"] != true || itemsProxy["kind"] != "sequence" {
		t.Fatalf("handle_index envelope = %#v, want resource proxy for items", env)
	}
	itemsID, err := bridgeHandleID(itemsProxy["id"])
	if err != nil {
		t.Fatalf("items proxy id: %v", err)
	}
	result, err = e.HandleCall(`{"op":"handle_len","id":` + strconv.FormatUint(uint64(itemsID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_len: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(2) {
		t.Fatalf("nested handle_len envelope = %#v, want 2", env)
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" || !jsonEqual(env.Value, []interface{}{"first", "second"}) {
		t.Fatalf("nested handle_iter envelope = %#v, want items", env)
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"mode":"keys"}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_iter keys: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" || !jsonEqual(env.Value, []interface{}{float64(0), float64(1)}) {
		t.Fatalf("nested handle_iter keys envelope = %#v, want indexes", env)
	}
	result, err = e.HandleCall(`{"op":"handle_contains","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `,"value":"path"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_contains: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("handle_contains envelope = %#v, want true", env)
	}
	result, err = e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_index: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "second" {
		t.Fatalf("nested handle_index envelope = %#v, want second item", env)
	}
	result, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"key":"0","value":"updated"}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_set index: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("nested handle_set index envelope = %#v, want true", env)
	}
	result, err = e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"value":0}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_index after set: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "updated" {
		t.Fatalf("nested handle_index after set envelope = %#v, want updated item", env)
	}
	result, err = e.HandleCall(`{"op":"handle_contains","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"value":"0"}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_contains index: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("nested handle_contains index envelope = %#v, want true", env)
	}
	result, err = e.HandleCall(`{"op":"handle_contains","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"value":"updated"}`)
	if err != nil {
		t.Fatalf("HandleCall nested handle_contains value: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("nested handle_contains value envelope = %#v, want true", env)
	}

	result, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `,"key":"status","value":"accepted"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_set: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("handle_set envelope = %#v, want true", env)
	}
	status, ok, err := e.handleProperty(ref.ID, "status")
	if err != nil || !ok || status != "accepted" {
		t.Fatalf("handle_set did not mutate resource map: value=%#v ok=%v err=%v", status, ok, err)
	}

	_, err = e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "go",
		Bind:    "obj",
		Kind:    "object",
		Value:   &ValueExpr{Kind: "literal", Value: &bridgeObjectForTest{Name: "Ada"}},
	})
	if err != nil {
		t.Fatalf("resource open object: %v", err)
	}
	objVal, _ := e.getBinding("obj")
	obj := objVal.(*ResourceRef)
	result, err = e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(obj.ID), 10) + `,"key":"Greet"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get callable: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" || !jsonEqual(env.Value, map[string]interface{}{"__omnivm_callable__": true, "key": "Greet"}) {
		t.Fatalf("handle_get callable envelope = %#v, want callable descriptor", env)
	}

	result, err = e.HandleCall(`{"op":"handle_call","id":` + strconv.FormatUint(uint64(obj.ID), 10) + `,"key":"Greet","args":["hello"]}`)
	if err != nil {
		t.Fatalf("HandleCall handle_call: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "hello Ada" {
		t.Fatalf("handle_call envelope = %#v, want greeting", env)
	}

	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["index"] < 2 || stats.HandleAccessesByKind["contains"] == 0 || stats.HandleAccessesByKind["length"] == 0 || stats.HandleAccessesByKind["iterate"] == 0 || stats.HandleAccessesByKind["mutation"] == 0 || stats.HandleAccessesByKind["call"] == 0 {
		t.Fatalf("bridge ops did not record access kinds: %+v", stats)
	}
	if stats.Live < 3 || stats.ReferenceEdges == 0 {
		t.Fatalf("bridge result proxy did not preserve child handle/reference state: %+v", stats)
	}
}

func TestHandleCallInternalOpsDecodeRuntimeRefArguments(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	target := &runtimeRefArgReceiver{}
	id, err := e.ensureHandleTable().Register(target, handles.RegisterOptions{
		Runtime: "go",
		Kind:    "object",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register receiver: %v", err)
	}

	result, err := e.HandleCall(`{"op":"handle_call","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"Take","args":[{"__omnivm_runtime_ref__":true,"runtime":"python","var":"__omnivm_arg_refs[\"arg_1\"]"}]}`)
	if err != nil {
		t.Fatalf("HandleCall handle_call runtime ref: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "ok" {
		t.Fatalf("handle_call runtime ref envelope = %#v, want ok", env)
	}
	ref, ok := target.Last.(RuntimeRef)
	if !ok || ref.Runtime != "python" || ref.VarName != `__omnivm_arg_refs["arg_1"]` {
		t.Fatalf("decoded handle_call arg = %#v", target.Last)
	}

	store := map[string]interface{}{}
	storeID, err := e.ensureHandleTable().Register(store, handles.RegisterOptions{
		Runtime: "go",
		Kind:    "map",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register map: %v", err)
	}
	_, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(storeID), 10) + `,"key":"payload","value":{"__omnivm_runtime_ref__":true,"runtime":"javascript","var":"__omnivm_arg_refs[\"arg_2\"]"}}`)
	if err != nil {
		t.Fatalf("HandleCall handle_set runtime ref: %v", err)
	}
	ref, ok = store["payload"].(RuntimeRef)
	if !ok || ref.Runtime != "javascript" || ref.VarName != `__omnivm_arg_refs["arg_2"]` {
		t.Fatalf("decoded handle_set value = %#v", store["payload"])
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 1 {
		t.Fatalf("handle_set runtime ref did not record mutation edge: %+v", stats)
	}
}

func TestHandleCallRecordsComplexArgumentReferenceEdges(t *testing.T) {
	e, _ := makeExecutor("go", "javascript")
	target := &runtimeRefArgReceiver{}
	targetID, err := e.ensureHandleTable().Register(target, handles.RegisterOptions{
		Runtime: "go",
		Kind:    "object",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register receiver: %v", err)
	}

	child := &ResourceRef{Runtime: "javascript", Kind: "object"}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child
	nestedChild := &ResourceRef{Runtime: "javascript", Kind: "object"}
	nestedChildID, err := e.ensureHandleTable().Register(nestedChild, handles.RegisterOptions{
		Runtime: nestedChild.Runtime,
		Kind:    nestedChild.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register nested child: %v", err)
	}
	nestedChild.ID = nestedChildID
	e.resources[nestedChildID] = nestedChild

	req := map[string]interface{}{
		"op":  "handle_call",
		"id":  uint64(targetID),
		"key": "Take",
		"args": []interface{}{
			map[string]interface{}{
				"direct": resourceProxyValue(child),
				"nested": []interface{}{resourceProxyValue(nestedChild)},
			},
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if _, err := e.HandleCall(string(data)); err != nil {
		t.Fatalf("HandleCall handle_call complex arg: %v", err)
	}

	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["call_arg"] != 2 || stats.ReferenceEdgesByRuntime["go->javascript"] != 2 {
		t.Fatalf("handle_call complex arg did not record call_arg edge: %+v", stats)
	}

	failReq := map[string]interface{}{
		"op":   "handle_call",
		"id":   uint64(targetID),
		"key":  "TakeAndFail",
		"args": []interface{}{resourceProxyValue(child)},
	}
	data, err = json.Marshal(failReq)
	if err != nil {
		t.Fatalf("marshal failing request: %v", err)
	}
	if _, err := e.HandleCall(string(data)); err == nil {
		t.Fatal("HandleCall TakeAndFail succeeded, want error")
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["call_arg"] != 2 || stats.ReferenceEdgesByRuntime["go->javascript"] != 2 {
		t.Fatalf("failed handle_call should keep retained arg observable without duplicating edges: %+v", stats)
	}

	if err := e.ensureHandleTable().ReleaseScope(e.currentHandleScope()); err != nil {
		t.Fatalf("release scope: %v", err)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdges != 0 || stats.Live != 0 {
		t.Fatalf("scope release did not bound call_arg edge lifetime: %+v", stats)
	}
}

func TestHandleSetRecordsProxyMutationEdgesAndCycles(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	left := &ResourceRef{
		Runtime: "python",
		Kind:    "object",
		Value:   map[string]interface{}{},
	}
	right := &ResourceRef{
		Runtime: "javascript",
		Kind:    "object",
		Value:   map[string]interface{}{},
	}
	leftID, err := e.ensureHandleTable().Register(left, handles.RegisterOptions{
		Runtime: left.Runtime,
		Kind:    left.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register left: %v", err)
	}
	rightID, err := e.ensureHandleTable().Register(right, handles.RegisterOptions{
		Runtime: right.Runtime,
		Kind:    right.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register right: %v", err)
	}
	left.ID = leftID
	right.ID = rightID
	e.resources[leftID] = left
	e.resources[rightID] = right

	rightJSON, err := json.Marshal(resourceProxyValue(right))
	if err != nil {
		t.Fatalf("marshal right descriptor: %v", err)
	}
	_, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(leftID), 10) + `,"key":"peer","value":` + string(rightJSON) + `}`)
	if err != nil {
		t.Fatalf("HandleCall set left.peer: %v", err)
	}
	leftJSON, err := json.Marshal(resourceProxyValue(left))
	if err != nil {
		t.Fatalf("marshal left descriptor: %v", err)
	}
	_, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(rightID), 10) + `,"key":"peer","value":` + string(leftJSON) + `}`)
	if err != nil {
		t.Fatalf("HandleCall set right.peer: %v", err)
	}

	stats := e.handleTable.Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 2 || stats.SuspectedCycles == 0 || stats.CyclicHandles < 2 || stats.LargestCycle < 2 || len(stats.CycleSample) < 2 {
		t.Fatalf("handle_set did not record mutation cycle: %+v", stats)
	}
}

func TestHandleSetDropsStaleMutationEdgesOnOverwrite(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	store := map[string]interface{}{}
	storeID, err := e.ensureHandleTable().Register(store, handles.RegisterOptions{
		Runtime: "go",
		Kind:    "map",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register store: %v", err)
	}
	child := &ResourceRef{Runtime: "javascript", Kind: "object", Value: map[string]interface{}{}}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child
	childJSON, err := json.Marshal(resourceProxyValue(child))
	if err != nil {
		t.Fatalf("marshal child descriptor: %v", err)
	}

	for _, key := range []string{"first", "second"} {
		_, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(storeID), 10) + `,"key":"` + key + `","value":` + string(childJSON) + `}`)
		if err != nil {
			t.Fatalf("HandleCall set %s: %v", key, err)
		}
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 1 {
		t.Fatalf("expected one coalesced mutation edge for shared child: %+v", stats)
	}

	_, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(storeID), 10) + `,"key":"first","value":"primitive"}`)
	if err != nil {
		t.Fatalf("HandleCall overwrite first: %v", err)
	}
	stats = e.handleTable.Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 1 {
		t.Fatalf("overwriting one of two references should keep edge: %+v", stats)
	}

	_, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(storeID), 10) + `,"key":"second","value":"primitive"}`)
	if err != nil {
		t.Fatalf("HandleCall overwrite second: %v", err)
	}
	stats = e.handleTable.Stats(time.Now())
	if stats.ReferenceEdges != 0 {
		t.Fatalf("overwriting final proxy reference should drop stale edge: %+v", stats)
	}
}

func TestGoHandleProxySetRecordsAndDropsMutationEdges(t *testing.T) {
	e, _ := makeExecutor("go", "python")
	parent := &ResourceRef{Runtime: "go", Kind: "object", Value: map[string]interface{}{}}
	child := &ResourceRef{Runtime: "python", Kind: "object", Value: map[string]interface{}{}}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	parent.ID = parentID
	child.ID = childID
	e.resources[parentID] = parent
	e.resources[childID] = child

	proxy, ok := e.normalizeGoArg(parent).(*GoHandleProxy)
	if !ok {
		t.Fatalf("normalizeGoArg parent = %T, want *GoHandleProxy", e.normalizeGoArg(parent))
	}
	childProxy, ok := e.normalizeGoArg(child).(*GoHandleProxy)
	if !ok {
		t.Fatalf("normalizeGoArg child = %T, want *GoHandleProxy", e.normalizeGoArg(child))
	}
	if !proxy.Set("peer", childProxy) {
		t.Fatal("GoHandleProxy.Set peer returned false")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 1 {
		t.Fatalf("GoHandleProxy.Set did not record mutation edge: %+v", stats)
	}
	if !proxy.Set("peer", "primitive") {
		t.Fatal("GoHandleProxy.Set primitive returned false")
	}
	stats = e.handleTable.Stats(time.Now())
	if stats.ReferenceEdges != 0 {
		t.Fatalf("GoHandleProxy.Set overwrite did not drop stale edge: %+v", stats)
	}
}

func TestHandleCallInternalHandleReferenceRecordsAndDropsEdge(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	for _, bind := range []string{"left", "right"} {
		if _, err := e.executeOp(&Op{
			OpType:  "resource",
			Action:  "open",
			Runtime: "python",
			Bind:    bind,
			Kind:    "object",
		}); err != nil {
			t.Fatalf("resource open %s: %v", bind, err)
		}
	}
	leftVal, _ := e.getBinding("left")
	rightVal, _ := e.getBinding("right")
	left := leftVal.(*ResourceRef)
	right := rightVal.(*ResourceRef)

	_, err := e.HandleCall(`{"op":"handle_reference","from":` + strconv.FormatUint(uint64(left.ID), 10) + `,"to":` + strconv.FormatUint(uint64(right.ID), 10) + `,"kind":"proxy"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_reference: %v", err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.ReferenceEdges != 1 || stats.ReferenceEdgesByKind["proxy"] != 1 {
		t.Fatalf("reference edge stats not recorded: %+v", stats)
	}

	_, err = e.HandleCall(`{"op":"handle_drop_reference","from":` + strconv.FormatUint(uint64(left.ID), 10) + `,"to":` + strconv.FormatUint(uint64(right.ID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_drop_reference: %v", err)
	}
	stats = e.handleTable.Stats(time.Now())
	if stats.ReferenceEdges != 0 {
		t.Fatalf("reference edge was not dropped: %+v", stats)
	}
}

func TestHandleCallInternalHandleReleaseFinalizerQueuesRelease(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	_, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)

	result, err := e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_release_finalizer: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("handle_release_finalizer envelope = %#v, want true", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.FinalizerQueued != 1 || stats.FinalizerQueueLen != 1 {
		t.Fatalf("finalizer release was not queued: %+v", stats)
	}
}

func TestHandleCallInternalHandleReleaseExplicitReleasesImmediately(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/explicit"}},
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)

	result, err := e.HandleCall(`{"op":"handle_release_explicit","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_release_explicit: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("handle_release_explicit envelope = %#v, want true", env)
	}
	if !ref.Closed {
		t.Fatal("explicit release did not close owner immediately")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.ExplicitReleases != 1 || stats.FinalizerQueued != 0 || stats.FinalizerQueueLen != 0 {
		t.Fatalf("explicit release stats = %+v, want one explicit release and no finalizer queue", stats)
	}
}

func TestHandleRetainProtectsScopeOwnerFromProxyFinalizer(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	_, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/retained"}},
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)

	result, err := e.HandleCall(`{"op":"handle_retain","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_retain: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != true {
		t.Fatalf("handle_retain envelope = %#v, want true", env)
	}
	if _, err := e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`); err != nil {
		t.Fatalf("HandleCall handle_release_finalizer: %v", err)
	}
	if err := e.ensureHandleTable().DrainFinalizerReleases(0); err != nil {
		t.Fatalf("DrainFinalizerReleases: %v", err)
	}
	if _, ok := e.ensureHandleTable().Get(ref.ID); !ok {
		t.Fatal("finalizer release consumed the scope owner reference")
	}
	if ref.Closed {
		t.Fatal("resource was closed by guest proxy finalizer while scope owner was still live")
	}
	if err := e.ensureHandleTable().Release(ref.ID); err != nil {
		t.Fatalf("release scope owner: %v", err)
	}
	if !ref.Closed {
		t.Fatal("resource should close when scope owner reference is released")
	}
}

func TestHandleAdoptTransfersReturnedProxyToFinalizerLifetime(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	_, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/adopted"}},
	})
	if err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)

	result, err := e.marshalReturnResult(ref)
	if err != nil {
		t.Fatalf("marshal return result: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true || descriptor["transfer"] != true {
		t.Fatalf("returned resource descriptor = %#v, want transfer-marked resource proxy", env)
	}
	entry, live := e.ensureHandleTable().Get(ref.ID)
	if !live || !entry.Escaped || entry.StrongRefs != 1 {
		t.Fatalf("returned handle entry = %+v live=%v, want escaped transfer reference only", entry, live)
	}

	adopted, err := e.HandleCall(`{"op":"handle_adopt","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_adopt: %v", err)
	}
	adoptedEnv := decodeResultEnvelopeForTest(t, adopted)
	if adoptedEnv.Kind != "bool" || adoptedEnv.Value != true {
		t.Fatalf("handle_adopt envelope = %#v, want true", adoptedEnv)
	}
	entry, live = e.ensureHandleTable().Get(ref.ID)
	if !live || entry.StrongRefs != 1 {
		t.Fatalf("adopted handle entry = %+v live=%v, want one finalizer-owned reference", entry, live)
	}
	if _, err := e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`); err != nil {
		t.Fatalf("HandleCall handle_release_finalizer: %v", err)
	}
	if err := e.ensureHandleTable().DrainFinalizerReleases(0); err != nil {
		t.Fatalf("DrainFinalizerReleases: %v", err)
	}
	if _, live := e.ensureHandleTable().Get(ref.ID); live {
		t.Fatalf("adopted returned handle %d remained live after proxy finalizer", ref.ID)
	}
	if !ref.Closed {
		t.Fatal("adopted returned resource should close when guest proxy finalizer releases it")
	}
}

func TestHandleCallGoFunc(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.goFuncs["double"] = func(n interface{}) interface{} {
		return n.(int) * 2
	}

	result, err := e.HandleCall(`{"func": "double", "args": [21]}`)
	if err != nil {
		t.Fatalf("HandleCall: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(42) {
		t.Errorf("HandleCall envelope = %#v, want number 42", env)
	}
}

func TestHandleCallGoFuncReturnsTypedSliceAsArrowTable(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.goFuncs["scores"] = func(arg interface{}) interface{} {
		return []int32{4, 5, 6}
	}

	result, err := e.HandleCall(`{"func": "scores", "args": []}`)
	if err != nil {
		t.Fatalf("HandleCall scores: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_table__"] != true || descriptor["format"] != "arrow_c_data" {
		t.Fatalf("Go typed slice bridge result = %#v, want Arrow table descriptor", env)
	}
	metadata, ok := descriptor["metadata"].(map[string]interface{})
	if !ok || metadata["dtype"] != float64(arrow.DtypeI32) || metadata["arrow_format"] != "i" {
		t.Fatalf("Go typed slice metadata = %#v, want int32 Arrow metadata", descriptor["metadata"])
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("table id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)
	indexed, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	indexEnv := decodeResultEnvelopeForTest(t, indexed)
	if indexEnv.Kind != "number" || indexEnv.Value != float64(5) {
		t.Fatalf("Go typed slice table index = %#v, want number 5", indexEnv)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("Go typed slice bridge stats = %+v, want Arrow table without JSON fallback", stats)
	}
}

func TestHandleCallGoFuncReturnsEmptyTypedSliceAsArrowTable(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.goFuncs["emptyScores"] = func(arg interface{}) interface{} {
		return []uint16{}
	}

	result, err := e.HandleCall(`{"func": "emptyScores", "args": []}`)
	if err != nil {
		t.Fatalf("HandleCall emptyScores: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_table__"] != true || descriptor["format"] != "arrow_c_data" {
		t.Fatalf("Go empty typed slice bridge result = %#v, want Arrow table descriptor", env)
	}
	metadata, ok := descriptor["metadata"].(map[string]interface{})
	shape, shapeOK := metadata["shape"].([]interface{})
	if !ok || metadata["dtype"] != float64(arrow.DtypeU16) || metadata["arrow_format"] != "S" || !shapeOK || len(shape) != 1 || shape[0] != float64(0) {
		t.Fatalf("Go empty typed slice metadata = %#v, want uint16 shape [0]", descriptor["metadata"])
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("table id: %v", err)
	}
	defer e.ensureHandleTable().Release(id)
	length, err := e.HandleCall(`{"op":"handle_len","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_len: %v", err)
	}
	lenEnv := decodeResultEnvelopeForTest(t, length)
	if lenEnv.Kind != "number" || lenEnv.Value != float64(0) {
		t.Fatalf("Go empty typed slice table len = %#v, want 0", lenEnv)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("Go empty typed slice bridge stats = %+v, want Arrow table without JSON fallback", stats)
	}
}

func TestHandleCallGoFuncReturnsNativeChannelAsStream(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.goFuncs["events"] = func(arg interface{}) interface{} {
		ch := make(chan interface{}, 2)
		ch <- "first"
		ch <- "second"
		close(ch)
		return ch
	}

	result, err := e.HandleCall(`{"func": "events", "args": []}`)
	if err != nil {
		t.Fatalf("HandleCall: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if !ok || descriptor["__omnivm_stream__"] != true || descriptor["kind"] != "channel" {
		t.Fatalf("Go channel bridge result = %#v, want stream descriptor", env.Value)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("stream id: %v", err)
	}
	for _, want := range []string{"first", "second"} {
		next, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
		if err != nil {
			t.Fatalf("HandleCall stream_next: %v", err)
		}
		nextEnv := decodeResultEnvelopeForTest(t, next)
		item, ok := nextEnv.Value.(map[string]interface{})
		if !ok || item["done"] == true || item["value"] != want {
			t.Fatalf("stream_next envelope = %#v, want %q", nextEnv, want)
		}
	}
	next, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_next done: %v", err)
	}
	nextEnv := decodeResultEnvelopeForTest(t, next)
	item, ok := nextEnv.Value.(map[string]interface{})
	if !ok || item["done"] != true {
		t.Fatalf("stream_next done envelope = %#v, want done", nextEnv)
	}
	stats := e.BoundaryStats()
	if stats.StreamProxyCaptures != 1 || stats.ChannelMaterializations != 0 {
		t.Fatalf("Go channel bridge stats = %+v, want stream proxy without materialization", stats)
	}
}

func TestHandleCallGoFuncReturnsComplexObjectAsIdentityProxy(t *testing.T) {
	e, _ := makeExecutor("javascript")
	store := map[string]interface{}{
		"path":  "/go-return",
		"items": []interface{}{"first", "second"},
	}
	e.goFuncs["request"] = func(arg interface{}) interface{} {
		return store
	}

	result, err := e.HandleCall(`{"func": "request", "args": []}`)
	if err != nil {
		t.Fatalf("HandleCall request: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_resource__"] != true || descriptor["runtime"] != "go" || descriptor["kind"] != "map" {
		t.Fatalf("Go complex object bridge result = %#v, want Go resource proxy descriptor", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("resource id: %v", err)
	}
	if strings.Contains(result, `"/go-return"`) || strings.Contains(result, `"first"`) {
		t.Fatalf("Go complex object should not be JSON-copied into descriptor: %s", result)
	}

	path, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"path"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get path: %v", err)
	}
	pathEnv := decodeResultEnvelopeForTest(t, path)
	if pathEnv.Kind != "string" || pathEnv.Value != "/go-return" {
		t.Fatalf("Go complex proxy path = %#v, want /go-return", pathEnv)
	}

	items, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"items"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get items: %v", err)
	}
	itemsEnv := decodeResultEnvelopeForTest(t, items)
	itemsDescriptor, ok := itemsEnv.Value.(map[string]interface{})
	if itemsEnv.Kind != "json" || !ok || itemsDescriptor["__omnivm_resource__"] != true || itemsDescriptor["kind"] != "sequence" {
		t.Fatalf("Go nested slice bridge result = %#v, want sequence proxy descriptor", itemsEnv)
	}
	itemsID, err := bridgeHandleID(itemsDescriptor["id"])
	if err != nil {
		t.Fatalf("items id: %v", err)
	}
	indexed, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index items: %v", err)
	}
	indexEnv := decodeResultEnvelopeForTest(t, indexed)
	if indexEnv.Kind != "string" || indexEnv.Value != "second" {
		t.Fatalf("Go nested sequence index = %#v, want second", indexEnv)
	}

	if _, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"status","value":"accepted"}`); err != nil {
		t.Fatalf("HandleCall handle_set status: %v", err)
	}
	if _, err = e.HandleCall(`{"op":"handle_set","id":` + strconv.FormatUint(uint64(itemsID), 10) + `,"key":"0","value":"changed"}`); err != nil {
		t.Fatalf("HandleCall handle_set items: %v", err)
	}
	if store["status"] != "accepted" || store["items"].([]interface{})[0] != "changed" {
		t.Fatalf("Go complex proxy mutation did not preserve identity: %#v", store)
	}

	again, err := e.HandleCall(`{"func": "request", "args": []}`)
	if err != nil {
		t.Fatalf("HandleCall request again: %v", err)
	}
	againEnv := decodeResultEnvelopeForTest(t, again)
	againDescriptor, ok := againEnv.Value.(map[string]interface{})
	if !ok {
		t.Fatalf("second Go complex bridge result = %#v, want descriptor", againEnv)
	}
	againID, err := bridgeHandleID(againDescriptor["id"])
	if err != nil {
		t.Fatalf("second resource id: %v", err)
	}
	if againID != id {
		t.Fatalf("Go complex identity cache returned handle %d, want existing handle %d", againID, id)
	}

	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures < 2 || stats.JSONFallbacks != 0 {
		t.Fatalf("Go complex bridge stats = %+v, want resource proxy without JSON fallback", stats)
	}
	handleStats := e.ensureHandleTable().Stats(time.Now())
	if handleStats.HandleAccessesByKind["property"] == 0 || handleStats.HandleAccessesByKind["index"] == 0 || handleStats.HandleAccessesByKind["mutation"] == 0 {
		t.Fatalf("Go complex proxy did not record access kinds: %+v", handleStats)
	}
}

func TestHandleCallFuncDef(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.funcs["greet"] = &FuncDef{
		Name:   "greet",
		Params: []*Param{{Name: "name"}},
		Body: []*Op{
			{OpType: "return", Value: &ValueExpr{Kind: "ref", Name: "name"}},
		},
	}

	result, err := e.HandleCall(`{"func": "greet", "args": ["world"]}`)
	if err != nil {
		t.Fatalf("HandleCall: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "world" {
		t.Errorf("HandleCall envelope = %#v, want string world", env)
	}
}

func TestHandleCallGenerator(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.funcs["gen"] = &FuncDef{
		Name:      "gen",
		Params:    []*Param{},
		Generator: true,
		Body: []*Op{
			{OpType: "yield", Value: &ValueExpr{Kind: "literal", Value: 1}},
			{OpType: "yield", Value: &ValueExpr{Kind: "literal", Value: 2}},
			{OpType: "yield", Value: &ValueExpr{Kind: "literal", Value: 3}},
		},
	}

	result, err := e.HandleCall(`{"func": "gen", "args": []}`)
	if err != nil {
		t.Fatalf("HandleCall generator: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || descriptor["__omnivm_resource__"] != true || descriptor["kind"] != "sequence" || descriptor["transfer"] != true {
		t.Fatalf("generator envelope = %#v, want transfer sequence proxy", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("generator sequence id: %v", err)
	}
	for i, want := range []float64{1, 2, 3} {
		indexed, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":` + strconv.Itoa(i) + `}`)
		if err != nil {
			t.Fatalf("HandleCall handle_index[%d]: %v", i, err)
		}
		indexEnv := decodeResultEnvelopeForTest(t, indexed)
		if indexEnv.Kind != "number" || indexEnv.Value != want {
			t.Fatalf("generator index %d = %#v, want %v", i, indexEnv, want)
		}
	}
}

func TestHandleCallSpread(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.funcs["variadic"] = &FuncDef{
		Name:   "variadic",
		Params: []*Param{{Name: "first"}, {Name: "rest", Spread: true}},
		Body: []*Op{
			{OpType: "return", Value: &ValueExpr{Kind: "ref", Name: "first"}},
		},
	}

	result, err := e.HandleCall(`{"func": "variadic", "args": ["a", "b", "c"]}`)
	if err != nil {
		t.Fatalf("HandleCall spread: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "a" {
		t.Errorf("spread envelope = %#v, want string a", env)
	}
}

// --- Stub generation tests ---

func TestJSStub(t *testing.T) {
	code := jsStub("add", []*Param{{Name: "a"}, {Name: "b"}})
	if !contains(code, `globalThis["add"]`) {
		t.Error("JS stub should set a global function property")
	}
	if !contains(code, "__omnivm_decode_result") {
		t.Error("JS stub should decode manifest result envelopes")
	}
	if !contains(code, "__omnivm_materialize_capture(env.value)") {
		t.Error("JS stub should materialize returned bridge descriptors")
	}
	if !contains(code, `"add"`) {
		t.Error("JS stub should reference function name")
	}
	if !contains(code, "__omnivm_manifest_invoke") || !contains(code, "args.map(globalThis.__omnivm_encode_arg)") || !contains(code, "__omnivm_runtime_ref__") {
		t.Fatalf("JS stub should preserve complex args as runtime refs, got %q", code)
	}
}

func TestJSStubCallableShape(t *testing.T) {
	code := jsStub("render", []*Param{{
		Name: "__options",
		CallableShape: &CallableShape{
			AcceptsOptionsObject: true,
			DestructuredKeys:     []string{"limit", "payload"},
		},
	}})
	if !contains(code, "__omnivm_callable_shape__") || !contains(code, "callable_shape") {
		t.Fatalf("JS stub should attach callable shape to runtime-ref descriptors, got %q", code)
	}
	if !contains(code, `"acceptsOptionsObject":true`) || !contains(code, `"destructuredKeys":["limit","payload"]`) {
		t.Fatalf("JS stub should encode options-object callable shape, got %q", code)
	}
}

func TestDecodeRuntimeRefArgCallableShape(t *testing.T) {
	arity := 2
	decoded := decodeRuntimeRefArg(map[string]interface{}{
		"__omnivm_runtime_ref__": true,
		"runtime":                "javascript",
		"var":                    "render",
		"callable":               true,
		"callable_shape": map[string]interface{}{
			"acceptsOptionsObject": true,
			"destructuredKeys":     []interface{}{"limit", "payload"},
			"parameterNames":       []interface{}{"options"},
			"arity":                float64(arity),
			"javaAdapter": map[string]interface{}{
				"kind":       "map",
				"method":     "accept",
				"targetType": "com.example.Handler",
				"keys":       []interface{}{"limit", "payload"},
			},
		},
	})
	ref, ok := decoded.(RuntimeRef)
	if !ok {
		t.Fatalf("decodeRuntimeRefArg = %T, want RuntimeRef", decoded)
	}
	if !ref.CallableKnown || !ref.Callable {
		t.Fatalf("decoded callable flags = known:%v callable:%v", ref.CallableKnown, ref.Callable)
	}
	if ref.CallableShape == nil || !ref.CallableShape.AcceptsOptionsObject || strings.Join(ref.CallableShape.DestructuredKeys, ",") != "limit,payload" {
		t.Fatalf("decoded callable shape = %#v", ref.CallableShape)
	}
	if ref.CallableShape.Arity == nil || *ref.CallableShape.Arity != arity || strings.Join(ref.CallableShape.ParameterNames, ",") != "options" {
		t.Fatalf("decoded callable signature shape = %#v", ref.CallableShape)
	}
	if ref.CallableShape.JavaAdapter == nil ||
		ref.CallableShape.JavaAdapter.Kind != "map" ||
		ref.CallableShape.JavaAdapter.Method != "accept" ||
		ref.CallableShape.JavaAdapter.TargetType != "com.example.Handler" ||
		strings.Join(ref.CallableShape.JavaAdapter.Keys, ",") != "limit,payload" {
		t.Fatalf("decoded Java adapter shape = %#v", ref.CallableShape.JavaAdapter)
	}
}

func TestRuntimePrimitiveSnapshotExprProbesCallableShape(t *testing.T) {
	js := runtimePrimitiveSnapshotExpr("javascript", "handler")
	if !strings.Contains(js, "Function.prototype.toString") || !strings.Contains(js, "acceptsOptionsObject") || !strings.Contains(js, "arity") {
		t.Fatalf("JS primitive snapshot should probe arity and destructured options shape, got %q", js)
	}
	py := runtimePrimitiveSnapshotExpr("python", "handler")
	if !strings.Contains(py, "__import__(\"inspect\")") || !strings.Contains(py, "VAR_KEYWORD") || !strings.Contains(py, "parameterNames") {
		t.Fatalf("Python primitive snapshot should inspect callable signature, got %q", py)
	}
	rb := runtimePrimitiveSnapshotExpr("ruby", "handler")
	if !strings.Contains(rb, ".parameters") || !strings.Contains(rb, ":keyrest") || !strings.Contains(rb, "parameterNames") {
		t.Fatalf("Ruby primitive snapshot should inspect callable parameters, got %q", rb)
	}
	java := runtimePrimitiveSnapshotExpr("java", "handler")
	if !strings.Contains(java, "primitiveSnapshot(handler)") {
		t.Fatalf("Java primitive snapshot should delegate to Java reflection helper, got %q", java)
	}
}

func TestRuntimeRefPythonLenExprSkipsUnsizedObjects(t *testing.T) {
	expr, ok := runtimeRefLenExpr(RuntimeRef{Runtime: "python", VarName: "row"})
	if !ok {
		t.Fatal("python len expression should be available")
	}
	for _, want := range []string{"hasattr(__o, '__len__')", "len(__o)", "else None"} {
		if !strings.Contains(expr, want) {
			t.Fatalf("python len expression missing %q in %q", want, expr)
		}
	}
}

func TestRuntimeRefRubyStreamProbeTreatsHTTPMessagesAsResources(t *testing.T) {
	expr, ok := runtimeRefStreamProbeExpr(RuntimeRef{Runtime: "ruby", VarName: "response"})
	if !ok {
		t.Fatal("ruby stream probe should be available")
	}
	for _, want := range []string{"respond_to?(:request_method)", "respond_to?(:status)", "respond_to?(:get_header)", "!__omnivm_http_message"} {
		if !strings.Contains(expr, want) {
			t.Fatalf("ruby stream probe missing %q in %q", want, expr)
		}
	}
}

func TestRuntimeRefPythonStreamProbeTreatsPydanticModelsAsResources(t *testing.T) {
	expr, ok := runtimeRefStreamProbeExpr(RuntimeRef{Runtime: "python", VarName: "model"})
	if !ok {
		t.Fatal("python stream probe should be available")
	}
	for _, want := range []string{"getattr(type(__v), 'model_fields', None)", "model_fields', None) is None"} {
		if !strings.Contains(expr, want) {
			t.Fatalf("python stream probe missing Pydantic model guard %q in %q", want, expr)
		}
	}
	if strings.Count(expr, "(") != strings.Count(expr, ")") {
		t.Fatalf("python stream probe has unbalanced parentheses: %q", expr)
	}
}

func TestRuntimeRefRubyStreamProbeTreatsResponseWritersAsResources(t *testing.T) {
	expr, ok := runtimeRefStreamProbeExpr(RuntimeRef{Runtime: "ruby", VarName: "stream"})
	if !ok {
		t.Fatal("ruby stream probe should be available")
	}
	for _, want := range []string{"respond_to?(:write)", "respond_to?(:close)", "respond_to?(:closed?)", "!__v.respond_to?(:read)", "!__omnivm_response_writer"} {
		if !strings.Contains(expr, want) {
			t.Fatalf("ruby stream probe missing response writer guard %q in %q", want, expr)
		}
	}
}

func TestJSStubUnsafeName(t *testing.T) {
	code := jsStub("bad-name", []*Param{{Name: "class"}})
	if contains(code, "globalThis.bad-name") {
		t.Fatalf("JS stub should not emit unsafe property syntax, got %q", code)
	}
	if !contains(code, `globalThis["bad-name"]`) {
		t.Fatalf("JS stub should register unsafe names with bracket syntax, got %q", code)
	}
}

func TestPythonStub(t *testing.T) {
	code := pythonStub("greet", []string{"name"})
	if !contains(code, "def greet(*__omnivm_args)") {
		t.Error("Python stub should define function")
	}
	if !contains(code, "omnivm.call") {
		t.Error("Python stub should call omnivm")
	}
	if !contains(code, "import omnivm") {
		t.Error("Python stub should import the bridge module instead of relying on ambient globals")
	}
	if !contains(code, "__omnivm_decode_result") {
		t.Error("Python stub should decode manifest result envelopes")
	}
	if !contains(code, `globals()["__omnivm_materialize_capture"](env.get('value'))`) {
		t.Error("Python stub should materialize returned bridge descriptors")
	}
	if !contains(code, "__omnivm_manifest_invoke") || !contains(code, `[__omnivm_encode_arg(__arg) for __arg in __omnivm_args]`) || !contains(code, `"__omnivm_runtime_ref__"`) {
		t.Fatalf("Python stub should preserve complex args as runtime refs, got %q", code)
	}
	if !contains(code, `or descriptor.get("__omnivm_channel__") is True`) {
		t.Fatalf("Python stub should preserve channel proxies as descriptors, got %q", code)
	}
}

func TestPythonStubUnsafeName(t *testing.T) {
	code := pythonStub("class", []string{"payload"})
	if contains(code, "def class(") {
		t.Fatalf("Python stub should not emit unsafe def syntax, got %q", code)
	}
	if !contains(code, `globals()["class"] = lambda *__omnivm_args`) {
		t.Fatalf("Python stub should expose unsafe names through globals registry, got %q", code)
	}
}

func TestRubyStub(t *testing.T) {
	code := rubyStub("greet", []string{"name"})
	if !contains(code, "def greet(*__omnivm_args)") {
		t.Error("Ruby stub should define function")
	}
	if !contains(code, "OmniVM.call") {
		t.Error("Ruby stub should call OmniVM")
	}
	if !contains(code, "__omnivm_decode_result") {
		t.Error("Ruby stub should decode manifest result envelopes")
	}
	if !contains(code, `__omnivm_materialize_capture(env["value"])`) {
		t.Error("Ruby stub should materialize returned bridge descriptors")
	}
	if !contains(code, "__omnivm_manifest_invoke") || !contains(code, "__omnivm_args.map { |arg| __omnivm_encode_arg(arg) }") || !contains(code, `"__omnivm_runtime_ref__"`) {
		t.Fatalf("Ruby stub should preserve complex args as runtime refs, got %q", code)
	}
	if !contains(code, `descriptor["__omnivm_channel__"] == true`) {
		t.Fatalf("Ruby stub should preserve channel proxies as descriptors, got %q", code)
	}
}

func TestRubyStubUnsafeName(t *testing.T) {
	code := rubyStub("class", []string{"payload"})
	if contains(code, "def class(") {
		t.Fatalf("Ruby stub should not emit unsafe def syntax, got %q", code)
	}
	if !contains(code, `$__omnivm_manifest_funcs["class"]`) {
		t.Fatalf("Ruby stub should expose unsafe names through manifest function registry, got %q", code)
	}
}

func TestJavaManifestStubs(t *testing.T) {
	code := javaManifestStubs(map[string]*FuncDef{
		"greet": &FuncDef{Name: "greet", Params: []*Param{{Name: "name"}}},
		"ping":  &FuncDef{Name: "ping"},
	})
	if !contains(code, "package omnivm;") || !contains(code, "public class OmniVMManifest") {
		t.Fatalf("Java manifest stubs should compile into the omnivm package, got %q", code)
	}
	if !contains(code, "public static Object greet(Object name)") {
		t.Fatalf("Java manifest stub should expose Object params, got %q", code)
	}
	if !contains(code, `return OmniVM.callManifest("greet", name);`) {
		t.Fatalf("Java manifest stub should preserve complex args through OmniVM.callManifest, got %q", code)
	}
	if !contains(code, `return OmniVM.callManifest("ping");`) {
		t.Fatalf("Java manifest stub should support zero-arg functions, got %q", code)
	}
	if !contains(code, `public static Object invoke(String func, Object... args)`) {
		t.Fatalf("Java manifest stubs should expose generic invoke fallback, got %q", code)
	}
}

func TestJavaManifestStubsSanitizeUnsafeNames(t *testing.T) {
	code := javaManifestStubs(map[string]*FuncDef{
		"class": {Name: "class", Params: []*Param{{Name: "payload"}}},
		"safe": {
			Name: "safe",
			Params: []*Param{
				{Name: "class"},
				{Name: "1bad"},
				{Name: "class"},
			},
		},
		"bad-name": {Name: "bad-name", Params: []*Param{{Name: "payload"}}},
	})
	if contains(code, "public static Object class(") || contains(code, "public static Object bad-name(") {
		t.Fatalf("Java manifest stubs should skip invalid convenience method names, got %q", code)
	}
	if !contains(code, `public static Object invoke(String func, Object... args)`) {
		t.Fatalf("Java manifest stubs should keep generic invoke fallback for unsafe names, got %q", code)
	}
	if !contains(code, "public static Object safe(Object __omnivm_arg_0, Object __omnivm_arg_1, Object __omnivm_arg_2)") {
		t.Fatalf("Java manifest stubs should sanitize invalid/reserved params, got %q", code)
	}
	if !contains(code, `return OmniVM.callManifest("safe", __omnivm_arg_0, __omnivm_arg_1, __omnivm_arg_2);`) {
		t.Fatalf("Java manifest stubs should call with sanitized params in order, got %q", code)
	}
}

// --- drainChannel tests ---

func TestDrainChannelEmpty(t *testing.T) {
	ch := &ChanRef{ch: make(chan interface{}, 5)}
	result := drainChannel(ch)
	if len(result) != 0 {
		t.Errorf("drain empty = %d items, want 0", len(result))
	}
}

func TestDrainChannelWithData(t *testing.T) {
	ch := &ChanRef{ch: make(chan interface{}, 5)}
	ch.ch <- "a"
	ch.ch <- "b"
	result := drainChannel(ch)
	if len(result) != 2 {
		t.Fatalf("drain = %d items, want 2", len(result))
	}
	if result[0] != "a" || result[1] != "b" {
		t.Errorf("drain = %v, want [a b]", result)
	}
}

func TestDrainChannelClosed(t *testing.T) {
	ch := &ChanRef{ch: make(chan interface{}, 5), closed: true}
	result := drainChannel(ch)
	if len(result) != 0 {
		t.Errorf("drain closed = %d items, want 0", len(result))
	}
}

func TestDrainChannelClosedWithBufferedData(t *testing.T) {
	ch := &ChanRef{ch: make(chan interface{}, 5)}
	ch.ch <- "a"
	ch.ch <- "b"
	close(ch.ch)
	ch.closed = true
	result := drainChannel(ch)
	if len(result) != 2 {
		t.Fatalf("drain closed buffered = %d items, want 2", len(result))
	}
	if result[0] != "a" || result[1] != "b" {
		t.Errorf("drain closed buffered = %v, want [a b]", result)
	}
}

func TestChannelStreamCaptureIsLazy(t *testing.T) {
	e, _ := makeExecutor("javascript")
	ch := &ChanRef{ch: make(chan interface{}, 3)}
	ch.ch <- "a"
	ch.ch <- map[string]interface{}{"name": "b"}
	if len(ch.ch) != 2 {
		t.Fatalf("test channel setup len = %d, want 2", len(ch.ch))
	}
	jsonVal, err := e.channelStreamCaptureJSON(ch)
	if err != nil {
		t.Fatalf("channelStreamCaptureJSON: %v", err)
	}
	if !strings.Contains(jsonVal, `"__omnivm_stream__":true`) || strings.Contains(jsonVal, `"name":"b"`) {
		t.Fatalf("stream capture should be a descriptor, not a drained snapshot: %s", jsonVal)
	}
	if len(ch.ch) != 2 {
		t.Fatalf("stream capture drained channel, len = %d, want 2", len(ch.ch))
	}
	stats := e.BoundaryStats()
	if stats.StreamProxyCaptures != 1 || stats.ChannelMaterializations != 0 {
		t.Fatalf("stream capture stats = %+v, want stream proxy without materialization", stats)
	}
}

func TestLocalStreamCaptureJSONDetectsNativeChannels(t *testing.T) {
	e, _ := makeExecutor("javascript")
	ch := make(chan interface{}, 3)
	ch <- "a"
	ch <- map[string]interface{}{"name": "b"}
	if len(ch) != 2 {
		t.Fatalf("test channel setup len = %d, want 2", len(ch))
	}
	jsonVal, ok, err := e.localStreamCaptureJSON(ch, "go")
	if err != nil {
		t.Fatalf("localStreamCaptureJSON: %v", err)
	}
	if !ok {
		t.Fatalf("localStreamCaptureJSON did not recognize native channel")
	}
	if !strings.Contains(jsonVal, `"__omnivm_stream__":true`) || strings.Contains(jsonVal, `"name":"b"`) {
		t.Fatalf("native channel capture should be a descriptor, not a drained snapshot: %s", jsonVal)
	}
	if len(ch) != 2 {
		t.Fatalf("stream capture drained native channel, len = %d, want 2", len(ch))
	}
	stats := e.BoundaryStats()
	if stats.StreamProxyCaptures != 1 || stats.ChannelMaterializations != 0 {
		t.Fatalf("native stream capture stats = %+v, want stream proxy without materialization", stats)
	}
}

func TestLocalStreamCaptureJSONDetectsReaders(t *testing.T) {
	e, _ := makeExecutor("javascript")
	reader := strings.NewReader("reader-body")
	jsonVal, ok, err := e.localStreamCaptureJSON(reader, "go")
	if err != nil {
		t.Fatalf("localStreamCaptureJSON reader: %v", err)
	}
	if !ok {
		t.Fatalf("localStreamCaptureJSON did not recognize io.Reader")
	}
	if !strings.Contains(jsonVal, `"__omnivm_stream__":true`) || !strings.Contains(jsonVal, `"kind":"reader"`) || strings.Contains(jsonVal, "reader-body") {
		t.Fatalf("reader capture should be a descriptor, not a drained snapshot: %s", jsonVal)
	}
	if reader.Len() != len("reader-body") {
		t.Fatalf("reader capture drained source, remaining = %d", reader.Len())
	}
	stats := e.BoundaryStats()
	if stats.StreamProxyCaptures != 1 || stats.ChannelMaterializations != 0 {
		t.Fatalf("reader stream capture stats = %+v, want stream proxy without materialization", stats)
	}
}

func TestLocalReaderHTTPMessageShapeCapturesAsResource(t *testing.T) {
	e, _ := makeExecutor("javascript")
	req := &goHTTPMessageReaderShape{
		Method:  "PUT",
		Path:    "/go-reader-request",
		Headers: map[string]string{"X-Request-Id": "go-reader-42"},
	}
	if isReaderStreamValue(req) {
		t.Fatalf("request-shaped reader should not classify as stream")
	}
	jsonVal, ok, err := e.localStreamCaptureJSON(req, "go")
	if err != nil {
		t.Fatalf("localStreamCaptureJSON request-shaped reader: %v", err)
	}
	if ok {
		t.Fatalf("request-shaped reader should not capture as stream: %s", jsonVal)
	}
	bridged, err := e.bridgeReturnValue(req)
	if err != nil {
		t.Fatalf("bridgeReturnValue request-shaped reader: %v", err)
	}
	descriptor, ok := bridged.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("request-shaped reader should capture as resource proxy, got %#v", bridged)
	}
	if req.reads != 0 {
		t.Fatalf("request-shaped reader was read during capture, reads=%d", req.reads)
	}
	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures != 1 || stats.StreamProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("request-shaped reader stats = %+v, want resource proxy without stream/json", stats)
	}
}

func TestAutoInjectScopePreservesNativeStreams(t *testing.T) {
	e, _ := makeExecutor("javascript")
	ch := make(chan interface{}, 2)
	ch <- "first"
	ch <- map[string]interface{}{"name": "second"}
	e.setBinding("events", ch)

	setup := e.autoInjectScope("javascript")
	if !strings.Contains(setup, `globalThis["events"]`) {
		t.Fatalf("autoInjectScope setup = %q, want events binding", setup)
	}
	if !strings.Contains(setup, `"__omnivm_stream__":true`) || strings.Contains(setup, `"name":"second"`) {
		t.Fatalf("auto-injected native stream should be a descriptor, not a drained snapshot: %s", setup)
	}
	if len(ch) != 2 {
		t.Fatalf("auto-injected stream drained native channel, len = %d, want 2", len(ch))
	}
	stats := e.BoundaryStats()
	if stats.StreamProxyCaptures != 1 || stats.JSONFallbacks != 0 || stats.ChannelMaterializations != 0 {
		t.Fatalf("auto-injected stream stats = %+v, want lazy stream descriptor without JSON fallback", stats)
	}
}

func TestHandleCallStreamNextReader(t *testing.T) {
	e, _ := makeExecutor("javascript")
	id, err := e.genericStreamHandle("go", strings.NewReader("reader-body"))
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}

	result, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_next reader: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	item, ok := env.Value.(map[string]interface{})
	if !ok || item["done"] == true {
		t.Fatalf("reader stream_next envelope = %#v, want chunk", env)
	}
	chunk, ok := item["value"].(map[string]interface{})
	if !ok || chunk["__omnivm_table__"] != true || chunk["format"] != "arrow_c_data" {
		t.Fatalf("reader stream_next chunk = %#v, want Arrow table descriptor", item["value"])
	}
	chunkID, err := bridgeHandleID(chunk["id"])
	if err != nil {
		t.Fatalf("reader chunk table id: %v", err)
	}

	result, err = e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_next reader done: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	item, ok = env.Value.(map[string]interface{})
	if !ok || item["done"] != true {
		t.Fatalf("reader stream_next done envelope = %#v, want done", env)
	}
	if err := e.ensureHandleTable().ReleaseAllRefs(chunkID); err != nil {
		t.Fatalf("release reader chunk table: %v", err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 2 || stats.Live != 0 || stats.ExplicitReleases != 2 {
		t.Fatalf("reader stream access/release stats = %+v, want 2 stream reads, EOF release, and chunk release", stats)
	}
}

func TestHandleCallStreamReaderClosesAtEOF(t *testing.T) {
	e, _ := makeExecutor("javascript")
	reader := &closeTrackingReader{Reader: strings.NewReader("reader-body")}
	id, err := e.genericStreamHandle("go", reader)
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}

	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err != nil {
		t.Fatalf("HandleCall stream_next reader chunk: %v", err)
	}
	if reader.closed {
		t.Fatal("reader closed before EOF")
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err != nil {
		t.Fatalf("HandleCall stream_next reader EOF: %v", err)
	}
	if !reader.closed {
		t.Fatal("reader was not closed when stream reached EOF")
	}
}

func TestHandleCallStreamReaderErrorReleasesOwner(t *testing.T) {
	e, _ := makeExecutor("javascript")
	reader := &errorAfterChunkReader{chunk: "first"}
	id, err := e.genericStreamHandle("go", reader)
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}

	result, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_next reader chunk: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	item, ok := env.Value.(map[string]interface{})
	if !ok || item["done"] == true {
		t.Fatalf("reader first stream_next envelope = %#v, want chunk", env)
	}
	chunk, ok := item["value"].(map[string]interface{})
	if !ok || chunk["__omnivm_table__"] != true {
		t.Fatalf("reader first stream_next chunk = %#v, want table", item["value"])
	}
	chunkID, err := bridgeHandleID(chunk["id"])
	if err != nil {
		t.Fatalf("reader chunk table id: %v", err)
	}
	if err := e.ensureHandleTable().ReleaseAllRefs(chunkID); err != nil {
		t.Fatalf("release reader chunk table: %v", err)
	}
	beforeError := e.handleTable.Stats(time.Now())
	if reader.closed {
		t.Fatal("reader closed before read error")
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stream_next reader error did not fail")
	} else if !strings.Contains(err.Error(), "owner read failed") {
		t.Fatalf("stream_next reader error = %v, want owner read failure", err)
	}
	if !reader.closed {
		t.Fatal("reader was not closed after read error")
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != beforeError.ExplicitReleases+1 {
		t.Fatalf("reader error should release stream handle once: before=%+v after=%+v", beforeError, stats)
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale stream_next after reader error did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=reader", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale stream_next after reader error missing %q: %s", want, got)
			}
		}
	}
}

func TestRuntimeRefStreamReadErrorReleasesHandle(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	execCalls := 0
	mocks["python"].execFn = func(code string) pkg.Result {
		execCalls++
		if execCalls == 1 {
			return pkg.Result{Err: errors.New("owner stream failed")}
		}
		return pkg.Result{}
	}
	mocks["python"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Value: false}
	}
	id, err := e.runtimeRefStreamHandle(RuntimeRef{Runtime: "python", VarName: "rows"})
	if err != nil {
		t.Fatalf("runtimeRefStreamHandle: %v", err)
	}

	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("runtime ref stream_next error did not fail")
	} else if !strings.Contains(err.Error(), "owner stream failed") {
		t.Fatalf("runtime ref stream_next error = %v, want owner failure", err)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("runtime ref stream read error should release handle once: %+v", stats)
	}
	if execCalls < 2 {
		t.Fatalf("runtime ref stream read error did not run stream close cleanup; execCalls=%d", execCalls)
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale runtime ref stream_next after read error did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=python", "kind=stream", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale runtime ref stream_next after error missing %q: %s", want, got)
			}
		}
	}
}

func TestHandleCallStreamCancelClosesReader(t *testing.T) {
	e, _ := makeExecutor("javascript")
	reader := &closeTrackingReader{Reader: strings.NewReader("reader-body")}
	id, err := e.genericStreamHandle("go", reader)
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}

	result, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_cancel reader: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Value != true {
		t.Fatalf("stream_cancel reader envelope = %#v, want true", env)
	}
	if !reader.closed {
		t.Fatal("reader was not closed on stream_cancel")
	}
	if reader.Len() != len("reader-body") {
		t.Fatalf("stream_cancel should not drain reader, remaining = %d", reader.Len())
	}
}

func TestHandleCallStreamCancelCloseErrorKeepsLifecycleTombstone(t *testing.T) {
	e, _ := makeExecutor("javascript")
	id, err := e.genericStreamHandle("go", &closeErrorReader{Reader: strings.NewReader("reader-body")})
	if err != nil {
		t.Fatalf("genericStreamHandle reader: %v", err)
	}
	if _, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stream_cancel close error did not fail")
	} else if !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("stream_cancel close error = %v, want close failure", err)
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale stream_next after close failure did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=reader", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale stream_next after close failure missing %q: %s", want, got)
			}
		}
	}
}

func TestRuntimeRefStreamEOFClosesOwnerAndTombstones(t *testing.T) {
	cases := []struct {
		name        string
		runtimeName string
		asyncProbe  bool
		closeMarker string
	}{
		{name: "python sync", runtimeName: "python", closeMarker: "__omnivm_stream_close_task"},
		{name: "python async", runtimeName: "python", asyncProbe: true, closeMarker: "__omnivm_stream_close_task"},
		{name: "javascript", runtimeName: "javascript", closeMarker: "return __omnivm_close_step"},
		{name: "ruby", runtimeName: "ruby", closeMarker: "to_io"},
		{name: "java", runtimeName: "java", closeMarker: "AutoCloseable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, mocks := makeExecutor(tc.runtimeName)
			rt := mocks[tc.runtimeName]
			ready := false
			rt.pumpFn = func() { ready = true }
			rt.evalFn = func(code string) pkg.Result {
				switch {
				case strings.Contains(code, "__aiter__"):
					return pkg.Result{Value: tc.asyncProbe}
				case (strings.Contains(code, "stream_next") || strings.Contains(code, "stream_close")) && strings.Contains(code, "ready"):
					if tc.runtimeName == "python" {
						if ready {
							return pkg.Result{Value: "True"}
						}
						return pkg.Result{Value: "False"}
					}
					return pkg.Result{Value: ready}
				case strings.Contains(code, "stream_next") && strings.Contains(code, "done"):
					return pkg.Result{Value: true}
				case (strings.Contains(code, "stream_next") || strings.Contains(code, "stream_close")) && strings.Contains(code, "error"):
					return pkg.Result{Value: nil}
				default:
					return pkg.Result{Value: nil}
				}
			}
			id, err := e.runtimeRefStreamHandle(RuntimeRef{Runtime: tc.runtimeName, VarName: "rows"})
			if err != nil {
				t.Fatalf("runtimeRefStreamHandle: %v", err)
			}

			result, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
			if err != nil {
				t.Fatalf("runtime ref stream_next EOF: %v", err)
			}
			env := decodeResultEnvelopeForTest(t, result)
			item, ok := env.Value.(map[string]interface{})
			if !ok || item["done"] != true {
				t.Fatalf("runtime ref stream_next EOF envelope = %#v, want done", env)
			}
			stats := e.handleTable.Stats(time.Now())
			if stats.Live != 0 || stats.ExplicitReleases != 1 {
				t.Fatalf("runtime ref stream EOF should release handle once: %+v", stats)
			}
			if !containsExecCall(rt.execCalls, tc.closeMarker) {
				t.Fatalf("runtime ref stream EOF did not run %s close protocol %q; calls=%q", tc.runtimeName, tc.closeMarker, rt.execCalls)
			}
			if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
				t.Fatal("stale runtime ref stream_next after EOF did not fail")
			} else {
				got := err.Error()
				for _, want := range []string{"closed stream handle", "runtime=" + tc.runtimeName, "kind=stream", "owner-side lifecycle is closed"} {
					if !strings.Contains(got, want) {
						t.Fatalf("stale runtime ref stream_next after EOF missing %q: %s", want, got)
					}
				}
			}
		})
	}
}

func TestRuntimeRefStreamCancelCloseErrorKeepsLifecycleTombstone(t *testing.T) {
	e, mocks := makeExecutor("javascript")
	id, err := e.runtimeRefStreamHandle(RuntimeRef{Runtime: "javascript", VarName: "rows"})
	if err != nil {
		t.Fatalf("runtimeRefStreamHandle: %v", err)
	}
	ready := false
	mocks["javascript"].pumpFn = func() { ready = true }
	mocks["javascript"].evalFn = func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "stream_close_ready"):
			return pkg.Result{Value: ready}
		case strings.Contains(code, "stream_close_error"):
			return pkg.Result{Value: "cancel failed"}
		default:
			return pkg.Result{Value: nil}
		}
	}

	if _, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("runtime ref stream_cancel close error did not fail")
	} else if !strings.Contains(err.Error(), "cancel failed") {
		t.Fatalf("runtime ref stream_cancel close error = %v, want cancel failure", err)
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale runtime ref stream_next after close failure did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=javascript", "kind=stream", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale runtime ref stream_next after close failure missing %q: %s", want, got)
			}
		}
	}
}

func TestRuntimeRefStreamCloseCodeUsesHostProtocols(t *testing.T) {
	cases := []struct {
		ref  RuntimeRef
		want string
	}{
		{RuntimeRef{Runtime: "python", VarName: "rows"}, "getattr(__omnivm_stream_obj, 'close', None)"},
		{RuntimeRef{Runtime: "python", VarName: "rows"}, "__omnivm_close_frame_iterators"},
		{RuntimeRef{Runtime: "ruby", VarName: "rows"}, "to_io"},
		{RuntimeRef{Runtime: "java", VarName: "rows"}, "AutoCloseable"},
	}
	for _, tc := range cases {
		code, ok := runtimeRefStreamCloseCode(tc.ref, "__omnivm_stream_state")
		if !ok {
			t.Fatalf("runtimeRefStreamCloseCode(%s) unsupported", tc.ref.Runtime)
		}
		if !contains(code, tc.want) {
			t.Fatalf("runtimeRefStreamCloseCode(%s) = %q, want %q", tc.ref.Runtime, code, tc.want)
		}
	}
	if code, ok := runtimeRefStreamCloseCode(RuntimeRef{Runtime: "javascript", VarName: "rows"}, "__omnivm_stream_state"); ok {
		t.Fatalf("runtimeRefStreamCloseCode(javascript) = %q, want unsupported so callers use awaited JS close step", code)
	}
}

func TestRuntimeRefRubyStreamCloseSkipsRequiredArgCloseMethods(t *testing.T) {
	code, ok := runtimeRefStreamCloseCode(RuntimeRef{Runtime: "ruby", VarName: "rows"}, "__omnivm_stream_state")
	if !ok {
		t.Fatal("runtimeRefStreamCloseCode(ruby) unsupported")
	}
	for _, want := range []string{
		"__omnivm_lifecycle_without_required_args",
		"value.method(name).arity",
		"arity == 0 || arity == -1",
		"__omnivm_lifecycle_without_required_args.call(__omnivm_stream_obj, :close)",
		"__omnivm_lifecycle_without_required_args.call(__omnivm_stream_obj, :return)",
		"__omnivm_lifecycle_without_required_args.call(__omnivm_io, :close)",
	} {
		if !contains(code, want) {
			t.Fatalf("runtimeRefStreamCloseCode(ruby) missing %q in %q", want, code)
		}
	}
	if contains(code, "__omnivm_stream_obj.respond_to?(:close); __omnivm_stream_obj.close") ||
		contains(code, "__omnivm_stream_obj.respond_to?(:return); __omnivm_stream_obj.return") {
		t.Fatalf("runtimeRefStreamCloseCode(ruby) should not close from respond_to? alone: %q", code)
	}

	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	script := `
class RequiredCloseWithReturn
  attr_reader :close_calls, :return_calls
  def initialize
    @close_calls = 0
    @return_calls = 0
  end
  def close(reason)
    @close_calls += 1
    raise "required close should not run"
  end
  def return
    @return_calls += 1
  end
end

$rows = RequiredCloseWithReturn.new
` + code + `
raise "required close was invoked" unless $rows.close_calls == 0
raise "zero-arg return fallback was not invoked" unless $rows.return_calls == 1

class RequiredOnlyClose
  attr_reader :close_calls
  def initialize
    @close_calls = 0
  end
  def close(reason)
    @close_calls += 1
    raise "required close should not run"
  end
end

$rows = RequiredOnlyClose.new
` + code + `
raise "required-only close was invoked" unless $rows.close_calls == 0

require "stringio"
class IOFallbackWrapper
  attr_reader :io
  def initialize
    @io = StringIO.new("body")
  end
  def to_io
    @io
  end
  def close(reason)
    raise "required wrapper close should not run"
  end
end

$rows = IOFallbackWrapper.new
` + code + `
raise "to_io close fallback was not invoked" unless $rows.io.closed?
`
	if out, err := exec.Command(ruby, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("ruby stream close required-arg guard failed: %v\n%s", err, out)
	}
}

func TestRuntimeRefRubyStreamNextSkipsRequiredArgCloseOnEOF(t *testing.T) {
	code, ok := runtimeRefStreamNextCode(RuntimeRef{Runtime: "ruby", VarName: "rows"}, "__omnivm_stream_value", "__omnivm_stream_done", "__omnivm_stream_state")
	if !ok {
		t.Fatal("runtimeRefStreamNextCode(ruby) unsupported")
	}
	for _, want := range []string{
		"__omnivm_lifecycle_without_required_args",
		"value.method(name).arity",
		"__omnivm_close_without_required_args",
		"__omnivm_close_without_required_args.call(__omnivm_stream_obj) if $__omnivm_stream_done",
		"__omnivm_close_without_required_args.call(__omnivm_io)",
		"__omnivm_close_without_required_args.call(__omnivm_stream_obj) if defined?(__omnivm_stream_obj)",
	} {
		if !contains(code, want) {
			t.Fatalf("runtimeRefStreamNextCode(ruby) missing %q in %q", want, code)
		}
	}
	for _, forbidden := range []string{
		"__omnivm_stream_obj.close if $__omnivm_stream_done && __omnivm_stream_obj.respond_to?(:close)",
		"if __omnivm_stream_obj.respond_to?(:close); __omnivm_stream_obj.close",
		"__omnivm_stream_obj.close if defined?(__omnivm_stream_obj) && __omnivm_stream_obj.respond_to?(:close)",
	} {
		if contains(code, forbidden) {
			t.Fatalf("runtimeRefStreamNextCode(ruby) should not close from respond_to? alone, found %q in %q", forbidden, code)
		}
	}

	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	script := `
class RequiredReadClose
  attr_reader :close_calls
  def initialize
    @close_calls = 0
  end
  def read(_size)
    ""
  end
  def close(reason)
    @close_calls += 1
    raise "required close should not run"
  end
end

$rows = RequiredReadClose.new
` + code + `
raise "required read close was invoked" unless $rows.close_calls == 0
raise "required read stream did not report done" unless $__omnivm_stream_done == true

class OptionalReadClose
  attr_reader :close_calls
  def initialize
    @close_calls = 0
  end
  def read(_size)
    ""
  end
  def close(reason = nil)
    @close_calls += 1
  end
end

$rows = OptionalReadClose.new
` + code + `
raise "optional read close was not invoked" unless $rows.close_calls == 1

class RequiredIteratorClose
  attr_reader :close_calls
  def initialize
    @close_calls = 0
  end
  def each
    self
  end
  def next
    raise StopIteration
  end
  def close(reason)
    @close_calls += 1
    raise "required iterator close should not run"
  end
end

$rows = RequiredIteratorClose.new
` + code + `
raise "required iterator close was invoked" unless $rows.close_calls == 0
raise "iterator EOF did not report done" unless $__omnivm_stream_done == true
`
	if out, err := exec.Command(ruby, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("ruby stream next required-arg close guard failed: %v\n%s", err, out)
	}
}

func TestRuntimeRefJavaStreamNextClosesBaseStreamAtEOF(t *testing.T) {
	code := runtimeRefJavaStreamNextCode("rows", "__omnivm_stream_value", "__omnivm_stream_done", "__omnivm_stream_state")
	for _, want := range []string{
		"java.util.stream.BaseStream __omnivm_base_stream = null;",
		"new Object[] { __omnivm_base_stream, __omnivm_next }",
		"__omnivm_base_stream.close();",
		`omnivm.OmniVM.setCaptureObject("__omnivm_stream_state", null);`,
	} {
		if !contains(code, want) {
			t.Fatalf("runtimeRefJavaStreamNextCode should close BaseStream at EOF and clear state, missing %q in %q", want, code)
		}
	}
	baseStreamBranch := code[strings.Index(code, "java.util.stream.BaseStream __omnivm_base_stream = null;"):]
	if strings.Index(baseStreamBranch, "__omnivm_base_stream.close();") > strings.Index(baseStreamBranch, `omnivm.OmniVM.setCaptureObject("__omnivm_stream_state", null);`) {
		t.Fatalf("runtimeRefJavaStreamNextCode should close BaseStream before clearing state: %q", code)
	}
}

func TestRuntimeRefPythonAsyncStreamCloseUsesOwnerLoopContract(t *testing.T) {
	code, ok := runtimeRefStreamCloseCode(RuntimeRef{Runtime: "python", VarName: "rows"}, "__omnivm_stream_state")
	if !ok {
		t.Fatal("runtimeRefStreamCloseCode(python) unsupported")
	}
	for _, want := range []string{
		"__omnivm_loop.is_running()",
		"__aio.run_coroutine_threadsafe(__omnivm_await_on_owner_loop(__omnivm_result), __omnivm_loop).result()",
		"OmniVM cannot synchronously await Python async stream close on its running owner asyncio loop; owner dispatch unsupported",
		"__omnivm_result_close()",
	} {
		if !contains(code, want) {
			t.Fatalf("runtimeRefStreamCloseCode(python) missing %q in %q", want, code)
		}
	}
	if contains(code, "__omnivm_close_loop = __aio.new_event_loop()") {
		t.Fatalf("runtimeRefStreamCloseCode(python) creates a second loop for running owner loop: %q", code)
	}
}

func TestRuntimeRefPythonStreamCloseSkipsRequiredArgCloseMethods(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code, ok := runtimeRefStreamCloseCode(RuntimeRef{Runtime: "python", VarName: "rows"}, "__omnivm_stream_state")
	if !ok {
		t.Fatal("runtimeRefStreamCloseCode(python) unsupported")
	}
	script := `
class RequiredClose:
    def __init__(self):
        self.calls = 0
    def close(self, reason):
        self.calls += 1
        raise RuntimeError("required close should not run")

rows = RequiredClose()
__omnivm_stream_state = rows
` + code + `
if rows.calls != 0:
    raise RuntimeError("required-arg close was invoked")

class OptionalClose:
    def __init__(self):
        self.calls = 0
    def close(self, reason=None):
        self.calls += 1

rows = OptionalClose()
__omnivm_stream_state = rows
` + code + `
if rows.calls != 1:
    raise RuntimeError(f"optional close call count mismatch: {rows.calls}")

closed = []
def generator_rows():
    try:
        yield "first"
    finally:
        closed.append(True)

rows = generator_rows()
__omnivm_stream_state = rows
next(rows)
` + code + `
if closed != [True]:
    raise RuntimeError(f"generator close was not preserved: {closed}")
`
	if out, err := exec.Command(python, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("python stream close required-arg guard failed: %v\n%s", err, out)
	}
}

func TestRuntimeRefPythonAsyncStreamCloseStepAwaitsCancellation(t *testing.T) {
	code, ok := runtimeRefPythonStreamCloseStepCode(RuntimeRef{Runtime: "python", VarName: "rows"}, "__omnivm_stream_state", "__omnivm_close_ready", "__omnivm_close_error")
	if !ok {
		t.Fatal("runtimeRefPythonStreamCloseStepCode unsupported")
	}
	for _, want := range []string{
		"__omnivm_close_ready = False",
		"__omnivm_close_error = None",
		"async def __omnivm_close_one",
		"await __omnivm_target_aclose()",
		"async def __omnivm_stream_close_task()",
		"await __omnivm_close_frame_iterators(__omnivm_stream_iter)",
		"globals()['__omnivm_stream_loop'] = __omnivm_loop",
		"__aio.ensure_future(__omnivm_stream_close_task(), loop=__omnivm_loop)",
		"globals()[\"__omnivm_stream_state\"] = None",
		"globals()[\"__omnivm_close_ready\"] = True",
	} {
		if !contains(code, want) {
			t.Fatalf("runtimeRefPythonStreamCloseStepCode missing %q in %q", want, code)
		}
	}
	if contains(code, "run_until_complete") || contains(code, "run_coroutine_threadsafe") {
		t.Fatalf("runtimeRefPythonStreamCloseStepCode should schedule close for the pump loop, got %q", code)
	}
}

func TestRuntimeRefPythonAsyncStreamCloseStepSkipsRequiredArgCloseMethods(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code, ok := runtimeRefPythonStreamCloseStepCode(RuntimeRef{Runtime: "python", VarName: "rows"}, "__omnivm_stream_state", "__omnivm_close_ready", "__omnivm_close_error")
	if !ok {
		t.Fatal("runtimeRefPythonStreamCloseStepCode unsupported")
	}
	script := `
import asyncio

def run_close_task():
    loop = globals()["__omnivm_stream_loop"]
    loop.run_until_complete(asyncio.sleep(0))

class RequiredAsyncClose:
    def __init__(self):
        self.calls = 0
    async def aclose(self, reason):
        self.calls += 1
        raise RuntimeError("required aclose should not run")

rows = RequiredAsyncClose()
__omnivm_stream_state = rows
` + code + `
run_close_task()
if not __omnivm_close_ready or __omnivm_close_error is not None:
    raise RuntimeError(f"required aclose task failed: ready={__omnivm_close_ready} error={__omnivm_close_error}")
if rows.calls != 0:
    raise RuntimeError("required-arg aclose was invoked")

class OptionalAsyncClose:
    def __init__(self):
        self.calls = 0
    async def aclose(self, reason=None):
        self.calls += 1

rows = OptionalAsyncClose()
__omnivm_stream_state = rows
` + code + `
run_close_task()
if not __omnivm_close_ready or __omnivm_close_error is not None:
    raise RuntimeError(f"optional aclose task failed: ready={__omnivm_close_ready} error={__omnivm_close_error}")
if rows.calls != 1:
    raise RuntimeError(f"optional aclose call count mismatch: {rows.calls}")

closed = []
async def async_generator_rows():
    try:
        yield "first"
    finally:
        closed.append(True)

rows = async_generator_rows()
__omnivm_stream_state = rows
globals()["__omnivm_stream_loop"].run_until_complete(rows.__anext__())
` + code + `
run_close_task()
if closed != [True]:
    raise RuntimeError(f"async generator aclose was not preserved: {closed}")
`
	if out, err := exec.Command(python, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("python async stream close required-arg guard failed: %v\n%s", err, out)
	}
}

func TestRuntimeRefJSStreamCloseStepAwaitsCancellation(t *testing.T) {
	code, ok := runtimeRefJSStreamCloseStepCode(RuntimeRef{Runtime: "javascript", VarName: "rows"}, "__omnivm_stream_state", "__omnivm_close_ready", "__omnivm_close_error")
	if !ok {
		t.Fatal("runtimeRefJSStreamCloseStepCode unsupported")
	}
	for _, want := range []string{
		"globalThis.__omnivm_close_ready = false",
		"globalThis.__omnivm_close_error = undefined",
		"Object.getOwnPropertyDescriptor(cursor, name)",
		"descriptor.value.length === 0",
		`__omnivm_iter_return = __omnivm_lifecycleWithoutRequiredArgs(__omnivm_iter, "return")`,
		`__omnivm_iter_cancel = __omnivm_lifecycleWithoutRequiredArgs(__omnivm_iter, "cancel")`,
		"__omnivm_close_step = __omnivm_iter_cancel()",
		"__omnivm_close_step = __omnivm_stream_cancel()",
		`__omnivm_releaseLock = __omnivm_lifecycleWithoutRequiredArgs(__omnivm_iter, "releaseLock")`,
		"return __omnivm_close_step",
		"globalThis.__omnivm_close_ready = true",
		"globalThis.__omnivm_close_error = __omnivm_err",
	} {
		if !contains(code, want) {
			t.Fatalf("runtimeRefJSStreamCloseStepCode missing %q in %q", want, code)
		}
	}
}

func TestRuntimeRefJSStreamCloseStepSkipsRequiredArgLifecycleMethods(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code, ok := runtimeRefJSStreamCloseStepCode(RuntimeRef{Runtime: "javascript", VarName: "rows"}, "__omnivm_stream_state", "__omnivm_close_ready", "__omnivm_close_error")
	if !ok {
		t.Fatal("runtimeRefJSStreamCloseStepCode unsupported")
	}
	script := `
(async function() {
  async function waitClose() {
    await Promise.resolve();
    await Promise.resolve();
  }

  globalThis.rows = {
    returnCalls: 0,
    cancelCalls: 0,
    releaseCalls: 0,
    return: function(reason) {
      this.returnCalls++;
      throw new Error("required return should not run: " + reason);
    },
    cancel: function() {
      this.cancelCalls++;
      return Promise.resolve("cancelled");
    },
    releaseLock: function(reason) {
      this.releaseCalls++;
      throw new Error("required releaseLock should not run: " + reason);
    }
  };
  globalThis.__omnivm_stream_state = globalThis.rows;
` + code + `
  await waitClose();
  if (globalThis.__omnivm_close_ready !== true || globalThis.__omnivm_close_error !== undefined) {
    throw new Error("required return close failed: ready=" + globalThis.__omnivm_close_ready + " error=" + globalThis.__omnivm_close_error);
  }
  if (globalThis.rows.returnCalls !== 0) throw new Error("required return was invoked");
  if (globalThis.rows.cancelCalls !== 1) throw new Error("zero-arg cancel fallback was not invoked");
  if (globalThis.rows.releaseCalls !== 0) throw new Error("required releaseLock was invoked");

  globalThis.rows = {
    returnCalls: 0,
    cancelCalls: 0,
    releaseCalls: 0,
    return: function(reason = undefined) {
      this.returnCalls++;
      return "returned";
    },
    cancel: function() {
      this.cancelCalls++;
      throw new Error("cancel should not run when optional return exists");
    },
    releaseLock: function() {
      this.releaseCalls++;
    }
  };
  globalThis.__omnivm_stream_state = globalThis.rows;
` + code + `
  await waitClose();
  if (globalThis.__omnivm_close_ready !== true || globalThis.__omnivm_close_error !== undefined) {
    throw new Error("optional return close failed: ready=" + globalThis.__omnivm_close_ready + " error=" + globalThis.__omnivm_close_error);
  }
  if (globalThis.rows.returnCalls !== 1) throw new Error("optional return was not invoked");
  if (globalThis.rows.cancelCalls !== 0) throw new Error("cancel ran after optional return");
  if (globalThis.rows.releaseCalls !== 1) throw new Error("zero-arg releaseLock was not invoked");
})().catch(function(err) {
  console.error(err && err.stack || err);
  process.exit(1);
});
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("node JS stream close required-arg guard failed: %v\n%s", err, out)
	}
}

func TestRuntimeRefPythonAsyncStreamCancelWaitsForCloseTask(t *testing.T) {
	e, mocks := makeExecutor("python")
	id, err := e.runtimeRefStreamHandle(RuntimeRef{Runtime: "python", VarName: "rows"})
	if err != nil {
		t.Fatalf("runtimeRefStreamHandle: %v", err)
	}
	ready := false
	mocks["python"].pumpFn = func() { ready = true }
	mocks["python"].evalFn = func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "__aiter__"):
			return pkg.Result{Value: true}
		case strings.Contains(code, "stream_close_ready"):
			if ready {
				return pkg.Result{Value: "True"}
			}
			return pkg.Result{Value: "False"}
		case strings.Contains(code, "stream_close_error"):
			return pkg.Result{Value: nil}
		default:
			return pkg.Result{Value: nil}
		}
	}

	result, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_cancel Python async runtime ref: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Value != true {
		t.Fatalf("stream_cancel Python async runtime ref envelope = %#v, want true", env)
	}
	if mocks["python"].pumpCalls == 0 {
		t.Fatal("stream_cancel did not pump while waiting for Python async close")
	}
	if len(mocks["python"].execCalls) != 1 || !strings.Contains(mocks["python"].execCalls[0], "__omnivm_stream_close_task") {
		t.Fatalf("Python async close execute calls = %#v, want scheduled close task", mocks["python"].execCalls)
	}
}

func TestRuntimeRefPythonStreamCancelAlwaysUsesScheduledCloseTask(t *testing.T) {
	e, mocks := makeExecutor("python")
	id, err := e.runtimeRefStreamHandle(RuntimeRef{Runtime: "python", VarName: "rows"})
	if err != nil {
		t.Fatalf("runtimeRefStreamHandle: %v", err)
	}
	ready := false
	mocks["python"].pumpFn = func() { ready = true }
	mocks["python"].evalFn = func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "stream_close_ready"):
			if ready {
				return pkg.Result{Value: "True"}
			}
			return pkg.Result{Value: "False"}
		case strings.Contains(code, "stream_close_error"):
			return pkg.Result{Value: nil}
		default:
			return pkg.Result{Value: nil}
		}
	}

	result, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_cancel Python runtime ref: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Value != true {
		t.Fatalf("stream_cancel Python runtime ref envelope = %#v, want true", env)
	}
	if mocks["python"].pumpCalls == 0 {
		t.Fatal("stream_cancel did not pump while waiting for Python close")
	}
	if len(mocks["python"].execCalls) != 1 || !strings.Contains(mocks["python"].execCalls[0], "__omnivm_stream_close_task") {
		t.Fatalf("Python close execute calls = %#v, want scheduled close task", mocks["python"].execCalls)
	}
	if strings.Contains(mocks["python"].execCalls[0], "run_until_complete") || strings.Contains(mocks["python"].execCalls[0], "owner dispatch unsupported") {
		t.Fatalf("Python close should not use the synchronous close helper: %s", mocks["python"].execCalls[0])
	}
}

func TestRuntimeRefPythonAsyncStreamCancelErrorKeepsLifecycleTombstone(t *testing.T) {
	e, mocks := makeExecutor("python")
	id, err := e.runtimeRefStreamHandle(RuntimeRef{Runtime: "python", VarName: "rows"})
	if err != nil {
		t.Fatalf("runtimeRefStreamHandle: %v", err)
	}
	ready := false
	mocks["python"].pumpFn = func() { ready = true }
	mocks["python"].evalFn = func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "__aiter__"):
			return pkg.Result{Value: true}
		case strings.Contains(code, "stream_close_ready"):
			if ready {
				return pkg.Result{Value: "True"}
			}
			return pkg.Result{Value: "False"}
		case strings.Contains(code, "stream_close_error"):
			return pkg.Result{Value: "aclose failed"}
		default:
			return pkg.Result{Value: nil}
		}
	}

	if _, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("Python async runtime ref stream_cancel close error did not fail")
	} else if !strings.Contains(err.Error(), "aclose failed") {
		t.Fatalf("Python async runtime ref stream_cancel close error = %v, want aclose failure", err)
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale Python async runtime ref stream_next after close failure did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=python", "kind=stream", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale Python async runtime ref stream_next after close failure missing %q: %s", want, got)
			}
		}
	}
}

func TestRuntimeRefJSStreamCancelWaitsForClosePromise(t *testing.T) {
	e, mocks := makeExecutor("javascript")
	id, err := e.runtimeRefStreamHandle(RuntimeRef{Runtime: "javascript", VarName: "rows"})
	if err != nil {
		t.Fatalf("runtimeRefStreamHandle: %v", err)
	}
	ready := false
	mocks["javascript"].pumpFn = func() { ready = true }
	mocks["javascript"].evalFn = func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "stream_close_ready"):
			return pkg.Result{Value: ready}
		case strings.Contains(code, "stream_close_error"):
			return pkg.Result{Value: nil}
		default:
			return pkg.Result{Value: nil}
		}
	}

	result, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_cancel JS runtime ref: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Value != true {
		t.Fatalf("stream_cancel JS runtime ref envelope = %#v, want true", env)
	}
	if mocks["javascript"].pumpCalls == 0 {
		t.Fatal("stream_cancel did not pump while waiting for JS close")
	}
	if len(mocks["javascript"].execCalls) != 1 || !strings.Contains(mocks["javascript"].execCalls[0], "return __omnivm_close_step") {
		t.Fatalf("JS close execute calls = %#v, want awaited close step", mocks["javascript"].execCalls)
	}
}

func TestHandleCallStreamNextChannel(t *testing.T) {
	e, _ := makeExecutor("javascript")
	ch := &ChanRef{ch: make(chan interface{}, 2)}
	ch.ch <- "first"
	ch.ch <- "second"
	if err := ch.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		t.Fatalf("channelStreamHandle: %v", err)
	}

	for _, want := range []string{"first", "second"} {
		result, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
		if err != nil {
			t.Fatalf("HandleCall stream_next: %v", err)
		}
		env := decodeResultEnvelopeForTest(t, result)
		item, ok := env.Value.(map[string]interface{})
		if !ok || item["done"] == true || item["value"] != want {
			t.Fatalf("stream_next envelope = %#v, want %q", env, want)
		}
	}
	result, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_next done: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	item, ok := env.Value.(map[string]interface{})
	if !ok || item["done"] != true {
		t.Fatalf("stream_next done envelope = %#v, want done", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 3 || stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("stream access/release stats = %+v, want 3 stream reads and release on EOF", stats)
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale stream_next after EOF did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale stream_next after EOF diagnostic missing %q: %s", want, got)
			}
		}
	}
	if _, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale stream_cancel after EOF did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale stream_cancel after EOF diagnostic missing %q: %s", want, got)
			}
		}
	}
	beforeCleanup := e.handleTable.Stats(time.Now())
	result, err = e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("closed stream EOF handle_release_finalizer should remain idempotent: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != false {
		t.Fatalf("closed stream EOF handle_release_finalizer envelope = %#v, want false", env)
	}
	afterCleanup := e.handleTable.Stats(time.Now())
	if afterCleanup.FinalizerQueued != beforeCleanup.FinalizerQueued || afterCleanup.FinalizerQueueLen != beforeCleanup.FinalizerQueueLen || afterCleanup.FinalizerReleases != beforeCleanup.FinalizerReleases {
		t.Fatalf("closed stream EOF finalizer cleanup changed finalizer stats: before=%+v after=%+v", beforeCleanup, afterCleanup)
	}
}

func TestHandleCallStreamNextNativeChannel(t *testing.T) {
	e, _ := makeExecutor("javascript")
	ch := make(chan interface{}, 2)
	ch <- "first"
	ch <- "second"
	close(ch)
	id, err := e.genericStreamHandle("go", ch)
	if err != nil {
		t.Fatalf("genericStreamHandle: %v", err)
	}

	for _, want := range []string{"first", "second"} {
		result, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
		if err != nil {
			t.Fatalf("HandleCall stream_next: %v", err)
		}
		env := decodeResultEnvelopeForTest(t, result)
		item, ok := env.Value.(map[string]interface{})
		if !ok || item["done"] == true || item["value"] != want {
			t.Fatalf("stream_next envelope = %#v, want %q", env, want)
		}
	}
	result, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_next done: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	item, ok := env.Value.(map[string]interface{})
	if !ok || item["done"] != true {
		t.Fatalf("stream_next done envelope = %#v, want done", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.HandleAccessesByKind["stream"] != 3 || stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("native stream access/release stats = %+v, want 3 stream reads and release on EOF", stats)
	}
}

func TestBridgeResultReusesStreamDescriptor(t *testing.T) {
	e, _ := makeExecutor("javascript")
	ch := make(chan interface{}, 1)
	ch <- "first"
	parent := &ResourceRef{Runtime: "go", Kind: "holder", Value: map[string]interface{}{"stream": ch}}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent
	streamID, err := e.genericStreamHandle("go", ch)
	if err != nil {
		t.Fatalf("genericStreamHandle: %v", err)
	}

	got, err := e.bridgeResultValue(parentID, ch)
	if err != nil {
		t.Fatalf("bridgeResultValue: %v", err)
	}
	descriptor, ok := got.(map[string]interface{})
	if !ok {
		t.Fatalf("bridgeResultValue = %T, want stream descriptor", got)
	}
	if descriptor["__omnivm_stream__"] != true || descriptor["id"] != uint64(streamID) || descriptor["kind"] != "channel" {
		t.Fatalf("bridgeResultValue descriptor = %#v, want stream id %d", descriptor, streamID)
	}
}

func TestMarshalReturnResultExportsNativeStreamAsTransfer(t *testing.T) {
	e, _ := makeExecutor("python")
	ch := make(chan interface{}, 1)
	ch <- "first"

	result, err := e.marshalReturnResult(ch)
	if err != nil {
		t.Fatalf("marshal stream return result: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	descriptor, ok := env.Value.(map[string]interface{})
	if !ok || descriptor["__omnivm_stream__"] != true || descriptor["transfer"] != true {
		t.Fatalf("returned stream descriptor = %#v, want transfer-marked stream proxy", env)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("stream resource id: %v", err)
	}
	entry, live := e.ensureHandleTable().Get(id)
	if !live || !entry.Escaped || entry.StrongRefs != 1 {
		t.Fatalf("returned stream entry = %+v live=%v, want escaped transfer reference only", entry, live)
	}

	if _, err := e.HandleCall(`{"op":"handle_adopt","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err != nil {
		t.Fatalf("HandleCall handle_adopt: %v", err)
	}
	entry, live = e.ensureHandleTable().Get(id)
	if !live || entry.StrongRefs != 1 {
		t.Fatalf("adopted stream entry = %+v live=%v, want one finalizer-owned reference", entry, live)
	}
	if _, err := e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err != nil {
		t.Fatalf("HandleCall handle_release_finalizer: %v", err)
	}
	if err := e.ensureHandleTable().DrainFinalizerReleases(0); err != nil {
		t.Fatalf("DrainFinalizerReleases: %v", err)
	}
	if _, live := e.ensureHandleTable().Get(id); live {
		t.Fatalf("adopted returned stream handle %d remained live after proxy finalizer", id)
	}
}

func TestHandleCallStreamCancelReleasesChannel(t *testing.T) {
	e, _ := makeExecutor("javascript")
	ch := &ChanRef{ch: make(chan interface{}, 1)}
	ch.ch <- "first"
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		t.Fatalf("channelStreamHandle: %v", err)
	}
	result, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall stream_cancel: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Value != true {
		t.Fatalf("stream_cancel envelope = %#v, want true", env)
	}
	stats := e.handleTable.Stats(time.Now())
	if stats.Live != 0 || stats.ExplicitReleases != 1 {
		t.Fatalf("stream_cancel stats = %+v, want explicit release", stats)
	}
	parentID, err := e.ensureHandleTable().Register(map[string]interface{}{"owner": "stream-parent"}, handles.RegisterOptions{
		Runtime: "javascript",
		Kind:    "resource",
	})
	if err != nil {
		t.Fatalf("register stream parent handle: %v", err)
	}
	if _, err := e.HandleCall(`{"op":"stream_next","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale stream_next after cancel did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale stream_next diagnostic missing %q: %s", want, got)
			}
		}
	}
	if _, err := e.HandleCall(`{"op":"stream_cancel","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("stale stream_cancel after cancel did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("stale stream_cancel diagnostic missing %q: %s", want, got)
			}
		}
		if strings.Contains(got, "unknown handle") {
			t.Fatalf("stale stream_cancel used generic handle-table diagnostic: %s", got)
		}
	}
	if _, err := e.HandleCall(`{"op":"handle_release_explicit","id":` + strconv.FormatUint(uint64(id), 10) + `}`); err == nil {
		t.Fatal("closed stream handle_release_explicit did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("closed stream handle_release_explicit diagnostic missing %q: %s", want, got)
			}
		}
	}
	beforeCleanup := e.handleTable.Stats(time.Now())
	result, err = e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("closed stream handle_release_finalizer should remain idempotent: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != false {
		t.Fatalf("closed stream handle_release_finalizer envelope = %#v, want false", env)
	}
	afterCleanup := e.handleTable.Stats(time.Now())
	if afterCleanup.FinalizerQueued != beforeCleanup.FinalizerQueued || afterCleanup.FinalizerQueueLen != beforeCleanup.FinalizerQueueLen || afterCleanup.FinalizerReleases != beforeCleanup.FinalizerReleases {
		t.Fatalf("closed stream finalizer cleanup changed finalizer stats: before=%+v after=%+v", beforeCleanup, afterCleanup)
	}
	for _, call := range []string{
		`{"op":"handle_retain","id":%d}`,
		`{"op":"handle_adopt","id":%d}`,
		`{"op":"handle_access","id":%d,"kind":"stream"}`,
	} {
		_, err := e.HandleCall(fmt.Sprintf(call, id))
		if err == nil {
			t.Fatalf("closed stream meta call %s did not fail", call)
		}
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("closed stream meta call %s diagnostic missing %q: %s", call, want, got)
			}
		}
	}
	for _, call := range []string{
		fmt.Sprintf(`{"op":"handle_reference","from":%d,"to":%d,"kind":"stream"}`, id, parentID),
		fmt.Sprintf(`{"op":"handle_reference","from":%d,"to":%d,"kind":"stream"}`, parentID, id),
	} {
		_, err := e.HandleCall(call)
		if err == nil {
			t.Fatalf("closed stream reference call %s did not fail", call)
		}
		got := err.Error()
		for _, want := range []string{"closed stream handle", "runtime=go", "kind=channel", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("closed stream reference call %s diagnostic missing %q: %s", call, want, got)
			}
		}
		if strings.Contains(got, "unknown source handle") || strings.Contains(got, "unknown target handle") {
			t.Fatalf("closed stream reference call %s used generic handle-table diagnostic: %s", call, got)
		}
	}
	for _, call := range []string{
		fmt.Sprintf(`{"op":"handle_drop_reference","from":%d,"to":%d}`, id, parentID),
		fmt.Sprintf(`{"op":"handle_drop_reference","from":%d,"to":%d}`, parentID, id),
	} {
		result, err := e.HandleCall(call)
		if err != nil {
			t.Fatalf("closed stream handle_drop_reference cleanup %s should remain idempotent: %v", call, err)
		}
		env := decodeResultEnvelopeForTest(t, result)
		if env.Kind != "bool" || env.Value != true {
			t.Fatalf("closed stream handle_drop_reference envelope = %#v, want true", env)
		}
	}
	if len(ch.ch) != 1 {
		t.Fatalf("stream_cancel should not drain channel, len = %d, want 1", len(ch.ch))
	}
}

// --- String escaping tests ---

func TestEscapePythonString(t *testing.T) {
	got := escapePythonString(`it's a "test" with \backslash`)
	if !contains(got, `\'`) {
		t.Error("should escape single quotes")
	}
	if !contains(got, `\\`) {
		t.Error("should escape backslashes")
	}
}

func TestEscapeJavaString(t *testing.T) {
	got := escapeJavaString(`say "hello"`)
	if !contains(got, `\"`) {
		t.Error("should escape double quotes")
	}
}

// --- Capture wrapping tests ---

func TestDescriptorInternalKeyContractFeedsGeneratedProxyMaterializers(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{name: "python", code: pythonCaptureMaterializer()},
		{name: "javascript", code: jsChannelMaterializer()},
		{name: "ruby", code: rubyCaptureMaterializer()},
	} {
		if contains(tc.code, descriptorInternalKeysMarker) {
			t.Fatalf("%s materializer still contains descriptor-key marker", tc.name)
		}
		for _, group := range descriptorInternalKeyGroups {
			for _, key := range group {
				if !contains(tc.code, strconv.Quote(key)) {
					t.Fatalf("%s materializer missing shared descriptor key %q", tc.name, key)
				}
			}
		}
	}
}

func TestWrapPythonCaptures(t *testing.T) {
	code := wrapPythonCaptures("print(x)", map[string]string{"x": "42"})
	if !contains(code, "__json.loads") {
		t.Error("should use json.loads")
	}
	if !contains(code, "__omnivm_materialize_capture") {
		t.Error("should materialize captures")
	}
	if !contains(code, "print(x)") {
		t.Error("should include user code")
	}
}

func TestWrapPythonCapturesUsesSafeBindingNames(t *testing.T) {
	code := wrapPythonCaptures(`print(globals()["class"])`, map[string]string{"class": "42"})
	if !contains(code, `globals()["class"] = __captures['class']`) {
		t.Fatalf("Python wrapper should assign unsafe names through globals(), got %q", code)
	}
	if contains(code, "class = __captures") {
		t.Fatalf("Python wrapper should not emit invalid local assignment, got %q", code)
	}
}

func TestInjectPythonCapturesMaterializesHandleProxy(t *testing.T) {
	code := injectPythonCaptures(map[string]string{
		"req": `{"__omnivm_resource__":true,"id":7,"runtime":"python","kind":"request","closed":false}`,
	})
	if !contains(code, "class __OmniVMHandleProxy") {
		t.Fatalf("Python materializer should define handle proxy, got %q", code)
	}
	if !contains(code, `"op": "handle_access"`) {
		t.Fatalf("Python materializer should record handle access, got %q", code)
	}
	if !contains(code, "chatty cross-runtime proxy access detected") {
		t.Fatalf("Python materializer should warn on chatty proxy access, got %q", code)
	}
	if !contains(code, "_omnivm_chatty_warned_limit = 4096") || !contains(code, "len(warned) > limit") {
		t.Fatalf("Python materializer should bound chatty warning dedupe entries, got %q", code)
	}
	if !contains(code, "def _materialize_chatty") || !contains(code, `"materialize": True`) {
		t.Fatalf("Python materializer should automatically batch-materialize chatty proxy items, got %q", code)
	}
	if !contains(code, `"op": "handle_get"`) {
		t.Fatalf("Python materializer should fetch handle properties, got %q", code)
	}
	if !contains(code, "class _OmniVMBridgeMissing") || !contains(code, "def _omnivm_is_missing_bridge_error") || !contains(code, "raise globals()[\"_OmniVMBridgeMissing\"]") {
		t.Fatalf("Python materializer should distinguish missing remote fields from lifecycle bridge failures, got %q", code)
	}
	if !contains(code, `value.get("zeroArg") is True`) || !contains(code, `return self._bridge_call(key, (), {})`) {
		t.Fatalf("Python materializer should invoke zero-arg callable descriptors as property access, got %q", code)
	}
	if !contains(code, `"op": "handle_index"`) || !contains(code, `"op": "handle_set"`) || !contains(code, `"op": "handle_call"`) || !contains(code, `"op": "handle_len"`) || !contains(code, `"op": "handle_iter"`) || !contains(code, `"op": "handle_contains"`) {
		t.Fatalf("Python materializer should forward generic index/set/call/len/iter/contains operations, got %q", code)
	}
	if !contains(code, "return self._bridge_index(key)") {
		t.Fatalf("Python materializer should fall back from attribute access to generic index access, got %q", code)
	}
	if !contains(code, "def __getattribute__(self, key)") || !contains(code, `object.__getattribute__(self, "__getattr__")(key)`) {
		t.Fatalf("Python materializer should route public proxy method-name collisions through remote lookup first, got %q", code)
	}
	if !contains(code, "def _is_internal_descriptor_key(self, key)") ||
		!contains(code, `self._value.get("__omnivm_resource__") is True`) ||
		!contains(code, `or self._value.get("__omnivm_table__") is True`) ||
		!contains(code, `or self._value.get("__omnivm_job__") is True`) ||
		!contains(code, `"format", "ownership", "metadata", "buffer", "released"`) ||
		!contains(code, `"done", "cancelled", "cancelReason", "payload", "result"`) ||
		!contains(code, "return self._has_local_value(key) or self._has_local_text_value(key)") {
		t.Fatalf("Python proxy should keep resource/table/job descriptor metadata out of user-visible fields, got %q", code)
	}
	if !contains(code, "def _omnivm_encode_arg") || !contains(code, `"__omnivm_runtime_ref__"`) || !contains(code, `[_omnivm_encode_arg(arg) for arg in args]`) {
		t.Fatalf("Python proxy calls should preserve complex args as runtime refs, got %q", code)
	}
	if !contains(code, `payload["kwargs"] = {str(k): _omnivm_encode_arg(v) for k, v in kwargs.items()}`) || !contains(code, "def __call__(self, *args, **kwargs)") {
		t.Fatalf("Python proxy calls should forward keyword args, got %q", code)
	}
	if !contains(code, `mode = "values" if self._value.get("kind") == "sequence" or self._value.get("__omnivm_table__") is True else "keys"`) {
		t.Fatalf("Python materializer should iterate sequence proxies by value and mapping proxies by key, got %q", code)
	}
	if !contains(code, `"op": "handle_release_finalizer"`) || !contains(code, "weakref") || !contains(code, "__omnivm_release_handle_id") {
		t.Fatalf("Python materializer should queue weakref finalizer releases, got %q", code)
	}
	if !contains(code, `"op": "handle_retain"`) || !contains(code, "__omnivm_retain_handle_id") {
		t.Fatalf("Python materializer should retain handles for guest proxy lifetime, got %q", code)
	}
	if !contains(code, "def omnivm_close(value):") ||
		!contains(code, "def proxy_close(value):") ||
		!contains(code, "async def aproxy_close(value):") ||
		!contains(code, "async def omnivm_aclose(value):") ||
		!contains(code, `def __omnivm_actual_public_method(value, name):`) ||
		!contains(code, `__inspect.getattr_static(value, name)`) ||
		!contains(code, `isinstance(raw, (staticmethod, classmethod))`) ||
		!contains(code, `method = raw.__get__(value, type(value))`) ||
		!contains(code, `__inspect.ismemberdescriptor(raw)`) ||
		!contains(code, `if not callable(raw):`) ||
		!contains(code, `instance_dict = object.__getattribute__(value, "__dict__")`) ||
		!contains(code, `instance_dict.get(name) is raw`) ||
		!contains(code, `__inspect.isfunction(raw)`) ||
		!contains(code, `__inspect.ismethoddescriptor(raw)`) ||
		!contains(code, "def __omnivm_lifecycle_method_accepts_no_args(method):") ||
		!contains(code, "__inspect.signature(method)") ||
		!contains(code, "parameter.default is __inspect.Signature.empty") ||
		!contains(code, `close = __omnivm_actual_public_method(value, "_omnivm_close")`) ||
		!contains(code, `close = __omnivm_actual_public_method(value, "close")`) ||
		!contains(code, `close = __omnivm_actual_public_method(value, "dispose")`) ||
		!contains(code, `close = __omnivm_actual_public_method(value, "aclose")`) ||
		!contains(code, `callable(close) and __omnivm_lifecycle_method_accepts_no_args(close)`) ||
		!contains(code, `__omnivm_inspect.isawaitable(result)`) ||
		!contains(code, "return omnivm_close(value)") ||
		!contains(code, "return await aproxy_close(value)") ||
		!contains(code, "result = close()\n        return True if result is None else result") ||
		!contains(code, "def cleanup_errors(error):") ||
		!contains(code, "def _omnivm_record_cleanup_error(error, cleanup_error, note):") ||
		!contains(code, `setattr(error, "omnivm_cleanup_errors", errors)`) ||
		!contains(code, "return list(errors) if isinstance(errors, list) else []") ||
		!contains(code, `result = self._bridge({"op": "handle_release_explicit"})`) ||
		!contains(code, "released = bool(result)\n        if released:\n            self._mark_closed()") ||
		!contains(code, "if object.__getattribute__(self, \"_closed\"):\n            return False") ||
		!contains(code, "finalizer.detach()") ||
		!contains(code, "def __enter__(self):\n        return self") ||
		!contains(code, "def __exit__(self, _exc_type, exc, _tb):") ||
		!contains(code, "if _exc_type is None:\n            self._omnivm_close()") ||
		!contains(code, "try:\n            self._omnivm_close()") ||
		!contains(code, "f\"OmniVM proxy close failed during exception cleanup: {close_exc}\"") {
		t.Fatalf("Python handle proxy should expose idempotent explicit close without relying on finalizers, got %q", code)
	}
	if contains(code, `getattr(value, "close", None)`) || contains(code, `getattr(value, "_omnivm_close", None)`) || contains(code, `getattr(value, name, None)`) || contains(code, `getattr(value, name)`) {
		t.Fatalf("Python proxy close helper should not invoke dynamic attribute lookup for lifecycle methods")
	}
	apreferred := code[strings.Index(code, "async def aproxy_close(value):"):]
	apreferred = apreferred[:strings.Index(apreferred, "async def omnivm_aclose(value):")]
	if strings.Index(apreferred, `close = __omnivm_actual_public_method(value, "aclose")`) >
		strings.Index(apreferred, `close = __omnivm_actual_public_method(value, "dispose")`) {
		t.Fatalf("Python async close helper should prefer aclose before dispose, got %q", apreferred)
	}
	if !contains(code, `"op": "handle_adopt"`) || !contains(code, "__omnivm_adopt_handle_id") || !contains(code, `value.get("transfer") is True`) {
		t.Fatalf("Python materializer should adopt returned transfer handles, got %q", code)
	}
	if !contains(code, "WeakValueDictionary") || !contains(code, "__omnivm_proxy_cache") || !contains(code, `cache_key = ("handle", handle_id)`) {
		t.Fatalf("Python materializer should weakly cache handle proxies by identity, got %q", code)
	}
	if !contains(code, `if cached is not None and not getattr(cached, "_closed", False):`) {
		t.Fatalf("Python materializer should not reuse closed cached proxies, got %q", code)
	}
	if !contains(code, "req = __omnivm_materialize_capture(") {
		t.Fatalf("Python capture should be materialized during injection, got %q", code)
	}
	if !contains(code, `or value.get("__omnivm_stream__") is True`) || !contains(code, `return globals()["__omnivm_materialize_capture"](value)`) {
		t.Fatalf("Python bridge results should materialize returned stream descriptors, got %q", code)
	}
	if !contains(code, "def __len__(self):") || !contains(code, "return len(self._materialize_all())") || !contains(code, "def __getitem__(self, key):") {
		t.Fatalf("Python stream proxy should auto-materialize for len/index operations, got %q", code)
	}
	if !contains(code, "def _mark_closed(self):") ||
		!contains(code, "def __next__(self):\n        if self._closed:\n            raise StopIteration") ||
		!contains(code, `self._local_values = values if isinstance(values, list) else None`) ||
		!contains(code, "if self._local_values is not None:\n            if len(self._cache) >= len(self._local_values):") ||
		!contains(code, "materialized = globals()[\"__omnivm_materialize_capture\"](self._local_values[len(self._cache)])") ||
		!contains(code, "materialized = globals()[\"__omnivm_materialize_capture\"](item.get(\"value\"))") ||
		!contains(code, "self._cache.append(materialized)") ||
		!contains(code, "class _OmniVMRuntimeError(RuntimeError):") ||
		!contains(code, "def _omnivm_runtime_error(message, boundary_path, details=None):") ||
		!contains(code, "return runtime_error(message, runtime=\"python\", boundary_path=boundary_path, details=details)") ||
		!contains(code, "err = _omnivm_runtime_error(") ||
		!contains(code, `{"stream": {"id": self._value.get("id"), "chunk": item}}`) ||
		!contains(code, "if isinstance(value, tuple):") ||
		!contains(code, "def _omnivm_traceback_frames(error):") ||
		!contains(code, "return __tb.format_tb(error.__traceback__) if error.__traceback__ is not None else []") ||
		!contains(code, "def traceback(self):") ||
		!contains(code, "def traceback(self, value):") ||
		!contains(code, "def stack_frames(self, value):") ||
		!contains(code, "def stackFrames(self, value):") ||
		!contains(code, "def originRuntime(self):") ||
		!contains(code, "def originRuntime(self, value):") ||
		!contains(code, "def cause_chain(self, value):") ||
		!contains(code, "def causeChain(self, value):") ||
		!contains(code, "def boundaryPath(self, value):") ||
		!contains(code, "def originalErrorHandle(self, value):") ||
		!contains(code, "def to_json(self):") ||
		!contains(code, "def details_json(self, value):") ||
		!contains(code, "try:\n                    self.close()\n                except Exception as close_exc:\n                    _omnivm_record_cleanup_error") ||
		!contains(code, "f\"OmniVM stream close failed during chunk materialization cleanup: {close_exc}\"") ||
		!contains(code, "def __iter__(self):\n        def __omnivm_iter():") ||
		!contains(code, "finally:\n                if not self._closed:") ||
		!contains(code, "self.close()\n                    except Exception:\n                        pass") ||
		!contains(code, "if finalizer is not None and finalizer.alive:") ||
		!contains(code, "finalizer.detach()") ||
		!contains(code, "except Exception:\n            self._mark_closed()\n            raise") ||
		!contains(code, "if self._closed:\n            return False") ||
		!contains(code, "if self._local_values is not None:\n            return self._mark_closed()") ||
		!contains(code, `"op": "stream_cancel"`) ||
		!contains(code, "released = isinstance(env, dict) and env.get(\"__omnivm_result__\") is True and env.get(\"value\") is True") ||
		!contains(code, "if released:\n            self._mark_closed()\n        return released") ||
		!contains(code, "def _omnivm_close(self):\n        return self.close()") ||
		!contains(code, "def __enter__(self):\n        return self") ||
		!contains(code, "def __exit__(self, _exc_type, exc, _tb):") ||
		!contains(code, "f\"OmniVM stream close failed during exception cleanup: {close_exc}\"") {
		t.Fatalf("Python stream proxy close should be explicit, idempotent, return the manifest release result, and detach finalizers after success, got %q", code)
	}
	if contains(code, "def close(self):\n        try:\n            caller = globals()[\"__omnivm_bridge_module\"]()") {
		t.Fatalf("Python stream close should not swallow user-initiated cancellation failures")
	}
}

func TestInjectPythonCapturesUsesSafeBindingNames(t *testing.T) {
	code := injectPythonCaptures(map[string]string{"class": "42"})
	if !contains(code, `globals()["class"] = __omnivm_materialize_capture(__json.loads('42'))`) {
		t.Fatalf("Python capture injection should assign unsafe names through globals(), got %q", code)
	}
}

func TestPythonHandleProxyLifecycleNameCollisionsPreferRemoteFields(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    fields = {
        "close": "remote-close-field",
        "dispose": "remote-dispose-field",
    }
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_access":
            return json.dumps({"__omnivm_result__": True, "value": {"chatty": False}})
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "handle_release_explicit":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "handle_contains":
            return json.dumps({"__omnivm_result__": True, "value": req["value"] in Bridge.fields})
        if req["op"] == "handle_get" and req["key"] in Bridge.fields:
            return json.dumps({"__omnivm_result__": True, "value": Bridge.fields[req["key"]]})
        if req["op"] == "handle_get":
            raise RuntimeError("resource has no property " + req["key"])
        if req["op"] == "handle_call" and req["key"] in ("close", "dispose"):
            raise RuntimeError("lifecycle helper called remote " + req["key"] + " field")
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
proxy = __omnivm_materialize_capture({"__omnivm_resource__": True, "id": 80, "runtime": "python", "kind": "object"})
if proxy.close != "remote-close-field":
    raise RuntimeError("close was not remote-first: " + repr(proxy.close))
if proxy.dispose != "remote-dispose-field":
    raise RuntimeError("dispose was not remote-first: " + repr(proxy.dispose))
if proxy.get("close") != "remote-close-field":
    raise RuntimeError("get did not recover remote close")
if proxy.get("dispose") != "remote-dispose-field":
    raise RuntimeError("get did not recover remote dispose")
if omnivm_close(proxy) is not True:
    raise RuntimeError("omnivm_close did not release the proxy")
if omnivm_close(proxy) is not False:
    raise RuntimeError("omnivm_close was not idempotent after release")
requested = [req["key"] for req in Bridge.requests if req["op"] == "handle_get"]
for key in ("close", "dispose"):
    if key not in requested:
        raise RuntimeError("missing remote lookup for " + key + ": " + repr(requested))
releases = [req for req in Bridge.requests if req["op"] == "handle_release_explicit"]
if len(releases) != 1 or releases[0]["id"] != 80:
    raise RuntimeError("explicit release mismatch: " + repr(releases))
if any(req["op"] == "handle_call" and req.get("key") in ("close", "dispose") for req in Bridge.requests):
    raise RuntimeError("lifecycle close dispatched to a remote lifecycle-named field")
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python lifecycle-name collision check failed: %v\n%s", err, out)
	}
}

func TestPythonCaptureProxyCacheSkipsClosedProxy(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] in {"handle_adopt", "handle_release_explicit"}:
            return json.dumps({"__omnivm_result__": True, "value": True})
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
descriptor = {"__omnivm_resource__": True, "id": 91, "runtime": "python", "kind": "object", "transfer": True}
first = __omnivm_materialize_capture(descriptor)
if first._omnivm_close() is not True:
    raise RuntimeError("first close failed")
second = __omnivm_materialize_capture(descriptor)
if second is first:
    raise RuntimeError("closed proxy was reused")
if second._omnivm_close() is not True:
    raise RuntimeError("second close failed")
adopts = [req for req in Bridge.requests if req["op"] == "handle_adopt"]
releases = [req for req in Bridge.requests if req["op"] == "handle_release_explicit"]
if adopts != [{"op": "handle_adopt", "id": 91}, {"op": "handle_adopt", "id": 91}]:
    raise RuntimeError("adopt requests mismatch: " + repr(adopts))
if releases != [{"op": "handle_release_explicit", "id": 91}, {"op": "handle_release_explicit", "id": 91}]:
    raise RuntimeError("release requests mismatch: " + repr(releases))
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python closed proxy cache check failed: %v\n%s", err, out)
	}
}

func TestPythonHandleProxyContextPreservesBodyExceptionWhenCloseFails(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "handle_release_explicit":
            raise RuntimeError("release failed")
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
proxy = __omnivm_materialize_capture({"__omnivm_resource__": True, "id": 91, "runtime": "python", "kind": "request"})
try:
    with proxy:
        raise ValueError("body failed")
except ValueError as exc:
    if "body failed" not in str(exc):
        raise
    notes = getattr(exc, "__notes__", [])
    if not any("release failed" in note for note in notes):
        raise RuntimeError("close failure note missing: " + repr(notes))
    cleanup = cleanup_errors(exc)
    if len(cleanup) != 1 or str(cleanup[0]) != "release failed":
        raise RuntimeError("cleanup error missing: " + repr(cleanup))
    cleanup.clear()
    if str(cleanup_errors(exc)[0]) != "release failed":
        raise RuntimeError("cleanup_errors returned internal storage")
else:
    raise RuntimeError("body exception was not preserved")
releases = [req for req in Bridge.requests if req.get("op") == "handle_release_explicit"]
if len(releases) != 1 or releases[0].get("id") != 91:
    raise RuntimeError("explicit release requests mismatch: " + repr(releases))
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python handle context close-failure check failed: %v\n%s", err, out)
	}
}

func TestPythonTableDescriptorFieldsPreferRemoteFields(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    fields = {
        "id": "remote-id",
        "runtime": "remote-runtime",
        "format": "remote-format",
        "metadata": {"remote": True},
        "buffer": "remote-buffer",
    }
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_access":
            return json.dumps({"__omnivm_result__": True, "value": {"chatty": False}})
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "handle_contains":
            return json.dumps({"__omnivm_result__": True, "value": req["value"] in Bridge.fields})
        if req["op"] == "handle_get" and req["key"] in Bridge.fields:
            return json.dumps({"__omnivm_result__": True, "value": Bridge.fields[req["key"]]})
        if req["op"] == "handle_get":
            raise RuntimeError("table has no property " + req["key"])
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
proxy = __omnivm_materialize_capture({
    "__omnivm_table__": True,
    "id": 81,
    "runtime": "python",
    "format": "arrow_c_data",
    "ownership": "borrowed",
    "metadata": {"dtype": 4},
    "buffer": "descriptor-buffer",
    "released": False,
})
if proxy.id != "remote-id":
    raise RuntimeError("id was not remote-first: " + repr(proxy.id))
if proxy.runtime != "remote-runtime":
    raise RuntimeError("runtime was not remote-first: " + repr(proxy.runtime))
if proxy.format != "remote-format":
    raise RuntimeError("format was not remote-first: " + repr(proxy.format))
if proxy.metadata != {"remote": True}:
    raise RuntimeError("metadata was not remote-first: " + repr(proxy.metadata))
if proxy.buffer != "remote-buffer":
    raise RuntimeError("buffer was not remote-first: " + repr(proxy.buffer))
descriptor = object.__getattribute__(proxy, "_value")
if descriptor["metadata"]["dtype"] != 4 or descriptor["format"] != "arrow_c_data" or descriptor["buffer"] != "descriptor-buffer":
    raise RuntimeError("local descriptor changed: " + repr(descriptor))
requested = [req["key"] for req in Bridge.requests if req["op"] == "handle_get"]
for key in ("id", "runtime", "format", "metadata", "buffer"):
    if key not in requested:
        raise RuntimeError("missing remote lookup for " + key + ": " + repr(requested))
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python table descriptor field collision check failed: %v\n%s", err, out)
	}
}

func TestPythonRemoteStreamCancelsOnEarlyBreak(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "stream_next":
            return json.dumps({"__omnivm_result__": True, "value": {"done": False, "value": "a"}})
        if req["op"] == "stream_cancel":
            return json.dumps({"__omnivm_result__": True, "value": True})
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
stream = __omnivm_materialize_capture({"__omnivm_stream__": True, "id": 88, "runtime": "python", "kind": "stream"})
seen = []
for item in stream:
    seen.append(item)
    break
if seen != ["a"]:
    raise RuntimeError("first item mismatch: " + repr(seen))
if not stream._closed:
    raise RuntimeError("stream was not marked closed")
if stream._omnivm_close() is not False:
    raise RuntimeError("_omnivm_close was not idempotent")
if stream.close() is not False:
    raise RuntimeError("close was not idempotent")
if not any(req.get("op") == "handle_retain" and req.get("id") == 88 for req in Bridge.requests):
    raise RuntimeError("stream handle was not retained")
cancels = [req for req in Bridge.requests if req.get("op") == "stream_cancel"]
if len(cancels) != 1 or cancels[0].get("id") != 88:
    raise RuntimeError("stream cancel requests mismatch: " + repr(cancels))
loaded = __omnivm_materialize_capture({"__omnivm_stream__": True, "id": 89, "runtime": "python", "kind": "stream"})
if loaded._pull_next() is not True:
    raise RuntimeError("prefetch did not load an item")
if loaded.close() is not True:
    raise RuntimeError("prefetched stream did not close")
try:
    next(loaded)
    raise RuntimeError("closed stream returned prefetched item")
except StopIteration:
    pass
loaded_cancels = [req for req in Bridge.requests if req.get("op") == "stream_cancel" and req.get("id") == 89]
if len(loaded_cancels) != 1:
    raise RuntimeError("prefetched close cancel mismatch: " + repr(loaded_cancels))
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python remote stream early-break cancellation check failed: %v\n%s", err, out)
	}
}

func TestPythonRemoteStreamRejectsMalformedNextChunk(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "stream_next":
            return json.dumps({"__omnivm_result__": True, "value": ""})
        if req["op"] == "stream_cancel":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "handle_release_finalizer":
            raise RuntimeError("malformed stream chunk should cancel explicitly")
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
stream = __omnivm_materialize_capture({"__omnivm_stream__": True, "id": 90, "runtime": "python", "kind": "stream"})
try:
    next(stream)
    raise RuntimeError("malformed stream chunk was treated as a value or EOF")
except RuntimeError as exc:
    if "stream_next returned malformed chunk" not in str(exc):
        raise
    if getattr(exc, "runtime", None) != "python":
        raise RuntimeError("malformed stream runtime mismatch: " + repr(getattr(exc, "runtime", None)))
    if getattr(exc, "originRuntime", None) != "python":
        raise RuntimeError("malformed stream originRuntime alias mismatch: " + repr(getattr(exc, "originRuntime", None)))
    if getattr(exc, "boundary_path", None) != "stream_next":
        raise RuntimeError("malformed stream boundary mismatch: " + repr(getattr(exc, "boundary_path", None)))
    if not exc.stack_frames:
        raise RuntimeError("malformed stream error missing stack frames")
    if "in _pull_next" not in exc.traceback:
        raise RuntimeError("malformed stream traceback missing origin frame: " + repr(exc.traceback))
    exc.originRuntime = "owner-python"
    exc.boundaryPath = "stream_next > normalized"
    exc.originalErrorHandle = "py-stream-90"
    exc.stackFrames = ("normalized stack",)
    exc.causeChain = ({"runtime": "python", "message": "inner"},)
    exc.traceback = "normalized traceback"
    if exc.details != {"stream": {"id": 90, "chunk": ""}}:
        raise RuntimeError("malformed stream details mismatch: " + repr(exc.details))
    details = exc.details
    details["stream"]["id"] = -1
    if exc.details != {"stream": {"id": 90, "chunk": ""}}:
        raise RuntimeError("malformed stream details reader leaked mutable state")
    exc.details_json = '{"stream":{"id":91,"chunk":"json"}}'
    if exc.details != {"stream": {"id": 91, "chunk": "json"}}:
        raise RuntimeError("details_json setter did not update details: " + repr(exc.details))
    exc.detailsJson = {"stream": {"id": 92, "chunk": "alias"}}
    if json.loads(exc.details_json) != {"stream": {"id": 92, "chunk": "alias"}}:
        raise RuntimeError("detailsJson setter did not update details_json: " + repr(exc.details_json))
    envelope = json.loads(exc.to_json())
    if envelope["origin_runtime"] != "owner-python":
        raise RuntimeError("malformed stream json originRuntime mismatch: " + repr(envelope))
    if envelope["boundary_path"] != "stream_next > normalized":
        raise RuntimeError("malformed stream json boundary mismatch: " + repr(envelope))
    if envelope["original_error_handle"] != "py-stream-90":
        raise RuntimeError("malformed stream json original handle mismatch: " + repr(envelope))
    if envelope["traceback"] != "normalized traceback":
        raise RuntimeError("malformed stream json traceback mismatch: " + repr(envelope))
    if envelope["stack_frames"] != ["normalized stack"]:
        raise RuntimeError("malformed stream json stack_frames missing: " + repr(envelope))
    if envelope["cause_chain"] != [{"runtime": "python", "message": "inner"}]:
        raise RuntimeError("malformed stream json cause_chain mismatch: " + repr(envelope))
    if envelope["details"] != {"stream": {"id": 92, "chunk": "alias"}}:
        raise RuntimeError("malformed stream json details mismatch: " + repr(envelope))
if not stream._closed:
    raise RuntimeError("stream was not marked closed after malformed chunk")
if stream.close() is not False:
    raise RuntimeError("close after malformed chunk should be idempotent false")
cancels = [req for req in Bridge.requests if req.get("op") == "stream_cancel"]
if len(cancels) != 1 or cancels[0].get("id") != 90:
    raise RuntimeError("malformed chunk cancel mismatch: " + repr(cancels))
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python remote stream malformed-chunk check failed: %v\n%s", err, out)
	}
}

func TestPythonCaptureOmnivmClosePreservesLocalCloseResult(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := code + `
class FalseCloser:
    def close(self):
        return False

class TextCloser:
    def close(self):
        return "closed"

class NoneCloser:
    def close(self):
        return None

class TextDispose:
    def dispose(self):
        return "disposed"

class FalseDispose:
    def dispose(self):
        return False

class NoneDispose:
    def dispose(self):
        return None

class CloseAndDispose:
    def close(self):
        return "closed"

    def dispose(self):
        raise RuntimeError("dispose should not run when close exists")

class RequiredCloseAndDispose:
    def close(self, reason):
        raise RuntimeError("required-arg close should not run")

    def dispose(self):
        return "disposed-after-required-close"

class RequiredOnlyClose:
    def close(self, reason):
        raise RuntimeError("required-arg close should not run")

class BothClosers:
    def _omnivm_close(self):
        return "omnivm-closed"

    def close(self):
        return "public-close"

class DynamicCloseTrap:
    dynamic_lookup_count = 0

    def __getattr__(self, name):
        if name == "close":
            self.dynamic_lookup_count += 1
            return lambda: "dynamic-close"
        raise AttributeError(name)

class DynamicDisposeTrap:
    dynamic_lookup_count = 0

    def __getattr__(self, name):
        if name == "dispose":
            self.dynamic_lookup_count += 1
            return lambda: "dynamic-dispose"
        raise AttributeError(name)

trap = DynamicCloseTrap()
dispose_trap = DynamicDisposeTrap()
if omnivm_close(FalseCloser()) is not False:
    raise RuntimeError("false close result was not preserved")
if omnivm_close(TextCloser()) != "closed":
    raise RuntimeError("string close result was not preserved")
if omnivm_close(NoneCloser()) is not True:
    raise RuntimeError("None close result should normalize to true")
if omnivm_close(TextDispose()) != "disposed":
    raise RuntimeError("string dispose result was not preserved")
if omnivm_close(FalseDispose()) is not False:
    raise RuntimeError("false dispose result was not preserved")
if omnivm_close(NoneDispose()) is not True:
    raise RuntimeError("None dispose result should normalize to true")
if omnivm_close(CloseAndDispose()) != "closed":
    raise RuntimeError("close should take priority over dispose")
if omnivm_close(RequiredCloseAndDispose()) != "disposed-after-required-close":
    raise RuntimeError("required-arg close should be skipped for dispose")
if omnivm_close(RequiredOnlyClose()) is not False:
    raise RuntimeError("required-arg close should not be treated as a lifecycle close")
if proxy_close(TextCloser()) != "closed":
    raise RuntimeError("proxy_close alias did not preserve close result")
if omnivm_close(BothClosers()) != "omnivm-closed":
    raise RuntimeError("collision-safe _omnivm_close was not preferred")
if omnivm_close(trap) is not False:
    raise RuntimeError("dynamic close lookup should not be used")
if trap.dynamic_lookup_count != 0:
    raise RuntimeError("dynamic close lookup was invoked")
if omnivm_close(dispose_trap) is not False:
    raise RuntimeError("dynamic dispose lookup should not be used")
if dispose_trap.dynamic_lookup_count != 0:
    raise RuntimeError("dynamic dispose lookup was invoked")
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python generated omnivm_close return preservation check failed: %v\n%s", err, out)
	}
}

func TestPythonCaptureOmnivmAclosePreservesAsyncCloseResult(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := code + `
import asyncio

class AsyncClose:
    async def close(self):
        return "async-close"

class AsyncNoneClose:
    async def close(self):
        return None

class AsyncDispose:
    async def dispose(self):
        return "async-dispose"

class SyncDispose:
    def dispose(self):
        return "sync-dispose"

class AsyncNoneDispose:
    async def dispose(self):
        return None

class CloseAndAsyncDispose:
    async def close(self):
        return "async-close"

    async def dispose(self):
        raise RuntimeError("dispose should not run when close exists")

class AsyncAclose:
    def __init__(self):
        self.closed = False

    async def aclose(self):
        self.closed = True

class AcloseAndDispose:
    async def aclose(self):
        return "async-aclose"

    async def dispose(self):
        raise RuntimeError("dispose should not run when aclose exists")

class RequiredAcloseAndDispose:
    async def aclose(self, reason):
        raise RuntimeError("required-arg aclose should not run")

    async def dispose(self):
        return "dispose-after-required-aclose"

class BothAsyncClosers:
    async def _omnivm_close(self):
        return "omnivm-async-closed"

    async def aclose(self):
        raise RuntimeError("aclose should not run when _omnivm_close exists")

class DynamicAcloseTrap:
    dynamic_lookup_count = 0

    def __getattr__(self, name):
        if name == "aclose":
            self.dynamic_lookup_count += 1
            return lambda: "dynamic-aclose"
        raise AttributeError(name)

class DynamicDisposeTrap:
    dynamic_lookup_count = 0

    def __getattr__(self, name):
        if name == "dispose":
            self.dynamic_lookup_count += 1
            return lambda: "dynamic-dispose"
        raise AttributeError(name)

async def main():
    if await aproxy_close(AsyncClose()) != "async-close":
        raise RuntimeError("async close result was not preserved")
    if await aproxy_close(AsyncNoneClose()) is not True:
        raise RuntimeError("None async close result should normalize to true")
    if await aproxy_close(AsyncDispose()) != "async-dispose":
        raise RuntimeError("async dispose result was not preserved")
    if await aproxy_close(SyncDispose()) != "sync-dispose":
        raise RuntimeError("sync dispose result was not preserved")
    if await aproxy_close(AsyncNoneDispose()) is not True:
        raise RuntimeError("None async dispose result should normalize to true")
    if await aproxy_close(CloseAndAsyncDispose()) != "async-close":
        raise RuntimeError("close should take priority over async dispose")
    closer = AsyncAclose()
    if await aproxy_close(closer) is not True or closer.closed is not True:
        raise RuntimeError("aclose was not awaited")
    if await aproxy_close(AcloseAndDispose()) != "async-aclose":
        raise RuntimeError("aclose should take priority over dispose")
    if await aproxy_close(RequiredAcloseAndDispose()) != "dispose-after-required-aclose":
        raise RuntimeError("required-arg aclose should be skipped for dispose")
    if await omnivm_aclose(BothAsyncClosers()) != "omnivm-async-closed":
        raise RuntimeError("omnivm_aclose alias did not prefer _omnivm_close")
    trap = DynamicAcloseTrap()
    if await aproxy_close(trap) is not False:
        raise RuntimeError("dynamic aclose lookup should not be used")
    if trap.dynamic_lookup_count != 0:
        raise RuntimeError("dynamic aclose lookup was invoked")
    dispose_trap = DynamicDisposeTrap()
    if await aproxy_close(dispose_trap) is not False:
        raise RuntimeError("dynamic dispose lookup should not be used")
    if dispose_trap.dynamic_lookup_count != 0:
        raise RuntimeError("dynamic dispose lookup was invoked")

asyncio.run(main())
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python generated omnivm_aclose return preservation check failed: %v\n%s", err, out)
	}
}

func TestPythonRemoteStreamCancelsOnChunkMaterializationError(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "stream_next":
            return json.dumps({"__omnivm_result__": True, "value": {"done": False, "value": {"bad": True}}})
        if req["op"] == "stream_cancel":
            raise RuntimeError("cancel failed")
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
stream = __OmniVMStreamProxy({"__omnivm_stream__": True, "id": 89, "runtime": "python", "kind": "stream"})
def bad_materializer(_value):
    raise RuntimeError("chunk boom")
globals()["__omnivm_materialize_capture"] = bad_materializer
try:
    next(stream)
except RuntimeError as exc:
    if "chunk boom" not in str(exc):
        raise
    cleanup = cleanup_errors(exc)
    if len(cleanup) != 1 or str(cleanup[0]) != "cancel failed":
        raise RuntimeError("cleanup error missing: " + repr(cleanup))
else:
    raise RuntimeError("chunk materialization error was not raised")
if not stream._closed:
    raise RuntimeError("stream was not marked closed")
if stream.close() is not False:
    raise RuntimeError("close was not idempotent")
cancels = [req for req in Bridge.requests if req.get("op") == "stream_cancel"]
if len(cancels) != 1 or cancels[0].get("id") != 89:
    raise RuntimeError("stream cancel requests mismatch: " + repr(cancels))
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python remote stream materialization-error cancellation check failed: %v\n%s", err, out)
	}
}

func TestPythonRemoteStreamContextPreservesBodyExceptionWhenCloseFails(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	code := injectPythonCaptures(nil)
	script := `
import json
class Bridge:
    requests = []
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "stream_cancel":
            raise RuntimeError("cancel failed")
        raise RuntimeError("unexpected manifest op " + req["op"])
` + code + `
omnivm = Bridge
stream = __omnivm_materialize_capture({"__omnivm_stream__": True, "id": 90, "runtime": "python", "kind": "stream"})
try:
    with stream:
        raise ValueError("body failed")
except ValueError as exc:
    if "body failed" not in str(exc):
        raise
    notes = getattr(exc, "__notes__", [])
    if not any("cancel failed" in note for note in notes):
        raise RuntimeError("close failure note missing: " + repr(notes))
    cleanup = cleanup_errors(exc)
    if len(cleanup) != 1 or str(cleanup[0]) != "cancel failed":
        raise RuntimeError("cleanup error missing: " + repr(cleanup))
    cleanup.clear()
    if str(cleanup_errors(exc)[0]) != "cancel failed":
        raise RuntimeError("cleanup_errors returned internal storage")
else:
    raise RuntimeError("body exception was not preserved")
cancels = [req for req in Bridge.requests if req.get("op") == "stream_cancel"]
if len(cancels) != 1 or cancels[0].get("id") != 90:
    raise RuntimeError("stream cancel requests mismatch: " + repr(cancels))
`
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python remote stream context close-failure check failed: %v\n%s", err, out)
	}
}

func TestWrapJavaScriptCaptures(t *testing.T) {
	code := wrapJavaScriptCaptures("console.log(x)", map[string]string{"x": "42"})
	if !contains(code, "(function(") {
		t.Error("should wrap in IIFE")
	}
	if !contains(code, "__omnivm_materialize_capture") {
		t.Error("should materialize captures")
	}
	if !contains(code, "console.log(x)") {
		t.Error("should include user code")
	}
}

func TestWrapJavaScriptCapturesUsesSafeBindingNames(t *testing.T) {
	code := wrapJavaScriptCaptures(`console.log(globalThis["bad-name"])`, map[string]string{"bad-name": "42", "class": "7"})
	if !contains(code, `globalThis["bad-name"] = __omnivm_captures`) || !contains(code, `globalThis["class"] = __omnivm_captures`) {
		t.Fatalf("JS wrapper should assign unsafe names through globalThis properties, got %q", code)
	}
	if contains(code, "function(bad-name") || contains(code, "const class =") {
		t.Fatalf("JS wrapper should not emit unsafe parameter/local names, got %q", code)
	}
}

func TestInjectJSCapturesMaterializesChannelCapture(t *testing.T) {
	channelJSON := streamCaptureJSON(7, "go", "channel")
	code := injectJSCaptures(map[string]string{"outbox": channelJSON})
	if !contains(code, "__omnivm_stream__") {
		t.Error("should mark stream captures")
	}
	if !contains(code, `globalThis["outbox"] = globalThis.__omnivm_materialize_capture(`) {
		t.Error("should assign materialized channel capture")
	}
	if !contains(code, "[Symbol.iterator]") || !contains(code, `op: "stream_next"`) {
		t.Error("should expose captured channels as lazy JS iterables")
	}
	if !contains(code, `op: "stream_cancel"`) {
		t.Error("should support explicit stream cancellation")
	}
	if !contains(code, "var cancelRemote = function()") ||
		!contains(code, "var markRemoteClosed = function(notifyAdapters)") ||
		!contains(code, "var closeListeners = []") ||
		!contains(code, "var addCloseListener = function(listener)") ||
		!contains(code, "var listeners = closeListeners.slice()") ||
		!contains(code, "var listenerResult = listeners[i]();") ||
		!contains(code, "if (listenerResult && typeof listenerResult.then === 'function') listenerResult.catch(function() {});") ||
		!contains(code, "if (notifyAdapters === true)") ||
		!contains(code, "var localValues = Array.isArray(value && value.values) ? value.values.slice() : null;") ||
		!contains(code, "if (localValues) return markRemoteClosed(true);") ||
		!contains(code, "if (remoteClosed) return {done: true};") ||
		!contains(code, "if (localIndex >= localValues.length) {\n        markRemoteClosed(false);\n        return {done: true};\n      }") ||
		!contains(code, "return {done: false, value: globalThis.__omnivm_materialize_capture(localValues[localIndex++])};") ||
		!contains(code, "catch (_localMaterializeErr) {\n        markRemoteClosed(true);\n        throw _localMaterializeErr;\n      }") ||
		!contains(code, "if (typeof omnivm === 'undefined' || !omnivm || typeof omnivm.call !== 'function') {\n        closeRemote();\n        return {done: true};\n      }") ||
		!contains(code, "var released = !!(env && env.__omnivm_result__ === true && env.value === true)") ||
		!contains(code, "if (released === true) markRemoteClosed(true);\n    return released;") ||
		!contains(code, "var recordCleanupError = function(error, cleanupError)") ||
		!contains(code, "error.omnivmCleanupErrors = (error.omnivmCleanupErrors || []).concat([cleanupError]);") ||
		!contains(code, "var cancelRemoteQuiet = function(error)") ||
		!contains(code, "if (cancelRemote() !== true) markRemoteClosed(false);") ||
		!contains(code, "recordCleanupError(error, _cancelErr);") ||
		!contains(code, "catch (_e) {\n      cancelRemoteQuiet(_e);\n      throw _e;\n    }") ||
		!contains(code, "closeRemote();\n    return {done: true};") ||
		!contains(code, "if (closed) return;") ||
		!contains(code, "var unregisterCloseListener = null") ||
		!contains(code, "var detachCloseListener = function()") ||
		!contains(code, "var actualMethod = function(value, name)") ||
		!contains(code, "Object.getOwnPropertyDescriptor(cursor, name)") ||
		!contains(code, `var iteratorReturn = actualMethod(iterator, "return")`) ||
		!contains(code, "return Promise.resolve(iteratorReturn(reason)).then(function() {});") ||
		!contains(code, `var sourceCancel = actualMethod(source, "cancel")`) ||
		!contains(code, "return Promise.resolve(sourceCancel(reason)).then(function() {});") ||
		!contains(code, "closed = true;\n            detachCloseListener();\n            target.push(null);") ||
		!contains(code, "unregisterCloseListener = addCloseListener(function()") ||
		!contains(code, "return closeIterator(\"source closed\");") ||
		!contains(code, "return {done: true, value: owner.cancel(reason)}") ||
		!contains(code, "return {done: true, value: released};") ||
		!contains(code, "__omnivm_close: function() {\n      return cancelRemote();\n    }") ||
		!contains(code, "stream[Symbol.dispose] = function() {\n      return cancelRemote();\n    };") ||
		!contains(code, "stream[Symbol.asyncDispose] = function() {\n      return cancelRemote();\n    };") {
		t.Fatalf("JS stream proxy close/error handling should return the manifest release result and mark remote streams closed through explicit paths, got %q", code)
	}
}

func TestInjectJSCapturesUsesSafeBindingNames(t *testing.T) {
	code := injectJSCaptures(map[string]string{"bad-name": "42"})
	if !contains(code, `globalThis["bad-name"] = globalThis.__omnivm_materialize_capture(42);`) {
		t.Fatalf("JS capture injection should assign unsafe names through globalThis properties, got %q", code)
	}
}

func TestJSCaptureMaterializerHandlesTableProxy(t *testing.T) {
	code := injectJSCaptures(map[string]string{
		"orders": `{"__omnivm_table__":true,"id":7,"runtime":"python","format":"arrow_c_data","ownership":"borrowed","metadata":{"dtype":4,"shape":[10,3],"read_only":true},"released":false}`,
	})
	if !contains(code, "__omnivm_table__ === true") {
		t.Fatalf("JS materializer should recognize table proxies, got %q", code)
	}
	if !contains(code, "format: value.format") || !contains(code, "ownership: value.ownership") {
		t.Fatalf("JS materializer should preserve table metadata, got %q", code)
	}
	if !contains(code, "metadata: value.metadata || null") {
		t.Fatalf("JS materializer should preserve Arrow metadata, got %q", code)
	}
	if !contains(code, "__omnivm_make_handle_proxy") || !contains(code, `op: "handle_access"`) {
		t.Fatalf("JS materializer should wrap table descriptors with handle telemetry, got %q", code)
	}
	if !contains(code, "var isInternalDescriptorProp = function(prop)") ||
		!contains(code, `prop === "format" || prop === "ownership" || prop === "metadata" || prop === "buffer" || prop === "released"`) ||
		!contains(code, `prop === "done" || prop === "cancelled" || prop === "cancelReason" || prop === "payload" || prop === "result"`) ||
		!contains(code, "&& !isInternalDescriptorProp(prop)") ||
		!contains(code, "if (local && !isInternalDescriptorProp(prop)) return local;") {
		t.Fatalf("JS materializer should keep descriptor schema fields remote-first on table/job proxies, got %q", code)
	}
	if !contains(code, "chatty cross-runtime proxy access detected") {
		t.Fatalf("JS materializer should warn on chatty proxy access, got %q", code)
	}
	if !contains(code, "__omnivm_chatty_proxy_warned_limit") || !contains(code, "__omnivm_chatty_proxy_warned_order.shift()") {
		t.Fatalf("JS materializer should bound chatty warning dedupe entries, got %q", code)
	}
	if !contains(code, "__omnivm_materialize_chatty_proxy") || !contains(code, `materialize: true`) {
		t.Fatalf("JS materializer should automatically batch-materialize chatty proxy items, got %q", code)
	}
	if !contains(code, `op: "handle_get"`) {
		t.Fatalf("JS materializer should fetch handle properties, got %q", code)
	}
	if !contains(code, "globalThis.__omnivm_is_missing_bridge_error") || !contains(code, "has no property") || !contains(code, "throw _e") {
		t.Fatalf("JS materializer should distinguish missing remote fields from lifecycle bridge failures, got %q", code)
	}
	if !contains(code, `if (bridge({op: "handle_contains", value: "length"})) return bridge({op: "handle_get", key: "length"});`) {
		t.Fatalf("JS materializer should prefer remote length fields before collection length on non-indexed proxies, got %q", code)
	}
	if !contains(code, `if (bridge({op: "handle_contains", value: "name"})) return bridge({op: "handle_get", key: "name"});`) {
		t.Fatalf("JS materializer should prefer remote name fields before Function.name on function-backed proxies, got %q", code)
	}
	if !contains(code, `if (textKey === 'length' && isIndexedDescriptor())`) || !contains(code, `Number.isInteger(lengthValue)`) || !contains(code, `source runtime rejected length write`) || !contains(code, `runtime=`) {
		t.Fatalf("JS materializer should diagnose unsupported length writes on indexed proxies, got %q", code)
	}
	if !contains(code, `if (isIndexedDescriptor() && /^(0|[1-9][0-9]*)$/.test(prop))`) || !contains(code, `return bridge({op: "handle_index", value: Number(prop)});`) {
		t.Fatalf("JS materializer should route numeric properties on indexed proxies through handle_index before handle_get, got %q", code)
	}
	if !contains(code, "omnivm.proxyGet") || !contains(code, "__omnivm_get") || !contains(code, "omnivm.proxySet") || !contains(code, "__omnivm_set") || !contains(code, "omnivm.proxyCall") || !contains(code, "__omnivm_call") || !contains(code, "omnivm.proxyLen") || !contains(code, "__omnivm_len") || !contains(code, "omnivm.proxyIter") || !contains(code, "__omnivm_iter") || !contains(code, "omnivm.proxyKeys") || !contains(code, "omnivm.proxyValues") || !contains(code, "omnivm.proxyItems") || !contains(code, "omnivm.proxyContains") || !contains(code, "__omnivm_contains") || !contains(code, "omnivm.proxyClose") || !contains(code, "omnivm.omnivmClose") || !contains(code, "__omnivm_close") || !contains(code, "omnivm.bufferOwner") || !contains(code, "omnivm.proxyLength") || !contains(code, `Symbol.for("omnivm.proxy.length")`) {
		t.Fatalf("JS materializer should expose proxy-safe get/set/call/len/iter/contains/close helpers and length symbol for collision cases, got %q", code)
	}
	for _, want := range []string{
		"omnivm.ownerDispatchStatus",
		"omnivm.ownerDispatchTargetStatus",
		"omnivm.assertOwnerDispatchSupported",
		"omnivm.assertOwnerDispatchTargetSupported",
		"omnivm.rubyThreadingStatus",
		"omnivm.assertRubyNativeThreadsSupported",
		"globalThis.__omnivm_owner_dispatch_contract",
		"globalThis.__omnivm_ruby_threading_contract",
		`owner_dispatch_supported: false`,
		`native_threads_supported: false`,
		`javascript_event_loop`,
		`python_async_loop: "python_asyncio"`,
		`nodejs: "javascript_event_loop"`,
		`event_loop: "javascript_event_loop"`,
		`fiber: "ruby_fiber_thread"`,
		`thread: "ruby_fiber_thread"`,
		`python_async_stream_pull`,
		`known_targets: Object.keys(status.owner_dispatch_targets || {}).sort()`,
		`detailsSnapshot = globalThis.__omnivm_clone_json(details)`,
		`detailsJson = JSON.stringify(detailsSnapshot)`,
		`var originRuntime = "javascript"`,
		`var boundaryValue = boundaryPath`,
		`var originalErrorHandle = null`,
		`Object.defineProperty(err, "origin_runtime"`,
		`Object.defineProperty(err, "originRuntime"`,
		`Object.defineProperty(err, "boundary_path"`,
		`Object.defineProperty(err, "boundaryPath"`,
		`Object.defineProperty(err, "original_error_handle"`,
		`Object.defineProperty(err, "originalErrorHandle"`,
		`Object.defineProperty(err, "stack_frames"`,
		`get: function() { return stackFrames.slice(); }`,
		`Object.defineProperty(err, "cause_chain"`,
		`get: function() { return globalThis.__omnivm_clone_json(causeChain); }`,
		`Object.defineProperty(err, "details"`,
		`get: function() { return globalThis.__omnivm_clone_json(detailsSnapshot); }`,
		`if (typeof value === 'string')`,
		`detailsSnapshot = globalThis.__omnivm_clone_json(JSON.parse(value));`,
		`err.details_json = value;`,
		`origin_runtime: originRuntime`,
		`boundary_path: boundaryValue`,
		`original_error_handle: originalErrorHandle`,
		`details: globalThis.__omnivm_clone_json(detailsSnapshot)`,
	} {
		if !contains(code, want) {
			t.Fatalf("JS materializer should expose owner-dispatch diagnostic guards, missing %q", want)
		}
	}
	if !contains(code, "globalThis.__omnivm_actual_public_method") ||
		!contains(code, "Object.getOwnPropertyDescriptor(cursor, name)") ||
		!contains(code, "globalThis.__omnivm_lifecycle_method_without_required_args") ||
		!contains(code, "method.length === 0") ||
		!contains(code, `omnivmClose = globalThis.__omnivm_lifecycle_method_without_required_args(value, "__omnivm_close")`) ||
		!contains(code, "return omnivmClose.call(value)") ||
		!contains(code, "symbolDispose = Symbol.dispose ? globalThis.__omnivm_lifecycle_method_without_required_args(value, Symbol.dispose) : null") ||
		!contains(code, "symbolAsyncDispose = Symbol.asyncDispose ? globalThis.__omnivm_lifecycle_method_without_required_args(value, Symbol.asyncDispose) : null") ||
		!contains(code, "return symbolDisposeResult === undefined ? true : symbolDisposeResult") ||
		!contains(code, "return symbolAsyncDisposeResult === undefined ? true : symbolAsyncDisposeResult") ||
		!contains(code, `var close = globalThis.__omnivm_lifecycle_method_without_required_args(value, "close")`) ||
		!contains(code, "var result = close.call(value);\n          return result === undefined ? true : result") ||
		!contains(code, `var dispose = globalThis.__omnivm_lifecycle_method_without_required_args(value, "dispose")`) ||
		!contains(code, "var disposeResult = dispose.call(value);\n          return disposeResult === undefined ? true : disposeResult") ||
		!contains(code, `var destroy = globalThis.__omnivm_lifecycle_method_without_required_args(value, "destroy")`) ||
		!contains(code, "var destroyResult = destroy.call(value);\n          return destroyResult === undefined ? true : destroyResult") {
		t.Fatalf("JS proxyClose should use descriptor-based close lookup for collision cases, got %q", code)
	}
	if !contains(code, `Object.defineProperty(omnivm, "omnivmClose"`) ||
		!contains(code, `value: function(value) { return omnivm.proxyClose(value); }`) {
		t.Fatalf("JS materializer should expose omnivmClose as an alias for proxyClose, got %q", code)
	}
	if contains(code, "typeof value.close === 'function'") || contains(code, "value.close();") || contains(code, "typeof value.dispose === 'function'") || contains(code, "value.dispose();") || contains(code, "typeof value.destroy === 'function'") || contains(code, "value.destroy();") || contains(code, "typeof value.__omnivm_close === 'function'") || contains(code, "value && value.__omnivm_close") || contains(code, "value[Symbol.dispose]") || contains(code, "value[Symbol.asyncDispose]") {
		t.Fatalf("JS proxyClose should not invoke dynamic close property lookup")
	}
	if !contains(code, `prop === "__omnivm_contains" || prop === "__omnivm_close" || prop === "toJSON"`) {
		t.Fatalf("JS proxy bookkeeping should protect the explicit close helper from remote fallback operations, got %q", code)
	}
	if !contains(code, "Symbol.dispose && prop === Symbol.dispose") ||
		!contains(code, "Symbol.asyncDispose && prop === Symbol.asyncDispose") {
		t.Fatalf("JS materializer should expose disposal symbols as collision-free lifecycle close hooks, got %q", code)
	}
	if !contains(code, `if (prop === "__omnivm_close") return {value: releaseProxyLease`) ||
		!contains(code, `Symbol.dispose && prop === Symbol.dispose) return {value: releaseProxyLease`) ||
		!contains(code, `Symbol.asyncDispose && prop === Symbol.asyncDispose) return {value: releaseProxyLease`) {
		t.Fatalf("JS materializer should expose lifecycle close hooks through descriptors for collision-safe proxyClose lookup, got %q", code)
	}
	for _, want := range []string{
		"globalThis.__omnivm_BufferOwner",
		"Object.defineProperty(omnivm, \"bufferOwner\"",
		"omnivm.setBuffer(this.name, this.__omnivm_data, this.__omnivm_dtype)",
		"omnivm.releaseBuffer(this.name)",
		"__omnivm_buffer_owner_error",
		`"native_memory"`,
		`active_owner: true`,
		"cannot be re-entered after release",
		"is already active",
		"if (this.released === true) return false",
		"this.__omnivm_entered = false",
		"var finishSuccess = function(value)",
		"var result = callback(owner)",
		"typeof result.then === 'function'",
		"return Promise.resolve(result).then(finishSuccess, finishError)",
		"bodyError.omnivmCleanupErrors",
		"Object.defineProperty(omnivm, \"cleanupErrors\"",
		"return Array.isArray(errors) ? errors.slice() : []",
		"globalThis.__omnivm_BufferOwner.prototype.status = function()",
		"return omnivm.bufferStatus(this.name)",
		"return finishSuccess(result)",
	} {
		if !contains(code, want) {
			t.Fatalf("JS bufferOwner helper contract missing %q", want)
		}
	}
	if !contains(code, `return value.__omnivm_get(key, defaultValue, true);`) ||
		!contains(code, `return function(key, defaultValue, remoteFirst) { return bridgeGet(key, defaultValue, remoteFirst === true); };`) ||
		!contains(code, `if (remoteFirst === true)`) {
		t.Fatalf("JS proxyGet should force remote-first lookup for descriptor/identity-name collisions, got %q", code)
	}
	if !contains(code, `if (typeof prop === 'string' && !isProxyBookkeepingProp(prop)`) ||
		!contains(code, `if (bridge({op: "handle_contains", value: prop})) return bridge({op: "handle_get", key: prop});`) ||
		!contains(code, `return Reflect.get(obj, prop, receiver);`) {
		t.Fatalf("JS materializer should prefer remote fields before inherited identity properties such as constructor/toString/valueOf, got %q", code)
	}
	if !contains(code, `Symbol.toPrimitive && prop === Symbol.toPrimitive`) ||
		!contains(code, `var keys = hint === 'number' ? ['valueOf', 'toString'] : ['toString', 'valueOf'];`) ||
		!contains(code, `if (value === missing) continue;`) ||
		!contains(code, `return primitiveDescription();`) {
		t.Fatalf("JS materializer should keep primitive coercion collision-safe for remote toString/valueOf fields, got %q", code)
	}
	if !contains(code, `prop === globalThis.__omnivm_proxy_length_symbol`) {
		t.Fatalf("JS materializer should expose collection length through a collision-free symbol, got %q", code)
	}
	if !contains(code, `if (prop === 'then') return undefined;`) {
		t.Fatalf("JS materializer should prevent remote then fields from becoming JS thenables or triggering bridge access, got %q", code)
	}
	if !contains(code, `env.value.zeroArg === true`) || !contains(code, `return bridge({op: "handle_call", key: env.value.key, args: []});`) {
		t.Fatalf("JS materializer should invoke zero-arg callable descriptors as property access, got %q", code)
	}
	if !contains(code, `preserveCallable`) || !contains(code, `return value.__omnivm_get(key, defaultValue, true);`) {
		t.Fatalf("JS proxyGet should preserve callable then descriptors through the explicit escape hatch, got %q", code)
	}
	if !contains(code, `op: "handle_index"`) || !contains(code, `op: "handle_set"`) || !contains(code, `op: "handle_call"`) || !contains(code, `op: "handle_len"`) || !contains(code, `op: "handle_iter"`) || !contains(code, `op: "handle_contains"`) {
		t.Fatalf("JS materializer should forward generic index/set/call/len/iter/contains operations, got %q", code)
	}
	if !contains(code, "globalThis.__omnivm_encode_arg") || !contains(code, "__omnivm_runtime_ref__") || !contains(code, ".map(globalThis.__omnivm_encode_arg)") {
		t.Fatalf("JS proxy calls should preserve complex args as runtime refs, got %q", code)
	}
	if !contains(code, "getOwnPropertyDescriptor") || !contains(code, `mode: "keys"`) {
		t.Fatalf("JS materializer should enumerate remote proxy keys generically, got %q", code)
	}
	if !contains(code, "FinalizationRegistry") || !contains(code, `op: "handle_release_finalizer"`) {
		t.Fatalf("JS materializer should queue finalizer releases, got %q", code)
	}
	if !contains(code, "__omnivm_release_handle_explicit") ||
		!contains(code, `op: "handle_release_explicit"`) ||
		!contains(code, "globalThis.__omnivm_release_handle_explicit(handleId)") ||
		!contains(code, "if (released === true)") ||
		!contains(code, "globalThis.__omnivm_handle_finalizers.unregister(target)") ||
		!contains(code, "globalThis.__omnivm_handle_finalizers.register(proxy, finalizerHandleId, target)") ||
		!contains(code, "globalThis.__omnivm_handle_finalizers.register(stream, value.id, stream)") {
		t.Fatalf("JS explicit proxy close should use a non-quiet release path and unregister finalizers, got %q", code)
	}
	if !contains(code, `op: "handle_retain"`) || !contains(code, "__omnivm_retain_handle") {
		t.Fatalf("JS materializer should retain handles for guest proxy lifetime, got %q", code)
	}
	if !contains(code, `op: "handle_adopt"`) || !contains(code, "__omnivm_adopt_handle") || !contains(code, "descriptor.transfer === true") {
		t.Fatalf("JS materializer should adopt returned transfer handles, got %q", code)
	}
	if !contains(code, "globalThis.__omnivm_proxy_cache") || !contains(code, "WeakRef") ||
		!contains(code, `__omnivm_cached_proxy("resource", value.id`) ||
		!contains(code, `__omnivm_cached_proxy("table", value.id`) ||
		!contains(code, `__omnivm_cached_proxy("job", value.id`) {
		t.Fatalf("JS materializer should weakly cache descriptor proxies by namespaced identity, got %q", code)
	}
	if !contains(code, "cached.__omnivm_closed__ === true") {
		t.Fatalf("JS materializer should not reuse closed cached proxies, got %q", code)
	}
	if !contains(code, "__omnivm_prune_proxy_cache") || !contains(code, "cache.size <= 4096") {
		t.Fatalf("JS materializer should bound stale weak proxy cache entries, got %q", code)
	}
}

func TestJSCaptureOwnerDispatchGuardsReportDiagnosticOnly(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {};
` + code + `
var status = omnivm.ownerDispatchStatus();
if (status.mode !== "diagnostic_only") throw new Error("bad mode: " + status.mode);
if (status.owner_dispatch_supported !== false) throw new Error("owner dispatch should be unsupported");
if (!status.owner_dispatch_targets.javascript_event_loop) throw new Error("missing JS target");
status.owner_dispatch_targets.javascript_event_loop.supported = true;
if (omnivm.ownerDispatchStatus().owner_dispatch_targets.javascript_event_loop.supported !== false) {
  throw new Error("ownerDispatchStatus leaked mutable state");
}
var rubyStatus = omnivm.rubyThreadingStatus();
if (rubyStatus.mode !== "single_vm_thread") throw new Error("bad ruby threading mode: " + rubyStatus.mode);
if (rubyStatus.native_threads_supported !== false) throw new Error("ruby native threads should be unsupported");
rubyStatus.native_threads_supported = true;
if (omnivm.rubyThreadingStatus().native_threads_supported !== false) {
  throw new Error("rubyThreadingStatus leaked mutable state");
}
var target = omnivm.ownerDispatchTargetStatus("js");
if (target.target !== "javascript_event_loop") throw new Error("alias did not normalize: " + target.target);
if (target.requested_target !== "js") throw new Error("requested target missing");
if (omnivm.ownerDispatchTargetStatus("python async loop").target !== "python_asyncio") throw new Error("python alias did not normalize");
if (omnivm.ownerDispatchTargetStatus("nodejs").target !== "javascript_event_loop") throw new Error("nodejs alias did not normalize");
if (omnivm.ownerDispatchTargetStatus("thread").target !== "ruby_fiber_thread") throw new Error("thread alias did not normalize");
target.supported = true;
if (omnivm.ownerDispatchTargetStatus("js").supported !== false) throw new Error("target status leaked mutable state");
try {
  omnivm.ownerDispatchTargetStatus("unknown-loop");
  throw new Error("missing unknown target diagnostic");
} catch (err) {
  if (err.name !== "OmniVMRuntimeError") throw new Error("unknown target used generic error: " + err.name);
  if (err.boundary_path !== "owner_dispatch_target") throw new Error("bad unknown target boundary: " + err.boundary_path);
  if (err.details.target !== "unknown_loop") throw new Error("missing top-level unknown target");
  if (err.details.requested_target !== "unknown-loop") throw new Error("missing top-level requested unknown target");
  if (!err.details || err.details.owner_dispatch_target.target !== "unknown_loop") throw new Error("missing unknown target details");
  if (err.details.owner_dispatch_target.requested_target !== "unknown-loop") throw new Error("missing requested unknown target");
  if (err.details.owner_dispatch_target.known_targets.indexOf("javascript_event_loop") < 0) throw new Error("missing known targets");
  if (JSON.stringify(err).indexOf('"boundary_path":"owner_dispatch_target"') < 0) throw new Error("unknown target envelope missing boundary");
}
try {
  omnivm.assertOwnerDispatchSupported("express startup");
  throw new Error("missing universal dispatch diagnostic");
} catch (err) {
  if (err.message.indexOf("express startup: owner dispatch unsupported") < 0) throw new Error("bad universal message: " + err.message);
  if (err.boundary_path !== "owner_dispatch") throw new Error("bad universal boundary: " + err.boundary_path);
  if (!err.details || err.details.owner_dispatch.owner_dispatch_supported !== false) throw new Error("missing universal details");
  if (err.originRuntime !== "javascript") throw new Error("missing error originRuntime alias");
  if (!err.traceback || err.traceback.indexOf("OmniVMRuntimeError") < 0) throw new Error("missing error traceback property");
  if (!Array.isArray(err.stack_frames) || err.stack_frames.length === 0) throw new Error("missing error stack_frames property");
  if (!Array.isArray(err.stackFrames) || err.stackFrames.length !== err.stack_frames.length) throw new Error("missing error stackFrames alias");
  if (!Array.isArray(err.cause_chain) || err.cause_chain.length !== 0) throw new Error("missing error cause_chain property");
  if (!Array.isArray(err.causeChain) || err.causeChain.length !== 0) throw new Error("missing error causeChain alias");
  if (typeof err.toJSON !== "function") throw new Error("missing owner dispatch error toJSON");
  var envelope = err.toJSON();
  if (envelope.runtime !== "javascript") throw new Error("bad envelope runtime: " + envelope.runtime);
  if (envelope.origin_runtime !== "javascript") throw new Error("bad envelope origin runtime: " + envelope.origin_runtime);
  if (envelope.type !== "RuntimeError") throw new Error("bad envelope type: " + envelope.type);
  if (envelope.message !== err.message) throw new Error("bad envelope message: " + envelope.message);
  if (envelope.boundary_path !== "owner_dispatch") throw new Error("bad envelope boundary: " + envelope.boundary_path);
  if (envelope.original_error_handle !== null) throw new Error("bad envelope original error handle: " + envelope.original_error_handle);
  if (!envelope.traceback || envelope.traceback.indexOf("OmniVMRuntimeError") < 0) throw new Error("missing envelope traceback");
  if (!Array.isArray(envelope.stack_frames) || envelope.stack_frames.length === 0) throw new Error("missing envelope stack frames");
  if (!Array.isArray(envelope.cause_chain) || envelope.cause_chain.length !== 0) throw new Error("bad envelope cause chain");
  if (!envelope.details || envelope.details.owner_dispatch.owner_dispatch_supported !== false) throw new Error("missing envelope details");
  envelope.stack_frames.length = 0;
  if (err.stack_frames.length === 0) throw new Error("toJSON leaked mutable stack frames");
  envelope.cause_chain.push({message: "mutated"});
  if (err.cause_chain.length !== 0) throw new Error("toJSON leaked mutable cause chain");
  envelope.details.owner_dispatch.mode = "mutated-envelope";
  if (err.details.owner_dispatch.mode !== "diagnostic_only") throw new Error("toJSON leaked mutable details");
  err.stack_frames.length = 0;
  err.stackFrames.length = 0;
  err.cause_chain.push({message: "mutated-error"});
  err.causeChain.push({message: "mutated-error"});
  err.details.owner_dispatch.mode = "mutated-error";
  if (err.stack_frames.length === 0 || err.stackFrames.length === 0) throw new Error("error stack reader leaked mutable state");
  if (err.cause_chain.length !== 0 || err.causeChain.length !== 0) throw new Error("error cause reader leaked mutable state");
  if (err.details.owner_dispatch.mode !== "diagnostic_only") throw new Error("error details reader leaked mutable state");
  var stableEnvelope = err.toJSON();
  if (!Array.isArray(stableEnvelope.stack_frames) || stableEnvelope.stack_frames.length === 0) throw new Error("error stack mutation changed snapshot");
  if (!Array.isArray(stableEnvelope.cause_chain) || stableEnvelope.cause_chain.length !== 0) throw new Error("error cause mutation changed snapshot");
  if (stableEnvelope.details.owner_dispatch.mode !== "diagnostic_only") throw new Error("error details mutation changed snapshot");
  var serialized = JSON.stringify(err);
  if (serialized.indexOf('"boundary_path":"owner_dispatch"') < 0) throw new Error("serialized envelope missing boundary: " + serialized);
  if (serialized.indexOf('"message":"express startup: owner dispatch unsupported') < 0) throw new Error("serialized envelope missing message: " + serialized);
  if (JSON.parse(err.details_json).owner_dispatch.mode !== "diagnostic_only") throw new Error("details_json was not stable");
  err.details_json = '{"owner_dispatch":{"mode":"json-set"}}';
  if (err.details.owner_dispatch.mode !== "json-set") throw new Error("details_json string setter did not update details");
  if (err.toJSON().details.owner_dispatch.mode !== "json-set") throw new Error("details_json string setter did not update envelope details");
  err.detailsJson = {owner_dispatch: {mode: "alias-object-set"}};
  if (JSON.parse(err.details_json).owner_dispatch.mode !== "alias-object-set") throw new Error("detailsJson object setter did not update details_json");
  if (err.details.owner_dispatch.mode !== "alias-object-set") throw new Error("detailsJson object setter did not update details");
  err.originRuntime = "owner-js";
  err.boundaryPath = "owner_dispatch > normalized";
  err.originalErrorHandle = "js-owner-1";
  if (err.origin_runtime !== "owner-js") throw new Error("originRuntime did not update origin_runtime");
  if (err.boundary_path !== "owner_dispatch > normalized") throw new Error("boundaryPath did not update boundary_path");
  if (err.original_error_handle !== "js-owner-1") throw new Error("originalErrorHandle did not update original_error_handle");
  var aliasEnvelope = err.toJSON();
  if (aliasEnvelope.origin_runtime !== "owner-js") throw new Error("originRuntime setter did not update envelope");
  if (aliasEnvelope.boundary_path !== "owner_dispatch > normalized") throw new Error("boundaryPath setter did not update envelope");
  if (aliasEnvelope.original_error_handle !== "js-owner-1") throw new Error("originalErrorHandle setter did not update envelope");
}
try {
  omnivm.assertRubyNativeThreadsSupported("puma startup");
  throw new Error("missing ruby threading diagnostic");
} catch (err) {
  if (err.message.indexOf("puma startup: native Ruby threads unsupported: mode=single_vm_thread") < 0) throw new Error("bad ruby threading message: " + err.message);
  if (err.boundary_path !== "ruby_threading") throw new Error("bad ruby threading boundary: " + err.boundary_path);
  if (!err.details || err.details.ruby_threading.native_threads_supported !== false) throw new Error("missing ruby threading details");
  if (JSON.stringify(err).indexOf('"boundary_path":"ruby_threading"') < 0) throw new Error("ruby threading envelope missing boundary");
  if (JSON.parse(err.details_json).ruby_threading.mode !== "single_vm_thread") throw new Error("ruby threading details_json was not stable");
}
try {
  omnivm.assertOwnerDispatchTargetSupported("ruby", "async bridge");
  throw new Error("missing target dispatch diagnostic");
} catch (err) {
  if (err.message.indexOf("async bridge: owner dispatch target unsupported: ruby_fiber_thread") < 0) throw new Error("bad target message: " + err.message);
  if (err.boundary_path !== "owner_dispatch_target") throw new Error("bad target boundary: " + err.boundary_path);
  if (err.details.target !== "ruby_fiber_thread") throw new Error("missing top-level target");
  if (err.details.requested_target !== "ruby") throw new Error("missing top-level requested target");
  if (!err.details || err.details.owner_dispatch_target.target !== "ruby_fiber_thread") throw new Error("missing target details");
}
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node owner dispatch guard check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureProxyClosePreservesLocalCloseResult(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {};
` + code + `
var falseClosed = 0;
var falseResult = omnivm.proxyClose({close: function() { falseClosed++; return false; }});
if (falseResult !== false || falseClosed !== 1) throw new Error("false close result was not preserved");
var textResult = omnivm.proxyClose({close: function() { return "closed"; }});
if (textResult !== "closed") throw new Error("string close result was not preserved: " + textResult);
var aliasResult = omnivm.omnivmClose({close: function() { return "alias-closed"; }});
if (aliasResult !== "alias-closed") throw new Error("omnivmClose alias did not preserve close result: " + aliasResult);
var receiverTarget = {closed: false, close: function() { this.closed = true; return this === receiverTarget; }};
var receiverResult = omnivm.proxyClose(receiverTarget);
if (receiverResult !== true || receiverTarget.closed !== true) throw new Error("close receiver was not preserved");
var undefinedResult = omnivm.proxyClose({close: function() {}});
if (undefinedResult !== true) throw new Error("undefined close result should normalize to true");
var disposeResult = omnivm.proxyClose({dispose: function() { return "disposed"; }});
if (disposeResult !== "disposed") throw new Error("dispose result was not preserved: " + disposeResult);
var disposeUndefinedResult = omnivm.proxyClose({dispose: function() {}});
if (disposeUndefinedResult !== true) throw new Error("undefined dispose result should normalize to true");
var disposeFalseResult = omnivm.proxyClose({dispose: function() { return false; }});
if (disposeFalseResult !== false) throw new Error("false dispose result was not preserved");
var requiredCloseDisposeCalls = 0;
var requiredCloseDisposeResult = omnivm.proxyClose({
  close: function(reason) { throw new Error("required close should not run: " + reason); },
  dispose: function() { requiredCloseDisposeCalls++; return "disposed-after-required-close"; }
});
if (requiredCloseDisposeResult !== "disposed-after-required-close" || requiredCloseDisposeCalls !== 1) throw new Error("required close did not fall through to dispose");
var requiredOnlyCloseResult = omnivm.proxyClose({close: function(reason) { throw new Error("required close should not run: " + reason); }});
if (requiredOnlyCloseResult !== false) throw new Error("required-only close should not be treated as lifecycle close");
var getterCount = 0;
var getterTarget = {close: function() { return "getter-safe"; }};
Object.defineProperty(getterTarget, "__omnivm_close", {get: function() { getterCount++; throw new Error("getter invoked"); }});
var getterResult = omnivm.proxyClose(getterTarget);
if (getterResult !== "getter-safe" || getterCount !== 0) throw new Error("__omnivm_close getter was invoked");
var disposeGetterCount = 0;
var disposeGetterTarget = {};
Object.defineProperty(disposeGetterTarget, "dispose", {get: function() { disposeGetterCount++; throw new Error("dispose getter invoked"); }});
var disposeGetterResult = omnivm.proxyClose(disposeGetterTarget);
if (disposeGetterResult !== false || disposeGetterCount !== 0) throw new Error("dispose getter was invoked");
var promise = Promise.resolve("async-closed");
var promiseResult = omnivm.proxyClose({close: function() { return promise; }});
if (promiseResult !== promise) throw new Error("promise close result was not preserved");
var disposePromise = Promise.resolve("async-disposed");
var disposePromiseResult = omnivm.proxyClose({dispose: function() { return disposePromise; }});
if (disposePromiseResult !== disposePromise) throw new Error("promise dispose result was not preserved");
var destroyTarget = {destroyed: false, destroy: function() { this.destroyed = true; return this; }};
var destroyResult = omnivm.proxyClose(destroyTarget);
if (destroyResult !== destroyTarget || destroyTarget.destroyed !== true) throw new Error("destroy receiver/result was not preserved");
var destroyUndefinedResult = omnivm.proxyClose({destroy: function() {}});
if (destroyUndefinedResult !== true) throw new Error("undefined destroy result should normalize to true");
var destroyFalseResult = omnivm.proxyClose({destroy: function() { return false; }});
if (destroyFalseResult !== false) throw new Error("false destroy result was not preserved");
var requiredDestroyResult = omnivm.proxyClose({destroy: function(reason) { throw new Error("required destroy should not run: " + reason); }});
if (requiredDestroyResult !== false) throw new Error("required destroy should not be treated as lifecycle close");
var destroyGetterCount = 0;
var destroyGetterTarget = {};
Object.defineProperty(destroyGetterTarget, "destroy", {get: function() { destroyGetterCount++; throw new Error("destroy getter invoked"); }});
var destroyGetterResult = omnivm.proxyClose(destroyGetterTarget);
if (destroyGetterResult !== false || destroyGetterCount !== 0) throw new Error("destroy getter was invoked");
var symbolDisposeResult = omnivm.proxyClose({[Symbol.dispose]: function() {}});
if (symbolDisposeResult !== true) throw new Error("undefined Symbol.dispose result should normalize to true");
var symbolDisposeFalse = omnivm.proxyClose({[Symbol.dispose]: function() { return false; }});
if (symbolDisposeFalse !== false) throw new Error("false Symbol.dispose result was not preserved");
var requiredSymbolDispose = omnivm.proxyClose({[Symbol.dispose]: function(reason) { throw new Error("required Symbol.dispose should not run: " + reason); }});
if (requiredSymbolDispose !== false) throw new Error("required Symbol.dispose should not be treated as lifecycle close");
var symbolAsyncDisposePromise = Promise.resolve("async-symbol-closed");
var symbolAsyncDisposeResult = omnivm.proxyClose({[Symbol.asyncDispose]: function() { return symbolAsyncDisposePromise; }});
if (symbolAsyncDisposeResult !== symbolAsyncDisposePromise) throw new Error("Symbol.asyncDispose promise result was not preserved");
var requiredSymbolAsyncDispose = omnivm.proxyClose({[Symbol.asyncDispose]: function(reason) { throw new Error("required Symbol.asyncDispose should not run: " + reason); }});
if (requiredSymbolAsyncDispose !== false) throw new Error("required Symbol.asyncDispose should not be treated as lifecycle close");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node proxyClose return preservation check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureBufferOwnerScopesAndReleases(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  events: [],
  setBuffer: function(name, data, dtype) {
    this.events.push(["set", name, data, dtype]);
  },
  releaseBuffer: function(name) {
    this.events.push(["release", name]);
  },
  bufferStatus: function(name) {
    this.events.push(["status", name]);
    return {name: name, lease_state: "owned"};
  }
};
` + code + `
var owner = omnivm.bufferOwner("payload", "abc", 7);
if (JSON.stringify(omnivm.events) !== JSON.stringify([["set", "payload", "abc", 7]])) throw new Error("set event mismatch: " + JSON.stringify(omnivm.events));
var ownerStatus = owner.status();
if (ownerStatus.name !== "payload" || ownerStatus.lease_state !== "owned") throw new Error("owner status mismatch: " + JSON.stringify(ownerStatus));
try {
  owner.enter();
  throw new Error("active owner re-enter did not fail");
} catch (err) {
  if (err.message.indexOf("already active") < 0) throw err;
  if (err.boundary_path !== "native_memory") throw new Error("active owner boundary mismatch: " + err.boundary_path);
  if (!err.details || !err.details.buffer || err.details.buffer.active_owner !== true || err.details.buffer.state !== "active") throw new Error("active owner details mismatch: " + JSON.stringify(err.details));
}
if (owner.release() !== true) throw new Error("release did not return true");
if (owner.release() !== false) throw new Error("second release was not idempotent");
if (owner.released !== true) throw new Error("released flag mismatch");
try {
  owner.enter();
  throw new Error("released owner re-enter did not fail");
} catch (err) {
  if (err.message.indexOf("cannot be re-entered after release") < 0) throw err;
  if (err.boundary_path !== "native_memory") throw new Error("released owner boundary mismatch: " + err.boundary_path);
  if (!err.details || !err.details.buffer || err.details.buffer.released !== true || err.details.buffer.state !== "released") throw new Error("released owner details mismatch: " + JSON.stringify(err.details));
}
if (JSON.stringify(omnivm.events) !== JSON.stringify([["set", "payload", "abc", 7], ["status", "payload"], ["release", "payload"]])) throw new Error("re-enter changed owner events: " + JSON.stringify(omnivm.events));

var beforeBlock = omnivm.events.slice();
var blockResult = omnivm.bufferOwner("block", function(scoped) {
  if (scoped.name !== "block") throw new Error("block owner name mismatch");
  return "body-result";
});
if (blockResult !== "body-result") throw new Error("block result mismatch: " + blockResult);
var expectedBlock = beforeBlock.concat([["release", "block"]]);
if (JSON.stringify(omnivm.events) !== JSON.stringify(expectedBlock)) throw new Error("block release mismatch: " + JSON.stringify(omnivm.events));

var thenFieldResult = {then: "data-field"};
var thenFieldOwnerResult = omnivm.bufferOwner("then-field", function() {
  return thenFieldResult;
});
if (thenFieldOwnerResult !== thenFieldResult) throw new Error("then data field was treated as a Promise");

var beforeThenable = omnivm.events.slice();
var thenableSettled = false;
var thenableResult = omnivm.bufferOwner("thenable", function(scoped) {
  return {
    then: function(resolve) {
      setTimeout(function() {
        thenableSettled = true;
        resolve(scoped.name + "-result");
      }, 0);
    }
  };
});
if (!(thenableResult instanceof Promise)) throw new Error("thenable callback did not return a Promise");
if (JSON.stringify(omnivm.events) !== JSON.stringify(beforeThenable)) throw new Error("thenable buffer released before settle: " + JSON.stringify(omnivm.events));
thenableResult.then(function(value) {
  if (value !== "thenable-result") throw new Error("thenable value mismatch: " + value);
  if (thenableSettled !== true) throw new Error("thenable release ran before settlement");
  var expectedThenable = beforeThenable.concat([["release", "thenable"]]);
  if (JSON.stringify(omnivm.events) !== JSON.stringify(expectedThenable)) throw new Error("thenable release mismatch: " + JSON.stringify(omnivm.events));

var beforePromise = omnivm.events.slice();
var promiseResult = omnivm.bufferOwner("async", function(scoped) {
  return Promise.resolve(scoped.name + "-result");
});
if (!(promiseResult instanceof Promise)) throw new Error("Promise callback did not return a Promise");
return promiseResult.then(function(value) {
  if (value !== "async-result") throw new Error("Promise value mismatch: " + value);
  var expectedPromise = beforePromise.concat([["release", "async"]]);
  if (JSON.stringify(omnivm.events) !== JSON.stringify(expectedPromise)) throw new Error("Promise release mismatch: " + JSON.stringify(omnivm.events));

  omnivm.releaseBuffer = function(name) {
    this.events.push(["release-fail", name]);
    throw new Error("release failed");
  };
  try {
    omnivm.bufferOwner("failing", function() {
      throw new Error("body failed");
    });
    throw new Error("body exception was not raised");
  } catch (err) {
    if (err.message !== "body failed") throw new Error("body exception was masked: " + err.message);
    var cleanupErrors = omnivm.cleanupErrors(err);
    if (!cleanupErrors || cleanupErrors[0].message !== "release failed") throw new Error("cleanup error was not retained");
    cleanupErrors.length = 0;
    if (omnivm.cleanupErrors(err)[0].message !== "release failed") throw new Error("cleanupErrors returned internal storage");
  }
  omnivm.releaseBuffer = function(name) {
    this.events.push(["release-tombstone", name]);
    throw globalThis.__omnivm_owner_dispatch_error(
      "release failed for " + name,
      "native_memory",
      {buffer: {name: name, state: "released_detached", released: true, release_error: "producer release failed"}}
    );
  };
  var tombstoned = omnivm.bufferOwner("tombstoned");
  try {
    tombstoned.release();
    throw new Error("tombstone release failure was not raised");
  } catch (err) {
    if (err.boundary_path !== "native_memory") throw new Error("tombstone boundary mismatch: " + err.boundary_path);
    if (!err.details || !err.details.buffer || err.details.buffer.released !== true) throw new Error("tombstone details mismatch: " + JSON.stringify(err.details));
    if (err.details.buffer.release_error !== "producer release failed") throw new Error("tombstone release_error missing: " + JSON.stringify(err.details));
  }
  if (tombstoned.released !== true) throw new Error("tombstoned owner did not mark released");
  if (tombstoned.release() !== false) throw new Error("tombstoned owner second release was not idempotent");
});
}).catch(function(err) {
  console.error(err && err.stack || err);
  process.exit(1);
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node bufferOwner scope check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureProxyIdentityNameCollisionsPreferRemoteFields(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  calls: [],
  fields: {
    constructor: "remote-constructor",
    toString: "remote-toString",
    valueOf: "remote-valueOf",
    inspect: "remote-inspect",
    toJSON: "remote-toJSON"
  },
  call: function(name, raw) {
    if (name !== "__manifest") throw new Error("unexpected bridge name " + name);
    var payload = JSON.parse(raw);
    this.calls.push(payload);
    if (payload.op === "handle_access") {
      return JSON.stringify({__omnivm_result__: true, value: {chatty: false}});
    }
    if (payload.op === "handle_retain") {
      return JSON.stringify({__omnivm_result__: true, value: true});
    }
    if (payload.op === "handle_contains") {
      return JSON.stringify({__omnivm_result__: true, value: Object.prototype.hasOwnProperty.call(this.fields, payload.value)});
    }
    if (payload.op === "handle_get") {
      if (Object.prototype.hasOwnProperty.call(this.fields, payload.key)) {
        return JSON.stringify({__omnivm_result__: true, value: this.fields[payload.key]});
      }
      throw new Error("resource has no property " + payload.key);
    }
    if (payload.op === "handle_index") {
      throw new Error("resource has no index " + payload.value);
    }
    throw new Error("unexpected op " + payload.op);
  }
};
` + code + `
var proxy = globalThis.__omnivm_materialize_capture({__omnivm_resource__: true, id: 77, runtime: "python", kind: "object"});
if (proxy.constructor !== "remote-constructor") throw new Error("constructor was not remote-first: " + String(proxy.constructor));
if (proxy.toString !== "remote-toString") throw new Error("toString was not remote-first: " + String(proxy.toString));
if (proxy.valueOf !== "remote-valueOf") throw new Error("valueOf was not remote-first: " + String(proxy.valueOf));
if (proxy.inspect !== "remote-inspect") throw new Error("inspect was not remote-first: " + String(proxy.inspect));
if (typeof proxy[Symbol.toPrimitive] !== "function") throw new Error("missing Symbol.toPrimitive coercion helper");
if (String(proxy) !== "remote-toString") throw new Error("String(proxy) did not use remote toString field: " + String(proxy));
if (` + "`" + `${proxy}` + "`" + ` !== "remote-toString") throw new Error("template coercion did not use remote toString field");
if (proxy[Symbol.toPrimitive]("number") !== "remote-valueOf") throw new Error("number-hint primitive did not use remote valueOf field");
omnivm.fields = {};
var fallbackProxy = globalThis.__omnivm_materialize_capture({__omnivm_resource__: true, id: 78, runtime: "python", kind: "object"});
var fallbackText = String(fallbackProxy);
if (fallbackText !== "[object OmniVMProxy (runtime=python, kind=object, id=78)]") throw new Error("missing identity fields did not use stable fallback: " + fallbackText);
omnivm.fields = {
  constructor: "remote-constructor",
  toString: "remote-toString",
  valueOf: "remote-valueOf",
  inspect: "remote-inspect",
  toJSON: "remote-toJSON"
};
var localToJSON = proxy.toJSON();
if (!localToJSON || localToJSON.id !== 77 || localToJSON.runtime !== "python") throw new Error("local toJSON bookkeeping changed");
var remoteToJSON = omnivm.proxyGet(proxy, "toJSON");
if (remoteToJSON !== "remote-toJSON") throw new Error("proxyGet did not recover remote toJSON: " + String(remoteToJSON));
var requested = omnivm.calls.filter(function(call) { return call.op === "handle_get"; }).map(function(call) { return call.key; });
["constructor", "toString", "valueOf", "inspect", "toJSON"].forEach(function(key) {
  if (requested.indexOf(key) < 0) throw new Error("missing remote lookup for " + key + ": " + requested.join(","));
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node identity-name collision check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureProxyLifecycleNameCollisionsPreferRemoteFields(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  calls: [],
  fields: {
    close: "remote-close-field",
    dispose: "remote-dispose-field"
  },
  call: function(name, raw) {
    if (name !== "__manifest") throw new Error("unexpected bridge name " + name);
    var payload = JSON.parse(raw);
    this.calls.push(payload);
    if (payload.op === "handle_access") {
      return JSON.stringify({__omnivm_result__: true, value: {chatty: false}});
    }
    if (payload.op === "handle_retain") {
      return JSON.stringify({__omnivm_result__: true, value: true});
    }
    if (payload.op === "handle_contains") {
      return JSON.stringify({__omnivm_result__: true, value: Object.prototype.hasOwnProperty.call(this.fields, payload.value)});
    }
    if (payload.op === "handle_get") {
      if (Object.prototype.hasOwnProperty.call(this.fields, payload.key)) {
        return JSON.stringify({__omnivm_result__: true, value: this.fields[payload.key]});
      }
      throw new Error("resource has no property " + payload.key);
    }
    if (payload.op === "handle_release_explicit") {
      return JSON.stringify({__omnivm_result__: true, value: true});
    }
    if (payload.op === "handle_call" && (payload.key === "close" || payload.key === "dispose")) {
      throw new Error("lifecycle helper called remote " + payload.key + " field");
    }
    throw new Error("unexpected op " + payload.op);
  }
};
` + code + `
var proxy = globalThis.__omnivm_materialize_capture({__omnivm_resource__: true, id: 80, runtime: "python", kind: "object"});
if (proxy.close !== "remote-close-field") throw new Error("close was not remote-first: " + String(proxy.close));
if (proxy.dispose !== "remote-dispose-field") throw new Error("dispose was not remote-first: " + String(proxy.dispose));
if (omnivm.proxyGet(proxy, "close") !== "remote-close-field") throw new Error("proxyGet did not recover remote close");
if (omnivm.proxyGet(proxy, "dispose") !== "remote-dispose-field") throw new Error("proxyGet did not recover remote dispose");
if (omnivm.proxyClose(proxy) !== true) throw new Error("proxyClose did not release the proxy");
if (omnivm.proxyClose(proxy) !== false) throw new Error("proxyClose was not idempotent after release");
var requested = omnivm.calls.filter(function(call) { return call.op === "handle_get"; }).map(function(call) { return call.key; });
["close", "dispose"].forEach(function(key) {
  if (requested.indexOf(key) < 0) throw new Error("missing remote lookup for " + key + ": " + requested.join(","));
});
var releases = omnivm.calls.filter(function(call) { return call.op === "handle_release_explicit"; });
if (releases.length !== 1 || releases[0].id !== 80) throw new Error("explicit release mismatch: " + JSON.stringify(releases));
if (omnivm.calls.some(function(call) { return call.op === "handle_call" && (call.key === "close" || call.key === "dispose"); })) {
  throw new Error("lifecycle close dispatched to a remote lifecycle-named field");
}
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node lifecycle-name collision check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureProxyCacheSkipsClosedProxy(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  calls: [],
  call: function(name, raw) {
    if (name !== "__manifest") throw new Error("unexpected bridge name " + name);
    var payload = JSON.parse(raw);
    this.calls.push(payload);
    if (payload.op === "handle_adopt") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "handle_release_explicit") return JSON.stringify({__omnivm_result__: true, value: true});
    throw new Error("unexpected op " + payload.op);
  }
};
` + code + `
var descriptor = {__omnivm_resource__: true, id: 91, runtime: "javascript", kind: "object", transfer: true};
var first = globalThis.__omnivm_materialize_capture(descriptor);
if (omnivm.proxyClose(first) !== true) throw new Error("first close failed");
var second = globalThis.__omnivm_materialize_capture(descriptor);
if (second === first) throw new Error("closed proxy was reused");
if (omnivm.proxyClose(second) !== true) throw new Error("second close failed");
var adopts = omnivm.calls.filter(function(call) { return call.op === "handle_adopt"; });
var releases = omnivm.calls.filter(function(call) { return call.op === "handle_release_explicit"; });
if (JSON.stringify(adopts) !== JSON.stringify([{op: "handle_adopt", id: 91}, {op: "handle_adopt", id: 91}])) {
  throw new Error("adopt requests mismatch: " + JSON.stringify(adopts));
}
if (JSON.stringify(releases) !== JSON.stringify([{op: "handle_release_explicit", id: 91}, {op: "handle_release_explicit", id: 91}])) {
  throw new Error("release requests mismatch: " + JSON.stringify(releases));
}
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node closed proxy cache check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureTableDescriptorFieldsPreferRemoteFields(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  calls: [],
  fields: {
    id: "remote-id",
    runtime: "remote-runtime",
    format: "remote-format",
    metadata: {remote: true},
    buffer: "remote-buffer"
  },
  call: function(name, raw) {
    if (name !== "__manifest") throw new Error("unexpected bridge name " + name);
    var payload = JSON.parse(raw);
    this.calls.push(payload);
    if (payload.op === "handle_access") {
      return JSON.stringify({__omnivm_result__: true, value: {chatty: false}});
    }
    if (payload.op === "handle_retain") {
      return JSON.stringify({__omnivm_result__: true, value: true});
    }
    if (payload.op === "handle_contains") {
      return JSON.stringify({__omnivm_result__: true, value: Object.prototype.hasOwnProperty.call(this.fields, payload.value)});
    }
    if (payload.op === "handle_get") {
      if (Object.prototype.hasOwnProperty.call(this.fields, payload.key)) {
        return JSON.stringify({__omnivm_result__: true, value: this.fields[payload.key]});
      }
      throw new Error("table has no property " + payload.key);
    }
    throw new Error("unexpected op " + payload.op);
  }
};
` + code + `
var proxy = globalThis.__omnivm_materialize_capture({
  __omnivm_table__: true,
  id: 81,
  runtime: "python",
  format: "arrow_c_data",
  ownership: "borrowed",
  metadata: {dtype: 4},
  buffer: "descriptor-buffer",
  released: false
});
if (proxy.id !== "remote-id") throw new Error("id was not remote-first: " + String(proxy.id));
if (proxy.runtime !== "remote-runtime") throw new Error("runtime was not remote-first: " + String(proxy.runtime));
if (proxy.format !== "remote-format") throw new Error("format was not remote-first: " + String(proxy.format));
if (!proxy.metadata || proxy.metadata.remote !== true) throw new Error("metadata was not remote-first: " + JSON.stringify(proxy.metadata));
if (proxy.buffer !== "remote-buffer") throw new Error("buffer was not remote-first: " + String(proxy.buffer));
var descriptor = proxy.toJSON();
if (!descriptor || descriptor.metadata.dtype !== 4 || descriptor.format !== "arrow_c_data" || descriptor.buffer !== "descriptor-buffer") {
  throw new Error("local descriptor toJSON changed: " + JSON.stringify(descriptor));
}
var metadataDescriptor = Object.getOwnPropertyDescriptor(proxy, "metadata");
if (!metadataDescriptor || metadataDescriptor.enumerable !== true) {
  throw new Error("remote metadata descriptor missing: " + JSON.stringify(metadataDescriptor));
}
var requested = omnivm.calls.filter(function(call) { return call.op === "handle_get"; }).map(function(call) { return call.key; });
["id", "runtime", "format", "metadata", "buffer"].forEach(function(key) {
  if (requested.indexOf(key) < 0) throw new Error("missing remote lookup for " + key + ": " + requested.join(","));
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node table descriptor field collision check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureProxyThenCollisionAvoidsPromiseAssimilation(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  calls: [],
  call: function(name, raw) {
    if (name !== "__manifest") throw new Error("unexpected bridge name " + name);
    var payload = JSON.parse(raw);
    this.calls.push(payload);
    if (payload.op === "handle_access") {
      return JSON.stringify({__omnivm_result__: true, value: {chatty: false}});
    }
    if (payload.op === "handle_retain") {
      return JSON.stringify({__omnivm_result__: true, value: true});
    }
    if (payload.op === "handle_contains") {
      return JSON.stringify({__omnivm_result__: true, value: payload.value === "then"});
    }
    if (payload.op === "handle_get" && payload.key === "then") {
      if (payload.id === 78) {
        return JSON.stringify({__omnivm_result__: true, value: {__omnivm_callable__: true, key: "then"}});
      }
      return JSON.stringify({__omnivm_result__: true, value: "remote-then"});
    }
    if (payload.op === "handle_call" && payload.key === "then") {
      return JSON.stringify({__omnivm_result__: true, value: "called:" + payload.args[0]});
    }
    throw new Error("unexpected op " + payload.op);
  }
};
` + code + `
var callableThen = globalThis.__omnivm_materialize_capture({__omnivm_resource__: true, id: 78, runtime: "javascript", kind: "object"});
if (callableThen.then !== undefined) throw new Error("callable remote then became a JS thenable");
if (omnivm.calls.some(function(call) { return call.op === "handle_get" && call.key === "then"; })) throw new Error("plain callable then access touched the bridge");
var thenMethod = omnivm.proxyGet(callableThen, "then");
if (typeof thenMethod !== "function") throw new Error("proxyGet did not recover callable remote then");
if (thenMethod("ok") !== "called:ok") throw new Error("callable remote then did not dispatch through handle_call");
var plainThen = globalThis.__omnivm_materialize_capture({__omnivm_resource__: true, id: 79, runtime: "javascript", kind: "object"});
var beforePlainThen = omnivm.calls.length;
if (plainThen.then !== undefined) throw new Error("non-callable remote then became a JS thenable");
if (omnivm.calls.length !== beforePlainThen) throw new Error("plain non-callable then access touched the bridge");
if (omnivm.proxyGet(plainThen, "then") !== "remote-then") throw new Error("proxyGet did not recover non-callable remote then");
var beforePromise = omnivm.calls.length;
var callablePromise = Promise.resolve(callableThen);
var plainPromise = Promise.resolve(plainThen);
if (omnivm.calls.length !== beforePromise) throw new Error("Promise.resolve touched remote then through the bridge");
callablePromise.then(function(value) {
  if (value !== callableThen) throw new Error("callable remote then assimilated the proxy");
});
plainPromise.then(function(value) {
  if (value !== plainThen) throw new Error("non-callable remote then assimilated the proxy");
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node then-collision check failed: %v\n%s", err, out)
	}
}

func TestJSLocalStreamMarksClosedAtEOF(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
var registered = 0;
var unregistered = 0;
globalThis.__omnivm_handle_finalizers = {
  register: function(target, id, token) {
    registered++;
  },
  unregister: function(token) {
    unregistered++;
  }
};
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "stream_cancel") throw new Error("local EOF should not cancel remote stream");
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 77, runtime: "javascript", kind: "stream", values: ["a"]});
var iter = stream[Symbol.iterator]();
var first = iter.next();
if (first.done !== false || first.value !== "a") throw new Error("first value mismatch");
var done = iter.next();
if (done.done !== true) throw new Error("stream did not report done at EOF");
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed at EOF");
if (registered !== 1) throw new Error("stream finalizer was not registered once: " + registered);
if (unregistered !== 1) throw new Error("stream finalizer was not unregistered at EOF: " + unregistered);
if (!requests.some(function(req) { return req.op === "handle_retain" && req.id === 77; })) {
  throw new Error("stream handle was not retained");
}
if (requests.some(function(req) { return req.op === "stream_cancel"; })) {
  throw new Error("local EOF called remote stream_cancel");
}
if (stream.cancel() !== false) throw new Error("closed local stream cancel should be idempotent false");
if (unregistered !== 1) throw new Error("closed local stream was unregistered more than once");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node local stream EOF lifecycle check failed: %v\n%s", err, out)
	}
}

func TestJSLocalStreamMaterializesNestedValuesLazily(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
var unregistered = 0;
globalThis.__omnivm_handle_finalizers = {
  register: function(target, id, token) {},
  unregister: function(token) {
    unregistered++;
  }
};
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "handle_release_explicit") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_cancel") throw new Error("local stream should not call remote stream_cancel");
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var nested = {__omnivm_resource__: true, id: 101, runtime: "python", kind: "object"};
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 77, runtime: "javascript", kind: "stream", values: ["first", nested]});
var retainsBeforePull = requests.filter(function(req) { return req.op === "handle_retain"; }).map(function(req) { return req.id; });
if (JSON.stringify(retainsBeforePull) !== JSON.stringify([77])) {
  throw new Error("nested local stream value was retained before pull: " + JSON.stringify(retainsBeforePull));
}
var iter = stream[Symbol.iterator]();
var first = iter.next();
if (first.done !== false || first.value !== "first") throw new Error("first value mismatch: " + JSON.stringify(first));
var retainsAfterFirst = requests.filter(function(req) { return req.op === "handle_retain"; }).map(function(req) { return req.id; });
if (JSON.stringify(retainsAfterFirst) !== JSON.stringify([77])) {
  throw new Error("nested local stream value was retained before its chunk: " + JSON.stringify(retainsAfterFirst));
}
var second = iter.next();
if (second.done !== false || second.value.__omnivm_descriptor__.id !== 101) {
  throw new Error("second value was not the nested proxy: " + JSON.stringify(second));
}
var retainsAfterSecond = requests.filter(function(req) { return req.op === "handle_retain"; }).map(function(req) { return req.id; });
if (JSON.stringify(retainsAfterSecond) !== JSON.stringify([77, 101])) {
  throw new Error("nested local stream value was not retained when pulled: " + JSON.stringify(retainsAfterSecond));
}
stream.cancel();
if (stream.__omnivm_closed__ !== true) throw new Error("local stream did not close after cancel");
if (unregistered !== 1) throw new Error("local stream finalizer unregister mismatch: " + unregistered);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node local stream lazy materialization check failed: %v\n%s", err, out)
	}
}

func TestJSRemoteStreamCancelsOnEarlyBreak(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
var registered = 0;
var unregistered = 0;
globalThis.__omnivm_handle_finalizers = {
  register: function(target, id, token) {
    registered++;
  },
  unregister: function(token) {
    unregistered++;
  }
};
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_next") return JSON.stringify({__omnivm_result__: true, value: {done: false, value: "a"}});
    if (payload.op === "stream_cancel") return JSON.stringify({__omnivm_result__: true, value: true});
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 88, runtime: "javascript", kind: "stream"});
var seen = [];
for (var item of stream) {
  seen.push(item);
  break;
}
if (seen.length !== 1 || seen[0] !== "a") throw new Error("first value mismatch: " + JSON.stringify(seen));
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed after early break");
if (registered !== 1) throw new Error("stream finalizer was not registered once: " + registered);
if (unregistered !== 1) throw new Error("stream finalizer was not unregistered after early break: " + unregistered);
var cancels = requests.filter(function(req) { return req.op === "stream_cancel"; });
if (cancels.length !== 1 || cancels[0].id !== 88) throw new Error("stream cancel requests mismatch: " + JSON.stringify(cancels));
if (stream.cancel() !== false) throw new Error("closed remote stream cancel should be idempotent false");
if (unregistered !== 1) throw new Error("closed remote stream was unregistered more than once");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node remote stream early-break lifecycle check failed: %v\n%s", err, out)
	}
}

func TestJSRemoteStreamCancelsOnChunkMaterializationError(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
var registered = 0;
var unregistered = 0;
globalThis.__omnivm_handle_finalizers = {
  register: function(target, id, token) {
    registered++;
  },
  unregister: function(token) {
    unregistered++;
  }
};
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_next") return JSON.stringify({__omnivm_result__: true, value: {done: false, value: {bad: true}}});
    if (payload.op === "stream_cancel") throw new Error("cancel failed");
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 89, runtime: "javascript", kind: "stream"});
globalThis.__omnivm_stream_chunk_value = function(_value) {
  throw new Error("chunk boom");
};
try {
  stream[Symbol.iterator]().next();
} catch (err) {
  if (!String(err && err.message).includes("chunk boom")) throw err;
  var cleanupErrors = omnivm.cleanupErrors(err);
  if (!cleanupErrors || cleanupErrors.length !== 1 || cleanupErrors[0].message !== "cancel failed") {
    throw new Error("cleanup error missing: " + JSON.stringify(cleanupErrors && cleanupErrors.map(function(item) { return item.message; })));
  }
  cleanupErrors.length = 0;
  if (omnivm.cleanupErrors(err)[0].message !== "cancel failed") throw new Error("cleanupErrors returned internal storage");
}
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed after materialization error");
if (registered !== 1) throw new Error("stream finalizer was not registered once: " + registered);
if (unregistered !== 1) throw new Error("stream finalizer was not unregistered after materialization error: " + unregistered);
var cancels = requests.filter(function(req) { return req.op === "stream_cancel"; });
if (cancels.length !== 1 || cancels[0].id !== 89) throw new Error("stream cancel requests mismatch: " + JSON.stringify(cancels));
if (stream.cancel() !== false) throw new Error("closed remote stream cancel should be idempotent false");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node remote stream materialization-error lifecycle check failed: %v\n%s", err, out)
	}
}

func TestJSRemoteStreamRejectsMalformedNextChunk(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
var registered = 0;
var unregistered = 0;
globalThis.__omnivm_handle_finalizers = {
  register: function(target, id, token) {
    registered++;
  },
  unregister: function(token) {
    unregistered++;
  }
};
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_next") return JSON.stringify({__omnivm_result__: true, value: ""});
    if (payload.op === "stream_cancel") return JSON.stringify({__omnivm_result__: true, value: true});
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 90, runtime: "javascript", kind: "stream"});
try {
  stream[Symbol.iterator]().next();
  throw new Error("malformed stream chunk was treated as a value or EOF");
} catch (err) {
  if (!String(err && err.message).includes("stream_next returned malformed chunk")) throw err;
  if (err.boundary_path !== "stream_next") throw new Error("malformed chunk boundary mismatch: " + err.boundary_path);
  if (!err.details || !err.details.stream || err.details.stream.id !== 90 || err.details.stream.chunk !== "") {
    throw new Error("malformed chunk details mismatch: " + JSON.stringify(err.details));
  }
}
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed after malformed chunk");
if (registered !== 1) throw new Error("stream finalizer was not registered once: " + registered);
if (unregistered !== 1) throw new Error("stream finalizer was not unregistered after malformed chunk: " + unregistered);
var cancels = requests.filter(function(req) { return req.op === "stream_cancel"; });
if (cancels.length !== 1 || cancels[0].id !== 90) throw new Error("malformed chunk cancel mismatch: " + JSON.stringify(cancels));
if (stream.cancel() !== false) throw new Error("closed remote stream cancel should be idempotent false");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node remote stream malformed-chunk check failed: %v\n%s", err, out)
	}
}

func TestJSNodeReadableCloseUsesDescriptorSafeIteratorCleanup(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  call: function(_runtime, _payloadRaw) {
    return JSON.stringify({__omnivm_result__: true, value: true});
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 93, runtime: "javascript", kind: "stream"});
var nextStartedResolve;
var nextStarted = new Promise(function(resolve) {
  nextStartedResolve = resolve;
});
var resolveNext;
var cancelled = [];
var returnGetterCount = 0;
var releaseGetterCount = 0;
var iterator = {
  next: function() {
    nextStartedResolve();
    return new Promise(function(resolve) {
      resolveNext = resolve;
    });
  }
};
Object.defineProperty(iterator, "return", {
  get: function() {
    returnGetterCount++;
    throw new Error("return getter should not run");
  },
  configurable: true
});
Object.defineProperty(iterator, "releaseLock", {
  get: function() {
    releaseGetterCount++;
    throw new Error("releaseLock getter should not run");
  }
});
stream[Symbol.asyncIterator] = function() {
  return iterator;
};
stream.cancel = function(reason) {
  cancelled.push(reason && reason.message ? reason.message : String(reason));
  return true;
};
var readable = stream.toNodeReadable({objectMode: true});
readable.on("error", function() {});
readable.resume();
nextStarted.then(function() {
  readable.destroy(new Error("client abort"));
  resolveNext({done: false, value: "late"});
  setImmediate(function() {
    if (returnGetterCount !== 0) throw new Error("return getter was invoked");
    if (releaseGetterCount !== 0) throw new Error("releaseLock getter was invoked");
    if (cancelled.length !== 1 || cancelled[0] !== "client abort") {
      throw new Error("source cancel reason mismatch: " + JSON.stringify(cancelled));
    }
  });
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node readable descriptor-safe close check failed: %v\n%s", err, out)
	}
}

func TestJSNodeReadableDropsLateChunksAfterDestroy(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  call: function(_runtime, _payloadRaw) {
    return JSON.stringify({__omnivm_result__: true, value: true});
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 91, runtime: "javascript", kind: "stream"});
var resolveNext;
var nextStartedResolve;
var nextStarted = new Promise(function(resolve) {
  nextStartedResolve = resolve;
});
var returned = 0;
stream[Symbol.asyncIterator] = function() {
  return {
    next: function() {
      nextStartedResolve();
      return new Promise(function(resolve) {
        resolveNext = resolve;
      });
    },
    return: function(_reason) {
      returned++;
      return Promise.resolve(true);
    }
  };
};
var readable = stream.toNodeReadable({objectMode: true});
var pushed = [];
var originalPush = readable.push;
readable.push = function(value) {
  pushed.push(value);
  return originalPush.call(this, value);
};
readable.on("error", function() {});
readable.resume();
nextStarted.then(function() {
  readable.destroy(new Error("client abort"));
  resolveNext({done: false, value: "late"});
  setImmediate(function() {
    if (returned !== 1) throw new Error("destroy did not close iterator exactly once: " + returned);
    if (pushed.length !== 0) throw new Error("destroyed readable pushed late chunks: " + JSON.stringify(pushed));
  });
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node readable destroy late-chunk check failed: %v\n%s", err, out)
	}
}

func TestJSNodeReadablePushesNullAtSourceEOF(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_cancel") throw new Error("EOF should not cancel remote stream");
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 95, runtime: "javascript", kind: "stream", values: ["a"]});
var readable = stream.toNodeReadable({objectMode: true});
var pushed = [];
var originalPush = readable.push;
readable.push = function(value) {
  pushed.push(value);
  return originalPush.call(this, value);
};
var timer = setTimeout(function() {
  throw new Error("readable did not end after source EOF; pushed=" + JSON.stringify(pushed));
}, 100);
readable.on("end", function() {
  clearTimeout(timer);
  if (pushed.length !== 2 || pushed[0] !== "a" || pushed[1] !== null) {
    throw new Error("readable EOF push sequence mismatch: " + JSON.stringify(pushed));
  }
  if (requests.some(function(req) { return req.op === "stream_cancel"; })) {
    throw new Error("source EOF called remote stream_cancel");
  }
});
readable.resume();
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node readable EOF check failed: %v\n%s", err, out)
	}
}

func TestJSNodeReadableDropsLateChunksAfterSourceCancel(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_cancel") return JSON.stringify({__omnivm_result__: true, value: true});
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 94, runtime: "javascript", kind: "stream"});
var resolveNext;
var nextStartedResolve;
var nextStarted = new Promise(function(resolve) {
  nextStartedResolve = resolve;
});
var returned = 0;
stream[Symbol.asyncIterator] = function() {
  return {
    next: function() {
      nextStartedResolve();
      return new Promise(function(resolve) {
        resolveNext = resolve;
      });
    },
    return: function(_reason) {
      returned++;
      return Promise.resolve(true);
    }
  };
};
var readable = stream.toNodeReadable({objectMode: true});
var pushed = [];
var originalPush = readable.push;
readable.push = function(value) {
  pushed.push(value);
  return originalPush.call(this, value);
};
readable.on("error", function() {});
readable.resume();
nextStarted.then(function() {
  if (stream.cancel("client abort") !== true) throw new Error("source cancel did not release remote stream");
  resolveNext({done: false, value: "late"});
  setImmediate(function() {
    if (returned !== 1) throw new Error("source cancel did not close readable iterator exactly once: " + returned);
    if (pushed.length !== 0) throw new Error("cancelled readable pushed late chunks: " + JSON.stringify(pushed));
    var cancels = requests.filter(function(req) { return req.op === "stream_cancel"; });
    if (cancels.length !== 1 || cancels[0].id !== 94) throw new Error("stream cancel requests mismatch: " + JSON.stringify(cancels));
  });
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node readable source-cancel late-chunk check failed: %v\n%s", err, out)
	}
}

func TestJSNodeReadableSourceCancelAbsorbsAdapterCloseRejection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_cancel") return JSON.stringify({__omnivm_result__: true, value: true});
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var unhandled = [];
process.on("unhandledRejection", function(err) {
  unhandled.push(err && err.message);
});
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 96, runtime: "javascript", kind: "stream"});
var returned = 0;
stream[Symbol.asyncIterator] = function() {
  return {
    next: function() {
      return Promise.resolve({done: true});
    },
    return: function(_reason) {
      returned++;
      return Promise.reject(new Error("adapter close failed"));
    }
  };
};
var readable = stream.toNodeReadable({objectMode: true});
readable.on("error", function() {});
if (stream.cancel("client abort") !== true) throw new Error("source cancel did not release remote stream");
setTimeout(function() {
  if (returned !== 1) throw new Error("source cancel did not close readable iterator exactly once: " + returned);
  if (unhandled.length !== 0) throw new Error("source cancel produced unhandled close rejection: " + JSON.stringify(unhandled));
  var cancels = requests.filter(function(req) { return req.op === "stream_cancel"; });
  if (cancels.length !== 1 || cancels[0].id !== 96) throw new Error("stream cancel requests mismatch: " + JSON.stringify(cancels));
}, 20);
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node readable source-cancel close-rejection check failed: %v\n%s", err, out)
	}
}

func TestJSNodeReadableSerializesPendingPulls(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  call: function(_runtime, _payloadRaw) {
    return JSON.stringify({__omnivm_result__: true, value: true});
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 92, runtime: "javascript", kind: "stream"});
var active = 0;
var maxConcurrent = 0;
var resolves = [];
stream[Symbol.asyncIterator] = function() {
  return {
    next: function() {
      active++;
      if (active > maxConcurrent) maxConcurrent = active;
      return new Promise(function(resolve) {
        resolves.push(function(value) {
          active--;
          resolve(value);
        });
      });
    },
    return: function(_reason) {
      return Promise.resolve(true);
    }
  };
};
var readable = stream.toNodeReadable({objectMode: true});
readable._read(0);
readable._read(0);
Promise.resolve().then(function() {
  if (active !== 1 || maxConcurrent !== 1 || resolves.length !== 1) {
    throw new Error("readable started concurrent owner pulls: active=" + active + " max=" + maxConcurrent + " pending=" + resolves.length);
  }
  resolves[0]({done: true});
  setImmediate(function() {
    if (active !== 0 || maxConcurrent !== 1) {
      throw new Error("readable pull did not settle cleanly: active=" + active + " max=" + maxConcurrent);
    }
  });
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node readable pending-pull serialization check failed: %v\n%s", err, out)
	}
}

func TestJSNodeReadableDestroysOnSynchronousNextError(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
globalThis.omnivm = {
  call: function(_runtime, _payloadRaw) {
    return JSON.stringify({__omnivm_result__: true, value: true});
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 93, runtime: "javascript", kind: "stream"});
var returned = 0;
stream[Symbol.asyncIterator] = function() {
  return {
    next: function() {
      throw new Error("next boom");
    },
    return: function(_reason) {
      returned++;
      return Promise.resolve(true);
    }
  };
};
var readable = stream.toNodeReadable({objectMode: true});
var errorMessage = "";
readable.on("error", function(err) {
  errorMessage = err && err.message;
});
readable._read(0);
setImmediate(function() {
  if (errorMessage !== "next boom") {
    throw new Error("readable did not emit synchronous next error: " + errorMessage);
  }
  if (returned !== 1) {
    throw new Error("readable destroy did not close iterator after next error: " + returned);
  }
});
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node readable synchronous next error check failed: %v\n%s", err, out)
	}
}

func TestJSRemoteStreamTerminalFallbackMarksClosed(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(nil)
	script := `
var requests = [];
var registered = 0;
var unregistered = 0;
globalThis.__omnivm_handle_finalizers = {
  register: function(target, id, token) {
    registered++;
  },
  unregister: function(token) {
    unregistered++;
  }
};
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_next") return JSON.stringify({__omnivm_result__: false, value: null});
    if (payload.op === "stream_cancel") throw new Error("terminal fallback should not call stream_cancel");
    throw new Error("unexpected manifest op " + payload.op);
  }
};
` + code + `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 99, runtime: "javascript", kind: "stream"});
var done = stream[Symbol.iterator]().next();
if (done.done !== true) throw new Error("terminal fallback did not report done");
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed after terminal fallback");
if (registered !== 1) throw new Error("stream finalizer was not registered once: " + registered);
if (unregistered !== 1) throw new Error("stream finalizer was not unregistered after terminal fallback: " + unregistered);
if (requests.some(function(req) { return req.op === "stream_cancel"; })) {
  throw new Error("terminal fallback called remote stream_cancel");
}
if (stream.cancel() !== false) throw new Error("closed terminal stream cancel should be idempotent false");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node remote stream terminal fallback lifecycle check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureCallableProxyOwnKeysPreservesProxyInvariants(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	code := injectJSCaptures(map[string]string{
		"handler": `{"__omnivm_resource__":true,"id":7,"runtime":"javascript","kind":"callable","closed":false}`,
	})
	script := `
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    var payload = JSON.parse(payloadRaw);
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "handle_access") return JSON.stringify({__omnivm_result__: true, value: {}});
    if (payload.op === "handle_iter" && payload.mode === "keys") return JSON.stringify({__omnivm_result__: true, value: ["remoteKey"]});
    if (payload.op === "handle_contains") return JSON.stringify({__omnivm_result__: true, value: payload.value === "remoteKey"});
    if (payload.op === "handle_get" && payload.key === "remoteKey") return JSON.stringify({__omnivm_result__: true, value: "remote-value"});
    throw new Error("handle " + payload.id + " has no property " + (payload.key || payload.value || payload.op));
  }
};
` + code + `
var keys = Reflect.ownKeys(globalThis.handler);
if (keys.indexOf("remoteKey") < 0) throw new Error("missing remote key: " + keys.join(","));
if (keys.indexOf("prototype") < 0) {
  throw new Error("missing required function own keys: " + keys.join(","));
}
var enumerable = Object.keys(globalThis.handler);
if (enumerable.indexOf("remoteKey") < 0) throw new Error("Object.keys missed remote key: " + enumerable.join(","));
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node callable proxy ownKeys check failed: %v\n%s", err, out)
	}
}

func TestJSCaptureMaterializerKeepsResourceMetadataPrivate(t *testing.T) {
	code := injectJSCaptures(map[string]string{
		"user": `{"__omnivm_resource__":true,"id":7,"runtime":"python","kind":"object","closed":false}`,
	})
	if !contains(code, "__omnivm_proxy_handle_id") || !contains(code, "payload.id = globalThis.__omnivm_proxy_handle_id(target)") {
		t.Fatalf("JS materializer should route internal handle ids through private descriptor metadata, got %q", code)
	}
	for _, localAssignment := range []string{"target.id = value.id", "target.runtime = value.runtime", "target.kind = value.kind", "target.closed = value.closed"} {
		if contains(code, localAssignment) {
			t.Fatalf("JS resource proxy should not expose internal metadata as user-visible properties %q in %q", localAssignment, code)
		}
	}
}

func TestWrapRubyCaptures(t *testing.T) {
	code := wrapRubyCaptures("puts x", map[string]string{"x": `"hi"`})
	if !contains(code, "JSON.parse") {
		t.Error("should use JSON.parse")
	}
	if !contains(code, "__omnivm_materialize_capture") {
		t.Error("should materialize captures")
	}
}

func TestWrapRubyCapturesUsesSafeBindingNames(t *testing.T) {
	code := wrapRubyCaptures(`puts(($omnivm_bindings ||= {})["class"])`, map[string]string{"class": `"hi"`})
	if !contains(code, `($omnivm_bindings ||= {})["class"] =`) || !contains(code, `__omnivm_materialize_capture(JSON.parse`) {
		t.Fatalf("Ruby wrapper should assign unsafe names through binding map, got %q", code)
	}
	if contains(code, "$class =") || contains(code, "class = $class") {
		t.Fatalf("Ruby wrapper should not emit reserved global/local names, got %q", code)
	}
}

func TestInjectRubyCapturesMaterializesHandleProxy(t *testing.T) {
	code := injectRubyCaptures(map[string]string{
		"req": `{"__omnivm_resource__":true,"id":7,"runtime":"ruby","kind":"request","closed":false}`,
	})
	if !contains(code, "class OmniVMHandleProxy") {
		t.Fatalf("Ruby materializer should define handle proxy, got %q", code)
	}
	if contains(code, "class OmniVMHandleProxy\n  include Enumerable") {
		t.Fatalf("Ruby handle proxy should avoid broad Enumerable methods shadowing remote data keys, got %q", code)
	}
	if !contains(code, `op: "handle_access"`) {
		t.Fatalf("Ruby materializer should record handle access, got %q", code)
	}
	if !contains(code, "OMNIVM_MISSING = Object.new") || !contains(code, "def __omnivm_data_key?") || !contains(code, "def __omnivm_data_key_value") {
		t.Fatalf("Ruby materializer should prefer remote data keys before local proxy methods, got %q", code)
	}
	if !contains(code, "def then(*args, &block)") || !contains(code, `__omnivm_data_key_value("then")`) {
		t.Fatalf("Ruby materializer should let remote then fields beat Object#then, got %q", code)
	}
	if !contains(code, "def class") || !contains(code, `__omnivm_data_key_value("class")`) ||
		!contains(code, "def object_id") || !contains(code, `__omnivm_data_key_value("object_id")`) ||
		!contains(code, "def inspect") || !contains(code, `__omnivm_data_key_value("inspect")`) ||
		!contains(code, "def hash") || !contains(code, `__omnivm_data_key_value("hash")`) ||
		!contains(code, "def to_s") || !contains(code, `__omnivm_data_key_value("to_s")`) {
		t.Fatalf("Ruby materializer should let remote identity-name fields beat local Object methods, got %q", code)
	}
	if !contains(code, "def __omnivm_internal_descriptor_key?(key)") ||
		!contains(code, `@value["__omnivm_resource__"] == true || @value["__omnivm_table__"] == true || @value["__omnivm_job__"] == true`) ||
		!contains(code, `"__omnivm_resource__", "__omnivm_table__", "__omnivm_job__", "__omnivm_materialized__"`) ||
		!contains(code, `"format", "ownership", "metadata", "buffer", "released"`) ||
		!contains(code, `"done", "cancelled", "cancelReason", "payload", "result"`) ||
		!contains(code, "def __omnivm_local_value(key)") ||
		!contains(code, "__omnivm_local_key?(key)") {
		t.Fatalf("Ruby resource proxy should keep internal descriptor metadata out of user-visible fields, got %q", code)
	}
	if !contains(code, "def self.__omnivm_missing_bridge_error?(error)") || !contains(code, "raise unless __omnivm_missing_bridge_error?(e)") {
		t.Fatalf("Ruby materializer should propagate owner lifecycle errors while preserving ordinary missing-field fallbacks, got %q", code)
	}
	if !contains(code, "chatty cross-runtime proxy access detected") {
		t.Fatalf("Ruby materializer should warn on chatty proxy access, got %q", code)
	}
	if !contains(code, "@@__omnivm_chatty_warned_limit = 4096") || !contains(code, "@@__omnivm_chatty_warned.keys.first") {
		t.Fatalf("Ruby materializer should bound chatty warning dedupe entries, got %q", code)
	}
	if !contains(code, "def __omnivm_materialize_chatty") || !contains(code, `materialize: true`) {
		t.Fatalf("Ruby materializer should automatically batch-materialize chatty proxy items, got %q", code)
	}
	if !contains(code, `op: "handle_get"`) {
		t.Fatalf("Ruby materializer should fetch handle properties, got %q", code)
	}
	if !contains(code, "def omnivm_get(key)") || !contains(code, "def omnivm_set(key, value)") || !contains(code, "def omnivm_call(key, *args)") || !contains(code, "def omnivm_len") || !contains(code, "def omnivm_iter(mode = \"values\")") || !contains(code, "def omnivm_keys") || !contains(code, "def omnivm_values") || !contains(code, "def omnivm_items") || !contains(code, "def omnivm_contains(key)") || !contains(code, "def omnivm_close") {
		t.Fatalf("Ruby materializer should expose explicit proxy get/set/call/len/iter/contains/close helpers for collision cases, got %q", code)
	}
	if !contains(code, "def omnivm_close(value)") ||
		!contains(code, "def proxy_close(value)") ||
		!contains(code, "def omnivm_close(value)\n      proxy_close(value)\n    end") ||
		!contains(code, "def __omnivm_actual_public_method?(value, name)") ||
		!contains(code, "def __omnivm_lifecycle_method_without_required_args?(value, name)") ||
		!contains(code, "return false unless __omnivm_actual_public_method?(value, name)") ||
		!contains(code, "arity = value.method(name).arity") ||
		!contains(code, "arity == 0 || arity == -1") ||
		!contains(code, "return value.public_send(:omnivm_close) if __omnivm_lifecycle_method_without_required_args?(value, :omnivm_close)") ||
		!contains(code, "if __omnivm_lifecycle_method_without_required_args?(value, :close)") ||
		!contains(code, "result = value.public_send(:close)\n    return result.nil? ? true : result") ||
		!contains(code, "result = value.public_send(:close)\n        return result.nil? ? true : result") ||
		!contains(code, "if __omnivm_lifecycle_method_without_required_args?(value, :dispose)") ||
		!contains(code, "result = value.public_send(:dispose)\n    return result.nil? ? true : result") ||
		!contains(code, "result = value.public_send(:dispose)\n        return result.nil? ? true : result") ||
		!contains(code, "def cleanup_errors(error)") ||
		!contains(code, "def __record_cleanup_error(error, cleanup_error)") ||
		!contains(code, "error.instance_variable_set(:@omnivm_cleanup_errors, errors)") {
		t.Fatalf("Ruby materializer should expose top-level and OmniVM proxy close helpers, got %q", code)
	}
	if !contains(code, "OmniVM stream_next returned malformed chunk for handle") ||
		!contains(code, "def __omnivm_runtime_error(message, boundary_path, details = nil)") ||
		!contains(code, "return OmniVM::RuntimeError.new(message, runtime: \"ruby\", boundary_path: boundary_path, details: details)") ||
		!contains(code, "attr_reader :runtime, :origin_runtime, :type, :boundary_path, :original_error_handle, :details_json") ||
		!contains(code, "def stack_frames") ||
		!contains(code, "def stack_frames=(value)") ||
		!contains(code, "def stackFrames=(value)") ||
		!contains(code, "def cause_chain") ||
		!contains(code, "def cause_chain=(value)") ||
		!contains(code, "def causeChain=(value)") ||
		!contains(code, "def traceback=(value)") ||
		!contains(code, "def originRuntime=(value)") ||
		!contains(code, "def boundaryPath=(value)") ||
		!contains(code, "def originalErrorHandle=(value)") ||
		!contains(code, "def details") ||
		!contains(code, "def details=(value)") ||
		!contains(code, "def details_json=(value)") ||
		!contains(code, "@details = __omnivm_copy_json_value(JSON.parse(value))") ||
		!contains(code, "def detailsJson=(value)") ||
		!contains(code, "def to_h") ||
		!contains(code, "def to_json(*args)") ||
		!contains(code, `malformed = __omnivm_runtime_error("OmniVM stream_next returned malformed chunk`) ||
		!contains(code, "OmniVM.__record_cleanup_error(malformed, cleanup_error)") {
		t.Fatalf("Ruby stream proxy should reject malformed stream_next chunks with structured error details and explicit cancel cleanup, got %q", code)
	}
	if contains(code, "value.respond_to?(:close)") || contains(code, "value.respond_to?(:omnivm_close)") {
		t.Fatalf("Ruby proxy close helpers should not trust respond_to_missing? for lifecycle methods")
	}
	if !contains(code, `op: "handle_index"`) || !contains(code, `op: "handle_set"`) || !contains(code, `op: "handle_call"`) || !contains(code, `op: "handle_len"`) || !contains(code, `op: "handle_iter"`) || !contains(code, `op: "handle_contains"`) {
		t.Fatalf("Ruby materializer should forward generic index/set/call/len/iter/contains operations, got %q", code)
	}
	if !contains(code, `value["zeroArg"] == true`) {
		t.Fatalf("Ruby materializer should invoke zero-arg callable descriptors as property access, got %q", code)
	}
	if !contains(code, `key.end_with?("=")`) || !contains(code, `key[0...-1]`) {
		t.Fatalf("Ruby materializer should route property assignment syntax through handle_set, got %q", code)
	}
	if !contains(code, "def __omnivm_encode_arg") || !contains(code, `"__omnivm_runtime_ref__"`) || !contains(code, "args.map { |arg| __omnivm_encode_arg(arg) }") {
		t.Fatalf("Ruby proxy calls should preserve complex args as runtime refs, got %q", code)
	}
	if !contains(code, "ObjectSpace.define_finalizer") || !contains(code, `op: "handle_release_finalizer"`) {
		t.Fatalf("Ruby materializer should queue finalizer releases, got %q", code)
	}
	if !contains(code, "@__omnivm_closed = false") ||
		!contains(code, `JSON.generate({op: "handle_release_explicit", id: @value["id"]})`) ||
		!contains(code, `released = env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"] == true`) ||
		!contains(code, "if released\n      @__omnivm_closed = true") ||
		!contains(code, "ObjectSpace.undefine_finalizer(self)") ||
		!contains(code, "return false if @__omnivm_closed == true") ||
		!contains(code, "released\n  end") {
		t.Fatalf("Ruby explicit proxy close should be idempotent, return the manifest release result, and unregister its finalizer after release, got %q", code)
	}
	if !contains(code, "class OmniVMStreamProxy") ||
		!contains(code, `@local_values = value["values"].is_a?(Array) ? value["values"] : nil`) ||
		!contains(code, "begin\n      if @local_values\n        @local_values.each do |item|") ||
		!contains(code, "yield __omnivm_materialize_capture(item)") ||
		!contains(code, "ensure\n      if @__omnivm_closed != true") ||
		!contains(code, "begin\n          close\n        rescue => cleanup_error") ||
		!contains(code, "return __omnivm_mark_closed if @local_values") ||
		!contains(code, "def __omnivm_mark_closed") ||
		!contains(code, "rescue\n          __omnivm_mark_closed\n          raise") ||
		!contains(code, `JSON.generate({op: "stream_cancel", id: @value["id"]})`) ||
		!contains(code, `released = env.is_a?(Hash) && env["__omnivm_result__"] == true && env["value"] == true`) ||
		!contains(code, "__omnivm_mark_closed if released") ||
		!contains(code, "def omnivm_close\n    close\n  end") ||
		!contains(code, "body_error = $!") ||
		!contains(code, "OmniVM.__record_cleanup_error(body_error, cleanup_error)") {
		t.Fatalf("Ruby stream proxies should expose idempotent collision-safe close helpers, return the manifest release result, and mark pull errors closed, got %q", code)
	}
	if contains(code, "def close\n    return false if @__omnivm_closed == true\n    begin\n      OmniVM.call(\"__manifest\", JSON.generate({op: \"stream_cancel\"") {
		t.Fatalf("Ruby stream close should not swallow user-initiated cancellation failures")
	}
	if !contains(code, `op: "handle_retain"`) || !contains(code, "def self.omnivm_retain") {
		t.Fatalf("Ruby materializer should retain handles for guest proxy lifetime, got %q", code)
	}
	if !contains(code, `op: "handle_adopt"`) || !contains(code, "def self.omnivm_adopt") || !contains(code, `@value["transfer"] == true`) {
		t.Fatalf("Ruby materializer should adopt returned transfer handles, got %q", code)
	}
	if !contains(code, "WeakRef.new") || !contains(code, "$__omnivm_proxy_cache") || !contains(code, `__omnivm_cached_proxy("handle", value)`) {
		t.Fatalf("Ruby materializer should weakly cache handle proxies by identity, got %q", code)
	}
	if !contains(code, "cached.instance_variable_get(:@__omnivm_closed) != true") {
		t.Fatalf("Ruby materializer should not reuse closed cached proxies, got %q", code)
	}
	if !contains(code, "def __omnivm_prune_proxy_cache") || !contains(code, "$__omnivm_proxy_cache_limit") || !contains(code, "WeakRef::RefError") {
		t.Fatalf("Ruby materializer should bound stale weak proxy cache entries, got %q", code)
	}
	if !contains(code, "$req = (begin; __omnivm_materialize_capture(") {
		t.Fatalf("Ruby capture should be materialized during injection, got %q", code)
	}
	if !contains(code, "value[\"__omnivm_job__\"] == true ||\n      value[\"__omnivm_stream__\"] == true") || !contains(code, `return __omnivm_materialize_capture(value)`) {
		t.Fatalf("Ruby bridge results should materialize returned stream descriptors, got %q", code)
	}
	if !contains(code, "def __omnivm_stream_chunk_value") || !contains(code, "OmniVM.get_buffer(buffer_name)") || !contains(code, "yield __omnivm_stream_chunk_value(item[\"value\"])") {
		t.Fatalf("Ruby stream proxy should materialize byte-table chunks as binary strings, got %q", code)
	}
}

func TestInjectRubyCapturesUsesSafeBindingNames(t *testing.T) {
	code := injectRubyCaptures(map[string]string{"class": `"hi"`})
	if !contains(code, `($omnivm_bindings ||= {})["class"] =`) || !contains(code, `__omnivm_materialize_capture(JSON.parse`) {
		t.Fatalf("Ruby capture injection should assign unsafe names through binding map, got %q", code)
	}
}

func TestRubyHandleProxyLifecycleNameCollisionsPreferRemoteFields(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  @@fields = {
    "close" => "remote-close-field",
    "dispose" => "remote-dispose-field"
  }
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    return JSON.generate({"__omnivm_result__" => true, "value" => {"chatty" => false}}) if req["op"] == "handle_access"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_retain"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_release_explicit"
    if req["op"] == "handle_get" && @@fields.key?(req["key"])
      return JSON.generate({"__omnivm_result__" => true, "value" => @@fields[req["key"]]})
    end
    if req["op"] == "handle_call" && ["close", "dispose"].include?(req["key"])
      raise "lifecycle helper called remote #{req["key"]} field"
    end
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
proxy = __omnivm_materialize_capture({"__omnivm_resource__" => true, "id" => 80, "runtime" => "python", "kind" => "object"})
raise "close was not remote-first: #{proxy.close.inspect}" unless proxy.close == "remote-close-field"
raise "dispose was not remote-first: #{proxy.dispose.inspect}" unless proxy.dispose == "remote-dispose-field"
raise "omnivm_get did not recover remote close" unless proxy.omnivm_get("close") == "remote-close-field"
raise "omnivm_get did not recover remote dispose" unless proxy.omnivm_get("dispose") == "remote-dispose-field"
raise "proxy_close did not release the proxy" unless OmniVM.proxy_close(proxy) == true
raise "proxy_close was not idempotent after release" unless OmniVM.proxy_close(proxy) == false
raise "module omnivm_close alias was not idempotent after release" unless OmniVM.omnivm_close(proxy) == false
class TextDispose
  def dispose
    "disposed"
  end
end
class FalseDispose
  def dispose
    false
  end
end
class NilDispose
  def dispose
    nil
  end
end
class CloseAndDispose
  def close
    "closed"
  end
  def dispose
    raise "dispose should not run when close is available"
  end
end
class RequiredCloseAndDispose
  def close(reason)
    raise "required-arg close should not run"
  end
  def dispose
    "disposed-after-required-close"
  end
end
class RequiredOnlyClose
  def close(reason)
    raise "required-arg close should not run"
  end
end
raise "dispose result was not preserved" unless OmniVM.proxy_close(TextDispose.new) == "disposed"
raise "module omnivm_close alias did not preserve dispose result" unless OmniVM.omnivm_close(TextDispose.new) == "disposed"
raise "false dispose result was not preserved" unless OmniVM.proxy_close(FalseDispose.new) == false
raise "nil dispose result did not normalize true" unless OmniVM.proxy_close(NilDispose.new) == true
raise "close did not take priority over dispose" unless OmniVM.proxy_close(CloseAndDispose.new) == "closed"
raise "required-arg close should be skipped for dispose" unless OmniVM.proxy_close(RequiredCloseAndDispose.new) == "disposed-after-required-close"
raise "required-arg close should not be treated as lifecycle close" unless OmniVM.proxy_close(RequiredOnlyClose.new) == false
requested = OmniVM.requests.select { |req| req["op"] == "handle_get" }.map { |req| req["key"] }
["close", "dispose"].each do |key|
  raise "missing remote lookup for #{key}: #{requested.inspect}" unless requested.include?(key)
end
releases = OmniVM.requests.select { |req| req["op"] == "handle_release_explicit" }
raise "explicit release mismatch: #{releases.inspect}" unless releases.length == 1 && releases[0]["id"] == 80
if OmniVM.requests.any? { |req| req["op"] == "handle_call" && ["close", "dispose"].include?(req["key"]) }
  raise "lifecycle close dispatched to a remote lifecycle-named field"
end
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby lifecycle-name collision check failed: %v\n%s", err, out)
	}
}

func TestRubyHandleProxyCacheSkipsClosedProxy(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_adopt"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_release_explicit"
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
descriptor = {"__omnivm_resource__" => true, "id" => 91, "runtime" => "ruby", "kind" => "object", "transfer" => true}
first = __omnivm_materialize_capture(descriptor)
raise "first close failed" unless OmniVM.proxy_close(first) == true
second = __omnivm_materialize_capture(descriptor)
raise "closed proxy was reused" if second.equal?(first)
raise "second close failed" unless OmniVM.proxy_close(second) == true
adopts = OmniVM.requests.select { |req| req["op"] == "handle_adopt" }
releases = OmniVM.requests.select { |req| req["op"] == "handle_release_explicit" }
expected = [{"op" => "handle_adopt", "id" => 91}, {"op" => "handle_adopt", "id" => 91}]
raise "adopt requests mismatch: #{adopts.inspect}" unless adopts == expected
expected_releases = [{"op" => "handle_release_explicit", "id" => 91}, {"op" => "handle_release_explicit", "id" => 91}]
raise "release requests mismatch: #{releases.inspect}" unless releases == expected_releases
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby closed proxy cache check failed: %v\n%s", err, out)
	}
}

func TestRubyHandleProxyIdentityNameCollisionsPreferRemoteFields(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  @@fields = {
    "class" => "remote-class",
    "object_id" => "remote-object-id",
    "hash" => "remote-hash",
    "inspect" => "remote-inspect",
    "to_s" => "remote-to-s",
    "to_h" => {"remote" => true},
    "to_json" => "{\"remote\":true}"
  }
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    return JSON.generate({"__omnivm_result__" => true, "value" => {"chatty" => false}}) if req["op"] == "handle_access"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_retain"
    return JSON.generate({"__omnivm_result__" => true, "value" => @@fields.key?(req["value"])}) if req["op"] == "handle_contains"
    if req["op"] == "handle_get" && @@fields.key?(req["key"])
      return JSON.generate({"__omnivm_result__" => true, "value" => @@fields[req["key"]]})
    end
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
proxy = __omnivm_materialize_capture({"__omnivm_resource__" => true, "id" => 81, "runtime" => "python", "kind" => "object"})
raise "class was not remote-first: #{proxy.class.inspect}" unless proxy.class == "remote-class"
raise "object_id was not remote-first: #{proxy.object_id.inspect}" unless proxy.object_id == "remote-object-id"
raise "hash was not remote-first: #{proxy.hash.inspect}" unless proxy.hash == "remote-hash"
raise "inspect was not remote-first: #{proxy.inspect.inspect}" unless proxy.inspect == "remote-inspect"
raise "to_s was not remote-first: #{proxy.to_s.inspect}" unless proxy.to_s == "remote-to-s"
raise "to_h was not remote-first: #{proxy.to_h.inspect}" unless proxy.to_h == {"remote" => true}
raise "to_json was not remote-first: #{proxy.to_json.inspect}" unless proxy.to_json == "{\"remote\":true}"
requested = OmniVM.requests.select { |req| req["op"] == "handle_get" }.map { |req| req["key"] }
["class", "object_id", "hash", "inspect", "to_s", "to_h", "to_json"].each do |key|
  raise "missing remote lookup for #{key}: #{requested.inspect}" unless requested.include?(key)
end
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby identity-name collision check failed: %v\n%s", err, out)
	}
}

func TestRubyHandleProxyTableDescriptorMetadataDoesNotShadowRemoteFields(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    return JSON.generate({"__omnivm_result__" => true, "value" => {"chatty" => false}}) if req["op"] == "handle_access"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_retain"
    if req["op"] == "handle_contains"
      return JSON.generate({"__omnivm_result__" => true, "value" => ["metadata", "buffer"].include?(req["value"])})
    end
    if req["op"] == "handle_index"
      raise "manifest HandleCall: handle #{req["id"]} has no index #{req["value"]}"
    end
    if req["op"] == "handle_get" && req["key"] == "metadata"
      return JSON.generate({"__omnivm_result__" => true, "value" => {"remote" => true}})
    end
    if req["op"] == "handle_get" && req["key"] == "buffer"
      return JSON.generate({"__omnivm_result__" => true, "value" => "remote-buffer"})
    end
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
proxy = __omnivm_materialize_capture({
  "__omnivm_table__" => true,
  "id" => 81,
  "runtime" => "python",
  "format" => "arrow_c_data",
  "ownership" => "borrowed",
  "metadata" => {"dtype" => 4},
  "buffer" => "descriptor-buffer",
  "released" => false
})
raise "descriptor metadata shadowed remote field: #{proxy.metadata.inspect}" unless proxy.metadata == {"remote" => true}
raise "descriptor buffer shadowed remote field: #{proxy.buffer.inspect}" unless proxy.buffer == "remote-buffer"
raise "fetch metadata shadowed remote field" unless proxy.fetch("metadata") == {"remote" => true}
raise "fetch buffer shadowed remote field" unless proxy.fetch("buffer") == "remote-buffer"
if proxy.respond_to?(:format)
  raise "descriptor format should not be reported as a local method"
end
requested = OmniVM.requests.select { |req| req["op"] == "handle_get" }.map { |req| req["key"] }
raise "metadata was not fetched remotely: #{requested.inspect}" unless requested.include?("metadata")
raise "buffer was not fetched remotely: #{requested.inspect}" unless requested.include?("buffer")
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby table descriptor collision check failed: %v\n%s", err, out)
	}
}

func TestRubyLocalStreamClosesOnEarlyBreak(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    raise "local early break should not cancel remote stream" if req["op"] == "stream_cancel"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_retain"
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
stream = __omnivm_materialize_capture({"__omnivm_stream__" => true, "id" => 77, "runtime" => "ruby", "kind" => "stream", "values" => ["a", "b"]})
seen = []
stream.each do |item|
  seen << item
  break
end
raise "first item mismatch: #{seen.inspect}" unless seen == ["a"]
raise "stream was not marked closed" unless stream.instance_variable_get(:@__omnivm_closed) == true
raise "close was not idempotent" unless stream.close == false
raise "stream handle was not retained" unless OmniVM.requests.any? { |req| req["op"] == "handle_retain" && req["id"] == 77 }
raise "local stream called remote cancel" if OmniVM.requests.any? { |req| req["op"] == "stream_cancel" }
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby local stream early-break lifecycle check failed: %v\n%s", err, out)
	}
}

func TestRubyRemoteStreamCancelsOnEarlyBreak(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_retain"
    return JSON.generate({"__omnivm_result__" => true, "value" => {"done" => false, "value" => "a"}}) if req["op"] == "stream_next"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "stream_cancel"
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
stream = __omnivm_materialize_capture({"__omnivm_stream__" => true, "id" => 88, "runtime" => "ruby", "kind" => "stream"})
seen = []
stream.each do |item|
  seen << item
  break
end
raise "first item mismatch: #{seen.inspect}" unless seen == ["a"]
raise "stream was not marked closed" unless stream.instance_variable_get(:@__omnivm_closed) == true
raise "close was not idempotent" unless stream.close == false
raise "stream handle was not retained" unless OmniVM.requests.any? { |req| req["op"] == "handle_retain" && req["id"] == 88 }
cancels = OmniVM.requests.select { |req| req["op"] == "stream_cancel" }
raise "stream cancel requests mismatch: #{cancels.inspect}" unless cancels.length == 1 && cancels[0]["id"] == 88
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby remote stream early-break lifecycle check failed: %v\n%s", err, out)
	}
}

func TestRubyRemoteStreamCancelsOnChunkMaterializationError(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_retain"
    return JSON.generate({"__omnivm_result__" => true, "value" => {"done" => false, "value" => {"bad" => true}}}) if req["op"] == "stream_next"
    raise "cancel failed" if req["op"] == "stream_cancel"
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
def __omnivm_stream_chunk_value(_value)
  raise "chunk boom"
end
stream = __omnivm_materialize_capture({"__omnivm_stream__" => true, "id" => 89, "runtime" => "ruby", "kind" => "stream"})
begin
  stream.each { |_item| }
rescue => e
  raise unless e.message.include?("chunk boom")
  cleanup_errors = OmniVM.cleanup_errors(e)
  raise "cleanup error was not retained: #{cleanup_errors.inspect}" unless cleanup_errors.length == 1 && cleanup_errors.first.message == "cancel failed"
  cleanup_errors.clear
  raise "cleanup_errors returned internal storage" unless OmniVM.cleanup_errors(e).first.message == "cancel failed"
else
  raise "chunk materialization error was not raised"
end
raise "stream was not marked closed" unless stream.instance_variable_get(:@__omnivm_closed) == true
raise "close was not idempotent" unless stream.close == false
raise "stream handle was not retained" unless OmniVM.requests.any? { |req| req["op"] == "handle_retain" && req["id"] == 89 }
cancels = OmniVM.requests.select { |req| req["op"] == "stream_cancel" }
raise "stream cancel requests mismatch: #{cancels.inspect}" unless cancels.length == 1 && cancels[0]["id"] == 89
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby remote stream materialization-error lifecycle check failed: %v\n%s", err, out)
	}
}

func TestRubyRemoteStreamRejectsMalformedNextChunk(t *testing.T) {
	ruby, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not available")
	}
	code := injectRubyCaptures(nil)
	script := `
require 'json'
class OmniVM
  @@requests = []
  def self.requests
    @@requests
  end
  def self.call(runtime, payload)
    raise "unexpected runtime #{runtime}" unless runtime == "__manifest"
    req = JSON.parse(payload)
    @@requests << req
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "handle_retain"
    return JSON.generate({"__omnivm_result__" => true, "value" => ""}) if req["op"] == "stream_next"
    return JSON.generate({"__omnivm_result__" => true, "value" => true}) if req["op"] == "stream_cancel"
    raise "unexpected manifest op #{req["op"]}"
  end
end
` + code + `
stream = __omnivm_materialize_capture({"__omnivm_stream__" => true, "id" => 90, "runtime" => "ruby", "kind" => "stream"})
begin
  stream.each { |_item| }
  raise "malformed stream chunk was treated as a value or EOF"
rescue => e
  raise unless e.message.include?("stream_next returned malformed chunk")
  raise "malformed chunk runtime mismatch: #{e.runtime.inspect}" unless e.respond_to?(:runtime) && e.runtime == "ruby"
  raise "malformed chunk boundary mismatch: #{e.boundary_path.inspect}" unless e.respond_to?(:boundary_path) && e.boundary_path == "stream_next"
  e.originRuntime = "owner-ruby"
  e.boundaryPath = "stream_next > normalized"
  e.originalErrorHandle = "rb-stream-90"
  e.stackFrames = ["normalized stack"]
  e.causeChain = [{"runtime" => "ruby", "message" => "inner"}]
  e.traceback = "normalized traceback"
  details = e.details
  raise "malformed chunk details mismatch: #{details.inspect}" unless details == {"stream" => {"id" => 90, "chunk" => ""}}
  details["stream"]["id"] = -1
  raise "malformed chunk details reader leaked mutable state" unless e.details == {"stream" => {"id" => 90, "chunk" => ""}}
  e.details_json = '{"stream":{"id":91,"chunk":"json"}}'
  raise "details_json setter did not update details: #{e.details.inspect}" unless e.details == {"stream" => {"id" => 91, "chunk" => "json"}}
  e.detailsJson = {"stream" => {"id" => 92, "chunk" => "alias"}}
  raise "detailsJson setter did not update details_json: #{e.details_json.inspect}" unless JSON.parse(e.details_json) == {"stream" => {"id" => 92, "chunk" => "alias"}}
  e.details = {"stream" => {"id" => 93, "chunk" => "details"}}
  raise "details setter did not update details_json: #{e.details_json.inspect}" unless JSON.parse(e.details_json) == {"stream" => {"id" => 93, "chunk" => "details"}}
  envelope = JSON.parse(e.to_json)
  raise "malformed chunk json origin mismatch: #{envelope.inspect}" unless envelope["origin_runtime"] == "owner-ruby"
  raise "malformed chunk json boundary mismatch: #{envelope.inspect}" unless envelope["boundary_path"] == "stream_next > normalized"
  raise "malformed chunk json handle mismatch: #{envelope.inspect}" unless envelope["original_error_handle"] == "rb-stream-90"
  raise "malformed chunk json traceback mismatch: #{envelope.inspect}" unless envelope["traceback"] == "normalized traceback"
  raise "malformed chunk json stack mismatch: #{envelope.inspect}" unless envelope["stack_frames"] == ["normalized stack"]
  raise "malformed chunk json cause mismatch: #{envelope.inspect}" unless envelope["cause_chain"] == [{"runtime" => "ruby", "message" => "inner"}]
  raise "malformed chunk json details mismatch: #{envelope.inspect}" unless envelope["details"] == {"stream" => {"id" => 93, "chunk" => "details"}}
end
raise "stream was not marked closed" unless stream.instance_variable_get(:@__omnivm_closed) == true
raise "close was not idempotent" unless stream.close == false
raise "stream handle was not retained" unless OmniVM.requests.any? { |req| req["op"] == "handle_retain" && req["id"] == 90 }
cancels = OmniVM.requests.select { |req| req["op"] == "stream_cancel" }
raise "stream cancel requests mismatch: #{cancels.inspect}" unless cancels.length == 1 && cancels[0]["id"] == 90
`
	out, err := exec.Command(ruby, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("ruby remote stream malformed-chunk check failed: %v\n%s", err, out)
	}
}

func TestInjectJavaCapturesUsesManifestCaptureStore(t *testing.T) {
	code := injectJavaCaptures(map[string]string{
		"req": `{"__omnivm_resource__":true,"id":7,"runtime":"java","kind":"request","closed":false}`,
	})
	if !contains(code, `omnivm.OmniVM.setCapture("req",`) {
		t.Fatalf("Java capture should use OmniVM capture store, got %q", code)
	}
	if !contains(code, `__omnivm_resource__`) {
		t.Fatalf("Java capture should preserve handle descriptor JSON, got %q", code)
	}
}

func TestJavaRuntimeAdoptsReturnedTransferHandles(t *testing.T) {
	var data []byte
	var err error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	code := string(data)
	if !contains(code, `\"op\":\"handle_adopt\"`) || !contains(code, "private static boolean adopt(Object id)") {
		t.Fatalf("Java runtime should expose internal handle adoption for returned proxies")
	}
	if !contains(code, `Boolean.TRUE.equals(value.get("transfer"))`) ||
		!contains(code, "adopt(value.get(\"id\"))") ||
		!contains(code, "if (id != null && Boolean.TRUE.equals(value.get(\"transfer\")))") ||
		!contains(code, "HandleProxy.adopt(id)") {
		t.Fatalf("Java runtime should adopt transfer handles for handle and stream proxies")
	}
	if !contains(code, "public static List<Object> proxyIter") || !contains(code, "public static List<Object> proxyKeys") || !contains(code, "public static List<Object> proxyValues") || !contains(code, "public static List<Object> proxyItems") || !contains(code, "public static boolean proxyContains") || !contains(code, "public static boolean proxyClose") || !contains(code, "public static boolean omnivmClose") || !contains(code, "return proxyClose(target);") || !contains(code, "public static boolean proxyCallable") {
		t.Fatalf("Java runtime should expose explicit proxy iter/key/value/item/contains/close/callable helpers plus an omnivmClose alias")
	}
	for _, want := range []string{
		"public static Map<String, Object> ownerDispatchStatus()",
		"public static Map<String, Object> ownerDispatchTargetStatus(String target)",
		"public static boolean assertOwnerDispatchSupported(String label)",
		"public static boolean assertOwnerDispatchTargetSupported(String target, String label)",
		"public static Map<String, Object> rubyThreadingStatus()",
		"public static boolean assertRubyNativeThreadsSupported(String label)",
		`"owner_dispatch_supported", false`,
		`"native_threads_supported", false`,
		`"javascript_event_loop"`,
		`"python_async_loop"`,
		`"nodejs"`,
		`"event_loop"`,
		`"fiber"`,
		`"thread"`,
		`"python_async_stream_pull"`,
		`"ruby_threading"`,
		`"owner_dispatch_target"`,
		"private static RuntimeError runtimeError(String message, String boundaryPath, Object details)",
		`parsed.runtime = "java"`,
		"parsed.detailsJson = jsonValue(RuntimeError.copyJsonValue(details))",
	} {
		if !contains(code, want) {
			t.Fatalf("Java runtime should expose owner-dispatch diagnostic guards, missing %q", want)
		}
	}
	if !contains(code, "import java.util.concurrent.atomic.AtomicBoolean;") ||
		!contains(code, "import java.util.concurrent.ArrayBlockingQueue;") ||
		!contains(code, "import java.util.stream.Stream;") ||
		!contains(code, "import java.util.stream.StreamSupport;") ||
		!contains(code, "public static final class HandleProxy extends AbstractMap<String, Object> implements AutoCloseable") ||
		!contains(code, "public static final class StreamProxy implements Iterable<Object>, AutoCloseable") ||
		!contains(code, "public Stream<Object> stream()") ||
		!contains(code, "return StreamSupport.stream(spliterator(), false).onClose(this::close);") ||
		!contains(code, "try (Stream<Object> items = stream())") ||
		!contains(code, "items.forEach(out::add);") ||
		!contains(code, "return proxy.releaseExplicit();") ||
		!contains(code, "return proxy.cancel();") ||
		!contains(code, `"op\":\"handle_release_explicit\"`) ||
		!contains(code, "if (cleanable != null) {\n                cleanable.clean();\n            }\n            return true;") ||
		!contains(code, "public boolean releaseExplicit()") ||
		!contains(code, "public void close()") ||
		!contains(code, "java.lang.reflect.Modifier.isPublic(method.getModifiers())") ||
		!contains(code, "Object result = invokeProxyMethod(method, target);") ||
		!contains(code, `invokePublicProxyLifecycleMethod(target, "close")`) ||
		!contains(code, `invokePublicProxyLifecycleMethod(target, "dispose")`) ||
		!contains(code, "return !(disposeResult instanceof Boolean) || Boolean.TRUE.equals(disposeResult);") ||
		!contains(code, "private boolean markReleased()") ||
		!contains(code, "released.compareAndSet(false, true)") ||
		!contains(code, "new FinalizerState(value.get(\"id\"), released)") {
		t.Fatalf("Java proxyClose should use explicit release markers while keeping Cleaner cleanup idempotent")
	}
	if !contains(code, "if (id == null || !released.compareAndSet(false, true))") ||
		!contains(code, "released.set(false);\n                throw err;") ||
		!contains(code, "if (!Boolean.TRUE.equals(result)) {\n                released.set(false);\n                return false;\n            }") ||
		strings.Index(code, "if (id == null || !released.compareAndSet(false, true))") > strings.Index(code, `bridgeManifestOp("{\"op\":\"handle_release_explicit\"`) {
		t.Fatalf("Java explicit close should claim released before owner calls and reset it only after failed calls")
	}
	if !contains(code, "public String toString()") ||
		!contains(code, `if (hasLocalValue("toString")) {`) ||
		!contains(code, `return String.valueOf(localValue("toString"));`) ||
		!contains(code, `return String.valueOf(bridgeGet("toString"));`) ||
		!contains(code, "if (!isMissingBridgeError(err)) {\n                    throw err;\n                }") {
		t.Fatalf("Java HandleProxy.toString should prefer materialized then remote toString fields with missing-bridge fallback")
	}
	if !contains(code, `catch (RuntimeException err)`) ||
		!contains(code, `result = bridgeManifestOp("{\"op\":\"stream_next\"`) ||
		!contains(code, "private void cancelAfterLoadFailure(RuntimeException err)") ||
		!contains(code, "if (!StreamProxy.this.cancel()) {\n                    markReleased();\n                }") ||
		!contains(code, "OmniVM stream_next returned malformed chunk for handle") ||
		!contains(code, `"stream_next"`) ||
		!contains(code, `ownerDispatchMap("stream", ownerDispatchMap("id", id, "chunk", result))`) ||
		!contains(code, "err.addSuppressed(closeErr);") ||
		!contains(code, "throw err;") {
		t.Fatalf("Java stream proxy should cancel remote streams after terminal owner stream errors and malformed chunks")
	}
	streamClass := strings.Index(code, "public static final class StreamProxy")
	streamRelease := -1
	streamCancel := -1
	if streamClass >= 0 {
		streamBody := code[streamClass:]
		streamRelease = strings.Index(streamBody, "public boolean releaseExplicit() {\n            return cancel();\n        }")
		streamCancel = strings.Index(streamBody, "public boolean cancel()")
	}
	if streamClass < 0 || streamRelease < 0 || streamCancel < 0 || streamRelease > streamCancel {
		t.Fatalf("Java stream proxy releaseExplicit should use stream cancellation, not generic handle release")
	}
	if !contains(code, "private final List<?> localValues;") ||
		!contains(code, "this.localValues = values instanceof List<?> ? (List<?>) values : null;") ||
		!contains(code, "public void releaseFromFinalizer() {\n            if (cleanable != null) {\n                cleanable.clean();\n            } else {\n                markReleased();\n            }\n        }") ||
		!contains(code, "if (localValues != null) {\n                return markReleased();\n            }") ||
		!contains(code, "if (localValues != null) {\n                        if (released.get() || localIndex >= localValues.size())") ||
		!contains(code, "next = materializeCapture(localValues.get(localIndex++));") ||
		!contains(code, "if (cleanable != null) {\n                cleanable.clean();\n            }") {
		t.Fatalf("Java stream proxy should consume embedded local stream values without manifest next/cancel calls")
	}
	if !contains(code, "if (closed) {\n                subscription.cancel();\n                subscribed.countDown();\n                return;\n            }") {
		t.Fatalf("Java Flow.Publisher iterator should cancel subscriptions that arrive after close")
	}
	for _, want := range []string{
		"private final BlockingQueue<Object> queue = new ArrayBlockingQueue<>(2);",
		"private final AtomicLong requested = new AtomicLong(0);",
		"private final AtomicBoolean terminalSignalled = new AtomicBoolean(false);",
		"private final AtomicBoolean closeSignalled = new AtomicBoolean(false);",
		"if (!claimRequested())",
		`failProtocol(new IllegalStateException("Flow.Publisher emitted without demand"))`,
		`failProtocol(new IllegalStateException("Flow.Publisher exceeded OmniVM stream backpressure buffer"))`,
		"private void signalDone(Throwable failure, boolean discardPending)",
		"signalDone(failure, true);",
		"if (!closeSignalled.compareAndSet(false, true))",
		"item = null;\n            loaded = false;\n            finished = true;",
		"subscribed.countDown();",
	} {
		if !contains(code, want) {
			t.Fatalf("Java Flow.Publisher iterator should enforce bounded backpressure, missing %q", want)
		}
	}
	if contains(code, "new LinkedBlockingQueue<>") {
		t.Fatalf("Java Flow.Publisher iterator should not use an unbounded stream queue")
	}
	if !contains(code, "private boolean isDescriptorValue()") ||
		!contains(code, `Boolean.TRUE.equals(value.get("__omnivm_table__"))`) ||
		!contains(code, `Boolean.TRUE.equals(value.get("__omnivm_job__"))`) ||
		!contains(code, `"format".equals(text)`) ||
		!contains(code, `"metadata".equals(text)`) ||
		!contains(code, `"buffer".equals(text)`) ||
		!contains(code, `"released".equals(text)`) ||
		!contains(code, `"done".equals(text)`) ||
		!contains(code, `"cancelled".equals(text)`) ||
		!contains(code, `"cancelReason".equals(text)`) ||
		!contains(code, `"payload".equals(text)`) ||
		!contains(code, `"result".equals(text)`) {
		t.Fatalf("Java HandleProxy should keep resource/table/job descriptor metadata out of user-visible map fields")
	}
	if !contains(code, "private final String originRuntime;") ||
		!contains(code, "public String getOriginRuntime()") ||
		!contains(code, `out.put("origin_runtime", originRuntime)`) ||
		!contains(code, "ParsedRuntimeError envelope = parseStructuredErrorEnvelope") ||
		!contains(code, `parsed.originRuntime = nonEmptyJsonString(jsonValue(envelope, "origin_runtime", "originRuntime"), parsed.runtime)`) ||
		!contains(code, `parsed.type = jsonString(jsonValue(envelope, "type", "name"))`) ||
		!contains(code, `parsed.traceback = jsonString(jsonValue(envelope, "traceback", "stack"))`) ||
		!contains(code, `parsed.stackFrames = stringListJsonValue(jsonValue(envelope, "stack_frames", "stackFrames"), parseStackFrames(parsed.traceback))`) ||
		!contains(code, `parsed.causeChain = causeChainJsonValue(jsonValue(envelope, "cause_chain", "causeChain"), parsed.runtime)`) ||
		!contains(code, `parsed.causeChain = parseCauseChain(text, parsed.runtime)`) ||
		!contains(code, "String wrappedBoundary = parsed.boundaryPath") ||
		!contains(code, `wrappedBoundary = String.join(" > ", boundaryParts)`) ||
		!contains(code, "envelope = parseStructuredErrorEnvelope(text, parsed.runtime, wrappedBoundary)") {
		t.Fatalf("Java runtime error envelope should preserve structured origin_runtime and accept JS camelCase fields")
	}
	if contains(code, `wrappedBoundary = String.join(" -> ", boundaryParts)`) {
		t.Fatalf("Java runtime error envelope should use normalized boundary separator")
	}
	if !contains(code, "public List<String> getStackFrames()") || !contains(code, `out.put("stack_frames", new ArrayList<>(stackFrames))`) {
		t.Fatalf("Java runtime error envelope should expose normalized stack frames")
	}
	for _, want := range []string{
		"private final Object details;",
		"this.details = copyJsonValue(parseDetailsJson(parsed.detailsJson));",
		"public Object getDetails()",
		`out.put("details", copyJsonValue(details))`,
		`out.put("details_json", detailsJson)`,
		"public String toJson()",
		"return jsonValue(toMap());",
		"private static Object parseDetailsJson(String detailsJson)",
		"return detailsJson;",
		"private static Object copyJsonValue(Object value)",
		"private static List<Map<String, Object>> copyCauseChain(List<Map<String, Object>> causes)",
		"return copyCauseChain(causeChain);",
		`out.put("cause_chain", copyJsonValue(causeChain))`,
		"private static ParsedRuntimeError parseStructuredErrorEnvelope",
		`parsed.detailsJson = detailsJsonValue(envelope)`,
		"private static String detailsJsonValue(Map<?, ?> envelope)",
		`Object rawDetails = jsonValue(envelope, "details_json", "detailsJson")`,
		"private static Object detailsObjectValue(Map<?, ?> source)",
		"private static List<String> stringListJsonValue",
		"private static List<Map<String, Object>> causeChainJsonValue(Object value, String fallbackRuntime)",
		`private static Object jsonValue(Map<?, ?> value, String preferredKey, String fallbackKey)`,
		`entry.put("type", jsonString(jsonValue(cause, "type", "name")))`,
		`String traceback = jsonString(jsonValue(cause, "traceback", "stack"))`,
		`entry.put("traceback", traceback)`,
		`entry.put("stack_frames", stackFrames)`,
		`String defaultRuntime = safeString(fallbackRuntime)`,
		`String runtime = nonEmptyJsonString(cause.get("runtime"), defaultRuntime)`,
		`String originRuntime = jsonString(jsonValue(cause, "origin_runtime", "originRuntime"))`,
		`originRuntime = runtime`,
		`entry.put("runtime", runtime)`,
		`entry.put("origin_runtime", runtime)`,
		`String boundaryPath = jsonString(jsonValue(cause, "boundary_path", "boundaryPath"))`,
		`String originalErrorHandle = jsonString(jsonValue(cause, "original_error_handle", "originalErrorHandle"))`,
		`Object causeDetails = detailsObjectValue(cause)`,
		`entry.put("details", causeDetails)`,
		`String causeDetailsJson = detailsJsonValue(cause)`,
		`entry.put("details_json", causeDetailsJson)`,
	} {
		if !contains(code, want) {
			t.Fatalf("Java runtime error envelope should expose copied structured details, missing %q", want)
		}
	}

	runnerData, err := os.ReadFile("../../runtime/java/OmniVMRunner.java")
	if err != nil {
		t.Fatalf("read Java runner helper: %v", err)
	}
	runnerCode := string(runnerData)
	for _, want := range []string{
		"Throwable cause = throwable.getCause();",
		`details.put("cause", throwableSummary(cause, 0));`,
		"Throwable[] suppressed = throwable.getSuppressed();",
		`details.put("suppressed", items);`,
		`details.put("suppressed_truncated", true);`,
		"private static Map<String, Object> throwableSummary(Throwable throwable, int depth)",
		`out.put("type", throwable.getClass().getName());`,
		`out.put("message", String.valueOf(throwable.getMessage()));`,
		`out.put("stack_frames", frames);`,
		"if (depth >= 4)",
		"private static String originalErrorHandleFromMethod(Throwable throwable, String name)",
		"current.getDeclaredMethod(name)",
		"method.setAccessible(true)",
	} {
		if !contains(runnerCode, want) {
			t.Fatalf("Java runner throwable details should preserve cause/suppressed cleanup failures, missing %q", want)
		}
	}
}

func TestJavaProxyCloseRuntimePreservesLocalCloseResult(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}

	tmp := t.TempDir()
	checkPath := tmp + "/ProxyCloseCheck.java"
	check := `package omnivm;

public final class ProxyCloseCheck {
    public static final class FalseClose {
        public int calls = 0;
        public Boolean close() {
            calls++;
            return Boolean.FALSE;
        }
    }

    public static final class TextClose {
        public String close() {
            return "closed";
        }
    }

    public static final class VoidClose implements AutoCloseable {
        public int calls = 0;
        @Override
        public void close() {
            calls++;
        }
    }

    public static final class PrivateClose {
        private boolean closed = false;
        private Boolean close() {
            closed = true;
            return Boolean.TRUE;
        }
    }

    public static final class TextDispose {
        public String dispose() {
            return "disposed";
        }
    }

    public static final class FalseDispose {
        public Boolean dispose() {
            return Boolean.FALSE;
        }
    }

    public static final class VoidDispose {
        public int calls = 0;
        public void dispose() {
            calls++;
        }
    }

    public static final class PrivateDispose {
        private boolean disposed = false;
        private Boolean dispose() {
            disposed = true;
            return Boolean.TRUE;
        }
    }

    public static final class CloseAndDispose {
        public String close() {
            return "closed";
        }

        public Boolean dispose() {
            throw new AssertionError("dispose should not run when close is available");
        }
    }

    public static final class RequiredCloseAndDispose {
        public Boolean close(String reason) {
            throw new AssertionError("required-arg close should not run");
        }

        public String dispose() {
            return "disposed-after-required-close";
        }
    }

    public static final class RequiredOnlyClose {
        public Boolean close(String reason) {
            throw new AssertionError("required-arg close should not run");
        }
    }

    public static final class NoClose {
    }

    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static void main(String[] args) {
        FalseClose falseClose = new FalseClose();
        require(!OmniVM.proxyClose(falseClose), "false close result was not preserved");
        require(falseClose.calls == 1, "false close call count mismatch");

        require(OmniVM.proxyClose(new TextClose()), "truthy non-boolean close result should normalize true");
        require(OmniVM.omnivmClose(new TextClose()), "omnivmClose alias should match proxyClose");

        VoidClose voidClose = new VoidClose();
        require(OmniVM.proxyClose(voidClose), "AutoCloseable close should normalize true");
        require(voidClose.calls == 1, "AutoCloseable close call count mismatch");

        PrivateClose privateClose = new PrivateClose();
        require(!OmniVM.proxyClose(privateClose), "private close should not be invoked as a lifecycle hook");
        require(!privateClose.closed, "private close was invoked");

        require(OmniVM.proxyClose(new TextDispose()), "truthy non-boolean dispose result should normalize true");
        require(!OmniVM.proxyClose(new FalseDispose()), "false dispose result was not preserved");

        VoidDispose voidDispose = new VoidDispose();
        require(OmniVM.proxyClose(voidDispose), "void dispose should normalize true");
        require(voidDispose.calls == 1, "void dispose call count mismatch");

        PrivateDispose privateDispose = new PrivateDispose();
        require(!OmniVM.proxyClose(privateDispose), "private dispose should not be invoked as a lifecycle hook");
        require(!privateDispose.disposed, "private dispose was invoked");

        require(OmniVM.proxyClose(new CloseAndDispose()), "close should take priority over dispose");
        require(OmniVM.proxyClose(new RequiredCloseAndDispose()), "required-arg close should be skipped for dispose");
        require(!OmniVM.proxyClose(new RequiredOnlyClose()), "required-arg close should not be treated as lifecycle close");

        require(!OmniVM.proxyClose(new NoClose()), "object without close should return false");
        require(!OmniVM.proxyClose(null), "null close should return false");
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java proxy close check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, javaRuntimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java proxy close check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.ProxyCloseCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java proxy close check: %v\n%s", err, out)
	}
}

func TestJavaScopedBufferOwnerReleasesAndSuppressesCleanupFailure(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}
	runtimeData, err := os.ReadFile(javaRuntimePath)
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	runtimeCode := strings.Replace(string(runtimeData), "public class OmniVM {\n", `public class OmniVM {
    public static int setBufferCalls = 0;
    public static int releaseBufferCalls = 0;
    public static String lastSetName = "";
    public static int lastSetDtype = -1;
    public static boolean failRelease = false;
    public static boolean failReleaseWithTombstone = false;
`, 1)
	runtimeCode = strings.Replace(runtimeCode, `    public static void setBuffer(String name, byte[] data, int dtype) {
        nativeSetBuffer(name, data, dtype);
    }`, `    public static void setBuffer(String name, byte[] data, int dtype) {
        setBufferCalls++;
        lastSetName = String.valueOf(name);
        lastSetDtype = dtype;
    }`, 1)
	runtimeCode = strings.Replace(runtimeCode, `    public static void releaseBuffer(String name) {
        nativeReleaseBuffer(name);
    }`, `    public static void releaseBuffer(String name) {
        releaseBufferCalls++;
        if (failReleaseWithTombstone) {
            throw runtimeError(
                "release failed for " + name,
                "native_memory",
                ownerDispatchMap(
                    "buffer",
                    ownerDispatchMap(
                        "name", name,
                        "state", "released_detached",
                        "released", true,
                        "release_error", "producer release failed")));
        }
        if (failRelease) {
            throw new RuntimeException("release failed for " + name);
        }
    }`, 1)
	runtimeCode = strings.Replace(runtimeCode, `    public static String bufferStatus(String name) {
        return nativeBufferStatus(name);
    }`, `    public static String bufferStatus(String name) {
        return "{\"name\":\"" + name + "\",\"lease_state\":\"owned\"}";
    }`, 1)

	tmp := t.TempDir()
	runtimePath := tmp + "/OmniVM.java"
	if err := os.WriteFile(runtimePath, []byte(runtimeCode), 0644); err != nil {
		t.Fatalf("write Java runtime helper copy: %v", err)
	}
	checkPath := tmp + "/ScopedBufferOwnerCheck.java"
	check := `package omnivm;

public final class ScopedBufferOwnerCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static void main(String[] args) {
        String result = OmniVM.bufferOwner("payload", new byte[] {1, 2, 3}, 7, owner -> {
            require("payload".equals(owner.name()), "owner name mismatch");
            require(!owner.isReleased(), "owner released before callback returned");
            require(owner.status().contains("\"lease_state\":\"owned\""), "status mismatch: " + owner.status());
            return owner.name() + "-done";
        });
        require("payload-done".equals(result), "scoped result mismatch: " + result);
        require(OmniVM.setBufferCalls == 1, "setBuffer calls mismatch: " + OmniVM.setBufferCalls);
        require("payload".equals(OmniVM.lastSetName), "last set name mismatch: " + OmniVM.lastSetName);
        require(OmniVM.lastSetDtype == 7, "last set dtype mismatch: " + OmniVM.lastSetDtype);
        require(OmniVM.releaseBufferCalls == 1, "release calls mismatch after success: " + OmniVM.releaseBufferCalls);

        int setBeforeDirect = OmniVM.setBufferCalls;
        int releaseBeforeDirect = OmniVM.releaseBufferCalls;
        OmniVM.BufferOwner direct = OmniVM.bufferOwner("direct", new byte[] {4, 5}, 9);
        require(OmniVM.setBufferCalls == setBeforeDirect + 1, "direct setBuffer mismatch: " + OmniVM.setBufferCalls);
        try {
            direct.enter();
            throw new AssertionError("active owner re-entry did not fail");
        } catch (OmniVM.RuntimeError err) {
            require(err.getMessage().contains("is already active"), "active re-entry message mismatch: " + err.getMessage());
            require("native_memory".equals(err.getBoundaryPath()), "active re-entry boundary mismatch: " + err.getBoundaryPath());
            require(String.valueOf(err.getDetails()).contains("active_owner=true"), "active re-entry details mismatch: " + err.getDetails());
        }
        require(OmniVM.setBufferCalls == setBeforeDirect + 1, "active re-entry republished buffer: " + OmniVM.setBufferCalls);
        require(OmniVM.releaseBufferCalls == releaseBeforeDirect, "active re-entry released buffer: " + OmniVM.releaseBufferCalls);
        require(direct.release(), "direct release did not return true");
        require(!direct.release(), "direct second release was not idempotent");
        try {
            direct.enter();
            throw new AssertionError("released owner re-entry did not fail");
        } catch (OmniVM.RuntimeError err) {
            require(err.getMessage().contains("cannot be re-entered after release"), "released re-entry message mismatch: " + err.getMessage());
            require("native_memory".equals(err.getBoundaryPath()), "released re-entry boundary mismatch: " + err.getBoundaryPath());
            require(String.valueOf(err.getDetails()).contains("released=true"), "released re-entry details mismatch: " + err.getDetails());
        }
        require(OmniVM.setBufferCalls == setBeforeDirect + 1, "released re-entry republished buffer: " + OmniVM.setBufferCalls);
        require(OmniVM.releaseBufferCalls == releaseBeforeDirect + 1, "released re-entry release mismatch: " + OmniVM.releaseBufferCalls);

        OmniVM.failRelease = true;
        try {
            OmniVM.bufferOwner("failing", owner -> {
                throw new IllegalStateException("body failed");
            });
            throw new AssertionError("body exception was not raised");
        } catch (IllegalStateException err) {
            require("body failed".equals(err.getMessage()), "body exception was masked: " + err);
            Throwable[] suppressed = err.getSuppressed();
            require(suppressed.length == 1, "cleanup failure was not suppressed: " + suppressed.length);
            require(String.valueOf(suppressed[0].getMessage()).contains("release failed for failing"), "suppressed cleanup mismatch: " + suppressed[0]);
        }
        require(OmniVM.releaseBufferCalls == 3, "release calls mismatch after failure: " + OmniVM.releaseBufferCalls);
        OmniVM.failRelease = false;
        OmniVM.failReleaseWithTombstone = true;
        OmniVM.BufferOwner tombstoned = OmniVM.bufferOwner("tombstoned", new byte[] {6}, 1);
        try {
            tombstoned.release();
            throw new AssertionError("tombstone release failure was not raised");
        } catch (OmniVM.RuntimeError err) {
            require("native_memory".equals(err.getBoundaryPath()), "tombstone boundary mismatch: " + err.getBoundaryPath());
            require(String.valueOf(err.getDetails()).contains("released=true"), "tombstone details mismatch: " + err.getDetails());
            require(String.valueOf(err.getDetails()).contains("release_error=producer release failed"), "tombstone release_error missing: " + err.getDetails());
        }
        require(tombstoned.isReleased(), "tombstoned owner did not mark released after release failure");
        require(!tombstoned.release(), "tombstoned owner second release was not idempotent");
        require(OmniVM.releaseBufferCalls == 4, "release calls mismatch after tombstone failure: " + OmniVM.releaseBufferCalls);
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java scoped buffer owner check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, runtimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java scoped buffer owner check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.ScopedBufferOwnerCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java scoped buffer owner check: %v\n%s", err, out)
	}
}

func TestJavaHandleProxyToStringPrefersMaterializedField(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}

	tmp := t.TempDir()
	runtimeData, err := os.ReadFile(javaRuntimePath)
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	runtimeCode := strings.Replace(string(runtimeData),
		`    public static native String nativeCall(String runtime, String code);`,
		`    public static String nativeCall(String runtime, String code) {
        if (!"__manifest".equals(runtime)) {
            throw new RuntimeException("unexpected runtime " + runtime);
        }
        if (code.contains("\"op\":\"handle_get\"") && code.contains("\"key\":\"toString\"")) {
            return "{\"__omnivm_result__\":true,\"value\":\"bridge-to-string\"}";
        }
        throw new RuntimeException("unexpected manifest op " + code);
    }`, 1)
	runtimePath := tmp + "/OmniVM.java"
	if err := os.WriteFile(runtimePath, []byte(runtimeCode), 0644); err != nil {
		t.Fatalf("write Java runtime helper: %v", err)
	}
	checkPath := tmp + "/ProxyToStringCheck.java"
	check := `package omnivm;

public final class ProxyToStringCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static void main(String[] args) {
        Object proxy = OmniVM.materializeJsonCapture("{\"__omnivm_resource__\":true,\"id\":82,\"runtime\":\"python\",\"kind\":\"object\",\"toString\":\"remote-to-string\"}");
        String text = String.valueOf(proxy);
        require("remote-to-string".equals(text), "toString did not prefer local materialized remote field: " + text);
        Object bridged = OmniVM.materializeJsonCapture("{\"__omnivm_resource__\":true,\"id\":83,\"runtime\":\"python\",\"kind\":\"object\"}");
        String bridgedText = String.valueOf(bridged);
        require("bridge-to-string".equals(bridgedText), "toString did not fetch remote field through bridge: " + bridgedText);
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java proxy toString check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, runtimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java proxy toString check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.ProxyToStringCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java proxy toString check: %v\n%s", err, out)
	}
}

func TestJavaFlowPublisherIteratorEnforcesBackpressure(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}

	tmp := t.TempDir()
	checkPath := tmp + "/FlowPublisherBackpressureCheck.java"
	check := `package omnivm;

import java.util.concurrent.Flow;

public final class FlowPublisherBackpressureCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static final class OneAndDonePublisher implements Flow.Publisher<Object> {
        @Override
        public void subscribe(Flow.Subscriber<? super Object> subscriber) {
            subscriber.onSubscribe(new Flow.Subscription() {
                @Override
                public void request(long n) {
                    subscriber.onNext("ok");
                    subscriber.onComplete();
                }

                @Override
                public void cancel() {
                }
            });
        }
    }

    public static final class BurstPublisher implements Flow.Publisher<Object> {
        BurstSubscription subscription;

        @Override
        public void subscribe(Flow.Subscriber<? super Object> subscriber) {
            subscription = new BurstSubscription(subscriber);
            subscriber.onSubscribe(subscription);
        }
    }

    public static final class BurstSubscription implements Flow.Subscription {
        private final Flow.Subscriber<? super Object> subscriber;
        int cancels;

        BurstSubscription(Flow.Subscriber<? super Object> subscriber) {
            this.subscriber = subscriber;
        }

        @Override
        public void request(long n) {
            subscriber.onNext("first");
            subscriber.onNext("overflow");
        }

        @Override
        public void cancel() {
            cancels++;
        }
    }

    public static final class CancellablePublisher implements Flow.Publisher<Object> {
        CancellableSubscription subscription;

        @Override
        public void subscribe(Flow.Subscriber<? super Object> subscriber) {
            subscription = new CancellableSubscription(subscriber);
            subscriber.onSubscribe(subscription);
        }
    }

    public static final class CancellableSubscription implements Flow.Subscription {
        private final Flow.Subscriber<? super Object> subscriber;
        int cancels;

        CancellableSubscription(Flow.Subscriber<? super Object> subscriber) {
            this.subscriber = subscriber;
        }

        @Override
        public void request(long n) {
            subscriber.onNext("loaded");
        }

        @Override
        public void cancel() {
            cancels++;
        }
    }

    public static void main(String[] args) {
        OmniVM.FlowPublisherIterator compliant = new OmniVM.FlowPublisherIterator(new OneAndDonePublisher());
        require(compliant.hasNext(), "compliant publisher first item missing");
        require("ok".equals(compliant.next()), "compliant publisher item mismatch");
        require(!compliant.hasNext(), "compliant publisher terminal signal was lost");

        CancellablePublisher cancellable = new CancellablePublisher();
        OmniVM.FlowPublisherIterator closing = new OmniVM.FlowPublisherIterator(cancellable);
        require(closing.hasNext(), "cancellable publisher first item missing");
        closing.close();
        require(cancellable.subscription.cancels == 1, "close did not cancel exactly once: " + cancellable.subscription.cancels);
        closing.close();
        require(cancellable.subscription.cancels == 1, "second close cancelled again: " + cancellable.subscription.cancels);
        require(!closing.hasNext(), "closed iterator still reported a loaded item");
        try {
            closing.next();
            throw new AssertionError("closed iterator returned an item after close");
        } catch (java.util.NoSuchElementException expected) {
        }

        BurstPublisher burst = new BurstPublisher();
        OmniVM.FlowPublisherIterator iterator = new OmniVM.FlowPublisherIterator(burst);
        try {
            iterator.hasNext();
            throw new AssertionError("overproducing publisher did not fail");
        } catch (RuntimeException err) {
            Throwable cause = err.getCause();
            require(cause instanceof IllegalStateException, "unexpected overproduction cause: " + cause);
            require(String.valueOf(cause.getMessage()).contains("without demand"), "missing demand diagnostic: " + cause.getMessage());
        }
        require(burst.subscription.cancels == 1, "overproducing subscription was not cancelled exactly once: " + burst.subscription.cancels);
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java Flow.Publisher backpressure check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, javaRuntimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java Flow.Publisher backpressure check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.FlowPublisherBackpressureCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java Flow.Publisher backpressure check: %v\n%s", err, out)
	}
}

func TestJavaLocalStreamProxyCloseIsIdempotent(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}

	tmp := t.TempDir()
	checkPath := tmp + "/LocalStreamProxyCloseCheck.java"
	check := `package omnivm;

public final class LocalStreamProxyCloseCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static void main(String[] args) {
        OmniVM.setCapture("rows", "{\"__omnivm_stream__\":true,\"values\":[\"a\",\"b\"]}");
        Object rows = OmniVM.getCapture("rows");
        require(rows instanceof OmniVM.StreamProxy, "capture did not materialize a stream proxy");
        OmniVM.StreamProxy stream = (OmniVM.StreamProxy) rows;
        require(stream.releaseExplicit(), "local stream releaseExplicit should close the stream");
        stream.close();
        require(!stream.iterator().hasNext(), "closed local stream still had items");
        require(!stream.releaseExplicit(), "local stream releaseExplicit should be idempotent false");
        require(!OmniVM.proxyClose(stream), "proxyClose after local stream close should be idempotent false");

        OmniVM.setCapture("again", "{\"__omnivm_stream__\":true,\"values\":[\"c\"]}");
        OmniVM.StreamProxy cleanup = (OmniVM.StreamProxy) OmniVM.getCapture("again");
        cleanup.releaseFromFinalizer();
        require(!cleanup.iterator().hasNext(), "local stream finalizer cleanup left items visible");
        require(!cleanup.releaseExplicit(), "local stream releaseExplicit after finalizer cleanup should be idempotent false");
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java local stream close check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, javaRuntimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java local stream close check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.LocalStreamProxyCloseCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java local stream close check: %v\n%s", err, out)
	}
}

func TestJavaRemoteStreamCancelsOnOwnerReadFailure(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}

	tmp := t.TempDir()
	runtimeData, err := os.ReadFile(javaRuntimePath)
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	runtimeCode := strings.Replace(string(runtimeData),
		`    public static native String nativeCall(String runtime, String code);`,
		`    public static int streamCancelCalls = 0;
    public static String nativeCall(String runtime, String code) {
        if (!"__manifest".equals(runtime)) {
            throw new RuntimeException("unexpected runtime " + runtime);
        }
        if (code.contains("\"op\":\"stream_next\"")) {
            throw new RuntimeException("owner read failed");
        }
        if (code.contains("\"op\":\"stream_cancel\"")) {
            streamCancelCalls++;
            return "{\"__omnivm_result__\":true,\"value\":true}";
        }
        throw new RuntimeException("unexpected manifest op " + code);
    }`, 1)
	runtimePath := tmp + "/OmniVM.java"
	if err := os.WriteFile(runtimePath, []byte(runtimeCode), 0644); err != nil {
		t.Fatalf("write Java runtime helper: %v", err)
	}
	checkPath := tmp + "/RemoteStreamOwnerReadFailureCheck.java"
	check := `package omnivm;

public final class RemoteStreamOwnerReadFailureCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static void main(String[] args) {
        Object rows = OmniVM.materializeJsonCapture("{\"__omnivm_stream__\":true,\"id\":88,\"runtime\":\"python\",\"kind\":\"stream\"}");
        require(rows instanceof OmniVM.StreamProxy, "capture did not materialize a stream proxy");
        OmniVM.StreamProxy stream = (OmniVM.StreamProxy) rows;
        try {
            stream.iterator().hasNext();
            throw new AssertionError("owner read failure was not raised");
        } catch (RuntimeException err) {
            require(String.valueOf(err.getMessage()).contains("owner read failed"), "owner read error was masked: " + err);
        }
        require(OmniVM.streamCancelCalls == 1, "owner read failure did not cancel exactly once: " + OmniVM.streamCancelCalls);
        require(!stream.cancel(), "stream cancel after read failure should be idempotent false");
        require(OmniVM.streamCancelCalls == 1, "second cancel retried owner cancel: " + OmniVM.streamCancelCalls);
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java remote stream read-failure check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, runtimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java remote stream read-failure check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.RemoteStreamOwnerReadFailureCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java remote stream read-failure check: %v\n%s", err, out)
	}
}

func TestJavaRemoteStreamRejectsMalformedNextChunk(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}

	tmp := t.TempDir()
	runtimeData, err := os.ReadFile(javaRuntimePath)
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	runtimeCode := strings.Replace(string(runtimeData),
		`    public static native String nativeCall(String runtime, String code);`,
		`    public static int streamCancelCalls = 0;
    public static String nativeCall(String runtime, String code) {
        if (!"__manifest".equals(runtime)) {
            throw new RuntimeException("unexpected runtime " + runtime);
        }
        if (code.contains("\"op\":\"stream_next\"")) {
            return "{\"__omnivm_result__\":true,\"value\":\"\"}";
        }
        if (code.contains("\"op\":\"stream_cancel\"")) {
            streamCancelCalls++;
            return "{\"__omnivm_result__\":true,\"value\":true}";
        }
        if (code.contains("\"op\":\"handle_retain\"")) {
            return "{\"__omnivm_result__\":true,\"value\":true}";
        }
        throw new RuntimeException("unexpected manifest op " + code);
    }`, 1)
	runtimePath := tmp + "/OmniVM.java"
	if err := os.WriteFile(runtimePath, []byte(runtimeCode), 0644); err != nil {
		t.Fatalf("write Java runtime helper: %v", err)
	}
	checkPath := tmp + "/RemoteStreamMalformedChunkCheck.java"
	check := `package omnivm;

import java.util.Map;

public final class RemoteStreamMalformedChunkCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static void main(String[] args) {
        Object rows = OmniVM.materializeJsonCapture("{\"__omnivm_stream__\":true,\"id\":90,\"runtime\":\"python\",\"kind\":\"stream\"}");
        require(rows instanceof OmniVM.StreamProxy, "capture did not materialize a stream proxy");
        OmniVM.StreamProxy stream = (OmniVM.StreamProxy) rows;
        try {
            stream.iterator().hasNext();
            throw new AssertionError("malformed stream chunk was treated as a value or EOF");
        } catch (OmniVM.RuntimeError err) {
            require(String.valueOf(err.getMessage()).contains("stream_next returned malformed chunk"), "malformed chunk message mismatch: " + err.getMessage());
            require("stream_next".equals(err.getBoundaryPath()), "malformed chunk boundary mismatch: " + err.getBoundaryPath());
            Object details = err.getDetails();
            require(details instanceof Map<?, ?>, "malformed chunk details were not a map: " + details);
            Object streamDetails = ((Map<?, ?>) details).get("stream");
            require(streamDetails instanceof Map<?, ?>, "malformed chunk stream details were not a map: " + streamDetails);
            Object streamId = ((Map<?, ?>) streamDetails).get("id");
            require(streamId instanceof Number && ((Number) streamId).longValue() == 90L, "malformed chunk id mismatch: " + streamDetails);
            require("".equals(((Map<?, ?>) streamDetails).get("chunk")), "malformed chunk value mismatch: " + streamDetails);
            require(String.valueOf(err.toJson()).contains("\"boundary_path\":\"stream_next\""), "malformed chunk JSON missing boundary: " + err.toJson());
        }
        require(OmniVM.streamCancelCalls == 1, "malformed chunk did not cancel exactly once: " + OmniVM.streamCancelCalls);
        require(!stream.cancel(), "stream cancel after malformed chunk should be idempotent false");
        require(OmniVM.streamCancelCalls == 1, "second cancel retried owner cancel: " + OmniVM.streamCancelCalls);
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java remote stream malformed-chunk check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, runtimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java remote stream malformed-chunk check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.RemoteStreamMalformedChunkCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java remote stream malformed-chunk check: %v\n%s", err, out)
	}
}

func TestJavaRuntimeErrorPreservesStructuredEnvelope(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	javaRuntimePath := ""
	var javaRuntimeErr error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		if _, err := os.Stat(path); err == nil {
			javaRuntimePath = path
			break
		} else {
			javaRuntimeErr = err
		}
	}
	if javaRuntimePath == "" {
		t.Fatalf("read Java runtime helper: %v", javaRuntimeErr)
	}

	tmp := t.TempDir()
	checkPath := tmp + "/RuntimeErrorCheck.java"
	check := `package omnivm;

import java.util.List;
import java.util.Map;

public final class RuntimeErrorCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    @SuppressWarnings("unchecked")
    public static void main(String[] args) {
        String payload = """
{
  "runtime": "javascript",
  "originRuntime": "python",
  "name": "AggregateError",
  "message": "invalid",
  "stack": "fallback frame",
  "stackFrames": ["at parse (<anonymous>:1:2)"],
  "causeChain": [{
    "runtime": "java",
    "originRuntime": "ruby",
    "name": "TypeError",
    "message": "inner",
    "stack": "TypeError: inner",
    "stackFrames": ["at cause (<anonymous>:2:4)"],
    "boundaryPath": "call[javascript] > callback[java]",
    "originalErrorHandle": "java-error-3",
    "details": {"code": "E_INNER", "path": ["user", "age"]}
  }],
  "originalErrorHandle": "js-error-7",
  "details": [{"path": ["user", "age"]}]
}
""";
        OmniVM.RuntimeError err = OmniVM.RuntimeError.fromBridge(
            "ERR:execute manifest: call[javascript]: " + payload,
            "go",
            "call[go]",
            new IllegalStateException("bridge cause")
        );

        require("javascript".equals(err.getRuntime()), "runtime mismatch: " + err.getRuntime());
        require("python".equals(err.getOriginRuntime()), "origin runtime mismatch: " + err.getOriginRuntime());
        require("AggregateError".equals(err.getType()), "type mismatch: " + err.getType());
        require("invalid".equals(err.getMessage()), "message mismatch: " + err.getMessage());
        require("execute manifest > call[javascript]".equals(err.getBoundaryPath()), "boundary mismatch: " + err.getBoundaryPath());
        require("js-error-7".equals(err.getOriginalErrorHandle()), "handle mismatch: " + err.getOriginalErrorHandle());
        require(err.getCause() instanceof IllegalStateException, "bridge cause was not preserved");
        require(err.getStackFrames().equals(List.of("at parse (<anonymous>:1:2)")), "stack frames mismatch: " + err.getStackFrames());

        List<Object> details = (List<Object>) err.getDetails();
        ((Map<String, Object>) details.get(0)).put("path", List.of("changed"));
        List<Object> detailsAgain = (List<Object>) err.getDetails();
        require(((Map<String, Object>) detailsAgain.get(0)).get("path").equals(List.of("user", "age")), "details getter leaked mutable state");

        List<Map<String, Object>> causes = err.getCauseChain();
        Map<String, Object> first = causes.get(0);
        require("java".equals(first.get("runtime")), "cause runtime mismatch: " + first);
        require("ruby".equals(first.get("origin_runtime")), "cause origin mismatch: " + first);
        require("call[javascript] > callback[java]".equals(first.get("boundary_path")), "cause boundary mismatch: " + first);
        require("java-error-3".equals(first.get("original_error_handle")), "cause handle mismatch: " + first);
        Map<String, Object> causeDetails = (Map<String, Object>) first.get("details");
        require("E_INNER".equals(causeDetails.get("code")), "cause details mismatch: " + causeDetails);
        causeDetails.put("code", "changed");
        causes.get(0).put("message", "changed");

        Map<String, Object> firstAgain = err.getCauseChain().get(0);
        require("inner".equals(firstAgain.get("message")), "cause-chain getter leaked top-level mutation");
        require("E_INNER".equals(((Map<String, Object>) firstAgain.get("details")).get("code")), "cause-chain getter leaked nested details mutation");

        Map<String, Object> envelope = err.toMap();
        ((Map<String, Object>) ((List<Map<String, Object>>) envelope.get("cause_chain")).get(0).get("details")).put("code", "changed-again");
        require("E_INNER".equals(((Map<String, Object>) err.getCauseChain().get(0).get("details")).get("code")), "toMap leaked nested cause details mutation");
        String json = err.toJson();
        require(json.contains("\"origin_runtime\":\"python\""), "toJson missing origin runtime: " + json);
        require(json.contains("\"cause_chain\":[{\"type\":\"TypeError\""), "toJson missing cause chain: " + json);
        require(json.contains("\"details_json\":\"[{\\\"path\\\":[\\\"user\\\",\\\"age\\\"]}]\""), "toJson missing raw details_json: " + json);
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java runtime error check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, javaRuntimePath, checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java runtime error check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.RuntimeErrorCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java runtime error check: %v\n%s", err, out)
	}
}

func TestJavaRunnerThrowableDetailsPreserveSuppressedExceptions(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	tmp := t.TempDir()
	checkPath := tmp + "/JavaThrowableDetailsCheck.java"
	check := `package omnivm;

import java.io.IOException;
import java.lang.reflect.Method;

public final class JavaThrowableDetailsCheck {
    static class BaseHandledException extends RuntimeException {
        BaseHandledException(String message) {
            super(message);
        }

        private String originalErrorHandle() {
            return "base-handle";
        }
    }

    static final class HandledException extends BaseHandledException {
        HandledException(String message) {
            super(message);
        }
    }

    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    public static void main(String[] args) throws Exception {
        RuntimeException root = new RuntimeException("outer");
        root.initCause(new IOException("disk"));
        root.addSuppressed(new IllegalStateException("closing"));

        Method format = OmniVMRunner.class.getDeclaredMethod("formatThrowable", Throwable.class);
        format.setAccessible(true);
        String text = String.valueOf(format.invoke(null, root));

        require(text.contains("Details: "), "formatted throwable omitted details: " + text);
        require(text.contains("\"cause\":{\"type\":\"java.io.IOException\",\"message\":\"disk\""), "cause details missing: " + text);
        require(text.contains("\"suppressed\":[{\"type\":\"java.lang.IllegalStateException\",\"message\":\"closing\""), "suppressed details missing: " + text);
        require(text.contains("\"stack_frames\":["), "stack frames missing: " + text);

        String handled = String.valueOf(format.invoke(null, new HandledException("handled")));
        require(handled.contains("Original error handle: base-handle"), "private inherited original handle missing: " + handled);
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java throwable details check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, "../../runtime/java/OmniVM.java", "../../runtime/java/OmniVMRunner.java", checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java throwable details check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.JavaThrowableDetailsCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java throwable details check: %v\n%s", err, out)
	}
}

func TestJavaOwnerDispatchGuardsReportDiagnosticOnly(t *testing.T) {
	javac, err := exec.LookPath("javac")
	if err != nil {
		t.Skip("javac not available")
	}
	java, err := exec.LookPath("java")
	if err != nil {
		t.Skip("java not available")
	}

	tmp := t.TempDir()
	checkPath := tmp + "/OwnerDispatchCheck.java"
	check := `package omnivm;

import java.util.Map;

public final class OwnerDispatchCheck {
    private static void require(boolean ok, String message) {
        if (!ok) {
            throw new AssertionError(message);
        }
    }

    @SuppressWarnings("unchecked")
    public static void main(String[] args) {
        Map<String, Object> status = OmniVM.ownerDispatchStatus();
        require("diagnostic_only".equals(status.get("mode")), "bad mode: " + status);
        require(Boolean.FALSE.equals(status.get("owner_dispatch_supported")), "owner dispatch should be unsupported");
        Map<String, Object> targets = (Map<String, Object>) status.get("owner_dispatch_targets");
        require(targets.containsKey("java_executor"), "missing java target: " + targets);
        ((Map<String, Object>) targets.get("java_executor")).put("supported", true);
        Map<String, Object> statusAgain = OmniVM.ownerDispatchStatus();
        Map<String, Object> targetsAgain = (Map<String, Object>) statusAgain.get("owner_dispatch_targets");
        require(Boolean.FALSE.equals(((Map<String, Object>) targetsAgain.get("java_executor")).get("supported")), "status leaked mutable state");

        Map<String, Object> rubyStatus = OmniVM.rubyThreadingStatus();
        require("single_vm_thread".equals(rubyStatus.get("mode")), "bad ruby threading mode: " + rubyStatus);
        require(Boolean.FALSE.equals(rubyStatus.get("native_threads_supported")), "ruby native threads should be unsupported");
        rubyStatus.put("native_threads_supported", true);
        require(Boolean.FALSE.equals(OmniVM.rubyThreadingStatus().get("native_threads_supported")), "ruby threading status leaked mutable state");

        Map<String, Object> target = OmniVM.ownerDispatchTargetStatus("js");
        require("javascript_event_loop".equals(target.get("target")), "alias did not normalize: " + target);
        require("js".equals(target.get("requested_target")), "requested target missing: " + target);
        require("python_asyncio".equals(OmniVM.ownerDispatchTargetStatus("python async loop").get("target")), "python alias did not normalize");
        require("javascript_event_loop".equals(OmniVM.ownerDispatchTargetStatus("nodejs").get("target")), "nodejs alias did not normalize");
        require("ruby_fiber_thread".equals(OmniVM.ownerDispatchTargetStatus("thread").get("target")), "thread alias did not normalize");
        target.put("supported", true);
        require(Boolean.FALSE.equals(OmniVM.ownerDispatchTargetStatus("js").get("supported")), "target leaked mutable state");

        try {
            OmniVM.ownerDispatchTargetStatus("unknown-loop");
            throw new AssertionError("missing unknown target diagnostic");
        } catch (OmniVM.RuntimeError err) {
            require(err.getMessage().contains("unknown owner dispatch target: unknown-loop"), "bad unknown target message: " + err.getMessage());
            require("owner_dispatch_target".equals(err.getBoundaryPath()), "bad unknown target boundary: " + err.getBoundaryPath());
            Map<String, Object> details = (Map<String, Object>) err.getDetails();
            require("unknown_loop".equals(details.get("target")), "missing top-level unknown target: " + details);
            require("unknown-loop".equals(details.get("requested_target")), "missing top-level requested unknown target: " + details);
            Map<String, Object> dispatch = (Map<String, Object>) details.get("owner_dispatch_target");
            require("unknown_loop".equals(dispatch.get("target")), "missing unknown target: " + details);
            require("unknown-loop".equals(dispatch.get("requested_target")), "missing requested unknown target: " + details);
            require(((java.util.List<?>) dispatch.get("known_targets")).contains("java_executor"), "missing known targets: " + details);
            require(dispatch.get("known_targets").equals(java.util.Arrays.asList("java_executor", "javascript_event_loop", "python_asyncio", "ruby_fiber_thread")), "known targets were not sorted: " + details);
        }

        try {
            OmniVM.assertOwnerDispatchSupported("servlet startup");
            throw new AssertionError("missing universal diagnostic");
        } catch (OmniVM.RuntimeError err) {
            require(err.getMessage().contains("servlet startup: owner dispatch unsupported"), "bad universal message: " + err.getMessage());
            require("owner_dispatch".equals(err.getBoundaryPath()), "bad universal boundary: " + err.getBoundaryPath());
            Map<String, Object> details = (Map<String, Object>) err.getDetails();
            Map<String, Object> dispatch = (Map<String, Object>) details.get("owner_dispatch");
            require(Boolean.FALSE.equals(dispatch.get("owner_dispatch_supported")), "missing universal details: " + details);
            dispatch.put("mode", "mutated");
            Map<String, Object> detailsAgain = (Map<String, Object>) err.getDetails();
            require("diagnostic_only".equals(((Map<String, Object>) detailsAgain.get("owner_dispatch")).get("mode")), "details leaked mutable state");
            require(err.getDetailsJson().contains("\"owner_dispatch_supported\":false"), "details json missing: " + err.getDetailsJson());
        }

        try {
            OmniVM.assertRubyNativeThreadsSupported("puma startup");
            throw new AssertionError("missing ruby threading diagnostic");
        } catch (OmniVM.RuntimeError err) {
            require(err.getMessage().contains("puma startup: native Ruby threads unsupported: mode=single_vm_thread"), "bad ruby threading message: " + err.getMessage());
            require("ruby_threading".equals(err.getBoundaryPath()), "bad ruby threading boundary: " + err.getBoundaryPath());
            Map<String, Object> details = (Map<String, Object>) err.getDetails();
            Map<String, Object> threading = (Map<String, Object>) details.get("ruby_threading");
            require(Boolean.FALSE.equals(threading.get("native_threads_supported")), "missing ruby threading details: " + details);
            threading.put("mode", "mutated");
            Map<String, Object> detailsAgain = (Map<String, Object>) err.getDetails();
            require("single_vm_thread".equals(((Map<String, Object>) detailsAgain.get("ruby_threading")).get("mode")), "ruby threading details leaked mutable state");
            require(err.getDetailsJson().contains("\"native_threads_supported\":false"), "ruby threading details json missing: " + err.getDetailsJson());
        }

        try {
            OmniVM.assertOwnerDispatchTargetSupported("ruby", "async bridge");
            throw new AssertionError("missing target diagnostic");
        } catch (OmniVM.RuntimeError err) {
            require(err.getMessage().contains("async bridge: owner dispatch target unsupported: ruby_fiber_thread"), "bad target message: " + err.getMessage());
            require("owner_dispatch_target".equals(err.getBoundaryPath()), "bad target boundary: " + err.getBoundaryPath());
            Map<String, Object> details = (Map<String, Object>) err.getDetails();
            require("ruby_fiber_thread".equals(details.get("target")), "missing top-level target details: " + details);
            require("ruby".equals(details.get("requested_target")), "missing top-level requested target details: " + details);
            Map<String, Object> dispatch = (Map<String, Object>) details.get("owner_dispatch_target");
            require("ruby_fiber_thread".equals(dispatch.get("target")), "missing target details: " + details);
        }
    }
}
`
	if err := os.WriteFile(checkPath, []byte(check), 0644); err != nil {
		t.Fatalf("write Java owner dispatch check: %v", err)
	}
	if out, err := exec.Command(javac, "-d", tmp, "../../runtime/java/OmniVM.java", checkPath).CombinedOutput(); err != nil {
		t.Fatalf("compile Java owner dispatch check: %v\n%s", err, out)
	}
	if out, err := exec.Command(java, "-cp", tmp, "omnivm.OwnerDispatchCheck").CombinedOutput(); err != nil {
		t.Fatalf("run Java owner dispatch check: %v\n%s", err, out)
	}
}

func TestPythonRubyRuntimeErrorsParseWrappedStructuredEnvelopes(t *testing.T) {
	files := map[string]string{}
	for _, path := range []string{
		"../../pkg/python/python.go",
		"../../pkg/ruby/ruby.go",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		files[path] = string(data)
	}
	if !contains(files["../../pkg/python/python.go"], "wrapped_boundary = ' > '.join(boundary_parts) or (f'call[{source_runtime}]' if source_runtime and source_runtime != runtime else boundary_path)") ||
		!contains(files["../../pkg/python/python.go"], "envelope = _parse_runtime_error_envelope(body, runtime=source_runtime, boundary_path=wrapped_boundary)") {
		t.Fatalf("embedded Python RuntimeError should retry structured envelope parsing after boundary stripping")
	}
	if !contains(files["../../pkg/python/python.go"], "def __init__(self, message, runtime=None, boundary_path=None, details=None)") ||
		!contains(files["../../pkg/python/python.go"], "self._details = _copy_json_value(details) if details is not None else _copy_json_value(parsed['details'])") {
		t.Fatalf("embedded Python RuntimeError should accept copied structured details overrides")
	}
	for _, want := range []string{
		"#define OMNIVM_PY_EXCEPTION_GROUP_MAX_DEPTH 4",
		"#define OMNIVM_PY_EXCEPTION_GROUP_MAX_CHILDREN 64",
		"static PyObject* omnivm_py_details_from_exception_group(PyObject* value)",
		`PyObject_HasAttrString(value, "exceptions")`,
		`PyDict_SetItemString(item, "type", type_name)`,
		`PyDict_SetItemString(item, "message", message)`,
		`PyDict_SetItemString(item, "exceptions", nested)`,
		`PyDict_SetItemString(details, "exceptions", exceptions)`,
		`PyDict_SetItemString(details, "exceptions_truncated", truncated)`,
		"details = omnivm_py_details_from_exception_group(value)",
		"static void omnivm_py_append_exception_cause_line(PyObject* value, char** out, size_t* len)",
		"static void omnivm_py_append_exception_link_causes(PyObject* value, char** out, size_t* len, int depth)",
		`PyObject_GetAttrString(value, "__cause__")`,
		`PyObject_GetAttrString(value, "__suppress_context__")`,
		`PyObject_GetAttrString(value, "__context__")`,
		"static void omnivm_py_append_exception_group_causes(PyObject* value, char** out, size_t* len, int depth)",
		`omnivm_py_append_text(out, len, "\nCaused by: ")`,
		`omnivm_py_append_exception_group_causes(child, out, len, depth + 1)`,
		`omnivm_py_append_text(out, len, "\nCaused by: ExceptionGroup: additional grouped exceptions truncated")`,
		"omnivm_py_append_exception_group_causes(value, &result, &len, 0)",
		"omnivm_py_append_exception_link_causes(value, &result, &len, 0)",
	} {
		if !contains(files["../../pkg/python/python.go"], want) {
			t.Fatalf("embedded Python errors should expose ExceptionGroup details, missing %q", want)
		}
	}
	for _, want := range []string{
		"self._stack_frames = _copy_json_value(parsed['stack_frames'])",
		"self._cause_chain = _copy_json_value(parsed['cause_chain'])",
		"def stack_frames(self):",
		"return _copy_json_value(self._stack_frames)",
		"def stackFrames(self):",
		"return self.stack_frames",
		"def cause_chain(self):",
		"return _copy_json_value(self._cause_chain)",
		"def causeChain(self):",
		"return self.cause_chain",
		"def details(self):",
		"return _copy_json_value(self._details)",
		"self._details_json = _runtime_error_details_json(self._details)",
		"def details_json(self):",
		"return self._details_json",
		"self._details = _copy_json_value(__omnivm_json.loads(value))",
		"self._details_json = _runtime_error_details_json(self._details)",
		"def originRuntime(self):",
		"return self.origin_runtime",
		"def boundaryPath(self):",
		"return self.boundary_path",
		"def originalErrorHandle(self):",
		"return self.original_error_handle",
		"def detailsJson(self):",
		"return self.details_json",
		"self._details_json = _runtime_error_details_json(self._details) if details is not None else parsed.get('details_json')",
		"if self._details_json is None and self._details is not None:",
		"'details_json': self.details_json",
		"def to_json(self):",
		"return globals()['__omnivm_json'].dumps(self.to_dict(), separators=(',', ':'))",
		"if isinstance(value, tuple):",
		"return [_copy_json_value(item) for item in value]",
		"def _runtime_error_details_json(value):",
	} {
		if !contains(files["../../pkg/python/python.go"], want) {
			t.Fatalf("embedded Python RuntimeError readers should copy mutable structured values, missing %q", want)
		}
	}
	for _, want := range []string{
		"def field(preferred, fallback):",
		"def text_field(value, fallback=''):",
		"runtime_name = text_field(envelope.get('runtime'), runtime)",
		"origin_runtime = text_field(field('origin_runtime', 'originRuntime'), runtime_name)",
		"err_type = text_field(field('type', 'name'))",
		"detail = text_field(envelope.get('message'))",
		"traceback = text_field(field('traceback', 'stack'))",
		"def details_field(source):",
		"raw_details = source.get('details_json')",
		"raw_details = source.get('detailsJson')",
		"return __omnivm_json.loads(raw_details)",
		"return raw_details",
		"def details_json_field(source):",
		"lower.startswith('details_json:')",
		"def _split_runtime_error_details_metadata_line(line):",
		"return raw if label in ('details_json', 'detailsjson') else None",
		"if label in ('details_json', 'detailsjson'):",
		"return _runtime_error_details_json(value)",
		"'details_json': details_json_field(envelope)",
		"'details_json': _parse_runtime_error_details_json(body)",
		"stack_frames = field('stack_frames', 'stackFrames')",
		"cause_chain = field('cause_chain', 'causeChain')",
		"item = {'type': str(cause.get('type') or cause.get('name') or ''), 'message': str(cause.get('message') or '')}",
		"cause_traceback = cause.get('stack')",
		"cause_stack_frames = cause.get('stackFrames')",
		"for key, fallback in (('runtime', 'runtime'), ('origin_runtime', 'originRuntime'), ('boundary_path', 'boundaryPath'), ('original_error_handle', 'originalErrorHandle')):",
		"if not item.get('runtime') and runtime_name:",
		"item['runtime'] = runtime_name",
		"if item.get('runtime') and not item.get('origin_runtime'):",
		"cause_chain.append({'type': cause_type, 'message': cause_message, 'runtime': source_runtime, 'origin_runtime': source_runtime})",
		"'details': details_field(envelope)",
	} {
		if !contains(files["../../pkg/python/python.go"], want) {
			t.Fatalf("embedded Python RuntimeError should accept JS camelCase envelope fields, missing %q", want)
		}
	}
	for _, want := range []string{
		"cause_traceback = cause.get('traceback')",
		"cause_traceback = cause.get('stack')",
		"item['stack_frames'] = list(cause_stack_frames)",
		"cause_details = details_field(cause)",
		"item['details'] = cause_details",
		"cause_details_json = details_json_field(cause)",
		"item['details_json'] = cause_details_json",
	} {
		if !contains(files["../../pkg/python/python.go"], want) {
			t.Fatalf("embedded Python RuntimeError should preserve nested cause envelope fields, missing %q", want)
		}
	}
	if !contains(files["../../pkg/ruby/ruby.go"], "fallback_boundary = source_runtime && source_runtime != runtime ?") ||
		!contains(files["../../pkg/ruby/ruby.go"], `body = body[4..-1].to_s if body.start_with?(\"ERR:\")`) ||
		!contains(files["../../pkg/ruby/ruby.go"], "wrapped_boundary = boundary_parts.empty? ? fallback_boundary : boundary_parts.join") ||
		!contains(files["../../pkg/ruby/ruby.go"], "envelope = __parse_runtime_error_envelope(body, source_runtime, wrapped_boundary)") {
		t.Fatalf("embedded Ruby RuntimeError should retry structured envelope parsing after boundary stripping")
	}
	for _, want := range []string{
		`read_field = ->(hash, preferred, fallback = nil) do`,
		`return hash[preferred_sym] if hash.key?(preferred_sym)`,
		`field = ->(preferred, fallback) { read_field.call(envelope, preferred, fallback) }`,
		`text_field = ->(value, fallback = \"\") { value.nil? ? fallback : value.to_s }`,
		`runtime_name = text_field.call(field.call(\"runtime\", \"runtime\"), runtime)`,
		`origin_runtime = text_field.call(field.call(\"origin_runtime\", \"originRuntime\"), runtime_name)`,
		`err_type = text_field.call(field.call(\"type\", \"name\"))`,
		`detail = text_field.call(field.call(\"message\", \"message\"))`,
		`traceback = text_field.call(field.call(\"traceback\", \"stack\"))`,
		`details_field = ->(source) do`,
		`raw_details = read_field.call(source, \"details_json\", \"detailsJson\")`,
		`return JSON.parse(raw_details)`,
		`return raw_details`,
		`lower.start_with?(\"details_json:\")`,
		`def self.__split_runtime_error_details_metadata_line(line)`,
		`return [nil, nil] unless [\"details\", \"details_json\", \"detailsjson\"].include?(label)`,
		`return [\"details_json\", \"detailsjson\"].include?(label) ? raw : nil`,
		`return raw.empty? ? nil : raw if [\"details_json\", \"detailsjson\"].include?(label)`,
		`return __runtime_error_details_json(JSON.parse(raw))`,
		`stack_frames = field.call(\"stack_frames\", \"stackFrames\")`,
		`cause_chain = field.call(\"cause_chain\", \"causeChain\")`,
		`{\"runtime\" => \"runtime\", \"origin_runtime\" => \"originRuntime\", \"boundary_path\" => \"boundaryPath\", \"original_error_handle\" => \"originalErrorHandle\"}.each`,
		"item[:runtime] = runtime_name if !item[:runtime] && runtime_name && !runtime_name.to_s.empty?",
		`boundary_path: text_field.call(field.call(\"boundary_path\", \"boundaryPath\"), boundary_path)`,
		`item = {type: (read_field.call(cause, \"type\", \"name\") || \"\").to_s, message: (read_field.call(cause, \"message\") || \"\").to_s}`,
		"causes << {type: cause_type, message: cause_message, runtime: source_runtime, origin_runtime: source_runtime}",
		`details: details_field.call(envelope)`,
	} {
		if !contains(files["../../pkg/ruby/ruby.go"], want) {
			t.Fatalf("embedded Ruby RuntimeError should accept JS camelCase envelope fields, missing %q", want)
		}
	}
	for _, want := range []string{
		`cause_traceback = read_field.call(cause, \"traceback\", \"stack\")`,
		`cause_stack_frames = read_field.call(cause, \"stack_frames\", \"stackFrames\")`,
		"item[:stack_frames] = cause_stack_frames.dup",
		`item[:origin_runtime] = item[:runtime] if item[:runtime] && !item[:origin_runtime]`,
		`cause_details = details_field.call(cause)`,
		`item[:details] = cause_details unless cause_details.nil?`,
		`cause_details_json = details_json_field.call(cause)`,
		`item[:details_json] = cause_details_json unless cause_details_json.nil?`,
	} {
		if !contains(files["../../pkg/ruby/ruby.go"], want) {
			t.Fatalf("embedded Ruby RuntimeError should preserve nested cause envelope fields, missing %q", want)
		}
	}
	for _, want := range []string{
		"def self.__actual_public_method?(value, name)",
		"def self.__lifecycle_method_without_required_args?(value, name)",
		"return false unless __actual_public_method?(value, name)",
		"arity = value.method(name).arity",
		"arity == 0 || arity == -1",
		"def self.proxy_close(value)",
		"def self.omnivm_close(value)",
		"return value.public_send(:omnivm_close) if __lifecycle_method_without_required_args?(value, :omnivm_close)",
		"if __lifecycle_method_without_required_args?(value, :close)",
		"result = value.public_send(:close)",
		"if __lifecycle_method_without_required_args?(value, :dispose)",
		"result = value.public_send(:dispose)",
		"return result.nil? ? true : result",
	} {
		if !contains(files["../../pkg/ruby/ruby.go"], want) {
			t.Fatalf("embedded Ruby OmniVM module should expose collision-safe close helpers, missing %q", want)
		}
	}
	if contains(files["../../pkg/ruby/ruby.go"], "value.respond_to?(:close)") ||
		contains(files["../../pkg/ruby/ruby.go"], "value.respond_to?(:dispose)") ||
		contains(files["../../pkg/ruby/ruby.go"], "value.respond_to?(:omnivm_close)") {
		t.Fatalf("embedded Ruby close helpers should not trust respond_to_missing? for lifecycle methods")
	}
	for _, want := range []string{
		"def stack_frames",
		"OmniVM.__copy_json_value(@stack_frames)",
		"def stack_frames=(value)",
		"def stackFrames=(value)",
		"def cause_chain",
		"OmniVM.__copy_json_value(@cause_chain)",
		"def cause_chain=(value)",
		"def causeChain=(value)",
		"def traceback=(value)",
		"def originRuntime=(value)",
		"def boundaryPath=(value)",
		"def originalErrorHandle=(value)",
		"def details",
		"OmniVM.__copy_json_value(@details)",
		"def details=(value)",
		"@details_json = OmniVM.__runtime_error_details_json(@details)",
		"def details_json=(value)",
		"@details = OmniVM.__copy_json_value(JSON.parse(value))",
		"def detailsJson=(value)",
		"attr_reader :runtime, :origin_runtime, :type, :traceback, :boundary_path, :original_error_handle, :details_json",
		"def initialize(message, runtime: nil, boundary_path: nil, details: nil)",
		"@details = details.nil? ? parsed[:details] : OmniVM.__copy_json_value(details)",
		"@details_json = details.nil? ? parsed[:details_json] : nil",
		"def self.__runtime_error_details_json(value)",
		"alias originRuntime origin_runtime",
		"alias stackFrames stack_frames",
		"alias causeChain cause_chain",
		"alias boundaryPath boundary_path",
		"alias originalErrorHandle original_error_handle",
		"alias detailsJson details_json",
		"details_json: @details_json",
	} {
		if !contains(files["../../pkg/ruby/ruby.go"], want) {
			t.Fatalf("embedded Ruby RuntimeError readers should copy mutable structured values, missing %q", want)
		}
	}
	if contains(files["../../pkg/ruby/ruby.go"], "attr_reader :runtime, :origin_runtime, :type, :traceback, :stack_frames, :cause_chain") {
		t.Fatalf("embedded Ruby RuntimeError should not expose mutable structured fields through attr_reader")
	}
	if contains(files["../../pkg/ruby/ruby.go"], "value = {errors: value} if value.is_a?(Array)") {
		t.Fatalf("embedded Ruby error details should preserve non-object JSON instead of wrapping arrays")
	}
	if !contains(files["../../pkg/ruby/ruby.go"], "def as_json(*_args)") ||
		!contains(files["../../pkg/ruby/ruby.go"], "def to_json(*args)") ||
		!contains(files["../../pkg/ruby/ruby.go"], "JSON.generate(to_h, *args)") {
		t.Fatalf("embedded Ruby RuntimeError should expose Rails/Ruby JSON envelope helpers")
	}
	if !contains(files["../../pkg/ruby/ruby.go"], "details_json_field = ->(source) do") ||
		!contains(files["../../pkg/ruby/ruby.go"], "details_json: details_json_field.call(envelope)") ||
		!contains(files["../../pkg/ruby/ruby.go"], "def self.__parse_runtime_error_details_json(text)") ||
		!contains(files["../../pkg/ruby/ruby.go"], "details_json: OmniVM.__parse_runtime_error_details_json(body)") {
		t.Fatalf("embedded Ruby RuntimeError should preserve raw details_json in structured envelopes")
	}
}

func TestEmbeddedPythonRegistersCoreProxyCloseHelper(t *testing.T) {
	data, err := os.ReadFile("../../pkg/python/python.go")
	if err != nil {
		t.Fatalf("read Python runtime source: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		"static const char* omnivm_py_proxy_close_code",
		"def __omnivm_actual_public_method(value, name):",
		"__omnivm_inspect.getattr_static(value, name)",
		"isinstance(raw, (staticmethod, classmethod))",
		"method = raw.__get__(value, type(value))",
		"__omnivm_inspect.ismemberdescriptor(raw)",
		"if not callable(raw):",
		"instance_dict = object.__getattribute__(value, '__dict__')",
		"instance_dict.get(name) is raw",
		"__omnivm_inspect.isfunction(raw)",
		"__omnivm_inspect.ismethoddescriptor(raw)",
		"def __omnivm_lifecycle_method_accepts_no_args(method):",
		"__omnivm_inspect.signature(method)",
		"parameter.default is __omnivm_inspect.Signature.empty",
		"callable(close) and __omnivm_lifecycle_method_accepts_no_args(close)",
		"def proxy_close(value):",
		"async def aproxy_close(value):",
		"def omnivm_close(value):",
		"async def omnivm_aclose(value):",
		"def cleanup_errors(error):",
		"return list(errors) if isinstance(errors, list) else []",
		"('proxy_close', 'aproxy_close', 'omnivm_close', 'omnivm_aclose', 'cleanup_errors')",
		"close = __omnivm_actual_public_method(value, '_omnivm_close')",
		"close = __omnivm_actual_public_method(value, 'close')",
		"close = __omnivm_actual_public_method(value, 'dispose')",
		"close = __omnivm_actual_public_method(value, 'aclose')",
		"__omnivm_inspect.isawaitable(result)",
		"return True if result is None else result",
		"static void omnivm_py_install_proxy_close_helpers(PyObject* module)",
		"omnivm_py_install_proxy_close_helpers(module)",
		"omnivm_py_install_proxy_close_helpers(mod)",
	} {
		if !contains(code, want) {
			t.Fatalf("embedded Python omnivm module should expose collision-safe close helpers, missing %q", want)
		}
	}
	if contains(code, `getattr(value, "close", None)`) ||
		contains(code, `getattr(value, "_omnivm_close", None)`) ||
		contains(code, `getattr(value, name, None)`) ||
		contains(code, `getattr(value, name)`) {
		t.Fatalf("embedded Python close helpers should not invoke dynamic close attribute lookup")
	}
	apreferred := code[strings.Index(code, "async def aproxy_close(value):"):]
	apreferred = apreferred[:strings.Index(apreferred, "async def omnivm_aclose(value):")]
	if strings.Index(apreferred, "close = __omnivm_actual_public_method(value, 'aclose')") >
		strings.Index(apreferred, "close = __omnivm_actual_public_method(value, 'dispose')") {
		t.Fatalf("embedded Python async close helper should prefer aclose before dispose, got %q", apreferred)
	}
}

func TestEmbeddedRubyThreadCreationAliasesReportUnsupportedDiagnostic(t *testing.T) {
	data, err := os.ReadFile("../../pkg/ruby/ruby.go")
	if err != nil {
		t.Fatalf("read Ruby runtime source: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		"alias __omnivm_native_new new",
		"alias __omnivm_native_start start",
		"alias __omnivm_native_fork fork",
		"alias new __omnivm_unsupported_new",
		"alias start __omnivm_unsupported_new",
		"alias fork __omnivm_unsupported_new",
		"alias __omnivm_native_daemon daemon",
		"def daemon(*args)",
		"daemon() is not safe in OmniVM embedded Ruby",
		"alias __omnivm_native_spawn spawn",
		"spawn() is not safe in OmniVM embedded Ruby",
		"alias __omnivm_native_popen popen",
		"IO.popen is not safe in OmniVM embedded Ruby",
		"module Kernel",
		"alias __omnivm_native_fork fork unless private_method_defined?(:__omnivm_native_fork)",
		"def fork(*args, &block)",
		"fork() is not safe in OmniVM embedded Ruby",
		"alias __omnivm_native_system system",
		"system() is not safe in OmniVM embedded Ruby",
		"alias __omnivm_native_exec exec",
		"exec() is not safe in OmniVM embedded Ruby",
		"def `(cmd)",
		"backticks are not safe in OmniVM embedded Ruby",
		"Kernel#fork diagnostic",
		"Kernel.fork diagnostic",
		"Process.fork diagnostic",
		"Process.daemon diagnostic",
		"Process.spawn diagnostic",
		"Kernel.spawn diagnostic",
		"Kernel.system diagnostic",
		"Kernel.exec diagnostic",
		"Kernel backtick diagnostic",
		"IO.popen diagnostic",
		"Thread.new diagnostic",
		"Thread.start diagnostic",
		"Thread.fork diagnostic",
		"native-threaded Ruby app servers such as Puma out of process",
		"def self.ruby_threading_status",
		`\"native_threads_supported\" => false`,
		`\"app_server_boundary\" => \"Use Fiber/Async or single-thread Rack servers in process; run native-threaded Ruby app servers such as Puma out of process.\"`,
		"def self.assert_ruby_native_threads_supported(label = nil)",
		`boundary_path: \"ruby_threading\"`,
		`details: {\"ruby_threading\" => info}`,
		"def self.owner_dispatch_status",
		`\"owner_dispatch_supported\" => false`,
		`\"javascript_event_loop\" => {`,
		`\"python_async_stream_pull\"`,
		`\"python_async_loop\" => \"python_asyncio\"`,
		`\"nodejs\" => \"javascript_event_loop\"`,
		`\"event_loop\" => \"javascript_event_loop\"`,
		`\"fiber\" => \"ruby_fiber_thread\"`,
		`\"thread\" => \"ruby_fiber_thread\"`,
		"def self.owner_dispatch_target_status(target)",
		"def self.assert_owner_dispatch_supported(label = nil)",
		"def self.assert_owner_dispatch_target_supported(target, label = nil)",
		`RuntimeError.new(\"unknown owner dispatch target: #{requested}\"`,
		`\"known_targets\" => targets.keys.sort`,
		`boundary_path: \"owner_dispatch\"`,
		`details: {\"owner_dispatch\" => info}`,
		`boundary_path: \"owner_dispatch_target\"`,
		`\"target\" => info[\"target\"]`,
		`\"requested_target\" => info[\"requested_target\"]`,
		`\"owner_dispatch_target\" => info`,
		"def push(value, non_block = false)",
		"raise ThreadError, 'queue full' if non_block",
		"Ruby SizedQueue#push would block in OmniVM embedded Ruby",
	} {
		if !contains(code, want) {
			t.Fatalf("embedded Ruby should diagnose unsupported native thread creation through all aliases, missing %q", want)
		}
	}
}

func TestLibOmniVMThreadAffinityStatusReportsUnsupportedOwnerDispatch(t *testing.T) {
	data, err := os.ReadFile("../../cmd/libomnivm/main.go")
	if err != nil {
		t.Fatalf("read libomnivm source: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		`"mode":                     "diagnostic_only"`,
		`"host_thread_required":     true`,
		`"owner_dispatch_supported": false`,
		`"foreign_thread_behavior":   "reject_runtime_calls"`,
		`"python_asyncio": map[string]interface{}{`,
		`"narrow_capabilities": []string{"python_async_stream_pull", "python_async_stream_close"}`,
		`"diagnostic":          "Python async streams have narrow pump-owned pull/close paths, but general callbacks are not migrated back to the owner loop"`,
		`"javascript_event_loop": map[string]interface{}{`,
		`"java_executor": map[string]interface{}{`,
		`"ruby_fiber_thread": map[string]interface{}{`,
		`"current_behavior":    "Ruby runs on the single VM thread with native Ruby thread scheduling disabled"`,
		`"diagnostic":          "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported"`,
		`"app_server_boundary":      "Use Fiber/Async or single-thread Rack servers in process; run native-threaded Ruby app servers such as Puma out of process."`,
	} {
		if !contains(code, want) {
			t.Fatalf("libomnivm thread affinity status should expose diagnostic-only owner dispatch boundary, missing %q", want)
		}
	}
	if contains(code, `"owner_dispatch_supported": true`) ||
		contains(code, `"supported":           true`) ||
		contains(code, "go threadAffinityStatus") {
		t.Fatalf("libomnivm thread affinity status should not claim universal owner dispatch or add helper threads")
	}
}

func TestLibOmniVMRuntimeEntrypointsRequireHostThread(t *testing.T) {
	data, err := os.ReadFile("../../cmd/libomnivm/main.go")
	if err != nil {
		t.Fatalf("read libomnivm source: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		`done, err := beginRuntimeExternalCall("call", threadID)`,
		`done, err := beginRuntimeExternalCall("exec", threadID)`,
		`done, err := beginRuntimeExternalCall("manifest", threadID)`,
		`done, err := beginRuntimeExternalCall("load_manifest_module", threadID)`,
		`done, err := beginRuntimeExternalCall("manifest_call", threadID)`,
		`done, err := beginRuntimeExternalCall("load_plugin", threadID)`,
		`done, err := beginRuntimeExternalCall("typed_call", threadID)`,
		"func beginRuntimeExternalCall(op string, threadID int64) (func(), error)",
		"if err := requireHostThreadForRuntimeCall(op, threadID); err != nil",
		"thread affinity violation: %s must run on OmniVM host thread",
		"owner dispatch is unsupported in c-shared mode",
	} {
		if !contains(code, want) {
			t.Fatalf("libomnivm runtime entrypoints should reject foreign host threads, missing %q", want)
		}
	}
}

func TestManifestRunnerTypedCallbacksRejectForeignThreads(t *testing.T) {
	data, err := os.ReadFile("../../cmd/manifest-runner/main.go")
	if err != nil {
		t.Fatalf("read manifest-runner source: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		`func OmniCallTyped(cRuntime *C.char, cFuncName *C.char, cArgs *C.omni_value_t, nargs C.int32_t) C.omni_value_t`,
		`currentTid := int64(C.get_thread_id())`,
		`if currentTid != goldenThreadID {`,
		`polyglot.Error(fmt.Sprintf("omnivm.typed_call from non-Golden Thread (tid=%d, expected=%d)", currentTid, goldenThreadID)).ToCValueRaw(unsafe.Pointer(&cv))`,
	} {
		if !contains(code, want) {
			t.Fatalf("manifest-runner typed bridge should reject non-Golden Thread callbacks, missing %q", want)
		}
	}
}

func TestPythonInterpreterModeExposesDiagnosticStatusGuards(t *testing.T) {
	files := map[string]string{}
	for _, path := range []string{
		"../../pkg/python/python.go",
		"../../cmd/omnivm/main.go",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		files[path] = string(data)
	}
	pythonCode := files["../../pkg/python/python.go"]
	for _, want := range []string{
		"typedef char* (*omni_status_fn)(void)",
		"static omni_status_fn         g_status",
		"static PyObject* pymode_status",
		`{"status",        pymode_status`,
		"def affinity_status():",
		"def assert_host_thread(label=''):",
		"__threading.get_native_id()",
		"__asyncio.get_running_loop()",
		"owner dispatch unsupported",
		"details={'affinity': info}",
		"def owner_dispatch_status():",
		"return _copy_json_value(info)",
		"_OWNER_DISPATCH_TARGET_ALIASES = {'asyncio': 'python_asyncio'",
		"'python_loop': 'python_asyncio'",
		"'py': 'python_asyncio'",
		"'javascript_loop': 'javascript_event_loop'",
		"'nodejs': 'javascript_event_loop'",
		"'event_loop': 'javascript_event_loop'",
		"'fiber': 'ruby_fiber_thread'",
		"'thread': 'ruby_fiber_thread'",
		"def _owner_dispatch_target_name(target):",
		"normalized = target_name.strip().lower().replace('-', '_').replace(' ', '_')",
		"def owner_dispatch_target_status(target):",
		"requested_target = str(target)",
		"boundary_path='owner_dispatch_target'",
		"'owner_dispatch': dispatch_info",
		"'requested_target': requested_target",
		"'owner_dispatch_target': {'target': target_name",
		"info['requested_target'] = requested_target",
		"info['target'] = target_name",
		"def assert_owner_dispatch_supported(label=''):",
		"boundary_path='owner_dispatch', details={'owner_dispatch': info}",
		"def assert_owner_dispatch_target_supported(target, label=''):",
		"info = owner_dispatch_target_status(requested_target)",
		"boundary_path='owner_dispatch_target'",
		"def ruby_threading_status():",
		"def assert_ruby_native_threads_supported(label=''):",
		"omnivm_py_install_pymode_status_helpers(mod)",
		"C.omni_status_fn(statusPtr)",
	} {
		if !contains(pythonCode, want) {
			t.Fatalf("Python interpreter mode status helpers missing %q", want)
		}
	}
	cmdCode := files["../../cmd/omnivm/main.go"]
	for _, want := range []string{
		"extern char* OmniPyModeStatus(void)",
		"get_omni_pymode_status_ptr",
		"func pythonModeThreadAffinityStatus(hostThreadID int64)",
		`"mode":                     "diagnostic_only"`,
		`"owner_dispatch_supported": false`,
		`"foreign_thread_behavior": "reject_runtime_calls"`,
		`"python_asyncio": map[string]interface{}{`,
		`"narrow_capabilities": []string{"python_async_stream_pull", "python_async_stream_close"}`,
		`"ruby_fiber_thread": map[string]interface{}{`,
		`"diagnostic":          "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported"`,
		"func OmniPyModeStatus() *C.char",
		`"ruby_threading": map[string]interface{}{`,
		`"native_threads_supported": false`,
		`"app_server_boundary":      "Use Fiber/Async or single-thread Rack servers in process; run native-threaded Ruby app servers such as Puma out of process."`,
		"Python interpreter mode runs direct runtime calls on the pinned host",
		"Starting a Go background dispatcher here would move runtime",
		`if err := requireGoldenThreadForRuntimeCall("call", threadID); err != nil`,
		`if err := requireGoldenThreadForRuntimeCall("typed call", threadID); err != nil`,
		"func requireGoldenThreadForRuntimeCall(op string, threadID int64) error",
		"owner dispatch is unsupported in this mode, so OmniVM will not route this call onto the host thread",
		"json.Marshal(status)",
	} {
		if !contains(cmdCode, want) {
			t.Fatalf("Python interpreter mode Go status contract missing %q", want)
		}
	}
	if contains(cmdCode, "eng.StartDispatcher()") {
		t.Fatalf("cmd/omnivm should not start a background dispatcher after host-thread runtime initialization")
	}
	if contains(cmdCode, `"owner_dispatch_supported": true`) ||
		contains(cmdCode, `"supported":           true`) {
		t.Fatalf("Python interpreter mode status should not claim universal owner dispatch")
	}
}

func TestRuntimeBufferCallbacksSeparateFreeFromBorrowRelease(t *testing.T) {
	files := map[string]string{}
	for _, path := range []string{
		"../../pkg/python/python.go",
		"../../pkg/ruby/ruby.go",
		"../../pkg/javascript/javascript.go",
		"../../scripts/v8_bridge_node.cc",
		"../../scripts/jvm_docker.go",
		"../../pkg/engine/engine.go",
		"../../cmd/omnivm/main.go",
		"../../cmd/libomnivm/main.go",
		"../../cmd/manifest-runner/main.go",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		files[path] = string(data)
	}

	for path, code := range files {
		if !contains(code, "freePtr") && !contains(code, "g_buf_free") && !contains(code, "get_omni_buf_free_ptr") {
			t.Fatalf("%s does not participate in explicit buffer-free callback plumbing", path)
		}
	}
	if !contains(files["../../pkg/python/python.go"], "g_buf_free(name)") || !contains(files["../../pkg/python/python.go"], "g_buf_release(name)") {
		t.Fatalf("embedded Python should use g_buf_free for release_buffer and g_buf_release for borrow cleanup")
	}
	if !contains(files["../../pkg/python/python.go"], "g_buf_status(name)") || !contains(files["../../pkg/python/python.go"], `PyErr_Format(PyExc_RuntimeError, "omnivm.release_buffer failed: %s", raw)`) {
		t.Fatalf("embedded Python explicit release_buffer should include buffer status diagnostics on release failure")
	}
	for _, want := range []string{
		"omnivm_py_last_export_rejection",
		"omnivm_py_has_export_rejection",
		"omnivm_py_reject_cuda_array_interface",
		"__cuda_array_interface__ exposes device memory; OmniVM zero-copy native memory currently requires host memory",
		"__dlpack__ device is not CPU-addressable; OmniVM zero-copy native memory currently requires host memory",
		"dataframe interchange data buffer is not CPU-addressable; OmniVM zero-copy native memory currently requires host memory",
		"if (!exported && !omnivm_py_has_export_rejection())",
		`fmt.Errorf("python: native_memory unsupported zero-copy buffer export for %q: %s", name, C.GoString(rejection))`,
	} {
		if !contains(files["../../pkg/python/python.go"], want) {
			t.Fatalf("embedded Python buffer export should diagnose explicit non-host native-memory protocols, missing %q", want)
		}
	}
	if !contains(files["../../pkg/ruby/ruby.go"], "g_buf_free(name)") || !contains(files["../../pkg/ruby/ruby.go"], "g_buf_release(name)") {
		t.Fatalf("embedded Ruby should use g_buf_free for release_buffer and g_buf_release for borrow cleanup")
	}
	if !contains(files["../../pkg/ruby/ruby.go"], `rb_raise(rb_eRuntimeError, "omnivm buffer bridge not initialized")`) {
		t.Fatalf("embedded Ruby explicit release_buffer should diagnose a missing buffer-free callback")
	}
	if !contains(files["../../pkg/ruby/ruby.go"], "g_buf_status(name)") || !contains(files["../../pkg/ruby/ruby.go"], `rb_str_cat2(message, ": ")`) {
		t.Fatalf("embedded Ruby explicit release_buffer should include buffer status diagnostics on release failure")
	}
	for _, want := range []string{
		"class BufferOwner",
		"def self.buffer_owner(name, data = BUFFER_OWNER_UNSET, dtype: 0)",
		"cannot be re-entered after release",
		"is already active",
		"OmniVM.set_buffer(@name, @data, @dtype) unless @data.equal?(BUFFER_OWNER_UNSET)",
		"return false if @released",
		"OmniVM.release_buffer(@name)",
		"@entered = false",
		"alias close release",
		"def status",
		"OmniVM.buffer_status(@name)",
		"def self.buffer_status(name)",
		"JSON.parse(buffer_status_json(name.to_s))",
		"def self.cleanup_errors(error)",
		"errors.is_a?(Array) ? errors.dup : []",
		"result = yield owner",
		"result",
		"__record_cleanup_error(body_error, cleanup_error)",
		"raise body_error",
	} {
		if !contains(files["../../pkg/ruby/ruby.go"], want) {
			t.Fatalf("embedded Ruby buffer owner helper contract missing %q", want)
		}
	}
	if !contains(files["../../scripts/v8_bridge_node.cc"], "g_buf_free(*name)") || !contains(files["../../scripts/v8_bridge_node.cc"], "g_buf_release(lease->name)") {
		t.Fatalf("V8 bridge should use g_buf_free for releaseBuffer and g_buf_release for external buffer cleanup")
	}
	if !contains(files["../../scripts/v8_bridge_node.cc"], "g_buf_status(*name)") || !contains(files["../../scripts/v8_bridge_node.cc"], `"bufferStatus"`) {
		t.Fatalf("V8 bridge should expose bufferStatus through the shared buffer status callback")
	}
	if !contains(files["../../scripts/v8_bridge_node.cc"], "omnivmBufferStatus") || !contains(files["../../scripts/v8_bridge_node.cc"], `message += status_json`) {
		t.Fatalf("V8 releaseBuffer failure should include parsed buffer status diagnostics")
	}
	if !contains(files["../../scripts/jvm_docker.go"], "g_buf_free(name)") || !contains(files["../../scripts/jvm_docker.go"], "g_buf_release(name)") {
		t.Fatalf("JVM bridge should use g_buf_free for releaseBuffer and g_buf_release for copied buffer cleanup")
	}
	if !contains(files["../../scripts/jvm_docker.go"], "g_buf_status(name)") || !contains(files["../../scripts/jvm_docker.go"], "nativeBufferStatus") {
		t.Fatalf("JVM bridge should expose bufferStatus through the shared buffer status callback")
	}
	if !contains(files["../../scripts/jvm_docker.go"], "OmniVM.releaseBuffer failed: %s") || !contains(files["../../scripts/jvm_docker.go"], "snprintf(msg, msg_len") {
		t.Fatalf("JVM releaseBuffer failure should include buffer status diagnostics")
	}
	javaRuntime, err := os.ReadFile("../../runtime/java/OmniVM.java")
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	javaCode := string(javaRuntime)
	for _, want := range []string{
		"public static BufferOwner bufferOwner(String name)",
		"public static <T> T bufferOwner(String name, Function<BufferOwner, T> body)",
		"public static BufferOwner bufferOwner(String name, byte[] data)",
		"public static <T> T bufferOwner(String name, byte[] data, Function<BufferOwner, T> body)",
		"public static BufferOwner bufferOwner(String name, byte[] data, int dtype)",
		"public static <T> T bufferOwner(String name, byte[] data, int dtype, Function<BufferOwner, T> body)",
		"import java.util.function.Function;",
		"private static <T> T withBufferOwner(BufferOwner owner, Function<BufferOwner, T> body)",
		"T result = body.apply(owner);",
		"cleanupError",
		"err.addSuppressed(cleanupError);",
		"public static final class BufferOwner implements AutoCloseable",
		"cannot be re-entered after release",
		"is already active",
		"\"native_memory\"",
		"setBuffer(name, data, dtype)",
		"if (released) {\n                return false;",
		"releaseBuffer(name)",
		"public static String bufferStatus(String name)",
		"public String status()",
		"return bufferStatus(name)",
		"public static native String nativeBufferStatus(String name)",
		"released = true",
		"entered = false",
		"public void close() {\n            release();",
		"if (target instanceof BufferOwner owner) {\n            return owner.release();",
	} {
		if !contains(javaCode, want) {
			t.Fatalf("Java BufferOwner helper contract missing %q", want)
		}
	}
	for _, want := range []string{
		"GetDirectBufferAddress(env, obj)",
		"handle->object = (*env)->NewGlobalRef(env, obj)",
		"handle->array_kind = JVM_EXPORT_DIRECT",
		"ReleasePrimitiveArrayCritical(env, (jarray)handle->object, handle->critical, JNI_ABORT)",
		"(*env)->DeleteGlobalRef(env, handle->object)",
		`MemorySpace: "host"`,
		"SetExternalWithMetadata(name, unsafe.Pointer(out.data), byteLen, meta",
	} {
		if !contains(files["../../scripts/jvm_docker.go"], want) {
			t.Fatalf("JVM direct/NIO buffer export should carry explicit host producer-memory lease semantics, missing %q", want)
		}
	}
	if !contains(files["../../cmd/libomnivm/main.go"], "func OmniBufFree") || !contains(files["../../cmd/libomnivm/main.go"], "get_omni_buf_free_ptr") {
		t.Fatalf("libomnivm should export and pass OmniBufFree")
	}
	for _, path := range []string{
		"../../cmd/omnivm/main.go",
		"../../cmd/libomnivm/main.go",
		"../../cmd/manifest-runner/main.go",
	} {
		if !contains(files[path], "func OmniBufStatus") || !contains(files[path], "arrow.BufStatusJSON") {
			t.Fatalf("%s should export OmniBufStatus lifecycle diagnostics", path)
		}
	}
}

func TestPythonNativeRuntimeErrorFormatterAcceptsNameStackAliases(t *testing.T) {
	data, err := os.ReadFile("../../pkg/python/python.go")
	if err != nil {
		t.Fatalf("read Python runtime source: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		"static char* omnivm_py_unicode_attr_dup_fallback(PyObject* obj, const char* primary, const char* fallback)",
		`char* err_type = omnivm_py_unicode_attr_dup_fallback(value, "type", "name")`,
		`char* traceback = omnivm_py_unicode_attr_dup_fallback(value, "traceback", "stack")`,
		`static PyObject* omnivm_py_mapping_get_item_fallback(PyObject* obj, const char* primary, const char* fallback)`,
		`PyObject* cause_type_obj = omnivm_py_mapping_get_item_fallback(cause, "type", "name")`,
	} {
		if !contains(code, want) {
			t.Fatalf("Python native RuntimeError formatter should normalize JS-style aliases, missing %q", want)
		}
	}
}

func TestRubyNativeErrorFormatterPreservesCauseChain(t *testing.T) {
	data, err := os.ReadFile("../../pkg/ruby/ruby.go")
	if err != nil {
		t.Fatalf("read Ruby runtime source: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		"static VALUE call_exception_cause_chain_text(VALUE exception)",
		`ID method = rb_intern("__error_cause_chain_text")`,
		"const char* causes_cstr = NULL",
		"VALUE causes = rb_protect(call_exception_cause_chain_text, exception, &inner_state)",
		`if (causes_cstr) len += strlen("\n") + strlen(causes_cstr)`,
		"if (causes_cstr) {\n        strcat(err, \"\\n\");\n        strcat(err, causes_cstr);\n    }",
		"def self.__error_cause_chain_text(error)",
		"while cause && lines.length < 16",
		"break if seen[oid]",
		`lines << \"Caused by: #{type}: #{message}\"`,
	} {
		if !contains(code, want) {
			t.Fatalf("Ruby native error formatter should preserve bounded cause chains, missing %q", want)
		}
	}
}

func TestV8RuntimeErrorExposesJSONEnvelope(t *testing.T) {
	data, err := os.ReadFile("../../scripts/v8_bridge_node.cc")
	if err != nil {
		t.Fatalf("read V8 bridge: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		"omnivm_v8_runtime_error_to_json",
		"omnivm_v8_parse_runtime_error_envelope_text",
		"omnivm_v8_parse_runtime_error_envelope_object",
		"omnivm_v8_details_json_prop_fallback",
		"omnivm_v8_append_aggregate_errors",
		"omnivm_v8_json_clone_value",
		"omnivm_v8_json_fallback_stringify",
		"value->IsBigInt()",
		`"[Circular]"`,
		"v8::TryCatch try_catch(isolate)",
		"std::string json = omnivm_v8_json_stringify(isolate, context, value)",
		`value = omnivm_v8_json_clone_value(isolate, context, value)`,
		`"toJSON"`,
		`"origin_runtime"`,
		`"stack_frames"`,
		`"cause_chain"`,
		`"boundary_path"`,
		`"original_error_handle"`,
	} {
		if !contains(code, want) {
			t.Fatalf("V8 runtime error JSON envelope missing %q", want)
		}
	}
	for _, want := range []string{
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "origin_runtime", "originRuntime", "origin_runtime")`,
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "type", "name", "type")`,
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "traceback", "stack", "traceback")`,
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "stack_frames", "stackFrames", "stack_frames")`,
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "cause_chain", "causeChain", "cause_chain")`,
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "boundary_path", "boundaryPath", "boundary_path")`,
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "original_error_handle", "originalErrorHandle", "original_error_handle")`,
		`omnivm_v8_copy_prop_fallback(isolate, context, error, out, "details_json", "detailsJson", "details_json")`,
	} {
		if !contains(code, want) {
			t.Fatalf("V8 runtime error toJSON should normalize snake_case and camelCase fields, missing %q", want)
		}
	}
	for _, want := range []string{
		`if (!omnivm_v8_parse_runtime_error_envelope_text(isolate, context, err_msg, runtime_hint, envelope))`,
		`std::string origin_runtime = env.origin_runtime.empty() ? env.runtime : env.origin_runtime`,
		`std::string type = omnivm_v8_get_string_prop_fallback(isolate, context, object, "type", "name")`,
		`std::string traceback = omnivm_v8_get_string_prop_fallback(isolate, context, object, "traceback", "stack")`,
		`std::string cause_type = omnivm_v8_get_string_prop_fallback(isolate, context, cause, "type", "name")`,
		`std::string details = omnivm_v8_details_json_prop_fallback(isolate, context, object)`,
		`env.origin_runtime = omnivm_v8_get_string_prop_fallback(isolate, context, object, "origin_runtime", "originRuntime")`,
		`env.type = omnivm_v8_get_string_prop_fallback(isolate, context, object, "type", "name")`,
		`env.traceback = omnivm_v8_get_string_prop_fallback(isolate, context, object, "traceback", "stack")`,
		`env.details_json = omnivm_v8_details_json_prop_fallback(isolate, context, object)`,
		`if (cause.runtime.empty())`,
		`cause.runtime = env.runtime`,
		`cause.origin_runtime = cause.runtime`,
		`cause.origin_runtime = env.runtime`,
		`cause.type = omnivm_v8_get_string_prop_fallback(isolate, context, cause_object, "type", "name")`,
		`cause.traceback = omnivm_v8_get_string_prop_fallback(isolate, context, cause_object, "traceback", "stack")`,
		`cause.details_json = omnivm_v8_details_json_prop_fallback(isolate, context, cause_object)`,
		`omnivm_v8_set_string_prop(isolate, context, cause, "origin_runtime", env.cause_chain[i].origin_runtime)`,
		`v8::String::NewFromUtf8Literal(isolate, "detailsJson")`,
		`v8::String::NewFromUtf8Literal(isolate, "details_json")`,
		`omnivm_v8_set_string_prop(isolate, context, error, "detailsJson", env.details_json)`,
		`omnivm_v8_set_string_prop(isolate, context, error, "details_json", env.details_json)`,
		`const char* keys[] = {"details_json", "detailsJson"}`,
		`std::string issues = omnivm_v8_json_stringify_prop(isolate, context, object, "issues")`,
		`return "{\"issues\":" + issues + "}"`,
		`std::string errors = omnivm_v8_json_stringify_prop(isolate, context, object, "errors")`,
		`return "{\"errors\":" + errors + "}"`,
		`v8::String::NewFromUtf8(isolate, "errors").ToLocalChecked()`,
		`uint32_t limit = length < 64 ? length : 64`,
		`out += "\nCaused by: "`,
		`omnivm_v8_append_error_causes(isolate, context, item, out, 0)`,
		`omnivm_v8_append_aggregate_errors(isolate, context, item, out, depth + 1)`,
		`out += "\nCaused by: AggregateError: additional aggregate errors truncated"`,
		`omnivm_v8_append_aggregate_errors(isolate, context, exception, text, 0)`,
		`details = details_text`,
	} {
		if !contains(code, want) {
			t.Fatalf("V8 runtime error parser should preserve structured envelopes across prefixed bridge errors, missing %q", want)
		}
	}
	if contains(code, `v8::String::NewFromUtf8Literal(isolate, "errors"),`) {
		t.Fatalf("V8 runtime error details should preserve non-object JSON instead of wrapping arrays")
	}
	genericDetailsStart := strings.Index(code, "static void omnivm_v8_append_error_details")
	runtimeFormatterStart := strings.Index(code, "static std::string omnivm_v8_format_runtime_error_object")
	if genericDetailsStart < 0 || runtimeFormatterStart < 0 || runtimeFormatterStart <= genericDetailsStart {
		t.Fatalf("V8 generic exception details formatter should remain defined before runtime error object formatter")
	}
	genericDetailsBody := code[genericDetailsStart:runtimeFormatterStart]
	if !contains(genericDetailsBody, `std::string details = omnivm_v8_details_json_prop_fallback(isolate, context, obj)`) {
		t.Fatalf("V8 generic exception details formatter should use details_json/detailsJson fallback aliases")
	}
}

func TestV8BridgeRegistersCoreProxyCloseHelper(t *testing.T) {
	data, err := os.ReadFile("../../scripts/v8_bridge_node.cc")
	if err != nil {
		t.Fatalf("read V8 bridge: %v", err)
	}
	code := string(data)
	for _, want := range []string{
		"static void register_omnivm_proxy_helpers",
		`Object.defineProperty(globalThis, "__omnivm_actual_public_method"`,
		"Object.getOwnPropertyDescriptor(cursor, name)",
		`Object.defineProperty(globalThis.omnivm, "proxyClose"`,
		`Object.defineProperty(globalThis.omnivm, "omnivmClose"`,
		`Object.defineProperty(globalThis.omnivm, "ownerDispatchStatus"`,
		`Object.defineProperty(globalThis.omnivm, "ownerDispatchTargetStatus"`,
		`Object.defineProperty(globalThis.omnivm, "assertOwnerDispatchSupported"`,
		`Object.defineProperty(globalThis.omnivm, "assertOwnerDispatchTargetSupported"`,
		`Object.defineProperty(globalThis.omnivm, "rubyThreadingStatus"`,
		`Object.defineProperty(globalThis.omnivm, "assertRubyNativeThreadsSupported"`,
		`globalThis.__omnivm_owner_dispatch_contract`,
		`globalThis.__omnivm_ruby_threading_contract`,
		`owner_dispatch_supported: false`,
		`native_threads_supported: false`,
		`javascript_event_loop`,
		`python_async_loop: "python_asyncio"`,
		`nodejs: "javascript_event_loop"`,
		`event_loop: "javascript_event_loop"`,
		`fiber: "ruby_fiber_thread"`,
		`thread: "ruby_fiber_thread"`,
		`owner_dispatch_target`,
		`known_targets: Object.keys(status.owner_dispatch_targets || {}).sort()`,
		`detailsSnapshot = globalThis.__omnivm_clone_json(details)`,
		`detailsJson = JSON.stringify(detailsSnapshot)`,
		`var originRuntime = "javascript"`,
		`var boundaryValue = boundaryPath`,
		`var originalErrorHandle = null`,
		`err.toJSON = function()`,
		`Object.defineProperty(err, "origin_runtime"`,
		`Object.defineProperty(err, "originRuntime"`,
		`Object.defineProperty(err, "boundary_path"`,
		`Object.defineProperty(err, "boundaryPath"`,
		`Object.defineProperty(err, "original_error_handle"`,
		`Object.defineProperty(err, "originalErrorHandle"`,
		`Object.defineProperty(err, "stack_frames"`,
		`get: function() { return stackFrames.slice(); }`,
		`Object.defineProperty(err, "cause_chain"`,
		`get: function() { return globalThis.__omnivm_clone_json(causeChain); }`,
		`Object.defineProperty(err, "details"`,
		`get: function() { return globalThis.__omnivm_clone_json(detailsSnapshot); }`,
		`if (typeof value === 'string')`,
		`detailsSnapshot = globalThis.__omnivm_clone_json(JSON.parse(value));`,
		`err.details_json = value;`,
		`stack_frames: stackFrames.slice()`,
		`cause_chain: globalThis.__omnivm_clone_json(causeChain)`,
		`details: globalThis.__omnivm_clone_json(detailsSnapshot)`,
		`origin_runtime: originRuntime`,
		`boundary_path: boundaryValue`,
		`original_error_handle: originalErrorHandle`,
		`Object.defineProperty(globalThis.omnivm, "bufferOwner"`,
		"globalThis.__omnivm_BufferOwner",
		"globalThis.__omnivm_lifecycle_method_without_required_args",
		"method.length === 0",
		`omnivmClose = globalThis.__omnivm_lifecycle_method_without_required_args(value, "__omnivm_close")`,
		"return omnivmClose.call(value)",
		"return globalThis.omnivm.proxyClose(value)",
		"symbolDispose = Symbol.dispose ? globalThis.__omnivm_lifecycle_method_without_required_args(value, Symbol.dispose) : null",
		"symbolAsyncDispose = Symbol.asyncDispose ? globalThis.__omnivm_lifecycle_method_without_required_args(value, Symbol.asyncDispose) : null",
		"return symbolDisposeResult === undefined ? true : symbolDisposeResult",
		"return symbolAsyncDisposeResult === undefined ? true : symbolAsyncDisposeResult",
		`var close = globalThis.__omnivm_lifecycle_method_without_required_args(value, "close")`,
		"return result === undefined ? true : result",
		`var dispose = globalThis.__omnivm_lifecycle_method_without_required_args(value, "dispose")`,
		"return disposeResult === undefined ? true : disposeResult",
		`var destroy = globalThis.__omnivm_lifecycle_method_without_required_args(value, "destroy")`,
		"return destroyResult === undefined ? true : destroyResult",
		`Object.defineProperty(globalThis.omnivm, "cleanupErrors"`,
		"return Array.isArray(errors) ? errors.slice() : []",
		"globalThis.omnivm.setBuffer(this.name, this.__omnivm_data, this.__omnivm_dtype)",
		"globalThis.omnivm.releaseBuffer(this.name)",
		"__omnivm_buffer_owner_error",
		`"native_memory"`,
		`active_owner: true`,
		"cannot be re-entered after release",
		"is already active",
		"this.__omnivm_entered = false",
		"typeof result.then === 'function'",
		"return Promise.resolve(result).then(finishSuccess, finishError)",
		"bodyError.omnivmCleanupErrors",
		"register_omnivm_proxy_helpers(isolate, context)",
	} {
		if !contains(code, want) {
			t.Fatalf("V8 bridge should expose core collision-safe proxyClose helper, missing %q", want)
		}
	}
	if contains(code, "typeof value.close === 'function'") ||
		contains(code, "value.close();") ||
		contains(code, "typeof value.dispose === 'function'") ||
		contains(code, "value.dispose();") ||
		contains(code, "typeof value.destroy === 'function'") ||
		contains(code, "value.destroy();") ||
		contains(code, "typeof value.__omnivm_close === 'function'") ||
		contains(code, "value && value.__omnivm_close") ||
		contains(code, "value[Symbol.dispose]") ||
		contains(code, "value[Symbol.asyncDispose]") {
		t.Fatalf("V8 core proxyClose helper should not invoke dynamic close property lookup")
	}
}

func TestJavaRuntimeKeepsResourceDescriptorFieldsPrivate(t *testing.T) {
	var data []byte
	var err error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	code := string(data)
	if !contains(code, "private boolean isInternalDescriptorKey(Object key)") || !contains(code, "private boolean hasLocalValue(Object key)") {
		t.Fatalf("Java runtime should separate resource descriptor metadata from user-visible fields")
	}
	if !contains(code, "return hasLocalValue(key)") {
		t.Fatalf("Java containsKey fallback should not expose descriptor metadata")
	}
	if !contains(code, "private boolean isMissingBridgeError(RuntimeException err)") || !contains(code, "if (!isMissingBridgeError(err))") {
		t.Fatalf("Java runtime should propagate owner lifecycle errors while preserving ordinary missing-field fallbacks")
	}
	if contains(code, "if (value.containsKey(key)) {\n                return value.get(key);\n            }\n            Map<?, ?> report = record(\"property\");") {
		t.Fatalf("Java get should not return descriptor fields before consulting the handle bridge")
	}
}

func TestJavaRuntimeDoesNotRematerializeStreamChunkProxies(t *testing.T) {
	var data []byte
	var err error
	for _, path := range []string{"../../runtime/java/OmniVM.java", "/tmp/java-src/OmniVM.java"} {
		data, err = os.ReadFile(path)
		if err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("read Java runtime helper: %v", err)
	}
	code := string(data)
	if !contains(code, "if (value instanceof HandleProxy || value instanceof StreamProxy)") || !contains(code, "return value;") {
		t.Fatalf("Java stream chunks should return already-materialized proxies before Map fallback")
	}
}

func TestWrapJavaCapturesClearsTemporaryCaptures(t *testing.T) {
	code := wrapJavaCaptures("import java.util.*;\nObject req = omnivm.OmniVM.getCapture(\"req\");", map[string]string{
		"req": `{"__omnivm_resource__":true,"id":7,"runtime":"java","kind":"request","closed":false}`,
	})
	if !strings.HasPrefix(code, "import java.util.*;") {
		t.Fatalf("Java imports should remain before generated statements, got %q", code)
	}
	if !contains(code, "try {") || !contains(code, "} finally {") || !contains(code, `omnivm.OmniVM.clearCapture("req");`) {
		t.Fatalf("Java wrapped captures should clear temporary captures in finally, got %q", code)
	}
	if strings.Index(code, `omnivm.OmniVM.setCapture("req",`) > strings.Index(code, "try {") {
		t.Fatalf("Java capture setup should run before user body, got %q", code)
	}
}

func TestJavaCaptureInjectionCleanupCanSkipBind(t *testing.T) {
	injection := javaCaptureInjection(map[string]string{
		"out": `{"name":"result"}`,
		"req": `{"__omnivm_resource__":true,"id":7,"runtime":"java","kind":"request","closed":false}`,
	})
	cleanup := injection.javaCleanupCode("out")
	if contains(cleanup, `clearCapture("out")`) {
		t.Fatalf("Java cleanup should skip excluded bind name, got %q", cleanup)
	}
	if !contains(cleanup, `clearCapture("req")`) {
		t.Fatalf("Java cleanup should clear other temporary captures, got %q", cleanup)
	}
}

func TestOpEvalJavaCleansTemporaryCaptures(t *testing.T) {
	e, mocks := makeExecutor("java")
	e.setBinding("req", map[string]interface{}{"path": "/cleanup"})
	mocks["java"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Value: `{"ok":true}`}
	}

	_, err := e.opEval(&Op{
		OpType:  "eval",
		Runtime: "java",
		Code:    `omnivm.OmniVM.getCapture("req")`,
		Bind:    "out",
		Captures: map[string]string{
			"req": "req",
		},
	})
	if err != nil {
		t.Fatalf("opEval failed: %v", err)
	}

	joined := strings.Join(mocks["java"].execCalls, "\n")
	if !contains(joined, `omnivm.OmniVM.setCapture("req",`) {
		t.Fatalf("Java eval should inject temporary capture, got %q", joined)
	}
	if !contains(joined, `omnivm.OmniVM.clearCapture("req");`) {
		t.Fatalf("Java eval should clean temporary capture after use, got %q", joined)
	}
	if strings.LastIndex(joined, `omnivm.OmniVM.clearCapture("req");`) < strings.Index(joined, `omnivm.OmniVM.setCaptureObject("out",`) {
		t.Fatalf("Java eval cleanup should run after user assignment, got %q", joined)
	}
}

func TestOpEvalJavaCaptureCleanupPreservesBind(t *testing.T) {
	e, mocks := makeExecutor("java")
	e.setBinding("req", map[string]interface{}{"path": "/preserve"})
	mocks["java"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Value: `{"ok":true}`}
	}

	_, err := e.opEval(&Op{
		OpType:  "eval",
		Runtime: "java",
		Code:    `omnivm.OmniVM.getCapture("req")`,
		Bind:    "req",
		Captures: map[string]string{
			"req": "req",
		},
	})
	if err != nil {
		t.Fatalf("opEval failed: %v", err)
	}

	joined := strings.Join(mocks["java"].execCalls, "\n")
	if !contains(joined, `omnivm.OmniVM.setCaptureObject("req",`) {
		t.Fatalf("Java eval should persist the bound result, got %q", joined)
	}
	if contains(joined, `omnivm.OmniVM.clearCapture("req");`) {
		t.Fatalf("Java cleanup should not clear the persistent bind name, got %q", joined)
	}
}

func TestWrapEmptyCaptures(t *testing.T) {
	code := wrapPythonCaptures("print(1)", map[string]string{})
	if code != "print(1)" {
		t.Errorf("empty captures should return code as-is, got %q", code)
	}
}

func TestResolveRuntimeRefCaptureCopiesPrimitives(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	mocks["javascript"].evalFn = func(code string) pkg.Result {
		if strings.Contains(code, "primitive") {
			return pkg.Result{Value: `{"primitive":true,"value":7}`}
		}
		return pkg.Result{Value: "false"}
	}
	jsonVal, err := e.resolveRuntimeRefCapture("score", "python", RuntimeRef{Runtime: "javascript", VarName: "score", Value: 7})
	if err != nil {
		t.Fatalf("resolveRuntimeRefCapture: %v", err)
	}
	if jsonVal != "7" {
		t.Fatalf("primitive RuntimeRef capture = %q, want 7", jsonVal)
	}
	stats := e.BoundaryStats()
	if stats.RuntimeSerializations != 0 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("primitive RuntimeRef stats = %+v, want typed copy without runtime serialization or proxy/fallback", stats)
	}
}

func TestResolveRuntimeRefCaptureUsesKnownPrimitiveSnapshot(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	mocks["javascript"].evalFn = func(code string) pkg.Result {
		t.Fatalf("known primitive RuntimeRef should not re-enter source runtime, got eval %q", code)
		return pkg.Result{}
	}
	jsonVal, err := e.resolveRuntimeRefCapture("payload", "python", RuntimeRef{
		Runtime:       "javascript",
		VarName:       "payload",
		Value:         nil,
		SnapshotKnown: true,
	})
	if err != nil {
		t.Fatalf("resolveRuntimeRefCapture: %v", err)
	}
	if jsonVal != "null" {
		t.Fatalf("known primitive null capture = %q, want null", jsonVal)
	}
	stats := e.BoundaryStats()
	if stats.RuntimeSerializations != 0 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("known primitive RuntimeRef stats = %+v, want typed copy without runtime serialization", stats)
	}
}

func TestResolveRuntimeRefCaptureProxiesComplexValues(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	serializationProbes := 0
	mocks["javascript"].evalFn = func(code string) pkg.Result {
		if code == "JSON.stringify(req)" {
			serializationProbes++
			return pkg.Result{Value: `{"path":"/cart","items":["one","two"]}`}
		}
		if strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `"/cart"`}
		}
		if strings.Contains(code, "function") {
			return pkg.Result{Value: "false"}
		}
		return pkg.Result{Value: `{"path":"/cart","items":["one","two"]}`}
	}
	mocks["javascript"].execFn = func(code string) pkg.Result {
		return pkg.Result{}
	}
	ref := RuntimeRef{
		Runtime: "javascript",
		VarName: "req",
		Value: map[string]interface{}{
			"path":  "/cart",
			"items": []interface{}{"one", "two"},
		},
	}
	jsonVal, err := e.resolveRuntimeRefCapture("req", "python", ref)
	if err != nil {
		t.Fatalf("resolveRuntimeRefCapture: %v", err)
	}
	if !strings.Contains(jsonVal, `"__omnivm_resource__":true`) || strings.Contains(jsonVal, `"path":"/cart"`) {
		t.Fatalf("complex RuntimeRef should cross as descriptor, not JSON copy: %s", jsonVal)
	}
	if serializationProbes != 0 {
		t.Fatalf("complex RuntimeRef capture used JSON serialization probe %d times", serializationProbes)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}
	got, ok, err := e.handleProperty(id, "path")
	if err != nil || !ok || got != "/cart" {
		t.Fatalf("RuntimeRef proxy path = (%#v,%v,%v), want /cart,true,nil", got, ok, err)
	}
	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("complex RuntimeRef stats = %+v, want resource proxy without JSON fallback", stats)
	}
}

func TestResolveRuntimeRefCaptureExportsBufferProtocolAsArrowTable(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	before := arrow.GlobalStore().Stats()
	payload := []byte("abcdef")
	mocks["python"].evalFn = func(code string) pkg.Result {
		t.Fatalf("buffer-protocol capture should not JSON-serialize source runtime, got eval %q", code)
		return pkg.Result{}
	}
	mocks["python"].exportFn = func(name, expr string) (pkg.ExportedBuffer, bool, error) {
		if expr != "payload" {
			t.Fatalf("ExportBuffer expr = %q, want payload", expr)
		}
		if _, err := arrow.GlobalStore().SetWithMetadata(name, payload, arrow.BufferMetadata{
			Dtype:     arrow.DtypeBytes,
			Format:    "C",
			Shape:     []int64{3, 2},
			Strides:   []int64{2, 1},
			ReadOnly:  true,
			Ownership: "producer",
		}); err != nil {
			return pkg.ExportedBuffer{}, false, err
		}
		return pkg.ExportedBuffer{
			Name:        name,
			Dtype:       arrow.DtypeBytes,
			ArrowFormat: "C",
			Elements:    int64(len(payload)),
			Shape:       []int64{3, 2},
			Strides:     []int64{2, 1},
			ReadOnly:    true,
		}, true, nil
	}

	jsonVal, err := e.resolveRuntimeRefCapture("payload", "javascript", RuntimeRef{
		Runtime:       "python",
		VarName:       "payload",
		SnapshotKnown: true,
		Opaque:        true,
	})
	if err != nil {
		t.Fatalf("resolveRuntimeRefCapture: %v", err)
	}
	if !strings.Contains(jsonVal, `"__omnivm_table__":true`) || !strings.Contains(jsonVal, `"arrow_c_data"`) || !strings.Contains(jsonVal, `"buffer"`) || !strings.Contains(jsonVal, `"memory_space":"host"`) {
		t.Fatalf("buffer-protocol RuntimeRef should cross as Arrow table descriptor, got %s", jsonVal)
	}
	if len(mocks["python"].exports) != 1 {
		t.Fatalf("ExportBuffer calls = %d, want 1", len(mocks["python"].exports))
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("buffer-protocol RuntimeRef stats = %+v, want Arrow table without proxy or JSON fallback", stats)
	}

	var tableID handles.ID
	for id := range e.tables {
		tableID = id
	}
	if tableID == 0 {
		t.Fatalf("buffer-protocol capture did not register table handle")
	}
	table := e.tables[tableID]
	if table.Metadata == nil || len(table.Metadata.Shape) != 2 || table.Metadata.Shape[0] != 3 || table.Metadata.Shape[1] != 2 || len(table.Metadata.Strides) != 2 || table.Metadata.Strides[0] != 2 || table.Metadata.Strides[1] != 1 || !table.Metadata.ReadOnly || table.Metadata.MemorySpace != "host" {
		t.Fatalf("buffer-protocol table metadata not preserved: %+v", table.Metadata)
	}
	result, err := e.HandleCall(`{"op":"handle_len","id":` + strconv.FormatUint(uint64(tableID), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_len: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(3) {
		t.Fatalf("buffer-protocol shaped len envelope = %#v, want 3 rows", env)
	}
	result, err = e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	row, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(row) != 2 || row[0] != float64(payload[2]) || row[1] != float64(payload[3]) {
		t.Fatalf("buffer-protocol shaped index envelope = %#v, want second row", env)
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	rows, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(rows) != 3 {
		t.Fatalf("buffer-protocol shaped iter envelope = %#v", env)
	}
	firstRow, ok := rows[0].([]interface{})
	if !ok || len(firstRow) != 2 || firstRow[0] != float64(payload[0]) || firstRow[1] != float64(payload[1]) {
		t.Fatalf("buffer-protocol first iter row = %#v", rows[0])
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("scope release did not free automatic runtime buffer: before=%+v after=%+v", before, released)
	}
}

func TestStridedTableBufferProxyUsesShapeStrides(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	name := "test_strided_table_buffer_proxy"
	_ = arrow.GlobalStore().Free(name)
	before := arrow.GlobalStore().Stats()
	payload := []byte{2, 1, 0, 0, 4, 3, 0, 0, 6, 5}
	if _, err := arrow.GlobalStore().SetWithMetadata(name, payload, arrow.BufferMetadata{
		Dtype:     arrow.DtypeU16,
		Format:    "S",
		Shape:     []int64{3},
		Strides:   []int64{4},
		ReadOnly:  true,
		Ownership: "producer",
	}); err != nil {
		t.Fatalf("SetWithMetadata: %v", err)
	}

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
	var id handles.ID
	var err error
	id, err = e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    "table:" + ref.Format,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			ref.Released = true
			return arrow.GlobalStore().Free(name)
		},
	})
	if err != nil {
		t.Fatalf("register table handle: %v", err)
	}
	ref.ID = id
	e.tables[id] = ref

	result, err := e.HandleCall(`{"op":"handle_len","id":` + strconv.FormatUint(uint64(id), 10) + `}`)
	if err != nil {
		t.Fatalf("HandleCall handle_len: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(3) {
		t.Fatalf("strided table len envelope = %#v, want 3", env)
	}
	result, err = e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64(772) {
		t.Fatalf("strided table index envelope = %#v, want 772", env)
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(id), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	values, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(values) != 3 || values[0] != float64(258) || values[1] != float64(772) || values[2] != float64(1286) {
		t.Fatalf("strided table iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("scope release did not free strided Arrow buffer: before=%+v after=%+v", before, released)
	}
}

func TestNullableTableBufferProxyReturnsNulls(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	name := "test_nullable_table_buffer_proxy"
	_ = arrow.GlobalStore().Free(name)
	before := arrow.GlobalStore().Stats()
	payload := []byte{
		1, 0, 0, 0,
		2, 0, 0, 0,
		3, 0, 0, 0,
	}
	validity := []byte{0b00000101}
	if _, err := arrow.GlobalStore().SetWithValidityMetadata(name, payload, validity, arrow.BufferMetadata{
		Dtype:             arrow.DtypeI32,
		Format:            "i",
		Shape:             []int64{3},
		NullCount:         1,
		ValidityBytes:     1,
		ValidityBitOffset: 0,
		ReadOnly:          true,
		Ownership:         "producer",
	}); err != nil {
		t.Fatalf("SetWithValidityMetadata: %v", err)
	}

	dtype := int32(arrow.DtypeI32)
	nullCount := int64(1)
	ref := &TableRef{
		Runtime:   "python",
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: "i",
			Buffer:      name,
			Shape:       []int64{3},
			NullCount:   &nullCount,
			ReadOnly:    true,
		},
		Value: name,
	}
	id, err := e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    "table:" + ref.Format,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			ref.Released = true
			return arrow.GlobalStore().Free(name)
		},
	})
	if err != nil {
		t.Fatalf("register table handle: %v", err)
	}
	ref.ID = id
	e.tables[id] = ref

	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "null" || env.Value != nil {
		t.Fatalf("nullable table index envelope = %#v, want null", env)
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(id), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	values, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(values) != 3 || values[0] != float64(1) || values[1] != nil || values[2] != float64(3) {
		t.Fatalf("nullable table iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("scope release did not free nullable Arrow buffer: before=%+v after=%+v", before, released)
	}
}

func TestNegativeStridedTableBufferProxyUsesOffset(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	name := "test_negative_strided_table_buffer_proxy"
	_ = arrow.GlobalStore().Free(name)
	before := arrow.GlobalStore().Stats()
	payload := []byte{'b', 'c', 'd', 'e', 'f'}
	if _, err := arrow.GlobalStore().SetWithMetadata(name, payload, arrow.BufferMetadata{
		Dtype:     arrow.DtypeU8,
		Format:    "C",
		Shape:     []int64{3},
		Strides:   []int64{-2},
		Offset:    4,
		ReadOnly:  true,
		Ownership: "producer",
	}); err != nil {
		t.Fatalf("SetWithMetadata: %v", err)
	}

	dtype := int32(arrow.DtypeU8)
	ref := &TableRef{
		Runtime:   "python",
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: "C",
			Buffer:      name,
			Shape:       []int64{3},
			Strides:     []int64{-2},
			Offset:      4,
			ReadOnly:    true,
		},
		Value: name,
	}
	var id handles.ID
	var err error
	id, err = e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    "table:" + ref.Format,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			ref.Released = true
			return arrow.GlobalStore().Free(name)
		},
	})
	if err != nil {
		t.Fatalf("register table handle: %v", err)
	}
	ref.ID = id
	e.tables[id] = ref

	result, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":0}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "number" || env.Value != float64('f') {
		t.Fatalf("negative-strided table index envelope = %#v, want %d", env, 'f')
	}
	result, err = e.HandleCall(`{"op":"handle_iter","id":` + strconv.FormatUint(uint64(id), 10) + `,"mode":"values"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_iter: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	values, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(values) != 3 || values[0] != float64('f') || values[1] != float64('d') || values[2] != float64('b') {
		t.Fatalf("negative-strided table iter envelope = %#v", env)
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("scope release did not free negative-strided Arrow buffer: before=%+v after=%+v", before, released)
	}
}

func TestResolveRuntimeRefCaptureProxiesOpaqueValuesDespitePrimitiveSnapshot(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	mocks["python"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Err: errors.New("not JSON serializable")}
	}
	ref := RuntimeRef{Runtime: "python", VarName: "handler", Value: "<Handler object>"}
	jsonVal, err := e.resolveRuntimeRefCapture("handler", "javascript", ref)
	if err != nil {
		t.Fatalf("resolveRuntimeRefCapture: %v", err)
	}
	if !strings.Contains(jsonVal, `"__omnivm_resource__":true`) || strings.Contains(jsonVal, "Handler object") {
		t.Fatalf("opaque RuntimeRef should cross as live proxy, not cached primitive snapshot: %s", jsonVal)
	}
	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("opaque RuntimeRef stats = %+v, want resource proxy without JSON fallback", stats)
	}
}

func TestAutoInjectScopeProxiesRuntimeRefWhenSerializationFails(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	mocks["javascript"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Value: ""}
	}
	e.setBinding("handler", RuntimeRef{Runtime: "javascript", VarName: "handler", Value: nil})

	code := e.autoInjectScope("python")
	if !contains(code, "handler = __omnivm_materialize_capture(") || !contains(code, "__omnivm_resource__") {
		t.Errorf("autoInjectScope did not inject proxied handler capture: %q", code)
	}
	if contains(code, "__json.loads('null')") {
		t.Errorf("autoInjectScope should not copy failed opaque RuntimeRef as null: %q", code)
	}
	stats := e.BoundaryStats()
	if stats.ResourceProxyCaptures != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("opaque RuntimeRef stats = %+v, want resource proxy without JSON fallback", stats)
	}
}

func TestRuntimeRefProxyReleaseDropsGeneratedArgRefs(t *testing.T) {
	cases := []struct {
		runtimeName string
		varName     string
		want        string
	}{
		{"javascript", `__omnivm_arg_refs["arg_1"]`, `delete globalThis.__omnivm_arg_refs["arg_1"]`},
		{"python", `__omnivm_arg_refs['arg_2']`, `pop("arg_2", None)`},
		{"ruby", `$__omnivm_arg_refs["arg_3"]`, `.delete("arg_3")`},
		{"java", `__omnivm_arg_refs["arg_4"]`, `releaseArgRef("arg_4")`},
	}
	for _, tc := range cases {
		t.Run(tc.runtimeName, func(t *testing.T) {
			e, mocks := makeExecutor(tc.runtimeName)
			jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: tc.runtimeName, VarName: tc.varName})
			if err != nil {
				t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
			}
			var descriptor map[string]interface{}
			if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
				t.Fatalf("descriptor json: %v", err)
			}
			id, err := bridgeHandleID(descriptor["id"])
			if err != nil {
				t.Fatalf("descriptor id: %v", err)
			}
			if err := e.ensureHandleTable().Release(id); err != nil {
				t.Fatalf("release runtime ref handle: %v", err)
			}
			if !containsExecCall(mocks[tc.runtimeName].execCalls, tc.want) {
				t.Fatalf("runtime arg ref cleanup missing %q in calls %q", tc.want, mocks[tc.runtimeName].execCalls)
			}
		})
	}
}

func TestRuntimeRefProxyScopeReleaseDropsGeneratedArgRefsAndCaches(t *testing.T) {
	e, mocks := makeExecutor("python")
	ref := RuntimeRef{Runtime: "python", VarName: "__omnivm_arg_refs['arg_7']"}
	jsonVal, err := e.runtimeRefProxyCaptureJSON(ref)
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}
	if e.resources[id] == nil {
		t.Fatalf("runtime ref resource was not cached")
	}
	if cached, ok := e.bridgeHandleForValue("python", ref); !ok || cached != id {
		t.Fatalf("runtime ref identity cache = %d/%v, want %d/true", cached, ok, id)
	}

	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	if e.resources[id] != nil {
		t.Fatalf("released runtime ref resource stayed cached: %#v", e.resources[id])
	}
	if _, ok := e.bridgeHandles[bridgeIdentity{typ: "RuntimeRef", key: "python\x00__omnivm_arg_refs['arg_7']"}]; ok {
		t.Fatalf("released runtime ref identity stayed cached")
	}
	if len(mocks["python"].execCalls) != 1 || !strings.Contains(mocks["python"].execCalls[0], `pop("arg_7", None)`) {
		t.Fatalf("runtime arg ref release calls = %#v", mocks["python"].execCalls)
	}
}

func TestBridgeResultScopeReleaseDropsResourceIdentityCache(t *testing.T) {
	e, _ := makeExecutor("go")
	parent := &ResourceRef{Runtime: "go", Kind: "root"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: "go",
		Kind:    "root",
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			parent.Closed = true
			e.forgetReleasedHandle(parent.ID, parent)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent
	child := map[string]interface{}{"name": "child"}
	wrapped, err := e.bridgeResultValue(parentID, child)
	if err != nil {
		t.Fatalf("bridgeResultValue: %v", err)
	}
	descriptor, ok := wrapped.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("bridgeResultValue = %#v, want resource descriptor", wrapped)
	}
	childID, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("child descriptor id: %v", err)
	}
	if e.resources[childID] == nil {
		t.Fatalf("child resource was not cached")
	}
	if cached, ok := e.bridgeHandleForValue("go", child); !ok || cached != childID {
		t.Fatalf("child identity cache = %d/%v, want %d/true", cached, ok, childID)
	}

	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	if e.resources[childID] != nil {
		t.Fatalf("released child resource stayed cached: %#v", e.resources[childID])
	}
	if cached, ok := e.bridgeHandleForValue("go", child); ok {
		t.Fatalf("released child identity cache = %d/true, want missing", cached)
	}
	if stats := e.ensureHandleTable().Stats(time.Now()); stats.Live != 0 {
		t.Fatalf("handles still live after scope release: %+v", stats)
	}
}

func TestBridgeResultExistingHandlesRecordReferenceEdges(t *testing.T) {
	e, _ := makeExecutor("python", "javascript", "java", "ruby")
	parent := &ResourceRef{Runtime: "python", Kind: "root"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	child := &ResourceRef{Runtime: "javascript", Kind: "object"}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child

	table := &TableRef{Runtime: "java", Format: "arrow/c-data", Ownership: "producer"}
	tableID, err := e.ensureHandleTable().Register(table, handles.RegisterOptions{
		Runtime: table.Runtime,
		Kind:    "table",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register table: %v", err)
	}
	table.ID = tableID
	e.tables[tableID] = table

	job := &JobHandle{Runtime: "ruby", Kind: "job"}
	jobID, err := e.ensureHandleTable().Register(job, handles.RegisterOptions{
		Runtime: job.Runtime,
		Kind:    job.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register job: %v", err)
	}
	job.ID = int(jobID)

	for name, value := range map[string]interface{}{
		"resource": child,
		"table":    table,
		"job":      job,
	} {
		wrapped, err := e.bridgeResultValue(parentID, value)
		if err != nil {
			t.Fatalf("bridgeResultValue %s: %v", name, err)
		}
		descriptor, ok := wrapped.(map[string]interface{})
		if !ok {
			t.Fatalf("bridgeResultValue %s = %T, want descriptor map", name, wrapped)
		}
		if _, ok := descriptor["id"]; !ok {
			t.Fatalf("bridgeResultValue %s descriptor missing id: %#v", name, descriptor)
		}
	}

	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["property"] != 3 {
		t.Fatalf("property reference edges = %+v, want 3", stats)
	}
	for _, runtimePair := range []string{"python->javascript", "python->java", "python->ruby"} {
		if stats.ReferenceEdgesByRuntime[runtimePair] != 1 {
			t.Fatalf("reference edges by runtime = %+v, want %s", stats.ReferenceEdgesByRuntime, runtimePair)
		}
	}
}

func TestBridgeResultNewProxyRecordsNestedHandleEdges(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	parent := &ResourceRef{Runtime: "python", Kind: "root"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	child := &ResourceRef{Runtime: "javascript", Kind: "object"}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child

	wrapped, err := e.bridgeResultValue(parentID, map[string]interface{}{
		"child": child,
		"items": []interface{}{
			resourceProxyValue(child),
		},
	})
	if err != nil {
		t.Fatalf("bridgeResultValue nested container: %v", err)
	}
	descriptor, ok := wrapped.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("bridgeResultValue = %#v, want resource descriptor", wrapped)
	}
	containerID, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("container descriptor id: %v", err)
	}

	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["property"] != 2 {
		t.Fatalf("property reference edges = %+v, want parent->container and container->child", stats)
	}
	if stats.ReferenceEdgesByRuntime["python->python"] != 1 || stats.ReferenceEdgesByRuntime["python->javascript"] != 1 {
		t.Fatalf("reference edges by runtime = %+v, want python->python and python->javascript", stats.ReferenceEdgesByRuntime)
	}

	if err := e.ensureHandleTable().ReleaseAllRefs(containerID); err != nil {
		t.Fatalf("release container: %v", err)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdges != 0 {
		t.Fatalf("container release should drop nested and parent graph edges: %+v", stats)
	}
}

func TestBridgeResultNewProxyRecordsTypedNestedHandleEdges(t *testing.T) {
	type typedPayload struct {
		Primary *ResourceRef
		Others  map[string]*ResourceRef
		List    []ResourceRef
	}

	e, _ := makeExecutor("python", "javascript", "ruby")
	parent := &ResourceRef{Runtime: "python", Kind: "root"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	first := &ResourceRef{Runtime: "javascript", Kind: "object"}
	firstID, err := e.ensureHandleTable().Register(first, handles.RegisterOptions{
		Runtime: first.Runtime,
		Kind:    first.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register first: %v", err)
	}
	first.ID = firstID
	e.resources[firstID] = first

	second := &ResourceRef{Runtime: "ruby", Kind: "object"}
	secondID, err := e.ensureHandleTable().Register(second, handles.RegisterOptions{
		Runtime: second.Runtime,
		Kind:    second.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register second: %v", err)
	}
	second.ID = secondID
	e.resources[secondID] = second

	wrapped, err := e.bridgeResultValue(parentID, typedPayload{
		Primary: first,
		Others:  map[string]*ResourceRef{"second": second},
		List:    []ResourceRef{*second},
	})
	if err != nil {
		t.Fatalf("bridgeResultValue typed nested container: %v", err)
	}
	descriptor, ok := wrapped.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("bridgeResultValue = %#v, want resource descriptor", wrapped)
	}

	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["property"] != 3 {
		t.Fatalf("property reference edges = %+v, want parent->container plus two typed children", stats)
	}
	for _, runtimePair := range []string{"python->python", "python->javascript", "python->ruby"} {
		if stats.ReferenceEdgesByRuntime[runtimePair] != 1 {
			t.Fatalf("reference edges by runtime = %+v, want %s", stats.ReferenceEdgesByRuntime, runtimePair)
		}
	}
}

func TestHandleSetDropsStaleTypedContainerEdgesOnOverwrite(t *testing.T) {
	type typedPayload struct {
		Child *ResourceRef
	}

	e, _ := makeExecutor("python", "javascript")
	store := map[string]interface{}{}
	storeID, err := e.ensureHandleTable().Register(store, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "map",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register store: %v", err)
	}

	child := &ResourceRef{Runtime: "javascript", Kind: "object"}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child

	ok, err := e.handleSetForProxy(storeID, "payload", typedPayload{Child: child})
	if err != nil || !ok {
		t.Fatalf("handleSetForProxy typed payload = %v, %v", ok, err)
	}
	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 1 {
		t.Fatalf("typed payload mutation edge not recorded: %+v", stats)
	}

	ok, err = e.handleSetForProxy(storeID, "payload", "primitive")
	if err != nil || !ok {
		t.Fatalf("handleSetForProxy primitive = %v, %v", ok, err)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdges != 0 {
		t.Fatalf("typed payload overwrite did not drop stale edge: %+v", stats)
	}
}

func TestTypedMapKeyReferencesAreTracked(t *testing.T) {
	type typedPayload struct {
		ByKey map[*ResourceRef]string
	}

	e, _ := makeExecutor("python", "javascript")
	parent := &ResourceRef{Runtime: "python", Kind: "root"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	child := &ResourceRef{Runtime: "javascript", Kind: "object"}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child

	if _, err := e.bridgeResultValue(parentID, typedPayload{
		ByKey: map[*ResourceRef]string{child: "value"},
	}); err != nil {
		t.Fatalf("bridgeResultValue typed key container: %v", err)
	}
	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["property"] != 2 {
		t.Fatalf("typed map key property edges = %+v, want parent->container plus container->child", stats)
	}
	if stats.ReferenceEdgesByRuntime["python->javascript"] != 1 {
		t.Fatalf("typed map key child edge = %+v, want python->javascript", stats.ReferenceEdgesByRuntime)
	}

	store := map[string]interface{}{}
	storeID, err := e.ensureHandleTable().Register(store, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "map",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register store: %v", err)
	}
	ok, err := e.handleSetForProxy(storeID, "payload", typedPayload{
		ByKey: map[*ResourceRef]string{child: "value"},
	})
	if err != nil || !ok {
		t.Fatalf("handleSetForProxy typed key payload = %v, %v", ok, err)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 1 {
		t.Fatalf("typed map key mutation edge not recorded: %+v", stats)
	}
	ok, err = e.handleSetForProxy(storeID, "payload", "primitive")
	if err != nil || !ok {
		t.Fatalf("handleSetForProxy primitive = %v, %v", ok, err)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 0 {
		t.Fatalf("typed map key overwrite did not drop stale mutation edge: %+v", stats)
	}
}

func TestTypedReferenceTraversalHandlesCycles(t *testing.T) {
	type cyclicPayload struct {
		Name  string
		Child *ResourceRef
		Next  *cyclicPayload
	}

	e, _ := makeExecutor("python", "javascript")
	parent := &ResourceRef{Runtime: "python", Kind: "root"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	child := &ResourceRef{Runtime: "javascript", Kind: "object"}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child

	payload := &cyclicPayload{Name: "root", Child: child}
	payload.Next = payload

	if _, err := e.bridgeResultValue(parentID, payload); err != nil {
		t.Fatalf("bridgeResultValue cyclic payload: %v", err)
	}
	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["property"] != 2 {
		t.Fatalf("cyclic payload property edges = %+v, want parent->container plus container->child", stats)
	}
	if stats.ReferenceEdgesByRuntime["python->javascript"] != 1 {
		t.Fatalf("cyclic payload child edge = %+v, want python->javascript", stats.ReferenceEdgesByRuntime)
	}

	store := map[string]interface{}{}
	storeID, err := e.ensureHandleTable().Register(store, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "map",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register store: %v", err)
	}
	ok, err := e.handleSetForProxy(storeID, "payload", payload)
	if err != nil || !ok {
		t.Fatalf("handleSetForProxy cyclic payload = %v, %v", ok, err)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 1 {
		t.Fatalf("cyclic payload mutation edge not recorded: %+v", stats)
	}
	ok, err = e.handleSetForProxy(storeID, "payload", "primitive")
	if err != nil || !ok {
		t.Fatalf("handleSetForProxy primitive = %v, %v", ok, err)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["mutation"] != 0 {
		t.Fatalf("cyclic payload overwrite did not drop stale mutation edge: %+v", stats)
	}
}

func TestBridgeResultMarkerDescriptorsRecordReferenceEdges(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	parent := &ResourceRef{Runtime: "python", Kind: "root"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: parent.Runtime,
		Kind:    parent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	child := &ResourceRef{Runtime: "javascript", Kind: "object"}
	childID, err := e.ensureHandleTable().Register(child, handles.RegisterOptions{
		Runtime: child.Runtime,
		Kind:    child.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register child: %v", err)
	}
	child.ID = childID
	e.resources[childID] = child

	wrapped, err := e.bridgeResultValue(parentID, resourceProxyValue(child))
	if err != nil {
		t.Fatalf("bridgeResultValue marker: %v", err)
	}
	if descriptor, ok := wrapped.(map[string]interface{}); !ok || descriptor["id"] != childID {
		t.Fatalf("bridgeResultValue marker = %#v, want child descriptor", wrapped)
	}
	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["property"] != 1 || stats.ReferenceEdgesByRuntime["python->javascript"] != 1 {
		t.Fatalf("direct marker did not record reference edge: %+v", stats)
	}

	if err := e.ensureHandleTable().ReleaseAllRefs(parentID); err != nil {
		t.Fatalf("release parent: %v", err)
	}

	nextParent := &ResourceRef{Runtime: "python", Kind: "root"}
	nextParentID, err := e.ensureHandleTable().Register(nextParent, handles.RegisterOptions{
		Runtime: nextParent.Runtime,
		Kind:    nextParent.Kind,
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register next parent: %v", err)
	}
	nextParent.ID = nextParentID
	e.resources[nextParentID] = nextParent

	wrapped, err = e.bridgeListValue(nextParentID, []interface{}{resourceProxyValue(child)})
	if err != nil {
		t.Fatalf("bridgeListValue marker: %v", err)
	}
	if values, ok := wrapped.([]interface{}); !ok || len(values) != 1 {
		t.Fatalf("bridgeListValue marker = %#v, want one descriptor", wrapped)
	}
	stats = e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByKind["property"] != 1 || stats.ReferenceEdgesByRuntime["python->javascript"] != 1 {
		t.Fatalf("nested marker did not record reference edge: %+v", stats)
	}
}

func TestBridgeResultRuntimeRefPreservesSourceRuntime(t *testing.T) {
	e, _ := makeExecutor("javascript", "python")
	parent := &ResourceRef{Runtime: "javascript", Kind: "stream"}
	parentID, err := e.ensureHandleTable().Register(parent, handles.RegisterOptions{
		Runtime: "javascript",
		Kind:    "stream",
		ScopeID: e.currentHandleScope(),
	})
	if err != nil {
		t.Fatalf("register parent: %v", err)
	}
	parent.ID = parentID
	e.resources[parentID] = parent

	wrapped, err := e.bridgeResultValue(parentID, RuntimeRef{
		Runtime: "python",
		VarName: "__omnivm_arg_refs['arg_3']",
		Value:   map[string]interface{}{"name": "request"},
	})
	if err != nil {
		t.Fatalf("bridgeResultValue: %v", err)
	}
	descriptor, ok := wrapped.(map[string]interface{})
	if !ok || descriptor["__omnivm_resource__"] != true {
		t.Fatalf("bridgeResultValue = %#v, want resource descriptor", wrapped)
	}
	if descriptor["runtime"] != "python" || descriptor["kind"] != "runtime_ref" {
		t.Fatalf("descriptor = %#v, want python runtime_ref", descriptor)
	}
	childID, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("child descriptor id: %v", err)
	}
	entry, ok := e.ensureHandleTable().Get(childID)
	if !ok || entry.Runtime != "python" {
		t.Fatalf("child entry = %+v/%v, want python", entry, ok)
	}
	stats := e.ensureHandleTable().Stats(time.Now())
	if stats.ReferenceEdgesByRuntime["javascript->python"] != 1 {
		t.Fatalf("reference edges = %+v, want javascript->python", stats.ReferenceEdgesByRuntime)
	}
}

func TestRuntimeRefProxyReleaseIgnoresArbitraryRuntimeRefs(t *testing.T) {
	e, mocks := makeExecutor("python")
	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "python", VarName: "request"})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor json: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}
	if err := e.ensureHandleTable().Release(id); err != nil {
		t.Fatalf("release runtime ref handle: %v", err)
	}
	if len(mocks["python"].execCalls) != 0 {
		t.Fatalf("arbitrary runtime ref should not be deleted, calls=%q", mocks["python"].execCalls)
	}
}

func TestRuntimeRefProxyReadsLiveRuntimeProperty(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	var execCode string
	mocks["python"].execFn = func(code string) pkg.Result {
		execCode = code
		return pkg.Result{}
	}
	mocks["python"].evalFn = func(code string) pkg.Result {
		if strings.Contains(code, "primitive") && strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `{"primitive":true,"value":"/live"}`}
		}
		if strings.Contains(code, "callable(") {
			return pkg.Result{Value: "false"}
		}
		if strings.Contains(code, "json") && strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `"/live"`}
		}
		return pkg.Result{Value: `{"path":"/cached"}`}
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{
		Runtime: "python",
		VarName: "req",
		Value: map[string]interface{}{
			"path": "/cached",
		},
	})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}
	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"path"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "/live" {
		t.Fatalf("live RuntimeRef property envelope = %#v, want /live", env)
	}
	if !strings.Contains(execCode, "req") || strings.Contains(execCode, "/cached") {
		t.Fatalf("property read should execute against live runtime variable, got %q", execCode)
	}
}

func TestRuntimeRefProxyComplexPropertyStaysProxied(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	mocks["python"].execFn = func(code string) pkg.Result {
		return pkg.Result{}
	}
	mocks["python"].evalFn = func(code string) pkg.Result {
		if strings.Contains(code, "primitive") && strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `{"primitive":false,"callable":false}`}
		}
		if strings.Contains(code, "callable(") {
			return pkg.Result{Value: "false"}
		}
		if strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `["first","second"]`}
		}
		return pkg.Result{Value: `{"items":["cached"]}`}
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "python", VarName: "req", Value: nil})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"items"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	itemsProxy, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || itemsProxy["__omnivm_resource__"] != true {
		t.Fatalf("complex live RuntimeRef property should return proxy descriptor, got %#v", env)
	}
	itemsID, err := bridgeHandleID(itemsProxy["id"])
	if err != nil {
		t.Fatalf("items proxy id: %v", err)
	}
	entry, ok := e.handleTable.Get(itemsID)
	if !ok {
		t.Fatalf("items proxy handle was not registered")
	}
	child, ok := runtimeRefFromHandleValue(entry.Value)
	if !ok || child.Runtime != "python" || !strings.HasPrefix(child.VarName, "__omnivm_ref_") {
		t.Fatalf("items proxy value = %#v, want live RuntimeRef child", entry.Value)
	}
}

func TestRuntimeRefProxyBufferPropertyExportsAsArrowTable(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	payload := []byte("abcdef")
	mocks["python"].execFn = func(code string) pkg.Result {
		return pkg.Result{}
	}
	mocks["python"].evalFn = func(code string) pkg.Result {
		if strings.Contains(code, "primitive") && strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `{"primitive":false,"callable":false}`}
		}
		if strings.Contains(code, "callable(") {
			return pkg.Result{Value: "false"}
		}
		return pkg.Result{Value: `{"primitive":false,"callable":false}`}
	}
	mocks["python"].exportFn = func(name, expr string) (pkg.ExportedBuffer, bool, error) {
		if !strings.HasPrefix(expr, "__omnivm_ref_") {
			t.Fatalf("ExportBuffer expr = %q, want generated runtime ref", expr)
		}
		if _, err := arrow.GlobalStore().SetWithMetadata(name, payload, arrow.BufferMetadata{
			Dtype:     arrow.DtypeBytes,
			Format:    "C",
			Shape:     []int64{3, 2},
			Strides:   []int64{2, 1},
			ReadOnly:  true,
			Ownership: "producer",
		}); err != nil {
			return pkg.ExportedBuffer{}, false, err
		}
		return pkg.ExportedBuffer{
			Name:        name,
			Dtype:       arrow.DtypeBytes,
			ArrowFormat: "C",
			Elements:    int64(len(payload)),
			Shape:       []int64{3, 2},
			Strides:     []int64{2, 1},
			ReadOnly:    true,
		}, true, nil
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "python", VarName: "req", Value: nil})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"payload"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get payload: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	tableDescriptor, ok := env.Value.(map[string]interface{})
	if env.Kind != "json" || !ok || tableDescriptor["__omnivm_table__"] != true || tableDescriptor["format"] != "arrow_c_data" {
		t.Fatalf("buffer live RuntimeRef property should return Arrow table descriptor, got %#v", env)
	}
	if len(mocks["python"].exports) != 1 {
		t.Fatalf("ExportBuffer calls = %d, want 1", len(mocks["python"].exports))
	}
	tableID, err := bridgeHandleID(tableDescriptor["id"])
	if err != nil {
		t.Fatalf("table descriptor id: %v", err)
	}
	indexed, err := e.HandleCall(`{"op":"handle_index","id":` + strconv.FormatUint(uint64(tableID), 10) + `,"value":1}`)
	if err != nil {
		t.Fatalf("HandleCall handle_index table: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, indexed)
	row, ok := env.Value.([]interface{})
	if env.Kind != "json" || !ok || len(row) != 2 || row[0] != float64(payload[2]) || row[1] != float64(payload[3]) {
		t.Fatalf("buffer table index envelope = %#v, want second row", env)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 1 || stats.JSONFallbacks != 0 {
		t.Fatalf("buffer RuntimeRef property stats = %+v, want request proxy plus Arrow table without JSON fallback", stats)
	}
}

func TestRuntimeRefProxyCallableDispatchesLiveMethod(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	var execCode string
	mocks["python"].execFn = func(code string) pkg.Result {
		execCode = code
		return pkg.Result{}
	}
	mocks["python"].evalFn = func(code string) pkg.Result {
		if strings.Contains(code, "primitive") && strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `{"primitive":true,"value":null}`}
		}
		if strings.Contains(code, "callable(") {
			return pkg.Result{Value: "true"}
		}
		if strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `null`}
		}
		return pkg.Result{Value: `[]`}
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "python", VarName: "items", Value: []interface{}{}})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"append"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get callable: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" || !jsonEqual(env.Value, map[string]interface{}{"__omnivm_callable__": true, "key": "append"}) {
		t.Fatalf("handle_get callable envelope = %#v, want callable descriptor", env)
	}

	result, err = e.HandleCall(`{"op":"handle_call","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"append","args":["next"]}`)
	if err != nil {
		t.Fatalf("HandleCall handle_call: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "null" || env.Value != nil {
		t.Fatalf("handle_call envelope = %#v, want null append result", env)
	}
	if !strings.Contains(execCode, "items") || !strings.Contains(execCode, "append") || !strings.Contains(execCode, "next") {
		t.Fatalf("method call should execute against live runtime object, got %q", execCode)
	}
}

func TestRuntimeRefProxyLifecycleNamedPythonMethodIsContainedAndCallable(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	var evalCodes []string
	mocks["python"].execFn = func(code string) pkg.Result {
		return pkg.Result{}
	}
	mocks["python"].evalFn = func(code string) pkg.Result {
		evalCodes = append(evalCodes, code)
		switch {
		case strings.Contains(code, "hasattr(__o, __k)") && strings.Contains(code, "close"):
			return pkg.Result{Value: "true"}
		case strings.Contains(code, "callable(") && strings.Contains(code, "close"):
			return pkg.Result{Value: "true"}
		case strings.Contains(code, "primitive") && strings.Contains(code, "__omnivm_ref_"):
			return pkg.Result{Value: `{"primitive":true,"value":"closed-remotely"}`}
		default:
			return pkg.Result{Value: `{"primitive":false,"callable":false}`}
		}
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "python", VarName: "owner", Value: nil})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}

	containsResult, err := e.HandleCall(`{"op":"handle_contains","id":` + strconv.FormatUint(uint64(id), 10) + `,"value":"close"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_contains close: %v", err)
	}
	containsEnv := decodeResultEnvelopeForTest(t, containsResult)
	if containsEnv.Kind != "bool" || containsEnv.Value != true {
		t.Fatalf("handle_contains close envelope = %#v, want true", containsEnv)
	}

	getResult, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"close"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get close: %v", err)
	}
	getEnv := decodeResultEnvelopeForTest(t, getResult)
	if getEnv.Kind != "json" || !jsonEqual(getEnv.Value, map[string]interface{}{"__omnivm_callable__": true, "key": "close"}) {
		t.Fatalf("handle_get close envelope = %#v, want callable descriptor", getEnv)
	}

	callResult, err := e.HandleCall(`{"op":"handle_call","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"close","args":["user"]}`)
	if err != nil {
		t.Fatalf("HandleCall handle_call close: %v", err)
	}
	callEnv := decodeResultEnvelopeForTest(t, callResult)
	if callEnv.Kind != "string" || callEnv.Value != "closed-remotely" {
		t.Fatalf("handle_call close envelope = %#v, want remote close result", callEnv)
	}
	joinedEval := strings.Join(evalCodes, "\n")
	if !strings.Contains(joinedEval, "hasattr(__o, __k)") || !strings.Contains(joinedEval, "callable(") {
		t.Fatalf("close collision path did not exercise contains and callable probes, evals=%q", joinedEval)
	}
}

func TestRuntimeRefRubyZeroArgMethodsReadAsProperties(t *testing.T) {
	e, mocks := makeExecutor("ruby", "python")
	var execCodes []string
	primitiveSnapshots := 0
	mocks["ruby"].execFn = func(code string) pkg.Result {
		execCodes = append(execCodes, code)
		return pkg.Result{}
	}
	mocks["ruby"].evalFn = func(code string) pkg.Result {
		switch {
		case code == "false":
			return pkg.Result{Value: "false"}
		case strings.Contains(code, ".arity == 0") && strings.Contains(code, "label"):
			return pkg.Result{Value: "true"}
		case strings.Contains(code, ".arity == 0") && strings.Contains(code, "join"):
			return pkg.Result{Value: "false"}
		case strings.Contains(code, "respond_to?") && (strings.Contains(code, "join") || strings.Contains(code, "close")):
			return pkg.Result{Value: "true"}
		case strings.Contains(code, "primitive") && strings.Contains(code, "__omnivm_ref_"):
			primitiveSnapshots++
			if primitiveSnapshots == 1 {
				return pkg.Result{Value: `{"primitive":true,"value":"alpha"}`}
			}
			return pkg.Result{Value: `{"primitive":true,"value":"joined"}`}
		default:
			return pkg.Result{Value: `{"primitive":false,"callable":false}`}
		}
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "ruby", VarName: "req", Value: nil})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"label"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get label: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "alpha" {
		t.Fatalf("zero-arg Ruby method property envelope = %#v, want alpha", env)
	}

	result, err = e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"join"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get join: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" || !jsonEqual(env.Value, map[string]interface{}{"__omnivm_callable__": true, "key": "join"}) {
		t.Fatalf("arity Ruby method handle_get envelope = %#v, want callable descriptor", env)
	}

	result, err = e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"close"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get close: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "json" || !jsonEqual(env.Value, map[string]interface{}{"__omnivm_callable__": true, "key": "close"}) {
		t.Fatalf("command-style Ruby method handle_get envelope = %#v, want callable descriptor", env)
	}

	result, err = e.HandleCall(`{"op":"handle_call","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"join","args":["tail"]}`)
	if err != nil {
		t.Fatalf("HandleCall handle_call join: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "joined" {
		t.Fatalf("Ruby method call envelope = %#v, want joined", env)
	}
	joinedExec := strings.Join(execCodes, "\n")
	if !strings.Contains(joinedExec, "public_send(__k)") || !strings.Contains(joinedExec, "join") || !strings.Contains(joinedExec, "tail") {
		t.Fatalf("Ruby property/call dispatch should execute against live runtime object, got %q", joinedExec)
	}

	result, err = e.HandleCall(`{"op":"handle_call","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"join","args":["tail"],"kwargs":{"limit":2}}`)
	if err != nil {
		t.Fatalf("HandleCall handle_call join kwargs: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "joined" {
		t.Fatalf("Ruby keyword method call envelope = %#v, want joined", env)
	}
	keywordCalls := strings.Join(append(execCodes, mocks["ruby"].evalCalls...), "\n")
	if !strings.Contains(keywordCalls, "**__kwargs") || !strings.Contains(keywordCalls, "transform_keys") || !strings.Contains(keywordCalls, "limit") {
		t.Fatalf("Ruby keyword method call should execute through keyword splat dispatch, got %q", keywordCalls)
	}
}

func TestRuntimeRefProxyCallArgumentsStayLiveRefs(t *testing.T) {
	e, _ := makeExecutor("python", "javascript", "java")

	expr, ok, err := e.runtimeRefCallExpr(
		RuntimeRef{Runtime: "python", VarName: "handler"},
		"accept",
		[]interface{}{
			RuntimeRef{Runtime: "python", VarName: "same_runtime"},
			RuntimeRef{Runtime: "javascript", VarName: "foreign_object"},
			map[string]interface{}{
				"nested": RuntimeRef{Runtime: "javascript", VarName: "nested_object"},
			},
		},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExpr python: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(expr, "same_runtime") {
		t.Fatalf("same-runtime argument should stay a direct variable reference, got %q", expr)
	}
	if !strings.Contains(expr, "__omnivm_materialize_capture") || !strings.Contains(expr, "__omnivm_resource__") {
		t.Fatalf("cross-runtime argument should be rematerialized as a generic proxy, got %q", expr)
	}
	callExpr := expr
	if idx := strings.LastIndex(callExpr, "\n"); idx >= 0 {
		callExpr = callExpr[idx+1:]
	}
	if strings.Contains(callExpr, "__omnivm_runtime_ref__") {
		t.Fatalf("runtime-ref arguments should not degrade into JSON runtime-ref structs, got %q", callExpr)
	}

	kwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{Runtime: "python", VarName: "handler"},
		"accept",
		[]interface{}{"open"},
		map[string]interface{}{
			"limit":   2,
			"payload": RuntimeRef{Runtime: "javascript", VarName: "kw_payload"},
		},
		&runtimeExprBuilder{executor: e, targetRuntime: "python"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder python kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(kwExpr, "**__kwargs") || !strings.Contains(kwExpr, "limit") {
		t.Fatalf("python keyword method call should pass kwargs through, got %q", kwExpr)
	}
	if !strings.Contains(kwExpr, "__omnivm_materialize_capture") || !strings.Contains(kwExpr, "__omnivm_resource__") {
		t.Fatalf("python keyword runtime-ref values should be rematerialized, got %q", kwExpr)
	}

	rubyKwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{Runtime: "ruby", VarName: "handler"},
		"accept",
		[]interface{}{"open"},
		map[string]interface{}{
			"limit":   2,
			"payload": RuntimeRef{Runtime: "javascript", VarName: "kw_payload"},
		},
		&runtimeExprBuilder{executor: e, targetRuntime: "ruby"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder ruby kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rubyKwExpr, "**__kwargs") || !strings.Contains(rubyKwExpr, "transform_keys") || !strings.Contains(rubyKwExpr, "limit") {
		t.Fatalf("ruby keyword method call should pass symbolized kwargs through, got %q", rubyKwExpr)
	}
	if !strings.Contains(rubyKwExpr, "__omnivm_materialize_capture") || !strings.Contains(rubyKwExpr, "__omnivm_resource__") {
		t.Fatalf("ruby keyword runtime-ref values should be rematerialized, got %q", rubyKwExpr)
	}

	rubyCallableKwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{Runtime: "ruby", VarName: "handler"},
		"",
		[]interface{}{"open"},
		map[string]interface{}{"limit": 2},
		&runtimeExprBuilder{executor: e, targetRuntime: "ruby"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder ruby callable kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rubyCallableKwExpr, "__o.call(*__args, **__kwargs)") {
		t.Fatalf("ruby keyword callable call should splat kwargs, got %q", rubyCallableKwExpr)
	}

	jsOptionsKwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "javascript",
			VarName: "handler",
			CallableShape: &CallableShape{
				AcceptsOptionsObject: true,
				DestructuredKeys:     []string{"limit", "payload"},
			},
		},
		"",
		[]interface{}{"open"},
		map[string]interface{}{
			"limit":   2,
			"payload": RuntimeRef{Runtime: "python", VarName: "kw_payload"},
		},
		&runtimeExprBuilder{executor: e, targetRuntime: "javascript"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder javascript options kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(jsOptionsKwExpr, ".concat([__kwargs])") || !strings.Contains(jsOptionsKwExpr, "limit") {
		t.Fatalf("javascript options-object callable should append kwargs object, got %q", jsOptionsKwExpr)
	}
	if !strings.Contains(jsOptionsKwExpr, "__omnivm_materialize_capture") || !strings.Contains(jsOptionsKwExpr, "__omnivm_resource__") {
		t.Fatalf("javascript options-object kwargs should rematerialize runtime-ref values, got %q", jsOptionsKwExpr)
	}

	if _, _, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "javascript",
			VarName: "handler",
			CallableShape: &CallableShape{
				AcceptsOptionsObject: true,
				DestructuredKeys:     []string{"limit"},
			},
		},
		"",
		[]interface{}{},
		map[string]interface{}{"payload": 2},
		&runtimeExprBuilder{executor: e, targetRuntime: "javascript"},
	); err == nil || !strings.Contains(err.Error(), "keyword") {
		t.Fatalf("javascript options-object callable should reject unknown keyword shape, got %v", err)
	}

	if _, _, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{Runtime: "javascript", VarName: "handler"},
		"accept",
		[]interface{}{},
		map[string]interface{}{"limit": 2},
		&runtimeExprBuilder{executor: e, targetRuntime: "javascript"},
	); err == nil || !strings.Contains(err.Error(), "keyword") {
		t.Fatalf("javascript keyword method call should fail explicitly, got %v", err)
	}

	if _, _, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{Runtime: "java", VarName: "handler"},
		"accept",
		[]interface{}{},
		map[string]interface{}{"limit": 2},
		&runtimeExprBuilder{executor: e, targetRuntime: "java"},
	); err == nil || !strings.Contains(err.Error(), "keyword") {
		t.Fatalf("java keyword method call should fail explicitly, got %v", err)
	}

	javaKwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "java",
			VarName: "handler",
			CallableShape: &CallableShape{
				JavaAdapter: &JavaCallableAdapter{
					Kind:   "map",
					Method: "accept",
					Keys:   []string{"limit", "payload"},
				},
			},
		},
		"accept",
		[]interface{}{"open"},
		map[string]interface{}{
			"limit":   2,
			"payload": RuntimeRef{Runtime: "python", VarName: "kw_payload"},
		},
		&runtimeExprBuilder{executor: e, targetRuntime: "java"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder java map kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(javaKwExpr, "proxyCall") ||
		!strings.Contains(javaKwExpr, "getCapture(\"handler\")") ||
		!strings.Contains(javaKwExpr, "accept") ||
		!strings.Contains(javaKwExpr, "omnivm.OmniVM.listOf") ||
		!strings.Contains(javaKwExpr, "omnivm.OmniVM.mapOf") ||
		!strings.Contains(javaKwExpr, "limit") {
		t.Fatalf("java map-adapter keyword method should append kwargs map, got %q", javaKwExpr)
	}
	if !strings.Contains(javaKwExpr, "omnivm.OmniVM.materializeJsonCapture") || !strings.Contains(javaKwExpr, "__omnivm_resource__") {
		t.Fatalf("java map-adapter keyword runtime-ref values should be rematerialized, got %q", javaKwExpr)
	}

	javaCallableKwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "java",
			VarName: "handler",
			CallableShape: &CallableShape{
				JavaAdapter: &JavaCallableAdapter{Kind: "map", Keys: []string{"limit"}},
			},
		},
		"",
		[]interface{}{},
		map[string]interface{}{"limit": 2},
		&runtimeExprBuilder{executor: e, targetRuntime: "java"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder java callable map kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(javaCallableKwExpr, "proxyCall") ||
		!strings.Contains(javaCallableKwExpr, "getCapture(\"handler\")") ||
		!strings.Contains(javaCallableKwExpr, `""`) ||
		!strings.Contains(javaCallableKwExpr, "omnivm.OmniVM.mapOf") {
		t.Fatalf("java map-adapter keyword callable should append kwargs map, got %q", javaCallableKwExpr)
	}

	javaRecordKwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "java",
			VarName: "handler",
			CallableShape: &CallableShape{
				JavaAdapter: &JavaCallableAdapter{
					Kind:       "record",
					Method:     "accept",
					TargetType: "com.example.SearchOptions",
					Keys:       []string{"limit", "payload"},
				},
			},
		},
		"accept",
		[]interface{}{},
		map[string]interface{}{"limit": 2, "payload": "open"},
		&runtimeExprBuilder{executor: e, targetRuntime: "java"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder java record kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(javaRecordKwExpr, "kwargsRecord") ||
		!strings.Contains(javaRecordKwExpr, "com.example.SearchOptions") ||
		!strings.Contains(javaRecordKwExpr, "proxyCall") ||
		!strings.Contains(javaRecordKwExpr, "accept") {
		t.Fatalf("java record-adapter keyword method should construct record arg, got %q", javaRecordKwExpr)
	}

	javaBuilderKwExpr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "java",
			VarName: "handler",
			CallableShape: &CallableShape{
				JavaAdapter: &JavaCallableAdapter{
					Kind:       "builder",
					Method:     "accept",
					TargetType: "com.example.SearchOptionsBuilder",
					Keys:       []string{"limit", "payload"},
				},
			},
		},
		"accept",
		[]interface{}{},
		map[string]interface{}{"limit": 2, "payload": "open"},
		&runtimeExprBuilder{executor: e, targetRuntime: "java"},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder java builder kwargs: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(javaBuilderKwExpr, "kwargsBuilder") ||
		!strings.Contains(javaBuilderKwExpr, "com.example.SearchOptionsBuilder") ||
		!strings.Contains(javaBuilderKwExpr, "proxyCall") ||
		!strings.Contains(javaBuilderKwExpr, "accept") {
		t.Fatalf("java builder-adapter keyword method should construct builder arg, got %q", javaBuilderKwExpr)
	}

	if _, _, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "java",
			VarName: "handler",
			CallableShape: &CallableShape{
				JavaAdapter: &JavaCallableAdapter{Kind: "map", Method: "other", Keys: []string{"limit"}},
			},
		},
		"accept",
		[]interface{}{},
		map[string]interface{}{"limit": 2},
		&runtimeExprBuilder{executor: e, targetRuntime: "java"},
	); err == nil || !strings.Contains(err.Error(), "keyword") {
		t.Fatalf("java keyword method call should reject mismatched adapter method, got %v", err)
	}

	if _, _, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{
			Runtime: "java",
			VarName: "handler",
			CallableShape: &CallableShape{
				JavaAdapter: &JavaCallableAdapter{Kind: "map", Method: "accept", Keys: []string{"limit"}},
			},
		},
		"accept",
		[]interface{}{},
		map[string]interface{}{"payload": 2},
		&runtimeExprBuilder{executor: e, targetRuntime: "java"},
	); err == nil || !strings.Contains(err.Error(), "keyword") {
		t.Fatalf("java keyword method call should reject unknown adapter key, got %v", err)
	}

	javaExpr, ok, err := e.runtimeRefCallExpr(
		RuntimeRef{Runtime: "java", VarName: "sink"},
		"accept",
		[]interface{}{RuntimeRef{Runtime: "python", VarName: "payload"}},
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExpr java: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(javaExpr, "omnivm.OmniVM.materializeJsonCapture") || !strings.Contains(javaExpr, "omnivm.OmniVM.listOf") {
		t.Fatalf("Java runtime-ref args should use generic Java materialization helpers, got %q", javaExpr)
	}
}

func TestRuntimeRefPythonCallMaterializesJSByteArrayArgAsBytes(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	before := arrow.GlobalStore().Stats()
	payload := []byte("late")
	mocks["javascript"].exportFn = func(name, expr string) (pkg.ExportedBuffer, bool, error) {
		if expr != `globalThis.__omnivm_arg_refs["arg_1"]` {
			t.Fatalf("ExportBuffer expr = %q", expr)
		}
		if _, err := arrow.GlobalStore().SetWithMetadata(name, payload, arrow.BufferMetadata{
			Dtype:     arrow.DtypeU8,
			Format:    "C",
			Shape:     []int64{int64(len(payload))},
			ReadOnly:  false,
			Ownership: "producer",
		}); err != nil {
			return pkg.ExportedBuffer{}, false, err
		}
		return pkg.ExportedBuffer{
			Name:        name,
			Dtype:       arrow.DtypeU8,
			ArrowFormat: "C",
			Elements:    int64(len(payload)),
			Shape:       []int64{int64(len(payload))},
		}, true, nil
	}
	builder := &runtimeExprBuilder{executor: e, targetRuntime: "python"}
	expr, ok, err := runtimeRefCallExprWithBuilder(
		RuntimeRef{Runtime: "python", VarName: "response"},
		"write",
		[]interface{}{RuntimeRef{Runtime: "javascript", VarName: `__omnivm_arg_refs["arg_1"]`}},
		nil,
		builder,
	)
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExprWithBuilder python byte arg: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(expr, "__omnivm_table_bytes") || strings.Contains(expr, "__omnivm_resource__") {
		t.Fatalf("JS byte-array argument should compile as Python bytes, got %q", expr)
	}
	if prelude := builder.prelude(); !strings.Contains(prelude, "def __omnivm_table_bytes") || strings.Contains(prelude, "class __OmniVMHandleProxy") {
		t.Fatalf("Python byte table prelude should be narrow, got %q", prelude)
	}
	stats := e.BoundaryStats()
	if stats.TableProxyCaptures != 1 || stats.ArrowTransfers != 1 || stats.ResourceProxyCaptures != 0 || stats.JSONFallbacks != 0 {
		t.Fatalf("JS byte-array arg stats = %+v, want Arrow table without proxy fallback", stats)
	}
	if len(mocks["javascript"].exports) != 1 {
		t.Fatalf("ExportBuffer calls = %d, want 1", len(mocks["javascript"].exports))
	}
	if err := e.releaseAllHandleScopes(); err != nil {
		t.Fatalf("releaseAllHandleScopes: %v", err)
	}
	released := arrow.GlobalStore().Stats()
	if released.LiveBuffers != before.LiveBuffers {
		t.Fatalf("runtime-ref byte arg buffer was not released: before=%+v after=%+v", before, released)
	}
}

func TestRuntimeRefKwargsDiagnosticsExplainRejectedShape(t *testing.T) {
	e, _ := makeExecutor("python", "javascript", "java")

	cases := []struct {
		name   string
		ref    RuntimeRef
		key    string
		kwargs map[string]interface{}
		target string
		want   []string
	}{
		{
			name: "javascript callable unknown option key",
			ref: RuntimeRef{
				Runtime: "javascript",
				VarName: "handler",
				CallableShape: &CallableShape{
					AcceptsOptionsObject: true,
					DestructuredKeys:     []string{"limit"},
				},
			},
			kwargs: map[string]interface{}{"payload": 2},
			target: "python",
			want: []string{
				"keyword proxy call rejected",
				"source runtime=javascript",
				"target runtime=python",
				"call shape=callable",
				"kwargs=[payload]",
				"detected shape=acceptsOptionsObject;destructuredKeys=[limit]",
				`reason=keyword "payload" is not in destructuredKeys [limit]`,
				"smallest fix=declare callable_shape.acceptsOptionsObject/destructuredKeys",
			},
		},
		{
			name: "javascript method kwargs without method shape",
			ref: RuntimeRef{
				Runtime: "javascript",
				VarName: "handler",
			},
			key:    "accept",
			kwargs: map[string]interface{}{"limit": 2},
			target: "javascript",
			want: []string{
				"source runtime=javascript",
				"target runtime=javascript",
				`call shape=method "accept"`,
				"detected shape=none",
				"reason=JavaScript method property has no proven options-object callable shape",
				"smallest fix=capture the JavaScript method as a function",
			},
		},
		{
			name: "java method adapter mismatch",
			ref: RuntimeRef{
				Runtime: "java",
				VarName: "handler",
				CallableShape: &CallableShape{
					JavaAdapter: &JavaCallableAdapter{
						Kind:   "map",
						Method: "other",
						Keys:   []string{"limit"},
					},
				},
			},
			key:    "accept",
			kwargs: map[string]interface{}{"limit": 2},
			target: "java",
			want: []string{
				"source runtime=java",
				"target runtime=java",
				`call shape=method "accept"`,
				"kwargs=[limit]",
				"detected shape=javaAdapter{kind=map,method=other,keys=[limit]}",
				`reason=javaAdapter method "other" does not match method "accept"`,
				"smallest fix=declare callable_shape.javaAdapter",
			},
		},
		{
			name: "java callable without adapter",
			ref: RuntimeRef{
				Runtime: "java",
				VarName: "handler",
			},
			kwargs: map[string]interface{}{"limit": 2},
			target: "java",
			want: []string{
				"source runtime=java",
				"target runtime=java",
				"call shape=callable",
				"detected shape=none",
				"reason=no callable_shape metadata was captured for the Java target",
				"smallest fix=declare callable_shape.javaAdapter",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runtimeRefCallExprWithBuilder(
				tc.ref,
				tc.key,
				[]interface{}{},
				tc.kwargs,
				&runtimeExprBuilder{executor: e, targetRuntime: tc.target},
			)
			if err == nil {
				t.Fatalf("runtimeRefCallExprWithBuilder should reject kwargs")
			}
			got := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("diagnostic missing %q\nfull diagnostic: %s", want, got)
				}
			}
		})
	}
}

func TestGoldenProxyAndMaterializationDiagnostics(t *testing.T) {
	e, _ := makeExecutor("python")

	if _, err := e.HandleCall(`{"op":"handle_get","id":404,"key":"path"}`); err == nil || err.Error() != "manifest HandleCall: unknown handle 404" {
		t.Fatalf("unknown proxy diagnostic = %v", err)
	}

	id, err := e.ensureHandleTable().Register(map[string]interface{}{"path": "/orders"}, handles.RegisterOptions{
		Runtime: "python",
		Kind:    "resource",
	})
	if err != nil {
		t.Fatalf("register proxy handle: %v", err)
	}
	_, err = e.HandleCall(fmt.Sprintf(`{"op":"handle_call","id":%d,"key":"accept","kwargs":{"limit":2}}`, id))
	wantProxy := fmt.Sprintf("manifest HandleCall: handle %d method %q does not support keyword arguments", id, "accept")
	if err == nil || err.Error() != wantProxy {
		t.Fatalf("proxy kwargs diagnostic = %v, want %q", err, wantProxy)
	}

	_, err = marshalResult(make(chan int))
	const wantMaterialization = "bridge result value chan int is not JSON-marshalable; boundary classification must produce a primitive, descriptor, table, stream, or proxy"
	if err == nil || err.Error() != wantMaterialization {
		t.Fatalf("materialization diagnostic = %v, want %q", err, wantMaterialization)
	}
}

func TestClosedResourceHandleOpsReportLifecycleError(t *testing.T) {
	e, _ := makeExecutor("python")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value: &ValueExpr{Kind: "literal", Value: map[string]interface{}{
			"path":  "/closed-owner",
			"items": []interface{}{"first"},
		}},
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "arg",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/arg"}},
	}); err != nil {
		t.Fatalf("resource arg open: %v", err)
	}
	argVal, _ := e.getBinding("arg")
	argRef := argVal.(*ResourceRef)
	if _, err := e.executeOp(&Op{OpType: "resource", Action: "close", Target: "req"}); err != nil {
		t.Fatalf("resource close: %v", err)
	}

	calls := []string{
		`{"op":"handle_retain","id":%d}`,
		`{"op":"handle_adopt","id":%d}`,
		`{"op":"handle_access","id":%d,"kind":"property"}`,
		`{"op":"handle_get","id":%d,"key":"path"}`,
		`{"op":"handle_index","id":%d,"value":"items"}`,
		`{"op":"handle_len","id":%d}`,
		`{"op":"handle_iter","id":%d,"mode":"values"}`,
		`{"op":"handle_contains","id":%d,"value":"path"}`,
		`{"op":"stream_next","id":%d}`,
		`{"op":"handle_set","id":%d,"key":"path","value":"/new"}`,
		`{"op":"handle_call","id":%d,"key":"close","args":[]}`,
	}
	for _, call := range calls {
		_, err := e.HandleCall(fmt.Sprintf(call, ref.ID))
		if err == nil {
			t.Fatalf("closed resource call %s did not fail", call)
		}
		got := err.Error()
		for _, want := range []string{"closed resource handle", "runtime=python", "kind=request", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("closed resource call %s diagnostic missing %q: %s", call, want, got)
			}
		}
	}
	argCall := fmt.Sprintf(
		`{"op":"handle_call","id":%d,"key":"accept","args":[{"__omnivm_resource__":true,"id":%d,"runtime":"python","kind":"request"}]}`,
		ref.ID,
		argRef.ID,
	)
	if _, err := e.HandleCall(argCall); err == nil {
		t.Fatal("closed resource handle_call with live proxy arg did not fail")
	} else {
		got := err.Error()
		for _, want := range []string{"closed resource handle", "runtime=python", "kind=request", "owner-side lifecycle is closed"} {
			if !strings.Contains(got, want) {
				t.Fatalf("closed resource handle_call with arg diagnostic missing %q: %s", want, got)
			}
		}
		if strings.Contains(got, "unknown source handle") {
			t.Fatalf("closed resource handle_call with arg used generic handle-table diagnostic: %s", got)
		}
	}

	referenceCalls := []string{
		fmt.Sprintf(`{"op":"handle_reference","from":%d,"to":%d,"kind":"property"}`, ref.ID, argRef.ID),
		fmt.Sprintf(`{"op":"handle_reference","from":%d,"to":%d,"kind":"property"}`, argRef.ID, ref.ID),
	}
	for _, call := range referenceCalls {
		if _, err := e.HandleCall(call); err == nil {
			t.Fatalf("closed resource reference call %s did not fail", call)
		} else {
			got := err.Error()
			for _, want := range []string{"closed resource handle", "runtime=python", "kind=request", "owner-side lifecycle is closed"} {
				if !strings.Contains(got, want) {
					t.Fatalf("closed resource reference call %s diagnostic missing %q: %s", call, want, got)
				}
			}
			if strings.Contains(got, "unknown source handle") || strings.Contains(got, "unknown target handle") {
				t.Fatalf("closed resource reference call %s used generic handle-table diagnostic: %s", call, got)
			}
		}
	}
}

func TestClosedProxyCleanupOpsRemainIdempotent(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/closed-cleanup"}},
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "arg",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/arg-cleanup"}},
	}); err != nil {
		t.Fatalf("resource arg open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)
	argVal, _ := e.getBinding("arg")
	argRef := argVal.(*ResourceRef)
	if _, err := e.executeOp(&Op{OpType: "resource", Action: "close", Target: "req"}); err != nil {
		t.Fatalf("resource close: %v", err)
	}
	before := e.handleTable.Stats(time.Now())

	result, err := e.HandleCall(`{"op":"handle_release_finalizer","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`)
	if err != nil {
		t.Fatalf("closed handle_release_finalizer should remain idempotent: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "bool" || env.Value != false {
		t.Fatalf("closed handle_release_finalizer envelope = %#v, want false", env)
	}
	after := e.handleTable.Stats(time.Now())
	if after.FinalizerQueued != before.FinalizerQueued || after.FinalizerQueueLen != before.FinalizerQueueLen || after.FinalizerReleases != before.FinalizerReleases {
		t.Fatalf("closed finalizer cleanup changed finalizer stats: before=%+v after=%+v", before, after)
	}

	for _, call := range []string{
		fmt.Sprintf(`{"op":"handle_drop_reference","from":%d,"to":%d}`, ref.ID, argRef.ID),
		fmt.Sprintf(`{"op":"handle_drop_reference","from":%d,"to":%d}`, argRef.ID, ref.ID),
	} {
		result, err := e.HandleCall(call)
		if err != nil {
			t.Fatalf("closed handle_drop_reference cleanup %s should remain idempotent: %v", call, err)
		}
		env := decodeResultEnvelopeForTest(t, result)
		if env.Kind != "bool" || env.Value != true {
			t.Fatalf("closed handle_drop_reference envelope = %#v, want true", env)
		}
	}
}

func TestClosedProxyExplicitReleaseReportsLifecycleDiagnostic(t *testing.T) {
	e, _ := makeExecutor("python", "javascript")
	if _, err := e.executeOp(&Op{
		OpType:  "resource",
		Action:  "open",
		Runtime: "python",
		Bind:    "req",
		Kind:    "request",
		Value:   &ValueExpr{Kind: "literal", Value: map[string]interface{}{"path": "/closed-explicit"}},
	}); err != nil {
		t.Fatalf("resource open: %v", err)
	}
	val, _ := e.getBinding("req")
	ref := val.(*ResourceRef)
	if _, err := e.executeOp(&Op{OpType: "resource", Action: "close", Target: "req"}); err != nil {
		t.Fatalf("resource close: %v", err)
	}

	_, err := e.HandleCall(`{"op":"handle_release_explicit","id":` + strconv.FormatUint(uint64(ref.ID), 10) + `}`)
	if err == nil {
		t.Fatal("closed handle_release_explicit did not report owner lifecycle error")
	}
	got := err.Error()
	for _, want := range []string{"closed resource handle", "runtime=python", "kind=request", "owner-side lifecycle is closed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("closed handle_release_explicit diagnostic missing %q: %s", want, got)
		}
	}
}

func TestAdapterConformanceCoversRuntimeAndFrameworkShapes(t *testing.T) {
	e, _ := makeExecutor("python", "javascript", "java", "ruby")

	callCases := []struct {
		name   string
		ref    RuntimeRef
		key    string
		kwargs map[string]interface{}
		target string
		want   []string
	}{
		{
			name:   "python method kwargs",
			ref:    RuntimeRef{Runtime: "python", VarName: "handler"},
			key:    "accept",
			kwargs: map[string]interface{}{"limit": 2},
			target: "python",
			want:   []string{"**__kwargs", "limit"},
		},
		{
			name:   "ruby method keyword args",
			ref:    RuntimeRef{Runtime: "ruby", VarName: "handler"},
			key:    "accept",
			kwargs: map[string]interface{}{"limit": 2},
			target: "ruby",
			want:   []string{"transform_keys", "**__kwargs", "limit"},
		},
		{
			name: "javascript options object",
			ref: RuntimeRef{
				Runtime: "javascript",
				VarName: "handler",
				CallableShape: &CallableShape{
					AcceptsOptionsObject: true,
					DestructuredKeys:     []string{"limit", "payload"},
				},
			},
			kwargs: map[string]interface{}{"limit": 2, "payload": "open"},
			target: "javascript",
			want:   []string{".concat([__kwargs])", "limit", "payload"},
		},
		{
			name: "java map adapter",
			ref: RuntimeRef{
				Runtime: "java",
				VarName: "handler",
				CallableShape: &CallableShape{
					JavaAdapter: &JavaCallableAdapter{Kind: "map", Method: "accept", Keys: []string{"limit"}},
				},
			},
			key:    "accept",
			kwargs: map[string]interface{}{"limit": 2},
			target: "java",
			want:   []string{"proxyCall", "accept", "omnivm.OmniVM.mapOf", "limit"},
		},
		{
			name: "java record adapter",
			ref: RuntimeRef{
				Runtime: "java",
				VarName: "handler",
				CallableShape: &CallableShape{
					JavaAdapter: &JavaCallableAdapter{
						Kind:       "record",
						Method:     "accept",
						TargetType: "com.example.SearchOptions",
						Keys:       []string{"limit"},
					},
				},
			},
			key:    "accept",
			kwargs: map[string]interface{}{"limit": 2},
			target: "java",
			want:   []string{"proxyCall", "kwargsRecord", "com.example.SearchOptions", "limit"},
		},
		{
			name: "java builder adapter",
			ref: RuntimeRef{
				Runtime: "java",
				VarName: "handler",
				CallableShape: &CallableShape{
					JavaAdapter: &JavaCallableAdapter{
						Kind:       "builder",
						Method:     "accept",
						TargetType: "com.example.SearchOptionsBuilder",
						Keys:       []string{"limit"},
					},
				},
			},
			key:    "accept",
			kwargs: map[string]interface{}{"limit": 2},
			target: "java",
			want:   []string{"proxyCall", "kwargsBuilder", "com.example.SearchOptionsBuilder", "limit"},
		},
	}

	for _, tc := range callCases {
		t.Run(tc.name, func(t *testing.T) {
			expr, ok, err := runtimeRefCallExprWithBuilder(
				tc.ref,
				tc.key,
				[]interface{}{"open"},
				tc.kwargs,
				&runtimeExprBuilder{executor: e, targetRuntime: tc.target},
			)
			if err != nil || !ok {
				t.Fatalf("runtimeRefCallExprWithBuilder: ok=%v err=%v", ok, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(expr, want) {
					t.Fatalf("%s missing %q in expr %q", tc.name, want, expr)
				}
			}
		})
	}

	frameworkCases := []struct {
		name            string
		value           interface{}
		wantHTTPMessage bool
	}{
		{
			name: "request reader shape",
			value: &goHTTPMessageReaderShape{
				Method:  "GET",
				Path:    "/orders/42",
				Headers: map[string]string{"X-Request-Id": "req-42"},
			},
			wantHTTPMessage: true,
		},
		{
			name: "response reader shape",
			value: &goHTTPResponseReaderShape{
				RequestMethod: "GET",
				Header:        map[string]string{"Content-Type": "application/json"},
				StatusCode:    202,
			},
			wantHTTPMessage: true,
		},
	}

	for _, tc := range frameworkCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isHTTPMessageShapeValue(tc.value); got != tc.wantHTTPMessage {
				t.Fatalf("isHTTPMessageShapeValue = %v, want %v", got, tc.wantHTTPMessage)
			}
			if isReaderStreamValue(tc.value) {
				t.Fatalf("%s should stay a framework resource, not a reader stream", tc.name)
			}
			bridged, err := e.bridgeReturnValue(tc.value)
			if err != nil {
				t.Fatalf("bridgeReturnValue: %v", err)
			}
			descriptor, ok := bridged.(map[string]interface{})
			if !ok || descriptor["__omnivm_resource__"] != true || descriptor["runtime"] != "go" || descriptor["kind"] != "object" {
				t.Fatalf("framework shape should bridge as go object resource proxy, got %#v", bridged)
			}
			reads := 0
			switch v := tc.value.(type) {
			case *goHTTPMessageReaderShape:
				reads = v.reads
			case *goHTTPResponseReaderShape:
				reads = v.reads
			}
			if reads != 0 {
				t.Fatalf("%s was read during bridge, reads=%d", tc.name, reads)
			}
		})
	}
}

func TestRuntimeRefProxyReadsLiveJavaProperty(t *testing.T) {
	e, mocks := makeExecutor("java", "javascript")
	var execCode string
	mocks["java"].execFn = func(code string) pkg.Result {
		execCode = code
		return pkg.Result{}
	}
	mocks["java"].evalFn = func(code string) pkg.Result {
		if strings.Contains(code, "proxyCallable") {
			return pkg.Result{Value: "false"}
		}
		if strings.Contains(code, "__omnivm_ref_") {
			return pkg.Result{Value: `"Ada"`}
		}
		return pkg.Result{Value: `{"__omnivm_java_object__":true,"class":"User"}`}
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "java", VarName: "user", Value: nil})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"name"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	if env.Kind != "string" || env.Value != "Ada" {
		t.Fatalf("live Java RuntimeRef property envelope = %#v, want Ada", env)
	}
	if !strings.Contains(execCode, "omnivm.OmniVM.proxyGet") || !strings.Contains(execCode, `omnivm.OmniVM.getCapture("user")`) {
		t.Fatalf("Java property read should execute through generic live proxy helpers, got %q", execCode)
	}
}

func TestRuntimeRefProxyMarksJavaZeroArgMethodDescriptor(t *testing.T) {
	e, mocks := makeExecutor("java", "javascript")
	mocks["java"].execFn = func(code string) pkg.Result {
		return pkg.Result{}
	}
	mocks["java"].evalFn = func(code string) pkg.Result {
		if strings.Contains(code, "proxyCallable") {
			return pkg.Result{Value: "true"}
		}
		if strings.Contains(code, "proxyZeroArgCallable") {
			return pkg.Result{Value: "true"}
		}
		return pkg.Result{Value: `{"__omnivm_java_object__":true,"class":"ResultSet"}`}
	}

	jsonVal, err := e.runtimeRefProxyCaptureJSON(RuntimeRef{Runtime: "java", VarName: "rows", Value: nil})
	if err != nil {
		t.Fatalf("runtimeRefProxyCaptureJSON: %v", err)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		t.Fatalf("descriptor JSON: %v", err)
	}
	id, err := bridgeHandleID(descriptor["id"])
	if err != nil {
		t.Fatalf("descriptor id: %v", err)
	}

	result, err := e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"isClosed"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get: %v", err)
	}
	env := decodeResultEnvelopeForTest(t, result)
	want := map[string]interface{}{"__omnivm_callable__": true, "key": "isClosed", "zeroArg": true}
	if env.Kind != "json" || !jsonEqual(env.Value, want) {
		t.Fatalf("live Java zero-arg method descriptor = %#v, want %#v", env, want)
	}

	result, err = e.HandleCall(`{"op":"handle_get","id":` + strconv.FormatUint(uint64(id), 10) + `,"key":"close"}`)
	if err != nil {
		t.Fatalf("HandleCall handle_get close: %v", err)
	}
	env = decodeResultEnvelopeForTest(t, result)
	want = map[string]interface{}{"__omnivm_callable__": true, "key": "close"}
	if env.Kind != "json" || !jsonEqual(env.Value, want) {
		t.Fatalf("live Java command method descriptor = %#v, want %#v", env, want)
	}
}

func TestRuntimeRefLookupPrefersMappingKeysBeforeMethods(t *testing.T) {
	pythonProp, ok, err := runtimeRefPropertyExpr(RuntimeRef{Runtime: "python", VarName: "payload"}, "items")
	if err != nil || !ok {
		t.Fatalf("runtimeRefPropertyExpr python: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(pythonProp, "collections.abc") || !strings.Contains(pythonProp, "Mapping) and __k in __o") || strings.Index(pythonProp, "__o[__k]") > strings.Index(pythonProp, "getattr") {
		t.Fatalf("python property lookup should prefer mapping keys before attributes, got %q", pythonProp)
	}
	if !strings.Contains(pythonProp, "hasattr(getattr(__o, 'keys', None), '__call__')") || !strings.Contains(pythonProp, "hasattr(__o, '__getitem__')") {
		t.Fatalf("python property lookup should treat key-addressable session-like objects as data before attributes without broad membership probes, got %q", pythonProp)
	}
	if strings.Contains(pythonProp, "hasattr(__o, '__contains__')") {
		t.Fatalf("python property lookup should not call arbitrary __contains__ while probing key-addressable objects, got %q", pythonProp)
	}
	if !strings.Contains(pythonProp, "model_fields") || strings.Index(pythonProp, "Mapping) and __k in __o") > strings.Index(pythonProp, "model_fields") || strings.Index(pythonProp, "model_fields") > strings.LastIndex(pythonProp, "hasattr(__o, __k)") {
		t.Fatalf("python property lookup should prefer Pydantic fields before same-named methods, got %q", pythonProp)
	}
	if !strings.Contains(pythonProp, "else None") || !strings.Contains(pythonProp, "__o[int(__k)]") {
		t.Fatalf("python property lookup should avoid unconditional item access on unscriptable objects, got %q", pythonProp)
	}

	pythonIndex, ok, err := runtimeRefIndexExpr(RuntimeRef{Runtime: "python", VarName: "payload"}, "0")
	if err != nil || !ok {
		t.Fatalf("runtimeRefIndexExpr python: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(pythonIndex, "__o[int(__k)]") || !strings.Contains(pythonIndex, "else None") || strings.Contains(pythonIndex, "else __o[__k]") {
		t.Fatalf("python index lookup should only index mappings or bounded sequences, got %q", pythonIndex)
	}

	pythonHeadersProp, ok, err := runtimeRefPropertyExpr(RuntimeRef{Runtime: "python", VarName: "request"}, "headers")
	if err != nil || !ok {
		t.Fatalf("runtimeRefPropertyExpr python headers: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(pythonHeadersProp, "hasattr(__o, 'method')") || strings.Index(pythonHeadersProp, "getattr(__o, __k)") > strings.Index(pythonHeadersProp, "Mapping) and __k in __o") {
		t.Fatalf("python HTTP message lookup should prefer request/response attributes before scope mapping keys, got %q", pythonHeadersProp)
	}

	rubyProp, ok, err := runtimeRefPropertyExpr(RuntimeRef{Runtime: "ruby", VarName: "payload"}, "count")
	if err != nil || !ok {
		t.Fatalf("runtimeRefPropertyExpr ruby: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rubyProp, "respond_to?(:key?)") || !strings.Contains(rubyProp, "__o.key?(__k)") || strings.Index(rubyProp, "__o[__k]") > strings.Index(rubyProp, "public_send") {
		t.Fatalf("ruby property lookup should prefer mapping keys before methods, got %q", rubyProp)
	}
	if !strings.Contains(rubyProp, "has_attribute?") || strings.Index(rubyProp, "has_attribute?") > strings.Index(rubyProp, "public_send") {
		t.Fatalf("ruby property lookup should prefer ActiveRecord attributes before methods, got %q", rubyProp)
	}

	pythonCallable, ok, err := runtimeRefCallableExpr(RuntimeRef{Runtime: "python", VarName: "payload"}, "items")
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallableExpr python: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(pythonCallable, "callable(__o[__k]) if ((isinstance(__o, __import__('collections.abc'") || !strings.Contains(pythonCallable, "Mapping) and __k in __o") {
		t.Fatalf("python callable lookup should inspect mapping keys before attributes, got %q", pythonCallable)
	}
	if !strings.Contains(pythonCallable, "hasattr(getattr(__o, 'keys', None), '__call__')") || !strings.Contains(pythonCallable, "hasattr(__o, '__getitem__')") {
		t.Fatalf("python callable lookup should inspect key-addressable session-like objects before attributes without broad membership probes, got %q", pythonCallable)
	}
	if strings.Contains(pythonCallable, "hasattr(__o, '__contains__')") {
		t.Fatalf("python callable lookup should not call arbitrary __contains__ while probing key-addressable objects, got %q", pythonCallable)
	}
	if !strings.Contains(pythonCallable, "model_fields") || strings.Index(pythonCallable, "Mapping) and __k in __o") > strings.Index(pythonCallable, "model_fields") || strings.Index(pythonCallable, "model_fields") > strings.LastIndex(pythonCallable, "hasattr(__o, __k)") {
		t.Fatalf("python callable lookup should inspect Pydantic fields before same-named methods, got %q", pythonCallable)
	}

	pythonCall, ok, err := runtimeRefCallExpr(RuntimeRef{Runtime: "python", VarName: "payload"}, "items", []interface{}{})
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExpr python: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(pythonCall, "model_fields") || strings.Index(pythonCall, "Mapping) and __k in __o") > strings.Index(pythonCall, "model_fields") || strings.Index(pythonCall, "model_fields") > strings.LastIndex(pythonCall, "hasattr(__o, __k)") {
		t.Fatalf("python call lookup should inspect Pydantic fields before same-named methods, got %q", pythonCall)
	}

	pythonItemsIter, ok, err := runtimeRefIterExpr(RuntimeRef{Runtime: "python", VarName: "payload"}, "items")
	if err != nil || !ok {
		t.Fatalf("runtimeRefIterExpr python items: ok=%v err=%v", ok, err)
	}
	if strings.Contains(pythonItemsIter, ".items()) if hasattr") || !strings.Contains(pythonItemsIter, "collections.abc") || !strings.Contains(pythonItemsIter, "Mapping") {
		t.Fatalf("python item iteration should require real Mapping before calling items(), got %q", pythonItemsIter)
	}

	pythonKeysIter, ok, err := runtimeRefIterExpr(RuntimeRef{Runtime: "python", VarName: "payload"}, "keys")
	if err != nil || !ok {
		t.Fatalf("runtimeRefIterExpr python keys: ok=%v err=%v", ok, err)
	}
	if strings.Contains(pythonKeysIter, ".keys()) if hasattr") || !strings.Contains(pythonKeysIter, "collections.abc") || !strings.Contains(pythonKeysIter, "Mapping") {
		t.Fatalf("python key iteration should require real Mapping before calling keys(), got %q", pythonKeysIter)
	}

	rubyCall, ok, err := runtimeRefCallExpr(RuntimeRef{Runtime: "ruby", VarName: "payload"}, "count", []interface{}{})
	if err != nil || !ok {
		t.Fatalf("runtimeRefCallExpr ruby: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rubyCall, "(__o.respond_to?(:key?) && __o.key?(__k)) ? __o[__k].call") {
		t.Fatalf("ruby call lookup should call mapping values before methods, got %q", rubyCall)
	}
}

func TestRuntimeRefSetCodeCoercesNumericSequenceKeys(t *testing.T) {
	pythonCode, ok, err := runtimeRefSetCode(RuntimeRef{Runtime: "python", VarName: "items"}, "0", "updated")
	if err != nil || !ok {
		t.Fatalf("runtimeRefSetCode python: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(pythonCode, "int(__k)") || !strings.Contains(pythonCode, "__o.__setitem__(__k, __v)") {
		t.Fatalf("python RuntimeRef set should coerce numeric sequence keys with fallback, got %q", pythonCode)
	}
	if !strings.Contains(pythonCode, "MutableMapping") || strings.Index(pythonCode, "__o[__k] = __v") > strings.Index(pythonCode, "hasattr(__o, __k)") {
		t.Fatalf("python RuntimeRef set should prefer mutable mapping keys before attributes, got %q", pythonCode)
	}
	if !strings.Contains(pythonCode, "hasattr(getattr(__o, 'keys', None), '__call__')") || !strings.Contains(pythonCode, "hasattr(__o, '__setitem__')") {
		t.Fatalf("python RuntimeRef set should update existing key-addressable session-like keys before attributes without broad membership probes, got %q", pythonCode)
	}
	if strings.Contains(pythonCode, "hasattr(__o, '__contains__')") {
		t.Fatalf("python RuntimeRef set should not call arbitrary __contains__ while probing key-addressable objects, got %q", pythonCode)
	}
	if !strings.Contains(pythonCode, "MutableSequence") || !strings.Contains(pythonCode, "__k == 'length'") || !strings.Contains(pythonCode, "del __o[__n:]") {
		t.Fatalf("python RuntimeRef set should resize mutable sequences for length writes, got %q", pythonCode)
	}
	if !strings.Contains(pythonCode, "model_fields") || strings.Index(pythonCode, "MutableMapping") > strings.Index(pythonCode, "model_fields") || strings.Index(pythonCode, "model_fields") > strings.Index(pythonCode, "hasattr(__o, __k)") {
		t.Fatalf("python RuntimeRef set should prefer Pydantic fields before same-named methods, got %q", pythonCode)
	}

	rubyCode, ok, err := runtimeRefSetCode(RuntimeRef{Runtime: "ruby", VarName: "items"}, "0", "updated")
	if err != nil || !ok {
		t.Fatalf("runtimeRefSetCode ruby: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rubyCode, "__k.to_i") || !strings.Contains(rubyCode, "each_with_index") {
		t.Fatalf("ruby RuntimeRef set should coerce numeric sequence keys with generic shape checks, got %q", rubyCode)
	}
	if !strings.Contains(rubyCode, "has_attribute?") || strings.Index(rubyCode, "has_attribute?") > strings.Index(rubyCode, "public_send") {
		t.Fatalf("ruby RuntimeRef set should prefer ActiveRecord attributes before setters, got %q", rubyCode)
	}
	if !strings.Contains(rubyCode, `__k == "length"`) || !strings.Contains(rubyCode, "__o.is_a?(Array)") || !strings.Contains(rubyCode, "__o.concat(Array.new") {
		t.Fatalf("ruby RuntimeRef set should resize arrays for length writes, got %q", rubyCode)
	}

	javaCode, ok, err := runtimeRefSetCode(RuntimeRef{Runtime: "java", VarName: "items"}, "length", 1)
	if err != nil || !ok {
		t.Fatalf("runtimeRefSetCode java: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(javaCode, "if (!omnivm.OmniVM.proxySet") || !strings.Contains(javaCode, "OmniVM Java proxy rejected set for key") {
		t.Fatalf("java RuntimeRef set should throw when proxySet rejects fixed-size targets, got %q", javaCode)
	}

	pythonContains, ok, err := runtimeRefContainsExpr(RuntimeRef{Runtime: "python", VarName: "items"}, "0")
	if err != nil || !ok {
		t.Fatalf("runtimeRefContainsExpr python: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(pythonContains, "not isinstance(__o, __import__('collections.abc'") || !strings.Contains(pythonContains, "Mapping)") || !strings.Contains(pythonContains, "int(__k)") {
		t.Fatalf("python RuntimeRef contains should recognize sequence indexes generically, got %q", pythonContains)
	}
	if !strings.Contains(pythonContains, "fromlist=['Set']).Set") || !strings.Contains(pythonContains, "hasattr(getattr(__o, 'keys', None), '__call__')") {
		t.Fatalf("python RuntimeRef contains should keep explicit Set/session-like membership without broad __contains__ probes, got %q", pythonContains)
	}
	if strings.Contains(pythonContains, "hasattr(__o, '__contains__')") {
		t.Fatalf("python RuntimeRef contains should not call arbitrary __contains__ for lazy objects, got %q", pythonContains)
	}

	rubyContains, ok, err := runtimeRefContainsExpr(RuntimeRef{Runtime: "ruby", VarName: "items"}, "0")
	if err != nil || !ok {
		t.Fatalf("runtimeRefContainsExpr ruby: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(rubyContains, "!__o.respond_to?(:key?)") || !strings.Contains(rubyContains, "__k.to_i") {
		t.Fatalf("ruby RuntimeRef contains should recognize sequence indexes generically, got %q", rubyContains)
	}
}

// --- runtimeAssign / runtimeVarRef tests ---

func TestRuntimeAssign(t *testing.T) {
	cases := []struct {
		rt, varName, expr, want string
	}{
		{"javascript", "x", "42", `globalThis["x"] = 42;`},
		{"javascript", "bad-name", "42", `globalThis["bad-name"] = 42;`},
		{"python", "x", "42", "x = 42"},
		{"python", "class", "42", `globals()["class"] = 42`},
		{"ruby", "x", "42", "$x = (begin; 42; end)"},
		{"ruby", "x", "setup; value", "$x = (begin; setup; value; end)"},
		{"ruby", "class", "42", `($omnivm_bindings ||= {})["class"] = (begin; 42; end)`},
		{"java", "x", "42", `omnivm.OmniVM.setCaptureObject("x", 42);`},
	}
	for _, tc := range cases {
		got := runtimeAssign(tc.rt, tc.varName, tc.expr)
		if got != tc.want {
			t.Errorf("runtimeAssign(%q, %q, %q) = %q, want %q", tc.rt, tc.varName, tc.expr, got, tc.want)
		}
	}
}

func TestRuntimeVarRef(t *testing.T) {
	cases := []struct {
		rt, varName, want string
	}{
		{"javascript", "x", `globalThis["x"]`},
		{"javascript", "bad-name", `globalThis["bad-name"]`},
		{"javascript", `__omnivm_arg_refs["arg_9"]`, `globalThis.__omnivm_arg_refs["arg_9"]`},
		{"ruby", "x", "$x"},
		{"ruby", "class", `($omnivm_bindings ||= {})["class"]`},
		{"ruby", `$__omnivm_arg_refs["arg_9"]`, `$__omnivm_arg_refs["arg_9"]`},
		{"java", "x", `omnivm.OmniVM.getCapture("x")`},
		{"java", `__omnivm_arg_refs["arg_9"]`, `omnivm.OmniVM.getArgRef("arg_9")`},
		{"python", "x", "x"},
		{"python", "class", `globals()["class"]`},
		{"python", `__omnivm_arg_refs["arg_9"]`, `__omnivm_arg_refs["arg_9"]`},
	}
	for _, tc := range cases {
		got := runtimeVarRef(tc.rt, tc.varName)
		if got != tc.want {
			t.Errorf("runtimeVarRef(%q, %q) = %q, want %q", tc.rt, tc.varName, got, tc.want)
		}
	}
}

func TestOpImportJavaScriptUsesPropertyBindings(t *testing.T) {
	e, mocks := makeExecutor("javascript")
	if _, err := e.opImport(&Op{Runtime: "javascript", Path: "left-pad", DefaultImport: "class"}); err != nil {
		t.Fatalf("opImport default: %v", err)
	}
	if len(mocks["javascript"].execCalls) != 1 || !strings.Contains(mocks["javascript"].execCalls[0], `globalThis["class"] = require("left-pad");`) {
		t.Fatalf("JavaScript default import should use property assignment, calls=%q", mocks["javascript"].execCalls)
	}
	if _, ok := e.getBinding("class"); !ok {
		t.Fatal("default import should record a manifest binding")
	}

	mocks["javascript"].execCalls = nil
	if _, err := e.opImport(&Op{
		Runtime: "javascript",
		Path:    "pkg",
		Specifiers: []*ImportSpec{
			{Imported: "map", Local: "bad-name"},
		},
	}); err != nil {
		t.Fatalf("opImport specifier: %v", err)
	}
	if len(mocks["javascript"].execCalls) != 1 ||
		!strings.Contains(mocks["javascript"].execCalls[0], `var __omnivm_import = require("pkg");`) ||
		!strings.Contains(mocks["javascript"].execCalls[0], `globalThis["bad-name"] = __omnivm_import["map"];`) ||
		strings.Contains(mocks["javascript"].execCalls[0], "var {") {
		t.Fatalf("JavaScript named import should use property assignment, calls=%q", mocks["javascript"].execCalls)
	}
	if _, ok := e.getBinding("bad-name"); !ok {
		t.Fatal("named import should record a manifest binding")
	}
}

func TestOpImportPythonUsesSafeAliases(t *testing.T) {
	e, mocks := makeExecutor("python")
	if _, err := e.opImport(&Op{Runtime: "python", Path: "json", DefaultImport: "class"}); err != nil {
		t.Fatalf("opImport default: %v", err)
	}
	if len(mocks["python"].execCalls) != 1 ||
		!strings.Contains(mocks["python"].execCalls[0], "import json as __omnivm_import_default") ||
		!strings.Contains(mocks["python"].execCalls[0], `globals()["class"] = __omnivm_import_default`) {
		t.Fatalf("Python default import should use a safe temporary alias, calls=%q", mocks["python"].execCalls)
	}
	if _, ok := e.getBinding("class"); !ok {
		t.Fatal("default import should record a manifest binding")
	}

	mocks["python"].execCalls = nil
	if _, err := e.opImport(&Op{
		Runtime: "python",
		Path:    "math",
		Specifiers: []*ImportSpec{
			{Imported: "sqrt", Local: "bad-name"},
		},
	}); err != nil {
		t.Fatalf("opImport specifier: %v", err)
	}
	if len(mocks["python"].execCalls) != 1 ||
		!strings.Contains(mocks["python"].execCalls[0], "from math import sqrt as __omnivm_import_0") ||
		!strings.Contains(mocks["python"].execCalls[0], `globals()["bad-name"] = __omnivm_import_0`) {
		t.Fatalf("Python named import should use a safe temporary alias, calls=%q", mocks["python"].execCalls)
	}
	if _, ok := e.getBinding("bad-name"); !ok {
		t.Fatal("named import should record a manifest binding")
	}

	mocks["python"].execCalls = nil
	if _, err := e.opImport(&Op{Runtime: "python", Path: "numpy", Bind: "np"}); err != nil {
		t.Fatalf("opImport bind alias: %v", err)
	}
	if len(mocks["python"].execCalls) != 1 ||
		!strings.Contains(mocks["python"].execCalls[0], "import numpy as __omnivm_import_bind") ||
		!strings.Contains(mocks["python"].execCalls[0], `np = __omnivm_import_bind`) {
		t.Fatalf("Python bind import should use a safe temporary alias, calls=%q", mocks["python"].execCalls)
	}
	if _, ok := e.getBinding("np"); !ok {
		t.Fatal("bind import should record a manifest binding")
	}
}

func TestOpImportRubyLoadsBaselineStdlib(t *testing.T) {
	e, mocks := makeExecutor("ruby")
	if _, err := e.opImport(&Op{Runtime: "ruby", Path: "active_record"}); err != nil {
		t.Fatalf("opImport ruby: %v", err)
	}
	if len(mocks["ruby"].execCalls) != 1 ||
		!strings.Contains(mocks["ruby"].execCalls[0], "require 'set'") ||
		!strings.Contains(mocks["ruby"].execCalls[0], "Gem::Specification.each") ||
		!strings.Contains(mocks["ruby"].execCalls[0], "require 'active_record'") {
		t.Fatalf("Ruby import should load baseline stdlib before package import, calls=%q", mocks["ruby"].execCalls)
	}
}

func TestOpImportGoRecordsCompileTimeBinding(t *testing.T) {
	e, _ := makeExecutor()
	if _, err := e.opImport(&Op{Runtime: "go", Path: "net/http", DefaultImport: "http"}); err != nil {
		t.Fatalf("opImport go default: %v", err)
	}
	bound, ok := e.getBinding("http")
	if !ok {
		t.Fatal("go default import should record a manifest binding")
	}
	ref, ok := bound.(ImportRef)
	if !ok {
		t.Fatalf("go default import binding = %T, want ImportRef", bound)
	}
	if ref.Runtime != "go" || ref.Name != "net/http" {
		t.Fatalf("go default import ref = %#v", ref)
	}
	if _, err := goToolPath(); err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	if got := e.normalizeGoArg("http.StatusAccepted"); got != 202 {
		t.Fatalf("go selector constant arg = %#v (%T), want 202", got, got)
	}

	if _, err := e.opImport(&Op{
		Runtime: "go",
		Path:    "pkg",
		Specifiers: []*ImportSpec{
			{Imported: "HandlerFunc", Local: "handler"},
		},
	}); err != nil {
		t.Fatalf("opImport go specifier: %v", err)
	}
	if _, ok := e.getBinding("handler"); !ok {
		t.Fatal("go named import should record a manifest binding")
	}
}

// --- Execute manifest tests ---

func TestExecuteManifestDeclareAndConcat(t *testing.T) {
	e, _ := makeExecutor("javascript")

	m := &Manifest{
		Version:        1,
		DefaultRuntime: "javascript",
		Ops: []*Op{
			{OpType: "declare", Bind: "a", Value: &ValueExpr{Kind: "literal", Value: "Hello"}},
			{OpType: "declare", Bind: "b", Value: &ValueExpr{Kind: "literal", Value: "World"}},
			{OpType: "concat", Bind: "msg", Segments: []*ConcatSegment{
				{Kind: "ref", Name: "a"},
				{Kind: "text", Value: " "},
				{Kind: "ref", Name: "b"},
			}},
		},
	}
	err := e.Execute(m)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	val, _ := e.getBinding("msg")
	if val != "Hello World" {
		t.Errorf("msg = %q, want %q", val, "Hello World")
	}
}

func TestExecuteDrainsPostOpRuntimeWork(t *testing.T) {
	table := handles.NewTable()
	js := newMockRuntime("javascript")
	e := NewExecutorWithHandles(map[string]pkg.Runtime{"javascript": js}, table)

	var released int
	id, err := table.Register("runtime-owned", handles.RegisterOptions{
		Runtime: "javascript",
		Kind:    "runtime_ref",
		ScopeID: e.currentHandleScope(),
		Release: func(value any) error {
			released++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	js.pumpFn = func() {
		table.QueueReleaseFromFinalizer(id)
	}

	m := &Manifest{
		Version:        1,
		DefaultRuntime: "javascript",
		Ops: []*Op{
			{OpType: "exec", Runtime: "javascript", Code: "void 0"},
		},
	}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if js.pumpCalls == 0 {
		t.Fatal("post-op drain did not pump runtime")
	}
	stats := table.Stats(time.Now())
	if stats.FinalizerQueueDrains == 0 || stats.FinalizerReleases == 0 {
		t.Fatalf("post-op drain did not release queued finalizer handle: %+v", stats)
	}
	if released != 1 {
		t.Fatalf("release callback called %d times, want 1", released)
	}
}

func TestExecutePostOpFinalizerDrainKeepsReleaseErrorsQuiet(t *testing.T) {
	table := handles.NewTable()
	js := newMockRuntime("javascript")
	e := NewExecutorWithHandles(map[string]pkg.Runtime{"javascript": js}, table)

	id, err := table.Register("runtime-owned", handles.RegisterOptions{
		Runtime: "javascript",
		Kind:    "runtime_ref",
		ScopeID: e.currentHandleScope(),
		Release: func(value any) error {
			return errors.New("finalizer release failed")
		},
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	js.pumpFn = func() {
		table.QueueReleaseFromFinalizer(id)
	}

	m := &Manifest{
		Version:        1,
		DefaultRuntime: "javascript",
		Ops: []*Op{
			{OpType: "exec", Runtime: "javascript", Code: "void 0"},
		},
	}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute should keep finalizer release errors quiet, got: %v", err)
	}
	stats := table.Stats(time.Now())
	if stats.Live != 0 || stats.FinalizerReleases != 1 || stats.ReleaseErrors != 1 {
		t.Fatalf("finalizer release stats = %+v, want quiet release error recorded", stats)
	}
}

func TestExecuteUnknownRuntime(t *testing.T) {
	e, _ := makeExecutor("javascript")
	e.defaultRuntime = "javascript"

	op := &Op{
		OpType:  "exec",
		Runtime: "lua",
		Code:    "print('hi')",
	}
	_, err := e.executeOp(op)
	if err == nil {
		t.Error("expected error for unknown runtime")
	}
}

// --- Yield tests ---

func TestYieldOutsideGenerator(t *testing.T) {
	e, _ := makeExecutor()
	// yield outside generator context should be a no-op
	_, err := e.executeOp(&Op{OpType: "yield", Value: &ValueExpr{Kind: "literal", Value: 1}})
	if err != nil {
		t.Fatalf("yield outside generator: %v", err)
	}
}

func TestYieldDelegate(t *testing.T) {
	e, _ := makeExecutor()
	e.yieldCollectors = append(e.yieldCollectors, []interface{}{})

	// yield delegate with array
	_, err := e.executeOp(&Op{
		OpType:   "yield",
		Delegate: true,
		Value:    &ValueExpr{Kind: "literal", Value: []interface{}{1, 2, 3}},
	})
	if err != nil {
		t.Fatalf("yield delegate: %v", err)
	}
	if len(e.yieldCollectors[0]) != 3 {
		t.Errorf("collected %d items, want 3", len(e.yieldCollectors[0]))
	}
}

// --- Ruby alias prefix tests ---

func TestRubyAliasPrefix(t *testing.T) {
	e, _ := makeExecutor("ruby")
	e.setBinding("name", "test")
	e.setBinding("count", 42)

	prefix := e.rubyAliasPrefix(map[string]string{"name": "name"})
	if !contains(prefix, "name = $name") {
		t.Errorf("prefix = %q, want 'name = $name'", prefix)
	}
	// Should NOT contain count since we passed explicit captures
	if contains(prefix, "count") {
		t.Error("explicit captures should not include other bindings")
	}
}

func TestRubyAliasPrefixAutoInject(t *testing.T) {
	e, _ := makeExecutor("ruby")
	e.setBinding("x", "hello")
	e.setBinding("y", 42)

	prefix := e.rubyAliasPrefix(nil)
	if !contains(prefix, "x = $x") {
		t.Errorf("prefix = %q, want 'x = $x'", prefix)
	}
	if !contains(prefix, "y = $y") {
		t.Errorf("prefix = %q, want 'y = $y'", prefix)
	}
}

func TestRubyAliasPrefixAutoInjectIncludesStreams(t *testing.T) {
	e, _ := makeExecutor("ruby")
	e.setBinding("outbox", &ChanRef{ch: make(chan interface{}, 1)})

	prefix := e.rubyAliasPrefix(nil)
	if !contains(prefix, "outbox = $outbox") {
		t.Errorf("prefix = %q, want stream alias", prefix)
	}
}

func TestRubyAliasPrefixSkipsImportRef(t *testing.T) {
	e, _ := makeExecutor("ruby")
	e.setBinding("json", ImportRef{Runtime: "ruby", Name: "json"})
	e.setBinding("x", 1)

	prefix := e.rubyAliasPrefix(nil)
	if contains(prefix, "json") {
		t.Error("should skip ImportRef bindings")
	}
}

func TestRubyAliasPrefixIncludesSameRuntimeRuntimeRef(t *testing.T) {
	e, _ := makeExecutor("ruby")
	e.setBinding("rv", RuntimeRef{Runtime: "ruby", VarName: "rv", Value: "x"})
	e.setBinding("pv", RuntimeRef{Runtime: "python", VarName: "pv", Value: "y"})

	prefix := e.rubyAliasPrefix(nil)
	if !contains(prefix, "rv = $rv") {
		t.Error("should include same-runtime RuntimeRef so persisted Ruby globals are visible as locals")
	}
	if !contains(prefix, "pv = $pv") {
		t.Error("should include cross-runtime RuntimeRef")
	}
}

// --- Ruby alias integration with opExec/opEval ---

func TestOpExecRubyAutoInjectAlias(t *testing.T) {
	e, mocks := makeExecutor("ruby")
	e.setBinding("greeting", "hello")

	mocks["ruby"].execFn = func(code string) pkg.Result {
		return pkg.Result{Output: ""}
	}

	manifest := `{
		"version": 1, "defaultRuntime": "ruby",
		"ops": [
			{"op": "declare", "bind": "greeting", "value": {"kind": "literal", "value": "hello"}},
			{"op": "exec", "runtime": "ruby", "code": "puts greeting"}
		]
	}`
	m, _ := ParseManifest([]byte(manifest))
	_ = e.Execute(m)

	// The exec call to ruby should contain the alias prefix
	found := false
	for _, call := range mocks["ruby"].execCalls {
		if contains(call, "greeting = $greeting") && contains(call, "puts greeting") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected Ruby exec to include alias prefix, calls: %v", mocks["ruby"].execCalls)
	}
}

func TestOpEvalRubyWithBindCombinesAliasAndAssign(t *testing.T) {
	e, mocks := makeExecutor("ruby")

	mocks["ruby"].execFn = func(code string) pkg.Result {
		return pkg.Result{Output: ""}
	}
	mocks["ruby"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Value: "HELLO"}
	}

	manifest := `{
		"version": 1, "defaultRuntime": "ruby",
		"ops": [
			{"op": "declare", "bind": "text", "value": {"kind": "literal", "value": "hello"}},
			{"op": "eval", "runtime": "ruby", "code": "text.upcase", "bind": "result", "captures": {"text": "text"}}
		]
	}`
	m, _ := ParseManifest([]byte(manifest))
	_ = e.Execute(m)

	// Ruby eval-with-bind should use Execute (not Eval) for the combined alias+assign
	found := false
	for _, call := range mocks["ruby"].execCalls {
		if contains(call, "text = $text") && contains(call, "$result = (begin; text.upcase; end)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected combined alias+assign in Execute call, exec calls: %v", mocks["ruby"].execCalls)
	}
}

func TestOpEvalRubyNoBind(t *testing.T) {
	e, mocks := makeExecutor("ruby")

	mocks["ruby"].execFn = func(code string) pkg.Result {
		return pkg.Result{Output: ""}
	}
	mocks["ruby"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Value: "result"}
	}

	manifest := `{
		"version": 1, "defaultRuntime": "ruby",
		"ops": [
			{"op": "declare", "bind": "x", "value": {"kind": "literal", "value": "val"}},
			{"op": "eval", "runtime": "ruby", "code": "x.length", "captures": {"x": "x"}}
		]
	}`
	m, _ := ParseManifest([]byte(manifest))
	_ = e.Execute(m)

	// Without bind, should use Eval with prefix prepended
	found := false
	for _, call := range mocks["ruby"].evalCalls {
		if contains(call, "x = $x") && contains(call, "x.length") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected eval call with alias prefix, eval calls: %v", mocks["ruby"].evalCalls)
	}
}

// --- InjectRubyCaptures tests ---

func TestInjectRubyCapturesUsesGlobals(t *testing.T) {
	code := injectRubyCaptures(map[string]string{"name": `"alice"`})
	if !contains(code, "$name") {
		t.Errorf("should use $global vars, got: %s", code)
	}
}

// helper
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
