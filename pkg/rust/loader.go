// Package rust provides the Rust runtime: user source compiles to a cdylib
// with a stable C ABI, loads in-process via dlopen (RTLD_LOCAL), and calls
// other runtimes through the bridge. One artifact works in both the OmniVM
// binary and libomnivm.so c-shared deployments — no plugin/c-shared split.
package rust

/*
#include <stdlib.h>
#include <dlfcn.h>

typedef char* (*omnivm_rs_call_fn)(char*);
typedef void (*omnivm_rs_set_bridge_fn)(void*, void*);
typedef int  (*omnivm_rs_int_fn)(void);
typedef char* (*omnivm_rs_drive_fn)(unsigned long long, unsigned long long, int);
typedef char* (*omnivm_rs_str0_fn)(void);
typedef int  (*omnivm_rs_release_fn)(unsigned long long);
typedef int  (*omnivm_rs_complete_fn)(unsigned long long, int, const char*);
typedef int  (*omnivm_rs_mode_fn)(int);

static void* omnivm_rust_dlopen(const char* path) {
	return dlopen(path, RTLD_NOW | RTLD_LOCAL);
}
static void* omnivm_rust_dlsym(void* h, const char* n) { return dlsym(h, n); }
static const char* omnivm_rust_dlerror(void) { return dlerror(); }
static char* omnivm_rust_call_str(void* fn, char* arg) { return ((omnivm_rs_call_fn)fn)(arg); }
static int omnivm_rust_call_int(void* fn) { return ((omnivm_rs_int_fn)fn)(); }
static void omnivm_rust_set_bridge(void* fn, void* call, void* freep) {
	((omnivm_rs_set_bridge_fn)fn)(call, freep);
}
static char* omnivm_rust_drive(void* fn, unsigned long long h, unsigned long long ms, int fd) {
	return ((omnivm_rs_drive_fn)fn)(h, ms, fd);
}
static char* omnivm_rust_str0(void* fn) { return ((omnivm_rs_str0_fn)fn)(); }
static int omnivm_rust_release(void* fn, unsigned long long h) { return ((omnivm_rs_release_fn)fn)(h); }
static int omnivm_rust_complete(void* fn, unsigned long long id, int ok, const char* payload) {
	return ((omnivm_rs_complete_fn)fn)(id, ok, payload);
}
static int omnivm_rust_mode(void* fn, int mode) { return ((omnivm_rs_mode_fn)fn)(mode); }

typedef unsigned long long (*omnivm_rs_spawn_blocking_fn)(unsigned long long, const char*);
static unsigned long long omnivm_rust_spawn_blocking(void* fn, unsigned long long ptr, const char* args) {
	return ((omnivm_rs_spawn_blocking_fn)fn)(ptr, args);
}

#cgo LDFLAGS: -ldl
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

// Support is the loaded libomnivm_rs.so — the runtime-support dylib that owns
// the (single) tokio runtime, the bridge statics, the future table, and the
// object handle table. Units link it dynamically, so installing the bridge
// here installs it for every unit.
type Support struct {
	handle unsafe.Pointer

	setBridge     unsafe.Pointer
	abiVersion    unsafe.Pointer
	pump          unsafe.Pointer
	drive         unsafe.Pointer
	releaseFut    unsafe.Pointer
	completeBr    unsafe.Pointer
	setExecutor   unsafe.Pointer
	completionFD  unsafe.Pointer
	stats         unsafe.Pointer
	spawnBg       unsafe.Pointer
	spawnBlocking unsafe.Pointer

	mu              sync.Mutex
	bridgeInstalled bool
	bridgeIsGo      bool
}

var (
	supportOnce sync.Once
	supportLib  *Support
	supportErr  error
)

// GetSupport loads the support dylib once per process.
func GetSupport() (*Support, error) {
	supportOnce.Do(func() {
		tc, err := GetToolchain()
		if err != nil {
			supportErr = err
			return
		}
		supportLib, supportErr = loadSupport(tc.SupportDylib)
	})
	return supportLib, supportErr
}

func dlopen(path string) (unsafe.Pointer, error) {
	cPath := C.CString(path)
	defer C.free(unsafe.Pointer(cPath))
	h := C.omnivm_rust_dlopen(cPath)
	if h == nil {
		return nil, fmt.Errorf("dlopen %s: %s", path, C.GoString(C.omnivm_rust_dlerror()))
	}
	return h, nil
}

func dlsym(h unsafe.Pointer, name string) (unsafe.Pointer, error) {
	cName := C.CString(name)
	defer C.free(unsafe.Pointer(cName))
	sym := C.omnivm_rust_dlsym(h, cName)
	if sym == nil {
		return nil, fmt.Errorf("dlsym %s: %s", name, C.GoString(C.omnivm_rust_dlerror()))
	}
	return sym, nil
}

func loadSupport(path string) (*Support, error) {
	h, err := dlopen(path)
	if err != nil {
		return nil, fmt.Errorf("rust support dylib: %w", err)
	}
	s := &Support{handle: h}
	syms := map[string]*unsafe.Pointer{
		"omnivm_set_bridge_v1":         &s.setBridge,
		"omnivm_abi_version_v1":        &s.abiVersion,
		"omnivm_rs_pump_v1":            &s.pump,
		"omnivm_rs_drive_v1":           &s.drive,
		"omnivm_rs_release_future_v1":  &s.releaseFut,
		"omnivm_rs_complete_bridge_v1": &s.completeBr,
		"omnivm_rs_set_executor_v1":    &s.setExecutor,
		"omnivm_rs_completion_fd_v1":   &s.completionFD,
		"omnivm_rs_stats_v1":           &s.stats,
		"omnivm_rs_spawn_background_v1": &s.spawnBg,
		"omnivm_rs_spawn_blocking_v1":   &s.spawnBlocking,
	}
	for name, dst := range syms {
		sym, symErr := dlsym(h, name)
		if symErr != nil {
			// Versioned-symbol contract: refusing to load is a structured
			// error, not a crash later.
			return nil, fmt.Errorf("rust support dylib %s does not speak this host's bridge ABI (missing %s): %w", path, name, symErr)
		}
		*dst = sym
	}
	if rev := int(C.omnivm_rust_call_int(s.abiVersion)); rev != ABIRev {
		return nil, fmt.Errorf("rust support dylib ABI revision %d, host speaks %d — rebuild required", rev, ABIRev)
	}
	return s, nil
}

// callOwnedString invokes a fn returning a malloc'd char* and frees it.
func callOwnedString(raw *C.char) string {
	if raw == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(raw))
	return C.GoString(raw)
}

// InstallBridge installs the host bridge function pointers into the support
// dylib (and therefore every unit). callPtr/freePtr follow the OmniCall /
// OmniFree contract; zero pointers select the Go-closure trampolines.
func (s *Support) InstallBridge(callPtr, freePtr uintptr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	call := unsafe.Pointer(callPtr)
	free := unsafe.Pointer(freePtr)
	if callPtr == 0 {
		call = goBridgeCallPtr()
		free = goBridgeFreePtr()
	}
	C.omnivm_rust_set_bridge(s.setBridge, call, free)
	s.bridgeInstalled = true
	s.bridgeIsGo = callPtr == 0
}

func (s *Support) BridgeInstalled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridgeInstalled
}

// BridgeIsGo reports whether the installed bridge routes through the Go
// trampolines (vs host C pointers). Trampoline routing is process-global and
// must be refreshed when a new executor takes over.
func (s *Support) BridgeIsGo() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bridgeIsGo
}

// Pump runs one dispatcher-cycle tick of the tokio runtime; the returned JSON
// may carry outbound bridge calls.
func (s *Support) Pump() string {
	return callOwnedString(C.omnivm_rust_call_str(s.pump, nil))
}

// Drive runs one park of the re-park loop for an await handle.
func (s *Support) Drive(handle uint64, sliceMs uint64, taskFD int) string {
	return callOwnedString(C.omnivm_rust_drive(s.drive, C.ulonglong(handle), C.ulonglong(sliceMs), C.int(taskFD)))
}

// ReleaseFuture releases an abandoned await handle (idempotent).
func (s *Support) ReleaseFuture(handle uint64) bool {
	return C.omnivm_rust_release(s.releaseFut, C.ulonglong(handle)) != 0
}

// CompleteBridge completes an outbound bridge request surfaced by Drive/Pump.
func (s *Support) CompleteBridge(id uint64, ok bool, payload string) bool {
	cPayload := C.CString(payload)
	defer C.free(unsafe.Pointer(cPayload))
	okInt := C.int(0)
	if ok {
		okInt = 1
	}
	return C.omnivm_rust_complete(s.completeBr, C.ulonglong(id), okInt, cPayload) != 0
}

// SetExecutor selects the executor (0=current-thread, 1=multi); load-time only.
func (s *Support) SetExecutor(mode int) bool {
	return C.omnivm_rust_mode(s.setExecutor, C.int(mode)) != 0
}

// CompletionFD returns the multi-executor completion eventfd, or -1.
func (s *Support) CompletionFD() int {
	return int(C.omnivm_rust_call_int(s.completionFD))
}

// Stats returns runtime diagnostics JSON.
func (s *Support) Stats() string {
	return callOwnedString(C.omnivm_rust_call_str(s.stats, nil))
}

// SpawnBackground converts a stored future into a background LocalSet task
// (`go expr` semantics: progress on pump ticks and during other parks).
func (s *Support) SpawnBackground(handle uint64) bool {
	return C.omnivm_rust_release(s.spawnBg, C.ulonglong(handle)) != 0
}

// SpawnBlocking runs a synchronous unit export on tokio's blocking pool and
// returns an await handle (0 on failure).
func (s *Support) SpawnBlocking(fnAddr uintptr, argsJSON string) uint64 {
	cArgs := C.CString(argsJSON)
	defer C.free(unsafe.Pointer(cArgs))
	return uint64(C.omnivm_rust_spawn_blocking(s.spawnBlocking, C.ulonglong(fnAddr), cArgs))
}

// Unit is a loaded user cdylib.
type Unit struct {
	handle unsafe.Pointer
	path   string
}

var (
	unitsMu     sync.Mutex
	loadedUnits = map[string]*Unit{}
)

// LoadUnit dlopens a compiled unit (RTLD_NOW|RTLD_LOCAL), verifies the
// baked-in ABI revision, and returns the cached handle if already loaded.
// The support dylib must be loaded (and its bridge installed) first.
func LoadUnit(path string) (*Unit, error) {
	unitsMu.Lock()
	defer unitsMu.Unlock()
	if u, ok := loadedUnits[path]; ok {
		return u, nil
	}
	h, err := dlopen(path)
	if err != nil {
		return nil, fmt.Errorf("rust unit: %w", err)
	}
	abiSym, err := dlsym(h, "omnivm_unit_abi_v1")
	if err != nil {
		return nil, fmt.Errorf("rust unit %s does not carry an ABI marker (built without omnivm::unit_abi_marker!): %w", path, err)
	}
	if rev := int(C.omnivm_rust_call_int(abiSym)); rev != ABIRev {
		return nil, fmt.Errorf("rust unit %s was built against bridge ABI revision %d; this host speaks %d — the artifact is stale and was refused", path, rev, ABIRev)
	}
	u := &Unit{handle: h, path: path}
	loadedUnits[path] = u
	return u, nil
}

// Handle exposes the raw dlopen handle so the manifest layer can reuse the
// shared c-shared envelope/object-proxy machinery against this unit.
func (u *Unit) Handle() uintptr { return uintptr(u.handle) }

func (u *Unit) Path() string { return u.path }

// Call invokes an exported `char* fn(char*)` symbol with a JSON payload.
func (u *Unit) Call(symbol, argsJSON string) (string, error) {
	sym, err := dlsym(u.handle, symbol)
	if err != nil {
		return "", err
	}
	cArgs := C.CString(argsJSON)
	defer C.free(unsafe.Pointer(cArgs))
	return callOwnedString(C.omnivm_rust_call_str(sym, cArgs)), nil
}

// SymbolAddr resolves an exported symbol's raw address (for blocking-pool
// dispatch of sync exports).
func (u *Unit) SymbolAddr(symbol string) (uintptr, error) {
	sym, err := dlsym(u.handle, symbol)
	if err != nil {
		return 0, err
	}
	return uintptr(sym), nil
}

// loadedUnitPath reports whether an artifact path is dlopen'd in this process
// (loaded units are never pruned from the cache).
func loadedUnitPath(path string) (*Unit, bool) {
	unitsMu.Lock()
	defer unitsMu.Unlock()
	u, ok := loadedUnits[path]
	return u, ok
}
