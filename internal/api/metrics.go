package api

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"kinora/internal/metrics"
	"kinora/internal/store"
)

// The manager's metrics page: the computer and traffic over time, storage,
// what the libraries are made of, how they're watched, how the API
// performs, and recent warnings.

// StreamCounts is what the metrics collector samples: devices playing
// directly, and conversions running.
func (s *Server) StreamCounts() (direct, converting int) {
	playing := s.playing.list(time.Now())
	converting = 0
	busy := map[string]bool{}
	if s.Transcoder != nil {
		for _, l := range s.Transcoder.Live() {
			converting++
			busy[l.Device] = true
		}
	}
	for _, p := range playing {
		if !busy[p.Device] {
			direct++
		}
	}
	return direct, converting
}

func (s *Server) metricsLive(w http.ResponseWriter, r *http.Request, u store.User) {
	if s.Metrics == nil {
		writeErr(w, http.StatusNotImplemented, "metrics are off")
		return
	}
	writeJSON(w, http.StatusOK, s.Metrics.Range(r.URL.Query().Get("range")))
}

func (s *Server) metricsHTTP(w http.ResponseWriter, r *http.Request, u store.User) {
	writeJSON(w, http.StatusOK, map[string]any{"since": s.httpStats.Since().Unix(), "routes": s.httpStats.Routes()})
}

func (s *Server) metricsLogs(w http.ResponseWriter, r *http.Request, u store.User) {
	entries := []metrics.LogEntry{}
	if s.Logs != nil {
		entries = s.Logs.Entries()
	}
	writeJSON(w, http.StatusOK, entries)
}

// --- Storage ---

type volumeView struct {
	// Path is a folder on the volume (the first library folder seen there,
	// or the server's data folder).
	Path      string   `json:"path"`
	Total     uint64   `json:"total"`
	Free      uint64   `json:"free"`
	Libraries []string `json:"libraries"`
	Data      bool     `json:"data"`
}

type libraryStorage struct {
	ID    int64   `json:"id"`
	Name  string  `json:"name"`
	Kind  string  `json:"kind"`
	Files int     `json:"files"`
	Bytes int64   `json:"bytes"`
	Hours float64 `json:"hours"`
}

type storageView struct {
	Volumes     []volumeView      `json:"volumes"`
	Libraries   []libraryStorage  `json:"libraries"`
	Cache       []store.Share     `json:"cache"`
	Database    int64             `json:"database"`
	Composition store.Composition `json:"composition"`
	At          int64             `json:"at"`
}

// storageCache keeps the last answer: walking the cache folders (thousands
// of preview images) takes a moment.
type storageCache struct {
	mu   sync.Mutex
	view *storageView
}

const storageFresh = 5 * time.Minute

func (s *Server) metricsStorage(w http.ResponseWriter, r *http.Request, u store.User) {
	s.storage.mu.Lock()
	defer s.storage.mu.Unlock()
	if v := s.storage.view; v != nil && r.URL.Query().Get("fresh") == "" && time.Since(time.Unix(v.At, 0)) < storageFresh {
		writeJSON(w, http.StatusOK, v)
		return
	}
	v, err := s.buildStorage(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.storage.view = v
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) buildStorage(r *http.Request) (*storageView, error) {
	ctx := r.Context()
	v := &storageView{At: time.Now().Unix(), Volumes: []volumeView{}, Libraries: []libraryStorage{}, Cache: []store.Share{}}
	libs, err := s.Store.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	sizes, err := s.Store.LibrarySizes(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[int64]store.LibrarySize{}
	for _, l := range sizes {
		byID[l.LibraryID] = l
	}
	// Volumes: folders whose disks report the same size and free space are
	// on the same disk.
	type key struct{ total, free uint64 }
	vols := map[key]int{}
	addVolume := func(path, lib string, data bool) {
		d, ok := metrics.DiskUsage(path)
		if !ok {
			return
		}
		k := key{d.Total, d.Free}
		i, seen := vols[k]
		if !seen {
			i = len(v.Volumes)
			vols[k] = i
			v.Volumes = append(v.Volumes, volumeView{Path: path, Total: d.Total, Free: d.Free, Libraries: []string{}})
		}
		if lib != "" && !contains(v.Volumes[i].Libraries, lib) {
			v.Volumes[i].Libraries = append(v.Volumes[i].Libraries, lib)
		}
		if data {
			v.Volumes[i].Data = true
		}
	}
	for _, l := range libs {
		sz := byID[l.ID]
		v.Libraries = append(v.Libraries, libraryStorage{ID: l.ID, Name: l.Name, Kind: l.Kind, Files: sz.Files, Bytes: sz.Bytes, Hours: sz.Hours})
		for _, f := range l.Folders {
			addVolume(f, l.Name, false)
		}
	}
	sort.Slice(v.Libraries, func(i, j int) bool { return v.Libraries[i].Bytes > v.Libraries[j].Bytes })
	if s.DataDir != "" {
		addVolume(s.DataDir, "", true)
		cacheDir := s.CacheDir
		if cacheDir == "" {
			cacheDir = filepath.Join(s.DataDir, "cache")
		} else {
			// A cache on another disk shows up as that disk (same disk: merged).
			addVolume(cacheDir, "", true)
		}
		if entries, err := os.ReadDir(cacheDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				n, b := dirSize(filepath.Join(cacheDir, e.Name()))
				v.Cache = append(v.Cache, store.Share{Label: e.Name(), Files: n, Bytes: b})
			}
		}
		sort.Slice(v.Cache, func(i, j int) bool { return v.Cache[i].Bytes > v.Cache[j].Bytes })
		for _, f := range []string{"kinora.db", "kinora.db-wal", "kinora.db-shm"} {
			if fi, err := os.Stat(filepath.Join(s.DataDir, f)); err == nil {
				v.Database += fi.Size()
			}
		}
	}
	if v.Composition, err = s.Store.LibraryComposition(ctx); err != nil {
		return nil, err
	}
	return v, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func dirSize(dir string) (files int, bytes int64) {
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			files++
			bytes += fi.Size()
		}
		return nil
	})
	return files, bytes
}

// --- Usage ---

type usageDay struct {
	Day     string  `json:"day"` // 2026-10-14, server time
	Plays   int     `json:"plays"`
	Hours   float64 `json:"hours"`
	Direct  float64 `json:"direct"`  // hours
	Copy    float64 `json:"copy"`    // hours
	Convert float64 `json:"convert"` // hours
}

type usageRow struct {
	Label string  `json:"label"`
	Kind  string  `json:"kind,omitempty"`
	Plays int     `json:"plays"`
	Hours float64 `json:"hours"`
}

type usageView struct {
	Days    []usageDay `json:"days"`
	Users   []usageRow `json:"users"`
	Devices []usageRow `json:"devices"`
	Titles  []usageRow `json:"titles"`
	Methods struct {
		Direct  float64 `json:"direct"`
		Copy    float64 `json:"copy"`
		Convert float64 `json:"convert"`
	} `json:"methods"`
	Plays int     `json:"plays"`
	Hours float64 `json:"hours"`
}

// metricsUsage sums plays over the last ?days= (default 30, at most 365).
func (s *Server) metricsUsage(w http.ResponseWriter, r *http.Request, u store.User) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = 30
	}
	days = min(days, 365)
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -(days - 1))
	plays, err := s.Store.PlaysSince(r.Context(), start.Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summarizeUsage(plays, start, days))
}

func summarizeUsage(plays []store.Play, start time.Time, days int) usageView {
	var v usageView
	index := map[string]int{}
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i).Format("2006-01-02")
		index[d] = i
		v.Days = append(v.Days, usageDay{Day: d})
	}
	users, devices, titles := map[string]*usageRow{}, map[string]*usageRow{}, map[string]*usageRow{}
	add := func(m map[string]*usageRow, key, label, kind string, h float64) {
		row := m[key]
		if row == nil {
			row = &usageRow{Label: label, Kind: kind}
			m[key] = row
		}
		row.Plays++
		row.Hours += h
	}
	for _, p := range plays {
		h := p.WatchedSec / 3600
		day := time.Unix(p.StartedAt, 0).In(start.Location()).Format("2006-01-02")
		if i, ok := index[day]; ok {
			d := &v.Days[i]
			d.Plays++
			d.Hours += h
			switch p.Method {
			case "convert":
				d.Convert += h
			case "copy":
				d.Copy += h
			default:
				d.Direct += h
			}
		}
		switch p.Method {
		case "convert":
			v.Methods.Convert += h
		case "copy":
			v.Methods.Copy += h
		default:
			v.Methods.Direct += h
		}
		v.Plays++
		v.Hours += h
		add(users, p.Username, p.Username, "", h)
		name := p.DeviceName
		if name == "" {
			name = "Unnamed device"
		}
		add(devices, p.Device, name, "", h)
		add(titles, p.ItemID, p.Title, p.Kind, h)
	}
	flatten := func(m map[string]*usageRow, limit int) []usageRow {
		out := make([]usageRow, 0, len(m))
		for _, r := range m {
			r.Hours = round2(r.Hours)
			out = append(out, *r)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Hours != out[j].Hours {
				return out[i].Hours > out[j].Hours
			}
			return out[i].Plays > out[j].Plays
		})
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		return out
	}
	v.Users, v.Devices, v.Titles = flatten(users, 0), flatten(devices, 0), flatten(titles, 10)
	for i := range v.Days {
		d := &v.Days[i]
		d.Hours, d.Direct, d.Copy, d.Convert = round2(d.Hours), round2(d.Direct), round2(d.Copy), round2(d.Convert)
	}
	v.Hours = round2(v.Hours)
	v.Methods.Direct, v.Methods.Copy, v.Methods.Convert = round2(v.Methods.Direct), round2(v.Methods.Copy), round2(v.Methods.Convert)
	return v
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }
