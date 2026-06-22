package manifest

import "strings"

// In-process Go plugins (the plugin.Open path used by manifest-runner / the
// omnivm CLI) share the host Go runtime, so the host hands them a Go-func bridge
// directly (no C trampoline). This mirrors the c-shared bridge ABI in plugins.go
// so a .poly Go function can invoke a guest callback in either plugin mode.

// goInProcessBridgeShim is compiled alongside an in-process Go plugin's source.
// It defines __omnivm_invoke (the callable-param lowering target) and OmniSetBridge
// (installed by compileGoPlugin). Helpers are package-level; harmless if unused.
const goInProcessBridgeShim = `package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var __omnivm_bridge func(string, string) string

// OmniSetBridge is called by the host after load to install the manifest bridge.
func OmniSetBridge(fn func(string, string) string) { __omnivm_bridge = fn }

// __omnivm_invoke invokes a guest callback (passed as its handle descriptor) on
// the host via a handle_call bridge op and decodes the result. The host marshals
// off-Golden-Thread calls (e.g. from spawned goroutines) automatically.
func __omnivm_invoke(fn interface{}, args ...interface{}) interface{} {
	if __omnivm_bridge == nil {
		panic("omnivm: host bridge not installed for in-process Go plugin")
	}
	var id uint64
	switch v := fn.(type) {
	case interface{ HandleID() uint64 }:
		// In-process: the live *GoHandleProxy is passed directly.
		id = v.HandleID()
	case map[string]interface{}:
		// Serialized callable descriptor (defensive; in-process passes the proxy).
		rawID, hasID := v["id"]
		if !hasID {
			panic("omnivm: callable value has no handle id")
		}
		switch n := rawID.(type) {
		case float64:
			id = uint64(n)
		case int:
			id = uint64(n)
		case int64:
			id = uint64(n)
		case uint64:
			id = n
		case json.Number:
			i, _ := n.Int64()
			id = uint64(i)
		default:
			panic(fmt.Sprintf("omnivm: callable handle id has unexpected type %T", rawID))
		}
	default:
		panic(fmt.Sprintf("omnivm: value of type %T is not callable", fn))
	}
	if args == nil {
		args = []interface{}{}
	}
	reqJSON, err := json.Marshal(map[string]interface{}{
		"op": "handle_call", "id": id, "key": "", "args": args,
	})
	if err != nil {
		panic(err)
	}
	reply := __omnivm_bridge("__manifest", string(reqJSON))
	if strings.HasPrefix(reply, "ERR:") {
		panic(errors.New(reply[4:]))
	}
	reply = strings.TrimPrefix(reply, "OK:")
	var env struct {
		Value interface{} ` + "`json:\"value\"`" + `
	}
	if err := json.Unmarshal([]byte(reply), &env); err == nil {
		return env.Value
	}
	var raw interface{}
	if json.Unmarshal([]byte(reply), &raw) == nil {
		return raw
	}
	return reply
}
`

// goPluginBridge is the Go-func bridge handed to in-process plugins. It routes
// "__manifest" requests to HandleCall, auto-marshaling off-Golden-Thread calls to
// the Golden Thread (serviced by a pumping-wait or the cooperative host driver).
func (e *Executor) goPluginBridge(runtime, code string) string {
	if runtime != "__manifest" {
		return "ERR:in-process plugin bridge only supports __manifest, got " + runtime
	}
	run := func() (string, error) { return e.HandleCall(code) }
	var res string
	var err error
	if onGoldenThread() || hostDispatcher == nil {
		res, err = run()
	} else if rerr := hostDispatcher.RunOnMain(func() error { res, err = run(); return nil }); rerr != nil {
		return "ERR:" + rerr.Error()
	}
	if err != nil {
		return "ERR:" + err.Error()
	}
	return "OK:" + res
}

// pluginBridgeShimNeeded reports whether a generated Go plugin source invokes a
// guest callback (and thus needs the bridge shim compiled in).
func pluginBridgeShimNeeded(source string) bool {
	return strings.Contains(source, "__omnivm_invoke")
}
