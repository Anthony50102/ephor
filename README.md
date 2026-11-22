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
switchboard cloud nabu                # every cloud resource the project's sessions touched, checked with Azure
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
| `space c` | check cloud resources with Azure now |
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

## Cloud resources

The project brief and the session detail have a **CLOUD** section listing the
cloud resources the agents worked with: VMs, AKS clusters and node pools,
Kubernetes jobs, model deployments, registries, storage, SSH hosts, and so on.
Each row says whether the agent created, used, or deleted it, and when.

It comes from the shell commands the agents ran (`az`, `kubectl`, `helm`,
`ssh`, `scp`, `rsync`), read from their session logs. Then Switchboard asks
Azure what is still there (see below). From the logs alone:

- A row means the agent named that resource in a command, not that it still
  exists. "deleted" means the agent ran a delete; it might have failed.
- Names built at run time (`-n "vm-$(date +%s)"`, loop variables) are skipped.
  Variables set earlier in the same command (`RG=x; az ... -g $RG`) are
  resolved.
- Commands run on a remote host over SSH, or through scripts, are not seen.
- Read-only noise is left out: tokens, `az rest`, boards, repos, pipelines,
  role assignments, and listings. Network plumbing, identities, and resource
  groups the agent did not create are counted on one line instead of listed.
- Kubernetes jobs and Helm releases are folded into their cluster's row. The
  cluster is named after `--context` or the kubeconfig file name.

Logs can be gigabytes, so each one is read once and then only from where the
last scan stopped. Offsets and results live in `cloud.json` next to the state
file; deleting it forces a rescan (a couple of seconds, even with gigabyte logs).

The recent resources are also given to the summarizer, so the "happening now"
paragraph can say things like "training is running on the ring1 cluster."

### Checking with Azure

When `az` is installed and logged in, Switchboard sends one
[Azure Resource Graph](https://learn.microsoft.com/azure/governance/resource-graph/)
query (`az graph query`) for every resource the logs named. It takes about a
second, covers every subscription you can read, and runs again every 10
minutes, a minute after new resources show up, or when you press `space c`.
It only reads. The rows then say what Azure says now:

| Shown | Meaning |
| --- | --- |
| `running` (green) | VM or cluster is up |
| `stopped, still billed` (amber) | VM stopped from inside; it still holds its hardware |
| `delete failed?` (amber) | an agent deleted it, but it still exists |
| `deallocated` | VM is off and not billed for compute |
| `gone` | it no longer exists |

The detail column shows the VM size or node pools (`2× D4as_v5 + auto nodes`)
and the real resource group. Anything running or still billed moves to the
top, however long ago an agent touched it.

Limits:

- Only kinds whose logged name is the real Azure name are checked: VMs, scale
  sets, AKS, AI accounts, registries, storage, key vaults, container apps, web
  apps, and resource groups. Model deployments, Kubernetes jobs, and SSH hosts
  are not.
- A kubectl cluster is matched to AKS by its context or kubeconfig name. If
  those differ, it is just not checked.
- "gone" can also mean the resource is in a subscription you cannot read.

`--live off` (or `SWITCHBOARD_LIVE=off`) turns it off; `--live on` fails if
`az` is missing. `switchboard cloud` and `summarize` use it too, and the
summarizer is told what Azure said.

## Not yet

- Cost figures. Cost Management lags about a day, so sizes and power state
  stand in for now.
- Checking Kubernetes jobs with `kubectl`.
- Starting new agent sessions from the panel.
