package ui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/anthonypoole/switchboard/internal/cloud"
	"github.com/anthonypoole/switchboard/internal/core"
)

type cloudMsg struct {
	res map[string][]cloud.Resource
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
	return cloud.Rows(cloud.Merge(by))
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

func (m Model) cloudStatus(r cloud.Row) string {
	t := r.Status() + " " + core.Ago(r.Last, m.opt.Now())
	switch {
	case r.Deleted:
		return sSubtle.Render(t)
	case r.Created:
		return sGreen.Render(t)
	}
	return sMuted.Render(t)
}

// cloudSection renders the CLOUD block for a project brief or a session.
func (m Model) cloudSection(ss []core.Session, w, max int, compact bool) []string {
	rows, note := m.cloudRows(ss)
	if len(rows) == 0 && note == "" {
		return nil
	}
	lines := []string{"", section("CLOUD", "named in commands the agents ran")}
	if compact {
		lines[1] = section("CLOUD", "")
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
		right := m.cloudStatus(r)
		rw := len(r.Status()) + 1 + len(core.Ago(r.Last, m.opt.Now()))
		name := sText
		if r.Deleted {
			name = sSubtle
		}
		var left string
		if compact {
			k := shortKind(r.Kind)
			left = sSubtle.Render(k+" ") + name.Render(trunc(r.Name, w-rw-len(k)-3))
		} else {
			kind := pad(sSubtle.Render(trunc(r.Kind, 19)), 20)
			where := r.Detail
			if where == "" {
				where = r.Scope
			}
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
