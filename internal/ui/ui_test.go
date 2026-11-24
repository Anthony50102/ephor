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

func (h *harness) mustNotSee(part string) {
	h.t.Helper()
	if strings.Contains(h.screen(), part) {
		h.t.Fatalf("screen should not contain %q:\n%s", part, h.screen())
	}
}

// change edits the state file as another instance would and reloads it.
func (h *harness) change(fn func(*store.State)) {
	h.t.Helper()
	if _, err := store.Update(h.path, func(st *store.State) error { fn(st); return nil }); err != nil {
		h.t.Fatal(err)
	}
	h.m.stamp = ""
	h.m.reload()
}

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
	h.keys("tab", "tab")
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
	if h.m.focus != focusCenter {
		t.Fatal("ctrl-h from the sessions should focus the middle")
	}
	h.keys("ctrl+h")
	if h.m.focus != focusSide {
		t.Fatal("ctrl-h should focus the projects column")
	}
	h.keys("ctrl+l", "ctrl+l")
	if h.m.focus != focusSessions {
		t.Fatal("ctrl-l twice should focus the sessions column")
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
	lines := h.m.centerPage(h.m.layout()).lines
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
	scr := ansi.Strip(strings.Join(first2(h.m.cloudSection(h.m.sessions, 100, 6, false)), "\n"))
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
	if r.Dir != dir || r.Window != "nabu-work" || r.Session != tmux.PoolSession {
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

func TestCardsAndPanels(t *testing.T) {
	a := session("a", "alpha", agents.NeedsYou, time.Minute, 1)
	a.Info.Reason = "Approve the migration"
	b := session("b", "beta", agents.Working, time.Minute, 2)
	c := session("c", "gamma", agents.Working, 2*time.Minute, 3)
	h := newHarness(t, []core.Session{a, b, c}, true)
	p := h.seedProject("Nabu", "a", now)
	h.change(func(st *store.State) {
		st.AssignSession("b", p.ID, "", "beta", now)
		st.AssignSession("c", p.ID, "", "gamma", now)
	})

	// Overview: enter on the projects panel focuses the cards; enter on a
	// project card opens the project.
	h.keys("enter")
	if h.m.focus != focusCenter || h.m.cardID != "needs:a" || h.m.selKey != "a" {
		t.Fatalf("enter should highlight the first card, got focus %d card %q sel %q", h.m.focus, h.m.cardID, h.m.selKey)
	}
	h.keys("G")
	if h.m.cardID != "proj:"+p.ID {
		t.Fatalf("G should highlight the last card, got %q", h.m.cardID)
	}
	h.keys("enter")
	if h.m.scope().pid != p.ID || h.m.focus != focusCenter {
		t.Fatal("enter on a project card should open the project")
	}

	// Project: step to a session card, expand it, collapse it.
	h.keys("g", "g")
	var ids []string
	for i := 0; i < 6; i++ {
		ids = append(ids, h.m.cardID)
		h.keys("j")
	}
	if !strings.Contains(strings.Join(ids, " "), "sess:b") {
		t.Fatalf("cards visited: %v", ids)
	}
	h.keys("g", "g")
	for h.m.cardID != "sess:b" {
		h.keys("j")
	}
	if h.m.selKey != "b" {
		t.Fatal("highlighting a session card should select its session")
	}
	center := func() string { return ansi.Strip(strings.Join(h.m.centerPage(h.m.layout()).lines, "\n")) }
	if strings.Contains(center(), "0:2 beta") {
		t.Fatal("collapsed card shows detail")
	}
	h.keys("enter")
	if !strings.Contains(center(), "0:2 beta") {
		t.Fatal("enter should expand the session card")
	}
	h.keys("esc")
	if strings.Contains(center(), "0:2 beta") {
		t.Fatal("esc should collapse the card")
	}

	// Panels.
	h.keys("space", "e")
	if h.m.layout().sideW != 0 || strings.Contains(h.screen(), "WORKSPACE") {
		t.Fatal("space e should hide the projects panel")
	}
	h.keys("h")
	if h.m.focus != focusCenter {
		t.Fatal("h should not focus a hidden panel")
	}
	h.keys("space", "l")
	if h.m.layout().wide {
		t.Fatal("space l should hide the sessions panel")
	}
	h.mustSee("SESSIONS")
	h.keys("space", "z")
	if h.m.hideSide || h.m.hideRight {
		t.Fatal("space z with both hidden should show both")
	}
	h.keys("space", "z")
	if !h.m.hideSide || !h.m.hideRight {
		t.Fatal("space z should hide both")
	}
}

func first2[A, B any](a A, _ B) A { return a }

func TestInfraCards(t *testing.T) {
	s := session("k1", "trainer", agents.Working, time.Minute, 1)
	s.Info.Log = "/logs/k1"
	h := newHarness(t, []core.Session{s}, true)
	h.seedProject("ml", "k1", now)
	tunnel := "az network bastion tunnel --name b -g rg-a --target-resource-id /x/virtualMachines/box --resource-port 22 --port 2299"
	h.send(cloudMsg{res: map[string][]cloud.Resource{"/logs/k1": {
		{Kind: "VM", Name: "box", Scope: "rg-a", SSH: "ssh -p 2299 azureuser@127.0.0.1", Tunnel: tunnel, Last: now.Add(-20 * time.Hour), Count: 4},
		{Kind: "AKS cluster", Name: "aks1", Scope: "rg-k", Last: now.Add(-time.Hour), Count: 1},
		{Kind: "Kubernetes", Name: "aks1", Scope: "exp", Kube: "--context aks1", Last: now.Add(-time.Hour), Count: 3},
		{Kind: "Kubernetes job", Name: "train", Scope: "aks1/exp", Last: now.Add(-time.Hour), Count: 1},
	}}})
	h.m.opt.CheckLive = func(context.Context, string) ([]byte, error) {
		return []byte(`{"data":[
			{"name":"box","type":"microsoft.compute/virtualmachines","resourceGroup":"rg-a","location":"westus3","vmSize":"Standard_E32s_v5","power":"PowerState/running"},
			{"name":"aks1","type":"microsoft.containerservice/managedclusters","resourceGroup":"rg-k","location":"westus2","power":"Running","k8s":"1.36.5",
			 "pools":[{"name":"sys","count":2,"vmSize":"Standard_D4as_v5","mode":"System","powerState":{"code":"Running"}}]}]}`), nil
	}
	h.m.prices.USD = map[string]float64{"westus3|standard_e32s_v5": 2.016, "westus2|standard_d4as_v5": 0.25}
	var kubeCalls []string
	h.m.opt.Kubectl = func(_ context.Context, args ...string) ([]byte, error) {
		c := strings.Join(args, " ")
		kubeCalls = append(kubeCalls, c)
		if strings.Contains(c, "get nodes") {
			return []byte(`{"items":[{"metadata":{"name":"aks-sys-0","labels":{"agentpool":"sys"},"creationTimestamp":"2026-01-01T00:00:00Z"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`), nil
		}
		return []byte(`{"items":[{"kind":"Job","metadata":{"name":"train","namespace":"exp"},"status":{"failed":1,"conditions":[{"type":"Failed","status":"True","reason":"BackoffLimitExceeded"}]}},
			{"kind":"Job","metadata":{"name":"someone-else","namespace":"exp"},"status":{"active":1}}]}`), nil
	}
	var copied []string
	h.m.opt.Copy = func(s string) error { copied = append(copied, s); return nil }
	var launched []LaunchReq
	h.m.opt.Launch = func(r LaunchReq) (string, error) { launched = append(launched, r); return "%7", nil }

	h.keys("L")
	h.keys("space", "c")
	h.send(h.cmd())
	// The bill is totalled, and the VM nobody touched since yesterday is flagged.
	h.mustSee("$2.52/h · $60 a day at list price", "$2.02/h · running", "1 running resource no agent has touched in 12 hours")

	h.keys("l", "g", "g")
	for i := 0; h.m.cardID != "res:"+cloudID(h, "box"); i++ {
		if i > 20 {
			t.Fatalf("never reached the VM card; at %q", h.m.cardID)
		}
		h.keys("j")
	}
	h.keys("enter")
	h.mustSee("log in", tunnel[:40], "ssh -p 2299 azureuser@127.0.0.1", "E32s_v5 · $2.02/h list price when running")
	h.keys("y")
	if len(copied) != 1 || !strings.HasPrefix(copied[0], tunnel+" >/dev/null 2>&1 & sleep 10; ssh -p 2299") {
		t.Fatalf("copied %q", copied)
	}
	h.keys("s")
	if len(launched) != 1 || launched[0].Command != copied[0] {
		t.Fatalf("launched %+v", launched)
	}

	// Opening the cluster asks kubectl, with the agent's own access flags.
	h.keys("esc", "g", "g")
	for i := 0; h.m.cardID != "res:"+cloudID(h, "aks1"); i++ {
		if i > 20 {
			t.Fatalf("never reached the cluster card; at %q", h.m.cardID)
		}
		h.keys("j")
	}
	h.cmd = nil
	h.keys("enter")
	h.mustSee("NODES", "asking kubectl")
	h.send(h.cmd())
	if len(kubeCalls) != 2 || kubeCalls[0] != "--context aks1 get nodes -o json" {
		t.Fatalf("kubectl calls %q", kubeCalls)
	}
	h.mustSee("sys", "System · 2× D4as_v5", "$0.50/h", "aks-sys-0", "train", "failed (BackoffLimitExceeded)", "+ 1 other job in these namespaces: 1 running")
	h.keys("s")
	if r := launched[1]; !strings.HasPrefix(r.Command, "kubectl --context aks1 get nodes") || r.Pretype != "kubectl --context aks1 debug -it --image="+debugImage+" node/" {
		t.Fatalf("launched %+v", r)
	}
	h.keys("y")
	if copied[1] != "kubectl --context aks1 debug node/aks-sys-0 -it --image="+debugImage {
		t.Fatalf("copied %q", copied[1])
	}
	// A fresh answer is not asked for again until it ages.
	h.cmd = nil
	if cmd := h.m.maybeKube(false); cmd != nil && cmd() != nil {
		t.Fatal("kubectl should not be asked again within a minute")
	}
}

func cloudID(h *harness, name string) string {
	rows, _ := h.m.cloudRows(h.m.sessionsIn(h.m.scope()))
	for _, r := range rows {
		if r.Name == name {
			return r.ID()
		}
	}
	return "?"
}

func TestDockAgent(t *testing.T) {
	a := session("a", "alpha", agents.Working, time.Minute, 1)
	b := session("b", "beta", agents.Working, 2*time.Minute, 2)
	h := newHarness(t, []core.Session{a, b}, true)
	var log []string
	h.m.opt.Dock = func(id string) error { log = append(log, "dock "+id); return nil }
	h.m.opt.Undock = func(id string) error { log = append(log, "undock "+id); return nil }
	h.m.focus = focusSessions
	sel, _ := h.m.selected()
	other := map[string]string{"%a": "%b", "%b": "%a"}[sel.Pane.PaneID]

	h.keys("v")
	h.mustSee("Docked " + sel.Title())
	h.keys("u") // clears the status so the footer shows
	h.mustSee("v send back " + sel.Title())
	h.keys("j")
	h.mustSee("v dock this instead")
	h.keys("v") // swaps
	h.keys("v") // sends it back
	if w := "dock " + sel.Pane.PaneID + "|undock " + sel.Pane.PaneID + "|dock " + other + "|undock " + other; strings.Join(log, "|") != w {
		t.Fatalf("got %q, want %q", strings.Join(log, "|"), w)
	}
	if h.m.Docked() != "" {
		t.Fatal("nothing should be docked")
	}

	// An agent that exits while docked is forgotten, not sent back.
	h.keys("v")
	h.send(sessionsMsg{ss: []core.Session{a, b}[:1]})
	if h.m.Docked() != "" && h.m.Docked() != "%a" {
		t.Fatalf("docked = %q", h.m.Docked())
	}
	h.send(sessionsMsg{ss: nil})
	if h.m.Docked() != "" {
		t.Fatal("a closed pane should not stay docked")
	}

	// ctrl-l past the last panel moves to the docked agent's pane, as in
	// vim-tmux-navigator; when narrow, the cards are already the edge.
	var moved []string
	h.m.opt.SelectPane = func(dir string) error { moved = append(moved, dir); return nil }
	h.m.focus = focusSide
	h.keys("ctrl+l", "ctrl+l", "ctrl+l")
	if h.m.focus != focusSessions || strings.Join(moved, "") != "R" {
		t.Fatalf("focus %d, moved %q", h.m.focus, moved)
	}
	h.send(tea.WindowSizeMsg{Width: 98, Height: 50})
	h.m.focus = focusCenter
	h.keys("ctrl+l", "ctrl+h", "ctrl+h")
	if h.m.focus != focusSide || strings.Join(moved, "") != "RRL" {
		t.Fatalf("narrow: focus %d, moved %q", h.m.focus, moved)
	}

	h.m.opt.Dock = nil
	h.keys("v")
	h.mustSee("needs Switchboard running inside tmux")
}

func TestUXPolish(t *testing.T) {
	// Markdown cleanup strips noisy tokens while keeping link labels.
	got := cleanProse("## **Merged** `[PR #42](https://example.com/pr/42)` - check [logs](https://example.com)")
	if strings.Contains(got, "**") || strings.Contains(got, "`") || strings.Contains(got, "https://") || strings.HasPrefix(got, "#") {
		t.Fatalf("cleanProse left markdown noise: %q", got)
	}
	if !strings.Contains(got, "Merged") || !strings.Contains(got, "logs") {
		t.Fatalf("cleanProse dropped readable text: %q", got)
	}

	// Entering center auto-highlights the first card, and the footer stays
	// context-aware (no dead y/s hints on non-infra cards).
	a := session("a", "alpha", agents.Working, time.Minute, 1)
	a.Info.Ask = "Implement streaming parser"
	h := newHarness(t, []core.Session{a}, true)
	h.keys("l")
	if h.m.focus != focusCenter || h.m.cardID == "" {
		t.Fatalf("expected center focus with an active card, got focus=%d cardID=%q", h.m.focus, h.m.cardID)
	}
	h.mustNotSee("y copy")

	// Session detail does not duplicate LAST ASKED under DOING when there is
	// no AI summary for the session.
	h.mustSee("LAST ASKED")
	h.mustNotSee("DOING  summary")

	// Help screen scrolls on short terminals and still closes on esc.
	h.send(tea.WindowSizeMsg{Width: 84, Height: 16})
	h.keys("?")
	if h.m.mode != modeHelp {
		t.Fatalf("expected modeHelp, got %d", h.m.mode)
	}
	h.mustSee("more · ctrl-d")
	h.keys("j", "j")
	if h.m.helpOff == 0 {
		t.Fatal("j should scroll help on short terminals")
	}
	h.keys("esc")
	if h.m.mode != modeNormal {
		t.Fatalf("esc should close help, got %d", h.m.mode)
	}

	// Narrow/docked width uses compact sidebar legend and fits within width.
	h.send(tea.WindowSizeMsg{Width: 84, Height: 30})
	v := h.screen()
	if !strings.Contains(v, "● wait   ● work") || strings.Contains(v, "creat│") {
		t.Fatalf("expected compact narrow sidebar without mid-word clipping, got:\n%s", v)
	}
	for i, line := range strings.Split(v, "\n") {
		if w := ansi.StringWidth(line); w > 84 {
			t.Fatalf("narrow line %d width %d > 84: %q", i, w, line)
		}
	}

	// Left-click focuses the clicked panel and selects rows.
	h.send(tea.WindowSizeMsg{Width: 160, Height: 40})
	h.m.focus = focusSide
	h.send(tea.MouseMsg{X: 140, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if h.m.focus != focusSessions {
		t.Fatalf("left-click in right pane should focus sessions, got %d", h.m.focus)
	}
	h.send(tea.MouseMsg{X: 4, Y: 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if h.m.focus != focusSide {
		t.Fatalf("left-click in left pane should focus sidebar, got %d", h.m.focus)
	}
}

func TestParkAgents(t *testing.T) {
	a := session("a", "alpha", agents.Working, time.Minute, 1)
	b := session("b", "beta", agents.Working, 2*time.Minute, 2)
	c := session("c", "gamma", agents.Ready, 3*time.Minute, 3)
	c.Pane.SessionName = tmux.PoolSession

	h := newHarness(t, []core.Session{a, b, c}, true)
	var log []string
	h.m.opt.Dock = func(id string) error { log = append(log, "dock "+id); return nil }
	h.m.opt.Undock = func(id string) error { log = append(log, "undock "+id); return nil }
	h.m.opt.Rehome = func(id string) error { log = append(log, "rehome "+id); return nil }
	h.m.opt.Park = func(id string) (bool, error) { log = append(log, "park "+id); return true, nil }
	h.m.focus = focusSessions

	// Dock alpha, then press P (park all): beta parks immediately, gamma is
	// skipped (already in agents), and alpha stays docked but is re-homed.
	h.keys("v")
	h.keys("P")
	if h.m.Docked() != "%a" {
		t.Fatalf("park-all should keep docked agent open, got %q", h.m.Docked())
	}
	h.mustSee("Parked 1 agent in agents (docked agent will return there too).")
	want := "dock %a|rehome %a|park %b"
	if strings.Join(log, "|") != want {
		t.Fatalf("log = %q, want %q", strings.Join(log, "|"), want)
	}

	// Pressing p directly on the docked session re-homes and undocks it into agents.
	log = nil
	h.keys("p")
	if h.m.Docked() != "" {
		t.Fatalf("p on docked session should undock it, got %q", h.m.Docked())
	}
	if strings.Join(log, "|") != "rehome %a|undock %a" {
		t.Fatalf("p on docked log = %q", strings.Join(log, "|"))
	}
	h.mustSee("Parked alpha in agents.")

	// :park-all also works from command mode.
	log = nil
	h.keys(":")
	h.typeText("park-all")
	h.keys("enter")
	if strings.Join(log, "|") != "park %a|park %b" {
		t.Fatalf(":park-all log = %q", strings.Join(log, "|"))
	}
}
