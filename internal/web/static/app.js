// The apex-engine console. No framework and no build step: everything on screen
// is built with DOM calls, and all data goes in as text, never as markup, because
// tool and model output is shown here.

const root = document.getElementById("app");

/* -------------------------------- dom helpers ------------------------------- */

function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k in el) el[k] = v;
    else el.setAttribute(k, v);
  }
  append(el, kids);
  return el;
}

function append(el, kids) {
  for (const kid of kids.flat(Infinity)) {
    if (kid === null || kid === undefined || kid === false) continue;
    el.append(kid instanceof Node ? kid : String(kid));
  }
}

const SVG = "http://www.w3.org/2000/svg";
function s(tag, attrs, ...kids) {
  const el = document.createElementNS(SVG, tag);
  for (const [k, v] of Object.entries(attrs || {})) el.setAttribute(k, v);
  append(el, kids);
  return el;
}

function clear(el, ...kids) {
  el.replaceChildren();
  append(el, kids);
}

let toastTimer;
function toast(message, bad) {
  document.querySelector(".toast")?.remove();
  const el = h("div", { class: "toast" + (bad ? " bad" : ""), role: "status" }, message);
  document.body.append(el);
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.remove(), bad ? 7000 : 3000);
}

function confirmDialog(title, body, action) {
  return new Promise((resolve) => {
    const dlg = h("dialog", {},
      h("div", { class: "stack" },
        h("h2", {}, title),
        h("p", { class: "muted" }, body),
        h("div", { class: "row" },
          h("button", { class: "danger", onclick: () => finish(true) }, action),
          h("button", { onclick: () => finish(false) }, "Keep it"))));
    function finish(answer) { dlg.close(); dlg.remove(); resolve(answer); }
    dlg.addEventListener("cancel", () => resolve(false));
    document.body.append(dlg);
    dlg.showModal();
  });
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    toast("Copied");
  } catch {
    toast("Could not reach the clipboard. Select the text and copy it.", true);
  }
}

function pretty(v) {
  return typeof v === "string" ? v : JSON.stringify(v, null, 2);
}

function ago(iso) {
  const secs = Math.round((Date.now() - new Date(iso)) / 1000);
  if (secs < 5) return "just now";
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return new Date(iso).toLocaleDateString();
}

function duration(from, to) {
  if (!from) return "";
  const secs = ((to ? new Date(to) : Date.now()) - new Date(from)) / 1000;
  if (secs < 60) return `${secs.toFixed(1)}s`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ${Math.round(secs % 60)}s`;
  return `${Math.floor(secs / 3600)}h ${Math.round((secs % 3600) / 60)}m`;
}

const STATUS = {
  queued: "Queued", running: "Running", waiting_approval: "Needs approval", succeeded: "Succeeded",
  failed: "Failed", cancelled: "Cancelled", pending: "Pending", skipped: "Skipped",
};
const SOURCE = { api: "Manual", webhook: "Webhook", schedule: "Schedule", retry: "Continued" };
const pill = (status) => h("span", { class: "pill " + status }, STATUS[status] || status);

/* ------------------------------------ api ----------------------------------- */

const KEY = "apex.key";
function storedKey() { try { return localStorage.getItem(KEY); } catch { return null; } }
function storeKey(v) { try { v ? localStorage.setItem(KEY, v) : localStorage.removeItem(KEY); } catch { /* private window */ } }

let apiKey = storedKey();

class ApiError extends Error {
  constructor(status, body) {
    super(body?.error?.message || `Request failed (${status})`);
    this.status = status;
    this.code = body?.error?.code;
    this.problems = body?.error?.problems;
  }
}

async function api(method, path, body) {
  const resp = await fetch(path, {
    method,
    headers: { Authorization: `Bearer ${apiKey}`, ...(body !== undefined && { "Content-Type": "application/json" }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (resp.status === 204) return null;
  const data = await resp.json().catch(() => null);
  if (!resp.ok) {
    if (resp.status === 401) signOut();
    throw new ApiError(resp.status, data);
  }
  return data;
}

function signOut() {
  apiKey = null;
  storeKey(null);
  view.stop();
  renderSignIn();
}

function errorText(err) {
  if (err.problems?.length) return `${err.message}: ${err.problems.join("; ")}`;
  return err.message;
}

function run(fn) {
  return async (ev) => {
    const btn = ev?.currentTarget instanceof HTMLButtonElement ? ev.currentTarget : null;
    if (btn) btn.disabled = true;
    try {
      await fn(ev);
    } catch (err) {
      toast(errorText(err), true);
    } finally {
      if (btn) btn.disabled = false;
    }
  };
}

/* ------------------------------ page lifecycle ------------------------------ */

// Timers and in-flight results belong to the page that started them.
const view = {
  token: 0,
  timers: [],
  stop() {
    this.token++;
    this.timers.forEach(clearInterval);
    this.timers = [];
  },
  every(ms, fn) {
    const token = this.token;
    let busy = false;
    const id = setInterval(async () => {
      if (busy || token !== this.token) return;
      busy = true;
      try { await fn(); } catch { /* the next tick retries */ } finally { busy = false; }
    }, ms);
    this.timers.push(id);
  },
  live(token) { return token === this.token; },
};

/* ----------------------------------- graph ---------------------------------- */

const NODE_W = 212, NODE_H = 56, GAP_X = 18, GAP_Y = 30, PAD = 12;

function kindLabel(type) {
  if (type.startsWith("trigger.")) return "Trigger";
  if (type.startsWith("agent.")) return "AI step";
  if (type === "tool.http") return "HTTP request";
  if (type === "logic.branch") return "If / else";
  if (type === "logic.filter") return "Filter";
  if (type === "logic.loop") return "Loop";
  if (type === "logic.approval") return "Human approval";
  const [family, , op] = type.split(".");
  if (family === "api") return `API · ${op}`;
  if (family === "mcp") return `MCP · ${op}`;
  return type;
}

function clip(text, n) {
  return text.length > n ? text.slice(0, n - 1) + "…" : text;
}

function layout(nodes) {
  const byKey = new Map(nodes.map((n) => [n.key, n]));
  const depth = new Map();
  const depthOf = (key, seen = new Set()) => {
    if (depth.has(key)) return depth.get(key);
    if (seen.has(key)) return 0;
    seen.add(key);
    const deps = (byKey.get(key)?.dependsOn || []).filter((d) => byKey.has(d));
    const d = deps.length ? 1 + Math.max(...deps.map((x) => depthOf(x, seen))) : 0;
    depth.set(key, d);
    return d;
  };
  nodes.forEach((n) => depthOf(n.key));

  // Most workflows are long chains, so the graph flows down the page.
  const levels = [];
  for (const n of nodes) (levels[depth.get(n.key)] ||= []).push(n);
  const slot = new Map();
  levels.forEach((level) => {
    // Order each level by where its parents sit, which removes most crossings.
    const score = (n) => {
      const parents = (n.dependsOn || []).filter((d) => slot.has(d));
      return parents.length ? parents.reduce((a, d) => a + slot.get(d), 0) / parents.length : 0;
    };
    level.sort((a, b) => score(a) - score(b));
    level.forEach((n, i) => slot.set(n.key, i));
  });

  const widest = Math.max(...levels.map((l) => l.length));
  const width = widest * NODE_W + (widest - 1) * GAP_X;
  const pos = new Map();
  levels.forEach((level, y) => {
    const own = level.length * NODE_W + (level.length - 1) * GAP_X;
    level.forEach((n, i) => pos.set(n.key, {
      x: PAD + (width - own) / 2 + i * (NODE_W + GAP_X),
      y: PAD + y * (NODE_H + GAP_Y),
    }));
  });
  return { pos, width: PAD * 2 + width, height: PAD * 2 + levels.length * NODE_H + (levels.length - 1) * GAP_Y };
}

function graphView(nodes, onSelect) {
  const { pos, width, height } = layout(nodes);
  const svg = s("svg", { width, height, viewBox: `0 0 ${width} ${height}`, role: "group", "aria-label": "Workflow graph" });
  svg.append(s("defs", {}, s("marker", { id: "arrow", viewBox: "0 0 8 8", refX: "7", refY: "4", markerWidth: "7", markerHeight: "7", orient: "auto" },
    s("path", { d: "M0 0 8 4 0 8z", class: "arrow" }))));
  const edges = [];
  for (const n of nodes) {
    const to = pos.get(n.key);
    for (const d of n.dependsOn || []) {
      const from = pos.get(d);
      if (!from) continue;
      const x1 = from.x + NODE_W / 2, y1 = from.y + NODE_H, x2 = to.x + NODE_W / 2, y2 = to.y - 2;
      const mid = (y2 - y1) / 2;
      const path = s("path", { class: "edge", d: `M${x1} ${y1}C${x1} ${y1 + mid} ${x2} ${y2 - mid} ${x2} ${y2}`, "marker-end": "url(#arrow)" });
      edges.push({ from: d, path });
      svg.append(path);
    }
  }
  const shapes = new Map();
  for (const n of nodes) {
    const p = pos.get(n.key);
    const name = n.name || n.key;
    const g = s("g", { class: "gnode", transform: `translate(${p.x} ${p.y})`, tabindex: "0", role: "button", "aria-label": `${name}, ${kindLabel(n.type)}` },
      s("title", {}, `${name}\n${n.type}`),
      s("rect", { class: "box", width: NODE_W, height: NODE_H, rx: "7" }),
      s("rect", { class: "bar", x: "1", y: "9", width: "3.5", height: NODE_H - 18, rx: "1.5" }),
      s("text", { class: "title", x: "15", y: "24" }, clip(name, 27)),
      s("text", { class: "kind", x: "15", y: "42" }, clip(kindLabel(n.type), 34)));
    g.addEventListener("click", () => onSelect(n.key));
    g.addEventListener("keydown", (e) => {
      if (e.key === "Enter" || e.key === " ") { e.preventDefault(); onSelect(n.key); }
    });
    shapes.set(n.key, g);
    svg.append(g);
  }
  const statuses = new Map();
  return {
    el: h("div", { class: "graph" }, svg),
    select(key) {
      shapes.forEach((g, k) => g.classList.toggle("selected", k === key));
    },
    setStatuses(next) {
      next.forEach((status, key) => {
        const g = shapes.get(key);
        if (!g || statuses.get(key) === status) return;
        if (statuses.has(key)) g.classList.remove(statuses.get(key));
        g.classList.add(status);
        statuses.set(key, status);
      });
      edges.forEach((e) => e.path.classList.toggle("done", statuses.get(e.from) === "succeeded"));
    },
  };
}

function insideLoop(nodes, key) {
  const byKey = new Map(nodes.map((n) => [n.key, n]));
  const seen = new Set();
  const walk = (k) => {
    for (const d of byKey.get(k)?.dependsOn || []) {
      if (seen.has(d)) continue;
      seen.add(d);
      if (byKey.get(d)?.type === "logic.loop" || walk(d)) return true;
    }
    return false;
  };
  return walk(key);
}

/* ------------------------------ review of a plan ---------------------------- */

const catalogCache = new Map();

async function catalogNodes(ids) {
  const index = new Map();
  for (const id of ids || []) {
    if (!catalogCache.has(id)) catalogCache.set(id, await api("GET", `/v1/catalogs/${id}`));
    for (const n of catalogCache.get(id).nodes || []) index.set(n.id, n);
  }
  return index;
}

function ancestors(nodes, key) {
  const byKey = new Map(nodes.map((n) => [n.key, n]));
  const out = new Set();
  const walk = (k) => {
    for (const d of byKey.get(k)?.dependsOn || []) {
      if (!out.has(d)) { out.add(d); walk(d); }
    }
  };
  walk(key);
  return [...out].map((k) => byKey.get(k)).filter(Boolean);
}

// sideEffects lists the steps that can change something outside the engine, and
// whether a person has to approve first.
function sideEffects(nodes, index) {
  const out = [];
  for (const n of nodes) {
    let what = null;
    if (n.type.startsWith("api.")) {
      const method = index.get(n.type)?.exec?.method || "API";
      if (method !== "GET") what = `${method} call`;
    } else if (n.type.startsWith("mcp.")) {
      what = "MCP tool, may change data";
    } else if (n.type === "tool.http") {
      const method = String(n.config?.method || "GET").toUpperCase();
      if (method !== "GET") what = `${method} request`;
    }
    if (what) {
      out.push({ node: n, what, guard: ancestors(nodes, n.key).find((a) => a.type === "logic.approval") });
    }
  }
  return out;
}

function reviewPanel(nodes, index) {
  const effects = sideEffects(nodes, index);
  const ai = nodes.filter((n) => n.type.startsWith("agent.")).length;
  const unguarded = effects.filter((e) => !e.guard).length;
  return h("div", { class: "panel stack" },
    h("h2", {}, "Before this runs"),
    effects.length === 0 && h("p", { class: "muted" }, "Nothing here changes data. Every step only reads."),
    effects.length > 0 && h("div", { class: "review" },
      effects.map((e) => h("div", { class: "item" },
        h("span", { class: "guard " + (e.guard ? "yes" : "no") }, e.guard ? "Approval first" : "No approval"),
        h("span", { class: "what" }, h("strong", {}, e.node.name || e.node.key), " ", h("span", { class: "muted" }, e.what))))),
    unguarded > 0 && h("div", { class: "notice warn small" },
      `${unguarded} ${unguarded === 1 ? "step" : "steps"} can change data with no approval in front. Ask for an approval in your prompt if you want one.`),
    ai > 0 && h("p", { class: "muted small" }, `${ai} AI ${ai === 1 ? "step calls" : "steps call"} a model on every run.`));
}

/* ------------------------------- step inspector ----------------------------- */

function configValue(raw, original) {
  if (typeof original === "string") return raw;
  try { return JSON.parse(raw); } catch { return raw; }
}

function stepEditor(node, onChange) {
  const entries = Object.entries(node.config || {});
  return h("div", { class: "panel inspector" },
    h("div", {}, h("h2", {}, node.name || node.key), h("div", { class: "key mono" }, node.type)),
    entries.length === 0 && h("p", { class: "muted small" }, "This step has no settings."),
    entries.map(([key, value]) => h("label", { class: "field" }, key,
      h("textarea", {
        class: "mono", rows: Math.min(6, pretty(value).split("\n").length + (pretty(value).length > 42 ? 1 : 0)),
        value: pretty(value), spellcheck: false,
        oninput: (e) => { node.config[key] = configValue(e.target.value, value); onChange(); },
      }))),
    entries.length > 0 && h("p", { class: "muted small" }, "{{input.x}}, {{outputs.step.field}} and {{item.x}} fill in at run time."));
}

function stepHint(text) {
  return h("div", { class: "panel inspector" }, h("p", { class: "muted" }, text));
}

function inputsForm(defaults) {
  const values = { ...defaults };
  const keys = Object.keys(values);
  const el = h("div", { class: keys.length ? "stack" : "hidden" },
    keys.map((key) => h("label", { class: "field" }, key,
      h("input", { value: pretty(values[key]), class: "mono", spellcheck: false, oninput: (e) => { values[key] = configValue(e.target.value, defaults[key]); } }))));
  return { el, values, hasFields: keys.length > 0 };
}

/* ------------------------------ compile stream ------------------------------ */

async function compileStream(body, onEvent, signal) {
  const resp = await fetch("/v1/compile/stream", {
    method: "POST", signal,
    headers: { Authorization: `Bearer ${apiKey}`, "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!resp.ok) {
    const data = await resp.json().catch(() => null);
    if (resp.status === 401) signOut();
    throw new ApiError(resp.status, data);
  }
  const reader = resp.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buffer += value;
    let cut;
    while ((cut = buffer.indexOf("\n\n")) >= 0) {
      const frame = buffer.slice(0, cut);
      buffer = buffer.slice(cut + 2);
      let event = "message", data = "";
      for (const line of frame.split("\n")) {
        if (line.startsWith("event: ")) event = line.slice(7);
        else if (line.startsWith("data: ")) data += line.slice(6);
      }
      if (data) onEvent(event, JSON.parse(data));
    }
  }
}

/* ----------------------------------- build ---------------------------------- */

// The draft outlives navigation, so opening Runs and coming back loses nothing.
const draft = { prompt: "", selected: null, plan: null, catalogIds: [], trace: null };
const STAGE_NAMES = { route: "Route", map: "Map", reduce: "Write", validate: "Check", repair: "Repair" };

async function pageBuild(main) {
  const token = view.token;
  const [status, cats, flows] = await Promise.all([api("GET", "/v1/status"), api("GET", "/v1/catalogs"), api("GET", "/v1/workflows")]);
  if (!view.live(token)) return;
  const catalogs = cats.catalogs;
  draft.selected ??= new Set(catalogs.map((c) => c.id));
  // Tools added since this draft started count as chosen.
  catalogs.filter((c) => !draft.seen?.has(c.id)).forEach((c) => draft.selected.add(c.id));
  draft.seen = new Set(catalogs.map((c) => c.id));

  const hasDemo = catalogs.some((c) => c.name === "Demo Orders");
  const box = h("textarea", {
    rows: 3, value: draft.prompt, "aria-label": "What should happen",
    placeholder: "Describe what should happen, in plain English.",
    oninput: (e) => { draft.prompt = e.target.value; examples?.classList.toggle("hidden", !!draft.prompt); sync(); },
    onkeydown: (e) => { if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) compile(); },
  });
  const compileBtn = h("button", { class: "primary", onclick: () => compile() }, "Create plan");
  const progress = h("div", { class: "hidden panel stages" });
  const result = h("div", { class: "stack" });
  const sync = () => { compileBtn.disabled = !draft.prompt.trim() || !status.models.ready || busy; };
  let busy = false;

  const chips = catalogs.map((c) => {
    const input = h("input", {
      type: "checkbox", checked: draft.selected.has(c.id),
      onchange: (e) => {
        e.target.checked ? draft.selected.add(c.id) : draft.selected.delete(c.id);
        chip.classList.toggle("on", e.target.checked);
      },
    });
    const chip = h("label", { class: "chip" + (input.checked ? " on" : "") }, input, c.name);
    return chip;
  });

  const examples = hasDemo && !draft.prompt && !draft.plan ? h("div", { class: "stack" },
    h("h3", { class: "muted" }, "Try one with the sample tools"),
    [
      "Get the late orders, check the refund policy for each, and ask me to approve before refunding any that qualify.",
      "List every shipped order and tell me the total value.",
    ].map((text) => h("button", {
      class: "example",
      onclick: () => { draft.prompt = text; box.value = text; examples.remove(); sync(); box.focus(); },
    }, text))) : null;

  async function compile() {
    if (busy || !draft.prompt.trim() || !status.models.ready) return;
    busy = true; sync();
    examples?.remove();
    draft.plan = null;
    clear(result);
    const stages = new Map();
    const started = Date.now();
    progress.classList.remove("hidden");
    const paint = () => clear(progress, [...stages.values()].map((st) => h("div", { class: "stage " + st.status },
      h("span", { class: "state" }, st.status === "start" ? "Working" : st.status === "done" ? "Done" : "Failed"),
      h("span", { class: "label" }, STAGE_NAMES[st.stage] || st.stage),
      h("span", { class: "detail" }, st.detail || ""))));
    clear(progress, h("div", { class: "muted" }, "Starting…"));
    const ids = catalogs.filter((c) => draft.selected.has(c.id)).map((c) => c.id);
    let failed = null;
    try {
      await compileStream({ prompt: draft.prompt, catalogIds: ids }, (event, data) => {
        if (event === "stage") { stages.set(data.stage, data); paint(); }
        else if (event === "result") { draft.plan = data.plan; draft.trace = data.trace; draft.catalogIds = ids; }
        else if (event === "error") failed = data.message;
      });
      if (!draft.plan && !failed) failed = "The connection closed before the plan finished. Try again.";
    } catch (err) {
      failed = errorText(err);
    }
    busy = false; sync();
    if (!view.live(token)) return;
    if (failed) {
      clear(result, h("div", { class: "notice bad" }, h("strong", {}, "This prompt did not compile. "), failed));
      return;
    }
    progress.classList.add("hidden");
    const secs = Math.round((Date.now() - started) / 1000);
    await showPlan(result, token, secs);
  }

  main.append(
    h("div", { class: "head" }, h("div", {},
      h("h1", {}, "Say what should happen"),
      h("p", { class: "sub" }, "You get a plan built only from your real tools. Nothing runs until you have looked at it."))),
    h("div", { class: "stack" },
      !status.models.ready && h("div", { class: "notice warn" }, status.models.problem),
      status.demoAvailable && !hasDemo && flows.workflows.length === 0 && demoCard(),
      h("div", { class: "panel prompt" }, box,
        h("div", { class: "row" },
          h("div", { class: "row grow" },
            chips.length ? chips : h("span", { class: "muted small" }, "Only built-in steps. ", h("a", { href: "#/tools" }, "Add your tools"), " to use them here.")),
          compileBtn)),
      examples, progress, result));
  sync();
  if (draft.plan) await showPlan(result, token);
}

function demoCard() {
  return h("div", { class: "panel row" },
    h("div", { class: "grow" },
      h("h2", {}, "New here? Start with the sample"),
      h("p", { class: "muted" }, "Adds a small Orders API, a policy tool and a ready-made workflow you can run without a model.")),
    h("button", { class: "primary", onclick: run(installDemo) }, "Add the sample"));
}

async function installDemo() {
  const out = await api("POST", "/v1/demo", {});
  toast("Sample added");
  location.hash = `#/workflows/${out.workflowId}`;
}

async function showPlan(result, token, secs) {
  const plan = draft.plan;
  let index = new Map();
  try { index = await catalogNodes(draft.catalogIds); } catch (err) { toast(errorText(err), true); }
  if (!view.live(token)) return;
  let selected = null;
  const side = h("div", {}, stepHint("Select a step to read or change its settings."));
  const graph = graphView(plan.nodes, (key) => {
    selected = key;
    graph.select(key);
    clear(side, stepEditor(plan.nodes.find((n) => n.key === key), scheduleCheck));
  });
  const problems = h("div", {});
  const inputs = inputsForm(plan.defaultInput || {});
  const name = h("input", { value: plan.name, "aria-label": "Workflow name", oninput: (e) => { plan.name = e.target.value; } });
  const saveBtn = h("button", { class: "primary", onclick: run(save) }, "Save workflow");
  const runBtn = h("button", { onclick: run(runOnce) }, "Run once");
  const dag = () => ({ nodes: plan.nodes, defaultInput: { ...plan.defaultInput, ...inputs.values } });

  let timer;
  function scheduleCheck() {
    clearTimeout(timer);
    timer = setTimeout(check, 350);
  }
  async function check() {
    const out = await api("POST", "/v1/validate", { dag: dag(), catalogIds: draft.catalogIds });
    saveBtn.disabled = runBtn.disabled = !out.valid;
    clear(problems, !out.valid && h("div", { class: "notice bad" }, h("strong", {}, "This edit breaks the plan."),
      h("ul", {}, out.problems.map((p) => h("li", {}, p)))));
  }
  async function save() {
    const wf = await api("POST", "/v1/workflows", {
      name: plan.name, description: plan.description, prompt: draft.prompt,
      nodes: plan.nodes, defaultInput: dag().defaultInput, catalogIds: draft.catalogIds,
    });
    const cron = plan.schedule;
    draft.plan = null; draft.prompt = "";
    location.hash = `#/workflows/${wf.id}` + (cron ? `?cron=${encodeURIComponent(cron)}` : "");
  }
  async function runOnce() {
    const r = await api("POST", "/v1/runs", { dag: dag(), catalogIds: draft.catalogIds, input: inputs.values, name: plan.name });
    location.hash = `#/runs/${r.id}`;
  }

  const trace = draft.trace;
  clear(result,
    h("div", { class: "panel row" },
      h("div", { class: "grow" }, name),
      runBtn, saveBtn),
    h("p", { class: "muted small" },
      `${plan.nodes.length} steps, checked against your tools`,
      secs ? ` in ${secs}s` : "",
      trace?.repairs ? `, repaired ${trace.repairs} time${trace.repairs === 1 ? "" : "s"}` : "",
      trace?.models?.reduce ? `, written by ${trace.models.reduce}` : "",
      "."),
    reviewPanel(plan.nodes, index),
    h("div", { class: "workbench" }, graph.el, h("div", { class: "stack" }, side, problems)),
    inputs.hasFields && h("div", { class: "panel stack" }, h("h2", {}, "Inputs"), inputs.el),
    plan.schedule && h("p", { class: "muted small" }, `Your prompt mentions a schedule (${plan.schedule}, UTC). Save the workflow to turn it on.`));
}

/* --------------------------------- workflows -------------------------------- */

async function pageWorkflows(main) {
  const token = view.token;
  const [{ workflows }, status] = await Promise.all([api("GET", "/v1/workflows"), api("GET", "/v1/status")]);
  if (!view.live(token)) return;
  main.append(h("div", { class: "head" },
    h("div", {}, h("h1", {}, "Workflows"), h("p", { class: "sub" }, "Saved plans. Run them by hand, on a schedule, or from another system.")),
    h("a", { href: "#/" }, h("button", { class: "primary" }, "New workflow"))));
  if (workflows.length === 0) {
    main.append(h("div", { class: "panel empty" },
      h("h2", {}, "No workflows yet"),
      h("p", {}, "Describe one on the Build page and save it."),
      status.demoAvailable && h("button", { class: "primary", onclick: run(installDemo) }, "Add the sample")));
    return;
  }
  main.append(h("div", { class: "panel scrollx" }, h("table", { class: "table" },
    h("thead", {}, h("tr", {}, ["Name", "Steps", "Triggers", "Last run"].map((t) => h("th", {}, t)))),
    h("tbody", {}, workflows.map((w) => h("tr", { class: "link", onclick: () => { location.hash = `#/workflows/${w.id}`; } },
      h("td", { class: "name" }, h("a", { href: `#/workflows/${w.id}`, onclick: (e) => e.stopPropagation() }, w.name)),
      h("td", { class: "muted" }, w.nodes.length),
      h("td", {}, w.webhookPath ? h("span", { class: "tag" }, "Webhook") : h("span", { class: "muted small" }, "Manual")),
      h("td", {}, w.lastRun ? h("span", { class: "row" }, pill(w.lastRun.status), h("span", { class: "muted small" }, ago(w.lastRun.createdAt))) : h("span", { class: "muted small" }, "Never"))))))));
}

const CRON_PRESETS = [
  ["0 * * * *", "Every hour"],
  ["0 9 * * *", "Every day at 09:00"],
  ["0 9 * * 1-5", "Weekdays at 09:00"],
  ["0 9 * * 1", "Mondays at 09:00"],
];

async function pageWorkflow(main, id, params) {
  const token = view.token;
  const [wf, { runs }] = await Promise.all([api("GET", `/v1/workflows/${id}`), api("GET", `/v1/runs?workflowId=${id}&limit=10`)]);
  if (!view.live(token)) return;
  let index = new Map();
  try { index = await catalogNodes(wf.catalogIds); } catch (err) { toast(errorText(err), true); }
  if (!view.live(token)) return;

  const side = h("div", {}, stepHint("Select a step to read or change its settings."));
  const graph = graphView(wf.nodes, (key) => {
    graph.select(key);
    clear(side, stepEditor(wf.nodes.find((n) => n.key === key), edited));
  });
  const problems = h("div", {});
  const saveBtn = h("button", { class: "hidden primary", onclick: run(saveEdits) }, "Save changes");
  const inputs = inputsForm(wf.defaultInput || {});
  const runBtn = h("button", { class: "primary", onclick: run(start) }, "Run now");

  let timer;
  function edited() {
    saveBtn.classList.remove("hidden");
    runBtn.disabled = true;
    clearTimeout(timer);
    timer = setTimeout(async () => {
      const out = await api("POST", "/v1/validate", { dag: { nodes: wf.nodes, defaultInput: wf.defaultInput }, catalogIds: wf.catalogIds });
      saveBtn.disabled = !out.valid;
      clear(problems, !out.valid && h("div", { class: "notice bad" }, h("strong", {}, "This edit breaks the workflow."),
        h("ul", {}, out.problems.map((p) => h("li", {}, p)))));
    }, 350);
  }
  async function saveEdits() {
    await api("PUT", `/v1/workflows/${id}`, {
      name: wf.name, description: wf.description, prompt: wf.prompt,
      nodes: wf.nodes, defaultInput: wf.defaultInput, catalogIds: wf.catalogIds, callbackUrl: wf.callbackUrl,
    });
    toast("Saved");
    saveBtn.classList.add("hidden");
    runBtn.disabled = false;
  }
  async function start() {
    const r = await api("POST", `/v1/workflows/${id}/runs`, { input: inputs.values });
    location.hash = `#/runs/${r.id}`;
  }
  async function remove() {
    if (await confirmDialog("Delete this workflow?", "Its schedules and webhook stop. Past runs stay in history.", "Delete")) {
      await api("DELETE", `/v1/workflows/${id}`);
      location.hash = "#/workflows";
    }
  }

  main.append(
    h("div", { class: "head" },
      h("div", { class: "grow" }, h("h1", {}, wf.name), wf.description && h("p", { class: "sub" }, wf.description)),
      h("div", { class: "row" }, saveBtn, runBtn)),
    h("div", { class: "stack" },
      reviewPanel(wf.nodes, index),
      h("div", { class: "workbench" }, graph.el, h("div", { class: "stack" }, side, problems)),
      inputs.hasFields && h("div", { class: "panel stack" }, h("h2", {}, "Inputs for the next run"), inputs.el),
      webhookPanel(wf),
      schedulePanel(wf, params.get("cron")),
      recentRuns(runs),
      wf.prompt && h("div", { class: "panel stack" }, h("h2", {}, "Original prompt"), h("p", { class: "muted" }, wf.prompt)),
      h("div", {}, h("button", { class: "danger", onclick: run(remove) }, "Delete workflow"))));
}

function webhookPanel(wf) {
  const panel = h("div", { class: "panel stack" });
  const paint = () => {
    if (!wf.webhookPath) {
      clear(panel, h("div", { class: "row" },
        h("div", { class: "grow" }, h("h2", {}, "Webhook"), h("p", { class: "muted" }, "Let another system start this workflow with an HTTP POST. Off until you turn it on.")),
        h("button", { onclick: run(turnOn) }, "Turn on")));
      return;
    }
    const url = location.origin + wf.webhookPath;
    const example = `curl -X POST ${url} \\\n  -H 'Content-Type: application/json' \\\n  -d '{"example":"value"}'`;
    clear(panel,
      h("h2", {}, "Webhook"),
      h("p", { class: "muted" }, "The JSON body becomes the run's input. Anyone with this URL can start the workflow, so treat it like a password."),
      h("div", { class: "row" },
        h("input", { class: "mono grow", readOnly: true, value: url, "aria-label": "Webhook URL", onfocus: (e) => e.target.select() }),
        h("button", { onclick: () => copy(url) }, "Copy")),
      h("pre", {}, example),
      h("div", { class: "row" },
        h("button", { onclick: run(turnOn) }, "Issue a new URL"),
        h("button", { class: "danger", onclick: run(turnOff) }, "Turn off")));
  };
  async function turnOn() {
    wf.webhookPath = (await api("PUT", `/v1/workflows/${wf.id}/webhook`)).webhookPath;
    paint();
  }
  async function turnOff() {
    await api("DELETE", `/v1/workflows/${wf.id}/webhook`);
    wf.webhookPath = null;
    paint();
  }
  paint();
  return panel;
}

function schedulePanel(wf, suggested) {
  const panel = h("div", { class: "panel stack" });
  const cron = h("input", { class: "mono", value: suggested || "", placeholder: "0 9 * * 1-5", "aria-label": "Cron expression" });
  const paint = () => clear(panel,
    h("div", {}, h("h2", {}, "Schedule"), h("p", { class: "muted" }, "Times are UTC. Each tick runs the latest saved version of this workflow.")),
    wf.schedules.map((sc) => h("div", { class: "row" },
      h("code", { class: "grow" }, sc.cron),
      h("span", { class: "muted small" }, `added ${ago(sc.createdAt)}`),
      h("button", { class: "danger", onclick: run(() => remove(sc.id)) }, "Remove"))),
    h("div", { class: "row" },
      h("select", { class: "grow", "aria-label": "Common schedules", onchange: (e) => { if (e.target.value) cron.value = e.target.value; } },
        h("option", { value: "" }, "Common schedules"),
        CRON_PRESETS.map(([value, label]) => h("option", { value }, label))),
      h("div", { class: "grow" }, cron),
      h("button", { onclick: run(add) }, "Add schedule")));
  async function add() {
    if (!cron.value.trim()) { toast("Choose a schedule or type a cron expression", true); return; }
    const sc = await api("POST", "/v1/schedules", { cron: cron.value, workflowId: wf.id });
    wf.schedules = [sc, ...wf.schedules];
    cron.value = "";
    paint();
    toast("Schedule added");
  }
  async function remove(id) {
    await api("DELETE", `/v1/schedules/${id}`);
    wf.schedules = wf.schedules.filter((sc) => sc.id !== id);
    paint();
  }
  paint();
  return panel;
}

function runRows(runs) {
  return runs.map((r) => h("tr", { class: "link", onclick: () => { location.hash = `#/runs/${r.id}`; } },
    h("td", { class: "name" }, h("a", { href: `#/runs/${r.id}`, onclick: (e) => e.stopPropagation() }, r.name || "Untitled run")),
    h("td", {}, pill(r.status)),
    h("td", {}, h("span", { class: "tag" }, SOURCE[r.source] || r.source)),
    h("td", { class: "muted small", title: new Date(r.createdAt).toLocaleString() }, ago(r.createdAt)),
    h("td", { class: "muted small" }, duration(r.createdAt, r.finishedAt))));
}

const RUN_HEADERS = ["Run", "Status", "Started by", "When", "Took"];

function recentRuns(runs) {
  if (!runs.length) return null;
  return h("div", { class: "panel scrollx" }, h("h2", {}, "Recent runs"), h("table", { class: "table" },
    h("thead", {}, h("tr", {}, RUN_HEADERS.map((t) => h("th", {}, t)))),
    h("tbody", {}, runRows(runs))));
}

/* ----------------------------------- runs ----------------------------------- */

async function pageRuns(main) {
  const token = view.token;
  let filter = "";
  const body = h("div", {});
  const tabs = h("div", { class: "segments" });
  async function load() {
    const { runs } = await api("GET", `/v1/runs?limit=100${filter ? `&status=${filter}` : ""}`);
    if (!view.live(token)) return;
    clear(body, runs.length === 0
      ? h("div", { class: "panel empty" }, h("h2", {}, filter ? "Nothing is waiting on you" : "No runs yet"), h("p", {}, filter ? "Runs that need a decision show up here." : "Run a workflow and watch it here."))
      : h("div", { class: "panel scrollx" }, h("table", { class: "table" },
          h("thead", {}, h("tr", {}, RUN_HEADERS.map((t) => h("th", {}, t)))),
          h("tbody", {}, runRows(runs)))));
  }
  const paintTabs = () => clear(tabs, [["", "All"], ["waiting_approval", "Needs approval"], ["failed", "Failed"]].map(([value, label]) =>
    h("button", { class: filter === value ? "on" : "", onclick: run(async () => { filter = value; paintTabs(); await load(); }) }, label)));
  paintTabs();
  main.append(h("div", { class: "head" }, h("div", {}, h("h1", {}, "Runs"), h("p", { class: "sub" }, "Everything that has executed, and what it did.")), tabs), body);
  await load();
  view.every(3000, load);
}

async function pageRun(main, id) {
  const token = view.token;
  let current = await api("GET", `/v1/runs/${id}`);
  if (!view.live(token)) return;
  const nodes = current.dag;
  let selected = null, shown = "";

  const title = h("h1", {}, current.name || "Untitled run");
  const statusEl = h("div", { class: "row" });
  const actions = h("div", { class: "row" });
  const errorEl = h("div", {});
  const approvals = h("div", { class: "stack" });
  const side = h("div", {}, stepHint("Select a step to see what it did."));
  const graph = graphView(nodes, (key) => { selected = key; shown = ""; graph.select(key); paintStep(); });
  let approvalKey = "";

  function paintStep() {
    if (!selected) return;
    const node = current.nodes.find((n) => n.key === selected);
    const spec = nodes.find((n) => n.key === selected);
    const snapshot = JSON.stringify(node);
    if (snapshot === shown) return;
    shown = snapshot;
    const looped = insideLoop(nodes, selected); // there, attempts counts every item
    clear(side, h("div", { class: "panel inspector" },
      h("div", {}, h("h2", {}, node.name || node.key), h("div", { class: "key mono" }, node.type)),
      h("div", { class: "row" }, pill(node.status),
        !looped && node.attempts > 1 && h("span", { class: "muted small" }, `${node.attempts} attempts`),
        node.startedAt && h("span", { class: "muted small" }, duration(node.startedAt, node.finishedAt))),
      node.reused && h("p", { class: "muted small" }, "Carried over from the run this one continues. It was not run again."),
      node.error && h("div", { class: "notice bad small" }, node.error),
      looped && h("p", { class: "muted small" },
        `Inside a loop: ${node.itemsSucceeded || 0} item(s) succeeded, ${node.itemsFailed || 0} failed. The output below is the latest item.`),
      looped && node.itemsFailed > 0 && failedItems(node.key),
      node.output !== null && node.output !== undefined && h("div", {}, h("h3", {}, "Output"), h("pre", {}, pretty(node.output))),
      Object.keys(spec?.config || {}).length > 0 && h("div", {}, h("h3", {}, "Settings"), h("pre", {}, pretty(spec.config)))));
  }

  // The items of a step that failed, fetched when asked for.
  function failedItems(key) {
    const box = h("div", { class: "stack" });
    const show = async () => {
      const { items } = await api("GET", `/v1/runs/${id}/nodes/${encodeURIComponent(key)}/items?status=failed&limit=50`);
      clear(box, items.map((it) => h("div", { class: "notice bad small" }, h("strong", {}, `Item ${it.index + 1}: `), it.error || "failed")));
    };
    clear(box, h("button", { class: "link", onclick: run(show) }, "Show the items that failed"));
    return box;
  }

  async function decide(key, approved, note) {
    await api("POST", `/v1/runs/${id}/nodes/${encodeURIComponent(key)}/approve`, { approved, note });
    toast(approved ? "Approved" : "Rejected");
    approvalKey = "";
    await refresh();
  }

  function paintApprovals() {
    const waiting = current.nodes.filter((n) => n.status === "waiting_approval");
    const key = JSON.stringify(waiting.map((n) => [n.key, n.output]));
    if (key === approvalKey) return;
    approvalKey = key; // so a half-typed note survives the next poll
    clear(approvals, waiting.map((n) => {
      const note = h("input", { placeholder: "Note (optional)", "aria-label": "Note" });
      return h("div", { class: "panel approval" },
        h("div", { class: "muted small" }, n.name),
        h("div", { class: "message" }, n.output?.message || "This step needs your decision."),
        h("div", { class: "row" },
          h("div", { class: "grow" }, note),
          h("button", { class: "primary", onclick: run(() => decide(n.key, true, note.value)) }, "Approve"),
          h("button", { onclick: run(() => decide(n.key, false, note.value)) }, "Reject")));
    }));
  }

  function paint() {
    clear(statusEl, pill(current.status),
      h("span", { class: "tag" }, SOURCE[current.source] || current.source),
      h("span", { class: "muted small" }, `${ago(current.createdAt)}, ${duration(current.createdAt, current.finishedAt)}`),
      current.startedBy && h("span", { class: "muted small" }, `started by ${current.startedBy}`),
      current.resumeOf && h("a", { class: "small", href: `#/runs/${current.resumeOf}` }, "Continues an earlier run"),
      current.workflowId && h("a", { class: "small", href: `#/workflows/${current.workflowId}` }, "Open workflow"));
    const active = !current.finishedAt;
    const resumable = current.status === "failed" || current.status === "cancelled";
    clear(actions,
      active && h("button", { class: "danger", onclick: run(cancel) }, "Stop run"),
      resumable && h("button", { class: "primary", onclick: run(resume), title: "Steps and loop items that succeeded are not run again" }, "Continue from here"),
      !active && h("button", { onclick: run(again) }, "Run again from the start"));
    clear(errorEl,
      current.error && h("div", { class: "notice bad" }, h("strong", {}, "This run failed. "), current.error),
      resumable && h("p", { class: "muted small" }, "Continue from here picks up at the step that failed, without repeating what already succeeded."),
      current.status === "succeeded" && current.failedItems > 0 && h("div", { class: "notice bad" },
        h("strong", {}, `${current.failedItems} loop item(s) failed. `), "The loop was set to carry on. Select a red step to see which."));
    graph.setStatuses(new Map(current.nodes.map((n) => [n.key, n.status])));
    paintApprovals();
    if (!selected) {
      const focus = current.nodes.find((n) => n.status === "failed");
      if (focus) { selected = focus.key; graph.select(selected); }
    }
    paintStep();
  }

  async function refresh() {
    const next = await api("GET", `/v1/runs/${id}`);
    if (!view.live(token)) return;
    current = next;
    paint();
    refreshBadge();
  }
  async function cancel() {
    if (await confirmDialog("Stop this run?", "Steps already finished stay done. The step in progress is cancelled.", "Stop run")) {
      await api("POST", `/v1/runs/${id}/cancel`);
      toast("Stopping");
      await refresh();
    }
  }
  async function resume() {
    const next = await api("POST", `/v1/runs/${id}/retry`);
    location.hash = `#/runs/${next.id}`;
  }
  async function again() {
    const r = current.workflowId
      ? await api("POST", `/v1/workflows/${current.workflowId}/runs`, { input: current.input }).catch((err) => {
          if (err.status === 404) return null;
          throw err;
        })
      : null;
    const next = r || await api("POST", "/v1/runs", { dag: { nodes: current.dag }, catalogIds: current.catalogIds, input: current.input, name: current.name });
    location.hash = `#/runs/${next.id}`;
  }

  main.append(
    h("div", { class: "head" }, h("div", { class: "grow" }, title, statusEl), actions),
    h("div", { class: "stack" }, approvals, errorEl, h("div", { class: "workbench" }, graph.el, side)));
  paint();
  view.every(1200, async () => { if (!current.finishedAt) await refresh(); });
}

/* ----------------------------------- tools ---------------------------------- */

async function pageTools(main) {
  const token = view.token;
  const [{ catalogs }, status] = await Promise.all([api("GET", "/v1/catalogs"), api("GET", "/v1/status")]);
  if (!view.live(token)) return;
  const reload = () => { catalogCache.clear(); render(); };

  main.append(h("div", { class: "head" }, h("div", {},
    h("h1", {}, "Tools"),
    h("p", { class: "sub" }, "Anything here can be used in a plan, and nothing else can. Add an OpenAPI spec, an MCP server or an Activepieces piece."))));
  const list = h("div", { class: "stack" },
    catalogs.length === 0 && h("div", { class: "panel empty" },
      h("h2", {}, "No tools yet"),
      h("p", {}, "Add one below, or use the sample."),
      status.demoAvailable && h("button", { class: "primary", onclick: run(installDemo) }, "Add the sample")),
    catalogs.map((c) => toolCard(c, reload)),
    addToolForm(reload, status.piecesAvailable));
  main.append(list);
}

function toolCard(c, reload) {
  const ops = h("div", { class: "ops hidden" });
  const toggle = h("button", { class: "link", onclick: run(showOps) }, `${c.nodeCount} operations`);
  let loaded = false;
  async function showOps() {
    if (!loaded) {
      const full = await api("GET", `/v1/catalogs/${c.id}`);
      clear(ops, full.nodes.map((n) => h("div", { class: "op" },
        h("code", {}, n.exec?.method ? `${n.exec.method} ${n.exec.path}` : n.id),
        n.description && h("span", { class: "desc" }, clip(n.description, 180)))));
      loaded = true;
    }
    ops.classList.toggle("hidden");
  }
  const credForm = h("div", { class: "hidden stack" });
  const header = h("input", { value: "Authorization", class: "mono", "aria-label": "Header name" });
  const value = h("input", { type: "password", class: "mono", placeholder: "Bearer ...", autocomplete: "off", "aria-label": "Header value" });
  clear(credForm,
    h("div", { class: "row" },
      h("div", { class: "grow" }, header),
      h("div", { class: "grow" }, value),
      h("button", { class: "primary", onclick: run(saveCred) }, "Save")),
    h("p", { class: "muted small" }, "Encrypted at rest and only ever sent to this tool's own address. It cannot be read back."));
  const isPiece = c.kind === "piece";
  if (isPiece) {
    header.classList.add("hidden");
    value.placeholder = "API key, or the connection as JSON";
  }
  async function saveCred() {
    if (isPiece) {
      if (!value.value) { toast("Enter the connection value", true); return; }
      await api("PUT", `/v1/catalogs/${c.id}/credentials`, { piece: jsonOrText(value.value) });
    } else {
      if (!header.value.trim() || !value.value) { toast("Enter a header name and a value", true); return; }
      await api("PUT", `/v1/catalogs/${c.id}/credentials`, { headers: { [header.value.trim()]: value.value } });
    }
    toast("Credential saved");
    reload();
  }
  const authLabel = {
    headers: `Using ${(c.credentialHeaders || []).join(", ") || "stored credentials"}`,
    oauth2: "OAuth connected",
    oauth2_pending: "OAuth: waiting for consent",
    piece: "Connection saved",
  }[c.auth];
  async function remove() {
    if (await confirmDialog(`Remove ${c.name}?`, "Workflows that use it will fail when they run.", "Remove")) {
      await api("DELETE", `/v1/catalogs/${c.id}`);
      reload();
    }
  }
  return h("div", { class: "panel" },
    h("div", { class: "row" },
      h("div", { class: "grow" },
        h("div", { class: "row" }, h("h2", {}, c.name), h("span", { class: "tag" }, { mcp: "MCP", piece: "Piece" }[c.kind] || "OpenAPI")),
        h("div", { class: "muted small mono" }, c.baseUrl)),
      authLabel
        ? h("span", { class: "pill " + (c.auth === "oauth2_pending" ? "waiting_approval" : "succeeded") }, authLabel)
        : h("span", { class: "pill" }, "No credential"),
      h("button", { onclick: () => credForm.classList.toggle("hidden") }, c.hasCredentials ? "Replace credential" : "Add credential"),
      h("button", { class: "danger", onclick: run(remove) }, "Remove")),
    h("div", { class: "row small" }, toggle),
    credForm, ops);
}

// A connection may be a bare secret or a JSON object, depending on the piece.
function jsonOrText(v) {
  const t = v.trim();
  if (t.startsWith("{") || t.startsWith("[")) {
    try { return JSON.parse(t); } catch { /* a secret that happens to start with a brace */ }
  }
  return v;
}

function addToolForm(reload, piecesAvailable) {
  let mode = "openapi-url";
  const name = h("input", { placeholder: "Stripe, Internal Orders, ...", "aria-label": "Name" });
  const url = h("input", { class: "mono", placeholder: "https://api.example.com/openapi.json", "aria-label": "URL" });
  const spec = h("textarea", { class: "mono", rows: 8, placeholder: "Paste an OpenAPI 3 document (JSON or YAML)", spellcheck: false, "aria-label": "OpenAPI document" });
  const base = h("input", { class: "mono", placeholder: "Taken from the spec if left empty", "aria-label": "Base URL" });
  const auth = h("input", { type: "password", class: "mono", placeholder: "Bearer ... (optional)", autocomplete: "off", "aria-label": "Authorization header" });
  const urlField = h("label", { class: "field" }, h("span", {}, "URL"), url,
    h("span", { class: "hint" }, "The engine runs in Docker. To reach something on this computer, use host.docker.internal instead of localhost."));
  const specField = h("label", { class: "field" }, "Spec", spec);
  const baseField = h("label", { class: "field" }, "Base URL", base);
  const authField = h("label", { class: "field" }, "Authorization header", auth);
  const pieceNames = h("datalist", { id: "piece-names" });
  const piece = h("input", { class: "mono", placeholder: "slack, github, google-sheets, ...", "aria-label": "Piece" });
  piece.setAttribute("list", "piece-names"); // "list" is read-only as a property
  const pieceAuth = h("input", { type: "password", class: "mono", placeholder: "API key, or the connection as JSON (optional)", autocomplete: "off", "aria-label": "Connection" });
  const pieceField = h("label", { class: "field" }, h("span", {}, "Piece"), piece, pieceNames,
    h("span", { class: "hint" }, "An Activepieces piece installed on this engine's pieces runner. Its actions become tools."));
  const pieceAuthField = h("label", { class: "field" }, h("span", {}, "Connection"), pieceAuth,
    h("span", { class: "hint" }, "What the piece needs to sign in. For a piece that uses OAuth, add it without one, then connect it with apexctl tools oauth."));
  let piecesLoaded = false;
  async function loadPieces() {
    if (piecesLoaded) return;
    piecesLoaded = true;
    const { pieces } = await api("GET", "/v1/pieces");
    clear(pieceNames, pieces.map((p) => h("option", { value: p.name }, p.displayName)));
  }
  const modes = [["openapi-url", "OpenAPI URL"], ["openapi-spec", "Paste OpenAPI"], ["mcp", "MCP server"]];
  if (piecesAvailable) modes.push(["piece", "Piece"]);
  const seg = h("div", { class: "segments" });
  const paint = () => {
    clear(seg, modes.map(([value, label]) =>
      h("button", { class: mode === value ? "on" : "", onclick: () => { mode = value; paint(); } }, label)));
    urlField.classList.toggle("hidden", mode === "openapi-spec" || mode === "piece");
    specField.classList.toggle("hidden", mode !== "openapi-spec");
    baseField.classList.toggle("hidden", mode === "mcp" || mode === "piece");
    authField.classList.toggle("hidden", mode !== "mcp");
    pieceField.classList.toggle("hidden", mode !== "piece");
    pieceAuthField.classList.toggle("hidden", mode !== "piece");
    if (mode === "piece") run(loadPieces)();
    url.placeholder = mode === "mcp" ? "https://example.com/mcp" : "https://api.example.com/openapi.json";
  };
  async function add() {
    const source = mode === "piece"
      ? { type: "piece", piece: piece.value.trim(), ...(pieceAuth.value && { auth: jsonOrText(pieceAuth.value) }) }
      : mode === "mcp"
      ? { type: "mcp", url: url.value.trim(), ...(auth.value && { headers: { Authorization: auth.value } }) }
      : mode === "openapi-url" ? { type: "openapi", url: url.value.trim() } : { type: "openapi", spec: spec.value };
    await api("POST", "/v1/catalogs", { name: name.value, source, ...(base.value.trim() && mode.startsWith("openapi") && { baseUrl: base.value.trim() }) });
    toast("Tool added");
    reload();
  }
  paint();
  return h("div", { class: "panel stack" },
    h("div", { class: "row" }, h("h2", { class: "grow" }, "Add a tool"), seg),
    h("label", { class: "field" }, "Name", name),
    urlField, specField, baseField, authField, pieceField, pieceAuthField,
    h("div", {}, h("button", { class: "primary", onclick: run(add) }, "Add tool")));
}

/* ---------------------------------- routing --------------------------------- */

let badgeEl = null;

async function refreshBadge() {
  if (!badgeEl) return;
  try {
    const { runs } = await api("GET", "/v1/runs?status=waiting_approval&limit=100");
    badgeEl.textContent = runs.length;
    badgeEl.classList.toggle("hidden", runs.length === 0);
  } catch { /* the next tick retries */ }
}

function shell() {
  badgeEl = h("span", { class: "badge hidden", title: "Runs waiting on you" });
  const links = [["#/", "Build", "build"], ["#/workflows", "Workflows", "workflows"], ["#/runs", "Runs", "runs"], ["#/tools", "Tools", "tools"]];
  const nav = h("nav", { class: "nav" }, links.map(([href, label, id]) =>
    h("a", { href, "data-page": id }, label, id === "runs" && badgeEl)));
  const main = h("main", {});
  clear(root,
    h("header", { class: "top" },
      h("a", { class: "brand", href: "#/" }, h("img", { src: "favicon.svg", alt: "" }), h("span", {}, "apex-engine")),
      nav,
      h("button", { class: "link", onclick: signOut }, "Sign out")),
    main);
  return { main, nav };
}

let frame = null;

async function route() {
  if (!apiKey) return renderSignIn();
  frame ||= shell();
  view.stop();
  const { main, nav } = frame;
  clear(main);
  const [path, query] = location.hash.replace(/^#\/?/, "").split("?");
  const params = new URLSearchParams(query || "");
  const [page, id] = path.split("/");
  nav.querySelectorAll("a").forEach((a) => a.classList.toggle("on", a.dataset.page === (page || "build")));
  window.scrollTo(0, 0);
  try {
    if (page === "workflows" && id) await pageWorkflow(main, id, params);
    else if (page === "workflows") await pageWorkflows(main);
    else if (page === "runs" && id) await pageRun(main, id);
    else if (page === "runs") await pageRuns(main);
    else if (page === "tools") await pageTools(main);
    else await pageBuild(main);
  } catch (err) {
    if (err.status === 401) return;
    clear(main, h("div", { class: "panel empty" },
      h("h2", {}, err.status === 404 ? "Not found" : "Something went wrong"),
      h("p", {}, err.status === 404 ? "It may have been deleted." : errorText(err)),
      h("a", { href: "#/" }, h("button", {}, "Back to Build"))));
  }
  refreshBadge();
  view.every(8000, refreshBadge);
}

function renderSignIn() {
  frame = null;
  badgeEl = null;
  const key = h("input", { type: "password", class: "mono", autocomplete: "off", "aria-label": "API key", placeholder: "API key" });
  const error = h("div", {});
  async function submit() {
    apiKey = key.value.trim();
    const resp = await fetch("/v1/status", { headers: { Authorization: `Bearer ${apiKey}` } });
    if (!resp.ok) {
      apiKey = null;
      clear(error, h("div", { class: "notice bad" }, "That key was not accepted."));
      return;
    }
    storeKey(apiKey);
    route();
  }
  key.addEventListener("keydown", (e) => { if (e.key === "Enter") run(submit)(); });
  clear(root, h("div", { class: "signin" }, h("div", { class: "panel" },
    h("div", { class: "row" }, h("img", { src: "favicon.svg", alt: "", width: 26, height: 26 }), h("h1", {}, "apex-engine")),
    h("p", { class: "muted" }, "Enter your API key. On your own machine, ", h("code", {}, "./apex console"), " signs you in."),
    key, error,
    h("button", { class: "primary", onclick: run(submit) }, "Sign in"))));
  key.focus();
}

// `./apex console` puts the key in the fragment, which browsers never send to a server.
const fromLink = new URLSearchParams(location.hash.replace(/^#/, "")).get("key");
if (fromLink) {
  apiKey = fromLink;
  storeKey(fromLink);
  history.replaceState(null, "", location.pathname + "#/");
}

window.addEventListener("hashchange", route);
route();
