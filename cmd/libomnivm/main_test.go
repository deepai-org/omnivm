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

func TestUnloadManifestModulesForWorkerDrainReleasesHandles(t *testing.T) {
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

	if err := unloadManifestModulesForWorkerDrain(); err != nil {
		t.Fatalf("unload manifest modules: %v", err)
	}

	if releases != 1 {
		t.Fatalf("handle releases = %d, want 1", releases)
	}
	if manifestExecutor != nil || len(manifestModules) != 0 {
		t.Fatalf("manifest modules not cleared: active=%v modules=%d", manifestExecutor, len(manifestModules))
	}
	stats := eng.Handles.Stats(time.Now())
	if stats.Live != 0 || stats.ScopeReleases != 1 {
		t.Fatalf("bad handle stats after manifest unload: %+v", stats)
	}
}
