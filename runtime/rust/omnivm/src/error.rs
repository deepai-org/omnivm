//! Structured error envelope support: panics and `Err` returns become
//! host-visible RuntimeErrors, never worker aborts. `anyhow`/`thiserror`
//! errors walk their `source()` chains into the cause chain.

use std::fmt;

/// An error crossing the Rust boundary. `chain` holds `source()` causes in
/// order, outermost first.
#[derive(Debug, Clone)]
pub struct OmniError {
    pub message: String,
    pub chain: Vec<String>,
}

impl OmniError {
    pub fn msg(message: impl Into<String>) -> Self {
        OmniError { message: message.into(), chain: Vec::new() }
    }

    /// Flattens the cause chain into the single envelope error string the
    /// host understands today (the envelope error field is flat).
    pub fn envelope_message(&self) -> String {
        if self.chain.is_empty() {
            return self.message.clone();
        }
        let mut out = self.message.clone();
        for cause in &self.chain {
            out.push_str("\ncaused by: ");
            out.push_str(cause);
        }
        out
    }

    pub fn from_std_error(err: &(dyn std::error::Error + 'static)) -> Self {
        let mut chain = Vec::new();
        let mut cur = err.source();
        while let Some(c) = cur {
            chain.push(c.to_string());
            cur = c.source();
        }
        OmniError { message: err.to_string(), chain }
    }
}

impl fmt::Display for OmniError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{}", self.envelope_message())
    }
}

impl std::error::Error for OmniError {}

impl From<String> for OmniError {
    fn from(s: String) -> Self {
        OmniError::msg(s)
    }
}

impl From<&str> for OmniError {
    fn from(s: &str) -> Self {
        OmniError::msg(s)
    }
}

/// Autoref-specialization tokens so the export macros can convert arbitrary
/// error types without (unstable) specialization: `ErrToken(e).to_omni_error()`
/// resolves to the by-value impl when `E: std::error::Error` (full cause
/// chain), and falls back through autoref to the `Display` impl otherwise.
pub struct ErrToken<E>(pub E);

pub trait StdErrorToOmni {
    fn to_omni_error(self) -> OmniError;
}

impl<E: std::error::Error + 'static> StdErrorToOmni for ErrToken<E> {
    fn to_omni_error(self) -> OmniError {
        OmniError::from_std_error(&self.0)
    }
}

pub trait DisplayToOmni {
    fn to_omni_error(self) -> OmniError;
}

impl<E: fmt::Display> DisplayToOmni for &ErrToken<E> {
    fn to_omni_error(self) -> OmniError {
        OmniError::msg(self.0.to_string())
    }
}

/// Extracts a printable message from a `catch_unwind` payload.
pub fn panic_message(payload: Box<dyn std::any::Any + Send>) -> String {
    if let Some(s) = payload.downcast_ref::<&str>() {
        return (*s).to_string();
    }
    if let Some(s) = payload.downcast_ref::<String>() {
        return s.clone();
    }
    "unknown panic payload".to_string()
}
