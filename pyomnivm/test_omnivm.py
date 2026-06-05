"""Tests for the omnivm Python package.

These test the pure-Python layer (ctypes setup, error handling, metrics)
without requiring libomnivm.so to be present.
"""

import builtins
import asyncio
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
            {
                "type": "java.lang.IllegalArgumentException",
                "message": "inner",
                "runtime": "java",
                "origin_runtime": "java",
            }
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
            {
                "type": "TypeError",
                "message": "inner",
                "runtime": "javascript",
                "origin_runtime": "javascript",
            }
        ]

    def test_parses_python_exception_group_causes(self):
        err = omnivm_mod.RuntimeError(
            "Traceback (most recent call last):\n"
            "ExceptionGroup: batch failed (2 sub-exceptions)\n"
            "Caused by: ValueError: bad age\n"
            "Caused by: TypeError: bad name",
            runtime="python",
        )
        assert err.runtime == "python"
        assert err.type == "ExceptionGroup"
        assert err.message == "batch failed (2 sub-exceptions)"
        assert err.cause_chain == [
            {
                "type": "ValueError",
                "message": "bad age",
                "runtime": "python",
                "origin_runtime": "python",
            },
            {
                "type": "TypeError",
                "message": "bad name",
                "runtime": "python",
                "origin_runtime": "python",
            },
        ]

    def test_runtime_error_accepts_transport_error_marker(self):
        err = omnivm_mod.RuntimeError(
            'ERR:{"runtime":"javascript","type":"TypeError","message":"bad"}',
            runtime="go",
            boundary_path="call[javascript]",
        )
        assert err.runtime == "javascript"
        assert err.type == "TypeError"
        assert err.message == "bad"
        assert err.boundary_path == "call[javascript]"

    def test_runtime_error_accepts_javascript_name_stack_aliases(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "name": "TypeError",
                    "message": "bad",
                    "stack": "TypeError: bad\n    at <anonymous>:1:7",
                }
            ),
            runtime="go",
            boundary_path="call[go]",
        )

        assert err.runtime == "javascript"
        assert err.type == "TypeError"
        assert err.message == "bad"
        assert err.traceback == "TypeError: bad\n    at <anonymous>:1:7"
        assert err.stack_frames == ["TypeError: bad", "at <anonymous>:1:7"]
        assert err.boundary_path == "call[go]"

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
                            "traceback": "TypeError: inner\n    at cause (<anonymous>:2:4)",
                            "stack_frames": ["at cause (<anonymous>:2:4)"],
                            "boundary_path": "call[javascript] > callback[java]",
                            "original_error_handle": "java-error-3",
                            "details": {"code": "E_INNER", "path": ["user", "age"]},
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
                "traceback": "TypeError: inner\n    at cause (<anonymous>:2:4)",
                "stack_frames": ["at cause (<anonymous>:2:4)"],
                "runtime": "java",
                "origin_runtime": "ruby",
                "boundary_path": "call[javascript] > callback[java]",
                "original_error_handle": "java-error-3",
                "details": {"code": "E_INNER", "path": ["user", "age"]},
                "details_json": '{"code":"E_INNER","path":["user","age"]}',
            }
        ]
        assert err.boundary_path == "call[javascript] > callback[python]"
        assert err.original_error_handle == "js-error-7"
        assert err.details == [{"path": ["user", "age"]}]
        assert err.to_dict()["origin_runtime"] == "python"

    def test_parses_camel_case_structured_json_error_envelope(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "originRuntime": "python",
                    "type": "AggregateError",
                    "message": "invalid",
                    "traceback": "fallback frame",
                    "stackFrames": ["at parse (<anonymous>:1:2)"],
                    "causeChain": [
                        {
                            "runtime": "java",
                            "originRuntime": "ruby",
                            "type": "TypeError",
                            "message": "inner",
                            "traceback": "TypeError: inner\n    at cause (<anonymous>:2:4)",
                            "stackFrames": ["at cause (<anonymous>:2:4)"],
                            "boundaryPath": "call[javascript] > callback[java]",
                            "originalErrorHandle": "java-error-3",
                            "details": {"code": "E_INNER"},
                        }
                    ],
                    "boundaryPath": "call[javascript] > callback[python]",
                    "originalErrorHandle": "js-error-7",
                    "details": [{"path": ["user", "age"]}],
                }
            ),
            runtime="go",
        )
        assert err.origin_runtime == "python"
        assert err.stack_frames == ["at parse (<anonymous>:1:2)"]
        assert err.boundary_path == "call[javascript] > callback[python]"
        assert err.original_error_handle == "js-error-7"
        assert err.cause_chain == [
            {
                "type": "TypeError",
                "message": "inner",
                "traceback": "TypeError: inner\n    at cause (<anonymous>:2:4)",
                "stack_frames": ["at cause (<anonymous>:2:4)"],
                "runtime": "java",
                "origin_runtime": "ruby",
                "boundary_path": "call[javascript] > callback[java]",
                "original_error_handle": "java-error-3",
                "details": {"code": "E_INNER"},
                "details_json": '{"code":"E_INNER"}',
            }
        ]

    def test_runtime_error_exposes_camel_case_field_aliases(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "origin_runtime": "python",
                    "type": "AggregateError",
                    "message": "invalid",
                    "stack_frames": ["at parse (<anonymous>:1:2)"],
                    "cause_chain": [
                        {
                            "runtime": "java",
                            "type": "TypeError",
                            "message": "inner",
                            "details": {"path": ["payload", "age"]},
                        }
                    ],
                    "boundary_path": "call[javascript] > callback[python]",
                    "original_error_handle": "js-error-7",
                    "details_json": "{\"code\":\"E_OUTER\"}",
                }
            ),
            runtime="go",
        )

        assert err.originRuntime == "python"
        assert err.stackFrames == ["at parse (<anonymous>:1:2)"]
        assert err.causeChain == [
            {
                "type": "TypeError",
                "message": "inner",
                "runtime": "java",
                "origin_runtime": "java",
                "details": {"path": ["payload", "age"]},
                "details_json": '{"path":["payload","age"]}',
            }
        ]
        assert err.boundaryPath == "call[javascript] > callback[python]"
        assert err.originalErrorHandle == "js-error-7"
        assert err.detailsJson == "{\"code\":\"E_OUTER\"}"

        stack_frames = err.stackFrames
        stack_frames[0] = "changed"
        cause_chain = err.causeChain
        cause_chain[0]["details"]["path"][1] = "changed"
        assert err.stack_frames == ["at parse (<anonymous>:1:2)"]
        assert err.cause_chain[0]["details"] == {"path": ["payload", "age"]}

        err.originRuntime = "ruby"
        err.stackFrames = ("at alias (<anonymous>:9:1)",)
        err.causeChain = [{"runtime": "python", "message": "inner alias"}]
        err.boundaryPath = "call[javascript] > callback[ruby]"
        err.originalErrorHandle = "ruby-error-9"
        err.detailsJson = "{\"code\":\"E_ALIAS\"}"

        assert err.origin_runtime == "ruby"
        assert err.stack_frames == ["at alias (<anonymous>:9:1)"]
        assert err.cause_chain == [{"runtime": "python", "message": "inner alias"}]
        assert err.boundary_path == "call[javascript] > callback[ruby]"
        assert err.original_error_handle == "ruby-error-9"
        assert err.details_json == "{\"code\":\"E_ALIAS\"}"

    def test_details_assignment_updates_details_json(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "python",
                    "type": "ValidationError",
                    "message": "invalid",
                    "details": {"field": "old"},
                }
            ),
            runtime="python",
        )

        err.details = {"field": "age", "errors": [{"code": "too_small"}]}

        assert err.details == {"field": "age", "errors": [{"code": "too_small"}]}
        assert err.details_json == '{"field":"age","errors":[{"code":"too_small"}]}'
        assert err.to_dict()["details_json"] == err.details_json

    def test_details_json_assignment_updates_details(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "python",
                    "type": "ValidationError",
                    "message": "invalid",
                    "details": {"field": "old"},
                }
            ),
            runtime="python",
        )

        err.details_json = '{"field":"age","errors":[{"code":"too_small"}]}'

        assert err.details == {"field": "age", "errors": [{"code": "too_small"}]}
        assert err.detailsJson == '{"field":"age","errors":[{"code":"too_small"}]}'
        assert err.to_dict()["details"] == err.details

        err.detailsJson = "not json"
        assert err.details == "not json"
        assert err.details_json == "not json"

    def test_parses_details_json_structured_error_envelope(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "java",
                    "type": "IllegalStateException",
                    "message": "outer",
                    "details_json": "{\"code\":\"E_OUTER\",\"path\":[\"payload\",\"age\"]}",
                    "cause_chain": [
                        {
                            "runtime": "javascript",
                            "type": "TypeError",
                            "message": "inner",
                            "details_json": "[{\"code\":\"E_INNER\"}]",
                        },
                        {
                            "runtime": "python",
                            "type": "ValueError",
                            "message": "bad details",
                            "detailsJson": "not json",
                        },
                    ],
                }
            ),
            runtime="java",
        )
        assert err.details == {"code": "E_OUTER", "path": ["payload", "age"]}
        assert err.details_json == "{\"code\":\"E_OUTER\",\"path\":[\"payload\",\"age\"]}"
        assert err.cause_chain[0]["details"] == [{"code": "E_INNER"}]
        assert err.cause_chain[0]["details_json"] == "[{\"code\":\"E_INNER\"}]"
        assert err.cause_chain[1]["details"] == "not json"
        assert err.cause_chain[1]["details_json"] == "not json"

    def test_runtime_error_accepts_javascript_cause_name_stack_aliases(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "name": "AggregateError",
                    "message": "outer",
                    "causeChain": [
                        {
                            "runtime": "javascript",
                            "name": "TypeError",
                            "message": "inner",
                            "stack": "TypeError: inner\n    at cause (<anonymous>:2:4)",
                        }
                    ],
                }
            ),
            runtime="go",
        )

        assert err.type == "AggregateError"
        assert err.cause_chain == [
            {
                "type": "TypeError",
                "message": "inner",
                "traceback": "TypeError: inner\n    at cause (<anonymous>:2:4)",
                "stack_frames": ["TypeError: inner", "at cause (<anonymous>:2:4)"],
                "runtime": "javascript",
                "origin_runtime": "javascript",
            }
        ]

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
            "cause_chain": [
                {
                    "type": "TypeError",
                    "message": "inner",
                    "runtime": "javascript",
                    "origin_runtime": "javascript",
                }
            ],
            "boundary_path": "call[javascript]",
            "original_error_handle": None,
            "details": None,
            "details_json": None,
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

    def test_to_json_returns_structured_error_envelope(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "origin_runtime": "python",
                    "type": "AggregateError",
                    "message": "invalid",
                    "traceback": "    at <anonymous>:1:7",
                    "stack_frames": ["at <anonymous>:1:7"],
                    "cause_chain": [
                        {
                            "type": "TypeError",
                            "message": "inner",
                            "details": {"path": ["payload", "age"]},
                        }
                    ],
                    "boundary_path": "call[javascript] > callback[python]",
                    "details": [{"code": "too_small"}],
                }
            ),
            runtime="javascript",
        )

        envelope = json.loads(err.to_json())
        assert envelope == err.to_dict()
        envelope["details"][0]["code"] = "changed"
        envelope["cause_chain"][0]["details"]["path"][1] = "changed"
        assert err.details == [{"code": "too_small"}]
        assert err.details_json == '[{"code":"too_small"}]'
        assert err.cause_chain[0]["details"] == {"path": ["payload", "age"]}
        assert err.cause_chain[0]["details_json"] == '{"path":["payload","age"]}'

    def test_cause_runtime_defaults_origin_runtime(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "type": "Error",
                    "message": "outer",
                    "causeChain": [
                        {
                            "runtime": "java",
                            "type": "java.lang.IllegalArgumentException",
                            "message": "inner",
                        }
                    ],
                }
            ),
            runtime="javascript",
        )
        assert err.cause_chain == [
            {
                "type": "java.lang.IllegalArgumentException",
                "message": "inner",
                "runtime": "java",
                "origin_runtime": "java",
            }
        ]

    def test_cause_runtime_defaults_to_envelope_runtime(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "type": "Error",
                    "message": "outer",
                    "cause_chain": [{"type": "TypeError", "message": "inner"}],
                }
            ),
            runtime="go",
        )
        assert err.cause_chain == [
            {
                "type": "TypeError",
                "message": "inner",
                "runtime": "javascript",
                "origin_runtime": "javascript",
            }
        ]

    def test_structured_envelope_normalizes_scalar_fields_to_strings(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": 7,
                    "originRuntime": 8,
                    "type": 409,
                    "message": {"error": "bad"},
                    "traceback": ["frame"],
                    "boundaryPath": 10,
                    "originalErrorHandle": 11,
                }
            ),
            runtime="javascript",
            boundary_path="call[javascript]",
        )
        assert err.runtime == "7"
        assert err.origin_runtime == "8"
        assert err.type == "409"
        assert err.message == "{'error': 'bad'}"
        assert err.traceback == "['frame']"
        assert err.boundary_path == "10"
        assert err.original_error_handle == "11"

    def test_details_override_supplies_structured_guard_context(self):
        details = {"owner_dispatch": {"owner_dispatch_supported": False}}
        err = omnivm_mod.RuntimeError(
            "owner dispatch unsupported",
            boundary_path="owner_dispatch",
            details=details,
        )
        details["owner_dispatch"]["owner_dispatch_supported"] = True

        assert err.details == {"owner_dispatch": {"owner_dispatch_supported": False}}
        envelope = err.to_dict()
        envelope["details"]["owner_dispatch"]["owner_dispatch_supported"] = True
        assert err.details == {"owner_dispatch": {"owner_dispatch_supported": False}}

    def test_details_override_copies_tuple_payloads_as_json_lists(self):
        details = ({"items": [{"path": "payload.age"}]},)
        err = omnivm_mod.RuntimeError(
            "validation failed",
            runtime="python",
            boundary_path="call[python]",
            details=details,
        )
        assert err.details == [{"items": [{"path": "payload.age"}]}]

        details[0]["items"][0]["path"] = "changed"
        envelope = err.to_dict()
        envelope["details"][0]["items"][0]["path"] = "also-changed"

        assert err.details == [{"items": [{"path": "payload.age"}]}]

    def test_to_dict_copies_mutable_envelope_values(self):
        err = omnivm_mod.RuntimeError(
            json.dumps(
                {
                    "runtime": "javascript",
                    "type": "Error",
                    "message": "outer",
                    "traceback": "    at <anonymous>:1:7",
                    "stack_frames": ["at <anonymous>:1:7"],
                    "cause_chain": [
                        {
                            "type": "TypeError",
                            "message": "inner",
                            "stack_frames": ["at cause (<anonymous>:2:4)"],
                            "details": {"items": [{"path": "cause.path"}]},
                        }
                    ],
                    "details": {"items": [{"path": "user.age"}]},
                }
            ),
            runtime="javascript",
        )
        stack_frames = err.stack_frames
        stack_frames[0] = "changed"
        cause_chain = err.cause_chain
        cause_chain[0]["message"] = "changed"
        cause_chain[0]["stack_frames"][0] = "changed"
        cause_chain[0]["details"]["items"][0]["path"] = "changed"
        details = err.details
        details["items"][0]["path"] = "changed"

        assert err.stack_frames == ["at <anonymous>:1:7"]
        assert err.cause_chain == [
            {
                "type": "TypeError",
                "message": "inner",
                "runtime": "javascript",
                "origin_runtime": "javascript",
                "stack_frames": ["at cause (<anonymous>:2:4)"],
                "details": {"items": [{"path": "cause.path"}]},
                "details_json": '{"items":[{"path":"cause.path"}]}',
            }
        ]
        assert err.details == {"items": [{"path": "user.age"}]}

        envelope = err.to_dict()
        envelope["stack_frames"][0] = "changed"
        envelope["cause_chain"][0]["message"] = "changed"
        envelope["cause_chain"][0]["stack_frames"][0] = "changed"
        envelope["cause_chain"][0]["details"]["items"][0]["path"] = "changed"
        envelope["details"]["items"][0]["path"] = "changed"

        assert err.stack_frames == ["at <anonymous>:1:7"]
        assert err.cause_chain == [
            {
                "type": "TypeError",
                "message": "inner",
                "runtime": "javascript",
                "origin_runtime": "javascript",
                "stack_frames": ["at cause (<anonymous>:2:4)"],
                "details": {"items": [{"path": "cause.path"}]},
                "details_json": '{"items":[{"path":"cause.path"}]}',
            }
        ]
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
            {
                "type": "",
                "message": "inner layer",
                "runtime": "go",
                "origin_runtime": "go",
            }
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

    def test_prefixed_structured_envelope_preserves_error_fields(self):
        err = omnivm_mod.RuntimeError(
            "execute manifest: exec [javascript]: "
            + json.dumps(
                {
                    "runtime": "javascript",
                    "origin_runtime": "python",
                    "type": "AggregateError",
                    "message": "outer",
                    "traceback": "    at outer (<anonymous>:1:7)",
                    "stack_frames": ["at outer (<anonymous>:1:7)"],
                    "cause_chain": [
                        {
                            "runtime": "python",
                            "origin_runtime": "ruby",
                            "type": "ValueError",
                            "message": "inner",
                            "details": {"field": "age"},
                        }
                    ],
                    "details": [{"code": "invalid"}],
                }
            ),
            runtime="go",
            boundary_path="call[go]",
        )
        assert err.runtime == "javascript"
        assert err.origin_runtime == "python"
        assert err.type == "AggregateError"
        assert err.message == "outer"
        assert err.boundary_path == "execute manifest > exec[javascript]"
        assert err.details == [{"code": "invalid"}]
        assert err.cause_chain == [
            {
                "type": "ValueError",
                "message": "inner",
                "runtime": "python",
                "origin_runtime": "ruby",
                "details": {"field": "age"},
                "details_json": '{"field":"age"}',
            }
        ]

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

    def test_drain_finalizer_releases_noops_without_runtime(self):
        assert omnivm_mod.drain_finalizer_releases() is False

    def test_drain_finalizer_releases_noops_without_capability(self):
        class OldLib:
            pass

        omnivm_mod._lib = OldLib()
        assert omnivm_mod.drain_finalizer_releases() is False

    def test_lifecycle_scope_noops_without_runtime(self):
        with omnivm_mod.lifecycle_scope() as scope:
            assert scope.drained_finalizers is None
        assert scope.drained_finalizers is False

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
        omnivm_mod._manifest_proxy_cache.clear()

    def tearDown(self):
        omnivm_mod._lib = None
        omnivm_mod._worker_drain_hook_installed = False
        omnivm_mod._manifest_proxy_cache.clear()

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

    def test_drain_finalizer_releases_calls_lib(self):
        self.mock_lib.OmniDrainFinalizerReleases.return_value = 0
        assert omnivm_mod.drain_finalizer_releases(12) is True
        self.mock_lib.OmniDrainFinalizerReleases.assert_called_once_with(12)

    def test_drain_finalizer_releases_stays_quiet_on_failure(self):
        self.mock_lib.OmniDrainFinalizerReleases.return_value = -1
        assert omnivm_mod.drain_finalizer_releases() is False
        self.mock_lib.OmniDrainFinalizerReleases.assert_called_once_with(0)

    def test_drain_finalizer_releases_stays_quiet_on_exception(self):
        self.mock_lib.OmniDrainFinalizerReleases.side_effect = RuntimeError("drain failed")
        assert omnivm_mod.drain_finalizer_releases() is False
        self.mock_lib.OmniDrainFinalizerReleases.assert_called_once_with(0)

    def test_lifecycle_scope_drains_finalizers_on_exit(self):
        self.mock_lib.OmniDrainFinalizerReleases.return_value = 0
        with omnivm_mod.lifecycle_scope(max_finalizer_releases=5) as scope:
            assert scope.drained_finalizers is None
        assert scope.drained_finalizers is True
        self.mock_lib.OmniDrainFinalizerReleases.assert_called_once_with(5)

    def test_lifecycle_scope_preserves_body_exception_when_cleanup_fails(self):
        self.mock_lib.OmniDrainFinalizerReleases.side_effect = RuntimeError("drain failed")
        with self.assertRaisesRegex(ValueError, "body failed") as ctx:
            with omnivm_mod.lifecycle_scope() as scope:
                raise ValueError("body failed")
        assert str(ctx.exception) == "body failed"
        assert scope.drained_finalizers is False
        self.mock_lib.OmniDrainFinalizerReleases.assert_called_once_with(0)

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
        proxy._omnivm_close()
        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        assert requests[0]["args"][0]["var"].startswith("__omnivm_arg_refs['py_")
        assert {"op": "handle_adopt", "id": 7} in requests
        assert {"op": "handle_release_explicit", "id": 7} in requests

    def test_manifest_proxy_method_keeps_returned_complex_arg_refs_until_proxy_close(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        request = object()
        requests = []
        if hasattr(builtins, "__omnivm_arg_refs"):
            delattr(builtins, "__omnivm_arg_refs")
        self.mock_lib.OmniExecHost.return_value = b"OK:"

        def descriptor(handle_id):
            return {
                "__omnivm_resource__": True,
                "id": handle_id,
                "runtime": "python",
                "kind": "request",
                "transfer": True,
            }

        def manifest_call(_module_id, payload):
            request_payload = json.loads(payload.decode("utf-8"))
            requests.append(request_payload)
            if request_payload.get("func") == "root":
                return envelope(descriptor(7))
            if request_payload.get("op") == "handle_get" and request_payload.get("key") == "child":
                return envelope({"__omnivm_callable__": True, "key": "child"})
            if request_payload.get("op") == "handle_call" and request_payload.get("key") == "child":
                return envelope(descriptor(8))
            if request_payload.get("op") in {"handle_adopt", "handle_release_explicit"}:
                return envelope(True, "bool")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        root = omnivm_mod.manifest_call("demo", "root")
        child = root.child(request)

        refs = getattr(builtins, "__omnivm_arg_refs", {})
        assert request in refs.values()
        child._omnivm_close()
        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        method_call = next(req for req in requests if req.get("op") == "handle_call")
        assert method_call["args"][0]["var"].startswith("__omnivm_arg_refs['py_")
        assert {"op": "handle_adopt", "id": 8} in requests
        assert {"op": "handle_release_explicit", "id": 8} in requests
        root._omnivm_close()

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
        proxy._omnivm_close()
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
        fields = {
            "close": "field-close",
            "dispose": "field-dispose",
            "length": 3,
            "__class__": "remote-class",
            "__repr__": "remote-repr",
        }

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
                return envelope(["close", "dispose", "length", "__class__", "__repr__"])
            if request.get("op") == "handle_iter" and request.get("mode") == "items":
                return envelope([
                    ["close", fields["close"]],
                    ["dispose", fields["dispose"]],
                    ["length", fields["length"]],
                    ["__class__", fields["__class__"]],
                    ["__repr__", fields["__repr__"]],
                ])
            if request.get("op") == "handle_iter" and request.get("mode") == "values":
                return envelope([fields["close"], fields["dispose"], fields["length"], fields["__class__"], fields["__repr__"]])
            if request.get("op") == "handle_contains":
                return envelope(request["value"] in fields, "bool")
            if request.get("op") == "handle_release_explicit":
                return envelope(True, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        proxy = omnivm_mod.manifest_call("demo", "tool")

        assert proxy.close == "field-close"
        assert proxy.dispose == "field-dispose"
        assert proxy.__class__ is omnivm_mod.ManifestProxy
        assert repr(proxy).startswith("<omnivm.ManifestProxy ")
        assert omnivm_mod.proxy_get(proxy, "close") == "field-close"
        assert omnivm_mod.proxy_get(proxy, "dispose") == "field-dispose"
        assert omnivm_mod.proxy_get(proxy, "__class__") == "remote-class"
        assert omnivm_mod.proxy_get(proxy, "__repr__") == "remote-repr"
        assert omnivm_mod.proxy_set(proxy, "close", "field-updated") is True
        assert omnivm_mod.proxy_get(proxy, "close") == "field-updated"
        assert omnivm_mod.proxy_call(proxy, "accept", args=("js",)) == "accepted-js"
        assert omnivm_mod.proxy_len(proxy) == 7
        assert omnivm_mod.proxy_keys(proxy) == ["close", "dispose", "length", "__class__", "__repr__"]
        assert omnivm_mod.proxy_items(proxy) == [
            ["close", "field-updated"],
            ["dispose", "field-dispose"],
            ["length", 3],
            ["__class__", "remote-class"],
            ["__repr__", "remote-repr"],
        ]
        assert omnivm_mod.proxy_values(proxy) == ["field-updated", "field-dispose", 3, "remote-class", "remote-repr"]
        assert omnivm_mod.proxy_contains(proxy, "close") is True
        assert omnivm_mod.proxy_contains(proxy, "missing") is False
        assert proxy._omnivm_close() is True
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
            proxy._omnivm_close()
        with self.assertRaisesRegex(RuntimeError, "release failed"):
            proxy._omnivm_close()

    def test_manifest_proxy_close_false_result_remains_retryable(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        releases = 0

        def manifest_call(_module_id, payload):
            nonlocal releases
            request = json.loads(payload.decode("utf-8"))
            if request.get("func") == "tool":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 49,
                    "runtime": "python",
                    "kind": "object",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "handle_release_explicit":
                releases += 1
                return envelope(releases >= 2, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "tool")

        assert proxy._omnivm_close() is False
        assert proxy._omnivm_close() is True
        assert proxy._omnivm_close() is False
        assert releases == 2

    def test_manifest_proxy_close_remains_fallback_when_owner_has_no_close_field(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            requests.append(request)
            if request.get("func") == "tool":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 53,
                    "runtime": "python",
                    "kind": "object",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "handle_contains" and request.get("value") in {"close", "dispose"}:
                return envelope(False, "bool")
            if request.get("op") == "handle_get" and request.get("key") in {"close", "dispose"}:
                raise AssertionError("missing lifecycle field should not be fetched")
            if request.get("op") == "handle_release_explicit":
                return envelope(True, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "tool")

        assert proxy.close() is True
        assert proxy.close() is False
        assert [request for request in requests if request.get("op") == "handle_release_explicit"] == [
            {"op": "handle_release_explicit", "id": 53}
        ]

    def test_manifest_proxy_callable_close_field_stays_owner_method(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            requests.append(request)
            if request.get("func") == "tool":
                return envelope({
                    "__omnivm_resource__": True,
                    "id": 54,
                    "runtime": "python",
                    "kind": "object",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "handle_contains" and request.get("value") == "close":
                return envelope(True, "bool")
            if request.get("op") == "handle_get" and request.get("key") == "close":
                return envelope({"__omnivm_callable__": True, "key": "close"})
            if request.get("op") == "handle_call" and request.get("key") == "close":
                return envelope("owner-close:" + request["args"][0])
            if request.get("op") == "handle_release_explicit":
                return envelope(True, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "tool")

        assert proxy.close("field") == "owner-close:field"
        assert omnivm_mod.proxy_close(proxy) is True
        assert [request for request in requests if request.get("op") == "handle_release_explicit"] == [
            {"op": "handle_release_explicit", "id": 54}
        ]
        assert [request for request in requests if request.get("op") == "handle_call" and request.get("key") == "close"] == [
            {"op": "handle_call", "id": 54, "key": "close", "args": ["field"]}
        ]

    def test_proxy_close_preserves_public_close_result_without_dynamic_lookup(self):
        class FalseCloser:
            def close(self):
                return False

        class TextCloser:
            def close(self):
                return "closed"

        class NoneCloser:
            def close(self):
                return None

        class TextDispose:
            def dispose(self):
                return "disposed"

        class FalseDispose:
            def dispose(self):
                return False

        class NoneDispose:
            def dispose(self):
                return None

        class CloseAndDispose:
            def close(self):
                return "closed"

            def dispose(self):
                raise AssertionError("dispose should not run when close exists")

        class RequiredCloseAndDispose:
            def close(self, reason):
                raise AssertionError("required-arg close should not run")

            def dispose(self):
                return "disposed-after-required-close"

        class RequiredOnlyClose:
            def close(self, reason):
                raise AssertionError("required-arg close should not run")

        class DynamicCloseTrap:
            dynamic_lookup_count = 0

            def __getattr__(self, name):
                if name == "close":
                    self.dynamic_lookup_count += 1
                    return lambda: "dynamic-close"
                raise AttributeError(name)

        class DynamicDisposeTrap:
            dynamic_lookup_count = 0

            def __getattr__(self, name):
                if name == "dispose":
                    self.dynamic_lookup_count += 1
                    return lambda: "dynamic-dispose"
                raise AttributeError(name)

        trap = DynamicCloseTrap()
        dispose_trap = DynamicDisposeTrap()

        assert omnivm_mod.proxy_close(FalseCloser()) is False
        assert omnivm_mod.proxy_close(TextCloser()) == "closed"
        assert omnivm_mod.proxy_close(NoneCloser()) is True
        assert omnivm_mod.proxy_close(TextDispose()) == "disposed"
        assert omnivm_mod.proxy_close(FalseDispose()) is False
        assert omnivm_mod.proxy_close(NoneDispose()) is True
        assert omnivm_mod.proxy_close(CloseAndDispose()) == "closed"
        assert omnivm_mod.proxy_close(RequiredCloseAndDispose()) == "disposed-after-required-close"
        assert omnivm_mod.proxy_close(RequiredOnlyClose()) is False
        assert omnivm_mod.proxy_close(trap) is False
        assert trap.dynamic_lookup_count == 0
        assert omnivm_mod.proxy_close(dispose_trap) is False
        assert dispose_trap.dynamic_lookup_count == 0

    def test_proxy_close_prefers_collision_safe_omnivm_close(self):
        class BothClosers:
            def _omnivm_close(self):
                return "omnivm-closed"

            def close(self):
                return "public-close"

        assert omnivm_mod.proxy_close(BothClosers()) == "omnivm-closed"

    def test_omnivm_close_alias_matches_proxy_close(self):
        class BothClosers:
            def _omnivm_close(self):
                return "omnivm-closed"

            def close(self):
                return "public-close"

        assert "omnivm_close" in omnivm_mod.__all__
        assert omnivm_mod.omnivm_close(BothClosers()) == "omnivm-closed"

    def test_aproxy_close_awaits_close_and_aclose_without_dynamic_lookup(self):
        class AsyncClose:
            async def close(self):
                return "async-close"

        class AsyncNoneClose:
            async def close(self):
                return None

        class AsyncDispose:
            async def dispose(self):
                return "async-dispose"

        class SyncDispose:
            def dispose(self):
                return "sync-dispose"

        class AsyncNoneDispose:
            async def dispose(self):
                return None

        class CloseAndAsyncDispose:
            async def close(self):
                return "async-close"

            async def dispose(self):
                raise AssertionError("dispose should not run when close exists")

        class AsyncAclose:
            def __init__(self):
                self.closed = False

            async def aclose(self):
                self.closed = True

        class AcloseAndDispose:
            async def aclose(self):
                return "async-aclose"

            async def dispose(self):
                raise AssertionError("dispose should not run when aclose exists")

        class RequiredAcloseAndDispose:
            async def aclose(self, reason):
                raise AssertionError("required-arg aclose should not run")

            async def dispose(self):
                return "dispose-after-required-aclose"

        class BothAsyncClosers:
            async def _omnivm_close(self):
                return "omnivm-async-closed"

            async def aclose(self):
                raise AssertionError("aclose should not run when _omnivm_close exists")

        class DynamicAcloseTrap:
            dynamic_lookup_count = 0

            def __getattr__(self, name):
                if name == "aclose":
                    self.dynamic_lookup_count += 1
                    return lambda: "dynamic-aclose"
                raise AttributeError(name)

        class DynamicDisposeTrap:
            dynamic_lookup_count = 0

            def __getattr__(self, name):
                if name == "dispose":
                    self.dynamic_lookup_count += 1
                    return lambda: "dynamic-dispose"
                raise AttributeError(name)

        async def run():
            assert await omnivm_mod.aproxy_close(AsyncClose()) == "async-close"
            assert await omnivm_mod.aproxy_close(AsyncNoneClose()) is True
            assert await omnivm_mod.aproxy_close(AsyncDispose()) == "async-dispose"
            assert await omnivm_mod.aproxy_close(SyncDispose()) == "sync-dispose"
            assert await omnivm_mod.aproxy_close(AsyncNoneDispose()) is True
            assert await omnivm_mod.aproxy_close(CloseAndAsyncDispose()) == "async-close"
            aclose = AsyncAclose()
            assert await omnivm_mod.aproxy_close(aclose) is True
            assert aclose.closed is True
            assert await omnivm_mod.aproxy_close(AcloseAndDispose()) == "async-aclose"
            assert await omnivm_mod.aproxy_close(RequiredAcloseAndDispose()) == "dispose-after-required-aclose"
            assert await omnivm_mod.omnivm_aclose(BothAsyncClosers()) == "omnivm-async-closed"
            trap = DynamicAcloseTrap()
            assert await omnivm_mod.aproxy_close(trap) is False
            assert trap.dynamic_lookup_count == 0
            dispose_trap = DynamicDisposeTrap()
            assert await omnivm_mod.aproxy_close(dispose_trap) is False
            assert dispose_trap.dynamic_lookup_count == 0

        assert "aproxy_close" in omnivm_mod.__all__
        assert "omnivm_aclose" in omnivm_mod.__all__
        assert omnivm_mod.proxy_close(AsyncAclose()) is False
        asyncio.run(run())

    def test_proxy_close_preserves_static_class_and_instance_methods(self):
        class StaticCloser:
            @staticmethod
            def close():
                return "static-closed"

        class ClassCloser:
            @classmethod
            def close(cls):
                return cls.__name__

        class InstanceCloser:
            def __init__(self):
                self.close = lambda: "instance-closed"

        class SlottedInstanceCloser:
            __slots__ = ("close",)

            def __init__(self):
                self.close = lambda: "slotted-instance-closed"

        class InstanceGetattributeTrap:
            def __init__(self):
                self.lookup_count = 0
                self.close = lambda: "instance-static-closed"

            def __getattribute__(self, name):
                if name == "close":
                    object.__setattr__(
                        self,
                        "lookup_count",
                        object.__getattribute__(self, "lookup_count") + 1,
                    )
                    raise AssertionError("dynamic close lookup should not run")
                return object.__getattribute__(self, name)

        trap = InstanceGetattributeTrap()

        assert omnivm_mod.proxy_close(StaticCloser()) == "static-closed"
        assert omnivm_mod.proxy_close(ClassCloser()) == "ClassCloser"
        assert omnivm_mod.proxy_close(InstanceCloser()) == "instance-closed"
        assert omnivm_mod.proxy_close(SlottedInstanceCloser()) == "slotted-instance-closed"
        assert omnivm_mod.proxy_close(trap) == "instance-static-closed"
        assert trap.lookup_count == 0

    def test_proxy_helpers_ignore_descriptor_fields(self):
        class CloseProperty:
            property_accesses = 0

            @property
            def close(self):
                self.property_accesses += 1
                return lambda: "property-close"

        class ValuesProperty:
            property_accesses = 0

            @property
            def values(self):
                self.property_accesses += 1
                return lambda: ["property-value"]

            def __iter__(self):
                return iter(["a", "b"])

        class ItemsProperty:
            property_accesses = 0

            @property
            def items(self):
                self.property_accesses += 1
                return lambda: [("property", "item")]

            def __iter__(self):
                return iter(["a", "b"])

        class KeysProperty:
            property_accesses = 0

            @property
            def keys(self):
                self.property_accesses += 1
                return lambda: ["property-key"]

            def __len__(self):
                return 2

        close_value = CloseProperty()
        values = ValuesProperty()
        items = ItemsProperty()
        keys = KeysProperty()

        assert omnivm_mod.proxy_close(close_value) is False
        assert close_value.property_accesses == 0
        assert omnivm_mod.proxy_values(values) == ["a", "b"]
        assert values.property_accesses == 0
        assert omnivm_mod.proxy_items(items) == [(0, "a"), (1, "b")]
        assert items.property_accesses == 0
        assert omnivm_mod.proxy_keys(keys) == [0, 1]
        assert keys.property_accesses == 0

    def test_proxy_iter_ignores_non_method_identity_fields(self):
        class ValuesField:
            values = "field-values"

            def __iter__(self):
                return iter(["a", "b"])

        class ItemsField:
            items = "field-items"

            def __iter__(self):
                return iter(["a", "b"])

        class KeysTrap:
            dynamic_lookup_count = 0

            def __getattr__(self, name):
                if name == "keys":
                    self.dynamic_lookup_count += 1
                    return lambda: ["dynamic-key"]
                raise AttributeError(name)

            def __len__(self):
                return 2

        trap = KeysTrap()

        assert omnivm_mod.proxy_values(ValuesField()) == ["a", "b"]
        assert omnivm_mod.proxy_items(ItemsField()) == [(0, "a"), (1, "b")]
        assert omnivm_mod.proxy_keys(trap) == [0, 1]
        assert trap.dynamic_lookup_count == 0

    def test_proxy_call_uses_declared_methods_without_dynamic_lookup(self):
        class DeclaredMethod:
            def accept(self, value):
                return f"accepted-{value}"

        class StaticMethod:
            @staticmethod
            def accept(value):
                return f"static-{value}"

        class ClassMethod:
            @classmethod
            def accept(cls, value):
                return f"{cls.__name__}-{value}"

        class InstanceMethod:
            def __init__(self):
                self.accept = lambda value: f"instance-{value}"

        class DynamicMethodTrap:
            dynamic_lookup_count = 0

            def __getattr__(self, name):
                if name == "accept":
                    self.dynamic_lookup_count += 1
                    return lambda value: f"dynamic-{value}"
                raise AttributeError(name)

        trap = DynamicMethodTrap()

        assert omnivm_mod.proxy_call(DeclaredMethod(), "accept", args=("ok",)) == "accepted-ok"
        assert omnivm_mod.proxy_call(StaticMethod(), "accept", args=("ok",)) == "static-ok"
        assert omnivm_mod.proxy_call(ClassMethod(), "accept", args=("ok",)) == "ClassMethod-ok"
        assert omnivm_mod.proxy_call(InstanceMethod(), "accept", args=("ok",)) == "instance-ok"
        with self.assertRaises(AttributeError):
            omnivm_mod.proxy_call(trap, "accept", args=("ok",))
        assert trap.dynamic_lookup_count == 0

    def test_proxy_call_ignores_non_callable_fields_and_properties(self):
        class FieldCollision:
            accept = "field"

        class PropertyCollision:
            property_accesses = 0

            @property
            def accept(self):
                self.property_accesses += 1
                return lambda value: f"property-{value}"

        prop = PropertyCollision()

        with self.assertRaises(TypeError):
            omnivm_mod.proxy_call(FieldCollision(), "accept", args=("ok",))
        with self.assertRaises(AttributeError):
            omnivm_mod.proxy_call(prop, "accept", args=("ok",))
        assert prop.property_accesses == 0

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
        cleanup = omnivm_mod.cleanup_errors(ctx.exception)
        assert len(cleanup) == 1
        assert str(cleanup[0]) == "release failed"
        cleanup.clear()
        assert str(omnivm_mod.cleanup_errors(ctx.exception)[0]) == "release failed"

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

        assert proxy._omnivm_close() is True
        assert omnivm_mod.proxy_close(proxy) is False
        assert requests[-1] == {"op": "stream_cancel", "id": 45}
        assert sum(1 for request in requests if request.get("op") == "stream_cancel") == 1

    def test_manifest_stream_iterator_stops_after_proxy_close(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []

        def manifest_call(_module_id, payload):
            request = json.loads(payload.decode("utf-8"))
            requests.append(request)
            if request.get("func") == "rows":
                return envelope({
                    "__omnivm_stream__": True,
                    "id": 46,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request.get("op") == "stream_cancel":
                return envelope(True, "bool")
            if request.get("op") == "stream_next":
                raise AssertionError("closed iterator should not request another chunk")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows")
        iterator = iter(proxy)

        assert proxy._omnivm_close() is True
        with self.assertRaises(StopIteration):
            next(iterator)
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

    def test_manifest_call_wraps_embedded_local_stream_values(self):
        def envelope(value, kind="json"):
            return ("OK:" + json.dumps({"__omnivm_result__": True, "kind": kind, "value": value})).encode("utf-8")

        requests = []

        def manifest_call(_module_id, payload):
            request_payload = json.loads(payload.decode("utf-8"))
            requests.append(request_payload)
            if request_payload.get("func") == "rows":
                return envelope({
                    "__omnivm_stream__": True,
                    "runtime": "python",
                    "kind": "stream",
                    "values": [
                        "row-1",
                        {"nested": {"__omnivm_stream__": True, "values": ["inner"]}},
                    ],
                })
            if request_payload.get("op") in {"handle_retain", "handle_adopt", "stream_next", "stream_cancel", "handle_release_finalizer"}:
                raise AssertionError("embedded local stream should not call manifest lifecycle ops")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        stream = omnivm_mod.manifest_call("demo", "rows")
        assert not isinstance(stream, dict)
        values = list(stream)
        assert values[0] == "row-1"
        assert list(values[1]["nested"]) == ["inner"]
        assert stream._omnivm_close() is False
        assert omnivm_mod.proxy_close(stream) is False
        assert requests == [{"func": "rows", "args": []}]

    def test_embedded_local_stream_closes_on_early_break(self):
        stream = omnivm_mod._wrap_manifest_value(
            "demo",
            {"__omnivm_stream__": True, "runtime": "python", "kind": "stream", "values": ["row-1", "row-2"]},
        )

        def consume_one():
            for item in stream:
                assert item == "row-1"
                break

        consume_one()
        assert omnivm_mod.proxy_close(stream) is False
        assert list(stream) == []

    def test_embedded_local_stream_exposes_collision_safe_close(self):
        stream = omnivm_mod._wrap_manifest_value(
            "demo",
            {"__omnivm_stream__": True, "runtime": "python", "kind": "stream", "values": ["row-1", "row-2"]},
        )

        assert stream._omnivm_close() is True
        assert stream._omnivm_close() is False
        assert list(stream) == []

    def test_embedded_local_stream_context_closes_on_exit(self):
        stream = omnivm_mod._wrap_manifest_value(
            "demo",
            {"__omnivm_stream__": True, "runtime": "python", "kind": "stream", "values": ["row-1", "row-2"]},
        )

        with stream as scoped:
            assert scoped is stream
            assert next(iter(scoped)) == "row-1"

        assert omnivm_mod.proxy_close(stream) is False
        assert list(stream) == []

    def test_embedded_local_stream_context_preserves_body_exception(self):
        stream = omnivm_mod._wrap_manifest_value(
            "demo",
            {"__omnivm_stream__": True, "runtime": "python", "kind": "stream", "values": ["row-1", "row-2"]},
        )

        with self.assertRaisesRegex(ValueError, "body failed"):
            with stream:
                raise ValueError("body failed")

        assert omnivm_mod.proxy_close(stream) is False
        assert list(stream) == []

    def test_manifest_stream_iterator_cancels_on_early_break(self):
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
                    "id": 49,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request_payload.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request_payload.get("op") == "stream_next":
                return envelope({"done": False, "value": "row-1"})
            if request_payload.get("op") == "stream_cancel":
                return envelope(True, "bool")
            if request_payload.get("op") == "handle_release_finalizer":
                raise AssertionError("early break should cancel, not queue quiet finalizer cleanup")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows", args=(request,))

        def consume_one():
            for item in proxy:
                assert item == "row-1"
                break

        consume_one()
        gc.collect()

        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        assert proxy.close() is False
        cancels = [request for request in requests if request.get("op") == "stream_cancel"]
        assert cancels == [{"op": "stream_cancel", "id": 49}]

    def test_manifest_stream_iterator_cancels_on_chunk_materialization_error(self):
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
                    "id": 51,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request_payload.get("op") == "handle_adopt" and request_payload.get("id") == 51:
                return envelope(True, "bool")
            if request_payload.get("op") == "stream_next":
                return envelope({
                    "done": False,
                    "value": {
                        "__omnivm_resource__": True,
                        "id": 91,
                        "runtime": "python",
                        "kind": "row",
                        "transfer": True,
                    },
                })
            if request_payload.get("op") == "handle_adopt" and request_payload.get("id") == 91:
                raise RuntimeError("chunk adopt failed")
            if request_payload.get("op") == "stream_cancel":
                return envelope(True, "bool")
            if request_payload.get("op") == "handle_release_finalizer":
                raise AssertionError("chunk materialization failure should cancel the stream explicitly")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows", args=(request,))

        iterator = iter(proxy)
        with self.assertRaisesRegex(RuntimeError, "chunk adopt failed"):
            next(iterator)

        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        assert proxy.close() is False
        cancels = [request for request in requests if request.get("op") == "stream_cancel"]
        assert cancels == [{"op": "stream_cancel", "id": 51}]

    def test_manifest_stream_iterator_materialization_error_keeps_failed_cancel_quiet_after_recording(self):
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
                    "id": 52,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request_payload.get("op") == "handle_adopt" and request_payload.get("id") == 52:
                return envelope(True, "bool")
            if request_payload.get("op") == "stream_next":
                return envelope({
                    "done": False,
                    "value": {
                        "__omnivm_resource__": True,
                        "id": 92,
                        "runtime": "python",
                        "kind": "row",
                        "transfer": True,
                    },
                })
            if request_payload.get("op") == "handle_adopt" and request_payload.get("id") == 92:
                raise RuntimeError("chunk adopt failed")
            if request_payload.get("op") == "stream_cancel":
                raise RuntimeError("cancel failed")
            if request_payload.get("op") == "handle_release_finalizer":
                raise AssertionError("failed materialization cleanup should not fall back to finalizer release")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows", args=(request,))

        iterator = iter(proxy)
        with self.assertRaisesRegex(RuntimeError, "chunk adopt failed") as ctx:
            next(iterator)

        cleanup = omnivm_mod.cleanup_errors(ctx.exception)
        assert len(cleanup) == 1
        assert str(cleanup[0]) == "cancel failed"
        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        assert proxy.close() is False
        gc.collect()
        cancels = [request for request in requests if request.get("op") == "stream_cancel"]
        assert cancels == [{"op": "stream_cancel", "id": 52}]

    def test_manifest_stream_iterator_detaches_on_next_error(self):
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
                    "id": 50,
                    "runtime": "python",
                    "kind": "queryset",
                    "transfer": True,
                })
            if request_payload.get("op") == "handle_adopt":
                return envelope(True, "bool")
            if request_payload.get("op") == "stream_next":
                raise RuntimeError("owner read failed")
            if request_payload.get("op") in {"stream_cancel", "handle_release_finalizer"}:
                raise AssertionError("terminal stream error should detach without later cleanup")
            raise AssertionError(request_payload)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call
        proxy = omnivm_mod.manifest_call("demo", "rows", args=(request,))

        iterator = iter(proxy)
        with self.assertRaisesRegex(RuntimeError, "owner read failed"):
            next(iterator)
        assert request not in getattr(builtins, "__omnivm_arg_refs", {}).values()
        assert proxy.close() is False
        assert not any(request.get("op") == "stream_cancel" for request in requests)

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
        cleanup = omnivm_mod.cleanup_errors(ctx.exception)
        assert len(cleanup) == 1
        assert str(cleanup[0]) == "cancel failed"
        cleanup.clear()
        assert str(omnivm_mod.cleanup_errors(ctx.exception)[0]) == "cancel failed"

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
        child._omnivm_close()
        primary._omnivm_close()
        assert {"op": "handle_adopt", "id": 7} in requests
        assert {"op": "handle_retain", "id": 8} in requests
        assert {"op": "handle_release_explicit", "id": 7} in requests
        assert {"op": "handle_release_explicit", "id": 8} in requests

    def test_manifest_call_reuses_duplicate_return_proxy_descriptors(self):
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
            if request.get("func") == "duplicates":
                return envelope({
                    "items": [descriptor(7), descriptor(7)],
                    "meta": {
                        "primary": descriptor(8, transfer=False),
                        "alias": descriptor(8, transfer=False),
                    },
                })
            if request.get("op") in {"handle_adopt", "handle_retain", "handle_release_explicit"}:
                return envelope(True, "bool")
            raise AssertionError(request)

        self.mock_lib.OmniManifestCall.side_effect = manifest_call

        result = omnivm_mod.manifest_call("demo", "duplicates")

        adopted = result["items"][0]
        retained = result["meta"]["primary"]
        assert adopted is result["items"][1]
        assert retained is result["meta"]["alias"]
        assert [request for request in requests if request.get("op") == "handle_adopt"] == [
            {"op": "handle_adopt", "id": 7}
        ]
        assert [request for request in requests if request.get("op") == "handle_retain"] == [
            {"op": "handle_retain", "id": 8}
        ]
        assert adopted._omnivm_close() is True
        assert adopted._omnivm_close() is False
        assert retained._omnivm_close() is True
        assert retained._omnivm_close() is False
        assert [request for request in requests if request.get("op") == "handle_release_explicit"] == [
            {"op": "handle_release_explicit", "id": 7},
            {"op": "handle_release_explicit", "id": 8},
        ]

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

    def test_buffer_borrow_finalizer_ignores_release_failure(self):
        self.mock_lib.OmniBufRelease.side_effect = RuntimeError("release failed")

        omnivm_mod._release_buffer_borrow(b"payload")

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
            b'"lease_state":"detached","memory_space":"host",'
            b'"released":true,"active_borrows":2,'
            b'"active_named_borrows":2,"named_borrow_queue":2,'
            b'"active_borrowed_bytes":6,"detached_buffers":1,'
            b'"release_error":"producer release failed"}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.release_buffer("payload")
        assert "release_buffer failed" in str(ctx.exception)
        assert "state='released_detached'" in str(ctx.exception)
        assert "lease_state='detached'" in str(ctx.exception)
        assert "memory_space='host'" in str(ctx.exception)
        assert "active_borrows=2" in str(ctx.exception)
        assert "active_named_borrows=2" in str(ctx.exception)
        assert "named_borrow_queue=2" in str(ctx.exception)
        assert "detached_buffers=1" in str(ctx.exception)
        assert "release_error='producer release failed'" in str(ctx.exception)
        assert ctx.exception.boundary_path == "native_memory"
        assert ctx.exception.details["buffer"]["state"] == "released_detached"
        assert ctx.exception.details["buffer"]["lease_state"] == "detached"
        assert ctx.exception.details["buffer"]["memory_space"] == "host"
        assert ctx.exception.details["buffer"]["active_borrows"] == 2
        assert ctx.exception.details["buffer"]["active_named_borrows"] == 2
        assert ctx.exception.details["buffer"]["named_borrow_queue"] == 2
        assert ctx.exception.details["buffer"]["release_error"] == "producer release failed"
        self.mock_lib.OmniBufStatus.assert_called_once_with(b"payload")

    def test_buffer_owner_sets_and_releases_buffer(self):
        self.mock_lib.OmniBufSet.return_value = 0
        self.mock_lib.OmniBufFree.return_value = 0

        with omnivm_mod.buffer_owner("payload", b"abc", dtype=7) as owner:
            assert owner.name == "payload"
            assert owner.released is False

        assert owner.released is True
        name, buf = self.mock_lib.OmniBufSet.call_args.args
        assert name == b"payload"
        assert buf.len == 3
        assert buf.dtype == 7
        self.mock_lib.OmniBufFree.assert_called_once_with(b"payload")

    def test_buffer_owner_release_is_idempotent(self):
        self.mock_lib.OmniBufFree.return_value = 0
        owner = omnivm_mod.buffer_owner("payload")
        assert owner.release() is True
        assert owner.release() is False
        self.mock_lib.OmniBufFree.assert_called_once_with(b"payload")

    def test_buffer_owner_close_helpers_are_idempotent(self):
        self.mock_lib.OmniBufFree.return_value = 0
        owner = omnivm_mod.buffer_owner("payload")
        assert owner.close() is True
        assert owner.close() is False
        self.mock_lib.OmniBufFree.assert_called_once_with(b"payload")

        self.mock_lib.OmniBufFree.reset_mock()
        owner = omnivm_mod.buffer_owner("other")
        assert omnivm_mod.proxy_close(owner) is True
        assert omnivm_mod.proxy_close(owner) is False
        self.mock_lib.OmniBufFree.assert_called_once_with(b"other")

    def test_buffer_owner_rejects_reenter_after_release(self):
        self.mock_lib.OmniBufSet.return_value = 0
        self.mock_lib.OmniBufFree.return_value = 0
        owner = omnivm_mod.buffer_owner("payload", b"abc")

        with owner:
            pass

        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            with owner:
                pass

        assert ctx.exception.boundary_path == "native_memory"
        assert ctx.exception.details["buffer"] == {"name": "payload", "released": True}
        self.mock_lib.OmniBufSet.assert_called_once()
        self.mock_lib.OmniBufFree.assert_called_once_with(b"payload")

    def test_buffer_owner_rejects_nested_active_enter(self):
        self.mock_lib.OmniBufSet.return_value = 0
        self.mock_lib.OmniBufFree.return_value = 0
        owner = omnivm_mod.buffer_owner("payload", b"abc")

        with owner:
            with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
                with owner:
                    pass

        assert ctx.exception.boundary_path == "native_memory"
        assert ctx.exception.details["buffer"] == {"name": "payload", "active_owner": True}
        self.mock_lib.OmniBufSet.assert_called_once()
        self.mock_lib.OmniBufFree.assert_called_once_with(b"payload")

    def test_buffer_owner_enter_failure_resets_active_state(self):
        self.mock_lib.OmniBufSet.side_effect = [RuntimeError("set failed"), 0]
        self.mock_lib.OmniBufFree.return_value = 0
        owner = omnivm_mod.buffer_owner("payload", b"abc")

        with self.assertRaisesRegex(RuntimeError, "set failed"):
            with owner:
                pass
        with owner:
            pass

        assert self.mock_lib.OmniBufSet.call_count == 2
        self.mock_lib.OmniBufFree.assert_called_once_with(b"payload")

    def test_buffer_owner_status_delegates_to_buffer_status(self):
        self.mock_lib.OmniBufStatus.return_value = b'{"name":"payload","lease_state":"owned"}'
        owner = omnivm_mod.buffer_owner("payload")
        assert owner.status() == {"name": "payload", "lease_state": "owned"}
        self.mock_lib.OmniBufStatus.assert_called_once_with(b"payload")

    def test_buffer_owner_context_reports_release_failure_without_body_error(self):
        self.mock_lib.OmniBufFree.return_value = -1
        self.mock_lib.OmniBufStatus.return_value = (
            b'{"name":"payload","state":"released","lease_state":"released",'
            b'"released":true,"memory_space":"host","release_error":"producer release failed"}'
        )
        owner = omnivm_mod.buffer_owner("payload")

        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            with owner:
                pass

        assert ctx.exception.boundary_path == "native_memory"
        assert ctx.exception.details["buffer"]["lease_state"] == "released"
        assert ctx.exception.details["buffer"]["release_error"] == "producer release failed"
        assert owner.released is True
        assert owner.release() is False

    def test_buffer_owner_context_preserves_body_exception_when_release_fails(self):
        self.mock_lib.OmniBufFree.return_value = -1
        self.mock_lib.OmniBufStatus.return_value = (
            b'{"name":"payload","state":"released_detached","lease_state":"detached",'
            b'"released":true,"memory_space":"host","active_borrows":1}'
        )
        owner = omnivm_mod.buffer_owner("payload")

        with self.assertRaisesRegex(ValueError, "body failed") as ctx:
            with owner:
                raise ValueError("body failed")

        notes = getattr(ctx.exception, "__notes__", [])
        assert any("buffer release failed" in note for note in notes)
        cleanup = omnivm_mod.cleanup_errors(ctx.exception)
        assert len(cleanup) == 1
        assert cleanup[0].boundary_path == "native_memory"
        assert owner.released is True
        assert owner.release() is False
        cleanup.clear()
        assert omnivm_mod.cleanup_errors(ctx.exception)[0].boundary_path == "native_memory"

    def test_buffer_status_returns_lifecycle_diagnostics(self):
        self.mock_lib.OmniBufStatus.return_value = (
            b'{"name":"payload","state":"released_detached","live":false,'
            b'"lease_state":"detached","memory_space":"host","released":true,'
            b'"dtype":1,"format":"i","shape":[2],"strides":[4],'
            b'"offset":1,"null_count":1,"validity_bytes":1,"validity_bit_offset":3,'
            b'"active_borrows":1,"active_named_borrows":1,'
            b'"named_borrow_queue":1,"detached_buffers":1}'
        )
        status = omnivm_mod.buffer_status("payload")
        self.mock_lib.OmniBufStatus.assert_called_once_with(b"payload")
        assert status["state"] == "released_detached"
        assert status["lease_state"] == "detached"
        assert status["memory_space"] == "host"
        assert status["released"] is True
        assert status["dtype"] == 1
        assert status["format"] == "i"
        assert status["shape"] == [2]
        assert status["strides"] == [4]
        assert status["offset"] == 1
        assert status["null_count"] == 1
        assert status["validity_bytes"] == 1
        assert status["validity_bit_offset"] == 3
        assert status["active_borrows"] == 1
        assert status["active_named_borrows"] == 1
        assert status["named_borrow_queue"] == 1
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

    def test_release_handle_from_finalizer_stays_quiet_without_runtime(self):
        omnivm_mod._lib = None
        assert omnivm_mod._release_handle_from_finalizer(123) is False

    def test_release_handle_from_finalizer_stays_quiet_without_capability(self):
        class OldLib:
            pass

        omnivm_mod._lib = OldLib()
        assert omnivm_mod._release_handle_from_finalizer(123) is False

    def test_release_handle_from_finalizer_stays_quiet_on_failure(self):
        self.mock_lib.OmniHandleReleaseFromFinalizer.side_effect = RuntimeError("release failed")
        assert omnivm_mod._release_handle_from_finalizer(123) is False
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
        self.mock_lib.OmniHandleDropReference.return_value = None
        assert omnivm_mod._drop_handle_reference(123, 456) is True
        self.mock_lib.OmniHandleDropReference.assert_called_once_with(123, 456)

    def test_drop_handle_reference_stays_quiet_without_runtime(self):
        omnivm_mod._lib = None
        assert omnivm_mod._drop_handle_reference(123, 456) is False

    def test_drop_handle_reference_stays_quiet_without_capability(self):
        class OldLib:
            pass

        omnivm_mod._lib = OldLib()
        assert omnivm_mod._drop_handle_reference(123, 456) is False

    def test_drop_handle_reference_stays_quiet_on_failure(self):
        self.mock_lib.OmniHandleDropReference.side_effect = RuntimeError("drop failed")
        assert omnivm_mod._drop_handle_reference(123, 456) is False
        self.mock_lib.OmniHandleDropReference.assert_called_once_with(123, 456)

    def test_drain_finalizer_releases_calls_lib(self):
        self.mock_lib.OmniDrainFinalizerReleases.return_value = 0
        assert omnivm_mod._drain_finalizer_releases(25) is True
        self.mock_lib.OmniDrainFinalizerReleases.assert_called_once_with(25)

    def test_drain_finalizer_releases_stays_quiet_without_runtime(self):
        omnivm_mod._lib = None
        assert omnivm_mod._drain_finalizer_releases(25) is False

    def test_drain_finalizer_releases_stays_quiet_without_capability(self):
        class OldLib:
            pass

        omnivm_mod._lib = OldLib()
        assert omnivm_mod._drain_finalizer_releases(25) is False

    def test_drain_finalizer_releases_stays_quiet_on_failure(self):
        self.mock_lib.OmniDrainFinalizerReleases.side_effect = RuntimeError("drain failed")
        assert omnivm_mod._drain_finalizer_releases(25) is False
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
        info["owner_dispatch_targets"]["python_asyncio"]["supported"] = True
        assert omnivm_mod.owner_dispatch_status()["owner_dispatch_targets"]["python_asyncio"]["supported"] is False

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
        assert ctx.exception.boundary_path == "owner_dispatch"
        assert ctx.exception.details["owner_dispatch"]["owner_dispatch_supported"] is False
        assert "c-shared mode" in ctx.exception.details["owner_dispatch"]["reason"]

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
        assert info["target"] == "python_asyncio"
        assert info["requested_target"] == "python_asyncio"
        info["supported"] = True
        assert omnivm_mod.owner_dispatch_target_status("python_asyncio")["supported"] is False

    def test_owner_dispatch_target_status_accepts_common_aliases(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{"python_asyncio":{"supported":false},'
            b'"javascript_event_loop":{"supported":false},'
            b'"java_executor":{"supported":false},'
            b'"ruby_fiber_thread":{"supported":false}}}}'
        )
        assert omnivm_mod.owner_dispatch_target_status("asyncio") == {
            "supported": False,
            "requested_target": "asyncio",
            "target": "python_asyncio",
        }
        assert omnivm_mod.owner_dispatch_target_status("python loop") == {
            "supported": False,
            "requested_target": "python loop",
            "target": "python_asyncio",
        }
        assert omnivm_mod.owner_dispatch_target_status("JavaScript") == {
            "supported": False,
            "requested_target": "JavaScript",
            "target": "javascript_event_loop",
        }
        assert omnivm_mod.owner_dispatch_target_status("nodejs") == {
            "supported": False,
            "requested_target": "nodejs",
            "target": "javascript_event_loop",
        }
        assert omnivm_mod.owner_dispatch_target_status("java-executor") == {
            "supported": False,
            "requested_target": "java-executor",
            "target": "java_executor",
        }
        assert omnivm_mod.owner_dispatch_target_status("thread") == {
            "supported": False,
            "requested_target": "thread",
            "target": "ruby_fiber_thread",
        }
        assert omnivm_mod.owner_dispatch_target_status("ruby fiber") == {
            "supported": False,
            "requested_target": "ruby fiber",
            "target": "ruby_fiber_thread",
        }

    def test_owner_dispatch_target_status_requires_target_map(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.owner_dispatch_target_status("asyncio")
        assert "owner_dispatch_targets capability" in str(ctx.exception)
        assert ctx.exception.boundary_path == "owner_dispatch_target"
        assert ctx.exception.details["target"] == "python_asyncio"
        assert ctx.exception.details["requested_target"] == "asyncio"
        assert ctx.exception.details["owner_dispatch"]["owner_dispatch_supported"] is False

    def test_owner_dispatch_target_status_requires_known_target(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{"java_executor":{"supported":false}}}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.owner_dispatch_target_status("python_asyncio")
        assert "known targets: java_executor" in str(ctx.exception)
        assert ctx.exception.boundary_path == "owner_dispatch_target"
        assert ctx.exception.details["target"] == "python_asyncio"
        assert ctx.exception.details["known_targets"] == ["java_executor"]
        assert ctx.exception.details["owner_dispatch_targets"] == {"java_executor": {"supported": False}}
        assert ctx.exception.details["owner_dispatch_target"]["target"] == "python_asyncio"
        assert ctx.exception.details["owner_dispatch_target"]["known_targets"] == ["java_executor"]

    def test_owner_dispatch_target_status_unknown_alias_reports_requested_target(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{"java_executor":{"supported":false}}}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.owner_dispatch_target_status("asyncio")
        assert "owner dispatch target 'python_asyncio'" in str(ctx.exception)
        assert ctx.exception.boundary_path == "owner_dispatch_target"
        assert ctx.exception.details["target"] == "python_asyncio"
        assert ctx.exception.details["requested_target"] == "asyncio"
        assert ctx.exception.details["owner_dispatch_target"]["requested_target"] == "asyncio"

    def test_assert_owner_dispatch_target_supported_reports_diagnostic(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{"java_executor":{'
            b'"supported":false,"owner_kind":"java_executor",'
            b'"required_capability":"resubmit callbacks to executor",'
            b'"current_behavior":"caller-managed",'
            b'"diagnostic":"executor caller-managed"}}}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.assert_owner_dispatch_target_supported("java_executor", "reactor startup")
        assert "reactor startup: owner dispatch target java_executor unsupported" in str(ctx.exception)
        assert "executor caller-managed" in str(ctx.exception)
        assert ctx.exception.boundary_path == "owner_dispatch_target"
        assert ctx.exception.details["target"] == "java_executor"
        assert ctx.exception.details["owner_dispatch_target"]["target"] == "java_executor"
        assert ctx.exception.details["owner_dispatch_target"]["requested_target"] == "java_executor"
        assert ctx.exception.details["owner_dispatch_target"]["owner_kind"] == "java_executor"
        assert ctx.exception.details["owner_dispatch_target"]["required_capability"] == "resubmit callbacks to executor"
        assert ctx.exception.details["owner_dispatch_target"]["current_behavior"] == "caller-managed"
        assert ctx.exception.details["owner_dispatch_target"]["diagnostic"] == "executor caller-managed"

    def test_assert_owner_dispatch_target_supported_reports_alias_diagnostic(self):
        self.mock_lib.OmniStatus.return_value = (
            b'{"thread_affinity":{"mode":"diagnostic_only",'
            b'"owner_dispatch_supported":false,'
            b'"owner_dispatch_targets":{"java_executor":{'
            b'"supported":false,"diagnostic":"executor caller-managed"}}}}'
        )
        with self.assertRaises(omnivm_mod.RuntimeError) as ctx:
            omnivm_mod.assert_owner_dispatch_target_supported("java", "reactor startup")
        assert "reactor startup: owner dispatch target java_executor unsupported" in str(ctx.exception)
        assert ctx.exception.boundary_path == "owner_dispatch_target"
        assert ctx.exception.details["target"] == "java_executor"
        assert ctx.exception.details["requested_target"] == "java"
        assert ctx.exception.details["owner_dispatch_target"]["target"] == "java_executor"
        assert ctx.exception.details["owner_dispatch_target"]["requested_target"] == "java"

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
