# Kinara for Windows

`build.ps1` makes `out\Kinara-Setup-<version>-x64.exe`, an unsigned Inno Setup
installer published by Macroboom LLC.

## Build (on Windows)

Needs Go 1.24+, Rust with the MSVC toolchain, Node 20+ and
[Inno Setup 6.3+](https://jrsoftware.org/isdl.php). `kinora-web` must sit next
to `kinora-server` (or pass `-WebRepo`).

```powershell
cd packaging\windows
powershell -ExecutionPolicy Bypass -File build.ps1
```

The first build downloads FFmpeg (GyanD 9.0.2 full build, from GitHub), shows its
SHA-256 and asks to record it in `ffmpeg-9.0.2-full_build.sha256`. Commit that
file; later builds refuse a download that doesn't match. `-FfmpegFlavor
essentials_build` gives a smaller installer without libplacebo (HDR tone
mapping falls back to zscale). `-SkipWeb` and `-SkipSidecar` speed up test builds.

## What the installer does

The wizard says all of this to the user before anything happens:

| Page | |
| --- | --- |
| What Kinara installs and does | every file, every Windows change, every network port, what goes online |
| Program folder | default `C:\Program Files\Kinara` |
| Where Kinara keeps your data | database and settings (default `C:\ProgramData\Kinara`), cache (default `...\Kinara\cache`) |
| How Kinara runs | Windows service (default) or only while signed in |
| Service account | Local System (default) or Network Service; service mode only |
| Tasks | firewall (private and domain networks), tray at sign-in, users may start/stop the service, desktop shortcut |
| Ready | the whole plan, with the real paths |

Then it:

- writes `kinara.env` in the data folder (`KINORA_DATA`, `KINORA_CACHE`,
  `KINORA_LISTEN`), keeping whatever else is already in it;
- records the folders, mode, account and port in `HKLM\Software\Kinara`;
- **service mode:** creates the `Kinara` service
  (`kinara-server.exe service`, delayed auto start, restarts after 5 s, 15 s,
  60 s on failure), points it at `kinara.env` through the service's
  `Environment` registry value, locks the data folder to SYSTEM and
  Administrators (`logs\` stays readable by Users) and starts it;
- **sign-in mode:** removes any old service, lets Users write the data and
  cache folders, and the tray runs `kinara-server.exe serve` hidden as the
  signed-in user (`KINORA_STOP_ON_EOF=1`: the server stops when the tray goes);
- adds firewall rules for `kinara-server.exe` and `kinora-iroh.exe` (if ticked).

Upgrades keep the folders, mode and account picked last time, stop the server
and tray first, and never change a service account set by hand in
`services.msc` unless a different one is picked in the wizard. Uninstalling
removes the service, rules and shortcuts, then asks whether to delete the data
(No by default).

## Things to know

- **Mapped drives.** A service can't see `Z:`-style mapped drives. Add network
  folders in Kinara as `\\server\share\...`. A share that needs a password: run
  the service as your own account (services.msc, Log On tab), or use sign-in mode.
- **Unsigned.** SmartScreen shows "Windows protected your PC" until the file
  has a reputation or is signed. Publish the `.sha256` next to the download.
- **Logs:** `<data>\logs\server.log` (rotated to `server.log.old` past 10 MB).

## Testing checklist (Windows 10/11 VM)

1. Fresh install, defaults: service running (`sc query Kinara`), tray green,
   Open Kinara loads, `icacls C:\ProgramData\Kinara` shows only SYSTEM and
   Administrators, a standard user can't open `kinara.env`.
2. Reboot without signing in: another device reaches `http://<pc>:8420`.
3. Tray: Stop, Start, Restart as a standard user (no UAC prompt if that task
   was ticked; UAC prompt if not).
4. Kill `kinara-server.exe` in Task Manager: it comes back within 5 s.
5. Reinstall over it choosing sign-in mode: service gone, tray starts the
   server, quitting the tray stops it, signing out stops it.
6. Custom cache on another drive, path with spaces and non-English letters.
7. Uninstall: answer No, data stays; reinstall: library is back. Uninstall
   again answering Yes: data and cache gone, media untouched.
