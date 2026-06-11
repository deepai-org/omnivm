//! Golden-thread-first tokio (docs/rust-runtime-design.md, "Events / Async").
//!
//! One lazily-initialized current-thread runtime plus a `LocalSet`, owned by
//! the support dylib, living on the golden thread. The unifying principle:
//! the golden thread parks in exactly one reactor at a time, and that reactor
//! watches the other reactors' fds. A manifest `await` on a Rust future is a
//! re-park loop: the host repeatedly calls `omnivm_rs_drive_v1`, which parks
//! in `block_on(select!(...))` with arms for the awaited future, a heartbeat
//! (so the host can pump libuv/asyncio between parks), the dispatcher taskFD
//! (binary mode), and the outbound bridge queue (the async hop).
//!
//! Because the park can exit and resume, the pending future is not a local:
//! it is a stored `Pin<Box<dyn Future>>` keyed by the await handle, polled
//! across multiple `block_on` entries. Dropping the box is tokio
//! cancellation, so no bespoke teardown exists.

use std::cell::RefCell;
use std::collections::HashMap;
use std::future::Future;
use std::os::fd::RawFd;
use std::pin::Pin;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::sync::Mutex;
use std::sync::OnceLock;
use std::task::{Context, Poll, Wake, Waker};
use std::time::{Duration, Instant};

use tokio::io::unix::AsyncFd;
use tokio::io::Interest;
use tokio::runtime::Handle;
use tokio::task::LocalSet;

use crate::error::OmniError;

pub type FutResult = Result<serde_json::Value, OmniError>;
pub type LocalBoxFut = Pin<Box<dyn Future<Output = FutResult>>>;

// ---------------------------------------------------------------------------
// Executor selection: current-thread by default, multi-thread as an explicit
// escalation (`executor = "multi"`). Must be chosen before first use.
// ---------------------------------------------------------------------------

pub const EXECUTOR_CURRENT_THREAD: usize = 0;
pub const EXECUTOR_MULTI: usize = 1;

static EXECUTOR_MODE: AtomicUsize = AtomicUsize::new(EXECUTOR_CURRENT_THREAD);
static RUNTIME: OnceLock<tokio::runtime::Runtime> = OnceLock::new();

/// Selects the executor. Returns false if the runtime already initialized
/// (the knob is load-time only).
pub fn set_executor(mode: usize) -> bool {
    if RUNTIME.get().is_some() {
        return EXECUTOR_MODE.load(Ordering::SeqCst) == mode;
    }
    EXECUTOR_MODE.store(mode, Ordering::SeqCst);
    true
}

pub fn executor_mode() -> usize {
    EXECUTOR_MODE.load(Ordering::SeqCst)
}

fn runtime() -> &'static tokio::runtime::Runtime {
    RUNTIME.get_or_init(|| {
        if executor_mode() == EXECUTOR_MULTI {
            tokio::runtime::Builder::new_multi_thread()
                .enable_all()
                .max_blocking_threads(64)
                .build()
                .expect("omnivm: tokio multi-thread runtime")
        } else {
            // Zero additional threads: blocking-pool threads are lazy and the
            // reactor/timer are driven inline by the golden thread.
            tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .max_blocking_threads(16)
                .build()
                .expect("omnivm: tokio current-thread runtime")
        }
    })
}

thread_local! {
    // The LocalSet permits !Send futures (spawn_local), which is friendlier
    // for generated glue than multi-thread Send + 'static bounds. Golden
    // thread only.
    static LOCAL: LocalSet = LocalSet::new();
    static LOCAL_FUTURES: RefCell<HashMap<u64, LocalBoxFut>> = RefCell::new(HashMap::new());
    static TASK_FDS: RefCell<HashMap<RawFd, AsyncFd<BorrowedTaskFd>>> = RefCell::new(HashMap::new());
}

static SPAWNED: Mutex<Vec<(u64, tokio::task::JoinHandle<FutResult>)>> = Mutex::new(Vec::new());
static NEXT_FUTURE_ID: AtomicU64 = AtomicU64::new(1);

// ---------------------------------------------------------------------------
// Future registration (the await-handle table)
// ---------------------------------------------------------------------------

/// Stores a (possibly !Send) future and returns its await handle. Used by
/// `export_async_fn!` for the default executor.
pub fn register_local_future(fut: LocalBoxFut) -> u64 {
    let id = NEXT_FUTURE_ID.fetch_add(1, Ordering::Relaxed);
    LOCAL_FUTURES.with(|m| m.borrow_mut().insert(id, fut));
    id
}

/// Stores a Send future. Under `executor = "multi"` it is spawned immediately
/// so it makes background progress between drives, with completion delivered
/// on the completion eventfd; otherwise equivalent to local registration.
pub fn register_send_future<F>(fut: F) -> u64
where
    F: Future<Output = FutResult> + Send + 'static,
{
    if executor_mode() == EXECUTOR_MULTI {
        let id = NEXT_FUTURE_ID.fetch_add(1, Ordering::Relaxed);
        let handle = runtime().spawn(async move {
            let result = fut.await;
            signal_completion_fd();
            result
        });
        SPAWNED.lock().unwrap().push((id, handle));
        return id;
    }
    register_local_future(Box::pin(fut))
}

/// Autoref-specialization shim so the export macro registers Send futures via
/// the spawn-capable path and !Send futures locally, without specialization.
pub struct FutToken<F>(pub Option<F>);

pub trait RegisterSendFut {
    fn register(self) -> u64;
}

impl<F: Future<Output = FutResult> + Send + 'static> RegisterSendFut for FutToken<F> {
    fn register(mut self) -> u64 {
        register_send_future(self.0.take().expect("future token"))
    }
}

pub trait RegisterLocalFut {
    fn register(self) -> u64;
}

impl<F: Future<Output = FutResult> + 'static> RegisterLocalFut for &mut FutToken<F> {
    fn register(self) -> u64 {
        register_local_future(Box::pin(self.0.take().expect("future token")))
    }
}

thread_local! {
    // `go expr` tasks on the default executor: spawned onto the LocalSet so
    // they progress on every pump tick and during any park, and resolved by
    // the same drive loop via their JoinHandle.
    static LOCAL_SPAWNED: RefCell<HashMap<u64, tokio::task::JoinHandle<FutResult>>> =
        RefCell::new(HashMap::new());
}

/// Converts a stored (not yet driven) future into a background task: it is
/// spawned onto the LocalSet under the same await handle, so it makes
/// progress whenever the golden thread enters the runtime (pump ticks and
/// other futures' parks) instead of only when itself driven.
pub fn spawn_background(id: u64) -> bool {
    let Some(fut) = LOCAL_FUTURES.with(|m| m.borrow_mut().remove(&id)) else {
        // Multi-executor mode futures are already spawned.
        return SPAWNED.lock().unwrap().iter().any(|(sid, _)| *sid == id);
    };
    if Handle::try_current().is_ok() {
        // Already inside the runtime: spawn_local directly.
        let handle = tokio::task::spawn_local(fut);
        LOCAL_SPAWNED.with(|m| m.borrow_mut().insert(id, handle));
        return true;
    }
    LOCAL.with(|local| {
        let _guard = runtime().enter();
        let handle = local.spawn_local(fut);
        LOCAL_SPAWNED.with(|m| m.borrow_mut().insert(id, handle));
    });
    true
}

/// Runs a synchronous unit export (`char* fn(char*)`) on tokio's blocking
/// pool, parking-free: the `go expr` escalation for sync fns. Returns an
/// await handle resolving to the export's envelope `value`.
pub fn spawn_blocking_call(fn_ptr: usize, args_json: String) -> u64 {
    let id = NEXT_FUTURE_ID.fetch_add(1, Ordering::Relaxed);
    let handle = runtime().spawn(async move {
        let raw = tokio::task::spawn_blocking(move || {
            type ExportFn = unsafe extern "C" fn(*mut std::ffi::c_char) -> *mut std::ffi::c_char;
            let export: ExportFn = unsafe { std::mem::transmute(fn_ptr) };
            let c_args = std::ffi::CString::new(args_json)
                .map_err(|e| OmniError::msg(format!("spawn args: {e}")))?;
            let out = unsafe { export(c_args.as_ptr() as *mut std::ffi::c_char) };
            if out.is_null() {
                return Ok::<String, OmniError>(String::new());
            }
            let s = unsafe { std::ffi::CStr::from_ptr(out) }
                .to_string_lossy()
                .into_owned();
            extern "C" {
                fn free(p: *mut std::ffi::c_void);
            }
            unsafe { free(out as *mut std::ffi::c_void) };
            Ok(s)
        })
        .await
        .map_err(|e| OmniError::msg(format!("spawn_blocking: {e}")))??;
        let env: serde_json::Value = serde_json::from_str(&raw)
            .map_err(|e| OmniError::msg(format!("spawn envelope: {e}")))?;
        if env.get("ok") != Some(&serde_json::Value::Bool(true)) {
            let message = env.get("error").and_then(|v| v.as_str()).unwrap_or("unknown error");
            return Err(OmniError::msg(message.to_string()));
        }
        Ok(env.get("value").cloned().unwrap_or(serde_json::Value::Null))
    });
    SPAWNED.lock().unwrap().push((id, handle));
    signal_completion_fd();
    id
}

/// Releases an abandoned await (watchdog timeout, scope cleanup, manifest
/// error between parks). Dropping the box / aborting the task IS tokio
/// cancellation. Quiet and idempotent.
pub fn release_future(id: u64) -> bool {
    let local = LOCAL_FUTURES.with(|m| m.borrow_mut().remove(&id).is_some());
    if local {
        return true;
    }
    if let Some(handle) = LOCAL_SPAWNED.with(|m| m.borrow_mut().remove(&id)) {
        handle.abort();
        return true;
    }
    let mut spawned = SPAWNED.lock().unwrap();
    if let Some(pos) = spawned.iter().position(|(sid, _)| *sid == id) {
        let (_, handle) = spawned.swap_remove(pos);
        handle.abort();
        return true;
    }
    false
}

pub fn pending_future_count() -> usize {
    LOCAL_FUTURES.with(|m| m.borrow().len())
        + LOCAL_SPAWNED.with(|m| m.borrow().len())
        + SPAWNED.lock().unwrap().len()
}

// ---------------------------------------------------------------------------
// Outbound bridge queue (the async hop, step 2c)
// ---------------------------------------------------------------------------

#[derive(serde::Serialize)]
pub struct BridgeRequest {
    pub id: u64,
    pub runtime: String,
    pub code: String,
}

static OUTBOUND: Mutex<Vec<BridgeRequest>> = Mutex::new(Vec::new());
static PENDING_BRIDGE: Mutex<Vec<(u64, tokio::sync::oneshot::Sender<Result<String, String>>)>> =
    Mutex::new(Vec::new());
static NEXT_BRIDGE_ID: AtomicU64 = AtomicU64::new(1);

fn outbound_notify() -> &'static tokio::sync::Notify {
    static NOTIFY: OnceLock<tokio::sync::Notify> = OnceLock::new();
    NOTIFY.get_or_init(tokio::sync::Notify::new)
}

/// The async bridge hop. The future suspends on a oneshot; the drive loop's
/// outbound arm fires; the park exits; the host runs the call on the golden
/// thread as plain dispatcher work with no active runtime context; the
/// oneshot completes; the loop re-parks. Because the park exits before the
/// bridge call runs, an inner Rust entry is a sequential block_on, not a
/// nested one — which is exactly the distinction that does not panic.
pub async fn bridge_call_async(runtime_name: &str, code: &str) -> Result<String, OmniError> {
    if Handle::try_current().is_err() {
        // Not inside the runtime: plain synchronous bridge call.
        return crate::abi::bridge_call(runtime_name, code);
    }
    let id = NEXT_BRIDGE_ID.fetch_add(1, Ordering::Relaxed);
    let (tx, rx) = tokio::sync::oneshot::channel();
    PENDING_BRIDGE.lock().unwrap().push((id, tx));
    OUTBOUND.lock().unwrap().push(BridgeRequest {
        id,
        runtime: runtime_name.to_string(),
        code: code.to_string(),
    });
    outbound_notify().notify_one();
    match rx.await {
        Ok(Ok(value)) => Ok(value),
        Ok(Err(message)) => Err(OmniError::msg(message)),
        Err(_) => Err(OmniError::msg("bridge hop: host abandoned the call")),
    }
}

/// Completes a previously surfaced outbound bridge request.
pub fn complete_bridge(id: u64, result: Result<String, String>) -> bool {
    let mut pending = PENDING_BRIDGE.lock().unwrap();
    if let Some(pos) = pending.iter().position(|(pid, _)| *pid == id) {
        let (_, tx) = pending.swap_remove(pos);
        return tx.send(result).is_ok();
    }
    false
}

fn drain_outbound() -> Vec<BridgeRequest> {
    std::mem::take(&mut *OUTBOUND.lock().unwrap())
}

// ---------------------------------------------------------------------------
// Completion eventfd (multi-executor mode): background completions are
// delivered into the dispatcher epoll.
// ---------------------------------------------------------------------------

static COMPLETION_FD: OnceLock<RawFd> = OnceLock::new();

extern "C" {
    fn eventfd(initval: u32, flags: i32) -> i32;
    fn write(fd: i32, buf: *const std::ffi::c_void, count: usize) -> isize;
    fn poll(fds: *mut PollFd, nfds: u64, timeout: i32) -> i32;
}

#[repr(C)]
struct PollFd {
    fd: i32,
    events: i16,
    revents: i16,
}

const POLLIN: i16 = 0x001;
const EFD_NONBLOCK: i32 = 0o4000;
const EFD_CLOEXEC: i32 = 0o2000000;

pub fn completion_fd() -> RawFd {
    if executor_mode() != EXECUTOR_MULTI {
        return -1;
    }
    *COMPLETION_FD.get_or_init(|| unsafe { eventfd(0, EFD_NONBLOCK | EFD_CLOEXEC) })
}

fn signal_completion_fd() {
    if let Some(&fd) = COMPLETION_FD.get() {
        if fd >= 0 {
            let one: u64 = 1;
            unsafe {
                write(fd, &one as *const u64 as *const std::ffi::c_void, 8);
            }
        }
    }
}

/// Level-triggered readiness check, independent of tokio's edge-triggered
/// cache. This is the safety net for the drained-but-not-yet-re-parked
/// window: poll(2) always reports current level.
fn fd_level_readable(fd: RawFd) -> bool {
    let mut pfd = PollFd { fd, events: POLLIN, revents: 0 };
    let rc = unsafe { poll(&mut pfd as *mut PollFd, 1, 0) };
    rc > 0 && (pfd.revents & POLLIN) != 0
}

/// Wrapper handing a borrowed fd to AsyncFd. The dispatcher owns the eventfd;
/// the tokio arm only observes readiness and never reads.
struct BorrowedTaskFd(RawFd);

impl std::os::fd::AsRawFd for BorrowedTaskFd {
    fn as_raw_fd(&self) -> RawFd {
        self.0
    }
}

// ---------------------------------------------------------------------------
// The drive loop (one park) and pump
// ---------------------------------------------------------------------------

pub enum DriveOutcome {
    Done(FutResult),
    Heartbeat,
    TaskFd,
    Bridge,
    NotFound,
}

/// Result of one park, serialized for the host.
pub fn drive_to_json(id: u64, slice_ms: u64, task_fd: RawFd) -> String {
    let outcome = drive(id, slice_ms, task_fd);
    let bridge_calls = drain_outbound();
    let mut obj = serde_json::Map::new();
    match outcome {
        DriveOutcome::Done(result) => {
            obj.insert("done".into(), true.into());
            let env = match result {
                Ok(value) => crate::envelope::Envelope::ok_value(value),
                Err(err) => crate::envelope::Envelope::err(&err),
            };
            obj.insert(
                "envelope".into(),
                serde_json::from_str(&env.to_json()).unwrap_or(serde_json::Value::Null),
            );
        }
        DriveOutcome::Heartbeat => {
            obj.insert("pending".into(), true.into());
            obj.insert("reason".into(), "heartbeat".into());
        }
        DriveOutcome::TaskFd => {
            obj.insert("pending".into(), true.into());
            obj.insert("reason".into(), "taskfd".into());
        }
        DriveOutcome::Bridge => {
            obj.insert("pending".into(), true.into());
            obj.insert("reason".into(), "bridge".into());
        }
        DriveOutcome::NotFound => {
            obj.insert("done".into(), true.into());
            obj.insert(
                "envelope".into(),
                serde_json::from_str(
                    &crate::envelope::Envelope::err(&OmniError::msg(format!(
                        "await handle {id} is not live (released, already completed, or created on a different OS thread — Rust runtime calls must stay on the golden thread)"
                    )))
                    .to_json(),
                )
                .unwrap_or(serde_json::Value::Null),
            );
        }
    }
    if !bridge_calls.is_empty() {
        obj.insert(
            "bridge_calls".into(),
            serde_json::to_value(&bridge_calls).unwrap_or(serde_json::Value::Null),
        );
    }
    serde_json::to_string(&serde_json::Value::Object(obj)).unwrap()
}

fn drive(id: u64, slice_ms: u64, task_fd: RawFd) -> DriveOutcome {
    // Reentrancy guard: a sequential block_on is fine, a nested one panics.
    // If we are already inside the runtime (a synchronous bridge call from a
    // parked future re-entered Rust), fall back to a manual poll loop.
    if Handle::try_current().is_ok() {
        return drive_nested(id, slice_ms);
    }

    let mut fut = match LOCAL_FUTURES.with(|m| m.borrow_mut().remove(&id)) {
        Some(f) => f,
        None => match LOCAL_SPAWNED.with(|m| m.borrow_mut().remove(&id)) {
            // `go expr` task on the LocalSet: await its JoinHandle through
            // the same park (the wrapper re-stores in LOCAL_FUTURES while
            // the underlying task keeps running on the set).
            Some(join) => Box::pin(async move {
                match join.await {
                    Ok(result) => result,
                    Err(join_err) => Err(OmniError::msg(format!("rust task failed: {join_err}"))),
                }
            }),
            None => {
                // Multi-executor mode stores spawned tasks instead.
                if SPAWNED.lock().unwrap().iter().any(|(sid, _)| *sid == id) {
                    return drive_spawned(id, slice_ms);
                }
                return DriveOutcome::NotFound;
            }
        },
    };

    // Take the AsyncFd out of the cache for the duration of the park (the
    // golden thread is the only consumer).
    let cached_fd = if task_fd >= 0 {
        TASK_FDS.with(|m| m.borrow_mut().remove(&task_fd))
    } else {
        None
    };

    let (outcome, fd_back) = LOCAL.with(|local| {
        local.block_on(runtime(), async {
            // taskFD protocol (edge-observed, two consumers, no reads):
            // 1. clear tokio's cached readiness from the previous park,
            // 2. level-check via poll(2) — catches both "not yet drained" and
            //    "written during the drained-but-not-re-parked window",
            // 3. only then park; a fresh write is a fresh edge and wakes us.
            let mut armed_fd: Option<AsyncFd<BorrowedTaskFd>> = None;
            if task_fd >= 0 {
                let fd_obj = match cached_fd {
                    Some(existing) => existing,
                    None => match AsyncFd::with_interest(BorrowedTaskFd(task_fd), Interest::READABLE) {
                        Ok(new_fd) => new_fd,
                        Err(_) => return (DriveOutcome::Heartbeat, None),
                    },
                };
                let waker = Waker::noop();
                let mut cx = Context::from_waker(&waker);
                if let Poll::Ready(Ok(mut guard)) = fd_obj.poll_read_ready(&mut cx) {
                    guard.clear_ready();
                }
                if fd_level_readable(task_fd) {
                    return (DriveOutcome::TaskFd, Some(fd_obj));
                }
                armed_fd = Some(fd_obj);
            }

            let outcome = tokio::select! {
                biased;
                result = &mut fut => DriveOutcome::Done(result),
                _ = outbound_notify().notified() => DriveOutcome::Bridge,
                _ = async {
                    match &armed_fd {
                        // Observed readiness only — the dispatcher drains the fd.
                        Some(fd_obj) => { let _ = fd_obj.readable().await; }
                        None => std::future::pending::<()>().await,
                    }
                }, if armed_fd.is_some() => DriveOutcome::TaskFd,
                _ = tokio::time::sleep(Duration::from_millis(slice_ms.max(1))) => DriveOutcome::Heartbeat,
            };
            (outcome, armed_fd)
        })
    });

    if let Some(fd_obj) = fd_back {
        TASK_FDS.with(|m| {
            m.borrow_mut().insert(task_fd, fd_obj);
        });
    }
    if !matches!(outcome, DriveOutcome::Done(_)) {
        LOCAL_FUTURES.with(|m| m.borrow_mut().insert(id, fut));
    }
    outcome
}

fn drive_spawned(id: u64, slice_ms: u64) -> DriveOutcome {
    let handle_opt = {
        let mut spawned = SPAWNED.lock().unwrap();
        spawned
            .iter()
            .position(|(sid, _)| *sid == id)
            .map(|pos| spawned.swap_remove(pos).1)
    };
    let mut handle = match handle_opt {
        Some(h) => h,
        None => return DriveOutcome::NotFound,
    };
    let outcome = runtime().block_on(async {
        tokio::select! {
            joined = &mut handle => Some(match joined {
                Ok(result) => result,
                Err(join_err) => Err(OmniError::msg(format!("rust task failed: {join_err}"))),
            }),
            _ = outbound_notify().notified() => None,
            _ = tokio::time::sleep(Duration::from_millis(slice_ms.max(1))) => None,
        }
    });
    match outcome {
        Some(result) => DriveOutcome::Done(result),
        None => {
            SPAWNED.lock().unwrap().push((id, handle));
            DriveOutcome::Heartbeat
        }
    }
}

struct FlagWake(std::sync::atomic::AtomicBool);

impl Wake for FlagWake {
    fn wake(self: std::sync::Arc<Self>) {
        self.0.store(true, Ordering::SeqCst);
    }
}

/// The 2a reentrancy-guard path: drive the future without entering block_on.
/// Manual polling makes progress for bridge-fed and compute futures; timers
/// owned by the parked outer reactor cannot fire here — that chain is exactly
/// what the async bridge hop removes (with the hop, re-entry happens after
/// the park exits, so this path is only reached via synchronous
/// `omnivm::call` from async context).
fn drive_nested(id: u64, slice_ms: u64) -> DriveOutcome {
    let mut fut = match LOCAL_FUTURES.with(|m| m.borrow_mut().remove(&id)) {
        Some(f) => f,
        None => return DriveOutcome::NotFound,
    };
    let flag = std::sync::Arc::new(FlagWake(std::sync::atomic::AtomicBool::new(true)));
    let waker = Waker::from(flag.clone());
    let mut cx = Context::from_waker(&waker);
    let deadline = Instant::now() + Duration::from_millis(slice_ms.max(1));
    loop {
        if flag.0.swap(false, Ordering::SeqCst) {
            match fut.as_mut().poll(&mut cx) {
                Poll::Ready(result) => return DriveOutcome::Done(result),
                Poll::Pending => {}
            }
        }
        if !OUTBOUND.lock().unwrap().is_empty() {
            LOCAL_FUTURES.with(|m| m.borrow_mut().insert(id, fut));
            return DriveOutcome::Bridge;
        }
        if Instant::now() >= deadline {
            LOCAL_FUTURES.with(|m| m.borrow_mut().insert(id, fut));
            return DriveOutcome::Heartbeat;
        }
        std::thread::sleep(Duration::from_micros(200));
    }
}

/// One dispatcher-cycle pump: drives ready tasks, fires expired timers, and
/// polls the I/O reactor inline — zero additional threads. Skipped when the
/// golden thread is already inside the runtime.
pub fn pump_to_json() -> String {
    if Handle::try_current().is_ok() {
        return "{\"skipped\":\"nested\"}".to_string();
    }
    if RUNTIME.get().is_some() && executor_mode() == EXECUTOR_CURRENT_THREAD {
        LOCAL.with(|local| {
            local.block_on(runtime(), async {
                tokio::task::yield_now().await;
            })
        });
    }
    let bridge_calls = drain_outbound();
    if bridge_calls.is_empty() {
        return "{}".to_string();
    }
    serde_json::to_string(&serde_json::json!({ "bridge_calls": bridge_calls })).unwrap()
}

pub fn stats_json() -> String {
    serde_json::json!({
        "abi_rev": crate::abi::ABI_REV,
        "executor": if executor_mode() == EXECUTOR_MULTI { "multi" } else { "current_thread" },
        "runtime_initialized": RUNTIME.get().is_some(),
        "pending_futures": pending_future_count(),
        "pending_bridge_calls": PENDING_BRIDGE.lock().unwrap().len(),
        "live_objects": crate::objects::live_count(),
        "live_cdata_shells": crate::cdata::live_shell_count(),
        "live_byte_buffers": crate::cdata::live_byte_buffer_count(),
        "log_records_forwarded": crate::logging::records_forwarded(),
    })
    .to_string()
}

/// Marker error: an async stream pull happened inside an active drive and
/// the stream wasn't ready — blocking would re-enter the runtime.
pub struct StreamPollWouldBlock;

/// Pulls the next item from an async stream. Outside a drive this blocks on
/// the runtime (timers/IO progress, golden-thread discipline preserved);
/// inside a drive it polls once and reports would-block instead of nesting.
pub fn block_on_stream_next(
    mut stream: std::pin::Pin<&mut (dyn futures_core::Stream<Item = serde_json::Value> + Send)>,
) -> Result<Option<serde_json::Value>, StreamPollWouldBlock> {
    if tokio::runtime::Handle::try_current().is_ok() {
        let waker = std::task::Waker::noop();
        let mut cx = std::task::Context::from_waker(&waker);
        return match stream.as_mut().poll_next(&mut cx) {
            std::task::Poll::Ready(v) => Ok(v),
            std::task::Poll::Pending => Err(StreamPollWouldBlock),
        };
    }
    Ok(LOCAL.with(|local| {
        local.block_on(runtime(), std::future::poll_fn(|cx| stream.as_mut().poll_next(cx)))
    }))
}
