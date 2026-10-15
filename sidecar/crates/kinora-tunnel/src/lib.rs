//! Kinora remote access tunnel over iroh.
//!
//! One crate for both ends: `server` is used by the kinora-iroh sidecar next
//! to the Go server, `client` by kinora-connect today and by the mobile apps
//! (through a uniffi wrapper) later. The wire protocol is deliberately dumb:
//! one QUIC bidirectional stream per TCP connection, bytes piped raw.

pub mod addr;
pub mod client;
#[cfg(feature = "cli")]
pub mod config;
pub mod events;
pub mod hexutil;
pub mod keys;
pub mod path;
pub mod proxy_v2;
pub mod pump;
pub mod ratelimit;
pub mod server;
pub mod transport;
pub mod units;

pub use client::{ClientConfig, ClientStatus, TunnelClient, TunnelError};
pub use server::{Server, ServerConfig};

/// Re-exported so the binaries parse node IDs with the same types.
pub use iroh;

/// ALPN for the tunnel. Bump the suffix for any incompatible wire change.
pub const ALPN: &[u8] = b"kinora/tunnel/1";

/// QUIC connection close codes sent by the server.
pub mod close_codes {
    pub const NORMAL: u32 = 0;
    pub const SHUTDOWN: u32 = 1;
    pub const BLOCKED: u32 = 2;
    pub const SUPERSEDED: u32 = 3;
    pub const RATE_LIMITED: u32 = 4;
    pub const BAD_ALPN: u32 = 5;
}

pub(crate) fn now_ms() -> u64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_millis() as u64).unwrap_or(0)
}

/// iroh's error types all implement Display but not always std::error::Error,
/// so convert through the message.
pub(crate) trait ErrStr<T> {
    fn err_str(self, what: &str) -> anyhow::Result<T>;
}

impl<T, E: std::fmt::Display> ErrStr<T> for Result<T, E> {
    fn err_str(self, what: &str) -> anyhow::Result<T> {
        self.map_err(|e| anyhow::anyhow!("{what}: {e}"))
    }
}
