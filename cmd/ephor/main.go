// Command ephor is a keyboard-first control panel for agent sessions in tmux.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Anthony50102/ephor/internal/cloud"
	"github.com/Anthony50102/ephor/internal/core"
	"github.com/Anthony50102/ephor/internal/store"
	"github.com/Anthony50102/ephor/internal/summary"
	"github.com/Anthony50102/ephor/internal/tmux"
	"github.com/Anthony50102/ephor/internal/ui"
)

var version = "dev"

func main() {
	fs := flag.NewFlagSet("ephor", flag.ExitOnError)
	statePath := fs.String("state", store.DefaultPath(), "path to the state file")
	showVersion := fs.Bool("version", false, "print version and exit")
	summarizer := fs.String("summarizer", envOr("EPHOR_SUMMARIZER", "auto"),
		"who writes project summaries: auto, copilot, off, or a shell command that reads the prompt on stdin (env EPHOR_SUMMARIZER)")
	model := fs.String("summary-model", envOr("EPHOR_SUMMARY_MODEL", "claude-haiku-4.5"),
		"model for the copilot summarizer (env EPHOR_SUMMARY_MODEL)")
	liveFlag := fs.String("live", envOr("EPHOR_LIVE", "auto"),
		"ask Azure Resource Graph whether listed resources still exist: auto (when az is installed), on, or off (env EPHOR_LIVE)")
	every := fs.Duration("summary-every", ui.DefaultSummaryEvery, "shortest gap between automatic rewrites of one project's summary")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: ephor [flags] [command]

Commands:
  (none)                            open the control panel
  list [--json]                     print discovered (and recoverable offline) sessions
  restore [--all] [--dry-run] [project]
                                    resume offline sessions from the previous tmux server into agents
  summarize [--dry-run] <project>   write one project's summary now, or print the prompt
  cloud [project]                   list cloud resources sessions have touched

Flags:
`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println("ephor", version)
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

	var check cloud.Checker
	switch *liveFlag {
	case "off", "":
	case "auto", "on":
		if _, err := exec.LookPath("az"); err == nil {
			check = cloud.AzChecker()
		} else if *liveFlag == "on" {
			fail(fmt.Errorf("--live on: %w", err))
		}
	default:
		fail(fmt.Errorf("--live must be auto, on, or off"))
	}

	snapPath := store.DefaultSnapshotPath(*statePath)
	switch fs.Arg(0) {
	case "":
		opt := ui.Options{Store: st, StatePath: *statePath, SnapshotPath: snapPath,
			Summarize: run, SummaryPath: filepath.Join(dir, "summaries.json"), SummaryEvery: *every,
			CloudPath: filepath.Join(dir, "cloud.json"), CheckLive: check,
			PricePath: filepath.Join(dir, "prices.json"), Copy: ui.Clipboard}
		if check != nil {
			opt.Prices = cloud.HTTPGetter()
		}
		if _, err := exec.LookPath("kubectl"); err == nil && *liveFlag != "off" {
			opt.Kubectl = cloud.RealKubectl()
		}
		target, err := ui.Run(src, opt)
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
		if err := list(src, st, snapPath, fs.Arg(1) == "--json"); err != nil {
			fail(err)
		}
	case "restore":
		if err := restoreCLI(src, st, snapPath, fs.Args()[1:]); err != nil {
			fail(err)
		}
	case "cloud":
		if err := listCloud(src, st, check, filepath.Join(dir, "cloud.json"), strings.Join(fs.Args()[1:], " ")); err != nil {
			fail(err)
		}
	case "summarize":
		args := fs.Args()[1:]
		dry := len(args) > 0 && args[0] == "--dry-run"
		if dry {
			args = args[1:]
		}
		if err := summarize(src, st, run, check, filepath.Join(dir, "cloud.json"), filepath.Join(dir, "summaries.json"), strings.Join(args, " "), dry); err != nil {
			fail(err)
		}
	default:
		fs.Usage()
		os.Exit(2)
	}
}

func list(src core.Sources, st *store.State, snapPath string, jsonOut bool) error {
	now := time.Now()
	ss, err := core.DiscoverWithSnapshot(src, snapPath, now)
	if err != nil {
		return err
	}
	core.ApplyHolds(ss, st.Holds)
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
			Key        string    `json:"key"`
			Kind       string    `json:"kind"`
			Title      string    `json:"title"`
			Tmux       string    `json:"tmux"`
			State      string    `json:"state"`
			Offline    bool      `json:"offline,omitempty"`
			Scheduled  string    `json:"scheduled,omitempty"`
			HoldReason string    `json:"holdReason,omitempty"`
			Reason     string    `json:"reason,omitempty"`
			Ask        string    `json:"ask,omitempty"`
			Project    string    `json:"project,omitempty"`
			Thread     string    `json:"thread,omitempty"`
			Updated    time.Time `json:"updated"`
		}
		rows := []row{}
		for _, s := range ss {
			p, t := placement(s)
			target := s.Pane.Target()
			if s.Offline {
				target = ""
			}
			rows = append(rows, row{s.Key, s.Kind, s.Title(), target, s.State(now).String(), s.Offline,
				s.Info.Scheduled, s.HoldReason, s.Info.Reason, s.Info.Ask, p, t, s.Info.UpdatedAt})
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
		target := s.Pane.Target()
		if s.Offline {
			target = "(saved)"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", s.State(now), core.Ago(s.Info.UpdatedAt, now), target, s.Kind, s.Title(), p)
	}
	return w.Flush()
}

func restoreCLI(src core.Sources, st *store.State, snapPath string, args []string) error {
	rfs := flag.NewFlagSet("restore", flag.ContinueOnError)
	includeAll := rfs.Bool("all", false, "also restore sessions that were put on hold")
	dryRun := rfs.Bool("dry-run", false, "show what would be restored without starting tmux windows")
	if err := rfs.Parse(args); err != nil {
		return err
	}
	projFilter := strings.TrimSpace(strings.Join(rfs.Args(), " "))
	var wantPID string
	if projFilter != "" {
		for _, p := range st.Projects {
			if strings.EqualFold(p.Name, projFilter) {
				wantPID = p.ID
				break
			}
		}
		if wantPID == "" {
			return fmt.Errorf("no project named %q", projFilter)
		}
	}

	now := time.Now()
	live, liveErr := core.Discover(src)
	serverID := ""
	if liveErr == nil {
		serverID, _ = src.Tmux.ServerID()
	}

	snap, err := store.LoadSnapshot(snapPath)
	if err != nil {
		return err
	}
	if !*dryRun && serverID != "" {
		savedLive := core.SavedFromSessions(live, now)
		snap, err = store.UpdateSnapshot(snapPath, func(s *store.Snapshot) (bool, error) {
			return s.Sync(serverID, savedLive, now), nil
		})
		if err != nil {
			return err
		}
	}

	liveKeys := map[string]bool{}
	for _, s := range live {
		liveKeys[s.Key] = true
	}
	var candidates []store.SavedSession
	for _, sv := range snap.AllRecoverable(serverID) {
		if !liveKeys[sv.Key] {
			candidates = append(candidates, sv)
		}
	}
	hydrated := core.HydrateSaved(candidates, src)
	core.ApplyHolds(hydrated, st.Holds)

	var targets []core.Session
	skippedHeld := 0
	for _, s := range hydrated {
		if wantPID != "" {
			a, ok := st.Assign[s.Key]
			if !ok || a.Project != wantPID {
				continue
			}
		}
		if s.Held && !*includeAll {
			skippedHeld++
			continue
		}
		targets = append(targets, s)
	}

	if len(targets) == 0 {
		if skippedHeld > 0 {
			fmt.Printf("No active sessions to restore (%d on hold; pass --all to restore them).\n", skippedHeld)
			return nil
		}
		fmt.Println("Nothing to restore (all saved sessions are already live).")
		return nil
	}

	projectName := func(key string) string {
		if a, ok := st.Assign[key]; ok {
			if p := st.Project(a.Project); p != nil {
				return p.Name
			}
		}
		return "-"
	}

	if *dryRun {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "PROJECT\tWINDOW\tKIND\tTITLE\tCOMMAND")
		for _, s := range targets {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
				projectName(s.Key), core.ResumeWindowName(s), s.Kind, s.Title(), core.ResumeCommand(s.Kind, s.Info.ID))
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if skippedHeld > 0 {
			fmt.Printf("\nSkipping %d on-hold session(s) (pass --all to include them).\n", skippedHeld)
		}
		return nil
	}

	launchSess, needsPark := src.Tmux.LaunchSession(tmux.PoolSession)
	var restoredKeys []string
	var parkPanes []string
	for _, s := range targets {
		cmd := core.ResumeCommand(s.Kind, s.Info.ID)
		if cmd == "" {
			continue
		}
		dir, fallback := core.ResumeDir(s.Info.Cwd)
		win := core.ResumeWindowName(s)
		pane, err := src.Tmux.NewWindow(launchSess, win, dir)
		if err != nil {
			return fmt.Errorf("open tmux window for %s: %w", s.Title(), err)
		}
		if err := src.Tmux.Type(pane, cmd); err != nil {
			return fmt.Errorf("start %s: %w", s.Title(), err)
		}
		restoredKeys = append(restoredKeys, s.Key)
		if needsPark && pane != "" {
			parkPanes = append(parkPanes, pane)
		}
		note := ""
		if fallback {
			note = " (folder missing; started in ~)"
		}
		fmt.Printf("Restored %-24s -> %s:%s (%s)%s\n", s.Title(), tmux.PoolSession, win, projectName(s.Key), note)
	}
	if len(parkPanes) > 0 {
		// Wait briefly for zsh + CLI startup OSC color detection in the
		// attached session before parking the restored windows into agents.
		time.Sleep(3 * time.Second)
		self := os.Getenv("TMUX_PANE")
		for _, pane := range parkPanes {
			_, _ = src.Tmux.Park(pane, tmux.PoolSession, self)
		}
	}

	// Sync the snapshot to the now-running tmux server and drop restored keys
	// from Recoverable so the UI does not show them as offline while booting.
	newServerID, _ := src.Tmux.ServerID()
	savedLive := core.SavedFromSessions(live, now)
	_, _ = store.UpdateSnapshot(snapPath, func(s *store.Snapshot) (bool, error) {
		ch := false
		if newServerID != "" {
			ch = s.Sync(newServerID, savedLive, now) || ch
		}
		if s.Dismiss(restoredKeys...) {
			ch = true
		}
		return ch, nil
	})

	if skippedHeld > 0 {
		fmt.Printf("Skipped %d on-hold session(s) (pass --all to include them).\n", skippedHeld)
	}
	return nil
}

func fail(err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "ephor:", err)
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

func summarize(src core.Sources, st *store.State, run summary.Runner, check cloud.Checker, cloudPath, summaryPath, name string, dry bool) error {
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
	core.ApplyHolds(ss, st.Holds)
	now := time.Now()
	res := scanCloud(cloudPath, ss)
	sums := summary.LoadCache(summaryPath)
	in := ui.SummaryInput(st, sums, ss, pid, now, res, checkLive(check, res))
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
	if sum.Headline != "" {
		fmt.Print(sum.Headline, "\n\n")
	}
	fmt.Println(sum.Now)
	if sum.Next != "" {
		fmt.Println("\nNext up:", sum.Next)
	}
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

func listCloud(src core.Sources, st *store.State, check cloud.Checker, indexPath, project string) error {
	ss, err := core.Discover(src)
	if err != nil {
		return err
	}
	res := scanCloud(indexPath, ss)
	by := map[string]map[string][]cloud.Resource{} // project name to session title to resources
	for _, s := range ss {
		if res[s.Info.Log] == nil {
			continue
		}
		p := "Unassigned"
		if a, ok := st.Assign[s.Key]; ok {
			if pr := st.Project(a.Project); pr != nil {
				p = pr.Name
			}
		}
		if project != "" && !strings.EqualFold(p, project) {
			continue
		}
		if by[p] == nil {
			by[p] = map[string][]cloud.Resource{}
		}
		by[p][s.Title()] = res[s.Info.Log]
	}
	if project != "" && by[project] == nil {
		found := false
		for p := range by {
			found = found || strings.EqualFold(p, project)
		}
		if !found {
			return fmt.Errorf("no live sessions in a project named %q", project)
		}
	}
	var all []cloud.Resource
	for _, p := range by {
		for _, rs := range p {
			all = append(all, rs...)
		}
	}
	live := checkLive(check, map[string][]cloud.Resource{"": all})
	now := time.Now()
	names := make([]string, 0, len(by))
	for p := range by {
		names = append(names, p)
	}
	sort.Strings(names)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, p := range names {
		rows, note := cloud.Rows(cloud.Merge(by[p]), live)
		if len(rows) == 0 && note == "" {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", p)
		for _, r := range rows {
			did := r.Resource.Status() + " " + core.Ago(r.Last, now)
			if s := r.Status(); s != r.Resource.Status() && !(r.Deleted && r.Gone()) {
				did = s + " · " + did
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", r.Kind, r.Name, clip(r.Where(), 70), did, strings.Join(r.By, ", "))
		}
		if note != "" {
			fmt.Fprintf(w, "  also %s\n", note)
		}
	}
	return w.Flush()
}

// checkLive asks Azure about the resources, or returns nil when it cannot.
func checkLive(check cloud.Checker, res map[string][]cloud.Resource) map[string]cloud.Live {
	if check == nil {
		return nil
	}
	var all []cloud.Resource
	for _, rs := range res {
		all = append(all, rs...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	live, err := cloud.Check(ctx, check, all)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ephor: cloud check skipped:", err)
		return nil
	}
	return live
}

// scanCloud brings the cloud index up to date for the given sessions.
func scanCloud(indexPath string, ss []core.Session) map[string][]cloud.Resource {
	ix := cloud.LoadIndex(indexPath)
	res := map[string][]cloud.Resource{}
	for _, s := range ss {
		if s.Info.Log == "" {
			continue
		}
		if _, err := ix.Scan(s.Info.Log); err == nil {
			res[s.Info.Log] = ix.Resources(s.Info.Log)
		}
	}
	_ = ix.Save(indexPath)
	return res
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
