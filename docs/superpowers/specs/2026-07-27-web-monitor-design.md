# Web Monitor UI Design (phase 2.5)

**Status:** Approved design.
**Date:** 2026-07-27.
**Context:** Phase 2 shipped the read-only JSON/SSE API (`/api/status`,
`/api/connections`, `/api/events`) in v1.100-Beta. This "phase 2.5" adds a tiny
browser-based real-time monitor that consumes that API — a demo that is also
genuinely useful, especially on **Windows**, where tailing terminal logs is
awkward and native logging is poor. It makes the Windows build meaningfully more
usable ahead of phase 4.

## Purpose

Serve a single, self-contained web page from tncd's existing `[api]` HTTP server
that shows, in real time: the live event feed, per-port status + counters, and
the active AX.25 connections with their troubleshooting fields. Read-only, no
controls — a comfortable window into what the terminal logs and the raw API
already expose.

## Decisions (settled during brainstorming)

| Question | Decision |
|---|---|
| Content | **All three panels** — live event log, port status strip, active-connections table. |
| Layout | **Event log dominant** (2/3–3/4+ of the viewport); status strip + connections table compact (≤3–4 connections expected). |
| Tech | **Embedded static files** (`go:embed`), **vanilla HTML/CSS/JS**, no framework, no build step, no new dependencies. Browser-native `EventSource` for SSE. |
| Gating | Separate **`[api] serve_ui`** toggle, default **true** when `[api]` is enabled (API-only users set it false). |
| Theme | Match **SkyNetControl** (slate + cyan). **No purple/violet.** |
| Security | Unchanged — served over the same unauthenticated, localhost-by-default `[api]` server. No auth (explicit non-goal). |

## Architecture

The UI is a small set of static assets embedded into the `internal/frontend/api`
package binary via `//go:embed`. No Node, no toolchain, no build step — tncd stays
a single static binary (`CGO_ENABLED=0`).

The existing `api.Server` (a `net/http` `ServeMux`) gains a **root handler** when
`serve_ui` is enabled: requests that are not under `/api/` are served from the
embedded `embed.FS` (via `http.FileServer`/`http.FS`). The `/api/*` routes are
untouched. When `serve_ui` is off, no root handler is registered (only `/api/*`
is served; `GET /` returns 404).

The page is served **same-origin** as the API, so it calls `/api/status`,
`/api/connections`, and `/api/events` with no CORS. `EventSource` gives auto-
reconnect for free (the reason SSE was chosen over WebSocket).

A framework (Vue/Svelte/React + build) was considered and **rejected**: it drags
a Node build toolchain into a no-dependency Go project for no real benefit at
this scale. Vanilla + `EventSource` is sufficient for a read-only monitor.

### Files

```
internal/frontend/api/ui/index.html   page structure + panels
internal/frontend/api/ui/monitor.css  slate+cyan theme (SkyNetControl palette)
internal/frontend/api/ui/monitor.js   fetch/EventSource logic, rendering, filters
internal/frontend/api/ui.go           //go:embed ui  (embed.FS) + the root handler
```

## Theme (SkyNetControl palette)

CSS custom properties, dark:

```
--bg-base:      #0c1222
--bg-surface:   #0f172a
--bg-elevated:  #1e293b
--border-color: #1e293b
--text-primary:   #f1f5f9
--text-secondary: #cbd5e1
--text-muted:     #94a3b8
--accent:       #22d3ee   /* cyan */
--accent-hover: #06b6d4
--success: #22c55e   --warning: #fbbf24   --danger: #ef4444
--font-sans: system-ui, -apple-system, sans-serif
--font-mono: 'SF Mono', 'Cascadia Code', 'JetBrains Mono', 'Fira Code', monospace
```

No purple/violet/indigo anywhere.

## Layout

```
┌──────────────────────────────────────────────────────────────────┐
│ tncd monitor   v1.100-Beta   ● connected            [header strip] │  thin
│ Port 0  Direwolf (TS-2000)  ● online   rx 274  tx 162              │
├──────────────────────────────────────────────────────────────────┤
│ Connections (2)                                                    │  compact,
│ port local     remote   state      V(S)/V(R) unack T1  RTT  SREJ   │  auto-height
│  0   KU0HN-10  W0NE-10  connected   5/3       2    0   1.2s  —      │
├──────────────────────────────────────────────────────────────────┤
│ Events                                    [rx] [tx] [conn] filters │  DOMINANT,
│ 11:40:44  ● connect    W0NE-10                                     │  flex-grow,
│ 11:40:45  ← rx  W0NE-7 → KU0HN-2  I  len 1  "·"                    │  monospace,
│ 11:40:45  → tx  KU0HN-2 → W0NE-7  RR                               │  auto-scroll
│ 11:40:57  ○ disconnect W0NE-10                                     │
└──────────────────────────────────────────────────────────────────┘
```

- **Header strip** (thin): title + `version` + an SSE connection dot (green
  connected / red reconnecting). Per-port pills: name, online dot, rx/tx counters.
- **Connections** (compact, auto-height): columns state, V(S)/V(R), unacked, T1
  retries, RTT (from `srtt_ms`), SREJ badge, modulo. Empty state: "No active
  connections."
- **Events** (fills the rest): monospace, auto-scrolling, color-coded by type
  (rx=cyan, tx=amber, connect=green, disconnect=muted), with `rx`/`tx`/`conn`
  filter toggles.

## Behavior & data flow

- **On load:** one fetch of `/api/status` + `/api/connections` to populate the
  header and table; then `new EventSource('/api/events')` for the live log.
- **Snapshots on a timer:** re-poll `/api/status` + `/api/connections` every ~2s
  (their fields are point-in-time snapshots). Events arrive push-style via SSE.
- **SSE handlers:** one per event type (`rx`/`tx`/`connect`/`disconnect`) appends
  a color-coded line to a **capped ring buffer (~2000 lines)** and renders.
  Auto-scroll to bottom unless the user has scrolled up (resume when back at
  bottom).
- **Reconnect:** `EventSource` auto-reconnects on drop; header dot red on error,
  green on `onopen`.
- **Payloads:** `rx`/`tx` lines show `from → to`, frame type, and `len`. For
  `I`/`UI` frames, a short **decoded-text preview** (base64 → printable ASCII,
  non-printable shown as `·`, truncated to a small width) so APRS/text is
  readable. S-frames show no payload (none present).
- **Filters:** client-side `rx`/`tx`/`conn` toggles hide/show line types.

All logic is vanilla JS in the embedded page; the only network calls are the
three same-origin API endpoints.

## Config & serving

- New `APIConfig.ServeUI bool` from `[api] serve_ui`, **default true**. Add
  `serve_ui` to the API known-keys list and to the commented `[api]` block in the
  example config.
- `api.Serve` registers the root handler only when `ServeUI` is true.
- `//go:embed ui` in `internal/frontend/api/ui.go` exposes the `embed.FS`; the
  root handler serves it (index at `/`).

## Testing

- **Go (`net/http/httptest`):**
  - `serve_ui=true` → `GET /` returns 200 `text/html`, body contains the expected
    markers (page title + the three panel element IDs); `GET /api/status` still
    works.
  - `serve_ui=false` → `GET /` returns 404; `/api/*` still work.
  - Embed sanity: the `embed.FS` contains `index.html` (and the .css/.js).
  - Config: `serve_ui` parses; defaults to true; absent `[api]` ⇒ no server.
- **No JS test framework** (no dependencies). The UI's behavior is validated by
  opening it in a browser against a live session — trivial on the existing bench,
  and the underlying API is already OTA-proven.
- **Regression:** full `go test ./...` + `go vet ./...` green under
  `CGO_ENABLED=0`; the existing `[api]` and monitor tests unchanged.
- **No OTA gate** — a static page over an already-validated read-only API.

## Non-goals

- Any control/write actions (still read-only; the API has none to call).
- Authentication / TLS (unchanged posture; explicit non-goal).
- A JS framework, bundler, or any build step; any new dependency.
- Historical charts / persistence (live view only; counters are live-scoped as
  the API already defines).
- Multi-page app, routing, themes toggle — one page, dark, done.

## Exit criteria

1. With `[api] enabled = true` (and `serve_ui` default), `GET /` serves the
   monitor page; it shows live events, port status/counters, and active
   connections, updating in real time.
2. `serve_ui = false` serves the JSON API with no HTML page (`GET /` → 404).
3. No new dependencies; single static binary; `go test ./...` + `go vet ./...`
   pass under `CGO_ENABLED=0`.
4. Palette matches SkyNetControl (slate + cyan); no purple.
