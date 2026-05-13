// Minimal vanilla JS for the Sentinel dashboard.
// No framework, no build step. Just fetch + DOM.

const POLL_INTERVAL_MS = 2500;

const els = {
  auditPath: document.getElementById("audit-path"),
  liveIndicator: document.getElementById("live-indicator"),
  liveLabel: document.getElementById("live-label"),
  statTotal: document.getElementById("stat-total"),
  stat24h: document.getElementById("stat-24h"),
  statBlocked: document.getElementById("stat-blocked"),
  statSessions: document.getElementById("stat-sessions"),
  pendingSection: document.getElementById("pending-section"),
  pendingList: document.getElementById("pending-list"),
  autoSection: document.getElementById("auto-section"),
  autoList: document.getElementById("auto-list"),
  recentBlockedSection: document.getElementById("recent-blocked-section"),
  recentBlockedList: document.getElementById("recent-blocked-list"),
  eventsTbody: document.getElementById("events-tbody"),
  autorefresh: document.getElementById("autorefresh"),
  modal: document.getElementById("detail-modal"),
  modalTitle: document.getElementById("detail-title"),
  modalMeta: document.getElementById("detail-meta"),
  modalPayload: document.getElementById("detail-payload"),
  modalClose: document.getElementById("detail-close"),
};

function fmtTime(unixNanos) {
  const ms = Math.floor(unixNanos / 1e6);
  const d = new Date(ms);
  return d.toLocaleTimeString([], { hour12: false }) +
    "." + String(d.getMilliseconds()).padStart(3, "0");
}

function fmtBytes(n) {
  if (n < 1024) return n + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / 1024 / 1024).toFixed(1) + " MB";
}

function fmtSession(id) {
  if (!id) return "—";
  return id.slice(0, 8);
}

function fmtMethod(ev, payloadHint) {
  if (ev.method) return ev.method;
  return "—";
}

function eventStatus(ev) {
  if (ev.msg_type === "error") {
    return { label: "BLOCKED", cls: "tag-blocked" };
  }
  if (ev.msg_type === "response") return { label: "ok", cls: "tag-ok" };
  if (ev.msg_type === "request") return { label: "req", cls: "tag-c2s" };
  if (ev.msg_type === "notification") return { label: "notif", cls: "" };
  return { label: ev.msg_type, cls: "" };
}

function setLive(ok) {
  els.liveIndicator.classList.toggle("stale", !ok);
  els.liveLabel.textContent = ok ? "live" : "offline";
}

async function fetchJSON(url) {
  const r = await fetch(url, { cache: "no-store" });
  if (!r.ok) throw new Error(r.status + " " + r.statusText);
  return r.json();
}

async function refreshStats() {
  try {
    const s = await fetchJSON("/api/stats");
    els.statTotal.textContent = s.total.toLocaleString();
    els.stat24h.textContent = s.last_24h.toLocaleString();
    els.statBlocked.textContent = s.blocked_count.toLocaleString();
    els.statSessions.textContent = s.sessions.toLocaleString();
    els.auditPath.textContent = s.audit_db_path || "";

    if (s.recent_blocked && s.recent_blocked.length > 0) {
      els.recentBlockedSection.style.display = "";
      els.recentBlockedList.innerHTML = "";
      for (const ev of s.recent_blocked) {
        const li = document.createElement("li");
        li.dataset.id = ev.id;
        li.textContent = `${fmtTime(ev.ts)}  •  ${ev.upstream || "—"}  •  ${ev.method || "—"}  (id ${ev.msg_id || "—"})`;
        li.addEventListener("click", () => openDetail(ev.id));
        els.recentBlockedList.appendChild(li);
      }
    } else {
      els.recentBlockedSection.style.display = "none";
    }
    setLive(true);
  } catch (err) {
    setLive(false);
    console.error("stats", err);
  }
}

async function refreshEvents() {
  try {
    const evs = await fetchJSON("/api/events?limit=200");
    renderEvents(evs);
    setLive(true);
  } catch (err) {
    setLive(false);
    console.error("events", err);
  }
}

function renderEvents(evs) {
  const tbody = els.eventsTbody;
  tbody.innerHTML = "";
  for (const ev of evs) {
    const tr = document.createElement("tr");
    tr.dataset.id = ev.id;

    const status = eventStatus(ev);
    tr.appendChild(td(fmtTime(ev.ts)));
    tr.appendChild(td(tag(ev.direction, "tag-" + ev.direction)));
    tr.appendChild(td(tag(ev.msg_type, ev.msg_type === "error" ? "tag-error" : "")));
    tr.appendChild(td(fmtMethod(ev)));
    tr.appendChild(td(fmtSession(ev.session_id)));
    tr.appendChild(td(ev.upstream || "—"));
    tr.appendChild(td(tag(status.label, status.cls)));
    tr.appendChild(td(fmtBytes(ev.bytes)));

    tr.addEventListener("click", () => openDetail(ev.id));
    tbody.appendChild(tr);
  }
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

async function openDetail(id) {
  try {
    const ev = await fetchJSON("/api/event/" + id);
    els.modalTitle.textContent = `Event #${ev.id} — ${ev.method || ev.msg_type}`;
    els.modalMeta.innerHTML = "";
    const fields = [
      ["Time", new Date(Math.floor(ev.ts / 1e6)).toISOString()],
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
      const dt = document.createElement("dt");
      dt.textContent = k;
      const dd = document.createElement("dd");
      dd.textContent = v;
      div.appendChild(dt);
      div.appendChild(dd);
      els.modalMeta.appendChild(div);
    }
    els.modalPayload.textContent = prettyJSON(ev.payload);
    els.modal.style.display = "flex";
  } catch (err) {
    console.error("event detail", err);
  }
}

function prettyJSON(s) {
  try {
    return JSON.stringify(JSON.parse(s), null, 2);
  } catch {
    return s;
  }
}

function closeDetail() {
  els.modal.style.display = "none";
}

els.modalClose.addEventListener("click", closeDetail);
els.modal.addEventListener("click", (e) => {
  if (e.target === els.modal) closeDetail();
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeDetail();
});

let pollHandle = null;
function startPoll() {
  refreshAll();
  pollHandle = setInterval(() => {
    if (els.autorefresh.checked) refreshAll();
  }, POLL_INTERVAL_MS);
}
async function refreshApprovals() {
  try {
    const list = await fetchJSON("/api/approvals");
    if (!list || list.length === 0) {
      els.pendingSection.style.display = "none";
      els.pendingList.innerHTML = "";
      return;
    }
    els.pendingSection.style.display = "";
    renderPending(list);
  } catch (err) {
    console.error("approvals", err);
  }
}

function renderPending(list) {
  els.pendingList.innerHTML = "";
  for (const a of list) {
    const card = document.createElement("div");
    card.className = "approval";
    card.dataset.id = a.id;

    const head = document.createElement("div");
    head.className = "approval-head";
    const title = document.createElement("div");
    title.className = "approval-title";
    title.innerHTML = `<b>${escapeHTML(a.tool_name || a.method)}</b> on ${escapeHTML(a.upstream || "—")} <span style="color:var(--text-dim)">(msg ${escapeHTML(a.msg_id)})</span>`;
    const score = document.createElement("span");
    score.className = "approval-score";
    score.textContent = "risk " + a.risk_score;
    head.appendChild(title);
    head.appendChild(score);

    const meta = document.createElement("div");
    meta.className = "approval-meta";
    meta.textContent = `id ${a.id}  •  session ${(a.session_id || "").slice(0, 8)}  •  queued ${fmtTime(a.created_at)}`;

    const findings = document.createElement("div");
    findings.className = "approval-findings";
    try {
      const list = JSON.parse(a.findings) || [];
      if (list.length > 0) {
        findings.textContent = "Findings:";
        const ul = document.createElement("ul");
        for (const f of list) {
          const li = document.createElement("li");
          li.textContent = `[${f.Severity || f.severity}] ${f.Category || f.category}/${f.Rule || f.rule} at ${f.JSONPath || f.jsonpath}`;
          ul.appendChild(li);
        }
        findings.appendChild(ul);
      }
    } catch {}

    // Remember checkbox — collects the rule IDs that would be auto-decided.
    const ruleIDs = collectRuleIDs(a.findings);
    const remember = document.createElement("label");
    remember.className = "approval-remember";
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.id = "remember-" + a.id;
    remember.appendChild(cb);
    const text = document.createElement("span");
    text.appendChild(document.createTextNode("Remember this decision for "));
    if (ruleIDs.length === 0) {
      text.appendChild(document.createTextNode("(no rules)"));
      cb.disabled = true;
    } else {
      ruleIDs.forEach((rid, i) => {
        const code = document.createElement("code");
        code.textContent = rid;
        text.appendChild(code);
        if (i < ruleIDs.length - 1) text.appendChild(document.createTextNode(", "));
      });
    }
    remember.appendChild(text);

    const actions = document.createElement("div");
    actions.className = "approval-actions";
    const approveBtn = document.createElement("button");
    approveBtn.className = "btn btn-approve";
    approveBtn.textContent = "Approve";
    const denyBtn = document.createElement("button");
    denyBtn.className = "btn btn-deny";
    denyBtn.textContent = "Deny";
    approveBtn.addEventListener("click", () =>
      resolveApproval(a.id, "approve", [approveBtn, denyBtn], cb.checked));
    denyBtn.addEventListener("click", () =>
      resolveApproval(a.id, "deny", [approveBtn, denyBtn], cb.checked));
    actions.appendChild(approveBtn);
    actions.appendChild(denyBtn);

    card.appendChild(head);
    card.appendChild(meta);
    if (findings.textContent) card.appendChild(findings);
    card.appendChild(remember);
    card.appendChild(actions);

    // Click-through to detail for the original request payload.
    card.addEventListener("dblclick", () => showRawPayload(a));

    els.pendingList.appendChild(card);
  }
}

async function resolveApproval(id, action, btns, remember) {
  btns.forEach((b) => (b.disabled = true));
  try {
    const url = `/api/approvals/${id}/${action}` + (remember ? "?remember=true" : "");
    const r = await fetch(url, { method: "POST" });
    if (!r.ok) {
      const text = await r.text();
      alert(`Could not ${action} #${id}: ${text}`);
      btns.forEach((b) => (b.disabled = false));
      return;
    }
    await Promise.all([refreshApprovals(), refreshStats(), refreshAutoDecisions()]);
  } catch (err) {
    alert(`Network error: ${err.message}`);
    btns.forEach((b) => (b.disabled = false));
  }
}

function collectRuleIDs(findingsJSON) {
  try {
    const list = JSON.parse(findingsJSON) || [];
    const seen = new Set();
    for (const f of list) {
      const cat = f.Category || f.category;
      const rule = f.Rule || f.rule;
      if (cat && rule) seen.add(cat + "/" + rule);
    }
    return Array.from(seen);
  } catch {
    return [];
  }
}

async function refreshAutoDecisions() {
  try {
    const list = await fetchJSON("/api/auto-decisions");
    if (!list || list.length === 0) {
      els.autoSection.style.display = "none";
      els.autoList.innerHTML = "";
      return;
    }
    els.autoSection.style.display = "";
    els.autoList.innerHTML = "";
    for (const ad of list) {
      const row = document.createElement("div");
      row.className = "auto-entry";

      const left = document.createElement("div");
      const rule = document.createElement("span");
      rule.className = "rule";
      rule.textContent = ad.rule_id + " → ";
      const dec = document.createElement("span");
      dec.className = ad.decision === "approved" ? "decision-allow" : "decision-deny";
      dec.textContent = ad.decision === "approved" ? "ALLOW" : "DENY";
      left.appendChild(rule);
      left.appendChild(dec);
      const meta = document.createElement("div");
      meta.className = "meta";
      meta.textContent = `set by ${ad.created_by} • ${new Date(Math.floor(ad.created_at / 1e6)).toLocaleString()}`;
      left.appendChild(meta);

      const removeBtn = document.createElement("button");
      removeBtn.className = "btn-remove";
      removeBtn.textContent = "Remove";
      removeBtn.addEventListener("click", () => removeAutoDecision(ad.rule_id));

      row.appendChild(left);
      row.appendChild(removeBtn);
      els.autoList.appendChild(row);
    }
  } catch (err) {
    console.error("auto-decisions", err);
  }
}

async function removeAutoDecision(ruleID) {
  try {
    const r = await fetch("/api/auto-decisions/" + encodeURIComponent(ruleID), { method: "DELETE" });
    if (!r.ok && r.status !== 204) {
      const text = await r.text();
      alert(`Could not remove ${ruleID}: ${text}`);
      return;
    }
    await refreshAutoDecisions();
  } catch (err) {
    alert(`Network error: ${err.message}`);
  }
}

function showRawPayload(a) {
  els.modalTitle.textContent = `Pending approval #${a.id}`;
  els.modalMeta.innerHTML = "";
  for (const [k, v] of [
    ["Tool", a.tool_name || "—"],
    ["Method", a.method],
    ["Upstream", a.upstream],
    ["Session", a.session_id],
    ["JSON-RPC id", a.msg_id],
    ["Queued", new Date(Math.floor(a.created_at / 1e6)).toISOString()],
    ["Risk score", String(a.risk_score)],
  ]) {
    const div = document.createElement("div");
    const dt = document.createElement("dt");
    dt.textContent = k;
    const dd = document.createElement("dd");
    dd.textContent = v;
    div.appendChild(dt);
    div.appendChild(dd);
    els.modalMeta.appendChild(div);
  }
  els.modalPayload.textContent = prettyJSON(a.payload);
  els.modal.style.display = "flex";
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

async function refreshAll() {
  await Promise.all([
    refreshStats(),
    refreshEvents(),
    refreshApprovals(),
    refreshAutoDecisions(),
  ]);
}

els.autorefresh.addEventListener("change", () => {
  if (els.autorefresh.checked) refreshAll();
});

startPoll();
