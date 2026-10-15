// The PC's load while you use the local models, and warnings when it gets
// high. Shared by the chat page (which shows the live strip in #monitor) and
// the dashboard and Local AI pages (warnings only).
//
// Every few seconds it reads /api/status (CPU, memory, and the NVIDIA GPU via
// nvidia-smi: see internal/resources) and Ollama's /api/ps (which model is
// loaded, and how much of it fits on the GPU). A warning appears only after a
// condition holds for a couple of readings, so a brief spike stays quiet, and
// it goes away by itself once things settle. Dismissing one keeps it hidden
// until that condition clears and comes back.

(() => {
  const POLL_MS = 3000;
  /** When to warn. Readings come every 3 s; "for" is how many in a row. */
  const LIMITS = {
    vram: { percent: 95, for: 2 },
    ram: { percent: 90, for: 2 },
    cpu: { percent: 95, for: 4 },
    gpuTemp: { celsius: 85, for: 2 },
  };

  const strip = document.getElementById("monitor");
  const gb = (bytes) => `${(bytes / 1e9).toFixed(1)} GB`;
  const pct = (n) => `${Math.round(n)}%`;
  const make = (tag, text, cls) => {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (cls) e.className = cls;
    return e;
  };

  // ---------------- warnings ----------------

  const streak = {}; // condition -> readings in a row
  const shown = new Map(); // condition -> its toast element
  const dismissed = new Set(); // hidden until the condition clears

  let stack = null;
  function toasts() {
    if (!stack) {
      stack = make("div", undefined, "hb-alerts");
      stack.setAttribute("role", "status");
      stack.setAttribute("aria-live", "polite");
      document.body.append(stack);
    }
    return stack;
  }

  /** Reports one condition for this reading: true (high) with its message, or false. */
  function report(key, high, need, title, detail) {
    streak[key] = high ? (streak[key] || 0) + 1 : 0;
    const on = streak[key] >= need;
    if (!on) {
      dismissed.delete(key);
      shown.get(key)?.remove();
      shown.delete(key);
      return;
    }
    if (dismissed.has(key)) return;
    let toast = shown.get(key);
    if (!toast) {
      toast = make("div", undefined, "hb-alert");
      const close = make("button", "Dismiss", "hb-alert-close");
      close.type = "button";
      close.addEventListener("click", () => {
        dismissed.add(key);
        toast.remove();
        shown.delete(key);
      });
      toast.append(make("strong"), make("p"), close);
      toasts().append(toast);
      shown.set(key, toast);
    }
    // Numbers change each reading: keep the text current.
    toast.querySelector("strong").textContent = title;
    toast.querySelector("p").textContent = detail;
  }

  function checkLimits(res, loaded) {
    const gpu = res?.gpus?.[0];
    if (gpu) {
      const vram = (gpu.mem_used_bytes / gpu.mem_total_bytes) * 100;
      report("vram", vram >= LIMITS.vram.percent, LIMITS.vram.for, "GPU memory is nearly full",
        `${gb(gpu.mem_used_bytes)} of ${gb(gpu.mem_total_bytes)} used. A longer chat or a second model may spill into system RAM and slow down.`);
      report("gpuTemp", gpu.temperature_c >= LIMITS.gpuTemp.celsius, LIMITS.gpuTemp.for, "The GPU is running hot",
        `${Math.round(gpu.temperature_c)}°C. It throttles itself if it keeps climbing; a pause between long answers lets it cool.`);
    }
    if (res?.mem_total_bytes) {
      const ram = (res.mem_used_bytes / res.mem_total_bytes) * 100;
      report("ram", ram >= LIMITS.ram.percent, LIMITS.ram.for, "The PC is low on memory",
        `${pct(ram)} of ${gb(res.mem_total_bytes)} in use. Close other apps, or use a smaller model.`);
    }
    if (res) {
      report("cpu", res.cpu_percent >= LIMITS.cpu.percent, LIMITS.cpu.for, "The CPU has been maxed out",
        `${pct(res.cpu_percent)} for the last ${(LIMITS.cpu.for * POLL_MS) / 1000} seconds. Part of a model running from RAM uses the CPU heavily.`);
    }
    // A model that doesn't fit on the GPU: works, but slowly. Once is enough.
    for (const m of loaded) {
      const onGpu = m.size ? m.size_vram / m.size : 1;
      report(`spill:${m.name}`, onGpu < 0.98, 1, `${m.name} doesn't fit on the GPU`,
        `${pct((1 - onGpu) * 100)} of it (${gb(m.size - m.size_vram)}) runs from system RAM, so answers are slower. A model that fits in ${gpu ? gb(gpu.mem_total_bytes) : "the GPU's memory"} answers much faster.`);
    }
    // Models that were loaded before and aren't now: clear their warnings.
    for (const key of Object.keys(streak)) {
      if (key.startsWith("spill:") && !loaded.some((m) => `spill:${m.name}` === key)) report(key, false, 1);
    }
  }

  // ---------------- the live strip ----------------

  function level(percent, warnAt, badAt) {
    return percent >= badAt ? "bad" : percent >= warnAt ? "warn" : "ok";
  }

  function meter(label, value, percent, lvl, title) {
    const m = make("div", undefined, `hb-meter hb-${lvl}`);
    if (title) m.title = title;
    const bar = make("span", undefined, "hb-bar");
    const fill = make("span", undefined, "hb-fill");
    fill.style.width = `${Math.max(0, Math.min(100, percent))}%`;
    bar.append(fill);
    m.append(make("span", label, "hb-label"), bar, make("span", value, "hb-value"));
    return m;
  }

  function renderStrip(res, loaded) {
    if (!strip) return;
    if (!res) {
      strip.hidden = true;
      return;
    }
    const items = [];
    const gpu = res.gpus?.[0];
    if (gpu) {
      const vram = (gpu.mem_used_bytes / gpu.mem_total_bytes) * 100;
      items.push(meter("GPU", pct(gpu.util_percent), gpu.util_percent, "ok", gpu.name));
      items.push(meter("VRAM", `${gb(gpu.mem_used_bytes)} / ${gb(gpu.mem_total_bytes)}`, vram, level(vram, 85, LIMITS.vram.percent)));
      const temp = make("div", `${Math.round(gpu.temperature_c)}°C`, `hb-temp hb-${level(gpu.temperature_c, 78, LIMITS.gpuTemp.celsius)}`);
      temp.title = gpu.power_watts ? `GPU temperature, drawing ${Math.round(gpu.power_watts)} W` : "GPU temperature";
      items.push(temp);
    }
    const ram = (res.mem_used_bytes / res.mem_total_bytes) * 100;
    items.push(meter("RAM", `${gb(res.mem_used_bytes)} / ${gb(res.mem_total_bytes)}`, ram, level(ram, 80, LIMITS.ram.percent)));
    items.push(meter("CPU", pct(res.cpu_percent), res.cpu_percent, level(res.cpu_percent, 80, LIMITS.cpu.percent)));
    for (const m of loaded) {
      const onGpu = m.size ? m.size_vram / m.size : 1;
      const where = onGpu >= 0.98 ? "all on GPU" : `${pct(onGpu * 100)} on GPU, rest in RAM`;
      items.push(make("div", `${m.name}: ${where}`, `hb-model hb-${onGpu >= 0.98 ? "ok" : "warn"}`));
    }
    if (!loaded.length && gpu) items.push(make("div", "No model loaded", "hb-model muted"));
    strip.replaceChildren(...items);
    strip.hidden = false;
  }

  // ---------------- polling ----------------

  let aiEnabled = null;
  async function tick() {
    if (document.hidden) return; // nobody's looking: check again when they are
    const res = await fetch("api/status").then((r) => r.json()).then((s) => s.resources ?? null).catch(() => null);
    if (aiEnabled === null) aiEnabled = await fetch("api/ai").then((r) => r.json()).then((i) => !!i.enabled).catch(() => false);
    const loaded = aiEnabled
      ? await fetch("ollama/api/ps").then((r) => (r.ok ? r.json() : { models: [] })).then((d) => d.models ?? []).catch(() => [])
      : [];
    renderStrip(res, loaded);
    checkLimits(res, loaded);
  }

  tick();
  setInterval(tick, POLL_MS);
  document.addEventListener("visibilitychange", () => !document.hidden && tick());
})();
