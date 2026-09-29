const el = (id) => document.getElementById(id);
const POLL_MS = 3000;
const LOG_POLL_MS = 1500;

const LABELS = {
  running: "Running", starting: "Starting", stopping: "Stopping", stopped: "Stopped",
  backoff: "Restarting", crashed: "Crashed", external: "External",
  unhealthy: "Unhealthy", recovering: "Recovering",
};

let logApp = null;    // app whose logs are open
let logAfter = 0;     // last log seq received
let logTimer = null;
const pending = new Set(); // apps with an action in flight

async function refresh() {
  try {
    const res = await fetch("api/status");
    const data = await res.json();
    el("error").hidden = true;
    render(data);
  } catch {
    el("error").textContent = "Can't reach homebase. Is it still running?";
    el("error").hidden = false;
    el("overall").dataset.state = "crashed";
  }
}

// Refine the server's state for display: a running app whose health check
// fails is "unhealthy", and one that came back from a crash less than a
// minute ago is "recovering" (so a crash loop doesn't look green).
function visualState(app) {
  if (app.state === "running" && app.health && !app.health.ok) return "unhealthy";
  if (app.state === "running" && app.last_exit && !app.last_exit.requested && app.last_exit.code !== 0 && app.started_at &&
      Date.now() - Date.parse(app.started_at) < 60_000) return "recovering";
  return app.state;
}

function render(data) {
  el("host").textContent = data.host ? `on ${data.host}` : "";
  const states = data.apps.map(visualState);
  const up = states.filter((s) => s === "running").length;
  const problems = states.filter((s) => ["crashed", "backoff", "unhealthy", "recovering"].includes(s)).length;
  el("summary").textContent =
    `${up}/${data.apps.length} running` + (problems ? ` · ${problems} need attention` : "") +
    ` · homebase up ${duration(data.uptime_seconds)}`;
  el("overall").dataset.state = problems ? "crashed" : up === data.apps.length ? "running" : "backoff";
  document.title = problems ? `(${problems}) homebase` : "homebase";

  const grid = el("apps");
  if (data.apps.length === 0) {
    grid.innerHTML = "";
    const p = document.createElement("p");
    p.className = "muted";
    p.textContent = "No apps configured yet. Add one to homebase.json and restart homebase.";
    grid.append(p);
    return;
  }
  grid.replaceChildren(...data.apps.map(card));
}

function card(app) {
  const node = el("card").content.firstElementChild.cloneNode(true);
  const state = visualState(app);
  node.querySelector(".dot").dataset.state = state;
  node.querySelector("h2").textContent = app.title;
  node.querySelector(".state").textContent = LABELS[state] ?? state;

  const link = node.querySelector(".link");
  if (app.url) {
    link.href = app.url;
    link.textContent = app.url.replace(/^https?:\/\//, "").replace(/\/$/, "") + " ↗";
  } else {
    link.remove();
  }

  const msg = node.querySelector(".message");
  const healthMsg = state === "unhealthy" ? `Health check failing: ${app.health.error || "no response"}` : "";
  if (app.message || healthMsg) {
    msg.textContent = app.message || healthMsg;
    msg.hidden = false;
  }

  const facts = [
    ["Uptime", app.started_at ? duration((Date.now() - Date.parse(app.started_at)) / 1000) : "–"],
    ["Response", app.health ? (app.health.ok ? `${app.health.latency_ms} ms` : "down") : "–", app.health && !app.health.ok],
    ["Memory", app.memory_bytes ? bytes(app.memory_bytes) : "–"],
    ["Restarts", String(app.restarts)],
    ["PID", app.pid ? String(app.pid) : "–"],
    ["Last exit", lastExit(app.last_exit), app.last_exit && !app.last_exit.requested && app.last_exit.code !== 0],
  ];
  const dl = node.querySelector(".facts");
  for (const [label, value, bad] of facts) {
    const div = document.createElement("div");
    const dt = document.createElement("dt");
    const dd = document.createElement("dd");
    dt.textContent = label;
    dd.textContent = value;
    if (bad) dd.className = "bad";
    div.append(dt, dd);
    dl.append(div);
  }

  const busy = pending.has(app.name) || ["starting", "stopping"].includes(app.state);
  const owned = app.pid > 0 || ["backoff", "starting"].includes(app.state);
  const buttons = {
    start: !busy && !owned,
    restart: !busy && app.state !== "external",
    stop: !busy && (owned || app.state === "backoff"),
  };
  for (const btn of node.querySelectorAll("[data-action]")) {
    const action = btn.dataset.action;
    btn.disabled = !buttons[action];
    btn.addEventListener("click", () => act(app, action));
  }
  node.querySelector("[data-logs]").addEventListener("click", () => openLogs(app));
  return node;
}

async function act(app, action) {
  if (action === "stop" && !confirm(`Stop ${app.title}?`)) return;
  pending.add(app.name);
  try {
    // The custom header proves the request came from this page (see server.go).
    const res = await fetch(`api/apps/${encodeURIComponent(app.name)}/${action}`, {
      method: "POST",
      headers: { "X-Homebase": "1" },
    });
    if (!res.ok) {
      const body = await res.json().catch(() => ({}));
      alert(body.error || `Couldn't ${action} ${app.title}.`);
    }
  } finally {
    pending.delete(app.name);
    setTimeout(refresh, 400);
  }
}

// ---------- logs ----------

function openLogs(app) {
  logApp = app.name;
  logAfter = 0;
  el("logs-title").textContent = `Logs · ${app.title}`;
  el("logs-body").replaceChildren();
  el("logs").hidden = false;
  el("logs").scrollIntoView({ behavior: "smooth", block: "nearest" });
  clearInterval(logTimer);
  pollLogs();
  logTimer = setInterval(pollLogs, LOG_POLL_MS);
}

function closeLogs() {
  clearInterval(logTimer);
  logApp = null;
  el("logs").hidden = true;
}

async function pollLogs() {
  if (!logApp) return;
  const name = logApp;
  const res = await fetch(`api/apps/${encodeURIComponent(name)}/logs?after=${logAfter}`);
  if (!res.ok || name !== logApp) return;
  const lines = await res.json();
  if (lines.length === 0) return;

  const pre = el("logs-body");
  const frag = document.createDocumentFragment();
  for (const ln of lines) {
    // textContent only: app output must never be interpreted as HTML.
    const row = document.createElement("div");
    const ts = document.createElement("span");
    ts.className = "ts";
    ts.textContent = new Date(ln.time).toLocaleTimeString() + " ";
    const text = document.createElement("span");
    text.className = ln.stream;
    text.textContent = ln.text;
    row.append(ts, text);
    frag.append(row);
  }
  pre.append(frag);
  while (pre.childElementCount > 2000) pre.firstElementChild.remove();
  logAfter = lines[lines.length - 1].seq;
  if (el("follow").checked) pre.scrollTop = pre.scrollHeight;
}

// ---------- formatting ----------

function duration(sec) {
  sec = Math.max(0, Math.floor(sec));
  if (sec < 60) return `${sec}s`;
  const m = Math.floor(sec / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

function lastExit(e) {
  if (!e) return "–";
  return `${e.requested ? "stopped" : `code ${e.code}`}, ${ago(e.at)}`;
}

function bytes(n) {
  if (n < 1048576) return `${Math.max(1, Math.round(n / 1024))} KB`;
  if (n < 1073741824) return `${Math.round(n / 1048576)} MB`;
  return `${(n / 1073741824).toFixed(1)} GB`;
}

function ago(iso) {
  return `${duration((Date.now() - Date.parse(iso)) / 1000)} ago`;
}

el("logs-close").addEventListener("click", closeLogs);
refresh();
setInterval(refresh, POLL_MS);
