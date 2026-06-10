package manifest

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/rust"
)

// compileRustPlugin handles func_def ops with bodyRuntime:"rust" and a source
// field. The source is a complete compilation unit (every Rust func_def in a
// program carries the same unit, so the SHA256 cache compiles it exactly
// once); it compiles to a cdylib, loads via dlopen under RTLD_LOCAL, and the
// exports register in the executor's goFuncs registry speaking the same
// envelope contract as c-shared Go plugins — so owned buffers, object
// proxies, and typed slices reuse the existing decode machinery.
func (e *Executor) compileRustPlugin(op *Op) error {
	if op.Source == "" {
		return fmt.Errorf("rust func_def %q: missing source", op.Name)
	}
	exports := append([]string(nil), op.Exports...)
	if len(exports) == 0 && op.Name != "" {
		exports = []string{op.Name}
	}
	for _, exportName := range exports {
		if !goIdentifierRE.MatchString(exportName) {
			return fmt.Errorf("rust func_def: invalid export %q", exportName)
		}
	}

	rustRT, err := e.rustRuntime()
	if err != nil {
		return err
	}

	source := rustUnitSource(op, exports)

	tc, err := rust.GetToolchain()
	if err != nil {
		return err
	}
	soPath, err := tc.BuildUnit(source, exports)
	if err != nil {
		return fmt.Errorf("rust func_def %q: %w", op.Name, err)
	}
	unit, err := rust.LoadUnit(soPath)
	if err != nil {
		return fmt.Errorf("rust func_def %q: %w", op.Name, err)
	}

	for _, exportName := range exports {
		exportName := exportName
		symbol := cSharedWrapperSymbol(exportName)
		fn := func(args []interface{}) interface{} {
			encodedArgs, leases, encodeErr := e.encodeCSharedGoArgs(args)
			if encodeErr != nil {
				panic(encodeErr)
			}
			defer func() {
				for _, lease := range leases {
					lease.release()
				}
			}()
			value, callErr := e.callRustUnit(rustRT, unit, symbol, encodedArgs)
			if callErr != nil {
				panic(callErr)
			}
			return normalizeArg(value)
		}
		if _, exists := e.goFuncs[exportName]; !exists {
			e.goFuncs[exportName] = fn
		}
	}

	if op.Name != "" && len(exports) > 0 {
		if _, exists := e.goFuncs[op.Name]; !exists {
			e.goFuncs[op.Name] = e.goFuncs[exports[0]]
		}
	}

	if op.Name != "" {
		fd := &FuncDef{Name: op.Name, Params: op.Params}
		if err := e.registerStubs(fd); err != nil {
			return fmt.Errorf("rust func_def stubs: %w", err)
		}
	}
	return nil
}

// rustRuntime fetches the initialized Rust runtime and wires the executor's
// bridge/pump hooks into it (idempotent). Hosts that did not register a
// "rust" runtime get one lazily — like Go func_defs, Rust units need no
// host-side registration to work in any manifest host.
func (e *Executor) rustRuntime() (*rust.Runtime, error) {
	var rustRT *rust.Runtime
	if rt, ok := e.runtimes["rust"]; ok {
		typed, isRust := rt.(*rust.Runtime)
		if !isRust {
			return nil, fmt.Errorf("runtime %q is %T, want *rust.Runtime", "rust", rt)
		}
		rustRT = typed
	} else {
		rustRT = rust.New()
		e.runtimes["rust"] = rustRT
	}
	if rustRT.Support() == nil {
		if err := rustRT.Initialize(); err != nil {
			return nil, err
		}
	}
	// Heartbeat-pump arm: while a Rust park holds the golden thread, the
	// other reactors (libuv, asyncio) ride this between re-parks.
	if rustRT.PumpOthers == nil {
		rustRT.PumpOthers = func() {
			for name, other := range e.runtimes {
				if name != "rust" {
					other.Pump()
				}
			}
			arrow.GlobalStore().DrainDeferred()
		}
	}
	// JS pump cadence: ride uv_backend_timeout() instead of a fixed heartbeat
	// when the JS runtime exposes it.
	if rustRT.UVDeadline == nil {
		if jsrt, hasJS := e.runtimes["javascript"]; hasJS {
			if uv, hasTimeout := jsrt.(interface{ GetUVBackendTimeout() int }); hasTimeout {
				rustRT.UVDeadline = uv.GetUVBackendTimeout
			}
		}
	}
	// Bridge: if the host already installed C pointers (OmniCall/OmniFree),
	// keep them; otherwise route through the executor directly.
	if rustRT.Support() != nil && !rustRT.Support().BridgeInstalled() && rustRT.BridgeFn == nil {
		rustRT.BridgeFn = e.rustBridgeRouter
		rustRT.SetBridgeCallback(0, 0)
	}
	return rustRT, nil
}

// rustBridgeRouter services omnivm::call from Rust code in executor-hosted
// contexts: "__manifest" routes to HandleCall, anything else evaluates in the
// named runtime — the same contract as the host OmniCall export.
func (e *Executor) rustBridgeRouter(rtName, code string) string {
	if rtName == "__manifest" {
		res, err := e.HandleCall(code)
		if err != nil {
			return "ERR:" + err.Error()
		}
		return res
	}
	rt, ok := e.runtimes[rtName]
	if !ok {
		return "ERR:unknown runtime: " + rtName
	}
	result := rt.Eval(code)
	if result.Err != nil {
		return "ERR:" + result.Err.Error()
	}
	if result.Value != nil {
		return fmt.Sprintf("%v", result.Value)
	}
	return result.Output
}

// callRustUnit invokes one exported symbol and decodes the envelope,
// transparently driving stored futures (async fns) to completion through the
// re-park loop.
func (e *Executor) callRustUnit(rustRT *rust.Runtime, unit *rust.Unit, symbol string, args []interface{}) (interface{}, error) {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("marshal rust args: %w", err)
	}
	raw, err := unit.Call(symbol, string(argsJSON))
	if err != nil {
		return nil, err
	}
	return e.decodeRustEnvelope(rustRT, unit, raw)
}

func (e *Executor) decodeRustEnvelope(rustRT *rust.Runtime, unit *rust.Unit, raw string) (interface{}, error) {
	var env cSharedPluginEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("decode rust result: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("%s", env.Error)
	}
	if env.Boundary == "rust_future" {
		return e.driveRustFuture(rustRT, unit, env.HandleID)
	}
	return decodeCSharedEnvelopeValue(cSharedPluginHandle(unit.Handle()), env)
}

// driveRustFuture awaits a stored future: the golden thread re-parks in
// tokio's select loop while the heartbeat arm keeps the other runtimes
// pumped, and outbound bridge calls (the async hop) run between parks as
// plain executor work.
func (e *Executor) driveRustFuture(rustRT *rust.Runtime, unit *rust.Unit, handleID string) (interface{}, error) {
	var handle uint64
	if _, err := fmt.Sscanf(handleID, "%d", &handle); err != nil {
		return nil, fmt.Errorf("rust future: invalid handle %q", handleID)
	}
	envJSON, err := rustRT.DriveFuture(handle, func(rtName, code string) (string, error) {
		out := e.rustBridgeRouter(rtName, code)
		if msg, isErr := strings.CutPrefix(out, "ERR:"); isErr {
			return "", fmt.Errorf("%s", msg)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	var env cSharedPluginEnvelope
	if err := json.Unmarshal(envJSON, &env); err != nil {
		return nil, fmt.Errorf("decode rust future result: %w", err)
	}
	if !env.OK {
		return nil, fmt.Errorf("%s", env.Error)
	}
	return decodeCSharedEnvelopeValue(cSharedPluginHandle(unit.Handle()), env)
}

// rustUnitSource completes a func_def source into a full compilation unit:
// codegen-emitted sources already carry export shims; hand-written manifest
// sources get shims generated from the op's params, and every unit gets the
// baked ABI marker.
func rustUnitSource(op *Op, exports []string) string {
	source := op.Source
	var extra strings.Builder
	if !strings.Contains(source, "export_fn!") && !strings.Contains(source, "export_async_fn!") {
		arity := len(op.Params)
		macro := "export_fn!"
		if op.Async {
			macro = "export_async_fn!"
		}
		for _, exportName := range exports {
			fmt.Fprintf(&extra, "omnivm::%s(%s, %s, %d);\n", macro, cSharedWrapperSymbol(exportName), exportName, arity)
		}
	}
	if !strings.Contains(source, "unit_abi_marker!") {
		extra.WriteString("omnivm::unit_abi_marker!();\n")
	}
	if extra.Len() == 0 {
		return source
	}
	return source + "\n" + extra.String()
}
