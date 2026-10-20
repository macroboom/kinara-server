//! C ABI, declared in `include/kinara_tunnel.h`. Strings in and out are
//! UTF-8 JSON (ASCII in practice: see `api::escape_non_ascii`). Every string
//! returned must be released with `kinara_tunnel_string_free`.
//!
//! A panic never crosses into the caller: it becomes an error answer.

use std::ffi::{c_char, CStr, CString};
use std::panic::{catch_unwind, AssertUnwindSafe};

use crate::api;

fn guarded(f: impl FnOnce() -> String) -> String {
    catch_unwind(AssertUnwindSafe(f))
        .unwrap_or_else(|_| r#"{"ok":false,"error":"the tunnel library hit an internal error"}"#.to_string())
}

fn out(s: String) -> *mut c_char {
    // api never produces NUL (it escapes it), so this only fails on a bug.
    CString::new(s).unwrap_or_else(|_| CString::new(r#"{"ok":false,"error":"bad output"}"#).unwrap()).into_raw()
}

/// # Safety
/// `s` must be null or a valid NUL-terminated string.
unsafe fn text(s: *const c_char) -> String {
    if s.is_null() {
        return String::new();
    }
    CStr::from_ptr(s).to_string_lossy().into_owned()
}

/// Starts a tunnel from JSON settings; answers {"ok", "handle", "port", "url", "nodeId"}.
///
/// # Safety
/// `config_json` must be null or a valid NUL-terminated string.
#[no_mangle]
pub unsafe extern "C" fn kinara_tunnel_start(config_json: *const c_char) -> *mut c_char {
    let cfg = text(config_json);
    out(guarded(|| api::start(&cfg)))
}

/// The tunnel's state as JSON.
#[no_mangle]
pub extern "C" fn kinara_tunnel_status(handle: u64) -> *mut c_char {
    out(guarded(|| api::status(handle)))
}

/// Call when the device's network changes.
#[no_mangle]
pub extern "C" fn kinara_tunnel_network_change() {
    let _ = catch_unwind(api::network_change);
}

#[no_mangle]
pub extern "C" fn kinara_tunnel_stop(handle: u64) {
    let _ = catch_unwind(|| api::stop(handle));
}

#[no_mangle]
pub extern "C" fn kinara_tunnel_stop_all() {
    let _ = catch_unwind(api::stop_all);
}

/// Log lines since the last call, newline separated.
#[no_mangle]
pub extern "C" fn kinara_tunnel_drain_logs() -> *mut c_char {
    out(guarded(api::drain_logs))
}

/// Frees a string returned by this library. Null is ignored.
///
/// # Safety
/// `s` must be null or a pointer returned by this library, freed once.
#[no_mangle]
pub unsafe extern "C" fn kinara_tunnel_string_free(s: *mut c_char) {
    if !s.is_null() {
        drop(CString::from_raw(s));
    }
}
