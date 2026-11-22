// Package store persists projects, threads, and session assignments.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const version = 1

// Thread is one line of work inside a project.
type Thread struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Project is a long-lived goal that outlives individual agent sessions.
type Project struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Purpose    string    `json:"purpose,omitempty"`
	Threads    []Thread  `json:"threads,omitempty"`
	Created    time.Time `json:"created"`
	LastViewed time.Time `json:"lastViewed,omitempty"`
}

// Assignment places a session in a project and, optionally, a thread.
type Assignment struct {
	Project string    `json:"project"`
	Thread  string    `json:"thread,omitempty"`
	Label   string    `json:"label,omitempty"` // last known title, for humans reading the file
	At      time.Time `json:"at"`
}

// State is the whole persisted file.
type State struct {
	Version  int                   `json:"version"`
	Projects []Project             `json:"projects"`
	Assign   map[string]Assignment `json:"assignments"`
}

// DefaultPath is $XDG_STATE_HOME/switchboard/state.json or ~/.local/state/switchboard/state.json.
func DefaultPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "switchboard", "state.json")
}

// Load reads state. A missing file is an empty state; a corrupt file is an
// error so Switchboard never overwrites groupings it could not read.
func Load(path string) (*State, error) {
	s := &State{Version: version, Assign: map[string]Assignment{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("read %s: %w (fix or move the file; Switchboard will not overwrite it)", path, err)
	}
	if s.Version > version {
		return nil, fmt.Errorf("%s was written by a newer Switchboard (version %d)", path, s.Version)
	}
	if s.Assign == nil {
		s.Assign = map[string]Assignment{}
	}
	return s, nil
}

// Save writes state atomically.
func (s *State) Save(path string) error {
	s.Version = version
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
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

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Project returns the project with id, or nil.
func (s *State) Project(id string) *Project {
	for i := range s.Projects {
		if s.Projects[i].ID == id {
			return &s.Projects[i]
		}
	}
	return nil
}

// Thread returns the thread with id in p, or nil.
func (p *Project) Thread(id string) *Thread {
	for i := range p.Threads {
		if p.Threads[i].ID == id {
			return &p.Threads[i]
		}
	}
	return nil
}

// AddProject creates a project.
func (s *State) AddProject(name string, now time.Time) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("project name is empty")
	}
	s.Projects = append(s.Projects, Project{ID: newID(), Name: name, Created: now})
	return &s.Projects[len(s.Projects)-1], nil
}

// AddThread creates a thread in project pid.
func (s *State) AddThread(pid, name string) (*Thread, error) {
	p := s.Project(pid)
	name = strings.TrimSpace(name)
	if p == nil {
		return nil, errors.New("no such project")
	}
	if name == "" {
		return nil, errors.New("thread name is empty")
	}
	p.Threads = append(p.Threads, Thread{ID: newID(), Name: name})
	return &p.Threads[len(p.Threads)-1], nil
}

// AssignSession places a session; an empty project unassigns it.
func (s *State) AssignSession(key, pid, tid, label string, now time.Time) {
	if pid == "" {
		delete(s.Assign, key)
		return
	}
	s.Assign[key] = Assignment{Project: pid, Thread: tid, Label: label, At: now}
}

// DeleteProject removes a project and unassigns its sessions.
func (s *State) DeleteProject(pid string) {
	for i := range s.Projects {
		if s.Projects[i].ID == pid {
			s.Projects = append(s.Projects[:i], s.Projects[i+1:]...)
			break
		}
	}
	for k, a := range s.Assign {
		if a.Project == pid {
			delete(s.Assign, k)
		}
	}
}

// DeleteThread removes a thread; its sessions stay in the project.
func (s *State) DeleteThread(pid, tid string) {
	p := s.Project(pid)
	if p == nil {
		return
	}
	for i := range p.Threads {
		if p.Threads[i].ID == tid {
			p.Threads = append(p.Threads[:i], p.Threads[i+1:]...)
			break
		}
	}
	for k, a := range s.Assign {
		if a.Project == pid && a.Thread == tid {
			a.Thread = ""
			s.Assign[k] = a
		}
	}
}

// Update applies change to the latest state on disk and saves it, holding an
// exclusive lock so concurrent Switchboard instances merge instead of
// overwriting each other. It returns the saved state.
func Update(path string, change func(*State) error) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	s, err := Load(path)
	if err != nil {
		return nil, err
	}
	if err := change(s); err != nil {
		return nil, err
	}
	if err := s.Save(path); err != nil {
		return nil, err
	}
	return s, nil
}

// Stamp identifies a version of the state file so callers can tell when
// another process changed it.
func Stamp(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d", fi.ModTime().UnixNano(), fi.Size())
}
