package ui

import (
	"fmt"
	"time"

	"github.com/Anthony50102/ephor/internal/tmux"
	tea "github.com/charmbracelet/bubbletea"
)

// dockWidth is the docked agent's share of the window.
const dockWidth = "55%"

// dockGrace keeps a freshly docked pane tracked while zsh starts copilot/claude
// when PaneExists is unavailable (such as in tests).
const dockGrace = 10 * time.Second

// Docking moves an agent's real tmux pane beside Ephor, so you work
// in the agent itself, then sends it back to its own window.

func (m *Model) recoverDocked() {
	if m.docked != "" || m.opt.DockedPane == nil {
		return
	}
	if id := m.opt.DockedPane(); id != "" {
		m.docked = id
		if m.dockedTitle == "" {
			m.dockedTitle = "docked pane"
		}
		for _, s := range m.sessions {
			if s.Pane.PaneID == id {
				m.dockedTitle = s.Title()
				break
			}
		}
	}
}

func (m *Model) toggleDock() tea.Cmd {
	if m.opt.Dock == nil || m.opt.Undock == nil {
		m.setErr(fmt.Errorf("docking an agent needs Ephor running inside tmux"))
		return nil
	}
	m.recoverDocked()
	s, ok := m.selected()
	if ok && s.Offline {
		return m.restoreOne(s, restoreDock)
	}
	if m.docked != "" {
		was := m.docked
		if !m.undock() || !ok || s.Pane.PaneID == was {
			return nil
		}
	}
	if !ok || s.Pane.PaneID == "" {
		m.setErr(fmt.Errorf("select a session to dock"))
		return nil
	}
	if err := m.opt.Dock(s.Pane.PaneID); err != nil {
		m.setErr(err)
		return nil
	}
	m.docked, m.dockedTitle, m.dockedAt = s.Pane.PaneID, s.Title(), time.Time{}
	if m.overlayMode() && m.focus == focusSessions {
		m.focus = focusCenter
		m.ensureCard()
	}
	m.setStatus("Docked " + s.Title() + ". Pane keys move between it and here; V undocks from anywhere.")
	return nil
}

// undockAny sends the currently docked agent back to its window from anywhere,
// regardless of which project, panel, or session is selected.
func (m *Model) undockAny() {
	m.recoverDocked()
	if m.docked == "" {
		m.setStatus("Nothing is docked.")
		return
	}
	if m.opt.Undock == nil {
		m.setErr(fmt.Errorf("undocking an agent needs Ephor running inside tmux"))
		return
	}
	m.undock()
}

// undock sends the docked agent home, reporting whether it went.
func (m *Model) undock() bool {
	if err := m.opt.Undock(m.docked); err != nil {
		m.setErr(fmt.Errorf("sending %s back: %w", m.dockedTitle, err))
		return false
	}
	m.setStatus("Sent " + m.dockedTitle + " back to its window.")
	m.docked, m.dockedTitle, m.dockedAt = "", "", time.Time{}
	return true
}

// dockGone forgets a docked agent only when its tmux pane has actually closed,
// not merely while a restored pane is still booting zsh -> copilot or sitting
// at a shell prompt.
func (m *Model) dockGone() {
	if m.docked == "" || !m.loaded {
		return
	}
	if m.opt.PaneExists != nil {
		if m.opt.PaneExists(m.docked) {
			return
		}
		m.docked, m.dockedTitle, m.dockedAt = "", "", time.Time{}
		return
	}
	if !m.dockedAt.IsZero() && m.opt.Now().Sub(m.dockedAt) < dockGrace {
		return
	}
	for _, s := range m.sessions {
		if s.Pane.PaneID == m.docked {
			return
		}
	}
	m.docked, m.dockedTitle, m.dockedAt = "", "", time.Time{}
}

// Docked is the pane still docked when the program ends, for Run to send back.
func (m Model) Docked() string { return m.docked }

// parkOne moves the selected session's window into the background agents pool.
func (m *Model) parkOne() tea.Cmd {
	if m.opt.Park == nil {
		m.setErr(fmt.Errorf("parking sessions needs tmux"))
		return nil
	}
	s, ok := m.selected()
	if !ok || s.Pane.PaneID == "" {
		m.setErr(fmt.Errorf("select a session to park"))
		return nil
	}
	if s.Pane.PaneID == m.docked {
		if m.opt.Rehome != nil {
			if err := m.opt.Rehome(m.docked); err != nil {
				m.setErr(fmt.Errorf("parking %s: %w", s.Title(), err))
				return nil
			}
		}
		if !m.undock() {
			return nil
		}
		m.setStatus(fmt.Sprintf("Parked %s in %s.", s.Title(), tmux.PoolSession))
		return m.refreshNow()
	}
	moved, err := m.opt.Park(s.Pane.PaneID)
	if err != nil {
		m.setErr(fmt.Errorf("parking %s: %w", s.Title(), err))
		return nil
	}
	if !moved {
		m.setStatus(fmt.Sprintf("%s is already parked in %s.", s.Title(), tmux.PoolSession))
		return nil
	}
	m.setStatus(fmt.Sprintf("Parked %s in %s.", s.Title(), tmux.PoolSession))
	return m.refreshNow()
}

// parkAll moves every live agent window outside the agents pool into it,
// leaving non-agent shells untouched and keeping any docked agent open beside
// Ephor (re-homed so it returns to agents when undocked).
func (m *Model) parkAll() tea.Cmd {
	if m.opt.Park == nil {
		m.setErr(fmt.Errorf("parking sessions needs tmux"))
		return nil
	}
	if len(m.sessions) == 0 {
		m.setStatus("No live sessions to park.")
		return nil
	}
	moved := 0
	rehomed := false
	var firstErr error
	for _, s := range m.sessions {
		if s.Pane.PaneID == "" {
			continue
		}
		if s.Pane.PaneID == m.docked {
			if m.opt.Rehome != nil {
				if err := m.opt.Rehome(m.docked); err != nil {
					if firstErr == nil {
						firstErr = err
					}
				} else {
					rehomed = true
				}
			}
			continue
		}
		if s.Pane.SessionName == tmux.PoolSession {
			continue
		}
		ok, err := m.opt.Park(s.Pane.PaneID)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			moved++
		}
	}
	if firstErr != nil && moved == 0 && !rehomed {
		m.setErr(fmt.Errorf("parking sessions: %w", firstErr))
		return nil
	}
	switch {
	case moved > 0 && rehomed:
		m.setStatus(fmt.Sprintf("Parked %s in %s (docked agent will return there too).", plural(moved, "agent"), tmux.PoolSession))
	case moved > 0:
		m.setStatus(fmt.Sprintf("Parked %s in %s.", plural(moved, "agent"), tmux.PoolSession))
	case rehomed:
		m.setStatus(fmt.Sprintf("Docked agent will return to %s when sent back.", tmux.PoolSession))
	default:
		m.setStatus(fmt.Sprintf("All agents are already parked in %s.", tmux.PoolSession))
	}
	if moved > 0 {
		return m.refreshNow()
	}
	return nil
}

func (m *Model) refreshNow() tea.Cmd {
	if m.refreshing || m.opt.Discover == nil {
		return nil
	}
	m.refreshing = true
	return m.refresh()
}
