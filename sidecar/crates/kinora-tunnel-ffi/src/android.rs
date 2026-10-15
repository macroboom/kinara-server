//! JNI entry points for `expo.modules.kinoratunnel.TunnelNative`, a Kotlin
//! `object` whose `@JvmStatic external fun`s land here. Names follow JNI's
//! `Java_<package>_<class>_<method>` rule; renaming the Kotlin package,
//! class or a method means renaming the function here too.
//!
//! Uses the raw `jni-sys` table (a stable C layout) rather than a wrapper
//! crate, since only three JNI calls are needed.

use std::ffi::{c_char, CStr, CString};
use std::panic::{catch_unwind, AssertUnwindSafe};
use std::ptr;

use jni_sys::{jclass, jlong, jstring, JNIEnv};

use crate::api;

/// Copies a Java string. Java's modified UTF-8 equals UTF-8 for everything
/// the app sends (tickets, URLs and paths are ASCII).
///
/// # Safety
/// `env` must be the JNIEnv of the calling thread.
unsafe fn from_java(env: *mut JNIEnv, s: jstring) -> String {
    if s.is_null() {
        return String::new();
    }
    let (Some(get), Some(release)) = ((**env).GetStringUTFChars, (**env).ReleaseStringUTFChars) else {
        return String::new();
    };
    let chars = get(env, s, ptr::null_mut());
    if chars.is_null() {
        // Out of memory: the JVM has an exception pending, which Kotlin sees.
        return String::new();
    }
    let out = CStr::from_ptr(chars).to_string_lossy().into_owned();
    release(env, s, chars);
    out
}

/// # Safety
/// `env` must be the JNIEnv of the calling thread.
unsafe fn to_java(env: *mut JNIEnv, s: &str) -> jstring {
    let Some(new_string) = (**env).NewStringUTF else {
        return ptr::null_mut();
    };
    // api output is ASCII without NUL, so it is valid modified UTF-8 as is.
    let c = CString::new(s.replace('\0', "")).unwrap_or_default();
    new_string(env, c.as_ptr() as *const c_char)
}

fn guarded(f: impl FnOnce() -> String) -> String {
    catch_unwind(AssertUnwindSafe(f))
        .unwrap_or_else(|_| r#"{"ok":false,"error":"the tunnel library hit an internal error"}"#.to_string())
}

/// # Safety
/// Called by the JVM.
#[no_mangle]
pub unsafe extern "system" fn Java_expo_modules_kinoratunnel_TunnelNative_nativeStart(
    env: *mut JNIEnv,
    _class: jclass,
    config: jstring,
) -> jstring {
    let cfg = from_java(env, config);
    to_java(env, &guarded(|| api::start(&cfg)))
}

/// # Safety
/// Called by the JVM.
#[no_mangle]
pub unsafe extern "system" fn Java_expo_modules_kinoratunnel_TunnelNative_nativeStatus(
    env: *mut JNIEnv,
    _class: jclass,
    handle: jlong,
) -> jstring {
    to_java(env, &guarded(|| api::status(handle as u64)))
}

/// # Safety
/// Called by the JVM.
#[no_mangle]
pub unsafe extern "system" fn Java_expo_modules_kinoratunnel_TunnelNative_nativeNetworkChange(
    _env: *mut JNIEnv,
    _class: jclass,
) {
    let _ = catch_unwind(api::network_change);
}

/// # Safety
/// Called by the JVM.
#[no_mangle]
pub unsafe extern "system" fn Java_expo_modules_kinoratunnel_TunnelNative_nativeStop(
    _env: *mut JNIEnv,
    _class: jclass,
    handle: jlong,
) {
    let _ = catch_unwind(|| api::stop(handle as u64));
}

/// # Safety
/// Called by the JVM.
#[no_mangle]
pub unsafe extern "system" fn Java_expo_modules_kinoratunnel_TunnelNative_nativeStopAll(
    _env: *mut JNIEnv,
    _class: jclass,
) {
    let _ = catch_unwind(api::stop_all);
}

/// # Safety
/// Called by the JVM.
#[no_mangle]
pub unsafe extern "system" fn Java_expo_modules_kinoratunnel_TunnelNative_nativeDrainLogs(
    env: *mut JNIEnv,
    _class: jclass,
) -> jstring {
    to_java(env, &guarded(api::drain_logs))
}
