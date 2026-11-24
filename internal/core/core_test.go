package core

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/procs"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/tmux"
)

func paneLine(session, win string, idx int, name, pane string, pid int, path string) string {
	return strings.Join([]string{"$0", session, win, fmt.Sprint(idx), name, pane, fmt.Sprint(pid), "x", path, "1795200000", "1", "1"}, "\x1f")
}

func TestDiscover(t *testing.T) {
	cop := t.TempDir()
	sess := filepath.Join(cop, "sess-a")
	_ = os.MkdirAll(sess, 0o755)
	_ = os.WriteFile(filepath.Join(sess, "workspace.yaml"), []byte("name: Payments retry\nrepository: me/pay\nbranch: retry\n"), 0o644)
	_ = os.WriteFile(filepath.Join(sess, "events.jsonl"), []byte(`{"type":"assistant.turn_start","data":{},"timestamp":"2026-11-22T10:00:00Z"}`+"\n"), 0o644)
	_ = os.WriteFile(filepath.Join(sess, "inuse.101.lock"), nil, 0o644)

	panes := strings.Join([]string{
		paneLine("main", "@1", 0, "pay", "%1", 100, "/w/pay"),    // shell 100 -> copilot 101
		paneLine("main", "@2", 1, "zsh", "%2", 200, "/w"),        // plain shell, ignored
		paneLine("main", "@3", 2, "claude", "%3", 300, "/w/api"), // claude without a transcript
	}, "\n")
	src := Sources{
		Tmux: tmux.Client{Run: func(args ...string) (string, error) {
			switch args[0] {
			case "list-panes":
				return panes, nil
			case "capture-pane":
				if args[4] == "%3" {
					return "Do you want to proceed?\n❯ 1. Yes\n  2. No", nil
				}
			}
			return "", nil
		}},
		Procs: func() (procs.Table, error) {
			return procs.Parse("100 1 -zsh\n101 100 copilot --resume\n200 1 -zsh\n300 1 -zsh\n301 300 node /usr/lib/claude-code/cli.js\n"), nil
		},
		Copilot: &agents.Copilot{Dir: cop},
		Claude:  &agents.Claude{Dir: t.TempDir()},
		Started: func(int) (time.Time, error) { return time.Now(), nil },
	}
	ss, err := Discover(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 {
		t.Fatalf("want 2 agent sessions, got %d: %+v", len(ss), ss)
	}
	a, c := ss[0], ss[1]
	if a.Key != "copilot:sess-a" || a.Title() != "Payments retry" || a.Info.State != agents.Working || a.PID != 101 {
		t.Fatalf("copilot session: %+v", a)
	}
	if c.Kind != "claude" || c.Key != "pane:main:@3:%3" || c.Info.State != agents.NeedsYou {
		t.Fatalf("claude session should fall back to pane identity and detect the prompt: %+v", c)
	}
}

func sess(key, name, repo, branch string, st agents.State, updated time.Time) Session {
	return Session{Key: key, Info: agents.Info{Name: name, Repo: repo, Branch: branch, State: st, UpdatedAt: updated}}
}

func TestSuggest(t *testing.T) {
	now := time.Now()
	st := &store.State{Assign: map[string]store.Assignment{}}
	p, _ := st.AddProject("Payments", now)
	pid := p.ID
	th, _ := st.AddThread(pid, "Retry policy")
	tid := th.ID
	q, _ := st.AddProject("Website", now)
	qid := q.ID

	assigned := sess("copilot:w", "site work", "me/mono", "feature/landing", agents.Working, now)
	mainA := sess("copilot:m", "misc", "me/mono", "main", agents.Working, now)
	st.AssignSession(assigned.Key, qid, "", "", now)
	st.AssignSession(mainA.Key, qid, "", "", now)
	all := []Session{assigned, mainA}

	if sg, ok := Suggest(sess("x", "retry experiments", "", "", 0, now), st, all); !ok || sg.Project != pid || sg.Thread != tid {
		t.Fatalf("thread name match: %+v %v", sg, ok)
	}
	if sg, ok := Suggest(sess("x", "payments cleanup", "", "", 0, now), st, all); !ok || sg.Project != pid || sg.Thread != "" {
		t.Fatalf("project name match: %+v %v", sg, ok)
	}
	if sg, ok := Suggest(sess("x", "unrelated", "me/mono", "feature/landing", 0, now), st, all); !ok || sg.Project != qid {
		t.Fatalf("feature branch match: %+v %v", sg, ok)
	}
	if _, ok := Suggest(sess("x", "unrelated", "me/mono", "main", 0, now), st, all); ok {
		t.Fatal("a shared default branch must not produce a suggestion")
	}
}

func TestRollAndSort(t *testing.T) {
	now := time.Now()
	ss := []Session{
		sess("old", "old", "", "", agents.Ready, now.Add(-30*time.Hour)),
		sess("busy", "busy", "", "", agents.Working, now.Add(-time.Minute)),
		sess("done", "done", "", "", agents.Ready, now.Add(-time.Hour)),
		sess("ask", "ask", "", "", agents.NeedsYou, now.Add(-2*time.Hour)),
	}
	if ss[0].State(now) != agents.Quiet {
		t.Fatal("a session finished more than a day ago should be quiet")
	}
	r := Roll(ss, now)
	if r.State != agents.NeedsYou || r.NeedsYou != 1 || r.Working != 1 || r.Ready != 1 || r.Total != 4 {
		t.Fatalf("rollup = %+v", r)
	}
	if Roll(nil, now).State != agents.Quiet {
		t.Fatal("an empty group should be quiet, not unknown")
	}
	SortSessions(ss, now)
	var order []string
	for _, s := range ss {
		order = append(order, s.Key)
	}
	if strings.Join(order, ",") != "ask,busy,done,old" {
		t.Fatalf("order = %v", order)
	}

	// Scheduled working sessions stay working between ticks, then age to quiet if silent past StaleAfter.
	freshSched := Session{Key: "s1", Info: agents.Info{State: agents.Working, Scheduled: "every 20m", UpdatedAt: now.Add(-20 * time.Minute)}}
	staleSched := Session{Key: "s2", Info: agents.Info{State: agents.Working, Scheduled: "every 20m", UpdatedAt: now.Add(-30 * time.Hour)}}
	if freshSched.State(now) != agents.Working {
		t.Fatalf("fresh scheduled state = %v, want working", freshSched.State(now))
	}
	if staleSched.State(now) != agents.Quiet {
		t.Fatalf("stale scheduled state = %v, want quiet", staleSched.State(now))
	}
}
