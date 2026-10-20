//! End-to-end runner. Starts kinara-testserver, kinara-iroh and
//! kinara-connect from the same target directory and checks the tunnel the
//! way the apps will use it. Plain threads and blocking sockets on purpose:
//! the client side here should look like ExoPlayer, not like tokio.
//!
//!   cargo build --release --workspace
//!   target/release/kinara-e2e            (add --help for options)

use std::collections::BTreeMap;
use std::fs;
use std::io::{self, BufRead, BufReader, Read, Write};
use std::net::{Shutdown, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::mpsc::{self, Receiver};
use std::sync::{Arc, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use clap::Parser;
use kinara_testserver::fill;
use kinara_tunnel::events::Event;
use serde_json::Value;
use sha2::{Digest, Sha256};

const SEED: u64 = 7;
const MIB: u64 = 1024 * 1024;

#[derive(Debug, Parser)]
#[command(name = "kinara-e2e", about = "End-to-end tests for the Kinara iroh tunnel")]
struct Args {
    /// Folder holding kinara-iroh, kinara-connect and kinara-testserver.
    /// Default: the folder this binary is in.
    #[arg(long)]
    bin_dir: Option<PathBuf>,
    /// Size of the virtual test file.
    #[arg(long, default_value_t = 1024 * MIB)]
    file_size: u64,
    #[arg(long, default_value_t = 18421)]
    target_port: u16,
    /// Fixed UDP port for the sidecar. Default: let iroh choose.
    #[arg(long)]
    udp_port: Option<u16>,
    /// Seconds of slow reading in the backpressure test.
    #[arg(long, default_value_t = 20)]
    backpressure_secs: u64,
    /// Rate of the slow reader in KiB/s.
    #[arg(long, default_value_t = 2048)]
    slow_kbps: u64,
    /// Run the long chunked (piped transcode) test for this many seconds. 0 skips it.
    #[arg(long, default_value_t = 0)]
    long_secs: u64,
    /// MiB to move through the relay for the throughput figure.
    #[arg(long, default_value_t = 64)]
    relay_mib: u64,
    /// Skip the relay-only client (no internet access, for example).
    #[arg(long)]
    skip_relay: bool,
    /// Keep the temporary folder with logs and keys.
    #[arg(long)]
    keep_temp: bool,
}

#[derive(Debug, Clone)]
struct Outcome {
    name: String,
    pass: Option<bool>,
    detail: String,
}

struct Report {
    results: Vec<Outcome>,
}

impl Report {
    fn add(&mut self, name: &str, pass: Option<bool>, detail: impl Into<String>) {
        let detail = detail.into();
        let mark = match pass {
            Some(true) => "PASS",
            Some(false) => "FAIL",
            None => "SKIP",
        };
        println!("[{mark}] {name}: {detail}");
        self.results.push(Outcome { name: name.into(), pass, detail });
    }

    fn failed(&self) -> usize {
        self.results.iter().filter(|r| r.pass == Some(false)).count()
    }
}

// ---------------------------------------------------------------- processes

struct Proc {
    name: String,
    child: Child,
    stdin: Option<ChildStdin>,
    lines: Arc<Mutex<Vec<String>>>,
    rx: Receiver<String>,
}

impl Proc {
    fn spawn(name: &str, bin: &Path, args: &[String], log: &Path) -> io::Result<Proc> {
        let log_file = fs::File::create(log)?;
        let mut child = Command::new(bin)
            .args(args)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::from(log_file))
            .spawn()
            .map_err(|e| io::Error::new(e.kind(), format!("starting {}: {e}", bin.display())))?;
        let stdout = child.stdout.take().expect("piped stdout");
        let stdin = child.stdin.take();
        let lines = Arc::new(Mutex::new(Vec::new()));
        let (tx, rx) = mpsc::channel();
        let sink = lines.clone();
        thread::spawn(move || {
            for line in BufReader::new(stdout).lines() {
                let Ok(line) = line else { break };
                sink.lock().unwrap().push(line.clone());
                if tx.send(line).is_err() {
                    // Receiver gone; keep recording for the status line check.
                }
            }
        });
        Ok(Proc { name: name.into(), child, stdin, lines, rx })
    }

    fn pid(&self) -> u32 {
        self.child.id()
    }

    /// Waits for a stdout JSON line matching `pred`, skipping others.
    fn wait_json(&self, timeout: Duration, pred: impl Fn(&Value) -> bool) -> Option<Value> {
        let deadline = Instant::now() + timeout;
        loop {
            let left = deadline.checked_duration_since(Instant::now())?;
            match self.rx.recv_timeout(left) {
                Ok(line) => {
                    if let Ok(v) = serde_json::from_str::<Value>(&line) {
                        if pred(&v) {
                            return Some(v);
                        }
                    }
                }
                Err(_) => return None,
            }
        }
    }

    fn send(&mut self, line: &str) {
        if let Some(stdin) = self.stdin.as_mut() {
            let _ = writeln!(stdin, "{line}");
            let _ = stdin.flush();
        }
    }

    fn close_stdin(&mut self) {
        self.stdin.take();
    }

    /// Returns how long the process took to exit, or None on timeout.
    fn wait_exit(&mut self, timeout: Duration) -> Option<Duration> {
        let start = Instant::now();
        while start.elapsed() < timeout {
            if let Ok(Some(_)) = self.child.try_wait() {
                return Some(start.elapsed());
            }
            thread::sleep(Duration::from_millis(20));
        }
        None
    }

    fn stop(&mut self) {
        self.close_stdin();
        if self.wait_exit(Duration::from_secs(3)).is_none() {
            eprintln!("{} did not exit, killing it", self.name);
            let _ = self.child.kill();
            let _ = self.child.wait();
        }
    }

    fn all_lines(&self) -> Vec<String> {
        self.lines.lock().unwrap().clone()
    }
}

impl Drop for Proc {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

// ---------------------------------------------------------------- memory

/// Resident set size in KiB, from the OS tools available on each platform.
#[allow(unreachable_code)]
fn rss_kib(pid: u32) -> Option<u64> {
    #[cfg(target_os = "linux")]
    {
        let status = fs::read_to_string(format!("/proc/{pid}/status")).ok()?;
        let line = status.lines().find(|l| l.starts_with("VmRSS:"))?;
        return line.split_whitespace().nth(1)?.parse().ok();
    }
    #[cfg(target_os = "macos")]
    {
        let out = Command::new("ps").args(["-o", "rss=", "-p", &pid.to_string()]).output().ok()?;
        return String::from_utf8_lossy(&out.stdout).trim().parse().ok();
    }
    #[cfg(windows)]
    {
        // "kinara-iroh.exe","1234","Console","1","12,345 K"
        let out =
            Command::new("tasklist").args(["/FI", &format!("PID eq {pid}"), "/FO", "CSV", "/NH"]).output().ok()?;
        let text = String::from_utf8_lossy(&out.stdout);
        let field = text.trim().rsplit("\",\"").next()?;
        let digits: String = field.chars().filter(|c| c.is_ascii_digit()).collect();
        return digits.parse().ok();
    }
    None
}

// ---------------------------------------------------------------- http

struct HttpResponse {
    status: u16,
    headers: BTreeMap<String, String>,
    reader: BufReader<TcpStream>,
}

impl HttpResponse {
    fn header(&self, name: &str) -> Option<&str> {
        self.headers.get(&name.to_ascii_lowercase()).map(|s| s.as_str())
    }

    /// Streams the body into `sink`. Handles Content-Length, chunked, and
    /// read-to-close.
    fn body(&mut self, mut sink: impl FnMut(&[u8])) -> io::Result<u64> {
        let chunked = self.header("transfer-encoding").is_some_and(|v| v.eq_ignore_ascii_case("chunked"));
        let content_length = self.header("content-length").and_then(|v| v.parse::<u64>().ok());
        let mut buf = vec![0u8; 64 * 1024];
        let mut total = 0u64;
        if chunked {
            loop {
                let mut size_line = String::new();
                self.reader.read_line(&mut size_line)?;
                let size_hex = size_line.trim().split(';').next().unwrap_or("");
                let size = u64::from_str_radix(size_hex, 16)
                    .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, format!("bad chunk size {size_line:?}")))?;
                if size == 0 {
                    loop {
                        let mut trailer = String::new();
                        if self.reader.read_line(&mut trailer)? == 0 || trailer.trim().is_empty() {
                            return Ok(total);
                        }
                    }
                }
                let mut left = size;
                while left > 0 {
                    let n = (left as usize).min(buf.len());
                    self.reader.read_exact(&mut buf[..n])?;
                    sink(&buf[..n]);
                    left -= n as u64;
                    total += n as u64;
                }
                let mut crlf = [0u8; 2];
                self.reader.read_exact(&mut crlf)?;
            }
        }
        if let Some(len) = content_length {
            let mut left = len;
            while left > 0 {
                let n = (left as usize).min(buf.len());
                let got = self.reader.read(&mut buf[..n])?;
                if got == 0 {
                    return Err(io::Error::new(
                        io::ErrorKind::UnexpectedEof,
                        format!("body ended at {total} of {len}"),
                    ));
                }
                sink(&buf[..got]);
                left -= got as u64;
                total += got as u64;
            }
            return Ok(total);
        }
        loop {
            let got = self.reader.read(&mut buf)?;
            if got == 0 {
                return Ok(total);
            }
            sink(&buf[..got]);
            total += got as u64;
        }
    }

    fn json(&mut self) -> io::Result<Value> {
        let mut data = Vec::new();
        self.body(|b| data.extend_from_slice(b))?;
        serde_json::from_slice(&data).map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))
    }
}

struct RequestSpec<'a> {
    method: &'a str,
    path: &'a str,
    headers: Vec<(&'a str, String)>,
    body: Option<&'a [u8]>,
    /// Shut the write side after the request, like a client that half-closes.
    half_close: bool,
}

impl<'a> RequestSpec<'a> {
    fn get(path: &'a str) -> Self {
        RequestSpec { method: "GET", path, headers: Vec::new(), body: None, half_close: false }
    }
}

fn request(port: u16, spec: &RequestSpec) -> io::Result<HttpResponse> {
    let stream = TcpStream::connect(("127.0.0.1", port))?;
    stream.set_read_timeout(Some(Duration::from_secs(60)))?;
    stream.set_nodelay(true)?;
    let mut head = format!("{} {} HTTP/1.1\r\nHost: 127.0.0.1:{port}\r\nConnection: close\r\n", spec.method, spec.path);
    for (k, v) in &spec.headers {
        head.push_str(&format!("{k}: {v}\r\n"));
    }
    if let Some(body) = spec.body {
        head.push_str(&format!("Content-Length: {}\r\n", body.len()));
    }
    head.push_str("\r\n");
    let mut w = stream.try_clone()?;
    w.write_all(head.as_bytes())?;
    if let Some(body) = spec.body {
        w.write_all(body)?;
    }
    w.flush()?;
    if spec.half_close {
        stream.shutdown(Shutdown::Write)?;
    }
    let mut reader = BufReader::new(stream);
    let mut status_line = String::new();
    if reader.read_line(&mut status_line)? == 0 {
        return Err(io::Error::new(io::ErrorKind::UnexpectedEof, "connection closed before a response"));
    }
    let status = status_line
        .split_whitespace()
        .nth(1)
        .and_then(|s| s.parse().ok())
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, format!("bad status line {status_line:?}")))?;
    let mut headers = BTreeMap::new();
    loop {
        let mut line = String::new();
        if reader.read_line(&mut line)? == 0 {
            break;
        }
        let line = line.trim_end();
        if line.is_empty() {
            break;
        }
        if let Some((k, v)) = line.split_once(':') {
            headers.insert(k.trim().to_ascii_lowercase(), v.trim().to_string());
        }
    }
    Ok(HttpResponse { status, headers, reader })
}

fn expected_sha(start: u64, len: u64) -> String {
    let mut hasher = Sha256::new();
    let mut buf = vec![0u8; 256 * 1024];
    let mut pos = start;
    let end = start + len;
    while pos < end {
        let n = ((end - pos) as usize).min(buf.len());
        fill(SEED, pos, &mut buf[..n]);
        hasher.update(&buf[..n]);
        pos += n as u64;
    }
    format!("{:x}", hasher.finalize())
}

/// Fetches bytes [start, start+len) and checks status, Content-Range and checksum.
fn check_range(port: u16, start: u64, len: u64, size: u64) -> Result<(), String> {
    let mut spec = RequestSpec::get("/file");
    spec.headers.push(("Range", format!("bytes={}-{}", start, start + len - 1)));
    let mut resp = request(port, &spec).map_err(|e| format!("request: {e}"))?;
    if resp.status != 206 {
        return Err(format!("status {} for range {start}+{len}", resp.status));
    }
    let want_range = format!("bytes {}-{}/{}", start, start + len - 1, size);
    if resp.header("content-range") != Some(want_range.as_str()) {
        return Err(format!("content-range {:?}, want {want_range:?}", resp.header("content-range")));
    }
    let mut hasher = Sha256::new();
    let got = resp.body(|b| hasher.update(b)).map_err(|e| format!("body: {e}"))?;
    if got != len {
        return Err(format!("got {got} bytes, want {len}"));
    }
    let digest = format!("{:x}", hasher.finalize());
    if digest != expected_sha(start, len) {
        return Err(format!("checksum mismatch for range {start}+{len}"));
    }
    Ok(())
}

/// Small deterministic generator for range positions.
struct XorShift(u64);

impl XorShift {
    fn next(&mut self) -> u64 {
        let mut x = self.0;
        x ^= x << 13;
        x ^= x >> 7;
        x ^= x << 17;
        self.0 = x;
        x
    }

    fn below(&mut self, n: u64) -> u64 {
        if n == 0 {
            0
        } else {
            self.next() % n
        }
    }
}

fn echo(port: u16) -> io::Result<Value> {
    request(port, &RequestSpec::get("/echo"))?.json()
}

/// Retries `f` until it returns Some or `limit` passes.
fn retry<T>(limit: Duration, every: Duration, mut f: impl FnMut() -> Option<T>) -> Option<T> {
    let start = Instant::now();
    loop {
        if let Some(v) = f() {
            return Some(v);
        }
        if start.elapsed() >= limit {
            return None;
        }
        thread::sleep(every);
    }
}

fn mbps(bytes: u64, elapsed: Duration) -> f64 {
    bytes as f64 * 8.0 / 1_000_000.0 / elapsed.as_secs_f64().max(1e-6)
}

// ---------------------------------------------------------------- setup

struct Bins {
    iroh: PathBuf,
    connect: PathBuf,
    testserver: PathBuf,
}

fn find_bins(args: &Args) -> io::Result<Bins> {
    let dir = match &args.bin_dir {
        Some(d) => d.clone(),
        None => std::env::current_exe()?.parent().map(Path::to_path_buf).unwrap_or_default(),
    };
    let bin = |name: &str| dir.join(format!("{name}{}", std::env::consts::EXE_SUFFIX));
    let bins = Bins { iroh: bin("kinara-iroh"), connect: bin("kinara-connect"), testserver: bin("kinara-testserver") };
    for b in [&bins.iroh, &bins.connect, &bins.testserver] {
        if !b.exists() {
            return Err(io::Error::new(
                io::ErrorKind::NotFound,
                format!("{} not found; run cargo build --release --workspace first", b.display()),
            ));
        }
    }
    Ok(bins)
}

fn sidecar_args(args: &Args, data: &Path) -> Vec<String> {
    let mut v = vec![
        "--data-dir".to_string(),
        data.display().to_string(),
        "--target".into(),
        format!("127.0.0.1:{}", args.target_port),
        "--status-interval-secs".into(),
        "2".into(),
        "--log-level".into(),
        "info".into(),
    ];
    if let Some(p) = args.udp_port {
        v.push("--udp-port".into());
        v.push(p.to_string());
    }
    v
}

fn start_sidecar(bins: &Bins, args: &Args, data: &Path, log: &Path) -> Result<(Proc, String, String), String> {
    let p = Proc::spawn("kinara-iroh", &bins.iroh, &sidecar_args(args, data), log).map_err(|e| e.to_string())?;
    let ready = p
        .wait_json(Duration::from_secs(30), |v| v["type"] == "ready")
        .ok_or_else(|| format!("kinara-iroh sent no ready line within 30s, see {}", log.display()))?;
    let node = ready["nodeId"].as_str().unwrap_or_default().to_string();
    let ticket = ready["ticket"].as_str().unwrap_or_default().to_string();
    Ok((p, node, ticket))
}

fn start_client(
    bins: &Bins,
    ticket: &str,
    data: &Path,
    log: &Path,
    relay_only: bool,
) -> Result<(Proc, u16, String), String> {
    let mut a =
        vec![ticket.to_string(), "--data-dir".into(), data.display().to_string(), "--exit-on-stdin-close".into()];
    if relay_only {
        a.push("--relay-only".into());
    }
    let p = Proc::spawn("kinara-connect", &bins.connect, &a, log).map_err(|e| e.to_string())?;
    let listening = p
        .wait_json(Duration::from_secs(20), |v| v["type"] == "listening")
        .ok_or_else(|| format!("kinara-connect did not start, see {}", log.display()))?;
    let port = listening["port"].as_u64().unwrap_or(0) as u16;
    let node = listening["nodeId"].as_str().unwrap_or_default().to_string();
    Ok((p, port, node))
}

// ---------------------------------------------------------------- main

fn main() {
    let args = Args::parse();
    let code = match run(&args) {
        Ok(failed) if failed == 0 => 0,
        Ok(failed) => {
            println!("\n{failed} test(s) failed");
            1
        }
        Err(e) => {
            eprintln!("setup failed: {e}");
            2
        }
    };
    std::process::exit(code);
}

fn run(args: &Args) -> Result<usize, String> {
    let bins = find_bins(args).map_err(|e| e.to_string())?;
    let tmp = std::env::temp_dir().join(format!("kinara-e2e-{}", std::process::id()));
    fs::create_dir_all(&tmp).map_err(|e| e.to_string())?;
    println!("temp folder: {}", tmp.display());
    let srv_data = tmp.join("server");
    let size = args.file_size;
    let mut report = Report { results: Vec::new() };

    // Stand-in Go listener.
    let _testserver = Proc::spawn(
        "kinara-testserver",
        &bins.testserver,
        &[
            "--listen".into(),
            format!("127.0.0.1:{}", args.target_port),
            "--file-size".into(),
            size.to_string(),
            "--seed".into(),
            SEED.to_string(),
        ],
        &tmp.join("testserver.log"),
    )
    .map_err(|e| e.to_string())?;
    retry(Duration::from_secs(10), Duration::from_millis(100), || {
        TcpStream::connect(("127.0.0.1", args.target_port)).ok()
    })
    .ok_or("kinara-testserver did not start listening")?;

    let (mut sidecar, node_id, ticket) = start_sidecar(&bins, args, &srv_data, &tmp.join("kinara-iroh-1.log"))?;
    println!("server node {node_id}");
    let (mut client_a, port_a, node_a) =
        start_client(&bins, &ticket, &tmp.join("client-a"), &tmp.join("kinara-connect-a.log"), false)?;
    println!("client A node {node_a} on port {port_a}");

    // 7 and 6: identity through PROXY, and a direct path.
    let first = retry(Duration::from_secs(30), Duration::from_millis(500), || echo(port_a).ok());
    match &first {
        Some(v) if v["nodeId"] == node_a.as_str() => {
            report.add("7 peer identity", Some(true), format!("echo nodeId matches client A, path {}", v["path"]))
        }
        Some(v) => report.add("7 peer identity", Some(false), format!("echo returned {v}")),
        None => report.add("7 peer identity", Some(false), "no response through the tunnel within 30s"),
    }
    let mut seen_paths = Vec::new();
    let direct = retry(Duration::from_secs(20), Duration::from_millis(500), || {
        let v = echo(port_a).ok()?;
        let path = v["path"].as_str().unwrap_or("").to_string();
        if !seen_paths.contains(&path) {
            seen_paths.push(path.clone());
        }
        path.starts_with("direct").then_some((path, v["source"].clone()))
    });
    let has_global_v6 = sidecar
        .all_lines()
        .iter()
        .rev()
        .find_map(|l| serde_json::from_str::<Value>(l).ok().filter(|v| v["type"] == "status"))
        .and_then(|v| v["addrs"].as_array().cloned())
        .map(|addrs| addrs.iter().any(|a| a["family"] == "ipv6" && a["scope"] == "global"))
        .unwrap_or(false);
    match direct {
        Some((path, source)) => report.add(
            "6 direct path",
            Some(true),
            format!(
                "{path} from {source}; paths seen {seen_paths:?}; machine has global IPv6: {has_global_v6} \
                 (loopback and LAN addresses only prove the mechanics, not real IPv6 hole punching)"
            ),
        ),
        None => {
            report.add("6 direct path", Some(false), format!("never went direct in 20s, paths seen {seen_paths:?}"))
        }
    }

    // 1: range integrity, sequential edges then 32 in parallel.
    let mut rng = XorShift(0x9E37_79B9_7F4A_7C15);
    let mut ranges: Vec<(u64, u64)> = vec![(0, 1), (size - 1, 1), (size - 1000, 1000), (12_345, 3 * MIB + 17)];
    let mut problems = Vec::new();
    for &(s, l) in &ranges {
        if let Err(e) = check_range(port_a, s, l, size) {
            problems.push(e);
        }
    }
    ranges.clear();
    for _ in 0..32 {
        let len = 1 + rng.below(8 * MIB);
        let start = rng.below(size - len);
        ranges.push((start, len));
    }
    let started = Instant::now();
    let handles: Vec<_> = ranges.iter().map(|&(s, l)| thread::spawn(move || check_range(port_a, s, l, size))).collect();
    for h in handles {
        match h.join() {
            Ok(Ok(())) => {}
            Ok(Err(e)) => problems.push(e),
            Err(_) => problems.push("range thread panicked".into()),
        }
    }
    let total: u64 = ranges.iter().map(|r| r.1).sum();
    if problems.is_empty() {
        report.add(
            "1 range integrity",
            Some(true),
            format!(
                "4 edge ranges + 32 parallel ranges ({} MiB) verified by sha256, parallel batch at {:.0} Mbps",
                total / MIB,
                mbps(total, started.elapsed())
            ),
        );
    } else {
        report.add("1 range integrity", Some(false), problems.join("; "));
    }

    // 3: half-close. Send a body, shut the write side, expect the response.
    let mut body = vec![0u8; 4 * MIB as usize];
    fill(SEED, 99, &mut body);
    let want_sha = format!("{:x}", Sha256::digest(&body));
    let mut spec = RequestSpec {
        method: "POST",
        path: "/upload",
        headers: Vec::new(),
        body: Some(body.as_slice()),
        half_close: true,
    };
    spec.headers.push(("Content-Type", "application/octet-stream".into()));
    match request(port_a, &spec).and_then(|mut r| r.json()) {
        Ok(v) if v["sha256"] == want_sha.as_str() && v["length"] == body.len() as u64 => {
            report.add("3 half-close", Some(true), "4 MiB upload, write side shut down, response arrived intact")
        }
        Ok(v) => report.add("3 half-close", Some(false), format!("unexpected response {v}")),
        Err(e) => report.add("3 half-close", Some(false), format!("{e}")),
    }

    // 11 (direct): one long download over loopback.
    let dl = size.min(1024 * MIB);
    let started = Instant::now();
    let mut spec = RequestSpec::get("/file");
    spec.headers.push(("Range", format!("bytes=0-{}", dl - 1)));
    match request(port_a, &spec).and_then(|mut r| r.body(|_| {})) {
        Ok(n) if n == dl => {
            let t = started.elapsed();
            report.add(
                "11 throughput (direct, loopback)",
                Some(true),
                format!("{} MiB in {:.1}s = {:.0} Mbps", dl / MIB, t.as_secs_f64(), mbps(dl, t)),
            );
        }
        Ok(n) => report.add("11 throughput (direct, loopback)", Some(false), format!("got {n} of {dl} bytes")),
        Err(e) => report.add("11 throughput (direct, loopback)", Some(false), e.to_string()),
    }

    // 2: backpressure. A slow reader must not make either side buffer the file.
    report_backpressure(&mut report, args, port_a, &sidecar, &client_a, &_testserver);

    // Optional: long chunked stream, the piped transcode shape.
    if args.long_secs > 0 {
        report_long_chunked(&mut report, args.long_secs, port_a);
    } else {
        report.add("long chunked stream", None, "skipped, pass --long-secs 180 to run it");
    }

    // 5 and 11 (relay): a second client forced onto the relay.
    let mut client_b = None;
    if args.skip_relay {
        report.add("5 relay-only", None, "skipped by --skip-relay");
    } else {
        match start_client(&bins, &ticket, &tmp.join("client-b"), &tmp.join("kinara-connect-b.log"), true) {
            Ok((proc_b, port_b, node_b)) => {
                let v = retry(Duration::from_secs(45), Duration::from_secs(1), || echo(port_b).ok());
                match v {
                    Some(v) if v["path"] == "relay" && v["nodeId"] == node_b.as_str() => {
                        let want = args.relay_mib * MIB;
                        let started = Instant::now();
                        let mut spec = RequestSpec::get("/file");
                        spec.headers.push(("Range", format!("bytes=0-{}", want - 1)));
                        match request(port_b, &spec).and_then(|mut r| r.body(|_| {})) {
                            Ok(n) if n == want => {
                                let t = started.elapsed();
                                report.add(
                                    "5 relay-only",
                                    Some(true),
                                    format!("path relay, {} MiB in {:.1}s", want / MIB, t.as_secs_f64()),
                                );
                                report.add(
                                    "11 throughput (relay)",
                                    Some(true),
                                    format!("{:.1} Mbps through {}", mbps(want, t), relay_of(&sidecar)),
                                );
                            }
                            Ok(n) => report.add("5 relay-only", Some(false), format!("got {n} of {want} bytes")),
                            Err(e) => report.add("5 relay-only", Some(false), e.to_string()),
                        }
                    }
                    Some(v) => report.add("5 relay-only", Some(false), format!("echo said {v}")),
                    None => report.add("5 relay-only", Some(false), "no response through the relay within 45s"),
                }
                client_b = Some(proc_b);
            }
            Err(e) => report.add("5 relay-only", Some(false), e),
        }
    }

    // 8: block drops the peer within a second and refuses reconnects.
    let sent_at = Instant::now();
    sidecar.send(&format!(r#"{{"cmd":"block","nodeId":"{node_a}"}}"#));
    let dropped = sidecar
        .wait_json(Duration::from_secs(3), |v| v["type"] == "peer_disconnected" && v["nodeId"] == node_a.as_str());
    let drop_time = sent_at.elapsed();
    thread::sleep(Duration::from_millis(1500));
    let refused = match echo(port_a) {
        Ok(v) => v.get("nodeId").is_none(),
        Err(_) => true,
    };
    let refused_status = request(port_a, &RequestSpec::get("/echo")).map(|r| r.status).unwrap_or(0);
    let pass = dropped.is_some() && drop_time <= Duration::from_secs(1) && refused;
    report.add(
        "8 block",
        Some(pass),
        format!(
            "peer_disconnected after {:?}, reconnect answered with HTTP {} ({})",
            drop_time,
            refused_status,
            if refused { "refused" } else { "NOT refused" }
        ),
    );
    sidecar.send(&format!(r#"{{"cmd":"unblock","nodeId":"{node_a}"}}"#));
    let back = retry(Duration::from_secs(15), Duration::from_millis(500), || {
        echo(port_a).ok().filter(|v| v["nodeId"] == node_a.as_str())
    });
    report.add(
        "8b unblock",
        Some(back.is_some()),
        if back.is_some() { "client A works again" } else { "client A still refused after 15s" },
    );

    // 9: closing stdin stops the sidecar within 2 seconds.
    let first_run_lines = sidecar.all_lines();
    sidecar.close_stdin();
    let exit = sidecar.wait_exit(Duration::from_secs(5));
    match exit {
        Some(t) => {
            report.add("9 lifetime", Some(t <= Duration::from_secs(2)), format!("exited {t:?} after stdin closed"))
        }
        None => {
            report.add("9 lifetime", Some(false), "still running 5s after stdin closed");
            sidecar.stop();
        }
    }
    drop(sidecar);

    // 4: restart keeps the node ID and client A reconnects without help.
    let (sidecar2, node_id2, _) = start_sidecar(&bins, args, &srv_data, &tmp.join("kinara-iroh-2.log"))?;
    let started = Instant::now();
    let back = retry(Duration::from_secs(60), Duration::from_millis(500), || {
        echo(port_a).ok().filter(|v| v["nodeId"] == node_a.as_str())
    });
    report.add(
        "4 persistence",
        Some(node_id2 == node_id && back.is_some()),
        format!(
            "node ID {} after restart; client A {}",
            if node_id2 == node_id { "unchanged" } else { "CHANGED" },
            match back {
                Some(_) => format!("reconnected by itself in {:.1}s", started.elapsed().as_secs_f64()),
                None => "did not reconnect within 60s".into(),
            }
        ),
    );

    // 10: every stdout line from both sidecar runs is a known, versioned event.
    let mut all = first_run_lines;
    all.extend(sidecar2.all_lines());
    let mut counts: BTreeMap<&'static str, usize> = BTreeMap::new();
    let mut bad = Vec::new();
    for line in &all {
        match serde_json::from_str::<Event>(line) {
            Ok(ev) => {
                let (kind, v) = match &ev {
                    Event::Ready(r) => ("ready", r.v),
                    Event::Status(s) => ("status", s.v),
                    Event::PeerConnected(p) => ("peer_connected", p.v),
                    Event::PeerDisconnected(p) => ("peer_disconnected", p.v),
                    Event::Error(e) => ("error", e.v),
                };
                if v != 1 {
                    bad.push(format!("v={v} in {kind}"));
                }
                *counts.entry(kind).or_default() += 1;
            }
            Err(e) => bad.push(format!("{e}: {}", line.chars().take(120).collect::<String>())),
        }
    }
    report.add(
        "10 status lines",
        Some(bad.is_empty() && counts.get("ready") == Some(&2)),
        if bad.is_empty() { format!("{} lines, all valid: {counts:?}", all.len()) } else { bad.join("; ") },
    );
    for line in &all {
        if let Ok(Event::Error(e)) = serde_json::from_str::<Event>(line) {
            println!("       sidecar error line: [{}] {}", e.code, e.message);
        }
    }

    // Cleanup.
    let mut sidecar2 = sidecar2;
    sidecar2.stop();
    client_a.stop();
    if let Some(mut b) = client_b {
        b.stop();
    }
    if !args.keep_temp && report.failed() == 0 {
        let _ = fs::remove_dir_all(&tmp);
    } else {
        println!("logs and keys kept in {}", tmp.display());
    }

    println!("\nsummary:");
    for r in &report.results {
        let mark = match r.pass {
            Some(true) => "PASS",
            Some(false) => "FAIL",
            None => "SKIP",
        };
        println!("  {mark}  {}", r.name);
    }
    Ok(report.failed())
}

fn relay_of(sidecar: &Proc) -> String {
    sidecar
        .all_lines()
        .iter()
        .rev()
        .find_map(|l| serde_json::from_str::<Value>(l).ok().filter(|v| v["type"] == "status"))
        .and_then(|v| v["relay"]["url"].as_str().map(str::to_string))
        .unwrap_or_else(|| "the relay".into())
}

fn report_backpressure(report: &mut Report, args: &Args, port: u16, sidecar: &Proc, client: &Proc, testserver: &Proc) {
    let pids = [("kinara-iroh", sidecar.pid()), ("kinara-connect", client.pid()), ("testserver", testserver.pid())];
    let base: Vec<Option<u64>> = pids.iter().map(|(_, p)| rss_kib(*p)).collect();
    if base.iter().all(Option::is_none) {
        report.add("2 backpressure", None, "cannot read process memory on this platform");
        return;
    }
    let mut peak = base.clone();
    let stop = Arc::new(Mutex::new(false));
    let rate = args.slow_kbps * 1024;
    let secs = args.backpressure_secs;

    let stop_reader = stop.clone();
    let reader = thread::spawn(move || -> io::Result<u64> {
        let mut resp = request(port, &RequestSpec::get("/file"))?;
        let start = Instant::now();
        let mut buf = vec![0u8; 16 * 1024];
        let mut total = 0u64;
        while start.elapsed() < Duration::from_secs(secs) && !*stop_reader.lock().unwrap() {
            let n = resp.reader.read(&mut buf)?;
            if n == 0 {
                break;
            }
            total += n as u64;
            // Sleep until the byte count matches the target rate.
            let due = Duration::from_secs_f64(total as f64 / rate as f64);
            if let Some(wait) = due.checked_sub(start.elapsed()) {
                thread::sleep(wait);
            }
        }
        Ok(total)
    });

    let start = Instant::now();
    while start.elapsed() < Duration::from_secs(secs) && !reader.is_finished() {
        thread::sleep(Duration::from_millis(500));
        for (i, (_, pid)) in pids.iter().enumerate() {
            if let Some(now) = rss_kib(*pid) {
                peak[i] = Some(peak[i].map_or(now, |p| p.max(now)));
            }
        }
    }
    *stop.lock().unwrap() = true;
    let read = reader.join().ok().and_then(|r| r.ok()).unwrap_or(0);

    let mut parts = Vec::new();
    let mut ok = true;
    for (i, (name, _)) in pids.iter().enumerate() {
        if let (Some(b), Some(p)) = (base[i], peak[i]) {
            let growth = p.saturating_sub(b) / 1024;
            parts.push(format!("{name} {} -> {} MiB (+{growth})", b / 1024, p / 1024));
            // Windows are 16 MiB per connection; anything near the file size means no backpressure.
            if *name != "testserver" && growth > 64 {
                ok = false;
            }
        }
    }
    report.add(
        "2 backpressure",
        Some(ok && read > 0),
        format!(
            "slow reader got {} MiB at {} KiB/s over {secs}s; RSS {}",
            read / MIB,
            args.slow_kbps,
            parts.join(", ")
        ),
    );
}

fn report_long_chunked(report: &mut Report, secs: u64, port: u16) {
    let path = format!("/slow?secs={secs}&kbps=256");
    let start = Instant::now();
    let mut last = Instant::now();
    let mut max_gap = Duration::ZERO;
    let result = request(port, &RequestSpec::get(&path)).and_then(|mut r| {
        let chunked = r.header("transfer-encoding").map(str::to_string);
        let n = r.body(|_| {
            max_gap = max_gap.max(last.elapsed());
            last = Instant::now();
        })?;
        Ok((n, chunked))
    });
    match result {
        Ok((n, chunked)) => {
            let expected = secs * 256 * 1024;
            let ok = n >= expected * 9 / 10 && max_gap < Duration::from_secs(5);
            report.add(
                "long chunked stream",
                Some(ok),
                format!(
                    "{:.0}s, {} KiB, transfer-encoding {:?}, longest gap {:?}",
                    start.elapsed().as_secs_f64(),
                    n / 1024,
                    chunked,
                    max_gap
                ),
            );
        }
        Err(e) => report.add("long chunked stream", Some(false), e.to_string()),
    }
}
