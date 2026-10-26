// Package subs finds subtitle files next to videos and gets subtitles from
// OpenSubtitles.
package subs

import "strings"

// languages maps names and codes people put in subtitle file names
// (English, en, eng) to ISO 639-2 codes, as files' own tracks use, and back
// to the two-letter codes OpenSubtitles uses.
var languages = []struct {
	code3, code2 string
	names        []string
	more         []string // other three-letter codes (T and B forms)
}{
	{"eng", "en", []string{"english"}, nil},
	{"spa", "es", []string{"spanish", "espanol", "español", "castellano", "latino"}, nil},
	{"fre", "fr", []string{"french", "francais", "français"}, []string{"fra"}},
	{"ger", "de", []string{"german", "deutsch"}, []string{"deu"}},
	{"ita", "it", []string{"italian", "italiano"}, nil},
	{"por", "pt", []string{"portuguese", "portugues", "português", "brazilian"}, nil},
	{"dut", "nl", []string{"dutch", "nederlands"}, []string{"nld"}},
	{"swe", "sv", []string{"swedish", "svenska"}, nil},
	{"nor", "no", []string{"norwegian", "norsk"}, []string{"nob", "nno"}},
	{"dan", "da", []string{"danish", "dansk"}, nil},
	{"fin", "fi", []string{"finnish", "suomi"}, nil},
	{"ice", "is", []string{"icelandic"}, []string{"isl"}},
	{"pol", "pl", []string{"polish", "polski"}, nil},
	{"cze", "cs", []string{"czech"}, []string{"ces"}},
	{"slo", "sk", []string{"slovak"}, []string{"slk"}},
	{"slv", "sl", []string{"slovenian", "slovene"}, nil},
	{"hun", "hu", []string{"hungarian", "magyar"}, nil},
	{"rum", "ro", []string{"romanian"}, []string{"ron"}},
	{"bul", "bg", []string{"bulgarian"}, nil},
	{"hrv", "hr", []string{"croatian"}, nil},
	{"srp", "sr", []string{"serbian"}, nil},
	{"bos", "bs", []string{"bosnian"}, nil},
	{"gre", "el", []string{"greek"}, []string{"ell"}},
	{"tur", "tr", []string{"turkish"}, nil},
	{"rus", "ru", []string{"russian"}, nil},
	{"ukr", "uk", []string{"ukrainian"}, nil},
	{"est", "et", []string{"estonian"}, nil},
	{"lav", "lv", []string{"latvian"}, nil},
	{"lit", "lt", []string{"lithuanian"}, nil},
	{"ara", "ar", []string{"arabic"}, nil},
	{"heb", "he", []string{"hebrew"}, nil},
	{"per", "fa", []string{"persian", "farsi"}, []string{"fas"}},
	{"hin", "hi", []string{"hindi"}, nil},
	{"ben", "bn", []string{"bengali", "bangla"}, nil},
	{"tam", "ta", []string{"tamil"}, nil},
	{"tel", "te", []string{"telugu"}, nil},
	{"mal", "ml", []string{"malayalam"}, nil},
	{"kan", "kn", []string{"kannada"}, nil},
	{"mar", "mr", []string{"marathi"}, nil},
	{"urd", "ur", []string{"urdu"}, nil},
	{"tha", "th", []string{"thai"}, nil},
	{"vie", "vi", []string{"vietnamese"}, nil},
	{"ind", "id", []string{"indonesian"}, nil},
	{"may", "ms", []string{"malay"}, []string{"msa"}},
	{"fil", "tl", []string{"filipino", "tagalog"}, []string{"tgl"}},
	{"chi", "zh", []string{"chinese", "mandarin", "chs", "cht", "simplified", "traditional"}, []string{"zho"}},
	{"jpn", "ja", []string{"japanese"}, nil},
	{"kor", "ko", []string{"korean"}, nil},
	{"cat", "ca", []string{"catalan"}, nil},
	{"baq", "eu", []string{"basque"}, []string{"eus"}},
	{"glg", "gl", []string{"galician"}, nil},
}

var byToken = func() map[string]int {
	m := map[string]int{}
	for i, l := range languages {
		m[l.code3], m[l.code2] = i, i
		for _, n := range l.names {
			m[n] = i
		}
		for _, c := range l.more {
			m[c] = i
		}
	}
	return m
}()

// Language reads a language from a word in a file name or a code: "en",
// "eng", "English", "pt-BR". It returns the three-letter code, or "" when
// the word isn't a language.
func Language(word string) string {
	w := strings.ToLower(strings.TrimSpace(word))
	if i, ok := byToken[w]; ok {
		return languages[i].code3
	}
	// "pt-br", "en_us", "zh-cn"
	if base, _, ok := strings.Cut(strings.ReplaceAll(w, "_", "-"), "-"); ok {
		if i, ok := byToken[base]; ok {
			return languages[i].code3
		}
	}
	return ""
}

// Code2 is the two-letter code for a language ("eng" -> "en"); "" if unknown.
func Code2(lang string) string {
	if i, ok := byToken[strings.ToLower(lang)]; ok {
		return languages[i].code2
	}
	return ""
}
