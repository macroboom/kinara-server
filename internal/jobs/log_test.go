package jobs

import (
	"errors"
	"testing"
)

func TestTaskLog(t *testing.T) {
	w := &Worker{}
	for i := 0; i < keepRecent+5; i++ {
		task := w.begin("previews", "previews", "/media/Show/S01/ep.mkv")
		if c := w.Status().Current; c == nil || c.Name != "ep.mkv" {
			t.Fatalf("current = %+v", c)
		}
		w.finish(task, i == keepRecent+4, "ok")
	}
	st := w.Status()
	if st.Current != nil || len(st.Recent) != keepRecent {
		t.Fatalf("current %+v, %d recent", st.Current, len(st.Recent))
	}
	if !st.Recent[0].Failed {
		t.Fatal("newest first")
	}
	if got := shortErr(errors.New("ffmpeg: exit 1\nlots of output")); got != "ffmpeg: exit 1" {
		t.Fatalf("shortErr = %q", got)
	}
}
