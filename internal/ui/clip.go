package ui

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/Anthony50102/ephor/internal/tmux"
)

// Clipboard copies text to the tmux paste buffer, when inside tmux, and to
// the first system clipboard tool found.
func Clipboard(text string) error {
	done := false
	if tmux.InsideTmux() {
		cmd := exec.Command("tmux", "load-buffer", "-w", "-")
		cmd.Stdin = strings.NewReader(text)
		done = cmd.Run() == nil
	}
	for _, c := range [][]string{{"pbcopy"}, {"clip.exe"}, {"wl-copy"}, {"xclip", "-selection", "clipboard"}} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(text)
		if cmd.Run() == nil {
			return nil
		}
	}
	if done {
		return nil
	}
	return fmt.Errorf("no clipboard: install wl-copy or xclip, or run inside tmux")
}
