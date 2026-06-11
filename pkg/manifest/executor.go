package manifest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/omnivm/omnivm/pkg"
	"github.com/omnivm/omnivm/pkg/arrow"
	"github.com/omnivm/omnivm/pkg/handles"
	"github.com/omnivm/omnivm/pkg/polyglot"
)

// ErrReturn is a sentinel used to unwind the call stack on return ops.
type ErrReturn struct{ Value interface{} }

func (e ErrReturn) Error() string { return "return" }

// ErrBreak and ErrContinue are sentinels used to unwind manifest loop bodies.
type ErrBreak struct{}
type ErrContinue struct{}

func (e ErrBreak) Error() string    { return "break" }
func (e ErrContinue) Error() string { return "continue" }

// ErrThrow is a sentinel used for manifest-level throw ops.
type ErrThrow struct{ Value interface{} }

func (e ErrThrow) Error() string { return fmt.Sprintf("throw: %v", e.Value) }

func isManifestControlFlow(err error) bool {
	if err == nil {
		return false
	}
	switch err.(type) {
	case ErrReturn, ErrBreak, ErrContinue:
		return true
	default:
		return false
	}
}

// ImportRef marks a binding as a module import in a specific runtime.
// Captures for the same runtime skip JSON injection since the module
// is already in scope. Cross-runtime captures skip module refs entirely.
type ImportRef struct {
	Runtime string // which runtime owns this import
	Name    string // module name from the source runtime
}

// RuntimeRef marks a binding as a variable that lives in a specific runtime's
// global scope. Captures for the same runtime skip injection since the value is
// already in scope. Cross-runtime captures are resolved by bridge/boundary
// metadata, with JSON used only as the compatibility fallback.
type RuntimeRef struct {
	Runtime       string      // which runtime owns this variable
	VarName       string      // variable name in that runtime
	TypeName      string      // optional runtime-native type name for persistent local aliases
	Value         interface{} // last known primitive value, when SnapshotKnown and not Opaque
	SnapshotKnown bool        // true when bind-time probing classified the live value
	Opaque        bool        // true when the live value is complex and must stay behind a handle
	CallableKnown bool        // true when the source runtime classified callability
	Callable      bool        // true when the source runtime value can be called directly
	CallableShape *CallableShape
}

type bridgeIdentity struct {
	typ string
	ptr uintptr
	key string
}

// ResourceRef is an opaque runtime-owned handle. Other runtimes may receive a
// proxy descriptor, but the live object itself is not serialized.
type ResourceRef struct {
	ID       handles.ID  `json:"id"`
	Runtime  string      `json:"runtime"`
	Kind     string      `json:"kind"`
	Disposer string      `json:"disposer,omitempty"`
	Value    interface{} `json:"value,omitempty"`
	Closed   bool        `json:"closed"`
}

// TableRef is a zero-copy-oriented table/buffer handle. The table data remains
// owned by the source runtime or Arrow memory producer; captures receive a
// descriptor, not JSON rows.
type TableRef struct {
	ID        handles.ID     `json:"id"`
	Runtime   string         `json:"runtime"`
	Format    string         `json:"format"`
	Ownership string         `json:"ownership"`
	Release   string         `json:"release,omitempty"`
	Metadata  *TableMetadata `json:"metadata,omitempty"`
	Value     interface{}    `json:"value,omitempty"`
	Released  bool           `json:"released"`
}

// SpawnHandle is a manifest-visible handle returned by a spawn op.
// Closing done synchronizes result visibility for wait(handle).
type SpawnHandle struct {
	ID     int
	done   chan struct{}
	result interface{}
	err    error
}

// JobHandle models delayed/background work. It is a manifest-visible handle:
// payload/result values may cross, while scheduler internals stay in OmniVM.
type JobHandle struct {
	ID           int         `json:"id"`
	Runtime      string      `json:"runtime"`
	Kind         string      `json:"kind"`
	Payload      interface{} `json:"payload,omitempty"`
	Result       interface{} `json:"result,omitempty"`
	Done         bool        `json:"done"`
	Cancelled    bool        `json:"cancelled"`
	CancelReason interface{} `json:"cancelReason,omitempty"`
}

// BoundaryStats is a diagnostics snapshot for cross-runtime value movement.
type BoundaryStats struct {
	CaptureInjections       int64  `json:"capture_injections"`
	RuntimeSerializations   int64  `json:"runtime_serializations"`
	JSONFallbacks           int64  `json:"json_fallbacks"`
	LastJSONFallbackReason  string `json:"last_json_fallback_reason"`
	ArrowTransfers          int64  `json:"arrow_transfers"`
	BridgeTransforms        int64  `json:"bridge_transforms"`
	BoundaryWarnings        int64  `json:"boundary_warnings"`
	ProxyCaptures           int64  `json:"proxy_captures"`
	ProxyMaterializations   int64  `json:"proxy_materializations"`
	ChannelMaterializations int64  `json:"channel_materializations"`
	StreamProxyCaptures     int64  `json:"stream_proxy_captures"`
	ResourceProxyCaptures   int64  `json:"resource_proxy_captures"`
	TableProxyCaptures      int64  `json:"table_proxy_captures"`
	JobProxyCaptures        int64  `json:"job_proxy_captures"`
}

// FuncDef stores a manifest-level function definition.
type FuncDef struct {
	Name          string
	Params        []*Param
	Body          []*Op
	Generator     bool
	NativeRuntime string
}

// Executor runs manifest ops against a set of runtimes.
type Executor struct {
	runtimes          map[string]pkg.Runtime
	defaultRuntime    string
	scopes            []map[string]interface{}
	handleTable       *handles.Table
	handleScopes      []handles.ScopeID
	funcs             map[string]*FuncDef
	goFuncs           map[string]interface{}
	goSourceFuncs     map[string]*goSourceFuncDef
	rustFuncs         map[string]*rustFuncMeta
	bindingOrigins    map[string]string // binding name -> runtime whose global is the source of truth
	javaStubFuncs     map[string]*FuncDef
	channels          map[string]*ChanRef
	channelsMu        sync.RWMutex
	spawns            []*SpawnHandle
	spawnsMu          sync.Mutex
	nextSpawnID       int
	nextRuntimeRefID  int
	resources         map[handles.ID]*ResourceRef
	releasedResources map[handles.ID]*ResourceRef
	tables            map[handles.ID]*TableRef
	releasedTables    map[handles.ID]*TableRef
	releasedStreams   map[handles.ID]releasedStreamRef
	bridgeHandles     map[bridgeIdentity]handles.ID
	jobs              map[int]*JobHandle
	nextJobID         int
	yieldCollectors   [][]interface{}        // stack of yield collectors for nested generators
	bridgeOps         map[string][]*BridgeOp // key: "binding|from|to" → bridge ops
	boundaryWarnings  map[string]struct{}
	boundaryStats     BoundaryStats
	boundaryStatsMu   sync.Mutex
	spawnWG           sync.WaitGroup
	awaitFromDepth    int
	// streamNextServices counts serviced stream_next bridge ops (each is one
	// bridge hop); batched pulls (max_n) keep this far below the value count.
	streamNextServices atomic.Int64
}

var pythonDirectCallExprRe = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// NewExecutor creates an Executor with the given runtimes.
func NewExecutor(runtimes map[string]pkg.Runtime) *Executor {
	return NewExecutorWithHandles(runtimes, handles.NewTable())
}

// NewExecutorWithHandles creates an Executor that records runtime-owned values
// in the supplied process-local handle table.
func NewExecutorWithHandles(runtimes map[string]pkg.Runtime, table *handles.Table) *Executor {
	if table == nil {
		table = handles.NewTable()
	}
	e := &Executor{
		runtimes:          runtimes,
		scopes:            []map[string]interface{}{make(map[string]interface{})},
		handleTable:       table,
		handleScopes:      []handles.ScopeID{table.NewScope()},
		funcs:             make(map[string]*FuncDef),
		goFuncs:           make(map[string]interface{}),
		goSourceFuncs:     make(map[string]*goSourceFuncDef),
		rustFuncs:         make(map[string]*rustFuncMeta),
		bindingOrigins:    make(map[string]string),
		javaStubFuncs:     make(map[string]*FuncDef),
		channels:          make(map[string]*ChanRef),
		resources:         make(map[handles.ID]*ResourceRef),
		releasedResources: make(map[handles.ID]*ResourceRef),
		tables:            make(map[handles.ID]*TableRef),
		releasedTables:    make(map[handles.ID]*TableRef),
		releasedStreams:   make(map[handles.ID]releasedStreamRef),
		bridgeHandles:     make(map[bridgeIdentity]handles.ID),
		jobs:              make(map[int]*JobHandle),
	}
	e.registerChannelBuiltins()
	return e
}

// setupRuntimeBuiltins injects mock built-in functions into runtimes.
// The manifest executor has no HTTP server or real async I/O, so functions
// like fetch, loadConfig, etc. are stubbed to return mock values.
func (e *Executor) setupRuntimeBuiltins() {
	if jsRT, ok := e.runtimes["javascript"]; ok {
		jsRT.Execute(`
globalThis.fetch = function(url) {
  return Promise.resolve({
    ok: true, status: 200, statusText: 'OK',
    json: function() { return Promise.resolve({data: 'mock', url: String(url)}); },
    text: function() { return Promise.resolve('mock response'); }
  });
};
['loadConfig','authenticate','taskA','taskB','coro1','coro2',
 'loadPreferences','getNotifications','loadMeta','openResource',
 'cleanup','riskyOperation','processItem','transform','validate',
 'sleep','saveToDb','notifyUsers','startProducer','fetchPage'
].forEach(function(n) {
  globalThis[n] = function() { return 'mock_' + n; };
});
globalThis.token = 'mock_token';
if (typeof globalThis.print === 'undefined') {
  globalThis.print = function() { console.log.apply(console, arguments); };
}
if (typeof globalThis.os === 'undefined') {
  globalThis.os = {path: {getsize: function(p) { return 0; }}};
}
`)
	}

	// Go mock functions for manifest simulation
	if _, exists := e.goFuncs["getResult"]; !exists {
		e.goFuncs["getResult"] = func(arg interface{}) interface{} {
			return "mock_result"
		}
	}
}

// Execute runs all top-level ops in the manifest sequentially.
func (e *Executor) Execute(m *Manifest) (err error) {
	defer func() {
		if cleanupErr := e.releaseAllHandleScopes(); cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()

	e.defaultRuntime = m.DefaultRuntime
	e.bridgeOps = buildBridgeIndex(m.Bridges)

	if m.TypeSummary != nil && m.TypeSummary.Errors > 0 {
		fmt.Fprintf(os.Stderr, "warning: manifest has %d type errors that will fail at runtime\n", m.TypeSummary.Errors)
	}

	e.setupRuntimeBuiltins()
	_, err = e.executeOps(m.Ops)
	if _, ok := err.(ErrReturn); ok {
		return nil
	}
	return err
}

// executeOps runs a list of ops, catching ErrReturn.
func (e *Executor) executeOps(ops []*Op) (interface{}, error) {
	var lastVal interface{}
	for _, op := range ops {
		val, err := e.executeOp(op)
		if err != nil {
			return nil, err
		}
		if err := e.drainPostOpDeferredWork(); err != nil {
			return nil, err
		}
		lastVal = val
	}
	return lastVal, nil
}

func (e *Executor) drainPostOpDeferredWork() error {
	for _, rt := range e.runtimes {
		rt.Pump()
	}
	arrow.GlobalStore().DrainDeferred()
	_ = e.ensureHandleTable().DrainFinalizerReleases(0)
	return nil
}

// executeOp dispatches a single op by type.
func (e *Executor) executeOp(op *Op) (interface{}, error) {
	switch op.OpType {
	case "exec":
		return e.opExec(op)
	case "eval":
		return e.opEval(op)
	case "native":
		if strings.TrimSpace(op.Code) == "break" {
			return nil, ErrBreak{}
		}
		if strings.TrimSpace(op.Code) == "continue" {
			return nil, ErrContinue{}
		}
		return e.opExec(op) // same as exec
	case "import":
		return e.opImport(op)
	case "func_def":
		return e.opFuncDef(op)
	case "return":
		return e.opReturn(op)
	case "if":
		return e.opIf(op)
	case "loop":
		return e.opLoop(op)
	case "concat":
		return e.opConcat(op)
	case "declare":
		return e.opDeclare(op)
	case "assign":
		return e.opAssign(op)
	case "try":
		return e.opTry(op)
	case "throw":
		return e.opThrow(op)
	case "parallel":
		return e.opParallel(op)
	case "chan":
		return e.opChan(op)
	case "select":
		return e.opSelect(op)
	case "spawn":
		return e.opSpawn(op)
	case "yield":
		return e.opYield(op)
	case "await":
		return e.opAwait(op)
	case "resource":
		return e.opResource(op)
	case "table":
		return e.opTable(op)
	case "job":
		return e.opJob(op)
	case "exec_compiled":
		return e.opExecCompiled(op)
	case "eval_compiled":
		return e.opEvalCompiled(op)
	case "call_typed":
		return e.opCallTyped(op)
	default:
		return nil, fmt.Errorf("unknown op type: %q", op.OpType)
	}
}

// resolveRuntime returns the runtime for an op, falling back to defaultRuntime.
func (e *Executor) resolveRuntime(op *Op) (pkg.Runtime, error) {
	name := op.Runtime
	if name == "" {
		name = e.defaultRuntime
	}
	rt, ok := e.runtimes[name]
	if !ok {
		return nil, fmt.Errorf("unknown runtime: %q", name)
	}
	return rt, nil
}

func (e *Executor) runJavaCaptureCleanup(rt pkg.Runtime, injection captureInjection, excludeNames ...string) error {
	cleanupCode := injection.javaCleanupCode(excludeNames...)
	if cleanupCode == "" {
		return nil
	}
	result := rt.Execute(cleanupCode)
	if result.Err != nil {
		return result.Err
	}
	return nil
}

// opExec handles exec and native ops.
func (e *Executor) opExec(op *Op) (out interface{}, err error) {
	if op.Async {
		return e.execAsync(op)
	}

	// runtime:"rust" — same registered-call dispatch as eval, result discarded
	if op.Runtime == "rust" && op.Code != "" {
		return e.evalRustCode(op)
	}

	rt, err := e.resolveRuntime(op)
	if err != nil {
		return nil, err
	}

	// Auto-inject current scope bindings so exec code can
	// reference manifest variables without explicit captures.
	autoInjection := e.autoInjectScopePlanExcluding(rt.Name(), captureBindingExclusions(op.Captures))
	if autoInjection.setup != "" {
		injectResult := rt.Execute(autoInjection.setup)
		if injectResult.Err != nil {
			return nil, fmt.Errorf("exec auto-inject [%s]: %w", rt.Name(), injectResult.Err)
		}
		defer func() {
			if cleanupErr := e.runJavaCaptureCleanup(rt, autoInjection); cleanupErr != nil {
				if err != nil {
					err = fmt.Errorf("%w (auto capture cleanup failed: %v)", err, cleanupErr)
					return
				}
				err = fmt.Errorf("exec auto capture cleanup [%s]: %w", rt.Name(), cleanupErr)
			}
		}()
	}

	code := op.Code
	if len(op.Captures) > 0 {
		code, err = e.wrapWithCaptures(rt.Name(), op.Code, op.Captures)
		if err != nil {
			return nil, fmt.Errorf("exec captures: %w", err)
		}
	} else if rt.Name() == "ruby" {
		// Ruby snippets execute across eval/exec boundaries, so persisted
		// $globals need local aliases before user code runs.
		prefix := e.rubyAliasPrefix(nil)
		if prefix != "" {
			code = prefix + code
		}
	} else if rt.Name() == "java" {
		code = e.javaPersistentAliasPrefix(nil) + code
	}

	// Convert Python f-strings to JS template literals when targeting JavaScript
	if rt.Name() == "javascript" {
		code = convertFStringToTemplateLiteral(code)
		if len(op.Captures) == 0 && javascriptExecNeedsLexicalIsolation(code) {
			code = wrapJavaScriptExecLexicalScope(code)
		}
	}

	result := rt.Execute(code)
	if result.Err != nil {
		return nil, fmt.Errorf("exec [%s]: %w", rt.Name(), result.Err)
	}

	// Print captured stdout to real stdout (exec ops are side-effectful)
	if result.Output != "" {
		fmt.Fprint(os.Stdout, result.Output)
	}

	output := strings.TrimRight(result.Output, "\n")
	if op.Bind != "" {
		e.setBinding(op.Bind, output)
	}
	return output, nil
}

func javascriptExecNeedsLexicalIsolation(code string) bool {
	return regexp.MustCompile(`\b(?:const|let)\s+[A-Za-z_$]`).FindStringIndex(code) != nil
}

func wrapJavaScriptExecLexicalScope(code string) string {
	return "(function(){\n" + code + "\n})();"
}

// opEval handles eval ops.
func (e *Executor) opEval(op *Op) (out interface{}, err error) {
	// Go function call via goFuncs registry
	if op.Func != "" && (op.Runtime == "go" || op.Runtime == "") {
		args := make([]interface{}, 0, len(op.Args))
		for _, raw := range op.Args {
			args = append(args, e.resolveGoCallArg(raw))
		}
		return e.callGoFunc(op.Func, args, op.Bind)
	}

	// runtime:"go" with code — parse as function call expression
	if op.Runtime == "go" && op.Code != "" {
		return e.evalGoCode(op)
	}

	// runtime:"rust" — dispatch to unit exports registered by rust func_defs
	if op.Runtime == "rust" && op.Code != "" {
		return e.evalRustCode(op)
	}

	if op.Async {
		return e.evalAsync(op)
	}

	rt, err := e.resolveRuntime(op)
	if err != nil {
		return nil, err
	}

	// Auto-inject current scope bindings so eval code can
	// reference manifest variables without explicit captures.
	autoInjection := e.autoInjectScopePlanExcluding(rt.Name(), captureBindingExclusions(op.Captures))
	if autoInjection.setup != "" {
		injectResult := rt.Execute(autoInjection.setup)
		if injectResult.Err != nil {
			return nil, fmt.Errorf("eval auto-inject [%s]: %w", rt.Name(), injectResult.Err)
		}
		defer func() {
			if cleanupErr := e.runJavaCaptureCleanup(rt, autoInjection, op.Bind); cleanupErr != nil {
				if err != nil {
					err = fmt.Errorf("%w (auto capture cleanup failed: %v)", err, cleanupErr)
					return
				}
				err = fmt.Errorf("eval auto capture cleanup [%s]: %w", rt.Name(), cleanupErr)
			}
		}()
	}

	// Inject explicit captures (overrides auto-injected values)
	var explicitInjection captureInjection
	if len(op.Captures) > 0 {
		explicitInjection = e.buildCaptureInjectionPlan(rt.Name(), op.Captures)
		if explicitInjection.err != nil {
			return nil, fmt.Errorf("eval captures [%s]: %w", rt.Name(), explicitInjection.err)
		}
		if explicitInjection.setup != "" {
			injectResult := rt.Execute(explicitInjection.setup)
			if injectResult.Err != nil {
				return nil, fmt.Errorf("eval captures [%s]: %w", rt.Name(), injectResult.Err)
			}
			defer func() {
				if cleanupErr := e.runJavaCaptureCleanup(rt, explicitInjection, op.Bind); cleanupErr != nil {
					if err != nil {
						err = fmt.Errorf("%w (capture cleanup failed: %v)", err, cleanupErr)
						return
					}
					err = fmt.Errorf("eval capture cleanup [%s]: %w", rt.Name(), cleanupErr)
				}
			}()
		}
	}

	code := op.Code
	if rt.Name() == "python" {
		code = e.rewritePythonDirectAsyncSourceCall(code)
	}

	// Ruby: captures and prior same-runtime values are stored as $globals.
	// Create local aliases so user code can reference variables normally.
	if rt.Name() == "ruby" {
		prefix := e.rubyAliasPrefix(op.Captures)
		if prefix != "" {
			if op.Bind != "" {
				// Ruby locals don't persist across Execute/Eval boundaries.
				// Combine aliases + assignment into a single Execute() call.
				aliasCode := strings.TrimSuffix(prefix, "; ")
				e.bindingOrigins[op.Bind] = rt.Name()
				assignCode := runtimeAssign(rt.Name(), op.Bind, code)
				combinedCode := aliasCode + "\n" + assignCode
				execResult := rt.Execute(combinedCode)
				if execResult.Err != nil {
					return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), execResult.Err)
				}
				ref, val, snapshotErr := e.boundRuntimeRefSnapshot(rt.Name(), op.Bind)
				if snapshotErr != nil {
					return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), snapshotErr)
				}
				e.setBinding(op.Bind, ref)
				return val, nil
			}
			code = prefix + code
		}
	}

	// If bind is set, persist the result in the runtime's global scope
	// so subsequent ops in the same runtime can reference it directly.
	if op.Bind != "" {
		if rt.Name() == "java" {
			imports, expr := splitJavaImports(code)
			javaCaptureNames := append([]string{}, autoInjection.javaCaptureNames...)
			javaCaptureNames = append(javaCaptureNames, explicitInjection.javaCaptureNames...)
			expr = lowerJavaCapturedMemberAccess(expr, javaCaptureNames)
			expr = normalizeJavaEvalExpression(expr)
			e.bindingOrigins[op.Bind] = rt.Name()
			assignLines := append(imports, javaCaptureAliasCode(javaCaptureNames), e.javaPersistentAliasPrefix(nil), runtimeAssign(rt.Name(), op.Bind, expr))
			assignCode := strings.Join(assignLines, "\n")
			execResult := rt.Execute(assignCode)
			if execResult.Err != nil {
				return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), execResult.Err)
			}
			ref, val, snapshotErr := e.boundRuntimeRefSnapshot(rt.Name(), op.Bind)
			if snapshotErr != nil {
				return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), snapshotErr)
			}
			if ref.TypeName == "" {
				ref.TypeName = inferJavaBindType(expr)
			}
			if ref.TypeName == "" && ref.SnapshotKnown && !ref.Opaque {
				ref.TypeName = javaPrimitiveTypeName(ref.Value)
			}
			e.setBinding(op.Bind, ref)
			return val, nil
		}

		e.bindingOrigins[op.Bind] = rt.Name()
		assignCode := runtimeAssign(rt.Name(), op.Bind, code)
		execResult := rt.Execute(assignCode)
		if execResult.Err != nil {
			return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), execResult.Err)
		}

		ref, val, snapshotErr := e.boundRuntimeRefSnapshot(rt.Name(), op.Bind)
		if snapshotErr != nil {
			return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), snapshotErr)
		}
		e.setBinding(op.Bind, ref)
		return val, nil
	}

	result := rt.Eval(runtimeSerializeExpr(rt.Name(), code))
	if result.Err != nil {
		return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), result.Err)
	}

	val, decodeErr := decodeRuntimeValue(rt.Name(), result)
	if decodeErr != nil {
		return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), decodeErr)
	}

	return val, nil
}

// runtimeAssign generates code to assign a value to a global variable.
func runtimeAssign(rtName, varName, expr string) string {
	switch rtName {
	case "javascript":
		return fmt.Sprintf("globalThis[%s] = %s;", strconv.Quote(varName), expr)
	case "python":
		if !isPythonIdentifier(varName) || isPythonReservedWord(varName) {
			return fmt.Sprintf("globals()[%s] = %s", strconv.Quote(varName), expr)
		}
		return fmt.Sprintf("%s = %s", varName, expr)
	case "ruby":
		if !isRubyIdentifier(varName) || rubyReserved[varName] {
			return fmt.Sprintf("($omnivm_bindings ||= {})[%s] = (begin; %s; end)", strconv.Quote(varName), expr)
		}
		return fmt.Sprintf("$%s = (begin; %s; end)", varName, expr)
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.setCaptureObject(\"%s\", %s);", escapeJavaString(varName), expr)
	default:
		return fmt.Sprintf("%s = %s", varName, expr)
	}
}

// runtimeSerializeExpr wraps an eval expression so structured values cross the
// manifest bridge as JSON. This keeps JSON encoding out of user-level .poly.
func runtimeSerializeExpr(rtName, expr string) string {
	switch rtName {
	case "javascript":
		return fmt.Sprintf(`(function(){ var __v = (%s); var __s = JSON.stringify(__v); return typeof __s === "undefined" ? JSON.stringify(String(__v)) : __s; })()`, expr)
	case "python":
		return fmt.Sprintf(`__import__("json").dumps((%s), default=lambda __o: __o.to_json() if hasattr(__o, "to_json") else str(__o))`, expr)
	case "ruby":
		return fmt.Sprintf(`begin; require 'json'; JSON.generate(begin; %s; end); rescue; JSON.generate((begin; %s; end).to_s); end`, expr, expr)
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.toJson(%s)", expr)
	default:
		return expr
	}
}

type runtimePrimitiveSnapshot struct {
	Primitive     bool           `json:"primitive"`
	Value         interface{}    `json:"value"`
	TypeName      string         `json:"typeName,omitempty"`
	Callable      bool           `json:"callable,omitempty"`
	CallableShape *CallableShape `json:"callableShape,omitempty"`
}

func (e *Executor) boundRuntimeRefSnapshot(rtName, varName string) (RuntimeRef, interface{}, error) {
	ref := RuntimeRef{Runtime: rtName, VarName: varName}
	rt, ok := e.runtimes[rtName]
	if !ok {
		return ref, ref, fmt.Errorf("runtime %q not found", rtName)
	}
	result := rt.Eval(runtimePrimitiveSnapshotExpr(rtName, runtimeVarRef(rtName, varName)))
	if result.Err != nil {
		return ref, ref, result.Err
	}
	snapshot, ok, err := decodeRuntimePrimitiveSnapshot(rtName, result)
	if err != nil {
		return ref, ref, err
	}
	if !ok {
		return ref, ref, nil
	}
	ref.SnapshotKnown = true
	if snapshot.Primitive {
		ref.Value = snapshot.Value
		return ref, snapshot.Value, nil
	}
	ref.Opaque = true
	ref.TypeName = snapshot.TypeName
	ref.CallableKnown = true
	ref.Callable = snapshot.Callable
	ref.CallableShape = snapshot.CallableShape
	return ref, ref, nil
}

func runtimePrimitiveSnapshotExpr(rtName, expr string) string {
	switch rtName {
	case "javascript":
		return fmt.Sprintf(`(function(){ var __v = (%s); var __p = (__v === null || typeof __v === "boolean" || typeof __v === "number" || typeof __v === "string"); if (__p) return JSON.stringify({primitive:true,value:__v}); var __shape = null; if (typeof __v === "function") { __shape = __v.__omnivm_callable_shape__ || {arity: __v.length}; try { var __src = Function.prototype.toString.call(__v); var __m = __src.match(/^[^(]*\(\s*\{([^}]*)\}/) || __src.match(/^\s*\(?\s*\{([^}]*)\}/); if (__m) { var __keys = __m[1].split(",").map(function(s){ return s.split(":")[0].split("=")[0].trim(); }).filter(Boolean); __shape.acceptsOptionsObject = true; if (__keys.length) __shape.destructuredKeys = __keys; } } catch (_e) {} } return JSON.stringify({primitive:false,callable:typeof __v === "function",callableShape:__shape}); })()`, expr)
	case "python":
		return fmt.Sprintf(`(lambda __v: (lambda __json, __inspect: __json.dumps({"primitive": True, "value": __v} if isinstance(__v, (type(None), bool, int, float, str)) else (lambda __shape: {"primitive": False, "callable": callable(__v), "callableShape": __shape})(None if not callable(__v) else (lambda __sig: {"acceptsKwargs": any(__p.kind == __inspect.Parameter.VAR_KEYWORD for __p in __sig.parameters.values()), "parameterNames": [__n for __n, __p in __sig.parameters.items() if __p.kind in (__inspect.Parameter.POSITIONAL_ONLY, __inspect.Parameter.POSITIONAL_OR_KEYWORD, __inspect.Parameter.KEYWORD_ONLY)], "arity": sum(1 for __p in __sig.parameters.values() if __p.default is __inspect.Parameter.empty and __p.kind in (__inspect.Parameter.POSITIONAL_ONLY, __inspect.Parameter.POSITIONAL_OR_KEYWORD))})(__inspect.signature(__v)))))(__import__("json"), __import__("inspect")))(%s)`, expr)
	case "ruby":
		return fmt.Sprintf(`begin; require 'json'; __v = (begin; %s; end); __text = false; __text_value = __v; if __v.is_a?(String); __text_value = __v.dup; if __text_value.encoding == Encoding::ASCII_8BIT; __text_value.force_encoding(Encoding::UTF_8); elsif __text_value.encoding != Encoding::UTF_8; begin; __text_value = __text_value.encode(Encoding::UTF_8); rescue; end; end; __text = __text_value.encoding.ascii_compatible? && __text_value.valid_encoding?; end; if __v.nil? || __v == true || __v == false || __v.is_a?(Numeric) || __text; JSON.generate({primitive: true, value: __text ? __text_value : __v}); else; __params = (__v.respond_to?(:parameters) ? __v.parameters : []); __shape = (__v.respond_to?(:call) ? {acceptsKwargs: __params.any? { |p| p[0] == :keyrest }, parameterNames: __params.map { |p| p[1] }.compact.map(&:to_s), arity: (__v.respond_to?(:arity) ? __v.arity : nil)} : nil); JSON.generate({primitive: false, callable: __v.respond_to?(:call), callableShape: __shape}); end; end`, expr)
	case "java":
		return fmt.Sprintf("omnivm.OmniVM.primitiveSnapshot(%s)", expr)
	default:
		return expr
	}
}

func decodeRuntimePrimitiveSnapshot(rtName string, result pkg.Result) (runtimePrimitiveSnapshot, bool, error) {
	if result.Err != nil {
		return runtimePrimitiveSnapshot{}, false, result.Err
	}
	raw := result.Output
	if result.Value != nil {
		raw = fmt.Sprintf("%v", result.Value)
	}
	var snapshot runtimePrimitiveSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err == nil {
		return snapshot, true, nil
	}
	value, err := decodeRuntimeValue(rtName, result)
	if err != nil {
		return runtimePrimitiveSnapshot{}, false, err
	}
	if isBridgePrimitive(value) {
		return runtimePrimitiveSnapshot{Primitive: true, Value: value}, true, nil
	}
	return runtimePrimitiveSnapshot{Primitive: false}, true, nil
}

func decodeRuntimeValue(rtName string, result pkg.Result) (interface{}, error) {
	if result.Err != nil {
		return nil, result.Err
	}
	raw := result.Output
	if result.Value != nil {
		raw = fmt.Sprintf("%v", result.Value)
	}
	if rtName == "javascript" || rtName == "python" || rtName == "ruby" || rtName == "java" {
		var val interface{}
		if err := json.Unmarshal([]byte(raw), &val); err == nil {
			return val, nil
		}
	}
	return raw, nil
}

// runtimeVarRef generates code to reference a variable in a runtime.
func runtimeVarRef(rtName, varName string) string {
	switch rtName {
	case "javascript":
		if strings.HasPrefix(varName, "globalThis.") {
			return varName
		}
		if strings.ContainsAny(varName, "[(") {
			return fmt.Sprintf("globalThis.%s", varName)
		}
		return fmt.Sprintf("globalThis[%s]", strconv.Quote(varName))
	case "python":
		if strings.ContainsAny(varName, "[(") {
			return varName
		}
		if !isPythonIdentifier(varName) || isPythonReservedWord(varName) {
			return fmt.Sprintf("globals()[%s]", strconv.Quote(varName))
		}
		return varName
	case "ruby":
		if strings.HasPrefix(varName, "$") {
			return varName
		}
		if strings.ContainsAny(varName, "[(") {
			return varName
		}
		if !isRubyIdentifier(varName) || rubyReserved[varName] {
			return fmt.Sprintf("($omnivm_bindings ||= {})[%s]", strconv.Quote(varName))
		}
		return fmt.Sprintf("$%s", varName)
	case "java":
		if key, ok := runtimeArgRefKey(varName); ok {
			return fmt.Sprintf("omnivm.OmniVM.getArgRef(\"%s\")", escapeJavaString(key))
		}
		return fmt.Sprintf("omnivm.OmniVM.getCapture(\"%s\")", escapeJavaString(varName))
	default:
		return varName
	}
}

// opImport generates runtime-specific import code and executes it.
// opCallTyped calls a function via the typed bridge.
// JSON: {"op":"call_typed", "runtime":"go", "func":"math.sqrt", "args":[25], "bind":"result"}
func (e *Executor) opCallTyped(op *Op) (interface{}, error) {
	if op.Func == "" {
		return nil, fmt.Errorf("call_typed: missing 'func' field")
	}
	if op.Runtime == "" {
		return nil, fmt.Errorf("call_typed: missing 'runtime' field")
	}

	// Convert JSON args to polyglot.Value
	goArgs := make([]polyglot.Value, len(op.Args))
	for i, arg := range op.Args {
		goArgs[i] = jsonToPolyglot(arg)
	}

	result := polyglot.GlobalRegistry.Call(op.Runtime, op.Func, goArgs)
	if result.IsError() {
		// Not in registry — try eval fallback through engine
		// Build code: funcName(arg1, arg2, ...)
		code := op.Func + "("
		for i, arg := range goArgs {
			if i > 0 {
				code += ", "
			}
			code += arg.ToGoString()
		}
		code += ")"

		rt, ok := e.runtimes[op.Runtime]
		if !ok {
			return nil, fmt.Errorf("call_typed: unknown runtime %q", op.Runtime)
		}

		// Use EvalTyped if available
		type typedEvaler interface {
			EvalTyped(code string) polyglot.Value
		}
		if te, ok := rt.(typedEvaler); ok {
			result = te.EvalTyped(code)
		} else {
			evalResult := rt.Eval(code)
			if evalResult.Err != nil {
				return nil, fmt.Errorf("call_typed [%s]: %w", op.Runtime, evalResult.Err)
			}
			s := ""
			if evalResult.Value != nil {
				s = fmt.Sprintf("%v", evalResult.Value)
			} else {
				s = evalResult.Output
			}
			result = polyglot.String(s)
		}
	}

	if result.IsError() {
		return nil, fmt.Errorf("call_typed [%s.%s]: %s", op.Runtime, op.Func, result.Str)
	}

	// Convert result to Go interface for manifest scope
	var goResult interface{}
	switch result.Tag {
	case polyglot.TagNull:
		goResult = nil
	case polyglot.TagBool:
		goResult = result.Int != 0
	case polyglot.TagI64:
		goResult = result.Int
	case polyglot.TagF64:
		goResult = result.Float
	case polyglot.TagString:
		goResult = result.Str
	default:
		goResult = result.Str
	}

	if op.Bind != "" {
		e.setBinding(op.Bind, goResult)
	}

	return goResult, nil
}

// jsonToPolyglot converts a JSON-decoded value to a polyglot.Value.
func jsonToPolyglot(v interface{}) polyglot.Value {
	switch val := v.(type) {
	case nil:
		return polyglot.Null()
	case bool:
		return polyglot.Bool(val)
	case float64:
		// JSON numbers are float64; use I64 for exact integers
		if val == float64(int64(val)) {
			return polyglot.I64(int64(val))
		}
		return polyglot.F64(val)
	case string:
		return polyglot.String(val)
	case json.Number:
		if i, err := val.Int64(); err == nil {
			return polyglot.I64(i)
		}
		if f, err := val.Float64(); err == nil {
			return polyglot.F64(f)
		}
		return polyglot.String(string(val))
	default:
		return polyglot.String(fmt.Sprintf("%v", v))
	}
}

func (e *Executor) opImport(op *Op) (interface{}, error) {
	if op.Runtime == "go" {
		// Go imports are compile-time dependencies of generated func_defs.
		ref := ImportRef{Runtime: "go", Name: op.Path}
		if op.Bind != "" {
			e.setBinding(op.Bind, ref)
		} else if op.DefaultImport != "" {
			e.setBinding(op.DefaultImport, ref)
		}
		for _, s := range op.Specifiers {
			e.setBinding(s.Local, ImportRef{Runtime: "go", Name: s.Imported})
		}
		return ref, nil
	}

	rt, err := e.resolveRuntime(op)
	if err != nil {
		return nil, err
	}

	var code string
	switch rt.Name() {
	case "python":
		if len(op.Specifiers) > 0 {
			var lines []string
			for i, s := range op.Specifiers {
				temp := fmt.Sprintf("__omnivm_import_%d", i)
				lines = append(lines, fmt.Sprintf("from %s import %s as %s", op.Path, s.Imported, temp))
				lines = append(lines, runtimeAssign("python", s.Local, temp))
			}
			code = strings.Join(lines, "\n")
		} else if op.DefaultImport != "" {
			code = fmt.Sprintf("import %s as __omnivm_import_default\n%s", op.Path, runtimeAssign("python", op.DefaultImport, "__omnivm_import_default"))
		} else if op.Bind != "" {
			code = fmt.Sprintf("import %s as __omnivm_import_bind\n%s", op.Path, runtimeAssign("python", op.Bind, "__omnivm_import_bind"))
		} else {
			code = fmt.Sprintf("import %s", op.Path)
		}
	case "javascript":
		pathLiteral := strconv.Quote(op.Path)
		if len(op.Specifiers) > 0 {
			lines := []string{fmt.Sprintf("var __omnivm_import = require(%s);", pathLiteral)}
			for _, s := range op.Specifiers {
				lines = append(lines, fmt.Sprintf("globalThis[%s] = __omnivm_import[%s];", strconv.Quote(s.Local), strconv.Quote(s.Imported)))
			}
			code = strings.Join(lines, "\n")
		} else if op.DefaultImport != "" {
			code = fmt.Sprintf("globalThis[%s] = require(%s);", strconv.Quote(op.DefaultImport), pathLiteral)
		} else if op.Bind != "" {
			code = fmt.Sprintf("globalThis[%s] = require(%s);", strconv.Quote(op.Bind), pathLiteral)
		} else {
			code = fmt.Sprintf("require(%s);", pathLiteral)
		}
	case "ruby":
		code = fmt.Sprintf(`require 'set'
begin
  require 'rubygems' unless defined?(Gem::Specification)
  Gem::Specification.each do |spec|
    lib = File.join(spec.full_gem_path, 'lib')
    $LOAD_PATH.unshift(lib) if File.directory?(lib) && !$LOAD_PATH.include?(lib)
  end
rescue Exception
end
require '%s'`, op.Path)
	case "java":
		// Java imports are handled at compile time; just record the binding
		ref := ImportRef{Runtime: rt.Name(), Name: op.Path}
		if op.Bind != "" {
			e.setBinding(op.Bind, ref)
		}
		return ref, nil
	default:
		return nil, fmt.Errorf("import not supported for runtime %q", rt.Name())
	}

	result := rt.Execute(code)
	if result.Err != nil {
		return nil, fmt.Errorf("import [%s] %q: %w", rt.Name(), op.Path, result.Err)
	}

	ref := ImportRef{Runtime: rt.Name(), Name: op.Path}
	if op.Bind != "" {
		e.setBinding(op.Bind, ref)
	} else if op.DefaultImport != "" {
		e.setBinding(op.DefaultImport, ref)
	}
	for _, s := range op.Specifiers {
		e.setBinding(s.Local, ImportRef{Runtime: rt.Name(), Name: s.Imported})
	}
	return ref, nil
}

// opFuncDef stores a function definition and registers stubs in each runtime.
func (e *Executor) opFuncDef(op *Op) (interface{}, error) {
	// Go plugin with source
	if op.BodyRuntime == "go" && op.Source != "" {
		_, err := e.compileGoPlugin(op)
		if err != nil {
			fmt.Fprintf(os.Stderr, "func_def %q: go plugin: %v (registering as manifest function)\n", op.Name, err)
			// Fall through to register as regular func_def
		} else {
			return nil, nil
		}
	}

	// Rust cdylib with source (one artifact for binary and c-shared hosts)
	if op.BodyRuntime == "rust" && op.Source != "" {
		if err := e.compileRustPlugin(op); err != nil {
			return nil, fmt.Errorf("func_def %q: rust unit: %w", op.Name, err)
		}
		return nil, nil
	}

	nativeRuntime, err := e.executeNativeFuncSource(op)
	if err != nil {
		return nil, err
	}

	fd := &FuncDef{
		Name:          op.Name,
		Params:        op.Params,
		Body:          op.Body,
		Generator:     op.Generator,
		NativeRuntime: nativeRuntime,
	}
	e.funcs[op.Name] = fd

	// Register stubs in each available runtime
	if err := e.registerStubs(fd); err != nil {
		return nil, fmt.Errorf("func_def %q stubs: %w", op.Name, err)
	}
	return nil, nil
}

func (e *Executor) executeNativeFuncSource(op *Op) (string, error) {
	if op.SourceArtifact == nil {
		return "", nil
	}
	source := strings.TrimSpace(op.SourceArtifact.FunctionSource)
	if source == "" {
		return "", nil
	}

	runtimeName := op.BodyRuntime
	if runtimeName == "" {
		runtimeName = op.Runtime
	}
	if runtimeName == "" {
		runtimeName = e.defaultRuntime
	}
	if runtimeName == "javascript" {
		return e.executeJavaScriptNativeFuncSource(op, runtimeName)
	}
	if runtimeName != "python" {
		return "", nil
	}

	rt, ok := e.runtimes[runtimeName]
	if !ok {
		return "", fmt.Errorf("func_def %q sourceArtifact: unknown runtime %q", op.Name, runtimeName)
	}
	result := rt.Execute(op.SourceArtifact.FunctionSource)
	if result.Err != nil {
		return "", fmt.Errorf("func_def %q sourceArtifact [%s]: %w", op.Name, runtimeName, result.Err)
	}
	if op.Async {
		result = rt.Execute(pythonNativeAsyncWrapperSource(op.Name))
		if result.Err != nil {
			return "", fmt.Errorf("func_def %q sourceArtifact async wrapper [%s]: %w", op.Name, runtimeName, result.Err)
		}
	}
	snapshotName := op.Name
	if op.Async {
		snapshotName = pythonNativeAsyncWrapperName(op.Name)
	}
	ref, _, err := e.boundRuntimeRefSnapshot(runtimeName, snapshotName)
	if err != nil {
		return "", fmt.Errorf("func_def %q sourceArtifact [%s] snapshot: %w", op.Name, runtimeName, err)
	}
	e.setBinding(op.Name, ref)
	return runtimeName, nil
}

func (e *Executor) executeJavaScriptNativeFuncSource(op *Op, runtimeName string) (string, error) {
	rt, ok := e.runtimes[runtimeName]
	if !ok {
		return "", fmt.Errorf("func_def %q sourceArtifact: unknown runtime %q", op.Name, runtimeName)
	}
	result := rt.Execute(javascriptNativeFunctionSource(op.SourceArtifact.FunctionSource))
	if result.Err != nil {
		return "", fmt.Errorf("func_def %q sourceArtifact [%s]: %w", op.Name, runtimeName, result.Err)
	}
	ref, _, err := e.boundRuntimeRefSnapshot(runtimeName, op.Name)
	if err != nil {
		return "", fmt.Errorf("func_def %q sourceArtifact [%s] snapshot: %w", op.Name, runtimeName, err)
	}
	e.setBinding(op.Name, ref)
	return runtimeName, nil
}

func javascriptNativeFunctionSource(source string) string {
	return regexp.MustCompile(`^(\s*)(async\s+)?\*`).ReplaceAllString(source, `${1}${2}function*`)
}

func pythonNativeAsyncWrapperSource(name string) string {
	hidden := "__omnivm_native_async_" + name
	wrapper := pythonNativeAsyncWrapperName(name)
	return fmt.Sprintf(`import asyncio as __omnivm_asyncio
import functools as __omnivm_functools
import inspect as __omnivm_inspect
%s = %s
def %s(*__omnivm_args, **__omnivm_kwargs):
  __omnivm_value = %s(*__omnivm_args, **__omnivm_kwargs)
  if __omnivm_inspect.isawaitable(__omnivm_value):
    return __omnivm_asyncio.run(__omnivm_value)
  return __omnivm_value
%s = __omnivm_functools.wraps(%s)(%s)`, hidden, name, wrapper, hidden, wrapper, hidden, wrapper)
}

func pythonNativeAsyncWrapperName(name string) string {
	return "__omnivm_sync_" + name
}

func (e *Executor) rewritePythonDirectAsyncSourceCall(code string) string {
	if e.awaitFromDepth > 0 {
		return code
	}
	match := pythonDirectCallExprRe.FindStringSubmatchIndex(code)
	if match == nil {
		return code
	}
	name := code[match[2]:match[3]]
	got, ok := e.getBinding(name)
	if !ok {
		return code
	}
	ref, ok := got.(RuntimeRef)
	if !ok || ref.Runtime != "python" || ref.VarName != pythonNativeAsyncWrapperName(name) {
		return code
	}
	return code[:match[2]] + ref.VarName + code[match[3]:]
}

// opReturn evaluates the return value and returns an ErrReturn sentinel.
func (e *Executor) opReturn(op *Op) (interface{}, error) {
	var val interface{}

	if op.From != nil {
		v, err := e.executeOp(op.From)
		if err != nil {
			return nil, err
		}
		val = v
	} else if op.Value != nil {
		switch op.Value.Kind {
		case "literal":
			val = op.Value.Value
		case "ref":
			v, ok := e.getBinding(op.Value.Name)
			if !ok {
				return nil, fmt.Errorf("return: undefined binding %q", op.Value.Name)
			}
			val = v
		default:
			return nil, fmt.Errorf("return: unknown value kind %q", op.Value.Kind)
		}
	}

	return nil, ErrReturn{Value: val}
}

// opIf evaluates condition arms and executes the first truthy body.
func (e *Executor) opIf(op *Op) (interface{}, error) {
	for _, arm := range op.Arms {
		truthy, err := e.evalCondition(arm.Test)
		if err != nil {
			return nil, fmt.Errorf("if condition: %w", err)
		}
		if truthy {
			return e.executeOps(arm.Body)
		}
	}
	if len(op.ElseBody) > 0 {
		return e.executeOps(op.ElseBody)
	}
	return nil, nil
}

// opLoop executes a loop with a safety guard.
func (e *Executor) opLoop(op *Op) (interface{}, error) {
	const maxIterations = 100000

	// foreach mode with variable/iterable
	if op.Mode == "foreach" && op.Iterable != nil {
		return e.opLoopForeach(op)
	}

	for i := 0; i < maxIterations; i++ {
		switch op.Mode {
		case "infinite":
			// always run
		case "while", "for", "":
			if op.Test != nil {
				truthy, err := e.evalCondition(op.Test)
				if err != nil {
					return nil, fmt.Errorf("loop condition: %w", err)
				}
				if !truthy {
					return nil, nil
				}
			}
		default:
			return nil, fmt.Errorf("unknown loop mode: %q", op.Mode)
		}

		_, err := e.executeOps(op.Body)
		if err != nil {
			switch err.(type) {
			case ErrBreak:
				return nil, nil
			case ErrContinue:
				continue
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("loop exceeded %d iterations", maxIterations)
}

// opLoopForeach implements foreach loops over an iterable binding.
func (e *Executor) opLoopForeach(op *Op) (interface{}, error) {
	// Resolve the iterable
	var collection []interface{}
	iterationMode := foreachIterationMode(op.IterationMode)
	switch op.Iterable.Kind {
	case "ref":
		val, ok := e.getBinding(op.Iterable.Name)
		if !ok {
			// Check if the ref is a function call expression like "crawl(\"/var/data\")" or "os.scandir(root)"
			if strings.Contains(op.Iterable.Name, "(") {
				handled, err := e.opLoopForeachNativeGeneratorCall(op)
				if err != nil {
					return nil, fmt.Errorf("foreach: %w", err)
				}
				if handled {
					return nil, nil
				}
				resolved, err := e.resolveIterableCall(op.Iterable.Name)
				if err != nil {
					return nil, fmt.Errorf("foreach: %w", err)
				}
				collection = resolved
				break
			}
			return nil, fmt.Errorf("foreach: undefined binding %q", op.Iterable.Name)
		}
		if ref, ok := val.(RuntimeRef); ok {
			if op.Await {
				handled, err := e.opLoopForeachRuntimeRefStream(op, ref)
				if err != nil {
					return nil, err
				}
				if handled {
					return nil, nil
				}
			}
			mode := iterationMode
			if mode == "auto" {
				mode = e.runtimeRefAutoIterationMode(ref)
			}
			items, iterOK, err := e.runtimeRefIter(0, ref, mode)
			if err != nil {
				return nil, fmt.Errorf("foreach: runtime ref iterable %q: %w", op.Iterable.Name, err)
			}
			if iterOK {
				collection = items
				break
			}
			val = ref.Value
		}
		if op.Await {
			handled, err := e.opLoopForeachStreamValue(op, val)
			if err != nil {
				return nil, err
			}
			if handled {
				return nil, nil
			}
		}
		arr, ok := val.([]interface{})
		if !ok {
			return nil, fmt.Errorf("foreach: iterable %q is not an array (got %T)", op.Iterable.Name, val)
		}
		collection = arr
	case "literal":
		arr, ok := op.Iterable.Value.([]interface{})
		if !ok {
			return nil, fmt.Errorf("foreach: literal iterable is not an array")
		}
		collection = arr
	default:
		return nil, fmt.Errorf("foreach: unknown iterable kind %q", op.Iterable.Kind)
	}

	for _, elem := range collection {
		e.setBinding(op.Variable, elem)
		_, err := e.executeOps(op.Body)
		if err != nil {
			switch err.(type) {
			case ErrBreak:
				return nil, nil
			case ErrContinue:
				continue
			}
			return nil, err
		}
	}
	return nil, nil
}

func foreachIterationMode(mode string) string {
	switch mode {
	case "keys", "values", "auto":
		return mode
	default:
		return "values"
	}
}

func (e *Executor) runtimeRefAutoIterationMode(ref RuntimeRef) string {
	if kind, ok := e.runtimeRefCollectionKind(ref); ok && kind == "mapping" {
		return "keys"
	}
	return "values"
}

func (e *Executor) opLoopForeachRuntimeRefStream(op *Op, ref RuntimeRef) (bool, error) {
	stream, err := e.runtimeRefIsStream(ref)
	if err != nil {
		return true, fmt.Errorf("foreach: runtime ref stream probe %q: %w", op.Iterable.Name, err)
	}
	if !stream {
		return false, nil
	}
	id, err := e.runtimeRefStreamHandle(ref)
	if err != nil {
		return true, fmt.Errorf("foreach: runtime ref stream %q: %w", op.Iterable.Name, err)
	}
	return e.opLoopForeachStreamHandle(op, id, fmt.Sprintf("runtime ref stream %q", op.Iterable.Name))
}

func (e *Executor) opLoopForeachStreamValue(op *Op, value interface{}) (bool, error) {
	var id handles.ID
	var err error
	switch v := value.(type) {
	case *ChanRef:
		id, err = e.channelStreamHandle(v)
	case *GoStreamProxy:
		return e.opLoopForeachGoStreamProxy(op, v)
	default:
		if !isReceivableChannelValue(value) && !isReaderStreamValue(value) {
			return false, nil
		}
		id, err = e.genericStreamHandle("go", value)
	}
	if err != nil {
		return true, fmt.Errorf("foreach: stream %q: %w", op.Iterable.Name, err)
	}
	return e.opLoopForeachStreamHandle(op, id, fmt.Sprintf("stream %q", op.Iterable.Name))
}

func (e *Executor) opLoopForeachStreamHandle(op *Op, id handles.ID, label string) (bool, error) {
	for {
		elem, done, ok, err := e.handleStreamNext(id)
		if err != nil {
			return true, fmt.Errorf("foreach: %s: %w", label, err)
		}
		if !ok {
			return false, nil
		}
		if done {
			return true, nil
		}
		e.setBinding(op.Variable, elem)
		if _, err := e.executeOps(op.Body); err != nil {
			switch err.(type) {
			case ErrBreak:
				if releaseErr := e.ensureHandleTable().ReleaseAllRefs(id); releaseErr != nil {
					return true, fmt.Errorf("break; additionally failed to close foreach stream: %w", releaseErr)
				}
				return true, nil
			case ErrContinue:
				continue
			}
			if releaseErr := e.ensureHandleTable().ReleaseAllRefs(id); releaseErr != nil {
				return true, fmt.Errorf("%w; additionally failed to close foreach stream after body error: %w", err, releaseErr)
			}
			return true, err
		}
	}
}

func (e *Executor) opLoopForeachGoStreamProxy(op *Op, stream *GoStreamProxy) (bool, error) {
	for {
		elem, ok, err := stream.Next()
		if err != nil {
			return true, fmt.Errorf("foreach: stream %q: %w", op.Iterable.Name, err)
		}
		if !ok {
			return true, nil
		}
		e.setBinding(op.Variable, elem)
		if _, err := e.executeOps(op.Body); err != nil {
			switch err.(type) {
			case ErrBreak:
				if releaseErr := stream.Close(); releaseErr != nil {
					return true, fmt.Errorf("break; additionally failed to close foreach stream: %w", releaseErr)
				}
				return true, nil
			case ErrContinue:
				continue
			}
			if releaseErr := stream.Close(); releaseErr != nil {
				return true, fmt.Errorf("%w; additionally failed to close foreach stream after body error: %w", err, releaseErr)
			}
			return true, err
		}
	}
}

func (e *Executor) opLoopForeachNativeGeneratorCall(op *Op) (bool, error) {
	ref, ok, err := e.nativeGeneratorCallRuntimeRef(op.Iterable.Name)
	if err != nil || !ok {
		return ok, err
	}
	id, err := e.runtimeRefStreamHandle(ref)
	if err != nil {
		return true, err
	}
	return e.opLoopForeachStreamHandle(op, id, fmt.Sprintf("native generator call %q", op.Iterable.Name))
}

// opThrow resolves the value and returns an ErrThrow sentinel.
func (e *Executor) opThrow(op *Op) (interface{}, error) {
	var val interface{}
	if op.Value != nil {
		switch op.Value.Kind {
		case "literal":
			val = op.Value.Value
		case "ref":
			v, ok := e.getBinding(op.Value.Name)
			if !ok {
				return nil, fmt.Errorf("throw: undefined binding %q", op.Value.Name)
			}
			if ref, ok := v.(RuntimeRef); ok {
				val = ref.Value
			} else {
				val = v
			}
		default:
			return nil, fmt.Errorf("throw: unknown value kind %q", op.Value.Kind)
		}
	}
	return nil, ErrThrow{Value: val}
}

func manifestCatchSourceRuntime(ops []*Op, fallback string) string {
	for _, op := range ops {
		if op == nil {
			continue
		}
		if op.Runtime != "" {
			return op.Runtime
		}
		if op.From != nil && op.From.Runtime != "" {
			return op.From.Runtime
		}
		for _, child := range op.Body {
			if child != nil && child.Runtime != "" {
				return child.Runtime
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return "unknown"
}

func manifestCatchRuntimeErrorValue(err error, runtimeName string) map[string]interface{} {
	runtimeName = nonEmpty(runtimeName, "unknown")
	text := ""
	if err != nil {
		text = err.Error()
	}
	errorType, message := manifestRuntimeErrorTypeAndMessage(text, runtimeName)
	stackFrames := manifestRuntimeErrorStackFrames(text)
	causeChain := manifestRuntimeErrorCauseChain(text, runtimeName)
	boundaryPath := manifestRuntimeErrorBoundary(text, runtimeName)
	details, detailsJSON := manifestRuntimeErrorDetails(text)
	return map[string]interface{}{
		"runtime":               runtimeName,
		"origin_runtime":        runtimeName,
		"originRuntime":         runtimeName,
		"type":                  errorType,
		"name":                  errorType,
		"message":               message,
		"traceback":             text,
		"stack":                 text,
		"stack_frames":          stackFrames,
		"stackFrames":           stackFrames,
		"cause_chain":           causeChain,
		"causeChain":            causeChain,
		"boundary_path":         boundaryPath,
		"boundaryPath":          boundaryPath,
		"original_error_handle": "",
		"originalErrorHandle":   "",
		"details":               details,
		"details_json":          detailsJSON,
		"detailsJson":           detailsJSON,
	}
}

func manifestRuntimeErrorTypeAndMessage(text, runtimeName string) (string, string) {
	first := text
	if idx := strings.Index(first, "\n"); idx >= 0 {
		first = first[:idx]
	}
	first = strings.TrimSpace(first)
	detail := first
	if marker := "]:"; strings.Contains(detail, marker) {
		if idx := strings.LastIndex(detail, marker); idx >= 0 {
			detail = strings.TrimSpace(detail[idx+len(marker):])
		}
	}
	if runtimeName != "" {
		prefix := runtimeName + ":"
		if strings.HasPrefix(detail, prefix) {
			detail = strings.TrimSpace(detail[len(prefix):])
		}
	}

	errorType := "RuntimeError"
	message := detail
	if candidate, rest, ok := manifestRuntimeErrorFindTypedSegment(detail); ok {
		errorType = candidate
		message = rest
	}
	if errorType == "RuntimeError" {
		lines := strings.Split(text, "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			if strings.HasPrefix(strings.ToLower(line), "caused by:") {
				continue
			}
			if candidate, rest, ok := manifestRuntimeErrorFindTypedSegment(line); ok {
				errorType = candidate
				message = rest
				break
			}
		}
	}
	if message == "" {
		message = first
	}
	return errorType, message
}

func manifestRuntimeErrorFindTypedSegment(value string) (string, string, bool) {
	parts := strings.Split(value, ": ")
	for i := 0; i < len(parts)-1; i++ {
		candidate := strings.TrimSpace(parts[i])
		if manifestRuntimeErrorTypeCandidate(candidate) {
			return manifestRuntimeErrorNormalizeType(candidate), strings.TrimSpace(strings.Join(parts[i+1:], ": ")), true
		}
	}
	return "", "", false
}

func manifestRuntimeErrorNormalizeType(value string) string {
	value = strings.TrimSpace(value)
	if idx := strings.LastIndex(value, "."); idx >= 0 && idx < len(value)-1 {
		return value[idx+1:]
	}
	return value
}

func manifestRuntimeErrorTypeCandidate(value string) bool {
	if value == "" || strings.ContainsAny(value, " \t/\\") {
		return false
	}
	return strings.HasSuffix(value, "Error") ||
		strings.HasSuffix(value, "Exception") ||
		value == "TypeError" ||
		value == "ReferenceError" ||
		value == "SyntaxError"
}

func manifestRuntimeErrorStackFrames(text string) []interface{} {
	frames := []interface{}{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !manifestRuntimeErrorMetadataLine(line) {
			frames = append(frames, line)
		}
	}
	return frames
}

func manifestRuntimeErrorCauseChain(text, runtimeName string) []interface{} {
	causes := []interface{}{}
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(strings.ToLower(line), "caused by:") {
			continue
		}
		body := strings.TrimSpace(line[len("Caused by:"):])
		causeType, causeMessage := manifestRuntimeErrorTypeAndMessage(body, runtimeName)
		traceLines := []string{body}
		stackFrames := []interface{}{}
		for j := i + 1; j < len(lines); j++ {
			next := strings.TrimSpace(lines[j])
			lower := strings.ToLower(next)
			if next == "" || strings.HasPrefix(lower, "caused by:") || manifestRuntimeErrorMetadataLine(next) {
				break
			}
			traceLines = append(traceLines, next)
			stackFrames = append(stackFrames, next)
		}
		causes = append(causes, map[string]interface{}{
			"runtime":        runtimeName,
			"origin_runtime": runtimeName,
			"originRuntime":  runtimeName,
			"type":           causeType,
			"name":           causeType,
			"message":        causeMessage,
			"traceback":      strings.Join(traceLines, "\n"),
			"stack":          strings.Join(traceLines, "\n"),
			"stack_frames":   stackFrames,
			"stackFrames":    stackFrames,
		})
	}
	return causes
}

func manifestRuntimeErrorDetails(text string) (interface{}, string) {
	for _, line := range strings.Split(text, "\n") {
		label, raw, ok := manifestRuntimeErrorMetadata(line)
		if !ok {
			continue
		}
		if label == "details" || label == "details_json" || label == "detailsjson" {
			var decoded interface{}
			if raw != "" && json.Unmarshal([]byte(raw), &decoded) == nil {
				return decoded, raw
			}
			return raw, raw
		}
	}
	return nil, ""
}

func manifestRuntimeErrorMetadataLine(line string) bool {
	_, _, ok := manifestRuntimeErrorMetadata(line)
	return ok
}

func manifestRuntimeErrorMetadata(line string) (string, string, bool) {
	trimmed := strings.TrimSpace(line)
	idx := strings.Index(trimmed, ":")
	if idx < 0 {
		return "", "", false
	}
	label := strings.ToLower(strings.TrimSpace(trimmed[:idx]))
	switch label {
	case "details", "details_json", "detailsjson", "original_error_handle", "original error handle", "original-error-handle":
		return label, strings.TrimSpace(trimmed[idx+1:]), true
	default:
		return "", "", false
	}
}

func manifestRuntimeErrorBoundary(text, runtimeName string) string {
	runtimeName = nonEmpty(runtimeName, "unknown")
	if strings.Contains(text, "exec ["+runtimeName+"]") {
		return "exec[" + runtimeName + "]"
	}
	if strings.Contains(text, "eval ["+runtimeName+"]") {
		return "eval[" + runtimeName + "]"
	}
	return "call[" + runtimeName + "]"
}

// opTry executes body, catches thrown/runtime errors, and runs finally.
func (e *Executor) opTry(op *Op) (interface{}, error) {
	val, bodyErr := e.executeOps(op.Body)

	if bodyErr != nil {
		// Manifest control flow is never caught, but finally still runs.
		if isManifestControlFlow(bodyErr) {
			e.runFinally(op.FinallyBody)
			return nil, bodyErr
		}

		// Catch the error
		if len(op.Catches) > 0 {
			catch := op.Catches[0]
			e.pushScope()

			// Bind the error value to the catch param (if present)
			var errVal interface{}
			if thrown, ok := bodyErr.(ErrThrow); ok {
				errVal = thrown.Value
			} else {
				errVal = manifestCatchRuntimeErrorValue(bodyErr, manifestCatchSourceRuntime(op.Body, catch.Runtime))
			}
			if catch.Param != "" {
				e.setBinding(catch.Param, errVal)
			}

			catchVal, catchErr := e.executeOps(catch.Body)
			e.popScope()

			e.runFinally(op.FinallyBody)

			if catchErr != nil {
				return nil, catchErr
			}
			return catchVal, nil
		}

		e.runFinally(op.FinallyBody)
		return nil, bodyErr
	}

	e.runFinally(op.FinallyBody)
	return val, nil
}

// runFinally executes finallyBody if present, ignoring errors.
func (e *Executor) runFinally(ops []*Op) {
	if len(ops) > 0 {
		e.executeOps(ops)
	}
}

// opConcat evaluates segments and concatenates them into a string.
func (e *Executor) opConcat(op *Op) (interface{}, error) {
	var buf strings.Builder
	for _, seg := range op.Segments {
		switch seg.Kind {
		case "text":
			buf.WriteString(seg.Value)
		case "ref":
			val, ok := e.getBinding(seg.Name)
			if !ok {
				return nil, fmt.Errorf("concat: undefined ref %q", seg.Name)
			}
			// Unwrap RuntimeRef to get the actual value
			if ref, ok := val.(RuntimeRef); ok {
				val = ref.Value
			}
			buf.WriteString(fmt.Sprintf("%v", val))
		case "eval":
			rtName := seg.Runtime
			if rtName == "" {
				rtName = e.defaultRuntime
			}
			rt, ok := e.runtimes[rtName]
			if !ok {
				return nil, fmt.Errorf("concat eval: unknown runtime %q", rtName)
			}
			result := rt.Eval(seg.Code)
			if result.Err != nil {
				return nil, fmt.Errorf("concat eval [%s]: %w", rtName, result.Err)
			}
			if result.Value != nil {
				buf.WriteString(fmt.Sprintf("%v", result.Value))
			} else {
				buf.WriteString(result.Output)
			}
		default:
			return nil, fmt.Errorf("concat: unknown segment kind %q", seg.Kind)
		}
	}

	val := buf.String()
	if op.Bind != "" {
		e.setBinding(op.Bind, val)
	}
	return val, nil
}

// opDeclare creates a new binding from a literal value or inner from op.
func (e *Executor) opDeclare(op *Op) (interface{}, error) {
	var val interface{}

	if op.From != nil {
		v, err := e.executeOp(op.From)
		if err != nil {
			return nil, err
		}
		val = v
	} else if op.Value != nil {
		switch op.Value.Kind {
		case "literal":
			val = op.Value.Value
		case "ref":
			v, ok := e.getBinding(op.Value.Name)
			if !ok {
				return nil, fmt.Errorf("declare: undefined ref %q", op.Value.Name)
			}
			val = v
		}
	}

	if op.Bind != "" {
		e.setBinding(op.Bind, val)
	}
	return val, nil
}

// opAssign updates an existing binding.
func (e *Executor) opAssign(op *Op) (interface{}, error) {
	target := op.Target
	if target == "" {
		target = op.Bind
	}
	if target == "" {
		return nil, fmt.Errorf("assign: no target specified")
	}

	var newVal interface{}

	if op.From != nil {
		v, err := e.executeOp(op.From)
		if err != nil {
			return nil, err
		}
		newVal = v
	} else if op.Value != nil {
		switch op.Value.Kind {
		case "literal":
			newVal = op.Value.Value
		case "ref":
			v, ok := e.getBinding(op.Value.Name)
			if !ok {
				return nil, fmt.Errorf("assign: undefined ref %q", op.Value.Name)
			}
			newVal = v
		}
	}

	// Apply operator if present
	if op.Operator != "" && op.Operator != "=" {
		existing, ok := e.getBinding(target)
		if !ok {
			return nil, fmt.Errorf("assign: undefined target %q", target)
		}
		applied, err := applyOperator(existing, op.Operator, newVal)
		if err != nil {
			return nil, fmt.Errorf("assign: %w", err)
		}
		newVal = applied
	}

	// If the target was a RuntimeRef, update the runtime's global scope
	// so subsequent captures and condition auto-injection see the new value.
	if existing, ok := e.getBinding(target); ok {
		if ref, ok := existing.(RuntimeRef); ok {
			rt, rtOk := e.runtimes[ref.Runtime]
			if rtOk {
				// Convert to a value that can be injected as a literal
				valStr := fmt.Sprintf("%v", newVal)
				assignCode := runtimeAssign(ref.Runtime, ref.VarName, valStr)
				rt.Execute(assignCode)
			}
			// Keep as RuntimeRef with updated value
			e.setBinding(target, RuntimeRef{
				Runtime: ref.Runtime,
				VarName: ref.VarName,
				Value:   newVal,
			})
			return newVal, nil
		}
	}

	e.setBinding(target, newVal)
	return newVal, nil
}

// evalCondition evaluates a CondExpr and returns whether it's truthy.
func (e *Executor) evalCondition(cond *CondExpr) (truthy bool, err error) {
	switch cond.Kind {
	case "literal":
		return isTruthy(cond.Value), nil
	case "ref":
		val, ok := e.getBinding(cond.Name)
		if !ok {
			return false, nil
		}
		return isTruthy(val), nil
	case "expr":
		rtName := cond.Runtime
		if rtName == "" {
			rtName = e.defaultRuntime
		}
		rt, ok := e.runtimes[rtName]
		if !ok {
			return false, fmt.Errorf("condition: unknown runtime %q", rtName)
		}

		code := cond.Code
		if len(cond.Captures) > 0 {
			var err error
			code, err = e.wrapWithCaptures(rtName, cond.Code, cond.Captures)
			if err != nil {
				return false, err
			}
		} else {
			// Auto-inject current scope bindings so condition code can
			// reference func_def params and other manifest variables.
			injection := e.autoInjectScopePlan(rtName)
			if injection.setup != "" {
				injectResult := rt.Execute(injection.setup)
				if injectResult.Err != nil {
					return false, fmt.Errorf("condition auto-inject [%s]: %w", rtName, injectResult.Err)
				}
				defer func() {
					if cleanupErr := e.runJavaCaptureCleanup(rt, injection); cleanupErr != nil {
						if err != nil {
							err = fmt.Errorf("%w (auto capture cleanup failed: %v)", err, cleanupErr)
							return
						}
						err = fmt.Errorf("condition auto capture cleanup [%s]: %w", rtName, cleanupErr)
					}
				}()
				// Ruby/Java: create local aliases from persisted runtime globals.
				if rtName == "ruby" {
					code = e.rubyAliasPrefix(nil) + code
				} else if rtName == "java" {
					code = e.javaPersistentAliasPrefix(nil) + code
				}
			}
		}

		result := rt.Eval(code)
		if result.Err != nil {
			return false, fmt.Errorf("condition eval [%s]: %w", rtName, result.Err)
		}
		return isTruthy(result.Value), nil
	default:
		return false, fmt.Errorf("unknown condition kind: %q", cond.Kind)
	}
}

// callGoFunc invokes a function from the Go function registry.
func (e *Executor) callGoFunc(name string, args []interface{}, bind string) (val interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("go function %q panicked: %v", name, r)
		}
	}()

	fn, ok := e.goFuncs[name]
	if !ok {
		return nil, fmt.Errorf("eval go: unknown function %q", name)
	}

	normalizedArgs := e.normalizeGoArgs(args)

	// func() interface{} (no args)
	if f, ok := fn.(func() interface{}); ok {
		val = f()
		if bind != "" {
			e.setBinding(bind, val)
		}
		return val, nil
	}

	// func(interface{}) interface{} (single arg)
	if f, ok := fn.(func(interface{}) interface{}); ok {
		var arg interface{}
		if len(normalizedArgs) > 0 {
			arg = normalizedArgs[0]
		}
		val = f(arg)
		if bind != "" {
			e.setBinding(bind, val)
		}
		return val, nil
	}

	// func(interface{}, interface{}) interface{} (two args)
	if f, ok := fn.(func(interface{}, interface{}) interface{}); ok {
		var a, b interface{}
		if len(normalizedArgs) > 0 {
			a = normalizedArgs[0]
		}
		if len(normalizedArgs) > 1 {
			b = normalizedArgs[1]
		}
		val = f(a, b)
		if bind != "" {
			e.setBinding(bind, val)
		}
		return val, nil
	}

	// func([]interface{}) (interface{}, error)
	if f, ok := fn.(func([]interface{}) (interface{}, error)); ok {
		val, err = f(normalizedArgs)
		if err != nil {
			return nil, err
		}
		if bind != "" {
			e.setBinding(bind, val)
		}
		return val, nil
	}

	// func([]interface{}) interface{}
	if f, ok := fn.(func([]interface{}) interface{}); ok {
		val = f(normalizedArgs)
		if bind != "" {
			e.setBinding(bind, val)
		}
		return val, nil
	}

	return nil, fmt.Errorf("eval go: function %q has unsupported signature", name)
}

// evalGoCode parses a simple Go function call expression like "funcName(arg1, arg2)"
// and dispatches to the goFuncs registry.
func (e *Executor) evalGoCode(op *Op) (interface{}, error) {
	code := strings.TrimSpace(op.Code)
	parenIdx := strings.Index(code, "(")
	if parenIdx < 0 || !strings.HasSuffix(code, ")") {
		// Bare identifiers resolve as bindings (e.g. `await job` lowers to
		// an eval of the spawn-handle binding).
		if val, ok := e.getBinding(code); ok {
			if op.Bind != "" {
				e.setBinding(op.Bind, val)
			}
			return val, nil
		}
		return nil, fmt.Errorf("eval go: cannot parse expression %q", code)
	}

	funcName := strings.TrimSpace(code[:parenIdx])
	argsStr := strings.TrimSpace(code[parenIdx+1 : len(code)-1])

	var args []interface{}
	if argsStr != "" {
		for _, part := range strings.Split(argsStr, ",") {
			part = strings.TrimSpace(part)
			// Try parsing as number
			if f, err := strconv.ParseFloat(part, 64); err == nil {
				if f == float64(int(f)) {
					args = append(args, int(f))
				} else {
					args = append(args, f)
				}
			} else {
				// Try as binding reference
				if val, ok := e.getBinding(part); ok {
					// Unwrap RuntimeRef to get the actual value; refs whose
					// snapshot is stale or unknown re-snapshot from the
					// owner runtime (the global may have been mutated).
					if ref, ok := val.(RuntimeRef); ok {
						val = ref.Value
						if _, fresh, err := e.boundRuntimeRefSnapshot(ref.Runtime, ref.VarName); err == nil {
							val = fresh
						}
					}
					args = append(args, val)
				} else {
					// Use as string literal (strip quotes if present)
					part = strings.Trim(part, "\"'")
					args = append(args, part)
				}
			}
		}
	}

	return e.callGoFunc(funcName, args, op.Bind)
}

// resolveGoCallArg resolves one go-call argument expression: numbers parse,
// binding names resolve (refs re-snapshot from the owner runtime — the
// global may have been mutated since binding), quoted literals unquote, and
// non-string values pass through untouched.
func (e *Executor) resolveGoCallArg(raw interface{}) interface{} {
	part, isString := raw.(string)
	if !isString {
		return raw
	}
	part = strings.TrimSpace(part)
	if f, err := strconv.ParseFloat(part, 64); err == nil {
		if f == float64(int(f)) {
			return int(f)
		}
		return f
	}
	if val, ok := e.getBinding(part); ok {
		if ref, isRef := val.(RuntimeRef); isRef {
			val = ref.Value
			if _, fresh, err := e.boundRuntimeRefSnapshot(ref.Runtime, ref.VarName); err == nil {
				val = fresh
			}
		}
		return val
	}
	return strings.Trim(part, "\"'")
}

func (e *Executor) resolveGoSelectorConstant(expr string) (interface{}, bool) {
	expr = strings.TrimSpace(expr)
	parts := strings.Split(expr, ".")
	if len(parts) < 2 || !goIdentifierRE.MatchString(parts[0]) {
		return nil, false
	}
	for _, part := range parts[1:] {
		if !goIdentifierRE.MatchString(part) {
			return nil, false
		}
	}
	binding, ok := e.getBinding(parts[0])
	if !ok {
		return nil, false
	}
	ref, ok := binding.(ImportRef)
	if !ok || ref.Runtime != "go" || ref.Name == "" {
		return nil, false
	}
	goTool, err := goToolPath()
	if err != nil {
		return nil, false
	}
	tmpDir, err := os.MkdirTemp("", "omnivm-go-const-*")
	if err != nil {
		return nil, false
	}
	defer os.RemoveAll(tmpDir)

	source := fmt.Sprintf(`package main

import (
	__omnivm_json "encoding/json"
	__omnivm_os "os"
	%s %q
)

func main() {
	value := %s
	payload, err := __omnivm_json.Marshal(value)
	if err != nil {
		panic(err)
	}
	_, _ = __omnivm_os.Stdout.Write(payload)
}
`, parts[0], ref.Name, expr)
	sourcePath := tmpDir + "/main.go"
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		return nil, false
	}
	cmd := exec.Command(goTool, "run", sourcePath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, false
	}
	var value interface{}
	if err := json.Unmarshal(out, &value); err != nil {
		return nil, false
	}
	return normalizeGoConstantJSONValue(value), true
}

func normalizeGoConstantJSONValue(value interface{}) interface{} {
	switch v := value.(type) {
	case float64:
		maxInt := int(^uint(0) >> 1)
		minInt := -maxInt - 1
		if v == math.Trunc(v) && v >= float64(minInt) && v <= float64(maxInt) {
			return int(v)
		}
		return v
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = normalizeGoConstantJSONValue(item)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for key, item := range v {
			out[key] = normalizeGoConstantJSONValue(item)
		}
		return out
	default:
		return value
	}
}

// resolveValueExpr resolves a ValueExpr to its Go value.
func (e *Executor) resolveValueExpr(v *ValueExpr) (interface{}, error) {
	if v == nil {
		return nil, nil
	}
	switch v.Kind {
	case "literal":
		return v.Value, nil
	case "ref":
		val, ok := e.getBinding(v.Name)
		if !ok {
			return nil, fmt.Errorf("undefined binding %q", v.Name)
		}
		if ref, ok := val.(RuntimeRef); ok {
			return ref.Value, nil
		}
		return val, nil
	default:
		return nil, fmt.Errorf("unknown value kind %q", v.Kind)
	}
}

func (e *Executor) opResource(op *Op) (interface{}, error) {
	switch op.Action {
	case "open":
		if op.Bind == "" {
			return nil, fmt.Errorf("resource open: bind is required")
		}
		runtime := op.Runtime
		if runtime == "" {
			runtime = e.defaultRuntime
		}
		ref := &ResourceRef{
			Runtime:  runtime,
			Kind:     op.Kind,
			Disposer: op.Disposer,
		}
		if op.Value != nil {
			val, err := e.resolveValueExpr(op.Value)
			if err != nil {
				return nil, fmt.Errorf("resource open value: %w", err)
			}
			ref.Value = val
		}
		table := e.ensureHandleTable()
		var id handles.ID
		var err error
		id, err = table.Register(ref, handles.RegisterOptions{
			Runtime: runtime,
			Kind:    nonEmpty(op.Kind, "resource"),
			ScopeID: e.currentHandleScope(),
			Release: func(any) error {
				ref.Closed = true
				e.forgetReleasedHandle(id, ref)
				return nil
			},
		})
		if err != nil {
			return nil, fmt.Errorf("resource open handle: %w", err)
		}
		ref.ID = id
		e.resources[ref.ID] = ref
		e.setBinding(op.Bind, ref)
		return ref, nil
	case "close":
		name := op.Target
		if name == "" {
			name = op.Bind
		}
		if name == "" {
			return nil, fmt.Errorf("resource close: target is required")
		}
		val, ok := e.getBinding(name)
		if !ok {
			return nil, fmt.Errorf("resource close: undefined binding %q", name)
		}
		ref, ok := val.(*ResourceRef)
		if !ok {
			if op.Code != "" {
				runtime := op.Runtime
				if runtime == "" {
					runtime = e.defaultRuntime
				}
				if _, err := e.opExec(&Op{OpType: "exec", Runtime: runtime, Code: op.Code, Async: op.Async}); err != nil {
					return nil, fmt.Errorf("resource close cleanup: %w", err)
				}
				return val, nil
			}
			return nil, fmt.Errorf("resource close: %q is not a resource (got %T)", name, val)
		}
		if ref.Closed {
			return ref, nil
		}
		if op.Code != "" {
			runtime := op.Runtime
			if runtime == "" {
				runtime = ref.Runtime
			}
			if runtime == "" {
				runtime = e.defaultRuntime
			}
			if _, err := e.opExec(&Op{OpType: "exec", Runtime: runtime, Code: op.Code, Async: op.Async}); err != nil {
				return nil, fmt.Errorf("resource close cleanup: %w", err)
			}
		}
		if err := e.ensureHandleTable().ReleaseAllRefs(ref.ID); err != nil {
			return nil, fmt.Errorf("resource close handle: %w", err)
		}
		ref.Closed = true
		return ref, nil
	default:
		return nil, fmt.Errorf("resource: unknown action %q", op.Action)
	}
}

func (e *Executor) opTable(op *Op) (interface{}, error) {
	switch op.Action {
	case "export":
		if op.Bind == "" {
			return nil, fmt.Errorf("table export: bind is required")
		}
		runtime := op.Runtime
		if runtime == "" {
			runtime = e.defaultRuntime
		}
		format := op.Format
		if format == "" {
			format = "arrow_c_data"
		}
		ownership := op.Ownership
		if ownership == "" {
			ownership = "borrowed"
		}
		ref := &TableRef{
			Runtime:   runtime,
			Format:    format,
			Ownership: ownership,
			Release:   op.Release,
			Metadata:  cloneTableMetadata(op.Metadata),
		}
		fillTableMetadataMemorySpace(ref.Metadata)
		if op.Value != nil {
			val, err := e.resolveValueExpr(op.Value)
			if err != nil {
				return nil, fmt.Errorf("table export value: %w", err)
			}
			ref.Value = val
		}
		table := e.ensureHandleTable()
		var id handles.ID
		var err error
		id, err = table.Register(ref, handles.RegisterOptions{
			Runtime: runtime,
			Kind:    "table:" + format,
			ScopeID: e.currentHandleScope(),
			Release: func(any) error {
				ref.Released = true
				e.forgetReleasedHandle(id, ref)
				return nil
			},
		})
		if err != nil {
			return nil, fmt.Errorf("table export handle: %w", err)
		}
		ref.ID = id
		e.tables[ref.ID] = ref
		e.setBinding(op.Bind, ref)
		return ref, nil
	case "release":
		ref, err := e.tableFromTarget(op.Target)
		if err != nil {
			return nil, err
		}
		if ref.Released {
			return ref, nil
		}
		if op.Code != "" {
			runtime := op.Runtime
			if runtime == "" {
				runtime = ref.Runtime
			}
			if runtime == "" {
				runtime = e.defaultRuntime
			}
			if _, err := e.opExec(&Op{OpType: "exec", Runtime: runtime, Code: op.Code}); err != nil {
				return nil, fmt.Errorf("table release cleanup: %w", err)
			}
		}
		if err := e.ensureHandleTable().ReleaseAllRefs(ref.ID); err != nil {
			return nil, fmt.Errorf("table release handle: %w", err)
		}
		return ref, nil
	default:
		return nil, fmt.Errorf("table: unknown action %q", op.Action)
	}
}

func (e *Executor) opJob(op *Op) (interface{}, error) {
	switch op.Action {
	case "enqueue":
		if op.Bind == "" {
			return nil, fmt.Errorf("job enqueue: bind is required")
		}
		runtime := op.Runtime
		if runtime == "" {
			runtime = e.defaultRuntime
		}
		var payload interface{}
		if op.Payload != nil {
			val, err := e.resolveValueExpr(op.Payload)
			if err != nil {
				return nil, fmt.Errorf("job enqueue payload: %w", err)
			}
			payload = val
		}
		e.nextJobID++
		job := &JobHandle{
			ID:      e.nextJobID,
			Runtime: runtime,
			Kind:    op.Kind,
			Payload: payload,
		}
		e.jobs[job.ID] = job
		e.setBinding(op.Bind, job)
		return job, nil
	case "complete":
		job, err := e.jobFromTarget(op.Target)
		if err != nil {
			return nil, err
		}
		if job.Cancelled {
			return nil, fmt.Errorf("job complete: job %d was cancelled", job.ID)
		}
		var result interface{}
		if op.Value != nil {
			result, err = e.resolveValueExpr(op.Value)
			if err != nil {
				return nil, fmt.Errorf("job complete value: %w", err)
			}
		}
		job.Result = result
		job.Done = true
		return job, nil
	case "wait":
		job, err := e.jobFromTarget(op.Target)
		if err != nil {
			return nil, err
		}
		if job.Cancelled {
			return nil, fmt.Errorf("job wait: job %d was cancelled", job.ID)
		}
		if !job.Done {
			return nil, fmt.Errorf("job wait: job %d is not complete", job.ID)
		}
		if op.Bind != "" {
			e.setBinding(op.Bind, job.Result)
		}
		return job.Result, nil
	case "cancel":
		job, err := e.jobFromTarget(op.Target)
		if err != nil {
			return nil, err
		}
		if job.Done || job.Cancelled {
			return job, nil
		}
		var reason interface{}
		if op.Value != nil {
			reason, err = e.resolveValueExpr(op.Value)
			if err != nil {
				return nil, fmt.Errorf("job cancel value: %w", err)
			}
		}
		if op.Code != "" {
			runtime := op.Runtime
			if runtime == "" {
				runtime = job.Runtime
			}
			if runtime == "" {
				runtime = e.defaultRuntime
			}
			if _, err := e.opExec(&Op{OpType: "exec", Runtime: runtime, Code: op.Code}); err != nil {
				return nil, fmt.Errorf("job cancel cleanup: %w", err)
			}
		}
		job.Cancelled = true
		job.CancelReason = reason
		job.Done = true
		return job, nil
	default:
		return nil, fmt.Errorf("job: unknown action %q", op.Action)
	}
}

func (e *Executor) tableFromTarget(name string) (*TableRef, error) {
	if name == "" {
		return nil, fmt.Errorf("table release: target is required")
	}
	val, ok := e.getBinding(name)
	if !ok {
		return nil, fmt.Errorf("table release: undefined binding %q", name)
	}
	ref, ok := val.(*TableRef)
	if !ok {
		return nil, fmt.Errorf("table release: %q is not a table (got %T)", name, val)
	}
	return ref, nil
}

func (e *Executor) jobFromTarget(name string) (*JobHandle, error) {
	if name == "" {
		return nil, fmt.Errorf("job: target is required")
	}
	val, ok := e.getBinding(name)
	if !ok {
		return nil, fmt.Errorf("job: undefined binding %q", name)
	}
	job, ok := val.(*JobHandle)
	if !ok {
		return nil, fmt.Errorf("job: %q is not a job handle (got %T)", name, val)
	}
	return job, nil
}

// opYield appends a value to the current generator's yield collector.
// All manifest op execution runs single-threaded on the Golden Thread;
// spawned goroutines only call pure Go functions and never access yieldCollectors.
func (e *Executor) opYield(op *Op) (interface{}, error) {
	if len(e.yieldCollectors) == 0 {
		// Not inside a generator context, no-op
		return nil, nil
	}

	var val interface{}
	if op.From != nil {
		v, err := e.executeOp(op.From)
		if err != nil {
			return nil, err
		}
		val = v
	} else if op.Value != nil {
		v, err := e.resolveValueExpr(op.Value)
		if err != nil {
			return nil, err
		}
		val = v
	}
	// else: bare yield, val = nil
	val, err := e.freezeYieldValue(val)
	if err != nil {
		return nil, err
	}

	top := len(e.yieldCollectors) - 1
	if op.Delegate {
		// Delegate yield: spread array values into the collector
		switch arr := val.(type) {
		case []interface{}:
			e.yieldCollectors[top] = append(e.yieldCollectors[top], arr...)
		case string:
			// Try parsing JSON array (e.g. from nested generator call via bridge)
			var parsed []interface{}
			if json.Unmarshal([]byte(arr), &parsed) == nil {
				e.yieldCollectors[top] = append(e.yieldCollectors[top], parsed...)
			} else {
				e.yieldCollectors[top] = append(e.yieldCollectors[top], val)
			}
		default:
			e.yieldCollectors[top] = append(e.yieldCollectors[top], val)
		}
	} else {
		e.yieldCollectors[top] = append(e.yieldCollectors[top], val)
	}

	return nil, nil
}

func (e *Executor) freezeYieldValue(val interface{}) (interface{}, error) {
	ref, ok := val.(RuntimeRef)
	if !ok || ref.VarName != "__yield" {
		return val, nil
	}
	rt, ok := e.runtimes[ref.Runtime]
	if !ok {
		return nil, fmt.Errorf("yield freeze: unknown runtime %q", ref.Runtime)
	}
	e.nextRuntimeRefID++
	frozenName := fmt.Sprintf("__yield_%d", e.nextRuntimeRefID)
	assignCode := runtimeAssign(ref.Runtime, frozenName, runtimeVarRef(ref.Runtime, ref.VarName))
	result := rt.Execute(assignCode)
	if result.Err != nil {
		return nil, fmt.Errorf("yield freeze [%s]: %w", ref.Runtime, result.Err)
	}
	frozenRef, frozenVal, err := e.boundRuntimeRefSnapshot(ref.Runtime, frozenName)
	if err != nil {
		return nil, fmt.Errorf("yield freeze [%s]: %w", ref.Runtime, err)
	}
	e.setBinding(frozenName, frozenRef)
	return frozenVal, nil
}

// opAwait executes the inner from op and binds the resolved result.
func (e *Executor) opAwait(op *Op) (interface{}, error) {
	if op.From == nil {
		return nil, nil
	}
	e.awaitFromDepth++
	val, err := e.executeOp(op.From)
	e.awaitFromDepth--
	if err != nil {
		return nil, err
	}
	if ref, ok := val.(*RustFutureRef); ok {
		return e.awaitRustFutureRef(ref, op.Bind)
	}
	if ref, ok := val.(RuntimeRef); ok {
		switch ref.Runtime {
		case "javascript":
			return e.awaitJavaScriptRuntimeRef(ref, op.Bind)
		case "python":
			return e.awaitPythonRuntimeRef(ref, op.Bind)
		}
	}
	if op.Bind != "" {
		e.setBinding(op.Bind, val)
	}
	return val, nil
}

// rubyAliasPrefix generates Ruby code to create local aliases from $global
// variables. If explicit captures are provided, only those are aliased.
// Otherwise, all serializable scope bindings are aliased.
func (e *Executor) rubyAliasPrefix(captures map[string]string) string {
	var aliases []string
	if len(captures) > 0 {
		for varName := range captures {
			aliases = append(aliases, fmt.Sprintf("%s = $%s", varName, varName))
		}
	} else {
		for _, scope := range e.scopes {
			for varName, val := range scope {
				if _, ok := val.(ImportRef); ok {
					continue
				}
				aliases = append(aliases, fmt.Sprintf("%s = $%s", varName, varName))
			}
		}
	}
	if len(aliases) == 0 {
		return ""
	}
	return strings.Join(aliases, "; ") + "; "
}

func (e *Executor) javaPersistentAliasPrefix(exclude map[string]bool) string {
	var aliases []string
	seen := make(map[string]bool)
	for _, scope := range e.scopes {
		for varName, val := range scope {
			if exclude[varName] || seen[varName] || !isJavaIdentifier(varName) {
				continue
			}
			ref, ok := val.(RuntimeRef)
			if !ok || ref.Runtime != "java" {
				continue
			}
			seen[varName] = true
			typeName := ref.TypeName
			if typeName == "" {
				typeName = "Object"
			}
			aliases = append(aliases, fmt.Sprintf("%s %s = (%s) omnivm.OmniVM.getCapture(\"%s\");", typeName, varName, typeName, escapeJavaString(ref.VarName)))
		}
	}
	if len(aliases) == 0 {
		return ""
	}
	return strings.Join(aliases, "\n") + "\n"
}

func inferJavaBindType(expr string) string {
	expr = strings.TrimSpace(expr)
	if strings.HasPrefix(expr, "new ") && !strings.Contains(expr, ").") {
		rest := strings.TrimSpace(strings.TrimPrefix(expr, "new "))
		end := strings.IndexAny(rest, "(<[{ ")
		if end > 0 {
			return rest[:end]
		}
	}
	if strings.Contains(expr, ".readValue(") {
		return "java.util.Map"
	}
	if strings.Contains(expr, ".collectList().block()") {
		return "java.util.List"
	}
	return ""
}

func normalizeJavaEvalExpression(expr string) string {
	expr = strings.TrimSpace(expr)
	const marker = ".collectList().block().get("
	idx := strings.Index(expr, marker)
	if idx <= 0 {
		return expr
	}
	receiver := strings.TrimSpace(expr[:idx])
	rest := expr[idx+len(marker):]
	if receiver == "" || rest == "" {
		return expr
	}
	return fmt.Sprintf("((java.util.List)(%s.collectList().block())).get(%s", receiver, rest)
}

func javaPrimitiveTypeName(value interface{}) string {
	switch value.(type) {
	case string:
		return "String"
	case bool:
		return "Boolean"
	case int, int8, int16, int32, int64:
		return "Long"
	case uint, uint8, uint16, uint32, uint64:
		return "Long"
	case float32, float64:
		return "Double"
	default:
		return ""
	}
}

// Scope operations

func (e *Executor) pushScope() {
	e.scopes = append(e.scopes, make(map[string]interface{}))
	if e.handleTable != nil {
		e.handleScopes = append(e.handleScopes, e.handleTable.NewScope())
	}
}

func (e *Executor) popScope() {
	if len(e.scopes) > 1 {
		_ = e.releaseLastHandleScope()
		e.scopes = e.scopes[:len(e.scopes)-1]
	}
}

func (e *Executor) getBinding(name string) (interface{}, bool) {
	for i := len(e.scopes) - 1; i >= 0; i-- {
		if val, ok := e.scopes[i][name]; ok {
			return val, true
		}
	}
	return nil, false
}

func (e *Executor) setBinding(name string, val interface{}) {
	e.scopes[len(e.scopes)-1][name] = val
}

func (e *Executor) currentHandleScope() handles.ScopeID {
	table := e.ensureHandleTable()
	if len(e.handleScopes) == 0 {
		e.handleScopes = append(e.handleScopes, table.NewScope())
	}
	return e.handleScopes[len(e.handleScopes)-1]
}

func (e *Executor) ensureHandleTable() *handles.Table {
	if e.handleTable == nil {
		e.handleTable = handles.NewTable()
	}
	return e.handleTable
}

func (e *Executor) releaseLastHandleScope() error {
	if e.handleTable == nil || len(e.handleScopes) == 0 {
		return nil
	}
	scope := e.handleScopes[len(e.handleScopes)-1]
	e.handleScopes = e.handleScopes[:len(e.handleScopes)-1]
	return e.handleTable.ReleaseScope(scope)
}

func (e *Executor) releaseAllHandleScopes() error {
	var firstErr error
	for len(e.handleScopes) > 0 {
		if err := e.releaseLastHandleScope(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (e *Executor) forgetReleasedHandle(id handles.ID, value interface{}) {
	if id == 0 {
		return
	}
	e.rememberReleasedResource(id, value)
	e.rememberReleasedTable(id, value)
	delete(e.resources, id)
	delete(e.tables, id)
	e.forgetBridgeHandle(id, value)
	switch v := value.(type) {
	case *ResourceRef:
		if v != nil {
			e.forgetBridgeHandle(id, v.Value)
		}
	case ResourceRef:
		e.forgetBridgeHandle(id, v.Value)
	case *TableRef:
		if v != nil {
			e.forgetBridgeHandle(id, v.Value)
		}
	case TableRef:
		e.forgetBridgeHandle(id, v.Value)
	}
}

func (e *Executor) rememberReleasedResource(id handles.ID, value interface{}) {
	if e.releasedResources == nil {
		e.releasedResources = make(map[handles.ID]*ResourceRef)
	}
	var ref *ResourceRef
	switch v := value.(type) {
	case *ResourceRef:
		ref = v
	case ResourceRef:
		ref = &v
	}
	if ref == nil {
		return
	}
	tombstone := *ref
	tombstone.ID = id
	tombstone.Closed = true
	tombstone.Value = nil
	e.releasedResources[id] = &tombstone
}

func (e *Executor) rememberReleasedTable(id handles.ID, value interface{}) {
	if e.releasedTables == nil {
		e.releasedTables = make(map[handles.ID]*TableRef)
	}
	var ref *TableRef
	switch v := value.(type) {
	case *TableRef:
		ref = v
	case TableRef:
		ref = &v
	}
	if ref == nil {
		return
	}
	tombstone := *ref
	tombstone.ID = id
	tombstone.Released = true
	tombstone.Value = nil
	e.releasedTables[id] = &tombstone
}

type releasedStreamRef struct {
	Runtime string
	Kind    string
}

func (e *Executor) rememberReleasedStream(id handles.ID, runtime, kind string) {
	if e.releasedStreams == nil {
		e.releasedStreams = make(map[handles.ID]releasedStreamRef)
	}
	e.releasedStreams[id] = releasedStreamRef{
		Runtime: nonEmpty(runtime, "unknown"),
		Kind:    nonEmpty(kind, "stream"),
	}
}

func (e *Executor) handleEntry(id handles.ID) (handles.Entry, error) {
	entry, ok := e.ensureHandleTable().Get(id)
	if ok {
		return entry, nil
	}
	if ref := e.releasedResources[id]; ref != nil {
		return handles.Entry{}, releasedResourceLifecycleError(id, ref)
	}
	if ref := e.releasedTables[id]; ref != nil {
		return handles.Entry{}, releasedTableLifecycleError(id, ref)
	}
	if ref, ok := e.releasedStreams[id]; ok {
		return handles.Entry{}, releasedStreamLifecycleError(id, ref)
	}
	return handles.Entry{}, fmt.Errorf("manifest HandleCall: unknown handle %d", id)
}

func (e *Executor) forgetBridgeHandle(id handles.ID, value interface{}) {
	if e.bridgeHandles == nil {
		return
	}
	ident, ok := bridgeIdentityForValue(value)
	if !ok {
		return
	}
	if current, ok := e.bridgeHandles[ident]; ok && current == id {
		delete(e.bridgeHandles, ident)
	}
}

func (e *Executor) BoundaryStats() BoundaryStats {
	e.boundaryStatsMu.Lock()
	defer e.boundaryStatsMu.Unlock()
	return e.boundaryStats
}

func (e *Executor) addBoundaryStat(update func(*BoundaryStats)) {
	e.boundaryStatsMu.Lock()
	defer e.boundaryStatsMu.Unlock()
	update(&e.boundaryStats)
}

// Helpers

func nonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func isTruthy(val interface{}) bool {
	if val == nil {
		return false
	}
	switch v := val.(type) {
	case bool:
		return v
	case string:
		// Handle common runtime return values
		switch strings.ToLower(v) {
		case "", "false", "none", "null", "nil", "0", "undefined":
			return false
		}
		return true
	case float64:
		return v != 0
	case int:
		return v != 0
	case json.Number:
		f, _ := v.Float64()
		return f != 0
	default:
		return true
	}
}

func applyOperator(existing interface{}, op string, newVal interface{}) (interface{}, error) {
	ef := toFloat(existing)
	nf := toFloat(newVal)

	var result float64
	switch op {
	case "+=":
		result = ef + nf
	case "-=":
		result = ef - nf
	case "*=":
		result = ef * nf
	case "/=":
		if nf == 0 {
			return nil, fmt.Errorf("division by zero")
		}
		result = ef / nf
	default:
		return nil, fmt.Errorf("unknown operator %q", op)
	}

	// Return int if the result has no fractional part
	if result == float64(int(result)) {
		return int(result), nil
	}
	return result, nil
}

func toFloat(v interface{}) float64 {
	switch val := v.(type) {
	case float64:
		return val
	case int:
		return float64(val)
	case string:
		f, _ := strconv.ParseFloat(val, 64)
		return f
	case json.Number:
		f, _ := val.Float64()
		return f
	case RuntimeRef:
		return toFloat(val.Value)
	default:
		return 0
	}
}

// convertFStringToTemplateLiteral converts Python f-string syntax (f"...{var}...")
// to JavaScript template literal syntax (`...${var}...`).
func convertFStringToTemplateLiteral(code string) string {
	result := code
	for _, quote := range []string{`"`, `'`} {
		prefix := "f" + quote
		searchFrom := 0
		for {
			rel := strings.Index(result[searchFrom:], prefix)
			if rel < 0 {
				break
			}
			idx := searchFrom + rel
			// Only match f"/' at word boundary (not preceded by a letter/digit/underscore)
			if idx > 0 {
				prev := result[idx-1]
				if (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') ||
					(prev >= '0' && prev <= '9') || prev == '_' {
					searchFrom = idx + len(prefix)
					continue
				}
			}
			// Find the matching closing quote
			inner := result[idx+len(prefix):]
			closeIdx := strings.Index(inner, quote)
			if closeIdx < 0 {
				break
			}
			body := inner[:closeIdx]
			// Convert {var} to ${var}
			var converted strings.Builder
			for i := 0; i < len(body); i++ {
				if body[i] == '{' && (i == 0 || body[i-1] != '$') {
					converted.WriteString("${")
				} else {
					converted.WriteByte(body[i])
				}
			}
			// Replace f"..." with `...`
			replacement := "`" + converted.String() + "`"
			result = result[:idx] + replacement + result[idx+len(prefix)+closeIdx+len(quote):]
			searchFrom = idx + len(replacement)
		}
	}
	return result
}

// resolveIterableCall resolves a function call expression used as a foreach iterable.
// Handles manifest func_def calls like "crawl(\"/var/data\")" and runtime expressions
// like "os.scandir(root)" where the base object is an ImportRef.
func (e *Executor) resolveIterableCall(expr string) ([]interface{}, error) {
	parenIdx := strings.Index(expr, "(")
	funcName := strings.TrimSpace(expr[:parenIdx])
	argsStr := strings.TrimSpace(expr[parenIdx+1 : len(expr)-1])

	// Check if funcName is a manifest func_def
	if fd, ok := e.funcs[funcName]; ok {
		var args []interface{}
		if argsStr != "" {
			for _, part := range strings.Split(argsStr, ",") {
				part = strings.TrimSpace(part)
				if val, ok := e.getBinding(part); ok {
					if ref, ok := val.(RuntimeRef); ok {
						args = append(args, ref.Value)
					} else {
						args = append(args, val)
					}
				} else {
					part = strings.Trim(part, "\"'")
					args = append(args, part)
				}
			}
		}

		// Execute inline with generator semantics (iterable context implies generator).
		// Push scope, bind args, push yield collector, execute body, collect yields.
		e.pushScope()
		for i, param := range fd.Params {
			if i < len(args) {
				e.setBinding(param.Name, args[i])
			} else if param.DefaultValue != nil {
				e.setBinding(param.Name, param.DefaultValue)
			} else {
				e.setBinding(param.Name, nil)
			}
		}
		e.yieldCollectors = append(e.yieldCollectors, []interface{}{})
		_, bodyErr := e.executeOps(fd.Body)
		top := len(e.yieldCollectors) - 1
		collected := e.yieldCollectors[top]
		e.yieldCollectors = e.yieldCollectors[:top]
		e.popScope()

		if bodyErr != nil {
			if _, isReturn := bodyErr.(ErrReturn); !isReturn {
				return nil, fmt.Errorf("resolveIterableCall %q: %w", funcName, bodyErr)
			}
		}
		return collected, nil
	}

	// Check if the base object (before ".") is an ImportRef — runtime expression
	dotIdx := strings.Index(funcName, ".")
	if dotIdx > 0 {
		baseName := funcName[:dotIdx]
		if val, ok := e.getBinding(baseName); ok {
			if ref, ok := val.(ImportRef); ok {
				return e.evalRuntimeIterable(ref.Runtime, expr)
			}
		}
	}

	return nil, fmt.Errorf("resolveIterableCall: cannot resolve %q", expr)
}

func (e *Executor) nativeGeneratorCallRuntimeRef(expr string) (RuntimeRef, bool, error) {
	parenIdx := strings.Index(expr, "(")
	if parenIdx <= 0 || !strings.HasSuffix(strings.TrimSpace(expr), ")") {
		return RuntimeRef{}, false, nil
	}
	funcName := strings.TrimSpace(expr[:parenIdx])
	fd, ok := e.funcs[funcName]
	if !ok || !fd.Generator || fd.NativeRuntime == "" {
		return RuntimeRef{}, false, nil
	}
	rt, ok := e.runtimes[fd.NativeRuntime]
	if !ok {
		return RuntimeRef{}, true, fmt.Errorf("native generator call %q: unknown runtime %q", funcName, fd.NativeRuntime)
	}

	e.nextRuntimeRefID++
	varName := fmt.Sprintf("__omnivm_native_generator_%d", e.nextRuntimeRefID)
	assignCode := runtimeAssign(fd.NativeRuntime, varName, expr)
	result := rt.Execute(assignCode)
	if result.Err != nil {
		return RuntimeRef{}, true, fmt.Errorf("native generator call %q [%s]: %w", funcName, fd.NativeRuntime, result.Err)
	}
	ref, _, err := e.boundRuntimeRefSnapshot(fd.NativeRuntime, varName)
	if err != nil {
		return RuntimeRef{}, true, fmt.Errorf("native generator call %q [%s]: %w", funcName, fd.NativeRuntime, err)
	}
	e.setBinding(varName, ref)
	stream, err := e.runtimeRefIsStream(ref)
	if err != nil {
		return RuntimeRef{}, true, fmt.Errorf("native generator call %q stream probe: %w", funcName, err)
	}
	if !stream {
		return RuntimeRef{}, false, nil
	}
	return ref, true, nil
}

// evalRuntimeIterable evaluates an expression in a source runtime and returns
// the result as a JSON-parsed array. Substitutes manifest bindings into the expression.
func (e *Executor) evalRuntimeIterable(rtName, expr string) (items []interface{}, err error) {
	rt, ok := e.runtimes[rtName]
	if !ok {
		return nil, fmt.Errorf("evalRuntimeIterable: unknown runtime %q", rtName)
	}

	// Auto-inject scope so the runtime can see manifest bindings
	injection := e.autoInjectScopePlan(rtName)
	if injection.setup != "" {
		injectResult := rt.Execute(injection.setup)
		if injectResult.Err != nil {
			return nil, fmt.Errorf("evalRuntimeIterable auto-inject [%s]: %w", rtName, injectResult.Err)
		}
		defer func() {
			if cleanupErr := e.runJavaCaptureCleanup(rt, injection); cleanupErr != nil {
				if err != nil {
					err = fmt.Errorf("%w (auto capture cleanup failed: %v)", err, cleanupErr)
					return
				}
				err = fmt.Errorf("evalRuntimeIterable auto capture cleanup [%s]: %w", rtName, cleanupErr)
			}
		}()
	}

	// Wrap expression to produce JSON array
	var code string
	switch rtName {
	case "python":
		code = fmt.Sprintf("__import__('json').dumps([{'name': e.name, 'path': e.path} if hasattr(e, 'name') and hasattr(e, 'path') else str(e) for e in %s])", expr)
	case "javascript":
		code = fmt.Sprintf("JSON.stringify(Array.from(%s))", expr)
	case "ruby":
		code = fmt.Sprintf("require 'json'; JSON.generate(%s.to_a)", expr)
	default:
		return nil, fmt.Errorf("evalRuntimeIterable: unsupported runtime %q", rtName)
	}

	result := rt.Eval(code)
	if result.Err != nil {
		return nil, fmt.Errorf("evalRuntimeIterable [%s]: %w", rtName, result.Err)
	}

	val := result.Value
	if val == nil {
		val = result.Output
	}

	jsonStr := fmt.Sprintf("%v", val)
	var arr []interface{}
	if err := json.Unmarshal([]byte(jsonStr), &arr); err != nil {
		return nil, fmt.Errorf("evalRuntimeIterable: parse JSON: %w", err)
	}
	return arr, nil
}

// marshalForCapture serializes a value to JSON for injection into a runtime.
func (e *Executor) marshalForCapture(val interface{}) (string, error) {
	if ref, ok, err := e.autoBulkTableRefForCapture(val); ok || err != nil {
		if err != nil {
			return "", err
		}
		val = ref
	}
	if ref, ok, err := e.autoResourceRefForCapture(val); ok || err != nil {
		if err != nil {
			return "", err
		}
		val = ref
	}
	decision := classifyLocalCaptureBoundary(val)
	switch decision.Form {
	case BoundaryRef:
		e.addBoundaryStat(func(stats *BoundaryStats) {
			switch val.(type) {
			case *ResourceRef:
				stats.ResourceProxyCaptures++
			case *JobHandle:
				stats.JobProxyCaptures++
			}
		})
	case BoundaryArrow:
		e.addBoundaryStat(func(stats *BoundaryStats) {
			stats.TableProxyCaptures++
			stats.ArrowTransfers++
		})
	}
	if _, ok := val.(*TableRef); ok && decision.Form != BoundaryArrow {
		e.addBoundaryStat(func(stats *BoundaryStats) {
			stats.TableProxyCaptures++
		})
	}
	return marshalForCapture(val)
}

func (e *Executor) autoResourceRefForCapture(val interface{}) (*ResourceRef, bool, error) {
	if !shouldProxyLocalCapture(val) {
		return nil, false, nil
	}
	if id, ok := e.bridgeHandleForValue("go", val); ok {
		entry, live := e.ensureHandleTable().Get(id)
		if live {
			if ref, ok := entry.Value.(*ResourceRef); ok {
				return ref, true, nil
			}
		}
	}

	ref := &ResourceRef{
		Runtime: "go",
		Kind:    bridgeResultKind(val),
		Value:   val,
	}
	var id handles.ID
	id, err := e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    ref.Kind,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			ref.Closed = true
			e.forgetReleasedHandle(id, ref)
			e.forgetReleasedHandle(id, val)
			if proxy, ok := val.(*cSharedObjectProxy); ok {
				return proxy.Release()
			}
			if proxy, ok := val.(cSharedObjectProxy); ok {
				return proxy.Release()
			}
			return nil
		},
	})
	if err != nil {
		return nil, true, err
	}
	ref.ID = id
	e.resources[id] = ref
	if ident, ok := bridgeIdentityForValue(val); ok {
		e.bridgeHandles[ident] = id
	}
	return ref, true, nil
}

func shouldProxyLocalCapture(val interface{}) bool {
	if val == nil || isBridgePrimitive(val) || isBridgeMarker(val) {
		return false
	}
	switch val.(type) {
	case *ResourceRef, ResourceRef, *TableRef, TableRef, *JobHandle, JobHandle, *ChanRef, ChanRef, *SpawnHandle, SpawnHandle, ImportRef, RuntimeRef, *RuntimeRef, []byte:
		return false
	}
	if isReceivableChannelValue(val) || isReaderStreamValue(val) {
		return false
	}
	rv := reflect.ValueOf(val)
	if !rv.IsValid() {
		return false
	}
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	switch rv.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct, reflect.Pointer, reflect.Func:
		if (rv.Kind() == reflect.Map || rv.Kind() == reflect.Slice || rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Func) && rv.IsNil() {
			return false
		}
		return true
	default:
		return false
	}
}

func (e *Executor) autoBulkTableRefForCapture(val interface{}) (*TableRef, bool, error) {
	view, ok := bulkCaptureViewForValue(val)
	if !ok {
		return nil, false, nil
	}
	e.nextRuntimeRefID++
	name := fmt.Sprintf("__omnivm_auto_buffer_%p_%d", e, e.nextRuntimeRefID)
	dtype := view.dtype
	meta := arrow.BufferMetadata{
		Dtype:       dtype,
		Format:      view.format,
		Shape:       view.shapeOrDefault(),
		Strides:     append([]int64(nil), view.strides...),
		ReadOnly:    true,
		Ownership:   "producer",
		MemorySpace: nonEmpty(view.memorySpace, "host"),
	}
	var buf *arrow.Buffer
	var err error
	if view.ptr != nil || view.bytes == nil {
		buf, err = arrow.GlobalStore().SetExternalWithMetadata(name, view.ptr, view.bytesLen, meta, view.release)
	} else {
		buf, err = arrow.GlobalStore().SetWithMetadata(name, view.bytes, meta)
	}
	if err != nil {
		return nil, true, err
	}
	storedMeta := buf.Metadata()
	ref := &TableRef{
		Runtime:   "go",
		Format:    "arrow_c_data",
		Ownership: "borrowed",
		Metadata: &TableMetadata{
			Dtype:       &dtype,
			ArrowFormat: view.format,
			Buffer:      name,
			Shape:       view.shapeOrDefault(),
			Strides:     append([]int64(nil), view.strides...),
			ReadOnly:    true,
			MemorySpace: storedMeta.MemorySpace,
		},
		Value: name,
	}
	var id handles.ID
	id, err = e.ensureHandleTable().Register(ref, handles.RegisterOptions{
		Runtime: ref.Runtime,
		Kind:    "table:" + ref.Format,
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			ref.Released = true
			e.forgetReleasedHandle(id, ref)
			return arrow.GlobalStore().Free(name)
		},
	})
	if err != nil {
		_ = arrow.GlobalStore().Free(name)
		return nil, true, err
	}
	ref.ID = id
	e.tables[id] = ref
	return ref, true, nil
}

type bulkCaptureView struct {
	bytes       []byte
	ptr         unsafe.Pointer
	bytesLen    int64
	elements    int64
	shape       []int64
	strides     []int64
	dtype       int32
	format      string
	memorySpace string
	release     func() error
}

func (v bulkCaptureView) shapeOrDefault() []int64 {
	if len(v.shape) > 0 {
		return append([]int64(nil), v.shape...)
	}
	return []int64{v.elements}
}

func bulkCaptureViewForValue(val interface{}) (bulkCaptureView, bool) {
	switch data := val.(type) {
	case *cSharedOwnedBuffer:
		if data == nil {
			return bulkCaptureView{}, false
		}
		return cSharedOwnedBufferBulkCaptureView(data), true
	case cSharedOwnedBuffer:
		return cSharedOwnedBufferBulkCaptureView(&data), true
	case []byte:
		view := bulkCaptureView{
			bytesLen: int64(len(data)),
			elements: int64(len(data)),
			shape:    []int64{int64(len(data))},
			dtype:    arrow.DtypeBytes,
			format:   "C",
			release: func() error {
				runtime.KeepAlive(data)
				return nil
			},
		}
		if len(data) > 0 {
			view.ptr = unsafe.Pointer(&data[0])
		}
		return view, true
	case []int8:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeI8, "c", 1)
	case []int16:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeI16, "s", 2)
	case []uint16:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeU16, "S", 2)
	case []int32:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeI32, "i", 4)
	case []uint32:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeU32, "I", 4)
	case []int64:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeI64, "l", 8)
	case []uint64:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeU64, "L", 8)
	case []float32:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeF32, "f", 4)
	case []float64:
		return fixedWidthSliceBulkCaptureView(data, arrow.DtypeF64, "g", 8)
	default:
		return reflectBulkCaptureViewForValue(val)
	}
}

func cSharedOwnedBufferBulkCaptureView(data *cSharedOwnedBuffer) bulkCaptureView {
	return bulkCaptureView{
		ptr:         data.ptr,
		bytesLen:    data.bytesLen,
		elements:    data.elements,
		shape:       append([]int64(nil), data.shape...),
		strides:     append([]int64(nil), data.strides...),
		dtype:       data.dtype,
		format:      data.format,
		memorySpace: data.memorySpace,
		release:     data.release,
	}
}

func fixedWidthSliceBulkCaptureView[T any](data []T, dtype int32, format string, elemSize int64) (bulkCaptureView, bool) {
	view := bulkCaptureView{
		bytesLen: int64(len(data)) * elemSize,
		elements: int64(len(data)),
		shape:    []int64{int64(len(data))},
		dtype:    dtype,
		format:   format,
		release: func() error {
			runtime.KeepAlive(data)
			return nil
		},
	}
	if len(data) > 0 {
		view.ptr = unsafe.Pointer(&data[0])
	}
	return view, true
}

func reflectBulkCaptureViewForValue(val interface{}) (bulkCaptureView, bool) {
	rv := reflect.ValueOf(val)
	if !rv.IsValid() {
		return bulkCaptureView{}, false
	}
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return bulkCaptureView{}, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return bulkCaptureView{}, false
		}
		elem := rv.Elem()
		if elem.Kind() == reflect.Array || elem.Kind() == reflect.Slice {
			return reflectSequentialBulkCaptureView(elem, val)
		}
		return bulkCaptureView{}, false
	}
	if rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice {
		return bulkCaptureView{}, false
	}
	return reflectSequentialBulkCaptureView(rv, val)
}

func reflectSequentialBulkCaptureView(rv reflect.Value, keepAlive interface{}) (bulkCaptureView, bool) {
	shape, elem, contiguous, ok := reflectSequentialShape(rv)
	if !ok {
		return bulkCaptureView{}, false
	}
	dtype, format, elemSize, ok := arrowFormatForGoElem(elem)
	if !ok {
		return bulkCaptureView{}, false
	}
	elements, ok := shapeProduct(shape)
	if !ok {
		return bulkCaptureView{}, false
	}
	view := bulkCaptureView{
		bytesLen: elements * elemSize,
		elements: elements,
		shape:    shape,
		strides:  contiguousStrides(shape, elemSize),
		dtype:    dtype,
		format:   format,
		release: func() error {
			runtime.KeepAlive(keepAlive)
			return nil
		},
	}
	if elements == 0 {
		return view, true
	}
	first := firstScalarValue(rv)
	if contiguous && first.CanAddr() {
		view.ptr = unsafe.Pointer(first.UnsafeAddr())
		return view, true
	}
	view.bytes = copyReflectSequentialBulkBytes(rv, dtype, int(elemSize), int(elements))
	view.release = nil
	return view, true
}

func reflectSequentialShape(rv reflect.Value) ([]int64, reflect.Type, bool, bool) {
	if rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice {
		return nil, nil, false, false
	}
	elem, ok := reflectSequentialScalarType(rv.Type())
	if !ok {
		return nil, nil, false, false
	}
	shape, contiguous, ok := reflectSequentialShapeDims(rv)
	if !ok || len(shape) == 0 {
		return nil, nil, false, false
	}
	return shape, elem, contiguous, true
}

func reflectSequentialScalarType(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Array || t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	if t.Kind() == reflect.Array || t.Kind() == reflect.Slice {
		return nil, false
	}
	return t, true
}

func reflectSequentialShapeDims(rv reflect.Value) ([]int64, bool, bool) {
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return nil, false, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Array && rv.Kind() != reflect.Slice {
		return nil, true, true
	}
	shape := []int64{int64(rv.Len())}
	contiguous := rv.Type().Elem().Kind() != reflect.Slice
	if rv.Len() == 0 {
		inner, innerContiguous, ok := reflectSequentialTypeFixedShape(rv.Type().Elem())
		if !ok {
			return shape, contiguous, true
		}
		shape = append(shape, inner...)
		return shape, contiguous && innerContiguous, true
	}
	var expected []int64
	for i := 0; i < rv.Len(); i++ {
		inner, innerContiguous, ok := reflectSequentialShapeDims(rv.Index(i))
		if !ok {
			return nil, false, false
		}
		if i == 0 {
			expected = append([]int64(nil), inner...)
		} else if !int64SlicesEqual(expected, inner) {
			return nil, false, false
		}
		contiguous = contiguous && innerContiguous
	}
	shape = append(shape, expected...)
	return shape, contiguous, true
}

func reflectSequentialTypeFixedShape(t reflect.Type) ([]int64, bool, bool) {
	switch t.Kind() {
	case reflect.Array:
		inner, contiguous, ok := reflectSequentialTypeFixedShape(t.Elem())
		if !ok {
			return nil, false, false
		}
		return append([]int64{int64(t.Len())}, inner...), contiguous, true
	case reflect.Slice:
		return nil, false, false
	default:
		return nil, true, true
	}
}

func int64SlicesEqual(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func arrowFormatForGoElem(elem reflect.Type) (int32, string, int64, bool) {
	switch elem.Kind() {
	case reflect.Int8:
		return arrow.DtypeI8, "c", 1, true
	case reflect.Uint8:
		return arrow.DtypeU8, "C", 1, true
	case reflect.Int16:
		return arrow.DtypeI16, "s", 2, true
	case reflect.Uint16:
		return arrow.DtypeU16, "S", 2, true
	case reflect.Int32:
		return arrow.DtypeI32, "i", 4, true
	case reflect.Uint32:
		return arrow.DtypeU32, "I", 4, true
	case reflect.Int64:
		return arrow.DtypeI64, "l", 8, true
	case reflect.Uint64:
		return arrow.DtypeU64, "L", 8, true
	case reflect.Float32:
		return arrow.DtypeF32, "f", 4, true
	case reflect.Float64:
		return arrow.DtypeF64, "g", 8, true
	case reflect.Int:
		if elem.Size() == 4 {
			return arrow.DtypeI32, "i", 4, true
		}
		if elem.Size() == 8 {
			return arrow.DtypeI64, "l", 8, true
		}
	case reflect.Uint:
		if elem.Size() == 4 {
			return arrow.DtypeU32, "I", 4, true
		}
		if elem.Size() == 8 {
			return arrow.DtypeU64, "L", 8, true
		}
	}
	return 0, "", 0, false
}

func firstScalarValue(rv reflect.Value) reflect.Value {
	for rv.Kind() == reflect.Array || rv.Kind() == reflect.Slice {
		if rv.Len() == 0 {
			return reflect.Value{}
		}
		rv = rv.Index(0)
	}
	return rv
}

func copyReflectSequentialBulkBytes(rv reflect.Value, dtype int32, elemSize, elements int) []byte {
	out := make([]byte, elements*elemSize)
	offset := 0
	copyReflectSequentialBulkBytesInto(rv, dtype, elemSize, out, &offset)
	return out
}

func copyReflectSequentialBulkBytesInto(rv reflect.Value, dtype int32, elemSize int, out []byte, offset *int) {
	for rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	if rv.Kind() == reflect.Array || rv.Kind() == reflect.Slice {
		for i := 0; i < rv.Len(); i++ {
			copyReflectSequentialBulkBytesInto(rv.Index(i), dtype, elemSize, out, offset)
		}
		return
	}
	if *offset < 0 || *offset+elemSize > len(out) {
		return
	}
	slot := out[*offset : *offset+elemSize]
	switch dtype {
	case arrow.DtypeBytes:
		slot[0] = byte(rv.Uint())
	case arrow.DtypeI8:
		slot[0] = byte(int8(rv.Int()))
	case arrow.DtypeU8:
		slot[0] = byte(rv.Uint())
	case arrow.DtypeI16:
		binary.LittleEndian.PutUint16(slot, uint16(int16(rv.Int())))
	case arrow.DtypeU16:
		binary.LittleEndian.PutUint16(slot, uint16(rv.Uint()))
	case arrow.DtypeI32:
		binary.LittleEndian.PutUint32(slot, uint32(int32(rv.Int())))
	case arrow.DtypeU32:
		binary.LittleEndian.PutUint32(slot, uint32(rv.Uint()))
	case arrow.DtypeI64:
		binary.LittleEndian.PutUint64(slot, uint64(rv.Int()))
	case arrow.DtypeU64:
		binary.LittleEndian.PutUint64(slot, rv.Uint())
	case arrow.DtypeF32:
		binary.LittleEndian.PutUint32(slot, math.Float32bits(float32(rv.Float())))
	case arrow.DtypeF64:
		binary.LittleEndian.PutUint64(slot, math.Float64bits(rv.Float()))
	}
	*offset += elemSize
}

func shapeProduct(shape []int64) (int64, bool) {
	product := int64(1)
	for _, dim := range shape {
		if dim < 0 {
			return 0, false
		}
		if dim == 0 {
			return 0, true
		}
		if product > math.MaxInt64/dim {
			return 0, false
		}
		product *= dim
	}
	return product, true
}

func contiguousStrides(shape []int64, elemSize int64) []int64 {
	if len(shape) == 0 {
		return nil
	}
	strides := make([]int64, len(shape))
	stride := elemSize
	for i := len(shape) - 1; i >= 0; i-- {
		strides[i] = stride
		if shape[i] == 0 {
			stride = 0
			continue
		}
		if stride <= math.MaxInt64/shape[i] {
			stride *= shape[i]
		}
	}
	return strides
}

// marshalForCapture serializes a value to JSON for injection into a runtime.
func marshalForCapture(val interface{}) (string, error) {
	switch v := val.(type) {
	case *ResourceRef:
		if v == nil {
			return "null", nil
		}
		return marshalResourceProxy(v)
	case ResourceRef:
		return marshalResourceProxy(&v)
	case *TableRef:
		if v == nil {
			return "null", nil
		}
		return marshalTableProxy(v)
	case TableRef:
		return marshalTableProxy(&v)
	case *JobHandle:
		if v == nil {
			return "null", nil
		}
		return marshalJobProxy(v)
	case JobHandle:
		return marshalJobProxy(&v)
	}
	if !isBridgePrimitive(val) && !isBridgeMarker(val) {
		return "", fmt.Errorf("capture value %T is not a primitive or bridge descriptor; boundary classification must wrap complex values as Arrow, stream, or proxy handles", val)
	}
	b, err := json.Marshal(val)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func marshalResourceProxy(ref *ResourceRef) (string, error) {
	b, err := json.Marshal(resourceProxyValue(ref))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func resourceProxyValue(ref *ResourceRef) map[string]interface{} {
	return map[string]interface{}{
		"__omnivm_resource__": true,
		"id":                  ref.ID,
		"runtime":             ref.Runtime,
		"kind":                ref.Kind,
		"disposer":            ref.Disposer,
		"closed":              ref.Closed,
	}
}

func transferResourceProxyValue(ref *ResourceRef) map[string]interface{} {
	value := resourceProxyValue(ref)
	value["transfer"] = true
	return value
}

func marshalTableProxy(ref *TableRef) (string, error) {
	b, err := json.Marshal(tableProxyValue(ref))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func tableProxyValue(ref *TableRef) map[string]interface{} {
	metadata := cloneTableMetadata(ref.Metadata)
	fillTableMetadataMemorySpace(metadata)
	value := map[string]interface{}{
		"__omnivm_table__": true,
		"id":               ref.ID,
		"runtime":          ref.Runtime,
		"format":           ref.Format,
		"ownership":        ref.Ownership,
		"release":          ref.Release,
		"metadata":         metadata,
		"released":         ref.Released,
	}
	if name, ok := ref.Value.(string); ok && name != "" {
		value["buffer"] = name
	}
	return value
}

func tableMetadataValue(meta *TableMetadata) map[string]interface{} {
	if meta == nil {
		return nil
	}
	out := make(map[string]interface{})
	if meta.Dtype != nil {
		out["dtype"] = *meta.Dtype
	}
	if meta.ArrowFormat != "" {
		out["arrow_format"] = meta.ArrowFormat
	}
	if meta.Buffer != "" {
		out["buffer"] = meta.Buffer
	}
	if len(meta.Shape) > 0 {
		out["shape"] = append([]int64(nil), meta.Shape...)
	}
	if len(meta.Strides) > 0 {
		out["strides"] = append([]int64(nil), meta.Strides...)
	}
	if meta.Offset != 0 {
		out["offset"] = meta.Offset
	}
	if meta.NullCount != nil {
		out["null_count"] = *meta.NullCount
	}
	out["read_only"] = meta.ReadOnly
	if meta.MemorySpace != "" {
		out["memory_space"] = meta.MemorySpace
	}
	return out
}

func transferTableProxyValue(ref *TableRef) map[string]interface{} {
	value := tableProxyValue(ref)
	value["transfer"] = true
	return value
}

func cloneTableMetadata(meta *TableMetadata) *TableMetadata {
	if meta == nil {
		return nil
	}
	clone := *meta
	clone.Shape = append([]int64(nil), meta.Shape...)
	clone.Strides = append([]int64(nil), meta.Strides...)
	if meta.Dtype != nil {
		dtype := *meta.Dtype
		clone.Dtype = &dtype
	}
	if meta.NullCount != nil {
		nullCount := *meta.NullCount
		clone.NullCount = &nullCount
	}
	return &clone
}

func fillTableMetadataMemorySpace(meta *TableMetadata) {
	if meta == nil || meta.MemorySpace != "" || meta.Buffer == "" {
		return
	}
	if status := arrow.GlobalStore().Status(meta.Buffer); status.MemorySpace != "" {
		meta.MemorySpace = status.MemorySpace
	}
}

func marshalJobProxy(job *JobHandle) (string, error) {
	b, err := json.Marshal(jobProxyValue(job))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func jobProxyValue(job *JobHandle) map[string]interface{} {
	return map[string]interface{}{
		"__omnivm_job__": true,
		"id":             job.ID,
		"runtime":        job.Runtime,
		"kind":           job.Kind,
		"done":           job.Done,
		"cancelled":      job.Cancelled,
		"cancelReason":   job.CancelReason,
		"payload":        job.Payload,
		"result":         job.Result,
	}
}
