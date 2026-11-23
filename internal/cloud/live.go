package cloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Live is what Azure says about a resource right now.
type Live struct {
	Found bool
	State string // "running", "deallocated", "stopped", or "" when it has no power state
	Size  string // VM size, SKU, or AKS node pools
	Group string // the real resource group

	Location string
	VMSize   string // full Azure size name, for prices
	Spot     bool
	Version  string // Kubernetes version of an AKS cluster
	VM       string // for an SSH host: the VM that owns the address
	Pools    []Pool
	Machines []Machine // nodes made by node auto provisioning
}

// Pool is an AKS node pool.
type Pool struct {
	Name     string
	Mode     string // System, User, or Machines for auto provisioning
	Size     string
	Count    int
	Min, Max int
	Auto     bool // autoscaling
	Spot     bool
	Stopped  bool
}

// Machine is a VM that node auto provisioning made for a cluster.
type Machine struct {
	Name    string
	Size    string
	Spot    bool
	Running bool
}

// Word is the live state in a few words, or "" when it was not checked.
func (l Live) Word() string {
	switch {
	case !l.Found:
		return "gone"
	case l.State != "":
		return l.State
	}
	return "exists"
}

// armTypes are the kinds whose logged name is the real Azure name, so Azure
// can say whether they still exist.
var armTypes = map[string]string{
	"VM":                 "microsoft.compute/virtualmachines",
	"VM scale set":       "microsoft.compute/virtualmachinescalesets",
	"AKS cluster":        "microsoft.containerservice/managedclusters",
	"Azure AI account":   "microsoft.cognitiveservices/accounts",
	"Container registry": "microsoft.containerregistry/registries",
	"Container app":      "microsoft.app/containerapps",
	"Container apps env": "microsoft.app/managedenvironments",
	"Container apps job": "microsoft.app/jobs",
	"Storage account":    "microsoft.storage/storageaccounts",
	"Key vault":          "microsoft.keyvault/vaults",
	"Postgres server":    "microsoft.dbforpostgresql/flexibleservers",
	"Web app":            "microsoft.web/sites",
	"Function app":       "microsoft.web/sites",
	"ML workspace":       "microsoft.machinelearningservices/workspaces",
	"Batch account":      "microsoft.batch/batchaccounts",
	"Resource group":     "microsoft.resources/subscriptions/resourcegroups",
}

// kubectl names a cluster after its context or kubeconfig file, which is
// often, but not always, the AKS name. A match adds detail; a miss proves
// nothing.
const kubeType = "microsoft.containerservice/managedclusters"

const napTag = "karpenter.azure.com_cluster"

// Checkable reports whether a live check can say anything about r.
func Checkable(r Resource) bool {
	if r.Kind == "SSH host" {
		return ipv4.MatchString(r.Name)
	}
	return armType(r) != "" && safeName.MatchString(r.Name)
}

var ipv4 = regexp.MustCompile(`^\d{1,3}(\.\d{1,3}){3}$`)

func armType(r Resource) string {
	if r.Kind == "Kubernetes" {
		return kubeType
	}
	return armTypes[r.Kind]
}

// Only plain Azure names go into the query, so log text cannot change it.
var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._()-]{0,89}$`)

const (
	maxNames = 400
	pageSize = 1000
)

// Query builds one Resource Graph query covering every checkable resource.
func Query(rs []Resource) string {
	seen := map[string]bool{}
	var res, groups, types, clusters, ips []string
	tseen := map[string]bool{}
	for _, r := range rs {
		t := armType(r)
		if !Checkable(r) || len(seen) >= maxNames {
			continue
		}
		if r.Kind == "SSH host" {
			if !seen["ip/"+r.Name] {
				seen["ip/"+r.Name] = true
				ips = append(ips, "'"+r.Name+"'")
			}
			continue
		}
		n := strings.ToLower(r.Name)
		key := t + "/" + n
		if seen[key] {
			continue
		}
		seen[key] = true
		if r.Kind == "Resource group" {
			groups = append(groups, "'"+n+"'")
			continue
		}
		res = append(res, "'"+n+"'")
		if t == kubeType {
			clusters = append(clusters, "'"+n+"'")
		}
		if !tseen[t] {
			tseen[t] = true
			types = append(types, "'"+t+"'")
		}
	}
	sort.Strings(res)
	sort.Strings(types)
	sort.Strings(groups)
	sort.Strings(clusters)
	sort.Strings(ips)
	const cols = "name, type, resourceGroup, location"
	var parts []string
	if len(res) > 0 {
		parts = append(parts, "Resources | where type in~ ("+strings.Join(types, ",")+") and name in~ ("+strings.Join(res, ",")+")"+
			" | project "+cols+", sku=tostring(sku.name), cap=toint(sku.capacity),"+
			" vmSize=tostring(properties.hardwareProfile.vmSize),"+
			" power=coalesce(tostring(properties.extended.instanceView.powerState.code), tostring(properties.powerState.code)),"+
			" priority=tostring(properties.priority), k8s=tostring(properties.currentKubernetesVersion),"+
			" pools=properties.agentPoolProfiles")
	}
	if len(groups) > 0 {
		parts = append(parts, "ResourceContainers | where type =~ 'microsoft.resources/subscriptions/resourcegroups' and name in~ ("+strings.Join(groups, ",")+") | project "+cols)
	}
	if len(clusters) > 0 {
		// Node auto provisioning (managed Karpenter) makes plain VMs that
		// the cluster's pool list does not count. They carry its name in a tag.
		parts = append(parts, "Resources | where type =~ 'microsoft.compute/virtualmachines'"+
			" and tostring(tags['"+napTag+"']) in~ ("+strings.Join(clusters, ",")+")"+
			" | project "+cols+", vmSize=tostring(properties.hardwareProfile.vmSize),"+
			" power=tostring(properties.extended.instanceView.powerState.code),"+
			" aksCluster=tostring(tags['"+napTag+"']), priority=tostring(properties.priority)")
	}
	if len(ips) > 0 {
		// The VM behind each address agents logged in to, through its NIC.
		parts = append(parts, "Resources | where type =~ 'microsoft.network/publicipaddresses'"+
			" and tostring(properties.ipAddress) in ("+strings.Join(ips, ",")+")"+
			" | project ip=tostring(properties.ipAddress),"+
			" nic=tostring(split(tolower(tostring(properties.ipConfiguration.id)), '/ipconfigurations/')[0])"+
			" | join kind=inner (Resources | where type =~ 'microsoft.compute/virtualmachines'"+
			" | mv-expand n=properties.networkProfile.networkInterfaces"+
			" | project nic=tolower(tostring(n.id)), "+cols+", vmSize=tostring(properties.hardwareProfile.vmSize),"+
			" power=tostring(properties.extended.instanceView.powerState.code), priority=tostring(properties.priority)) on nic"+
			" | project "+cols+", vmSize, power, priority, ip")
	}
	if len(parts) == 0 {
		return ""
	}
	q := parts[0]
	if len(parts) > 1 {
		q += " | union (" + strings.Join(parts[1:], "), (") + ")"
	}
	return q
}

type graphRow struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Group    string `json:"resourceGroup"`
	Location string `json:"location"`
	K8s      string `json:"k8s"`
	IP       string `json:"ip"` // set on rows that answer an SSH host
	SKU      string `json:"sku"`
	Cap      *int   `json:"cap"`
	VMSize   string `json:"vmSize"`
	Power    string `json:"power"`
	// For machines made by node auto provisioning: their cluster.
	Cluster  string `json:"aksCluster"`
	Priority string `json:"priority"`
	Pools    []struct {
		Name     string `json:"name"`
		Count    *int   `json:"count"`
		VMSize   string `json:"vmSize"`
		Mode     string `json:"mode"`
		Min      *int   `json:"minCount"`
		Max      *int   `json:"maxCount"`
		Auto     *bool  `json:"enableAutoScaling"`
		Priority string `json:"scaleSetPriority"`
		Power    struct {
			Code string `json:"code"`
		} `json:"powerState"`
	} `json:"pools"`
}

// Match reads a Resource Graph reply and returns the live state of each
// checkable resource, keyed by Resource.ID. Kubernetes contexts that match
// no cluster are left out rather than called gone.
func Match(rs []Resource, reply []byte) (map[string]Live, error) {
	var out struct {
		Data []graphRow `json:"data"`
	}
	if err := json.Unmarshal(reply, &out); err != nil {
		return nil, fmt.Errorf("unreadable Resource Graph reply: %w", err)
	}
	found := map[string][]graphRow{}
	nodes := map[string][]graphRow{} // cluster name to its auto-provisioned machines
	ips := map[string]graphRow{}
	for _, g := range out.Data {
		if g.IP != "" {
			ips[g.IP] = g
			continue
		}
		if g.Cluster != "" {
			nodes[strings.ToLower(g.Cluster)] = append(nodes[strings.ToLower(g.Cluster)], g)
			continue
		}
		k := strings.ToLower(g.Type) + "/" + strings.ToLower(g.Name)
		found[k] = append(found[k], g)
	}
	// A full page may have cut matches off, so absence would prove nothing.
	complete := len(out.Data) < pageSize
	live := map[string]Live{}
	for _, r := range rs {
		if !Checkable(r) {
			continue
		}
		if r.Kind == "SSH host" {
			// An address that is not a VM's could be anything, so its
			// absence says nothing.
			if g, ok := ips[r.Name]; ok {
				l := describe(g, nil, complete)
				l.VM = g.Name
				live[r.ID()] = l
			}
			continue
		}
		cands := found[armType(r)+"/"+strings.ToLower(r.Name)]
		if len(cands) == 0 {
			if r.Kind != "Kubernetes" && complete {
				live[r.ID()] = Live{}
			}
			continue
		}
		g := pick(cands, r.Scope)
		live[r.ID()] = describe(g, nodes[strings.ToLower(g.Name)], complete)
	}
	return live, nil
}

// pick prefers the copy in the logged resource group, then a running one.
func pick(cands []graphRow, scope string) graphRow {
	for _, c := range cands {
		if scope != "" && strings.EqualFold(c.Group, scope) {
			return c
		}
	}
	for _, c := range cands {
		if strings.HasSuffix(strings.ToLower(c.Power), "running") {
			return c
		}
	}
	return cands[0]
}

func describe(g graphRow, nodes []graphRow, complete bool) Live {
	l := Live{Found: true, Group: g.Group, Location: g.Location, VMSize: g.VMSize, Spot: strings.EqualFold(g.Priority, "spot"), Version: g.K8s}
	switch p := strings.ToLower(strings.TrimPrefix(g.Power, "PowerState/")); p {
	case "":
	case "stopped":
		l.State = "stopped, still billed" // a VM stopped from inside still holds its hardware
		if strings.Contains(g.Type, "managedclusters") {
			l.State = "stopped"
		}
	default:
		l.State = p
	}
	switch {
	case g.VMSize != "":
		l.Size = shortSize(g.VMSize)
		if l.Spot {
			l.Size += " spot"
		}
	case len(g.Pools) > 0:
		var ps []string
		for _, p := range g.Pools {
			l.Pools = append(l.Pools, pool(p.Name, p.Mode, p.VMSize, p.Count, p.Min, p.Max, p.Auto, p.Priority, p.Power.Code))
			switch {
			case p.Mode == "Machines" && complete:
				ps = append(ps, machines(nodes)...)
			case p.Mode == "Machines" || p.Count == nil:
				ps = append(ps, "auto nodes")
			case *p.Count > 0 && !strings.EqualFold(p.Power.Code, "stopped"):
				ps = append(ps, fmt.Sprintf("%d× %s", *p.Count, shortSize(p.VMSize)))
			}
		}
		l.Size = strings.Join(ps, " + ")
		if l.Size == "" {
			l.Size = "no nodes"
		}
		for _, n := range nodes {
			l.Machines = append(l.Machines, Machine{Name: n.Name, Size: n.VMSize, Spot: strings.EqualFold(n.Priority, "spot"),
				Running: strings.HasSuffix(strings.ToLower(n.Power), "running")})
		}
		sort.Slice(l.Machines, func(i, j int) bool { return l.Machines[i].Name < l.Machines[j].Name })
	case g.Cap != nil && strings.Contains(g.Type, "virtualmachinescalesets"):
		l.Size = fmt.Sprintf("%d× %s", *g.Cap, shortSize(g.SKU))
	}
	return l
}

func pool(name, mode, size string, count, lo, hi *int, auto *bool, prio, power string) Pool {
	p := Pool{Name: name, Mode: mode, Size: size, Spot: strings.EqualFold(prio, "spot"), Stopped: strings.EqualFold(power, "stopped")}
	if count != nil {
		p.Count = *count
	}
	if lo != nil {
		p.Min = *lo
	}
	if hi != nil {
		p.Max = *hi
	}
	p.Auto = auto != nil && *auto
	return p
}

// ShortSize drops the Standard_ prefix from a VM size.
func ShortSize(s string) string { return shortSize(s) }

func shortSize(s string) string {
	return strings.TrimPrefix(s, "Standard_")
}

// Checker runs a Resource Graph query and returns the raw JSON reply.
type Checker func(ctx context.Context, query string) ([]byte, error)

// AzChecker asks Resource Graph through the az CLI.
func AzChecker() Checker {
	return func(ctx context.Context, q string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "az", "graph", "query", "--first", strconv.Itoa(pageSize), "-o", "json", "-q", q)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
		cmd.WaitDelay = 5 * time.Second
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if i := strings.IndexByte(msg, '\n'); i > 0 {
				msg = msg[:i]
			}
			return nil, fmt.Errorf("az graph query: %w: %s", err, msg)
		}
		return out.Bytes(), nil
	}
}

// Check asks Azure about every checkable resource in one query.
func Check(ctx context.Context, run Checker, rs []Resource) (map[string]Live, error) {
	q := Query(rs)
	if q == "" {
		return map[string]Live{}, nil
	}
	reply, err := run(ctx, q)
	if err != nil {
		return nil, err
	}
	return Match(rs, reply)
}

// machines counts running auto-provisioned nodes by size, for example
// "2× NC40ads_H100_v5 spot". Stopped ones are not billed for compute.
func machines(nodes []graphRow) []string {
	n := map[string]int{}
	for _, g := range nodes {
		if !strings.HasSuffix(strings.ToLower(g.Power), "running") {
			continue
		}
		k := shortSize(g.VMSize)
		if strings.EqualFold(g.Priority, "spot") {
			k += " spot"
		}
		n[k]++
	}
	var out []string
	for k, c := range n {
		out = append(out, fmt.Sprintf("%d× %s", c, k))
	}
	sort.Strings(out)
	return out
}
