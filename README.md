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

### Starting and steering agents

Press `o` in a project or thread to start Copilot there. Switchboard asks for
the folder (it suggests where that project's latest session runs) and an
optional first task. It then opens a new tmux window next to the project's
other sessions and types this into it:

```sh
copilot --session-id <new uuid> --name '<project or thread>' -i '<task>'
```

Switchboard chooses the session ID, so the session is filed under the project
before Copilot starts. It appears and is selected within a few seconds. The
command is typed into a shell rather than run directly, so your shell setup
applies and the window keeps a shell when Copilot exits. Agents still live in
tmux, so they keep running when Switchboard closes.

Press `i` on a session to type to it without leaving. The middle pane shows a
live view of its terminal. `enter` sends your text and presses Enter, so `1`
then `enter` answers a numbered approval prompt. `enter` on an empty line only
presses Enter. `esc` returns to the panel.

Keys follow LazyVim and the Snacks explorer. Press `space` for a which-key menu
of actions.

The screen has three panels: projects on the left, the brief in the middle,
and sessions on the right. The brief is a column of cards. Focus it with `l`
or `enter`, step through cards with `j` / `k`, and press `enter` to open a
project or thread card or to expand any other card in place. Highlighting a
session card also selects that session, so `i`, `m`, and `a` act on it. Hide
the side panels the way you would in LazyVim: `space e` for the projects panel
(like the explorer), `space l` for the sessions list, and `space z` for both.
With the sessions panel hidden, the list moves into the bottom of the brief.

| Key | Action |
| --- | --- |
| `j` / `k`, `gg` / `G` | move in the focused panel: projects, cards, or sessions |
| `h` / `l`, `ctrl-h` / `ctrl-l`, `tab` | move one panel left / right, like `ctrl-w h` / `ctrl-w l` |
| `enter` on a card | open a project or thread card; expand any other card (`esc` collapses) |
| `space e` / `space l` / `space z` | toggle the projects panel / sessions panel / both (zen) |
| `H` / `L` | previous / next project (like buffers) |
| `]d` / `[d` | next / previous session that needs you (like diagnostics) |
| `ctrl-d` / `ctrl-u`, `ctrl-e` / `ctrl-y`, mouse wheel | scroll the middle pane by half a page or a line. The wheel over the sidebar or session list moves the selection. Hold `shift` to select text with the mouse. |
| `enter` in the sessions panel | jump to the session's tmux window |
| `i` / `o` | type to the session / start Copilot in this project or thread |
| `m` / `a` | move session to a project / accept suggestion |
| `n` `t` `e` `r` `d` | new project, new thread, describe, rename, delete |
| `/` | filter sessions (`esc` clears); `space space` searches all |
| `u` / `space s` | refresh now / rewrite this project's summary |
| `y` / `s` on an INFRASTRUCTURE row | copy how to reach the machine or cluster / open a shell on it in a new tmux window |
| `space c` | check cloud resources with Azure (and open clusters with kubectl) now |
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
The summary also carries a bold headline for the card, and a "next up" step
when the agents' own messages make one clear.
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

The project brief has an **INFRASTRUCTURE** section, and the session detail a
shorter **CLOUD** list for that session alone, showing the cloud resources the
agents worked with: VMs, AKS clusters and node pools,
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

The detail column shows the VM size or node pools. For clusters with node
auto provisioning (managed Karpenter), the machines it made are found by
their `karpenter.azure.com_cluster` tag and counted by size, for example
`2× D4as_v5 + 2× NC40ads_H100_v5 spot`. Deallocated machines are not
counted. If the reply was cut off, the pool shows as `auto nodes`.
The detail column also shows the real resource group. The project brief groups rows by what they cost:

1. **Running, costing money:** running, stopped but still billed, or a delete
   that did not stick. These are always listed, however long ago an agent
   touched them.
2. **Stopped:** deallocated VMs and stopped clusters, which are cleanup
   candidates.
3. **Used recently:** everything else that still exists or was not checked.
4. **Gone:** deleted.

Within a group, the most recently used comes first.

Limits:

- Only kinds whose logged name is the real Azure name are checked: VMs, scale
  sets, AKS, AI accounts, registries, storage, key vaults, container apps, web
  apps, and resource groups. Model deployments, Kubernetes jobs, and SSH hosts
  are not, except that an SSH address is matched to the VM whose public IP
  it is.
- A kubectl cluster is matched to AKS by its context or kubeconfig name. If
  those differ, it is just not checked.
- "gone" can also mean the resource is in a subscription you cannot read.

`--live off` (or `SWITCHBOARD_LIVE=off`) turns it off; `--live on` fails if
`az` is missing. `switchboard cloud` and `summarize` use it too, and the
summarizer is told what Azure said.

### Cost

Each running row shows what it costs an hour, and the **Running, costing
money** header totals it per hour and per day. Prices come from the public
[Azure retail price list](https://learn.microsoft.com/rest/api/cost-management/retail-prices/azure-retail-prices)
(no login), are cached in `prices.json` for a week, and are fetched only for
sizes that are running. They are Linux pay-as-you-go list prices for compute
only: no reservations, savings plans, or enterprise discounts, and no disks,
IPs, or network. Spot machines use the spot list price, which moves. Treat the
figure as "about this much", not a bill. A `≥` or "at least" means a size had
no listed price. Cost Management would be exact but lags about a day.

A running resource that no agent has touched for 12 hours is flagged amber,
with a count above the list: it may be something left on.

### Expanding a row

Press `enter` on an INFRASTRUCTURE row to see more:

- **Where and size:** group, region, Kubernetes version, public address,
  subscription, and price.
- **Log in** (VMs): the exact commands an agent last used. When it reached the
  VM through `az network bastion tunnel` and then `ssh -p <port> ...@127.0.0.1`,
  the two are linked by the local port. If no agent logged in, it suggests
  `az ssh vm` and says so. `y` copies a one-liner (tunnel in the background,
  ssh, close the tunnel; just the ssh if the tunnel port is already open) and
  `s` runs it in a new tmux window.
- **Nodes** (clusters): node pools from Azure with count, size, autoscale
  range, and price, and under each pool its nodes from `kubectl get nodes`:
  ready or not, GPUs, uptime, and the job pods on it. Machines made by node
  auto provisioning sit under that pool. Pods waiting for a node are counted
  with the reason.
- **Workloads** (clusters): the jobs and deployments agents started or touched,
  with what kubectl says now (running, complete, failed and why, ready
  replicas, or gone), plus a count of other jobs in the same namespaces.
- **Shell on a node:** `kubectl debug node/<node> -it --image=...busybox`,
  which gives a root shell with the node's filesystem at `/host`. `y` copies
  it for the first GPU node; `s` opens a window that lists nodes and leaves
  the command typed, waiting for a node name.

kubectl runs only for cluster rows you expand, with the same `--kubeconfig`
or `--context` the agent used (else a context named after the AKS cluster),
again every minute while open, and on `space c`. It only reads. It is off
when `kubectl` is not installed or with `--live off`.

## Not yet

- Starting Claude Code from the panel (`i` already works with it).
