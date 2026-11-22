# Switchboard

A keyboard-first control panel for the GitHub Copilot CLI and Claude Code
sessions you run in tmux. It answers three questions without making you scroll
through every window:

- **What needs me?** Sessions waiting on a question or an approval.
- **What is moving, and why?** Projects, what each one is for, and what its
  sessions are working on right now.
- **What finished?** Recent results, with markers for anything new since you
  last looked.

Works on Linux and macOS. It only reads state; it never types into your agents.

## Install

Requires Go 1.24+ and tmux 3.2+.

```sh
go build -o ~/.local/bin/switchboard ./cmd/switchboard
```

Optional tmux binding that opens the panel in its own window, or returns to it
if it is already open:

```tmux
bind-key S new-window -S -n switchboard switchboard
```

## Use

```sh
switchboard          # open the panel
switchboard list     # print sessions as a table
switchboard list --json
switchboard summarize nabu            # write one project's summary now
switchboard summarize --dry-run nabu  # print the prompt the model would see
```

New sessions appear in **Unassigned**. Create a project with `n`, describe it
with `e`, then select a session and press `m` to move it into the project (or
`a` to accept a suggestion). Inside tmux, `enter` switches your client to the
session's window; outside tmux it attaches.

Keys follow LazyVim and the Snacks explorer. Press `space` for a which-key menu
of actions.

| Key | Action |
| --- | --- |
| `j` / `k`, `gg` / `G` | move in the focused list |
| `h` / `l`, `ctrl-h` / `ctrl-l`, `tab` | projects column / sessions column |
| `H` / `L` | previous / next project (like buffers) |
| `]d` / `[d` | next / previous session that needs you (like diagnostics) |
| `ctrl-d` / `ctrl-u` | scroll the brief |
| `enter` | jump to the session's tmux window |
| `m` / `a` | move session to a project / accept suggestion |
| `n` `t` `e` `r` `d` | new project, new thread, describe, rename, delete |
| `/` | filter sessions (`esc` clears); `space space` searches all |
| `u` / `space s` | refresh now / rewrite this project's summary |
| `:q`, `:new <name>`, `:help` | command line |
| `space`, `?` | which-key menu, all keys |

`ctrl-h` and `ctrl-l` only reach Switchboard if your tmux
`vim-tmux-navigator` check treats it like Vim. Add `switchboard` to the
process pattern:

```tmux
vim_pattern='([^[:space:]]+/)?g?\.?(view|l?n?vim?x?|fzf|switchboard)(diff)?(-wrapped)?'
```

Colors are fixed in meaning: amber needs you, green is working, cyan is
finished and ready for your next instruction, hollow is quiet (nothing new in a
day). The palette is Tokyo Night Moon.

## How it works

Every 3 seconds Switchboard lists tmux panes, walks each pane's process tree to
find `copilot` or `claude`, and reads that agent's own session files.

- **Copilot CLI.** Each running process holds
  `~/.copilot/session-state/<id>/inuse.<pid>.lock`, which ties the process to
  its session. State comes from the tail of `events.jsonl`: an unanswered
  `permission.requested` or `ask_user` means it needs you, a finished turn
  means ready, anything else in flight means working. Logs can be gigabytes,
  so they are read backward from the end and cached until they change.
- **Claude Code.** Transcripts live in `~/.claude/projects/<encoded cwd>/`.
  When exactly one Claude process runs in a directory, Switchboard uses the
  newest transcript written since that process started. Approval prompts are
  not logged, so it also checks the bottom of the pane for a prompt.

Sessions are identified by the agent's session ID, not the tmux window, so a
session keeps its project when you resume it somewhere else. Sessions without
a known ID fall back to their tmux pane.

Projects, threads, and assignments are stored in
`$XDG_STATE_HOME/switchboard/state.json` (default
`~/.local/state/switchboard/state.json`). Use `--state` to point elsewhere.
If the file cannot be parsed, Switchboard exits rather than overwrite it. You can run several
panels at once: each edit locks the file, rereads it, and applies only that
edit, and every refresh picks up edits made elsewhere.

## Summaries

Each project gets a short, plain-language "happening now" paragraph, written
for someone who knows the project but not the code, plus one line per session.
If a project has no description, the model also suggests one (`e` to accept or
edit).

- **Input.** For each session in the project: its state, its last few asks,
  its last two completion summaries, and the agent's latest message. Long text
  is clipped. Run `switchboard summarize --dry-run <project>` to see the exact
  prompt.
- **When.** Only when that input changes, and at most once per project every
  5 minutes (`--summary-every`). One call runs at a time, starting with the
  project on screen. `space s` rewrites the current project now. A failed call
  keeps the last good summary and waits before retrying.
- **Who writes it.** By default the Copilot CLI, if it is installed, with
  `claude-haiku-4.5`, no tools, and no custom instructions. The prompt goes
  over stdin, so it does not show in `ps`. Calls take about 15-45 seconds.

| Flag | Env | Default |
| --- | --- | --- |
| `--summarizer` | `SWITCHBOARD_SUMMARIZER` | `auto`: `copilot` if on PATH, else off. Also `copilot`, `off`, or any shell command that reads the prompt on stdin and prints JSON, for example `'claude -p --model haiku'`. |
| `--summary-model` | `SWITCHBOARD_SUMMARY_MODEL` | `claude-haiku-4.5` |
| `--summary-every` | | `5m` |

Things to know:

- Snippets of your agents' transcripts go to the model provider behind your
  CLI, the same one your agents already use.
- Each Copilot summary call is a real Copilot session, so these short sessions
  appear in `copilot --resume` history. They run from
  `~/.local/state/switchboard/summarizer`, so they are easy to tell apart.
- Summaries are cached in `summaries.json` next to the state file. Deleting it
  is safe.

## Not yet

- Cloud resources (AKS, VMs) per project.
- Starting new agent sessions from the panel.
