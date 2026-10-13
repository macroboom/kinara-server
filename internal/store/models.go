package store

// Library kinds.
const (
	KindMovies = "movies"
	KindTV     = "tv"
)

// Item kinds.
const (
	ItemMovie   = "movie"
	ItemShow    = "show"
	ItemSeason  = "season"
	ItemEpisode = "episode"
)

// Match states.
const (
	MatchPending   = "pending"
	MatchMatched   = "matched"
	MatchUnmatched = "unmatched"
	MatchManual    = "manual"
)

// External ID providers.
const (
	ProviderTMDBMovie   = "tmdb_movie"
	ProviderTMDBTV      = "tmdb_tv"
	ProviderTMDBEpisode = "tmdb_episode"
	ProviderIMDB        = "imdb"
)

type Library struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	CreatedAt  int64  `json:"createdAt"`
	LastScanAt int64  `json:"lastScanAt"`
}

type LibraryStats struct {
	Movies      int `json:"movies"`
	Shows       int `json:"shows"`
	Episodes    int `json:"episodes"`
	Unmatched   int `json:"unmatched"`
	Pending     int `json:"pending"`
	Files       int `json:"files"`
	Missing     int `json:"missing"`
	ProbeErrors int `json:"probeErrors"`
	Unplaced    int `json:"unplaced"`
}

// Item is a movie, show, season, or episode. SeasonNumber and EpisodeNumber
// are -1 when not set (season 0 is Specials).
type Item struct {
	ID              string  `json:"id"`
	LibraryID       int64   `json:"libraryId"`
	Kind            string  `json:"kind"`
	ParentID        string  `json:"parentId,omitempty"`
	Title           string  `json:"title"`
	SortTitle       string  `json:"sortTitle"`
	Year            int     `json:"year,omitempty"`
	SeasonNumber    int     `json:"seasonNumber"`
	EpisodeNumber   int     `json:"episodeNumber"`
	Overview        string  `json:"overview"`
	Rating          float64 `json:"rating,omitempty"`
	RuntimeMin      int     `json:"runtimeMin,omitempty"`
	AirDate         string  `json:"airDate,omitempty"`
	MatchState      string  `json:"matchState"`
	MatchConfidence float64 `json:"matchConfidence"`
	ParsedTitle     string  `json:"-"`
	ParsedYear      int     `json:"-"`
	AddedAt         int64   `json:"addedAt"`
	UpdatedAt       int64   `json:"updatedAt"`
}

// Version is one playable file for an item.
type Version struct {
	ID          string  `json:"id"`
	ItemID      string  `json:"itemId"`
	LibraryID   int64   `json:"libraryId"`
	Path        string  `json:"-"`
	RelPath     string  `json:"relPath"`
	Part        int     `json:"part"`
	EpisodeEnd  int     `json:"episodeEnd,omitempty"`
	Size        int64   `json:"size"`
	Mtime       int64   `json:"mtime"`
	Container   string  `json:"container"`
	DurationSec float64 `json:"durationSec"`
	Bitrate     int64   `json:"bitrate"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	VideoCodec  string  `json:"videoCodec"`
	HDR         string  `json:"hdr"`
	DVProfile   int     `json:"dvProfile,omitempty"`
	ProbeError  string  `json:"probeError,omitempty"`
	Missing     bool    `json:"missing"`
	AddedAt     int64   `json:"addedAt"`
	ScannedAt   int64   `json:"scannedAt"`
}

// Stream is a video, audio, or subtitle track inside a version.
type Stream struct {
	Index    int    `json:"index"`
	Type     string `json:"type"`
	Codec    string `json:"codec"`
	Profile  string `json:"profile,omitempty"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Channels int    `json:"channels,omitempty"`
	Default  bool   `json:"default"`
	Forced   bool   `json:"forced"`
}

type User struct {
	ID           int64  `json:"id"`
	Username     string `json:"username"`
	PasswordHash string `json:"-"`
	IsAdmin      bool   `json:"isAdmin"`
	CreatedAt    int64  `json:"createdAt"`
	// ProfileID is who's watching on this session (the account's first
	// profile unless the device picked another). Watch state belongs to it.
	ProfileID int64 `json:"profileId"`
}

// Profile is one viewer under an account.
type Profile struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"-"`
	Name         string `json:"name"`
	Color        string `json:"color"`
	PinHash      string `json:"-"`
	HasPin       bool   `json:"hasPin"`
	Kids         bool   `json:"kids"`
	AudioLang    string `json:"audioLang"`
	SubtitleLang string `json:"subtitleLang"`
	// SubtitleMode is off, forced (only forced tracks), or always.
	SubtitleMode string `json:"subtitleMode"`
	CreatedAt    int64  `json:"createdAt"`
}

// TrackPrefs are a profile's audio and subtitle choices for one show or
// movie. Empty fields mean "use the profile's default".
type TrackPrefs struct {
	AudioLang    string `json:"audioLang"`
	SubtitleLang string `json:"subtitleLang"`
	SubtitleMode string `json:"subtitleMode"`
}

// Credit is one cast member.
type Credit struct {
	Order    int    `json:"order"`
	Name     string `json:"name"`
	Role     string `json:"role"`
	PhotoURL string `json:"-"`
}

// LetterOffset is where titles starting with Letter begin in an A to Z list.
type LetterOffset struct {
	Letter string `json:"letter"`
	Offset int    `json:"offset"`
}

type WatchState struct {
	ItemID       string  `json:"itemId"`
	PositionSec  float64 `json:"positionSec"`
	Completed    bool    `json:"completed"`
	PlayCount    int     `json:"playCount"`
	LastPlayedAt int64   `json:"lastPlayedAt"`
	ClientTime   int64   `json:"-"`
}

// EpisodeRef is the minimum needed to work out the next episode.
type EpisodeRef struct {
	ID      string
	Season  int
	Episode int
}

// ItemFilter selects items for browsing.
type ItemFilter struct {
	LibraryID  int64
	Kind       string
	ParentID   string
	MatchState string
	Sort       string // title (default), added, year, length, rating
	Limit      int
	Offset     int
	// IncludeMissing includes items whose files are all missing.
	IncludeMissing bool

	// Filters for library browsing. Watched needs ProfileID.
	ProfileID int64
	// Watched is unwatched, started (begun, not finished) or watched. For a
	// show: unwatched = nothing seen, started = some seen, watched = all.
	Watched string
	// MaxRuntime keeps titles of at most this many minutes (0 = any).
	MaxRuntime int
	// BestQuality keeps titles with a 4K or HDR file.
	BestQuality bool
	// Search keeps titles containing these words (any order, any case).
	Search string
	// Genre keeps titles with this TMDB genre ("Comedy").
	Genre string
}
