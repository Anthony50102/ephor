package agents

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
	lines = append(lines, ev("user.message", "2026-11-20T08:00:00Z", `{"content":"Older request"}`))
	lines = append(lines, ev("user.message", "2026-11-20T09:00:00Z", `{"content":"Real request from the user"}`))
	for i := 0; i < 7; i++ {
		lines = append(lines, ev("session.task_complete", fmt.Sprintf("2026-11-20T10:0%d:00Z", i), fmt.Sprintf(`{"summary":"Result %d"}`, i)))
	}
	lines = append(lines,
		ev("session.task_complete", "2026-11-20T10:30:00Z", `{"summary":"Subagent result"}`, `"agentId":"sub"`),
		ev("user.message", "2026-11-20T11:00:00Z", `{"content":"","transformedContent":"You have not yet marked the task as complete"}`),
		ev("assistant.message", "2026-11-20T10:58:00Z", `{"content":"Main agent says hi"}`),
		ev("assistant.message", "2026-11-20T10:59:00Z", `{"content":"Subagent chatter"}`, `"agentId":"sub"`),
		ev("assistant.turn_end", "2026-11-20T11:01:00Z", `{}`))
	writeSession(t, dir, "s1", lines...)
	info, err := (&Copilot{Dir: dir}).Read("s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Ask != "Real request from the user" {
		t.Fatalf("ask = %q", info.Ask)
	}
	if len(info.Asks) != 2 || info.Asks[0].Text != "Real request from the user" || info.Asks[1].Text != "Older request" {
		t.Fatalf("asks = %+v", info.Asks)
	}
	if info.Said != "Main agent says hi" {
		t.Fatalf("said = %q", info.Said)
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
	if info.Said != "Auth is split into three files." || len(info.Asks) != 1 || info.Asks[0].Text != "Split the auth module" {
		t.Fatalf("said = %q, asks = %+v", info.Said, info.Asks)
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

func TestCopilotCheckpointArcAndFirstAsk(t *testing.T) {
	dir := t.TempDir()
	sid := "s-arc"
	sdir := filepath.Join(dir, sid)
	_ = os.MkdirAll(filepath.Join(sdir, "checkpoints"), 0o755)
	var ev strings.Builder
	for i := 1; i <= 6; i++ {
		ev.WriteString(`{"type":"user.message","timestamp":"2026-11-20T10:0` + string(rune('0'+i)) + `:00Z","data":{"content":"ask number ` + string(rune('0'+i)) + `"}}` + "\n")
	}
	_ = os.WriteFile(filepath.Join(sdir, "events.jsonl"), []byte(ev.String()), 0o644)

	c := &Copilot{Dir: dir}
	// Without checkpoints, FirstAsk captures the opening prompt that fell off maxAsks.
	info, err := c.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if info.FirstAsk != "ask number 1" {
		t.Fatalf("FirstAsk = %q, want %q", info.FirstAsk, "ask number 1")
	}
	if len(info.Arc) != 0 {
		t.Fatalf("expected no Arc yet, got %v", info.Arc)
	}

	// Adding checkpoints/index.md invalidates the cache and samples the full arc.
	var idx strings.Builder
	idx.WriteString("# Checkpoints\n\n| # | Title | File |\n|---|-------|------|\n")
	for i := 1; i <= 12; i++ {
		idx.WriteString("| " + strconv.Itoa(i) + " | Milestone " + strconv.Itoa(i) + " | m.md |\n")
	}
	_ = os.WriteFile(filepath.Join(sdir, "checkpoints", "index.md"), []byte(idx.String()), 0o644)

	info, err = c.Read(sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Arc) != maxArc {
		t.Fatalf("Arc len = %d, want %d (%v)", len(info.Arc), maxArc, info.Arc)
	}
	if info.Arc[0] != "Milestone 1" || info.Arc[len(info.Arc)-1] != "Milestone 12" {
		t.Fatalf("Arc endpoints = %v", info.Arc)
	}
	if info.FirstAsk != "" {
		t.Fatalf("FirstAsk should be omitted when Arc is present, got %q", info.FirstAsk)
	}
}

func TestCopilotScheduledPrompts(t *testing.T) {
	write := func(t *testing.T, sid, events string) Info {
		t.Helper()
		dir := t.TempDir()
		sdir := filepath.Join(dir, sid)
		_ = os.MkdirAll(sdir, 0o755)
		_ = os.WriteFile(filepath.Join(sdir, "events.jsonl"), []byte(events), 0o644)
		c := &Copilot{Dir: dir}
		info, err := c.Read(sid)
		if err != nil {
			t.Fatal(err)
		}
		return info
	}

	// Active recurring schedule keeps an otherwise-ready session Working.
	active := write(t, "active",
		`{"type":"user.message","timestamp":"2026-11-24T18:00:00Z","data":{"content":"watch the prover job"}}`+"\n"+
			`{"type":"session.schedule_created","timestamp":"2026-11-24T18:01:00Z","data":{"id":47,"intervalMs":1200000,"prompt":"check prover"}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T18:01:05Z","data":{"turnId":"0"}}`+"\n"+
			`{"type":"user.message","timestamp":"2026-11-24T18:21:00Z","data":{"content":"[Scheduled prompt #47]\ncheck prover"}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T18:21:10Z","data":{"turnId":"1"}}`+"\n",
	)
	if active.State != Working || active.Scheduled != "every 20m" {
		t.Fatalf("active schedule: state=%v scheduled=%q; want working / every 20m", active.State, active.Scheduled)
	}

	// Cancelled schedule lets the session settle back to Ready.
	cancelled := write(t, "cancelled",
		`{"type":"session.schedule_created","timestamp":"2026-11-24T18:01:00Z","data":{"id":47,"intervalMs":1200000,"prompt":"check"}}`+"\n"+
			`{"type":"session.schedule_cancelled","timestamp":"2026-11-24T18:22:00Z","data":{"id":47}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T18:22:05Z","data":{"turnId":"2"}}`+"\n",
	)
	if cancelled.State != Ready || cancelled.Scheduled != "" {
		t.Fatalf("cancelled schedule: state=%v scheduled=%q; want ready / empty", cancelled.State, cancelled.Scheduled)
	}

	// NeedsYou and Ended are never masked by an active schedule.
	needs := write(t, "needs",
		`{"type":"session.schedule_created","timestamp":"2026-11-24T18:01:00Z","data":{"id":9,"intervalMs":300000,"prompt":"poll"}}`+"\n"+
			`{"type":"tool.execution_start","timestamp":"2026-11-24T18:06:00Z","data":{"toolName":"ask_user","arguments":{"question":"Restart worker?"}}}`+"\n",
	)
	if needs.State != NeedsYou || needs.Scheduled != "every 5m" {
		t.Fatalf("needs_you with schedule: state=%v scheduled=%q; want needs_you / every 5m", needs.State, needs.Scheduled)
	}
	ended := write(t, "ended",
		`{"type":"session.schedule_created","timestamp":"2026-11-24T18:01:00Z","data":{"id":9,"intervalMs":300000,"prompt":"poll"}}`+"\n"+
			`{"type":"session.shutdown","timestamp":"2026-11-24T18:10:00Z","data":{}}`+"\n",
	)
	if ended.State != Ended {
		t.Fatalf("ended with schedule: state=%v; want ended", ended.State)
	}

	// One-shot schedule that already fired is done (Ready).
	oneshot := write(t, "oneshot",
		`{"type":"session.schedule_created","timestamp":"2026-11-24T18:01:00Z","data":{"id":5,"intervalMs":600000,"recurring":false,"prompt":"check once"}}`+"\n"+
			`{"type":"user.message","timestamp":"2026-11-24T18:11:00Z","data":{"content":"[Scheduled prompt #5]\ncheck once"}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T18:11:05Z","data":{"turnId":"1"}}`+"\n",
	)
	if oneshot.State != Ready || oneshot.Scheduled != "" {
		t.Fatalf("fired one-shot: state=%v scheduled=%q; want ready / empty", oneshot.State, oneshot.Scheduled)
	}

	// Fallback: fired scheduled prompt whose creation scrolled out of window still marks Working.
	scrolled := write(t, "scrolled",
		`{"type":"user.message","timestamp":"2026-11-24T18:21:00Z","data":{"content":"[Scheduled prompt #88]\ncheck training"}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T18:21:10Z","data":{"turnId":"9"}}`+"\n",
	)
	if scrolled.State != Working || scrolled.Scheduled != "scheduled" {
		t.Fatalf("scrolled schedule: state=%v scheduled=%q; want working / scheduled", scrolled.State, scrolled.Scheduled)
	}

	// Self-paced wakeup re-arm keeps the session Working until it fires or is cancelled.
	wakeup := write(t, "wakeup",
		`{"type":"tool.execution_start","timestamp":"2026-11-24T18:00:00Z","data":{"toolName":"manage_schedule","arguments":{"action":"wakeup","id":12,"delaySeconds":1800}}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T18:00:05Z","data":{"turnId":"1"}}`+"\n",
	)
	if wakeup.State != Working || wakeup.Scheduled != "every 30m" {
		t.Fatalf("self-paced wakeup: state=%v scheduled=%q; want working / every 30m", wakeup.State, wakeup.Scheduled)
	}
}

func TestCopilotBackgroundCheckpointAndDedupAsks(t *testing.T) {
	// Active background bash shell after the last user message keeps session Working.
	dir := t.TempDir()
	sdir := filepath.Join(dir, "bg")
	_ = os.MkdirAll(filepath.Join(sdir, "checkpoints"), 0o755)
	_ = os.WriteFile(filepath.Join(sdir, "checkpoints", "index.md"), []byte(
		"# Checkpoints\n\n| # | Title | File |\n|---|-------|------|\n| 1 | Wave 8 | 001-wave8.md |\n",
	), 0o644)
	_ = os.WriteFile(filepath.Join(sdir, "checkpoints", "001-wave8.md"), []byte(
		"<work_done>Built wave 8 authoring brief.</work_done>\n<next_steps>Fix relay rotation.</next_steps>\n",
	), 0o644)
	_ = os.WriteFile(filepath.Join(sdir, "events.jsonl"), []byte(
		`{"type":"tool.execution_complete","timestamp":"2026-11-24T21:10:00Z","data":{"toolCallId":"t1","result":{"content":"<command with shellId: 14444 is still running after 30 seconds.>"}}}`+"\n"+
			`{"type":"assistant.message","timestamp":"2026-11-24T21:10:05Z","data":{"content":"Waiting for the next check."}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T21:10:06Z","data":{"turnId":"4"}}`+"\n",
	), 0o644)
	c := &Copilot{Dir: dir}
	info, err := c.Read("bg")
	if err != nil {
		t.Fatal(err)
	}
	if info.State != Working || info.Scheduled != "background" {
		t.Fatalf("active bg: state=%v scheduled=%q; want working / background", info.State, info.Scheduled)
	}
	if !strings.Contains(info.Checkpoint, "Built wave 8 authoring brief.") || !strings.Contains(info.Checkpoint, "Fix relay rotation.") {
		t.Fatalf("expected latest checkpoint summary, got %q", info.Checkpoint)
	}

	// Completed or older-turn background shell does not keep the session Working.
	sdir2 := filepath.Join(dir, "bg-done")
	_ = os.MkdirAll(sdir2, 0o755)
	_ = os.WriteFile(filepath.Join(sdir2, "events.jsonl"), []byte(
		`{"type":"tool.execution_complete","timestamp":"2026-11-24T19:00:00Z","data":{"toolCallId":"t1","result":{"content":"<command with shellId: 99 is still running after 30 seconds.>"}}}`+"\n"+
			`{"type":"user.message","timestamp":"2026-11-24T20:00:00Z","data":{"content":"first human ask"}}`+"\n"+
			`{"type":"user.message","timestamp":"2026-11-24T20:05:00Z","data":{"content":"[Scheduled prompt #1] poll"}}`+"\n"+
			`{"type":"user.message","timestamp":"2026-11-24T20:10:00Z","data":{"content":"[Scheduled prompt #1] poll"}}`+"\n"+
			`{"type":"session.schedule_cancelled","timestamp":"2026-11-24T20:11:00Z","data":{"id":1}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T20:11:05Z","data":{"turnId":"2"}}`+"\n",
	), 0o644)
	info2, err := c.Read("bg-done")
	if err != nil {
		t.Fatal(err)
	}
	if info2.State != Ready || info2.Scheduled != "" {
		t.Fatalf("older bg: state=%v scheduled=%q; want ready / empty", info2.State, info2.Scheduled)
	}
	if len(info2.Asks) != 2 || info2.Asks[1].Text != "first human ask" {
		t.Fatalf("repeated asks should deduplicate while keeping earlier asks, got %+v", info2.Asks)
	}

	// Subagent markdown prose quoting shellId/still-running text must not mark a
	// finished session as Working, and stop_bash's "<command with id: X stopped>"
	// footer clears a real background shell.
	sdir3 := filepath.Join(dir, "bg-subagent-prose")
	_ = os.MkdirAll(sdir3, 0o755)
	_ = os.WriteFile(filepath.Join(sdir3, "events.jsonl"), []byte(
		`{"type":"tool.execution_complete","timestamp":"2026-11-24T21:00:00Z","data":{"toolCallId":"t1","result":{"content":"<command with shellId: 1069 is still running after 30 seconds.>"}}}`+"\n"+
			`{"type":"tool.execution_complete","timestamp":"2026-11-24T21:01:00Z","data":{"toolCallId":"t2","result":{"content":"- shellId: `+"`1069`"+`\n- exact tool message: `+"`The command is still running.`"+`\n</final_answer>"}}}`+"\n"+
			`{"type":"tool.execution_complete","timestamp":"2026-11-24T21:02:00Z","data":{"toolCallId":"t3","result":{"content":"<command with id: 1069 stopped>"}}}`+"\n"+
			`{"type":"assistant.turn_end","timestamp":"2026-11-24T21:03:00Z","data":{"turnId":"5"}}`+"\n",
	), 0o644)
	info3, err := c.Read("bg-subagent-prose")
	if err != nil {
		t.Fatal(err)
	}
	if info3.State != Ready || info3.Scheduled != "" {
		t.Fatalf("subagent prose + stopped shell: state=%v scheduled=%q; want ready / empty", info3.State, info3.Scheduled)
	}
}
