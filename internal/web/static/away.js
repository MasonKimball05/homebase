// The Away page: queue coding tasks for this PC to do while you're away,
// and accept or discard what comes back. API: internal/away/api.go.
"use strict";

const el = (id) => document.getElementById(id);
const POLL_MS = 5000;

function make(tag, text, cls) {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  if (cls) e.className = cls;
  return e;
}

async function api(path, body) {
  const opts = body === undefined ? {} : { method: "POST", headers: { "X-Homebase": "1", "Content-Type": "application/json" }, body: JSON.stringify(body) };
  const r = await fetch(path, opts);
  const ct = r.headers.get("Content-Type") || "";
  const data = ct.includes("json") ? await r.json() : await r.text();
  if (!r.ok) throw new Error((data && data.error) || `HTTP ${r.status}`);
  return data;
}

function showError(msg) {
  el("error").textContent = msg;
  el("error").hidden = !msg;
}

function duration(s) {
  if (s < 60) return `${Math.round(s)} s`;
  if (s < 3600) return `${Math.round(s / 60)} min`;
  return `${Math.floor(s / 3600)} h ${Math.round((s % 3600) / 60)} min`;
}
const ago = (iso) => `${duration((Date.now() - Date.parse(iso)) / 1000)} ago`;

const STATUS = {
  queued: "queued", running: "working", done: "ready to review", "needs-you": "needs you",
  failed: "failed", accepted: "merged", discarded: "discarded", cancelled: "cancelled",
};
const WHO = { qwen: "Local model", tests: "Its tests", check: "Quick check", review: "Claude reviewed", "claude-fix": "Claude fixed it" };

// ---------------- mode ----------------

function renderMode(v) {
  for (const b of document.querySelectorAll("[data-mode]")) b.setAttribute("aria-checked", String(b.dataset.mode === v.mode));
  const idle = v.idleSeconds === null ? null : duration(v.idleSeconds);
  el("mode-note").textContent =
    v.stopped ? `Switched itself off: ${v.stopped}. Tasks wait in the queue; fix it, then switch Away back on.`
    : v.mode === "off" ? "Paused. Tasks wait in the queue."
    : v.mode === "on" ? "Working through the queue now, even if someone is at the PC."
    : idle === null ? `Waiting for the PC to be idle ${v.idleMinutes} min, but the idle reporter isn't running there (homebase -idle-report), so it won't start.`
    : `Starts after ${v.idleMinutes} min without keyboard or mouse input, and stops when someone's back. Idle for ${idle}.`;
}

for (const b of document.querySelectorAll("[data-mode]")) {
  b.addEventListener("click", async () => {
    try {
      render(await api("api/away/mode", { mode: b.dataset.mode }));
    } catch (e) {
      showError(e.message);
    }
  });
}

// ---------------- tasks ----------------

const open = new Map(); // id → which panel is open ("diff" | "log"), kept across refreshes

function renderDiff(text) {
  const box = make("div", undefined, "diff");
  if (!text.trim()) {
    box.append(make("div", "(no changes)"));
    return box;
  }
  for (const line of text.split("\n")) {
    const cls = line.startsWith("diff --git") ? "d-file" : line.startsWith("@@") ? "d-hunk" : line.startsWith("+") && !line.startsWith("+++") ? "d-add" : line.startsWith("-") && !line.startsWith("---") ? "d-del" : "";
    box.append(make("div", line || " ", cls));
  }
  return box;
}

function panel(t, kind, label) {
  const d = make("details");
  d.append(make("summary", label));
  d.open = open.get(t.id) === kind;
  const load = async () => {
    d.querySelector(".diff, .log")?.remove();
    try {
      const text = await api(`api/away/tasks/${encodeURIComponent(t.id)}/${kind}`);
      d.append(kind === "diff" ? renderDiff(text) : make("pre", text || "(empty)", "log"));
    } catch (e) {
      d.append(make("pre", e.message, "log"));
    }
  };
  d.addEventListener("toggle", () => {
    if (d.open) {
      open.set(t.id, kind);
      load();
    } else if (open.get(t.id) === kind) open.delete(t.id);
  });
  if (d.open) load();
  return d;
}

function action(t, name, label, confirmText) {
  const b = make("button", label);
  if (name === "accept") b.className = "primary";
  b.type = "button";
  b.addEventListener("click", async () => {
    if (confirmText && !confirm(confirmText)) return;
    b.disabled = true;
    try {
      render(await api(`api/away/tasks/${encodeURIComponent(t.id)}/${name}`, {}));
      showError("");
    } catch (e) {
      showError(e.message);
      b.disabled = false;
    }
  });
  return b;
}

function renderTask(t) {
  const li = make("li", undefined, "task");
  const head = make("div", undefined, "task-head");
  head.append(make("span", t.project, "task-project"));
  const st = make("span", t.status === "running" && t.stage ? t.stage : STATUS[t.status] || t.status, "status");
  st.dataset.s = t.status;
  head.append(st);
  if (t.verdict === "fixed") head.append(make("span", "Claude finished it", "muted small"));
  if (t.branch) head.append(make("span", t.branch + (t.pushed ? " (pushed)" : ""), "branch-name muted"));
  head.append(make("span", t.finished ? `finished ${ago(t.finished)}` : t.started && t.status === "running" ? `started ${ago(t.started)}` : `added ${ago(t.created)}`, "task-when muted small"));
  li.append(head);
  li.append(make("p", t.text, "task-text"));
  if (t.notes) li.append(make("div", t.notes, "task-notes"));
  if (t.error) li.append(make("p", t.error, "task-error small"));
  if (t.rounds && t.rounds.length) {
    const ul = make("ul", undefined, "rounds");
    for (const r of t.rounds) {
      const verdict = r.verdict ? `: ${r.verdict}` : "";
      ul.append(make("li", `${WHO[r.who] || r.who}${verdict} (${duration(r.seconds)})${r.commit ? ` · ${r.commit}` : ""}`));
    }
    li.append(ul);
  }
  if (t.branch && t.status !== "accepted" && t.status !== "discarded") li.append(panel(t, "diff", `Changes${t.changed ? ` (${t.changed} file${t.changed === 1 ? "" : "s"})` : ""}`));
  if (t.status !== "queued") li.append(panel(t, "log", "Log"));

  const acts = make("div", undefined, "task-actions");
  switch (t.status) {
    case "done":
    case "needs-you":
      acts.append(
        action(t, "accept", "Accept", `Merge ${t.branch} into ${t.base}?`),
        action(t, "discard", "Discard", "Delete this branch and its changes?"),
        action(t, "retry", "Try again", "Throw this attempt away and queue the task again?"),
      );
      break;
    case "queued":
    case "running":
      acts.append(action(t, "cancel", "Cancel", t.status === "running" ? "Stop it now?" : undefined));
      break;
    case "failed":
      // Work it had committed before failing (a restart, say) can still be taken.
      if (t.branch) {
        acts.append(
          action(t, "accept", "Accept", `Merge ${t.branch} into ${t.base}? It wasn't reviewed.`),
          action(t, "discard", "Discard", "Delete this branch and its changes?"),
        );
      }
      acts.append(action(t, "retry", "Try again", t.branch ? "Throw this attempt away and queue the task again?" : undefined), action(t, "delete", "Remove", t.branch ? "Remove it and delete its branch?" : undefined));
      break;
    case "cancelled":
      acts.append(action(t, "retry", "Try again"), action(t, "delete", "Remove"));
      break;
    default:
      acts.append(action(t, "delete", "Remove"));
  }
  li.append(acts);
  return li;
}

function render(v) {
  renderMode(v);
  const ul = el("tasks");
  ul.replaceChildren(...v.tasks.map(renderTask));
  el("empty").hidden = v.tasks.length > 0;
}

async function refresh() {
  try {
    const v = await api("api/away");
    if (v.enabled === false) {
      showError('Away isn\'t set up: add an "away" section to homebase.json (see the README).');
      return;
    }
    // Don't rebuild while you're typing in a details panel or selecting text.
    if (window.getSelection && String(window.getSelection()).length) return;
    render(v);
    showError("");
  } catch (e) {
    showError(`Couldn't load the queue: ${e.message}`);
  }
}

// ---------------- adding ----------------

async function loadProjects() {
  try {
    const list = await api("api/projects");
    const sel = el("project");
    const last = localStorage.getItem("away.project");
    sel.replaceChildren(...(list.projects || list || []).map((p) => {
      const o = make("option", p.name);
      o.value = p.id;
      return o;
    }));
    if (last && [...sel.options].some((o) => o.value === last)) sel.value = last;
  } catch (e) {
    showError(`Couldn't list the projects: ${e.message}`);
  }
}

el("add").addEventListener("submit", async (e) => {
  e.preventDefault();
  try {
    await api("api/away/tasks", { project: el("project").value, task: el("task").value });
    try {
      localStorage.setItem("away.project", el("project").value);
    } catch {
      /* not critical */
    }
    el("task").value = "";
    showError("");
    refresh();
  } catch (err) {
    showError(err.message);
  }
});

// ---------------- suggested tasks ----------------

let suggestFor = "";

async function loadSuggestions() {
  const project = el("project").value;
  if (!project || project === suggestFor) return;
  suggestFor = project;
  const ul = el("suggest-list");
  ul.replaceChildren(make("li", "Looking…", "muted"));
  let list;
  try {
    list = await api(`api/away/suggest?project=${encodeURIComponent(project)}`);
  } catch (e) {
    ul.replaceChildren(make("li", e.message, "muted"));
    return;
  }
  if (!list.length) {
    ul.replaceChildren(make("li", "Nothing untested found (Python projects only, for now).", "muted"));
    return;
  }
  ul.replaceChildren(
    ...list.slice(0, 15).map((sg) => {
      const li = make("li");
      li.append(make("span", `${sg.path}: ${sg.function}`, "suggest-name"), make("span", `${sg.lines} lines`, "muted small"));
      const b = make("button", "Draft a task");
      b.type = "button";
      b.addEventListener("click", async () => {
        b.disabled = true;
        b.textContent = "Drafting (local model, ~30 s)…";
        try {
          const d = await api("api/away/draft", { project, path: sg.path, function: sg.function });
          el("task").value = d.task;
          el("task").focus();
          el("add").scrollIntoView({ behavior: "smooth", block: "center" });
          showError("");
          b.textContent = "Drafted";
        } catch (e) {
          showError(`Couldn't draft it: ${e.message}`);
          b.textContent = "Draft a task";
          b.disabled = false;
        }
      });
      li.append(b);
      if (sg.doc) li.append(make("span", sg.doc, "suggest-doc muted"));
      return li;
    }),
  );
}

el("suggest").addEventListener("toggle", () => el("suggest").open && loadSuggestions());
el("project").addEventListener("change", () => el("suggest").open && loadSuggestions());

// ---------------- track record ----------------

const OUTCOME = { accepted: "accepted", discarded: "discarded", retried: "tried again", removed: "removed", "": "waiting for you" };

function summarize(list, label) {
  const box = make("div");
  box.append(make("strong", label));
  const reviewed = list.filter((e) => e.stats.reviewed);
  if (!list.length) {
    box.append(make("span", "none yet", "muted"));
    return box;
  }
  const pct = (n, d) => (d ? `${Math.round((100 * n) / d)}%` : "–");
  const avg = (f) => list.reduce((s, e) => s + f(e), 0) / list.length;
  const accepted = list.filter((e) => e.outcome === "accepted").length;
  box.append(
    make("div", `${list.length} attempt${list.length === 1 ? "" : "s"}, ${accepted} accepted`),
    make("div", `passed Claude's first review: ${pct(reviewed.filter((e) => e.stats.passedFirstReview).length, reviewed.length)}`),
    make("div", `Claude had to fix: ${pct(list.filter((e) => e.stats.claudeFixed).length, list.length)}`),
    make("div", `model time ${duration(avg((e) => e.stats.qwenSeconds))}, ${avg((e) => e.stats.testRuns).toFixed(1)} test runs on average`),
  );
  return box;
}

async function loadRecord() {
  let list;
  try {
    list = await api("api/away/examples");
  } catch {
    return;
  }
  if (!Array.isArray(list) || !list.length) return;
  el("record").hidden = false;
  el("record-summary").replaceChildren(
    summarize(list.filter((e) => e.shown > 0), "With earlier examples"),
    summarize(list.filter((e) => e.heldOut), "Held out (none, on purpose)"),
    summarize(list.filter((e) => !e.shown && !e.heldOut), "Without (none similar enough, or before recall)"),
  );
  el("record-rows").replaceChildren(
    ...list.map((e) => {
      const tr = make("tr");
      const first = !e.stats.reviewed ? make("td", "not reviewed", "muted") : make("td", e.stats.passedFirstReview ? "passed" : "sent back", e.stats.passedFirstReview ? "yes" : "no");
      const result = `${e.author === "qwen+claude" ? "Claude fixed · " : ""}${OUTCOME[e.outcome] ?? e.outcome}`;
      tr.append(
        make("td", new Date(e.finished).toLocaleDateString([], { month: "short", day: "numeric" })),
        make("td", `${e.project}: ${e.task.length > 90 ? e.task.slice(0, 89) + "…" : e.task}`, "task"),
        make("td", e.heldOut ? "held out" : String(e.shown)),
        make("td", duration(e.stats.qwenSeconds)),
        make("td", String(e.stats.testRuns)),
        first,
        make("td", result, e.outcome === "accepted" ? "yes" : e.outcome === "discarded" ? "no" : ""),
      );
      tr.title = e.task;
      return tr;
    }),
  );
}

loadProjects();
refresh();
setInterval(refresh, POLL_MS);
loadRecord();
setInterval(loadRecord, 60_000);
