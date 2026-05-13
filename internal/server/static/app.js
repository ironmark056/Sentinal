// Sentinel Central — vanilla-JS SPA.
// Three tables (agents, sessions, events), event-detail modal, agent
// create/delete, composable filters (agent + session + q) encoded into
// the URL hash as #?agent=X&session=Y&q=Z so reload/share survives.
//
// No framework, no build step.

const POLL_INTERVAL_MS = 2500;
const ADMIN_TOKEN_KEY = "sentinel.adminToken";

const $ = (id) => document.getElementById(id);

const els = {
  // Header / status
  adminStatus: $("admin-status"),
  adminConfig: $("admin-config"),
  liveIndicator: $("live-indicator"),
  liveLabel: $("live-label"),
  // Stats
  statTotal: $("stat-total"),
  stat24h: $("stat-24h"),
  statBlocked: $("stat-blocked"),
  statAgents: $("stat-agents"),
  // Tables
  agentsTbody: $("agents-tbody"),
  agentsEmpty: $("agents-empty"),
  sessionsTbody: $("sessions-tbody"),
  sessionsEmpty: $("sessions-empty"),
  eventsTbody: $("events-tbody"),
  eventsTitle: $("events-title"),
  // Filter chips
  filtersBar: $("filters-bar"),
  autorefresh: $("autorefresh"),
  // Search (slice 0.2.4 — wired below; container hidden if absent)
  searchInput: $("search-input"),
  searchClear: $("search-clear"),
  // Add agent
  agentAdd: $("agent-add"),
  addModal: $("add-modal"),
  addForm: $("add-form"),
  addName: $("add-name"),
  addMeta: $("add-meta"),
  addError: $("add-error"),
  // Policy editor
  policyBody: $("policy-body"),
  policySave: $("policy-save"),
  policyMeta: $("policy-meta"),
  policyError: $("policy-error"),
  // Enroll agent
  agentEnroll: $("agent-enroll"),
  enrollModal: $("enroll-modal"),
  enrollForm: $("enroll-form"),
  enrollName: $("enroll-name"),
  enrollMeta: $("enroll-meta"),
  enrollTtl: $("enroll-ttl"),
  enrollError: $("enroll-error"),
  enrollUrlModal: $("enroll-url-modal"),
  enrollUrlCommand: $("enroll-url-command"),
  enrollUrlCopy: $("enroll-url-copy"),
  enrollUrlExpires: $("enroll-url-expires"),
  enrollUrlNote: $("enroll-url-note"),
  enrollmentsSection: $("enrollments-section"),
  enrollmentsTbody: $("enrollments-tbody"),
  // Token reveal
  tokenModal: $("token-modal"),
  tokenValue: $("token-value"),
  tokenCopy: $("token-copy"),
  tokenYaml: $("token-yaml"),
  // Admin token
  adminModal: $("admin-modal"),
  adminForm: $("admin-form"),
  adminInput: $("admin-input"),
  adminClear: $("admin-clear"),
  // Detail
  detailModal: $("detail-modal"),
  detailTitle: $("detail-title"),
  detailMeta: $("detail-meta"),
  detailPayload: $("detail-payload"),
};

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const state = {
  agents: [],
  sessions: [],
  enrollments: [],
  filter: {
    agentID: 0,
    sessionID: "",
    q: "",
  },
};

function getAdminToken() { return sessionStorage.getItem(ADMIN_TOKEN_KEY) || ""; }
function setAdminToken(v) {
  if (v) sessionStorage.setItem(ADMIN_TOKEN_KEY, v);
  else sessionStorage.removeItem(ADMIN_TOKEN_KEY);
  renderAdminStatus();
}
function renderAdminStatus() {
  const set = !!getAdminToken();
  els.adminStatus.textContent = set ? "admin token: set" : "admin token: unset";
  els.adminStatus.className = "admin-status " + (set ? "set" : "unset");
}
function authHeaders() {
  const tok = getAdminToken();
  return tok ? { Authorization: "Bearer " + tok } : {};
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

function fmtTimeNS(unixNanos) {
  if (!unixNanos) return "—";
  const ms = Math.floor(unixNanos / 1e6);
  const d = new Date(ms);
  return d.toLocaleTimeString([], { hour12: false }) +
    "." + String(d.getMilliseconds()).padStart(3, "0");
}
function fmtDateNS(unixNanos) {
  if (!unixNanos) return "—";
  return new Date(Math.floor(unixNanos / 1e6)).toLocaleString();
}
function fmtAgo(unixNanos) {
  if (!unixNanos) return null;
  const seconds = Math.max(0, Math.floor((Date.now() - unixNanos / 1e6) / 1000));
  if (seconds < 60) return seconds + "s ago";
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return minutes + "m ago";
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return hours + "h ago";
  const days = Math.floor(hours / 24);
  return days + "d ago";
}
function fmtDuration(fromNS, toNS) {
  if (!fromNS || !toNS) return "—";
  const secs = Math.max(0, Math.floor((toNS - fromNS) / 1e9));
  if (secs < 60) return secs + "s";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return mins + "m " + (secs % 60) + "s";
  const hrs = Math.floor(mins / 60);
  return hrs + "h " + (mins % 60) + "m";
}
function fmtBytes(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}
function fmtSession(id) { return id ? id.slice(0, 8) : "—"; }

function eventStatus(ev) {
  if (ev.msg_type === "error") return { label: "BLOCKED", cls: "tag-blocked" };
  if (ev.msg_type === "response") return { label: "ok", cls: "tag-ok" };
  if (ev.msg_type === "request") return { label: "req", cls: "tag-c2s" };
  if (ev.msg_type === "notification") return { label: "notif", cls: "" };
  return { label: ev.msg_type, cls: "" };
}

function setLive(ok) {
  els.liveIndicator.classList.toggle("stale", !ok);
  els.liveLabel.textContent = ok ? "live" : "offline";
}

function td(content) {
  const t = document.createElement("td");
  if (content instanceof Node) t.appendChild(content);
  else t.textContent = content == null ? "" : String(content);
  return t;
}
function tag(text, cls) {
  const span = document.createElement("span");
  span.className = "tag " + (cls || "");
  span.textContent = text || "—";
  return span;
}

// ---------------------------------------------------------------------------
// Filter model (composable: agent + session + q)
// ---------------------------------------------------------------------------

function readHashFilter() {
  // Accept #?key=val&key=val ... or legacy #/agent/<id> (last form preserved
  // briefly so a hot reload between slices doesn't blow up).
  const h = location.hash || "";
  const legacy = h.match(/^#\/agent\/(\d+)$/);
  if (legacy) return { agentID: parseInt(legacy[1], 10), sessionID: "", q: "" };

  if (!h.startsWith("#?")) return { agentID: 0, sessionID: "", q: "" };
  const p = new URLSearchParams(h.slice(2));
  return {
    agentID: parseInt(p.get("agent") || "0", 10) || 0,
    sessionID: p.get("session") || "",
    q: p.get("q") || "",
  };
}

function writeHashFilter() {
  const p = new URLSearchParams();
  if (state.filter.agentID > 0) p.set("agent", String(state.filter.agentID));
  if (state.filter.sessionID) p.set("session", state.filter.sessionID);
  if (state.filter.q) p.set("q", state.filter.q);
  const enc = p.toString();
  const next = enc ? ("#?" + enc) : "";
  if (location.hash !== next) {
    // history.replaceState avoids polluting the back stack on every keystroke.
    history.replaceState(null, "", location.pathname + location.search + next);
  }
}

function setFilter(patch, opts) {
  Object.assign(state.filter, patch || {});
  writeHashFilter();
  renderFilterChips();
  renderAgents();
  renderSessions();
  if (!opts || !opts.skipFetch) refreshEvents();
  refreshSessions(); // session list narrows when agent changes
}

function clearAgent() { setFilter({ agentID: 0, sessionID: "" }); }
function clearSession() { setFilter({ sessionID: "" }); }
function clearSearch() {
  if (els.searchInput) els.searchInput.value = "";
  setFilter({ q: "" });
}
function clearAllFilters() {
  if (els.searchInput) els.searchInput.value = "";
  setFilter({ agentID: 0, sessionID: "", q: "" });
}

function filterByAgent(id) {
  if (state.filter.agentID === id) {
    clearAgent();
    return;
  }
  setFilter({ agentID: id, sessionID: "" });
}

function filterBySession(sessionRow) {
  if (state.filter.sessionID === sessionRow.session_id) {
    clearSession();
    return;
  }
  setFilter({ sessionID: sessionRow.session_id, agentID: sessionRow.agent_id });
}

function renderFilterChips() {
  els.filtersBar.innerHTML = "";
  const chips = [];

  if (state.filter.agentID > 0) {
    const a = state.agents.find((x) => x.id === state.filter.agentID);
    chips.push({
      label: "agent",
      value: a ? a.name : ("#" + state.filter.agentID),
      onClear: clearAgent,
    });
  }
  if (state.filter.sessionID) {
    chips.push({
      label: "session",
      value: state.filter.sessionID.slice(0, 8),
      onClear: clearSession,
    });
  }
  if (state.filter.q) {
    chips.push({
      label: "search",
      value: '"' + state.filter.q + '"',
      onClear: clearSearch,
    });
  }
  if (chips.length === 0) {
    els.eventsTitle.textContent = "Activity";
    return;
  }

  for (const c of chips) {
    const chip = document.createElement("span");
    chip.className = "filter-chip";
    chip.innerHTML = `${c.label}: <b></b>`;
    chip.querySelector("b").textContent = c.value;
    const x = document.createElement("button");
    x.className = "btn-ghost";
    x.title = "clear this filter";
    x.textContent = "×";
    x.addEventListener("click", c.onClear);
    chip.appendChild(x);
    els.filtersBar.appendChild(chip);
  }

  if (chips.length > 1) {
    const clearAll = document.createElement("button");
    clearAll.className = "btn-ghost";
    clearAll.title = "clear all filters";
    clearAll.textContent = "clear all";
    clearAll.addEventListener("click", clearAllFilters);
    els.filtersBar.appendChild(clearAll);
  }

  els.eventsTitle.textContent =
    "Activity (" + chips.map((c) => c.label + ":" + c.value).join(", ") + ")";
}

// ---------------------------------------------------------------------------
// Network
// ---------------------------------------------------------------------------

async function fetchJSON(url, opts) {
  const r = await fetch(url, Object.assign({ cache: "no-store" }, opts || {}));
  if (!r.ok) {
    const t = await r.text();
    throw new Error(r.status + " " + (t || r.statusText));
  }
  return r.json();
}

// ---------------------------------------------------------------------------
// Renderers
// ---------------------------------------------------------------------------

async function refreshStats() {
  try {
    const s = await fetchJSON("/api/stats");
    els.statTotal.textContent = s.total.toLocaleString();
    els.stat24h.textContent = s.last_24h.toLocaleString();
    els.statBlocked.textContent = s.blocked_count.toLocaleString();
    els.statAgents.textContent = s.agent_count.toLocaleString();
    setLive(true);
  } catch (err) { setLive(false); console.error("stats", err); }
}

async function refreshAgents() {
  try {
    const list = await fetchJSON("/api/agents");
    state.agents = list || [];
    renderAgents();
    renderFilterChips(); // resolve agent name after agents arrive
    setLive(true);
  } catch (err) { setLive(false); console.error("agents", err); }
}

function renderAgents() {
  const tbody = els.agentsTbody;
  tbody.innerHTML = "";
  if (state.agents.length === 0) { els.agentsEmpty.style.display = ""; return; }
  els.agentsEmpty.style.display = "none";

  for (const a of state.agents) {
    const tr = document.createElement("tr");
    if (a.id === state.filter.agentID) tr.classList.add("selected");
    tr.dataset.agentId = a.id;

    tr.appendChild(td(String(a.id)));

    const nameCell = td("");
    const nameSpan = document.createElement("span");
    nameSpan.className = "agent-name";
    nameSpan.textContent = a.name;
    nameCell.appendChild(nameSpan);
    tr.appendChild(nameCell);

    if (a.last_seen) {
      const s = document.createElement("span");
      s.textContent = fmtAgo(a.last_seen);
      tr.appendChild(td(s));
    } else {
      const s = document.createElement("span");
      s.className = "never";
      s.textContent = "(never)";
      tr.appendChild(td(s));
    }
    tr.appendChild(td(fmtDateNS(a.created)));
    tr.appendChild(td(renderMetadata(a.metadata)));

    const actions = document.createElement("td");
    actions.className = "col-actions";
    const revoke = document.createElement("button");
    revoke.className = "btn-small";
    revoke.textContent = "Revoke";
    revoke.title = "Delete this agent and all its events";
    revoke.addEventListener("click", (e) => { e.stopPropagation(); revokeAgent(a); });
    actions.appendChild(revoke);
    tr.appendChild(actions);

    tr.addEventListener("click", () => filterByAgent(a.id));
    tbody.appendChild(tr);
  }
}

function renderMetadata(metaJSON) {
  const frag = document.createDocumentFragment();
  if (!metaJSON || metaJSON === "{}" || metaJSON === "null") {
    const s = document.createElement("span");
    s.className = "never";
    s.textContent = "—";
    frag.appendChild(s);
    return frag;
  }
  try {
    const obj = JSON.parse(metaJSON);
    for (const k of Object.keys(obj)) {
      const pill = document.createElement("span");
      pill.className = "meta-pill";
      pill.textContent = `${k}=${obj[k]}`;
      frag.appendChild(pill);
    }
  } catch {
    const s = document.createElement("span");
    s.textContent = metaJSON;
    frag.appendChild(s);
  }
  return frag;
}

async function refreshSessions() {
  try {
    const params = new URLSearchParams({ limit: "100" });
    if (state.filter.agentID > 0) params.set("agent_id", String(state.filter.agentID));
    const list = await fetchJSON("/api/sessions?" + params.toString());
    state.sessions = list || [];
    renderSessions();
    setLive(true);
  } catch (err) { setLive(false); console.error("sessions", err); }
}

function renderSessions() {
  const tbody = els.sessionsTbody;
  tbody.innerHTML = "";
  if (!state.sessions || state.sessions.length === 0) {
    els.sessionsEmpty.style.display = "";
    return;
  }
  els.sessionsEmpty.style.display = "none";

  for (const sRow of state.sessions) {
    const tr = document.createElement("tr");
    if (sRow.session_id === state.filter.sessionID) tr.classList.add("selected");

    const idCell = td("");
    const idSpan = document.createElement("span");
    idSpan.className = "session-id";
    idSpan.textContent = sRow.session_id.slice(0, 12);
    idSpan.title = sRow.session_id;
    idCell.appendChild(idSpan);
    tr.appendChild(idCell);

    const agentCell = td("");
    const agentSpan = document.createElement("span");
    agentSpan.className = "agent-name";
    agentSpan.textContent = sRow.agent_name || ("#" + sRow.agent_id);
    agentCell.appendChild(agentSpan);
    tr.appendChild(agentCell);

    tr.appendChild(td(fmtDateNS(sRow.first_ts)));
    tr.appendChild(td(fmtDuration(sRow.first_ts, sRow.last_ts)));
    tr.appendChild(td(String(sRow.event_count)));

    const blocked = document.createElement("span");
    if (sRow.blocked_count > 0) {
      blocked.className = "blocked-count";
      blocked.textContent = String(sRow.blocked_count);
    } else {
      blocked.className = "never";
      blocked.textContent = "0";
    }
    tr.appendChild(td(blocked));

    tr.appendChild(td(sRow.upstreams || "—"));

    tr.addEventListener("click", () => filterBySession(sRow));
    tbody.appendChild(tr);
  }
}

async function refreshEvents() {
  try {
    const params = new URLSearchParams({ limit: "200" });
    if (state.filter.agentID > 0) params.set("agent_id", String(state.filter.agentID));
    if (state.filter.sessionID) params.set("session_id", state.filter.sessionID);
    if (state.filter.q) params.set("q", state.filter.q);
    const evs = await fetchJSON("/api/events?" + params.toString());
    renderEvents(evs || []);
    setLive(true);
  } catch (err) { setLive(false); console.error("events", err); }
}

function renderEvents(evs) {
  const tbody = els.eventsTbody;
  tbody.innerHTML = "";
  for (const ev of evs) {
    const tr = document.createElement("tr");
    tr.dataset.id = ev.id;

    const status = eventStatus(ev);
    tr.appendChild(td(fmtTimeNS(ev.server_ts || ev.agent_ts)));

    const agentCell = td("");
    const agentSpan = document.createElement("span");
    agentSpan.className = "agent-name";
    agentSpan.textContent = ev.agent_name || ("#" + ev.agent_id);
    agentCell.appendChild(agentSpan);
    tr.appendChild(agentCell);

    tr.appendChild(td(tag(ev.direction, "tag-" + ev.direction)));
    tr.appendChild(td(tag(ev.msg_type, ev.msg_type === "error" ? "tag-error" : "")));
    tr.appendChild(td(ev.method || "—"));
    tr.appendChild(td(fmtSession(ev.session_id)));
    tr.appendChild(td(ev.upstream || "—"));
    tr.appendChild(td(tag(status.label, status.cls)));
    tr.appendChild(td(fmtBytes(ev.bytes)));

    tr.addEventListener("click", () => openDetail(ev.id));
    tbody.appendChild(tr);
  }
}

async function openDetail(id) {
  try {
    const ev = await fetchJSON("/api/events/" + id);
    els.detailTitle.textContent = `Event #${ev.id} — ${ev.method || ev.msg_type}`;
    els.detailMeta.innerHTML = "";
    const fields = [
      ["Agent", ev.agent_name + " (#" + ev.agent_id + ")"],
      ["Server time", new Date(Math.floor(ev.server_ts / 1e6)).toISOString()],
      ["Agent time", new Date(Math.floor(ev.agent_ts / 1e6)).toISOString()],
      ["Direction", ev.direction],
      ["Type", ev.msg_type],
      ["Method", ev.method || "—"],
      ["JSON-RPC id", ev.msg_id || "—"],
      ["Session", ev.session_id],
      ["Upstream", ev.upstream],
      ["Size", fmtBytes(ev.bytes)],
    ];
    for (const [k, v] of fields) {
      const div = document.createElement("div");
      const dt = document.createElement("dt"); dt.textContent = k;
      const dd = document.createElement("dd"); dd.textContent = v;
      div.appendChild(dt); div.appendChild(dd);
      els.detailMeta.appendChild(div);
    }
    els.detailPayload.textContent = prettyJSON(ev.payload);
    showModal("detail-modal");
  } catch (err) { console.error("event detail", err); }
}

function prettyJSON(s) {
  try { return JSON.stringify(JSON.parse(s), null, 2); }
  catch { return s; }
}

// ---------------------------------------------------------------------------
// Central policy editor (slice 0.2.7)
// ---------------------------------------------------------------------------

const POLICY_PLACEHOLDER = JSON.stringify({
  deny_paths: [],
  scoring: { approve_threshold: 30, block_threshold: 80 },
}, null, 2);

let policyServerETag = "";
let policyLastSaved = "";

async function refreshPolicy() {
  if (!els.policyBody) return;
  try {
    const resp = await fetch("/api/policy", { cache: "no-store" });
    if (!resp.ok) throw new Error(resp.status + " " + resp.statusText);
    const data = await resp.json();
    policyServerETag = resp.headers.get("ETag") || "";
    const body = data.body || {};
    const pretty = JSON.stringify(body, null, 2);
    // Only overwrite the textarea if the user hasn't been editing OR the
    // saved value matches what they're showing. Otherwise we'd nuke
    // their in-progress edits on every poll.
    if (els.policyBody.value === policyLastSaved || els.policyBody.value === "") {
      els.policyBody.value = pretty;
      policyLastSaved = pretty;
    }
    if (data.id) {
      els.policyMeta.textContent =
        "rev #" + data.id + " — " + new Date(Math.floor(data.created / 1e6)).toLocaleString();
    } else {
      els.policyMeta.textContent = "(no policy set yet)";
    }
    updateSaveButton();
  } catch (err) {
    if (els.policyMeta) els.policyMeta.textContent = "fetch failed: " + err.message;
  }
}

function updateSaveButton() {
  if (!els.policyBody || !els.policySave) return;
  const cur = els.policyBody.value;
  const dirty = cur !== policyLastSaved && cur.trim() !== "";
  els.policySave.disabled = !dirty;
}

async function savePolicy() {
  els.policyError.style.display = "none";
  let parsed;
  try {
    parsed = JSON.parse(els.policyBody.value);
  } catch (err) {
    els.policyError.textContent = "Invalid JSON: " + err.message;
    els.policyError.style.display = "";
    return;
  }
  els.policySave.disabled = true;
  try {
    const resp = await fetch("/api/policy", {
      method: "PUT",
      headers: Object.assign(
        { "Content-Type": "application/json" },
        authHeaders(),
      ),
      body: JSON.stringify(parsed),
    });
    if (!resp.ok) {
      const t = await resp.text();
      if (resp.status === 401) {
        els.policyError.textContent =
          "Admin token required (or wrong). Set it via the “admin token” button.";
      } else {
        els.policyError.textContent = "Save failed: " + resp.status + " " + t;
      }
      els.policyError.style.display = "";
      els.policySave.disabled = false;
      return;
    }
    const data = await resp.json();
    policyServerETag = resp.headers.get("ETag") || "";
    const pretty = JSON.stringify(data.body || {}, null, 2);
    els.policyBody.value = pretty;
    policyLastSaved = pretty;
    els.policyMeta.textContent =
      "rev #" + data.id + " — saved " + new Date(Math.floor(data.created / 1e6)).toLocaleString();
  } catch (err) {
    els.policyError.textContent = "Network error: " + err.message;
    els.policyError.style.display = "";
    els.policySave.disabled = false;
  }
}

if (els.policyBody) {
  els.policyBody.addEventListener("input", updateSaveButton);
  els.policySave.addEventListener("click", savePolicy);
}

// ---------------------------------------------------------------------------
// Enrollments (slice 0.2.5.1)
// ---------------------------------------------------------------------------

async function refreshEnrollments() {
  try {
    const list = await fetchJSON("/api/enroll");
    state.enrollments = list || [];
    renderEnrollments();
  } catch (err) {
    console.error("enrollments", err);
  }
}

function renderEnrollments() {
  // Filter to outstanding (not consumed AND not past expiry) — admins
  // mostly want to see these. Consumed ones become Agent rows.
  const now = Date.now() * 1e6; // unix nanos
  const outstanding = (state.enrollments || []).filter(
    (e) => !e.consumed && e.expires > now,
  );
  if (outstanding.length === 0) {
    els.enrollmentsSection.style.display = "none";
    return;
  }
  els.enrollmentsSection.style.display = "";
  els.enrollmentsTbody.innerHTML = "";
  for (const e of outstanding) {
    const tr = document.createElement("tr");
    tr.appendChild(td(String(e.id)));
    const nameCell = td("");
    const nameSpan = document.createElement("span");
    nameSpan.className = "agent-name";
    nameSpan.textContent = e.name;
    nameCell.appendChild(nameSpan);
    tr.appendChild(nameCell);
    tr.appendChild(td(fmtDateNS(e.expires)));
    const status = document.createElement("span");
    status.className = "tag tag-c2s";
    status.textContent = "outstanding";
    tr.appendChild(td(status));

    const actions = document.createElement("td");
    actions.className = "col-actions";
    const revoke = document.createElement("button");
    revoke.className = "btn-small";
    revoke.textContent = "Revoke";
    revoke.title = "Invalidate this enrollment URL";
    revoke.addEventListener("click", () => revokeEnrollment(e));
    actions.appendChild(revoke);
    tr.appendChild(actions);
    els.enrollmentsTbody.appendChild(tr);
  }
}

function openEnrollModal() {
  els.enrollName.value = "";
  els.enrollMeta.value = "";
  els.enrollTtl.value = "24";
  els.enrollError.style.display = "none";
  showModal("enroll-modal");
  setTimeout(() => els.enrollName.focus(), 0);
}

async function createEnrollment(evt) {
  evt.preventDefault();
  const name = els.enrollName.value.trim();
  if (!name) return;

  const metadata = {};
  const raw = els.enrollMeta.value.trim();
  if (raw) {
    for (const kv of raw.split(",")) {
      const idx = kv.indexOf("=");
      if (idx <= 0) continue;
      const k = kv.slice(0, idx).trim();
      const v = kv.slice(idx + 1).trim();
      if (k) metadata[k] = v;
    }
  }
  const hours = parseInt(els.enrollTtl.value, 10) || 24;
  const ttlSeconds = hours * 3600;

  els.enrollError.style.display = "none";
  try {
    const resp = await fetch("/api/enroll", {
      method: "POST",
      headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
      body: JSON.stringify({ name, metadata, ttl_seconds: ttlSeconds }),
    });
    if (!resp.ok) {
      const t = await resp.text();
      if (resp.status === 401) {
        els.enrollError.textContent =
          "Admin token required (or wrong). Set it via the “admin token” button.";
      } else {
        els.enrollError.textContent = `Create failed: ${resp.status} ${t}`;
      }
      els.enrollError.style.display = "";
      return;
    }
    const body = await resp.json();
    hideModal("enroll-modal");
    showEnrollUrlModal(body);
    await refreshEnrollments();
  } catch (err) {
    els.enrollError.textContent = "Network error: " + err.message;
    els.enrollError.style.display = "";
  }
}

function showEnrollUrlModal(body) {
  // body is { enrollment, ott, url, command, note }
  els.enrollUrlCommand.textContent = body.command || ("sentinel enroll " + body.url);
  els.enrollUrlExpires.textContent = body.enrollment
    ? fmtDateNS(body.enrollment.expires)
    : "";
  els.enrollUrlNote.textContent = body.note || "";
  showModal("enroll-url-modal");
}

async function revokeEnrollment(e) {
  if (!confirm(`Revoke enrollment "${e.name}" (#${e.id})?\n\nThe URL becomes invalid immediately.`)) return;
  try {
    const resp = await fetch("/api/enroll/" + e.id, {
      method: "DELETE",
      headers: authHeaders(),
    });
    if (!resp.ok && resp.status !== 204) {
      const t = await resp.text();
      if (resp.status === 401) {
        alert("Admin token required (or wrong).");
      } else {
        alert(`Revoke failed: ${resp.status} ${t}`);
      }
      return;
    }
    await refreshEnrollments();
  } catch (err) {
    alert("Network error: " + err.message);
  }
}

// ---------------------------------------------------------------------------
// Agent CRUD
// ---------------------------------------------------------------------------

function openAddAgentModal() {
  els.addName.value = "";
  els.addMeta.value = "";
  els.addError.style.display = "none";
  showModal("add-modal");
  setTimeout(() => els.addName.focus(), 0);
}

async function createAgent(evt) {
  evt.preventDefault();
  const name = els.addName.value.trim();
  if (!name) return;
  const metadata = {};
  const raw = els.addMeta.value.trim();
  if (raw) {
    for (const kv of raw.split(",")) {
      const idx = kv.indexOf("=");
      if (idx <= 0) continue;
      const k = kv.slice(0, idx).trim();
      const v = kv.slice(idx + 1).trim();
      if (k) metadata[k] = v;
    }
  }
  els.addError.style.display = "none";
  try {
    const resp = await fetch("/api/agents", {
      method: "POST",
      headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
      body: JSON.stringify({ name, metadata }),
    });
    if (!resp.ok) {
      const t = await resp.text();
      if (resp.status === 401) {
        els.addError.textContent = "Admin token required (or wrong). Set it via the “admin token” button.";
      } else {
        els.addError.textContent = `Create failed: ${resp.status} ${t}`;
      }
      els.addError.style.display = "";
      return;
    }
    const body = await resp.json();
    hideModal("add-modal");
    showTokenModal(body.agent, body.token);
    await refreshAgents();
  } catch (err) {
    els.addError.textContent = "Network error: " + err.message;
    els.addError.style.display = "";
  }
}

function showTokenModal(agent, token) {
  els.tokenValue.textContent = token;
  els.tokenYaml.textContent =
`central:
  url: ${location.protocol}//${location.host}
  token: ${token}
  agent_name: ${agent.name}`;
  showModal("token-modal");
}

async function revokeAgent(agent) {
  if (!confirm(`Revoke agent "${agent.name}" (#${agent.id})?\n\nThis deletes all its events.`)) return;
  try {
    const resp = await fetch("/api/agents/" + agent.id, {
      method: "DELETE",
      headers: authHeaders(),
    });
    if (!resp.ok && resp.status !== 204) {
      const t = await resp.text();
      if (resp.status === 401) {
        alert("Admin token required (or wrong). Set it via the “admin token” button.");
      } else {
        alert(`Delete failed: ${resp.status} ${t}`);
      }
      return;
    }
    if (state.filter.agentID === agent.id) clearAllFilters();
    await Promise.all([refreshAgents(), refreshStats(), refreshSessions(), refreshEvents()]);
  } catch (err) {
    alert("Network error: " + err.message);
  }
}

// ---------------------------------------------------------------------------
// Search input — slice 0.2.4. Debounced; writes q= into the hash.
// ---------------------------------------------------------------------------

if (els.searchInput) {
  let searchTimer = null;
  els.searchInput.addEventListener("input", () => {
    const v = els.searchInput.value;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
      // Only fetch when the value actually changed.
      if (v === state.filter.q) return;
      setFilter({ q: v });
    }, 250);
  });
  els.searchInput.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      els.searchInput.value = "";
      setFilter({ q: "" });
    }
  });
}
if (els.searchClear) {
  els.searchClear.addEventListener("click", clearSearch);
}

// ---------------------------------------------------------------------------
// Modals
// ---------------------------------------------------------------------------

function showModal(id) { document.getElementById(id).style.display = "flex"; }
function hideModal(id) { document.getElementById(id).style.display = "none"; }

for (const m of document.querySelectorAll(".modal")) {
  m.addEventListener("click", (e) => { if (e.target === m) m.style.display = "none"; });
}
for (const b of document.querySelectorAll("[data-modal]")) {
  b.addEventListener("click", () => hideModal(b.dataset.modal));
}
for (const b of document.querySelectorAll("[data-modal-close]")) {
  b.addEventListener("click", () => hideModal(b.dataset.modalClose));
}
document.addEventListener("keydown", (e) => {
  if (e.key !== "Escape") return;
  for (const m of document.querySelectorAll(".modal")) {
    if (m.style.display !== "none") m.style.display = "none";
  }
});

// Admin-token modal
function openAdminModal() {
  els.adminInput.value = getAdminToken();
  showModal("admin-modal");
  setTimeout(() => els.adminInput.focus(), 0);
}
els.adminConfig.addEventListener("click", openAdminModal);
els.adminForm.addEventListener("submit", (e) => {
  e.preventDefault();
  setAdminToken(els.adminInput.value.trim());
  hideModal("admin-modal");
});
els.adminClear.addEventListener("click", () => { setAdminToken(""); els.adminInput.value = ""; });

// Add-agent modal
els.agentAdd.addEventListener("click", openAddAgentModal);
els.addForm.addEventListener("submit", createAgent);

// Enroll-agent modal
if (els.agentEnroll) {
  els.agentEnroll.addEventListener("click", openEnrollModal);
  els.enrollForm.addEventListener("submit", createEnrollment);
  els.enrollUrlCopy.addEventListener("click", () => {
    const t = els.enrollUrlCommand.textContent;
    navigator.clipboard.writeText(t).then(
      () => { els.enrollUrlCopy.textContent = "Copied!"; setTimeout(() => (els.enrollUrlCopy.textContent = "Copy"), 1500); },
      () => { els.enrollUrlCopy.textContent = "(use Ctrl+C)"; }
    );
  });
}

// Token copy
els.tokenCopy.addEventListener("click", () => {
  const t = els.tokenValue.textContent;
  navigator.clipboard.writeText(t).then(
    () => { els.tokenCopy.textContent = "Copied!"; setTimeout(() => (els.tokenCopy.textContent = "Copy"), 1500); },
    () => { els.tokenCopy.textContent = "(use Ctrl+C)"; }
  );
});

window.addEventListener("hashchange", () => {
  Object.assign(state.filter, readHashFilter());
  if (els.searchInput) els.searchInput.value = state.filter.q;
  renderFilterChips();
  renderAgents();
  renderSessions();
  refreshEvents();
  refreshSessions();
});

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

async function refreshAll() {
  await Promise.all([
    refreshStats(),
    refreshAgents(),
    refreshSessions(),
    refreshEvents(),
    refreshEnrollments(),
    refreshPolicy(),
  ]);
}

Object.assign(state.filter, readHashFilter());
if (els.searchInput) els.searchInput.value = state.filter.q;
renderAdminStatus();
renderFilterChips();
refreshAll();
setInterval(() => {
  if (els.autorefresh.checked) refreshAll();
}, POLL_INTERVAL_MS);
