package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Claude reads Claude Code session files. Transcripts live in
// ~/.claude/projects/<encoded cwd>/<session>.jsonl. Some versions also record
// running sessions in ~/.claude/sessions/<pid>.json.
type Claude struct {
	Dir string // usually ~/.claude

	mu    sync.Mutex
	cache map[string]claudeCache
}

type claudeCache struct {
	size    int64
	mod     time.Time
	content Info
}

// DefaultClaudeDir is ~/.claude, or $CLAUDE_CONFIG_DIR.
func DefaultClaudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// ClaudeProc is the registration Claude Code writes for a running process.
type ClaudeProc struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
}

// Procs maps a Claude Code process ID to its registration.
func (c *Claude) Procs() map[int]ClaudeProc {
	out := map[int]ClaudeProc{}
	matches, _ := filepath.Glob(filepath.Join(c.Dir, "sessions", "*.json"))
	for _, m := range matches {
		pid, err := strconv.Atoi(strings.TrimSuffix(filepath.Base(m), ".json"))
		if err != nil {
			continue
		}
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var p ClaudeProc
		if json.Unmarshal(b, &p) == nil && p.SessionID != "" {
			out[pid] = p
		}
	}
	return out
}

// ProjectDir is where Claude Code keeps transcripts for sessions started in cwd.
// Claude replaces every character other than letters, digits, and '-' with '-'.
func (c *Claude) ProjectDir(cwd string) string {
	enc := []rune(cwd)
	for i, r := range enc {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
			enc[i] = '-'
		}
	}
	return filepath.Join(c.Dir, "projects", string(enc))
}

// Latest returns the newest transcript for cwd written at or after since.
// Callers must only use it when one Claude process runs in cwd, because the
// transcript cannot otherwise be tied to a process.
func (c *Claude) Latest(cwd string, since time.Time) (ClaudeProc, bool) {
	matches, _ := filepath.Glob(filepath.Join(c.ProjectDir(cwd), "*.jsonl"))
	var best string
	var bestMod time.Time
	for _, m := range matches {
		st, err := os.Stat(m)
		if err != nil || st.ModTime().Before(since) {
			continue
		}
		if st.ModTime().After(bestMod) {
			best, bestMod = m, st.ModTime()
		}
	}
	if best == "" {
		return ClaudeProc{}, false
	}
	return ClaudeProc{SessionID: strings.TrimSuffix(filepath.Base(best), ".jsonl"), Cwd: cwd}, true
}

// Read returns the current view of a Claude Code session.
func (c *Claude) Read(p ClaudeProc) (Info, error) {
	info := Info{Kind: "claude", ID: p.SessionID, Cwd: p.Cwd}
	matches, _ := filepath.Glob(filepath.Join(c.Dir, "projects", "*", p.SessionID+".jsonl"))
	if len(matches) == 0 {
		return info, nil
	}
	path := matches[0]
	info.Log = path
	st, err := os.Stat(path)
	if err != nil {
		return info, err
	}
	c.mu.Lock()
	if hit, ok := c.cache[p.SessionID]; ok && hit.size == st.Size() && hit.mod.Equal(st.ModTime()) {
		c.mu.Unlock()
		return hit.content, nil
	}
	c.mu.Unlock()

	info.UpdatedAt = st.ModTime()
	readClaudeTranscript(path, &info)

	c.mu.Lock()
	if c.cache == nil {
		c.cache = map[string]claudeCache{}
	}
	c.cache[p.SessionID] = claudeCache{size: st.Size(), mod: st.ModTime(), content: info}
	c.mu.Unlock()
	return info, nil
}

type claudeEntry struct {
	Type        string    `json:"type"`
	Timestamp   time.Time `json:"timestamp"`
	IsMeta      bool      `json:"isMeta"`
	IsSidechain bool      `json:"isSidechain"`
	GitBranch   string    `json:"gitBranch"`
	Cwd         string    `json:"cwd"`
	Summary     string    `json:"summary"`
	CustomTitle string    `json:"customTitle"`
	LastPrompt  string    `json:"lastPrompt"`
	Message     struct {
		StopReason string          `json:"stop_reason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func readClaudeTranscript(path string, info *Info) {
	decided, lastPrompt := false, false
	_ = scanBackward(path, maxScanBytes, func(line []byte) bool {
		var e claudeEntry
		if json.Unmarshal(line, &e) != nil || e.IsSidechain {
			return true
		}
		if info.Branch == "" && e.GitBranch != "" {
			info.Branch = e.GitBranch
		}
		if e.Cwd != "" && info.Cwd == "" {
			info.Cwd = e.Cwd
		}
		switch e.Type {
		case "custom-title":
			if info.Name == "" {
				info.Name = e.CustomTitle
			}
		case "summary":
			if info.Name == "" {
				info.Name = oneLine(e.Summary, 60)
			}
		case "last-prompt":
			if !lastPrompt && len(info.Asks) == 0 && strings.TrimSpace(e.LastPrompt) != "" {
				info.Ask, lastPrompt = oneLine(e.LastPrompt, 400), true
			}
		case "user":
			var s string
			isPrompt := json.Unmarshal(e.Message.Content, &s) == nil && !e.IsMeta && !strings.HasPrefix(s, "<")
			if isPrompt {
				if lastPrompt && len(info.Asks) == 0 {
					// last-prompt carries no time; prefer the real message.
					info.Ask = ""
				}
				info.noteAsk(s, e.Timestamp)
			}
			if !decided {
				info.State, decided = Working, true
			}
		case "assistant":
			var blocks []claudeBlock
			_ = json.Unmarshal(e.Message.Content, &blocks)
			var said []string
			for _, b := range blocks {
				if b.Type == "text" {
					said = append(said, b.Text)
				}
			}
			info.noteSaid(strings.Join(said, "\n"), e.Timestamp)
			// tool_use or no stop reason means the turn continues. Any other
			// reason (end_turn, stop_sequence, max_tokens, refusal) ends it.
			if r := e.Message.StopReason; r != "" && r != "tool_use" {
				var text []string
				for _, b := range blocks {
					if b.Type == "text" {
						text = append(text, b.Text)
					}
				}
				if t := strings.TrimSpace(strings.Join(text, "\n")); t != "" && len(info.Results) < maxResults {
					info.Results = append(info.Results, Result{At: e.Timestamp, Text: t})
				}
				if !decided {
					info.State, decided = Ready, true
				}
			} else if !decided {
				info.State, decided = Working, true
			}
		}
		return !(decided && len(info.Asks) >= maxAsks && info.Said != "" && len(info.Results) >= maxResults)
	})
}
