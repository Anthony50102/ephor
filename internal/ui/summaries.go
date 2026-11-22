package ui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/anthonypoole/switchboard/internal/cloud"
	"github.com/anthonypoole/switchboard/internal/core"
	"github.com/anthonypoole/switchboard/internal/store"
	"github.com/anthonypoole/switchboard/internal/summary"
)

// DefaultSummaryEvery is the shortest gap between automatic rewrites of one
// project's summary. Agents change their logs constantly; a summary that
// churns every few seconds is noise and costs model calls.
const DefaultSummaryEvery = 5 * time.Minute

const summaryTimeout = 2 * time.Minute

type summaryMsg struct {
	pid string
	sum summary.Summary
	err error
}

func (m Model) summaryFor(pid string) (summary.Summary, bool) {
	if m.sums == nil {
		return summary.Summary{}, false
	}
	s, ok := m.sums.Projects[pid]
	return s, ok && s.Now != ""
}

func stateName(st agents.State) string {
	switch st {
	case agents.NeedsYou:
		return "waiting on the user"
	case agents.Working:
		return "working"
	case agents.Ready:
		return "finished, waiting for the next instruction"
	case agents.Quiet:
		return "idle for over a day"
	case agents.Ended:
		return "ended"
	}
	return "unknown"
}

func (m Model) summaryInput(pid string) summary.Input {
	now := m.opt.Now()
	p := m.st.Project(pid)
	in := summary.Input{Project: p.Name, Purpose: p.Purpose}
	ss := m.sessionsIn(sideItem{kind: sideProject, pid: pid})
	core.SortSessions(ss, now)
	for _, s := range ss {
		si := summary.SessionInput{
			Key: s.Key, Title: s.Title(), Kind: s.Kind, State: stateName(s.State(now)),
			Reason: s.Info.Reason, Branch: s.Info.Branch, Updated: s.Info.UpdatedAt, Said: s.Info.Said,
		}
		for _, a := range s.Info.Asks {
			si.Asks = append(si.Asks, a.Text)
		}
		if len(si.Asks) == 0 && s.Info.Ask != "" {
			si.Asks = []string{s.Info.Ask}
		}
		for _, r := range s.Info.Results {
			si.Results = append(si.Results, r.Text)
		}
		in.Sessions = append(in.Sessions, si)
	}
	rows, _ := m.cloudRows(ss)
	for _, r := range rows {
		if len(in.Cloud) == 6 || r.Deleted {
			break
		}
		in.Cloud = append(in.Cloud, r.Line())
	}
	return in
}

// staleSummary reports whether a project's summary should be rewritten now.
func (m Model) staleSummary(pid string, now time.Time) bool {
	cur := m.sums.Projects[pid]
	if cur.Input == summary.Fingerprint(m.summaryInput(pid)) {
		return false
	}
	last := cur.At
	if cur.Tried.After(last) {
		last = cur.Tried
	}
	return last.IsZero() || now.Sub(last) >= m.opt.SummaryEvery
}

// maybeSummarize starts at most one summary at a time, preferring the project
// on screen, then the rest in sidebar order.
func (m *Model) maybeSummarize() tea.Cmd {
	if m.opt.Summarize == nil || m.sumBusy != "" || !m.loaded {
		return nil
	}
	if m.opt.SummaryPath != "" {
		// Another Switchboard may have just written one.
		m.sums = summary.LoadCache(m.opt.SummaryPath)
	}
	now := m.opt.Now()
	order := []string{m.scope().pid}
	for _, p := range m.st.Projects {
		order = append(order, p.ID)
	}
	for _, pid := range order {
		if pid == "" || m.st.Project(pid) == nil || len(m.sessionsIn(sideItem{kind: sideProject, pid: pid})) == 0 {
			continue
		}
		if m.staleSummary(pid, now) {
			return m.startSummary(pid)
		}
	}
	return nil
}

func (m *Model) startSummary(pid string) tea.Cmd {
	in := m.summaryInput(pid)
	now := m.opt.Now()
	prompt := summary.Prompt(in, now)
	run := m.opt.Summarize
	m.sumBusy = pid
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), summaryTimeout)
		defer cancel()
		out, err := run(ctx, prompt)
		if err != nil {
			return summaryMsg{pid: pid, err: err, sum: summary.Summary{Input: summary.Fingerprint(in), At: now}}
		}
		s, err := summary.Parse(out, in)
		s.Input, s.At = summary.Fingerprint(in), now
		return summaryMsg{pid: pid, sum: s, err: err}
	}
}

func (m *Model) gotSummary(msg summaryMsg) {
	m.sumBusy = ""
	if m.opt.SummaryPath != "" {
		m.sums = summary.LoadCache(m.opt.SummaryPath)
	}
	next := msg.sum
	if msg.err != nil {
		// Keep the last good text and when it was written; record the
		// failed attempt separately so it backs off.
		prev := m.sums.Projects[msg.pid]
		prev.Err, prev.Tried = msg.err.Error(), next.At
		next = prev
		m.setErr(fmt.Errorf("summary for %s failed: %w", m.placeName(msg.pid, ""), msg.err))
	}
	m.sums.Projects[msg.pid] = next
	if m.opt.SummaryPath != "" {
		if err := m.sums.Save(m.opt.SummaryPath); err != nil {
			m.setErr(fmt.Errorf("could not save summaries: %w", err))
		}
	}
}

// forceSummary rewrites the summary for the project on screen now.
func (m *Model) forceSummary() tea.Cmd {
	pid := m.scope().pid
	switch {
	case m.opt.Summarize == nil:
		m.setErr(fmt.Errorf("summaries are off (see --summarizer)"))
	case pid == "":
		m.setErr(fmt.Errorf("select a project first"))
	case m.sumBusy != "":
		m.setStatus("Already writing a summary for " + m.placeName(m.sumBusy, ""))
	case len(m.sessionsIn(sideItem{kind: sideProject, pid: pid})) == 0:
		m.setStatus("No live sessions to summarize.")
	default:
		m.setStatus("Writing a summary for " + m.placeName(pid, "") + "…")
		return m.startSummary(pid)
	}
	return nil
}

// SummaryInput builds what the model would see for one project, for the
// summarize command.
func SummaryInput(st *store.State, ss []core.Session, pid string, now time.Time, res map[string][]cloud.Resource) summary.Input {
	m := Model{opt: Options{Now: func() time.Time { return now }}, st: st, sessions: ss, cloudRes: res}
	return m.summaryInput(pid)
}
