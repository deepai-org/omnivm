//! The versioned C ABI between the host and the support dylib.
//!
//! Symbols are versioned (`*_v1`) and the host refuses to load an artifact
//! whose symbol version it does not speak — a structured load error, not a
//! crash later. The bridge ABI revision is part of the artifact cache key, so
//! a host upgrade invalidates incompatible artifacts instead of silently
//! loading them. All of this is only sound under the same-toolchain
//! invariant: every Rust artifact in a process is built by the image's pinned
//! rustc and prelude lockfile.

use std::ffi::{c_char, c_void, CStr};
use std::sync::atomic::{AtomicUsize, Ordering};

use crate::envelope::Envelope;
use crate::error::OmniError;

/// Bridge ABI revision. Bump on any change to the extern contract; the host
/// embeds this in the artifact cache key.
pub const ABI_REV: i32 = 1;

// Host-provided bridge function pointers (OmniCall / OmniFree shapes).
pub type BridgeCallFn = unsafe extern "C" fn(*const c_char, *const c_char) -> *mut c_char;
pub type BridgeFreeFn = unsafe extern "C" fn(*mut c_char);

static BRIDGE_CALL: AtomicUsize = AtomicUsize::new(0);
static BRIDGE_FREE: AtomicUsize = AtomicUsize::new(0);

extern "C" {
    fn malloc(n: usize) -> *mut c_void;
    fn free(p: *mut c_void);
}

/// Copies a string into a malloc'd, NUL-terminated buffer the host frees with
/// plain free(). (CString uses Rust's allocator; this contract is allocator-
/// agnostic by construction.)
pub fn to_c_owned(s: &str) -> *mut c_char {
    unsafe {
        let n = s.len();
        let p = malloc(n + 1) as *mut u8;
        if p.is_null() {
            return std::ptr::null_mut();
        }
        std::ptr::copy_nonoverlapping(s.as_ptr(), p, n);
        *p.add(n) = 0;
        p as *mut c_char
    }
}

fn c_str<'a>(p: *const c_char) -> &'a str {
    if p.is_null() {
        return "";
    }
    unsafe { CStr::from_ptr(p) }.to_str().unwrap_or("")
}

/// Synchronous bridge call into the host.
pub fn bridge_call(runtime: &str, code: &str) -> Result<String, OmniError> {
    let call = BRIDGE_CALL.load(Ordering::Acquire);
    if call == 0 {
        return Err(OmniError::msg("omnivm bridge is not initialized"));
    }
    let call: BridgeCallFn = unsafe { std::mem::transmute(call) };
    let c_runtime =
        std::ffi::CString::new(runtime).map_err(|e| OmniError::msg(format!("bridge: {e}")))?;
    let c_code = std::ffi::CString::new(code).map_err(|e| OmniError::msg(format!("bridge: {e}")))?;
    let raw = unsafe { call(c_runtime.as_ptr(), c_code.as_ptr()) };
    if raw.is_null() {
        return Ok(String::new());
    }
    let out = c_str(raw).to_string();
    let free = BRIDGE_FREE.load(Ordering::Acquire);
    if free != 0 {
        let free: BridgeFreeFn = unsafe { std::mem::transmute(free) };
        unsafe { free(raw) };
    }
    match out.strip_prefix("ERR:") {
        Some(message) => Err(OmniError::msg(message.to_string())),
        None => Ok(out),
    }
}

pub fn bridge_installed() -> bool {
    BRIDGE_CALL.load(Ordering::Acquire) != 0
}

// ---------------------------------------------------------------------------
// Versioned exports
// ---------------------------------------------------------------------------

/// Installs the host bridge. Called once on the support dylib; unit shims
/// forward here so the statics are process-global either way.
#[no_mangle]
pub extern "C" fn omnivm_set_bridge_v1(call: BridgeCallFn, free: BridgeFreeFn) {
    BRIDGE_CALL.store(call as usize, Ordering::Release);
    BRIDGE_FREE.store(free as usize, Ordering::Release);
    crate::logging::install();
}

#[no_mangle]
pub extern "C" fn omnivm_abi_version_v1() -> i32 {
    ABI_REV
}

/// One dispatcher-cycle pump. Returns a malloc'd JSON string (may carry
/// outbound bridge calls drained from async tasks).
#[no_mangle]
pub extern "C" fn omnivm_rs_pump_v1() -> *mut c_char {
    let out = std::panic::catch_unwind(crate::rt::pump_to_json)
        .unwrap_or_else(|p| {
            format!(
                "{{\"error\":{}}}",
                serde_json::Value::String(format!("panic in pump: {}", crate::error::panic_message(p)))
            )
        });
    to_c_owned(&out)
}

/// One park of the re-park loop for the await handle. Returns malloc'd JSON:
/// `{"done":true,"envelope":{...}}` or
/// `{"pending":true,"reason":"heartbeat"|"taskfd"|"bridge"}`, either with an
/// optional `"bridge_calls":[{id,runtime,code}]` array to service.
#[no_mangle]
pub extern "C" fn omnivm_rs_drive_v1(handle: u64, slice_ms: u64, task_fd: i32) -> *mut c_char {
    let out = std::panic::catch_unwind(|| crate::rt::drive_to_json(handle, slice_ms, task_fd))
        .unwrap_or_else(|p| {
            let env = Envelope::err(&OmniError::msg(format!(
                "panic while driving rust future: {}",
                crate::error::panic_message(p)
            )));
            format!("{{\"done\":true,\"envelope\":{}}}", env.to_json())
        });
    to_c_owned(&out)
}

/// Releases an abandoned await handle (drop is tokio cancellation).
#[no_mangle]
pub extern "C" fn omnivm_rs_release_future_v1(handle: u64) -> i32 {
    if crate::rt::release_future(handle) {
        1
    } else {
        0
    }
}

/// Completes an outbound bridge request surfaced by drive/pump.
/// ok != 0 means payload is the result; ok == 0 means payload is the error.
#[no_mangle]
pub extern "C" fn omnivm_rs_complete_bridge_v1(id: u64, ok: i32, payload: *const c_char) -> i32 {
    let text = c_str(payload).to_string();
    let result = if ok != 0 { Ok(text) } else { Err(text) };
    if crate::rt::complete_bridge(id, result) {
        1
    } else {
        0
    }
}

/// Selects the executor (0 = current-thread, 1 = multi). Load-time only.
#[no_mangle]
pub extern "C" fn omnivm_rs_set_executor_v1(mode: i32) -> i32 {
    let ok = crate::rt::set_executor(if mode == 1 {
        crate::rt::EXECUTOR_MULTI
    } else {
        crate::rt::EXECUTOR_CURRENT_THREAD
    });
    if ok {
        1
    } else {
        0
    }
}

/// The completion eventfd for `executor = "multi"` (delivered into the
/// dispatcher epoll); -1 on the default executor.
#[no_mangle]
pub extern "C" fn omnivm_rs_completion_fd_v1() -> i32 {
    crate::rt::completion_fd()
}

/// Diagnostics for tests and the doctor: pending futures, live objects, etc.
#[no_mangle]
pub extern "C" fn omnivm_rs_stats_v1() -> *mut c_char {
    to_c_owned(&crate::rt::stats_json())
}

/// Converts a stored future into a background task on the LocalSet (`go
/// expr` semantics: progress on pump ticks and during other parks).
#[no_mangle]
pub extern "C" fn omnivm_rs_spawn_background_v1(handle: u64) -> i32 {
    let ok = std::panic::catch_unwind(|| crate::rt::spawn_background(handle)).unwrap_or(false);
    if ok {
        1
    } else {
        0
    }
}

/// Runs a synchronous unit export on the blocking pool (`go expr` for sync
/// fns); returns an await handle, or 0 on panic.
#[no_mangle]
pub extern "C" fn omnivm_rs_spawn_blocking_v1(fn_ptr: u64, args_json: *const c_char) -> u64 {
    let args = c_str(args_json).to_string();
    std::panic::catch_unwind(|| crate::rt::spawn_blocking_call(fn_ptr as usize, args)).unwrap_or(0)
}

/// Object handle ops for the support dylib's own table (units forward here).
#[no_mangle]
pub extern "C" fn OmniVMHandleOp(payload: *mut c_char) -> *mut c_char {
    let env = std::panic::catch_unwind(|| crate::objects::handle_op(c_str(payload)))
        .unwrap_or_else(|p| {
            Envelope::err(&OmniError::msg(format!(
                "panic in handle op: {}",
                crate::error::panic_message(p)
            )))
        });
    to_c_owned(&env.to_json())
}

#[no_mangle]
pub extern "C" fn OmniVMReleaseObject(id: *mut c_char) -> *mut c_char {
    crate::objects::release(c_str(id));
    to_c_owned("{\"ok\":true}")
}

#[no_mangle]
pub extern "C" fn OmniVMReleaseBuffer(id: *mut c_char) -> *mut c_char {
    // Releases exported C-Data shells (and, by the Drop discipline, their
    // buffers when the consumer never imported them). Quiet and idempotent.
    if let Ok(n) = c_str(id).trim().parse::<u64>() {
        crate::cdata::release_shells(n);
    }
    to_c_owned("{\"ok\":true}")
}

// ---------------------------------------------------------------------------
// Invoke helpers used by the export macros
// ---------------------------------------------------------------------------

/// Parses the host's JSON args array.
pub fn parse_args(args_json: *mut c_char) -> Result<Vec<serde_json::Value>, OmniError> {
    let raw = c_str(args_json);
    if raw.trim().is_empty() {
        return Ok(Vec::new());
    }
    match serde_json::from_str::<serde_json::Value>(raw) {
        Ok(serde_json::Value::Array(items)) => Ok(items),
        Ok(serde_json::Value::Null) => Ok(Vec::new()),
        Ok(other) => Ok(vec![other]),
        Err(e) => Err(OmniError::msg(format!("decode args: {e}"))),
    }
}

/// Positional argument extraction; the target type comes from the user fn's
/// signature via inference, decoded through serde. Arrow IPC markers are
/// lifted to the polars serde projection first, so DataFrame parameters work
/// transparently.
pub fn arg<T: serde::de::DeserializeOwned>(args: &[serde_json::Value], index: usize) -> Result<T, OmniError> {
    let value = args.get(index).cloned().unwrap_or(serde_json::Value::Null);
    if let Some(decoded) = crate::cdata::decode_cdata_marker(&value) {
        let table_value = decoded.map_err(OmniError::msg)?;
        return serde_json::from_value(table_value)
            .map_err(|e| OmniError::msg(format!("argument {index} (arrow cdata): {e}")));
    }
    if let Some(decoded) = crate::table::decode_ipc_marker(&value) {
        let table_value = decoded.map_err(OmniError::msg)?;
        return serde_json::from_value(table_value)
            .map_err(|e| OmniError::msg(format!("argument {index} (arrow table): {e}")));
    }
    if let Some(decoded) = crate::cdata::decode_bytes_marker(&value) {
        // A bytes marker reaching a json-kind param (hand-written shim):
        // lift to a serde byte array so `Vec<u8>`-shaped targets still work.
        let bytes = decoded.map_err(OmniError::msg)?;
        let lifted =
            serde_json::Value::Array(bytes.into_iter().map(serde_json::Value::from).collect());
        return serde_json::from_value(lifted)
            .map_err(|e| OmniError::msg(format!("argument {index} (bytes): {e}")));
    }
    serde_json::from_value(value)
        .map_err(|e| OmniError::msg(format!("argument {index}: {e}")))
}

/// Owned byte-payload extraction for `bytes`-kind params (`Vec<u8>`/`&[u8]`
/// signatures): pointer markers copy once out of the producer's pinned
/// buffer; base64 markers/strings and plain JSON number arrays decode too
/// (hand-written manifests).
pub fn arg_bytes(args: &[serde_json::Value], index: usize) -> Result<Vec<u8>, OmniError> {
    let value = args.get(index).cloned().unwrap_or(serde_json::Value::Null);
    if let Some(decoded) = crate::cdata::decode_bytes_marker(&value) {
        return decoded.map_err(|e| OmniError::msg(format!("argument {index} (bytes): {e}")));
    }
    if let Some(text) = value.as_str() {
        use base64::Engine;
        return base64::engine::general_purpose::STANDARD
            .decode(text)
            .map_err(|e| OmniError::msg(format!("argument {index} (bytes): base64: {e}")));
    }
    serde_json::from_value(value)
        .map_err(|e| OmniError::msg(format!("argument {index} (bytes): {e}")))
}

/// Direct DataFrame extraction for `df`-kind params: Arrow C-Data markers
/// import by pointer (zero-copy), IPC markers decode without the serde
/// projection, plain values fall back to serde.
pub fn arg_dataframe(args: &[serde_json::Value], index: usize) -> Result<polars::prelude::DataFrame, OmniError> {
    let value = args.get(index).cloned().unwrap_or(serde_json::Value::Null);
    if let Some(obj) = value.as_object() {
        if obj.get(crate::cdata::ARROW_CDATA_KEY) == Some(&serde_json::Value::Bool(true)) {
            let parse = |key: &str| -> Option<u64> {
                obj.get(key).and_then(|v| {
                    v.as_u64().or_else(|| v.as_str().and_then(|s| s.parse().ok()))
                })
            };
            let (Some(schema_ptr), Some(array_ptr)) = (parse("schema"), parse("array")) else {
                return Err(OmniError::msg("arrow cdata marker missing pointers"));
            };
            return crate::cdata::import_dataframe_cdata(schema_ptr, array_ptr).map_err(OmniError::msg);
        }
        if obj.contains_key(crate::table::ARROW_IPC_KEY) {
            return crate::table::decode_ipc_dataframe(&value).map_err(OmniError::msg);
        }
    }
    serde_json::from_value(value)
        .map_err(|e| OmniError::msg(format!("argument {index} (dataframe): {e}")))
}

/// Runs an export body under catch_unwind, encoding the outcome as the
/// envelope. A stray `.unwrap()` becomes a structured RuntimeError, never a
/// worker abort (aborts, by contrast, taint the worker — by design).
pub fn invoke_enveloped<F>(body: F) -> *mut c_char
where
    F: FnOnce() -> Result<Envelope, OmniError>,
{
    let env = match std::panic::catch_unwind(std::panic::AssertUnwindSafe(body)) {
        Ok(Ok(env)) => env,
        Ok(Err(err)) => Envelope::err(&err),
        Err(panic) => {
            let mut message = format!("panic: {}", crate::error::panic_message(panic));
            if std::env::var("RUST_BACKTRACE").map(|v| v != "0").unwrap_or(false) {
                message.push('\n');
                message.push_str(&std::backtrace::Backtrace::force_capture().to_string());
            }
            Envelope::err(&OmniError::msg(message))
        }
    };
    to_c_owned(&env.to_json())
}

/// Autoref-specialization tokens converting a user fn's return value into the
/// envelope value: by-value impl for `Result<T, E>` (the structured error
/// envelope), autoref fallback for plain `T: Serialize`.
pub struct OutcomeToken<T>(pub Option<T>);

pub trait ResultOutcome {
    fn omni_outcome(self) -> Result<serde_json::Value, OmniError>;
}

impl<T: serde::Serialize, E: std::fmt::Display> ResultOutcome for OutcomeToken<Result<T, E>> {
    fn omni_outcome(mut self) -> Result<serde_json::Value, OmniError> {
        match self.0.take().expect("outcome token") {
            Ok(value) => serde_json::to_value(value)
                .map_err(|e| OmniError::msg(format!("encode result: {e}"))),
            // anyhow-style errors print "msg" plainly and
            // "msg: cause1: cause2" with `{:#}` — the delta reconstructs a
            // structured cause chain generically (autoref specialization
            // cannot fire here: E is a generic Display bound).
            Err(err) => {
                let plain = format!("{err}");
                let alt = format!("{err:#}");
                let chain: Vec<String> = if alt.len() > plain.len() && alt.starts_with(plain.as_str()) {
                    alt[plain.len()..]
                        .trim_start_matches(": ")
                        .split(": ")
                        .map(str::to_string)
                        .collect()
                } else {
                    Vec::new()
                };
                Err(OmniError { message: plain, chain })
            }
        }
    }
}

pub trait PlainOutcome {
    fn omni_outcome(self) -> Result<serde_json::Value, OmniError>;
}

impl<T: serde::Serialize> PlainOutcome for &mut OutcomeToken<T> {
    fn omni_outcome(self) -> Result<serde_json::Value, OmniError> {
        serde_json::to_value(self.0.take().expect("outcome token"))
            .map_err(|e| OmniError::msg(format!("encode result: {e}")))
    }
}


// ---------------------------------------------------------------------------
// omni_value_t typed lane: scalar-shaped exports also get a typed entry that
// crosses without JSON text. Layout mirrors the host's omni_value_t
// (pkg/javascript/v8_bridge.h). Strings are malloc-allocated; the receiver
// frees (same contract as the envelope lane).
// ---------------------------------------------------------------------------

pub const OMNI_TAG_NULL: i64 = 0;
pub const OMNI_TAG_BOOL: i64 = 1;
pub const OMNI_TAG_I64: i64 = 2;
pub const OMNI_TAG_F64: i64 = 3;
pub const OMNI_TAG_STRING: i64 = 4;
pub const OMNI_TAG_ERROR: i64 = 7;
/// Outbound-lane extension (set_typed_bridge_v1 contract only): a non-scalar
/// result crosses losslessly as its compact JSON text instead of failing the
/// call. The inbound export lane never produces this tag.
pub const OMNI_TAG_JSON: i64 = 8;

#[repr(C)]
#[derive(Clone, Copy)]
pub struct OmniString {
    pub ptr: *mut c_char,
    pub len: i64,
}

#[repr(C)]
#[derive(Clone, Copy)]
pub union OmniPayload {
    pub i: i64,
    pub f: f64,
    pub s: OmniString,
    pub r: u64,
}

#[repr(C)]
pub struct OmniValue {
    pub tag: i64,
    pub v: OmniPayload,
}

// SAFETY: the only non-Send/Sync member is the string payload pointer, an
// exclusively-owned malloc'd buffer (no shared mutable state, no thread
// affinity). Needed so `&[OmniValue]` args can be held across an await in
// Send-registered async exports.
unsafe impl Send for OmniValue {}
unsafe impl Sync for OmniValue {}

impl OmniValue {
    pub fn null() -> Self {
        OmniValue { tag: OMNI_TAG_NULL, v: OmniPayload { i: 0 } }
    }

    pub fn string(text: &str) -> Self {
        let buf = unsafe { malloc(text.len() + 1) } as *mut c_char;
        unsafe {
            std::ptr::copy_nonoverlapping(text.as_ptr(), buf as *mut u8, text.len());
            *buf.add(text.len()) = 0;
        }
        OmniValue { tag: OMNI_TAG_STRING, v: OmniPayload { s: OmniString { ptr: buf, len: text.len() as i64 } } }
    }

    pub fn error(message: &str) -> Self {
        let mut value = Self::string(message);
        value.tag = OMNI_TAG_ERROR;
        value
    }

    /// A non-scalar value as its compact JSON text (outbound-lane tag).
    pub fn json_text(text: &str) -> Self {
        let mut value = Self::string(text);
        value.tag = OMNI_TAG_JSON;
        value
    }

    fn string_payload(&self) -> Option<&str> {
        if !matches!(self.tag, OMNI_TAG_STRING | OMNI_TAG_ERROR | OMNI_TAG_JSON) {
            return None;
        }
        let s = unsafe { self.v.s };
        if s.ptr.is_null() {
            return Some("");
        }
        let bytes = unsafe { std::slice::from_raw_parts(s.ptr as *const u8, s.len as usize) };
        std::str::from_utf8(bytes).ok()
    }

    /// Projects any tag into the manifest value model. JSON-tagged payloads
    /// parse; an unparseable payload degrades to its raw text.
    pub fn to_json_value(&self) -> serde_json::Value {
        match self.tag {
            OMNI_TAG_BOOL => serde_json::Value::Bool(unsafe { self.v.i } != 0),
            OMNI_TAG_I64 => serde_json::Value::from(unsafe { self.v.i }),
            OMNI_TAG_F64 => serde_json::Value::from(unsafe { self.v.f }),
            OMNI_TAG_STRING | OMNI_TAG_ERROR => {
                serde_json::Value::String(self.string_payload().unwrap_or("").to_string())
            }
            OMNI_TAG_JSON => {
                let text = self.string_payload().unwrap_or("");
                serde_json::from_str(text)
                    .unwrap_or_else(|_| serde_json::Value::String(text.to_string()))
            }
            _ => serde_json::Value::Null,
        }
    }
}

/// String payloads are owned: dropping an OmniValue frees its malloc'd
/// buffer. Inbound export shims never drop host-owned values (args are
/// borrowed; results are written with `ptr::write`), so the Drop only fires
/// for crate/user-constructed values — which is exactly what makes
/// `call_typed_fn(&[OmniValue])` leak-free without a manual free step.
impl Drop for OmniValue {
    fn drop(&mut self) {
        if matches!(self.tag, OMNI_TAG_STRING | OMNI_TAG_ERROR | OMNI_TAG_JSON) {
            let s = unsafe { self.v.s };
            if !s.ptr.is_null() {
                unsafe { free(s.ptr as *mut c_void) };
            }
            self.tag = OMNI_TAG_NULL;
            self.v = OmniPayload { i: 0 };
        }
    }
}

/// Lifts a manifest value into the typed lane: scalars take their native
/// tags, anything structured rides as JSON text (lossless).
pub fn json_to_omni(value: serde_json::Value) -> OmniValue {
    match value {
        serde_json::Value::Null => OmniValue::null(),
        serde_json::Value::Bool(b) => OmniValue { tag: OMNI_TAG_BOOL, v: OmniPayload { i: b as i64 } },
        serde_json::Value::Number(n) => {
            if let Some(i) = n.as_i64() {
                OmniValue { tag: OMNI_TAG_I64, v: OmniPayload { i } }
            } else {
                OmniValue { tag: OMNI_TAG_F64, v: OmniPayload { f: n.as_f64().unwrap_or(0.0) } }
            }
        }
        serde_json::Value::String(s) => OmniValue::string(&s),
        other => OmniValue::json_text(&other.to_string()),
    }
}

/// Conversion from a typed argument — the impl target is the EXPORTED FN'S
/// declared parameter type, so inference is forward (never from the call).
pub trait FromOmniValue: Sized {
    fn from_omni(value: &OmniValue) -> Result<Self, String>;
}

impl FromOmniValue for i64 {
    fn from_omni(value: &OmniValue) -> Result<Self, String> {
        match value.tag {
            OMNI_TAG_I64 => Ok(unsafe { value.v.i }),
            OMNI_TAG_F64 => Ok(unsafe { value.v.f } as i64),
            OMNI_TAG_BOOL => Ok(unsafe { value.v.i }),
            _ => Err(format!("expected i64, got tag {}", value.tag)),
        }
    }
}

impl FromOmniValue for f64 {
    fn from_omni(value: &OmniValue) -> Result<Self, String> {
        match value.tag {
            OMNI_TAG_F64 => Ok(unsafe { value.v.f }),
            OMNI_TAG_I64 => Ok(unsafe { value.v.i } as f64),
            _ => Err(format!("expected f64, got tag {}", value.tag)),
        }
    }
}

impl FromOmniValue for bool {
    fn from_omni(value: &OmniValue) -> Result<Self, String> {
        match value.tag {
            OMNI_TAG_BOOL | OMNI_TAG_I64 => Ok(unsafe { value.v.i } != 0),
            _ => Err(format!("expected bool, got tag {}", value.tag)),
        }
    }
}

impl FromOmniValue for String {
    fn from_omni(value: &OmniValue) -> Result<Self, String> {
        if value.tag != OMNI_TAG_STRING {
            return Err(format!("expected string, got tag {}", value.tag));
        }
        let s = unsafe { value.v.s };
        let bytes = unsafe { std::slice::from_raw_parts(s.ptr as *const u8, s.len as usize) };
        String::from_utf8(bytes.to_vec()).map_err(|e| e.to_string())
    }
}

pub trait ToOmniValue {
    fn to_omni(self) -> OmniValue;
}

impl ToOmniValue for i64 {
    fn to_omni(self) -> OmniValue {
        OmniValue { tag: OMNI_TAG_I64, v: OmniPayload { i: self } }
    }
}

impl ToOmniValue for f64 {
    fn to_omni(self) -> OmniValue {
        OmniValue { tag: OMNI_TAG_F64, v: OmniPayload { f: self } }
    }
}

impl ToOmniValue for bool {
    fn to_omni(self) -> OmniValue {
        OmniValue { tag: OMNI_TAG_BOOL, v: OmniPayload { i: self as i64 } }
    }
}

impl ToOmniValue for String {
    fn to_omni(self) -> OmniValue {
        OmniValue::string(&self)
    }
}

impl ToOmniValue for &str {
    fn to_omni(self) -> OmniValue {
        OmniValue::string(self)
    }
}

// ---------------------------------------------------------------------------
// Typed OUTBOUND bridge (rust → host): scalar calls to peer-registered
// functions cross as omni_value_t with no JSON text — CallTypedByAddr's
// encoding in reverse. The host installs a trampoline through the versioned
// `omnivm_rs_set_typed_bridge_v1` export (mirroring omnivm_set_bridge_v1).
//
// Ownership: argument strings stay RUST-owned (borrowed for the duration of
// the call; the host copies) — the mirror of the inbound lane, where the host
// frees its own arg strings after the call. The RESULT string is malloc'd by
// the host and freed by the crate (OmniValue's Drop).
// ---------------------------------------------------------------------------

/// rc contract: 0 = ok (out is the result), 1 = error (out is the error
/// message), 2 = unhandled (nothing executed; caller rides the JSON lane).
pub type TypedBridgeFn =
    unsafe extern "C" fn(*const c_char, *const c_char, *const OmniValue, i32, *mut OmniValue) -> i32;

pub const TYPED_BRIDGE_OK: i32 = 0;
pub const TYPED_BRIDGE_ERR: i32 = 1;
pub const TYPED_BRIDGE_FALLBACK: i32 = 2;

static TYPED_BRIDGE: AtomicUsize = AtomicUsize::new(0);

/// Installs the host's typed outbound trampoline. Called once on the support
/// dylib; process-global like the JSON bridge statics.
#[no_mangle]
pub extern "C" fn omnivm_rs_set_typed_bridge_v1(call: TypedBridgeFn) {
    TYPED_BRIDGE.store(call as usize, Ordering::Release);
}

pub fn typed_bridge_installed() -> bool {
    TYPED_BRIDGE.load(Ordering::Acquire) != 0
}

/// One typed outbound call. `None` means the typed lane is unavailable for
/// this target (uninstalled, or the host reported fallback) and NOTHING was
/// executed — the caller falls back to the JSON lane.
pub fn typed_bridge_call(
    runtime: &str,
    func: &str,
    args: &[OmniValue],
) -> Option<Result<OmniValue, OmniError>> {
    let call = TYPED_BRIDGE.load(Ordering::Acquire);
    if call == 0 {
        return None;
    }
    let call: TypedBridgeFn = unsafe { std::mem::transmute(call) };
    let c_runtime = match std::ffi::CString::new(runtime) {
        Ok(s) => s,
        Err(e) => return Some(Err(OmniError::msg(format!("typed bridge: {e}")))),
    };
    let c_func = match std::ffi::CString::new(func) {
        Ok(s) => s,
        Err(e) => return Some(Err(OmniError::msg(format!("typed bridge: {e}")))),
    };
    let mut out = OmniValue::null();
    let rc = unsafe {
        call(c_runtime.as_ptr(), c_func.as_ptr(), args.as_ptr(), args.len() as i32, &mut out)
    };
    match rc {
        TYPED_BRIDGE_OK => Some(Ok(out)),
        TYPED_BRIDGE_FALLBACK => None,
        _ => {
            let message = out
                .string_payload()
                .filter(|m| !m.is_empty())
                .map(str::to_string)
                .unwrap_or_else(|| format!("typed bridge call failed (rc={rc})"));
            Some(Err(OmniError::msg(message)))
        }
    }
}
