package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/tmux"
	tea "github.com/charmbracelet/bubbletea"
)

type restoreMode int

const (
	restoreBackground restoreMode = iota
	restoreJump
	restoreDock
)

// restoreRefreshDelay gives a newly launched CLI a moment to acquire its lock
// file before Switchboard triggers an extra refresh.
const restoreRefreshDelay = 1500 * time.Millisecond

func (m Model) offlineIn(sc sideItem) []core.Session {
	var out []core.Session
	for _, s := range m.sessionsIn(sc) {
		if s.Offline {
			out = append(out, s)
		}
	}
	return out
}

func (m Model) liveCount(sc sideItem) int {
	n := 0
	for _, s := range m.sessionsIn(sc) {
		if !s.Offline {
			n++
		}
	}
	return n
}

// focusedOffline returns the offline session currently under the cursor, if any.
func (m Model) focusedOffline() (core.Session, bool) {
	if m.focus == focusCenter && strings.HasPrefix(m.cardID, "sess:") {
		key := strings.TrimPrefix(m.cardID, "sess:")
		if s, ok := m.sessionByKey(key); ok && s.Offline {
			return s, true
		}
	}
	if m.focus == focusSessions {
		if s, ok := m.selected(); ok && s.Offline {
			return s, true
		}
	}
	return core.Session{}, false
}

// restoreTargetScope chooses which scope bulk restore/dismiss applies to.
func (m Model) restoreTargetScope() (sideItem, string) {
	sc := m.scope()
	if m.focus == focusCenter {
		switch {
		case strings.HasPrefix(m.cardID, "proj:"):
			pid := strings.TrimPrefix(m.cardID, "proj:")
			if m.st.Project(pid) != nil {
				return sideItem{kind: sideProject, pid: pid}, m.placeName(pid, "")
			}
		case strings.HasPrefix(m.cardID, "thread:"):
			tid := strings.TrimPrefix(m.cardID, "thread:")
			if p := m.st.Project(sc.pid); p != nil && p.Thread(tid) != nil {
				return sideItem{kind: sideThread, pid: sc.pid, tid: tid}, m.placeName(sc.pid, tid)
			}
		}
	}
	switch sc.kind {
	case sideProject, sideThread:
		return sc, m.placeName(sc.pid, sc.tid)
	case sideInbox:
		return sc, "Unassigned"
	default:
		return sideItem{kind: sideOverview}, ""
	}
}

func (m *Model) removeOfflineKeys(keys ...string) {
	if len(keys) == 0 {
		return
	}
	drop := make(map[string]bool, len(keys))
	for _, k := range keys {
		drop[k] = true
	}
	if m.opt.SnapshotPath != "" {
		_, _ = store.UpdateSnapshot(m.opt.SnapshotPath, func(s *store.Snapshot) (bool, error) {
			return s.Dismiss(keys...), nil
		})
	}
	var kept []core.Session
	for _, s := range m.sessions {
		if s.Offline && drop[s.Key] {
			continue
		}
		kept = append(kept, s)
	}
	m.sessions = kept
	m.syncSelection()
}

func (m *Model) launchRestore(s core.Session) (paneID string, fallbackDir bool, err error) {
	if m.opt.Launch == nil {
		return "", false, fmt.Errorf("restoring sessions needs tmux")
	}
	cmd := core.ResumeCommand(s.Kind, s.Info.ID)
	if cmd == "" {
		return "", false, fmt.Errorf("cannot resume %s without a session ID", s.Title())
	}
	dir, fallback := core.ResumeDir(s.Info.Cwd)
	req := LaunchReq{
		Session: tmux.PoolSession,
		Dir:     dir,
		Window:  core.ResumeWindowName(s),
		Command: cmd,
	}
	pane, err := m.opt.Launch(req)
	return pane, fallback, err
}

func delayedRefreshCmd() tea.Cmd {
	return tea.Tick(restoreRefreshDelay, func(time.Time) tea.Msg { return tickMsg{} })
}

// restoreOne resumes a single offline session in the agents pool, optionally
// jumping to it or docking it beside Switchboard.
func (m *Model) restoreOne(s core.Session, how restoreMode) tea.Cmd {
	if how == restoreDock {
		if m.opt.Dock == nil || m.opt.Undock == nil {
			m.setErr(fmt.Errorf("docking an agent needs Switchboard running inside tmux"))
			return nil
		}
		if m.docked != "" && !m.undock() {
			return nil
		}
	}
	pane, fallback, err := m.launchRestore(s)
	if err != nil {
		m.setErr(fmt.Errorf("could not restore %s: %w", s.Title(), err))
		return nil
	}
	m.removeOfflineKeys(s.Key)
	m.wantKey = s.Key

	suffix := ""
	if fallback {
		suffix = " (original folder missing; started in ~)"
	}

	switch how {
	case restoreDock:
		if err := m.opt.Dock(pane); err != nil {
			m.setErr(fmt.Errorf("restored %s in %s, but could not dock: %w", s.Title(), tmux.PoolSession, err))
			return delayedRefreshCmd()
		}
		m.docked, m.dockedTitle = pane, s.Title()
		m.setStatus(fmt.Sprintf("Restored and docked %s%s. V undocks from anywhere.", s.Title(), suffix))
	case restoreJump:
		if m.opt.Show != nil {
			if err := m.opt.Show(pane); err != nil {
				m.setErr(fmt.Errorf("restored %s in %s, but could not jump: %w", s.Title(), tmux.PoolSession, err))
				return delayedRefreshCmd()
			}
			m.setStatus(fmt.Sprintf("Restored %s in %s%s.", s.Title(), tmux.PoolSession, suffix))
		} else {
			m.setStatus(fmt.Sprintf("Restored %s in %s (%s)%s.", s.Title(), tmux.PoolSession, pane, suffix))
		}
	default:
		m.setStatus(fmt.Sprintf("Restored %s in %s%s. Press v to dock it once ready.", s.Title(), tmux.PoolSession, suffix))
	}
	return delayedRefreshCmd()
}

// restoreAction handles R: restores a single focused offline session in the
// background, or all active (non-held) offline sessions in the current scope.
func (m *Model) restoreAction() tea.Cmd {
	if s, ok := m.focusedOffline(); ok {
		return m.restoreOne(s, restoreBackground)
	}
	return m.restoreScope(false)
}

// restoreScope restores offline sessions in the current scope (or focused
// project/thread card). When includeHeld is false, held sessions stay offline.
func (m *Model) restoreScope(includeHeld bool) tea.Cmd {
	if m.opt.Launch == nil {
		m.setErr(fmt.Errorf("restoring sessions needs tmux"))
		return nil
	}
	sc, label := m.restoreTargetScope()
	all := m.offlineIn(sc)
	if len(all) == 0 {
		m.setStatus("No offline sessions to restore here.")
		return nil
	}
	var targets []core.Session
	skippedHeld := 0
	for _, s := range all {
		if s.Held && !includeHeld {
			skippedHeld++
			continue
		}
		targets = append(targets, s)
	}
	if len(targets) == 0 {
		m.setStatus(fmt.Sprintf("All %s here are on hold. Use :restore all to resume them.", plural(skippedHeld, "offline session")))
		return nil
	}
	var restored []string
	var firstErr error
	fallbacks := 0
	for _, s := range targets {
		_, fb, err := m.launchRestore(s)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		restored = append(restored, s.Key)
		if fb {
			fallbacks++
		}
	}
	if len(restored) == 0 {
		m.setErr(fmt.Errorf("could not restore sessions: %w", firstErr))
		return nil
	}
	m.removeOfflineKeys(restored...)
	where := ""
	if label != "" {
		where = " in " + label
	}
	msg := fmt.Sprintf("Restoring %s%s into %s.", plural(len(restored), "session"), where, tmux.PoolSession)
	if skippedHeld > 0 {
		msg += fmt.Sprintf(" Skipped %d on hold (:restore all includes them).", skippedHeld)
	}
	if fallbacks > 0 {
		msg += fmt.Sprintf(" %d started in ~ (missing folder).", fallbacks)
	}
	if firstErr != nil {
		msg += " Some failed: " + firstErr.Error()
	}
	m.setStatus(msg)
	return delayedRefreshCmd()
}

// dismissOffline removes one offline session from recovery.
func (m *Model) dismissOffline(s core.Session) {
	m.removeOfflineKeys(s.Key)
	m.setStatus("Dismissed " + s.Title() + " from recovery.")
}

// dismissScopeOffline removes all offline sessions in the current scope.
func (m *Model) dismissScopeOffline() {
	sc, label := m.restoreTargetScope()
	all := m.offlineIn(sc)
	if len(all) == 0 {
		m.setStatus("No offline sessions to dismiss here.")
		return
	}
	keys := make([]string, 0, len(all))
	for _, s := range all {
		keys = append(keys, s.Key)
	}
	m.removeOfflineKeys(keys...)
	if label != "" {
		m.setStatus(fmt.Sprintf("Dismissed %s in %s.", plural(len(keys), "offline session"), label))
	} else {
		m.setStatus(fmt.Sprintf("Dismissed %s.", plural(len(keys), "offline session")))
	}
}
