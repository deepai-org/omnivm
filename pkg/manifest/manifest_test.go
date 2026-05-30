package manifest

import (
	"encoding/json"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// mockRuntime is a minimal mock of pkg.Runtime for testing the manifest executor
// without real runtimes (no cgo dependency).
type mockRuntime struct {
	name      string
	execFn    func(code string) pkg.Result
	evalFn    func(code string) pkg.Result
	execCalls []string
	evalCalls []string
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
func (m *mockRuntime) Pump()                                      {}
func (m *mockRuntime) Shutdown() error                            { return nil }

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
	if env.Kind != "json" || !jsonEqual(env.Value, []interface{}{float64(1), float64(2), float64(3)}) {
		t.Errorf("generator envelope = %#v, want json [1,2,3]", env)
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
	code := jsStub("add", []string{"a", "b"})
	if !contains(code, "globalThis.add") {
		t.Error("JS stub should set globalThis.add")
	}
	if !contains(code, "__omnivm_decode_result") {
		t.Error("JS stub should decode manifest result envelopes")
	}
	if !contains(code, `"add"`) {
		t.Error("JS stub should reference function name")
	}
}

func TestPythonStub(t *testing.T) {
	code := pythonStub("greet", []string{"name"})
	if !contains(code, "def greet(name)") {
		t.Error("Python stub should define function")
	}
	if !contains(code, "omnivm.call") {
		t.Error("Python stub should call omnivm")
	}
	if !contains(code, "__omnivm_decode_result") {
		t.Error("Python stub should decode manifest result envelopes")
	}
}

func TestRubyStub(t *testing.T) {
	code := rubyStub("greet", []string{"name"})
	if !contains(code, "def greet(name)") {
		t.Error("Ruby stub should define function")
	}
	if !contains(code, "OmniVM.call") {
		t.Error("Ruby stub should call OmniVM")
	}
	if !contains(code, "__omnivm_decode_result") {
		t.Error("Ruby stub should decode manifest result envelopes")
	}
}

func TestRubyStubReservedWord(t *testing.T) {
	code := rubyStub("test", []string{"end", "name"})
	if !contains(code, "end_") {
		t.Error("Ruby stub should suffix reserved word 'end' with _")
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

func TestWrapPythonCaptures(t *testing.T) {
	code := wrapPythonCaptures("print(x)", map[string]string{"x": "42"})
	if !contains(code, "__json.loads") {
		t.Error("should use json.loads")
	}
	if !contains(code, "print(x)") {
		t.Error("should include user code")
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

func TestInjectJSCapturesMaterializesChannelCapture(t *testing.T) {
	channelJSON := channelCaptureJSON("javascript", `[{"name":"a"}]`)
	code := injectJSCaptures(map[string]string{"outbox": channelJSON})
	if !contains(code, "__omnivm_channel__") {
		t.Error("should mark channel captures")
	}
	if !contains(code, "globalThis.outbox = globalThis.__omnivm_materialize_capture(") {
		t.Error("should assign materialized channel capture")
	}
	if !contains(code, "[Symbol.iterator]") {
		t.Error("should expose captured channels as JS iterables")
	}
}

func TestWrapRubyCaptures(t *testing.T) {
	code := wrapRubyCaptures("puts x", map[string]string{"x": `"hi"`})
	if !contains(code, "JSON.parse") {
		t.Error("should use JSON.parse")
	}
}

func TestWrapEmptyCaptures(t *testing.T) {
	code := wrapPythonCaptures("print(1)", map[string]string{})
	if code != "print(1)" {
		t.Errorf("empty captures should return code as-is, got %q", code)
	}
}

func TestAutoInjectScopeFallsBackWhenRuntimeRefHasNoJSON(t *testing.T) {
	e, mocks := makeExecutor("python", "javascript")
	mocks["javascript"].evalFn = func(code string) pkg.Result {
		return pkg.Result{Value: ""}
	}
	e.setBinding("handler", RuntimeRef{Runtime: "javascript", VarName: "handler", Value: nil})

	code := e.autoInjectScope("python")
	if !contains(code, "handler = __json.loads('null')") {
		t.Errorf("autoInjectScope did not inject fallback handler capture: %q", code)
	}
	if !contains(code, "__json.loads('null')") {
		t.Errorf("autoInjectScope should fall back to cached null JSON, got %q", code)
	}
}

// --- runtimeAssign / runtimeVarRef tests ---

func TestRuntimeAssign(t *testing.T) {
	cases := []struct {
		rt, varName, expr, want string
	}{
		{"javascript", "x", "42", "globalThis.x = 42;"},
		{"python", "x", "42", "x = 42"},
		{"ruby", "x", "42", "$x = 42"},
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
		{"javascript", "x", "globalThis.x"},
		{"ruby", "x", "$x"},
		{"python", "x", "x"},
	}
	for _, tc := range cases {
		got := runtimeVarRef(tc.rt, tc.varName)
		if got != tc.want {
			t.Errorf("runtimeVarRef(%q, %q) = %q, want %q", tc.rt, tc.varName, got, tc.want)
		}
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

func TestRubyAliasPrefixSkipsImportRef(t *testing.T) {
	e, _ := makeExecutor("ruby")
	e.setBinding("json", ImportRef{Runtime: "ruby", Name: "json"})
	e.setBinding("x", 1)

	prefix := e.rubyAliasPrefix(nil)
	if contains(prefix, "json") {
		t.Error("should skip ImportRef bindings")
	}
}

func TestRubyAliasPrefixSkipsSameRuntime(t *testing.T) {
	e, _ := makeExecutor("ruby")
	e.setBinding("rv", RuntimeRef{Runtime: "ruby", VarName: "rv", Value: "x"})
	e.setBinding("pv", RuntimeRef{Runtime: "python", VarName: "pv", Value: "y"})

	prefix := e.rubyAliasPrefix(nil)
	if contains(prefix, "rv = $rv") {
		t.Error("should skip same-runtime RuntimeRef")
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
		if contains(call, "text = $text") && contains(call, "$result = text.upcase") {
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
