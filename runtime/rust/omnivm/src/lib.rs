//! omnivm — the Rust guest-side support crate for OmniVM.
//!
//! This crate is the Rust analog of the Go bridge shim plus the
//! runtime-support library from docs/rust-runtime-design.md. It is built as a
//! Rust dylib shipped with the host image; every compiled user unit links it
//! dynamically, so the tokio runtime, the bridge function pointers, the
//! future table, and the object handle table are process-global singletons
//! (the "one executor per process" invariant).
//!
//! User-facing surface:
//!
//! ```ignore
//! let v: String = omnivm::call("python", "2 ** 100")?;
//! let users: Vec<User> = omnivm::call_typed("python", "get_users()")?;
//! // from async context, the bridge hop variant:
//! let v = omnivm::call_async("python", "2 ** 100").await?;
//! ```

pub mod abi;
pub mod cdata;
/// `dyn` is a keyword, so the module gets an explicit path + a plain name.
#[path = "dyn.rs"]
pub mod dyn_value;
pub mod envelope;
pub mod error;
pub mod interop;
pub mod logging;
pub mod objects;
pub mod rt;
pub mod table;

mod export_macros;

pub use error::OmniError;
pub use abi::{FromOmniValue, OmniValue, ToOmniValue};
pub use cdata::Bytes;
pub use dyn_value::{to_value, Dyn};
pub use futures_core;
pub use interop::{Callback, Channel};
pub use objects::ObjectExport;

/// Re-exported so user units and the support library agree on one tokio (the
/// version is pinned by the image, not the user's Cargo.toml — an accepted
/// trade, same as the pinned CPython/Node/JVM versions).
pub use tokio;

/// Re-exported `log` facade (installed automatically at bridge init).
pub use log;

/// Re-exported `tracing` facade: with no subscriber installed, events mirror
/// into the `log` facade (tracing's "log" compatibility), so spans/events
/// from ecosystem crates stay visible.
pub use tracing;

pub use serde;
pub use serde_json;

/// The manifest value model as seen from Rust. The serde data model is the
/// codec: any `Serialize`/`Deserialize` type crosses the boundary by building
/// this tree, honoring the author's serde attributes as the one canonical
/// projection.
pub type Value = serde_json::Value;

/// Calls another runtime through the bridge and returns the raw string result.
///
/// This is the synchronous bridge call. From inside async Rust it still works
/// (the host bridge is re-entrant), but prefer [`call_async`] there: it
/// suspends the future and lets the golden thread exit the tokio park before
/// the bridge call runs, which is what keeps deep mutual-recursion chains
/// sequential instead of nested.
pub fn call(runtime: &str, code: &str) -> Result<String, OmniError> {
    abi::bridge_call(runtime, code)
}

/// Calls another runtime and deserializes the result into `T` via serde.
pub fn call_typed<T: serde::de::DeserializeOwned>(runtime: &str, code: &str) -> Result<T, OmniError> {
    let raw = abi::bridge_call(runtime, code)?;
    // Bridge results are JSON when they decode as JSON, plain strings otherwise.
    if let Ok(v) = serde_json::from_str::<T>(&raw) {
        return Ok(v);
    }
    serde_json::from_value(serde_json::Value::String(raw))
        .map_err(|e| OmniError::msg(format!("call_typed: cannot decode bridge result: {e}")))
}

/// The async bridge hop (design step 2c): enqueue the outbound call, suspend
/// on a oneshot, and let the drive loop exit the park so the host performs the
/// call as plain dispatcher work. Falls back to the synchronous bridge when no
/// drive loop is consuming the outbound queue.
pub async fn call_async(runtime: &str, code: &str) -> Result<String, OmniError> {
    rt::bridge_call_async(runtime, code).await
}

/// Calls a peer-registered function (Go-registered functions, Rust unit
/// exports, manifest builtins in the host registry) through the typed
/// omni_value_t lane: scalar args and results cross with no JSON text.
///
/// ```ignore
/// use omnivm::ToOmniValue;
/// let out = omnivm::call_typed_fn("go", "boost", &[7i64.to_omni(), "x".to_omni()])?;
/// let n = i64::from_omni(&out)?;
/// ```
///
/// Non-scalar results still arrive losslessly (as `OMNI_TAG_JSON` text —
/// decode with [`OmniValue::to_json_value`]); targets the host does not speak
/// typed for (manifest func_defs, python/js/ruby callables) transparently
/// ride the JSON `{func,args}` / `call_ref` lane instead. From async context
/// prefer [`call_typed_fn_async`].
pub fn call_typed_fn(runtime: &str, func: &str, args: &[OmniValue]) -> Result<OmniValue, OmniError> {
    if let Some(result) = abi::typed_bridge_call(runtime, func, args) {
        return result.map(normalize_typed_result);
    }
    let raw = abi::bridge_call("__manifest", &typed_fn_request(runtime, func, args))?;
    Ok(abi::json_to_omni(interop::decode_bridge_result(&raw)?))
}

/// JSON-tagged trampoline results carry the bridge's enveloped wire shape
/// (`{"__omnivm_result__":true,"value":...}`); unwrap it exactly like the
/// JSON lane so both paths surface the same value.
fn normalize_typed_result(value: OmniValue) -> OmniValue {
    if value.tag != abi::OMNI_TAG_JSON {
        return value;
    }
    let projected = value.to_json_value();
    let unwrapped = if projected.get("__omnivm_result__") == Some(&serde_json::Value::Bool(true)) {
        projected.get("value").cloned().unwrap_or(serde_json::Value::Null)
    } else {
        projected
    };
    abi::json_to_omni(unwrapped)
}

/// [`call_typed_fn`] from async context: rides the bridge hop (the park exits
/// before the host services the call), so the wire is the JSON `{func,args}`
/// envelope — the typed omni_value_t crossing is the synchronous trampoline.
/// Outside the runtime this is exactly `call_typed_fn`.
pub async fn call_typed_fn_async(
    runtime: &str,
    func: &str,
    args: &[OmniValue],
) -> Result<OmniValue, OmniError> {
    if tokio::runtime::Handle::try_current().is_err() {
        return call_typed_fn(runtime, func, args);
    }
    let request = typed_fn_request(runtime, func, args);
    let raw = rt::bridge_call_async("__manifest", &request).await?;
    Ok(abi::json_to_omni(interop::decode_bridge_result(&raw)?))
}

/// The JSON-lane projection of a typed call: `{func,args}` for host-registry
/// targets, the structured `call_ref` op for named-runtime callables.
fn typed_fn_request(runtime: &str, func: &str, args: &[OmniValue]) -> String {
    let json_args: Vec<serde_json::Value> = args.iter().map(OmniValue::to_json_value).collect();
    let request = if runtime.is_empty() || runtime == "__manifest" || runtime == "go" {
        serde_json::json!({"func": func, "args": json_args})
    } else {
        serde_json::json!({"op": "call_ref", "runtime": runtime, "var": func, "args": json_args})
    };
    request.to_string()
}

/// Registers a stateful object in the process-global handle table and returns
/// a value that crosses the boundary as an object proxy (`owned_handle`).
/// Peers invoke methods across calls; lifetime follows the host handle-table
/// discipline (scope cleanup, idempotent release, watchdog teardown).
pub fn export_object(export: ObjectExport) -> objects::ObjectHandleRef {
    objects::register(export)
}
