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
func Rows(rs []Resource) ([]Row, string) {
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
		rows = append(rows, Row{Resource: r})
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
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Deleted != rows[j].Deleted {
			return !rows[i].Deleted
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

// Line is a short description of a row for the summarizer.
func (r Row) Line() string {
	s := r.Kind + " " + r.Name
	if r.Detail != "" {
		s += " (" + r.Detail + ")"
	} else if r.Scope != "" {
		s += " in " + r.Scope
	}
	return s + ", " + r.Status()
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
