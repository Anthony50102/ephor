// Package ui is the Bubble Tea control panel.
package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/cloud"
	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/summary"
	"github.com/anthonypoole/switchboard/internal/tmux"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

const refreshEvery = 3 * time.Second

type focus int

const (
	focusSide focus = iota
	focusCenter
	focusSessions
)

type mode int

const (
	modeNormal mode = iota
	modeInput
	modeOrganize
	modeConfirm
	modeHelp
)

type sideKind int

const (
	sideOverview sideKind = iota
	sideInbox
	sideProject
	sideThread
)

type sideItem struct {
	kind     sideKind
	pid, tid string
}

type inputKind int

const (
	inNewProject inputKind = iota
	inNewThread
	inPurpose
	inRename
	inFilter
	inCommand
	inLaunchDir
	inLaunchTask
	inReply
	inHoldReason
)

type orgOpt struct {
	label    string
	pid, tid string
	newProj  bool
	unassign bool
}

// Options configure a Model. Tests replace the side-effecting functions.
type Options struct {
	Store        *store.State
	StatePath    string
	SnapshotPath string
	Discover     func() ([]core.Session, error)
	Jump         func(tmux.Pane) error // nil means quit and attach from outside tmux
	Now          func() time.Time
	NoTick       bool

	// Summarize writes project summaries; nil turns them off.
	Summarize    summary.Runner
	SummaryPath  string
	SummaryEvery time.Duration

	// CloudPath is where the cloud scan index is kept; empty keeps it in
	// memory only.
	CloudPath string
	NoCloud   bool

	// CheckLive asks Azure whether the resources still exist; nil skips it.
	CheckLive cloud.Checker
	LiveEvery time.Duration

	// Launch opens a tmux window and types a command into it, returning the
	// new pane ID. Type sends text and Enter to a pane. Screen reads a pane.
	// Nil turns the matching feature off.
	Launch func(LaunchReq) (string, error)
	Type   func(paneID, text string) error
	Screen func(paneID string) (string, error)
	Show   func(paneID string) error // switch to a pane; nil outside tmux

	// Dock moves an agent's pane beside Switchboard and Undock sends it
	// back. PaneExists and DockedPane inspect tmux directly so docking stays
	// tracked even while a restored pane is still booting zsh -> copilot.
	Dock       func(paneID string) error
	Undock     func(paneID string) error
	PaneExists func(paneID string) bool
	DockedPane func() string

	// LookupSaved resolves a closed assigned session key from disk so :reopen
	// can bring it back into Recoverable.
	LookupSaved func(key string, a store.Assignment) (store.SavedSession, bool)

	// Park moves an agent pane into the background agents pool session.
	// Rehome updates a docked pane's saved return address to that pool.
	Park   func(paneID string) (bool, error)
	Rehome func(paneID string) error

	// SelectPane moves to the tmux pane beside Switchboard's.
	SelectPane func(dir string) error

	// Prices fetches Azure list prices; nil hides costs. PricePath caches
	// them.
	Prices    cloud.Getter
	PricePath string

	// Kubectl runs kubectl for open cluster cards; nil turns it off.
	Kubectl cloud.Kubectl

	// Copy puts text on the clipboard.
	Copy func(string) error
}

// Model is the Bubble Tea model.
type Model struct {
	opt   Options
	st    *store.State
	stamp string

	sessions   []core.Session
	loaded     bool
	loadErr    error
	refreshing bool

	width, height int
	side          []sideItem
	sideIdx       int
	focus         focus
	sessIdx       int
	selKey        string
	centerOff     int
	helpOff       int
	cardID        string          // highlighted card in the middle pane
	expanded      map[string]bool // cards opened with enter
	hideSide      bool
	hideRight     bool
	filter        string
	pending       string // first key of a multi-key sequence

	mode      mode
	input     textinput.Model
	inputKind inputKind
	inputPID  string
	inputTID  string
	inputKey  string // session to assign after creating a project or thread

	orgKey  string
	orgOpts []orgOpt
	orgIdx  int

	confirmText string
	confirmDo   func(*store.State) error

	sums    *summary.Cache
	sumBusy string // project whose summary is being written

	cloudIx   *cloud.Index // used only by the scan in flight
	cloudBusy bool
	cloudRes  map[string][]cloud.Resource // by session log
	cloudLive map[string]cloud.Live       // by resource ID; nil until Azure answers
	liveBusy  bool
	liveAt    time.Time // when the last check started
	liveKeys0 string    // the resources it asked about
	liveErr   string

	prices     *cloud.Prices
	priceBusy  bool
	priceTried time.Time
	kube       map[string]cloud.Kube // by card ID
	kubeBusy   map[string]bool

	wantKey    string // select this session once it appears
	replyPane  string
	replyTitle string
	screen     string
	screenErr  error
	screenGen  int

	viewedPID string
	newSince  map[string]time.Time

	count     int // Vim-style numeric prefix, e.g. 10 in 10j (0 means unset)
	status    string
	statusErr bool

	docked      string // pane ID of the agent docked beside us
	dockedTitle string
	dockedAt    time.Time

	JumpTarget *tmux.Pane
}

type sessionsMsg struct {
	ss  []core.Session
	err error
}

type tickMsg struct{}

// New builds a model.
func New(opt Options) Model {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 400
	if opt.SummaryEvery == 0 {
		opt.SummaryEvery = DefaultSummaryEvery
	}
	if opt.LiveEvery == 0 {
		opt.LiveEvery = DefaultLiveEvery
	}
	m := Model{opt: opt, st: opt.Store, input: ti, newSince: map[string]time.Time{}, expanded: map[string]bool{}, width: 120, height: 40}
	m.sums = &summary.Cache{Projects: map[string]summary.Summary{}}
	if opt.SummaryPath != "" {
		m.sums = summary.LoadCache(opt.SummaryPath)
	}
	if opt.StatePath != "" {
		m.stamp = store.Stamp(opt.StatePath)
	}
	m.prices = cloud.LoadPrices(opt.PricePath)
	m.kube = map[string]cloud.Kube{}
	m.kubeBusy = map[string]bool{}
	m.rebuildSide()
	return m
}

// Run starts the program. It returns a pane to attach to when the user jumped
// from outside tmux.
func Run(src core.Sources, opt Options) (*tmux.Pane, error) {
	if opt.SnapshotPath == "" && opt.StatePath != "" {
		opt.SnapshotPath = store.DefaultSnapshotPath(opt.StatePath)
	}
	nowFn := opt.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	snapPath := opt.SnapshotPath
	opt.Discover = func() ([]core.Session, error) {
		return core.DiscoverWithSnapshot(src, snapPath, nowFn())
	}
	if tmux.InsideTmux() {
		opt.Jump = func(p tmux.Pane) error {
			if err := src.Tmux.Focus(p); err != nil {
				return err
			}
			return src.Tmux.SwitchClient(p)
		}
	}
	opt.Launch = func(r LaunchReq) (string, error) {
		session := r.Session
		if r.BeforeType == nil && session == tmux.PoolSession {
			session, _ = src.Tmux.LaunchSession(tmux.PoolSession)
		}
		pane, err := src.Tmux.NewWindow(session, r.Window, r.Dir)
		if err != nil {
			return "", err
		}
		if r.BeforeType != nil {
			if err := r.BeforeType(pane); err != nil {
				return pane, err
			}
		}
		if err := src.Tmux.Type(pane, r.Command); err != nil || r.Pretype == "" {
			return pane, err
		}
		return pane, src.Tmux.Pretype(pane, r.Pretype)
	}
	self := os.Getenv("TMUX_PANE")
	opt.Park = func(id string) (bool, error) { return src.Tmux.Park(id, tmux.PoolSession, self) }
	opt.PaneExists = src.Tmux.PaneExists
	opt.LookupSaved = func(key string, a store.Assignment) (store.SavedSession, bool) {
		return core.SavedFromAssigned(key, a, src)
	}
	if tmux.InsideTmux() {
		opt.Show = src.Tmux.Show
		if self != "" {
			opt.Dock = func(id string) error { return src.Tmux.Dock(id, self, dockWidth) }
			opt.Undock = src.Tmux.Undock
			opt.DockedPane = func() string { return src.Tmux.DockedTo(self) }
			opt.Rehome = func(id string) error { return src.Tmux.Rehome(id, tmux.PoolSession) }
			opt.SelectPane = func(dir string) error { return src.Tmux.SelectPane(self, dir) }
			// Agents left docked by a Switchboard that crashed go home.
			if ids, err := src.Tmux.Orphans(); err == nil {
				for _, id := range ids {
					_ = src.Tmux.Undock(id)
				}
			}
		}
	}
	opt.Type = src.Tmux.Type
	opt.Screen = src.Tmux.Screen
	final, err := tea.NewProgram(New(opt), tea.WithAltScreen(), tea.WithMouseCellMotion()).Run()
	if err != nil {
		return nil, err
	}
	fm := final.(Model)
	dockID := fm.Docked()
	if dockID == "" && opt.DockedPane != nil {
		dockID = opt.DockedPane()
	}
	if dockID != "" && opt.Undock != nil {
		if err := opt.Undock(dockID); err != nil {
			return nil, err
		}
	}
	return fm.JumpTarget, nil
}

func (m Model) refresh() tea.Cmd {
	d := m.opt.Discover
	return func() tea.Msg {
		ss, err := d()
		return sessionsMsg{ss, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

// Init starts discovery and the refresh timer.
func (m Model) Init() tea.Cmd {
	if m.opt.NoTick {
		return m.refresh()
	}
	return tea.Batch(m.refresh(), tick())
}

// change applies a state edit. With a state file it reloads the file under a
// lock first, so edits from other Switchboard instances are kept.
func (m *Model) change(fn func(*store.State) error) bool {
	if m.opt.StatePath == "" {
		if err := fn(m.st); err != nil {
			m.setErr(err)
			return false
		}
		core.ApplyHolds(m.sessions, m.st.Holds)
		return true
	}
	st, err := store.Update(m.opt.StatePath, fn)
	if err != nil {
		m.setErr(fmt.Errorf("could not save: %w", err))
		return false
	}
	m.st = st
	m.stamp = store.Stamp(m.opt.StatePath)
	core.ApplyHolds(m.sessions, m.st.Holds)
	m.rebuildSide()
	return true
}

// reload picks up edits made by another Switchboard instance.
func (m *Model) reload() {
	if m.opt.StatePath == "" {
		return
	}
	stamp := store.Stamp(m.opt.StatePath)
	if stamp == m.stamp {
		return
	}
	st, err := store.Load(m.opt.StatePath)
	if err != nil {
		m.setErr(err)
		return
	}
	m.st, m.stamp = st, stamp
	core.ApplyHolds(m.sessions, m.st.Holds)
	m.rebuildSide()
}

func (m *Model) setStatus(s string) { m.status, m.statusErr = s, false }
func (m *Model) setErr(err error)   { m.status, m.statusErr = err.Error(), true }

// ---- sidebar and scope ----

func (m *Model) rebuildSide() {
	var cur sideItem
	if m.sideIdx < len(m.side) {
		cur = m.side[m.sideIdx]
	}
	m.side = []sideItem{{kind: sideOverview}, {kind: sideInbox}}
	for _, p := range m.st.Projects {
		m.side = append(m.side, sideItem{kind: sideProject, pid: p.ID})
		for _, t := range p.Threads {
			m.side = append(m.side, sideItem{kind: sideThread, pid: p.ID, tid: t.ID})
		}
	}
	m.sideIdx = 0
	for i, it := range m.side {
		if it == cur {
			m.sideIdx = i
			return
		}
	}
	// The item vanished (deleted thread or project). Fall back to its project, then overview.
	for i, it := range m.side {
		if it.kind == sideProject && it.pid == cur.pid {
			m.sideIdx = i
			return
		}
	}
}

func (m Model) scope() sideItem { return m.side[m.sideIdx] }

func (m Model) assignment(s core.Session) (store.Assignment, bool) {
	a, ok := m.st.Assign[s.Key]
	if ok && m.st.Project(a.Project) == nil {
		return a, false
	}
	return a, ok
}

func (m Model) inScope(s core.Session, sc sideItem) bool {
	a, ok := m.assignment(s)
	switch sc.kind {
	case sideInbox:
		return !ok
	case sideProject:
		return ok && a.Project == sc.pid
	case sideThread:
		return ok && a.Project == sc.pid && a.Thread == sc.tid
	}
	return true
}

func (m Model) sessionsIn(sc sideItem) []core.Session {
	var out []core.Session
	for _, s := range m.sessions {
		if m.inScope(s, sc) {
			out = append(out, s)
		}
	}
	core.SortSessions(out, m.opt.Now())
	return out
}

// visible is the session list for the current scope and filter.
func (m Model) visible() []core.Session {
	all := m.sessionsIn(m.scope())
	if m.filter == "" {
		return all
	}
	f := strings.ToLower(m.filter)
	var out []core.Session
	for _, s := range all {
		hay := strings.ToLower(s.Title() + " " + s.Pane.WindowName + " " + s.Info.Ask + " " + s.Info.Repo)
		if strings.Contains(hay, f) {
			out = append(out, s)
		}
	}
	return out
}

func (m Model) selected() (core.Session, bool) {
	v := m.visible()
	if m.sessIdx < 0 || m.sessIdx >= len(v) {
		return core.Session{}, false
	}
	return v[m.sessIdx], true
}

// syncSelection keeps the same session selected across refreshes and re-sorts.
func (m *Model) syncSelection() {
	v := m.visible()
	for i, s := range v {
		if s.Key == m.selKey {
			m.sessIdx = i
			return
		}
	}
	if m.sessIdx >= len(v) {
		m.sessIdx = len(v) - 1
	}
	if m.sessIdx < 0 {
		m.sessIdx = 0
	}
	if m.sessIdx < len(v) {
		m.selKey = v[m.sessIdx].Key
	} else {
		m.selKey = ""
	}
}

func (m *Model) selectSession(i int) {
	v := m.visible()
	if len(v) == 0 {
		m.sessIdx, m.selKey = 0, ""
		return
	}
	if i < 0 {
		i = 0
	}
	if i >= len(v) {
		i = len(v) - 1
	}
	m.sessIdx, m.selKey = i, v[i].Key
}

// setSide changes scope, resets per-scope view state, and records project views
// so "new" markers show what changed since the last visit.
func (m *Model) setSide(i int) {
	if i < 0 || i >= len(m.side) {
		return
	}
	m.sideIdx = i
	m.centerOff = 0
	m.cardID = ""
	m.filter = ""
	m.sessIdx = 0
	m.syncSelectionTo("")
	pid := m.side[i].pid
	if pid != m.viewedPID {
		m.viewedPID = pid
		if p := m.st.Project(pid); p != nil {
			m.newSince[pid] = p.LastViewed
			now := m.opt.Now()
			m.change(func(st *store.State) error {
				if p := st.Project(pid); p != nil {
					p.LastViewed = now
				}
				return nil
			})
		}
	}
}

func (m *Model) syncSelectionTo(key string) {
	m.selKey = key
	m.sessIdx = 0
	m.syncSelection()
}

func (m Model) newCount(p store.Project) int {
	if p.ID == m.viewedPID {
		return 0
	}
	var active []core.Session
	for _, s := range m.sessionsIn(sideItem{kind: sideProject, pid: p.ID}) {
		if !s.Held && !s.Offline {
			active = append(active, s)
		}
	}
	n := 0
	for _, it := range core.RecentResults(active, 20) {
		if it.At.After(p.LastViewed) {
			n++
		}
	}
	return n
}

// ---- update ----

// Update handles messages.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case sessionsMsg:
		m.refreshing = false
		m.loaded = true
		m.loadErr = msg.err
		if msg.err == nil {
			m.sessions = msg.ss
		}
		m.reload()
		core.ApplyHolds(m.sessions, m.st.Holds)
		m.syncSelection()
		m.dockGone()
		if m.wantKey != "" {
			for _, s := range m.visible() {
				if s.Key == m.wantKey {
					m.syncSelectionTo(s.Key)
					m.focus = focusSessions
					m.wantKey = ""
					break
				}
			}
		}
		return m, tea.Batch(m.maybeSummarize(), m.scanCloud())
	case screenMsg:
		m.gotScreen(msg)
		if !m.replying() || msg.gen != m.screenGen {
			return m, nil
		}
		gen := m.screenGen
		return m, tea.Tick(screenEvery, func(time.Time) tea.Msg { return screenTick{gen} })
	case screenTick:
		if !m.replying() || msg.gen != m.screenGen {
			return m, nil
		}
		return m, m.captureScreen()
	case summaryMsg:
		m.gotSummary(msg)
		return m, m.maybeSummarize()
	case cloudMsg:
		m.cloudBusy = false
		m.cloudRes = msg.res
		return m, m.maybeCheckLive(false)
	case liveMsg:
		m.gotLive(msg)
		return m, m.maybePrices()
	case priceMsg:
		m.gotPrices(msg)
		return m, nil
	case kubeMsg:
		m.gotKube(msg)
		return m, nil
	case parkPaneMsg:
		if m.opt.Park == nil {
			return m, nil
		}
		movedAny := false
		for _, pane := range msg.panes {
			if pane == "" {
				continue
			}
			if pane == m.docked {
				if m.opt.Rehome != nil {
					_ = m.opt.Rehome(pane)
				}
				continue
			}
			if moved, err := m.opt.Park(pane); err == nil && moved {
				movedAny = true
			}
		}
		if movedAny {
			return m, m.refreshNow()
		}
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{tick(), m.maybePrices(), m.maybeKube(false)}
		if !m.refreshing {
			m.refreshing = true
			cmds = append(cmds, m.refresh())
		}
		return m, tea.Batch(cmds...)
	case tea.MouseMsg:
		return m.updateMouse(msg)
	case tea.KeyMsg:
		switch m.mode {
		case modeInput:
			return m.updateInput(msg)
		case modeOrganize:
			return m.updateOrganize(msg)
		case modeConfirm:
			return m.updateConfirm(msg)
		case modeHelp:
			return m.updateHelp(msg)
		}
		return m.updateNormal(msg)
	}
	return m, nil
}

func (m *Model) stepProject(dir int) {
	var idx []int
	for i, it := range m.side {
		if it.kind == sideProject {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return
	}
	cur := m.sideIdx
	if dir > 0 {
		for _, i := range idx {
			if i > cur {
				m.setSide(i)
				return
			}
		}
		m.setSide(idx[0])
		return
	}
	for j := len(idx) - 1; j >= 0; j-- {
		if idx[j] < cur && !(m.side[cur].kind == sideThread && m.side[cur].pid == m.side[idx[j]].pid) {
			m.setSide(idx[j])
			return
		}
	}
	m.setSide(idx[len(idx)-1])
}

// stepNeedsYou cycles through every session that is blocked on the user,
// switching scope to wherever that session lives. dir is 1 or -1.
func (m *Model) stepNeedsYou(dir int) {
	now := m.opt.Now()
	var waiting []core.Session
	for _, s := range m.sessions {
		if s.State(now) == agents.NeedsYou {
			waiting = append(waiting, s)
		}
	}
	if len(waiting) == 0 {
		m.setStatus("Nothing needs you right now.")
		return
	}
	core.SortSessions(waiting, now)
	next := waiting[0]
	if dir < 0 {
		next = waiting[len(waiting)-1]
	}
	for i, s := range waiting {
		// Only step past the selection once the user is in the session list;
		// the first press from elsewhere goes to the most urgent session.
		if m.focus == focusSessions && s.Key == m.selKey {
			next = waiting[(i+dir+len(waiting))%len(waiting)]
			break
		}
	}
	target := sideItem{kind: sideInbox}
	if a, ok := m.assignment(next); ok {
		target = sideItem{kind: sideProject, pid: a.Project}
	}
	for i, it := range m.side {
		if it == target {
			if i != m.sideIdx {
				m.setSide(i)
			}
			break
		}
	}
	m.filter = ""
	m.focus = focusSessions
	m.syncSelectionTo(next.Key)
}

func (m Model) jump() (tea.Model, tea.Cmd) {
	s, ok := m.selected()
	if !ok {
		return m, nil
	}
	if s.Offline {
		return m, m.restoreOne(s, restoreJump)
	}
	if m.opt.Jump == nil {
		p := s.Pane
		m.JumpTarget = &p
		return m, tea.Quit
	}
	if err := m.opt.Jump(s.Pane); err != nil {
		m.setErr(err)
	} else {
		m.setStatus("Jumped to " + s.Pane.Target())
	}
	return m, nil
}

func (m *Model) acceptSuggestion() {
	s, ok := m.selected()
	if !ok {
		return
	}
	if _, assigned := m.assignment(s); assigned {
		m.setStatus("Already in a project. Press m to move it.")
		return
	}
	sg, ok := core.Suggest(s, m.st, m.sessions)
	if !ok {
		m.setStatus("No suggestion. Press m to choose a project.")
		return
	}
	m.assign(s, sg.Project, sg.Thread)
}

func (m *Model) assign(s core.Session, pid, tid string) {
	now := m.opt.Now()
	if !m.change(func(st *store.State) error {
		if pid != "" && st.Project(pid) == nil {
			return fmt.Errorf("that project was deleted")
		}
		st.AssignSession(s.Key, pid, tid, s.Title(), now)
		return nil
	}) {
		return
	}
	if pid == "" {
		m.setStatus(fmt.Sprintf("%q is unassigned", s.Title()))
	} else {
		m.setStatus(fmt.Sprintf("%q is now in %s", s.Title(), m.placeName(pid, tid)))
	}
	m.syncSelection()
}

func (m Model) placeName(pid, tid string) string {
	p := m.st.Project(pid)
	if p == nil {
		return "?"
	}
	if t := p.Thread(tid); t != nil {
		return p.Name + " › " + t.Name
	}
	return p.Name
}

// ---- organize ----

func (m *Model) openOrganize() {
	s, ok := m.selected()
	if !ok {
		m.setErr(fmt.Errorf("select a session to move (l to focus the session list)"))
		return
	}
	m.orgKey = s.Key
	m.orgOpts = nil
	for _, p := range m.st.Projects {
		m.orgOpts = append(m.orgOpts, orgOpt{label: p.Name, pid: p.ID})
		for _, t := range p.Threads {
			m.orgOpts = append(m.orgOpts, orgOpt{label: t.Name, pid: p.ID, tid: t.ID})
		}
	}
	m.orgOpts = append(m.orgOpts, orgOpt{label: "New project…", newProj: true})
	a, assigned := m.assignment(s)
	if assigned {
		m.orgOpts = append(m.orgOpts, orgOpt{label: "Unassign", unassign: true})
	}
	m.orgIdx = 0
	want := store.Assignment{}
	if assigned {
		want = a
	} else if sg, ok := core.Suggest(s, m.st, m.sessions); ok {
		want = store.Assignment{Project: sg.Project, Thread: sg.Thread}
	}
	for i, o := range m.orgOpts {
		if want.Project != "" && o.pid == want.Project && o.tid == want.Thread {
			m.orgIdx = i
		}
	}
	m.mode = modeOrganize
}

func (m Model) orgSession() (core.Session, bool) {
	for _, s := range m.sessions {
		if s.Key == m.orgKey {
			return s, true
		}
	}
	return core.Session{}, false
}

func (m Model) updateHelp(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.pushCount(k) {
		return m, nil
	}
	if k == "esc" && m.count > 0 {
		m.count = 0
		return m, nil
	}
	n := m.takeCount()
	switch k {
	case "j", "down", "ctrl+e":
		m.scrollHelp(n)
	case "k", "up", "ctrl+y":
		m.scrollHelp(-n)
	case "ctrl+d":
		m.scrollHelp(n * max(1, m.height/2))
	case "ctrl+u":
		m.scrollHelp(-n * max(1, m.height/2))
	case "g", "home":
		m.helpOff = 0
	case "G", "end":
		m.scrollHelp(1 << 20)
	default:
		m.mode = modeNormal
	}
	return m, nil
}

func (m Model) updateOrganize(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if m.pushCount(k) {
		return m, nil
	}
	if k == "esc" && m.count > 0 {
		m.count = 0
		return m, nil
	}
	hasCount := m.count > 0
	n := m.takeCount()
	switch k {
	case "esc", "q":
		m.mode = modeNormal
	case "j", "down":
		m.orgIdx = min(len(m.orgOpts)-1, m.orgIdx+n)
	case "k", "up":
		m.orgIdx = max(0, m.orgIdx-n)
	case "g", "home":
		if hasCount {
			m.orgIdx = max(0, min(len(m.orgOpts)-1, n-1))
		} else {
			m.orgIdx = 0
		}
	case "G", "end":
		if hasCount {
			m.orgIdx = max(0, min(len(m.orgOpts)-1, n-1))
		} else {
			m.orgIdx = max(0, len(m.orgOpts)-1)
		}
	case "n":
		m.startInput(inNewProject, "", "", m.orgKey, "")
	case "t":
		if o := m.orgOpts[m.orgIdx]; o.pid != "" {
			m.startInput(inNewThread, o.pid, "", m.orgKey, "")
		}
	case "enter":
		o := m.orgOpts[m.orgIdx]
		if o.newProj {
			m.startInput(inNewProject, "", "", m.orgKey, "")
			return m, nil
		}
		m.mode = modeNormal
		if s, ok := m.orgSession(); ok {
			m.assign(s, o.pid, o.tid)
		}
	}
	return m, nil
}

// ---- text input ----

func (m *Model) startInput(k inputKind, pid, tid, key, value string) {
	m.mode = modeInput
	m.inputKind, m.inputPID, m.inputTID, m.inputKey = k, pid, tid, key
	m.input.SetValue(value)
	m.input.CursorEnd()
	m.input.Focus()
}

func (m Model) inputPrompt() string {
	switch m.inputKind {
	case inNewProject:
		return "New project name"
	case inNewThread:
		return "New thread in " + m.placeName(m.inputPID, "")
	case inPurpose:
		return "What is " + m.placeName(m.inputPID, "") + " for"
	case inRename:
		return "Rename " + m.placeName(m.inputPID, m.inputTID)
	case inFilter:
		return "Filter sessions"
	case inCommand:
		return ":"
	case inLaunchDir:
		if m.inputPID == "" {
			return "Start Copilot in folder"
		}
		return "Start Copilot for " + m.placeName(m.inputPID, m.inputTID) + " in folder"
	case inLaunchTask:
		return "First task (optional, enter to skip)"
	case inReply:
		return "Type to " + m.replyTitle
	case inHoldReason:
		label := m.holdPromptLabel()
		return "Hold note for " + label + " (optional, enter to save)"
	}
	return ""
}

func (m Model) updateInput(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.inputKind == inFilter {
			m.filter = ""
			m.syncSelection()
		}
		m.mode = modeNormal
		m.input.Blur()
		return m, nil
	case "enter":
		if m.inputKind == inReply {
			m.sendReply(strings.TrimSpace(m.input.Value()))
			m.input.SetValue("")
			return m, nil
		}
		m.mode = modeNormal
		m.input.Blur()
		if m.inputKind == inCommand {
			return m.runCommand(m.input.Value())
		}
		cmd := m.commitInput(strings.TrimSpace(m.input.Value()))
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.inputKind == inFilter {
		m.filter = m.input.Value()
		m.syncSelection()
	}
	return m, cmd
}

func (m *Model) commitInput(v string) tea.Cmd {
	now := m.opt.Now()
	var cmd tea.Cmd
	switch m.inputKind {
	case inFilter:
		m.filter = v
		m.syncSelection()
		return nil
	case inNewProject:
		m.commitNewProject(v, m.inputKey)
	case inLaunchDir:
		m.commitLaunchDir(v)
		return nil
	case inLaunchTask:
		cmd = m.launch(m.inputKey, v)
	case inNewThread:
		var tid string
		pid := m.inputPID
		s, hasSess := m.sessionByKey(m.inputKey)
		if !m.change(func(st *store.State) error {
			t, err := st.AddThread(pid, v)
			if err != nil {
				return err
			}
			tid = t.ID
			if hasSess {
				st.AssignSession(s.Key, pid, tid, s.Title(), now)
			}
			return nil
		}) {
			return nil
		}
		m.rebuildSide()
		if m.inputKey == "" {
			m.selectSide(sideItem{kind: sideThread, pid: m.inputPID, tid: tid})
		}
		m.setStatus("Created thread " + v)
	case inPurpose:
		pid := m.inputPID
		m.change(func(st *store.State) error {
			if p := st.Project(pid); p != nil {
				p.Purpose = v
			}
			return nil
		})
	case inRename:
		if v == "" {
			m.setErr(fmt.Errorf("name is empty"))
			return nil
		}
		pid, tid := m.inputPID, m.inputTID
		m.change(func(st *store.State) error {
			if p := st.Project(pid); p != nil {
				if t := p.Thread(tid); t != nil {
					t.Name = v
				} else {
					p.Name = v
				}
			}
			return nil
		})
		m.rebuildSide()
	case inHoldReason:
		m.commitHoldNote(v)
	}
	m.syncSelection()
	return cmd
}

// ---- relative line numbers ----

func (m Model) showRelativeNumbers() bool {
	return m.st == nil || !m.st.NoRelativeNumbers
}

func (m *Model) setRelativeNumbers(on bool) {
	wantOff := !on
	m.change(func(st *store.State) error {
		st.NoRelativeNumbers = wantOff
		return nil
	})
	if on {
		m.setStatus("Relative line numbers on (space # or :nornu to hide)")
	} else {
		m.setStatus("Relative line numbers off (space # or :rnu to show)")
	}
}

func (m *Model) toggleRelativeNumbers() {
	m.setRelativeNumbers(!m.showRelativeNumbers())
}

// ---- holds ----

type holdTarget struct {
	label    string
	pid, tid string
	key      string
	sessions []core.Session
}

func (m Model) holdTargetFor() (holdTarget, bool) {
	sc := m.scope()
	if m.focus == focusCenter {
		switch {
		case strings.HasPrefix(m.cardID, "proj:"):
			pid := strings.TrimPrefix(m.cardID, "proj:")
			if m.st.Project(pid) != nil {
				return holdTarget{label: m.placeName(pid, ""), pid: pid, sessions: m.sessionsIn(sideItem{kind: sideProject, pid: pid})}, true
			}
		case strings.HasPrefix(m.cardID, "thread:"):
			tid := strings.TrimPrefix(m.cardID, "thread:")
			if p := m.st.Project(sc.pid); p != nil && p.Thread(tid) != nil {
				return holdTarget{label: m.placeName(sc.pid, tid), pid: sc.pid, tid: tid, sessions: m.sessionsIn(sideItem{kind: sideThread, pid: sc.pid, tid: tid})}, true
			}
		case strings.HasPrefix(m.cardID, "sess:"), strings.HasPrefix(m.cardID, "needs:"):
			key := strings.TrimPrefix(strings.TrimPrefix(m.cardID, "sess:"), "needs:")
			if s, ok := m.sessionByKey(key); ok {
				return holdTarget{label: s.Title(), key: s.Key, sessions: []core.Session{s}}, true
			}
		}
	}
	if m.focus == focusSide || (m.focus == focusCenter && (sc.kind == sideProject || sc.kind == sideThread)) {
		if sc.kind == sideProject || sc.kind == sideThread {
			return holdTarget{label: m.placeName(sc.pid, sc.tid), pid: sc.pid, tid: sc.tid, sessions: m.sessionsIn(sc)}, true
		}
	}
	if s, ok := m.selected(); ok {
		return holdTarget{label: s.Title(), key: s.Key, sessions: []core.Session{s}}, true
	}
	return holdTarget{}, false
}

func (m Model) holdPromptLabel() string {
	if m.inputKey != "" {
		if s, ok := m.sessionByKey(m.inputKey); ok {
			return s.Title()
		}
	}
	if m.inputPID != "" {
		return m.placeName(m.inputPID, m.inputTID)
	}
	return "selection"
}

func (m Model) holdTargetFromInput() (holdTarget, bool) {
	if m.inputKey != "" {
		if s, ok := m.sessionByKey(m.inputKey); ok {
			return holdTarget{label: s.Title(), key: s.Key, sessions: []core.Session{s}}, true
		}
		return holdTarget{}, false
	}
	if m.inputPID != "" {
		sc := sideItem{kind: sideProject, pid: m.inputPID}
		if m.inputTID != "" {
			sc = sideItem{kind: sideThread, pid: m.inputPID, tid: m.inputTID}
		}
		return holdTarget{label: m.placeName(m.inputPID, m.inputTID), pid: m.inputPID, tid: m.inputTID, sessions: m.sessionsIn(sc)}, true
	}
	return holdTarget{}, false
}

func (m Model) partitionHold(ss []core.Session) (holdable, held, active []core.Session) {
	for _, s := range ss {
		if s.Offline {
			if s.Held {
				held = append(held, s)
			} else {
				holdable = append(holdable, s)
			}
			continue
		}
		switch s.Info.State {
		case agents.Working, agents.NeedsYou:
			active = append(active, s)
		case agents.Ready, agents.Quiet:
			if s.Held {
				held = append(held, s)
			} else {
				holdable = append(holdable, s)
			}
		}
	}
	return holdable, held, active
}

func holdStamp(s core.Session, now time.Time) time.Time {
	if s.Info.UpdatedAt.After(now) {
		return s.Info.UpdatedAt
	}
	return now
}

// pruneWokenHolds removes persisted holds whose live sessions have already woken up.
func (m Model) pruneWokenHolds(st *store.State) {
	for _, s := range m.sessions {
		if s.Offline {
			continue
		}
		h, ok := st.Holds[s.Key]
		if !ok {
			continue
		}
		if s.Info.State == agents.NeedsYou || s.Info.State == agents.Working || (!s.Info.UpdatedAt.IsZero() && s.Info.UpdatedAt.After(h.At)) {
			st.ClearHold(s.Key)
		}
	}
}

func (m *Model) toggleHold() {
	tgt, ok := m.holdTargetFor()
	if !ok {
		m.setStatus("Select a project, thread, or session first.")
		return
	}
	holdable, held, active := m.partitionHold(tgt.sessions)
	now := m.opt.Now()
	switch {
	case len(holdable) > 0:
		if !m.change(func(st *store.State) error {
			m.pruneWokenHolds(st)
			for _, s := range holdable {
				st.SetHold(s.Key, holdStamp(s, now), "")
			}
			return nil
		}) {
			return
		}
		if len(holdable) == 1 && tgt.key != "" {
			m.setStatus(fmt.Sprintf("On hold: %s (X to add a note, x to resume)", holdable[0].Title()))
		} else {
			msg := fmt.Sprintf("Put %s on hold in %s (X to add a note)", plural(len(holdable), "session"), tgt.label)
			if len(active) > 0 {
				msg += fmt.Sprintf(" · %d still active", len(active))
			}
			m.setStatus(msg)
		}
		m.syncSelection()
	case len(held) > 0:
		if !m.change(func(st *store.State) error {
			m.pruneWokenHolds(st)
			for _, s := range held {
				st.ClearHold(s.Key)
			}
			return nil
		}) {
			return
		}
		if len(held) == 1 && tgt.key != "" {
			m.setStatus("Resumed " + held[0].Title())
		} else {
			m.setStatus(fmt.Sprintf("Resumed %s in %s", plural(len(held), "session"), tgt.label))
		}
		m.syncSelection()
	case len(active) > 0:
		m.setStatus("Still active - you can put it on hold once it finishes.")
	default:
		m.setStatus("No finished sessions here to put on hold.")
	}
}

func (m *Model) startHoldNote() {
	tgt, ok := m.holdTargetFor()
	if !ok {
		m.setStatus("Select a project, thread, or session first.")
		return
	}
	holdable, held, active := m.partitionHold(tgt.sessions)
	eligible := append(holdable, held...)
	if len(eligible) == 0 {
		if len(active) > 0 {
			m.setStatus("Still active - you can put it on hold once it finishes.")
		} else {
			m.setStatus("No finished sessions here to put on hold.")
		}
		return
	}
	existing := ""
	for _, s := range held {
		if s.HoldReason != "" {
			existing = s.HoldReason
			break
		}
	}
	m.startInput(inHoldReason, tgt.pid, tgt.tid, tgt.key, existing)
}

func (m *Model) holdWithReason(reason string) {
	tgt, ok := m.holdTargetFor()
	if !ok {
		m.setStatus("Select a project, thread, or session first.")
		return
	}
	m.applyHoldTarget(tgt, reason)
}

func (m *Model) commitHoldNote(reason string) {
	tgt, ok := m.holdTargetFromInput()
	if !ok {
		return
	}
	m.applyHoldTarget(tgt, reason)
}

func (m *Model) applyHoldTarget(tgt holdTarget, reason string) {
	holdable, held, active := m.partitionHold(tgt.sessions)
	eligible := append(holdable, held...)
	if len(eligible) == 0 {
		if len(active) > 0 {
			m.setStatus("Still active - you can put it on hold once it finishes.")
		} else {
			m.setStatus("No finished sessions here to put on hold.")
		}
		return
	}
	now := m.opt.Now()
	reason = strings.TrimSpace(reason)
	if !m.change(func(st *store.State) error {
		m.pruneWokenHolds(st)
		for _, s := range eligible {
			st.SetHold(s.Key, holdStamp(s, now), reason)
		}
		return nil
	}) {
		return
	}
	if reason != "" {
		m.setStatus(fmt.Sprintf("On hold (%s): %s", reason, tgt.label))
	} else {
		m.setStatus(fmt.Sprintf("On hold: %s (x to resume)", tgt.label))
	}
}

func (m *Model) resumeHold() {
	tgt, ok := m.holdTargetFor()
	if !ok {
		m.setStatus("Select a project, thread, or session first.")
		return
	}
	_, held, _ := m.partitionHold(tgt.sessions)
	if len(held) == 0 {
		m.setStatus("Nothing is on hold here.")
		return
	}
	if !m.change(func(st *store.State) error {
		m.pruneWokenHolds(st)
		for _, s := range held {
			st.ClearHold(s.Key)
		}
		return nil
	}) {
		return
	}
	if len(held) == 1 && tgt.key != "" {
		m.setStatus("Resumed " + held[0].Title())
	} else {
		m.setStatus(fmt.Sprintf("Resumed %s in %s", plural(len(held), "session"), tgt.label))
	}
	m.syncSelection()
}

func (m Model) sessionByKey(key string) (core.Session, bool) {
	if key == "" {
		return core.Session{}, false
	}
	for _, s := range m.sessions {
		if s.Key == key {
			return s, true
		}
	}
	return core.Session{}, false
}

func (m *Model) commitNewProject(name, sessKey string) {
	now := m.opt.Now()
	var pid string
	s, hasSess := m.sessionByKey(sessKey)
	if !m.change(func(st *store.State) error {
		p, err := st.AddProject(name, now)
		if err != nil {
			return err
		}
		pid = p.ID
		if hasSess {
			st.AssignSession(s.Key, pid, "", s.Title(), now)
		}
		return nil
	}) {
		return
	}
	m.rebuildSide()
	m.selectSide(sideItem{kind: sideProject, pid: pid})
	m.setStatus("Created " + name + ". Press e to say what it is for.")
}

func (m *Model) selectSide(it sideItem) {
	for i, x := range m.side {
		if x == it {
			m.setSide(i)
			return
		}
	}
}

func (m *Model) startRename() {
	sc := m.scope()
	if sc.pid == "" {
		m.setErr(fmt.Errorf("select a project or thread to rename"))
		return
	}
	p := m.st.Project(sc.pid)
	if p == nil {
		return
	}
	name := p.Name
	if t := p.Thread(sc.tid); t != nil {
		name = t.Name
	}
	m.startInput(inRename, sc.pid, sc.tid, "", name)
}

// ---- confirm ----

func (m *Model) startDelete() {
	if s, ok := m.focusedOffline(); ok {
		m.dismissOffline(s)
		return
	}
	sc := m.scope()
	if sc.pid == "" {
		m.setErr(fmt.Errorf("select a project or thread to delete"))
		return
	}
	name := m.placeName(sc.pid, sc.tid)
	if sc.kind == sideThread {
		m.confirmText = fmt.Sprintf("Delete thread %s? Its sessions stay in the project.", name)
		m.confirmDo = func(st *store.State) error { st.DeleteThread(sc.pid, sc.tid); return nil }
	} else {
		m.confirmText = fmt.Sprintf("Delete project %s? Its sessions become unassigned.", name)
		m.confirmDo = func(st *store.State) error { st.DeleteProject(sc.pid); return nil }
	}
	m.mode = modeConfirm
}

func (m Model) updateConfirm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.mode = modeNormal
	if msg.String() != "y" {
		m.setStatus("Cancelled")
		return m, nil
	}
	if !m.change(m.confirmDo) {
		return m, nil
	}
	m.rebuildSide()
	m.setSide(m.sideIdx)
	m.setStatus("Deleted")
	return m, nil
}

// updateMouse handles the wheel and left clicks across the three panes.
func (m Model) updateMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	if m.mode == modeHelp {
		switch msg.Button {
		case tea.MouseButtonWheelDown:
			m.scrollHelp(3)
		case tea.MouseButtonWheelUp:
			m.scrollHelp(-3)
		}
		return m, nil
	}
	if m.mode != modeNormal {
		return m, nil
	}
	l := m.layout()
	if msg.Button == tea.MouseButtonLeft {
		ry := msg.Y - 1
		if ry < 0 || ry >= l.bodyH {
			return m, nil
		}
		switch {
		case l.sideW > 0 && msg.X < l.sideW:
			m.focus = focusSide
			if idx, ok := m.sideItemAt(l.sideW, l.bodyH, ry); ok {
				m.setSide(idx)
			}
		case l.wide && msg.X > l.sideW+l.centerW:
			m.focus = focusSessions
			if idx, ok := m.sessionAt(rightW-2, l.bodyH, ry); ok {
				m.selectSession(idx)
			}
		default:
			m.focus = focusCenter
			if c, ok := m.cardAt(l, ry); ok {
				m.highlight(c)
			} else {
				m.ensureCard()
			}
		}
		return m, nil
	}
	step := 0
	switch msg.Button {
	case tea.MouseButtonWheelDown:
		step = 1
	case tea.MouseButtonWheelUp:
		step = -1
	default:
		return m, nil
	}
	switch {
	case l.sideW > 0 && msg.X < l.sideW:
		m.setSide(m.sideIdx + step)
	case l.wide && msg.X > l.sideW+l.centerW:
		m.selectSession(m.sessIdx + step)
	default:
		m.scrollCenter(3 * step)
	}
	return m, nil
}

func (m *Model) scrollHelp(n int) {
	l := m.layout()
	lines := m.viewHelp(l.centerW - 4)
	m.helpOff = clampOff(clampOff(m.helpOff, len(lines), l.bodyH)+n, len(lines), l.bodyH)
}
