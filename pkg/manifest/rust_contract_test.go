package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/omnivm/omnivm/pkg"
)

// scriptedRuntime is a minimal pkg.Runtime standing in for a guest: Eval is
// handled by a closure (enough to play the peer side of callback and channel
// protocols without cgo).
type scriptedRuntime struct {
	name string
	eval func(code string) pkg.Result
}

func (s *scriptedRuntime) Name() string                                          { return s.name }
func (s *scriptedRuntime) Initialize() error                                     { return nil }
func (s *scriptedRuntime) Execute(code string) pkg.Result                        { return s.eval(code) }
func (s *scriptedRuntime) Eval(code string) pkg.Result                           { return s.eval(code) }
func (s *scriptedRuntime) SetBridgeCallback(callPtr, freePtr uintptr)            {}
func (s *scriptedRuntime) Pump()                                                 {}
func (s *scriptedRuntime) Shutdown() error                                       { return nil }
func (s *scriptedRuntime) ExecuteFile(string, []string, io.Reader) pkg.Result    { return pkg.Result{} }

// TestRustSpawnHandleConcurrency: `go expr` on Rust async fns returns a real
// spawn handle, and two spawned sleeps run concurrently on the one reactor —
// awaiting both takes about one sleep, not two.
func TestRustSpawnHandleConcurrency(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	source := `
async fn slow_tag(tag: String, ms: u64) -> String {
    omnivm::tokio::time::sleep(std::time::Duration::from_millis(ms)).await;
    tag
}
`
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{{
		OpType: "func_def", Name: "slow_tag", BodyRuntime: "rust", Async: true,
		Params:  []*Param{{Name: "tag"}, {Name: "ms"}},
		Exports: []string{"slow_tag"}, Source: source,
	}}}); err != nil {
		t.Fatalf("compile: %v", err)
	}
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "spawn", Code: `slow_tag("alpha", 150)`, Bind: "a"},
		{OpType: "spawn", Code: `slow_tag("beta", 150)`, Bind: "b"},
		{OpType: "await", From: &Op{OpType: "declare", Bind: "__ta", Value: &ValueExpr{Kind: "ref", Name: "a"}}, Bind: "ra"},
		{OpType: "await", From: &Op{OpType: "declare", Bind: "__tb", Value: &ValueExpr{Kind: "ref", Name: "b"}}, Bind: "rb"},
	}}
	start := time.Now()
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	elapsed := time.Since(start)
	ra, _ := e.getBinding("ra")
	rb, _ := e.getBinding("rb")
	if ra != "alpha" || rb != "beta" {
		t.Fatalf("spawn results = %v / %v", ra, rb)
	}
	if elapsed > 260*time.Millisecond {
		t.Fatalf("two 150ms spawns took %v — they did not run concurrently", elapsed)
	}
}

// TestRustSpawnBlockingSyncFn: sync Rust fns under `go expr` dispatch onto
// tokio's blocking pool and resolve through the same await path.
func TestRustSpawnBlockingSyncFn(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{
			OpType: "func_def", Name: "crunch", BodyRuntime: "rust",
			Params:  []*Param{{Name: "n"}},
			Exports: []string{"crunch"},
			Source:  `fn crunch(n: i64) -> i64 { (0..n).sum() }`,
		},
		{OpType: "spawn", Code: "crunch(100000)", Bind: "job"},
		{OpType: "await", From: &Op{OpType: "declare", Bind: "__tj", Value: &ValueExpr{Kind: "ref", Name: "job"}}, Bind: "total"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	total, _ := e.getBinding("total")
	if !numEquals(total, 4999950000) {
		t.Fatalf("total = %#v", total)
	}
}

// TestRustCallbackFromPeer: a peer callable crosses into Rust as
// omnivm::Callback (Box<dyn Fn> morally) and is invoked through the bridge —
// including from async context, where the call hops (park exits first).
func TestRustCallbackFromPeer(t *testing.T) {
	requireRust(t)
	var invocations []string
	js := &scriptedRuntime{name: "javascript", eval: func(code string) pkg.Result {
		if !strings.HasPrefix(code, "score(") {
			return pkg.Result{}
		}
		invocations = append(invocations, code)
		{
			var args []float64
			payload := code[strings.Index(code, "JSON.parse(")+12:]
			payload = payload[:strings.Index(payload, `")`)]
			if err := json.Unmarshal([]byte(strings.ReplaceAll(payload, `\"`, `"`)), &args); err != nil {
				return pkg.Result{Err: fmt.Errorf("bad callback payload: %v", err)}
			}
			return pkg.Result{Value: fmt.Sprintf("%g", args[0]*10)}
		}
	}}
	e := NewExecutor(map[string]pkg.Runtime{"javascript": js})
	e.setBinding("score", RuntimeRef{Runtime: "javascript", VarName: "score", Callable: true, CallableKnown: true})

	m := &Manifest{Version: 1, Ops: []*Op{
		{
			OpType: "func_def", Name: "apply_twice", BodyRuntime: "rust", Async: true,
			Params:  []*Param{{Name: "cb"}, {Name: "x"}},
			Exports: []string{"apply_twice"},
			Source: `
async fn apply_twice(cb: omnivm::Callback, x: f64) -> f64 {
    let once: f64 = cb.call_async(&[omnivm::serde_json::json!(x)]).await.unwrap().parse().unwrap();
    let twice: f64 = cb.call_async(&[omnivm::serde_json::json!(once)]).await.unwrap().parse().unwrap();
    twice
}
`,
		},
		{OpType: "eval", Runtime: "rust", Code: "apply_twice(score, 2.5)", Bind: "out"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	out, _ := e.getBinding("out")
	if !numEquals(out, 250) {
		t.Fatalf("apply_twice = %#v, want 250", out)
	}
	if len(invocations) != 2 {
		t.Fatalf("callback invoked %d times: %v", len(invocations), invocations)
	}
}

// TestRustChannelBridge: Rust receives from and sends into manifest channels
// through the universal stream-pull protocol and the manifest send builtin.
func TestRustChannelBridge(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "chan", Action: "make", Bind: "jobs", Size: 8},
		{OpType: "chan", Action: "make", Bind: "results", Size: 8},
		{OpType: "chan", Action: "send", Channel: "jobs", Value: &ValueExpr{Kind: "literal", Value: 21}},
		{OpType: "chan", Action: "send", Channel: "jobs", Value: &ValueExpr{Kind: "literal", Value: 4}},
		{
			OpType: "func_def", Name: "relay_double", BodyRuntime: "rust", Async: true,
			Params:  []*Param{{Name: "input"}, {Name: "output"}},
			Exports: []string{"relay_double"},
			Source: `
async fn relay_double(input: omnivm::Channel, output: omnivm::Channel) -> i64 {
    let mut moved = 0i64;
    while let Some(value) = input.recv_async().await.unwrap() {
        let n = value.as_i64().unwrap_or(0);
        output.send_async(n * 2).await.unwrap();
        moved += 1;
        if moved == 2 { break; }
    }
    moved
}
`,
		},
		{OpType: "eval", Runtime: "rust", Code: "relay_double(jobs, results)", Bind: "moved"},
		{OpType: "chan", Action: "recv", Channel: "results", Bind: "r1"},
		{OpType: "chan", Action: "recv", Channel: "results", Bind: "r2"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	moved, _ := e.getBinding("moved")
	r1, _ := e.getBinding("r1")
	r2, _ := e.getBinding("r2")
	if !numEquals(moved, 2) || !numEquals(r1, 42) || !numEquals(r2, 8) {
		t.Fatalf("relay results: moved=%v r1=%v r2=%v", moved, r1, r2)
	}
}

// TestRustStreamProducer: a Rust iterator exported as a stream proxy crosses
// as a manifest stream and is pulled through stream_next; cancel releases the
// underlying object.
func TestRustStreamProducer(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{
			OpType: "func_def", Name: "countdown", BodyRuntime: "rust",
			Params:  []*Param{{Name: "from"}},
			Exports: []string{"countdown"},
			Source: `
fn countdown(from: i64) -> omnivm::objects::ObjectHandleRef {
    omnivm::objects::export_stream((0..from).rev())
}
`,
		},
		{OpType: "eval", Runtime: "rust", Code: "countdown(3)", Bind: "ticker"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	binding, _ := e.getBinding("ticker")
	descriptor, ok := binding.(map[string]interface{})
	if !ok || descriptor["__omnivm_stream__"] != true {
		t.Fatalf("ticker = %#v, want stream descriptor", binding)
	}
	var id int64
	switch v := descriptor["id"].(type) {
	case int64:
		id = v
	case float64:
		id = int64(v)
	default:
		t.Fatalf("descriptor id = %#v", descriptor["id"])
	}

	pull := func() (interface{}, bool) {
		raw, err := e.HandleCall(fmt.Sprintf(`{"op":"stream_next","id":%d}`, id))
		if err != nil {
			t.Fatalf("stream_next: %v", err)
		}
		var out struct {
			Value struct {
				Done  bool        `json:"done"`
				Value interface{} `json:"value"`
			} `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode stream_next %q: %v", raw, err)
		}
		return out.Value.Value, out.Value.Done
	}

	var got []interface{}
	for {
		value, done := pull()
		if done {
			break
		}
		got = append(got, value)
	}
	if len(got) != 3 || !numEquals(got[0], 2) || !numEquals(got[2], 0) {
		t.Fatalf("stream values = %#v", got)
	}
}
