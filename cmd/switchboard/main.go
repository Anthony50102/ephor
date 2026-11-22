// Command switchboard is a keyboard-first control panel for agent sessions in tmux.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/summary"
	"github.com/anthonypoole/switchboard/internal/tmux"
	"github.com/anthonypoole/switchboard/internal/ui"
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("switchboard", flag.ExitOnError)
	statePath := fs.String("state", store.DefaultPath(), "path to the state file")
	showVersion := fs.Bool("version", false, "print version and exit")
	summarizer := fs.String("summarizer", envOr("SWITCHBOARD_SUMMARIZER", "auto"),
		"who writes project summaries: auto, copilot, off, or a shell command that reads the prompt on stdin (env SWITCHBOARD_SUMMARIZER)")
	model := fs.String("summary-model", envOr("SWITCHBOARD_SUMMARY_MODEL", "claude-haiku-4.5"),
		"model for the copilot summarizer (env SWITCHBOARD_SUMMARY_MODEL)")
	every := fs.Duration("summary-every", ui.DefaultSummaryEvery, "shortest gap between automatic rewrites of one project's summary")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: switchboard [flags] [command]

Commands:
  (none)                            open the control panel
  list [--json]                     print discovered sessions
  summarize [--dry-run] <project>   write one project's summary now, or print the prompt

Flags:
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("switchboard", version)
		return
	}

	st, err := store.Load(*statePath)
	if err != nil {
		fail(err)
	}
	src := core.DefaultSources()

	dir := filepath.Dir(*statePath)
	run, err := pickSummarizer(*summarizer, *model, filepath.Join(dir, "summarizer"))
	if err != nil {
		fail(err)
	}

	switch fs.Arg(0) {
	case "":
		target, err := ui.Run(src, ui.Options{Store: st, StatePath: *statePath,
			Summarize: run, SummaryPath: filepath.Join(dir, "summaries.json"), SummaryEvery: *every})
		if err != nil {
			fail(err)
		}
		if target != nil {
			if err := src.Tmux.Focus(*target); err != nil {
				fail(err)
			}
			fail(tmux.ExecAttach(*target))
		}
	case "list":
		if err := list(src, st, fs.Arg(1) == "--json"); err != nil {
			fail(err)
		}
	case "summarize":
		args := fs.Args()[1:]
		dry := len(args) > 0 && args[0] == "--dry-run"
		if dry {
			args = args[1:]
		}
		if err := summarize(src, st, run, strings.Join(args, " "), dry); err != nil {
			fail(err)
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func list(src core.Sources, st *store.State, jsonOut bool) error {
	ss, err := core.Discover(src)
	if err != nil {
		return err
	}
	now := time.Now()
	core.SortSessions(ss, now)
	placement := func(s core.Session) (string, string) {
		a, ok := st.Assign[s.Key]
		if !ok {
			return "", ""
		}
		p := st.Project(a.Project)
		if p == nil {
			return "", ""
		}
		if t := p.Thread(a.Thread); t != nil {
			return p.Name, t.Name
		}
		return p.Name, ""
	}
	if jsonOut {
		type row struct {
			Key     string    `json:"key"`
			Kind    string    `json:"kind"`
			Title   string    `json:"title"`
			Tmux    string    `json:"tmux"`
			State   string    `json:"state"`
			Reason  string    `json:"reason,omitempty"`
			Ask     string    `json:"ask,omitempty"`
			Project string    `json:"project,omitempty"`
			Thread  string    `json:"thread,omitempty"`
			Updated time.Time `json:"updated"`
		}
		rows := []row{}
		for _, s := range ss {
			p, t := placement(s)
			rows = append(rows, row{s.Key, s.Kind, s.Title(), s.Pane.Target(), s.State(now).String(),
				s.Info.Reason, s.Info.Ask, p, t, s.Info.UpdatedAt})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "STATE\tAGE\tTMUX\tKIND\tTITLE\tPROJECT")
	for _, s := range ss {
		p, _ := placement(s)
		if p == "" {
			p = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.State(now), core.Ago(s.Info.UpdatedAt, now), s.Pane.Target(), s.Kind, s.Title(), p)
	}
	return w.Flush()
}

func fail(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "switchboard:", err)
	os.Exit(1)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func pickSummarizer(name, model, dir string) (summary.Runner, error) {
	switch name {
	case "off", "":
		return nil, nil
	case "auto":
		if _, err := exec.LookPath("copilot"); err != nil {
			return nil, nil
		}
		return summary.Copilot(model, dir), nil
	case "copilot":
		if _, err := exec.LookPath("copilot"); err != nil {
			return nil, fmt.Errorf("--summarizer copilot: %w", err)
		}
		return summary.Copilot(model, dir), nil
	}
	return summary.Shell(name, dir), nil
}

func summarize(src core.Sources, st *store.State, run summary.Runner, name string, dry bool) error {
	var pid string
	for _, p := range st.Projects {
		if strings.EqualFold(p.Name, name) {
			pid = p.ID
		}
	}
	if pid == "" {
		return fmt.Errorf("no project named %q", name)
	}
	ss, err := core.Discover(src)
	if err != nil {
		return err
	}
	now := time.Now()
	in := ui.SummaryInput(st, ss, pid, now)
	prompt := summary.Prompt(in, now)
	if dry {
		fmt.Print(prompt)
		return nil
	}
	if run == nil {
		return fmt.Errorf("summaries are off (see --summarizer)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := run(ctx, prompt)
	if err != nil {
		return err
	}
	sum, err := summary.Parse(out, in)
	if err != nil {
		return err
	}
	fmt.Println(sum.Now)
	if sum.Purpose != "" {
		fmt.Println("\nProbably for:", sum.Purpose)
	}
	for _, s := range in.Sessions {
		if d := sum.Sessions[s.Key]; d != "" {
			fmt.Printf("\n  %s: %s", s.Title, d)
		}
	}
	fmt.Println()
	return nil
}
