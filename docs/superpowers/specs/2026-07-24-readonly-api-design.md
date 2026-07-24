# Read-only JSON/SSE API Design

**Status:** Approved design.
**Date:** 2026-07-24.
**Context:** Phase 2 of the tncd 2.0 Go port, sub-project 2 (the second of the
two "new frontends"). Sub-project 1 (KISS-over-TCP passthrough) shipped in
v1.99-Beta and introduced the frontend subscriber bus; it deliberately deferred
connection-state / TX events on the bus to this sub-project. This spec adds a
read-only HTTP API for monitoring and web tooling.

## Purpose

Expose tncd's live state to web tooling and dashboards as JSON, plus a streaming
event feed — the AGWPE `m` monitor stream reimagined as structured JSON, with
connection lifecycle and transmit visibility added. Read-only in 2.0: no control
actions. Disabled by default.

## Decisions (settled during brainstorming)

| Question | Decision |
|---|---|
| Event stream transport | **Server-Sent Events (SSE)** over `net/http` — stdlib only, no new dependency, unidirectional (fits read-only), native browser `EventSource`. Not WebSocket. |
| Event scope | **RX frames + TX frames + connect/disconnect.** Full channel monitor, both directions, plus lifecycle. |
| Counters | **Live-scoped**, not historical: per-port counts exist only while the port is online (reset when it goes offline); `/api/connections` lists only currently-active connections. No history for a down port or an ended connection. |
| Connection detail | Rich troubleshooting fields (seq numbers, window/queue, T1 retries, RNR, RTT, SREJ) — important for diagnosing stalled QSOs. |
| Frame payloads | Base64 in the event `data` field, for `UI`/`I` frames only (binary-safe: APRS text and Winlink B2 alike). |
| Default exposure | **Disabled by default**; `127.0.0.1:8002` when enabled. See Security. |

## Architecture

New package `internal/frontend/api`: a `net/http` server (stdlib only), mirroring
the AGWPE/kisstcp frontend pattern —
`api.Serve(eng, b, host string, port, maxClients int) (*Server, error)` returning
a `*Server` whose `Close()` (called on the engine loop from shutdown) stops it.

Three routes, all read-only:
- `GET /api/status`
- `GET /api/connections`
- `GET /api/events` (SSE)

### Subscriber-bus extension

The bus (in `internal/bridge`) gains two narrow sink interfaces alongside the
existing `RawRXSink` / `MonitorSink`:

```go
type TxFrameSink interface { OnTXFrame(port int, f *ax25.Frame) }
type ConnSink     interface { OnConn(e ConnEvent) }

type ConnEvent struct {
	Port           int
	Local, Remote  string
	State          string // "connected" | "disconnected"
	Incoming       bool   // meaningful for "connected"
}
```

- `OnTXFrame` is emitted from the single TX choke point `Bridge.SendToKISS`
  (parse the raw bytes → `*ax25.Frame`, exactly as RX does in `OnKISSFrame`), so
  it captures **all** transmit traffic uniformly: L2 connected-mode I/S/U frames,
  AGWPE UI, and kisstcp-passthrough frames. A parse failure skips emission (no
  error — our own TX is normally well-formed).
- `OnConn` is emitted from the existing `notifyConnected` / `notifyDisconnected`
  paths.

Registration/emission mirror the existing sinks (typed lists, register/unregister
on the loop, `emit*` on the loop). These complete the "add when the API needs
them" note from the kisstcp spec — added now, not speculatively before.

The API `Server` implements `MonitorSink` (RX) + `TxFrameSink` + `ConnSink` and
registers for all three; each SSE client is fanned an event via a per-client
buffered channel + writer (same pattern as kisstcp).

### Engine-loop discipline (the concurrency model)

All bridge/l2 state is confined to the single engine loop, so:

- **GET handlers** run on `net/http` goroutines; each marshals onto the loop via
  `eng.Do` to *snapshot* the needed state into a plain struct (done-channel
  handshake), then JSON-serializes and writes the response **off** the loop. No
  new locks or atomics.
- **SSE push**: sinks fire on the loop and enqueue to per-client channels; a slow
  client is dropped, never blocking the loop. The client set is loop-confined
  (mutated via `eng.Do`), like kisstcp. Client disconnect is detected via the
  request context (`r.Context().Done()`).
- **Counters** are plain per-port ints on the bridge, incremented on the loop in
  `OnKISSFrame` (rx) and `SendToKISS` (tx), reset in `PortOffline`. Read via the
  snapshot. No atomics.

The rejected alternative — thread-safe accessors/atomics on the bridge/l2 — would
spread concurrency primitives into the loop-confined core for endpoints hit
rarely. Snapshot-on-loop is consistent with the existing model.

## Endpoints & JSON schemas

### `GET /api/status`

```json
{
  "version": "1.99-Beta",
  "ports": [
    {"port": 0, "name": "Port 0", "type": "serial", "online": true, "rx_frames": 42, "tx_frames": 17}
  ]
}
```

`rx_frames`/`tx_frames` are the live-scoped counters (0 when the port is offline).

### `GET /api/connections`

Only currently-active (Connecting/Connected) connections:

```json
{
  "connections": [
    {
      "port": 0,
      "local": "KU0HN-10", "remote": "W0NE-10", "via": ["W0NE-7"],
      "state": "connected", "incoming": false, "modulo": 8,
      "send_seq": 5,
      "recv_seq": 3,
      "unacked": 2,
      "send_queue": 1,
      "t1_retries": 0,
      "remote_busy": false,
      "srtt_ms": 1200,
      "srej": true
    }
  ]
}
```

Field meanings: `send_seq` = V(S) (next N(S) to send); `recv_seq` = V(R) (next
N(S) expected); `unacked` = outstanding unacked I-frames (window pressure);
`send_queue` = frames queued but not yet transmitted; `t1_retries` = T1
poll/retransmit attempts (nonzero indicates trouble); `remote_busy` = remote sent
RNR; `srtt_ms` = smoothed round-trip time (Karn); `srej` = SREJ selective repeat
active; `modulo` = 8 or 128; `incoming` = connection was remote-initiated.
`state` is lowercased in JSON (`"connected"` / `"connecting"`) — consistent with
the lowercase event tokens — rather than the capitalized `ConnState.String()`
form used internally.

Exposing these requires an exported snapshot in the `l2` package —
`type ConnInfo struct { … }` plus `(*l2.Table) Snapshot() []ConnInfo` (called on
the loop) that copies the fields from each active `Conn`. `Conn` gains an
`incoming bool` field set at connect time (currently direction is only passed to
the hook, not stored).

### `GET /api/events` (SSE)

`Content-Type: text/event-stream`. Each event is `event: <type>\ndata: <json>\n\n`.
Four event types:

```
event: rx
data: {"port":0,"from":"W0NE-7","to":"APRS","type":"UI","pid":240,"len":18,"via":["WIDE1-1"],"data":"<base64>"}

event: tx
data: {"port":0,"from":"KU0HN-10","to":"W0NE-10","type":"I","pid":240,"len":128,"via":[],"data":"<base64>"}

event: connect
data: {"port":0,"local":"KU0HN-10","remote":"W0NE-10","incoming":false}

event: disconnect
data: {"port":0,"local":"KU0HN-10","remote":"W0NE-10"}
```

- `rx`/`tx` share one shape (decoded frame metadata). `type` is the AX.25 frame
  type name (`UI`/`I`/`RR`/`RNR`/`REJ`/`SABM`/`SABME`/`DISC`/`UA`/`DM`/`FRMR`/
  `XID`/`SREJ`). `via` is the digipeater path (may be empty).
- `data` is base64 of the info field, included only for `UI`/`I` frames; omitted
  for frames with no info field.
- No client→server protocol: the server ignores anything a client sends and just
  streams.

## Config & lifecycle

New `[api]` section, disabled by default:

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | Off unless explicitly enabled (see Security). |
| `listen_host` | `127.0.0.1` | Localhost only by default. |
| `listen_port` | `8002` | After 8000 (AGWPE) / 8001 (kisstcp). |
| `max_clients` | `16` | Cap on concurrent SSE `/api/events` streams. |

- `APIConfig` struct + `Config.API`; register `[api]` + its keys in the
  known-keys validation (unknown-key warnings + did-you-mean); commented `[api]`
  block in `example.go`. `[api]` absent ⇒ disabled, behavior identical to today.
- `cmd/tncd/main.go`: when `cfg.API.Enabled`, start `api.Serve(...)` after the
  kisstcp block, and register the API as `MonitorSink` + `TxFrameSink` +
  `ConnSink` (setup phase, before `engine.Run`). Add `apiSrv.Close()` to the
  `eng.Do` shutdown sequence, next to `kissSrv.Close()`.

## Security

The API is **unauthenticated attack surface**. Its only protections are that it
is **read-only**, **localhost-bound by default**, and **disabled by default** —
an operator must consciously enable it. There are no credentials, tokens, or TLS.

- Disabled by default is a deliberate security decision, not just a convenience:
  a tncd install exposes no HTTP surface unless the operator opts in.
- Read-only is the containment boundary: even fully exposed, the API cannot key
  the transmitter, change config, or alter connections — it only reveals state
  and traffic metadata (and frame payloads, which are already in the clear on the
  RF channel).
- An operator who binds it to a non-localhost address is exposing packet traffic
  metadata + payloads to that network; that is their explicit choice.

**Explicit non-goals:** authentication, authorization, TLS, rate limiting, and
any control/write actions. If control actions are added post-2.0, they must come
with an auth story — this spec does not open that door.

## Testing

- **Unit (Go, `net/http/httptest`, no hardware):**
  - `GET /api/status` — snapshotted bridge (fake ports online/offline) → assert
    version + per-port `online` + `rx_frames`/`tx_frames` (0 for offline).
  - `GET /api/connections` — active `l2` connections → assert every
    troubleshooting field serializes with correct values.
  - `GET /api/events` (SSE) — HTTP client against an `httptest` server; drive bus
    events (`OnRXFrame`/`OnTXFrame`/`OnConn`) → assert correctly-framed
    `event: rx|tx|connect|disconnect` + `data`; base64 payload on `UI`/`I`,
    omitted on `S`-frames; via path present.
  - Bus extension — `TxFrameSink`/`ConnSink` register/unregister + fan-out
    (bridge-level fake sinks), mirroring the `RawRXSink` registry test.
  - TX emission — `SendToKISS` parses raw → emits `OnTXFrame`; parse failure
    skips emission without error.
  - Counters — `OnKISSFrame` bumps rx, `SendToKISS` bumps tx, `PortOffline`
    resets; snapshot reflects.
  - `l2.Table.Snapshot()` — returns only active conns with correct field values
    (tested in the `l2` package against a hand-built conn).
  - Config `[api]` — parse + defaults + absent-disabled + unknown-key warning.
  - SSE lifecycle — slow client dropped (non-blocking enqueue); client disconnect
    (context canceled) unregisters cleanly.
- **e2e (existing harness):** with `[api] enabled = true` during a Direwolf/PAT
  session in the e2e suite, `curl` the endpoints mid-session and assert
  `/api/status` shows the port online with nonzero counters, `/api/connections`
  reflects the live CMS/P2P connection with sane seq/window fields, and
  `/api/events` streams the connect + I/RR frames. Runs alongside the existing
  e2e tests (no separate hardware needed — reuses the Direwolf virtual path).
- **Regression:** AGWPE monitor output stays byte-identical (the `MonitorSink`
  path is unchanged); full `go test ./...` + `go vet ./...` green under
  `CGO_ENABLED=0`.
- **Gate:** Go unit + e2e + vet. **No OTA** — read-only host-side observability,
  no on-air protocol change (same reasoning as kisstcp/serial-robustness). Manual
  smoke: `curl /api/status` and `curl -N /api/events` while traffic flows.

## Non-goals

- WebSocket transport (SSE chosen; WS addable later if a consumer needs it).
- Any write/control actions (key TX, connect/disconnect, config changes).
- Authentication / authorization / TLS / rate limiting (see Security).
- Historical/persistent counters or connection history (live-scoped only).
- New third-party dependencies (stdlib `net/http` + `encoding/json` only).

## Exit criteria

1. `[api]` absent or `enabled = false` ⇒ no HTTP surface; behavior identical to
   today.
2. When enabled: `GET /api/status` and `GET /api/connections` return the schemas
   above from a live snapshot; `GET /api/events` streams `rx`/`tx`/`connect`/
   `disconnect` SSE events.
3. TX frames appear on the bus (`OnTXFrame` from `SendToKISS`) and in the event
   stream; AGWPE monitor output is unchanged.
4. e2e: the endpoints reflect a live Direwolf/PAT session.
5. All new + existing `go test ./...` and `go vet ./...` pass under
   `CGO_ENABLED=0`.
