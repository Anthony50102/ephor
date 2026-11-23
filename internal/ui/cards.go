package ui

import (
	"strings"

	"github.com/anthonypoole/switchboard/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The middle pane is a column of cards. With the middle pane focused, j/k
// step between them and enter opens a project or thread card, or expands any
// other card in place.

type cardKind int

const (
	cardExpand  cardKind = iota // enter shows more
	cardSession                 // enter shows more; selecting it selects the session
	cardProject                 // enter opens the project
	cardThread                  // enter opens the thread
)

type cardRef struct {
	id         string
	kind       cardKind
	key        string // session key for cardSession
	pid, tid   string
	start, end int // line range in the page, end exclusive
}

// page is the middle pane before scrolling.
type page struct {
	lines   []string
	cards   []cardRef
	selLine int // line of the selected session when the list is inline, or -1
}

func (p *page) add(ls ...string) { p.lines = append(p.lines, ls...) }

// addCard appends a card's lines and records where they are.
func (p *page) addCard(c cardRef, ls []string) {
	c.start = len(p.lines)
	p.lines = append(p.lines, ls...)
	c.end = len(p.lines)
	p.cards = append(p.cards, c)
}

// addPart appends lines built elsewhere along with the cards inside them,
// whose ranges are relative to those lines.
func (p *page) addPart(ls []string, cs []cardRef) {
	base := len(p.lines)
	for _, c := range cs {
		c.start += base
		c.end += base
		p.cards = append(p.cards, c)
	}
	p.lines = append(p.lines, ls...)
}

// cardSel reports whether the card is the highlighted one.
func (m Model) cardSel(id string) bool {
	return m.focus == focusCenter && m.cardID == id && m.mode == modeNormal
}

// cardBorder is a frame color, turned blue when the card is highlighted.
func (m Model) cardBorder(id string, c lipgloss.Color) lipgloss.Color {
	if m.cardSel(id) {
		return cBlue
	}
	return c
}

// cardBg is a raised card's background, the selection color when highlighted.
func (m Model) cardBg(id string) lipgloss.Color {
	if m.cardSel(id) {
		return cSelection
	}
	return cRaised
}

func (m Model) open(id string) bool { return m.expanded[id] }

func (m Model) cardIndex(cs []cardRef) int {
	for i, c := range cs {
		if c.id == m.cardID {
			return i
		}
	}
	return -1
}

// moveCard highlights the card dir steps away and scrolls it into view.
// With nothing highlighted it starts from the first card on screen.
func (m *Model) moveCard(dir int) {
	l := m.layout()
	pg := m.centerPage(l)
	if len(pg.cards) == 0 {
		m.scrollCenter(dir)
		return
	}
	i := m.cardIndex(pg.cards)
	switch {
	case i >= 0:
		i = max(0, min(len(pg.cards)-1, i+dir))
	case dir == 2: // last
		i = len(pg.cards) - 1
	default:
		off := clampOff(m.centerOff, len(pg.lines), l.bodyH)
		i = len(pg.cards) - 1
		for j, c := range pg.cards {
			if c.start >= off {
				i = j
				break
			}
		}
	}
	m.highlight(pg.cards[i])
}

func (m *Model) firstCard() {
	m.cardID = ""
	m.centerOff = 0
	m.moveCard(0)
}

func (m *Model) lastCard() {
	m.cardID = ""
	m.moveCard(2)
}

// highlight selects a card, selects its session, and scrolls it into view.
func (m *Model) highlight(c cardRef) {
	m.cardID = c.id
	if c.kind == cardSession {
		if _, ok := m.sessionByKey(c.key); ok {
			m.syncSelectionTo(c.key)
		}
	}
	m.followCard()
}

// followCard scrolls so the highlighted card is on screen, keeping its top
// visible when it is taller than the pane.
func (m *Model) followCard() {
	l := m.layout()
	pg := m.centerPage(l)
	i := m.cardIndex(pg.cards)
	if i < 0 {
		return
	}
	c := pg.cards[i]
	off := clampOff(m.centerOff, len(pg.lines), l.bodyH)
	top := max(0, c.start-1)
	if top < off {
		off = top
	} else if c.end+1 > off+l.bodyH {
		off = min(top, c.end+1-l.bodyH)
	}
	m.centerOff = clampOff(off, len(pg.lines), l.bodyH)
}

// enterCard opens a project or thread card, or expands any other card.
func (m Model) enterCard() (tea.Model, tea.Cmd) {
	pg := m.centerPage(m.layout())
	i := m.cardIndex(pg.cards)
	if i < 0 {
		m.moveCard(0)
		return m, nil
	}
	c := pg.cards[i]
	switch c.kind {
	case cardProject:
		m.selectSide(sideItem{kind: sideProject, pid: c.pid})
		m.focus = focusCenter
	case cardThread:
		m.selectSide(sideItem{kind: sideThread, pid: c.pid, tid: c.tid})
		m.focus = focusCenter
	default:
		if m.expanded[c.id] {
			delete(m.expanded, c.id)
		} else {
			m.expanded[c.id] = true
		}
		m.followCard()
		if m.expanded[c.id] && strings.HasPrefix(c.id, "res:") {
			return m, m.maybeKube(false)
		}
	}
	return m, nil
}

// ---- panels ----

func (m *Model) toggleSide() {
	m.hideSide = !m.hideSide
	m.fixFocus()
}

func (m *Model) toggleRight() {
	m.hideRight = !m.hideRight
	m.fixFocus()
}

// toggleZen hides both side panels, or shows both when they are hidden.
func (m *Model) toggleZen() {
	hide := !(m.hideSide && m.hideRight)
	m.hideSide, m.hideRight = hide, hide
	m.fixFocus()
}

func (m *Model) fixFocus() {
	if m.focus == focusSide && m.hideSide {
		m.focus = focusCenter
	}
	m.followCard()
}

// focusLeft and focusRight move between panes like ctrl-w h and ctrl-w l.
// With the right panel hidden, the session list sits inside the middle pane
// and still takes focus.
func (m *Model) focusLeft() {
	switch m.focus {
	case focusSessions:
		m.focus = focusCenter
	case focusCenter:
		if !m.hideSide {
			m.focus = focusSide
		}
	}
}

func (m *Model) focusRight() {
	switch m.focus {
	case focusSide:
		m.focus = focusCenter
	case focusCenter:
		m.focus = focusSessions
	}
}

// expandedSession is the extra detail an expanded session card shows.
func (m Model) expandedSession(s core.Session, w int) []string {
	lines := []string{"", sLine.Render(strings.Repeat("─", w))}
	return append(lines, m.detailFor(s, w, true)...)
}
