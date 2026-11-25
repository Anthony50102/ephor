package ui

import (
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/tmux"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// LaunchReq asks for a new tmux window that runs Command in Dir.
type LaunchReq struct {
	Session string // tmux session ID to open the window in; empty picks one
	Window  string
	Dir     string
	Command string
	Pretype string // typed after Command without Enter, left for the user to finish
	// BeforeType runs after the window is created and before Command is typed
	// (e.g. docking or switching the client so the pane has an attached client
	// for terminal color queries before Copilot starts).
	BeforeType func(paneID string) error
}

// screenMsg carries a capture of the pane being typed to.
type screenMsg struct {
	pane, text string
	err        error
	gen        int
}

type screenTick struct{ gen int }

// screenEvery is how often the live terminal view refreshes.
const screenEvery = time.Second

// ---- starting a new agent ----

// startLaunch asks where to start a new Copilot session for the current scope.
func (m *Model) startLaunch() {
	if m.opt.Launch == nil {
		m.setErr(fmt.Errorf("starting sessions needs tmux"))
		return
	}
	sc := m.scope()
	m.startInput(inLaunchDir, sc.pid, sc.tid, "", shortPath(m.launchDir(sc)))
}

// launchDir guesses the folder for a new session: where the scope's most
// recent session runs, else the selected session's folder, else home.
func (m Model) launchDir(sc sideItem) string {
	var best core.Session
	for _, s := range m.sessionsIn(sc) {
		if s.Info.Cwd != "" && (best.Info.Cwd == "" || s.Info.UpdatedAt.After(best.Info.UpdatedAt)) {
			best = s
		}
	}
	if best.Info.Cwd != "" {
		return best.Info.Cwd
	}
	if s, ok := m.selected(); ok && s.Info.Cwd != "" {
		return s.Info.Cwd
	}
	home, _ := os.UserHomeDir()
	return home
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// commitLaunchDir checks the folder and asks for the first task.
func (m *Model) commitLaunchDir(v string) {
	dir := expandHome(v)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		m.setErr(fmt.Errorf("%s is not a folder", v))
		return
	}
	m.inputKey = dir
	m.startInput(inLaunchTask, m.inputPID, m.inputTID, dir, "")
}

// launch opens a tmux window running Copilot. Switchboard picks the session
// ID, so the session is filed under its project before Copilot starts.
func (m *Model) launch(dir, task string) tea.Cmd {
	pid, tid := m.inputPID, m.inputTID
	if pid != "" && m.st.Project(pid) == nil {
		pid, tid = "", ""
	}
	name := m.placeName(pid, tid)
	if pid == "" {
		name = ""
	}
	id := newUUID()
	key := "copilot:" + id
	if pid != "" {
		now := m.opt.Now()
		if !m.change(func(st *store.State) error {
			if st.Project(pid) == nil {
				return fmt.Errorf("that project was deleted")
			}
			st.AssignSession(key, pid, tid, name, now)
			return nil
		}) {
			return nil
		}
	}
	req := LaunchReq{Session: tmux.PoolSession, Dir: dir, Window: windowName(name), Command: copilotCommand(id, name, task)}
	pane, err := m.opt.Launch(req)
	if err != nil {
		if pid != "" {
			m.change(func(st *store.State) error { st.AssignSession(key, "", "", "", m.opt.Now()); return nil })
		}
		m.setErr(fmt.Errorf("could not start Copilot: %w", err))
		return nil
	}
	m.wantKey = key
	where := "the inbox"
	if pid != "" {
		where = name
	}
	m.setStatus(fmt.Sprintf("Started Copilot in %s (%s %s). Press v to dock it.", where, tmux.PoolSession, pane))
	return parkAfterLaunchCmd(pane)
}

// copilotCommand is the shell line typed into the new window. Typing it,
// rather than running it as the window's command, loads the user's shell
// setup and leaves a shell behind when Copilot exits.
func copilotCommand(id, name, task string) string {
	cmd := "copilot --session-id " + id
	if name != "" {
		cmd += " --name " + shellQuote(name)
	}
	if task != "" {
		cmd += " -i " + shellQuote(task)
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func windowName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	w := strings.Trim(b.String(), "-")
	if len(w) > 20 {
		w = strings.Trim(w[:20], "-")
	}
	if w == "" {
		return "copilot"
	}
	return w
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// ---- typing to an agent ----

func (m *Model) startReply() tea.Cmd {
	if m.opt.Type == nil {
		m.setErr(fmt.Errorf("typing to sessions needs tmux"))
		return nil
	}
	s, ok := m.selected()
	if !ok {
		m.setErr(fmt.Errorf("select a session first"))
		return nil
	}
	if s.Offline {
		m.setStatus(s.Title() + " is offline. Press v to dock and resume it, or enter to open it.")
		return nil
	}
	m.replyPane, m.replyTitle, m.screen, m.screenErr = s.Pane.PaneID, s.Title(), "", nil
	m.screenGen++
	m.status = ""
	m.startInput(inReply, "", "", s.Key, "")
	return m.captureScreen()
}

func (m Model) captureScreen() tea.Cmd {
	if m.opt.Screen == nil || m.replyPane == "" {
		return nil
	}
	pane, f, gen := m.replyPane, m.opt.Screen, m.screenGen
	return func() tea.Msg {
		text, err := f(pane)
		return screenMsg{pane, text, err, gen}
	}
}

func (m *Model) gotScreen(msg screenMsg) {
	if msg.pane != m.replyPane {
		return
	}
	m.screen, m.screenErr = msg.text, msg.err
}

func (m Model) replying() bool { return m.mode == modeInput && m.inputKind == inReply }

// sendReply types text into the agent's pane and presses Enter.
func (m *Model) sendReply(text string) {
	s, ok := m.sessionByKey(m.inputKey)
	if !ok {
		m.setErr(fmt.Errorf("%s has closed", m.replyTitle))
		return
	}
	if err := m.opt.Type(m.replyPane, text); err != nil {
		m.setErr(err)
		return
	}
	if text != "" && s.Held {
		m.change(func(st *store.State) error {
			st.ClearHold(s.Key)
			return nil
		})
	}
	if text == "" {
		m.setStatus("Pressed Enter in " + m.replyTitle)
	} else {
		m.setStatus("Sent to " + m.replyTitle)
	}
}

// viewScreen shows the bottom of the agent's terminal while typing to it.
func (m Model) viewScreen(w, h int) []string {
	lines := []string{"", sKicker.Render("TYPING TO") + "  " + sTitle.Render(m.replyTitle),
		sMuted.Render("Live view of its terminal. enter sends your text and presses Enter; empty enter only presses Enter. esc cancels."), rule(w)}
	if m.status != "" {
		st := sGreen
		if m.statusErr {
			st = sRed
		}
		lines = append(lines[:3], st.Render(m.status), lines[3])
	}
	switch {
	case m.screenErr != nil:
		return append(lines, sRed.Render("Could not read the terminal: "+m.screenErr.Error()))
	case m.screen == "":
		return append(lines, sMuted.Render("Reading the terminal…"))
	}
	// Full-screen agents pad between the conversation and the input box.
	// Collapse blank runs so the conversation stays in view.
	var text []string
	for _, t := range strings.Split(strings.TrimRight(ansi.Strip(m.screen), "\n \t"), "\n") {
		t = strings.TrimRight(t, " \t")
		if t == "" && len(text) > 0 && text[len(text)-1] == "" {
			continue
		}
		text = append(text, t)
	}
	if room := h - len(lines); len(text) > room {
		text = text[len(text)-max(room, 0):]
	}
	for _, t := range text {
		lines = append(lines, sText.Render(ansi.Truncate(strings.ReplaceAll(t, "\t", "    "), w, "")))
	}
	return lines
}
