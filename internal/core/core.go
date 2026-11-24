// Package core joins tmux panes, processes, and agent state into sessions,
// and derives project-level status from them.
package core

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/procs"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/tmux"
)

// StaleAfter is how long a finished session stays "ready" before it counts as quiet.
const StaleAfter = 24 * time.Hour

// Session is one live agent in a tmux pane.
type Session struct {
	// Key is the stable identity used for grouping. It is the agent's own
	// session ID when known, because tmux windows get renumbered and reused.
	Key  string
	Kind string
	PID  int
	Pane tmux.Pane
	Info agents.Info
}

// Title is the best short name for the session.
func (s Session) Title() string {
	if s.Info.Name != "" {
		return s.Info.Name
	}
	return s.Pane.WindowName
}

// State is the session state with staleness applied.
func (s Session) State(now time.Time) agents.State {
	st := s.Info.State
	if (st == agents.Ready || (st == agents.Working && s.Info.Scheduled != "")) && !s.Info.UpdatedAt.IsZero() && now.Sub(s.Info.UpdatedAt) > StaleAfter {
		return agents.Quiet
	}
	return st
}

// Sources are the inputs Discover needs. Tests replace them.
type Sources struct {
	Tmux    tmux.Client
	Procs   func() (procs.Table, error)
	Copilot *agents.Copilot
	Claude  *agents.Claude
	Started func(pid int) (time.Time, error)
	// SkipPane is Switchboard's own pane, so its summarizer is not listed.
	SkipPane string
}

// DefaultSources reads the real tmux server, process table, and agent directories.
func DefaultSources() Sources {
	return Sources{
		Tmux:     tmux.Client{Run: tmux.Exec},
		Procs:    procs.Snapshot,
		Copilot:  &agents.Copilot{Dir: agents.DefaultCopilotDir()},
		Claude:   &agents.Claude{Dir: agents.DefaultClaudeDir()},
		Started:  procs.StartTime,
		SkipPane: os.Getenv("TMUX_PANE"),
	}
}

var promptPattern = regexp.MustCompile(`(?i)(do you want to (proceed|make this edit|create|run)|\(y/n\)|\[y/n\]|❯\s*1\.\s*yes|allow (this|once)|approve\?|enter to confirm|esc to cancel)`)

// Discover finds agent sessions in tmux panes.
func Discover(src Sources) ([]Session, error) {
	panes, err := src.Tmux.ListPanes()
	if err != nil {
		return nil, err
	}
	table, err := src.Procs()
	if err != nil {
		return nil, err
	}
	locks := src.Copilot.Locks()
	claudeProcs := src.Claude.Procs()

	claudePerDir := map[string]int{}
	for _, p := range panes {
		if _, kind, ok := table.FindAgent(p.PanePID); ok && kind == procs.Claude {
			claudePerDir[p.Path]++
		}
	}

	var out []Session
	for _, p := range panes {
		if src.SkipPane != "" && p.PaneID == src.SkipPane {
			continue
		}
		proc, kind, ok := table.FindAgent(p.PanePID)
		if !ok {
			continue
		}
		s := Session{Kind: kind, PID: proc.PID, Pane: p, Info: agents.Info{Kind: kind, Cwd: p.Path, UpdatedAt: p.Activity}}
		switch kind {
		case procs.Copilot:
			for _, d := range table.Descendants(proc.PID) {
				if id, ok := locks[d.PID]; ok {
					if info, err := src.Copilot.Read(id); err == nil || info.ID != "" {
						s.Info = info
					}
					break
				}
			}
		case procs.Claude:
			cp, found := ClaudeProcFor(table, proc.PID, claudeProcs)
			if !found && claudePerDir[p.Path] == 1 && src.Started != nil {
				if started, err := src.Started(proc.PID); err == nil {
					cp, found = src.Claude.Latest(p.Path, started.Add(-time.Second))
				}
			}
			if found {
				if info, err := src.Claude.Read(cp); err == nil || info.ID != "" {
					s.Info = info
				}
			}
		}
		if s.Info.ID != "" {
			s.Key = kind + ":" + s.Info.ID
		} else {
			s.Key = "pane:" + p.SessionName + ":" + p.WindowID + ":" + p.PaneID
		}
		if s.Info.Cwd == "" {
			s.Info.Cwd = p.Path
		}
		// Copilot logs permission prompts. Claude Code and unidentified
		// sessions do not, so look at the screen for a prompt.
		if kind != procs.Copilot || s.Info.ID == "" {
			if text, err := src.Tmux.Capture(p.PaneID, 15); err == nil && promptPattern.MatchString(text) {
				s.Info.State = agents.NeedsYou
				if s.Info.Reason == "" {
					s.Info.Reason = "Waiting at a prompt in the terminal"
				}
			}
		}
		if s.Info.UpdatedAt.Before(p.Activity) && s.Info.State == agents.Unknown {
			s.Info.UpdatedAt = p.Activity
		}
		out = append(out, s)
	}
	return out, nil
}

// ClaudeProcFor finds a registration for the Claude process or one of its children.
func ClaudeProcFor(table procs.Table, pid int, reg map[int]agents.ClaudeProc) (agents.ClaudeProc, bool) {
	for _, d := range table.Descendants(pid) {
		if cp, ok := reg[d.PID]; ok {
			return cp, true
		}
	}
	return agents.ClaudeProc{}, false
}

// Rollup summarizes a group of sessions.
type Rollup struct {
	State     agents.State
	NeedsYou  int
	Working   int
	Ready     int
	Total     int
	UpdatedAt time.Time
}

// Roll computes the rollup for sessions.
func Roll(ss []Session, now time.Time) Rollup {
	var r Rollup
	for _, s := range ss {
		st := s.State(now)
		r.Total++
		switch st {
		case agents.NeedsYou:
			r.NeedsYou++
		case agents.Working:
			r.Working++
		case agents.Ready:
			r.Ready++
		}
		if st > r.State {
			r.State = st
		}
		if s.Info.UpdatedAt.After(r.UpdatedAt) {
			r.UpdatedAt = s.Info.UpdatedAt
		}
	}
	if r.Total == 0 {
		r.State = agents.Quiet
	}
	return r
}

// SortSessions orders by urgency, then most recent first.
func SortSessions(ss []Session, now time.Time) {
	sort.SliceStable(ss, func(i, j int) bool {
		a, b := ss[i].State(now), ss[j].State(now)
		if a != b {
			return a > b
		}
		return ss[i].Info.UpdatedAt.After(ss[j].Info.UpdatedAt)
	})
}

// Item is one dated line in a project's recent history.
type Item struct {
	At      time.Time
	Text    string
	Session string
}

// RecentResults merges finished-work summaries across sessions, newest first.
func RecentResults(ss []Session, n int) []Item {
	var out []Item
	for _, s := range ss {
		for _, r := range s.Info.Results {
			out = append(out, Item{At: r.At, Text: r.Text, Session: s.Title()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// Suggestion is a guessed home for an unassigned session.
type Suggestion struct {
	Project string
	Thread  string
	Why     string
}

var stop = map[string]bool{"the": true, "a": true, "and": true, "of": true, "for": true, "to": true, "in": true, "copilot": true, "claude": true, "zsh": true, "bash": true}

func tokens(s string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if len(t) >= 3 && !stop[t] {
			out = append(out, t)
		}
	}
	return out
}

func overlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y || (len(x) >= 4 && len(y) >= 4 && (strings.HasPrefix(x, y) || strings.HasPrefix(y, x))) {
				return true
			}
		}
	}
	return false
}

// defaultBranches are shared by unrelated work, so they say little about which
// project a session belongs to.
var defaultBranches = map[string]bool{"": true, "main": true, "master": true, "trunk": true, "develop": true, "dev": true}

// Suggest guesses where an unassigned session belongs. It prefers name
// matches against threads, then projects, then a feature branch that only one
// project uses. It returns false when nothing is convincing.
func Suggest(s Session, st *store.State, all []Session) (Suggestion, bool) {
	names := tokens(s.Title() + " " + s.Pane.WindowName)
	for _, p := range st.Projects {
		for _, t := range p.Threads {
			if overlap(names, tokens(t.Name)) {
				return Suggestion{Project: p.ID, Thread: t.ID, Why: "name matches thread"}, true
			}
		}
	}
	for _, p := range st.Projects {
		if overlap(names, tokens(p.Name)) {
			return Suggestion{Project: p.ID, Why: "name matches project"}, true
		}
	}
	if s.Info.Repo == "" || defaultBranches[s.Info.Branch] {
		return Suggestion{}, false
	}
	owners := map[string]store.Assignment{}
	for _, o := range all {
		a, ok := st.Assign[o.Key]
		if !ok || o.Info.Repo != s.Info.Repo || o.Info.Branch != s.Info.Branch {
			continue
		}
		owners[a.Project] = a
	}
	if len(owners) == 1 {
		for pid := range owners {
			return Suggestion{Project: pid, Why: "same branch"}, true
		}
	}
	return Suggestion{}, false
}

// Ago formats a duration in the compact style used across the UI.
func Ago(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
