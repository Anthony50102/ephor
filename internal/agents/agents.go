// Package agents reads Copilot CLI and Claude Code session state from disk.
package agents

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"time"
)

// State is what a session is doing from the user's point of view.
type State int

// States are ordered from least to most urgent so rollups can take the max.
const (
	Unknown  State = iota
	Ended          // process gone or session shut down
	Quiet          // finished, and nothing has happened for a long time
	Ready          // finished a turn; waiting for the next instruction
	Working        // a turn is in progress
	NeedsYou       // blocked on a question or permission prompt
)

func (s State) String() string {
	switch s {
	case Ended:
		return "ended"
	case Quiet:
		return "quiet"
	case Ready:
		return "ready"
	case Working:
		return "working"
	case NeedsYou:
		return "needs you"
	}
	return "unknown"
}

// Result is a finished piece of work, such as a task_complete summary.
type Result struct {
	At   time.Time
	Text string
}

// Info is what Switchboard knows about one agent session.
type Info struct {
	Kind      string
	ID        string
	Name      string
	Cwd       string
	Repo      string
	Branch    string
	State     State
	Reason    string // why it needs you, for example the pending question
	Scheduled string // active scheduled check-in cadence, e.g. "every 20m"
	Ask       string // latest instruction typed by the user
	AskAt     time.Time
	Results   []Result // newest first
	Asks      []Result // recent instructions from the user, newest first
	FirstAsk  string   // opening user prompt when the session has no milestone arc
	Arc       []string // chronological milestone titles across the session's life
	Said      string   // the agent's latest reply, for summaries
	SaidAt    time.Time
	UpdatedAt time.Time
	Log       string // the session's event log or transcript
}

const (
	maxResults   = 5
	maxAsks      = 4
	maxArc       = 8
	maxSaid      = 1200
	maxScanBytes = 24 << 20
	maxHeadBytes = 128 << 10
	maxLineBytes = 16 << 20
)

// chunkSize is a variable so tests can exercise lines that span chunks.
var chunkSize = 1 << 20

// scanForward calls fn for each complete line from the start of path, stopping
// when fn returns false or limit bytes have been read.
func scanForward(path string, limit int64, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, limit)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	data := buf[:n]
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			if int64(n) < limit {
				fn(data)
			}
			break
		}
		if !fn(data[:i]) {
			break
		}
		data = data[i+1:]
	}
	return nil
}

// sampleArc reduces a long chronological list of milestones to at most limit
// items while preserving the beginning, middle progression, and latest entries.
func sampleArc(titles []string, limit int) []string {
	if len(titles) <= limit || limit < 3 {
		return titles
	}
	out := make([]string, 0, limit)
	seen := map[int]bool{}
	add := func(i int) {
		if i >= 0 && i < len(titles) && !seen[i] {
			seen[i] = true
			out = append(out, titles[i])
		}
	}
	// Keep the first 2 milestones and last 3, then fill evenly across the middle.
	n := len(titles)
	add(0)
	add(1)
	midSlots := limit - 5
	for k := 1; k <= midSlots; k++ {
		idx := 1 + k*(n-4)/(midSlots+1)
		add(idx)
	}
	for i := n - 3; i < n; i++ {
		add(i)
	}
	return out
}

// scanBackward calls fn for each complete line of path, newest first, until fn
// returns false or limit bytes have been read.
func scanBackward(path string, limit int64, fn func(line []byte) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	off := st.Size()
	var carry []byte
	var read int64
	for off > 0 && read < limit {
		n := int64(chunkSize)
		if n > off {
			n = off
		}
		off -= n
		read += n
		buf := make([]byte, n, int(n)+len(carry))
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		buf = append(buf, carry...)
		for {
			i := bytes.LastIndexByte(buf, '\n')
			if i < 0 {
				break
			}
			line := buf[i+1:]
			buf = buf[:i]
			if len(bytes.TrimSpace(line)) > 0 && !fn(line) {
				return nil
			}
		}
		carry = buf
		if len(carry) > maxLineBytes {
			carry = nil // pathological line; skip it rather than grow without bound
		}
	}
	if off == 0 && len(bytes.TrimSpace(carry)) > 0 {
		fn(carry)
	}
	return nil
}

// eventType reads the leading "type" field without decoding the whole line.
func eventType(line []byte) string {
	const p = `{"type":"`
	if !bytes.HasPrefix(line, []byte(p)) {
		return ""
	}
	rest := line[len(p):]
	if i := bytes.IndexByte(rest, '"'); i >= 0 {
		return string(rest[:i])
	}
	return ""
}

// oneLine collapses whitespace and trims to n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if n > 0 && len(r) > n {
		return strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

// FirstSentence returns the first line or sentence of s, for headlines.
func FirstSentence(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n"); i > 0 {
		s = s[:i]
	}
	s = strings.TrimLeft(s, "#*- ")
	for _, sep := range []string{". ", "? ", "! "} {
		if i := strings.Index(s, sep); i > 20 {
			s = s[:i+1]
			break
		}
	}
	return oneLine(strings.ReplaceAll(s, "**", ""), n)
}

// noteAsk records a user instruction while scanning newest first.
func (info *Info) noteAsk(text string, at time.Time) {
	text = strings.TrimSpace(text)
	if text == "" || len(info.Asks) >= maxAsks {
		return
	}
	if len(info.Asks) == 0 {
		info.Ask, info.AskAt = oneLine(text, 400), at
	}
	info.Asks = append(info.Asks, Result{At: at, Text: oneLine(text, 400)})
}

// noteSaid records the agent's newest reply while scanning newest first.
func (info *Info) noteSaid(text string, at time.Time) {
	text = strings.TrimSpace(text)
	if text == "" || info.Said != "" {
		return
	}
	if len(text) > maxSaid {
		text = text[:maxSaid] + "…"
	}
	info.Said, info.SaidAt = text, at
}
