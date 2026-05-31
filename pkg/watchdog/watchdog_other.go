//go:build !linux

// Package watchdog provides stubs on non-Linux platforms.
// The real implementation uses C pthreads and Linux-specific signals.
package watchdog

import "unsafe"

const (
	RuntimeNone       = 0
	RuntimePython     = 1
	RuntimeJavaScript = 2
	RuntimeRuby       = 3
	RuntimeJVM        = 4
	RuntimeGo         = 5
)

func Init()                                {}
func SetPythonInterrupt(fn unsafe.Pointer) {}
func SetV8Terminate(fn unsafe.Pointer)     {}
func SetRubyInterrupt(fn unsafe.Pointer)   {}
func SetJVMInterrupt(fn unsafe.Pointer)    {}
func Arm(timeoutMS int)                    {}
func Disarm()                              {}
func SetActiveRuntime(rt int)              {}
func GetActiveRuntime() int                { return RuntimeNone }
func Shutdown()                            {}
