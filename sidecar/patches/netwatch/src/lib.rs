//! Networking related utilities

pub mod interfaces;
pub mod ip;
mod ip_family;
pub mod netmon;
// Kinara patch: route constants libc lacks on iPhone and Apple TV.
#[cfg(any(target_os = "macos", any(target_os = "ios", target_os = "tvos")))]
mod apple_route;
#[cfg(not(wasm_browser))]
mod udp;

pub use self::ip_family::IpFamily;
#[cfg(not(wasm_browser))]
pub use self::udp::{UdpSender, UdpSocket};
