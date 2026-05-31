#!/usr/bin/env python3
"""CPython-hosted libomnivm stress checks.

This is intentionally separate from cmd/stresstest, which boots OmniVM as a Go
process. These checks keep CPython as process host, load libomnivm through
ctypes, and exercise nested runtime callbacks from that topology.
"""

import os
import gc
import struct
import subprocess
import sys
import threading
import time

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


def main():
    print("=== libomnivm CPython Host Stress Tests ===")
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
        check("Repeated crossings", test_repeated_crossings)
        check("Python-first host stays CPython", test_python_first_process_survives_plain_python)
        check("JVM direct call timeout uses Thread.interrupt", test_jvm_interruptible_direct_call_timeout)
        check("Go plugin direct call deadline returns to host", test_go_plugin_deadline_direct_call_timeout)
    finally:
        omnivm.shutdown()

    check("Fork guard", test_fork_guard)
    check("Prefork master import, worker init", test_prefork_master_import_worker_init)
    check("Multiple prefork workers initialize independently", test_multiple_prefork_workers)
    check("Recycled worker processes initialize cleanly", test_recycled_worker_processes)
    check("python3-polyscript WSGI smoke", test_python3_polyscript_wsgi_smoke)
    print(f"\nResults: {PASSED} passed, {FAILED} failed")
    return 1 if FAILED else 0


if __name__ == "__main__":
    raise SystemExit(main())
