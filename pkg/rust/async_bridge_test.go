package rust

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func buildAndLoad(t *testing.T, r *Runtime, source string, exports []string) *Unit {
	t.Helper()
	soPath, err := r.Toolchain().BuildUnit(source, exports)
	if err != nil {
		t.Fatalf("BuildUnit: %v", err)
	}
	unit, err := LoadUnit(soPath)
	if err != nil {
		t.Fatalf("LoadUnit: %v", err)
	}
	return unit
}

func futureHandle(t *testing.T, unit *Unit, symbol, args string) uint64 {
	t.Helper()
	raw, err := unit.Call(symbol, args)
	if err != nil {
		t.Fatalf("%s: %v", symbol, err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil || !env.OK || env.Boundary != "rust_future" {
		t.Fatalf("%s envelope: %+v (%v)", symbol, env, err)
	}
	n, err := strconv.ParseUint(env.HandleID, 10, 64)
	if err != nil {
		t.Fatalf("handle %q: %v", env.HandleID, err)
	}
	return n
}

// TestAsyncBridgeHop is the 2c end state: an outbound bridge call from async
// context suspends the future on a oneshot, the park exits, the host services
// the call as plain work (with NO active runtime context — the
// sequential-not-nested invariant), and the future resumes on re-park.
func TestAsyncBridgeHop(t *testing.T) {
	r := requireToolchain(t)
	source := `
async fn ask_python() -> String {
    let a = omnivm::call_async("python", "2 ** 100").await.unwrap();
    let b = omnivm::call_async("python", "1 + 1").await.unwrap();
    format!("{a}|{b}")
}

fn in_runtime_context() -> bool {
    omnivm::tokio::runtime::Handle::try_current().is_ok()
}

omnivm::export_async_fn!(OmniVMCall_ask_python, ask_python, 0);
omnivm::export_fn!(OmniVMCall_in_runtime_context, in_runtime_context, 0);
omnivm::unit_abi_marker!();
`
	unit := buildAndLoad(t, r, source, []string{"ask_python", "in_runtime_context"})
	handle := futureHandle(t, unit, "OmniVMCall_ask_python", "[]")

	var sawBridgeCalls []string
	bridge := func(rtName, code string) (string, error) {
		sawBridgeCalls = append(sawBridgeCalls, rtName+":"+code)
		// Required invariant: Handle::try_current().is_err() at every bridge
		// entry — the park exited before this call ran, so re-entering Rust
		// here is a sequential block_on, not a nested one.
		raw, err := unit.Call("OmniVMCall_in_runtime_context", "[]")
		if err != nil {
			return "", err
		}
		env, _ := decodeEnvelope(raw)
		if env.Value != false {
			t.Fatalf("bridge entry ran with active runtime context: %+v", env)
		}
		switch code {
		case "2 ** 100":
			return "1267650600228229401496703205376", nil
		case "1 + 1":
			return "2", nil
		}
		return "", fmt.Errorf("unexpected code %q", code)
	}

	envJSON, err := r.DriveFuture(handle, bridge)
	if err != nil {
		t.Fatalf("DriveFuture: %v", err)
	}
	env, _ := decodeEnvelope(string(envJSON))
	if env.Value != "1267650600228229401496703205376|2" {
		t.Fatalf("hop result = %#v", env.Value)
	}
	if len(sawBridgeCalls) != 2 {
		t.Fatalf("bridge calls = %v, want 2 hops", sawBridgeCalls)
	}
}

// TestMutualRecursionThroughBridge re-runs the deep mutual-recursion shape
// with Rust in the chain: a synchronous Rust fn bridges out, the host calls
// back into the same Rust unit, 18 levels deep (the guard path, 2a).
func TestMutualRecursionThroughBridge(t *testing.T) {
	r := requireToolchain(t)
	source := `
fn descend(n: i64) -> i64 {
    if n <= 0 {
        return 0;
    }
    // Hop out to the "other runtime"; the host bounces it back into this
    // unit at n-1, building alternating host/Rust frames on the OS stack.
    let below: i64 = omnivm::call("chain", &format!("{}", n - 1)).unwrap().parse().unwrap();
    below + 1
}
omnivm::export_fn!(OmniVMCall_descend, descend, 1);
omnivm::unit_abi_marker!();
`
	unit := buildAndLoad(t, r, source, []string{"descend"})
	var depth int
	r.BridgeFn = func(rtName, code string) string {
		depth++
		raw, err := unit.Call("OmniVMCall_descend", "["+code+"]")
		if err != nil {
			return "ERR:" + err.Error()
		}
		env, _ := decodeEnvelope(raw)
		if !env.OK {
			return "ERR:" + env.Error
		}
		return fmt.Sprintf("%v", env.Value)
	}
	r.installBridge()

	raw, err := unit.Call("OmniVMCall_descend", "[18]")
	if err != nil {
		t.Fatalf("descend: %v", err)
	}
	env, _ := decodeEnvelope(raw)
	if !env.OK {
		t.Fatalf("descend failed: %s", env.Error)
	}
	if got := fmt.Sprintf("%v", env.Value); got != "18" {
		t.Fatalf("descend(18) = %s, want 18", got)
	}
	if depth != 18 {
		t.Fatalf("bridge depth = %d, want 18", depth)
	}
}

// TestMutualRecursionAsyncHop is the same chain under the async hop (2c):
// every level suspends on the outbound queue, the park exits, and the host
// re-enters Rust with sequential block_on frames.
func TestMutualRecursionAsyncHop(t *testing.T) {
	r := requireToolchain(t)
	source := `
async fn descend_async(n: i64) -> i64 {
    if n <= 0 {
        return 0;
    }
    let below: i64 = omnivm::call_async("chain", &format!("{}", n - 1)).await.unwrap().parse().unwrap();
    below + 1
}
omnivm::export_async_fn!(OmniVMCall_descend_async, descend_async, 1);
omnivm::unit_abi_marker!();
`
	unit := buildAndLoad(t, r, source, []string{"descend_async"})

	var drive func(level string) (string, error)
	drive = func(level string) (string, error) {
		handle := futureHandle(t, unit, "OmniVMCall_descend_async", "["+level+"]")
		envJSON, err := r.DriveFuture(handle, func(rtName, code string) (string, error) {
			return drive(code)
		})
		if err != nil {
			return "", err
		}
		env, _ := decodeEnvelope(string(envJSON))
		if !env.OK {
			return "", fmt.Errorf("%s", env.Error)
		}
		return fmt.Sprintf("%v", env.Value), nil
	}

	got, err := drive("18")
	if err != nil {
		t.Fatalf("async chain: %v", err)
	}
	if got != "18" {
		t.Fatalf("descend_async(18) = %s, want 18", got)
	}
}

// TestEntangledFutureHeartbeat: a Rust future awaiting completion fed by
// another runtime only progresses because the heartbeat pump arm exits the
// park and lets the other reactor run (here, the pump hook plays the JS
// interval's role).
func TestEntangledFutureHeartbeat(t *testing.T) {
	r := requireToolchain(t)
	source := `
use std::sync::Mutex;

static SLOT: Mutex<Option<omnivm::tokio::sync::oneshot::Sender<i64>>> = Mutex::new(None);

async fn wait_for_feed() -> i64 {
    let (tx, rx) = omnivm::tokio::sync::oneshot::channel();
    *SLOT.lock().unwrap() = Some(tx);
    rx.await.unwrap_or(-1)
}

fn feed(value: i64) -> bool {
    match SLOT.lock().unwrap().take() {
        Some(tx) => tx.send(value).is_ok(),
        None => false,
    }
}

omnivm::export_async_fn!(OmniVMCall_wait_for_feed, wait_for_feed, 0);
omnivm::export_fn!(OmniVMCall_feed, feed, 1);
omnivm::unit_abi_marker!();
`
	unit := buildAndLoad(t, r, source, []string{"wait_for_feed", "feed"})
	handle := futureHandle(t, unit, "OmniVMCall_wait_for_feed", "[]")

	beats := 0
	r.PumpOthers = func() {
		beats++
		if beats == 3 {
			// The "JS interval" fires between parks.
			if _, err := unit.Call("OmniVMCall_feed", "[99]"); err != nil {
				t.Errorf("feed: %v", err)
			}
		}
	}
	defer func() { r.PumpOthers = nil }()

	envJSON, err := r.DriveFuture(handle, nil)
	if err != nil {
		t.Fatalf("DriveFuture: %v", err)
	}
	env, _ := decodeEnvelope(string(envJSON))
	if fmt.Sprintf("%v", env.Value) != "99" {
		t.Fatalf("entangled future = %#v, want 99 after %d beats", env.Value, beats)
	}
	if beats < 3 {
		t.Fatalf("future completed after %d beats; the heartbeat arm never pumped", beats)
	}
}

// TestStatefulObjectHandle: a stateful Rust object crosses as an owned_handle
// proxy; methods dispatch across calls through OmniVMHandleOp, and release is
// quiet and idempotent (scope cleanup + finalizer may both fire).
func TestStatefulObjectHandle(t *testing.T) {
	r := requireToolchain(t)
	source := `
use std::sync::atomic::{AtomicI64, Ordering};

struct Counter {
    hits: AtomicI64,
}

fn make_counter() -> omnivm::objects::ObjectHandleRef {
    omnivm::export_object(
        omnivm::ObjectExport::new("counter", Counter { hits: AtomicI64::new(0) })
            .method("hit", |c: &Counter, args: &[omnivm::Value]| {
                let by = args.first().and_then(|v| v.as_i64()).unwrap_or(1);
                Ok(omnivm::Value::from(c.hits.fetch_add(by, Ordering::SeqCst) + by))
            }),
    )
}
omnivm::export_fn!(OmniVMCall_make_counter, make_counter, 0);
omnivm::unit_abi_marker!();
`
	unit := buildAndLoad(t, r, source, []string{"make_counter"})
	raw, err := unit.Call("OmniVMCall_make_counter", "[]")
	if err != nil {
		t.Fatalf("make_counter: %v", err)
	}
	env, _ := decodeEnvelope(raw)
	if !env.OK || env.Boundary != "owned_handle" || env.HandleID == "" {
		t.Fatalf("expected owned_handle envelope, got %+v", env)
	}

	call := func(op string, extra map[string]interface{}) map[string]interface{} {
		payload := map[string]interface{}{"op": op, "handle_id": env.HandleID}
		for k, v := range extra {
			payload[k] = v
		}
		data, _ := json.Marshal(payload)
		out, callErr := unit.Call("OmniVMHandleOp", string(data))
		if callErr != nil {
			t.Fatalf("OmniVMHandleOp %s: %v", op, callErr)
		}
		var decoded map[string]interface{}
		json.Unmarshal([]byte(out), &decoded)
		return decoded
	}

	// The pool/client analogy: state persists across method calls.
	if out := call("call", map[string]interface{}{"key": "hit", "args": []interface{}{2}}); out["value"] != float64(2) {
		t.Fatalf("hit(2) = %v", out)
	}
	if out := call("call", map[string]interface{}{"key": "hit", "args": []interface{}{3}}); out["value"] != float64(5) {
		t.Fatalf("hit(3) after hit(2) = %v — state was not reused", out)
	}
	if out := call("callable", map[string]interface{}{"key": "hit"}); out["value"] != true {
		t.Fatalf("callable(hit) = %v", out)
	}

	// Release (drop path): quiet, idempotent.
	releaseOut, err := unit.Call("OmniVMReleaseObject", env.HandleID)
	if err != nil || !strings.Contains(releaseOut, `"ok":true`) {
		t.Fatalf("release: %v %s", err, releaseOut)
	}
	releaseOut, _ = unit.Call("OmniVMReleaseObject", env.HandleID)
	if !strings.Contains(releaseOut, `"ok":true`) {
		t.Fatalf("second release not quiet: %s", releaseOut)
	}
	if out := call("call", map[string]interface{}{"key": "hit", "args": []interface{}{1}}); out["ok"] != false {
		t.Fatalf("released handle still callable: %v", out)
	}

	var stats map[string]interface{}
	json.Unmarshal([]byte(r.Support().Stats()), &stats)
	if stats["live_objects"] != float64(0) {
		t.Fatalf("live_objects = %v after release", stats["live_objects"])
	}
}
