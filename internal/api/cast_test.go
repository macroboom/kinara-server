package api

import (
	"strings"
	"testing"
)

func TestLANBasesUsePortAndPrivateAddresses(t *testing.T) {
	s := &Server{}
	if got := s.lanBases(); len(got) != 0 {
		t.Fatalf("no port: %v", got)
	}
	s.LANPort = 8420
	for _, b := range s.lanBases() {
		if !strings.HasPrefix(b, "http://") || !strings.HasSuffix(b, ":8420") || strings.HasPrefix(b, "http://100.") || strings.HasPrefix(b, "http://127.") {
			t.Fatalf("unexpected address %q", b)
		}
	}
}
