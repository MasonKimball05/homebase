// The Projects page: this PC's git projects, what changed in them since the
// last commit, and their files. Read-only (see internal/projects).
//
// Where you are lives in the address (#shop, #shop/files/src, #shop/file/
// src/app.py), so Back works and a project can be bookmarked. With Live on,
// it checks again every few seconds, keeping open diffs open.

const el = (id) => document.getElementById(id);
const make = (tag, text, cls) => {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (cls) e.className = cls;
  return e;
};
const REFRESH_MS = 5000;
const enc = (p) => p.split("/").map(encodeURIComponent).join("/");

/** "just now", "4 min ago", "3 h ago", "2 days ago". */
function ago(iso) {
  if (!iso) return "";
  const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  const d = Math.round(s / 86400);
  return d === 1 ? "yesterday" : `${d} days ago`;
}
const size = (n) => (n >= 1e6 ? `${(n / 1e6).toFixed(1)} MB` : n >= 1e3 ? `${Math.round(n / 1e3)} KB` : `${n} B`);

function showError(text) {
  el("error").textContent = text;
  el("error").hidden = !text;
}

async function getJSON(url) {
  const r = await fetch(url);
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
  return data;
}

// ---------------- where we are ----------------

/** #id | #id/changes | #id/files/<dir> | #id/file/<path> */
function route() {
  const raw = decodeURIComponent(location.hash.slice(1));
  const [id = "", view = "changes", ...rest] = raw.split("/");
  return { id, view: ["changes", "files", "file"].includes(view) ? view : "changes", path: rest.join("/") };
}
const link = (id, view = "changes", path = "") => `#${encodeURIComponent(id)}/${view}${path ? "/" + enc(path) : ""}`;

// ---------------- the project list ----------------

let projects = [];

async function loadList() {
  const data = await getJSON("api/projects");
  if (!data.enabled) {
    showError("The Projects page isn't set up: add a \"projects\" section with your project folders to homebase.json (see the README).");
    return false;
  }
  projects = data.projects || [];
  const current = route().id;
  const items = projects.map((p) => {
    const a = make("a");
    a.href = link(p.id);
    if (p.id === current) a.setAttribute("aria-current", "true");
    a.title = p.root;
    a.append(make("span", p.name, "p-name"));
    if (p.changed) a.append(make("span", String(p.changed), "p-changed"));
    a.append(make("span", `${p.changed ? "changed" : "committed"} ${ago(p.last_change)}`, "p-when"));
    const sync = [p.ahead ? `${p.ahead} to push` : "", p.behind ? `${p.behind} to pull` : ""].filter(Boolean).join(" · ");
    if (sync) a.append(make("span", sync, "p-sync"));
    const li = make("li");
    li.append(a);
    return li;
  });
  if (!items.length) items.push(make("li", "No git projects in the configured folders.", "muted small"));
  el("projects").replaceChildren(...items);
  return true;
}

// ---------------- changes ----------------

const openDiffs = new Set(); // paths whose diff is open, across refreshes
let seenFiles = null; // for "open the first few" on first view
let lastChanges = "";

function renderDiff(text) {
  const pre = make("div", undefined, "diff");
  for (const line of text.split("\n")) {
    const cls = line.startsWith("@@") ? "d-hunk" : line.startsWith("+++") || line.startsWith("---") ? "d-file" : line.startsWith("+") ? "d-add" : line.startsWith("-") ? "d-del" : "";
    pre.append(make("div", line || " ", cls));
  }
  return pre;
}

async function showChanges(p) {
  const data = await getJSON(`api/projects/${encodeURIComponent(p.id)}/changes`);
  const key = JSON.stringify(data.files);
  if (key === lastChanges) return; // nothing new: keep the page (and scroll) as it is
  lastChanges = key;
  el("p-count").textContent = data.files.length || "";
  if (!data.files.length) {
    el("changes").replaceChildren(make("p", "Nothing changed since the last commit.", "muted"));
    return;
  }
  // First time: open the first few so there's something to read.
  if (seenFiles === null) {
    seenFiles = p.id;
    data.files.slice(0, 6).forEach((f) => openDiffs.add(`${p.id}:${f.path}`));
  }
  const cards = data.files.map((f) => {
    const d = make("details", undefined, "file-change");
    const k = `${p.id}:${f.path}`;
    d.open = openDiffs.has(k);
    d.addEventListener("toggle", () => (d.open ? openDiffs.add(k) : openDiffs.delete(k)));
    const s = make("summary");
    s.append(make("span", f.kind, `kind kind-${f.kind}`), make("span", f.path, "fc-path"));
    if (f.diff) {
      const lines = f.diff.split("\n");
      const add = lines.filter((l) => l.startsWith("+") && !l.startsWith("+++")).length;
      const del = lines.filter((l) => l.startsWith("-") && !l.startsWith("---")).length;
      const stat = make("span", undefined, "fc-stat");
      stat.append(make("span", `+${add}`, "add"), " ", make("span", `−${del}`, "del"));
      s.append(stat);
    }
    if (f.kind !== "deleted") {
      const open = make("a", "View file", "open-file");
      open.href = link(p.id, "file", f.path);
      open.addEventListener("click", (e) => e.stopPropagation());
      s.append(open);
    }
    d.append(s);
    if (f.note) d.append(make("p", f.note, "fc-note"));
    if (f.diff) d.append(renderDiff(f.diff));
    if (f.truncated) d.append(make("p", "Cut short: the rest is too long to show here.", "fc-note"));
    return d;
  });
  el("changes").replaceChildren(...cards);
}

// ---------------- files ----------------

let lastTree = "";

function crumbs(p, path) {
  const parts = path ? path.split("/") : [];
  const nodes = [];
  const root = make("a", p.name);
  root.href = link(p.id, "files");
  nodes.push(root);
  parts.forEach((part, i) => {
    nodes.push(make("span", "/", "muted"));
    const sub = parts.slice(0, i + 1).join("/");
    const a = make("a", part);
    a.href = link(p.id, i === parts.length - 1 && route().view === "file" ? "file" : "files", sub);
    nodes.push(a);
  });
  el("crumbs").replaceChildren(...nodes);
}

function statusDot(status) {
  if (!status) return null;
  const cls = status === "??" ? "dot-new" : status.includes("D") ? "dot-deleted" : "";
  const dot = make("span", undefined, `dot-changed ${cls}`);
  dot.title = status === "changed" ? "Changes inside" : status === "??" ? "New, not committed" : "Changed since the last commit";
  return dot;
}

async function showTree(p, path) {
  const data = await getJSON(`api/projects/${encodeURIComponent(p.id)}/tree?path=${encodeURIComponent(path)}`);
  crumbs(p, data.path);
  el("viewer").hidden = true;
  el("entries").closest("table").hidden = false;
  const key = JSON.stringify(data);
  if (key === lastTree) return;
  lastTree = key;
  const rows = data.entries.map((e) => {
    const tr = make("tr", undefined, e.dir ? "e-dir" : "");
    const name = make("td");
    const a = make("a", e.name);
    const sub = data.path ? `${data.path}/${e.name}` : e.name;
    a.href = link(p.id, e.dir ? "files" : "file", sub);
    name.append(a);
    const dot = statusDot(e.status);
    if (dot) name.append(dot);
    tr.append(name, make("td", e.dir ? "" : size(e.size), "e-meta"), make("td", ago(e.modified), "e-meta"));
    return tr;
  });
  if (data.path) {
    const up = make("tr", undefined, "e-dir");
    const td = make("td");
    const a = make("a", "..");
    a.href = link(p.id, "files", data.path.split("/").slice(0, -1).join("/"));
    td.append(a);
    up.append(td, make("td"), make("td"));
    rows.unshift(up);
  }
  if (!rows.length) rows.push(make("tr", "This folder is empty."));
  el("entries").replaceChildren(...rows);
}

let lastFile = "";

async function showFile(p, path) {
  const data = await getJSON(`api/projects/${encodeURIComponent(p.id)}/file?path=${encodeURIComponent(path)}`);
  crumbs(p, data.path);
  el("entries").closest("table").hidden = true;
  el("viewer").hidden = false;
  const key = JSON.stringify(data);
  if (key === lastFile) return;
  lastFile = key;
  el("v-path").textContent = data.path;
  el("v-meta").textContent = `${size(data.size)} · changed ${ago(data.modified)}${data.status ? " · not committed" : ""}`;
  el("v-note").textContent = data.note || "";
  el("v-note").hidden = !data.note;
  const text = (data.text ?? "").replace(/\r\n/g, "\n").replace(/\n$/, "");
  el("v-text").replaceChildren(...(data.text !== undefined ? text.split("\n").map((l) => make("span", l)) : []));
}

// ---------------- git buttons ----------------

let busy = false;

function renderSync(p) {
  const parts = [];
  if (!p.upstream) parts.push(make("span", "No remote branch"));
  else if (p.gone) parts.push(make("span", `${p.upstream} was deleted on the remote`));
  else {
    parts.push(make("span", p.upstream));
    if (p.ahead) parts.push(" · ", make("span", `${p.ahead} to push`, "push"));
    if (p.behind) parts.push(" · ", make("span", `${p.behind} to pull`, "pull"));
    if (!p.ahead && !p.behind) parts.push(" · in sync");
    parts.push(` · ${p.last_fetch ? `checked ${ago(p.last_fetch)}` : "never fetched"}`);
  }
  el("sync-state").replaceChildren(...parts);
  const set = (sel, on) => (document.querySelector(sel).disabled = busy || !on);
  set('[data-git="fetch"]', !!p.upstream);
  set('[data-git="pull"]', !!p.upstream && !p.gone && p.behind > 0);
  set('[data-git="push"]', !!p.upstream && !p.gone && p.ahead > 0);
  set("#commit-open", p.changed > 0);
}

function showResult(ok, text, output) {
  el("result").hidden = false;
  el("result").classList.toggle("bad", !ok);
  el("result-text").textContent = text;
  el("result-output").textContent = output || "";
  el("result-details").hidden = !output;
  el("result-details").open = false;
}

const VERB = { fetch: "Fetching…", pull: "Pulling…", push: "Pushing…", commit: "Committing…" };

async function gitAction(action, body) {
  const p = projects.find((x) => x.id === route().id);
  if (!p || busy) return;
  if (action === "push" && !confirm(`Push ${p.ahead} commit${p.ahead === 1 ? "" : "s"} from ${p.name} to ${p.upstream}?`)) return;
  if (action === "pull" && !confirm(`Pull ${p.behind} commit${p.behind === 1 ? "" : "s"} from ${p.upstream} into ${p.name} on the desktop?`)) return;
  busy = true;
  renderSync(p);
  showResult(true, VERB[action]);
  el("result").classList.remove("bad");
  try {
    const r = await fetch(`api/projects/${encodeURIComponent(p.id)}/git/${action}`, {
      method: "POST",
      headers: { "X-Homebase": "1", "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : "{}",
    });
    const res = await r.json().catch(() => ({}));
    showResult(r.ok && res.ok, res.summary || res.error || `HTTP ${r.status}`, res.output);
    if (r.ok && action === "commit") {
      el("commit-form").hidden = true;
      el("commit-message").value = "";
    }
  } catch {
    showResult(false, "Can't reach homebase. Is it still running?");
  } finally {
    busy = false;
    lastChanges = lastTree = lastFile = ""; // the commit changed what's shown
    await refresh();
  }
}

document.querySelectorAll("[data-git]").forEach((b) => b.addEventListener("click", () => gitAction(b.dataset.git)));
el("commit-open").addEventListener("click", () => {
  const p = projects.find((x) => x.id === route().id);
  el("commit-note").textContent = `Commits all ${p?.changed ?? ""} changed file${p?.changed === 1 ? "" : "s"} under Changes (new files too), on ${p?.branch ?? "this branch"}. Pushing is a separate step.`;
  el("commit-form").hidden = false;
  el("commit-message").focus();
});
el("commit-cancel").addEventListener("click", () => (el("commit-form").hidden = true));
el("commit-form").addEventListener("submit", (e) => {
  e.preventDefault();
  const message = el("commit-message").value.trim();
  if (!message) return el("commit-message").focus();
  gitAction("commit", { message });
});

// ---------------- putting it together ----------------

let showing = "";

async function render() {
  const r = route();
  const p = projects.find((x) => x.id === r.id);
  el("placeholder").hidden = !!p;
  el("project").hidden = !p;
  for (const a of el("projects").querySelectorAll("a")) a.setAttribute("aria-current", String(a.getAttribute("href") === link(r.id)));
  if (!p) return;
  const where = `${r.id}|${r.view}|${r.path}`;
  if (where !== showing) {
    // Somewhere new: forget what the last view drew.
    if (showing.split("|")[0] !== r.id) {
      el("result").hidden = true;
      el("commit-form").hidden = true;
    }
    showing = where;
    lastChanges = lastTree = lastFile = "";
    if (seenFiles !== p.id) seenFiles = null;
  }
  el("p-name").textContent = p.name;
  el("p-branch").textContent = p.branch || "";
  el("p-branch").hidden = !p.branch;
  el("p-commit").textContent = p.last_commit ? `last commit ${p.last_commit.hash} "${p.last_commit.subject}", ${ago(p.last_commit.when)}` : "no commits yet";
  el("p-count").textContent = p.changed || "";
  renderSync(p);
  el("tab-changes").href = link(p.id, "changes");
  el("tab-files").href = link(p.id, "files");
  const onChanges = r.view === "changes";
  el("tab-changes").setAttribute("aria-selected", String(onChanges));
  el("tab-files").setAttribute("aria-selected", String(!onChanges));
  el("changes").hidden = !onChanges;
  el("files").hidden = onChanges;
  try {
    if (onChanges) await showChanges(p);
    else if (r.view === "files") await showTree(p, r.path);
    else await showFile(p, r.path);
    showError("");
  } catch (e) {
    showError(e.message);
  }
}

async function refresh() {
  try {
    if (!(await loadList())) return;
    await render();
    el("updated").textContent = `· updated ${new Date().toLocaleTimeString([], { hour: "numeric", minute: "2-digit", second: "2-digit" })}`;
  } catch {
    showError("Can't reach homebase. Is it still running?");
  }
}

window.addEventListener("hashchange", render);
setInterval(() => {
  if (el("live").checked && !document.hidden) refresh();
}, REFRESH_MS);
document.addEventListener("visibilitychange", () => !document.hidden && el("live").checked && refresh());
refresh();
