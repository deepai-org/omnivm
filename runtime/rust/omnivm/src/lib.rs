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
pub mod envelope;
pub mod error;
pub mod objects;
pub mod rt;
pub mod logging;
pub mod table;

mod export_macros;

pub use error::OmniError;
pub use objects::ObjectExport;

/// Re-exported so user units and the support library agree on one tokio (the
/// version is pinned by the image, not the user's Cargo.toml — an accepted
/// trade, same as the pinned CPython/Node/JVM versions).
pub use tokio;

/// Re-exported `log` facade (installed automatically at bridge init).
pub use log;

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

/// Registers a stateful object in the process-global handle table and returns
/// a value that crosses the boundary as an object proxy (`owned_handle`).
/// Peers invoke methods across calls; lifetime follows the host handle-table
/// discipline (scope cleanup, idempotent release, watchdog teardown).
pub fn export_object(export: ObjectExport) -> objects::ObjectHandleRef {
    objects::register(export)
}
