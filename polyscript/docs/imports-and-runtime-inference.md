# Imports and Runtime Inference

PolyScript uses import syntax as runtime evidence. It does not need to know every package name in every ecosystem, and it should not infer a runtime just because a third-party package is popular.

The practical rule is:

- write the import exactly as the owning runtime expects it;
- let that syntax establish runtime ownership for imported bindings;
- use `// @runtime ...` or explicit runtime tags only when the source shape is intentionally ambiguous.

## Python

Use normal Python import syntax:

```polyscript
import pandas as pd
import package_name.submodule as pkg
from fastapi import FastAPI
from sqlalchemy.orm import Session as DbSession
```

Python package distribution names and Python import names are not always the same. For example, a project might be installed under one name and imported under another. PolyScript follows Python import syntax; it does not translate distribution names.

Runtime inference comes from the Python-shaped import statement, not from a table that knows `pandas`, `fastapi`, or `sqlalchemy`.

## JavaScript

Use ES import syntax for packages, scoped packages, subpaths, and side-effect imports:

```polyscript
import express from "express"
import { z } from "zod"
import * as tools from "@scope/package/subpath"
import "package/register"
```

JSX is JavaScript-family syntax. It defaults to `React.createElement` and `React.Fragment`, but pragmas can target another JSX ecosystem:

```polyscript
/** @jsx h */
/** @jsxFrag Fragment */
import { h, Fragment } from "preact"

const badge = <><span className="badge">Poly</span></>
```

The package path can be unfamiliar. The ES import form is the runtime signal.

## Go

Use Go quoted imports, including aliases and grouped imports:

```polyscript
import "net/http"
import worker "example.com/org/pkg-name/worker"

import (
  "github.com/acme/pkg-name/subpkg"
  tools "example.com/org/tool-kit"
)
```

Quoted domain-style module paths are Go evidence. Go package paths may contain dashes even when the local package identifier does not.

## Ruby

Use Ruby `require` syntax:

```polyscript
require "json"
require "dry/validation"
require "active_record"
require "my-gem/subpath"
```

Ruby gem names and require paths can differ. PolyScript preserves the require path and lets Ruby resolve it.

## Java

Use Java dotted imports:

```polyscript
import java.util.concurrent.CompletableFuture
import static java.util.concurrent.TimeUnit.SECONDS
import java.util.*
```

Java class, static, and wildcard import syntax is Java evidence. Third-party Java package roots such as `com.*`, `org.*`, and `jakarta.*` are handled by Java dotted syntax rather than a per-library table.

## Why Package Names Do Not Choose Runtimes

Many package names collide or look ambiguous:

- `http` exists in more than one runtime.
- `json` is common across Python, Ruby, JavaScript, and Java ecosystems.
- `react-dom/server`, `django`, `zod`, `active_record`, and `sqlalchemy` are useful examples, but hardcoding them would not scale to real work.

PolyScript should make decisions from source evidence:

- `import django.forms` is Python syntax.
- `import { renderToString } from "react-dom/server"` is JavaScript syntax.
- `require "active_record"` is Ruby syntax.
- `import com.example.Service` is Java syntax.

If a raw package path is seen without source syntax that proves its owner, the resolver should stay conservative.

## When To Use `// @runtime`

Use a file-level directive when a file intentionally has weak syntax evidence but should default to one runtime:

```polyscript
// @runtime python

value = build_from_framework_convention(request)
```

This is useful for compatibility snippets, generated code, or framework callbacks that hide normal syntax. It should not replace ordinary imports.

## When To Use Explicit Runtime Tags

Runtime tags are expression-level escape hatches:

```polyscript
const rendered = @js(render(view_model))
const count = @py(len(records))
```

Use them when one expression is genuinely ambiguous and adding natural syntax would distort the code. Avoid them in ordinary examples; a `.poly` file should usually be readable as normal donor-language code.

## Runtime Availability

Parsing an import and resolving a runtime does not install the package. The package must still exist in the target runtime environment:

- Python modules must be installed and importable by Python.
- JavaScript packages must be present in Node's module search path.
- Go imports must be available to generated Go compilation.
- Ruby gems must be installed and require-able.
- Java classes and jars must be on the classpath.

The compiler decides ownership. OmniVM or the deployment image provides the runtime packages.
