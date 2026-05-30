"""PolyScript integration for Python applications.

The package deliberately keeps CPython semantics intact. Existing ``.py`` code
runs under the normal interpreter path; ``.poly`` files are compiled to OmniVM
manifests only when imported through the hook or run explicitly.
"""

from __future__ import annotations

import hashlib
import importlib.abc
import importlib.machinery
import importlib.util
import os
from pathlib import Path
import shlex
import subprocess
import sys
from types import ModuleType
from typing import Iterable, Optional

__all__ = [
    "PolyScriptError",
    "PolyScriptFinder",
    "PolyScriptLoader",
    "compile_manifest",
    "install",
    "is_enabled",
    "run_manifest",
    "run_poly",
    "uninstall",
]


class PolyScriptError(RuntimeError):
    """Raised when a PolyScript compile or manifest run command fails."""


class PolyScriptLoader(importlib.abc.Loader):
    """Import loader for side-effect-oriented ``.poly`` modules."""

    def __init__(self, path: Path):
        self.path = path

    def create_module(self, spec):  # pragma: no cover - default is correct
        return None

    def exec_module(self, module: ModuleType) -> None:
        result = run_poly(self.path)
        module.__file__ = str(self.path)
        module.__loader__ = self
        module.__poly_manifest__ = str(result.manifest_path)
        module.__poly_result__ = result
        module.__all__ = []


class PolyScriptFinder(importlib.abc.MetaPathFinder):
    """Find ``module.poly`` and ``package/__init__.poly`` on ``sys.path``."""

    def find_spec(self, fullname, path=None, target=None):
        name = fullname.rpartition(".")[2]
        search_paths = path if path is not None else sys.path

        for base in search_paths:
            if not base:
                base = os.getcwd()
            root = Path(base)

            module_path = root / f"{name}.poly"
            if module_path.is_file():
                loader = PolyScriptLoader(module_path)
                return importlib.util.spec_from_file_location(
                    fullname,
                    module_path,
                    loader=loader,
                )

            package_path = root / name / "__init__.poly"
            if package_path.is_file():
                loader = PolyScriptLoader(package_path)
                return importlib.machinery.ModuleSpec(
                    fullname,
                    loader,
                    origin=str(package_path),
                    is_package=True,
                )

        return None


class PolyScriptRunResult:
    """Result returned by ``run_poly``."""

    def __init__(self, manifest_path: Path, process: subprocess.CompletedProcess):
        self.manifest_path = manifest_path
        self.process = process

    @property
    def stdout(self) -> str:
        return self.process.stdout

    @property
    def stderr(self) -> str:
        return self.process.stderr

    @property
    def returncode(self) -> int:
        return self.process.returncode


_FINDER = PolyScriptFinder()


def is_enabled() -> bool:
    """Return true when running through the PolyScript Python entrypoint."""

    return os.environ.get("POLYSCRIPT_PYTHON") == "1"


def install() -> PolyScriptFinder:
    """Install the ``.poly`` import hook once and return the finder."""

    if not any(finder is _FINDER for finder in sys.meta_path):
        sys.meta_path.insert(0, _FINDER)
    return _FINDER


def uninstall() -> None:
    """Remove the ``.poly`` import hook if it is installed."""

    sys.meta_path[:] = [finder for finder in sys.meta_path if finder is not _FINDER]


def compile_manifest(source: os.PathLike | str, output: os.PathLike | str | None = None) -> Path:
    """Compile a ``.poly`` file to an OmniVM dispatch manifest."""

    source_path = Path(source)
    if output is None:
        output_path = _default_manifest_path(source_path)
    else:
        output_path = Path(output)

    output_path.parent.mkdir(parents=True, exist_ok=True)
    command = _command_from_env("POLYSCRIPT_COMPILER", ["polyc"])
    result = subprocess.run(
        [*command, str(source_path), "-o", str(output_path)],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if result.returncode != 0:
        raise PolyScriptError(_format_failure("compile", result))
    return output_path


def run_manifest(manifest: os.PathLike | str) -> subprocess.CompletedProcess:
    """Run an OmniVM manifest with the configured manifest runner."""

    command = _command_from_env("POLYSCRIPT_MANIFEST_RUNNER", ["manifest-runner"])
    result = subprocess.run(
        [*command, str(manifest)],
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if result.returncode != 0:
        raise PolyScriptError(_format_failure("run", result))
    return result


def run_poly(source: os.PathLike | str) -> PolyScriptRunResult:
    """Compile and run a ``.poly`` file."""

    manifest_path = compile_manifest(source)
    return PolyScriptRunResult(manifest_path, run_manifest(manifest_path))


def _command_from_env(name: str, default: Iterable[str]) -> list[str]:
    raw = os.environ.get(name)
    if not raw:
        return list(default)
    return shlex.split(raw)


def _default_manifest_path(source: Path) -> Path:
    cache_root = Path(os.environ.get("POLYSCRIPT_CACHE_DIR", ".polyscript-cache"))
    try:
        stat = source.stat()
        fingerprint_input = f"{source.resolve()}:{stat.st_mtime_ns}:{stat.st_size}"
    except FileNotFoundError:
        fingerprint_input = str(source)
    fingerprint = hashlib.sha256(fingerprint_input.encode("utf-8")).hexdigest()[:16]
    return cache_root / f"{source.stem}-{fingerprint}.manifest.json"


def _format_failure(phase: str, result: subprocess.CompletedProcess) -> str:
    details = result.stderr.strip() or result.stdout.strip() or "no output"
    return f"PolyScript {phase} failed with exit code {result.returncode}: {details}"
