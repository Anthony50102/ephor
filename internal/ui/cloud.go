package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Anthony50102/ephor/internal/cloud"
	"github.com/Anthony50102/ephor/internal/core"
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
	cost := ""
	if usd, ok := r.Hourly(m.prices); ok && usd > 0 {
		cost = money(usd) + "/h"
	} else if usd > 0 {
		cost = "≥" + money(usd) + "/h"
	}
	s, n := m.cloudStatus0(r, did)
	if cost != "" {
		s = sText.Render(cost) + sSubtle.Render(" · ") + s
		n += len([]rune(cost)) + 3
	}
	return s, n
}

func (m Model) cloudStatus0(r cloud.Row, did string) (string, int) {
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
	switch {
	case r.Gone():
		didStyle = sSubtle
	case r.Billing() && m.untouched(r):
		didStyle = sAmber // paying for something no agent has touched lately
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
	return st.Render(now) + sSubtle.Render(" · ") + didStyle.Render(did), len([]rune(now)) + 3 + len([]rune(did))
}

// cloudSection renders the CLOUD block for a project brief or a session.
// In a brief each resource is a card, and so is the line that shows the
// rest; card ranges are relative to the returned lines.
func (m Model) cloudSection(ss []core.Session, w, limit int, compact bool) ([]string, []cardRef) {
	rows, note := m.cloudRows(ss)
	if len(rows) == 0 && note == "" {
		return nil, nil
	}
	var cards []cardRef
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
		lines[1] = sKicker.Render("CLOUD") + sSubtle.Render("  this session")
	}
	if !compact {
		if n := m.untouchedCount(rows); n > 0 {
			lines = append(lines, sAmber.Render("! ")+sText.Render(plural(n, "running resource")+" no agent has touched in "+untouchedWords)+
				sSubtle.Render(" (amber below) - still needed?"))
		}
	}
	// Everything costing money is listed, however many; the rest fills up
	// to limit.
	var picked []cloud.Row
	for _, r := range rows {
		if r.Bucket() == cloud.InBilling || len(picked) < limit {
			picked = append(picked, r)
		}
	}
	// Adapt the kind column to available width so narrow/docked panes keep
	// room for resource names and live status.
	narrowCloud := !compact && w < 82
	kindW := 0
	for _, r := range picked {
		k := r.Kind
		if narrowCloud {
			k = shortKind(k)
		}
		if n := len([]rune(k)); n > kindW {
			kindW = n
		}
	}
	if narrowCloud {
		kindW = min(max(kindW, 6), 10) + 2
	} else {
		kindW = min(max(kindW, 10), 19) + 2
	}
	nameW := 0
	for _, r := range picked {
		if len([]rune(r.Name)) > nameW {
			nameW = len([]rune(r.Name))
		}
	}
	if nameW > 30 {
		nameW = 30
	}
	grouped := !compact && len(picked) > 0 && picked[0].Bucket() != picked[len(picked)-1].Bucket()
	for i, r := range picked {
		if !compact && (i == 0 || r.Bucket() != picked[i-1].Bucket()) && (grouped || r.Bucket() == cloud.InBilling) {
			if i > 0 {
				lines = append(lines, "")
			}
			lines = append(lines, spread(bucketHead(r.Bucket()), m.bucketCost(rows, r.Bucket()), w))
		}
		right, rw := m.cloudStatus(r)
		name := sText
		if r.Gone() {
			name = sSubtle
		}
		id := resCard(r)
		sel := !compact && m.cardSel(id)
		opened := !compact && m.open(id)
		var left string
		if compact {
			k := shortKind(r.Kind)
			left = sSubtle.Render(k+" ") + name.Render(trunc(r.Name, w-rw-len(k)-3))
		} else {
			chev := sSubtle.Render("▸ ")
			if opened {
				chev = sBlue.Render("▾ ")
			} else if sel {
				chev = sBlue.Render("▸ ")
			}
			kl := r.Kind
			if narrowCloud {
				kl = shortKind(kl)
			}
			kind := chev + pad(sSubtle.Render(trunc(kl, kindW-2)), kindW)
			where := r.Where()
			room := w - rw - kindW - 4
			nw := nameW
			if nw > room {
				nw = room
			}
			if sel {
				name = sBold
			}
			left = kind + pad(name.Render(trunc(r.Name, nw)), nw)
			if rest := room - nw - 2; where != "" && rest > 8 {
				left += "  " + sSubtle.Render(trunc(where, rest))
			}
		}
		start := len(lines)
		rowLine := spread(left, right, w)
		if sel {
			rowLine = paint(pad(rowLine, w), cSelection)
		}
		lines = append(lines, rowLine)
		if compact {
			continue
		}
		if opened {
			lines = append(lines, indent(m.resourceDetail(r, w-4), 4)...)
			lines = append(lines, "")
		}
		cards = append(cards, cardRef{id: id, start: start, end: len(lines)})
	}
	var more []string
	if hidden := len(rows) - len(picked); hidden > 0 {
		more = append(more, fmt.Sprintf("%d more", hidden))
	}
	if note != "" {
		more = append(more, note)
	}
	switch {
	case len(more) > 0:
		moreLine := sSubtle.Render(trunc("+ "+strings.Join(more, ", "), w))
		if !compact && m.cardSel("cloud") {
			moreLine = paint(pad(sBlue.Render("▸ ")+sText.Render(trunc(strings.Join(more, ", "), w-2)), w), cSelection)
		}
		cards = append(cards, cardRef{id: "cloud", start: len(lines), end: len(lines) + 1})
		lines = append(lines, moreLine)
	case !compact && m.open("cloud") && len(rows) > 6:
		lessLine := sSubtle.Render("▾ show fewer")
		if m.cardSel("cloud") {
			lessLine = paint(pad(sBlue.Render("▾ ")+sText.Render("show fewer"), w), cSelection)
		}
		cards = append(cards, cardRef{id: "cloud", start: len(lines), end: len(lines) + 1})
		lines = append(lines, lessLine)
	}
	return lines, cards
}

func bucketHead(b cloud.Bucket) string {
	switch b {
	case cloud.InBilling:
		return sGreen.Render("● ") + sGreen.Bold(true).Render(b.Label())
	case cloud.InStopped:
		return sMuted.Render("○ " + b.Label())
	case cloud.InGone:
		return sSubtle.Render("✕ " + b.Label())
	}
	return sMuted.Render("◦ " + b.Label())
}
