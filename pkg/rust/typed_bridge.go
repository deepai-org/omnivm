package rust

// Typed OUTBOUND bridge trampoline (rust → host): scalar calls to
// host-registered functions cross as omni_value_t with no JSON text —
// CallTypedByAddr's encoding in REVERSE. The support dylib receives this
// trampoline through omnivm_rs_set_typed_bridge_v1 (mirroring how
// omnivm_set_bridge_v1 installs the OmniCall/OmniFree pointers).
//
// Ownership contract (the mirror of the inbound lane): argument strings stay
// rust-owned and are only borrowed for the duration of the call (Go copies);
// the RESULT string is malloc'd here (C.CString) and freed by the crate.

/*
#include <stdlib.h>

typedef struct {
	long long tag;
	union {
		long long i;
		double f;
		struct { char* ptr; long long len; } s;
		unsigned long long ref;
	} v;
} omnivm_rs_typed_value_t;

extern int omnivmRustTypedBridgeGo(char* runtime, char* func, omnivm_rs_typed_value_t* args, int nargs, omnivm_rs_typed_value_t* out);

static void* omnivm_rust_go_typed_bridge_ptr(void) { return (void*)omnivmRustTypedBridgeGo; }

typedef void (*omnivm_rs_set_typed_bridge_fn)(void*);
static void omnivm_rust_set_typed_bridge(void* fn, void* call) {
	((omnivm_rs_set_typed_bridge_fn)fn)(call);
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

// TypedResult is the router's projection of one call result: Scalar when the
// value fits the omni_value_t scalar tags, JSON wire text otherwise (the
// crate decodes OMNI_TAG_JSON losslessly).
type TypedResult struct {
	Scalar interface{}
	JSON   string
	IsJSON bool
}

// GoTypedBridgeFn services one typed outbound call. handled=false means the
// host does not speak typed for this target and NOTHING was executed — the
// crate falls back to the JSON lane.
type GoTypedBridgeFn func(runtime, fn string, args []interface{}) (TypedResult, bool, error)

var (
	goTypedBridgeMu sync.RWMutex
	goTypedBridgeFn GoTypedBridgeFn
)

// TypedBridgeCallCount counts serviced (handled) typed outbound calls —
// observability for tests and benchmarks, additive like TypedCallCount.
var TypedBridgeCallCount uint64

// SetGoTypedBridge installs the process-wide typed outbound router (mirrors
// SetGoBridge: a new executor must refresh it before installing the bridge).
func SetGoTypedBridge(fn GoTypedBridgeFn) {
	goTypedBridgeMu.Lock()
	goTypedBridgeFn = fn
	goTypedBridgeMu.Unlock()
}

// InstallTypedBridge points the support dylib's typed-bridge static at the Go
// trampoline. Idempotent; routing goes through the closure installed with
// SetGoTypedBridge.
func (s *Support) InstallTypedBridge() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setTypedBridge != nil {
		C.omnivm_rust_set_typed_bridge(s.setTypedBridge, C.omnivm_rust_go_typed_bridge_ptr())
	}
}

// rc contract shared with the crate (abi::TYPED_BRIDGE_*).
const (
	typedBridgeOK       = 0
	typedBridgeErr      = 1
	typedBridgeFallback = 2
)

//export omnivmRustTypedBridgeGo
func omnivmRustTypedBridgeGo(runtime *C.char, fn *C.char, args *C.omnivm_rs_typed_value_t, nargs C.int, out *C.omnivm_rs_typed_value_t) C.int {
	goTypedBridgeMu.RLock()
	bridge := goTypedBridgeFn
	goTypedBridgeMu.RUnlock()
	if bridge == nil {
		return typedBridgeFallback
	}
	goArgs, err := decodeTypedBridgeArgs(args, int(nargs))
	if err != nil {
		writeTypedBridgeText(out, omniTagError, err.Error())
		return typedBridgeErr
	}
	result, handled, err := bridge(C.GoString(runtime), C.GoString(fn), goArgs)
	if !handled {
		return typedBridgeFallback
	}
	atomic.AddUint64(&TypedBridgeCallCount, 1)
	if err != nil {
		writeTypedBridgeText(out, omniTagError, err.Error())
		return typedBridgeErr
	}
	if result.IsJSON {
		writeTypedBridgeText(out, omniTagJSON, result.JSON)
		return typedBridgeOK
	}
	if !writeTypedBridgeScalar(out, result.Scalar) {
		// Defensive: a router that mislabels a structured value as Scalar
		// still answers losslessly through the JSON tag.
		encoded, jsonErr := json.Marshal(result.Scalar)
		if jsonErr != nil {
			writeTypedBridgeText(out, omniTagError, fmt.Sprintf("typed bridge: unencodable result %T", result.Scalar))
			return typedBridgeErr
		}
		writeTypedBridgeText(out, omniTagJSON, string(encoded))
	}
	return typedBridgeOK
}

func typedBridgeValueAt(args *C.omnivm_rs_typed_value_t, i int) *C.omnivm_rs_typed_value_t {
	return (*C.omnivm_rs_typed_value_t)(unsafe.Pointer(uintptr(unsafe.Pointer(args)) + uintptr(i)*unsafe.Sizeof(*args)))
}

type typedBridgeString struct {
	ptr *C.char
	len C.longlong
}

// decodeTypedBridgeArgs copies the borrowed omni_value_t args into native Go
// values (strings are copied, never freed — they stay rust-owned).
func decodeTypedBridgeArgs(args *C.omnivm_rs_typed_value_t, n int) ([]interface{}, error) {
	out := make([]interface{}, 0, n)
	for i := 0; i < n; i++ {
		v := typedBridgeValueAt(args, i)
		switch int64(v.tag) {
		case omniTagNull:
			out = append(out, nil)
		case omniTagBool:
			out = append(out, *(*C.longlong)(unsafe.Pointer(&v.v)) != 0)
		case omniTagI64:
			out = append(out, int64(*(*C.longlong)(unsafe.Pointer(&v.v))))
		case omniTagF64:
			out = append(out, float64(*(*C.double)(unsafe.Pointer(&v.v))))
		case omniTagString:
			str := (*typedBridgeString)(unsafe.Pointer(&v.v))
			out = append(out, C.GoStringN(str.ptr, C.int(str.len)))
		case omniTagJSON:
			str := (*typedBridgeString)(unsafe.Pointer(&v.v))
			var decoded interface{}
			if err := json.Unmarshal([]byte(C.GoStringN(str.ptr, C.int(str.len))), &decoded); err != nil {
				return nil, fmt.Errorf("typed bridge: argument %d (json): %w", i, err)
			}
			out = append(out, decoded)
		default:
			return nil, fmt.Errorf("typed bridge: unsupported argument tag %d", int64(v.tag))
		}
	}
	return out, nil
}

func writeTypedBridgeText(out *C.omnivm_rs_typed_value_t, tag int64, text string) {
	out.tag = C.longlong(tag)
	str := (*typedBridgeString)(unsafe.Pointer(&out.v))
	str.ptr = C.CString(text)
	str.len = C.longlong(len(text))
}

func writeTypedBridgeScalar(out *C.omnivm_rs_typed_value_t, value interface{}) bool {
	switch v := value.(type) {
	case nil:
		out.tag = omniTagNull
		*(*C.longlong)(unsafe.Pointer(&out.v)) = 0
	case bool:
		out.tag = omniTagBool
		n := C.longlong(0)
		if v {
			n = 1
		}
		*(*C.longlong)(unsafe.Pointer(&out.v)) = n
	case int:
		out.tag = omniTagI64
		*(*C.longlong)(unsafe.Pointer(&out.v)) = C.longlong(v)
	case int32:
		out.tag = omniTagI64
		*(*C.longlong)(unsafe.Pointer(&out.v)) = C.longlong(v)
	case int64:
		out.tag = omniTagI64
		*(*C.longlong)(unsafe.Pointer(&out.v)) = C.longlong(v)
	case uint:
		out.tag = omniTagI64
		*(*C.longlong)(unsafe.Pointer(&out.v)) = C.longlong(v)
	case uint32:
		out.tag = omniTagI64
		*(*C.longlong)(unsafe.Pointer(&out.v)) = C.longlong(v)
	case float32:
		out.tag = omniTagF64
		*(*C.double)(unsafe.Pointer(&out.v)) = C.double(v)
	case float64:
		out.tag = omniTagF64
		*(*C.double)(unsafe.Pointer(&out.v)) = C.double(v)
	case string:
		writeTypedBridgeText(out, omniTagString, v)
	default:
		return false
	}
	return true
}
