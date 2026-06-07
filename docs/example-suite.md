# Example Suite Coverage

The example suite is meant to prove real ecosystem shapes, not just toy arithmetic. The current manifest examples exercise these runtimes under both `manifest-runner` and CPython-hosted `libomnivm`.

## Current milestone

The CPython-hosted `libomnivm` path now runs the checked-in manifest examples,
the selected in-repo PolyScript examples, and the Passenger/Django import
fixture with Python as the parent process. That coverage includes Go selector
constants such as `http.StatusAccepted`, Go `main()` entrypoints compiled from
`.go` examples, imported `.poly` functions returning nested proxy descriptors,
and boundary counters that do not count cached primitive movement as JSON
fallback. The intended invariant is that ordinary `.poly` code does not need
manual JSON encode/decode glue for runtime boundaries.

| Example | Main coverage |
| --- | --- |
| `python-fastapi-sqlalchemy-polars-docs.json` | Python framework, ORM, and dataframe-style shapes |
| `javascript-react-jsx-docs.json` | JavaScript package usage and React/JSX output shape |
| `javascript-jsx-factory-docs.poly` | JavaScript JSX lowered through a non-React local factory/fragment pair |
| `java-jackson-reactor-docs.json` | Java object mapping and reactive-style value flow |
| `ruby-activerecord-docs.json` | Ruby ORM-style class and record shapes |
| `go-http-handler-docs.json` | Go `http.HandlerFunc`-style callable shape |
| `java-manifest-function-proxy.json` | Java manifest stubs calling Python manifest functions with live object identity and unsafe-name fallback |
| `vertical-order-review-app.poly` | Source-level Django/FastAPI/Pydantic intake, Express/Zod routing, React rendering, Java service enrichment, Ruby ActiveRecord/Fiber normalization, Go worker fan-out, and checked runtime output golden |

Broader application fixtures cover Django/Zod/Go HMAC, Express/Pandas/Go workers, Java Gson/Pandas/Zod/Express, Java OkHttp/HTTPX/Go retry, Jinja2/Marked/Go docs, Python/JavaScript/Ruby/Java collision-heavy map fields, Pandas/Polars table proxy access, Pydantic/Zod validation error details, and other cross-runtime package combinations.

## Boundary contracts

The suite expects the automatic boundary model:

- primitives cross by value
- complex objects cross as live proxies/handles
- tabular and contiguous typed data prefer the Arrow/shared-buffer path
- streams and iterables stay lazy instead of being serialized eagerly
- JSON is application data, not the default glue between runtimes

Those contracts are also covered by the edge fixtures for resource/job handles, table bridges, stream proxies, request-like objects, function proxies, finalizers, chatty proxy materialization, and Java/Ruby/Python/JS/Go object member access.

## Useful commands

```bash
make test-all
make test-manifests
make test-libomnivm-manifests
make test-libomnivm-stress
make test-poly-libomnivm-smoke
```

`make test-all` is the canonical local and CI gate. The last command expects
the in-repo PolyScript compiler package and compiles selected `.poly` examples before
running them through CPython-hosted `libomnivm`.

The public vertical example can also be run manually through the Docker-backed
manifest runner after compiling it from the in-repo PolyScript compiler package:

```bash
cd "${POLYSCRIPT_DIR:-polyscript}"
npm run polyc -- examples/vertical-order-review-app.poly -o /tmp/vertical-order-review-app.json
docker run --rm \
  -v /tmp/vertical-order-review-app.json:/tmp/vertical-order-review-app.json:ro \
  --entrypoint manifest-runner omnivm \
  /tmp/vertical-order-review-app.json
```

Expected app output:

```text
Vertical order app order=ord-42 routes=6 django=200 react=71 java=priority ruby=review-active workers=2 adjustment=7
```

## Docker package availability

The Docker image includes the runtime packages needed by the checked-in
manifest examples and the selected `.poly` smoke examples. Package
coverage is intentionally environment-level: PolyScript decides runtime
ownership from source syntax, while Docker/OmniVM supplies the packages.

| Runtime | Docker/package coverage used by examples |
| --- | --- |
| Python | `Django`, `pandas`, `numpy`, `pyarrow`, `Pillow`, `polars`, `fastapi`, `uvicorn`, `Flask`, `beautifulsoup4`, `pydantic`, `marshmallow`, `jsonschema`, `jax`, `SQLAlchemy`, `psycopg[binary]`, `asyncpg`, `boto3`, `google-api-core`, `redis`, `pymongo`, `mockupdb`, `Jinja2`, `Markdown`, `httpx`, `aiohttp`, `requests`, and `pytest` are installed in `/opt/omnivm-python` and exposed through `PYTHONPATH`. |
| JavaScript | `express`, `fastify`, `zod`, `cheerio`, `lodash`, `d3-shape`, `marked`, `react`, `react-dom`, `rxjs`, `undici`, `busboy`, `multer`, `body-parser`, `koa`, `koa-bodyparser`, `prisma`, `@prisma/client`, `@prisma/adapter-pg`, and `pg` are installed under `/usr/local/lib/node_modules` and exposed through `NODE_PATH`. Prisma generation runs during image build and links the generated client into the parent `node_modules/.prisma` location that `@prisma/client` resolves. The non-React JSX factory smoke uses a local factory and does not require `preact` or another JSX package. |
| Ruby | Debian packages provide `nokogiri` and `rack`; gems install `activerecord`, `sqlite3`, `pg`, `puma`, `async`, and `actionpack`, with supporting gems linked into Ruby's site directory for embedded runtime loading. |
| Java | The image downloads jars for Gson, Apache Commons CSV, jsoup, OkHttp/Okio, Jackson, Reactor, reactive-streams, RxJava, Guava/failureaccess, Kotlin stdlib/coroutines, and H2 into `/omnivm/libs`. |
| Go | Go examples use the Go toolchain plus standard-library imports. Generated Go plugin/c-shared paths use the repo's build scripts and fixtures inside the image. |

If a new example imports a package outside this table, update the Dockerfile
and add a smoke assertion before relying on it in docs.

For the real-library gap matrix behind the next ecosystem fixtures, see
[`ecosystem-gap-assessment.md`](ecosystem-gap-assessment.md).
