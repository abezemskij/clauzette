# Clauzette

A terminal AI agent for a local Ollama model, built to run as a pod in Kubernetes and work on a shared workspace. It reads, searches, writes and edits files and runs commands, asks before changing anything, keeps its context under control by compacting itself, and pauses when the GPU runs hot.

Pure Go standard library, no external dependencies. Linux only (it uses process groups to stop commands cleanly).

## How it fits together

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
cmd/clauzette/            entry point, flags, wiring, "idle" mode for the pod
internal/config/       JSON config, defaults, validation
internal/ollama/       /api/chat streaming, load, ps, version
internal/agent/        agent loop, context and compaction, system prompt, warm-up, audit log
internal/tools/        tool interface, registry, file tools (read, search, write, edit, mv, delete), exec, diff, path confinement
internal/session/      session model and store
internal/gpu/          GPU guard
internal/ui/           terminal front end
deploy/                Kubernetes manifests
```

## Build

```sh
make test        # go test ./...
make build       # bin/clauzette
make image IMAGE=registry.local/clauzette VERSION=0.1.0
make push  IMAGE=registry.local/clauzette VERSION=0.1.0
```

The Docker build runs `go vet` and the tests before compiling. The runtime image is Debian slim with a curated set of command-line tools, because `exec_command` can only use what is installed. Edit the `apt-get` line to match your projects.

## Deploy

The manifests reference two PersistentVolumeClaims you provide: `shared-workspace` (the shared space, mounted at `/workspace`) and `clauzette-state` (sessions and the audit log, mounted at `/state`). Ollama's models need a third one, `ollama-models`.

```sh
kubectl apply -f deploy/00-namespace.yaml
kubectl apply -f deploy/10-ollama.yaml        # skip if Ollama already runs; see the file
kubectl apply -f deploy/20-clauzette.yaml        # set your image name first
kubectl apply -f deploy/30-networkpolicy.yaml
```

Then, on the master:

```sh
tmux new -A -s agent
kubectl -n ai exec -it deploy/clauzette -- clauzette            # new session
kubectl -n ai exec -it deploy/clauzette -- clauzette -resume    # continue the last one
```

If your SSH connection drops, tmux keeps `kubectl exec` and the agent running. Reattach with `tmux attach -t agent`. If the pod is restarted, the session survives on disk: start again with `-resume`.

For development on your own machine, port-forward Ollama and point the binary at it:

```sh
kubectl -n ai port-forward svc/ollama 11434:11434
mkdir -p scratch && make run
```

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
- Run `/retry`. This removes the last exchange and resends it, so history changes near the end. If the wait now looks like processing the whole conversation, edits to history are expensive with this model, and `/undo`, `/retry`, `/system reload` and compaction each cost a full re-read.
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
| `!safe [on\|off]` | Safe mode: back up before `mv_file` / `delete_file`, confirm each one separately |
| `!plan [on\|off]` | Plan mode: capture write/exec actions instead of running them |
| `!skip` | Skip a GPU cool-down pause |
| `!!text` | Send a message that starts with `!` |

**Slash commands work while the agent is idle:** `/context`, `/compact`, `/undo`, `/retry`, `/sessions`, `/new`, `/load <id>`, `/system [reload]`, `/gpu`, `/approvals [reset]`, `/safe [restore <n>]`, `/plan [approve|reject …]`, `/help`, `/exit`.

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

**Safe mode.** `mv_file` and `delete_file` are in their own risk class: they always ask, and an `a` answer does not cover them. `!safe on` goes one step further — before a move or delete runs, clauzette copies the target to `<sessions_dir>/backups/<session id>/`, and each destructive action needs a fresh `y` even if the tool was approved before. `exec_command` is not backed up; its approval note says so.

Backups live with the sessions — in the pod that is the state volume — and are outside the workspace, so the model never sees or edits them. `safe.max_backups` (default 20) limits how many are kept per session.

```
/safe                safe mode state and this session's backups, newest first
/safe restore <n>    put backup n back where it came from
```

`write_file` and `edit_file` do not create backups: if one of them goes wrong, the diff shown at approval and the audit log say exactly what changed.

**Plan mode.** `!plan on` makes the agent *propose* instead of *act*. While it is on, read tools still run, but every write, exec and destructive action is prepared and held in a pending list rather than executed. This lets you let the model explore and lay out a full set of changes, then review them as a batch before anything happens — useful for small models that would otherwise edit as they think.

```
/plan                      pending actions, numbered, with their summaries
/plan approve [n ...|all]  run the selected actions in the order they were planned
/plan reject  [n ...|all] [reason]   discard them; the reason is sent to the model
```

Approving runs the actions exactly as a live call would — including safe-mode backups and the normal approval prompts — and then hands the results back to the model to continue. Rejecting hands the rejection (and your reason, if any) back the same way, so it can adjust. The pending list is per session in memory: `/new` or `/load` starts fresh. `!plan on` and `!plan off` each tell the model about the change in the conversation.

**Exiting.** `!q`, `/exit`, Ctrl+D, Ctrl+C or a dropped connection all cancel running work, save the session and print the command to resume it.

## How the important parts work

**Agent loop.** A turn appends your message, then repeats: GPU check, context check, model call, run the requested tools, append their results. It ends when the model replies without tool calls, or after `agent.max_steps` model calls. Cancelling leaves a valid history behind: a partial reply is kept and marked as interrupted, and every tool call that did not run gets a "cancelled" result, because a tool call without a result confuses most models.

**Tools.** Each tool validates its arguments and prepares an action before anything happens; errors go back to the model as the tool result so it can correct itself. All paths are resolved against the workspace root with symlinks followed, and anything outside is refused. `edit_file` uses exact search-and-replace rather than model-written patches, which small models get wrong. The diff you see is computed by clauzette. Because the space is shared, overwriting an existing file requires that the agent read it first, and every write checks that the file has not changed since it was read or since the diff was shown.

**Commands** run with `tools.shell -c` in their own process group, with no input, a timeout and an output cap (the middle of long output is dropped). Cancelling or timing out stops the whole process tree; anything left running in the background is stopped when the command ends.

**Context and compaction.** Before every model call, clauzette estimates the size of the request. Ollama has no tokenize endpoint, so this is a character-based estimate calibrated against the `prompt_eval_count` Ollama reports; once a request has run, the prompt line and `/context` show Ollama's actual `prompt_eval_count` instead of the estimate. clauzette also asks Ollama (`/api/show`) how much context the model itself supports and reports it at startup and in `/context`, so you can see whether `num_ctx` is capping it (e.g. "model supports up to 262k; num_ctx caps this session at 131k"). When the context reaches `compact_at_tokens`, or the reply might not fit in what is left of `num_ctx`, it compacts in one step. First it replaces old tool output and large tool-call arguments (file contents) with short placeholders, since the model can re-read files. If that does not get below `compact_target_tokens`, it asks the model (thinking off) to summarize the older part in a fixed structure: goal, decisions, files and their state, commands, open tasks, preferences. The last `keep_recent_tokens` always stay verbatim, and a tool call is never separated from its result.

**Prompt cache stability.** Ollama reuses its processed prompt when a request starts with the same text as the previous one. So the system prompt is assembled once per session and stored with it (the date is frozen at session start), tools are always sent in name order, and every request uses the same options. A different `num_ctx` in any request would also make Ollama reload the model.

**Thinking.** Reasoning from the current turn is sent back to the model, so it remembers why it called its tools. Reasoning from earlier turns is dropped, which saves a lot of context.

**Timeouts.** There is no overall request timeout. Instead each phase has its own limit: connecting (`connect_seconds`), the wait for the first data, which covers loading the model and processing a long prompt (`first_token_seconds`), and the silence allowed once data flows (`inter_token_seconds`). The last one is generous because Ollama sends nothing while the model writes a tool call, which for a large file can take minutes.

**Warm-up.** At startup clauzette checks that Ollama is reachable, loads the model with the session's options unless it is already loaded, and reports whether it fits in VRAM. Messages you type meanwhile are queued. `warmup_prefill` additionally pre-processes the system prompt; whether that helps depends on the cache test above.

**GPU guard.** A background loop reads the exporter every few seconds. Before each model call the agent checks the rules: with `mode: temperature` (the default) it pauses above `pause_above_c` and resumes below `resume_below_c`; with `utilization` it pauses for `util_pause_seconds` after `util_window_seconds` at or above `util_threshold`. A running generation is never interrupted.

**System prompt.** It is assembled from the built-in base prompt (`internal/agent/base_prompt.md`, or `prompt.base_file`), an optional `prompt.extra_file`, an environment block (date, workspace, tools, context size), and the first of `prompt.project_files` found in the workspace root, such as an `AGENTS.md` with project conventions. Base and extra files may use template fields: `{{.Date}}`, `{{.Workspace}}`, `{{.Model}}`, `{{.Shell}}`, `{{join .Tools ", "}}`, `{{.Internet}}`, `{{.NumCtx}}`. To add your own instructions in the cluster, add a key to the ConfigMap and point `prompt.extra_file` at it. `/system reload` rebuilds the prompt for the current session.

## Configuration

Everything has a default; a config file only needs what you change. `clauzette -print-config` shows the effective configuration, and unknown field names are rejected so typos surface. `config.example.json` lists every field. `CLAUZETTE_OLLAMA_URL`, `CLAUZETTE_MODEL` and `CLAUZETTE_WORKSPACE` override the file.

To change the context limit, set `num_ctx` in the config. It must stay at or below the context the model supports, which clauzette reports at startup and in `/context` (e.g. "model supports up to 262k"). Keep `context.compact_at_tokens` at roughly 75–80% of `num_ctx`. Note that a higher `num_ctx` uses more GPU memory for the KV cache and can slow generation, because the model and the cache compete for the GPU; a model that does not fit fully on the GPU will still work, just slower.

## Security model

The agent can only do what the pod allows. The manifests run it as a non-root user with a read-only root filesystem, no Linux capabilities, no Kubernetes API token, and network access limited to DNS, Ollama and the GPU exporter. File access is confined to the workspace, writes and commands need approval, and every tool call is appended to the audit log on the state volume (inside `kubectl exec`, the program's output goes to your terminal, not to the pod log).

Treat two choices deliberately. Opening internet access lets commands install packages, and also lets a prompt-injected command send data out. `auto_approve` for `exec_command` removes your last checkpoint.

## Not built yet

Web search and page fetching, an image tool for the model's vision capability, Markdown rendering in the terminal, and a network server mode. The event-based core is designed so these can be added without changing the agent loop.
