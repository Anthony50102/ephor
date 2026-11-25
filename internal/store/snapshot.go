package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const snapshotVersion = 1

// snapshotHeartbeat refreshes timestamps during steady state without rewriting
// snapshot.json on every 3-second poll.
const snapshotHeartbeat = 15 * time.Minute

// SavedSession is the metadata needed to remember and resume a Copilot or
// Claude session after tmux restarts or the machine reboots.
type SavedSession struct {
	Key        string    `json:"key"`
	Kind       string    `json:"kind"`
	ID         string    `json:"id"`
	Title      string    `json:"title,omitempty"`
	WindowName string    `json:"windowName,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
	Repo       string    `json:"repo,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	SeenAt     time.Time `json:"seenAt"`
}

func (a SavedSession) sameMeta(b SavedSession) bool {
	return a.Key == b.Key &&
		a.Kind == b.Kind &&
		a.ID == b.ID &&
		a.Title == b.Title &&
		a.WindowName == b.WindowName &&
		a.Cwd == b.Cwd &&
		a.Repo == b.Repo &&
		a.Branch == b.Branch
}

// Snapshot records the live resumable sessions on the current tmux server and
// preserves sessions from earlier servers in Recoverable until they are
// restored or dismissed.
type Snapshot struct {
	Version     int            `json:"version"`
	ServerID    string         `json:"serverId,omitempty"`
	UpdatedAt   time.Time      `json:"updatedAt,omitempty"`
	Live        []SavedSession `json:"live,omitempty"`
	Recoverable []SavedSession `json:"recoverable,omitempty"`
}

// DefaultSnapshotPath places snapshot.json alongside statePath.
func DefaultSnapshotPath(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(statePath), "snapshot.json")
}

// LoadSnapshot reads a snapshot file. Missing files return an empty snapshot.
func LoadSnapshot(path string) (*Snapshot, error) {
	s := &Snapshot{Version: snapshotVersion}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if s.Version > snapshotVersion {
		return nil, fmt.Errorf("%s was written by a newer Ephor (version %d)", path, s.Version)
	}
	return s, nil
}

// Save writes the snapshot atomically.
func (s *Snapshot) Save(path string) error {
	s.Version = snapshotVersion
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// UpdateSnapshot locks, loads, edits, and saves snapshot.json when fn reports
// a change.
func UpdateSnapshot(path string, fn func(*Snapshot) (bool, error)) (*Snapshot, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	s, err := LoadSnapshot(path)
	if err != nil {
		return nil, err
	}
	changed, err := fn(s)
	if err != nil {
		return nil, err
	}
	if changed {
		if err := s.Save(path); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Sync reconciles the snapshot with the live resumable sessions on serverID.
// When serverID changes (tmux restarted or machine rebooted), the previous
// server's Live entries move into Recoverable instead of being overwritten.
func (s *Snapshot) Sync(serverID string, live []SavedSession, now time.Time) bool {
	if serverID == "" {
		return false
	}
	clean := normalizeSaved(live)
	liveKeys := make(map[string]bool, len(clean))
	for _, it := range clean {
		liveKeys[it.Key] = true
	}

	if s.ServerID != "" && s.ServerID != serverID {
		merged := mergeSaved(s.Recoverable, s.Live, liveKeys)
		s.ServerID = serverID
		s.Live = clean
		s.Recoverable = merged
		s.UpdatedAt = now
		return true
	}

	changed := s.ServerID != serverID
	s.ServerID = serverID

	filtered := filterOutSaved(s.Recoverable, liveKeys)
	if len(filtered) != len(s.Recoverable) {
		s.Recoverable = filtered
		changed = true
	}

	if !sameSavedList(s.Live, clean) || (len(clean) > 0 && (s.UpdatedAt.IsZero() || now.Sub(s.UpdatedAt) >= snapshotHeartbeat)) {
		s.Live = clean
		s.UpdatedAt = now
		changed = true
	}
	return changed
}

// AddRecoverable merges items into Recoverable (ignoring any already live on
// this server) and reports whether Recoverable changed.
func (s *Snapshot) AddRecoverable(items ...SavedSession) bool {
	if len(items) == 0 {
		return false
	}
	liveKeys := make(map[string]bool, len(s.Live))
	for _, it := range s.Live {
		liveKeys[it.Key] = true
	}
	merged := mergeSaved(s.Recoverable, items, liveKeys)
	if sameSavedList(s.Recoverable, merged) {
		return false
	}
	s.Recoverable = merged
	return true
}

// Dismiss removes keys from Recoverable.
func (s *Snapshot) Dismiss(keys ...string) bool {
	if len(keys) == 0 || len(s.Recoverable) == 0 {
		return false
	}
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	next := filterOutSaved(s.Recoverable, drop)
	if len(next) == len(s.Recoverable) {
		return false
	}
	s.Recoverable = next
	return true
}

// AllRecoverable returns the sessions that can be restored right now. When
// serverID matches s.ServerID, Live sessions are already running so only
// Recoverable is returned; when tmux is not running (empty serverID) or a new
// server is running, both Recoverable and Live are returned.
func (s *Snapshot) AllRecoverable(serverID string) []SavedSession {
	if serverID != "" && serverID == s.ServerID {
		return append([]SavedSession(nil), s.Recoverable...)
	}
	return mergeSaved(s.Recoverable, s.Live, nil)
}

func normalizeSaved(in []SavedSession) []SavedSession {
	byKey := map[string]SavedSession{}
	for _, it := range in {
		if it.Key == "" || it.ID == "" {
			continue
		}
		if prev, ok := byKey[it.Key]; !ok || it.SeenAt.After(prev.SeenAt) {
			byKey[it.Key] = it
		}
	}
	out := make([]SavedSession, 0, len(byKey))
	for _, it := range byKey {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func mergeSaved(a, b []SavedSession, exclude map[string]bool) []SavedSession {
	byKey := map[string]SavedSession{}
	add := func(list []SavedSession) {
		for _, it := range list {
			if it.Key == "" || it.ID == "" || exclude[it.Key] {
				continue
			}
			if prev, ok := byKey[it.Key]; !ok || it.SeenAt.After(prev.SeenAt) {
				byKey[it.Key] = it
			}
		}
	}
	add(a)
	add(b)
	out := make([]SavedSession, 0, len(byKey))
	for _, it := range byKey {
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SeenAt.Equal(out[j].SeenAt) {
			return out[i].SeenAt.After(out[j].SeenAt)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func filterOutSaved(in []SavedSession, drop map[string]bool) []SavedSession {
	if len(in) == 0 || len(drop) == 0 {
		return in
	}
	var out []SavedSession
	for _, it := range in {
		if !drop[it.Key] {
			out = append(out, it)
		}
	}
	return out
}

func sameSavedList(a, b []SavedSession) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].sameMeta(b[i]) {
			return false
		}
	}
	return true
}
