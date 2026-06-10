#!/usr/bin/env python3
"""Prefork fork-safety test for the Rust runtime (the gunicorn posture).

The parent imports the omnivm package but does NOT initialize; each forked
child initializes the Rust runtime post-fork and runs a compile+dlopen+eval
round trip. This is the production path for libomnivm deployments: the tokio
runtime must be created post-fork (it is — creation is lazy and the default
executor owns no threads), and concurrent children must not corrupt the
shared artifact cache (atomic temp+rename publishes, cargo's own locks).
"""
import os
import sys

sys.path.insert(0, "/build/pyomnivm")


def child(index: int) -> None:
    import omnivm

    omnivm.init_runtimes(["rust"])
    out = omnivm.call("rust", "6 * 7")
    if str(out).strip() != "42":
        print(f"child {index}: unexpected result {out!r}", file=sys.stderr)
        os._exit(1)
    # A second call exercises the warm cache path in the same child.
    out = omnivm.call("rust", "[1, 2, 3].iter().sum::<i64>()")
    if str(out).strip() != "6":
        print(f"child {index}: warm-cache result {out!r}", file=sys.stderr)
        os._exit(1)
    os._exit(0)


def main() -> int:
    pids = []
    for i in range(2):
        pid = os.fork()
        if pid == 0:
            try:
                child(i)
            except BaseException as exc:  # noqa: BLE001 — child must not unwind into parent code
                print(f"child {i}: {exc}", file=sys.stderr)
                os._exit(1)
        pids.append(pid)

    failures = 0
    for pid in pids:
        _, status = os.waitpid(pid, 0)
        if os.WIFSIGNALED(status):
            print(f"child {pid} died with signal {os.WTERMSIG(status)}", file=sys.stderr)
            failures += 1
        elif os.WEXITSTATUS(status) != 0:
            print(f"child {pid} exited {os.WEXITSTATUS(status)}", file=sys.stderr)
            failures += 1

    print("prefork rust test:", "FAIL" if failures else "PASS")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
