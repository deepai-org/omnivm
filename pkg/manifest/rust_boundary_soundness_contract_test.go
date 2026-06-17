package manifest

import (
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// TestRustBoundarySoundnessTypedLane proves the boundary guarantee at the
// typed omni_value_t scalar lane against the REAL toolchain:
//
//	"No silent type confusion at the boundary — lossless coercions are
//	automatic; lossy or cross-kind conversions are structured, catchable
//	errors."
//
// A typed export `need_int(i64)` is called with 2.0 (lossless -> 2), with 2.7
// (fractional float, must be a catchable error mentioning "fractional", NOT a
// silent truncation to 2), and with a bool (cross-kind, catchable error). A
// `need_bool(bool)` is called with an int (cross-kind, catchable error). After
// EACH rejection the worker must survive: the next call still works.
//
// Args reach the typed lane unnormalized: the executor's goFunc forwards them
// straight into CallTypedByAddr, which encodes a Go float64 as the F64 tag and
// a Go bool as the BOOL tag. So 2.0/2.7 genuinely arrive as floats (not folded
// to int by normalizeArg, which only runs on the JSON-lane path / return
// values), exercising abi::FromOmniValue for i64 / bool exactly.
func TestRustBoundarySoundnessTypedLane(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})

	source := `
fn need_int(x: i64) -> i64 {
    x + 1
}

fn need_bool(b: bool) -> bool {
    !b
}

omnivm::export_fn!(OmniVMCall_need_int, need_int, 1);
omnivm::export_typed_fn!(OmniVMCallTyped_need_int, need_int, 1);
omnivm::export_fn!(OmniVMCall_need_bool, need_bool, 1);
omnivm::export_typed_fn!(OmniVMCallTyped_need_bool, need_bool, 1);
`
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "need_int", BodyRuntime: "rust",
		Params:  []*Param{{Name: "x"}},
		Exports: []string{"need_int", "need_bool"},
		Source:  source,
	}}}); err != nil {
		t.Fatalf("compile: %v", err)
	}

	call := func(name string, arg interface{}) (interface{}, error) {
		fn := e.goFuncs[name].(func([]interface{}) (interface{}, error))
		return fn([]interface{}{arg})
	}

	// --- need_int ---------------------------------------------------------

	// 2.0 is an integral float: lossless coercion to 2, body returns 3.
	got, err := call("need_int", float64(2.0))
	if err != nil {
		t.Fatalf("need_int(2.0): unexpected error: %v", err)
	}
	if !numEquals(got, 3) {
		t.Fatalf("need_int(2.0) = %#v, want 3 (lossless 2.0->2)", got)
	}

	// 2.7 is fractional: must be a catchable error mentioning "fractional",
	// NEVER a silent truncation to 2 (which would have returned 3).
	got, err = call("need_int", float64(2.7))
	if err == nil {
		t.Fatalf("need_int(2.7) silently succeeded with %#v — truncation hole is open", got)
	}
	if !strings.Contains(err.Error(), "fractional") {
		t.Fatalf("need_int(2.7) error %q, want it to mention 'fractional'", err)
	}

	// Worker survives a lossy rejection: the next call still works.
	got, err = call("need_int", int64(41))
	if err != nil {
		t.Fatalf("need_int(41) after 2.7 rejection: worker did not survive: %v", err)
	}
	if !numEquals(got, 42) {
		t.Fatalf("need_int(41) = %#v, want 42", got)
	}

	// A bool reaching an i64 param is cross-kind: catchable error, no 0/1.
	got, err = call("need_int", true)
	if err == nil {
		t.Fatalf("need_int(true) silently succeeded with %#v — cross-kind hole is open", got)
	}
	if !strings.Contains(err.Error(), "cross-kind") {
		t.Fatalf("need_int(true) error %q, want it to mention 'cross-kind'", err)
	}

	// Worker survives the cross-kind rejection too.
	got, err = call("need_int", int64(7))
	if err != nil {
		t.Fatalf("need_int(7) after bool rejection: worker did not survive: %v", err)
	}
	if !numEquals(got, 8) {
		t.Fatalf("need_int(7) = %#v, want 8", got)
	}

	// --- need_bool --------------------------------------------------------

	// A real bool works: !true == false.
	got, err = call("need_bool", true)
	if err != nil {
		t.Fatalf("need_bool(true): unexpected error: %v", err)
	}
	if b, ok := got.(bool); !ok || b != false {
		t.Fatalf("need_bool(true) = %#v, want false", got)
	}

	// An int reaching a bool param is cross-kind: catchable error, NOT a
	// truthy coercion.
	got, err = call("need_bool", int64(1))
	if err == nil {
		t.Fatalf("need_bool(1) silently succeeded with %#v — int->bool hole is open", got)
	}
	if !strings.Contains(err.Error(), "cross-kind") {
		t.Fatalf("need_bool(1) error %q, want it to mention 'cross-kind'", err)
	}

	// Worker survives, and the valid path still works afterward.
	got, err = call("need_bool", false)
	if err != nil {
		t.Fatalf("need_bool(false) after int rejection: worker did not survive: %v", err)
	}
	if b, ok := got.(bool); !ok || b != true {
		t.Fatalf("need_bool(false) = %#v, want true", got)
	}
}
