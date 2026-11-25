// Package procs reads the process table with ps, which works on Linux and macOS.
package procs

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Proc is one process.
type Proc struct {
	PID  int
	PPID int
	Args string
}

// Table indexes processes by PID and by parent.
type Table struct {
	ByPID    map[int]Proc
	children map[int][]int
}

// Snapshot reads all processes.
func Snapshot() (Table, error) {
	out, err := exec.Command("ps", "-axww", "-o", "pid=,ppid=,args=").Output()
	if err != nil {
		return Table{}, err
	}
	return Parse(string(out)), nil
}

// Parse parses `ps -o pid=,ppid=,args=` output.
func Parse(out string) Table {
	t := Table{ByPID: map[int]Proc{}, children: map[int][]int{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		args := strings.TrimSpace(line)
		args = strings.TrimSpace(strings.TrimPrefix(args, f[0]))
		args = strings.TrimSpace(strings.TrimPrefix(args, f[1]))
		t.ByPID[pid] = Proc{PID: pid, PPID: ppid, Args: args}
		t.children[ppid] = append(t.children[ppid], pid)
	}
	return t
}

// Descendants returns root and its descendants in breadth-first order.
func (t Table) Descendants(root int) []Proc {
	var out []Proc
	queue := []int{root}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := t.ByPID[pid]; ok {
			out = append(out, p)
		}
		queue = append(queue, t.children[pid]...)
	}
	return out
}

// StartTime returns when pid started, to second precision.
func StartTime(pid int) (time.Time, error) {
	out, err := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return time.Time{}, err
	}
	return ParseLstart(string(out))
}

// ParseLstart parses ps lstart output such as "Sun Nov 22 13:28:00 2026".
func ParseLstart(s string) (time.Time, error) {
	return time.ParseInLocation("Mon Jan _2 15:04:05 2006", strings.Join(strings.Fields(s), " "), time.Local)
}

// Agent kinds.
const (
	Copilot = "copilot"
	Claude  = "claude"
)

// AgentKind identifies a Copilot CLI or Claude Code process from its arguments.
func AgentKind(args string) string {
	f := strings.Fields(args)
	if len(f) > 3 {
		f = f[:3]
	}
	for _, tok := range f {
		base := filepath.Base(tok)
		switch {
		case base == "copilot" || strings.Contains(tok, "@github/copilot"):
			return Copilot
		case base == "claude" || strings.Contains(tok, "claude-code"):
			return Claude
		}
	}
	return ""
}

func isSwitchboard(args string) bool {
	f := strings.Fields(args)
	if len(f) == 0 {
		return false
	}
	return filepath.Base(f[0]) == "switchboard"
}

// FindAgent returns the shallowest agent process under root, skipping any
// Switchboard subtree so transient summarizer sub-processes are never listed.
func (t Table) FindAgent(root int) (Proc, string, bool) {
	queue := []int{root}
	seen := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := t.ByPID[pid]; ok {
			if isSwitchboard(p.Args) {
				continue
			}
			if k := AgentKind(p.Args); k != "" {
				return p, k, true
			}
		}
		queue = append(queue, t.children[pid]...)
	}
	return Proc{}, "", false
}
