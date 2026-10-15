//! Classifies which network path a connection is using right now.

use std::net::SocketAddr;
use std::time::Duration;

use iroh::endpoint::Connection;
use iroh::TransportAddr;
use serde::{Deserialize, Serialize};

use crate::addr::canonical;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub enum PathType {
    #[serde(rename = "direct-v4")]
    DirectV4,
    #[serde(rename = "direct-v6")]
    DirectV6,
    #[serde(rename = "relay")]
    Relay,
    /// Several paths carry data, or iroh has not settled on one yet.
    #[serde(rename = "mixed")]
    Mixed,
    /// No open path, for example while disconnected.
    #[serde(rename = "none")]
    None,
}

impl PathType {
    pub fn as_str(self) -> &'static str {
        match self {
            PathType::DirectV4 => "direct-v4",
            PathType::DirectV6 => "direct-v6",
            PathType::Relay => "relay",
            PathType::Mixed => "mixed",
            PathType::None => "none",
        }
    }

    /// Value for the PROXY path TLV. A stream can only exist on an open
    /// connection, so "none" is reported as relay, the most conservative case.
    pub fn proxy_label(self) -> &'static str {
        match self {
            PathType::None => "relay",
            other => other.as_str(),
        }
    }

    fn for_ip(addr: SocketAddr) -> PathType {
        match canonical(addr) {
            SocketAddr::V4(_) => PathType::DirectV4,
            SocketAddr::V6(_) => PathType::DirectV6,
        }
    }
}

#[derive(Debug, Clone, PartialEq)]
pub struct PathSnapshot {
    pub kind: PathType,
    /// The peer's UDP address when a direct path is in use.
    pub remote_addr: Option<SocketAddr>,
    /// The relay in use, if any path goes through one.
    pub relay_url: Option<String>,
    pub rtt: Option<Duration>,
}

pub fn snapshot(conn: &Connection) -> PathSnapshot {
    let mut sel_ip: Vec<(SocketAddr, Duration)> = Vec::new();
    let mut sel_relay: Vec<(String, Duration)> = Vec::new();
    let mut ip: Vec<(SocketAddr, Duration)> = Vec::new();
    let mut relay: Vec<(String, Duration)> = Vec::new();

    let paths = conn.paths();
    for p in paths.iter() {
        let rtt = p.rtt();
        match p.remote_addr() {
            TransportAddr::Ip(a) => {
                let a = canonical(*a);
                if p.is_selected() {
                    sel_ip.push((a, rtt));
                } else {
                    ip.push((a, rtt));
                }
            }
            TransportAddr::Relay(url) => {
                if p.is_selected() {
                    sel_relay.push((url.to_string(), rtt));
                } else {
                    relay.push((url.to_string(), rtt));
                }
            }
            _ => {}
        }
    }

    let kind = match (sel_ip.len(), sel_relay.len()) {
        (1, 0) => PathType::for_ip(sel_ip[0].0),
        (0, 1) => PathType::Relay,
        (0, 0) => match (ip.is_empty(), relay.is_empty()) {
            (true, true) => PathType::None,
            (false, true) => {
                let first = PathType::for_ip(ip[0].0);
                if ip.iter().all(|(a, _)| PathType::for_ip(*a) == first) {
                    first
                } else {
                    PathType::Mixed
                }
            }
            (true, false) => PathType::Relay,
            (false, false) => PathType::Mixed,
        },
        _ => PathType::Mixed,
    };

    let remote_addr = match kind {
        PathType::DirectV4 | PathType::DirectV6 | PathType::Mixed => sel_ip.first().or(ip.first()).map(|(a, _)| *a),
        _ => None,
    };
    let relay_url = sel_relay.first().or(relay.first()).map(|(u, _)| u.clone());
    // Prefer the RTT of the path carrying data; otherwise the best open path.
    let rtt = sel_ip
        .first()
        .map(|(_, r)| *r)
        .or_else(|| sel_relay.first().map(|(_, r)| *r))
        .or_else(|| ip.iter().map(|(_, r)| *r).chain(relay.iter().map(|(_, r)| *r)).min());

    PathSnapshot { kind, remote_addr, relay_url, rtt }
}
