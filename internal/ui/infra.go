package ui

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/anthonypoole/switchboard/internal/cloud"
)

// Spend, SSH, and cluster detail for the infrastructure list.

const (
	// A running resource no agent has touched for this long is flagged.
	untouchedAfter = 12 * time.Hour
	untouchedWords = "12 hours"

	kubeEvery   = time.Minute // refresh of an open cluster card
	kubeTimeout = 60 * time.Second
	priceRetry  = 10 * time.Minute

	debugImage = "mcr.microsoft.com/azurelinux/busybox:1.36"
)

type priceMsg struct {
	skus []cloud.SKU
	got  map[string]float64
	err  error
}

type kubeMsg struct {
	id string
	k  cloud.Kube
}

func resCard(r cloud.Row) string { return "res:" + r.ID() }

// money formats dollars: cents under $10, whole dollars above.
func money(usd float64) string {
	if usd < 10 {
		return fmt.Sprintf("$%.2f", usd)
	}
	s := fmt.Sprintf("%.0f", usd)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return "$" + s
}

func (m Model) untouched(r cloud.Row) bool {
	return m.opt.Now().Sub(r.Last) >= untouchedAfter
}

func (m Model) untouchedCount(rows []cloud.Row) int {
	n := 0
	for _, r := range rows {
		if r.Billing() && m.untouched(r) {
			n++
		}
	}
	return n
}

// bucketCost totals what the running bucket costs at list price.
func (m Model) bucketCost(rows []cloud.Row, b cloud.Bucket) string {
	if b != cloud.InBilling {
		return ""
	}
	total, known, any := 0.0, true, false
	for _, r := range rows {
		if r.Bucket() != b {
			continue
		}
		usd, ok := r.Hourly(m.prices)
		total += usd
		known = known && ok
		any = any || usd > 0
	}
	if !any {
		return ""
	}
	pre := "≈ "
	if !known {
		pre = "at least "
	}
	return sText.Render(pre+money(total)+"/h") + sSubtle.Render(" · "+money(total*24)+" a day at list price")
}

// ---- prices ----

// maybePrices fetches list prices for running sizes that are not cached.
func (m *Model) maybePrices() tea.Cmd {
	if m.opt.Prices == nil || m.priceBusy || m.prices == nil {
		return nil
	}
	now := m.opt.Now()
	if now.Sub(m.priceTried) < priceRetry {
		return nil
	}
	rows, _ := m.cloudRows(m.sessions)
	skus := m.prices.Missing(rows, now)
	if len(skus) == 0 {
		return nil
	}
	m.priceBusy, m.priceTried = true, now
	get := m.opt.Prices
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		got, err := cloud.FetchPrices(ctx, get, skus)
		return priceMsg{skus, got, err}
	}
}

func (m *Model) gotPrices(msg priceMsg) {
	m.priceBusy = false
	if msg.err != nil {
		m.setErr(fmt.Errorf("price list: %w", msg.err))
		return
	}
	m.prices.Merge(msg.skus, msg.got, m.opt.Now())
	if m.opt.PricePath != "" {
		_ = m.prices.Save(m.opt.PricePath) // a lost save only costs a refetch
	}
}

// ---- kubectl ----

func isCluster(r cloud.Row) bool { return r.Kind == "AKS cluster" || r.Kind == "Kubernetes" }

// kubeAccess is the kubectl flags for a cluster row, falling back to a
// context named after the cluster, which is what az aks get-credentials
// makes. Empty means the current context.
func kubeAccess(r cloud.Row) string {
	switch {
	case r.Kube != "":
		return r.Kube
	case r.Name != "default context":
		return "--context " + r.Name
	}
	return ""
}

// maybeKube refreshes the open cluster cards in view.
func (m *Model) maybeKube(force bool) tea.Cmd {
	if m.opt.Kubectl == nil {
		return nil
	}
	rows, _ := m.cloudRows(m.sessionsIn(m.scope()))
	var cmds []tea.Cmd
	home, _ := os.UserHomeDir()
	for _, r := range rows {
		id := resCard(r)
		if !isCluster(r) || !m.open(id) || m.kubeBusy[id] {
			continue
		}
		if k, ok := m.kube[id]; ok && !force && m.opt.Now().Sub(k.At) < kubeEvery {
			continue
		}
		m.kubeBusy[id] = true
		run, now := m.opt.Kubectl, m.opt.Now
		access := cloud.KubeArgs(kubeAccess(r), home)
		ns, all := cloud.Namespaces(r.Scope)
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), kubeTimeout)
			defer cancel()
			k := cloud.FetchKube(ctx, run, access, ns, all)
			k.At = now()
			return kubeMsg{id, k}
		})
	}
	return tea.Batch(cmds...)
}

func (m *Model) gotKube(msg kubeMsg) {
	delete(m.kubeBusy, msg.id)
	m.kube[msg.id] = msg.k
}

// ---- reaching a resource ----

// login is how to get a shell on a machine.
type login struct {
	tunnel, ssh string
	port        string // local end of the tunnel
	guess       bool   // no agent logged in this way
}

func (l login) line(portOpen bool) string {
	if l.tunnel == "" || portOpen {
		return l.ssh
	}
	return l.tunnel + " >/dev/null 2>&1 & sleep 10; " + l.ssh + "; kill %1"
}

func flagValue(cmd, flag string) string {
	ws := strings.Fields(cmd)
	for i := 0; i+1 < len(ws); i++ {
		if ws[i] == flag {
			return ws[i+1]
		}
	}
	return ""
}

func localSSH(ssh string) bool {
	return strings.Contains(ssh, "@127.0.0.1") || strings.Contains(ssh, "@localhost")
}

// loginFor works out how to log in to a VM or host, preferring exactly what
// an agent ran.
func loginFor(r cloud.Row) (login, bool) {
	var l login
	switch {
	case r.SSH != "":
		l.ssh = r.SSH
		if localSSH(r.SSH) {
			l.tunnel = r.Tunnel
		}
	case r.Kind == "VM" && r.Tunnel != "":
		l.tunnel, l.guess = r.Tunnel, true
		l.ssh = "ssh -p " + flagValue(r.Tunnel, "--port") + " azureuser@127.0.0.1"
	case r.Kind == "VM" && r.Group() != "":
		l.ssh, l.guess = "az ssh vm -g "+r.Group()+" -n "+r.Name, true
	case r.Kind == "SSH host":
		l.ssh, l.guess = "ssh "+r.Name, true
	default:
		return l, false
	}
	if l.tunnel != "" {
		l.port = flagValue(l.tunnel, "--port")
	}
	return l, true
}

func portOpen(port string) bool {
	if port == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// nodeShell is a kubectl command that opens a root shell on a node.
func nodeShell(access, node string) string {
	return strings.TrimSpace("kubectl "+access) + " debug node/" + node + " -it --image=" + debugImage
}

// pickNode is the node a copied shell command targets: a GPU node first.
func pickNode(k cloud.Kube) string {
	for _, n := range k.Nodes {
		if n.GPU > 0 && n.Ready {
			return n.Name
		}
	}
	if len(k.Nodes) > 0 {
		return k.Nodes[0].Name
	}
	return "<node>"
}

// cardRow is the resource behind the highlighted card.
func (m Model) cardRow() (cloud.Row, bool) {
	if m.focus != focusCenter || !strings.HasPrefix(m.cardID, "res:") {
		return cloud.Row{}, false
	}
	rows, _ := m.cloudRows(m.sessionsIn(m.scope()))
	for _, r := range rows {
		if resCard(r) == m.cardID {
			return r, true
		}
	}
	return cloud.Row{}, false
}

// copyAccess copies the command that reaches the highlighted resource.
func (m *Model) copyAccess() {
	r, ok := m.cardRow()
	if !ok {
		m.setErr(fmt.Errorf("highlight a machine or cluster under INFRASTRUCTURE to copy how to reach it"))
		return
	}
	var text string
	if isCluster(r) {
		text = nodeShell(kubeAccess(r), pickNode(m.kube[resCard(r)]))
	} else if l, ok := loginFor(r); ok {
		text = l.line(portOpen(l.port))
	} else {
		m.setErr(fmt.Errorf("no way to log in to a %s", strings.ToLower(r.Kind)))
		return
	}
	if m.opt.Copy == nil {
		m.setErr(fmt.Errorf("no clipboard"))
		return
	}
	if err := m.opt.Copy(text); err != nil {
		m.setErr(err)
		return
	}
	m.setStatus("Copied: " + text)
}

// connect opens a tmux window logged in to the highlighted machine, or, for
// a cluster, listing its nodes with a node shell command ready to finish.
func (m *Model) connect() {
	r, ok := m.cardRow()
	if !ok {
		m.setErr(fmt.Errorf("highlight a machine or cluster under INFRASTRUCTURE to open a shell on it"))
		return
	}
	if m.opt.Launch == nil {
		m.setErr(fmt.Errorf("opening a shell needs tmux"))
		return
	}
	home, _ := os.UserHomeDir()
	req := LaunchReq{Dir: home, Window: windowName("sh " + r.Name)}
	if isCluster(r) {
		a := kubeAccess(r)
		req.Command = strings.TrimSpace("kubectl "+a) + " get nodes -L agentpool,karpenter.sh/nodepool,node.kubernetes.io/instance-type"
		req.Pretype = strings.TrimSpace("kubectl "+a) + " debug -it --image=" + debugImage + " node/"
	} else if l, ok := loginFor(r); ok {
		req.Command = l.line(portOpen(l.port))
	} else {
		m.setErr(fmt.Errorf("no way to log in to a %s", strings.ToLower(r.Kind)))
		return
	}
	pane, err := m.opt.Launch(req)
	if err != nil {
		m.setErr(err)
		return
	}
	if m.opt.Show != nil {
		if err := m.opt.Show(pane); err == nil {
			return
		}
	}
	m.setStatus("Opened tmux window " + req.Window)
}

// ---- the expanded resource card ----

func (m Model) resourceDetail(r cloud.Row, w int) []string {
	const lw = 9
	now := m.opt.Now()
	var out []string
	add := func(k string, v string) {
		for i, l := range wrap(v, w-lw) {
			if i == 0 {
				out = append(out, pad(sSubtle.Render(k), lw)+l)
			} else {
				out = append(out, strings.Repeat(" ", lw)+l)
			}
		}
	}
	var where []string
	if isCluster(r) {
		if r.Live != nil && r.Live.Group != "" {
			where = append(where, "group "+r.Live.Group)
		}
		if r.Scope != "" && (r.Live == nil || r.Scope != r.Live.Group) {
			where = append(where, "ns "+r.Scope)
		}
	} else if g := r.Group(); g != "" && r.Kind != "Resource group" {
		where = append(where, "group "+g)
	}
	if l := r.Live; l != nil {
		if l.Location != "" {
			where = append(where, l.Location)
		}
		if l.Version != "" {
			where = append(where, "Kubernetes "+l.Version)
		}
	}
	if len(r.Hosts) > 0 {
		where = append(where, "address "+strings.Join(r.Hosts, ", "))
	}
	if r.Sub != "" {
		where = append(where, "subscription "+r.Sub)
	}
	if len(where) > 0 {
		add("where", sMuted.Render(strings.Join(where, " · ")))
	}
	if l := r.Live; l != nil && l.VMSize != "" {
		s := cloud.ShortSize(l.VMSize)
		if l.Spot {
			s += " spot"
		}
		if usd, ok := m.prices.Hourly(l.Location, l.VMSize, l.Spot); ok {
			s += sSubtle.Render(" · " + money(usd) + "/h list price when running")
		}
		add("size", sText.Render(s))
	}
	if r.Live != nil && !r.Live.Found {
		add("azure", sMuted.Render("not found: deleted, or in a subscription az cannot see"))
	}
	if r.Billing() && m.untouched(r) {
		add("note", sAmber.Render("running, and no agent has touched it in "+untouchedWords)+sMuted.Render(". Stop it if nothing needs it."))
	}
	if !isCluster(r) {
		if l, ok := loginFor(r); ok {
			out = append(out, m.loginLines(l, w, lw)...)
		}
	} else {
		out = append(out, m.clusterDetail(r, w)...)
	}
	who := strings.Join(r.By, ", ")
	if who == "" {
		who = "?"
	}
	add("agents", sMuted.Render(who)+sSubtle.Render(" · first seen "+agoWords(r.First, now)+" · "+plural(r.Count, "command")))
	return out
}

func agoWords(t, now time.Time) string {
	if t.IsZero() {
		return "?"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func (m Model) loginLines(l login, w, lw int) []string {
	var out []string
	cmd := func(first bool, s string) {
		for i, c := range wrap(sCyan.Render(s), w-lw) {
			k := ""
			if first && i == 0 {
				k = "log in"
			}
			out = append(out, pad(sSubtle.Render(k), lw)+c)
		}
	}
	first := true
	if l.tunnel != "" {
		cmd(true, l.tunnel+" &")
		first = false
	}
	cmd(first, l.ssh)
	hint := sKey.Render("y") + sSubtle.Render(" copies · ") + sKey.Render("s") + sSubtle.Render(" opens it in a new tmux window")
	if l.tunnel != "" {
		hint += sSubtle.Render(" (opens the tunnel first)")
	}
	if l.guess {
		hint = sAmber.Render("untried: no agent logged in this way") + sSubtle.Render(" · ") + hint
	}
	return append(out, strings.Repeat(" ", lw)+hint)
}

// clusterDetail is the topology of a cluster and the state of the
// workloads agents ran on it.
func (m Model) clusterDetail(r cloud.Row, w int) []string {
	id := resCard(r)
	k, have := m.kube[id]
	now := m.opt.Now()
	var out []string

	sub := ""
	switch {
	case m.opt.Kubectl == nil:
		sub = "kubectl is off"
	case m.kubeBusy[id] && !have:
		sub = "asking kubectl…"
	case have && k.Err != "" && len(k.Nodes) == 0:
		sub = "kubectl failed"
	case have:
		sub = "checked with kubectl " + agoWords(k.At, now)
	}
	if have && k.Context != "" {
		sub += " · current context " + k.Context
	}
	out = append(out, "", sKicker.Render("NODES")+"  "+sSubtle.Render(sub))
	if have && k.Err != "" {
		out = append(out, wrap(sRed.Render(trunc(k.Err, 3*w)), w)...)
	}

	var region string
	var pools []cloud.Pool
	var machines []cloud.Machine
	if r.Live != nil {
		region, pools, machines = r.Live.Location, r.Live.Pools, r.Live.Machines
	}
	used := map[string]bool{}
	nodeLines := func(match func(cloud.Node) bool) {
		for _, n := range k.Nodes {
			if used[n.Name] || !match(n) {
				continue
			}
			used[n.Name] = true
			out = append(out, "  "+m.nodeLine(k, n, w-2))
		}
	}
	for _, p := range pools {
		head := sText.Render(pad(p.Name, 14))
		var desc string
		switch {
		case p.Mode == "Machines":
			desc = "auto provisioned"
			if n := len(machines); n > 0 {
				desc += " · " + machineSummary(machines)
			}
		case p.Stopped:
			desc = p.Mode + " · stopped"
		default:
			desc = fmt.Sprintf("%s · %d× %s", p.Mode, p.Count, cloud.ShortSize(p.Size))
			if p.Spot {
				desc += " spot"
			}
			if p.Auto {
				desc += fmt.Sprintf(" · autoscale %d-%d", p.Min, p.Max)
			}
		}
		right := ""
		if p.Mode != "Machines" {
			if usd, ok := cloud.PoolHourly(m.prices, region, p); ok && usd > 0 {
				right = money(usd) + "/h"
			}
		} else if usd := machinesHourly(m, region, machines); usd > 0 {
			right = money(usd) + "/h"
		}
		out = append(out, spread(head+sMuted.Render(desc), sText.Render(right), w))
		if p.Mode == "Machines" {
			nodeLines(func(n cloud.Node) bool { return n.Auto })
		} else {
			nodeLines(func(n cloud.Node) bool { return !n.Auto && n.Pool == p.Name })
		}
	}
	// Nodes Azure did not list a pool for, or every node when Azure was
	// not asked.
	var rest []string
	for _, n := range k.Nodes {
		if !used[n.Name] {
			rest = append(rest, n.Pool)
		}
	}
	if len(rest) > 0 {
		sort.Strings(rest)
		last := ""
		for _, p := range rest {
			if p == last {
				continue
			}
			last = p
			out = append(out, sText.Render(p))
			nodeLines(func(n cloud.Node) bool { return n.Pool == p })
		}
	}
	if len(pools) == 0 && len(k.Nodes) == 0 && !have {
		out = append(out, wrap(sSubtle.Render("Azure has not described this cluster's pools. Press enter again later, or space c."), w)...)
	}
	if wp := k.Waiting(); len(wp) > 0 {
		reasons := map[string]int{}
		for _, p := range wp {
			reasons[p.Reason]++
		}
		var rs []string
		for k, n := range reasons {
			if k == "" {
				k = "no reason given"
			}
			rs = append(rs, fmt.Sprintf("%s ×%d", k, n))
		}
		sort.Strings(rs)
		out = append(out, sAmber.Render("! "+plural(len(wp), "pod")+" waiting for a node: ")+sMuted.Render(strings.Join(rs, ", ")))
	}

	out = append(out, m.workloadLines(r, k, have, w)...)

	a := kubeAccess(r)
	out = append(out, "", sKicker.Render("SHELL ON A NODE"))
	out = append(out, wrap(sCyan.Render(nodeShell(a, pickNode(k))), w)...)
	out = append(out, wrap(sSubtle.Render("then chroot /host · ")+sKey.Render("y")+sSubtle.Render(" copies · ")+sKey.Render("s")+sSubtle.Render(" opens a window that lists nodes with this command ready to finish"), w)...)
	if r.Kind == "AKS cluster" && r.Group() != "" && r.Kube == "" {
		out = append(out, wrap(sSubtle.Render("no kubeconfig yet? ")+sCyan.Render("az aks get-credentials -g "+r.Group()+" -n "+r.Name), w)...)
	}
	return out
}

func machineSummary(ms []cloud.Machine) string {
	n := map[string]int{}
	off := 0
	for _, m := range ms {
		if !m.Running {
			off++
			continue
		}
		k := cloud.ShortSize(m.Size)
		if m.Spot {
			k += " spot"
		}
		n[k]++
	}
	var parts []string
	for k, c := range n {
		parts = append(parts, fmt.Sprintf("%d× %s", c, k))
	}
	sort.Strings(parts)
	if off > 0 {
		parts = append(parts, fmt.Sprintf("%d stopped", off))
	}
	if len(parts) == 0 {
		return "none running"
	}
	return strings.Join(parts, " + ")
}

func machinesHourly(m Model, region string, ms []cloud.Machine) float64 {
	total := 0.0
	for _, x := range ms {
		if v, ok := m.prices.Hourly(region, x.Size, x.Spot); ok && x.Running {
			total += v
		}
	}
	return total
}

func (m Model) nodeLine(k cloud.Kube, n cloud.Node, w int) string {
	var parts []string
	if !n.Ready {
		parts = append(parts, sRed.Render("not ready"))
	}
	if n.GPU > 0 {
		parts = append(parts, sText.Render(plural(n.GPU, "GPU")))
	}
	if n.Auto {
		s := cloud.ShortSize(n.Size)
		if n.Spot {
			s += " spot"
		}
		parts = append(parts, sSubtle.Render(s))
	}
	parts = append(parts, sSubtle.Render("up "+strings.TrimSuffix(agoWords(n.Born, m.opt.Now()), " ago")))
	for _, p := range k.PodsOn(n.Name) {
		st := p.Phase
		if p.Reason != "" {
			st = p.Reason
		}
		name := p.Job
		if name == "" {
			name = p.Name
		}
		parts = append(parts, sMuted.Render(name)+" "+podStyle(st).Render(strings.ToLower(st)))
	}
	dot := sGreen.Render("● ")
	if !n.Ready {
		dot = sRed.Render("● ")
	}
	return trunc(dot+sMuted.Render(n.Name)+sSubtle.Render("  ")+strings.Join(parts, sSubtle.Render(" · ")), w)
}

func podStyle(st string) lipgloss.Style {
	switch strings.ToLower(st) {
	case "running", "succeeded":
		return sGreen
	case "pending", "containercreating":
		return sMuted
	}
	return sAmber
}

// workloadLines shows the jobs and deployments agents touched, with what
// kubectl says about them now, and counts the rest.
func (m Model) workloadLines(r cloud.Row, k cloud.Kube, have bool, w int) []string {
	if len(r.Workloads) == 0 && len(k.Jobs) == 0 {
		return nil
	}
	now := m.opt.Now()
	ws := append([]cloud.Resource(nil), r.Workloads...)
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].Last.After(ws[j].Last) })
	out := []string{"", sKicker.Render("WORKLOADS") + "  " + sSubtle.Render("the ones agents started or touched")}
	ours := map[string]bool{}
	const limit = 8
	nameW := 0
	for i, x := range ws {
		if i < limit && len([]rune(x.Name)) > nameW {
			nameW = len([]rune(x.Name))
		}
	}
	nameW = min(nameW, 36)
	for i, x := range ws {
		_, ns, _ := strings.Cut(x.Scope, "/")
		if ns == "all namespaces" {
			ns = ""
		}
		kind := strings.ToLower(strings.TrimPrefix(strings.TrimPrefix(x.Kind, "Kubernetes "), "Helm "))
		ours[kind+"/"+x.Name] = true
		if i >= limit {
			continue
		}
		state := sSubtle.Render("not checked")
		switch {
		case x.Deleted:
			state = sSubtle.Render("deleted by an agent " + agoWords(x.Last, now))
		case !have || (k.Err != "" && len(k.Jobs) == 0 && len(k.Deploys) == 0):
		case kind == "job":
			if j, ok := k.JobFor(x.Name, ns); ok {
				state = m.jobState(k, j)
			} else {
				state = sSubtle.Render("gone")
			}
		case kind == "deployment":
			if d, ok := k.DeployFor(x.Name, ns); ok {
				st := sGreen
				if d.Ready < d.Want {
					st = sAmber
				}
				state = st.Render(fmt.Sprintf("%d of %d ready", d.Ready, d.Want))
			} else {
				state = sSubtle.Render("gone")
			}
		default:
			state = ""
		}
		left := sSubtle.Render(pad(kind, 11)) + sText.Render(pad(trunc(x.Name, nameW), nameW)) + "  " + sSubtle.Render(ns)
		out = append(out, spread(left, state, w))
	}
	if len(ws) > limit {
		out = append(out, sSubtle.Render(fmt.Sprintf("+ %d older", len(ws)-limit)))
	}
	if have {
		n := map[string]int{}
		others := 0
		for _, j := range k.Jobs {
			if !ours["job/"+j.Name] {
				others++
				n[j.State()]++
			}
		}
		if others > 0 {
			var parts []string
			for _, s := range []string{"running", "waiting", "failed", "complete", "suspended"} {
				if n[s] > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", n[s], s))
				}
			}
			where := "in these namespaces"
			if k.AllNS {
				where = "in the cluster"
			}
			out = append(out, sSubtle.Render("+ "+plural(others, "other job")+" "+where+": "+strings.Join(parts, ", ")))
		}
	}
	return out
}

func (m Model) jobState(k cloud.Kube, j cloud.Job) string {
	now := m.opt.Now()
	switch j.State() {
	case "running":
		s := "running"
		if j.Want > 1 {
			s += fmt.Sprintf(" · %d of %d done", j.Succeeded, j.Want)
		}
		return sGreen.Render(s) + sSubtle.Render(" · started "+agoWords(j.Start, now))
	case "complete":
		return sMuted.Render("complete") + sSubtle.Render(" · "+agoWords(j.End, now))
	case "failed":
		s := "failed"
		if j.Reason != "" {
			s += " (" + j.Reason + ")"
		}
		return sRed.Render(s) + sSubtle.Render(" · started "+agoWords(j.Start, now))
	case "suspended":
		return sMuted.Render("suspended")
	}
	for _, p := range k.Pods {
		if p.Job == j.Name && p.Reason != "" {
			return sAmber.Render("waiting: " + p.Reason)
		}
	}
	return sMuted.Render("waiting")
}
