#!/usr/bin/env python3
"""CPython-hosted libomnivm stress checks.

This is intentionally separate from cmd/stresstest, which boots OmniVM as a Go
process. These checks keep CPython as process host, load libomnivm through
ctypes, and exercise nested runtime callbacks from that topology.
"""

import os
import struct
import subprocess
import sys

sys.path.insert(0, "/build/pyomnivm")

import omnivm


PASSED = 0
FAILED = 0


def check(name, fn):
    global PASSED, FAILED
    print(f"[LIB TEST] {name}... ", end="", flush=True)
    try:
        fn()
    except Exception as exc:
        FAILED += 1
        print(f"FAILED: {exc}", flush=True)
    else:
        PASSED += 1
        print("PASSED", flush=True)


def expect(got, want):
    if str(got) != str(want):
        raise AssertionError(f"got {got!r}, want {want!r}")


def test_simple_reentry():
    expect(omnivm.call("javascript", "2 + 2"), "4")


def test_js_calls_host_python():
    expect(omnivm.call("javascript", "omnivm.call('python', '6 * 7')"), "42")


def test_deep_chain():
    code = (
        "parseInt(omnivm.call('ruby', "
        "\"OmniVM.call('java', '21 + 21')\")) + 18"
    )
    expect(omnivm.call("javascript", code), "60")


def test_java_to_python_to_js():
    code = (
        'omnivm.OmniVM.call("python", '
        '"omnivm.call(\\\'javascript\\\', \\\'7 * 8\\\')")'
    )
    expect(omnivm.call("java", code), "56")


def test_ruby_to_java_to_python():
    code = (
        "OmniVM.call('java', "
        "'omnivm.OmniVM.call(\"python\", \"9 * 9\")')"
    )
    expect(omnivm.call("ruby", code), "81")


def test_error_propagates():
    try:
        omnivm.call("javascript", "throw new Error('lib stress boom')")
    except omnivm.RuntimeError as exc:
        if "lib stress boom" not in str(exc):
            raise
    else:
        raise AssertionError("expected RuntimeError")


def test_buffer_bridge():
    data = bytes(range(64)) * 128
    omnivm.set_buffer("libstress", data, 0)
    js_sum = omnivm.call(
        "javascript",
        """
        var buf = omnivm.getBuffer('libstress');
        var arr = new Uint8Array(buf);
        var sum = 0;
        for (var i = 0; i < arr.length; i++) sum += arr[i];
        sum;
        """,
    )
    expect(js_sum, str(sum(data)))

    omnivm.call(
        "javascript",
        """
        var ab = new ArrayBuffer(8);
        new Float64Array(ab)[0] = 2.5;
        omnivm.setBuffer('libstress-pi', ab, 4);
        'ok';
        """,
    )
    pi = omnivm.get_buffer("libstress-pi")
    if pi is None:
        raise AssertionError("missing libstress-pi buffer")
    expect(struct.unpack("d", bytes(pi))[0], 2.5)
    omnivm.release_buffer("libstress")
    omnivm.release_buffer("libstress-pi")


def test_repeated_crossings():
    for i in range(100):
        expr = f"parseInt(omnivm.call('ruby', '{i} + 1')) + 1"
        expect(omnivm.call("javascript", expr), str(i + 2))


def test_python_first_process_survives_plain_python():
    expect(sys.implementation.name, "cpython")
    expect(omnivm.call("python", "__import__('sys').version_info.major"), "3")


def test_fork_guard():
    code = """
import os
import sys
sys.path.insert(0, '/build/pyomnivm')
import omnivm
omnivm.init_runtimes(['javascript', 'java', 'ruby'])
pid = os.fork()
if pid == 0:
    os._exit(0)
_, status = os.waitpid(pid, 0)
omnivm.shutdown()
raise SystemExit(os.waitstatus_to_exitcode(status))
"""
    proc = subprocess.run([sys.executable, "-c", code], check=False)
    if proc.returncode != 71:
        raise AssertionError(f"fork guard exit {proc.returncode}, want 71")


def main():
    print("=== libomnivm CPython Host Stress Tests ===")
    omnivm.init_runtimes(["javascript", "java", "ruby"])
    try:
        check("Simple re-entry", test_simple_reentry)
        check("JS calls host Python", test_js_calls_host_python)
        check("Deep JS -> Ruby -> Java chain", test_deep_chain)
        check("Java -> Python -> JS chain", test_java_to_python_to_js)
        check("Ruby -> Java -> Python chain", test_ruby_to_java_to_python)
        check("Error propagation", test_error_propagates)
        check("Buffer bridge", test_buffer_bridge)
        check("Repeated crossings", test_repeated_crossings)
        check("Python-first host stays CPython", test_python_first_process_survives_plain_python)
    finally:
        omnivm.shutdown()

    check("Fork guard", test_fork_guard)
    print(f"\nResults: {PASSED} passed, {FAILED} failed")
    return 1 if FAILED else 0


if __name__ == "__main__":
    raise SystemExit(main())
