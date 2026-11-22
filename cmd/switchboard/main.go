// Command switchboard is a keyboard-first control panel for agent sessions in tmux.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/tmux"
	"github.com/anthonypoole/switchboard/internal/ui"
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("switchboard", flag.ExitOnError)
	statePath := fs.String("state", store.DefaultPath(), "path to the state file")
	showVersion := fs.Bool("version", false, "print version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: switchboard [flags] [command]

Commands:
  (none)        open the control panel
  list [--json] print discovered sessions

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

	switch fs.Arg(0) {
	case "":
		target, err := ui.Run(src, st, *statePath)
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
