<#
.SYNOPSIS
  Builds the Kinara Windows installer: Kinara-Setup-<version>-x64.exe in out\.

.DESCRIPTION
  Run on Windows from this folder (packaging\windows):

      powershell -ExecutionPolicy Bypass -File build.ps1

  Needs: Go 1.24+, Rust (cargo, MSVC toolchain), Bun (for the web app),
  and Inno Setup 6.3+ (https://jrsoftware.org/isdl.php).

  Steps, each printed as it runs:
    1. kinara-server.exe and kinara-tray.exe   (go build, no cgo)
    2. kinara-iroh.exe                         (cargo build --release)
    3. web\                                    (bun install + bun run build in kinara-web)
    4. ffmpeg\                                 (pinned GyanD build, SHA-256 checked)
    5. THIRD-PARTY.txt
    6. the installer                           (iscc kinara.iss)

  FFmpeg: GyanD's releases publish no checksum, so the first build records
  the SHA-256 of the zip in ffmpeg-<version>-<flavor>.sha256 (commit it); every
  later build refuses a zip that doesn't match.
#>
param(
  # The version to stamp (a release tag without the v). Default: the one in
  # cmd\kinara-server\main.go. The release workflow passes the tag's.
  [string]$Version = "",
  [string]$WebRepo = "..\..\..\kinara-web",
  [string]$FfmpegVersion = "9.0.2",
  # full_build adds libplacebo (better HDR / Dolby Vision tone mapping) over
  # essentials_build; the server checks which filters exist either way.
  [ValidateSet("full_build", "essentials_build")]
  [string]$FfmpegFlavor = "full_build",
  # Kinara's TMDB key, built into the server so posters work out of the box.
  # Default: tmdb.key next to this script (git-ignored), else $env:KINARA_TMDB_KEY,
  # else KINARA_TMDB_KEY in the repo's .env (the one the dev server reads).
  [string]$TmdbKey = "",
  # Kinara's OpenSubtitles.com API key, for finding subtitles. Default:
  # opensubtitles.key next to this script, else $env:KINARA_OPENSUBTITLES_KEY.
  [string]$OpenSubtitlesKey = "",
  [switch]$SkipWeb,
  [switch]$SkipSidecar
)

$ErrorActionPreference = "Stop"
$here = $PSScriptRoot
$repo = Resolve-Path (Join-Path $here "..\..")
$stage = Join-Path $here "stage"
$cache = Join-Path $here ".cache"

function Step($text) { Write-Host "`n==> $text" -ForegroundColor Cyan }
function Need($cmd, $hint) {
  if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) { throw "$cmd not found. $hint" }
}
function Check-Exit($what) { if ($LASTEXITCODE -ne 0) { throw "$what failed (exit $LASTEXITCODE)" } }

# The version is the one the server reports (cmd/kinara-server/main.go).
if ($Version) {
  $version = $Version.TrimStart('v')
} else {
  $main = Get-Content (Join-Path $repo "cmd\kinara-server\main.go") -Raw
  if ($main -notmatch '(?m)^(?:const|var) version = "([^"]+)"') { throw "can't find the version in main.go" }
  $version = $Matches[1]
}
Write-Host "Kinara $version" -ForegroundColor Green

$iscc = @(
  "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
  "$env:ProgramFiles\Inno Setup 6\ISCC.exe",
  "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe"
) | Where-Object { Test-Path $_ } | Select-Object -First 1
if (-not $iscc) { throw "Inno Setup 6 not found. Install it from https://jrsoftware.org/isdl.php" }
Need go "Install Go from https://go.dev/dl/"

if (Test-Path $stage) { Remove-Item $stage -Recurse -Force }
New-Item -ItemType Directory -Force $stage, $cache | Out-Null

# The built-in TMDB key: never in the repository, only in the build.
if (-not $TmdbKey) {
  $keyFile = Join-Path $here "tmdb.key"
  if (Test-Path $keyFile) { $TmdbKey = (Get-Content $keyFile -Raw).Trim() }
  elseif ($env:KINARA_TMDB_KEY) { $TmdbKey = $env:KINARA_TMDB_KEY.Trim() }
  else {
    $dotenv = Join-Path $repo ".env"
    if (Test-Path $dotenv) {
      $line = Get-Content $dotenv | Where-Object { $_ -match '^\s*KINARA_TMDB_KEY\s*=' } | Select-Object -Last 1
      if ($line) { $TmdbKey = ($line -replace '^\s*KINARA_TMDB_KEY\s*=\s*', '').Trim().Trim('"', "'") }
    }
  }
}
if ($TmdbKey -match '\s') { throw "the TMDB key has spaces in it" }
if ($TmdbKey) {
  Write-Host "TMDB: building in a key ending ...$($TmdbKey.Substring([Math]::Max(0, $TmdbKey.Length - 4)))"
} else {
  Write-Warning "No TMDB key to build in (pass -TmdbKey, add tmdb.key, set KINARA_TMDB_KEY, or put it in the repo's .env): installs will show titles from file names, without artwork, until people add their own key"
}

# 1. Server and tray. -H=windowsgui: the tray opens no console window.
Step "Building kinara-server.exe and kinara-tray.exe"
$env:GOOS = "windows"; $env:GOARCH = "amd64"; $env:CGO_ENABLED = "0"
Push-Location $repo
try {
  # The version too, so the server reports the release it came from.
  $serverFlags = "-s -w -X main.version=$version"
  if ($TmdbKey) { $serverFlags += " -X main.builtinTMDBKey=$TmdbKey" }
  if (-not $OpenSubtitlesKey) {
    $osFile = Join-Path $here "opensubtitles.key"
    if (Test-Path $osFile) { $OpenSubtitlesKey = (Get-Content $osFile -Raw).Trim() }
    elseif ($env:KINARA_OPENSUBTITLES_KEY) { $OpenSubtitlesKey = $env:KINARA_OPENSUBTITLES_KEY.Trim() }
  }
  if ($OpenSubtitlesKey -match '\s') { throw "the OpenSubtitles key has spaces in it" }
  if ($OpenSubtitlesKey) { $serverFlags += " -X main.builtinOpenSubtitlesKey=$OpenSubtitlesKey" }
  go build -trimpath -ldflags $serverFlags -o (Join-Path $stage "kinara-server.exe") ./cmd/kinara-server; Check-Exit "go build server"
  go build -trimpath -ldflags "-s -w -H=windowsgui" -o (Join-Path $stage "kinara-tray.exe") ./cmd/kinara-tray; Check-Exit "go build tray"
} finally { Pop-Location }

# 2. Remote access sidecar.
if (-not $SkipSidecar) {
  Step "Building kinara-iroh.exe"
  Need cargo "Install Rust from https://rustup.rs (MSVC toolchain)"
  Push-Location (Join-Path $repo "sidecar")
  try {
    cargo build --release -p kinara-iroh; Check-Exit "cargo build"
    Copy-Item "target\release\kinara-iroh.exe" $stage
  } finally { Pop-Location }
} else {
  Write-Warning "Skipping kinara-iroh.exe: remote access won't work in this build"
  # Inno needs the file to exist.
  Set-Content (Join-Path $stage "kinara-iroh.exe") "" -NoNewline
}

# 3. Web app.
$web = Join-Path $stage "web"
if (-not $SkipWeb) {
  Step "Building the web app from $WebRepo"
  Push-Location (Resolve-Path (Join-Path $here $WebRepo))
  try {
    # Use whichever package manager the lockfile belongs to (kinara-web uses Bun).
    if ((Test-Path "bun.lock") -or (Test-Path "bun.lockb")) {
      Need bun "Install Bun from https://bun.sh (powershell -c `"irm bun.sh/install.ps1 | iex`")"
      bun install --frozen-lockfile; Check-Exit "bun install"
      bun run build; Check-Exit "bun run build"
    } elseif (Test-Path "package-lock.json") {
      Need npm "Install Node.js 20+ from https://nodejs.org"
      npm ci; Check-Exit "npm ci"
      npm run build; Check-Exit "npm run build"
    } else {
      Need npm "Install Node.js 20+ from https://nodejs.org"
      npm install; Check-Exit "npm install"
      npm run build; Check-Exit "npm run build"
    }
    if (-not (Test-Path "dist\index.html")) { throw "the web build finished but dist\index.html is missing" }
    Copy-Item "dist" $web -Recurse
  } finally { Pop-Location }
} else {
  Write-Warning "Skipping the web app: the server will serve only the API"
  New-Item -ItemType Directory -Force $web | Out-Null
  Set-Content (Join-Path $web "README.txt") "Web app not included in this build."
}

# 4. FFmpeg, pinned and checked.
Step "FFmpeg $FfmpegVersion ($FfmpegFlavor)"
$zipName = "ffmpeg-$FfmpegVersion-$FfmpegFlavor.zip"
$zip = Join-Path $cache $zipName
$url = "https://github.com/GyanD/codexffmpeg/releases/download/$FfmpegVersion/$zipName"
$pin = Join-Path $here "ffmpeg-$FfmpegVersion-$FfmpegFlavor.sha256"
if (-not (Test-Path $zip)) {
  Write-Host "Downloading $url"
  $ProgressPreference = "SilentlyContinue"   # makes Invoke-WebRequest far faster
  Invoke-WebRequest $url -OutFile "$zip.part"
  Move-Item "$zip.part" $zip -Force
}
$hash = (Get-FileHash $zip -Algorithm SHA256).Hash.ToLower()
if (Test-Path $pin) {
  $want = (Get-Content $pin -Raw).Trim().ToLower()
  if ($hash -ne $want) {
    Remove-Item $zip
    throw "FFmpeg zip SHA-256 is $hash but $([IO.Path]::GetFileName($pin)) says $want. Deleted the download; not building."
  }
  Write-Host "SHA-256 matches the pinned value."
} else {
  # In the release workflow nobody is there to answer: the pin must be committed.
  if ($env:CI) { throw "No $([IO.Path]::GetFileName($pin)) in the repository. Build once on your PC to record it, then commit it." }
  Write-Host "First build with this FFmpeg: SHA-256 $hash" -ForegroundColor Yellow
  Write-Host "Compare it with the zip from https://www.gyan.dev/ffmpeg/builds/ if you like."
  $ok = Read-Host "Record it in $([IO.Path]::GetFileName($pin)) and continue? (y/n)"
  if ($ok -ne "y") { throw "Stopped: FFmpeg hash not accepted" }
  Set-Content $pin $hash -NoNewline
}
$unz = Join-Path $cache "ffmpeg-unz"
if (Test-Path $unz) { Remove-Item $unz -Recurse -Force }
Expand-Archive $zip $unz
$root = Get-ChildItem $unz -Directory | Select-Object -First 1
$ff = Join-Path $stage "ffmpeg"
New-Item -ItemType Directory -Force $ff | Out-Null
Copy-Item (Join-Path $root.FullName "bin\ffmpeg.exe"), (Join-Path $root.FullName "bin\ffprobe.exe") $ff
Copy-Item (Join-Path $root.FullName "LICENSE") (Join-Path $ff "LICENSE.txt")
& (Join-Path $ff "ffmpeg.exe") -hide_banner -version | Select-Object -First 1

# 5. Licenses of what we ship.
Step "THIRD-PARTY.txt"
if (-not (Test-Path (Join-Path $repo "LICENSE"))) { throw "LICENSE is missing from the repo root" }
@"
Kinara for Windows

kinara-server.exe, kinara-tray.exe and kinara-iroh.exe are the Kinara server,
Copyright (C) 2026 Macroboom LLC, free software under the GNU Affero General
Public License version 3 or later (LICENSE.txt in this folder).

The Kinara web app (web\) is Copyright (C) 2026 Macroboom LLC, all rights
reserved. It is a separate program, not covered by the AGPL, included so the
server has a full interface in your browser.

Kinara for Windows also bundles this third-party software.

FFmpeg $FfmpegVersion ($FfmpegFlavor), in ffmpeg\
  Built by Gyan Doshi (https://www.gyan.dev/ffmpeg/builds/), unmodified.
  Licensed under the GNU General Public License v3; full text in
  ffmpeg\LICENSE.txt. Source code for this exact version:
  https://ffmpeg.org/releases/ffmpeg-$FfmpegVersion.tar.xz
  Build scripts and component list:
  https://github.com/GyanD/codexffmpeg/releases/tag/$FfmpegVersion
  Kinara runs ffmpeg.exe and ffprobe.exe as separate programs.

modernc.org/sqlite (BSD-3-Clause), compiled into kinara-server.exe.

kinara-iroh.exe is built with iroh (https://github.com/n0-computer/iroh,
MIT or Apache-2.0) and other Rust crates under MIT, Apache-2.0 and similar
permissive licenses; see sidecar\Cargo.lock in the Kinara server source.

The web app uses React and other packages under MIT and similar permissive
licenses.
"@ | Set-Content (Join-Path $stage "THIRD-PARTY.txt") -Encoding UTF8

# 6. Installer.
Step "Building the installer"
& $iscc "/DAppVersion=$version" "/DStage=$stage" (Join-Path $here "kinara.iss"); Check-Exit "iscc"
$out = Join-Path $here "out\Kinara-Setup-$version-x64.exe"
$sha = (Get-FileHash $out -Algorithm SHA256).Hash.ToLower()
"$sha  Kinara-Setup-$version-x64.exe" | Set-Content "$out.sha256" -NoNewline
Write-Host "`nDone: $out" -ForegroundColor Green
Write-Host "SHA-256: $sha (also in $([IO.Path]::GetFileName($out)).sha256; publish it next to the download)"
