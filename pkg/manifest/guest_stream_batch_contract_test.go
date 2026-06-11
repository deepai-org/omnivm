package manifest

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/omnivm/omnivm/pkg"
)

// This file is the contract suite for batched guest stream pulls: the python
// and JS stream proxies request {"op":"stream_next","max_n":64} and drain a
// client-side buffer instead of crossing the bridge once per value. The
// contract under test, in both directions:
//
//   - values and order are identical to the one-value-per-pull protocol,
//   - bridge hops drop ~64-fold (4 envelopes for a 200-value stream),
//   - "done" rides WITH the final values and means the host has already
//     released the handle: draining the buffered tail owes no stream_cancel,
//   - early exit mid-stream still wire-cancels exactly once and drops the
//     buffered-but-unconsumed tail (consumed values stay consumed),
//   - a pull error after a successful batch surfaces on the pull AFTER the
//     buffered values are consumed — the same consumption point as before,
//   - the proxies never send "pending": snapshot semantics, an open-but-empty
//     channel reads as done exactly as it did pre-batching.

// TestGuestStreamBatchHostServiceCount drives the host side with the EXACT
// payload shape the guest proxies now send and asserts the HandleCall service
// count for a 200-value channel-backed stream is ~64x below the value count.
func TestGuestStreamBatchHostServiceCount(t *testing.T) {
	e := NewExecutor(map[string]pkg.Runtime{})
	ch := &ChanRef{ch: make(chan interface{}, 200)}
	for i := 0; i < 200; i++ {
		ch.ch <- i
	}
	if err := ch.close(); err != nil {
		t.Fatalf("close channel: %v", err)
	}
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		t.Fatalf("register channel stream: %v", err)
	}

	before := e.streamNextServices.Load()
	var got []interface{}
	done := false
	for pulls := 0; pulls < 50 && !done; pulls++ {
		raw, err := e.HandleCall(fmt.Sprintf(`{"op":"stream_next","id":%d,"max_n":64}`, id))
		if err != nil {
			t.Fatalf("stream_next batch: %v", err)
		}
		var out struct {
			Value struct {
				Done   bool          `json:"done"`
				Values []interface{} `json:"values"`
			} `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode batch %q: %v", raw, err)
		}
		got = append(got, out.Value.Values...)
		done = out.Value.Done
	}
	if !done {
		t.Fatalf("stream never reported done")
	}
	if len(got) != 200 || !numEquals(got[0], 0) || !numEquals(got[199], 199) {
		t.Fatalf("batched stream: %d values (first/last %v/%v), want 200 (0..199)",
			len(got), got[0], got[len(got)-1])
	}
	for i, v := range got {
		if !numEquals(v, float64(i)) {
			t.Fatalf("value %d = %v, want %d (order must match unbatched protocol)", i, v, i)
		}
	}
	services := e.streamNextServices.Load() - before
	if services < 1 || services >= 20 {
		t.Fatalf("stream_next services for 200 values = %d, want batched (<20; unbatched would be 201)", services)
	}
	// done released the handle host-side: a follow-up pull must fail loudly.
	if _, err := e.handleEntry(id); err == nil {
		t.Fatalf("handle %d still registered after done", id)
	}
	t.Logf("stream_next services for 200 values: %d (unbatched would be 201)", services)
}

// TestGuestGeneratorBatchPullStaysLazy: a max_n pull against a
// guest-runtime-backed stream (a JS generator here) moves ONE value per
// envelope — still in the plural {"done","values"} shape — because generators
// run user code per value and eager pulls would make per-value side effects
// and finally-block timing observable (a consumer that breaks after 2 values
// must not have driven the generator 64 steps).
func TestGuestGeneratorBatchPullStaysLazy(t *testing.T) {
	const total = 5
	steps := 0
	cursor := 0
	current := 0
	done := false
	js := &scriptedRuntime{name: "javascript", eval: func(code string) pkg.Result {
		switch {
		case strings.Contains(code, "__omnivm_stream_obj"):
			// One generator step: produce the next value or report done.
			steps++
			if cursor < total {
				current = cursor
				done = false
				cursor++
			} else {
				done = true
			}
			return pkg.Result{}
		case strings.Contains(code, "primitive:true"):
			// Primitive snapshot of the stepped value.
			return pkg.Result{Value: fmt.Sprintf(`{"primitive":true,"value":%d}`, current)}
		case strings.Contains(code, "_done"):
			if done {
				return pkg.Result{Value: "true"}
			}
			return pkg.Result{Value: "false"}
		case strings.Contains(code, "_error"):
			return pkg.Result{}
		case strings.Contains(code, "_ready"):
			return pkg.Result{Value: "true"}
		default:
			return pkg.Result{}
		}
	}}
	e := NewExecutor(map[string]pkg.Runtime{"javascript": js})
	id, err := e.genericStreamHandle("javascript", RuntimeRef{Runtime: "javascript", VarName: "gen"})
	if err != nil {
		t.Fatalf("register generator stream: %v", err)
	}

	var got []interface{}
	finished := false
	for pulls := 0; pulls < 2*total+2 && !finished; pulls++ {
		raw, err := e.HandleCall(fmt.Sprintf(`{"op":"stream_next","id":%d,"max_n":64}`, id))
		if err != nil {
			t.Fatalf("stream_next batch: %v", err)
		}
		var out struct {
			Value struct {
				Done   bool          `json:"done"`
				Values []interface{} `json:"values"`
			} `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("decode batch %q: %v", raw, err)
		}
		if len(out.Value.Values) > 1 {
			t.Fatalf("guest generator pull moved %d values in one envelope, want at most 1 (laziness)", len(out.Value.Values))
		}
		if pulls == 0 && steps != 1 {
			t.Fatalf("first max_n=64 pull drove the generator %d steps, want exactly 1", steps)
		}
		got = append(got, out.Value.Values...)
		finished = out.Value.Done
	}
	if !finished {
		t.Fatalf("stream never reported done")
	}
	if len(got) != total {
		t.Fatalf("got %d values, want %d", len(got), total)
	}
	for i, v := range got {
		if !numEquals(v, float64(i)) {
			t.Fatalf("value %d = %v, want %d", i, v, i)
		}
	}
	if steps != total+1 {
		t.Fatalf("generator stepped %d times for %d values, want %d (one per value + done probe)", steps, total, total+1)
	}
}

// pythonBatchBridge is the scripted host shared by the python guest tests: a
// 200-value source served in the plural {"done","values"} shape, honoring
// max_n, with done riding with the final values.
const pythonBatchBridge = `
import json
class Bridge:
    requests = []
    values = list(range(200))
    cursor = 0
    @staticmethod
    def call(runtime, payload):
        if runtime != "__manifest":
            raise RuntimeError("unexpected runtime " + runtime)
        req = json.loads(payload)
        Bridge.requests.append(req)
        if req["op"] == "handle_retain":
            return json.dumps({"__omnivm_result__": True, "value": True})
        if req["op"] == "stream_next":
            max_n = int(req.get("max_n") or 1)
            chunk = Bridge.values[Bridge.cursor:Bridge.cursor + max_n]
            Bridge.cursor += len(chunk)
            done = Bridge.cursor >= len(Bridge.values)
            return json.dumps({"__omnivm_result__": True, "value": {"done": done, "values": chunk}})
        if req["op"] == "stream_cancel":
            return json.dumps({"__omnivm_result__": True, "value": True})
        raise RuntimeError("unexpected manifest op " + req["op"])
`

func runPythonGuestScript(t *testing.T, body string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	script := pythonBatchBridge + injectPythonCaptures(nil) + "\nomnivm = Bridge\n" + body
	out, err := exec.Command(python, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("python guest stream batch check failed: %v\n%s", err, out)
	}
}

// TestPythonGuestStreamBatchedPull: full drain of a 200-value stream yields
// identical values/order with 4 bridge hops instead of 201, and a stream that
// ended at done owes no stream_cancel.
func TestPythonGuestStreamBatchedPull(t *testing.T) {
	runPythonGuestScript(t, `
stream = __OmniVMStreamProxy({"__omnivm_stream__": True, "id": 77, "runtime": "python", "kind": "stream"})
seen = list(stream)
if seen != list(range(200)):
    raise RuntimeError("values/order mismatch: %d values, first/last %r/%r" % (len(seen), seen[:1], seen[-1:]))
nexts = [req for req in Bridge.requests if req["op"] == "stream_next"]
if len(nexts) != 4:
    raise RuntimeError("expected 4 batched pulls for 200 values, got %d" % len(nexts))
if any(req.get("max_n") != 64 for req in nexts):
    raise RuntimeError("stream_next pulls missing max_n=64: " + repr(nexts))
if any(req.get("pending") for req in nexts):
    raise RuntimeError("snapshot proxy must never send pending: " + repr(nexts))
if [req for req in Bridge.requests if req["op"] == "stream_cancel"]:
    raise RuntimeError("done-terminated stream sent stream_cancel")
if not stream._closed:
    raise RuntimeError("stream was not marked closed after done")
`)
}

// TestPythonGuestStreamEarlyExitCancels: breaking out after 3 values pulled
// from a 64-value buffer wire-cancels exactly once and drops the
// buffered-but-unconsumed tail (consumed values stay consumed).
func TestPythonGuestStreamEarlyExitCancels(t *testing.T) {
	runPythonGuestScript(t, `
stream = __OmniVMStreamProxy({"__omnivm_stream__": True, "id": 78, "runtime": "python", "kind": "stream"})
seen = []
for item in stream:
    seen.append(item)
    if len(seen) == 3:
        break
if seen != [0, 1, 2]:
    raise RuntimeError("early-exit values mismatch: " + repr(seen))
nexts = [req for req in Bridge.requests if req["op"] == "stream_next"]
if len(nexts) != 1:
    raise RuntimeError("expected a single batched pull before the break, got %d" % len(nexts))
cancels = [req for req in Bridge.requests if req["op"] == "stream_cancel"]
if len(cancels) != 1 or cancels[0].get("id") != 78:
    raise RuntimeError("stream cancel requests mismatch: " + repr(cancels))
if not stream._closed:
    raise RuntimeError("stream was not marked closed after early exit")
if len(stream._cache) != 3 or stream._remote_buffer:
    raise RuntimeError("buffered tail was not dropped on cancel")
if stream.close() is not False:
    raise RuntimeError("close was not idempotent")
`)
}

// TestPythonGuestStreamDoneTailCloseSkipsCancel: when done rode WITH the
// final values, the host has already released the handle — an explicit close
// mid-drain drops the tail without a stream_cancel wire call.
func TestPythonGuestStreamDoneTailCloseSkipsCancel(t *testing.T) {
	runPythonGuestScript(t, `
Bridge.values = list(range(10))
stream = __OmniVMStreamProxy({"__omnivm_stream__": True, "id": 79, "runtime": "python", "kind": "stream"})
seen = [next(stream), next(stream)]
if seen != [0, 1]:
    raise RuntimeError("buffered drain mismatch: " + repr(seen))
if stream.close() is not True:
    raise RuntimeError("close mid-drain should report released (host released at done)")
if [req for req in Bridge.requests if req["op"] == "stream_cancel"]:
    raise RuntimeError("close after done sent stream_cancel for a released handle")
try:
    next(stream)
except StopIteration:
    pass
else:
    raise RuntimeError("closed stream kept yielding buffered values")
`)
}

// TestPythonGuestStreamPullErrorAfterBatch: an owner error on the refill pull
// surfaces AFTER the buffered values are consumed — the same consumption
// point as the one-value-per-pull protocol — and still cancels once.
func TestPythonGuestStreamPullErrorAfterBatch(t *testing.T) {
	runPythonGuestScript(t, `
calls = {"n": 0}
orig_call = Bridge.call
def flaky_call(runtime, payload):
    req = json.loads(payload)
    if req["op"] == "stream_next":
        calls["n"] += 1
        if calls["n"] == 1:
            Bridge.requests.append(req)
            return json.dumps({"__omnivm_result__": True, "value": {"done": False, "values": [0, 1]}})
        Bridge.requests.append(req)
        raise RuntimeError("owner read failed")
    return orig_call(runtime, payload)
Bridge.call = flaky_call
stream = __OmniVMStreamProxy({"__omnivm_stream__": True, "id": 80, "runtime": "python", "kind": "stream"})
seen = []
try:
    for item in stream:
        seen.append(item)
except RuntimeError as exc:
    if "owner read failed" not in str(exc):
        raise
else:
    raise RuntimeError("owner pull error was not raised")
if seen != [0, 1]:
    raise RuntimeError("values before the error were not delivered: " + repr(seen))
cancels = [req for req in Bridge.requests if req["op"] == "stream_cancel"]
if len(cancels) != 1 or cancels[0].get("id") != 80:
    raise RuntimeError("stream cancel requests mismatch: " + repr(cancels))
if not stream._closed:
    raise RuntimeError("stream was not marked closed after pull error")
`)
}

// TestPythonGuestStreamSingularShapeFallback: the singular
// {"done":false,"value":...} envelope (pre-batch shape) still drives the
// proxy one value at a time.
func TestPythonGuestStreamSingularShapeFallback(t *testing.T) {
	runPythonGuestScript(t, `
def singular_call(runtime, payload):
    req = json.loads(payload)
    Bridge.requests.append(req)
    if req["op"] == "handle_retain":
        return json.dumps({"__omnivm_result__": True, "value": True})
    if req["op"] == "stream_next":
        if Bridge.cursor >= 3:
            return json.dumps({"__omnivm_result__": True, "value": {"done": True}})
        value = Bridge.values[Bridge.cursor]
        Bridge.cursor += 1
        return json.dumps({"__omnivm_result__": True, "value": {"done": False, "value": value}})
    raise RuntimeError("unexpected manifest op " + req["op"])
Bridge.call = singular_call
stream = __OmniVMStreamProxy({"__omnivm_stream__": True, "id": 81, "runtime": "python", "kind": "stream"})
seen = list(stream)
if seen != [0, 1, 2]:
    raise RuntimeError("singular-shape values mismatch: " + repr(seen))
nexts = [req for req in Bridge.requests if req["op"] == "stream_next"]
if len(nexts) != 4:
    raise RuntimeError("singular shape should pull per value (+done): " + repr(len(nexts)))
`)
}

// jsBatchBridge mirrors pythonBatchBridge for node guests.
const jsBatchBridge = `
var requests = [];
var registered = 0;
var unregistered = 0;
globalThis.__omnivm_handle_finalizers = {
  register: function(target, id, token) { registered++; },
  unregister: function(token) { unregistered++; }
};
var sourceValues = [];
for (var i = 0; i < 200; i++) sourceValues.push(i);
var sourceCursor = 0;
globalThis.omnivm = {
  call: function(runtime, payloadRaw) {
    if (runtime !== "__manifest") throw new Error("unexpected runtime " + runtime);
    var payload = JSON.parse(payloadRaw);
    requests.push(payload);
    if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
    if (payload.op === "stream_next") {
      var maxN = payload.max_n || 1;
      var chunk = sourceValues.slice(sourceCursor, sourceCursor + maxN);
      sourceCursor += chunk.length;
      var done = sourceCursor >= sourceValues.length;
      return JSON.stringify({__omnivm_result__: true, value: {done: done, values: chunk}});
    }
    if (payload.op === "stream_cancel") return JSON.stringify({__omnivm_result__: true, value: true});
    throw new Error("unexpected manifest op " + payload.op);
  }
};
`

func runJSGuestScript(t *testing.T, body string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	script := jsBatchBridge + injectJSCaptures(nil) + "\n" + body
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node guest stream batch check failed: %v\n%s", err, out)
	}
}

// TestJSGuestStreamBatchedPull: full drain of a 200-value stream yields
// identical values/order with 4 bridge hops instead of 201, and a stream that
// ended at done owes no stream_cancel.
func TestJSGuestStreamBatchedPull(t *testing.T) {
	runJSGuestScript(t, `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 77, runtime: "javascript", kind: "stream"});
var seen = stream.toArray();
if (seen.length !== 200) throw new Error("value count mismatch: " + seen.length);
for (var i = 0; i < 200; i++) {
  if (seen[i] !== i) throw new Error("order mismatch at " + i + ": " + seen[i]);
}
var nexts = requests.filter(function(req) { return req.op === "stream_next"; });
if (nexts.length !== 4) throw new Error("expected 4 batched pulls for 200 values, got " + nexts.length);
if (nexts.some(function(req) { return req.max_n !== 64; })) throw new Error("stream_next pulls missing max_n=64: " + JSON.stringify(nexts));
if (nexts.some(function(req) { return req.pending; })) throw new Error("snapshot proxy must never send pending");
if (requests.some(function(req) { return req.op === "stream_cancel"; })) throw new Error("done-terminated stream sent stream_cancel");
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed after done");
if (registered !== 1 || unregistered !== 1) throw new Error("finalizer register/unregister mismatch: " + registered + "/" + unregistered);
`)
}

// TestJSGuestStreamEarlyExitCancels: breaking out after 3 values pulled from
// a 64-value buffer wire-cancels exactly once and drops the buffered tail.
func TestJSGuestStreamEarlyExitCancels(t *testing.T) {
	runJSGuestScript(t, `
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 78, runtime: "javascript", kind: "stream"});
var seen = [];
for (var item of stream) {
  seen.push(item);
  if (seen.length === 3) break;
}
if (seen.join(",") !== "0,1,2") throw new Error("early-exit values mismatch: " + JSON.stringify(seen));
var nexts = requests.filter(function(req) { return req.op === "stream_next"; });
if (nexts.length !== 1) throw new Error("expected a single batched pull before the break, got " + nexts.length);
var cancels = requests.filter(function(req) { return req.op === "stream_cancel"; });
if (cancels.length !== 1 || cancels[0].id !== 78) throw new Error("stream cancel requests mismatch: " + JSON.stringify(cancels));
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed after early break");
if (registered !== 1 || unregistered !== 1) throw new Error("finalizer register/unregister mismatch: " + registered + "/" + unregistered);
if (stream.cancel() !== false) throw new Error("closed remote stream cancel should be idempotent false");
`)
}

// TestJSGuestStreamDoneTailCloseSkipsCancel: when done rode WITH the final
// values the host already released the handle — cancel mid-drain drops the
// tail without a stream_cancel wire call and still reports released.
func TestJSGuestStreamDoneTailCloseSkipsCancel(t *testing.T) {
	runJSGuestScript(t, `
sourceValues = sourceValues.slice(0, 10);
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 79, runtime: "javascript", kind: "stream"});
var it = stream[Symbol.iterator]();
var seen = [it.next().value, it.next().value];
if (seen.join(",") !== "0,1") throw new Error("buffered drain mismatch: " + JSON.stringify(seen));
if (stream.cancel() !== true) throw new Error("cancel mid-drain should report released (host released at done)");
if (requests.some(function(req) { return req.op === "stream_cancel"; })) throw new Error("cancel after done sent stream_cancel for a released handle");
var item = it.next();
if (!item || item.done !== true) throw new Error("closed stream kept yielding buffered values: " + JSON.stringify(item));
if (registered !== 1 || unregistered !== 1) throw new Error("finalizer register/unregister mismatch: " + registered + "/" + unregistered);
`)
}

// TestJSGuestStreamPullErrorAfterBatch: an owner error on the refill pull
// surfaces AFTER the buffered values are consumed and still cancels once.
func TestJSGuestStreamPullErrorAfterBatch(t *testing.T) {
	runJSGuestScript(t, `
var streamNextCalls = 0;
var baseCall = globalThis.omnivm.call;
globalThis.omnivm.call = function(runtime, payloadRaw) {
  var payload = JSON.parse(payloadRaw);
  if (payload.op === "stream_next") {
    streamNextCalls++;
    requests.push(payload);
    if (streamNextCalls === 1) {
      return JSON.stringify({__omnivm_result__: true, value: {done: false, values: [0, 1]}});
    }
    throw new Error("owner read failed");
  }
  return baseCall(runtime, payloadRaw);
};
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 80, runtime: "javascript", kind: "stream"});
var seen = [];
var raised = null;
try {
  for (var item of stream) seen.push(item);
} catch (err) {
  raised = err;
}
if (!raised || String(raised.message).indexOf("owner read failed") === -1) throw new Error("owner pull error was not raised: " + raised);
if (seen.join(",") !== "0,1") throw new Error("values before the error were not delivered: " + JSON.stringify(seen));
var cancels = requests.filter(function(req) { return req.op === "stream_cancel"; });
if (cancels.length !== 1 || cancels[0].id !== 80) throw new Error("stream cancel requests mismatch: " + JSON.stringify(cancels));
if (stream.__omnivm_closed__ !== true) throw new Error("stream was not marked closed after pull error");
`)
}

// TestJSGuestStreamSingularShapeFallback: the singular
// {"done":false,"value":...} envelope (pre-batch shape) still drives the
// proxy one value at a time.
func TestJSGuestStreamSingularShapeFallback(t *testing.T) {
	runJSGuestScript(t, `
globalThis.omnivm.call = function(runtime, payloadRaw) {
  var payload = JSON.parse(payloadRaw);
  requests.push(payload);
  if (payload.op === "handle_retain") return JSON.stringify({__omnivm_result__: true, value: true});
  if (payload.op === "stream_next") {
    if (sourceCursor >= 3) return JSON.stringify({__omnivm_result__: true, value: {done: true}});
    var value = sourceValues[sourceCursor++];
    return JSON.stringify({__omnivm_result__: true, value: {done: false, value: value}});
  }
  throw new Error("unexpected manifest op " + payload.op);
};
var stream = globalThis.__omnivm_make_stream_proxy({__omnivm_stream__: true, id: 81, runtime: "javascript", kind: "stream"});
var seen = stream.toArray();
if (seen.join(",") !== "0,1,2") throw new Error("singular-shape values mismatch: " + JSON.stringify(seen));
var nexts = requests.filter(function(req) { return req.op === "stream_next"; });
if (nexts.length !== 4) throw new Error("singular shape should pull per value (+done): " + nexts.length);
`)
}
