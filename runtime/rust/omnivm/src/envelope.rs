//! The result envelope shared with the host. Field names mirror the Go
//! `cSharedPluginEnvelope` JSON tags exactly, so the existing host-side
//! decode path (values, owned handles, owned buffers, typed slices) works
//! unchanged for Rust units.

use serde::Serialize;

use crate::error::OmniError;

pub const BOUNDARY_OWNED_HANDLE: &str = "owned_handle";
pub const BOUNDARY_OWNED_BUFFER: &str = "owned_buffer";
/// Rust-only boundary: an async fn's stored future, driven by the host's
/// re-park loop through `omnivm_rs_drive_v1`.
pub const BOUNDARY_RUST_FUTURE: &str = "rust_future";

#[derive(Serialize, Default)]
pub struct Envelope {
    pub ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub boundary: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub dtype: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub memory_space: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub ownership: Option<String>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub read_only: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub handle_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub kind: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub pointer: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub buffer_id: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub bytes_len: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub elements: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub shape: Option<Vec<i64>>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub strides: Option<Vec<i64>>,
    #[serde(skip_serializing_if = "std::ops::Not::not")]
    pub found: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub value: Option<serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

impl Envelope {
    pub fn ok_value(value: serde_json::Value) -> Envelope {
        // A serde-projected polars DataFrame crosses as an Arrow IPC table.
        let value = crate::table::maybe_encode_dataframe(value);
        // A top-level object-handle marker crosses as an owned_handle proxy.
        if let Some((id, kind)) = crate::objects::handle_marker(&value) {
            return Envelope {
                ok: true,
                boundary: Some(BOUNDARY_OWNED_HANDLE.to_string()),
                handle_id: Some(id),
                kind: Some(kind),
                ..Default::default()
            };
        }
        Envelope { ok: true, value: Some(value), ..Default::default() }
    }

    pub fn ok_found(value: serde_json::Value) -> Envelope {
        let mut env = Envelope::ok_value(value);
        env.found = true;
        env
    }

    pub fn err(err: &OmniError) -> Envelope {
        Envelope { ok: false, error: Some(err.envelope_message()), ..Default::default() }
    }

    pub fn future(handle: u64) -> Envelope {
        Envelope {
            ok: true,
            boundary: Some(BOUNDARY_RUST_FUTURE.to_string()),
            handle_id: Some(handle.to_string()),
            kind: Some("future".to_string()),
            ..Default::default()
        }
    }

    pub fn to_json(&self) -> String {
        serde_json::to_string(self).unwrap_or_else(|e| {
            format!("{{\"ok\":false,\"error\":\"envelope encode: {}\"}}", e)
        })
    }
}
