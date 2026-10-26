// Package match links library items to TMDB and fills in their metadata.
package match

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"kinara/internal/parse"
	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// Threshold is the score an item needs to be matched automatically.
// Below it, the item goes to the review list.
const Threshold = 0.6

// Source is the metadata provider. *tmdb.Client implements it.
type Source interface {
	SearchMovie(ctx context.Context, query string, year int) ([]tmdb.MovieResult, error)
	SearchTV(ctx context.Context, query string, year int) ([]tmdb.TVResult, error)
	Movie(ctx context.Context, id int) (tmdb.MovieDetails, error)
	TV(ctx context.Context, id int) (tmdb.TVDetails, error)
	Season(ctx context.Context, tvID, season int) (tmdb.SeasonDetails, error)
	FindIMDB(ctx context.Context, imdbID string) (movieID, tvID int, err error)
	MovieCast(ctx context.Context, id int) ([]tmdb.CastMember, error)
	TVCast(ctx context.Context, id int) ([]tmdb.CastMember, error)
	Image(path string) string
}

// Matcher matches pending items in a library.
type Matcher struct {
	Store *store.Store
	Src   Source
	Log   *slog.Logger
}

// Stats summarizes one matching run.
type Stats struct {
	Matched   int
	Unmatched int
	Errors    int
}

// ErrAuth means the metadata provider rejected the API key.
var ErrAuth = errors.New("metadata provider rejected the API key")

// MatchLibrary matches every pending movie and show, then fills in episode
// details for matched shows that gained new episodes.
func (m *Matcher) MatchLibrary(ctx context.Context, libraryID int64) (Stats, error) {
	var st Stats
	items, err := m.Store.PendingMatch(ctx, libraryID)
	if err != nil {
		return st, err
	}
	for _, it := range items {
		if ctx.Err() != nil {
			return st, ctx.Err()
		}
		var ok bool
		switch it.Kind {
		case store.ItemMovie:
			ok, err = m.matchMovie(ctx, it)
		case store.ItemShow:
			ok, err = m.matchShow(ctx, it)
		}
		switch {
		case isAuthError(err):
			return st, ErrAuth
		case err != nil:
			st.Errors++
			m.Log.Warn("match failed", "item", it.Title, "err", err)
		case ok:
			st.Matched++
		default:
			st.Unmatched++
		}
	}

	if err := m.fillCast(ctx, libraryID); err != nil {
		if isAuthError(err) {
			return st, ErrAuth
		}
		m.Log.Warn("cast lookup failed", "err", err)
	}

	if err := m.fillGenres(ctx, libraryID); err != nil {
		if isAuthError(err) {
			return st, ErrAuth
		}
		m.Log.Warn("genre lookup failed", "err", err)
	}

	if err := m.fillCollections(ctx, libraryID); err != nil {
		if isAuthError(err) {
			return st, ErrAuth
		}
		m.Log.Warn("franchise lookup failed", "err", err)
	}

	shows, err := m.Store.ShowsWithPendingEpisodes(ctx, libraryID)
	if err != nil {
		return st, err
	}
	for _, id := range shows {
		v, err := m.Store.ExternalID(ctx, id, store.ProviderTMDBTV)
		if err != nil || v == "" {
			continue
		}
		tvID, _ := strconv.Atoi(v)
		if err := m.refreshEpisodes(ctx, id, tvID); err != nil {
			if isAuthError(err) {
				return st, ErrAuth
			}
			st.Errors++
			m.Log.Warn("episode refresh failed", "show", id, "err", err)
		}
	}
	return st, nil
}

// castSize is how many cast members are kept per title.
const castSize = 15

// fillCast fetches cast for matched titles that have none yet (newly
// matched ones, and everything matched before cast was stored). A title
// TMDB has no cast for is recorded as fetched, so it isn't asked again.
func (m *Matcher) fillCast(ctx context.Context, libraryID int64) error {
	targets, err := m.Store.ItemsNeedingCast(ctx, libraryID, 500)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id, _ := strconv.Atoi(t.TMDBID)
		var cast []tmdb.CastMember
		if t.Kind == store.ItemMovie {
			cast, err = m.Src.MovieCast(ctx, id)
		} else {
			cast, err = m.Src.TVCast(ctx, id)
		}
		var se *tmdb.StatusError
		switch {
		case errors.As(err, &se) && se.Code == 404:
			cast = nil
		case err != nil:
			if isAuthError(err) {
				return err
			}
			m.Log.Warn("cast lookup", "item", t.ItemID, "err", err)
			continue
		}
		credits := make([]store.Credit, 0, min(len(cast), castSize))
		for _, c := range cast {
			if len(credits) == castSize {
				break
			}
			cr := store.Credit{Name: c.Name, Role: c.Character}
			if c.ProfilePath != "" {
				cr.PhotoURL = m.Src.Image(c.ProfilePath)
			}
			credits = append(credits, cr)
		}
		if err := m.Store.SetCredits(ctx, t.ItemID, credits); err != nil {
			return err
		}
	}
	return nil
}

// fillGenres fetches genres for titles matched before genres were stored.
func (m *Matcher) fillGenres(ctx context.Context, libraryID int64) error {
	targets, err := m.Store.ItemsNeedingGenres(ctx, libraryID, 1000)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		id, _ := strconv.Atoi(t.TMDBID)
		var genres []tmdb.Genre
		if t.Kind == store.ItemMovie {
			d, gerr := m.Src.Movie(ctx, id)
			genres, err = d.Genres, gerr
		} else {
			d, gerr := m.Src.TV(ctx, id)
			genres, err = d.Genres, gerr
		}
		var se *tmdb.StatusError
		switch {
		case errors.As(err, &se) && se.Code == 404:
			genres = nil
		case err != nil:
			if isAuthError(err) {
				return err
			}
			m.Log.Warn("genre lookup", "item", t.ItemID, "err", err)
			continue
		}
		if err := m.Store.SetGenres(ctx, t.ItemID, tmdb.GenreNames(genres)); err != nil {
			return err
		}
	}
	return nil
}

// collectionOf is a movie's franchise, as the store keeps it (nil: none).
func collectionOf(d tmdb.MovieDetails) *store.Collection {
	c := d.BelongsToCollection
	if c == nil || c.ID <= 0 || c.Name == "" {
		return nil
	}
	return &store.Collection{ID: c.ID, Name: c.Name, PosterPath: c.PosterPath, BackdropPath: c.BackdropPath}
}

// fillCollections looks up the franchise of movies matched before
// franchises were kept (newly matched ones get theirs when matched).
func (m *Matcher) fillCollections(ctx context.Context, libraryID int64) error {
	targets, err := m.Store.MoviesNeedingCollection(ctx, libraryID, 1000)
	if err != nil {
		return err
	}
	for itemID, id := range targets {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d, err := m.Src.Movie(ctx, id)
		var se *tmdb.StatusError
		switch {
		case errors.As(err, &se) && se.Code == 404:
		case err != nil:
			if isAuthError(err) {
				return err
			}
			m.Log.Warn("franchise lookup", "item", itemID, "err", err)
			continue
		}
		if err := m.Store.SetCollection(ctx, itemID, collectionOf(d)); err != nil {
			return err
		}
	}
	return nil
}

// MatchItem matches a single item now, for example after a manual fix.
func (m *Matcher) MatchItem(ctx context.Context, it store.Item) (bool, error) {
	var ok bool
	var err error
	switch it.Kind {
	case store.ItemMovie:
		ok, err = m.matchMovie(ctx, it)
	case store.ItemShow:
		ok, err = m.matchShow(ctx, it)
	default:
		return false, fmt.Errorf("cannot match a %s directly", it.Kind)
	}
	if ok && err == nil {
		// Fetch the cast for the new match now rather than at the next scan.
		if cerr := m.fillCast(ctx, it.LibraryID); cerr != nil {
			m.Log.Warn("cast lookup failed", "err", cerr)
		}
	}
	return ok, err
}

func isAuthError(err error) bool {
	var se *tmdb.StatusError
	return errors.As(err, &se) && se.Code == 401
}

type candidate struct {
	id         int
	title      string
	orig       string
	year       int
	popularity float64
	countries  []string // TV only: ISO codes such as GB, US
	score      float64
}

func (m *Matcher) matchMovie(ctx context.Context, it store.Item) (bool, error) {
	id, conf, err := m.knownID(ctx, it, store.ProviderTMDBMovie, true)
	if err != nil {
		return false, err
	}
	if id == 0 {
		var best *candidate
		var cands []candidate
		var tried []string
		for _, q := range withSplits(m.queries(ctx, it)) {
			res, err := m.Src.SearchMovie(ctx, q.text, it.ParsedYear)
			if err == nil && len(res) == 0 && it.ParsedYear > 0 {
				res, err = m.Src.SearchMovie(ctx, q.text, 0)
			}
			if err != nil {
				return false, err
			}
			tried = append(tried, fmt.Sprintf("%q: %d results", q.text, len(res)))
			cs := make([]candidate, 0, len(res))
			for _, r := range res {
				cs = append(cs, candidate{id: r.ID, title: r.Title, orig: r.OriginalTitle,
					year: tmdb.YearOf(r.ReleaseDate), popularity: r.Popularity})
			}
			b, _ := rank(cs, parse.Normalize(q.title), it.ParsedYear)
			if b != nil && (best == nil || b.score > best.score) {
				best, cands = b, cs
			}
			if best != nil && best.score >= Threshold {
				break
			}
		}
		if best == nil || best.score < Threshold {
			m.logNoMatch(it, tried, best)
			return false, m.unmatched(ctx, it, best)
		}
		if len(cands) > 1 && cands[1].score >= Threshold && cands[0].score-cands[1].score < 0.05 {
			// Same-name titles: compare the file's length to each candidate's runtime.
			if pick := m.runtimeTiebreak(ctx, it, cands[0], cands[1]); pick != nil {
				best = pick
			}
		}
		id, conf = best.id, math.Min(best.score, 1)
	}

	d, err := m.Src.Movie(ctx, id)
	if err != nil {
		return false, err
	}
	it.Title = d.Title
	it.SortTitle = parse.SortTitle(d.Title)
	it.Year = tmdb.YearOf(d.ReleaseDate)
	it.Overview = d.Overview
	it.Rating = d.VoteAverage
	it.RuntimeMin = d.Runtime
	it.AirDate = d.ReleaseDate
	it.MatchState = store.MatchMatched
	it.MatchConfidence = conf
	if err := m.Store.UpdateItemMetadata(ctx, it); err != nil {
		return false, err
	}
	if err := m.Store.SetExternalID(ctx, it.ID, store.ProviderTMDBMovie, strconv.Itoa(id)); err != nil {
		return false, err
	}
	// A new match (including a manual fix) gets its own cast.
	if err := m.Store.ResetCredits(ctx, it.ID); err != nil {
		return false, err
	}
	if d.IMDBID != "" {
		_ = m.Store.SetExternalID(ctx, it.ID, store.ProviderIMDB, d.IMDBID)
	}
	if err := m.Store.SetGenres(ctx, it.ID, tmdb.GenreNames(d.Genres)); err != nil {
		return false, err
	}
	if err := m.Store.SetCollection(ctx, it.ID, collectionOf(d)); err != nil {
		return false, err
	}
	m.setImage(ctx, it.ID, "poster", d.PosterPath)
	m.setImage(ctx, it.ID, "backdrop", d.BackdropPath)

	// Another folder already holds this movie (a 4K and a 1080p copy):
	// merge so it shows once with two versions.
	if dup, err := m.Store.FindByExternalID(ctx, it.LibraryID, store.ItemMovie,
		store.ProviderTMDBMovie, strconv.Itoa(id), it.ID); err == nil && dup != "" {
		if err := m.Store.MergeItem(ctx, it.ID, dup); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (m *Matcher) matchShow(ctx context.Context, it store.Item) (bool, error) {
	id, conf, err := m.knownID(ctx, it, store.ProviderTMDBTV, false)
	if err != nil {
		return false, err
	}
	if id == 0 {
		var best *candidate
		qs := m.queries(ctx, it)
		country := ""
		for _, q := range qs {
			if _, cc := splitCountry(q); cc != "" {
				country = cc
				break
			}
		}
		var tried []string
		for _, q := range withSplits(qs) {
			res, err := m.Src.SearchTV(ctx, q.text, it.ParsedYear)
			if err == nil && len(res) == 0 && it.ParsedYear > 0 {
				res, err = m.Src.SearchTV(ctx, q.text, 0)
			}
			if err != nil {
				return false, err
			}
			tried = append(tried, fmt.Sprintf("%q: %d results", q.text, len(res)))
			cs := make([]candidate, 0, len(res))
			for _, r := range res {
				cs = append(cs, candidate{id: r.ID, title: r.Name, orig: r.OriginalName,
					year: tmdb.YearOf(r.FirstAirDate), popularity: r.Popularity, countries: r.OriginCountry})
			}
			base, _ := splitCountry(q.title)
			b := rankCountry(cs, parse.Normalize(base), it.ParsedYear, country)
			if b != nil && (best == nil || b.score > best.score) {
				best = b
			}
			if best != nil && best.score >= Threshold {
				break
			}
		}
		if best == nil || best.score < Threshold {
			m.logNoMatch(it, tried, best)
			return false, m.unmatched(ctx, it, best)
		}
		id, conf = best.id, math.Min(best.score, 1)
	}

	d, err := m.Src.TV(ctx, id)
	if err != nil {
		return false, err
	}
	it.Title = d.Name
	it.SortTitle = parse.SortTitle(d.Name)
	it.Year = tmdb.YearOf(d.FirstAirDate)
	it.Overview = d.Overview
	it.Rating = d.VoteAverage
	it.AirDate = d.FirstAirDate
	if len(d.EpisodeRunTime) > 0 {
		it.RuntimeMin = d.EpisodeRunTime[0]
	}
	it.MatchState = store.MatchMatched
	it.MatchConfidence = conf
	if err := m.Store.UpdateItemMetadata(ctx, it); err != nil {
		return false, err
	}
	if err := m.Store.SetExternalID(ctx, it.ID, store.ProviderTMDBTV, strconv.Itoa(id)); err != nil {
		return false, err
	}
	// A new match (including a manual fix) gets its own cast.
	if err := m.Store.ResetCredits(ctx, it.ID); err != nil {
		return false, err
	}
	if err := m.Store.SetGenres(ctx, it.ID, tmdb.GenreNames(d.Genres)); err != nil {
		return false, err
	}
	m.setImage(ctx, it.ID, "poster", d.PosterPath)
	m.setImage(ctx, it.ID, "backdrop", d.BackdropPath)

	showID := it.ID
	if dup, err := m.Store.FindByExternalID(ctx, it.LibraryID, store.ItemShow,
		store.ProviderTMDBTV, strconv.Itoa(id), it.ID); err == nil && dup != "" {
		if err := m.Store.MergeItem(ctx, it.ID, dup); err != nil {
			return true, err
		}
		showID = dup
	}
	return true, m.refreshEpisodes(ctx, showID, id)
}

// queries lists search terms to try, best first: the folder title cleaned
// of season ranges and junk, then the title from the file names inside
// (folders like "teneighty-dta" or "Show S01-S02 br" often hold well-named
// files).
func (m *Matcher) queries(ctx context.Context, it store.Item) []string {
	var rel string
	switch it.Kind {
	case store.ItemShow:
		rel, _ = m.Store.ShowFilePath(ctx, it.ID)
	case store.ItemMovie:
		if vs, err := m.Store.VersionsForItem(ctx, it.ID); err == nil && len(vs) > 0 {
			rel = vs[0].RelPath
		}
	}
	return buildQueries(it.Title, rel)
}

// buildQueries lists the searches for an item titled title, one of whose
// files is at rel (relative to the library, may be empty).
func buildQueries(title, rel string) []string {
	var out []string
	var add func(q string)
	add = func(q string) {
		q = strings.TrimSpace(q)
		if q == "" {
			return
		}
		for _, have := range out {
			if parse.Normalize(have) == parse.Normalize(q) {
				return
			}
		}
		out = append(out, q)
		// "Coupling UK": TMDB calls it "Coupling", so also search without
		// the country (matchShow then prefers shows from that country).
		if base, cc := splitCountry(q); cc != "" {
			add(base)
		}
		for _, v := range variants(q) {
			add(v)
		}
	}
	add(parse.CleanTitle(title))

	if rel != "" {
		rel = strings.ReplaceAll(rel, `\`, "/")
		// The folders holding the file, nearest first, then the file name.
		if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
			segs := strings.Split(dir, "/")
			for i := len(segs) - 1; i >= 0; i-- {
				if t := parse.CleanTitle(segs[i]); !strings.HasPrefix(strings.ToLower(t), "season") {
					add(t)
				}
			}
		}
		add(parse.Parse(filepath.Base(rel)).Title)
	}
	return out
}

var (
	// "01 Star Wars", "1. Alien", "03 - Heat": a collection's order number.
	// A plain number without a leading zero or a dot or dash after it is
	// kept, since it's usually the title ("12 Angry Men", "21 Jump Street").
	reIndex = regexp.MustCompile(`^(?:0\d{1,2}|\d{1,3}\s*[.\-])\s*(\pL.*)$`)
	// "Title - George Lucas", "Title - 1080p rip": something after a spaced
	// dash. Only the part before is tried; the part after is too often a
	// short word that would match some other film ("Book 1 - Air").
	reCredit = regexp.MustCompile(`^(.+?\S)\s+-\s+\S.*$`)
	// "Star Wars Episode IV A New Hope": TMDB names some of these by the
	// subtitle ("The Empire Strikes Back") and some by the series name
	// ("Star Wars" for 1977).
	reEpisodeOf = regexp.MustCompile(`(?i)^(.+?)\s+episode\s+(?:[ivx]{1,4}|\d{1,2})\b\s*(.*)$`)
	// "Spiderman 1": the first film rarely has the 1 in its real title.
	reTrailingOne = regexp.MustCompile(`^(.*\pL)\s+1$`)
)

// variants lists fallback searches for a title that may carry a collection
// number, a credit, or a series name. They are tried after the title as
// written, so a real title like "12 Angry Men" is found first.
func variants(q string) []string {
	var out []string
	t := q
	if m := reIndex.FindStringSubmatch(t); m != nil {
		t = m[1]
		out = append(out, t)
	}
	if m := reCredit.FindStringSubmatch(t); m != nil {
		t = m[1]
		out = append(out, t)
	} else if m := reTrailingOne.FindStringSubmatch(t); m != nil {
		out = append(out, m[1])
	}
	if m := reEpisodeOf.FindStringSubmatch(t); m != nil {
		if rest := strings.TrimSpace(strings.Trim(m[2], " -:")); rest != "" {
			out = append(out, rest)
		}
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// knownID returns an ID given in the file names ({tmdb-603}, an IMDb ID) or
// set by a manual fix, skipping the search.
func (m *Matcher) knownID(ctx context.Context, it store.Item, provider string, movie bool) (int, float64, error) {
	if v, err := m.Store.ExternalID(ctx, it.ID, provider); err != nil {
		return 0, 0, err
	} else if v != "" {
		id, _ := strconv.Atoi(v)
		return id, 1, nil
	}
	imdb, err := m.Store.ExternalID(ctx, it.ID, store.ProviderIMDB)
	if err != nil || imdb == "" {
		return 0, 0, err
	}
	movieID, tvID, err := m.Src.FindIMDB(ctx, imdb)
	if err != nil {
		return 0, 0, err
	}
	if movie {
		return movieID, 1, nil
	}
	return tvID, 1, nil
}

// logNoMatch records what was searched for a title that stayed unmatched, so
// the reason shows in the server log.
func (m *Matcher) logNoMatch(it store.Item, tried []string, best *candidate) {
	args := []any{"item", it.Title, "searches", strings.Join(tried, "; ")}
	if best != nil {
		args = append(args, "best", fmt.Sprintf("%s (%d), tmdb %d, score %.2f", best.title, best.year, best.id, best.score))
	}
	m.Log.Info("no confident match", args...)
}

func (m *Matcher) unmatched(ctx context.Context, it store.Item, best *candidate) error {
	conf := 0.0
	if best != nil {
		conf = best.score
	}
	return m.Store.SetMatchState(ctx, it.ID, store.MatchUnmatched, conf)
}

// runtimeTiebreak picks between two close candidates by comparing the file's
// duration to each candidate's listed runtime. It returns nil if it can't tell.
func (m *Matcher) runtimeTiebreak(ctx context.Context, it store.Item, a, b candidate) *candidate {
	versions, err := m.Store.VersionsForItem(ctx, it.ID)
	if err != nil || len(versions) == 0 || versions[0].DurationSec <= 0 {
		return nil
	}
	fileMin := versions[0].DurationSec / 60
	da, errA := m.Src.Movie(ctx, a.id)
	db, errB := m.Src.Movie(ctx, b.id)
	if errA != nil || errB != nil || da.Runtime == 0 || db.Runtime == 0 {
		return nil
	}
	diffA := math.Abs(float64(da.Runtime) - fileMin)
	diffB := math.Abs(float64(db.Runtime) - fileMin)
	if math.Abs(diffA-diffB) < 5 {
		return nil
	}
	if diffA < diffB {
		return &a
	}
	return &b
}

func (m *Matcher) refreshEpisodes(ctx context.Context, showID string, tvID int) error {
	seasons, err := m.Store.Children(ctx, showID)
	if err != nil {
		return err
	}
	for _, season := range seasons {
		if season.Kind != store.ItemSeason {
			continue
		}
		episodes, err := m.Store.Children(ctx, season.ID)
		if err != nil {
			return err
		}
		sd, err := m.Src.Season(ctx, tvID, season.SeasonNumber)
		if err != nil {
			var se *tmdb.StatusError
			if errors.As(err, &se) && se.Code == 404 {
				// TMDB doesn't know this season: keep file-name titles.
				for _, e := range episodes {
					_ = m.Store.SetMatchState(ctx, e.ID, store.MatchUnmatched, 0)
				}
				continue
			}
			return err
		}
		if sd.Name != "" {
			season.Title = sd.Name
			season.SortTitle = sd.Name
		}
		season.Overview = sd.Overview
		season.AirDate = sd.AirDate
		season.Year = tmdb.YearOf(sd.AirDate)
		season.MatchState = store.MatchMatched
		season.MatchConfidence = 1
		if err := m.Store.UpdateItemMetadata(ctx, season); err != nil {
			return err
		}
		m.setImage(ctx, season.ID, "poster", sd.PosterPath)

		byNumber := make(map[int]int, len(sd.Episodes))
		for i, e := range sd.Episodes {
			byNumber[e.EpisodeNumber] = i
		}
		for _, ep := range episodes {
			i, ok := byNumber[ep.EpisodeNumber]
			if !ok {
				_ = m.Store.SetMatchState(ctx, ep.ID, store.MatchUnmatched, 0)
				continue
			}
			e := sd.Episodes[i]
			if e.Name != "" {
				ep.Title = e.Name
				ep.SortTitle = e.Name
			}
			ep.Overview = e.Overview
			ep.AirDate = e.AirDate
			ep.Year = tmdb.YearOf(e.AirDate)
			ep.RuntimeMin = e.Runtime
			ep.Rating = e.VoteAverage
			ep.MatchState = store.MatchMatched
			ep.MatchConfidence = 1
			if err := m.Store.UpdateItemMetadata(ctx, ep); err != nil {
				return err
			}
			_ = m.Store.SetExternalID(ctx, ep.ID, store.ProviderTMDBEpisode, strconv.Itoa(e.ID))
			m.setImage(ctx, ep.ID, "still", e.StillPath)
		}
	}
	return nil
}

func (m *Matcher) setImage(ctx context.Context, itemID, kind, path string) {
	if path == "" {
		return
	}
	if err := m.Store.SetImage(ctx, itemID, kind, m.Src.Image(path)); err != nil {
		m.Log.Warn("save image", "item", itemID, "err", err)
	}
}

// rank scores candidates against the parsed title and year, sorts them best
// first, and reports whether the top two are too close to call.
func rank(cands []candidate, parsedTitle string, parsedYear int) (*candidate, bool) {
	if len(cands) == 0 {
		return nil, false
	}
	for i := range cands {
		cands[i].score = score(parsedTitle, parsedYear, cands[i])
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	ambiguous := len(cands) > 1 && cands[1].score >= Threshold && cands[0].score-cands[1].score < 0.05
	return &cands[0], ambiguous
}

// search is one TMDB search: text is sent to TMDB, title is what results are
// scored against. They differ only for split guesses (see withSplits).
type search struct{ text, title string }

// withSplits adds, after the normal queries, guesses for titles written as
// one word: "BLUELOCK" is searched as "blu elock", "blue lock" and so on,
// because TMDB's search doesn't join words. Results are still scored
// against "bluelock", so only a title that is the same word with spaces
// ("Blue Lock") can match. The loop stops at the first good match, so these
// extra searches only run for titles nothing else found.
func withSplits(qs []string) []search {
	out := make([]search, 0, len(qs))
	for _, q := range qs {
		out = append(out, search{q, q})
	}
	for _, q := range qs {
		w := parse.Normalize(q)
		if len(strings.Fields(q)) != 1 || len(w) < 6 || len(w) > 16 || !isLetters(w) {
			continue
		}
		for i := 3; i <= len(w)-3; i++ {
			out = append(out, search{w[:i] + " " + w[i:], q})
		}
	}
	return out
}

func isLetters(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// countryCodes maps a trailing country tag in a show folder to TMDB's
// origin_country code.
var countryCodes = map[string]string{"UK": "GB", "US": "US", "AU": "AU", "NZ": "NZ", "CA": "CA", "IE": "IE"}

// splitCountry splits "Coupling UK" into "Coupling" and "GB". The tag must be
// in capitals so a title like "This Is Us" is left alone.
func splitCountry(title string) (base, country string) {
	title = strings.TrimSpace(title)
	i := strings.LastIndexByte(title, ' ')
	if i <= 0 {
		return title, ""
	}
	cc, ok := countryCodes[title[i+1:]]
	if !ok {
		return title, ""
	}
	return strings.TrimSpace(title[:i]), cc
}

// rankCountry is rank with a nudge toward shows from country (when set):
// "Coupling UK" should beat the American remake with the same name.
func rankCountry(cands []candidate, parsedTitle string, parsedYear int, country string) *candidate {
	if len(cands) == 0 {
		return nil
	}
	for i := range cands {
		cands[i].score = score(parsedTitle, parsedYear, cands[i])
		if country != "" && len(cands[i].countries) > 0 {
			if slices.Contains(cands[i].countries, country) {
				cands[i].score += 0.05
			} else {
				cands[i].score -= 0.1
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].score > cands[j].score })
	return &cands[0]
}

// score rates a candidate from 0 to about 1.03:
// 0.75 for the title, up to 0.25 for the year, and a tiny popularity nudge
// so a famous film beats an obscure one with the same name and year.
func score(parsedTitle string, parsedYear int, c candidate) float64 {
	t := math.Max(similarity(parsedTitle, parse.Normalize(c.title)),
		similarity(parsedTitle, parse.Normalize(c.orig)))
	t = math.Max(t, suffixMatch(parsedTitle, parse.Normalize(c.title)))
	s := 0.75 * t
	switch d := parsedYear - c.year; {
	case parsedYear == 0 || c.year == 0:
		s += 0.1
	case d == 0:
		s += 0.25
	case d == 1 || d == -1:
		s += 0.15
	default:
		s -= 0.2
	}
	return s + math.Min(c.popularity/2000, 0.03)
}

// suffixMatch handles folder names with an extra prefix, such as
// "Star Wars Andor" for the show "Andor": when the candidate's title is the
// whole ending of the parsed title, it counts as a good (not perfect) match.
// Only a suffix counts, so "Star Wars Andor" never matches "Star Wars".
func suffixMatch(parsed, cand string) float64 {
	if cand == "" || parsed == cand || !strings.HasSuffix(parsed, " "+cand) {
		return 0
	}
	return 0.82
}

// similarity compares two normalized titles: 1 when equal, otherwise the
// share of words they have in common.
func similarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1
	}
	// "chainsawman" and "chainsaw man" are the same title.
	if strings.ReplaceAll(a, " ", "") == strings.ReplaceAll(b, " ", "") {
		return 0.97
	}
	ta, tb := strings.Fields(a), strings.Fields(b)
	counts := make(map[string]int, len(ta))
	for _, w := range ta {
		counts[w]++
	}
	common := 0
	for _, w := range tb {
		if counts[w] > 0 {
			counts[w]--
			common++
		}
	}
	return 0.95 * 2 * float64(common) / float64(len(ta)+len(tb))
}
