package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Keys follow LazyVim defaults and the Snacks explorer: H/L step like
// buffers, ]d/[d step like diagnostics, n/r/d/m act like the explorer, and
// <space> opens a which-key menu.

type action int

const (
	aNone action = iota
	aQuit
	aHelp
	aFocusToggle
	aFocusSide
	aFocusSessions
	aDown
	aUp
	aTop
	aBottom
	aHalfDown
	aHalfUp
	aPrevProject
	aNextProject
	aNextNeeds
	aPrevNeeds
	aEnter
	aMove
	aAccept
	aNewProject
	aNewThread
	aDescribe
	aRename
	aDelete
	aFilter
	aFind
	aEsc
	aRefresh
	aCommand
	aSummarize
	aCheckCloud
)

var normalKeys = map[string]action{
	"q": aQuit, "ctrl+c": aQuit, "?": aHelp,
	"tab": aFocusToggle, "h": aFocusSide, "left": aFocusSide, "ctrl+h": aFocusSide,
	"l": aFocusSessions, "right": aFocusSessions, "ctrl+l": aFocusSessions,
	"j": aDown, "down": aDown, "k": aUp, "up": aUp,
	"home": aTop, "G": aBottom, "end": aBottom,
	"ctrl+d": aHalfDown, "ctrl+u": aHalfUp,
	"H": aPrevProject, "L": aNextProject,
	"enter": aEnter, "m": aMove, "a": aAccept,
	"n": aNewProject, "t": aNewThread, "e": aDescribe, "r": aRename, "d": aDelete,
	"/": aFilter, "esc": aEsc, "u": aRefresh, ":": aCommand,
}

type keyHint struct {
	key, desc string
	act       action
}

// prefixes are multi-key sequences. The hints double as the which-key menu.
var prefixes = map[string][]keyHint{
	"g": {{"g", "top of list", aTop}},
	"]": {{"d", "next session that needs you", aNextNeeds}, {"p", "next project", aNextProject}},
	"[": {{"d", "previous session that needs you", aPrevNeeds}, {"p", "previous project", aPrevProject}},
	" ": {
		{"space", "find a session", aFind},
		{"n", "new project", aNewProject},
		{"t", "new thread", aNewThread},
		{"e", "describe project", aDescribe},
		{"r", "rename", aRename},
		{"d", "delete", aDelete},
		{"m", "move session to a project", aMove},
		{"a", "accept suggestion", aAccept},
		{"s", "rewrite project summary", aSummarize},
		{"c", "check cloud resources with Azure", aCheckCloud},
		{"u", "refresh", aRefresh},
		{"?", "all keys", aHelp},
		{"q", "quit", aQuit},
	},
}

func prefixLabel(p string) string {
	if p == " " {
		return "<leader>"
	}
	return p
}

func hintKey(k string) string {
	if k == " " {
		return "space"
	}
	return k
}

func (m Model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.status = ""
	k := msg.String()
	if m.pending != "" {
		p := m.pending
		m.pending = ""
		if k == "esc" {
			return m, nil
		}
		for _, h := range prefixes[p] {
			if hintKey(k) == h.key {
				return m.do(h.act)
			}
		}
		m.setErr(fmt.Errorf("%s%s is not mapped", prefixLabel(p), k))
		return m, nil
	}
	if _, ok := prefixes[k]; ok {
		m.pending = k
		return m, nil
	}
	return m.do(normalKeys[k])
}

func (m Model) do(a action) (tea.Model, tea.Cmd) {
	switch a {
	case aQuit:
		return m, tea.Quit
	case aHelp:
		m.mode = modeHelp
	case aFocusToggle:
		if m.focus == focusSide {
			m.focus = focusSessions
		} else {
			m.focus = focusSide
		}
	case aFocusSide:
		m.focus = focusSide
	case aFocusSessions:
		m.focus = focusSessions
	case aDown:
		if m.focus == focusSide {
			m.setSide(m.sideIdx + 1)
		} else {
			m.selectSession(m.sessIdx + 1)
		}
	case aUp:
		if m.focus == focusSide {
			m.setSide(m.sideIdx - 1)
		} else {
			m.selectSession(m.sessIdx - 1)
		}
	case aTop:
		if m.focus == focusSide {
			m.setSide(0)
		} else {
			m.selectSession(0)
		}
	case aBottom:
		if m.focus == focusSide {
			m.setSide(len(m.side) - 1)
		} else {
			m.selectSession(len(m.visible()) - 1)
		}
	case aHalfDown:
		m.centerOff += m.height / 2
	case aHalfUp:
		m.centerOff = max(0, m.centerOff-m.height/2)
	case aPrevProject:
		m.stepProject(-1)
	case aNextProject:
		m.stepProject(1)
	case aNextNeeds:
		m.stepNeedsYou(1)
	case aPrevNeeds:
		m.stepNeedsYou(-1)
	case aEnter:
		if m.focus == focusSide {
			if len(m.visible()) > 0 {
				m.focus = focusSessions
			}
			return m, nil
		}
		return m.jump()
	case aMove:
		m.openOrganize()
	case aAccept:
		m.acceptSuggestion()
	case aNewProject:
		m.startInput(inNewProject, "", "", "", "")
	case aNewThread:
		if sc := m.scope(); sc.pid != "" {
			m.startInput(inNewThread, sc.pid, "", "", "")
		} else {
			m.setErr(fmt.Errorf("select a project first"))
		}
	case aDescribe:
		if p := m.st.Project(m.scope().pid); p != nil {
			purpose := p.Purpose
			if sum, ok := m.summaryFor(p.ID); ok && purpose == "" {
				purpose = sum.Purpose
			}
			m.startInput(inPurpose, p.ID, "", "", purpose)
		} else {
			m.setErr(fmt.Errorf("select a project first"))
		}
	case aRename:
		m.startRename()
	case aDelete:
		m.startDelete()
	case aFilter:
		m.startInput(inFilter, "", "", "", m.filter)
	case aFind:
		m.setSide(0)
		m.focus = focusSessions
		m.startInput(inFilter, "", "", "", "")
	case aEsc:
		m.filter = ""
		m.syncSelection()
	case aRefresh:
		if !m.refreshing {
			m.refreshing = true
			return m, m.refresh()
		}
	case aCommand:
		m.startInput(inCommand, "", "", "", "")
	case aSummarize:
		return m, m.forceSummary()
	case aCheckCloud:
		switch {
		case m.opt.CheckLive == nil:
			m.setErr(fmt.Errorf("Azure checks are off (see --live)"))
		case m.liveBusy:
			m.setStatus("Already checking with Azure.")
		default:
			if cmd := m.maybeCheckLive(true); cmd != nil {
				m.setStatus("Checking cloud resources with Azure…")
				return m, cmd
			}
			m.setStatus("No Azure resources to check.")
		}
	}
	return m, nil
}

// runCommand handles the : command line. Only the commands a Vim user would
// reach for by habit are supported.
func (m Model) runCommand(line string) (tea.Model, tea.Cmd) {
	cmd, arg, _ := strings.Cut(strings.TrimSpace(line), " ")
	arg = strings.TrimSpace(arg)
	switch cmd {
	case "":
		return m, nil
	case "q", "q!", "qa", "qa!", "qall", "quit", "wq", "wqa", "x", "xa":
		return m, tea.Quit
	case "h", "help":
		m.mode = modeHelp
	case "noh", "nohlsearch":
		m.filter = ""
		m.syncSelection()
	case "new", "project":
		if arg == "" {
			m.startInput(inNewProject, "", "", "", "")
		} else {
			m.commitNewProject(arg, "")
		}
	default:
		m.setErr(fmt.Errorf("not a command: %s (try :q, :help, :new <name>)", cmd))
	}
	return m, nil
}

// whichKey renders the menu for a pending prefix, laid out in columns.
func (m Model) whichKey(w int) []string {
	hints := prefixes[m.pending]
	const colW = 38
	cols := max(1, (w-2)/colW)
	rowsN := (len(hints) + cols - 1) / cols
	lines := []string{rule(w), " " + sKicker.Render(prefixLabel(m.pending)) + sSubtle.Render("   esc cancels")}
	for r := 0; r < rowsN; r++ {
		var b strings.Builder
		b.WriteString(" ")
		for c := 0; c < cols; c++ {
			i := c*rowsN + r
			if i >= len(hints) {
				break
			}
			cell := sKey.Render(hints[i].key) + sSubtle.Render(" ➜ ") + sText.Render(hints[i].desc)
			b.WriteString(pad(cell, colW))
		}
		lines = append(lines, b.String())
	}
	return lines
}
