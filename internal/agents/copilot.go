package agents

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Copilot reads GitHub Copilot CLI session directories.
type Copilot struct {
	Dir string // usually ~/.copilot/session-state

	mu    sync.Mutex
	cache map[string]copilotCache
}

type copilotCache struct {
	size    int64
	mod     time.Time
	wsMod   time.Time
	ckMod   time.Time
	content Info
}

// DefaultCopilotDir is ~/.copilot/session-state, or $COPILOT_HOME/session-state.
func DefaultCopilotDir() string {
	if h := os.Getenv("COPILOT_HOME"); h != "" {
		return filepath.Join(h, "session-state")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".copilot", "session-state")
}

// Locks maps a live Copilot process ID to its session ID using inuse.<pid>.lock files.
func (c *Copilot) Locks() map[int]string {
	out := map[int]string{}
	matches, _ := filepath.Glob(filepath.Join(c.Dir, "*", "inuse.*.lock"))
	for _, m := range matches {
		base := filepath.Base(m)
		pid, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(base, "inuse."), ".lock"))
		if err != nil {
			continue
		}
		out[pid] = filepath.Base(filepath.Dir(m))
	}
	return out
}

// Read returns the current view of one session. Results are cached until the
// event log or workspace file changes.
func (c *Copilot) Read(id string) (Info, error) {
	dir := filepath.Join(c.Dir, id)
	events := filepath.Join(dir, "events.jsonl")
	ws := filepath.Join(dir, "workspace.yaml")
	ck := filepath.Join(dir, "checkpoints", "index.md")
	var size int64
	var mod, wsMod, ckMod time.Time
	if st, err := os.Stat(events); err == nil {
		size, mod = st.Size(), st.ModTime()
	}
	if st, err := os.Stat(ws); err == nil {
		wsMod = st.ModTime()
	}
	if st, err := os.Stat(ck); err == nil {
		ckMod = st.ModTime()
	}
	c.mu.Lock()
	if hit, ok := c.cache[id]; ok && hit.size == size && hit.mod.Equal(mod) && hit.wsMod.Equal(wsMod) && hit.ckMod.Equal(ckMod) {
		c.mu.Unlock()
		return hit.content, nil
	}
	c.mu.Unlock()

	info := Info{Kind: "copilot", ID: id, Log: events}
	meta := readYAMLish(ws)
	info.Name = meta["name"]
	info.Cwd = meta["cwd"]
	info.Repo = meta["repository"]
	info.Branch = meta["branch"]
	if t, err := time.Parse(time.RFC3339, meta["updated_at"]); err == nil {
		info.UpdatedAt = t
	}
	var latestCk string
	info.Arc, latestCk = readCheckpoints(ck)
	if size > 0 {
		if err := readCopilotEvents(events, &info); err != nil {
			return info, err
		}
		if len(info.Arc) == 0 {
			info.FirstAsk = firstCopilotAsk(events, info.Asks)
		}
		if latestCk != "" && len(info.Asks) == 0 && len(info.Results) == 0 {
			info.Checkpoint = readCheckpointSummary(filepath.Join(filepath.Dir(ck), latestCk))
		}
	}

	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]copilotCache{}
	}
	c.cache[id] = copilotCache{size: size, mod: mod, wsMod: wsMod, ckMod: ckMod, content: info}
	c.mu.Unlock()
	return info, nil
}

type copilotEvent struct {
	Type      string    `json:"type"`
	AgentID   string    `json:"agentId"`
	Timestamp time.Time `json:"timestamp"`
	Data      struct {
		ID         int    `json:"id"`
		IntervalMs int64  `json:"intervalMs"`
		Recurring  *bool  `json:"recurring"`
		Content    string `json:"content"`
		Summary    string `json:"summary"`
		RequestID  string `json:"requestId"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Kind       struct {
			Type    string `json:"type"`
			ShellID string `json:"shellId"`
		} `json:"kind"`
		Result struct {
			Content string `json:"content"`
		} `json:"result"`
		Arguments struct {
			Message      string  `json:"message"`
			Question     string  `json:"question"`
			Action       string  `json:"action"`
			ID           int     `json:"id"`
			DelaySeconds float64 `json:"delaySeconds"`
			ShellID      string  `json:"shellId"`
		} `json:"arguments"`
		PermissionRequest struct {
			Kind            string `json:"kind"`
			Intention       string `json:"intention"`
			FullCommandText string `json:"fullCommandText"`
			Path            string `json:"path"`
		} `json:"permissionRequest"`
	} `json:"data"`
}

// Event types that decide state. Everything else is skipped without decoding.
var copilotStateTypes = map[string]bool{
	"permission.requested": true, "permission.completed": true,
	"tool.execution_start": true, "tool.execution_complete": true,
	"assistant.turn_start": true, "assistant.turn_end": true, "assistant.message": true,
	"user.message": true, "session.task_complete": true, "abort": true,
	"session.shutdown": true, "subagent.started": true,
	"session.start": true, "session.resume": true,
	"session.schedule_created": true, "session.schedule_cancelled": true,
}

// Event types that represent real conversation or agent work and should update
// UpdatedAt (and auto-wake a held session). Lifecycle bookkeeping events like
// session.resume, session.shutdown, and session.permission_grants_recorded are
// intentionally excluded so restarting or resuming a held session keeps it on
// hold until actual new work happens.
var copilotActivityTypes = map[string]bool{
	"permission.requested": true, "permission.completed": true,
	"tool.execution_start": true, "tool.execution_complete": true,
	"assistant.turn_start": true, "assistant.turn_end": true, "assistant.message": true,
	"user.message": true, "session.task_complete": true, "abort": true,
	"subagent.started": true, "subagent.completed": true,
	"session.schedule_created": true, "session.schedule_cancelled": true,
}

func readCopilotEvents(path string, info *Info) error {
	decided := false
	completed := map[string]bool{}
	schedCancelled := map[int]bool{}
	schedFired := map[int]int{}
	activeSchedID := 0
	activeSchedLabel := ""
	firedActiveID := 0
	bgDone := map[string]bool{}
	bgActive := false
	bgClosed := false
	var workAt time.Time
	var fallbackAt time.Time
	err := scanBackward(path, maxScanBytes, func(line []byte) bool {
		typ := eventType(line)
		isSched := typ == "session.schedule_created" || typ == "session.schedule_cancelled"
		isWakeup := typ == "tool.execution_start" && bytes.Contains(line, []byte(`"manage_schedule"`))
		isBg := !bgClosed && ((typ == "system.notification" && bytes.Contains(line, []byte(`"shell_completed"`))) ||
			(typ == "tool.execution_start" && bytes.Contains(line, []byte(`"stop_bash"`))) ||
			(typ == "tool.execution_complete" && (bytes.Contains(line, []byte(`is still running`)) || bytes.Contains(line, []byte(`started in background`)) || bytes.Contains(line, []byte(`completed with exit code`)) || bytes.Contains(line, []byte(`has been stopped`)) || bytes.Contains(line, []byte(`stopped>`)))) ||
			typ == "session.resume" || typ == "session.shutdown")
		needWorkAt := workAt.IsZero() && copilotActivityTypes[typ]
		if decided && !needWorkAt && !isSched && !isWakeup && !isBg && typ != "user.message" && typ != "session.task_complete" && !(typ == "assistant.message" && info.Said == "") {
			return true
		}
		if !decided && !copilotStateTypes[typ] && !isBg && typ != "" {
			return true
		}
		var e copilotEvent
		if json.Unmarshal(line, &e) != nil {
			return true
		}
		if copilotActivityTypes[e.Type] {
			if e.Timestamp.After(workAt) {
				workAt = e.Timestamp
			}
		} else if e.Type == "session.start" && e.Timestamp.After(fallbackAt) {
			fallbackAt = e.Timestamp
		}
		main := e.AgentID == ""
		switch e.Type {
		case "user.message":
			if main {
				bgClosed = true
				info.noteAsk(e.Data.Content, e.Timestamp)
				if id, ok := parseScheduledPromptID(e.Data.Content); ok {
					schedFired[id]++
					if !schedCancelled[id] && firedActiveID == 0 {
						firedActiveID = id
					}
				}
			}
		case "assistant.message":
			if main {
				info.noteSaid(e.Data.Content, e.Timestamp)
			}
		case "session.task_complete":
			if main {
				bgClosed = true
				if len(info.Results) < maxResults && strings.TrimSpace(e.Data.Summary) != "" {
					info.Results = append(info.Results, Result{At: e.Timestamp, Text: strings.TrimSpace(e.Data.Summary)})
				}
			}
		case "session.schedule_cancelled":
			if e.Data.ID > 0 {
				schedCancelled[e.Data.ID] = true
			}
		case "session.schedule_created":
			if id := e.Data.ID; id > 0 && !schedCancelled[id] {
				oneShot := e.Data.Recurring != nil && !*e.Data.Recurring
				if oneShot && schedFired[id] > 0 {
					// A one-shot schedule that already fired is done.
					schedCancelled[id] = true
					if firedActiveID == id {
						firedActiveID = 0
					}
				} else if activeSchedID == 0 {
					activeSchedID = id
					activeSchedLabel = formatScheduleInterval(e.Data.IntervalMs)
				}
			}
		case "session.resume", "session.shutdown":
			// Attached background shells do not survive a process restart.
			bgClosed = true
		case "system.notification":
			if !bgClosed && e.Data.Kind.Type == "shell_completed" && e.Data.Kind.ShellID != "" {
				bgDone[e.Data.Kind.ShellID] = true
			}
		case "tool.execution_complete":
			if main && !bgClosed {
				if id, ok := parseCompletedShellID(e.Data.Result.Content); ok {
					bgDone[id] = true
				} else if id, ok := parseBgShellID(e.Data.Result.Content); ok && !bgDone[id] {
					bgActive = true
				}
			}
		case "tool.execution_start":
			if !bgClosed && e.Data.ToolName == "stop_bash" && e.Data.Arguments.ShellID != "" {
				bgDone[e.Data.Arguments.ShellID] = true
			}
			if e.Data.ToolName == "manage_schedule" && e.Data.Arguments.Action == "wakeup" {
				if id := e.Data.Arguments.ID; id > 0 && !schedCancelled[id] && schedFired[id] == 0 && activeSchedID == 0 {
					activeSchedID = id
					activeSchedLabel = formatScheduleInterval(int64(e.Data.Arguments.DelaySeconds * 1000))
				}
			}
		}
		if !decided {
			decided = decideCopilot(e, main, completed, info)
		}
		return !(decided && !workAt.IsZero() && len(info.Asks) >= maxAsks && info.Said != "" && len(info.Results) >= maxResults)
	})
	if !workAt.IsZero() {
		info.UpdatedAt = workAt
	} else if !fallbackAt.IsZero() && fallbackAt.After(info.UpdatedAt) {
		info.UpdatedAt = fallbackAt
	}
	if activeSchedID == 0 && firedActiveID > 0 && !schedCancelled[firedActiveID] {
		activeSchedID = firedActiveID
		activeSchedLabel = "scheduled"
	}
	if activeSchedID > 0 {
		info.Scheduled = activeSchedLabel
		// A session idling between scheduled check-ins is still running an
		// autonomous loop, not finished and waiting for the user.
		if info.State == Ready {
			info.State = Working
		}
	} else if bgActive {
		info.Scheduled = "background"
		if info.State == Ready {
			info.State = Working
		}
	}
	return err
}

// decideCopilot applies one event, newest first, and reports whether the state is known.
func decideCopilot(e copilotEvent, main bool, completed map[string]bool, info *Info) bool {
	switch e.Type {
	case "permission.completed":
		completed["perm:"+e.Data.RequestID] = true
	case "tool.execution_complete":
		completed["tool:"+e.Data.ToolCallID] = true
	case "permission.requested":
		if completed["perm:"+e.Data.RequestID] {
			return false
		}
		info.State = NeedsYou
		pr := e.Data.PermissionRequest
		what := firstNonEmpty(pr.Intention, pr.FullCommandText, pr.Path, pr.Kind)
		info.Reason = "Approve: " + oneLine(what, 160)
		return true
	case "tool.execution_start":
		if e.Data.ToolName == "ask_user" && !completed["tool:"+e.Data.ToolCallID] {
			info.State = NeedsYou
			info.Reason = "Question: " + oneLine(firstNonEmpty(e.Data.Arguments.Message, e.Data.Arguments.Question, "waiting for your answer"), 200)
			return true
		}
		info.State = Working
		return true
	case "assistant.turn_end", "session.task_complete", "abort":
		if !main {
			info.State = Working
			return true
		}
		info.State = Ready
		return true
	case "session.shutdown":
		info.State = Ended
		return true
	case "session.start", "session.resume":
		// A fresh or resumed session idles at the prompt until the user types.
		info.State = Ready
		return true
	case "assistant.turn_start", "assistant.message", "user.message", "subagent.started":
		info.State = Working
		return true
	}
	return false
}

// parseScheduledPromptID extracts the schedule ID from Copilot CLI's
// "[Scheduled prompt #<id>]" prefix on timer-fired user messages.
func parseScheduledPromptID(content string) (int, bool) {
	const prefix = "[Scheduled prompt #"
	if !strings.HasPrefix(content, prefix) {
		return 0, false
	}
	rest := content[len(prefix):]
	end := strings.IndexByte(rest, ']')
	if end <= 0 {
		return 0, false
	}
	id, err := strconv.Atoi(rest[:end])
	return id, err == nil && id > 0
}

func formatScheduleInterval(ms int64) string {
	if ms <= 0 {
		return "scheduled"
	}
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d%time.Hour == 0 && d >= time.Hour:
		return "every " + strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	case d%time.Minute == 0 && d >= time.Minute:
		return "every " + strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	case d >= time.Second:
		return "every " + strconv.FormatInt(int64(d/time.Second), 10) + "s"
	default:
		return "scheduled"
	}
}

// lastFooter returns the final trimmed line of content when it is an
// angle-bracketed Copilot CLI bash tool footer (e.g. "<shellId: 12 ...>"),
// preventing subagent markdown reports or prose from matching shell patterns.
func lastFooter(content string) string {
	s := strings.TrimSpace(content)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[i+1:])
	}
	if !strings.HasPrefix(s, "<") || !strings.HasSuffix(s, ">") {
		return ""
	}
	return s
}

func extractFooterToken(footer, prefix string) (string, bool) {
	if !strings.HasPrefix(footer, prefix) {
		return "", false
	}
	rest := footer[len(prefix):]
	if end := strings.IndexAny(rest, " >\t\r\n"); end >= 0 {
		rest = rest[:end]
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

// parseCompletedShellID extracts the shellId when a background shell finishes
// via read_bash or stop_bash ("<shellId: 123 completed with exit code 0>" or
// "<command with id: 123 stopped>").
func parseCompletedShellID(content string) (string, bool) {
	footer := lastFooter(content)
	if footer == "" {
		return "", false
	}
	if strings.HasPrefix(footer, "<shellId: ") && (strings.Contains(footer, " completed with exit code ") || strings.Contains(footer, " has been stopped")) {
		return extractFooterToken(footer, "<shellId: ")
	}
	if strings.HasPrefix(footer, "<command with id: ") && strings.HasSuffix(footer, " stopped>") {
		return extractFooterToken(footer, "<command with id: ")
	}
	return "", false
}

// parseBgShellID extracts the shellId from Copilot CLI's background bash tool
// footer when a command continues running asynchronously after the turn ends.
func parseBgShellID(content string) (string, bool) {
	footer := lastFooter(content)
	if footer == "" {
		return "", false
	}
	if strings.HasPrefix(footer, "<command with shellId: ") && strings.Contains(footer, " is still running") {
		return extractFooterToken(footer, "<command with shellId: ")
	}
	if strings.HasPrefix(footer, "<command started in background with shellId: ") {
		return extractFooterToken(footer, "<command started in background with shellId: ")
	}
	return "", false
}

// readCheckpoints parses Copilot CLI's checkpoints/index.md table and returns
// a sampled chronological slice of milestone titles plus the newest checkpoint filename.
func readCheckpoints(path string) ([]string, string) {
	f, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer f.Close()
	var titles []string
	var latest string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 4 {
			continue
		}
		if _, err := strconv.Atoi(strings.TrimSpace(cols[1])); err != nil {
			continue
		}
		if title := oneLine(cols[2], 100); title != "" {
			titles = append(titles, title)
		}
		if file := strings.TrimSpace(cols[3]); file != "" && !strings.Contains(file, "/") && !strings.Contains(file, "..") {
			latest = file
		}
	}
	return sampleArc(titles, maxArc), latest
}

// readCheckpointSummary extracts a concise status excerpt from a Copilot
// checkpoint markdown file when recent asks/results have scrolled out of the
// event log scan window.
func readCheckpointSummary(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	txt := string(b)
	section := func(tag string) string {
		open, closeTag := "<"+tag+">", "</"+tag+">"
		i := strings.Index(txt, open)
		if i < 0 {
			return ""
		}
		rest := txt[i+len(open):]
		if j := strings.Index(rest, closeTag); j >= 0 {
			rest = rest[:j]
		}
		return oneLine(rest, 320)
	}
	var parts []string
	if s := section("work_done"); s != "" {
		parts = append(parts, "Recent work: "+s)
	}
	if s := section("next_steps"); s != "" {
		parts = append(parts, "Next: "+s)
	}
	if len(parts) == 0 {
		if s := section("overview"); s != "" {
			parts = append(parts, s)
		}
	}
	return oneLine(strings.Join(parts, " "), 600)
}

// firstCopilotAsk grabs the session's opening user prompt when it is older than
// the recent asks captured from the tail of events.jsonl.
func firstCopilotAsk(path string, recent []Result) string {
	var first string
	_ = scanForward(path, maxHeadBytes, func(line []byte) bool {
		if eventType(line) != "user.message" {
			return true
		}
		var e copilotEvent
		if json.Unmarshal(line, &e) != nil || e.AgentID != "" {
			return true
		}
		if s := oneLine(e.Data.Content, 400); s != "" {
			first = s
			return false
		}
		return true
	})
	if first == "" {
		return ""
	}
	for _, a := range recent {
		if a.Text == first {
			return ""
		}
	}
	return first
}

// readYAMLish reads flat `key: value` lines, which is all workspace.yaml uses.
func readYAMLish(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if uq, err := strconv.Unquote(v); err == nil {
			v = uq
		} else {
			v = strings.Trim(v, `'`)
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if strings.TrimSpace(x) != "" {
			return x
		}
	}
	return ""
}
