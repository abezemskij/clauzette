package control

// HelpText is the command reference shown by /help and !help.
const HelpText = `Any time, even while the agent works:
  !c                cancel the current operation (and discard queued messages)
  !q                save and exit
  !think [on|off]   toggle model thinking (applies from the next model request)
  !show [on|off]    toggle displaying the thinking text
  !safe [on|off]    safe mode: back up before mv/delete, and confirm each one separately
  !plan [on|off]    plan mode: capture write/exec actions instead of running them
  !skip             skip a GPU cool-down pause
  !!text            send a message that starts with '!'
While idle:
  /context          context usage and compaction settings
  /compact [percent]  compact the context now; with a percent (10-90),
                    summarise that share of the oldest conversation this once
  /undo  /retry     drop the last exchange (and resend it)
  /image <path> [question]  send an image with a question (default: describe
                    it); a relative path is from the workspace root, quote a
                    path with spaces. Needs a model with vision
  /sessions  /new  /load <id>
  /clear            discard this session's conversation and start a new one
                    (files and safe-mode backups are kept; asks first)
  /model [name]     list profiles and installed models, or switch to a profile
                    or model (also mid-task: then it waits for the task to end;
                    not in rogue mode). A cloud model is provider:model, e.g.
                    openrouter:vendor/model:free; the first switch to a cloud
                    provider in a session asks first
  /model --online [filter] [--all]  OpenRouter's free models (--all: paid too)
  /adapt [name [--verify]]  list adapted models, or find the largest context
                    that keeps a model fully on the GPU (unloads all models,
                    takes minutes; --verify also runs one near-full request)
  /adapt <name> set <tokens> | forget   pin or drop a model's value
  /system-prompt [reload]  show the system prompt in use, or rebuild all three
                    (normal, rogue mode, chat-only)
  /gpu              GPU guard status
  /markdown [on|off]  render the model's Markdown (headings, bold, code, lists)
                    or show it as written
  /approvals [reset]
  /safe [restore <n> [force]]  safe mode status and backups, restore a backup
                    (force replaces what exists now, after backing it up)
  /rogue [on [steps]|off]  rogue mode: no approvals, work until the model calls
                    finish (budget: steps or agent.max_steps, rogue.max_minutes);
                    /rogue off also works mid-task
  /fetch [on|off|allow <host>|forget <host>]  fetch_url allowlist: when on, a
                    listed host runs without asking (even in rogue), an unlisted
                    one is always asked about
  /plan             pending actions from plan mode
  /plan approve [n ...|all] [--no-continue] [comment]
                    run pending actions in order; the comment goes to the model
                    with the results, --no-continue holds them for your next message
  /plan reject [n ...|all] [reason]  discard them and tell the model why
                    (use -- before a comment or reason that starts with a number)
  /help  /exit
Input:
  """ on its own line starts and ends a multi-line message.
  Pasting multiple lines is taken as one message and sent on the next Enter,
  not split per line (needs a terminal with bracketed paste; most have it).
  Messages typed while the agent works are queued.
  Ctrl+D, Ctrl+C or a dropped connection save the session and exit.
Approvals:
  y = yes, n [reason] = no, a = always for this tool in this session
      (for a gated fetch, a adds the domain to the allowlist instead), v = show the full diff`
