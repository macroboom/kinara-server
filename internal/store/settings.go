package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
)

// Settings are the server-wide options the owner sets on the manager page.
type Settings struct {
	// ListAccounts shows this server's accounts on the sign-in screen, so a
	// TV or phone can pick one instead of typing a user name.
	ListAccounts bool `json:"listAccounts"`
	// AllowSignup lets anyone who can reach the server make an account from
	// the sign-in screen. Off by default: the owner adds accounts instead.
	AllowSignup bool `json:"allowSignup"`
	// ServerName is what apps and the cloud call this server; "" uses the
	// computer's name.
	ServerName string `json:"serverName"`
	// PriorityDevice is the device (see DeviceID) that keeps its quality
	// when the server can't convert everything at once: other devices step
	// down first, and it gets a slot even when all are taken. "" for none.
	PriorityDevice string `json:"priorityDevice"`
	// UpdateCheck asks the downloads site twice a day whether a newer
	// Kinara is out (it sends only this server's version and system).
	UpdateCheck bool `json:"updateCheck"`
	// AutoUpdate installs a new release on its own, at night when nothing
	// is playing (Windows and macOS installs; apt and Docker update
	// themselves their own way).
	AutoUpdate bool `json:"autoUpdate"`
	// GroupCollections shows movies of the same franchise (TMDB
	// collections, like "The Lord of the Rings") as one card in movie
	// libraries, which opens to the movies in order.
	GroupCollections bool `json:"groupCollections"`
	// HomeSignIn lets devices on the home network sign in by picking an
	// account, with no password: they can watch, from home only, and
	// can't manage the server. Off asks for the password as before.
	HomeSignIn bool `json:"homeSignIn"`
	// HasHomePIN says a PIN is asked for that (a shared network, a dorm).
	// Read only here: it's set with PUT /api/v1/settings/home-pin.
	HasHomePIN bool `json:"hasHomePin"`
	// WebRemotePlay lets the Kinara account watch in a web browser away
	// from home (cloud.kinara.media/watch): a pass from the cloud signs the
	// browser in. Off, the cloud page says it's off and passes are refused.
	WebRemotePlay bool `json:"webRemotePlay"`
	// RelayMaxKbps caps video sent through the cloud relay, in kbit/s (0
	// for no cap): what a browser at work gets when it can't reach this
	// server directly. Direct connections aren't capped.
	RelayMaxKbps int `json:"relayMaxKbps"`
}

// Relay cap limits (kbit/s), for PUT /api/v1/settings.
const (
	MinRelayKbps = 1000
	MaxRelayKbps = 40000
)

// homePINKey holds the hash of the home sign-in PIN ("" for none).
const homePINKey = "home_pin_hash"

// HomePINHash is the home sign-in PIN's hash, "" when there's none.
func (s *Store) HomePINHash(ctx context.Context) (string, error) {
	v, _, err := s.setting(ctx, homePINKey)
	return v, err
}

// SetHomePINHash sets (or, with "", removes) the home sign-in PIN.
func (s *Store) SetHomePINHash(ctx context.Context, hash string) error {
	return s.setSetting(ctx, homePINKey, hash)
}

// DefaultSettings applies until the owner changes something.
var DefaultSettings = Settings{ListAccounts: true, AllowSignup: false, UpdateCheck: true, AutoUpdate: true, GroupCollections: true, HomeSignIn: true,
	WebRemotePlay: true, RelayMaxKbps: 4000}

func (s *Store) setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) setSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (s *Store) boolSetting(ctx context.Context, key string, def bool) (bool, error) {
	v, ok, err := s.setting(ctx, key)
	if err != nil || !ok {
		return def, err
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, nil
	}
	return b, nil
}

// GetSettings reads the server settings, with defaults for anything unset.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	out := DefaultSettings
	var err error
	if out.ListAccounts, err = s.boolSetting(ctx, "list_accounts", DefaultSettings.ListAccounts); err != nil {
		return out, err
	}
	if out.AllowSignup, err = s.boolSetting(ctx, "allow_signup", DefaultSettings.AllowSignup); err != nil {
		return out, err
	}
	if out.UpdateCheck, err = s.boolSetting(ctx, "update_check", DefaultSettings.UpdateCheck); err != nil {
		return out, err
	}
	if out.AutoUpdate, err = s.boolSetting(ctx, "auto_update", DefaultSettings.AutoUpdate); err != nil {
		return out, err
	}
	if out.GroupCollections, err = s.boolSetting(ctx, "group_collections", DefaultSettings.GroupCollections); err != nil {
		return out, err
	}
	if out.HomeSignIn, err = s.boolSetting(ctx, "home_sign_in", DefaultSettings.HomeSignIn); err != nil {
		return out, err
	}
	if out.WebRemotePlay, err = s.boolSetting(ctx, "web_remote_play", DefaultSettings.WebRemotePlay); err != nil {
		return out, err
	}
	if v, ok, err := s.setting(ctx, "relay_max_kbps"); err != nil {
		return out, err
	} else if n, perr := strconv.Atoi(v); ok && perr == nil && n >= 0 {
		out.RelayMaxKbps = n
	}
	pin, err := s.HomePINHash(ctx)
	if err != nil {
		return out, err
	}
	out.HasHomePIN = pin != ""
	if out.ServerName, _, err = s.setting(ctx, "server_name"); err != nil {
		return out, err
	}
	out.PriorityDevice, _, err = s.setting(ctx, "priority_device")
	return out, err
}

// PutSettings saves the server settings.
func (s *Store) PutSettings(ctx context.Context, st Settings) error {
	if err := s.setSetting(ctx, "list_accounts", strconv.FormatBool(st.ListAccounts)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "server_name", st.ServerName); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "priority_device", st.PriorityDevice); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "update_check", strconv.FormatBool(st.UpdateCheck)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "auto_update", strconv.FormatBool(st.AutoUpdate)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "group_collections", strconv.FormatBool(st.GroupCollections)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "home_sign_in", strconv.FormatBool(st.HomeSignIn)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "web_remote_play", strconv.FormatBool(st.WebRemotePlay)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "relay_max_kbps", strconv.Itoa(st.RelayMaxKbps)); err != nil {
		return err
	}
	return s.setSetting(ctx, "allow_signup", strconv.FormatBool(st.AllowSignup))
}

// Value reads a value the server keeps for itself in the settings table
// ("" when unset): things it remembers between runs, not owner settings.
func (s *Store) Value(ctx context.Context, key string) (string, error) {
	v, _, err := s.setting(ctx, key)
	return v, err
}

// SetValue saves a value read back with Value.
func (s *Store) SetValue(ctx context.Context, key, value string) error {
	return s.setSetting(ctx, key, value)
}

// BackupSettings are the owner's choices for automatic backups.
type BackupSettings struct {
	// Auto makes a backup every night.
	Auto bool `json:"auto"`
	// Keep is how many scheduled and manual backups to keep (oldest go first).
	Keep int `json:"keep"`
	// Dir is the backups folder; "" for "backups" in the data folder. A
	// folder on another drive, a NAS or a synced folder (Dropbox, OneDrive,
	// iCloud Drive) keeps backups safe if this computer's drive fails.
	Dir string `json:"dir"`
}

// DefaultBackupSettings apply until the owner changes something.
var DefaultBackupSettings = BackupSettings{Auto: true, Keep: 7}

// GetBackupSettings reads the backup settings.
func (s *Store) GetBackupSettings(ctx context.Context) (BackupSettings, error) {
	out := DefaultBackupSettings
	var err error
	if out.Auto, err = s.boolSetting(ctx, "backup_auto", out.Auto); err != nil {
		return out, err
	}
	if v, ok, err := s.setting(ctx, "backup_keep"); err != nil {
		return out, err
	} else if n, perr := strconv.Atoi(v); ok && perr == nil && n > 0 {
		out.Keep = n
	}
	out.Dir, _, err = s.setting(ctx, "backup_dir")
	return out, err
}

// PutBackupSettings saves the backup settings.
func (s *Store) PutBackupSettings(ctx context.Context, b BackupSettings) error {
	if err := s.setSetting(ctx, "backup_auto", strconv.FormatBool(b.Auto)); err != nil {
		return err
	}
	if err := s.setSetting(ctx, "backup_keep", strconv.Itoa(b.Keep)); err != nil {
		return err
	}
	return s.setSetting(ctx, "backup_dir", b.Dir)
}

// TranscodeSettings are the owner's transcoding choices (Manage,
// Transcoding). Zero values are what Kinara picks by itself.
type TranscodeSettings struct {
	// Encoder is the graphics card backend: "auto", "none" (CPU only),
	// "nvenc", "qsv", "vaapi", "amf" or "videotoolbox". "" follows
	// KINARA_HWACCEL. Device is the Linux render node ("" follows
	// KINARA_HW_DEVICE, else each in turn).
	Encoder string `json:"encoder"`
	Device  string `json:"device"`
	// Speed is "faster", "balanced" or "quality" ("" is balanced).
	Speed string `json:"speed"`
	// MaxJobs is how many streams convert at once (0: automatic).
	MaxJobs int `json:"maxJobs"`
	// DefaultKbps is the bitrate for a conversion when the player doesn't
	// ask for one (0: 8000).
	DefaultKbps int `json:"defaultKbps"`
	// The most a conversion may use at home and away from home, in kbit/s
	// and picture height (0: no limit beyond what the player asks). A file
	// over the limit is re-encoded down to it.
	HomeMaxKbps   int `json:"homeMaxKbps"`
	HomeMaxHeight int `json:"homeMaxHeight"`
	AwayMaxKbps   int `json:"awayMaxKbps"`
	AwayMaxHeight int `json:"awayMaxHeight"`
	// ToneMap is "auto" or "zscale"; ToneMapCurve "auto", "hable",
	// "mobius", "reinhard" or "bt.2390" (see transcode.Prefs).
	ToneMap      string `json:"toneMap"`
	ToneMapCurve string `json:"toneMapCurve"`
	// AudioChannels caps converted sound (0: up to 5.1; 2: stereo).
	AudioChannels int `json:"audioChannels"`
	// StereoKbps and SurroundKbps are the AAC bitrates (0: 192 and 384).
	StereoKbps   int `json:"stereoKbps"`
	SurroundKbps int `json:"surroundKbps"`
	// DialogueBoost lifts dialogue when surround is mixed down to stereo.
	DialogueBoost bool `json:"dialogueBoost"`
}

// DefaultTranscodeSettings apply until the owner changes something.
var DefaultTranscodeSettings = TranscodeSettings{Encoder: "", Speed: "balanced", ToneMap: "auto", ToneMapCurve: "auto", DialogueBoost: true}

const transcodeSettingsKey = "transcode_settings"

// GetTranscodeSettings reads the transcoding settings.
func (s *Store) GetTranscodeSettings(ctx context.Context) (TranscodeSettings, error) {
	out := DefaultTranscodeSettings
	v, ok, err := s.setting(ctx, transcodeSettingsKey)
	if err != nil || !ok || v == "" {
		return out, err
	}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return DefaultTranscodeSettings, nil
	}
	return out, nil
}

// PutTranscodeSettings saves the transcoding settings.
func (s *Store) PutTranscodeSettings(ctx context.Context, t TranscodeSettings) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.setSetting(ctx, transcodeSettingsKey, string(b))
}

// ArrServer is a Radarr or Sonarr the server sends approved requests to.
type ArrServer struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
	APIKey  string `json:"apiKey"`
	// ProfileID and RootFolder are where new titles go.
	ProfileID  int    `json:"profileId"`
	RootFolder string `json:"rootFolder"`
	Tags       []int  `json:"tags"`
	// Search starts looking for a title as soon as it's added.
	Search bool `json:"search"`
	// MinimumAvailability (Radarr): announced, inCinemas or released.
	MinimumAvailability string `json:"minimumAvailability,omitempty"`
	// SeasonFolder and SeriesType (Sonarr).
	SeasonFolder bool   `json:"seasonFolder"`
	SeriesType   string `json:"seriesType,omitempty"`
}

// Ready says whether requests can be sent to it.
func (a ArrServer) Ready() bool {
	return a.Enabled && a.URL != "" && a.APIKey != "" && a.ProfileID > 0 && a.RootFolder != ""
}

// ArrLink is Prowlarr: where it is and its API key.
type ArrLink struct {
	URL    string `json:"url"`
	APIKey string `json:"apiKey"`
}

// QbitLink is qBittorrent's web interface and its sign-in.
type QbitLink struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// ArrStack is the Docker stack Kinara's setup script makes (see arr.Stack):
// its folders and the secrets its services start with.
type ArrStack struct {
	OS           string `json:"os"`
	MediaDir     string `json:"mediaDir"`
	MoviesDir    string `json:"moviesDir"`
	TVDir        string `json:"tvDir"`
	DownloadsDir string `json:"downloadsDir"`
	PUID         int    `json:"puid"`
	PGID         int    `json:"pgid"`
	TZ           string `json:"tz"`
	RadarrKey    string `json:"radarrKey"`
	SonarrKey    string `json:"sonarrKey"`
	ProwlarrKey  string `json:"prowlarrKey"`
	QbitUser     string `json:"qbitUser"`
	QbitPass     string `json:"qbitPass"`
	// TorrentPort is the stack's qBittorrent's port for peers (0: 6881,
	// from before it was chosen).
	TorrentPort int `json:"torrentPort,omitempty"`
	// OwnQbit: the owner's own qBittorrent (saved as Kinara's qBittorrent
	// connection) instead of one in the stack.
	OwnQbit bool `json:"ownQbit,omitempty"`
	// CreatedAt is when its script was made.
	CreatedAt int64 `json:"createdAt"`
}

// ArrSettings are the Radarr (movies) and Sonarr (shows) connections,
// and Prowlarr and qBittorrent, which Kinara can wire to them.
type ArrSettings struct {
	Radarr   ArrServer `json:"radarr"`
	Sonarr   ArrServer `json:"sonarr"`
	Prowlarr ArrLink   `json:"prowlarr"`
	Qbit     QbitLink  `json:"qbit"`
	Stack    *ArrStack `json:"stack,omitempty"`
}

// DefaultArrSettings apply until the owner sets one up.
var DefaultArrSettings = ArrSettings{
	Radarr: ArrServer{Search: true, MinimumAvailability: "released"},
	Sonarr: ArrServer{Search: true, SeasonFolder: true, SeriesType: "standard"},
}

const arrSettingsKey = "arr_settings"

// GetArrSettings reads the Radarr and Sonarr connections.
func (s *Store) GetArrSettings(ctx context.Context) (ArrSettings, error) {
	out := DefaultArrSettings
	v, ok, err := s.setting(ctx, arrSettingsKey)
	if err != nil || !ok || v == "" {
		return out, err
	}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return DefaultArrSettings, nil
	}
	return out, nil
}

// PutArrSettings saves the Radarr and Sonarr connections.
func (s *Store) PutArrSettings(ctx context.Context, a ArrSettings) error {
	b, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.setSetting(ctx, arrSettingsKey, string(b))
}
