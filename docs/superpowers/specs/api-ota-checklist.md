# Read-only API — OTA Validation Checklist (Direwolf as local TNC)

**Purpose**: Validate the 2.0 read-only monitoring API (`/api/status`,
`/api/connections`, `/api/events`) against a **live over-the-air AX.25 session**,
using **Direwolf as the local TNC/modem** (in place of a hardware TNC).

The API is host-side observability with no on-air protocol change, so it has no
formal OTA gate — but a live-session validation is worth doing before release:
it proves the bus TX/RX/connection events, the live-scoped counters, and the
`/api/connections` troubleshooting snapshot all reflect reality end to end.

### Relationship to the unit/e2e tests

The Go unit suite (`internal/frontend/api`, `internal/bridge`, `ax25/l2`) proves
encoding, snapshotting, SSE fan-out, and the bus events in isolation.
`e2e/test_e2e.py::TestConnectedModeKISSTCP::test_p2p_api_reflects_live_session`
proves the API reflects a live *virtual* Direwolf/PAT session (no RF). This
checklist is the real-radio half: a genuine QSO, watched through the API.

---

## Setup

- **Local TNC**: Direwolf, exposing a KISS **PTY** at `/tmp/kisstnc`
  (`direwolf -p ...` creates the symlink), keyed to a real radio on a real
  frequency. (A Direwolf KISS-over-TCP listener also works — then set tncd
  `[client.0]` to `type = tcp`, `host = 127.0.0.1`, `port = 8001`.)
- **tncd**: build and run with the provided config:
  ```bash
  CGO_ENABLED=0 go build -o tncd ./cmd/tncd
  ./tncd -c tncd-api-ota.ini
  ```
  Expected startup log line: `read-only API started listen=127.0.0.1:8002`.
- **AGWPE client**: PAT, pointed at `127.0.0.1:8005`, `radio_port = 0`
  (see the PAT notes in project memory). A peer to connect to: a Winlink RMS
  gateway reachable via your path, or a P2P station.
- **Two spare terminals** for `curl`.

> Confirm **disabled-by-default** first: start tncd once with the `[api]` section
> removed (or `enabled = false`) and verify nothing is listening on 8002
> (`curl -sS http://127.0.0.1:8002/api/status` → connection refused). Then
> restore `[api] enabled = true`.

---

## Procedure

### 1. Baseline (no connection yet)

```bash
curl -s http://127.0.0.1:8002/api/status | jq
curl -s http://127.0.0.1:8002/api/connections | jq
```
- `status`: `ports[0].online == true`; `type == "serial"`; `rx_frames` may be
  0 or rising (if you're hearing beacons/other traffic); `tx_frames == 0`.
- `connections`: `{"connections": []}` — nothing active.

### 2. Start the live event stream (leave running)

```bash
curl -N http://127.0.0.1:8002/api/events
```
If the channel is busy you should immediately see `event: rx` lines for heard
frames, each with a `data:` JSON body. On a quiet channel this stays silent
until step 3.

### 3. Connect (the live session)

Initiate a connection from PAT (Winlink CMS or P2P). Watch the SSE stream and,
**while the connection is up**, snapshot connections from another terminal:

```bash
curl -s http://127.0.0.1:8002/api/connections | jq
```

Expected during the session:
- SSE stream shows a `connect` event, then `tx`/`rx` events for the SABM/UA
  handshake and the I/RR frame exchange.
- `connections` shows one entry: `state == "connected"`, correct `local`/
  `remote`/`via`, and troubleshooting fields that make sense and change as
  traffic flows — `send_seq`/`recv_seq` advancing, `unacked`/`send_queue`
  reflecting backlog, `modulo` 8 or 128, `srtt_ms` populated (nonzero) once
  round-trips are measured, `t1_retries` normally 0 (rises on a bad path),
  `remote_busy` false unless the peer sent RNR, `srej` per negotiation.
- `status` counters (`rx_frames`, `tx_frames`) climb.

### 4. Verify a decoded payload

Pick a `tx` or `rx` event for an `I` or `UI` frame from the SSE stream and
confirm it carries a base64 `data` field:
```bash
# from an event's data line:
echo '<base64 from "data">' | base64 -d | xxd | head
```
`S`-frames (`RR`/`RNR`/`REJ`/`SREJ`) must have **no** `data`/`pid` fields.

### 5. Disconnect (teardown)

Disconnect from PAT (or let the session complete). Expected:
- SSE stream shows a `disconnect` event for that connection.
- `curl .../api/connections` → `{"connections": []}` again (live-scoped —
  ended connections are not retained).

### 6. (Optional) Port-offline counter reset

Stop Direwolf (or unplug the radio path) so the tncd port goes offline, then:
```bash
curl -s http://127.0.0.1:8002/api/status | jq '.ports[0]'
```
- `online == false`; `rx_frames`/`tx_frames` reset to `0` (live-scoped — no
  history for a down port).

---

## Pass criteria

1. **Disabled by default**: no listener on 8002 until `[api] enabled = true`.
2. **All three endpoints respond** once enabled; JSON is well-formed.
3. **`/api/events`** streams `rx`/`tx` during traffic and a `connect` then
   `disconnect` around the session; base64 `data` on `I`/`UI`, absent on
   `S`-frames.
4. **`/api/connections`** shows the live connection with sane, changing
   troubleshooting fields while up, and is empty when down.
5. **`/api/status`** counters increase during traffic and reset when the port
   goes offline.
6. **No interference**: the PAT/AGWPE session behaves exactly as without the API
   enabled (the API is strictly read-only — it never keys TX or alters state).

Record the result (PASS/notes) in the ledger and, on PASS, proceed to cut the
release (version bump + tag + packages + website changelog).
