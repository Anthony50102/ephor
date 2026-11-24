package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/charmbracelet/lipgloss"
)

const (
	sideWide   = 30
	sideNarrow = 22
	rightW     = 44
	wideEnough = 124
)

type layout struct {
	bodyH, sideW, centerW int
	wide                  bool
}

func (m Model) layout() layout {
	l := layout{bodyH: m.height - 2, wide: m.width >= wideEnough && !m.hideRight, sideW: sideWide}
	if m.width < 100 {
		l.sideW = sideNarrow
	}
	l.centerW = m.width - l.sideW - 1
	if m.hideSide {
		l.sideW, l.centerW = 0, m.width
	}
	if l.wide {
		l.centerW -= rightW + 1
	}
	return l
}

// centerPage is the whole middle pane before scrolling.
func (m Model) centerPage(l layout) page {
	switch m.mode {
	case modeOrganize:
		return page{lines: m.viewOrganize(l.centerW - 4), selLine: -1}
	case modeHelp:
		return page{lines: m.viewHelp(l.centerW - 4), selLine: -1}
	}
	if m.replying() {
		return page{lines: m.viewScreen(l.centerW-4, l.bodyH), selLine: -1}
	}
	return m.viewCenter(l.centerW-4, !l.wide)
}

// clampOff keeps a scroll offset between the top and the point where the
// last line is just on screen.
func clampOff(off, total, h int) int {
	return max(0, min(off, total-h+1))
}

// scrollCenter moves the middle pane by n lines, stopping at either end.
func (m *Model) scrollCenter(n int) {
	l := m.layout()
	lines := m.centerPage(l).lines
	m.centerOff = clampOff(clampOff(m.centerOff, len(lines), l.bodyH)+n, len(lines), l.bodyH)
}

// View renders the whole screen.
func (m Model) View() string {
	w, h := m.width, m.height
	if w < 60 || h < 12 {
		return "Switchboard needs at least 60×12. Make the window bigger."
	}
	lay := m.layout()
	bodyH, wide, sideW, centerW := lay.bodyH, lay.wide, lay.sideW, lay.centerW

	pg := m.centerPage(lay)
	center, selLine := pg.lines, pg.selLine
	// A bar in the left margin marks the highlighted card.
	margin := make([]string, len(center))
	for i := range margin {
		margin[i] = "  "
	}
	if i := m.cardIndex(pg.cards); i >= 0 && m.cardSel(m.cardID) {
		// Raised cards already render their own thickened ▌ bar when selected.
		switch m.cardID {
		case "happening", "ov:moving", "ov:ready":
		default:
			for j := pg.cards[i].start; j < pg.cards[i].end; j++ {
				margin[j] = sBlue.Render("▌") + " "
			}
		}
	}
	total := len(center)
	off := 0
	scrollable := m.mode == modeNormal || m.mode == modeInput || m.mode == modeConfirm || m.mode == modeHelp
	if scrollable && !m.replying() {
		if m.mode == modeHelp {
			off = clampOff(m.helpOff, total, bodyH)
		} else {
			off = m.centerOff
			// In the narrow layout the session list is inside the brief, so
			// follow the selection when the list has focus.
			if selLine >= 0 && m.focus == focusSessions && (selLine < off || selLine >= off+bodyH-2) {
				off = selLine - bodyH/3
			}
			off = clampOff(off, total, bodyH)
		}
		var vis []string
		var visM []string
		// Reserve dedicated top/bottom rows for scroll banners so content is
		// never overwritten mid-card.
		avail := bodyH
		if off > 0 {
			vis = append(vis, spread("", sSubtle.Render(fmt.Sprintf("↑ %d more · ctrl-u", off)), centerW-4))
			visM = append(visM, "  ")
			avail--
		}
		hasBelow := off+avail < total
		contentRows := avail
		if hasBelow {
			contentRows--
		}
		end := min(total, off+max(0, contentRows))
		for i := off; i < end; i++ {
			vis = append(vis, center[i])
			visM = append(visM, margin[i])
		}
		if hasBelow {
			below := total - end
			vis = append(vis, spread("", sBlue.Render(fmt.Sprintf("↓ %d more · ctrl-d or wheel", below)), centerW-4))
			visM = append(visM, "  ")
		}
		center, margin = vis, visM
	}
	for i := range center {
		center[i] = margin[i] + center[i]
	}
	if scrollable && !m.replying() && total > bodyH {
		center = box(center, centerW-1, bodyH)
		thumbH := max(1, bodyH*bodyH/total)
		maxOff := max(1, total-bodyH+1)
		thumbTop := min(bodyH-thumbH, (off*(bodyH-thumbH))/maxOff)
		for i := range center {
			if i >= thumbTop && i < thumbTop+thumbH {
				center[i] += sSubtle.Render("┃")
			} else {
				center[i] += " "
			}
		}
	} else {
		center = box(center, centerW, bodyH)
	}
	sep := paint(sLine.Render("│"), cPanel)
	var side []string
	if sideW > 0 {
		side = box(m.viewSide(sideW, bodyH), sideW, bodyH)
	}

	rows := make([]string, bodyH)
	var right []string
	if wide {
		right = box(indent(m.viewSessionsPane(rightW-2, bodyH), 1), rightW, bodyH)
	}
	for i := 0; i < bodyH; i++ {
		r := paint(center[i], cBg)
		if sideW > 0 {
			r = paint(side[i], cPanel) + sep + r
		}
		if wide {
			r += sep + paint(right[i], cPanel)
		}
		rows[i] = r
	}
	if m.pending != "" && m.mode == modeNormal {
		wk := m.whichKey(w)
		if len(wk) < bodyH {
			for i, l := range padAll(wk, w) {
				rows[bodyH-len(wk)+i] = paint(l, cRaised)
			}
		}
	}
	return paint(m.viewHeader(w), cPanel) + "\n" + strings.Join(rows, "\n") + "\n" + paint(m.viewFooter(w), cPanel)
}

// cardAt returns the card rendered at body row ry in the middle pane.
func (m Model) cardAt(l layout, ry int) (cardRef, bool) {
	pg := m.centerPage(l)
	total := len(pg.lines)
	off := clampOff(m.centerOff, total, l.bodyH)
	contentStart := 0
	if off > 0 {
		if ry == 0 {
			return cardRef{}, false
		}
		contentStart = 1
	}
	pageLine := off + (ry - contentStart)
	for _, c := range pg.cards {
		if pageLine >= c.start && pageLine < c.end {
			return c, true
		}
	}
	return cardRef{}, false
}

func padAll(lines []string, w int) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = pad(l, w)
	}
	return out
}

func indent(lines []string, n int) []string {
	p := strings.Repeat(" ", n)
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = p + l
	}
	return out
}

// ---- header and footer ----

func (m Model) viewHeader(w int) string {
	now := m.opt.Now()
	r := core.Roll(m.sessions, now)
	inbox := len(m.sessionsIn(sideItem{kind: sideInbox}))
	left := sBrand.Render(" s_ ") + " " + sBold.Render("switchboard")
	if crumb := m.crumb(); crumb != "" {
		left += sSubtle.Render(" / ") + sBlue.Render(crumb)
	}
	if m.docked != "" {
		left += "  " + sTeal.Background(cRaised).Render(" ⇄ "+trunc(m.dockedTitle, 18)+" ")
	}
	if !m.loaded {
		return spread(" "+left, sSubtle.Render("looking for agents…")+" ", w)
	}
	var parts []string
	if r.NeedsYou > 0 {
		parts = append(parts, sBadge.Render(fmt.Sprintf(" %d needs you ", r.NeedsYou)))
	}
	if r.Working > 0 {
		parts = append(parts, sGreen.Render("●")+sMuted.Render(fmt.Sprintf(" %d working", r.Working)))
	} else {
		parts = append(parts, sSubtle.Render("○ 0 working"))
	}
	if r.Ready > 0 {
		parts = append(parts, sCyan.Render("✓")+sMuted.Render(fmt.Sprintf(" %d ready", r.Ready)))
	} else {
		parts = append(parts, sSubtle.Render("✓ 0 ready"))
	}
	if inbox > 0 {
		parts = append(parts, sMagenta.Render("◇")+sMuted.Render(fmt.Sprintf(" %d unassigned", inbox)))
	}
	help := sSubtle.Render("? help")
	// Drop least urgent pills before squeezing the breadcrumb on narrow widths.
	leftW := lipgloss.Width(left) + 3
	for len(parts) > 1 && leftW+lipgloss.Width(strings.Join(parts, "  ")+"  "+help)+1 > w {
		parts = parts[:len(parts)-1]
	}
	right := strings.Join(parts, "  ") + "  " + help
	return spread(" "+left, right+" ", w)
}

func (m Model) viewFooter(w int) string {
	switch m.mode {
	case modeInput:
		if m.inputKind == inCommand {
			return pad(sBlue.Render(":")+m.input.View(), w)
		}
		return pad(" "+sBlue.Render(m.inputPrompt()+": ")+m.input.View(), w)
	case modeConfirm:
		return pad(" "+sAmber.Render(m.confirmText)+"  "+sKey.Render("y")+sMuted.Render(" yes  ")+sKey.Render("any key")+sMuted.Render(" no"), w)
	case modeHelp:
		return pad(" "+sKey.Render("j/k")+sMuted.Render(" scroll")+sSubtle.Render("  ·  ")+sKey.Render("ctrl-d/u")+sMuted.Render(" page")+sSubtle.Render("  ·  ")+sKey.Render("esc")+sMuted.Render(" close"), w)
	}
	if m.loadErr != nil {
		return pad(" "+sRed.Render("Could not read tmux: "+m.loadErr.Error()), w)
	}
	if m.status != "" {
		st := sMuted
		if m.statusErr {
			st = sRed
		}
		return pad(" "+st.Render(m.status), w)
	}
	var keys [][2]string
	switch {
	case m.mode == modeOrganize:
		keys = [][2]string{{"j/k", "move"}, {"enter", "place here"}, {"t", "new thread"}, {"n", "new project"}, {"esc", "cancel"}}
	case m.focus == focusSide:
		keys = [][2]string{{"j/k", "browse"}, {"enter/l", "brief"}, {"H/L", "projects"}, {"]d", "needs you"}, {"n", "new project"}, {"e", "describe"}}
	case m.focus == focusCenter:
		keys = m.centerFooterKeys()
	default:
		keys = [][2]string{{"j/k", "select"}, {"enter", "jump"}, {"v", "dock"}, {"i", "answer"}, {"m", "move"}, {"a", "accept"}, {"/", "filter"}, {"h", "brief"}}
	}
	if m.docked != "" && m.mode != modeOrganize {
		label := "send back " + m.dockedTitle
		if sel, ok := m.selected(); ok && sel.Pane.PaneID != m.docked {
			label = "dock this instead"
		}
		keys = append([][2]string{{"v", label}}, keys...)
		for i, k := range keys[1:] {
			if k[0] == "v" {
				keys = append(keys[:i+1], keys[i+2:]...)
				break
			}
		}
	}
	right := ""
	if m.filter != "" {
		right = sSubtle.Render("filter /") + sMagenta.Render(m.filter) + sSubtle.Render(" · esc  ")
	}
	if m.mode != modeOrganize {
		right += sKey.Render("space") + " " + sMuted.Render("menu")
	}
	avail := w - 2 - lipgloss.Width(right)
	if right != "" {
		avail -= 2
	}
	sep := sSubtle.Render("  ·  ")
	sepW := lipgloss.Width(sep)
	var b []string
	used := 0
	for _, k := range keys {
		piece := sKey.Render(k[0]) + " " + sMuted.Render(k[1])
		pw := lipgloss.Width(piece)
		need := pw
		if len(b) > 0 {
			need += sepW
		}
		if len(b) > 0 && used+need > avail {
			break
		}
		b = append(b, piece)
		used += need
	}
	left := " " + strings.Join(b, sep)
	if right == "" {
		return pad(left, w)
	}
	return spread(left, right+" ", w)
}

// centerFooterKeys picks hints relevant to the currently highlighted card.
func (m Model) centerFooterKeys() [][2]string {
	var keys [][2]string
	keys = append(keys, [2]string{"j/k", "cards"})
	switch {
	case strings.HasPrefix(m.cardID, "proj:"), strings.HasPrefix(m.cardID, "thread:"):
		keys = append(keys, [2]string{"enter", "open"})
	case strings.HasPrefix(m.cardID, "res:"):
		if m.open(m.cardID) {
			keys = append(keys, [2]string{"esc", "collapse"})
		} else {
			keys = append(keys, [2]string{"enter", "expand"})
		}
		keys = append(keys, [2]string{"y", "copy cmd"}, [2]string{"s", "shell"})
	case strings.HasPrefix(m.cardID, "needs:"), strings.HasPrefix(m.cardID, "sess:"):
		if m.open(m.cardID) {
			keys = append(keys, [2]string{"esc", "collapse"})
		} else {
			keys = append(keys, [2]string{"enter", "expand"})
		}
		keys = append(keys, [2]string{"i", "answer"}, [2]string{"v", "dock"})
	case m.cardID == "happening":
		if m.open(m.cardID) {
			keys = append(keys, [2]string{"esc", "collapse"})
		} else {
			keys = append(keys, [2]string{"enter", "more"})
		}
		keys = append(keys, [2]string{"space s", "rewrite"})
	default:
		if m.open(m.cardID) {
			keys = append(keys, [2]string{"esc", "collapse"})
		} else {
			keys = append(keys, [2]string{"enter", "expand"})
		}
	}
	keys = append(keys, [2]string{"h/l", "panels"}, [2]string{"space z", "zen"})
	return keys
}

// ---- sidebar ----

func (m Model) buildSide(w, h int) ([]string, []int) {
	now := m.opt.Now()
	var lines []string
	var itemAt []int // sideIdx for each rendered line, or -1
	push := func(line string, idx int) {
		lines = append(lines, line)
		itemAt = append(itemAt, idx)
	}
	kicker := sSubtle.Bold(true)
	if m.focus == focusSide {
		kicker = sBlue.Bold(true)
	}
	sel := 0
	add := func(i int, glyph, label, right string) {
		active := i == m.sideIdx
		inner := w - 2
		gW := lipgloss.Width(glyph)
		rW := lipgloss.Width(right)
		maxLabel := max(4, inner-gW-1-rW)
		if rW > 0 {
			maxLabel = max(4, maxLabel-1)
		}
		label = trunc(label, maxLabel)
		row := spread(glyph+" "+label, right, inner)
		if active {
			sel = len(lines)
			bg := cRaised
			bar := sLine.Render("│")
			if m.focus == focusSide {
				bg = cSelection
				bar = sBlue.Render("▌")
			}
			push(paint(bar+pad(row, inner)+" ", bg), i)
			return
		}
		push(" "+row, i)
	}
	push("", -1)
	push(" "+kicker.Render("WORKSPACE"), -1)
	for i, it := range m.side {
		switch it.kind {
		case sideOverview:
			r := core.Roll(m.sessions, now)
			right := ""
			if r.NeedsYou > 0 {
				if w < 26 {
					right = sBadge.Render(fmt.Sprintf(" %d ", r.NeedsYou))
				} else {
					right = sBadge.Render(fmt.Sprintf(" %d needs you ", r.NeedsYou))
				}
			}
			add(i, sBlue.Render("≡"), sText.Render("Overview"), right)
		case sideInbox:
			ss := m.sessionsIn(it)
			r := core.Roll(ss, now)
			right := sSubtle.Render(fmt.Sprint(len(ss)))
			if r.NeedsYou > 0 {
				right = sBadge.Render(fmt.Sprintf(" %d ", len(ss)))
			} else if len(ss) > 0 {
				right = sPill.Render(fmt.Sprintf(" %d ", len(ss)))
			}
			add(i, sMagenta.Render("◇"), sText.Render("Unassigned"), right)
			push("", -1)
			push(" "+kicker.Render("PROJECTS"), -1)
			if len(m.st.Projects) == 0 {
				if w < 25 {
					push(" "+sSubtle.Render("none yet · n new"), -1)
				} else {
					push(" "+sSubtle.Render("none yet · n to create"), -1)
				}
			}
		case sideProject:
			p := m.st.Project(it.pid)
			r := core.Roll(m.sessionsIn(it), now)
			right := ""
			if r.NeedsYou > 0 {
				right = sBadge.Render(fmt.Sprintf(" %d ", r.NeedsYou))
			} else if n := m.newCount(*p); n > 0 {
				right = sMagenta.Render(fmt.Sprintf("%d new", n))
			} else if r.Total > 0 {
				right = sSubtle.Render(fmt.Sprint(r.Total))
			}
			if i > 0 && m.side[i-1].kind == sideThread {
				push("", -1) // close the previous project's thread group
			}
			add(i, dot(r.State), sBold.Render(p.Name), right)
		case sideThread:
			p := m.st.Project(it.pid)
			t := p.Thread(it.tid)
			r := core.Roll(m.sessionsIn(it), now)
			branch := "├─"
			if i == len(m.side)-1 || m.side[i+1].kind != sideThread || m.side[i+1].pid != it.pid {
				branch = "└─"
			}
			right := ""
			if r.NeedsYou > 0 {
				right = sBadge.Render(fmt.Sprintf(" %d ", r.NeedsYou))
			} else if r.Total > 0 {
				right = sSubtle.Render(fmt.Sprint(r.Total))
			}
			add(i, sLine.Render(branch)+" "+dot(r.State), sMuted.Render(t.Name), right)
		}
	}
	push("", -1)
	if w < 26 {
		push(" "+sAmber.Render("●")+sSubtle.Render(" wait   ")+sGreen.Render("●")+sSubtle.Render(" work"), -1)
		push(" "+sCyan.Render("✓")+sSubtle.Render(" ready  ○ quiet"), -1)
	} else {
		push(" "+sAmber.Render("●")+sSubtle.Render(" needs you  ")+sGreen.Render("●")+sSubtle.Render(" working"), -1)
		push(" "+sCyan.Render("✓")+sSubtle.Render(" ready      ○ quiet"), -1)
	}
	if len(lines) > h {
		// Keep the selected item visible on short terminals.
		start := sel - h/2
		if start < 0 {
			start = 0
		}
		if start > len(lines)-h {
			start = len(lines) - h
		}
		lines = lines[start : start+h]
		itemAt = itemAt[start : start+h]
	}
	return lines, itemAt
}

func (m Model) viewSide(w, h int) []string {
	lines, _ := m.buildSide(w, h)
	return lines
}

func (m Model) sideItemAt(w, h, ry int) (int, bool) {
	_, itemAt := m.buildSide(w, h)
	if ry < 0 || ry >= len(itemAt) || itemAt[ry] < 0 {
		return 0, false
	}
	return itemAt[ry], true
}

// ---- center ----

func (m Model) viewCenter(w int, narrow bool) page {
	if !m.loaded {
		return page{lines: []string{"", sMuted.Render("Looking for Copilot and Claude Code sessions in tmux…")}, selLine: -1}
	}
	var pg page
	sc := m.scope()
	switch sc.kind {
	case sideOverview:
		pg = m.viewOverview(w)
	case sideInbox:
		pg = m.viewInbox(w)
	default:
		pg = m.viewProject(w, sc)
	}
	pg.selLine = -1
	if narrow {
		pg.add("", sKicker.Render("SESSIONS"))
		rows, sel := m.sessionRows(w, 0)
		pg.selLine = len(pg.lines) + sel
		pg.add(rows...)
		pg.add("")
		pg.add(m.viewDetail(w)...)
	}
	return pg
}

func section(title string, extra string) string {
	s := sBlue.Bold(true).Render(title)
	if extra != "" {
		s += "  " + sSubtle.Render(extra)
	}
	return s
}

func (m Model) viewOverview(w int) page {
	now := m.opt.Now()
	r := core.Roll(m.sessions, now)
	var pg page
	pg.add("", sSubtle.Render("Overview"), sBold.Render("What needs you, what is moving"), m.triage(r), rule(w))
	if r.Total == 0 {
		pg.add("", sMuted.Render("No Copilot or Claude Code sessions found in tmux."))
		return pg
	}

	sorted := append([]core.Session{}, m.sessions...)
	core.SortSessions(sorted, now)
	if block, cs := m.needsYouBlock(w, sorted, true); block != nil {
		pg.add("")
		pg.addPart(block, cs)
	}
	in := w - 4
	keep := 1
	movingW := in - 2
	if m.open("ov:moving") {
		keep = 6
		movingW = proseWidth(in - 2)
	}
	keepReady := 1
	readyW := in - 2
	if m.open("ov:ready") {
		keepReady = 8
		readyW = proseWidth(in - 2)
	}
	var moving, ready []string
	for _, s := range sorted {
		name := m.where(s) + sBold.Render(s.Title())
		age := sSubtle.Render(core.Ago(s.Info.UpdatedAt, now))
		switch s.State(now) {
		case agents.Working:
			moving = append(moving, spread(name, age, in))
			if d := cleanProse(m.doing(s)); d != "" {
				moving = append(moving, clamp(indent(wrap(sMuted.Render(d), movingW), 2), keep, in)...)
			}
		case agents.Ready:
			ready = append(ready, spread(name, age, in))
			if len(s.Info.Results) > 0 {
				text := cleanProse(agents.FirstSentence(s.Info.Results[0].Text, 240))
				if keepReady > 1 {
					text = cleanProse(s.Info.Results[0].Text)
				}
				ready = append(ready, clamp(indent(wrap(sMuted.Render(text), readyW), 2), keepReady, in)...)
			}
		}
	}
	if moving != nil {
		cue := ""
		if m.open("ov:moving") {
			cue = sSubtle.Render("▾ esc collapses")
		} else if m.cardSel("ov:moving") {
			cue = sSubtle.Render("▸ enter expands")
		}
		pg.add("")
		pg.addCard(cardRef{id: "ov:moving"}, cardOn(cGreen, m.cardBg("ov:moving"), m.cardSel("ov:moving"), w, append([]string{spread(sGreen.Bold(true).Render("HAPPENING NOW"), cue, in)}, moving...)))
	}
	if ready != nil {
		right := sSubtle.Render("finished in the last day")
		if m.open("ov:ready") {
			right = sSubtle.Render("▾ esc collapses")
		} else if m.cardSel("ov:ready") {
			right = sSubtle.Render("▸ enter expands")
		}
		pg.add("")
		pg.addCard(cardRef{id: "ov:ready"}, cardOn(cCyan, m.cardBg("ov:ready"), m.cardSel("ov:ready"), w, append([]string{spread(sCyan.Bold(true).Render("READY FOR YOU"), right, in)}, ready...)))
	}

	type card struct {
		p store.Project
		r core.Rollup
		s []core.Session
	}
	var cards []card
	for _, p := range m.st.Projects {
		ss := m.sessionsIn(sideItem{kind: sideProject, pid: p.ID})
		cards = append(cards, card{p, core.Roll(ss, now), ss})
	}
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].r.State != cards[j].r.State {
			return cards[i].r.State > cards[j].r.State
		}
		return cards[i].r.UpdatedAt.After(cards[j].r.UpdatedAt)
	})
	pg.add("", section("PROJECTS", "most urgent first · enter opens"))
	if len(cards) == 0 {
		pg.add("", sMuted.Render("No projects yet. Press ")+sKey.Render("n")+sMuted.Render(" to create one, then move sessions into it with ")+sKey.Render("m")+sMuted.Render("."))
	}
	for _, c := range cards {
		head := dot(c.r.State) + " " + sBold.Render(c.p.Name)
		meta := stateWord(c.r.State)
		if c.r.Total > 0 {
			meta += sSubtle.Render(" · " + plural(c.r.Total, "session") + " · " + core.Ago(c.r.UpdatedAt, now))
		} else {
			meta = sSubtle.Render("no live sessions")
		}
		body := []string{spread(head, meta, in)}
		sum, hasSum := m.summaryFor(c.p.ID)
		hasPurpose := false
		switch {
		case c.p.Purpose != "":
			body = append(body, clamp(wrap(sMuted.Render(c.p.Purpose), in), 1, in)...)
			hasPurpose = true
		case hasSum && sum.Purpose != "":
			body = append(body, clamp(wrap(sMuted.Render(sum.Purpose), in), 1, in)...)
			hasPurpose = true
		}
		if hasSum && c.r.Total > 0 {
			hl := sum.Headline
			if hl == "" {
				hl = agents.FirstSentence(sum.Now, 200)
			}
			if hasPurpose {
				body = append(body, "")
			}
			body = append(body, clamp(wrap(sText.Render(cleanProse(hl)), in), 2, in)...)
		}
		if c.r.NeedsYou > 0 || !hasSum {
			if hl := m.headline(c.s); hl != "" {
				body = append(body, clamp(wrap(hl, in), 2, in)...)
			}
		}
		border := cLine
		if c.r.NeedsYou > 0 {
			border = cAmber
		}
		id := "proj:" + c.p.ID
		pg.add("")
		pg.addCard(cardRef{id: id, kind: cardProject, pid: c.p.ID}, frame(m.cardBorder(id, border), w, 0, body))
	}
	if n := len(m.sessionsIn(sideItem{kind: sideInbox})); n > 0 {
		pg.add("", sMagenta.Render("◇ ")+sMuted.Render(plural(n, "session")+" not in a project yet. Open Unassigned to sort them."))
	}
	return pg
}

func (m Model) triage(r core.Rollup) string {
	needs := sSubtle.Render("○ 0 needs you")
	if r.NeedsYou > 0 {
		needs = sAmber.Bold(true).Render(fmt.Sprintf("● %d needs you", r.NeedsYou))
	}
	working := sSubtle.Render("○ 0 working")
	if r.Working > 0 {
		working = sGreen.Render(fmt.Sprintf("● %d working", r.Working))
	}
	ready := sSubtle.Render("✓ 0 ready")
	if r.Ready > 0 {
		ready = sCyan.Render(fmt.Sprintf("✓ %d ready", r.Ready))
	}
	quiet := sSubtle.Render(fmt.Sprintf("○ %d quiet", r.Total-r.NeedsYou-r.Working-r.Ready))
	return needs + "   " + working + "   " + ready + "   " + quiet
}

// headline is the single most useful line about a group of sessions.
func (m Model) headline(ss []core.Session) string {
	now := m.opt.Now()
	ss = append([]core.Session{}, ss...)
	core.SortSessions(ss, now)
	for _, s := range ss {
		switch s.State(now) {
		case agents.NeedsYou:
			return sAmber.Render("Needs you: ") + sText.Render(cleanProse(s.Info.Reason))
		case agents.Working:
			if s.Info.Ask != "" {
				return sGreen.Render("Working on: ") + sText.Render(cleanProse(s.Info.Ask))
			}
			return sGreen.Render("Working: ") + sText.Render(s.Title())
		case agents.Ready:
			if len(s.Info.Results) > 0 {
				return sCyan.Render("Finished: ") + sText.Render(cleanProse(agents.FirstSentence(s.Info.Results[0].Text, 200)))
			}
		}
	}
	return ""
}

func (m Model) needsYouBlock(w int, ss []core.Session, showPlace bool) ([]string, []cardRef) {
	now := m.opt.Now()
	in := w - 4
	pw := proseWidth(in)
	var out []string
	var cards []cardRef
	for _, s := range ss {
		if s.State(now) != agents.NeedsYou {
			continue
		}
		name := sAmber.Bold(true).Render(s.Title())
		if showPlace {
			name = m.where(s) + name
		}
		body := []string{
			spread(sAmber.Bold(true).Render("NEEDS YOU"), sSubtle.Render(core.Ago(s.Info.UpdatedAt, now)), in),
			trunc(name, in),
		}
		id := "needs:" + s.Key
		reason := cleanProse(s.Info.Reason)
		if m.open(id) {
			body = append(body, wrap(sText.Render(reason), pw)...)
			body = append(body, m.expandedSession(s, in)...)
		} else {
			body = append(body, clamp(wrap(sText.Render(reason), pw), 3, in)...)
		}
		cue := "enter more"
		if m.open(id) {
			cue = "esc collapses"
		}
		body = append(body, "", sKey.Render("i")+sAmber.Render(" answer")+sSubtle.Render("  ")+sKey.Render("v")+sMuted.Render(" dock")+sSubtle.Render("  ")+sKey.Render("]d")+sSubtle.Render(" selects it · "+cue))
		if out != nil {
			out = append(out, "")
		}
		c := cardRef{id: id, kind: cardSession, key: s.Key, start: len(out)}
		out = append(out, frame(m.cardBorder(id, cAmber), w, 0, body)...)
		c.end = len(out)
		cards = append(cards, c)
	}
	return out, cards
}

// where prefixes a session name with its project, or marks it unassigned.
func (m Model) where(s core.Session) string {
	if a, ok := m.assignment(s); ok {
		return sSubtle.Render(m.placeName(a.Project, a.Thread) + " › ")
	}
	return sMagenta.Render("◇ ")
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func (m Model) viewInbox(w int) page {
	now := m.opt.Now()
	ss := m.sessionsIn(sideItem{kind: sideInbox})
	var pg page
	pg.add("", sKicker.Render("UNASSIGNED"), sTitle.Render(plural(len(ss), "session")+" to sort"))
	pg.add(wrap(sMuted.Render("New sessions land here. Put each one in a project so its work rolls up into one place."), w)...)
	if len(ss) == 0 {
		pg.add("", sGreen.Render("✓ ")+sMuted.Render("Everything is sorted."))
		return pg
	}
	if len(m.st.Projects) == 0 {
		pg.add("", sMuted.Render("Start with ")+sKey.Render("n")+sMuted.Render(" to create a project, or select a session and press ")+sKey.Render("m")+sMuted.Render("."))
	}
	sel, _ := m.selected()
	for _, s := range ss {
		pg.add("")
		var lines []string
		title := dot(s.State(now)) + " " + sBold.Render(s.Title())
		if s.Key == sel.Key {
			title = sBlue.Render("▸ ") + title
		}
		lines = append(lines, spread(title, stateWord(s.State(now))+sSubtle.Render(" · "+core.Ago(s.Info.UpdatedAt, now)), w))
		lines = append(lines, "  "+sSubtle.Render(s.Pane.Target()+" · "+s.Kind+" · "+shortPath(s.Info.Cwd)))
		if s.Info.Ask != "" {
			lines = append(lines, clamp(indent(wrap(sMuted.Render("asked: ")+sText.Render(s.Info.Ask), w-2), 2), 2, w)...)
		}
		if sg, ok := core.Suggest(s, m.st, m.sessions); ok {
			lines = append(lines, "  "+sCyan.Render("→ ")+sText.Render(m.placeName(sg.Project, sg.Thread))+sSubtle.Render(" ("+sg.Why+")  ")+sKey.Render("a")+sMuted.Render(" accept  ")+sKey.Render("m")+sMuted.Render(" choose"))
		}
		id := "sess:" + s.Key
		if m.open(id) {
			lines = append(lines, indent(m.expandedSession(s, w-2), 2)...)
		}
		pg.addCard(cardRef{id: id, kind: cardSession, key: s.Key}, lines)
	}
	return pg
}

func (m Model) viewProject(w int, sc sideItem) page {
	now := m.opt.Now()
	p := m.st.Project(sc.pid)
	ss := m.sessionsIn(sc)
	r := core.Roll(ss, now)

	kicker := "Project"
	title := p.Name
	if t := p.Thread(sc.tid); t != nil {
		kicker = "Project › " + p.Name
		title = t.Name
	}
	meta := sSubtle.Render("no live sessions")
	if r.Total > 0 {
		meta = stateWord(r.State) + sSubtle.Render(" · updated "+core.Ago(r.UpdatedAt, now))
	}
	lines := []string{"", sSubtle.Render(kicker), spread(sBold.Render(title), meta, w)}
	sum, hasSum := m.summaryFor(p.ID)
	pw := proseWidth(w)
	switch {
	case p.Purpose != "":
		lines = append(lines, wrap(sMuted.Render(p.Purpose), pw)...)
	case hasSum && sum.Purpose != "":
		lines = append(lines, wrap(sSubtle.Render("Probably: ")+sMuted.Render(sum.Purpose)+sSubtle.Render("  ")+sKey.Render("e")+sSubtle.Render(" to confirm or edit"), pw)...)
	default:
		lines = append(lines, sSubtle.Render("No description yet. Press ")+sKey.Render("e")+sSubtle.Render(" to say what this project is for."))
	}
	lines = append(lines, rule(w))
	pg := page{lines: lines}

	if c := m.happeningCard(w, p.ID, ss, r); c != nil {
		pg.add("")
		pg.addCard(cardRef{id: "happening"}, c)
	}

	if block, cs := m.needsYouBlock(w, ss, false); block != nil {
		pg.add("")
		pg.addPart(block, cs)
	}

	if hasSum && sum.Next != "" && r.Total > 0 {
		pg.add("", sBlue.Render("→ ")+sBlue.Bold(true).Render("NEXT UP"))
		pg.add(indent(wrap(sText.Render(cleanProse(sum.Next)), proseWidth(w-2)), 2)...)
	}

	pg.addPart(m.workInMotion(w, p, sc, ss))
	maxCloud := 6
	if m.open("cloud") {
		maxCloud = 1 << 20
	}
	if c, cs := m.cloudSection(ss, w, maxCloud, false); c != nil {
		pg.addPart(c, cs)
	}
	lines = nil

	quiet := 0
	for _, s := range ss {
		if st := s.State(now); st == agents.Quiet || st == agents.Ended || st == agents.Unknown {
			quiet++
		}
	}
	if quiet > 0 {
		lines = append(lines, "", sSubtle.Render("○ "+plural(quiet, "quiet session")+" (nothing new in the last day)"))
	}
	pg.add(lines...)
	return pg
}

// happeningCard is the raised card at the top of a brief: a headline, the
// story, and what finished since the last look.
func (m Model) happeningCard(w int, pid string, ss []core.Session, r core.Rollup) []string {
	now := m.opt.Now()
	in := w - 4
	pw := proseWidth(in)
	sum, hasSum := m.summaryFor(pid)
	head := spread(sBlue.Bold(true).Render("HAPPENING NOW"), m.summaryMeta(pid), in)
	body := []string{head}
	open := m.open("happening")
	switch {
	case hasSum:
		hl, rest := sum.Headline, sum.Now
		if hl == "" {
			hl = agents.FirstSentence(sum.Now, 200)
			rest = strings.TrimSpace(strings.TrimPrefix(sum.Now, hl))
		}
		body = append(body, wrap(sBold.Render(cleanProse(hl)), pw)...)
		if rest != "" {
			body = append(body, "")
			body = append(body, wrap(sText.Render(cleanProse(rest)), pw)...)
		}
		if open {
			body = append(body, "", sLine.Render(strings.Repeat("─", in)), sSubtle.Render("Sessions"))
			for _, s := range ss {
				st := s.State(now)
				if st == agents.Quiet || st == agents.Ended || st == agents.Unknown {
					continue
				}
				body = append(body, spread(dot(st)+" "+sBold.Render(trunc(s.Title(), in-12)), sSubtle.Render(core.Ago(s.Info.UpdatedAt, now)), in))
				if d := cleanProse(m.status1(s)); d != "" {
					body = append(body, indent(wrap(sText.Render(d), proseWidth(in-2)), 2)...)
				}
			}
		}
	default:
		var moving []string
		for _, s := range ss {
			if s.State(now) != agents.Working {
				continue
			}
			moving = append(moving, spread(sGreen.Render("● ")+sBold.Render(s.Title()), sSubtle.Render(core.Ago(s.Info.UpdatedAt, now)), in))
			if d := cleanProse(m.doing(s)); d != "" {
				moving = append(moving, clamp(indent(wrap(sText.Render(d), proseWidth(in-2)), 2), 2, in)...)
			}
		}
		switch {
		case moving != nil:
			body = append(body, moving...)
		case r.Total == 0:
			body = append(body, sSubtle.Render("No live sessions. Move one into this project with ")+sKey.Render("m")+sSubtle.Render("."))
		case r.NeedsYou > 0 && len(core.RecentResults(ss, 1)) == 0:
			return nil // the needs-you card says it all
		default:
			body = append(body, sSubtle.Render("Nothing is running right now."))
		}
	}

	nres := 4
	if open {
		nres = 12
	}
	results := core.RecentResults(ss, nres)
	if len(results) > 0 {
		since := m.newSince[pid]
		label := "Recently finished"
		for _, it := range results {
			if it.At.After(since) && !since.IsZero() {
				label = "Since you last looked"
				break
			}
		}
		body = append(body, "", sLine.Render(strings.Repeat("─", in)), sSubtle.Render(label))
		anyNew := label == "Since you last looked"
		for _, it := range results {
			tag := ""
			if anyNew {
				tag = "    "
			}
			if it.At.After(since) && !since.IsZero() {
				tag = sNewTag.Render("new") + " "
			}
			age := core.Ago(it.At, now)
			if open {
				body = append(body, spread(tag+sSubtle.Render(it.Session), sSubtle.Render(age), in))
				body = append(body, clamp(indent(wrap(sText.Render(cleanProse(it.Text)), proseWidth(in-4)), 4), 6, in)...)
				continue
			}
			clean := cleanProse(agents.FirstSentence(it.Text, 240))
			text := sText.Render(trunc(clean, in-6-len(age)))
			body = append(body, spread(tag+text, sSubtle.Render(age), in))
		}
	}
	c := cBlue
	if !hasSum && r.Working == 0 {
		c = cLine
	}
	if open {
		body = append(body, "", sSubtle.Render("▾ esc collapses"))
	} else if m.cardSel("happening") {
		body = append(body, "", sSubtle.Render("▸ enter for every session and result"))
	}
	return cardOn(c, m.cardBg("happening"), m.cardSel("happening"), w, body)
}

// workInMotion shows threads, or else the active sessions, as cards.
func (m Model) workInMotion(w int, p *store.Project, sc sideItem, ss []core.Session) ([]string, []cardRef) {
	now := m.opt.Now()
	type item struct {
		title, meta, status, needs string
		state                      agents.State
		ref                        cardRef
		sess                       *core.Session
	}
	var items []item
	threads := sc.kind == sideProject && len(p.Threads) > 0
	if threads {
		for _, t := range p.Threads {
			ts := m.sessionsIn(sideItem{kind: sideThread, pid: p.ID, tid: t.ID})
			tr := core.Roll(ts, now)
			meta := ""
			if tr.Total > 0 {
				meta = core.Ago(tr.UpdatedAt, now)
				if tr.Total > 1 {
					meta = fmt.Sprintf("%d · %s", tr.Total, meta)
				}
			}
			it := item{title: t.Name, meta: meta, state: tr.State, ref: cardRef{id: "thread:" + t.ID, kind: cardThread, pid: p.ID, tid: t.ID}}
			core.SortSessions(ts, now)
			for _, s := range ts {
				if it.status == "" {
					it.status = m.status1(s)
				}
				if it.needs == "" && s.State(now) == agents.NeedsYou {
					it.needs = s.Info.Reason
				}
			}
			if it.status == "" {
				it.status = plural(tr.Total, "session")
			}
			items = append(items, it)
		}
	} else {
		sorted := append([]core.Session{}, ss...)
		core.SortSessions(sorted, now)
		for _, s := range sorted {
			st := s.State(now)
			if st != agents.NeedsYou && st != agents.Working && st != agents.Ready {
				continue
			}
			sCopy := s
			it := item{title: s.Title(), meta: core.Ago(s.Info.UpdatedAt, now), state: st, status: m.status1(s), ref: cardRef{id: "sess:" + s.Key, kind: cardSession, key: s.Key}, sess: &sCopy}
			if st == agents.NeedsYou {
				it.needs = s.Info.Reason
			}
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	extra := plural(len(items), "thread") + " · enter opens"
	if !threads {
		extra = plural(len(items), "active session") + " · enter shows more"
	}
	lines := []string{"", section("WORK IN MOTION", extra)}
	var cards []cardRef
	cols, gap := 2, 2
	if w < 70 || len(items) == 1 {
		cols = 1
	}
	render := func(it item, cw int) []string {
		in := cw - 4
		open := it.sess != nil && m.open(it.ref.id)
		keep := 2
		if open {
			keep = 1 << 20
		}
		meta := ""
		if it.meta != "" {
			meta = sSubtle.Render(it.meta)
		}
		head := spread(dot(it.state)+" "+sBold.Render(trunc(it.title, in-3-lipgloss.Width(meta))), meta, in)
		body := []string{head}
		if it.status != "" {
			body = append(body, clamp(wrap(sText.Render(cleanProse(it.status)), proseWidth(in)), keep, in)...)
		}
		if it.needs != "" {
			body = append(body, clamp(wrap(sAmber.Render("Needs you: "+cleanProse(it.needs)), proseWidth(in)), keep, in)...)
		}
		if open {
			body = append(body, m.expandedSession(*it.sess, in)...)
		}
		return body
	}
	border := func(it item) lipgloss.Color {
		c := cLine
		if it.state == agents.NeedsYou {
			c = cAmber
		}
		return m.cardBorder(it.ref.id, c)
	}
	// Pair cards into rows. An expanded card takes a row to itself.
	for i := 0; i < len(items); {
		n := 1
		if cols == 2 && i+1 < len(items) && !m.open(items[i].ref.id) && !m.open(items[i+1].ref.id) {
			n = 2
		}
		start := len(lines)
		if n == 1 {
			lines = append(lines, frame(border(items[i]), w, 0, render(items[i], w))...)
		} else {
			cw := (w - gap) / 2
			a, b := render(items[i], cw), render(items[i+1], cw)
			h := max(len(a), len(b)) + 2
			lines = append(lines, sideBySide(frame(border(items[i]), cw, h, a), frame(border(items[i+1]), cw, h, b), cw, gap)...)
		}
		for j := i; j < i+n; j++ {
			c := items[j].ref
			c.start, c.end = start, len(lines)
			cards = append(cards, c)
		}
		i += n
	}
	return lines, cards
}

// status1 is the one line that best says what a session is doing.
func (m Model) status1(s core.Session) string {
	if d := m.doing(s); d != "" {
		return d
	}
	if len(s.Info.Results) > 0 {
		return agents.FirstSentence(s.Info.Results[0].Text, 200)
	}
	return ""
}

// crumb names the place on screen for the header.
func (m Model) crumb() string {
	sc := m.scope()
	switch sc.kind {
	case sideOverview:
		return "Overview"
	case sideInbox:
		return "Unassigned"
	}
	return m.placeName(sc.pid, sc.tid)
}

// ---- sessions pane ----

func (m Model) buildSessionsPane(w, h int) ([]string, []int) {
	v := m.visible()
	titleStyle := sMuted.Bold(true)
	if m.focus == focusSessions {
		titleStyle = sTitle
	}
	pill := fmt.Sprintf(" %d ", len(v))
	if len(v) > 0 && (m.focus == focusSessions || len(v)*2 > h/2) {
		pill = fmt.Sprintf(" %d/%d ", m.sessIdx+1, len(v))
	}
	head := spread(titleStyle.Render("Sessions"), sPill.Render(pill), w)
	lines := []string{"", head}
	sessAt := []int{-1, -1}
	if m.filter != "" {
		lines = append(lines, sSubtle.Render("filter /")+sMagenta.Render(m.filter)+sSubtle.Render(" · esc clears"))
		sessAt = append(sessAt, -1)
	}
	lines = append(lines, "")
	sessAt = append(sessAt, -1)
	listH := h/2 - 2
	if listH < 4 {
		listH = 4
	}
	rows, _, rowIdx := m.buildSessionRows(w, listH)
	lines = append(lines, rows...)
	sessAt = append(sessAt, rowIdx...)
	lines = append(lines, "", rule(w))
	sessAt = append(sessAt, -1, -1)
	det := m.viewDetail(w)
	lines = append(lines, det...)
	for range det {
		sessAt = append(sessAt, -1)
	}
	return lines, sessAt
}

func (m Model) viewSessionsPane(w, h int) []string {
	lines, _ := m.buildSessionsPane(w, h)
	return lines
}

func (m Model) sessionAt(w, h, ry int) (int, bool) {
	_, sessAt := m.buildSessionsPane(w, h)
	if ry < 0 || ry >= len(sessAt) || sessAt[ry] < 0 {
		return 0, false
	}
	return sessAt[ry], true
}

// sessionRows renders the session list and the row index of the selection.
// maxRows of 0 means no limit.
func (m Model) sessionRows(w, maxRows int) ([]string, int) {
	rows, sel, _ := m.buildSessionRows(w, maxRows)
	return rows, sel
}

func (m Model) buildSessionRows(w, maxRows int) ([]string, int, []int) {
	now := m.opt.Now()
	v := m.visible()
	if len(v) == 0 {
		if m.filter != "" {
			return []string{sSubtle.Render("No sessions match. esc clears the filter.")}, 0, []int{-1}
		}
		return []string{sSubtle.Render("No sessions here.")}, 0, []int{-1}
	}
	var rows []string
	var rowIdx []int
	selRow := 0
	for i, s := range v {
		st := s.State(now)
		age := core.Ago(s.Info.UpdatedAt, now)
		titleStyle := sText
		if i == m.sessIdx {
			titleStyle = sBold
		}
		// Put the status dot beside the title so status and name scan in one fixation.
		one := spread(dot(st)+" "+titleStyle.Render(trunc(s.Title(), w-6-len(age))), sSubtle.Render(age), w-2)
		meta := stateWord(st) + sSubtle.Render(" · "+agentName(s.Kind)+" · "+s.Pane.Target())
		if m.docked != "" && s.Pane.PaneID == m.docked {
			meta = sTeal.Render("⇄ docked") + sSubtle.Render(" · ") + meta
		}
		two := "  " + trunc(meta, w-4)
		if i == m.sessIdx {
			selRow = len(rows)
			bg := cRaised
			bar := sLine.Render("│")
			if m.focus == focusSessions {
				bg = cSelection
				bar = sBlue.Render("▌")
			}
			rows = append(rows, paint(bar+" "+pad(one, w-2), bg), paint(bar+" "+pad(two, w-2), bg))
		} else {
			rows = append(rows, "  "+one, "  "+two)
		}
		rowIdx = append(rowIdx, i, i)
	}
	if maxRows > 0 && len(rows) > maxRows {
		// Reserve a line for top/bottom overflow cues when clipped.
		start := selRow - maxRows/2
		if start < 0 {
			start = 0
		}
		if start > len(rows)-maxRows {
			start = len(rows) - maxRows
		}
		// Snap start to an even item boundary so a session is never split in half.
		start &^= 1
		if start > len(rows)-maxRows {
			start = max(0, (len(rows)-maxRows+1)&^1)
		}
		avail := maxRows
		above := start / 2
		var out []string
		var outIdx []int
		if above > 0 {
			out = append(out, spread("", sSubtle.Render(fmt.Sprintf("↑ %d more", above)), w))
			outIdx = append(outIdx, -1)
			avail--
		}
		end := start + avail
		hasBelow := end < len(rows)
		if hasBelow {
			end = max(start, end-1)
		}
		if end > len(rows) {
			end = len(rows)
		}
		out = append(out, rows[start:end]...)
		outIdx = append(outIdx, rowIdx[start:end]...)
		if hasBelow {
			below := (len(rows) - end + 1) / 2
			out = append(out, spread("", sSubtle.Render(fmt.Sprintf("↓ %d more", below)), w))
			outIdx = append(outIdx, -1)
		}
		selRow = max(0, selRow-start)
		return out, selRow, outIdx
	}
	return rows, selRow, rowIdx
}

func agentName(kind string) string {
	switch kind {
	case "copilot":
		return "Copilot"
	case "claude":
		return "Claude Code"
	}
	return kind
}

func (m Model) viewDetail(w int) []string {
	s, ok := m.selected()
	if !ok {
		return nil
	}
	return m.detailFor(s, w, false)
}

// detailFor describes one session. full drops the title, which the card
// already shows, and the line limits.
func (m Model) detailFor(s core.Session, w int, full bool) []string {
	now := m.opt.Now()
	st := s.State(now)
	clamp := clamp
	cloudMax := 4
	if full {
		clamp = func(l []string, _ int, _ int) []string { return l }
		cloudMax = 1 << 20
	}
	lines := []string{sBold.Render(trunc(s.Title(), w)), stateWord(st) + sSubtle.Render(" · "+agentName(s.Kind)+" · updated "+core.Ago(s.Info.UpdatedAt, now))}
	if full {
		lines = nil
	}
	if st == agents.NeedsYou && s.Info.Reason != "" && !full {
		lines = append(lines, "")
		lines = append(lines, clamp(wrap(sAmber.Render(cleanProse(s.Info.Reason)), w), 4, w)...)
	}
	// Only show DOING when there is an actual summary distinct from the raw ask.
	if doing := cleanProse(m.summaryDoing(s)); doing != "" {
		lines = append(lines, "", sKicker.Render("DOING")+sSubtle.Render("  summary"))
		lines = append(lines, clamp(wrap(sText.Render(doing), w), 3, w)...)
	}
	if s.Info.Ask != "" {
		lines = append(lines, "", sKicker.Render("LAST ASKED")+sSubtle.Render("  "+core.Ago(s.Info.AskAt, now)))
		lines = append(lines, clamp(wrap(sText.Render(cleanProse(s.Info.Ask)), w), 4, w)...)
	}
	if len(s.Info.Results) > 0 {
		r := s.Info.Results[0]
		lines = append(lines, "", sKicker.Render("LATEST RESULT")+sSubtle.Render("  "+core.Ago(r.At, now)))
		lines = append(lines, clamp(wrap(sText.Render(cleanProse(r.Text)), w), 5, w)...)
	}
	c, _ := m.cloudSection([]core.Session{s}, w, cloudMax, true)
	lines = append(lines, c...)
	lines = append(lines, "", sKicker.Render("WHERE"))
	lines = append(lines, sTeal.Render("tmux ")+sText.Render(s.Pane.Target()))
	lines = append(lines, sTeal.Render("dir  ")+sMuted.Render(trunc(shortPath(s.Info.Cwd), w-5)))
	if s.Info.Repo != "" {
		repo := s.Info.Repo
		if s.Info.Branch != "" {
			repo += " @ " + s.Info.Branch
		}
		lines = append(lines, sTeal.Render("repo ")+sMuted.Render(trunc(repo, w-5)))
	}
	if a, ok := m.assignment(s); ok {
		lines = append(lines, sTeal.Render("in   ")+sText.Render(trunc(m.placeName(a.Project, a.Thread), w-5)))
	} else {
		lines = append(lines, sTeal.Render("in   ")+sMagenta.Render("unassigned")+sSubtle.Render(" · m to move"))
	}
	return lines
}

// ---- modals ----

func (m Model) viewOrganize(w int) []string {
	s, _ := m.orgSession()
	cur, assigned := m.assignment(s)
	sg, hasSug := core.Suggest(s, m.st, m.sessions)
	lines := []string{"", sKicker.Render("ORGANIZE"), sTitle.Render(s.Title()), sMuted.Render("Where does this session's work belong?"), ""}
	for i, o := range m.orgOpts {
		label := sText.Render(o.label)
		switch {
		case o.newProj:
			lines = append(lines, "")
			label = sCyan.Render("+ " + o.label)
		case o.unassign:
			label = sMagenta.Render("◇ " + o.label)
		case o.tid != "":
			label = sLine.Render("  │ ") + sMuted.Render(o.label)
		default:
			label = sBold.Render(o.label)
		}
		tag := ""
		if assigned && o.pid == cur.Project && o.tid == cur.Thread && !o.newProj && !o.unassign {
			tag = sSubtle.Render("  current")
		} else if !assigned && hasSug && o.pid == sg.Project && o.tid == sg.Thread && !o.newProj && !o.unassign {
			tag = sCyan.Render("  suggested")
		}
		row := "  " + label + tag
		if i == m.orgIdx {
			row = paint(pad(sBlue.Render("▌ ")+label+tag, w), cSelection)
		}
		lines = append(lines, row)
	}
	lines = append(lines, "", sSubtle.Render("t adds a thread to the highlighted project. n creates a project."))
	return lines
}

func (m Model) viewHelp(w int) []string {
	groups := []struct {
		title string
		keys  [][2]string
	}{
		{"MOVE", [][2]string{{"j / k", "down / up in the focused list"}, {"h / l", "move between panels (also tab)"}, {"ctrl-h/j/k/l", "panels, then the tmux pane past the edge"}, {"j / k (middle)", "step through cards; enter opens or expands, esc collapses"}, {"space e / l / z", "toggle projects panel / sessions panel / both"}, {"H / L", "previous / next project"}, {"]d / [d", "next / previous session that needs you"}, {"gg / G", "top / bottom"}, {"ctrl-d / ctrl-u", "scroll the middle pane (ctrl-e / ctrl-y a line, or the wheel)"}}},
		{"ACT", [][2]string{{"enter", "jump to the session's tmux window"}, {"i", "type to the session without leaving"}, {"v", "dock the session's real terminal beside Switchboard; v again sends it back"}, {"o", "start Copilot in this project or thread"}, {"y", "copy how to reach the highlighted machine or cluster"}, {"s", "open a shell on it in a new tmux window"}, {"m", "move the session to a project"}, {"a", "accept the suggested project"}, {"/", "filter sessions; esc clears"}, {"space space", "find any session"}, {"u", "refresh now (also every 3s)"}}},
		{"PROJECTS", [][2]string{{"n", "new project"}, {"t", "new thread in this project"}, {"e", "describe what the project is for"}, {"r", "rename project or thread"}, {"d", "delete project or thread"}}},
		{"COMMANDS", [][2]string{{"space", "menu of actions (which-key)"}, {":q", "quit (also q)"}, {":new name", "new project"}, {":help", "this screen"}}},
		{"COLORS", [][2]string{{sAmber.Render("●"), "needs you: a question or approval is waiting"}, {sGreen.Render("●"), "working"}, {sCyan.Render("✓"), "finished and ready for your next instruction"}, {sSubtle.Render("○"), "quiet: nothing new in a day"}, {sNewTag.Render("new"), "finished since you last opened the project"}}},
	}
	lines := []string{"", sKicker.Render("HELP"), sTitle.Render("Keys"), ""}
	const kw = 18
	descW := max(16, w-kw-2)
	for _, g := range groups {
		lines = append(lines, sKicker.Render(g.title))
		for _, k := range g.keys {
			wrapped := wrap(sMuted.Render(k[1]), descW)
			for i, wl := range wrapped {
				if i == 0 {
					lines = append(lines, "  "+pad(sKey.Render(k[0]), kw)+wl)
				} else {
					lines = append(lines, strings.Repeat(" ", kw+2)+wl)
				}
			}
		}
		lines = append(lines, "")
	}
	lines = append(lines, wrap(sSubtle.Render("j/k or ctrl-d/u scrolls; esc or any other key closes. Groupings are saved in "+shortPath(m.opt.StatePath)), w)...)
	return lines
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && p != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return p
}

// summaryDoing returns the AI summary's one-line status for s, or empty.
func (m Model) summaryDoing(s core.Session) string {
	if a, ok := m.assignment(s); ok {
		if sum, ok := m.summaryFor(a.Project); ok {
			return sum.Sessions[s.Key]
		}
	}
	return ""
}

// doing is the best one-line description of what a session is doing: the
// summary's line when there is one, otherwise the user's latest ask.
func (m Model) doing(s core.Session) string {
	if d := m.summaryDoing(s); d != "" {
		return d
	}
	return s.Info.Ask
}

// summaryMeta is the summary's age, progress, or failure, for a heading.
func (m Model) summaryMeta(pid string) string {
	if m.opt.Summarize == nil {
		return ""
	}
	sum := m.sums.Projects[pid]
	switch {
	case m.sumBusy == pid:
		return sSubtle.Render("writing summary…")
	case sum.Err != "" && sum.Now != "":
		return sRed.Render("rewrite failed") + sSubtle.Render(" · text from "+core.Ago(sum.At, m.opt.Now())+" · space s retries")
	case sum.Err != "":
		return sRed.Render("summary failed") + sSubtle.Render(" · space s retries")
	case sum.Now != "":
		return sSubtle.Render("summary " + core.Ago(sum.At, m.opt.Now()) + " · space s rewrites")
	}
	return ""
}
