package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/omnivm/omnivm/pkg/handles"
	"github.com/omnivm/omnivm/pkg/rust"

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
// The dispatch is structured (the call_ref op): the host builds ONE canonical
// invocation with native arg literals; the scripted runtime captures every
// eval string and fails the test if any json.loads/JSON.parse-wrapped
// synthesized source shows up (the old double-encode path).
func TestRustCallbackFromPeer(t *testing.T) {
	requireRust(t)
	var invocations []string
	vars := map[string]float64{}
	js := &scriptedRuntime{name: "javascript", eval: func(code string) pkg.Result {
		// The old double-encode path synthesized `score(...JSON.parse("..."))`
		// (stub registration legitimately contains JSON.parse, so match the
		// invocation shape, not the substring alone).
		if strings.Contains(code, "score(...JSON.parse(") || strings.Contains(code, "json.loads") {
			return pkg.Result{Err: fmt.Errorf("callback took the synthesized-source path: %.200q", code)}
		}
		// Canonical structured invocation: globalThis["<ref>"] = (globalThis["score"]).apply(undefined, [<args>]);
		const applyMarker = `(globalThis["score"]).apply(undefined, `
		if idx := strings.Index(code, applyMarker); idx >= 0 && strings.HasPrefix(code, `globalThis["`) {
			invocations = append(invocations, code)
			end := strings.Index(code, `"] = `)
			if end < 0 {
				return pkg.Result{Err: fmt.Errorf("unexpected invocation shape: %q", code)}
			}
			varName := code[len(`globalThis["`):end]
			argsJSON := code[idx+len(applyMarker) : strings.LastIndex(code, ")")]
			var args []float64
			if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
				return pkg.Result{Err: fmt.Errorf("bad structured args %q: %v", argsJSON, err)}
			}
			vars[varName] = args[0] * 10
			return pkg.Result{}
		}
		// Primitive snapshot of the invocation result.
		const snapMarker = `var __v = (globalThis["`
		if idx := strings.Index(code, snapMarker); idx >= 0 {
			rest := code[idx+len(snapMarker):]
			varName := rest[:strings.Index(rest, `"`)]
			if value, known := vars[varName]; known {
				return pkg.Result{Value: fmt.Sprintf(`{"primitive":true,"value":%g}`, value)}
			}
		}
		return pkg.Result{}
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
	if !strings.Contains(invocations[0], "[2.5]") {
		t.Fatalf("first invocation does not carry native arg literals: %q", invocations[0])
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

// TestRustSpawnedChannelRelay replicates the cursed-concurrency corpus shape:
// the Rust relay is SPAWNED first, the channel is fed afterwards, and the
// await resolves the relay — the spawned task must see the post-spawn sends.
func TestRustSpawnedChannelRelay(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "chan", Action: "make", Bind: "jobs", Size: 8},
		{OpType: "chan", Action: "make", Bind: "results", Size: 8},
		{
			OpType: "func_def", Name: "relay_triple", BodyRuntime: "rust", Async: true,
			Params:  []*Param{{Name: "input"}, {Name: "output"}},
			Exports: []string{"relay_triple"},
			Source: `
async fn relay_triple(input: omnivm::Channel, output: omnivm::Channel) -> i64 {
    let mut moved = 0i64;
    while let Some(value) = input.recv_async().await.unwrap() {
        let n = value.as_i64().unwrap_or(0);
        output.send_async(n * 3).await.unwrap();
        moved += 1;
        if moved == 4 { break; }
    }
    moved
}
`,
		},
		{OpType: "spawn", Code: "relay_triple(jobs, results)", Bind: "job"},
		{OpType: "chan", Action: "send", Channel: "jobs", Value: &ValueExpr{Kind: "literal", Value: 10}},
		{OpType: "chan", Action: "send", Channel: "jobs", Value: &ValueExpr{Kind: "literal", Value: 20}},
		{OpType: "chan", Action: "send", Channel: "jobs", Value: &ValueExpr{Kind: "literal", Value: 30}},
		{OpType: "chan", Action: "send", Channel: "jobs", Value: &ValueExpr{Kind: "literal", Value: 40}},
		{OpType: "await", From: &Op{OpType: "eval", Runtime: "go", Code: "job"}, Bind: "moved"},
		{OpType: "chan", Action: "recv", Channel: "results", Bind: "r1"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	moved, _ := e.getBinding("moved")
	r1, _ := e.getBinding("r1")
	if !numEquals(moved, 4) || !numEquals(r1, 30) {
		t.Fatalf("relay: moved=%v r1=%v, want 4/30", moved, r1)
	}
}

// TestRuntimeGlobalNotShadowedByStaleBinding pins the stale-binding fix: a
// binding created by a runtime's own eval has a live global there; later
// auto-injection must NOT re-inject the stale manifest snapshot over it
// (runtime code may have mutated the global since).
func TestRuntimeGlobalNotShadowedByStaleBinding(t *testing.T) {
	var executed []string
	py := &scriptedRuntime{name: "python", eval: func(code string) pkg.Result {
		executed = append(executed, code)
		return pkg.Result{Value: ""}
	}}
	e := NewExecutor(map[string]pkg.Runtime{"python": py})
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "eval", Runtime: "python", Code: `""`, Bind: "caught"},
		// Imagine runtime code mutated `caught` here (an except handler).
		{OpType: "exec", Runtime: "python", Code: `print(caught)`},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	sawPrint := false
	for _, code := range executed {
		if strings.Contains(code, "print(caught)") {
			sawPrint = true
		}
		// Capture-injection blobs are identifiable by the materializer
		// wrapper; none may reassign caught over the live runtime global.
		if strings.Contains(code, "materialize_capture") && strings.Contains(code, "caught") {
			t.Fatalf("stale binding re-injected over the runtime global:\n%.300s", code)
		}
	}
	if !sawPrint {
		t.Fatalf("user code never executed: %q", executed)
	}
}

// TestRustChannelBackpressure pushes 4 values through a size-1 channel with
// concurrent producer/consumer tasks. Without send-side backpressure the
// non-blocking send would silently drop values 2-4; with it, sum == 10.
func TestRustChannelBackpressure(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "chan", Action: "make", Bind: "pipe", Size: 1},
		{OpType: "func_def", Name: "producer", BodyRuntime: "rust", Async: true,
			Params: []*Param{{Name: "out"}}, Exports: []string{"producer"},
			Source: `
async fn producer(out: omnivm::Channel) -> i64 {
    for i in 1..=4i64 {
        out.send_async(i).await.unwrap();
    }
    4
}
`},
		{OpType: "func_def", Name: "consumer", BodyRuntime: "rust", Async: true,
			Params: []*Param{{Name: "input"}}, Exports: []string{"consumer"},
			Source: `
async fn consumer(input: omnivm::Channel) -> i64 {
    let mut sum = 0i64;
    for _ in 0..4 {
        sum += input.recv_async().await.unwrap().unwrap().as_i64().unwrap_or(0);
    }
    sum
}
`},
		{OpType: "spawn", Code: "producer(pipe)", Bind: "p"},
		{OpType: "spawn", Code: "consumer(pipe)", Bind: "c"},
		{OpType: "await", From: &Op{OpType: "eval", Runtime: "go", Code: "c"}, Bind: "sum"},
		{OpType: "await", From: &Op{OpType: "eval", Runtime: "go", Code: "p"}, Bind: "produced"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	sum, _ := e.getBinding("sum")
	if !numEquals(sum, 10) {
		t.Fatalf("backpressure: sum=%v, want 10 (values were dropped)", sum)
	}
}

// TestRustChannelBatchedRecv: recv_async pulls in batches (max_n), so
// draining 200 queued values costs a handful of stream_next services rather
// than one bridge hop per value. The Rust consumer verifies strict ordering
// (returns a negative sentinel on the first out-of-order value), and the
// host-side service counter proves the hop reduction.
func TestRustChannelBatchedRecv(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	ops := []*Op{{OpType: "chan", Action: "make", Bind: "feed", Size: 256}}
	for i := 1; i <= 200; i++ {
		ops = append(ops, &Op{OpType: "chan", Action: "send", Channel: "feed", Value: &ValueExpr{Kind: "literal", Value: i}})
	}
	ops = append(ops,
		&Op{OpType: "func_def", Name: "drain_sum", BodyRuntime: "rust", Async: true,
			Params: []*Param{{Name: "input"}}, Exports: []string{"drain_sum"},
			Source: `
async fn drain_sum(input: omnivm::Channel) -> i64 {
    let mut sum = 0i64;
    for expect in 1..=200i64 {
        let value = input.recv_async().await.unwrap().unwrap().as_i64().unwrap_or(-1);
        if value != expect {
            return -expect; // order/loss sentinel: which receive went wrong
        }
        sum += value;
    }
    sum
}
`},
		&Op{OpType: "eval", Runtime: "rust", Code: "drain_sum(feed)", Bind: "sum"},
	)
	before := e.streamNextServices.Load()
	if err := e.Execute(&Manifest{Version: 1, Ops: ops}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	sum, _ := e.getBinding("sum")
	if !numEquals(sum, 20100) {
		t.Fatalf("drain_sum = %v, want 20100 (values lost or reordered)", sum)
	}
	hops := e.streamNextServices.Load() - before
	if hops < 1 || hops >= 20 {
		t.Fatalf("stream_next serviced %d times for 200 values, want batched (<20)", hops)
	}
	t.Logf("stream_next services for 200 values: %d (unbatched would be 200)", hops)
}

// TestRustAsyncStreamExport pulls a timer-paced futures Stream through the
// universal stream protocol: each pull blocks on the runtime (timers
// progress) outside any drive.
func TestRustAsyncStreamExport(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "func_def", Name: "ticks", BodyRuntime: "rust",
			Exports: []string{"ticks"},
			Source: `
fn ticks() -> omnivm::objects::ObjectHandleRef {
    let stream = futures_stream_of_squares(3);
    omnivm::objects::export_async_stream(stream)
}

fn futures_stream_of_squares(n: i64) -> impl futures_core::Stream<Item = i64> + Send {
    let mut i = 0i64;
    futures_unfold(move || {
        i += 1;
        if i <= n { Some(i * i) } else { None }
    })
}

fn futures_unfold<F>(f: F) -> impl futures_core::Stream<Item = i64> + Send
where F: FnMut() -> Option<i64> + Send + Unpin + 'static {
    struct S<F>(F, bool);
    impl<F: FnMut() -> Option<i64> + Unpin> futures_core::Stream for S<F> {
        type Item = i64;
        fn poll_next(mut self: std::pin::Pin<&mut Self>, cx: &mut std::task::Context<'_>) -> std::task::Poll<Option<i64>> {
            // Alternate Pending/Ready to exercise the block_on wake path.
            if self.1 {
                self.1 = false;
                cx.waker().wake_by_ref();
                return std::task::Poll::Pending;
            }
            self.1 = true;
            std::task::Poll::Ready((self.0)())
        }
    }
    S(f, true)
}
`},
		{OpType: "eval", Runtime: "rust", Code: "ticks()", Bind: "s"},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	binding, _ := e.getBinding("s")
	descriptor, ok := binding.(map[string]interface{})
	if !ok || descriptor["__omnivm_stream__"] != true {
		t.Fatalf("s = %T %#v, want stream descriptor", binding, binding)
	}
	rawID, isInt := descriptor["id"].(int64)
	if !isInt {
		t.Fatalf("handle id type %T", descriptor["id"])
	}
	id := handles.ID(rawID)
	var got []interface{}
	for i := 0; i < 4; i++ {
		value, done, okPull, pullErr := e.handleStreamNext(id)
		if pullErr != nil || !okPull {
			t.Fatalf("pull %d: ok=%v err=%v", i, okPull, pullErr)
		}
		if done {
			break
		}
		got = append(got, value)
	}
	if len(got) != 3 || !numEquals(got[0], 1) || !numEquals(got[1], 4) || !numEquals(got[2], 9) {
		t.Fatalf("stream values = %v, want [1 4 9]", got)
	}
}

// TestRustTypedLane: scalar-shaped exports cross as omni_value_t (no JSON
// text); the typed entry's presence is the capability signal, and mixed
// shapes fall back to the envelope path.
func TestRustTypedLane(t *testing.T) {
	requireRust(t)
	e := NewExecutor(map[string]pkg.Runtime{})
	m := &Manifest{Version: 1, Ops: []*Op{
		{OpType: "func_def", Name: "typed_join", BodyRuntime: "rust",
			Params: []*Param{{Name: "label"}, {Name: "count"}}, Exports: []string{"typed_join"},
			Source: `
fn typed_join(label: String, count: i64) -> String {
    format!("{label}:{}", count * 2)
}
omnivm::export_fn!(OmniVMCall_typed_join, typed_join, 2);
omnivm::export_typed_fn!(OmniVMCallTyped_typed_join, typed_join, 2);
`},
	}}
	if err := e.Execute(m); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	before := rust.TypedCallCount
	value, err := e.goFuncs["typed_join"].(func([]interface{}) (interface{}, error))([]interface{}{"answer", int64(21)})
	if err != nil {
		t.Fatalf("typed call: %v", err)
	}
	if value != "answer:42" {
		t.Fatalf("typed call = %v, want answer:42", value)
	}
	if rust.TypedCallCount != before+1 {
		t.Fatalf("typed lane not taken (count %d -> %d)", before, rust.TypedCallCount)
	}
	// Non-scalar args fall back to the envelope path.
	value, err = e.goFuncs["typed_join"].(func([]interface{}) (interface{}, error))([]interface{}{"answer", map[string]interface{}{"n": 1}})
	if err == nil {
		t.Logf("fallback returned %v", value)
	}
	if rust.TypedCallCount != before+1 {
		t.Fatalf("fallback should not increment typed count")
	}
}
