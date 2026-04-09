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
    "execute",
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

        lib.OmniExec.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
        lib.OmniExec.restype = ctypes.c_char_p

        lib.OmniLoadPlugin.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
        lib.OmniLoadPlugin.restype = ctypes.c_char_p

        lib.OmniShutdown.argtypes = []
        lib.OmniShutdown.restype = None

        lib.OmniFree.argtypes = [ctypes.c_char_p]
        lib.OmniFree.restype = None

        _lib = lib
        return _lib


def _check_result(result, runtime=None):
    """Check a C string result for ERR: prefix and raise if needed."""
    if result is None:
        raise RuntimeError("call returned NULL", runtime=runtime)
    text = result.decode("utf-8") if isinstance(result, bytes) else result
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
    result = _lib.OmniCall(
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
    result = _lib.OmniExec(
        runtime.encode("utf-8"),
        code.encode("utf-8"),
    )
    return _check_result(result, runtime=runtime)


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
