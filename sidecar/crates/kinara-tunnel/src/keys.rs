//! Persistent iroh secret key. The node ID derived from it is how devices
//! find the server, so losing or rotating it unpairs every device.

use std::fs;
use std::io::Write;
use std::path::Path;

use anyhow::{bail, Context, Result};
use iroh::SecretKey;
use tracing::{info, warn};

use crate::hexutil;

/// Loads the key at `path`, creating it on first run. An existing file that
/// cannot be parsed is an error rather than silently replaced, because a new
/// key would change the node ID.
pub fn load_or_create(path: &Path) -> Result<SecretKey> {
    if path.exists() {
        let text = fs::read_to_string(path).with_context(|| format!("reading {}", path.display()))?;
        let bytes = hexutil::decode(&text).with_context(|| format!("{} is not a hex encoded key", path.display()))?;
        let bytes: [u8; 32] = match bytes.try_into() {
            Ok(b) => b,
            Err(_) => bail!("{} must hold exactly 32 bytes (64 hex characters)", path.display()),
        };
        return Ok(SecretKey::from_bytes(&bytes));
    }

    let dir = path.parent().context("key path has no parent directory")?;
    fs::create_dir_all(dir).with_context(|| format!("creating {}", dir.display()))?;
    restrict_dir(dir);

    let key = SecretKey::generate();
    let tmp = path.with_extension("key.tmp");
    let _ = fs::remove_file(&tmp);
    {
        let mut f = create_private(&tmp).with_context(|| format!("creating {}", tmp.display()))?;
        f.write_all(hexutil::encode(&key.to_bytes()).as_bytes())?;
        f.write_all(b"\n")?;
        f.sync_all()?;
    }
    restrict_file(&tmp);
    // Rename so a crash mid-write never leaves a truncated key behind.
    fs::rename(&tmp, path).with_context(|| format!("moving key into {}", path.display()))?;
    info!(path = %path.display(), node_id = %key.public(), "created new iroh secret key");
    Ok(key)
}

#[cfg(unix)]
fn create_private(path: &Path) -> std::io::Result<fs::File> {
    use std::os::unix::fs::OpenOptionsExt;
    fs::OpenOptions::new().write(true).create_new(true).mode(0o600).open(path)
}

#[cfg(not(unix))]
fn create_private(path: &Path) -> std::io::Result<fs::File> {
    fs::OpenOptions::new().write(true).create_new(true).open(path)
}

#[cfg(unix)]
fn restrict_dir(dir: &Path) {
    use std::os::unix::fs::PermissionsExt;
    if let Err(e) = fs::set_permissions(dir, fs::Permissions::from_mode(0o700)) {
        warn!(dir = %dir.display(), "could not restrict key directory: {e}");
    }
}

#[cfg(not(unix))]
fn restrict_dir(_dir: &Path) {}

#[cfg(unix)]
fn restrict_file(_path: &Path) {}

/// Windows has no mode bits. Drop inherited ACL entries and grant only the
/// current user, which is the closest equivalent of 0600. Best effort: if
/// icacls is missing the file keeps the ACL of the data folder.
#[cfg(windows)]
fn restrict_file(path: &Path) {
    use std::process::Command;
    let Ok(user) = std::env::var("USERNAME") else {
        warn!("USERNAME not set; key file keeps inherited permissions");
        return;
    };
    use std::os::windows::process::CommandExt;
    // kinara-server starts the sidecar without a console; without this flag
    // icacls would open a console window of its own.
    const CREATE_NO_WINDOW: u32 = 0x0800_0000;
    let result = Command::new("icacls")
        .arg(path)
        .arg("/inheritance:r")
        .arg("/grant:r")
        .arg(format!("{user}:F"))
        .creation_flags(CREATE_NO_WINDOW)
        .output();
    match result {
        Ok(out) if out.status.success() => {}
        Ok(out) => warn!("icacls failed: {}", String::from_utf8_lossy(&out.stderr).trim()),
        Err(e) => warn!("could not run icacls: {e}"),
    }
}

#[cfg(not(any(unix, windows)))]
fn restrict_file(_path: &Path) {}
