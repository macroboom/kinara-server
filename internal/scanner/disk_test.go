package scanner

import "testing"

func TestDetectDiskNeverPanics(t *testing.T) {
	for _, p := range []string{".", t.TempDir(), "/does/not/exist", ""} {
		k := DetectDisk(p)
		if WorkersFor(k) < 2 || WorkersFor(k) > 8 {
			t.Errorf("%q: %s gives %d workers", p, k, WorkersFor(k))
		}
		t.Logf("%q: %s", p, k)
	}
}

func TestWorkersFor(t *testing.T) {
	if WorkersFor(DiskHDD) != 2 || WorkersFor(DiskUnknown) != 4 || WorkersFor(DiskNetwork) != 6 {
		t.Error("unexpected worker counts")
	}
	if n := WorkersFor(DiskSSD); n < 2 || n > 8 {
		t.Errorf("SSD workers %d", n)
	}
}
