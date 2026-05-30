"""
OmniVM — call Go, JavaScript, Java, and Ruby from Python with zero IPC overhead.

Usage:
    import omnivm

    # Initialize in Gunicorn post_fork hook (loads Go runtime fresh per worker)
    omnivm.init_runtimes(["go", "javascript", "java", "ruby"])

    # Call Go — GIL released during execution, other threads aren't blocked
    result = omnivm.call("go", "6 * 7")  # "42"

    # Load pre-compiled Go plugin
    omnivm.load_plugin("go", "/app/plugins/sessvalidator.so")
    user_id = omnivm.call("go", 'sessvalidator.ValidateSession("key")')

    # Call JavaScript (full Node.js — require() and npm packages work)
    html = omnivm.call("javascript", "JSON.stringify({ok: true})")

    # Call Java (in-memory compilation via javax.tools.JavaCompiler)
    result = omnivm.call("java", "Math.pow(2, 10)")

    # Call Ruby
    result = omnivm.call("ruby", "[1,2,3].map{|x| x*x}.to_s")
"""

import ctypes
import ctypes.util
import os
import threading

__all__ = [
    "init_runtimes",
    "call",
    "call_typed",
    "execute",
    "run_manifest",
    "set_task_timeout",
    "host_thread_id",
    "get_buffer",
    "set_buffer",
    "release_buffer",
    "load_plugin",
    "shutdown",
    "RuntimeError",
]


import builtins as _builtins


class RuntimeError(_builtins.RuntimeError):
    """Raised when a runtime call fails (Go panic, JS exception, etc.)."""

    def __init__(self, message, runtime=None):
        super().__init__(message)
        self.runtime = runtime


# Lazy-loaded shared library handle. Not loaded until init_runtimes() is called.
# This is critical for prefork servers: the master process must not load the Go
# runtime before fork(). Each worker loads it independently post-fork.
_lib = None
_lock = threading.Lock()

# Thread-local metrics
_local = threading.local()


def _find_libomnivm():
    """Find libomnivm.so in standard locations."""
    # 1. Explicit environment variable
    path = os.environ.get("OMNIVM_LIB")
    if path and os.path.isfile(path):
        return path

    # 2. Next to this package
    pkg_dir = os.path.dirname(os.path.abspath(__file__))
    for candidate in ("libomnivm.so", "libomnivm.dylib"):
        path = os.path.join(pkg_dir, candidate)
        if os.path.isfile(path):
            return path

    # 3. Standard library search
    path = ctypes.util.find_library("omnivm")
    if path:
        return path

    # 4. Common install locations
    for d in ("/usr/local/lib", "/usr/lib"):
        for name in ("libomnivm.so", "libomnivm.dylib"):
            path = os.path.join(d, name)
            if os.path.isfile(path):
                return path

    return None


def _load_lib():
    """Load libomnivm.so lazily. Called once from init_runtimes()."""
    global _lib
    if _lib is not None:
        return _lib

    with _lock:
        if _lib is not None:
            return _lib

        path = _find_libomnivm()
        if path is None:
            raise FileNotFoundError(
                "libomnivm.so not found. Set OMNIVM_LIB=/path/to/libomnivm.so "
                "or install with: pip install omnivm[runtimes]"
            )

        lib = ctypes.CDLL(path)

        # Set up function signatures
        lib.OmniInit.argtypes = [ctypes.c_char_p]
        lib.OmniInit.restype = ctypes.c_char_p

        lib.OmniCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
        lib.OmniCall.restype = ctypes.c_char_p

        if hasattr(lib, "OmniCallHost"):
            lib.OmniCallHost.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniCallHost.restype = ctypes.c_char_p

        lib.OmniExec.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
        lib.OmniExec.restype = ctypes.c_char_p

        if hasattr(lib, "OmniExecHost"):
            lib.OmniExecHost.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniExecHost.restype = ctypes.c_char_p

        lib.OmniRunManifestFile.argtypes = [ctypes.c_char_p]
        lib.OmniRunManifestFile.restype = ctypes.c_char_p

        lib.OmniBufGet.argtypes = [
            ctypes.c_char_p,
            ctypes.POINTER(_OmniBuffer),
        ]
        lib.OmniBufGet.restype = ctypes.c_int

        lib.OmniBufSet.argtypes = [ctypes.c_char_p, _OmniBuffer]
        lib.OmniBufSet.restype = ctypes.c_int

        lib.OmniBufRelease.argtypes = [ctypes.c_char_p]
        lib.OmniBufRelease.restype = None

        lib.OmniLoadPlugin.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
        lib.OmniLoadPlugin.restype = ctypes.c_char_p

        lib.OmniShutdown.argtypes = []
        lib.OmniShutdown.restype = None

        if hasattr(lib, "OmniSetTaskTimeout"):
            lib.OmniSetTaskTimeout.argtypes = [ctypes.c_int]
            lib.OmniSetTaskTimeout.restype = None

        if hasattr(lib, "OmniHostThreadID"):
            lib.OmniHostThreadID.argtypes = []
            lib.OmniHostThreadID.restype = ctypes.c_long

        lib.OmniFree.argtypes = [ctypes.c_char_p]
        lib.OmniFree.restype = None

        # Typed call bridge
        lib.OmniCallTyped.argtypes = [
            ctypes.c_char_p,  # runtime
            ctypes.c_char_p,  # func_name
            ctypes.POINTER(_OmniValue),  # args
            ctypes.c_int32,   # nargs
        ]
        lib.OmniCallTyped.restype = _OmniValue

        _lib = lib
        return _lib


# omni_value_t tag constants
_TAG_NULL = 0
_TAG_BOOL = 1
_TAG_I64 = 2
_TAG_F64 = 3
_TAG_STRING = 4
_TAG_BYTES = 5
_TAG_REF = 6
_TAG_ERROR = 7


class _OmniValueUnionStr(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_char_p), ("len", ctypes.c_int64)]


class _OmniValueUnion(ctypes.Union):
    _fields_ = [
        ("i", ctypes.c_int64),
        ("f", ctypes.c_double),
        ("s", _OmniValueUnionStr),
        ("ref", ctypes.c_uint64),
    ]


class _OmniValue(ctypes.Structure):
    _fields_ = [("tag", ctypes.c_int64), ("v", _OmniValueUnion)]


class _OmniBuffer(ctypes.Structure):
    _fields_ = [
        ("data", ctypes.c_void_p),
        ("len", ctypes.c_int64),
        ("dtype", ctypes.c_int32),
    ]


def _py_to_omni_value(val):
    """Convert a Python value to an _OmniValue."""
    ov = _OmniValue()
    if val is None:
        ov.tag = _TAG_NULL
    elif isinstance(val, bool):
        ov.tag = _TAG_BOOL
        ov.v.i = 1 if val else 0
    elif isinstance(val, int):
        ov.tag = _TAG_I64
        ov.v.i = val
    elif isinstance(val, float):
        ov.tag = _TAG_F64
        ov.v.f = val
    elif isinstance(val, str):
        ov.tag = _TAG_STRING
        encoded = val.encode("utf-8")
        ov.v.s.ptr = encoded
        ov.v.s.len = len(encoded)
    elif isinstance(val, (bytes, bytearray)):
        ov.tag = _TAG_BYTES
        ov.v.s.ptr = bytes(val)
        ov.v.s.len = len(val)
    else:
        # Fallback: stringify
        s = str(val).encode("utf-8")
        ov.tag = _TAG_STRING
        ov.v.s.ptr = s
        ov.v.s.len = len(s)
    return ov


def _omni_value_to_py(ov):
    """Convert an _OmniValue to a Python value."""
    if ov.tag == _TAG_NULL:
        return None
    elif ov.tag == _TAG_BOOL:
        return bool(ov.v.i)
    elif ov.tag == _TAG_I64:
        return ov.v.i
    elif ov.tag == _TAG_F64:
        return ov.v.f
    elif ov.tag == _TAG_STRING:
        if ov.v.s.ptr and ov.v.s.len > 0:
            return ov.v.s.ptr[:ov.v.s.len].decode("utf-8")
        return ""
    elif ov.tag == _TAG_BYTES:
        if ov.v.s.ptr and ov.v.s.len > 0:
            return ov.v.s.ptr[:ov.v.s.len]
        return b""
    elif ov.tag == _TAG_ERROR:
        msg = ""
        if ov.v.s.ptr and ov.v.s.len > 0:
            msg = ov.v.s.ptr[:ov.v.s.len].decode("utf-8")
        raise RuntimeError(msg)
    return None


def _check_result(result, runtime=None):
    """Check a C string result for ERR: prefix and raise if needed."""
    if result is None:
        raise RuntimeError("call returned NULL", runtime=runtime)
    text = result.decode("utf-8") if isinstance(result, bytes) else result
    if text.startswith("OK:"):
        return text[3:]
    if text.startswith("ERR:"):
        raise RuntimeError(text[4:], runtime=runtime)
    return text


def init_runtimes(runtimes):
    """
    Initialize OmniVM runtimes. Call this in Gunicorn's post_fork hook.

    This is when libomnivm.so is loaded (via dlopen) and the Go runtime starts.
    Must be called AFTER fork in prefork servers. Safe to call from any thread.

    Args:
        runtimes: List of runtime names, e.g. ["go", "javascript"]

    Raises:
        RuntimeError: If initialization fails.
        FileNotFoundError: If libomnivm.so is not found.
    """
    lib = _load_lib()
    runtime_list = ",".join(runtimes).encode("utf-8")
    result = lib.OmniInit(runtime_list)
    _check_result(result)


def call(runtime, code):
    """
    Evaluate an expression in a runtime and return the result as a string.

    The GIL is released during execution — other Python threads continue running.
    This is the primary API for calling Go/JavaScript from Python.

    Args:
        runtime: Runtime name ("go" or "javascript")
        code: Expression to evaluate

    Returns:
        String result of the expression.

    Raises:
        RuntimeError: On evaluation error (Go panic, JS exception, etc.)
    """
    if _lib is None:
        raise RuntimeError(
            "omnivm not initialized — call init_runtimes() first",
            runtime=runtime,
        )
    import time

    start = time.monotonic_ns()
    call_fn = getattr(_lib, "OmniCallHost", _lib.OmniCall)
    result = call_fn(
        runtime.encode("utf-8"),
        code.encode("utf-8"),
    )
    elapsed = time.monotonic_ns() - start

    # Thread-local metrics
    _local.last_call_duration_ns = elapsed
    if not hasattr(_local, "total_call_duration_ns"):
        _local.total_call_duration_ns = 0
    _local.total_call_duration_ns += elapsed

    return _check_result(result, runtime=runtime)


def call_typed(runtime, func_name, args=()):
    """
    Call a function in a runtime with typed arguments, returning a typed result.

    Unlike call(), this preserves native types (int, float, bool, str, bytes)
    through the bridge without string serialization.

    Args:
        runtime: Runtime name ("go", "javascript", "python", "ruby", "java")
        func_name: Function name to call (e.g., "Math.sqrt", "math.abs")
        args: Tuple or list of typed arguments

    Returns:
        Typed result (int, float, bool, str, bytes, or None).

    Raises:
        RuntimeError: On evaluation error.
    """
    if _lib is None:
        raise RuntimeError(
            "omnivm not initialized — call init_runtimes() first",
            runtime=runtime,
        )

    # Convert args to omni_value_t array
    n = len(args)
    c_args = (_OmniValue * n)() if n > 0 else None
    # Keep references to encoded strings alive
    _refs = []
    for i, arg in enumerate(args):
        c_args[i] = _py_to_omni_value(arg)
        # Keep encoded bytes alive so ctypes doesn't GC them
        if isinstance(arg, str):
            _refs.append(arg.encode("utf-8"))
        elif isinstance(arg, (bytes, bytearray)):
            _refs.append(bytes(arg))

    result = _lib.OmniCallTyped(
        runtime.encode("utf-8"),
        func_name.encode("utf-8"),
        c_args,
        ctypes.c_int32(n),
    )

    return _omni_value_to_py(result)


def execute(runtime, code):
    """
    Execute code in a runtime (for side effects). Returns captured stdout.

    Args:
        runtime: Runtime name ("go" or "javascript")
        code: Code to execute

    Returns:
        Captured stdout output as a string.

    Raises:
        RuntimeError: On execution error.
    """
    if _lib is None:
        raise RuntimeError(
            "omnivm not initialized — call init_runtimes() first",
            runtime=runtime,
        )
    exec_fn = getattr(_lib, "OmniExecHost", _lib.OmniExec)
    result = exec_fn(
        runtime.encode("utf-8"),
        code.encode("utf-8"),
    )
    return _check_result(result, runtime=runtime)


def run_manifest(path):
    """
    Run an OmniVM dispatch manifest in this process through libomnivm.so.

    Call init_runtimes(["javascript", "java", "ruby"]) first for manifests
    that may use the full example-suite surface. Python is always the host
    runtime; Go manifest functions use the manifest executor's Go registry.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized — call init_runtimes() first")
    result = _lib.OmniRunManifestFile(os.fsencode(path))
    return _check_result(result)


def set_task_timeout(ms):
    """
    Set the direct libomnivm call watchdog timeout in milliseconds.

    A value of 0 disables direct-call watchdog arming. This controls calls made
    through the c-shared host API; guest bridge calls inherit the same setting.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniSetTaskTimeout"):
        raise RuntimeError("libomnivm does not expose OmniSetTaskTimeout")
    _lib.OmniSetTaskTimeout(int(ms))


def host_thread_id():
    """Return the OS thread id that libomnivm pinned as the host thread."""
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHostThreadID"):
        raise RuntimeError("libomnivm does not expose OmniHostThreadID")
    return int(_lib.OmniHostThreadID())


def get_buffer(name):
    """
    Return a shared OmniVM buffer as a Python memoryview, or None if missing.

    The current libomnivm bridge copies the buffer into Python-owned memory for
    host safety. Guest runtimes can still exchange the named buffer through the
    shared OmniVM buffer store.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    out = _OmniBuffer()
    rc = _lib.OmniBufGet(str(name).encode("utf-8"), ctypes.byref(out))
    if rc != 0 or not out.data or out.len <= 0:
        return None
    return memoryview(ctypes.string_at(out.data, out.len))


def set_buffer(name, data, dtype=0):
    """
    Store bytes-like data in the shared OmniVM buffer store.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    view = memoryview(data).cast("B")
    backing = ctypes.create_string_buffer(view.tobytes())
    buf = _OmniBuffer(
        ctypes.cast(backing, ctypes.c_void_p),
        len(view),
        int(dtype),
    )
    rc = _lib.OmniBufSet(str(name).encode("utf-8"), buf)
    if rc != 0:
        raise RuntimeError("omnivm.set_buffer failed")


def release_buffer(name):
    """
    Release a named shared OmniVM buffer.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    _lib.OmniBufRelease(str(name).encode("utf-8"))


def load_plugin(runtime, path):
    """
    Load a pre-compiled Go plugin (.so file).

    The plugin must be built with the same Go version as libomnivm.so.
    Use omnivm-build-plugin to compile plugins.

    Args:
        runtime: Must be "go"
        path: Path to the .so file

    Raises:
        RuntimeError: On load error (version mismatch, missing symbols, etc.)
    """
    if _lib is None:
        raise RuntimeError(
            "omnivm not initialized — call init_runtimes() first",
            runtime=runtime,
        )
    result = _lib.OmniLoadPlugin(
        runtime.encode("utf-8"),
        path.encode("utf-8"),
    )
    _check_result(result, runtime=runtime)


def shutdown():
    """
    Shut down all runtimes. Optional — process exit cleans up automatically.
    """
    global _lib
    if _lib is not None:
        _lib.OmniShutdown()


def last_call_duration_ns():
    """Return the duration of the last omnivm.call() in nanoseconds (thread-local)."""
    return getattr(_local, "last_call_duration_ns", 0)


def thread_local_total_ns():
    """Return total omnivm.call() time on this thread in nanoseconds."""
    return getattr(_local, "total_call_duration_ns", 0)


def thread_local_reset():
    """Reset thread-local metrics (call at start of each HTTP request)."""
    _local.last_call_duration_ns = 0
    _local.total_call_duration_ns = 0


def thread_local_total_ms():
    """Return total omnivm.call() time on this thread in milliseconds."""
    return getattr(_local, "total_call_duration_ns", 0) / 1_000_000
