package agents

import (
	"bufio"
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
	var size int64
	var mod, wsMod time.Time
	if st, err := os.Stat(events); err == nil {
		size, mod = st.Size(), st.ModTime()
	}
	if st, err := os.Stat(ws); err == nil {
		wsMod = st.ModTime()
	}
	c.mu.Lock()
	if hit, ok := c.cache[id]; ok && hit.size == size && hit.mod.Equal(mod) && hit.wsMod.Equal(wsMod) {
		c.mu.Unlock()
		return hit.content, nil
	}
	c.mu.Unlock()

	info := Info{Kind: "copilot", ID: id}
	meta := readYAMLish(ws)
	info.Name = meta["name"]
	info.Cwd = meta["cwd"]
	info.Repo = meta["repository"]
	info.Branch = meta["branch"]
	if t, err := time.Parse(time.RFC3339, meta["updated_at"]); err == nil {
		info.UpdatedAt = t
	}
	if size > 0 {
		if err := readCopilotEvents(events, &info); err != nil {
			return info, err
		}
	}

	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]copilotCache{}
	}
	c.cache[id] = copilotCache{size: size, mod: mod, wsMod: wsMod, content: info}
	c.mu.Unlock()
	return info, nil
}

type copilotEvent struct {
	Type      string    `json:"type"`
	AgentID   string    `json:"agentId"`
	Timestamp time.Time `json:"timestamp"`
	Data      struct {
		Content    string `json:"content"`
		Summary    string `json:"summary"`
		RequestID  string `json:"requestId"`
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		Arguments  struct {
			Message  string `json:"message"`
			Question string `json:"question"`
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
}

func readCopilotEvents(path string, info *Info) error {
	decided := false
	completed := map[string]bool{}
	return scanBackward(path, maxScanBytes, func(line []byte) bool {
		typ := eventType(line)
		if decided && typ != "user.message" && typ != "session.task_complete" && !(typ == "assistant.message" && info.Said == "") {
			return true
		}
		if !decided && !copilotStateTypes[typ] && typ != "" {
			if info.UpdatedAt.IsZero() || len(line) < 4096 {
				noteTime(line, info)
			}
			return true
		}
		var e copilotEvent
		if json.Unmarshal(line, &e) != nil {
			return true
		}
		if e.Timestamp.After(info.UpdatedAt) {
			info.UpdatedAt = e.Timestamp
		}
		main := e.AgentID == ""
		switch e.Type {
		case "user.message":
			if main {
				info.noteAsk(e.Data.Content, e.Timestamp)
			}
		case "assistant.message":
			if main {
				info.noteSaid(e.Data.Content, e.Timestamp)
			}
		case "session.task_complete":
			if main && len(info.Results) < maxResults && strings.TrimSpace(e.Data.Summary) != "" {
				info.Results = append(info.Results, Result{At: e.Timestamp, Text: strings.TrimSpace(e.Data.Summary)})
			}
		}
		if !decided {
			decided = decideCopilot(e, main, completed, info)
		}
		return !(decided && len(info.Asks) >= maxAsks && info.Said != "" && len(info.Results) >= maxResults)
	})
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

func noteTime(line []byte, info *Info) {
	var e struct {
		Timestamp time.Time `json:"timestamp"`
	}
	if json.Unmarshal(line, &e) == nil && e.Timestamp.After(info.UpdatedAt) {
		info.UpdatedAt = e.Timestamp
	}
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
