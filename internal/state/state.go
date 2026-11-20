// Package state owns cardo's persistent ownership and rollback record
// (ADR-0004): what cardo believes is resident, what `up` must restore on
// failure, surviving host reboots.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrNoState means no state file exists: nothing is resident by cardo's
// record and there is no rollback target. Callers treat this as a typed
// condition, never as an empty-queue signal.
var ErrNoState = errors.New("no cardo state file")

// Record is the whole state document.
type Record struct {
	Version    int       `json:"version"`
	Slot       int       `json:"slot"`
	Active     string    `json:"active,omitempty"`
	ActiveAt   time.Time `json:"active_at,omitempty"`
	ActivePID  int       `json:"active_pid,omitempty"`
	Previous   string    `json:"previous,omitempty"`
	PreviousAt time.Time `json:"previous_at,omitempty"`
}

// Store is the state file location and its operations.
type Store struct {
	// Dir defaults to the XDG state dir in ResolveDir; tests set it directly.
	Dir string
}

const fileName = "state.json"

// ResolveDir returns $XDG_STATE_DIR/cardo (default ~/.local/state/cardo).
func ResolveDir(stateHome string) string {
	if stateHome == "" {
		stateHome = os.Getenv("XDG_STATE_DIR")
	}
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(stateHome, "cardo")
}

// currentVersion is the schema version Save writes and Load understands.
const currentVersion = 1

// Load reads the state record. Missing file is ErrNoState; corrupt content or
// an unknown schema version is a distinct error (a divergent record must be
// reported, not ignored, and never half-trusted as an ownership record).
func (s *Store) Load() (*Record, error) {
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoState
	}
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("corrupt state file %s: %w", s.path(), err)
	}
	// Version 0 is a record written without the field (or hand-authored);
	// anything else is a schema this cardo does not understand.
	if r.Version != 0 && r.Version != currentVersion {
		return nil, fmt.Errorf("unsupported state file version %d in %s: this cardo writes version %d; run `cardo down --forget` to start over",
			r.Version, s.path(), currentVersion)
	}
	return &r, nil
}

// Save atomically replaces the state record: temp file, fsync, rename, and a
// directory fsync so the rename itself is durable (the record is what tells a
// rebooted host which engine cardo owns).
func (s *Store) Save(r Record) error {
	r.Version = currentVersion
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, "state-*.json.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path()); err != nil {
		return err
	}
	return syncDir(s.Dir)
}

// syncDir flushes the directory entry so a rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Clear removes the state file. Clearing an absent file is fine.
func (s *Store) Clear() error {
	err := os.Remove(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) path() string {
	return filepath.Join(s.Dir, fileName)
}
