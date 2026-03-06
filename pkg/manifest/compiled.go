package manifest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

/*
#include <dlfcn.h>
#include <stdlib.h>

// loadLib opens a shared library and returns its handle.
static void* omnivm_dlopen(const char* path) {
	return dlopen(path, RTLD_NOW);
}

// findSym looks up a symbol in a loaded library.
static void* omnivm_dlsym(void* handle, const char* name) {
	return dlsym(handle, name);
}

// dlError returns the last dlopen/dlsym error message.
static const char* omnivm_dlerror() {
	return dlerror();
}

// closeLib closes a shared library handle.
static void omnivm_dlclose(void* handle) {
	if (handle) dlclose(handle);
}

// callNoArgs calls a function pointer that takes no args and returns an int.
static int omnivm_call_int_noargs(void* fn) {
	return ((int(*)())fn)();
}

// callStringArg calls a function pointer that takes a const char* and returns a char*.
static char* omnivm_call_str(void* fn, const char* arg) {
	return ((char*(*)(const char*))fn)(arg);
}

#cgo LDFLAGS: -ldl
*/
import "C"
import (
	"unsafe"
)

const compiledCacheDir = "/tmp/omnivm-compiled"

// opExecCompiled compiles C or Rust source to a shared library, loads it,
// and calls its main() or run() function.
func (e *Executor) opExecCompiled(op *Op) (interface{}, error) {
	soPath, err := e.compileNative(op.Code, op.Lang)
	if err != nil {
		return nil, err
	}

	result, err := e.callCompiledFunc(soPath, "run", "")
	if err != nil {
		return nil, err
	}

	if op.Bind != "" {
		e.setBinding(op.Bind, result)
	}
	return result, nil
}

// opEvalCompiled compiles C or Rust source, loads it, calls an eval(code) function
// that processes the expression, and returns the result.
func (e *Executor) opEvalCompiled(op *Op) (interface{}, error) {
	soPath, err := e.compileNative(op.Code, op.Lang)
	if err != nil {
		return nil, err
	}

	result, err := e.callCompiledFunc(soPath, "eval", op.Code)
	if err != nil {
		return nil, err
	}

	if op.Bind != "" {
		e.setBinding(op.Bind, result)
	}
	return result, nil
}

// compileNative compiles C or Rust source to a cached shared library.
func (e *Executor) compileNative(source, lang string) (string, error) {
	hash := sha256Hash(source + "|" + lang)
	soPath := filepath.Join(compiledCacheDir, hash+".so")

	if _, err := os.Stat(soPath); err == nil {
		return soPath, nil
	}

	if err := os.MkdirAll(compiledCacheDir, 0o755); err != nil {
		return "", err
	}

	tmpDir, err := os.MkdirTemp("", "omnivm-compiled-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	switch strings.ToLower(lang) {
	case "c":
		srcPath := filepath.Join(tmpDir, "code.c")
		if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
			return "", err
		}
		cmd := exec.Command("gcc", "-shared", "-fPIC", "-o", soPath, srcPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("gcc compile: %s: %w", string(out), err)
		}

	case "rust":
		srcPath := filepath.Join(tmpDir, "code.rs")
		if err := os.WriteFile(srcPath, []byte(source), 0o644); err != nil {
			return "", err
		}
		cmd := exec.Command("rustc", "--crate-type", "cdylib", "-o", soPath, srcPath)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("rustc compile: %s: %w", string(out), err)
		}

	default:
		return "", fmt.Errorf("unsupported compiled language: %q", lang)
	}

	return soPath, nil
}

// callCompiledFunc loads a .so, finds a function, and calls it.
func (e *Executor) callCompiledFunc(soPath, funcName, arg string) (string, error) {
	cPath := C.CString(soPath)
	defer C.free(unsafe.Pointer(cPath))

	handle := C.omnivm_dlopen(cPath)
	if handle == nil {
		errMsg := C.GoString(C.omnivm_dlerror())
		return "", fmt.Errorf("dlopen %q: %s", soPath, errMsg)
	}
	defer C.omnivm_dlclose(handle)

	cFuncName := C.CString(funcName)
	defer C.free(unsafe.Pointer(cFuncName))

	sym := C.omnivm_dlsym(handle, cFuncName)
	if sym == nil {
		errMsg := C.GoString(C.omnivm_dlerror())
		return "", fmt.Errorf("dlsym %q: %s", funcName, errMsg)
	}

	if arg == "" {
		// Call as int (*)() — treat return as exit code
		ret := int(C.omnivm_call_int_noargs(sym))
		return fmt.Sprintf("%d", ret), nil
	}

	// Call as char* (*)(const char*) — treat return as string result
	cArg := C.CString(arg)
	defer C.free(unsafe.Pointer(cArg))

	cResult := C.omnivm_call_str(sym, cArg)
	if cResult == nil {
		return "", nil
	}
	result := C.GoString(cResult)
	return result, nil
}
