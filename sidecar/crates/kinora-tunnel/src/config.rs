//! Command line and environment configuration for both binaries.

use std::path::PathBuf;
use std::str::FromStr;
use std::time::Duration;

use clap::{Args, Parser};

use crate::client::ClientConfig;
use crate::server::ServerConfig;
use crate::transport::{self, EndpointOptions, RelayChoice, TransportOptions};
use crate::units;

/// Quiet iroh's own chatter by default; kinora lines stay at info.
pub const DEFAULT_LOG: &str =
    "info,iroh=warn,noq=warn,iroh_relay=warn,portmapper=warn,iroh_mdns_address_lookup=warn,swarm_discovery=warn";

#[derive(Debug, Clone, Args)]
pub struct RelayArgs {
    /// Relay servers: n0's public relays, none, or the --relay-url list.
    #[arg(long, env = "KINORA_IROH_RELAY_MODE", default_value = "default", value_parser = ["default", "disabled", "custom"])]
    pub relay_mode: String,

    /// Self-hosted iroh-relay URL. Repeat the flag, or comma separate in the env var.
    #[arg(long = "relay-url", env = "KINORA_IROH_RELAY_URLS", value_delimiter = ',')]
    pub relay_urls: Vec<String>,

    /// Do not publish to or look up through n0's DNS and pkarr servers.
    #[arg(long, env = "KINORA_IROH_NO_DISCOVERY_N0")]
    pub no_discovery_n0: bool,

    /// Do not use mDNS on the local network.
    #[arg(long, env = "KINORA_IROH_NO_MDNS")]
    pub no_mdns: bool,
}

#[derive(Debug, Clone, Args)]
pub struct TransportArgs {
    /// Per-stream receive window, e.g. 4M. One stream needs about RTT x rate.
    #[arg(long, env = "KINORA_IROH_STREAM_WINDOW", default_value = "4M", value_parser = units::parse_window)]
    pub stream_window: u32,

    /// Connection-wide receive window across all streams of one peer, e.g. 16M.
    #[arg(long, env = "KINORA_IROH_CONNECTION_WINDOW", default_value = "16M", value_parser = units::parse_window)]
    pub connection_window: u32,

    /// Send buffer per connection, e.g. 16M.
    #[arg(long, env = "KINORA_IROH_SEND_WINDOW", default_value = "16M", value_parser = units::parse_size)]
    pub send_window: u64,

    /// Close a connection after this many seconds without any packet.
    #[arg(long, env = "KINORA_IROH_IDLE_TIMEOUT_SECS", default_value_t = 60)]
    pub idle_timeout_secs: u64,

    /// Keepalive interval, keeps NAT mappings and long idle streams open.
    #[arg(long, env = "KINORA_IROH_KEEPALIVE_SECS", default_value_t = 15)]
    pub keepalive_secs: u64,
}

impl TransportArgs {
    fn to_options(&self, max_incoming_streams: u32) -> Result<TransportOptions, String> {
        if self.keepalive_secs == 0 || self.keepalive_secs >= self.idle_timeout_secs {
            return Err(format!(
                "--keepalive-secs ({}) must be above 0 and below --idle-timeout-secs ({})",
                self.keepalive_secs, self.idle_timeout_secs
            ));
        }
        Ok(TransportOptions {
            idle_timeout: Duration::from_secs(self.idle_timeout_secs),
            keep_alive: Duration::from_secs(self.keepalive_secs),
            stream_window: self.stream_window,
            connection_window: self.connection_window,
            send_window: self.send_window,
            max_incoming_streams,
        })
    }
}

/// kinora-iroh: the server-side sidecar started and supervised by kinora-server.
#[derive(Debug, Clone, Parser)]
#[command(name = "kinora-iroh", version, about)]
pub struct ServerArgs {
    /// Kinora data folder (KINORA_DATA). The key goes in <data-dir>/iroh/.
    #[arg(long, env = "KINORA_IROH_DATA_DIR")]
    pub data_dir: PathBuf,

    /// kinora-server's PROXY protocol tunnel listener.
    #[arg(long, env = "KINORA_IROH_TARGET", default_value = "127.0.0.1:8421")]
    pub target: String,

    /// Fixed UDP port for IPv4 and IPv6, for a firewall rule or port forward.
    #[arg(long, env = "KINORA_IROH_UDP_PORT")]
    pub udp_port: Option<u16>,

    #[command(flatten)]
    pub relay: RelayArgs,

    #[command(flatten)]
    pub transport: TransportArgs,

    /// Seconds between periodic status lines on stdout.
    #[arg(long, env = "KINORA_IROH_STATUS_INTERVAL_SECS", default_value_t = 5)]
    pub status_interval_secs: u64,

    /// Concurrent streams (tunnelled TCP connections) per device.
    #[arg(long, env = "KINORA_IROH_MAX_STREAMS_PER_PEER", default_value_t = transport::DEFAULT_MAX_STREAMS)]
    pub max_streams_per_peer: u32,

    /// New connections per device per minute. 0 disables the limit.
    #[arg(long, env = "KINORA_IROH_MAX_CONNS_PER_MINUTE", default_value_t = 30)]
    pub max_conns_per_minute: u32,

    /// New connections from all devices together per minute. 0 disables the limit.
    #[arg(long, env = "KINORA_IROH_MAX_HANDSHAKES_PER_MINUTE", default_value_t = 120)]
    pub max_handshakes_per_minute: u32,

    /// Shared secret put in every PROXY header so kinora-server can tell the
    /// sidecar from other local programs. kinora-server sets it; env only.
    #[arg(long, env = "KINORA_IROH_SECRET", hide = true, hide_env_values = true)]
    pub secret: Option<String>,

    /// Node ID to refuse from the start. Repeat, or comma separate in the env var.
    #[arg(long = "block", env = "KINORA_IROH_BLOCK", value_delimiter = ',')]
    pub block: Vec<String>,

    /// How long in-flight streams get on shutdown.
    #[arg(long, env = "KINORA_IROH_SHUTDOWN_GRACE_MS", default_value_t = 1000)]
    pub shutdown_grace_ms: u64,

    /// tracing filter for stderr logs, e.g. "debug" or "info,kinora_tunnel=debug".
    #[arg(long, env = "KINORA_IROH_LOG", default_value = DEFAULT_LOG)]
    pub log_level: String,
}

impl ServerArgs {
    pub fn to_config(&self) -> Result<ServerConfig, String> {
        if self.max_streams_per_peer == 0 {
            return Err("--max-streams-per-peer must be at least 1".into());
        }
        if self.status_interval_secs == 0 {
            return Err("--status-interval-secs must be at least 1".into());
        }
        if self.udp_port == Some(0) {
            return Err("--udp-port 0 means automatic; leave the flag out instead".into());
        }
        let blocked = self
            .block
            .iter()
            .filter(|s| !s.trim().is_empty())
            .map(|s| iroh::EndpointId::from_str(s.trim()).map_err(|e| format!("invalid node ID in --block {s:?}: {e}")))
            .collect::<Result<Vec<_>, _>>()?;
        Ok(ServerConfig {
            data_dir: self.data_dir.clone(),
            target: self.target.clone(),
            endpoint: EndpointOptions {
                relay: RelayChoice::parse(&self.relay.relay_mode, &self.relay.relay_urls)?,
                discovery_n0: !self.relay.no_discovery_n0,
                mdns: !self.relay.no_mdns,
                udp_port: self.udp_port,
                ip_transports: true,
                transport: self.transport.to_options(self.max_streams_per_peer)?,
            },
            max_conns_per_minute: self.max_conns_per_minute,
            max_handshakes_per_minute: self.max_handshakes_per_minute,
            secret: self.secret.clone().filter(|s| !s.is_empty()),
            blocked,
        })
    }

    pub fn status_interval(&self) -> Duration {
        Duration::from_secs(self.status_interval_secs.max(1))
    }

    pub fn shutdown_grace(&self) -> Duration {
        Duration::from_millis(self.shutdown_grace_ms)
    }
}

/// kinora-connect: test client. Exposes the home server on a local port.
#[derive(Debug, Clone, Parser)]
#[command(name = "kinora-connect", version, about)]
pub struct ConnectArgs {
    /// Connection ticket printed by kinora-iroh, or the server's node ID.
    pub server: String,

    /// Local port to listen on (127.0.0.1 only). Default: any free port.
    #[arg(long)]
    pub port: Option<u16>,

    /// Force every byte through the relay, to test the worst case.
    #[arg(long)]
    pub relay_only: bool,

    /// Print a status line every --status-interval-secs, not only on changes.
    #[arg(long)]
    pub status: bool,

    #[arg(long, default_value_t = 2)]
    pub status_interval_secs: u64,

    /// Folder for this client's own key. Default: ./kinora-connect-data
    #[arg(long, env = "KINORA_CONNECT_DATA_DIR", default_value = "kinora-connect-data")]
    pub data_dir: PathBuf,

    #[command(flatten)]
    pub relay: RelayArgs,

    /// Give up on a connect attempt after this many milliseconds.
    #[arg(long, default_value_t = 10_000)]
    pub connect_timeout_ms: u64,

    /// Exit when stdin closes, so a test harness can stop it like the sidecar.
    #[arg(long)]
    pub exit_on_stdin_close: bool,

    #[arg(long, env = "KINORA_CONNECT_LOG", default_value = DEFAULT_LOG)]
    pub log_level: String,
}

impl ConnectArgs {
    pub fn to_config(&self) -> Result<ClientConfig, String> {
        // Validate early so a typo fails before any network setup.
        RelayChoice::parse(&self.relay.relay_mode, &self.relay.relay_urls)?;
        let mut cfg = ClientConfig::new(self.server.clone(), self.data_dir.to_string_lossy().into_owned());
        cfg.relay_mode = self.relay.relay_mode.clone();
        cfg.relay_urls = self.relay.relay_urls.clone();
        cfg.relay_only = self.relay_only;
        cfg.discovery_n0 = !self.relay.no_discovery_n0;
        cfg.mdns = !self.relay.no_mdns;
        cfg.local_port = self.port;
        cfg.connect_timeout_ms = self.connect_timeout_ms;
        Ok(cfg)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const NODE: &str = "ae58ff8833241ac82d6ff7611046ed67b5072d142c588d0063e942d9a75502b6";

    fn server(args: &[&str]) -> Result<ServerConfig, String> {
        let mut full = vec!["kinora-iroh"];
        full.extend_from_slice(args);
        ServerArgs::try_parse_from(full).map_err(|e| e.to_string())?.to_config()
    }

    #[test]
    fn server_defaults() {
        let cfg = server(&["--data-dir", "/tmp/k"]).unwrap();
        assert_eq!(cfg.target, "127.0.0.1:8421");
        assert_eq!(cfg.endpoint.relay, RelayChoice::Default);
        assert!(cfg.endpoint.discovery_n0);
        assert!(cfg.endpoint.mdns);
        assert_eq!(cfg.endpoint.udp_port, None);
        assert_eq!(cfg.endpoint.transport, TransportOptions::default());
        assert_eq!(cfg.max_conns_per_minute, 30);
        assert!(cfg.blocked.is_empty());
    }

    #[test]
    fn server_flags() {
        let cfg = server(&[
            "--data-dir",
            "/tmp/k",
            "--target",
            "127.0.0.1:9000",
            "--udp-port",
            "7777",
            "--relay-mode",
            "custom",
            "--relay-url",
            "https://relay.example.com",
            "--relay-url",
            "https://relay2.example.com",
            "--no-discovery-n0",
            "--no-mdns",
            "--max-streams-per-peer",
            "16",
            "--stream-window",
            "8M",
            "--connection-window",
            "32M",
            "--block",
            NODE,
        ])
        .unwrap();
        assert_eq!(cfg.target, "127.0.0.1:9000");
        assert_eq!(cfg.endpoint.udp_port, Some(7777));
        match &cfg.endpoint.relay {
            RelayChoice::Custom(urls) => assert_eq!(urls.len(), 2),
            other => panic!("unexpected relay {other:?}"),
        }
        assert!(!cfg.endpoint.discovery_n0);
        assert!(!cfg.endpoint.mdns);
        assert_eq!(cfg.endpoint.transport.max_incoming_streams, 16);
        assert_eq!(cfg.endpoint.transport.stream_window, 8 << 20);
        assert_eq!(cfg.endpoint.transport.connection_window, 32 << 20);
        assert_eq!(cfg.blocked.len(), 1);
        assert_eq!(cfg.blocked[0].to_string(), NODE);
    }

    #[test]
    fn server_rejects_bad_input() {
        assert!(server(&[]).is_err(), "data dir is required");
        assert!(server(&["--data-dir", "d", "--relay-mode", "custom"]).is_err());
        assert!(server(&["--data-dir", "d", "--relay-url", "https://r.example.com"]).is_err());
        assert!(server(&["--data-dir", "d", "--relay-mode", "sometimes"]).is_err());
        assert!(server(&["--data-dir", "d", "--stream-window", "12X"]).is_err());
        assert!(server(&["--data-dir", "d", "--max-streams-per-peer", "0"]).is_err());
        assert!(server(&["--data-dir", "d", "--keepalive-secs", "90"]).is_err());
        assert!(server(&["--data-dir", "d", "--block", "not-a-node"]).is_err());
        assert!(server(&["--data-dir", "d", "--udp-port", "0"]).is_err());
    }

    #[test]
    fn relay_disabled() {
        let cfg = server(&["--data-dir", "d", "--relay-mode", "disabled"]).unwrap();
        assert_eq!(cfg.endpoint.relay, RelayChoice::Disabled);
        assert!(!cfg.endpoint.relay.enabled());
    }

    #[test]
    fn connect_args() {
        let args = ConnectArgs::try_parse_from(["kinora-connect", NODE, "--port", "18080", "--relay-only"]).unwrap();
        let cfg = args.to_config().unwrap();
        assert_eq!(cfg.server, NODE);
        assert_eq!(cfg.local_port, Some(18080));
        assert!(cfg.relay_only);
        assert_eq!(cfg.relay_mode, "default");
    }
}
