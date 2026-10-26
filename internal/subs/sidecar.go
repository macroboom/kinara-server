package subs

import (
	"path/filepath"
	"sort"
	"strings"
)

// Sidecar is a subtitle file that goes with a video.
type Sidecar struct {
	Path     string
	Language string // three-letter code, or ""
	// Title is what else the name says ("Commentary"), for telling two
	// files of one language apart.
	Title           string
	Format          string // subrip, ass, ssa, webvtt
	Forced          bool
	HearingImpaired bool
}

// formats maps subtitle file extensions to codec names (as ffprobe uses).
var formats = map[string]string{".srt": "subrip", ".ass": "ass", ".ssa": "ssa", ".vtt": "webvtt"}

// IsSubtitle says whether a file name is a text subtitle file Kinara reads.
func IsSubtitle(name string) bool {
	_, ok := formats[strings.ToLower(filepath.Ext(name))]
	return ok
}

// subsDirs are folders releases keep subtitles in.
var subsDirs = map[string]bool{"subs": true, "subtitles": true, "sub": true}

// Find picks the subtitle files that go with a video, from the subtitle
// files found in its folder and the folders under it (paths, any order).
// videosInDir is how many videos share the video's folder: a Subs folder's
// loose files only go with the video when it's the only one.
//
// What counts:
//
//	Movie (2010).srt, Movie (2010).en.srt, Movie (2010).English.forced.srt,
//	Movie (2010).eng.sdh.srt                     next to Movie (2010).mkv
//	Subs/English.srt, Subs/2_English.srt         when it's the only video there
//	Subs/Show.S01E01/3_English.srt               in a folder named after it
func Find(video string, subtitles []string, videosInDir int) []Sidecar {
	dir := filepath.Dir(video)
	base := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
	lowerBase := strings.ToLower(base)
	var out []Sidecar
	for _, p := range subtitles {
		ext := strings.ToLower(filepath.Ext(p))
		format, ok := formats[ext]
		if !ok {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		lowerName := strings.ToLower(name)
		parent := filepath.Dir(p)
		var tags string
		switch {
		case parent == dir:
			if lowerName == lowerBase {
				tags = ""
			} else if strings.HasPrefix(lowerName, lowerBase+".") {
				tags = name[len(base)+1:]
			} else {
				continue
			}
		case filepath.Dir(parent) == dir && subsDirs[strings.ToLower(filepath.Base(parent))]:
			if videosInDir != 1 {
				continue
			}
			tags = name
		case filepath.Dir(filepath.Dir(parent)) == dir && subsDirs[strings.ToLower(filepath.Base(filepath.Dir(parent)))] &&
			strings.EqualFold(filepath.Base(parent), base):
			tags = name
		default:
			continue
		}
		sc := Sidecar{Path: p, Format: format}
		sc.Language, sc.Title, sc.Forced, sc.HearingImpaired = readTags(tags)
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// readTags reads "en.forced", "English", "2_English", "eng.sdh",
// "Commentary.en".
func readTags(tags string) (lang, title string, forced, hi bool) {
	var rest []string
	for _, tok := range strings.FieldsFunc(tags, func(r rune) bool {
		return r == '.' || r == '_' || r == ' ' || r == '[' || r == ']' || r == '(' || r == ')'
	}) {
		lower := strings.ToLower(tok)
		switch {
		case lower == "forced" || lower == "foreign":
			forced = true
		case lower == "sdh" || lower == "cc" || lower == "hearing" || lower == "impaired":
			hi = true
		case lower == "hi" && lang != "":
			// After a language (Bazarr's "en.hi"), hearing impaired; on
			// its own, Hindi.
			hi = true
		case lower == "default" || isNumber(lower):
		case lang == "" && Language(lower) != "":
			lang = Language(lower)
		default:
			rest = append(rest, tok)
		}
	}
	return lang, strings.Join(rest, " "), forced, hi
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
