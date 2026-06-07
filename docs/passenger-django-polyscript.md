# Passenger/Django PolyScript Migration

This path is for gradually moving an existing Django app from Python files to `.poly` files while keeping Passenger Standalone as the process manager.

Passenger should keep launching ordinary CPython workers. Use `python3-polyscript` as the Python interpreter for the worker process, or make the Passenger Python command resolve to that wrapper. The wrapper is CPython with the `omnivm` module registered before interpreter startup and the `.poly` import hook installed; it is not a Go-hosted replacement for Passenger's process model.

The important rule is fork order:

1. Passenger's master process stays plain Python and does not initialize OmniVM.
2. Each worker imports `passenger_wsgi.py` after fork.
3. Worker code imports `.py` modules normally, and progressively imports `.poly` modules as they are added.
4. The first `.poly` import compiles the file, initializes `libomnivm` inside the worker, and executes the generated manifest in-process.

That keeps Passenger's embedded Nginx, Unix-domain socket plumbing, worker pool sizing, request recycling, ALB draining, and queue-overflow behavior unchanged.

## WSGI shape

```python
import mysite.wsgi

application = mysite.wsgi.application
```

That is intentionally the same shape as a normal Passenger WSGI shim. The project `wsgi.py` can stay standard:

```python
import os

from django.core.wsgi import get_wsgi_application

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "mysite.settings.production")
application = get_wsgi_application()
```

Django modules can then import `.poly` files normally:

```python
import fraud_scoring
from billing_rules import rank_user
```

No app-level `omnivm.init_runtimes()` hook is required for imported `.poly`
modules in the single-request-per-process WSGI shape shown below. The runtime
initialization is lazy and worker-local: the first `.poly` import initializes
`libomnivm`, and that importing thread becomes the c-shared host thread for
direct OmniVM calls. Threaded WSGI/ASGI deployments should either ensure the
same worker thread performs later `.poly` calls, or fail fast during startup
with `omnivm.assert_host_thread()` / `omnivm.owner_dispatch_status()` before
registering request handlers that would need owner-loop dispatch. Top-level
manifest functions are exposed as Python callables, so `from ... import ...`
works for progressively converted service/helper modules.

## Passengerfile shape

```json
{
  "python": "python3-polyscript",
  "app_type": "wsgi",
  "startup_file": "passenger_wsgi.py",
  "environment": "production",
  "max_pool_size": 20,
  "min_instances": 4,
  "pool_idle_time": 300,
  "port": 77,
  "nginx_config_template": "nginx.conf.erb",
  "daemonize": true,
  "disable_security_update_check": true,
  "max_requests": 500,
  "force_max_concurrent_requests_per_process": 1,
  "max_request_queue_size": 100
}
```

Set `POLYSCRIPT_COMPILER` to the installed PolyScript compiler command and `POLYSCRIPT_CACHE_DIR` to a worker-writable directory. By default, `.poly` imports initialize JavaScript, Java, and Ruby so later imports do not depend on first-import ordering. Set `POLYSCRIPT_RUNTIMES=infer` only when a deployment wants lean first-import initialization and can guarantee all imported `.poly` modules use the same runtime set.

## Operational notes

- `force_max_concurrent_requests_per_process: 1` maps cleanly to the current WSGI model. Runtime state is worker-local, and OmniVM calls stay on the thread that first initialized `libomnivm`.
- `max_requests` recycling is still useful. It bounds memory growth from Django, native libraries, and embedded runtimes in the same way it bounds regular Python extension state.
- SIGTERM and ALB draining stay Passenger-owned. Workers can usually let process
  exit reclaim embedded runtime state; call `omnivm.drain_worker_hook(server,
  worker)` from an explicit worker-drain hook when the server keeps the process
  alive after draining requests. `omnivm.install_worker_drain_hook()` adds an
  idempotent `atexit` fallback for worker process exits and no-ops in workers
  that never initialized OmniVM.
- The Hypercorn/ASGI sidecar can use the same principle: run it with `python3-polyscript -m hypercorn ...`. Imported `.poly` modules initialize OmniVM only inside the serving process.

## Coverage

`make test-libomnivm-stress` includes:

- master-import/worker-init prefork checks
- multiple independent prefork workers
- recycled worker initialization
- `python3-polyscript` raw WSGI smoke
- `python3-polyscript` Django `get_wsgi_application()` smoke
- a prefork WSGI worker lifecycle harness

`make test-poly-libomnivm-smoke` compiles selected in-repo PolyScript examples, executes the generated manifests through CPython-hosted `libomnivm`, and runs the checked-in `test/fixtures/passenger-django-polyscript` fixture. The fixture keeps the Django stack realistic enough to catch migration issues: a middleware attaches request/session-style state, a class-based view delegates to a service object that imports the `.poly` feature module, the `.poly` code reads `request.headers` and `request.META`, and a nested dict/list response crosses back through Django's WSGI handler.
