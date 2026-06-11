//! Rust as a proxy *consumer*: peer-language callables and manifest channels
//! arrive as in-band descriptors and wrap into ordinary Rust values.

use serde::de::Error as DeError;
use serde::{Deserialize, Deserializer};

use crate::error::OmniError;

fn json_escape_into_lang_literal(payload: &str) -> String {
    // A JSON document embedded as a double-quoted literal valid in Python,
    // JavaScript, and Ruby alike: escape backslashes and double quotes.
    let mut out = String::with_capacity(payload.len() + 2);
    out.push('"');
    for c in payload.chars() {
        match c {
            '\\' => out.push_str("\\\\"),
            '"' => out.push_str("\\\""),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            _ => out.push(c),
        }
    }
    out.push('"');
    out
}

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
    fn invocation_expr(&self, args: &[serde_json::Value]) -> Result<String, OmniError> {
        let payload = serde_json::to_string(args)
            .map_err(|e| OmniError::msg(format!("callback args: {e}")))?;
        let literal = json_escape_into_lang_literal(&payload);
        Ok(match self.runtime.as_str() {
            "python" => format!("{}(*__import__(\"json\").loads({literal}))", self.var),
            "javascript" => format!("{}(...JSON.parse({literal}))", self.var),
            "ruby" => format!("(require \"json\"; {}(*JSON.parse({literal})))", self.var),
            // Go peer functions: the host's go eval branch parses
            // name(arg, ...) invocations of registered functions directly.
            "go" => {
                let args_src = args
                    .iter()
                    .map(|a| serde_json::to_string(a).unwrap_or_else(|_| "null".into()))
                    .collect::<Vec<_>>()
                    .join(", ");
                format!("{}({args_src})", self.var)
            }
            other => return Err(OmniError::msg(format!("callback: unsupported runtime {other:?}"))),
        })
    }

    /// Synchronous invocation (fine from sync Rust; from inside async code
    /// prefer [`Callback::call_async`]).
    pub fn call(&self, args: &[serde_json::Value]) -> Result<String, OmniError> {
        crate::abi::bridge_call(&self.runtime, &self.invocation_expr(args)?)
    }

    /// Async invocation through the bridge hop: the future suspends, the
    /// golden thread exits the park, the peer runs, the future resumes.
    pub async fn call_async(&self, args: &[serde_json::Value]) -> Result<String, OmniError> {
        let expr = self.invocation_expr(args)?;
        crate::rt::bridge_call_async(&self.runtime, &expr).await
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
}

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
        Ok(Channel { id, descriptor: value })
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

    /// Non-blocking pull (snapshot semantics): `None` for both "closed and
    /// drained" and "nothing queued right now".
    pub fn recv(&self) -> Result<Option<serde_json::Value>, OmniError> {
        match Self::decode_next(&crate::abi::bridge_call("__manifest", &self.next_request(false))?)? {
            Next::Value(v) => Ok(Some(v)),
            _ => Ok(None),
        }
    }

    /// Live async pull: waits cooperatively while the channel is open but
    /// momentarily empty (other runtimes feed it between re-parks); `None`
    /// only when the channel is closed and drained.
    pub async fn recv_async(&self) -> Result<Option<serde_json::Value>, OmniError> {
        loop {
            let raw = crate::rt::bridge_call_async("__manifest", &self.next_request(true)).await?;
            match Self::decode_next(&raw)? {
                Next::Value(v) => return Ok(Some(v)),
                Next::Done => return Ok(None),
                Next::Pending => {
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
