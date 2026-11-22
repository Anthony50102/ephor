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

// View renders the whole screen.
func (m Model) View() string {
	w, h := m.width, m.height
	if w < 60 || h < 12 {
		return "Switchboard needs at least 60×12. Make the window bigger."
	}
	bodyH := h - 2
	wide := w >= wideEnough
	sideW := sideWide
	if w < 100 {
		sideW = sideNarrow
	}
	centerW := w - sideW - 1
	if wide {
		centerW -= rightW + 1
	}

	side := box(m.viewSide(sideW, bodyH), sideW, bodyH)
	var center []string
	selLine := -1
	switch m.mode {
	case modeOrganize:
		center = m.viewOrganize(centerW - 4)
	case modeHelp:
		center = m.viewHelp(centerW - 4)
	default:
		center, selLine = m.viewCenter(centerW-4, !wide)
	}
	if m.mode == modeNormal || m.mode == modeInput || m.mode == modeConfirm {
		off := m.centerOff
		// In the narrow layout the session list is inside the brief, so
		// follow the selection when the list has focus.
		if selLine >= 0 && m.focus == focusSessions && (selLine < off || selLine >= off+bodyH-2) {
			off = selLine - bodyH/3
		}
		if off < 0 {
			off = 0
		}
		if max := len(center) - bodyH + 1; off > max {
			off = max
		}
		if off > 0 {
			center = center[off:]
		}
	}
	center = box(indent(center, 2), centerW, bodyH)
	sep := paint(sLine.Render("│"), cPanel)

	rows := make([]string, bodyH)
	var right []string
	if wide {
		right = box(indent(m.viewSessionsPane(rightW-2, bodyH), 1), rightW, bodyH)
	}
	for i := 0; i < bodyH; i++ {
		r := paint(side[i], cPanel) + sep + paint(center[i], cBg)
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
	var parts []string
	if r.NeedsYou > 0 {
		parts = append(parts, sBadge.Render(fmt.Sprintf(" %d needs you ", r.NeedsYou)))
	}
	parts = append(parts,
		sGreen.Render("●")+sMuted.Render(fmt.Sprintf(" %d working", r.Working)),
		sCyan.Render("✓")+sMuted.Render(fmt.Sprintf(" %d ready", r.Ready)),
	)
	if inbox > 0 {
		parts = append(parts, sMagenta.Render("◇")+sMuted.Render(fmt.Sprintf(" %d unassigned", inbox)))
	}
	if w < 100 {
		parts = parts[:len(parts)-1] // drop the least urgent counts on narrow screens
		if inbox > 0 {
			parts = parts[:len(parts)-1]
		}
	}
	right := strings.Join(parts, "  ") + "  " + sSubtle.Render("? help")
	if !m.loaded {
		right = sSubtle.Render("looking for agents…")
	}
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
	if m.mode == modeOrganize {
		keys = [][2]string{{"j/k", "move"}, {"enter", "place here"}, {"t", "new thread"}, {"n", "new project"}, {"esc", "cancel"}}
	} else if m.focus == focusSide {
		keys = [][2]string{{"j/k", "browse"}, {"H/L", "projects"}, {"]d", "next needs you"}, {"l", "sessions"}, {"n", "new project"}, {"e", "describe"}, {"space", "menu"}}
	} else {
		keys = [][2]string{{"j/k", "select"}, {"enter", "jump"}, {"m", "move"}, {"a", "accept"}, {"]d", "next needs you"}, {"/", "filter"}, {"h", "projects"}, {"space", "menu"}}
	}
	var b []string
	for _, k := range keys {
		b = append(b, sKey.Render(k[0])+" "+sMuted.Render(k[1]))
	}
	line := " " + strings.Join(b, sSubtle.Render("  ·  "))
	if m.filter != "" {
		line += sSubtle.Render("   filter: ") + sMagenta.Render(m.filter)
	}
	return pad(line, w)
}

// ---- sidebar ----

func (m Model) viewSide(w, h int) []string {
	now := m.opt.Now()
	var lines []string
	sel := 0
	add := func(i int, glyph, label, right string) {
		active := i == m.sideIdx
		inner := w - 2
		label = trunc(label, inner-3-lipgloss.Width(right))
		row := spread(glyph+" "+label, right, inner)
		if active {
			sel = len(lines)
			bg := cRaised
			if m.focus == focusSide {
				bg = cSelection
			}
			lines = append(lines, paint(" "+pad(row, inner)+" ", bg))
			return
		}
		lines = append(lines, " "+row)
	}
	lines = append(lines, "", " "+sKicker.Render("WORKSPACE"))
	for i, it := range m.side {
		switch it.kind {
		case sideOverview:
			r := core.Roll(m.sessions, now)
			right := ""
			if r.NeedsYou > 0 {
				right = sBadge.Render(fmt.Sprintf(" %d needs you ", r.NeedsYou))
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
			lines = append(lines, "", " "+sKicker.Render("PROJECTS"))
			if len(m.st.Projects) == 0 {
				lines = append(lines, " "+sSubtle.Render("none yet · n to create"))
			}
		case sideProject:
			p := m.st.Project(it.pid)
			r := core.Roll(m.sessionsIn(it), now)
			right := ""
			if n := m.newCount(*p); n > 0 {
				right = sMagenta.Render(fmt.Sprintf("%d new", n))
			} else if r.Total > 0 {
				right = sSubtle.Render(fmt.Sprint(r.Total))
			}
			if i > 0 && m.side[i-1].kind != sideInbox {
				lines = append(lines, "")
			}
			add(i, dot(r.State), sBold.Render(p.Name), right)
		case sideThread:
			p := m.st.Project(it.pid)
			t := p.Thread(it.tid)
			r := core.Roll(m.sessionsIn(it), now)
			add(i, " "+dot(r.State), sMuted.Render(t.Name), "")
		}
	}
	lines = append(lines, "",
		" "+sAmber.Render("●")+sSubtle.Render(" needs you  ")+sGreen.Render("●")+sSubtle.Render(" working"),
		" "+sCyan.Render("✓")+sSubtle.Render(" ready      ○ quiet"))
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
	}
	return lines
}

// ---- center ----

func (m Model) viewCenter(w int, narrow bool) ([]string, int) {
	if !m.loaded {
		return []string{"", sMuted.Render("Looking for Copilot and Claude Code sessions in tmux…")}, -1
	}
	var lines []string
	sc := m.scope()
	switch sc.kind {
	case sideOverview:
		lines = m.viewOverview(w)
	case sideInbox:
		lines = m.viewInbox(w)
	default:
		lines = m.viewProject(w, sc)
	}
	selLine := -1
	if narrow {
		lines = append(lines, "", sKicker.Render("SESSIONS"))
		rows, sel := m.sessionRows(w, 0)
		selLine = len(lines) + sel
		lines = append(lines, rows...)
		lines = append(lines, "")
		lines = append(lines, m.viewDetail(w)...)
	}
	return lines, selLine
}

func section(title string, extra string) string {
	s := sBlue.Bold(true).Render(title)
	if extra != "" {
		s += "  " + sSubtle.Render(extra)
	}
	return s
}

func (m Model) viewOverview(w int) []string {
	now := m.opt.Now()
	r := core.Roll(m.sessions, now)
	lines := []string{"", sSubtle.Render("Overview"), "", sBold.Render("What needs you, what is moving"), m.triage(r), "", rule(w)}
	if r.Total == 0 {
		lines = append(lines, "", sMuted.Render("No Copilot or Claude Code sessions found in tmux."))
		return lines
	}

	sorted := append([]core.Session{}, m.sessions...)
	core.SortSessions(sorted, now)
	if block := m.needsYouBlock(w, sorted, true); block != nil {
		lines = append(lines, "")
		lines = append(lines, block...)
	}
	in := w - 4
	var moving, ready []string
	for _, s := range sorted {
		name := m.where(s) + sBold.Render(s.Title())
		switch s.State(now) {
		case agents.Working:
			moving = append(moving, name+sSubtle.Render("  "+core.Ago(s.Info.UpdatedAt, now)))
			if d := m.doing(s); d != "" {
				moving = append(moving, clamp(indent(wrap(sText.Render(d), in-2), 2), 1, in)...)
			}
		case agents.Ready:
			ready = append(ready, name+sSubtle.Render("  "+core.Ago(s.Info.UpdatedAt, now)))
			if len(s.Info.Results) > 0 {
				ready = append(ready, clamp(indent(wrap(sText.Render(agents.FirstSentence(s.Info.Results[0].Text, 200)), in-2), 2), 1, in)...)
			}
		}
	}
	if moving != nil {
		lines = append(lines, "")
		lines = append(lines, card(cGreen, w, append([]string{sGreen.Bold(true).Render("HAPPENING NOW"), ""}, moving...))...)
	}
	if ready != nil {
		lines = append(lines, "")
		lines = append(lines, card(cCyan, w, append([]string{spread(sCyan.Bold(true).Render("READY FOR YOU"), sSubtle.Render("finished in the last day"), in), ""}, ready...))...)
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
	lines = append(lines, "", section("PROJECTS", "most urgent first"))
	if len(cards) == 0 {
		lines = append(lines, "", sMuted.Render("No projects yet. Press ")+sKey.Render("n")+sMuted.Render(" to create one, then move sessions into it with ")+sKey.Render("m")+sMuted.Render("."))
	}
	for _, c := range cards {
		head := dot(c.r.State) + " " + sMagenta.Bold(true).Render(c.p.Name)
		meta := stateWord(c.r.State)
		if c.r.Total > 0 {
			meta += sSubtle.Render(" · " + plural(c.r.Total, "session") + " · " + core.Ago(c.r.UpdatedAt, now))
		} else {
			meta = sSubtle.Render("no live sessions")
		}
		body := []string{spread(head, meta, in)}
		sum, hasSum := m.summaryFor(c.p.ID)
		switch {
		case c.p.Purpose != "":
			body = append(body, clamp(wrap(sMuted.Render(c.p.Purpose), in), 1, in)...)
		case hasSum && sum.Purpose != "":
			body = append(body, clamp(wrap(sMuted.Render(sum.Purpose), in), 1, in)...)
		}
		if hasSum && c.r.Total > 0 {
			hl := sum.Headline
			if hl == "" {
				hl = agents.FirstSentence(sum.Now, 200)
			}
			body = append(body, "")
			body = append(body, clamp(wrap(sBold.Render(hl), in), 2, in)...)
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
		lines = append(lines, "")
		lines = append(lines, frame(border, w, 0, body)...)
	}
	if n := len(m.sessionsIn(sideItem{kind: sideInbox})); n > 0 {
		lines = append(lines, "", sMagenta.Render("◇ ")+sMuted.Render(plural(n, "session")+" not in a project yet. Open Unassigned to sort them."))
	}
	return lines
}

func (m Model) triage(r core.Rollup) string {
	return sAmber.Render(fmt.Sprintf("● %d needs you", r.NeedsYou)) + "   " +
		sGreen.Render(fmt.Sprintf("● %d working", r.Working)) + "   " +
		sCyan.Render(fmt.Sprintf("✓ %d ready", r.Ready)) + "   " +
		sSubtle.Render(fmt.Sprintf("○ %d quiet", r.Total-r.NeedsYou-r.Working-r.Ready))
}

// headline is the single most useful line about a group of sessions.
func (m Model) headline(ss []core.Session) string {
	now := m.opt.Now()
	ss = append([]core.Session{}, ss...)
	core.SortSessions(ss, now)
	for _, s := range ss {
		switch s.State(now) {
		case agents.NeedsYou:
			return sAmber.Render("Needs you: ") + sText.Render(s.Info.Reason)
		case agents.Working:
			if s.Info.Ask != "" {
				return sGreen.Render("Working on: ") + sText.Render(s.Info.Ask)
			}
			return sGreen.Render("Working: ") + sText.Render(s.Title())
		case agents.Ready:
			if len(s.Info.Results) > 0 {
				return sCyan.Render("Finished: ") + sText.Render(agents.FirstSentence(s.Info.Results[0].Text, 200))
			}
		}
	}
	return ""
}

func (m Model) needsYouBlock(w int, ss []core.Session, showPlace bool) []string {
	now := m.opt.Now()
	in := w - 4
	var out []string
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
		body = append(body, clamp(wrap(sText.Render(s.Info.Reason), in), 3, in)...)
		body = append(body, "", sKey.Render("enter")+sAmber.Render(" open "+trunc(s.Title(), in-20)+" →")+sSubtle.Render("   ]d selects it"))
		if out != nil {
			out = append(out, "")
		}
		out = append(out, frame(cAmber, w, 0, body)...)
	}
	return out
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

func (m Model) viewInbox(w int) []string {
	now := m.opt.Now()
	ss := m.sessionsIn(sideItem{kind: sideInbox})
	lines := []string{"", sKicker.Render("UNASSIGNED"), sTitle.Render(plural(len(ss), "session") + " to sort")}
	lines = append(lines, wrap(sMuted.Render("New sessions land here. Put each one in a project so its work rolls up into one place."), w)...)
	if len(ss) == 0 {
		return append(lines, "", sGreen.Render("✓ ")+sMuted.Render("Everything is sorted."))
	}
	if len(m.st.Projects) == 0 {
		lines = append(lines, "", sMuted.Render("Start with ")+sKey.Render("n")+sMuted.Render(" to create a project, or select a session and press ")+sKey.Render("m")+sMuted.Render("."))
	}
	sel, _ := m.selected()
	for _, s := range ss {
		lines = append(lines, "")
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
	}
	return lines
}

func (m Model) viewProject(w int, sc sideItem) []string {
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
	lines := []string{"", sSubtle.Render(kicker), "", spread(sBold.Render(title), meta, w)}
	sum, hasSum := m.summaryFor(p.ID)
	switch {
	case p.Purpose != "":
		lines = append(lines, wrap(sMuted.Render(p.Purpose), w)...)
	case hasSum && sum.Purpose != "":
		lines = append(lines, wrap(sSubtle.Render("Probably: ")+sMuted.Render(sum.Purpose)+sSubtle.Render("  ")+sKey.Render("e")+sSubtle.Render(" to confirm or edit"), w)...)
	default:
		lines = append(lines, sSubtle.Render("No description yet. Press ")+sKey.Render("e")+sSubtle.Render(" to say what this project is for."))
	}
	lines = append(lines, "", rule(w))

	if c := m.happeningCard(w, p.ID, ss, r); c != nil {
		lines = append(lines, "")
		lines = append(lines, c...)
	}

	if block := m.needsYouBlock(w, ss, false); block != nil {
		lines = append(lines, "")
		lines = append(lines, block...)
	}

	if hasSum && sum.Next != "" && r.Total > 0 {
		lines = append(lines, "", sBlue.Render("→ ")+sBlue.Bold(true).Render("NEXT UP"))
		lines = append(lines, indent(wrap(sText.Render(sum.Next), w-2), 2)...)
	}

	lines = append(lines, m.workInMotion(w, p, sc, ss)...)
	lines = append(lines, m.cloudSection(ss, w, 6, false)...)

	quiet := 0
	for _, s := range ss {
		if st := s.State(now); st == agents.Quiet || st == agents.Ended || st == agents.Unknown {
			quiet++
		}
	}
	if quiet > 0 {
		lines = append(lines, "", sSubtle.Render("○ "+plural(quiet, "quiet session")+" (nothing new in the last day)"))
	}
	return lines
}

// happeningCard is the raised card at the top of a brief: a headline, the
// story, and what finished since the last look.
func (m Model) happeningCard(w int, pid string, ss []core.Session, r core.Rollup) []string {
	now := m.opt.Now()
	in := w - 4
	sum, hasSum := m.summaryFor(pid)
	head := spread(sBlue.Bold(true).Render("HAPPENING NOW"), m.summaryMeta(pid), in)
	body := []string{head, ""}
	switch {
	case hasSum:
		hl, rest := sum.Headline, sum.Now
		if hl == "" {
			hl = agents.FirstSentence(sum.Now, 200)
			rest = strings.TrimSpace(strings.TrimPrefix(sum.Now, hl))
		}
		body = append(body, wrap(sBold.Render(hl), in)...)
		if rest != "" {
			body = append(body, "")
			body = append(body, wrap(sText.Render(rest), in)...)
		}
	default:
		var moving []string
		for _, s := range ss {
			if s.State(now) != agents.Working {
				continue
			}
			moving = append(moving, sGreen.Render("● ")+sBold.Render(s.Title())+sSubtle.Render("  "+core.Ago(s.Info.UpdatedAt, now)))
			if d := m.doing(s); d != "" {
				moving = append(moving, clamp(indent(wrap(sText.Render(d), in-2), 2), 2, in)...)
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

	results := core.RecentResults(ss, 4)
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
			text := sText.Render(trunc(agents.FirstSentence(it.Text, 240), in-6-len(age)))
			body = append(body, spread(tag+text, sSubtle.Render(age), in))
		}
	}
	c := cBlue
	if !hasSum && r.Working == 0 {
		c = cLine
	}
	return card(c, w, body)
}

// workInMotion shows threads, or else the active sessions, as cards.
func (m Model) workInMotion(w int, p *store.Project, sc sideItem, ss []core.Session) []string {
	now := m.opt.Now()
	type item struct {
		title, status, needs string
		state                agents.State
	}
	var items []item
	if sc.kind == sideProject && len(p.Threads) > 0 {
		for _, t := range p.Threads {
			ts := m.sessionsIn(sideItem{kind: sideThread, pid: p.ID, tid: t.ID})
			tr := core.Roll(ts, now)
			it := item{title: t.Name, state: tr.State}
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
			it := item{title: s.Title(), state: st, status: m.status1(s)}
			if st == agents.NeedsYou {
				it.needs = s.Info.Reason
			}
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return nil
	}
	extra := plural(len(items), "thread")
	if !(sc.kind == sideProject && len(p.Threads) > 0) {
		extra = plural(len(items), "active session")
	}
	lines := []string{"", section("WORK IN MOTION", extra)}
	cols, gap := 2, 2
	if w < 70 || len(items) == 1 {
		cols = 1
	}
	cw := (w - gap*(cols-1)) / cols
	in := cw - 4
	render := func(it item) []string {
		body := []string{dot(it.state) + " " + sMagenta.Bold(true).Render(trunc(it.title, in-2))}
		if it.status != "" {
			body = append(body, "")
			body = append(body, clamp(wrap(sText.Render(it.status), in), 2, in)...)
		}
		if it.needs != "" {
			body = append(body, clamp(wrap(sAmber.Render("Needs you: "+it.needs), in), 2, in)...)
		}
		return body
	}
	for i := 0; i < len(items); i += cols {
		row := items[i:min(i+cols, len(items))]
		h := 0
		var bodies [][]string
		for _, it := range row {
			b := render(it)
			bodies = append(bodies, b)
			h = max(h, len(b)+2)
		}
		var blocks [][]string
		for j, it := range row {
			c := cLine
			if it.state == agents.NeedsYou {
				c = cAmber
			}
			blocks = append(blocks, frame(c, cw, h, bodies[j]))
		}
		if len(blocks) == 2 {
			lines = append(lines, sideBySide(blocks[0], blocks[1], cw, gap)...)
		} else {
			lines = append(lines, blocks[0]...)
		}
	}
	return lines
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

func (m Model) viewSessionsPane(w, h int) []string {
	v := m.visible()
	head := spread(sBold.Render("Sessions"), sPill.Render(fmt.Sprintf(" %d ", len(v))), w)
	hint := sSubtle.Render("Waiting sessions sort first.")
	if m.filter != "" {
		hint = sSubtle.Render("filter /") + sMagenta.Render(m.filter) + sSubtle.Render(" · esc clears")
	}
	lines := []string{"", head, hint, ""}
	listH := h/2 - 2
	if listH < 4 {
		listH = 4
	}
	rows, _ := m.sessionRows(w, listH)
	lines = append(lines, rows...)
	lines = append(lines, "", rule(w))
	lines = append(lines, m.viewDetail(w)...)
	return lines
}

// sessionRows renders the session list and the row index of the selection.
// maxRows of 0 means no limit.
func (m Model) sessionRows(w, maxRows int) ([]string, int) {
	now := m.opt.Now()
	v := m.visible()
	if len(v) == 0 {
		if m.filter != "" {
			return []string{sSubtle.Render("No sessions match. esc clears the filter.")}, 0
		}
		return []string{sSubtle.Render("No sessions here.")}, 0
	}
	var rows []string
	selRow := 0
	for i, s := range v {
		st := s.State(now)
		age := core.Ago(s.Info.UpdatedAt, now)
		one := spread(sText.Render(trunc(s.Title(), w-4-len(age))), sSubtle.Render(age), w-2)
		two := dot(st) + " " + stateWord(st) + sSubtle.Render(" · "+agentName(s.Kind)+" · "+s.Pane.Target())
		if i == m.sessIdx {
			selRow = len(rows)
			bg := cRaised
			if m.focus == focusSessions {
				bg = cSelection
			}
			bar := sBlue.Render("▌")
			rows = append(rows, paint(bar+" "+pad(one, w-2), bg), paint(bar+" "+pad(trunc(two, w-2), w-2), bg))
		} else {
			rows = append(rows, "  "+one, "  "+trunc(two, w-2))
		}
		if i < len(v)-1 {
			rows = append(rows, sLine.Render(strings.Repeat("─", w)))
		}
	}
	if maxRows > 0 && len(rows) > maxRows {
		start := selRow - maxRows/2
		if start < 0 {
			start = 0
		}
		if start > len(rows)-maxRows {
			start = len(rows) - maxRows
		}
		rows = rows[start : start+maxRows]
		selRow -= start
	}
	return rows, selRow
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
	now := m.opt.Now()
	st := s.State(now)
	lines := []string{sBold.Render(trunc(s.Title(), w)), stateWord(st) + sSubtle.Render(" · "+s.Kind+" · updated "+core.Ago(s.Info.UpdatedAt, now))}
	if st == agents.NeedsYou && s.Info.Reason != "" {
		lines = append(lines, "")
		lines = append(lines, clamp(wrap(sAmber.Render(s.Info.Reason), w), 4, w)...)
	}
	if doing := m.doing(s); doing != "" {
		lines = append(lines, "", sKicker.Render("DOING")+sSubtle.Render("  summary"))
		lines = append(lines, clamp(wrap(sText.Render(doing), w), 3, w)...)
	}
	if s.Info.Ask != "" {
		lines = append(lines, "", sKicker.Render("LAST ASKED")+sSubtle.Render("  "+core.Ago(s.Info.AskAt, now)))
		lines = append(lines, clamp(wrap(sText.Render(s.Info.Ask), w), 4, w)...)
	}
	if len(s.Info.Results) > 0 {
		r := s.Info.Results[0]
		lines = append(lines, "", sKicker.Render("LATEST RESULT")+sSubtle.Render("  "+core.Ago(r.At, now)))
		lines = append(lines, clamp(wrap(sText.Render(strings.Join(strings.Fields(r.Text), " ")), w), 5, w)...)
	}
	lines = append(lines, m.cloudSection([]core.Session{s}, w, 4, true)...)
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
		row := "  " + label
		if i == m.orgIdx {
			row = sSel.Render(pad(sBlue.Render("▸ ")+label, w))
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
		{"MOVE", [][2]string{{"j / k", "down / up in the focused list"}, {"h l  ctrl-h ctrl-l", "projects / sessions column (also tab)"}, {"H / L", "previous / next project"}, {"]d / [d", "next / previous session that needs you"}, {"gg / G", "top / bottom"}, {"ctrl-d / ctrl-u", "scroll the brief"}}},
		{"ACT", [][2]string{{"enter", "jump to the session's tmux window"}, {"m", "move the session to a project"}, {"a", "accept the suggested project"}, {"/", "filter sessions; esc clears"}, {"space space", "find any session"}, {"u", "refresh now (also every 3s)"}}},
		{"PROJECTS", [][2]string{{"n", "new project"}, {"t", "new thread in this project"}, {"e", "describe what the project is for"}, {"r", "rename project or thread"}, {"d", "delete project or thread"}}},
		{"COMMANDS", [][2]string{{"space", "menu of actions (which-key)"}, {":q", "quit (also q)"}, {":new name", "new project"}, {":help", "this screen"}}},
		{"COLORS", [][2]string{{sAmber.Render("●"), "needs you: a question or approval is waiting"}, {sGreen.Render("●"), "working"}, {sCyan.Render("✓"), "finished and ready for your next instruction"}, {sSubtle.Render("○"), "quiet: nothing new in a day"}, {sNewTag.Render("new"), "finished since you last opened the project"}}},
	}
	lines := []string{"", sKicker.Render("HELP"), sTitle.Render("Keys"), ""}
	for _, g := range groups {
		lines = append(lines, sKicker.Render(g.title))
		for _, k := range g.keys {
			lines = append(lines, "  "+pad(sKey.Render(k[0]), 18)+sMuted.Render(k[1]))
		}
		lines = append(lines, "")
	}
	lines = append(lines, sSubtle.Render("Press any key to close. Groupings are saved in "+shortPath(m.opt.StatePath)))
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

// doing is the best one-line description of what a session is doing: the
// summary's line when there is one, otherwise the user's latest ask.
func (m Model) doing(s core.Session) string {
	if a, ok := m.assignment(s); ok {
		if sum, ok := m.summaryFor(a.Project); ok {
			if d := sum.Sessions[s.Key]; d != "" {
				return d
			}
		}
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
