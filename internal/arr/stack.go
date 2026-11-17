package arr

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// The download stack: Prowlarr, Radarr, Sonarr and qBittorrent in Docker,
// set up so Kinara can wire them together by itself. The setup script the
// owner runs writes a compose file whose services start with API keys and a
// qBittorrent password Kinara chose, so "Connect everything" needs nothing
// typed in. Everything shares one media folder, mounted at /data in every
// container (the layout the TRaSH guides recommend): downloads and the
// libraries are on one drive, so imports are instant moves, not copies.

// Stack ports on the host (inside, each service keeps its usual one).
const (
	PortRadarr   = 7878
	PortSonarr   = 8989
	PortProwlarr = 9696
	// qBittorrent's web interface: 8090, as 8080 is often taken. It's the
	// same port inside the container: qBittorrent turns away (401) any
	// request whose Host header names a port other than the one it listens
	// on, so a "8090:8080" mapping locks out everything from outside.
	PortQbit       = 8090
	portQbitInside = PortQbit
	// FlareSolverr gets past Cloudflare's checks for Prowlarr's indexers.
	PortFlareSolverr = 8191
)

// Stack is everything the setup script needs.
type Stack struct {
	// OS is the server's: linux, darwin or windows (the script's kind).
	OS string `json:"os"`
	// MediaDir is the host folder mounted at /data; MoviesDir, TVDir and
	// DownloadsDir are folders inside it (slash-separated).
	MediaDir     string `json:"mediaDir"`
	MoviesDir    string `json:"moviesDir"`
	TVDir        string `json:"tvDir"`
	DownloadsDir string `json:"downloadsDir"`
	PUID         int    `json:"puid"`
	PGID         int    `json:"pgid"`
	TZ           string `json:"tz"`
	// The secrets the services start with.
	RadarrKey   string `json:"radarrKey"`
	SonarrKey   string `json:"sonarrKey"`
	ProwlarrKey string `json:"prowlarrKey"`
	QbitUser    string `json:"qbitUser"`
	QbitPass    string `json:"qbitPass"`
	// TorrentPort is qBittorrent's port for peers, the same inside and
	// out (peers are told the port it listens on). Kinara picks one that's
	// free, so it doesn't clash with a qBittorrent already on the computer.
	TorrentPort int `json:"torrentPort"`
	// OwnQbit: the owner runs qBittorrent themselves (on this computer, with
	// their VPN, say), so the stack has none of its own.
	OwnQbit bool `json:"ownQbit"`
}

// DefaultTorrentPort is where the search for a free peer port starts.
const DefaultTorrentPort = 6881

func (s Stack) torrentPort() int {
	if s.TorrentPort > 0 {
		return s.TorrentPort
	}
	return DefaultTorrentPort
}

// NewSecrets fills in fresh API keys and a qBittorrent password.
func (s *Stack) NewSecrets() {
	s.RadarrKey, s.SonarrKey, s.ProwlarrKey = randomHex(16), randomHex(16), randomHex(16)
	s.QbitUser, s.QbitPass = "kinara", randomHex(12)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Container paths, as Radarr, Sonarr and qBittorrent see them.
func (s Stack) MoviesPath() string    { return path.Join("/data", s.MoviesDir) }
func (s Stack) TVPath() string        { return path.Join("/data", s.TVDir) }
func (s Stack) DownloadsPath() string { return path.Join("/data", s.DownloadsDir) }

// hostMedia is the media folder as the compose file writes it (forward
// slashes: Docker Desktop on Windows takes "D:/Media"). A drive's root
// keeps its slash ("E:/"): "E:" alone means the current folder on E.
func (s Stack) hostMedia() string {
	m := strings.ReplaceAll(strings.TrimRight(s.MediaDir, `/\`), `\`, "/")
	if m == "" || (len(m) == 2 && m[1] == ':') {
		m += "/"
	}
	return m
}

// mediaMount is the shared folder's volume entry, in the long form: the
// short "source:target" form splits on colons, so a Windows drive letter
// ("E:/") breaks it.
func (s Stack) mediaMount() string {
	return "      - type: bind\n        source: " + yamlQuote(s.hostMedia()) + "\n        target: /data\n"
}

// Compose is the compose file.
func (s Stack) Compose() string {
	env := func(app, key string) string {
		up := strings.ToUpper(app)
		return fmt.Sprintf(`      - PUID=%d
      - PGID=%d
      - TZ=%s
      - %s__AUTH__APIKEY=%s
      - %s__AUTH__METHOD=Forms
      - %s__AUTH__REQUIRED=DisabledForLocalAddresses
`, s.PUID, s.PGID, s.TZ, up, key, up, up)
	}
	media := s.mediaMount()
	// With the owner's own qBittorrent, Radarr and Sonarr reach it on the
	// computer itself (Docker Desktop names it host.docker.internal; on
	// Linux this line does).
	hostLine := ""
	if s.OwnQbit {
		hostLine = "    extra_hosts:\n      - \"host.docker.internal:host-gateway\"\n"
	}
	qbit := ""
	if !s.OwnQbit {
		qbit = fmt.Sprintf(`
  qbittorrent:
    image: lscr.io/linuxserver/qbittorrent:latest
    container_name: qbittorrent
    environment:
      - PUID=%d
      - PGID=%d
      - TZ=%s
      - WEBUI_PORT=%d
      - TORRENTING_PORT=%d
    volumes:
      - ./config/qbittorrent:/config
%s    ports:
      - "%d:%d"
      - "%d:%d"
      - "%d:%d/udp"
    restart: unless-stopped
`, s.PUID, s.PGID, s.TZ, portQbitInside, s.torrentPort(), media, PortQbit, portQbitInside,
			s.torrentPort(), s.torrentPort(), s.torrentPort(), s.torrentPort())
	}
	who := "qBittorrent downloads them"
	if s.OwnQbit {
		who = "your own qBittorrent (on this computer) downloads them"
	}
	return fmt.Sprintf(`# Kinara's download stack: Prowlarr finds releases, Radarr (movies) and
# Sonarr (shows) pick them, %s. Made by Kinara;
# Manage, Requests, "Connect everything" wires them together.
name: kinara-stack
services:
  prowlarr:
    image: lscr.io/linuxserver/prowlarr:latest
    container_name: prowlarr
    environment:
%s    volumes:
      - ./config/prowlarr:/config
    ports:
      - "%d:9696"
    restart: unless-stopped

  radarr:
    image: lscr.io/linuxserver/radarr:latest
    container_name: radarr
    environment:
%s    volumes:
      - ./config/radarr:/config
%s%s    ports:
      - "%d:7878"
    restart: unless-stopped

  sonarr:
    image: lscr.io/linuxserver/sonarr:latest
    container_name: sonarr
    environment:
%s    volumes:
      - ./config/sonarr:/config
%s%s    ports:
      - "%d:8989"
    restart: unless-stopped
%s
  # Prowlarr sends indexers tagged "flaresolverr" through it.
  flaresolverr:
    image: ghcr.io/flaresolverr/flaresolverr:latest
    container_name: flaresolverr
    environment:
      - LOG_LEVEL=info
      - TZ=%s
    ports:
      - "%d:8191"
    restart: unless-stopped
`, who, env("prowlarr", s.ProwlarrKey), PortProwlarr,
		env("radarr", s.RadarrKey), media, hostLine, PortRadarr,
		env("sonarr", s.SonarrKey), media, hostLine, PortSonarr,
		qbit, s.TZ, PortFlareSolverr)
}

// HostDownloads is the downloads folder as the computer itself has it: where
// the owner's own qBittorrent saves, and what Radarr and Sonarr map to
// /data/downloads (a remote path mapping).
func (s Stack) HostDownloads() string {
	m := strings.TrimRight(s.MediaDir, `/\`)
	sep := "/"
	if s.OS == "windows" {
		sep = `\`
	}
	return m + sep + strings.ReplaceAll(strings.Trim(s.DownloadsDir, "/"), "/", sep) + sep
}

func yamlQuote(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
}

// qbitHash is qBittorrent's form of a password: PBKDF2-SHA512, 100000
// rounds, salt and key in base64.
func qbitHash(pass string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	key, _ := pbkdf2.Key(sha512.New, pass, salt, 100000, 64)
	return base64.StdEncoding.EncodeToString(salt) + ":" + base64.StdEncoding.EncodeToString(key)
}

// qbitLogin is the two settings lines with Kinara's user name and password.
func (s Stack) qbitLogin(hash string) (user, pass string) {
	return `WebUI\Username=` + s.QbitUser, `WebUI\Password_PBKDF2="@ByteArray(` + hash + `)"`
}

// QbitConf is qBittorrent's settings file for its first start: Kinara's
// user name and password, downloads in the shared folder, and the legal
// notice accepted (it waits on it otherwise).
func (s Stack) QbitConf() string { return s.qbitConf(qbitHash(s.QbitPass)) }

func (s Stack) qbitConf(hash string) string {
	dl := s.DownloadsPath() + "/"
	user, pass := s.qbitLogin(hash)
	return fmt.Sprintf(`[LegalNotice]
Accepted=true

[BitTorrent]
Session\DefaultSavePath=%s
Session\TempPath=%sincomplete/
Session\TempPathEnabled=true

[Preferences]
Downloads\SavePath=%s
WebUI\Port=%d
%s
%s
`, dl, dl, dl, portQbitInside, user, pass)
}

// Script is the setup script for the server's system: its file name and
// text. It writes the compose file, makes the folders, writes qBittorrent's
// settings on its first start and after that sets its user name and
// password again (so they always match what Kinara signs in with), and
// starts the stack. Running it again is safe.
func (s Stack) Script() (name, text string) {
	hash := qbitHash(s.QbitPass)
	name, text, next := "kinara-stack.sh", s.shell(hash), "\ncd \"$STACK\""
	if s.OS == "windows" {
		name, text, next = "kinara-stack.ps1", s.powershell(hash), "\nSet-Location $Stack"
	}
	if s.OwnQbit {
		// No qBittorrent of the stack's: none of its settings, and the line
		// about it says it's the owner's.
		if i, j := strings.Index(text, "# qBittorrent's settings:"), strings.Index(text, next); i >= 0 && j > i {
			text = text[:i] + text[j+1:]
		}
		text = qbitLineRE.ReplaceAllString(text, "${1}qBittorrent: yours (Kinara connects to it)${2}")
	}
	return name, text
}

// qbitLineRE is the script's closing line about qBittorrent.
var qbitLineRE = regexp.MustCompile(`(?m)^((?:echo|Write-Host) ")qBittorrent: [^"]*(")`)

func (s Stack) shell(hash string) string {
	media := strings.TrimRight(s.hostMedia(), "/")
	dirs := []string{s.MoviesDir, s.TVDir, s.DownloadsDir + "/incomplete", s.DownloadsDir + "/" + Radarr, s.DownloadsDir + "/" + Sonarr}
	var mk []string
	for _, d := range dirs {
		mk = append(mk, shQuote(media+"/"+d))
	}
	user, pass := s.qbitLogin(hash)
	return fmt.Sprintf(`#!/bin/sh
# Kinara: sets up Prowlarr, Radarr, Sonarr, qBittorrent and FlareSolverr in Docker.
# Run it on the server, as yourself (not with sudo): sh kinara-stack.sh
# Running it again is safe: it updates the apps and keeps their settings.
set -e
command -v docker >/dev/null 2>&1 || { echo "Docker isn't installed yet: install it first (see Kinara's Manage, Requests)."; exit 1; }
docker compose version >/dev/null 2>&1 || { echo "Docker Compose is missing: install the docker-compose-plugin package."; exit 1; }

STACK="${KINARA_STACK_DIR:-$HOME/kinara-stack}"
mkdir -p "$STACK/config/prowlarr" "$STACK/config/radarr" "$STACK/config/sonarr" "$STACK/config/qbittorrent/qBittorrent"
mkdir -p %s

cat > "$STACK/compose.yaml" <<'KINARA_COMPOSE'
%sKINARA_COMPOSE

# qBittorrent's settings: all of them on its first start; after that only
# its user name and password, so they match what Kinara signs in with. It
# writes its settings when it stops, so it's stopped first.
CONF="$STACK/config/qbittorrent/qBittorrent/qBittorrent.conf"
if [ ! -f "$CONF" ]; then
cat > "$CONF" <<'KINARA_QBIT'
%sKINARA_QBIT
else
  docker stop qbittorrent >/dev/null 2>&1 || true
  KQ_USER=%s KQ_PASS=%s awk '
    /^WebUI\\(Username|Password_PBKDF2)=/ { next }
    { print }
    $0 == "[Preferences]" { print ENVIRON["KQ_USER"]; print ENVIRON["KQ_PASS"]; done = 1 }
    END { if (!done) { print ""; print "[Preferences]"; print ENVIRON["KQ_USER"]; print ENVIRON["KQ_PASS"] } }
  ' "$CONF" > "$CONF.new"
  mv "$CONF.new" "$CONF"
fi

cd "$STACK"
docker compose pull
docker compose up -d --remove-orphans
echo
echo "Started. Back in Kinara, press Connect everything."
echo "Prowlarr:    http://localhost:%d  (add your indexers here)"
echo "Radarr:      http://localhost:%d"
echo "Sonarr:      http://localhost:%d"
echo "qBittorrent: http://localhost:%d  (user %s; the password is on Kinara's setup page)"
`, strings.Join(mk, " "), s.Compose(), s.qbitConf(hash), shQuote(user), shQuote(pass), PortProwlarr, PortRadarr, PortSonarr, PortQbit, s.QbitUser)
}

func shQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func (s Stack) powershell(hash string) string {
	media := strings.TrimRight(strings.ReplaceAll(s.hostMedia(), "/", `\`), `\`)
	var mk []string
	for _, d := range []string{s.MoviesDir, s.TVDir, s.DownloadsDir + "/incomplete", s.DownloadsDir + "/" + Radarr, s.DownloadsDir + "/" + Sonarr} {
		mk = append(mk, psQuote(media+`\`+strings.ReplaceAll(d, "/", `\`)))
	}
	user, pass := s.qbitLogin(hash)
	return fmt.Sprintf(`# Kinara: sets up Prowlarr, Radarr, Sonarr, qBittorrent and FlareSolverr in Docker Desktop.
# Run it in PowerShell on the server: powershell -ExecutionPolicy Bypass -File kinara-stack.ps1
# Running it again is safe: it updates the apps and keeps their settings.
$ErrorActionPreference = "Stop"
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { Write-Host "Docker Desktop isn't installed yet: install it first (see Kinara's Manage, Requests)."; exit 1 }

$Stack = if ($env:KINARA_STACK_DIR) { $env:KINARA_STACK_DIR } else { Join-Path $HOME "kinara-stack" }
foreach ($d in @("config\prowlarr", "config\radarr", "config\sonarr", "config\qbittorrent\qBittorrent")) {
  New-Item -ItemType Directory -Force -Path (Join-Path $Stack $d) | Out-Null
}
foreach ($d in @(%s)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }

# Files are written as UTF-8 without a byte order mark (Windows PowerShell
# adds one otherwise).
$Utf8 = New-Object System.Text.UTF8Encoding $false
[IO.File]::WriteAllText((Join-Path $Stack "compose.yaml"), @'
%s'@, $Utf8)

# qBittorrent's settings: all of them on its first start; after that only
# its user name and password, so they match what Kinara signs in with. It
# writes its settings when it stops, so it's stopped first.
$Conf = Join-Path $Stack "config\qbittorrent\qBittorrent\qBittorrent.conf"
$Login = @(%s, %s)
if (-not (Test-Path $Conf)) {
  [IO.File]::WriteAllText($Conf, @'
%s'@, $Utf8)
} else {
  if (docker ps -q --filter "name=^qbittorrent$") { docker stop qbittorrent | Out-Null }
  $Keep = @(Get-Content $Conf | Where-Object { $_ -notmatch '^WebUI\\(Username|Password_PBKDF2)=' })
  $At = [Array]::IndexOf($Keep, '[Preferences]')
  if ($At -lt 0) {
    $Out = $Keep + @('', '[Preferences]') + $Login
  } else {
    $Out = @($Keep[0..$At]) + $Login
    if ($At + 1 -lt $Keep.Count) { $Out += $Keep[($At + 1)..($Keep.Count - 1)] }
  }
  [IO.File]::WriteAllLines($Conf, [string[]]$Out, $Utf8)
}

Set-Location $Stack
# PowerShell doesn't stop by itself when docker fails.
docker compose pull
if ($LASTEXITCODE -ne 0) { Write-Host "Downloading the apps failed (above). Is Docker Desktop running?"; exit 1 }
docker compose up -d --remove-orphans
if ($LASTEXITCODE -ne 0) { Write-Host "Starting the apps failed (above). Fix that, then run this again: it picks up where it stopped."; exit 1 }
Write-Host ""
Write-Host "Started. Back in Kinara, press Connect everything."
Write-Host "Prowlarr:    http://localhost:%d  (add your indexers here)"
Write-Host "Radarr:      http://localhost:%d"
Write-Host "Sonarr:      http://localhost:%d"
Write-Host "qBittorrent: http://localhost:%d  (user %s; the password is on Kinara's setup page)"
`, strings.Join(mk, ", "), s.Compose(), psQuote(user), psQuote(pass), s.qbitConf(hash), PortProwlarr, PortRadarr, PortSonarr, PortQbit, s.QbitUser)
}

func psQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// InstallStep is one step to get Docker on a system.
type InstallStep struct {
	Title   string `json:"title"`
	Command string `json:"command,omitempty"`
	Note    string `json:"note,omitempty"`
	URL     string `json:"url,omitempty"`
}

// DockerInstall is how to install Docker on a system.
func DockerInstall(goos string) []InstallStep {
	switch goos {
	case "windows":
		return []InstallStep{
			{Title: "Turn on WSL 2 (Docker Desktop runs on it)", Command: "wsl --install", Note: "In PowerShell as administrator. Restart the computer when it asks."},
			{Title: "Install Docker Desktop", Command: "winget install -e --id Docker.DockerDesktop", URL: "https://docs.docker.com/desktop/setup/install/windows-install/",
				Note: "Then open Docker Desktop once and accept its terms. Turn on \"Start Docker Desktop when you sign in\" so the stack comes back after a restart."},
		}
	case "darwin":
		return []InstallStep{
			{Title: "Install Docker Desktop", Command: "brew install --cask docker", URL: "https://docs.docker.com/desktop/setup/install/mac-install/",
				Note: "Without Homebrew, download it from Docker's site. Open it once and accept its terms; turn on starting at sign-in."},
		}
	}
	return []InstallStep{
		{Title: "Install Docker", Command: "curl -fsSL https://get.docker.com | sudo sh", URL: "https://docs.docker.com/engine/install/",
			Note: "Docker's own installer: it adds Docker and Compose for Ubuntu, Debian, Fedora and most others. On a NAS (Synology, Unraid, TrueNAS), use its own Docker or Container Manager app instead."},
		{Title: "Let your user run Docker", Command: "sudo usermod -aG docker $USER && newgrp docker",
			Note: "So the setup script runs without sudo, and the files it makes belong to you."},
	}
}
