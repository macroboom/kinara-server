//! JSON Lines contract between kinora-iroh and the Go server.
//!
//! stdout carries `Event` objects, one per line, each tagged by "type" and
//! carrying "v": 1. stdin carries `Command` objects tagged by "cmd".
//! Field names are camelCase. Additive changes keep v = 1; anything that
//! renames or removes a field bumps it.

use serde::{Deserialize, Serialize};

use crate::path::PathType;

pub const VERSION: u32 = 1;

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum Event {
    Ready(Ready),
    Status(Status),
    PeerConnected(PeerChange),
    PeerDisconnected(PeerChange),
    Error(ErrorLine),
}

impl Event {
    pub fn to_line(&self) -> String {
        // Serializing these plain structs cannot fail.
        serde_json::to_string(self).unwrap_or_else(|e| {
            format!(r#"{{"type":"error","v":1,"ts":0,"code":"internal","message":"encode failed: {e}"}}"#)
        })
    }
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Ready {
    pub v: u32,
    /// Unix time in milliseconds.
    pub ts: u64,
    pub node_id: String,
    pub ticket: String,
    /// Home relay URL, if connected to one.
    pub relay: Option<String>,
    pub addrs: Vec<AddrInfo>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct AddrInfo {
    /// "ip:port" or "[ipv6]:port".
    pub addr: String,
    /// "ipv4" or "ipv6".
    pub family: String,
    /// "global", "lan", "link-local" or "loopback".
    pub scope: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Status {
    pub v: u32,
    pub ts: u64,
    pub node_id: String,
    /// Current ticket. Changes when addresses or the home relay change.
    pub ticket: String,
    pub uptime_secs: u64,
    pub relay: RelayInfo,
    pub addrs: Vec<AddrInfo>,
    /// Absent until the first network check finishes.
    pub net_report: Option<NetReportInfo>,
    /// Conditions that are true right now. Each one also produced an
    /// "error" line when it first appeared.
    pub warnings: Vec<Warning>,
    /// Node IDs currently blocked.
    pub blocked: Vec<String>,
    /// Open tunnelled TCP connections across all peers.
    pub active_streams: u64,
    pub peers: Vec<PeerStatus>,
    /// When a device last used a direct path, by family. This is what
    /// actually happened, so it outranks the network report's UDP probes
    /// (which only ask the relay servers and can fail on their own).
    #[serde(default)]
    pub direct_seen: DirectSeen,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct DirectSeen {
    /// Seconds since a device was last seen on a direct IPv4 path.
    pub v4_secs_ago: Option<u64>,
    /// Seconds since a device was last seen on a direct IPv6 path.
    pub v6_secs_ago: Option<u64>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct RelayInfo {
    /// "default", "disabled" or "custom".
    pub mode: String,
    pub url: Option<String>,
    pub connected: bool,
    pub latency_ms: Option<f64>,
    pub last_error: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct NetReportInfo {
    /// Any UDP path works. False means remote devices will use the relay.
    pub udp: bool,
    pub udp_v4: bool,
    pub udp_v6: bool,
    /// Public address as seen by the relay servers.
    pub global_v4: Option<String>,
    pub global_v6: Option<String>,
    /// True means a symmetric NAT, which makes hole punching unlikely.
    pub mapping_varies_by_dest: Option<bool>,
    pub captive_portal: Option<bool>,
    pub preferred_relay: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Warning {
    pub code: String,
    pub message: String,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PeerStatus {
    pub node_id: String,
    pub path: PathType,
    /// The peer's UDP address on a direct path.
    pub remote_addr: Option<String>,
    pub relay_url: Option<String>,
    pub rtt_ms: Option<f64>,
    /// Bytes received from the peer (requests, uploads).
    pub bytes_in: u64,
    /// Bytes sent to the peer (media, API responses).
    pub bytes_out: u64,
    pub open_streams: u64,
    pub total_streams: u64,
    /// Unix time in milliseconds.
    pub connected_since: u64,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct PeerChange {
    pub v: u32,
    pub ts: u64,
    pub node_id: String,
    pub path: PathType,
    pub remote_addr: Option<String>,
    /// Why a peer disconnected, for example "blocked" or "timed out".
    pub reason: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct ErrorLine {
    pub v: u32,
    pub ts: u64,
    /// Stable machine readable code, see the README for the list.
    pub code: String,
    /// Human readable text for the manager UI.
    pub message: String,
}

impl ErrorLine {
    pub fn event(code: &str, message: impl Into<String>) -> Event {
        Event::Error(ErrorLine { v: VERSION, ts: crate::now_ms(), code: code.into(), message: message.into() })
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(tag = "cmd", rename_all = "snake_case")]
pub enum Command {
    Block {
        #[serde(rename = "nodeId")]
        node_id: String,
    },
    Unblock {
        #[serde(rename = "nodeId")]
        node_id: String,
    },
    Status,
    Shutdown,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn commands_parse() {
        let c: Command = serde_json::from_str(r#"{"cmd":"block","nodeId":"abc"}"#).unwrap();
        assert_eq!(c, Command::Block { node_id: "abc".into() });
        let c: Command = serde_json::from_str(r#"{"cmd":"status"}"#).unwrap();
        assert_eq!(c, Command::Status);
        let c: Command = serde_json::from_str(r#"{"cmd":"shutdown","extra":1}"#).unwrap();
        assert_eq!(c, Command::Shutdown);
        assert!(serde_json::from_str::<Command>(r#"{"cmd":"reboot"}"#).is_err());
        assert!(serde_json::from_str::<Command>(r#"{"cmd":"block"}"#).is_err());
    }

    #[test]
    fn events_are_tagged_and_camel_case() {
        let ev = Event::PeerConnected(PeerChange {
            v: VERSION,
            ts: 1,
            node_id: "n".into(),
            path: PathType::DirectV6,
            remote_addr: Some("[2001:db8::1]:5".into()),
            reason: None,
        });
        let line = ev.to_line();
        let v: serde_json::Value = serde_json::from_str(&line).unwrap();
        assert_eq!(v["type"], "peer_connected");
        assert_eq!(v["v"], 1);
        assert_eq!(v["nodeId"], "n");
        assert_eq!(v["path"], "direct-v6");
        assert_eq!(serde_json::from_str::<Event>(&line).unwrap(), ev);

        let err = ErrorLine::event("udp_blocked", "UDP blocked, relay only").to_line();
        let v: serde_json::Value = serde_json::from_str(&err).unwrap();
        assert_eq!(v["type"], "error");
        assert_eq!(v["code"], "udp_blocked");
    }
}
