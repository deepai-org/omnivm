package manifest

import (
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// numericValue flattens the envelope's numeric representations (typed-lane
// ints decode as int/int64, JSON-lane numbers as float64) for assertions.
func numericValue(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case float32:
		return float64(n), true
	case float64:
		return n, true
	default:
		return 0, false
	}
}

// Boundary generics (docs/rust-boundary-generics.md): these tests pin the
// exact glue SHAPES the polyscript codegen emits for Tier 2 (autoref-pruned
// lattice dispatcher) and Tier 3 (the Dyn instantiation) against the real
// toolchain — the unit must compile, dispatch by serde tag, prune candidates
// that fail the bounds via autoref, and fail CATCHABLY when no candidate
// (including Dyn) satisfies them.

// TestRustBoundaryGenericsTier2Dispatcher: a generic fn whose bound set is
// OUTSIDE the Dyn vocabulary (Copy). The dispatcher routes int args to the
// i64 instantiation (rustc kept its probe: i64 is Copy), routes string args
// through Miss arms on both String (not Copy at the lattice... it is not
// Copy) and Dyn (not Copy), and panics with the structured TypeError the
// envelope converts into a catchable error.
func TestRustBoundaryGenericsTier2Dispatcher(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	source := `
fn add_self<T: std::ops::Add<Output = T> + Copy>(x: T) -> T {
    x + x
}

#[allow(non_camel_case_types, dead_code)]
struct __OmnivmProbe_add_self<T>(::std::marker::PhantomData<T>);
#[allow(non_camel_case_types)]
trait __OmnivmHit_add_self { fn __omnivm_probe(self, __args: Vec<omnivm::serde_json::Value>) -> Result<omnivm::serde_json::Value, String>; }
impl<T> __OmnivmHit_add_self for __OmnivmProbe_add_self<T>
where T: std::ops::Add<Output = T> + Copy + omnivm::serde::Serialize + omnivm::serde::de::DeserializeOwned {
    fn __omnivm_probe(self, __args: Vec<omnivm::serde_json::Value>) -> Result<omnivm::serde_json::Value, String> {
        let mut __it = __args.into_iter();
        let __a0: T = omnivm::serde_json::from_value(__it.next().unwrap_or(omnivm::serde_json::Value::Null)).map_err(|e| format!("argument 1 of 'add_self': {e}"))?;
        omnivm::serde_json::to_value(add_self(__a0)).map_err(|e| e.to_string())
    }
}
#[allow(non_camel_case_types)]
trait __OmnivmMiss_add_self { fn __omnivm_probe(self, __args: Vec<omnivm::serde_json::Value>) -> Result<omnivm::serde_json::Value, String>; }
impl<T> __OmnivmMiss_add_self for &__OmnivmProbe_add_self<T> {
    fn __omnivm_probe(self, _args: Vec<omnivm::serde_json::Value>) -> Result<omnivm::serde_json::Value, String> {
        Err(format!("candidate {} does not satisfy the declared bounds",
            ::std::any::type_name::<T>()))
    }
}
fn __omnivm_dispatch_add_self(__omnivm_a0: omnivm::Dyn) -> omnivm::Dyn {
    let __args: Vec<omnivm::serde_json::Value> = vec![__omnivm_a0.0];
    let __lattice = if __args.iter().all(|v| v.as_i64().is_some()) {
        __OmnivmProbe_add_self::<i64>(::std::marker::PhantomData).__omnivm_probe(__args.clone())
    } else if __args.iter().all(|v| v.is_number()) {
        __OmnivmProbe_add_self::<f64>(::std::marker::PhantomData).__omnivm_probe(__args.clone())
    } else if __args.iter().all(|v| v.is_boolean()) {
        __OmnivmProbe_add_self::<bool>(::std::marker::PhantomData).__omnivm_probe(__args.clone())
    } else if __args.iter().all(|v| v.is_string()) {
        __OmnivmProbe_add_self::<String>(::std::marker::PhantomData).__omnivm_probe(__args.clone())
    } else {
        Err("no scalar candidate matches the argument tags".to_string())
    };
    match __lattice {
        Ok(v) => omnivm::Dyn(v),
        Err(__lattice_err) => {
            match __OmnivmProbe_add_self::<omnivm::Dyn>(::std::marker::PhantomData).__omnivm_probe(__args) {
                Ok(v) => omnivm::Dyn(v),
                Err(__dyn_err) => panic!(
                    "TypeError: no boundary instantiation of 'add_self' accepts these arguments: {__lattice_err}; dynamic fallback (omnivm::Dyn): {__dyn_err}"
                ),
            }
        }
    }
}
omnivm::export_fn!(OmniVMCall_add_self, __omnivm_dispatch_add_self, 1);
`
	m := &Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "add_self", BodyRuntime: "rust",
		Params:  []*Param{{Name: "x"}},
		Exports: []string{"add_self"}, Source: source,
	}}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("compile: %v", err)
	}
	call := e.goFuncs["add_self"].(func([]interface{}) (interface{}, error))

	// Int tag → the i64 candidate (i64 is Copy: the Hit probe survived).
	value, err := call([]interface{}{int64(21)})
	if err != nil {
		t.Fatalf("int call: %v", err)
	}
	if v, ok := numericValue(value); !ok || v != 42 {
		t.Fatalf("int call = %#v, want 42", value)
	}

	// Float tag → the f64 candidate.
	value, err = call([]interface{}{1.5})
	if err != nil {
		t.Fatalf("float call: %v", err)
	}
	if v, ok := numericValue(value); !ok || v != 3.0 {
		t.Fatalf("float call = %#v, want 3.0", value)
	}

	// String tag: String is not Copy and neither is Dyn — both probes
	// resolve to the autoref Miss arm, and the dispatcher's panic surfaces
	// as a catchable structured error naming both failures.
	_, err = call([]interface{}{"oops"})
	if err == nil {
		t.Fatalf("expected a catchable TypeError for the string tag")
	}
	for _, want := range []string{
		"TypeError: no boundary instantiation of 'add_self'",
		"does not satisfy the declared bounds",
		"dynamic fallback (omnivm::Dyn)",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q missing %q", err.Error(), want)
		}
	}

	// The unit (and worker) still works after the panic.
	value, err = call([]interface{}{int64(5)})
	if v, ok := numericValue(value); err != nil || !ok || v != 10 {
		t.Fatalf("call after panic = %#v, %v", value, err)
	}
}

// TestRustBoundaryGenericsTier3DynWrapper: the f::<Dyn> instantiation for a
// PartialOrd+Clone-bounded generic with a Vec<T> param — exactly the wrapper
// codegen emits, exported under the ORIGINAL name. Trait bounds become
// gradual contracts: a homogeneous int list works, and the heterogeneous
// comparison semantics are Dyn's (canonical total order), never a crash.
func TestRustBoundaryGenericsTier3DynWrapper(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	source := `
fn max_of<T: PartialOrd + Clone>(items: Vec<T>) -> T {
    let mut best = items[0].clone();
    for it in items.iter() {
        if *it > best { best = it.clone(); }
    }
    best
}

fn __omnivm_dyn_max_of(items: Vec<omnivm::Dyn>) -> omnivm::Dyn {
    max_of(items)
}
omnivm::export_fn!(OmniVMCall_max_of, __omnivm_dyn_max_of, 1);
`
	m := &Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "max_of", BodyRuntime: "rust",
		Params:  []*Param{{Name: "items"}},
		Exports: []string{"max_of"}, Source: source,
	}}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("compile: %v", err)
	}
	call := e.goFuncs["max_of"].(func([]interface{}) (interface{}, error))

	value, err := call([]interface{}{[]interface{}{int64(3), int64(11), int64(7)}})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if v, ok := numericValue(value); !ok || v != 11 {
		t.Fatalf("max_of = %#v, want 11", value)
	}

	value, err = call([]interface{}{[]interface{}{"alpha", "omega", "beta"}})
	if err != nil {
		t.Fatalf("string call: %v", err)
	}
	if value != "omega" {
		t.Fatalf("max_of strings = %#v, want omega", value)
	}

	// Empty list: the fn's own items[0] panics — catchable, worker survives.
	if _, err = call([]interface{}{[]interface{}{}}); err == nil {
		t.Fatalf("expected a catchable error for the empty list")
	}
	value, err = call([]interface{}{[]interface{}{int64(1), int64(2)}})
	if v, ok := numericValue(value); err != nil || !ok || v != 2 {
		t.Fatalf("call after panic = %#v, %v", value, err)
	}
}
