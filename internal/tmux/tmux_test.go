package tmux

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewWindowCreatesMissingPoolSession(t *testing.T) {
	var calls []string
	c := Client{Run: func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "has-session":
			return "", errors.New("can't find session")
		case "new-session":
			return "%12\n", nil
		}
		return "", nil
	}}
	pane, err := c.NewWindow(PoolSession, "nabu", "/work")
	if err != nil || pane != "%12" {
		t.Fatalf("NewWindow = (%q, %v)", pane, err)
	}
	want := "new-session -d -P -F #{pane_id} -s agents -n nabu -c /work"
	if len(calls) != 2 || calls[1] != want {
		t.Fatalf("calls = %v", calls)
	}
}

func TestUndockRecreatesVanishedSession(t *testing.T) {
	var calls []string
	saved := strings.Join([]string{"$9", "@5", "0", "pt-train", "%0", PoolSession}, sep)
	c := Client{Run: func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "show-options":
			return saved + "\n", nil
		case "list-windows":
			return "@0\n", nil
		case "has-session":
			return "", errors.New("no such session")
		case "new-session":
			return "@99\n", nil
		}
		return "", nil
	}}
	if err := c.Undock("%5"); err != nil {
		t.Fatalf("Undock: %v", err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"new-session -d -P -F #{window_id} -s agents",
		"break-pane -d -s %5 -t =agents: -n pt-train",
		"kill-window -t @99",
		"set-option -p -u -t %5 @switchboard-home",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestParkSingleAndMultiPaneAndRehome(t *testing.T) {
	var calls []string
	hasAgents := false
	c := Client{Run: func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "display-message":
			switch args[3] {
			case "%1":
				return strings.Join([]string{"0", "@1", "nabu", "1"}, sep), nil
			case "%2":
				return strings.Join([]string{"0", "@2", "split", "2"}, sep), nil
			case "%3":
				return strings.Join([]string{PoolSession, "@3", "done", "1"}, sep), nil
			}
		case "has-session":
			if hasAgents {
				return "", nil
			}
			return "", errors.New("not found")
		case "new-session":
			hasAgents = true
			return "@tmp\n", nil
		case "show-options":
			return strings.Join([]string{"$0", "@4", "4", "docked", "%0", "0"}, sep), nil
		}
		return "", nil
	}}

	// Single-pane window creates agents without leaving a placeholder shell.
	moved, err := c.Park("%1", PoolSession, "%0")
	if err != nil || !moved {
		t.Fatalf("Park single = (%v, %v)", moved, err)
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"new-session -d -P -F #{window_id} -s agents",
		"move-window -d -s @1 -t =agents:",
		"kill-window -t @tmp",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("single-pane park missing %q in:\n%s", want, joined)
		}
	}

	// Multi-pane window breaks out just the agent pane so sibling panes stay put.
	calls = nil
	moved, err = c.Park("%2", PoolSession, "%0")
	if err != nil || !moved {
		t.Fatalf("Park multi = (%v, %v)", moved, err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "break-pane -d -s %2 -t =agents: -n split") {
		t.Fatalf("multi-pane park calls = %v", calls)
	}

	// Already in agents is a no-op; host pane is also skipped.
	if moved, err := c.Park("%3", PoolSession, "%0"); err != nil || moved {
		t.Fatalf("already parked = (%v, %v)", moved, err)
	}
	if moved, err := c.Park("%0", PoolSession, "%0"); err != nil || moved {
		t.Fatalf("host pane = (%v, %v)", moved, err)
	}

	// Rehome preserves window name and host pane while aiming Undock at agents.
	calls = nil
	if err := c.Rehome("%4", PoolSession); err != nil {
		t.Fatalf("Rehome: %v", err)
	}
	wantHome := strings.Join([]string{"", "", "", "docked", "%0", PoolSession}, sep)
	if !strings.Contains(strings.Join(calls, "\n"), "set-option -p -t %4 @switchboard-home "+wantHome) {
		t.Fatalf("Rehome calls = %v", calls)
	}
}

func TestPoolRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := filepath.Join(t.TempDir(), "tmux.sock")
	run := func(args ...string) (string, error) {
		out, err := exec.Command("tmux", append([]string{"-S", sock, "-f", "/dev/null"}, args...)...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("tmux %v: %s (%w)", args, strings.TrimSpace(string(out)), err)
		}
		return string(out), nil
	}
	t.Cleanup(func() { _, _ = run("kill-server") })
	c := Client{Run: run}
	must := func(label string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
	}
	list := func() string {
		out, err := run("list-panes", "-a", "-F", "#{session_name}:#{window_index} #{window_name} #{pane_id}")
		must("list-panes", err)
		return out
	}

	hostPane, err := run("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "main", "-n", "sb", "sleep 60")
	must("new main", err)
	hostPane = strings.TrimSpace(hostPane)
	a1, err := run("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "=main:", "-n", "a1", "sleep 60")
	must("new a1", err)
	a1 = strings.TrimSpace(a1)
	a2, err := run("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "=main:", "-n", "split", "sleep 60")
	must("new a2", err)
	a2 = strings.TrimSpace(a2)
	_, err = run("split-window", "-d", "-t", a2, "sleep 60")
	must("split a2", err)
	a3, err := run("new-window", "-d", "-P", "-F", "#{pane_id}", "-t", "=main:", "-n", "a3", "sleep 60")
	must("new a3", err)
	a3 = strings.TrimSpace(a3)

	// 1. Park single-pane window: creates agents without leaving a placeholder window.
	moved, err := c.Park(a1, PoolSession, hostPane)
	must("park a1", err)
	if !moved || strings.Count(list(), "agents:") != 1 {
		t.Fatalf("after park a1 (moved=%v):\n%s", moved, list())
	}

	// 2. Park multi-pane window: breaks only a2 out; sibling pane stays in main:split.
	moved, err = c.Park(a2, PoolSession, hostPane)
	must("park a2", err)
	if out := list(); !moved || strings.Count(out, "agents:") != 2 || !strings.Contains(out, "main:") || !strings.Contains(out, "split") {
		t.Fatalf("after park a2:\n%s", out)
	}

	// 3. Docking the last window in agents destroys agents; Undock recreates it cleanly.
	_, err = run("kill-pane", "-t", a2)
	must("kill a2", err)
	must("dock last a1", c.Dock(a1, hostPane, "55%"))
	if strings.Contains(list(), "agents:") {
		t.Fatalf("expected agents destroyed while last pane docked:\n%s", list())
	}
	must("undock a1", c.Undock(a1))
	if out := list(); strings.Count(out, "agents:") != 1 {
		t.Fatalf("expected agents recreated with 1 pane:\n%s", out)
	}

	// 4. Dock a3 from main, Rehome to agents, Undock: returns to agents, not main.
	must("dock a3", c.Dock(a3, hostPane, "55%"))
	must("rehome a3", c.Rehome(a3, PoolSession))
	must("undock a3", c.Undock(a3))
	if out := list(); strings.Count(out, "agents:") != 2 || strings.Contains(out, "main:3 a3") {
		t.Fatalf("after rehome+undock a3:\n%s", out)
	}

	// 5. NewWindow into agents opens in agents.
	a4, err := c.NewWindow(PoolSession, "fresh", t.TempDir())
	must("NewWindow", err)
	if out := list(); a4 == "" || strings.Count(out, "agents:") != 3 {
		t.Fatalf("after NewWindow:\n%s", out)
	}
}
