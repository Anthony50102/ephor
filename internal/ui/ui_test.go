package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/tmux"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

var now = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func session(key, name string, st agents.State, age time.Duration, win int) core.Session {
	return core.Session{
		Key:  key,
		Kind: "copilot",
		Pane: tmux.Pane{SessionID: "$0", SessionName: "0", WindowID: "@" + key, WindowIndex: win, WindowName: name, PaneID: "%" + key},
		Info: agents.Info{Name: name, State: st, UpdatedAt: now.Add(-age), Ask: "ask for " + name},
	}
}

type harness struct {
	t      *testing.T
	m      Model
	jumped []tmux.Pane
	path   string
}

func newHarness(t *testing.T, ss []core.Session, inTmux bool) *harness {
	h := &harness{t: t, path: filepath.Join(t.TempDir(), "state.json")}
	st, _ := store.Load(h.path)
	opt := Options{Store: st, StatePath: h.path, Now: func() time.Time { return now }, NoTick: true,
		Discover: func() ([]core.Session, error) { return ss, nil }}
	if inTmux {
		opt.Jump = func(p tmux.Pane) error { h.jumped = append(h.jumped, p); return nil }
	}
	h.m = New(opt)
	h.send(tea.WindowSizeMsg{Width: 160, Height: 50})
	h.send(sessionsMsg{ss: ss})
	return h
}

// seedProject writes a project holding one session to the state file, as
// another Switchboard instance would, and lets the model pick it up.
func (h *harness) seedProject(name, key string, viewed time.Time) *store.Project {
	var pid string
	_, err := store.Update(h.path, func(st *store.State) error {
		p, err := st.AddProject(name, now)
		if err != nil {
			return err
		}
		pid, p.LastViewed = p.ID, viewed
		st.AssignSession(key, pid, "", key, now)
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.m.stamp = ""
	h.m.reload()
	return h.m.st.Project(pid)
}

func (h *harness) send(msg tea.Msg) tea.Cmd {
	next, cmd := h.m.Update(msg)
	h.m = next.(Model)
	return cmd
}

func (h *harness) keys(ks ...string) {
	for _, k := range ks {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		case "ctrl+h":
			msg = tea.KeyMsg{Type: tea.KeyCtrlH}
		case "ctrl+l":
			msg = tea.KeyMsg{Type: tea.KeyCtrlL}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		h.send(msg)
	}
}

func (h *harness) typeText(s string) {
	for _, r := range s {
		h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func (h *harness) screen() string { return ansi.Strip(h.m.View()) }

func (h *harness) mustSee(parts ...string) {
	h.t.Helper()
	s := h.screen()
	for _, p := range parts {
		if !strings.Contains(s, p) {
			h.t.Fatalf("screen is missing %q:\n%s", p, s)
		}
	}
}

func TestCreateProjectAndOrganize(t *testing.T) {
	ss := []core.Session{session("a", "billing fix", agents.Working, time.Minute, 1), session("b", "docs", agents.Ready, time.Hour, 2)}
	h := newHarness(t, ss, true)
	h.mustSee("1 working", "2 unassigned", "No projects yet")

	h.keys("n")
	h.typeText("Billing")
	h.keys("enter", "e")
	h.typeText("Charge customers correctly.")
	h.keys("enter")
	if h.m.scope().kind != sideProject {
		t.Fatal("creating a project should select it")
	}
	h.mustSee("Billing", "Charge customers correctly.", "no live sessions")

	// Unassigned shows a suggestion from the name, and `a` accepts it.
	h.keys("h", "g", "g", "j")
	h.mustSee("2 sessions to sort", "→ Billing (name matches project)")
	h.keys("tab", "a")
	st, _ := store.Load(h.path)
	if a, ok := st.Assign["a"]; !ok || st.Project(a.Project).Name != "Billing" {
		t.Fatalf("accept did not persist: %+v", st.Assign)
	}

	// Organize the other one into a new thread from the dialog.
	h.keys("m")
	if h.m.mode != modeOrganize {
		t.Fatal("m should open the organize dialog")
	}
	h.keys("t")
	h.typeText("Docs")
	h.keys("enter")
	st, _ = store.Load(h.path)
	p := st.Project(st.Assign["b"].Project)
	if p == nil || p.Thread(st.Assign["b"].Thread) == nil || p.Thread(st.Assign["b"].Thread).Name != "Docs" {
		t.Fatalf("thread assignment did not persist: %+v", st)
	}
	h.mustSee("Everything is sorted")

	h.keys("h", "L")
	h.mustSee("HAPPENING NOW", "billing fix", "ask for billing fix", "THREADS", "Docs")
}

func TestNextNeedsYouCyclesAcrossScopesAndJumps(t *testing.T) {
	ss := []core.Session{
		session("a", "alpha", agents.NeedsYou, time.Minute, 1),
		session("b", "beta", agents.Working, time.Minute, 2),
		session("c", "gamma", agents.NeedsYou, 2*time.Minute, 3),
	}
	ss[0].Info.Reason = "Question: Which region?"
	h := newHarness(t, ss, true)
	p := h.seedProject("Infra", "c", time.Time{})
	h.mustSee("2 needs you", "NEEDS YOU", "Question: Which region?")

	h.keys("]", "d")
	if h.m.scope().kind != sideInbox || h.m.selKey != "a" || h.m.focus != focusSessions {
		t.Fatalf("first n should select alpha in Unassigned: %+v %s", h.m.scope(), h.m.selKey)
	}
	h.keys("]", "d")
	if h.m.scope().pid != p.ID || h.m.selKey != "c" {
		t.Fatalf("second n should move to Infra/gamma: %+v %s", h.m.scope(), h.m.selKey)
	}
	h.keys("enter")
	if len(h.jumped) != 1 || h.jumped[0].WindowID != "@c" {
		t.Fatalf("enter should jump to gamma: %+v", h.jumped)
	}
	h.mustSee("Jumped to 0:3 gamma")
}

func TestJumpOutsideTmuxQuitsWithTarget(t *testing.T) {
	h := newHarness(t, []core.Session{session("a", "alpha", agents.Working, time.Minute, 4)}, false)
	h.keys("tab")
	cmd := h.send(tea.KeyMsg{Type: tea.KeyEnter})
	if h.m.JumpTarget == nil || h.m.JumpTarget.WindowIndex != 4 || cmd == nil {
		t.Fatal("outside tmux, enter should quit and hand back the pane to attach")
	}
}

func TestNewSinceLastVisit(t *testing.T) {
	s := session("a", "alpha", agents.Ready, time.Minute, 1)
	s.Info.Results = []agents.Result{{At: now.Add(-time.Minute), Text: "Shipped the fix."}, {At: now.Add(-48 * time.Hour), Text: "Old result."}}
	h := newHarness(t, []core.Session{s}, true)
	pid := h.seedProject("Alpha", "a", now.Add(-time.Hour)).ID
	h.mustSee("1 new") // sidebar badge before opening

	h.keys("L")
	h.mustSee("RECENTLY FINISHED", "1 new since you last looked", "new Shipped the fix.")
	if !h.m.st.Project(pid).LastViewed.Equal(now) {
		t.Fatal("opening a project should record the visit")
	}
	h.keys("g", "g", "L") // leave for the overview, then come back
	if strings.Contains(h.screen(), "new since you last looked") {
		t.Fatal("new markers should clear after a visit")
	}
}

func TestFilterAndDelete(t *testing.T) {
	ss := []core.Session{session("a", "alpha", agents.Working, time.Minute, 1), session("b", "beta", agents.Working, time.Minute, 2)}
	h := newHarness(t, ss, true)
	h.keys("/")
	h.typeText("bet")
	if v := h.m.visible(); len(v) != 1 || v[0].Key != "b" {
		t.Fatalf("filter: %+v", v)
	}
	h.keys("esc")
	if len(h.m.visible()) != 2 {
		t.Fatal("esc should clear the filter")
	}

	pid := h.seedProject("Doomed", "a", time.Time{}).ID
	h.keys("L", "d")
	h.mustSee("Delete project Doomed?")
	h.keys("y")
	if h.m.st.Project(pid) != nil || len(h.m.st.Assign) != 0 || h.m.scope().kind != sideOverview {
		t.Fatal("delete should remove the project, unassign sessions, and return to the overview")
	}
}

func TestRendersAtManySizes(t *testing.T) {
	ss := []core.Session{session("a", "a very long session name that will not fit anywhere at all", agents.NeedsYou, time.Minute, 1)}
	ss[0].Info.Reason = strings.Repeat("long reason ", 40)
	h := newHarness(t, ss, true)
	for _, size := range [][2]int{{60, 12}, {90, 30}, {123, 40}, {124, 40}, {240, 70}} {
		h.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, k := range []string{"space", "?", "x", "]", "tab", "m", "esc"} {
			out := h.m.View()
			lines := strings.Split(out, "\n")
			if len(lines) != size[1] {
				t.Fatalf("%v after %q: %d lines", size, k, len(lines))
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Fatalf("%v after %q: line %d is %d wide", size, k, i, w)
				}
			}
			h.keys(k)
		}
	}
}

func TestOtherInstanceEditsSurviveNavigation(t *testing.T) {
	h := newHarness(t, []core.Session{session("a", "alpha", agents.Ready, time.Minute, 1)}, true)
	// Another instance adds a project after this one loaded the file.
	if _, err := store.Update(h.path, func(st *store.State) error { _, err := st.AddProject("Theirs", now); return err }); err != nil {
		t.Fatal(err)
	}
	h.keys("n")
	h.typeText("Mine")
	h.keys("enter")
	st, _ := store.Load(h.path)
	if len(st.Projects) != 2 {
		t.Fatalf("lost the other instance's project: %+v", st.Projects)
	}
	h.mustSee("Theirs", "Mine")
}

func TestVimKeys(t *testing.T) {
	ss := []core.Session{
		session("a", "alpha", agents.NeedsYou, time.Minute, 1),
		session("b", "beta", agents.NeedsYou, 2*time.Minute, 2),
		session("c", "gamma", agents.Working, 3*time.Minute, 3),
	}
	h := newHarness(t, ss, true)

	h.keys("space")
	h.mustSee("<leader>", "find a session", "new project")
	h.keys("n")
	if h.m.mode != modeInput || h.m.inputKind != inNewProject {
		t.Fatal("<leader>n should start a new project")
	}
	h.keys("esc")

	h.keys("[", "d")
	if h.m.selKey != "b" {
		t.Fatalf("[d from outside the list should pick the last waiting session, got %q", h.m.selKey)
	}
	h.keys("[", "d")
	if h.m.selKey != "a" {
		t.Fatalf("[d should step back, got %q", h.m.selKey)
	}

	h.keys("G")
	if h.m.selKey != "c" {
		t.Fatalf("G should select the last session, got %q", h.m.selKey)
	}
	h.keys("g", "g")
	if h.m.selKey != "a" {
		t.Fatalf("gg should select the first session, got %q", h.m.selKey)
	}
	h.keys("ctrl+h")
	if h.m.focus != focusSide {
		t.Fatal("ctrl-h should focus the projects column")
	}
	h.keys("ctrl+l")
	if h.m.focus != focusSessions {
		t.Fatal("ctrl-l should focus the sessions column")
	}

	h.keys("]", "x")
	h.mustSee("]x is not mapped")

	h.keys(":")
	h.typeText("new Infra")
	h.keys("enter")
	if h.m.scope().kind != sideProject || h.m.placeName(h.m.scope().pid, "") != "Infra" {
		t.Fatal(":new should create and open a project")
	}
	h.keys(":")
	h.typeText("q")
	next, cmd := h.m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	h.m = next.(Model)
	if cmd == nil {
		t.Fatal(":q should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal(":q should quit")
	}
}
