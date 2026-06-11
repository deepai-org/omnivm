package manifest

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/rust"
)

// TestRustTypedOutboundCall: rust calls a Go-registered function through the
// typed omni_value_t lane (omnivm::call_typed_fn) — i64/f64/bool/string args
// and a string result cross with no JSON text, proven by the trampoline
// counter. Error results come back as structured OmniError values, and
// non-scalar results arrive losslessly through the JSON tag.
func TestRustTypedOutboundCall(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})

	var seenArgs []interface{}
	e.goFuncs["typed_boost"] = func(args []interface{}) (interface{}, error) {
		seenArgs = append([]interface{}(nil), args...)
		if len(args) != 4 {
			return nil, fmt.Errorf("got %d args", len(args))
		}
		a, _ := args[0].(int64)
		b, _ := args[1].(float64)
		c, _ := args[2].(string)
		d, _ := args[3].(bool)
		return fmt.Sprintf("%s:%d:%v:%v", c, a*2, b+0.5, d), nil
	}
	e.goFuncs["typed_fail"] = func(args []interface{}) (interface{}, error) {
		return nil, fmt.Errorf("boom from go")
	}
	e.goFuncs["typed_dict"] = func(args []interface{}) (interface{}, error) {
		return map[string]interface{}{"n": int64(7), "tag": "deep"}, nil
	}

	source := `
use omnivm::{FromOmniValue, ToOmniValue};

fn typed_relay(a: i64, b: f64, c: String) -> String {
    let out = omnivm::call_typed_fn(
        "go",
        "typed_boost",
        &[a.to_omni(), b.to_omni(), c.to_omni(), true.to_omni()],
    )
    .unwrap();
    String::from_omni(&out).unwrap()
}

fn typed_relay_err() -> String {
    match omnivm::call_typed_fn("go", "typed_fail", &[]) {
        Ok(_) => "no-error".to_string(),
        Err(e) => format!("err={e}"),
    }
}

fn typed_relay_json() -> String {
    let out = omnivm::call_typed_fn("go", "typed_dict", &[]).unwrap();
    let value = out.to_json_value();
    format!("n={} tag={}", value["n"].as_i64().unwrap_or(-1), value["tag"].as_str().unwrap_or("?"))
}

omnivm::export_fn!(OmniVMCall_typed_relay, typed_relay, 3);
omnivm::export_fn!(OmniVMCall_typed_relay_err, typed_relay_err, 0);
omnivm::export_fn!(OmniVMCall_typed_relay_json, typed_relay_json, 0);
`
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "typed_relay", BodyRuntime: "rust",
		Params:  []*Param{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		Exports: []string{"typed_relay", "typed_relay_err", "typed_relay_json"},
		Source:  source,
	}}}); err != nil {
		t.Fatalf("compile: %v", err)
	}

	call := func(name string, args ...interface{}) (interface{}, error) {
		return e.goFuncs[name].(func([]interface{}) (interface{}, error))(args)
	}

	// b is non-integral on purpose: normalizeArg folds whole floats to int,
	// which would mask the f64 tag assertion below.
	before := atomic.LoadUint64(&rust.TypedBridgeCallCount)
	value, err := call("typed_relay", int64(21), 2.25, "answer")
	if err != nil {
		t.Fatalf("typed_relay: %v", err)
	}
	if value != "answer:42:2.75:true" {
		t.Fatalf("typed_relay = %#v, want answer:42:2.75:true", value)
	}
	if got := atomic.LoadUint64(&rust.TypedBridgeCallCount); got != before+1 {
		t.Fatalf("typed outbound lane not taken (count %d -> %d)", before, got)
	}
	if len(seenArgs) != 4 {
		t.Fatalf("go func saw %d args: %#v", len(seenArgs), seenArgs)
	}
	if _, isInt := seenArgs[0].(int64); !isInt {
		t.Fatalf("arg 0 crossed as %T, want int64 (typed lane)", seenArgs[0])
	}
	if _, isFloat := seenArgs[1].(float64); !isFloat {
		t.Fatalf("arg 1 crossed as %T, want float64 (typed lane)", seenArgs[1])
	}

	// Error path: a Go error surfaces as a structured OmniError in rust.
	value, err = call("typed_relay_err")
	if err != nil {
		t.Fatalf("typed_relay_err: %v", err)
	}
	if s, _ := value.(string); !strings.Contains(s, "boom from go") {
		t.Fatalf("error path = %#v, want it to carry 'boom from go'", value)
	}

	// Non-scalar result: crosses losslessly as JSON-tagged text.
	value, err = call("typed_relay_json")
	if err != nil {
		t.Fatalf("typed_relay_json: %v", err)
	}
	if value != "n=7 tag=deep" {
		t.Fatalf("json-tag path = %#v, want n=7 tag=deep", value)
	}
}

// TestRustTypedOutboundFallback: targets the host does not speak typed for
// (a manifest func_def, absent from the Go registry) report unhandled BEFORE
// executing, and the crate transparently rides the JSON {func,args} lane —
// no double execution, no typed-counter increment.
func TestRustTypedOutboundFallback(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "func_def", Name: "fortytwo", Body: []*Op{
			{OpType: "return", Value: &ValueExpr{Kind: "literal", Value: 42}},
		}},
		{
			OpType: "func_def", Name: "typed_fallback", BodyRuntime: "rust",
			Exports: []string{"typed_fallback"},
			Source: `
use omnivm::FromOmniValue;

fn typed_fallback() -> i64 {
    let out = omnivm::call_typed_fn("__manifest", "fortytwo", &[]).unwrap();
    i64::from_omni(&out).unwrap()
}
`,
		},
	}}); err != nil {
		t.Fatalf("compile: %v", err)
	}

	before := atomic.LoadUint64(&rust.TypedBridgeCallCount)
	value, err := e.goFuncs["typed_fallback"].(func([]interface{}) (interface{}, error))(nil)
	if err != nil {
		t.Fatalf("typed_fallback: %v", err)
	}
	if !numEquals(value, 42) {
		t.Fatalf("typed_fallback = %#v, want 42", value)
	}
	if got := atomic.LoadUint64(&rust.TypedBridgeCallCount); got != before {
		t.Fatalf("fallback must not count as a typed service (%d -> %d)", before, got)
	}
}

// TestRustTypedOutboundAsyncHop: omnivm::call_typed_fn_async from async
// context rides the bridge hop (the park exits before the host services the
// call) and resolves to the same typed result surface.
func TestRustTypedOutboundAsyncHop(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	e.goFuncs["typed_double"] = func(args []interface{}) (interface{}, error) {
		// The hop rides the JSON lane, so whole numbers normalize to int.
		switch v := args[0].(type) {
		case int:
			return int64(v) * 2, nil
		case int64:
			return v * 2, nil
		case float64:
			return int64(v) * 2, nil
		}
		return nil, fmt.Errorf("unexpected arg type %T", args[0])
	}
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "hop_double", BodyRuntime: "rust", Async: true,
		Params:  []*Param{{Name: "n"}},
		Exports: []string{"hop_double"},
		Source: `
use omnivm::{FromOmniValue, ToOmniValue};

async fn hop_double(n: i64) -> i64 {
    // Two sequential hops prove the future resumes after each park exit.
    let once = omnivm::call_typed_fn_async("go", "typed_double", &[n.to_omni()]).await.unwrap();
    let once = i64::from_omni(&once).unwrap();
    let twice = omnivm::call_typed_fn_async("go", "typed_double", &[once.to_omni()]).await.unwrap();
    i64::from_omni(&twice).unwrap()
}
`,
	}, {OpType: "eval", Runtime: "rust", Code: "hop_double(5)", Bind: "out"}}}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, _ := e.getBinding("out")
	if !numEquals(out, 20) {
		t.Fatalf("hop_double = %#v, want 20", out)
	}
}
