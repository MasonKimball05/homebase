// The Local AI page: is Ollama up, which models are there, and how to
// connect to it from Loom or anything else. Read-only: it only calls the
// same GET endpoints the chat page uses.

const el = (id) => document.getElementById(id);
const make = (tag, text, cls) => {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (cls) e.className = cls;
  return e;
};

// The address to give other machines: the one you reached this page at
// (arkans-pc1:8090 over Tailscale). Opened on the PC itself as localhost,
// that address would mean "this machine" everywhere else, so the PC's name
// (from /api/status) is used instead.
const LOCAL = ["localhost", "127.0.0.1", "[::1]"];
let base = `${location.origin}/ollama`;
function useHostName(name) {
  if (!name || !LOCAL.includes(location.hostname)) return;
  base = `${location.protocol}//${name.toLowerCase()}${location.port ? `:${location.port}` : ""}/ollama`;
}

/** 18556948123 → "18.6 GB" */
function formatSize(bytes) {
  if (!bytes) return "";
  const gb = bytes / 1e9;
  return gb >= 1 ? `${gb.toFixed(1)} GB` : `${Math.round(bytes / 1e6)} MB`;
}

function showError(text) {
  el("error").textContent = text;
  el("error").hidden = false;
}

function fillConnectInfo(defaultModel) {
  el("host-url").textContent = base;
  document.querySelectorAll(".base").forEach((s) => (s.textContent = base));
  const model = defaultModel || "qwen3-coder:30b";
  el("curl-example").textContent =
    `curl ${base}/api/chat -H "X-Homebase: 1" -d '{"model": "${model}", "stream": false, ` +
    `"messages": [{"role": "user", "content": "Say hello in five words."}]}'`;
  if (defaultModel) el("loom-model-hint").textContent = `${defaultModel} is the default here.`;
}

async function load() {
  let info = { enabled: false };
  try {
    info = await (await fetch("api/ai")).json();
  } catch {
    el("ollama-dot").dataset.state = "down";
    el("ollama-status").textContent = "Can't reach homebase. Is it still running?";
    return;
  }
  const status = await fetch("api/status").then((r) => r.json()).catch(() => null);
  useHostName(status?.host);
  fillConnectInfo(info.default_model);
  if (!info.enabled) {
    el("ollama-dot").dataset.state = "down";
    el("ollama-status").textContent = "Local AI isn't set up: add an \"ai\" section to homebase.json (see the README) and restart homebase.";
    return;
  }

  const [version, tags] = await Promise.all([
    fetch("ollama/api/version").then((r) => r.json().then((d) => ({ ok: r.ok, d }))).catch(() => null),
    fetch("ollama/api/tags").then((r) => r.json().then((d) => ({ ok: r.ok, d }))).catch(() => null),
  ]);
  const where = status?.host ? ` on ${status.host}` : "";
  if (!version?.ok) {
    el("ollama-dot").dataset.state = "down";
    el("ollama-status").textContent = `Ollama isn't answering${where}.`;
    showError(version?.d?.error || "Start Ollama on the PC (the tray app, or `ollama serve`), then reload this page.");
    return;
  }
  el("ollama-dot").dataset.state = "running";
  el("ollama-status").textContent = `Ollama ${version.d.version} is running${where}.`;

  const models = (tags?.ok && tags.d.models) || [];
  models.sort((a, b) => a.name.localeCompare(b.name));
  el("models-note").textContent = models.length ? `${models.length} on this PC` : "";
  const rows = models.map((m) => {
    const tr = make("tr");
    const name = make("td", m.name);
    const isDefault = info.default_model && (m.name === info.default_model || m.name === `${info.default_model}:latest`);
    if (isDefault) name.append(make("span", "default", "badge"));
    const chat = make("a", "Chat", "button");
    chat.href = `chat.html?model=${encodeURIComponent(m.name)}`;
    const action = make("td");
    action.append(chat);
    tr.append(
      name,
      make("td", formatSize(m.size), "num"),
      make("td", m.details?.parameter_size || "", "num"),
      make("td", m.details?.quantization_level || ""),
      action,
    );
    return tr;
  });
  if (!rows.length) {
    const tr = make("tr");
    const td = make("td", "No models yet. On the PC: ollama pull qwen3-coder:30b");
    td.colSpan = 5;
    tr.append(td);
    rows.push(tr);
  }
  el("model-rows").replaceChildren(...rows);
}

// Copy buttons: data-copy names the element whose text to copy.
document.addEventListener("click", async (e) => {
  const button = e.target.closest("button[data-copy]");
  if (!button) return;
  const text = el(button.dataset.copy).textContent;
  try {
    await navigator.clipboard.writeText(text);
    button.textContent = "Copied";
  } catch {
    // Clipboard access needs https or localhost; over plain http, select it instead.
    const range = document.createRange();
    range.selectNodeContents(el(button.dataset.copy));
    getSelection().removeAllRanges();
    getSelection().addRange(range);
    button.textContent = "Selected: copy it now";
  }
  setTimeout(() => (button.textContent = "Copy"), 2000);
});

load();
