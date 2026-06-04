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
        assert err.type == ""
        assert err.message == "fail"
        assert err.traceback == ""
        assert err.cause_chain == []

    def test_is_builtin_runtime_error(self):
        err = omnivm_mod.RuntimeError("x")
        assert isinstance(err, builtins.RuntimeError)

    def test_parses_prefixed_type_message_and_traceback(self):
        err = omnivm_mod.RuntimeError(
            "javascript: ZodError: invalid input\n    at <anonymous>:1:1",
            runtime="javascript",
        )
        assert str(err) == "javascript: ZodError: invalid input\n    at <anonymous>:1:1"
        assert err.runtime == "javascript"
        assert err.type == "ZodError"
        assert err.message == "invalid input"
        assert "anonymous" in err.traceback
        assert err.stack_frames == ["at <anonymous>:1:1"]

    def test_parses_java_cause_chain(self):
        err = omnivm_mod.RuntimeError(
            "jvm: java.lang.RuntimeException: outer\n"
            "\tat OmniVMEval.run(OmniVMEval.java:3)\n"
            "Caused by: java.lang.IllegalArgumentException: inner\n"
            "\t... 6 more",
            runtime="java",
        )
        assert err.runtime == "java"
        assert err.type == "java.lang.RuntimeException"
        assert err.message == "outer"
        assert err.cause_chain == [
            {"type": "java.lang.IllegalArgumentException", "message": "inner"}
        ]
        assert err.stack_frames == [
            "at OmniVMEval.run(OmniVMEval.java:3)",
            "... 6 more",
        ]

    def test_parses_javascript_error_cause_chain(self):
        err = omnivm_mod.RuntimeError(
            "javascript: Error: outer\n"
            "    at <anonymous>:1:7\n"
            "Caused by: TypeError: inner\n"
            "    at <anonymous>:1:42",
            runtime="javascript",
        )
        assert err.runtime == "javascript"
        assert err.type == "Error"
        assert err.message == "outer"
        assert err.cause_chain == [
            {"type": "TypeError", "message": "inner"}
        ]

    def test_traceback_parser_ignores_metadata_lines(self):
        err = omnivm_mod.RuntimeError(
            "python: Traceback (most recent call last):\n"
            "  File \"<string>\", line 1, in <module>\n"
            "sqlalchemy.exc.IntegrityError: UNIQUE constraint failed: users.name\n"
            "[SQL: INSERT INTO users (name) VALUES (?)]\n"
            "[parameters: ('ada',)]\n"
            "Details: {\"errors\":[{\"loc\":[\"age\"],\"type\":\"greater_than\"}]}\n"
            "(Background on this error at: https://sqlalche.me/e/20/gkpj)\n",
            runtime="python",
        )
        assert err.runtime == "python"
        assert err.type == "IntegrityError"
        assert err.message == "UNIQUE constraint failed: users.name"
        assert "[parameters:" in err.traceback
        assert err.details == {"errors": [{"loc": ["age"], "type": "greater_than"}]}

    def test_details_preserves_non_object_json(self):
        array_err = omnivm_mod.RuntimeError(
            "javascript: AggregateError: invalid\n"
            "Details: [{\"path\":[\"user\",\"age\"],\"code\":\"too_small\"}]",
            runtime="javascript",
        )
        assert array_err.details == [{"path": ["user", "age"], "code": "too_small"}]

        scalar_err = omnivm_mod.RuntimeError(
            "go: ValidationError: invalid\n"
            "Details: \"too_small\"",
            runtime="go",
        )
        assert scalar_err.details == "too_small"

    def test_parses_structured_json_error_envelope(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "origin_runtime": "python",
                    "type": "AggregateError",
                    "message": "invalid",
                    "traceback": "fallback frame",
                    "stack_frames": ["at parse (<anonymous>:1:2)"],
                    "cause_chain": [
                        {
                            "runtime": "java",
                            "origin_runtime": "ruby",
                            "type": "TypeError",
                            "message": "inner",
                            "boundary_path": "call[javascript] > callback[java]",
                            "original_error_handle": "java-error-3",
                        }
                    ],
                    "boundary_path": "call[javascript] > callback[python]",
                    "original_error_handle": "js-error-7",
                    "details": [{"path": ["user", "age"]}],
                }
            ),
            runtime="go",
        )
        assert err.runtime == "javascript"
        assert err.origin_runtime == "python"
        assert err.type == "AggregateError"
        assert err.message == "invalid"
        assert err.stack_frames == ["at parse (<anonymous>:1:2)"]
        assert err.cause_chain == [
            {
                "type": "TypeError",
                "message": "inner",
                "runtime": "java",
                "origin_runtime": "ruby",
                "boundary_path": "call[javascript] > callback[java]",
                "original_error_handle": "java-error-3",
            }
        ]
        assert err.boundary_path == "call[javascript] > callback[python]"
        assert err.original_error_handle == "js-error-7"
        assert err.details == [{"path": ["user", "age"]}]
        assert err.to_dict()["origin_runtime"] == "python"

    def test_runtime_ref_assign_preserves_owner_runtime(self):
        err = omnivm_mod.RuntimeError(
            "runtime ref assign [python]: Traceback (most recent call last):\n"
            "  File \"<string>\", line 1, in <module>\n"
            "OSError: [Errno 9] Bad file descriptor\n"
            " (expr: (lambda __o, __args, __kwargs: __o(*__args, **__kwargs))(...))",
            runtime="__manifest",
            boundary_path="call[__manifest]",
        )
        assert err.runtime == "python"
        assert err.type == "OSError"
        assert err.message == "[Errno 9] Bad file descriptor"
        assert err.boundary_path == "call[python]"
        assert "Traceback" in err.traceback
        assert "(expr:" in err.traceback

    def test_parses_original_error_handle_marker(self):
        err = omnivm_mod.RuntimeError(
            "javascript: Error: outer\n"
            "    at <anonymous>:1:7\n"
            "Original error handle: js-error-42",
            runtime="javascript",
        )
        assert err.original_error_handle == "js-error-42"

    def test_to_dict_returns_structured_error_envelope(self):
        err = omnivm_mod.RuntimeError(
            "javascript: Error: outer\n"
            "    at <anonymous>:1:7\n"
            "Caused by: TypeError: inner",
            runtime="javascript",
            boundary_path="call[javascript]",
        )
        assert err.to_dict() == {
            "runtime": "javascript",
            "origin_runtime": "javascript",
            "type": "Error",
            "message": "outer",
            "traceback": "    at <anonymous>:1:7\nCaused by: TypeError: inner",
            "stack_frames": ["at <anonymous>:1:7"],
            "cause_chain": [{"type": "TypeError", "message": "inner"}],
            "boundary_path": "call[javascript]",
            "original_error_handle": None,
            "details": None,
        }

    def test_as_dict_alias_returns_structured_error_envelope(self):
        err = omnivm_mod.RuntimeError(
            "python: ValueError: bad\n"
            "Traceback (most recent call last):\n"
            "  File \"app.py\", line 1, in <module>\n"
            "Details: {\"field\":\"age\"}",
            runtime="python",
            boundary_path="call[python]",
        )
        assert err.as_dict() == err.to_dict()
        envelope = err.as_dict()
        envelope["details"]["field"] = "changed"
        assert err.details == {"field": "age"}

    def test_details_override_supplies_structured_guard_context(self):
        details = {"thread_affinity": {"owner_dispatch_supported": False}}
        err = omnivm_mod.RuntimeError(
            "owner dispatch unsupported",
            boundary_path="thread_affinity",
            details=details,
        )
        details["thread_affinity"]["owner_dispatch_supported"] = True

        assert err.details == {"thread_affinity": {"owner_dispatch_supported": False}}
        envelope = err.to_dict()
        envelope["details"]["thread_affinity"]["owner_dispatch_supported"] = True
        assert err.details == {"thread_affinity": {"owner_dispatch_supported": False}}

    def test_to_dict_copies_mutable_envelope_values(self):
        err = omnivm_mod.RuntimeError(
            "javascript: Error: outer\n"
            "    at <anonymous>:1:7\n"
            "Caused by: TypeError: inner\n"
            "Details: {\"items\":[{\"path\":\"user.age\"}]}",
            runtime="javascript",
        )
        envelope = err.to_dict()
        envelope["stack_frames"][0] = "changed"
        envelope["cause_chain"][0]["message"] = "changed"
        envelope["details"]["items"][0]["path"] = "changed"

        assert err.stack_frames == ["at <anonymous>:1:7"]
        assert err.cause_chain == [{"type": "TypeError", "message": "inner"}]
        assert err.details == {"items": [{"path": "user.age"}]}

    def test_parses_go_wrapped_error_cause_chain(self):
        err = omnivm_mod.RuntimeError(
            "go: outer layer: inner layer\n"
            "Caused by: inner layer",
            runtime="go",
        )
        assert err.runtime == "go"
        assert err.type == ""
        assert err.message == "outer layer: inner layer"
        assert err.cause_chain == [
            {"type": "", "message": "inner layer"}
        ]

    def test_parses_manifest_runtime_error_boundary_path(self):
        err = omnivm_mod.RuntimeError(
            "execute manifest: exec [python]: python: ValidationError: bad input\n"
            "Traceback (most recent call last):\n"
            "  File \"<string>\", line 1, in <module>"
        )
        assert err.runtime == "python"
        assert err.type == "ValidationError"
        assert err.message == "bad input"
        assert err.boundary_path == "execute manifest > exec[python]"
        assert "Traceback" in err.traceback

    def test_uses_boundary_path_fallback_for_direct_calls(self):
        err = omnivm_mod.RuntimeError(
            "javascript: TypeError: bad input",
            runtime="javascript",
            boundary_path="call[javascript]",
        )
        assert err.runtime == "javascript"
        assert err.type == "TypeError"
        assert err.message == "bad input"
        assert err.boundary_path == "call[javascript]"

    def test_nested_runtime_prefix_preserves_rethrown_source_runtime(self):
        err = omnivm_mod.RuntimeError(
            "javascript: python: ZeroDivisionError: division by zero",
            runtime="javascript",
            boundary_path="call[javascript]",
        )
        assert err.runtime == "python"
        assert err.type == "ZeroDivisionError"
        assert err.message == "division by zero"
        assert err.boundary_path == "call[python]"

    def test_parsed_boundary_path_overrides_fallback(self):
        err = omnivm_mod.RuntimeError(
            "execute manifest: exec [python]: python: ValidationError: bad input",
            runtime="javascript",
            boundary_path="call[javascript]",
        )
        assert err.runtime == "python"
        assert err.boundary_path == "execute manifest > exec[python]"

    def test_parses_manifest_python_traceback_tail(self):
        err = omnivm_mod.RuntimeError(
            "execute manifest: exec [python]: Traceback (most recent call last):\n"
            "  File \"<string>\", line 5, in <module>\n"
            "pydantic_core._pydantic_core.ValidationError: 2 validation errors for User\n"
            "age\n"
            "  Input should be greater than 0"
        )
        assert err.runtime == "python"
        assert err.type == "ValidationError"
        assert err.message == "2 validation errors for User"
        assert err.boundary_path == "execute manifest > exec[python]"
        assert "Traceback" in err.traceback
        assert "File \"<string>\", line 5" in err.traceback


class TestCheckResult(unittest.TestCase):
    def tearDown(self):
        omnivm_mod._lib = None

    def test_none_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod._check_result(None, runtime="go")
        assert "NULL" in str(ctx.exception)
        assert ctx.exception.runtime == "go"

    def test_err_prefix(self):
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod._check_result(b"ERR:something went wrong")
        assert str(ctx.exception) == "something went wrong"
        assert ctx.exception.message == "something went wrong"

    def test_err_prefix_sets_boundary_path(self):
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod._check_result(
                b"ERR:javascript: TypeError: bad",
                runtime="javascript",
                boundary_path="call[javascript]",
            )
        assert ctx.exception.runtime == "javascript"
        assert ctx.exception.type == "TypeError"
        assert ctx.exception.message == "bad"
        assert ctx.exception.boundary_path == "call[javascript]"

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

    def test_raw_c_string_result_is_freed_after_decode(self):
        backing = ctypes.create_string_buffer(b"OK:payload")
        free_mock = MagicMock()
        omnivm_mod._lib = type("Lib", (), {"OmniFree": free_mock})()

        result = omnivm_mod._check_result(ctypes.cast(backing, ctypes.c_void_p))

        assert result == "payload"
        free_mock.assert_called_once_with(ctypes.addressof(backing))

    def test_raw_c_string_error_is_freed_before_raise(self):
        backing = ctypes.create_string_buffer(b"ERR:javascript: TypeError: bad")
        free_mock = MagicMock()
        omnivm_mod._lib = type("Lib", (), {"OmniFree": free_mock})()

        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod._check_result(
                ctypes.addressof(backing),
                runtime="javascript",
                boundary_path="call[javascript]",
            )

        assert ctx.exception.type == "TypeError"
        free_mock.assert_called_once_with(ctypes.addressof(backing))


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
        omnivm_mod._worker_drain_hook_installed = False

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

    def test_affinity_status_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.affinity_status()

    def test_owner_dispatch_status_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.owner_dispatch_status()

    def test_assert_owner_dispatch_supported_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.assert_owner_dispatch_supported()

    def test_ruby_threading_status_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.ruby_threading_status()

    def test_assert_ruby_native_threads_supported_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.assert_ruby_native_threads_supported()

    def test_assert_host_thread_raises(self):
        with self.assertRaises(omnivm_mod.RuntimeError):
            omnivm_mod.assert_host_thread()

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

    def test_drain_worker_hook_noops(self):
        assert omnivm_mod.drain_worker_hook(object(), worker=object()) is None

    @patch.object(omnivm_mod.atexit, "register")
    def test_install_worker_drain_hook_registers_once(self, register):
        assert omnivm_mod.install_worker_drain_hook() is omnivm_mod.drain_worker_hook
        assert omnivm_mod.install_worker_drain_hook() is omnivm_mod.drain_worker_hook
        register.assert_called_once_with(omnivm_mod.drain_worker_hook)


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
        omnivm_mod._worker_drain_hook_installed = False

    def tearDown(self):
        omnivm_mod._lib = None
        omnivm_mod._worker_drain_hook_installed = False

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

    def test_unload_manifest_modules_calls_lib(self):
        self.mock_lib.OmniUnloadManifestModules.return_value = b"OK"
        result = omnivm_mod.unload_manifest_modules()
        assert result == "OK"
        self.mock_lib.OmniUnloadManifestModules.assert_called_once_with()

    def test_unload_manifest_modules_error_propagation(self):
        self.mock_lib.OmniUnloadManifestModules.return_value = b"ERR:unload manifest modules: busy"
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.unload_manifest_modules()
        assert "busy" in str(ctx.exception)

    def test_drain_worker_calls_lib(self):
        self.mock_lib.OmniDrainWorker.return_value = b"OK"
        result = omnivm_mod.drain_worker()
        assert result == "OK"
        self.mock_lib.OmniDrainWorker.assert_called_once_with()

    def test_drain_worker_error_boundary_path(self):
        self.mock_lib.OmniDrainWorker.return_value = b"ERR:drain worker: busy"
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.drain_worker()
        assert "busy" in str(ctx.exception)
        assert ctx.exception.boundary_path == "drain_worker"

    def test_drain_worker_hook_accepts_app_server_args(self):
        self.mock_lib.OmniDrainWorker.return_value = b"OK"
        result = omnivm_mod.drain_worker_hook(object(), object(), reason="reload")
        assert result == "OK"
        self.mock_lib.OmniDrainWorker.assert_called_once_with()

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

    def test_manifest_call_keeps_returned_complex_arg_refs_until_proxy_close(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        request = object()
        requests = []
        if hasattr(builtins, "__omnivm_arg_refs"):
            delattr(builtins, "__omnivm_arg_refs")
        self.mock_lib.OmniExecHost.return_value = b"OK:"

        def manifest_call(_module_id, payload):
            request_payload = json.loads(payload.decode("utf-8"))
            requests.append(request_payload)
            if request_payload.get("func") == "echo":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 7,
                    "runtime": "python",
                    "kind": "request",
                    "transfer": True,
                })
            if request_payload.get("op") in {"handle_adopt", "handle_release_explicit"}:
                return envelope(True, "bool")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        proxy = omnivm_mod.manifest_call("demo", "echo", args=(request,))

        refs = getattr(builtins, "__omnivm_arg_refs", {})
        assert request in refs.values()
        proxy.close()
        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        assert requests[0]["args"][0]["var"].startswith("__omnivm_arg_refs['py_")
        assert {"op": "handle_adopt", "id": 7} in requests
        assert {"op": "handle_release_explicit", "id": 7} in requests

    def test_manifest_call_wraps_complex_return_proxy(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            requests.append(request)
            if request.get("func") == "request":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 42,
                    "runtime": "python",
                    "kind": "request",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "handle_get" and request.get("key") == "path":
                return envelope("/orders")
            if request.get("op") == "handle_get" and request.get("key") == "items":
                return envelope({"__omnivm_callable__": True, "key": "items"})
            if request.get("op") == "handle_call" and request.get("key") == "items":
                return envelope(["a", "b"])
            if request.get("op") == "handle_release_explicit":
                return envelope(True, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        proxy = omnivm_mod.manifest_call("demo", "request")

        assert isinstance(proxy, omnivm_mod.ManifestProxy)
        assert proxy.path == "/orders"
        assert proxy.items("open", limit=2) == ["a", "b"]
        proxy.close()
        assert requests[0] == {"func": "request", "args": []}
        assert requests[1] == {"op": "handle_adopt", "id": 42}
        assert requests[4] == {
            "op": "handle_call",
            "id": 42,
            "key": "items",
            "args": ["open"],
            "kwargs": {"limit": 2},
        }
        assert requests[-1] == {"op": "handle_release_explicit", "id": 42}

    def test_manifest_proxy_helpers_bypass_local_method_collisions(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []
        fields = {"close": "field-close", "length": 3}

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            requests.append(request)
            if request.get("func") == "tool":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 43,
                    "runtime": "python",
                    "kind": "object",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "handle_get":
                return envelope(fields[request["key"]])
            if request.get("op") == "handle_set":
                fields[request["key"]] = request["value"]
                return envelope(True, "bool")
            if request.get("op") == "handle_call" and request.get("key") == "accept":
                return envelope("accepted-" + request["args"][0])
            if request.get("op") == "handle_len":
                return envelope(7, "number")
            if request.get("op") == "handle_iter" and request.get("mode") == "keys":
                return envelope(["close", "length"])
            if request.get("op") == "handle_iter" and request.get("mode") == "items":
                return envelope([["close", fields["close"]], ["length", fields["length"]]])
            if request.get("op") == "handle_iter" and request.get("mode") == "values":
                return envelope([fields["close"], fields["length"]])
            if request.get("op") == "handle_contains":
                return envelope(request["value"] in fields, "bool")
            if request.get("op") == "handle_release_explicit":
                return envelope(True, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        proxy = omnivm_mod.manifest_call("demo", "tool")

        assert callable(proxy.close)
        assert omnivm_mod.proxy_get(proxy, "close") == "field-close"
        assert omnivm_mod.proxy_set(proxy, "close", "field-updated") is True
        assert omnivm_mod.proxy_get(proxy, "close") == "field-updated"
        assert omnivm_mod.proxy_call(proxy, "accept", args=("js",)) == "accepted-js"
        assert omnivm_mod.proxy_len(proxy) == 7
        assert omnivm_mod.proxy_keys(proxy) == ["close", "length"]
        assert omnivm_mod.proxy_items(proxy) == [["close", "field-updated"], ["length", 3]]
        assert omnivm_mod.proxy_values(proxy) == ["field-updated", 3]
        assert omnivm_mod.proxy_contains(proxy, "close") is True
        assert omnivm_mod.proxy_contains(proxy, "missing") is False
        assert omnivm_mod.proxy_close(proxy) is True
        release_count = sum(1 for request in requests if request.get("op") == "handle_release_explicit")
        assert omnivm_mod.proxy_close(proxy) is False
        assert sum(1 for request in requests if request.get("op") == "handle_release_explicit") == release_count
        assert requests[-1] == {"op": "handle_release_explicit", "id": 43}

    def test_manifest_proxy_close_propagates_explicit_release_failure(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            if request.get("func") == "tool":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 44,
                    "runtime": "python",
                    "kind": "object",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "handle_release_explicit":
                raise RuntimeError("release failed")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "tool")

        with self.assertRaisesRegex(RuntimeError, "release failed"):
            proxy.close()
        with self.assertRaisesRegex(RuntimeError, "release failed"):
            proxy.close()

    def test_manifest_proxy_context_preserves_body_exception_when_close_fails(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            if request.get("func") == "tool":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 47,
                    "runtime": "python",
                    "kind": "object",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "handle_release_explicit":
                raise RuntimeError("release failed")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "tool")

        with self.assertRaisesRegex(ValueError, "body failed") as ctx:
            with proxy:
                raise ValueError("body failed")
        notes = getattr(ctx.exception, "__notes__", [])
        assert any("release failed" in note for note in notes)

    def test_manifest_stream_proxy_close_cancels_stream_once(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            requests.append(request)
            if request.get("func") == "rows":
                return envelope({
                    "__omnivm_stream__": True,
                    "id": 45,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "stream_cancel":
                return envelope(True, "bool")
            if request.get("op") == "handle_release_finalizer":
                raise AssertionError("stream close should cancel, not release as finalizer")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows")

        assert omnivm_mod.proxy_close(proxy) is True
        assert omnivm_mod.proxy_close(proxy) is False
        assert requests[-1] == {"op": "stream_cancel", "id": 45}
        assert sum(1 for request in requests if request.get("op") == "stream_cancel") == 1

    def test_manifest_stream_iterator_detaches_on_eof(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        request = object()
        requests = []
        if hasattr(builtins, "__omnivm_arg_refs"):
            delattr(builtins, "__omnivm_arg_refs")
        self.mock_lib.OmniExecHost.return_value = b"OK:"

        def manifest_call(_module_id, payload):
            request_payload = json.loads(payload.decode("utf-8"))
            requests.append(request_payload)
            if request_payload.get("func") == "rows":
                return envelope({
                    "__omnivm_stream__": True,
                    "id": 46,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request_payload.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request_payload.get("op") == "stream_next":
                return envelope({"done": True})
            if request_payload.get("op") in {"stream_cancel", "handle_release_finalizer"}:
                raise AssertionError("exhausted stream should not need later cleanup")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows", args=(request,))

        iterator = iter(proxy)
        with self.assertRaises(StopIteration):
            next(iterator)
        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        assert proxy.close() is False

    def test_manifest_stream_iterator_context_preserves_body_exception_when_close_fails(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        def manifest_call(_module_id, payload):
            request_payload = json.loads(payload.decode("utf-8"))
            if request_payload.get("func") == "rows":
                return envelope({
                    "__omnivm_stream__": True,
                    "id": 48,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request_payload.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request_payload.get("op") == "stream_cancel":
                raise RuntimeError("cancel failed")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows")

        with self.assertRaisesRegex(ValueError, "body failed") as ctx:
            with iter(proxy):
                raise ValueError("body failed")
        notes = getattr(ctx.exception, "__notes__", [])
        assert any("cancel failed" in note for note in notes)

    def test_manifest_call_wraps_nested_complex_return_proxies(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []

        def descriptor(handle_id, transfer=True):
            return {
                "__omnivm_resource__": True,
                "id": handle_id,
                "runtime": "python",
                "kind": "object",
                "transfer": transfer,
            }

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            requests.append(request)
            if request.get("func") == "nested":
                return envelope({
                    "items": [descriptor(7)],
                    "meta": {"primary": descriptor(8, transfer=False)},
                })
            if request.get("op") in {"handle_adopt", "handle_retain", "handle_release_explicit"}:
                return envelope(True, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        result = omnivm_mod.manifest_call("demo", "nested")

        child = result["items"][0]
        primary = result["meta"]["primary"]
        assert isinstance(child, omnivm_mod.ManifestProxy)
        assert isinstance(primary, omnivm_mod.ManifestProxy)
        assert child.__omnivm_handle_id__ == 7
        assert primary.__omnivm_handle_id__ == 8
        child.close()
        primary.close()
        assert {"op": "handle_adopt", "id": 7} in requests
        assert {"op": "handle_retain", "id": 8} in requests
        assert {"op": "handle_release_explicit", "id": 7} in requests
        assert {"op": "handle_release_explicit", "id": 8} in requests

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

    def test_get_buffer_returns_empty_memoryview_for_empty_buffer(self):
        def fill_buffer(_name, out):
            buf = ctypes.cast(
                out,
                ctypes.POINTER(omnivm_mod._OmniBuffer),
            ).contents
            buf.data = None
            buf.len = 0
            buf.dtype = 0
            buf.read_only = 0
            return 0

        self.mock_lib.OmniBufGet.side_effect = fill_buffer
        result = omnivm_mod.get_buffer("empty")
        assert isinstance(result, memoryview)
        assert len(result) == 0
        assert result.readonly is False
        self.mock_lib.OmniBufRelease.assert_called_once_with(b"empty")

    def test_get_buffer_preserves_readonly_for_empty_buffer(self):
        def fill_buffer(_name, out):
            buf = ctypes.cast(
                out,
                ctypes.POINTER(omnivm_mod._OmniBuffer),
            ).contents
            buf.data = None
            buf.len = 0
            buf.dtype = 0
            buf.read_only = 1
            return 0

        self.mock_lib.OmniBufGet.side_effect = fill_buffer
        result = omnivm_mod.get_buffer("empty")
        assert isinstance(result, memoryview)
        assert len(result) == 0
        assert result.readonly is True
        self.mock_lib.OmniBufRelease.assert_called_once_with(b"empty")

    def test_release_buffer_prefers_explicit_free(self):
        self.mock_lib.OmniBufFree.return_value = 0
        omnivm_mod.release_buffer("payload")
        self.mock_lib.OmniBufFree.assert_called_once_with(b"payload")
        self.mock_lib.OmniBufRelease.assert_not_called()

    def test_release_buffer_falls_back_to_release_for_old_libs(self):
        class OldLib:
            def __init__(self):
                self.OmniBufRelease = MagicMock()

        old_lib = OldLib()
        omnivm_mod._lib = old_lib
        omnivm_mod.release_buffer("payload")
        old_lib.OmniBufRelease.assert_called_once_with(b"payload")

    def test_release_buffer_reports_free_failure(self):
        self.mock_lib.OmniBufFree.return_value = -1
        self.mock_lib.OmniBufStatus.return_value = (
            b'{"name":"payload","state":"released_detached","live":false,'
            b'"lease_state":"detached","released":true,"active_borrows":2,'
            b'"active_borrowed_bytes":6,"detached_buffers":1}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.release_buffer("payload")
        assert "release_buffer failed" in str(ctx.exception)
        assert "state='released_detached'" in str(ctx.exception)
        assert "lease_state='detached'" in str(ctx.exception)
        assert "active_borrows=2" in str(ctx.exception)
        assert "detached_buffers=1" in str(ctx.exception)
        assert ctx.exception.boundary_path == "native_memory"
        assert ctx.exception.details["buffer"]["state"] == "released_detached"
        assert ctx.exception.details["buffer"]["lease_state"] == "detached"
        assert ctx.exception.details["buffer"]["active_borrows"] == 2
        self.mock_lib.OmniBufStatus.assert_called_once_with(b"payload")

    def test_buffer_status_returns_lifecycle_diagnostics(self):
        self.mock_lib.OmniBufStatus.return_value = (
            b'{"name":"payload","state":"released_detached","live":false,'
            b'"lease_state":"detached","released":true,'
            b'"active_borrows":1,"detached_buffers":1}'
        )
        status = omnivm_mod.buffer_status("payload")
        self.mock_lib.OmniBufStatus.assert_called_once_with(b"payload")
        assert status["state"] == "released_detached"
        assert status["lease_state"] == "detached"
        assert status["released"] is True
        assert status["active_borrows"] == 1
        assert status["detached_buffers"] == 1

    def test_buffer_status_requires_capability(self):
        class OldLib:
            pass

        omnivm_mod._lib = OldLib()
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.buffer_status("payload")
        assert "OmniBufStatus" in str(ctx.exception)

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

    @patch.object(omnivm_mod.threading, "get_native_id", return_value=12345)
    def test_affinity_status_reports_host_thread(self, _):
        self.mock_lib.OmniHostThreadID.return_value = 12345
        info = omnivm_mod.affinity_status()
        assert info["host_thread_id"] == 12345
        assert info["current_thread_id"] == 12345
        assert info["on_host_thread"] is True
        assert info["thread_name"]
        assert info["asyncio"]["running"] is False

    @patch.object(omnivm_mod.threading, "get_native_id", return_value=12345)
    def test_assert_host_thread_accepts_owner_thread(self, _):
        self.mock_lib.OmniHostThreadID.return_value = 12345
        assert omnivm_mod.assert_host_thread("startup") is True

    @patch.object(omnivm_mod.threading, "get_native_id", return_value=99)
    def test_assert_host_thread_reports_violation(self, _):
        self.mock_lib.OmniHostThreadID.return_value = 12345
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.assert_host_thread("callback")
        assert "callback: thread affinity violation" in str(ctx.exception)
        assert ctx.exception.boundary_path == "thread_affinity"
        assert ctx.exception.details["affinity"]["host_thread_id"] == 12345
        assert ctx.exception.details["affinity"]["current_thread_id"] == 99

    def test_owner_dispatch_status_reports_capability_contract(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"host_thread_id":12345,"owner_dispatch_supported":false,'
            b'"reason":"c-shared mode exposes diagnostics only",'
            b'"owner_dispatch_targets":{"python_asyncio":{"supported":false},'
            b'"javascript_event_loop":{"supported":false},'
            b'"java_executor":{"supported":false},'
            b'"ruby_fiber_thread":{"supported":false}},'
            b'"python_assert_host_thread":true}}'
        )
        info = omnivm_mod.owner_dispatch_status()
        assert info["mode"] == "diagnostic_only"
        assert info["host_thread_id"] == 12345
        assert info["owner_dispatch_supported"] is False
        assert "diagnostics only" in info["reason"]
        assert info["python_assert_host_thread"] is True
        assert info["owner_dispatch_targets"]["python_asyncio"]["supported"] is False
        assert info["owner_dispatch_targets"]["javascript_event_loop"]["supported"] is False
        assert info["owner_dispatch_targets"]["java_executor"]["supported"] is False
        assert info["owner_dispatch_targets"]["ruby_fiber_thread"]["supported"] is False

    def test_owner_dispatch_status_requires_status_capability(self):
        self.mock_lib.OmniStatus.return_value = b'{"initialized":true}'
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.owner_dispatch_status()
        assert ctx.exception.boundary_path == "thread_affinity"

    def test_assert_owner_dispatch_supported_reports_diagnostic(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"reason":"c-shared mode runs direct calls on the calling host thread"}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.assert_owner_dispatch_supported("django startup")
        assert "django startup: owner dispatch unsupported" in str(ctx.exception)
        assert "diagnostic_only" in str(ctx.exception)
        assert ctx.exception.boundary_path == "thread_affinity"
        assert ctx.exception.details["thread_affinity"]["owner_dispatch_supported"] is False
        assert "c-shared mode" in ctx.exception.details["thread_affinity"]["reason"]

    def test_assert_owner_dispatch_supported_accepts_supported_status(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"owner_dispatch",'
            b'"owner_dispatch_supported":true}}'
        )
        assert omnivm_mod.assert_owner_dispatch_supported("startup") is True

    def test_owner_dispatch_target_status_reports_target_contract(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{"python_asyncio":{'
            b'"supported":false,"diagnostic":"loop not owned"}}}}'
        )
        info = omnivm_mod.owner_dispatch_target_status("python_asyncio")
        assert info["supported"] is False
        assert info["diagnostic"] == "loop not owned"

    def test_owner_dispatch_target_status_requires_known_target(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{}}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.owner_dispatch_target_status("python_asyncio")
        assert ctx.exception.boundary_path == "thread_affinity"
        assert ctx.exception.details["target"] == "python_asyncio"
        assert ctx.exception.details["owner_dispatch_targets"] == {}

    def test_assert_owner_dispatch_target_supported_reports_diagnostic(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{"java_executor":{'
            b'"supported":false,"diagnostic":"executor caller-managed"}}}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.assert_owner_dispatch_target_supported("java_executor", "reactor startup")
        assert "reactor startup: owner dispatch target java_executor unsupported" in str(ctx.exception)
        assert "executor caller-managed" in str(ctx.exception)
        assert ctx.exception.boundary_path == "thread_affinity"
        assert ctx.exception.details["target"] == "java_executor"
        assert ctx.exception.details["owner_dispatch_target"]["diagnostic"] == "executor caller-managed"

    def test_assert_owner_dispatch_target_supported_accepts_supported_target(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"owner_dispatch",'
            b'"owner_dispatch_supported":true,'
            b'"owner_dispatch_targets":{"python_asyncio":{"supported":true}}}}'
        )
        assert omnivm_mod.assert_owner_dispatch_target_supported("python_asyncio", "startup") is True

    def test_ruby_threading_status_reports_capability_contract(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"ruby_threading":{"mode":"single_vm_thread",'
            b'"native_threads_supported":false,'
            b'"app_server_boundary":"run Puma out of process"}}'
        )
        info = omnivm_mod.ruby_threading_status()
        assert info["mode"] == "single_vm_thread"
        assert info["native_threads_supported"] is False
        assert "Puma" in info["app_server_boundary"]

    def test_ruby_threading_status_requires_status_capability(self):
        self.mock_lib.OmniStatus.return_value = b'{"initialized":true}'
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.ruby_threading_status()
        assert ctx.exception.boundary_path == "ruby_threading"

    def test_assert_ruby_native_threads_supported_reports_diagnostic(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"ruby_threading":{"mode":"single_vm_thread",'
            b'"native_threads_supported":false,'
            b'"app_server_boundary":"run Puma out of process"}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.assert_ruby_native_threads_supported("puma startup")
        assert "puma startup: native Ruby threads unsupported" in str(ctx.exception)
        assert "single_vm_thread" in str(ctx.exception)
        assert ctx.exception.boundary_path == "ruby_threading"
        assert ctx.exception.details["ruby_threading"]["native_threads_supported"] is False
        assert "Puma" in ctx.exception.details["ruby_threading"]["app_server_boundary"]

    def test_assert_ruby_native_threads_supported_accepts_supported_status(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"ruby_threading":{"mode":"native_threads",'
            b'"native_threads_supported":true}}'
        )
        assert omnivm_mod.assert_ruby_native_threads_supported("startup") is True

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
        assert omnivm_mod._lib is None

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
