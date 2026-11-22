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

// Checkable reports whether a live check can say anything about r.
func Checkable(r Resource) bool {
	return armType(r) != "" && safeName.MatchString(r.Name)
}

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
	var res, groups, types []string
	tseen := map[string]bool{}
	for _, r := range rs {
		t := armType(r)
		if !Checkable(r) || len(seen) >= maxNames {
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
		if !tseen[t] {
			tseen[t] = true
			types = append(types, "'"+t+"'")
		}
	}
	sort.Strings(res)
	sort.Strings(types)
	sort.Strings(groups)
	const cols = "name, type, resourceGroup"
	var parts []string
	if len(res) > 0 {
		parts = append(parts, "Resources | where type in~ ("+strings.Join(types, ",")+") and name in~ ("+strings.Join(res, ",")+")"+
			" | project "+cols+", sku=tostring(sku.name), cap=toint(sku.capacity),"+
			" vmSize=tostring(properties.hardwareProfile.vmSize),"+
			" power=coalesce(tostring(properties.extended.instanceView.powerState.code), tostring(properties.powerState.code)),"+
			" pools=properties.agentPoolProfiles")
	}
	if len(groups) > 0 {
		parts = append(parts, "ResourceContainers | where type =~ 'microsoft.resources/subscriptions/resourcegroups' and name in~ ("+strings.Join(groups, ",")+") | project "+cols)
	}
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return parts[0] + " | union (" + parts[1] + ")"
}

type graphRow struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Group  string `json:"resourceGroup"`
	SKU    string `json:"sku"`
	Cap    *int   `json:"cap"`
	VMSize string `json:"vmSize"`
	Power  string `json:"power"`
	Pools  []struct {
		Name   string `json:"name"`
		Count  *int   `json:"count"`
		VMSize string `json:"vmSize"`
		Mode   string `json:"mode"`
		Power  struct {
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
	for _, g := range out.Data {
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
		cands := found[armType(r)+"/"+strings.ToLower(r.Name)]
		if len(cands) == 0 {
			if r.Kind != "Kubernetes" && complete {
				live[r.ID()] = Live{}
			}
			continue
		}
		live[r.ID()] = describe(pick(cands, r.Scope))
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

func describe(g graphRow) Live {
	l := Live{Found: true, Group: g.Group}
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
	case len(g.Pools) > 0:
		var ps []string
		for _, p := range g.Pools {
			switch {
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
	case g.Cap != nil && strings.Contains(g.Type, "virtualmachinescalesets"):
		l.Size = fmt.Sprintf("%d× %s", *g.Cap, shortSize(g.SKU))
	}
	return l
}

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
