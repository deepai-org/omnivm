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
import json
import os
from pathlib import Path
import shutil
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
    "load_poly_module",
    "run_manifest",
    "run_poly",
    "uninstall",
]


class PolyScriptError(RuntimeError):
    """Raised when a PolyScript compile or manifest run command fails."""


class PolyScriptFunction:
    """Python-callable proxy for a retained manifest ``func_def``."""

    def __init__(self, module_id: str, name: str, params: list[dict] | None = None):
        self.__module_id__ = module_id
        self.__name__ = name
        self.__qualname__ = name
        self.__poly_params__ = params or []

    def __call__(self, *args, **kwargs):
        import omnivm

        return omnivm.manifest_call(self.__module_id__, self.__name__, self._bind_args(args, kwargs))

    def _bind_args(self, args, kwargs):
        if not kwargs:
            return args
        params = self.__poly_params__
        if not params:
            raise TypeError(f"{self.__name__}() does not accept keyword arguments")
        if len(args) > len(params):
            raise TypeError(f"{self.__name__}() takes at most {len(params)} positional arguments")

        values = list(args)
        consumed = set()
        for index, param in enumerate(params[len(values):], start=len(values)):
            name = param.get("name")
            if name in kwargs:
                values.append(kwargs[name])
                consumed.add(name)
            elif "defaultValue" in param:
                values.append(_manifest_default_value(param["defaultValue"]))
            else:
                break

        extra = set(kwargs) - consumed
        if extra:
            names = ", ".join(sorted(extra))
            raise TypeError(f"{self.__name__}() got unexpected keyword argument(s): {names}")
        return tuple(values)

    def __repr__(self) -> str:  # pragma: no cover - diagnostic only
        return f"<polyscript function {self.__name__}>"


class PolyScriptLoader(importlib.abc.Loader):
    """Import loader for ``.poly`` modules."""

    def __init__(self, path: Path):
        self.path = path

    def create_module(self, spec):  # pragma: no cover - default is correct
        return None

    def exec_module(self, module: ModuleType) -> None:
        result = load_poly_module(self.path, module)
        module.__file__ = str(self.path)
        module.__loader__ = self
        module.__poly_manifest__ = str(result.manifest_path)
        module.__poly_result__ = result


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

    def __init__(
        self,
        manifest_path: Path,
        process: subprocess.CompletedProcess,
        module_id: str | None = None,
    ):
        self.manifest_path = manifest_path
        self.process = process
        self.module_id = module_id

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

    if _should_run_manifest_in_process():
        return _run_manifest_in_process(Path(manifest))

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


def load_poly_module(source: os.PathLike | str, module: ModuleType) -> PolyScriptRunResult:
    """Compile, run, and expose callable manifest functions on ``module``."""

    manifest_path = compile_manifest(source)
    module_id = _module_id(module.__name__, manifest_path)
    if _should_run_manifest_in_process():
        process = _load_manifest_module_in_process(module_id, manifest_path)
        functions = _manifest_functions(manifest_path)
        function_names = list(functions)
        module.__all__ = function_names
        module.__poly_exports__ = function_names
        for name, meta in functions.items():
            setattr(module, name, PolyScriptFunction(module_id, name, meta.get("params", [])))
    else:
        process = run_manifest(manifest_path)
        module.__all__ = []
        module.__poly_exports__ = []
    return PolyScriptRunResult(manifest_path, process, module_id=module_id)


def _should_run_manifest_in_process() -> bool:
    if os.environ.get("POLYSCRIPT_MANIFEST_RUNNER"):
        return False
    if os.environ.get("POLYSCRIPT_IN_PROCESS") == "0":
        return False
    return is_enabled()


def _run_manifest_in_process(manifest: Path) -> subprocess.CompletedProcess:
    import omnivm

    _ensure_omnivm_initialized(omnivm, manifest)

    try:
        stdout = omnivm.run_manifest(manifest)
    except Exception as exc:
        raise PolyScriptError(f"PolyScript run failed during in-process manifest execution: {exc}") from exc
    return subprocess.CompletedProcess(
        ["omnivm.run_manifest", str(manifest)],
        0,
        stdout=stdout,
        stderr="",
    )


def _load_manifest_module_in_process(module_id: str, manifest: Path) -> subprocess.CompletedProcess:
    import omnivm

    _ensure_omnivm_initialized(omnivm, manifest)

    try:
        stdout = omnivm.load_manifest_module(module_id, manifest)
    except Exception as exc:
        raise PolyScriptError(f"PolyScript import failed during in-process manifest load: {exc}") from exc
    return subprocess.CompletedProcess(
        ["omnivm.load_manifest_module", module_id, str(manifest)],
        0,
        stdout=stdout,
        stderr="",
    )


def _ensure_omnivm_initialized(omnivm, manifest: Path) -> None:
    try:
        omnivm.status()
    except Exception:
        try:
            omnivm.init_runtimes(_configured_runtimes(manifest))
        except Exception as exc:
            raise PolyScriptError(f"PolyScript run failed during libomnivm initialization: {exc}") from exc


def _configured_runtimes(manifest: Path) -> list[str]:
    configured = os.environ.get("POLYSCRIPT_RUNTIMES")
    if configured:
        configured = configured.strip()
        if configured == "infer":
            return _manifest_runtimes(manifest)
        return [name.strip() for name in configured.split(",") if name.strip()]
    return ["javascript", "java", "ruby"]


def _manifest_runtimes(path: Path) -> list[str]:
    with path.open("r", encoding="utf-8") as f:
        manifest = json.load(f)
    runtimes: set[str] = set()
    _collect_runtimes(manifest, runtimes)
    return [name for name in ("go", "javascript", "java", "ruby") if name in runtimes]


def _collect_runtimes(value, out: set[str]) -> None:
    if isinstance(value, dict):
        runtime = value.get("runtime")
        if runtime in {"go", "javascript", "java", "ruby"}:
            out.add(runtime)
        for child in value.values():
            _collect_runtimes(child, out)
    elif isinstance(value, list):
        for child in value:
            _collect_runtimes(child, out)


def _command_from_env(name: str, default: Iterable[str]) -> list[str]:
    raw = os.environ.get(name)
    if not raw:
        return list(default)
    return shlex.split(raw)


def _default_manifest_path(source: Path) -> Path:
    cache_root = Path(os.environ.get("POLYSCRIPT_CACHE_DIR", ".polyscript-cache"))
    try:
        source_bytes = source.read_bytes()
        source_hash = hashlib.sha256(source_bytes).hexdigest()
        source_id = str(source.resolve())
    except FileNotFoundError:
        source_hash = "missing"
        source_id = str(source)
    fingerprint_input = json.dumps(
        {
            "compiler": _compiler_cache_identity(),
            "source": source_hash,
            "path": source_id,
        },
        sort_keys=True,
    )
    fingerprint = hashlib.sha256(fingerprint_input.encode("utf-8")).hexdigest()[:16]
    return cache_root / f"{source.stem}-{fingerprint}.manifest.json"


def _compiler_cache_identity() -> str:
    command = _command_from_env("POLYSCRIPT_COMPILER", ["polyc"])
    executable = shutil.which(command[0]) if command else None
    parts = ["\0".join(command)]
    if executable:
        try:
            path = Path(executable)
            stat = path.stat()
            parts.append(f"{path.resolve()}:{stat.st_mtime_ns}:{stat.st_size}")
        except OSError:
            parts.append(executable)
    version = os.environ.get("POLYSCRIPT_COMPILER_VERSION")
    if version:
        parts.append(version)
    return "\0".join(parts)


def _module_id(fullname: str, manifest: Path) -> str:
    return f"{fullname}:{manifest.resolve()}"


def _manifest_functions(path: Path) -> dict[str, dict]:
    with path.open("r", encoding="utf-8") as f:
        manifest = json.load(f)
    functions: dict[str, dict] = {}
    for op in manifest.get("ops", []):
        if isinstance(op, dict) and op.get("op") == "func_def":
            name = op.get("name")
            if isinstance(name, str) and name and name not in functions:
                functions[name] = {"params": op.get("params", [])}
    return functions


def _manifest_default_value(value):
    if isinstance(value, dict) and value.get("kind") == "literal":
        return value.get("value")
    return value


def _format_failure(phase: str, result: subprocess.CompletedProcess) -> str:
    details = result.stderr.strip() or result.stdout.strip() or "no output"
    return f"PolyScript {phase} failed with exit code {result.returncode}: {details}"
