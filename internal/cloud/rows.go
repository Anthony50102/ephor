package cloud

import (
	"fmt"
	"sort"
	"strings"
)

// Row is one line of the cloud section: a resource, with Kubernetes
// workloads folded into their cluster.
type Row struct {
	Resource
	Detail string // for clusters: namespaces and workloads
	Live   *Live  // nil when Azure was not asked
}

// Gone is true when the resource no longer exists, by the logs or by Azure.
func (r Row) Gone() bool {
	if r.Live != nil {
		return !r.Live.Found
	}
	return r.Deleted
}

// Billing is true when Azure says it is running or holding hardware.
func (r Row) Billing() bool {
	return r.Live != nil && (r.Live.State == "running" || strings.HasPrefix(r.Live.State, "stopped,"))
}

// Status is the most useful word for the row: the live state when known,
// else what the agent last did.
func (r Row) Status() string {
	if r.Live == nil {
		return r.Resource.Status()
	}
	switch {
	case r.Deleted && r.Live.Found:
		return "delete failed?"
	case r.Live.Found && r.Live.State == "":
		return r.Resource.Status()
	}
	return r.Live.Word()
}

// Bucket says how much attention a row deserves, in display order.
type Bucket int

const (
	InBilling Bucket = iota // running or holding hardware, or a delete that did not stick
	InStopped               // exists but off: cheap, and a cleanup candidate
	InUse                   // everything else that still exists, or that Azure was not asked about
	InGone                  // deleted
)

// Label names a bucket for a heading.
func (b Bucket) Label() string {
	return [...]string{"Running, costing money", "Stopped", "Used recently", "Gone"}[b]
}

// Bucket puts the row in a group.
func (r Row) Bucket() Bucket {
	switch {
	case r.Billing() || r.Status() == "delete failed?":
		return InBilling
	case r.Gone():
		return InGone
	case r.Live != nil && (r.Live.State == "deallocated" || r.Live.State == "stopped" || r.Live.State == "deallocating"):
		return InStopped
	}
	return InUse
}

// Where is the detail column: size and workloads, else the resource group.
func (r Row) Where() string {
	var parts []string
	if r.Live != nil && r.Live.Size != "" {
		parts = append(parts, r.Live.Size)
	}
	if r.Detail != "" {
		parts = append(parts, r.Detail)
	} else if g := r.Group(); g != "" {
		parts = append(parts, g)
	}
	return strings.Join(parts, " · ")
}

// Group is the real resource group when Azure said, else the logged scope.
func (r Row) Group() string {
	if r.Live != nil && r.Live.Group != "" && r.Kind != "Resource group" {
		return r.Live.Group
	}
	return r.Scope
}

// minor resources are plumbing. They are counted, not listed.
func minor(r Resource) string {
	switch {
	case strings.HasPrefix(r.Kind, "Azure network"), r.Kind == "Public IP":
		return "network"
	case r.Kind == "Managed identity":
		return "identity"
	case r.Kind == "Resource group" && !r.Created:
		return "resource group"
	}
	return ""
}

// Rows turns merged resources into display rows, most relevant first, and a
// note about what was left out.
func Rows(rs []Resource, live map[string]Live) ([]Row, string) {
	type work struct {
		kinds map[string]int
		last  Resource
	}
	workloads := map[string]*work{} // cluster name to its workloads
	hidden := map[string]int{}
	var rows []Row
	for _, r := range rs {
		if strings.HasPrefix(r.Kind, "Kubernetes ") || r.Kind == "Helm release" {
			cl, _, _ := strings.Cut(r.Scope, "/")
			w := workloads[cl]
			if w == nil {
				w = &work{kinds: map[string]int{}}
				workloads[cl] = w
			}
			w.kinds[strings.TrimPrefix(strings.TrimPrefix(r.Kind, "Kubernetes "), "Helm ")]++
			if r.Last.After(w.last.Last) {
				w.last = r
			}
			continue
		}
		if m := minor(r); m != "" {
			hidden[m]++
			continue
		}
		row := Row{Resource: r}
		if l, ok := live[r.ID()]; ok {
			row.Live = &l
		}
		rows = append(rows, row)
	}
	for i := range rows {
		r := &rows[i]
		if r.Kind != "Kubernetes" {
			continue
		}
		var parts []string
		if w := workloads[r.Name]; w != nil {
			var ks []string
			for k, n := range w.kinds {
				ks = append(ks, plural(n, k))
			}
			sort.Strings(ks)
			s := strings.Join(ks, ", ")
			if w.last.Name != "" {
				s += ", latest " + w.last.Name
				if w.last.Deleted {
					s += " (deleted)"
				}
			}
			parts = append(parts, s)
			if w.last.Last.After(r.Last) {
				r.Last = w.last.Last
			}
		}
		if r.Scope != "" {
			parts = append(parts, "ns "+r.Scope)
		}
		r.Detail = strings.Join(parts, " · ")
	}
	rows = foldAKS(rows)
	// Whatever is costing money comes first, however old; then what is
	// stopped; then recent work; then what no longer exists.
	sort.SliceStable(rows, func(i, j int) bool {
		if ti, tj := rows[i].Bucket(), rows[j].Bucket(); ti != tj {
			return ti < tj
		}
		return rows[i].Last.After(rows[j].Last)
	})
	var notes []string
	for _, k := range []string{"network", "identity", "resource group"} {
		if n := hidden[k]; n > 0 {
			label := k + " resource"
			if k != "network" {
				label = k
			}
			notes = append(notes, plural(n, label))
		}
	}
	return rows, strings.Join(notes, ", ")
}

// foldAKS merges a kubectl cluster into the AKS cluster of the same name, so
// one cluster is one row whether the agent reached it by az or kubectl.
func foldAKS(rows []Row) []Row {
	aks := map[string]int{}
	for i, r := range rows {
		if r.Kind == "AKS cluster" {
			aks[strings.ToLower(r.Name)] = i
		}
	}
	drop := map[int]bool{}
	for i := range rows {
		k := &rows[i]
		j, ok := aks[strings.ToLower(k.Name)]
		if k.Kind != "Kubernetes" || !ok {
			continue
		}
		a := rows[j]
		drop[j] = true
		k.Kind = "AKS cluster"
		k.Created = k.Created || a.Created
		k.Deleted = a.Deleted && !a.Last.Before(k.Last)
		k.Count += a.Count
		k.By = union(k.By, a.By)
		if a.Last.After(k.Last) {
			k.Last = a.Last
		}
		if a.First.Before(k.First) {
			k.First = a.First
		}
		if a.Live != nil {
			k.Live = a.Live
		}
	}
	if len(drop) == 0 {
		return rows
	}
	out := rows[:0:0]
	for i, r := range rows {
		if !drop[i] {
			out = append(out, r)
		}
	}
	return out
}

func union(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Line is a short description of a row for the summarizer.
func (r Row) Line() string {
	s := r.Kind + " " + r.Name
	if w := r.Where(); w != "" {
		s += " (" + w + ")"
	}
	s += ", " + r.Resource.Status() + " by an agent"
	if r.Live != nil {
		now := r.Status()
		if now == r.Resource.Status() {
			now = "exists"
		}
		s += ", Azure says now: " + now
	}
	return s
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	if strings.HasSuffix(word, "y") && !strings.HasSuffix(word, "ey") {
		return fmt.Sprintf("%d %sies", n, word[:len(word)-1])
	}
	return fmt.Sprintf("%d %ss", n, word)
}
