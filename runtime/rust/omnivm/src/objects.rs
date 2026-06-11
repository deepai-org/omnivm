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
type GetterFn = Box<dyn Fn(&(dyn Any + Send)) -> Result<serde_json::Value, OmniError> + Send + Sync>;
type ViewFn = Box<dyn Fn(&(dyn Any + Send)) -> serde_json::Value + Send + Sync>;

pub struct ObjectExport {
    kind: String,
    value: Box<dyn Any + Send>,
    methods: HashMap<String, MethodFn>,
    getters: HashMap<String, GetterFn>,
    view: Option<ViewFn>,
}

impl ObjectExport {
    pub fn new<T: Any + Send>(kind: impl Into<String>, value: T) -> Self {
        ObjectExport {
            kind: kind.into(),
            value: Box::new(value),
            methods: HashMap::new(),
            getters: HashMap::new(),
            view: None,
        }
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

    /// Registers a readable property (peer-side attribute access).
    pub fn getter<T, F>(mut self, name: impl Into<String>, f: F) -> Self
    where
        T: Any + Send,
        F: Fn(&T) -> Result<serde_json::Value, OmniError> + Send + Sync + 'static,
    {
        self.getters.insert(
            name.into(),
            Box::new(move |obj| {
                let typed = obj
                    .downcast_ref::<T>()
                    .ok_or_else(|| OmniError::msg("object handle: type mismatch in getter dispatch"))?;
                f(typed)
            }),
        );
        self
    }

    /// Registers a data projection used for index/len/iter/contains: the
    /// closure returns the object's current value-model view (array or map).
    pub fn view<T, F>(mut self, f: F) -> Self
    where
        T: Any + Send,
        F: Fn(&T) -> serde_json::Value + Send + Sync + 'static,
    {
        self.view = Some(Box::new(move |obj| {
            obj.downcast_ref::<T>().map(&f).unwrap_or(serde_json::Value::Null)
        }));
        self
    }
}

struct Entry {
    kind: String,
    value: Box<dyn Any + Send>,
    methods: HashMap<String, MethodFn>,
    getters: HashMap<String, GetterFn>,
    view: Option<ViewFn>,
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

type BoxedValueIter = std::sync::Mutex<Box<dyn Iterator<Item = serde_json::Value> + Send>>;

/// Exports an iterator as a stream proxy: peers pull through the existing
/// `stream_next`/`stream_cancel` protocol (the host adapts the handle's
/// `next` method into a manifest stream). Lazy and cancellable: dropping the
/// handle (release) drops the iterator.
pub fn export_stream<I>(iter: I) -> ObjectHandleRef
where
    I: Iterator + Send + 'static,
    I::Item: serde::Serialize,
{
    let boxed: Box<dyn Iterator<Item = serde_json::Value> + Send> = Box::new(
        iter.map(|item| serde_json::to_value(item).unwrap_or(serde_json::Value::Null)),
    );
    register(
        ObjectExport::new("stream", std::sync::Mutex::new(boxed) as BoxedValueIter).method(
            "next",
            |cell: &BoxedValueIter, _args| {
                Ok(match cell.lock().unwrap().next() {
                    Some(value) => serde_json::json!({"done": false, "value": value}),
                    None => serde_json::json!({"done": true}),
                })
            },
        ),
    )
}

type BoxedValueStream =
    std::sync::Mutex<std::pin::Pin<Box<dyn futures_core::Stream<Item = serde_json::Value> + Send>>>;

/// Exports an async `Stream` as a stream proxy. Pulls block on the runtime
/// (timers/IO progress) when called outside a drive; inside a drive a
/// not-ready stream reports `pending` (live consumers retry, snapshot
/// consumers stop) rather than deadlocking the golden thread.
pub fn export_async_stream<S>(stream: S) -> ObjectHandleRef
where
    S: futures_core::Stream + Send + 'static,
    S::Item: serde::Serialize,
{
    use futures_core::Stream;
    let mapped = Box::pin(MapSerialize { inner: stream });
    let boxed: std::pin::Pin<Box<dyn Stream<Item = serde_json::Value> + Send>> = mapped;
    register(
        ObjectExport::new("stream", std::sync::Mutex::new(boxed) as BoxedValueStream).method(
            "next",
            |cell: &BoxedValueStream, _args| {
                let mut guard = cell.lock().unwrap();
                match crate::rt::block_on_stream_next(guard.as_mut()) {
                    Ok(Some(value)) => Ok(serde_json::json!({"done": false, "value": value})),
                    Ok(None) => Ok(serde_json::json!({"done": true})),
                    Err(crate::rt::StreamPollWouldBlock) => {
                        Ok(serde_json::json!({"done": false, "pending": true}))
                    }
                }
            },
        ),
    )
}

struct MapSerialize<S> {
    inner: S,
}

impl<S> futures_core::Stream for MapSerialize<S>
where
    S: futures_core::Stream,
    S::Item: serde::Serialize,
{
    type Item = serde_json::Value;

    fn poll_next(
        self: std::pin::Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        // SAFETY: structural projection of the only field.
        let inner = unsafe { self.map_unchecked_mut(|s| &mut s.inner) };
        inner.poll_next(cx).map(|opt| {
            opt.map(|item| serde_json::to_value(item).unwrap_or(serde_json::Value::Null))
        })
    }
}

pub fn register(export: ObjectExport) -> ObjectHandleRef {
    let id = NEXT_ID.fetch_add(1, Ordering::Relaxed).to_string();
    let kind = export.kind.clone();
    let mut guard = OBJECTS.lock().unwrap();
    guard.get_or_insert_with(HashMap::new).insert(
        id.clone(),
        Entry {
            kind: kind.clone(),
            value: export.value,
            methods: export.methods,
            getters: export.getters,
            view: export.view,
        },
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
            let key = req.get("key").and_then(|v| v.as_str()).unwrap_or("");
            if let Some(getter) = entry.getters.get(key) {
                return match getter(entry.value.as_ref()) {
                    Ok(value) => Envelope::ok_found(value),
                    Err(err) => Envelope::err(&err),
                };
            }
            // Map-shaped views expose their fields as attributes too.
            if let Some(view) = &entry.view {
                if let serde_json::Value::Object(map) = view(entry.value.as_ref()) {
                    if let Some(value) = map.get(key) {
                        return Envelope::ok_found(value.clone());
                    }
                }
            }
            Envelope { ok: true, found: false, ..Default::default() }
        }
        "index" => {
            let Some(view) = &entry.view else {
                return Envelope { ok: true, found: false, ..Default::default() };
            };
            let key = req.get("value").cloned().unwrap_or(serde_json::Value::Null);
            let data = view(entry.value.as_ref());
            let found = match (&data, &key) {
                (serde_json::Value::Array(items), serde_json::Value::Number(n)) => {
                    let idx = n.as_i64().unwrap_or(-1);
                    let len = items.len() as i64;
                    let idx = if idx < 0 { len + idx } else { idx };
                    if idx >= 0 && idx < len { Some(items[idx as usize].clone()) } else { None }
                }
                (serde_json::Value::Object(map), serde_json::Value::String(s)) => map.get(s).cloned(),
                _ => None,
            };
            match found {
                Some(value) => Envelope::ok_found(value),
                None => Envelope { ok: true, found: false, ..Default::default() },
            }
        }
        "len" => {
            let Some(view) = &entry.view else {
                return Envelope { ok: true, found: false, ..Default::default() };
            };
            let n = match view(entry.value.as_ref()) {
                serde_json::Value::Array(items) => Some(items.len()),
                serde_json::Value::Object(map) => Some(map.len()),
                serde_json::Value::String(s) => Some(s.chars().count()),
                _ => None,
            };
            match n {
                Some(n) => Envelope::ok_found(serde_json::Value::from(n as u64)),
                None => Envelope { ok: true, found: false, ..Default::default() },
            }
        }
        "iter" => {
            let Some(view) = &entry.view else {
                return Envelope { ok: true, found: false, ..Default::default() };
            };
            let mode = req.get("mode").and_then(|v| v.as_str()).unwrap_or("values");
            let items = match (view(entry.value.as_ref()), mode) {
                (serde_json::Value::Array(items), _) => Some(items),
                (serde_json::Value::Object(map), "keys") => {
                    Some(map.keys().map(|k| serde_json::Value::String(k.clone())).collect())
                }
                (serde_json::Value::Object(map), "items") => Some(
                    map.into_iter()
                        .map(|(k, v)| serde_json::Value::Array(vec![serde_json::Value::String(k), v]))
                        .collect(),
                ),
                (serde_json::Value::Object(map), _) => Some(map.into_values().collect()),
                _ => None,
            };
            match items {
                Some(items) => Envelope::ok_found(serde_json::Value::Array(items)),
                None => Envelope { ok: true, found: false, ..Default::default() },
            }
        }
        "contains" => {
            let Some(view) = &entry.view else {
                return Envelope { ok: true, found: false, ..Default::default() };
            };
            let needle = req.get("value").cloned().unwrap_or(serde_json::Value::Null);
            let found = match view(entry.value.as_ref()) {
                serde_json::Value::Array(items) => items.contains(&needle),
                serde_json::Value::Object(map) => {
                    needle.as_str().map(|s| map.contains_key(s)).unwrap_or(false)
                }
                _ => false,
            };
            Envelope::ok_found(serde_json::Value::Bool(found))
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
        "set" | "read" | "close" => {
            Envelope { ok: true, found: false, ..Default::default() }
        }
        other => Envelope::err(&OmniError::msg(format!("unsupported object handle op {other:?}"))),
    }
}
