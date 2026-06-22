# Go → guest callbacks + auto-marshal (implementation plan)

Goal: let a `.poly` Go function invoke a guest (Python/JS/JVM/Ruby) callback —
`func go_invoke(cb, n) { return cb(n) }` — and have it work both when called on
the Golden Thread (a1) and when invoked from a spawned goroutine (a2, via
auto-marshal), without deadlock (time-free pumping-wait + wait-for-graph cycle
detection).

## Failing tests (committed, currently RED)

- `polyscript/examples/go-guest-callback-direct.poly` (a1): direct call. Today:
  `eval go: unknown function "go_invoke"` (the c-shared plugin fails to compile
  because `cb(n)` is `interface{}(...)`). Target: `result=42`.
- `polyscript/examples/auto-marshal-callback-proof.poly` (a2): `go go_invoke(cb,21)`
  + `wait`. Needs auto-marshal once a1 works. Target: `result=42`.

## The core obstacle (verified)

In libomnivm (`UseGoSourceFallback=true`) Go funcs compile as **c-shared plugins**
(separate Go runtime, loaded via dlopen; args cross **serialized**, results
decoded — `pkg/manifest/plugin_cshared.go`). c-shared plugins today are
**one-directional**: the host calls in (`callCSharedGoPlugin` + encoded args, plus
`Init(deps)` injection); the plugin has **no channel to call back into the host**.
A callable therefore cannot cross as a Go closure — it must be invoked by asking
the host to run `handle_call` on the callable's handle id. So Go→guest callbacks
need a **net-new bidirectional bridge ABI** for c-shared plugins.

(Host-side primitive already exists: bridge op `handle_call` with `key==""`
invokes the callable — `pkg/manifest/stubs.go:1145`, via `e.HandleCall`. The
callable arrives at `normalizeGoArg` as a `*GoHandleProxy{kind:"callable", id:N}`
— `pkg/manifest/go_proxy.go:888`, `captures.go:4883` — so `N` is the key.)

## Implementation slices (each independently verifiable; all additive)

### a1-i — install a host bridge into the c-shared plugin (cgo ABI)
- `goCSharedWrapperSource` (`plugins.go:444`): add a stored host bridge
  C-function-pointer var, an `//export OmniSetBridge` that records it, and a Go
  helper `__omnivm_bridge_call(reqJSON string) (string)` that calls the pointer
  and returns the host's reply.
- Host: after `openCSharedGoPlugin` (`plugins.go:148`), `Lookup("OmniSetBridge")`
  and install a C trampoline whose Go target forwards to `e.HandleCall` for the
  `"__manifest"` pseudo-runtime. (Mirror the in-process `bridgeShimSource` /
  `golang.go:170` pattern, but over the C ABI.)
- Verify: existing c-shared go examples still compile/run (bridge unused).

### a1-ii — encode callable args with their handle id
- `encodeCSharedGoArgs` (`plugin_cshared.go:463`): encode a `*GoHandleProxy` with
  `kind=="callable"` as a descriptor carrying `{__omnivm_callable_handle__: N}`.
- Plugin wrapper: decode that descriptor into an opaque `__omnivmCallable{id:N}`.

### a1-iii — lower `cb(args)` in the compiler
- `manifest-generator.ts` `goExprToCode` `Call` case (~line 4904): when the
  callee is an `Identifier` in `params`, emit `__omnivm_invoke(cb, args...)`
  instead of `cb(args)`.
- Emit `func __omnivm_invoke(fn interface{}, args ...interface{}) interface{}` in
  the `package main` wrapper: build `{"op":"handle_call","id":fn.id,"key":"",
  "args":[...]}`, call `__omnivm_bridge_call`, decode the envelope's value.
- Traps: `manifest_test.go` golden-substring tests pin generated indentation;
  `stripGeneratedGoInit` (`plugins.go:353`) — keep new preamble outside the Init
  block or strip in parallel.
- GREEN gate: `go-guest-callback-direct.poly` → `result=42`.

### a2 — auto-marshal for the goroutine case
- When `__omnivm_bridge_call` runs on a goroutine thread (spawned via `go`), the
  host trampoline → `e.HandleCall` → `runtimeRefEvalExpr` would `rt.Eval` off the
  Golden Thread. Intercept in the host: if not on the Golden Thread, marshal the
  guest eval via `eng.Disp.RunOnMain` (reuse `submitContext.hostRun` shape).
  Detect "on Golden Thread" via injected `currentThreadID()` vs `GoldenThreadID`.
- Pumping-wait: `waitSpawnValue` (`channels.go:425`) must pump the dispatcher
  while blocked (`dispatcher.PumpUntil(done)`) so the marshaled callback is
  serviced — already proven viable by the cooperative join (task b).
- GREEN gate: `auto-marshal-callback-proof.poly` → `result=42`.

### a3 — wait-for-graph cycle detection (time-free deadlock diagnostic)
- Register edges: Golden-Thread pumping-wait → goroutine; marshaled callback →
  Golden Thread. Detect a cycle in O(edges) when it closes; raise a structured
  error. No timeout. Out-of-model (opaque guest) blocking is left to hang (the
  author's own deadlock), as designed.
- Proof: a deliberately cyclic in-model `.poly` flagged instantly.

## Outcome (implemented)

- **a1 (done):** `cb(args)` on a callable param lowers to `__omnivm_invoke` →
  host bridge `handle_call`. New bidirectional c-shared bridge ABI (OmniSetBridge
  installs the host OmniCall pointer). Works on the Golden Thread.
- **a2 (done):** a bridge call from a foreign thread (spawned goroutine) is
  auto-marshaled onto the Golden Thread (`callRuntime` → `Disp.RunOnMain`) and
  serviced by a Golden-Thread **pumping-wait** (`dispatcher.PumpUntil`, no
  timeout). Reentrancy keyed on the real Golden-Thread id, so nested/marshaled
  guest calls run inline (fixes the cooperative-mode double-marshal). Works in
  blocking **and** gevent/asyncio cooperative modes.
- **a3 (done, by prevention not detection):** the pumping-wait breaks the
  hold-and-wait condition, so in-model deadlocks **cannot form** — no wait-for
  graph and no timeout are needed. A detector would be dead code for in-model
  cases and can't observe out-of-model (opaque guest) blocking, which is the
  author's own deadlock (hangs like any). Validated by the edge suite
  (`scripts/test-go-callbacks.sh`, `make test-go-callbacks`): direct, auto-marshal,
  Python + JS callbacks, nested cross-runtime (goroutine→JS→Python=105),
  concurrent goroutines, error propagation, and fire-and-forget (immediate +
  slow) all pass without hang. Leaked fire-and-forget goroutines get a clean
  dispatcher-shutdown error rather than blocking.

## Scope note

a1 is a net-new bidirectional cgo bridge ABI for c-shared plugins (host C
trampoline + plugin C fn pointer + handle-id encoding + TS codegen). It is
larger than a typical change and touches three languages (TS, Go, C). It is
additive, so it can land slice-by-slice with the existing go examples as
regression at each step. a2/a3 reuse the cooperative machinery already shipped.
