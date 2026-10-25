// Package store implements the maildir-style envelope store. Envelopes are JSON
// files in <root>/{pending,claimed,done,dead}/, named <id>.json. The daemon
// is the sole writer; the CLI reads. Writes are atomic (temp file + rename)
// and status transitions are file renames, so a crash never leaves a torn file.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/TamTunnel/switchboard/internal/envelope"
)

// Dir names mirror envelope statuses that persist on disk. "running" envelopes
// live in claimed/ with Status=running; "failed" is transient (retry or dead).
const (
	DirPending = "pending"
	DirClaimed = "claimed"
	DirDone    = "done"
	DirDead    = "dead"
	DirLogs    = "logs"
)

// AllDirs lists every directory the store manages.
var AllDirs = []string{DirPending, DirClaimed, DirDone, DirDead, DirLogs}

// Store is a rooted maildir store.
type Store struct {
	root string
}

// New creates a store rooted at dir, creating the directory tree.
func New(root string) (*Store, error) {
	s := &Store{root: root}
	for _, d := range AllDirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", d, err)
		}
	}
	return s, nil
}

// Root returns the store root directory.
func (s *Store) Root() string { return s.root }

// DirFor maps a status to its on-disk directory. Running lives in claimed.
func DirFor(status string) string {
	switch status {
	case envelope.StatusPending:
		return DirPending
	case envelope.StatusClaimed, envelope.StatusRunning:
		return DirClaimed
	case envelope.StatusDone:
		return DirDone
	case envelope.StatusDead:
		return DirDead
	default:
		return DirPending
	}
}

func (s *Store) path(dir, id string) string {
	return filepath.Join(s.root, dir, id+".json")
}

// Write persists env into the directory for its status, atomically.
func (s *Store) Write(env *envelope.Envelope) error {
	env.Touch()
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Join(s.root, DirFor(env.Status))
	tmp, err := os.CreateTemp(dir, ".tmp-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path(DirFor(env.Status), env.ID))
}

// Move transitions env to a new status directory, updating Status first.
func (s *Store) Move(env *envelope.Envelope, newStatus string) error {
	oldDir := DirFor(env.Status)
	oldPath := s.path(oldDir, env.ID)
	env.Status = newStatus
	if err := s.Write(env); err != nil {
		return err
	}
	newPath := s.path(DirFor(newStatus), env.ID)
	if oldDir != DirFor(newStatus) {
		// Write created the new file; remove the old one.
		_ = os.Remove(oldPath)
		_ = newPath
	}
	return nil
}

// Read loads one envelope from a specific directory.
func (s *Store) Read(dir, id string) (*envelope.Envelope, error) {
	data, err := os.ReadFile(s.path(dir, id))
	if err != nil {
		return nil, err
	}
	var env envelope.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("store: corrupt envelope %s: %w", id, err)
	}
	return &env, nil
}

// ReadAny searches all status directories for an envelope id.
func (s *Store) ReadAny(id string) (*envelope.Envelope, string, error) {
	for _, d := range []string{DirPending, DirClaimed, DirDone, DirDead} {
		if env, err := s.Read(d, id); err == nil {
			return env, d, nil
		}
	}
	return nil, "", fmt.Errorf("store: envelope %s not found", id)
}

// List returns all envelopes in a directory, newest first by CreatedAt.
func (s *Store) List(dir string) ([]*envelope.Envelope, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, dir))
	if err != nil {
		return nil, err
	}
	var out []*envelope.Envelope
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.root, dir, e.Name()))
		if err != nil {
			continue
		}
		var env envelope.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue // skip corrupt files; daemon logs them at startup
		}
		out = append(out, &env)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// AppendLog appends a line to an envelope's run log.
func (s *Store) AppendLog(id, line string) error {
	f, err := os.OpenFile(filepath.Join(s.root, DirLogs, id+".log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line + "\n")
	return err
}

// ReadLog returns an envelope's run log, or "" if none exists.
func (s *Store) ReadLog(id string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.root, DirLogs, id+".log"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}
