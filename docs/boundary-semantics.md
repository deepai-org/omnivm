# OmniVM Boundary Semantics

This document defines how values cross OmniVM runtime boundaries. It is the
contract for manifest execution, compiler lowering, bridge validation, and
future runtime inference work.

## Boundary Model

Every value crossing a runtime boundary is represented as one of three forms:

- `copy`: an immutable value copied into the target runtime.
- `ref`: an opaque runtime-owned handle with identity and lifetime managed by
  the source runtime.
- `stream`: a sequenced value source, such as a channel or iterator, that is
  consumed according to an explicit materialization rule.

Serialization is not the default boundary model. It is used only when requested
by a manifest bridge operation or when a runtime cannot expose a usable `ref`.

## Value Matrix

| Value kind | Default crossing | Ownership | Mutation visibility | Notes |
| --- | --- | --- | --- | --- |
| `null` / `nil` / `undefined` | `copy` | none | none | Lowered to the nearest target-runtime null value. |
| booleans | `copy` | none | none | No identity is preserved. |
| integers | `copy` | none | none | Narrowing must be explicit or validated by bridge rules. |
| floats | `copy` | none | none | Precision loss must be explicit or diagnosed. |
| strings | `copy` | none | none | UTF-8 text; runtimes may store internally however they need. |
| bytes | `copy` unless explicitly shared | source owns original | no | Shared buffers must use the Arrow/shared-buffer path. |
| arrays/lists | `copy` by default | target owns copy | no | Elements cross recursively using this matrix. |
| maps/objects/structs | `copy` by default | target owns copy | no | Opaque host objects may cross as `ref` instead. |
| functions/callbacks | `ref` | defining runtime | yes, via calls | Calls marshal arguments/results through this contract. |
| runtime objects/classes/modules | `ref` | source runtime | yes, via methods | Target receives an opaque handle or generated stub. |
| errors/exceptions | `copy` summary plus optional `ref` | source runtime | no | Structured error data should include runtime, type, message, traceback. |
| channels | `stream` | OmniVM manifest scope | consumption-dependent | See channel rules below. |
| iterators/generators | `stream` or `ref` | defining runtime | consumption-dependent | Must declare whether crossing drains or proxies. |

## Runtime Refs

A runtime ref is an opaque handle to a value owned by one runtime.

- The source runtime owns allocation and object identity.
- Other runtimes must access the value through bridge calls, generated stubs, or
  explicit manifest operations.
- Runtime refs must not be silently serialized just because the target runtime
  cannot inspect them.
- Ref lifetime is at least the duration of the manifest execution unless an
  explicit release operation is introduced.
- Future release/finalizer work must be idempotent and safe after source-runtime
  shutdown.

## Copies

Copied values are detached from the source runtime.

- Mutating a copied array, map, object, or struct in the target runtime does not
  mutate the source value.
- Copy operations must be deterministic and JSON-compatible only when the bridge
  operation says JSON is the representation.
- Unsupported nested values must fail with a boundary error unless a fallback
  bridge is explicitly configured.

## Channels And Streams

Channels are OmniVM-owned manifest resources, not native runtime objects.

- `chan make` creates a manifest-scoped channel.
- `chan send` copies the sent value into the channel unless the value is already
  a runtime ref.
- `chan recv` consumes one item and returns either the item or the runtime's null
  value when the channel is closed and empty.
- Capturing a channel into JavaScript with `Array.from(channel)` materializes the
  currently buffered/drainable values into a strict array.
- Global `wait(...)` returns spawn results, not channel contents.
- Channel draining must be explicit in the lowered IR or manifest operation.

Iterators and generators need an explicit crossing mode:

- `stream`: target pulls values lazily through a bridge.
- `copy`: target drains the iterator into an array/list.
- `ref`: target receives an opaque iterator handle.

`stream_proxy` bridge ops now carry an explicit stream marker into JavaScript
captures. The materialized target value is iterable, exposes a strict `toArray()`
snapshot, and has cancellation metadata. This is still a proxy contract, not an
implicit JSON array contract.

## Opaque Resources And Jobs

Runtime-owned handles such as transactions, request/response objects, database
connections, and job scheduler internals should not cross as JSON copies.

- `resource open` creates a manifest-owned opaque handle with runtime, kind, and
  disposer metadata.
- `resource close` marks that handle closed and is intended for `finallyBody`
  cleanup paths.
- Capturing a resource into another runtime injects a proxy descriptor, not the
  live object.
- `job enqueue` creates a delayed-work handle; `job complete` records its
  eventual result; `job wait` materializes that result into a normal binding.

## Callbacks

Callbacks cross as refs to callable values.

- The defining runtime owns the callback and its closure.
- Arguments are marshalled using this same boundary matrix.
- Return values are marshalled back to the caller using this same matrix.
- Exceptions propagate as structured boundary errors and must preserve the source
  runtime.
- Callback refs must remain alive at least as long as any generated stub can call
  them.

## Serialization

Serialization is an explicit bridge operation, not an implicit fallback.

Allowed serialization triggers:

- a manifest `bridge` op such as `serialize_json` or `deserialize_json`;
- a user-authored encode/decode call in source code;
- an explicit compiler-lowered fallback marked in diagnostics.

Implicit serialization is forbidden for:

- runtime refs;
- callbacks;
- channels;
- iterators/generators;
- objects with unsupported identity or mutation semantics.

## Boundary Errors

Boundary failures must identify:

- source runtime;
- target runtime;
- value kind;
- attempted bridge operation;
- reason the crossing was rejected;
- suggested explicit bridge operation when one exists.

The manifest runner should prefer typed boundary errors over runtime-specific
string failures.

## Lowering Requirements

Compiler lowering must make boundary intent explicit before manifest emission:

- value copy: `BridgeValue` with copy semantics;
- runtime ref: `BridgeValue` with ref semantics;
- callback: `BridgeValue` with callback semantics;
- channel materialization: channel-specific IR, not hidden capture behavior;
- serialization: explicit serialize/deserialize bridge operation.

Manifest emission should not infer boundary behavior from raw source strings.
