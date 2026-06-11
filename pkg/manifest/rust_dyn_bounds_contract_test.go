package manifest

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// TestRustDynBoundedGenerics: the Tier-3 boundary-generics contract
// (docs/rust-boundary-generics.md) end-to-end. A hand-written unit declares
// BOUNDED generic fns — byte-verbatim, never rewritten — plus concrete
// `f::<Dyn>` instantiations (exactly what the compiler's Tier-3 auto-export
// emits when a generic's bounds are a subset of the Dyn vocabulary):
//
//   - `T: PartialOrd + Clone`  (smallest)
//   - `T: Into<f64>`           (mean — coerce-or-panic)
//   - `T: Ord`                 (dedup via BTreeSet, canonical total order)
//   - `T: Hash + Eq`           (distinct count via HashSet)
//
// Bound failures are gradual contracts: checked at the moment of use and
// surfaced as the catchable Python-style TypeError dialect.
func TestRustDynBoundedGenerics(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	source := `
use std::collections::{BTreeSet, HashSet};

// The generic declarations stay byte-verbatim; omnivm::Dyn is just another
// valid instantiation target for their bounds.

fn smallest<T: PartialOrd + Clone>(items: Vec<T>) -> T {
    let mut it = items.into_iter();
    let mut best = it.next().expect("ValueError: smallest() arg is an empty sequence");
    for x in it {
        if x < best { best = x; }
    }
    best
}

fn mean<T: Into<f64>>(items: Vec<T>) -> f64 {
    let n = items.len() as f64;
    let total: f64 = items.into_iter().map(Into::into).sum();
    total / n
}

fn dedup_sorted<T: Ord>(items: Vec<T>) -> Vec<T> {
    items.into_iter().collect::<BTreeSet<T>>().into_iter().collect()
}

fn distinct_count<T: std::hash::Hash + Eq>(items: Vec<T>) -> i64 {
    items.into_iter().collect::<HashSet<T>>().len() as i64
}

// The Tier-3 instantiations the boundary exports.
fn smallest_dyn(items: Vec<omnivm::Dyn>) -> omnivm::Dyn { smallest(items) }
fn mean_dyn(items: Vec<omnivm::Dyn>) -> f64 { mean(items) }
fn dedup_dyn(items: Vec<omnivm::Dyn>) -> Vec<omnivm::Dyn> { dedup_sorted(items) }
fn distinct_dyn(items: Vec<omnivm::Dyn>) -> i64 { distinct_count(items) }
`
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "smallest_dyn", BodyRuntime: "rust",
		Params:  []*Param{{Name: "items"}},
		Exports: []string{"smallest_dyn", "mean_dyn", "dedup_dyn", "distinct_dyn"},
		Source:  source,
	}}}); err != nil {
		t.Fatalf("compile: %v", err)
	}
	call := func(name string, items []interface{}) (interface{}, error) {
		return e.goFuncs[name].(func([]interface{}) (interface{}, error))(
			[]interface{}{items})
	}

	// T: PartialOrd + Clone — Dyn-vs-Dyn `<` is the canonical order, so the
	// int 2 is the smallest of the mixed numeric list.
	value, err := call("smallest_dyn", []interface{}{4.5, int64(2), 3.25})
	if err != nil {
		t.Fatalf("smallest_dyn: %v", err)
	}
	if !numEquals(value, 2) {
		t.Fatalf("smallest_dyn = %#v, want 2", value)
	}

	// T: Into<f64> — coerce-or-panic: ints and bools coerce like as_f64.
	value, err = call("mean_dyn", []interface{}{int64(1), int64(2), 3.5, true})
	if err != nil {
		t.Fatalf("mean_dyn: %v", err)
	}
	if !numEquals(value, 1.875) {
		t.Fatalf("mean_dyn = %#v, want 1.875", value)
	}

	// The bound is a gradual contract: a non-coercible element panics with
	// the Python dialect at the moment of use and stays catchable.
	_, err = call("mean_dyn", []interface{}{int64(1), "oops"})
	if err == nil || !strings.Contains(err.Error(), "TypeError: expected float, got 'str'") {
		t.Fatalf("expected catchable TypeError from Into<f64>, got %v", err)
	}

	// The unit (and worker) still works after the contract violation.
	value, err = call("mean_dyn", []interface{}{int64(4)})
	if err != nil {
		t.Fatalf("mean_dyn after panic: %v", err)
	}
	if !numEquals(value, 4) {
		t.Fatalf("mean_dyn after panic = %#v, want 4", value)
	}

	// T: Ord — BTreeSet dedups and yields the canonical total order:
	// type rank (bool < number < string), then value.
	value, err = call("dedup_dyn", []interface{}{
		int64(3), int64(1), true, "b", int64(1), "a", 2.5, false, true})
	if err != nil {
		t.Fatalf("dedup_dyn: %v", err)
	}
	got, merr := json.Marshal(value)
	if merr != nil {
		t.Fatalf("marshal dedup_dyn result: %v", merr)
	}
	want := `[false,true,1,2.5,3,"a","b"]`
	if string(got) != want {
		t.Fatalf("dedup_dyn = %s, want %s", got, want)
	}

	// T: Hash + Eq — HashSet sees {1, 2.5, "a", true}: 4 distinct values.
	value, err = call("distinct_dyn", []interface{}{
		int64(1), int64(1), 2.5, 2.5, "a", "a", true})
	if err != nil {
		t.Fatalf("distinct_dyn: %v", err)
	}
	if !numEquals(value, 4) {
		t.Fatalf("distinct_dyn = %#v, want 4", value)
	}
}
