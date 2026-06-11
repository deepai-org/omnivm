//! Arrow C Data Interface crossing (Tier-2): same-process pointer handoff of
//! record batches, replacing the base64-IPC copy for the python→rust arg
//! direction. Ownership follows the C Data spec: the importer takes the
//! array by value (the source shell is left empty, so producer-side cleanup
//! is a no-op for moved arrays); the schema is borrowed and stays with the
//! producer.

use polars::prelude::*;
use polars_arrow::array::StructArray;
use polars_arrow::datatypes::{ArrowDataType, Field as ArrowField};
use polars_arrow::ffi;

pub const ARROW_CDATA_KEY: &str = "__omnivm_arrow_cdata__";

/// Detects the C-Data marker: `{"__omnivm_arrow_cdata__": true,
/// "schema": "<addr>", "array": "<addr>"}` (addresses as decimal strings).
pub fn decode_cdata_marker(value: &serde_json::Value) -> Option<Result<serde_json::Value, String>> {
    let obj = value.as_object()?;
    if obj.get(ARROW_CDATA_KEY) != Some(&serde_json::Value::Bool(true)) {
        return None;
    }
    let parse = |key: &str| -> Option<u64> {
        obj.get(key).and_then(|v| {
            v.as_u64()
                .or_else(|| v.as_str().and_then(|s| s.parse().ok()))
        })
    };
    let (Some(schema_ptr), Some(array_ptr)) = (parse("schema"), parse("array")) else {
        return Some(Err("arrow cdata marker missing schema/array pointers".to_string()));
    };
    Some(import_dataframe_cdata(schema_ptr, array_ptr).and_then(|df| {
        serde_json::to_value(&df).map_err(|e| format!("arrow cdata: project: {e}"))
    }))
}

/// Imports a record batch (exported as a struct array) into a DataFrame.
/// Takes ownership of the array per the C Data contract; the schema shell
/// stays with the producer.
pub fn import_dataframe_cdata(schema_ptr: u64, array_ptr: u64) -> Result<DataFrame, String> {
    if schema_ptr == 0 || array_ptr == 0 {
        return Err("arrow cdata: null pointer".to_string());
    }
    unsafe {
        let schema_ref: &ffi::ArrowSchema = &*(schema_ptr as *const ffi::ArrowSchema);
        let field = ffi::import_field_from_c(schema_ref)
            .map_err(|e| format!("arrow cdata: schema import: {e}"))?;
        // Move the array out, leaving an empty (released) shell behind.
        let array = std::ptr::replace(array_ptr as *mut ffi::ArrowArray, ffi::ArrowArray::empty());
        let boxed = ffi::import_array_from_c(array, field.dtype.clone())
            .map_err(|e| format!("arrow cdata: array import: {e}"))?;
        let struct_array = boxed
            .as_any()
            .downcast_ref::<StructArray>()
            .ok_or_else(|| "arrow cdata: top-level array is not a struct (record batch)".to_string())?;

        let ArrowDataType::Struct(fields) = field.dtype() else {
            return Err("arrow cdata: schema is not a struct".to_string());
        };
        let child_fields: &[ArrowField] = fields;
        let child_arrays: &[Box<dyn polars_arrow::array::Array>] = struct_array.values();
        let mut columns = Vec::with_capacity(child_arrays.len());
        for (child_field, child_array) in child_fields.iter().zip(child_arrays.iter()) {
            let series = Series::from_arrow_chunks(
                child_field.name.clone().into(),
                vec![child_array.clone()],
            )
            .map_err(|e| format!("arrow cdata: column {}: {e}", child_field.name))?;
            columns.push(series.into_column());
        }
        DataFrame::new(columns).map_err(|e| format!("arrow cdata: dataframe: {e}"))
    }
}

/// Returns the raw data-buffer address of a numeric column (testing aid for
/// the pointer-identity guarantee).
pub fn column_buffer_address(df: &DataFrame, name: &str) -> Result<u64, String> {
    let series = df.column(name).map_err(|e| e.to_string())?.as_materialized_series();
    let chunk = series
        .chunks()
        .first()
        .ok_or_else(|| "empty column".to_string())?;
    if let Some(arr) = chunk
        .as_any()
        .downcast_ref::<polars_arrow::array::PrimitiveArray<f64>>()
    {
        return Ok(arr.values().as_ptr() as u64);
    }
    if let Some(arr) = chunk
        .as_any()
        .downcast_ref::<polars_arrow::array::PrimitiveArray<i64>>()
    {
        return Ok(arr.values().as_ptr() as u64);
    }
    Err(format!("column {name} is not a primitive f64/i64 array"))
}

// ---------------------------------------------------------------------------
// Export direction (rust → peer): returned DataFrames cross as C-Data pointer
// handoffs. The exported shells (ArrowSchema/ArrowArray boxes) are registered
// under a buffer id; the host releases them after the consumer imports
// (Drop is a no-op for moved/consumed structs, frees buffers otherwise —
// the failure path the C Data spec requires).
// ---------------------------------------------------------------------------

use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

static SHELLS: Mutex<Option<HashMap<u64, (usize, usize)>>> = Mutex::new(None);
static NEXT_SHELL_ID: AtomicU64 = AtomicU64::new(1);

/// Exports a DataFrame as a single record batch behind C-Data pointers,
/// returning the in-band marker. The shells stay alive until
/// [`release_shells`] (normally via the host's OmniVMReleaseBuffer call).
pub fn export_dataframe_cdata(df: DataFrame) -> Result<serde_json::Value, String> {
    let mut df = df;
    df.as_single_chunk_par();
    let rows = df.height();
    let arrow_schema = df.schema().to_arrow(CompatLevel::newest());
    let chunk = df
        .iter_chunks(CompatLevel::newest(), true)
        .next()
        .ok_or_else(|| "arrow cdata export: empty frame chunking".to_string())?;
    let arrays = chunk.into_arrays();
    let fields: Vec<ArrowField> = arrow_schema.iter_values().cloned().collect();
    let dtype = ArrowDataType::Struct(fields);
    let struct_array = StructArray::new(dtype.clone(), rows, arrays, None);
    let field = ArrowField::new("".into(), dtype, false);

    let schema_ptr = Box::into_raw(Box::new(ffi::export_field_to_c(&field))) as usize;
    let array_ptr = Box::into_raw(Box::new(ffi::export_array_to_c(Box::new(struct_array)))) as usize;

    let id = NEXT_SHELL_ID.fetch_add(1, Ordering::Relaxed);
    SHELLS
        .lock()
        .unwrap()
        .get_or_insert_with(HashMap::new)
        .insert(id, (schema_ptr, array_ptr));

    Ok(serde_json::json!({
        ARROW_CDATA_KEY: true,
        "schema": schema_ptr.to_string(),
        "array": array_ptr.to_string(),
        "buffer_id": id.to_string(),
        "rows": rows as u64,
    }))
}

/// Releases an exported pair's shells. Quiet and idempotent; Drop runs each
/// struct's embedded release callback only if the consumer never took it.
pub fn release_shells(id: u64) -> bool {
    let entry = SHELLS.lock().unwrap().as_mut().and_then(|m| m.remove(&id));
    match entry {
        Some((schema_ptr, array_ptr)) => {
            unsafe {
                drop(Box::from_raw(schema_ptr as *mut ffi::ArrowSchema));
                drop(Box::from_raw(array_ptr as *mut ffi::ArrowArray));
            }
            true
        }
        None => release_byte_buffer(id),
    }
}

pub fn live_shell_count() -> usize {
    SHELLS.lock().unwrap().as_ref().map(|m| m.len()).unwrap_or(0)
}

// ---------------------------------------------------------------------------
// Opaque byte buffers: a single large Vec<u8> (a model file, an image) that
// would otherwise base64 through JSON crosses as a pointer + length marker,
// owned by this registry until the host releases it (same OmniVMReleaseBuffer
// id space as the C-Data shells).
// ---------------------------------------------------------------------------

pub const BYTES_KEY: &str = "__omnivm_bytes__";

static BYTE_BUFFERS: Mutex<Option<HashMap<u64, Box<[u8]>>>> = Mutex::new(None);

/// A byte payload that crosses by pointer when the host has opted into the
/// pointer lane, and as base64 otherwise. Wrap large `Vec<u8>` returns:
/// `omnivm::cdata::Bytes(data)`.
pub struct Bytes(pub Vec<u8>);

impl serde::Serialize for Bytes {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        use serde::ser::SerializeMap;
        if std::env::var("OMNIVM_ARROW_CDATA_RETURN").as_deref() == Ok("1") {
            let boxed: Box<[u8]> = self.0.clone().into_boxed_slice();
            let ptr = boxed.as_ptr() as usize;
            let len = boxed.len();
            let id = NEXT_SHELL_ID.fetch_add(1, Ordering::Relaxed);
            BYTE_BUFFERS
                .lock()
                .unwrap()
                .get_or_insert_with(HashMap::new)
                .insert(id, boxed);
            let mut map = serializer.serialize_map(Some(4))?;
            map.serialize_entry(BYTES_KEY, &true)?;
            map.serialize_entry("ptr", &ptr.to_string())?;
            map.serialize_entry("len", &(len as u64))?;
            map.serialize_entry("buffer_id", &id.to_string())?;
            return map.end();
        }
        use base64::Engine;
        let mut map = serializer.serialize_map(Some(2))?;
        map.serialize_entry(BYTES_KEY, &true)?;
        map.serialize_entry(
            "b64",
            &base64::engine::general_purpose::STANDARD.encode(&self.0),
        )?;
        map.end()
    }
}

/// Detects the inbound bytes marker and decodes it to owned data. Two wire
/// forms: `{"__omnivm_bytes__": true, "ptr": "<addr>", "len": "<n>", ...}`
/// (the pointer lane — one copy into an owned Vec, the producer's keep-alive
/// is released after the call) and `{"__omnivm_bytes__": true, "b64": "..."}`
/// (the producer's no-ctypes fallback).
pub fn decode_bytes_marker(value: &serde_json::Value) -> Option<Result<Vec<u8>, String>> {
    let obj = value.as_object()?;
    if obj.get(BYTES_KEY) != Some(&serde_json::Value::Bool(true)) {
        return None;
    }
    if let Some(b64) = obj.get("b64").and_then(|v| v.as_str()) {
        use base64::Engine;
        return Some(
            base64::engine::general_purpose::STANDARD
                .decode(b64)
                .map_err(|e| format!("bytes marker: base64: {e}")),
        );
    }
    let parse = |key: &str| -> Option<u64> {
        obj.get(key).and_then(|v| {
            v.as_u64()
                .or_else(|| v.as_str().and_then(|s| s.parse().ok()))
        })
    };
    let (Some(ptr), Some(len)) = (parse("ptr"), parse("len")) else {
        return Some(Err("bytes marker missing ptr/len".to_string()));
    };
    if len == 0 {
        return Some(Ok(Vec::new()));
    }
    if ptr == 0 {
        return Some(Err(format!("bytes marker: null pointer with len {len}")));
    }
    Some(Ok(unsafe {
        std::slice::from_raw_parts(ptr as *const u8, len as usize)
    }
    .to_vec()))
}

pub(crate) fn release_byte_buffer(id: u64) -> bool {
    BYTE_BUFFERS
        .lock()
        .unwrap()
        .as_mut()
        .map(|m| m.remove(&id).is_some())
        .unwrap_or(false)
}

pub fn live_byte_buffer_count() -> usize {
    BYTE_BUFFERS.lock().unwrap().as_ref().map(|m| m.len()).unwrap_or(0)
}
