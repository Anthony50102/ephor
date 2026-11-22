package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ev(typ, ts string, data string, extra ...string) string {
	e := fmt.Sprintf(`{"type":%q,"data":%s,"timestamp":%q`, typ, data, ts)
	for _, x := range extra {
		e += "," + x
	}
	return e + "}"
}

func writeSession(t *testing.T, dir, id string, lines ...string) {
	t.Helper()
	d := filepath.Join(dir, id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := "id: " + id + "\ncwd: /work/app\nrepository: me/app\nbranch: feature/x\nname: Fix login\nupdated_at: 2026-11-01T00:00:00Z\n"
	if err := os.WriteFile(filepath.Join(d, "workspace.yaml"), []byte(ws), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "events.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCopilotStates(t *testing.T) {
	const t0, t1, t2, t3 = "2026-11-20T10:00:00Z", "2026-11-20T10:01:00Z", "2026-11-20T10:02:00Z", "2026-11-20T10:03:00Z"
	user := ev("user.message", t0, `{"content":"Fix the login bug"}`)
	cases := []struct {
		name   string
		lines  []string
		state  State
		reason string
	}{
		{"finished turn is ready", []string{user,
			ev("assistant.turn_start", t1, `{}`),
			ev("session.task_complete", t2, `{"summary":"Login works again. Tests pass."}`),
			ev("assistant.turn_end", t2, `{}`),
			ev("session.usage_checkpoint", t3, `{}`)}, Ready, ""},
		{"pending permission needs you", []string{user,
			ev("tool.execution_start", t1, `{"toolCallId":"c1","toolName":"bash"}`),
			ev("permission.requested", t2, `{"requestId":"r1","permissionRequest":{"kind":"shell","intention":"Run the test suite"}}`)},
			NeedsYou, "Approve: Run the test suite"},
		{"approved permission is working", []string{user,
			ev("permission.requested", t1, `{"requestId":"r1","permissionRequest":{"kind":"shell"}}`),
			ev("permission.completed", t2, `{"requestId":"r1"}`)}, Working, ""},
		{"unanswered ask_user needs you", []string{user,
			ev("tool.execution_start", t1, `{"toolCallId":"q1","toolName":"ask_user","arguments":{"question":"Which database?"}}`)},
			NeedsYou, "Question: Which database?"},
		{"answered ask_user is working", []string{user,
			ev("tool.execution_start", t1, `{"toolCallId":"q1","toolName":"ask_user","arguments":{"question":"Which database?"}}`),
			ev("tool.execution_complete", t2, `{"toolCallId":"q1"}`)}, Working, ""},
		{"subagent abort does not end the turn", []string{user,
			ev("assistant.turn_start", t1, `{}`),
			ev("abort", t2, `{"reason":"x"}`, `"agentId":"sub1"`)}, Working, ""},
		{"user abort is ready", []string{user, ev("abort", t2, `{"reason":"user_initiated"}`)}, Ready, ""},
		{"shutdown ends", []string{user, ev("session.shutdown", t2, `{}`)}, Ended, ""},
		{"resumed session idles as ready", []string{user,
			ev("session.shutdown", t1, `{}`),
			ev("session.resume", t2, `{}`),
			ev("session.mode_changed", t3, `{}`)}, Ready, ""},
		{"ask_user message argument is the question", []string{user,
			ev("tool.execution_start", t1, `{"toolCallId":"q1","toolName":"ask_user","arguments":{"message":"Ship it?","requestedSchema":{}}}`)},
			NeedsYou, "Question: Ship it?"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSession(t, dir, "s1", tc.lines...)
			info, err := (&Copilot{Dir: dir}).Read("s1")
			if err != nil {
				t.Fatal(err)
			}
			if info.State != tc.state || info.Reason != tc.reason {
				t.Fatalf("got %v %q, want %v %q", info.State, info.Reason, tc.state, tc.reason)
			}
			if info.Ask != "Fix the login bug" || info.Name != "Fix login" || info.Branch != "feature/x" {
				t.Fatalf("metadata: %+v", info)
			}
		})
	}
}

func TestCopilotAskSkipsInjectedMessagesAndCollectsResults(t *testing.T) {
	saved := chunkSize
	chunkSize = 64 // force lines to span chunk boundaries
	defer func() { chunkSize = saved }()

	dir := t.TempDir()
	var lines []string
	lines = append(lines, ev("user.message", "2026-11-20T09:00:00Z", `{"content":"Real request from the user"}`))
	for i := 0; i < 7; i++ {
		lines = append(lines, ev("session.task_complete", fmt.Sprintf("2026-11-20T10:0%d:00Z", i), fmt.Sprintf(`{"summary":"Result %d"}`, i)))
	}
	lines = append(lines,
		ev("session.task_complete", "2026-11-20T10:30:00Z", `{"summary":"Subagent result"}`, `"agentId":"sub"`),
		ev("user.message", "2026-11-20T11:00:00Z", `{"content":"","transformedContent":"You have not yet marked the task as complete"}`),
		ev("assistant.turn_end", "2026-11-20T11:01:00Z", `{}`))
	writeSession(t, dir, "s1", lines...)
	info, err := (&Copilot{Dir: dir}).Read("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Ask != "Real request from the user" {
		t.Fatalf("ask = %q", info.Ask)
	}
	if len(info.Results) != maxResults || info.Results[0].Text != "Result 6" || info.Results[4].Text != "Result 2" {
		t.Fatalf("results = %+v", info.Results)
	}
	if !info.UpdatedAt.Equal(time.Date(2026, 11, 20, 11, 1, 0, 0, time.UTC)) {
		t.Fatalf("updated = %v", info.UpdatedAt)
	}
}

func TestCopilotLocksAndCache(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "abc", ev("assistant.turn_end", "2026-11-20T10:00:00Z", `{}`))
	_ = os.WriteFile(filepath.Join(dir, "abc", "inuse.4242.lock"), nil, 0o644)
	c := &Copilot{Dir: dir}
	if got := c.Locks(); got[4242] != "abc" || len(got) != 1 {
		t.Fatalf("locks = %v", got)
	}
	if info, _ := c.Read("abc"); info.State != Ready {
		t.Fatalf("state = %v", info.State)
	}
	f, _ := os.OpenFile(filepath.Join(dir, "abc", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintln(f, ev("assistant.turn_start", "2026-11-20T10:05:00Z", `{}`))
	f.Close()
	if info, _ := c.Read("abc"); info.State != Working {
		t.Fatalf("cache was not invalidated: %v", info.State)
	}
}

func TestClaudeTranscript(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{Dir: dir}
	pdir := c.ProjectDir("/home/me/my.app")
	if filepath.Base(pdir) != "-home-me-my-app" {
		t.Fatalf("project dir = %s", pdir)
	}
	_ = os.MkdirAll(pdir, 0o755)
	lines := []string{
		`{"type":"summary","summary":"Refactor auth"}`,
		`{"type":"user","timestamp":"2026-11-20T10:00:00Z","gitBranch":"auth","message":{"content":"Split the auth module"}}`,
		`{"type":"assistant","timestamp":"2026-11-20T10:01:00Z","message":{"stop_reason":"tool_use","content":[{"type":"tool_use"}]}}`,
		`{"type":"user","timestamp":"2026-11-20T10:02:00Z","message":{"content":[{"type":"tool_result"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-20T10:03:00Z","isSidechain":true,"message":{"stop_reason":"end_turn","content":[{"type":"text","text":"sidechain"}]}}`,
		`{"type":"assistant","timestamp":"2026-11-20T10:04:00Z","message":{"stop_reason":"end_turn","content":[{"type":"text","text":"Auth is split into three files."}]}}`,
	}
	path := filepath.Join(pdir, "sess1.jsonl")
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	cp, ok := c.Latest("/home/me/my.app", time.Now().Add(-time.Minute))
	if !ok || cp.SessionID != "sess1" {
		t.Fatalf("latest = %+v %v", cp, ok)
	}
	if _, ok := c.Latest("/home/me/my.app", time.Now().Add(time.Hour)); ok {
		t.Fatal("a transcript older than the process must not match")
	}
	info, err := c.Read(cp)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != Ready || info.Ask != "Split the auth module" || info.Name != "Refactor auth" || info.Branch != "auth" {
		t.Fatalf("info = %+v", info)
	}
	if len(info.Results) != 1 || info.Results[0].Text != "Auth is split into three files." {
		t.Fatalf("results = %+v", info.Results)
	}
}

func TestFirstSentence(t *testing.T) {
	got := FirstSentence("**Done.** The migration ran cleanly. Next I will clean up.\nMore", 200)
	if got != "Done. The migration ran cleanly." {
		t.Fatalf("got %q", got)
	}
}

func TestClaudeStopSequenceEndsTurn(t *testing.T) {
	dir := t.TempDir()
	c := &Claude{Dir: dir}
	pdir := c.ProjectDir("/w")
	_ = os.MkdirAll(pdir, 0o755)
	lines := `{"type":"user","timestamp":"2026-11-20T10:00:00Z","message":{"content":"hi"}}
{"type":"assistant","timestamp":"2026-11-20T10:01:00Z","message":{"model":"<synthetic>","stop_reason":"stop_sequence","content":[{"type":"text","text":"API Error: 401"}]}}
`
	_ = os.WriteFile(filepath.Join(pdir, "s.jsonl"), []byte(lines), 0o644)
	cp, ok := c.Latest("/w", time.Now().Add(-time.Minute))
	if !ok {
		t.Fatal("no transcript")
	}
	info, err := c.Read(cp)
	if err != nil || info.State != Ready {
		t.Fatalf("state = %v, err = %v; want ready", info.State, err)
	}
}
