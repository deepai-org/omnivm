package manifest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"time"

	"github.com/omnivm/omnivm/pkg/handles"
)

// GoHandleProxy is the manifest-side Go equivalent of the descriptor proxies
// injected into dynamic runtimes. It gives Go/plugin functions a generic,
// map-like handle without serializing runtime-owned objects into JSON.
type GoHandleProxy struct {
	id            handles.ID
	table         *handles.Table
	kind          string
	payload       map[string]interface{}
	get           func(handles.ID, string) (interface{}, bool, error)
	index         func(handles.ID, interface{}) (interface{}, bool, error)
	set           func(handles.ID, string, interface{}) (bool, error)
	len           func(handles.ID) (int, bool, error)
	iter          func(handles.ID, string) ([]interface{}, bool, error)
	contains      func(handles.ID, interface{}) (bool, bool, error)
	call          func(handles.ID, string, []interface{}) (interface{}, error)
	material      func(interface{}) interface{}
	boundary      func(handles.ID, interface{}) interface{}
	onMaterialize func()
	closed        bool
	closeErr      error
}

// GoProxyItem is a single key/value pair returned by GoHandleProxy.Items.
type GoProxyItem struct {
	Key   interface{}
	Value interface{}
}

func newGoHandleProxy(id handles.ID, table *handles.Table, kind string, payload map[string]interface{}, get func(handles.ID, string) (interface{}, bool, error), index func(handles.ID, interface{}) (interface{}, bool, error), set func(handles.ID, string, interface{}) (bool, error), length func(handles.ID) (int, bool, error), iter func(handles.ID, string) ([]interface{}, bool, error), contains func(handles.ID, interface{}) (bool, bool, error), call func(handles.ID, string, []interface{}) (interface{}, error), material func(interface{}) interface{}, boundary func(handles.ID, interface{}) interface{}, onMaterialize func()) *GoHandleProxy {
	proxy := &GoHandleProxy{
		id:            id,
		table:         table,
		kind:          kind,
		payload:       payload,
		get:           get,
		index:         index,
		set:           set,
		len:           length,
		iter:          iter,
		contains:      contains,
		call:          call,
		material:      material,
		boundary:      boundary,
		onMaterialize: onMaterialize,
	}
	if table != nil && id != 0 {
		_ = table.Retain(id)
		runtime.SetFinalizer(proxy, func(p *GoHandleProxy) {
			p.ReleaseFromFinalizer()
		})
	}
	return proxy
}

// ID returns the opaque OmniVM handle id.
func (p *GoHandleProxy) ID() handles.ID {
	if p == nil {
		return 0
	}
	p.record("property")
	return p.id
}

// Kind returns the descriptor kind, such as resource, table, or job.
func (p *GoHandleProxy) Kind() string {
	if p == nil {
		return ""
	}
	p.record("property")
	return p.kind
}

// Runtime returns the runtime that owns this handle, when the descriptor has one.
func (p *GoHandleProxy) Runtime() string {
	if p == nil {
		return ""
	}
	p.record("property")
	value, _ := p.payload["runtime"].(string)
	return value
}

// ResourceKind returns the host-side resource kind, when the descriptor has one.
func (p *GoHandleProxy) ResourceKind() string {
	if p == nil {
		return ""
	}
	p.record("property")
	value, _ := p.payload["kind"].(string)
	return value
}

// Get reads the target object's generic property surface.
func (p *GoHandleProxy) Get(key string) interface{} {
	value, _ := p.GetWithError(key)
	return value
}

// GetWithError reads the target object's generic property surface and reports
// owner-side lifecycle or bridge errors instead of collapsing them to nil.
func (p *GoHandleProxy) GetWithError(key string) (interface{}, error) {
	if p == nil {
		return nil, nil
	}
	if p.closed {
		return nil, p.closedOperationError("get")
	}
	if value, ok := p.localValue(key); ok {
		return value, nil
	}
	if report, ok := p.record("property"); ok && report.Chatty {
		p.materializeChatty()
		if value, ok := p.localValue(key); ok {
			return value, nil
		}
	}
	if p.get == nil || p.id == 0 {
		return nil, nil
	}
	value, ok, err := p.get(p.id, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return p.materialize(value), nil
}

// Index reads through the target object's native indexing protocol when
// descriptor fields do not satisfy the access locally.
func (p *GoHandleProxy) Index(key interface{}) interface{} {
	value, _ := p.IndexWithError(key)
	return value
}

// IndexWithError reads through the target object's native indexing protocol
// and reports owner-side lifecycle or bridge errors.
func (p *GoHandleProxy) IndexWithError(key interface{}) (interface{}, error) {
	if p == nil {
		return nil, nil
	}
	if p.closed {
		return nil, p.closedOperationError("index")
	}
	if keyStr, ok := key.(string); ok {
		if value, ok := p.localValue(keyStr); ok {
			return value, nil
		}
	}
	if report, ok := p.record("index"); ok && report.Chatty {
		p.materializeChatty()
		if keyStr, ok := key.(string); ok {
			if value, ok := p.localValue(keyStr); ok {
				return value, nil
			}
		}
		textKey := stringifyGoProxyKey(key)
		if value, ok := p.localValue(textKey); ok {
			return value, nil
		}
	}
	if p.index == nil || p.id == 0 {
		return nil, nil
	}
	value, ok, err := p.index(p.id, key)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return p.materialize(value), nil
}

// Values returns a batched snapshot of the target object's iterable values.
func (p *GoHandleProxy) Values() []interface{} {
	values, _ := p.ValuesWithError()
	return values
}

// ValuesWithError returns a batched snapshot of iterable values and reports
// owner-side lifecycle or bridge errors.
func (p *GoHandleProxy) ValuesWithError() ([]interface{}, error) {
	if p == nil {
		return nil, nil
	}
	if p.closed {
		return nil, p.closedOperationError("iterate")
	}
	if p.iter == nil || p.id == 0 {
		p.record("iterate")
		payload := p.localPayload()
		out := make([]interface{}, 0, len(payload))
		for _, value := range payload {
			out = append(out, value)
		}
		return out, nil
	}
	values, ok, err := p.iter(p.id, "values")
	if err != nil {
		return nil, err
	}
	if !ok {
		p.record("iterate")
		payload := p.localPayload()
		out := make([]interface{}, 0, len(payload))
		for _, value := range payload {
			out = append(out, value)
		}
		return out, nil
	}
	out := make([]interface{}, 0, len(values))
	for _, value := range values {
		out = append(out, p.materialize(value))
	}
	return out, nil
}

// Keys returns a batched snapshot of the target object's iterable keys.
func (p *GoHandleProxy) Keys() []interface{} {
	keys, _ := p.KeysWithError()
	return keys
}

// KeysWithError returns a batched snapshot of iterable keys and reports
// owner-side lifecycle or bridge errors.
func (p *GoHandleProxy) KeysWithError() ([]interface{}, error) {
	if p == nil {
		return nil, nil
	}
	if p.closed {
		return nil, p.closedOperationError("iterate")
	}
	if p.iter == nil || p.id == 0 {
		p.record("iterate")
		payload := p.localPayload()
		out := make([]interface{}, 0, len(payload))
		for key := range payload {
			out = append(out, key)
		}
		return out, nil
	}
	keys, ok, err := p.iter(p.id, "keys")
	if err != nil {
		return nil, err
	}
	if !ok {
		p.record("iterate")
		payload := p.localPayload()
		out := make([]interface{}, 0, len(payload))
		for key := range payload {
			out = append(out, key)
		}
		return out, nil
	}
	out := make([]interface{}, 0, len(keys))
	for _, key := range keys {
		out = append(out, p.materialize(key))
	}
	return out, nil
}

// Items returns a batched snapshot of the target object's iterable key/value
// pairs. Sequence-like targets report numeric indexes as keys.
func (p *GoHandleProxy) Items() []GoProxyItem {
	items, _ := p.ItemsWithError()
	return items
}

// ItemsWithError returns a batched snapshot of iterable key/value pairs and
// reports owner-side lifecycle or bridge errors.
func (p *GoHandleProxy) ItemsWithError() ([]GoProxyItem, error) {
	if p == nil {
		return nil, nil
	}
	if p.closed {
		return nil, p.closedOperationError("iterate")
	}
	if p.iter == nil || p.id == 0 {
		p.record("iterate")
		payload := p.localPayload()
		out := make([]GoProxyItem, 0, len(payload))
		for key, value := range payload {
			out = append(out, GoProxyItem{Key: key, Value: p.materialize(value)})
		}
		return out, nil
	}
	items, ok, err := p.iter(p.id, "items")
	if err != nil {
		return nil, err
	}
	if !ok {
		p.record("iterate")
		payload := p.localPayload()
		out := make([]GoProxyItem, 0, len(payload))
		for key, value := range payload {
			out = append(out, GoProxyItem{Key: key, Value: p.materialize(value)})
		}
		return out, nil
	}
	out := make([]GoProxyItem, 0, len(items))
	for _, item := range items {
		pair, ok := item.([]interface{})
		if !ok || len(pair) != 2 {
			continue
		}
		out = append(out, GoProxyItem{
			Key:   p.materialize(pair[0]),
			Value: p.materialize(pair[1]),
		})
	}
	return out, nil
}

// Contains reports whether the target object's generic membership protocol
// contains key.
func (p *GoHandleProxy) Contains(key interface{}) bool {
	found, _ := p.ContainsWithError(key)
	return found
}

// ContainsWithError reports membership and owner-side lifecycle or bridge
// errors.
func (p *GoHandleProxy) ContainsWithError(key interface{}) (bool, error) {
	if p == nil {
		return false, nil
	}
	if p.closed {
		return false, p.closedOperationError("contains")
	}
	if p.contains == nil || p.id == 0 {
		p.record("property")
		if keyStr, ok := key.(string); ok {
			return p.hasLocalValue(keyStr), nil
		}
		return false, nil
	}
	ok, found, err := p.contains(p.id, key)
	if err != nil {
		return false, err
	}
	if !ok {
		p.record("property")
		if keyStr, ok := key.(string); ok {
			return p.hasLocalValue(keyStr), nil
		}
		return false, nil
	}
	return found, nil
}

// Len reports the target object's generic collection length when available.
func (p *GoHandleProxy) Len() int {
	value, _ := p.LenWithError()
	return value
}

// LenWithError reports the target object's collection length and owner-side
// lifecycle or bridge errors.
func (p *GoHandleProxy) LenWithError() (int, error) {
	if p == nil {
		return 0, nil
	}
	if p.closed {
		return 0, p.closedOperationError("len")
	}
	if p.len == nil || p.id == 0 {
		p.record("property")
		return len(p.localPayload()), nil
	}
	value, ok, err := p.len(p.id)
	if err != nil {
		return 0, err
	}
	if !ok {
		p.record("property")
		return len(p.localPayload()), nil
	}
	return value, nil
}

// Set mutates the target object's generic property surface.
func (p *GoHandleProxy) Set(key string, value interface{}) bool {
	ok, _ := p.SetWithError(key, value)
	return ok
}

// SetWithError mutates the target object's generic property surface and reports
// owner-side lifecycle or bridge errors.
func (p *GoHandleProxy) SetWithError(key string, value interface{}) (bool, error) {
	if p == nil {
		return false, nil
	}
	if p.closed {
		return false, p.closedOperationError("set")
	}
	p.record("mutation")
	if p.set == nil || p.id == 0 {
		return false, nil
	}
	ok, err := p.set(p.id, key, value)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// Call invokes a callable property or method on the target object.
func (p *GoHandleProxy) Call(key string, args ...interface{}) interface{} {
	value, _ := p.CallWithError(key, args...)
	return value
}

// CallWithError invokes a callable property or method and reports owner-side
// lifecycle or bridge errors.
func (p *GoHandleProxy) CallWithError(key string, args ...interface{}) (interface{}, error) {
	if p == nil {
		return nil, nil
	}
	if p.closed {
		return nil, p.closedOperationError("call")
	}
	p.record("call")
	if p.call == nil || p.id == 0 {
		return nil, nil
	}
	value, err := p.call(p.id, key, args)
	if err != nil {
		return nil, err
	}
	return p.materialize(value), nil
}

// AsMap returns a shallow copy of the descriptor and records iteration.
func (p *GoHandleProxy) AsMap() map[string]interface{} {
	value, _ := p.AsMapWithError()
	return value
}

// AsMapWithError returns a shallow map snapshot and reports owner-side
// lifecycle or bridge errors.
func (p *GoHandleProxy) AsMapWithError() (map[string]interface{}, error) {
	if p == nil {
		return nil, nil
	}
	if p.closed {
		return nil, p.closedOperationError("iterate")
	}
	if p.iter != nil && p.id != 0 {
		items, ok, err := p.iter(p.id, "items")
		if err != nil {
			return nil, err
		}
		if ok {
			out := make(map[string]interface{}, len(items))
			for _, item := range items {
				pair, ok := item.([]interface{})
				if !ok || len(pair) != 2 {
					continue
				}
				out[stringifyGoProxyKey(pair[0])] = p.materialize(pair[1])
			}
			return out, nil
		}
	}
	p.record("iterate")
	payload := p.localPayload()
	out := make(map[string]interface{}, len(payload))
	for key, value := range payload {
		out[key] = p.materialize(value)
	}
	return out, nil
}

func (p *GoHandleProxy) localValue(key string) (interface{}, bool) {
	if p == nil || p.closed || p.isInternalDescriptorKey(key) {
		return nil, false
	}
	value, ok := p.payload[key]
	return value, ok
}

func (p *GoHandleProxy) hasLocalValue(key string) bool {
	_, ok := p.localValue(key)
	return ok
}

func (p *GoHandleProxy) localPayload() map[string]interface{} {
	if p == nil || p.closed || len(p.payload) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(p.payload))
	for key, value := range p.payload {
		if p.isInternalDescriptorKey(key) {
			continue
		}
		out[key] = value
	}
	return out
}

func (p *GoHandleProxy) isInternalDescriptorKey(key string) bool {
	if p == nil || !isDescriptorPayload(p.payload) {
		return false
	}
	return isDescriptorInternalKey(key)
}

func (p *GoHandleProxy) closedOperationError(op string) error {
	if p == nil {
		return nil
	}
	if p.id != 0 {
		return fmt.Errorf("omnivm: Go handle proxy %s on closed %s handle #%d", op, p.kind, p.id)
	}
	return fmt.Errorf("omnivm: Go handle proxy %s on closed %s handle", op, p.kind)
}

func stringifyGoProxyKey(key interface{}) string {
	switch v := key.(type) {
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// ReleaseFromFinalizer queues a guest-proxy finalizer release on the handle
// table. Release callbacks are drained later from host-owned safe points.
func (p *GoHandleProxy) ReleaseFromFinalizer() {
	if p == nil || p.closed || p.table == nil || p.id == 0 {
		return
	}
	p.table.QueueReleaseFromFinalizer(p.id)
}

// Close releases the Go proxy's retained handle reference immediately and
// detaches its finalizer. It is safe to call more than once.
func (p *GoHandleProxy) Close() error {
	if p == nil || p.closed {
		return nil
	}
	if p.closeErr != nil {
		return p.closeErr
	}
	if p.table == nil || p.id == 0 {
		p.closed = true
		runtime.SetFinalizer(p, nil)
		return nil
	}
	if err := p.table.Release(p.id); err != nil {
		p.closeErr = err
		return err
	}
	p.closed = true
	runtime.SetFinalizer(p, nil)
	return nil
}

// ProxyClose closes an OmniVM Go proxy or ordinary Go closeable through the
// same collision-safe lifecycle path exposed by guest runtimes. It returns
// false,nil when the value is nil, has no close operation, or was already
// closed; release/cancel failures are returned with closed=false so callers do
// not mistake failed owner cleanup for a successful local close.
func ProxyClose(value interface{}) (bool, error) {
	if goProxyValueIsNil(value) {
		return false, nil
	}
	switch v := value.(type) {
	case nil:
		return false, nil
	case *GoHandleProxy:
		if v.closed {
			return false, nil
		}
		if err := v.Close(); err != nil {
			return false, err
		}
		return true, nil
	case *GoStreamProxy:
		if v.closed {
			return false, nil
		}
		if err := v.Close(); err != nil {
			return false, err
		}
		return true, nil
	case interface{ Close() error }:
		if err := v.Close(); err != nil {
			return false, err
		}
		return true, nil
	default:
		return false, nil
	}
}

func goProxyValueIsNil(value interface{}) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}

// OmnivmClose is an alias for ProxyClose, matching generated helper names in
// other OmniVM guest runtimes.
func OmnivmClose(value interface{}) (bool, error) {
	return ProxyClose(value)
}

func (p *GoHandleProxy) record(kind string) (handles.AccessReport, bool) {
	if p == nil || p.closed || p.table == nil || p.id == 0 {
		return handles.AccessReport{}, false
	}
	report, err := p.table.RecordAccess(p.id, handles.AccessOptions{Kind: kind, Now: time.Now()})
	return report, err == nil
}

func (p *GoHandleProxy) materializeChatty() {
	if p.payload == nil || p.iter == nil || p.id == 0 {
		return
	}
	if materialized, _ := p.payload["__omnivm_materialized__"].(bool); materialized {
		return
	}
	items, ok, err := p.iter(p.id, "items")
	if err != nil || !ok {
		return
	}
	for _, item := range items {
		pair, ok := item.([]interface{})
		if !ok || len(pair) < 2 {
			continue
		}
		key := stringifyGoProxyKey(pair[0])
		if _, exists := p.payload[key]; !exists {
			p.payload[key] = p.materialize(pair[1])
		}
	}
	p.payload["__omnivm_materialized__"] = true
	if p.onMaterialize != nil {
		p.onMaterialize()
	}
}

func (p *GoHandleProxy) materialize(value interface{}) interface{} {
	if p.boundary != nil && p.id != 0 {
		return p.boundary(p.id, value)
	}
	if p.material == nil {
		return value
	}
	return p.material(value)
}

// GoStreamProxy is the Go-side representation of a manifest stream descriptor.
// It pulls lazily from the owning runtime, can be closed explicitly to cancel
// partial consumption, and queues release through the handle table when the
// proxy is finalized.
type GoStreamProxy struct {
	id          handles.ID
	table       *handles.Table
	next        func(handles.ID) (interface{}, bool, bool, error)
	cancel      func(handles.ID) error
	normalize   func(interface{}) interface{}
	localValues []interface{}
	localIndex  int
	closed      bool
}

func newGoStreamProxy(id handles.ID, table *handles.Table, next func(handles.ID) (interface{}, bool, bool, error), cancel func(handles.ID) error) *GoStreamProxy {
	proxy := &GoStreamProxy{id: id, table: table, next: next, cancel: cancel}
	if table != nil && id != 0 {
		_ = table.Retain(id)
		runtime.SetFinalizer(proxy, func(p *GoStreamProxy) {
			p.ReleaseFromFinalizer()
		})
	}
	return proxy
}

func newGoLocalStreamProxy(values []interface{}, normalize func(interface{}) interface{}) *GoStreamProxy {
	return &GoStreamProxy{localValues: values, normalize: normalize}
}

// Next returns the next stream value, whether a value was produced, and any
// owner-side read error. EOF and closed streams return ok=false with nil error.
func (p *GoStreamProxy) Next() (interface{}, bool, error) {
	if p == nil || p.closed {
		return nil, false, nil
	}
	if p.localValues != nil {
		if p.localIndex >= len(p.localValues) {
			p.closed = true
			return nil, false, nil
		}
		value := p.localValues[p.localIndex]
		p.localIndex++
		if p.normalize != nil {
			value = p.normalize(value)
		}
		return value, true, nil
	}
	if p.next == nil || p.id == 0 {
		return nil, false, nil
	}
	value, done, ok, err := p.next(p.id)
	if err != nil || !ok || done {
		p.closed = true
		runtime.SetFinalizer(p, nil)
		return nil, false, err
	}
	return value, true, nil
}

func (p *GoStreamProxy) Recv() (interface{}, bool) {
	value, ok, _ := p.Next()
	return value, ok
}

func (p *GoStreamProxy) Values() []interface{} {
	out, _ := p.ValuesWithError()
	return out
}

func (p *GoStreamProxy) ValuesWithError() ([]interface{}, error) {
	out := []interface{}{}
	for {
		value, ok, err := p.Next()
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, value)
	}
}

// Close cancels a partially consumed stream and releases all refs for the
// underlying stream handle. It is safe to call more than once.
func (p *GoStreamProxy) Close() error {
	if p == nil || p.closed {
		return nil
	}
	if p.localValues != nil {
		p.closed = true
		runtime.SetFinalizer(p, nil)
		return nil
	}
	if p.table == nil || p.id == 0 {
		p.closed = true
		runtime.SetFinalizer(p, nil)
		return nil
	}
	if p.cancel != nil {
		if err := p.cancel(p.id); err != nil {
			return err
		}
	} else if err := p.table.ReleaseAllRefs(p.id); err != nil {
		return err
	}
	p.closed = true
	runtime.SetFinalizer(p, nil)
	return nil
}

func (p *GoStreamProxy) ReleaseFromFinalizer() {
	if p == nil || p.closed {
		return
	}
	if p.localValues != nil {
		p.closed = true
		return
	}
	if p.table == nil || p.id == 0 {
		return
	}
	p.table.QueueReleaseFromFinalizer(p.id)
}

func (e *Executor) normalizeGoArgs(args []interface{}) []interface{} {
	out := make([]interface{}, len(args))
	for i, arg := range args {
		out[i] = e.normalizeGoArg(arg)
	}
	return out
}

func (e *Executor) normalizeGoArg(arg interface{}) interface{} {
	switch v := arg.(type) {
	case RuntimeRef:
		if runtimeRefNeedsProxy(v) {
			return e.goHandleProxyForRuntimeRef(v)
		}
		return e.normalizeGoArg(v.Value)
	case *ResourceRef:
		return newGoHandleProxy(v.ID, e.ensureHandleTable(), "resource", map[string]interface{}{
			"__omnivm_resource__": true,
			"id":                  uint64(v.ID),
			"runtime":             v.Runtime,
			"kind":                v.Kind,
			"disposer":            v.Disposer,
			"closed":              v.Closed,
		}, e.handleProperty, e.handleIndex, e.handleSetForProxy, e.handleLen, e.handleIter, e.handleContains, e.handleMethodCallPositional, e.normalizeGoArg, e.normalizeGoBoundaryValue, e.recordProxyMaterialization)
	case *TableRef:
		return newGoHandleProxy(v.ID, e.ensureHandleTable(), "table", map[string]interface{}{
			"__omnivm_table__": true,
			"id":               uint64(v.ID),
			"runtime":          v.Runtime,
			"format":           v.Format,
			"ownership":        v.Ownership,
			"release":          v.Release,
			"metadata":         v.Metadata,
			"released":         v.Released,
		}, e.handleProperty, e.handleIndex, e.handleSetForProxy, e.handleLen, e.handleIter, e.handleContains, e.handleMethodCallPositional, e.normalizeGoArg, e.normalizeGoBoundaryValue, e.recordProxyMaterialization)
	case *JobHandle:
		return newGoHandleProxy(0, nil, "job", map[string]interface{}{
			"__omnivm_job__": true,
			"id":             v.ID,
			"runtime":        v.Runtime,
			"kind":           v.Kind,
			"done":           v.Done,
			"cancelled":      v.Cancelled,
			"cancelReason":   e.normalizeGoArg(v.CancelReason),
			"payload":        e.normalizeGoArg(v.Payload),
			"result":         e.normalizeGoArg(v.Result),
		}, nil, nil, nil, nil, nil, nil, nil, e.normalizeGoArg, nil, nil)
	case map[string]interface{}:
		if isGoStreamDescriptor(v) {
			if values, ok := goStreamDescriptorValues(v); ok {
				return newGoLocalStreamProxy(values, e.normalizeGoArg)
			}
			id, err := bridgeHandleID(v["id"])
			if err != nil {
				return v
			}
			return e.goStreamProxyForID(id)
		}
		if isGoHandleDescriptor(v) {
			id, _ := bridgeHandleID(v["id"])
			kind := goDescriptorKind(v)
			if kind == "job" {
				id = 0
			}
			return newGoHandleProxy(id, e.ensureHandleTable(), kind, normalizeGoMap(v, e.normalizeGoArg), e.handleProperty, e.handleIndex, e.handleSetForProxy, e.handleLen, e.handleIter, e.handleContains, e.handleMethodCallPositional, e.normalizeGoArg, e.normalizeGoBoundaryValue, e.recordProxyMaterialization)
		}
		return normalizeGoMap(v, e.normalizeGoArg)
	case []interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, e.normalizeGoArg(item))
		}
		return out
	case string:
		if value, ok := e.resolveGoSelectorConstant(v); ok {
			return value
		}
		return normalizeArg(arg)
	default:
		if isReceivableChannelValue(arg) || isReaderStreamValue(arg) {
			id, err := e.genericStreamHandle("go", arg)
			if err == nil {
				e.addBoundaryStat(func(stats *BoundaryStats) {
					stats.StreamProxyCaptures++
				})
				return e.goStreamProxyForID(id)
			}
		}
		return normalizeArg(arg)
	}
}

func (e *Executor) normalizeGoBoundaryValue(parent handles.ID, value interface{}) interface{} {
	wrapped, err := e.bridgeResultValue(parent, value)
	if err != nil {
		return e.normalizeGoArg(value)
	}
	return e.normalizeGoArg(wrapped)
}

func (e *Executor) goHandleProxyForRuntimeRef(ref RuntimeRef) interface{} {
	if jsonVal, ok, err := e.runtimeRefBulkTableCaptureJSON("", "go", ref); ok || err != nil {
		if err == nil {
			var descriptor map[string]interface{}
			if decodeErr := json.Unmarshal([]byte(jsonVal), &descriptor); decodeErr == nil {
				return e.normalizeGoArg(descriptor)
			}
		}
	}
	jsonVal, err := e.runtimeRefProxyCaptureJSON(ref)
	if err != nil {
		return e.normalizeGoArg(ref.Value)
	}
	var descriptor map[string]interface{}
	if err := json.Unmarshal([]byte(jsonVal), &descriptor); err != nil {
		return e.normalizeGoArg(ref.Value)
	}
	return e.normalizeGoArg(descriptor)
}

func (e *Executor) goStreamProxyForID(id handles.ID) *GoStreamProxy {
	return newGoStreamProxy(id, e.ensureHandleTable(), func(id handles.ID) (interface{}, bool, bool, error) {
		value, done, ok, err := e.handleStreamNext(id)
		if err != nil || !ok || done {
			return value, done, ok, err
		}
		return e.normalizeGoArg(value), false, true, nil
	}, func(id handles.ID) error {
		return e.handleStreamCancel(id)
	})
}

func (e *Executor) recordProxyMaterialization() {
	e.addBoundaryStat(func(stats *BoundaryStats) {
		stats.ProxyMaterializations++
	})
}

func isGoStreamDescriptor(value map[string]interface{}) bool {
	return value["__omnivm_stream__"] == true || value["__omnivm_channel__"] == true
}

func goStreamDescriptorValues(value map[string]interface{}) ([]interface{}, bool) {
	values, ok := value["values"].([]interface{})
	return values, ok
}

func isGoHandleDescriptor(value map[string]interface{}) bool {
	return value["__omnivm_resource__"] == true || value["__omnivm_table__"] == true || value["__omnivm_job__"] == true
}

func goDescriptorKind(value map[string]interface{}) string {
	switch {
	case value["__omnivm_resource__"] == true:
		return "resource"
	case value["__omnivm_table__"] == true:
		return "table"
	case value["__omnivm_job__"] == true:
		return "job"
	default:
		return "object"
	}
}

func normalizeGoMap(value map[string]interface{}, materialize func(interface{}) interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(value))
	for key, item := range value {
		if materialize == nil {
			out[key] = item
			continue
		}
		out[key] = materialize(item)
	}
	return out
}
