//! The `log` facade bridge: a large fraction of the crate ecosystem still
//! emits through the older `log` facade and would otherwise go silent inside
//! a unit. Records forward to the worker's stderr in a structured form and
//! are counted in the runtime stats (the seam where richer forwarding into
//! host CallMetrics lands later).

use std::sync::atomic::{AtomicU64, Ordering};

pub static RECORDS_FORWARDED: AtomicU64 = AtomicU64::new(0);

struct OmniLogger;

impl log::Log for OmniLogger {
    fn enabled(&self, _metadata: &log::Metadata) -> bool {
        true
    }

    fn log(&self, record: &log::Record) {
        RECORDS_FORWARDED.fetch_add(1, Ordering::Relaxed);
        eprintln!(
            "[rust:{}] {}: {}",
            record.level().as_str().to_ascii_lowercase(),
            record.target(),
            record.args()
        );
    }

    fn flush(&self) {}
}

static LOGGER: OmniLogger = OmniLogger;

/// Installs the facade once (idempotent; a logger already installed by user
/// code wins quietly).
pub fn install() {
    if log::set_logger(&LOGGER).is_ok() {
        let level = match std::env::var("OMNIVM_RUST_LOG").as_deref() {
            Ok("trace") => log::LevelFilter::Trace,
            Ok("debug") => log::LevelFilter::Debug,
            Ok("warn") => log::LevelFilter::Warn,
            Ok("error") => log::LevelFilter::Error,
            Ok("off") => log::LevelFilter::Off,
            _ => log::LevelFilter::Info,
        };
        log::set_max_level(level);
    }
}

pub fn records_forwarded() -> u64 {
    RECORDS_FORWARDED.load(Ordering::Relaxed)
}
