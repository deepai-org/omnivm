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
import json
import os
import threading
import uuid
import weakref

__all__ = [
    "init_runtimes",
    "call",
    "call_typed",
    "execute",
    "run_manifest",
    "load_manifest_module",
    "manifest_call",
    "ManifestProxy",
    "set_task_timeout",
    "host_thread_id",
    "watchdog_capabilities",
    "worker_tainted",
    "last_timeout_runtime",
    "worker_taint_reason",
    "status",
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
_manifest_arg_refs_lock = threading.Lock()

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

        if hasattr(lib, "OmniLoadManifestModule"):
            lib.OmniLoadManifestModule.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniLoadManifestModule.restype = ctypes.c_char_p

        if hasattr(lib, "OmniManifestCall"):
            lib.OmniManifestCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniManifestCall.restype = ctypes.c_char_p

        lib.OmniBufGet.argtypes = [
            ctypes.c_char_p,
            ctypes.POINTER(_OmniBuffer),
        ]
        lib.OmniBufGet.restype = ctypes.c_int

        lib.OmniBufSet.argtypes = [ctypes.c_char_p, _OmniBuffer]
        lib.OmniBufSet.restype = ctypes.c_int

        lib.OmniBufRelease.argtypes = [ctypes.c_char_p]
        lib.OmniBufRelease.restype = None

        if hasattr(lib, "OmniArrowGet"):
            lib.OmniArrowGet.argtypes = [
                ctypes.c_char_p,
                ctypes.POINTER(_ArrowSchema),
                ctypes.POINTER(_ArrowArray),
            ]
            lib.OmniArrowGet.restype = ctypes.c_int

        if hasattr(lib, "OmniArrowSet"):
            lib.OmniArrowSet.argtypes = [
                ctypes.c_char_p,
                ctypes.POINTER(_ArrowSchema),
                ctypes.POINTER(_ArrowArray),
            ]
            lib.OmniArrowSet.restype = ctypes.c_int

        if hasattr(lib, "OmniHandleRelease"):
            lib.OmniHandleRelease.argtypes = [ctypes.c_uint64]
            lib.OmniHandleRelease.restype = ctypes.c_int

        if hasattr(lib, "OmniHandleRetain"):
            lib.OmniHandleRetain.argtypes = [ctypes.c_uint64]
            lib.OmniHandleRetain.restype = ctypes.c_int

        if hasattr(lib, "OmniHandleEscape"):
            lib.OmniHandleEscape.argtypes = [ctypes.c_uint64]
            lib.OmniHandleEscape.restype = ctypes.c_int

        if hasattr(lib, "OmniHandleReleaseFromFinalizer"):
            lib.OmniHandleReleaseFromFinalizer.argtypes = [ctypes.c_uint64]
            lib.OmniHandleReleaseFromFinalizer.restype = ctypes.c_int

        if hasattr(lib, "OmniHandleAccess"):
            lib.OmniHandleAccess.argtypes = [
                ctypes.c_uint64,
                ctypes.c_char_p,
                ctypes.c_int64,
            ]
            lib.OmniHandleAccess.restype = ctypes.c_int

        if hasattr(lib, "OmniHandleRecordReference"):
            lib.OmniHandleRecordReference.argtypes = [
                ctypes.c_uint64,
                ctypes.c_uint64,
                ctypes.c_char_p,
            ]
            lib.OmniHandleRecordReference.restype = ctypes.c_int

        if hasattr(lib, "OmniHandleDropReference"):
            lib.OmniHandleDropReference.argtypes = [ctypes.c_uint64, ctypes.c_uint64]
            lib.OmniHandleDropReference.restype = None

        if hasattr(lib, "OmniDrainFinalizerReleases"):
            lib.OmniDrainFinalizerReleases.argtypes = [ctypes.c_int]
            lib.OmniDrainFinalizerReleases.restype = ctypes.c_int

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

        if hasattr(lib, "OmniWatchdogCapabilities"):
            lib.OmniWatchdogCapabilities.argtypes = []
            lib.OmniWatchdogCapabilities.restype = ctypes.c_char_p

        if hasattr(lib, "OmniWorkerTainted"):
            lib.OmniWorkerTainted.argtypes = []
            lib.OmniWorkerTainted.restype = ctypes.c_int

        if hasattr(lib, "OmniLastTimeoutRuntime"):
            lib.OmniLastTimeoutRuntime.argtypes = []
            lib.OmniLastTimeoutRuntime.restype = ctypes.c_char_p

        if hasattr(lib, "OmniWorkerTaintReason"):
            lib.OmniWorkerTaintReason.argtypes = []
            lib.OmniWorkerTaintReason.restype = ctypes.c_char_p

        if hasattr(lib, "OmniStatus"):
            lib.OmniStatus.argtypes = []
            lib.OmniStatus.restype = ctypes.c_char_p

        if hasattr(lib, "OmniClearWorkerTaintForTest"):
            lib.OmniClearWorkerTaintForTest.argtypes = []
            lib.OmniClearWorkerTaintForTest.restype = None

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
        ("owned", ctypes.c_int8),
        ("read_only", ctypes.c_int8),
    ]


class _ArrowSchema(ctypes.Structure):
    pass


class _ArrowArray(ctypes.Structure):
    pass


_ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(_ArrowSchema))
_ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(_ArrowArray))

_ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p),
    ("name", ctypes.c_char_p),
    ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("children", ctypes.POINTER(ctypes.POINTER(_ArrowSchema))),
    ("dictionary", ctypes.POINTER(_ArrowSchema)),
    ("release", _ArrowSchemaRelease),
    ("private_data", ctypes.c_void_p),
]

_ArrowArray._fields_ = [
    ("length", ctypes.c_int64),
    ("null_count", ctypes.c_int64),
    ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64),
    ("n_children", ctypes.c_int64),
    ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.POINTER(ctypes.POINTER(_ArrowArray))),
    ("dictionary", ctypes.POINTER(_ArrowArray)),
    ("release", _ArrowArrayRelease),
    ("private_data", ctypes.c_void_p),
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
        raise TypeError(
            "unsupported typed bridge argument; complex values must cross "
            "through the manifest proxy/Arrow boundary, not implicit stringification"
        )
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


def load_manifest_module(module_id, path):
    """
    Load an OmniVM manifest as a retained callable module.

    Top-level manifest operations execute once during load. Manifest
    ``func_def`` entries remain callable through manifest_call().
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized — call init_runtimes() first")
    if not hasattr(_lib, "OmniLoadManifestModule"):
        raise RuntimeError("libomnivm does not expose OmniLoadManifestModule")
    result = _lib.OmniLoadManifestModule(
        str(module_id).encode("utf-8"),
        os.fsencode(path),
    )
    return _check_result(result)


def manifest_call(module_id, func, args=()):
    """
    Call a retained manifest function and decode its return envelope.

    Primitive values cross by value. Complex Python objects are passed as
    host-runtime references so framework objects, request objects, and other
    live values do not fall back to JSON serialization.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized — call init_runtimes() first")
    if not hasattr(_lib, "OmniManifestCall"):
        raise RuntimeError("libomnivm does not expose OmniManifestCall")

    retained_keys = []
    try:
        payload = {
            "func": str(func),
            "args": [_manifest_arg(arg, retained_keys) for arg in args],
        }
        return _manifest_bridge_call(module_id, payload)
    finally:
        _release_manifest_args(retained_keys)


def _manifest_bridge_call(module_id, payload):
    result = _lib.OmniManifestCall(
        str(module_id).encode("utf-8"),
        json.dumps(payload, separators=(",", ":")).encode("utf-8"),
    )
    return _decode_manifest_result(_check_result(result), module_id=module_id)


def _manifest_arg(value, retained_keys):
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    key = _retain_manifest_arg(value)
    retained_keys.append(key)
    return {
        "__omnivm_runtime_ref__": True,
        "runtime": "python",
        "var": f"__omnivm_arg_refs[{key!r}]",
        "callable": callable(value),
    }


def _retain_manifest_arg(value):
    key = f"py_{uuid.uuid4().hex}"
    with _manifest_arg_refs_lock:
        refs = getattr(_builtins, "__omnivm_arg_refs", None)
        if refs is None:
            refs = {}
            setattr(_builtins, "__omnivm_arg_refs", refs)
        refs[key] = value
    execute("python", "import builtins\n__omnivm_arg_refs = builtins.__omnivm_arg_refs")
    return key


def _release_manifest_args(keys):
    if not keys:
        return
    with _manifest_arg_refs_lock:
        refs = getattr(_builtins, "__omnivm_arg_refs", None)
        if refs is None:
            return
        for key in keys:
            refs.pop(key, None)


def _decode_manifest_result(result, module_id=None):
    if result == "":
        return None
    try:
        envelope = json.loads(result)
    except json.JSONDecodeError:
        return result
    if not isinstance(envelope, dict) or not envelope.get("__omnivm_result__"):
        return _wrap_manifest_value(module_id, envelope)
    if envelope.get("kind") == "null":
        return None
    return _wrap_manifest_value(module_id, envelope.get("value"))


def _wrap_manifest_value(module_id, value):
    if isinstance(value, dict):
        if _is_manifest_proxy_descriptor(value) and module_id is not None:
            return ManifestProxy(module_id, value)
        return {key: _wrap_manifest_value(module_id, item) for key, item in value.items()}
    if isinstance(value, list):
        return [_wrap_manifest_value(module_id, item) for item in value]
    return value


def _is_manifest_proxy_descriptor(value):
    return (
        value.get("__omnivm_resource__") is True
        or value.get("__omnivm_table__") is True
        or value.get("__omnivm_stream__") is True
        or value.get("__omnivm_channel__") is True
        or value.get("__omnivm_job__") is True
    ) and value.get("id") is not None


def _manifest_proxy_release(module_id, handle_id):
    try:
        if _lib is None or not hasattr(_lib, "OmniManifestCall"):
            return
        _manifest_bridge_call(module_id, {"op": "handle_release_finalizer", "id": handle_id})
    except Exception:
        pass


class ManifestProxy:
    """Python wrapper for a live object returned by a retained manifest."""

    def __init__(self, module_id, descriptor):
        object.__setattr__(self, "_module_id", str(module_id))
        object.__setattr__(self, "_descriptor", dict(descriptor))
        object.__setattr__(self, "_handle_id", int(descriptor["id"]))
        if descriptor.get("transfer") is True:
            _manifest_bridge_call(module_id, {"op": "handle_adopt", "id": self._handle_id})
        else:
            _manifest_bridge_call(module_id, {"op": "handle_retain", "id": self._handle_id})
        object.__setattr__(
            self,
            "_finalizer",
            weakref.finalize(self, _manifest_proxy_release, self._module_id, self._handle_id),
        )

    @property
    def __omnivm_descriptor__(self):
        return dict(self._descriptor)

    @property
    def __omnivm_handle_id__(self):
        return self._handle_id

    def close(self):
        finalizer = object.__getattribute__(self, "_finalizer")
        if finalizer.alive:
            finalizer()

    def __enter__(self):
        return self

    def __exit__(self, _exc_type, _exc, _tb):
        self.close()
        return False

    def __repr__(self):
        descriptor = object.__getattribute__(self, "_descriptor")
        runtime = descriptor.get("runtime", "unknown")
        kind = descriptor.get("kind", "object")
        return f"<omnivm.ManifestProxy {runtime}:{kind}#{self._handle_id}>"

    def __getattr__(self, key):
        result = self._op({"op": "handle_get", "id": self._handle_id, "key": key})
        if isinstance(result, dict) and result.get("__omnivm_callable__") is True:
            return _ManifestProxyMethod(self, key)
        return result

    def __getitem__(self, key):
        return self._op({"op": "handle_index", "id": self._handle_id, "value": key})

    def __setitem__(self, key, value):
        retained_keys = []
        try:
            self._op({"op": "handle_set", "id": self._handle_id, "key": str(key), "value": self._arg(value, retained_keys)})
        finally:
            _release_manifest_args(retained_keys)

    def __setattr__(self, key, value):
        if key.startswith("_"):
            object.__setattr__(self, key, value)
            return
        retained_keys = []
        try:
            self._op({"op": "handle_set", "id": self._handle_id, "key": key, "value": self._arg(value, retained_keys)})
        finally:
            _release_manifest_args(retained_keys)

    def __len__(self):
        return int(self._op({"op": "handle_len", "id": self._handle_id}))

    def __iter__(self):
        if self._descriptor.get("__omnivm_stream__") is True or self._descriptor.get("__omnivm_channel__") is True:
            return _ManifestStreamIterator(self)
        mode = "values" if self._descriptor.get("kind") == "sequence" or self._descriptor.get("__omnivm_table__") is True else "keys"
        return iter(self._op({"op": "handle_iter", "id": self._handle_id, "mode": mode}))

    def __contains__(self, value):
        retained_keys = []
        try:
            return bool(self._op({"op": "handle_contains", "id": self._handle_id, "value": self._arg(value, retained_keys)}))
        finally:
            _release_manifest_args(retained_keys)

    def _method_call(self, key, args, kwargs=None):
        retained_keys = []
        try:
            payload = {
                "op": "handle_call",
                "id": self._handle_id,
                "key": key,
                "args": [self._arg(arg, retained_keys) for arg in args],
            }
            if kwargs:
                payload["kwargs"] = {
                    str(name): self._arg(value, retained_keys)
                    for name, value in kwargs.items()
                }
            return self._op(payload)
        finally:
            _release_manifest_args(retained_keys)

    def _op(self, payload):
        return _manifest_bridge_call(self._module_id, payload)

    def _arg(self, value, retained_keys=None):
        if isinstance(value, ManifestProxy):
            return value.__omnivm_descriptor__
        retained = retained_keys if retained_keys is not None else []
        return _manifest_arg(value, retained)


class _ManifestProxyMethod:
    def __init__(self, proxy, key):
        self._proxy = proxy
        self._key = key

    def __call__(self, *args, **kwargs):
        return self._proxy._method_call(self._key, args, kwargs)

    def __repr__(self):
        return f"<omnivm.ManifestProxyMethod {self._key}>"


class _ManifestStreamIterator:
    def __init__(self, proxy):
        self._proxy = proxy

    def __iter__(self):
        return self

    def __next__(self):
        item = self._proxy._op({"op": "stream_next", "id": self._proxy.__omnivm_handle_id__})
        if item.get("done") is True:
            raise StopIteration
        return item.get("value")


def set_task_timeout(ms):
    """
    Set the direct libomnivm call watchdog timeout in milliseconds.

    A value of 0 disables direct-call watchdog arming. In c-shared mode the
    watchdog can preempt JavaScript and Ruby calls, interrupts Java calls that
    honor Thread.interrupt(), and applies a deadline to Go plugin calls. Host
    CPython interruption is handled by CPython-native mechanisms; inspect
    watchdog_capabilities() before relying on timeouts for a runtime.
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


def watchdog_capabilities():
    """
    Return the direct-call watchdog support matrix for this libomnivm build.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniWatchdogCapabilities"):
        return {
            "python": "host-interrupt",
            "javascript": "watchdog",
            "ruby": "watchdog",
            "java": "interrupt",
            "go": "deadline",
        }
    text = _check_result(_lib.OmniWatchdogCapabilities())
    caps = {}
    for item in text.split(","):
        if not item:
            continue
        name, _, value = item.partition("=")
        if name:
            caps[name] = value
    return caps


def worker_tainted():
    """
    Return True when this worker should be recycled.

    Today this is set after a Go plugin deadline, because arbitrary in-process
    Go plugin code cannot be force-preempted after the host call returns.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniWorkerTainted"):
        raise RuntimeError("libomnivm does not expose OmniWorkerTainted")
    return bool(_lib.OmniWorkerTainted())


def last_timeout_runtime():
    """Return the runtime responsible for the last non-recoverable timeout."""
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniLastTimeoutRuntime"):
        raise RuntimeError("libomnivm does not expose OmniLastTimeoutRuntime")
    return _check_result(_lib.OmniLastTimeoutRuntime())


def worker_taint_reason():
    """Return the reason this worker was marked for recycling."""
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniWorkerTaintReason"):
        raise RuntimeError("libomnivm does not expose OmniWorkerTaintReason")
    return _check_result(_lib.OmniWorkerTaintReason())


def status():
    """
    Return libomnivm worker status as a dict.

    This is intentionally observational: normal application code can keep using
    omnivm.call(), while server glue or health checks can decide whether to
    recycle a tainted worker.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniStatus"):
        raise RuntimeError("libomnivm does not expose OmniStatus")
    return json.loads(_check_result(_lib.OmniStatus()))


def _clear_worker_taint_for_test():
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniClearWorkerTaintForTest"):
        raise RuntimeError("libomnivm does not expose OmniClearWorkerTaintForTest")
    _lib.OmniClearWorkerTaintForTest()


def get_buffer(name):
    """
    Return a shared OmniVM buffer as a Python memoryview, or None if missing.

    The returned memoryview is a borrowed view over OmniVM-managed memory. Its
    finalizer releases the underlying borrow when the view is garbage collected.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    out = _OmniBuffer()
    encoded_name = str(name).encode("utf-8")
    rc = _lib.OmniBufGet(encoded_name, ctypes.byref(out))
    if rc != 0:
        return None
    if not out.data or out.len <= 0:
        _lib.OmniBufRelease(encoded_name)
        return None
    view_owner = (ctypes.c_char * int(out.len)).from_address(int(out.data))
    weakref.finalize(view_owner, _release_buffer_borrow, encoded_name)
    view = memoryview(view_owner).cast("B")
    if out.read_only:
        view = view.toreadonly()
    return view


def _release_buffer_borrow(encoded_name):
    if _lib is not None:
        _lib.OmniBufRelease(encoded_name)


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
        0,
        int(view.readonly),
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


def _release_handle(handle_id):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHandleRelease"):
        raise RuntimeError("libomnivm does not expose OmniHandleRelease")
    return _lib.OmniHandleRelease(int(handle_id)) == 0


def _retain_handle(handle_id):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHandleRetain"):
        raise RuntimeError("libomnivm does not expose OmniHandleRetain")
    return _lib.OmniHandleRetain(int(handle_id)) == 0


def _escape_handle(handle_id):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHandleEscape"):
        raise RuntimeError("libomnivm does not expose OmniHandleEscape")
    return _lib.OmniHandleEscape(int(handle_id)) == 0


def _release_handle_from_finalizer(handle_id):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHandleReleaseFromFinalizer"):
        raise RuntimeError("libomnivm does not expose OmniHandleReleaseFromFinalizer")
    return _lib.OmniHandleReleaseFromFinalizer(int(handle_id)) == 0


def _record_handle_access(handle_id, kind="access", chatty_threshold=0):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHandleAccess"):
        raise RuntimeError("libomnivm does not expose OmniHandleAccess")
    rc = _lib.OmniHandleAccess(
        int(handle_id),
        str(kind).encode("utf-8"),
        int(chatty_threshold),
    )
    if rc < 0:
        raise RuntimeError("omnivm handle access failed")
    return rc == 1


def _record_handle_reference(from_id, to_id, kind="reference"):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHandleRecordReference"):
        raise RuntimeError("libomnivm does not expose OmniHandleRecordReference")
    return (
        _lib.OmniHandleRecordReference(
            int(from_id),
            int(to_id),
            str(kind).encode("utf-8"),
        )
        == 0
    )


def _drop_handle_reference(from_id, to_id):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniHandleDropReference"):
        raise RuntimeError("libomnivm does not expose OmniHandleDropReference")
    _lib.OmniHandleDropReference(int(from_id), int(to_id))


def _drain_finalizer_releases(max_releases=0):
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniDrainFinalizerReleases"):
        raise RuntimeError("libomnivm does not expose OmniDrainFinalizerReleases")
    return _lib.OmniDrainFinalizerReleases(int(max_releases)) == 0


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
