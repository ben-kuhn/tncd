"use strict";
const $ = (id) => document.getElementById(id);
const MAX_LINES = 2000;
const filters = { rx: true, tx: true, conn: true };

// --- snapshots (status + connections) on a timer ---
async function refresh() {
  try {
    const st = await (await fetch("api/status")).json();
    $("version").textContent = st.version || "";
    $("ports").innerHTML = (st.ports || []).map(p =>
      `<div class="port"><span class="dot ${p.online ? "dot-on" : "dot-off"}"></span>`
      + `<span class="name">#${p.port} ${esc(p.name || p.type)}</span>`
      + `<span class="cnt">rx ${p.rx_frames} · tx ${p.tx_frames}</span>`
      + `<button class="relink" data-port="${p.port}" title="Cycle this port's transport (close + reconnect)">relink</button>`
      + `</div>`).join("");
  } catch (e) {}
  try {
    const cn = await (await fetch("api/connections")).json();
    const rows = cn.connections || [];
    $("conn-count").textContent = rows.length ? `(${rows.length})` : "";
    const body = $("conn-body");
    if (!rows.length) { body.innerHTML = `<tr><td colspan="10" class="muted">No active connections.</td></tr>`; return; }
    body.innerHTML = rows.map(c =>
      `<tr><td>${c.port}</td><td>${esc(c.local)}</td><td>${esc(c.remote)}</td>`
      + `<td class="state-${c.state}">${c.state}</td>`
      + `<td>${c.send_seq}/${c.recv_seq}</td><td>${c.unacked}</td><td>${c.send_queue}</td>`
      + `<td>${c.t1_retries}</td><td>${fmtRtt(c.srtt_ms)}</td>`
      + `<td>${c.srej ? "yes" : "—"}</td></tr>`).join("");
  } catch (e) {}
}
function fmtRtt(ms) { return ms > 0 ? (ms >= 1000 ? (ms/1000).toFixed(1)+"s" : ms+"ms") : "—"; }

// --- live event stream (SSE) ---
function connectSSE() {
  const es = new EventSource("api/events");
  es.onopen = () => setSSE(true);
  es.onerror = () => setSSE(false);
  for (const t of ["rx", "tx", "connect", "disconnect"]) {
    es.addEventListener(t, (e) => appendEvent(t, JSON.parse(e.data)));
  }
}
function setSSE(ok) {
  $("sse-status").className = "dot " + (ok ? "dot-on" : "dot-off");
  $("sse-label").textContent = ok ? "connected" : "reconnecting…";
}

function appendEvent(type, d) {
  const box = $("events");
  const atBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  const line = document.createElement("div");
  const cls = (type === "rx" || type === "tx") ? "ev-" + type
            : (type === "connect" ? "ev-connect" : "ev-disconnect");
  const filterKey = (type === "rx" || type === "tx") ? type : "conn";
  line.className = "ev " + cls;
  line.dataset.f = filterKey;
  if (type === "connect") {
    line.innerHTML = `<span class="ts">${now()}</span>  ● connect    <span class="call">${esc(d.remote)}</span>`
      + (d.incoming ? " (incoming)" : "");
  } else if (type === "disconnect") {
    line.innerHTML = `<span class="ts">${now()}</span>  ○ disconnect <span class="call">${esc(d.remote)}</span>`;
  } else {
    const arrow = type === "rx" ? "←" : "→";
    const payload = d.data ? `  <span class="data">"${esc(decodePreview(d.data))}"</span>` : "";
    line.innerHTML = `<span class="ts">${now()}</span>  <span class="arrow">${arrow} ${type}</span>  `
      + `<span class="call">${esc(d.from)} → ${esc(d.to)}</span>  <span class="type">${d.type}</span>`
      + ` len ${d.len}${payload}`;
  }
  if (!filters[filterKey]) line.style.display = "none";
  box.appendChild(line);
  while (box.childElementCount > MAX_LINES) box.removeChild(box.firstElementChild);
  if (atBottom) box.scrollTop = box.scrollHeight;
}
function now() { return new Date().toTimeString().slice(0, 8); }

// base64 -> short printable preview (non-printable shown as ·, truncated)
function decodePreview(b64) {
  let s;
  try { s = atob(b64); } catch (e) { return ""; }
  let out = "";
  for (let i = 0; i < s.length && i < 40; i++) {
    const c = s.charCodeAt(i);
    out += (c >= 0x20 && c < 0x7f) ? s[i] : "·";
  }
  if (s.length > 40) out += "…";
  return out;
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"]/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}

// --- filters ---
document.querySelectorAll(".filters button").forEach((btn) => {
  btn.addEventListener("click", () => {
    const f = btn.dataset.f;
    filters[f] = !filters[f];
    btn.classList.toggle("on", filters[f]);
    document.querySelectorAll(`.ev[data-f="${f}"]`).forEach((el) => {
      el.style.display = filters[f] ? "" : "none";
    });
  });
});

// --- manual relink (POST /api/ports/{n}/reconnect) ---
// Delegated so it survives the ports panel re-rendering every refresh.
$("ports").addEventListener("click", async (e) => {
  const btn = e.target.closest("button.relink");
  if (!btn || btn.dataset.busy) return;
  const port = btn.dataset.port;
  btn.dataset.busy = "1";
  btn.disabled = true;
  btn.textContent = "relinking…";
  try {
    const resp = await fetch(`api/ports/${port}/reconnect`, { method: "POST" });
    btn.textContent = resp.ok ? "relinked" : "failed";
  } catch (_) {
    btn.textContent = "failed";
  }
  // Refresh repaints the panel shortly; clear the busy latch so the fresh
  // button is clickable again.
  setTimeout(() => { delete btn.dataset.busy; }, 2500);
});

// --- boot ---
refresh();
setInterval(refresh, 2000);
connectSSE();
