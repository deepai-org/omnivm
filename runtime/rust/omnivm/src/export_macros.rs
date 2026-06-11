//! Export shims: one macro invocation per exported fn turns a plain Rust
//! function into the host's c-shared call contract
//! (`char* OmniVMCall_<name>(char* args_json)` returning the JSON envelope).
//!
//! Codegen (the PolyScript compiler or the manifest host) appends these to
//! the compilation unit; arity is known from the func_def params:
//!
//! ```ignore
//! omnivm::export_fn!(OmniVMCall_classify, classify, 1);
//! omnivm::export_async_fn!(OmniVMCall_enrich, enrich, 1);
//! ```
//!
//! Argument types and the return type come from the user fn's own signature
//! via serde — `Result<T, E>` at the boundary compiles to the structured
//! error envelope, panics are caught, and async fns return a stored-future
//! handle driven by the host's re-park loop.

#[macro_export]
#[doc(hidden)]
macro_rules! __omnivm_export_impl {
    ($sym:ident, $func:ident, [$($idx:tt),*]) => {
        #[no_mangle]
        pub extern "C" fn $sym(args_json: *mut ::std::os::raw::c_char) -> *mut ::std::os::raw::c_char {
            $crate::abi::invoke_enveloped(|| {
                let __args = $crate::abi::parse_args(args_json)?;
                let __ret = $func($($crate::abi::arg(&__args, $idx)?),*);
                let __value = {
                    #[allow(unused_imports)]
                    use $crate::abi::{PlainOutcome, ResultOutcome};
                    $crate::abi::OutcomeToken(Some(__ret)).omni_outcome()?
                };
                Ok($crate::envelope::Envelope::ok_value(__value))
            })
        }
    };
}

#[macro_export]
#[doc(hidden)]
macro_rules! __omnivm_export_async_impl {
    ($sym:ident, $func:ident, [$($idx:tt),*]) => {
        #[no_mangle]
        pub extern "C" fn $sym(args_json: *mut ::std::os::raw::c_char) -> *mut ::std::os::raw::c_char {
            $crate::abi::invoke_enveloped(|| {
                let __args = $crate::abi::parse_args(args_json)?;
                let __fut = async move {
                    let __ret = $func($($crate::abi::arg(&__args, $idx)?),*).await;
                    #[allow(unused_imports)]
                    use $crate::abi::{PlainOutcome, ResultOutcome};
                    $crate::abi::OutcomeToken(Some(__ret)).omni_outcome()
                };
                let __handle = {
                    #[allow(unused_imports)]
                    use $crate::rt::{RegisterLocalFut, RegisterSendFut};
                    $crate::rt::FutToken(Some(__fut)).register()
                };
                Ok($crate::envelope::Envelope::future(__handle))
            })
        }
    };
}

/// Per-argument extraction by declared kind: `df` params import Arrow
/// markers directly (the C-Data pointer handoff stays zero-copy), `bytes`
/// params (`Vec<u8>`-shaped) ride the bytes pointer lane, `json` params go
/// through serde. Codegen emits kinds from the fn's signature.
#[macro_export]
#[doc(hidden)]
macro_rules! __omnivm_arg_kind {
    (df, $args:expr, $idx:tt) => {
        $crate::abi::arg_dataframe($args, $idx)?
    };
    (bytes, $args:expr, $idx:tt) => {
        $crate::abi::arg_bytes($args, $idx)?
    };
    (json, $args:expr, $idx:tt) => {
        $crate::abi::arg($args, $idx)?
    };
}

#[macro_export]
#[doc(hidden)]
macro_rules! __omnivm_export_kinds_impl {
    ($sym:ident, $func:ident, [$(($kind:ident, $idx:tt)),*]) => {
        #[no_mangle]
        pub extern "C" fn $sym(args_json: *mut ::std::os::raw::c_char) -> *mut ::std::os::raw::c_char {
            $crate::abi::invoke_enveloped(|| {
                let __args = $crate::abi::parse_args(args_json)?;
                let __ret = $func($($crate::__omnivm_arg_kind!($kind, &__args, $idx)),*);
                let __value = {
                    #[allow(unused_imports)]
                    use $crate::abi::{PlainOutcome, ResultOutcome};
                    $crate::abi::OutcomeToken(Some(__ret)).omni_outcome()?
                };
                Ok($crate::envelope::Envelope::ok_value(__value))
            })
        }
    };
}

#[macro_export]
#[doc(hidden)]
macro_rules! __omnivm_export_async_kinds_impl {
    ($sym:ident, $func:ident, [$(($kind:ident, $idx:tt)),*]) => {
        #[no_mangle]
        pub extern "C" fn $sym(args_json: *mut ::std::os::raw::c_char) -> *mut ::std::os::raw::c_char {
            $crate::abi::invoke_enveloped(|| {
                let __args = $crate::abi::parse_args(args_json)?;
                let __fut = async move {
                    let __ret = $func($($crate::__omnivm_arg_kind!($kind, &__args, $idx)),*).await;
                    #[allow(unused_imports)]
                    use $crate::abi::{PlainOutcome, ResultOutcome};
                    $crate::abi::OutcomeToken(Some(__ret)).omni_outcome()
                };
                let __handle = {
                    #[allow(unused_imports)]
                    use $crate::rt::{RegisterLocalFut, RegisterSendFut};
                    $crate::rt::FutToken(Some(__fut)).register()
                };
                Ok($crate::envelope::Envelope::future(__handle))
            })
        }
    };
}

#[macro_export]
macro_rules! export_fn {
    ($sym:ident, $func:ident, 0) => { $crate::__omnivm_export_impl!($sym, $func, []); };
    ($sym:ident, $func:ident, 1) => { $crate::__omnivm_export_impl!($sym, $func, [0]); };
    ($sym:ident, $func:ident, 2) => { $crate::__omnivm_export_impl!($sym, $func, [0, 1]); };
    ($sym:ident, $func:ident, 3) => { $crate::__omnivm_export_impl!($sym, $func, [0, 1, 2]); };
    ($sym:ident, $func:ident, 4) => { $crate::__omnivm_export_impl!($sym, $func, [0, 1, 2, 3]); };
    ($sym:ident, $func:ident, 5) => { $crate::__omnivm_export_impl!($sym, $func, [0, 1, 2, 3, 4]); };
    ($sym:ident, $func:ident, 6) => { $crate::__omnivm_export_impl!($sym, $func, [0, 1, 2, 3, 4, 5]); };
    ($sym:ident, $func:ident, 7) => { $crate::__omnivm_export_impl!($sym, $func, [0, 1, 2, 3, 4, 5, 6]); };
    ($sym:ident, $func:ident, 8) => { $crate::__omnivm_export_impl!($sym, $func, [0, 1, 2, 3, 4, 5, 6, 7]); };
    ($sym:ident, $func:ident, ($k0:ident)) => { $crate::__omnivm_export_kinds_impl!($sym, $func, [($k0, 0)]); };
    ($sym:ident, $func:ident, ($k0:ident, $k1:ident)) => { $crate::__omnivm_export_kinds_impl!($sym, $func, [($k0, 0), ($k1, 1)]); };
    ($sym:ident, $func:ident, ($k0:ident, $k1:ident, $k2:ident)) => { $crate::__omnivm_export_kinds_impl!($sym, $func, [($k0, 0), ($k1, 1), ($k2, 2)]); };
    ($sym:ident, $func:ident, ($k0:ident, $k1:ident, $k2:ident, $k3:ident)) => { $crate::__omnivm_export_kinds_impl!($sym, $func, [($k0, 0), ($k1, 1), ($k2, 2), ($k3, 3)]); };
}

#[macro_export]
macro_rules! export_async_fn {
    ($sym:ident, $func:ident, 0) => { $crate::__omnivm_export_async_impl!($sym, $func, []); };
    ($sym:ident, $func:ident, 1) => { $crate::__omnivm_export_async_impl!($sym, $func, [0]); };
    ($sym:ident, $func:ident, 2) => { $crate::__omnivm_export_async_impl!($sym, $func, [0, 1]); };
    ($sym:ident, $func:ident, 3) => { $crate::__omnivm_export_async_impl!($sym, $func, [0, 1, 2]); };
    ($sym:ident, $func:ident, 4) => { $crate::__omnivm_export_async_impl!($sym, $func, [0, 1, 2, 3]); };
    ($sym:ident, $func:ident, 5) => { $crate::__omnivm_export_async_impl!($sym, $func, [0, 1, 2, 3, 4]); };
    ($sym:ident, $func:ident, 6) => { $crate::__omnivm_export_async_impl!($sym, $func, [0, 1, 2, 3, 4, 5]); };
    ($sym:ident, $func:ident, 7) => { $crate::__omnivm_export_async_impl!($sym, $func, [0, 1, 2, 3, 4, 5, 6]); };
    ($sym:ident, $func:ident, 8) => { $crate::__omnivm_export_async_impl!($sym, $func, [0, 1, 2, 3, 4, 5, 6, 7]); };
    ($sym:ident, $func:ident, ($k0:ident)) => { $crate::__omnivm_export_async_kinds_impl!($sym, $func, [($k0, 0)]); };
    ($sym:ident, $func:ident, ($k0:ident, $k1:ident)) => { $crate::__omnivm_export_async_kinds_impl!($sym, $func, [($k0, 0), ($k1, 1)]); };
    ($sym:ident, $func:ident, ($k0:ident, $k1:ident, $k2:ident)) => { $crate::__omnivm_export_async_kinds_impl!($sym, $func, [($k0, 0), ($k1, 1), ($k2, 2)]); };
    ($sym:ident, $func:ident, ($k0:ident, $k1:ident, $k2:ident, $k3:ident)) => { $crate::__omnivm_export_async_kinds_impl!($sym, $func, [($k0, 0), ($k1, 1), ($k2, 2), ($k3, 3)]); };
}

/// Standard per-unit boilerplate appended by the host when compiling a unit:
/// bakes the build-time ABI revision into the artifact so the host can reject
/// stale artifacts with a structured load error instead of crashing later.
#[macro_export]
macro_rules! unit_abi_marker {
    () => {
        #[no_mangle]
        pub extern "C" fn omnivm_unit_abi_v1() -> i32 {
            // ABI_REV is a const: its value is baked in at unit compile time.
            $crate::abi::ABI_REV
        }
    };
}

/// Typed fast-path export: a second `extern "C"` entry crossing scalar
/// args/returns as omni_value_t (no JSON text). Codegen emits one only when
/// the fn's declared signature is scalar-shaped — argument types are
/// inferred FORWARD from the fn, never from the call site.
#[macro_export]
macro_rules! export_typed_fn {
    ($sym:ident, $func:ident, 0) => {
        #[no_mangle]
        pub extern "C" fn $sym(_args: *const $crate::abi::OmniValue, nargs: i32, out: *mut $crate::abi::OmniValue) -> i32 {
            $crate::export_typed_fn!(@check nargs, 0, out);
            $crate::export_typed_fn!(@call out, move || $func())
        }
    };
    ($sym:ident, $func:ident, 1) => {
        #[no_mangle]
        pub extern "C" fn $sym(args: *const $crate::abi::OmniValue, nargs: i32, out: *mut $crate::abi::OmniValue) -> i32 {
            $crate::export_typed_fn!(@check nargs, 1, out);
            let a0 = $crate::export_typed_fn!(@arg args, 0, out);
            $crate::export_typed_fn!(@call out, move || $func(a0))
        }
    };
    ($sym:ident, $func:ident, 2) => {
        #[no_mangle]
        pub extern "C" fn $sym(args: *const $crate::abi::OmniValue, nargs: i32, out: *mut $crate::abi::OmniValue) -> i32 {
            $crate::export_typed_fn!(@check nargs, 2, out);
            let a0 = $crate::export_typed_fn!(@arg args, 0, out);
            let a1 = $crate::export_typed_fn!(@arg args, 1, out);
            $crate::export_typed_fn!(@call out, move || $func(a0, a1))
        }
    };
    ($sym:ident, $func:ident, 3) => {
        #[no_mangle]
        pub extern "C" fn $sym(args: *const $crate::abi::OmniValue, nargs: i32, out: *mut $crate::abi::OmniValue) -> i32 {
            $crate::export_typed_fn!(@check nargs, 3, out);
            let a0 = $crate::export_typed_fn!(@arg args, 0, out);
            let a1 = $crate::export_typed_fn!(@arg args, 1, out);
            let a2 = $crate::export_typed_fn!(@arg args, 2, out);
            $crate::export_typed_fn!(@call out, move || $func(a0, a1, a2))
        }
    };
    // Results are written with ptr::write: the out slot is host memory whose
    // previous contents must never be dropped (OmniValue owns its string
    // payload via Drop).
    (@check $nargs:ident, $want:expr, $out:ident) => {
        if $nargs != $want {
            unsafe { ::std::ptr::write($out, $crate::abi::OmniValue::error("typed call: arity mismatch")) };
            return 1;
        }
    };
    (@arg $args:ident, $idx:expr, $out:ident) => {
        match $crate::abi::FromOmniValue::from_omni(unsafe { &*$args.add($idx) }) {
            Ok(v) => v,
            Err(message) => {
                unsafe { ::std::ptr::write($out, $crate::abi::OmniValue::error(&message)) };
                return 1;
            }
        }
    };
    (@call $out:ident, $thunk:expr) => {
        match std::panic::catch_unwind(std::panic::AssertUnwindSafe($thunk)) {
            Ok(result) => {
                unsafe { ::std::ptr::write($out, $crate::abi::ToOmniValue::to_omni(result)) };
                0
            }
            Err(payload) => {
                let message = $crate::error::panic_message(payload);
                unsafe { ::std::ptr::write($out, $crate::abi::OmniValue::error(&message)) };
                1
            }
        }
    };
}
