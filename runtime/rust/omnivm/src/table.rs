//! Arrow tabular interchange: the `table` lane for Rust.
//!
//! Tabular data crosses the boundary as Arrow (the universal tabular type for
//! all guests, per the bridge plan's "no library-specific fast paths" rule).
//! The wire form here is an Arrow IPC stream in an in-band marker; the
//! decode/encode happens inside this crate so user functions take and return
//! plain `polars::prelude::DataFrame` values — `arg()` lifts the marker into
//! a DataFrame via polars' serde representation, and returned DataFrames are
//! lowered back to the marker before crossing.

use base64::Engine;
use polars::prelude::*;
use std::io::Cursor;

pub const ARROW_IPC_KEY: &str = "__omnivm_arrow_ipc_b64__";

fn b64() -> base64::engine::GeneralPurpose {
    base64::engine::general_purpose::STANDARD
}

/// If the value is an Arrow IPC marker, decode it into the serde
/// representation of a polars DataFrame so `serde_json::from_value::<DataFrame>`
/// (or any compatible type) succeeds.
pub fn decode_ipc_marker(value: &serde_json::Value) -> Option<Result<serde_json::Value, String>> {
    let obj = value.as_object()?;
    let payload = obj.get(ARROW_IPC_KEY)?.as_str()?;
    Some(decode_ipc_payload(payload))
}

/// Decodes an IPC marker straight to a DataFrame (no serde projection).
pub fn decode_ipc_dataframe(value: &serde_json::Value) -> Result<DataFrame, String> {
    let payload = value
        .as_object()
        .and_then(|o| o.get(ARROW_IPC_KEY))
        .and_then(|v| v.as_str())
        .ok_or_else(|| "not an arrow ipc marker".to_string())?;
    let bytes = b64()
        .decode(payload)
        .map_err(|e| format!("arrow ipc: base64 decode: {e}"))?;
    IpcStreamReader::new(Cursor::new(bytes))
        .finish()
        .map_err(|e| format!("arrow ipc: read: {e}"))
}

fn decode_ipc_payload(payload: &str) -> Result<serde_json::Value, String> {
    let bytes = b64()
        .decode(payload)
        .map_err(|e| format!("arrow ipc: base64 decode: {e}"))?;
    let df = IpcStreamReader::new(Cursor::new(bytes))
        .finish()
        .map_err(|e| format!("arrow ipc: read: {e}"))?;
    serde_json::to_value(&df).map_err(|e| format!("arrow ipc: project: {e}"))
}

/// If a returned value is a serde-projected polars DataFrame, lower it to the
/// Arrow IPC marker so it crosses as a table rather than a giant JSON tree.
pub fn maybe_encode_dataframe(value: serde_json::Value) -> serde_json::Value {
    if !looks_like_dataframe(&value) {
        return value;
    }
    let df: DataFrame = match serde_json::from_value(value.clone()) {
        Ok(df) => df,
        Err(_) => return value,
    };
    match encode_dataframe(df) {
        Ok(marker) => marker,
        Err(_) => value,
    }
}

pub fn encode_dataframe(mut df: DataFrame) -> Result<serde_json::Value, String> {
    let mut buf = Vec::new();
    IpcStreamWriter::new(&mut buf)
        .finish(&mut df)
        .map_err(|e| format!("arrow ipc: write: {e}"))?;
    let payload = b64().encode(&buf);
    let mut obj = serde_json::Map::new();
    obj.insert(ARROW_IPC_KEY.to_string(), serde_json::Value::String(payload));
    obj.insert("rows".to_string(), serde_json::Value::from(df.height() as u64));
    Ok(serde_json::Value::Object(obj))
}

// polars' serde representation of a DataFrame is its own Arrow IPC byte
// stream, so a serde-projected frame appears as a JSON array of bytes
// starting with the IPC continuation marker 0xFFFFFFFF. Only candidates
// matching that prefix get the (validating) from_value attempt above.
fn looks_like_dataframe(value: &serde_json::Value) -> bool {
    let Some(items) = value.as_array() else {
        return false;
    };
    if items.len() < 8 {
        return false;
    }
    items[..4].iter().all(|b| b.as_u64() == Some(255))
        && items.iter().take(64).all(|b| matches!(b.as_u64(), Some(0..=255)))
}
