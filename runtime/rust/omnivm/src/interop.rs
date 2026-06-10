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
    value: serde_json::Value,
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
    fn next_request(&self) -> String {
        format!("{{\"op\":\"stream_next\",\"id\":{},\"mode\":\"values\"}}", self.id)
    }

    fn send_request(&self, value: &serde_json::Value) -> Result<String, OmniError> {
        serde_json::to_string(&serde_json::json!({
            "func": "send",
            "args": [self.descriptor, value],
        }))
        .map_err(|e| OmniError::msg(format!("channel send: {e}")))
    }

    fn decode_next(raw: &str) -> Result<Option<serde_json::Value>, OmniError> {
        let result = decode_bridge_result(raw)?;
        let next: StreamNextResult = serde_json::from_value(result)
            .map_err(|e| OmniError::msg(format!("stream_next: {e}")))?;
        Ok(if next.done { None } else { Some(next.value) })
    }

    /// Pulls the next value; `None` when the channel is closed and drained.
    pub fn recv(&self) -> Result<Option<serde_json::Value>, OmniError> {
        Self::decode_next(&crate::abi::bridge_call("__manifest", &self.next_request())?)
    }

    /// Async pull via the bridge hop; pending values arrive as other
    /// runtimes feed the channel between re-parks.
    pub async fn recv_async(&self) -> Result<Option<serde_json::Value>, OmniError> {
        Self::decode_next(&crate::rt::bridge_call_async("__manifest", &self.next_request()).await?)
    }

    /// Non-blocking send through the manifest `send` builtin.
    pub fn send(&self, value: impl serde::Serialize) -> Result<(), OmniError> {
        let value = serde_json::to_value(value)
            .map_err(|e| OmniError::msg(format!("channel send: {e}")))?;
        crate::abi::bridge_call("__manifest", &self.send_request(&value)?)?;
        Ok(())
    }

    /// Async send via the bridge hop.
    pub async fn send_async(&self, value: impl serde::Serialize) -> Result<(), OmniError> {
        let value = serde_json::to_value(value)
            .map_err(|e| OmniError::msg(format!("channel send: {e}")))?;
        crate::rt::bridge_call_async("__manifest", &self.send_request(&value)?).await?;
        Ok(())
    }
}
