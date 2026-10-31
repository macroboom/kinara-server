// Package parse turns media file paths into titles, years, and episode numbers.
//
// It is pure string work: no file system or network access, so it is fast and
// easy to test against a corpus of real file names.
package parse

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Kind is what a file looks like from its name and folders alone.
type Kind int

const (
	Unknown Kind = iota
	Movie
	Episode
)

func (k Kind) String() string {
	switch k {
	case Movie:
		return "movie"
	case Episode:
		return "episode"
	}
	return "unknown"
}

// Result is everything that can be read from a path.
type Result struct {
	Kind Kind

	// Title is the movie title, or the show title for episodes.
	Title string
	// Year is the release year (movies) or first-air year (shows), 0 if absent.
	Year int

	// Season and Episode are set for episodes. EpisodeEnd is set for
	// multi-episode files such as S01E01E02 (otherwise equal to Episode).
	Season     int
	Episode    int
	EpisodeEnd int

	// Part is the part number of a movie split across files (cd1, part2), 0 if none.
	Part int

	// IDs found in the path, such as {tmdb-603} or [imdbid-tt0133093].
	TMDBID int
	IMDBID string

	// Extra is a trailer, featurette, or other bonus content.
	Extra bool
	// Sample is a short sample clip that should be ignored.
	Sample bool
	// SeasonFromFolder reports whether the season number came from a folder.
	SeasonFromFolder bool
}

var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true,
	".ts": true, ".m2ts": true, ".webm": true, ".wmv": true, ".mpg": true,
	".mpeg": true,
}

// IsVideo reports whether the file extension is a supported video container.
func IsVideo(p string) bool {
	return videoExts[strings.ToLower(path.Ext(normalizeSlashes(p)))]
}

var (
	reSample   = regexp.MustCompile(`(?i)(^|[ ._\-\[(])sample([ ._\-\])]|$)`)
	reExtraSuf = regexp.MustCompile(`(?i)-(trailer|featurette|behindthescenes|deleted|interview|scene|short|other)$`)
	// "Movie (2012) preview", "Movie.Trailer.2", "Movie - teaser": a trailer
	// saved next to the film. Needs text before it, so a film named just
	// "Preview" isn't skipped.
	reExtraWord = regexp.MustCompile(`(?i)\S[ ._\-]+(?:trailer|preview|teaser)(?:[ ._\-]?\d{1,2})?$`)
	// Edition labels in parentheses: "(Theatrical)", "(Director's Cut)",
	// "(Extended Edition)", "(25th Anniversary Edition)". They aren't part
	// of the title, and TMDB finds nothing when they're searched.
	reParens  = regexp.MustCompile(`\(([^()]*)\)`)
	reEdition = regexp.MustCompile(`(?i)^\s*(?:(?:the\s+)?(?:theatrical|extended|director'?s|directors|unrated|uncut|remastered|restored|special|ultimate|collector'?s|definitive|anniversary|\d{1,3}(?:st|nd|rd|th)?\s+anniversary|imax|final|redux|criterion|open\s+matte|international|original)\b.*|.*\b(?:cut|edition|version)\s*)$`)
	reTMDB    = regexp.MustCompile(`(?i)[\[{](?:tmdb|tmdbid)[-=](\d+)[\]}]`)
	reIMDB    = regexp.MustCompile(`(?i)[\[{](?:imdb|imdbid)[-=](tt\d+)[\]}]`)
	reBracket = regexp.MustCompile(`\[[^\]]*\]|\{[^}]*\}`)

	// S01E02, S1E2, s01.e02, S01EP02, S01E01E02, S01E01-E02, S01E01-02
	// The second number needs an E or a dash right before it: "S01E01 - 10
	// Years" is one episode called "10 Years".
	reSxE = regexp.MustCompile(`(?i)\bs(\d{1,3}) ?e(?:p ?)?(\d{1,3})(?:(?: ?- ?)?e(\d{1,3})|-(\d{1,3}))?\b`)
	// S03E051080p, S04E01BluRay: no separator after the episode number.
	reSxEGlued = regexp.MustCompile(`(?i)\bs(\d{1,2}) ?e(\d{2})(?:\d{3,4}[pi]|[a-z])`)
	// S01X02
	reSxX = regexp.MustCompile(`(?i)\bs(\d{1,2})x(\d{2,3})\b`)
	// 1x02, 01x02
	reNxN = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{2,3})\b`)
	// Anime-style absolute numbering: "Show - 05", "Show - 12v2".
	reAbsolute = regexp.MustCompile(`(?:^|\s)-\s+(\d{1,3})(?:v\d)?(?:\s|$)`)
	// E05, Ep 05, Episode 5
	reEpOnly = regexp.MustCompile(`(?i)\b(?:e|ep|episode) ?(\d{1,3})\b`)
	// 05 - Title, 05 Title
	reLeadingNum = regexp.MustCompile(`^(\d{1,3})\b`)

	reSeasonDir = regexp.MustCompile(`(?i)^(?:season|series|staffel|saison|temporada) ?(\d{1,3})$`)
	reSeasonS   = regexp.MustCompile(`(?i)^s(\d{1,3})$`)
	// A season tag inside a longer folder name: "Glee S01 (360p)",
	// "Dexter.S05.1080p.BluRay", "Solar Opposites Season 1 S01 (...)".
	reSeasonLoose = regexp.MustCompile(`(?i)(?:^|[ ._(\[-])(?:s(\d{1,2})|season ?(\d{1,2}))(?:[ ._)\]-]|$)`)
	reSeasonRange = regexp.MustCompile(`(?i)s\d{1,2}\s*-\s*s?\d{1,2}|seasons?\s*\d{1,2}\s*(?:-|to)\s*\d`)
	// Folders of bonus material, matched anywhere in a folder name.
	reExtraDir = regexp.MustCompile(`(?i)\b(?:featurettes?|bonus features?|bonus|behind the scenes|deleted scenes|bloopers)\b`)

	reParenYear = regexp.MustCompile(`\(((?:19|20)\d{2})\)`)
	reYearTok   = regexp.MustCompile(`^(?:19|20)\d{2}$`)
	rePart      = regexp.MustCompile(`(?i)(?:^| )(?:cd|disc|disk|dvd|part|pt) ?(\d{1,2})(?: |$)`)
	reJunk      = regexp.MustCompile(`(?i)^(2160p|1080p|1080i|720p|576p|480p|4k|uhd|bluray|blu-ray|bdremux|brrip|bdrip|webrip|web-dl|webdl|web|hdtv|dvdrip|hdrip|remux|x264|x265|h264|h265|h\.264|h\.265|hevc|avc|10bit|hdr|hdr10|hdr10\+|dv|dovi|atmos|truehd|dts|dts-hd|aac|ac3|eac3|ddp5|ddp|dd5|proper|repack|extended|unrated|remastered|internal|limited|imax|multi|subbed|dubbed)$`)
	reSpaces    = regexp.MustCompile(`\s+`)
	// Season ranges and pack labels in folder names: "S01-S02", "S01",
	// "Season 1-3", "Seasons 1 to 4", "Complete Series". Everything from the
	// first one on is dropped, along with junk after it ("S01-S02 br").
	reSeasonTag = regexp.MustCompile(`(?i)(?:^|\s)(?:s\d{1,3}\s?e\d{1,3}|s\d{1,2}(?:\s*-\s*s?\d{1,2})?|seasons?\s*\d{1,2}(?:\s*(?:-|to)\s*\d{1,2})?|complete\s+series|the\s+complete\s+series)(?:\s|$)`)
)

var extraDirs = map[string]bool{
	"extras": true, "featurettes": true, "behind the scenes": true,
	"deleted scenes": true, "interviews": true, "scenes": true, "shorts": true,
	"trailers": true, "other": true, "bonus": true,
}

var genericNames = map[string]bool{
	"movie": true, "film": true, "video": true, "main": true, "feature": true,
}

func normalizeSlashes(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// Parse reads a path relative to the library root, for example
// "The Long Harbor (2024)/Season 02/The Long Harbor - S02E05 - Low Tide.mkv".
func Parse(rel string) Result {
	rel = strings.Trim(normalizeSlashes(rel), "/")
	segs := strings.Split(rel, "/")
	file := segs[len(segs)-1]
	dirs := segs[:len(segs)-1]
	name := strings.TrimSuffix(file, path.Ext(file))

	var r Result

	// IDs anywhere in the path; the one nearest the file wins.
	if m := reTMDB.FindAllStringSubmatch(rel, -1); len(m) > 0 {
		r.TMDBID, _ = strconv.Atoi(m[len(m)-1][1])
	}
	if m := reIMDB.FindAllStringSubmatch(rel, -1); len(m) > 0 {
		r.IMDBID = strings.ToLower(m[len(m)-1][1])
	}

	if reSample.MatchString(name) {
		r.Sample = true
	}
	for i, d := range dirs {
		ld := strings.ToLower(strings.TrimSpace(d))
		if ld == "sample" || ld == "samples" {
			r.Sample = true
		}
		// Exact names like "Extras" only below the top level, so a show
		// called "Extras" isn't skipped.
		if (i > 0 && extraDirs[ld]) || reExtraDir.MatchString(separatorsToSpaces(d)) {
			r.Extra = true
		}
	}
	if reExtraSuf.MatchString(name) {
		r.Extra = true
	}
	// A trailing "preview" or "trailer" only on files that aren't episodes:
	// an episode can be called "The Preview".
	if w := strings.TrimSpace(separatorsToSpaces(stripBracketsKeepEpisode(name))); reExtraWord.MatchString(w) {
		if _, ep := findEpisode(w); !ep {
			r.Extra = true
		}
	}

	// Season folder, nearest to the file.
	seasonDirIdx := -1
	folderSeason := 0
	strictSeasonDir := false
	for i := len(dirs) - 1; i >= 0; i-- {
		if n, ok, strict := seasonFromDir(dirs[i]); ok {
			seasonDirIdx, folderSeason, strictSeasonDir = i, n, strict
			break
		}
	}

	work := separatorsToSpaces(stripBracketsKeepEpisode(name))

	// Episode patterns: in the file name first, then in the folders holding
	// it (releases that put "Show.S01E01.Title" on the folder and a short
	// code like "show.101-grp.avi" on the file).
	epPrefix := ""
	found := false
	if m, ok := findEpisode(work); ok {
		r.Season, r.Episode, r.EpisodeEnd = m.season, m.episode, m.episodeEnd
		// A three-digit season ("S021E01") is almost always a typo. Trust
		// the season folder when there is one.
		if m.seasonDigits == 3 && seasonDirIdx >= 0 {
			r.Season = folderSeason
			r.SeasonFromFolder = true
		}
		epPrefix = work[:m.start]
		found = true
	}
	if !found {
		for i := len(dirs) - 1; i >= 0; i-- {
			if m, ok := findEpisode(separatorsToSpaces(stripBracketsKeepEpisode(dirs[i]))); ok && m.seasonDigits < 3 {
				r.Season, r.Episode, r.EpisodeEnd = m.season, m.episode, m.episodeEnd
				found = true
				break
			}
		}
	}
	if !found && seasonDirIdx >= 0 {
		if m := reEpOnly.FindStringSubmatchIndex(work); m != nil {
			r.Episode = atoi(work[m[2]:m[3]])
			epPrefix = work[:m[0]]
			found = true
		} else if m := reLeadingNum.FindStringSubmatch(strings.TrimSpace(work)); m != nil {
			r.Episode = atoi(m[1])
			found = true
		}
		if found {
			r.Season = folderSeason
			r.EpisodeEnd = r.Episode
			r.SeasonFromFolder = true
		}
	}
	if !found {
		// Anime-style "Show - 05": season from the folder, else season 1.
		// Episode 0 is usually a prologue, filed under Specials.
		if m := reAbsolute.FindStringSubmatchIndex(work); m != nil {
			r.Episode = atoi(work[m[2]:m[3]])
			r.EpisodeEnd = r.Episode
			r.Season = 1
			if seasonDirIdx >= 0 {
				r.Season, r.SeasonFromFolder = folderSeason, true
			}
			if r.Episode == 0 {
				r.Season = 0
			}
			epPrefix = work[:m[0]]
			found = true
		}
	}

	if found {
		r.Kind = Episode
		// Show title, like Plex: the folder above the season folder, else the
		// folder holding the file, else the text before the episode marker.
		// Folders come first because file names are often scene-group
		// shorthand ("teneighty-dta-s05e01.mkv").
		switch {
		case seasonDirIdx > 0:
			r.Title, r.Year = cleanTitle(dirs[seasonDirIdx-1])
		case seasonDirIdx == 0 && !strictSeasonDir:
			// A top-level folder like "Glee S01 (360p)" names the show too.
			r.Title, r.Year = cleanTitle(dirs[0])
		case len(dirs) > 0 && seasonDirIdx != len(dirs)-1:
			r.Title, r.Year = cleanTitle(dirs[len(dirs)-1])
		}
		if r.Title == "" && strings.TrimSpace(strings.Trim(epPrefix, " -")) != "" {
			r.Title, r.Year = cleanTitle(epPrefix)
		}
		return r
	}

	// Movie.
	r.Kind = Movie
	r.Part, work = extractPart(work)
	title, year := cleanTitle(work)
	if len(dirs) > 0 && !r.Extra {
		dirTitle, dirYear := cleanTitle(dirs[len(dirs)-1])
		useDir := false
		switch {
		case title == "" || genericNames[strings.ToLower(title)]:
			useDir = true
		case year == 0 && dirYear != 0 && sameStart(title, dirTitle):
			useDir = true
		}
		if useDir && dirTitle != "" {
			title, year = dirTitle, dirYear
		}
	}
	r.Title, r.Year = title, year
	return r
}

// seasonFromDir reads a season number from a folder name. strict is true
// for folders that are only a season ("Season 2", "S02", "Specials") and
// false for longer names that contain one ("Glee S01 (360p)").
func seasonFromDir(d string) (season int, ok, strict bool) {
	d = strings.TrimSpace(d)
	if strings.EqualFold(d, "specials") || strings.EqualFold(d, "season 0") {
		return 0, true, true
	}
	if m := reSeasonDir.FindStringSubmatch(separatorsToSpaces(d)); m != nil {
		return atoi(m[1]), true, true
	}
	if m := reSeasonS.FindStringSubmatch(d); m != nil {
		return atoi(m[1]), true, true
	}
	w := separatorsToSpaces(stripBrackets(d))
	if reSeasonRange.MatchString(w) {
		return 0, false, false // a multi-season pack, not one season
	}
	if _, isEpisode := findEpisode(w); isEpisode {
		return 0, false, false // a single-episode folder
	}
	if m := reSeasonLoose.FindStringSubmatch(w); m != nil {
		n := m[1]
		if n == "" {
			n = m[2]
		}
		return atoi(n), true, false
	}
	return 0, false, false
}

type episodeMatch struct {
	season, episode, episodeEnd int
	seasonDigits                int
	start                       int // where the marker starts in the text
}

// findEpisode looks for an episode marker: S01E02 (and S01EP02, S01E01-E03),
// S03E051080p with no separator, S01X02, or 1x02.
func findEpisode(w string) (episodeMatch, bool) {
	if m := reSxE.FindStringSubmatchIndex(w); m != nil {
		e := episodeMatch{season: atoi(w[m[2]:m[3]]), episode: atoi(w[m[4]:m[5]]),
			seasonDigits: m[3] - m[2], start: m[0]}
		e.episodeEnd = e.episode
		for _, g := range [][2]int{{m[6], m[7]}, {m[8], m[9]}} {
			// A file holds a few episodes, not dozens: a bigger span is
			// something else in the name.
			if g[0] >= 0 {
				if end := atoi(w[g[0]:g[1]]); end > e.episode && end-e.episode <= 10 {
					e.episodeEnd = end
				}
			}
		}
		return e, true
	}
	for _, re := range []*regexp.Regexp{reSxEGlued, reSxX, reNxN} {
		if m := re.FindStringSubmatchIndex(w); m != nil {
			e := episodeMatch{season: atoi(w[m[2]:m[3]]), episode: atoi(w[m[4]:m[5]]),
				seasonDigits: m[3] - m[2], start: m[0]}
			e.episodeEnd = e.episode
			return e, true
		}
	}
	return episodeMatch{}, false
}

// stripBracketsKeepEpisode drops [tags] and {tags} but keeps the contents of
// ones that hold an episode marker, like "Castle [1x01] Flowers".
func stripBracketsKeepEpisode(s string) string {
	return reBracket.ReplaceAllStringFunc(s, func(b string) string {
		inner := b[1 : len(b)-1]
		if _, ok := findEpisode(inner); ok {
			return " " + inner + " "
		}
		return " "
	})
}

// cd, disc and dvd always mark a piece of one movie. "Part 2" may instead
// be in the title ("Nymphomaniac Part 1 (2013)", "Dune Part Two"): it only
// counts as a piece when it comes after the year ("Heat (1995) Part 1").
func extractPart(s string) (int, string) {
	m := rePart.FindStringSubmatchIndex(s)
	if m == nil {
		return 0, s
	}
	word := strings.ToLower(strings.TrimSpace(s[m[0]:m[2]]))
	if strings.HasPrefix(word, "part") || strings.HasPrefix(word, "pt") {
		if !hasYear(s[:m[0]]) {
			return 0, s
		}
	}
	part := atoi(s[m[2]:m[3]])
	return part, strings.TrimSpace(s[:m[0]] + " " + s[m[1]:])
}

// hasYear reports whether s holds a release year (1900 to next year).
func hasYear(s string) bool {
	for _, t := range strings.Fields(separatorsToSpaces(strings.NewReplacer("(", " ", ")", " ").Replace(s))) {
		if reYearTok.MatchString(t) && plausibleYear(atoi(t)) {
			return true
		}
	}
	return false
}

// plausibleYear: a 4-digit number that can be a release year. "2049" in
// "Blade.Runner.2049.1080p" is part of the title.
func plausibleYear(y int) bool { return y >= 1900 && y <= time.Now().Year()+1 }

// cleanTitle pulls a title and year out of a folder or file name such as
// "Night.Shift.2023.1080p.BluRay.x264" or "The Long Harbor (2024)".
func cleanTitle(s string) (string, int) {
	s = stripBrackets(s)
	s = stripEditions(s)
	work := separatorsToSpaces(s)
	if m := reSeasonTag.FindStringIndex(work); m != nil && strings.TrimSpace(work[:m[0]]) != "" {
		s = work[:m[0]]
	}
	if m := reParenYear.FindStringSubmatchIndex(s); m != nil {
		year := atoi(s[m[2]:m[3]])
		title := s[:m[0]]
		if strings.TrimSpace(title) == "" {
			// "(2009) Title" is unusual; keep the rest.
			title = s[m[1]:]
		}
		return tidy(separatorsToSpaces(title)), year
	}

	toks := strings.Fields(separatorsToSpaces(s))
	// Scene style: cut at the last year token that is not the first token,
	// so "1917.2019.1080p" keeps the title "1917".
	yearIdx := -1
	for i := len(toks) - 1; i >= 1; i-- {
		if reYearTok.MatchString(toks[i]) && plausibleYear(atoi(toks[i])) {
			yearIdx = i
			break
		}
	}
	if yearIdx >= 1 {
		return tidy(strings.Join(toks[:yearIdx], " ")), atoi(toks[yearIdx])
	}
	// No year: cut at the first junk token (resolution, source, codec).
	for i, t := range toks {
		if i > 0 && reJunk.MatchString(t) {
			toks = toks[:i]
			break
		}
	}
	return tidy(strings.Join(toks, " ")), 0
}

func stripBrackets(s string) string { return reBracket.ReplaceAllString(s, " ") }

// stripEditions drops edition labels in parentheses, keeping years and any
// other parenthesised words (which may be part of the title).
func stripEditions(s string) string {
	return reParens.ReplaceAllStringFunc(s, func(m string) string {
		if reEdition.MatchString(m[1 : len(m)-1]) {
			return " "
		}
		return m
	})
}

// separatorsToSpaces turns dots and underscores into spaces when the name
// mostly uses them as word separators. Dots followed by a space are kept, so
// "Mr. Robot" stays intact even in "Mr. Robot.S01.1080p".
func separatorsToSpaces(s string) string {
	s = strings.ReplaceAll(s, "_", " ")
	t := strings.TrimSpace(s)
	if strings.Count(t, ".") <= strings.Count(t, " ") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '.' && (i+1 >= len(s) || s[i+1] != ' ') {
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func tidy(s string) string {
	s = reSpaces.ReplaceAllString(s, " ")
	s = strings.Trim(s, " -–:,")
	return strings.TrimSpace(s)
}

func sameStart(a, b string) bool {
	na, nb := Normalize(a), Normalize(b)
	if na == "" || nb == "" {
		return false
	}
	return strings.HasPrefix(na, nb) || strings.HasPrefix(nb, na)
}

// SeasonOnly reports the season of a file name that has a season number but
// no episode number, such as "Show.S01.1080p.mkv".
func SeasonOnly(name string) (int, bool) {
	base := strings.TrimSuffix(path.Base(normalizeSlashes(name)), path.Ext(name))
	w := separatorsToSpaces(base)
	if _, ok := findEpisode(w); ok || reSeasonRange.MatchString(w) {
		return 0, false
	}
	m := reSeasonLoose.FindStringSubmatch(w)
	if m == nil {
		return 0, false
	}
	if m[1] != "" {
		return atoi(m[1]), true
	}
	return atoi(m[2]), true
}

// CleanTitle tidies a raw folder or file name into a title, dropping years,
// season ranges, and release junk.
func CleanTitle(s string) string {
	t, _ := cleanTitle(s)
	return t
}

var accents = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "æ", "ae",
	"ç", "c", "è", "e", "é", "e", "ê", "e", "ë", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i", "ñ", "n",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o", "œ", "oe",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "ý", "y", "ÿ", "y", "ß", "ss",
)

// Normalize lowercases a title, folds accents, and strips punctuation and
// leading articles, for comparing titles from different sources. Apostrophes
// are dropped rather than split, so "I'm Sorry" and "Im Sorry" compare equal.
func Normalize(s string) string {
	s = accents.Replace(strings.ToLower(s))
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\'' || r == '’' || r == 'ʼ':
			// dropped
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > 127 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			// Other scripts are kept; symbols like the dot in "WALL·E" or
			// a dash are separators, as "-" is.
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	f := strings.Fields(b.String())
	if len(f) > 1 && (f[0] == "the" || f[0] == "a" || f[0] == "an") {
		f = f[1:]
	}
	return strings.Join(f, " ")
}

// SortTitle returns the title with a leading article moved out of the way,
// so "The Orchard" sorts under O.
func SortTitle(s string) string {
	l := strings.ToLower(s)
	for _, a := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(l, a) && len(s) > len(a) {
			return strings.TrimSpace(s[len(a):])
		}
	}
	return s
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimLeft(s, "0"))
	return n
}
