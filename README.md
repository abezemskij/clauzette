<p align="center">
  <img src="media/logo.svg" width="270" alt="Clauzette">
</p>

# Clauzette

A terminal AI agent for a local Ollama model. It reads, searches, writes and edits files, runs commands, searches the web and reads git history, asks before changing anything, keeps its context under control by compacting itself, and pauses when the GPU runs hot. It is built to run as a pod in Kubernetes on a shared workspace, and runs just as well on your own machine.

Pure Go standard library, no external dependencies. Linux only.

## Features

- **Approvals with diffs.** Reads run automatically; every write, edit and command shows a diff or the command line and waits for `y`.
- **Safe workspace access.** File tools are confined to the workspace through `os.Root`, never overwrite files changed by someone else, and handle symlinks safely. Several agents can share one workspace, each in its own directory.
- **Beyond the workspace.** Web search through a SearXNG instance, URL fetch with a per-host allowlist (HTML comes back as readable text with scripts and markup stripped and links kept as a numbered reference list), read-only git (status, diff, log), an IP-to-country lookup, and a todo list the model keeps for itself on longer tasks.
- **Modes.** *Safe mode* backs up before moves and deletes. *Plan mode* collects changes for review before anything runs. *Rogue mode* works unattended until the task is done, within a step budget and a time limit.
- **Long sessions.** Automatic compaction (fixed size or a proportion of the conversation), sessions saved after every step and resumable after a restart, and a complete transcript that is never shortened.
- **Made for local models.** Stable prompts for Ollama's prompt cache, phased timeouts for slow model loads, a GPU temperature guard, and an audit log of every tool call.

## Quick start

**In Kubernetes** (manifests in `deploy/`; set the image name first, and see [Running in Kubernetes](DETAILS.md#running-in-kubernetes) for the volumes you provide and when to skip the Ollama manifest):

```sh
kubectl apply -f deploy/
tmux new -A -s agent
kubectl -n ai exec -it deploy/clauzette -- clauzette            # new session
kubectl -n ai exec -it deploy/clauzette -- clauzette -resume    # continue the last one
```

**Locally** (Go 1.25+, Ollama on this machine): create `clauzette.local.json` as shown in [Running locally](DETAILS.md#running-locally), then:

```sh
make build && mkdir -p scratch
./bin/clauzette -config clauzette.local.json            # workspace ./scratch, sessions and audit log in ./.local
./bin/clauzette -config clauzette.local.json -resume
```

Locally, commands run as your own user with your credentials in reach; read the notes in [Running locally](DETAILS.md#running-locally) first.

## Everyday commands

| Command | Effect |
|---|---|
| `!c` / `!q` | Cancel the current work / save and exit (work at any time) |
| `!think`, `!safe`, `!plan` `[on\|off]` | Thinking, safe mode, plan mode |
| `/context`, `/compact [percent]` | Context usage; compact now |
| `/undo`, `/retry` | Drop the last exchange (and resend it) |
| `/sessions`, `/new`, `/load <id>` | Manage sessions |
| `/clear` | Discard this session's conversation and start fresh (files kept) |
| `/model [name]` | List models and profiles; switch between them |
| `/adapt [name]` | Find the largest context that keeps a model fully on the GPU |
| `/plan approve [n\|all] [comment]` | Run planned actions, with a note for the model |
| `/rogue [on [steps]\|off]` | Unattended mode |
| `/help` | Everything else |

The full list, with what each mode does, is in [Using it](DETAILS.md#using-it). For multi-line input, `"""` on its own line starts and ends a block, and pasting several lines at once is taken as one message (sent on the next Enter) rather than split per line.

## Documentation

[DETAILS.md](DETAILS.md) covers the architecture, building and deploying, running locally, the M0 checklist for verifying the model and GPU, every command and mode, how compaction and the other internals work, configuration and the security model. `config.example.json` lists every setting, and `clauzette -print-config` shows the effective configuration.

## Security

In the pod, the agent can do only what the pod allows: non-root, read-only root filesystem, no capabilities, and no Kubernetes token. File tools are confined to the workspace, and every tool call is written to an audit log. Outbound network access is **not** restricted at the network layer: the shipped `NetworkPolicy` (`deploy/30-networkpolicy.yaml`) is inert under a connectivity-only CNI such as flannel, and the agent legitimately reaches DNS, Ollama, SearXNG, Postgres, the GPU exporter and the public Internet (for `fetch_url` and commands). Outbound containment is therefore app-layer — `fetch_url`'s per-host allowlist and SSRF guard, command approvals, and the rogue-mode budget. The shell is not confined beyond the pod, so instructions hidden in files or command output are the main risk: keep approvals on for commands unless the agent runs isolated. The NetworkPolicy becomes a real second line of defence (notably against reaching the cluster's own infrastructure) once a policy-enforcing CNI like Calico or Cilium is installed. Cloud profiles (OpenRouter, OpenAI) send the conversation, including file contents and command output, to that provider; the default stays on the local model. See the [Security model](DETAILS.md#security-model).
