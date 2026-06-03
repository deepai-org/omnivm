#!/bin/bash
# Runs CPython-hosted libomnivm stress checks.

set -euo pipefail

IMAGE="${OMNIVM_IMAGE:-omnivm:latest}"
PYTHON_BIN="${OMNIVM_PYTHON_BIN:-python3.14}"

docker run --rm --entrypoint "$PYTHON_BIN" "$IMAGE" /build/scripts/test-libomnivm-stress.py "$@"
