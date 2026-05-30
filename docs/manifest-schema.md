# Dispatch Manifest Schema

OmniVM's dispatch manifest schema is versioned with the manifest format.

- Current manifest version: `1`
- Canonical compiler schema export: Garbage `DISPATCH_MANIFEST_SCHEMA`
- Schema id: `https://omnivm.dev/schemas/dispatch-manifest.v1.schema.json`

The schema covers:

- top-level `version`, `defaultRuntime`, `ops`, `bridges`, `typeSummary`, and
  `diagnostics`;
- every manifest op consumed by OmniVM: `exec`, `eval`, `exec_compiled`,
  `eval_compiled`, `declare`, `assign`, `func_def`, `return`, `if`, `loop`,
  `try`, `throw`, `parallel`, `concat`, `import`, `native`, `chan`, `select`,
  `spawn`, `yield`, and `await`;
- bridge metadata and diagnostic records.

Conformance is intentionally split:

- Garbage owns schema generation and source-to-manifest tests.
- OmniVM owns manifest execution tests against real examples.
- The shared compatibility gate is the root workspace `make test`, which runs
  Garbage's schema/generator tests and OmniVM's Docker manifest runner.

When manifest version `2` is introduced, it should add a new schema id rather
than mutating the version `1` contract.
