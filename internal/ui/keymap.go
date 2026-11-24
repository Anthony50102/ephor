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
	aLineDown
	aLineUp
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
	aLaunch
	aReply
	aToggleSide
	aToggleRight
	aZen
	aCopy
	aConnect
	aDock
	aUndock
	aPark
	aParkAll
	aNavLeft
	aNavRight
	aNavDown
	aNavUp
)

var normalKeys = map[string]action{
	"q": aQuit, "ctrl+c": aQuit, "?": aHelp,
	"tab": aFocusToggle, "h": aFocusSide, "left": aFocusSide, "ctrl+h": aNavLeft,
	"l": aFocusSessions, "right": aFocusSessions, "ctrl+l": aNavRight,
	"ctrl+j": aNavDown, "ctrl+k": aNavUp,
	"j": aDown, "down": aDown, "k": aUp, "up": aUp,
	"home": aTop, "G": aBottom, "end": aBottom,
	"ctrl+d": aHalfDown, "ctrl+u": aHalfUp, "ctrl+e": aLineDown, "ctrl+y": aLineUp,
	"H": aPrevProject, "L": aNextProject,
	"enter": aEnter, "m": aMove, "a": aAccept,
	"n": aNewProject, "t": aNewThread, "e": aDescribe, "r": aRename, "d": aDelete,
	"o": aLaunch, "i": aReply, "y": aCopy, "s": aConnect, "v": aDock, "V": aUndock,
	"p": aPark, "P": aParkAll,
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
		{"o", "start Copilot here", aLaunch},
		{"i", "type to the session", aReply},
		{"v", "dock the session beside this panel (again sends it back)", aDock},
		{"V", "undock the docked session from anywhere", aUndock},
		{"p", "park this session in the background agents session", aPark},
		{"P", "park all agent windows in the background agents session", aParkAll},
		{"n", "new project", aNewProject},
		{"t", "new thread", aNewThread},
		{"e", "toggle projects panel", aToggleSide},
		{"l", "toggle sessions panel", aToggleRight},
		{"z", "zen: hide both panels", aZen},
		{"r", "rename", aRename},
		{"d", "delete", aDelete},
		{"m", "move session to a project", aMove},
		{"a", "accept suggestion", aAccept},
		{"s", "rewrite project summary", aSummarize},
		{"c", "check cloud resources with Azure and kubectl", aCheckCloud},
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

// pushCount accumulates Vim-style numeric motion prefixes (1-9, and 0 once a
// count has started). Returns true if k was consumed as part of the count.
func (m *Model) pushCount(k string) bool {
	if len(k) != 1 || k[0] < '0' || k[0] > '9' {
		return false
	}
	d := int(k[0] - '0')
	if d == 0 && m.count == 0 {
		return false
	}
	const maxCount = 9999
	if m.count*10+d <= maxCount {
		m.count = m.count*10 + d
	} else {
		m.count = maxCount
	}
	return true
}

// takeCount returns the active count (defaulting to 1) and resets it.
func (m *Model) takeCount() int {
	n := m.count
	m.count = 0
	if n < 1 {
		return 1
	}
	return n
}

func (m Model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.status = ""
	k := msg.String()
	if m.pending != "" {
		p := m.pending
		m.pending = ""
		if k == "esc" {
			m.count = 0
			return m, nil
		}
		for _, h := range prefixes[p] {
			if hintKey(k) == h.key {
				return m.do(h.act)
			}
		}
		m.count = 0
		m.setErr(fmt.Errorf("%s%s is not mapped", prefixLabel(p), k))
		return m, nil
	}
	if m.pushCount(k) {
		return m, nil
	}
	if k == "esc" && m.count > 0 {
		m.count = 0
		return m, nil
	}
	if _, ok := prefixes[k]; ok {
		m.pending = k
		return m, nil
	}
	return m.do(normalKeys[k])
}

func (m Model) do(a action) (tea.Model, tea.Cmd) {
	hasCount := m.count > 0
	n := m.takeCount()
	switch a {
	case aQuit:
		return m, tea.Quit
	case aHelp:
		m.mode = modeHelp
		m.helpOff = 0
	case aFocusToggle:
		for range n {
			switch m.focus {
			case focusSide:
				m.focus = focusCenter
				m.ensureCard()
			case focusCenter:
				m.focus = focusSessions
			default:
				m.focus = focusSide
				m.fixFocus()
			}
		}
	case aFocusSide:
		for range n {
			m.focusLeft()
		}
	case aFocusSessions:
		for range n {
			m.focusRight()
		}
	case aNavLeft, aNavRight, aNavDown, aNavUp:
		for range n {
			m.navigate(a)
		}
	case aDown, aUp:
		delta := n
		if a == aUp {
			delta = -n
		}
		switch m.focus {
		case focusSide:
			m.setSide(m.sideIdx + delta)
		case focusCenter:
			m.moveCard(delta)
		default:
			m.selectSession(m.sessIdx + delta)
		}
	case aTop:
		if hasCount {
			m.jumpIndex(n - 1)
			return m, nil
		}
		switch m.focus {
		case focusSide:
			m.setSide(0)
		case focusCenter:
			m.firstCard()
		default:
			m.selectSession(0)
		}
	case aBottom:
		if hasCount {
			m.jumpIndex(n - 1)
			return m, nil
		}
		switch m.focus {
		case focusSide:
			m.setSide(len(m.side) - 1)
		case focusCenter:
			m.lastCard()
		default:
			m.selectSession(len(m.visible()) - 1)
		}
	case aToggleSide:
		m.toggleSide()
	case aToggleRight:
		m.toggleRight()
	case aZen:
		m.toggleZen()
	case aHalfDown:
		m.scrollCenter(n * max(1, m.height/2))
	case aHalfUp:
		m.scrollCenter(-n * max(1, m.height/2))
	case aLineDown:
		m.scrollCenter(n)
	case aLineUp:
		m.scrollCenter(-n)
	case aPrevProject:
		for range n {
			m.stepProject(-1)
		}
	case aNextProject:
		for range n {
			m.stepProject(1)
		}
	case aNextNeeds:
		for range n {
			m.stepNeedsYou(1)
		}
	case aPrevNeeds:
		for range n {
			m.stepNeedsYou(-1)
		}
	case aEnter:
		switch m.focus {
		case focusSide:
			m.focus = focusCenter
			m.firstCard()
			return m, nil
		case focusCenter:
			return m.enterCard()
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
		if m.focus == focusCenter && m.expanded[m.cardID] {
			delete(m.expanded, m.cardID)
			m.followCard()
			return m, nil
		}
		m.filter = ""
		m.syncSelection()
	case aRefresh:
		if !m.refreshing {
			m.refreshing = true
			return m, m.refresh()
		}
	case aCommand:
		m.startInput(inCommand, "", "", "", "")
	case aLaunch:
		m.startLaunch()
	case aReply:
		return m, m.startReply()
	case aSummarize:
		return m, m.forceSummary()
	case aDock:
		m.toggleDock()
	case aUndock:
		m.undockAny()
	case aPark:
		return m, m.parkOne()
	case aParkAll:
		return m, m.parkAll()
	case aCopy:
		m.copyAccess()
	case aConnect:
		m.connect()
	case aCheckCloud:
		kube := m.maybeKube(true)
		switch {
		case m.opt.CheckLive == nil:
			m.setErr(fmt.Errorf("Azure checks are off (see --live)"))
		case m.liveBusy:
			m.setStatus("Already checking with Azure.")
		default:
			if cmd := m.maybeCheckLive(true); cmd != nil {
				m.setStatus("Checking cloud resources with Azure…")
				return m, tea.Batch(cmd, kube)
			}
			m.setStatus("No Azure resources to check.")
		}
		return m, kube
	}
	return m, nil
}

// jumpIndex moves the focused list or card pane directly to 0-based index idx,
// used for Vim-style {count}G and {count}gg jumps.
func (m *Model) jumpIndex(idx int) {
	switch m.focus {
	case focusSide:
		m.setSide(idx)
	case focusCenter:
		m.selectCard(idx)
	default:
		m.selectSession(idx)
	}
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
		m.helpOff = 0
	case "noh", "nohlsearch":
		m.filter = ""
		m.syncSelection()
	case "new", "project":
		if arg == "" {
			m.startInput(inNewProject, "", "", "", "")
		} else {
			m.commitNewProject(arg, "")
		}
	case "undock":
		m.undockAny()
	case "park":
		if arg == "all" {
			return m, m.parkAll()
		}
		return m, m.parkOne()
	case "park-all", "parkall":
		return m, m.parkAll()
	default:
		m.setErr(fmt.Errorf("not a command: %s (try :q, :help, :new <name>, :undock, :park, :park-all)", cmd))
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
