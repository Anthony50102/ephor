package ui

import (
	"strings"

	"github.com/anthonypoole/switchboard/internal/agents"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Tokyo Night Moon, matching the user's Neovim colorscheme.
var (
	cDeep      = lipgloss.Color("#191b29")
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
	sSel     = lipgloss.NewStyle().Background(cSelection)
	sSelDim  = lipgloss.NewStyle().Background(cRaised)
	sBrand   = lipgloss.NewStyle().Foreground(cDeep).Background(cMagenta).Bold(true)
)

// Color meaning is fixed: amber means it needs you, green means moving,
// cyan means finished and ready for you, hollow means quiet.
func stateColor(s agents.State) lipgloss.Style {
	switch s {
	case agents.NeedsYou:
		return sAmber
	case agents.Working:
		return sGreen
	case agents.Ready:
		return sCyan
	}
	return sSubtle
}

func dot(s agents.State) string {
	switch s {
	case agents.NeedsYou, agents.Working:
		return stateColor(s).Render("●")
	case agents.Ready:
		return stateColor(s).Render("✓")
	case agents.Unknown:
		return sSubtle.Render("?")
	}
	return sSubtle.Render("○")
}

func stateWord(s agents.State) string {
	return stateColor(s).Render(s.String())
}

// accent draws a block with a colored bar on its left edge.
func accent(c lipgloss.Color, w int, body string) []string {
	st := lipgloss.NewStyle().
		Border(lipgloss.ThickBorder(), false, false, false, true).
		BorderForeground(c).PaddingLeft(1).Width(w - 1)
	return strings.Split(st.Render(body), "\n")
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
