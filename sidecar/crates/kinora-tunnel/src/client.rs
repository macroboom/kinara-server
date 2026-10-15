//! Client end: a local 127.0.0.1 listener whose every TCP connection is
//! carried as one QUIC stream to the home server.
//!
//! The public API is synchronous and uses only strings, integers, booleans,
//! options and plain structs, so a uniffi wrapper can expose it to Kotlin and
//! Swift unchanged. The handle owns its own tokio runtime.

use std::fmt;
use std::path::PathBuf;
use std::str::FromStr;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use iroh::endpoint::Connection;
use iroh::{Endpoint, EndpointAddr, EndpointId, Watcher};
use iroh_tickets::endpoint::EndpointTicket;
use serde::Serialize;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream};
use tokio::runtime::Runtime;
use tokio::sync::{watch, Notify};
use tracing::{debug, info, warn};

use crate::path::{self, PathType};
use crate::pump::{self, Counters, StreamGuard};
use crate::transport::{self, EndpointOptions, RelayChoice, TransportOptions};
use crate::ALPN;

/// After a failed connect, new local connections get an immediate 502 for
/// this long instead of each waiting out its own attempt.
const FAST_FAIL_WINDOW: Duration = Duration::from_secs(2);
const OPEN_STREAM_TIMEOUT: Duration = Duration::from_secs(30);

#[derive(Debug, Clone)]
pub struct ClientConfig {
    /// Connection ticket from the server, or a bare node ID (hex) that is
    /// then resolved through n0 discovery or mDNS.
    pub server: String,
    /// Folder for this device's own key, so its node ID stays stable.
    pub data_dir: String,
    /// "default", "disabled" or "custom".
    pub relay_mode: String,
    pub relay_urls: Vec<String>,
    /// Testing aid: no IP transports, everything goes through the relay.
    pub relay_only: bool,
    pub discovery_n0: bool,
    pub mdns: bool,
    /// Preferred local port. Falls back to a free port if it is taken.
    pub local_port: Option<u16>,
    pub connect_timeout_ms: u64,
}

impl ClientConfig {
    pub fn new(server: impl Into<String>, data_dir: impl Into<String>) -> Self {
        Self {
            server: server.into(),
            data_dir: data_dir.into(),
            relay_mode: "default".into(),
            relay_urls: Vec::new(),
            relay_only: false,
            discovery_n0: true,
            mdns: true,
            local_port: None,
            connect_timeout_ms: 10_000,
        }
    }
}

#[derive(Debug, Clone, PartialEq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct ClientStatus {
    /// This device's node ID, which the server sees in the PROXY header.
    pub node_id: String,
    pub server_node_id: String,
    pub local_port: u16,
    pub connected: bool,
    /// "direct-v4", "direct-v6", "relay", "mixed" or "none".
    pub path: String,
    pub remote_addr: Option<String>,
    pub relay_url: Option<String>,
    pub home_relay: Option<String>,
    pub rtt_ms: Option<f64>,
    pub bytes_in: u64,
    pub bytes_out: u64,
    pub open_streams: u64,
    pub total_streams: u64,
    pub last_error: Option<String>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum TunnelError {
    InvalidConfig(String),
    Io(String),
    Iroh(String),
}

impl fmt::Display for TunnelError {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            TunnelError::InvalidConfig(m) => write!(f, "invalid config: {m}"),
            TunnelError::Io(m) => write!(f, "io: {m}"),
            TunnelError::Iroh(m) => write!(f, "iroh: {m}"),
        }
    }
}

impl std::error::Error for TunnelError {}

/// Parses a ticket or a bare node ID into an address to dial.
pub fn parse_server(s: &str) -> Result<EndpointAddr, TunnelError> {
    let s = s.trim();
    if let Ok(ticket) = EndpointTicket::from_str(s) {
        return Ok(ticket.into());
    }
    match EndpointId::from_str(s) {
        Ok(id) => Ok(EndpointAddr::new(id)),
        Err(e) => Err(TunnelError::InvalidConfig(format!("server is neither a ticket nor a node ID: {e}"))),
    }
}

struct ClientInner {
    ep: Endpoint,
    server: EndpointAddr,
    /// Serialises connect attempts so a burst of local connections shares one.
    connecting: tokio::sync::Mutex<()>,
    current: Mutex<Option<Connection>>,
    counters: Arc<Counters>,
    active: Arc<AtomicU64>,
    last_error: Mutex<Option<String>>,
    last_failure: Mutex<Option<Instant>>,
    connect_timeout: Duration,
    stopping: AtomicBool,
    /// Poked when a connection dies. Kept separate from `connection()` so the
    /// connect and watch tasks do not form a recursive async type.
    reconnect: Notify,
}

impl ClientInner {
    fn live(&self) -> Option<Connection> {
        let cur = self.current.lock().unwrap_or_else(|e| e.into_inner());
        cur.as_ref().filter(|c| c.close_reason().is_none()).cloned()
    }

    fn set_error(&self, msg: String) {
        *self.last_error.lock().unwrap_or_else(|e| e.into_inner()) = Some(msg);
    }

    /// Returns the live connection, connecting lazily if there is none.
    async fn connection(self: &Arc<Self>) -> Result<Connection, String> {
        if let Some(c) = self.live() {
            return Ok(c);
        }
        let _one_at_a_time = self.connecting.lock().await;
        if let Some(c) = self.live() {
            return Ok(c);
        }
        if self.stopping.load(Ordering::SeqCst) {
            return Err("tunnel stopped".into());
        }
        let recent_failure =
            self.last_failure.lock().unwrap_or_else(|e| e.into_inner()).is_some_and(|t| t.elapsed() < FAST_FAIL_WINDOW);
        if recent_failure {
            let msg = self.last_error.lock().unwrap_or_else(|e| e.into_inner()).clone();
            return Err(msg.unwrap_or_else(|| "server unreachable".into()));
        }

        debug!(server = %self.server.id, "connecting");
        let result = tokio::time::timeout(self.connect_timeout, self.ep.connect(self.server.clone(), ALPN)).await;
        match result {
            Ok(Ok(conn)) => {
                info!(path = path::snapshot(&conn).kind.as_str(), "connected to server");
                *self.current.lock().unwrap_or_else(|e| e.into_inner()) = Some(conn.clone());
                *self.last_failure.lock().unwrap_or_else(|e| e.into_inner()) = None;
                *self.last_error.lock().unwrap_or_else(|e| e.into_inner()) = None;
                tokio::spawn(watch_connection(self.clone(), conn.clone()));
                Ok(conn)
            }
            Ok(Err(e)) => Err(self.connect_failed(e.to_string())),
            Err(_) => Err(self.connect_failed(format!("no answer within {:?}", self.connect_timeout))),
        }
    }

    fn connect_failed(&self, err: String) -> String {
        let msg = format!("cannot reach server: {err}");
        warn!("{msg}");
        *self.last_failure.lock().unwrap_or_else(|e| e.into_inner()) = Some(Instant::now());
        self.set_error(msg.clone());
        msg
    }
}

/// Records why a connection died and asks the reconnect task to bring it
/// back, so a network change does not wait for the app's next request.
async fn watch_connection(inner: Arc<ClientInner>, conn: Connection) {
    let reason = conn.closed().await;
    if inner.stopping.load(Ordering::SeqCst) {
        return;
    }
    let msg = format!("connection lost: {reason}");
    info!("{msg}");
    inner.set_error(msg);
    inner.reconnect.notify_one();
}

/// One immediate attempt per lost connection. Further attempts happen lazily
/// when the app opens a new local connection.
async fn reconnect_loop(inner: Arc<ClientInner>, mut stop: watch::Receiver<bool>) {
    loop {
        tokio::select! {
            _ = stop.changed() => break,
            _ = inner.reconnect.notified() => {
                tokio::time::sleep(Duration::from_millis(500)).await;
                if inner.live().is_none() && !inner.stopping.load(Ordering::SeqCst) {
                    let _ = inner.connection().await;
                }
            }
        }
    }
}

async fn accept_loop(inner: Arc<ClientInner>, listener: TcpListener, mut stop: watch::Receiver<bool>) {
    loop {
        tokio::select! {
            _ = stop.changed() => break,
            accepted = listener.accept() => match accepted {
                Ok((tcp, _)) => {
                    tokio::spawn(handle_local(inner.clone(), tcp));
                }
                Err(e) => {
                    warn!("local accept failed: {e}");
                    tokio::time::sleep(Duration::from_millis(100)).await;
                }
            },
        }
    }
}

async fn handle_local(inner: Arc<ClientInner>, tcp: TcpStream) {
    let _ = tcp.set_nodelay(true);
    let conn = match inner.connection().await {
        Ok(c) => c,
        Err(e) => return reply_bad_gateway(tcp, &e).await,
    };
    let (mut send, mut recv) = match tokio::time::timeout(OPEN_STREAM_TIMEOUT, conn.open_bi()).await {
        Ok(Ok(s)) => s,
        Ok(Err(e)) => return reply_bad_gateway(tcp, &format!("cannot open stream: {e}")).await,
        Err(_) => return reply_bad_gateway(tcp, "server did not grant a stream in time").await,
    };
    let _guard = StreamGuard::new(inner.counters.clone(), inner.active.clone());
    if let Err(e) = pump::pump(tcp, &mut send, &mut recv, &inner.counters).await {
        debug!("stream ended with error: {e}");
        pump::abort(&mut send, &mut recv);
    }
}

async fn reply_bad_gateway(mut tcp: TcpStream, msg: &str) {
    let _ = tcp.write_all(&pump::bad_gateway(msg)).await;
    let _ = tcp.shutdown().await;
    // Read what the app already sent so closing does not turn into a reset
    // that hides the response.
    let mut sink = [0u8; 4096];
    let _ = tokio::time::timeout(Duration::from_secs(1), async {
        while let Ok(n) = tcp.read(&mut sink).await {
            if n == 0 {
                break;
            }
        }
    })
    .await;
}

/// Handle to a running tunnel client.
pub struct TunnelClient {
    runtime: Mutex<Option<Runtime>>,
    inner: Arc<ClientInner>,
    stop: watch::Sender<bool>,
    local_port: u16,
    node_id: String,
}

impl TunnelClient {
    /// Binds the endpoint and the local listener. Does not contact the
    /// server; the first local connection does that.
    pub fn start(config: ClientConfig) -> Result<TunnelClient, TunnelError> {
        let server = parse_server(&config.server)?;
        let relay = RelayChoice::parse(&config.relay_mode, &config.relay_urls).map_err(TunnelError::InvalidConfig)?;
        let opts = EndpointOptions {
            relay,
            discovery_n0: config.discovery_n0,
            mdns: config.mdns,
            udp_port: None,
            ip_transports: !config.relay_only,
            transport: TransportOptions {
                // The server never opens streams towards the client.
                max_incoming_streams: 0,
                ..TransportOptions::default()
            },
        };

        let key_path = PathBuf::from(&config.data_dir).join("iroh").join("secret.key");
        let secret = crate::keys::load_or_create(&key_path).map_err(|e| TunnelError::Io(format!("{e:#}")))?;

        let runtime = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .thread_name("kinora-tunnel")
            .enable_all()
            .build()
            .map_err(|e| TunnelError::Io(e.to_string()))?;

        let connect_timeout = Duration::from_millis(config.connect_timeout_ms.max(1000));
        let preferred = config.local_port.unwrap_or(0);
        let (inner, listener) = runtime.block_on(async {
            let ep = transport::endpoint_builder(&opts, secret)
                .map_err(|e| TunnelError::Iroh(format!("{e:#}")))?
                .bind()
                .await
                .map_err(|e| TunnelError::Iroh(e.to_string()))?;
            if opts.mdns && opts.ip_transports {
                if let Err(e) = transport::attach_mdns(&ep) {
                    warn!("local network discovery unavailable: {e:#}");
                }
            }
            let listener = match TcpListener::bind(("127.0.0.1", preferred)).await {
                Ok(l) => l,
                Err(e) if preferred != 0 => {
                    warn!("port {preferred} unavailable ({e}), using a free port");
                    TcpListener::bind(("127.0.0.1", 0)).await.map_err(|e| TunnelError::Io(e.to_string()))?
                }
                Err(e) => return Err(TunnelError::Io(e.to_string())),
            };
            let inner = Arc::new(ClientInner {
                ep,
                server,
                connecting: tokio::sync::Mutex::new(()),
                current: Mutex::new(None),
                counters: Arc::new(Counters::default()),
                active: Arc::new(AtomicU64::new(0)),
                last_error: Mutex::new(None),
                last_failure: Mutex::new(None),
                connect_timeout,
                stopping: AtomicBool::new(false),
                reconnect: Notify::new(),
            });
            Ok::<_, TunnelError>((inner, listener))
        })?;

        let local_port = listener.local_addr().map_err(|e| TunnelError::Io(e.to_string()))?.port();
        let node_id = inner.ep.id().to_string();
        let (stop, stop_rx) = watch::channel(false);
        runtime.spawn(accept_loop(inner.clone(), listener, stop_rx.clone()));
        runtime.spawn(reconnect_loop(inner.clone(), stop_rx));
        info!(local_port, node_id = %node_id, server = %inner.server.id, "tunnel client started");

        Ok(TunnelClient { runtime: Mutex::new(Some(runtime)), inner, stop, local_port, node_id })
    }

    /// Point the app's HTTP base URL at http://127.0.0.1:<port>.
    pub fn local_port(&self) -> u16 {
        self.local_port
    }

    pub fn node_id(&self) -> String {
        self.node_id.clone()
    }

    pub fn status(&self) -> ClientStatus {
        let inner = &self.inner;
        let conn = inner.live();
        let snap = conn.as_ref().map(path::snapshot);
        let mut relay_watch = inner.ep.home_relay_status();
        let relays = relay_watch.get();
        let home_relay = relays.iter().find(|r| r.is_connected()).map(|r| r.url().to_string());
        ClientStatus {
            node_id: self.node_id.clone(),
            server_node_id: inner.server.id.to_string(),
            local_port: self.local_port,
            connected: conn.is_some(),
            path: snap.as_ref().map(|s| s.kind).unwrap_or(PathType::None).as_str().to_string(),
            remote_addr: snap.as_ref().and_then(|s| s.remote_addr).map(|a| a.to_string()),
            relay_url: snap.as_ref().and_then(|s| s.relay_url.clone()),
            home_relay,
            rtt_ms: snap.as_ref().and_then(|s| s.rtt).map(|d| (d.as_secs_f64() * 10_000.0).round() / 10.0),
            bytes_in: inner.counters.bytes_in.load(Ordering::Relaxed),
            bytes_out: inner.counters.bytes_out.load(Ordering::Relaxed),
            open_streams: inner.counters.open_streams.load(Ordering::Relaxed),
            total_streams: inner.counters.total_streams.load(Ordering::Relaxed),
            last_error: inner.last_error.lock().unwrap_or_else(|e| e.into_inner()).clone(),
        }
    }

    /// Mobile apps call this on connectivity changes (Wi-Fi to cellular).
    /// iroh then re-probes paths instead of waiting for timeouts.
    pub fn network_change(&self) {
        let rt = self.runtime.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(rt) = rt.as_ref() {
            let inner = self.inner.clone();
            rt.spawn(async move { inner.ep.network_change().await });
        }
    }

    /// Stops the listener, closes the connection and the runtime. Safe to
    /// call more than once and from any thread.
    pub fn stop(&self) {
        let Some(rt) = self.runtime.lock().unwrap_or_else(|e| e.into_inner()).take() else {
            return;
        };
        self.inner.stopping.store(true, Ordering::SeqCst);
        let _ = self.stop.send(true);
        let inner = self.inner.clone();
        let finish = move || {
            rt.block_on(async {
                if let Some(c) = inner.live() {
                    c.close(0u8.into(), b"client stopping");
                }
                let _ = tokio::time::timeout(Duration::from_millis(500), inner.ep.close()).await;
            });
            rt.shutdown_timeout(Duration::from_secs(1));
        };
        // block_on panics inside an async context, so hop to a plain thread then.
        if tokio::runtime::Handle::try_current().is_ok() {
            let _ = std::thread::spawn(finish).join();
        } else {
            finish();
        }
    }
}

impl Drop for TunnelClient {
    fn drop(&mut self) {
        self.stop();
    }
}
