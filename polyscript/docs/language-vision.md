# PolyScript Language Vision

PolyScript is for writing normal code from several runtimes in one source file, then letting the compiler make the runtime boundary explicit in the manifest. A `.poly` file should read like application code, not like a sequence of FFI calls.

## Core Model

The source language is the union of donor-language syntax that users already know. Python should look like Python, JavaScript should look like JavaScript, Go should look like Go, Ruby should look like Ruby, and Java should look like Java. PolyScript adds manifest-level orchestration, not new spellings for ordinary runtime operations.

Runtime ownership comes from syntax and local provenance:

- Imports establish the runtime for the bindings they introduce.
- Syntax that only exists in one donor language is stronger than vague package-name guesses.
- Expressions can cross runtime boundaries by referencing live bindings from another runtime.
- The generated manifest records captures, bridges, resources, streams, tables, and jobs.

Explicit runtime tags such as `@py(...)`, `@js(...)`, `@go(...)`, `@rb(...)`, and `@java(...)` are escape hatches. They are useful when a framework callback or intentionally ambiguous expression hides its runtime signal, but they should not be normal example style.

## Runtime Boundaries

The preferred boundary is a typed or structural manifest value:

- primitives cross as values;
- arrays, maps, and records cross structurally;
- live framework objects, connections, transactions, streams, and jobs cross as handles or proxies;
- tabular and buffer-like values should move toward Arrow/table or native-memory handles rather than JSON rows;
- errors cross with structured type, message, cause, stack, and detail fields when the source runtime exposes them.

JSON is application data, not runtime glue. It belongs in HTTP bodies, persisted documents, API payloads, or intentionally opaque values. Examples should not manually `JSON.stringify`, `json.loads`, `to_json`, or `String.valueOf` just to move ordinary data across a PolyScript boundary.

## Ecosystem Libraries

PolyScript should work with library ecosystems without knowing their brand names. The parser and resolver may recognize platform syntax and standard-library shapes, but they should not decide runtime ownership because a package is popular.

Good evidence:

- `import pandas as pd` is Python syntax.
- `import { h } from "preact"` is JavaScript syntax.
- `import "github.com/acme/pkg-name/subpkg"` is Go syntax.
- `require "dry/validation"` is Ruby syntax.
- `import java.util.concurrent.CompletableFuture` is Java syntax.

Weak evidence:

- a package name such as `django`, `zod`, `active_record`, `sqlalchemy`, or `react-dom/server` by itself;
- method names that collide across ecosystems, such as `items`, `keys`, `values`, `get`, `then`, `close`, or `length`;
- framework-specific error type names when a structural shape is available.

The language should preserve natural library behavior. If a runtime object has a field named `then`, `items`, `keys`, `close`, or `length`, user code should access that field naturally. Proxy metadata and bridge helpers must not shadow application fields.

## JSX

JSX is JavaScript-family syntax, not a React-only feature. The default lowering target is `React.createElement` and `React.Fragment` for compatibility, but source pragmas can select another ecosystem:

```polyscript
/** @jsx h */
/** @jsxFrag Fragment */
import { h, Fragment } from "preact"

const badge = <><span className="badge">Poly</span></>
```

Custom factories and member factories are equally valid:

```polyscript
/** @jsx view.create */
/** @jsxFrag view.Fragment */
const panel = <Panel title="Orders" />
```

The compiler should lower JSX according to these source-level signals without requiring a React import or assuming a particular package.

## Import Syntax

Import syntax should stay donor-language-shaped:

- Python: `import package`, `import package.module as alias`, `from package.module import name as alias`.
- JavaScript: `import x from "package"`, `import { name } from "@scope/package/subpath"`, `import "package/register"`.
- Go: `import "domain.example/org/pkg-name"`, grouped imports, and Go aliases such as `alias "domain.example/pkg"`.
- Ruby: `require "gem_name"` or `require "gem-name/subpath"`.
- Java: dotted class/package imports, static imports, and wildcard imports.

When the same spelling could belong to more than one runtime, syntax and surrounding context should decide. If there is no reliable signal, the resolver should stay conservative instead of adding a package-name table.

For practical examples, see [`imports-and-runtime-inference.md`](imports-and-runtime-inference.md).

## Example Style

Public `.poly` examples should demonstrate the intended mental model:

- write normal donor-language code;
- let captures happen automatically;
- keep resource lifecycles local until a manifest handle is the natural boundary;
- use framework objects as live proxies when materializing them would change behavior;
- use explicit tags only in examples about explicit tags;
- avoid source-level runtime coercion that exists only to satisfy the current implementation.
