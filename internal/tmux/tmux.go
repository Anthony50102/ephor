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

// ServerID identifies the running tmux server instance using its PID and start
// time, so Switchboard can distinguish closing one window from a tmux restart
// or machine reboot.
func (c Client) ServerID() (string, error) {
	out, err := c.Run("display-message", "-p", "#{pid}:#{start_time}")
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(out)
	if id == "" || id == ":" {
		return "", fmt.Errorf("empty tmux server id")
	}
	return id, nil
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

// PoolSession is the background tmux session where Switchboard parks agent
// windows so the user's main session stays uncluttered.
const PoolSession = "agents"

// homeOpt is a pane option holding where a docked pane came from, so it can
// go back even after Switchboard restarts.
const homeOpt = "@switchboard-home"

func exactSession(s string) string {
	if s == "" || strings.HasPrefix(s, "$") {
		return s
	}
	return "=" + s
}

func (c Client) hasSession(session string) bool {
	if session == "" {
		return false
	}
	_, err := c.Run("has-session", "-t", exactSession(session))
	return err == nil
}

// Dock moves an agent's pane into the host pane's window, to its right, and
// selects it. width is a tmux size such as "60%".
func (c Client) Dock(paneID, hostPane, width string) error {
	out, err := c.Run("display-message", "-p", "-t", paneID,
		"#{session_id}"+sep+"#{window_id}"+sep+"#{window_index}"+sep+"#{window_name}"+sep+"#{session_name}")
	if err != nil {
		return err
	}
	f := strings.Split(strings.TrimSpace(out), sep)
	if len(f) < 5 {
		return fmt.Errorf("unexpected tmux reply for %s", paneID)
	}
	// Keep hostPane at index 4 so Orphans stays compatible with older homes.
	home := strings.Join([]string{f[0], f[1], f[2], f[3], hostPane, f[4]}, sep)
	if _, err := c.Run("set-option", "-p", "-t", paneID, homeOpt, home); err != nil {
		return err
	}
	if _, err := c.Run("join-pane", "-h", "-l", width, "-s", paneID, "-t", hostPane); err != nil {
		c.Run("set-option", "-p", "-u", "-t", paneID, homeOpt)
		return err
	}
	return nil
}

// Rehome updates a docked pane's saved return address so Undock sends it to
// poolSession instead of the session it originally came from.
func (c Client) Rehome(paneID, poolSession string) error {
	out, err := c.Run("show-options", "-p", "-v", "-t", paneID, homeOpt)
	if err != nil {
		return err
	}
	f := strings.Split(strings.TrimSpace(out), sep)
	if len(f) < 5 {
		return fmt.Errorf("pane %s has no home to update", paneID)
	}
	home := strings.Join([]string{"", "", "", f[3], f[4], poolSession}, sep)
	_, err = c.Run("set-option", "-p", "-t", paneID, homeOpt, home)
	return err
}

// Undock sends a docked pane back to its window, or to a new window in its
// home session (recreating that session if docking its last window closed it),
// and leaves the host focused.
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
	sessionName := ""
	if len(f) >= 6 {
		sessionName = f[5]
	}
	switch {
	case window != "" && c.windowExists(window):
		if _, err := c.Run("join-pane", "-d", "-h", "-s", paneID, "-t", window); err != nil {
			return err
		}
	case session != "" && c.hasSession(session):
		target := exactSession(session) + ":"
		if index != "" {
			if _, err := c.Run("break-pane", "-d", "-s", paneID, "-n", name, "-t", target+index); err == nil {
				break
			}
		}
		if _, err := c.Run("break-pane", "-d", "-s", paneID, "-n", name, "-t", target); err != nil {
			return err
		}
	default:
		if sessionName == "" {
			sessionName = PoolSession
		}
		if err := c.breakToSession(paneID, name, sessionName); err != nil {
			return err
		}
	}
	_, err = c.Run("set-option", "-p", "-u", "-t", paneID, homeOpt)
	return err
}

// breakToSession breaks paneID into its own window in sessionName, creating
// sessionName without leaving a blank shell window behind if needed.
func (c Client) breakToSession(paneID, name, sessionName string) error {
	placeholder := ""
	if !c.hasSession(sessionName) {
		out, err := c.Run("new-session", "-d", "-P", "-F", "#{window_id}", "-s", sessionName)
		if err != nil {
			return err
		}
		placeholder = strings.TrimSpace(out)
	}
	args := []string{"break-pane", "-d", "-s", paneID, "-t", exactSession(sessionName) + ":"}
	if name != "" {
		args = append(args, "-n", name)
	}
	if _, err := c.Run(args...); err != nil {
		if placeholder != "" {
			_, _ = c.Run("kill-window", "-t", placeholder)
		}
		return err
	}
	if placeholder != "" {
		_, _ = c.Run("kill-window", "-t", placeholder)
	}
	return nil
}

// Park moves an agent pane into poolSession so it stays out of the user's main
// session. Single-pane windows move whole; multi-pane windows split out just
// the agent pane so sibling shells stay put. Returns false when already parked.
func (c Client) Park(paneID, poolSession, hostPane string) (bool, error) {
	if paneID == "" || paneID == hostPane {
		return false, nil
	}
	out, err := c.Run("display-message", "-p", "-t", paneID,
		"#{session_name}"+sep+"#{window_id}"+sep+"#{window_name}"+sep+"#{window_panes}")
	if err != nil {
		return false, err
	}
	f := strings.Split(strings.TrimSpace(out), sep)
	if len(f) < 4 {
		return false, fmt.Errorf("unexpected tmux reply for %s", paneID)
	}
	sessName, winID, winName := f[0], f[1], f[2]
	panes, _ := strconv.Atoi(f[3])
	if sessName == poolSession {
		return false, nil
	}
	if panes > 1 {
		return true, c.breakToSession(paneID, winName, poolSession)
	}
	placeholder := ""
	if !c.hasSession(poolSession) {
		pout, err := c.Run("new-session", "-d", "-P", "-F", "#{window_id}", "-s", poolSession)
		if err != nil {
			return false, err
		}
		placeholder = strings.TrimSpace(pout)
	}
	if _, err := c.Run("move-window", "-d", "-s", winID, "-t", exactSession(poolSession)+":"); err != nil {
		if placeholder != "" {
			_, _ = c.Run("kill-window", "-t", placeholder)
		}
		return false, err
	}
	if placeholder != "" {
		_, _ = c.Run("kill-window", "-t", placeholder)
	}
	return true, nil
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
// when empty) and returns its pane ID. If a named session does not exist yet,
// it creates that detached session with the new window. With no tmux server
// running it starts a detached session named "switchboard".
func (c Client) NewWindow(session, name, dir string) (string, error) {
	if session != "" && !strings.HasPrefix(session, "$") && !c.hasSession(session) {
		out, err := c.Run("new-session", "-d", "-P", "-F", "#{pane_id}", "-s", session, "-n", name, "-c", dir)
		return strings.TrimSpace(out), err
	}
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
		args = append(args, "-t", exactSession(session)+":")
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
