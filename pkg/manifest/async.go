package manifest

import (
	"fmt"
	"time"
)

// execAsync handles exec ops with async:true.
// For Python, wraps in asyncio; for JS, wraps for Promise handling.
// Ruby and Java execute synchronously (no cooperative event loop).
func (e *Executor) execAsync(op *Op) (interface{}, error) {
	rt, err := e.resolveRuntime(op)
	if err != nil {
		return nil, err
	}

	switch rt.Name() {
	case "python":
		return e.execAsyncPython(op)
	case "javascript":
		return e.execAsyncJS(op)
	default:
		// Fall back to synchronous execution
		op.Async = false
		return e.opExec(op)
	}
}

// evalAsync handles eval ops with async:true.
func (e *Executor) evalAsync(op *Op) (interface{}, error) {
	rt, err := e.resolveRuntime(op)
	if err != nil {
		return nil, err
	}

	switch rt.Name() {
	case "python":
		return e.evalAsyncPython(op)
	case "javascript":
		return e.evalAsyncJS(op)
	default:
		op.Async = false
		return e.opEval(op)
	}
}

// execAsyncPython schedules a Python coroutine via asyncio and pumps until done.
func (e *Executor) execAsyncPython(op *Op) (interface{}, error) {
	rt := e.runtimes["python"]

	code := op.Code
	if len(op.Captures) > 0 {
		var err error
		code, err = e.wrapWithCaptures("python", op.Code, op.Captures)
		if err != nil {
			return nil, err
		}
	}

	// Schedule the coroutine and set a completion flag
	wrapper := fmt.Sprintf(`
import asyncio as __aio
__omni_async_done = False
__omni_async_result = None
async def __omni_async_task():
    global __omni_async_done, __omni_async_result
    %s
    __omni_async_done = True
__omni_loop = __aio.get_event_loop() if __aio.get_event_loop().is_running() else __aio.new_event_loop()
__aio.ensure_future(__omni_async_task(), loop=__omni_loop)
`, indentCode(code, "    "))

	result := rt.Execute(wrapper)
	if result.Err != nil {
		return nil, fmt.Errorf("async exec [python]: %w", result.Err)
	}

	// Pump until the flag is set
	e.pumpUntilDone(func() bool {
		check := rt.Eval("__omni_async_done")
		return check.Value != nil && fmt.Sprintf("%v", check.Value) == "True"
	})

	output := result.Output
	if op.Bind != "" {
		e.setBinding(op.Bind, output)
	}
	return output, nil
}

// evalAsyncPython evaluates an async Python expression and pumps until done.
func (e *Executor) evalAsyncPython(op *Op) (interface{}, error) {
	rt := e.runtimes["python"]

	code := op.Code
	if len(op.Captures) > 0 {
		var err error
		code, err = e.wrapWithCaptures("python", op.Code, op.Captures)
		if err != nil {
			return nil, err
		}
	}

	wrapper := fmt.Sprintf(`
import asyncio as __aio
__omni_async_done = False
__omni_async_result = None
async def __omni_async_eval():
    global __omni_async_done, __omni_async_result
    __omni_async_result = %s
    __omni_async_done = True
__omni_loop = __aio.get_event_loop() if __aio.get_event_loop().is_running() else __aio.new_event_loop()
__aio.ensure_future(__omni_async_eval(), loop=__omni_loop)
`, code)

	result := rt.Execute(wrapper)
	if result.Err != nil {
		return nil, fmt.Errorf("async eval [python]: %w", result.Err)
	}

	e.pumpUntilDone(func() bool {
		check := rt.Eval("__omni_async_done")
		return check.Value != nil && fmt.Sprintf("%v", check.Value) == "True"
	})

	valResult := rt.Eval("__omni_async_result")
	val := valResult.Value
	if op.Bind != "" {
		e.setBinding(op.Bind, val)
	}
	return val, nil
}

// execAsyncJS executes async JS code and pumps until Promises resolve.
func (e *Executor) execAsyncJS(op *Op) (interface{}, error) {
	rt := e.runtimes["javascript"]

	code := op.Code
	if len(op.Captures) > 0 {
		var err error
		code, err = e.wrapWithCaptures("javascript", op.Code, op.Captures)
		if err != nil {
			return nil, err
		}
	}

	// Wrap in a Promise that sets a global flag on completion
	wrapper := fmt.Sprintf(`
globalThis.__omni_async_done = false;
globalThis.__omni_async_result = undefined;
(async function() {
  %s
  globalThis.__omni_async_done = true;
})().catch(function(e) { globalThis.__omni_async_done = true; });
`, code)

	result := rt.Execute(wrapper)
	if result.Err != nil {
		return nil, fmt.Errorf("async exec [javascript]: %w", result.Err)
	}

	e.pumpUntilDone(func() bool {
		check := rt.Eval("globalThis.__omni_async_done")
		return check.Value != nil && fmt.Sprintf("%v", check.Value) == "true"
	})

	output := result.Output
	if op.Bind != "" {
		e.setBinding(op.Bind, output)
	}
	return output, nil
}

// evalAsyncJS evaluates an async JS expression and pumps until the Promise resolves.
func (e *Executor) evalAsyncJS(op *Op) (interface{}, error) {
	rt := e.runtimes["javascript"]

	code := op.Code
	if len(op.Captures) > 0 {
		var err error
		code, err = e.wrapWithCaptures("javascript", op.Code, op.Captures)
		if err != nil {
			return nil, err
		}
	}

	wrapper := fmt.Sprintf(`
globalThis.__omni_async_done = false;
globalThis.__omni_async_result = undefined;
Promise.resolve(%s).then(function(v) {
  globalThis.__omni_async_result = v;
  globalThis.__omni_async_done = true;
}).catch(function(e) {
  globalThis.__omni_async_result = "ERR:" + e.message;
  globalThis.__omni_async_done = true;
});
`, code)

	result := rt.Execute(wrapper)
	if result.Err != nil {
		return nil, fmt.Errorf("async eval [javascript]: %w", result.Err)
	}

	e.pumpUntilDone(func() bool {
		check := rt.Eval("globalThis.__omni_async_done")
		return check.Value != nil && fmt.Sprintf("%v", check.Value) == "true"
	})

	valResult := rt.Eval("globalThis.__omni_async_result")
	val := valResult.Value
	if op.Bind != "" {
		e.setBinding(op.Bind, val)
	}
	return val, nil
}

// opParallel executes branches cooperatively.
// Async-capable runtimes (Python, JS) start tasks as futures/promises,
// then we pump until all complete. Sync runtimes execute sequentially.
func (e *Executor) opParallel(op *Op) (interface{}, error) {
	if len(op.Branches) == 0 {
		return nil, nil
	}

	// Separate branches by async capability
	type asyncBranch struct {
		op      *Op
		flagVar string
		idx     int // original branch index for result variable naming
	}

	var asyncBranches []asyncBranch
	branchIdx := 0

	for _, branch := range op.Branches {
		rtName := branch.Runtime
		if rtName == "" {
			rtName = e.defaultRuntime
		}

		switch rtName {
		case "python", "javascript":
			flagVar := fmt.Sprintf("__omni_parallel_%d_done", branchIdx)
			if err := e.startAsyncBranch(branch, rtName, flagVar, branchIdx); err != nil {
				return nil, fmt.Errorf("parallel branch %d: %w", branchIdx, err)
			}
			asyncBranches = append(asyncBranches, asyncBranch{op: branch, flagVar: flagVar, idx: branchIdx})
		default:
			// Execute synchronously — branches are implicit eval ops
			if branch.OpType == "" {
				branch.OpType = "eval"
			}
			if _, err := e.executeOp(branch); err != nil {
				return nil, fmt.Errorf("parallel branch %d [%s]: %w", branchIdx, rtName, err)
			}
		}
		branchIdx++
	}

	// Pump until all async branches complete
	if len(asyncBranches) > 0 {
		e.pumpUntilDone(func() bool {
			for _, ab := range asyncBranches {
				rtName := ab.op.Runtime
				if rtName == "" {
					rtName = e.defaultRuntime
				}
				rt := e.runtimes[rtName]

				var checkCode string
				switch rtName {
				case "python":
					checkCode = ab.flagVar
				case "javascript":
					checkCode = "globalThis." + ab.flagVar
				}

				check := rt.Eval(checkCode)
				val := fmt.Sprintf("%v", check.Value)
				if val != "True" && val != "true" {
					return false
				}
			}
			return true
		})

		// Collect results and bind
		for _, ab := range asyncBranches {
			if ab.op.Bind == "" {
				continue
			}
			rtName := ab.op.Runtime
			if rtName == "" {
				rtName = e.defaultRuntime
			}
			rt := e.runtimes[rtName]

			resultVar := fmt.Sprintf("__omni_parallel_%d_result", ab.idx)
			var evalCode string
			switch rtName {
			case "python":
				evalCode = resultVar
			case "javascript":
				evalCode = "globalThis." + resultVar
			}

			res := rt.Eval(evalCode)
			e.setBinding(ab.op.Bind, res.Value)
		}
	}

	return nil, nil
}

// startAsyncBranch starts an async task for a parallel branch.
func (e *Executor) startAsyncBranch(branch *Op, rtName, flagVar string, idx int) error {
	rt := e.runtimes[rtName]
	resultVar := fmt.Sprintf("__omni_parallel_%d_result", idx)

	// Auto-inject scope bindings so branch code can reference manifest variables
	autoCode := e.autoInjectScope(rtName)
	if autoCode != "" {
		injectResult := rt.Execute(autoCode)
		if injectResult.Err != nil {
			return fmt.Errorf("parallel auto-inject [%s]: %w", rtName, injectResult.Err)
		}
	}

	code := branch.Code
	if len(branch.Captures) > 0 {
		var err error
		code, err = e.wrapWithCaptures(rtName, branch.Code, branch.Captures)
		if err != nil {
			return err
		}
	}

	var wrapper string
	switch rtName {
	case "python":
		wrapper = fmt.Sprintf(`
import asyncio as __aio
%s = False
%s = None
async def __omni_parallel_task_%d():
    global %s, %s
    %s = %s
    %s = True
__omni_loop = __aio.get_event_loop() if __aio.get_event_loop().is_running() else __aio.new_event_loop()
__aio.ensure_future(__omni_parallel_task_%d(), loop=__omni_loop)
`, flagVar, resultVar, idx, flagVar, resultVar, resultVar, code, flagVar, idx)

	case "javascript":
		wrapper = fmt.Sprintf(`
globalThis.%s = false;
globalThis.%s = undefined;
Promise.resolve(%s).then(function(v) {
  globalThis.%s = v;
  globalThis.%s = true;
}).catch(function(e) {
  globalThis.%s = "ERR:" + e.message;
  globalThis.%s = true;
});
`, flagVar, resultVar, code, resultVar, flagVar, resultVar, flagVar)
	}

	result := rt.Execute(wrapper)
	if result.Err != nil {
		return result.Err
	}
	return nil
}

// pumpUntilDone calls Pump() on all runtimes until checkDone returns true.
func (e *Executor) pumpUntilDone(checkDone func() bool) {
	const maxPumps = 30000 // 30 seconds at 1ms/pump
	for i := 0; i < maxPumps; i++ {
		if checkDone() {
			return
		}
		for _, rt := range e.runtimes {
			rt.Pump()
		}
		time.Sleep(1 * time.Millisecond)
	}
}

// indentCode adds a prefix to each line of code (for embedding in wrappers).
func indentCode(code, prefix string) string {
	lines := splitLines(code)
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return joinLines(lines)
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func joinLines(lines []string) string {
	result := ""
	for i, line := range lines {
		if i > 0 {
			result += "\n"
		}
		result += line
	}
	return result
}
