package scanner

import "testing"

func TestVideoTitle(t *testing.T) {
	cases := map[string]string{
		"Birthday_Party_2019.mp4":         "Birthday Party 2019",
		"trips/Beach.Day.mkv":             "Beach Day",
		"Already has spaces.v2.mov":       "Already has spaces.v2",
		"a/b/  messy   name  .avi":        "messy name",
		".mkv":                            "Untitled video",
		"Screen Recording 2024-01-02.mp4": "Screen Recording 2024-01-02",
	}
	for in, want := range cases {
		if got := VideoTitle(in); got != want {
			t.Errorf("VideoTitle(%q) = %q, want %q", in, got, want)
		}
	}
}
