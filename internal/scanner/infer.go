package scanner

import (
	"context"
	"path/filepath"
	"slices"

	"kinara/internal/parse"
	"kinara/internal/store"
)

// inferEpisodes places files named with a season but no episode number when
// the folder makes the answer obvious. Scene releases sometimes name the
// first episode after the whole pack:
//
//	Scavengers.Reign.S01.1080p.MAX.WEB-DL.H.264-FLUX.mkv   <- E01
//	Scavengers.Reign.S01E02.The.Storm.1080p...mkv
//	...
//	Scavengers.Reign.S01E12.The.Reunion.1080p...mkv
//
// A file is placed only when all of these hold:
//   - it is the only such file in its folder,
//   - at least two other files in the folder are episodes of that season,
//   - exactly one episode number between 1 and the highest is missing,
//   - its length is in line with the others (so a whole season packed into
//     one file is never mistaken for an episode).
func (s *Scanner) inferEpisodes(ctx context.Context, lib store.Library) (int, error) {
	if lib.Kind != store.KindTV {
		return 0, nil
	}
	unplaced, err := s.Store.UnlinkedVersions(ctx, lib.ID, 100000)
	if err != nil {
		return 0, err
	}
	byDir := map[string][]store.Version{}
	for _, v := range unplaced {
		if v.ProbeError != "" {
			continue
		}
		if _, ok := parse.SeasonOnly(v.RelPath); ok {
			dir := filepath.Dir(v.Path)
			byDir[dir] = append(byDir[dir], v)
		}
	}
	if len(byDir) == 0 {
		return 0, nil
	}
	all, err := s.Store.LibraryVersions(ctx, lib.ID)
	if err != nil {
		return 0, err
	}
	siblings := map[string][]store.Version{}
	for p, v := range all {
		dir := filepath.Dir(p)
		if _, ok := byDir[dir]; ok && v.ItemID != "" && !v.Missing {
			siblings[dir] = append(siblings[dir], v)
		}
	}

	placed := 0
	for dir, files := range byDir {
		if len(files) != 1 {
			continue
		}
		v := files[0]
		season, _ := parse.SeasonOnly(v.RelPath)
		p, ok := inferFrom(season, v, siblings[dir])
		if !ok {
			continue
		}
		itemID, err := s.place(ctx, lib, p, v.RelPath)
		if err != nil {
			return placed, err
		}
		if itemID == "" {
			continue
		}
		if err := s.Store.SetVersionItem(ctx, v.ID, itemID); err != nil {
			return placed, err
		}
		s.Log.Info("episode number inferred from folder", "path", v.RelPath,
			"season", p.Season, "episode", p.Episode)
		placed++
	}
	return placed, nil
}

// inferFrom works out the episode for file v from its placed siblings. The
// result reuses a sibling's parsed show title and year, so the file joins
// the same show.
func inferFrom(season int, v store.Version, sibs []store.Version) (parse.Result, bool) {
	var eps []int
	var durations []float64
	var show parse.Result
	for _, sv := range sibs {
		sp := parse.Parse(sv.RelPath)
		if sp.Kind != parse.Episode || sp.Season != season || sp.Episode <= 0 {
			continue
		}
		show = sp
		for e := sp.Episode; e <= max(sp.EpisodeEnd, sp.Episode); e++ {
			eps = append(eps, e)
		}
		if sv.DurationSec > 0 {
			durations = append(durations, sv.DurationSec)
		}
	}
	if len(eps) < 2 {
		return parse.Result{}, false
	}
	top := slices.Max(eps)
	missing := 0
	for e := 1; e <= top; e++ {
		if !slices.Contains(eps, e) {
			if missing != 0 {
				return parse.Result{}, false // more than one gap: can't tell
			}
			missing = e
		}
	}
	if missing == 0 {
		return parse.Result{}, false
	}
	if v.DurationSec > 0 && len(durations) > 0 {
		lo, hi := slices.Min(durations), slices.Max(durations)
		if v.DurationSec > hi*1.5 || v.DurationSec < lo*0.5 {
			return parse.Result{}, false
		}
	}
	show.Episode, show.EpisodeEnd = missing, missing
	show.Part = 0
	return show, true
}
