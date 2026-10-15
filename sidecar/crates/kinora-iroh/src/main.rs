//! kinora-iroh: server-side sidecar. kinora-server starts it with piped
//! stdin/stdout/stderr, reads JSON Lines status from stdout, writes JSON
//! Lines commands to stdin, and closes stdin to stop it.

use std::collections::BTreeMap;
use std::io::{BufRead, Write};
use std::process::ExitCode;
use std::str::FromStr;
use std::sync::mpsc as std_mpsc;
use std::thread;
use std::time::Duration;

use clap::Parser;
use kinora_tunnel::config::ServerArgs;
use kinora_tunnel::events::{Command, ErrorLine, Event, Status};
use kinora_tunnel::iroh::EndpointId;
use kinora_tunnel::Server;
use tokio::sync::mpsc;
use tracing::{error, info, warn};

/// How long to wait for a relay before announcing "ready" anyway.
const READY_RELAY_WAIT: Duration = Duration::from_secs(10);
/// How often to look for changes worth an immediate status line.
const CHANGE_POLL: Duration = Duration::from_secs(1);

fn main() -> ExitCode {
    let args = ServerArgs::parse();
    init_logging(&args.log_level);

    let out = Output::spawn();
    let code = match tokio::runtime::Builder::new_multi_thread().enable_all().build() {
        Ok(rt) => rt.block_on(run(args, out.clone())),
        Err(e) => {
            out.emit(&ErrorLine::event("runtime", format!("cannot start runtime: {e}")));
            1
        }
    };
    out.finish();
    // The stdin reader thread may still sit in a blocking read; exiting the
    // process directly avoids waiting on it.
    std::process::exit(code)
}

fn init_logging(filter: &str) {
    let filter = tracing_subscriber::EnvFilter::try_new(filter)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(kinora_tunnel::config::DEFAULT_LOG));
    tracing_subscriber::fmt().with_env_filter(filter).with_writer(std::io::stderr).with_ansi(false).init();
}

/// stdout belongs to the JSON Lines contract only. A dedicated thread writes
/// and flushes each line so a slow reader never stalls the async runtime.
#[derive(Clone)]
struct Output {
    tx: std_mpsc::Sender<OutMsg>,
}

enum OutMsg {
    Line(String),
    Finish(std_mpsc::Sender<()>),
}

impl Output {
    fn spawn() -> Output {
        let (tx, rx) = std_mpsc::channel::<OutMsg>();
        thread::Builder::new()
            .name("stdout".into())
            .spawn(move || {
                let stdout = std::io::stdout();
                let mut broken = false;
                for msg in rx {
                    match msg {
                        OutMsg::Line(line) if !broken => {
                            let mut lock = stdout.lock();
                            if writeln!(lock, "{line}").and_then(|_| lock.flush()).is_err() {
                                // Parent is gone; stdin EOF will follow and stop us.
                                broken = true;
                            }
                        }
                        OutMsg::Line(_) => {}
                        OutMsg::Finish(ack) => {
                            let _ = ack.send(());
                        }
                    }
                }
            })
            .expect("spawn stdout thread");
        Output { tx }
    }

    fn emit(&self, ev: &Event) {
        let _ = self.tx.send(OutMsg::Line(ev.to_line()));
    }

    /// Waits until everything queued so far is written.
    fn finish(&self) {
        let (ack, done) = std_mpsc::channel();
        if self.tx.send(OutMsg::Finish(ack)).is_ok() {
            let _ = done.recv_timeout(Duration::from_secs(1));
        }
    }
}

enum Input {
    Line(String),
    Eof,
}

/// Reads stdin on a plain thread. EOF is the stop signal from kinora-server
/// and also what happens when kinora-server dies, so the sidecar never
/// outlives its parent.
fn spawn_stdin_reader() -> mpsc::UnboundedReceiver<Input> {
    let (tx, rx) = mpsc::unbounded_channel();
    thread::Builder::new()
        .name("stdin".into())
        .spawn(move || {
            let stdin = std::io::stdin();
            for line in stdin.lock().lines() {
                match line {
                    Ok(l) => {
                        if tx.send(Input::Line(l)).is_err() {
                            return;
                        }
                    }
                    Err(_) => break,
                }
            }
            let _ = tx.send(Input::Eof);
        })
        .expect("spawn stdin thread");
    rx
}

async fn run(args: ServerArgs, out: Output) -> i32 {
    let cfg = match args.to_config() {
        Ok(c) => c,
        Err(e) => {
            error!("invalid configuration: {e}");
            out.emit(&ErrorLine::event("config", e));
            return 2;
        }
    };
    let mut input = spawn_stdin_reader();
    let (ev_tx, mut ev_rx) = mpsc::unbounded_channel::<Event>();

    let server = match Server::bind(cfg, ev_tx).await {
        Ok(s) => s,
        Err(e) => {
            error!("startup failed: {e:#}");
            out.emit(&ErrorLine::event("bind_failed", format!("{e:#}")));
            return 1;
        }
    };

    let online = server.wait_online(READY_RELAY_WAIT);
    tokio::pin!(online);
    // Created once so a signal between loop iterations is not missed.
    let interrupt = tokio::signal::ctrl_c();
    tokio::pin!(interrupt);
    let terminate = terminate_signal();
    tokio::pin!(terminate);
    let mut ready_sent = false;
    let mut status_tick = tokio::time::interval(args.status_interval());
    status_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut change_tick = tokio::time::interval(CHANGE_POLL);
    change_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    let mut last_fingerprint = String::new();
    // Warning codes currently active, so each produces one error line when it appears.
    let mut active_warnings: BTreeMap<String, String> = BTreeMap::new();

    let reason = loop {
        tokio::select! {
            got_relay = &mut online, if !ready_sent => {
                if !got_relay {
                    warn!("no relay connection after {READY_RELAY_WAIT:?}; announcing ready anyway");
                }
                let ready = server.ready();
                info!(node_id = %ready.node_id, "ready");
                out.emit(&Event::Ready(ready));
                ready_sent = true;
                let st = server.status();
                last_fingerprint = fingerprint(&st);
                emit_status(&out, st, &mut active_warnings);
            }
            _ = status_tick.tick(), if ready_sent => {
                let st = server.status();
                last_fingerprint = fingerprint(&st);
                emit_status(&out, st, &mut active_warnings);
            }
            _ = change_tick.tick(), if ready_sent => {
                let st = server.status();
                let fp = fingerprint(&st);
                if fp != last_fingerprint {
                    last_fingerprint = fp;
                    emit_status(&out, st, &mut active_warnings);
                }
            }
            Some(ev) = ev_rx.recv() => out.emit(&ev),
            msg = input.recv() => match msg {
                None | Some(Input::Eof) => break "stdin closed",
                Some(Input::Line(line)) => {
                    if let Some(reason) = handle_command(&server, &out, line.trim(), &mut active_warnings) {
                        break reason;
                    }
                }
            },
            _ = &mut interrupt => break "interrupt",
            _ = &mut terminate => break "terminate signal",
        }
    };

    info!("shutting down: {reason}");
    server.shutdown(args.shutdown_grace()).await;
    0
}

/// Returns Some(reason) when the command asks the sidecar to stop.
fn handle_command(
    server: &Server,
    out: &Output,
    line: &str,
    active_warnings: &mut BTreeMap<String, String>,
) -> Option<&'static str> {
    if line.is_empty() {
        return None;
    }
    let cmd: Command = match serde_json::from_str(line) {
        Ok(c) => c,
        Err(e) => {
            warn!("bad command {line:?}: {e}");
            out.emit(&ErrorLine::event("bad_command", format!("cannot parse command: {e}")));
            return None;
        }
    };
    match cmd {
        Command::Block { node_id } => match EndpointId::from_str(node_id.trim()) {
            Ok(id) => {
                server.block(id);
                emit_status(out, server.status(), active_warnings);
            }
            Err(e) => out.emit(&ErrorLine::event("bad_command", format!("invalid nodeId {node_id:?}: {e}"))),
        },
        Command::Unblock { node_id } => match EndpointId::from_str(node_id.trim()) {
            Ok(id) => {
                server.unblock(id);
                emit_status(out, server.status(), active_warnings);
            }
            Err(e) => out.emit(&ErrorLine::event("bad_command", format!("invalid nodeId {node_id:?}: {e}"))),
        },
        Command::Status => emit_status(out, server.status(), active_warnings),
        Command::Shutdown => return Some("shutdown command"),
    }
    None
}

/// Emits error lines for warnings that just appeared, then the status line.
fn emit_status(out: &Output, st: Status, active: &mut BTreeMap<String, String>) {
    let now: BTreeMap<String, String> = st.warnings.iter().map(|w| (w.code.clone(), w.message.clone())).collect();
    for (code, message) in &now {
        if !active.contains_key(code) {
            warn!(code = %code, "{message}");
            out.emit(&ErrorLine::event(code, message.clone()));
        }
    }
    *active = now;
    out.emit(&Event::Status(st));
}

/// The parts of status whose change deserves an immediate line. Counters and
/// RTT move constantly and only go out on the periodic tick.
fn fingerprint(st: &Status) -> String {
    let mut s = format!(
        "{}|{:?}|{}|{}|",
        st.ticket,
        st.relay.url,
        st.relay.connected,
        st.net_report.as_ref().map(|n| n.udp).unwrap_or(false)
    );
    for a in &st.addrs {
        s.push_str(&a.addr);
        s.push(',');
    }
    s.push('|');
    for w in &st.warnings {
        s.push_str(&w.code);
        s.push(',');
    }
    s.push('|');
    for b in &st.blocked {
        s.push_str(b);
        s.push(',');
    }
    s.push('|');
    for p in &st.peers {
        s.push_str(&format!("{}:{}:{:?};", p.node_id, p.path.as_str(), p.remote_addr));
    }
    s
}

#[cfg(unix)]
async fn terminate_signal() {
    use tokio::signal::unix::{signal, SignalKind};
    match signal(SignalKind::terminate()) {
        Ok(mut s) => {
            s.recv().await;
        }
        Err(_) => std::future::pending::<()>().await,
    }
}

#[cfg(windows)]
async fn terminate_signal() {
    // Console close and Ctrl-Break, the Windows counterparts of SIGTERM.
    use tokio::signal::windows::{ctrl_break, ctrl_close};
    match (ctrl_break(), ctrl_close()) {
        (Ok(mut brk), Ok(mut close)) => {
            tokio::select! {
                _ = brk.recv() => {}
                _ = close.recv() => {}
            }
        }
        _ => std::future::pending::<()>().await,
    }
}

#[cfg(not(any(unix, windows)))]
async fn terminate_signal() {
    std::future::pending::<()>().await
}
