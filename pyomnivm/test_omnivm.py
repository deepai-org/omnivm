"""Tests for the omnivm Python package.

These test the pure-Python layer (ctypes setup, error handling, metrics)
without requiring libomnivm.so to be present.
"""

import builtins
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
        mock_lib.OmniCall.return_value = b"42"
        omnivm_mod._lib = mock_lib

        result = omnivm_mod.call("go", "6 * 7")
        assert result == "42"
        assert omnivm_mod.last_call_duration_ns() > 0
        assert omnivm_mod.thread_local_total_ns() > 0

    def test_call_accumulates(self):
        mock_lib = MagicMock()
        mock_lib.OmniCall.return_value = b"ok"
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
        self.mock_lib.OmniCall.return_value = b"result"
        omnivm_mod.call("javascript", "Math.sqrt(4)")
        self.mock_lib.OmniCall.assert_called_once_with(
            b"javascript", b"Math.sqrt(4)"
        )

    def test_execute_encodes_args(self):
        self.mock_lib.OmniExec.return_value = b"output"
        result = omnivm_mod.execute("python", "print('hi')")
        assert result == "output"
        self.mock_lib.OmniExec.assert_called_once_with(
            b"python", b"print('hi')"
        )

    def test_load_plugin_encodes_args(self):
        self.mock_lib.OmniLoadPlugin.return_value = b"ok"
        omnivm_mod.load_plugin("go", "/path/to/plugin.so")
        self.mock_lib.OmniLoadPlugin.assert_called_once_with(
            b"go", b"/path/to/plugin.so"
        )

    def test_call_error_propagation(self):
        self.mock_lib.OmniCall.return_value = b"ERR:go panic: index out of range"
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.call("go", "bad code")
        assert "index out of range" in str(ctx.exception)
        assert ctx.exception.runtime == "go"

    def test_execute_null_result(self):
        self.mock_lib.OmniExec.return_value = None
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.execute("go", "code")


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
