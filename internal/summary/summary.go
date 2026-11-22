// Package summary writes plain-language status notes for projects by asking a
// language model, and caches them on disk.
package summary

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Input is everything the model sees about one project.
type Input struct {
	Project  string
	Purpose  string
	Sessions []SessionInput
	Cloud    []string // resources the sessions' commands named, most recent first
}

// SessionInput describes one agent session.
type SessionInput struct {
	Key     string
	Title   string
	Kind    string
	State   string
	Reason  string
	Branch  string
	Updated time.Time
	Asks    []string // newest first
	Results []string // newest first
	Said    string
}

// Summary is one generated note.
type Summary struct {
	Headline string            `json:"headline,omitempty"` // the gist in a few words
	Now      string            `json:"now"`
	Next     string            `json:"next,omitempty"`     // the next step, if clear
	Purpose  string            `json:"purpose,omitempty"`  // suggested when the user wrote none
	Sessions map[string]string `json:"sessions,omitempty"` // session key to one line
	At       time.Time         `json:"at"`
	Input    string            `json:"input"`           // fingerprint of what it was written from
	Err      string            `json:"err,omitempty"`   // the last attempt failed; the text above is older
	Tried    time.Time         `json:"tried,omitempty"` // when that failed attempt ran
}

const (
	maxSessions = 12
	maxText     = 600
)

// Fingerprint changes when anything a summary depends on changes. Times are
// left out so a session that only ages does not trigger a rewrite.
func Fingerprint(in Input) string {
	c := in
	c.Sessions = make([]SessionInput, len(in.Sessions))
	for i, s := range in.Sessions {
		s.Updated = time.Time{}
		c.Sessions[i] = s
	}
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func ago(now, t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := now.Sub(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// Prompt builds the model prompt. Sessions are labelled S1, S2, … so the
// reply can refer to them compactly.
func Prompt(in Input, now time.Time) string {
	var b strings.Builder
	b.WriteString(`You write status notes for a dashboard that tracks AI coding agents working on a project.
The reader knows what the project is about at a high level, like a manager, but not the code.

Reply with only a JSON object, no code fence:
{"headline": "...", "now": "...", "next": "...", "purpose": "...", "sessions": {"S1": "...", "S2": "..."}}

- headline: the gist in five to ten words, like a news headline, for example "Training data is ready. Config decision pending." No trailing period needed for a single phrase.

- now: two or three plain sentences. Say what the work is trying to achieve right now and where it stands. If a session is waiting on the user, say that first and say what it needs. Prefer outcomes over mechanics. Avoid file names, function names, and jargon unless they are essential. Present tense. Do not use em dashes.
- next: one short sentence on the next step, if the facts make it clear. Otherwise "".
- purpose: if the project has no stated purpose, one sentence on what the project is for. Otherwise "".
- In "headline", "now", "next", and "purpose", never write session labels like S1. The reader cannot see them. Describe the work instead, or name a session by its quoted title if you must.
- sessions: for every session label, at most twelve words on what it is doing or just finished.
- Mention cloud resources only when they explain what is happening, for example a training job on a cluster or a VM that was just created.
- Use only the facts below. If they are thin, say less rather than guess.

`)
	fmt.Fprintf(&b, "PROJECT: %s\n", in.Project)
	if in.Purpose != "" {
		fmt.Fprintf(&b, "STATED PURPOSE: %s\n", in.Purpose)
	} else {
		b.WriteString("STATED PURPOSE: none\n")
	}
	if len(in.Cloud) > 0 {
		b.WriteString("CLOUD RESOURCES (named in commands the agents ran):\n")
		for _, c := range in.Cloud {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	for i, s := range in.Sessions {
		if i == maxSessions {
			break
		}
		fmt.Fprintf(&b, "\nS%d: %q (%s)\n", i+1, s.Title, s.Kind)
		state := s.State
		if s.Reason != "" {
			state += ": " + s.Reason
		}
		fmt.Fprintf(&b, "  state: %s, last activity %s\n", state, ago(now, s.Updated))
		if s.Branch != "" {
			fmt.Fprintf(&b, "  branch: %s\n", s.Branch)
		}
		for j, a := range s.Asks {
			label := "earlier ask"
			if j == 0 {
				label = "latest ask"
			}
			fmt.Fprintf(&b, "  %s: %s\n", label, clip(a, maxText))
		}
		for j, r := range s.Results {
			if j == 2 {
				break
			}
			fmt.Fprintf(&b, "  finished: %s\n", clip(r, maxText))
		}
		if s.Said != "" {
			fmt.Fprintf(&b, "  agent's latest message: %s\n", clip(s.Said, maxText*2))
		}
	}
	return b.String()
}

// Parse reads the model reply, tolerating text or a code fence around the
// JSON, and maps S1, S2, … back to session keys.
func Parse(out string, in Input) (Summary, error) {
	i, j := strings.Index(out, "{"), strings.LastIndex(out, "}")
	if i < 0 || j < i {
		return Summary{}, fmt.Errorf("no JSON in reply: %s", clip(out, 120))
	}
	var raw struct {
		Headline string            `json:"headline"`
		Now      string            `json:"now"`
		Next     string            `json:"next"`
		Purpose  string            `json:"purpose"`
		Sessions map[string]string `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(out[i:j+1]), &raw); err != nil {
		return Summary{}, fmt.Errorf("bad JSON in reply: %w", err)
	}
	if strings.TrimSpace(raw.Now) == "" {
		return Summary{}, errors.New("reply had no summary")
	}
	s := Summary{Headline: unlabel(tidy(raw.Headline), in), Now: unlabel(tidy(raw.Now), in),
		Next: unlabel(tidy(raw.Next), in), Sessions: map[string]string{}}
	if in.Purpose == "" {
		s.Purpose = unlabel(tidy(raw.Purpose), in)
	}
	for k, v := range raw.Sessions {
		var n int
		if _, err := fmt.Sscanf(k, "S%d", &n); err == nil && n >= 1 && n <= len(in.Sessions) && strings.TrimSpace(v) != "" {
			s.Sessions[in.Sessions[n-1].Key] = tidy(v)
		}
	}
	return s, nil
}

var labelRE = regexp.MustCompile(`\bS(\d+)\b`)

// unlabel swaps any S1-style label the model leaked for the session title.
func unlabel(s string, in Input) string {
	return labelRE.ReplaceAllStringFunc(s, func(m string) string {
		var n int
		if _, err := fmt.Sscanf(m, "S%d", &n); err == nil && n >= 1 && n <= len(in.Sessions) {
			return `"` + in.Sessions[n-1].Title + `"`
		}
		return m
	})
}

// tidy enforces the house style regardless of what the model did.
func tidy(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, " — ", " - ")
	return strings.ReplaceAll(s, "—", "-")
}

// Runner sends a prompt to a model and returns its reply.
type Runner func(ctx context.Context, prompt string) (string, error)

// Copilot runs the GitHub Copilot CLI non-interactively with no tools. The
// prompt goes over stdin so it does not appear in the process list.
func Copilot(model, dir string) Runner {
	args := []string{"-s", "--no-custom-instructions", "--no-ask-user", "--available-tools="}
	if model != "" {
		args = append(args, "--model", model)
	}
	return command(dir, "copilot", args...)
}

// Shell runs a user-supplied command through sh with the prompt on stdin.
func Shell(cmdline, dir string) Runner {
	return command(dir, "sh", "-c", cmdline)
}

func command(dir, name string, args ...string) Runner {
	return func(ctx context.Context, prompt string) (string, error) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", err
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		// Kill the whole process group on timeout, and stop waiting for pipes
		// that orphaned grandchildren may still hold open.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		cmd.Stdin = strings.NewReader(prompt)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if msg == "" {
				msg = strings.TrimSpace(out.String())
			}
			return "", fmt.Errorf("%s: %w: %s", name, err, clip(msg, 200))
		}
		return out.String(), nil
	}
}

// Cache holds the latest summary per project ID. It can always be rebuilt,
// so a damaged file is simply replaced.
type Cache struct {
	Projects map[string]Summary `json:"projects"`
}

// LoadCache reads the cache, returning an empty one when it is missing or unreadable.
func LoadCache(path string) *Cache {
	c := &Cache{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, c)
	}
	if c.Projects == nil {
		c.Projects = map[string]Summary{}
	}
	return c
}

// Save writes the cache atomically.
func (c *Cache) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".summaries-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
