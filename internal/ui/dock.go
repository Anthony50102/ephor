package ui

import "fmt"

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
	m.setStatus("Docked " + s.Title() + ". Your pane keys move between it and here; v sends it back.")
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
