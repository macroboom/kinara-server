package parse

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		path string
		want Result
	}{
		// Movies, Plex-style folders.
		{"Night Shift (2023)/Night Shift (2023).mkv",
			Result{Kind: Movie, Title: "Night Shift", Year: 2023}},
		{"The Orchard (2019)/The Orchard (2019) - 2160p.mkv",
			Result{Kind: Movie, Title: "The Orchard", Year: 2019}},
		{"Mr. Holmes (2015)/Mr. Holmes (2015).mp4",
			Result{Kind: Movie, Title: "Mr. Holmes", Year: 2015}},
		// Scene names.
		{"Night.Shift.2023.1080p.BluRay.x264-GRP.mkv",
			Result{Kind: Movie, Title: "Night Shift", Year: 2023}},
		{"1917.2019.2160p.UHD.BluRay.REMUX.mkv",
			Result{Kind: Movie, Title: "1917", Year: 2019}},
		{"2012 (2009)/2012 (2009).mkv",
			Result{Kind: Movie, Title: "2012", Year: 2009}},
		{"Second_Light_2021_720p.mkv",
			Result{Kind: Movie, Title: "Second Light", Year: 2021}},
		// No year at all: cut at the first junk token.
		{"Field Day 1080p WEB-DL.mkv",
			Result{Kind: Movie, Title: "Field Day"}},
		// Year only on the folder.
		{"Glass House (2018)/Glass House.mkv",
			Result{Kind: Movie, Title: "Glass House", Year: 2018}},
		// Generic file name: take the folder.
		{"Paper Moons (2016)/movie.mkv",
			Result{Kind: Movie, Title: "Paper Moons", Year: 2016}},
		// IDs in names.
		{"The Matrix (1999) {tmdb-603}/The Matrix (1999) {tmdb-603}.mkv",
			Result{Kind: Movie, Title: "The Matrix", Year: 1999, TMDBID: 603}},
		{"Saltwater (2020) [imdbid-tt1234567]/Saltwater.mkv",
			Result{Kind: Movie, Title: "Saltwater", Year: 2020, IMDBID: "tt1234567"}},
		// Editions in braces are ignored.
		{"Ember Road (2014) {edition-Director's Cut}.mkv",
			Result{Kind: Movie, Title: "Ember Road", Year: 2014}},
		// Split movies.
		{"Long Division (2010)/Long Division (2010) - cd1.avi",
			Result{Kind: Movie, Title: "Long Division", Year: 2010, Part: 1}},
		{"Long Division (2010)/Long Division (2010) part2.avi",
			Result{Kind: Movie, Title: "Long Division", Year: 2010, Part: 2}},
		// Extras and samples.
		{"Northbound (2022)/Extras/Making of.mkv",
			Result{Kind: Movie, Title: "Making of", Extra: true}},
		{"Northbound (2022)/Northbound-trailer.mp4",
			Result{Kind: Movie, Title: "Northbound-trailer", Extra: true}},
		{"Northbound.2022.1080p.sample.mkv",
			Result{Kind: Movie, Title: "Northbound", Year: 2022, Sample: true}},

		// Episodes, Plex-style.
		{"The Long Harbor (2024)/Season 02/The Long Harbor - S02E05 - Low Tide.mkv",
			Result{Kind: Episode, Title: "The Long Harbor", Year: 2024, Season: 2, Episode: 5, EpisodeEnd: 5}},
		{"Copper Valley/Season 1/Copper Valley S01E03.mkv",
			Result{Kind: Episode, Title: "Copper Valley", Season: 1, Episode: 3, EpisodeEnd: 3}},
		// Scene episode names.
		{"Small.Hours.S03E01.1080p.WEB-DL.mkv",
			Result{Kind: Episode, Title: "Small Hours", Season: 3, Episode: 1, EpisodeEnd: 1}},
		{"Small Hours (2019) - s03e01.mkv",
			Result{Kind: Episode, Title: "Small Hours", Year: 2019, Season: 3, Episode: 1, EpisodeEnd: 1}},
		// Multi-episode files.
		{"Field Notes/Season 1/Field Notes - S01E01E02.mkv",
			Result{Kind: Episode, Title: "Field Notes", Season: 1, Episode: 1, EpisodeEnd: 2}},
		{"Field Notes/Season 1/Field Notes - S01E03-E04.mkv",
			Result{Kind: Episode, Title: "Field Notes", Season: 1, Episode: 3, EpisodeEnd: 4}},
		// 1x02 style.
		{"Low Country/Low Country 2x07.mkv",
			Result{Kind: Episode, Title: "Low Country", Season: 2, Episode: 7, EpisodeEnd: 7}},
		// Episode number only, season from the folder.
		{"Wildflower/Season 3/E04 - The Field.mkv",
			Result{Kind: Episode, Title: "Wildflower", Season: 3, Episode: 4, EpisodeEnd: 4, SeasonFromFolder: true}},
		{"Wildflower/Season 3/05 - The Hill.mkv",
			Result{Kind: Episode, Title: "Wildflower", Season: 3, Episode: 5, EpisodeEnd: 5, SeasonFromFolder: true}},
		// Specials folder is season 0.
		{"Wildflower/Specials/Wildflower - S00E01 - Christmas.mkv",
			Result{Kind: Episode, Title: "Wildflower", Season: 0, Episode: 1, EpisodeEnd: 1}},
		// Episode file directly in the show folder.
		{"Static/S01E02.mkv",
			Result{Kind: Episode, Title: "Static", Season: 1, Episode: 2, EpisodeEnd: 2}},
		// Resolution must not look like 1x02.
		{"Meridian.2024.1920x1080.mkv",
			Result{Kind: Movie, Title: "Meridian", Year: 2024}},
		// Season ranges and pack labels on show folders.
		{"Pushing Daisies S01-S02 br/Season 1/Pushing Daisies - S01E01.mkv",
			Result{Kind: Episode, Title: "Pushing Daisies", Season: 1, Episode: 1, EpisodeEnd: 1}},
		{"Detroiters S01-S02/Season 2/S02E03.mkv",
			Result{Kind: Episode, Title: "Detroiters", Season: 2, Episode: 3, EpisodeEnd: 3}},
		{"Brockmire Season 1-4/Season 1/Brockmire S01E01.mkv",
			Result{Kind: Episode, Title: "Brockmire", Season: 1, Episode: 1, EpisodeEnd: 1}},
		{"Legit The Complete Series/Season 1/Legit S01E01.mkv",
			Result{Kind: Episode, Title: "Legit", Season: 1, Episode: 1, EpisodeEnd: 1}},
		// Titles that only look like season tags stay whole.
		{"A Complete Unknown (2024)/A Complete Unknown (2024).mkv",
			Result{Kind: Movie, Title: "A Complete Unknown", Year: 2024}},
		{"Season of the Witch (2011)/Season of the Witch (2011).mkv",
			Result{Kind: Movie, Title: "Season of the Witch", Year: 2011}},
		// Release typo with a three-digit season: the folder wins.
		{"Pushing Daisies S01-S02 br 10bit hevc-d3g/S02/Pushing.Daisies.S021E07.RBluRay.1080p.10Bit.DD5.1.H265-d3g.mkv",
			Result{Kind: Episode, Title: "Pushing Daisies", Season: 2, Episode: 7, EpisodeEnd: 7, SeasonFromFolder: true}},
		// Real folder names from a library.
		{"Shrinking S03 2023 1080p ATVP WEB-DL DDP5 1 Atmos H 264-HHWEB/Shrinking.S03E01.mkv",
			Result{Kind: Episode, Title: "Shrinking", Season: 3, Episode: 1, EpisodeEnd: 1}},
		{"Dexter Resurrection 2025 Season 1 S01 2160p AMZN WEB-DL x265/Season 1/S01E02.mkv",
			Result{Kind: Episode, Title: "Dexter Resurrection", Year: 2025, Season: 1, Episode: 2, EpisodeEnd: 2}},
		{"Platonic (2023) S02 (1080p ATVP WEB-DL H265 SDR DDP Atmos 5.1 English - HONE)/Season 2/Platonic S02E01.mkv",
			Result{Kind: Episode, Title: "Platonic", Year: 2023, Season: 2, Episode: 1, EpisodeEnd: 1}},
		{"Downton.Abbey.S01-06.1080p.BluRay.x264-MIXED [NO RAR]/Season 3/E05.mkv",
			Result{Kind: Episode, Title: "Downton Abbey", Season: 3, Episode: 5, EpisodeEnd: 5, SeasonFromFolder: true}},
		{"Legit.2013.S01.1080p.WEB DL.DD5.1.H.264-BS/Season 1/Legit S01E01.mkv",
			Result{Kind: Episode, Title: "Legit", Year: 2013, Season: 1, Episode: 1, EpisodeEnd: 1}},
		// Scene-group file names: the folder names the show.
		{"Downton.Abbey.S01-06.1080p.BluRay.x264-MIXED [NO RAR]/teneighty-dta-s05e01.mkv",
			Result{Kind: Episode, Title: "Downton Abbey", Season: 5, Episode: 1, EpisodeEnd: 1}},
		{"Succession S02 2160p MAX WEB-DL DD 5 1 Atmos DoVi HDR H 265-playWEB/Succession.S02.2160p.MAX.WEB-DL.DD+5.1.Atmos.DoVi.HDR.H.265-playWEB/Succession.S02E01.The.Summer.Palace.2160p.MAX.WEB-DL.mkv",
			Result{Kind: Episode, Title: "Succession", Season: 2, Episode: 1, EpisodeEnd: 1}},
		{"Gen V S02E04 1080p AMZN WEB-DL DDP5 1 H 264 DUAL-BiOMA/Gen.V.S02E04.1080p.mkv",
			Result{Kind: Episode, Title: "Gen V", Season: 2, Episode: 4, EpisodeEnd: 4}},
		// A season folder at the library root: the file name names the show.
		{"Season 1/Static S01E03.mkv",
			Result{Kind: Episode, Title: "Static", Season: 1, Episode: 3, EpisodeEnd: 3}},
		// Marker variants: glued to the next word, NxN in brackets, SxN, EP.
		{"Barry S01-S04 web hdr hevc-d3g/Barry S03/Barry.S03E051080p.WEBRip.HDR10.10Bit.DDP5.1.H265-d3g.mkv",
			Result{Kind: Episode, Title: "Barry", Season: 3, Episode: 5, EpisodeEnd: 5}},
		{"Justified S01-06 br 10bit dts hevc-d3g (remux)/Justified S04/Justified.S04E01BluRay.10Bit.1080p.Dts-HDMa5.1.H265-d3g.mkv",
			Result{Kind: Episode, Title: "Justified", Season: 4, Episode: 1, EpisodeEnd: 1}},
		{"Castle/Castle [1x01] Flowers for Your Grave.mkv",
			Result{Kind: Episode, Title: "Castle", Season: 1, Episode: 1, EpisodeEnd: 1}},
		{"Glee/Glee S01 (360p re-blurip)/Glee S01X01 Somebody to Love.mp4",
			Result{Kind: Episode, Title: "Glee", Season: 1, Episode: 1, EpisodeEnd: 1}},
		{"Scam 1992 The Harshad Mehta Story 2020 S01 REPACK 1080p WEB-DL-Ranvijay/Scam 1992 The Harshad Mehta Story 2020 S01EP01 REPACK 1080p WEB-DL.mkv",
			Result{Kind: Episode, Title: "Scam 1992 The Harshad Mehta Story", Year: 2020, Season: 1, Episode: 1, EpisodeEnd: 1}},
		// The episode marker is only in the folder above the file.
		{"Coupling.UK.S01-S04.DVDRip.XviD-PFa-FoV/Coupling.UK.S01.DVDRip.XviD.iNT-PFa/Coupling.UK.S01E01.Flushed.DVDRip.XviD.iNT-PFa/coupling.uk.101-pfa.avi",
			Result{Kind: Episode, Title: "Coupling UK", Season: 1, Episode: 1, EpisodeEnd: 1}},
		{"Dexter.S01-S08.COMPLETE.1080p.BluRay.x264-IPT/Dexter.S05.1080p.BluRay.x264-/Dexter.S05E01.PROPER.1080p.BluRay.x264-FLHD/flhd-dexters05e01.mkv",
			Result{Kind: Episode, Title: "Dexter", Season: 5, Episode: 1, EpisodeEnd: 1}},
		{"Rick and Morty S08E04 The Last Temptation of Jerry 1080p AMZN WEB-DL DDP5 1 H 264-NTb/817941587e61412ba9f1ccfe4d494d42.mkv",
			Result{Kind: Episode, Title: "Rick and Morty", Season: 8, Episode: 4, EpisodeEnd: 4}},
		// A loose season folder ("Show S01 (...)") and a leading episode number.
		{"Solar Opposites/Solar Opposites Season 1 S01 (1080p HULU WEBRip x265) [UTR]/01 - The Matter Transfer Array - theincognito.mkv",
			Result{Kind: Episode, Title: "Solar Opposites", Season: 1, Episode: 1, EpisodeEnd: 1, SeasonFromFolder: true}},
		// Anime absolute numbering: "Show - 01".
		{"Assassination Classroom/[AnimeRG] Assassination Classroom - 05 [720p] [x264] [pseudo].mkv",
			Result{Kind: Episode, Title: "Assassination Classroom", Season: 1, Episode: 5, EpisodeEnd: 5}},
		{"Assassination Classroom/[AnimeRG] Assassination Classroom - 00 [720p] [x264] [pseudo].mkv",
			Result{Kind: Episode, Title: "Assassination Classroom", Season: 0, Episode: 0, EpisodeEnd: 0}},
		{"Chainsawman/[EMBER] Chainsaw Man - 01.mkv",
			Result{Kind: Episode, Title: "Chainsawman", Season: 1, Episode: 1, EpisodeEnd: 1}},
		// Bonus folders anywhere in the path.
		{"The Office/Season 2/Featurettes/Bloopers.mkv",
			Result{Kind: Movie, Title: "Bloopers", Extra: true}},
		// Windows separators.
		{`Night Shift (2023)\Night Shift (2023).mkv`,
			Result{Kind: Movie, Title: "Night Shift", Year: 2023}},
	}

	for _, c := range cases {
		got := Parse(c.path)
		if got != c.want {
			t.Errorf("Parse(%q)\n got  %+v\n want %+v", c.path, got, c.want)
		}
	}
}

func TestIsVideo(t *testing.T) {
	for p, want := range map[string]bool{
		"a.mkv": true, "a.MP4": true, "a.srt": false, "a.nfo": false, "dir/a.m2ts": true,
	} {
		if IsVideo(p) != want {
			t.Errorf("IsVideo(%q) = %v", p, !want)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"The Long Harbor": "long harbor",
		"Night Shift!":    "night shift",
		"Copper & Valley": "copper and valley",
		"Mr. Holmes":      "mr holmes",
		"Amélie":          "amelie",
		"90 Day Fiancé":   "90 day fiance",
		"I'm Sorry":       "im sorry",
		"Im Sorry":        "im sorry",
		"The":             "the",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanTitleShowFolders(t *testing.T) {
	for in, want := range map[string]string{
		"Gen V S02E04 1080p AMZN WEB-DL DDP5 1 H 264 DUAL-BiOMA":                          "Gen V",
		"The White Lotus S03E07 Killer Instincts 2160p MAX WEB-DL DDP5 1 DV HDR x265-NTb": "The White Lotus",
		"Malcolm.in.the.Middle.S01-S07.720p.WEB-DL.x264":                                  "Malcolm in the Middle",
		"Downton.Abbey.S01-06.1080p.BluRay.x264-MIXED [NO RAR]":                           "Downton Abbey",
		"Daredevil Born Again S01 web hdr hevc-d3g":                                       "Daredevil Born Again",
		"90.Day.Fiance.Before.the.90.Days.S04.1080p.HULU.WEB-DL.AAC2.0.H.264-NTb":         "90 Day Fiance Before the 90 Days",
	} {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSortTitle(t *testing.T) {
	if got := SortTitle("The Orchard"); got != "Orchard" {
		t.Errorf("got %q", got)
	}
	if got := SortTitle("A"); got != "A" {
		t.Errorf("got %q", got)
	}
}

// TestCorpus checks a corpus of real file names, if one exists.
//
// Create testdata/corpus.tsv with one line per file:
//
//	<relative path> TAB <kind> TAB <title> TAB <year> TAB <season> TAB <episode>
//
// Lines starting with # are skipped. Build it from your library with:
//
//	cd /media/Movies && find . -type f -name '*.mkv' | sed 's|^\./||'
//
// then fill in the expected columns. Every mismatch found in daily use
// should become a line here.
func TestCorpus(t *testing.T) {
	f, err := os.Open("testdata/corpus.tsv")
	if err != nil {
		t.Skip("no testdata/corpus.tsv")
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue
		}
		cols := strings.Split(text, "\t")
		if len(cols) < 4 {
			t.Errorf("line %d: need at least 4 columns", line)
			continue
		}
		got := Parse(cols[0])
		year, _ := strconv.Atoi(cols[3])
		if got.Kind.String() != cols[1] || got.Title != cols[2] || got.Year != year {
			t.Errorf("line %d %q: got %s %q %d", line, cols[0], got.Kind, got.Title, got.Year)
			continue
		}
		if len(cols) >= 6 && got.Kind == Episode {
			s, _ := strconv.Atoi(cols[4])
			e, _ := strconv.Atoi(cols[5])
			if got.Season != s || got.Episode != e {
				t.Errorf("line %d %q: got S%02dE%02d", line, cols[0], got.Season, got.Episode)
			}
		}
	}
}
