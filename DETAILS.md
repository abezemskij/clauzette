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
                    │ ui      terminal: input, rendering, Markdown
                    │ control ! and / commands, approvals, queue, consent
                    │ agent   session, agent loop, compaction, events
                    │ tools   read/list/search/write/edit/exec, patch, git, web, fetch, todo, images
                    │ ollama  streaming client, phased timeouts
                    │ gpu     exporter sampling, cool-down pauses
                    └──────────────┬──────────────┘
                     Service ollama:11434 (Ollama pod on the GPU node)
```

The `agent` package has no terminal code, and neither does `control`, which implements every command once for any front end: the terminal (`ui`) only reads input and draws what `control` reports through a small `View` interface. It reports everything as events and receives approval decisions through a channel, so a network server or web front end could later drive the same core.

The model is stateless: every request sends the full conversation. Each session keeps two lists. The **context** is what the model sees, and it gets compacted. The **transcript** is the complete record and is never shortened. Both live in one JSON file per session, saved after every step.

```
cmd/clauzette/      entry point, flags, wiring, "idle" mode for the pod
internal/config/    JSON config, defaults, validation
internal/ollama/    /api/chat streaming, load, ps, version
internal/agent/     agent loop, context and compaction, system prompts, plan and rogue modes, backups, warm-up, audit log
internal/tools/     tool interface, registry, file tools (read, search, write, edit, mv, delete), exec, apply_patch, read-only git (status/diff/log), web_search, fetch_url, ip_country, todo, view_image and the image store, diff, path confinement
internal/session/   session model and store
internal/gpu/       GPU guard
internal/control/   commands, approvals, queue, cloud consent, model switches (front-end neutral)
internal/server/    clauzette serve: the controller behind an HTTP API
internal/wire/      the protocol between serve and attach, the token
internal/ui/        terminal front end: input and paste handling, rendering, Markdown, prompt
deploy/             Kubernetes manifests
```

## Build

Requires Go 1.25 or newer (the Docker build uses 1.27).

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

The PROCESSOR column must say `100% GPU`. If it shows a CPU share, lower `num_ctx` (96k or 64k); the compaction thresholds follow it, as shares of the window.

**2. Generation and prompt speed.** After a few exchanges, each reply shows its tokens per second. For prompt processing at size, `/context` plus a long conversation will show it; or time a request with a long pasted file.

**3. Cache behaviour.** Qwen3.8 is a hybrid model: three of every four layers keep a fixed-size recurrent state instead of a per-token cache. Such a state cannot be rewound, so how Ollama reuses its prompt cache when earlier history changes matters a lot. Test it:

- Send a few long messages. Note the time to the first token of the next reply (the waiting indicator shows it). It should be short: only the new message is processed.
- Run `/retry`. This removes the last exchange and resends it, so history changes near the end. If the wait now looks like processing the whole conversation, edits to history are expensive with this model, and `/undo`, `/retry`, `/system-prompt reload`, switching rogue mode on or off, `/model`, and compaction each cost a full re-read.
- Toggle `!think off` and send a message. If that also triggers a full re-read, the chat template places the thinking switch inside the system prompt.

**4. Does `prompt_eval_count` count cached tokens?** `/context` shows the last `prompt_eval_count` next to the estimate. If it is far below the estimate on normal turns, Ollama reports only the uncached part. clauzette detects that and ignores such samples for its token calibration.

**5. GPU metrics.** Check that the exporter answers and which metric names it uses:

```sh
kubectl -n ai run curl --rm -it --image=curlimages/curl --restart=Never -- \
  curl -s http://nvidia-dcgm-exporter.gpu-operator.svc.cluster.local:9400/metrics | grep -i -E 'temp|util'
```

NVIDIA's DCGM exporter targets data-center GPUs and may report only part of its metrics for a consumer GPU. If temperature is missing, use an exporter built on `nvidia-smi` and set `gpu.temp_metric` and `gpu.util_metric` to its names (and `gpu.util_scale` to 100 if it reports utilization as 0–1). Then `/gpu` in clauzette shows the current reading.

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
| `/undo`, `/retry` | Drop the last exchange from the context (and resend it, images included) |
| `/image <path> [question]` | Send an image with a question; without a question the model describes it. See [Images](#images) |
| `/sessions`, `/new`, `/load <id>` | List, start or switch sessions |
| `/clear` | Delete this session's conversation and start a new one, so a throwaway question does not linger in `/sessions`; asks first, and keeps files you worked on, safe-mode backups and the audit log |
| `/model [name]` | List the profiles and installed models, or switch to a profile or model, local or cloud (see [Model profiles](#configuration)); `--online [filter] [--all]` lists OpenRouter's models |
| `/adapt [name [--verify]]` | List adapted models, or measure the largest context that keeps a model fully on the GPU; `set <tokens>` and `forget` pin or drop a value |
| `/system-prompt [reload]` | Show the system prompt in use, or rebuild all three (normal, rogue mode, chat-only) |
| `/gpu` | GPU guard status |
| `/markdown [on\|off]` | Render the model's Markdown, or show it as written |
| `/approvals [reset]` | Tools that run without asking; forget this session's "always" answers |
| `/safe [restore <n> [force]]` | Safe mode status and backups; restore one |
| `/fetch [on\|off\|allow <host>\|forget <host>]` | `fetch_url` host allowlist: show it, gate it on/off, or edit it |
| `/plan [approve\|reject …]` | Pending actions from plan mode; run or discard them |
| `/rogue [on [steps]\|off]` | [Rogue mode](#rogue-mode): work unattended until the model finishes |
| `/help`, `/exit` | Help; save and exit |

**Multi-line input:** a line containing only `"""` starts a block, and another one ends it. Pasting several lines at once is also taken as a single message and sent on the next Enter, rather than split into one message per line (via bracketed paste, which clauzette turns on for the session on a terminal; any terminal without it falls back to line-by-line). Text you type before or after a paste belongs to the same message, so `download this image <pasted URL>` is sent as one message. A message that begins with pasted text is never run as a command, but a typed command can take a pasted argument (`/image <pasted path>`).

**Typing while the agent works** queues the message; it is sent when the current turn finishes. A y/N question (sending to a cloud endpoint, `/clear`) takes your next line as the answer; a `/` command typed instead counts as "no" and then runs.

**Replies are rendered as Markdown** as they stream: headings in bold, `**bold**`, `*italic*`, `` `code` `` and code blocks in colour, `-`/`*`/`+` bullets as `•`, `>` quotes with a bar, `---` as a line, and `[text](url)` as the underlined text followed by the URL. Code block fences stay visible (dimmed) and code lines are not indented, so code copied from the terminal is clean. Underscores, tables, numbered lists and HTML are shown as written: `_` appears in too many identifiers, and aligning a table would need the whole table before showing any of it. A `*` counts as emphasis only next to a word, so `2 * 3` and `rm *.tmp` stay as they are. Rendering needs colour; `"markdown": false` in the config or `/markdown off` shows replies as written. Either way, control characters in the model's output are removed, so a reply cannot send escape sequences to your terminal.

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

## Server mode: serve and attach

`clauzette serve` runs the session in a server instead of in your terminal; `clauzette attach` connects a terminal to it. The work keeps running when you detach or the connection drops, and several clients can watch and type at once (they see the same output).

```sh
clauzette serve -config c.json            # same flags as clauzette: -resume, -session, -profile
clauzette attach -config c.json           # in another terminal; /exit, !q or Ctrl+D detach
```

`attach` looks and behaves like the terminal: the same commands, approvals, Markdown and paste handling. It shows the last 200 lines of output from before it attached, and after a dropped connection it reconnects and continues where it stopped. `/exit`, `!q` and Ctrl+D detach that client only; the session ends when the server stops (Ctrl+C or SIGTERM), which saves it. `/help` and `/markdown` are handled by each client itself.

**Access.** The server listens on `server.listen`, by default `127.0.0.1:8484`: this machine (or pod) only. Every request needs a token, from `$CLAUZETTE_SERVER_TOKEN`; without it, `serve` creates a random one in `server.token_file` (default `<sessions>/server.token`, mode 0600), where `attach` on the same machine finds it.

Prefer the environment variable. The token is what answers approvals, and commands the agent runs (`exec_command`) run as the same user and can reach the server: with the token they could answer the agent's own approval requests. They never inherit `$CLAUZETTE_SERVER_TOKEN` (like the API keys), the process is non-dumpable so they cannot read it from `/proc`, and the token is redacted from tool output; a token file, though, is readable to them, and `serve` warns when it uses one. From elsewhere, forward the port (`kubectl port-forward`, an SSH tunnel) and pass the token in `$CLAUZETTE_SERVER_TOKEN`; `attach -server http://host:port` (or `$CLAUZETTE_SERVER`) names the server. A listen address other than loopback works but carries the token over plain HTTP, and `serve` warns about it; put TLS in front first. Every refused request and every input (its size, not its text) is recorded in the audit log.

**One owner per session.** A session is open in at most one clauzette at a time: the terminal and the server take a lock (`<session>.lock` next to it, with the process id and host). Opening a session that is open elsewhere is refused with who holds it; a lock left by a process that ended (a crash, `kill -9`) is taken over automatically. Sessions are the same files in both modes, so a session started in the terminal can continue in the server and back.

**Running it.** Locally, run `serve` in tmux or with `nohup`, so it outlives your shell. In the pod, `serve` is the container command (`kubectl exec` would end it on disconnect):

```yaml
command: ["clauzette", "serve", "-resume", "-take-over"]
env:
  - name: CLAUZETTE_SERVER_TOKEN
    valueFrom:
      secretKeyRef: {name: clauzette-server, key: token}
readinessProbe:
  exec: {command: ["/bin/bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/8484"]}
```

```sh
openssl rand -hex 32 | tr -d '\n' | kubectl -n ai create secret generic clauzette-server --from-file=token=/dev/stdin
```

`-resume` continues the latest session, or starts one on an empty state volume. `-take-over` lets a new pod take the session lock the previous pod left behind (a new pod has a new host name, so its lock cannot be checked otherwise; use it only where one process at a time uses the sessions directory). Then `kubectl -n ai exec -it deploy/clauzette -- clauzette attach` connects a terminal (the token is in its environment), and `kubectl -n ai port-forward deploy/clauzette 8484:8484` reaches the web page. Plain `clauzette -resume` in the pod is refused while the server holds the session; `clauzette` alone still starts a separate new session.

**The web page.** `serve` also serves a web page at its address (`http://127.0.0.1:8484/`; `"server": {"web": false}` turns it off). It asks for the token once, keeps it for that browser tab (or on the device, if you tick "remember"), and then shows the same session as `attach`: replies with Markdown and tables (with copy buttons on code and on each reply), thinking (collapsed), tool calls with their full results a click away, approval cards with the diff and Approve / Always / Deny (with an optional reason) buttons, Yes/No buttons for y/N questions, and a Stop button (`!c`). From a phone or another computer, reach it the same way as the API: port forwarding or a tunnel, or TLS in front.

Around the conversation:

- **Header:** the session menu (switch with a click, New, Clear), the model menu (profiles and installed models with what each supports, which is loaded, cloud ones marked; a switch waits for the current work, and a cloud model asks first), notifications, and the side panel.
- **Toolbar:** toggles for think, show thinking, safe, plan, the fetch allowlist and rogue (which asks first), each showing its state; the context meter with the compaction mark and a Compact button; Retry, Undo, Context, Help and Upload.
- **Side panel:** the agent's task plan (`todo_write`), plan mode's pending actions with Approve / Reject for each or all, and a read-only file browser for the workspace with a viewer (text with line numbers, images) and the git diff of a file or of the whole workspace.
- **Input:** exactly what you would type in the terminal. Enter sends, Shift+Enter starts a new line, a message of several lines is sent as text (never as a command), ↑/↓ walk through what you sent, and `/` or `!` suggests commands (Tab completes).
- **Notifications:** when the tab is in the background, its title shows when an approval or a y/N question waits or the work is done; with the bell on, the browser shows a notification too.
- **Upload:** the Upload button, dropping files on the page, or pasting them stores them in the workspace under `uploads/` (named `<date>-<time>-<name>`, characters other than letters, digits, `.`, `-` and `_` replaced; nothing is overwritten, unpacked or run; at most `server.upload_max_bytes`, 25 MB). The upload stays out of the conversation: the page offers to mention its path in your message, to ask about an image (`/image`), or to view it, and the agent opens it with its usual tools (`read_file`, `view_image`) when you ask it to. Every upload is recorded in the audit log with its size and hash.

Every action on the page is either a command you could type, or one of the read-only data requests below; the file browser reads through the same workspace confinement as the agent's tools.

The page treats everything the model and the tools produce as untrusted text: it builds the page from text only, never from HTML in a reply, and links in replies work only for `http(s)` addresses. A strict content security policy (only the page's own script and style, no inline script, no other sites, no framing) backs this up. The page itself needs no token to load and contains none; every API call carries it.

The protocol (package `wire`): `GET /api/state`, `POST /api/input` with `{"text": "...", "literal": false}` (exactly what you would type, so approvals and y/N answers need nothing extra) and `GET /api/events?since=N` (server-sent events, resumable with `Last-Event-ID`). For the page: `GET /api/sessions`, `/api/models`, `/api/todos`, `/api/plan`, `/api/files?path=`, `/api/file?path=` (text or an image, at most 2 MB), `/api/diff?path=|stat=1`, and `POST /api/upload?name=`.

## Images

A model with vision (`/model` lists it under what each model supports) can look at PNG, JPEG, WebP and GIF images. It reads them; it cannot draw them, though it can write SVG, HTML or plotting code that renders one.

- **`/image <path> [question]`** sends an image with your question, or asks for a description without one. A relative path is taken from the workspace root; quote a path with spaces. You may name a file outside the workspace: you typed it.
- **`view_image`** lets the model look at an image in the workspace on its own: a screenshot, a chart it rendered with a script, a photo you dropped in. Like `read_file` it is confined to the workspace and runs without asking. Its result is a line of text; the image itself follows as a separate message, because OpenAI-style APIs accept images only in user messages.

The file type is checked from the file's content, not its name, and an image may be at most `tools.image_max_bytes` (10 MB). Nothing is resized: a larger image is refused with a hint to shrink it (for example with ImageMagick, which the model can run). Attached images are copied into `<sessions>/images/`, named by their SHA-256, and the session stores only that reference. A resumed session therefore sends the exact image the model saw before, even if the original was edited or deleted. Each attached image is recorded in the audit log with its path, size and hash. The image directory is not cleaned up when sessions are deleted (several sessions may share an image); it can be emptied at any time, and an image that is gone becomes a note in the conversation. Key redaction applies to text only: a key that is visible in a screenshot reaches the model.

**Context.** An image costs tokens like text. On Qwen3.8 it is about one token per 32×32 pixels (400×300 ≈ 120 tokens, 1920×1080 ≈ 2,000), and the context estimate uses that. `tools.image_tokens` sets a fixed number per image instead, for models whose encoder counts differently. `/context` shows the images' share. Compaction removes images from the older part of the conversation and leaves a note naming them, so the model can look again with `view_image`; the summary is written from text only.

**Models without vision.** `view_image` is offered only to a model with vision, and `/image` refuses otherwise. If a session with images moves to a model without vision, each image is replaced by a note saying it is not shown, and it is sent again after switching back. If Ollama does not report vision for a model that has it, set `"caps": {"vision": true}` in its profile.

**Cloud models.** Images go to OpenRouter or OpenAI as `image_url` parts with the data inline (a `data:` URL); nothing is fetched from elsewhere, and the local file paths are not sent. Approving a cloud endpoint covers the images in the conversation, like the rest of it.

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

**Fetching pages.** `fetch_url` decides what to return from the response itself, not only from its `Content-Type`:

- HTML (`text/html`, XHTML, or a body that starts with `<!doctype html` or `<html` whatever its label) is converted to readable text: scripts, styles and markup removed, entities decoded, links kept as a numbered list of absolute URLs at the end, so the model can fetch the next page.
- With `raw: true` the model gets the body exactly as sent, for when it needs the HTML source or markup itself. The approval prompt shows `raw`.
- Binary content is never returned, raw or not: a binary type (PDF, archives, images, audio, video, fonts, office documents, executables) or a body with NUL bytes. The model gets one line with the type, the size and a `curl -o` command to download it with `exec_command`, where `file`, `pdftotext`, `unzip -l` or `view_image` can work with it. `application/octet-stream` is judged by its body, since servers also send text files with it.
- Other text (plain text, JSON, SVG, source files) is returned as it is.

The body is capped at `tools.fetch_max_bytes` (128 KB) after conversion.

**Context and compaction.** Before every model call, clauzette estimates the size of the request. Ollama has no tokenize endpoint, so this is a character-based estimate calibrated against the `prompt_eval_count` Ollama reports; once a request has run, the prompt line and `/context` show Ollama's actual `prompt_eval_count` instead of the estimate. clauzette also asks Ollama (`/api/show`) how much context the model itself supports and reports it at startup and in `/context`, so you can see whether `num_ctx` is capping it (e.g. "model supports up to 262k; num_ctx caps this session at 131k"). The thresholds are shares of the context window (the active profile's `num_ctx`), so they suit models with different windows: by default compaction starts at 75% (`compact_at_fraction`; 98k of a 131k window), stage 1 must get below 30% (`compact_target_fraction`), and the newest 15% (`keep_recent_fraction`) always stays verbatim. When the context reaches the threshold, or the reply might not fit in what is left of the window, it compacts in one step. First it replaces old tool output and large tool-call arguments (file contents) with short placeholders, since the model can re-read files. If that does not get below the target, it asks the model (thinking off) to summarize the older part in a fixed structure: goal, decisions, files and their state, commands, open tasks, preferences. The recent part always stays verbatim, and a tool call is never separated from its result. A `*_tokens` value above 0 (`compact_at_tokens`, `compact_target_tokens`, `keep_recent_tokens`, `summary_chunk_tokens`) replaces its share with a fixed number; if those fixed values do not fit a profile's window, clauzette uses the shares for that profile and says so. The reply reserves are the smaller of `reserve_tokens` (`reserve_tokens_thinking` with thinking on) and their share of the window.

The characters-per-token estimate belongs to the model's weights, not to a session: it is kept in `models_file` (default `models.json` next to the audit log), keyed by the weights digest, so every session on the same model starts from what earlier ones learned. Models that share weights, such as Modelfile variants that only change `num_ctx`, share one entry. `/context` shows the estimate in use.

To compact in proportion to the conversation instead, set `context.keep_recent_ratio` (0.1–0.9; 0 = off): `0.5` keeps the newest half of the conversation verbatim, whatever its size, and replaces the keep and target thresholds. The older part loses its old tool output first and is summarized only if that does not at least halve it. Keeping more verbatim keeps exact recent detail, but compaction comes round sooner, and with a hybrid model each one costs a full re-read; repeated summaries also fold earlier summaries in, so the oldest detail fades over time. `/compact 75` summarizes the oldest 75% once, whatever the setting, and `/context` shows what compacting now would keep and summarize.

**Prompt cache stability.** Ollama reuses its processed prompt when a request starts with the same text as the previous one. So the system prompt is assembled once per session and stored with it (the date is frozen at session start), tools are always sent in name order, and every request uses the same options. A different `num_ctx` in any request would also make Ollama reload the model.

**Thinking.** Reasoning from the current turn is sent back to the model, so it remembers why it called its tools. Reasoning from earlier turns is dropped, which saves a lot of context. Whether a model can think comes from Ollama; for a model without a thinking mode the `think` field is not sent at all, and `!think` says it has no effect. Some small models ignore `think: false` and write their reasoning into the reply anyway (qwen3:4b does, ending it with `</think>`); run those with thinking on, or give their profile the `inline-think` quirk (see [Model profiles](#configuration)).

**Models without tools.** When Ollama reports that the model cannot call tools (phi3:mini, for example), clauzette runs it chat-only: no tool definitions are sent, the session uses its chat-only system prompt, and plan and rogue mode are refused (a session resumed in either mode has it switched off). The prompt line shows `chat only`.

**Timeouts.** There is no overall request timeout. Instead each phase has its own limit: connecting (`connect_seconds`), the wait for the first data, which covers loading the model and processing a long prompt (`first_token_seconds`), and the silence allowed once data flows (`inter_token_seconds`). The last one is generous because Ollama sends nothing while the model writes a tool call, which for a large file can take minutes. In rogue mode, `rogue.max_minutes` also applies, as a hard limit.

**Warm-up.** At startup clauzette checks that Ollama is reachable, asks it what the model supports (tools, thinking, vision and its native context, from the same list `ollama list` shows) and which weights it uses, loads the model with the profile's options unless it is already loaded, and reports whether it fits in VRAM. Until that answer arrives, or if Ollama cannot give it, the model is assumed to support tools and thinking. Messages you type meanwhile are queued. `warmup_prefill` additionally pre-processes the system prompt; whether that helps depends on the cache test in the [M0 checklist](#m0-checklist-verify-the-model-and-the-hardware).

**GPU guard.** A background loop reads the exporter every few seconds. Before each model call the agent checks the rules: with `mode: temperature` (the default) it pauses above `pause_above_c` and resumes below `resume_below_c`; with `utilization` it pauses for `util_pause_seconds` after `util_window_seconds` at or above `util_threshold`. A running generation is never interrupted.

**System prompt.** It is assembled from the built-in base prompt (`internal/agent/base_prompt.md`, or `prompt.base_file`; in rogue mode `internal/agent/rogue_prompt.md`, or `prompt.rogue_base_file`; for a model without tools `internal/agent/chat_prompt.md`, or `prompt.chat_base_file`, with an environment block that only says the model has no tools), an optional `prompt.extra_file`, an environment block (date, workspace, tools, context size), and the first of `prompt.project_files` found in the workspace root, such as an `AGENTS.md` with project conventions. With an agent directory, the first one found in the shared workspace root comes first (common rules), then the first one found in the agent's own directory. Base and extra files may use template fields: `{{.Date}}`, `{{.Workspace}}`, `{{.Model}}`, `{{.Shell}}`, `{{join .Tools ", "}}`, `{{.Internet}}`, `{{.NumCtx}}`. Template comments (`{{/* ... */}}`) are removed before sending. To add your own instructions in the cluster, add a key to the ConfigMap and point `prompt.extra_file` at it. All three prompts are built when a session starts and frozen with it (sessions saved before the chat-only prompt existed get it added when they are opened); `/system-prompt` shows the one in use, and `/system-prompt reload` rebuilds all three.

## Configuration

Everything has a default; a config file only needs what you change. `clauzette -print-config` shows the effective configuration, and unknown field names are rejected so typos surface. `config.example.json` lists every field. `CLAUZETTE_OLLAMA_URL`, `CLAUZETTE_MODEL`, `CLAUZETTE_PROFILE`, `CLAUZETTE_WORKSPACE`, `CLAUZETTE_AGENT_ID`, `CLAUZETTE_AGENT_DIR` and `CLAUZETTE_SHARED_ACCESS` override the file. The config file is taken from `-config`, then `$CLAUZETTE_CONFIG`, then `./clauzette.json`, then `/etc/clauzette/config.json`.

**Model profiles.** A profile is a named way of running a model. Without `profiles`, the top-level `model`, `num_ctx`, `think`, `keep_alive` and `options` form one implicit profile called `default`, so existing configs work unchanged. With profiles, those top-level settings become the defaults each profile inherits:

```json
"default_profile": "qwen-big",
"profiles": [
  { "name": "qwen-big", "model": "qwen3.8:27b", "think": "on" },
  { "name": "gemma",    "model": "gemma4:12b", "num_ctx": 196608, "options": { "temperature": 0.7 } },
  { "name": "phi",      "model": "phi3:mini",  "num_ctx": 32768 }
]
```

A profile may set `num_ctx` (tokens, or `"fit"`: the value `/adapt` measured for the model, else the top-level `num_ctx`), `max_ctx` (an upper limit for `"fit"`), `think` (`on` or `off`; unset inherits the default profile's, then the top-level `think`), `options` (added to the top-level ones), `keep_alive`, `caps` to correct what Ollama reports (`{"tools": false}`, `{"context": 65536}`), and `quirks`, named fixes for models that misbehave in a known way. There is one so far: `inline-think`, for a model that writes its reasoning into the reply, ending it with `</think>`, even with thinking off. With it, a reply streamed with thinking off is shown as thinking until `</think>` arrives (if it never does, the text is shown as the answer when the reply ends, so such replies appear all at once), and the stored reply and compaction summaries are split at the tag, so the reasoning never enters the history. Turn it on only for models that need it: a reply that legitimately contains `</think>` would be cut there. `provider`, `base_url` and `api_key_env` select a cloud provider (see Cloud models below); a local profile uses the one Ollama at `ollama_url`. The profile is chosen by `-profile`, else the resumed session's profile, else `default_profile` (or `$CLAUZETTE_PROFILE`), else the first one. Sessions remember their profile, and each reply in the transcript records the model that wrote it.

**Switching models.** `/model` lists the profiles and the installed models: what each supports, its context, which one is loaded and how much of it is on the GPU, and which installed models share weights. `/model <name>` switches to a profile, or to an installed model by name (with the top-level settings). The switch happens between operations: typed while the agent works, it waits for the current turn to end (and is dropped if that turn is cancelled). In order, clauzette checks the target is installed and what it supports; compacts the conversation with the current model if it is too big for the target's window (and refuses if it still is); unloads the current model, so the target has the GPU; loads the target; and only then switches. If loading fails, the current profile stays active and its model reloads on the next request. After a switch, thinking follows the new profile (a `!think` toggle does not carry over), plan mode is switched off for a model without tools, and the session's system prompts are rebuilt with the new model and window (the session's date stays). The first reply after a switch processes the whole conversation again; with `warmup_prefill`, the switch does that right away. `/model` is refused in rogue mode, which stays on the model it started with, and while the first model is still loading. `/load` keeps the loaded model and says how to switch back to the session's own.

**Cloud models (OpenRouter, OpenAI).** A profile with `"provider": "openrouter"` or `"openai"` runs on that provider through its OpenAI-compatible Chat Completions API; `"openai"` with another `base_url` also fits compatible servers such as vLLM or LM Studio. Each provider has built-in defaults (OpenRouter at `https://openrouter.ai/api/v1` with its key in `OPENROUTER_API_KEY`, OpenAI at `https://api.openai.com/v1` with `OPENAI_API_KEY`), which a `providers` section can change:

```json
"providers": { "openrouter": { "free_only": true } },
"profiles": [
  { "name": "or-free", "provider": "openrouter", "model": "vendor/model:free" }
]
```

The key is read from the environment variable named by `api_key_env`; it never goes in the config. Locally, export it before starting clauzette; in Kubernetes, keep it in a Secret, created by hand rather than in git, and expose it to the pod as that variable:

```sh
read -rs k; printf %s "$k" | kubectl -n ai create secret generic clauzette-openrouter --from-file=api-key=/dev/stdin; unset k
```

```yaml
env:
  - name: OPENROUTER_API_KEY
    valueFrom:
      secretKeyRef: {name: clauzette-openrouter, key: api-key, optional: true}
```

With `optional: true` the pod also starts without the Secret; cloud models then report the missing key. Keys stay out of what the model sees. Commands it runs (`exec_command`, the git tools) do not inherit the key variables (every `api_key_env`, plus `OPENROUTER_API_KEY` and `OPENAI_API_KEY`). clauzette marks itself non-dumpable at start, so its `/proc/<pid>/environ` belongs to root and a command running as the same user cannot read the keys there either (this also disables core dumps and attaching a debugger as that user). And any key value that turns up in tool output anyway, for example from a `.env` file in the workspace, is replaced with `[redacted NAME]` before it reaches the conversation, the session file or the audit log. Other secrets in files or command output are not recognised: they go wherever the conversation goes. The built-in key variables belong to the built-in servers: a provider or profile with its own `base_url` sends no key unless it names its own `api_key_env`, so a profile pointed at vLLM or a proxy never receives your OpenAI or OpenRouter key. With `free_only`, any model whose price is not zero is refused, both when switching to it and on every request. `/model --online [filter] [--all]` lists OpenRouter's models, free ones unless `--all`, with their context, capabilities and price; `/model openrouter:<id>` switches to one without a profile, with the top-level settings. A cloud model's window is the profile's `num_ctx`, else the top-level `num_ctx`, capped at the provider's limit (several free models advertise a million tokens, which compaction at 75% would turn into very large requests). Capabilities and the limit come from OpenRouter's model list; a plain OpenAI-compatible server only names its models, so tools and thinking are assumed. Rate limits are retried when the wait is short; OpenRouter's daily free-model quota (50 requests a day until $10 of credit has been bought, then 1000) is reported with its reset time instead, since each agent step is one request. Nothing local applies to a cloud model: no `/adapt`, warm-up loading, GPU placement or GPU guard pause, and switching to one leaves the local model loaded. Before the first request to a cloud endpoint (the provider and its server) in a session, clauzette asks, whether the session got there by `/model`, `-profile`, `default_profile`, `-resume` or `/clear`, because from then on the conversation, including the file contents and command output in it, is sent to that provider (some free endpoints may log or train on prompts; OpenRouter has account settings for this). The prompt line shows `cloud: <provider>`.

**Finding the largest context: `/adapt`.** `/adapt <profile or model>` measures the largest context with which a model stays entirely on the GPU, and profiles with `"num_ctx": "fit"` use it from then on (capped by `max_ctx`). Run it by hand, once per model and whenever the GPU or Ollama's settings change; it is not done automatically. It unloads every loaded model, whoever loaded it, and refuses if VRAM is still in use afterwards (something outside Ollama holds the GPU, so the result would be too small). Then it loads the model at 100%, 75%, 50% and 25% of its native context, largest first, and searches in 8k steps between the largest size that fit and the smallest that did not. A size fits when Ollama keeps all of the model on the GPU (it does not fail when a model is too big; it quietly moves layers to the CPU) and at most 97% of VRAM is in use. That takes a handful of loads, usually from the page cache. `--verify` also runs one request filling about 90% of the result, which shows that it really runs and how long a full context takes to process. Afterwards the active profile is loaded again, with its new window if it uses `"fit"`; `!c` stops it at any time and also reloads the active profile. Results are kept in `models_file` per set of weights (Modelfile variants of one model share them); `/adapt` lists them, `/adapt <name> set <tokens>` pins a value and `/adapt <name> forget` drops it (both apply from the next start or `/model` switch). Each later load with an adapted value is checked against the measurements: if the model no longer fits fully on the GPU, loads at a clearly different size (a changed KV cache type, say) or the GPU's memory differs, the value is marked stale and not used again until `/adapt` runs; a new Ollama version only warns.

GPU memory comes from the exporter at `gpu.metrics_url`, the same one the GPU guard reads; only the URL is needed, not `gpu.enabled`. The defaults fit the DCGM exporter (`DCGM_FI_DEV_FB_USED` and `_FREE`, in MiB). For an nvidia-smi based exporter such as `nvidia_gpu_exporter`, set `"mem_used_metric": "nvidia_smi_memory_used_bytes"`, `"mem_total_metric": "nvidia_smi_memory_total_bytes"`, `"mem_free_metric": ""` and `"mem_scale": 1`. Without memory metrics, `/adapt` still works, but checks only that the model stays on the GPU, without the 97% margin.

**Context size.** To change the context limit, set `num_ctx` (top-level, or per profile). It never exceeds the context the model supports, which clauzette reports at startup and in `/context` (e.g. "model supports up to 262k"); a larger value is lowered to the model's limit. The compaction thresholds follow the window (see [Context and compaction](#how-the-important-parts-work)). A higher `num_ctx` uses more GPU memory for the KV cache and can slow generation, because the model and the cache compete for the GPU; a model that does not fit fully on the GPU will still work, just slower. How much the cache grows per token depends on the model's attention layout, so two models of similar size can fit very different windows.

**One directory per agent.** Several agents can share one workspace, each in its own directory. With `"agent_dir": true` the agent works in `<workspace>/<agent_id>`, created on first start; relative paths and `exec_command` start there. `agent_id` defaults to `$CLAUZETTE_AGENT_ID`, then the hostname, which in Kubernetes is the pod name; it must be letters, digits, `.`, `_` and `-`. `shared_access` says what the file tools may do in the rest of the shared workspace, such as `../other-agent/notes.md`: `none`, `read` (the default: read, list and search) or `write`. Sessions and backups move to `<sessions_dir>/<agent_id>/`; sessions saved earlier in `<sessions_dir>` still load with `-session <id>`. Audit records get an `agent` field.

Two limits. `shared_access` binds the file tools only: `exec_command` runs a normal shell, which sees whatever the pod mounts, so real isolation needs the mount itself (one pod per agent, the shared volume mounted with `subPathExpr`). And a Deployment's pod name changes on every restart, so set `CLAUZETTE_AGENT_ID` to a fixed name there, or use a StatefulSet, whose pod names are stable.

## Security model

The agent can only do what the pod allows. The manifests run it as a non-root user with a read-only root filesystem, no Linux capabilities, and no Kubernetes API token. Outbound network access is not limited at the network layer: the shipped NetworkPolicy (`deploy/30-networkpolicy.yaml`) allows DNS, the in-cluster backends and the public Internet while denying the rest of the cluster, but it is enforced only by a policy-capable CNI (Calico, Cilium); under flannel it is inert, and containment is app-layer (`fetch_url`'s allowlist and SSRF guard, command approvals, the rogue-mode budget). File access is confined to the workspace, writes and commands need approval, and every tool call is appended to the audit log on the state volume (inside `kubectl exec`, the program's output goes to your terminal, not to the pod log). With a cloud profile, the conversation leaves the cluster: everything in it, including file contents and command output, goes to the provider; clauzette asks before the first request to each cloud endpoint in a session. API keys are kept from the model's commands and redacted from tool output (see Cloud models).

Treat three choices deliberately. Opening internet access lets commands install packages, and also lets a prompt-injected command send data out. `auto_approve` for `exec_command` removes your last checkpoint, and so does rogue mode. Running locally drops the pod's limits altogether (see [Running locally](#running-locally)).

## Not built yet

A separate client binary for machines that do not run clauzette itself. The event-based core is designed so these can be added without changing the agent loop.
