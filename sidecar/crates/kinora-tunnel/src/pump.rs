//! Raw byte piping between one local TCP connection and one QUIC
//! bidirectional stream. No HTTP parsing happens here.

use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;

use iroh::endpoint::{RecvStream, SendStream, VarInt};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;

/// 64 KiB per direction keeps memory per stream small while still moving
/// 100+ Mbps; flow control windows, not this buffer, set the throughput.
const BUF_SIZE: usize = 64 * 1024;

/// Stream reset and stop code used when a pump fails partway.
pub const STREAM_ABORTED: u32 = 1;

#[derive(Debug, Default)]
pub struct Counters {
    /// Bytes read from the QUIC stream (received from the other end).
    pub bytes_in: AtomicU64,
    /// Bytes written to the QUIC stream (sent to the other end).
    pub bytes_out: AtomicU64,
    pub open_streams: AtomicU64,
    pub total_streams: AtomicU64,
}

/// Tracks one open stream in the per-peer and global counters, and undoes it
/// on drop so early returns cannot leak a count.
pub struct StreamGuard {
    counters: Arc<Counters>,
    global: Arc<AtomicU64>,
}

impl StreamGuard {
    pub fn new(counters: Arc<Counters>, global: Arc<AtomicU64>) -> Self {
        counters.open_streams.fetch_add(1, Ordering::Relaxed);
        counters.total_streams.fetch_add(1, Ordering::Relaxed);
        global.fetch_add(1, Ordering::Relaxed);
        Self { counters, global }
    }
}

impl Drop for StreamGuard {
    fn drop(&mut self) {
        self.counters.open_streams.fetch_sub(1, Ordering::Relaxed);
        self.global.fetch_sub(1, Ordering::Relaxed);
    }
}

#[derive(Debug)]
pub enum PumpError {
    Tcp(std::io::Error),
    Quic(String),
}

impl std::fmt::Display for PumpError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PumpError::Tcp(e) => write!(f, "tcp: {e}"),
            PumpError::Quic(e) => write!(f, "quic: {e}"),
        }
    }
}

/// Copies both directions until each side has finished. Half-close is kept:
/// EOF on TCP becomes `finish()` on the QUIC send side, and the end of the
/// QUIC receive side becomes a TCP write shutdown, while the other direction
/// keeps flowing. Every write waits for flow control credit, so a slow reader
/// at either end slows the writer at the other end.
///
/// On error the caller should call `abort` so the peer sees a reset rather
/// than a clean end of stream.
pub async fn pump(
    tcp: TcpStream,
    send: &mut SendStream,
    recv: &mut RecvStream,
    counters: &Counters,
) -> Result<(), PumpError> {
    let (tcp_read, tcp_write) = tcp.into_split();
    tokio::try_join!(tcp_to_quic(tcp_read, send, counters), quic_to_tcp(recv, tcp_write, counters)).map(|_| ())
}

async fn tcp_to_quic(
    mut tcp: tokio::net::tcp::OwnedReadHalf,
    send: &mut SendStream,
    counters: &Counters,
) -> Result<(), PumpError> {
    let mut buf = vec![0u8; BUF_SIZE];
    loop {
        let n = tcp.read(&mut buf).await.map_err(PumpError::Tcp)?;
        if n == 0 {
            // Finishing an already reset stream is harmless, so ignore it.
            let _ = send.finish();
            return Ok(());
        }
        // Called by path so tokio's AsyncWriteExt::write_all is never picked.
        SendStream::write_all(send, &buf[..n]).await.map_err(|e| PumpError::Quic(e.to_string()))?;
        counters.bytes_out.fetch_add(n as u64, Ordering::Relaxed);
    }
}

async fn quic_to_tcp(
    recv: &mut RecvStream,
    mut tcp: tokio::net::tcp::OwnedWriteHalf,
    counters: &Counters,
) -> Result<(), PumpError> {
    let mut buf = vec![0u8; BUF_SIZE];
    loop {
        match RecvStream::read(recv, &mut buf).await.map_err(|e| PumpError::Quic(e.to_string()))? {
            None => {
                let _ = tcp.shutdown().await;
                return Ok(());
            }
            Some(n) => {
                tcp.write_all(&buf[..n]).await.map_err(PumpError::Tcp)?;
                counters.bytes_in.fetch_add(n as u64, Ordering::Relaxed);
            }
        }
    }
}

/// Tears a stream down after a failed pump so the other end stops promptly.
pub fn abort(send: &mut SendStream, recv: &mut RecvStream) {
    let _ = send.reset(VarInt::from_u32(STREAM_ABORTED));
    let _ = recv.stop(VarInt::from_u32(STREAM_ABORTED));
}

/// A minimal HTTP 502 used when the tunnel cannot carry a request at all.
/// Players and fetch handle a 502 far better than a silent hang or reset.
pub fn bad_gateway(message: &str) -> Vec<u8> {
    let body = serde_json::json!({ "error": "tunnel_unavailable", "message": message }).to_string();
    format!(
        "HTTP/1.1 502 Bad Gateway\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\nX-Kinora-Tunnel: error\r\n\r\n{}",
        body.len(),
        body
    )
    .into_bytes()
}
