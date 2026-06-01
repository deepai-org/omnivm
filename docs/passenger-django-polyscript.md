# Passenger/Django PolyScript Migration

This path is for gradually moving an existing Django app from Python files to `.poly` files while keeping Passenger Standalone as the process manager.

Passenger should keep launching ordinary CPython workers. Use `python3-polyscript` as the Python interpreter for the worker process, or make the Passenger Python command resolve to that wrapper. The wrapper is CPython with the `omnivm` module registered before interpreter startup; it is not a Go-hosted replacement for Passenger's process model.

The important rule is fork order:

1. Passenger's master process stays plain Python and does not initialize OmniVM.
2. Each worker imports `passenger_wsgi.py` after fork.
3. Worker code imports `.py` modules normally, and progressively imports `.poly` modules through PolyScript as they are added.
4. OmniVM runtimes initialize only inside the worker, after fork, either lazily at first polyglot boundary or explicitly in worker-local setup.

That keeps Passenger's embedded Nginx, Unix-domain socket plumbing, worker pool sizing, request recycling, ALB draining, and queue-overflow behavior unchanged.

## WSGI shape

```python
import os

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "mysite.settings")

from django.core.wsgi import get_wsgi_application

django_application = get_wsgi_application()
_omnivm_ready = False

def _ensure_omnivm():
    global _omnivm_ready
    if _omnivm_ready:
        return
    import omnivm
    omnivm.init_runtimes(["javascript", "java", "ruby"])
    _omnivm_ready = True

def application(environ, start_response):
    _ensure_omnivm()
    return django_application(environ, start_response)
```

Projects that only import `.poly` modules and do not call `omnivm` directly can keep this even thinner; the goal is still the same: no OmniVM initialization in Passenger's master process.

## Operational notes

- `force_max_concurrent_requests_per_process: 1` maps cleanly to the current WSGI model. Runtime state is worker-local.
- `max_requests` recycling is still useful. It bounds memory growth from Django, native libraries, and embedded runtimes in the same way it bounds regular Python extension state.
- SIGTERM and ALB draining stay Passenger-owned. Workers should let the normal process exit reclaim embedded runtime state; OmniVM does not require user-visible shutdown hooks.
- The Hypercorn/ASGI sidecar can use the same principle: run it with `python3-polyscript -m hypercorn ...`, and initialize OmniVM only inside the serving process.

## Coverage

`make test-libomnivm-stress` includes:

- master-import/worker-init prefork checks
- multiple independent prefork workers
- recycled worker initialization
- `python3-polyscript` raw WSGI smoke
- `python3-polyscript` Django `get_wsgi_application()` smoke
- a prefork WSGI worker lifecycle harness

`make test-poly-libomnivm-smoke` compiles selected sibling Garbage `.poly` examples and executes the generated manifests through CPython-hosted `libomnivm`.
