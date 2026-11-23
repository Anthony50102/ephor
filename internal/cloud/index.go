package cloud

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Resource is everything one session did to one resource.
type Resource struct {
	Kind    string    `json:"kind"`
	Name    string    `json:"name"`
	Scope   string    `json:"scope,omitempty"`
	Sub     string    `json:"sub,omitempty"`
	Created bool      `json:"created,omitempty"` // the session created it
	Deleted bool      `json:"deleted,omitempty"` // the session deleted it and has not recreated it
	First   time.Time `json:"first"`
	Last    time.Time `json:"last"`
	Count   int       `json:"count"`
	By      []string  `json:"by,omitempty"` // sessions, filled in by Merge

	// The latest way an agent reached it; see Touch.
	SSH    string `json:"ssh,omitempty"`
	Tunnel string `json:"tunnel,omitempty"`
	Kube   string `json:"kube,omitempty"`
}

// scoped kinds have names that are only unique within their parent.
var scoped = map[string]bool{
	"Model deployment": true, "AKS node pool": true, "Batch pool": true, "Batch job": true, "ML compute": true,
	"Kubernetes job": true, "Kubernetes cronjob": true, "Kubernetes deployment": true,
	"Kubernetes statefulset": true, "Helm release": true,
}

// ID identifies a resource across commands and sessions. Most Azure names
// are unique per kind, and commands often omit the resource group, so the
// scope only counts where names repeat across parents.
func (r Resource) ID() string {
	id := r.Kind + "\x00" + strings.ToLower(r.Name)
	if scoped[r.Kind] {
		id += "\x00" + strings.ToLower(r.Scope)
	}
	return id
}

// addScope records another parent. Kubernetes clusters collect namespaces;
// everything else keeps the first scope it was given.
func addScope(cur, next, kind string) string {
	switch {
	case next == "" || cur == next:
		return cur
	case cur == "":
		return next
	case kind != "Kubernetes":
		return cur
	}
	parts := strings.Split(cur, ", ")
	for _, p := range parts {
		if p == next {
			return cur
		}
	}
	if len(parts) >= 4 {
		return cur
	}
	return cur + ", " + next
}

// Status is the last thing that happened to the resource, in words.
func (r Resource) Status() string {
	switch {
	case r.Deleted:
		return "deleted"
	case r.Created:
		return "created"
	}
	return "used"
}

func apply(list []Resource, t Touch, at time.Time) []Resource {
	r := Resource{Kind: t.Kind, Name: t.Name, Scope: t.Scope, SSH: t.SSH, Tunnel: t.Tunnel, Kube: t.Kube}
	id := r.ID()
	for i := range list {
		if list[i].ID() != id {
			continue
		}
		p := &list[i]
		p.takeAccess(r, !at.Before(p.Last))
		if at.After(p.Last) {
			p.Last = at
		}
		if p.Sub == "" {
			p.Sub = t.Sub
		}
		p.Scope = addScope(p.Scope, t.Scope, p.Kind)
		p.Count++
		switch t.Verb {
		case Created:
			p.Created, p.Deleted = true, false
		case Deleted:
			p.Deleted = true
		}
		return list
	}
	r.Sub, r.First, r.Last, r.Count = t.Sub, at, at, 1
	r.Created, r.Deleted = t.Verb == Created, t.Verb == Deleted
	return append(list, r)
}

const maxPerLog = 200

// Merge combines resources from several sessions, newest first, with live
// resources before deleted ones.
func Merge(bySession map[string][]Resource) []Resource {
	idx := map[string]int{}
	var out []Resource
	names := make([]string, 0, len(bySession))
	for n := range bySession {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, r := range bySession[n] {
			id := r.ID()
			i, ok := idx[id]
			if !ok {
				r.By = []string{n}
				idx[id] = len(out)
				out = append(out, r)
				continue
			}
			p := &out[i]
			p.takeAccess(r, r.Last.After(p.Last))
			p.Count += r.Count
			p.Created = p.Created || r.Created
			if r.Last.After(p.Last) {
				p.Last, p.Deleted = r.Last, r.Deleted
			}
			if r.First.Before(p.First) {
				p.First = r.First
			}
			if p.Sub == "" {
				p.Sub = r.Sub
			}
			for _, sc := range strings.Split(r.Scope, ", ") {
				p.Scope = addScope(p.Scope, sc, p.Kind)
			}
			p.By = append(p.By, n)
		}
	}
	Sort(out)
	return out
}

// Sort orders resources for display.
func Sort(rs []Resource) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Deleted != rs[j].Deleted {
			return !rs[i].Deleted
		}
		return rs[i].Last.After(rs[j].Last)
	})
}

// ---- index ----

// version changes whenever parsing changes, so old offsets are rescanned.
const version = 4

// Index remembers how far each session log has been read and what it found,
// so that a log of several gigabytes is read once and then only its tail.
type Index struct {
	Version int             `json:"version"`
	Logs    map[string]*Log `json:"logs"`
}

// Log is the scan state for one session log.
type Log struct {
	Offset    int64      `json:"offset"`
	Resources []Resource `json:"resources,omitempty"`
	State     State      `json:"state"`
}

// LoadIndex reads an index, returning an empty one when it is missing,
// unreadable, or from an older parser.
func LoadIndex(path string) *Index {
	ix := &Index{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, ix)
	}
	if ix.Version != version || ix.Logs == nil {
		ix = &Index{Version: version, Logs: map[string]*Log{}}
	}
	return ix
}

// Save writes the index atomically, dropping logs that no longer exist.
func (ix *Index) Save(path string) error {
	for p := range ix.Logs {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			delete(ix.Logs, p)
		}
	}
	b, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Scan reads whatever has been appended to a session log since the last
// scan and reports whether anything was read.
func (ix *Index) Scan(path string) (changed bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	l := ix.Logs[path]
	if l == nil {
		l = &Log{}
		ix.Logs[path] = l
	}
	if st.Size() < l.Offset {
		*l = Log{} // rewritten; start over
	}
	if st.Size() == l.Offset {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err := f.Seek(l.Offset, io.SeekStart); err != nil {
		return false, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	off := l.Offset
	for {
		line, err := r.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// A huge line; tool calls with commands we care about are never
			// this large, so skip to its end.
			n := int64(len(line))
			for err == bufio.ErrBufferFull {
				line, err = r.ReadSlice('\n')
				n += int64(len(line))
			}
			if err != nil {
				break
			}
			off += n
			continue
		}
		if err != nil {
			break // a partial last line is read next time
		}
		off += int64(len(line))
		for _, c := range lineCommands(line) {
			for _, t := range ParseWith(c.cmd, &l.State) {
				l.Resources = apply(l.Resources, t, c.at)
			}
		}
	}
	changed = off != l.Offset
	l.Offset = off
	if len(l.Resources) > maxPerLog {
		Sort(l.Resources)
		l.Resources = l.Resources[:maxPerLog]
	}
	return changed, nil
}

// Resources returns what has been found in one log so far.
func (ix *Index) Resources(path string) []Resource {
	if l := ix.Logs[path]; l != nil {
		return append([]Resource(nil), l.Resources...)
	}
	return nil
}

type command struct {
	cmd string
	at  time.Time
}

var (
	copilotStart = []byte(`{"type":"tool.execution_start"`)
	claudeTool   = []byte(`"tool_use"`)
)

// lineCommands extracts shell commands from one Copilot CLI event or one
// Claude Code transcript entry.
func lineCommands(line []byte) []command {
	switch {
	case bytes.HasPrefix(line, copilotStart):
		var e struct {
			Data struct {
				Arguments json.RawMessage `json:"arguments"`
			} `json:"data"`
			Timestamp time.Time `json:"timestamp"`
		}
		if json.Unmarshal(line, &e) != nil {
			return nil
		}
		var args struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(e.Data.Arguments, &args) != nil || args.Command == "" {
			return nil
		}
		return []command{{args.Command, e.Timestamp}}
	case bytes.Contains(line, claudeTool) && bytes.Contains(line, []byte(`"assistant"`)):
		var e struct {
			Type      string    `json:"type"`
			Timestamp time.Time `json:"timestamp"`
			Message   struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
			return nil
		}
		var blocks []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Input struct {
				Command string `json:"command"`
			} `json:"input"`
		}
		if json.Unmarshal(e.Message.Content, &blocks) != nil {
			return nil
		}
		var out []command
		for _, b := range blocks {
			if b.Type == "tool_use" && b.Input.Command != "" {
				out = append(out, command{b.Input.Command, e.Timestamp})
			}
		}
		return out
	}
	return nil
}
