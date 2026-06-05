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

import atexit
import ctypes
import ctypes.util
import inspect
import json
import os
import re
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
    "drain_worker",
    "drain_worker_hook",
    "install_worker_drain_hook",
    "drain_finalizer_releases",
    "lifecycle_scope",
    "unload_manifest_modules",
    "manifest_call",
    "ManifestProxy",
    "proxy_get",
    "proxy_set",
    "proxy_call",
    "proxy_len",
    "proxy_iter",
    "proxy_keys",
    "proxy_values",
    "proxy_items",
    "proxy_contains",
    "proxy_close",
    "aproxy_close",
    "omnivm_close",
    "omnivm_aclose",
    "cleanup_errors",
    "set_task_timeout",
    "host_thread_id",
    "affinity_status",
    "owner_dispatch_status",
    "owner_dispatch_target_status",
    "assert_owner_dispatch_supported",
    "assert_owner_dispatch_target_supported",
    "ruby_threading_status",
    "assert_ruby_native_threads_supported",
    "assert_host_thread",
    "watchdog_capabilities",
    "worker_tainted",
    "last_timeout_runtime",
    "worker_taint_reason",
    "status",
    "get_buffer",
    "set_buffer",
    "release_buffer",
    "buffer_owner",
    "BufferOwner",
    "buffer_status",
    "load_plugin",
    "shutdown",
    "RuntimeError",
]


import builtins as _builtins


class RuntimeError(_builtins.RuntimeError):
    """Raised when a runtime call fails (Go panic, JS exception, etc.)."""

    def __init__(self, message, runtime=None, boundary_path=None, details=None):
        super().__init__(message)
        parsed = _parse_runtime_error_text(
            str(message),
            runtime=runtime,
            boundary_path=boundary_path,
        )
        self.runtime = parsed["runtime"]
        self.origin_runtime = parsed["origin_runtime"]
        self.type = parsed["type"]
        self.message = parsed["message"]
        self.traceback = parsed["traceback"]
        self._stack_frames = _copy_json_value(parsed["stack_frames"])
        self._cause_chain = _copy_json_value(parsed["cause_chain"])
        self.boundary_path = parsed["boundary_path"]
        self.original_error_handle = parsed["original_error_handle"]
        self._details = _copy_json_value(details) if details is not None else _copy_json_value(parsed["details"])
        self._details_json = _runtime_error_details_json(self._details) if details is not None else parsed.get("details_json")
        if self._details_json is None and self._details is not None:
            self._details_json = _runtime_error_details_json(self._details)

    @property
    def stack_frames(self):
        return _copy_json_value(self._stack_frames)

    @stack_frames.setter
    def stack_frames(self, value):
        self._stack_frames = _copy_json_value(value)

    @property
    def stackFrames(self):
        return self.stack_frames

    @stackFrames.setter
    def stackFrames(self, value):
        self.stack_frames = value

    @property
    def cause_chain(self):
        return _copy_json_value(self._cause_chain)

    @cause_chain.setter
    def cause_chain(self, value):
        self._cause_chain = _copy_json_value(value)

    @property
    def causeChain(self):
        return self.cause_chain

    @causeChain.setter
    def causeChain(self, value):
        self.cause_chain = value

    @property
    def details(self):
        return _copy_json_value(self._details)

    @details.setter
    def details(self, value):
        self._details = _copy_json_value(value)
        self._details_json = _runtime_error_details_json(self._details)

    @property
    def details_json(self):
        return self._details_json

    @details_json.setter
    def details_json(self, value):
        if value is None:
            self._details_json = None
            self._details = None
            return
        if isinstance(value, str):
            self._details_json = value
            try:
                self._details = _copy_json_value(json.loads(value))
            except Exception:
                self._details = value
            return
        self._details = _copy_json_value(value)
        self._details_json = _runtime_error_details_json(self._details)

    @property
    def originRuntime(self):
        return self.origin_runtime

    @originRuntime.setter
    def originRuntime(self, value):
        self.origin_runtime = value

    @property
    def boundaryPath(self):
        return self.boundary_path

    @boundaryPath.setter
    def boundaryPath(self, value):
        self.boundary_path = value

    @property
    def originalErrorHandle(self):
        return self.original_error_handle

    @originalErrorHandle.setter
    def originalErrorHandle(self, value):
        self.original_error_handle = value

    @property
    def detailsJson(self):
        return self.details_json

    @detailsJson.setter
    def detailsJson(self, value):
        self.details_json = value

    def to_dict(self):
        """Return a structured, JSON-serializable runtime error envelope."""
        return {
            "runtime": self.runtime,
            "origin_runtime": self.origin_runtime,
            "type": self.type,
            "message": self.message,
            "traceback": self.traceback,
            "stack_frames": _copy_json_value(self.stack_frames),
            "cause_chain": _copy_json_value(self.cause_chain),
            "boundary_path": self.boundary_path,
            "original_error_handle": self.original_error_handle,
            "details": _copy_json_value(self.details),
            "details_json": self.details_json,
        }

    def as_dict(self):
        """Alias for to_dict(), matching common Python error-envelope APIs."""
        return self.to_dict()

    def to_json(self):
        """Return the structured runtime error envelope as compact JSON."""
        return json.dumps(self.to_dict(), separators=(",", ":"))


def _copy_json_value(value):
    if isinstance(value, dict):
        return {key: _copy_json_value(item) for key, item in value.items()}
    if isinstance(value, list):
        return [_copy_json_value(item) for item in value]
    if isinstance(value, tuple):
        return [_copy_json_value(item) for item in value]
    return value


def _runtime_error_details_json(value):
    if value is None:
        return None
    try:
        return json.dumps(_copy_json_value(value), separators=(",", ":"))
    except Exception:
        return str(value)


def _parse_runtime_error_text(text, runtime=None, boundary_path=None):
    envelope = _parse_runtime_error_envelope(
        text,
        runtime=runtime,
        boundary_path=boundary_path,
    )
    if envelope is not None:
        return envelope

    source_runtime = runtime
    body = text
    if body.startswith("ERR:"):
        body = body[4:]
    boundary_parts = []
    for marker, label in (
        ("execute manifest: ", "execute manifest"),
        ("load manifest module: ", "load manifest module"),
        ("manifest module call: ", "manifest module call"),
    ):
        if body.startswith(marker):
            boundary_parts.append(label)
            body = body[len(marker) :]
            break

    op_match = re.match(r"(?P<op>[A-Za-z_][A-Za-z0-9_]*) \[(?P<runtime>[A-Za-z0-9_-]+)\]: (?P<body>.*)", body, re.S)
    if op_match:
        op_name = op_match.group("op")
        op_runtime = op_match.group("runtime")
        boundary_parts.append(f"{op_name}[{op_runtime}]")
        source_runtime = op_runtime
        body = op_match.group("body")

    runtime_ref_assign_match = re.match(r"runtime ref assign \[(?P<runtime>[A-Za-z0-9_-]+)\]: (?P<body>.*)", body, re.S)
    if runtime_ref_assign_match:
        source_runtime = runtime_ref_assign_match.group("runtime")
        body = runtime_ref_assign_match.group("body")

    changed = True
    while changed:
        changed = False
        for prefix, canonical in (
            ("javascript: ", "javascript"),
            ("python: ", "python"),
            ("ruby: ", "ruby"),
            ("jvm: ", "java"),
            ("java: ", "java"),
            ("go: ", "go"),
        ):
            if body.startswith(prefix):
                source_runtime = canonical
                body = body[len(prefix) :]
                changed = True
                break

    wrapped_boundary = " > ".join(boundary_parts) or (
        f"call[{source_runtime}]" if source_runtime and source_runtime != runtime else boundary_path
    )
    envelope = _parse_runtime_error_envelope(
        body,
        runtime=source_runtime,
        boundary_path=wrapped_boundary,
    )
    if envelope is not None:
        return envelope

    first_line, _, rest = body.partition("\n")
    err_type = ""
    detail = first_line
    original_error_handle = None

    handle_match = re.search(
        r"(?im)^\s*(?:Original[- ]error[- ]handle|original_error_handle):\s*(?P<handle>\S+)\s*$",
        body,
    )
    if handle_match:
        original_error_handle = handle_match.group("handle")

    parse_line = first_line
    traceback = rest
    if first_line.startswith("Traceback "):
        traceback = body
        traceback_lines = [line.strip() for line in body.splitlines() if line.strip()]
        for line in reversed(traceback_lines):
            if _is_runtime_error_metadata_line(line):
                continue
            if ": " not in line:
                continue
            candidate, _ = line.split(": ", 1)
            if _is_error_type_candidate(candidate):
                parse_line = line
                break

    if ": " in parse_line:
        candidate, tail = parse_line.split(": ", 1)
        if _is_error_type_candidate(candidate):
            if source_runtime == "python" and "." in candidate:
                candidate = candidate.rsplit(".", 1)[-1]
            err_type = candidate
            detail = tail

    cause_chain = []
    if rest:
        for line in rest.splitlines():
            stripped = line.strip()
            if not stripped.startswith("Caused by: "):
                continue
            cause_text = stripped[len("Caused by: ") :]
            cause_type = ""
            cause_message = cause_text
            if ": " in cause_text:
                candidate, tail = cause_text.split(": ", 1)
                if _is_error_type_candidate(candidate):
                    cause_type = candidate
                    cause_message = tail
            cause_chain.append(
                {
                    "type": cause_type,
                    "message": cause_message,
                    "runtime": source_runtime,
                    "origin_runtime": source_runtime,
                }
            )

    return {
        "runtime": source_runtime,
        "origin_runtime": source_runtime,
        "type": err_type,
        "message": detail,
        "traceback": traceback,
        "stack_frames": _runtime_error_stack_frames(traceback),
        "cause_chain": cause_chain,
        "boundary_path": wrapped_boundary,
        "original_error_handle": original_error_handle,
        "details": _parse_runtime_error_details(body),
        "details_json": _parse_runtime_error_details_json(body),
    }


def _parse_runtime_error_envelope(text, runtime=None, boundary_path=None):
    body = str(text or "").strip()
    if not body.startswith("{"):
        return None
    try:
        envelope = json.loads(body)
    except Exception:
        return None
    if not isinstance(envelope, dict):
        return None
    def field(preferred, fallback):
        value = envelope.get(preferred)
        return envelope.get(fallback) if value is None else value
    def text_field(value, fallback=""):
        return str(value) if value is not None else fallback
    def details_field(source):
        if not isinstance(source, dict):
            return None
        if "details" in source:
            return _copy_json_value(source.get("details"))
        raw_details = source.get("details_json")
        if raw_details is None:
            raw_details = source.get("detailsJson")
        if isinstance(raw_details, str):
            try:
                return json.loads(raw_details)
            except Exception:
                return raw_details
        return _copy_json_value(raw_details) if raw_details is not None else None
    def details_json_field(source):
        if not isinstance(source, dict):
            return None
        if "details" in source:
            return _runtime_error_details_json(source.get("details"))
        raw_details = source.get("details_json")
        if raw_details is None:
            raw_details = source.get("detailsJson")
        if raw_details is None:
            return None
        return raw_details if isinstance(raw_details, str) else _runtime_error_details_json(raw_details)
    runtime_name = text_field(envelope.get("runtime"), runtime)
    origin_runtime = text_field(field("origin_runtime", "originRuntime"), runtime_name)
    err_type = text_field(field("type", "name"))
    message = text_field(envelope.get("message"))
    traceback = text_field(field("traceback", "stack"))
    if not any((runtime_name, err_type, message, traceback)):
        return None
    stack_frames = field("stack_frames", "stackFrames")
    if not isinstance(stack_frames, list) or not all(isinstance(frame, str) for frame in stack_frames):
        stack_frames = _runtime_error_stack_frames(traceback)
    cause_chain = field("cause_chain", "causeChain")
    if not isinstance(cause_chain, list):
        cause_chain = []
    else:
        parsed_causes = []
        for cause in cause_chain:
            if not isinstance(cause, dict):
                continue
            item = {
                "type": str(cause.get("type") or cause.get("name") or ""),
                "message": str(cause.get("message") or ""),
            }
            cause_traceback = cause.get("traceback")
            if cause_traceback is None:
                cause_traceback = cause.get("stack")
            if isinstance(cause_traceback, str):
                item["traceback"] = cause_traceback
            cause_stack_frames = cause.get("stack_frames")
            if cause_stack_frames is None:
                cause_stack_frames = cause.get("stackFrames")
            if isinstance(cause_stack_frames, list) and all(isinstance(frame, str) for frame in cause_stack_frames):
                item["stack_frames"] = list(cause_stack_frames)
            elif isinstance(cause_traceback, str):
                item["stack_frames"] = _runtime_error_stack_frames(cause_traceback)
            cause_field_pairs = (
                ("runtime", "runtime"),
                ("origin_runtime", "originRuntime"),
                ("boundary_path", "boundaryPath"),
                ("original_error_handle", "originalErrorHandle"),
            )
            for key, fallback in cause_field_pairs:
                value = cause.get(key)
                if value is None:
                    value = cause.get(fallback)
                if value:
                    item[key] = str(value)
            if not item.get("runtime") and runtime_name:
                item["runtime"] = runtime_name
            if "runtime" in item and not item.get("origin_runtime"):
                item["origin_runtime"] = item["runtime"]
            cause_details = details_field(cause)
            if cause_details is not None:
                item["details"] = cause_details
            cause_details_json = details_json_field(cause)
            if cause_details_json is not None:
                item["details_json"] = cause_details_json
            parsed_causes.append(item)
        cause_chain = parsed_causes
    return {
        "runtime": runtime_name,
        "origin_runtime": origin_runtime,
        "type": err_type,
        "message": message,
        "traceback": traceback,
        "stack_frames": stack_frames,
        "cause_chain": cause_chain,
        "boundary_path": text_field(field("boundary_path", "boundaryPath"), boundary_path),
        "original_error_handle": text_field(field("original_error_handle", "originalErrorHandle"), None),
        "details": details_field(envelope),
        "details_json": details_json_field(envelope),
    }


def _is_error_type_candidate(candidate):
    return bool(re.match(r"^[A-Za-z_][A-Za-z0-9_.$:]*$", candidate or ""))


def _is_runtime_error_metadata_line(line):
    lower = (line or "").strip().lower()
    return (
        lower.startswith("caused by:")
        or lower.startswith("details:")
        or lower.startswith("details_json:")
        or lower.startswith("detailsjson:")
        or lower.startswith("original_error_handle:")
        or lower.startswith("original error handle:")
        or lower.startswith("original-error-handle:")
    )


def _split_runtime_error_details_metadata_line(line):
    stripped = (line or "").strip()
    if ":" not in stripped:
        return None, None
    label, raw = stripped.split(":", 1)
    label = label.strip().lower()
    if label not in ("details", "details_json", "detailsjson"):
        return None, None
    return label, raw.strip()


def _runtime_error_stack_frames(traceback):
    return [
        line.strip()
        for line in str(traceback or "").splitlines()
        if line.strip() and not _is_runtime_error_metadata_line(line)
    ]


def _parse_runtime_error_details(text):
    for line in str(text).splitlines():
        label, raw = _split_runtime_error_details_metadata_line(line)
        if label is None:
            continue
        try:
            value = json.loads(raw)
        except Exception:
            return raw if label in ("details_json", "detailsjson") else None
        return value
    return None


def _parse_runtime_error_details_json(text):
    for line in str(text).splitlines():
        label, raw = _split_runtime_error_details_metadata_line(line)
        if label is None:
            continue
        if label in ("details_json", "detailsjson"):
            return raw or None
        try:
            value = json.loads(raw)
        except Exception:
            return None
        return _runtime_error_details_json(value)
    return None


# Lazy-loaded shared library handle. Not loaded until init_runtimes() is called.
# This is critical for prefork servers: the master process must not load the Go
# runtime before fork(). Each worker loads it independently post-fork.
_lib = None
_lock = threading.Lock()
_worker_drain_hook_installed = False
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
        lib.OmniInit.restype = ctypes.c_void_p

        lib.OmniCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
        lib.OmniCall.restype = ctypes.c_void_p

        if hasattr(lib, "OmniCallHost"):
            lib.OmniCallHost.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniCallHost.restype = ctypes.c_void_p

        lib.OmniExec.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
        lib.OmniExec.restype = ctypes.c_void_p

        if hasattr(lib, "OmniExecHost"):
            lib.OmniExecHost.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniExecHost.restype = ctypes.c_void_p

        lib.OmniRunManifestFile.argtypes = [ctypes.c_char_p]
        lib.OmniRunManifestFile.restype = ctypes.c_void_p

        if hasattr(lib, "OmniLoadManifestModule"):
            lib.OmniLoadManifestModule.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniLoadManifestModule.restype = ctypes.c_void_p

        if hasattr(lib, "OmniDrainWorker"):
            lib.OmniDrainWorker.argtypes = []
            lib.OmniDrainWorker.restype = ctypes.c_void_p

        if hasattr(lib, "OmniUnloadManifestModules"):
            lib.OmniUnloadManifestModules.argtypes = []
            lib.OmniUnloadManifestModules.restype = ctypes.c_void_p

        if hasattr(lib, "OmniManifestCall"):
            lib.OmniManifestCall.argtypes = [ctypes.c_char_p, ctypes.c_char_p]
            lib.OmniManifestCall.restype = ctypes.c_void_p

        lib.OmniBufGet.argtypes = [
            ctypes.c_char_p,
            ctypes.POINTER(_OmniBuffer),
        ]
        lib.OmniBufGet.restype = ctypes.c_int

        lib.OmniBufSet.argtypes = [ctypes.c_char_p, _OmniBuffer]
        lib.OmniBufSet.restype = ctypes.c_int

        lib.OmniBufRelease.argtypes = [ctypes.c_char_p]
        lib.OmniBufRelease.restype = None

        if hasattr(lib, "OmniBufFree"):
            lib.OmniBufFree.argtypes = [ctypes.c_char_p]
            lib.OmniBufFree.restype = ctypes.c_int

        if hasattr(lib, "OmniBufStatus"):
            lib.OmniBufStatus.argtypes = [ctypes.c_char_p]
            lib.OmniBufStatus.restype = ctypes.c_void_p

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
        lib.OmniLoadPlugin.restype = ctypes.c_void_p

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
            lib.OmniWatchdogCapabilities.restype = ctypes.c_void_p

        if hasattr(lib, "OmniWorkerTainted"):
            lib.OmniWorkerTainted.argtypes = []
            lib.OmniWorkerTainted.restype = ctypes.c_int

        if hasattr(lib, "OmniLastTimeoutRuntime"):
            lib.OmniLastTimeoutRuntime.argtypes = []
            lib.OmniLastTimeoutRuntime.restype = ctypes.c_void_p

        if hasattr(lib, "OmniWorkerTaintReason"):
            lib.OmniWorkerTaintReason.argtypes = []
            lib.OmniWorkerTaintReason.restype = ctypes.c_void_p

        if hasattr(lib, "OmniStatus"):
            lib.OmniStatus.argtypes = []
            lib.OmniStatus.restype = ctypes.c_void_p

        if hasattr(lib, "OmniClearWorkerTaintForTest"):
            lib.OmniClearWorkerTaintForTest.argtypes = []
            lib.OmniClearWorkerTaintForTest.restype = None

        lib.OmniFree.argtypes = [ctypes.c_void_p]
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


def _omni_value_to_py(ov, runtime=None, boundary_path=None):
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
        raise RuntimeError(msg, runtime=runtime, boundary_path=boundary_path)
    return None


def _check_result(result, runtime=None, boundary_path=None):
    """Check a C string result for ERR: prefix and raise if needed."""
    if result is None or result == 0:
        raise RuntimeError(
            "call returned NULL",
            runtime=runtime,
            boundary_path=boundary_path,
        )
    free_ptr = None
    if isinstance(result, int):
        free_ptr = result
        raw = ctypes.string_at(result)
    elif isinstance(result, ctypes.c_void_p):
        if not result.value:
            raise RuntimeError(
                "call returned NULL",
                runtime=runtime,
                boundary_path=boundary_path,
            )
        free_ptr = result.value
        raw = ctypes.string_at(result.value)
    else:
        raw = result
    try:
        text = raw.decode("utf-8") if isinstance(raw, bytes) else raw
    finally:
        if free_ptr is not None and _lib is not None and hasattr(_lib, "OmniFree"):
            _lib.OmniFree(free_ptr)
    if text.startswith("OK:"):
        return text[3:]
    if text.startswith("ERR:"):
        raise RuntimeError(text[4:], runtime=runtime, boundary_path=boundary_path)
    return text


def init_runtimes(runtimes):
    """
    Initialize OmniVM runtimes. Call this in Gunicorn's post_fork hook.

    This is when libomnivm.so is loaded (via dlopen) and the Go runtime starts.
    Must be called AFTER fork in prefork servers. The initializing thread
    becomes the libomnivm host thread; framework integrations should call this
    from the worker thread that will own direct OmniVM runtime calls.

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

    return _check_result(result, runtime=runtime, boundary_path=f"call[{runtime}]")


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

    return _omni_value_to_py(
        result,
        runtime=runtime,
        boundary_path=f"call_typed[{runtime}]",
    )


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
    return _check_result(result, runtime=runtime, boundary_path=f"execute[{runtime}]")


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
    checked = _check_result(result)
    _clear_manifest_proxy_cache(module_id)
    return checked


def unload_manifest_modules():
    """
    Release all retained manifest module handles before recycling a worker.

    Use this from a server worker-drain/reload hook when live manifest proxies
    might still exist. It intentionally unloads every retained manifest module
    in this process because retained handles share the process handle table.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized — call init_runtimes() first")
    if not hasattr(_lib, "OmniUnloadManifestModules"):
        raise RuntimeError("libomnivm does not expose OmniUnloadManifestModules")
    result = _lib.OmniUnloadManifestModules()
    checked = _check_result(result)
    _clear_manifest_proxy_cache()
    return checked


def drain_worker():
    """
    Release live OmniVM process state before recycling a server worker.

    Call this from worker-drain/reload hooks when live proxies, retained
    manifest modules, streams, or resource handles might still exist. The
    operation is process-wide because live handles share one process handle
    table.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized — call init_runtimes() first")
    if hasattr(_lib, "OmniDrainWorker"):
        result = _lib.OmniDrainWorker()
    elif hasattr(_lib, "OmniUnloadManifestModules"):
        result = _lib.OmniUnloadManifestModules()
    else:
        raise RuntimeError("libomnivm does not expose OmniDrainWorker")
    checked = _check_result(result, boundary_path="drain_worker")
    _clear_manifest_proxy_cache()
    return checked


def drain_worker_hook(*_args, **_kwargs):
    """
    App-server hook compatible wrapper for drain_worker().

    Gunicorn and similar servers pass server/worker objects to lifecycle hooks;
    this function accepts and ignores those arguments so configs can assign it
    directly. If OmniVM was never initialized in this worker, it is a no-op.
    Once libomnivm is loaded, real drain failures still raise RuntimeError.
    """
    if _lib is None:
        return None
    return drain_worker()


def install_worker_drain_hook():
    """
    Register drain_worker_hook() with atexit and return the hook.

    This is a small safety net for app-server reload/exit paths. Prefer an
    explicit server hook when the server exposes one; the atexit registration
    covers ordinary worker process exit and is idempotent per process.
    """
    global _worker_drain_hook_installed
    if not _worker_drain_hook_installed:
        atexit.register(drain_worker_hook)
        _worker_drain_hook_installed = True
    return drain_worker_hook


def drain_finalizer_releases(max_releases=0):
    """
    Quietly drain queued proxy-finalizer releases from a safe host callback.

    Call this at the end of a request, job, or framework lifecycle callback to
    avoid waiting for process-wide worker drain. It is intentionally best-effort:
    cleanup-only finalizer paths must stay idempotent and should not mask the
    application error that caused the cleanup path to run. Use drain_worker()
    for explicit worker-reload failures that should be reported.
    """
    return _drain_finalizer_releases(max_releases)


class _LifecycleScope:
    def __init__(self, max_finalizer_releases=0):
        self.max_finalizer_releases = max_finalizer_releases
        self.drained_finalizers = None

    def __enter__(self):
        return self

    def __exit__(self, _exc_type, _exc, _tb):
        self.drained_finalizers = drain_finalizer_releases(self.max_finalizer_releases)
        return False


def lifecycle_scope(max_finalizer_releases=0):
    """
    Return a request/job lifecycle context that drains finalizer cleanup on exit.

    The scope does not own live proxies; callers should still close streams,
    handles, and buffers explicitly when their lifetime is known. This only
    drains queued GC/finalizer releases as quiet teardown, preserving any
    exception raised by the application body.
    """
    return _LifecycleScope(max_finalizer_releases)


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
        result = _manifest_bridge_call(module_id, payload)
        if retained_keys:
            lease = _RetainedManifestArgLease(retained_keys)
            if _attach_retained_arg_lease(result, lease) > 0:
                retained_keys = []
        return result
    finally:
        _release_manifest_args(retained_keys)


def _manifest_bridge_call(module_id, payload):
    result = _lib.OmniManifestCall(
        str(module_id).encode("utf-8"),
        json.dumps(payload, separators=(",", ":")).encode("utf-8"),
    )
    return _decode_manifest_result(_check_result(result), module_id=module_id)


def _manifest_bridge_call_unwrapped(module_id, payload):
    result = _lib.OmniManifestCall(
        str(module_id).encode("utf-8"),
        json.dumps(payload, separators=(",", ":")).encode("utf-8"),
    )
    return _decode_manifest_result_unwrapped(_check_result(result))


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


class _RetainedManifestArgLease:
    def __init__(self, keys):
        self._keys = list(keys)
        self._refs = 0
        self._released = False
        self._lock = threading.Lock()

    def retain(self):
        with self._lock:
            if self._released:
                return
            self._refs += 1

    def release(self):
        keys = None
        with self._lock:
            if self._released:
                return
            if self._refs > 0:
                self._refs -= 1
            if self._refs == 0:
                self._released = True
                keys = self._keys
                self._keys = []
        if keys:
            _release_manifest_args(keys)


def _release_retained_arg_lease(lease):
    lease.release()


def _attach_retained_arg_lease(value, lease):
    attached = 0
    if isinstance(value, ManifestProxy):
        value._retain_arg_lease(lease)
        return 1
    if isinstance(value, dict):
        for item in value.values():
            attached += _attach_retained_arg_lease(item, lease)
    elif isinstance(value, (list, tuple)):
        for item in value:
            attached += _attach_retained_arg_lease(item, lease)
    return attached


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


def _decode_manifest_result_unwrapped(result):
    if result == "":
        return None
    try:
        envelope = json.loads(result)
    except json.JSONDecodeError:
        return result
    if not isinstance(envelope, dict) or not envelope.get("__omnivm_result__"):
        return envelope
    if envelope.get("kind") == "null":
        return None
    return envelope.get("value")


_manifest_proxy_cache = weakref.WeakValueDictionary()


def _clear_manifest_proxy_cache(module_id=None):
    if module_id is None:
        _manifest_proxy_cache.clear()
        return
    module_key = str(module_id)
    for cache_key in list(_manifest_proxy_cache.keys()):
        if cache_key[0] == module_key:
            _manifest_proxy_cache.pop(cache_key, None)


def _wrap_manifest_value(module_id, value):
    if isinstance(value, dict):
        if _is_local_stream_descriptor(value):
            return _LocalManifestStreamProxy(module_id, value)
        if _is_manifest_proxy_descriptor(value) and module_id is not None:
            cache_key = (str(module_id), int(value["id"]))
            cached = _manifest_proxy_cache.get(cache_key)
            if cached is not None and not object.__getattribute__(cached, "_closed"):
                return cached
            proxy = ManifestProxy(module_id, value)
            _manifest_proxy_cache[cache_key] = proxy
            return proxy
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


def _is_local_stream_descriptor(value):
    return (
        value.get("__omnivm_stream__") is True
        or value.get("__omnivm_channel__") is True
    ) and isinstance(value.get("values"), list) and value.get("id") is None


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
        object.__setattr__(self, "_arg_finalizers", [])
        object.__setattr__(self, "_closed", False)
        if descriptor.get("transfer") is True:
            _manifest_bridge_call(module_id, {"op": "handle_adopt", "id": self._handle_id})
        else:
            _manifest_bridge_call(module_id, {"op": "handle_retain", "id": self._handle_id})
        object.__setattr__(
            self,
            "_finalizer",
            weakref.finalize(self, _manifest_proxy_release, self._module_id, self._handle_id),
        )

    def _is_stream_proxy(self):
        descriptor = object.__getattribute__(self, "_descriptor")
        return (
            descriptor.get("__omnivm_stream__") is True
            or descriptor.get("__omnivm_channel__") is True
        )

    def _release_arg_finalizers(self):
        for arg_finalizer in object.__getattribute__(self, "_arg_finalizers"):
            if arg_finalizer.alive:
                arg_finalizer()

    def _detach_after_remote_close(self):
        object.__setattr__(self, "_closed", True)
        finalizer = object.__getattribute__(self, "_finalizer")
        if finalizer.alive:
            finalizer.detach()
        self._release_arg_finalizers()

    @property
    def __omnivm_descriptor__(self):
        return dict(self._descriptor)

    @property
    def __omnivm_handle_id__(self):
        return self._handle_id

    def _remote_lifecycle_named_field(self, key, missing):
        if object.__getattribute__(self, "_closed"):
            return missing
        handle_id = object.__getattribute__(self, "_handle_id")
        if not bool(self._op({"op": "handle_contains", "id": handle_id, "value": key})):
            return missing
        result = self._op({"op": "handle_get", "id": handle_id, "key": key})
        if isinstance(result, dict) and result.get("__omnivm_callable__") is True:
            return _ManifestProxyMethod(self, key)
        return result

    def close(self):
        if object.__getattribute__(self, "_closed"):
            return False
        op = "stream_cancel" if self._is_stream_proxy() else "handle_release_explicit"
        released = bool(_manifest_bridge_call(
            object.__getattribute__(self, "_module_id"),
            {
                "op": op,
                "id": object.__getattribute__(self, "_handle_id"),
            },
        ))
        if released:
            self._detach_after_remote_close()
        return released

    def _omnivm_close(self):
        return object.__getattribute__(self, "close")()

    def _retain_arg_lease(self, lease):
        lease.retain()
        object.__getattribute__(self, "_arg_finalizers").append(
            weakref.finalize(self, _release_retained_arg_lease, lease)
        )

    def __enter__(self):
        return self

    def __exit__(self, _exc_type, exc, _tb):
        if _exc_type is None:
            self._omnivm_close()
            return False
        try:
            self._omnivm_close()
        except BaseException as close_exc:
            _record_cleanup_error(
                exc,
                close_exc,
                f"OmniVM proxy close failed during exception cleanup: {close_exc}",
            )
        return False

    def __repr__(self):
        descriptor = object.__getattribute__(self, "_descriptor")
        runtime = descriptor.get("runtime", "unknown")
        kind = descriptor.get("kind", "object")
        return f"<omnivm.ManifestProxy {runtime}:{kind}#{self._handle_id}>"

    def __getattribute__(self, key):
        if key in ("close", "dispose"):
            missing = object()
            value = object.__getattribute__(self, "_remote_lifecycle_named_field")(key, missing)
            if value is not missing:
                return value
        return object.__getattribute__(self, key)

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
            result = self._op(payload)
            if retained_keys:
                lease = _RetainedManifestArgLease(retained_keys)
                if _attach_retained_arg_lease(result, lease) > 0:
                    retained_keys = []
            return result
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


class _LocalManifestStreamProxy:
    """One-shot stream wrapper for manifest values embedded in an envelope."""

    def __init__(self, module_id, descriptor):
        self._module_id = module_id
        self._values = list(descriptor.get("values") or [])
        self._cursor = 0
        self._closed = False

    def close(self):
        if self._closed:
            return False
        self._closed = True
        self._cursor = len(self._values)
        return True

    def _omnivm_close(self):
        return self.close()

    def __iter__(self):
        try:
            while not self._closed and self._cursor < len(self._values):
                item = self._values[self._cursor]
                self._cursor += 1
                yield _wrap_manifest_value(self._module_id, item)
            if not self._closed:
                self.close()
        finally:
            if not self._closed:
                self.close()

    def __len__(self):
        remaining = len(self._values) - self._cursor
        return remaining if remaining > 0 and not self._closed else 0

    def __enter__(self):
        return self

    def __exit__(self, _exc_type, _exc, _tb):
        self.close()
        return False

    def __repr__(self):
        return f"<omnivm.LocalManifestStream remaining={len(self)}>"


def proxy_get(value, key):
    """Return a proxy field/key without colliding with local proxy methods."""
    if isinstance(value, ManifestProxy):
        return value._op({"op": "handle_get", "id": value.__omnivm_handle_id__, "key": str(key)})
    if isinstance(value, dict):
        return value[key]
    try:
        return value[key]
    except (TypeError, KeyError, IndexError):
        return getattr(value, str(key))


def proxy_set(value, key, next_value):
    """Set a proxy field/key without colliding with local proxy methods."""
    if isinstance(value, ManifestProxy):
        retained_keys = []
        try:
            value._op({
                "op": "handle_set",
                "id": value.__omnivm_handle_id__,
                "key": str(key),
                "value": value._arg(next_value, retained_keys),
            })
            return True
        finally:
            _release_manifest_args(retained_keys)
    if isinstance(value, dict):
        value[key] = next_value
        return True
    try:
        value[key] = next_value
    except TypeError:
        setattr(value, str(key), next_value)
    return True


def proxy_call(value, key=None, args=(), kwargs=None):
    """Call a proxy method without colliding with local proxy methods."""
    call_args = tuple(args or ())
    call_kwargs = dict(kwargs or {})
    if isinstance(value, ManifestProxy):
        return value._method_call("" if key is None else str(key), call_args, call_kwargs)
    if key is None or key == "":
        return value(*call_args, **call_kwargs)
    method = _actual_public_method(value, str(key))
    if method is None:
        raw = inspect.getattr_static(value, str(key))
        if not callable(raw):
            if inspect.isdatadescriptor(raw):
                raise AttributeError(str(key))
            raise TypeError(f"{str(key)!r} is not callable")
        raise AttributeError(str(key))
    return method(*call_args, **call_kwargs)


def proxy_len(value):
    """Return remote collection length without colliding with a data field."""
    if isinstance(value, ManifestProxy):
        return int(value._op({"op": "handle_len", "id": value.__omnivm_handle_id__}))
    return len(value)


def proxy_iter(value, mode="values"):
    """Return remote keys/items/values without colliding with proxy methods."""
    mode = str(mode or "values")
    if mode not in ("keys", "items", "values"):
        raise ValueError("proxy_iter mode must be 'keys', 'items', or 'values'")
    if isinstance(value, ManifestProxy):
        return value._op({"op": "handle_iter", "id": value.__omnivm_handle_id__, "mode": mode})
    if mode == "keys":
        keys = _actual_public_method(value, "keys")
        if keys is not None:
            return list(keys())
        return list(range(len(value)))
    if mode == "items":
        items = _actual_public_method(value, "items")
        if items is not None:
            return list(items())
        return list(enumerate(value))
    values = _actual_public_method(value, "values")
    if values is not None:
        return list(values())
    return list(value)


def proxy_keys(value):
    """Return remote keys without colliding with a data field named keys."""
    return proxy_iter(value, "keys")


def proxy_values(value):
    """Return remote values without colliding with a data field named values."""
    return proxy_iter(value, "values")


def proxy_items(value):
    """Return remote items without colliding with a data field named items."""
    return proxy_iter(value, "items")


def proxy_contains(value, key):
    """Test remote membership without colliding with local proxy methods."""
    if isinstance(value, ManifestProxy):
        retained_keys = []
        try:
            return bool(value._op({
                "op": "handle_contains",
                "id": value.__omnivm_handle_id__,
                "value": value._arg(key, retained_keys),
            }))
        finally:
            _release_manifest_args(retained_keys)
    return key in value


def _actual_public_method(value, name):
    if value is None:
        return None
    try:
        raw = inspect.getattr_static(value, name)
    except Exception:
        return None
    if isinstance(raw, (staticmethod, classmethod)):
        try:
            method = raw.__get__(value, type(value))
        except Exception:
            return None
        return method if callable(method) else None
    if inspect.ismemberdescriptor(raw):
        try:
            method = raw.__get__(value, type(value))
        except Exception:
            return None
        return method if callable(method) else None
    if not callable(raw):
        return None
    try:
        instance_dict = object.__getattribute__(value, "__dict__")
    except Exception:
        instance_dict = None
    if isinstance(instance_dict, dict) and instance_dict.get(name) is raw:
        return raw
    if hasattr(raw, "__get__") and (
        inspect.isfunction(raw) or inspect.ismethoddescriptor(raw) or inspect.isbuiltin(raw)
    ):
        try:
            method = raw.__get__(value, type(value))
        except Exception:
            return None
        return method if callable(method) else None
    if not hasattr(raw, "__get__"):
        return raw
    return None


def _lifecycle_method_accepts_no_args(method):
    try:
        signature = inspect.signature(method)
    except (TypeError, ValueError):
        return True
    for parameter in signature.parameters.values():
        if parameter.kind in (parameter.VAR_POSITIONAL, parameter.VAR_KEYWORD):
            continue
        if parameter.default is inspect.Signature.empty:
            return False
    return True


def proxy_close(value):
    """Release a proxy lease without colliding with a data field named close."""
    if isinstance(value, ManifestProxy):
        return value._omnivm_close()
    close = _actual_public_method(value, "_omnivm_close")
    if callable(close) and _lifecycle_method_accepts_no_args(close):
        return close()
    close = _actual_public_method(value, "close")
    if callable(close) and _lifecycle_method_accepts_no_args(close):
        result = close()
        return True if result is None else result
    close = _actual_public_method(value, "dispose")
    if callable(close) and _lifecycle_method_accepts_no_args(close):
        result = close()
        return True if result is None else result
    return False


async def aproxy_close(value):
    """Async close helper for proxy leases and Python objects exposing close/aclose."""
    if isinstance(value, ManifestProxy):
        result = value._omnivm_close()
        return await result if inspect.isawaitable(result) else result
    close = _actual_public_method(value, "_omnivm_close")
    if callable(close) and _lifecycle_method_accepts_no_args(close):
        result = close()
        return await result if inspect.isawaitable(result) else result
    close = _actual_public_method(value, "close")
    if callable(close) and _lifecycle_method_accepts_no_args(close):
        result = close()
        if inspect.isawaitable(result):
            result = await result
        return True if result is None else result
    close = _actual_public_method(value, "aclose")
    if callable(close) and _lifecycle_method_accepts_no_args(close):
        result = close()
        if inspect.isawaitable(result):
            result = await result
        return True if result is None else result
    close = _actual_public_method(value, "dispose")
    if callable(close) and _lifecycle_method_accepts_no_args(close):
        result = close()
        if inspect.isawaitable(result):
            result = await result
        return True if result is None else result
    return False


def omnivm_close(value):
    """Alias for proxy_close(), matching generated Python manifest snippets."""
    return proxy_close(value)


async def omnivm_aclose(value):
    """Alias for aproxy_close(), matching generated Python manifest snippets."""
    return await aproxy_close(value)


def _record_cleanup_error(error, cleanup_error, note):
    try:
        errors = getattr(error, "omnivm_cleanup_errors", None)
        if not isinstance(errors, list):
            errors = []
        errors.append(cleanup_error)
        setattr(error, "omnivm_cleanup_errors", errors)
    except BaseException:
        pass
    add_note = getattr(error, "add_note", None)
    if callable(add_note):
        add_note(note)


def cleanup_errors(error):
    """
    Return cleanup exceptions recorded while preserving a body exception.

    Context managers keep the original application exception as primary and
    record failed close/release attempts here for structured inspection.
    """
    errors = getattr(error, "omnivm_cleanup_errors", None)
    return list(errors) if isinstance(errors, list) else []


def _manifest_stream_iterator_release(proxy):
    try:
        proxy._omnivm_close()
    except BaseException:
        pass


class _ManifestStreamIterator:
    def __init__(self, proxy):
        self._proxy = proxy
        self._finalizer = weakref.finalize(self, _manifest_stream_iterator_release, proxy)

    def __iter__(self):
        return self

    def _detach_finalizer(self):
        finalizer = object.__getattribute__(self, "_finalizer")
        if finalizer.alive:
            finalizer.detach()

    def __next__(self):
        if object.__getattribute__(self._proxy, "_closed"):
            self._detach_finalizer()
            raise StopIteration
        try:
            item = _manifest_bridge_call_unwrapped(
                self._proxy._module_id,
                {"op": "stream_next", "id": self._proxy.__omnivm_handle_id__},
            )
        except BaseException:
            self._detach_finalizer()
            self._proxy._detach_after_remote_close()
            raise
        if not isinstance(item, dict) or "done" not in item:
            err = RuntimeError(
                f"OmniVM stream_next returned malformed chunk for handle {self._proxy.__omnivm_handle_id__}: expected an object with a done flag",
                boundary_path="stream_next",
                details={"stream": {"id": self._proxy.__omnivm_handle_id__, "chunk": item}},
            )
            try:
                self.close()
            except BaseException as close_exc:
                _record_cleanup_error(
                    err,
                    close_exc,
                    f"OmniVM stream close failed during malformed chunk cleanup: {close_exc}",
                )
                self._detach_finalizer()
                self._proxy._detach_after_remote_close()
            raise err
        if item.get("done") is True:
            self._detach_finalizer()
            self._proxy._detach_after_remote_close()
            raise StopIteration
        try:
            return _wrap_manifest_value(self._proxy._module_id, item.get("value"))
        except BaseException as err:
            try:
                self.close()
            except BaseException as close_exc:
                _record_cleanup_error(
                    err,
                    close_exc,
                    f"OmniVM stream close failed during chunk materialization cleanup: {close_exc}",
                )
                self._detach_finalizer()
                self._proxy._detach_after_remote_close()
            raise

    def close(self):
        result = self._proxy._omnivm_close()
        self._detach_finalizer()
        return result

    def _omnivm_close(self):
        return self.close()

    def __enter__(self):
        return self

    def __exit__(self, _exc_type, exc, _tb):
        if _exc_type is None:
            self.close()
            return False
        try:
            self.close()
        except BaseException as close_exc:
            _record_cleanup_error(
                exc,
                close_exc,
                f"OmniVM stream close failed during exception cleanup: {close_exc}",
            )
        return False


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


def affinity_status():
    """
    Return current Python thread/loop affinity diagnostics.

    The snapshot is intended for app-server startup checks and callback
    diagnostics. It does not move work between threads; it makes the current
    thread and active asyncio loop relationship explicit.
    """
    host_tid = host_thread_id()
    current_tid = threading.get_native_id()
    info = {
        "host_thread_id": host_tid,
        "current_thread_id": current_tid,
        "on_host_thread": current_tid == host_tid,
        "thread_name": threading.current_thread().name,
    }
    try:
        import asyncio

        loop = asyncio.get_running_loop()
    except _builtins.RuntimeError:
        info["asyncio"] = {
            "running": False,
            "loop_id": None,
            "closed": None,
        }
    else:
        info["asyncio"] = {
            "running": True,
            "loop_id": id(loop),
            "closed": loop.is_closed(),
        }
    return _copy_json_value(info)


def owner_dispatch_status():
    """
    Return the process-level owner-dispatch capability contract.

    This is the machine-readable companion to affinity_status(): it tells app
    integrations whether OmniVM can move callbacks back to owner schedulers, and
    which runtime-specific affinity diagnostics are available.
    """
    status_info = status()
    info = status_info.get("thread_affinity")
    if not isinstance(info, dict):
        raise RuntimeError(
            "libomnivm status omitted thread_affinity capability",
            boundary_path="thread_affinity",
            details={"status": status_info},
        )
    return _copy_json_value(info)


_OWNER_DISPATCH_TARGET_ALIASES = {
    "asyncio": "python_asyncio",
    "python": "python_asyncio",
    "python_loop": "python_asyncio",
    "python_async_loop": "python_asyncio",
    "py": "python_asyncio",
    "javascript": "javascript_event_loop",
    "js": "javascript_event_loop",
    "javascript_loop": "javascript_event_loop",
    "node": "javascript_event_loop",
    "nodejs": "javascript_event_loop",
    "event_loop": "javascript_event_loop",
    "java": "java_executor",
    "jvm": "java_executor",
    "executor": "java_executor",
    "ruby": "ruby_fiber_thread",
    "fiber": "ruby_fiber_thread",
    "thread": "ruby_fiber_thread",
    "ruby_fiber": "ruby_fiber_thread",
    "ruby_thread": "ruby_fiber_thread",
}


def _owner_dispatch_target_name(target):
    target_name = str(target)
    normalized = target_name.strip().lower().replace("-", "_").replace(" ", "_")
    return _OWNER_DISPATCH_TARGET_ALIASES.get(normalized, normalized)


def owner_dispatch_target_status(target):
    """
    Return the owner-dispatch capability block for one owner kind.

    Known target names are reported by owner_dispatch_status()["owner_dispatch_targets"],
    such as "python_asyncio", "javascript_event_loop", "java_executor", and
    "ruby_fiber_thread". Common aliases such as "asyncio", "js", "java", and
    "ruby" are normalized to those canonical target names.
    """
    requested_target = str(target)
    target_name = _owner_dispatch_target_name(requested_target)
    dispatch_info = owner_dispatch_status()
    targets = dispatch_info.get("owner_dispatch_targets")
    if not isinstance(targets, dict):
        raise RuntimeError(
            "libomnivm status omitted owner_dispatch_targets capability",
            boundary_path="owner_dispatch_target",
            details={
                "target": target_name,
                "requested_target": requested_target,
                "owner_dispatch": dispatch_info,
            },
        )
    info = targets.get(target_name)
    if not isinstance(info, dict):
        known_targets = sorted(str(name) for name in targets.keys())
        raise RuntimeError(
            (
                f"libomnivm status omitted owner dispatch target {target_name!r}; "
                f"known targets: {', '.join(known_targets) if known_targets else 'none'}"
            ),
            boundary_path="owner_dispatch_target",
            details={
                "target": target_name,
                "requested_target": requested_target,
                "known_targets": known_targets,
                "owner_dispatch_targets": targets,
                "owner_dispatch_target": {
                    "target": target_name,
                    "requested_target": requested_target,
                    "known_targets": known_targets,
                    "owner_dispatch_targets": targets,
                },
            },
        )
    info = _copy_json_value(info)
    info["requested_target"] = requested_target
    info["target"] = target_name
    return info


def assert_owner_dispatch_supported(label=""):
    """
    Raise RuntimeError when this build cannot migrate callbacks to owners.

    Use this in framework startup checks that require a universal owner
    loop/executor/thread dispatcher rather than diagnostic-only affinity guards.
    """
    info = owner_dispatch_status()
    if info.get("owner_dispatch_supported") is True:
        return True
    prefix = f"{label}: " if label else ""
    mode = info.get("mode") or "unknown"
    reason = info.get("reason") or "owner dispatch is not supported by this libomnivm build"
    raise RuntimeError(
        f"{prefix}owner dispatch unsupported: mode={mode}: {reason}",
        boundary_path="owner_dispatch",
        details={"owner_dispatch": info},
    )


def assert_owner_dispatch_target_supported(target, label=""):
    """
    Raise RuntimeError when this build cannot dispatch callbacks to one owner.

    Use this for startup checks that require one specific owner scheduler, for
    example "python_asyncio" for ASGI callbacks or "java_executor" for Java
    executor callbacks.
    """
    requested_target = str(target)
    target_name = _owner_dispatch_target_name(requested_target)
    info = owner_dispatch_target_status(requested_target)
    if info.get("supported") is True:
        return True
    prefix = f"{label}: " if label else ""
    diagnostic = info.get("diagnostic") or "owner dispatch is not supported for this target"
    raise RuntimeError(
        f"{prefix}owner dispatch target {target_name} unsupported: {diagnostic}",
        boundary_path="owner_dispatch_target",
        details={
            "target": target_name,
            "requested_target": requested_target,
            "owner_dispatch_target": info,
        },
    )


def ruby_threading_status():
    """
    Return the embedded Ruby threading capability contract.

    This lets host startup code decide whether a Ruby framework that requires
    native Ruby threads can run in process before loading that framework.
    """
    status_info = status()
    info = status_info.get("ruby_threading")
    if not isinstance(info, dict):
        raise RuntimeError(
            "libomnivm status omitted ruby_threading capability",
            boundary_path="ruby_threading",
            details={"status": status_info},
        )
    return info


def assert_ruby_native_threads_supported(label=""):
    """
    Raise RuntimeError when embedded Ruby cannot run native Ruby threads.

    Puma and other native-threaded Ruby app servers should use this as a
    startup guard and choose an out-of-process deployment when it fails.
    """
    info = ruby_threading_status()
    if info.get("native_threads_supported") is True:
        return True
    prefix = f"{label}: " if label else ""
    mode = info.get("mode") or "unknown"
    reason = info.get("app_server_boundary") or (
        "native Ruby threads are not supported by this libomnivm build"
    )
    raise RuntimeError(
        f"{prefix}native Ruby threads unsupported: mode={mode}: {reason}",
        boundary_path="ruby_threading",
        details={"ruby_threading": info},
    )


def assert_host_thread(label=""):
    """
    Raise RuntimeError if called from a non-host Python thread.

    Use this in server lifecycle callbacks or framework integrations that must
    run on the worker's owner thread. In c-shared mode direct runtime entrypoints
    reject foreign threads; this guard lets integrations fail before registering
    callbacks or request hooks that would later cross that boundary.
    """
    info = affinity_status()
    if info["on_host_thread"]:
        return True
    prefix = f"{label}: " if label else ""
    raise RuntimeError(
        f"{prefix}thread affinity violation: current OS thread "
        f"{info['current_thread_id']} is not libomnivm host thread "
        f"{info['host_thread_id']}",
        boundary_path="thread_affinity",
        details={"affinity": info},
    )


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
    if out.len == 0:
        _lib.OmniBufRelease(encoded_name)
        empty = memoryview(b"" if out.read_only else bytearray())
        return empty.toreadonly() if out.read_only else empty
    if not out.data or out.len < 0:
        _lib.OmniBufRelease(encoded_name)
        return None
    view_owner = (ctypes.c_char * int(out.len)).from_address(int(out.data))
    weakref.finalize(view_owner, _release_buffer_borrow, encoded_name)
    view = memoryview(view_owner).cast("B")
    if out.read_only:
        view = view.toreadonly()
    return view


def _release_buffer_borrow(encoded_name):
    try:
        if _lib is not None:
            _lib.OmniBufRelease(encoded_name)
    except BaseException:
        pass


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
    encoded_name = str(name).encode("utf-8")
    if hasattr(_lib, "OmniBufFree"):
        rc = _lib.OmniBufFree(encoded_name)
        if rc != 0:
            status_details = _buffer_status_details(name)
            detail = _buffer_status_summary(status_details)
            suffix = f": {detail}" if detail else ""
            raise RuntimeError(
                f"omnivm.release_buffer failed for {name!r}{suffix}",
                boundary_path="native_memory",
                details={"buffer": status_details} if status_details is not None else None,
            )
        return
    _lib.OmniBufRelease(encoded_name)


_UNSET = object()


class BufferOwner:
    """
    Context object for a named shared-buffer owner.

    release() is idempotent at the Python API level. The underlying
    release_buffer() call remains the user-initiated diagnostic path and can
    raise RuntimeError when libomnivm reports a native-memory lifecycle error.
    """

    def __init__(self, name, data=_UNSET, dtype=0):
        self.name = str(name)
        self._data = data
        self._dtype = dtype
        self.released = False
        self._entered = False

    def __enter__(self):
        if self.released:
            raise RuntimeError(
                f"omnivm.buffer_owner {self.name!r} cannot be re-entered after release",
                boundary_path="native_memory",
                details={"buffer": {"name": self.name, "released": True}},
            )
        if self._entered:
            raise RuntimeError(
                f"omnivm.buffer_owner {self.name!r} is already active",
                boundary_path="native_memory",
                details={"buffer": {"name": self.name, "active_owner": True}},
            )
        self._entered = True
        if self._data is not _UNSET:
            try:
                set_buffer(self.name, self._data, self._dtype)
            except BaseException:
                self._entered = False
                raise
        return self

    def __exit__(self, _exc_type, exc, _tb):
        try:
            if _exc_type is None:
                self.release()
                return False
            try:
                self.release()
            except BaseException as release_exc:
                _record_cleanup_error(
                    exc,
                    release_exc,
                    f"OmniVM buffer release failed during exception cleanup: {release_exc}",
                )
        finally:
            self._entered = False
        return False

    def release(self):
        if self.released:
            return False
        try:
            release_buffer(self.name)
        except RuntimeError as exc:
            details = getattr(exc, "details", None)
            status = details.get("buffer") if isinstance(details, dict) else None
            if _buffer_status_is_released(status):
                self.released = True
                self._entered = False
            raise
        self.released = True
        self._entered = False
        return True

    def close(self):
        return self.release()

    def _omnivm_close(self):
        return self.release()

    def status(self):
        return buffer_status(self.name)


def buffer_owner(name, data=_UNSET, dtype=0):
    """
    Return a context object that owns and releases a named shared buffer.

    If data is provided, the buffer is published on entry. The owner releases
    the public name on exit; active borrowed views remain valid until their own
    borrow finalizers run, and release failures keep native-memory diagnostics.
    """
    return BufferOwner(name, data, dtype)


def _buffer_status_details(name):
    try:
        return buffer_status(name)
    except Exception:
        return None


def _buffer_status_summary(status):
    if status is None:
        return ""
    if not isinstance(status, dict):
        return ""
    fields = []
    for key in (
        "state",
        "lease_state",
        "memory_space",
        "live",
        "released",
        "active_borrows",
        "active_borrowed_bytes",
        "active_named_borrows",
        "named_borrow_queue",
        "detached_buffers",
        "detached_bytes",
        "release_error",
    ):
        if key in status:
            fields.append(f"{key}={status[key]!r}")
    return ", ".join(fields)


def _buffer_status_is_released(status):
    if not isinstance(status, dict):
        return False
    return bool(status.get("released")) or status.get("state") in {
        "released",
        "released_detached",
    }


def buffer_status(name):
    """
    Return lifecycle diagnostics for a named shared OmniVM buffer.
    """
    if _lib is None:
        raise RuntimeError("omnivm not initialized - call init_runtimes() first")
    if not hasattr(_lib, "OmniBufStatus"):
        raise RuntimeError("libomnivm does not expose OmniBufStatus")
    raw = _check_result(_lib.OmniBufStatus(str(name).encode("utf-8")))
    return json.loads(raw)


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
    try:
        if _lib is None or not hasattr(_lib, "OmniHandleReleaseFromFinalizer"):
            return False
        return _lib.OmniHandleReleaseFromFinalizer(int(handle_id)) == 0
    except BaseException:
        return False


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
    try:
        if _lib is None or not hasattr(_lib, "OmniHandleDropReference"):
            return False
        _lib.OmniHandleDropReference(int(from_id), int(to_id))
        return True
    except BaseException:
        return False


def _drain_finalizer_releases(max_releases=0):
    try:
        if _lib is None or not hasattr(_lib, "OmniDrainFinalizerReleases"):
            return False
        return _lib.OmniDrainFinalizerReleases(int(max_releases)) == 0
    except BaseException:
        return False


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
        lib = _lib
        _lib = None
        lib.OmniShutdown()


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
