// Package cloud links this server to a Kinara account and keeps it
// reachable from anywhere: it reports its addresses to kinara-cloud, opens
// a port on the router (UPnP or NAT-PMP), gets a certificate for its
// *.<id>.direct.<domain> names, serves HTTPS with it, and keeps relay
// connections open for players that can't connect directly (Roku,
// browsers).
//
// Everything here uses only the standard library.
package cloud

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Owner is the Kinara account the server is linked to.
type Owner struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// State is what survives restarts, in <data>/cloud/state.json.
type State struct {
	ServerID     string `json:"serverId"`
	ServerSecret string `json:"serverSecret"`
	Owner        Owner  `json:"owner"`
	// OwnerUserID is the local admin account the cloud owner signs in as.
	OwnerUserID int64     `json:"ownerUserId"`
	DirectZone  string    `json:"directZone"` // <id>.direct.<domain>
	CertName    string    `json:"certName"`   // *.<id>.direct.<domain>
	RelayHost   string    `json:"relayHost"`  // <id>.relay.<domain>
	IrohRelays  []string  `json:"irohRelays"`
	CloudKeyID  string    `json:"cloudKeyId"`
	CloudKey    string    `json:"cloudKey"` // base64 Ed25519 public key
	Linked      time.Time `json:"linked"`
}

type stateFile struct {
	path string
	mu   sync.Mutex
}

func (f *stateFile) load() (*State, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.ServerID == "" || s.ServerSecret == "" {
		return nil, nil
	}
	return &s, nil
}

func (f *stateFile) save(s *State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s == nil {
		err := os.Remove(f.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}
