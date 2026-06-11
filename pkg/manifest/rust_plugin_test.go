package manifest

import (
	"runtime"
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/rust"
)

func numEquals(v interface{}, want float64) bool {
	switch n := v.(type) {
	case int:
		return float64(n) == want
	case int64:
		return float64(n) == want
	case float64:
		return n == want
	default:
		return false
	}
}

func requireRust(t *testing.T) {
	t.Helper()
	// Rust future/LocalSet state is golden-thread-affine; tests pin their
	// goroutine the way the dispatcher pins the golden thread.
	runtime.LockOSThread()
	if _, err := rust.GetToolchain(); err != nil {
		t.Skipf("rust toolchain unavailable: %v", err)
	}
}

func TestRustFuncDefEval(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{
			OpType:      "func_def",
			Name:        "add",
			BodyRuntime: "rust",
			Params:      []*Param{{Name: "a"}, {Name: "b"}},
			Exports:     []string{"add"},
			Source:      "fn add(a: i64, b: i64) -> i64 { a + b }",
		},
		{OpType: "eval", Runtime: "rust", Code: "add(40, 2)", Bind: "x"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	got, ok := e.getBinding("x")
	if !ok {
		t.Fatal("binding x missing")
	}
	if !numEquals(got, 42) {
		t.Fatalf("x = %#v, want 42", got)
	}
}

func TestRustFuncDefSharedUnitMultipleExports(t *testing.T) {
	requireRust(t)
	// Mirrors the PolyScript contract: every Rust func_def carries the SAME
	// full unit source + the full export list, so the host compiles exactly
	// one cdylib for the program.
	unit := `
use serde::{Serialize, Deserialize};

#[derive(Serialize, Deserialize)]
struct Item { name: String, score: f64 }

#[derive(Serialize)]
#[serde(tag = "type")]
enum Tag {
    Hot { score: f64 },
    Cold,
}

fn rate(items: Vec<Item>) -> Vec<Tag> {
    items.iter().map(|i| if i.score > 0.5 { Tag::Hot { score: i.score } } else { Tag::Cold }).collect()
}

fn count(items: Vec<Item>) -> i64 { items.len() as i64 }

omnivm::export_fn!(OmniVMCall_rate, rate, 1);
omnivm::export_fn!(OmniVMCall_count, count, 1);
`
	exports := []string{"rate", "count"}
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "func_def", Name: "rate", BodyRuntime: "rust", Params: []*Param{{Name: "items"}}, Exports: exports, Source: unit},
		{OpType: "func_def", Name: "count", BodyRuntime: "rust", Params: []*Param{{Name: "items"}}, Exports: exports, Source: unit},
		{OpType: "eval", Runtime: "rust", Code: `rate([{"name":"a","score":0.9},{"name":"b","score":0.1}])`, Bind: "tags"},
		{OpType: "eval", Runtime: "rust", Code: `count([{"name":"a","score":0.9}])`, Bind: "n"},
	}}
	e := NewExecutor(map[string]pkg.Runtime{})
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	tags, _ := e.getBinding("tags")
	list, ok := tags.([]interface{})
	if !ok || len(list) != 2 {
		t.Fatalf("tags = %#v", tags)
	}
	first, _ := list[0].(map[string]interface{})
	if first["type"] != "Hot" || !numEquals(first["score"], 0.9) {
		t.Fatalf("tagged enum projection broken: %#v", first)
	}
	second, _ := list[1].(map[string]interface{})
	if second["type"] != "Cold" {
		t.Fatalf("unit variant projection broken: %#v", second)
	}
	if n, _ := e.getBinding("n"); !numEquals(n, 1) {
		t.Fatalf("n = %#v", n)
	}
}

func TestRustAsyncFuncDriveThroughCall(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{
			OpType:      "func_def",
			Name:        "slow_id",
			BodyRuntime: "rust",
			Params:      []*Param{{Name: "x"}},
			Exports:     []string{"slow_id"},
			Async:       true,
			Source: `
async fn slow_id(x: i64) -> i64 {
    omnivm::tokio::time::sleep(std::time::Duration::from_millis(30)).await;
    x
}
`,
		},
		{OpType: "eval", Runtime: "rust", Code: "slow_id(7)", Bind: "v"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v, _ := e.getBinding("v"); !numEquals(v, 7) {
		t.Fatalf("v = %#v, want 7", v)
	}
}

func TestRustPanicSurfacesAsError(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{
			OpType:      "func_def",
			Name:        "boom",
			BodyRuntime: "rust",
			Exports:     []string{"boom"},
			Source:      `fn boom() -> i64 { panic!("rust kaboom") }`,
		},
		{OpType: "eval", Runtime: "rust", Code: "boom()", Bind: "v"},
	}}
	err := e.Execute(m)
	if err == nil {
		t.Fatal("expected panic to surface as a manifest error")
	}
	if !strings.Contains(err.Error(), "rust kaboom") {
		t.Fatalf("error %q does not carry the panic message", err)
	}
}

func TestRustResultErrEnvelope(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{
			OpType:      "func_def",
			Name:        "must_pos",
			BodyRuntime: "rust",
			Params:      []*Param{{Name: "x"}},
			Exports:     []string{"must_pos"},
			Source: `
fn must_pos(x: i64) -> Result<i64, String> {
    if x > 0 { Ok(x) } else { Err(format!("{x} is not positive")) }
}
`,
		},
		{OpType: "eval", Runtime: "rust", Code: "must_pos(-3)", Bind: "v"},
	}}
	err := e.Execute(m)
	if err == nil || !strings.Contains(err.Error(), "-3 is not positive") {
		t.Fatalf("Result::Err did not become a structured error: %v", err)
	}
}

func TestRustExecutorOwnerDispatchTarget(t *testing.T) {
	status, err := OwnerDispatchTargetStatus("rust")
	if err != nil {
		t.Fatalf("OwnerDispatchTargetStatus: %v", err)
	}
	if status["target"] != "rust_executor" {
		t.Fatalf("target = %v", status["target"])
	}
	if status["supported"] != false {
		t.Fatalf("default executor must be diagnostic-only, got %v", status["supported"])
	}
	old := rustExecutorIsMulti
	rustExecutorIsMulti = func() bool { return true }
	defer func() { rustExecutorIsMulti = old }()
	status, _ = OwnerDispatchTargetStatus("tokio")
	if status["supported"] != true {
		t.Fatalf("multi executor must report supported, got %v", status["supported"])
	}
}

// TestRustCompileErrorMapsToPolyLine: a func_def whose unit carries a source
// map and a deliberate type error must fail with the ORIGINAL .poly
// coordinates leading the message — never the raw generated lib.rs line the
// user has never seen.
func TestRustCompileErrorMapsToPolyLine(t *testing.T) {
	requireRust(t)
	// Mirrors a unit polyc assembles from a .poly where `use` sits on poly
	// line 6 and the fn on poly line 13. The bad assignment is unit line 5
	// (= entry 13 + offset 2) -> review.poly:15.
	source := strings.Join([]string{
		`use regex::Regex;`,
		``,
		`fn tokenize(text: String) -> Vec<String> {`,
		`    let re = Regex::new(r"\w+").unwrap();`,
		`    let n: i64 = "not a number";`,
		`    re.find_iter(&text).map(|m| m.as_str().to_string()).collect()`,
		`}`,
	}, "\n")
	m := &Manifest{Version: 1, Ops: []*Op{{
		OpType:      "func_def",
		Name:        "tokenize",
		BodyRuntime: "rust",
		Params:      []*Param{{Name: "text"}},
		Exports:     []string{"tokenize"},
		Source:      source,
		PolyFile:    "review.poly",
		SourceMap: []*rust.SourceMapEntry{
			{UnitLine: 1, PolyLine: 6, Lines: 1},
			{UnitLine: 3, PolyLine: 13, Lines: 5},
		},
	}}}
	e := NewExecutor(map[string]pkg.Runtime{})
	err := e.Execute(m)
	if err == nil {
		t.Fatal("expected a compile error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "review.poly:15") {
		t.Fatalf("error does not lead with mapped .poly coordinates:\n%s", msg)
	}
	if strings.Contains(msg, "src/lib.rs:5") {
		t.Fatalf("error still points at the raw lib.rs line:\n%s", msg)
	}
	if !strings.Contains(msg, "mismatched types") {
		t.Fatalf("rustc message lost in mapping:\n%s", msg)
	}
}
