package watchdog

import (
	"testing"
)

func TestConstants(t *testing.T) {
	if RuntimeNone != 0 {
		t.Errorf("RuntimeNone = %d, want 0", RuntimeNone)
	}
	if RuntimePython != 1 {
		t.Errorf("RuntimePython = %d, want 1", RuntimePython)
	}
	if RuntimeJavaScript != 2 {
		t.Errorf("RuntimeJavaScript = %d, want 2", RuntimeJavaScript)
	}
	if RuntimeRuby != 3 {
		t.Errorf("RuntimeRuby = %d, want 3", RuntimeRuby)
	}
	if RuntimeJVM != 4 {
		t.Errorf("RuntimeJVM = %d, want 4", RuntimeJVM)
	}
}

func TestInitDoesNotPanic(t *testing.T) {
	Init()
}

func TestArmDisarmDoNotPanic(t *testing.T) {
	Init()
	Arm(1000)
	Disarm()
}

func TestSetActiveRuntime(t *testing.T) {
	Init()
	SetActiveRuntime(RuntimePython)
	got := GetActiveRuntime()
	// On non-Linux, GetActiveRuntime always returns RuntimeNone (stub)
	// On Linux, it would return RuntimePython
	// Just verify no panic and valid return
	if got != RuntimeNone && got != RuntimePython {
		t.Errorf("GetActiveRuntime() = %d, unexpected", got)
	}
}

func TestSetInterruptPointersDoNotPanic(t *testing.T) {
	Init()
	SetPythonInterrupt(nil)
	SetV8Terminate(nil)
	SetRubyInterrupt(nil)
}

func TestShutdownDoesNotPanic(t *testing.T) {
	Init()
	Shutdown()
}

func TestRapidArmDisarm(t *testing.T) {
	Init()
	for i := 0; i < 100; i++ {
		Arm(100)
		Disarm()
	}
}
