//! kinara-connect: thin CLI over the tunnel client library, the same API the
//! mobile apps will call. Prints JSON lines on stdout: one "listening" line,
//! then "status" lines when the connection state changes (or periodically
//! with --status). Logs go to stderr.

use std::io::{BufRead, Write};
use std::process::ExitCode;
use std::time::{Duration, Instant};

use clap::Parser;
use kinara_tunnel::config::ConnectArgs;
use kinara_tunnel::{ClientStatus, TunnelClient};
use serde_json::json;

fn main() -> ExitCode {
    let args = ConnectArgs::parse();
    let filter = tracing_subscriber::EnvFilter::try_new(&args.log_level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new(kinara_tunnel::config::DEFAULT_LOG));
    tracing_subscriber::fmt().with_env_filter(filter).with_writer(std::io::stderr).with_ansi(false).init();

    let cfg = match args.to_config() {
        Ok(c) => c,
        Err(e) => {
            eprintln!("kinara-connect: {e}");
            return ExitCode::from(2);
        }
    };
    let client = match TunnelClient::start(cfg) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("kinara-connect: {e}");
            return ExitCode::from(1);
        }
    };

    let port = client.local_port();
    print_line(json!({
        "type": "listening",
        "v": 1,
        "port": port,
        "url": format!("http://127.0.0.1:{port}"),
        "nodeId": client.node_id(),
    }));
    eprintln!("kinara-connect: point the app at http://127.0.0.1:{port}");

    let (stdin_tx, stdin_rx) = std::sync::mpsc::channel::<()>();
    if args.exit_on_stdin_close {
        std::thread::spawn(move || {
            for line in std::io::stdin().lock().lines() {
                if line.is_err() {
                    break;
                }
            }
            let _ = stdin_tx.send(());
        });
    }

    // The client owns its own runtime; this one only waits for Ctrl-C.
    let rt = match tokio::runtime::Builder::new_current_thread().enable_all().build() {
        Ok(rt) => rt,
        Err(e) => {
            eprintln!("kinara-connect: {e}");
            return ExitCode::from(1);
        }
    };
    let interval = Duration::from_secs(args.status_interval_secs.max(1));
    rt.block_on(async {
        let ctrl_c = tokio::signal::ctrl_c();
        tokio::pin!(ctrl_c);
        let mut last: Option<(bool, String, Option<String>)> = None;
        let mut last_print = Instant::now();
        loop {
            tokio::select! {
                _ = &mut ctrl_c => break,
                _ = tokio::time::sleep(Duration::from_millis(250)) => {}
            }
            if stdin_rx.try_recv().is_ok() {
                break;
            }
            let st = client.status();
            let key = (st.connected, st.path.clone(), st.last_error.clone());
            let changed = last.as_ref() != Some(&key);
            let due = args.status && last_print.elapsed() >= interval;
            if changed || due {
                print_status(&st);
                last = Some(key);
                last_print = Instant::now();
            }
        }
    });
    drop(rt);
    client.stop();
    ExitCode::SUCCESS
}

fn print_status(st: &ClientStatus) {
    let mut v = serde_json::to_value(st).unwrap_or_default();
    if let Some(obj) = v.as_object_mut() {
        obj.insert("type".into(), json!("status"));
        obj.insert("v".into(), json!(1));
    }
    print_line(v);
}

fn print_line(v: serde_json::Value) {
    let mut out = std::io::stdout().lock();
    let _ = writeln!(out, "{v}");
    let _ = out.flush();
}
