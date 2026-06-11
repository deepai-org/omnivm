package manifest

import (
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// TestRustDynGradualUnit: the gradual-typing boundary type. A hand-written
// unit declares `omnivm::Dyn` params explicitly (exactly what the compiler's
// signature completion emits for `fn top_review(reviews, min)`) and exercises
// the dynamic surface: ["key"] indexing, iteration, arithmetic and
// comparisons against native scalars, len(), and the
// `-> impl omnivm::serde::Serialize` completed return.
func TestRustDynGradualUnit(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	source := `
fn top_review(reviews: omnivm::Dyn, min: omnivm::Dyn) -> impl omnivm::serde::Serialize {
    let mut best = 0.0;
    let mut count: i64 = 0;
    for r in reviews.iter() {
        if r["score"] > min { count = count + 1; }
        if r["score"] > best { best = r["score"].as_f64(); }
    }
    let bonus = &min + 1;
    format!("best={best} count={count} len={} bonus={bonus}", reviews.len())
}
`
	m := &Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "top_review", BodyRuntime: "rust",
		Params:  []*Param{{Name: "reviews"}, {Name: "min"}},
		Exports: []string{"top_review"}, Source: source,
	}}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("compile: %v", err)
	}
	call := e.goFuncs["top_review"].(func([]interface{}) (interface{}, error))

	reviews := []interface{}{
		map[string]interface{}{"score": 4.5, "id": 1},
		map[string]interface{}{"score": 3.0, "id": 2},
		map[string]interface{}{"score": 4.9, "id": 3},
	}
	value, err := call([]interface{}{reviews, int64(4)})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if value != "best=4.9 count=2 len=3 bonus=5" {
		t.Fatalf("value = %#v", value)
	}

	// Dynamic type errors panic with Python-style messages and surface as
	// structured, catchable errors — never a dead worker.
	_, err = call([]interface{}{[]interface{}{"oops"}, int64(0)})
	if err == nil || !strings.Contains(err.Error(), "TypeError") {
		t.Fatalf("expected a catchable TypeError, got %v", err)
	}

	// The unit (and worker) still works after the panic.
	value, err = call([]interface{}{reviews, int64(5)})
	if err != nil {
		t.Fatalf("call after panic: %v", err)
	}
	if value != "best=4.9 count=0 len=3 bonus=6" {
		t.Fatalf("value after panic = %#v", value)
	}
}
