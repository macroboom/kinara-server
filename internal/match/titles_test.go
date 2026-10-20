//go:build !nosqlite

package match

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"kinara/internal/parse"
	"kinara/internal/store"
	"kinara/internal/tmdb"
)

// strictTMDB searches a small catalog the way TMDB does: every word of the
// query must be in the title, punctuation ignored, and nothing is found for
// extra words like "01", "George Lucas" or "Theatrical".
type strictTMDB struct{ movies []tmdb.MovieResult }

func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func (f strictTMDB) SearchMovie(_ context.Context, q string, year int) ([]tmdb.MovieResult, error) {
	var out []tmdb.MovieResult
	for _, m := range f.movies {
		if year > 0 && tmdb.YearOf(m.ReleaseDate) != year {
			continue
		}
		have := map[string]bool{}
		for _, w := range words(m.Title) {
			have[w] = true
		}
		all := true
		for _, w := range words(q) {
			if !have[w] {
				all = false
			}
		}
		if all {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f strictTMDB) Movie(_ context.Context, id int) (tmdb.MovieDetails, error) {
	for _, m := range f.movies {
		if m.ID == id {
			return tmdb.MovieDetails{ID: id, Title: m.Title, ReleaseDate: m.ReleaseDate}, nil
		}
	}
	return tmdb.MovieDetails{}, errors.New("no such movie")
}
func (strictTMDB) SearchTV(context.Context, string, int) ([]tmdb.TVResult, error) { return nil, nil }
func (strictTMDB) TV(context.Context, int) (tmdb.TVDetails, error)                { return tmdb.TVDetails{}, nil }
func (strictTMDB) Season(context.Context, int, int) (tmdb.SeasonDetails, error) {
	return tmdb.SeasonDetails{}, nil
}
func (strictTMDB) FindIMDB(context.Context, string) (int, int, error)        { return 0, 0, nil }
func (strictTMDB) MovieCast(context.Context, int) ([]tmdb.CastMember, error) { return nil, nil }
func (strictTMDB) TVCast(context.Context, int) ([]tmdb.CastMember, error)    { return nil, nil }
func (strictTMDB) Image(string) string                                       { return "" }

// Names from a real collection that didn't match before.
func TestMatchCollectionNames(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	lib, _ := st.CreateLibrary(ctx, "Movies", "/m", store.KindMovies)
	src := strictTMDB{movies: []tmdb.MovieResult{
		{ID: 1893, Title: "Star Wars: Episode I - The Phantom Menace", ReleaseDate: "1999-05-19"},
		{ID: 11, Title: "Star Wars", ReleaseDate: "1977-05-25"},
		{ID: 1891, Title: "The Empire Strikes Back", ReleaseDate: "1980-05-20"},
		{ID: 49051, Title: "The Hobbit: An Unexpected Journey", ReleaseDate: "2012-12-12"},
		{ID: 557, Title: "Spider-Man", ReleaseDate: "2002-05-01"},
		{ID: 10681, Title: "WALL·E", ReleaseDate: "2008-06-22"},
		{ID: 389, Title: "12 Angry Men", ReleaseDate: "1957-04-10"},
		// Decoys a careless fallback would pick.
		{ID: 900, Title: "Air", ReleaseDate: "2012-01-01"},
		{ID: 901, Title: "Angry Men", ReleaseDate: "1957-01-01"},
	}}
	m := &Matcher{Store: st, Src: src, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	for rel, want := range map[string]int{
		"01 Star Wars Episode I The Phantom Menace - George Lucas (1999).mkv":      1893,
		"04 Star Wars Episode IV A New Hope - George Lucas (1977).mkv":             11,
		"05 Star Wars Episode V The Empire Strikes Back - George Lucas (1980).mkv": 1891,
		"The Hobbit- An Unexpected Journey (Theatrical) (2012).mkv":                49051,
		"The Hobbit- An Unexpected Journey (Theatrical).mkv":                       49051,
		"Spiderman 1 (2002).mkv":  557,
		"WALL-E (2008).mkv":       10681,
		"12 Angry Men (1957).mkv": 389,
		"Book 1 - Air (2012).mkv": 0,
	} {
		p := parse.Parse(rel)
		it := store.Item{LibraryID: lib.ID, Kind: store.ItemMovie, Title: p.Title, SortTitle: p.Title,
			ParsedTitle: parse.Normalize(p.Title), ParsedYear: p.Year, SeasonNumber: -1, EpisodeNumber: -1}
		if err := st.CreateItem(ctx, &it); err != nil {
			t.Fatal(err)
		}
		v := store.Version{LibraryID: lib.ID, ItemID: it.ID, Path: "/m/" + rel, RelPath: rel, Size: 1}
		if err := st.SaveVersion(ctx, &v, nil); err != nil {
			t.Fatal(err)
		}
		ok, err := m.matchMovie(ctx, it)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		// A second copy of a film is merged into the first, so look the
		// file's title up again.
		owner := it.ID
		if v, err := st.VersionByPath(ctx, "/m/"+rel); err == nil && v.ItemID != "" {
			owner = v.ItemID
		}
		got, _ := st.ExternalID(ctx, owner, store.ProviderTMDBMovie)
		if want == 0 {
			if ok || got != "" {
				t.Errorf("%s matched tmdb %s, want no match", rel, got)
			}
			continue
		}
		if !ok || got != strconv.Itoa(want) {
			t.Errorf("%s: matched %v, tmdb %q, want %d", rel, ok, got, want)
		}
	}
}

func TestVariants(t *testing.T) {
	for in, want := range map[string]string{
		"01 Star Wars Episode I The Phantom Menace - George Lucas": "Star Wars Episode I The Phantom Menace - George Lucas|Star Wars Episode I The Phantom Menace|The Phantom Menace|Star Wars",
		"Spiderman 1":    "Spiderman",
		"12 Angry Men":   "",
		"21 Jump Street": "",
		"1. Alien":       "Alien",
		"Book 1 - Air":   "Book 1",
	} {
		if got := strings.Join(variants(in), "|"); got != want {
			t.Errorf("variants(%q) = %q, want %q", in, got, want)
		}
	}
}
