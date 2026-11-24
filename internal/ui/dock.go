package ui

import (
	"fmt"

	"github.com/anthonypoole/switchboard/internal/tmux"
	tea "github.com/charmbracelet/bubbletea"
)

// dockWidth is the docked agent's share of the window.
const dockWidth = "55%"

// Docking moves an agent's real tmux pane beside Switchboard, so you work
// in the agent itself, then sends it back to its own window.

func (m *Model) toggleDock() {
	if m.opt.Dock == nil || m.opt.Undock == nil {
		m.setErr(fmt.Errorf("docking an agent needs Switchboard running inside tmux"))
		return
	}
	s, ok := m.selected()
	if m.docked != "" {
		was := m.docked
		if !m.undock() || !ok || s.Pane.PaneID == was {
			return
		}
	}
	if !ok || s.Pane.PaneID == "" {
		m.setErr(fmt.Errorf("select a session to dock"))
		return
	}
	if err := m.opt.Dock(s.Pane.PaneID); err != nil {
		m.setErr(err)
		return
	}
	m.docked, m.dockedTitle = s.Pane.PaneID, s.Title()
	m.setStatus("Docked " + s.Title() + ". Pane keys move between it and here; V undocks from anywhere.")
}

// undockAny sends the currently docked agent back to its window from anywhere,
// regardless of which project, panel, or session is selected.
func (m *Model) undockAny() {
	if m.docked == "" {
		m.setStatus("Nothing is docked.")
		return
	}
	if m.opt.Undock == nil {
		m.setErr(fmt.Errorf("undocking an agent needs Switchboard running inside tmux"))
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
	m.docked, m.dockedTitle = "", ""
	return true
}

// dockGone forgets a docked agent whose pane has closed.
func (m *Model) dockGone() {
	if m.docked == "" || !m.loaded {
		return
	}
	for _, s := range m.sessions {
		if s.Pane.PaneID == m.docked {
			return
		}
	}
	m.docked, m.dockedTitle = "", ""
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
// Switchboard (re-homed so it returns to agents when undocked).
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
