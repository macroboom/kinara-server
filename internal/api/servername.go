package api

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// The server's name: what the apps list it as on the home network, and
// what the Kinara account shows. The owner can set one on the manager's
// Settings page; otherwise it's the computer's name ("DESKTOP-374S98U").

// serverName keeps the owner's name for the server in memory, read from
// the settings the first time it's needed.
type serverName struct {
	mu     sync.Mutex
	loaded bool
	v      string
}

func (n *serverName) set(v string) {
	n.mu.Lock()
	n.v, n.loaded = v, true
	n.mu.Unlock()
}

// Name is this server's name: the owner's, or the computer's.
func (s *Server) Name() string {
	s.name.mu.Lock()
	if !s.name.loaded && s.Store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if st, err := s.Store.GetSettings(ctx); err == nil {
			s.name.v, s.name.loaded = st.ServerName, true
		}
		cancel()
	}
	v := s.name.v
	s.name.mu.Unlock()
	if v != "" {
		return v
	}
	host, _ := os.Hostname()
	return host
}

const maxServerName = 40

// cleanServerName checks a name the owner typed: trimmed, at most 40
// characters, no control characters. "" means the computer's name.
func cleanServerName(v string) (string, error) {
	v = strings.Join(strings.Fields(v), " ")
	if utf8.RuneCountInString(v) > maxServerName {
		return "", errors.New("the server's name can be at most 40 characters")
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", errors.New("the server's name can't contain control characters")
		}
	}
	return v, nil
}
