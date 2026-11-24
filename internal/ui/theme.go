package ui

import (
	"strings"
	"sync"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Tokyo Night Moon, matching the user's Neovim colorscheme.
var (
	cDeep      = lipgloss.Color("#191b29")
	cPanel     = lipgloss.Color("#1e2030") // side panels and bars
	cBg        = lipgloss.Color("#222436") // the brief
	cRaised    = lipgloss.Color("#2f334d")
	cLine      = lipgloss.Color("#3b4261")
	cSubtle    = lipgloss.Color("#636da6")
	cSelection = lipgloss.Color("#2d3f76")
	cText      = lipgloss.Color("#c8d3f5")
	cMuted     = lipgloss.Color("#828bb8")
	cBlue      = lipgloss.Color("#82aaff")
	cCyan      = lipgloss.Color("#86e1fc")
	cTeal      = lipgloss.Color("#4fd6be")
	cGreen     = lipgloss.Color("#c3e88d")
	cAmber     = lipgloss.Color("#ffc777")
	cMagenta   = lipgloss.Color("#c099ff")
	cRed       = lipgloss.Color("#ff757f")
)

var (
	sText    = lipgloss.NewStyle().Foreground(cText)
	sMuted   = lipgloss.NewStyle().Foreground(cMuted)
	sSubtle  = lipgloss.NewStyle().Foreground(cSubtle)
	sLine    = lipgloss.NewStyle().Foreground(cLine)
	sBold    = lipgloss.NewStyle().Foreground(cText).Bold(true)
	sTitle   = lipgloss.NewStyle().Foreground(cBlue).Bold(true)
	sKicker  = lipgloss.NewStyle().Foreground(cSubtle).Bold(true)
	sAmber   = lipgloss.NewStyle().Foreground(cAmber)
	sGreen   = lipgloss.NewStyle().Foreground(cGreen)
	sCyan    = lipgloss.NewStyle().Foreground(cCyan)
	sTeal    = lipgloss.NewStyle().Foreground(cTeal)
	sMagenta = lipgloss.NewStyle().Foreground(cMagenta)
	sBlue    = lipgloss.NewStyle().Foreground(cBlue)
	sRed     = lipgloss.NewStyle().Foreground(cRed)
	sKey     = lipgloss.NewStyle().Foreground(cBlue).Bold(true)
	sNewTag  = lipgloss.NewStyle().Foreground(cDeep).Background(cMagenta).Bold(true)
	sBadge   = lipgloss.NewStyle().Foreground(cDeep).Background(cAmber).Bold(true)
	sPill    = lipgloss.NewStyle().Foreground(cMagenta).Background(cRaised)
	sSel     = lipgloss.NewStyle().Background(cSelection)
	sSelDim  = lipgloss.NewStyle().Background(cRaised)
	sBrand   = lipgloss.NewStyle().Foreground(cDeep).Background(cMagenta).Bold(true)
)

// Color meaning is fixed: amber means it needs you, green means moving,
// cyan means finished and awaiting your follow-up, muted pause bars mean
// parked / on hold, hollow means quiet.
func stateColor(s agents.State) lipgloss.Style {
	switch s {
	case agents.NeedsYou:
		return sAmber
	case agents.Working:
		return sGreen
	case agents.Ready:
		return sCyan
	case agents.OnHold:
		return sMuted
	}
	return sSubtle
}

func dot(s agents.State) string {
	switch s {
	case agents.NeedsYou, agents.Working:
		return stateColor(s).Render("●")
	case agents.Ready:
		return stateColor(s).Render("✓")
	case agents.OnHold:
		return stateColor(s).Render("‖")
	case agents.Offline:
		return sSubtle.Render("◌")
	case agents.Unknown:
		return sSubtle.Render("?")
	}
	return sSubtle.Render("○")
}

func stateWord(s agents.State) string {
	return stateColor(s).Render(s.String())
}

// proseWidth caps multi-line natural language for comfortable reading.
const maxProseWidth = 84

func proseWidth(w int) int {
	if w > maxProseWidth {
		return maxProseWidth
	}
	return w
}

// cleanProse strips inline markdown syntax from agent text so briefs and
// result snippets scan cleanly without losing meaning.
func cleanProse(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	// Strip leading markdown heading markers (e.g. "## Summary").
	for strings.HasPrefix(s, "#") {
		t := strings.TrimLeft(s, "#")
		if strings.HasPrefix(t, " ") {
			s = strings.TrimSpace(t)
		} else {
			break
		}
	}
	// Replace markdown links [label](target) with label.
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '[' {
			if closeBr := strings.IndexByte(s[i+1:], ']'); closeBr >= 0 {
				after := i + 1 + closeBr + 1
				if after < len(s) && s[after] == '(' {
					if closePar := strings.IndexByte(s[after+1:], ')'); closePar >= 0 {
						out.WriteString(s[i+1 : i+1+closeBr])
						i = after + 1 + closePar + 1
						continue
					}
				}
			}
		}
		out.WriteByte(s[i])
		i++
	}
	s = out.String()
	s = strings.ReplaceAll(s, "**", "")
	s = strings.ReplaceAll(s, "__", "")
	s = strings.ReplaceAll(s, "`", "")
	return strings.TrimSpace(s)
}

// wrap word-wraps plain or styled text to w columns.
func wrap(s string, w int) []string {
	if w < 4 {
		w = 4
	}
	return strings.Split(lipgloss.NewStyle().Width(w).Render(s), "\n")
}

// clamp keeps at most n lines, marking truncation with an ellipsis.
func clamp(lines []string, n int, w int) []string {
	if len(lines) <= n {
		return lines
	}
	out := append([]string{}, lines[:n]...)
	out[n-1] = ansi.Truncate(strings.TrimRight(out[n-1], " "), w-1, "") + sSubtle.Render("…")
	return out
}

func trunc(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// pad fits a line to exactly w columns.
func pad(s string, w int) string {
	s = ansi.Truncate(s, w, "")
	if d := w - lipgloss.Width(s); d > 0 {
		s += strings.Repeat(" ", d)
	}
	return s
}

// box fits lines to exactly w by h.
func box(lines []string, w, h int) []string {
	out := make([]string, h)
	for i := 0; i < h; i++ {
		if i < len(lines) {
			out[i] = pad(lines[i], w)
		} else {
			out[i] = strings.Repeat(" ", w)
		}
	}
	return out
}

// spread places left and right text at the ends of a w-column line.
func spread(left, right string, w int) string {
	if lipgloss.Width(right) >= w {
		return trunc(right, w)
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return trunc(left, w-lipgloss.Width(right)-1) + " " + right
	}
	return left + strings.Repeat(" ", gap) + right
}

func rule(w int) string { return sLine.Render(strings.Repeat("─", w)) }

var bgSeqs sync.Map

// bgSeq is the escape sequence that sets background c, or "" when the
// terminal has no color.
func bgSeq(c lipgloss.Color) string {
	if v, ok := bgSeqs.Load(c); ok {
		return v.(string)
	}
	r := lipgloss.NewStyle().Background(c).Render("x")
	seq := ""
	if i := strings.Index(r, "x"); i > 0 {
		seq = r[:i]
	}
	bgSeqs.Store(c, seq)
	return seq
}

// paint gives an already styled line a background that survives the
// resets inside it. Later paints win, so a card painted first keeps its
// own color inside a painted panel.
func paint(s string, c lipgloss.Color) string {
	open := bgSeq(c)
	if open == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\x1b[m", "\x1b[0m")
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+open)
	s = strings.ReplaceAll(s, "\x1b[49m", open)
	return open + s + "\x1b[0m"
}

// card is a raised block with a colored bar down its left edge. Content
// lines should fit in w-4 columns.
func card(c lipgloss.Color, w int, lines []string) []string {
	return cardOn(c, cRaised, false, w, lines)
}

// cardOn is a card on background bg. Selected cards use a thick bar; resting
// cards use a thin bar so the eye distinguishes active focus without double bars.
func cardOn(c, bg lipgloss.Color, selected bool, w int, lines []string) []string {
	glyph := "│"
	if selected {
		glyph = "▌"
	}
	bar := lipgloss.NewStyle().Foreground(c).Render(glyph)
	blank := bar + paint(strings.Repeat(" ", w-1), bg)
	out := []string{blank}
	for _, l := range lines {
		out = append(out, bar+paint(pad("  "+l, w-2)+" ", bg))
	}
	return append(out, blank)
}

// frame is a bordered card. Content lines should fit in w-4 columns; h of
// 0 fits the content.
func frame(c lipgloss.Color, w, h int, lines []string) []string {
	st := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c).Padding(0, 1).Width(w - 2)
	if h > 2 {
		st = st.Height(h - 2)
	}
	return strings.Split(st.Render(strings.Join(lines, "\n")), "\n")
}

// sideBySide joins two blocks of width w each with a gap.
func sideBySide(a, b []string, w, gap int) []string {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	a, b = box(a, w, n), box(b, w, n)
	out := make([]string, n)
	for i := range out {
		out[i] = a[i] + strings.Repeat(" ", gap) + b[i]
	}
	return out
}
