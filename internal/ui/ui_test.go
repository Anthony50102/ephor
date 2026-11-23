package ui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/cloud"
	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/summary"
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
	cmd    tea.Cmd // last non-nil command from an Update
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
	if cmd != nil {
		h.cmd = cmd
	}
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
		case "ctrl+d":
			msg = tea.KeyMsg{Type: tea.KeyCtrlD}
		case "ctrl+u":
			msg = tea.KeyMsg{Type: tea.KeyCtrlU}
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
	h.mustSee("HAPPENING NOW", "billing fix", "ask for billing fix", "WORK IN MOTION", "Docs")
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
	h.mustSee("Since you last looked", "new Shipped the fix.")
	if !h.m.st.Project(pid).LastViewed.Equal(now) {
		t.Fatal("opening a project should record the visit")
	}
	h.keys("g", "g", "L") // leave for the overview, then come back
	if strings.Contains(h.screen(), "Since you last looked") || !strings.Contains(h.screen(), "Recently finished") {
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

func TestScrollMiddlePane(t *testing.T) {
	var ss []core.Session
	for i := 0; i < 15; i++ {
		ss = append(ss, session(fmt.Sprint("s", i), fmt.Sprint("worker", i), agents.Working, time.Minute, i))
	}
	h := newHarness(t, ss, true)
	h.send(tea.WindowSizeMsg{Width: 160, Height: 20})
	h.mustSee("more · ctrl-d or wheel")
	lines, _ := h.m.centerLines(h.m.layout())
	end := len(lines) - h.m.layout().bodyH + 1

	for i := 0; i < 20; i++ {
		h.keys("ctrl+d")
	}
	if h.m.centerOff != end {
		t.Fatalf("scrolling past the end should stop at %d, got %d", end, h.m.centerOff)
	}
	h.mustSee("more · ctrl-u")
	if strings.Contains(h.screen(), "ctrl-d or wheel") {
		t.Fatal("at the end there is nothing more below")
	}
	h.keys("ctrl+u")
	if h.m.centerOff != end-10 {
		t.Fatalf("one ctrl-u from the end should move half a page, got %d", h.m.centerOff)
	}
	h.send(tea.MouseMsg{X: 60, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if h.m.centerOff != end-13 {
		t.Fatalf("the wheel over the middle pane should scroll it, got %d", h.m.centerOff)
	}
	side := h.m.sideIdx
	h.send(tea.MouseMsg{X: 5, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if h.m.sideIdx != side+1 {
		t.Fatal("the wheel over the sidebar should move its selection")
	}
}

func TestNeedsYouCard(t *testing.T) {
	s := session("a", "alpha", agents.NeedsYou, time.Minute, 1)
	s.Info.Reason = "Pick a retry policy."
	h := newHarness(t, []core.Session{s}, true)
	h.seedProject("Alpha", "a", now)
	h.keys("L")
	h.mustSee("NEEDS YOU", "Pick a retry policy.", "]d selects it")
	if strings.Contains(h.screen(), "Nothing is running") {
		t.Fatal("an empty happening card should give way to the needs-you card")
	}
}

func TestSummaries(t *testing.T) {
	ss := []core.Session{session("k1", "worker", agents.Working, time.Minute, 1)}
	h := newHarness(t, ss, true)
	calls, fail := 0, false
	h.m.opt.Summarize = func(_ context.Context, prompt string) (string, error) {
		calls++
		if fail {
			return "", errors.New("offline")
		}
		n := calls
		return fmt.Sprintf(`{"headline":"Release %d is going out","now":"The team is shipping release %d.","next":"Tag the build once tests pass.","purpose":"Ship releases","sessions":{"S1":"Cutting the build"}}`, n, n), nil
	}
	h.m.opt.SummaryPath = filepath.Join(t.TempDir(), "summaries.json")
	p := h.seedProject("app", "k1", now)

	run := func(cmd tea.Cmd) {
		t.Helper()
		if cmd == nil {
			t.Fatal("expected a summary to start")
		}
		h.send(cmd())
	}
	run(h.m.maybeSummarize())
	if h.m.maybeSummarize() != nil {
		t.Fatal("nothing changed, so nothing should be rewritten")
	}
	h.keys("L")
	h.mustSee("Release 1 is going out", "shipping release 1", "NEXT UP", "Tag the build once tests pass.", "WORK IN MOTION", "Cutting the build", "Probably: Ship releases")
	if got := summary.LoadCache(h.m.opt.SummaryPath).Projects[p.ID].Now; got != "The team is shipping release 1." {
		t.Fatalf("cache = %q", got)
	}

	// A failure keeps the last good text and backs off.
	fail = true
	h.cmd = nil
	h.keys("space", "s")
	run(h.cmd)
	h.mustSee("shipping release 1", "rewrite failed")
	if got := h.m.sums.Projects[p.ID]; got.Tried.IsZero() || !got.At.Equal(now) {
		t.Fatalf("failure must not move the text's timestamp: %+v", got)
	}
	if h.m.sums.Projects[p.ID].Err == "" || h.m.maybeSummarize() != nil {
		t.Fatal("a failed summary must be recorded and not retried at once")
	}

	fail = false
	h.cmd = nil
	h.keys("space", "s")
	run(h.cmd)
	h.mustSee("shipping release 3")
}

func TestCloudSection(t *testing.T) {
	s := session("k1", "trainer", agents.Working, time.Minute, 1)
	s.Info.Log = "/logs/k1"
	h := newHarness(t, []core.Session{s}, true)
	h.seedProject("ml", "k1", now)
	h.send(cloudMsg{res: map[string][]cloud.Resource{"/logs/k1": {
		{Kind: "VM", Name: "gpu-box", Scope: "rg-ml", Created: true, Last: now.Add(-time.Hour), Count: 1},
		{Kind: "Kubernetes", Name: "aks1", Scope: "training", Last: now.Add(-2 * time.Hour), Count: 3},
		{Kind: "Kubernetes job", Name: "sft", Scope: "aks1/training", Last: now.Add(-2 * time.Hour), Count: 1},
		{Kind: "VM", Name: "old-box", Created: true, Deleted: true, Last: now.Add(-3 * time.Hour), Count: 2},
		{Kind: "VM", Name: "spare", Scope: "rg-ml", Last: now.Add(-4 * time.Hour), Count: 1},
		{Kind: "Azure network vnet", Name: "v", Created: true, Last: now, Count: 1},
	}}})
	h.keys("L")
	h.mustSee("CLOUD", "gpu-box", "rg-ml", "created 1h", "1 job, latest sft · ns training", "deleted 3h", "+ 1 network resource")
	h.keys("l")
	h.mustSee("VM gpu-box", "k8s aks1")

	in := h.m.summaryInput(h.m.scope().pid)
	if len(in.Cloud) != 3 || in.Cloud[0] != "VM gpu-box (rg-ml), created by an agent" {
		t.Fatalf("summary input cloud = %q", in.Cloud)
	}

	// Azure says the VM still runs, in another group, and the deleted one
	// survived. Both rise to the top.
	asked := ""
	h.m.opt.CheckLive = func(_ context.Context, q string) ([]byte, error) {
		asked = q
		return []byte(`{"data":[
			{"name":"gpu-box","type":"Microsoft.Compute/virtualMachines","resourceGroup":"rg-real","vmSize":"Standard_NC24ads_A100_v4","power":"PowerState/running"},
			{"name":"old-box","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-ml","vmSize":"Standard_D4s_v5","power":"PowerState/deallocated"},
			{"name":"spare","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-ml","vmSize":"Standard_D4s_v5","power":"PowerState/deallocated"}]}`), nil
	}
	h.keys("H")
	h.cmd = nil
	h.keys("space", "c")
	if h.cmd == nil || !strings.Contains(h.cmd().(liveMsg).keys, "gpu-box") {
		t.Fatal("space c should check with Azure")
	}
	h.keys("space", "c")
	h.mustSee("Already checking")
	h.m.liveBusy = false
	h.cmd = nil
	h.keys("space", "c")
	h.send(h.cmd())
	if !strings.Contains(asked, "'gpu-box'") || strings.Contains(asked, "'aks1'") == false {
		t.Fatalf("query = %s", asked)
	}
	h.mustSee("checked with Azure", "running · created 1h", "NC24ads_A100_v4 · rg-real", "delete failed? · deleted 3h")
	// Rows group by what they cost: running, then stopped, then the rest.
	scr := ansi.Strip(strings.Join(h.m.cloudSection(h.m.sessions, 100, 6, false), "\n"))
	billing, stopped, used := strings.Index(scr, "Running, costing money"), strings.Index(scr, "○ Stopped"), strings.Index(scr, "Used recently")
	if billing < 0 || stopped < billing || used < stopped || strings.Index(scr, "old-box") > stopped || strings.Index(scr, "spare") < stopped {
		t.Fatalf("cloud rows are not grouped by cost:\n%s", scr)
	}
	in = h.m.summaryInput(h.m.scope().pid)
	if in.Cloud[0] != "VM gpu-box (NC24ads_A100_v4 · rg-real), created by an agent, Azure says now: running" {
		t.Fatalf("summary input cloud = %q", in.Cloud)
	}

	// A failed check keeps the last answer and says so.
	h.m.opt.CheckLive = func(context.Context, string) ([]byte, error) { return nil, fmt.Errorf("not logged in") }
	h.cmd = nil
	h.keys("space", "c")
	h.send(h.cmd())
	h.mustSee("Azure check failed", "running · created 1h")
}

func TestLaunchCopilotIntoProject(t *testing.T) {
	dir := t.TempDir()
	a := session("a", "alpha", agents.Working, time.Minute, 1)
	a.Info.Cwd = dir
	h := newHarness(t, []core.Session{a}, true)
	p := h.seedProject("Nabu Work", "a", now)
	var got []LaunchReq
	h.m.opt.Launch = func(r LaunchReq) (string, error) { got = append(got, r); return "%9", nil }
	h.m.setSide(2)

	h.keys("o")
	if expandHome(h.m.input.Value()) != dir {
		t.Fatalf("folder default = %q, want %q", h.m.input.Value(), dir)
	}
	h.keys("enter")
	h.mustSee("First task")
	h.typeText("fix Bob's bug")
	h.keys("enter")

	if len(got) != 1 {
		t.Fatalf("launches = %d", len(got))
	}
	r := got[0]
	if r.Dir != dir || r.Window != "nabu-work" || r.Session != "$0" {
		t.Fatalf("request = %+v", r)
	}
	id, rest, _ := strings.Cut(strings.TrimPrefix(r.Command, "copilot --session-id "), " ")
	if len(id) != 36 || rest != `--name 'Nabu Work' -i 'fix Bob'\''s bug'` {
		t.Fatalf("command = %q", r.Command)
	}
	if as := h.m.st.Assign["copilot:"+id]; as.Project != p.ID {
		t.Fatalf("new session not filed under the project: %+v", as)
	}

	// When it appears, it is already in the project and gets selected.
	b := session("copilot:"+id, "Nabu Work", agents.Working, 0, 2)
	h.send(sessionsMsg{ss: []core.Session{a, b}})
	if s, ok := h.m.selected(); !ok || s.Key != b.Key {
		t.Fatalf("selected %q, want the new session", s.Key)
	}
}

func TestLaunchRejectsMissingFolder(t *testing.T) {
	h := newHarness(t, nil, true)
	h.m.opt.Launch = func(LaunchReq) (string, error) { t.Fatal("launched"); return "", nil }
	h.keys("o")
	h.m.input.SetValue(filepath.Join(t.TempDir(), "nope"))
	h.keys("enter")
	h.mustSee("is not a folder")
}

func TestTypeToSession(t *testing.T) {
	a := session("a", "alpha", agents.NeedsYou, time.Minute, 1)
	h := newHarness(t, []core.Session{a}, true)
	var typed [][2]string
	h.m.opt.Type = func(pane, text string) error { typed = append(typed, [2]string{pane, text}); return nil }
	h.m.opt.Screen = func(pane string) (string, error) {
		return "Run the migration?" + strings.Repeat("\n", 80) + "❯ 1. Yes\n  2. No\n\n\n", nil
	}
	h.m.focus = focusSessions

	cmd := h.send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if cmd == nil {
		t.Fatal("no screen capture")
	}
	h.send(cmd())
	h.mustSee("TYPING TO", "Run the migration?", "❯ 1. Yes", "Type to alpha")

	h.typeText("1")
	h.keys("enter")
	h.keys("enter")
	want := [][2]string{{"%a", "1"}, {"%a", ""}}
	if fmt.Sprint(typed) != fmt.Sprint(want) {
		t.Fatalf("typed %v, want %v", typed, want)
	}
	h.mustSee("Pressed Enter in alpha")
	h.keys("esc")
	if h.m.mode != modeNormal || strings.Contains(h.screen(), "TYPING TO") {
		t.Fatal("esc did not leave typing mode")
	}
}
