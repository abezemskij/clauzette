# Clauzette in detail

The full reference: architecture, building and running (in Kubernetes and locally), every command and mode, how the important parts work, configuration and security. For a short overview, see [README.md](README.md).

- [Architecture](#architecture)
- [Build](#build)
- [Running in Kubernetes](#running-in-kubernetes)
- [Running locally](#running-locally)
- [M0 checklist: verify the model and the hardware](#m0-checklist-verify-the-model-and-the-hardware)
- [Using it](#using-it)
- [Safe mode](#safe-mode)
- [Plan mode](#plan-mode)
- [Rogue mode](#rogue-mode)
- [How the important parts work](#how-the-important-parts-work)
- [Configuration](#configuration)
- [Security model](#security-model)
- [Not built yet](#not-built-yet)

## Architecture

```
ssh master → tmux → kubectl exec -it deploy/clauzette -- clauzette
                                   │
                    ┌──────────────┴──────────────┐
                    │ ui      terminal, ! and / commands, approvals
                    │ agent   session, agent loop, compaction, events
                    │ tools   read/list/search/write/edit/exec
                    │ ollama  streaming client, phased timeouts
                    │ gpu     exporter sampling, cool-down pauses
                    └──────────────┬──────────────┘
                     Service ollama:11434 (pod on the master, Titan RTX)
```

The `agent` package has no terminal code. It reports everything as events and receives approval decisions through a channel, so a network server or web front end could later drive the same core.

The model is stateless: every request sends the full conversation. Each session keeps two lists. The **context** is what the model sees, and it gets compacted. The **transcript** is the complete record and is never shortened. Both live in one JSON file per session, saved after every step.

```
cmd/clauzette/      entry point, flags, wiring, "idle" mode for the pod
internal/config/    JSON config, defaults, validation
internal/ollama/    /api/chat streaming, load, ps, version
internal/agent/     agent loop, context and compaction, system prompts, plan and rogue modes, backups, warm-up, audit log
internal/tools/     tool interface, registry, file tools (read, search, write, edit, mv, delete), exec, diff, path confinement
internal/session/   session model and store
internal/gpu/       GPU guard
internal/ui/        terminal front end
deploy/             Kubernetes manifests
```

## Build

Requires Go 1.25 or newer (the Docker build uses 1.26).

```sh
make test        # go test ./...
make build       # bin/clauzette
make image IMAGE=registry.local/clauzette VERSION=0.1.0
make push  IMAGE=registry.local/clauzette VERSION=0.1.0
```

The Docker build runs `go vet` and the tests before compiling. The runtime image is Debian slim with a curated set of command-line tools, because `exec_command` can only use what is installed. Edit the `apt-get` line to match your projects.

## Running in Kubernetes

The manifests reference two PersistentVolumeClaims you provide: `shared-workspace` (the shared space, mounted at `/workspace`) and `clauzette-state` (sessions and the audit log, mounted at `/state`). Ollama's models need a third one, `ollama-models`.

```sh
kubectl apply -f deploy/00-namespace.yaml
kubectl apply -f deploy/10-ollama.yaml        # skip if Ollama already runs; see the file
kubectl apply -f deploy/20-clauzette.yaml     # set your image name first
kubectl apply -f deploy/30-networkpolicy.yaml
```

Then, on the master:

```sh
tmux new -A -s agent
kubectl -n ai exec -it deploy/clauzette -- clauzette            # new session
kubectl -n ai exec -it deploy/clauzette -- clauzette -resume    # continue the last one
```

If your SSH connection drops, tmux keeps `kubectl exec` and the agent running. Reattach with `tmux attach -t agent`. If the pod is restarted, the session survives on disk: start again with `-resume`.

## Running locally

clauzette runs just as well on your own Linux machine, against an Ollama on the same machine or one port-forwarded from the cluster. Nothing about the agent changes; only where the workspace, the sessions and the model are.

**1. Ollama.** Install your GPU drivers and [Ollama](https://ollama.com). On Linux, the installer sets Ollama up as a service listening on `127.0.0.1:11434`; if it is not running, enable it with `sudo systemctl enable --now ollama`. Then pull a model that supports tool calling; the examples use Qwen3.8, but any such model works:

```sh
ollama pull qwen3.8:27b
curl http://127.0.0.1:11434/             # "Ollama is running"
curl http://127.0.0.1:11434/v1/models    # the models you have pulled, as JSON
```

**2. Build.** clauzette needs Go 1.25 or newer. A distribution's `golang-go` package may be older, so check with `go version`, and install a current release from [go.dev/dl](https://go.dev/dl) if needed.

```sh
git clone https://github.com/abezemskij/clauzette
cd clauzette
make build                       # the binary is bin/clauzette
```

**3. Configure.** Create `clauzette.local.json` in the repository root. Git ignores it, so each machine keeps its own:

```json
{
  "ollama_url": "http://127.0.0.1:11434",
  "model": "qwen3.8:27b",
  "num_ctx": 163840,
  "workspace": "./scratch",
  "sessions_dir": "./.local/sessions",
  "audit_log": "./.local/audit.jsonl",
  "tools": { "shell": "/bin/bash" },
  "gpu": { "enabled": false }
}
```

Keep `num_ctx` within what the model supports and your GPU memory allows; clauzette reports both at startup (see [Context size](#configuration)).

**4. Run.** Create the workspace once (clauzette does not start without it), then run the binary:

```sh
mkdir -p scratch
./bin/clauzette -config clauzette.local.json            # new session
./bin/clauzette -config clauzette.local.json -resume    # continue the last one
```

Everything the agent writes (code, drafts and so on) ends up in `./scratch`; sessions, safe-mode backups and the audit log go to `./.local`, created on first start. All of these stay out of git. `make run-local` (with `ARGS=-resume`) does the build, the directories and the run in one step.

**Against the cluster's Ollama.** Port-forward it instead of running Ollama locally; `ollama_url` stays `http://127.0.0.1:11434`, and `model` must name a model the cluster's Ollama has:

```sh
kubectl -n ai port-forward svc/ollama 11434:11434
```

Things that differ from the pod:

- **No isolation.** Locally, `exec_command` runs as your user, with your files, credentials and network access (SSH keys, a kubeconfig, cloud credentials). The workspace limit binds the file tools only, not the shell. Keep approvals on, and be especially careful with rogue mode.
- **The GPU guard** needs a metrics endpoint; leave it disabled unless you run an exporter.
- **One directory per agent** works locally too (`"agent_dir": true`); the agent ID defaults to your hostname.

## M0 checklist: verify the model and the hardware

These measurements decide the real values for `num_ctx` and the compaction thresholds. Do them once after deploying Ollama.

**1. The model fits on the GPU at 128k context.** Start clauzette (it loads the model with `num_ctx` from the config) and watch the warm-up messages: it reports whether the model runs fully on the GPU. Or check directly:

```sh
kubectl -n ai exec deploy/ollama -- ollama ps
```

The PROCESSOR column must say `100% GPU`. If it shows a CPU share, lower `num_ctx` (96k or 64k) and lower `context.compact_at_tokens` with it, to roughly 75–80% of `num_ctx`.

**2. Generation and prompt speed.** After a few exchanges, each reply shows its tokens per second. For prompt processing at size, `/context` plus a long conversation will show it; or time a request with a long pasted file.

**3. Cache behaviour.** Qwen3.8 is a hybrid model: three of every four layers keep a fixed-size recurrent state instead of a per-token cache. Such a state cannot be rewound, so how Ollama reuses its prompt cache when earlier history changes matters a lot. Test it:

- Send a few long messages. Note the time to the first token of the next reply (the waiting indicator shows it). It should be short: only the new message is processed.
- Run `/retry`. This removes the last exchange and resends it, so history changes near the end. If the wait now looks like processing the whole conversation, edits to history are expensive with this model, and `/undo`, `/retry`, `/system-prompt reload`, switching rogue mode on or off, and compaction each cost a full re-read.
- Toggle `!think off` and send a message. If that also triggers a full re-read, the chat template places the thinking switch inside the system prompt.

**4. Does `prompt_eval_count` count cached tokens?** `/context` shows the last `prompt_eval_count` next to the estimate. If it is far below the estimate on normal turns, Ollama reports only the uncached part. clauzette detects that and ignores such samples for its token calibration.

**5. GPU metrics.** Check that the exporter answers and which metric names it uses:

```sh
kubectl -n ai run curl --rm -it --image=curlimages/curl --restart=Never -- \
  curl -s http://nvidia-dcgm-exporter.gpu-operator.svc.cluster.local:9400/metrics | grep -i -E 'temp|util'
```

NVIDIA's DCGM exporter targets data-center GPUs and may report only part of its metrics for a Titan RTX. If temperature is missing, use an exporter built on `nvidia-smi` and set `gpu.temp_metric` and `gpu.util_metric` to its names (and `gpu.util_scale` to 100 if it reports utilization as 0–1). Then `/gpu` in clauzette shows the current reading.

## Using it

Type a message and press Enter. While the agent works, its reply streams in, tool calls appear as `→ tool args`, and their results as `✓` or `✗` lines.

**Control commands work at any time**, including while the agent is generating, running a command or paused:

| Command | Effect |
|---|---|
| `!c` | Cancel the current operation and discard queued messages |
| `!q` | Save and exit |
| `!think [on\|off]` | Toggle model thinking; applies from the next model request |
| `!show [on\|off]` | Toggle displaying the thinking text |
| `!safe [on\|off]` | [Safe mode](#safe-mode): back up before `mv_file` / `delete_file`, confirm each one separately |
| `!plan [on\|off]` | [Plan mode](#plan-mode): capture write/exec actions instead of running them |
| `!skip` | Skip a GPU cool-down pause |
| `!!text` | Send a message that starts with `!` |

**Slash commands work while the agent is idle** (except `/rogue off`, which also works mid-task):

| Command | Effect |
|---|---|
| `/context` | Context usage, compaction settings, and what compacting now would keep |
| `/compact [percent]` | Compact now; with a percent (10–90), summarize that share of the oldest conversation once |
| `/undo`, `/retry` | Drop the last exchange from the context (and resend it) |
| `/sessions`, `/new`, `/load <id>` | List, start or switch sessions |
| `/system-prompt [reload]` | Show the system prompt in use, or rebuild both (normal and rogue mode) |
| `/gpu` | GPU guard status |
| `/approvals [reset]` | Tools that run without asking; forget this session's "always" answers |
| `/safe [restore <n> [force]]` | Safe mode status and backups; restore one |
| `/plan [approve\|reject …]` | Pending actions from plan mode; run or discard them |
| `/rogue [on [steps]\|off]` | [Rogue mode](#rogue-mode): work unattended until the model finishes |
| `/help`, `/exit` | Help; save and exit |

**Multi-line input:** a line containing only `"""` starts a block, and another one ends it.

**Typing while the agent works** queues the message; it is sent when the current turn finishes.

**Approvals.** Reading, listing and searching run automatically. Writing, editing and running commands ask first, showing a diff or the command:

```
Approval needed (write): edit cmd/server/main.go (1 replacement, +3 -1 lines)
--- a/cmd/server/main.go
+++ b/cmd/server/main.go
@@ -40,7 +40,9 @@
...
Approve? y / n [reason] / a (always edit_file) / v (view all):
```

`n` with a reason passes the reason to the model. `a` approves that tool for the rest of the session. `tools.auto_approve` in the config makes tools run without asking permanently.

**Exiting.** `!q`, `/exit`, Ctrl+D, Ctrl+C or a dropped connection all cancel running work, save the session and print the command to resume it.

## Safe mode

`mv_file` and `delete_file` are in their own risk class: they always ask, and an `a` answer does not cover them. `!safe on` goes one step further: before a move or delete runs, clauzette copies the target to `<sessions_dir>/backups/<session id>/`, and each destructive action needs a fresh `y` even if the tool was approved before. `exec_command` is not backed up; its approval note says so. `write_file` and `edit_file` do not create backups either: if one of them goes wrong, the diff shown at approval and the audit log say exactly what changed.

Backups live with the sessions (in the pod, that is the state volume) and are outside the workspace, so the model's file tools never see or edit them. `safe.max_backups` (default 20) limits how many are kept per session.

Backups never follow symlinks: a link inside a backed-up directory is saved and restored as a link, so a venv's `lib64 -> lib` works, and a link pointing outside the workspace never pulls outside content into the backup. Sockets and device files are skipped. A restore puts each entry back where its record says, relative to the agent's directory or the shared workspace; a stored absolute path is ignored, and restoring into the shared workspace needs `shared_access: write`.

```
/safe                       safe mode state and this session's backups, newest first
/safe restore <n>           put backup n back where it came from
/safe restore <n> force     the same, replacing what exists now (after backing it up)
```

`/safe restore <n>` refuses when any path it would write exists now, lists those paths and changes nothing; `/safe` marks such backups. `/safe restore <n> force` first saves whatever it is about to replace as a new backup, then replaces those entries with the backup's versions; files that are not in the backup are kept. Because of that new backup, `/safe restore 1 force` undoes a forced restore.

## Plan mode

`!plan on` makes the agent *propose* instead of *act*. While it is on, read tools still run, but every write, exec and destructive action is prepared and held in a pending list rather than executed. This lets the model explore and lay out a full set of changes, which you then review as a batch before anything happens: useful for small models that would otherwise edit as they think.

```
/plan                      pending actions, numbered, with their summaries
/plan approve [n ...|all] [--no-continue] [comment]
                           run the selected actions in the order they were planned
/plan reject  [n ...|all] [reason]   discard them; the reason is sent to the model
```

Approving runs the actions exactly as a live call would, including safe-mode backups and the normal approval prompts, and then hands the results back to the model to continue. Each action is prepared again right before it runs, so several edits to one file apply in order, and the approval prompt shows the diff against the file as it is now. If an action can no longer be prepared (its text is gone, say), the model is told and the run goes on. Denying an action stops the run: the model hears about the denial, and the actions after it stay pending. Cancelling with `!c` stops it too, and the action that did not run stays pending.

Anything after the selection is a comment for the model, delivered with the results, and it takes priority over continuing the plan:

```
/plan approve 1 but then stop and let's review the changes
/plan approve all looks good, continue
/plan approve 1 -- 2 files only, nothing else     (-- when the comment starts with a number)
/plan approve 2 --no-continue                     run it, but don't start a model turn
```

A comment needs a selection: `/plan approve looks good` is refused rather than guessed to mean all. With `--no-continue` the results are held back and reach the model together with your next message. Rejecting hands the rejection (and your reason, if any) back to the model, so it can adjust.

The pending list is saved with the session, so it survives a restart or `/load`. `!plan on` and `!plan off` take effect at once and are announced to the model at its next safe point (between steps, or with your next message); toggling back before then announces nothing.

## Rogue mode

`/rogue on` lets the agent work unattended: every action runs without asking, and each message you send is worked on until the model calls the `finish` tool (offered only in this mode) with a summary for you. Each tool result tells the model how many steps it has left. A reply without tool calls gets one automatic nudge (nobody is there to answer questions), and a second one ends the task.

```
/rogue on          budget per message: agent.max_steps (default 100)
/rogue on 40       budget per message: 40 steps
/rogue off         approvals are back from the next action; works mid-task
/rogue             status
```

Rogue mode has its own system prompt (`internal/agent/rogue_prompt.md`, or `prompt.rogue_base_file`): it drops "ask before acting" and "the operator approves", asks the model to think before its first action and before finishing, to make and report assumptions instead of asking, and to prefer reversible actions. It is meant for thinking models; `/rogue on` mentions it when thinking is off.

A step is one model reply. `rogue.max_minutes` (default 60, 0 = no limit) is a hard limit: when it passes, whatever is running is stopped, including a reply the model is still thinking about or a command. When it passes or the step budget is used up, the model gets one last step to summarize what is done and what is left, with thinking off and at most 5 minutes; tool calls in that reply do not run.

`mv_file` and `delete_file` always back up their target first, as in safe mode, so `/safe restore` can undo them. Every action is in the audit log with the decision `rogue`, and the prompt line shows `ROGUE` while it is on. `!c` stops the current task. Rogue mode is never saved with the session (a resumed session starts with it off), and it cannot be combined with plan mode: `!plan on` switches it off.

Running unattended removes the last check between the model and `exec_command`. Instructions hidden in files or command output (prompt injection) are then acted on directly, and the shell is only confined by what the pod allows: use it in an isolated agent directory, with no secrets in reach.

## How the important parts work

**Agent loop.** A turn appends your message, then repeats: GPU check, context check, model call, run the requested tools, append their results. It ends when the model replies without tool calls, or after `agent.max_steps` model calls. Cancelling leaves a valid history behind: a partial reply is kept and marked as interrupted, and every tool call that did not run gets a "cancelled" result, because a tool call without a result confuses most models.

**Tools.** Each tool validates its arguments and prepares an action before anything happens; errors go back to the model as the tool result so it can correct itself. Relative paths start in the workspace root (or the agent's own directory, see [Configuration](#configuration)). Every file operation goes through an `os.Root` handle, which resolves each path component at the moment of the operation: a symlink is followed only while it stays inside, so a directory swapped for a link to somewhere else after you approved an action cannot lead it outside. Symlinks with absolute targets are not followed. Deleting or moving a symlink acts on the link itself, never on its target; reading or writing through a relative link uses the target. `edit_file` uses exact search-and-replace rather than model-written patches, which small models get wrong. The diff you see is computed by clauzette. Because the space is shared, overwriting an existing file requires that the agent read it first, and every write checks that the file has not changed since it was read or since the diff was shown.

**Commands** run with `tools.shell -c` in their own process group, with no input, a timeout and an output cap (the middle of long output is dropped). Cancelling or timing out stops the whole process tree; anything left running in the background is stopped when the command ends.

**Context and compaction.** Before every model call, clauzette estimates the size of the request. Ollama has no tokenize endpoint, so this is a character-based estimate calibrated against the `prompt_eval_count` Ollama reports; once a request has run, the prompt line and `/context` show Ollama's actual `prompt_eval_count` instead of the estimate. clauzette also asks Ollama (`/api/show`) how much context the model itself supports and reports it at startup and in `/context`, so you can see whether `num_ctx` is capping it (e.g. "model supports up to 262k; num_ctx caps this session at 131k"). When the context reaches `compact_at_tokens`, or the reply might not fit in what is left of `num_ctx`, it compacts in one step. First it replaces old tool output and large tool-call arguments (file contents) with short placeholders, since the model can re-read files. If that does not get below `compact_target_tokens`, it asks the model (thinking off) to summarize the older part in a fixed structure: goal, decisions, files and their state, commands, open tasks, preferences. The last `keep_recent_tokens` always stay verbatim, and a tool call is never separated from its result.

To compact in proportion instead, set `context.keep_recent_ratio` (0.1–0.9; 0 = off): `0.5` keeps the newest half of the conversation verbatim, whatever its size, and replaces `keep_recent_tokens` and `compact_target_tokens`. The older part loses its old tool output first and is summarized only if that does not at least halve it. Keeping more verbatim keeps exact recent detail, but compaction comes round sooner, and with a hybrid model each one costs a full re-read; repeated summaries also fold earlier summaries in, so the oldest detail fades over time. `/compact 75` summarizes the oldest 75% once, whatever the setting, and `/context` shows what compacting now would keep and summarize.

**Prompt cache stability.** Ollama reuses its processed prompt when a request starts with the same text as the previous one. So the system prompt is assembled once per session and stored with it (the date is frozen at session start), tools are always sent in name order, and every request uses the same options. A different `num_ctx` in any request would also make Ollama reload the model.

**Thinking.** Reasoning from the current turn is sent back to the model, so it remembers why it called its tools. Reasoning from earlier turns is dropped, which saves a lot of context.

**Timeouts.** There is no overall request timeout. Instead each phase has its own limit: connecting (`connect_seconds`), the wait for the first data, which covers loading the model and processing a long prompt (`first_token_seconds`), and the silence allowed once data flows (`inter_token_seconds`). The last one is generous because Ollama sends nothing while the model writes a tool call, which for a large file can take minutes. In rogue mode, `rogue.max_minutes` also applies, as a hard limit.

**Warm-up.** At startup clauzette checks that Ollama is reachable, loads the model with the session's options unless it is already loaded, and reports whether it fits in VRAM. Messages you type meanwhile are queued. `warmup_prefill` additionally pre-processes the system prompt; whether that helps depends on the cache test in the [M0 checklist](#m0-checklist-verify-the-model-and-the-hardware).

**GPU guard.** A background loop reads the exporter every few seconds. Before each model call the agent checks the rules: with `mode: temperature` (the default) it pauses above `pause_above_c` and resumes below `resume_below_c`; with `utilization` it pauses for `util_pause_seconds` after `util_window_seconds` at or above `util_threshold`. A running generation is never interrupted.

**System prompt.** It is assembled from the built-in base prompt (`internal/agent/base_prompt.md`, or `prompt.base_file`; in rogue mode `internal/agent/rogue_prompt.md`, or `prompt.rogue_base_file`), an optional `prompt.extra_file`, an environment block (date, workspace, tools, context size), and the first of `prompt.project_files` found in the workspace root, such as an `AGENTS.md` with project conventions. With an agent directory, the first one found in the shared workspace root comes first (common rules), then the first one found in the agent's own directory. Base and extra files may use template fields: `{{.Date}}`, `{{.Workspace}}`, `{{.Model}}`, `{{.Shell}}`, `{{join .Tools ", "}}`, `{{.Internet}}`, `{{.NumCtx}}`. Template comments (`{{/* ... */}}`) are removed before sending. To add your own instructions in the cluster, add a key to the ConfigMap and point `prompt.extra_file` at it. Both prompts are built when a session starts and frozen with it; `/system-prompt` shows the one in use, and `/system-prompt reload` rebuilds both.

## Configuration

Everything has a default; a config file only needs what you change. `clauzette -print-config` shows the effective configuration, and unknown field names are rejected so typos surface. `config.example.json` lists every field. `CLAUZETTE_OLLAMA_URL`, `CLAUZETTE_MODEL`, `CLAUZETTE_WORKSPACE`, `CLAUZETTE_AGENT_ID`, `CLAUZETTE_AGENT_DIR` and `CLAUZETTE_SHARED_ACCESS` override the file. The config file is taken from `-config`, then `$CLAUZETTE_CONFIG`, then `./clauzette.json`, then `/etc/clauzette/config.json`.

**Context size.** To change the context limit, set `num_ctx`. It must stay at or below the context the model supports, which clauzette reports at startup and in `/context` (e.g. "model supports up to 262k"). Keep `context.compact_at_tokens` at roughly 75–80% of `num_ctx`. A higher `num_ctx` uses more GPU memory for the KV cache and can slow generation, because the model and the cache compete for the GPU; a model that does not fit fully on the GPU will still work, just slower.

**One directory per agent.** Several agents can share one workspace, each in its own directory. With `"agent_dir": true` the agent works in `<workspace>/<agent_id>`, created on first start; relative paths and `exec_command` start there. `agent_id` defaults to `$CLAUZETTE_AGENT_ID`, then the hostname, which in Kubernetes is the pod name; it must be letters, digits, `.`, `_` and `-`. `shared_access` says what the file tools may do in the rest of the shared workspace, such as `../other-agent/notes.md`: `none`, `read` (the default: read, list and search) or `write`. Sessions and backups move to `<sessions_dir>/<agent_id>/`; sessions saved earlier in `<sessions_dir>` still load with `-session <id>`. Audit records get an `agent` field.

Two limits. `shared_access` binds the file tools only: `exec_command` runs a normal shell, which sees whatever the pod mounts, so real isolation needs the mount itself (one pod per agent, the shared volume mounted with `subPathExpr`). And a Deployment's pod name changes on every restart, so set `CLAUZETTE_AGENT_ID` to a fixed name there, or use a StatefulSet, whose pod names are stable.

## Security model

The agent can only do what the pod allows. The manifests run it as a non-root user with a read-only root filesystem, no Linux capabilities, no Kubernetes API token, and network access limited to DNS, Ollama and the GPU exporter. File access is confined to the workspace, writes and commands need approval, and every tool call is appended to the audit log on the state volume (inside `kubectl exec`, the program's output goes to your terminal, not to the pod log).

Treat three choices deliberately. Opening internet access lets commands install packages, and also lets a prompt-injected command send data out. `auto_approve` for `exec_command` removes your last checkpoint, and so does rogue mode. Running locally drops the pod's limits altogether (see [Running locally](#running-locally)).

## Not built yet

Web search and page fetching, an image tool for the model's vision capability, Markdown rendering in the terminal, and a network server mode. The event-based core is designed so these can be added without changing the agent loop.
