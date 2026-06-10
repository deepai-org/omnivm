//! The process-global object handle table: Rust as a proxy *producer*.
//!
//! Stateful, expensive-to-create objects (a `reqwest::Client`, a connection
//! pool, a compiled regex, a loaded model) register here and cross the
//! boundary as `owned_handle` proxies. Peers call methods across calls via
//! the host's `OmniVMHandleOp` protocol (`method_call`/`drop` mirroring the
//! stream proxy's `stream_next`/`stream_cancel`). Because the table lives in
//! the shared support dylib, two separately compiled units can share one pool
//! or model through a handle — cross-unit state without shared Rust statics.

use std::any::Any;
use std::collections::HashMap;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Mutex;

use serde::ser::SerializeMap;
use serde::{Serialize, Serializer};

use crate::error::OmniError;
use crate::envelope::Envelope;

type MethodFn = Box<dyn Fn(&(dyn Any + Send), &[serde_json::Value]) -> Result<serde_json::Value, OmniError> + Send + Sync>;

pub struct ObjectExport {
    kind: String,
    value: Box<dyn Any + Send>,
    methods: HashMap<String, MethodFn>,
}

impl ObjectExport {
    pub fn new<T: Any + Send>(kind: impl Into<String>, value: T) -> Self {
        ObjectExport { kind: kind.into(), value: Box::new(value), methods: HashMap::new() }
    }

    /// Registers a method callable from any peer runtime. The closure
    /// receives the object and JSON-model args; use serde to project.
    pub fn method<T, F>(mut self, name: impl Into<String>, f: F) -> Self
    where
        T: Any + Send,
        F: Fn(&T, &[serde_json::Value]) -> Result<serde_json::Value, OmniError> + Send + Sync + 'static,
    {
        self.methods.insert(
            name.into(),
            Box::new(move |obj, args| {
                let typed = obj
                    .downcast_ref::<T>()
                    .ok_or_else(|| OmniError::msg("object handle: type mismatch in method dispatch"))?;
                f(typed, args)
            }),
        );
        self
    }
}

struct Entry {
    kind: String,
    value: Box<dyn Any + Send>,
    methods: HashMap<String, MethodFn>,
}

static OBJECTS: Mutex<Option<HashMap<String, Entry>>> = Mutex::new(None);
static NEXT_ID: AtomicU64 = AtomicU64::new(1);

/// A serializable reference to a registered object. Serializes to the
/// in-band marker that `Envelope::ok_value` lifts to an `owned_handle`
/// boundary at the top level.
#[derive(Clone)]
pub struct ObjectHandleRef {
    pub id: String,
    pub kind: String,
}

impl Serialize for ObjectHandleRef {
    fn serialize<S: Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        let mut map = serializer.serialize_map(Some(3))?;
        map.serialize_entry("__omnivm_rs_handle__", &true)?;
        map.serialize_entry("id", &self.id)?;
        map.serialize_entry("kind", &self.kind)?;
        map.end()
    }
}

/// Detects the serialized handle marker in a value tree (top level only).
pub fn handle_marker(value: &serde_json::Value) -> Option<(String, String)> {
    let obj = value.as_object()?;
    if obj.get("__omnivm_rs_handle__") != Some(&serde_json::Value::Bool(true)) {
        return None;
    }
    let id = obj.get("id")?.as_str()?.to_string();
    let kind = obj.get("kind").and_then(|k| k.as_str()).unwrap_or("object").to_string();
    Some((id, kind))
}

pub fn register(export: ObjectExport) -> ObjectHandleRef {
    let id = NEXT_ID.fetch_add(1, Ordering::Relaxed).to_string();
    let kind = export.kind.clone();
    let mut guard = OBJECTS.lock().unwrap();
    guard.get_or_insert_with(HashMap::new).insert(
        id.clone(),
        Entry { kind: kind.clone(), value: export.value, methods: export.methods },
    );
    ObjectHandleRef { id, kind }
}

/// Releases an object. Quiet and idempotent, matching the host handle-table
/// discipline (finalizers and scope cleanup may both fire).
pub fn release(id: &str) -> bool {
    let mut guard = OBJECTS.lock().unwrap();
    guard.as_mut().map(|m| m.remove(id).is_some()).unwrap_or(false)
}

pub fn live_count() -> usize {
    OBJECTS.lock().unwrap().as_ref().map(|m| m.len()).unwrap_or(0)
}

/// Dispatches one `OmniVMHandleOp` request payload against the table.
pub fn handle_op(payload: &str) -> Envelope {
    let req: serde_json::Value = match serde_json::from_str(payload) {
        Ok(v) => v,
        Err(e) => return Envelope::err(&OmniError::msg(format!("handle op decode: {e}"))),
    };
    let op = req.get("op").and_then(|v| v.as_str()).unwrap_or("");
    let id = req.get("handle_id").and_then(|v| v.as_str()).unwrap_or("");

    let guard = OBJECTS.lock().unwrap();
    let entry = match guard.as_ref().and_then(|m| m.get(id)) {
        Some(e) => e,
        None => {
            return Envelope::err(&OmniError::msg(format!("object handle {id} is not live")));
        }
    };

    match op {
        "callable" => {
            let key = req.get("key").and_then(|v| v.as_str()).unwrap_or("");
            let mut env = Envelope::ok_value(serde_json::Value::Bool(entry.methods.contains_key(key)));
            env.found = true;
            env
        }
        "get" => {
            // Methods surface as callable attributes; no data fields by default.
            let key = req.get("key").and_then(|v| v.as_str()).unwrap_or("");
            if entry.methods.contains_key(key) {
                let mut env = Envelope::default();
                env.ok = true;
                env.found = false;
                return env;
            }
            Envelope { ok: true, found: false, ..Default::default() }
        }
        "call" => {
            let key = req.get("key").and_then(|v| v.as_str()).unwrap_or("");
            let args: Vec<serde_json::Value> = match req.get("args") {
                Some(serde_json::Value::Array(a)) => a.clone(),
                Some(serde_json::Value::Null) | None => Vec::new(),
                Some(other) => vec![other.clone()],
            };
            let method = match entry.methods.get(key) {
                Some(m) => m,
                None => {
                    return Envelope::err(&OmniError::msg(format!(
                        "object handle {id} ({}) has no method {key:?}",
                        entry.kind
                    )))
                }
            };
            let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                method(entry.value.as_ref(), &args)
            }));
            match result {
                Ok(Ok(value)) => Envelope::ok_found(value),
                Ok(Err(err)) => Envelope::err(&err),
                Err(panic) => Envelope::err(&OmniError::msg(format!(
                    "panic in object method {key:?}: {}",
                    crate::error::panic_message(panic)
                ))),
            }
        }
        "len" | "iter" | "contains" | "index" | "set" | "read" | "close" => {
            Envelope { ok: true, found: false, ..Default::default() }
        }
        other => Envelope::err(&OmniError::msg(format!("unsupported object handle op {other:?}"))),
    }
}
