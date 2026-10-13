package scanner

import (
	"fmt"
	"testing"

	"kinora/internal/store"
)

func TestInferFrom(t *testing.T) {
	dir := `Scavengers Reign S01 1080p MAX WEB-DL H 264-FLUX\Scavengers.Reign.S01.1080p.MAX.WEB-DL.H.264-FLUX\`
	var sibs []store.Version
	for e := 2; e <= 12; e++ {
		sibs = append(sibs, store.Version{
			RelPath:     dir + fmt.Sprintf("Scavengers.Reign.S01E%02d.Title.1080p.MAX.WEB-DL.DDP5.1.H.264-FLUX.mkv", e),
			DurationSec: 1500,
		})
	}
	file := store.Version{RelPath: dir + "Scavengers.Reign.S01.1080p.MAX.WEB-DL.H.264-FLUX.mkv", DurationSec: 1440}

	p, ok := inferFrom(1, file, sibs)
	if !ok || p.Title != "Scavengers Reign" || p.Season != 1 || p.Episode != 1 {
		t.Fatalf("got %+v ok=%v, want Scavengers Reign S01E01", p, ok)
	}

	// A whole season in one file is far longer than an episode: leave it.
	file.DurationSec = 1500 * 12
	if _, ok := inferFrom(1, file, sibs); ok {
		t.Error("placed a season-length file as an episode")
	}

	// Two gaps (E01 and E02): can't tell which one it is.
	file.DurationSec = 1440
	if _, ok := inferFrom(1, file, sibs[1:]); ok {
		t.Error("placed a file when two episodes were missing")
	}

	// Siblings from another season don't count.
	if _, ok := inferFrom(2, file, sibs); ok {
		t.Error("placed a season 2 file using season 1 episodes")
	}
}
