// Package tmux lists panes and moves the client between windows.
package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const sep = "\x1f"

var paneFormat = strings.Join([]string{
	"#{session_id}", "#{session_name}", "#{window_id}", "#{window_index}", "#{window_name}",
	"#{pane_id}", "#{pane_pid}", "#{pane_current_command}", "#{pane_current_path}",
	"#{window_activity}", "#{window_active}", "#{pane_active}",
}, sep)

// Pane is one tmux pane and the window that contains it.
type Pane struct {
	SessionID   string
	SessionName string
	WindowID    string
	WindowIndex int
	WindowName  string
	PaneID      string
	PanePID     int
	Command     string
	Path        string
	Activity    time.Time
	WindowLive  bool
	PaneActive  bool
}

// Target is the human-readable location, for example "0:3 bf".
func (p Pane) Target() string {
	return fmt.Sprintf("%s:%d %s", p.SessionName, p.WindowIndex, p.WindowName)
}

// Runner executes tmux. Tests replace it.
type Runner func(args ...string) (string, error)

// Exec runs the real tmux binary.
func Exec(args ...string) (string, error) {
	out, err := exec.Command("tmux", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("tmux %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return string(out), nil
}

// Client wraps a Runner with the operations Switchboard needs.
type Client struct{ Run Runner }

// ListPanes returns every pane on the tmux server.
func (c Client) ListPanes() ([]Pane, error) {
	out, err := c.Run("list-panes", "-a", "-F", paneFormat)
	if err != nil {
		return nil, err
	}
	return ParsePanes(out), nil
}

// ParsePanes parses list-panes output produced with paneFormat.
func ParsePanes(out string) []Pane {
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, sep)
		if len(f) != 12 {
			continue
		}
		idx, _ := strconv.Atoi(f[3])
		pid, _ := strconv.Atoi(f[6])
		act, _ := strconv.ParseInt(f[9], 10, 64)
		panes = append(panes, Pane{
			SessionID: f[0], SessionName: f[1], WindowID: f[2], WindowIndex: idx,
			WindowName: f[4], PaneID: f[5], PanePID: pid, Command: f[7], Path: f[8],
			Activity: time.Unix(act, 0), WindowLive: f[10] == "1", PaneActive: f[11] == "1",
		})
	}
	return panes
}

// Capture returns the last n lines of a pane.
func (c Client) Capture(paneID string, n int) (string, error) {
	return c.Run("capture-pane", "-p", "-J", "-t", paneID, "-S", strconv.Itoa(-n))
}

// Focus selects the pane's window and pane so a later attach or switch lands on it.
func (c Client) Focus(p Pane) error {
	if _, err := c.Run("select-window", "-t", p.WindowID); err != nil {
		return err
	}
	_, err := c.Run("select-pane", "-t", p.PaneID)
	return err
}

// SwitchClient moves the current tmux client to the pane's session.
func (c Client) SwitchClient(p Pane) error {
	_, err := c.Run("switch-client", "-t", p.SessionID)
	return err
}

// Show selects a pane's window and moves the current client to it.
func (c Client) Show(paneID string) error {
	if _, err := c.Run("select-window", "-t", paneID); err != nil {
		return err
	}
	_, err := c.Run("switch-client", "-t", paneID)
	return err
}

// SelectPane moves to the neighboring pane in a direction: L, R, U, or D.
func (c Client) SelectPane(from, dir string) error {
	_, err := c.Run("select-pane", "-t", from, "-"+dir)
	return err
}

// Pretype types text into a pane without pressing Enter.
func (c Client) Pretype(paneID, text string) error {
	_, err := c.Run("send-keys", "-t", paneID, "-l", "--", text)
	return err
}

// homeOpt is a pane option holding where a docked pane came from, so it can
// go back even after Switchboard restarts.
const homeOpt = "@switchboard-home"

// Dock moves an agent's pane into the host pane's window, to its right, and
// selects it. width is a tmux size such as "60%".
func (c Client) Dock(paneID, hostPane, width string) error {
	out, err := c.Run("display-message", "-p", "-t", paneID, "#{session_id}"+sep+"#{window_id}"+sep+"#{window_index}"+sep+"#{window_name}")
	if err != nil {
		return err
	}
	home := strings.TrimSpace(out) + sep + hostPane
	if _, err := c.Run("set-option", "-p", "-t", paneID, homeOpt, home); err != nil {
		return err
	}
	if _, err := c.Run("join-pane", "-h", "-l", width, "-s", paneID, "-t", hostPane); err != nil {
		c.Run("set-option", "-p", "-u", "-t", paneID, homeOpt)
		return err
	}
	return nil
}

// Undock sends a docked pane back to its window, or to a new window at its
// old index when that window closed with it, and leaves the host focused.
func (c Client) Undock(paneID string) error {
	out, err := c.Run("show-options", "-p", "-v", "-t", paneID, homeOpt)
	if err != nil {
		return err
	}
	f := strings.Split(strings.TrimSpace(out), sep)
	if len(f) < 4 {
		return fmt.Errorf("pane %s has no home to go back to", paneID)
	}
	session, window, index, name := f[0], f[1], f[2], f[3]
	if c.windowExists(window) {
		_, err = c.Run("join-pane", "-d", "-h", "-s", paneID, "-t", window)
		if err != nil {
			return err
		}
	} else if _, err := c.Run("break-pane", "-d", "-s", paneID, "-n", name, "-t", session+":"+index); err != nil {
		// The index was taken meanwhile; any free one will do.
		if _, err := c.Run("break-pane", "-d", "-s", paneID, "-n", name, "-t", session+":"); err != nil {
			return err
		}
	}
	_, err = c.Run("set-option", "-p", "-u", "-t", paneID, homeOpt)
	return err
}

func (c Client) windowExists(id string) bool {
	out, err := c.Run("list-windows", "-a", "-F", "#{window_id}")
	if err != nil {
		return false
	}
	for _, w := range strings.Fields(out) {
		if w == id {
			return true
		}
	}
	return false
}

// Orphans lists docked panes whose Switchboard pane has closed, as after a
// crash, so they can be sent back.
func (c Client) Orphans() ([]string, error) {
	out, err := c.Run("list-panes", "-a", "-F", "#{pane_id}"+sep+"#{"+homeOpt+"}")
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	homes := map[string][]string{}
	for _, l := range strings.Split(out, "\n") {
		id, home, _ := strings.Cut(l, sep)
		if id == "" {
			continue
		}
		live[id] = true
		if home != "" {
			homes[id] = strings.Split(home, sep)
		}
	}
	var ids []string
	for id, f := range homes {
		if len(f) < 5 || !live[f[4]] {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// InsideTmux reports whether this process runs inside a tmux client.
func InsideTmux() bool { return os.Getenv("TMUX") != "" }

// ExecAttach replaces the current process with `tmux attach` to the pane's session.
func ExecAttach(p Pane) error {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return err
	}
	return syscall.Exec(bin, []string{"tmux", "attach-session", "-t", p.SessionID}, os.Environ())
}

// Screen returns the visible text of a pane, without colors.
func (c Client) Screen(paneID string) (string, error) {
	return c.Run("capture-pane", "-p", "-t", paneID)
}

// NewWindow opens a detached window in session (the current or first session
// when empty) and returns its pane ID. With no tmux server running it starts
// a detached session named "switchboard".
func (c Client) NewWindow(session, name, dir string) (string, error) {
	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-n", name, "-c", dir}
	if session == "" && !InsideTmux() {
		out, err := c.Run("list-sessions", "-F", "#{session_id}")
		if err != nil || strings.TrimSpace(out) == "" {
			out, err := c.Run("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", "switchboard", "-n", name, "-c", dir)
			return strings.TrimSpace(out), err
		}
		session, _, _ = strings.Cut(strings.TrimSpace(out), "\n")
	}
	if session != "" {
		args = append(args, "-t", session+":")
	}
	out, err := c.Run(args...)
	return strings.TrimSpace(out), err
}

// Type sends text to a pane as if typed, then presses Enter. Empty text
// presses Enter only.
func (c Client) Type(paneID, text string) error {
	if text != "" {
		if _, err := c.Run("send-keys", "-t", paneID, "-l", "--", text); err != nil {
			return err
		}
	}
	_, err := c.Run("send-keys", "-t", paneID, "Enter")
	return err
}
