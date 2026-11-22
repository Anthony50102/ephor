package summary

import (
	"context"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

func input() Input {
	return Input{Project: "nabu", Sessions: []SessionInput{
		{Key: "a", Title: "thinker", Kind: "copilot", State: "working", Updated: t0, Asks: []string{"train it"}},
		{Key: "b", Title: "gpu", Kind: "claude", State: "ended", Updated: t0},
	}}
}

func TestParseMapsLabelsAndTidies(t *testing.T) {
	out := "Sure:\n```json\n" + `{"headline":"S1 trains","now":"S1 is training — S2's run is done. S9 is not real.","next":"Review S2",` +
		`"purpose":"Train a model","sessions":{"S1":"Training — almost done","S2":"Finished","S7":"ghost","x":"bad"}}` + "\n```"
	s, err := Parse(out, input())
	if err != nil {
		t.Fatal(err)
	}
	if s.Now != `"thinker" is training - "gpu"'s run is done. S9 is not real.` {
		t.Fatalf("now = %q", s.Now)
	}
	if s.Headline != `"thinker" trains` || s.Next != `Review "gpu"` {
		t.Fatalf("headline/next = %q / %q", s.Headline, s.Next)
	}
	if s.Purpose != "Train a model" || len(s.Sessions) != 2 || s.Sessions["a"] != "Training - almost done" || s.Sessions["b"] != "Finished" {
		t.Fatalf("summary = %+v", s)
	}

	in := input()
	in.Purpose = "Mine"
	if s, _ := Parse(out, in); s.Purpose != "" {
		t.Fatalf("purpose must not be suggested when the user wrote one: %q", s.Purpose)
	}
}

func TestParseRejectsJunk(t *testing.T) {
	for _, out := range []string{"no json here", `{"now":""}`, `{"now": oops}`} {
		if _, err := Parse(out, input()); err == nil {
			t.Fatalf("Parse(%q) should fail", out)
		}
	}
}

func TestFingerprintIgnoresTimeOnly(t *testing.T) {
	a, b := input(), input()
	b.Sessions[0].Updated = t0.Add(time.Hour)
	if Fingerprint(a) != Fingerprint(b) {
		t.Fatal("a newer timestamp alone must not trigger a rewrite")
	}
	b.Sessions[0].State = "ready"
	if Fingerprint(a) == Fingerprint(b) {
		t.Fatal("a state change must trigger a rewrite")
	}
}

func TestPromptLabelsSessions(t *testing.T) {
	in := input()
	in.Cloud = []string{"VM gpu in rg, created"}
	p := Prompt(in, t0)
	for _, want := range []string{"PROJECT: nabu", "STATED PURPOSE: none", `S1: "thinker" (copilot)`, `S2: "gpu" (claude)`, "latest ask: train it", "  - VM gpu in rg, created"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, p)
		}
	}
}

func TestShellTimeoutKillsGrandchildren(t *testing.T) {
	// The pipeline forks children that hold stdout open; killing only sh
	// would leave Run waiting on them.
	run := Shell("sleep 30 | cat", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := run(ctx, "prompt"); err == nil {
		t.Fatal("want a timeout error")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("runner took %v after its deadline", d)
	}
}
