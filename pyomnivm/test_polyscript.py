"""Tests for the PolyScript Python compatibility layer."""

import importlib
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

import polyscript


class TestPolyScriptCommands(unittest.TestCase):
    def tearDown(self):
        polyscript.uninstall()

    @patch.dict(os.environ, {"POLYSCRIPT_COMPILER": "node /app/polyc", "POLYSCRIPT_CACHE_DIR": "/tmp/ps-cache"})
    @patch("subprocess.run")
    def test_compile_manifest_uses_configured_compiler(self, run):
        run.return_value.returncode = 0
        run.return_value.stdout = ""
        run.return_value.stderr = ""

        out = polyscript.compile_manifest("/app/demo.poly")

        self.assertEqual(out.parent, Path("/tmp/ps-cache"))
        self.assertTrue(out.name.startswith("demo-"))
        self.assertTrue(out.name.endswith(".manifest.json"))
        self.assertEqual(run.call_args[0][0][:2], ["node", "/app/polyc"])
        self.assertIn("/app/demo.poly", run.call_args[0][0])
        self.assertIn("-o", run.call_args[0][0])

    @patch.dict(os.environ, {"POLYSCRIPT_MANIFEST_RUNNER": "manifest-runner --trace"})
    @patch("subprocess.run")
    def test_run_manifest_uses_configured_runner(self, run):
        run.return_value.returncode = 0
        run.return_value.stdout = "ok"
        run.return_value.stderr = ""

        result = polyscript.run_manifest("/tmp/demo.manifest.json")

        self.assertEqual(result.stdout, "ok")
        self.assertEqual(run.call_args[0][0], ["manifest-runner", "--trace", "/tmp/demo.manifest.json"])

    @patch("subprocess.run")
    def test_compile_failure_is_actionable(self, run):
        run.return_value.returncode = 2
        run.return_value.stdout = ""
        run.return_value.stderr = "parse error"

        with self.assertRaises(polyscript.PolyScriptError) as ctx:
            polyscript.compile_manifest("/tmp/bad.poly")

        self.assertIn("compile failed", str(ctx.exception))
        self.assertIn("parse error", str(ctx.exception))


class TestPolyScriptImportHook(unittest.TestCase):
    def tearDown(self):
        polyscript.uninstall()
        sys.modules.pop("demo_poly_module", None)

    def test_install_is_idempotent(self):
        finder = polyscript.install()
        polyscript.install()
        self.assertEqual(sum(1 for item in sys.meta_path if item is finder), 1)

    @patch("polyscript.run_poly")
    def test_import_poly_module_runs_manifest(self, run_poly):
        class Result:
            manifest_path = Path("/tmp/demo.manifest.json")
            stdout = "ok"
            stderr = ""
            returncode = 0

        run_poly.return_value = Result()

        with tempfile.TemporaryDirectory() as tmp:
            Path(tmp, "demo_poly_module.poly").write_text('console.log("hello")\n')
            sys.path.insert(0, tmp)
            try:
                polyscript.install()
                module = importlib.import_module("demo_poly_module")
            finally:
                sys.path.remove(tmp)

        run_poly.assert_called_once()
        self.assertEqual(module.__poly_manifest__, "/tmp/demo.manifest.json")
        self.assertIs(module.__poly_result__, run_poly.return_value)


class TestPolyScriptMode(unittest.TestCase):
    @patch.dict(os.environ, {"POLYSCRIPT_PYTHON": "1"})
    def test_is_enabled(self):
        self.assertTrue(polyscript.is_enabled())

    @patch.dict(os.environ, {}, clear=True)
    def test_is_not_enabled_by_default(self):
        self.assertFalse(polyscript.is_enabled())
