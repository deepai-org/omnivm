package manifest

import (
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// TestRustDynForLoopAndDictSugar: `for x in dyn` compiles for owned and
// borrowed Dyn (IntoIterator, arrays-only), and dicts expose Python-style
// .keys()/.values()/.items(). Non-iterables panic with the Python TypeError
// dialect and surface as structured, catchable errors.
func TestRustDynForLoopAndDictSugar(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	source := `
fn dyn_iter_report(reviews: omnivm::Dyn, weights: omnivm::Dyn) -> impl omnivm::serde::Serialize {
    // Borrowed iteration: yields &Dyn, the receiver stays usable.
    let mut peak = 0.0;
    for r in &reviews {
        if r["score"] > peak { peak = r["score"].as_f64(); }
    }
    let n = reviews.len();
    // Owned iteration: plain "for r in reviews" consumes the Dyn.
    let mut total = 0.0;
    for r in reviews {
        total = total + r["score"].as_f64();
    }
    // Dict sugar: keys / values / items.
    let keys: Vec<&str> = weights.keys().collect();
    let weight_sum: f64 = weights.values().map(|v| v.as_f64()).sum();
    let mut tagged = String::new();
    for (k, v) in weights.items() {
        tagged.push_str(&format!("{k}={v};"));
    }
    format!("peak={peak} total={total} n={n} keys={} weight_sum={weight_sum} tagged={tagged}", keys.join(","))
}

fn dyn_iter_misuse(value: omnivm::Dyn) -> i64 {
    let mut count = 0i64;
    for _ in value { count += 1; }
    count
}

omnivm::export_fn!(OmniVMCall_dyn_iter_report, dyn_iter_report, 2);
omnivm::export_fn!(OmniVMCall_dyn_iter_misuse, dyn_iter_misuse, 1);
`
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "dyn_iter_report", BodyRuntime: "rust",
		Params:  []*Param{{Name: "reviews"}, {Name: "weights"}},
		Exports: []string{"dyn_iter_report", "dyn_iter_misuse"},
		Source:  source,
	}}}); err != nil {
		t.Fatalf("compile: %v", err)
	}

	reviews := []interface{}{
		map[string]interface{}{"score": 4.5},
		map[string]interface{}{"score": 3.25},
		map[string]interface{}{"score": 4.75},
	}
	weights := map[string]interface{}{"a": 1.5, "b": 2.5}
	value, err := e.goFuncs["dyn_iter_report"].(func([]interface{}) (interface{}, error))(
		[]interface{}{reviews, weights})
	if err != nil {
		t.Fatalf("dyn_iter_report: %v", err)
	}
	want := "peak=4.75 total=12.5 n=3 keys=a,b weight_sum=4 tagged=a=1.5;b=2.5;"
	if value != want {
		t.Fatalf("dyn_iter_report = %#v, want %q", value, want)
	}

	// Iterating a non-array panics Python-style and stays catchable.
	_, err = e.goFuncs["dyn_iter_misuse"].(func([]interface{}) (interface{}, error))(
		[]interface{}{map[string]interface{}{"oops": 1}})
	if err == nil || !strings.Contains(err.Error(), "'dict' object is not iterable") {
		t.Fatalf("expected python-style TypeError, got %v", err)
	}
}
