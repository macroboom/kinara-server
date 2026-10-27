package api

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kinara/internal/metrics"
	"kinara/internal/store"
	"kinara/internal/transcode"
)

// Downloads: apps save movies and episodes to watch without a connection.
//
// An app asks for a title (a movie, an episode, or a whole season or show)
// at a quality. "Original" is the file as it is, ready at once. The other
// qualities are prepared first: the server converts the file in the
// background into an MP4 a phone plays (H.264, stereo AAC), one at a time,
// while a conversion slot stays free for anyone watching. Two people
// downloading the same file at the same quality share one prepared copy.
// The app follows progress on GET /downloads, then fetches the file with
// byte ranges (so it can pick up where it stopped) and says when it has
// all of it. Subtitles come separately, as WebVTT (GET /downloads/{id}).
//
// Prepared copies take room on the server, so they're removed a few days
// after every device that wanted one has it, after 30 days unused, when
// nobody wants them any more, and oldest first when the owner's size limit
// needs room. A removed copy is prepared again if it's asked for.
// Watch progress made offline goes back with PUT /progress and clientTime.

const (
	// keepAfterSaved is how long a prepared copy stays after the last
	// device that wanted it has all of it (for a re-download).
	keepAfterSaved = 3 * 24 * time.Hour
	// keepUnused removes copies nobody has fetched from in this long.
	keepUnused = 30 * 24 * time.Hour
	// maxPerRequest caps the episodes one request adds (a long show).
	maxPerRequest = 300
	// diskReserve is room always left free on the server's drive.
	diskReserve = 2 << 30
)

type downloadsState struct {
	mu      sync.Mutex
	kick    chan struct{}
	running map[int64]*runningDownload
	// cleanMu runs one tidy-up at a time.
	cleanMu sync.Mutex
}

// runningDownload is a copy being prepared now.
type runningDownload struct {
	cancel   context.CancelFunc
	progress float64
	saved    time.Time
}

func (s *Server) downloadRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/items/{id}/download", s.user(s.downloadOptions))
	mux.Handle("GET /api/v1/downloads", s.user(s.listDownloads))
	mux.Handle("POST /api/v1/downloads", s.user(s.createDownloads))
	mux.Handle("GET /api/v1/downloads/all", s.admin(s.allDownloads))
	mux.Handle("PUT /api/v1/downloads/settings", s.admin(s.putDownloadSettings))
	mux.Handle("GET /api/v1/downloads/{id}", s.user(s.getDownload))
	// Background downloaders can't always send a header, so this takes ?token=.
	mux.Handle("GET /api/v1/downloads/{id}/file", s.userQuery(s.downloadFile))
	mux.Handle("POST /api/v1/downloads/{id}/saved", s.user(s.downloadSaved))
	mux.Handle("DELETE /api/v1/downloads/{id}", s.user(s.deleteDownload))
}

// canPrepare says whether this server can make the smaller qualities.
func (s *Server) canPrepare() bool { return s.Transcoder != nil && s.DownloadDir != "" }

func (s *Server) kickDownloads() {
	s.dl.mu.Lock()
	if s.dl.kick == nil {
		s.dl.kick = make(chan struct{}, 1)
	}
	k := s.dl.kick
	s.dl.mu.Unlock()
	select {
	case k <- struct{}{}:
	default:
	}
}

// --- Views ---

type downloadView struct {
	ID        int64  `json:"id"`
	ItemID    string `json:"itemId"`
	VersionID string `json:"versionId"`
	Quality   string `json:"quality"`
	// State is queued, preparing, ready, failed or expired (see the store).
	State string `json:"state"`
	// Progress is the share prepared, 0 to 1.
	Progress float64 `json:"progress"`
	// Place is how many copies are ahead of this one in line (queued only).
	Place int `json:"place,omitempty"`
	// Size is the file's size once ready; Estimate the expected size.
	Size     int64  `json:"size"`
	Estimate int64  `json:"estimate"`
	Error    string `json:"error,omitempty"`
	// FileURL fetches the file (with byte ranges) once it's ready.
	FileURL    string `json:"fileUrl,omitempty"`
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	// ThisDevice: made on the device asking.
	ThisDevice bool   `json:"thisDevice"`
	Username   string `json:"username,omitempty"`
	CreatedAt  int64  `json:"createdAt"`
	ReadyAt    int64  `json:"readyAt,omitempty"`
	// SavedAt is when the device said it has all of it.
	SavedAt int64    `json:"savedAt,omitempty"`
	Item    ItemView `json:"item"`
	// ShowTitle names an episode's show.
	ShowTitle string `json:"showTitle,omitempty"`
	ShowID    string `json:"showId,omitempty"`
}

// downloadDetail is one download with what the app keeps for playing it
// offline: the file's details, markers and subtitles.
type downloadDetail struct {
	downloadView
	Version VersionView `json:"version"`
	// Audio is the audio track kept (the original keeps them all).
	Audio *store.Stream `json:"audio,omitempty"`
	// Subtitles are the text subtitles the app can save as WebVTT: the
	// file's own tracks (by stream index) and files beside it. Picture
	// subtitles (Blu-ray, DVD) don't come along.
	Subtitles []ExternalSubtitle `json:"subtitles"`
}

func (s *Server) downloadViews(ctx context.Context, u store.User, device string, list []store.Download) []downloadView {
	ids := make([]string, 0, len(list))
	byID := map[string]store.Item{}
	for _, d := range list {
		if _, ok := byID[d.ItemID]; ok {
			continue
		}
		if it, err := s.Store.GetItem(ctx, d.ItemID); err == nil {
			byID[d.ItemID] = it
			ids = append(ids, d.ItemID)
		}
	}
	items := make([]store.Item, 0, len(ids))
	for _, id := range ids {
		items = append(items, byID[id])
	}
	views, _ := s.views(ctx, u.ProfileID, items)
	viewByID := map[string]ItemView{}
	for _, v := range views {
		viewByID[v.ID] = v
	}
	shows := map[string]store.Item{}
	places, _ := s.Store.QueuedBefore(ctx)
	out := make([]downloadView, 0, len(list))
	for _, d := range list {
		v := s.downloadViewOf(d, device, places)
		v.Item = viewByID[d.ItemID]
		if it, ok := byID[d.ItemID]; ok && it.Kind == store.ItemEpisode {
			showID, err := s.Store.ShowOfEpisode(ctx, it.ID)
			if err == nil {
				show, ok := shows[showID]
				if !ok {
					show, _ = s.Store.GetItem(ctx, showID)
					shows[showID] = show
				}
				v.ShowTitle, v.ShowID = show.Title, show.ID
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) downloadViewOf(d store.Download, device string, places map[int64]int) downloadView {
	f := d.File
	v := downloadView{ID: d.ID, ItemID: d.ItemID, VersionID: f.VersionID, Quality: f.Quality, State: f.State,
		Progress: f.Progress, Size: f.Size, Estimate: f.Estimate, Error: f.Error, DeviceID: d.Device,
		DeviceName: d.DeviceName, ThisDevice: d.Device == device, Username: d.Username, CreatedAt: d.CreatedAt,
		ReadyAt: f.ReadyAt, SavedAt: d.SavedAt}
	switch f.State {
	case store.DownloadReady:
		v.FileURL = "/api/v1/downloads/" + strconv.FormatInt(d.ID, 10) + "/file"
		v.Progress = 1
	case store.DownloadPreparing:
		s.dl.mu.Lock()
		if run := s.dl.running[f.ID]; run != nil {
			v.Progress = run.progress
		}
		s.dl.mu.Unlock()
	case store.DownloadQueued:
		v.Place = places[f.ID]
	}
	v.Progress = math.Round(v.Progress*1000) / 1000
	return v
}

// --- Requests ---

type qualityOption struct {
	transcode.DownloadPreset
	// Estimate is the expected size in bytes (for all the episodes).
	Estimate int64 `json:"estimate"`
	// Available is false when the server can't make it (no ffmpeg).
	Available bool `json:"available"`
}

// downloadOptions answers GET /items/{id}/download: whether downloads are
// on, and the qualities with their expected sizes, for a movie, an
// episode, or a season or show (all its episodes, or ?unwatched=1).
func (s *Server) downloadOptions(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	ds, _ := s.Store.GetDownloadSettings(ctx)
	it, err := s.Store.GetItem(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	targets, err := s.downloadTargets(ctx, u, it, r.URL.Query().Get("unwatched") == "1")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	opts := make([]qualityOption, 0, len(transcode.DownloadPresets))
	for _, p := range transcode.DownloadPresets {
		o := qualityOption{DownloadPreset: p, Available: p.MaxHeight == 0 || s.canPrepare()}
		for _, t := range targets {
			copyVideo := p.CanCopyVideo(t.v.VideoCodec, t.v.HDR, t.v.Height, t.v.Bitrate)
			o.Estimate += p.Estimate(t.v.DurationSec, t.v.Size, copyVideo)
		}
		opts = append(opts, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": ds.Enabled, "qualities": opts, "count": len(targets),
		// The quality apps pick unless the person chose another.
		"default": map[bool]string{true: "medium", false: "original"}[s.canPrepare()],
	})
}

type downloadTarget struct {
	it store.Item
	v  store.Version
}

// downloadTargets are the movies or episodes a download of it covers, each
// with the file to use (the first one that's there).
func (s *Server) downloadTargets(ctx context.Context, u store.User, it store.Item, unwatchedOnly bool) ([]downloadTarget, error) {
	var items []store.Item
	switch it.Kind {
	case store.ItemMovie, store.ItemEpisode:
		items = []store.Item{it}
	case store.ItemSeason:
		eps, err := s.Store.Children(ctx, it.ID)
		if err != nil {
			return nil, err
		}
		items = eps
	case store.ItemShow:
		refs, err := s.Store.ShowEpisodes(ctx, it.ID)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			if ref.Season == 0 {
				continue // specials only when asked for one by one
			}
			if ep, err := s.Store.GetItem(ctx, ref.ID); err == nil {
				items = append(items, ep)
			}
		}
	default:
		return nil, nil
	}
	if unwatchedOnly && len(items) > 0 {
		ids := make([]string, len(items))
		for i, x := range items {
			ids[i] = x.ID
		}
		states, err := s.Store.WatchStates(ctx, u.ProfileID, ids)
		if err != nil {
			return nil, err
		}
		kept := items[:0]
		for _, x := range items {
			if st, ok := states[x.ID]; ok && st.Completed {
				continue
			}
			kept = append(kept, x)
		}
		items = kept
	}
	var out []downloadTarget
	for _, x := range items {
		if x.Kind != store.ItemMovie && x.Kind != store.ItemEpisode {
			continue
		}
		versions, err := s.Store.VersionsForItem(ctx, x.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			if !v.Missing {
				out = append(out, downloadTarget{it: x, v: v})
				break
			}
		}
	}
	return out, nil
}

type downloadRequest struct {
	ItemID string `json:"itemId"`
	// VersionID picks the file (a movie or an episode only).
	VersionID string `json:"versionId"`
	// Quality is original, high, medium or low (default: medium when the
	// server can convert, else original).
	Quality string `json:"quality"`
	// Audio is the audio track's stream index (a movie or an episode only;
	// default: the profile's language, else the file's default track).
	Audio *int `json:"audio"`
	// Unwatched leaves out episodes already watched (a season or show).
	Unwatched bool `json:"unwatched"`
}

func (s *Server) createDownloads(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	var req downloadRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ds, err := s.Store.GetDownloadSettings(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ds.Enabled {
		writeErr(w, http.StatusForbidden, "downloads are turned off on this server")
		return
	}
	if req.Quality == "" {
		req.Quality = "original"
		if s.canPrepare() {
			req.Quality = "medium"
		}
	}
	preset, ok := transcode.FindPreset(req.Quality)
	if !ok {
		writeErr(w, http.StatusBadRequest, "quality must be original, high, medium or low")
		return
	}
	if preset.MaxHeight > 0 && !s.canPrepare() {
		writeErr(w, http.StatusBadRequest, "this server can't convert files (no ffmpeg), so downloads keep the original quality")
		return
	}
	it, err := s.Store.GetItem(ctx, req.ItemID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	targets, err := s.downloadTargets(ctx, u, it, req.Unwatched)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	single := it.Kind == store.ItemMovie || it.Kind == store.ItemEpisode
	if single && req.VersionID != "" {
		v, err := s.Store.GetVersion(ctx, req.VersionID)
		if err != nil || v.ItemID != it.ID || v.Missing {
			writeErr(w, http.StatusBadRequest, "that file isn't this title's")
			return
		}
		targets = []downloadTarget{{it: it, v: v}}
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "there's nothing to download here")
		return
	}
	if len(targets) > maxPerRequest {
		targets = targets[:maxPerRequest]
	}
	device := myDeviceID(r)
	if ds.PerUser > 0 {
		have, err := s.Store.UserDownloads(ctx, u.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		mine := map[string]bool{}
		for _, d := range have {
			if d.Device == device {
				mine[d.ItemID] = true
			}
		}
		adding := 0
		for _, t := range targets {
			if !mine[t.it.ID] {
				adding++
			}
		}
		if len(have)+adding > ds.PerUser {
			writeErr(w, http.StatusConflict, fmt.Sprintf("each account can have up to %d downloads on this server; remove some first", ds.PerUser))
			return
		}
	}
	profile, _ := s.Store.GetProfile(ctx, u.ProfileID)
	deviceName := s.Store.DeviceName(ctx, sessionFrom(r).tokenHash)

	var made []store.Download
	for _, t := range targets {
		streams, err := s.Store.Streams(ctx, t.v.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		audio := pickAudio(streams, profile.AudioLang)
		if single && req.Audio != nil {
			audio = -2
			for _, st := range streams {
				if st.Type == "audio" && st.Index == *req.Audio {
					audio = st.Index
				}
			}
			if audio == -2 {
				writeErr(w, http.StatusBadRequest, "no audio track with that index")
				return
			}
		}
		d := store.Download{UserID: u.ID, ProfileID: u.ProfileID, Device: device, DeviceName: deviceName, ItemID: t.it.ID}
		// A tidy-up can remove a copy nobody wants yet in the moment
		// between making it and adding the download: then make it again.
		for try := 0; ; try++ {
			f, err := s.ensureCopy(ctx, t.v, streams, preset, audio)
			if err == nil {
				d.FileID = f.ID
				err = s.Store.PutDownload(ctx, &d)
			}
			if err == nil {
				break
			}
			if try == 2 {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
		if full, err := s.Store.GetDownload(ctx, d.ID); err == nil {
			made = append(made, full)
		}
	}
	s.kickDownloads()
	// Another quality for a title already downloaded leaves its old copy
	// unused: clean up now rather than on the hour.
	go s.cleanDownloads(s.BaseCtx)
	writeJSON(w, http.StatusCreated, map[string]any{"downloads": s.downloadViews(ctx, u, device, made)})
}

// pickAudio is the audio track a download keeps: the profile's language,
// else the file's default, else the first (-1 when the file has none).
func pickAudio(streams []store.Stream, lang string) int {
	first, def, match := -1, -1, -1
	for _, st := range streams {
		if st.Type != "audio" {
			continue
		}
		if first < 0 {
			first = st.Index
		}
		if st.Default && def < 0 {
			def = st.Index
		}
		if lang != "" && match < 0 && strings.EqualFold(st.Language, lang) {
			match = st.Index
		}
	}
	switch {
	case match >= 0:
		return match
	case def >= 0:
		return def
	}
	return first
}

// ensureCopy finds or adds the copy a download uses, putting a failed,
// removed or out of date one back in line.
func (s *Server) ensureCopy(ctx context.Context, v store.Version, streams []store.Stream, p transcode.DownloadPreset, audio int) (store.DownloadFile, error) {
	quality := p.ID
	if p.MaxHeight > 0 && playsAsIs(v, streams, p) {
		// Already a small MP4 a phone plays: send it as it is.
		quality = "original"
	}
	if quality == "original" {
		// The whole file, every track: nothing to choose or prepare.
		audio = -1
	}
	want := store.DownloadFile{VersionID: v.ID, Quality: quality, Audio: audio, State: store.DownloadQueued,
		Estimate: p.Estimate(v.DurationSec, v.Size, p.CanCopyVideo(v.VideoCodec, v.HDR, v.Height, v.Bitrate))}
	if quality == "original" {
		want.State, want.Size, want.Estimate, want.ReadyAt, want.SourceMtime = store.DownloadReady, v.Size, v.Size, time.Now().Unix(), v.Mtime
	}
	f, created, err := s.Store.EnsureDownloadFile(ctx, want)
	if err != nil || created {
		return f, err
	}
	stale := f.SourceMtime != 0 && f.SourceMtime != v.Mtime
	switch {
	case quality == "original":
		if f.Size != v.Size || stale {
			f.Size, f.Estimate, f.SourceMtime, f.State, f.Error = v.Size, v.Size, v.Mtime, store.DownloadReady, ""
			err = s.Store.UpdateDownloadFile(ctx, f)
		}
	case f.State == store.DownloadFailed || f.State == store.DownloadExpired || (f.State == store.DownloadReady && (stale || !s.copyOnDisk(f))):
		s.removeCopyFile(f)
		f.State, f.Progress, f.Path, f.Size, f.Error, f.ReadyAt, f.Estimate = store.DownloadQueued, 0, "", 0, "", 0, want.Estimate
		err = s.Store.UpdateDownloadFile(ctx, f)
	}
	return f, err
}

// playsAsIs: an MP4 with H.264 that fits the quality and one stereo AAC
// track needs no preparing.
func playsAsIs(v store.Version, streams []store.Stream, p transcode.DownloadPreset) bool {
	if v.Container != "mp4" && v.Container != "m4v" {
		return false
	}
	if !p.CanCopyVideo(v.VideoCodec, v.HDR, v.Height, v.Bitrate) {
		return false
	}
	audio := 0
	for _, st := range streams {
		if st.Type == "audio" {
			audio++
			if st.Codec != "aac" || st.Channels > 2 {
				return false
			}
		}
	}
	return audio <= 1
}

func (s *Server) copyPath(f store.DownloadFile) string {
	if f.Path == "" || s.DownloadDir == "" {
		return ""
	}
	return filepath.Join(s.DownloadDir, filepath.Base(f.Path))
}

func (s *Server) copyOnDisk(f store.DownloadFile) bool {
	p := s.copyPath(f)
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

func (s *Server) removeCopyFile(f store.DownloadFile) {
	if p := s.copyPath(f); p != "" {
		_ = os.Remove(p)
	}
}

func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request, u store.User) {
	list, err := s.Store.UserDownloads(r.Context(), u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	device := myDeviceID(r)
	if r.URL.Query().Get("device") == "this" {
		kept := list[:0]
		for _, d := range list {
			if d.Device == device {
				kept = append(kept, d)
			}
		}
		list = kept
	}
	ds, _ := s.Store.GetDownloadSettings(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"enabled": ds.Enabled, "downloads": s.downloadViews(r.Context(), u, device, list)})
}

// myDownload reads the download in the path, if it's this account's (or
// the asker is the owner).
func (s *Server) myDownload(w http.ResponseWriter, r *http.Request, u store.User) (store.Download, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "download not found")
		return store.Download{}, false
	}
	d, err := s.Store.GetDownload(r.Context(), id)
	if err != nil || (d.UserID != u.ID && !u.IsAdmin) {
		writeErr(w, http.StatusNotFound, "download not found")
		return store.Download{}, false
	}
	return d, true
}

func (s *Server) getDownload(w http.ResponseWriter, r *http.Request, u store.User) {
	d, ok := s.myDownload(w, r, u)
	if !ok {
		return
	}
	ctx := r.Context()
	views := s.downloadViews(ctx, u, myDeviceID(r), []store.Download{d})
	out := downloadDetail{downloadView: views[0], Subtitles: []ExternalSubtitle{}}
	v, err := s.Store.GetVersion(ctx, d.File.VersionID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	streams, _ := s.Store.Streams(ctx, v.ID)
	out.Version = VersionView{Version: v, Streams: streams, Subtitles: []ExternalSubtitle{}}
	if in, err := s.Store.IntroFor(ctx, v.ID); err == nil {
		out.Version.Intro = in
	}
	if c, err := s.Store.EndCreditsFor(ctx, v.ID); err == nil {
		out.Version.EndCredits = c
	}
	for i, st := range streams {
		switch {
		case st.Type == "audio" && d.File.Audio >= 0 && st.Index == d.File.Audio:
			out.Audio = &streams[i]
		case st.Type == "subtitle" && transcode.TextSubtitle(st.Codec):
			out.Subtitles = append(out.Subtitles, ExternalSubtitle{Index: st.Index, Language: st.Language, Title: st.Title,
				Format: "webvtt", Forced: st.Forced, Source: "file",
				URL: "/api/v1/subtitles/" + v.ID + "/" + strconv.Itoa(st.Index) + ".vtt"})
		}
	}
	if d.File.Audio == -1 && d.File.Quality != "original" {
		for i, st := range streams {
			if st.Type == "audio" {
				out.Audio = &streams[i]
				break
			}
		}
	}
	out.Subtitles = append(out.Subtitles, s.externalSubtitles(ctx, v.ID, u)...)
	writeJSON(w, http.StatusOK, out)
}

// downloadFile sends a ready download's file, with byte ranges.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request, u store.User) {
	d, ok := s.myDownload(w, r, u)
	if !ok {
		return
	}
	ctx := r.Context()
	f := d.File
	if f.State == store.DownloadExpired {
		// Removed to save room: make it again.
		if v, err := s.Store.GetVersion(ctx, f.VersionID); err == nil {
			streams, _ := s.Store.Streams(ctx, v.ID)
			if p, ok := transcode.FindPreset(f.Quality); ok {
				_, _ = s.ensureCopy(ctx, v, streams, p, f.Audio)
				s.kickDownloads()
			}
		}
		writeErr(w, http.StatusConflict, "this download is being prepared again; try once it's ready")
		return
	}
	if f.State != store.DownloadReady {
		writeErr(w, http.StatusConflict, "this download isn't ready yet ("+f.State+")")
		return
	}
	v, err := s.Store.GetVersion(ctx, f.VersionID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not found")
		return
	}
	path, ext := s.copyPath(f), "mp4"
	if f.Quality == "original" {
		if v.Missing {
			writeErr(w, http.StatusNotFound, "the file is gone from the library")
			return
		}
		path, ext = v.Path, strings.TrimPrefix(strings.ToLower(filepath.Ext(v.Path)), ".")
	}
	file, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "file not available")
		return
	}
	defer file.Close()
	fi, err := file.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.Store.MarkDownloadFetched(ctx, d.ID)
	_ = s.Store.TouchDownloadFile(ctx, f.ID)
	if ct, ok := contentTypes[ext]; ok {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	name := s.downloadName(ctx, d.ItemID) + "." + ext
	w.Header().Set("Content-Disposition", `attachment; filename="`+asciiName(name)+`"; filename*=UTF-8''`+urlPathEscape(name))
	w.Header().Set("Cache-Control", "private, no-transform")
	var content io.ReadSeeker = file
	if runtime.GOOS == "windows" {
		content = plainReader{file} // see stream
	}
	http.ServeContent(w, r, "", fi.ModTime(), content)
}

// downloadName is a file name for a title: "Heat (1995)", "Show S01E02".
func (s *Server) downloadName(ctx context.Context, itemID string) string {
	it, err := s.Store.GetItem(ctx, itemID)
	if err != nil {
		return "Kinara download"
	}
	name := it.Title
	if it.Kind == store.ItemEpisode {
		if showID, err := s.Store.ShowOfEpisode(ctx, it.ID); err == nil {
			if show, err := s.Store.GetItem(ctx, showID); err == nil {
				name = fmt.Sprintf("%s S%02dE%02d", show.Title, it.SeasonNumber, it.EpisodeNumber)
			}
		}
	} else if it.Year > 0 {
		name = fmt.Sprintf("%s (%d)", it.Title, it.Year)
	}
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`<>:"/\|?*`, r) || r < 32 {
			return -1
		}
		return r
	}, name)
}

func asciiName(s string) string {
	return strings.Map(func(r rune) rune {
		if r > 126 || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, s)
}

func urlPathEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *Server) downloadSaved(w http.ResponseWriter, r *http.Request, u store.User) {
	d, ok := s.myDownload(w, r, u)
	if !ok {
		return
	}
	if d.File.State != store.DownloadReady {
		writeErr(w, http.StatusConflict, "this download isn't ready")
		return
	}
	if err := s.Store.MarkDownloadSaved(r.Context(), d.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteDownload(w http.ResponseWriter, r *http.Request, u store.User) {
	d, ok := s.myDownload(w, r, u)
	if !ok {
		return
	}
	if err := s.Store.DeleteDownload(r.Context(), d.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Stop and remove its copy at once if nobody else wants it.
	s.cleanDownloads(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// --- Owner ---

type downloadsAdminView struct {
	Settings store.DownloadSettings `json:"settings"`
	// CanConvert: the server can prepare smaller copies (ffmpeg found).
	CanConvert bool `json:"canConvert"`
	// UsedBytes is the room prepared copies take; Free the drive's free space.
	UsedBytes int64          `json:"usedBytes"`
	FreeBytes int64          `json:"freeBytes,omitempty"`
	Dir       string         `json:"dir,omitempty"`
	Downloads []downloadView `json:"downloads"`
}

func (s *Server) allDownloads(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	ds, _ := s.Store.GetDownloadSettings(ctx)
	list, err := s.Store.AllDownloads(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	v := downloadsAdminView{Settings: ds, CanConvert: s.canPrepare(), Dir: s.DownloadDir,
		Downloads: s.downloadViews(ctx, u, myDeviceID(r), list)}
	v.UsedBytes, _, _ = s.copiesSize(ctx)
	if s.DownloadDir != "" {
		if disk, ok := metrics.DiskUsage(s.DownloadDir); ok {
			v.FreeBytes = int64(disk.Free)
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) putDownloadSettings(w http.ResponseWriter, r *http.Request, u store.User) {
	ds, err := s.Store.GetDownloadSettings(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := readJSON(r, &ds); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if ds.MaxGB < 1 || ds.MaxGB > 100000 {
		writeErr(w, http.StatusBadRequest, "the size limit must be between 1 and 100000 GB")
		return
	}
	if ds.PerUser < 0 || ds.PerUser > 10000 {
		writeErr(w, http.StatusBadRequest, "downloads per account must be between 0 (no limit) and 10000")
		return
	}
	if err := s.Store.PutDownloadSettings(r.Context(), ds); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	go s.cleanDownloads(s.BaseCtx) // a smaller limit takes effect now
	writeJSON(w, http.StatusOK, ds)
}

// --- Preparing ---

// RunDownloads prepares queued copies one at a time and tidies up every
// hour, until ctx ends.
func (s *Server) RunDownloads(ctx context.Context) {
	if !s.canPrepare() {
		return
	}
	if err := os.MkdirAll(s.DownloadDir, 0o755); err != nil {
		s.Log.Warn("downloads folder", "err", err)
		return
	}
	if err := s.Store.RequeueInterrupted(ctx); err != nil {
		s.Log.Warn("downloads: requeue", "err", err)
	}
	s.planOptimized(ctx)
	s.kickDownloads()
	s.cleanDownloads(ctx)
	s.dl.mu.Lock()
	kick := s.dl.kick
	s.dl.mu.Unlock()
	tidy := time.NewTicker(time.Hour)
	defer tidy.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tidy.C:
			// New files in optimized libraries get their copies.
			s.planOptimized(ctx)
			s.cleanDownloads(ctx)
			s.kickDownloads()
			continue
		case <-kick:
		}
		last := int64(0)
		for ctx.Err() == nil {
			f, err := s.Store.NextDownloadFile(ctx)
			if err != nil || f.ID == last {
				break // none left, or one that couldn't even start (try on the next kick)
			}
			last = f.ID
			s.prepareCopy(ctx, f)
		}
	}
}

// prepareCopy converts one queued copy.
func (s *Server) prepareCopy(parent context.Context, f store.DownloadFile) {
	fail := func(msg string) {
		f.State, f.Error, f.Progress = store.DownloadFailed, msg, 0
		if err := s.Store.UpdateDownloadFile(parent, f); err != nil {
			s.Log.Warn("downloads: save", "err", err)
		}
	}
	v, err := s.Store.GetVersion(parent, f.VersionID)
	if err != nil || v.Missing {
		fail("the file is gone from the library")
		return
	}
	if _, err := os.Stat(v.Path); err != nil {
		fail("the file can't be read right now (is its drive connected?)")
		return
	}
	p, ok := transcode.FindPreset(f.Quality)
	if !ok || p.MaxHeight == 0 {
		f.State, f.Size, f.ReadyAt, f.SourceMtime = store.DownloadReady, v.Size, time.Now().Unix(), v.Mtime
		_ = s.Store.UpdateDownloadFile(parent, f)
		return
	}
	if msg := s.makeRoom(parent, f.Estimate, f.Optimized); msg != "" {
		fail(msg)
		return
	}
	streams, err := s.Store.Streams(parent, v.ID)
	if err != nil {
		fail(err.Error())
		return
	}
	req := downloadTranscode(v, streams, p, f.Audio)
	if it, err := s.Store.GetItem(parent, v.ItemID); err == nil {
		req.Owner = transcode.Owner{DeviceName: "Download", ItemID: it.ID, VersionID: v.ID, Title: it.Title}
		if f.Optimized {
			req.Owner.DeviceName = "Optimized version"
		}
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	run := &runningDownload{cancel: cancel, saved: time.Now()}
	s.dl.mu.Lock()
	if s.dl.running == nil {
		s.dl.running = map[int64]*runningDownload{}
	}
	s.dl.running[f.ID] = run
	s.dl.mu.Unlock()
	defer func() {
		s.dl.mu.Lock()
		delete(s.dl.running, f.ID)
		s.dl.mu.Unlock()
	}()

	f.State, f.Progress, f.Error = store.DownloadPreparing, 0, ""
	if err := s.Store.UpdateDownloadFile(ctx, f); err != nil {
		s.Log.Warn("downloads: save", "err", err)
		return
	}
	name := fmt.Sprintf("%d-%s.mp4", f.ID, f.Quality)
	out := filepath.Join(s.DownloadDir, name)
	start := time.Now()
	s.Log.Info("download preparing", "file", v.RelPath, "quality", f.Quality, "video", req.Video)
	err = s.Transcoder.Convert(ctx, req, out, func(sec float64) {
		if v.DurationSec <= 0 {
			return
		}
		share := min(sec/v.DurationSec, 0.99)
		s.dl.mu.Lock()
		run.progress = share
		save := time.Since(run.saved) > 10*time.Second
		if save {
			run.saved = time.Now()
		}
		s.dl.mu.Unlock()
		if save {
			_ = s.Store.SetDownloadProgress(parent, f.ID, share)
		}
	})
	if parent.Err() != nil {
		return // shutting down: RequeueInterrupted puts it back in line
	}
	if ctx.Err() != nil {
		// Cancelled: nobody wants it any more (cleanDownloads removes it),
		// or it was asked for again.
		_ = os.Remove(out)
		if cur, err := s.Store.GetDownloadFile(parent, f.ID); err == nil && cur.State == store.DownloadPreparing {
			cur.State, cur.Progress = store.DownloadQueued, 0
			_ = s.Store.UpdateDownloadFile(parent, cur)
		}
		return
	}
	if err != nil {
		_ = os.Remove(out)
		s.Log.Warn("download preparing failed", "file", v.RelPath, "quality", f.Quality, "err", err)
		msg := err.Error()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		fail("couldn't convert the file: " + msg)
		return
	}
	fi, err := os.Stat(out)
	if err != nil {
		fail(err.Error())
		return
	}
	f.State, f.Progress, f.Path, f.Size, f.ReadyAt, f.SourceMtime = store.DownloadReady, 1, name, fi.Size(), time.Now().Unix(), v.Mtime
	if err := s.Store.UpdateDownloadFile(parent, f); err != nil {
		s.Log.Warn("downloads: save", "err", err)
	}
	s.Log.Info("download ready", "file", v.RelPath, "quality", f.Quality, "size", fi.Size(),
		"took", time.Since(start).Round(time.Second))
}

// downloadTranscode is how a version becomes a download at a quality:
// H.264 within the quality's size (kept as is when it already fits) and one
// stereo AAC track.
func downloadTranscode(v store.Version, streams []store.Stream, p transcode.DownloadPreset, audio int) transcode.Request {
	req := transcode.Request{Path: v.Path, Video: "h264", MaxBitrate: p.MaxBitrate, MaxHeight: p.MaxHeight,
		HDR: v.HDR, SourceCodec: v.VideoCodec, Audio: audio, AudioCodec: "aac", MaxAudioChannels: 2,
		// The phone shows no Dolby Vision from a download it converts.
		ClientDV: "0"}
	if p.CanCopyVideo(v.VideoCodec, v.HDR, v.Height, v.Bitrate) {
		req.Video = "copy"
	}
	var track *store.Stream
	for i, st := range streams {
		if st.Type == "audio" && (audio < 0 || st.Index == audio) {
			track = &streams[i]
			break
		}
	}
	if track != nil {
		req.AudioChannels = track.Channels
		if track.Codec == "aac" && track.Channels > 0 && track.Channels <= 2 {
			req.AudioCodec = "copy"
		}
	}
	for _, st := range streams {
		if st.Type == "video" {
			req.SourceProfile = st.Profile
			break
		}
	}
	return req
}

// --- Tidying up ---

// copiesSize is the room prepared copies take: those for downloads (which
// count against the owner's limit) and optimized versions.
func (s *Server) copiesSize(ctx context.Context) (downloads, optimized int64, err error) {
	uses, err := s.Store.DownloadFileUses(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, u := range uses {
		if u.Path == "" || u.State != store.DownloadReady {
			continue
		}
		if u.Optimized {
			optimized += u.Size
		} else {
			downloads += u.Size
		}
	}
	return downloads, optimized, nil
}

// makeRoom frees room for a copy of need bytes within the owner's limit
// and the drive's free space, removing copies every device already has,
// oldest first. It says why there isn't room ("" when there is). An
// optimized version only needs room on the drive: the limit is for
// downloads, and optimized versions are never removed to make room.
func (s *Server) makeRoom(ctx context.Context, need int64, optimized bool) string {
	ds, _ := s.Store.GetDownloadSettings(ctx)
	limit := int64(ds.MaxGB) << 30
	uses, err := s.Store.DownloadFileUses(ctx)
	if err != nil {
		return err.Error()
	}
	var used int64
	var spare []store.DownloadFileUse
	for _, u := range uses {
		if u.Path == "" || u.State != store.DownloadReady || u.Optimized {
			continue
		}
		used += u.Size
		if u.Unsaved == 0 {
			spare = append(spare, u)
		}
	}
	if optimized {
		used, limit = 0, math.MaxInt64
	}
	sort.Slice(spare, func(i, j int) bool { return spare[i].LastSaved < spare[j].LastSaved })
	free := func() int64 {
		if disk, ok := metrics.DiskUsage(s.DownloadDir); ok {
			return int64(disk.Free) - diskReserve
		}
		return math.MaxInt64
	}
	for len(spare) > 0 && (used+need > limit || need > free()) {
		u := spare[0]
		spare = spare[1:]
		s.expireCopy(ctx, u.DownloadFile)
		used -= u.Size
	}
	switch {
	case used+need > limit:
		return fmt.Sprintf("there's no room for it in the server's %d GB for downloads; remove some downloads first", ds.MaxGB)
	case need > free():
		return "the server's drive is nearly full"
	}
	return ""
}

// expireCopy removes a prepared copy's file, keeping its record so asking
// again prepares it again.
func (s *Server) expireCopy(ctx context.Context, f store.DownloadFile) {
	s.removeCopyFile(f)
	f.State, f.Path, f.Size, f.Progress = store.DownloadExpired, "", 0, 0
	_ = s.Store.UpdateDownloadFile(ctx, f)
	s.Log.Info("download copy removed to save room", "id", f.ID, "quality", f.Quality)
}

// cleanDownloads removes copies nobody wants (stopping one being prepared),
// copies every device has had for a while, copies unused for a month,
// copies of files that changed, and stray files in the folder.
func (s *Server) cleanDownloads(ctx context.Context) {
	s.dl.cleanMu.Lock()
	defer s.dl.cleanMu.Unlock()
	uses, err := s.Store.DownloadFileUses(ctx)
	if err != nil {
		s.Log.Warn("downloads: tidy", "err", err)
		return
	}
	nowT := time.Now()
	known := map[string]bool{}
	for _, u := range uses {
		f := u.DownloadFile
		if u.Downloads == 0 && !f.Optimized {
			s.dl.mu.Lock()
			run := s.dl.running[f.ID]
			if run != nil {
				run.cancel()
			}
			s.dl.mu.Unlock()
			if run == nil && f.State != store.DownloadReady && nowT.Sub(time.Unix(f.CreatedAt, 0)) < 2*time.Minute {
				continue // just made: its download is being added
			}
			s.removeCopyFile(f)
			_ = s.Store.DeleteDownloadFile(ctx, f.ID)
			continue
		}
		if f.Path == "" || f.State != store.DownloadReady {
			continue
		}
		known[filepath.Base(f.Path)] = true
		lastUse := max(f.UsedAt, f.ReadyAt)
		switch {
		case f.Optimized:
			// Kept while the owner's rule wants it (see optimize.go); only a
			// replaced source file makes it again.
			if v, err := s.Store.GetVersion(ctx, f.VersionID); err == nil && v.Mtime != f.SourceMtime && f.SourceMtime != 0 {
				s.removeCopyFile(f)
				f.State, f.Path, f.Size, f.Progress, f.ReadyAt = store.DownloadQueued, "", 0, 0, 0
				_ = s.Store.UpdateDownloadFile(ctx, f)
				s.kickDownloads()
			}
		case u.Unsaved == 0 && u.LastSaved > 0 && nowT.Sub(time.Unix(u.LastSaved, 0)) > keepAfterSaved,
			nowT.Sub(time.Unix(lastUse, 0)) > keepUnused:
			s.expireCopy(ctx, f)
		default:
			if v, err := s.Store.GetVersion(ctx, f.VersionID); err == nil && v.Mtime != f.SourceMtime && f.SourceMtime != 0 {
				// The file was replaced: prepare it again for whoever's still waiting.
				s.removeCopyFile(f)
				f.State, f.Path, f.Size, f.Progress, f.ReadyAt = store.DownloadQueued, "", 0, 0, 0
				_ = s.Store.UpdateDownloadFile(ctx, f)
				s.kickDownloads()
			}
		}
	}
	if s.DownloadDir == "" {
		return
	}
	entries, err := os.ReadDir(s.DownloadDir)
	if err != nil {
		return
	}
	s.dl.mu.Lock()
	busy := len(s.dl.running) > 0
	s.dl.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if known[name] || e.IsDir() {
			continue
		}
		if strings.HasSuffix(name, ".part") && busy {
			continue // being written
		}
		if !strings.HasSuffix(name, ".mp4") && !strings.HasSuffix(name, ".part") {
			continue
		}
		// A copy being written right now is renamed into place before it's
		// recorded as ready: leave fresh files alone.
		if info, err := e.Info(); err == nil && nowT.Sub(info.ModTime()) < time.Hour {
			continue
		}
		_ = os.Remove(filepath.Join(s.DownloadDir, name))
	}
}
