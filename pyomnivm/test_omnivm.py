"""Tests for the omnivm Python package.

These test the pure-Python layer (ctypes setup, error handling, metrics)
without requiring libomnivm.so to be present.
"""

import builtins
import ctypes
import gc
import json
import threading
import unittest
from unittest.mock import MagicMock, patch

# Import the module under test
import omnivm as omnivm_mod


class TestRuntimeError(unittest.TestCase):
    def test_basic(self):
        err = omnivm_mod.RuntimeError("boom")
        assert str(err) == "boom"
        assert err.runtime is None

    def test_with_runtime(self):
        err = omnivm_mod.RuntimeError("fail", runtime="python")
        assert err.runtime == "python"

    def test_is_builtin_runtime_error(self):
        err = omnivm_mod.RuntimeError("x")
        assert isinstance(err, builtins.RuntimeError)


class TestCheckResult(unittest.TestCase):
    def test_none_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod._check_result(None, runtime="go")
        assert "NULL" in str(ctx.exception)
        assert ctx.exception.runtime == "go"

    def test_err_prefix(self):
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod._check_result(b"ERR:something went wrong")
        assert str(ctx.exception) == "something went wrong"

    def test_success_bytes(self):
        result = omnivm_mod._check_result(b"hello")
        assert result == "hello"

    def test_success_str(self):
        result = omnivm_mod._check_result("hello")
        assert result == "hello"

    def test_err_prefix_str(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod._check_result("ERR:bad")

    def test_ok_prefix_strips_transport_marker(self):
        result = omnivm_mod._check_result("OK:ERR: plain value")
        assert result == "ERR: plain value"


class TestFindLibomnivm(unittest.TestCase):
    @patch.dict("os.environ", {"OMNIVM_LIB": "/fake/libomnivm.so"})
    @patch("os.path.isfile", return_value=True)
    def test_env_var(self, _):
        assert omnivm_mod._find_libomnivm() == "/fake/libomnivm.so"

    @patch.dict("os.environ", {}, clear=True)
    @patch("os.path.isfile", return_value=False)
    @patch("ctypes.util.find_library", return_value=None)
    def test_not_found(self, *_):
        assert omnivm_mod._find_libomnivm() is None


class TestNotInitialized(unittest.TestCase):
    def setUp(self):
        # Ensure _lib is None
        omnivm_mod._lib = None

    def test_call_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.call("go", "1+1")
        assert "init_runtimes" in str(ctx.exception)

    def test_execute_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.execute("go", "fmt.Println()")

    def test_load_plugin_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.load_plugin("go", "/fake.so")

    def test_set_task_timeout_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.set_task_timeout(100)

    def test_host_thread_id_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.host_thread_id()

    def test_watchdog_capabilities_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.watchdog_capabilities()

    def test_worker_tainted_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.worker_tainted()

    def test_last_timeout_runtime_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.last_timeout_runtime()

    def test_worker_taint_reason_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.worker_taint_reason()

    def test_status_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.status()


class TestLoadLib(unittest.TestCase):
    def setUp(self):
        omnivm_mod._lib = None

    def tearDown(self):
        omnivm_mod._lib = None

    @patch.object(omnivm_mod, "_find_libomnivm", return_value=None)
    def test_not_found_raises(self, _):
        with self.assertRaises(FileNotFoundError):
            omnivm_mod._load_lib()

    def test_cached(self):
        sentinel = object()
        omnivm_mod._lib = sentinel
        assert omnivm_mod._load_lib() is sentinel
        omnivm_mod._lib = None


class TestThreadLocalMetrics(unittest.TestCase):
    def test_defaults(self):
        # Use a fresh thread to get clean thread-local state
        results = {}

        def worker():
            results["last"] = omnivm_mod.last_call_duration_ns()
            results["total_ns"] = omnivm_mod.thread_local_total_ns()
            results["total_ms"] = omnivm_mod.thread_local_total_ms()

        t = threading.Thread(target=worker)
        t.start()
        t.join()
        assert results["last"] == 0
        assert results["total_ns"] == 0
        assert results["total_ms"] == 0.0

    def test_reset(self):
        omnivm_mod._local.last_call_duration_ns = 1000
        omnivm_mod._local.total_call_duration_ns = 5000
        omnivm_mod.thread_local_reset()
        assert omnivm_mod.last_call_duration_ns() == 0
        assert omnivm_mod.thread_local_total_ns() == 0


class TestCallMetrics(unittest.TestCase):
    """Test that call() tracks timing metrics."""

    def setUp(self):
        omnivm_mod._lib = None

    def tearDown(self):
        omnivm_mod._lib = None

    def test_call_updates_metrics(self):
        mock_lib = MagicMock()
        mock_lib.OmniCallHost.return_value = b"OK:42"
        omnivm_mod._lib = mock_lib

        result = omnivm_mod.call("go", "6 * 7")
        assert result == "42"
        assert omnivm_mod.last_call_duration_ns() > 0
        assert omnivm_mod.thread_local_total_ns() > 0

    def test_call_accumulates(self):
        mock_lib = MagicMock()
        mock_lib.OmniCallHost.return_value = b"OK:ok"
        omnivm_mod._lib = mock_lib

        omnivm_mod.thread_local_reset()
        omnivm_mod.call("go", "1")
        first = omnivm_mod.thread_local_total_ns()
        omnivm_mod.call("go", "2")
        second = omnivm_mod.thread_local_total_ns()
        assert second >= first


class TestCallWithMockLib(unittest.TestCase):
    def setUp(self):
        self.mock_lib = MagicMock()
        omnivm_mod._lib = self.mock_lib

    def tearDown(self):
        omnivm_mod._lib = None

    def test_call_encodes_args(self):
        self.mock_lib.OmniCallHost.return_value = b"OK:result"
        omnivm_mod.call("javascript", "Math.sqrt(4)")
        self.mock_lib.OmniCallHost.assert_called_once_with(
            b"javascript", b"Math.sqrt(4)"
        )

    def test_call_typed_rejects_implicit_complex_stringification(self):
        with self.assertRaises(TypeError) as ctx:
            omnivm_mod.call_typed("javascript", "use", args=({"path": "/orders"},))
        assert "implicit stringification" in str(ctx.exception)
        self.mock_lib.OmniCallTyped.assert_not_called()

    def test_execute_encodes_args(self):
        self.mock_lib.OmniExecHost.return_value = b"OK:output"
        result = omnivm_mod.execute("python", "print('hi')")
        assert result == "output"
        self.mock_lib.OmniExecHost.assert_called_once_with(
            b"python", b"print('hi')"
        )

    def test_load_plugin_encodes_args(self):
        self.mock_lib.OmniLoadPlugin.return_value = b"ok"
        omnivm_mod.load_plugin("go", "/path/to/plugin.so")
        self.mock_lib.OmniLoadPlugin.assert_called_once_with(
            b"go", b"/path/to/plugin.so"
        )

    def test_call_error_propagation(self):
        self.mock_lib.OmniCallHost.return_value = b"ERR:go panic: index out of range"
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.call("go", "bad code")
        assert "index out of range" in str(ctx.exception)
        assert ctx.exception.runtime == "go"

    def test_execute_null_result(self):
        self.mock_lib.OmniExecHost.return_value = None
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.execute("go", "code")

    def test_run_manifest_calls_lib(self):
        self.mock_lib.OmniRunManifestFile.return_value = b"OK"
        result = omnivm_mod.run_manifest("/tmp/app.manifest.json")
        assert result == "OK"
        self.mock_lib.OmniRunManifestFile.assert_called_once_with(b"/tmp/app.manifest.json")

    def test_run_manifest_error_propagation(self):
        self.mock_lib.OmniRunManifestFile.return_value = b"ERR:execute manifest: bad op"
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.run_manifest("/tmp/bad.manifest.json")
        assert "bad op" in str(ctx.exception)

    def test_load_manifest_module_calls_lib(self):
        self.mock_lib.OmniLoadManifestModule.return_value = b"OK"
        result = omnivm_mod.load_manifest_module("demo", "/tmp/app.manifest.json")
        assert result == "OK"
        self.mock_lib.OmniLoadManifestModule.assert_called_once_with(
            b"demo", b"/tmp/app.manifest.json"
        )

    def test_manifest_call_decodes_return_envelope(self):
        self.mock_lib.OmniManifestCall.return_value = (
            b'OK:{"__omnivm_result__":true,"kind":"string","value":"ranked"}'
        )
        result = omnivm_mod.manifest_call("demo", "rank_user", args=("user-1",))
        assert result == "ranked"
        module_id, payload = self.mock_lib.OmniManifestCall.call_args.args
        assert module_id == b"demo"
        assert json.loads(payload.decode("utf-8")) == {
            "func": "rank_user",
            "args": ["user-1"],
        }

    def test_manifest_call_passes_complex_args_as_runtime_refs(self):
        request = object()
        if hasattr(builtins, "__omnivm_arg_refs"):
            delattr(builtins, "__omnivm_arg_refs")
        self.mock_lib.OmniExecHost.return_value = b"OK:"
        self.mock_lib.OmniManifestCall.return_value = (
            b'OK:{"__omnivm_result__":true,"kind":"string","value":"ok"}'
        )

        result = omnivm_mod.manifest_call("demo", "rank_user", args=(request,))

        assert result == "ok"
        _module_id, payload = self.mock_lib.OmniManifestCall.call_args.args
        arg = json.loads(payload.decode("utf-8"))["args"][0]
        assert arg["__omnivm_runtime_ref__"] is True
        assert arg["runtime"] == "python"
        assert arg["var"].startswith("__omnivm_arg_refs['py_")
        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()

    def test_set_buffer_calls_lib(self):
        self.mock_lib.OmniBufSet.return_value = 0
        omnivm_mod.set_buffer("payload", b"abc", 7)
        name, buf = self.mock_lib.OmniBufSet.call_args.args
        assert name == b"payload"
        assert buf.len == 3
        assert buf.dtype == 7
        assert buf.owned == 0

    def test_get_buffer_returns_borrowed_memoryview(self):
        backing = ctypes.create_string_buffer(b"abc")

        def fill_buffer(_name, out):
            buf = ctypes.cast(
                out,
                ctypes.POINTER(omnivm_mod._OmniBuffer),
            ).contents
            buf.data = ctypes.cast(backing, ctypes.c_void_p)
            buf.len = 3
            buf.dtype = 0
            buf.read_only = 0
            return 0

        self.mock_lib.OmniBufGet.side_effect = fill_buffer
        result = omnivm_mod.get_buffer("payload")
        assert isinstance(result, memoryview)
        assert result.readonly is False
        assert bytes(result) == b"abc"
        result[0] = ord("z")
        assert backing.raw[:3] == b"zbc"
        self.mock_lib.OmniBufRelease.assert_not_called()
        del result
        gc.collect()
        self.mock_lib.OmniBufRelease.assert_called_once_with(b"payload")

    def test_get_buffer_preserves_readonly_metadata(self):
        backing = ctypes.create_string_buffer(b"abc")

        def fill_buffer(_name, out):
            buf = ctypes.cast(
                out,
                ctypes.POINTER(omnivm_mod._OmniBuffer),
            ).contents
            buf.data = ctypes.cast(backing, ctypes.c_void_p)
            buf.len = 3
            buf.dtype = 0
            buf.read_only = 1
            return 0

        self.mock_lib.OmniBufGet.side_effect = fill_buffer
        result = omnivm_mod.get_buffer("payload")
        assert result.readonly is True
        assert bytes(result) == b"abc"
        with self.assertRaises(TypeError):
            result[0] = ord("z")
        del result
        gc.collect()
        self.mock_lib.OmniBufRelease.assert_called_once_with(b"payload")

    def test_release_buffer_calls_lib(self):
        omnivm_mod.release_buffer("payload")
        self.mock_lib.OmniBufRelease.assert_called_once_with(b"payload")

    def test_release_handle_calls_lib(self):
        self.mock_lib.OmniHandleRelease.return_value = 0
        assert omnivm_mod._release_handle(123) is True
        self.mock_lib.OmniHandleRelease.assert_called_once_with(123)

    def test_retain_handle_calls_lib(self):
        self.mock_lib.OmniHandleRetain.return_value = 0
        assert omnivm_mod._retain_handle(123) is True
        self.mock_lib.OmniHandleRetain.assert_called_once_with(123)

    def test_escape_handle_calls_lib(self):
        self.mock_lib.OmniHandleEscape.return_value = 0
        assert omnivm_mod._escape_handle(123) is True
        self.mock_lib.OmniHandleEscape.assert_called_once_with(123)

    def test_release_handle_from_finalizer_calls_lib(self):
        self.mock_lib.OmniHandleReleaseFromFinalizer.return_value = 0
        assert omnivm_mod._release_handle_from_finalizer(123) is True
        self.mock_lib.OmniHandleReleaseFromFinalizer.assert_called_once_with(123)

    def test_record_handle_access_returns_chatty_flag(self):
        self.mock_lib.OmniHandleAccess.return_value = 1
        assert omnivm_mod._record_handle_access(123, "index", 16) is True
        self.mock_lib.OmniHandleAccess.assert_called_once_with(123, b"index", 16)

    def test_record_handle_access_error_raises(self):
        self.mock_lib.OmniHandleAccess.return_value = -1
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod._record_handle_access(123, "index")

    def test_record_handle_reference_calls_lib(self):
        self.mock_lib.OmniHandleRecordReference.return_value = 0
        assert omnivm_mod._record_handle_reference(123, 456, "proxy") is True
        self.mock_lib.OmniHandleRecordReference.assert_called_once_with(123, 456, b"proxy")

    def test_drop_handle_reference_calls_lib(self):
        omnivm_mod._drop_handle_reference(123, 456)
        self.mock_lib.OmniHandleDropReference.assert_called_once_with(123, 456)

    def test_drain_finalizer_releases_calls_lib(self):
        self.mock_lib.OmniDrainFinalizerReleases.return_value = 0
        assert omnivm_mod._drain_finalizer_releases(25) is True
        self.mock_lib.OmniDrainFinalizerReleases.assert_called_once_with(25)

    def test_set_task_timeout_calls_lib(self):
        omnivm_mod.set_task_timeout(250)
        self.mock_lib.OmniSetTaskTimeout.assert_called_once_with(250)

    def test_host_thread_id_returns_int(self):
        self.mock_lib.OmniHostThreadID.return_value = 12345
        assert omnivm_mod.host_thread_id() == 12345

    def test_watchdog_capabilities_parses_matrix(self):
        self.mock_lib.OmniWatchdogCapabilities.return_value = (
            b"python=host-interrupt,javascript=watchdog,ruby=watchdog,java=interrupt,go=deadline"
        )
        assert omnivm_mod.watchdog_capabilities() == {
            "python": "host-interrupt",
            "javascript": "watchdog",
            "ruby": "watchdog",
            "java": "interrupt",
            "go": "deadline",
        }

    def test_worker_tainted_returns_bool(self):
        self.mock_lib.OmniWorkerTainted.return_value = 1
        assert omnivm_mod.worker_tainted() is True
        self.mock_lib.OmniWorkerTainted.return_value = 0
        assert omnivm_mod.worker_tainted() is False

    def test_worker_taint_details(self):
        self.mock_lib.OmniLastTimeoutRuntime.return_value = b"go"
        self.mock_lib.OmniWorkerTaintReason.return_value = b"deadline"
        assert omnivm_mod.last_timeout_runtime() == "go"
        assert omnivm_mod.worker_taint_reason() == "deadline"

    def test_status_parses_json(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"initialized":true,"worker_tainted":true,"active_calls":0}'
        )
        assert omnivm_mod.status() == {
            "initialized": True,
            "worker_tainted": True,
            "active_calls": 0,
        }

    def test_clear_worker_taint_for_test_calls_lib(self):
        omnivm_mod._clear_worker_taint_for_test()
        self.mock_lib.OmniClearWorkerTaintForTest.assert_called_once_with()


class TestShutdown(unittest.TestCase):
    def test_shutdown_calls_lib(self):
        mock_lib = MagicMock()
        omnivm_mod._lib = mock_lib
        omnivm_mod.shutdown()
        mock_lib.OmniShutdown.assert_called_once()
        omnivm_mod._lib = None

    def test_shutdown_noop_when_not_loaded(self):
        omnivm_mod._lib = None
        omnivm_mod.shutdown()  # should not raise


class TestInitRuntimes(unittest.TestCase):
    def setUp(self):
        omnivm_mod._lib = None

    def tearDown(self):
        omnivm_mod._lib = None

    @patch.object(omnivm_mod, "_load_lib")
    def test_init_joins_runtimes(self, mock_load):
        mock_lib = MagicMock()
        mock_lib.OmniInit.return_value = b"ok"
        mock_load.return_value = mock_lib

        omnivm_mod.init_runtimes(["go", "javascript", "ruby"])
        mock_lib.OmniInit.assert_called_once_with(b"go,javascript,ruby")

    @patch.object(omnivm_mod, "_load_lib")
    def test_init_error(self, mock_load):
        mock_lib = MagicMock()
        mock_lib.OmniInit.return_value = b"ERR:failed to init go"
        mock_load.return_value = mock_lib

        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.init_runtimes(["go"])


if __name__ == "__main__":
    unittest.main()
