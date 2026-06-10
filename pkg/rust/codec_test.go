package rust

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// TestChronoTemporalRoundTrip: chrono timestamps cross the boundary with
// timezone and sub-second precision intact (serde RFC3339 projection), not
// degraded to second-resolution or UTC-normalized strings.
func TestChronoTemporalRoundTrip(t *testing.T) {
	r := requireToolchain(t)
	source := `
use chrono::{DateTime, FixedOffset};

fn echo_time(t: DateTime<FixedOffset>) -> DateTime<FixedOffset> { t }

fn shift_time(t: DateTime<FixedOffset>) -> DateTime<FixedOffset> {
    t + chrono::Duration::nanoseconds(1)
}

omnivm::export_fn!(OmniVMCall_echo_time, echo_time, 1);
omnivm::export_fn!(OmniVMCall_shift_time, shift_time, 1);
omnivm::unit_abi_marker!();
`
	unit := buildAndLoad(t, r, source, []string{"echo_time", "shift_time"})

	const stamp = `"2026-06-10T22:13:20.123456789+05:45"`
	raw, err := unit.Call("OmniVMCall_echo_time", "["+stamp+"]")
	if err != nil {
		t.Fatalf("echo_time: %v", err)
	}
	env, _ := decodeEnvelope(raw)
	if !env.OK {
		t.Fatalf("echo_time: %s", env.Error)
	}
	got, _ := env.Value.(string)
	if !strings.Contains(got, ".123456789") {
		t.Fatalf("sub-second precision lost: %q", got)
	}
	if !strings.Contains(got, "+05:45") {
		t.Fatalf("timezone lost: %q", got)
	}

	raw, _ = unit.Call("OmniVMCall_shift_time", "["+stamp+"]")
	env, _ = decodeEnvelope(raw)
	if got, _ := env.Value.(string); !strings.Contains(got, ".123456790") {
		t.Fatalf("nanosecond arithmetic did not survive the boundary: %q", got)
	}
}

// TestAbortHonesty: catch_unwind cannot catch aborts. A dependency calling
// std::process::abort() takes the worker down through the documented recycle
// path — it cannot become a RuntimeError, and it must not be silent.
func TestAbortHonesty(t *testing.T) {
	if os.Getenv("OMNIVM_RUST_ABORT_CHILD") == "1" {
		r := requireToolchain(t)
		source := `
fn hard_abort() -> i64 {
    std::process::abort();
}
omnivm::export_fn!(OmniVMCall_hard_abort, hard_abort, 0);
omnivm::unit_abi_marker!();
`
		unit := buildAndLoad(t, r, source, []string{"hard_abort"})
		_, _ = unit.Call("OmniVMCall_hard_abort", "[]")
		t.Fatal("abort was swallowed — it must never become a RuntimeError")
		return
	}

	if _, err := GetToolchain(); err != nil {
		t.Skipf("rust toolchain unavailable: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestAbortHonesty$", "-test.v")
	cmd.Env = append(os.Environ(), "OMNIVM_RUST_ABORT_CHILD=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("worker survived an abort (output: %.300s)", out)
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected worker death, got %v", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if ok && status.Signaled() && status.Signal() != syscall.SIGABRT {
		t.Fatalf("worker died with %v, want SIGABRT", status.Signal())
	}
	if strings.Contains(string(out), "--- PASS: TestAbortHonesty") {
		t.Fatalf("abort must not be reported as a passing call:\n%.300s", out)
	}
}

// TestLogFacadeBridge: crates logging through the `log` facade stay visible
// (forwarded with a structured prefix and counted in runtime stats).
func TestLogFacadeBridge(t *testing.T) {
	r := requireToolchain(t)
	r.BridgeFn = func(runtime, code string) string { return "" }
	r.installBridge() // installs the facade
	source := `
fn noisy() -> i64 {
    omnivm::log::info!(target: "unit_test", "facade record {}", 7);
    7
}
omnivm::export_fn!(OmniVMCall_noisy, noisy, 0);
omnivm::unit_abi_marker!();
`
	unit := buildAndLoad(t, r, source, []string{"noisy"})
	before := logRecords(t, r)
	raw, err := unit.Call("OmniVMCall_noisy", "[]")
	if err != nil {
		t.Fatalf("noisy: %v", err)
	}
	if env, _ := decodeEnvelope(raw); !env.OK {
		t.Fatalf("noisy: %s", env.Error)
	}
	if after := logRecords(t, r); after != before+1 {
		t.Fatalf("log_records_forwarded %v -> %v, want +1", before, after)
	}
}

func logRecords(t *testing.T, r *Runtime) float64 {
	t.Helper()
	var stats map[string]interface{}
	if err := json.Unmarshal([]byte(r.Support().Stats()), &stats); err != nil {
		t.Fatalf("stats: %v", err)
	}
	n, _ := stats["log_records_forwarded"].(float64)
	return n
}
