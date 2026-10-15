//! Stand-in for kinora-server's tunnel listener (8421). Parses the PROXY v2
//! header on every connection, then serves HTTP/1.1:
//!
//!   GET|HEAD /file             virtual file with Range support (seeded bytes)
//!   GET      /slow?secs=&kbps= long chunked response, like a piped transcode
//!   GET      /echo             JSON of the PROXY header fields
//!   POST     /upload           JSON with length and sha256 of the body
//!   GET      /health           {"ok":true}

use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use clap::Parser;
use futures_util::stream;
use http_body_util::combinators::UnsyncBoxBody;
use http_body_util::{BodyExt, Full, StreamBody};
use hyper::body::{Frame, Incoming};
use hyper::server::conn::http1;
use hyper::service::service_fn;
use hyper::{Method, Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use kinora_testserver::fill;
use kinora_tunnel::proxy_v2;
use serde_json::json;
use sha2::{Digest, Sha256};
use tokio::io::AsyncReadExt;
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;

const CHUNK: usize = 64 * 1024;

#[derive(Debug, Clone, Parser)]
#[command(name = "kinora-testserver", about = "PROXY v2 aware HTTP test server for the Kinora tunnel")]
struct Args {
    #[arg(long, default_value = "127.0.0.1:8421")]
    listen: SocketAddr,
    /// Size of the virtual /file in bytes.
    #[arg(long, default_value_t = 2 * 1024 * 1024 * 1024)]
    file_size: u64,
    #[arg(long, default_value_t = 7)]
    seed: u64,
    /// Accept plain connections without a PROXY header.
    #[arg(long)]
    no_proxy: bool,
    /// Log every PROXY header to stderr.
    #[arg(long)]
    verbose: bool,
}

struct Ctx {
    args: Args,
}

struct ConnInfo {
    header: Option<proxy_v2::Header>,
    tcp_peer: SocketAddr,
}

type Body = UnsyncBoxBody<Bytes, std::io::Error>;

#[tokio::main]
async fn main() -> std::io::Result<()> {
    let args = Args::parse();
    let listener = TcpListener::bind(args.listen).await?;
    eprintln!(
        "kinora-testserver listening on {} (file {} bytes, proxy {})",
        listener.local_addr()?,
        args.file_size,
        if args.no_proxy { "optional" } else { "required" }
    );
    let ctx = Arc::new(Ctx { args });
    loop {
        let (stream, tcp_peer) = listener.accept().await?;
        let ctx = ctx.clone();
        tokio::spawn(serve(stream, tcp_peer, ctx));
    }
}

async fn serve(mut stream: TcpStream, tcp_peer: SocketAddr, ctx: Arc<Ctx>) {
    let _ = stream.set_nodelay(true);
    let header = if ctx.args.no_proxy {
        None
    } else {
        match read_proxy(&mut stream).await {
            Ok(h) => Some(h),
            Err(e) => {
                eprintln!("rejecting {tcp_peer}: {e}");
                return;
            }
        }
    };
    if ctx.args.verbose {
        if let Some(h) = &header {
            eprintln!("conn from {tcp_peer}: node={:?} path={:?} source={:?}", h.node_id(), h.path(), h.source());
        }
    }
    let info = Arc::new(ConnInfo { header, tcp_peer });
    let svc = service_fn(move |req| {
        let ctx = ctx.clone();
        let info = info.clone();
        async move { Ok::<_, Infallible>(route(req, &ctx, &info).await) }
    });
    // half_close: a client may shut its write side after the request and
    // still expect the response, which is exactly what the tunnel must carry.
    if let Err(e) =
        http1::Builder::new().half_close(true).keep_alive(true).serve_connection(TokioIo::new(stream), svc).await
    {
        if worth_logging(&e) {
            eprintln!("connection from {tcp_peer} ended: {e}");
        }
    }
}

fn worth_logging(e: &hyper::Error) -> bool {
    // Clients dropping a download midway is normal in these tests.
    !(e.is_incomplete_message() || e.is_canceled())
}

async fn read_proxy(stream: &mut TcpStream) -> Result<proxy_v2::Header, String> {
    let read = async {
        let mut fixed = [0u8; proxy_v2::FIXED_LEN];
        stream.read_exact(&mut fixed).await.map_err(|e| e.to_string())?;
        let total = proxy_v2::header_len(&fixed).map_err(|e| e.to_string())?;
        let mut buf = fixed.to_vec();
        buf.resize(total, 0);
        stream.read_exact(&mut buf[proxy_v2::FIXED_LEN..]).await.map_err(|e| e.to_string())?;
        proxy_v2::parse(&buf).map(|(h, _)| h).map_err(|e| e.to_string())
    };
    tokio::time::timeout(Duration::from_secs(5), read).await.map_err(|_| "PROXY header timeout".to_string())?
}

async fn route(req: Request<Incoming>, ctx: &Ctx, info: &ConnInfo) -> Response<Body> {
    let path = req.uri().path().to_string();
    match (req.method(), path.as_str()) {
        (&Method::GET, "/file") | (&Method::HEAD, "/file") => file(&req, ctx),
        (&Method::GET, "/slow") => slow(req.uri().query().unwrap_or(""), ctx),
        (&Method::GET, "/echo") => echo(info),
        (&Method::POST, "/upload") => upload(req).await,
        (&Method::GET, "/health") => json_response(StatusCode::OK, json!({ "ok": true })),
        _ => json_response(StatusCode::NOT_FOUND, json!({ "error": "not found" })),
    }
}

fn full(bytes: impl Into<Bytes>) -> Body {
    Full::new(bytes.into()).map_err(|never| match never {}).boxed_unsync()
}

fn json_response(status: StatusCode, value: serde_json::Value) -> Response<Body> {
    Response::builder()
        .status(status)
        .header("content-type", "application/json")
        .body(full(value.to_string()))
        .expect("valid response")
}

/// Body fed by a bounded channel. The producer waits whenever hyper is not
/// writing, so a slow reader caps memory at a couple of chunks.
fn channel_body(rx: mpsc::Receiver<Bytes>) -> Body {
    let s =
        stream::unfold(
            rx,
            |mut rx| async move { rx.recv().await.map(|b| (Ok::<_, std::io::Error>(Frame::data(b)), rx)) },
        );
    StreamBody::new(s).boxed_unsync()
}

enum Range {
    Full,
    Part(u64, u64),
    Unsatisfiable,
}

fn parse_range(header: Option<&str>, size: u64) -> Range {
    let Some(spec) = header.and_then(|h| h.trim().strip_prefix("bytes=")) else {
        return Range::Full;
    };
    if spec.contains(',') || size == 0 {
        return Range::Full;
    }
    let Some((a, b)) = spec.split_once('-') else {
        return Range::Full;
    };
    let (a, b) = (a.trim(), b.trim());
    if a.is_empty() {
        return match b.parse::<u64>() {
            Ok(0) => Range::Unsatisfiable,
            Ok(n) => Range::Part(size - n.min(size), size - 1),
            Err(_) => Range::Full,
        };
    }
    let Ok(start) = a.parse::<u64>() else {
        return Range::Full;
    };
    if start >= size {
        return Range::Unsatisfiable;
    }
    let end = if b.is_empty() {
        size - 1
    } else {
        match b.parse::<u64>() {
            Ok(e) if e >= start => e.min(size - 1),
            _ => return Range::Full,
        }
    };
    Range::Part(start, end)
}

fn file(req: &Request<Incoming>, ctx: &Ctx) -> Response<Body> {
    let size = ctx.args.file_size;
    let range = parse_range(req.headers().get("range").and_then(|v| v.to_str().ok()), size);
    let (status, start, len) = match range {
        Range::Full => (StatusCode::OK, 0, size),
        Range::Part(s, e) => (StatusCode::PARTIAL_CONTENT, s, e - s + 1),
        Range::Unsatisfiable => {
            return Response::builder()
                .status(StatusCode::RANGE_NOT_SATISFIABLE)
                .header("content-range", format!("bytes */{size}"))
                .body(full(Bytes::new()))
                .expect("valid response");
        }
    };
    let mut builder = Response::builder()
        .status(status)
        .header("accept-ranges", "bytes")
        .header("content-type", "application/octet-stream")
        .header("content-length", len.to_string());
    if status == StatusCode::PARTIAL_CONTENT {
        builder = builder.header("content-range", format!("bytes {}-{}/{}", start, start + len - 1, size));
    }
    if req.method() == Method::HEAD {
        return builder.body(full(Bytes::new())).expect("valid response");
    }
    let seed = ctx.args.seed;
    let (tx, rx) = mpsc::channel::<Bytes>(2);
    tokio::spawn(async move {
        let end = start + len;
        let mut pos = start;
        while pos < end {
            let n = ((end - pos) as usize).min(CHUNK);
            let mut buf = vec![0u8; n];
            fill(seed, pos, &mut buf);
            if tx.send(Bytes::from(buf)).await.is_err() {
                return;
            }
            pos += n as u64;
        }
    });
    builder.body(channel_body(rx)).expect("valid response")
}

fn query_u64(query: &str, key: &str, default: u64) -> u64 {
    query
        .split('&')
        .filter_map(|kv| kv.split_once('='))
        .find(|(k, _)| *k == key)
        .and_then(|(_, v)| v.parse().ok())
        .unwrap_or(default)
}

/// Streams for `secs` seconds at about `kbps` KiB/s with no content length,
/// so hyper uses chunked encoding like ffmpeg piping a transcode.
fn slow(query: &str, ctx: &Ctx) -> Response<Body> {
    let secs = query_u64(query, "secs", 180);
    let kbps = query_u64(query, "kbps", 256).max(1);
    let per_tick = ((kbps * 1024) / 10).max(1) as usize;
    let seed = ctx.args.seed;
    let (tx, rx) = mpsc::channel::<Bytes>(2);
    tokio::spawn(async move {
        let mut pos = 0u64;
        let mut tick = tokio::time::interval(Duration::from_millis(100));
        for _ in 0..secs * 10 {
            tick.tick().await;
            let mut buf = vec![0u8; per_tick];
            fill(seed, pos, &mut buf);
            if tx.send(Bytes::from(buf)).await.is_err() {
                return;
            }
            pos += per_tick as u64;
        }
    });
    Response::builder()
        .status(StatusCode::OK)
        .header("content-type", "application/octet-stream")
        .body(channel_body(rx))
        .expect("valid response")
}

fn echo(info: &ConnInfo) -> Response<Body> {
    let h = info.header.as_ref();
    json_response(
        StatusCode::OK,
        json!({
            "proxy": h.is_some(),
            "nodeId": h.and_then(|h| h.node_id()),
            "path": h.and_then(|h| h.path()),
            "source": h.and_then(|h| h.source()).map(|a| a.to_string()),
            "tcpPeer": info.tcp_peer.to_string(),
        }),
    )
}

async fn upload(req: Request<Incoming>) -> Response<Body> {
    match req.into_body().collect().await {
        Ok(collected) => {
            let bytes = collected.to_bytes();
            let digest = Sha256::digest(&bytes);
            json_response(StatusCode::OK, json!({ "length": bytes.len(), "sha256": format!("{digest:x}") }))
        }
        Err(e) => json_response(StatusCode::BAD_REQUEST, json!({ "error": e.to_string() })),
    }
}
