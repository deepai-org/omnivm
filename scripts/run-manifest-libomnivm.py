#!/usr/bin/env python3
import json
import os
import sys

sys.path.insert(0, "/build/pyomnivm")

import omnivm


def _collect_runtimes(value, out):
    if isinstance(value, dict):
        runtime = value.get("runtime")
        if runtime in {"go", "javascript", "java", "ruby"}:
            out.add(runtime)
        for child in value.values():
            _collect_runtimes(child, out)
    elif isinstance(value, list):
        for child in value:
            _collect_runtimes(child, out)


def _manifest_runtimes(path):
    with open(path, "r", encoding="utf-8") as f:
        manifest = json.load(f)
    runtimes = set()
    _collect_runtimes(manifest, runtimes)
    return [name for name in ("go", "javascript", "java", "ruby") if name in runtimes]


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: run-manifest-libomnivm.py <manifest.json>", file=sys.stderr)
        return 2

    omnivm.init_runtimes(_manifest_runtimes(sys.argv[1]))
    try:
        omnivm.run_manifest(sys.argv[1])
    finally:
        omnivm.shutdown()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
