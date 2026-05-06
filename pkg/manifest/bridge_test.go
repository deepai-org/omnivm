package manifest

import (
	"encoding/json"
	"testing"
)

func TestBridgeKey(t *testing.T) {
	k := bridgeKey("data", "python", "go")
	if k != "data|python|go" {
		t.Fatalf("got %q", k)
	}
}

func TestBuildBridgeIndex(t *testing.T) {
	bridges := []*BridgeOp{
		{Binding: "x", Op: "widen", From: "python", To: "go"},
		{Binding: "x", Op: "narrow", From: "python", To: "javascript"},
		{Binding: "x", Op: "to_string", From: "python", To: "go"},
	}
	idx := buildBridgeIndex(bridges)
	if len(idx["x|python|go"]) != 2 {
		t.Fatalf("expected 2 ops for x|python|go, got %d", len(idx["x|python|go"]))
	}
	if len(idx["x|python|javascript"]) != 1 {
		t.Fatalf("expected 1 op for x|python|javascript, got %d", len(idx["x|python|javascript"]))
	}
}

func TestApplyIdentity(t *testing.T) {
	b := &BridgeOp{Op: "identity", Binding: "x"}
	val, err := applyBridge(b, 42)
	if err != nil {
		t.Fatal(err)
	}
	if val != 42 {
		t.Fatalf("got %v", val)
	}
}

func TestApplyWiden(t *testing.T) {
	b := &BridgeOp{Op: "widen", Binding: "x", Meta: map[string]interface{}{"from": "i32", "to": "i64"}}
	val, err := applyBridge(b, 42)
	if err != nil {
		t.Fatal(err)
	}
	if val != int64(42) {
		t.Fatalf("got %v (%T)", val, val)
	}
}

func TestApplyWidenToFloat(t *testing.T) {
	b := &BridgeOp{Op: "widen", Binding: "x", Meta: map[string]interface{}{"from": "i32", "to": "f64"}}
	val, err := applyBridge(b, 42)
	if err != nil {
		t.Fatal(err)
	}
	if val != float64(42) {
		t.Fatalf("got %v (%T)", val, val)
	}
}

func TestApplyNarrowSuccess(t *testing.T) {
	b := &BridgeOp{Op: "narrow", Binding: "x", Meta: map[string]interface{}{"to": "i32"}}
	val, err := applyBridge(b, float64(100))
	if err != nil {
		t.Fatal(err)
	}
	if val != int64(100) {
		t.Fatalf("got %v (%T)", val, val)
	}
}

func TestApplyNarrowOverflow(t *testing.T) {
	b := &BridgeOp{Op: "narrow", Binding: "score", Meta: map[string]interface{}{"to": "i8"}}
	_, err := applyBridge(b, float64(999))
	if err == nil {
		t.Fatal("expected error for i8 overflow")
	}
	be, ok := err.(*BridgeError)
	if !ok {
		t.Fatalf("expected BridgeError, got %T", err)
	}
	if be.Op != "narrow" {
		t.Fatalf("expected op narrow, got %s", be.Op)
	}
}

func TestApplyNarrowFractional(t *testing.T) {
	b := &BridgeOp{Op: "narrow", Binding: "x", Meta: map[string]interface{}{"to": "i32"}}
	_, err := applyBridge(b, float64(3.14))
	if err == nil {
		t.Fatal("expected error for fractional narrowing to i32")
	}
}

func TestApplyNarrowU8(t *testing.T) {
	b := &BridgeOp{Op: "narrow", Binding: "x", Meta: map[string]interface{}{"to": "u8"}}
	val, err := applyBridge(b, float64(255))
	if err != nil {
		t.Fatal(err)
	}
	if val != int64(255) {
		t.Fatalf("got %v", val)
	}
	_, err = applyBridge(b, float64(256))
	if err == nil {
		t.Fatal("expected error for u8 overflow at 256")
	}
	_, err = applyBridge(b, float64(-1))
	if err == nil {
		t.Fatal("expected error for u8 negative")
	}
}

func TestApplyNarrowF32(t *testing.T) {
	b := &BridgeOp{Op: "narrow", Binding: "x", Meta: map[string]interface{}{"to": "f32"}}
	val, err := applyBridge(b, float64(3.14))
	if err != nil {
		t.Fatal(err)
	}
	// f32 round-trip loses precision
	if val == float64(3.14) {
		t.Fatal("f32 narrowing should lose precision")
	}
}

func TestApplyToString(t *testing.T) {
	b := &BridgeOp{Op: "to_string", Binding: "x"}
	val, err := applyBridge(b, 42)
	if err != nil {
		t.Fatal(err)
	}
	if val != "42" {
		t.Fatalf("got %v", val)
	}

	val, err = applyBridge(b, true)
	if err != nil {
		t.Fatal(err)
	}
	if val != "true" {
		t.Fatalf("got %v", val)
	}

	val, err = applyBridge(b, nil)
	if err != nil {
		t.Fatal(err)
	}
	if val != "null" {
		t.Fatalf("got %v", val)
	}
}

func TestApplyParseInt(t *testing.T) {
	b := &BridgeOp{Op: "parse_int", Binding: "x"}
	val, err := applyBridge(b, "42")
	if err != nil {
		t.Fatal(err)
	}
	if val != int64(42) {
		t.Fatalf("got %v (%T)", val, val)
	}

	_, err = applyBridge(b, "hello")
	if err == nil {
		t.Fatal("expected error for non-numeric")
	}

	_, err = applyBridge(b, "3.14")
	if err == nil {
		t.Fatal("expected error for fractional parse_int")
	}
}

func TestApplyParseFloat(t *testing.T) {
	b := &BridgeOp{Op: "parse_float", Binding: "x"}
	val, err := applyBridge(b, "3.14")
	if err != nil {
		t.Fatal(err)
	}
	if val != 3.14 {
		t.Fatalf("got %v", val)
	}
}

func TestApplySerializeJSON(t *testing.T) {
	b := &BridgeOp{Op: "serialize", Binding: "x", Meta: map[string]interface{}{"format": "json"}}
	val, err := applyBridge(b, map[string]interface{}{"a": 1})
	if err != nil {
		t.Fatal(err)
	}
	s, ok := val.(string)
	if !ok {
		t.Fatalf("expected string, got %T", val)
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDeserializeJSON(t *testing.T) {
	b := &BridgeOp{Op: "deserialize", Binding: "x", Meta: map[string]interface{}{"format": "json"}}
	val, err := applyBridge(b, `{"a":1}`)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := val.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map, got %T", val)
	}
	if m["a"] != float64(1) {
		t.Fatalf("got %v", m["a"])
	}
}

func TestApplyCopyArray(t *testing.T) {
	b := &BridgeOp{Op: "copy_array", Binding: "x"}
	orig := []interface{}{1, 2, 3}
	val, err := applyBridge(b, orig)
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := val.([]interface{})
	if !ok {
		t.Fatalf("expected slice, got %T", val)
	}
	if len(arr) != 3 {
		t.Fatalf("got len %d", len(arr))
	}
}

func TestApplyWrapUnwrapOption(t *testing.T) {
	wrap := &BridgeOp{Op: "wrap_option", Binding: "x"}
	val, err := applyBridge(wrap, 42)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := val.(map[string]interface{})
	if !ok || m["some"] != 42 {
		t.Fatalf("wrap_option: got %v", val)
	}

	unwrap := &BridgeOp{Op: "unwrap_option", Binding: "x"}
	val, err = applyBridge(unwrap, m)
	if err != nil {
		t.Fatal(err)
	}
	if val != 42 {
		t.Fatalf("unwrap_option: got %v", val)
	}

	// unwrap nil
	_, err = applyBridge(unwrap, nil)
	if err == nil {
		t.Fatal("expected error unwrapping nil")
	}
}

func TestApplyWrapUnwrapResult(t *testing.T) {
	wrap := &BridgeOp{Op: "wrap_result", Binding: "x"}
	val, err := applyBridge(wrap, "hello")
	if err != nil {
		t.Fatal(err)
	}
	m := val.(map[string]interface{})
	if m["ok"] != "hello" {
		t.Fatalf("got %v", val)
	}

	unwrap := &BridgeOp{Op: "unwrap_result", Binding: "x"}
	val, err = applyBridge(unwrap, m)
	if err != nil {
		t.Fatal(err)
	}
	if val != "hello" {
		t.Fatalf("got %v", val)
	}

	// unwrap error
	_, err = applyBridge(unwrap, map[string]interface{}{"err": "boom"})
	if err == nil {
		t.Fatal("expected error unwrapping Err result")
	}
}

func TestApplyThrowTyped(t *testing.T) {
	b := &BridgeOp{Op: "throw_typed", Binding: "x", Meta: map[string]interface{}{"errorKind": "ValueError"}}
	_, err := applyBridge(b, map[string]interface{}{"err": "bad input"})
	if err == nil {
		t.Fatal("expected error")
	}
	be := err.(*BridgeError)
	if be.Detail != "ValueError: bad input" {
		t.Fatalf("got %q", be.Detail)
	}

	// Ok path
	val, err := applyBridge(b, map[string]interface{}{"ok": 42})
	if err != nil {
		t.Fatal(err)
	}
	if val != 42 {
		t.Fatalf("got %v", val)
	}
}

func TestApplyCompose(t *testing.T) {
	b := &BridgeOp{
		Op:      "compose",
		Binding: "result",
		From:    "rust",
		To:      "javascript",
		Meta: map[string]interface{}{
			"steps": []interface{}{"unwrap_result", "to_string"},
		},
	}
	val, err := applyBridge(b, map[string]interface{}{"ok": 42})
	if err != nil {
		t.Fatal(err)
	}
	if val != "42" {
		t.Fatalf("got %v", val)
	}
}

func TestApplyComposeWithObjectSteps(t *testing.T) {
	b := &BridgeOp{
		Op:      "compose",
		Binding: "x",
		From:    "python",
		To:      "go",
		Meta: map[string]interface{}{
			"steps": []interface{}{
				map[string]interface{}{
					"op":   "narrow",
					"meta": map[string]interface{}{"to": "i32"},
				},
				"to_string",
			},
		},
	}
	val, err := applyBridge(b, float64(42))
	if err != nil {
		t.Fatal(err)
	}
	if val != "42" {
		t.Fatalf("got %v", val)
	}
}

func TestApplyTagDispatch(t *testing.T) {
	// Rust-style single-key enum → tagged union
	b := &BridgeOp{Op: "tag_dispatch", Binding: "x", Meta: map[string]interface{}{
		"variants": []interface{}{"Ok", "Err"},
	}}
	val, err := applyBridge(b, map[string]interface{}{"Ok": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	m := val.(map[string]interface{})
	if m["tag"] != "Ok" || m["value"] != "hello" {
		t.Fatalf("got %v", val)
	}

	// Already tagged — validate
	val, err = applyBridge(b, map[string]interface{}{"tag": "Ok", "value": "hello"})
	if err != nil {
		t.Fatal(err)
	}

	// Invalid variant
	_, err = applyBridge(b, map[string]interface{}{"tag": "Unknown"})
	if err == nil {
		t.Fatal("expected error for unknown variant")
	}
}

func TestApplyStructReshape(t *testing.T) {
	b := &BridgeOp{Op: "struct_reshape", Binding: "x", Meta: map[string]interface{}{
		"fieldMap": map[string]interface{}{
			"firstName": "first_name",
			"lastName":  "last_name",
		},
	}}
	val, err := applyBridge(b, map[string]interface{}{
		"firstName": "John",
		"lastName":  "Doe",
		"age":       30,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := val.(map[string]interface{})
	if m["first_name"] != "John" || m["last_name"] != "Doe" || m["age"] != 30 {
		t.Fatalf("got %v", m)
	}
}

func TestApplyUnknownOp(t *testing.T) {
	b := &BridgeOp{Op: "quantum_teleport", Binding: "x"}
	_, err := applyBridge(b, 42)
	if err == nil {
		t.Fatal("expected error for unknown op")
	}
}

func TestApplyBridgeOpsJSON(t *testing.T) {
	e := NewExecutor(nil)
	e.bridgeOps = buildBridgeIndex([]*BridgeOp{
		{Binding: "score", Op: "narrow", From: "python", To: "go", Meta: map[string]interface{}{"to": "i32"}},
	})

	// Valid narrowing
	result, err := e.applyBridgeOpsJSON("score", "python", "go", "42")
	if err != nil {
		t.Fatal(err)
	}
	if result != "42" {
		t.Fatalf("got %q", result)
	}

	// No bridge for this crossing — passthrough
	result, err = e.applyBridgeOpsJSON("score", "python", "javascript", "42")
	if err != nil {
		t.Fatal(err)
	}
	if result != "42" {
		t.Fatalf("got %q", result)
	}

	// Overflow
	_, err = e.applyBridgeOpsJSON("score", "python", "go", "99999999999")
	if err == nil {
		t.Fatal("expected overflow error")
	}
}

func TestManifestParseBridges(t *testing.T) {
	raw := `{
		"version": 1,
		"defaultRuntime": "javascript",
		"ops": [],
		"bridges": [
			{"binding": "data", "op": "serialize", "from": "python", "to": "javascript", "meta": {"format": "json"}},
			{"binding": "cb", "op": "proxy_callable", "from": "javascript", "to": "go"}
		],
		"typeSummary": {"crossings": 5, "safe": 2, "coerce": 1, "check": 1, "errors": 1}
	}`
	m, err := ParseManifest([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Bridges) != 2 {
		t.Fatalf("expected 2 bridges, got %d", len(m.Bridges))
	}
	if m.Bridges[0].Op != "serialize" {
		t.Fatalf("got %q", m.Bridges[0].Op)
	}
	if m.TypeSummary == nil {
		t.Fatal("expected typeSummary")
	}
	if m.TypeSummary.Errors != 1 {
		t.Fatalf("expected 1 error, got %d", m.TypeSummary.Errors)
	}
}

func TestSerializeMsgpackUnsupported(t *testing.T) {
	b := &BridgeOp{Op: "serialize", Binding: "x", Meta: map[string]interface{}{"format": "msgpack"}}
	_, err := applyBridge(b, "hello")
	if err == nil {
		t.Fatal("expected msgpack unsupported error")
	}
}

func TestUnwrapOptionStringNull(t *testing.T) {
	b := &BridgeOp{Op: "unwrap_option", Binding: "x"}
	for _, s := range []string{"null", "None", "nil", "undefined"} {
		_, err := applyBridge(b, s)
		if err == nil {
			t.Fatalf("expected error for %q", s)
		}
	}
	// Non-null string passes through
	val, err := applyBridge(b, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if val != "hello" {
		t.Fatalf("got %v", val)
	}
}
