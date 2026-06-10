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
