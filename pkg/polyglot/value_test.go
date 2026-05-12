package polyglot

import "testing"

func TestValueNull(t *testing.T) {
	v := Null()
	if v.Tag != TagNull || !v.IsNull() {
		t.Fatal("expected null")
	}
	cv := v.ToCValue()
	v2 := FromCValue(cv)
	if v2.Tag != TagNull {
		t.Fatal("roundtrip null failed")
	}
}

func TestValueBool(t *testing.T) {
	for _, b := range []bool{true, false} {
		v := Bool(b)
		if v.Tag != TagBool {
			t.Fatalf("expected bool tag")
		}
		cv := v.ToCValue()
		v2 := FromCValue(cv)
		FreeCValue(cv)
		got := v2.Int != 0
		if got != b {
			t.Fatalf("bool roundtrip: expected %v, got %v", b, got)
		}
	}
}

func TestValueI64(t *testing.T) {
	for _, i := range []int64{0, 1, -1, 1<<62, -(1 << 62)} {
		v := I64(i)
		cv := v.ToCValue()
		v2 := FromCValue(cv)
		FreeCValue(cv)
		if v2.Int != i {
			t.Fatalf("i64 roundtrip: expected %d, got %d", i, v2.Int)
		}
	}
}

func TestValueF64(t *testing.T) {
	for _, f := range []float64{0, 3.14159265, -1e300, 1e-300} {
		v := F64(f)
		cv := v.ToCValue()
		v2 := FromCValue(cv)
		FreeCValue(cv)
		if v2.Float != f {
			t.Fatalf("f64 roundtrip: expected %g, got %g", f, v2.Float)
		}
	}
}

func TestValueString(t *testing.T) {
	for _, s := range []string{"", "hello", "hello world with spaces", "日本語"} {
		v := String(s)
		cv := v.ToCValue()
		v2 := FromCValue(cv)
		FreeCValue(cv)
		if v2.Str != s {
			t.Fatalf("string roundtrip: expected %q, got %q", s, v2.Str)
		}
	}
}

func TestValueBytes(t *testing.T) {
	data := make([]byte, 1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	v := BytesVal(data)
	cv := v.ToCValue()
	v2 := FromCValue(cv)
	FreeCValue(cv)
	if len(v2.Bytes) != len(data) {
		t.Fatalf("bytes roundtrip: expected len %d, got %d", len(data), len(v2.Bytes))
	}
	for i := range data {
		if v2.Bytes[i] != data[i] {
			t.Fatalf("bytes mismatch at %d", i)
		}
	}
}

func TestValueError(t *testing.T) {
	v := Error("something failed")
	if !v.IsError() {
		t.Fatal("expected error")
	}
	cv := v.ToCValue()
	v2 := FromCValue(cv)
	FreeCValue(cv)
	if v2.Str != "something failed" {
		t.Fatalf("error roundtrip: expected %q, got %q", "something failed", v2.Str)
	}
}

func TestValueRef(t *testing.T) {
	v := Ref(12345)
	cv := v.ToCValue()
	v2 := FromCValue(cv)
	FreeCValue(cv)
	if v2.Ref != 12345 {
		t.Fatalf("ref roundtrip: expected 12345, got %d", v2.Ref)
	}
}

func TestToGoString(t *testing.T) {
	tests := []struct {
		v    Value
		want string
	}{
		{Null(), "null"},
		{Bool(true), "true"},
		{Bool(false), "false"},
		{I64(42), "42"},
		{F64(3.14), "3.14"},
		{String("hello"), `"hello"`},
		{Error("oops"), "ERR:oops"},
	}
	for _, tt := range tests {
		got := tt.v.ToGoString()
		if got != tt.want {
			t.Errorf("ToGoString(%d): expected %q, got %q", tt.v.Tag, tt.want, got)
		}
	}
}

func TestBuiltins(t *testing.T) {
	RegisterBuiltins()

	tests := []struct {
		fn   string
		args []Value
		want Value
	}{
		{"math.abs", []Value{I64(-42)}, I64(42)},
		{"math.abs", []Value{F64(-3.14)}, F64(3.14)},
		{"math.sqrt", []Value{I64(25)}, F64(5)},
		{"math.pow", []Value{I64(2), I64(10)}, F64(1024)},
		{"math.floor", []Value{F64(3.7)}, I64(3)},
		{"math.ceil", []Value{F64(3.2)}, I64(4)},
		{"math.min", []Value{I64(3), I64(7), I64(1)}, I64(1)},
		{"math.max", []Value{I64(3), I64(7), I64(1)}, I64(7)},
		{"str.len", []Value{String("hello")}, I64(5)},
		{"str.upper", []Value{String("hello")}, String("HELLO")},
		{"str.lower", []Value{String("HELLO")}, String("hello")},
		{"str.contains", []Value{String("hello world"), String("world")}, Bool(true)},
		{"str.replace", []Value{String("hello"), String("l"), String("r")}, String("herro")},
		{"int", []Value{F64(3.14)}, I64(3)},
		{"float", []Value{I64(42)}, F64(42)},
		{"echo", []Value{I64(99)}, I64(99)},
	}

	for _, tt := range tests {
		got := GlobalRegistry.Call("go", tt.fn, tt.args)
		if got.Tag != tt.want.Tag || got.Int != tt.want.Int || got.Float != tt.want.Float || got.Str != tt.want.Str {
			t.Errorf("go.%s: expected %+v, got %+v", tt.fn, tt.want, got)
		}
	}
}

func TestEmptyBytesRoundtrip(t *testing.T) {
	v := BytesVal(nil)
	cv := v.ToCValue()
	v2 := FromCValue(cv)
	FreeCValue(cv)
	if len(v2.Bytes) != 0 {
		t.Fatalf("expected empty bytes, got %d", len(v2.Bytes))
	}
}
