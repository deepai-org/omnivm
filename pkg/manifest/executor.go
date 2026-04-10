package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/omnivm/omnivm/pkg"
)

// ErrReturn is a sentinel used to unwind the call stack on return ops.
type ErrReturn struct{ Value interface{} }

func (e ErrReturn) Error() string { return "return" }

// ErrThrow is a sentinel used for manifest-level throw ops.
type ErrThrow struct{ Value interface{} }

func (e ErrThrow) Error() string { return fmt.Sprintf("throw: %v", e.Value) }

// ImportRef marks a binding as a module import in a specific runtime.
// Captures for the same runtime skip JSON injection since the module
// is already in scope. Cross-runtime captures skip module refs entirely.
type ImportRef struct {
	Runtime string // which runtime owns this import
	Name    string // module name (e.g. "os", "re", "express")
}

// RuntimeRef marks a binding as a variable that lives in a specific runtime's
// global scope. Captures for the same runtime skip injection (already in scope).
// Cross-runtime captures serialize the value via JSON.
type RuntimeRef struct {
	Runtime  string      // which runtime owns this variable
	VarName  string      // variable name in that runtime
	Value    interface{} // last known value (for cross-runtime capture)
}

// FuncDef stores a manifest-level function definition.
type FuncDef struct {
	Name      string
	Params    []*Param
	Body      []*Op
	Generator bool
}

// Executor runs manifest ops against a set of runtimes.
type Executor struct {
	runtimes        map[string]pkg.Runtime
	defaultRuntime  string
	scopes          []map[string]interface{}
	funcs           map[string]*FuncDef
	goFuncs         map[string]interface{}
	yieldCollectors [][]interface{} // stack of yield collectors for nested generators
}

// NewExecutor creates an Executor with the given runtimes.
func NewExecutor(runtimes map[string]pkg.Runtime) *Executor {
	e := &Executor{
		runtimes: runtimes,
		scopes:   []map[string]interface{}{make(map[string]interface{})},
		funcs:    make(map[string]*FuncDef),
		goFuncs:  make(map[string]interface{}),
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

	if pyRT, ok := e.runtimes["python"]; ok {
		pyRT.Execute(`
import types as _t, sys as _s
if 'requests' not in _s.modules:
    _m = _t.ModuleType('requests')
    _r = type('Response', (), {'status_code': 200, 'text': 'mock', 'json': lambda self: {'data': 'mock'}})
    _m.get = lambda url, **kw: _r()
    _m.post = lambda url, **kw: _r()
    _s.modules['requests'] = _m
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
func (e *Executor) Execute(m *Manifest) error {
	e.defaultRuntime = m.DefaultRuntime
	e.setupRuntimeBuiltins()
	_, err := e.executeOps(m.Ops)
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
		lastVal = val
	}
	return lastVal, nil
}

// executeOp dispatches a single op by type.
func (e *Executor) executeOp(op *Op) (interface{}, error) {
	switch op.OpType {
	case "exec":
		return e.opExec(op)
	case "eval":
		return e.opEval(op)
	case "native":
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
	case "exec_compiled":
		return e.opExecCompiled(op)
	case "eval_compiled":
		return e.opEvalCompiled(op)
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

// opExec handles exec and native ops.
func (e *Executor) opExec(op *Op) (interface{}, error) {
	if op.Async {
		return e.execAsync(op)
	}

	rt, err := e.resolveRuntime(op)
	if err != nil {
		return nil, err
	}

	// Auto-inject current scope bindings so exec code can
	// reference manifest variables without explicit captures.
	autoCode := e.autoInjectScope(rt.Name())
	if autoCode != "" {
		injectResult := rt.Execute(autoCode)
		if injectResult.Err != nil {
			return nil, fmt.Errorf("exec auto-inject [%s]: %w", rt.Name(), injectResult.Err)
		}
	}

	code := op.Code
	if len(op.Captures) > 0 {
		code, err = e.wrapWithCaptures(rt.Name(), op.Code, op.Captures)
		if err != nil {
			return nil, fmt.Errorf("exec captures: %w", err)
		}
	}

	// Convert Python f-strings to JS template literals when targeting JavaScript
	if rt.Name() == "javascript" {
		code = convertFStringToTemplateLiteral(code)
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

// opEval handles eval ops.
func (e *Executor) opEval(op *Op) (interface{}, error) {
	// Go function call via goFuncs registry
	if op.Func != "" && (op.Runtime == "go" || op.Runtime == "") {
		return e.callGoFunc(op.Func, op.Args, op.Bind)
	}

	// runtime:"go" with code — parse as function call expression
	if op.Runtime == "go" && op.Code != "" {
		return e.evalGoCode(op)
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
	autoCode := e.autoInjectScope(rt.Name())
	if autoCode != "" {
		injectResult := rt.Execute(autoCode)
		if injectResult.Err != nil {
			return nil, fmt.Errorf("eval auto-inject [%s]: %w", rt.Name(), injectResult.Err)
		}
	}

	// Inject explicit captures (overrides auto-injected values)
	if len(op.Captures) > 0 {
		captureCode := e.buildCaptureInjection(rt.Name(), op.Captures)
		if captureCode != "" {
			injectResult := rt.Execute(captureCode)
			if injectResult.Err != nil {
				return nil, fmt.Errorf("eval captures [%s]: %w", rt.Name(), injectResult.Err)
			}
		}
	}

	code := op.Code

	// If bind is set, persist the result in the runtime's global scope
	// so subsequent ops in the same runtime can reference it directly.
	if op.Bind != "" {
		assignCode := runtimeAssign(rt.Name(), op.Bind, code)
		execResult := rt.Execute(assignCode)
		if execResult.Err != nil {
			return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), execResult.Err)
		}

		// Read back the value
		valResult := rt.Eval(runtimeVarRef(rt.Name(), op.Bind))
		val := valResult.Value
		if val == nil {
			val = valResult.Output
		}

		ref := RuntimeRef{Runtime: rt.Name(), VarName: op.Bind, Value: val}
		e.setBinding(op.Bind, ref)
		return val, nil
	}

	result := rt.Eval(code)
	if result.Err != nil {
		return nil, fmt.Errorf("eval [%s]: %w", rt.Name(), result.Err)
	}

	val := result.Value
	if val == nil {
		val = result.Output
	}

	return val, nil
}

// runtimeAssign generates code to assign a value to a global variable.
func runtimeAssign(rtName, varName, expr string) string {
	switch rtName {
	case "javascript":
		return fmt.Sprintf("globalThis.%s = %s;", varName, expr)
	case "python":
		return fmt.Sprintf("%s = %s", varName, expr)
	case "ruby":
		return fmt.Sprintf("$%s = %s", varName, expr)
	default:
		return fmt.Sprintf("%s = %s", varName, expr)
	}
}

// runtimeVarRef generates code to reference a variable in a runtime.
func runtimeVarRef(rtName, varName string) string {
	switch rtName {
	case "javascript":
		return fmt.Sprintf("globalThis.%s", varName)
	case "ruby":
		return fmt.Sprintf("$%s", varName)
	default:
		return varName
	}
}

// opImport generates runtime-specific import code and executes it.
func (e *Executor) opImport(op *Op) (interface{}, error) {
	rt, err := e.resolveRuntime(op)
	if err != nil {
		return nil, err
	}

	var code string
	switch rt.Name() {
	case "python":
		if len(op.Specifiers) > 0 {
			var specs []string
			for _, s := range op.Specifiers {
				if s.Imported == s.Local {
					specs = append(specs, s.Local)
				} else {
					specs = append(specs, s.Imported+" as "+s.Local)
				}
			}
			code = fmt.Sprintf("from %s import %s", op.Path, strings.Join(specs, ", "))
		} else if op.DefaultImport != "" {
			if op.DefaultImport == op.Path {
				code = fmt.Sprintf("import %s", op.Path)
			} else {
				code = fmt.Sprintf("import %s as %s", op.Path, op.DefaultImport)
			}
		} else {
			code = fmt.Sprintf("import %s", op.Path)
		}
	case "javascript":
		if len(op.Specifiers) > 0 {
			var specs []string
			for _, s := range op.Specifiers {
				if s.Imported == s.Local {
					specs = append(specs, s.Local)
				} else {
					specs = append(specs, s.Imported+": "+s.Local)
				}
			}
			code = fmt.Sprintf("var { %s } = require('%s');", strings.Join(specs, ", "), op.Path)
		} else if op.DefaultImport != "" {
			code = fmt.Sprintf("var %s = require('%s');", op.DefaultImport, op.Path)
		} else if op.Bind != "" {
			code = fmt.Sprintf("var %s = require('%s');", op.Bind, op.Path)
		} else {
			code = fmt.Sprintf("require('%s');", op.Path)
		}
	case "ruby":
		code = fmt.Sprintf("require '%s'", op.Path)
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

	fd := &FuncDef{
		Name:      op.Name,
		Params:    op.Params,
		Body:      op.Body,
		Generator: op.Generator,
	}
	e.funcs[op.Name] = fd

	// Register stubs in each available runtime
	if err := e.registerStubs(fd); err != nil {
		return nil, fmt.Errorf("func_def %q stubs: %w", op.Name, err)
	}
	return nil, nil
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
			// Unwrap RuntimeRef to get the actual value
			if ref, ok := v.(RuntimeRef); ok {
				val = ref.Value
			} else {
				val = v
			}
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
			return nil, err
		}
	}
	return nil, fmt.Errorf("loop exceeded %d iterations", maxIterations)
}

// opLoopForeach implements foreach loops over an iterable binding.
func (e *Executor) opLoopForeach(op *Op) (interface{}, error) {
	// Resolve the iterable
	var collection []interface{}
	switch op.Iterable.Kind {
	case "ref":
		val, ok := e.getBinding(op.Iterable.Name)
		if !ok {
			// Check if the ref is a function call expression like "crawl(\"/var/data\")" or "os.scandir(root)"
			if strings.Contains(op.Iterable.Name, "(") {
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
			val = ref.Value
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
			return nil, err
		}
	}
	return nil, nil
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

// opTry executes body, catches thrown/runtime errors, and runs finally.
func (e *Executor) opTry(op *Op) (interface{}, error) {
	val, bodyErr := e.executeOps(op.Body)

	if bodyErr != nil {
		// ErrReturn is control flow — never caught, re-propagate
		if _, ok := bodyErr.(ErrReturn); ok {
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
				errVal = bodyErr.Error()
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
func (e *Executor) evalCondition(cond *CondExpr) (bool, error) {
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
			captureCode := e.autoInjectScope(rtName)
			if captureCode != "" {
				injectResult := rt.Execute(captureCode)
				if injectResult.Err != nil {
					return false, fmt.Errorf("condition auto-inject [%s]: %w", rtName, injectResult.Err)
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

	normalizedArgs := normalizeArgs(args)

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
					// Unwrap RuntimeRef to get the actual value
					if ref, ok := val.(RuntimeRef); ok {
						val = ref.Value
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

// opAwait executes the inner from op and binds the result.
// In the current single-threaded executor, this is a passthrough —
// the await semantics are preserved in the IR for future async runtimes.
func (e *Executor) opAwait(op *Op) (interface{}, error) {
	if op.From == nil {
		return nil, nil
	}
	val, err := e.executeOp(op.From)
	if err != nil {
		return nil, err
	}
	if op.Bind != "" {
		e.setBinding(op.Bind, val)
	}
	return val, nil
}

// Scope operations

func (e *Executor) pushScope() {
	e.scopes = append(e.scopes, make(map[string]interface{}))
}

func (e *Executor) popScope() {
	if len(e.scopes) > 1 {
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

// Helpers

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
		for {
			idx := strings.Index(result, prefix)
			if idx < 0 {
				break
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

// evalRuntimeIterable evaluates an expression in a source runtime and returns
// the result as a JSON-parsed array. Substitutes manifest bindings into the expression.
func (e *Executor) evalRuntimeIterable(rtName, expr string) ([]interface{}, error) {
	rt, ok := e.runtimes[rtName]
	if !ok {
		return nil, fmt.Errorf("evalRuntimeIterable: unknown runtime %q", rtName)
	}

	// Auto-inject scope so the runtime can see manifest bindings
	autoCode := e.autoInjectScope(rtName)
	if autoCode != "" {
		injectResult := rt.Execute(autoCode)
		if injectResult.Err != nil {
			return nil, fmt.Errorf("evalRuntimeIterable auto-inject [%s]: %w", rtName, injectResult.Err)
		}
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
func marshalForCapture(val interface{}) (string, error) {
	b, err := json.Marshal(val)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
