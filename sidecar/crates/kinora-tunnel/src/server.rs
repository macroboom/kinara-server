//! Server end: accepts tunnel connections from devices and forwards each
//! stream to the Go server's PROXY tunnel listener.

use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::net::SocketAddr;
use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, RwLock};
use std::time::{Duration, Instant};

use anyhow::Result;
use iroh::endpoint::{AfterHandshakeOutcome, Connection, EndpointHooks, RecvStream, SendStream, VarInt};
use iroh::{Endpoint, EndpointId, Watcher};
use iroh_tickets::endpoint::EndpointTicket;
use tokio::io::AsyncWriteExt;
use tokio::net::TcpStream;
use tokio::sync::mpsc;
use tracing::{debug, info, warn};

use crate::events::{
    AddrInfo, DirectSeen, ErrorLine, Event, NetReportInfo, PeerChange, PeerStatus, Ready, RelayInfo, Status, Warning,
    VERSION,
};
use crate::path::{self, PathType};
use crate::pump::{self, Counters, StreamGuard};
use crate::ratelimit::RateWindow;
use crate::transport::{self, EndpointOptions, RelayChoice};
use crate::{addr, close_codes, now_ms, proxy_v2, ErrStr, ALPN};

const TARGET_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// How long a failed connect to the Go listener stays in the warnings list.
const TARGET_WARNING_TTL: Duration = Duration::from_secs(60);
/// Grace before "no relay" is reported, so a normal startup is not flagged.
const RELAY_WARNING_AFTER: Duration = Duration::from_secs(20);
/// A direct path seen this recently proves UDP works, whatever the network
/// report's probes say.
const DIRECT_SEEN_FRESH: Duration = Duration::from_secs(30 * 60);

#[derive(Debug, Clone)]
pub struct ServerConfig {
    /// The key lives at `<data_dir>/iroh/secret.key`.
    pub data_dir: PathBuf,
    /// The Go server's PROXY tunnel listener, normally 127.0.0.1:8421.
    pub target: String,
    pub endpoint: EndpointOptions,
    /// New connections allowed per peer per minute; 0 disables the limit.
    pub max_conns_per_minute: u32,
    /// New connections allowed from all peers together per minute; 0
    /// disables it. The per-peer limit alone is easy to dodge, since a new
    /// node ID costs nothing.
    pub max_handshakes_per_minute: u32,
    /// Sent to kinora-server in every PROXY header (TLV 0xE2).
    pub secret: Option<String>,
    /// Peers refused from the start, so a restart has no unblocked window.
    pub blocked: Vec<EndpointId>,
}

/// Decides in the handshake hook whether a peer may connect at all.
#[derive(Debug)]
struct GateState {
    blocked: RwLock<HashSet<EndpointId>>,
    window: Mutex<RateWindow<EndpointId>>,
    all: Mutex<RateWindow<()>>,
}

impl GateState {
    fn is_blocked(&self, id: &EndpointId) -> bool {
        self.blocked.read().unwrap_or_else(|e| e.into_inner()).contains(id)
    }

    fn verdict(&self, id: &EndpointId) -> Option<(u32, &'static [u8])> {
        if self.is_blocked(id) {
            return Some((close_codes::BLOCKED, b"blocked"));
        }
        let now = Instant::now();
        let mut window = self.window.lock().unwrap_or_else(|e| e.into_inner());
        if !window.allow(id, now) {
            return Some((close_codes::RATE_LIMITED, b"too many connections"));
        }
        drop(window);
        if !self.all.lock().unwrap_or_else(|e| e.into_inner()).allow(&(), now) {
            return Some((close_codes::RATE_LIMITED, b"server busy"));
        }
        None
    }
}

#[derive(Debug, Clone)]
struct Gate(Arc<GateState>);

impl EndpointHooks for Gate {
    fn after_handshake<'a>(&'a self, conn: &'a Connection) -> impl Future<Output = AfterHandshakeOutcome> + Send + 'a {
        // Decide synchronously so no lock is held across an await.
        let outcome = match self.0.verdict(&conn.remote_id()) {
            None => AfterHandshakeOutcome::Accept,
            Some((code, reason)) => {
                debug!(peer = %conn.remote_id(), "refusing connection: {}", String::from_utf8_lossy(reason));
                AfterHandshakeOutcome::Reject { error_code: VarInt::from_u32(code), reason: reason.to_vec() }
            }
        };
        async move { outcome }
    }
}

struct PeerEntry {
    conn: Connection,
    generation: u64,
    counters: Arc<Counters>,
    since_ms: u64,
}

struct Inner {
    ep: Endpoint,
    target: String,
    relay: RelayChoice,
    gate: Arc<GateState>,
    peers: Mutex<HashMap<EndpointId, PeerEntry>>,
    next_generation: AtomicU64,
    events: mpsc::UnboundedSender<Event>,
    active_streams: Arc<AtomicU64>,
    started: Instant,
    shutting_down: AtomicBool,
    mdns_error: Option<String>,
    target_failure: Mutex<Option<(Instant, String)>>,
    /// Last time any device was on a direct IPv4 / IPv6 path.
    direct_seen: Mutex<(Option<Instant>, Option<Instant>)>,
    local_v4: Option<SocketAddr>,
    local_v6: Option<SocketAddr>,
    secret: Option<String>,
}

/// Handle to the running server end. Cheap to clone.
#[derive(Clone)]
pub struct Server {
    inner: Arc<Inner>,
}

impl Server {
    /// Loads or creates the identity, binds the endpoint and starts accepting.
    /// Peer events and one-off errors are sent on `events`.
    pub async fn bind(cfg: ServerConfig, events: mpsc::UnboundedSender<Event>) -> Result<Server> {
        let key_path = cfg.data_dir.join("iroh").join("secret.key");
        let secret = crate::keys::load_or_create(&key_path)?;

        let gate = Arc::new(GateState {
            blocked: RwLock::new(cfg.blocked.iter().copied().collect()),
            window: Mutex::new(RateWindow::new(cfg.max_conns_per_minute, Duration::from_secs(60))),
            all: Mutex::new(RateWindow::new(cfg.max_handshakes_per_minute, Duration::from_secs(60))),
        });

        let ep = transport::endpoint_builder(&cfg.endpoint, secret)?
            .alpns(vec![ALPN.to_vec()])
            .hooks(Gate(gate.clone()))
            .bind()
            .await
            .err_str("binding iroh endpoint")?;

        let mdns_error = if cfg.endpoint.mdns {
            match transport::attach_mdns(&ep) {
                Ok(()) => None,
                Err(e) => {
                    warn!("local network discovery unavailable: {e:#}");
                    Some(format!("{e:#}"))
                }
            }
        } else {
            None
        };

        let bound = ep.bound_sockets();
        info!(node_id = %ep.id(), ?bound, target = %cfg.target, "iroh endpoint bound");
        let local_v4 = bound.iter().copied().find(|a| a.is_ipv4());
        let local_v6 = bound.iter().copied().find(|a| a.is_ipv6());

        let inner = Arc::new(Inner {
            ep,
            target: cfg.target,
            relay: cfg.endpoint.relay,
            gate,
            peers: Mutex::new(HashMap::new()),
            next_generation: AtomicU64::new(1),
            events,
            active_streams: Arc::new(AtomicU64::new(0)),
            started: Instant::now(),
            shutting_down: AtomicBool::new(false),
            mdns_error,
            target_failure: Mutex::new(None),
            direct_seen: Mutex::new((None, None)),
            local_v4,
            local_v6,
            secret: cfg.secret,
        });
        tokio::spawn(accept_loop(inner.clone()));
        Ok(Server { inner })
    }

    pub fn node_id(&self) -> EndpointId {
        self.inner.ep.id()
    }

    pub fn endpoint(&self) -> &Endpoint {
        &self.inner.ep
    }

    /// Waits until a home relay is connected, up to `limit`. Returns at once
    /// (after a short pause for address discovery) when relays are disabled.
    pub async fn wait_online(&self, limit: Duration) -> bool {
        if !self.inner.relay.enabled() {
            tokio::time::sleep(Duration::from_millis(300)).await;
            return true;
        }
        tokio::time::timeout(limit, self.inner.ep.online()).await.is_ok()
    }

    pub fn ready(&self) -> Ready {
        let st = self.status();
        Ready { v: VERSION, ts: now_ms(), node_id: st.node_id, ticket: st.ticket, relay: st.relay.url, addrs: st.addrs }
    }

    pub fn ticket(&self) -> String {
        EndpointTicket::new(self.inner.ep.addr()).to_string()
    }

    pub fn status(&self) -> Status {
        let inner = &self.inner;
        let ep = &inner.ep;
        let addr = ep.addr();
        let ticket = EndpointTicket::new(addr.clone()).to_string();

        let mut addrs: Vec<AddrInfo> = addr
            .ip_addrs()
            .map(|a| {
                let a = addr::canonical(*a);
                AddrInfo { addr: a.to_string(), family: addr::family(a).into(), scope: addr::scope(a.ip()).into() }
            })
            .collect();
        addrs.sort_by(|a, b| a.addr.cmp(&b.addr));

        let mut net_watch = ep.net_report();
        let report = net_watch.get();

        let mut relay_watch = ep.home_relay_status();
        let relays = relay_watch.get();
        let home = relays.iter().find(|r| r.is_connected()).or(relays.first());
        let relay_url = home.map(|r| r.url().to_string());
        let latency_ms = match (&report, &relay_url) {
            (Some(rep), Some(url)) => {
                rep.relay_latency.iter().filter(|(_, u, _)| u.to_string() == *url).map(|(_, _, d)| d).min().map(ms)
            }
            _ => None,
        };
        let relay = RelayInfo {
            mode: inner.relay.label().into(),
            url: relay_url,
            connected: home.is_some_and(|r| r.is_connected()),
            latency_ms,
            last_error: home.and_then(|r| r.last_error().map(|e| e.to_string())),
        };

        let net_report = report.as_ref().map(|r| NetReportInfo {
            udp: r.has_udp(),
            udp_v4: r.udp_v4,
            udp_v6: r.udp_v6,
            global_v4: r.global_v4.map(|a| a.to_string()),
            global_v6: r.global_v6.map(|a| a.to_string()),
            mapping_varies_by_dest: r.mapping_varies_by_dest(),
            captive_portal: r.captive_portal,
            preferred_relay: r.preferred_relay.as_ref().map(|u| u.to_string()),
        });

        let mut peers: Vec<PeerStatus> = {
            let map = inner.peers.lock().unwrap_or_else(|e| e.into_inner());
            map.iter()
                .map(|(id, p)| {
                    let snap = path::snapshot(&p.conn);
                    note_direct(inner, snap.kind);
                    PeerStatus {
                        node_id: id.to_string(),
                        path: snap.kind,
                        remote_addr: snap.remote_addr.map(|a| a.to_string()),
                        relay_url: snap.relay_url,
                        rtt_ms: snap.rtt.map(ms),
                        bytes_in: p.counters.bytes_in.load(Ordering::Relaxed),
                        bytes_out: p.counters.bytes_out.load(Ordering::Relaxed),
                        open_streams: p.counters.open_streams.load(Ordering::Relaxed),
                        total_streams: p.counters.total_streams.load(Ordering::Relaxed),
                        connected_since: p.since_ms,
                    }
                })
                .collect()
        };
        peers.sort_by(|a, b| a.node_id.cmp(&b.node_id));

        let mut blocked: Vec<String> =
            inner.gate.blocked.read().unwrap_or_else(|e| e.into_inner()).iter().map(|id| id.to_string()).collect();
        blocked.sort();

        let mut st = Status {
            v: VERSION,
            ts: now_ms(),
            node_id: ep.id().to_string(),
            ticket,
            uptime_secs: inner.started.elapsed().as_secs(),
            relay,
            addrs,
            net_report,
            warnings: Vec::new(),
            blocked,
            active_streams: inner.active_streams.load(Ordering::Relaxed),
            peers,
            direct_seen: {
                let (v4, v6) = *inner.direct_seen.lock().unwrap_or_else(|e| e.into_inner());
                DirectSeen {
                    v4_secs_ago: v4.map(|t| t.elapsed().as_secs()),
                    v6_secs_ago: v6.map(|t| t.elapsed().as_secs()),
                }
            },
        };
        st.warnings = self.warnings(&st);
        st
    }

    /// Conditions worth showing in the manager UI. The binary turns newly
    /// appearing ones into "error" lines.
    fn warnings(&self, st: &Status) -> Vec<Warning> {
        let inner = &self.inner;
        let mut out = Vec::new();
        let mut push = |code: &str, message: String| out.push(Warning { code: code.into(), message });

        if let Some(nr) = &st.net_report {
            // The report's UDP check only asks the relay servers. With one
            // relay of our own, that relay not answering looks exactly like
            // a blocked network, so check what devices really did first.
            let fresh = |s: Option<u64>| s.is_some_and(|s| s < DIRECT_SEEN_FRESH.as_secs());
            let direct_lately = fresh(st.direct_seen.v4_secs_ago) || fresh(st.direct_seen.v6_secs_ago);
            if !nr.udp && direct_lately {
                // Devices connect directly, so UDP works; the probe was wrong.
            } else if !nr.udp && matches!(inner.relay, RelayChoice::Custom(_)) && st.relay.connected {
                let host = st.relay.url.as_deref().map(relay_host).unwrap_or("your relay");
                push(
                    "udp_unverified",
                    format!(
                        "Couldn't check direct connections: the relay {host} didn't answer the UDP check (UDP port 7842 by default). Devices may still connect directly. If this stays, allow UDP 7842 for IPv4 and IPv6 on the relay server."
                    ),
                );
            } else if !nr.udp {
                push("udp_blocked", "UDP blocked, relay only. Remote playback will go through the relay.".into());
            } else if nr.mapping_varies_by_dest == Some(true) && !st.addrs.iter().any(|a| a.scope == "global") {
                push(
                    "symmetric_nat",
                    "This network uses a symmetric NAT and has no global IPv6 address, so most remote devices will use the relay. A fixed --udp-port with a port forward fixes this.".into(),
                );
            }
        }
        if inner.relay.enabled() && !st.relay.connected && inner.started.elapsed() > RELAY_WARNING_AFTER {
            let detail = st.relay.last_error.clone().unwrap_or_else(|| "no response".into());
            push("relay_unreachable", format!("Cannot reach the relay server: {detail}"));
        }
        if let Some(err) = &inner.mdns_error {
            push("mdns_unavailable", format!("Local network discovery is off: {err}"));
        }
        if let Some((when, err)) = inner.target_failure.lock().unwrap_or_else(|e| e.into_inner()).as_ref() {
            if when.elapsed() < TARGET_WARNING_TTL {
                push(
                    "target_unreachable",
                    format!("Cannot reach the kinora-server tunnel listener at {}: {err}", inner.target),
                );
            }
        }
        out
    }

    /// Drops the peer now and refuses it until unblocked.
    pub fn block(&self, id: EndpointId) {
        self.inner.gate.blocked.write().unwrap_or_else(|e| e.into_inner()).insert(id);
        let conn = self.inner.peers.lock().unwrap_or_else(|e| e.into_inner()).get(&id).map(|p| p.conn.clone());
        if let Some(conn) = conn {
            info!(peer = %id, "blocking connected peer");
            conn.close(VarInt::from_u32(close_codes::BLOCKED), b"blocked");
        }
    }

    pub fn unblock(&self, id: EndpointId) {
        self.inner.gate.blocked.write().unwrap_or_else(|e| e.into_inner()).remove(&id);
    }

    /// Stops accepting, waits up to `grace` for open streams, then closes.
    pub async fn shutdown(&self, grace: Duration) {
        let inner = &self.inner;
        inner.shutting_down.store(true, Ordering::SeqCst);
        let deadline = Instant::now() + grace;
        while inner.active_streams.load(Ordering::Relaxed) > 0 && Instant::now() < deadline {
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
        let conns: Vec<Connection> =
            inner.peers.lock().unwrap_or_else(|e| e.into_inner()).values().map(|p| p.conn.clone()).collect();
        for c in conns {
            c.close(VarInt::from_u32(close_codes::SHUTDOWN), b"server shutting down");
        }
        // Bounded so a dead network cannot hold the process past its deadline.
        let _ = tokio::time::timeout(Duration::from_millis(500), inner.ep.close()).await;
    }
}

fn ms(d: Duration) -> f64 {
    (d.as_secs_f64() * 10_000.0).round() / 10.0
}

async fn accept_loop(inner: Arc<Inner>) {
    while let Some(incoming) = inner.ep.accept().await {
        if inner.shutting_down.load(Ordering::SeqCst) {
            incoming.refuse();
            continue;
        }
        let inner = inner.clone();
        tokio::spawn(async move {
            match incoming.await {
                Ok(conn) => handle_connection(inner, conn).await,
                Err(e) => debug!("incoming connection failed: {e}"),
            }
        });
    }
    debug!("accept loop ended");
}

async fn handle_connection(inner: Arc<Inner>, conn: Connection) {
    if conn.alpn() != ALPN {
        conn.close(VarInt::from_u32(close_codes::BAD_ALPN), b"unsupported protocol");
        return;
    }
    let peer = conn.remote_id();
    // The hook already checked this; re-check in case a block raced the handshake.
    if inner.gate.is_blocked(&peer) {
        conn.close(VarInt::from_u32(close_codes::BLOCKED), b"blocked");
        return;
    }

    let generation = inner.next_generation.fetch_add(1, Ordering::Relaxed);
    let counters = Arc::new(Counters::default());
    let replaced = inner
        .peers
        .lock()
        .unwrap_or_else(|e| e.into_inner())
        .insert(peer, PeerEntry { conn: conn.clone(), generation, counters: counters.clone(), since_ms: now_ms() });
    // One connection per device: a reconnect replaces the old one, which is
    // usually already dead after a network change.
    if let Some(old) = replaced {
        old.conn.close(VarInt::from_u32(close_codes::SUPERSEDED), b"superseded by a new connection");
    }

    let snap = path::snapshot(&conn);
    note_direct(&inner, snap.kind);
    info!(peer = %peer, path = snap.kind.as_str(), "peer connected");
    let _ = inner.events.send(Event::PeerConnected(PeerChange {
        v: VERSION,
        ts: now_ms(),
        node_id: peer.to_string(),
        path: snap.kind,
        remote_addr: snap.remote_addr.map(|a| a.to_string()),
        reason: None,
    }));

    let reason = loop {
        match conn.accept_bi().await {
            Ok((send, recv)) => {
                tokio::spawn(handle_stream(inner.clone(), conn.clone(), peer, counters.clone(), send, recv));
            }
            Err(e) => break e.to_string(),
        }
    };

    let removed = {
        let mut map = inner.peers.lock().unwrap_or_else(|e| e.into_inner());
        if map.get(&peer).is_some_and(|p| p.generation == generation) {
            map.remove(&peer);
            true
        } else {
            false
        }
    };
    // A superseded connection ends quietly; the peer is still connected.
    if removed {
        let reason = if inner.gate.is_blocked(&peer) { "blocked".to_string() } else { reason };
        info!(peer = %peer, %reason, "peer disconnected");
        let _ = inner.events.send(Event::PeerDisconnected(PeerChange {
            v: VERSION,
            ts: now_ms(),
            node_id: peer.to_string(),
            path: PathType::None,
            remote_addr: None,
            reason: Some(reason),
        }));
    }
}

async fn handle_stream(
    inner: Arc<Inner>,
    conn: Connection,
    peer: EndpointId,
    counters: Arc<Counters>,
    mut send: SendStream,
    mut recv: RecvStream,
) {
    let _guard = StreamGuard::new(counters.clone(), inner.active_streams.clone());

    // Path is sampled per stream so the Go side always sees where this
    // particular request came from.
    let snap = path::snapshot(&conn);
    let local = match snap.remote_addr.map(addr::canonical) {
        Some(SocketAddr::V4(_)) => inner.local_v4,
        Some(SocketAddr::V6(_)) => inner.local_v6,
        None => None,
    };
    let header = proxy_v2::Header::for_peer(snap.remote_addr, local, &peer.to_string(), snap.kind.proxy_label())
        .with_secret(inner.secret.as_deref())
        .encode();

    let tcp = match tokio::time::timeout(TARGET_CONNECT_TIMEOUT, TcpStream::connect(inner.target.as_str())).await {
        Ok(Ok(tcp)) => tcp,
        Ok(Err(e)) => return target_unreachable(&inner, e.to_string(), &mut send, &mut recv).await,
        Err(_) => return target_unreachable(&inner, "connect timed out".into(), &mut send, &mut recv).await,
    };
    let _ = tcp.set_nodelay(true);
    let mut tcp = tcp;
    if let Err(e) = tcp.write_all(&header).await {
        return target_unreachable(&inner, e.to_string(), &mut send, &mut recv).await;
    }
    *inner.target_failure.lock().unwrap_or_else(|e| e.into_inner()) = None;

    if let Err(e) = pump::pump(tcp, &mut send, &mut recv, &counters).await {
        debug!(peer = %peer, "stream ended with error: {e}");
        pump::abort(&mut send, &mut recv);
    }
}

/// Answers the stream with a 502 so the app gets a real HTTP error, and
/// records the failure for the warnings list.
async fn target_unreachable(inner: &Inner, err: String, send: &mut SendStream, recv: &mut RecvStream) {
    let first = {
        let mut slot = inner.target_failure.lock().unwrap_or_else(|e| e.into_inner());
        let first = slot.is_none();
        *slot = Some((Instant::now(), err.clone()));
        first
    };
    if first {
        warn!(target = %inner.target, "cannot reach kinora-server tunnel listener: {err}");
        let _ = inner.events.send(ErrorLine::event(
            "target_unreachable",
            format!("Cannot reach the kinora-server tunnel listener at {}: {err}", inner.target),
        ));
    }
    let body = pump::bad_gateway("Kinora server is not accepting tunnel connections");
    if SendStream::write_all(send, &body).await.is_ok() {
        let _ = send.finish();
        // Drain the request so the client does not see a stop before reading the reply.
        let _ = tokio::time::timeout(Duration::from_secs(2), RecvStream::read_to_end(recv, 1 << 20)).await;
    }
}

/// Records that a device is on a direct path right now.
fn note_direct(inner: &Inner, kind: PathType) {
    let mut seen = inner.direct_seen.lock().unwrap_or_else(|e| e.into_inner());
    match kind {
        PathType::DirectV4 => seen.0 = Some(Instant::now()),
        PathType::DirectV6 => seen.1 = Some(Instant::now()),
        _ => {}
    }
}

/// "iroh.relay.example.com" from "https://iroh.relay.example.com:8443/".
fn relay_host(url: &str) -> &str {
    let rest = url.split_once("://").map_or(url, |(_, r)| r);
    let host = rest.split(['/', '?']).next().unwrap_or(rest);
    if host.starts_with('[') {
        return host.split_once(']').map_or(host, |(h, _)| &h[1..]);
    }
    host.rsplit_once(':').map_or(host, |(h, _)| h)
}

#[cfg(test)]
mod reach_tests {
    use super::relay_host;

    #[test]
    fn relay_host_strips_scheme_port_and_path() {
        assert_eq!(relay_host("https://iroh.relay.kinara.media:8443/"), "iroh.relay.kinara.media");
        assert_eq!(relay_host("https://relay.example.com/"), "relay.example.com");
        assert_eq!(relay_host("https://[2001:db8::1]:8443/"), "2001:db8::1");
    }
}
