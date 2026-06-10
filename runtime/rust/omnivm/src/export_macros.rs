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
