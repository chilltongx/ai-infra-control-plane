const $ = (q, root = document) => root.querySelector(q);
const $$ = (q, root = document) => [...root.querySelectorAll(q)];
const state = { experiments: [], runs: [], nodes: [], selected: null };
const apiTokenKey = "controlPlaneApiToken";
let authPrompt = null;
let inspectorTrigger = null;
let inspectorRunID = "";

async function api(path, options = {}, retryAuth = true) {
  const headers = new Headers(options.headers || {});
  if (!headers.has("Content-Type")) headers.set("Content-Type", "application/json");
  if (path === "/metrics" || path.startsWith("/v1/")) {
    const token = sessionStorage.getItem(apiTokenKey);
    if (token) headers.set("Authorization", `Bearer ${token}`);
  }
  const response = await fetch(path, { ...options, headers });
  if (response.status === 204) return null;
  const body = await response.json().catch(() => ({}));
  if (response.status === 401) {
    sessionStorage.removeItem(apiTokenKey);
    if (retryAuth) {
      await requestAPIToken(body?.error?.message);
      return api(path, options, false);
    }
  }
  if (!response.ok) throw new Error(body?.error?.message || `${response.status} ${response.statusText}`);
  return body;
}
function requestAPIToken(message = "") {
  if (authPrompt) return authPrompt;
  const dialog = $("#auth-dialog"); const form = $("#auth-form"); const input = $("#api-token"); const error = $("#auth-form-error");
  error.textContent = message; input.value = ""; dialog.showModal(); queueMicrotask(() => input.focus());
  authPrompt = new Promise((resolve, reject) => {
    const finish = (token, failure) => {
      form.removeEventListener("submit", submit); dialog.removeEventListener("cancel", cancel); $("#cancel-auth-dialog").removeEventListener("click", cancel); $("#close-auth-dialog").removeEventListener("click", cancel);
      authPrompt = null; dialog.close();
      if (failure) reject(failure); else resolve(token);
    };
    const submit = event => { event.preventDefault(); const token = input.value.trim(); if (!token) { error.textContent = "API token is required."; input.focus(); return; } sessionStorage.setItem(apiTokenKey, token); finish(token); };
    const cancel = event => { event.preventDefault(); finish("", new Error("Authentication cancelled")); };
    form.addEventListener("submit", submit); dialog.addEventListener("cancel", cancel); $("#cancel-auth-dialog").addEventListener("click", cancel); $("#close-auth-dialog").addEventListener("click", cancel);
  });
  return authPrompt;
}
function esc(value = "") { const n = document.createElement("span"); n.textContent = String(value); return n.innerHTML; }
function ago(value) { const timestamp = new Date(value).getTime(); if (!value || Number.isNaN(timestamp)) return "Never"; const seconds = Math.max(0, (Date.now() - timestamp) / 1000); if (seconds < 60) return `${Math.floor(seconds)}s ago`; if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`; if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`; return new Date(timestamp).toLocaleDateString(); }
function shortID(value = "") { return value ? String(value).slice(0, 12) : "—"; }
function gib(bytes = 0) { return Number(bytes || 0) / (1024 ** 3); }
function formatGiB(bytes = 0) { const value = gib(bytes); return `${value >= 10 ? value.toFixed(0) : value.toFixed(1)} GiB`; }
function formatGPURequest(requirements = {}) {
  const count = Number(requirements.gpu_count || 0);
  if (!count) return "Unconstrained";
  const memory = Number(requirements.min_free_gpu_memory_bytes || 0);
  return `${count} GPU${count === 1 ? "" : "s"}${memory ? ` · ${formatGiB(memory)} free` : ""}`;
}
function formatAllocation(allocation) {
  if (!allocation) return "Awaiting placement";
  const gpuIDs = allocation.gpu_ids || (allocation.gpus || []).map(gpu => gpu.id || `GPU ${gpu.index}`);
  const detail = gpuIDs.length ? ` · ${gpuIDs.join(", ")}` : " · unconstrained";
  return `${shortID(allocation.worker_id)}${detail}`;
}
function toast(message) { const el = $("#toast"); el.textContent = message; el.classList.add("show"); clearTimeout(toast.timer); toast.timer = setTimeout(() => el.classList.remove("show"), 2400); }

async function refresh() {
  $("#refresh").disabled = true;
  try {
    await api("/healthz"); $("#health").classList.add("ok"); $("#health").lastChild.textContent = " Ready";
    const [experiments, nodes] = await Promise.all([api("/v1/experiments?limit=100"), api("/v1/workers?limit=100")]);
    state.experiments = experiments.items || []; state.nodes = nodes.items || [];
    const lists = await Promise.all(state.experiments.map(e => api(`/v1/experiments/${encodeURIComponent(e.id)}/runs?limit=100`).catch(() => ({ items: [] }))));
    state.runs = lists.flatMap(x => x.items || []).sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at));
    render();
  } catch (error) { $("#health").classList.remove("ok"); $("#health").lastChild.textContent = " Offline"; toast(error.message); }
  finally { $("#refresh").disabled = false; }
}
function render() {
  const counts = state.runs.reduce((a, r) => (a[r.state] = (a[r.state] || 0) + 1, a), {});
  $("#queued-count").textContent = (counts.queued || 0) + (counts.retry_wait || 0);
  $("#active-count").textContent = (counts.active || 0) + (counts.cancel_requested || 0);
  $("#success-count").textContent = counts.succeeded || 0; $("#failed-count").textContent = (counts.failed || 0) + (counts.cancelled || 0);
  $("#node-count").textContent = state.nodes.filter(node => node.state !== "offline").length;
  renderRuns(); renderNodes();
}
function renderRuns() {
  const query = $("#run-filter").value.toLowerCase();
  const runs = state.runs.filter(r => `${r.id} ${r.recipe?.adapter || ""}`.toLowerCase().includes(query));
  $("#run-rows").innerHTML = runs.map(r => `<tr data-run="${esc(r.id)}" tabindex="0" role="button" aria-label="Inspect run ${esc(shortID(r.id))}"><td><strong>${esc(shortID(r.id))}</strong><br><span class="mono muted">${esc(shortID(r.experiment_id))}</span></td><td><span class="mono">${esc(r.recipe?.adapter || "—")}</span></td><td><span class="status ${esc(r.state)}">${esc(r.state)}</span></td><td><span class="resource-summary">${esc(formatGPURequest(r.resource_requirements))}</span></td><td><span class="allocation-summary">${esc(formatAllocation(r.last_allocation))}</span></td><td>${r.attempt_count || 0} / ${r.max_attempts || 1}</td><td class="muted">${ago(r.updated_at)}</td><td aria-hidden="true">›</td></tr>`).join("");
  $("#runs-empty").classList.toggle("hidden", runs.length > 0);
  $$('[data-run]').forEach(row => {
    row.onclick = () => inspect(row.dataset.run, row);
    row.onkeydown = event => {
      if (event.key === "Enter" || event.key === " ") { event.preventDefault(); inspect(row.dataset.run, row); }
    };
  });
}
function renderGPU(gpu) {
  const total = Number(gpu.total_memory_bytes || 0);
  const free = Number(gpu.free_memory_bytes || 0);
  const usedPercent = total > 0 ? Math.max(0, Math.min(100, ((total - free) / total) * 100)) : 0;
  return `<li class="gpu"><div class="gpu-heading"><div><strong>${esc(gpu.name || `GPU ${gpu.index}`)}</strong><small>${esc(gpu.vendor || gpu.id || "GPU")}</small></div><span>${esc(formatGiB(free))} free</span></div><div class="memory-bar" aria-label="${esc(usedPercent.toFixed(0))}% GPU memory used"><i style="width:${usedPercent.toFixed(1)}%"></i></div><div class="gpu-stats"><span>${esc(formatGiB(total))} total</span><span>${Number(gpu.utilization_percent || 0).toFixed(0)}% compute</span>${Number(gpu.temperature_celsius || 0) ? `<span>${Number(gpu.temperature_celsius).toFixed(0)}°C</span>` : ""}</div></li>`;
}
function renderNodes() {
  $("#node-grid").innerHTML = state.nodes.map(node => {
    const resources = node.resources || {};
    const gpus = resources.gpus || [];
    const activeRun = node.active_run_id
      ? `<a class="active-run" href="#" data-inspect-run="${esc(node.active_run_id)}">Run ${esc(shortID(node.active_run_id))}</a>`
      : '<span class="muted">Idle</span>';
    return `<article class="node-card"><header><div><h3>${esc(node.name || node.id)}</h3><p class="mono">${esc(node.id)}</p></div><span class="status ${esc(node.state)}">${esc(node.state)}</span></header><dl class="node-meta"><div><dt>Adapter</dt><dd>${esc(node.adapter || "—")}</dd></div><div><dt>Heartbeat</dt><dd title="${esc(node.last_heartbeat_at || "")}">${esc(ago(node.last_heartbeat_at))}</dd></div><div><dt>Active run</dt><dd>${activeRun}</dd></div><div><dt>GPU probe</dt><dd>${esc(resources.probe_status || "unreported")}</dd></div></dl>${gpus.length ? `<ul class="gpu-list">${gpus.map(renderGPU).join("")}</ul>` : '<p class="no-gpus">No GPUs reported by this node.</p>'}</article>`;
  }).join("");
  $("#nodes-empty").classList.toggle("hidden", state.nodes.length > 0);
  $$('[data-inspect-run]').forEach(link => link.onclick = event => { event.preventDefault(); inspect(link.dataset.inspectRun, link); });
}
async function inspect(id, trigger = document.activeElement) {
  const run = await api(`/v1/runs/${encodeURIComponent(id)}`); state.selected = run;
  const events = await api(`/v1/runs/${encodeURIComponent(id)}/events?limit=100`).catch(() => ({ items: [] }));
  inspectorTrigger = trigger instanceof HTMLElement ? trigger : null;
  inspectorRunID = id;
  $("#inspector-title").textContent = run.id.slice(0,16);
  $("#inspector-body").innerHTML = `<span class="status ${esc(run.state)}">${esc(run.state)}</span><dl><dt>Recipe</dt><dd class="mono">${esc(run.recipe?.adapter || "—")}</dd><dt>Command</dt><dd class="mono">${esc((run.recipe?.command || []).join(" ") || "—")}</dd><dt>Attempt</dt><dd>${run.attempt_count || 0} / ${run.max_attempts || 1}</dd><dt>Required labels</dt><dd class="mono">${esc(JSON.stringify(run.required_labels || {}))}</dd><dt>GPU request</dt><dd>${esc(formatGPURequest(run.resource_requirements))}</dd><dt>Last allocation</dt><dd>${esc(formatAllocation(run.last_allocation))}</dd>${run.last_allocation?.resources_observed_at ? `<dt>Resource snapshot</dt><dd>${esc(ago(run.last_allocation.resources_observed_at))}</dd>` : ""}</dl>${(run.last_allocation?.gpus || []).length ? `<p class="eyebrow inspector-section">ALLOCATED GPUS</p><ul class="gpu-list inspector-gpus">${run.last_allocation.gpus.map(renderGPU).join("")}</ul>` : ""}<p class="eyebrow inspector-section">EVENT STREAM</p>${(events.items || []).map(e => `<div class="event"><strong>${esc(e.type)}</strong><small>${new Date(e.timestamp).toLocaleString()}</small></div>`).join("") || '<p class="muted">No events yet.</p>'}${["queued","active","cancel_requested"].includes(run.state) ? '<button class="danger-button" id="cancel-selected">Cancel run</button>' : ""}`;
  const inspector = $("#inspector");
  inspector.inert = false; inspector.classList.add("open"); inspector.setAttribute("aria-hidden","false");
  queueMicrotask(() => $("#close-inspector").focus());
  const cancel = $("#cancel-selected"); if (cancel) cancel.onclick = async () => { await api(`/v1/runs/${encodeURIComponent(id)}/cancel`, { method:"POST", body:JSON.stringify({ reason:"cancelled from web console" }) }); toast("Cancellation requested"); await refresh(); inspect(id); };
}
function closeInspector() {
  const inspector = $("#inspector");
  inspector.classList.remove("open"); inspector.setAttribute("aria-hidden", "true"); inspector.inert = true;
  const fallbackTriggers = [...$$('[data-inspect-run]'), ...$$('[data-run]')].filter(element => (element.dataset.inspectRun || element.dataset.run) === inspectorRunID);
  const fallbackTrigger = fallbackTriggers.find(element => !element.closest('[hidden]')) || fallbackTriggers[0];
  if (inspectorTrigger?.isConnected) inspectorTrigger.focus();
  else if (fallbackTrigger) fallbackTrigger.focus();
  inspectorTrigger = null;
  inspectorRunID = "";
}
function parseLabels(raw) { return Object.fromEntries(raw.split(",").map(x => x.trim()).filter(Boolean).map(x => { const [k,...v] = x.split("="); return [k.trim(),v.join("=").trim()]; })); }
async function submitRun(event) {
  event.preventDefault(); const form = new FormData(event.currentTarget); const button = $("#submit-run"); button.disabled = true; $("#form-error").textContent = "";
  try {
    const gpuCount = Number(form.get("gpu_count") || 0);
    const minimumMemoryGiB = Number(form.get("min_free_gpu_memory_bytes") || 0);
    if (minimumMemoryGiB > 0 && gpuCount < 1) throw new Error("Choose at least one GPU when minimum free memory is set.");
    const experiment = await api("/v1/experiments", { method:"POST", headers:{"Idempotency-Key":crypto.randomUUID()}, body:JSON.stringify({ name:form.get("experiment_name") }) });
    await api(`/v1/experiments/${encodeURIComponent(experiment.id)}/runs`, { method:"POST", headers:{"Idempotency-Key":crypto.randomUUID()}, body:JSON.stringify({ recipe:{ adapter:form.get("adapter"), command:String(form.get("command")).trim().split(/\s+/).filter(Boolean) }, required_labels:parseLabels(String(form.get("labels"))), resource_requirements:{ gpu_count:gpuCount, min_free_gpu_memory_bytes:Math.round(minimumMemoryGiB * (1024 ** 3)) }, max_attempts:Number(form.get("max_attempts")) }) });
    $("#run-dialog").close(); toast("Run queued"); await refresh();
  } catch (error) { $("#form-error").textContent = error.message; }
  finally { button.disabled = false; }
}
function selectView(button, focus = false) {
  $$('.nav-item').forEach(tab => {
    const selected = tab === button;
    tab.classList.toggle('active', selected); tab.setAttribute('aria-selected', String(selected)); tab.tabIndex = selected ? 0 : -1;
    const panel = $(`#${tab.dataset.view}-view`); panel.classList.toggle('hidden', !selected); panel.hidden = !selected;
  });
  if (focus) button.focus();
}
$$('.nav-item').forEach((button, index, tabs) => {
  button.onclick = () => selectView(button);
  button.onkeydown = event => {
    if (!['ArrowLeft','ArrowRight','Home','End'].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : (index + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length;
    selectView(tabs[next], true);
  };
});
$("#refresh").onclick = refresh; $("#run-filter").oninput = renderRuns; $("#new-run").onclick = () => $("#run-dialog").showModal(); $$('[data-open-run]').forEach(b => b.onclick=()=>$("#run-dialog").showModal()); $("#close-dialog").onclick = $("#cancel-dialog").onclick = () => $("#run-dialog").close(); $("#run-form").onsubmit = submitRun;
$("#close-inspector").onclick = closeInspector;
document.addEventListener("keydown", event => { if (event.key === "Escape" && $("#inspector").classList.contains("open")) { event.preventDefault(); closeInspector(); } });
refresh(); setInterval(refresh, 5000);
