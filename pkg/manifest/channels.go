package manifest

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/omnivm/omnivm/pkg/handles"
)

// ChanRef wraps a Go channel for use as a manifest binding.
type ChanRef struct {
	mu     sync.Mutex
	ch     chan interface{}
	closed bool
}

func (ch *ChanRef) sendNonBlocking(val interface{}) bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return false
	}
	select {
	case ch.ch <- val:
		return true
	default:
		return false
	}
}

func (ch *ChanRef) close() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.closed {
		return fmt.Errorf("already closed")
	}
	close(ch.ch)
	ch.closed = true
	return nil
}

func (ch *ChanRef) isClosed() bool {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	return ch.closed
}

func (ch *ChanRef) recvStreamValue() (interface{}, bool) {
	select {
	case v, ok := <-ch.ch:
		if !ok {
			return nil, true
		}
		return v, false
	default:
		return nil, true
	}
}

func (e *Executor) channelStreamCaptureJSON(ch *ChanRef) (string, error) {
	id, err := e.channelStreamHandle(ch)
	if err != nil {
		return "", err
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.StreamProxyCaptures++
	})
	return streamCaptureJSON(id, "go", "channel"), nil
}

func (e *Executor) channelStreamHandle(ch *ChanRef) (handles.ID, error) {
	return e.genericStreamHandle("go", ch)
}

func (e *Executor) localStreamCaptureJSON(value interface{}, runtime string) (string, bool, error) {
	switch v := value.(type) {
	case *ChanRef:
		jsonVal, err := e.channelStreamCaptureJSON(v)
		return jsonVal, true, err
	}
	if !isReceivableChannelValue(value) {
		if !isReaderStreamValue(value) {
			return "", false, nil
		}
	}
	if runtime == "" {
		runtime = "go"
	}
	id, err := e.genericStreamHandle(runtime, value)
	if err != nil {
		return "", true, err
	}
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.StreamProxyCaptures++
	})
	return streamCaptureJSON(id, runtime, streamKindForValue(value)), true, nil
}

func (e *Executor) genericStreamHandle(runtime string, value interface{}) (handles.ID, error) {
	if runtime == "" {
		runtime = "go"
	}
	if id, ok := e.bridgeHandleForValue(runtime, value); ok {
		return id, nil
	}
	var id handles.ID
	id, err := e.ensureHandleTable().Register(value, handles.RegisterOptions{
		Runtime: runtime,
		Kind:    streamKindForValue(value),
		ScopeID: e.currentHandleScope(),
		Release: func(any) error {
			if err := closeGenericStreamValue(value); err != nil {
				return err
			}
			e.forgetReleasedHandle(id, value)
			return nil
		},
	})
	if err != nil {
		return 0, err
	}
	if ident, ok := bridgeIdentityForValue(value); ok {
		e.bridgeHandles[ident] = id
	}
	return id, nil
}

func isReceivableChannelValue(value interface{}) bool {
	rv, ok := reflectChannelValue(value)
	if !ok {
		return false
	}
	return rv.Type().ChanDir()&reflect.RecvDir != 0
}

func isReaderStreamValue(value interface{}) bool {
	if isHTTPMessageShapeValue(value) {
		return false
	}
	_, ok := value.(io.Reader)
	return ok
}

func isHTTPMessageShapeValue(value interface{}) bool {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return false
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return false
		}
		rv = rv.Elem()
	}
	rt := rv.Type()
	methodLike := false
	targetLike := false
	if rt.Kind() == reflect.Struct {
		for _, name := range []string{"Method", "RequestMethod"} {
			if _, ok := rt.FieldByName(name); ok {
				methodLike = true
				break
			}
		}
		for _, name := range []string{"Path", "URL", "Url", "URI", "Headers", "Header", "Env", "PathInfo", "RequestURI"} {
			if _, ok := rt.FieldByName(name); ok {
				targetLike = true
				break
			}
		}
	}
	for i := 0; i < rt.NumMethod(); i++ {
		method := rt.Method(i)
		if method.Type.NumIn() != 1 || method.Type.NumOut() == 0 {
			continue
		}
		switch method.Name {
		case "Method", "GetMethod", "RequestMethod", "GetRequestMethod":
			methodLike = true
		case "Path", "GetPath", "URL", "Url", "GetURL", "GetUrl", "URI", "GetURI", "Headers", "GetHeaders", "Header", "GetHeader", "Env", "GetEnv", "PathInfo", "GetPathInfo", "RequestURI", "GetRequestURI":
			targetLike = true
		}
	}
	return methodLike && targetLike
}

func streamKindForValue(value interface{}) string {
	switch value.(type) {
	case *ChanRef:
		return "channel"
	}
	if isReceivableChannelValue(value) {
		return "channel"
	}
	if isReaderStreamValue(value) {
		return "reader"
	}
	return "stream"
}

func reflectChannelValue(value interface{}) (reflect.Value, bool) {
	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return reflect.Value{}, false
	}
	for rv.Kind() == reflect.Interface || rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return reflect.Value{}, false
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Chan {
		return reflect.Value{}, false
	}
	return rv, true
}

func recvReflectStreamValue(value interface{}) (interface{}, bool, bool) {
	rv, ok := reflectChannelValue(value)
	if !ok || rv.Type().ChanDir()&reflect.RecvDir == 0 {
		return nil, false, false
	}
	chosen, recv, recvOK := reflect.Select([]reflect.SelectCase{
		{Dir: reflect.SelectRecv, Chan: rv},
		{Dir: reflect.SelectDefault},
	})
	if chosen == 1 {
		return nil, true, true
	}
	if !recvOK {
		return nil, true, true
	}
	if !recv.IsValid() || !recv.CanInterface() {
		return nil, false, true
	}
	return recv.Interface(), false, true
}

func readGenericStreamValue(value interface{}) (interface{}, bool, bool, error) {
	reader, ok := value.(io.Reader)
	if !ok {
		return nil, false, false, nil
	}
	buf := make([]byte, 8192)
	n, err := reader.Read(buf)
	if n > 0 {
		chunk := make([]byte, n)
		copy(chunk, buf[:n])
		return chunk, false, true, nil
	}
	if err == nil {
		return nil, true, true, nil
	}
	if err == io.EOF {
		return nil, true, true, nil
	}
	return nil, false, true, err
}

func closeGenericStreamValue(value interface{}) error {
	closer, ok := value.(io.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

// registerChannelBuiltins registers Go channel builtins in the goFuncs registry.
func (e *Executor) registerChannelBuiltins() {
	e.goFuncs["make"] = func(arg interface{}) interface{} {
		size := int(toFloat(arg))
		if size < 0 {
			size = 0
		}
		return &ChanRef{ch: make(chan interface{}, size)}
	}
	e.goFuncs["recv"] = func(arg interface{}) interface{} {
		ch, ok := e.channelFromArg(arg)
		if !ok {
			return nil
		}
		v, ok := <-ch.ch
		if !ok {
			return nil
		}
		return v
	}
	e.goFuncs["send"] = func(chArg interface{}, val interface{}) interface{} {
		ch, ok := e.channelFromArg(chArg)
		if !ok {
			return false
		}
		return ch.sendNonBlocking(val)
	}
	e.goFuncs["wait"] = func(args []interface{}) interface{} {
		return e.waitSpawns(args)
	}
}

func (e *Executor) newSpawnHandle() *SpawnHandle {
	e.spawnsMu.Lock()
	defer e.spawnsMu.Unlock()
	e.nextSpawnID++
	handle := &SpawnHandle{
		ID:   e.nextSpawnID,
		done: make(chan struct{}),
	}
	e.spawns = append(e.spawns, handle)
	e.spawnWG.Add(1)
	return handle
}

func (e *Executor) completeSpawn(handle *SpawnHandle, result interface{}, err error) {
	handle.result = result
	handle.err = err
	close(handle.done)
	e.spawnWG.Done()
}

func (e *Executor) spawnCount() int {
	e.spawnsMu.Lock()
	defer e.spawnsMu.Unlock()
	return len(e.spawns)
}

func (e *Executor) waitSpawns(args []interface{}) interface{} {
	if len(args) == 0 {
		e.spawnWG.Wait()
		return e.spawnCount()
	}
	if len(args) == 1 {
		return waitSpawnValue(args[0])
	}
	results := make([]interface{}, 0, len(args))
	for _, arg := range args {
		results = append(results, waitSpawnValue(arg))
	}
	return results
}

func waitSpawnValue(arg interface{}) interface{} {
	switch v := arg.(type) {
	case *SpawnHandle:
		<-v.done
		if v.err != nil {
			return nil
		}
		return v.result
	case []interface{}:
		results := make([]interface{}, 0, len(v))
		for _, item := range v {
			results = append(results, waitSpawnValue(item))
		}
		return results
	default:
		return nil
	}
}

func (e *Executor) channelFromArg(arg interface{}) (*ChanRef, bool) {
	if ref, ok := arg.(*ChanRef); ok {
		return ref, true
	}
	name, ok := arg.(string)
	if !ok {
		return nil, false
	}
	e.channelsMu.RLock()
	ref, found := e.channels[name]
	e.channelsMu.RUnlock()
	if found {
		return ref, true
	}
	val, ok := e.getBinding(name)
	if !ok {
		return nil, false
	}
	ref, ok = val.(*ChanRef)
	return ref, ok
}

// opChan handles channel make/send/recv/close operations.
func (e *Executor) opChan(op *Op) (interface{}, error) {
	// make action creates a new channel — no existing channel to resolve
	if op.Action == "make" {
		size := 0
		if op.Size != nil {
			size = int(toFloat(op.Size))
		}
		if size < 0 {
			size = 0
		}
		ch := &ChanRef{ch: make(chan interface{}, size)}
		if op.Bind != "" {
			e.setBinding(op.Bind, ch)
			e.channelsMu.Lock()
			e.channels[op.Bind] = ch
			e.channelsMu.Unlock()
		}
		return ch, nil
	}

	chVal, ok := e.getBinding(op.Channel)
	if !ok {
		return nil, fmt.Errorf("chan %s: undefined channel %q", op.Action, op.Channel)
	}
	chRef, ok := chVal.(*ChanRef)
	if !ok {
		return nil, fmt.Errorf("chan %s: %q is not a channel (got %T)", op.Action, op.Channel, chVal)
	}

	switch op.Action {
	case "send":
		val, err := e.resolveValueExpr(op.Value)
		if err != nil {
			return nil, fmt.Errorf("chan send: %w", err)
		}
		// Non-blocking send to prevent deadlocks in single-threaded executor.
		// Buffered channels with capacity accept the value; full/unbuffered channels drop it.
		chRef.sendNonBlocking(val)
		return nil, nil

	case "recv":
		// Non-blocking recv to prevent deadlocks in single-threaded executor.
		// Buffered channels with data return immediately; empty channels return nil.
		var val interface{}
		select {
		case v := <-chRef.ch:
			val = v
		default:
		}
		if op.Bind != "" {
			e.setBinding(op.Bind, val)
		}
		return val, nil

	case "close":
		if err := chRef.close(); err != nil {
			return nil, fmt.Errorf("chan close: channel %q already closed", op.Channel)
		}
		return nil, nil

	default:
		return nil, fmt.Errorf("chan: unknown action %q", op.Action)
	}
}

// opSelect implements Go-style select on channels using reflect.Select.
func (e *Executor) opSelect(op *Op) (interface{}, error) {
	var cases []reflect.SelectCase

	for _, sc := range op.Cases {
		chVal, ok := e.getBinding(sc.Channel)
		if !ok {
			return nil, fmt.Errorf("select: undefined channel %q", sc.Channel)
		}
		chRef, ok := chVal.(*ChanRef)
		if !ok {
			return nil, fmt.Errorf("select: %q is not a channel (got %T)", sc.Channel, chVal)
		}

		switch sc.Action {
		case "recv":
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectRecv,
				Chan: reflect.ValueOf(chRef.ch),
			})
		case "send":
			if chRef.isClosed() {
				return nil, fmt.Errorf("select send: channel %q is closed", sc.Channel)
			}
			val, err := e.resolveValueExpr(sc.Value)
			if err != nil {
				return nil, fmt.Errorf("select send: %w", err)
			}
			var sendVal reflect.Value
			if val == nil {
				sendVal = reflect.Zero(reflect.TypeOf(chRef.ch).Elem())
			} else {
				sendVal = reflect.ValueOf(val)
			}
			cases = append(cases, reflect.SelectCase{
				Dir:  reflect.SelectSend,
				Chan: reflect.ValueOf(chRef.ch),
				Send: sendVal,
			})
		default:
			return nil, fmt.Errorf("select: unknown action %q", sc.Action)
		}
	}

	// Only add default case when defaultBody is present. Without an explicit
	// default, add a bounded timeout case so a manifest cannot wedge the
	// single-threaded executor forever on an empty select.
	hasDefault := len(op.DefaultBody) > 0
	if hasDefault {
		cases = append(cases, reflect.SelectCase{
			Dir: reflect.SelectDefault,
		})
	} else {
		timeout := time.After(100 * time.Millisecond)
		cases = append(cases, reflect.SelectCase{
			Dir:  reflect.SelectRecv,
			Chan: reflect.ValueOf(timeout),
		})
	}

	chosen, _, _ := reflect.Select(cases)

	if hasDefault && chosen == len(op.Cases) {
		return e.executeOps(op.DefaultBody)
	}
	if !hasDefault && chosen == len(op.Cases) {
		return nil, fmt.Errorf("select: no case ready")
	}

	return e.executeOps(op.Cases[chosen].Body)
}

// opSpawn launches a Go function in a new goroutine (best-effort).
func (e *Executor) opSpawn(op *Op) (interface{}, error) {
	code := strings.TrimSpace(op.Code)

	// Check for closure/arrow syntax — not supported
	if strings.HasPrefix(code, "()") {
		fmt.Fprintf(os.Stderr, "spawn: closure syntax not supported, skipping\n")
		return nil, nil
	}

	parenIdx := strings.Index(code, "(")
	if parenIdx < 0 {
		fmt.Fprintf(os.Stderr, "spawn: cannot parse %q\n", code)
		return nil, nil
	}

	funcName := strings.TrimSpace(code[:parenIdx])

	fn, ok := e.goFuncs[funcName]
	if !ok {
		// Check if funcName is a manifest func_def
		if _, isFuncDef := e.funcs[funcName]; isFuncDef {
			result, err := e.spawnFuncDef(funcName, code[parenIdx+1:len(code)-1])
			handle := e.newSpawnHandle()
			e.completeSpawn(handle, result, err)
			if op.Bind != "" {
				e.setBinding(op.Bind, handle)
			}
			return handle, nil
		}
		fmt.Fprintf(os.Stderr, "spawn: undefined function %q\n", funcName)
		return nil, nil
	}

	// Parse arguments
	argsStr := strings.TrimSpace(code[parenIdx+1 : len(code)-1])
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
			} else if f, err := strconv.ParseFloat(part, 64); err == nil {
				if f == float64(int(f)) {
					args = append(args, int(f))
				} else {
					args = append(args, f)
				}
			} else {
				part = strings.Trim(part, "\"'")
				args = append(args, part)
			}
		}
	}

	normalizedArgs := e.normalizeGoArgs(args)
	handle := e.newSpawnHandle()

	go func() {
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("spawn: %q panicked: %v", funcName, r)
				fmt.Fprintln(os.Stderr, err)
				e.completeSpawn(handle, nil, err)
			}
		}()
		result := callGoFuncDirect(fn, normalizedArgs)
		e.completeSpawn(handle, result, nil)
	}()

	if op.Bind != "" {
		e.setBinding(op.Bind, handle)
	}
	return handle, nil
}

// spawnFuncDef executes a manifest func_def inline (single-threaded executor).
// Parses args from the call expression string, builds a HandleCall JSON request,
// and invokes the function body via HandleCall.
func (e *Executor) spawnFuncDef(funcName, argsStr string) (interface{}, error) {
	argsStr = strings.TrimSpace(argsStr)
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
			} else if f, err := strconv.ParseFloat(part, 64); err == nil {
				if f == float64(int(f)) {
					args = append(args, int(f))
				} else {
					args = append(args, f)
				}
			} else {
				part = strings.Trim(part, "\"'")
				args = append(args, part)
			}
		}
	}

	reqJSON, err := json.Marshal(map[string]interface{}{"func": funcName, "args": args})
	if err != nil {
		return nil, fmt.Errorf("spawn func_def: marshal request: %w", err)
	}
	_, err = e.HandleCall(string(reqJSON))
	if err != nil {
		fmt.Fprintf(os.Stderr, "spawn: func_def %q error: %v\n", funcName, err)
	}
	return nil, nil
}

// callGoFuncDirect calls a Go function with the given args.
func callGoFuncDirect(fn interface{}, args []interface{}) interface{} {
	if f, ok := fn.(func() interface{}); ok {
		return f()
	}
	if f, ok := fn.(func(interface{}) interface{}); ok {
		var arg interface{}
		if len(args) > 0 {
			arg = args[0]
		}
		return f(arg)
	}
	if f, ok := fn.(func(interface{}, interface{}) interface{}); ok {
		var a, b interface{}
		if len(args) > 0 {
			a = args[0]
		}
		if len(args) > 1 {
			b = args[1]
		}
		return f(a, b)
	}
	if f, ok := fn.(func([]interface{}) (interface{}, error)); ok {
		val, err := f(args)
		if err != nil {
			panic(err)
		}
		return val
	}
	if f, ok := fn.(func([]interface{}) interface{}); ok {
		return f(args)
	}
	return nil
}
