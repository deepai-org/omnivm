//! Rust as a proxy *consumer*: peer-language callables and manifest channels
//! arrive as in-band descriptors and wrap into ordinary Rust values.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};

use serde::de::Error as DeError;
use serde::{Deserialize, Deserializer};

use crate::error::OmniError;

/// A peer-language callable (`Box<dyn Fn>` morally): deserializes from the
/// `__omnivm_runtime_ref__` marker, so crates that take closures integrate —
/// wrap `cb` in a closure capturing it. Calls hop through the bridge; from
/// async context use [`Callback::call_async`] (the park exits before the
/// peer code runs).
#[derive(Clone, Debug)]
pub struct Callback {
    pub runtime: String,
    pub var: String,
}

impl<'de> Deserialize<'de> for Callback {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let value = serde_json::Value::deserialize(deserializer)?;
        let obj = value
            .as_object()
            .filter(|o| o.get("__omnivm_runtime_ref__") == Some(&serde_json::Value::Bool(true)))
            .ok_or_else(|| D::Error::custom("expected an __omnivm_runtime_ref__ callable marker"))?;
        let runtime = obj.get("runtime").and_then(|v| v.as_str()).unwrap_or_default();
        let var = obj.get("var").and_then(|v| v.as_str()).unwrap_or_default();
        if runtime.is_empty() || var.is_empty() {
            return Err(D::Error::custom("runtime ref marker missing runtime/var"));
        }
        Ok(Callback { runtime: runtime.to_string(), var: var.to_string() })
    }
}

impl Callback {
    /// Builds the structured `call_ref` bridge op: the HOST resolves `var` in
    /// the target runtime and performs one canonical invocation with
    /// already-decoded args. No source synthesis here, no JSON re-decode in
    /// the peer (the old `f(*json.loads('...'))` double encode is gone).
    fn call_ref_request(&self, args: &[serde_json::Value]) -> Result<String, OmniError> {
        serde_json::to_string(&serde_json::json!({
            "op": "call_ref",
            "runtime": self.runtime,
            "var": self.var,
            "args": args,
        }))
        .map_err(|e| OmniError::msg(format!("callback args: {e}")))
    }

    /// Result contract is unchanged from the source-synthesis era: callers
    /// receive a string (and typically `.parse()` it). String results cross
    /// as their contents; other values as their compact JSON text.
    fn decode_call_result(raw: &str) -> Result<String, OmniError> {
        let value = decode_bridge_result(raw)?;
        Ok(match value {
            serde_json::Value::Null => String::new(),
            serde_json::Value::String(s) => s,
            other => other.to_string(),
        })
    }

    /// Synchronous invocation (fine from sync Rust; from inside async code
    /// prefer [`Callback::call_async`]).
    pub fn call(&self, args: &[serde_json::Value]) -> Result<String, OmniError> {
        let raw = crate::abi::bridge_call("__manifest", &self.call_ref_request(args)?)?;
        Self::decode_call_result(&raw)
    }

    /// Async invocation through the bridge hop: the future suspends, the
    /// golden thread exits the park, the peer runs, the future resumes.
    pub async fn call_async(&self, args: &[serde_json::Value]) -> Result<String, OmniError> {
        let request = self.call_ref_request(args)?;
        let raw = crate::rt::bridge_call_async("__manifest", &request).await?;
        Self::decode_call_result(&raw)
    }
}

/// A manifest channel endpoint: deserializes from the `__omnivm_channel__`
/// (or stream) descriptor. Receives ride the universal stream-pull protocol
/// (`stream_next`); sends go through the manifest `send` builtin. The async
/// variants hop, so a parked Rust await stays cooperative.
#[derive(Clone, Debug)]
pub struct Channel {
    pub id: u64,
    descriptor: serde_json::Value,
    // Receive-side buffer for batched pulls: recv_async asks the host for up
    // to RECV_BATCH_MAX_N values per bridge hop and serves later receives
    // from here with no hop at all. Interior mutability because Channel is
    // used via &self; calls are golden-thread serialized, and clones share
    // the buffer (Arc) so no value can be stranded in an abandoned clone.
    recv_state: Arc<Mutex<ChannelRecvState>>,
}

#[derive(Debug, Default)]
struct ChannelRecvState {
    buffered: VecDeque<serde_json::Value>,
    done: bool,
}

/// Upper bound on values pulled per `stream_next` hop in [`Channel::recv_async`].
const RECV_BATCH_MAX_N: usize = 64;

impl<'de> Deserialize<'de> for Channel {
    fn deserialize<D: Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let value = serde_json::Value::deserialize(deserializer)?;
        let obj = value
            .as_object()
            .filter(|o| {
                o.get("__omnivm_channel__") == Some(&serde_json::Value::Bool(true))
                    || o.get("__omnivm_stream__") == Some(&serde_json::Value::Bool(true))
            })
            .ok_or_else(|| D::Error::custom("expected an __omnivm_channel__/__omnivm_stream__ descriptor"))?;
        let id = obj
            .get("id")
            .and_then(|v| v.as_u64().or_else(|| v.as_str().and_then(|s| s.parse().ok())))
            .ok_or_else(|| D::Error::custom("channel descriptor missing id"))?;
        Ok(Channel { id, descriptor: value, recv_state: Arc::default() })
    }
}

#[derive(Deserialize)]
struct StreamNextResult {
    #[serde(default)]
    done: bool,
    #[serde(default)]
    pending: bool,
    #[serde(default)]
    value: serde_json::Value,
}

enum Next {
    Value(serde_json::Value),
    Pending,
    Done,
}

#[derive(Deserialize)]
struct StreamNextBatchResult {
    #[serde(default)]
    done: bool,
    #[serde(default)]
    pending: bool,
    #[serde(default)]
    values: Vec<serde_json::Value>,
}

enum NextBatch {
    Values { values: Vec<serde_json::Value>, done: bool },
    Pending,
}

enum SendOutcome {
    Sent,
    Full,
    Closed,
}

fn decode_bridge_result(raw: &str) -> Result<serde_json::Value, OmniError> {
    let parsed: serde_json::Value =
        serde_json::from_str(raw).map_err(|e| OmniError::msg(format!("bridge result: {e}")))?;
    if parsed.get("__omnivm_result__") == Some(&serde_json::Value::Bool(true)) {
        return Ok(parsed.get("value").cloned().unwrap_or(serde_json::Value::Null));
    }
    Ok(parsed)
}

impl Channel {
    fn next_request(&self, live: bool) -> String {
        if live {
            // Live consumption: open-but-empty is "pending", not "done".
            format!(
                "{{\"op\":\"stream_next\",\"id\":{},\"mode\":\"values\",\"pending\":true}}",
                self.id
            )
        } else {
            format!("{{\"op\":\"stream_next\",\"id\":{},\"mode\":\"values\"}}", self.id)
        }
    }

    // Batched live pull: up to max_n queued values cross in one response.
    // Hosts that predate max_n ignore the field and answer in the singular
    // shape, which decode_next_batch does NOT accept — but those hosts also
    // never see this request, because the host grows both sides atomically
    // (the support dylib and stubs.go ship together).
    fn next_batch_request(&self) -> String {
        format!(
            "{{\"op\":\"stream_next\",\"id\":{},\"mode\":\"values\",\"pending\":true,\"max_n\":{}}}",
            self.id, RECV_BATCH_MAX_N
        )
    }

    fn send_request(&self, value: &serde_json::Value, wait: bool) -> Result<String, OmniError> {
        serde_json::to_string(&serde_json::json!({
            "func": if wait { "send_wait" } else { "send" },
            "args": [self.descriptor, value],
        }))
        .map_err(|e| OmniError::msg(format!("channel send: {e}")))
    }

    fn decode_send(raw: &str) -> Result<SendOutcome, OmniError> {
        let result = decode_bridge_result(raw)?;
        // send_wait crosses status as a plain string (maps would become
        // resource descriptors at the func-result boundary).
        match result.as_str() {
            Some("sent") => return Ok(SendOutcome::Sent),
            Some("pending") => return Ok(SendOutcome::Full),
            Some("closed") => return Ok(SendOutcome::Closed),
            _ => {}
        }
        // Legacy `send` builtin: plain bool, false = closed-or-full.
        Ok(if result.as_bool().unwrap_or(false) {
            SendOutcome::Sent
        } else {
            SendOutcome::Closed
        })
    }

    fn decode_next(raw: &str) -> Result<Next, OmniError> {
        let result = decode_bridge_result(raw)?;
        let next: StreamNextResult = serde_json::from_value(result)
            .map_err(|e| OmniError::msg(format!("stream_next: {e}")))?;
        Ok(if next.pending {
            Next::Pending
        } else if next.done {
            Next::Done
        } else {
            Next::Value(next.value)
        })
    }

    fn decode_next_batch(raw: &str) -> Result<NextBatch, OmniError> {
        let result = decode_bridge_result(raw)?;
        let batch: StreamNextBatchResult = serde_json::from_value(result)
            .map_err(|e| OmniError::msg(format!("stream_next batch: {e}")))?;
        Ok(if batch.pending {
            NextBatch::Pending
        } else {
            NextBatch::Values { values: batch.values, done: batch.done }
        })
    }

    /// Serves a receive from the local batch buffer, if it can be decided
    /// without a bridge hop: `Some(Some(v))` buffered value, `Some(None)`
    /// drained-and-done, `None` undecided (a hop is needed).
    fn take_buffered(&self) -> Option<Option<serde_json::Value>> {
        let mut state = self.recv_state.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(value) = state.buffered.pop_front() {
            return Some(Some(value));
        }
        if state.done {
            return Some(None);
        }
        None
    }

    /// Non-blocking pull (snapshot semantics): `None` for both "closed and
    /// drained" and "nothing queued right now". Stays a single-value pull on
    /// the wire; values already buffered by a batched recv_async are served
    /// first (otherwise mixing the two would reorder).
    pub fn recv(&self) -> Result<Option<serde_json::Value>, OmniError> {
        if let Some(buffered) = self.take_buffered() {
            return Ok(buffered);
        }
        match Self::decode_next(&crate::abi::bridge_call("__manifest", &self.next_request(false))?)? {
            Next::Value(v) => Ok(Some(v)),
            _ => Ok(None),
        }
    }

    /// Live async pull: waits cooperatively while the channel is open but
    /// momentarily empty (other runtimes feed it between re-parks); `None`
    /// only when the channel is closed and drained. Pulls are batched: one
    /// hop fetches up to [`RECV_BATCH_MAX_N`] queued values and later calls
    /// drain the buffer with no hop.
    pub async fn recv_async(&self) -> Result<Option<serde_json::Value>, OmniError> {
        loop {
            if let Some(buffered) = self.take_buffered() {
                return Ok(buffered);
            }
            let raw = crate::rt::bridge_call_async("__manifest", &self.next_batch_request()).await?;
            match Self::decode_next_batch(&raw)? {
                NextBatch::Values { values, done } => {
                    // Lock scope must close before any await (the future is Send).
                    let idle = {
                        let mut state = self.recv_state.lock().unwrap_or_else(|e| e.into_inner());
                        let empty = values.is_empty();
                        state.buffered.extend(values);
                        if done {
                            state.done = true;
                        }
                        !done && empty
                    };
                    if idle {
                        // Defensive: an empty, not-done, not-pending batch
                        // must not spin the loop hot.
                        tokio::time::sleep(std::time::Duration::from_millis(2)).await;
                    }
                }
                NextBatch::Pending => {
                    tokio::time::sleep(std::time::Duration::from_millis(2)).await;
                }
            }
        }
    }

    /// Non-blocking send: errors honestly when the value did NOT enter the
    /// channel (closed, or full in this non-waiting form).
    pub fn send(&self, value: impl serde::Serialize) -> Result<(), OmniError> {
        let value = serde_json::to_value(value)
            .map_err(|e| OmniError::msg(format!("channel send: {e}")))?;
        let raw = crate::abi::bridge_call("__manifest", &self.send_request(&value, true)?)?;
        match Self::decode_send(&raw)? {
            SendOutcome::Sent => Ok(()),
            SendOutcome::Full => Err(OmniError::msg("channel full (non-blocking send)")),
            SendOutcome::Closed => Err(OmniError::msg("channel closed")),
        }
    }

    /// Async send with backpressure: waits cooperatively while the channel
    /// is full (a consumer drains it between re-parks); errors only when the
    /// channel is closed. Values are never silently dropped.
    pub async fn send_async(&self, value: impl serde::Serialize) -> Result<(), OmniError> {
        let value = serde_json::to_value(value)
            .map_err(|e| OmniError::msg(format!("channel send: {e}")))?;
        let request = self.send_request(&value, true)?;
        loop {
            let raw = crate::rt::bridge_call_async("__manifest", &request).await?;
            match Self::decode_send(&raw)? {
                SendOutcome::Sent => return Ok(()),
                SendOutcome::Closed => return Err(OmniError::msg("channel closed")),
                SendOutcome::Full => {
                    tokio::time::sleep(std::time::Duration::from_millis(2)).await;
                }
            }
        }
    }
}
