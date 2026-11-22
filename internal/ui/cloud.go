package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/anthonypoole/switchboard/internal/cloud"
	"github.com/anthonypoole/switchboard/internal/core"
)

type cloudMsg struct {
	res map[string][]cloud.Resource
}

type liveMsg struct {
	live map[string]cloud.Live
	keys string
	err  error
}

// DefaultLiveEvery is how often Azure is asked about known resources.
const DefaultLiveEvery = 10 * time.Minute

const (
	liveTimeout = 90 * time.Second
	liveSoonest = time.Minute // after new resources appear
)

// liveKeys fingerprints the resources a live check would ask about.
func (m Model) liveKeys() (string, []cloud.Resource) {
	var rs []cloud.Resource
	var ids []string
	for _, l := range m.cloudRes {
		for _, r := range l {
			if cloud.Checkable(r) {
				rs = append(rs, r)
				ids = append(ids, r.ID())
			}
		}
	}
	sort.Strings(ids)
	return strings.Join(ids, "\n"), rs
}

// maybeCheckLive asks Azure about the resources on a schedule: every
// LiveEvery, or a minute after new ones show up. One check runs at a time.
func (m *Model) maybeCheckLive(force bool) tea.Cmd {
	if m.opt.CheckLive == nil || m.liveBusy {
		return nil
	}
	keys, rs := m.liveKeys()
	if len(rs) == 0 {
		return nil
	}
	since := m.opt.Now().Sub(m.liveAt)
	if !force && !(keys != m.liveKeys0 && since >= liveSoonest) && since < m.opt.LiveEvery {
		return nil
	}
	m.liveBusy, m.liveAt = true, m.opt.Now()
	run := m.opt.CheckLive
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
		defer cancel()
		live, err := cloud.Check(ctx, run, rs)
		return liveMsg{live: live, keys: keys, err: err}
	}
}

func (m *Model) gotLive(msg liveMsg) {
	m.liveBusy = false
	m.liveKeys0 = msg.keys
	if msg.err != nil {
		// Keep the last answer; say so once rather than every ten minutes.
		if m.liveErr == "" {
			m.setErr(fmt.Errorf("cloud check failed: %w", msg.err))
		}
		m.liveErr = msg.err.Error()
		return
	}
	m.liveErr = ""
	m.cloudLive = msg.live
}

// scanCloud reads any new commands from the live sessions' logs in the
// background. Only one scan runs at a time and it owns the index while it
// does, so the index needs no lock.
func (m *Model) scanCloud() tea.Cmd {
	if m.opt.NoCloud || m.cloudBusy {
		return nil
	}
	var logs []string
	for _, s := range m.sessions {
		if s.Info.Log != "" {
			logs = append(logs, s.Info.Log)
		}
	}
	if len(logs) == 0 {
		return nil
	}
	if m.cloudIx == nil {
		m.cloudIx = cloud.LoadIndex(m.opt.CloudPath)
	}
	ix, path := m.cloudIx, m.opt.CloudPath
	m.cloudBusy = true
	return func() tea.Msg {
		changed := false
		res := map[string][]cloud.Resource{}
		for _, l := range logs {
			c, err := ix.Scan(l)
			changed = changed || c
			if err == nil {
				res[l] = ix.Resources(l)
			}
		}
		if changed && path != "" {
			_ = ix.Save(path) // a lost save only costs a rescan
		}
		return cloudMsg{res}
	}
}

// cloudRows merges what the given sessions touched.
func (m Model) cloudRows(ss []core.Session) ([]cloud.Row, string) {
	by := map[string][]cloud.Resource{}
	for _, s := range ss {
		if rs := m.cloudRes[s.Info.Log]; len(rs) > 0 && s.Info.Log != "" {
			by[s.Title()] = append(by[s.Title()], rs...)
		}
	}
	if len(by) == 0 {
		return nil, ""
	}
	return cloud.Rows(cloud.Merge(by), m.cloudLive)
}

var shortKinds = map[string]string{
	"Kubernetes": "k8s", "AKS cluster": "AKS", "AKS node pool": "AKS pool", "Model deployment": "model",
	"Azure AI account": "AI acct", "Container registry": "ACR", "Storage account": "storage",
	"SSH host": "ssh", "Container app": "app", "Container apps env": "app env", "VM scale set": "VMSS",
	"Resource group": "group", "Key vault": "vault", "VM": "VM",
}

func shortKind(k string) string {
	if s := shortKinds[k]; s != "" {
		return s
	}
	return strings.ToLower(strings.TrimPrefix(k, "Azure "))
}

// cloudStatus is the right-hand column: what Azure says now, when that
// adds anything, then what the agent last did and when. It also returns
// the plain width.
func (m Model) cloudStatus(r cloud.Row) (string, int) {
	did := r.Resource.Status() + " " + core.Ago(r.Last, m.opt.Now())
	now := r.Status()
	if now == r.Resource.Status() || (r.Deleted && r.Gone()) {
		now = ""
	}
	if r.Live == nil {
		switch {
		case r.Deleted:
			return sSubtle.Render(did), len(did)
		case r.Created:
			return sGreen.Render(did), len(did)
		}
		return sMuted.Render(did), len(did)
	}
	didStyle := sMuted
	if r.Gone() {
		didStyle = sSubtle
	}
	if now == "" {
		return didStyle.Render(did), len(did)
	}
	st := sMuted
	switch {
	case r.Billing() && now == "running":
		st = sGreen
	case r.Billing(), now == "delete failed?":
		st = sAmber
	case r.Gone():
		st = sSubtle
	}
	return st.Render(now) + sSubtle.Render(" · ") + didStyle.Render(did), len([]rune(now)) + 3 + len(did)
}

// cloudSection renders the CLOUD block for a project brief or a session.
func (m Model) cloudSection(ss []core.Session, w, max int, compact bool) []string {
	rows, note := m.cloudRows(ss)
	if len(rows) == 0 && note == "" {
		return nil
	}
	sub := "named in commands the agents ran"
	switch {
	case m.liveErr != "":
		sub += " · Azure check failed, space c retries"
	case m.cloudLive != nil:
		sub += " · checked with Azure " + core.Ago(m.liveAt, m.opt.Now())
	case m.liveBusy:
		sub += " · checking with Azure…"
	}
	lines := []string{"", section("INFRASTRUCTURE", sub)}
	if compact {
		lines[1] = sKicker.Render("CLOUD")
	}
	nameW := 0
	for i, r := range rows {
		if i < max && len([]rune(r.Name)) > nameW {
			nameW = len([]rune(r.Name))
		}
	}
	if nameW > 30 {
		nameW = 30
	}
	shown := 0
	for _, r := range rows {
		if shown == max {
			break
		}
		shown++
		right, rw := m.cloudStatus(r)
		name := sText
		if r.Gone() {
			name = sSubtle
		}
		var left string
		if compact {
			k := shortKind(r.Kind)
			left = sSubtle.Render(k+" ") + name.Render(trunc(r.Name, w-rw-len(k)-3))
		} else {
			kind := pad(sSubtle.Render(trunc(r.Kind, 19)), 20)
			where := r.Where()
			room := w - rw - 20 - 2
			nw := nameW
			if nw > room {
				nw = room
			}
			left = kind + pad(name.Render(trunc(r.Name, nw)), nw)
			if rest := room - nw - 2; where != "" && rest > 8 {
				left += "  " + sSubtle.Render(trunc(where, rest))
			}
		}
		lines = append(lines, spread(left, right, w))
	}
	var more []string
	if hidden := len(rows) - shown; hidden > 0 {
		more = append(more, fmt.Sprintf("%d older", hidden))
	}
	if note != "" {
		more = append(more, note)
	}
	if len(more) > 0 {
		lines = append(lines, sSubtle.Render(trunc("+ "+strings.Join(more, ", "), w)))
	}
	return lines
}
