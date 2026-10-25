"use strict";

const $ = (sel) => document.querySelector(sel);

const PRESETS = {
  hackclub:   { name: "Hack Club AI", type: "openai",    base_url: "https://ai.hackclub.com/proxy/v1",                          model: "" },
  openrouter: { name: "OpenRouter",   type: "openai",    base_url: "https://openrouter.ai/api/v1",                              model: "" },
  claude:     { name: "Claude",       type: "anthropic", base_url: "https://api.anthropic.com/v1",                              model: "claude-sonnet-4-5" },
  gemini:     { name: "Gemini",       type: "gemini",    base_url: "https://generativelanguage.googleapis.com/v1beta",           model: "gemini-2.5-flash" },
};

const TYPE_LABELS = { openai: "OpenAI", anthropic: "Claude", gemini: "Gemini" };

let providers = [];

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => (
    { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
  ));
}

async function api(path, opts = {}) {
  const res = await fetch(path, {
    credentials: "same-origin",
    headers: opts.body ? { "Content-Type": "application/json" } : {},
    ...opts,
  });
  if (res.status === 401) {
    showLogin();
    throw new Error("unauthorized");
  }
  return res;
}

// ---------- Views ----------
function showLogin() {
  $("#login-view").classList.remove("hidden");
  $("#app-view").classList.add("hidden");
}
function showApp() {
  $("#login-view").classList.add("hidden");
  $("#app-view").classList.remove("hidden");
}

// ---------- Auth ----------
$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const password = $("#login-password").value;
  try {
    const res = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password }),
    });
    if (res.ok) {
      $("#login-error").classList.add("hidden");
      $("#login-password").value = "";
      showApp();
      await loadAll();
    } else {
      $("#login-error").textContent = "Wrong password";
      $("#login-error").classList.remove("hidden");
    }
  } catch {
    $("#login-error").textContent = "Network error";
    $("#login-error").classList.remove("hidden");
  }
});

$("#logout-btn").addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST" });
  showLogin();
});

// ---------- Data loading ----------
async function loadAll() {
  await Promise.all([loadProviders(), loadUsage()]);
}

async function loadProviders() {
  const res = await api("/api/providers");
  const data = await res.json();
  providers = data.providers || [];
  renderProviders();
}

async function loadUsage() {
  const res = await api("/api/usage");
  const data = await res.json();
  renderUsage(data.usage || {}, data.recent || []);
}

// ---------- Providers rendering ----------
function renderProviders() {
  const list = $("#providers-list");
  list.innerHTML = "";
  $("#no-providers").classList.toggle("hidden", providers.length > 0);

  providers.forEach((p, i) => {
    const card = document.createElement("div");
    card.className = "provider-card" + (p.enabled ? "" : " disabled");
    card.innerHTML = `
      <div class="provider-head">
        <span class="order">#${i + 1}</span>
        <span class="pname">${esc(p.name)}</span>
        <span class="badge">${esc(TYPE_LABELS[p.type] || p.type)}</span>
        <span class="p-model">${p.model ? esc(p.model) : "<em>any model</em>"}</span>
        <span class="status-dot ${p.enabled ? "on" : "off"}"></span>
      </div>
      <div class="provider-actions">
        <button class="icon" data-action="up" ${i === 0 ? "disabled" : ""} title="Move up">▲</button>
        <button class="icon" data-action="down" ${i === providers.length - 1 ? "disabled" : ""} title="Move down">▼</button>
        <button data-action="toggle">${p.enabled ? "Disable" : "Enable"}</button>
        <button data-action="test">Test</button>
        <button data-action="edit">Edit</button>
        <button data-action="del" class="danger">Delete</button>
      </div>
      <div class="test-result"></div>
    `;
    card.querySelectorAll("button[data-action]").forEach((btn) => {
      btn.addEventListener("click", () => handleAction(btn.dataset.action, p, i, card));
    });
    list.appendChild(card);
  });
}

function handleAction(action, p, i, card) {
  if (action === "up") reorder(i, i - 1);
  else if (action === "down") reorder(i, i + 1);
  else if (action === "toggle") toggleProvider(p);
  else if (action === "test") testProvider(p, card);
  else if (action === "edit") openModal(p);
  else if (action === "del") deleteProvider(p);
}

async function reorder(from, to) {
  if (to < 0 || to >= providers.length) return;
  const arr = providers.slice();
  const [item] = arr.splice(from, 1);
  arr.splice(to, 0, item);
  const res = await api("/api/providers/reorder", {
    method: "POST",
    body: JSON.stringify({ ids: arr.map((p) => p.id) }),
  });
  if (res.ok) await loadProviders();
}

async function toggleProvider(p) {
  await api(`/api/providers/${p.id}`, {
    method: "PUT",
    body: JSON.stringify({ ...p, enabled: !p.enabled }),
  });
  await loadProviders();
}

async function testProvider(p, card) {
  const out = card.querySelector(".test-result");
  out.textContent = "Testing…";
  out.className = "test-result";
  try {
    const res = await api(`/api/providers/${p.id}/test`, { method: "POST" });
    const data = await res.json();
    if (data.ok) {
      out.textContent = `✓ ${data.text}  (${data.latency_ms}ms, ${data.completion_tokens} tokens)`;
      out.className = "test-result ok";
    } else {
      out.textContent = `✗ ${data.error}`;
      out.className = "test-result err";
    }
  } catch {
    out.textContent = "✗ network error";
    out.className = "test-result err";
  }
}

async function deleteProvider(p) {
  if (!confirm(`Delete provider "${p.name}"?`)) return;
  await api(`/api/providers/${p.id}`, { method: "DELETE" });
  await loadProviders();
}

// ---------- Modal ----------
$("#add-provider-btn").addEventListener("click", () => openModal());
$("#modal-cancel").addEventListener("click", closeModal);
$("#modal-backdrop").addEventListener("click", (e) => {
  if (e.target === e.currentTarget) closeModal();
});
$("#p-preset").addEventListener("change", applyPreset);
$("#p-wildcard").addEventListener("change", syncWildcard);
$("#provider-form").addEventListener("submit", saveProvider);

function syncWildcard() {
  const wild = $("#p-wildcard").checked;
  $("#p-model").disabled = wild;
  if (wild) $("#p-model").value = "";
  $("#p-model").placeholder = wild ? "any model (wildcard)" : "model id";
}

function openModal(p) {
  $("#provider-form").reset();
  $("#p-id").value = p ? p.id : "";
  $("#modal-title").textContent = p ? "Edit provider" : "Add provider";
  if (p) {
    $("#p-name").value = p.name;
    $("#p-type").value = p.type;
    $("#p-base-url").value = p.base_url || "";
    $("#p-model").value = p.model || "";
    $("#p-api-key").value = p.api_key || "";
    $("#p-enabled").checked = !!p.enabled;
    $("#p-wildcard").checked = !p.model;
    $("#p-preset").value = "";
  } else {
    $("#p-preset").value = "";
    $("#p-enabled").checked = true;
    $("#p-wildcard").checked = false;
  }
  syncWildcard();
  $("#modal-backdrop").classList.remove("hidden");
}

function closeModal() {
  $("#modal-backdrop").classList.add("hidden");
}

function applyPreset() {
  const preset = PRESETS[$("#p-preset").value];
  if (!preset) return;
  $("#p-name").value = preset.name;
  $("#p-type").value = preset.type;
  $("#p-base-url").value = preset.base_url;
  $("#p-model").value = preset.model;
  $("#p-wildcard").checked = !preset.model;
  syncWildcard();
}

async function saveProvider(e) {
  e.preventDefault();
  const id = $("#p-id").value;
  const wildcard = $("#p-wildcard").checked;
  const payload = {
    name: $("#p-name").value.trim(),
    type: $("#p-type").value,
    base_url: $("#p-base-url").value.trim(),
    model: wildcard ? "" : $("#p-model").value.trim(),
    api_key: $("#p-api-key").value.trim(),
    enabled: $("#p-enabled").checked,
  };
  const url = id ? `/api/providers/${id}` : "/api/providers";
  const res = await api(url, { method: id ? "PUT" : "POST", body: JSON.stringify(payload) });
  if (res.ok) {
    closeModal();
    await loadProviders();
  } else {
    const d = await res.json().catch(() => ({}));
    alert(d.error || "Failed to save provider");
  }
}

// ---------- Usage rendering ----------
function renderUsage(usage, recent) {
  let req = 0, succ = 0, fail = 0, tokens = 0;
  const byId = {};
  providers.forEach((p) => (byId[p.id] = p.name));

  for (const k in usage) {
    const u = usage[k];
    req += u.requests;
    succ += u.successes;
    fail += u.failures;
    tokens += u.total_tokens;
  }
  $("#stat-cards").innerHTML = `
    <div class="stat"><div class="num">${req}</div><div class="lbl">Requests</div></div>
    <div class="stat"><div class="num ok">${succ}</div><div class="lbl">Succeeded</div></div>
    <div class="stat"><div class="num err">${fail}</div><div class="lbl">Failed</div></div>
    <div class="stat"><div class="num">${tokens}</div><div class="lbl">Tokens</div></div>
  `;

  const tb = $("#usage-table");
  tb.innerHTML = "";
  const rows = Object.entries(usage);
  if (rows.length === 0) {
    tb.innerHTML = `<tr><td colspan="7" class="muted">No requests yet</td></tr>`;
  } else {
    rows.forEach(([id, u]) => {
      const avg = u.requests ? Math.round(u.total_latency_ms / u.requests) : 0;
      const last = u.last_used_unix ? timeAgo(u.last_used_unix) : "—";
      tb.insertAdjacentHTML("beforeend", `
        <tr>
          <td>${esc(u.name || byId[id] || id)}</td>
          <td>${u.requests}</td>
          <td>${u.successes}</td>
          <td>${u.failures}</td>
          <td>${u.total_tokens}</td>
          <td>${avg}ms</td>
          <td>${last}</td>
        </tr>`);
    });
  }

  const rt = $("#recent-table");
  rt.innerHTML = "";
  if (recent.length === 0) {
    rt.innerHTML = `<tr><td colspan="7" class="muted">No requests yet</td></tr>`;
  } else {
    recent.forEach((r) => {
      rt.insertAdjacentHTML("beforeend", `
        <tr>
          <td>${esc(new Date(r.time_unix * 1000).toLocaleTimeString())}</td>
          <td>${esc(r.provider)}</td>
          <td>${esc(r.model)}</td>
          <td><span class="status ${r.status}">${r.status}</span></td>
          <td>${r.latency_ms}ms</td>
          <td>${r.completion_tokens}</td>
          <td>${esc(r.error || "")}</td>
        </tr>`);
    });
  }
}

function timeAgo(unix) {
  const s = Math.floor(Date.now() / 1000 - unix);
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
}

// ---------- Init ----------
async function init() {
  try {
    const res = await fetch("/api/me");
    if (res.ok) {
      showApp();
      await loadAll();
    } else {
      showLogin();
    }
  } catch {
    showLogin();
  }
}

setInterval(() => {
  if (!$("#app-view").classList.contains("hidden")) loadUsage();
}, 5000);

init();
