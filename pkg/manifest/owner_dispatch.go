package manifest

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	ownerDispatchBoundary       = "owner_dispatch"
	ownerDispatchTargetBoundary = "owner_dispatch_target"
	rubyThreadingBoundary       = "ruby_threading"
)

// OwnerDispatchError is the structured diagnostic returned by Go manifest
// preflight helpers when an integration requires unsupported owner dispatch.
type OwnerDispatchError struct {
	Runtime             string
	OriginRuntime       string
	Type                string
	Message             string
	BoundaryPath        string
	OriginalErrorHandle string
	Details             interface{}
	DetailsJSON         string
}

func (e *OwnerDispatchError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Message
}

// ToMap returns a copied JSON-style error envelope for logging or middleware.
func (e *OwnerDispatchError) ToMap() map[string]interface{} {
	if e == nil {
		return nil
	}
	runtimeName := e.Runtime
	if runtimeName == "" {
		runtimeName = "go"
	}
	originRuntime := e.OriginRuntime
	if originRuntime == "" {
		originRuntime = runtimeName
	}
	errorType := e.Type
	if errorType == "" {
		errorType = "RuntimeError"
	}
	details := cloneJSONValue(e.Details)
	detailsJSON := e.DetailsJSON
	if detailsJSON == "" && details != nil {
		detailsJSON = jsonString(details)
	}
	return map[string]interface{}{
		"runtime":               runtimeName,
		"origin_runtime":        originRuntime,
		"type":                  errorType,
		"message":               e.Message,
		"traceback":             "",
		"stack_frames":          []interface{}{},
		"cause_chain":           []interface{}{},
		"boundary_path":         e.BoundaryPath,
		"original_error_handle": e.OriginalErrorHandle,
		"details":               details,
		"details_json":          detailsJSON,
	}
}

func (e *OwnerDispatchError) MarshalJSON() ([]byte, error) {
	return json.Marshal(e.ToMap())
}

// OwnerDispatchStatus returns the diagnostic-only owner-dispatch capability
// contract exposed to Go manifest callers.
func OwnerDispatchStatus() map[string]interface{} {
	return cloneJSONObject(ownerDispatchContract())
}

// RubyThreadingStatus returns the embedded Ruby threading capability contract.
func RubyThreadingStatus() map[string]interface{} {
	return cloneJSONObject(rubyThreadingContract())
}

// OwnerDispatchTargetStatus returns one target capability block. Common aliases
// such as "asyncio", "js", "java", and "ruby" are accepted.
func OwnerDispatchTargetStatus(target string) (map[string]interface{}, error) {
	requested := fmt.Sprint(target)
	name := ownerDispatchTargetName(requested)
	status := ownerDispatchContract()
	targets, _ := status["owner_dispatch_targets"].(map[string]interface{})
	info, ok := targets[name].(map[string]interface{})
	if !ok {
		known := make([]string, 0, len(targets))
		for key := range targets {
			known = append(known, key)
		}
		sort.Strings(known)
		targetDetails := map[string]interface{}{
			"target":                 name,
			"requested_target":       requested,
			"known_targets":          known,
			"owner_dispatch_targets": targets,
		}
		return nil, ownerDispatchRuntimeError(
			fmt.Sprintf("unknown owner dispatch target: %s", requested),
			ownerDispatchTargetBoundary,
			map[string]interface{}{
				"target":                 name,
				"requested_target":       requested,
				"known_targets":          known,
				"owner_dispatch_targets": targets,
				"owner_dispatch_target":  targetDetails,
			},
		)
	}
	out := cloneJSONObject(info)
	out["requested_target"] = requested
	out["target"] = name
	return out, nil
}

// AssertOwnerDispatchSupported returns a structured diagnostic error because
// OmniVM currently does not provide universal owner-loop dispatch.
func AssertOwnerDispatchSupported(label string) error {
	info := OwnerDispatchStatus()
	if info["owner_dispatch_supported"] == true {
		return nil
	}
	prefix := labelPrefix(label)
	return ownerDispatchRuntimeError(
		fmt.Sprintf("%sowner dispatch unsupported: %s", prefix, info["reason"]),
		ownerDispatchBoundary,
		map[string]interface{}{"owner_dispatch": info},
	)
}

// AssertOwnerDispatchTargetSupported returns a structured diagnostic error when
// one owner-loop/executor target is unsupported.
func AssertOwnerDispatchTargetSupported(target, label string) error {
	info, err := OwnerDispatchTargetStatus(target)
	if err != nil {
		return err
	}
	if info["supported"] == true {
		return nil
	}
	prefix := labelPrefix(label)
	return ownerDispatchRuntimeError(
		fmt.Sprintf("%sowner dispatch target unsupported: %s: %s", prefix, info["target"], info["diagnostic"]),
		ownerDispatchTargetBoundary,
		map[string]interface{}{
			"target":                info["target"],
			"requested_target":      info["requested_target"],
			"owner_dispatch_target": info,
		},
	)
}

// AssertRubyNativeThreadsSupported returns a structured diagnostic error because
// embedded Ruby remains on OmniVM's single VM execution lane.
func AssertRubyNativeThreadsSupported(label string) error {
	info := RubyThreadingStatus()
	if info["native_threads_supported"] == true {
		return nil
	}
	prefix := labelPrefix(label)
	return ownerDispatchRuntimeError(
		fmt.Sprintf("%snative Ruby threads unsupported: mode=%s: %s", prefix, info["mode"], info["diagnostic"]),
		rubyThreadingBoundary,
		map[string]interface{}{"ruby_threading": info},
	)
}

func ownerDispatchRuntimeError(message, boundary string, details interface{}) *OwnerDispatchError {
	copiedDetails := cloneJSONValue(details)
	return &OwnerDispatchError{
		Runtime:       "go",
		OriginRuntime: "go",
		Type:          "RuntimeError",
		Message:       message,
		BoundaryPath:  boundary,
		Details:       copiedDetails,
		DetailsJSON:   jsonString(copiedDetails),
	}
}

func labelPrefix(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return ""
	}
	return label + ": "
}

func ownerDispatchTargetName(target string) string {
	normalized := strings.ToLower(strings.TrimSpace(target))
	normalized = strings.NewReplacer("-", "_", " ", "_", "\t", "_", "\n", "_").Replace(normalized)
	for strings.Contains(normalized, "__") {
		normalized = strings.ReplaceAll(normalized, "__", "_")
	}
	switch normalized {
	case "asyncio", "python", "python_loop", "python_async_loop", "py":
		return "python_asyncio"
	case "js", "javascript", "javascript_loop", "node", "nodejs", "event_loop":
		return "javascript_event_loop"
	case "java", "jvm", "executor":
		return "java_executor"
	case "ruby", "fiber", "thread", "ruby_fiber", "ruby_thread":
		return "ruby_fiber_thread"
	default:
		return normalized
	}
}

func ownerDispatchContract() map[string]interface{} {
	return map[string]interface{}{
		"mode":                     "diagnostic_only",
		"host_thread_required":     true,
		"owner_dispatch_supported": false,
		"foreign_thread_behavior":  "reject_runtime_calls",
		"reason":                   "owner dispatch is unsupported in this mode, so OmniVM will not route calls onto foreign owner loops",
		"owner_dispatch_targets": map[string]interface{}{
			"python_asyncio": map[string]interface{}{
				"supported":           false,
				"owner_kind":          "python_asyncio_loop",
				"required_capability": "run callback on owning asyncio loop",
				"current_behavior":    "Python async stream pulls and close have narrow pump-owned paths; general callbacks are not migrated back to the owner loop",
				"diagnostic":          "Python async streams have narrow pump-owned pull/close paths, but general callbacks are not migrated back to the owner loop",
				"narrow_capabilities": []interface{}{"python_async_stream_pull", "python_async_stream_close"},
			},
			"javascript_event_loop": map[string]interface{}{
				"supported":           false,
				"owner_kind":          "javascript_event_loop",
				"required_capability": "run callback on the owning JavaScript event loop",
				"current_behavior":    "JavaScript promises and timers are pumped at OmniVM call boundaries; foreign owner-loop callback dispatch is not available",
				"diagnostic":          "OmniVM does not currently route arbitrary callbacks back onto a JavaScript event loop owner",
			},
			"java_executor": map[string]interface{}{
				"supported":           false,
				"owner_kind":          "java_executor",
				"required_capability": "run callback on the owning Java Executor",
				"current_behavior":    "Java futures and reactive handles expose cancellation/status, but arbitrary callbacks are not migrated to a captured Executor",
				"diagnostic":          "OmniVM does not currently route arbitrary callbacks back onto a Java Executor owner",
			},
			"ruby_fiber_thread": map[string]interface{}{
				"supported":           false,
				"owner_kind":          "ruby_fiber_thread",
				"required_capability": "run callback on the owning Ruby Fiber or native Thread",
				"current_behavior":    "Ruby runs on the single VM thread with native Ruby thread scheduling disabled",
				"diagnostic":          "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported",
			},
		},
	}
}

func rubyThreadingContract() map[string]interface{} {
	return map[string]interface{}{
		"mode":                     "single_vm_thread",
		"native_threads_supported": false,
		"ruby_vm_thread":           "single_vm_thread",
		"thread_new_behavior":      "unsupported_diagnostic",
		"diagnostic":               "Ruby runs on the single VM thread; native Ruby thread scheduling and Puma-style in-process thread ownership remain unsupported",
		"app_server_boundary":      "Use Fiber/Async or single-thread Rack servers in process; run native-threaded Ruby app servers such as Puma out of process.",
	}
}

func cloneJSONObject(value map[string]interface{}) map[string]interface{} {
	cloned, _ := cloneJSONValue(value).(map[string]interface{})
	if cloned == nil {
		return map[string]interface{}{}
	}
	return cloned
}

func cloneJSONValue(value interface{}) interface{} {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return value
	}
	return out
}

func jsonString(value interface{}) string {
	if value == nil {
		return "null"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
