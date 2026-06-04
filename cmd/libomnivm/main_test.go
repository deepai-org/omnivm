package main

import (
	"testing"
	"time"

	"github.com/omnivm/omnivm/pkg/engine"
	"github.com/omnivm/omnivm/pkg/handles"
	"github.com/omnivm/omnivm/pkg/manifest"
	"github.com/omnivm/omnivm/pkg/watchdog"
)

func TestDrainFinalizerReleasesOnHostBoundary(t *testing.T) {
	prevEng := eng
	defer func() {
		eng = prevEng
		watchdog.SetActiveRuntime(watchdog.RuntimeNone)
	}()

	eng = engine.New()
	eng.GoldenThreadID = 42
	releases := 0
	id, err := eng.Handles.Register("payload", handles.RegisterOptions{
		Runtime: "python",
		Kind:    "object",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	if !eng.Handles.QueueReleaseFromFinalizer(id) {
		t.Fatal("queue finalizer release failed")
	}

	drainFinalizerReleasesOnHostBoundary(42)

	if releases != 1 {
		t.Fatalf("finalizer releases = %d, want 1", releases)
	}
	stats := eng.Handles.Stats(time.Now())
	if stats.Live != 0 || stats.FinalizerQueueLen != 0 || stats.FinalizerQueueDrains != 1 {
		t.Fatalf("bad handle stats after host-boundary drain: %+v", stats)
	}
}

func TestThreadAffinityStatusReportsDiagnosticOnlyDispatch(t *testing.T) {
	status := threadAffinityStatus(42)
	if status["mode"] != "diagnostic_only" {
		t.Fatalf("thread affinity mode = %v, want diagnostic_only", status["mode"])
	}
	if status["host_thread_id"] != int64(42) {
		t.Fatalf("thread affinity host thread = %v, want 42", status["host_thread_id"])
	}
	if status["owner_dispatch_supported"] != false {
		t.Fatalf("owner dispatch should be reported unsupported: %+v", status)
	}
	reason, ok := status["reason"].(string)
	if !ok || reason == "" {
		t.Fatalf("owner dispatch status omitted unsupported reason: %+v", status)
	}
	targets, ok := status["owner_dispatch_targets"].(map[string]interface{})
	if !ok {
		t.Fatalf("owner dispatch targets omitted: %+v", status)
	}
	for _, key := range []string{"python_asyncio", "javascript_event_loop", "java_executor", "ruby_fiber_thread"} {
		target, ok := targets[key].(map[string]interface{})
		if !ok {
			t.Fatalf("owner dispatch target %q omitted: %+v", key, targets)
		}
		if target["supported"] != false {
			t.Fatalf("owner dispatch target %q should report unsupported: %+v", key, target)
		}
		if target["diagnostic"] == "" {
			t.Fatalf("owner dispatch target %q omitted diagnostic: %+v", key, target)
		}
	}
	if status["python_assert_host_thread"] != true {
		t.Fatalf("Python host-thread assertion capability omitted: %+v", status)
	}
	if status["ruby_vm_thread"] != "single_vm_thread" {
		t.Fatalf("Ruby VM thread boundary omitted: %+v", status)
	}
}

func TestDrainFinalizerReleasesSkipsActiveRuntime(t *testing.T) {
	prevEng := eng
	defer func() {
		eng = prevEng
		watchdog.SetActiveRuntime(watchdog.RuntimeNone)
	}()

	eng = engine.New()
	eng.GoldenThreadID = 42
	releases := 0
	id, err := eng.Handles.Register("payload", handles.RegisterOptions{
		Runtime: "javascript",
		Kind:    "object",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	if !eng.Handles.QueueReleaseFromFinalizer(id) {
		t.Fatal("queue finalizer release failed")
	}

	watchdog.SetActiveRuntime(watchdog.RuntimeJavaScript)
	drainFinalizerReleasesOnHostBoundary(42)

	if releases != 0 {
		t.Fatalf("release ran while guest runtime was active")
	}
	stats := eng.Handles.Stats(time.Now())
	if stats.Live != 1 || stats.FinalizerQueueLen != 1 {
		t.Fatalf("active-runtime drain should leave queue intact: %+v", stats)
	}

	watchdog.SetActiveRuntime(watchdog.RuntimeNone)
	drainFinalizerReleasesOnHostBoundary(42)
	if releases != 1 {
		t.Fatalf("release after active runtime cleared = %d, want 1", releases)
	}
}

func TestDrainFinalizerReleasesSkipsActiveManifest(t *testing.T) {
	prevEng := eng
	prevManifest := manifestExecutor
	defer func() {
		eng = prevEng
		manifestExecutor = prevManifest
		watchdog.SetActiveRuntime(watchdog.RuntimeNone)
	}()

	eng = engine.New()
	eng.GoldenThreadID = 42
	manifestExecutor = manifest.NewExecutorWithHandles(eng.Runtimes, eng.Handles)
	releases := 0
	id, err := eng.Handles.Register("payload", handles.RegisterOptions{
		Runtime: "python",
		Kind:    "object",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register handle: %v", err)
	}
	if !eng.Handles.QueueReleaseFromFinalizer(id) {
		t.Fatal("queue finalizer release failed")
	}

	drainFinalizerReleasesOnHostBoundary(42)
	if releases != 0 {
		t.Fatalf("release ran while manifest executor was active")
	}

	manifestExecutor = nil
	drainFinalizerReleasesOnHostBoundary(42)
	if releases != 1 {
		t.Fatalf("release after manifest cleared = %d, want 1", releases)
	}
}

func TestWorkerDrainReleasesAllProcessHandles(t *testing.T) {
	prevEng := eng
	prevManifest := manifestExecutor
	prevModules := manifestModules
	defer func() {
		eng = prevEng
		manifestExecutor = prevManifest
		manifestModules = prevModules
	}()

	eng = engine.New()
	manifestExecutor = manifest.NewExecutorWithHandles(eng.Runtimes, eng.Handles)
	manifestModules = map[string]*manifest.Executor{
		"demo": manifest.NewExecutorWithHandles(eng.Runtimes, eng.Handles),
	}
	releases := 0
	if _, err := eng.Handles.Register("payload", handles.RegisterOptions{
		Runtime: "python",
		Kind:    "object",
		Release: func(value any) error {
			releases++
			return nil
		},
	}); err != nil {
		t.Fatalf("register handle: %v", err)
	}
	retained, err := eng.Handles.Register("retained", handles.RegisterOptions{
		Runtime: "javascript",
		Kind:    "proxy",
		Release: func(value any) error {
			releases++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("register retained handle: %v", err)
	}
	if err := eng.Handles.Retain(retained); err != nil {
		t.Fatalf("retain handle: %v", err)
	}
	if err := eng.Handles.Escape(retained); err != nil {
		t.Fatalf("escape handle: %v", err)
	}

	if err := unloadManifestModulesForWorkerDrain(); err != nil {
		t.Fatalf("unload manifest modules: %v", err)
	}

	if releases != 2 {
		t.Fatalf("handle releases = %d, want 2", releases)
	}
	if manifestExecutor != nil || len(manifestModules) != 0 {
		t.Fatalf("manifest modules not cleared: active=%v modules=%d", manifestExecutor, len(manifestModules))
	}
	stats := eng.Handles.Stats(time.Now())
	if stats.Live != 0 || stats.ScopeReleases != 2 {
		t.Fatalf("bad handle stats after manifest unload: %+v", stats)
	}
}
