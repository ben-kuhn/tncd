# KISS-over-TCP Passthrough + Frontend Subscriber Bus Design

**Status:** Approved design.
**Date:** 2026-07-23.
**Context:** Phase 2 of the tncd 2.0 Go port ("new frontends"). Phase 2 was
deferred to land AX.25 v2.2 (phase 3) and SREJ (phase 3.5) first; it is now
being built before the v2.0.0 release gate (phase 4). Phase 2 comprises two
independent frontends and is **decomposed into two sub-projects**, each with
its own spec → plan → build cycle:

1. **KISS-over-TCP passthrough** (this spec) — also introduces a generalized
   frontend subscriber bus in the bridge.
2. **Read-only JSON/WebSocket API** (next sub-project) — consumes the same bus.

This spec covers sub-project 1.

## Purpose

Let KISS-native applications (woad, mobile clients, `kissutil`, any Direwolf-KISS
app) share one physical TNC alongside AGWPE clients (PAT/Winlink, Paracon,
Xastir). tncd exposes a Direwolf-8001-style KISS-over-TCP listener; connected
clients hear every frame received from the air and can transmit, with their TX
frames sharing the same per-port queue and channel-timing discipline as the
AX.25 L2 engine's frames.

Building this cleanly also means generalizing how the bridge distributes
received traffic to frontends — today the AGWPE monitor formatting lives in the
bridge core. This spec introduces a **frontend subscriber bus** and migrates the
AGWPE monitor onto it, so KISS-TCP (and later the API) are first-class
observers rather than special cases.

## Decisions (settled during brainstorming)

| Question | Decision |
|---|---|
| Phase 2 vs. release | Build phase 2 **before** v2.0.0. |
| Phase 2 shape | Two sub-projects; **KISS-TCP first**, read-only API second. |
| Plug-in model | **Generalized subscriber bus** (not a one-off sink); migrate the AGWPE monitor onto it **now**. |
| Echo/loopback | **No loopback** (Direwolf-faithful). A frontend's TX is never re-delivered as RX to any client. Falls out of the existing TX echo-suppression. |
| Client KISS command frames | **Forward** timing params (TXDELAY/persistence/slottime/txtail/fullduplex, low nibble 1–5) and **SetHardware** (6) to the physical TNC; they take effect (last-writer-wins across clients on that port). |
| Client exit-KISS (0x0F) | **Never forwarded** — dropped/logged. Protects the shared TNC from one client kicking the whole station out of KISS. |
| Out-of-range port nibble / unknown cmd | Dropped + debug log. |
| Default exposure | Disabled by default; `127.0.0.1:8001` when enabled. |

## Architecture

### Frontend subscriber bus

The bridge stops knowing frontends' output formats. It emits typed events on the
engine loop; frontends register narrow **sink** interfaces for what they consume.
Narrow interfaces (rather than one fat `OnEvent`) mean adding an event type later
(for the API) never breaks existing sinks.

```go
// internal/bridge/events.go — all methods invoked ONLY on the engine loop.
type RawRXSink interface {
    OnRawRX(port int, raw []byte) // raw AX.25 heard from air, post echo-suppression
}
type MonitorSink interface {
    OnRXFrame(port int, f *ax25.Frame) // decoded received frame, for monitoring
}
```

The bridge keeps typed registration lists (`rawSinks []RawRXSink`,
`monitorSinks []MonitorSink`) with `RegisterRawRXSink` / `UnregisterRawRXSink`
(and the Monitor equivalents), callable only on the engine loop. `OnKISSFrame`,
after its existing echo-suppression and mod-8/mod-128 parse, emits:

- `raw, port` → every `RawRXSink` (KISS-TCP)
- `frame, port` → every `MonitorSink` (AGWPE monitor; later, the API)

Only these two event types are introduced now. Connection-state, TX, and
status events are **deferred to the API sub-project** (YAGNI) and added to the
bus when that spec needs them.

### AGWPE monitor migration

`distributeMonitor` (currently `internal/bridge/monitor.go`, formatting AGWPE
`U`/`I`/`S` monitor lines) moves into the AGWPE frontend
(`internal/frontend/agwpe/monitor.go`) as a `MonitorSink`. The bridge core no
longer contains AGWPE wire-formatting. **The emitted bytes stay byte-identical**
— the format is validated against `pyham-pe`/PAT and OTA-proven — so this is a
plumbing move locked by the existing monitor tests, not a behavior change.

The migrated `MonitorSink` iterates the AGWPE client set to format and deliver
to the monitoring clients (`Client.Monitoring()` / `Client.SendAGWPE(...)`, as
`distributeMonitor` does today). That client set is exposed to the sink via a
bridge accessor (e.g. `Bridge.Clients()`), keeping `b.clients` the single source
of truth rather than duplicating the list in the AGWPE package.

AGWPE **connection ownership** is untouched: `RegisteredCalls`, owner assignment,
connected-mode `D` delivery, and `C`/`d` notifications remain in the existing
AGWPE `Client` path. Those are connection concerns, not passive-observation
events, and do not belong on the bus. The bridge keeps `b.clients` for those
lookups; the monitor simply reads it through the accessor instead of the core
calling `distributeMonitor` directly.

### Package layout

```
internal/bridge/events.go            sink interfaces + registry            (new)
internal/bridge/monitor.go           REMOVED (formatting moves to agwpe)
internal/frontend/agwpe/monitor.go   AGWPE MonitorSink                     (moved)
internal/frontend/kisstcp/server.go  listener + accept loop                (new)
internal/frontend/kisstcp/client.go  per-conn KISS framing, RawRXSink, TX  (new)
```

### Concurrency

KISS-TCP mirrors the AGWPE frontend exactly:

- `Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int) (net.Listener, error)` — creates the listener, runs the accept loop in a goroutine, returns the listener for the caller to `Close` on shutdown.
- One goroutine per connection reads and reassembles KISS frames; a second per-connection writer goroutine drains a buffered outbound channel to the socket, so a slow client can never stall the engine loop.
- Every bridge call (`RegisterRawRXSink`, `SendToKISS`, the new command-send) is marshalled onto the engine loop via `eng.Do(...)`. Sinks are invoked on the loop and only enqueue to per-client channels.

## RX fan-out to KISS-TCP clients

The frontend registers one `RawRXSink` (shared across its clients, or per-server
holding the client list — implementation choice for the plan). On
`OnRawRX(port, raw)`:

- Wrap `raw` as a KISS **data** frame: command byte `(port<<4) | 0x00`,
  KISS-escape the payload (`FEND→FESC TFEND`, `FESC→FESC TFESC`), bracket with
  `FEND`. (`kiss/framing.go` already has the escaping primitives.)
- Enqueue the wrapped bytes to **every** connected client's outbound channel.
- **Promiscuous**, per standard KISS: every heard frame goes to every client,
  including frames belonging to AGWPE connected-mode sessions on the same
  channel — it is all shared-channel traffic, already TX-echo-suppressed.
- The port nibble reflects the frame's real port so multiport clients can
  distinguish ports.

## TX + command path from KISS-TCP clients

The per-connection reader reassembles a KISS frame (FEND-delimited, FESC
unescaped), decodes the command byte (high nibble = port, low nibble = type),
and marshals the action onto the engine loop:

| Low nibble | Handling |
|---|---|
| `0` data | Extract AX.25 payload → `bridge.SendToKISS(port, payload)`. Reuses the per-port TX queue, echo-suppression tracking, and channel timing. No loopback. |
| `1–5` TXDELAY/P/SlotTime/TXtail/FullDuplex | Forward to the physical TNC (takes effect, last-writer-wins on that port). |
| `6` SetHardware | Forward to the physical TNC likewise. |
| `0x0F` exit-KISS | **Dropped**, never forwarded (debug log). |
| out-of-range port / unknown nibble | Dropped + debug log. |

**New `kiss.Port` capability.** A command-send path — sketch
`SendCommand(cmdType byte, value []byte)` — KISS-wraps a command frame with
`WrapCommand` (already in `kiss/framing.go`) using `(port<<4)|cmdType` and
enqueues it on the **same per-port write path** as data frames, preserving
command/data ordering to the TNC. tncd still sends its own configured params at
startup (`sendParams`); a client command overrides on the TNC afterward
(last-writer-wins), and a reconnect/re-init reasserts tncd's own — documented
behavior, not a conflict.

Data frames bypass the L2 engine entirely (KISS clients run their own L2, like
woad/airmail). The only coupling with the AGWPE/L2 side is the shared per-port
TX queue; no cross-frontend arbitration is needed.

## Config & lifecycle

New `[kisstcp]` section, disabled by default:

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | Off unless explicitly enabled. |
| `listen_host` | `127.0.0.1` | Localhost — KISS-over-TCP is unauthenticated, same posture as AGWPE's default. |
| `listen_port` | `8001` | Direwolf KISS-over-TCP convention; avoids AGWPE's 8000. |
| `max_clients` | `16` | Simple resource cap. |

- Add `KISSTCPConfig` to `internal/config`; register `[kisstcp]` and its keys in
  the known-sections/keys validation so unknown-key warnings and did-you-mean
  suggestions still apply. A working 1.x/2.0 config without `[kisstcp]` behaves
  exactly as today (section absent ⇒ disabled).
- Add a commented `[kisstcp]` block to the `genconfig` / `example.go` output.
- `cmd/tncd` starts the listener when `enabled`, mirroring the AGWPE `Serve(...)`
  startup, and registers the `RawRXSink` via `eng.Do`. Graceful shutdown closes
  the listener and client connections, slotting into the existing signal-driven
  shutdown sequence (close clients before the accept loop, as AGWPE does).

## Testing

- **Unit (Go, no hardware):**
  - KISS reassembly: partial frame across reads, multiple frames per read, FESC
    unescaping, oversized-frame handling.
  - Command-byte decode: port-nibble extraction, data vs. command classification,
    exit-KISS drop, out-of-range-port drop.
  - `RawRXSink` fan-out: correct wrapping (cmd byte, escaping, FEND, port
    nibble), delivery to multiple clients.
  - TX path against a mock bridge: data → `SendToKISS(port, payload)`; timing /
    SetHardware → `SendCommand`; exit-KISS → neither.
  - Subscriber registry: register/unregister, fan-out to N sinks, no delivery
    after unregister.
  - Concurrency: a slow/blocked client does not stall the engine loop (buffered
    outbound channel + writer goroutine); disconnect cleanup unregisters cleanly.
- **AGWPE monitor migration regression:** existing monitor tests stay green with
  **byte-identical** output after the move to `MonitorSink`.
- **e2e (local, nice-to-have):** a KISS-over-TCP client (e.g. a small Go test
  client or `kissutil`) sharing a Direwolf PTY with an AGWPE client — verifies
  coexistence, RX fan-out, and TX on one TNC.
- **Gate:** full `go test ./...` + `go vet ./...` green. **No OTA required** —
  host-side frontend, no new on-air protocol (same reasoning as the
  serial-robustness release). The umbrella's phase-4 hardware revalidation
  covers real-radio coverage before v2.0.0.

## Non-goals

- The read-only JSON/WebSocket API — the second phase-2 sub-project, its own spec.
- Connection-state / TX / status events on the bus — added when the API needs
  them, not now.
- Per-client KISS parameter isolation — there is one physical TNC; client
  params are last-writer-wins by design.
- Authentication / TLS on the KISS-TCP listener — matches AGWPE (unauthenticated,
  localhost-default); out of scope.
- TX monitoring of KISS-TCP frames into the AGWPE `m` stream — the existing
  monitor is RX-only; unchanged here.

## Exit criteria

1. A KISS-over-TCP client can connect to `[kisstcp]` and receive every heard
   frame (correct KISS framing + port nibble) while an AGWPE client is connected
   to the same TNC.
2. A KISS-TCP client can transmit data frames; they go out via the shared per-port
   queue with no loopback to any client.
3. Client timing/SetHardware commands reach the TNC; exit-KISS is dropped.
4. AGWPE monitor output is byte-identical to pre-refactor (existing tests green).
5. `[kisstcp]` absent ⇒ behavior identical to today; all new + existing
   `go test ./...` and `go vet ./...` pass.
