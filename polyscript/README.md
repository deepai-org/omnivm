# PolyScript

A universal parser that handles multiple programming language syntaxes in a single file. Write Python, JavaScript, Go, Rust, Ruby, Bash, and more — all in the same `.poly` file — and PolyScript will parse, resolve runtimes, and generate a dispatch manifest that orchestrates execution across language boundaries.

## Why?

Modern backends glue together multiple runtimes anyway — Python for ML, Go for concurrency, JS for APIs. PolyScript makes that explicit at the syntax level. Instead of shelling out or using FFI, you write naturally in each language and let the compiler figure out the bridging.

```polyscript
import os
import json

// Python: scan the filesystem
const files = os.listdir("/var/data")

// JS: arrow function forces this to JavaScript
const loud = files.map(f => f.toUpperCase())

// Python: list comprehension stays in Python
const filtered = [f for f in loud if f.endswith(".log")]

// Go: spin up concurrent workers
const w1 = go worker(1)
const w2 = go worker(2)
const joined = wait(w1, w2)

print(f"Found {len(filtered)} log files")
```

## Getting Started

```bash
npm install
npm test          # Run all tests
npm run build     # Compile TypeScript
node scripts/audit-manifests.js
```

### Compile a .poly File

```bash
npm run polyc -- myapp.poly
```

This outputs a **dispatch manifest** — a JSON IR that OmniVM interprets, dispatching each code fragment to the appropriate runtime.

To run the generated manifest, build OmniVM and pass the output to its manifest runner:

```bash
npm run polyc -- examples/cursed-concurrency.poly -o /tmp/cursed-concurrency.json
docker run --rm \
  -v /tmp/cursed-concurrency.json:/tmp/cursed-concurrency.json \
  --entrypoint manifest-runner omnivm \
  /tmp/cursed-concurrency.json
```

The public vertical example exercises the full stack across Python, JavaScript,
Java, Ruby, and Go and has a checked-in output golden:

```bash
npm run polyc -- examples/vertical-order-review-app.poly -o /tmp/vertical-order-review-app.json
docker run --rm \
  -v /tmp/vertical-order-review-app.json:/tmp/vertical-order-review-app.json:ro \
  --entrypoint manifest-runner omnivm \
  /tmp/vertical-order-review-app.json
```

Expected app output:

```text
Vertical order app order=ord-42 routes=5 django=200 react=71 java=priority ruby=fiber-active workers=2 adjustment=7
```

For the current cross-repo smoke, keep `garbage` and `omnivm` as sibling
checkouts and run the CPython-hosted `libomnivm` path from the OmniVM repo:

```bash
cd ../omnivm
make test-all
make test-poly-libomnivm-smoke
make test-libomnivm-manifests
make test-libomnivm-stress
```

Set `GARBAGE_DIR=/path/to/garbage` if the repos are not siblings. This is the
same coverage path the README expects CI to mirror: compile selected `.poly`
examples, execute the generated manifests under Python-hosted `libomnivm`, then
run the checked-in manifest and stress suites. The current milestone is that
CPython-hosted `libomnivm` runs the example suite with Python as the parent
process, including Passenger/Django `.poly` imports, Go selector constants,
Go `main()` entrypoints, nested complex return proxies, and zero JSON fallback
counts for cached primitive movement.

### As a Library

```javascript
const { Lexer, Parser, RuntimeResolver, ManifestCodeGenerator } = require('./dist');

const code = `...`;
const lexer = new Lexer(code);
const tokens = lexer.tokenize();
const parser = new Parser(tokens, code);
const ast = parser.parse();

const resolver = new RuntimeResolver();
const annotated = resolver.resolve(ast, code);
const generator = new ManifestCodeGenerator();
const manifest = generator.generate(annotated);
```

## How It Works

```
Source (.poly) → Lexer → Parser → AST
  → Runtime Resolver (which runtime owns each node?)
  → Type System (validate types at boundaries, emit bridge ops)
  → Manifest Generator (emit dispatch ops with captures + bridges)
  → Dispatch Manifest (JSON IR for OmniVM)
```

1. **Lexer** — Tokenizes polyglot source with virtual semicolon insertion (MASI), 5 context modes, and operator ambiguity handling (`<` as comparison vs generic vs JSX)
2. **Parser** — Pratt parser with parselet registry producing a unified AST from mixed syntax
3. **Runtime Resolver** — Two-pass analysis determines which language each expression belongs to using import provenance, syntactic dominance (arrows → JS, list comprehensions → Python), cross-runtime variable tracking, and cost modeling
4. **Type System** — Unified canonical type IR validates data flowing across runtime boundaries and emits bridge operations for the manifest
5. **Manifest Generator** — Emits a dispatch manifest that tells OmniVM how to orchestrate calls across runtimes with automatic bridging

## Concurrency Model

PolyScript treats Go concurrency syntax as first-class `.poly` syntax and lowers it into OmniVM manifest operations.

```polyscript
const inbox = make(chan int)
const outbox = make(chan int)

func worker(id int) {
  for item := range inbox {
    outbox <- item * id
  }
  return id
}

const w1 = go worker(1)
const w2 = go worker(2)

inbox <- 10
close(inbox)

const joined = wait(w1, w2)
close(outbox)
```

`go worker(...)` returns a manifest-visible spawn handle. `wait()` with no arguments waits for all spawned workers and returns their count; `wait(handle)` returns one worker's result; `wait(h1, h2)` waits for each handle and returns an array of results in argument order. Channels are also manifest-visible values, so JavaScript, Python, and Go fragments can consume the same channel when the manifest runner captures it into that runtime.

## PolyScript Syntax Contract

The platonic `.poly` model is: write normal donor-language syntax, let the compiler infer ownership, and use explicit syntax only where a boundary needs to become a manifest value.

Core rules:

| Rule | Contract |
|------|----------|
| Runtime ownership | Imports and syntax decide ownership. `os.scandir(...)` is Python; `items.map(x => x.id)` is JavaScript; `<-`, `make(...)`, `go`, and `wait(...)` are Go/manifest concurrency. |
| Runtime tags | `@py(...)`, `@js(...)`, `@go(...)`, `@rb(...)`, and `@java(...)` are escape hatches, not normal style. Use them only when inference is ambiguous or a framework convention hides the runtime signal. |
| Captures | Referencing a binding from another runtime emits an explicit manifest capture. The source code should not manually JSON encode/decode ordinary arrays, maps, strings, numbers, or booleans just to cross a runtime boundary. |
| Serialization | JSON belongs in application semantics: HTTP bodies, persisted documents, APIs, or intentionally opaque payloads. It should not be required as glue between `.poly` statements. |
| Channels | `const ch = make(size)` creates a manifest channel. `ch <- value`, `<-ch`, and `close(ch)` lower to channel ops. Non-Go runtimes receive channel captures as snapshots/adapters. |
| Spawn handles | `const h = go worker(args)` binds a spawn handle. Prefer named handles plus `wait(h)` or `wait(h1, h2)` over bare fire-and-forget spawns when later code depends on worker completion. |
| Resources, tables, and jobs | Runtime-owned resources, Arrow/DataFrame tables, and queued jobs lower to first-class OmniVM manifest ops. Live request/response, transaction, table, connection, stream, and queued-job internals should cross as inferred handles or proxies. |
| Worker shape | Long-term portable workers are named Go functions that return a value and use manifest helpers such as `recv("channel")` and `send("channel", value)`. Inline spawn closures may parse, but they are not the durable contract for OmniVM joins. |
| JSX | JSX is JavaScript-family syntax, not React-specific syntax. By default it lowers to `React.createElement`/`React.Fragment`; `/** @jsx factory */` and `/** @jsxFrag Fragment */` select another factory such as Preact's `h` or an application-local member factory. |
| Diagnostics | The compiler emits manifest diagnostics for likely runtime-boundary mistakes, including `wait(...)` on non-handles, channel operations on unknown/non-channel bindings, and spawn forms OmniVM cannot reliably join. |

For tabular data, the intended long-term boundary is a zero-copy Arrow handle,
not JSON rows. Garbage should eventually lower DataFrame/Arrow-friendly library
values to an OmniVM `table` handle using the Arrow C Data Interface in-process,
with Arrow IPC or an explicit copy only when pointer sharing is not available.
The current contract is syntax/protocol-driven, not a source-level helper or
package-name runtime guess:

```polyscript
import numpy as np

const orders: Table = np.array([1, 2, 3])
console.log(orders)  // captures an Arrow/table proxy, not JSON rows
```

`borrowed` is the default ownership mode for inferred table handles. It keeps
the producer alive and passes a table descriptor across runtimes rather than
materializing rows.

## Runtime Resolver

The runtime resolver determines which language owns each statement — fully automatically, with no annotations or pragmas required. It uses a two-pass analysis:

**Pass 1 (Structural)** tags nodes with evidence from syntax and imports. Import
syntax is runtime evidence; third-party package names are not a runtime table.
For example, `import pandas as pd` is Python because it is Python syntax,
`import { h } from "preact"` is JavaScript because it is ES import-from syntax,
`import "github.com/acme/pkg-name"` is Go because it is quoted Go module syntax,
`require "dry/validation"` is Ruby because it is Ruby require syntax, and
`import java.util.concurrent.CompletableFuture` is Java because it is Java dotted
class syntax.

| Signal | Language | Example |
|--------|----------|---------|
| `import os` | Python | Import provenance |
| `import { h } from "preact"` | JavaScript | ES import-from syntax |
| `import "github.com/acme/pkg-name"` | Go | Quoted Go module syntax |
| `require "dry/validation"` | Ruby | Ruby require syntax |
| `import java.util.List` | Java | Java dotted class syntax |
| `=>` arrow functions | JavaScript | Syntactic dominance (impossible in Python) |
| `===`, `!==` | JavaScript | Strict equality operators |
| `[x for x in ...]` | Python | List comprehension |
| `<-` channel send | Go | Channel operator |
| `/.../` regex literal | JavaScript | Regex syntax |
| `sorted()`, `len()` | Python | Builtin recognition |

**Pass 2 (Propagation)** flows affinities through expression chains:

- **Variable tracking**: `const files = os.listdir()` registers `files` as Python, so later uses of `files` carry Python provenance
- **Syntactic override**: `files.map(f => f.toUpperCase())` — even though `files` is Python, the arrow function forces JS
- **Function scoping**: statements inside `def crawl():` inherit Python; inside `func worker()` inherit Go
- **Expression inheritance**: `ExprStmt`, `Loop`, `ConstDecl`, and `VarDecl` inherit their child expression's runtime

When a variable crosses runtimes, the manifest generator automatically inserts captures and bridge ops:

```polyscript
import os
const files = os.listdir("/data")          // Python (import provenance)
const loud = files.map(f => f.toUpperCase()) // JS (arrow override) — captures `files` from Python
const ordered = sorted(loud)               // Python (builtin) — captures `loud` from JS
```

The resolver intentionally stays conservative for ambiguous third-party names.
Raw package names such as `django`, `zod`, `sqlalchemy`, `active_record`, and
`react-dom/server` are not enough by themselves to choose a runtime; the source
form that imports or uses them supplies the runtime evidence.

For practical import examples across Python, JavaScript, Go, Ruby, and Java,
see [`docs/imports-and-runtime-inference.md`](docs/imports-and-runtime-inference.md).

## Type System

PolyScript's type system is **permissive by default** — like TypeScript, but with `any` as the starting point rather than an opt-out. Untyped code runs without interference. Types only come into play when you write them, and only known-incompatible crossings are blocked.

```polyscript
// No types → everything is `any` → runs without restriction
const x = getData()
process(x)

// One side typed → `any` narrows to the target type (always succeeds)
function process(x: number) { ... }
process(getData())  // any → number: fine

// Both sides typed → real checking kicks in
fn get_count() -> i32 { 42 }
function display(s: string) { console.log(s) }
display(get_count())  // i32 → string: auto-coerced via to_string bridge op

// Only blocked when types are known AND incompatible
fn get_point() -> Point { ... }
function process(v: Vec<u8>) { ... }
process(get_point())  // Point → Vec<u8>: REJECTED at compile time
```

The philosophy: **never reject code you don't understand**. The type system is purely additive — it helps when annotations are present, emits bridge ops for safe marshaling, and stays silent when types are unknown. You can add types incrementally as your polyglot program grows, and each annotation you add gives you more safety without breaking existing untyped code.

Every type annotation — TypeScript generics, Python type hints, Go types, Rust signatures — lowers to a canonical IR, then the boundary checker validates crossings and emits bridge operations.

### Canonical Types

All language-specific types lower to a shared representation:

| Category | Types | Examples |
|----------|-------|---------|
| Primitives | `int`, `float`, `bool`, `string`, `bytes`, `void`, `never`, `any` | `i32`, `f64`, `bool`, `String` |
| Collections | `array`, `map`, `set`, `tuple` | `Vec<u8>`, `Dict[str, int]`, `[]string` |
| Wrappers | `option`, `result`, `async` | `Option<T>`, `Result<T,E>`, `Promise<T>` |
| Functions | `func` | `(i32) => bool`, `fn(u8) -> u8` |
| Structs | `struct` (nominal or structural) | `interface User {}`, `struct Point` |
| Enums | `enum` with variant payloads | `enum Shape { Circle(f64) }` |
| Concurrency | `channel`, `stream` | `chan int`, `AsyncIterable<T>` |
| Memory | `buffer_view` | `Uint8Array`, `&[u8]`, `[]byte` |
| Resources | `disposable` | `Disposable<T>`, `io.Closer` |

### Boundary Checking

When data flows between runtimes, the type system determines one of four outcomes:

| Result | Meaning | Example |
|--------|---------|---------|
| **safe** | No conversion needed | `string` → `string` |
| **coerce** | Lossless conversion | `i32` → `f64`, struct subtyping |
| **check** | May fail at runtime | `f64` → `i32` (truncation), `Option<T>` → `T` |
| **incompatible** | Cannot cross | `Vec<u8>` where `Point` expected |

Cross-runtime structs use **structural** compatibility (duck typing at boundaries), while same-runtime structs use **nominal** matching. This means a Go `User{name, age}` can flow into a TypeScript `{name: string, age: number}` if the fields match.

### Bridge Operations

The type system emits concrete bridge ops that tell OmniVM how to marshal data:

| Bridge Op | Purpose |
|-----------|---------|
| `widen` / `narrow` | Numeric conversions with size tracking |
| `wrap_option` / `unwrap_option` | `T` ↔ `Option<T>` |
| `wrap_result` / `unwrap_result` | `T` ↔ `Result<T, E>` |
| `throw_typed` | `Result.Err` → typed exception with error kind metadata |
| `proxy_callable` | Cross-boundary function/closure proxy |
| `tag_dispatch` | Enum → discriminated union mapping |
| `share_memory` / `copy_buffer` | Zero-copy buffer passing vs copying |
| `stream_proxy` | Channel/Stream → AsyncIterable proxy |
| `serialize` / `deserialize` | Complex type marshaling (JSON/msgpack) |
| `compose` | Chained ops for nested generics (`Async<Option<T>>` → `T`) |

### Runtime Guards

When a crossing is `check` (may fail), the type system generates guard code hints for validation:

```
f64 → i32:
  JS:     Number.isInteger(value) && value >= -2147483648 && value <= 2147483647
  Python: isinstance(value, (int, float)) and float(value).is_integer()
  Go:     _, ok := value.(int64)

string → int:
  JS:     !isNaN(parseInt(value, 10))
  Python: value.lstrip("-").isdigit()
  Go:     _, err := strconv.ParseInt(value, 10, 64); err == nil
```

### Type Inference

The manifest generator infers types when annotations are absent:

- **Function returns**: `const x = getUser()` → infers from `getUser`'s return type
- **Member access**: `user.name` → resolves field type from known struct
- **Index access**: `arr[0]` → resolves element type from known array/map
- **Literals**: strings, numbers, booleans, array literals
- **Lambda params**: `(b: number) => b + 1` → infers `func` type

## Supported Syntax

PolyScript parses real syntax from each donor language — nothing is invented.

| Language   | Features                                                                 |
|------------|--------------------------------------------------------------------------|
| JavaScript | Arrow functions, async/await, destructuring, template literals, JSX, `===` |
| TypeScript | Type annotations, generics, interfaces, mapped types, type assertions    |
| Python     | List comprehensions, `def`, `for...in`, f-strings, decorators, `from...import` |
| Go         | Short declarations `:=`, goroutines, channels `<-`, `select`, `func`, composite literals |
| Rust       | `fn`, `match`, `::` paths, `.await`, `?` operator, macro patterns, traits |
| Ruby       | `do...end` blocks, `\|params\|`, `unless`, symbols                       |
| Bash       | `if...then...fi`, `case...esac`, `[ ]` tests                            |
| C++        | Templates, concepts, `requires` expressions, `::` scope resolution       |
| Kotlin     | `when` expressions, `fun`                                                |
| Swift      | `guard`, operator declarations                                           |
| Elixir     | Pipe operator `\|>`, `defmacro`, `do...end`                              |

Import forms cover runtime package ecosystems without package-specific parser
rules:

| Runtime | Import forms |
|---------|--------------|
| Python | `import package`, `import package.module as alias`, `from package.module import name as alias` |
| JavaScript | default, namespace, named, side-effect, scoped-package, and package-subpath ES imports |
| Go | quoted imports, grouped imports, aliases, domain-style module paths, and package paths containing dashes |
| Ruby | `require "gem"` and `require "gem/subpath"` |
| Java | dotted class imports, static imports, and wildcard imports |

## Dispatch Manifest

The manifest is a sequence of ops that OmniVM executes. No language is "on top" — OmniVM is the orchestrator.

| Op | Purpose |
|----|---------|
| `import` | Import a module in a runtime |
| `eval` | Execute code, bind result to a name |
| `exec` | Execute code, discard result |
| `func_def` | Define a callable function (possibly polyglot body) |
| `return` | Return from function |
| `declare` | Declare a manifest-scope literal |
| `if` / `loop` | Control flow |
| `try` / `throw` | Error handling with cross-runtime catch |
| `parallel` | Cooperative concurrency across runtimes |
| `chan` / `select` / `spawn` | Go-style concurrency primitives; spawned workers can bind handles |
| `await` | Async pump signal |
| `concat` | Polyglot string interpolation |

The manifest also includes a `bridges` array (bridge ops needed at boundary points) and a `typeSummary` (crossing statistics) when cross-runtime type checking detects boundary crossings.

Go functions emit a `source` field (complete compilation unit) with `exports` (PascalCase symbol names for `plugin.Lookup`) and `requires` (external dependencies injected via `Init()`).

Spawn expressions lower to `spawn` ops with an optional `bind` field:

```json
{ "op": "spawn", "runtime": "go", "code": "worker(1)", "bind": "w1" }
```

The OmniVM manifest runner owns the runtime behavior for handles, channels, `recv`, `send`, `close`, and `wait`.

## Project Structure

```
src/
  lexer.ts                # Main scanner
  lexer-*.ts              # 6 lexer modules (operators, literals, identifiers, comments, MASI, cursor)
  parser.ts               # Pratt parser dispatch (~925 lines)
  parser-cursor.ts        # Transactional cursor base
  parselet-registry.ts    # Keyword → parselet dispatch table
  parselets/              # 11 parselet modules
    jsx.ts                #   JSX/TSX elements
    types.ts              #   Type annotations and generics
    functions.ts          #   Function declarations, parameters, lambdas
    control-flow.ts       #   if/for/while/try/do/defer/go
    blocks.ts             #   Block parsing, switch/match, case...esac
    declarations.ts       #   const/let/var/type/export
    imports.ts            #   import/require/from...import/using
    class-decl.ts         #   Classes, interfaces, traits, enums
    expr-prefix.ts        #   Primary expressions, unary ops, literals
    expr-postfix.ts       #   Calls, member access, Ruby blocks
    literals.ts           #   Strings, arrays, objects, regex
  ast.ts                  # Unified AST node types
  type-system/            # Unified canonical type system
    canonical.ts          #   Type IR (all language types lower to this)
    lowering.ts           #   AST TypeNode → CanonicalType
    coercion.ts           #   Cross-runtime compatibility rules + bridge ops
    boundary-checker.ts   #   Type environment + crossing validation
  runtime-resolver/       # Two-pass runtime affinity analysis
    pass1-structural.ts   #   Syntax/import evidence tagging
    pass2-propagation.ts  #   Bottom-up affinity propagation
    symbol-table.ts       #   Scoped variable tracking
    method-tables.ts      #   Builtin method → runtime mapping
    import-analyzer.ts    #   Import path → runtime mapping
    cost-model.ts         #   Bridge cost computation
  codegen-omnivm/         # Dispatch manifest + source reconstruction
test/                     # 1075+ tests across 42 suites
examples/                 # Polyglot example files
```

## Examples

See [`examples/`](examples/) for complete polyglot programs. All runtimes are **autodetected**. Aside from documented source pragmas such as JSX factory comments, comments in the files are just for human readers.

The sibling OmniVM repo documents how these examples map onto CPython-hosted `libomnivm` and prefork deployments in `docs/example-suite.md` and `docs/passenger-django-polyscript.md`.

- **cursed-polyglot.poly** — Python/JS pipeline that ping-pongs between runtimes every single line
- **cursed-concurrency.poly** — Python generators + Go channels/spawn handles + JS async all talking to each other
- **django-go-typescript-views.poly** — Django-style views using Go and TypeScript without annotation pragmas
- **java-gson-pandas-zod-express.poly** — Java Gson, Python Pandas, JS Zod, and Express in one endpoint-shaped flow
- **java-commons-csv-pydantic-go-batching.poly** — Java Commons CSV parsing with Python validation and Go batching workers
- **java-jsoup-bs4-cheerio.poly** — Java Jsoup, BeautifulSoup, and Cheerio parsing the same HTML fixture
- **java-okhttp-httpx-go-retry.poly** — Java OkHttp request shaping with Python HTTPX and Go retry workers
- **async-httpx-rxjs-errgroup.poly** — Python asyncio/HTTPX, RxJS stream operators, and Go errgroup workers with explicit stream materialization
- **pandas-pydantic-zod-dry-validation.poly** — Pandas groupby/window output with nested Pydantic, Zod, and dry-validation error-shape boundaries
- **orm-session-boundaries.poly** — SQLAlchemy, Prisma, and ActiveRecord session/client shapes where only materialized records cross runtimes
- **framework-middleware-render.poly** — FastAPI-style handlers, Express middleware, React server rendering, and Rack responses
- **java-futures-jdbc-streaming.poly** — Java CompletableFuture, JDBC, OkHttp, and Jackson values analyzed by Python
- **go-http-cobra-observability.poly** — Go net/http, database/sql, Cobra, zap, and slog shapes in one handler/CLI flow
- **request-analytics-ecosystem.poly** — Django request objects, Express routing, Zod validation, Java OkHttp requests, and Pandas/NumPy/Polars analytics
- **orm-model-client-flow.poly** — SQLAlchemy metadata/query values, Prisma-shaped lookups, Zod model validation, Pandas rows, and Java HTTP client objects
- **python-docs-popular-packages.poly** — Docs-style Pandas, NumPy, Pydantic, Jinja2, and BeautifulSoup usage that runs unchanged through OmniVM manifests
- **javascript-docs-popular-packages.poly** — Docs-style Express, Zod, Lodash, Cheerio, and Marked usage
- **javascript-jsx-factory-docs.poly** — JSX lowered through a local factory/fragment pair rather than React
- **java-docs-popular-packages.poly** — Docs-style Gson, jsoup, OkHttp, and Apache Commons CSV usage
- **ruby-docs-popular-packages.poly** — Docs-style Nokogiri and Rack usage
- **go-docs-popular-packages.poly** — Docs-style Go `net/http` and `encoding/json` usage
- **pydantic-zod-error-fidelity.poly** — Pydantic and Zod validation failures preserving structured error details across Python/JavaScript catch blocks
- **python-map-collision-docs.poly** — Python object and mapping fields named like proxy methods accessed naturally from JavaScript
- **python-dataframe-js-table-docs.poly** — Pandas and Polars table values inspected naturally from JavaScript through the inferred table boundary
- **true-async-stream-boundary.poly** — HTTPX async streams crossing into RxJS and Go workers through an explicit stream/materialization boundary
- **live-middleware-opaque-handles.poly** — FastAPI, Express, Rack, and Go HTTP handler shapes where request/response objects stay opaque
- **database-transaction-resource-boundary.poly** — SQLAlchemy, Prisma, ActiveRecord, and JDBC transaction/resource boundaries with materialized rows
- **cross-runtime-job-queue.poly** — Celery, Sidekiq, BullMQ, and Go worker-pool job handle shapes
- **dataframe-arrow-zero-copy-boundary.poly** — Pandas, PyArrow, Polars, and JS analytical table shapes for future buffer/table bridges
- **reactive-future-streams.poly** — Java CompletableFuture/Reactor, RxJS, and Python async stream contracts
- **template-component-rendering-boundary.poly** — Jinja2, React SSR, Rack, and Spring-style model rendering where only HTML crosses
- **typed-validation-error-fidelity.poly** — Pydantic, Zod, dry-validation, and Java Bean Validation structured error paths
- **syntactic-dominance.poly** — Demonstrates how arrow functions override import provenance

Low-level runtime-contract fixtures that intentionally exercise explicit
manifest primitives live under `test/fixtures/runtime-contracts/`, not top-level
examples. Those are regression tests for `resource`, `job`, and `table` ops;
they are not the intended shape of user-authored `.poly`.

## License

MIT
