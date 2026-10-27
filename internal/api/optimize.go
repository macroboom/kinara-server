package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"kinara/internal/metrics"
	"kinara/internal/store"
	"kinara/internal/transcode"
)

// diskFree is the free space on the drive holding dir.
func diskFree(dir string) (int64, bool) {
	d, ok := metrics.DiskUsage(dir)
	return int64(d.Free), ok
}

// Optimized versions: the owner asks for a smaller copy of a library or a
// title ahead of time (a 720p copy of a 4K HDR film, say). The server makes
// them in the background with the download queue (downloads.go), after any
// downloads someone is waiting for, and keeps them while the rule stands.
//
// A ready copy shows up as an extra version of its title, with the ID "o"
// plus the copy's number ("o42"): an MP4 with H.264 and one stereo AAC
// track. Apps already pick the best version that fits their bandwidth, so
// remote play and phones on mobile data pick the copy and play it as it is,
// with seeking and no live conversion; at home the original still wins.
// Subtitles, preview thumbnails and markers come from the original (the
// timeline is the same), so those routes take a copy's ID too.

// optimizeState remembers which copies each rule made, for the Manage page.
type optimizeState struct {
	mu sync.Mutex
	// files: each rule's copies; small: titles already as small as the
	// quality (nothing to make).
	files map[int64][]int64
	small map[int64]int
}

func (s *Server) optimizeRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/optimize", s.admin(s.listOptimize))
	mux.Handle("POST /api/v1/optimize", s.admin(s.createOptimize))
	mux.Handle("DELETE /api/v1/optimize/{id}", s.admin(s.deleteOptimize))
	mux.Handle("POST /api/v1/optimize/{id}/retry", s.admin(s.retryOptimize))
	mux.Handle("GET /api/v1/items/{id}/optimize", s.admin(s.itemOptimize))
}

// --- IDs ------------------------------------------------------------------------

const optimizedPrefix = "o"

func optimizedID(fileID int64) string { return optimizedPrefix + strconv.FormatInt(fileID, 10) }

// parseOptimizedID reads a copy's number from "o42". Real version IDs are
// hex, so they never start with "o".
func parseOptimizedID(id string) (int64, bool) {
	rest, ok := strings.CutPrefix(id, optimizedPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil && n > 0
}

// sourceVersion is the original file behind a version ID (a copy's ID
// gives its original), for what the two share: subtitles, thumbnails,
// markers and watch progress.
func (s *Server) sourceVersion(ctx context.Context, id string) (store.Version, error) {
	if fid, ok := parseOptimizedID(id); ok {
		f, err := s.Store.GetDownloadFile(ctx, fid)
		if err != nil {
			return store.Version{}, store.ErrNotFound
		}
		return s.Store.GetVersion(ctx, f.VersionID)
	}
	return s.Store.GetVersion(ctx, id)
}

// sourceVersionID maps a copy's ID to its original's ("" stays "").
func (s *Server) sourceVersionID(ctx context.Context, id string) string {
	if _, ok := parseOptimizedID(id); !ok {
		return id
	}
	v, err := s.sourceVersion(ctx, id)
	if err != nil {
		return ""
	}
	return v.ID
}

// playableVersion is the file a version ID plays: a ready copy for a
// copy's ID, else the version itself, with the tracks inside that file.
func (s *Server) playableVersion(ctx context.Context, id string) (store.Version, []store.Stream, error) {
	if fid, ok := parseOptimizedID(id); ok {
		f, err := s.Store.GetDownloadFile(ctx, fid)
		if err != nil || f.State != store.DownloadReady || f.Path == "" {
			return store.Version{}, nil, store.ErrNotFound
		}
		src, err := s.Store.GetVersion(ctx, f.VersionID)
		if err != nil {
			return store.Version{}, nil, err
		}
		streams, err := s.Store.Streams(ctx, src.ID)
		if err != nil {
			return store.Version{}, nil, err
		}
		v, st := s.optimizedVersion(src, streams, f, false)
		return v, st, nil
	}
	v, err := s.Store.GetVersion(ctx, id)
	if err != nil {
		return v, nil, err
	}
	streams, err := s.Store.Streams(ctx, v.ID)
	return v, streams, err
}

// optimizedVersion describes a ready copy as a version of its title: the
// copy's file and tracks (H.264 video, one stereo AAC track) on the
// original's timeline. withSubs adds the original's subtitle tracks, which
// apps fetch as WebVTT from the original (by the copy's ID).
func (s *Server) optimizedVersion(src store.Version, streams []store.Stream, f store.DownloadFile, withSubs bool) (store.Version, []store.Stream) {
	p, _ := transcode.FindPreset(f.Quality)
	h := src.Height
	if p.MaxHeight > 0 && (h == 0 || h > p.MaxHeight) {
		h = p.MaxHeight
	}
	w := src.Width
	if src.Height > 0 && h != src.Height {
		w = int(float64(src.Width)*float64(h)/float64(src.Height)/2+0.5) * 2
	}
	v := store.Version{ID: optimizedID(f.ID), ItemID: src.ItemID, LibraryID: src.LibraryID, Path: s.copyPath(f),
		RelPath: "Optimized copy (" + strconv.Itoa(h) + "p)", Part: src.Part,
		EpisodeEnd: src.EpisodeEnd, Size: f.Size, Mtime: f.ReadyAt, Container: "mp4", DurationSec: src.DurationSec,
		Width: w, Height: h, VideoCodec: "h264", AddedAt: f.ReadyAt, ScannedAt: f.ReadyAt}
	if src.DurationSec > 0 {
		v.Bitrate = int64(float64(f.Size) * 8 / src.DurationSec)
	}
	out := []store.Stream{{Index: 0, Type: "video", Codec: "h264", Profile: "High", Default: true}}
	audio := store.Stream{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Default: true}
	for _, st := range streams {
		if st.Type == "audio" && (f.Audio < 0 || st.Index == f.Audio) {
			audio.Language, audio.Title = st.Language, st.Title
			audio.Channels = min(max(st.Channels, 1), 2)
			break
		}
	}
	out = append(out, audio)
	if withSubs {
		for _, st := range streams {
			if st.Type == "subtitle" {
				out = append(out, st)
			}
		}
	}
	return v, out
}

// optimizedViews are a version's ready copies, as versions after it.
func (s *Server) optimizedViews(ctx context.Context, src store.Version, streams []store.Stream, base VersionView) []VersionView {
	copies, err := s.Store.OptimizedCopies(ctx, src.ID)
	if err != nil || len(copies) == 0 {
		return nil
	}
	var out []VersionView
	for _, f := range copies {
		if !s.copyOnDisk(f) {
			continue
		}
		v, st := s.optimizedVersion(src, streams, f, true)
		vv := base
		vv.Version, vv.Streams = v, st
		vv.StreamURL = "/api/v1/stream/" + v.ID
		if base.TranscodeURL != "" {
			vv.TranscodeURL = "/api/v1/transcode/" + v.ID
		}
		if base.TrickplayURL != "" {
			vv.TrickplayURL = "/api/v1/trickplay/" + v.ID
		}
		vv.Optimized = f.Quality
		out = append(out, vv)
	}
	return out
}

// --- Making them -------------------------------------------------------------------

// planOptimized turns the rules into copies: each covered title gets one at
// its rule's quality (unless it's already that small), copies no rule wants
// any more stop being optimized versions, and new files in an optimized
// library are picked up. Runs at start, hourly and when a rule changes.
func (s *Server) planOptimized(ctx context.Context) {
	if !s.canPrepare() {
		return
	}
	s.opt.mu.Lock()
	defer s.opt.mu.Unlock()
	rules, err := s.Store.OptimizeRules(ctx)
	if err != nil {
		s.Log.Warn("optimize: rules", "err", err)
		return
	}
	files := map[int64][]int64{}
	small := map[int64]int{}
	var want []int64
	for _, r := range rules {
		p, ok := transcode.FindPreset(r.Quality)
		if !ok || p.MaxHeight == 0 {
			continue
		}
		versions, err := s.ruleVersions(ctx, r)
		if err != nil {
			s.Log.Warn("optimize: titles", "rule", r.ID, "err", err)
			continue
		}
		for _, v := range versions {
			fid, made, err := s.ensureOptimized(ctx, v, p)
			if err != nil {
				s.Log.Warn("optimize: copy", "file", v.RelPath, "err", err)
				continue
			}
			if !made {
				small[r.ID]++
				continue
			}
			files[r.ID] = append(files[r.ID], fid)
			want = append(want, fid)
		}
	}
	if err := s.Store.SetOptimized(ctx, want); err != nil {
		s.Log.Warn("optimize: mark", "err", err)
		return
	}
	s.opt.files, s.opt.small = files, small
}

// OptimizeNewFiles plans optimized versions again after a scan.
func (s *Server) OptimizeNewFiles() {
	if !s.canPrepare() {
		return
	}
	s.planOptimized(s.BaseCtx)
	s.kickDownloads()
}

// ruleVersions are the files a rule covers: the best version of each
// movie or episode (and each part of one in several files).
func (s *Server) ruleVersions(ctx context.Context, r store.OptimizeRule) ([]store.Version, error) {
	var all []store.Version
	if r.LibraryID > 0 {
		byPath, err := s.Store.LibraryVersions(ctx, r.LibraryID)
		if err != nil {
			return nil, err
		}
		for _, v := range byPath {
			all = append(all, v)
		}
	} else {
		it, err := s.Store.GetItem(ctx, r.ItemID)
		if err != nil {
			return nil, nil // the title left the library
		}
		targets, err := s.downloadTargets(ctx, store.User{}, it, false)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			vs, err := s.Store.VersionsForItem(ctx, t.it.ID)
			if err != nil {
				return nil, err
			}
			all = append(all, vs...)
		}
	}
	best := map[string]store.Version{}
	for _, v := range all {
		if v.Missing || v.ItemID == "" {
			continue
		}
		key := v.ItemID + "#" + strconv.Itoa(v.Part)
		if old, ok := best[key]; !ok || v.Height > old.Height || (v.Height == old.Height && v.Size > old.Size) {
			best[key] = v
		}
	}
	out := make([]store.Version, 0, len(best))
	for _, v := range best {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// ensureOptimized finds or adds a version's copy at a quality (made says
// whether one is needed: a file already that small isn't copied). A
// removed or out of date copy goes back in line; a failed one waits for
// the owner's "Try again".
func (s *Server) ensureOptimized(ctx context.Context, v store.Version, p transcode.DownloadPreset) (id int64, made bool, err error) {
	if p.CanCopyVideo(v.VideoCodec, v.HDR, v.Height, v.Bitrate) {
		return 0, false, nil
	}
	streams, err := s.Store.Streams(ctx, v.ID)
	if err != nil {
		return 0, false, err
	}
	want := store.DownloadFile{VersionID: v.ID, Quality: p.ID, Audio: pickAudio(streams, ""), State: store.DownloadQueued,
		Estimate: p.Estimate(v.DurationSec, v.Size, false)}
	f, created, err := s.Store.EnsureDownloadFile(ctx, want)
	if err != nil {
		return 0, false, err
	}
	if !created {
		stale := f.SourceMtime != 0 && f.SourceMtime != v.Mtime
		if f.State == store.DownloadExpired || (f.State == store.DownloadReady && (stale || !s.copyOnDisk(f))) {
			s.removeCopyFile(f)
			f.State, f.Progress, f.Path, f.Size, f.Error, f.ReadyAt, f.Estimate = store.DownloadQueued, 0, "", 0, "", 0, want.Estimate
			if err := s.Store.UpdateDownloadFile(ctx, f); err != nil {
				return 0, false, err
			}
		}
	}
	return f.ID, true, nil
}

// --- Owner routes --------------------------------------------------------------------

type optimizeRuleView struct {
	store.OptimizeRule
	// Title names the library or the title ("The Bear · Season 2").
	Title string `json:"title"`
	// Kind is "library", or the title's kind.
	Kind string `json:"kind"`
	// Copies counts the copies by state; Small counts titles already as
	// small as the quality.
	Copies    int   `json:"copies"`
	Ready     int   `json:"ready"`
	Preparing int   `json:"preparing"`
	Queued    int   `json:"queued"`
	Failed    int   `json:"failed"`
	Small     int   `json:"small"`
	Bytes     int64 `json:"bytes"`
	// Progress is the share of the copy being made now, when there is one.
	Progress float64 `json:"progress,omitempty"`
}

type optimizeView struct {
	CanConvert bool                       `json:"canConvert"`
	UsedBytes  int64                      `json:"usedBytes"`
	FreeBytes  int64                      `json:"freeBytes,omitempty"`
	Rules      []optimizeRuleView         `json:"rules"`
	Qualities  []transcode.DownloadPreset `json:"qualities"`
}

func (s *Server) listOptimize(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	rules, err := s.Store.OptimizeRules(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	uses, err := s.Store.DownloadFileUses(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := map[int64]store.DownloadFile{}
	for _, x := range uses {
		byID[x.ID] = x.DownloadFile
	}
	s.opt.mu.Lock()
	files, small := s.opt.files, s.opt.small
	s.opt.mu.Unlock()
	out := optimizeView{CanConvert: s.canPrepare(), Rules: []optimizeRuleView{}, Qualities: transcode.DownloadPresets[1:]}
	_, out.UsedBytes, _ = s.copiesSize(ctx)
	if s.DownloadDir != "" {
		if disk, ok := diskFree(s.DownloadDir); ok {
			out.FreeBytes = disk
		}
	}
	for _, rule := range rules {
		v := optimizeRuleView{OptimizeRule: rule, Small: small[rule.ID]}
		v.Title, v.Kind = s.ruleTitle(ctx, rule)
		for _, fid := range files[rule.ID] {
			f, ok := byID[fid]
			if !ok {
				continue
			}
			v.Copies++
			switch f.State {
			case store.DownloadReady:
				v.Ready++
				v.Bytes += f.Size
			case store.DownloadPreparing:
				v.Preparing++
				s.dl.mu.Lock()
				if run := s.dl.running[f.ID]; run != nil {
					v.Progress = run.progress
				}
				s.dl.mu.Unlock()
			case store.DownloadFailed:
				v.Failed++
			default:
				v.Queued++
			}
		}
		out.Rules = append(out.Rules, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// ruleTitle names what a rule covers.
func (s *Server) ruleTitle(ctx context.Context, r store.OptimizeRule) (title, kind string) {
	if r.LibraryID > 0 {
		if lib, err := s.Store.GetLibrary(ctx, r.LibraryID); err == nil {
			return lib.Name, "library"
		}
		return "A library", "library"
	}
	it, err := s.Store.GetItem(ctx, r.ItemID)
	if err != nil {
		return "A title", "movie"
	}
	switch it.Kind {
	case store.ItemSeason:
		if show, err := s.Store.GetItem(ctx, it.ParentID); err == nil {
			return show.Title + " · " + it.Title, it.Kind
		}
	case store.ItemEpisode:
		if showID, err := s.Store.ShowOfEpisode(ctx, it.ID); err == nil {
			if show, err := s.Store.GetItem(ctx, showID); err == nil {
				return show.Title + " · S" + strconv.Itoa(it.SeasonNumber) + "E" + strconv.Itoa(it.EpisodeNumber) + " " + it.Title, it.Kind
			}
		}
	}
	return it.Title, it.Kind
}

type optimizeRequest struct {
	LibraryID int64  `json:"libraryId"`
	ItemID    string `json:"itemId"`
	Quality   string `json:"quality"`
}

func (s *Server) createOptimize(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	if !s.canPrepare() {
		writeErr(w, http.StatusBadRequest, "this server can't convert files (ffmpeg wasn't found)")
		return
	}
	var req optimizeRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Quality == "" {
		req.Quality = "medium"
	}
	if p, ok := transcode.FindPreset(req.Quality); !ok || p.MaxHeight == 0 {
		writeErr(w, http.StatusBadRequest, "quality must be high, medium or low")
		return
	}
	rule := store.OptimizeRule{Quality: req.Quality}
	switch {
	case req.LibraryID > 0 && req.ItemID == "":
		if _, err := s.Store.GetLibrary(ctx, req.LibraryID); err != nil {
			writeErr(w, http.StatusNotFound, "library not found")
			return
		}
		rule.LibraryID = req.LibraryID
	case req.ItemID != "" && req.LibraryID == 0:
		it, err := s.Store.GetItem(ctx, req.ItemID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "item not found")
			return
		}
		switch it.Kind {
		case store.ItemMovie, store.ItemEpisode, store.ItemSeason, store.ItemShow:
		default:
			writeErr(w, http.StatusBadRequest, "movies, episodes, seasons and shows can be optimized")
			return
		}
		rule.ItemID = it.ID
	default:
		writeErr(w, http.StatusBadRequest, "give a libraryId or an itemId")
		return
	}
	if err := s.Store.PutOptimizeRule(ctx, &rule); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.planOptimized(ctx)
	s.kickDownloads()
	go s.cleanDownloads(s.BaseCtx) // a changed quality leaves the old copies unwanted
	writeJSON(w, http.StatusCreated, rule)
}

func (s *Server) ruleID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusNotFound, "rule not found")
		return 0, false
	}
	return id, true
}

func (s *Server) deleteOptimize(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := s.ruleID(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteOptimizeRule(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.planOptimized(r.Context())
	// Copies nobody wants now go (and one being made stops).
	s.cleanDownloads(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// retryOptimize puts a rule's failed copies back in line.
func (s *Server) retryOptimize(w http.ResponseWriter, r *http.Request, u store.User) {
	id, ok := s.ruleID(w, r)
	if !ok {
		return
	}
	s.opt.mu.Lock()
	ids := append([]int64(nil), s.opt.files[id]...)
	s.opt.mu.Unlock()
	n := 0
	for _, fid := range ids {
		f, err := s.Store.GetDownloadFile(r.Context(), fid)
		if err != nil || f.State != store.DownloadFailed {
			continue
		}
		f.State, f.Error, f.Progress = store.DownloadQueued, "", 0
		if err := s.Store.UpdateDownloadFile(r.Context(), f); err == nil {
			n++
		}
	}
	s.kickDownloads()
	writeJSON(w, http.StatusOK, map[string]int{"retried": n})
}

// itemOptimize answers GET /items/{id}/optimize: the rule for this title
// (or the library's), and its copies.
func (s *Server) itemOptimize(w http.ResponseWriter, r *http.Request, u store.User) {
	ctx := r.Context()
	it, err := s.Store.GetItem(ctx, r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "item not found")
		return
	}
	rules, err := s.Store.OptimizeRules(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := map[string]any{"canConvert": s.canPrepare()}
	for _, rule := range rules {
		if rule.ItemID == it.ID {
			out["rule"] = rule
		}
		if rule.LibraryID == it.LibraryID {
			out["libraryRule"] = rule
		}
	}
	type copyView struct {
		Quality  string  `json:"quality"`
		State    string  `json:"state"`
		Progress float64 `json:"progress"`
		Size     int64   `json:"size"`
		Error    string  `json:"error,omitempty"`
	}
	copies := []copyView{}
	if it.Kind == store.ItemMovie || it.Kind == store.ItemEpisode {
		uses, _ := s.Store.DownloadFileUses(ctx)
		versions, _ := s.Store.VersionsForItem(ctx, it.ID)
		ids := map[string]bool{}
		for _, v := range versions {
			ids[v.ID] = true
		}
		for _, x := range uses {
			if x.Optimized && ids[x.VersionID] {
				copies = append(copies, copyView{Quality: x.Quality, State: x.State, Progress: x.Progress, Size: x.Size, Error: x.Error})
			}
		}
	}
	out["copies"] = copies
	writeJSON(w, http.StatusOK, out)
}
