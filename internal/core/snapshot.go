package core

import (
	"os"
	"strings"
	"time"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/procs"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/tmux"
)

// SavedFromSessions converts live resumable sessions into snapshot records.
// Unidentified pane-only sessions without an agent ID are skipped because
// there is no conversation ID to resume.
func SavedFromSessions(ss []Session, now time.Time) []store.SavedSession {
	var out []store.SavedSession
	for _, s := range ss {
		if s.Offline || s.Info.ID == "" || s.Key == "" {
			continue
		}
		if s.Kind != procs.Copilot && s.Kind != procs.Claude {
			continue
		}
		seen := s.Info.UpdatedAt
		if seen.IsZero() {
			seen = now
		}
		cwd := s.Info.Cwd
		if cwd == "" {
			cwd = s.Pane.Path
		}
		out = append(out, store.SavedSession{
			Key:        s.Key,
			Kind:       s.Kind,
			ID:         s.Info.ID,
			Title:      s.Title(),
			WindowName: s.Pane.WindowName,
			Cwd:        cwd,
			Repo:       s.Info.Repo,
			Branch:     s.Info.Branch,
			SeenAt:     seen,
		})
	}
	return out
}

// HydrateSaved turns saved snapshot entries into offline Session values,
// enriching each from its on-disk Copilot or Claude transcript when available.
func HydrateSaved(saved []store.SavedSession, src Sources) []Session {
	out := make([]Session, 0, len(saved))
	for _, sv := range saved {
		if sv.Key == "" || sv.ID == "" {
			continue
		}
		info := agents.Info{
			Kind:      sv.Kind,
			ID:        sv.ID,
			Name:      sv.Title,
			Cwd:       sv.Cwd,
			Repo:      sv.Repo,
			Branch:    sv.Branch,
			State:     agents.Offline,
			UpdatedAt: sv.SeenAt,
		}
		switch sv.Kind {
		case procs.Copilot:
			if src.Copilot != nil {
				if read, err := src.Copilot.Read(sv.ID); err == nil || read.ID != "" {
					mergeSavedInfo(&read, sv)
					info = read
				}
			}
		case procs.Claude:
			if src.Claude != nil && sv.Cwd != "" {
				if read, err := src.Claude.Read(agents.ClaudeProc{SessionID: sv.ID, Cwd: sv.Cwd}); err == nil || read.ID != "" {
					mergeSavedInfo(&read, sv)
					info = read
				}
			}
		}
		out = append(out, Session{
			Key:     sv.Key,
			Kind:    sv.Kind,
			Pane:    tmux.Pane{WindowName: sv.WindowName, Path: info.Cwd},
			Info:    info,
			Offline: true,
		})
	}
	return out
}

// SavedFromAssigned looks up an assigned session key (copilot:<id>) on disk so
// a previously closed session can be reopened.
func SavedFromAssigned(key string, a store.Assignment, src Sources) (store.SavedSession, bool) {
	kind, id, ok := strings.Cut(key, ":")
	if !ok || id == "" {
		return store.SavedSession{}, false
	}
	switch kind {
	case procs.Copilot:
		if src.Copilot == nil {
			return store.SavedSession{}, false
		}
		info, err := src.Copilot.Read(id)
		if err != nil && info.Name == "" && info.Cwd == "" && info.UpdatedAt.IsZero() {
			return store.SavedSession{}, false
		}
		if info.Name == "" && info.Cwd == "" && info.UpdatedAt.IsZero() {
			return store.SavedSession{}, false
		}
		title := info.Name
		if title == "" {
			title = a.Label
		}
		seen := info.UpdatedAt
		if seen.IsZero() {
			seen = a.At
		}
		return store.SavedSession{
			Key:    key,
			Kind:   kind,
			ID:     id,
			Title:  title,
			Cwd:    info.Cwd,
			Repo:   info.Repo,
			Branch: info.Branch,
			SeenAt: seen,
		}, true
	}
	return store.SavedSession{}, false
}

func mergeSavedInfo(info *agents.Info, sv store.SavedSession) {
	if info.Kind == "" {
		info.Kind = sv.Kind
	}
	if info.ID == "" {
		info.ID = sv.ID
	}
	if info.Name == "" {
		info.Name = sv.Title
	}
	if info.Cwd == "" {
		info.Cwd = sv.Cwd
	}
	if info.Repo == "" {
		info.Repo = sv.Repo
	}
	if info.Branch == "" {
		info.Branch = sv.Branch
	}
	if info.UpdatedAt.IsZero() {
		info.UpdatedAt = sv.SeenAt
	}
}

// DiscoverWithSnapshot discovers live sessions, syncs snapPath against the
// current tmux server ID, and appends any recoverable offline sessions from
// previous tmux servers.
func DiscoverWithSnapshot(src Sources, snapPath string, now time.Time) ([]Session, error) {
	live, err := Discover(src)
	if err != nil {
		return nil, err
	}
	if snapPath == "" {
		return live, nil
	}
	serverID, _ := src.Tmux.ServerID()
	savedLive := SavedFromSessions(live, now)
	snap, err := store.UpdateSnapshot(snapPath, func(s *store.Snapshot) (bool, error) {
		return s.Sync(serverID, savedLive, now), nil
	})
	if err != nil {
		return live, nil
	}
	liveKeys := make(map[string]bool, len(live))
	for _, s := range live {
		liveKeys[s.Key] = true
	}
	var missing []store.SavedSession
	for _, sv := range snap.Recoverable {
		if !liveKeys[sv.Key] {
			missing = append(missing, sv)
		}
	}
	if len(missing) == 0 {
		return live, nil
	}
	return append(live, HydrateSaved(missing, src)...), nil
}

// ResumeCommand returns the shell command to resume an agent session by ID.
func ResumeCommand(kind, id string) string {
	if id == "" {
		return ""
	}
	arg := safeShellArg(id)
	switch kind {
	case procs.Copilot:
		return "copilot --resume=" + arg
	case procs.Claude:
		return "claude --resume " + arg
	}
	return ""
}

func safeShellArg(s string) string {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// ResumeDir returns cwd when it still exists as a directory, otherwise the
// user's home directory, reporting whether it fell back.
func ResumeDir(cwd string) (dir string, fallback bool) {
	if cwd != "" {
		if st, err := os.Stat(cwd); err == nil && st.IsDir() {
			return cwd, false
		}
		if home, err := os.UserHomeDir(); err == nil {
			return home, true
		}
	}
	home, _ := os.UserHomeDir()
	return home, false
}

var genericWindowNames = map[string]bool{
	"": true, "bash": true, "zsh": true, "sh": true, "fish": true,
	"copilot": true, "claude": true, "node": true,
}

// ResumeWindowName picks a clean tmux window name for a restored session.
func ResumeWindowName(s Session) string {
	w := strings.TrimSpace(s.Pane.WindowName)
	if !genericWindowNames[strings.ToLower(w)] {
		return w
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s.Title()) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 20 {
		slug = strings.Trim(slug[:20], "-")
	}
	if slug != "" {
		return slug
	}
	if s.Kind != "" {
		return s.Kind
	}
	return "agent"
}
