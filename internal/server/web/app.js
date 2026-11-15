// The web page of clauzette serve. It speaks the same protocol as
// clauzette attach (package wire): records from GET /api/events are drawn,
// and input is sent to POST /api/input exactly as typed. Menus, panels and
// buttons only send the same commands you could type, or read data from
// the read-only endpoints (sessions, models, todos, plan, files).
//
// Everything the model, a tool or a workspace file contains is untrusted:
// the page builds elements with textContent only, never from markup
// strings, and links work only for http(s) URLs. The content security
// policy (no inline script, no other origins) backs this up.
"use strict";

// agent.EventKind values (internal/agent/events.go; checked by a Go test).
const Ev = { Text: 0, Thinking: 1, StreamEnd: 2, Waiting: 3, WaitingDone: 4, ToolCall: 5, ToolResult: 6, Approval: 7, Info: 8, Warn: 9, Error: 10, Usage: 11 };

const $ = (id) => document.getElementById(id);
const log = $("log");

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

function button(label, cls, onclick, title) {
  const b = el("button", cls, label);
  b.type = "button";
  if (title) b.title = title;
  b.addEventListener("click", onclick);
  return b;
}

function humanBytes(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1).replace(/\.0$/, "") + " KB";
  return (n / 1024 / 1024).toFixed(1).replace(/\.0$/, "") + " MB";
}

function ktok(n) {
  if (n >= 1000) return (Math.round(n / 100) / 10).toString().replace(/\.0$/, "") + "k";
  return String(n);
}

// ------------------------------------------------------------- the token

const store = {
  get(k) { try { return sessionStorage.getItem(k) || localStorage.getItem(k) || ""; } catch (e) { return ""; } },
  setToken(t, remember) {
    try {
      sessionStorage.setItem("token", t);
      if (remember) localStorage.setItem("token", t); else localStorage.removeItem("token");
    } catch (e) { /* storage blocked: the token lasts for this page only */ }
  },
  clearToken() { try { sessionStorage.removeItem("token"); localStorage.removeItem("token"); } catch (e) { } },
  local(k, v) {
    try { if (v === undefined) return localStorage.getItem(k); localStorage.setItem(k, v); } catch (e) { }
    return null;
  },
};
let token = store.get("token");

class Unauthorized extends Error { }

async function api(method, path, body, headers) {
  const opts = {
    method,
    headers: Object.assign({ Authorization: "Bearer " + token }, headers || {}),
    cache: "no-store",
    credentials: "omit",
  };
  if (body !== undefined) {
    if (body instanceof Blob) opts.body = body;
    else { opts.body = JSON.stringify(body); opts.headers["Content-Type"] = "application/json"; }
  }
  const resp = await fetch(path, opts);
  if (resp.status === 401) throw new Unauthorized("the server rejected the token");
  if (!resp.ok) {
    let msg = resp.status + "";
    try { const j = await resp.json(); if (j.error) msg = j.error; } catch (e) { }
    throw new Error(msg);
  }
  return resp;
}

async function apiJSON(path) { return (await api("GET", path)).json(); }

function unauthorized() {
  store.clearToken();
  showLogin("The server rejected the token.");
}

function showLogin(message) {
  stopStream();
  $("login").hidden = false;
  $("app").hidden = true;
  $("login-error").hidden = !message;
  $("login-error").textContent = message || "";
  $("token").focus();
}

$("login-form").addEventListener("submit", (e) => {
  e.preventDefault();
  token = $("token").value.trim();
  store.setToken(token, $("remember").checked);
  $("token").value = "";
  start();
});

// ------------------------------------------------------------- scrolling

// The log follows new output while you are at the bottom. Scrolling up
// yourself stops that (a "latest" button takes you back); approvals and
// questions always bring you down, since they need an answer.
let follow = true;
let lastTop = 0;
function nearBottom() { return log.scrollHeight - log.scrollTop - log.clientHeight < 60; }
function toBottom() { log.scrollTop = log.scrollHeight; lastTop = log.scrollTop; }
function setFollow(on) {
  follow = on;
  $("jump").hidden = on;
  if (on) toBottom();
}
log.addEventListener("scroll", () => {
  // Only a move up is the reader's doing: output growing never lowers scrollTop.
  if (log.scrollTop < lastTop - 4 && !nearBottom()) { follow = false; $("jump").hidden = false; }
  else if (nearBottom()) { follow = true; $("jump").hidden = true; }
  lastTop = log.scrollTop;
});
new MutationObserver(() => { if (follow) toBottom(); }).observe(log, { childList: true, subtree: true, characterData: true });
window.addEventListener("resize", () => { if (follow) toBottom(); });
$("jump").addEventListener("click", () => setFollow(true));

function add(node) { log.appendChild(node); return node; }
function keepDown(fn) { fn(); }

// ------------------------------------------------------------- clipboard

async function copyText(text, btn) {
  let ok = false;
  try { await navigator.clipboard.writeText(text); ok = true; } catch (e) {
    const t = el("textarea");
    t.value = text;
    document.body.appendChild(t);
    t.select();
    try { ok = document.execCommand("copy"); } catch (e2) { }
    t.remove();
  }
  if (btn) {
    const old = btn.textContent;
    btn.textContent = ok ? "copied" : "copy failed";
    setTimeout(() => { btn.textContent = old; }, 1200);
  }
}

// -------------------------------------------------------------- Markdown

const linkRE = /^https?:\/\//i;

// inline renders `code`, **bold**, *italic* and [text](url) into parent.
function inline(parent, text) {
  const re = /(`+)([\s\S]*?[^`])\1(?!`)|\*\*(?=\S)([\s\S]*?\S)\*\*|(^|[\s(\["'])\*(?=[\p{L}\p{N}`])([^*\n]*?\S)\*|\[([^\]\n]+)\]\(([^)\s]+)\)/gu;
  let last = 0, m;
  while ((m = re.exec(text)) !== null) {
    let start = m.index;
    if (m[4] !== undefined) start += m[4].length; // the space before *italic*
    if (start > last) parent.appendChild(document.createTextNode(text.slice(last, start)));
    if (m[1] !== undefined) parent.appendChild(el("code", "", m[2].replace(/^ (.*) $/, "$1")));
    else if (m[3] !== undefined) inline(parent.appendChild(el("strong")), m[3]);
    else if (m[5] !== undefined) inline(parent.appendChild(el("em")), m[5]);
    else if (linkRE.test(m[7])) {
      const a = el("a", "", m[6]);
      a.href = m[7];
      a.rel = "noopener noreferrer";
      a.target = "_blank";
      parent.appendChild(a);
    } else parent.appendChild(document.createTextNode(m[0]));
    last = m.index + m[0].length;
  }
  if (last < text.length) parent.appendChild(document.createTextNode(text.slice(last)));
}

function cells(row) {
  let s = row.trim();
  if (s.startsWith("|")) s = s.slice(1);
  if (s.endsWith("|")) s = s.slice(0, -1);
  return s.split("|").map((c) => c.trim());
}

// markdown renders a whole reply (it is re-rendered as it streams).
function markdown(target, src) {
  target.replaceChildren();
  const lines = src.split("\n");
  let para = null, list = null;
  const endBlocks = () => { para = null; list = null; };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const fence = line.match(/^\s*(```|~~~)\s*(\S*)/);
    if (fence) {
      endBlocks();
      const pre = target.appendChild(el("pre"));
      if (fence[2]) pre.appendChild(el("span", "lang", fence[2]));
      const code = pre.appendChild(el("code"));
      const body = [];
      for (i++; i < lines.length && !lines[i].trim().startsWith(fence[1]); i++) body.push(lines[i]);
      code.textContent = body.join("\n");
      pre.appendChild(button("copy", "copy", (e) => copyText(code.textContent, e.currentTarget), "copy the code"));
      continue;
    }
    if (line.trim() === "") { endBlocks(); continue; }
    const h = line.match(/^\s*(#{1,6})\s+(.*)$/);
    if (h) { endBlocks(); inline(target.appendChild(el("h" + Math.min(h[1].length, 4))), h[2]); continue; }
    if (/^\s*([-*_])(\s*\1){2,}\s*$/.test(line)) { endBlocks(); target.appendChild(el("hr")); continue; }
    if (line.trim().startsWith("|") && i + 1 < lines.length && /^\s*\|?\s*:?-{2,}/.test(lines[i + 1])) {
      endBlocks();
      const table = target.appendChild(el("table"));
      const head = table.appendChild(el("tr"));
      cells(line).forEach((c) => inline(head.appendChild(el("th")), c));
      for (i += 2; i < lines.length && lines[i].trim().startsWith("|"); i++) {
        const tr = table.appendChild(el("tr"));
        cells(lines[i]).forEach((c) => inline(tr.appendChild(el("td")), c));
      }
      i--;
      continue;
    }
    const bullet = line.match(/^(\s*)[-*+]\s+(.*)$/);
    if (bullet) {
      para = null;
      if (!list) list = target.appendChild(el("ul"));
      const li = list.appendChild(el("li"));
      if (bullet[1].length >= 2) li.style.marginLeft = Math.min(bullet[1].length, 12) * 8 + "px";
      inline(li, bullet[2]);
      continue;
    }
    const quote = line.match(/^\s*>\s?(.*)$/);
    if (quote) { endBlocks(); inline(target.appendChild(el("blockquote")), quote[1]); continue; }
    list = null;
    if (!para) para = target.appendChild(el("p"));
    else para.appendChild(el("br"));
    inline(para, line);
  }
}

// --------------------------------------------------------------- drawing

let reply = null;      // the reply being streamed: {node, text, queued}
let thinking = null;   // the thinking being streamed: {node, pre}
let waiting = null;    // {node, timer}
let card = null;       // the open approval card
let status = null;

function endStream() {
  if (reply) {
    const r = reply;
    markdown(r.node, r.text);
    if (r.text.trim()) {
      const acts = r.node.appendChild(el("div", "reply-actions"));
      acts.appendChild(button("copy as Markdown", "", (e) => copyText(r.text, e.currentTarget)));
    }
  }
  reply = null;
  thinking = null;
}

function line(cls, text) { endStream(); return add(el("div", "line " + cls, text)); }

function streamText(t) {
  thinking = null;
  if (!reply) reply = { node: add(el("div", "reply")), text: "", queued: false };
  reply.text += t;
  if (!reply.queued) {
    reply.queued = true;
    const r = reply;
    requestAnimationFrame(() => { r.queued = false; if (reply === r) keepDown(() => markdown(r.node, r.text)); });
  }
}

function streamThinking(t) {
  if (reply) endStream();
  if (!thinking) {
    const d = add(el("details", "thinking"));
    d.appendChild(el("summary", "", "thinking"));
    thinking = { node: d, pre: d.appendChild(el("pre")) };
    if (status && status.show_thinking) d.open = true;
  }
  keepDown(() => { thinking.pre.textContent += t; });
}

function startWaiting(text) {
  stopWaiting();
  endStream();
  const node = add(el("div", "waiting"));
  const since = Date.now();
  const draw = () => { node.textContent = "⋯ " + text + " " + Math.round((Date.now() - since) / 1000) + "s"; };
  draw();
  waiting = { node, timer: setInterval(draw, 1000) };
}

function stopWaiting() {
  if (!waiting) return;
  clearInterval(waiting.timer);
  waiting.node.remove();
  waiting = null;
}

function toolResult(text, bad) {
  endStream();
  const d = add(el("details", "result" + (bad ? " bad" : "")));
  const lines = text.replace(/\n+$/, "").split("\n");
  d.appendChild(el("summary", "", (bad ? "  ✗ " : "  ✓ ") + lines[0] + (lines.length > 1 ? "  (" + lines.length + " lines)" : "")));
  d.appendChild(el("pre", "", text));
}

function render(ev) {
  switch (ev.kind) {
    case Ev.Text: stopWaiting(); streamText(ev.text || ""); break;
    case Ev.Thinking: stopWaiting(); streamThinking(ev.text || ""); break;
    case Ev.StreamEnd: endStream(); break;
    case Ev.Waiting: startWaiting(ev.text || "waiting"); break;
    case Ev.WaitingDone: stopWaiting(); break;
    case Ev.ToolCall: {
      endStream();
      const d = add(el("div", "tool"));
      d.appendChild(el("span", "name", "→ " + ev.tool));
      d.appendChild(document.createTextNode(" " + (ev.text || "")));
      break;
    }
    case Ev.ToolResult: toolResult(ev.text || "", ev.is_error); break;
    case Ev.Info: line("info", "· " + ev.text); break;
    case Ev.Warn: warn(ev.text); break;
    case Ev.Error: line("err", "✗ " + ev.text); break;
    case Ev.Usage:
      if (ev.usage && ev.usage.NumCtx > 0) {
        if (status) { status.used_tokens = ev.usage.Used; status.window = ev.usage.NumCtx; status.compact_at = ev.usage.CompactAt; }
        drawMeter(ev.usage.Used, ev.usage.NumCtx, ev.usage.CompactAt);
      }
      if (ev.usage && ev.usage.TokPerSec > 0) {
        line("usage", "[" + ev.usage.Eval + " tokens, " + ev.usage.TokPerSec.toFixed(1) + " tok/s · context ~" + ktok(ev.usage.Tokens) + "/" + ktok(ev.usage.NumCtx) + "]");
      }
      break;
  }
}

// warn shows a warning; a y/N question gets buttons.
function warn(text) {
  const d = line("warn", "! " + text);
  if (/\[y\/N\]\s*$/.test(text)) {
    notify("question", "Clauzette asks", text);
    setFollow(true);
    const actions = d.appendChild(el("span"));
    actions.style.marginLeft = "8px";
    const answer = (a) => { yes.disabled = no.disabled = true; send(a, false); };
    const yes = actions.appendChild(button("Yes", "primary", () => answer("y")));
    const no = actions.appendChild(button("No", "", () => answer("n")));
  }
}

const riskName = ["read-only", "write", "exec", "destructive"];

function showApproval(a) {
  endStream();
  closeCard("");
  notify("approval", "Approval needed", a.summary);
  const c = add(el("div", "card"));
  c.appendChild(el("div", "head", "Approval needed (" + (riskName[a.risk] || "?") + "): " + a.summary));
  if (a.preview) c.appendChild(diffPre(a.preview));
  const actions = c.appendChild(el("div", "actions"));
  const reason = el("input");
  reason.placeholder = "reason (optional)";
  const answer = (input, label) => { send(input, false); closeCard(label); };
  actions.appendChild(button("Approve", "primary", () => answer("y", "approved")));
  actions.appendChild(button(a.allow_scope ? "Allowlist " + a.allow_scope : "Always " + a.tool, "", () =>
    answer("a", a.allow_scope ? "approved; " + a.allow_scope + " added to the allowlist" : "approved; " + a.tool + " runs without asking this session")));
  actions.appendChild(reason);
  const no = actions.appendChild(button("Deny", "danger", () => {
    const r = reason.value.trim();
    answer(r ? "n " + r : "n", r ? "denied: " + r : "denied");
  }));
  reason.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); no.click(); } });
  card = c;
  setFollow(true);
}

function diffPre(text) {
  const pre = el("pre");
  for (const l of text.replace(/\n+$/, "").split("\n")) {
    let cls = "";
    if (l.startsWith("@@")) cls = "hunk";
    else if (l.startsWith("+") && !l.startsWith("+++")) cls = "add";
    else if (l.startsWith("-") && !l.startsWith("---")) cls = "del";
    pre.appendChild(el("span", cls, l + "\n"));
  }
  return pre;
}

function closeCard(label) {
  if (!card) return;
  card.classList.add("answered");
  if (label) card.appendChild(el("div", "answer", label));
  card = null;
}

// ----------------------------------------------------- status and toolbar

// Each toggle shows a mode and sends the command that changes it.
const toggles = [
  { label: "Think", key: "think", on: "!think on", off: "!think off", show: (s) => s.think_available, title: "model thinking (!think)" },
  { label: "Show thinking", key: "show_thinking", on: "!show on", off: "!show off", title: "open the thinking text (!show)" },
  { label: "Safe", key: "safe", on: "!safe on", off: "!safe off", title: "safe mode: back up before mv/delete, confirm each (!safe)" },
  { label: "Plan", key: "plan", on: "!plan on", off: "!plan off", title: "plan mode: capture actions instead of running them (!plan)" },
  { label: "Fetch allowlist", key: "fetch_gate", on: "/fetch on", off: "/fetch off", title: "fetch_url allowlist gate (/fetch)" },
  { label: "Rogue", key: "rogue", cls: "rogue", on: "/rogue on", off: "/rogue off", title: "rogue mode: no approvals until the task is done (/rogue)",
    confirm: "Rogue mode runs every action without asking until the model finishes the task. Turn it on?" },
];
const toggleButtons = {};
for (const t of toggles) {
  const b = button(t.label, "toggle " + (t.cls || ""), () => {
    const on = status && status[t.key];
    if (!on && t.confirm && !window.confirm(t.confirm)) return;
    send(on ? t.off : t.on, false);
  }, t.title);
  toggleButtons[t.key] = b;
  $("toggles").appendChild(b);
}

function drawStatus() {
  const s = status;
  if (!s) return;
  $("models-btn").textContent = s.model + " ▾";
  $("models-btn").title = s.model + " @ " + s.server;
  $("sessions-btn").textContent = "Session " + s.session + " ▾";
  const box = $("status");
  box.replaceChildren();
  if (s.rogue) box.appendChild(el("span", "rogue", "ROGUE "));
  const parts = [];
  if (s.chat_only) parts.push("chat only");
  if (s.cloud) parts.push("cloud: " + s.cloud);
  if (s.gpu) parts.push(s.gpu);
  if (!s.warm) parts.push("model loading");
  if (s.plan_pending) parts.push(s.plan_pending + " planned");
  if (s.vision) parts.push("sees images");
  parts.push(s.busy ? "working…" : s.approval ? "waiting for your approval" : "ready");
  box.appendChild(document.createTextNode(parts.join(" · ")));
  $("stop").disabled = !s.busy;

  for (const t of toggles) {
    const b = toggleButtons[t.key];
    b.hidden = t.show ? !t.show(s) : false;
    b.classList.toggle("on", !!s[t.key]);
  }
  toggleButtons.plan.textContent = "Plan" + (s.plan_pending ? " (" + s.plan_pending + ")" : "");

  drawMeter(s.used_tokens, s.window, s.compact_at);
}

// drawMeter shows the context used. It runs on every status and also on
// every usage event, so it moves during a long run, not only when it ends.
function drawMeter(used, win, compactAt) {
  used = used || 0; win = win || 1;
  $("meter-fill").style.width = Math.min(100, (100 * used) / win) + "%";
  $("meter-mark").style.left = Math.min(100, (100 * (compactAt || win)) / win) + "%";
  $("meter").classList.toggle("near", !!compactAt && used >= 0.85 * compactAt);
  $("meter-text").textContent = ktok(used) + "/" + ktok(win);
}

let wasBusy = false;
function statusChanged(kind) {
  drawStatus();
  if (wasBusy && !status.busy && !status.approval && kind === "prompt") notify("done", "Clauzette finished", "ready for your next message");
  wasBusy = status.busy;
  if (!$("panel").hidden) refreshPanel();
}

// apply draws one record.
function apply(r) {
  if (r.status) status = r.status;
  switch (r.kind) {
    case "info": line("info", "· " + r.text); break;
    case "warn": warn(r.text); break;
    case "error": line("err", "✗ " + r.text); break;
    case "block": endStream(); add(el("div", "block mono", r.text)); break;
    case "dim": line("info", r.text); break;
    case "echo": line("echo", "› " + r.text); break;
    case "render": if (r.event) render(r.event); break;
    case "approval": if (r.approval) showApproval(r.approval); break;
    case "ask": break; // the card is still open
    case "prompt": case "refresh": case "working": endStream(); break;
    case "stop_waiting": stopWaiting(); break;
    case "end_stream": endStream(); break;
    case "gap": line("warn", "! " + r.text + " earlier records were dropped while this page was away"); break;
  }
  if (r.status) statusChanged(r.kind);
}

// ---------------------------------------------------------- notifications

let notifyOn = store.local("notify") === "1";
let quietUntil = 0; // no notifications while the backlog is replayed
const baseTitle = document.title;

function drawNotify() {
  const b = $("notify-btn");
  b.textContent = notifyOn ? "🔔" : "🔕";
  b.classList.toggle("on", notifyOn);
}
drawNotify();

$("notify-btn").addEventListener("click", async () => {
  notifyOn = !notifyOn;
  if (notifyOn && "Notification" in window && Notification.permission === "default") {
    try { await Notification.requestPermission(); } catch (e) { }
  }
  store.local("notify", notifyOn ? "1" : "0");
  drawNotify();
});

// notify marks the tab title, and with notifications on shows one, when
// the page is not being looked at.
function notify(kind, title, body) {
  if (!document.hidden || Date.now() < quietUntil) return;
  document.title = (kind === "done" ? "✓ " : "(!) ") + title + " — " + baseTitle;
  if (notifyOn && "Notification" in window && Notification.permission === "granted") {
    try { new Notification(title, { body: (body || "").slice(0, 200), tag: "clauzette-" + kind }); } catch (e) { }
  }
}
document.addEventListener("visibilitychange", () => { if (!document.hidden) document.title = baseTitle; });

// ---------------------------------------------------------------- stream

let lastSeq = 0;
let streamAbort = null;
let failures = 0;

function setConn(state, title) {
  const c = $("conn");
  c.className = "conn " + state;
  c.title = title;
}

function stopStream() {
  if (streamAbort) streamAbort.abort();
  streamAbort = null;
}

// EventSource cannot send the token, so the stream is read with fetch.
async function stream() {
  stopStream();
  const ctl = new AbortController();
  streamAbort = ctl;
  for (;;) {
    try {
      const q = lastSeq > 0 ? "since=" + lastSeq : "since=0&tail=200";
      quietUntil = Date.now() + 1500;
      const resp = await fetch("/api/events?" + q, { headers: { Authorization: "Bearer " + token }, signal: ctl.signal, cache: "no-store", credentials: "omit" });
      if (resp.status === 401) { unauthorized(); return; }
      if (!resp.ok || !resp.body) throw new Error("events: " + resp.status);
      if (failures > 0) line("info", "· reconnected");
      failures = 0;
      setConn("on", "connected");
      const reader = resp.body.getReader();
      const dec = new TextDecoder();
      let buf = "";
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const chunk = buf.slice(0, i);
          buf = buf.slice(i + 2);
          for (const l of chunk.split("\n")) {
            if (!l.startsWith("data: ")) continue;
            const r = JSON.parse(l.slice(6));
            lastSeq = r.seq;
            apply(r);
          }
        }
      }
      throw new Error("the stream ended");
    } catch (e) {
      if (ctl.signal.aborted) return;
      failures++;
      setConn("off", "disconnected: " + e.message);
      if (failures === 1) line("warn", "! the connection to the server was lost; reconnecting…");
      await new Promise((res) => setTimeout(res, Math.min(failures, 5) * 1000));
    }
  }
}

// ----------------------------------------------------------------- input

async function send(text, literal) {
  try {
    await api("POST", "/api/input", { text, literal });
  } catch (e) {
    if (e instanceof Unauthorized) unauthorized();
    else line("err", "✗ not sent: " + e.message);
  }
}

const input = $("input");
// grow fits the input to its text (plus the border: the box is border-box).
function grow() {
  input.style.height = "auto";
  const border = input.offsetHeight - input.clientHeight;
  input.style.height = Math.min(input.scrollHeight + border, window.innerHeight * 0.4) + "px";
}

function insertText(t) {
  const start = input.selectionStart ?? input.value.length, end = input.selectionEnd ?? start;
  const before = input.value.slice(0, start), after = input.value.slice(end);
  const pad = before && !/\s$/.test(before) ? " " : "";
  input.value = before + pad + t + (after && !/^\s/.test(after) ? " " : "") + after;
  const pos = (before + pad + t).length;
  input.setSelectionRange(pos, pos);
  grow();
  input.focus();
}

// history: ↑ at the start and ↓ at the end of the input walk through what was sent
let history = [];
try { history = JSON.parse(sessionStorage.getItem("history") || "[]"); } catch (e) { }
let histPos = history.length, histDraft = "";
function remember(text) {
  if (history[history.length - 1] !== text) history.push(text);
  if (history.length > 100) history = history.slice(-100);
  try { sessionStorage.setItem("history", JSON.stringify(history)); } catch (e) { }
  histPos = history.length;
}

// command suggestions for / and ! at the start of the input
const commands = [
  ["/help", "all commands"], ["/context", "context usage"], ["/compact", "compact now, [percent]"], ["/undo", "drop the last exchange"],
  ["/retry", "resend your last message"], ["/image", "<path> [question]: ask about an image"], ["/sessions", "list sessions"],
  ["/new", "start a new session"], ["/load", "<id>: switch session"], ["/clear", "discard this conversation"],
  ["/model", "[name]: list or switch models"], ["/adapt", "[name]: fit the context to the GPU"], ["/system-prompt", "[reload]"],
  ["/gpu", "GPU guard"], ["/approvals", "[reset]"], ["/safe", "[restore <n>]"], ["/rogue", "[on [steps]|off]"],
  ["/fetch", "[on|off|allow <host>|forget <host>]"], ["/plan", "[approve|reject …]"],
  ["!c", "cancel the current work"], ["!think", "[on|off]"], ["!show", "[on|off]"], ["!safe", "[on|off]"], ["!plan", "[on|off]"], ["!skip", "skip a GPU pause"],
];
let suggestSel = -1;
function suggestions() {
  const v = input.value;
  if (!/^[\/!][\w-]*$/.test(v)) return [];
  return commands.filter(([c]) => c.startsWith(v) && c !== v);
}
function drawSuggest() {
  const box = $("suggest");
  const list = suggestions();
  box.replaceChildren();
  box.hidden = list.length === 0;
  if (suggestSel >= list.length) suggestSel = list.length - 1;
  list.forEach(([c, hint], i) => {
    const b = button(c, "item" + (i === suggestSel ? " sel" : ""), () => { input.value = c + " "; drawSuggest(); input.focus(); });
    b.appendChild(el("span", "sub", hint));
    box.appendChild(b);
  });
}

input.addEventListener("input", () => { grow(); suggestSel = -1; drawSuggest(); });
input.addEventListener("keydown", (e) => {
  const list = suggestions();
  if (list.length && (e.key === "Tab" || (e.key === "Enter" && suggestSel >= 0))) {
    e.preventDefault();
    input.value = list[Math.max(0, suggestSel)][0] + " ";
    suggestSel = -1;
    drawSuggest();
    return;
  }
  if (list.length && (e.key === "ArrowDown" || e.key === "ArrowUp")) {
    e.preventDefault();
    suggestSel = (suggestSel + (e.key === "ArrowDown" ? 1 : list.length - 1)) % list.length;
    drawSuggest();
    return;
  }
  if (e.key === "Escape") { $("suggest").hidden = true; return; }
  if (e.key === "ArrowUp" && input.selectionStart === 0 && input.selectionEnd === 0 && histPos > 0) {
    e.preventDefault();
    if (histPos === history.length) histDraft = input.value;
    input.value = history[--histPos];
    grow();
    return;
  }
  if (e.key === "ArrowDown" && input.selectionStart === input.value.length && histPos < history.length) {
    e.preventDefault();
    histPos++;
    input.value = histPos === history.length ? histDraft : history[histPos];
    grow();
    return;
  }
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); $("send-form").requestSubmit(); }
});
$("send-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const text = input.value;
  if (text.trim() === "") return;
  input.value = "";
  grow();
  $("suggest").hidden = true;
  remember(text);
  // One line is read like a typed line (commands work); several lines are a
  // message, like a paste in the terminal.
  send(text, text.includes("\n"));
});
$("stop").addEventListener("click", () => send("!c", false));
for (const b of document.querySelectorAll("[data-send]")) b.addEventListener("click", () => send(b.dataset.send, false));

// ------------------------------------------------------------- popovers

function closePopovers(except) {
  for (const id of ["sessions-menu", "models-menu", "suggest"]) if (id !== except) $(id).hidden = true;
}
document.addEventListener("click", (e) => {
  if (!e.target.closest(".popover, .menu-btn, #input")) closePopovers();
});
document.addEventListener("keydown", (e) => { if (e.key === "Escape") closePopovers(); });

function placeUnder(menu, btn) {
  const r = btn.getBoundingClientRect();
  menu.style.left = Math.max(8, Math.min(r.left, window.innerWidth - 300)) + "px";
}

$("sessions-btn").addEventListener("click", async (e) => {
  const menu = $("sessions-menu");
  if (!menu.hidden) { menu.hidden = true; return; }
  closePopovers("sessions-menu");
  placeUnder(menu, e.currentTarget);
  menu.replaceChildren(el("div", "sect", "loading…"));
  menu.hidden = false;
  try {
    const d = await apiJSON("/api/sessions");
    menu.replaceChildren(el("div", "sect", "Sessions"));
    for (const s of d.sessions.slice(0, 50)) {
      const b = button(s.Title || "(untitled)", "item" + (s.ID === d.current ? " current" : ""), () => {
        menu.hidden = true;
        if (s.ID !== d.current) send("/load " + s.ID, false);
      });
      b.appendChild(el("span", "sub", s.ID + " · " + new Date(s.Updated).toLocaleString() + " · " + s.Messages + " entries"));
      menu.appendChild(b);
    }
    const row = menu.appendChild(el("div", "row"));
    row.appendChild(button("New session", "small primary", () => { menu.hidden = true; send("/new", false); }));
    row.appendChild(button("Clear this one…", "small danger", () => { menu.hidden = true; send("/clear", false); }, "asks first"));
  } catch (err) {
    if (err instanceof Unauthorized) return unauthorized();
    menu.replaceChildren(el("div", "sect", "could not list sessions: " + err.message));
  }
});

$("models-btn").addEventListener("click", async (e) => {
  const menu = $("models-menu");
  if (!menu.hidden) { menu.hidden = true; return; }
  closePopovers("models-menu");
  placeUnder(menu, e.currentTarget);
  menu.replaceChildren(el("div", "sect", "asking the model servers…"));
  menu.hidden = false;
  try {
    const d = await apiJSON("/api/models");
    menu.replaceChildren();
    const sect = (name, items) => {
      if (!items.length) return;
      menu.appendChild(el("div", "sect", name));
      for (const m of items) {
        const b = button(m.name, "item" + (m.active ? " current" : ""), () => { menu.hidden = true; if (!m.active) send("/model " + m.name, false); });
        if (m.cloud) b.appendChild(el("span", "badge cloud", m.cloud));
        if (m.loaded) b.appendChild(el("span", "badge loaded", "loaded"));
        const caps = !m.known ? ["caps unknown"] : !m.tools ? ["chat only"] : ["tools"].concat(m.thinking ? ["thinking"] : [], m.vision ? ["vision"] : []);
        const sub = (m.profile && m.model !== m.name ? m.model + " · " : "") + caps.join(", ") + (m.context ? " · native " + ktok(m.context) : "");
        b.appendChild(el("span", "sub", sub));
        menu.appendChild(b);
      }
    };
    sect("Profiles", d.models.filter((m) => m.profile));
    sect("Installed, no profile", d.models.filter((m) => !m.profile));
    if (d.error) menu.appendChild(el("div", "sect", "installed models could not be listed: " + d.error));
    menu.appendChild(el("div", "sect", "switching waits for the current work to end; a cloud model asks first"));
  } catch (err) {
    if (err instanceof Unauthorized) return unauthorized();
    menu.replaceChildren(el("div", "sect", "could not list models: " + err.message));
  }
});

// ------------------------------------------------------------ side panel

let tab = "todo";
function openPanel(name) {
  $("panel").hidden = false;
  $("panel-btn").classList.add("on");
  if (name) selectTab(name);
  else refreshPanel(true);
}
function selectTab(name) {
  tab = name;
  for (const t of document.querySelectorAll("#tabs .tab")) t.classList.toggle("active", t.dataset.tab === name);
  for (const n of ["todo", "plan", "files"]) $("tab-" + n).hidden = n !== name;
  refreshPanel(true);
}
$("panel-btn").addEventListener("click", () => {
  if ($("panel").hidden) openPanel();
  else { $("panel").hidden = true; $("panel-btn").classList.remove("on"); }
});
$("panel-close").addEventListener("click", () => { $("panel").hidden = true; $("panel-btn").classList.remove("on"); });
for (const b of document.querySelectorAll("#tabs .tab")) b.addEventListener("click", () => selectTab(b.dataset.tab));

let panelTimer = null;
// refreshPanel updates the open tab (debounced: status changes come in bursts).
function refreshPanel(now) {
  clearTimeout(panelTimer);
  panelTimer = setTimeout(() => {
    if (tab === "todo") loadTodos();
    else if (tab === "plan") loadPlan();
    else if (now) loadFiles(cwd);
  }, now ? 0 : 400);
}

async function loadTodos() {
  const box = $("tab-todo");
  try {
    const d = await apiJSON("/api/todos");
    box.replaceChildren();
    if (!d.todos.length) { box.appendChild(el("p", "dim", "The agent has no task plan in this session. It writes one (todo_write) for longer tasks.")); return; }
    const ul = box.appendChild(el("ul", "todo"));
    for (const t of d.todos) {
      const li = ul.appendChild(el("li", t.status));
      li.appendChild(el("span", "", t.status === "done" ? "☑" : t.status === "in_progress" ? "◐" : "☐"));
      li.appendChild(el("span", "", t.text));
    }
  } catch (err) {
    if (err instanceof Unauthorized) return unauthorized();
    box.replaceChildren(el("p", "error", err.message));
  }
}

async function loadPlan() {
  const box = $("tab-plan");
  try {
    const d = await apiJSON("/api/plan");
    box.replaceChildren();
    box.appendChild(el("p", "dim", "Plan mode is " + (d.plan ? "on: write, exec and destructive actions are captured here instead of running." : "off. Turn it on with the Plan toggle.")));
    if (!d.pending.length) { box.appendChild(el("p", "dim", "No pending actions.")); return; }
    const all = box.appendChild(el("div", "plan-all"));
    all.appendChild(button("Approve all", "small primary", () => send("/plan approve all", false)));
    all.appendChild(button("Reject all", "small danger", () => send("/plan reject all", false)));
    for (const p of d.pending) {
      const c = box.appendChild(el("div", "pending"));
      c.appendChild(el("div", "what", p.n + ". " + p.tool + ": " + p.summary));
      const acts = c.appendChild(el("div", "actions"));
      acts.appendChild(button("Approve", "small primary", () => send("/plan approve " + p.n, false)));
      acts.appendChild(button("Reject", "small danger", () => send("/plan reject " + p.n, false)));
    }
  } catch (err) {
    if (err instanceof Unauthorized) return unauthorized();
    box.replaceChildren(el("p", "error", err.message));
  }
}

// ------------------------------------------------------------------ files

let cwd = ".";
let viewing = null;  // the path in the viewer
let objectURL = null;

function joinPath(dir, name) { return dir === "." || dir === "" ? name : dir.replace(/\/$/, "") + "/" + name; }

async function loadFiles(dir) {
  const list = $("file-list");
  try {
    const d = await apiJSON("/api/files?path=" + encodeURIComponent(dir));
    cwd = d.path || ".";
    const crumbs = $("crumbs");
    crumbs.replaceChildren();
    const parts = cwd === "." ? [] : cwd.split("/");
    const root = crumbs.appendChild(el("a", "", "workspace"));
    root.addEventListener("click", () => loadFiles("."));
    parts.forEach((p, i) => {
      crumbs.appendChild(document.createTextNode(" / "));
      const a = crumbs.appendChild(el("a", "", p));
      a.addEventListener("click", () => loadFiles(parts.slice(0, i + 1).join("/")));
    });
    list.replaceChildren();
    if (cwd !== ".") {
      const up = list.appendChild(el("li"));
      up.appendChild(el("span", "fname", "↑ .."));
      up.addEventListener("click", () => loadFiles(parts.slice(0, -1).join("/") || "."));
    }
    for (const f of d.entries) {
      const li = list.appendChild(el("li"));
      li.appendChild(el("span", "", f.dir ? "📁" : "📄"));
      li.appendChild(el("span", "fname", f.name + (f.link ? " →" : "")));
      if (!f.dir) li.appendChild(el("span", "fsize", humanBytes(f.size)));
      li.title = new Date(f.modified).toLocaleString();
      li.addEventListener("click", () => f.dir ? loadFiles(joinPath(cwd, f.name)) : viewFile(joinPath(cwd, f.name)));
    }
    if (!d.entries.length) list.appendChild(el("li", "dim", "(empty)"));
  } catch (err) {
    if (err instanceof Unauthorized) return unauthorized();
    list.replaceChildren(el("li", "error", err.message));
  }
}

function showViewer(name, node) {
  if (objectURL) { URL.revokeObjectURL(objectURL); objectURL = null; }
  $("viewer").hidden = false;
  $("viewer-name").textContent = name;
  $("viewer-body").replaceChildren(node);
}

async function viewFile(path) {
  viewing = path;
  $("viewer-diff").hidden = false;
  $("viewer-mention").hidden = false;
  showViewer(path, el("p", "dim", "loading…"));
  try {
    const resp = await api("GET", "/api/file?path=" + encodeURIComponent(path));
    const type = resp.headers.get("Content-Type") || "";
    if (type.startsWith("image/")) {
      const blob = await resp.blob();
      const img = el("img");
      img.alt = path;
      showViewer(path, img);
      objectURL = URL.createObjectURL(new Blob([blob], { type }));
      img.src = objectURL;
      return;
    }
    const text = await resp.text();
    const pre = el("pre");
    text.replace(/\n$/, "").split("\n").forEach((l, i) => {
      pre.appendChild(el("span", "ln", String(i + 1)));
      pre.appendChild(document.createTextNode(l + "\n"));
    });
    showViewer(path, pre);
  } catch (err) {
    if (err instanceof Unauthorized) return unauthorized();
    showViewer(path, el("p", "error", err.message));
  }
}

async function showDiff(path, stat) {
  const title = stat ? "changes in the workspace (git diff --stat)" : "git diff " + path;
  $("viewer-diff").hidden = !!stat;
  $("viewer-mention").hidden = !!stat;
  showViewer(title, el("p", "dim", "loading…"));
  try {
    const q = stat ? "stat=1" : "path=" + encodeURIComponent(path);
    const text = await (await api("GET", "/api/diff?" + q)).text();
    showViewer(title, text.trim() ? diffPre(text) : el("p", "dim", "no changes"));
  } catch (err) {
    if (err instanceof Unauthorized) return unauthorized();
    showViewer(title, el("p", "error", err.message));
  }
}

$("viewer-diff").addEventListener("click", () => viewing && showDiff(viewing, false));
$("viewer-mention").addEventListener("click", () => viewing && insertText(viewing));
$("viewer-close").addEventListener("click", () => { $("viewer").hidden = true; if (objectURL) URL.revokeObjectURL(objectURL); objectURL = null; viewing = null; });
$("changes-btn").addEventListener("click", () => showDiff("", true));
$("refresh-files").addEventListener("click", () => loadFiles(cwd));

// ----------------------------------------------------------------- upload

// Uploads go into the workspace (uploads/), separately from the
// conversation: the agent opens them with its own tools when asked to.
async function uploadFiles(files) {
  for (const f of files) {
    const row = line("info upload-line", "· uploading " + f.name + " (" + humanBytes(f.size) + ")…");
    try {
      const d = await (await api("POST", "/api/upload?name=" + encodeURIComponent(f.name), f, { "Content-Type": "application/octet-stream" })).json();
      row.textContent = "· uploaded " + d.path + " (" + (d.image ? d.mime.replace("image/", "").toUpperCase() + ", " + d.width + "×" + d.height + ", " : "") + humanBytes(d.size) + ")";
      const acts = row.appendChild(el("span", "acts"));
      acts.appendChild(button("Mention", "", () => insertText(d.path), "put the path into your message"));
      if (d.image) {
        acts.appendChild(button("Ask about it", "", () => { input.value = "/image " + d.path + " "; grow(); input.focus(); },
          "send the image with your question (needs a model with vision)"));
      }
      acts.appendChild(button("View", "", () => { openPanel("files"); viewFile(d.path); }));
    } catch (err) {
      if (err instanceof Unauthorized) return unauthorized();
      row.className = "line err";
      row.textContent = "✗ upload of " + f.name + " failed: " + err.message;
    }
  }
}

$("upload-btn").addEventListener("click", () => $("upload-input").click());
$("upload-input").addEventListener("change", (e) => { uploadFiles([...e.target.files]); e.target.value = ""; });
input.addEventListener("paste", (e) => {
  const files = [...(e.clipboardData?.files || [])];
  if (files.length) { e.preventDefault(); uploadFiles(files); }
});
let dragDepth = 0;
const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes("Files");
document.addEventListener("dragenter", (e) => { if (!hasFiles(e) || $("app").hidden) return; dragDepth++; $("drop").hidden = false; });
document.addEventListener("dragleave", () => { if (--dragDepth <= 0) { dragDepth = 0; $("drop").hidden = true; } });
document.addEventListener("dragover", (e) => { if (hasFiles(e)) e.preventDefault(); });
document.addEventListener("drop", (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  $("drop").hidden = true;
  if (!$("app").hidden) uploadFiles([...e.dataTransfer.files]);
});

// ----------------------------------------------------------------- start

async function start() {
  if (!token) { showLogin(""); return; }
  try {
    status = await apiJSON("/api/state");
  } catch (e) {
    if (e instanceof Unauthorized) { unauthorized(); return; }
    showLogin("Could not reach the server: " + e.message);
    return;
  }
  $("login").hidden = true;
  $("app").hidden = false;
  wasBusy = status.busy;
  drawStatus();
  input.focus();
  stream();
}

start();
