package rust

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omnivm/omnivm/pkg"
)

// Runtime implements pkg.Runtime and pkg.FileExecutor for Rust via cdylibs.
type Runtime struct {
	// BridgeFn, when set, services cross-runtime calls from Rust through a Go
	// closure (mirrors golang.Runtime.BridgeFn). Used when the host has no C
	// bridge pointers (manifest executor, tests).
	BridgeFn func(runtime, code string) string

	// TaskFDFn, when set, returns the dispatcher's task eventfd (or -1):
	// parked awaits watch it as an edge-observed AsyncFd so dispatcher work
	// (including fast-channel calls) is never starved by a Rust park.
	// Binary mode only.
	TaskFDFn func() int

	// UVDeadline, when set, returns libuv's uv_backend_timeout() in ms (-1
	// for none). The heartbeat slice becomes min(floor, uv deadline) so a JS
	// setInterval(…, 7) feeding a Rust channel gets pumped at 7ms cadence
	// instead of riding a 10ms heartbeat with jitter. asyncio publishes no
	// equivalent and stays on the fixed heartbeat.
	UVDeadline func() int

	// PumpOthers, when set, pumps the other runtimes between re-parks (the
	// heartbeat-pump arm of the await select loop).
	PumpOthers func()

	callPtr, freePtr uintptr

	tc      *Toolchain
	support *Support
}

func New() *Runtime { return &Runtime{} }

func (r *Runtime) Name() string { return "rust" }

func (r *Runtime) Initialize() error {
	tc, err := GetToolchain()
	if err != nil {
		return fmt.Errorf("rust: %w", err)
	}
	r.tc = tc
	support, err := GetSupport()
	if err != nil {
		return fmt.Errorf("rust: %w", err)
	}
	r.support = support
	// Executor knob: current-thread by default, multi as explicit escalation.
	if os.Getenv("OMNIVM_RUST_EXECUTOR") == "multi" {
		support.SetExecutor(1)
	}
	r.installBridge()
	return nil
}

// SetBridgeCallback installs the cross-runtime callback function pointers
// (OmniCall / OmniFree shapes).
func (r *Runtime) SetBridgeCallback(callPtr, freePtr uintptr) {
	r.callPtr, r.freePtr = callPtr, freePtr
	r.installBridge()
}

func (r *Runtime) installBridge() {
	if r.support == nil {
		return
	}
	if r.callPtr != 0 {
		r.support.InstallBridge(r.callPtr, r.freePtr)
		return
	}
	if r.BridgeFn != nil {
		SetGoBridge(r.BridgeFn)
		r.support.InstallBridge(0, 0)
	}
}

// Pump drives ready tokio tasks, expired timers, and the I/O reactor inline
// on the golden thread, then services any outbound bridge calls.
func (r *Runtime) Pump() {
	if r.support == nil {
		return
	}
	out := r.support.Pump()
	r.serviceBridgeCalls(out)
}

func (r *Runtime) Shutdown() error { return nil }

// Support exposes the loaded support dylib (nil before Initialize).
func (r *Runtime) Support() *Support { return r.support }

// Toolchain exposes the resolved toolchain (nil before Initialize).
func (r *Runtime) Toolchain() *Toolchain { return r.tc }

// ---------------------------------------------------------------------------
// Execute / Eval / ExecuteFile
// ---------------------------------------------------------------------------

const (
	runEntry  = "__omnivm_run"
	evalEntry = "__omnivm_eval"
	fileEntry = "__omnivm_file_entry"
)

// Execute compiles a Rust snippet as a cdylib and runs it. With a warm cache
// it is a dlopen; cold, it is a cargo build — the same deal Go already has.
func (r *Runtime) Execute(code string) pkg.Result {
	source, entry := wrapSnippet(code)
	return r.compileAndRun(source, entry)
}

// Eval evaluates a Rust expression and returns its value.
func (r *Runtime) Eval(code string) pkg.Result {
	source := wrapEval(code)
	result := r.compileAndRun(source, evalEntry)
	if result.Err == nil && result.Value == nil {
		result.Value = strings.TrimSpace(result.Output)
		result.Output = ""
	}
	return result
}

// ExecuteFile compiles a .rs file and calls its main().
func (r *Runtime) ExecuteFile(path string, args []string, stdin io.Reader) pkg.Result {
	data, err := os.ReadFile(path)
	if err != nil {
		return pkg.Result{Err: fmt.Errorf("rust: %w", err), ExitCode: 1}
	}
	source := string(data)
	if !strings.Contains(source, "fn main") {
		return pkg.Result{Err: fmt.Errorf("rust: %s has no fn main()", filepath.Base(path)), ExitCode: 1}
	}
	wrapped := source + "\n\nfn " + fileEntry + "() { main() }\n" +
		"omnivm::export_fn!(OmniVMCall_" + fileEntry + ", " + fileEntry + ", 0);\n" +
		"omnivm::unit_abi_marker!();\n"
	return r.compileAndRun(wrapped, fileEntry)
}

func (r *Runtime) compileAndRun(source, entry string) pkg.Result {
	if r.tc == nil || r.support == nil {
		if err := r.Initialize(); err != nil {
			return pkg.Result{Err: err, ExitCode: 1}
		}
	}
	r.installBridge()

	soPath, err := r.tc.BuildUnit(source, []string{entry})
	if err != nil {
		return pkg.Result{Err: err, ExitCode: 1}
	}
	unit, err := LoadUnit(soPath)
	if err != nil {
		return pkg.Result{Err: err, ExitCode: 1}
	}

	var raw string
	var callErr error
	output := captureFD1(func() {
		raw, callErr = unit.Call("OmniVMCall_"+entry, "[]")
	})
	if callErr != nil {
		return pkg.Result{Output: output, Err: callErr, ExitCode: 1}
	}

	env, err := decodeEnvelope(raw)
	if err != nil {
		return pkg.Result{Output: output, Err: err, ExitCode: 1}
	}
	if !env.OK {
		return pkg.Result{Output: output, Err: fmt.Errorf("%s", env.Error), ExitCode: 1}
	}

	// Async entrypoint: drive the stored future to completion.
	if env.Boundary == "rust_future" {
		value, driveErr := r.DriveFutureByID(env.HandleID, nil)
		if driveErr != nil {
			return pkg.Result{Output: output, Err: driveErr, ExitCode: 1}
		}
		return pkg.Result{Output: output, Value: value}
	}

	var value interface{}
	if env.Value != nil {
		value = env.Value
	}
	return pkg.Result{Output: output, Value: value}
}

// envelope is the subset of the c-shared envelope pkg/rust needs standalone;
// the manifest layer decodes the full envelope with its shared machinery.
type envelope struct {
	OK       bool        `json:"ok"`
	Boundary string      `json:"boundary,omitempty"`
	HandleID string      `json:"handle_id,omitempty"`
	Kind     string      `json:"kind,omitempty"`
	Value    interface{} `json:"value,omitempty"`
	Error    string      `json:"error,omitempty"`
}

func decodeEnvelope(raw string) (*envelope, error) {
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("rust: decode envelope: %w (raw: %.200s)", err, raw)
	}
	return &env, nil
}

// ---------------------------------------------------------------------------
// The re-park drive loop (host side)
// ---------------------------------------------------------------------------

// DriveResponse mirrors omnivm_rs_drive_v1's JSON.
type DriveResponse struct {
	Done        bool            `json:"done"`
	Pending     bool            `json:"pending"`
	Reason      string          `json:"reason"`
	Envelope    json.RawMessage `json:"envelope"`
	BridgeCalls []BridgeCall    `json:"bridge_calls"`
}

type BridgeCall struct {
	ID      uint64 `json:"id"`
	Runtime string `json:"runtime"`
	Code    string `json:"code"`
}

// DriveTimeout bounds a single awaited future (watchdog deadline composed
// inline with the park).
var DriveTimeout = 30 * time.Second

// DriveHeartbeatMS is the heartbeat-pump floor: while parked, no call
// boundaries occur, so the park exits at least this often to pump the other
// runtimes' reactors.
const DriveHeartbeatMS = 10

// DriveFuture drives a stored future to completion through the re-park loop.
// bridge handles outbound calls (nil falls back to BridgeFn); returns the
// raw envelope JSON of the completed future.
func (r *Runtime) DriveFuture(handle uint64, bridge func(runtime, code string) (string, error)) (json.RawMessage, error) {
	if r.support == nil {
		return nil, fmt.Errorf("rust: runtime not initialized")
	}
	deadline := time.Now().Add(DriveTimeout)
	for {
		if time.Now().After(deadline) {
			// Abandonment releases the boxed future through the handle table
			// — quiet, idempotent, and dropping the box is tokio cancellation.
			r.support.ReleaseFuture(handle)
			return nil, fmt.Errorf("rust: await timed out after %s (future released)", DriveTimeout)
		}
		// Heartbeat slice: uv_backend_timeout-aware for the JS pump arm.
		sliceMS := uint64(DriveHeartbeatMS)
		if r.UVDeadline != nil {
			if t := r.UVDeadline(); t >= 0 && uint64(t) < sliceMS {
				sliceMS = uint64(t)
				if sliceMS == 0 {
					sliceMS = 1
				}
			}
		}
		taskFD := -1
		if r.TaskFDFn != nil {
			taskFD = r.TaskFDFn()
		}
		raw := r.support.Drive(handle, sliceMS, taskFD)
		var resp DriveResponse
		if err := json.Unmarshal([]byte(raw), &resp); err != nil {
			return nil, fmt.Errorf("rust: decode drive response: %w (raw: %.200s)", err, raw)
		}
		r.handleBridgeCalls(resp.BridgeCalls, bridge)
		if resp.Done {
			return resp.Envelope, nil
		}
		// Heartbeat pump arm: cross-runtime-entangled futures (e.g. a Rust
		// future awaiting a manifest channel fed by a JS interval) only make
		// progress if the other reactors get pumped between parks.
		if r.PumpOthers != nil && resp.Reason != "bridge" {
			r.PumpOthers()
		}
	}
}

// DriveFutureByID is DriveFuture for a decimal handle string, returning the
// decoded envelope value.
func (r *Runtime) DriveFutureByID(handleID string, bridge func(runtime, code string) (string, error)) (interface{}, error) {
	var handle uint64
	if _, err := fmt.Sscanf(handleID, "%d", &handle); err != nil {
		return nil, fmt.Errorf("rust: invalid future handle %q", handleID)
	}
	envJSON, err := r.DriveFuture(handle, bridge)
	if err != nil {
		return nil, err
	}
	env, err := decodeEnvelope(string(envJSON))
	if err != nil {
		return nil, err
	}
	if !env.OK {
		return nil, fmt.Errorf("%s", env.Error)
	}
	return env.Value, nil
}

func (r *Runtime) handleBridgeCalls(calls []BridgeCall, bridge func(runtime, code string) (string, error)) {
	for _, call := range calls {
		var result string
		var err error
		switch {
		case bridge != nil:
			result, err = bridge(call.Runtime, call.Code)
		case r.BridgeFn != nil:
			out := r.BridgeFn(call.Runtime, call.Code)
			if msg, isErr := strings.CutPrefix(out, "ERR:"); isErr {
				err = fmt.Errorf("%s", msg)
			} else {
				result = out
			}
		default:
			err = fmt.Errorf("no bridge available for outbound rust call to %s", call.Runtime)
		}
		if err != nil {
			r.support.CompleteBridge(call.ID, false, err.Error())
		} else {
			r.support.CompleteBridge(call.ID, true, result)
		}
	}
}

func (r *Runtime) serviceBridgeCalls(pumpJSON string) {
	if !strings.Contains(pumpJSON, "bridge_calls") {
		return
	}
	var out struct {
		BridgeCalls []BridgeCall `json:"bridge_calls"`
	}
	if err := json.Unmarshal([]byte(pumpJSON), &out); err != nil {
		return
	}
	r.handleBridgeCalls(out.BridgeCalls, nil)
}

// ---------------------------------------------------------------------------
// Snippet wrapping
// ---------------------------------------------------------------------------

// wrapSnippet turns a snippet into a complete compilation unit. Statements
// wrap into a fn body; sources with their own items (fn main, structs, ...)
// pass through with an entry shim appended.
func wrapSnippet(code string) (string, string) {
	if strings.Contains(code, "fn main") {
		return code + "\n\nfn " + fileEntry + "() { main() }\n" +
			"omnivm::export_fn!(OmniVMCall_" + fileEntry + ", " + fileEntry + ", 0);\n" +
			"omnivm::unit_abi_marker!();\n", fileEntry
	}
	return "#[allow(unused)]\nfn " + runEntry + "() {\n" + code + "\n}\n" +
		"omnivm::export_fn!(OmniVMCall_" + runEntry + ", " + runEntry + ", 0);\n" +
		"omnivm::unit_abi_marker!();\n", runEntry
}

// wrapEval wraps an expression in a function returning its serde projection.
func wrapEval(code string) string {
	return "#[allow(unused)]\nfn " + evalEntry + "() -> omnivm::Value {\n" +
		"    omnivm::serde_json::json!(" + code + ")\n}\n" +
		"omnivm::export_fn!(OmniVMCall_" + evalEntry + ", " + evalEntry + ", 0);\n" +
		"omnivm::unit_abi_marker!();\n"
}
