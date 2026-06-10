package rust

// Go-closure bridge trampolines. Hosts that have C bridge pointers (OmniCall/
// OmniFree) pass them straight through; hosts that work with Go closures (the
// manifest executor, tests) install these exported trampolines instead.

/*
#include <stdlib.h>

extern char* omnivmRustBridgeCallGo(char* runtime, char* code);
extern void omnivmRustBridgeFreeGo(char* p);

static void* omnivm_rust_go_bridge_call_ptr(void) { return (void*)omnivmRustBridgeCallGo; }
static void* omnivm_rust_go_bridge_free_ptr(void) { return (void*)omnivmRustBridgeFreeGo; }
*/
import "C"

import (
	"sync"
	"unsafe"
)

var (
	goBridgeMu sync.RWMutex
	goBridgeFn func(runtime, code string) string
)

// SetGoBridge installs the process-wide Go bridge closure used by the
// trampolines. The closure must return "ERR:..." for failures.
func SetGoBridge(fn func(runtime, code string) string) {
	goBridgeMu.Lock()
	goBridgeFn = fn
	goBridgeMu.Unlock()
}

//export omnivmRustBridgeCallGo
func omnivmRustBridgeCallGo(runtime *C.char, code *C.char) *C.char {
	goBridgeMu.RLock()
	fn := goBridgeFn
	goBridgeMu.RUnlock()
	if fn == nil {
		return C.CString("ERR:rust bridge not configured")
	}
	return C.CString(fn(C.GoString(runtime), C.GoString(code)))
}

//export omnivmRustBridgeFreeGo
func omnivmRustBridgeFreeGo(p *C.char) {
	if p != nil {
		C.free(unsafe.Pointer(p))
	}
}

func goBridgeCallPtr() unsafe.Pointer { return C.omnivm_rust_go_bridge_call_ptr() }
func goBridgeFreePtr() unsafe.Pointer { return C.omnivm_rust_go_bridge_free_ptr() }
