package manifest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestGoOwnerDispatchStatusReportsDiagnosticOnly(t *testing.T) {
	status := OwnerDispatchStatus()
	if status["mode"] != "diagnostic_only" || status["owner_dispatch_supported"] != false {
		t.Fatalf("OwnerDispatchStatus = %#v, want diagnostic-only unsupported", status)
	}
	if status["host_thread_required"] != true {
		t.Fatalf("OwnerDispatchStatus host_thread_required = %#v, want true", status["host_thread_required"])
	}
	targets, ok := status["owner_dispatch_targets"].(map[string]interface{})
	if !ok {
		t.Fatalf("owner_dispatch_targets = %T, want map", status["owner_dispatch_targets"])
	}
	if _, ok := targets["javascript_event_loop"]; !ok {
		t.Fatalf("missing javascript_event_loop target: %#v", targets)
	}
	targets["javascript_event_loop"] = map[string]interface{}{"supported": true}
	nextTargets := OwnerDispatchStatus()["owner_dispatch_targets"].(map[string]interface{})
	nextJS := nextTargets["javascript_event_loop"].(map[string]interface{})
	if nextJS["supported"] != false {
		t.Fatalf("OwnerDispatchStatus leaked mutable target state: %#v", nextJS)
	}
}

func TestGoOwnerDispatchTargetStatusAliasesAndUnknowns(t *testing.T) {
	for alias, want := range map[string]string{
		"asyncio":           "python_asyncio",
		"python_loop":       "python_asyncio",
		"python async loop": "python_asyncio",
		"JavaScript":        "javascript_event_loop",
		"nodejs":            "javascript_event_loop",
		"event loop":        "javascript_event_loop",
		"java-executor":     "java_executor",
		"fiber":             "ruby_fiber_thread",
		"thread":            "ruby_fiber_thread",
		"ruby fiber":        "ruby_fiber_thread",
	} {
		info, err := OwnerDispatchTargetStatus(alias)
		if err != nil {
			t.Fatalf("OwnerDispatchTargetStatus(%q): %v", alias, err)
		}
		if info["target"] != want || info["requested_target"] != alias || info["supported"] != false {
			t.Fatalf("OwnerDispatchTargetStatus(%q) = %#v, want target %q unsupported", alias, info, want)
		}
		info["supported"] = true
		again, err := OwnerDispatchTargetStatus(alias)
		if err != nil {
			t.Fatalf("OwnerDispatchTargetStatus(%q) again: %v", alias, err)
		}
		if again["supported"] != false {
			t.Fatalf("OwnerDispatchTargetStatus leaked mutable state for %q: %#v", alias, again)
		}
	}

	_, err := OwnerDispatchTargetStatus("unknown-loop")
	var dispatchErr *OwnerDispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("unknown target err = %T %v, want *OwnerDispatchError", err, err)
	}
	if dispatchErr.BoundaryPath != "owner_dispatch_target" || !strings.Contains(dispatchErr.Error(), "unknown owner dispatch target: unknown-loop") {
		t.Fatalf("unknown target diagnostic mismatch: %#v", dispatchErr)
	}
	details := dispatchErr.Details.(map[string]interface{})
	targetDetails := details["owner_dispatch_target"].(map[string]interface{})
	if targetDetails["target"] != "unknown_loop" || targetDetails["requested_target"] != "unknown-loop" {
		t.Fatalf("unknown target details mismatch: %#v", targetDetails)
	}
}

func TestGoOwnerDispatchAssertErrorsUseStructuredEnvelope(t *testing.T) {
	err := AssertOwnerDispatchSupported("django startup")
	var dispatchErr *OwnerDispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("AssertOwnerDispatchSupported err = %T %v, want *OwnerDispatchError", err, err)
	}
	if dispatchErr.BoundaryPath != "owner_dispatch" || !strings.Contains(dispatchErr.Error(), "django startup: owner dispatch unsupported") {
		t.Fatalf("owner dispatch diagnostic mismatch: %#v", dispatchErr)
	}
	details := dispatchErr.Details.(map[string]interface{})
	dispatch := details["owner_dispatch"].(map[string]interface{})
	if dispatch["owner_dispatch_supported"] != false {
		t.Fatalf("owner dispatch details mismatch: %#v", dispatch)
	}
	envelope := dispatchErr.ToMap()
	if envelope["runtime"] != "go" || envelope["origin_runtime"] != "go" || envelope["type"] != "RuntimeError" || envelope["boundary_path"] != "owner_dispatch" {
		t.Fatalf("owner dispatch envelope mismatch: %#v", envelope)
	}
	if err := json.Unmarshal(mustJSON(dispatchErr), &envelope); err != nil {
		t.Fatalf("OwnerDispatchError JSON envelope did not marshal: %v", err)
	}
	if envelope["boundary_path"] != "owner_dispatch" {
		t.Fatalf("marshaled owner dispatch envelope mismatch: %#v", envelope)
	}

	envelopeDetails := envelope["details"].(map[string]interface{})["owner_dispatch"].(map[string]interface{})
	envelopeDetails["mode"] = "mutated"
	again := dispatchErr.ToMap()["details"].(map[string]interface{})["owner_dispatch"].(map[string]interface{})
	if again["mode"] != "diagnostic_only" {
		t.Fatalf("OwnerDispatchError ToMap leaked mutable details: %#v", again)
	}
}

func TestGoOwnerDispatchTargetAndRubyThreadingAssertErrors(t *testing.T) {
	err := AssertOwnerDispatchTargetSupported("ruby", "async bridge")
	var dispatchErr *OwnerDispatchError
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("AssertOwnerDispatchTargetSupported err = %T %v, want *OwnerDispatchError", err, err)
	}
	if dispatchErr.BoundaryPath != "owner_dispatch_target" || !strings.Contains(dispatchErr.Error(), "async bridge: owner dispatch target unsupported: ruby_fiber_thread") {
		t.Fatalf("target diagnostic mismatch: %#v", dispatchErr)
	}
	targetDetails := dispatchErr.Details.(map[string]interface{})["owner_dispatch_target"].(map[string]interface{})
	if targetDetails["target"] != "ruby_fiber_thread" || targetDetails["requested_target"] != "ruby" {
		t.Fatalf("target details mismatch: %#v", targetDetails)
	}

	rubyStatus := RubyThreadingStatus()
	if rubyStatus["mode"] != "single_vm_thread" || rubyStatus["native_threads_supported"] != false {
		t.Fatalf("RubyThreadingStatus = %#v, want single VM thread unsupported", rubyStatus)
	}
	rubyStatus["native_threads_supported"] = true
	if RubyThreadingStatus()["native_threads_supported"] != false {
		t.Fatal("RubyThreadingStatus leaked mutable state")
	}

	err = AssertRubyNativeThreadsSupported("puma startup")
	if !errors.As(err, &dispatchErr) {
		t.Fatalf("AssertRubyNativeThreadsSupported err = %T %v, want *OwnerDispatchError", err, err)
	}
	if dispatchErr.BoundaryPath != "ruby_threading" || !strings.Contains(dispatchErr.Error(), "puma startup: native Ruby threads unsupported: mode=single_vm_thread") {
		t.Fatalf("ruby threading diagnostic mismatch: %#v", dispatchErr)
	}
	threading := dispatchErr.Details.(map[string]interface{})["ruby_threading"].(map[string]interface{})
	if threading["native_threads_supported"] != false {
		t.Fatalf("ruby threading details mismatch: %#v", threading)
	}
}

func mustJSON(value interface{}) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
