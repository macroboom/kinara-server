//! Endpoint and QUIC transport settings shared by both ends.

use std::net::{Ipv4Addr, Ipv6Addr, SocketAddrV4, SocketAddrV6};
use std::time::Duration;

use anyhow::Result;
use iroh::endpoint::{presets, Builder, IdleTimeout, QuicTransportConfig, VarInt};
use iroh::{Endpoint, RelayMode, RelayUrl, SecretKey};

use crate::ErrStr;

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RelayChoice {
    /// n0's public relays, as chosen by the N0 preset.
    Default,
    Disabled,
    /// Self-hosted iroh-relay servers.
    Custom(Vec<RelayUrl>),
}

impl RelayChoice {
    pub fn label(&self) -> &'static str {
        match self {
            RelayChoice::Default => "default",
            RelayChoice::Disabled => "disabled",
            RelayChoice::Custom(_) => "custom",
        }
    }

    pub fn enabled(&self) -> bool {
        !matches!(self, RelayChoice::Disabled)
    }

    /// Parses "default", "disabled" or "custom" plus the URL list.
    pub fn parse(mode: &str, urls: &[String]) -> Result<RelayChoice, String> {
        let parsed: Vec<RelayUrl> = urls
            .iter()
            .map(|u| u.parse::<RelayUrl>().map_err(|e| format!("invalid relay URL {u:?}: {e}")))
            .collect::<Result<_, _>>()?;
        match mode.to_ascii_lowercase().as_str() {
            "default" if parsed.is_empty() => Ok(RelayChoice::Default),
            "disabled" if parsed.is_empty() => Ok(RelayChoice::Disabled),
            "default" | "disabled" => Err("--relay-url needs --relay-mode custom".into()),
            "custom" if parsed.is_empty() => Err("--relay-mode custom needs at least one --relay-url".into()),
            "custom" => Ok(RelayChoice::Custom(parsed)),
            other => Err(format!("unknown relay mode {other:?}, expected default, disabled or custom")),
        }
    }
}

/// QUIC tuning. Defaults suit 4K remux streaming: about 150 Mbps at 100 ms
/// RTT is 1.9 MB in flight, so one stream gets a 4 MiB window and the whole
/// connection 16 MiB for several parallel range requests.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TransportOptions {
    pub idle_timeout: Duration,
    pub keep_alive: Duration,
    pub stream_window: u32,
    pub connection_window: u32,
    pub send_window: u64,
    /// Concurrent bidirectional streams the other end may open.
    pub max_incoming_streams: u32,
}

pub const DEFAULT_STREAM_WINDOW: u32 = 4 * 1024 * 1024;
pub const DEFAULT_CONNECTION_WINDOW: u32 = 16 * 1024 * 1024;
pub const DEFAULT_MAX_STREAMS: u32 = 64;

impl Default for TransportOptions {
    fn default() -> Self {
        Self {
            idle_timeout: Duration::from_secs(60),
            keep_alive: Duration::from_secs(15),
            stream_window: DEFAULT_STREAM_WINDOW,
            connection_window: DEFAULT_CONNECTION_WINDOW,
            send_window: DEFAULT_CONNECTION_WINDOW as u64,
            max_incoming_streams: DEFAULT_MAX_STREAMS,
        }
    }
}

impl TransportOptions {
    /// Starts from iroh's builder, which already carries the multipath and
    /// hole punching settings, and only touches timeouts, windows and limits.
    pub fn quic_config(&self) -> Result<QuicTransportConfig> {
        let idle = IdleTimeout::try_from(self.idle_timeout).err_str("idle timeout out of range")?;
        Ok(QuicTransportConfig::builder()
            .max_idle_timeout(Some(idle))
            .keep_alive_interval(self.keep_alive)
            .stream_receive_window(VarInt::from_u32(self.stream_window))
            .receive_window(VarInt::from_u32(self.connection_window))
            .send_window(self.send_window)
            // The QUIC layer enforces this cap: the peer's open_bi waits for credit.
            .max_concurrent_bidi_streams(VarInt::from_u32(self.max_incoming_streams))
            // Nothing in this protocol uses unidirectional streams.
            .max_concurrent_uni_streams(VarInt::from_u32(0))
            .build())
    }
}

#[derive(Debug, Clone)]
pub struct EndpointOptions {
    pub relay: RelayChoice,
    /// n0 DNS and pkarr publishing and lookup.
    pub discovery_n0: bool,
    /// mDNS on the local network.
    pub mdns: bool,
    /// Fixed UDP port for both IPv4 and IPv6, for a firewall rule or forward.
    pub udp_port: Option<u16>,
    /// False forces every path through the relay (client testing only).
    pub ip_transports: bool,
    pub transport: TransportOptions,
}

impl Default for EndpointOptions {
    fn default() -> Self {
        Self {
            relay: RelayChoice::Default,
            discovery_n0: true,
            mdns: true,
            udp_port: None,
            ip_transports: true,
            transport: TransportOptions::default(),
        }
    }
}

/// Builder with the n0 preset plus our overrides. Without a fixed port iroh
/// binds unspecified IPv4 and IPv6 sockets on its own, so both families are
/// always present.
pub fn endpoint_builder(opts: &EndpointOptions, secret: SecretKey) -> Result<Builder> {
    let mut b = Endpoint::builder(presets::N0).secret_key(secret);
    match &opts.relay {
        // Keep the preset's choice, which also follows iroh's staging override.
        RelayChoice::Default => {}
        RelayChoice::Disabled => b = b.relay_mode(RelayMode::Disabled),
        RelayChoice::Custom(urls) => b = b.relay_mode(RelayMode::custom(urls.iter().cloned())),
    }
    if !opts.discovery_n0 {
        b = b.clear_address_lookup();
    }
    if let Some(port) = opts.udp_port {
        b = b
            .bind_addr(SocketAddrV4::new(Ipv4Addr::UNSPECIFIED, port))
            .err_str("binding IPv4 UDP port")?
            .bind_addr(SocketAddrV6::new(Ipv6Addr::UNSPECIFIED, port, 0, 0))
            .err_str("binding IPv6 UDP port")?;
    }
    if !opts.ip_transports {
        b = b.clear_ip_transports();
    }
    Ok(b.transport_config(opts.transport.quic_config()?))
}

/// Adds mDNS lookup to a bound endpoint. Done after bind so a network
/// without multicast only loses LAN discovery instead of failing startup.
#[cfg(feature = "mdns")]
pub fn attach_mdns(ep: &Endpoint) -> Result<()> {
    use iroh_mdns_address_lookup::MdnsAddressLookup;
    let mdns = MdnsAddressLookup::builder().build(ep.id()).err_str("starting mDNS")?;
    ep.address_lookup().err_str("endpoint closed")?.add(mdns);
    Ok(())
}

#[cfg(not(feature = "mdns"))]
pub fn attach_mdns(_ep: &Endpoint) -> Result<()> {
    anyhow::bail!("built without the mdns feature")
}
