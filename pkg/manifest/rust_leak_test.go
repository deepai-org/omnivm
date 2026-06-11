package manifest

import (
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/handles"
	"github.com/omnivm/omnivm/pkg/rust"
)

// rustLeakIterations amplifies per-iteration leaks: a single-handle leak in
// any lane shows up as a +N delta against the post-warmup baseline.
const rustLeakIterations = 50

// rustLeakUnit is one compilation unit covering every pointer-carrying lane
// the in-process gate can exercise without an embedded Python:
//   - object handles (export_stream) full-drain AND partial-drain+cancel
//   - byte buffers (omnivm::Bytes pointer lane + OmniVMReleaseBuffer)
//   - channels (stream_next pull protocol + send builtin)
//   - bridge hops (omnivm::Callback through the executor router)
//   - stored futures (eval drive + spawn/await background tasks)
//   - panic storm (catch_unwind envelopes must not strand anything)
const rustLeakUnit = `
fn lk_add(a: i64, b: i64) -> i64 { a + b }

fn lk_countdown(from: i64) -> omnivm::objects::ObjectHandleRef {
    omnivm::objects::export_stream((0..from).rev())
}

fn lk_blob(n: i64) -> omnivm::Bytes {
    omnivm::Bytes((0..n).map(|i| (i % 251) as u8).collect())
}

fn lk_boom(n: i64) -> i64 { panic!("leak-gate kaboom {n}") }

async fn lk_relay(input: omnivm::Channel, output: omnivm::Channel) -> i64 {
    let mut moved = 0i64;
    while let Some(value) = input.recv_async().await.unwrap() {
        let n = value.as_i64().unwrap_or(0);
        output.send_async(n + 1).await.unwrap();
        moved += 1;
    }
    moved
}

async fn lk_apply(cb: omnivm::Callback, x: f64) -> f64 {
    let once: f64 = cb.call_async(&[omnivm::serde_json::json!(x)]).await.unwrap().parse().unwrap();
    once
}

async fn lk_slow(x: i64) -> i64 {
    omnivm::tokio::time::sleep(std::time::Duration::from_millis(1)).await;
    x
}

omnivm::export_fn!(OmniVMCall_lk_add, lk_add, 2);
omnivm::export_typed_fn!(OmniVMCallTyped_lk_add, lk_add, 2);
omnivm::export_fn!(OmniVMCall_lk_countdown, lk_countdown, 1);
omnivm::export_fn!(OmniVMCall_lk_blob, lk_blob, 1);
omnivm::export_fn!(OmniVMCall_lk_boom, lk_boom, 1);
omnivm::export_async_fn!(OmniVMCall_lk_relay, lk_relay, 2);
omnivm::export_async_fn!(OmniVMCall_lk_apply, lk_apply, 2);
omnivm::export_async_fn!(OmniVMCall_lk_slow, lk_slow, 1);
`

// rustLeakCounters are the crate-side liveness counters that MUST read zero
// when every lane has drained. log_records_forwarded and the executor/runtime
// info fields are intentionally excluded (monotonic / static by design).
var rustLeakCounters = []string{
	"live_objects",
	"live_cdata_shells",
	"live_byte_buffers",
	"pending_futures",
	"pending_bridge_calls",
}

func rustLeakStats(t *testing.T, rt *rust.Runtime) map[string]float64 {
	t.Helper()
	support := rt.Support()
	if support == nil {
		t.Fatal("rust support dylib not initialized")
	}
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(support.Stats()), &raw); err != nil {
		t.Fatalf("decode rust stats: %v", err)
	}
	out := make(map[string]float64, len(raw))
	for k, v := range raw {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	return out
}

// rustLeakSettle gives deferred host-side releases (finalizer queue) a chance
// to drain before a snapshot, so the gate measures real leaks, not timing.
func rustLeakSettle(e *Executor) int {
	goruntime.GC()
	table := e.ensureHandleTable()
	_ = table.DrainFinalizerReleases(0)
	return table.Stats(time.Now()).Live
}

// rustLeakStreamID extracts the handle table id from a stream descriptor.
func rustLeakStreamID(t *testing.T, binding interface{}) handles.ID {
	t.Helper()
	descriptor, ok := binding.(map[string]interface{})
	if !ok || descriptor["__omnivm_stream__"] != true {
		t.Fatalf("binding is not a stream descriptor: %#v", binding)
	}
	switch v := descriptor["id"].(type) {
	case int64:
		return handles.ID(v)
	case float64:
		return handles.ID(int64(v))
	default:
		t.Fatalf("stream descriptor id has type %T", descriptor["id"])
		return 0
	}
}

// rustLeakCallbackRuntime plays the JS peer for the Callback lane: it accepts
// the canonical structured invocation for score10 (multiplies by 10) and
// serves the primitive snapshot of the result.
func rustLeakCallbackRuntime() *scriptedRuntime {
	vars := map[string]float64{}
	return &scriptedRuntime{name: "javascript", eval: func(code string) pkg.Result {
		const applyMarker = `(globalThis["score10"]).apply(undefined, `
		if idx := strings.Index(code, applyMarker); idx >= 0 && strings.HasPrefix(code, `globalThis["`) {
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
}

// rustLeakIteration drives every lane once on the shared executor and drains
// everything it opened. Any handle/object/buffer/future left behind by one
// pass shows up as a per-iteration delta in TestRustLeakGate.
func rustLeakIteration(t *testing.T, e *Executor, i int) {
	t.Helper()

	// Lane 1: typed scalar calls (omni_value_t crossing — nothing may register).
	addFn, ok := e.goFuncs["lk_add"].(func([]interface{}) (interface{}, error))
	if !ok {
		t.Fatalf("lk_add not registered as a callable (got %T)", e.goFuncs["lk_add"])
	}
	value, err := addFn([]interface{}{int64(40), int64(2)})
	if err != nil {
		t.Fatalf("iter %d: typed lk_add: %v", i, err)
	}
	if !numEquals(value, 42) {
		t.Fatalf("iter %d: lk_add = %#v, want 42", i, value)
	}

	// Lane 2: object handle stream, fully drained (done pull releases the
	// rust object AND the host handle table entry).
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "eval", Runtime: "rust", Code: "lk_countdown(4)", Bind: "lk_t"},
	}}); err != nil {
		t.Fatalf("iter %d: countdown: %v", i, err)
	}
	full, _ := e.getBinding("lk_t")
	fullID := rustLeakStreamID(t, full)
	pulls := 0
	for {
		_, done, ok, err := e.handleStreamNext(fullID)
		if err != nil || !ok {
			t.Fatalf("iter %d: stream pull %d: ok=%v err=%v", i, pulls, ok, err)
		}
		if done {
			break
		}
		if pulls++; pulls > 8 {
			t.Fatalf("iter %d: countdown stream never finished", i)
		}
	}

	// Lane 3: object handle stream, partially drained then cancelled (the
	// cancel path must release just as completely as exhaustion).
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "eval", Runtime: "rust", Code: "lk_countdown(6)", Bind: "lk_p"},
	}}); err != nil {
		t.Fatalf("iter %d: countdown(partial): %v", i, err)
	}
	partial, _ := e.getBinding("lk_p")
	partialID := rustLeakStreamID(t, partial)
	for n := 0; n < 2; n++ {
		if _, done, ok, err := e.handleStreamNext(partialID); err != nil || !ok || done {
			t.Fatalf("iter %d: partial pull %d: done=%v ok=%v err=%v", i, n, done, ok, err)
		}
	}
	if err := e.handleStreamCancel(partialID); err != nil {
		t.Fatalf("iter %d: stream cancel: %v", i, err)
	}

	// Lane 4: byte buffer pointer lane. The executor path requires a Python
	// adopter, so this mirrors the host adoption contract directly: call the
	// export, read the pointer marker, release via OmniVMReleaseBuffer.
	meta := e.rustFuncs["lk_blob"]
	if meta == nil {
		t.Fatal("lk_blob has no rust func meta")
	}
	raw, err := meta.unit.Call("OmniVMCall_lk_blob", "[2048]")
	if err != nil {
		t.Fatalf("iter %d: lk_blob: %v", i, err)
	}
	var blobEnv struct {
		OK    bool                   `json:"ok"`
		Value map[string]interface{} `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &blobEnv); err != nil || !blobEnv.OK {
		t.Fatalf("iter %d: lk_blob envelope %q: err=%v", i, raw, err)
	}
	if blobEnv.Value["__omnivm_bytes__"] != true {
		t.Fatalf("iter %d: lk_blob did not return a bytes marker: %v", i, blobEnv.Value)
	}
	bufferID, _ := blobEnv.Value["buffer_id"].(string)
	if bufferID == "" {
		t.Fatalf("iter %d: bytes marker took the b64 path (pointer lane not armed): %v", i, blobEnv.Value)
	}
	if _, err := meta.unit.Call("OmniVMReleaseBuffer", bufferID); err != nil {
		t.Fatalf("iter %d: release buffer %s: %v", i, bufferID, err)
	}

	// Lane 5: channel relay. Closing jobs lets the relay drain to None (the
	// done pull releases the input handle); the output handle is drained by
	// the host afterwards, exactly like a guest consumer would.
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "chan", Action: "make", Bind: "lk_jobs", Size: 8},
		{OpType: "chan", Action: "make", Bind: "lk_results", Size: 8},
		{OpType: "chan", Action: "send", Channel: "lk_jobs", Value: &ValueExpr{Kind: "literal", Value: 1}},
		{OpType: "chan", Action: "send", Channel: "lk_jobs", Value: &ValueExpr{Kind: "literal", Value: 2}},
		{OpType: "chan", Action: "send", Channel: "lk_jobs", Value: &ValueExpr{Kind: "literal", Value: 3}},
		{OpType: "chan", Action: "close", Channel: "lk_jobs"},
		{OpType: "eval", Runtime: "rust", Code: "lk_relay(lk_jobs, lk_results)", Bind: "lk_moved"},
		{OpType: "chan", Action: "close", Channel: "lk_results"},
	}}); err != nil {
		t.Fatalf("iter %d: relay: %v", i, err)
	}
	if moved, _ := e.getBinding("lk_moved"); !numEquals(moved, 3) {
		t.Fatalf("iter %d: relay moved = %#v, want 3", i, moved)
	}
	resultsVal, _ := e.getBinding("lk_results")
	resultsChan, isChan := resultsVal.(*ChanRef)
	if !isChan {
		t.Fatalf("iter %d: lk_results is %T", i, resultsVal)
	}
	if outID, registered := e.bridgeHandleForValue("go", resultsChan); registered {
		got := 0
		for {
			_, done, ok, err := e.handleStreamNext(outID)
			if err != nil || !ok {
				t.Fatalf("iter %d: results drain: ok=%v err=%v", i, ok, err)
			}
			if done {
				break
			}
			if got++; got > 8 {
				t.Fatalf("iter %d: results channel never drained", i)
			}
		}
		if got != 3 {
			t.Fatalf("iter %d: results carried %d values, want 3", i, got)
		}
	} else {
		t.Fatalf("iter %d: results channel never crossed into rust", i)
	}

	// Lane 6: bridge hop via Callback (async context: park exits, the call
	// runs between parks, the pending-bridge entry must drain).
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "eval", Runtime: "rust", Code: "lk_apply(score10, 2.5)", Bind: "lk_cb"},
	}}); err != nil {
		t.Fatalf("iter %d: callback: %v", i, err)
	}
	if cb, _ := e.getBinding("lk_cb"); !numEquals(cb, 25) {
		t.Fatalf("iter %d: callback result = %#v, want 25", i, cb)
	}

	// Lane 7: spawn handle (backgrounded stored future) resolved by await.
	if err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
		{OpType: "spawn", Code: "lk_slow(7)", Bind: "lk_job"},
		{OpType: "await", From: &Op{OpType: "declare", Bind: "__lkj", Value: &ValueExpr{Kind: "ref", Name: "lk_job"}}, Bind: "lk_done"},
	}}); err != nil {
		t.Fatalf("iter %d: spawn/await: %v", i, err)
	}
	if done, _ := e.getBinding("lk_done"); !numEquals(done, 7) {
		t.Fatalf("iter %d: spawn result = %#v, want 7", i, done)
	}

	// Lane 8: panic storm — every panic must come back as a structured error
	// with nothing stranded in the future/object tables.
	for n := 0; n < 4; n++ {
		err := e.Execute(&Manifest{Version: 1, Ops: []*Op{
			{OpType: "eval", Runtime: "rust", Code: fmt.Sprintf("lk_boom(%d)", n), Bind: "lk_x"},
		}})
		if err == nil || !strings.Contains(err.Error(), "leak-gate kaboom") {
			t.Fatalf("iter %d: panic %d did not surface as an error: %v", i, n, err)
		}
	}
}

// TestRustLeakGate is the in-process soak gate: ONE executor exercises every
// pointer-carrying lane rustLeakIterations times and the crate liveness
// counters plus the host handle table must return exactly to the post-warmup
// baseline (which itself must be fully drained — all gated counters at 0).
//
// Documented baselines (post-warmup, all by construction):
//   - live_objects = 0            (streams fully drained or cancelled)
//   - live_cdata_shells = 0       (no C-Data in this gate; covered by
//     scripts/test-rust-leaks.sh with a live Python)
//   - live_byte_buffers = 0       (every Bytes return released by buffer_id)
//   - pending_futures = 0         (eval drives + spawn/await both resolve)
//   - pending_bridge_calls = 0    (callback hops complete between parks)
//   - host handle table Live = 0  (stream/channel handles release on
//     exhaustion or cancel; bindings hold descriptors, not entries)
func TestRustLeakGate(t *testing.T) {
	requireRust(t)
	// Arm the crate's pointer return lane (Bytes crosses as ptr+buffer_id);
	// cgo propagates this into the dylib's environment.
	t.Setenv("OMNIVM_ARROW_CDATA_RETURN", "1")

	e := NewExecutor(map[string]pkg.Runtime{"javascript": rustLeakCallbackRuntime()})
	e.setBinding("score10", RuntimeRef{Runtime: "javascript", VarName: "score10", Callable: true, CallableKnown: true})

	exports := []string{"lk_add", "lk_countdown", "lk_blob", "lk_boom", "lk_relay", "lk_apply", "lk_slow"}
	var defs []*Op
	for _, def := range []struct {
		name  string
		arity int
		async bool
	}{
		{"lk_add", 2, false},
		{"lk_countdown", 1, false},
		{"lk_blob", 1, false},
		{"lk_boom", 1, false},
		{"lk_relay", 2, true},
		{"lk_apply", 2, true},
		{"lk_slow", 1, true},
	} {
		params := make([]*Param, def.arity)
		for p := range params {
			params[p] = &Param{Name: fmt.Sprintf("p%d", p)}
		}
		defs = append(defs, &Op{
			OpType:      "func_def",
			Name:        def.name,
			BodyRuntime: "rust",
			Async:       def.async,
			Params:      params,
			Exports:     exports,
			Source:      rustLeakUnit,
		})
	}
	if err := e.Execute(&Manifest{Version: 1, Ops: defs}); err != nil {
		t.Fatalf("compile leak unit: %v", err)
	}

	rustRT, isRust := e.runtimes["rust"].(*rust.Runtime)
	if !isRust {
		t.Fatalf("rust runtime not registered after compile (got %T)", e.runtimes["rust"])
	}

	// Warmup: one full pass arms lazy machinery (tokio runtime, stub
	// registration, callback router) so the baseline is steady-state.
	rustLeakIteration(t, e, -1)

	baselineLive := rustLeakSettle(e)
	baseline := rustLeakStats(t, rustRT)
	for _, key := range rustLeakCounters {
		if baseline[key] != 0 {
			t.Fatalf("post-warmup baseline not drained: %s = %v (full stats: %v)", key, baseline[key], baseline)
		}
	}
	if baselineLive != 0 {
		stats := e.ensureHandleTable().Stats(time.Now())
		t.Fatalf("post-warmup host handle table not drained: live=%d by_runtime=%v oldest=%s",
			baselineLive, stats.LiveByRuntime, stats.OldestLiveHandleKind)
	}

	for i := 0; i < rustLeakIterations; i++ {
		rustLeakIteration(t, e, i)
	}

	finalLive := rustLeakSettle(e)
	final := rustLeakStats(t, rustRT)
	for _, key := range rustLeakCounters {
		if final[key] != baseline[key] {
			t.Errorf("LEAK: %s = %v after %d iterations (baseline %v, delta %+v)",
				key, final[key], rustLeakIterations, baseline[key], final[key]-baseline[key])
		}
	}
	if finalLive != baselineLive {
		stats := e.ensureHandleTable().Stats(time.Now())
		t.Errorf("LEAK: host handle table live = %d after %d iterations (baseline %d) by_runtime=%v oldest=%s",
			finalLive, rustLeakIterations, baselineLive, stats.LiveByRuntime, stats.OldestLiveHandleKind)
	}
	t.Logf("leak gate: %d iterations, crate counters %v, host live handles %d",
		rustLeakIterations, final, finalLive)
}
