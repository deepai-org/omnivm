# Manifest Channel and Spawn Contract

This document defines the manifest-level contract for Go-backed channels,
spawned workers, and cross-runtime channel captures.

## Channels

- `{"op":"chan","action":"make","bind":"name","size":N}` creates a buffered Go channel and binds it in manifest scope.
- `send` appends to the channel when capacity is available. Sending to a closed channel is an error for `chan` ops and returns `false` for Go helper calls.
- `recv` is non-blocking for manifest `chan` ops. It binds `nil` when no buffered item is immediately available or when the channel is closed and drained.
- `close` closes the channel. Closing an already closed channel is an error.
- Channel names are registered outside the general manifest binding map so spawned Go workers can safely call `recv("name")` and `send("name", value)`.

## Go Helper Functions

Go plugin functions can request these helpers through `Init(deps map[string]interface{})`:

- `recv(channel)` blocks until a value is available or the channel is closed. It returns `nil` on closed-and-drained channels.
- `send(channel, value)` sends a value and returns `true`; it returns `false` when the channel cannot be resolved or is closed.
- `wait()` waits for all manifest spawns known to the executor and returns the count.
- `wait(handle)` waits for one spawn handle and returns that worker's result.
- `wait(handle1, handle2, ...)` waits for the listed handles and returns results in argument order.

The `channel` argument may be either a channel binding or a channel binding name.

## Spawn Handles

- `spawn` ops return a manifest `SpawnHandle`.
- If a `spawn` op has `bind`, the handle is bound under that name.
- A handle completes when the worker returns or panics.
- A panicking worker logs the panic and resolves the handle to `nil`.
- Bare `wait()` remains supported for existing manifests, but selective `wait(handle)` is preferred for long-term manifests.

## Cross-Runtime Channel Captures

When a channel is explicitly captured by a non-Go runtime, OmniVM drains the channel's buffered values into a snapshot.

- Python receives a list.
- JavaScript receives an iterable adapter, so `Array.from(channelBinding)` is valid.
- Ruby and Java receive JSON-compatible arrays through their existing capture paths.

Channel captures are snapshots. They do not keep a live channel subscription open across runtime boundaries.
