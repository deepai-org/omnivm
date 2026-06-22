"""asyncio cooperative-boundary proof.

Runs a multi-op OmniVM manifest from inside a real asyncio event loop while a
concurrent ticker coroutine counts how many times it gets scheduled *during* the
call. A blocking call freezes the loop (ticks ~= 0). The cooperative driver
(`omnivm.run_manifest_async`) yields between manifest ops so the loop keeps
running (ticks >> 0) — while every guest eval still runs on the Golden Thread.

Usage: proof_asyncio.py <manifest.json> <blocking|cooperative>
Emits: RESULT mode=<m> ticks=<n> call_ms=<d> max_gap_ms=<g>
"""
import asyncio
import sys
import time

sys.path.insert(0, "/build/pyomnivm")
import omnivm

MANIFEST = sys.argv[1]
MODE = sys.argv[2] if len(sys.argv) > 2 else "cooperative"

TICK_INTERVAL = 0.005


async def run():
    state = {"ticks": 0, "stop": False, "last": None, "max_gap": 0.0}

    async def ticker():
        state["last"] = time.monotonic()
        while not state["stop"]:
            now = time.monotonic()
            gap = now - state["last"]
            if gap > state["max_gap"]:
                state["max_gap"] = gap
            state["last"] = now
            state["ticks"] += 1
            await asyncio.sleep(TICK_INTERVAL)

    t = asyncio.create_task(ticker())
    await asyncio.sleep(0.03)  # let the ticker spin up before the call
    before = state["ticks"]
    state["max_gap"] = 0.0
    t0 = time.monotonic()

    if MODE == "blocking":
        # Raw synchronous manifest run: blocks the event loop for its full duration.
        omnivm.run_manifest_blocking(MANIFEST)
    else:
        # Cooperative driver: an awaitable that yields to the loop between ops.
        await omnivm.run_manifest_async(MANIFEST)

    call_ms = (time.monotonic() - t0) * 1000.0
    during = state["ticks"] - before
    max_gap_ms = state["max_gap"] * 1000.0
    state["stop"] = True
    await t
    return during, call_ms, max_gap_ms


def main():
    omnivm.init_runtimes(["javascript"])
    try:
        during, call_ms, max_gap_ms = asyncio.run(run())
    finally:
        omnivm.shutdown()
    print(
        f"[asyncio:{MODE}] call={call_ms:.0f}ms ticks_during_call={during} "
        f"max_gap={max_gap_ms:.0f}ms"
    )
    print(f"RESULT mode={MODE} ticks={during} call_ms={call_ms:.0f} max_gap_ms={max_gap_ms:.0f}")


if __name__ == "__main__":
    main()
