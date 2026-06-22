"""gevent cooperative-boundary proof.

Same idea as proof_asyncio.py, but under gevent.monkey.patch_all(). Greenlets all
share one OS thread (the Golden Thread), so thread affinity holds. A raw blocking
manifest run freezes the hub (ticks ~= 0). The default `omnivm.run_manifest`
auto-detects the gevent monkeypatch and cooperates — fully invisible to the
caller (a normal synchronous call) — so the hub keeps scheduling other greenlets
(ticks >> 0) while every guest eval stays on the Golden Thread.

Usage: proof_gevent.py <manifest.json> <blocking|cooperative>
Emits: RESULT mode=<m> ticks=<n> call_ms=<d> max_gap_ms=<g>
"""
import sys
import time

from gevent import monkey

monkey.patch_all()

import gevent  # noqa: E402

sys.path.insert(0, "/build/pyomnivm")
import omnivm  # noqa: E402

MANIFEST = sys.argv[1]
MODE = sys.argv[2] if len(sys.argv) > 2 else "cooperative"

TICK_INTERVAL = 0.005


def main():
    omnivm.init_runtimes(["javascript"])
    state = {"ticks": 0, "stop": False, "last": None, "max_gap": 0.0}

    def ticker():
        state["last"] = time.monotonic()
        while not state["stop"]:
            now = time.monotonic()
            gap = now - state["last"]
            if gap > state["max_gap"]:
                state["max_gap"] = gap
            state["last"] = now
            state["ticks"] += 1
            gevent.sleep(TICK_INTERVAL)

    g = gevent.spawn(ticker)
    gevent.sleep(0.03)  # let the ticker spin up before the call
    before = state["ticks"]
    state["max_gap"] = 0.0
    t0 = time.monotonic()

    if MODE == "blocking":
        # Raw synchronous manifest run: blocks the gevent hub for its full duration.
        omnivm.run_manifest_blocking(MANIFEST)
    else:
        # Default entry: auto-detects gevent and cooperates, invisibly.
        omnivm.run_manifest(MANIFEST)

    call_ms = (time.monotonic() - t0) * 1000.0
    during = state["ticks"] - before
    max_gap_ms = state["max_gap"] * 1000.0
    state["stop"] = True
    g.join()
    omnivm.shutdown()
    print(
        f"[gevent:{MODE}] call={call_ms:.0f}ms ticks_during_call={during} "
        f"max_gap={max_gap_ms:.0f}ms"
    )
    print(f"RESULT mode={MODE} ticks={during} call_ms={call_ms:.0f} max_gap_ms={max_gap_ms:.0f}")


if __name__ == "__main__":
    main()
