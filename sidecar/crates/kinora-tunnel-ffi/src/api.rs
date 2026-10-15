//! The tunnel calls behind both bindings, with JSON in and JSON out so the
//! C and JNI layers only move strings.
//!
//! Every answer is an object with "ok": true plus the result, or "ok": false
//! and "error". Tunnels are referred to by a number (the handle) so several
//! can run at once (switching servers starts the new one before stopping
//! the old).

use std::collections::{HashMap, VecDeque};
use std::io::Write;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use kinora_tunnel::{ClientConfig, TunnelClient};
use serde::Deserialize;
use serde_json::{json, Value};

/// What the app sends to start a tunnel. Field names are camelCase.
#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct StartConfig {
    /// The server's connection ticket, or its node ID.
    server: String,
    /// A folder the app owns; this device's key is kept in <dataDir>/iroh/.
    data_dir: String,
    /// "default" (n0's relays), "disabled" or "custom".
    #[serde(default = "default_relay_mode")]
    relay_mode: String,
    #[serde(default)]
    relay_urls: Vec<String>,
    #[serde(default)]
    relay_only: bool,
    #[serde(default = "yes")]
    discovery_n0: bool,
    #[serde(default = "yes")]
    mdns: bool,
    #[serde(default)]
    local_port: Option<u16>,
    #[serde(default = "default_connect_timeout")]
    connect_timeout_ms: u64,
}

fn default_relay_mode() -> String {
    "default".into()
}

fn yes() -> bool {
    true
}

fn default_connect_timeout() -> u64 {
    10_000
}

impl StartConfig {
    fn into_client(self) -> ClientConfig {
        let mut c = ClientConfig::new(self.server, self.data_dir);
        // A relay list means "custom", whatever mode was sent with it.
        c.relay_mode = if self.relay_urls.is_empty() { self.relay_mode } else { "custom".into() };
        c.relay_urls = self.relay_urls;
        c.relay_only = self.relay_only;
        c.discovery_n0 = self.discovery_n0;
        c.mdns = self.mdns;
        c.local_port = self.local_port;
        c.connect_timeout_ms = self.connect_timeout_ms;
        c
    }
}

fn tunnels() -> &'static Mutex<HashMap<u64, Arc<TunnelClient>>> {
    static T: OnceLock<Mutex<HashMap<u64, Arc<TunnelClient>>>> = OnceLock::new();
    T.get_or_init(|| Mutex::new(HashMap::new()))
}

static NEXT_HANDLE: AtomicU64 = AtomicU64::new(1);

fn get(handle: u64) -> Option<Arc<TunnelClient>> {
    tunnels().lock().unwrap_or_else(|e| e.into_inner()).get(&handle).cloned()
}

fn fail(msg: impl Into<String>) -> String {
    encode(&json!({ "ok": false, "error": msg.into() }))
}

/// Starts a tunnel. Binds the endpoint and a 127.0.0.1 port; the server is
/// dialled on the first connection to that port. Blocks for the bind, so
/// call it off the UI thread.
pub fn start(config_json: &str) -> String {
    init_logging();
    let cfg: StartConfig = match serde_json::from_str(config_json) {
        Ok(c) => c,
        Err(e) => return fail(format!("bad tunnel settings: {e}")),
    };
    if cfg.server.trim().is_empty() {
        return fail("no server ticket");
    }
    if cfg.data_dir.trim().is_empty() {
        return fail("no data folder");
    }
    let client = match TunnelClient::start(cfg.into_client()) {
        Ok(c) => Arc::new(c),
        Err(e) => return fail(e.to_string()),
    };
    let handle = NEXT_HANDLE.fetch_add(1, Ordering::Relaxed);
    let port = client.local_port();
    let node_id = client.node_id();
    tunnels().lock().unwrap_or_else(|e| e.into_inner()).insert(handle, client);
    encode(&json!({
        "ok": true,
        "handle": handle,
        "port": port,
        "url": format!("http://127.0.0.1:{port}"),
        "nodeId": node_id,
    }))
}

/// Connection state: path ("direct-v4", "direct-v6", "relay", "mixed",
/// "none"), round trip time, bytes and the last error.
pub fn status(handle: u64) -> String {
    let Some(client) = get(handle) else {
        return fail("no such tunnel");
    };
    let mut v = serde_json::to_value(client.status()).unwrap_or_else(|_| json!({}));
    if let Some(obj) = v.as_object_mut() {
        obj.insert("ok".into(), Value::Bool(true));
        obj.insert("handle".into(), json!(handle));
    }
    encode(&v)
}

/// Tells every tunnel the network changed (Wi-Fi to mobile data), so iroh
/// looks for new paths now instead of waiting for the old one to time out.
pub fn network_change() {
    let all: Vec<Arc<TunnelClient>> = tunnels().lock().unwrap_or_else(|e| e.into_inner()).values().cloned().collect();
    for c in all {
        c.network_change();
    }
}

/// Stops one tunnel. Unknown handles are ignored.
pub fn stop(handle: u64) {
    let client = tunnels().lock().unwrap_or_else(|e| e.into_inner()).remove(&handle);
    // Outside the lock: stopping waits up to about 1.5 s for a clean close.
    if let Some(c) = client {
        c.stop();
    }
}

/// Stops every tunnel (the app is closing).
pub fn stop_all() {
    let all: Vec<Arc<TunnelClient>> =
        tunnels().lock().unwrap_or_else(|e| e.into_inner()).drain().map(|(_, c)| c).collect();
    for c in all {
        c.stop();
    }
}

// --- Logs ---------------------------------------------------------------------------------

/// Same levels as the command line tools: kinora lines at info, iroh quiet.
const LOG_FILTER: &str =
    "info,iroh=warn,noq=warn,iroh_relay=warn,portmapper=warn,iroh_mdns_address_lookup=warn,swarm_discovery=warn";
const LOG_KEEP: usize = 400;

fn log_lines() -> &'static Mutex<VecDeque<String>> {
    static L: OnceLock<Mutex<VecDeque<String>>> = OnceLock::new();
    L.get_or_init(|| Mutex::new(VecDeque::new()))
}

/// Collects log output in memory; the app reads it with `drain_logs` (the
/// Android module copies it to Logcat). Phones have no console for stderr.
struct LogSink;

impl Write for LogSink {
    fn write(&mut self, buf: &[u8]) -> std::io::Result<usize> {
        let text = String::from_utf8_lossy(buf);
        let mut lines = log_lines().lock().unwrap_or_else(|e| e.into_inner());
        for line in text.lines().filter(|l| !l.trim().is_empty()) {
            if lines.len() >= LOG_KEEP {
                lines.pop_front();
            }
            lines.push_back(line.to_string());
        }
        Ok(buf.len())
    }

    fn flush(&mut self) -> std::io::Result<()> {
        Ok(())
    }
}

fn init_logging() {
    static ONCE: OnceLock<()> = OnceLock::new();
    ONCE.get_or_init(|| {
        let filter = tracing_subscriber::EnvFilter::new(LOG_FILTER);
        // try_init: a host app that set up tracing itself keeps its own.
        let _ = tracing_subscriber::fmt()
            .with_env_filter(filter)
            .with_writer(|| LogSink)
            .with_ansi(false)
            .with_target(false)
            .try_init();
    });
}

/// Log lines since the last call, oldest first, one per line.
pub fn drain_logs() -> String {
    let mut lines = log_lines().lock().unwrap_or_else(|e| e.into_inner());
    let out: Vec<String> = lines.drain(..).collect();
    escape_non_ascii(&out.join("\n"))
}

// --- Encoding -----------------------------------------------------------------------------

/// JSON with every non-ASCII character written as \uXXXX, so the text is
/// the same in UTF-8 and in Java's "modified UTF-8" that JNI strings use.
fn encode(v: &Value) -> String {
    escape_non_ascii(&v.to_string())
}

/// Outside JSON strings only ASCII appears, so escaping every non-ASCII
/// character is valid wherever it is. Characters beyond U+FFFF become a
/// surrogate pair, as JSON requires. NUL can't pass through C strings and
/// becomes \u0000 too.
pub(crate) fn escape_non_ascii(s: &str) -> String {
    if s.bytes().all(|b| b.is_ascii() && b != 0) {
        return s.to_string();
    }
    let mut out = String::with_capacity(s.len() + 16);
    for ch in s.chars() {
        if ch.is_ascii() && ch != '\0' {
            out.push(ch);
            continue;
        }
        let mut units = [0u16; 2];
        for u in ch.encode_utf16(&mut units) {
            out.push_str(&format!("\\u{:04x}", u));
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn ascii_passes_through() {
        assert_eq!(escape_non_ascii(r#"{"a":"b"}"#), r#"{"a":"b"}"#);
    }

    #[test]
    fn non_ascii_is_escaped() {
        assert_eq!(escape_non_ascii("caf\u{e9}"), "caf\\u00e9");
        assert_eq!(escape_non_ascii("\u{1f600}"), "\\ud83d\\ude00");
        assert_eq!(escape_non_ascii("a\0b"), "a\\u0000b");
        let v: Value = serde_json::from_str(&encode(&json!({ "e": "Zo\u{eb} \u{1f600}" }))).unwrap();
        assert_eq!(v["e"], "Zo\u{eb} \u{1f600}");
    }

    #[test]
    fn config_defaults_and_custom_relays() {
        let c: StartConfig = serde_json::from_str(r#"{"server":"t","dataDir":"/d"}"#).unwrap();
        let c = c.into_client();
        assert_eq!(c.relay_mode, "default");
        assert!(c.discovery_n0 && c.mdns && !c.relay_only);
        assert_eq!(c.connect_timeout_ms, 10_000);

        let c: StartConfig =
            serde_json::from_str(r#"{"server":"t","dataDir":"/d","relayUrls":["https://iroh.relay.example.com:8443"]}"#)
                .unwrap();
        let c = c.into_client();
        assert_eq!(c.relay_mode, "custom");
        assert_eq!(c.relay_urls.len(), 1);
    }

    #[test]
    fn bad_input_is_an_error_not_a_panic() {
        let v: Value = serde_json::from_str(&start("not json")).unwrap();
        assert_eq!(v["ok"], false);
        let v: Value = serde_json::from_str(&start(r#"{"server":" ","dataDir":"/d"}"#)).unwrap();
        assert_eq!(v["ok"], false);
        let v: Value = serde_json::from_str(&status(999_999)).unwrap();
        assert_eq!(v["ok"], false);
        stop(999_999);
    }
}
