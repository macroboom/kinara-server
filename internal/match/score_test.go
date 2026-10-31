package match

import (
	"testing"

	"kinara/internal/parse"
)

func TestRankPrefersTitleAndYear(t *testing.T) {
	cands := []candidate{
		{id: 1, title: "Night Shift", year: 1982, popularity: 30},
		{id: 2, title: "Night Shift", year: 2023, popularity: 5},
		{id: 3, title: "The Night Shift", year: 2014, popularity: 80},
	}
	best, _ := rank(cands, parse.Normalize("Night Shift"), 2023)
	if best == nil || best.id != 2 || best.score < Threshold {
		t.Fatalf("want id 2 above threshold, got %+v", best)
	}
}

func TestRankWithoutYear(t *testing.T) {
	cands := []candidate{
		{id: 10, title: "Second Light", year: 2021, popularity: 3},
		{id: 11, title: "Light", year: 2019, popularity: 50},
	}
	best, _ := rank(cands, parse.Normalize("Second Light"), 0)
	if best.id != 10 || best.score < Threshold {
		t.Fatalf("got %+v", best)
	}
}

func TestRankRejectsPoorMatch(t *testing.T) {
	cands := []candidate{{id: 1, title: "Completely Different Film", year: 2001}}
	best, _ := rank(cands, parse.Normalize("Meridian"), 2024)
	if best.score >= Threshold {
		t.Fatalf("poor match scored %v", best.score)
	}
}

func TestRankAmbiguous(t *testing.T) {
	cands := []candidate{
		{id: 1, title: "Meridian", year: 2024},
		{id: 2, title: "Meridian", year: 2024},
	}
	_, ambiguous := rank(cands, parse.Normalize("Meridian"), 2024)
	if !ambiguous {
		t.Fatal("two identical candidates should be ambiguous")
	}
}

func TestOriginalTitleCounts(t *testing.T) {
	cands := []candidate{{id: 5, title: "The Lives of Others", orig: "Das Leben der Anderen", year: 2006}}
	best, _ := rank(cands, parse.Normalize("Das Leben der Anderen"), 2006)
	if best.score < Threshold {
		t.Fatalf("original title should match, score %v", best.score)
	}
}

func TestSimilarity(t *testing.T) {
	if similarity("night shift", "night shift") != 1 {
		t.Error("equal titles should score 1")
	}
	if s := similarity("night shift", "night"); s <= 0 || s >= 1 {
		t.Errorf("partial overlap = %v", s)
	}
	if similarity("", "x") != 0 {
		t.Error("empty title should score 0")
	}
}

func TestRealWorldTitles(t *testing.T) {
	cases := []struct {
		folder string
		cand   candidate
	}{
		{"90 Day Fiance", candidate{id: 1, title: "90 Day Fiancé", year: 2014}},
		{"Im Sorry", candidate{id: 2, title: "I'm Sorry", year: 2017}},
		{"Star Wars Andor", candidate{id: 3, title: "Andor", year: 2022}},
		{parse.CleanTitle("Pushing Daisies S01-S02 br"), candidate{id: 4, title: "Pushing Daisies", year: 2007}},
		{parse.CleanTitle("Detroiters S01-S02"), candidate{id: 5, title: "Detroiters", year: 2017}},
	}
	for _, c := range cases {
		best, _ := rank([]candidate{c.cand}, parse.Normalize(c.folder), 0)
		if best.score < Threshold {
			t.Errorf("%q vs %q scored %.2f", c.folder, c.cand.title, best.score)
		}
	}
	// Given both, the show named by the ending beats the franchise name.
	best, _ := rank([]candidate{
		{id: 9, title: "Star Wars", year: 1977, popularity: 100},
		{id: 3, title: "Andor", year: 2022},
	}, parse.Normalize("Star Wars Andor"), 0)
	if best.id != 3 {
		t.Errorf("want Andor, got %q (%.2f)", best.title, best.score)
	}
}

func TestSplitCountry(t *testing.T) {
	cases := []struct{ in, base, cc string }{
		{"Coupling UK", "Coupling", "GB"},
		{"The Office US", "The Office", "US"},
		{"This Is Us", "This Is Us", ""},
		{"Castle", "Castle", ""},
	}
	for _, c := range cases {
		base, cc := splitCountry(c.in)
		if base != c.base || cc != c.cc {
			t.Errorf("splitCountry(%q) = %q, %q; want %q, %q", c.in, base, cc, c.base, c.cc)
		}
	}
}

func TestCountryPrefersOrigin(t *testing.T) {
	cands := []candidate{
		{id: 1, title: "Coupling", year: 2003, popularity: 40, countries: []string{"US"}},
		{id: 2, title: "Coupling", year: 2000, popularity: 20, countries: []string{"GB"}},
	}
	best := rankCountry(cands, "coupling", 0, "GB")
	if best == nil || best.id != 2 || best.score < Threshold {
		t.Fatalf("got %+v, want the British Coupling above threshold", best)
	}
}

func TestCompactTitles(t *testing.T) {
	if s := similarity("chainsawman", "chainsaw man"); s < 0.9 {
		t.Errorf("chainsawman vs chainsaw man = %.2f, want a near match", s)
	}
}

func TestWithSplits(t *testing.T) {
	got := withSplits([]string{"BLUELOCK"})
	found := false
	for _, q := range got {
		if q.text == "blue lock" && q.title == "BLUELOCK" {
			found = true
		}
	}
	if !found || got[0].text != "BLUELOCK" {
		t.Fatalf("got %+v, want the original first and a \"blue lock\" guess", got)
	}
	if n := len(withSplits([]string{"The Office"})); n != 1 {
		t.Errorf("split a multi-word title into %d searches", n)
	}
	// Scored against the joined word, "Blue Lock" is a match and an unrelated
	// show that happens to contain "blue" is not.
	best, _ := rank([]candidate{
		{id: 1, title: "Blue Bloods", popularity: 200},
		{id: 2, title: "Blue Lock", popularity: 50},
	}, parse.Normalize("BLUELOCK"), 0)
	if best.id != 2 || best.score < Threshold {
		t.Errorf("got %+v, want Blue Lock above threshold", best)
	}
}

func TestBuildQueriesCountry(t *testing.T) {
	got := buildQueries("Coupling UK",
		`Coupling.UK.S01-S04.DVDRip.XviD-PFa-FoV\Coupling.UK.S01.DVDRip.XviD.iNT-PFa\Coupling.UK.S01E01.Flushed.DVDRip.XviD.iNT-PFa\coupling.uk.101-pfa.avi`)
	if len(got) < 2 || got[0] != "Coupling UK" || got[1] != "Coupling" {
		t.Fatalf("got %q, want [\"Coupling UK\" \"Coupling\" ...]", got)
	}
}

// A folder the library is sorted by isn't searched as a title.
func TestBuildQueriesSkipsCategories(t *testing.T) {
	got := buildQueries("Some Show", "TV Shows/Comedy/Some Show/Season 1/Some Show - S01E01.mkv")
	for _, q := range got {
		if q == "Comedy" || q == "TV Shows" {
			t.Fatalf("category folder searched: %q", got)
		}
	}
	if got[0] != "Some Show" {
		t.Fatalf("got %q", got)
	}
}
