// The chat page: talks to Ollama on this PC through homebase's /ollama/ proxy
// (internal/ai). Answers stream in as they're generated; Stop cancels the
// request, which stops the model. The conversation lives in this browser's
// localStorage only.
//
// Model output is rendered with textContent only, never innerHTML (see the
// small Markdown renderer at the bottom).

const el = (id) => document.getElementById(id);
const STORE = "homebase.chat.v1";

let messages = [];      // [{role, content}] in Ollama's format
let controller = null;  // AbortController for the answer in progress

// ---------------- startup ----------------

init();

async function init() {
  restore();
  el("composer").addEventListener("submit", (e) => {
    e.preventDefault();
    controller ? stop() : send();
  });
  el("input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      controller ? null : send();
    }
  });
  el("new").addEventListener("click", () => {
    stop();
    messages = [];
    save();
    renderAll();
    el("input").focus();
  });
  el("model").addEventListener("change", () => {
    try { localStorage.setItem(STORE + ".model", el("model").value); } catch {}
  });
  await loadModels();
  el("input").focus();
}

async function loadModels() {
  try {
    const info = await (await fetch("api/ai")).json();
    if (!info.enabled) return showError("Chat isn't set up: add an \"ai\" section to homebase.json (see the README) and restart homebase.");
    const res = await fetch("ollama/api/tags");
    const data = await res.json();
    if (!res.ok) return showError(data.error || `Ollama answered ${res.status}`);
    const names = (data.models || []).map((m) => m.name).sort();
    if (!names.length) return showError("Ollama has no models yet. On the PC: ollama pull qwen3-coder:30b");
    let saved = null;
    try { saved = localStorage.getItem(STORE + ".model"); } catch {}
    const pick = [saved, info.default_model, info.default_model && `${info.default_model}:latest`].find((n) => n && names.includes(n)) || names[0];
    const sel = el("model");
    sel.replaceChildren(...names.map((n) => new Option(n, n, false, n === pick)));
    el("error").hidden = true;
  } catch {
    showError("Can't reach homebase. Is it still running?");
  }
}

function showError(text) {
  el("error").textContent = text;
  el("error").hidden = false;
}

// ---------------- conversation ----------------

async function send() {
  const text = el("input").value.trim();
  if (!text || controller) return;
  const model = el("model").value;
  if (!model) return showError("Pick a model first.");
  el("input").value = "";
  messages.push({ role: "user", content: text });
  save();
  renderAll();

  const reply = { role: "assistant", content: "" };
  const view = addAssistant();
  controller = new AbortController();
  setBusy(true);
  let thinking = "";
  let stats = "";
  try {
    const res = await fetch("ollama/api/chat", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Homebase": "1" },
      body: JSON.stringify({ model, messages, stream: true }),
      signal: controller.signal,
    });
    if (!res.ok) {
      const err = await res.json().catch(() => ({}));
      throw new Error(err.error || `HTTP ${res.status}`);
    }
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    let frame = 0;
    const draw = () => {
      frame = 0;
      view.update(reply.content, thinking, false);
    };
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        const chunk = JSON.parse(line);
        if (chunk.error) throw new Error(chunk.error);
        if (chunk.message?.content) reply.content += chunk.message.content;
        if (chunk.message?.thinking) thinking += chunk.message.thinking;
        if (chunk.done && chunk.eval_count && chunk.eval_duration) {
          stats = `${model} · ${chunk.eval_count} tokens · ${(chunk.eval_count / (chunk.eval_duration / 1e9)).toFixed(1)} tok/s`;
        }
      }
      frame ||= requestAnimationFrame(draw);
    }
  } catch (e) {
    if (e.name !== "AbortError") view.error(e.message || String(e));
  } finally {
    controller = null;
    setBusy(false);
    if (reply.content) {
      messages.push(reply);
      save();
    }
    view.update(reply.content, thinking, true, stats);
    el("input").focus();
  }
}

function stop() {
  controller?.abort();
}

function setBusy(busy) {
  const b = el("send");
  b.textContent = busy ? "Stop" : "Send";
  b.classList.toggle("stop", busy);
  el("status").textContent = busy ? "Generating…" : "";
}

function save() {
  try { localStorage.setItem(STORE, JSON.stringify(messages.slice(-200))); } catch {}
}

function restore() {
  try {
    const m = JSON.parse(localStorage.getItem(STORE) || "[]");
    if (Array.isArray(m)) messages = m.filter((x) => x && typeof x.content === "string" && (x.role === "user" || x.role === "assistant"));
  } catch {}
  renderAll();
}

// ---------------- rendering ----------------

function renderAll() {
  const t = el("transcript");
  t.replaceChildren();
  if (!messages.length) {
    t.append(emptyNote());
    return;
  }
  for (const m of messages) {
    if (m.role === "user") t.append(userBubble(m.content));
    else {
      const v = addAssistant(false);
      v.update(m.content, "", true);
    }
  }
  scrollDown(true);
}

function emptyNote() {
  const d = document.createElement("div");
  d.className = "empty muted";
  const p1 = document.createElement("p");
  p1.textContent = "Chat with the models Ollama runs on this PC's GPU. Nothing leaves your machines.";
  const p2 = document.createElement("p");
  p2.className = "small";
  p2.textContent = "Conversations are kept in this browser only. To have the model read and edit your code, use Loom's Local AI view with this PC as its server.";
  d.append(p1, p2);
  return d;
}

function userBubble(text) {
  const d = document.createElement("div");
  d.className = "msg-user";
  d.textContent = text;
  return d;
}

function addAssistant(append = true) {
  const t = el("transcript");
  t.querySelector(".empty")?.remove();
  const box = document.createElement("div");
  box.className = "msg-assistant";
  const think = document.createElement("details");
  think.className = "msg-thinking";
  think.hidden = true;
  const sum = document.createElement("summary");
  sum.textContent = "Thinking";
  const thinkBody = document.createElement("div");
  think.append(sum, thinkBody);
  const body = document.createElement("div");
  const pending = document.createElement("div");
  pending.className = "msg-pending";
  pending.textContent = "Thinking… (the first answer after a while can take a minute while the model loads)";
  body.append(pending);
  const foot = document.createElement("div");
  foot.className = "msg-stats";
  box.append(think, body, foot);
  t.append(box);
  if (append) scrollDown(true);
  return {
    update(text, thinking, done, stats = "") {
      const stick = nearBottom();
      think.hidden = !thinking;
      thinkBody.textContent = thinking;
      if (text) body.replaceChildren(renderMarkdown(text));
      else if (done) body.replaceChildren();
      foot.textContent = stats;
      scrollDown(stick);
    },
    error(msg) {
      const e = document.createElement("div");
      e.className = "msg-error";
      e.textContent = msg;
      body.replaceChildren(e);
    },
  };
}

const nearBottom = () => innerHeight + scrollY >= document.body.scrollHeight - 80;
function scrollDown(really) {
  if (really) scrollTo({ top: document.body.scrollHeight });
}

// ---------------- Markdown, without innerHTML ----------------
// Headings, paragraphs, lists, fenced code (with a Copy button), `code`,
// **bold** and *italic*. Re-run on every streamed chunk, so an unclosed code
// fence renders as code so far.

function renderMarkdown(md) {
  const root = document.createElement("div");
  const lines = md.replace(/\r\n?/g, "\n").split("\n");
  let para = [];
  let list = null;
  const flush = () => {
    if (para.length) {
      const p = document.createElement("p");
      inline(p, para.join("\n"));
      root.append(p);
    }
    para = [];
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    const fence = /^\s*(`{3,}|~{3,})/.exec(line);
    if (fence) {
      flush();
      list = null;
      const code = [];
      for (i++; i < lines.length && !lines[i].trim().startsWith(fence[1]); i++) code.push(lines[i]);
      root.append(codeBlock(code.join("\n")));
      continue;
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) {
      flush();
      list = null;
      const el = document.createElement("h4");
      inline(el, h[2]);
      root.append(el);
      continue;
    }
    const item = /^\s*([-*+]|\d+[.)])\s+(.*)$/.exec(line);
    if (item) {
      flush();
      const ordered = /\d/.test(item[1]);
      if (!list || list.ordered !== ordered) {
        list = { ordered, el: document.createElement(ordered ? "ol" : "ul") };
        root.append(list.el);
      }
      const li = document.createElement("li");
      inline(li, item[2]);
      list.el.append(li);
      continue;
    }
    if (!line.trim()) {
      flush();
      list = null;
    } else {
      list = null;
      para.push(line);
    }
  }
  flush();
  return root;
}

function codeBlock(text) {
  const pre = document.createElement("pre");
  pre.textContent = text;
  const copy = document.createElement("button");
  copy.type = "button";
  copy.className = "copy";
  copy.textContent = "Copy";
  copy.onclick = () => navigator.clipboard?.writeText(text).then(() => (copy.textContent = "Copied"), () => {});
  pre.append(copy);
  return pre;
}

function inline(parent, s) {
  const re = /(`+)([\s\S]*?)\1|\*\*([^*]+)\*\*|\*([^*\s][^*]*)\*/g;
  let last = 0;
  for (const m of s.matchAll(re)) {
    if (m.index > last) parent.append(s.slice(last, m.index));
    const node = document.createElement(m[1] ? "code" : m[3] ? "strong" : "em");
    node.textContent = m[1] ? m[2].trim() : m[3] ?? m[4];
    parent.append(node);
    last = m.index + m[0].length;
  }
  if (last < s.length) parent.append(s.slice(last));
}
