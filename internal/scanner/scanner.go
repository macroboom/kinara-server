// Package scanner walks library folders, probes new or changed files, and
// files them under movies, shows, seasons, and episodes.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"kinora/internal/match"
	"kinora/internal/parse"
	"kinora/internal/probe"
	"kinora/internal/store"
)

// ErrBusy is returned when a library is already being scanned.
var ErrBusy = errors.New("scan already running")

// Progress describes the current or last scan of a library.
type Progress struct {
	LibraryID   int64  `json:"libraryId"`
	Running     bool   `json:"running"`
	Phase       string `json:"phase"` // walking, probing, matching, done, failed
	FilesSeen   int    `json:"filesSeen"`
	ToProbe     int    `json:"toProbe"` // new or changed files sent to ffprobe
	Probed      int    `json:"probed"`
	Disk        string `json:"disk"`    // SSD, hard drive, network drive, unknown drive
	Workers     int    `json:"workers"` // files probed at once
	New         int    `json:"new"`
	Changed     int    `json:"changed"`
	Skipped     int    `json:"skipped"`     // still copying, samples, extras
	DiscFolders int    `json:"discFolders"` // Blu-ray/DVD folders (BDMV, VIDEO_TS) skipped
	Unplaced    int    `json:"unplaced"`
	Inferred    int    `json:"inferred"` // episode numbers worked out from the folder
	Missing     int64  `json:"missing"`
	Errors      int    `json:"errors"`
	Matched     int    `json:"matched"`
	Unmatched   int    `json:"unmatched"`
	Message     string `json:"message,omitempty"`
	StartedAt   int64  `json:"startedAt"`
	FinishedAt  int64  `json:"finishedAt,omitempty"`
}

// Scanner scans libraries. Matcher may be nil when no TMDB key is set.
type Scanner struct {
	Store   *store.Store
	Prober  *probe.Prober
	Matcher *match.Matcher
	Log     *slog.Logger
	// StableAge is how long a file must go unmodified before it is scanned,
	// so half-copied files are never matched.
	StableAge time.Duration
	// Workers is how many ffprobe processes run at once. 0 picks it from the
	// kind of storage the library is on (see WorkersFor).
	Workers int
	// OnDone, if set, is called after each scan finishes (successfully or not).
	OnDone func(libraryID int64)

	mu       sync.Mutex
	progress map[int64]*Progress
}

// Progress returns a copy of a library's scan progress.
func (s *Scanner) Progress(libraryID int64) (Progress, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.progress[libraryID]
	if !ok {
		return Progress{LibraryID: libraryID}, false
	}
	return *p, true
}

func (s *Scanner) update(libraryID int64, f func(p *Progress)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s.progress[libraryID])
}

// Start begins a scan in the background. It returns ErrBusy if the library
// is already being scanned.
func (s *Scanner) Start(ctx context.Context, libraryID int64) error {
	s.mu.Lock()
	if s.progress == nil {
		s.progress = map[int64]*Progress{}
	}
	if p, ok := s.progress[libraryID]; ok && p.Running {
		s.mu.Unlock()
		return ErrBusy
	}
	s.progress[libraryID] = &Progress{LibraryID: libraryID, Running: true, Phase: "walking", StartedAt: time.Now().Unix()}
	s.mu.Unlock()

	go func() {
		err := s.run(ctx, libraryID)
		s.update(libraryID, func(p *Progress) {
			p.Running = false
			p.FinishedAt = time.Now().Unix()
			if err != nil {
				p.Phase = "failed"
				p.Message = err.Error()
			} else {
				p.Phase = "done"
			}
		})
		if err != nil {
			s.Log.Error("scan failed", "library", libraryID, "err", err)
		}
		if s.OnDone != nil {
			s.OnDone(libraryID)
		}
	}()
	return nil
}

// Run scans a library and waits for it to finish.
func (s *Scanner) Run(ctx context.Context, libraryID int64) (Progress, error) {
	if err := s.Start(ctx, libraryID); err != nil {
		return Progress{}, err
	}
	for {
		p, _ := s.Progress(libraryID)
		if !p.Running {
			if p.Phase == "failed" {
				return p, errors.New(p.Message)
			}
			return p, nil
		}
		select {
		case <-ctx.Done():
			return p, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type job struct {
	path, rel string
	size      int64
	mtime     int64
	existing  *store.Version
}

type result struct {
	job
	probe probe.Result
	err   error
}

func (s *Scanner) run(ctx context.Context, libraryID int64) error {
	lib, err := s.Store.GetLibrary(ctx, libraryID)
	if err != nil {
		return err
	}
	scanStart := time.Now().Unix()
	s.Log.Info("scan started", "library", lib.Name, "path", lib.Path)

	disk := DetectDisk(lib.Path)
	workers := s.Workers
	if workers <= 0 {
		workers = WorkersFor(disk)
	}
	s.update(libraryID, func(p *Progress) { p.Disk, p.Workers = string(disk), workers })
	s.Log.Info("scan storage", "library", lib.Name, "disk", disk, "workers", workers)
	known, err := s.Store.LibraryVersions(ctx, libraryID)
	if err != nil {
		return err
	}
	// Buffered so the walk keeps going while ffprobe works.
	jobs := make(chan job, 256)
	results := make(chan result, workers)

	// Probe in parallel; file results one at a time so new shows and
	// seasons are never created twice.
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				r, err := s.Prober.Probe(ctx, j.path)
				select {
				case results <- result{job: j, probe: r, err: err}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	walkErr := make(chan error, 1)
	var unchanged, dropped []string
	go func() {
		defer close(jobs)
		var err error
		unchanged, dropped, err = s.walk(ctx, lib, known, jobs)
		s.update(libraryID, func(p *Progress) {
			if p.Phase == "walking" {
				p.Phase = "probing"
			}
		})
		walkErr <- err
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	for r := range results {
		s.update(libraryID, func(p *Progress) { p.Probed++ })
		if err := s.file(ctx, lib, r); err != nil {
			s.Log.Warn("file failed", "path", r.rel, "err", err)
			s.update(libraryID, func(p *Progress) { p.Errors++ })
		}
	}
	if err := <-walkErr; err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := s.Store.TouchVersions(ctx, unchanged); err != nil {
		return err
	}
	if err := s.Store.ForgetVersions(ctx, dropped); err != nil {
		return err
	}

	missing, err := s.Store.MarkMissing(ctx, libraryID, scanStart)
	if err != nil {
		return err
	}
	inferred, err := s.inferEpisodes(ctx, lib)
	if err != nil {
		return err
	}
	s.update(libraryID, func(p *Progress) { p.Inferred = inferred })
	if err := s.Store.DeleteEmptyItems(ctx, libraryID); err != nil {
		return err
	}
	if err := s.Store.SetLibraryScanned(ctx, libraryID, time.Now().Unix()); err != nil {
		return err
	}
	s.update(libraryID, func(p *Progress) { p.Missing = missing; p.Phase = "matching" })

	if s.Matcher == nil {
		s.update(libraryID, func(p *Progress) {
			p.Message = "No TMDB key set: items keep their file-name titles until one is added."
		})
	} else {
		st, err := s.Matcher.MatchLibrary(ctx, libraryID)
		s.update(libraryID, func(p *Progress) { p.Matched, p.Unmatched = st.Matched, st.Unmatched })
		if err != nil {
			return fmt.Errorf("matching: %w", err)
		}
	}
	p, _ := s.Progress(libraryID)
	s.Log.Info("scan finished", "library", lib.Name, "files", p.FilesSeen, "new", p.New,
		"changed", p.Changed, "missing", p.Missing, "matched", p.Matched, "unmatched", p.Unmatched)
	return nil
}

var skipDirs = map[string]bool{
	"@eadir": true, "#recycle": true, "$recycle.bin": true, "lost+found": true,
	".trash": true, "system volume information": true,
}

// discDirs are the insides of a copied Blu-ray or DVD. Their .m2ts and .vob
// files are menus and stream fragments, not episodes, so they can't be
// placed. Rip them to a single file (MakeMKV) to add them.
var discDirs = map[string]bool{"bdmv": true, "video_ts": true, "certificate": true}

// walk sends new and changed files to jobs. It returns the IDs of files that
// haven't changed, so they can be marked as seen in one batch, and the IDs of
// known files that are now skipped (extras, samples, disc folders), so stale
// "can't place" records can be removed.
func (s *Scanner) walk(ctx context.Context, lib store.Library, known map[string]store.Version, jobs chan<- job) (unchanged, dropped []string, err error) {
	stable := s.StableAge
	if stable == 0 {
		stable = time.Minute
	}
	cutoff := time.Now().Add(-stable).Unix()
	forget := func(dir string) {
		prefix := dir + string(filepath.Separator)
		for p, v := range known {
			if strings.HasPrefix(p, prefix) {
				dropped = append(dropped, v.ID)
			}
		}
	}
	err = filepath.WalkDir(lib.Path, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if path == lib.Path {
				return fmt.Errorf("library folder: %w", err)
			}
			s.Log.Warn("walk", "path", path, "err", err)
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if path == lib.Path {
				return nil
			}
			lower := strings.ToLower(name)
			if discDirs[lower] {
				if lower != "certificate" {
					s.update(lib.ID, func(p *Progress) { p.DiscFolders++ })
					s.Log.Info("skipping disc folder", "path", path)
				}
				forget(path)
				return filepath.SkipDir
			}
			if strings.HasPrefix(name, ".") || skipDirs[lower] {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || !parse.IsVideo(name) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(lib.Path, path)
		s.update(lib.ID, func(p *Progress) { p.FilesSeen++ })
		parsed := parse.Parse(rel)
		if parsed.Sample || parsed.Extra {
			s.update(lib.ID, func(p *Progress) { p.Skipped++ })
			if v, ok := known[path]; ok {
				dropped = append(dropped, v.ID)
			}
			return nil
		}

		if info.ModTime().Unix() > cutoff {
			s.update(lib.ID, func(p *Progress) { p.Skipped++ })
			return nil // still being copied; picked up next scan
		}
		j := job{path: path, rel: rel, size: info.Size(), mtime: info.ModTime().Unix()}
		if v, ok := known[path]; ok {
			if v.Size == j.size && v.Mtime == j.mtime && v.ProbeError == "" {
				if v.ItemID != "" || s.placeFor(lib, parsed) == "" {
					unchanged = append(unchanged, v.ID)
					return nil
				}
				// Unplaced earlier; try again in case parsing improved.
			}
			j.existing = &v
		}
		s.update(lib.ID, func(p *Progress) { p.ToProbe++ })
		select {
		case jobs <- j:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	return unchanged, dropped, err
}

// placeFor says where a parsed file belongs in this library: "movie",
// "episode", or "" if it can't be placed.
func (s *Scanner) placeFor(lib store.Library, p parse.Result) string {
	if p.Sample || p.Extra {
		return ""
	}
	switch lib.Kind {
	case store.KindTV:
		if p.Kind == parse.Episode && p.Title != "" {
			return "episode"
		}
		return ""
	default:
		// A Movies folder: treat everything as a movie. A file that looks
		// like an episode will most likely end up unmatched for review.
		if p.Title != "" {
			return "movie"
		}
		return ""
	}
}

// file stores one probed file and links it to its item.
func (s *Scanner) file(ctx context.Context, lib store.Library, r result) error {
	p := parse.Parse(r.rel)
	if p.Sample || p.Extra {
		s.update(lib.ID, func(pr *Progress) { pr.Skipped++ })
		return nil
	}

	v := store.Version{LibraryID: lib.ID, Path: r.path, RelPath: r.rel, Size: r.size, Mtime: r.mtime}
	if r.existing != nil {
		v.ID = r.existing.ID
		v.ItemID = r.existing.ItemID
		v.AddedAt = r.existing.AddedAt
	}
	if r.err != nil {
		v.ProbeError = r.err.Error()
	} else {
		v.Container = r.probe.Container
		v.DurationSec = r.probe.DurationSec
		v.Bitrate = r.probe.Bitrate
		v.Width, v.Height = r.probe.Width, r.probe.Height
		v.VideoCodec = r.probe.VideoCodec
		v.HDR = r.probe.HDR
		v.DVProfile = r.probe.DVProfile
	}
	v.Part = p.Part

	if v.ItemID == "" {
		id, err := s.place(ctx, lib, p)
		if err != nil {
			return err
		}
		v.ItemID = id
		if p.Kind == parse.Episode && p.EpisodeEnd > p.Episode {
			v.EpisodeEnd = p.EpisodeEnd
		}
	}
	if err := s.Store.SaveVersion(ctx, &v, r.probe.Streams); err != nil {
		return err
	}
	s.update(lib.ID, func(pr *Progress) {
		switch {
		case r.existing == nil:
			pr.New++
		default:
			pr.Changed++
		}
		if v.ItemID == "" {
			pr.Unplaced++
		}
		if r.err != nil {
			pr.Errors++
		}
	})
	return nil
}

// place finds or creates the item a file belongs to. It returns "" when the
// file can't be placed (for example a TV library file with no episode number).
func (s *Scanner) place(ctx context.Context, lib store.Library, p parse.Result) (string, error) {
	switch s.placeFor(lib, p) {
	case "movie":
		return s.findOrCreate(ctx, store.Item{
			LibraryID: lib.ID, Kind: store.ItemMovie, Title: p.Title,
			SortTitle: parse.SortTitle(p.Title), Year: p.Year,
			ParsedTitle: parse.Normalize(p.Title), ParsedYear: p.Year,
			SeasonNumber: -1, EpisodeNumber: -1,
		}, store.ProviderTMDBMovie, p)
	case "episode":
		showID, err := s.findOrCreate(ctx, store.Item{
			LibraryID: lib.ID, Kind: store.ItemShow, Title: p.Title,
			SortTitle: parse.SortTitle(p.Title), Year: p.Year,
			ParsedTitle: parse.Normalize(p.Title), ParsedYear: p.Year,
			SeasonNumber: -1, EpisodeNumber: -1,
		}, store.ProviderTMDBTV, p)
		if err != nil {
			return "", err
		}
		seasonID, err := s.Store.FindSeason(ctx, showID, p.Season)
		if err != nil {
			return "", err
		}
		if seasonID == "" {
			title := fmt.Sprintf("Season %d", p.Season)
			if p.Season == 0 {
				title = "Specials"
			}
			season := store.Item{LibraryID: lib.ID, Kind: store.ItemSeason, ParentID: showID,
				Title: title, SortTitle: title, SeasonNumber: p.Season, EpisodeNumber: -1}
			if err := s.Store.CreateItem(ctx, &season); err != nil {
				return "", err
			}
			seasonID = season.ID
		}
		epID, err := s.Store.FindEpisode(ctx, seasonID, p.Episode)
		if err != nil {
			return "", err
		}
		if epID == "" {
			title := fmt.Sprintf("Episode %d", p.Episode)
			ep := store.Item{LibraryID: lib.ID, Kind: store.ItemEpisode, ParentID: seasonID,
				Title: title, SortTitle: title, SeasonNumber: p.Season, EpisodeNumber: p.Episode}
			if err := s.Store.CreateItem(ctx, &ep); err != nil {
				return "", err
			}
			epID = ep.ID
		}
		return epID, nil
	}
	return "", nil
}

// findOrCreate reuses the item an earlier file created for the same parsed
// title and year, or makes a new one and records any IDs from the path.
func (s *Scanner) findOrCreate(ctx context.Context, it store.Item, tmdbProvider string, p parse.Result) (string, error) {
	id, err := s.Store.FindByParsed(ctx, it.LibraryID, it.Kind, it.ParsedTitle, it.ParsedYear)
	if err != nil || id != "" {
		return id, err
	}
	if err := s.Store.CreateItem(ctx, &it); err != nil {
		return "", err
	}
	if p.TMDBID > 0 {
		if err := s.Store.SetExternalID(ctx, it.ID, tmdbProvider, fmt.Sprint(p.TMDBID)); err != nil {
			return "", err
		}
	}
	if p.IMDBID != "" {
		if err := s.Store.SetExternalID(ctx, it.ID, store.ProviderIMDB, p.IMDBID); err != nil {
			return "", err
		}
	}
	return it.ID, nil
}
