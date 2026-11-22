package summary

import (
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
	out := "Sure:\n```json\n" + `{"now":"S1 is training — S2's run is done. S9 is not real.",` +
		`"purpose":"Train a model","sessions":{"S1":"Training — almost done","S2":"Finished","S7":"ghost","x":"bad"}}` + "\n```"
	s, err := Parse(out, input())
	if err != nil {
		t.Fatal(err)
	}
	if s.Now != `"thinker" is training - "gpu"'s run is done. S9 is not real.` {
		t.Fatalf("now = %q", s.Now)
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
	p := Prompt(input(), t0)
	for _, want := range []string{"PROJECT: nabu", "STATED PURPOSE: none", `S1: "thinker" (copilot)`, `S2: "gpu" (claude)`, "latest ask: train it"} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, p)
		}
	}
}
