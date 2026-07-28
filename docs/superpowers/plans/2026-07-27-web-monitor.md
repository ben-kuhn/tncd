# Web Monitor UI Implementation Plan (phase 2.5)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A tiny embedded real-time web monitor, served by tncd's `[api]` server, that shows the live event feed, port status/counters, and active connections — improving usability (especially on Windows) with no new dependencies.

**Architecture:** Static HTML/CSS/JS embedded in the `internal/frontend/api` package via `go:embed`. When `[api] serve_ui` is on, `api.Serve` registers a root handler serving the embedded page; the page consumes the existing `/api/status`, `/api/connections`, and `/api/events` (SSE) endpoints same-origin. Vanilla JS + browser-native `EventSource`. No framework, no build step.

**Tech Stack:** Go stdlib (`embed`, `net/http`, `io/fs`, `net/http/httptest`); vanilla HTML/CSS/JS in the browser. No third-party dependencies.

## Global Constraints

- Module path `github.com/ben-kuhn/tncd/v2`. `go` at `/nix/store/gb0njhqswlc5n127ikgyikvq39r40l6f-go-1.26.4/bin/go` if not on PATH. No gcc — `CGO_ENABLED=0` for `go test`/`go vet`.
- Branch `feature/web-monitor` off `main`. Commit per task. No version bump (a later release cuts the tag).
- **No new third-party dependencies; no build step; no JS framework.** Stdlib + vanilla only.
- **Read-only**; unchanged security posture (unauthenticated, localhost-by-default `[api]` server; no auth — a non-goal).
- `serve_ui` default **true** when `[api]` is enabled; `serve_ui = false` ⇒ JSON API only, `GET /` returns 404.
- **Theme = SkyNetControl slate + cyan. No purple/violet/indigo.** Exact palette in Task 3.
- The existing `/api/*` routes, the `[api]` monitor tests, and the AGWPE monitor stay unchanged.

---

### Task 1: `[api] serve_ui` config

**Files:**
- Modify: `internal/config/config.go` (`APIConfig`, `knownAPIKeys`, `[api]` parse)
- Modify: `internal/config/example.go` (the commented `[api]` block)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `config.APIConfig.ServeUI bool` (from `serve_ui`, default `true`).

- [ ] **Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestAPIServeUIDefaultTrue(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n\n[api]\nenabled=true\n"), 0o644)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.API.ServeUI {
		t.Fatal("serve_ui should default to true when [api] present")
	}
}

func TestAPIServeUIExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/t.ini"
	os.WriteFile(path, []byte("[client.0]\ntype=serial\ndevice=/dev/null\n\n[api]\nenabled=true\nserve_ui=false\n"), 0o644)
	cfg, _ := Load(path)
	if cfg.API.ServeUI {
		t.Fatal("serve_ui=false must disable the UI")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/config/ -run APIServeUI -v`
Expected: FAIL — `cfg.API.ServeUI` undefined.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the field to `APIConfig` (which has `Enabled`, `ListenHost`, `ListenPort`, `MaxClients`):

```go
	ServeUI    bool   // default true — serve the embedded web monitor at /
```

Add `serve_ui` to `knownAPIKeys` (the list currently `{"enabled", "listen_host", "listen_port", "max_clients"}`):

```go
var knownAPIKeys = []string{"enabled", "listen_host", "listen_port", "max_clients", "serve_ui"}
```

In the `[api]` parse block in `Load` (currently sets `Enabled`/`ListenHost`/`ListenPort`/`MaxClients`), add:

```go
		ServeUI:    getBool(apiSec, "serve_ui", true),
```

- [ ] **Step 4: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./internal/config/ -run APIServeUI -v` — Expected: PASS.

- [ ] **Step 5: Add serve_ui to the example config**

In `internal/config/example.go`, find the commented `[api]` block (grep `grep -n "\[api\]" internal/config/example.go`; it lists `enabled`/`listen_host`/`listen_port`/`max_clients` with the UNAUTHENTICATED note). Add a commented `serve_ui` line to it, e.g. after `max_clients`:

```
# serve_ui = true       # serve the embedded web monitor at http://<host>:<port>/
```

Confirm the example still loads: `CGO_ENABLED=0 go test ./internal/config/ -run ExampleLoads -v` (or the full config suite).

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/config/example.go
git commit -m "feat(config): [api] serve_ui toggle (default true)"
```

---

### Task 2: Embed + serve gating (with a stub page)

**Files:**
- Create: `internal/frontend/api/ui/index.html` (temporary stub — Task 3 replaces it)
- Create: `internal/frontend/api/ui.go` (`//go:embed` + root handler)
- Modify: `internal/frontend/api/server.go` (`Serve` gains a `serveUI` param; register root handler)
- Modify: `cmd/tncd/main.go` (pass `cfg.API.ServeUI`)
- Test: `internal/frontend/api/server_test.go`

**Interfaces:**
- Consumes: `config.APIConfig.ServeUI` (Task 1).
- Produces:
  - `api.Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int, serveUI bool) (*Server, error)` (added trailing `serveUI bool`).
  - `internal/frontend/api/ui.go`: `//go:embed ui` `var uiFS embed.FS` and `func uiHandler() (http.Handler, error)` returning a file server over the `ui` subtree.

- [ ] **Step 1: Create the stub page**

Create `internal/frontend/api/ui/index.html` (minimal — Task 3 replaces it with the real monitor; this exists so the embed compiles and serving is testable now):

```html
<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>tncd monitor</title></head>
<body><div id="app">tncd monitor</div></body></html>
```

- [ ] **Step 2: Write the failing test**

The existing `server_test.go` builds a bridge via `newBridge(t, eng)` and calls `Serve(...)`. Update those existing `Serve(...)` calls to pass the new trailing `serveUI` arg (use `true` for the existing status/SSE tests). Add these tests:

```go
func TestServeUIEnabledServesRoot(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET / = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("GET / content-type = %q, want text/html", ct)
	}
	// /api still works with the UI on
	r2, _ := http.Get("http://" + srv.Addr() + "/api/status")
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("GET /api/status = %d, want 200", r2.StatusCode)
	}
}

func TestServeUIDisabledRootIs404(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOnLoop(eng, srv)

	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("GET / with serve_ui=false = %d, want 404", resp.StatusCode)
	}
	r2, _ := http.Get("http://" + srv.Addr() + "/api/status") // API still served
	defer r2.Body.Close()
	if r2.StatusCode != 200 {
		t.Fatalf("GET /api/status = %d, want 200", r2.StatusCode)
	}
}

func TestUIEmbedHasIndex(t *testing.T) {
	if _, err := uiFS.ReadFile("ui/index.html"); err != nil {
		t.Fatalf("embedded ui/index.html missing: %v", err)
	}
}
```

(Ensure `strings` is imported in `server_test.go`.)

- [ ] **Step 3: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./internal/frontend/api/ -run 'ServeUI|UIEmbed' -v`
Expected: FAIL — `uiFS` undefined and `Serve` has the wrong arity.

- [ ] **Step 4: Create the embed + handler**

Create `internal/frontend/api/ui.go`:

```go
package api

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui
var uiFS embed.FS

// uiHandler serves the embedded web monitor (the ui/ subtree) at /.
func uiHandler() (http.Handler, error) {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		return nil, err
	}
	return http.FileServer(http.FS(sub)), nil
}
```

- [ ] **Step 5: Wire `serveUI` into `Serve`**

In `internal/frontend/api/server.go`, change the `Serve` signature and register the root handler when `serveUI`. The current function creates `mux`, registers the three `/api/*` routes, and sets `s.httpSrv`. Add the param and the root registration:

```go
func Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int, serveUI bool) (*Server, error) {
	// ... existing listen + Server construction ...
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/connections", s.handleConnections)
	mux.HandleFunc("/api/events", s.handleEvents)
	if serveUI {
		h, err := uiHandler()
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("api: ui handler: %w", err)
		}
		mux.Handle("/", h)
	}
	s.httpSrv = &http.Server{Handler: mux}
	// ... existing sink registration + go s.httpSrv.Serve(ln) + return ...
}
```

(`fmt` is already imported in server.go. The `mux.Handle("/", h)` only registers when `serveUI` — with it absent, unmatched paths get ServeMux's built-in 404, and the more-specific `/api/*` patterns still match.)

- [ ] **Step 6: Update main.go**

In `cmd/tncd/main.go`, the API start passes `cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.MaxClients`. Add `cfg.API.ServeUI`:

```go
		apiSrv, err = apiserver.Serve(eng, b, cfg.API.ListenHost, cfg.API.ListenPort, cfg.API.MaxClients, cfg.API.ServeUI)
```

- [ ] **Step 7: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./internal/frontend/api/ -v && CGO_ENABLED=0 go build ./...`
Expected: PASS (new gating/embed tests + the existing api tests, whose `Serve(...)` calls now pass `true`); build clean.

- [ ] **Step 8: Commit**

```bash
git add internal/frontend/api/ui.go internal/frontend/api/ui/index.html internal/frontend/api/server.go internal/frontend/api/server_test.go cmd/tncd/main.go
git commit -m "feat(api): embed + serve the web monitor at / gated by serve_ui"
```

---

### Task 3: The real web monitor UI

**Files:**
- Modify: `internal/frontend/api/ui/index.html` (replace the stub)
- Create: `internal/frontend/api/ui/monitor.css`
- Create: `internal/frontend/api/ui/monitor.js`
- Test: `internal/frontend/api/server_test.go` (tighten the root-page marker assertions)

**Interfaces:**
- Consumes: the embed + root handler (Task 2); the API endpoints `/api/status`, `/api/connections`, `/api/events`.
- Produces: the page markup with element IDs `version`, `sse-status`, `ports`, `connections`, `events` (asserted by the marker test).

- [ ] **Step 1: Write `index.html`**

Replace `internal/frontend/api/ui/index.html` with:

```html
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>tncd monitor</title>
  <link rel="stylesheet" href="monitor.css">
</head>
<body>
  <header id="header">
    <div class="brand">tncd monitor <span id="version" class="muted"></span></div>
    <div class="sse"><span id="sse-status" class="dot dot-off"></span><span id="sse-label">connecting…</span></div>
  </header>
  <div id="ports" class="ports"></div>

  <section class="panel conns-panel">
    <div class="panel-head">Connections <span id="conn-count" class="muted"></span></div>
    <table id="connections">
      <thead><tr>
        <th>port</th><th>local</th><th>remote</th><th>state</th>
        <th>V(S)/V(R)</th><th>unack</th><th>queue</th><th>T1</th><th>RTT</th><th>SREJ</th>
      </tr></thead>
      <tbody id="conn-body"><tr><td colspan="10" class="muted">No active connections.</td></tr></tbody>
    </table>
  </section>

  <section class="panel events-panel">
    <div class="panel-head">
      Events
      <span class="filters">
        <button data-f="rx" class="on">rx</button>
        <button data-f="tx" class="on">tx</button>
        <button data-f="conn" class="on">conn</button>
      </span>
    </div>
    <div id="events" class="events"></div>
  </section>

  <script src="monitor.js"></script>
</body>
</html>
```

- [ ] **Step 2: Write `monitor.css` (SkyNetControl slate + cyan)**

Create `internal/frontend/api/ui/monitor.css`:

```css
:root{
  --bg-base:#0c1222; --bg-surface:#0f172a; --bg-elevated:#1e293b; --border:#1e293b;
  --text-primary:#f1f5f9; --text-secondary:#cbd5e1; --text-muted:#94a3b8;
  --accent:#22d3ee; --accent-hover:#06b6d4;
  --success:#22c55e; --warning:#fbbf24; --danger:#ef4444;
  --font-sans:system-ui,-apple-system,sans-serif;
  --font-mono:'SF Mono','Cascadia Code','JetBrains Mono','Fira Code',monospace;
}
*{box-sizing:border-box;margin:0;padding:0}
html,body{height:100%}
body{display:flex;flex-direction:column;height:100vh;background:var(--bg-base);
  color:var(--text-primary);font-family:var(--font-sans);font-size:14px}
.muted{color:var(--text-muted)}
#header{display:flex;align-items:center;justify-content:space-between;
  padding:.5rem .9rem;background:var(--bg-surface);border-bottom:1px solid var(--border)}
.brand{font-weight:600;color:var(--accent);font-family:var(--font-mono)}
.sse{display:flex;align-items:center;gap:.4rem;color:var(--text-secondary)}
.dot{width:9px;height:9px;border-radius:50%;display:inline-block}
.dot-on{background:var(--success)} .dot-off{background:var(--danger)}
.ports{display:flex;flex-wrap:wrap;gap:.5rem;padding:.5rem .9rem;
  background:var(--bg-surface);border-bottom:1px solid var(--border)}
.port{display:flex;align-items:center;gap:.5rem;padding:.25rem .6rem;
  background:var(--bg-elevated);border:1px solid var(--border);border-radius:6px;font-size:13px}
.port .name{color:var(--text-primary)} .port .cnt{color:var(--text-muted);font-family:var(--font-mono)}
.panel{background:var(--bg-surface);border-bottom:1px solid var(--border)}
.panel-head{display:flex;align-items:center;justify-content:space-between;
  padding:.4rem .9rem;color:var(--text-secondary);text-transform:uppercase;
  font-size:11px;letter-spacing:.5px;border-bottom:1px solid var(--border)}
.conns-panel{flex:0 0 auto;max-height:30vh;overflow:auto}
table{width:100%;border-collapse:collapse;font-size:13px;font-family:var(--font-mono)}
th,td{text-align:left;padding:.3rem .9rem;white-space:nowrap}
th{color:var(--text-muted);font-weight:500}
tbody tr{border-top:1px solid var(--border)}
.state-connected{color:var(--success)} .state-connecting{color:var(--warning)}
.filters button{background:var(--bg-elevated);color:var(--text-muted);
  border:1px solid var(--border);border-radius:4px;padding:.1rem .5rem;
  margin-left:.3rem;cursor:pointer;font-size:11px}
.filters button.on{color:var(--accent);border-color:var(--accent)}
.events-panel{flex:1 1 auto;display:flex;flex-direction:column;min-height:0}
.events{flex:1 1 auto;overflow-y:auto;padding:.3rem .9rem;
  font-family:var(--font-mono);font-size:12.5px;line-height:1.5}
.ev{white-space:pre-wrap;word-break:break-word}
.ev .ts{color:var(--text-muted)}
.ev-rx .arrow{color:var(--accent)} .ev-tx .arrow{color:var(--warning)}
.ev-connect{color:var(--success)} .ev-disconnect{color:var(--text-muted)}
.ev .call{color:var(--text-primary)} .ev .type{color:var(--text-secondary)}
.ev .data{color:var(--text-secondary)}
```

(This is a pure `.css` file — no `<style>` tags. Step 6 greps to confirm none slipped in.)

- [ ] **Step 3: Write `monitor.js`**

Create `internal/frontend/api/ui/monitor.js`:

```js
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
      + `<span class="cnt">rx ${p.rx_frames} · tx ${p.tx_frames}</span></div>`).join("");
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

// --- boot ---
refresh();
setInterval(refresh, 2000);
connectSSE();
```

- [ ] **Step 4: Tighten the marker test**

In `internal/frontend/api/server_test.go`, extend `TestServeUIEnabledServesRoot` (or add a test) to read the body and assert the real page shipped — the three panel IDs and the title:

```go
	body, _ := io.ReadAll(resp.Body)
	for _, marker := range []string{"tncd monitor", `id="ports"`, `id="connections"`, `id="events"`, "monitor.js"} {
		if !strings.Contains(string(body), marker) {
			t.Fatalf("root page missing marker %q", marker)
		}
	}
```

(Add `io` to the imports. Read the body once — if `TestServeUIEnabledServesRoot` already checks content-type, fold this in before closing the body.)

- [ ] **Step 5: Run + build + vet**

Run:
```
CGO_ENABLED=0 go test ./internal/frontend/api/ -v && CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go vet ./...
```
Expected: PASS; build + vet clean.

- [ ] **Step 6: Structural sanity of the assets**

Confirm the assets are well-formed and complete (no automated JS test — no JS tooling in this env):
- `python3 -c "from html.parser import HTMLParser as P; import sys; p=P(); p.feed(open('internal/frontend/api/ui/index.html').read()); print('html ok')"`
- Confirm the CSS has no stray tags: `grep -c "</style>\|<style>" internal/frontend/api/ui/monitor.css` should print `0` (the transcription-guard line from Step 2 must NOT be in the file).
- `grep -q "api/events" internal/frontend/api/ui/monitor.js && grep -q "api/status" internal/frontend/api/ui/monitor.js && echo "js endpoints ok"`

- [ ] **Step 7: Manual browser smoke (documented, not automated)**

For the reviewer/user, not CI: run tncd with `[api] enabled=true`, open `http://127.0.0.1:8002/`, and confirm the page loads dark (slate+cyan, no purple), the SSE dot goes green, port counters update, and events scroll. Note this in the report as PENDING-MANUAL.

- [ ] **Step 8: Commit**

```bash
git add internal/frontend/api/ui/ internal/frontend/api/server_test.go
git commit -m "feat(api): real-time web monitor page (slate+cyan; events/status/connections)"
```

---

## Final verification

- [ ] `CGO_ENABLED=0 go test ./...` — all pass.
- [ ] `CGO_ENABLED=0 go vet ./...` — clean.
- [ ] `CGO_ENABLED=0 go build ./...` — single static binary, no new deps (`git diff --stat go.mod go.sum` shows no change).
- [ ] `serve_ui=true` (default) ⇒ `GET /` serves the monitor (markers present), `/api/*` intact; `serve_ui=false` ⇒ `GET /` 404, `/api/*` intact.
- [ ] The page uses the SkyNetControl slate+cyan palette; no purple/violet/indigo (`grep -riE "purple|violet|indigo|8b5cf6|6366f1|a855f7" internal/frontend/api/ui/` returns nothing).
- [ ] AGWPE monitor + existing `[api]` tests unchanged and green.
- [ ] Merge `feature/web-monitor` → `main` with `--no-ff` after final review. No version bump.
