//! The tunnel client for the Kinara apps.
//!
//! The apps run `kinora_tunnel::TunnelClient` in their own process: it
//! listens on a 127.0.0.1 port and carries every connection to that port to
//! the home server over iroh. The app then uses http://127.0.0.1:<port> as
//! the server address, so its API calls, artwork and video players work
//! unchanged.
//!
//! Two thin bindings over `api`, both passing JSON text:
//!
//! * `c`: a plain C ABI (`include/kinora_tunnel.h`) for Swift on iOS.
//! * `android`: JNI entry points for the Kotlin class
//!   `expo.modules.kinoratunnel.TunnelNative` in the app's
//!   `modules/kinora-tunnel`.

pub mod api;
pub mod c;

#[cfg(target_os = "android")]
mod android;
