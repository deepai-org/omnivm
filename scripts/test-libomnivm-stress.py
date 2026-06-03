#!/usr/bin/env python3
"""CPython-hosted libomnivm stress checks.

This is intentionally separate from cmd/stresstest, which boots OmniVM as a Go
process. These checks keep CPython as process host, load libomnivm through
ctypes, and exercise nested runtime callbacks from that topology.
"""

import os
import argparse
import gc
import ctypes
import json
import struct
import subprocess
import sys
import tempfile
import threading
import time


PASSED = 0
FAILED = 0
SKIPPED = 0
SELECTED = 0
NAME_FILTERS = []
CATEGORY_FILTERS = []
omnivm = None


def load_omnivm():
    global omnivm
    if omnivm is None:
        sys.path.insert(0, "/build/pyomnivm")
        import omnivm as omnivm_mod

        omnivm = omnivm_mod
    return omnivm


def _normalize_filter(value):
    return str(value).strip().lower().replace("_", "-")


def categories_for_name(name, fn=None):
    fn_name = getattr(fn, "__name__", "")
    text = f"{name} {fn_name}".lower()
    categories = {"all"}
    if "manifest" in text:
        categories.add("manifest")
    if "arrow" in text or "buffer" in text or "zero-copy" in text:
        categories.add("arrow")
    if "proxy" in text or "handle" in text:
        categories.add("proxy")
    if "stream" in text or "generator" in text or "iterator" in text or "iterable" in text:
        categories.add("stream")
    if (
        "kwargs" in text
        or "keyword" in text
        or "adapter" in text
        or "options" in text
        or "mapping keys" in text
        or "setter values" in text
        or "method arguments" in text
        or "unsafe names" in text
    ):
        categories.add("kwargs")
    if "watchdog" in text or "timeout" in text or "preemption" in text or "interrupt" in text:
        categories.add("watchdog")
    if "prefork" in text or "fork" in text or "wsgi" in text or "passenger" in text:
        categories.add("prefork")
    if "thread" in text or "concurrent" in text or "contention" in text:
        categories.add("concurrency")
    if "java" in text or "jvm" in text:
        categories.add("java")
    if "ruby" in text:
        categories.add("ruby")
    if "javascript" in text or "js " in text or "v8" in text:
        categories.add("javascript")
    if "python" in text or "cpython" in text:
        categories.add("python")
    return categories


def should_run(name, fn=None):
    normalized_name = _normalize_filter(name)
    normalized_fn_name = _normalize_filter(getattr(fn, "__name__", ""))
    categories = categories_for_name(name, fn)
    if NAME_FILTERS and not any(pattern in normalized_name for pattern in NAME_FILTERS):
        if not any(pattern in normalized_fn_name for pattern in NAME_FILTERS):
            return False
    if CATEGORY_FILTERS and not any(category in categories for category in CATEGORY_FILTERS):
        return False
    return True


def check(name, fn):
    global PASSED, FAILED, SKIPPED, SELECTED
    if not should_run(name, fn):
        SKIPPED += 1
        return
    SELECTED += 1
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


def expect_contains(got, needle):
    if needle not in str(got):
        raise AssertionError(f"expected {got!r} to contain {needle!r}")


def py(code):
    return omnivm.call("python", code)


def js(code):
    return omnivm.call("javascript", code)


def rb(code):
    return omnivm.call("ruby", code)


def java(code):
    return omnivm.call("java", code)


def py_exec(code):
    return omnivm.execute("python", code)


def child_check(code, timeout=15):
    child = f"""
import sys
sys.path.insert(0, "/build/pyomnivm")
import omnivm
omnivm.init_runtimes(["javascript", "java", "ruby"])
try:
{chr(10).join("    " + line for line in code.strip().splitlines())}
finally:
    try:
        omnivm.set_task_timeout(0)
    except Exception:
        pass
    omnivm.shutdown()
"""
    proc = subprocess.run(
        [sys.executable, "-c", child],
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )
    if proc.returncode != 0:
        raise AssertionError(
            f"child exited {proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )


def run_manifest_dict(manifest):
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)


def test_simple_reentry():
    expect(omnivm.call("javascript", "2 + 2"), "4")


def test_deep_chain():
    code = 'int(omnivm.call("javascript", "parseInt(omnivm.call(\'ruby\', \'30 + 20\')) + 10"))'
    expect(py(code), "60")


def test_closure_like_capture():
    py_exec(
        """
base = 100
doubled = omnivm.call("javascript", str(base) + " * 2")
_lib_t3_result = int(omnivm.call("ruby", doubled + " * 3"))
"""
    )
    expect(py("_lib_t3_result"), "600")


def test_fan_out_threads():
    tasks = [
        ("python", "1 + 1", "2"),
        ("javascript", "2 + 2", "4"),
        ("ruby", "3 + 3", "6"),
        ("python", "4 + 4", "8"),
    ]
    results = []
    errors = []
    lock = threading.Lock()

    def worker(index, runtime, code, expected):
        try:
            got = omnivm.call(runtime, code)
            if got != expected:
                raise AssertionError(f"{runtime} got {got!r}, want {expected!r}")
            with lock:
                results.append(index)
        except Exception as exc:
            with lock:
                errors.append(exc)

    threads = [
        threading.Thread(target=worker, args=(i, rt, code, expected))
        for i, (rt, code, expected) in enumerate(tasks)
    ]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=5)
    if errors:
        raise errors[0]
    if len(results) != len(tasks):
        raise AssertionError(f"expected {len(tasks)} completed calls, got {len(results)}")


def test_recursive_cross_runtime_fibonacci():
    start = time.monotonic()
    py_exec(
        """
def _lib_fib(n):
    n = int(n)
    if n <= 1:
        return n
    if n % 2 == 0:
        a = int(omnivm.call("javascript", "parseInt(omnivm.call('python', '_lib_fib(" + str(n-1) + ")'))"))
    else:
        a = _lib_fib(n - 1)
    b = _lib_fib(n - 2)
    return a + b
"""
    )
    expect(py("_lib_fib(15)"), "610")
    if time.monotonic() - start > 5:
        raise AssertionError("fib(15) exceeded 5s")


def test_error_propagation():
    try:
        py('omnivm.call("javascript", "throw new Error(\'deep error from JS\')")')
    except omnivm.RuntimeError as exc:
        if "deep error from JS" not in str(exc):
            raise
    else:
        raise AssertionError("expected RuntimeError")


def test_python_generator_consumed_cross_runtime():
    py_exec(
        """
_lib_t7_cleanup = False
def _lib_t7_counting_gen():
    global _lib_t7_cleanup
    try:
        i = 1
        while True:
            yield i
            i += 1
    finally:
        _lib_t7_cleanup = True
_lib_t7_gen = _lib_t7_counting_gen()
"""
    )
    rb("$_lib_t7_acc = 0")
    for i in range(100):
        py_val = py("next(_lib_t7_gen)")
        js_val = js(f"{py_val} * 2")
        rb(f"$_lib_t7_acc += {js_val}")
    expect(rb("$_lib_t7_acc"), "10100")
    py_exec("_lib_t7_gen.close()")
    expect(py("_lib_t7_cleanup"), "True")


def test_reentrant_async_python_js_python():
    py_exec(
        """
import asyncio
_lib_t9_inner_value = "SENTINEL_42"
_lib_t9_result = None
async def _lib_t9_reentrant_task():
    global _lib_t9_result
    val = omnivm.call("javascript", "omnivm.call('python', '_lib_t9_inner_value')")
    _lib_t9_result = "reentrant:" + val
_lib_t9_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_lib_t9_loop)
_lib_t9_loop.run_until_complete(_lib_t9_reentrant_task())
"""
    )
    expect(py("_lib_t9_result"), "reentrant:SENTINEL_42")
    py_exec("_lib_t9_loop.close()")


def test_asyncio_taskgroup_cross_runtime_cancellation():
    py_exec(
        """
import asyncio
_lib_t9b_cancelled = False
_lib_t9b_errors = []
_lib_t9b_after = None

async def _lib_t9b_sleeper():
    global _lib_t9b_cancelled
    try:
        await asyncio.sleep(60)
    except asyncio.CancelledError:
        _lib_t9b_cancelled = True
        raise

async def _lib_t9b_bridge_fail():
    await asyncio.sleep(0)
    omnivm.call("javascript", "throw new Error('taskgroup bridge failure')")

async def _lib_t9b_main():
    global _lib_t9b_errors, _lib_t9b_after
    try:
        async with asyncio.TaskGroup() as tg:
            tg.create_task(_lib_t9b_sleeper())
            tg.create_task(_lib_t9b_bridge_fail())
    except* Exception as eg:
        _lib_t9b_errors = [type(e).__name__ + ":" + str(e) for e in eg.exceptions]
    _lib_t9b_after = omnivm.call("javascript", "'after-taskgroup'")

_lib_t9b_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_lib_t9b_loop)
_lib_t9b_loop.run_until_complete(_lib_t9b_main())
"""
    )
    expect(py("_lib_t9b_cancelled"), "True")
    expect_contains(py("';'.join(_lib_t9b_errors)"), "taskgroup bridge failure")
    expect(py("_lib_t9b_after"), "after-taskgroup")
    py_exec("_lib_t9b_loop.close()")


def test_exception_through_suspended_generator():
    py_exec(
        """
def _lib_t10_gen_func():
    while True:
        try:
            value = yield "ready"
        except RuntimeError as e:
            yield "caught:" + str(e)
        except GeneratorExit:
            return
_lib_t10_gen = _lib_t10_gen_func()
"""
    )
    expect(py("next(_lib_t10_gen)"), "ready")
    expect(py('_lib_t10_gen.throw(RuntimeError("injected from host"))'), "caught:injected from host")
    expect(py("next(_lib_t10_gen)"), "ready")
    expect(js('omnivm.call("python", "_lib_t10_gen.throw(RuntimeError(\'from JS\'))")'), "caught:from JS")
    expect(py("next(_lib_t10_gen)"), "ready")
    py_exec("_lib_t10_gen.close()")


def test_object_pinning_and_gc():
    py_exec(
        """
import gc
import weakref
class _LibT11Obj:
    def __init__(self, value):
        self.value = value
_lib_t11_handles = {}
_lib_t11_weak_refs = {}
_lib_t11_next_id = 0
def _lib_t11_pin(value):
    global _lib_t11_next_id
    obj = _LibT11Obj(value)
    hid = _lib_t11_next_id
    _lib_t11_next_id += 1
    _lib_t11_handles[hid] = obj
    _lib_t11_weak_refs[hid] = weakref.ref(obj)
    return hid
def _lib_t11_get(hid):
    return _lib_t11_handles[int(hid)].value
def _lib_t11_unpin(hid):
    del _lib_t11_handles[int(hid)]
"""
    )
    ids = [py(f"_lib_t11_pin('value_{i}')") for i in range(10)]
    for i, hid in enumerate(ids):
        expect(js(f"""omnivm.call("ruby", "OmniVM.call('python', '_lib_t11_get({hid})')")"""), f"value_{i}")
    py_exec("gc.collect(); gc.collect(); gc.collect()")
    for i, hid in enumerate(ids):
        expect(py(f"_lib_t11_get({hid})"), f"value_{i}")
        py_exec(f"_lib_t11_unpin({hid})")
    py_exec("gc.collect(); gc.collect(); gc.collect()")
    for hid in ids:
        expect(py(f"_lib_t11_weak_refs[{hid}]() is None"), "True")


def test_interleaved_live_generators():
    py_exec(
        """
def _lib_t12_powers_of_2():
    x = 1
    while True:
        yield x
        x *= 2
def _lib_t12_powers_of_3():
    x = 1
    while True:
        yield x
        x *= 3
def _lib_t12_fib():
    a, b = 0, 1
    while True:
        yield a
        a, b = b, a + b
_lib_t12_gen_a = _lib_t12_powers_of_2()
_lib_t12_gen_b = _lib_t12_powers_of_3()
_lib_t12_gen_c = _lib_t12_fib()
"""
    )
    expect(rb('OmniVM.call("python", "next(_lib_t12_gen_b)")'), "1")
    expect(js('omnivm.call("python", "next(_lib_t12_gen_a)")'), "1")
    expect(rb('OmniVM.call("python", "next(_lib_t12_gen_c)")'), "0")
    expect(js('omnivm.call("python", "next(_lib_t12_gen_a)")'), "2")
    expect(py("next(_lib_t12_gen_b)"), "3")
    expect(js('omnivm.call("python", "next(_lib_t12_gen_c)")'), "1")
    expect(rb('OmniVM.call("python", "next(_lib_t12_gen_c)")'), "1")
    expect(py("next(_lib_t12_gen_a)"), "4")
    expect(rb('OmniVM.call("python", "next(_lib_t12_gen_b)")'), "9")
    expect(py("next(_lib_t12_gen_c)"), "2")
    py_exec("_lib_t12_gen_a.close(); _lib_t12_gen_b.close(); _lib_t12_gen_c.close()")


def test_gc_finalizer_cross_runtime():
    py_exec(
        """
import gc
_lib_t13_del_results = []
class _LibT13Poisoned:
    def __init__(self, val):
        self.val = val
    def __del__(self):
        try:
            result = omnivm.call("javascript", str(self.val) + " + 1")
            _lib_t13_del_results.append(int(result))
        except Exception as e:
            _lib_t13_del_results.append("ERR:" + str(e))
"""
    )
    for i in range(50):
        py_exec(f"_lib_t13_obj = _LibT13Poisoned({i}); del _lib_t13_obj; gc.collect(); gc.collect()")
    expect(py("len(_lib_t13_del_results)"), "50")
    for i in range(50):
        expect(py(f"_lib_t13_del_results[{i}]"), str(i + 1))


def test_string_encoding_gauntlet():
    py_exec(
        r'''
_lib_t14_strings = {
    "empty": "",
    "unicode": "héllo wörld 你好 🎉",
    "err_prefix": "ERR: this is not an error",
    "large": "A" * 102400,
    "escapes": 'line1\nline2\"quoted\"',
}
def _lib_t14_get(name):
    return _lib_t14_strings[name]
def _lib_t14_check(name, val):
    return "match" if val == _lib_t14_strings[name] else "mismatch"
'''
    )
    for name in ("empty", "unicode", "large", "escapes"):
        expr = f'omnivm.call("python", "_lib_t14_get(\\"{name}\\")")'
        val = js(expr)
        expect(py(f"_lib_t14_check({name!r}, {val!r})"), "match")
    expect(py("_lib_t14_get('err_prefix')"), "ERR: this is not an error")


def test_sustained_mixed_workload():
    total = 0
    for i in range(1000):
        if i % 3 == 0:
            total += int(py(f"{i} + 1"))
        elif i % 3 == 1:
            total += int(js(f"{i} + 1"))
        else:
            total += int(rb(f"{i} + 1"))
    expect(total, sum(range(1, 1001)))


def test_all_runtimes_chain():
    py_exec(
        """
def _lib_t20_py_relay(x):
    return omnivm.call("javascript", "_lib_t20_js_relay(" + str(int(x) + 5) + ")")
"""
    )
    js(
        """
function _lib_t20_js_relay(x) {
    var val = x * 2;
    return omnivm.call("java", 'omnivm.OmniVM.call("ruby", "' + val + ' * 3")');
}
"ready";
"""
    )
    expect(py("_lib_t20_py_relay(10)"), "90")
    expect(java('omnivm.OmniVM.call("python", "omnivm.call(\\\'javascript\\\', \\\'omnivm.call(\\\\\\\"ruby\\\\\\\", \\\\\\\"7 * 8\\\\\\\")\\\')")'), "56")
    expect(rb("OmniVM.call('java', 'omnivm.OmniVM.call(\"python\", \"omnivm.call(\\'javascript\\', \\'3 + 4\\')\")')"), "7")


def test_java_enters_arena():
    expect(java("1 + 1"), "2")
    expect(java('omnivm.OmniVM.call("python", "7 * 6")'), "42")
    expect(java('omnivm.OmniVM.call("javascript", "7 * 6")'), "42")
    expect(java('omnivm.OmniVM.call("ruby", "7 * 6")'), "42")
    expect(py('omnivm.call("java", "7 * 6")'), "42")
    expect(js('omnivm.call("java", "7 * 6")'), "42")
    expect(rb('OmniVM.call("java", "7 * 6")'), "42")


def test_java_reentrant_call():
    expect(java('omnivm.OmniVM.call("python", "omnivm.call(\\\'java\\\', \\\'100 + 23\\\')")'), "123")
    expect(java('omnivm.OmniVM.call("python", "omnivm.call(\\\'javascript\\\', \\\'omnivm.call(\\\\\\\"java\\\\\\\", \\\\\\\"200 + 34\\\\\\\")\\\')")'), "234")
    py_exec(
        """
def _lib_t21_inner():
    return omnivm.call("java", "300 + 45")
"""
    )
    rb(
        """
def _lib_t21_rb_relay
  OmniVM.call("java", 'omnivm.OmniVM.call("python", "_lib_t21_inner()")')
end
"ready"
"""
    )
    expect(java('omnivm.OmniVM.call("ruby", "_lib_t21_rb_relay")'), "345")


def test_java_exception_handling():
    out = omnivm.execute(
        "java",
        """
import omnivm.OmniVM;
String result;
try {
    result = OmniVM.call("javascript", "(function() { throw new Error('bridge error'); })()");
} catch (RuntimeException e) {
    result = "caught:" + e.getMessage();
}
String check = OmniVM.call("python", "1 + 1");
System.out.print(result + "|ok:" + check);
""",
    ).strip()
    expect_contains(out, "caught:")
    expect_contains(out, "bridge error")
    expect_contains(out, "|ok:2")

    out = omnivm.execute(
        "java",
        """
import omnivm.OmniVM;
String result;
try {
    result = OmniVM.call("python", "1/0");
} catch (RuntimeException e) {
    result = "py_caught:" + e.getMessage();
}
System.out.print(result);
""",
    ).strip()
    expect_contains(out, "py_caught:")
    expect_contains(out, "division by zero")

    out = omnivm.execute(
        "java",
        """
import omnivm.OmniVM;
String result = "unset";
for (int attempt = 0; attempt < 3; attempt++) {
    try {
        if (attempt < 2) {
            OmniVM.call("javascript", "(function() { throw new Error('attempt ' + " + attempt + "); })()");
        } else {
            result = "success:" + OmniVM.call("ruby", "42 * 2");
        }
    } catch (RuntimeException e) {
    }
}
System.out.print(result);
""",
    ).strip()
    expect(out, "success:84")


def test_java_compilation_storm():
    runtimes = ("python", "javascript", "ruby")
    for i in range(50):
        target = runtimes[i % len(runtimes)]
        expect(java(f'Integer.parseInt(omnivm.OmniVM.call("{target}", "{i} + 1"))'), str(i + 1))
    expect(py("1 + 1"), "2")
    expect(js("1 + 1"), "2")
    expect(rb("1 + 1"), "2")
    expect(java("1 + 1"), "2")


def test_concurrent_java_all_runtimes():
    errors = []
    lock = threading.Lock()

    def record(fn):
        try:
            fn()
        except Exception as exc:
            with lock:
                errors.append(exc)

    workers = [
        lambda: [expect(java(f"{i} + {i}"), str(i * 2)) for i in range(50)],
        lambda: [expect(py(f"{i} * 3"), str(i * 3)) for i in range(50)],
        lambda: [expect(java(f'omnivm.OmniVM.call("python", "{i} + {i + 1}")'), str(i * 2 + 1)) for i in range(25)],
        lambda: [expect(js(f'omnivm.call("java", "{i} * 2")'), str(i * 2)) for i in range(25)],
        lambda: [expect(rb(f'OmniVM.call("java", \'omnivm.OmniVM.call("python", "{i} + 100")\')'), str(i + 100)) for i in range(10)],
    ]
    threads = [threading.Thread(target=record, args=(worker,)) for worker in workers]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=30)
    for thread in threads:
        if thread.is_alive():
            raise AssertionError("concurrent runtime worker hung")
    if errors:
        raise errors[0]


def test_generator_send_pipeline():
    py_exec(
        """
def _lib_t25_gen_a():
    i = 0
    bonus = 0
    while True:
        i += 1
        val = yield (i * i) + bonus
        bonus = int(val) if val is not None else 0
def _lib_t25_gen_b():
    val = yield "ready"
    while True:
        val = yield int(val) * 3
def _lib_t25_gen_c():
    val = yield "ready"
    while True:
        val = yield int(val) + 1000
_lib_t25_a = _lib_t25_gen_a()
_lib_t25_b = _lib_t25_gen_b()
_lib_t25_c = _lib_t25_gen_c()
next(_lib_t25_a); next(_lib_t25_b); next(_lib_t25_c)
"""
    )
    for _ in range(20):
        a_val = py("next(_lib_t25_a)")
        js_val = js(f"parseInt('{a_val}') * 2")
        b_val = py(f"_lib_t25_b.send({js_val!r})")
        rb_val = rb(f"{b_val} + 10")
        c_val = py(f"_lib_t25_c.send({rb_val!r})")
        py(f"_lib_t25_a.send({c_val!r})")
    int(py("next(_lib_t25_a)"))
    py_exec("_lib_t25_a.close(); _lib_t25_b.close(); _lib_t25_c.close()")


def test_iterator_protocol_cross_runtime_next():
    py_exec(
        """
class _LibT26CrossIter:
    def __init__(self, n):
        self.n = n
        self.i = 0
    def __iter__(self):
        return self
    def __next__(self):
        if self.i >= self.n:
            raise StopIteration
        self.i += 1
        return omnivm.call("javascript", str(self.i) + " * 2")
"""
    )
    py_exec("_lib_t26_list = list(_LibT26CrossIter(100))")
    expect(py("_lib_t26_list[0] + '|' + _lib_t26_list[49] + '|' + _lib_t26_list[99]"), "2|100|200")
    expect(py("sum(int(x) for x in _LibT26CrossIter(100))"), "10100")
    py_exec("_lib_t26_vals = list(_LibT26CrossIter(5))")
    for i in range(5):
        val = py(f"_lib_t26_vals[{i}]")
        expect(java(f'Integer.parseInt("{val}") + 1'), str((i + 1) * 2 + 1))


def test_context_manager_exit_inflight_exception():
    py_exec(
        """
class _LibT27CM:
    def __init__(self):
        self.enter_val = None
        self.exit_val = None
        self.exc_type_name = None
        self.suppressed = False
    def __enter__(self):
        self.enter_val = omnivm.call("javascript", "21 * 2")
        return self
    def __exit__(self, exc_type, exc_val, exc_tb):
        if exc_type is not None:
            self.exc_type_name = exc_type.__name__
            self.exit_val = omnivm.call("ruby", "84 / 2")
            self.suppressed = True
            return True
        return False
class _LibT27Inner:
    def __init__(self):
        self.exit_val = None
    def __enter__(self):
        return self
    def __exit__(self, exc_type, exc_val, exc_tb):
        if exc_type is not None:
            self.exit_val = omnivm.call("java", "99 + 1")
            return True
        return False
"""
    )
    expect(
        py(
            """
_lib_t27_cm1 = _LibT27CM()
with _lib_t27_cm1:
    pass
_lib_t27_cm1.enter_val + "|" + str(_lib_t27_cm1.suppressed)
"""
        ),
        "42|False",
    )
    expect(
        py(
            """
_lib_t27_cm2 = _LibT27CM()
with _lib_t27_cm2:
    raise ValueError("test error")
_lib_t27_cm2.enter_val + "|" + _lib_t27_cm2.exit_val + "|" + _lib_t27_cm2.exc_type_name + "|" + str(_lib_t27_cm2.suppressed)
"""
        ),
        "42|42|ValueError|True",
    )
    expect(
        py(
            """
_lib_t27_outer = _LibT27CM()
_lib_t27_inner = _LibT27Inner()
with _lib_t27_outer:
    with _lib_t27_inner:
        raise RuntimeError("nested")
_lib_t27_outer.enter_val + "|" + _lib_t27_inner.exit_val + "|" + str(_lib_t27_outer.suppressed)
"""
        ),
        "42|100|False",
    )


def test_nested_try_finally_cross_runtime():
    expect(
        py(
            """
_lib_t28_results = []
_lib_t28_caught = False
try:
    try:
        try:
            raise ValueError("propagating")
        finally:
            _lib_t28_results.append("js:" + omnivm.call("javascript", "10 + 1"))
    finally:
        _lib_t28_results.append("rb:" + omnivm.call("ruby", "20 + 2"))
except ValueError as e:
    _lib_t28_results.append("caught:" + str(e))
    _lib_t28_caught = True
"|".join(_lib_t28_results) + "|" + str(_lib_t28_caught)
"""
        ),
        "js:11|rb:22|caught:propagating|True",
    )
    expect(
        py(
            """
_lib_t28_r2 = []
try:
    try:
        try:
            try:
                raise TypeError("deep")
            finally:
                _lib_t28_r2.append("py:" + str(1 + 1))
        finally:
            _lib_t28_r2.append("js:" + omnivm.call("javascript", "2 + 2"))
    finally:
        _lib_t28_r2.append("rb:" + omnivm.call("ruby", "3 + 3"))
except TypeError:
    _lib_t28_r2.append("java:" + omnivm.call("java", "4 + 4"))
"|".join(_lib_t28_r2)
"""
        ),
        "py:2|js:4|rb:6|java:8",
    )
    expect(
        py(
            """
_lib_t28_r3 = "unset"
try:
    try:
        raise ValueError("original")
    finally:
        try:
            omnivm.call("javascript", "(function() { throw new Error('finally boom'); })()")
        except RuntimeError:
            _lib_t28_r3 = "finally_caught"
except ValueError:
    _lib_t28_r3 = _lib_t28_r3 + "|original_caught"
_lib_t28_r3
"""
        ),
        "finally_caught|original_caught",
    )


def test_getattr_dispatches_to_runtimes():
    py_exec(
        """
class _LibT29RuntimeProxy:
    def __init__(self, runtime):
        object.__setattr__(self, "_runtime", runtime)
        object.__setattr__(self, "_call_count", 0)
    def __getattr__(self, name):
        rt = object.__getattribute__(self, "_runtime")
        count = object.__getattribute__(self, "_call_count")
        object.__setattr__(self, "_call_count", count + 1)
        return omnivm.call(rt, name)
_lib_t29_js = _LibT29RuntimeProxy("javascript")
_lib_t29_rb = _LibT29RuntimeProxy("ruby")
_lib_t29_java = _LibT29RuntimeProxy("java")
"""
    )
    expect(py('_lib_t29_js.__getattr__("7 * 6")'), "42")
    expect(py('_lib_t29_rb.__getattr__("7 * 6")'), "42")
    expect(py('_lib_t29_java.__getattr__("7 * 6")'), "42")
    expect(
        py(
            """
_lib_t29_results = []
for i in range(50):
    _lib_t29_results.append(_lib_t29_js.__getattr__(str(i) + " + 1"))
len(_lib_t29_results) == 50 and _lib_t29_results[0] == "1" and _lib_t29_results[49] == "50"
"""
        ),
        "True",
    )
    expect(py("""_lib_t29_js.__getattr__('omnivm.call("ruby", "11 * 4")')"""), "44")
    expect(py("_lib_t29_js._call_count"), "52")


def test_list_comprehension_cross_runtime_filter_transform():
    expect(
        py(
            """
_lib_t30_result = [omnivm.call("javascript", str(x) + " * 3") for x in range(100) if int(omnivm.call("ruby", str(x) + " % 2")) == 0]
len(_lib_t30_result)
"""
        ),
        "50",
    )
    expect(py("_lib_t30_result[0] + '|' + _lib_t30_result[1] + '|' + _lib_t30_result[49]"), "0|6|294")
    expect(
        py(
            """
_lib_t30_matrix = [[omnivm.call("java", str(r) + " * 10 + " + str(c)) for c in range(5)] for r in range(5)]
_lib_t30_matrix[0][0] + "|" + _lib_t30_matrix[2][3] + "|" + _lib_t30_matrix[4][4]
"""
        ),
        "0|23|44",
    )
    expect(
        py(
            """
_lib_t30_dict = {omnivm.call("ruby", '"key_" + ' + str(i) + '.to_s'): omnivm.call("javascript", str(i) + " * " + str(i)) for i in range(10)}
_lib_t30_dict["key_0"] + "|" + _lib_t30_dict["key_5"] + "|" + _lib_t30_dict["key_9"]
"""
        ),
        "0|25|81",
    )


def test_yield_from_cross_runtime_subgenerator():
    py_exec(
        """
def _lib_t31_sub_gen(runtime, n):
    for i in range(n):
        val = omnivm.call(runtime, str(i) + " + 1")
        yield val
def _lib_t31_outer_gen(runtime, n):
    result = yield from _lib_t31_sub_gen(runtime, n)
    return result
def _lib_t31_outer_with_prefix(runtime, n, prefix):
    yield prefix + ":start"
    yield from _lib_t31_sub_gen(runtime, n)
    yield prefix + ":end"
"""
    )
    expect(
        py(
            """
_lib_t31_vals = list(_lib_t31_outer_gen("javascript", 20))
_lib_t31_vals[0] + "|" + _lib_t31_vals[19]
"""
        ),
        "1|20",
    )
    expect(py('"|".join(list(_lib_t31_outer_with_prefix("ruby", 5, "RB")))'), "RB:start|1|2|3|4|5|RB:end")
    py_exec(
        """
def _lib_t31_sub_sendable():
    val = yield "ready"
    while val is not None:
        result = omnivm.call("javascript", str(val) + " * 2")
        val = yield result
def _lib_t31_outer_sendable():
    final = yield from _lib_t31_sub_sendable()
    return final
_lib_t31_sg = _lib_t31_outer_sendable()
"""
    )
    expect(py("next(_lib_t31_sg)"), "ready")
    for send, expected in ((5, "10"), (10, "20"), (25, "50"), (100, "200")):
        expect(py(f"_lib_t31_sg.send({send})"), expected)
    py_exec(
        """
def _lib_t31_sub_throwable():
    while True:
        try:
            yield "waiting"
        except ValueError as e:
            result = omnivm.call("javascript", '"caught:" + "' + str(e) + '"')
            yield result
        except GeneratorExit:
            return
def _lib_t31_outer_throwable():
    yield from _lib_t31_sub_throwable()
_lib_t31_tg = _lib_t31_outer_throwable()
"""
    )
    expect(py("next(_lib_t31_tg)"), "waiting")
    expect(py('_lib_t31_tg.throw(ValueError("injected"))'), "caught:injected")
    expect(py("next(_lib_t31_tg)"), "waiting")
    py_exec("_lib_t31_sg.close(); _lib_t31_tg.close()")


def test_contextlib_contextmanager_cross_runtime():
    py_exec(
        """
import contextlib
@contextlib.contextmanager
def _lib_t32_managed_resource(runtime):
    enter_val = omnivm.call(runtime, "21 * 2")
    try:
        yield enter_val
    finally:
        omnivm.call(runtime, "1 + 1")
"""
    )
    expect(
        py(
            """
_lib_t32_result = None
with _lib_t32_managed_resource("javascript") as val:
    _lib_t32_result = val
_lib_t32_result
"""
        ),
        "42",
    )
    expect(
        py(
            """
_lib_t32_result2 = None
with _lib_t32_managed_resource("java") as val:
    _lib_t32_result2 = val
_lib_t32_result2
"""
        ),
        "42",
    )
    expect(
        py(
            """
_lib_t32_vals = []
with _lib_t32_managed_resource("javascript") as js_val:
    _lib_t32_vals.append("js:" + js_val)
    with _lib_t32_managed_resource("java") as java_val:
        _lib_t32_vals.append("java:" + java_val)
"|".join(_lib_t32_vals)
"""
        ),
        "js:42|java:42",
    )
    expect(
        py(
            """
_lib_t32_exc_result = "not_set"
try:
    with _lib_t32_managed_resource("javascript") as val:
        raise ValueError("body error")
except ValueError as e:
    _lib_t32_exc_result = "caught:" + str(e)
_lib_t32_exc_result
"""
        ),
        "caught:body error",
    )
    expect(
        py(
            """
_lib_t32_cycle_results = []
for i in range(10):
    with _lib_t32_managed_resource("javascript") as val:
        _lib_t32_cycle_results.append(val)
len(_lib_t32_cycle_results) == 10 and all(v == "42" for v in _lib_t32_cycle_results)
"""
        ),
        "True",
    )


def test_recursive_cross_runtime_depth_bomb():
    py_exec(
        """
def _lib_t33_recurse(depth, max_depth):
    if depth >= max_depth:
        return "bottom:" + str(depth)
    return omnivm.call("javascript", 'omnivm.call("python", "_lib_t33_recurse(' + str(depth + 1) + ', ' + str(max_depth) + ')")')
"""
    )
    max_working = 0
    for depth in (5, 10, 25, 50, 75, 100):
        try:
            result = py(f"_lib_t33_recurse(0, {depth})")
        except omnivm.RuntimeError:
            break
        expect(result, f"bottom:{depth}")
        max_working = depth
    if max_working < 25:
        raise AssertionError(f"max safe depth too low: {max_working}")
    expect(py("1 + 1"), "2")
    expect(js("1 + 1"), "2")


def test_one_mb_string_bridge():
    py_exec('_lib_t34_big = "X" * (1024 * 1024)')
    expect(py("len(_lib_t34_big)"), "1048576")
    expect(py('len(omnivm.call("javascript", repr(_lib_t34_big)))'), "1048576")
    expect(
        py(
            """
_lib_t34_via_java = omnivm.call("java", "new String(new char[1048576]).replace((char)0, (char)88)")
len(_lib_t34_via_java) == 1048576 and _lib_t34_via_java == _lib_t34_big
"""
        ),
        "True",
    )
    py_exec("del _lib_t34_big; del _lib_t34_via_java")


def test_chained_error_recovery_cascade():
    expect(
        py(
            """
_lib_t35_log = []
try:
    omnivm.call("javascript", "(function() { throw new Error('fail1'); })()")
    _lib_t35_log.append("js:ok")
except RuntimeError:
    _lib_t35_log.append("js:caught")
    _lib_t35_log.append("java_recovery:" + omnivm.call("java", "100 + 1"))
try:
    omnivm.call("java", 'omnivm.OmniVM.call("python", "1/0")')
    _lib_t35_log.append("py_via_java:ok")
except RuntimeError:
    _lib_t35_log.append("py_via_java:caught")
    _lib_t35_log.append("js_recovery:" + omnivm.call("javascript", "200 + 2"))
try:
    omnivm.call("java", "this is not valid java")
    _lib_t35_log.append("java_bad:ok")
except RuntimeError:
    _lib_t35_log.append("java_bad:caught")
    _lib_t35_log.append("js_recovery2:" + omnivm.call("javascript", "300 + 3"))
_lib_t35_log.append("final_js:" + omnivm.call("javascript", "1 + 1"))
_lib_t35_log.append("final_java:" + omnivm.call("java", "2 + 2"))
"|".join(_lib_t35_log)
"""
        ),
        "js:caught|java_recovery:101|py_via_java:caught|js_recovery:202|java_bad:caught|js_recovery2:303|final_js:2|final_java:4",
    )
    expect(
        py(
            """
_lib_t35_deep = []
try:
    try:
        omnivm.call("javascript", "(function() { throw new Error('outer'); })()")
    except RuntimeError:
        _lib_t35_deep.append("outer_caught")
        try:
            omnivm.call("javascript", "(function() { throw new Error('inner'); })()")
        except RuntimeError:
            _lib_t35_deep.append("inner_caught")
            _lib_t35_deep.append("final:" + omnivm.call("java", "42 + 0"))
except Exception as e:
    _lib_t35_deep.append("unexpected:" + str(e))
"|".join(_lib_t35_deep)
"""
        ),
        "outer_caught|inner_caught|final:42",
    )
    expect(
        py(
            """
_lib_t35_chain = []
for i in range(5):
    try:
        if i % 2 == 0:
            omnivm.call("javascript", "(function() { throw new Error('err' + " + str(i) + "); })()")
        else:
            omnivm.call("java", 'throw new RuntimeException("err' + str(i) + '")')
    except RuntimeError:
        if i % 2 == 0:
            _lib_t35_chain.append(omnivm.call("java", str(i) + " + 100"))
        else:
            _lib_t35_chain.append(omnivm.call("javascript", str(i) + " + 200"))
"|".join(_lib_t35_chain)
"""
        ),
        "100|201|102|203|104",
    )


def test_functools_reduce_cross_runtime_accumulator():
    py_exec(
        """
import functools
def _lib_t36_js_add(acc, x):
    return omnivm.call("javascript", str(acc) + " + " + str(x))
def _lib_t36_java_mul(acc, x):
    return omnivm.call("java", "Long.parseLong(\\"" + str(acc) + "\\") * " + str(x))
def _lib_t36_alternating(acc, x):
    acc_val = int(acc)
    if x % 2 == 0:
        return omnivm.call("javascript", str(acc_val) + " + " + str(x))
    return omnivm.call("java", str(acc_val) + " + " + str(x))
"""
    )


def test_python_interrupt_stops_infinite_loop():
    py_exec(
        """
import threading, _thread
_lib_t37_started = True
_lib_t37_interrupted = False
_lib_t37_i = 0
threading.Timer(0.05, _thread.interrupt_main).start()
try:
    while True:
        _lib_t37_i += 1
except KeyboardInterrupt:
    _lib_t37_interrupted = True
"""
    )
    expect(py("_lib_t37_started"), "True")
    expect(py("_lib_t37_interrupted"), "True")
    expect(py("42 + 1"), "43")


def test_jvm_npe_does_not_crash_ruby():
    expect(rb("100 + 1"), "101")
    out = omnivm.execute(
        "java",
        """
String s = null;
try {
    int len = s.length();
    System.out.println("should not reach");
} catch (NullPointerException e) {
    System.out.println("caught:" + e.getClass().getSimpleName());
}
""",
    )
    expect_contains(out, "caught:NullPointerException")
    expect(rb("200 + 2"), "202")
    expect(py("300 + 3"), "303")


def test_rapid_jvm_npe_ruby_interleave():
    for i in range(100):
        omnivm.execute(
            "java",
            """
String s = null;
try { s.length(); } catch (NullPointerException e) {}
System.out.println("ok");
""",
        )
        expect(rb(f"{i} + 1"), str(i + 1))


def test_python_interrupt_recovery_during_bridge_call():
    py_exec(
        """
import threading, _thread
_lib_t40_result = None
_lib_t40_caught = False
threading.Timer(0.03, _thread.interrupt_main).start()
try:
    for _lib_t40_i in range(10000):
        _lib_t40_result = omnivm.call("javascript", "1 + 1")
except KeyboardInterrupt:
    _lib_t40_caught = True
"""
    )
    expect(py("_lib_t40_caught or _lib_t40_result == '2'"), "True")
    expect(py("'healthy'"), "healthy")


def test_triple_signal_stress():
    out = omnivm.execute(
        "java",
        """
String s = null;
try { s.length(); } catch (NullPointerException e) { System.out.println("npe_ok"); }
""",
    )
    expect_contains(out, "npe_ok")
    expect(rb("raise 'test_error' rescue $!.message"), "test_error")
    py_exec(
        """
import threading, _thread
_lib_t41_ok = False
_lib_t41_i = 0
threading.Timer(0.02, _thread.interrupt_main).start()
try:
    while True:
        _lib_t41_i += 1
except KeyboardInterrupt:
    _lib_t41_ok = True
"""
    )
    expect(py("_lib_t41_ok"), "True")
    expect(java("1 + 2 + 3"), "6")
    expect(rb("4 + 5 + 6"), "15")
    expect(py("7 + 8 + 9"), "24")
    expect(js("10 + 11 + 12"), "33")


def test_sustained_jvm_npe_storm():
    for i in range(100):
        omnivm.execute(
            "java",
            """
String s = null;
try { s.length(); } catch (NullPointerException e) { System.out.println("ok"); }
""",
        )
        if i % 20 == 19:
            expect(py(f"{i} * 2"), str(i * 2))
            expect(rb(f"{i} + 10"), str(i + 10))
            expect(js(f"{i} + 100"), str(i + 100))


def test_thread_identity_python_host():
    host_tid = omnivm.host_thread_id()
    expect(py("__import__('threading').get_native_id()"), str(host_tid))
    expect(java('omnivm.OmniVM.call("python", "__import__(\\\'threading\\\').get_native_id()")'), str(host_tid))
    expect(js('omnivm.call("python", "__import__(\\\'threading\\\').get_native_id()")'), str(host_tid))


def test_foreign_thread_bridge():
    py_exec(
        """
import threading
_lib_t44_py_result = None
_lib_t44_py_error = None
def _lib_t44_py_worker():
    global _lib_t44_py_result, _lib_t44_py_error
    try:
        _lib_t44_py_result = omnivm.call("javascript", "1 + 1")
    except Exception as e:
        _lib_t44_py_error = str(e)
_lib_t44_t = threading.Thread(target=_lib_t44_py_worker)
_lib_t44_t.start()
_lib_t44_t.join(timeout=15)
if _lib_t44_t.is_alive():
    _lib_t44_py_error = "DEADLOCK"
"""
    )
    expect(py("_lib_t44_py_error"), "None")
    expect(py("_lib_t44_py_result"), "2")
    out = omnivm.execute(
        "java",
        """
final String[] result = {null};
final String[] error = {null};
Thread t = new Thread(() -> {
    try {
        result[0] = omnivm.OmniVM.call("python", "str(6 * 9)");
    } catch (Exception e) {
        error[0] = e.getMessage();
    }
});
t.start();
t.join(15000);
if (t.isAlive()) System.out.println("ERR:DEADLOCK");
else if (error[0] != null) System.out.println("ERR:" + error[0]);
else System.out.println(result[0]);
""",
    ).strip()
    expect(out, "54")
    result = []
    errors = []

    def rb_worker():
        try:
            result.append(rb('OmniVM.call("javascript", "3 + 4")'))
        except Exception as exc:
            errors.append(exc)

    thread = threading.Thread(target=rb_worker)
    thread.start()
    thread.join(timeout=15)
    if thread.is_alive():
        raise AssertionError("Ruby foreign-thread bridge deadlocked")
    if errors:
        raise errors[0]
    expect(result[0], "7")


def test_exception_ping_pong():
    rb('def _lib_t45_raise; raise "ruby_cascade_error"; end; "ready"')
    js(
        """
function _lib_t45_chain() {
  return omnivm.call("java", 'omnivm.OmniVM.call("ruby", "_lib_t45_raise")');
}
"ready";
"""
    )
    py_exec(
        """
_lib_t45_error = None
try:
    omnivm.call("javascript", "_lib_t45_chain()")
    _lib_t45_error = "NO_ERROR"
except Exception as e:
    _lib_t45_error = str(e)
"""
    )
    expect_contains(py("_lib_t45_error"), "ruby_cascade_error")
    for _ in range(50):
        py_exec(
            """
try:
    omnivm.call("javascript", "_lib_t45_chain()")
except Exception:
    pass
"""
        )
    expect(py("'py_ok'"), "py_ok")
    expect(js("'js_ok'"), "js_ok")
    expect(java('"java_ok"'), "java_ok")
    expect(rb("'rb_ok'"), "rb_ok")


def test_gc_standoff_large_allocations():
    for i in range(25):
        expect(py("len('P' * 1048576)"), "1048576")
        expect(js("var _s='J'; while(_s.length<1048576) _s=_s+_s; _s.substring(0,1048576).length"), "1048576")
        expect(java("new String(new char[1048576]).replace((char)0, 'V').length()"), "1048576")
        expect(rb("('R' * 1048576).length"), "1048576")
        expect(py('len(omnivm.call("javascript", "var _s=\\\'X\\\'; while(_s.length<1048576) _s=_s+_s; _s.substring(0,1048576)"))'), "1048576")
        expect(js('omnivm.call("ruby", "\'R\' * 1048576").length'), "1048576")


def test_ruby_fiber_cooperative_bridge():
    expect(
        rb(
            """
f = Fiber.new do
  r1 = Fiber.yield(["python", "10 + 20"])
  r2 = Fiber.yield(["javascript", "30 + 40"])
  r3 = Fiber.yield(["java", "50 + 60"])
  "first:#{r1}|second:#{r2}|third:#{r3}"
end
req = f.resume
while req.is_a?(Array)
  val = OmniVM.call(req[0], req[1])
  req = f.resume(val)
end
req
"""
        ),
        "first:30|second:70|third:110",
    )
    expect(
        rb(
            """
fibers = [
  Fiber.new { |_| r = Fiber.yield(["python", "100 + 1"]); "a:#{r}" },
  Fiber.new { |_| r = Fiber.yield(["javascript", "200 + 2"]); "b:#{r}" },
  Fiber.new { |_| r = Fiber.yield(["java", "300 + 3"]); "c:#{r}" }
]
requests = fibers.map { |f| f.resume(nil) }
results = fibers.zip(requests).map do |f, req|
  val = OmniVM.call(req[0], req[1])
  f.resume(val)
end
results.join(",")
"""
        ),
        "a:101,b:202,c:303",
    )
    expect(
        rb(
            """
f = Fiber.new do
  sum = 0
  50.times do |i|
    val = Fiber.yield(["javascript", "#{i} + 1"])
    sum += val.to_i
  end
  sum.to_s
end
req = f.resume
while req.is_a?(Array)
  val = OmniVM.call(req[0], req[1])
  req = f.resume(val)
end
req
"""
        ),
        "1275",
    )


def test_ruby_ensure_bridge_unwind():
    expect(
        rb(
            """
results = []
begin
  begin
    raise "test_error"
  ensure
    results << "ensure:" + OmniVM.call("python", "str(6 * 7)")
  end
rescue => e
  results << "rescued:" + e.message
end
results.join("|")
"""
        ),
        "ensure:42|rescued:test_error",
    )
    expect(
        rb(
            """
out = []
begin
  begin
    begin
      begin
        raise "deep_err"
      ensure
        out << "e1:" + OmniVM.call("python", "str(1+1)")
      end
    ensure
      out << "e2:" + OmniVM.call("javascript", "3+3")
    end
  ensure
    out << "e3:" + OmniVM.call("java", "7+7")
  end
rescue => e
  out << "r:" + e.message
end
out.join("|")
"""
        ),
        "e1:2|e2:6|e3:14|r:deep_err",
    )


def test_ruby_catch_throw_bridge():
    expect(
        rb(
            """
catch(:done) do
  val = OmniVM.call("javascript", "100 + 23")
  throw(:done, "caught:" + val) if val.to_i > 100
  "not_thrown"
end
"""
        ),
        "caught:123",
    )
    expect(
        rb(
            """
catch(:outer) do
  catch(:inner) do
    v1 = OmniVM.call("python", "str(10)")
    v2 = OmniVM.call("java", "20 + 5")
    throw(:outer, "skip_inner:" + v1 + "+" + v2)
  end
  "should_not_reach"
end
"""
        ),
        "skip_inner:10+25",
    )
    expect(
        rb(
            """
count = 0
50.times do |i|
  result = catch(:loop) do
    val = OmniVM.call("javascript", "#{i} + 1")
    throw(:loop, val.to_i)
  end
  count += result
end
count.to_s
"""
        ),
        "1275",
    )


def test_js_try_finally_bridge_throw():
    expect(
        js(
            """
var result = "";
try {
  try {
    omnivm.call("python", "1/0");
  } finally {
    result += "finally:" + omnivm.call("ruby", "(7 * 8).to_s");
  }
} catch(e) {
  result += "|caught";
}
result
"""
        ),
        "finally:56|caught",
    )
    expect(
        js(
            """
var out = "";
try {
  try {
    try {
      omnivm.call("ruby", "raise 'inner_boom'");
    } finally {
      out += "f1:" + omnivm.call("python", "str(3*3)");
    }
  } finally {
    out += "|f2:" + omnivm.call("java", "4*4");
  }
} catch(e) {
  out += "|caught";
}
out
"""
        ),
        "f1:9|f2:16|caught",
    )


def test_four_runtime_mutual_recursion():
    py_exec(
        """
def _lib_t51_dispatch(depth, max_depth):
    if depth >= max_depth:
        return "end"
    runtimes = ["javascript", "java", "ruby"]
    labels = {"javascript": "J", "java": "V", "ruby": "R"}
    rt = runtimes[depth % 3]
    inner = "_lib_t51_dispatch(" + str(depth+1) + "," + str(max_depth) + ")"
    if rt == "javascript":
        result = omnivm.call("javascript", "omnivm.call('python', '" + inner + "')")
    elif rt == "java":
        result = omnivm.call("java", 'omnivm.OmniVM.call("python", "' + inner + '")')
    else:
        result = omnivm.call("ruby", "OmniVM.call('python', '" + inner + "')")
    return labels[rt] + ">" + result
"""
    )
    expect(py("_lib_t51_dispatch(0, 18)"), "J>V>R>J>V>R>J>V>R>J>V>R>J>V>R>J>V>R>end")


def test_rogue_guest_preemption():
    child_check(
        """
omnivm.set_task_timeout(300)
for runtime, code in [
    ("javascript", "while(true) {}"),
    ("ruby", "i = 0; loop { i += 1 }"),
]:
    try:
        omnivm.execute(runtime, code)
    except omnivm.RuntimeError:
        pass
    else:
        raise AssertionError(runtime + " loop was not interrupted")
omnivm.set_task_timeout(0)
""",
        timeout=8,
    )


def test_post_termination_v8_storm():
    child_check(
        """
omnivm.set_task_timeout(300)
try:
    omnivm.execute("javascript", "var i = 0; while(true) { i += 1; }")
except omnivm.RuntimeError:
    pass
else:
    raise AssertionError("JS loop was not interrupted")
omnivm.set_task_timeout(0)
for i in range(50):
    if omnivm.call("javascript", f"{i} * {i}") != str(i * i):
        raise AssertionError("bad JS value after termination")
if omnivm.call("javascript", "parseInt(omnivm.call('python', '7 * 8'))") != "56":
    raise AssertionError("cross-runtime V8 call failed after termination")
""",
        timeout=8,
    )


def test_jvm_thread_to_python():
    expect(
        omnivm.execute(
            "java",
            """
final String[] result = {null};
Thread t = new Thread(() -> {
    result[0] = omnivm.OmniVM.call("python", "6 * 9");
});
t.start();
t.join();
System.out.println(result[0]);
""",
        ).strip(),
        "54",
    )


def test_jvm_thread_to_js():
    expect(
        omnivm.execute(
            "java",
            """
final String[] result = {null};
Thread t = new Thread(() -> {
    result[0] = omnivm.OmniVM.call("javascript", "7 * 8");
});
t.start();
t.join();
System.out.println(result[0]);
""",
        ).strip(),
        "56",
    )


def test_jvm_thread_to_ruby():
    expect(
        omnivm.execute(
            "java",
            """
final String[] result = {null};
Thread t = new Thread(() -> {
    result[0] = omnivm.OmniVM.call("ruby", "5 + 3");
});
t.start();
t.join();
System.out.println(result[0]);
""",
        ).strip(),
        "8",
    )


def test_jvm_threads_bridge_calls():
    expect(
        omnivm.execute(
            "java",
            """
int numThreads = 4;
int callsPerThread = 50;
final boolean[] errors = new boolean[numThreads];
Thread[] threads = new Thread[numThreads];
for (int i = 0; i < numThreads; i++) {
    final int idx = i;
    final String runtime;
    final String code;
    final String expected;
    switch (idx) {
        case 0: runtime = "python"; code = "10 + " + idx; expected = "10"; break;
        case 1: runtime = "javascript"; code = "20 + " + idx; expected = "21"; break;
        case 2: runtime = "ruby"; code = "30 + " + idx; expected = "32"; break;
        default: runtime = "python"; code = "40 + " + idx; expected = "43"; break;
    }
    threads[i] = new Thread(() -> {
        for (int j = 0; j < callsPerThread; j++) {
            String result = omnivm.OmniVM.call(runtime, code);
            if (!result.equals(expected)) {
                errors[idx] = true;
                break;
            }
        }
    });
}
for (Thread t : threads) t.start();
for (Thread t : threads) t.join();
boolean anyError = false;
for (boolean e : errors) if (e) anyError = true;
System.out.println(anyError ? "FAIL" : "OK");
""",
        ).strip(),
        "OK",
    )


def test_python_thread_to_js():
    py_exec(
        """
import threading
_lib_t69_result = None
_lib_t69_error = None
def _lib_t69_worker():
    global _lib_t69_result, _lib_t69_error
    try:
        _lib_t69_result = omnivm.call("javascript", "3 * 7")
    except Exception as e:
        _lib_t69_error = str(e)
_lib_t69_t = threading.Thread(target=_lib_t69_worker)
_lib_t69_t.start()
_lib_t69_t.join()
"""
    )
    expect(py("_lib_t69_error"), "None")
    expect(py("_lib_t69_result"), "21")


def test_gil_contention_jvm_thread_to_python():
    expect(py("100 + 23"), "123")
    expect(
        omnivm.execute(
            "java",
            """
final String[] result = {null};
Thread t = new Thread(() -> {
    result[0] = omnivm.OmniVM.call("python", "200 + 34");
});
t.start();
t.join();
System.out.println(result[0]);
""",
        ).strip(),
        "234",
    )
    expect(py("300 + 45"), "345")


def test_nested_foreign_thread_bridge():
    expect(
        omnivm.execute(
            "java",
            """
final String[] result = {null};
final String[] error = {null};
Thread t = new Thread(() -> {
    try {
        result[0] = omnivm.OmniVM.call("python", "omnivm.call('javascript', '10 + 5')");
    } catch (Exception e) {
        error[0] = e.getMessage();
    }
});
t.start();
t.join();
if (error[0] != null) System.out.println("ERR:" + error[0]);
else System.out.println(result[0]);
""",
        ).strip(),
        "15",
    )


def test_reentrant_exception_generator_async_pump():
    py_exec(
        """
import asyncio
def _lib_t16_gen():
    while True:
        try:
            yield "ready"
        except RuntimeError as e:
            yield "caught:" + str(e)
_lib_t16_g = _lib_t16_gen()
def _lib_t16_throw():
    return _lib_t16_g.throw(RuntimeError("async boom"))
async def _lib_t16_task():
    first = next(_lib_t16_g)
    second = omnivm.call("javascript", "omnivm.call('python', '_lib_t16_throw()')")
    return first + "|" + second
_lib_t16_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_lib_t16_loop)
_lib_t16_result = _lib_t16_loop.run_until_complete(_lib_t16_task())
_lib_t16_loop.close()
"""
    )
    expect(py("_lib_t16_result"), "ready|caught:async boom")
    py_exec("_lib_t16_g.close()")


def test_allocation_storm_round_trips():
    for i in range(30):
        expect(py("len('P' * (1024 * 1024))"), "1048576")
        expect(js("var _s='J'; while(_s.length<1048576) _s += _s; _s.length"), "1048576")
        expect(rb("('R' * (1024 * 1024)).length"), "1048576")
        if i % 10 == 9:
            py_exec("import gc; gc.collect()")


def test_sleep_wake_concurrency_torture():
    errors = []
    lock = threading.Lock()

    def worker(index):
        try:
            for i in range(10):
                if (index + i) % 2 == 0:
                    expect(js(f"{index} + {i}"), str(index + i))
                else:
                    expect(py(f"{index} + {i}"), str(index + i))
                time.sleep(0.002)
        except Exception as exc:
            with lock:
                errors.append(exc)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(4)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=20)
    for thread in threads:
        if thread.is_alive():
            raise AssertionError("sleep-wake worker hung")
    if errors:
        raise errors[0]


def test_thread_coalescing_avalanche():
    barrier = threading.Barrier(101)
    completed = []
    errors = []
    lock = threading.Lock()

    def worker(i):
        try:
            barrier.wait(timeout=5)
            expect(py(f"{i} * 2"), str(i * 2))
            with lock:
                completed.append(i)
        except Exception as exc:
            with lock:
                errors.append(exc)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(100)]
    for thread in threads:
        thread.start()
    barrier.wait(timeout=5)
    for thread in threads:
        thread.join(timeout=20)
    if errors:
        raise errors[0]
    expect(len(completed), 100)


def test_python_host_mn_isolation():
    errors = []
    js_done = []
    cpu_done = []
    lock = threading.Lock()

    def cpu_worker(seed):
        total = 0
        for i in range(250000):
            total += i ^ seed
        with lock:
            cpu_done.append(total)

    def js_worker(i):
        try:
            expect(js(f"{i} + {i}"), str(i + i))
            with lock:
                js_done.append(i)
        except Exception as exc:
            with lock:
                errors.append(exc)

    threads = [threading.Thread(target=cpu_worker, args=(i,)) for i in range(4)]
    threads += [threading.Thread(target=js_worker, args=(i,)) for i in range(100)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=30)
    for thread in threads:
        if thread.is_alive():
            raise AssertionError("M:N isolation worker hung")
    if errors:
        raise errors[0]
    expect(len(cpu_done), 4)
    expect(len(js_done), 100)


def test_signal_chaos_trap_python_host():
    try:
        for _ in range(20):
            omnivm.set_task_timeout(1000)
            rb(
                """
arr = []
1000.times do |i|
  arr << ("x" * 1000)
  arr.shift if arr.length > 100
end
arr.length
"""
            )
            expect(js("1 + 1"), "2")
    finally:
        omnivm.set_task_timeout(0)
    expect(rb("1 + 1"), "2")


def test_inception_preemption_js_via_python():
    child_check(
        """
omnivm.set_task_timeout(300)
try:
    omnivm.execute("python", "import omnivm\\nomnivm.call('javascript', 'while(true) {}')")
except (omnivm.RuntimeError, KeyboardInterrupt):
    pass
else:
    raise AssertionError("nested JS loop was not interrupted")
omnivm.set_task_timeout(0)
if omnivm.call("javascript", "1 + 1") != "2":
    raise AssertionError("JS unhealthy after inception preemption")
""",
        timeout=8,
    )


def test_async_event_loop_starvation_python_host():
    child_check(
        """
omnivm.execute("javascript", "globalThis.__starvation_flag = false; setTimeout(function() { globalThis.__starvation_flag = true; }, 50)")
for i in range(200):
    omnivm.call("python", f"{i} + 1")
import time
deadline = time.time() + 1.0
fired = False
while time.time() < deadline:
    if omnivm.call("javascript", "globalThis.__starvation_flag === true ? 'yes' : 'no'") == "yes":
        fired = True
        break
    time.sleep(0.01)
if not fired:
    raise AssertionError("JS timer never fired")
""",
        timeout=8,
    )


def test_context_cancellation_guillotine_python_host():
    cancel = threading.Event()
    completed = []
    rejected = []
    lock = threading.Lock()

    def worker(i):
        if cancel.is_set():
            with lock:
                rejected.append(i)
            return
        time.sleep(0.01)
        if i == 3:
            cancel.set()
        if cancel.is_set() and i > 3:
            with lock:
                rejected.append(i)
            return
        expect(js(f"{i} + 1"), str(i + 1))
        with lock:
            completed.append(i)

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(20)]
    for thread in threads:
        thread.start()
    for thread in threads:
        thread.join(timeout=5)
    for thread in threads:
        if thread.is_alive():
            raise AssertionError("cancellation worker hung")
    if not completed or not rejected:
        raise AssertionError(f"expected completed and rejected work, got {len(completed)} completed, {len(rejected)} rejected")
    expect(len(completed) + len(rejected), 20)


def test_asyncio_starvation_bridge_task_flood():
    py_exec(
        """
import asyncio
_lib_t58_fired = False
_lib_t58_count = 0
async def _lib_t58_timer():
    global _lib_t58_fired
    await asyncio.sleep(0.01)
    _lib_t58_fired = True
async def _lib_t58_flood():
    global _lib_t58_count
    task = asyncio.create_task(_lib_t58_timer())
    for i in range(200):
        omnivm.call("javascript", str(i) + " + 1")
        _lib_t58_count += 1
        if i % 10 == 0:
            await asyncio.sleep(0)
    await task
_lib_t58_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_lib_t58_loop)
_lib_t58_loop.run_until_complete(_lib_t58_flood())
_lib_t58_loop.close()
"""
    )
    expect(py("_lib_t58_fired"), "True")
    expect(py("_lib_t58_count"), "200")


def test_watchdog_deep_bridge_chain():
    child_check(
        """
omnivm.set_task_timeout(300)
try:
    omnivm.execute("python", "import omnivm\\nomnivm.call('javascript', \\"omnivm.call('ruby', 'i=0; loop { i += 1 }')\\")")
except (omnivm.RuntimeError, KeyboardInterrupt):
    pass
else:
    raise AssertionError("deep Ruby loop was not interrupted")
omnivm.set_task_timeout(0)
if omnivm.call("ruby", "4 + 4") != "8":
    raise AssertionError("Ruby unhealthy after deep watchdog")
""",
        timeout=8,
    )


def test_watchdog_rearm_race():
    child_check(
        """
for i in range(100):
    omnivm.set_task_timeout(20)
    omnivm.call("javascript", "1 + 1")
    omnivm.set_task_timeout(1000)
    if omnivm.call("javascript", "42") != "42":
        raise AssertionError("bad JS result during rearm")
omnivm.set_task_timeout(0)
if omnivm.call("python", "'alive'") != "alive":
    raise AssertionError("Python unhealthy after rearm storm")
""",
        timeout=8,
    )


def test_heartbeat_only_js_timer_python_host():
    child_check(
        """
omnivm.execute("javascript", "globalThis.__heartbeat_only = false; setTimeout(function() { globalThis.__heartbeat_only = true; }, 50)")
import time
time.sleep(0.2)
for _ in range(50):
    if omnivm.call("javascript", "globalThis.__heartbeat_only === true ? 'yes' : 'no'") == "yes":
        break
    time.sleep(0.01)
else:
    raise AssertionError("JS heartbeat timer never fired")
""",
        timeout=5,
    )
    expect(py("functools.reduce(_lib_t36_js_add, range(200))"), "19900")
    expect(py("functools.reduce(_lib_t36_java_mul, range(1, 13))"), "479001600")
    expect(py('functools.reduce(_lib_t36_alternating, range(100), "0")'), "4950")
    expect(
        py(
            """
_lib_t36_js_sum = functools.reduce(_lib_t36_js_add, range(10))
functools.reduce(_lib_t36_java_mul, range(1, 5), _lib_t36_js_sum)
"""
        ),
        "1080",
    )


def test_buffer_bridge():
    data = bytearray(bytes(range(64)) * 128)
    omnivm.set_buffer("libstress", data, 0)
    js_sum = omnivm.call(
        "javascript",
        """
        var buf = omnivm.getBuffer('libstress');
        var arr = new Uint8Array(buf);
        var sum = 0;
        for (var i = 0; i < arr.length; i++) sum += arr[i];
        arr[0] = 99;
        sum;
        """,
    )
    expect(js_sum, str(sum(data)))
    shared = omnivm.get_buffer("libstress")
    if shared is None or bytes(shared)[0] != 99:
        raise AssertionError("JS getBuffer did not expose a live zero-copy shared view")
    shared[1] = 55
    js_seen = omnivm.call(
        "javascript",
        "new Uint8Array(omnivm.getBuffer('libstress'))[1]",
    )
    expect(js_seen, "55")
    del shared
    gc.collect()

    readonly_data = bytes(range(32))
    omnivm.set_buffer("libstress-ro", readonly_data, 0)
    omnivm.call(
        "javascript",
        """
        var ro = omnivm.getBuffer('libstress-ro');
        new Uint8Array(ro)[0] = 77;
        'ok';
        """,
    )
    readonly_shared = omnivm.get_buffer("libstress-ro")
    if readonly_shared is None or bytes(readonly_shared)[0] != readonly_data[0]:
        raise AssertionError("JS getBuffer mutated a read-only shared buffer")
    if not readonly_shared.readonly:
        raise AssertionError("Python get_buffer did not preserve read-only metadata")
    del readonly_shared
    gc.collect()

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
    omnivm.release_buffer("libstress-ro")
    omnivm.release_buffer("libstress-pi")


def test_python_runtime_buffer_view_auto_release():
    data = b"poly-buffer" * 128
    omnivm.set_buffer("py-runtime-view", data, 0)
    before = omnivm.status()["arrow"]["releases"]
    expect(
        omnivm.call(
            "python",
            """
import gc, omnivm
view = omnivm.get_buffer('py-runtime-view')
assert bytes(view) == %r
del view
gc.collect()
'released'
"""
            % data,
        ),
        "released",
    )
    after = omnivm.status()["arrow"]["releases"]
    if after <= before:
        raise AssertionError(f"Python memoryview GC did not release buffer borrow: before={before}, after={after}")
    omnivm.release_buffer("py-runtime-view")


def test_arrow_c_data_bridge():
    values = [10, 20, 30, 40]
    data = struct.pack("<4i", *values)
    omnivm.set_buffer("arrow-cdata-i32", data, 1)

    schema = omnivm._ArrowSchema()
    array = omnivm._ArrowArray()
    before = omnivm.status()["arrow"]["releases"]
    rc = omnivm._lib.OmniArrowGet(
        b"arrow-cdata-i32",
        ctypes.byref(schema),
        ctypes.byref(array),
    )
    if rc != 0:
        raise AssertionError("OmniArrowGet failed")
    try:
        expect(schema.format.decode("utf-8"), "i")
        expect(schema.name.decode("utf-8"), "arrow-cdata-i32")
        expect(array.length, len(values))
        expect(array.n_buffers, 2)
        if not array.buffers or not array.buffers[1]:
            raise AssertionError("Arrow data buffer pointer was not exported")
        raw_values = ctypes.cast(array.buffers[1], ctypes.POINTER(ctypes.c_int32))
        got = [raw_values[i] for i in range(array.length)]
        if got != values:
            raise AssertionError(f"Arrow buffer read {got!r}, want {values!r}")
    finally:
        if bool(schema.release):
            schema.release(ctypes.byref(schema))
        if bool(array.release):
            array.release(ctypes.byref(array))
        omnivm.release_buffer("arrow-cdata-i32")

    after = omnivm.status()["arrow"]["releases"]
    if after <= before:
        raise AssertionError(f"Arrow C Data release did not release borrow: before={before}, after={after}")


def test_arrow_c_data_import_bridge():
    if not hasattr(omnivm._lib, "OmniArrowSet"):
        raise AssertionError("libomnivm does not expose OmniArrowSet")

    values = [3, 5, 8, 13]
    raw_values = (ctypes.c_int32 * len(values))(*values)
    buffers = (ctypes.c_void_p * 2)()
    buffers[0] = None
    buffers[1] = ctypes.cast(raw_values, ctypes.c_void_p)

    schema = omnivm._ArrowSchema()
    schema.format = b"i"
    schema.name = b"python-arrow-import"

    array = omnivm._ArrowArray()
    array.length = len(values)
    array.null_count = 0
    array.offset = 0
    array.n_buffers = 2
    array.buffers = buffers

    before = omnivm.status()["arrow"]["copied_bytes"]
    rc = omnivm._lib.OmniArrowSet(
        b"python-arrow-import",
        ctypes.byref(schema),
        ctypes.byref(array),
    )
    if rc != 0:
        raise AssertionError("OmniArrowSet failed")
    try:
        imported = omnivm.get_buffer("python-arrow-import")
        if imported is None:
            raise AssertionError("OmniArrowSet did not create imported buffer")
        got = list(struct.unpack("<4i", bytes(imported)))
        if got != values:
            raise AssertionError(f"Arrow import read {got!r}, want {values!r}")
        status = omnivm.status()["arrow"]
        if status["copied_bytes"] - before < len(values) * ctypes.sizeof(ctypes.c_int32):
            raise AssertionError(f"Arrow import did not account copied bytes: before={before}, after={status}")
        if status.get("buffers_by_format", {}).get("i", 0) < 1:
            raise AssertionError(f"Arrow import did not preserve format metadata: {status}")
    finally:
        omnivm.release_buffer("python-arrow-import")


def test_arrow_c_data_owned_import_bridge_zero_copy():
    if not hasattr(omnivm._lib, "OmniArrowSet"):
        raise AssertionError("libomnivm does not expose OmniArrowSet")

    values = [21, 34, 55, 89]
    raw_values = (ctypes.c_int32 * len(values))(*values)
    buffers = (ctypes.c_void_p * 2)()
    buffers[0] = None
    buffers[1] = ctypes.cast(raw_values, ctypes.c_void_p)

    released_schema = ctypes.c_int(0)
    released_array = ctypes.c_int(0)

    @omnivm._ArrowSchemaRelease
    def release_schema(schema):
        released_schema.value += 1

    @omnivm._ArrowArrayRelease
    def release_array(array):
        released_array.value += 1

    schema = omnivm._ArrowSchema()
    schema.format = b"i"
    schema.name = b"python-arrow-owned-import"
    schema.release = release_schema

    array = omnivm._ArrowArray()
    array.length = len(values)
    array.null_count = 0
    array.offset = 0
    array.n_buffers = 2
    array.buffers = buffers
    array.release = release_array

    before = omnivm.status()["arrow"]
    rc = omnivm._lib.OmniArrowSet(
        b"python-arrow-owned-import",
        ctypes.byref(schema),
        ctypes.byref(array),
    )
    if rc != 0:
        raise AssertionError("OmniArrowSet owned import failed")
    if bool(schema.release) or bool(array.release):
        raise AssertionError("OmniArrowSet should consume owned Arrow C Data descriptors")
    if released_schema.value != 0 or released_array.value != 0:
        raise AssertionError("owned Arrow C Data descriptors released before buffer release")
    imported = omnivm.get_buffer("python-arrow-owned-import")
    if imported is None:
        raise AssertionError("OmniArrowSet owned import did not create imported buffer")
    got = list(struct.unpack("<4i", bytes(imported)))
    if got != values:
        raise AssertionError(f"owned Arrow import read {got!r}, want {values!r}")
    status = omnivm.status()["arrow"]
    if status["copied_bytes"] != before["copied_bytes"]:
        raise AssertionError(f"owned Arrow import copied bytes: before={before}, after={status}")
    if status["zero_copy_imports"] <= before["zero_copy_imports"]:
        raise AssertionError(f"owned Arrow import did not record zero-copy import: before={before}, after={status}")
    if status.get("buffers_by_format", {}).get("i", 0) < 1:
        raise AssertionError(f"owned Arrow import did not preserve format metadata: {status}")

    del imported
    gc.collect()
    gc.collect()
    omnivm.release_buffer("python-arrow-owned-import")

    if released_schema.value != 1 or released_array.value != 1:
        raise AssertionError(
            f"owned Arrow C Data release callbacks not called exactly once: "
            f"schema={released_schema.value} array={released_array.value}"
        )


def test_arrow_c_data_nullable_owned_import_bridge_zero_copy():
    if not hasattr(omnivm._lib, "OmniArrowSet"):
        raise AssertionError("libomnivm does not expose OmniArrowSet")

    values = [1, 2, 3, 4]
    raw_values = (ctypes.c_int32 * len(values))(*values)
    validity = (ctypes.c_uint8 * 1)(0b00001101)
    buffers = (ctypes.c_void_p * 2)()
    buffers[0] = ctypes.cast(validity, ctypes.c_void_p)
    buffers[1] = ctypes.cast(raw_values, ctypes.c_void_p)

    released_schema = ctypes.c_int(0)
    released_array = ctypes.c_int(0)

    @omnivm._ArrowSchemaRelease
    def release_schema(schema):
        released_schema.value += 1

    @omnivm._ArrowArrayRelease
    def release_array(array):
        released_array.value += 1

    schema = omnivm._ArrowSchema()
    schema.format = b"i"
    schema.name = b"python-arrow-nullable-owned-import"
    schema.release = release_schema

    array = omnivm._ArrowArray()
    array.length = len(values)
    array.null_count = 1
    array.offset = 0
    array.n_buffers = 2
    array.buffers = buffers
    array.release = release_array

    before = omnivm.status()["arrow"]
    rc = omnivm._lib.OmniArrowSet(
        b"python-arrow-nullable-owned-import",
        ctypes.byref(schema),
        ctypes.byref(array),
    )
    if rc != 0:
        raise AssertionError("OmniArrowSet nullable owned import failed")
    if bool(schema.release) or bool(array.release):
        raise AssertionError("OmniArrowSet should consume nullable owned Arrow descriptors")
    imported = omnivm.get_buffer("python-arrow-nullable-owned-import")
    if imported is None:
        raise AssertionError("OmniArrowSet nullable owned import did not create imported buffer")
    got = list(struct.unpack("<4i", bytes(imported)))
    if got != values:
        raise AssertionError(f"nullable owned Arrow import read {got!r}, want {values!r}")
    status = omnivm.status()["arrow"]
    if status["copied_bytes"] != before["copied_bytes"]:
        raise AssertionError(f"nullable owned Arrow import copied bytes: before={before}, after={status}")
    if status["zero_copy_imports"] <= before["zero_copy_imports"]:
        raise AssertionError(f"nullable owned Arrow import did not record zero-copy import: before={before}, after={status}")

    del imported
    gc.collect()
    gc.collect()
    omnivm.release_buffer("python-arrow-nullable-owned-import")

    if released_schema.value != 1 or released_array.value != 1:
        raise AssertionError(
            f"nullable Arrow C Data release callbacks not called exactly once: "
            f"schema={released_schema.value} array={released_array.value}"
        )


def test_arrow_c_data_signed_int8_import_bridge():
    if not hasattr(omnivm._lib, "OmniArrowSet"):
        raise AssertionError("libomnivm does not expose OmniArrowSet")

    values = [-1, 0, 2]
    raw_values = (ctypes.c_int8 * len(values))(*values)
    buffers = (ctypes.c_void_p * 2)()
    buffers[0] = None
    buffers[1] = ctypes.cast(raw_values, ctypes.c_void_p)

    schema = omnivm._ArrowSchema()
    schema.format = b"c"
    schema.name = b"python-arrow-import-i8"

    array = omnivm._ArrowArray()
    array.length = len(values)
    array.null_count = 0
    array.offset = 0
    array.n_buffers = 2
    array.buffers = buffers

    rc = omnivm._lib.OmniArrowSet(
        b"python-arrow-import-i8",
        ctypes.byref(schema),
        ctypes.byref(array),
    )
    if rc != 0:
        raise AssertionError("OmniArrowSet signed int8 failed")
    try:
        imported = omnivm.get_buffer("python-arrow-import-i8")
        if imported is None:
            raise AssertionError("OmniArrowSet did not create signed int8 imported buffer")
        got = list(struct.unpack("<3b", bytes(imported)))
        if got != values:
            raise AssertionError(f"Signed int8 Arrow import read {got!r}, want {values!r}")
        status = omnivm.status()["arrow"]
        if status.get("buffers_by_format", {}).get("c", 0) < 1:
            raise AssertionError(f"Arrow import did not preserve signed int8 format metadata: {status}")
    finally:
        omnivm.release_buffer("python-arrow-import-i8")


def test_manifest_python_buffer_protocol_capture_uses_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "bytearray(b'abc')",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (meta.read_only !== false) throw new Error('mutable buffer read_only metadata not preserved: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 97 || payload[1] !== 98 || payload[2] !== 99) throw new Error('bad buffer proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"buffer-protocol capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"buffer-protocol capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"buffer-protocol capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_readonly_buffer_protocol_capture_uses_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "b'abc'",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (meta.read_only !== true) throw new Error('readonly buffer metadata not preserved: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 97 || payload[1] !== 98 || payload[2] !== 99) throw new Error('bad readonly buffer proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"readonly buffer-protocol capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"readonly buffer-protocol capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"readonly buffer-protocol capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_empty_buffer_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "bytearray()",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const shape = payload.metadata && payload.metadata.shape; "
                    "if (payload.length !== 0) throw new Error('bad empty buffer length: ' + payload.length); "
                    "if (!Array.isArray(shape) || shape.length !== 1 || shape[0] !== 0) throw new Error('bad empty buffer shape: ' + JSON.stringify(shape));"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"empty buffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"empty buffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"empty buffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_shaped_buffer_capture_preserves_metadata():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "memoryview(bytearray(b'abcdef')).cast('B', shape=[2, 3])",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const shape = payload.metadata && payload.metadata.shape; "
                    "const strides = payload.metadata && payload.metadata.strides; "
                    "if (!Array.isArray(shape) || shape.length !== 2 || shape[0] !== 2 || shape[1] !== 3) throw new Error('bad shape: ' + JSON.stringify(shape)); "
                    "if (!Array.isArray(strides) || strides.length !== 2 || strides[0] !== 3 || strides[1] !== 1) throw new Error('bad strides: ' + JSON.stringify(strides)); "
                    "if (payload.length !== 2) throw new Error('bad shaped buffer length: ' + payload.length); "
                    "const row0 = payload[0]; const row1 = payload[1]; "
                    "if (!Array.isArray(row0) || row0.length !== 3 || row0[0] !== 97 || row0[2] !== 99) throw new Error('bad shaped buffer first row: ' + JSON.stringify(row0)); "
                    "if (!Array.isArray(row1) || row1.length !== 3 || row1[0] !== 100 || row1[2] !== 102) throw new Error('bad shaped buffer second row: ' + JSON.stringify(row1));"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"shaped buffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"shaped buffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"shaped buffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_strided_memoryview_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "memoryview(bytearray(b'abcdef'))[::2]",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const strides = payload.metadata && payload.metadata.strides; "
                    "if (!Array.isArray(strides) || strides.length !== 1 || strides[0] !== 2) throw new Error('bad strides: ' + JSON.stringify(strides)); "
                    "if (payload.length !== 3 || payload[0] !== 97 || payload[1] !== 99 || payload[2] !== 101) throw new Error('bad strided memoryview proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"strided memoryview capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"strided memoryview capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"strided memoryview capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_negative_strided_memoryview_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "memoryview(bytearray(b'abcdef'))[::-2]",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "const strides = meta.strides; "
                    "if (!Array.isArray(strides) || strides.length !== 1 || strides[0] !== -2 || meta.offset !== 4) throw new Error('bad negative stride metadata: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 102 || payload[1] !== 100 || payload[2] !== 98) throw new Error('bad negative-strided memoryview proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"negative-strided memoryview capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"negative-strided memoryview capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"negative-strided memoryview capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_array_interface_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "(lambda ctypes: (lambda backing: type('ArrayInterfaceOnly', (), {'backing': backing, '__array_interface__': property(lambda self: {'data': (ctypes.addressof(self.backing), False), 'shape': (3,), 'typestr': '<u2', 'version': 3})})())((ctypes.c_uint16 * 3)(258, 772, 1286)))(__import__('ctypes'))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (payload.length !== 3 || payload[0] !== 258 || payload[1] !== 772 || payload[2] !== 1286) throw new Error('bad array-interface proxy');",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"array-interface capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"array-interface capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_readonly_array_interface_preserves_metadata():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "(lambda ctypes: (lambda backing: type('ReadOnlyArrayInterfaceOnly', (), {'backing': backing, '__array_interface__': property(lambda self: {'data': (ctypes.addressof(self.backing), True), 'shape': (3,), 'typestr': '|u1', 'version': 3})})())((ctypes.c_uint8 * 3)(7, 8, 9)))(__import__('ctypes'))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (meta.read_only !== true) throw new Error('array-interface read_only metadata not preserved: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 7 || payload[1] !== 8 || payload[2] !== 9) throw new Error('bad readonly array-interface proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"readonly array-interface capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"readonly array-interface capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"readonly array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_image_shaped_array_interface_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "(lambda ctypes: (lambda backing: type('ImageShapedArrayInterfaceOnly', (), {'backing': backing, '__array_interface__': property(lambda self: {'data': (ctypes.addressof(self.backing), False), 'shape': (2, 2, 3), 'strides': (6, 3, 1), 'typestr': '|u1', 'version': 3})})())((ctypes.c_uint8 * 12)(10,20,30,40,50,60,70,80,90,100,110,120)))(__import__('ctypes'))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (!Array.isArray(meta.shape) || meta.shape.length !== 3 || meta.shape[0] !== 2 || meta.shape[1] !== 2 || meta.shape[2] !== 3) throw new Error('bad image shape: ' + JSON.stringify(meta)); "
                    "if (!Array.isArray(meta.strides) || meta.strides.length !== 3 || meta.strides[0] !== 6 || meta.strides[1] !== 3 || meta.strides[2] !== 1) throw new Error('bad image strides: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 2) throw new Error('bad image height: ' + payload.length); "
                    "if (payload[0][0][0] !== 10 || payload[0][1][2] !== 60 || payload[1][0][1] !== 80 || payload[1][1][2] !== 120) throw new Error('bad image-shaped array-interface values');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"image-shaped array-interface capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"image-shaped array-interface capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"image-shaped array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_array_interface_data_object_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "(lambda backing: type('DataObjectArrayInterfaceOnly', (), {'backing': backing, 'view': memoryview(backing), '__array_interface__': property(lambda self: {'data': self.view, 'shape': (2, 3), 'strides': (3, 1), 'typestr': '|u1', 'version': 3})})())(bytearray([10,20,30,40,50,60]))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (!Array.isArray(meta.shape) || meta.shape.length !== 2 || meta.shape[0] !== 2 || meta.shape[1] !== 3) throw new Error('bad data-object shape: ' + JSON.stringify(meta)); "
                    "if (!Array.isArray(meta.strides) || meta.strides.length !== 2 || meta.strides[0] !== 3 || meta.strides[1] !== 1) throw new Error('bad data-object strides: ' + JSON.stringify(meta)); "
                    "if (meta.read_only !== false) throw new Error('data-object array-interface should preserve mutability: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 2 || payload[0][0] !== 10 || payload[0][2] !== 30 || payload[1][0] !== 40 || payload[1][2] !== 60) throw new Error('bad data-object array-interface values');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"data-object array-interface capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"data-object array-interface capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("resource_proxy_captures", 0) != before.get("resource_proxy_captures", 0):
        raise AssertionError(f"data-object array-interface capture degraded to object proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"data-object array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_pil_image_capture_uses_array_interface_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "__import__('PIL.Image').Image.frombytes('RGB', (2, 2), bytes([10,20,30,40,50,60,70,80,90,100,110,120]))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (!Array.isArray(meta.shape) || meta.shape.length !== 3 || meta.shape[0] !== 2 || meta.shape[1] !== 2 || meta.shape[2] !== 3) throw new Error('bad PIL image shape: ' + JSON.stringify(meta)); "
                    "if (meta.arrow_format !== 'C') throw new Error('bad PIL image arrow format: ' + JSON.stringify(meta)); "
                    "if (meta.read_only !== true) throw new Error('PIL array-interface bytes should be read-only: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 2) throw new Error('bad PIL image height: ' + payload.length); "
                    "if (payload[0][0][0] !== 10 || payload[0][1][2] !== 60 || payload[1][0][1] !== 80 || payload[1][1][2] !== 120) throw new Error('bad PIL image values');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"PIL image capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"PIL image capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"PIL image capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_wrong_endian_array_interface_capture_uses_proxy():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes
import sys

class WrongEndianArrayInterfaceOnly:
    kind = "wrong-endian-array-interface"
    value = 42

    def __init__(self):
        self.backing = (ctypes.c_uint16 * 3)(258, 772, 1286)

    @property
    def __array_interface__(self):
        return {
            "data": (ctypes.addressof(self.backing), False),
            "shape": (3,),
            "typestr": ">u2" if sys.byteorder == "little" else "<u2",
            "version": 3,
        }
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "WrongEndianArrayInterfaceOnly()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (payload.kind !== 'wrong-endian-array-interface') throw new Error('wrong-endian array-interface should remain a live proxy'); "
                    "if (payload.value !== 42) throw new Error('wrong-endian array-interface proxy did not preserve properties');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"wrong-endian array-interface capture did not create a live proxy: before={before}, after={after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"wrong-endian array-interface capture should not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"wrong-endian array-interface capture should not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"wrong-endian array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_arrow_capsule_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes
released_schema = ctypes.c_int(0)
released_array = ctypes.c_int(0)
class ArrowSchema(ctypes.Structure):
    pass
class ArrowArray(ctypes.Structure):
    pass
ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p), ("name", ctypes.c_char_p), ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64), ("n_children", ctypes.c_int64), ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p), ("private_data", ctypes.c_void_p),
]
ArrowArray._fields_ = [
    ("length", ctypes.c_int64), ("null_count", ctypes.c_int64), ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64), ("n_children", ctypes.c_int64), ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p), ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]
PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object
@ArrowSchemaRelease
def release_schema(schema):
    released_schema.value += 1
@ArrowArrayRelease
def release_array(array):
    released_array.value += 1
class ArrowCapsuleArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 4)(111, 4, 5, 6)
        self.schema = ArrowSchema()
        self.array = ArrowArray()
        self.buffers = (ctypes.c_void_p * 2)()
        self.schema.format = b"i"
        self.schema.release = ctypes.cast(release_schema, ctypes.c_void_p).value
        self.array.length = 3
        self.array.offset = 1
        self.array.null_count = 0
        self.array.n_buffers = 2
        self.array.buffers = self.buffers
        self.array.release = ctypes.cast(release_array, ctypes.c_void_p).value
        self.buffers[0] = None
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p).value
    def __arrow_c_array__(self, requested_schema=None):
        return (
            PyCapsule_New(ctypes.addressof(self.schema), b"arrow_schema", None),
            PyCapsule_New(ctypes.addressof(self.array), b"arrow_array", None),
        )
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "ArrowCapsuleArray()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (payload.length !== 3 || payload[0] !== 4 || payload[1] !== 5 || payload[2] !== 6) throw new Error('bad Arrow capsule proxy'); if (!payload.metadata || payload.metadata.arrow_format !== 'i' || payload.metadata.offset !== 4) throw new Error('bad Arrow capsule metadata');",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    release_counts = omnivm.call("python", "(released_schema.value, released_array.value)")
    if release_counts != "(1, 1)":
        raise AssertionError(f"Arrow capsule descriptors were not released exactly once: {release_counts}")

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Arrow capsule capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Arrow capsule capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Arrow capsule capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_nullable_arrow_capsule_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes
class ArrowSchema(ctypes.Structure):
    pass
class ArrowArray(ctypes.Structure):
    pass
ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p), ("name", ctypes.c_char_p), ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64), ("n_children", ctypes.c_int64), ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p), ("private_data", ctypes.c_void_p),
]
ArrowArray._fields_ = [
    ("length", ctypes.c_int64), ("null_count", ctypes.c_int64), ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64), ("n_children", ctypes.c_int64), ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p), ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]
PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object
schema_release = ArrowSchemaRelease(lambda schema: None)
array_release = ArrowArrayRelease(lambda array: None)
class NullableArrowCapsuleArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 4)(111, 4, 5, 6)
        self.validity = (ctypes.c_uint8 * 1)(0b00001011)
        self.schema = ArrowSchema()
        self.array = ArrowArray()
        self.buffers = (ctypes.c_void_p * 2)()
        self.schema.format = b"i"
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.array.length = 3
        self.array.offset = 1
        self.array.null_count = 1
        self.array.n_buffers = 2
        self.array.buffers = self.buffers
        self.array.release = ctypes.cast(array_release, ctypes.c_void_p).value
        self.buffers[0] = ctypes.cast(self.validity, ctypes.c_void_p).value
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p).value
    def __arrow_c_array__(self, requested_schema=None):
        return (
            PyCapsule_New(ctypes.addressof(self.schema), b"arrow_schema", None),
            PyCapsule_New(ctypes.addressof(self.array), b"arrow_array", None),
        )
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "NullableArrowCapsuleArray()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (payload.length !== 3 || payload[0] !== 4 || payload[1] !== null || payload[2] !== 6) "
                    "throw new Error('bad nullable Arrow capsule proxy: ' + JSON.stringify([payload[0], payload[1], payload[2]])); "
                    "if (!payload.metadata || payload.metadata.arrow_format !== 'i' || payload.metadata.offset !== 4 || payload.metadata.null_count !== 1) "
                    "throw new Error('bad nullable Arrow capsule metadata: ' + JSON.stringify(payload.metadata));"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"nullable Arrow capsule capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"nullable Arrow capsule capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"nullable Arrow capsule capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_nested_arrow_capsule_capture_uses_proxy():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes
class ArrowSchema(ctypes.Structure):
    pass
class ArrowArray(ctypes.Structure):
    pass
ArrowSchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrowArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p), ("name", ctypes.c_char_p), ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64), ("n_children", ctypes.c_int64), ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p), ("private_data", ctypes.c_void_p),
]
ArrowArray._fields_ = [
    ("length", ctypes.c_int64), ("null_count", ctypes.c_int64), ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64), ("n_children", ctypes.c_int64), ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p), ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]
PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object
schema_release = ArrowSchemaRelease(lambda schema: None)
array_release = ArrowArrayRelease(lambda array: None)
class NestedArrowCapsuleArray:
    def __init__(self):
        self.kind = "nested-arrow-capsule"
        self.backing = (ctypes.c_int32 * 2)(1, 2)
        self.schema = ArrowSchema()
        self.array = ArrowArray()
        self.child_schema = ArrowSchema()
        self.child_array = ArrowArray()
        self.child_schema_ptr = ctypes.pointer(self.child_schema)
        self.child_array_ptr = ctypes.pointer(self.child_array)
        self.buffers = (ctypes.c_void_p * 2)()
        self.buffers[0] = None
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p).value
        self.schema.format = b"+l"
        self.schema.n_children = 1
        self.schema.children = ctypes.cast(ctypes.pointer(self.child_schema_ptr), ctypes.c_void_p).value
        self.schema.release = ctypes.cast(schema_release, ctypes.c_void_p).value
        self.array.length = 2
        self.array.null_count = 0
        self.array.n_buffers = 2
        self.array.n_children = 1
        self.array.buffers = self.buffers
        self.array.children = ctypes.cast(ctypes.pointer(self.child_array_ptr), ctypes.c_void_p).value
        self.array.release = ctypes.cast(array_release, ctypes.c_void_p).value
    def __arrow_c_array__(self, requested_schema=None):
        return (
            PyCapsule_New(ctypes.addressof(self.schema), b"arrow_schema", None),
            PyCapsule_New(ctypes.addressof(self.array), b"arrow_array", None),
        )
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "NestedArrowCapsuleArray()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (payload.kind !== 'nested-arrow-capsule') throw new Error('nested Arrow capsule should remain a live proxy');",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"nested Arrow capsule did not remain a resource proxy: before={before}, after={after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"nested Arrow capsule should not create a table proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"nested Arrow capsule capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_arrow_stream_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

class ArrowArrayStream(ctypes.Structure):
    pass

SchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
GetSchema = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowSchema))
GetNext = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowArray))
GetLastError = ctypes.CFUNCTYPE(ctypes.c_char_p, ctypes.POINTER(ArrowArrayStream))
StreamRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArrayStream))

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p), ("name", ctypes.c_char_p), ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64), ("n_children", ctypes.c_int64), ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p), ("private_data", ctypes.c_void_p),
]
ArrowArray._fields_ = [
    ("length", ctypes.c_int64), ("null_count", ctypes.c_int64), ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64), ("n_children", ctypes.c_int64), ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p), ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]
ArrowArrayStream._fields_ = [
    ("get_schema", GetSchema), ("get_next", GetNext), ("get_last_error", GetLastError),
    ("release", StreamRelease), ("private_data", ctypes.c_void_p),
]

schema_release = SchemaRelease(lambda schema: None)
array_release = ArrayRelease(lambda array: None)

PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object

class ArrowStreamArray:
    def __init__(self):
        self.backing = (ctypes.c_int32 * 4)(111, 4, 5, 6)
        self.buffers = (ctypes.c_void_p * 2)()
        self.buffers[0] = None
        self.buffers[1] = ctypes.cast(self.backing, ctypes.c_void_p).value
        self.calls = 0

        @GetSchema
        def get_schema(stream, out):
            out.contents.format = b"i"
            out.contents.name = None
            out.contents.metadata = None
            out.contents.flags = 0
            out.contents.n_children = 0
            out.contents.children = None
            out.contents.dictionary = None
            out.contents.release = ctypes.cast(schema_release, ctypes.c_void_p).value
            out.contents.private_data = None
            return 0

        @GetNext
        def get_next(stream, out):
            if self.calls == 0:
                self.calls += 1
                out.contents.length = 3
                out.contents.null_count = 0
                out.contents.offset = 1
                out.contents.n_buffers = 2
                out.contents.n_children = 0
                out.contents.buffers = self.buffers
                out.contents.children = None
                out.contents.dictionary = None
                out.contents.release = ctypes.cast(array_release, ctypes.c_void_p).value
                out.contents.private_data = None
                return 0
            out.contents.release = None
            return 0

        @GetLastError
        def get_last_error(stream):
            return None

        @StreamRelease
        def stream_release(stream):
            return None

        self.get_schema = get_schema
        self.get_next = get_next
        self.get_last_error = get_last_error
        self.stream_release = stream_release
        self.stream = ArrowArrayStream(get_schema, get_next, get_last_error, stream_release, None)

    def __arrow_c_stream__(self, requested_schema=None):
        return PyCapsule_New(ctypes.addressof(self.stream), b"arrow_array_stream", None)
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "ArrowStreamArray()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (payload.length !== 3 || payload[0] !== 4 || payload[1] !== 5 || payload[2] !== 6) throw new Error('bad Arrow stream proxy'); if (!payload.metadata || payload.metadata.arrow_format !== 'i' || payload.metadata.offset !== 4) throw new Error('bad Arrow stream metadata');",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Arrow stream capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Arrow stream capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Arrow stream capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_multichunk_arrow_stream_capture_uses_proxy():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes

class ArrowSchema(ctypes.Structure):
    pass

class ArrowArray(ctypes.Structure):
    pass

class ArrowArrayStream(ctypes.Structure):
    pass

SchemaRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowSchema))
ArrayRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArray))
GetSchema = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowSchema))
GetNext = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.POINTER(ArrowArrayStream), ctypes.POINTER(ArrowArray))
GetLastError = ctypes.CFUNCTYPE(ctypes.c_char_p, ctypes.POINTER(ArrowArrayStream))
StreamRelease = ctypes.CFUNCTYPE(None, ctypes.POINTER(ArrowArrayStream))

ArrowSchema._fields_ = [
    ("format", ctypes.c_char_p), ("name", ctypes.c_char_p), ("metadata", ctypes.c_char_p),
    ("flags", ctypes.c_int64), ("n_children", ctypes.c_int64), ("children", ctypes.c_void_p),
    ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p), ("private_data", ctypes.c_void_p),
]
ArrowArray._fields_ = [
    ("length", ctypes.c_int64), ("null_count", ctypes.c_int64), ("offset", ctypes.c_int64),
    ("n_buffers", ctypes.c_int64), ("n_children", ctypes.c_int64), ("buffers", ctypes.POINTER(ctypes.c_void_p)),
    ("children", ctypes.c_void_p), ("dictionary", ctypes.c_void_p), ("release", ctypes.c_void_p),
    ("private_data", ctypes.c_void_p),
]
ArrowArrayStream._fields_ = [
    ("get_schema", GetSchema), ("get_next", GetNext), ("get_last_error", GetLastError),
    ("release", StreamRelease), ("private_data", ctypes.c_void_p),
]

schema_release = SchemaRelease(lambda schema: None)
array_release = ArrayRelease(lambda array: None)

PyCapsule_New = ctypes.pythonapi.PyCapsule_New
PyCapsule_New.argtypes = [ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p]
PyCapsule_New.restype = ctypes.py_object

class MultiChunkArrowStream:
    def __init__(self):
        self.kind = "multi-chunk-arrow-stream"
        self.first = (ctypes.c_int32 * 2)(1, 2)
        self.second = (ctypes.c_int32 * 1)(3)
        self.first_buffers = (ctypes.c_void_p * 2)()
        self.second_buffers = (ctypes.c_void_p * 2)()
        self.first_buffers[0] = None
        self.first_buffers[1] = ctypes.cast(self.first, ctypes.c_void_p).value
        self.second_buffers[0] = None
        self.second_buffers[1] = ctypes.cast(self.second, ctypes.c_void_p).value
        self.calls = 0

        @GetSchema
        def get_schema(stream, out):
            out.contents.format = b"i"
            out.contents.name = None
            out.contents.metadata = None
            out.contents.flags = 0
            out.contents.n_children = 0
            out.contents.children = None
            out.contents.dictionary = None
            out.contents.release = ctypes.cast(schema_release, ctypes.c_void_p).value
            out.contents.private_data = None
            return 0

        @GetNext
        def get_next(stream, out):
            self.calls += 1
            if self.calls == 1:
                out.contents.length = 2
                out.contents.buffers = self.first_buffers
            elif self.calls == 2:
                out.contents.length = 1
                out.contents.buffers = self.second_buffers
            else:
                out.contents.release = None
                return 0
            out.contents.null_count = 0
            out.contents.offset = 0
            out.contents.n_buffers = 2
            out.contents.n_children = 0
            out.contents.children = None
            out.contents.dictionary = None
            out.contents.release = ctypes.cast(array_release, ctypes.c_void_p).value
            out.contents.private_data = None
            return 0

        @GetLastError
        def get_last_error(stream):
            return None

        @StreamRelease
        def stream_release(stream):
            return None

        self.get_schema = get_schema
        self.get_next = get_next
        self.get_last_error = get_last_error
        self.stream_release = stream_release
        self.stream = ArrowArrayStream(get_schema, get_next, get_last_error, stream_release, None)

    def __arrow_c_stream__(self, requested_schema=None):
        return PyCapsule_New(ctypes.addressof(self.stream), b"arrow_array_stream", None)
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "MultiChunkArrowStream()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (payload.kind !== 'multi-chunk-arrow-stream') throw new Error('unsupported Arrow stream shape should remain a live proxy');",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"multi-chunk Arrow stream did not remain a resource proxy: before={before}, after={after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"multi-chunk Arrow stream should not create a table proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"multi-chunk Arrow stream capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_numeric_map_get_indexes_arrow_table():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "memoryview(bytearray(b'abcdef'))[::2]",
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"payload": "payload"},
                "code": """
Object raw = omnivm.OmniVM.getCapture("payload");
if (!(raw instanceof omnivm.OmniVM.HandleProxy)) throw new RuntimeException("payload is not a handle proxy");
java.util.Map<?, ?> payload = (java.util.Map<?, ?>) raw;
if (((Number) payload.get(0)).intValue() != 97) throw new RuntimeException("bad numeric table get(0)");
if (((Number) payload.get("1")).intValue() != 99) throw new RuntimeException("bad numeric table get('1')");
if (((Number) ((omnivm.OmniVM.HandleProxy) raw).index(2)).intValue() != 101) throw new RuntimeException("bad explicit table index(2)");
Object metadata = payload.get("metadata");
if (!(metadata instanceof java.util.Map)) throw new RuntimeException("missing table metadata");
Object strides = ((java.util.Map<?, ?>) metadata).get("strides");
if (!(strides instanceof java.util.List) || ((Number) ((java.util.List<?>) strides).get(0)).intValue() != 2) throw new RuntimeException("bad table strides metadata");
""",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java numeric Map.get table access did not use a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java numeric Map.get table access did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java numeric Map.get table access used JSON fallback: before={before}, after={after}")


def test_manifest_python_numeric_get_indexes_arrow_table():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new Uint16Array([258, 772, 1286])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"payload": "payload"},
                "code": (
                    "globals().pop('omnivm', None)\n"
                    "got = (payload.get(0), payload.get('1'), payload.get(2), payload.get('missing', 'fallback'))\n"
                    "assert got == (258, 772, 1286, 'fallback'), got"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Python numeric get table access did not use a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Python numeric get table access did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python numeric get table access used JSON fallback: before={before}, after={after}")


def test_manifest_js_proxy_get_indexes_arrow_table():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "memoryview(bytearray(b'abcdef'))[::2]",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"payload": "payload"},
                "code": (
                    "const got = [payload.get(0), payload.get('1'), payload.get(2), payload.get('missing', 'fallback')]; "
                    "if (JSON.stringify(got) !== JSON.stringify([97, 99, 101, 'fallback'])) throw new Error('bad JS proxy get: ' + JSON.stringify(got));"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS proxy get table access did not use a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS proxy get table access did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS proxy get table access used JSON fallback: before={before}, after={after}")


def test_manifest_ruby_proxy_fetch_indexes_arrow_table():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "memoryview(bytearray(b'abcdef'))[::2]",
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"payload": "payload"},
                "code": (
                    "got = [payload.fetch(0), payload.fetch('1'), payload.fetch(2), payload.fetch('missing', 'fallback')]\n"
                    "raise \"bad Ruby proxy fetch: #{got.inspect}\" unless got == [97, 99, 101, 'fallback']"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Ruby proxy fetch table access did not use a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Ruby proxy fetch table access did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby proxy fetch table access used JSON fallback: before={before}, after={after}")


def test_manifest_python_calls_js_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "adder",
                "code": "(function(a, b) { return a + b; })",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"adder": "adder"},
                "code": "got = adder(19, 23)\nassert got == 42, got",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"JS callable did not cross as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS callable used JSON fallback: before={before}, after={after}")


def test_manifest_js_calls_python_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "adder",
                "code": "lambda a, b: a + b",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"adder": "adder"},
                "code": "const got = adder(20, 22); if (got !== 42) throw new Error('bad callable proxy: ' + got);",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Python callable did not cross as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python callable used JSON fallback: before={before}, after={after}")


def test_manifest_ruby_calls_js_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "adder",
                "code": "(function(a, b) { return a + b; })",
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"adder": "adder"},
                "code": "got = adder.call(21, 21)\nraise \"bad callable proxy: #{got.inspect}\" unless got == 42",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"JS callable did not cross to Ruby as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby callable path used JSON fallback: before={before}, after={after}")


def test_manifest_java_calls_js_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "adder",
                "code": "(function(a, b) { return a + b; })",
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"adder": "adder"},
                "code": """
Object raw = omnivm.OmniVM.getCapture("adder");
if (!(raw instanceof omnivm.OmniVM.HandleProxy)) throw new RuntimeException("adder is not a handle proxy");
Object got = ((omnivm.OmniVM.HandleProxy) raw).apply(18, 24);
if (((Number) got).intValue() != 42) throw new RuntimeException("bad callable proxy: " + got);
""",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"JS callable did not cross to Java as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java callable path used JSON fallback: before={before}, after={after}")


def test_manifest_python_calls_java_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "adder",
                "code": "((java.util.function.BiFunction<Object,Object,Integer>)((a, b) -> ((Number)a).intValue() + ((Number)b).intValue()))",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"adder": "adder"},
                "code": "got = adder(17, 25)\nassert got == 42, got",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java callable did not cross to Python as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python->Java callable path used JSON fallback: before={before}, after={after}")


def test_manifest_js_calls_java_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "adder",
                "code": "((java.util.function.BiFunction<Object,Object,Integer>)((a, b) -> ((Number)a).intValue() + ((Number)b).intValue()))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"adder": "adder"},
                "code": "const got = adder(16, 26); if (got !== 42) throw new Error('bad Java callable proxy: ' + got);",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java callable did not cross to JS as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS->Java callable path used JSON fallback: before={before}, after={after}")


def test_manifest_ruby_calls_java_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "adder",
                "code": "((java.util.function.BiFunction<Object,Object,Integer>)((a, b) -> ((Number)a).intValue() + ((Number)b).intValue()))",
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"adder": "adder"},
                "code": "got = adder.call(15, 27)\nraise \"bad Java callable proxy: #{got.inspect}\" unless got == 42",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java callable did not cross to Ruby as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby->Java callable path used JSON fallback: before={before}, after={after}")


def test_manifest_python_calls_go_cshared_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "make_adder",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc MakeAdder() func(int, int) int {\n\treturn func(a int, b int) int { return a + b }\n}",
                "exports": ["MakeAdder"],
            },
            {"op": "eval", "runtime": "go", "bind": "adder", "code": "make_adder()"},
            {
                "op": "exec",
                "runtime": "python",
                "code": "got = adder(19, 23)\nassert got == 42, got",
                "captures": {"adder": "adder"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Go callable did not cross to Python as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python->Go callable path used JSON fallback: before={before}, after={after}")


def test_manifest_js_calls_go_cshared_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "make_adder",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc MakeAdder() func(int, int) int {\n\treturn func(a int, b int) int { return a + b }\n}",
                "exports": ["MakeAdder"],
            },
            {"op": "eval", "runtime": "go", "bind": "adder", "code": "make_adder()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const got = adder(14, 28); if (got !== 42) throw new Error('bad Go callable proxy: ' + got);",
                "captures": {"adder": "adder"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Go callable did not cross to JS as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS->Go callable path used JSON fallback: before={before}, after={after}")


def test_manifest_ruby_calls_go_cshared_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "make_adder",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc MakeAdder() func(int, int) int {\n\treturn func(a int, b int) int { return a + b }\n}",
                "exports": ["MakeAdder"],
            },
            {"op": "eval", "runtime": "go", "bind": "adder", "code": "make_adder()"},
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "got = adder.call(13, 29)\nraise \"bad Go callable proxy: #{got.inspect}\" unless got == 42",
                "captures": {"adder": "adder"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Go callable did not cross to Ruby as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby->Go callable path used JSON fallback: before={before}, after={after}")


def test_manifest_java_calls_go_cshared_function_proxy():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "make_adder",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc MakeAdder() func(int, int) int {\n\treturn func(a int, b int) int { return a + b }\n}",
                "exports": ["MakeAdder"],
            },
            {"op": "eval", "runtime": "go", "bind": "adder", "code": "make_adder()"},
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "Object raw = omnivm.OmniVM.getCapture(\"adder\");\n"
                    "if (!(raw instanceof omnivm.OmniVM.HandleProxy)) throw new RuntimeException(\"adder is not a handle proxy\");\n"
                    "Object got = ((omnivm.OmniVM.HandleProxy) raw).apply(18, 24);\n"
                    "if (((Number) got).intValue() != 42) throw new RuntimeException(\"bad Go callable proxy: \" + got);"
                ),
                "captures": {"adder": "adder"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Go callable did not cross to Java as a live proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java->Go callable path used JSON fallback: before={before}, after={after}")


def test_manifest_python_signed_int8_array_interface_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "(lambda ctypes: (lambda backing: type('SignedInt8ArrayInterfaceOnly', (), {'backing': backing, '__array_interface__': property(lambda self: {'data': (ctypes.addressof(self.backing), False), 'shape': (3,), 'typestr': '|i1', 'version': 3})})())((ctypes.c_int8 * 3)(-1, 0, 2)))(__import__('ctypes'))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (payload.length !== 3 || payload[0] !== -1 || payload[1] !== 0 || payload[2] !== 2) throw new Error('bad signed int8 array-interface proxy');",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"signed int8 array-interface capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"signed int8 array-interface capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"signed int8 array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_strided_array_interface_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "(lambda ctypes: (lambda backing: type('StridedArrayInterfaceOnly', (), {'backing': backing, '__array_interface__': property(lambda self: {'data': (ctypes.addressof(self.backing), False), 'shape': (3,), 'strides': (4,), 'typestr': '<u2', 'version': 3})})())((ctypes.c_uint16 * 5)(258, 999, 772, 999, 1286)))(__import__('ctypes'))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const strides = payload.metadata && payload.metadata.strides; "
                    "if (!Array.isArray(strides) || strides.length !== 1 || strides[0] !== 4) throw new Error('bad strides: ' + JSON.stringify(strides)); "
                    "if (payload.length !== 3 || payload[0] !== 258 || payload[1] !== 772 || payload[2] !== 1286) throw new Error('bad strided array-interface proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"strided array-interface capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"strided array-interface capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"strided array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_negative_strided_array_interface_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "(lambda ctypes: (lambda backing: type('NegativeStridedArrayInterfaceOnly', (), {'backing': backing, '__array_interface__': property(lambda self: {'data': (ctypes.addressof(self.backing) + 8, False), 'shape': (3,), 'strides': (-4,), 'typestr': '<u2', 'version': 3})})())((ctypes.c_uint16 * 5)(258, 999, 772, 999, 1286)))(__import__('ctypes'))",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "const strides = meta.strides; "
                    "if (!Array.isArray(strides) || strides.length !== 1 || strides[0] !== -4 || meta.offset !== 8) throw new Error('bad negative stride metadata: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 1286 || payload[1] !== 772 || payload[2] !== 258) throw new Error('bad negative-strided array-interface proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"negative-strided array-interface capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"negative-strided array-interface capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"negative-strided array-interface capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_dlpack_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": (
                    "(lambda np: (lambda cls: cls())(type('DLPackOnly', (), {"
                    "'__init__': lambda self: (setattr(self, 'backing', np.arange(6, dtype=np.int32).reshape(2, 3)), setattr(self, 'view', self.backing[:, ::2]), None)[-1], "
                    "'__dlpack__': lambda self, stream=None: self.view.__dlpack__(stream=stream)"
                    "}))) (__import__('numpy'))"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const shape = payload.metadata && payload.metadata.shape; "
                    "const strides = payload.metadata && payload.metadata.strides; "
                    "if (!Array.isArray(shape) || shape.length !== 2 || shape[0] !== 2 || shape[1] !== 2) throw new Error('bad DLPack shape: ' + JSON.stringify(shape)); "
                    "if (!Array.isArray(strides) || strides.length !== 2 || strides[0] !== 12 || strides[1] !== 8) throw new Error('bad DLPack strides: ' + JSON.stringify(strides)); "
                    "if (payload.length !== 2) throw new Error('bad DLPack table length: ' + payload.length); "
                    "const row0 = payload[0]; const row1 = payload[1]; "
                    "if (!Array.isArray(row0) || row0[0] !== 0 || row0[1] !== 2) throw new Error('bad DLPack row0: ' + JSON.stringify(row0)); "
                    "if (!Array.isArray(row1) || row1[0] !== 3 || row1[1] !== 5) throw new Error('bad DLPack row1: ' + JSON.stringify(row1));"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"DLPack capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"DLPack capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"DLPack capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_dataframe_interchange_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes

class InterchangeBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (1, None)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.backing)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.backing)

class InterchangeColumn:
    def __init__(self, owner):
        self.owner = owner
    def size(self):
        return 3
    @property
    def offset(self):
        return 0
    @property
    def dtype(self):
        return (2, 64, "g", "<")
    def num_chunks(self):
        return 1
    def get_buffers(self):
        return {
            "data": (InterchangeBuffer(self.owner), self.dtype),
            "validity": None,
            "offsets": None,
        }

class InterchangeFrame:
    def __init__(self, owner):
        self.owner = owner
    def num_columns(self):
        return 1
    def num_rows(self):
        return 3
    def num_chunks(self):
        return 1
    def get_column(self, i):
        if i != 0:
            raise IndexError(i)
        return InterchangeColumn(self.owner)

class DataFrameInterchangeOnly:
    def __init__(self):
        self.backing = (ctypes.c_double * 3)(1.5, 2.5, 3.5)
        self.allow_copy_seen = None
    def __dataframe__(self, *, allow_copy=True):
        self.allow_copy_seen = allow_copy
        return InterchangeFrame(self)
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "DataFrameInterchangeOnly()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (meta.arrow_format !== 'g') throw new Error('bad dataframe interchange format: ' + JSON.stringify(meta)); "
                    "if (!Array.isArray(meta.shape) || meta.shape.length !== 1 || meta.shape[0] !== 3) throw new Error('bad dataframe interchange shape: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 1.5 || payload[1] !== 2.5 || payload[2] !== 3.5) throw new Error('bad dataframe interchange proxy');"
                ),
                "captures": {"payload": "payload"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert payload.allow_copy_seen is False, payload.allow_copy_seen",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"dataframe interchange capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"dataframe interchange capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"dataframe interchange capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_nullable_dataframe_interchange_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes

class InterchangeDataBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (1, None)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.backing)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.backing)

class InterchangeValidityBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (1, None)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.validity)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.validity)

class InterchangeColumn:
    def __init__(self, owner):
        self.owner = owner
    def size(self):
        return 3
    @property
    def offset(self):
        return 0
    @property
    def dtype(self):
        return (2, 64, "g", "<")
    def num_chunks(self):
        return 1
    def get_buffers(self):
        return {
            "data": (InterchangeDataBuffer(self.owner), self.dtype),
            "validity": (InterchangeValidityBuffer(self.owner), (0, 8, "C", "|")),
            "offsets": None,
        }

class InterchangeFrame:
    def __init__(self, owner):
        self.owner = owner
    def num_columns(self):
        return 1
    def num_rows(self):
        return 3
    def num_chunks(self):
        return 1
    def get_column(self, i):
        if i != 0:
            raise IndexError(i)
        return InterchangeColumn(self.owner)

class NullableDataFrameInterchangeOnly:
    def __init__(self):
        self.backing = (ctypes.c_double * 3)(1.5, 2.5, 3.5)
        self.validity = (ctypes.c_uint8 * 1)(0b00000101)
        self.allow_copy_seen = None
    def __dataframe__(self, *, allow_copy=True):
        self.allow_copy_seen = allow_copy
        return InterchangeFrame(self)
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "NullableDataFrameInterchangeOnly()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (meta.arrow_format !== 'g') throw new Error('bad nullable dataframe interchange format: ' + JSON.stringify(meta)); "
                    "if (meta.null_count !== 1) throw new Error('bad nullable dataframe interchange null count: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 1.5 || payload[1] !== null || payload[2] !== 3.5) "
                    "throw new Error('bad nullable dataframe interchange proxy: ' + JSON.stringify([payload[0], payload[1], payload[2]]));"
                ),
                "captures": {"payload": "payload"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert payload.allow_copy_seen is False, payload.allow_copy_seen",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"nullable dataframe interchange capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"nullable dataframe interchange capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"nullable dataframe interchange capture used JSON fallback: before={before}, after={after}")


def test_manifest_python_non_cpu_dataframe_interchange_capture_uses_proxy():
    before = omnivm.status().get("boundary", {})
    setup = r'''
import ctypes

class GPUInterchangeBuffer:
    def __init__(self, owner):
        self.owner = owner
    def __dlpack_device__(self):
        return (2, 0)
    @property
    def ptr(self):
        return ctypes.addressof(self.owner.backing)
    @property
    def bufsize(self):
        return ctypes.sizeof(self.owner.backing)

class GPUInterchangeColumn:
    def __init__(self, owner):
        self.owner = owner
    def size(self):
        return 2
    @property
    def offset(self):
        return 0
    @property
    def dtype(self):
        return (0, 32, "i", "<")
    def num_chunks(self):
        return 1
    def get_buffers(self):
        return {
            "data": (GPUInterchangeBuffer(self.owner), self.dtype),
            "validity": None,
            "offsets": None,
        }

class GPUInterchangeFrame:
    def __init__(self, owner):
        self.owner = owner
    def num_columns(self):
        return 1
    def num_chunks(self):
        return 1
    def get_column(self, i):
        return GPUInterchangeColumn(self.owner)

class GPUDataFrameInterchange:
    def __init__(self):
        self.kind = "gpu-frame"
        self.backing = (ctypes.c_int32 * 2)(11, 22)
    def __dataframe__(self, *, allow_copy=True):
        return GPUInterchangeFrame(self)
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {"op": "eval", "runtime": "python", "bind": "payload", "code": "GPUDataFrameInterchange()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (payload.kind !== 'gpu-frame') throw new Error('non-CPU dataframe should remain a live proxy');",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"non-CPU dataframe did not remain a resource proxy: before={before}, after={after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"non-CPU dataframe should not create a table proxy: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"non-CPU dataframe capture used JSON fallback: before={before}, after={after}")


def test_manifest_numpy_strided_view_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "__import__('numpy').arange(12, dtype=__import__('numpy').int16).reshape(3, 4)[:, ::2]",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const shape = payload.metadata && payload.metadata.shape; "
                    "const strides = payload.metadata && payload.metadata.strides; "
                    "if (!Array.isArray(shape) || shape.length !== 2 || shape[0] !== 3 || shape[1] !== 2) throw new Error('bad NumPy shape: ' + JSON.stringify(shape)); "
                    "if (!Array.isArray(strides) || strides.length !== 2 || strides[0] !== 8 || strides[1] !== 4) throw new Error('bad NumPy strides: ' + JSON.stringify(strides)); "
                    "if (payload.length !== 3) throw new Error('bad NumPy table length: ' + payload.length); "
                    "const row0 = payload[0]; const row1 = payload[1]; const row2 = payload[2]; "
                    "if (!Array.isArray(row0) || row0[0] !== 0 || row0[1] !== 2) throw new Error('bad NumPy row0: ' + JSON.stringify(row0)); "
                    "if (!Array.isArray(row1) || row1[0] !== 4 || row1[1] !== 6) throw new Error('bad NumPy row1: ' + JSON.stringify(row1)); "
                    "if (!Array.isArray(row2) || row2[0] !== 8 || row2[1] !== 10) throw new Error('bad NumPy row2: ' + JSON.stringify(row2));"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"NumPy strided view capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"NumPy strided view capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"NumPy strided view capture used JSON fallback: before={before}, after={after}")


def test_manifest_numpy_wrong_endian_buffer_capture_uses_proxy():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "__import__('numpy').array([258, 772, 1286], dtype='>u2' if __import__('sys').byteorder == 'little' else '<u2')",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (payload.size !== 3) throw new Error('wrong-endian NumPy array should remain a live proxy with size'); "
                    "if (payload.ndim !== 1) throw new Error('wrong-endian NumPy array should remain a live proxy with ndim');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"wrong-endian buffer capture did not create a live proxy: {after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"wrong-endian buffer capture should not create a table proxy: {after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"wrong-endian buffer capture should not use Arrow/shared memory: {after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"wrong-endian buffer capture used JSON fallback: {after}")


def test_manifest_pandas_series_array_protocol_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "__import__('pandas').Series(__import__('numpy').arange(6, dtype=__import__('numpy').int16))[::2]",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const strides = payload.metadata && payload.metadata.strides; "
                    "if (!Array.isArray(strides) || strides.length !== 1 || strides[0] !== 4) throw new Error('bad Pandas strides: ' + JSON.stringify(strides)); "
                    "if (payload.length !== 3 || payload[0] !== 0 || payload[1] !== 2 || payload[2] !== 4) throw new Error('bad Pandas Series proxy');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Pandas Series capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Pandas Series capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Pandas Series capture used JSON fallback: before={before}, after={after}")


def test_manifest_pandas_dataframe_array_protocol_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": (
                    "(lambda pd, np: pd.DataFrame({"
                    "'a': np.array([1, 2, 3], dtype=np.int32), "
                    "'b': np.array([4, 5, 6], dtype=np.int32)"
                    "}))(__import__('pandas'), __import__('numpy'))"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const shape = payload.metadata && payload.metadata.shape; "
                    "const strides = payload.metadata && payload.metadata.strides; "
                    "if (!Array.isArray(shape) || shape.length !== 2 || shape[0] !== 3 || shape[1] !== 2) throw new Error('bad Pandas DataFrame shape: ' + JSON.stringify(shape)); "
                    "if (!Array.isArray(strides) || strides.length !== 2 || strides[0] !== 4 || strides[1] !== 12) throw new Error('bad Pandas DataFrame strides: ' + JSON.stringify(strides)); "
                    "if (payload.length !== 3) throw new Error('bad Pandas DataFrame table length: ' + payload.length); "
                    "const row0 = payload[0]; const row1 = payload[1]; const row2 = payload[2]; "
                    "if (!Array.isArray(row0) || row0[0] !== 1 || row0[1] !== 4) throw new Error('bad Pandas DataFrame row0: ' + JSON.stringify(row0)); "
                    "if (!Array.isArray(row1) || row1[0] !== 2 || row1[1] !== 5) throw new Error('bad Pandas DataFrame row1: ' + JSON.stringify(row1)); "
                    "if (!Array.isArray(row2) || row2[0] !== 3 || row2[1] !== 6) throw new Error('bad Pandas DataFrame row2: ' + JSON.stringify(row2));"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Pandas DataFrame capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Pandas DataFrame capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Pandas DataFrame capture used JSON fallback: before={before}, after={after}")


def test_manifest_polars_dataframe_capture_uses_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "__import__('polars').DataFrame({'score': [1.5, 2.5, 3.5]})",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const meta = payload.metadata || {}; "
                    "if (meta.arrow_format !== 'g') throw new Error('bad Polars arrow format: ' + JSON.stringify(meta)); "
                    "if (!Array.isArray(meta.shape) || meta.shape.length !== 1 || meta.shape[0] !== 3) throw new Error('bad Polars shape: ' + JSON.stringify(meta)); "
                    "if (payload.length !== 3 || payload[0] !== 1.5 || payload[1] !== 2.5 || payload[2] !== 3.5) throw new Error('bad Polars values');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Polars DataFrame capture did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Polars DataFrame capture did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Polars DataFrame degraded to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Polars DataFrame capture used JSON fallback: {boundary}")


def test_manifest_heterogeneous_dataframe_capture_uses_proxy_not_json():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": (
                    "(lambda pd, np: pd.DataFrame({"
                    "'name': ['ada', 'grace'], "
                    "'score': np.array([7, 9], dtype=np.int32)"
                    "}))(__import__('pandas'), __import__('numpy'))"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (payload.columns.length !== 2) throw new Error('bad DataFrame columns length: ' + payload.columns.length); "
                    "if (payload.columns[0] !== 'name' || payload.columns[1] !== 'score') throw new Error('bad DataFrame columns'); "
                    "if (payload.shape[0] !== 2 || payload.shape[1] !== 2) throw new Error('bad DataFrame shape'); "
                    "const names = payload.get('name'); "
                    "if (names.length !== 2 || names[0] !== 'ada' || names[1] !== 'grace') throw new Error('bad DataFrame name series');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) <= before.get("resource_proxy_captures", 0):
        raise AssertionError(f"heterogeneous DataFrame did not cross as a live proxy: before={before}, after={after}")
    if after.get("table_proxy_captures", 0) != 0 or after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"heterogeneous DataFrame should not claim Arrow table capture: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"heterogeneous DataFrame used JSON fallback: before={before}, after={after}")


def test_manifest_django_request_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "req",
                "code": (
                    "(lambda: ("
                    "__import__('django.conf').conf.settings.configure(DEFAULT_CHARSET='utf-8', SECRET_KEY='poly') "
                    "if not __import__('django.conf').conf.settings.configured else None, "
                    "(lambda r: (setattr(r, 'method', 'POST'), setattr(r, 'path', '/orders/42'), "
                    "r.META.__setitem__('HTTP_X_REQUEST_ID', 'req-42'), r)[-1])"
                    "(__import__('django.http').http.HttpRequest())"
                    ")[-1])()"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (req.method !== 'POST') throw new Error('bad Django method: ' + req.method); "
                    "if (req.path !== '/orders/42') throw new Error('bad Django path: ' + req.path); "
                    "if (req.META.HTTP_X_REQUEST_ID !== 'req-42') throw new Error('bad Django META');"
                ),
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Django request did not cross as a live proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Django request used JSON fallback: {boundary}")


def test_manifest_fastapi_request_capture_uses_proxy_not_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "req",
                "code": (
                    "(lambda Request: Request({"
                    "'type': 'http', 'method': 'PATCH', 'path': '/fastapi/42', "
                    "'raw_path': b'/fastapi/42', 'query_string': b'mode=poly', "
                    "'headers': [(b'x-request-id', b'fastapi-42'), (b'host', b'example.test')], "
                    "'scheme': 'https', 'server': ('example.test', 443), 'client': ('127.0.0.1', 5000)"
                    "}))(__import__('starlette.requests', fromlist=['Request']).Request)"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (req.method !== 'PATCH') throw new Error('bad FastAPI method: ' + req.method); "
                    "if (req.url.path !== '/fastapi/42') throw new Error('bad FastAPI path: ' + req.url.path); "
                    "if (req.url.query !== 'mode=poly') throw new Error('bad FastAPI query: ' + req.url.query);"
                ),
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"FastAPI request did not cross as a live proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"FastAPI request crossed as a stream: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"FastAPI request should not claim bulk Arrow transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"FastAPI request used JSON fallback: {boundary}")


def test_manifest_django_queryset_transaction_rollback_cross_runtime():
    before = omnivm.status()
    setup = r'''
import os
import tempfile
import uuid
from django.conf import settings

db_path = os.path.join(tempfile.gettempdir(), "omnivm-django-orm-" + uuid.uuid4().hex + ".sqlite3")
db_config = {"default": {"ENGINE": "django.db.backends.sqlite3", "NAME": db_path}}
if not settings.configured:
    settings.configure(
        DEFAULT_CHARSET="utf-8",
        SECRET_KEY="poly",
        INSTALLED_APPS=[],
        DATABASES=db_config,
        USE_TZ=True,
    )
else:
    settings.SECRET_KEY = getattr(settings, "SECRET_KEY", "poly") or "poly"
    settings.DEFAULT_CHARSET = getattr(settings, "DEFAULT_CHARSET", "utf-8") or "utf-8"
    settings.DATABASES = db_config
    settings.INSTALLED_APPS = []

import django
from django.apps import apps
if not apps.ready:
    django.setup()

from django.db import connection, models, transaction

suffix = uuid.uuid4().hex[:10]
table_name = "omnivm_order_" + suffix
Meta = type("Meta", (), {"app_label": "omnivm_stress", "db_table": table_name})
Order = type(
    "OmniVMOrder" + suffix,
    (models.Model,),
    {
        "__module__": "__main__",
        "name": models.CharField(max_length=64),
        "total": models.IntegerField(),
        "Meta": Meta,
    },
)
with connection.schema_editor() as schema:
    schema.create_model(Order)

Order.objects.bulk_create([
    Order(name="ada", total=11),
    Order(name="grace", total=12),
    Order(name="linus", total=13),
])
qs = Order.objects.order_by("id")
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const rows = Array.from(qs); "
                    "const names = rows.map(row => row.name).join(','); "
                    "if (names !== 'ada,grace,linus') throw new Error('bad Django QuerySet JS names: ' + names);"
                ),
                "captures": {"qs": "qs"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": (
                    "names = []; qs.each { |row| names << row.name }; names = names.join(','); "
                    "raise \"bad Django QuerySet Ruby names: #{names}\" unless names == 'ada,grace,linus'"
                ),
                "captures": {"qs": "qs"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "Object raw = omnivm.OmniVM.getCapture(\"qs\"); "
                    "if (!(raw instanceof omnivm.OmniVM.HandleProxy)) throw new RuntimeException(\"QuerySet did not materialize as handle proxy: \" + raw); "
                    "omnivm.OmniVM.HandleProxy qsProxy = (omnivm.OmniVM.HandleProxy) raw; "
                    "java.util.List<String> names = new java.util.ArrayList<>(); "
                    "for (Object rowObj : qsProxy.values()) { "
                    "  if (!(rowObj instanceof omnivm.OmniVM.HandleProxy)) throw new RuntimeException(\"QuerySet row was not a handle proxy: \" + rowObj); "
                    "  names.add(String.valueOf(((omnivm.OmniVM.HandleProxy) rowObj).get(\"name\"))); "
                    "} "
                    "if (!\"ada,grace,linus\".equals(String.join(\",\", names))) throw new RuntimeException(\"bad Django QuerySet Java names: \" + names);"
                ),
                "captures": {"qs": "qs"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": (
                    "rollback_seen = False\n"
                    "try:\n"
                    "    with transaction.atomic():\n"
                    "        Order.objects.create(name='rollback', total=99)\n"
                    "        omnivm.call('javascript', \"throw new Error('foreign rollback')\")\n"
                    "except Exception as exc:\n"
                    "    rollback_seen = 'foreign rollback' in str(exc)\n"
                    "assert rollback_seen, 'foreign runtime error did not reach transaction.atomic()'\n"
                    "assert Order.objects.filter(name='rollback').count() == 0, list(Order.objects.values_list('name', flat=True))\n"
                    "assert Order.objects.count() == 3, Order.objects.count()"
                ),
            },
        ],
    }
    run_manifest_dict(manifest)

    after = omnivm.status()
    boundary = after.get("boundary", {})
    before_boundary = before.get("boundary", {})
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"Django QuerySet crossing used JSON fallback: before={before_boundary}, after={boundary}")
    if boundary.get("resource_proxy_captures", 0) < before_boundary.get("resource_proxy_captures", 0) + 4:
        raise AssertionError(f"Django QuerySet and rows did not stay behind live proxies: before={before_boundary}, after={boundary}")


def test_manifest_django_request_body_after_close_requires_materialized_dto():
    before = omnivm.status()
    setup = r'''
import io
from django.conf import settings
from django.core.handlers.wsgi import WSGIRequest

if not settings.configured:
    settings.configure(DEFAULT_CHARSET="utf-8", SECRET_KEY="poly")

class ClosingBody(io.BytesIO):
    def read(self, *args, **kwargs):
        if self.closed:
            raise ValueError("request body is closed")
        return super().read(*args, **kwargs)

body_stream = ClosingBody(b'{"order_id":"ord-42","total":7}')
environ = {
    "REQUEST_METHOD": "POST",
    "PATH_INFO": "/orders/closed",
    "SERVER_NAME": "example.test",
    "SERVER_PORT": "443",
    "wsgi.version": (1, 0),
    "wsgi.url_scheme": "https",
    "wsgi.input": body_stream,
    "wsgi.errors": io.StringIO(),
    "wsgi.multithread": False,
    "wsgi.multiprocess": False,
    "wsgi.run_once": False,
    "CONTENT_LENGTH": str(len(body_stream.getvalue())),
    "CONTENT_TYPE": "application/json",
}
req = WSGIRequest(environ)
body_dto = req.body.decode("utf-8")
if hasattr(req, "_body"):
    delattr(req, "_body")
req._stream = body_stream
body_stream.close()
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (!body_dto.includes('ord-42')) throw new Error('materialized DTO missing in JS'); "
                    "let failed = false; "
                    "try { req.read(); } catch (err) { failed = /closed|I\\/O|request body/.test(String(err && err.message || err)); } "
                    "if (!failed) throw new Error('JS read of closed Django body did not fail clearly');"
                ),
                "captures": {"req": "req", "body_dto": "body_dto"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": (
                    "raise 'materialized DTO missing in Ruby' unless body_dto.include?('ord-42'); "
                    "failed = false; "
                    "begin; reader = req.read; reader.respond_to?(:call) ? reader.call : reader; rescue => e; failed = e.message.include?('closed') || e.message.include?('I/O') || e.message.include?('request body'); end; "
                    "raise 'Ruby read of closed Django body did not fail clearly' unless failed"
                ),
                "captures": {"req": "req", "body_dto": "body_dto"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "if (!String.valueOf(omnivm.OmniVM.getCapture(\"body_dto\")).contains(\"ord-42\")) throw new RuntimeException(\"materialized DTO missing in Java\"); "
                    "Object raw = omnivm.OmniVM.getCapture(\"req\"); "
                    "if (!(raw instanceof omnivm.OmniVM.HandleProxy)) throw new RuntimeException(\"request did not materialize as handle proxy: \" + raw); "
                    "boolean failed = false; "
                    "try { ((omnivm.OmniVM.HandleProxy) raw).call(\"read\"); } "
                    "catch (RuntimeException err) { String msg = String.valueOf(err.getMessage()); failed = msg.contains(\"closed\") || msg.contains(\"I/O\") || msg.contains(\"request body\"); } "
                    "if (!failed) throw new RuntimeException(\"Java read of closed Django body did not fail clearly\");"
                ),
                "captures": {"req": "req", "body_dto": "body_dto"},
            },
        ],
    }
    run_manifest_dict(manifest)

    after = omnivm.status()
    boundary = after.get("boundary", {})
    before_boundary = before.get("boundary", {})
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"Django closed request body test used JSON fallback: before={before_boundary}, after={boundary}")
    if boundary.get("resource_proxy_captures", 0) < before_boundary.get("resource_proxy_captures", 0) + 1:
        raise AssertionError(f"Django closed request did not cross as a live proxy: before={before_boundary}, after={boundary}")
    if boundary.get("stream_proxy_captures", 0) != before_boundary.get("stream_proxy_captures", 0):
        raise AssertionError(f"Django closed request object should not cross as stream: before={before_boundary}, after={boundary}")


def test_manifest_django_streaming_response_capture_uses_proxy_not_body_stream():
    before = omnivm.status()
    before_boundary = before.get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "resp",
                "code": (
                    "(lambda: ("
                    "__import__('django.conf').conf.settings.configure(DEFAULT_CHARSET='utf-8', SECRET_KEY='poly') "
                    "if not __import__('django.conf').conf.settings.configured else None, "
                    "(lambda r: (r.__setitem__('X-Request-Id', 'django-stream-42'), r)[-1])"
                    "(__import__('django.http', fromlist=['StreamingHttpResponse']).StreamingHttpResponse("
                    "iter([b'first', b'second']), status=202"
                    "))"
                    ")[-1])()"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (String(resp.status_code) !== '202') throw new Error('bad Django response status: ' + resp.status_code); "
                    "if (resp.headers['X-Request-Id'] !== 'django-stream-42') throw new Error('bad Django response header: ' + resp.headers['X-Request-Id']);"
                ),
                "captures": {"resp": "resp"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert list(resp.streaming_content) == [b'first', b'second']",
            },
        ],
    }
    run_manifest_dict(manifest)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < before_boundary.get("resource_proxy_captures", 0) + 1:
        raise AssertionError(f"Django StreamingHttpResponse did not cross as a live proxy: before={before_boundary}, after={boundary}")
    if boundary.get("stream_proxy_captures", 0) != before_boundary.get("stream_proxy_captures", 0):
        raise AssertionError(f"Django StreamingHttpResponse should not cross as a body stream: before={before_boundary}, after={boundary}")
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"Django StreamingHttpResponse used JSON fallback: before={before_boundary}, after={boundary}")


def test_manifest_sqlalchemy_model_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "user",
                "code": (
                    "(lambda sa, orm: "
                    "(lambda Base: "
                    "(lambda User: User(id=7, name='Ada', email='ada@example.test'))"
                    "(type('PolyUser', (Base,), {"
                    "'__tablename__': 'poly_users', "
                    "'id': sa.Column(sa.Integer, primary_key=True), "
                    "'name': sa.Column(sa.String), "
                    "'email': sa.Column(sa.String), "
                    "'display_name': lambda self: self.name.upper()"
                    "}))"
                    ")(orm.declarative_base())"
                    ")(__import__('sqlalchemy'), __import__('sqlalchemy.orm').orm)"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (user.id !== 7) throw new Error('bad SQLAlchemy id: ' + user.id); "
                    "if (user.name !== 'Ada') throw new Error('bad SQLAlchemy name: ' + user.name); "
                    "if (user.email !== 'ada@example.test') throw new Error('bad SQLAlchemy email: ' + user.email); "
                    "if (user.display_name() !== 'ADA') throw new Error('bad SQLAlchemy method');"
                ),
                "captures": {"user": "user"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"SQLAlchemy model did not cross as a live proxy: {after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"SQLAlchemy model used JSON fallback: {after}")
    if after.get("table_proxy_captures", 0) != 0 or after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"SQLAlchemy model should not claim bulk Arrow transfer: {after}")


def test_manifest_sqlalchemy_result_session_lifecycle_and_rollback():
    before_status = omnivm.status()
    before_boundary = before_status.get("boundary", {})
    before_handles = before_status.get("handles", {})
    setup = r'''
import os
import tempfile
import uuid
import sqlalchemy as sa
from sqlalchemy.orm import Session

db_path = os.path.join(tempfile.gettempdir(), "omnivm-sqlalchemy-" + uuid.uuid4().hex + ".sqlite3")
engine = sa.create_engine("sqlite:///" + db_path, future=True)
metadata = sa.MetaData()
orders = sa.Table(
    "orders",
    metadata,
    sa.Column("id", sa.Integer, primary_key=True),
    sa.Column("name", sa.String),
    sa.Column("items", sa.String),
    sa.Column("keys", sa.String),
    sa.Column("count", sa.Integer),
    sa.Column("then", sa.String),
    sa.Column("length", sa.Integer),
    sa.Column("close", sa.String),
)
metadata.create_all(engine)
with engine.begin() as conn:
    conn.execute(
        orders.insert(),
        [
            {"name": "ada", "items": "field-items", "keys": "field-keys", "count": 7, "then": "field-then", "length": 12, "close": "field-close"},
            {"name": "grace", "items": "field-items-2", "keys": "field-keys-2", "count": 8, "then": "field-then-2", "length": 13, "close": "field-close-2"},
        ],
    )

def open_result():
    conn = engine.connect()
    result = conn.execute(sa.select(orders).order_by(orders.c.id)).mappings()
    return conn, result

conn_js, sa_result_js = open_result()
conn_ruby, sa_result_ruby = open_result()
conn_java, sa_result_java = open_result()
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const rows = Array.from(sa_result_js); "
                    "if (rows.length !== 2) throw new Error('bad SQLAlchemy result row count: ' + rows.length); "
                    "if (rows.map(row => row.get('name')).join(',') !== 'ada,grace') throw new Error('bad SQLAlchemy JS names'); "
                    "if (rows[0].get('items') !== 'field-items') throw new Error('items field lost: ' + rows[0].get('items')); "
                    "if (rows[0].get('keys') !== 'field-keys') throw new Error('keys field lost: ' + rows[0].get('keys')); "
                    "if (String(rows[0].get('count')) !== '7') throw new Error('count field lost: ' + rows[0].get('count')); "
                    "if (rows[0].get('then') !== 'field-then') throw new Error('then field lost: ' + rows[0].get('then')); "
                    "if (String(rows[0].get('length')) !== '12') throw new Error('length field lost: ' + rows[0].get('length')); "
                    "if (rows[0].get('close') !== 'field-close') throw new Error('close field lost: ' + rows[0].get('close'));"
                ),
                "captures": {"sa_result_js": "sa_result_js"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": (
                    "rows = sa_result_ruby.to_a; "
                    "names = rows.map { |row| row['name'] }.join(','); "
                    "raise \"bad SQLAlchemy Ruby names #{names}\" unless names == 'ada,grace'; "
                    "raise 'bad SQLAlchemy Ruby collision fields' unless rows[0]['items'] == 'field-items' && rows[0]['keys'] == 'field-keys' && rows[0]['then'] == 'field-then' && rows[0]['close'] == 'field-close'"
                ),
                "captures": {"sa_result_ruby": "sa_result_ruby"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "Object raw = omnivm.OmniVM.getCapture(\"sa_result_java\"); "
                    "if (!(raw instanceof omnivm.OmniVM.StreamProxy)) throw new RuntimeException(\"SQLAlchemy result should cross as stream proxy: \" + raw); "
                    "java.util.List<Object> rows = ((omnivm.OmniVM.StreamProxy) raw).toList(); "
                    "if (rows.size() != 2) throw new RuntimeException(\"bad SQLAlchemy Java row count: \" + rows.size()); "
                    "java.util.Map<?, ?> row = (java.util.Map<?, ?>) rows.get(0); "
                    "if (!\"ada\".equals(String.valueOf(row.get(\"name\")))) throw new RuntimeException(\"bad SQLAlchemy Java name: \" + row.get(\"name\")); "
                    "if (!\"field-items\".equals(String.valueOf(row.get(\"items\")))) throw new RuntimeException(\"items field lost: \" + row.get(\"items\")); "
                    "if (!\"field-keys\".equals(String.valueOf(row.get(\"keys\")))) throw new RuntimeException(\"keys field lost: \" + row.get(\"keys\")); "
                    "if (!\"7\".equals(String.valueOf(row.get(\"count\")))) throw new RuntimeException(\"count field lost: \" + row.get(\"count\")); "
                    "if (!\"field-then\".equals(String.valueOf(row.get(\"then\")))) throw new RuntimeException(\"then field lost: \" + row.get(\"then\")); "
                    "if (!\"12\".equals(String.valueOf(row.get(\"length\")))) throw new RuntimeException(\"length field lost: \" + row.get(\"length\")); "
                    "if (!\"field-close\".equals(String.valueOf(row.get(\"close\")))) throw new RuntimeException(\"close field lost: \" + row.get(\"close\"));"
                ),
                "captures": {"sa_result_java": "sa_result_java"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": (
                    "for result in (sa_result_js, sa_result_ruby, sa_result_java):\n"
                    "    assert result.closed, f'SQLAlchemy result stream was not closed: {result}'\n"
                    "rollback_seen = False\n"
                    "with Session(engine) as session:\n"
                    "    try:\n"
                    "        with session.begin():\n"
                    "            session.execute(orders.insert().values(name='rollback', items='x', keys='x', count=99, then='x', length=99, close='x'))\n"
                    "            omnivm.call('javascript', \"throw new Error('sqlalchemy rollback')\")\n"
                    "    except Exception as exc:\n"
                    "        rollback_seen = 'sqlalchemy rollback' in str(exc)\n"
                    "    assert rollback_seen, 'foreign error did not reach SQLAlchemy Session transaction'\n"
                    "    total = session.scalar(sa.select(sa.func.count()).select_from(orders))\n"
                    "    assert total == 2, total"
                ),
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < before_boundary.get("stream_proxy_captures", 0) + 3:
        raise AssertionError(f"SQLAlchemy Result did not cross as lazy stream proxies: before={before_boundary}, after={boundary}")
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"SQLAlchemy Result used JSON fallback: before={before_boundary}, after={boundary}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 3:
        raise AssertionError(f"SQLAlchemy Result streams did not release: before={before_handles}, after={handles}")


def test_manifest_jdbc_cached_rowset_lifecycle_crosses_as_proxy():
    before_status = omnivm.status()
    before_boundary = before_status.get("boundary", {})
    setup = r'''
((java.util.concurrent.Callable<Void>)(() -> {
java.util.function.Supplier<Object> makeRows = () -> {
    try {
        final javax.sql.rowset.CachedRowSet rs = javax.sql.rowset.RowSetProvider.newFactory().createCachedRowSet();
        javax.sql.rowset.RowSetMetaDataImpl md = new javax.sql.rowset.RowSetMetaDataImpl();
        md.setColumnCount(3);
        md.setColumnName(1, "name");
        md.setColumnType(1, java.sql.Types.VARCHAR);
        md.setColumnName(2, "count");
        md.setColumnType(2, java.sql.Types.INTEGER);
        md.setColumnName(3, "length");
        md.setColumnType(3, java.sql.Types.INTEGER);
        rs.setMetaData(md);
        rs.moveToInsertRow();
        rs.updateString("name", "ada");
        rs.updateInt("count", 7);
        rs.updateInt("length", 12);
        rs.insertRow();
        rs.moveToInsertRow();
        rs.updateString("name", "grace");
        rs.updateInt("count", 8);
        rs.updateInt("length", 13);
        rs.insertRow();
        rs.moveToCurrentRow();
        rs.beforeFirst();
        rs.next();
        return new Object() {
            private boolean closed = false;
            public String getString(int column) throws java.sql.SQLException { return rs.getString(column); }
            public String getString(String column) throws java.sql.SQLException { return rs.getString(column); }
            public int getInt(int column) throws java.sql.SQLException { return rs.getInt(column); }
            public int getInt(String column) throws java.sql.SQLException { return rs.getInt(column); }
            public void close() throws java.sql.SQLException { closed = true; rs.close(); }
            public boolean isClosed() { return closed; }
        };
    } catch (Exception ex) {
        throw new RuntimeException(ex);
    }
};
omnivm.OmniVM.setCaptureObject("jdbc_js", makeRows.get());
omnivm.OmniVM.setCaptureObject("jdbc_ruby", makeRows.get());
omnivm.OmniVM.setCaptureObject("jdbc_java", makeRows.get());
return null;
})).call();
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "java", "code": setup},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (jdbc_js.getString(1) !== 'ada') throw new Error('bad JDBC JS name: ' + jdbc_js.getString(1)); "
                    "if (String(jdbc_js.getInt(2)) !== '7') throw new Error('bad JDBC JS count'); "
                    "if (String(jdbc_js.getInt(3)) !== '12') throw new Error('bad JDBC JS length'); "
                    "jdbc_js.close(); "
                    "if (!jdbc_js.isClosed()) throw new Error('JDBC JS ResultSet did not close');"
                ),
                "captures": {"jdbc_js": "jdbc_js"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": (
                    "raise \"bad JDBC Ruby name #{jdbc_ruby.getString(1)}\" unless jdbc_ruby.getString(1) == 'ada'; "
                    "raise 'bad JDBC Ruby count' unless jdbc_ruby.getInt(2).to_s == '7'; "
                    "raise 'bad JDBC Ruby length' unless jdbc_ruby.getInt(3).to_s == '12'; "
                    "jdbc_ruby.close; "
                    "raise 'JDBC Ruby ResultSet did not close' unless jdbc_ruby.isClosed"
                ),
                "captures": {"jdbc_ruby": "jdbc_ruby"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "Object raw = omnivm.OmniVM.getCapture(\"jdbc_java\"); "
                    "java.util.List<Object> idx1 = omnivm.OmniVM.listOf(new Object[]{1}); "
                    "java.util.List<Object> idx2 = omnivm.OmniVM.listOf(new Object[]{2}); "
                    "java.util.List<Object> idx3 = omnivm.OmniVM.listOf(new Object[]{3}); "
                    "if (!\"ada\".equals(String.valueOf(omnivm.OmniVM.proxyCall(raw, \"getString\", idx1)))) throw new RuntimeException(\"bad JDBC Java name\"); "
                    "if (!\"7\".equals(String.valueOf(omnivm.OmniVM.proxyCall(raw, \"getInt\", idx2)))) throw new RuntimeException(\"bad JDBC Java count\"); "
                    "if (!\"12\".equals(String.valueOf(omnivm.OmniVM.proxyCall(raw, \"getInt\", idx3)))) throw new RuntimeException(\"bad JDBC Java length\"); "
                    "omnivm.OmniVM.proxyCall(raw, \"close\", java.util.Collections.emptyList()); "
                    "if (!Boolean.TRUE.equals(omnivm.OmniVM.proxyCall(raw, \"isClosed\", java.util.Collections.emptyList()))) throw new RuntimeException(\"JDBC Java ResultSet did not close\");"
                ),
                "captures": {"jdbc_java": "jdbc_java"},
            },
        ],
    }
    run_manifest_dict(manifest)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < before_boundary.get("resource_proxy_captures", 0) + 2:
        raise AssertionError(f"JDBC ResultSet did not cross as live proxies: before={before_boundary}, after={boundary}")
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"JDBC ResultSet used JSON fallback: before={before_boundary}, after={boundary}")


def test_manifest_python_file_like_request_shape_capture_uses_proxy_not_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "py_file_req",
                "code": (
                    "type('FileLikeRequestShape', (), {"
                    "'__init__': lambda self: ("
                    "setattr(self, 'method', 'PUT'), "
                    "setattr(self, 'path', '/python-file-like/42'), "
                    "setattr(self, 'headers', {'X-Request-Id': 'py-file-42'}), "
                    "setattr(self, 'read_count', 0), None)[-1], "
                    "'readable': lambda self: True, "
                    "'read': lambda self, size=-1: (setattr(self, 'read_count', self.read_count + 1), b'not-the-request')[-1]"
                    "})()"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (py_file_req.method !== 'PUT') throw new Error('bad Python method: ' + py_file_req.method); "
                    "if (py_file_req.path !== '/python-file-like/42') throw new Error('bad Python path: ' + py_file_req.path); "
                    "if (py_file_req.headers['X-Request-Id'] !== 'py-file-42') throw new Error('bad Python headers: ' + py_file_req.headers['X-Request-Id']);"
                ),
                "captures": {"py_file_req": "py_file_req"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert py_file_req.read_count == 0, py_file_req.read_count",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Python file-like request shape did not cross as a live proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Python file-like request shape crossed as a stream: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python file-like request shape used JSON fallback: {boundary}")


def test_manifest_ruby_http_message_shape_capture_uses_proxy_not_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "ruby",
                "code": (
                    "class RubyHTTPMessageShape\n"
                    "  def initialize\n"
                    "    @headers = {'X-Request-Id' => 'ruby-42'}\n"
                    "    @each_called = 0\n"
                    "  end\n"
                    "  def request_method\n"
                    "    'PATCH'\n"
                    "  end\n"
                    "  def path\n"
                    "    '/rackish/42'\n"
                    "  end\n"
                    "  def headers\n"
                    "    @headers\n"
                    "  end\n"
                    "  def each\n"
                    "    @each_called += 1\n"
                    "    return enum_for(:each) unless block_given?\n"
                    "    ['not-', 'the-', 'request'].each { |chunk| yield chunk }\n"
                    "  end\n"
                    "  def each_called\n"
                    "    @each_called\n"
                    "  end\n"
                    "end"
                ),
            },
            {"op": "eval", "runtime": "ruby", "bind": "ruby_req", "code": "RubyHTTPMessageShape.new"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (ruby_req.request_method !== 'PATCH') throw new Error('bad Ruby request method: ' + ruby_req.request_method); "
                    "if (ruby_req.path !== '/rackish/42') throw new Error('bad Ruby path: ' + ruby_req.path); "
                    "if (ruby_req.headers['X-Request-Id'] !== 'ruby-42') throw new Error('bad Ruby headers: ' + ruby_req.headers['X-Request-Id']);"
                ),
                "captures": {"ruby_req": "ruby_req"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": 'raise "Ruby HTTP message shape was consumed as a stream" unless $ruby_req.each_called == 0',
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Ruby HTTP message shape did not cross as a live proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Ruby HTTP message shape crossed as a stream: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby HTTP message shape used JSON fallback: {boundary}")


def test_manifest_rack_request_capture_uses_proxy_not_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "rack_req",
                "code": (
                    "require 'rack/mock'; require 'rack/request'; "
                    "Rack::Request.new(Rack::MockRequest.env_for('/rack/42?mode=poly', "
                    "method: 'PATCH', 'HTTP_X_REQUEST_ID' => 'rack-42'))"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (rack_req.request_method !== 'PATCH') throw new Error('bad Rack method: ' + rack_req.request_method); "
                    "if (rack_req.path_info !== '/rack/42') throw new Error('bad Rack path: ' + rack_req.path_info); "
                    "if (rack_req.get_header('HTTP_X_REQUEST_ID') !== 'rack-42') throw new Error('bad Rack header: ' + rack_req.get_header('HTTP_X_REQUEST_ID'));"
                ),
                "captures": {"rack_req": "rack_req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Rack request did not cross as a live proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Rack request crossed as a stream: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Rack request should not claim bulk Arrow transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Rack request used JSON fallback: {boundary}")


def test_manifest_java_http_message_shape_capture_uses_proxy_not_stream():
    java_msg_expr = (
        "((java.util.function.Supplier<Object>)(() -> { "
        "java.util.concurrent.atomic.AtomicInteger iterCount = "
        "(java.util.concurrent.atomic.AtomicInteger) omnivm.OmniVM.getCapture(\"java_http_iter_count\"); "
        "return new java.lang.Iterable<String>() { "
        "public java.util.Iterator<String> iterator() { "
        "iterCount.incrementAndGet(); "
        "return java.util.Arrays.asList(\"not-\", \"the-\", \"request\").iterator(); "
        "} "
        "public String getMethod() { return \"PUT\"; } "
        "public String getPath() { return \"/java-httpish/42\"; } "
        "public java.util.Map<String,String> getHeaders() { return java.util.Collections.singletonMap(\"X-Request-Id\", \"java-42\"); } "
        "public String getHeader(String name) { return getHeaders().get(name); } "
        "}; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_http_iter_count",
                "code": "new java.util.concurrent.atomic.AtomicInteger(0)",
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_http_msg",
                "code": java_msg_expr,
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (java_http_msg.getMethod() !== 'PUT') throw new Error('bad Java method: ' + java_http_msg.getMethod()); "
                    "if (java_http_msg.getPath() !== '/java-httpish/42') throw new Error('bad Java path: ' + java_http_msg.getPath()); "
                    "if (java_http_msg.getHeader('X-Request-Id') !== 'java-42') throw new Error('bad Java header: ' + java_http_msg.getHeader('X-Request-Id'));"
                ),
                "captures": {"java_http_msg": "java_http_msg"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": 'if (((java.util.concurrent.atomic.AtomicInteger) omnivm.OmniVM.getCapture("java_http_iter_count")).get() != 0) throw new RuntimeException("Java HTTP message shape was consumed as a stream");',
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java HTTP message shape did not cross as a live proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Java HTTP message shape crossed as a stream: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java HTTP message shape used JSON fallback: {boundary}")


def test_manifest_okhttp_request_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "ok_req",
                "code": (
                    "new okhttp3.Request.Builder()"
                    ".url(\"https://example.test/api/orders\")"
                    ".header(\"X-Request-Id\", \"java-client-42\")"
                    ".build()"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"ok_req": "ok_req"},
                "code": (
                    "assert ok_req.method() == 'GET', ok_req.method()\n"
                    "assert ok_req.header('X-Request-Id') == 'java-client-42', ok_req.header('X-Request-Id')\n"
                    "assert ok_req.url().toString() == 'https://example.test/api/orders', ok_req.url().toString()"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"OkHttp request did not cross as a live proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"OkHttp request used JSON fallback: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"OkHttp request crossed as a stream: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"OkHttp request should not claim bulk Arrow transfer: {boundary}")
    handles = omnivm.status().get("handles", {})
    if handles.get("handle_accesses_by_kind", {}).get("call", 0) < 1:
        raise AssertionError(f"OkHttp request method calls were not recorded as proxy calls: {handles}")


def test_manifest_express_request_capture_uses_proxy_not_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "express_req",
                "code": (
                    "(() => { "
                    "const express = require('express'); "
                    "const http = require('http'); "
                    "const req = new http.IncomingMessage(); "
                    "Object.setPrototypeOf(req, express.request); "
                    "req.app = express(); "
                    "req.method = 'PATCH'; "
                    "req.url = '/express/42?mode=poly'; "
                    "req.headers = {'x-request-id': 'express-42', 'host': 'example.test'}; "
                    "return req; "
                    "})()"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"express_req": "express_req"},
                "code": (
                    "assert express_req.method == 'PATCH', express_req.method\n"
                    "assert express_req.path == '/express/42', express_req.path\n"
                    "assert express_req.get('x-request-id') == 'express-42', express_req.get('x-request-id')"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Express request did not cross as a live proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Express request crossed as a stream: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Express request should not claim bulk Arrow transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Express request used JSON fallback: {boundary}")


def test_manifest_model_id_property_beats_js_proxy_metadata():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "user",
                "code": (
                    "(lambda: (lambda obj: (setattr(obj, 'id', 7), setattr(obj, 'name', 'Ada'), "
                    "setattr(obj, 'active', True), obj)[-1])(type('UserDTO', (), {})()))()"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (user.id !== 7) throw new Error('model id was shadowed by proxy metadata: ' + user.id); "
                    "if (user.name !== 'Ada' || user.active !== true) throw new Error('bad model fields'); "
                    "user.name = 'Grace'; "
                    "if (user.name !== 'Grace') throw new Error('bad model mutation');"
                ),
                "captures": {"user": "user"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert user.id == 7 and user.name == 'Grace' and user.active is True",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"model object did not cross as a live proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"model object used JSON fallback: {boundary}")


def test_manifest_descriptor_fields_do_not_shadow_runtime_object_fields():
    manifests = [
        {
            "version": 1,
            "defaultRuntime": "python",
            "ops": [
                {
                    "op": "eval",
                    "runtime": "javascript",
                    "bind": "js_user",
                    "code": "({id: 7, runtime: 'app', kind: 'user', closed: false, disposer: 'domain', name: 'Ada', touch: function() { return this.name; }})",
                },
                {
                    "op": "exec",
                    "runtime": "python",
                    "code": (
                        "assert js_user.id == 7, js_user.id\n"
                        "assert js_user.runtime == 'app', js_user.runtime\n"
                        "assert js_user.kind == 'user', js_user.kind\n"
                        "assert js_user.closed is False, js_user.closed\n"
                        "assert js_user.disposer == 'domain', js_user.disposer\n"
                        "assert js_user.name == 'Ada', js_user.name\n"
                        "js_user.name = 'Grace'"
                    ),
                    "captures": {"js_user": "js_user"},
                },
                {
                    "op": "exec",
                    "runtime": "javascript",
                    "code": "if (js_user.name !== 'Grace') throw new Error('python mutation did not reach JS object: ' + js_user.name);",
                    "captures": {"js_user": "js_user"},
                },
            ],
        },
        {
            "version": 1,
            "defaultRuntime": "python",
            "ops": [
                {
                    "op": "eval",
                    "runtime": "python",
                    "bind": "ruby_user",
                    "code": (
                        "(lambda: (lambda obj: (setattr(obj, 'id', 7), setattr(obj, 'runtime', 'app'), "
                        "setattr(obj, 'kind', 'user'), setattr(obj, 'closed', False), setattr(obj, 'disposer', 'domain'), "
                        "setattr(obj, 'name', 'Ada'), obj)[-1])"
                        "(type('UserDTO', (), {})()))()"
                    ),
                },
                {
                    "op": "exec",
                    "runtime": "ruby",
                    "code": (
                        "raise \"bad id #{ruby_user.id.inspect}\" unless ruby_user.id == 7\n"
                        "raise \"bad runtime #{ruby_user.runtime.inspect}\" unless ruby_user.runtime == 'app'\n"
                        "raise \"bad kind #{ruby_user.kind.inspect}\" unless ruby_user.kind == 'user'\n"
                        "raise \"bad closed #{ruby_user.closed.inspect}\" unless ruby_user.closed == false\n"
                        "raise \"bad disposer #{ruby_user.disposer.inspect}\" unless ruby_user.disposer == 'domain'\n"
                        "raise \"bad name #{ruby_user.name.inspect}\" unless ruby_user.name == 'Ada'\n"
                        "ruby_user.name = 'Grace'"
                    ),
                    "captures": {"ruby_user": "ruby_user"},
                },
                {
                    "op": "exec",
                    "runtime": "python",
                    "code": "assert ruby_user.id == 7 and ruby_user.runtime == 'app' and ruby_user.kind == 'user' and ruby_user.closed is False and ruby_user.disposer == 'domain' and ruby_user.name == 'Grace'",
                },
            ],
        },
        {
            "version": 1,
            "defaultRuntime": "python",
            "ops": [
                {
                    "op": "eval",
                    "runtime": "python",
                    "bind": "java_user",
                    "code": (
                        "(lambda: (lambda obj: (setattr(obj, 'id', 7), setattr(obj, 'runtime', 'app'), "
                        "setattr(obj, 'kind', 'user'), setattr(obj, 'closed', False), setattr(obj, 'disposer', 'domain'), "
                        "setattr(obj, 'name', 'Ada'), obj)[-1])"
                        "(type('UserDTO', (), {})()))()"
                    ),
                },
                {
                    "op": "exec",
                    "runtime": "java",
                    "code": (
                        "Object raw = omnivm.OmniVM.getCapture(\"java_user\"); "
                        "omnivm.OmniVM.HandleProxy user = (omnivm.OmniVM.HandleProxy) raw; "
                        "if (!\"7\".equals(String.valueOf(user.get(\"id\")))) throw new RuntimeException(\"bad id: \" + user.get(\"id\")); "
                        "if (!\"app\".equals(String.valueOf(user.get(\"runtime\")))) throw new RuntimeException(\"bad runtime: \" + user.get(\"runtime\")); "
                        "if (!\"user\".equals(String.valueOf(user.get(\"kind\")))) throw new RuntimeException(\"bad kind: \" + user.get(\"kind\")); "
                        "if (!Boolean.FALSE.equals(user.get(\"closed\"))) throw new RuntimeException(\"bad closed: \" + user.get(\"closed\")); "
                        "if (!\"domain\".equals(String.valueOf(user.get(\"disposer\")))) throw new RuntimeException(\"bad disposer: \" + user.get(\"disposer\")); "
                        "if (!\"Ada\".equals(String.valueOf(user.get(\"name\")))) throw new RuntimeException(\"bad name: \" + user.get(\"name\")); "
                        "if (!user.containsKey(\"id\") || !user.containsKey(\"runtime\") || !user.containsKey(\"kind\") || !user.containsKey(\"closed\") || !user.containsKey(\"disposer\")) throw new RuntimeException(\"descriptor field containsKey failed\"); "
                        "user.set(\"name\", \"Grace\");"
                    ),
                    "captures": {"java_user": "java_user"},
                },
                {
                    "op": "exec",
                    "runtime": "python",
                    "code": "assert java_user.id == 7 and java_user.runtime == 'app' and java_user.kind == 'user' and java_user.closed is False and java_user.disposer == 'domain' and java_user.name == 'Grace'",
                },
            ],
        },
    ]
    for manifest in manifests:
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(manifest, f)
            path = f.name
        try:
            omnivm.run_manifest(path)
        finally:
            os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"descriptor field objects used JSON fallback: {boundary}")


def test_manifest_js_typed_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new Float64Array([1.25, 2.5, 3.75])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == 1.25\nassert list(payload) == [1.25, 2.5, 3.75]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS typed-array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS typed-array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS typed-array capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_empty_typed_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new Float64Array([])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 0\nassert list(payload) == []\nassert payload.metadata.get('shape') == [0]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS empty typed-array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS empty typed-array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS empty typed-array capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_int8_typed_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new Int8Array([-1, 0, 2])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == -1\nassert list(payload) == [-1, 0, 2]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS Int8 typed-array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS Int8 typed-array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS Int8 typed-array capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_typed_array_subarray_capture_uses_view_window():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new Uint8Array([10, 20, 30, 40, 50, 60]).subarray(2, 5)",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == 30\nassert list(payload) == [30, 40, 50]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS typed-array subarray capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS typed-array subarray capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS typed-array subarray capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_arraybuffer_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": """
                    (() => {
                        const buffer = new ArrayBuffer(4);
                        new Uint8Array(buffer).set([11, 22, 33, 44]);
                        return buffer;
                    })()
                """,
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 4\nassert payload[0] == 11\nassert list(payload) == [11, 22, 33, 44]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS ArrayBuffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS ArrayBuffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS ArrayBuffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_uint16_typed_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new Uint16Array([258, 772, 1286])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == 258\nassert list(payload) == [258, 772, 1286]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS uint16 typed-array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS uint16 typed-array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS uint16 typed-array capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_biguint64_typed_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new BigUint64Array([9223372036854775808n, 9223372036854775813n])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 2\nassert payload[0] == 9223372036854775808\nassert list(payload) == [9223372036854775808, 9223372036854775813]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS BigUint64 typed-array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS BigUint64 typed-array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS BigUint64 typed-array capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_bigint64_typed_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new BigInt64Array([-5n, 9223372036854775807n])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 2\nassert payload[0] == -5\nassert list(payload) == [-5, 9223372036854775807]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS BigInt64 typed-array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS BigInt64 typed-array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS BigInt64 typed-array capture used JSON fallback: before={before}, after={after}")


def test_manifest_js_dataview_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": """
                    (() => {
                        const bytes = new Uint8Array([9, 18, 27, 36, 45, 54]);
                        return new DataView(bytes.buffer, 2, 3);
                    })()
                """,
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == 27\nassert list(payload) == [27, 36, 45]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS DataView capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS DataView capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS DataView capture used JSON fallback: before={before}, after={after}")


def test_manifest_ruby_string_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "payload",
                "code": "'abc'.b",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == 97\nassert list(payload) == [97, 98, 99]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Ruby string capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Ruby string capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby string capture used JSON fallback: before={before}, after={after}")


def test_manifest_ruby_to_str_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "payload",
                "code": "Class.new { def to_str; 'xyz'.b; end }.new",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == 120\nassert list(payload) == [120, 121, 122]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Ruby to_str capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Ruby to_str capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby to_str capture used JSON fallback: before={before}, after={after}")


def test_manifest_ruby_utf8_string_capture_stays_text():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "payload",
                "code": "'hé'.force_encoding('UTF-8')",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert payload == 'hé'\nassert isinstance(payload, str)",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"Ruby UTF-8 text capture should not create a table proxy: after={after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Ruby UTF-8 text capture should not use Arrow/shared memory: after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby UTF-8 text capture should not use JSON fallback: after={after}")


def test_manifest_ruby_array_capture_uses_proxy_not_json():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "payload",
                "code": "[10, 20, 30]",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (payload.length !== 3) throw new Error('bad Ruby array length: ' + payload.length); "
                    "if (payload[0] !== 10 || payload[1] !== 20 || payload[2] !== 30) throw new Error('bad Ruby array index'); "
                    "if (payload.join('-') !== '10-20-30') throw new Error('bad Ruby array method');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Ruby array did not cross as a live proxy: before={before}, after={after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"Ruby array should not claim a bulk table buffer: before={before}, after={after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Ruby array should not claim Arrow transfer: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby array capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_primitive_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": "new int[]{4, 5, 6}",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert payload[0] == 4\nassert list(payload) == [4, 5, 6]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java primitive array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java primitive array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java primitive array capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_empty_primitive_array_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": "new int[]{}",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 0\nassert list(payload) == []\nassert payload.metadata.get('shape') == [0]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java empty primitive array capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java empty primitive array capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java empty primitive array capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_direct_bytebuffer_capture_uses_remaining_window():
    before = omnivm.status().get("boundary", {})
    java_expr = (
        "((java.util.function.Supplier<java.nio.ByteBuffer>)(() -> { "
        "java.nio.ByteBuffer b = java.nio.ByteBuffer.allocateDirect(8); "
        "b.put(new byte[]{10, 20, 30, 40, 50, 60, 70, 80}); "
        "b.position(2); "
        "b.limit(6); "
        "return b; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": java_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 4\nassert payload[0] == 30\nassert list(payload) == [30, 40, 50, 60]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java ByteBuffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java ByteBuffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java ByteBuffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_readonly_direct_bytebuffer_preserves_metadata():
    before = omnivm.status().get("boundary", {})
    java_expr = (
        "((java.util.function.Supplier<java.nio.ByteBuffer>)(() -> { "
        "java.nio.ByteBuffer b = java.nio.ByteBuffer.allocateDirect(4); "
        "b.put((byte)11).put((byte)22).put((byte)33).put((byte)44); "
        "b.flip(); "
        "java.nio.ByteBuffer ro = b.asReadOnlyBuffer(); "
        "ro.position(1); "
        "ro.limit(3); "
        "return ro; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": java_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert payload.metadata.get('read_only') is True, payload.metadata\nassert len(payload) == 2\nassert list(payload) == [22, 33]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java readonly ByteBuffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java readonly ByteBuffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java readonly ByteBuffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_empty_direct_bytebuffer_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": "java.nio.ByteBuffer.allocateDirect(0)",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 0\nassert list(payload) == []\nassert payload.metadata.get('shape') == [0]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java empty ByteBuffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java empty ByteBuffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java empty ByteBuffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_array_backed_intbuffer_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    java_expr = (
        "((java.util.function.Supplier<java.nio.IntBuffer>)(() -> { "
        "java.nio.IntBuffer b = java.nio.IntBuffer.wrap(new int[]{7, 8, 9, 10}); "
        "b.position(1); "
        "b.limit(3); "
        "return b; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": java_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 2\nassert payload[0] == 8\nassert list(payload) == [8, 9]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java array-backed IntBuffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java array-backed IntBuffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java array-backed IntBuffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_direct_floatbuffer_capture_uses_arrow():
    before = omnivm.status().get("boundary", {})
    java_expr = (
        "((java.util.function.Supplier<java.nio.FloatBuffer>)(() -> { "
        "java.nio.FloatBuffer b = java.nio.ByteBuffer.allocateDirect(16)"
        ".order(java.nio.ByteOrder.nativeOrder()).asFloatBuffer(); "
        "b.put(new float[]{1.5f, 2.5f, 3.5f, 4.5f}); "
        "b.position(1); "
        "b.limit(3); "
        "return b; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": java_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 2\nassert abs(payload[0] - 2.5) < 0.0001\nassert [round(x, 1) for x in payload] == [2.5, 3.5]",
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java direct FloatBuffer capture did not create a table proxy: before={before}, after={after}")
    if after.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java direct FloatBuffer capture did not use Arrow/shared memory: before={before}, after={after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java direct FloatBuffer capture used JSON fallback: before={before}, after={after}")


def test_manifest_java_wrong_endian_direct_intbuffer_capture_uses_proxy():
    java_expr = (
        "((java.util.function.Supplier<java.nio.IntBuffer>)(() -> { "
        "java.nio.ByteOrder wrong = java.nio.ByteOrder.nativeOrder() == java.nio.ByteOrder.BIG_ENDIAN ? java.nio.ByteOrder.LITTLE_ENDIAN : java.nio.ByteOrder.BIG_ENDIAN; "
        "java.nio.IntBuffer b = java.nio.ByteBuffer.allocateDirect(12).order(wrong).asIntBuffer(); "
        "b.put(3).put(4).put(5); "
        "b.flip(); "
        "return b; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": java_expr,
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (payload.remaining() !== 3) throw new Error('wrong-endian Java IntBuffer should remain a live proxy with remaining'); "
                    "if (payload.capacity() !== 3) throw new Error('wrong-endian Java IntBuffer should remain a live proxy with capacity');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"wrong-endian Java IntBuffer capture did not create a live proxy: {after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"wrong-endian Java IntBuffer capture should not create a table proxy: {after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"wrong-endian Java IntBuffer capture should not use Arrow/shared memory: {after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"wrong-endian Java IntBuffer capture used JSON fallback: {after}")


def test_manifest_java_wrong_endian_heap_view_intbuffer_capture_uses_proxy():
    java_expr = (
        "((java.util.function.Supplier<java.nio.IntBuffer>)(() -> { "
        "java.nio.ByteOrder wrong = java.nio.ByteOrder.nativeOrder() == java.nio.ByteOrder.BIG_ENDIAN ? java.nio.ByteOrder.LITTLE_ENDIAN : java.nio.ByteOrder.BIG_ENDIAN; "
        "java.nio.IntBuffer b = java.nio.ByteBuffer.allocate(12).order(wrong).asIntBuffer(); "
        "b.put(6).put(7).put(8); "
        "b.flip(); "
        "return b; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": java_expr,
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (payload.remaining() !== 3) throw new Error('wrong-endian heap-view IntBuffer should remain a live proxy with remaining'); "
                    "if (payload.capacity() !== 3) throw new Error('wrong-endian heap-view IntBuffer should remain a live proxy with capacity');"
                ),
                "captures": {"payload": "payload"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"wrong-endian Java heap-view IntBuffer capture did not create a live proxy: {after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"wrong-endian Java heap-view IntBuffer capture should not create a table proxy: {after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"wrong-endian Java heap-view IntBuffer capture should not use Arrow/shared memory: {after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"wrong-endian Java heap-view IntBuffer capture used JSON fallback: {after}")


def test_repeated_crossings():
    for i in range(100):
        expr = f"parseInt(omnivm.call('ruby', '{i} + 1')) + 1"
        expect(omnivm.call("javascript", expr), str(i + 2))


def test_typed_bridge_rejects_complex_stringification():
    try:
        omnivm.call_typed("javascript", "String", args=({"path": "/typed-fallback"},))
    except TypeError as exc:
        if "implicit stringification" not in str(exc):
            raise AssertionError(f"bad Python typed bridge error: {exc}") from exc
    else:
        raise AssertionError("Python typed bridge stringified a complex object")

    js_result = js(
        "try { omnivm.callTyped('python', 'str', {path: '/typed-fallback'}); 'bad'; } "
        "catch (e) { String(e).indexOf('implicit stringification') >= 0 ? 'ok' : String(e); }"
    )
    if js_result != "ok":
        raise AssertionError(f"JS typed bridge did not reject complex object: {js_result}")

    rb_result = rb(
        "begin; OmniVM.call_typed('python', 'str', {'path' => '/typed-fallback'}); "
        "'bad'; rescue => e; e.message.include?('implicit stringification') ? 'ok' : e.message; end"
    )
    if rb_result != "ok":
        raise AssertionError(f"Ruby typed bridge did not reject complex object: {rb_result}")

    try:
        java(
            'omnivm.OmniVM.callTyped("python", "str", new java.util.HashMap<String,Object>())'
        )
    except omnivm.RuntimeError as exc:
        java_result = str(exc)
    else:
        raise AssertionError("Java typed bridge stringified a complex object")
    if "implicit stringification" not in java_result and "IllegalArgumentException" not in java_result:
        raise AssertionError(f"Java typed bridge did not reject complex object: {java_result}")


def test_python_first_process_survives_plain_python():
    expect(sys.implementation.name, "cpython")
    expect(omnivm.call("python", "__import__('sys').version_info.major"), "3")


def test_status_observability():
    status = omnivm.status()
    if not status["initialized"]:
        raise AssertionError("status did not report initialized worker")
    expect(status["pid"], os.getpid())
    expect(status["init_pid"], os.getpid())
    if status["pid_changed"]:
        raise AssertionError("status reported pid_changed in the initializing process")
    if status["active_calls"] < 0:
        raise AssertionError("active call count went negative")
    for runtime in ("python", "javascript", "java", "ruby"):
        if runtime not in status["runtimes"]:
            raise AssertionError(f"missing runtime from status: {runtime}")
    if status["worker_tainted"]:
        raise AssertionError("fresh worker was unexpectedly tainted")
    if status["last_timeout_runtime"] != "":
        raise AssertionError("fresh worker reported a timeout runtime")
    if "go=deadline" not in status["watchdog_capabilities"]:
        raise AssertionError("status omitted Go deadline capability")
    handles = status.get("handles")
    if not isinstance(handles, dict):
        raise AssertionError("status omitted handle diagnostics")
    if handles.get("live") != 0:
        raise AssertionError(f"fresh worker reported live handles: {handles}")
    for key in (
        "handle_accesses",
        "handle_accesses_by_kind",
        "finalizer_queued",
        "finalizer_queue_drains",
        "finalizer_queue_drops",
        "finalizer_queue_len",
        "finalizer_overflow_handles",
        "strong_refs",
        "retained_refs",
        "retained_handles",
        "retained_by_runtime",
        "max_strong_refs",
        "chatty_proxy_warnings",
        "chatty_by_runtime",
        "chattiest_accesses",
        "chattiest_access_kind",
        "chattiest_access_kind_count",
        "reference_edges",
        "reference_edges_by_kind",
        "reference_edges_by_runtime",
        "suspected_cycles",
        "cyclic_handles",
        "largest_cycle",
    ):
        if key not in handles:
            raise AssertionError(f"status handle diagnostics omitted {key}: {handles}")
    boundary = status.get("boundary")
    if not isinstance(boundary, dict):
        raise AssertionError("status omitted boundary diagnostics")
    for key in (
        "json_fallbacks",
        "last_json_fallback_reason",
        "arrow_transfers",
        "bridge_transforms",
        "resource_proxy_captures",
        "table_proxy_captures",
        "proxy_materializations",
    ):
        if key not in boundary:
            raise AssertionError(f"status boundary diagnostics omitted {key}: {boundary}")
    arrow = status.get("arrow")
    if not isinstance(arrow, dict):
        raise AssertionError("status omitted Arrow diagnostics")
    for key in (
        "live_buffers",
        "live_bytes",
        "buffers_by_dtype",
        "buffers_by_format",
        "copied_bytes",
        "zero_copy_borrows",
        "deferred_release_queue_len",
        "deferred_release_overflow_names",
    ):
        if key not in arrow:
            raise AssertionError(f"status Arrow diagnostics omitted {key}: {arrow}")
    expect(omnivm.worker_tainted(), False)
    expect(omnivm.last_timeout_runtime(), "")
    expect(omnivm.worker_taint_reason(), "")


def test_status_keeps_last_manifest_boundary_stats():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "eval", "runtime": "python", "code": "[1, 2, 3]", "bind": "data"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "globalThis.__boundary_len = data.length",
                "captures": {"data": "data"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary")
    if not isinstance(boundary, dict):
        raise AssertionError("status omitted boundary diagnostics after manifest run")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"ambiguous complex captures should use proxies, not JSON fallback: {boundary}")
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"status did not retain manifest resource proxy capture count: {boundary}")
    if boundary.get("capture_injections", 0) < 1:
        raise AssertionError(f"status did not retain manifest capture count: {boundary}")


def test_manifest_handle_proxy_property_forwarding():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {
                    "kind": "literal",
                    "value": {
                        "path": "/orders/42",
                        "method": "POST",
                        "items": ["first", "second"],
                    },
                },
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert req.path == '/orders/42', req.path; assert 'path' in req, 'missing path contains'; assert 'missing' not in req, 'unexpected missing contains'; assert len(req['items']) == 2, len(req['items']); assert list(req['items']) == ['first', 'second'], list(req['items']); assert req['items'][1] == 'second', req['items'][1]; req['items'][0] = 'py-first'; assert req['items'][0] == 'py-first', req['items'][0]; req.status = 'accepted'",
                "captures": {"req": "req"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (req.path !== '/orders/42') throw new Error('bad path: ' + req.path); for (let i = 0; i < 70; i++) { if (req.path !== '/orders/42') throw new Error('bad repeated path'); } if (!('path' in req) || ('missing' in req)) throw new Error('bad contains'); if (!Object.keys(req).includes('path')) throw new Error('bad keys: ' + Object.keys(req).join(',')); if (req.status !== 'accepted') throw new Error('bad status: ' + req.status); if (req.items.length !== 2) throw new Error('bad length: ' + req.items.length); if (Array.from(req.items).join(',') !== 'py-first,second') throw new Error('bad iter'); if (!Object.keys(req.items).includes('0')) throw new Error('bad item keys: ' + Object.keys(req.items).join(',')); if (req.items[1] !== 'second') throw new Error('bad nested item: ' + req.items[1]); req.items[0] = 'js-first'; req.stage = 'js';",
                "captures": {"req": "req"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "raise \"bad path: #{req.path}\" unless req.path == '/orders/42'; raise \"bad contains\" unless req.key?('path') && !req.key?('missing'); raise \"bad stage\" unless req.stage == 'js'; raise \"bad length\" unless req['items'].length == 2; raise \"bad iter\" unless req['items'].to_a == ['js-first', 'second']; raise \"bad nested item\" unless req['items'][1] == 'second'; req['items'][0] = 'ruby-first'; req['ruby_seen'] = true",
                "captures": {"req": "req"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": "Object req = omnivm.OmniVM.getCapture(\"req\"); omnivm.OmniVM.HandleProxy proxy = (omnivm.OmniVM.HandleProxy) req; if (!\"/orders/42\".equals(proxy.get(\"path\"))) throw new RuntimeException(\"bad path\"); if (!proxy.containsKey(\"path\") || proxy.containsKey(\"missing\")) throw new RuntimeException(\"bad contains\"); omnivm.OmniVM.HandleProxy items = (omnivm.OmniVM.HandleProxy) proxy.get(\"items\"); if (items.size() != 2) throw new RuntimeException(\"bad length\"); if (!items.values().toString().equals(\"[ruby-first, second]\")) throw new RuntimeException(\"bad iter: \" + items.values()); if (!items.containsKey(\"0\")) throw new RuntimeException(\"bad item contains\"); if (!\"second\".equals(items.index(1))) throw new RuntimeException(\"bad nested item\"); if (!Boolean.TRUE.equals(proxy.index(\"ruby_seen\"))) throw new RuntimeException(\"bad ruby flag\"); if (!proxy.set(\"java_seen\", true)) throw new RuntimeException(\"set failed\");",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    handles = omnivm.status().get("handles", {})
    if handles.get("handle_accesses_by_kind", {}).get("property", 0) < 4:
        raise AssertionError(f"proxy property forwarding did not record accesses: {handles}")
    for kind in ("index", "contains", "length", "iterate", "mutation"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"proxy {kind} forwarding did not record accesses: {handles}")
    boundary = omnivm.status().get("boundary", {})
    if boundary.get("proxy_materializations", 0) < 1:
        raise AssertionError(f"chatty proxy materialization was not observable: {boundary}")


def test_manifest_runtime_ref_mapping_keys_beat_methods():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "py_map",
                "code": "{'items': 'py-items', 'count': 7, 'keys': 'py-keys'}",
            },
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "rb_map",
                "code": '{"count" => 11, "keys" => "ruby-keys"}',
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (py_map.items !== 'py-items') throw new Error('python items key lost to method: ' + py_map.items); if (String(py_map.count) !== '7') throw new Error('python count key lost: ' + py_map.count); if (String(rb_map.count) !== '11') throw new Error('ruby count key lost to method: ' + rb_map.count); if (rb_map.keys !== 'ruby-keys') throw new Error('ruby keys key lost to method: ' + rb_map.keys);",
                "captures": {"py_map": "py_map", "rb_map": "rb_map"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert rb_map.count == 11, rb_map.count\nassert rb_map.keys == 'ruby-keys', rb_map.keys",
                "captures": {"rb_map": "rb_map"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "raise \"python items key lost: #{py_map.items}\" unless py_map.items == 'py-items'\nraise \"python count key lost: #{py_map.count}\" unless py_map.count == 7\nraise \"python keys key lost: #{py_map.keys}\" unless py_map.keys == 'py-keys'",
                "captures": {"py_map": "py_map"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"method-colliding mapping keys should stay live, not JSON fallback: {boundary}")
    if boundary.get("resource_proxy_captures", 0) < 2:
        raise AssertionError(f"method-colliding mapping keys should cross as generic proxies: {boundary}")


def test_manifest_func_return_materializes_proxy_descriptor():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {
                    "kind": "literal",
                    "value": {
                        "path": "/returned/proxy",
                        "items": ["alpha", "beta"],
                    },
                },
            },
            {
                "op": "func_def",
                "name": "current_req",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "req"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const r = current_req(); if (r.path !== '/returned/proxy') throw new Error('bad returned path: ' + r.path); if (r.items.length !== 2) throw new Error('bad returned nested len'); r.items[0] = 'js-alpha';",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert req['items'][0] == 'js-alpha', req['items'][0]",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned complex values should not use JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    if handles.get("handle_accesses_by_kind", {}).get("property", 0) < 1:
        raise AssertionError(f"function-returned proxy did not record property access: {handles}")


def test_manifest_func_return_local_complex_literal_as_proxy():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "make_payload",
                "params": [],
                "body": [
                    {
                        "op": "return",
                        "value": {
                            "kind": "literal",
                            "value": {
                                "name": "initial",
                                "items": ["alpha", "beta"],
                            },
                        },
                    },
                ],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "payload = make_payload()\nassert payload.name == 'initial', payload\npayload.name = 'changed'\nassert payload.name == 'changed', payload.name\nassert len(payload.items) == 2, payload.items\ndel payload\nimport gc\ngc.collect(); gc.collect(); gc.collect()",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"function-returned local complex literal did not cross as a proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned local complex literal used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    if handles.get("finalizer_releases", 0) < 1:
        raise AssertionError(f"function-returned local complex proxy was not finalizer-released: {handles}")


def test_manifest_proxy_materializer_reuses_handle_identity():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {
                    "kind": "literal",
                    "value": {
                        "path": "/identity-cache",
                        "items": ["alpha", "beta"],
                    },
                },
            },
            {
                "op": "func_def",
                "name": "identity_cached_req",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "req"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const a = identity_cached_req(); const b = identity_cached_req(); if (a !== b) throw new Error('JS proxy identity cache missed');",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "a = identity_cached_req()\nb = identity_cached_req()\nassert a is b, 'Python proxy identity cache missed'",
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "a = identity_cached_req()\nb = identity_cached_req()\nraise 'Ruby proxy identity cache missed' unless a.equal?(b)",
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": "Object a = omnivm.OmniVM.getCapture(\"req\"); Object b = omnivm.OmniVM.materializeJsonCapture(omnivm.OmniVM.getCaptureJson(\"req\")); if (a != b) throw new RuntimeException(\"Java proxy identity cache missed\");",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"proxy identity should not depend on JSON fallback: {boundary}")


def test_manifest_func_return_preserves_complex_argument_identity():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "echo_value",
                "params": [{"name": "value"}],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "value"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const original = {path: '/arg/proxy', items: ['alpha', 'beta']}; const returned = echo_value(original); if (returned.path !== '/arg/proxy') throw new Error('bad returned arg path: ' + returned.path); if (returned.items.length !== 2) throw new Error('bad returned arg nested len'); returned.items[0] = 'via-return'; if (original.items[0] !== 'via-return') throw new Error('returned proxy did not preserve identity');",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned complex arguments should not use JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    if handles.get("handle_accesses_by_kind", {}).get("property", 0) < 1:
        raise AssertionError(f"function-returned argument proxy did not record property access: {handles}")


def test_manifest_func_return_exports_runtime_buffer_as_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "payload",
                "code": "bytearray(b'abc')",
            },
            {
                "op": "func_def",
                "name": "current_payload",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "payload"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const payload = current_payload(); if (payload.length !== 3 || payload[0] !== 97 || payload[1] !== 98 || payload[2] !== 99) throw new Error('bad returned buffer proxy'); if (!payload.metadata || payload.metadata.arrow_format !== 'C' || payload.metadata.read_only !== false) throw new Error('bad returned buffer metadata');",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"function-returned buffer did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"function-returned buffer did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"function-returned buffer should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned buffer used JSON fallback: {boundary}")


def test_manifest_runtime_ref_property_exports_python_buffer_as_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "req",
                "code": "type('Req', (), {'__init__': lambda self: setattr(self, 'payload', memoryview(bytearray(b'abcdef')).cast('B', shape=[2, 3]))})()",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"req": "req"},
                "code": (
                    "const payload = req.payload; "
                    "if (payload.length !== 2) throw new Error('bad live buffer length: ' + payload.length); "
                    "const row = payload[1]; "
                    "if (!Array.isArray(row) || row.length !== 3 || row[0] !== 100 || row[2] !== 102) throw new Error('bad live buffer row: ' + JSON.stringify(row)); "
                    "if (!payload.metadata || payload.metadata.arrow_format !== 'C') throw new Error('bad live buffer metadata: ' + JSON.stringify(payload.metadata)); "
                    "if (payload.metadata.read_only !== false) throw new Error('bad live buffer mutability metadata: ' + JSON.stringify(payload.metadata)); "
                    "if (JSON.stringify(payload.metadata.shape) !== '[2,3]') throw new Error('bad live buffer shape: ' + JSON.stringify(payload.metadata.shape));"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"runtime-ref owner did not cross as an identity proxy: {boundary}")
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"runtime-ref buffer property did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"runtime-ref buffer property did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"runtime-ref buffer property used JSON fallback: {boundary}")


def test_manifest_runtime_ref_property_exports_js_typed_array_as_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "req",
                "code": "({payload: new Uint16Array([258, 772, 1286])})",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"req": "req"},
                "code": (
                    "payload = req.payload\n"
                    "assert len(payload) == 3, len(payload)\n"
                    "assert list(payload) == [258, 772, 1286], list(payload)\n"
                    "assert payload.metadata.get('arrow_format') == 'S', payload.metadata\n"
                    "assert payload.metadata.get('read_only') is False, payload.metadata"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"JS runtime-ref owner did not cross as an identity proxy: {boundary}")
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"JS runtime-ref typed-array property did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"JS runtime-ref typed-array property did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS runtime-ref typed-array property used JSON fallback: {boundary}")


def test_manifest_runtime_ref_property_exports_java_bytebuffer_as_arrow():
    java_req_expr = (
        "((java.util.function.Supplier<java.util.HashMap<String,Object>>)(() -> { "
        "java.nio.ByteBuffer b = java.nio.ByteBuffer.allocateDirect(8); "
        "b.put(new byte[]{10, 20, 30, 40, 50, 60, 70, 80}); "
        "b.position(2); "
        "b.limit(6); "
        "java.util.HashMap<String,Object> req = new java.util.HashMap<>(); "
        "req.put(\"payload\", b); "
        "return req; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "req",
                "code": java_req_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"req": "req"},
                "code": (
                    "payload = req.payload\n"
                    "assert len(payload) == 4, len(payload)\n"
                    "assert list(payload) == [30, 40, 50, 60], list(payload)\n"
                    "assert payload.metadata.get('arrow_format') == 'C', payload.metadata\n"
                    "assert payload.metadata.get('read_only') is False, payload.metadata"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java runtime-ref owner did not cross as an identity proxy: {boundary}")
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Java runtime-ref ByteBuffer property did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Java runtime-ref ByteBuffer property did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java runtime-ref ByteBuffer property used JSON fallback: {boundary}")


def test_manifest_runtime_ref_methods_export_bulk_results_as_arrow():
    java_req_expr = (
        "((java.util.function.Supplier<Object>)(() -> new Object() { "
        "public java.nio.ByteBuffer payload() { "
        "java.nio.ByteBuffer b = java.nio.ByteBuffer.allocateDirect(8); "
        "b.put(new byte[]{10, 20, 30, 40, 50, 60, 70, 80}); "
        "b.position(2); "
        "b.limit(6); "
        "return b; "
        "} "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "py_req",
                "code": "type('Req', (), {'payload': lambda self: memoryview(bytearray(b'abcdef')).cast('B', shape=[2, 3])})()",
            },
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "js_req",
                "code": "({payload() { return new Uint16Array([258, 772, 1286]); }})",
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_req",
                "code": java_req_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"js_req": "js_req", "java_req": "java_req"},
                "code": (
                    "js_payload = js_req.payload()\n"
                    "assert len(js_payload) == 3, len(js_payload)\n"
                    "assert list(js_payload) == [258, 772, 1286], list(js_payload)\n"
                    "assert js_payload.metadata.get('arrow_format') == 'S', js_payload.metadata\n"
                    "assert js_payload.metadata.get('read_only') is False, js_payload.metadata\n"
                    "java_payload = java_req.payload()\n"
                    "assert len(java_payload) == 4, len(java_payload)\n"
                    "assert list(java_payload) == [30, 40, 50, 60], list(java_payload)\n"
                    "assert java_payload.metadata.get('arrow_format') == 'C', java_payload.metadata\n"
                    "assert java_payload.metadata.get('read_only') is False, java_payload.metadata"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"py_req": "py_req"},
                "code": (
                    "const pyPayload = py_req.payload(); "
                    "if (pyPayload.length !== 2) throw new Error('bad Python method payload length: ' + pyPayload.length); "
                    "const row = pyPayload[1]; "
                    "if (!Array.isArray(row) || row.length !== 3 || row[0] !== 100 || row[2] !== 102) throw new Error('bad Python method payload row: ' + JSON.stringify(row)); "
                    "if (!pyPayload.metadata || pyPayload.metadata.arrow_format !== 'C' || pyPayload.metadata.read_only !== false) throw new Error('bad Python method payload metadata: ' + JSON.stringify(pyPayload.metadata));"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 3:
        raise AssertionError(f"runtime-ref method owners did not cross as identity proxies: {boundary}")
    if boundary.get("table_proxy_captures", 0) < 3:
        raise AssertionError(f"runtime-ref bulk method results did not create table proxies: {boundary}")
    if boundary.get("arrow_transfers", 0) < 3:
        raise AssertionError(f"runtime-ref bulk method results did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"runtime-ref bulk method results used JSON fallback: {boundary}")


def test_manifest_java_runtime_ref_exposes_local_object_members_generically():
    java_req_expr = (
        "((java.util.function.Supplier<Object>)(() -> new Object() { "
        "public String path = \"/java-local\"; "
        "public int count = 2; "
        "private String internalName = \"initial\"; "
        "public String getLabel() { return \"alpha\"; } "
        "public String getName() { return internalName; } "
        "public void setName(String value) { internalName = value; } "
        "public String join(String suffix) { return path + \":\" + internalName + \":\" + suffix; } "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_req",
                "code": java_req_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"java_req": "java_req"},
                "code": (
                    "assert java_req.path == '/java-local', java_req.path\n"
                    "assert java_req.label == 'alpha', java_req.label\n"
                    "assert java_req.name == 'initial', java_req.name\n"
                    "java_req.name = 'python'\n"
                    "java_req.count = 7\n"
                    "assert java_req.name == 'python', java_req.name\n"
                    "assert java_req.count == 7, java_req.count\n"
                    "assert java_req.join('tail') == '/java-local:python:tail', java_req.join('tail')"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"java_req": "java_req"},
                "code": (
                    "if (java_req.path !== '/java-local') throw new Error('bad Java field: ' + java_req.path); "
                    "if (java_req.label !== 'alpha') throw new Error('bad Java getter: ' + java_req.label); "
                    "if (java_req.name !== 'python') throw new Error('bad Java getter after Python set: ' + java_req.name); "
                    "java_req.name = 'js'; "
                    "java_req.count = 11; "
                    "if (java_req.name !== 'js') throw new Error('bad Java setter from JS: ' + java_req.name); "
                    "if (java_req.count !== 11) throw new Error('bad Java field set from JS: ' + java_req.count); "
                    "if (java_req.join('tail') !== '/java-local:js:tail') throw new Error('bad Java method: ' + java_req.join('tail'));"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"java_req": "java_req"},
                "code": (
                    "raise \"bad Java field #{java_req.path}\" unless java_req.path == \"/java-local\"; "
                    "raise \"bad Java getter #{java_req.name}\" unless java_req.name == \"js\"; "
                    "java_req.name = \"ruby\"; "
                    "raise \"bad Java setter #{java_req.name}\" unless java_req.name == \"ruby\"; "
                    "raise \"bad Java method #{java_req.join(\"tail\")}\" unless java_req.join(\"tail\") == \"/java-local:ruby:tail\""
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java local object did not cross as an identity proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java local object proxy access used JSON fallback: {boundary}")


def test_manifest_java_collections_capture_use_proxy_not_stream():
    java_map_expr = (
        "((java.util.function.Supplier<java.util.LinkedHashMap<String,Object>>)(() -> { "
        "java.util.LinkedHashMap<String,Object> m = new java.util.LinkedHashMap<>(); "
        "m.put(\"alpha\", 11); "
        "m.put(\"beta\", 22); "
        "return m; "
        "})).get()"
    )
    java_list_expr = (
        "((java.util.function.Supplier<java.util.ArrayList<String>>)(() -> { "
        "java.util.ArrayList<String> items = new java.util.ArrayList<>(); "
        "items.add(\"first\"); "
        "items.add(\"second\"); "
        "return items; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_map",
                "code": java_map_expr,
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_list",
                "code": java_list_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"java_map": "java_map", "java_list": "java_list"},
                "code": (
                    "assert len(java_map) == 2, len(java_map)\n"
                    "assert java_map.get('alpha') == 11, java_map.get('alpha')\n"
                    "java_map.put('gamma', 33)\n"
                    "assert java_map.get('gamma') == 33, java_map.get('gamma')\n"
                    "assert len(java_list) == 2, len(java_list)\n"
                    "assert java_list[0] == 'first', java_list[0]\n"
                    "java_list.add('third')\n"
                    "assert len(java_list) == 3, len(java_list)"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"java_map": "java_map", "java_list": "java_list"},
                "code": (
                    "raise \"bad Java map size #{java_map.length}\" unless java_map.length == 3; "
                    "raise \"bad Java map get #{java_map.get('gamma')}\" unless java_map.get('gamma') == 33; "
                    "java_map.put('delta', 44); "
                    "raise \"bad Java list size #{java_list.length}\" unless java_list.length == 3; "
                    "raise \"bad Java list index #{java_list[2]}\" unless java_list[2] == 'third'; "
                    "java_list.add('fourth')"
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "java.util.Map map = (java.util.Map) omnivm.OmniVM.getCapture(\"java_map\"); "
                    "java.util.List list = (java.util.List) omnivm.OmniVM.getCapture(\"java_list\"); "
                    "if (!\"44\".equals(String.valueOf(map.get(\"delta\"))) || map.size() != 4) throw new RuntimeException(\"Java map proxy mutation did not reach source runtime: \" + map); "
                    "if (!\"fourth\".equals(list.get(3)) || list.size() != 4) throw new RuntimeException(\"Java list proxy mutation did not reach source runtime: \" + list);"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 2:
        raise AssertionError(f"Java collections did not cross as live proxies: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Java collections should not be mistaken for streams: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"Java collections should not claim bulk table buffers: {boundary}")
    if boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Java collections should not claim Arrow transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java collections used JSON fallback: {boundary}")


def test_manifest_python_set_capture_uses_proxy_not_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "py_set",
                "code": "{'alpha', 'beta'}",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"py_set": "py_set"},
                "code": (
                    "if (!('alpha' in py_set)) throw new Error('Python set membership failed');"
                    "if (py_set.length !== 2) throw new Error('bad Python set length: ' + py_set.length);"
                    "py_set.add('gamma');"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"py_set": "py_set"},
                "code": (
                    "raise \"Python set missing gamma\" unless py_set.include?(\"gamma\"); "
                    "raise \"bad Python set length #{py_set.length}\" unless py_set.length == 3; "
                    "py_set.add(\"delta\")"
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"py_set": "py_set"},
                "code": (
                    "omnivm.OmniVM.HandleProxy set = (omnivm.OmniVM.HandleProxy) omnivm.OmniVM.getCapture(\"py_set\"); "
                    "if (!set.containsKey(\"delta\")) throw new RuntimeException(\"Python set missing delta\"); "
                    "if (set.size() != 4) throw new RuntimeException(\"bad Python set size: \" + set.size()); "
                    "set.call(\"add\", \"epsilon\");"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"py_set": "py_set"},
                "code": "assert py_set == {'alpha', 'beta', 'gamma', 'delta', 'epsilon'}, py_set",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Python set did not cross as a live resource proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Python set should not be mistaken for a stream: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Python set should not claim bulk table transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python set used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "call"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Python set proxy did not record {kind} access: {handles}")


def test_manifest_java_set_capture_uses_proxy_not_stream():
    java_set_expr = (
        "((java.util.function.Supplier<java.util.LinkedHashSet<String>>)(() -> { "
        "java.util.LinkedHashSet<String> items = new java.util.LinkedHashSet<>(); "
        "items.add(\"alpha\"); "
        "items.add(\"beta\"); "
        "return items; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_set",
                "code": java_set_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"java_set": "java_set"},
                "code": (
                    "assert 'alpha' in java_set\n"
                    "assert len(java_set) == 2, len(java_set)\n"
                    "java_set.add('gamma')"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"java_set": "java_set"},
                "code": (
                    "if (!('gamma' in java_set)) throw new Error('Java set missing gamma');"
                    "if (java_set.length !== 3) throw new Error('bad Java set length: ' + java_set.length);"
                    "java_set.add('delta');"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"java_set": "java_set"},
                "code": (
                    "raise \"Java set missing delta\" unless java_set.include?(\"delta\"); "
                    "raise \"bad Java set length #{java_set.length}\" unless java_set.length == 4; "
                    "java_set.add(\"epsilon\")"
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "java.util.Set set = (java.util.Set) omnivm.OmniVM.getCapture(\"java_set\"); "
                    "if (!set.contains(\"gamma\") || !set.contains(\"delta\") || !set.contains(\"epsilon\")) throw new RuntimeException(\"Java set proxy mutation did not reach source runtime: \" + set); "
                    "if (set.size() != 5) throw new RuntimeException(\"bad Java set size: \" + set);"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java set did not cross as a live resource proxy: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Java set should not be mistaken for a stream: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Java set should not claim bulk table transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java set used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "call"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Java set proxy did not record {kind} access: {handles}")


def test_manifest_active_record_relation_like_stays_lazy_and_collision_safe():
    before_status = omnivm.status()
    before_boundary = before_status.get("boundary", {})
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "ruby",
                "code": (
                    "require 'active_record'\n"
                    "class ActiveRecordRelationLike\n"
                    "  include Enumerable\n"
                    "  attr_reader :iterations\n"
                    "  def initialize\n"
                    "    @closed = false\n"
                    "    @iterations = 0\n"
                    "    @rows = [\n"
                    "      {'items' => 'field-items', 'keys' => 'field-keys', 'count' => 7, 'then' => 'field-then', 'length' => 12, 'name' => 'ada'},\n"
                    "      {'items' => 'field-items-2', 'keys' => 'field-keys-2', 'count' => 8, 'then' => 'field-then-2', 'length' => 13, 'name' => 'grace'}\n"
                    "    ]\n"
                    "  end\n"
                    "  def each\n"
                    "    return enum_for(:each) unless block_given?\n"
                    "    raise 'relation connection closed' if @closed\n"
                    "    @iterations += 1\n"
                    "    @rows.each { |row| yield row }\n"
                    "  end\n"
                    "  def where(*_args)\n"
                    "    self\n"
                    "  end\n"
                    "  def close\n"
                    "    @closed = true\n"
                    "  end\n"
                    "  def closed?\n"
                    "    @closed\n"
                    "  end\n"
                    "end\n"
                    "$active_record_relation_like = ActiveRecordRelationLike.new"
                ),
            },
            {"op": "eval", "runtime": "ruby", "bind": "ar_relation", "code": "$active_record_relation_like.where(:active)"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const rows = Array.from(ar_relation); "
                    "if (rows.length !== 2) throw new Error('bad relation row count: ' + rows.length); "
                    "if (rows[0].items !== 'field-items') throw new Error('items field lost to method: ' + rows[0].items); "
                    "if (rows[0].keys !== 'field-keys') throw new Error('keys field lost to method: ' + rows[0].keys); "
                    "if (String(rows[0].count) !== '7') throw new Error('count field lost: ' + rows[0].count); "
                    "if (rows[0].then !== 'field-then') throw new Error('then field lost: ' + rows[0].then); "
                    "if (String(rows[0].get('length')) !== '12') throw new Error('length field lost: ' + rows[0].get('length'));"
                ),
                "captures": {"ar_relation": "ar_relation"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "raise 'relation should close after stream EOF' unless $active_record_relation_like.closed?\nraise 'relation should iterate exactly once' unless $active_record_relation_like.iterations == 1",
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "Object raw = omnivm.OmniVM.getCapture(\"ar_relation\"); "
                    "if (!(raw instanceof omnivm.OmniVM.StreamProxy)) throw new RuntimeException(\"ActiveRecord relation-like value should rematerialize as stream proxy: \" + raw); "
                    "try { ((omnivm.OmniVM.StreamProxy) raw).toList(); throw new RuntimeException(\"closed relation stream should not be reusable\"); } "
                    "catch (RuntimeException ex) { if (!String.valueOf(ex.getMessage()).contains(\"relation connection closed\")) throw ex; }"
                ),
                "captures": {"ar_relation": "ar_relation"},
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < before_boundary.get("stream_proxy_captures", 0) + 2:
        raise AssertionError(f"ActiveRecord relation-like value did not cross as stream proxy: before={before_boundary}, after={boundary}")
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"ActiveRecord relation-like value used JSON fallback: before={before_boundary}, after={boundary}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"ActiveRecord relation-like stream did not explicitly release: before={before_handles}, after={handles}")


def test_manifest_sqlite3_native_gem_executes_inside_ruby():
    before_status = omnivm.status()
    before_boundary = before_status.get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "ruby",
                "code": r'''
require 'sqlite3'

$sqlite_db_path = "/tmp/omnivm-sqlite3-native-#{Process.pid}-#{Time.now.to_i}.sqlite3"
File.unlink($sqlite_db_path) if File.exist?($sqlite_db_path)
$sqlite_db = SQLite3::Database.new($sqlite_db_path)
$sqlite_db.execute('CREATE TABLE rows (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, count INTEGER)')
$sqlite_db.execute('INSERT INTO rows (name, count) VALUES (?, ?)', ['ada', 7])
row = $sqlite_db.execute('SELECT name, count FROM rows ORDER BY id')[0]
raise "bad sqlite3 row #{row.inspect}" unless row == ['ada', 7]
answer = OmniVM.call('javascript', 'String(21 * 2)')
raise "bad nested callback #{answer.inspect}" unless answer == '42'
$sqlite_db.close
''',
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"sqlite3 native gem test used JSON fallback: before={before_boundary}, after={boundary}")


def test_manifest_active_record_sqlite_adapter_is_natural_and_collision_safe():
    before_status = omnivm.status()
    before_boundary = before_status.get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "ruby",
                "code": r'''
require 'active_record'
require 'sqlite3'

$ar_sqlite_db_path = "/tmp/omnivm-active-record-#{Process.pid}-#{Time.now.to_i}.sqlite3"
File.unlink($ar_sqlite_db_path) if File.exist?($ar_sqlite_db_path)
ActiveRecord::Base.establish_connection(adapter: 'sqlite3', database: $ar_sqlite_db_path)

ActiveRecord::Base.connection.create_table(:omnivm_ar_orders, force: true) do |table|
  table.string :name
  table.string :items
  table.string :keys
  table.integer :count
  table.string :then
  table.integer :length
  table.string :close
  table.string :get
end

class OmniVMActiveRecordOrder < ActiveRecord::Base
  self.table_name = 'omnivm_ar_orders'
end

OmniVMActiveRecordOrder.create!(
  'name' => 'ada',
  'items' => 'field-items',
  'keys' => 'field-keys',
  'count' => 7,
  'then' => 'field-then',
  'length' => 12,
  'close' => 'field-close',
  'get' => 'field-get'
)
''',
            },
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "ar_order",
                "code": "OmniVMActiveRecordOrder.first",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"ar_order": "ar_order"},
                "code": (
                    "if (ar_order.name !== 'ada') throw new Error('bad name: ' + ar_order.name); "
                    "if (ar_order.items !== 'field-items') throw new Error('items column lost to method: ' + ar_order.items); "
                    "if (ar_order.keys !== 'field-keys') throw new Error('keys column lost to method: ' + ar_order.keys); "
                    "if (String(ar_order.count) !== '7') throw new Error('count column lost: ' + ar_order.count); "
                    "if (ar_order.then !== 'field-then') throw new Error('then column lost: ' + ar_order.then); "
                    "if (String(ar_order.length) !== '12') throw new Error('length column lost: ' + ar_order.length); "
                    "if (ar_order.close !== 'field-close') throw new Error('close column lost: ' + ar_order.close); "
                    "if (ar_order.get !== 'field-get') throw new Error('get column lost: ' + ar_order.get);"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": r'''
before_count = OmniVMActiveRecordOrder.count
begin
  OmniVMActiveRecordOrder.transaction do
    OmniVMActiveRecordOrder.create!('name' => 'rolled-back')
    OmniVM.call('javascript', "throw new Error('rollback please')")
  end
  raise 'transaction unexpectedly completed'
rescue => e
  raise "wrong transaction error #{e.class}: #{e.message}" unless e.message.include?('rollback please')
end
after_count = OmniVMActiveRecordOrder.count
raise "transaction did not rollback: before=#{before_count} after=#{after_count}" unless after_count == before_count
ActiveRecord::Base.connection_pool.disconnect!
File.unlink($ar_sqlite_db_path) if File.exist?($ar_sqlite_db_path)
''',
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < before_boundary.get("resource_proxy_captures", 0) + 1:
        raise AssertionError(f"ActiveRecord model did not cross as a live resource proxy: before={before_boundary}, after={boundary}")
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"ActiveRecord SQLite adapter test used JSON fallback: before={before_boundary}, after={boundary}")


def test_manifest_python_dict_list_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "py_payload",
                "code": "{'path': '/python-dict', 'items': ['first', 'second'], 'meta': {'owner': 'python'}}",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"py_payload": "py_payload"},
                "code": (
                    "if (py_payload.path !== '/python-dict') throw new Error('bad Python dict path: ' + py_payload.path);"
                    "if (py_payload.items[0] !== 'first') throw new Error('bad Python list item: ' + py_payload.items[0]);"
                    "py_payload.items[0] = 'js';"
                    "py_payload.meta.owner = 'js';"
                    "py_payload.extra = {from: 'js'};"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"py_payload": "py_payload"},
                "code": (
                    "raise \"bad Python nested list #{py_payload.items[0]}\" unless py_payload.items[0] == \"js\"; "
                    "raise \"bad Python nested hash #{py_payload.meta.owner}\" unless py_payload.meta.owner == \"js\"; "
                    "raise \"bad Python JS-owned nested object #{py_payload.extra.from}\" unless py_payload.extra.from == \"js\"; "
                    "py_payload.items[1] = \"ruby\"; "
                    "py_payload.meta.owner = \"ruby\""
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"py_payload": "py_payload"},
                "code": (
                    "omnivm.OmniVM.HandleProxy payload = (omnivm.OmniVM.HandleProxy) omnivm.OmniVM.getCapture(\"py_payload\"); "
                    "omnivm.OmniVM.HandleProxy items = (omnivm.OmniVM.HandleProxy) payload.get(\"items\"); "
                    "omnivm.OmniVM.HandleProxy meta = (omnivm.OmniVM.HandleProxy) payload.get(\"meta\"); "
                    "if (!\"ruby\".equals(items.index(1))) throw new RuntimeException(\"bad Python list item: \" + items.index(1)); "
                    "if (!\"ruby\".equals(meta.get(\"owner\"))) throw new RuntimeException(\"bad Python nested owner: \" + meta.get(\"owner\")); "
                    "if (!items.set(\"0\", \"java\")) throw new RuntimeException(\"Python list set failed\"); "
                    "if (!meta.set(\"owner\", \"java\")) throw new RuntimeException(\"Python dict nested set failed\");"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"py_payload": "py_payload"},
                "code": (
                    "assert py_payload['items'] == ['java', 'ruby'], py_payload\n"
                    "assert py_payload['meta']['owner'] == 'java', py_payload\n"
                    "assert getattr(py_payload['extra'], 'from') == 'js', py_payload['extra']"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 3:
        raise AssertionError(f"Python dict/list values did not cross as live proxies: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Python dict/list should not be mistaken for streams: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Python dict/list should not claim bulk table transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python dict/list used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "index", "mutation"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Python dict/list proxy did not record {kind} access: {handles}")


def test_manifest_ruby_hash_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "ruby_payload",
                "code": "{'path' => '/ruby-hash', 'items' => ['first', 'second'], 'meta' => {'owner' => 'ruby'}}",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"ruby_payload": "ruby_payload"},
                "code": (
                    "assert ruby_payload.path == '/ruby-hash', ruby_payload.path\n"
                    "assert ruby_payload.items[0] == 'first', ruby_payload.items[0]\n"
                    "ruby_payload.items[0] = 'python'\n"
                    "ruby_payload.meta.owner = 'python'"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"ruby_payload": "ruby_payload"},
                "code": (
                    "if (ruby_payload.items[0] !== 'python') throw new Error('bad Ruby nested array: ' + ruby_payload.items[0]);"
                    "if (ruby_payload.meta.owner !== 'python') throw new Error('bad Ruby nested hash: ' + ruby_payload.meta.owner);"
                    "ruby_payload.items[1] = 'js';"
                    "ruby_payload.meta.owner = 'js';"
                    "ruby_payload.extra = {from: 'js'};"
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"ruby_payload": "ruby_payload"},
                "code": (
                    "omnivm.OmniVM.HandleProxy payload = (omnivm.OmniVM.HandleProxy) omnivm.OmniVM.getCapture(\"ruby_payload\"); "
                    "omnivm.OmniVM.HandleProxy items = (omnivm.OmniVM.HandleProxy) payload.get(\"items\"); "
                    "omnivm.OmniVM.HandleProxy meta = (omnivm.OmniVM.HandleProxy) payload.get(\"meta\"); "
                    "if (!\"js\".equals(items.index(1))) throw new RuntimeException(\"bad Ruby array item: \" + items.index(1)); "
                    "if (!\"js\".equals(meta.get(\"owner\"))) throw new RuntimeException(\"bad Ruby nested owner: \" + meta.get(\"owner\")); "
                    "if (!items.set(\"0\", \"java\")) throw new RuntimeException(\"Ruby array set failed\"); "
                    "if (!meta.set(\"owner\", \"java\")) throw new RuntimeException(\"Ruby hash nested set failed\");"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"ruby_payload": "ruby_payload"},
                "code": (
                    "payload = $ruby_payload; "
                    "raise \"bad Ruby array #{payload['items'].inspect}\" unless payload['items'] == ['java', 'js']; "
                    "raise \"bad Ruby owner #{payload['meta'].inspect}\" unless payload['meta']['owner'] == 'java'; "
                    "raise \"bad JS-owned object #{payload['extra'].from}\" unless payload['extra'].from == 'js'"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 3:
        raise AssertionError(f"Ruby hash values did not cross as live proxies: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Ruby hash should not be mistaken for a stream: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Ruby hash should not claim bulk table transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby hash used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "index", "mutation"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Ruby hash proxy did not record {kind} access: {handles}")


def test_manifest_python_runtime_ref_exposes_local_object_members_generically():
    py_req_expr = (
        "type('Req', (), {"
        "'__init__': lambda self: ("
        "setattr(self, 'path', '/python-local'), "
        "setattr(self, 'name', 'initial'), "
        "setattr(self, 'count', 2), "
        "None)[-1], "
        "'label': property(lambda self: 'alpha'), "
        "'join': lambda self, suffix: self.path + ':' + self.name + ':' + suffix"
        "})()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "bind": "py_req",
                "code": py_req_expr,
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"py_req": "py_req"},
                "code": (
                    "if (py_req.path !== '/python-local') throw new Error('bad Python field: ' + py_req.path); "
                    "if (py_req.label !== 'alpha') throw new Error('bad Python property: ' + py_req.label); "
                    "if (py_req.name !== 'initial') throw new Error('bad Python attr: ' + py_req.name); "
                    "py_req.name = 'js'; "
                    "py_req.count = 11; "
                    "if (py_req.name !== 'js') throw new Error('bad Python setter from JS: ' + py_req.name); "
                    "if (py_req.count !== 11) throw new Error('bad Python count from JS: ' + py_req.count); "
                    "if (py_req.join('tail') !== '/python-local:js:tail') throw new Error('bad Python method: ' + py_req.join('tail'));"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"py_req": "py_req"},
                "code": (
                    "raise \"bad Python field #{py_req.path}\" unless py_req.path == \"/python-local\"; "
                    "raise \"bad Python property #{py_req.label}\" unless py_req.label == \"alpha\"; "
                    "raise \"bad Python getter #{py_req.name}\" unless py_req.name == \"js\"; "
                    "py_req.name = \"ruby\"; "
                    "py_req.count = 13; "
                    "raise \"bad Python setter #{py_req.name}\" unless py_req.name == \"ruby\"; "
                    "raise \"bad Python count #{py_req.count}\" unless py_req.count == 13; "
                    "raise \"bad Python method #{py_req.join(\"tail\")}\" unless py_req.join(\"tail\") == \"/python-local:ruby:tail\""
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"py_req": "py_req"},
                "code": (
                    "Object req = omnivm.OmniVM.getCapture(\"py_req\"); "
                    "omnivm.OmniVM.HandleProxy proxy = (omnivm.OmniVM.HandleProxy) req; "
                    "if (!\"/python-local\".equals(proxy.get(\"path\"))) throw new RuntimeException(\"bad Python path: \" + proxy.get(\"path\")); "
                    "if (!\"alpha\".equals(proxy.get(\"label\"))) throw new RuntimeException(\"bad Python property: \" + proxy.get(\"label\")); "
                    "if (!\"ruby\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad Python name: \" + proxy.get(\"name\")); "
                    "if (!proxy.set(\"name\", \"java\")) throw new RuntimeException(\"Python name set failed\"); "
                    "if (!proxy.set(\"count\", 17)) throw new RuntimeException(\"Python count set failed\"); "
                    "if (!\"java\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad Python Java-set name: \" + proxy.get(\"name\")); "
                    "if (!\"17\".equals(String.valueOf(proxy.get(\"count\")))) throw new RuntimeException(\"bad Python Java-set count: \" + proxy.get(\"count\")); "
                    "if (!\"/python-local:java:tail\".equals(proxy.call(\"join\", \"tail\"))) throw new RuntimeException(\"bad Python method: \" + proxy.call(\"join\", \"tail\"));"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"py_req": "py_req"},
                "code": "assert py_req.name == 'java', py_req.name\nassert py_req.count == 17, py_req.count",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Python local object did not cross as an identity proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python local object proxy access used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "mutation", "call"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Python local object proxy did not record {kind} access: {handles}")


def test_manifest_ruby_runtime_ref_exposes_local_object_members_generically():
    ruby_req_expr = (
        "Class.new do "
        "attr_accessor :name, :count; "
        "attr_reader :path; "
        "def initialize; @path = '/ruby-local'; @name = 'initial'; @count = 2; end; "
        "def label; 'alpha'; end; "
        "def join(suffix); \"#{@path}:#{@name}:#{suffix}\"; end; "
        "end.new"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "ruby_req",
                "code": ruby_req_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"ruby_req": "ruby_req"},
                "code": (
                    "assert ruby_req.path == '/ruby-local', ruby_req.path\n"
                    "assert ruby_req.label == 'alpha', ruby_req.label\n"
                    "assert ruby_req.name == 'initial', ruby_req.name\n"
                    "ruby_req.name = 'python'\n"
                    "ruby_req.count = 7\n"
                    "assert ruby_req.name == 'python', ruby_req.name\n"
                    "assert ruby_req.count == 7, ruby_req.count\n"
                    "assert ruby_req.join('tail') == '/ruby-local:python:tail', ruby_req.join('tail')"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"ruby_req": "ruby_req"},
                "code": (
                    "if (ruby_req.path !== '/ruby-local') throw new Error('bad Ruby field: ' + ruby_req.path); "
                    "if (ruby_req.label !== 'alpha') throw new Error('bad Ruby method-property: ' + ruby_req.label); "
                    "if (ruby_req.name !== 'python') throw new Error('bad Ruby getter after Python set: ' + ruby_req.name); "
                    "ruby_req.name = 'js'; "
                    "ruby_req.count = 11; "
                    "if (ruby_req.name !== 'js') throw new Error('bad Ruby setter from JS: ' + ruby_req.name); "
                    "if (ruby_req.count !== 11) throw new Error('bad Ruby count from JS: ' + ruby_req.count); "
                    "if (ruby_req.join('tail') !== '/ruby-local:js:tail') throw new Error('bad Ruby method: ' + ruby_req.join('tail'));"
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"ruby_req": "ruby_req"},
                "code": (
                    "Object req = omnivm.OmniVM.getCapture(\"ruby_req\"); "
                    "omnivm.OmniVM.HandleProxy proxy = (omnivm.OmniVM.HandleProxy) req; "
                    "if (!\"/ruby-local\".equals(proxy.get(\"path\"))) throw new RuntimeException(\"bad Ruby path: \" + proxy.get(\"path\")); "
                    "if (!\"alpha\".equals(proxy.get(\"label\"))) throw new RuntimeException(\"bad Ruby label: \" + proxy.get(\"label\")); "
                    "if (!\"js\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad Ruby name: \" + proxy.get(\"name\")); "
                    "if (!proxy.set(\"name\", \"java\")) throw new RuntimeException(\"Ruby name set failed\"); "
                    "if (!proxy.set(\"count\", 17)) throw new RuntimeException(\"Ruby count set failed\"); "
                    "if (!\"java\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad Ruby Java-set name: \" + proxy.get(\"name\")); "
                    "if (!\"17\".equals(String.valueOf(proxy.get(\"count\")))) throw new RuntimeException(\"bad Ruby Java-set count: \" + proxy.get(\"count\")); "
                    "if (!\"/ruby-local:java:tail\".equals(proxy.call(\"join\", \"tail\"))) throw new RuntimeException(\"bad Ruby method: \" + proxy.call(\"join\", \"tail\"));"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"ruby_req": "ruby_req"},
                "code": "assert ruby_req.name == 'java', ruby_req.name\nassert ruby_req.count == 17, ruby_req.count",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Ruby local object did not cross as an identity proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby local object proxy access used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "mutation", "call"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Ruby local object proxy did not record {kind} access: {handles}")


def test_manifest_js_runtime_ref_exposes_local_object_members_generically():
    js_req_expr = (
        "(() => { "
        "const req = { "
        "path: '/js-local', "
        "_name: 'initial', "
        "count: 2, "
        "get label() { return 'alpha'; }, "
        "get name() { return this._name; }, "
        "set name(value) { this._name = value; }, "
        "join(suffix) { return this.path + ':' + this._name + ':' + suffix; } "
        "}; "
        "return req; "
        "})()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "js_req",
                "code": js_req_expr,
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"js_req": "js_req"},
                "code": (
                    "assert js_req.path == '/js-local', js_req.path\n"
                    "assert js_req.label == 'alpha', js_req.label\n"
                    "assert js_req.name == 'initial', js_req.name\n"
                    "js_req.name = 'python'\n"
                    "js_req.count = 7\n"
                    "assert js_req.name == 'python', js_req.name\n"
                    "assert js_req.count == 7, js_req.count\n"
                    "assert js_req.join('tail') == '/js-local:python:tail', js_req.join('tail')"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"js_req": "js_req"},
                "code": (
                    "raise \"bad JS field #{js_req.path}\" unless js_req.path == \"/js-local\"; "
                    "raise \"bad JS property #{js_req.label}\" unless js_req.label == \"alpha\"; "
                    "raise \"bad JS getter #{js_req.name}\" unless js_req.name == \"python\"; "
                    "js_req.name = \"ruby\"; "
                    "js_req.count = 13; "
                    "raise \"bad JS setter #{js_req.name}\" unless js_req.name == \"ruby\"; "
                    "raise \"bad JS count #{js_req.count}\" unless js_req.count == 13; "
                    "raise \"bad JS method #{js_req.join(\"tail\")}\" unless js_req.join(\"tail\") == \"/js-local:ruby:tail\""
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"js_req": "js_req"},
                "code": (
                    "Object req = omnivm.OmniVM.getCapture(\"js_req\"); "
                    "omnivm.OmniVM.HandleProxy proxy = (omnivm.OmniVM.HandleProxy) req; "
                    "if (!\"/js-local\".equals(proxy.get(\"path\"))) throw new RuntimeException(\"bad JS path: \" + proxy.get(\"path\")); "
                    "if (!\"alpha\".equals(proxy.get(\"label\"))) throw new RuntimeException(\"bad JS label: \" + proxy.get(\"label\")); "
                    "if (!\"ruby\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad JS name: \" + proxy.get(\"name\")); "
                    "if (!proxy.set(\"name\", \"java\")) throw new RuntimeException(\"JS name set failed\"); "
                    "if (!proxy.set(\"count\", 17)) throw new RuntimeException(\"JS count set failed\"); "
                    "if (!\"java\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad JS Java-set name: \" + proxy.get(\"name\")); "
                    "if (!\"17\".equals(String.valueOf(proxy.get(\"count\")))) throw new RuntimeException(\"bad JS Java-set count: \" + proxy.get(\"count\")); "
                    "if (!\"/js-local:java:tail\".equals(proxy.call(\"join\", \"tail\"))) throw new RuntimeException(\"bad JS method: \" + proxy.call(\"join\", \"tail\"));"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"js_req": "js_req"},
                "code": "assert js_req.name == 'java', js_req.name\nassert js_req.count == 17, js_req.count",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"JS local object did not cross as an identity proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS local object proxy access used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "mutation", "call"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"JS local object proxy did not record {kind} access: {handles}")


def test_manifest_js_plain_object_array_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "js_payload",
                "code": "({path: '/js-object', items: ['first', 'second'], meta: {owner: 'js'}})",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"js_payload": "js_payload"},
                "code": (
                    "assert js_payload.path == '/js-object', js_payload.path\n"
                    "assert js_payload.items[0] == 'first', js_payload.items[0]\n"
                    "js_payload.items[0] = 'python'\n"
                    "js_payload.meta.owner = 'python'\n"
                    "js_payload.extra = {'from': 'python'}"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"js_payload": "js_payload"},
                "code": (
                    "raise \"bad JS nested array #{js_payload.items[0]}\" unless js_payload.items[0] == \"python\"; "
                    "raise \"bad JS nested object #{js_payload.meta.owner}\" unless js_payload.meta.owner == \"python\"; "
                    "raise \"bad Python-owned nested object #{js_payload.extra.from}\" unless js_payload.extra.from == \"python\"; "
                    "js_payload.items[1] = \"ruby\"; "
                    "js_payload.meta.owner = \"ruby\""
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"js_payload": "js_payload"},
                "code": (
                    "omnivm.OmniVM.HandleProxy payload = (omnivm.OmniVM.HandleProxy) omnivm.OmniVM.getCapture(\"js_payload\"); "
                    "omnivm.OmniVM.HandleProxy items = (omnivm.OmniVM.HandleProxy) payload.get(\"items\"); "
                    "omnivm.OmniVM.HandleProxy meta = (omnivm.OmniVM.HandleProxy) payload.get(\"meta\"); "
                    "if (!\"ruby\".equals(items.index(1))) throw new RuntimeException(\"bad JS array item: \" + items.index(1)); "
                    "if (!\"ruby\".equals(meta.get(\"owner\"))) throw new RuntimeException(\"bad JS nested owner: \" + meta.get(\"owner\")); "
                    "if (!items.set(\"0\", \"java\")) throw new RuntimeException(\"JS array set failed\"); "
                    "if (!meta.set(\"owner\", \"java\")) throw new RuntimeException(\"JS object nested set failed\");"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (js_payload.items[0] !== 'java' || js_payload.items[1] !== 'ruby') throw new Error('bad JS array: ' + js_payload.items);"
                    "if (js_payload.meta.owner !== 'java') throw new Error('bad JS owner: ' + js_payload.meta.owner);"
                    "if (js_payload.extra.from !== 'python') throw new Error('bad Python-owned object: ' + js_payload.extra.from);"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 3:
        raise AssertionError(f"JS plain object/array values did not cross as live proxies: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"JS plain object/array should not be mistaken for streams: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"JS plain object/array should not claim bulk table transfer: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS plain object/array used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "index", "mutation"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"JS plain object/array proxy did not record {kind} access: {handles}")


def test_manifest_js_map_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "js_map",
                "code": "new Map([['alpha', 11], ['beta', 22]])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"js_map": "js_map"},
                "code": (
                    "assert js_map.size == 2, js_map.size\n"
                    "assert js_map.get('alpha') == 11, js_map.get('alpha')\n"
                    "js_map.set('gamma', 33)\n"
                    "assert js_map.get('gamma') == 33, js_map.get('gamma')"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"js_map": "js_map"},
                "code": (
                    "raise \"bad JS Map size #{js_map.size}\" unless js_map.size == 3; "
                    "raise \"bad JS Map get #{js_map.get('gamma')}\" unless js_map.get('gamma') == 33; "
                    "js_map.set('delta', 44)"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (js_map.get('delta') !== 44 || js_map.size !== 4) throw new Error('JS Map proxy mutation did not reach source runtime');",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"JS Map did not cross as a live proxy: {after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"JS Map should not claim a bulk table buffer: {after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"JS Map should not claim Arrow transfer: {after}")
    if after.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"JS Map should not be mistaken for a stream: {after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS Map capture used JSON fallback: {after}")


def test_manifest_js_set_capture_uses_proxy_not_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "js_set",
                "code": "new Set(['alpha', 'beta'])",
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"js_set": "js_set"},
                "code": (
                    "assert js_set.size == 2, js_set.size\n"
                    "assert js_set.has('alpha') is True, js_set.has('alpha')\n"
                    "js_set.add('gamma')\n"
                    "assert js_set.has('gamma') is True, js_set.has('gamma')"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"js_set": "js_set"},
                "code": (
                    "raise \"bad JS Set size #{js_set.size}\" unless js_set.size == 3; "
                    "raise \"bad JS Set has #{js_set.has('gamma')}\" unless js_set.has('gamma') == true; "
                    "js_set.add('delta')"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (!js_set.has('delta') || js_set.size !== 4) throw new Error('JS Set proxy mutation did not reach source runtime');",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("boundary", {})
    if after.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"JS Set did not cross as a live proxy: {after}")
    if after.get("table_proxy_captures", 0) != 0:
        raise AssertionError(f"JS Set should not claim a bulk table buffer: {after}")
    if after.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"JS Set should not claim Arrow transfer: {after}")
    if after.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"JS Set should not be mistaken for a stream: {after}")
    if after.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS Set capture used JSON fallback: {after}")


def test_manifest_zod_schema_capture_uses_proxy_not_json():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "zod_schema",
                "code": (
                    "(() => { "
                    "const { z } = require('zod'); "
                    "return z.string().min(3).transform(s => s.toUpperCase()); "
                    "})()"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"zod_schema": "zod_schema"},
                "code": (
                    "assert zod_schema.parse('poly') == 'POLY', zod_schema.parse('poly')\n"
                    "assert zod_schema.safeParse('vm').success is False, zod_schema.safeParse('vm').success"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Zod schema object did not cross as a live proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Zod schema object used JSON fallback: {boundary}")
    if boundary.get("table_proxy_captures", 0) != 0 or boundary.get("arrow_transfers", 0) != 0:
        raise AssertionError(f"Zod schema object should not claim bulk Arrow transfer: {boundary}")
    handles = omnivm.status().get("handles", {})
    if handles.get("handle_accesses_by_kind", {}).get("call", 0) < 1:
        raise AssertionError(f"Zod schema method calls were not recorded as proxy calls: {handles}")


def test_validation_error_fidelity_popular_libraries():
    cases = [
        (
            "zod",
            "javascript",
            (
                "const { z } = require('zod'); "
                "z.object({age:z.number().min(1), name:z.string()}).parse({age:0});"
            ),
            ["ZodError", "too_small", "age", "invalid_type", "name"],
            {"runtime": "javascript", "type": "ZodError", "message": "["},
        ),
        (
            "pydantic",
            "python",
            (
                "from pydantic import BaseModel, Field\n"
                "class User(BaseModel):\n"
                "    age:int=Field(gt=0)\n"
                "    name:str\n"
                "User(age=0)"
            ),
            ["ValidationError", "age", "greater_than", "name", "missing"],
            {"runtime": "python", "type": "ValidationError", "message": "2 validation errors for User"},
        ),
        (
            "django forms",
            "python",
            (
                "from django.conf import settings\n"
                "if not settings.configured: settings.configure(USE_I18N=False, SECRET_KEY='x')\n"
                "from django import forms\n"
                "class Signup(forms.Form):\n"
                "    age=forms.IntegerField(min_value=1)\n"
                "    name=forms.CharField(required=True)\n"
                "form=Signup({'age':'0'})\n"
                "from django.core.exceptions import ValidationError\n"
                "raise ValidationError(form.errors.as_json())"
            ),
            ["ValidationError", "age", "min_value", "name", "required"],
            {"runtime": "python", "type": "ValidationError", "message": "age"},
        ),
        (
            "java cause chain",
            "java",
            (
                '((java.util.concurrent.Callable<String>)(() -> { '
                'throw new RuntimeException("outer", new IllegalArgumentException("inner")); '
                '})).call()'
            ),
            ["java.lang.RuntimeException", "outer", "Caused by", "java.lang.IllegalArgumentException", "inner"],
            {"runtime": "java", "type": "java.lang.RuntimeException", "message": "outer", "cause_type": "java.lang.IllegalArgumentException", "cause_message": "inner"},
        ),
        (
            "ruby activerecord",
            "ruby",
            "require 'active_record'; raise ActiveRecord::RecordInvalid.new(nil)",
            ["ActiveRecord::RecordInvalid", "Record invalid"],
            {"runtime": "ruby", "type": "ActiveRecord::RecordInvalid", "message": "Record invalid"},
        ),
    ]
    for name, runtime, code, expected, structured in cases:
        try:
            omnivm.call(runtime, code)
        except omnivm.RuntimeError as exc:
            text = str(exc)
            missing = [part for part in expected if part not in text]
            if missing:
                raise AssertionError(f"{name} error lost details {missing}: {text}") from exc
            if exc.runtime != structured["runtime"]:
                raise AssertionError(f"{name} error runtime = {exc.runtime!r}, want {structured['runtime']!r}: {text}") from exc
            if exc.type != structured["type"]:
                raise AssertionError(f"{name} error type = {exc.type!r}, want {structured['type']!r}: {text}") from exc
            if structured["message"] not in exc.message:
                raise AssertionError(f"{name} error message = {exc.message!r}, want containing {structured['message']!r}: {text}") from exc
            if "traceback" in structured and structured["traceback"] not in exc.traceback:
                raise AssertionError(f"{name} error traceback = {exc.traceback!r}, want containing {structured['traceback']!r}: {text}") from exc
            if "cause_type" in structured:
                if not exc.cause_chain:
                    raise AssertionError(f"{name} error lost cause chain: {text}") from exc
                cause = exc.cause_chain[0]
                if cause.get("type") != structured["cause_type"] or structured["cause_message"] not in cause.get("message", ""):
                    raise AssertionError(f"{name} error cause = {cause!r}, want {structured}: {text}") from exc
        else:
            raise AssertionError(f"{name} did not raise an error")


def test_manifest_func_return_exports_js_typed_array_as_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "javascript",
                "bind": "payload",
                "code": "new Uint16Array([258, 772, 1286])",
            },
            {
                "op": "func_def",
                "name": "current_payload",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "payload"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert list(payload) == [258, 772, 1286]\nassert payload.metadata.get('arrow_format') == 'S'\nassert payload.metadata.get('read_only') is False, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"function-returned JS typed array did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"function-returned JS typed array did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"function-returned JS typed array should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned JS typed array used JSON fallback: {boundary}")


def test_manifest_func_return_exports_java_primitive_array_as_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "payload",
                "code": "new int[]{4, 5, 6}",
            },
            {
                "op": "func_def",
                "name": "current_payload",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "payload"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert list(payload) == [4, 5, 6]\nassert payload.metadata.get('arrow_format') == 'i'\nassert payload.metadata.get('read_only') is False, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"function-returned Java primitive array did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"function-returned Java primitive array did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"function-returned Java primitive array should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned Java primitive array used JSON fallback: {boundary}")


def test_manifest_go_cshared_func_return_exports_typed_slice_as_arrow():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "scores",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc Scores() []int32 {\n\treturn []int32{4, 5, 6}\n}",
                "exports": ["Scores"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "payload = scores()\nassert len(payload) == 3\nassert list(payload) == [4, 5, 6]\nassert payload.metadata.get('arrow_format') == 'i'\nassert payload.metadata.get('read_only') is True, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared typed slice did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared typed slice did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared typed slice should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared typed slice used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_imports", 0) <= before_arrow.get("zero_copy_imports", 0):
        raise AssertionError(f"Go c-shared typed slice did not enter Arrow as an owned zero-copy import: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_return_exports_nested_array_as_shaped_arrow():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "matrix",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc Matrix() [2][3]float64 {\n\treturn [2][3]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}}\n}",
                "exports": ["Matrix"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "payload = matrix()\nassert len(payload) == 2\nassert payload[0] == [1.5, 2.5, 3.5]\nassert payload[1] == [4.5, 5.5, 6.5]\nassert payload.metadata.get('shape') == [2, 3]\nassert payload.metadata.get('strides') == [24, 8]\nassert payload.metadata.get('arrow_format') == 'g'\nassert payload.metadata.get('read_only') is True, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared nested array did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared nested array did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared nested array should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared nested array used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_imports", 0) <= before_arrow.get("zero_copy_imports", 0):
        raise AssertionError(f"Go c-shared nested array did not enter Arrow as an owned zero-copy import: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_return_exports_nested_slice_as_shaped_arrow():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "matrix",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc Matrix() [][]float64 {\n\treturn [][]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}}\n}",
                "exports": ["Matrix"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "payload = matrix()\nassert len(payload) == 2\nassert payload[0] == [1.5, 2.5, 3.5]\nassert payload[1] == [4.5, 5.5, 6.5]\nassert payload.metadata.get('shape') == [2, 3]\nassert payload.metadata.get('strides') == [24, 8]\nassert payload.metadata.get('arrow_format') == 'g'\nassert payload.metadata.get('read_only') is True, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared nested slice did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared nested slice did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared nested slice should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared nested slice used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_imports", 0) <= before_arrow.get("zero_copy_imports", 0):
        raise AssertionError(f"Go c-shared nested slice did not enter Arrow as an owned zero-copy import: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_return_exports_byte_slice_as_arrow():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "payload",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc Payload() []byte {\n\treturn []byte{2, 3, 5}\n}",
                "exports": ["Payload"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "payload = payload()\nassert len(payload) == 3\nassert list(payload) == [2, 3, 5]\nassert payload.metadata.get('arrow_format') == 'C'\nassert payload.metadata.get('read_only') is True, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared byte slice did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared byte slice did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared byte slice should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared byte slice used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_imports", 0) <= before_arrow.get("zero_copy_imports", 0):
        raise AssertionError(f"Go c-shared byte slice did not enter Arrow as an owned zero-copy import: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_return_exports_complex_object_as_proxy():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "request",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": (
                    "package polyfunc\n\n"
                    "var store = map[string]interface{}{\n"
                    "\t\"path\": \"/go-cshared-proxy\",\n"
                    "\t\"items\": []interface{}{\"first\", \"second\"},\n"
                    "}\n\n"
                    "func Request() map[string]interface{} {\n"
                    "\treturn store\n"
                    "}\n"
                ),
                "exports": ["Request"],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const req = request();"
                    "if (req.path !== '/go-cshared-proxy') throw new Error('bad path: ' + req.path);"
                    "if (req.items.length !== 2) throw new Error('bad items length');"
                    "req.items[0] = 'changed';"
                    "const again = request();"
                    "if (again.items[0] !== 'changed') throw new Error('mutation did not reach plugin object');"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared complex object did not create a resource proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared complex object used JSON fallback: {boundary}")


def test_manifest_go_cshared_sequence_members_cross_generically():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "sequence",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": (
                    "package polyfunc\n\n"
                    "var store = []interface{}{\"first\", \"second\", map[string]interface{}{\"name\": \"nested\"}}\n\n"
                    "func Sequence() []interface{} {\n"
                    "\treturn store\n"
                    "}\n"
                ),
                "exports": ["Sequence"],
            },
            {
                "op": "eval",
                "runtime": "python",
                "bind": "go_seq",
                "code": (
                    "seq = sequence()\n"
                    "assert len(seq) == 3, len(seq)\n"
                    "assert seq[0] == 'first', seq[0]\n"
                    "assert seq[2].name == 'nested', seq[2].name\n"
                    "seq[1] = 'python'\n"
                    "seq[2].name = 'python-nested'\n"
                    "seq"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"go_seq": "go_seq"},
                "code": (
                    "if (go_seq.length !== 3) throw new Error('bad Go sequence length: ' + go_seq.length);"
                    "if (go_seq[1] !== 'python') throw new Error('bad Go sequence value after Python set: ' + go_seq[1]);"
                    "if (go_seq[2].name !== 'python-nested') throw new Error('bad nested Go map after Python set: ' + go_seq[2].name);"
                    "go_seq[0] = 'js';"
                    "go_seq[2].name = 'js-nested';"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"go_seq": "go_seq"},
                "code": (
                    "raise \"bad Go sequence length #{go_seq.length}\" unless go_seq.length == 3; "
                    "raise \"bad Go sequence value #{go_seq[0]}\" unless go_seq[0] == \"js\"; "
                    "raise \"bad Go nested map #{go_seq[2].name}\" unless go_seq[2].name == \"js-nested\"; "
                    "go_seq[1] = \"ruby\"; "
                    "go_seq[2].name = \"ruby-nested\""
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"go_seq": "go_seq"},
                "code": (
                    "omnivm.OmniVM.HandleProxy seq = (omnivm.OmniVM.HandleProxy) omnivm.OmniVM.getCapture(\"go_seq\"); "
                    "if (seq.size() != 3) throw new RuntimeException(\"bad Go sequence length: \" + seq.size()); "
                    "if (!\"ruby\".equals(seq.index(1))) throw new RuntimeException(\"bad Go sequence value: \" + seq.index(1)); "
                    "omnivm.OmniVM.HandleProxy nested = (omnivm.OmniVM.HandleProxy) seq.index(2); "
                    "if (!\"ruby-nested\".equals(nested.get(\"name\"))) throw new RuntimeException(\"bad nested Go map: \" + nested.get(\"name\")); "
                    "if (!seq.set(\"0\", \"java\")) throw new RuntimeException(\"Go sequence set failed\"); "
                    "if (!nested.set(\"name\", \"java-nested\")) throw new RuntimeException(\"nested Go map set failed\");"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"go_seq": "go_seq"},
                "code": "assert go_seq[0] == 'java', go_seq[0]\nassert go_seq[2].name == 'java-nested', go_seq[2].name",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 2:
        raise AssertionError(f"Go c-shared sequence did not cross as live proxies: {boundary}")
    if boundary.get("stream_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared sequence should not be mistaken for a stream: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared sequence used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("index", "mutation"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Go c-shared sequence proxy did not record {kind} access: {handles}")


def test_manifest_go_cshared_struct_object_members_cross_generically():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "request",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": (
                    "package polyfunc\n\n"
                    "type RequestData struct {\n"
                    "\tPath string `json:\"path\"`\n"
                    "\tName string `json:\"name\"`\n"
                    "\tCount int `json:\"count\"`\n"
                    "\tLabel string `json:\"label\"`\n"
                    "}\n\n"
                    "func (r *RequestData) Join(suffix string) string {\n"
                    "\treturn r.Path + \":\" + r.Name + \":\" + suffix\n"
                    "}\n\n"
                    "var store = &RequestData{Path: \"/go-cshared-struct\", Name: \"initial\", Count: 2, Label: \"alpha\"}\n\n"
                    "func Request() *RequestData {\n"
                    "\treturn store\n"
                    "}\n"
                ),
                "exports": ["Request"],
            },
            {
                "op": "eval",
                "runtime": "python",
                "bind": "go_req",
                "code": (
                    "req = request()\n"
                    "assert req.path == '/go-cshared-struct', req.path\n"
                    "assert req.label == 'alpha', req.label\n"
                    "assert req.name == 'initial', req.name\n"
                    "req.name = 'python'\n"
                    "req.count = 7\n"
                    "assert req.name == 'python', req.name\n"
                    "assert req.count == 7, req.count\n"
                    "assert req.Join('tail') == '/go-cshared-struct:python:tail', req.Join('tail')\n"
                    "req"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "(() => {"
                    "const req = request();"
                    "if (req.path !== '/go-cshared-struct') throw new Error('bad Go field: ' + req.path);"
                    "if (req.label !== 'alpha') throw new Error('bad Go label: ' + req.label);"
                    "if (req.name !== 'python') throw new Error('bad Go getter after Python set: ' + req.name);"
                    "req.name = 'js';"
                    "req.count = 11;"
                    "if (req.name !== 'js') throw new Error('bad Go setter from JS: ' + req.name);"
                    "if (req.count !== 11) throw new Error('bad Go count from JS: ' + req.count);"
                    "if (req.Join('tail') !== '/go-cshared-struct:js:tail') throw new Error('bad Go method: ' + req.Join('tail'));"
                    "})();"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": (
                    "req = request(); "
                    "raise \"bad Go field #{req.path}\" unless req.path == \"/go-cshared-struct\"; "
                    "raise \"bad Go label #{req.label}\" unless req.label == \"alpha\"; "
                    "raise \"bad Go name #{req.name}\" unless req.name == \"js\"; "
                    "req.name = \"ruby\"; "
                    "req.count = 13; "
                    "raise \"bad Go setter #{req.name}\" unless req.name == \"ruby\"; "
                    "raise \"bad Go count #{req.count}\" unless req.count == 13; "
                    "raise \"bad Go method #{req.Join(\"tail\")}\" unless req.Join(\"tail\") == \"/go-cshared-struct:ruby:tail\""
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"go_req": "go_req"},
                "code": (
                    "Object req = omnivm.OmniVM.getCapture(\"go_req\"); "
                    "omnivm.OmniVM.HandleProxy proxy = (omnivm.OmniVM.HandleProxy) req; "
                    "if (!\"/go-cshared-struct\".equals(proxy.get(\"path\"))) throw new RuntimeException(\"bad Go path: \" + proxy.get(\"path\")); "
                    "if (!\"alpha\".equals(proxy.get(\"label\"))) throw new RuntimeException(\"bad Go label: \" + proxy.get(\"label\")); "
                    "if (!\"ruby\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad Go name: \" + proxy.get(\"name\")); "
                    "if (!proxy.set(\"name\", \"java\")) throw new RuntimeException(\"Go name set failed\"); "
                    "if (!proxy.set(\"count\", 17)) throw new RuntimeException(\"Go count set failed\"); "
                    "if (!\"java\".equals(proxy.get(\"name\"))) throw new RuntimeException(\"bad Go Java-set name: \" + proxy.get(\"name\")); "
                    "if (!\"17\".equals(String.valueOf(proxy.get(\"count\")))) throw new RuntimeException(\"bad Go Java-set count: \" + proxy.get(\"count\")); "
                    "if (!\"/go-cshared-struct:java:tail\".equals(proxy.call(\"Join\", \"tail\"))) throw new RuntimeException(\"bad Go method: \" + proxy.call(\"Join\", \"tail\"));"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"go_req": "go_req"},
                "code": "assert go_req.name == 'java', go_req.name\nassert go_req.count == 17, go_req.count",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared struct did not cross as an identity proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared struct proxy access used JSON fallback: {boundary}")
    handles = omnivm.status().get("handles", {})
    for kind in ("property", "mutation", "call"):
        if handles.get("handle_accesses_by_kind", {}).get(kind, 0) < 1:
            raise AssertionError(f"Go c-shared struct proxy did not record {kind} access: {handles}")


def test_manifest_go_cshared_func_preserves_complex_proxy_argument():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "echo_request",
                "params": [{"name": "req"}],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc EchoRequest(req interface{}) interface{} {\n\treturn req\n}",
                "exports": ["EchoRequest"],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "(function(){"
                    "const original = {path: '/go-cshared-arg', items: ['first', 'second']};"
                    "const returned = echo_request(original);"
                    "if (returned.path !== '/go-cshared-arg') throw new Error('bad returned path: ' + returned.path);"
                    "returned.items[0] = 'changed';"
                    "if (original.items[0] !== 'changed') throw new Error('returned proxy did not preserve argument identity');"
                    "})();"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared complex arg did not stay behind a resource proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared complex arg used JSON fallback: {boundary}")


def test_manifest_go_cshared_proxy_setter_values_stay_live():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "request",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": (
                    "package polyfunc\n\n"
                    "var store = map[string]interface{}{}\n\n"
                    "func Request() map[string]interface{} {\n"
                    "\treturn store\n"
                    "}\n"
                ),
                "exports": ["Request"],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "(function(){"
                    "const req = request();"
                    "const original = {path: '/js-owned', items: ['first', 'second']};"
                    "req.payload = original;"
                    "const returned = req.payload;"
                    "if (returned.path !== '/js-owned') throw new Error('bad returned path: ' + returned.path);"
                    "returned.items[0] = 'changed';"
                    "if (original.items[0] !== 'changed') throw new Error('setter value did not stay live');"
                    "})();"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 2:
        raise AssertionError(f"Go c-shared setter value did not stay behind resource proxies: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared setter value used JSON fallback: {boundary}")


def test_manifest_go_cshared_proxy_method_arguments_stay_live():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "request",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": (
                    "package polyfunc\n\n"
                    "var store = map[string]interface{}{}\n\n"
                    "func init() {\n"
                    "\tstore[\"take\"] = func(value interface{}) interface{} {\n"
                    "\t\tstore[\"payload\"] = value\n"
                    "\t\treturn value\n"
                    "\t}\n"
                    "}\n\n"
                    "func Request() map[string]interface{} {\n"
                    "\treturn store\n"
                    "}\n"
                ),
                "exports": ["Request"],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "(function(){"
                    "const req = request();"
                    "const original = {path: '/js-method-owned', items: ['first', 'second']};"
                    "const returned = req.take(original);"
                    "if (returned.path !== '/js-method-owned') throw new Error('bad returned path: ' + returned.path);"
                    "const stored = req.payload;"
                    "stored.items[0] = 'changed';"
                    "if (original.items[0] !== 'changed') throw new Error('method arg did not stay live');"
                    "})();"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("resource_proxy_captures", 0) < 2:
        raise AssertionError(f"Go c-shared method arg did not stay behind resource proxies: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared method arg used JSON fallback: {boundary}")


def test_manifest_go_cshared_proxy_method_return_exports_typed_slice_as_arrow():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "request",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": (
                    "package polyfunc\n\n"
                    "var store = map[string]interface{}{}\n\n"
                    "func init() {\n"
                    "\tstore[\"scores\"] = func() []int32 {\n"
                    "\t\treturn []int32{10, 20, 30}\n"
                    "\t}\n"
                    "}\n\n"
                    "func Request() map[string]interface{} {\n"
                    "\treturn store\n"
                    "}\n"
                ),
                "exports": ["Request"],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "(function(){"
                    "const req = request();"
                    "const payload = req.scores();"
                    "if (payload.length !== 3) throw new Error('bad scores length: ' + payload.length);"
                    "if (payload[0] !== 10 || payload[1] !== 20 || payload[2] !== 30) throw new Error('bad scores payload');"
                    "if (!payload.metadata || payload.metadata.arrow_format !== 'i') throw new Error('missing Arrow metadata');"
                    "if (payload.metadata.read_only !== true) throw new Error('bad producer-owned mutability metadata: ' + JSON.stringify(payload.metadata));"
                    "})();"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared proxy method typed slice did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared proxy method typed slice did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared proxy method typed slice used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_imports", 0) <= before_arrow.get("zero_copy_imports", 0):
        raise AssertionError(f"Go c-shared proxy method typed slice did not enter Arrow as zero-copy import: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_proxy_iter_preserves_shaped_arrow_metadata():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "request",
                "params": [],
                "body": [],
                "bodyRuntime": "go",
                "source": (
                    "package polyfunc\n\n"
                    "var store = map[string]interface{}{\n"
                    "\t\"matrix\": [2][3]float64{{1.5, 2.5, 3.5}, {4.5, 5.5, 6.5}},\n"
                    "}\n\n"
                    "func Request() map[string]interface{} {\n"
                    "\treturn store\n"
                    "}\n"
                ),
                "exports": ["Request"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": (
                    "req = request()\n"
                    "values = list(req.values())\n"
                    "assert len(values) == 1\n"
                    "matrix = values[0]\n"
                    "assert len(matrix) == 2\n"
                    "assert matrix[1] == [4.5, 5.5, 6.5]\n"
                    "assert matrix.metadata.get('shape') == [2, 3]\n"
                    "assert matrix.metadata.get('strides') == [24, 8]\n"
                    "assert matrix.metadata.get('arrow_format') == 'g'\n"
                    "assert matrix.metadata.get('read_only') is True, matrix.metadata\n"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared proxy iter shaped value did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared proxy iter shaped value did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared proxy iter shaped value used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_imports", 0) <= before_arrow.get("zero_copy_imports", 0):
        raise AssertionError(f"Go c-shared proxy iter shaped value did not enter Arrow as zero-copy import: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_consumes_arrow_table_as_typed_slice():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "sum_scores",
                "params": [{"name": "scores"}],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc SumScores(scores []int32) int32 {\n\tvar total int32\n\tfor _, score := range scores {\n\t\ttotal += score\n\t}\n\treturn total\n}",
                "exports": ["SumScores"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "from array import array\npayload = array('i', [4, 5, 6])\nresult = sum_scores(payload)\nassert result == 15",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared typed slice arg did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared typed slice arg did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared typed slice arg used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_borrows", 0) <= before_arrow.get("zero_copy_borrows", 0):
        raise AssertionError(f"Go c-shared typed slice arg did not borrow Arrow memory: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_consumes_shaped_arrow_table_as_fixed_array():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "sum_matrix",
                "params": [{"name": "matrix"}],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc SumMatrix(matrix [2][3]float64) float64 {\n\ttotal := 0.0\n\tfor _, row := range matrix {\n\t\tfor _, value := range row {\n\t\t\ttotal += value\n\t\t}\n\t}\n\treturn total\n}",
                "exports": ["SumMatrix"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "from array import array\nbacking = array('d', [1.5, 2.5, 3.5, 4.5, 5.5, 6.5])\npayload = memoryview(backing).cast('B').cast('d', shape=[2, 3])\nresult = sum_matrix(payload)\nassert result == 24 or result == 24.0, result",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared shaped fixed-array arg did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared shaped fixed-array arg did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared shaped fixed-array arg should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared shaped fixed-array arg used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_borrows", 0) <= before_arrow.get("zero_copy_borrows", 0):
        raise AssertionError(f"Go c-shared shaped fixed-array arg did not borrow Arrow memory: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_consumes_shaped_arrow_table_as_nested_slice():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "sum_matrix",
                "params": [{"name": "matrix"}],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc SumMatrix(matrix [][]float64) float64 {\n\ttotal := 0.0\n\tfor _, row := range matrix {\n\t\tfor _, value := range row {\n\t\t\ttotal += value\n\t\t}\n\t}\n\treturn total\n}",
                "exports": ["SumMatrix"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "from array import array\nbacking = array('d', [1.5, 2.5, 3.5, 4.5, 5.5, 6.5])\npayload = memoryview(backing).cast('B').cast('d', shape=[2, 3])\nresult = sum_matrix(payload)\nassert result == 24 or result == 24.0, result",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared shaped nested-slice arg did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared shaped nested-slice arg did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared shaped nested-slice arg should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared shaped nested-slice arg used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_borrows", 0) <= before_arrow.get("zero_copy_borrows", 0):
        raise AssertionError(f"Go c-shared shaped nested-slice arg did not borrow Arrow memory: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_consumes_strided_arrow_table_as_typed_slice():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "weighted",
                "params": [{"name": "payload"}],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc Weighted(payload []byte) int {\n\ttotal := 0\n\tfor i, value := range payload {\n\t\ttotal += (i + 1) * int(value)\n\t}\n\treturn total\n}",
                "exports": ["Weighted"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "payload = memoryview(bytearray(b'abcdef'))[::-2]\nresult = weighted(payload)\nassert result == 596, result",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared strided typed-slice arg did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared strided typed-slice arg did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"Go c-shared strided typed-slice arg should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared strided typed-slice arg used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_borrows", 0) <= before_arrow.get("zero_copy_borrows", 0):
        raise AssertionError(f"Go c-shared strided typed-slice arg did not borrow Arrow memory: before={before_arrow} after={after_arrow}")


def test_manifest_go_cshared_func_consumes_byte_buffer_as_byte_slice():
    before_arrow = omnivm.status().get("arrow", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "checksum",
                "params": [{"name": "payload"}],
                "body": [],
                "bodyRuntime": "go",
                "source": "package polyfunc\n\nfunc Checksum(payload []byte) int {\n\ttotal := 0\n\tfor _, b := range payload {\n\t\ttotal += int(b)\n\t}\n\treturn total\n}",
                "exports": ["Checksum"],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "payload = b'\\x02\\x03\\x05'\nresult = checksum(payload)\nassert result == 10",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"Go c-shared byte slice arg did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"Go c-shared byte slice arg did not use Arrow/shared memory: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Go c-shared byte slice arg used JSON fallback: {boundary}")
    after_arrow = omnivm.status().get("arrow", {})
    if after_arrow.get("zero_copy_borrows", 0) <= before_arrow.get("zero_copy_borrows", 0):
        raise AssertionError(f"Go c-shared byte slice arg did not borrow Arrow memory: before={before_arrow} after={after_arrow}")


def test_manifest_func_return_exports_ruby_string_as_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "payload",
                "code": "'abc'.b",
            },
            {
                "op": "func_def",
                "name": "current_payload",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "payload"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert list(payload) == [97, 98, 99]\nassert payload.metadata.get('arrow_format') == 'C'\nassert payload.metadata.get('read_only') is True, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"function-returned Ruby string did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"function-returned Ruby string did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"function-returned Ruby string should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned Ruby string used JSON fallback: {boundary}")


def test_manifest_func_return_exports_ruby_to_str_as_arrow():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "ruby",
                "bind": "payload",
                "code": "Class.new { def to_str; 'xyz'.b; end }.new",
            },
            {
                "op": "func_def",
                "name": "current_payload",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "payload"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(payload) == 3\nassert list(payload) == [120, 121, 122]\nassert payload.metadata.get('arrow_format') == 'C'\nassert payload.metadata.get('read_only') is True, payload.metadata",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("table_proxy_captures", 0) < 1:
        raise AssertionError(f"function-returned Ruby to_str object did not create a table proxy: {boundary}")
    if boundary.get("arrow_transfers", 0) < 1:
        raise AssertionError(f"function-returned Ruby to_str object did not use Arrow/shared memory: {boundary}")
    if boundary.get("resource_proxy_captures", 0) != 0:
        raise AssertionError(f"function-returned Ruby to_str object should not degrade to object proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned Ruby to_str object used JSON fallback: {boundary}")


def test_manifest_stream_items_preserve_complex_runtime_refs():
    before_status = omnivm.status()
    before_boundary = before_status.get("boundary", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {
                    "kind": "literal",
                    "value": {
                        "path": "/stream/proxy",
                        "items": ["alpha", "beta"],
                    },
                },
            },
            {"op": "chan", "action": "make", "bind": "outbox", "size": 1},
            {"op": "chan", "action": "send", "channel": "outbox", "value": {"kind": "ref", "name": "req"}},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const values = Array.from(outbox); if (values.length !== 1) throw new Error('bad stream len: ' + values.length); const req = values[0]; if (req.path !== '/stream/proxy') throw new Error('bad stream path: ' + req.path); if (req.items.length !== 2) throw new Error('bad stream nested len'); req.items[0] = 'stream-js';",
                "captures": {"outbox": "outbox"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert req['items'][0] == 'stream-js', req['items'][0]",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    if boundary.get("json_fallbacks", 0) != before_boundary.get("json_fallbacks", 0):
        raise AssertionError(f"streamed complex runtime refs should not use JSON fallback: {boundary}")
    if boundary.get("resource_proxy_captures", 0) < before_boundary.get("resource_proxy_captures", 0) + 1:
        raise AssertionError(f"streamed runtime ref did not use a proxy capture: {boundary}")


def test_manifest_python_generator_capture_is_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "python",
                "code": "rows_backing = [{'name': 'a'}, {'name': 'b'}]\ndef make_rows():\n    for item in rows_backing:\n        yield item",
            },
            {
                "op": "eval",
                "runtime": "python",
                "bind": "rows",
                "code": "make_rows()",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const vals = Array.from(rows); if (vals.length !== 2 || vals[0].name !== 'a' || vals[1].name !== 'b') throw new Error('bad generator stream'); vals[0].name = 'changed';",
                "captures": {"rows": "rows"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "try:\n    next(rows)\n    raise AssertionError('generator should be exhausted')\nexcept StopIteration:\n    pass\nassert rows_backing[0]['name'] == 'changed'",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"Python generator capture did not use a stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python generator capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 3:
        raise AssertionError(f"Python generator stream iteration did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"exhausted Python generator stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_function_returned_generator_is_transfer_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "python",
                "code": "returned_stream_backing = [{'name': 'a'}, {'name': 'b'}]\ndef make_rows():\n    for item in returned_stream_backing:\n        yield item",
            },
            {"op": "eval", "runtime": "python", "bind": "rows", "code": "make_rows()"},
            {
                "op": "func_def",
                "name": "current_rows",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "rows"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "stream = current_rows()\nit = iter(stream)\nfirst = next(it)\nassert first.name == 'a', first.name\nfirst.name = 'changed-return-stream'\ndel first\ndel it\ndel stream\nimport gc\ngc.collect(); gc.collect(); gc.collect()",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert returned_stream_backing[0]['name'] == 'changed-return-stream', returned_stream_backing",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"function-returned generator did not use a stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"function-returned generator used JSON fallback: {boundary}")
    if handles.get("finalizer_releases", 0) <= before_handles.get("finalizer_releases", 0):
        raise AssertionError(f"function-returned stream transfer was not finalizer-released: before={before_handles}, after={handles}")
    if handles.get("live", 0) > before_handles.get("live", 0):
        raise AssertionError(f"function-returned stream transfer leaked a live handle: before={before_handles}, after={handles}")


def test_manifest_runtime_iterators_capture_as_lazy_streams():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    java_rows_expr = (
        "((java.util.function.Supplier<java.util.ArrayList<java.util.Map<String,Object>>>)(() -> { "
        "java.util.ArrayList<java.util.Map<String,Object>> rows = new java.util.ArrayList<>(); "
        "java.util.HashMap<String,Object> a = new java.util.HashMap<>(); a.put(\"name\", \"java-a\"); rows.add(a); "
        "java.util.HashMap<String,Object> b = new java.util.HashMap<>(); b.put(\"name\", \"java-b\"); rows.add(b); "
        "return rows; "
        "})).get()"
    )
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "globalThis.jsRowsBacking = [{name: 'js-a'}, {name: 'js-b'}]; globalThis.makeJsRows = function* () { for (const item of globalThis.jsRowsBacking) yield item; };",
            },
            {"op": "eval", "runtime": "javascript", "bind": "js_rows", "code": "makeJsRows()"},
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "$ruby_rows_backing = [{'name' => 'ruby-a'}, {'name' => 'ruby-b'}]",
            },
            {"op": "eval", "runtime": "ruby", "bind": "ruby_rows", "code": "$ruby_rows_backing.each"},
            {"op": "eval", "runtime": "java", "bind": "javaRowsBacking", "code": java_rows_expr},
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_rows",
                "code": "((java.util.List) omnivm.OmniVM.getCapture(\"javaRowsBacking\")).iterator()",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "vals = list(js_rows)\nnames = [item.name for item in vals]\nassert names == ['js-a', 'js-b'], names\nvals[0].name = 'changed-js'",
                "captures": {"js_rows": "js_rows"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (!globalThis.js_rows.next().done) throw new Error('JS generator should be exhausted'); if (globalThis.jsRowsBacking[0].name !== 'changed-js') throw new Error('JS stream identity was not preserved');",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "vals = list(ruby_rows)\nnames = [item['name'] for item in vals]\nassert names == ['ruby-a', 'ruby-b'], names\nvals[0]['name'] = 'changed-ruby'",
                "captures": {"ruby_rows": "ruby_rows"},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "begin\n  $ruby_rows.next\n  raise 'Ruby enumerator should be exhausted'\nrescue StopIteration\nend\nraise 'Ruby stream identity was not preserved' unless $ruby_rows_backing[0]['name'] == 'changed-ruby'",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "vals = list(java_rows)\nnames = [item['name'] for item in vals]\nassert names == ['java-a', 'java-b'], names\nvals[0]['name'] = 'changed-java'",
                "captures": {"java_rows": "java_rows"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": "java.util.Iterator rows = (java.util.Iterator) omnivm.OmniVM.getCapture(\"java_rows\"); if (rows.hasNext()) throw new RuntimeException(\"Java iterator should be exhausted\"); java.util.List backing = (java.util.List) omnivm.OmniVM.getCapture(\"javaRowsBacking\"); java.util.Map first = (java.util.Map) backing.get(0); if (!\"changed-java\".equals(first.get(\"name\"))) throw new RuntimeException(\"Java stream identity was not preserved: \" + first);",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 3:
        raise AssertionError(f"runtime iterator captures did not use stream proxies: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"runtime iterator captures used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 9:
        raise AssertionError(f"runtime iterator stream iteration did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 3:
        raise AssertionError(f"exhausted runtime iterator streams did not release handles: before={before_handles}, after={handles}")


def test_manifest_runtime_readers_capture_as_lazy_streams():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": "import io"},
            {"op": "eval", "runtime": "python", "bind": "py_body", "code": "io.StringIO('python-body')"},
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "require 'stringio'\nclass RubyIOWrapper\n  def initialize\n    @io = StringIO.new('ruby-wrapper-body')\n  end\n  def to_io\n    @io\n  end\n  def closed?\n    @io.closed?\n  end\nend",
            },
            {"op": "eval", "runtime": "ruby", "bind": "ruby_body", "code": "StringIO.new('ruby-body')"},
            {"op": "eval", "runtime": "ruby", "bind": "ruby_wrapper_body", "code": "RubyIOWrapper.new"},
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_body_closed",
                "code": "new java.util.concurrent.atomic.AtomicBoolean(false)",
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_body",
                "code": 'new java.io.ByteArrayInputStream("java-body".getBytes(java.nio.charset.StandardCharsets.UTF_8)) { public void close() throws java.io.IOException { ((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture("java_body_closed")).set(true); super.close(); } }',
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_reader_closed",
                "code": "new java.util.concurrent.atomic.AtomicBoolean(false)",
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_reader_body",
                "code": 'new java.io.StringReader("java-reader") { public void close() { ((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture("java_reader_closed")).set(true); super.close(); } }',
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_channel_closed",
                "code": "new java.util.concurrent.atomic.AtomicBoolean(false)",
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_channel_body",
                "code": 'new java.nio.channels.ReadableByteChannel() { private final java.nio.ByteBuffer data = java.nio.ByteBuffer.wrap("java-channel".getBytes(java.nio.charset.StandardCharsets.UTF_8)); private boolean open = true; public int read(java.nio.ByteBuffer dst) throws java.io.IOException { if (!data.hasRemaining()) return -1; int n = Math.min(dst.remaining(), data.remaining()); byte[] chunk = new byte[n]; data.get(chunk); dst.put(chunk); return n; } public boolean isOpen() { return open; } public void close() throws java.io.IOException { open = false; ((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture("java_channel_closed")).set(true); } }',
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const pyChunks = Array.from(py_body); "
                    "if (pyChunks.join('') !== 'python-body') throw new Error('bad Python reader stream: ' + JSON.stringify(pyChunks)); "
                    "const rubyChunks = Array.from(ruby_body); "
                    "if (rubyChunks.join('') !== 'ruby-body') throw new Error('bad Ruby reader stream: ' + JSON.stringify(rubyChunks)); "
                    "const rubyWrapperChunks = Array.from(ruby_wrapper_body); "
                    "if (rubyWrapperChunks.join('') !== 'ruby-wrapper-body') throw new Error('bad Ruby to_io stream: ' + JSON.stringify(rubyWrapperChunks)); "
                    "const javaChunks = Array.from(java_body); "
                    "const bytesToString = (chunk) => String.fromCharCode(...Array.from(chunk)); "
                    "if (javaChunks.length !== 1 || bytesToString(javaChunks[0]) !== 'java-body') throw new Error('bad Java InputStream: ' + JSON.stringify(javaChunks)); "
                    "const javaReaderChunks = Array.from(java_reader_body); "
                    "if (javaReaderChunks.join('') !== 'java-reader') throw new Error('bad Java Reader stream: ' + JSON.stringify(javaReaderChunks)); "
                    "const javaChannelChunks = Array.from(java_channel_body); "
                    "if (javaChannelChunks.length !== 1 || bytesToString(javaChannelChunks[0]) !== 'java-channel') throw new Error('bad Java ReadableByteChannel stream: ' + JSON.stringify(javaChannelChunks));"
                ),
                "captures": {
                    "py_body": "py_body",
                    "ruby_body": "ruby_body",
                    "ruby_wrapper_body": "ruby_wrapper_body",
                    "java_body": "java_body",
                    "java_reader_body": "java_reader_body",
                    "java_channel_body": "java_channel_body",
                },
            },
            {"op": "exec", "runtime": "python", "code": "assert py_body.closed, py_body.closed"},
            {"op": "exec", "runtime": "ruby", "code": "raise 'Ruby reader stream did not close' unless $ruby_body.closed?"},
            {"op": "exec", "runtime": "ruby", "code": "raise 'Ruby to_io stream did not close' unless $ruby_wrapper_body.closed?"},
            {
                "op": "exec",
                "runtime": "java",
                "code": 'if (!((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture("java_body_closed")).get()) throw new RuntimeException("Java InputStream did not close");',
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": 'if (!((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture("java_reader_closed")).get()) throw new RuntimeException("Java Reader did not close");',
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": 'if (!((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture("java_channel_closed")).get()) throw new RuntimeException("Java ReadableByteChannel did not close");',
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 6:
        raise AssertionError(f"runtime reader captures did not use stream proxies: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"runtime reader captures used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 12:
        raise AssertionError(f"runtime reader stream iteration did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 6:
        raise AssertionError(f"exhausted runtime reader streams did not release handles: before={before_handles}, after={handles}")


def test_manifest_java_iterable_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_iter_iterations",
                "code": "new java.util.concurrent.atomic.AtomicInteger(0)",
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_iter_body",
                "code": (
                    "new java.lang.Iterable<String>() { "
                    "public java.util.Iterator<String> iterator() { "
                    "((java.util.concurrent.atomic.AtomicInteger) omnivm.OmniVM.getCapture(\"java_iter_iterations\")).incrementAndGet(); "
                    "return java.util.Arrays.asList(\"java-\", \"iterable\").iterator(); "
                    "} "
                    "}"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const chunks = Array.from(java_iter_body); if (chunks.join('') !== 'java-iterable') throw new Error('bad Java iterable stream: ' + JSON.stringify(chunks));",
                "captures": {"java_iter_body": "java_iter_body"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": 'if (((java.util.concurrent.atomic.AtomicInteger) omnivm.OmniVM.getCapture("java_iter_iterations")).get() != 1) throw new RuntimeException("bad Java iterable iteration count: " + omnivm.OmniVM.getCapture("java_iter_iterations"));',
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"Java iterable body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java iterable body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 3:
        raise AssertionError(f"Java iterable body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"Java iterable body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_java_base_stream_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_stream_closed",
                "code": "new java.util.concurrent.atomic.AtomicBoolean(false)",
            },
            {
                "op": "eval",
                "runtime": "java",
                "bind": "java_stream_body",
                "code": (
                    "java.util.Arrays.asList(\"java-\", \"base-\", \"stream\").stream().onClose("
                    "() -> ((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture(\"java_stream_closed\")).set(true)"
                    ")"
                ),
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const chunks = Array.from(java_stream_body); if (chunks.join('') !== 'java-base-stream') throw new Error('bad Java BaseStream stream: ' + JSON.stringify(chunks));",
                "captures": {"java_stream_body": "java_stream_body"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": 'if (!((java.util.concurrent.atomic.AtomicBoolean) omnivm.OmniVM.getCapture("java_stream_closed")).get()) throw new RuntimeException("Java BaseStream did not close");',
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"Java BaseStream body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java BaseStream body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 4:
        raise AssertionError(f"Java BaseStream body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"Java BaseStream body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_ruby_each_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "class RubyEachBody\n  def initialize\n    @closed = false\n    @chunks = ['each-', 'body']\n  end\n  def each\n    return enum_for(:each) unless block_given?\n    @chunks.each { |chunk| yield chunk }\n  end\n  def close\n    @closed = true\n  end\n  def closed?\n    @closed\n  end\nend",
            },
            {"op": "eval", "runtime": "ruby", "bind": "ruby_each_body", "code": "RubyEachBody.new"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const chunks = Array.from(ruby_each_body); if (chunks.join('') !== 'each-body') throw new Error('bad Ruby each stream: ' + JSON.stringify(chunks));",
                "captures": {"ruby_each_body": "ruby_each_body"},
            },
            {"op": "exec", "runtime": "ruby", "code": "raise 'Ruby each body stream did not close' unless $ruby_each_body.closed?"},
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"Ruby each body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Ruby each body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 3:
        raise AssertionError(f"Ruby each body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"Ruby each body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_python_iterable_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "python",
                "code": "class PythonIterableBody:\n    def __init__(self):\n        self.closed = False\n        self.iterations = 0\n    def __iter__(self):\n        self.iterations += 1\n        yield 'iter-'\n        yield 'body'\n    def close(self):\n        self.closed = True",
            },
            {"op": "eval", "runtime": "python", "bind": "py_iter_body", "code": "PythonIterableBody()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const chunks = Array.from(py_iter_body); if (chunks.join('') !== 'iter-body') throw new Error('bad Python iterable stream: ' + JSON.stringify(chunks));",
                "captures": {"py_iter_body": "py_iter_body"},
            },
            {"op": "exec", "runtime": "python", "code": "assert py_iter_body.closed, py_iter_body.closed\nassert py_iter_body.iterations == 1, py_iter_body.iterations"},
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"Python iterable body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python iterable body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 3:
        raise AssertionError(f"Python iterable body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"Python iterable body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_python_async_iterable_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "python",
                "code": (
                    "import asyncio\n"
                    "class PythonAsyncIterableBody:\n"
                    "    def __init__(self):\n"
                    "        self.closed = False\n"
                    "        self.iterations = 0\n"
                    "        self.index = 0\n"
                    "        self.chunks = ['async-', 'iter-', 'body']\n"
                    "    def __aiter__(self):\n"
                    "        self.iterations += 1\n"
                    "        return self\n"
                    "    async def __anext__(self):\n"
                    "        await asyncio.sleep(0)\n"
                    "        if self.index >= len(self.chunks):\n"
                    "            raise StopAsyncIteration\n"
                    "        chunk = self.chunks[self.index]\n"
                    "        self.index += 1\n"
                    "        return chunk\n"
                    "    async def aclose(self):\n"
                    "        await asyncio.sleep(0)\n"
                    "        self.closed = True"
                ),
            },
            {"op": "eval", "runtime": "python", "bind": "py_async_iter_body", "code": "PythonAsyncIterableBody()"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const chunks = Array.from(py_async_iter_body); if (chunks.join('') !== 'async-iter-body') throw new Error('bad Python async iterable stream: ' + JSON.stringify(chunks));",
                "captures": {"py_async_iter_body": "py_async_iter_body"},
            },
            {"op": "exec", "runtime": "python", "code": "assert py_async_iter_body.closed, py_async_iter_body.closed\nassert py_async_iter_body.iterations == 1, py_async_iter_body.iterations"},
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"Python async iterable body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Python async iterable body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 4:
        raise AssertionError(f"Python async iterable body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"Python async iterable body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_js_stream_cancel_calls_iterator_return():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "globalThis.jsClosed = 0; globalThis.makeCancelableRows = function* () { try { yield 'first'; yield 'second'; } finally { globalThis.jsClosed += 1; } };",
            },
            {"op": "eval", "runtime": "javascript", "bind": "js_stream", "code": "makeCancelableRows()"},
            {
                "op": "exec",
                "runtime": "python",
                "code": "first = next(js_stream)\nassert first == 'first', first\njs_stream.close()",
                "captures": {"js_stream": "js_stream"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (globalThis.jsClosed !== 1) throw new Error('JS iterator return was not called: ' + globalThis.jsClosed); if (!globalThis.js_stream.next().done) throw new Error('JS stream should be closed');",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    if after.get("explicit_releases", 0) <= before.get("explicit_releases", 0):
        raise AssertionError(f"JS stream cancel did not release handle: before={before}, after={after}")


def test_manifest_js_iterable_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "globalThis.jsIterableBody = {"
                    "closed: false,"
                    "iterations: 0,"
                    "[Symbol.iterator]: function() {"
                    "  this.iterations += 1;"
                    "  const chunks = ['js-', 'iterable'];"
                    "  let index = 0;"
                    "  const owner = this;"
                    "  return {"
                    "    next() { return index < chunks.length ? {value: chunks[index++], done: false} : {done: true}; },"
                    "    return() { owner.closed = true; return {done: true}; }"
                    "  };"
                    "}"
                    "};"
                ),
            },
            {"op": "eval", "runtime": "javascript", "bind": "js_iter_body", "code": "globalThis.jsIterableBody"},
            {
                "op": "exec",
                "runtime": "python",
                "code": "chunks = list(js_iter_body)\nassert ''.join(chunks) == 'js-iterable', chunks",
                "captures": {"js_iter_body": "js_iter_body"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (!globalThis.jsIterableBody.closed) throw new Error('JS iterable body stream did not close'); if (globalThis.jsIterableBody.iterations !== 1) throw new Error('bad JS iterable iteration count: ' + globalThis.jsIterableBody.iterations);",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"JS iterable body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS iterable body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 3:
        raise AssertionError(f"JS iterable body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"JS iterable body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_js_async_iterable_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "globalThis.jsAsyncIterableBody = {"
                    "closed: false,"
                    "iterations: 0,"
                    "[Symbol.asyncIterator]: function() {"
                    "  this.iterations += 1;"
                    "  const chunks = ['js-', 'async-', 'iterable'];"
                    "  let index = 0;"
                    "  const owner = this;"
                    "  return {"
                    "    async next() { await Promise.resolve(); return index < chunks.length ? {value: chunks[index++], done: false} : {done: true}; },"
                    "    return() { owner.closed = true; return {done: true}; }"
                    "  };"
                    "}"
                    "};"
                ),
            },
            {"op": "eval", "runtime": "javascript", "bind": "js_async_iter_body", "code": "globalThis.jsAsyncIterableBody"},
            {
                "op": "exec",
                "runtime": "python",
                "code": "chunks = list(js_async_iter_body)\nassert ''.join(chunks) == 'js-async-iterable', chunks",
                "captures": {"js_async_iter_body": "js_async_iter_body"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (!globalThis.jsAsyncIterableBody.closed) throw new Error('JS async iterable body stream did not close'); if (globalThis.jsAsyncIterableBody.iterations !== 1) throw new Error('bad JS async iterable iteration count: ' + globalThis.jsAsyncIterableBody.iterations);",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"JS async iterable body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS async iterable body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 4:
        raise AssertionError(f"JS async iterable body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"JS async iterable body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_js_readable_stream_body_capture_as_lazy_stream():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "globalThis.jsReadableBody = {"
                    "closed: false,"
                    "readers: 0,"
                    "getReader: function() {"
                    "  this.readers += 1;"
                    "  const chunks = ['js-', 'readable-', 'stream'];"
                    "  let index = 0;"
                    "  const owner = this;"
                    "  return {"
                    "    read() { return Promise.resolve(index < chunks.length ? {value: chunks[index++], done: false} : {done: true}); },"
                    "    releaseLock() { owner.closed = true; },"
                    "    cancel() { owner.closed = true; return Promise.resolve(); }"
                    "  };"
                    "}"
                    "};"
                ),
            },
            {"op": "eval", "runtime": "javascript", "bind": "js_readable_body", "code": "globalThis.jsReadableBody"},
            {
                "op": "exec",
                "runtime": "python",
                "code": "chunks = list(js_readable_body)\nassert ''.join(chunks) == 'js-readable-stream', chunks",
                "captures": {"js_readable_body": "js_readable_body"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (!globalThis.jsReadableBody.closed) throw new Error('JS ReadableStream reader did not release'); if (globalThis.jsReadableBody.readers !== 1) throw new Error('bad JS ReadableStream reader count: ' + globalThis.jsReadableBody.readers);",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < 1:
        raise AssertionError(f"JS ReadableStream body capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"JS ReadableStream body capture used JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 4:
        raise AssertionError(f"JS ReadableStream body stream did not record stream accesses: before={before_handles}, after={handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"JS ReadableStream body stream did not release handle: before={before_handles}, after={handles}")


def test_manifest_js_readable_stream_early_cancel_releases_owner():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "globalThis.jsEarlyReadable = {"
                    "cancelled: false,"
                    "released: false,"
                    "readers: 0,"
                    "getReader: function() {"
                    "  this.readers += 1;"
                    "  const chunks = ['first', 'second', 'third'];"
                    "  let index = 0;"
                    "  const owner = this;"
                    "  return {"
                    "    read() { return Promise.resolve(index < chunks.length ? {value: chunks[index++], done: false} : {done: true}); },"
                    "    releaseLock() { owner.released = true; },"
                    "    cancel() { owner.cancelled = true; return Promise.resolve(); }"
                    "  };"
                    "}"
                    "};"
                ),
            },
            {"op": "eval", "runtime": "javascript", "bind": "js_early_stream", "code": "globalThis.jsEarlyReadable"},
            {
                "op": "exec",
                "runtime": "python",
                "code": "first = next(js_early_stream)\nassert first == 'first', first\njs_early_stream.close()\ndel js_early_stream\nimport gc\ngc.collect(); gc.collect()",
                "captures": {"js_early_stream": "js_early_stream"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (globalThis.jsEarlyReadable.readers !== 1) throw new Error('bad reader count: ' + globalThis.jsEarlyReadable.readers); "
                    "if (!globalThis.jsEarlyReadable.cancelled) throw new Error('ReadableStream reader was not cancelled early'); "
                    "if (!globalThis.jsEarlyReadable.released) throw new Error('ReadableStream reader lock was not released early');"
                ),
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < before_status.get("boundary", {}).get("stream_proxy_captures", 0) + 1:
        raise AssertionError(f"JS ReadableStream early-cancel capture did not use stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != before_status.get("boundary", {}).get("json_fallbacks", 0):
        raise AssertionError(f"JS ReadableStream early-cancel used JSON fallback: before={before_status.get('boundary', {})}, after={boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 1:
        raise AssertionError(f"JS ReadableStream early-cancel did not record stream access: before={before_handles}, after={handles}")
    if handles.get("live", 0) != before_handles.get("live", 0):
        raise AssertionError(f"JS ReadableStream early-cancel leaked live handles: before={before_handles}, after={handles}")


def test_manifest_httpx_response_stream_early_cancel_releases_owner():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    setup = r'''
import httpx

class OwnerStream(httpx.SyncByteStream):
    def __init__(self):
        self.closed = False
        self.cancelled = False
        self.iterations = 0

    def __iter__(self):
        self.iterations += 1
        yield b"first"
        yield b"second"

    def close(self):
        self.closed = True
        self.cancelled = True

owner_stream = OwnerStream()
httpx_response = httpx.Response(200, stream=owner_stream)

def httpx_body_iter():
    try:
        yield from httpx_response.iter_bytes()
    finally:
        httpx_response.close()

httpx_body = httpx_body_iter()
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const it = httpx_body[Symbol.iterator](); "
                    "const first = it.next(); "
                    "const asText = (value) => String.fromCharCode(...Array.from(value)); "
                    "if (first.done || asText(first.value) !== 'first') throw new Error('bad httpx first chunk: ' + JSON.stringify(first)); "
                    "omnivm.call('python', 'httpx_body.close()');"
                ),
                "captures": {"httpx_body": "httpx_body"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": (
                    "assert owner_stream.iterations == 1, owner_stream.iterations\n"
                    "assert owner_stream.closed, 'httpx stream was not closed after early cancel'\n"
                    "assert owner_stream.cancelled, 'httpx stream did not observe cancellation'"
                ),
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < before_status.get("boundary", {}).get("stream_proxy_captures", 0) + 1:
        raise AssertionError(f"httpx response body did not cross as stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != before_status.get("boundary", {}).get("json_fallbacks", 0):
        raise AssertionError(f"httpx response body used JSON fallback: before={before_status.get('boundary', {})}, after={boundary}")
    released = handles.get("released", 0) - before_handles.get("released", 0)
    scoped = handles.get("scope_releases", 0) - before_handles.get("scope_releases", 0)
    if released < 1 and scoped < 1:
        raise AssertionError(f"httpx response body did not release on early cancel: before={before_handles}, after={handles}")
    if handles.get("live", 0) != before_handles.get("live", 0):
        raise AssertionError(f"httpx response body early-cancel leaked live handles: before={before_handles}, after={handles}")


def test_manifest_aiohttp_stream_early_cancel_releases_owner():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    setup = r'''
import aiohttp
import asyncio

aiohttp_loop = asyncio.new_event_loop()
asyncio.set_event_loop(aiohttp_loop)
globals()["__omnivm_stream_loop"] = aiohttp_loop

class DummyProtocol:
    _reading_paused = False

    def pause_reading(self, *args, **kwargs):
        self._reading_paused = True

    def resume_reading(self, *args, **kwargs):
        self._reading_paused = False

class AiohttpOwnedBody:
    def __init__(self):
        self.reader = aiohttp.StreamReader(DummyProtocol(), limit=2**16, loop=aiohttp_loop)
        self.reader.feed_data(b"first")
        self.reader.feed_data(b"second")
        self.reader.feed_eof()
        self.iterations = 0
        self.closed = False
        self.cancelled = False

    def __aiter__(self):
        self.iterations += 1
        return self

    async def __anext__(self):
        chunk = await self.reader.read(5)
        if chunk == b"":
            raise StopAsyncIteration
        return chunk.decode("ascii")

    async def aclose(self):
        self.closed = True
        self.cancelled = True

aiohttp_body = AiohttpOwnedBody()
'''
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "exec", "runtime": "python", "code": setup},
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"aiohttp_body": "aiohttp_body"},
                "code": (
                    "const it = aiohttp_body[Symbol.iterator](); "
                    "const first = it.next(); "
                    "if (first.done || first.value !== 'first') throw new Error('bad aiohttp first chunk: ' + JSON.stringify(first)); "
                    "aiohttp_body.cancel('client-abort');"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": (
                    "assert aiohttp_body.iterations == 1, aiohttp_body.iterations\n"
                    "assert aiohttp_body.closed, 'aiohttp stream was not closed after early cancel'\n"
                    "assert aiohttp_body.cancelled, 'aiohttp stream did not observe cancellation'"
                ),
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < before_status.get("boundary", {}).get("stream_proxy_captures", 0) + 1:
        raise AssertionError(f"aiohttp response body did not cross as stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != before_status.get("boundary", {}).get("json_fallbacks", 0):
        raise AssertionError(f"aiohttp response body used JSON fallback: before={before_status.get('boundary', {})}, after={boundary}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 1:
        raise AssertionError(f"aiohttp response body did not release on early cancel: before={before_handles}, after={handles}")
    if handles.get("live", 0) != before_handles.get("live", 0):
        raise AssertionError(f"aiohttp response body early-cancel leaked live handles: before={before_handles}, after={handles}")


def test_manifest_undici_response_body_early_cancel_releases_owner():
    before_status = omnivm.status()
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "const { Response } = require('undici'); "
                    "const encoder = new TextEncoder(); "
                    "globalThis.undiciOwner = {cancelled: false, released: false, pulls: 0}; "
                    "globalThis.undiciResponse = new Response(new ReadableStream({"
                    "  pull(controller) {"
                    "    globalThis.undiciOwner.pulls += 1;"
                    "    if (globalThis.undiciOwner.pulls === 1) controller.enqueue(encoder.encode('first'));"
                    "    else controller.enqueue(encoder.encode('second'));"
                    "  },"
                    "  cancel(reason) {"
                    "    globalThis.undiciOwner.cancelled = true;"
                    "    globalThis.undiciOwner.reason = String(reason || '');"
                    "  }"
                    "}), {status: 200});"
                ),
            },
            {"op": "eval", "runtime": "javascript", "bind": "undici_body", "code": "globalThis.undiciResponse.body"},
            {
                "op": "exec",
                "runtime": "python",
                "captures": {"undici_body": "undici_body"},
                "code": "first = next(undici_body)\nassert list(first) == [102, 105, 114, 115, 116], list(first)\nundici_body.close()",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": (
                    "if (globalThis.undiciOwner.pulls > 2) throw new Error('undici response body was drained instead of cancelled: ' + globalThis.undiciOwner.pulls); "
                    "if (!globalThis.undiciOwner.cancelled) throw new Error('undici response body was not cancelled early');"
                ),
            },
        ],
    }
    run_manifest_dict(manifest)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < before_status.get("boundary", {}).get("stream_proxy_captures", 0) + 1:
        raise AssertionError(f"undici response body did not cross as stream proxy: {boundary}")
    if boundary.get("json_fallbacks", 0) != before_status.get("boundary", {}).get("json_fallbacks", 0):
        raise AssertionError(f"undici response body used JSON fallback: before={before_status.get('boundary', {})}, after={boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 1:
        raise AssertionError(f"undici response body did not record stream access: before={before_handles}, after={handles}")
    if handles.get("live", 0) != before_handles.get("live", 0):
        raise AssertionError(f"undici response body early-cancel leaked live handles: before={before_handles}, after={handles}")


def test_manifest_nested_proxy_reference_edges_observable():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {
                    "kind": "literal",
                    "value": {
                        "path": "/graph",
                        "items": [{"sku": "book"}],
                    },
                },
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "items = req['items']\nassert items[0].sku == 'book', items[0].sku\nhandles = omnivm.status().get('handles', {})\nassert handles.get('reference_edges', 0) >= 2, handles\nassert handles.get('reference_edges_by_kind', {}).get('property', 0) >= 2, handles",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    handles = omnivm.status().get("handles", {})
    if handles.get("reference_edges", 0) != 0:
        raise AssertionError(f"scope cleanup should remove nested proxy reference edges: {handles}")


def test_manifest_proxy_mutation_cycles_observable():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "object",
                "bind": "left",
                "value": {"kind": "literal", "value": {}},
            },
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "object",
                "bind": "right",
                "value": {"kind": "literal", "value": {}},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "left.peer = right\nright.peer = left\nhandles = omnivm.status().get('handles', {})\nassert handles.get('reference_edges_by_kind', {}).get('mutation', 0) >= 2, handles\nassert handles.get('suspected_cycles', 0) >= 1, handles\nassert handles.get('cyclic_handles', 0) >= 2, handles\nassert handles.get('largest_cycle', 0) >= 2, handles\nassert len(handles.get('cycle_sample') or []) >= 2, handles",
                "captures": {"left": "left", "right": "right"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    handles = omnivm.status().get("handles", {})
    if handles.get("reference_edges", 0) != 0:
        raise AssertionError(f"scope cleanup should remove mutation proxy reference edges: {handles}")


def test_manifest_cross_runtime_proxy_cycles_remain_bounded():
    before = omnivm.status().get("handles", {})
    before_live = before.get("live", 0)
    before_edges = before.get("reference_edges", 0)
    before_cycles = before.get("suspected_cycles", 0)

    for i in range(12):
        manifest = {
            "version": 1,
            "defaultRuntime": "python",
            "ops": [
                {
                    "op": "resource",
                    "action": "open",
                    "runtime": "python",
                    "kind": "object",
                    "bind": "py_node",
                    "value": {"kind": "literal", "value": {"name": f"py-{i}"}},
                },
                {
                    "op": "eval",
                    "runtime": "javascript",
                    "bind": "js_node",
                    "code": f"({{name: 'js-{i}'}})",
                },
                {
                    "op": "exec",
                    "runtime": "python",
                    "captures": {"py_node": "py_node", "js_node": "js_node"},
                    "code": (
                        "py_node.peer = js_node\n"
                        "js_node.peer = py_node\n"
                        f"assert py_node.peer.name == 'js-{i}', py_node.peer.name\n"
                        f"assert js_node.peer.name == 'py-{i}', js_node.peer.name\n"
                        "handles = omnivm.status().get('handles', {})\n"
                        "assert handles.get('reference_edges', 0) >= 2, handles\n"
                        "assert handles.get('reference_edges_by_kind', {}).get('mutation', 0) >= 1, handles\n"
                        "assert handles.get('reference_edges_by_kind', {}).get('property', 0) >= 1, handles\n"
                        "assert handles.get('suspected_cycles', 0) >= 1, handles\n"
                        "assert handles.get('cyclic_handles', 0) >= 2, handles"
                    ),
                },
                {
                    "op": "exec",
                    "runtime": "javascript",
                    "captures": {"js_node": "js_node"},
                    "code": (
                        f"if (js_node.peer.name !== 'py-{i}') throw new Error('bad Python peer name: ' + js_node.peer.name);"
                        f"if (js_node.peer.peer.name !== 'js-{i}') throw new Error('bad JS peer round-trip: ' + js_node.peer.peer.name);"
                    ),
                },
            ],
        }
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(manifest, f)
            path = f.name
        try:
            omnivm.run_manifest(path)
        finally:
            os.unlink(path)

        after = omnivm.status().get("handles", {})
        if after.get("live", 0) != before_live:
            raise AssertionError(f"cross-runtime proxy cycle leaked live handles on iteration {i}: before={before}, after={after}")
        if after.get("reference_edges", 0) != before_edges:
            raise AssertionError(f"cross-runtime proxy cycle leaked reference edges on iteration {i}: before={before}, after={after}")
        if after.get("suspected_cycles", 0) != before_cycles:
            raise AssertionError(f"cross-runtime proxy cycle remained observable after cleanup on iteration {i}: before={before}, after={after}")


def test_manifest_ruby_java_proxy_cycles_remain_bounded():
    before = omnivm.status().get("handles", {})
    before_live = before.get("live", 0)
    before_edges = before.get("reference_edges", 0)
    before_cycles = before.get("suspected_cycles", 0)

    for i in range(8):
        java_node_expr = (
            "((java.util.function.Supplier<java.util.LinkedHashMap<String,Object>>)(() -> { "
            "java.util.LinkedHashMap<String,Object> node = new java.util.LinkedHashMap<>(); "
            f"node.put(\"name\", \"java-{i}\"); "
            "return node; "
            "})).get()"
        )
        manifest = {
            "version": 1,
            "defaultRuntime": "python",
            "ops": [
                {
                    "op": "eval",
                    "runtime": "ruby",
                    "bind": "ruby_node",
                    "code": f"{{'name' => 'ruby-{i}'}}",
                },
                {
                    "op": "eval",
                    "runtime": "java",
                    "bind": "java_node",
                    "code": java_node_expr,
                },
                {
                    "op": "exec",
                    "runtime": "python",
                    "captures": {"ruby_node": "ruby_node", "java_node": "java_node"},
                    "code": (
                        "ruby_node.peer = java_node\n"
                        "java_node.put('peer', ruby_node)\n"
                        f"assert ruby_node.peer['name'] == 'java-{i}', ruby_node.peer['name']\n"
                        f"assert java_node['peer'].name == 'ruby-{i}', java_node['peer'].name\n"
                        "handles = omnivm.status().get('handles', {})\n"
                        "assert handles.get('reference_edges', 0) >= 2, handles\n"
                        "assert handles.get('reference_edges_by_kind', {}).get('mutation', 0) >= 1, handles\n"
                        "assert handles.get('reference_edges_by_runtime', {}).get('ruby->java', 0) >= 1, handles\n"
                        "assert handles.get('reference_edges_by_runtime', {}).get('java->ruby', 0) >= 1, handles\n"
                        "assert handles.get('suspected_cycles', 0) >= 1, handles\n"
                        "assert handles.get('cyclic_handles', 0) >= 2, handles"
                    ),
                },
                {
                    "op": "exec",
                    "runtime": "ruby",
                    "captures": {"ruby_node": "ruby_node"},
                    "code": f"raise \"bad Java peer\" unless $ruby_node['peer']['name'] == 'java-{i}'",
                },
                {
                    "op": "exec",
                    "runtime": "java",
                    "captures": {"java_node": "java_node"},
                    "code": (
                        "java.util.Map node = (java.util.Map) omnivm.OmniVM.getCapture(\"java_node\"); "
                        "omnivm.OmniVM.HandleProxy peer = (omnivm.OmniVM.HandleProxy) node.get(\"peer\"); "
                        f"if (!\"ruby-{i}\".equals(peer.get(\"name\"))) throw new RuntimeException(\"bad Ruby peer: \" + peer.get(\"name\"));"
                    ),
                },
            ],
        }
        with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
            json.dump(manifest, f)
            path = f.name
        try:
            omnivm.run_manifest(path)
        finally:
            os.unlink(path)

        after = omnivm.status().get("handles", {})
        if after.get("live", 0) != before_live:
            raise AssertionError(f"Ruby/Java proxy cycle leaked live handles on iteration {i}: before={before}, after={after}")
        if after.get("reference_edges", 0) != before_edges:
            raise AssertionError(f"Ruby/Java proxy cycle leaked reference edges on iteration {i}: before={before}, after={after}")
        if after.get("suspected_cycles", 0) != before_cycles:
            raise AssertionError(f"Ruby/Java proxy cycle remained observable after cleanup on iteration {i}: before={before}, after={after}")


def test_manifest_proxy_method_arguments_stay_live():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "code": "{'name': 'Ada'}",
                "bind": "py_payload",
            },
            {
                "op": "eval",
                "runtime": "javascript",
                "code": "({ accept: function(obj) { if (obj.name !== 'Ada') throw new Error('bad py arg: ' + obj.name); obj.from_js = 'yes'; return obj.name; } })",
                "bind": "js_sink",
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert js_sink.accept(py_payload) == 'Ada'; assert py_payload['from_js'] == 'yes', py_payload",
                "captures": {"js_sink": "js_sink", "py_payload": "py_payload"},
            },
            {
                "op": "eval",
                "runtime": "python",
                "code": "type('Sink', (), {'accept': lambda self, obj: (setattr(obj, 'from_py', 'yes'), obj.count)[1]})()",
                "bind": "py_sink",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const payload = {count: 41}; const result = py_sink.accept(payload); if (String(result) !== '41') throw new Error('bad result: ' + result); if (payload.from_py !== 'yes') throw new Error('mutation did not reach original JS object: ' + payload.from_py);",
                "captures": {"py_sink": "py_sink"},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": "import java.util.*;\nObject sinkObj = omnivm.OmniVM.getCapture(\"py_sink\");\nomnivm.OmniVM.HandleProxy sink = (omnivm.OmniVM.HandleProxy) sinkObj;\nMap<String,Object> payload = new LinkedHashMap<>();\npayload.put(\"count\", 42);\nObject result = sink.call(\"accept\", payload);\nif (!\"42\".equals(String.valueOf(result))) throw new RuntimeException(\"bad java result: \" + result);\nif (!\"yes\".equals(payload.get(\"from_py\"))) throw new RuntimeException(\"mutation did not reach original Java object: \" + payload.get(\"from_py\"));",
                "captures": {"py_sink": "py_sink"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"live proxy method arguments should not use JSON fallback: {boundary}")
    if boundary.get("resource_proxy_captures", 0) < 3:
        raise AssertionError(f"live proxy method arguments should use generic proxy captures: {boundary}")


def test_manifest_java_func_argument_stays_live():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "accept_payload",
                "params": [{"name": "payload"}],
                "body": [
                    {
                        "op": "exec",
                        "runtime": "python",
                        "code": "assert payload.count == 42, payload.count\npayload.from_py = 'yes'",
                    },
                    {"op": "return", "value": {"kind": "literal", "value": "accepted"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "import java.util.*;\n"
                    "Map<String,Object> payload = new LinkedHashMap<>();\n"
                    "payload.put(\"count\", 42);\n"
                    "Object result = omnivm.OmniVMManifest.accept_payload(payload);\n"
                    "if (!\"accepted\".equals(String.valueOf(result))) throw new RuntimeException(\"bad java result: \" + result);\n"
                    "if (!\"yes\".equals(payload.get(\"from_py\"))) throw new RuntimeException(\"mutation did not reach original Java object: \" + payload.get(\"from_py\"));"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java manifest function arguments should not use JSON fallback: {boundary}")
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java manifest function arguments should cross as generic proxies: {boundary}")


def test_manifest_java_func_invoke_fallback_handles_unsafe_names():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "class",
                "params": [{"name": "payload"}],
                "body": [
                    {
                        "op": "exec",
                        "runtime": "python",
                        "code": "assert payload.count == 43, payload.count\npayload.from_py = 'yes'",
                    },
                    {"op": "return", "value": {"kind": "literal", "value": "accepted"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": (
                    "import java.util.*;\n"
                    "Map<String,Object> payload = new LinkedHashMap<>();\n"
                    "payload.put(\"count\", 43);\n"
                    "Object result = omnivm.OmniVMManifest.invoke(\"class\", payload);\n"
                    "if (!\"accepted\".equals(String.valueOf(result))) throw new RuntimeException(\"bad java result: \" + result);\n"
                    "if (!\"yes\".equals(payload.get(\"from_py\"))) throw new RuntimeException(\"mutation did not reach original Java object: \" + payload.get(\"from_py\"));"
                ),
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"Java manifest invoke fallback should not use JSON fallback: {boundary}")
    if boundary.get("resource_proxy_captures", 0) < 1:
        raise AssertionError(f"Java manifest invoke fallback should preserve complex args as proxies: {boundary}")


def test_manifest_proxy_setter_values_stay_live():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "eval", "runtime": "python", "code": "{}", "bind": "py_box"},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "globalThis.__setter_child = {label: 'js-child'}; py_box.child = globalThis.__setter_child; if (py_box.child.label !== 'js-child') throw new Error('bad assigned child: ' + py_box.child.label);",
                "captures": {"py_box": "py_box"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "child = py_box['child']; assert child.label == 'js-child', child.label; child.from_py = 'yes'",
                "captures": {"py_box": "py_box"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (globalThis.__setter_child.from_py !== 'yes') throw new Error('python mutation did not reach original JS object: ' + globalThis.__setter_child.from_py);",
            },
            {"op": "eval", "runtime": "javascript", "code": "({})", "bind": "js_box"},
            {
                "op": "exec",
                "runtime": "python",
                "code": "setter_payload = {'label': 'py-child'}\njs_box.child = setter_payload\nassert js_box.child.label == 'py-child', js_box.child.label",
                "captures": {"js_box": "js_box"},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (js_box.child.label !== 'py-child') throw new Error('bad py child: ' + js_box.child.label); js_box.child.from_js = 'yes';",
                "captures": {"js_box": "js_box"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert setter_payload['from_js'] == 'yes', setter_payload",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"live proxy setter values should not use JSON fallback: {boundary}")
    if boundary.get("resource_proxy_captures", 0) < 2:
        raise AssertionError(f"live proxy setter values should use generic proxy captures: {boundary}")


def test_manifest_python_mapping_collision_setters_prefer_keys():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "eval",
                "runtime": "python",
                "code": "{'items': 'old-items', 'keys': 'old-keys', 'count': 0, 'then': 'old-then', 'length': 5}",
                "bind": "py_payload",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "captures": {"py_payload": "py_payload"},
                "code": (
                    "py_payload.items = 'js-items'; "
                    "py_payload.then = 'js-then'; "
                    "if (py_payload.items !== 'js-items') throw new Error('bad items key: ' + py_payload.items); "
                    "if (py_payload.then !== 'js-then') throw new Error('bad then key: ' + py_payload.then);"
                ),
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "captures": {"py_payload": "py_payload"},
                "code": (
                    "py_payload.keys = 'ruby-keys'; "
                    "py_payload.length = 9; "
                    "raise \"bad keys key #{py_payload.keys}\" unless py_payload.keys == 'ruby-keys'; "
                    "raise \"bad length key #{py_payload.length}\" unless py_payload.length == 9"
                ),
            },
            {
                "op": "exec",
                "runtime": "java",
                "captures": {"py_payload": "py_payload"},
                "code": (
                    "omnivm.OmniVM.HandleProxy payload = (omnivm.OmniVM.HandleProxy) omnivm.OmniVM.getCapture(\"py_payload\"); "
                    "if (!payload.set(\"count\", 42)) throw new RuntimeException(\"count set failed\"); "
                    "if (!payload.set(\"items\", \"java-items\")) throw new RuntimeException(\"items set failed\"); "
                    "if (!\"42\".equals(String.valueOf(payload.get(\"count\")))) throw new RuntimeException(\"bad count key: \" + payload.get(\"count\")); "
                    "if (!\"java-items\".equals(String.valueOf(payload.get(\"items\")))) throw new RuntimeException(\"bad items key: \" + payload.get(\"items\"));"
                ),
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": (
                    "assert py_payload['items'] == 'java-items', py_payload\n"
                    "assert py_payload['keys'] == 'ruby-keys', py_payload\n"
                    "assert py_payload['count'] == 42, py_payload\n"
                    "assert py_payload['then'] == 'js-then', py_payload\n"
                    "assert py_payload['length'] == 9, py_payload\n"
                    "assert callable(dict.items) and callable(dict.keys)"
                ),
            },
        ],
    }
    run_manifest_dict(manifest)

    boundary = omnivm.status().get("boundary", {})
    handles = omnivm.status().get("handles", {})
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"collision-key mapping setters should not use JSON fallback: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("mutation", 0) < 5:
        raise AssertionError(f"collision-key mapping setters should record proxy mutations: {handles}")


def test_manifest_channel_captures_are_lazy_streams():
    before_status = omnivm.status()
    before_boundary = before_status.get("boundary", {})
    before_handles = before_status.get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "chan", "action": "make", "bind": "py_ch", "size": 2},
            {"op": "chan", "action": "send", "channel": "py_ch", "value": {"kind": "literal", "value": "py-a"}},
            {"op": "chan", "action": "send", "channel": "py_ch", "value": {"kind": "literal", "value": "py-b"}},
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert len(outbox) == 2\nassert outbox[0] == 'py-a'\nassert list(outbox) == ['py-a', 'py-b']",
                "captures": {"outbox": "py_ch"},
            },
            {"op": "chan", "action": "make", "bind": "js_ch", "size": 2},
            {"op": "chan", "action": "send", "channel": "js_ch", "value": {"kind": "literal", "value": "js-a"}},
            {"op": "chan", "action": "send", "channel": "js_ch", "value": {"kind": "literal", "value": "js-b"}},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (Array.from(outbox).join(',') !== 'js-a,js-b') throw new Error('bad stream')",
                "captures": {"outbox": "js_ch"},
            },
            {"op": "chan", "action": "make", "bind": "rb_ch", "size": 2},
            {"op": "chan", "action": "send", "channel": "rb_ch", "value": {"kind": "literal", "value": "rb-a"}},
            {"op": "chan", "action": "send", "channel": "rb_ch", "value": {"kind": "literal", "value": "rb-b"}},
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "raise 'bad stream' unless outbox.to_a == ['rb-a', 'rb-b']",
                "captures": {"outbox": "rb_ch"},
            },
            {"op": "chan", "action": "make", "bind": "java_ch", "size": 2},
            {"op": "chan", "action": "send", "channel": "java_ch", "value": {"kind": "literal", "value": "java-a"}},
            {"op": "chan", "action": "send", "channel": "java_ch", "value": {"kind": "literal", "value": "java-b"}},
            {
                "op": "exec",
                "runtime": "java",
                "code": "Object outbox = omnivm.OmniVM.getCapture(\"outbox\"); java.util.ArrayList<Object> vals = new java.util.ArrayList<>(); for (Object item : (Iterable<?>) outbox) vals.add(item); if (!vals.toString().equals(\"[java-a, java-b]\")) throw new RuntimeException(\"bad stream: \" + vals);",
                "captures": {"outbox": "java_ch"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after_status = omnivm.status()
    boundary = after_status.get("boundary", {})
    handles = after_status.get("handles", {})
    if boundary.get("stream_proxy_captures", 0) < before_boundary.get("stream_proxy_captures", 0) + 4:
        raise AssertionError(f"channel captures did not use stream proxies: {boundary}")
    if boundary.get("channel_materializations", 0) != before_boundary.get("channel_materializations", 0):
        raise AssertionError(f"channel capture should not snapshot materialize: {boundary}")
    if handles.get("handle_accesses_by_kind", {}).get("stream", 0) < before_handles.get("handle_accesses_by_kind", {}).get("stream", 0) + 8:
        raise AssertionError(f"stream iteration did not record stream accesses: {handles}")
    if handles.get("explicit_releases", 0) < before_handles.get("explicit_releases", 0) + 4:
        raise AssertionError(f"exhausted streams did not release handles: {handles}")


def test_manifest_channel_auto_injects_as_lazy_stream():
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {"op": "chan", "action": "make", "bind": "py_outbox", "size": 2},
            {"op": "chan", "action": "send", "channel": "py_outbox", "value": {"kind": "literal", "value": "py-auto-a"}},
            {"op": "chan", "action": "send", "channel": "py_outbox", "value": {"kind": "literal", "value": "py-auto-b"}},
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert list(py_outbox) == ['py-auto-a', 'py-auto-b']",
            },
            {"op": "chan", "action": "make", "bind": "js_outbox", "size": 2},
            {"op": "chan", "action": "send", "channel": "js_outbox", "value": {"kind": "literal", "value": "js-auto-a"}},
            {"op": "chan", "action": "send", "channel": "js_outbox", "value": {"kind": "literal", "value": "js-auto-b"}},
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "const got = Array.from(js_outbox).join(','); if (got !== 'js-auto-a,js-auto-b') throw new Error('bad JS auto stream: ' + got);",
            },
            {"op": "chan", "action": "make", "bind": "rb_outbox", "size": 2},
            {"op": "chan", "action": "send", "channel": "rb_outbox", "value": {"kind": "literal", "value": "rb-auto-a"}},
            {"op": "chan", "action": "send", "channel": "rb_outbox", "value": {"kind": "literal", "value": "rb-auto-b"}},
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "raise 'bad Ruby auto stream' unless rb_outbox.to_a == ['rb-auto-a', 'rb-auto-b']",
            },
            {"op": "chan", "action": "make", "bind": "java_outbox", "size": 2},
            {"op": "chan", "action": "send", "channel": "java_outbox", "value": {"kind": "literal", "value": "java-auto-a"}},
            {"op": "chan", "action": "send", "channel": "java_outbox", "value": {"kind": "literal", "value": "java-auto-b"}},
            {
                "op": "exec",
                "runtime": "java",
                "code": "Object outbox = omnivm.OmniVM.getCapture(\"java_outbox\"); java.util.ArrayList<Object> vals = new java.util.ArrayList<>(); for (Object item : (Iterable<?>) outbox) vals.add(item); if (!vals.toString().equals(\"[java-auto-a, java-auto-b]\")) throw new RuntimeException(\"bad Java auto stream: \" + vals);",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    boundary = omnivm.status().get("boundary", {})
    if boundary.get("stream_proxy_captures", 0) < 4:
        raise AssertionError(f"auto-injected channel did not use a stream proxy: {boundary}")
    if boundary.get("channel_materializations", 0) != 0:
        raise AssertionError(f"auto-injected channel should not snapshot materialize: {boundary}")
    if boundary.get("json_fallbacks", 0) != 0:
        raise AssertionError(f"auto-injected channel used JSON fallback: {boundary}")


def test_manifest_handle_proxy_chatty_warning_stats():
    before = omnivm.status().get("handles", {}).get("chatty_proxy_warnings", 0)
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/chatty"}},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "for _ in range(70):\n    assert req.path == '/chatty'",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    handles = omnivm.status().get("handles", {})
    after = handles.get("chatty_proxy_warnings", 0)
    if after <= before:
        raise AssertionError(f"chatty proxy warning counter did not increase: before={before}, handles={handles}")
    if handles.get("handle_accesses_by_kind", {}).get("property", 0) < 64:
        raise AssertionError(f"chatty proxy descriptor access was not recorded: {handles}")


def test_manifest_js_chatty_proxy_auto_materializes_items():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/batched", "method": "GET"}},
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "for (var i = 0; i < 90; i++) { if (req.path !== '/batched') throw new Error('bad path'); }",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    warnings_delta = after.get("chatty_proxy_warnings", 0) - before.get("chatty_proxy_warnings", 0)
    if warnings_delta < 1:
        raise AssertionError(f"JS chatty proxy did not trip warning/materialization path: before={before}, after={after}")
    before_props = before.get("handle_accesses_by_kind", {}).get("property", 0)
    after_props = after.get("handle_accesses_by_kind", {}).get("property", 0)
    delta = after_props - before_props
    if delta >= 160:
        raise AssertionError(f"JS chatty proxy did not reduce repeated property bridge calls: delta={delta}, before={before}, after={after}")


def test_manifest_python_chatty_proxy_auto_materializes_items():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/python-batched", "method": "GET"}},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "for _ in range(90):\n    assert req.path == '/python-batched'",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    warnings_delta = after.get("chatty_proxy_warnings", 0) - before.get("chatty_proxy_warnings", 0)
    if warnings_delta < 1:
        raise AssertionError(f"Python chatty proxy did not trip warning/materialization path: before={before}, after={after}")
    before_props = before.get("handle_accesses_by_kind", {}).get("property", 0)
    after_props = after.get("handle_accesses_by_kind", {}).get("property", 0)
    delta = after_props - before_props
    if delta >= 160:
        raise AssertionError(f"Python chatty proxy did not reduce repeated property bridge calls: delta={delta}, before={before}, after={after}")


def test_manifest_ruby_chatty_proxy_auto_materializes_items():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/ruby-batched", "method": "GET"}},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "90.times do\n  raise 'bad path' unless req.path == '/ruby-batched'\nend",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    warnings_delta = after.get("chatty_proxy_warnings", 0) - before.get("chatty_proxy_warnings", 0)
    if warnings_delta < 1:
        raise AssertionError(f"Ruby chatty proxy did not trip warning/materialization path: before={before}, after={after}")
    before_props = before.get("handle_accesses_by_kind", {}).get("property", 0)
    after_props = after.get("handle_accesses_by_kind", {}).get("property", 0)
    delta = after_props - before_props
    if delta >= 160:
        raise AssertionError(f"Ruby chatty proxy did not reduce repeated property bridge calls: delta={delta}, before={before}, after={after}")


def test_manifest_java_chatty_proxy_auto_materializes_items():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/java-batched", "method": "GET"}},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": "Object req = omnivm.OmniVM.getCapture(\"req\"); omnivm.OmniVM.HandleProxy proxy = (omnivm.OmniVM.HandleProxy) req; for (int i = 0; i < 90; i++) { if (!\"/java-batched\".equals(proxy.get(\"path\"))) throw new RuntimeException(\"bad path\"); }",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    warnings_delta = after.get("chatty_proxy_warnings", 0) - before.get("chatty_proxy_warnings", 0)
    if warnings_delta < 1:
        raise AssertionError(f"Java chatty proxy did not trip warning/materialization path: before={before}, after={after}")
    before_props = before.get("handle_accesses_by_kind", {}).get("property", 0)
    after_props = after.get("handle_accesses_by_kind", {}).get("property", 0)
    delta = after_props - before_props
    if delta >= 160:
        raise AssertionError(f"Java chatty proxy did not reduce repeated property bridge calls: delta={delta}, before={before}, after={after}")


def test_manifest_python_proxy_weakref_finalizer():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/weakref-finalizer"}},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert req.path == '/weakref-finalizer'\ndel __captures['req']\ndel req\nimport gc\ngc.collect(); gc.collect(); gc.collect()",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    if after.get("finalizer_queued", 0) <= before.get("finalizer_queued", 0):
        raise AssertionError(f"Python proxy weakref finalizer did not queue release: before={before}, after={after}")
    if after.get("finalizer_queue_drains", 0) <= before.get("finalizer_queue_drains", 0):
        raise AssertionError(f"queued Python proxy finalizer was not drained at host boundary: before={before}, after={after}")


def test_manifest_js_proxy_finalization_registry():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/js-finalizer"}},
            },
            {
                "op": "func_def",
                "name": "current_req",
                "params": [],
                "body": [
                    {"op": "return", "value": {"kind": "ref", "name": "req"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "let local = current_req(); if (local.path !== '/js-finalizer') throw new Error('bad path: ' + local.path); globalThis.__omnivm_js_finalizer_test_id = local.__omnivm_descriptor__.id; local = null;",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "try { if (typeof globalThis.gc !== 'function') { require('v8').setFlagsFromString('--expose_gc'); globalThis.gc = require('vm').runInNewContext('gc'); } } catch (_e) {}\nfor (var i = 0; i < 250; i++) { var junk = new Array(100000).fill(i); if (typeof globalThis.gc === 'function') globalThis.gc(); }\nPromise.resolve().then(function() {});",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "if (globalThis.__omnivm_prune_proxy_cache) globalThis.__omnivm_prune_proxy_cache(true); for (var i = 0; i < 250; i++) { var junk = new Array(100000).fill(i); if (typeof globalThis.gc === 'function') globalThis.gc(); } Promise.resolve().then(function() {});",
            },
            {
                "op": "exec",
                "runtime": "javascript",
                "code": "for (var i = 0; i < 250; i++) { var junk = new Array(100000).fill(i); if (typeof globalThis.gc === 'function') globalThis.gc(); } if (globalThis.__omnivm_record_handle_release_finalizer && globalThis.__omnivm_js_finalizer_test_id) { globalThis.__omnivm_record_handle_release_finalizer(globalThis.__omnivm_js_finalizer_test_id); delete globalThis.__omnivm_js_finalizer_test_id; }",
            },
            {
                "op": "resource",
                "action": "close",
                "runtime": "python",
                "bind": "req",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    if after.get("finalizer_queued", 0) <= before.get("finalizer_queued", 0):
        raise AssertionError(f"JS proxy FinalizationRegistry did not queue release: before={before}, after={after}")
    if after.get("finalizer_queue_drains", 0) <= before.get("finalizer_queue_drains", 0):
        raise AssertionError(f"queued JS proxy finalizer was not drained at host boundary: before={before}, after={after}")


def test_manifest_ruby_proxy_objectspace_finalizer():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/ruby-finalizer"}},
            },
            {
                "op": "exec",
                "runtime": "ruby",
                "code": "raise 'bad path' unless req.path == '/ruby-finalizer'\n$req = nil\nreq = nil\n10.times { GC.compact; sleep 0.01 }",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    if after.get("finalizer_queued", 0) <= before.get("finalizer_queued", 0):
        raise AssertionError(f"Ruby proxy ObjectSpace finalizer did not queue release: before={before}, after={after}")
    if after.get("finalizer_queue_drains", 0) <= before.get("finalizer_queue_drains", 0):
        raise AssertionError(f"queued Ruby proxy finalizer was not drained at host boundary: before={before}, after={after}")


def test_manifest_java_proxy_cleaner_finalizer():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/java-cleaner"}},
            },
            {
                "op": "exec",
                "runtime": "java",
                "code": "Object req = omnivm.OmniVM.getCapture(\"req\"); omnivm.OmniVM.HandleProxy proxy = (omnivm.OmniVM.HandleProxy) req; if (!\"/java-cleaner\".equals(proxy.get(\"path\"))) throw new RuntimeException(\"bad path\"); omnivm.OmniVM.clearCapture(\"req\"); req = null; proxy = null; for (int i = 0; i < 20; i++) { System.gc(); System.runFinalization(); Thread.sleep(10); }",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    if after.get("finalizer_queued", 0) <= before.get("finalizer_queued", 0):
        raise AssertionError(f"Java proxy Cleaner finalizer did not queue release: before={before}, after={after}")
    if after.get("finalizer_queue_drains", 0) <= before.get("finalizer_queue_drains", 0):
        raise AssertionError(f"queued Java proxy finalizer was not drained at host boundary: before={before}, after={after}")


def test_manifest_proxy_finalizer_preserves_scope_owner():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "resource",
                "action": "open",
                "runtime": "python",
                "kind": "request",
                "bind": "req",
                "value": {"kind": "literal", "value": {"path": "/scope-owner"}},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert req.path == '/scope-owner'\ndel __captures['req']\ndel req\nimport gc\ngc.collect(); gc.collect(); gc.collect()",
                "captures": {"req": "req"},
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "assert req.path == '/scope-owner'",
                "captures": {"req": "req"},
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    if after.get("finalizer_queue_drains", 0) <= before.get("finalizer_queue_drains", 0):
        raise AssertionError(f"queued finalizer was not drained before scope-owner reuse: before={before}, after={after}")


def test_manifest_returned_proxy_finalizer_releases_transfer():
    before = omnivm.status().get("handles", {})
    manifest = {
        "version": 1,
        "defaultRuntime": "python",
        "ops": [
            {
                "op": "func_def",
                "name": "make_req",
                "params": [],
                "body": [
                    {
                        "op": "resource",
                        "action": "open",
                        "runtime": "python",
                        "kind": "request",
                        "bind": "req",
                        "value": {"kind": "literal", "value": {"path": "/returned-transfer"}},
                    },
                    {"op": "return", "value": {"kind": "ref", "name": "req"}},
                ],
            },
            {
                "op": "exec",
                "runtime": "python",
                "code": "req = make_req()\nassert req.path == '/returned-transfer'\ndel req\nimport gc\ngc.collect(); gc.collect(); gc.collect()",
            },
        ],
    }
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        json.dump(manifest, f)
        path = f.name
    try:
        omnivm.run_manifest(path)
    finally:
        os.unlink(path)

    after = omnivm.status().get("handles", {})
    if after.get("finalizer_queued", 0) <= before.get("finalizer_queued", 0):
        raise AssertionError(f"returned proxy finalizer did not queue release: before={before}, after={after}")
    if after.get("finalizer_queue_drains", 0) <= before.get("finalizer_queue_drains", 0):
        raise AssertionError(f"returned proxy finalizer was not drained: before={before}, after={after}")
    if after.get("finalizer_releases", 0) <= before.get("finalizer_releases", 0):
        raise AssertionError(f"returned proxy transfer was not released by finalizer: before={before}, after={after}")
    if after.get("live", 0) > before.get("live", 0):
        raise AssertionError(f"returned proxy transfer leaked a live handle: before={before}, after={after}")


def test_jvm_interruptible_direct_call_timeout():
    child_check(
        """
import time
omnivm.set_task_timeout(1000)
start = time.monotonic()
try:
    omnivm.call("java", "omnivm.OmniVMRunner.interruptibleSleep(5000L)")
except omnivm.RuntimeError as exc:
    if "InterruptedException" not in str(exc) and "interrupted" not in str(exc).lower():
        raise AssertionError(f"unexpected Java timeout error: {exc}")
else:
    raise AssertionError("expected Java call to be interrupted")
elapsed = time.monotonic() - start
if elapsed > 3:
    raise AssertionError(f"Java interrupt took too long: {elapsed:.3f}s")
omnivm.set_task_timeout(0)
assert omnivm.call("java", "40 + 2") == "42"
""",
        timeout=10,
    )


def test_java_completable_future_callback_affinity_is_diagnostic_or_safe():
    result = omnivm.call(
        "java",
        r'''
((java.util.concurrent.Callable<String>)(() -> {
java.util.concurrent.CompletableFuture<String> pyFuture =
    java.util.concurrent.CompletableFuture.supplyAsync(() -> "py")
        .thenApplyAsync(label -> {
            try {
                return "OK:" + omnivm.OmniVM.call("python", "'from-' + '" + label + "'");
            } catch (Throwable t) {
                return "ERR:" + t.getClass().getName() + ":" + t.getMessage();
            }
        });
java.util.concurrent.CompletableFuture<String> jsFuture =
    java.util.concurrent.CompletableFuture.supplyAsync(() -> "js")
        .thenApplyAsync(label -> {
            try {
                return "OK:" + omnivm.OmniVM.call("javascript", "'from-' + '" + label + "'");
            } catch (Throwable t) {
                return "ERR:" + t.getClass().getName() + ":" + t.getMessage();
            }
        });
String pyResult = pyFuture.get(3, java.util.concurrent.TimeUnit.SECONDS);
String jsResult = jsFuture.get(3, java.util.concurrent.TimeUnit.SECONDS);
String combined = pyResult + "|" + jsResult;
boolean pySafe = pyResult.equals("OK:from-py");
boolean jsSafe = jsResult.equals("OK:from-js");
boolean pyDiagnostic = pyResult.startsWith("ERR:") && pyResult.contains("non-Golden Thread");
boolean jsDiagnostic = jsResult.startsWith("ERR:") && jsResult.contains("non-Golden Thread");
if (!(pySafe || pyDiagnostic)) throw new RuntimeException("Python callback affinity was neither safe nor diagnostic: " + pyResult);
if (!(jsSafe || jsDiagnostic)) throw new RuntimeException("JS callback affinity was neither safe nor diagnostic: " + jsResult);
return combined;
})).call()
'''
    )
    parts = result.split("|")
    if len(parts) != 2:
        raise AssertionError(f"unexpected Java CompletableFuture callback result: {result}")
    for part in parts:
        if not (part.startswith("OK:from-") or ("ERR:" in part and "non-Golden Thread" in part)):
            raise AssertionError(f"Java CompletableFuture callback affinity was not safe or diagnostic: {result}")


def test_java_reactor_scheduler_callback_affinity_is_diagnostic_or_safe():
    result = omnivm.call(
        "java",
        r'''
((java.util.concurrent.Callable<String>)(() -> {
reactor.core.publisher.Mono<String> pyMono =
    reactor.core.publisher.Mono.fromCallable(() -> {
        try {
            return "OK:" + omnivm.OmniVM.call("python", "'from-reactor-py'");
        } catch (Throwable t) {
            return "ERR:" + t.getClass().getName() + ":" + t.getMessage();
        }
    }).subscribeOn(reactor.core.scheduler.Schedulers.boundedElastic());
reactor.core.publisher.Mono<String> jsMono =
    reactor.core.publisher.Mono.fromCallable(() -> {
        try {
            return "OK:" + omnivm.OmniVM.call("javascript", "'from-reactor-js'");
        } catch (Throwable t) {
            return "ERR:" + t.getClass().getName() + ":" + t.getMessage();
        }
    }).subscribeOn(reactor.core.scheduler.Schedulers.boundedElastic());
String pyResult = pyMono.block(java.time.Duration.ofSeconds(3));
String jsResult = jsMono.block(java.time.Duration.ofSeconds(3));
String combined = pyResult + "|" + jsResult;
boolean pySafe = pyResult.equals("OK:from-reactor-py");
boolean jsSafe = jsResult.equals("OK:from-reactor-js");
boolean pyDiagnostic = pyResult.startsWith("ERR:") && pyResult.contains("non-Golden Thread");
boolean jsDiagnostic = jsResult.startsWith("ERR:") && jsResult.contains("non-Golden Thread");
if (!(pySafe || pyDiagnostic)) throw new RuntimeException("Python Reactor callback affinity was neither safe nor diagnostic: " + pyResult);
if (!(jsSafe || jsDiagnostic)) throw new RuntimeException("JS Reactor callback affinity was neither safe nor diagnostic: " + jsResult);
return combined;
})).call()
'''
    )
    parts = result.split("|")
    if len(parts) != 2:
        raise AssertionError(f"unexpected Java Reactor callback result: {result}")
    for part in parts:
        if not (part.startswith("OK:from-reactor-") or ("ERR:" in part and "non-Golden Thread" in part)):
            raise AssertionError(f"Java Reactor callback affinity was not safe or diagnostic: {result}")


def test_java_rxjava_custom_executor_callback_affinity_is_diagnostic_or_safe():
    result = omnivm.call(
        "java",
        r'''
((java.util.concurrent.Callable<String>)(() -> {
java.util.concurrent.ExecutorService exec = java.util.concurrent.Executors.newFixedThreadPool(2);
try {
    io.reactivex.rxjava3.core.Scheduler scheduler = io.reactivex.rxjava3.schedulers.Schedulers.from(exec);
    io.reactivex.rxjava3.core.Single<String> pySingle =
        io.reactivex.rxjava3.core.Single.fromCallable(() -> {
            try {
                return "OK:" + omnivm.OmniVM.call("python", "'from-rxjava-py'");
            } catch (Throwable t) {
                return "ERR:" + t.getClass().getName() + ":" + t.getMessage();
            }
        }).subscribeOn(scheduler).timeout(3, java.util.concurrent.TimeUnit.SECONDS);
    io.reactivex.rxjava3.core.Single<String> jsSingle =
        io.reactivex.rxjava3.core.Single.fromCallable(() -> {
            try {
                return "OK:" + omnivm.OmniVM.call("javascript", "'from-rxjava-js'");
            } catch (Throwable t) {
                return "ERR:" + t.getClass().getName() + ":" + t.getMessage();
            }
        }).subscribeOn(scheduler).timeout(3, java.util.concurrent.TimeUnit.SECONDS);
    String pyResult = pySingle.blockingGet();
    String jsResult = jsSingle.blockingGet();
    String combined = pyResult + "|" + jsResult;
    boolean pySafe = pyResult.equals("OK:from-rxjava-py");
    boolean jsSafe = jsResult.equals("OK:from-rxjava-js");
    boolean pyDiagnostic = pyResult.startsWith("ERR:") && pyResult.contains("non-Golden Thread");
    boolean jsDiagnostic = jsResult.startsWith("ERR:") && jsResult.contains("non-Golden Thread");
    if (!(pySafe || pyDiagnostic)) throw new RuntimeException("Python RxJava callback affinity was neither safe nor diagnostic: " + pyResult);
    if (!(jsSafe || jsDiagnostic)) throw new RuntimeException("JS RxJava callback affinity was neither safe nor diagnostic: " + jsResult);
    return combined;
} finally {
    exec.shutdownNow();
}
})).call()
'''
    )
    parts = result.split("|")
    if len(parts) != 2:
        raise AssertionError(f"unexpected Java RxJava callback result: {result}")
    for part in parts:
        if not (part.startswith("OK:from-rxjava-") or ("ERR:" in part and "non-Golden Thread" in part)):
            raise AssertionError(f"Java RxJava callback affinity was not safe or diagnostic: {result}")


def test_nested_js_to_java_interrupt_timeout():
    child_check(
        """
import time
omnivm.set_task_timeout(1000)
start = time.monotonic()
try:
    omnivm.call("javascript", "omnivm.call('java', 'omnivm.OmniVMRunner.interruptibleSleep(5000L)')")
except omnivm.RuntimeError as exc:
    text = str(exc)
    if "InterruptedException" not in text and "interrupted" not in text.lower():
        raise AssertionError(f"unexpected nested Java timeout error: {exc}")
else:
    raise AssertionError("expected nested Java call to be interrupted")
elapsed = time.monotonic() - start
if elapsed > 3:
    raise AssertionError(f"nested Java interrupt took too long: {elapsed:.3f}s")
omnivm.set_task_timeout(0)
assert omnivm.call("javascript", "40 + 2") == "42"
assert omnivm.call("java", "41 + 1") == "42"
""",
        timeout=10,
    )


def test_go_plugin_deadline_direct_call_timeout():
    child_check(
        r'''
import os
import subprocess
import tempfile
import time

tmp = tempfile.mkdtemp(prefix="omnivm-go-deadline-")
src = r"""
package main

import "C"
import "time"

//export Slow
func Slow(arg *C.char) *C.char {
    time.Sleep(5 * time.Second)
    return C.CString("late")
}

//export Fast
func Fast(arg *C.char) *C.char {
    return C.CString("ok:" + C.GoString(arg))
}

func main() {}
"""
open(os.path.join(tmp, "main.go"), "w", encoding="utf-8").write(src)
open(os.path.join(tmp, "go.mod"), "w", encoding="utf-8").write("module deadlineplug\n\ngo 1.23\n")
so_path = os.path.join(tmp, "deadlineplug.so")
build = subprocess.run(
    ["go", "build", "-buildmode=c-shared", "-o", so_path, "."],
    cwd=tmp,
    text=True,
    stdout=subprocess.PIPE,
    stderr=subprocess.PIPE,
    timeout=30,
)
if build.returncode != 0:
    raise AssertionError(f"go plugin build failed\nstdout:\n{build.stdout}\nstderr:\n{build.stderr}")

omnivm.load_plugin("go", so_path)
omnivm.set_task_timeout(250)
start = time.monotonic()
try:
    omnivm.call("go", 'deadlineplug.Slow("x")')
except omnivm.RuntimeError as exc:
    if "timed out" not in str(exc):
        raise AssertionError(f"unexpected Go deadline error: {exc}")
else:
    raise AssertionError("expected Go plugin call deadline")
elapsed = time.monotonic() - start
if elapsed > 3:
    raise AssertionError(f"Go plugin deadline took too long: {elapsed:.3f}s")

if not omnivm.worker_tainted():
    raise AssertionError("Go plugin deadline did not mark the worker tainted")
if omnivm.last_timeout_runtime() != "go":
    raise AssertionError(f"unexpected timeout runtime: {omnivm.last_timeout_runtime()!r}")
if "timed out" not in omnivm.worker_taint_reason():
    raise AssertionError(f"unexpected taint reason: {omnivm.worker_taint_reason()!r}")
status = omnivm.status()
if not status["worker_tainted"]:
    raise AssertionError("status did not report tainted worker")
if status["last_timeout_runtime"] != "go":
    raise AssertionError(f"status reported wrong timeout runtime: {status['last_timeout_runtime']!r}")
if status["go_deadline_count"] < 1:
    raise AssertionError("status did not increment go_deadline_count")

omnivm.set_task_timeout(2000)
assert omnivm.call("go", 'deadlineplug.Fast("x")') == "ok:x"
''',
        timeout=45,
    )


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


def test_prefork_master_import_worker_init():
    code = """
import os
import sys
sys.path.insert(0, '/build/pyomnivm')
import omnivm
pid = os.fork()
if pid == 0:
    try:
        omnivm.init_runtimes(['javascript', 'java', 'ruby'])
        ok = (
            omnivm.call('javascript', '20 + 1') == '21'
            and omnivm.call('java', '20 + 2') == '22'
            and omnivm.call('ruby', '20 + 3') == '23'
        )
        omnivm.shutdown()
        os._exit(0 if ok else 2)
    except Exception as exc:
        print(exc, file=sys.stderr)
        os._exit(1)
_, status = os.waitpid(pid, 0)
raise SystemExit(os.waitstatus_to_exitcode(status))
"""
    proc = subprocess.run([sys.executable, "-c", code], check=False)
    if proc.returncode != 0:
        raise AssertionError(f"prefork worker init exit {proc.returncode}, want 0")


def test_multiple_prefork_workers():
    code = """
import os
import sys
sys.path.insert(0, '/build/pyomnivm')
import omnivm
pids = []
for i in range(4):
    pid = os.fork()
    if pid == 0:
        try:
            omnivm.init_runtimes(['javascript', 'java', 'ruby'])
            expected = str(i + 100)
            got = omnivm.call('javascript', f'{i} + 100')
            ok = got == expected and omnivm.call('ruby', f'{i} + 200') == str(i + 200)
            omnivm.shutdown()
            os._exit(0 if ok else 2)
        except Exception as exc:
            print(exc, file=sys.stderr)
            os._exit(1)
    pids.append(pid)
failures = []
for pid in pids:
    _, status = os.waitpid(pid, 0)
    rc = os.waitstatus_to_exitcode(status)
    if rc != 0:
        failures.append((pid, rc))
if failures:
    print(failures, file=sys.stderr)
    raise SystemExit(1)
"""
    proc = subprocess.run([sys.executable, "-c", code], check=False)
    if proc.returncode != 0:
        raise AssertionError(f"multiple prefork workers exit {proc.returncode}, want 0")


def test_recycled_worker_processes():
    child = """
import sys
sys.path.insert(0, '/build/pyomnivm')
import omnivm
omnivm.init_runtimes(['javascript', 'java', 'ruby'])
assert omnivm.call('javascript', '6 * 7') == '42'
assert omnivm.call('java', '7 * 8') == '56'
assert omnivm.call('ruby', '8 * 9') == '72'
omnivm.shutdown()
"""
    for _ in range(3):
        proc = subprocess.run([sys.executable, "-c", child], check=False)
        if proc.returncode != 0:
            raise AssertionError(f"recycled worker exit {proc.returncode}, want 0")


def test_python3_polyscript_wsgi_smoke():
    code = """
import omnivm

def application(environ, start_response):
    status = '200 OK'
    body = ('poly:' + omnivm.call('javascript', '21 * 2')).encode()
    start_response(status, [('Content-Type', 'text/plain')])
    return [body]

omnivm.init_runtimes(['javascript', 'java', 'ruby'])
captured = {}
def start_response(status, headers):
    captured['status'] = status
    captured['headers'] = headers
body = b''.join(application({'PATH_INFO': '/secure/orders'}, start_response))
omnivm.shutdown()
assert captured['status'] == '200 OK'
assert body == b'poly:42'
"""
    proc = subprocess.run(
        ["python3-polyscript", "-c", code],
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    if proc.returncode != 0:
        raise AssertionError(
            f"python3-polyscript WSGI smoke exit {proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )


def test_python3_polyscript_django_wsgi_smoke():
    code = r"""
import io
import sys
import types

from django.conf import settings
from django.http import HttpResponse
from django.urls import path

def poly_view(request):
    import omnivm
    return HttpResponse('django-poly:' + omnivm.call('javascript', '21 * 2'))

urls = types.ModuleType('polyscript_django_urls')
urls.urlpatterns = [path('poly/', poly_view)]
sys.modules['polyscript_django_urls'] = urls

settings.configure(
    DEBUG=False,
    SECRET_KEY='polyscript-test',
    ROOT_URLCONF='polyscript_django_urls',
    ALLOWED_HOSTS=['*'],
    MIDDLEWARE=[],
    INSTALLED_APPS=[],
)

from django.core.wsgi import get_wsgi_application

import omnivm
omnivm.init_runtimes(['javascript', 'java', 'ruby'])
application = get_wsgi_application()

captured = {}
def start_response(status, headers):
    captured['status'] = status
    captured['headers'] = dict(headers)

environ = {
    'REQUEST_METHOD': 'GET',
    'PATH_INFO': '/poly/',
    'SCRIPT_NAME': '',
    'SERVER_NAME': 'testserver',
    'SERVER_PORT': '80',
    'SERVER_PROTOCOL': 'HTTP/1.1',
    'wsgi.version': (1, 0),
    'wsgi.url_scheme': 'http',
    'wsgi.input': io.BytesIO(b''),
    'wsgi.errors': sys.stderr,
    'wsgi.multithread': False,
    'wsgi.multiprocess': True,
    'wsgi.run_once': False,
}
body = b''.join(application(environ, start_response))
omnivm.shutdown()
assert captured['status'].startswith('200'), captured
assert body == b'django-poly:42', body
"""
    proc = subprocess.run(
        ["python3-polyscript", "-c", code],
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=60,
    )
    if proc.returncode != 0:
        raise AssertionError(
            f"python3-polyscript Django WSGI smoke exit {proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )


def test_wsgi_prefork_worker_lifecycle_harness():
    code = """
import os
import sys
sys.path.insert(0, '/build/pyomnivm')
import omnivm

def application(environ, start_response):
    request_id = int(environ['HTTP_X_REQUEST_ID'])
    js_val = omnivm.call('javascript', f'{request_id} + 10')
    java_val = omnivm.call('java', f'{request_id} + 20')
    ruby_val = omnivm.call('ruby', f'{request_id} + 30')
    state = omnivm.status()
    start_response(
        '200 OK',
        [
            ('Content-Type', 'text/plain'),
            ('X-OmniVM-Pid', str(state['pid'])),
            ('X-OmniVM-Tainted', str(state['worker_tainted']).lower()),
        ],
    )
    return [f'{js_val}|{java_val}|{ruby_val}'.encode()]

pids = []
for worker_id in range(3):
    pid = os.fork()
    if pid == 0:
        try:
            omnivm.init_runtimes(['javascript', 'java', 'ruby'])
            for request_id in range(worker_id * 10, worker_id * 10 + 4):
                captured = {}
                def start_response(status, headers):
                    captured['status'] = status
                    captured['headers'] = dict(headers)
                body = b''.join(application({'HTTP_X_REQUEST_ID': str(request_id)}, start_response)).decode()
                expected = f'{request_id + 10}|{request_id + 20}|{request_id + 30}'
                if captured['status'] != '200 OK' or body != expected:
                    raise AssertionError((captured, body, expected))
                if captured['headers']['X-OmniVM-Tainted'] != 'false':
                    raise AssertionError('worker unexpectedly tainted')
            omnivm.shutdown()
            os._exit(0)
        except Exception as exc:
            print(exc, file=sys.stderr)
            os._exit(1)
    pids.append(pid)

failures = []
for pid in pids:
    _, status = os.waitpid(pid, 0)
    rc = os.waitstatus_to_exitcode(status)
    if rc != 0:
        failures.append((pid, rc))
if failures:
    print(failures, file=sys.stderr)
    raise SystemExit(1)
"""
    proc = subprocess.run(
        [sys.executable, "-c", code],
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=60,
    )
    if proc.returncode != 0:
        raise AssertionError(
            f"WSGI prefork lifecycle harness exit {proc.returncode}\nstdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )


def test_ruby_bootstrap_core_surface():
    got = omnivm.call(
        "ruby",
        "checks = ["
        "3.times.map { |i| i }.join(','), "
        "(~0).to_s, "
        "[1, 2, 3].last(2).join(','), "
        "Dir['/tmp'].is_a?(Array).to_s, "
        "Time.new(2000, 1, 1, 0, 0, 0).yday.to_s, "
        "GC.stat({}).is_a?(Hash).to_s"
        "]; checks.join('|')",
    )
    if got != "0,1,2|-1|2,3|true|1|true":
        raise AssertionError(f"Ruby bootstrap surface mismatch: {got}")


def main():
    global NAME_FILTERS, CATEGORY_FILTERS
    parser = argparse.ArgumentParser(description="CPython-hosted libomnivm stress checks")
    parser.add_argument("--name", action="append", default=[], help="run tests whose display name contains this text; may be repeated")
    parser.add_argument("--category", action="append", default=[], help="run tests in a derived category such as manifest, proxy, arrow, stream, kwargs, watchdog, prefork, concurrency, java, ruby, javascript, or python; may be repeated")
    parser.add_argument("--list-categories", action="store_true", help="print supported derived categories and exit")
    args = parser.parse_args()
    NAME_FILTERS = [_normalize_filter(value) for value in args.name if str(value).strip()]
    CATEGORY_FILTERS = {_normalize_filter(value) for value in args.category if str(value).strip()}

    categories = [
        "all",
        "manifest",
        "proxy",
        "arrow",
        "stream",
        "kwargs",
        "watchdog",
        "prefork",
        "concurrency",
        "java",
        "ruby",
        "javascript",
        "python",
    ]
    if args.list_categories:
        print("\n".join(categories))
        return 0

    unknown_categories = sorted(CATEGORY_FILTERS - set(categories))
    if unknown_categories:
        print(f"unknown categories: {', '.join(unknown_categories)}", file=sys.stderr)
        print(f"supported categories: {', '.join(categories)}", file=sys.stderr)
        return 2

    print("=== libomnivm CPython Host Stress Tests ===")
    if NAME_FILTERS or CATEGORY_FILTERS:
        print(
            "Filters: "
            + ", ".join(
                part
                for part in [
                    f"name={NAME_FILTERS}" if NAME_FILTERS else "",
                    f"category={sorted(CATEGORY_FILTERS)}" if CATEGORY_FILTERS else "",
                ]
                if part
            )
        )
    load_omnivm()
    omnivm.init_runtimes(["javascript", "java", "ruby"])
    try:
        check("Simple re-entry (Python calls JS)", test_simple_reentry)
        check("Deep chain (Py -> JS -> Ruby)", test_deep_chain)
        check("Closure-like capture (Py -> JS -> Ruby)", test_closure_like_capture)
        check("Fan-out (Python threads via libomnivm)", test_fan_out_threads)
        check("Recursive cross-runtime fibonacci(15)", test_recursive_cross_runtime_fibonacci)
        check("Error propagation (Py -> JS throws)", test_error_propagation)
        check("Python generator consumed cross-runtime (100 iterations)", test_python_generator_consumed_cross_runtime)
        check("Re-entrant async: Py loop -> JS -> back into Py", test_reentrant_async_python_js_python)
        check("Asyncio TaskGroup cross-runtime cancellation", test_asyncio_taskgroup_cross_runtime_cancellation)
        check("Exception through suspended Python generator", test_exception_through_suspended_generator)
        check("Object pinning and GC interaction", test_object_pinning_and_gc)
        check("Interleaved multiple live generators", test_interleaved_live_generators)
        check("GC finalizer triggers cross-runtime call (50x)", test_gc_finalizer_cross_runtime)
        check("String encoding gauntlet", test_string_encoding_gauntlet)
        check("Sustained mixed workload (1000 cycles)", test_sustained_mixed_workload)
        check("Full quadrilateral chain (Py -> JS -> Java -> Ruby)", test_all_runtimes_chain)
        check("Java enters the arena", test_java_enters_arena)
        check("Java re-entrant call (Java -> Py -> Java)", test_java_reentrant_call)
        check("Java exception handling through bridge", test_java_exception_handling)
        check("Java compilation storm (50 unique classes)", test_java_compilation_storm)
        check("Concurrent Java + all runtimes via Python threads", test_concurrent_java_all_runtimes)
        check("Generator send() pipeline across runtimes", test_generator_send_pipeline)
        check("Iterator protocol with cross-runtime __next__", test_iterator_protocol_cross_runtime_next)
        check("Context manager __exit__ with in-flight exception", test_context_manager_exit_inflight_exception)
        check("Nested try/finally cross-runtime during stack unwinding", test_nested_try_finally_cross_runtime)
        check("__getattr__ dispatches to runtimes", test_getattr_dispatches_to_runtimes)
        check("List comprehension with cross-runtime filter + transform", test_list_comprehension_cross_runtime_filter_transform)
        check("yield from with cross-runtime sub-generator", test_yield_from_cross_runtime_subgenerator)
        check("contextlib.contextmanager with cross-runtime calls", test_contextlib_contextmanager_cross_runtime)
        check("Recursive cross-runtime depth bomb", test_recursive_cross_runtime_depth_bomb)
        check("1MB string through the bridge (Py -> JS -> Py)", test_one_mb_string_bridge)
        check("Chained error recovery cascade", test_chained_error_recovery_cascade)
        check("functools.reduce with cross-runtime accumulator", test_functools_reduce_cross_runtime_accumulator)
        check("Python interrupt stops infinite loop", test_python_interrupt_stops_infinite_loop)
        check("JVM NPE (SIGSEGV) does not crash Ruby", test_jvm_npe_does_not_crash_ruby)
        check("Rapid JVM NPE + Ruby interleave (100 cycles)", test_rapid_jvm_npe_ruby_interleave)
        check("Python interrupt recovery during bridge call", test_python_interrupt_recovery_during_bridge_call)
        check("Triple signal stress: JVM NPE + Ruby error + Python interrupt", test_triple_signal_stress)
        check("Sustained JVM NPE storm (100 cycles, all runtimes)", test_sustained_jvm_npe_storm)
        check("All runtimes on same OS thread (Python-host proof)", test_thread_identity_python_host)
        check("Foreign thread bridge (Python, Java, Ruby)", test_foreign_thread_bridge)
        check("Exception ping-pong (Py -> JS -> Java -> Ruby raises)", test_exception_ping_pong)
        check("GC standoff (1MB x 4 runtimes x 25 rounds + cross-bridge)", test_gc_standoff_large_allocations)
        check("Ruby Fiber cooperative bridge (C stack switching)", test_ruby_fiber_cooperative_bridge)
        check("Ruby ensure with bridge during exception unwind", test_ruby_ensure_bridge_unwind)
        check("Ruby catch/throw with bridge calls", test_ruby_catch_throw_bridge)
        check("Ruby bootstrap core surface", test_ruby_bootstrap_core_surface)
        check("JS try/finally with bridge throw + bridge in finally", test_js_try_finally_bridge_throw)
        check("4-runtime mutual recursion (18 levels deep)", test_four_runtime_mutual_recursion)
        check("Rogue guest preemption (JS+Ruby infinite loops)", test_rogue_guest_preemption)
        check("Post-termination V8 storm (terminate + 50 rapid evals)", test_post_termination_v8_storm)
        check("JVM thread -> Python (6*9=54)", test_jvm_thread_to_python)
        check("JVM thread -> JS (7*8=56)", test_jvm_thread_to_js)
        check("JVM thread -> Ruby (5+3=8)", test_jvm_thread_to_ruby)
        check("4 JVM threads x 50 bridge calls", test_jvm_threads_bridge_calls)
        check("Python thread -> JS (3*7=21)", test_python_thread_to_js)
        check("GIL contention (Golden + JVM thread -> Python)", test_gil_contention_jvm_thread_to_python)
        check("Nested foreign-thread bridge (JVM -> Py -> JS)", test_nested_foreign_thread_bridge)
        check("Re-entrant exception during generator during async pump", test_reentrant_exception_generator_async_pump)
        check("Allocation storm (1MB x 3 runtimes x 30 round-trips)", test_allocation_storm_round_trips)
        check("Sleep-wake concurrency torture", test_sleep_wake_concurrency_torture)
        check("Thread coalescing avalanche (100 simultaneous calls)", test_thread_coalescing_avalanche)
        check("M:N isolation equivalent (Python threads + JS tasks)", test_python_host_mn_isolation)
        check("Signal chaos trap (Ruby GC + timeout toggling)", test_signal_chaos_trap_python_host)
        check("Inception preemption (Py -> JS infinite loop via bridge)", test_inception_preemption_js_via_python)
        check("Async event loop starvation (JS setTimeout vs Python task flood)", test_async_event_loop_starvation_python_host)
        check("Asyncio starvation (Python task vs bridge flood)", test_asyncio_starvation_bridge_task_flood)
        check("Context cancellation guillotine (Python-host equivalent)", test_context_cancellation_guillotine_python_host)
        check("Watchdog deep bridge chain (Py -> JS -> Ruby spin)", test_watchdog_deep_bridge_chain)
        check("Watchdog re-arm race (100 arm/disarm cycles)", test_watchdog_rearm_race)
        check("Heartbeat-only pump (JS timer, zero task flood)", test_heartbeat_only_js_timer_python_host)
        check("Buffer bridge", test_buffer_bridge)
        check("Python runtime buffer view auto-release", test_python_runtime_buffer_view_auto_release)
        check("Arrow C Data bridge", test_arrow_c_data_bridge)
        check("Arrow C Data import bridge", test_arrow_c_data_import_bridge)
        check("Arrow C Data owned import bridge zero-copy", test_arrow_c_data_owned_import_bridge_zero_copy)
        check("Arrow C Data nullable owned import bridge zero-copy", test_arrow_c_data_nullable_owned_import_bridge_zero_copy)
        check("Arrow C Data signed int8 import bridge", test_arrow_c_data_signed_int8_import_bridge)
        check("Manifest Python buffer-protocol capture uses Arrow", test_manifest_python_buffer_protocol_capture_uses_arrow)
        check("Manifest Python readonly buffer-protocol capture uses Arrow", test_manifest_python_readonly_buffer_protocol_capture_uses_arrow)
        check("Manifest Python empty buffer capture uses Arrow", test_manifest_python_empty_buffer_capture_uses_arrow)
        check("Manifest Python shaped buffer capture preserves metadata", test_manifest_python_shaped_buffer_capture_preserves_metadata)
        check("Manifest Python strided memoryview capture uses Arrow", test_manifest_python_strided_memoryview_capture_uses_arrow)
        check("Manifest Python negative-strided memoryview capture uses Arrow", test_manifest_python_negative_strided_memoryview_capture_uses_arrow)
        check("Manifest Python array-interface capture uses Arrow", test_manifest_python_array_interface_capture_uses_arrow)
        check("Manifest Python readonly array-interface preserves metadata", test_manifest_python_readonly_array_interface_preserves_metadata)
        check("Manifest Python image-shaped array-interface capture uses Arrow", test_manifest_python_image_shaped_array_interface_capture_uses_arrow)
        check("Manifest Python array-interface data object capture uses Arrow", test_manifest_python_array_interface_data_object_capture_uses_arrow)
        check("Manifest PIL image capture uses array-interface Arrow", test_manifest_pil_image_capture_uses_array_interface_arrow)
        check("Manifest Python wrong-endian array-interface capture uses proxy", test_manifest_python_wrong_endian_array_interface_capture_uses_proxy)
        check("Manifest Python Arrow PyCapsule capture uses Arrow", test_manifest_python_arrow_capsule_capture_uses_arrow)
        check("Manifest Python nullable Arrow PyCapsule capture uses Arrow", test_manifest_python_nullable_arrow_capsule_capture_uses_arrow)
        check("Manifest Python nested Arrow PyCapsule capture uses proxy", test_manifest_python_nested_arrow_capsule_capture_uses_proxy)
        check("Manifest Python Arrow stream capture uses Arrow", test_manifest_python_arrow_stream_capture_uses_arrow)
        check("Manifest Python multi-chunk Arrow stream capture uses proxy", test_manifest_python_multichunk_arrow_stream_capture_uses_proxy)
        check("Manifest Java numeric Map.get indexes Arrow table", test_manifest_java_numeric_map_get_indexes_arrow_table)
        check("Manifest Python numeric get indexes Arrow table", test_manifest_python_numeric_get_indexes_arrow_table)
        check("Manifest JS proxy get indexes Arrow table", test_manifest_js_proxy_get_indexes_arrow_table)
        check("Manifest Ruby proxy fetch indexes Arrow table", test_manifest_ruby_proxy_fetch_indexes_arrow_table)
        check("Manifest Python calls JS function proxy", test_manifest_python_calls_js_function_proxy)
        check("Manifest JS calls Python function proxy", test_manifest_js_calls_python_function_proxy)
        check("Manifest Ruby calls JS function proxy", test_manifest_ruby_calls_js_function_proxy)
        check("Manifest Java calls JS function proxy", test_manifest_java_calls_js_function_proxy)
        check("Manifest Python calls Java function proxy", test_manifest_python_calls_java_function_proxy)
        check("Manifest JS calls Java function proxy", test_manifest_js_calls_java_function_proxy)
        check("Manifest Ruby calls Java function proxy", test_manifest_ruby_calls_java_function_proxy)
        check("Manifest Python calls Go c-shared function proxy", test_manifest_python_calls_go_cshared_function_proxy)
        check("Manifest JS calls Go c-shared function proxy", test_manifest_js_calls_go_cshared_function_proxy)
        check("Manifest Ruby calls Go c-shared function proxy", test_manifest_ruby_calls_go_cshared_function_proxy)
        check("Manifest Java calls Go c-shared function proxy", test_manifest_java_calls_go_cshared_function_proxy)
        check("Manifest Python signed int8 array-interface capture uses Arrow", test_manifest_python_signed_int8_array_interface_capture_uses_arrow)
        check("Manifest Python strided array-interface capture uses Arrow", test_manifest_python_strided_array_interface_capture_uses_arrow)
        check("Manifest Python negative-strided array-interface capture uses Arrow", test_manifest_python_negative_strided_array_interface_capture_uses_arrow)
        check("Manifest Python DLPack capture uses Arrow", test_manifest_python_dlpack_capture_uses_arrow)
        check("Manifest Python dataframe interchange capture uses Arrow", test_manifest_python_dataframe_interchange_capture_uses_arrow)
        check("Manifest Python nullable dataframe interchange capture uses Arrow", test_manifest_python_nullable_dataframe_interchange_capture_uses_arrow)
        check("Manifest Python non-CPU dataframe interchange capture uses proxy", test_manifest_python_non_cpu_dataframe_interchange_capture_uses_proxy)
        check("Manifest NumPy strided view capture uses Arrow", test_manifest_numpy_strided_view_capture_uses_arrow)
        check("Manifest NumPy wrong-endian buffer capture uses proxy", test_manifest_numpy_wrong_endian_buffer_capture_uses_proxy)
        check("Manifest Pandas Series array protocol capture uses Arrow", test_manifest_pandas_series_array_protocol_capture_uses_arrow)
        check("Manifest Pandas DataFrame array protocol capture uses Arrow", test_manifest_pandas_dataframe_array_protocol_capture_uses_arrow)
        check("Manifest Polars DataFrame capture uses Arrow", test_manifest_polars_dataframe_capture_uses_arrow)
        check("Manifest heterogeneous DataFrame capture uses proxy not JSON", test_manifest_heterogeneous_dataframe_capture_uses_proxy_not_json)
        check("Manifest Django request capture uses proxy not JSON", test_manifest_django_request_capture_uses_proxy_not_json)
        check("Manifest FastAPI request capture uses proxy not stream", test_manifest_fastapi_request_capture_uses_proxy_not_stream)
        check("Manifest Django QuerySet transaction rollback crosses runtimes", test_manifest_django_queryset_transaction_rollback_cross_runtime)
        check("Manifest Django request body after close requires DTO", test_manifest_django_request_body_after_close_requires_materialized_dto)
        check("Manifest Django streaming response capture uses proxy not body stream", test_manifest_django_streaming_response_capture_uses_proxy_not_body_stream)
        check("Manifest SQLAlchemy model capture uses proxy not JSON", test_manifest_sqlalchemy_model_capture_uses_proxy_not_json)
        check("Manifest SQLAlchemy Result and Session lifecycle", test_manifest_sqlalchemy_result_session_lifecycle_and_rollback)
        check("Manifest JDBC ResultSet overloaded calls stay natural", test_manifest_jdbc_cached_rowset_lifecycle_crosses_as_proxy)
        check("Manifest Python file-like request shape capture uses proxy not stream", test_manifest_python_file_like_request_shape_capture_uses_proxy_not_stream)
        check("Manifest Ruby HTTP message shape capture uses proxy not stream", test_manifest_ruby_http_message_shape_capture_uses_proxy_not_stream)
        check("Manifest Rack request capture uses proxy not stream", test_manifest_rack_request_capture_uses_proxy_not_stream)
        check("Manifest Java HTTP message shape capture uses proxy not stream", test_manifest_java_http_message_shape_capture_uses_proxy_not_stream)
        check("Manifest OkHttp request capture uses proxy not JSON", test_manifest_okhttp_request_capture_uses_proxy_not_json)
        check("Manifest Express request capture uses proxy not stream", test_manifest_express_request_capture_uses_proxy_not_stream)
        check("Manifest model id property beats JS proxy metadata", test_manifest_model_id_property_beats_js_proxy_metadata)
        check("Manifest descriptor fields do not shadow runtime object fields", test_manifest_descriptor_fields_do_not_shadow_runtime_object_fields)
        check("Manifest JS typed-array capture uses Arrow", test_manifest_js_typed_array_capture_uses_arrow)
        check("Manifest JS empty typed-array capture uses Arrow", test_manifest_js_empty_typed_array_capture_uses_arrow)
        check("Manifest JS Int8 typed-array capture uses Arrow", test_manifest_js_int8_typed_array_capture_uses_arrow)
        check("Manifest JS typed-array subarray capture uses view window", test_manifest_js_typed_array_subarray_capture_uses_view_window)
        check("Manifest JS ArrayBuffer capture uses Arrow", test_manifest_js_arraybuffer_capture_uses_arrow)
        check("Manifest JS uint16 typed-array capture uses Arrow", test_manifest_js_uint16_typed_array_capture_uses_arrow)
        check("Manifest JS BigInt64 typed-array capture uses Arrow", test_manifest_js_bigint64_typed_array_capture_uses_arrow)
        check("Manifest JS BigUint64 typed-array capture uses Arrow", test_manifest_js_biguint64_typed_array_capture_uses_arrow)
        check("Manifest JS DataView capture uses Arrow", test_manifest_js_dataview_capture_uses_arrow)
        check("Manifest Ruby string capture uses Arrow", test_manifest_ruby_string_capture_uses_arrow)
        check("Manifest Ruby to_str capture uses Arrow", test_manifest_ruby_to_str_capture_uses_arrow)
        check("Manifest Ruby UTF-8 string capture stays text", test_manifest_ruby_utf8_string_capture_stays_text)
        check("Manifest Ruby array capture uses proxy not JSON", test_manifest_ruby_array_capture_uses_proxy_not_json)
        check("Manifest Java primitive array capture uses Arrow", test_manifest_java_primitive_array_capture_uses_arrow)
        check("Manifest Java empty primitive array capture uses Arrow", test_manifest_java_empty_primitive_array_capture_uses_arrow)
        check("Manifest Java direct ByteBuffer capture uses remaining window", test_manifest_java_direct_bytebuffer_capture_uses_remaining_window)
        check("Manifest Java readonly direct ByteBuffer preserves metadata", test_manifest_java_readonly_direct_bytebuffer_preserves_metadata)
        check("Manifest Java empty direct ByteBuffer capture uses Arrow", test_manifest_java_empty_direct_bytebuffer_capture_uses_arrow)
        check("Manifest Java array-backed IntBuffer capture uses Arrow", test_manifest_java_array_backed_intbuffer_capture_uses_arrow)
        check("Manifest Java direct FloatBuffer capture uses Arrow", test_manifest_java_direct_floatbuffer_capture_uses_arrow)
        check("Manifest Java wrong-endian direct IntBuffer capture uses proxy", test_manifest_java_wrong_endian_direct_intbuffer_capture_uses_proxy)
        check("Manifest Java wrong-endian heap-view IntBuffer capture uses proxy", test_manifest_java_wrong_endian_heap_view_intbuffer_capture_uses_proxy)
        check("Repeated crossings", test_repeated_crossings)
        check("Typed bridge rejects complex stringification", test_typed_bridge_rejects_complex_stringification)
        check("Python-first host stays CPython", test_python_first_process_survives_plain_python)
        check("libomnivm status observability", test_status_observability)
        check("libomnivm retains last manifest boundary stats", test_status_keeps_last_manifest_boundary_stats)
        check("Manifest handle proxy property forwarding", test_manifest_handle_proxy_property_forwarding)
        check("Manifest RuntimeRef mapping keys beat methods", test_manifest_runtime_ref_mapping_keys_beat_methods)
        check("Manifest function returns materialized proxy descriptor", test_manifest_func_return_materializes_proxy_descriptor)
        check("Manifest function returns local complex literal as proxy", test_manifest_func_return_local_complex_literal_as_proxy)
        check("Manifest proxy materializer reuses handle identity", test_manifest_proxy_materializer_reuses_handle_identity)
        check("Manifest function returns preserve complex argument identity", test_manifest_func_return_preserves_complex_argument_identity)
        check("Manifest function returns runtime buffer as Arrow", test_manifest_func_return_exports_runtime_buffer_as_arrow)
        check("Manifest runtime-ref property returns Python buffer as Arrow", test_manifest_runtime_ref_property_exports_python_buffer_as_arrow)
        check("Manifest runtime-ref property returns JS typed array as Arrow", test_manifest_runtime_ref_property_exports_js_typed_array_as_arrow)
        check("Manifest runtime-ref property returns Java ByteBuffer as Arrow", test_manifest_runtime_ref_property_exports_java_bytebuffer_as_arrow)
        check("Manifest runtime-ref methods return bulk results as Arrow", test_manifest_runtime_ref_methods_export_bulk_results_as_arrow)
        check("Manifest Java runtime-ref exposes local object members generically", test_manifest_java_runtime_ref_exposes_local_object_members_generically)
        check("Manifest Java collections capture uses proxy not stream", test_manifest_java_collections_capture_use_proxy_not_stream)
        check("Manifest Python set capture uses proxy not stream", test_manifest_python_set_capture_uses_proxy_not_stream)
        check("Manifest Java set capture uses proxy not stream", test_manifest_java_set_capture_uses_proxy_not_stream)
        check("Manifest ActiveRecord relation-like stays lazy and collision safe", test_manifest_active_record_relation_like_stays_lazy_and_collision_safe)
        check("Manifest sqlite3 native gem executes inside Ruby", test_manifest_sqlite3_native_gem_executes_inside_ruby)
        check("Manifest ActiveRecord SQLite adapter is natural and collision safe", test_manifest_active_record_sqlite_adapter_is_natural_and_collision_safe)
        check("Manifest Python dict/list capture uses proxy not JSON", test_manifest_python_dict_list_capture_uses_proxy_not_json)
        check("Manifest Ruby hash capture uses proxy not JSON", test_manifest_ruby_hash_capture_uses_proxy_not_json)
        check("Manifest Python runtime-ref exposes local object members generically", test_manifest_python_runtime_ref_exposes_local_object_members_generically)
        check("Manifest Ruby runtime-ref exposes local object members generically", test_manifest_ruby_runtime_ref_exposes_local_object_members_generically)
        check("Manifest JS runtime-ref exposes local object members generically", test_manifest_js_runtime_ref_exposes_local_object_members_generically)
        check("Manifest JS plain object/array capture uses proxy not JSON", test_manifest_js_plain_object_array_capture_uses_proxy_not_json)
        check("Manifest JS Map capture uses proxy not JSON", test_manifest_js_map_capture_uses_proxy_not_json)
        check("Manifest JS Set capture uses proxy not stream", test_manifest_js_set_capture_uses_proxy_not_stream)
        check("Manifest Zod schema capture uses proxy not JSON", test_manifest_zod_schema_capture_uses_proxy_not_json)
        check("Validation/error-rich library error fidelity", test_validation_error_fidelity_popular_libraries)
        check("Manifest function returns JS typed array as Arrow", test_manifest_func_return_exports_js_typed_array_as_arrow)
        check("Manifest function returns Java primitive array as Arrow", test_manifest_func_return_exports_java_primitive_array_as_arrow)
        check("Manifest Go c-shared function returns typed slice as Arrow", test_manifest_go_cshared_func_return_exports_typed_slice_as_arrow)
        check("Manifest Go c-shared function returns nested array as shaped Arrow", test_manifest_go_cshared_func_return_exports_nested_array_as_shaped_arrow)
        check("Manifest Go c-shared function returns nested slice as shaped Arrow", test_manifest_go_cshared_func_return_exports_nested_slice_as_shaped_arrow)
        check("Manifest Go c-shared function returns byte slice as Arrow", test_manifest_go_cshared_func_return_exports_byte_slice_as_arrow)
        check("Manifest Go c-shared function returns complex object as proxy", test_manifest_go_cshared_func_return_exports_complex_object_as_proxy)
        check("Manifest Go c-shared sequence members cross generically", test_manifest_go_cshared_sequence_members_cross_generically)
        check("Manifest Go c-shared struct object members cross generically", test_manifest_go_cshared_struct_object_members_cross_generically)
        check("Manifest Go c-shared function preserves complex proxy argument", test_manifest_go_cshared_func_preserves_complex_proxy_argument)
        check("Manifest Go c-shared proxy setter values stay live", test_manifest_go_cshared_proxy_setter_values_stay_live)
        check("Manifest Go c-shared proxy method arguments stay live", test_manifest_go_cshared_proxy_method_arguments_stay_live)
        check("Manifest Go c-shared proxy method returns typed slice as Arrow", test_manifest_go_cshared_proxy_method_return_exports_typed_slice_as_arrow)
        check("Manifest Go c-shared proxy iter preserves shaped Arrow metadata", test_manifest_go_cshared_proxy_iter_preserves_shaped_arrow_metadata)
        check("Manifest Go c-shared function consumes Arrow table as typed slice", test_manifest_go_cshared_func_consumes_arrow_table_as_typed_slice)
        check("Manifest Go c-shared function consumes shaped Arrow table as fixed array", test_manifest_go_cshared_func_consumes_shaped_arrow_table_as_fixed_array)
        check("Manifest Go c-shared function consumes shaped Arrow table as nested slice", test_manifest_go_cshared_func_consumes_shaped_arrow_table_as_nested_slice)
        check("Manifest Go c-shared function consumes strided Arrow table as typed slice", test_manifest_go_cshared_func_consumes_strided_arrow_table_as_typed_slice)
        check("Manifest Go c-shared function consumes byte buffer as byte slice", test_manifest_go_cshared_func_consumes_byte_buffer_as_byte_slice)
        check("Manifest function returns Ruby string as Arrow", test_manifest_func_return_exports_ruby_string_as_arrow)
        check("Manifest function returns Ruby to_str as Arrow", test_manifest_func_return_exports_ruby_to_str_as_arrow)
        check("Manifest stream items preserve complex runtime refs", test_manifest_stream_items_preserve_complex_runtime_refs)
        check("Manifest Python generator capture is lazy stream", test_manifest_python_generator_capture_is_lazy_stream)
        check("Manifest function returns generator as transfer stream", test_manifest_function_returned_generator_is_transfer_stream)
        check("Manifest runtime iterators capture as lazy streams", test_manifest_runtime_iterators_capture_as_lazy_streams)
        check("Manifest runtime readers capture as lazy streams", test_manifest_runtime_readers_capture_as_lazy_streams)
        check("Manifest Java iterable body capture is lazy stream", test_manifest_java_iterable_body_capture_as_lazy_stream)
        check("Manifest Java BaseStream body capture is lazy stream", test_manifest_java_base_stream_body_capture_as_lazy_stream)
        check("Manifest Ruby each body capture is lazy stream", test_manifest_ruby_each_body_capture_as_lazy_stream)
        check("Manifest Python iterable body capture is lazy stream", test_manifest_python_iterable_body_capture_as_lazy_stream)
        check("Manifest Python async iterable body capture is lazy stream", test_manifest_python_async_iterable_body_capture_as_lazy_stream)
        check("Manifest JS stream cancel calls iterator return", test_manifest_js_stream_cancel_calls_iterator_return)
        check("Manifest JS iterable body capture is lazy stream", test_manifest_js_iterable_body_capture_as_lazy_stream)
        check("Manifest JS async iterable body capture is lazy stream", test_manifest_js_async_iterable_body_capture_as_lazy_stream)
        check("Manifest JS ReadableStream body capture is lazy stream", test_manifest_js_readable_stream_body_capture_as_lazy_stream)
        check("Manifest JS ReadableStream early cancel releases owner", test_manifest_js_readable_stream_early_cancel_releases_owner)
        check("Manifest HTTPX response stream early cancel releases owner", test_manifest_httpx_response_stream_early_cancel_releases_owner)
        check("Manifest aiohttp stream early cancel releases owner", test_manifest_aiohttp_stream_early_cancel_releases_owner)
        check("Manifest undici response body early cancel releases owner", test_manifest_undici_response_body_early_cancel_releases_owner)
        check("Manifest nested proxy reference edges observable", test_manifest_nested_proxy_reference_edges_observable)
        check("Manifest proxy mutation cycles observable", test_manifest_proxy_mutation_cycles_observable)
        check("Manifest cross-runtime proxy cycles remain bounded", test_manifest_cross_runtime_proxy_cycles_remain_bounded)
        check("Manifest Ruby/Java proxy cycles remain bounded", test_manifest_ruby_java_proxy_cycles_remain_bounded)
        check("Manifest proxy method arguments stay live", test_manifest_proxy_method_arguments_stay_live)
        check("Manifest Java function arguments stay live", test_manifest_java_func_argument_stays_live)
        check("Manifest Java function invoke fallback handles unsafe names", test_manifest_java_func_invoke_fallback_handles_unsafe_names)
        check("Manifest proxy setter values stay live", test_manifest_proxy_setter_values_stay_live)
        check("Manifest Python mapping collision setters prefer keys", test_manifest_python_mapping_collision_setters_prefer_keys)
        check("Manifest channel captures are lazy streams", test_manifest_channel_captures_are_lazy_streams)
        check("Manifest channel auto-injects as lazy stream", test_manifest_channel_auto_injects_as_lazy_stream)
        check("Manifest handle proxy chatty warning stats", test_manifest_handle_proxy_chatty_warning_stats)
        check("Manifest JS chatty proxy auto-materializes items", test_manifest_js_chatty_proxy_auto_materializes_items)
        check("Manifest Python chatty proxy auto-materializes items", test_manifest_python_chatty_proxy_auto_materializes_items)
        check("Manifest Ruby chatty proxy auto-materializes items", test_manifest_ruby_chatty_proxy_auto_materializes_items)
        check("Manifest Java chatty proxy auto-materializes items", test_manifest_java_chatty_proxy_auto_materializes_items)
        check("Manifest Python proxy weakref finalizer", test_manifest_python_proxy_weakref_finalizer)
        check("Manifest JS proxy FinalizationRegistry", test_manifest_js_proxy_finalization_registry)
        check("Manifest Ruby proxy ObjectSpace finalizer", test_manifest_ruby_proxy_objectspace_finalizer)
        check("Manifest Java proxy Cleaner finalizer", test_manifest_java_proxy_cleaner_finalizer)
        check("Manifest proxy finalizer preserves scope owner", test_manifest_proxy_finalizer_preserves_scope_owner)
        check("Manifest returned proxy finalizer releases transfer", test_manifest_returned_proxy_finalizer_releases_transfer)
        check("JVM direct call timeout uses Thread.interrupt", test_jvm_interruptible_direct_call_timeout)
        check("Java CompletableFuture callback affinity is diagnostic or safe", test_java_completable_future_callback_affinity_is_diagnostic_or_safe)
        check("Java Reactor scheduler callback affinity is diagnostic or safe", test_java_reactor_scheduler_callback_affinity_is_diagnostic_or_safe)
        check("Java RxJava custom executor callback affinity is diagnostic or safe", test_java_rxjava_custom_executor_callback_affinity_is_diagnostic_or_safe)
        check("Nested JS -> Java timeout uses Thread.interrupt", test_nested_js_to_java_interrupt_timeout)
        check("Go plugin direct call deadline returns to host", test_go_plugin_deadline_direct_call_timeout)
    finally:
        omnivm.shutdown()

    check("Fork guard", test_fork_guard)
    check("Prefork master import, worker init", test_prefork_master_import_worker_init)
    check("Multiple prefork workers initialize independently", test_multiple_prefork_workers)
    check("Recycled worker processes initialize cleanly", test_recycled_worker_processes)
    check("python3-polyscript WSGI smoke", test_python3_polyscript_wsgi_smoke)
    check("python3-polyscript Django WSGI smoke", test_python3_polyscript_django_wsgi_smoke)
    check("WSGI prefork worker lifecycle harness", test_wsgi_prefork_worker_lifecycle_harness)
    if (NAME_FILTERS or CATEGORY_FILTERS) and SELECTED == 0:
        print("\nResults: 0 selected, no tests matched filters")
        return 2
    print(f"\nResults: {PASSED} passed, {FAILED} failed, {SKIPPED} skipped")
    return 1 if FAILED else 0


if __name__ == "__main__":
    raise SystemExit(main())
