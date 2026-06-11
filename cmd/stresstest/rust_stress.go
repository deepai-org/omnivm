// Rust runtime stress section: mirrors the per-runtime suites with the lanes
// that matter for the Rust peer — rapid typed-lane scalar calls, channel
// relay churn, unit compile-cache hits, a panic storm, and large-value
// crossings — then asserts the crate's liveness counters drained (the same
// absolute gate as pkg/manifest TestRustLeakGate and scripts/test-rust-leaks.sh).
//
// Runs as part of the full suite, or alone via `stresstest --rust-only`
// (no Python/JS/Ruby/JVM initialization — only the Rust toolchain).
package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/manifest"
	"github.com/omnivm/omnivm/pkg/rust"
)

// rustStressUnit is the one compilation unit shared by every Rust stress
// test (every func_def carries the same source, so the host compiles it once
// and every re-registration is a cache hit).
const rustStressUnit = `
fn st_scale(a: f64, b: f64) -> f64 { a * 10.0 + b }

fn st_echo(s: String) -> String { s }

fn st_digest(data: Vec<u8>) -> i64 {
    let mut sum: i64 = 0;
    for (i, b) in data.iter().enumerate() {
        sum += (*b as i64) * ((i % 7) as i64 + 1);
    }
    sum + data.len() as i64
}

fn st_blob(n: i64) -> omnivm::Bytes {
    omnivm::Bytes((0..n).map(|i| (i % 251) as u8).collect())
}

fn st_boom(n: i64) -> i64 { panic!("stress kaboom {n}") }

fn st_assert_eq(a: f64, b: f64) -> Result<bool, String> {
    if a == b { Ok(true) } else { Err(format!("assert_eq failed: {a} != {b}")) }
}

async fn st_relay(input: omnivm::Channel, output: omnivm::Channel) -> i64 {
    let mut moved = 0i64;
    while let Some(value) = input.recv_async().await.unwrap() {
        let n = value.as_i64().unwrap_or(0);
        output.send_async(n * 2).await.unwrap();
        moved += 1;
    }
    moved
}

async fn st_drain(input: omnivm::Channel) -> i64 {
    let mut sum = 0i64;
    while let Some(value) = input.recv_async().await.unwrap() {
        sum += value.as_i64().unwrap_or(0);
    }
    sum
}

omnivm::export_fn!(OmniVMCall_st_scale, st_scale, 2);
omnivm::export_typed_fn!(OmniVMCallTyped_st_scale, st_scale, 2);
omnivm::export_fn!(OmniVMCall_st_echo, st_echo, 1);
omnivm::export_typed_fn!(OmniVMCallTyped_st_echo, st_echo, 1);
omnivm::export_fn!(OmniVMCall_st_digest, st_digest, (bytes));
omnivm::export_fn!(OmniVMCall_st_blob, st_blob, 1);
omnivm::export_fn!(OmniVMCall_st_boom, st_boom, 1);
omnivm::export_fn!(OmniVMCall_st_assert_eq, st_assert_eq, 2);
omnivm::export_async_fn!(OmniVMCall_st_relay, st_relay, 2);
omnivm::export_async_fn!(OmniVMCall_st_drain, st_drain, 1);
`

var rustStressExports = []string{
	"st_scale", "st_echo", "st_digest", "st_blob", "st_boom", "st_assert_eq", "st_relay", "st_drain",
}

func rustStressFuncDefs() []*manifest.Op {
	defs := []struct {
		name  string
		arity int
		async bool
	}{
		{"st_scale", 2, false},
		{"st_echo", 1, false},
		{"st_digest", 1, false},
		{"st_blob", 1, false},
		{"st_boom", 1, false},
		{"st_assert_eq", 2, false},
		{"st_relay", 2, true},
		{"st_drain", 1, true},
	}
	ops := make([]*manifest.Op, 0, len(defs))
	for _, def := range defs {
		params := make([]*manifest.Param, def.arity)
		for p := range params {
			params[p] = &manifest.Param{Name: fmt.Sprintf("p%d", p)}
		}
		ops = append(ops, &manifest.Op{
			OpType:      "func_def",
			Name:        def.name,
			BodyRuntime: "rust",
			Async:       def.async,
			Params:      params,
			Exports:     rustStressExports,
			Source:      rustStressUnit,
		})
	}
	return ops
}

// rustStressCall invokes a registered Rust export through the executor's
// bridge contract ({func,args}) and decodes the result envelope — the same
// path guest-runtime stubs take.
func rustStressCall(e *manifest.Executor, fn string, args ...interface{}) (interface{}, error) {
	if args == nil {
		args = []interface{}{}
	}
	payload, err := json.Marshal(map[string]interface{}{"func": fn, "args": args})
	if err != nil {
		return nil, err
	}
	raw, err := e.HandleCall(string(payload))
	if err != nil {
		return nil, err
	}
	var out struct {
		Value interface{} `json:"value"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode bridge result %.120q: %w", raw, err)
	}
	return out.Value, nil
}

func rustStressNum(v interface{}) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

// runRustStressTests registers the Rust stress tests through the suite's
// run(name, fn) reporter. Skips (without failing) when no toolchain exists.
func runRustStressTests(run func(string, func() error)) {
	if _, err := rust.GetToolchain(); err != nil {
		fmt.Printf("[SKIP] Rust stress tests: toolchain unavailable: %v\n", err)
		return
	}

	// One runtimes map for the whole section (separate from the suite's
	// global registry): the executor creates the Rust runtime lazily and
	// stores it here, so every test — including the fresh executors in the
	// compile-cache round and the final liveness gate — shares one support
	// dylib + reactor.
	rustRuntimes := map[string]pkg.Runtime{}
	exec := manifest.NewExecutor(rustRuntimes)

	run("Rust unit compile + export registration", func() error {
		return exec.Execute(&manifest.Manifest{Version: 1, Ops: rustStressFuncDefs()})
	})

	rustRT, _ := rustRuntimes["rust"].(*rust.Runtime)
	if rustRT == nil || rustRT.Support() == nil {
		fmt.Println("[SKIP] Rust stress tests: runtime did not initialize (compile failed above)")
		return
	}

	// Test: 1000 rapid sequential scalar calls through the typed lane
	// (omni_value_t crossings, no JSON text).
	run("Rust rapid typed scalar calls (1000x)", func() error {
		before := atomic.LoadUint64(&rust.TypedCallCount)
		start := time.Now()
		for i := 0; i < 1000; i++ {
			value, err := rustStressCall(exec, "st_scale", float64(i), 3.0)
			if err != nil {
				return fmt.Errorf("call %d: %w", i, err)
			}
			if n, ok := rustStressNum(value); !ok || n != float64(i)*10+3 {
				return fmt.Errorf("call %d: st_scale = %#v, want %g", i, value, float64(i)*10+3)
			}
		}
		typed := atomic.LoadUint64(&rust.TypedCallCount) - before
		if typed < 1000 {
			return fmt.Errorf("typed lane taken only %d/1000 times", typed)
		}
		fmt.Printf("(%v for 1000 calls, %d typed) ", time.Since(start).Round(time.Millisecond), typed)
		return nil
	})

	// Test: channel relay churn — a spawned Rust task relays between two
	// manifest channels, 20 rounds, every round drained to exhaustion (the
	// done pull is what releases the host-side channel handles).
	run("Rust channel relay churn (20 rounds)", func() error {
		for round := 0; round < 20; round++ {
			ops := []*manifest.Op{
				{OpType: "chan", Action: "make", Bind: "st_jobs", Size: 8},
				{OpType: "chan", Action: "make", Bind: "st_results", Size: 16},
				{OpType: "spawn", Code: "st_relay(st_jobs, st_results)", Bind: "st_job"},
			}
			for v := 1; v <= 4; v++ {
				ops = append(ops, &manifest.Op{OpType: "chan", Action: "send", Channel: "st_jobs",
					Value: &manifest.ValueExpr{Kind: "literal", Value: v}})
			}
			ops = append(ops,
				&manifest.Op{OpType: "chan", Action: "close", Channel: "st_jobs"},
				&manifest.Op{OpType: "await", From: &manifest.Op{OpType: "declare", Bind: "__stj",
					Value: &manifest.ValueExpr{Kind: "ref", Name: "st_job"}}, Bind: "st_moved"},
				&manifest.Op{OpType: "eval", Runtime: "rust", Code: "st_assert_eq(st_moved, 4)"},
				&manifest.Op{OpType: "chan", Action: "close", Channel: "st_results"},
				&manifest.Op{OpType: "eval", Runtime: "rust", Code: "st_drain(st_results)", Bind: "st_sum"},
				// (1+2+3+4)*2 = 20: order/loss check for the round.
				&manifest.Op{OpType: "eval", Runtime: "rust", Code: "st_assert_eq(st_sum, 20)"},
			)
			if err := exec.Execute(&manifest.Manifest{Version: 1, Ops: ops}); err != nil {
				return fmt.Errorf("round %d: %w", round, err)
			}
		}
		return nil
	})

	// Test: repeated unit compile-cache hits — fresh executors re-register
	// the same unit; the SHA-keyed artifact cache must serve every round.
	run("Rust unit compile-cache hits (10 executors)", func() error {
		start := time.Now()
		for round := 0; round < 10; round++ {
			fresh := manifest.NewExecutor(rustRuntimes)
			if err := fresh.Execute(&manifest.Manifest{Version: 1, Ops: rustStressFuncDefs()}); err != nil {
				return fmt.Errorf("round %d: %w", round, err)
			}
			value, err := rustStressCall(fresh, "st_scale", 4.0, 2.0)
			if err != nil {
				return fmt.Errorf("round %d call: %w", round, err)
			}
			if n, ok := rustStressNum(value); !ok || n != 42 {
				return fmt.Errorf("round %d: st_scale = %#v, want 42", round, value)
			}
		}
		elapsed := time.Since(start)
		if elapsed > 60*time.Second {
			return fmt.Errorf("10 cached re-registrations took %v — cache not serving", elapsed)
		}
		fmt.Printf("(%v for 10 rounds) ", elapsed.Round(time.Millisecond))
		// Rebind the main executor's bridge router (process-global trampoline
		// was refreshed by the fresh executors above).
		return exec.Execute(&manifest.Manifest{Version: 1, Ops: rustStressFuncDefs()})
	})

	// Test: error/panic storm — 500 Rust panics must surface as structured
	// errors (no abort, no poisoned state), and the unit stays healthy.
	run("Rust panic storm (500 panics caught)", func() error {
		for i := 0; i < 500; i++ {
			_, err := rustStressCall(exec, "st_boom", i)
			if err == nil {
				return fmt.Errorf("panic %d did not surface as an error", i)
			}
			if !strings.Contains(err.Error(), "stress kaboom") {
				return fmt.Errorf("panic %d lost its message: %v", i, err)
			}
		}
		value, err := rustStressCall(exec, "st_scale", 1.0, 1.0)
		if err != nil {
			return fmt.Errorf("unit unhealthy after panic storm: %w", err)
		}
		if n, ok := rustStressNum(value); !ok || n != 11 {
			return fmt.Errorf("post-storm call = %#v, want 11", value)
		}
		return nil
	})

	// Test: large-value crossings — a 1MB string through the typed lane both
	// directions, and 100KB byte payloads both directions (base64 inbound
	// through the bytes-kind lane, omnivm::Bytes marker outbound).
	run("Rust large values (1MB string, 100KB bytes)", func() error {
		big := strings.Repeat("omnivm-stress-0123456789", 1<<20/24+1)[:1<<20]
		value, err := rustStressCall(exec, "st_echo", big)
		if err != nil {
			return fmt.Errorf("1MB echo: %w", err)
		}
		if s, ok := value.(string); !ok || s != big {
			return fmt.Errorf("1MB string did not round-trip (got %T, %d bytes)", value, len(fmt.Sprint(value)))
		}

		payload := make([]byte, 100*1024)
		var want int64
		for i := range payload {
			payload[i] = byte(i % 251)
			want += int64(payload[i]) * int64(i%7+1)
		}
		want += int64(len(payload))
		value, err = rustStressCall(exec, "st_digest", base64.StdEncoding.EncodeToString(payload))
		if err != nil {
			return fmt.Errorf("100KB digest: %w", err)
		}
		if n, ok := rustStressNum(value); !ok || n != float64(want) {
			return fmt.Errorf("100KB digest = %#v, want %d", value, want)
		}

		// Outbound + inbound 100KB bytes round trip inside one manifest:
		// st_blob's omnivm::Bytes return becomes a host []byte binding, which
		// crosses back into st_digest through the bytes lane; the digest
		// (content-sensitive) proves both crossings byte-exact. st_blob
		// generates the same i%251 pattern as `payload` above, so `want`
		// is the oracle for both.
		if err := exec.Execute(&manifest.Manifest{Version: 1, Ops: []*manifest.Op{
			{OpType: "eval", Runtime: "rust", Code: fmt.Sprintf("st_blob(%d)", len(payload)), Bind: "st_b"},
			{OpType: "eval", Runtime: "rust", Code: "st_digest(st_b)", Bind: "st_dig"},
			{OpType: "eval", Runtime: "rust", Code: fmt.Sprintf("st_assert_eq(st_dig, %d)", want)},
		}}); err != nil {
			return fmt.Errorf("100KB blob round trip: %w", err)
		}
		return nil
	})

	// Gate: after all of the above, the crate's liveness counters must read
	// zero — leak gates are absolute (drained or failed).
	run("Rust liveness counters drained", func() error {
		var stats map[string]interface{}
		if err := json.Unmarshal([]byte(rustRT.Support().Stats()), &stats); err != nil {
			return fmt.Errorf("decode stats: %w", err)
		}
		var leaks []string
		for _, key := range []string{"live_objects", "live_cdata_shells", "live_byte_buffers", "pending_futures", "pending_bridge_calls"} {
			if stats[key] != float64(0) {
				leaks = append(leaks, fmt.Sprintf("%s=%v", key, stats[key]))
			}
		}
		if len(leaks) > 0 {
			return fmt.Errorf("LEAK: %s (full stats: %v)", strings.Join(leaks, " "), stats)
		}
		return nil
	})
}

// runRustOnly is the `stresstest --rust-only` entrypoint: the Rust section
// with its own pass/fail reporting, no other runtimes initialized.
func runRustOnly() int {
	fmt.Println("=== OmniVM Rust Runtime Stress Test (--rust-only) ===")
	fmt.Println()
	passed, failed := 0, 0
	run := func(name string, fn func() error) {
		fmt.Printf("[TEST] %s... ", name)
		if err := fn(); err != nil {
			fmt.Printf("FAILED: %v\n", err)
			failed++
		} else {
			fmt.Println("PASSED")
			passed++
		}
	}
	runRustStressTests(run)
	fmt.Println()
	fmt.Printf("Results: %d passed, %d failed out of %d tests\n", passed, failed, passed+failed)
	if failed > 0 {
		return 1
	}
	return 0
}
