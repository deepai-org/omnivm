package rust

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// requireToolchain skips when no Rust toolchain is available (tests run in
// the Docker image, where it always is). It also pins the test goroutine to
// its OS thread: the future table and LocalSet live on the golden thread,
// and in production every drive/call happens on the locked dispatcher
// thread — tests must honor the same affinity.
func requireToolchain(t *testing.T) *Runtime {
	t.Helper()
	runtime.LockOSThread()
	if _, err := GetToolchain(); err != nil {
		t.Skipf("rust toolchain unavailable: %v", err)
	}
	r := New()
	if err := r.Initialize(); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	return r
}

func TestExecuteSnippet(t *testing.T) {
	r := requireToolchain(t)
	result := r.Execute(`println!("hello from rust: {}", 6 * 7);`)
	if result.Err != nil {
		t.Fatalf("Execute: %v", result.Err)
	}
	if !strings.Contains(result.Output, "hello from rust: 42") {
		t.Fatalf("Output = %q, want hello from rust: 42", result.Output)
	}
}

func TestEvalExpression(t *testing.T) {
	r := requireToolchain(t)
	result := r.Eval("6 * 7")
	if result.Err != nil {
		t.Fatalf("Eval: %v", result.Err)
	}
	if got, ok := result.Value.(float64); !ok || got != 42 {
		t.Fatalf("Value = %#v, want 42", result.Value)
	}
}

func TestEvalVecExpression(t *testing.T) {
	r := requireToolchain(t)
	result := r.Eval(`vec![1, 2, 3].iter().sum::<i64>()`)
	if result.Err != nil {
		t.Fatalf("Eval: %v", result.Err)
	}
	if got, ok := result.Value.(float64); !ok || got != 6 {
		t.Fatalf("Value = %#v, want 6", result.Value)
	}
}

func TestPanicBecomesError(t *testing.T) {
	r := requireToolchain(t)
	result := r.Execute(`let v: Vec<i64> = vec![]; let _ = v[3];`)
	if result.Err == nil {
		t.Fatal("expected a panic to surface as an error")
	}
	if !strings.Contains(result.Err.Error(), "panic") {
		t.Fatalf("error %q does not mention panic", result.Err)
	}
}

func TestCompileErrorIsStructured(t *testing.T) {
	r := requireToolchain(t)
	result := r.Execute(`this is not rust`)
	if result.Err == nil {
		t.Fatal("expected compile error")
	}
	if !strings.Contains(result.Err.Error(), "rust compilation failed") {
		t.Fatalf("error %q does not carry compiler output", result.Err)
	}
}

func TestBridgeCallFromRust(t *testing.T) {
	r := requireToolchain(t)
	var gotRuntime, gotCode string
	r.BridgeFn = func(runtime, code string) string {
		gotRuntime, gotCode = runtime, code
		return "1267650600228229401496703205376"
	}
	r.installBridge()
	result := r.Execute(`
let big = omnivm::call("python", "2 ** 100").unwrap();
println!("python says {}", big);
`)
	if result.Err != nil {
		t.Fatalf("Execute: %v", result.Err)
	}
	if gotRuntime != "python" || gotCode != "2 ** 100" {
		t.Fatalf("bridge saw (%q, %q)", gotRuntime, gotCode)
	}
	if !strings.Contains(result.Output, "python says 1267650600228229401496703205376") {
		t.Fatalf("Output = %q", result.Output)
	}
}

func TestSerdeStructRoundTrip(t *testing.T) {
	r := requireToolchain(t)
	tc := r.Toolchain()
	source := `
use serde::{Serialize, Deserialize};

#[derive(Serialize, Deserialize)]
struct Point { x: i64, y: i64 }

fn flip(p: Point) -> Point { Point { x: p.y, y: p.x } }

omnivm::export_fn!(OmniVMCall_flip, flip, 1);
omnivm::unit_abi_marker!();
`
	soPath, err := tc.BuildUnit(source, []string{"flip"})
	if err != nil {
		t.Fatalf("BuildUnit: %v", err)
	}
	unit, err := LoadUnit(soPath)
	if err != nil {
		t.Fatalf("LoadUnit: %v", err)
	}
	raw, err := unit.Call("OmniVMCall_flip", `[{"x": 1, "y": 2}]`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil || !env.OK {
		t.Fatalf("envelope: %v / %+v", err, env)
	}
	value, _ := json.Marshal(env.Value)
	if string(value) != `{"x":2,"y":1}` {
		t.Fatalf("flip = %s", value)
	}
}

func TestAsyncFutureDrive(t *testing.T) {
	r := requireToolchain(t)
	tc := r.Toolchain()
	source := `
async fn slow_add(a: i64, b: i64) -> i64 {
    omnivm::tokio::time::sleep(std::time::Duration::from_millis(40)).await;
    a + b
}
omnivm::export_async_fn!(OmniVMCall_slow_add, slow_add, 2);
omnivm::unit_abi_marker!();
`
	soPath, err := tc.BuildUnit(source, []string{"slow_add"})
	if err != nil {
		t.Fatalf("BuildUnit: %v", err)
	}
	unit, err := LoadUnit(soPath)
	if err != nil {
		t.Fatalf("LoadUnit: %v", err)
	}
	raw, err := unit.Call("OmniVMCall_slow_add", `[40, 2]`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	env, err := decodeEnvelope(raw)
	if err != nil || !env.OK || env.Boundary != "rust_future" {
		t.Fatalf("expected rust_future envelope, got %+v (err %v)", env, err)
	}
	pumped := 0
	r.PumpOthers = func() { pumped++ }
	value, err := r.DriveFutureByID(env.HandleID, nil)
	if err != nil {
		t.Fatalf("DriveFutureByID: %v", err)
	}
	if got, ok := value.(float64); !ok || got != 42 {
		t.Fatalf("value = %#v, want 42", value)
	}
	if pumped == 0 {
		t.Fatal("expected at least one heartbeat re-park to pump other runtimes")
	}

	var stats map[string]interface{}
	if err := json.Unmarshal([]byte(r.Support().Stats()), &stats); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if n := stats["pending_futures"].(float64); n != 0 {
		t.Fatalf("pending_futures = %v after completion, want 0 (leaked boxed future)", n)
	}
}

func TestAbandonedFutureRelease(t *testing.T) {
	r := requireToolchain(t)
	tc := r.Toolchain()
	source := `
async fn forever() -> i64 {
    omnivm::tokio::time::sleep(std::time::Duration::from_secs(3600)).await;
    0
}
omnivm::export_async_fn!(OmniVMCall_forever, forever, 0);
omnivm::unit_abi_marker!();
`
	soPath, err := tc.BuildUnit(source, []string{"forever"})
	if err != nil {
		t.Fatalf("BuildUnit: %v", err)
	}
	unit, err := LoadUnit(soPath)
	if err != nil {
		t.Fatalf("LoadUnit: %v", err)
	}
	raw, _ := unit.Call("OmniVMCall_forever", `[]`)
	env, _ := decodeEnvelope(raw)
	var handle uint64
	if _, err := json.Number(env.HandleID).Int64(); err == nil {
		n, _ := json.Number(env.HandleID).Int64()
		handle = uint64(n)
	}

	// Watchdog-timeout path: release mid-park leaves no leaked boxed future,
	// and release is idempotent.
	if !r.Support().ReleaseFuture(handle) {
		t.Fatal("first release should report the future was live")
	}
	if r.Support().ReleaseFuture(handle) {
		t.Fatal("second release should be a quiet no-op")
	}
	var stats map[string]interface{}
	json.Unmarshal([]byte(r.Support().Stats()), &stats)
	if n := stats["pending_futures"].(float64); n != 0 {
		t.Fatalf("pending_futures = %v after abandonment, want 0", n)
	}
}

func TestABIMismatchRejection(t *testing.T) {
	r := requireToolchain(t)
	tc := r.Toolchain()
	// An artifact carrying a stale baked-in ABI revision must fail to load
	// with a structured error, never load silently.
	source := `
fn nop() -> i64 { 0 }
omnivm::export_fn!(OmniVMCall_nop, nop, 0);

#[no_mangle]
pub extern "C" fn omnivm_unit_abi_v1() -> i32 { 999 }
`
	soPath, err := tc.BuildUnit(source, []string{"nop"})
	if err != nil {
		t.Fatalf("BuildUnit: %v", err)
	}
	_, err = LoadUnit(soPath)
	if err == nil {
		t.Fatal("stale-ABI artifact loaded silently")
	}
	if !strings.Contains(err.Error(), "ABI revision 999") || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("rejection not structured: %v", err)
	}
}

func TestTwoUnitIsolationSharedRuntime(t *testing.T) {
	r := requireToolchain(t)
	tc := r.Toolchain()
	// Two cdylibs exporting identical omnivm_* symbols load under RTLD_LOCAL;
	// each unit's exports resolve independently, and both observe the same
	// runtime handle (one reactor per process — shared future table).
	build := func(tag string) *Unit {
		source := `
fn whoami() -> String { "` + tag + `".to_string() }
async fn tick() -> String {
    omnivm::tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    "tick-` + tag + `".to_string()
}
omnivm::export_fn!(OmniVMCall_whoami, whoami, 0);
omnivm::export_async_fn!(OmniVMCall_tick, tick, 0);
omnivm::unit_abi_marker!();
`
		soPath, err := tc.BuildUnit(source, []string{"whoami", "tick"})
		if err != nil {
			t.Fatalf("BuildUnit(%s): %v", tag, err)
		}
		unit, err := LoadUnit(soPath)
		if err != nil {
			t.Fatalf("LoadUnit(%s): %v", tag, err)
		}
		return unit
	}
	unitA := build("alpha")
	unitB := build("beta")

	for tag, unit := range map[string]*Unit{"alpha": unitA, "beta": unitB} {
		raw, err := unit.Call("OmniVMCall_whoami", "[]")
		if err != nil {
			t.Fatalf("whoami(%s): %v", tag, err)
		}
		env, _ := decodeEnvelope(raw)
		if env.Value != tag {
			t.Fatalf("unit %s resolved to %v — symbol leakage across units", tag, env.Value)
		}
	}

	// Futures from both units land in the one shared table and drive through
	// the same support dylib.
	for tag, unit := range map[string]*Unit{"alpha": unitA, "beta": unitB} {
		raw, _ := unit.Call("OmniVMCall_tick", "[]")
		env, _ := decodeEnvelope(raw)
		if env.Boundary != "rust_future" {
			t.Fatalf("tick(%s): %+v", tag, env)
		}
		value, err := r.DriveFutureByID(env.HandleID, nil)
		if err != nil || value != "tick-"+tag {
			t.Fatalf("tick(%s) = %v (%v)", tag, value, err)
		}
	}
}

func TestUnitCacheKeyChangesWithABIRev(t *testing.T) {
	tc := &Toolchain{RustcVersion: "rustc test", LockHash: "lock"}
	a := tc.UnitCacheKey("fn x() {}", []string{"x"})
	b := tc.UnitCacheKey("fn x() {}", []string{"y"})
	c := tc.UnitCacheKey("fn x() {}", []string{"x"})
	if a == b {
		t.Fatal("cache key ignores exports")
	}
	if a != c {
		t.Fatal("cache key not deterministic")
	}
	tc2 := &Toolchain{RustcVersion: "rustc other", LockHash: "lock"}
	if tc2.UnitCacheKey("fn x() {}", []string{"x"}) == a {
		t.Fatal("cache key ignores toolchain version")
	}
}

func TestInferDependencies(t *testing.T) {
	source := `
use serde::{Serialize, Deserialize};
use reqwest::Client;
use rayon::prelude::*;
use polars::prelude::*;
use std::time::Duration;
use omnivm::tokio;
use some_unknown_crate::Thing;
`
	deps := InferDependencies(source)
	joined := strings.Join(deps, "\n")
	for _, want := range []string{"reqwest", "rustls-tls", "rayon", "polars"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("deps missing %s:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "std =") || strings.Contains(joined, "tokio =") || strings.Contains(joined, "omnivm =") {
		t.Fatalf("builtin roots leaked into deps:\n%s", joined)
	}
	if !strings.Contains(joined, `package = "some-unknown-crate"`) {
		t.Fatalf("hyphen mapping missing:\n%s", joined)
	}
	// serde is always declared explicitly by the generated Cargo.toml; the
	// inference must not duplicate it.
	if strings.Count(joined, "serde =") > 1 {
		t.Fatalf("duplicate serde dep:\n%s", joined)
	}
}

// TestDynCompileErrorHint: rustc errors about missing methods/operators on
// the gradually typed omnivm::Dyn get a gradual-typing hint appended.
func TestDynCompileErrorHint(t *testing.T) {
	out := enhanceCompileError("error[E0599]: no method named `pow` found for struct `omnivm::Dyn`")
	if !strings.Contains(out, "gradually typed (omnivm::Dyn)") {
		t.Fatalf("missing gradual-typing hint:\n%s", out)
	}
	// Same errors without Dyn involved stay un-hinted.
	out = enhanceCompileError("error[E0599]: no method named `pow` found for struct `Thing`")
	if strings.Contains(out, "gradually typed") {
		t.Fatalf("hint should require Dyn in the output:\n%s", out)
	}
	out = enhanceCompileError("error[E0369]: cannot multiply `Dyn` by `Vec<i64>`")
	if !strings.Contains(out, "annotate the parameter with a concrete type") {
		t.Fatalf("missing hint for operator error:\n%s", out)
	}
}
