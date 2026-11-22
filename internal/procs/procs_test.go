package procs

import (
	"testing"
	"time"
)

func TestFindAgent(t *testing.T) {
	tab := Parse(`    1     0 /sbin/init
  100     1 -zsh
  101   100 node /home/me/.npm/lib/node_modules/@github/copilot/index.js --resume
  200     1 -zsh
  201   200 vim notes.md
  300     1 /bin/bash
  301   300 /home/me/.local/bin/claude --continue
  302   301 rg claude
`)
	if p, k, ok := tab.FindAgent(100); !ok || k != Copilot || p.PID != 101 {
		t.Fatalf("copilot via node: %v %v %v", p, k, ok)
	}
	if _, _, ok := tab.FindAgent(200); ok {
		t.Fatal("vim is not an agent")
	}
	if p, k, ok := tab.FindAgent(300); !ok || k != Claude || p.PID != 301 {
		t.Fatalf("claude: %v %v %v", p, k, ok)
	}
	if AgentKind("grep copilot-notes.txt") != "" {
		t.Fatal("an argument mentioning copilot is not the copilot binary")
	}
}

func TestParseLstart(t *testing.T) {
	got, err := ParseLstart("Sun Nov  1 09:05:03 2026\n")
	if err != nil || got.Day() != 1 || got.Hour() != 9 || got.Month() != time.November {
		t.Fatalf("%v %v", got, err)
	}
}
