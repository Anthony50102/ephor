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
	Project     string
	Purpose     string
	PrevPurpose string   // previously inferred purpose, when Purpose is empty
	Threads     []string // named threads in the project
	Sessions    []SessionInput
	Cloud       []string // resources the sessions' commands named, most recent first
}

// SessionInput describes one agent session.
type SessionInput struct {
	Key        string
	Title      string
	Thread     string
	Kind       string
	State      string
	Reason     string
	Branch     string
	Updated    time.Time
	Arc        []string // chronological milestone titles across the session's life
	FirstAsk   string   // opening prompt when no milestone arc exists
	Checkpoint string   // latest checkpoint summary when recent asks/results scrolled away
	Asks       []string // newest first
	Results    []string // newest first
	Said       string
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

// Fingerprint changes when anything a summary depends on changes. Times and
// PrevPurpose are left out so aging or refining a purpose does not trigger a
// second rewrite.
func Fingerprint(in Input) string {
	c := in
	c.PrevPurpose = ""
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

- purpose: if the project has no stated purpose, write one or two concrete sentences describing the overarching vision, domain, and long-term direction of the WHOLE project across ALL its threads and session histories. Name the actual subject matter (what kind of models, system, or product is being built and to what end) rather than vague boilerplate like "building training, serving, and infrastructure for X". Never narrow it to a single thread's temporary task or let one utility/side thread define a broader project when other threads cover a different scope, and never just repeat the project name. If a PREVIOUSLY INFERRED PURPOSE is given, keep its breadth when it is already specific and accurate, or replace it with a richer, more concrete vision when the threads and histories show what the project is actually building. If the project already has a STATED PURPOSE, return "".
- headline: a high-level status headline in six to twelve words covering the WHOLE project across its active threads or sessions, never just whichever single session moved most recently or has the longest transcript. When multiple threads or sessions are active, summarize them in parallel clauses separated by semicolons. Example: "SFT pipeline scaling; infra PR ready; serving hunting capacity" (no trailing period needed for a single phrase).
- now: two or three plain sentences synthesizing where the WHOLE project stands across ALL its threads and sessions. CRITICAL FOR MULTI-SESSION PROJECTS: give every active or recently active thread/session equal altitude, and spend at most ONE sentence on any single session. Never let one thread's recent details, bug fixes, or verbose transcript crowd out the other threads. If a session is waiting on the user, mention that clearly without reducing the project to just that blocker. Prefer outcomes and direction over mechanics. Avoid file names, function names, and jargon unless essential. Present tense. Do not use em dashes.
- next: one short sentence on the project's next meaningful step, if the facts make it clear. Otherwise "".
- In "headline", "now", "next", and "purpose", never write session labels like S1. The reader cannot see them. Describe the work instead, or name a thread or session title if helpful.
- sessions: for every session label, at most twelve words on what it is doing or just finished.
- Mention cloud resources only when they explain what is happening, for example a training job on a cluster or a VM that was just created.
- Use only the facts below. If they are thin, say less rather than guess.

`)
	fmt.Fprintf(&b, "PROJECT: %s\n", in.Project)
	if len(in.Threads) > 0 {
		fmt.Fprintf(&b, "THREADS: %s\n", strings.Join(in.Threads, ", "))
	}
	if in.Purpose != "" {
		fmt.Fprintf(&b, "STATED PURPOSE: %s\n", in.Purpose)
	} else {
		b.WriteString("STATED PURPOSE: none\n")
		if prev := cleanPurpose(in.PrevPurpose, in.Project); prev != "" {
			fmt.Fprintf(&b, "PREVIOUSLY INFERRED PURPOSE: %s\n", prev)
		}
	}
	if len(in.Cloud) > 0 {
		b.WriteString("CLOUD RESOURCES (named in commands the agents ran):\n")
		for _, c := range in.Cloud {
			fmt.Fprintf(&b, "  - %s\n", c)
		}
	}
	// Balance per-session prompt budget on multi-session projects so one
	// verbose session cannot dominate the model's attention.
	askLimit, askChars, resLimit, resChars, saidChars, ckChars := 4, maxText, 2, maxText, maxText*2, maxText
	if len(in.Sessions) > 1 {
		askLimit, askChars, resLimit, resChars, saidChars, ckChars = 2, 320, 1, 400, 450, 450
	}
	for i, s := range in.Sessions {
		if i == maxSessions {
			break
		}
		meta := s.Kind
		if s.Thread != "" {
			meta = "thread: " + s.Thread + ", " + meta
		}
		fmt.Fprintf(&b, "\nS%d: %q (%s)\n", i+1, s.Title, meta)
		state := s.State
		if s.Reason != "" {
			state += ": " + s.Reason
		}
		fmt.Fprintf(&b, "  state: %s, last activity %s\n", state, ago(now, s.Updated))
		if s.Branch != "" {
			fmt.Fprintf(&b, "  branch: %s\n", s.Branch)
		}
		if len(s.Arc) > 0 {
			fmt.Fprintf(&b, "  history: %s\n", strings.Join(s.Arc, " -> "))
		} else if s.FirstAsk != "" {
			fmt.Fprintf(&b, "  started with: %s\n", clip(s.FirstAsk, askChars))
		}
		if s.Checkpoint != "" {
			fmt.Fprintf(&b, "  latest checkpoint: %s\n", clip(s.Checkpoint, ckChars))
		}
		for j, a := range s.Asks {
			if j == askLimit {
				break
			}
			label := "earlier ask"
			if j == 0 {
				label = "latest ask"
			}
			fmt.Fprintf(&b, "  %s: %s\n", label, clip(a, askChars))
		}
		for j, r := range s.Results {
			if j == resLimit {
				break
			}
			fmt.Fprintf(&b, "  finished: %s\n", clip(r, resChars))
		}
		if s.Said != "" {
			fmt.Fprintf(&b, "  agent's latest message: %s\n", clip(s.Said, saidChars))
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
		s.Purpose = cleanPurpose(unlabel(tidy(raw.Purpose), in), in.Project)
		if s.Purpose == "" {
			s.Purpose = cleanPurpose(in.PrevPurpose, in.Project)
		}
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

// cleanPurpose drops a tautological purpose that merely repeats the project
// name (for example "Harness Development." for project "Harness Development").
func cleanPurpose(purpose, project string) string {
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		return ""
	}
	norm := func(s string) string {
		return strings.ToLower(strings.Trim(strings.TrimSpace(s), ".!?:;\"'"))
	}
	if p := norm(project); p != "" && norm(purpose) == p {
		return ""
	}
	return purpose
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
