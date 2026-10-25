// Package config persists CLI state as JSON files under ~/.grok/bot-tunnel.
package config

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Auth holds the user's OAuth tokens and selected identity.
type Auth struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AuthID       string `json:"auth_id,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	Email        string `json:"email,omitempty"`
	// ExpiresAtMS is when AccessToken expires, if the server said so. Zero
	// means unknown; JWT access tokens carry their own expiry.
	ExpiresAtMS int64 `json:"expires_at_ms,omitempty"`
}

// Machine identifies this computer to the server.
type Machine struct {
	MachineID string `json:"machine_id"`
	Label     string `json:"label"`
}

// Computer is a short-lived credential for the user-computer channel.
type Computer struct {
	Credential          string `json:"credential"`
	AccountScope        string `json:"account_scope,omitempty"`
	Backend             string `json:"backend,omitempty"`
	MachineID           string `json:"machine_id,omitempty"`
	ExpiresAtMS         int64  `json:"expires_at_ms"`
	ServerAuthoritative bool   `json:"server_authoritative"`
}

// Store reads and writes named JSON documents in Dir.
type Store struct{ Dir string }

// Default returns the store in the user's home directory.
func Default() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: filepath.Join(home, ".grok", "bot-tunnel")}, nil
}

// UUID returns a random RFC 4122 version 4 UUID.
func UUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Store) path(name string) string {
	return filepath.Join(s.Dir, name+".json")
}

// Load decodes the named document into v.
func (s *Store) Load(name string, v any) error {
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// Save atomically replaces the named document with v. The directory and the
// file are readable only by the owner because they hold secrets.
func (s *Store) Save(name string, v any) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil { //nolint:gosec // directories need the execute bit
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.Dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path(name))
}

// lockPoll is how often Lock retries while another process holds the lock.
const lockPoll = 50 * time.Millisecond

// Lock takes an exclusive advisory lock shared by every process using the
// store and returns a function that releases it. It gives up when ctx ends.
func (s *Store) Lock(ctx context.Context) (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		// Closing the file releases the lock, also if the process dies.
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // fds fit in int
		if err == nil {
			return func() { f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		t := time.NewTimer(lockPoll)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// Machine loads the machine identity, creating it on first use.
func (s *Store) Machine() (Machine, error) {
	var m Machine
	err := s.Load("machine", &m)
	switch {
	case err == nil && m.MachineID == "":
		return m, errors.New("invalid machine.json: empty machine_id")
	case err == nil:
		return m, nil
	case !errors.Is(err, os.ErrNotExist):
		return m, err
	}
	m.MachineID = UUID()
	if host, err := os.Hostname(); err == nil {
		m.Label = host
	}
	return m, s.Save("machine", m)
}
