package api

import (
	"context"
	"crypto/rand"
	"errors"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/images"
	"kinara/internal/pgs"
	"kinara/internal/store"
	"kinara/internal/tmdb"
	"kinara/internal/transcode"
)

// --- Converting streams ---

// transcodeStream converts a file on the fly for players that can't play it
// as is. See package transcode. Query parameters:
//
//	start       seconds to begin at
//	video       copy | h264
//	maxBitrate  kbit/s for re-encoded video
//	maxHeight   pixels for re-encoded video
//	audio       source stream index of the audio track
//	audioCodec  copy | aac
func (s *Server) transcodeStream(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Transcoder == nil {
		writeErr(w, http.StatusNotImplemented, "converting is turned off on this server")
		return
	}
	req, v, status, err := s.transcodeRequest(r)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	if s.Jobs != nil {
		s.Jobs.Prioritize(v.ID)
	}
	req.Owner = s.streamOwner(r, v)
	if req.Container == "mp4" {
		w.Header().Set("Content-Type", "video/mp4")
	} else {
		w.Header().Set("Content-Type", "video/x-matroska")
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "none")
	// Headers go out with the first bytes ffmpeg writes, so an ffmpeg that
	// fails at once can still send an error.
	fw := &firstWrite{w: w}
	err = s.Transcoder.Stream(r.Context(), fw, req)
	if err != nil && !errors.Is(err, transcode.ErrBusy) {
		s.noteConversionFailed(r, u, v, err, "stream")
	}
	switch {
	case err == nil:
	case !fw.started:
		status := http.StatusInternalServerError
		if errors.Is(err, transcode.ErrBusy) {
			status = http.StatusServiceUnavailable
		}
		writeErr(w, status, err.Error())
	default:
		// ffmpeg failed partway. Ending the response normally would look
		// like the end of the video to the player (which would then mark it
		// watched); cut the connection so the player sees an error instead.
		panic(http.ErrAbortHandler)
	}
}

// transcodeStart says where a converted stream that keeps the original
// video can really start: the keyframe near ?at= (the next one with
// ?dir=forward, else the one before). The app asks for the stream at
// "request" and counts its clock from "start", so the position it shows
// matches the picture.
func (s *Server) transcodeStart(w http.ResponseWriter, r *http.Request, u store.User) {
	v, _, err := s.playableVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	at, _ := strconv.ParseFloat(r.URL.Query().Get("at"), 64)
	if math.IsNaN(at) || math.IsInf(at, 0) || at < 0 {
		at = 0
	}
	start, err := transcode.FindStart(r.Context(), s.FFprobe, v.Path, at, r.URL.Query().Get("dir") == "forward")
	if err != nil {
		s.Log.Debug("keyframe lookup", "file", v.RelPath, "err", err)
	}
	writeJSON(w, http.StatusOK, start)
}

// firstWrite remembers whether any bytes were sent.
type firstWrite struct {
	w       http.ResponseWriter
	started bool
}

func (f *firstWrite) Write(p []byte) (int, error) {
	if !f.started {
		f.started = true
		f.w.WriteHeader(http.StatusOK)
	}
	n, err := f.w.Write(p)
	_ = http.NewResponseController(f.w).Flush()
	return n, err
}

// speedTest sends some bytes so the app can measure the connection and
// pick a version (or a conversion) that fits it.
func (s *Server) speedTest(w http.ResponseWriter, r *http.Request, u store.User) {
	n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
	n = min(max(n, 64<<10), 32<<20)
	block := make([]byte, 64<<10)
	_, _ = rand.Read(block)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(n))
	for sent := 0; sent < n; {
		chunk := block[:min(len(block), n-sent)]
		if _, err := w.Write(chunk); err != nil {
			return
		}
		sent += len(chunk)
	}
}

// --- Preview thumbnails ---

type trickplayView struct {
	Interval int `json:"interval"`
	Width    int `json:"width"`
	Height   int `json:"height"`
	Cols     int `json:"cols"`
	Rows     int `json:"rows"`
	Count    int `json:"count"`
	Sheets   int `json:"sheets"`
	// SheetURL has "{n}" where the sheet number (from 1) goes.
	SheetURL string `json:"sheetUrl"`
}

func (s *Server) trickplayManifest(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Trickplay == nil {
		writeErr(w, http.StatusNotFound, "previews are turned off")
		return
	}
	// An optimized copy shares its original's previews.
	v, err := s.sourceVersion(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	man, ok := s.Trickplay.Ready(v)
	if !ok {
		if s.Jobs != nil && s.Trickplay.Needed(v) {
			s.Jobs.Prioritize(v.ID)
		}
		writeErr(w, http.StatusNotFound, "previews aren't ready yet")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	writeJSON(w, http.StatusOK, trickplayView{
		Interval: man.Interval, Width: man.Width, Height: man.Height, Cols: man.Cols, Rows: man.Rows,
		Count: man.Count, Sheets: man.Sheets,
		SheetURL: "/api/v1/trickplay/" + v.ID + "/{n}.jpg",
	})
}

func (s *Server) trickplaySheet(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Trickplay == nil {
		writeErr(w, http.StatusNotFound, "previews are turned off")
		return
	}
	// An optimized copy shares its original's previews.
	v, err := s.sourceVersion(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	name := strings.TrimSuffix(r.PathValue("sheet"), ".jpg")
	n, err := strconv.Atoi(name)
	if err != nil || n < 1 {
		writeErr(w, http.StatusNotFound, "no such sheet")
		return
	}
	man, ok := s.Trickplay.Ready(v)
	if !ok || n > man.Sheets {
		writeErr(w, http.StatusNotFound, "no such sheet")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, s.Trickplay.Sheet(v.ID, n))
}

// --- Genres ---

// genreRow is one "Comedies" style row on the home screen.
type genreRow struct {
	Name  string     `json:"name"`
	Items []ItemView `json:"items"`
}

// genreRows picks the library's biggest genres and fills each with its
// best rated titles, shuffled a little differently each day so the rows
// don't always look the same.
func (s *Server) genreRows(ctx context.Context, profileID int64, rows, perRow int) ([]genreRow, error) {
	counts, err := s.Store.GenreCounts(ctx, "")
	if err != nil {
		return nil, err
	}
	day := time.Now().YearDay() + time.Now().Year()*1000
	out := []genreRow{}
	for _, g := range counts {
		if len(out) == rows {
			break
		}
		if g.Count < 4 {
			continue
		}
		items, err := s.Store.ListItems(ctx, store.ItemFilter{Genre: g.Name, Sort: "rating", Limit: perRow * 2})
		if err != nil {
			return nil, err
		}
		dailyShuffle(items, g.Name, day)
		if len(items) > perRow {
			items = items[:perRow]
		}
		views, err := s.views(ctx, profileID, items)
		if err != nil {
			return nil, err
		}
		out = append(out, genreRow{Name: g.Name, Items: views})
	}
	return out, nil
}

// dailyShuffle mixes the list with a seed that changes daily, keeping
// better rated titles more likely near the front.
func dailyShuffle(items []store.Item, salt string, day int) {
	weight := make(map[string]float64, len(items))
	for i, it := range items {
		h := fnv.New32a()
		h.Write([]byte(salt + it.ID + strconv.Itoa(day)))
		noise := float64(h.Sum32()%1000) / 1000
		weight[it.ID] = float64(i) + noise*float64(len(items))*0.6
	}
	sort.SliceStable(items, func(a, b int) bool { return weight[items[a].ID] < weight[items[b].ID] })
}

func (s *Server) listGenres(w http.ResponseWriter, r *http.Request, u store.User) {
	counts, err := s.Store.GenreCounts(r.Context(), r.URL.Query().Get("kind"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"genres": counts})
}

// --- Trending ---

// trendingView is a title from TMDB's trending list, with the library item
// when it's in the library.
type trendingView struct {
	TMDBID      int       `json:"tmdbId"`
	Kind        string    `json:"kind"` // movie or show
	Title       string    `json:"title"`
	Year        int       `json:"year,omitempty"`
	Overview    string    `json:"overview"`
	Rating      float64   `json:"rating,omitempty"`
	Genres      []string  `json:"genres"`
	PosterURL   string    `json:"posterUrl,omitempty"`
	BackdropURL string    `json:"backdropUrl,omitempty"`
	Item        *ItemView `json:"item,omitempty"`
	// Request is an open request for the title (requests.go).
	Request *requestMarker `json:"request,omitempty"`
	// OnTheWay is the title's status from the owner's tools while it's on its
	// way (integrations_status.go).
	OnTheWay *titleStatusView `json:"onTheWay,omitempty"`
	// Rank is the place in a trending list (left out on discover lists).
	Rank int `json:"rank,omitempty"`
}

type trendingCache struct {
	mu     sync.Mutex
	at     time.Time
	movies []tmdb.TrendingResult
	shows  []tmdb.TrendingResult
	today  []tmdb.TrendingResult
	genres map[int]string
}

const trendingTTL = time.Hour

// trendingLists fetches (or reuses) TMDB's trending lists.
func (s *Server) trendingLists(ctx context.Context) (movies, shows, today []tmdb.TrendingResult, genres map[int]string, at time.Time, err error) {
	c := &s.trending
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < trendingTTL && c.movies != nil {
		return c.movies, c.shows, c.today, c.genres, c.at, nil
	}
	get := func(media, window string, pages int) ([]tmdb.TrendingResult, error) {
		var all []tmdb.TrendingResult
		for p := 1; p <= pages; p++ {
			res, err := s.TMDB.Trending(ctx, media, window, p)
			if err != nil {
				return all, err
			}
			all = append(all, res...)
		}
		return all, nil
	}
	if movies, err = get("movie", "week", 2); err != nil {
		return
	}
	if shows, err = get("tv", "week", 2); err != nil {
		return
	}
	if today, err = get("all", "day", 1); err != nil {
		return
	}
	genres = map[int]string{}
	for _, media := range []string{"movie", "tv"} {
		g, gerr := s.TMDB.GenreList(ctx, media)
		if gerr == nil {
			for id, name := range g {
				genres[id] = name
			}
		}
	}
	c.at, c.movies, c.shows, c.today, c.genres = time.Now(), movies, shows, today, genres
	at = c.at
	return
}

var tmdbImageFile = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}\.(jpg|png)$`)

func tmdbImageURL(path string) string {
	file := strings.TrimPrefix(path, "/")
	if !tmdbImageFile.MatchString(file) {
		return ""
	}
	return "/api/v1/tmdb-image/" + file
}

func (s *Server) trendingHandler(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.TMDB == nil {
		writeErr(w, http.StatusNotImplemented, "set a TMDB key on the server to see what's trending")
		return
	}
	movies, shows, today, genres, updated, err := s.trendingLists(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "couldn't reach TMDB: "+err.Error())
		return
	}
	// Which trending titles are in the library.
	ids := map[string][]string{}
	for _, list := range [][]tmdb.TrendingResult{movies, shows, today} {
		for _, t := range list {
			p := store.ProviderTMDBMovie
			if t.MediaType == "tv" {
				p = store.ProviderTMDBTV
			}
			ids[p] = append(ids[p], strconv.Itoa(t.ID))
		}
	}
	owned := map[string]map[string]store.Item{}
	var ownedItems []store.Item
	for p, vals := range ids {
		m, err := s.Store.ItemsByExternalIDs(r.Context(), p, vals)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		owned[p] = m
		for _, it := range m {
			ownedItems = append(ownedItems, it)
		}
	}
	views, err := s.views(r.Context(), u.ProfileID, ownedItems)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	viewByID := map[string]*ItemView{}
	for i := range views {
		viewByID[views[i].ID] = &views[i]
	}

	convert := func(list []tmdb.TrendingResult) []trendingView {
		out := make([]trendingView, 0, len(list))
		seen := map[string]bool{}
		for _, t := range list {
			if t.MediaType != "movie" && t.MediaType != "tv" {
				continue // people
			}
			key := t.MediaType + strconv.Itoa(t.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			v := trendingView{TMDBID: t.ID, Overview: t.Overview, Rating: math.Round(t.VoteAverage*10) / 10,
				PosterURL: tmdbImageURL(t.PosterPath), BackdropURL: tmdbImageURL(t.BackdropPath),
				Genres: []string{}, Rank: len(out) + 1}
			p := store.ProviderTMDBMovie
			if t.MediaType == "movie" {
				v.Kind, v.Title, v.Year = "movie", t.Title, tmdb.YearOf(t.ReleaseDate)
			} else {
				v.Kind, v.Title, v.Year = "show", t.Name, tmdb.YearOf(t.FirstAirDate)
				p = store.ProviderTMDBTV
			}
			for _, g := range t.GenreIDs {
				if name := genres[g]; name != "" {
					v.Genres = append(v.Genres, name)
				}
			}
			if it, ok := owned[p][strconv.Itoa(t.ID)]; ok {
				v.Item = viewByID[it.ID]
			}
			out = append(out, v)
		}
		return out
	}
	tm, ts, tt := convert(movies), convert(shows), convert(today)
	s.markRequests(r.Context(), u, tm, ts, tt)
	// "In your library": owned titles in order of how much they're trending.
	inLib := []trendingView{}
	seen := map[string]bool{}
	for _, list := range [][]trendingView{tt, tm, ts} {
		for _, v := range list {
			if v.Item != nil && !seen[v.Item.ID] {
				seen[v.Item.ID] = true
				inLib = append(inLib, v)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"today":     tt,
		"movies":    tm,
		"shows":     ts,
		"inLibrary": inLib,
		"updatedAt": updated.Unix(),
	})
}

// tmdbImage serves artwork for titles that aren't in the library (on the
// Trending page) through the same resizing cache as library artwork.
func (s *Server) tmdbImage(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.TMDB == nil {
		writeErr(w, http.StatusNotFound, "no TMDB key")
		return
	}
	file := r.PathValue("file")
	if !tmdbImageFile.MatchString(file) {
		writeErr(w, http.StatusBadRequest, "bad image name")
		return
	}
	width, _ := strconv.Atoi(r.URL.Query().Get("w"))
	path, err := s.Images.Get(r.Context(), s.TMDB.Image("/"+file), images.SnapWidth(width))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "image unavailable")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, path)
}

// transcodeRequest builds a conversion from a request's query: the file
// (path {id}, a version), the video and audio choices, and where to start.
// On failure it returns the HTTP status to answer with.
func (s *Server) transcodeRequest(r *http.Request) (transcode.Request, store.Version, int, error) {
	// An optimized copy's ID converts from the copy (optimize.go).
	v, streams, err := s.playableVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing {
		return transcode.Request{}, v, http.StatusNotFound, errors.New("file not found")
	}
	s.autoSubsPlaying(s.sourceVersionID(r.Context(), r.PathValue("id")))
	if _, err := os.Stat(v.Path); err != nil {
		return transcode.Request{}, v, http.StatusNotFound, errors.New("file not available")
	}
	q := r.URL.Query()
	req := transcode.Request{
		Path:        v.Path,
		Video:       q.Get("video"),
		AudioCodec:  q.Get("audioCodec"),
		HDR:         v.HDR,
		SourceCodec: v.VideoCodec,
		Container:   q.Get("container"),
		Audio:       -1,
	}
	req.Start, _ = strconv.ParseFloat(q.Get("start"), 64)
	if math.IsNaN(req.Start) || math.IsInf(req.Start, 0) {
		req.Start = 0
	}
	req.MaxBitrate, _ = strconv.Atoi(q.Get("maxBitrate"))
	req.MaxHeight, _ = strconv.Atoi(q.Get("maxHeight"))
	req.Segments = q.Get("segments")
	// "1": the player shows Dolby Vision; "0": it doesn't (see transcode/dovi.go).
	if d := q.Get("dovi"); d == "1" || d == "0" {
		req.ClientDV = d
	}
	req.MaxAudioChannels, _ = strconv.Atoi(q.Get("audioChannels"))
	if a := q.Get("audio"); a != "" {
		idx, err := strconv.Atoi(a)
		if err != nil {
			return req, v, http.StatusBadRequest, errors.New("bad audio index")
		}
		found := false
		for _, st := range streams {
			if st.Index == idx && st.Type == "audio" {
				req.Audio, req.AudioChannels, found = idx, st.Channels, true
			}
		}
		if !found {
			return req, v, http.StatusBadRequest, errors.New("no audio track with that index")
		}
	} else {
		for _, st := range streams {
			if st.Type == "audio" {
				req.AudioChannels = st.Channels
				break
			}
		}
	}
	for _, st := range streams {
		if st.Type == "subtitle" {
			req.Subtitles = append(req.Subtitles, st)
		}
	}
	for _, st := range streams {
		if st.Type == "video" {
			req.SourceProfile = st.Profile // the first one is what's converted
			break
		}
	}
	if req.Container != "hls" {
		if err := req.Validate(); err != nil {
			return req, v, http.StatusBadRequest, err
		}
	}
	return req, v, 0, nil
}

// hlsStart begins an HLS conversion of a file (for iPhones and Safari) and
// answers with its playlist URL once the first segment is ready.
func (s *Server) hlsStart(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.HLS == nil {
		writeErr(w, http.StatusNotImplemented, "converting is turned off on this server")
		return
	}
	req, v, status, err := s.transcodeRequest(r)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	if s.Jobs != nil {
		s.Jobs.Prioritize(v.ID)
	}
	req.Owner = s.streamOwner(r, v)
	// HLS.Start tries the slower ways itself when the graphics card fails.
	id, err := s.HLS.Start(r.Context(), req)
	if err != nil && !errors.Is(err, transcode.ErrBusy) {
		s.noteConversionFailed(r, u, v, err, "hls")
	}
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, transcode.ErrBusy) {
			status = http.StatusServiceUnavailable
		}
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session":  id,
		"playlist": "/api/v1/hls/session/" + id + "/index.m3u8",
		"start":    req.Start,
		// Where a Chromecast on the home network can fetch it (see cast.go).
		"lan": s.lanBases(),
	})
}

// hlsFile serves a session's playlist and segments. The session ID is the
// key: players fetch segments without the token.
func (s *Server) hlsFile(w http.ResponseWriter, r *http.Request) {
	if s.HLS == nil {
		http.NotFound(w, r)
		return
	}
	path, playlist, err := s.HLS.File(r.PathValue("session"), r.PathValue("file"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if playlist != nil {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Write(playlist)
		return
	}
	// Explicit types: Windows can map .ts to something else entirely.
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
	case ".m4s", ".mp4":
		w.Header().Set("Content-Type", "video/mp4")
	}
	http.ServeFile(w, r, path)
}

func (s *Server) hlsStop(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.HLS != nil {
		s.HLS.Stop(r.PathValue("session"))
	}
	w.WriteHeader(http.StatusNoContent)
}

// subtitleVTT answers a text subtitle track as WebVTT, for web browsers and
// phone players (the TV reads subtitles from the file itself).
func (s *Server) subtitleVTT(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Subtitles == nil {
		writeErr(w, http.StatusNotImplemented, "subtitle conversion needs ffmpeg on the server")
		return
	}
	// An optimized copy's subtitles are its original's.
	v, err := s.sourceVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	index, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("index"), ".vtt"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad track")
		return
	}
	if index >= ExternalSubtitleBase {
		s.serveExternalVTT(w, r, v, index)
		return
	}
	streams, err := s.Store.Streams(r.Context(), v.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	codec := ""
	for _, st := range streams {
		if st.Index == index && st.Type == "subtitle" {
			codec = st.Codec
		}
	}
	if codec == "" {
		writeErr(w, http.StatusNotFound, "no subtitle track with that index")
		return
	}
	vtt, err := s.Subtitles.VTT(r.Context(), v.ID, v.Path, v.Mtime, index, codec)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, transcode.ErrNotText) {
			status = http.StatusUnprocessableEntity
		}
		writeErr(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(vtt)
}

// subtitleTrack finds a version and one of its subtitle tracks from the
// request path, writing the error itself when it can't.
func (s *Server) subtitleTrack(w http.ResponseWriter, r *http.Request) (store.Version, store.Stream, bool) {
	v, err := s.sourceVersion(r.Context(), r.PathValue("id"))
	if err != nil || v.Missing {
		writeErr(w, http.StatusNotFound, "file not found")
		return v, store.Stream{}, false
	}
	index, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad track")
		return v, store.Stream{}, false
	}
	streams, err := s.Store.Streams(r.Context(), v.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return v, store.Stream{}, false
	}
	for _, st := range streams {
		if st.Index == index && st.Type == "subtitle" {
			return v, st, true
		}
	}
	writeErr(w, http.StatusNotFound, "no subtitle track with that index")
	return v, store.Stream{}, false
}

// subtitlePGS answers a picture subtitle track (Blu-ray PGS, DVD or DVB) as
// a timeline of PNGs for the app to draw over the video. The first request starts reading the
// track; until it's all read, done is false and the app asks again with
// ?from= set to the events it already has.
func (s *Server) subtitlePGS(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.PGS == nil {
		writeErr(w, http.StatusNotImplemented, "picture subtitles need ffmpeg on the server")
		return
	}
	v, st, ok := s.subtitleTrack(w, r)
	if !ok {
		return
	}
	if !pgs.Codecs[st.Codec] {
		writeErr(w, http.StatusUnprocessableEntity, "that track isn't picture subtitles (Blu-ray, DVD or DVB)")
		return
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	tl, err := s.PGS.Timeline(v.ID, v.Path, v.Mtime, st.Index, st.Codec, from)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tl.Done {
		w.Header().Set("Cache-Control", "private, max-age=3600")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	writeJSON(w, http.StatusOK, tl)
}

// subtitlePGSImage answers one picture of a PGS track.
func (s *Server) subtitlePGSImage(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.PGS == nil {
		writeErr(w, http.StatusNotImplemented, "picture subtitles need ffmpeg on the server")
		return
	}
	v, st, ok := s.subtitleTrack(w, r)
	if !ok {
		return
	}
	n, err := strconv.Atoi(strings.TrimSuffix(r.PathValue("n"), ".png"))
	if err != nil || n < 0 {
		writeErr(w, http.StatusBadRequest, "bad picture")
		return
	}
	f, err := os.Open(s.PGS.ImagePath(v.ID, v.Mtime, st.Index, n))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such picture (yet)")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/png")
	// A picture never changes: a changed file gets a new cache folder.
	w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
	_, _ = io.Copy(w, f)
}
