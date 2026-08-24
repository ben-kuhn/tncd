# tncd Security Review (2026-08-24, v2.0 line)

Scope: full read-through of the Go codebase (`ax25/`, `agwpe/`, `kiss/`,
`internal/`, `cmd/tncd/`), plus packaging/service files. All unit tests pass
at the reviewed commit.

## Threat model (ham-radio-specific)

Per project context: **encryption over the air is prohibited, so on-air privacy
is out of scope and not a finding.** What remains:

1. **Network attackers** reaching the TCP listeners (AGWPE 8000, KISS-TCP 8001,
   API 8002). Defaults bind `127.0.0.1`; a user can rebind to a LAN/WAN
   interface.
2. **On-air attackers** injecting arbitrary AX.25 frames via the TNC. Every
   byte decoded from KISS RX is untrusted input. An on-air attacker can also
   attempt to exhaust tncd resources (memory, timers, the engine loop) to
   knock the station off the air.
3. **A hostile or compromised KISS TCP server** (when a port is
   `type = tcp`); its output is decoded by the same KISS/AX.25 parsers.
4. **Local attackers** on the Windows host (installer/service privilege
   boundaries).

The uniquely "ham" impact to keep in mind: a network attacker who reaches the
AGWPE or KISS-TCP port can **key the transmitter with an arbitrary source
callsign** (the 'C'/'M'/'K' handlers take `CallFrom` from the wire
unverified). That's a regulatory/licensing problem for the station owner, not
just a data problem.

---

## Findings, prioritized

### HIGH 1 — Unbounded memory growth in the KISS decoder (network-reachable DoS)

`kiss/framing.go` `Decoder.Feed` appends every in-frame byte to `d.buf` with
**no size cap** (lines 109–132). The buffer is only released when a closing
FEND arrives.

Reachable from two untrusted sources:

- `internal/frontend/kisstcp/client.go:64-76` — bytes from **KISS-over-TCP
  clients**. Any client that connects can send `FEND` followed by an endless
  stream of non-FEND bytes; memory grows without bound for the life of the
  connection. `max_clients = 16` bounds the *connection count*, not
  per-connection memory.
- `kiss/port.go:83-106` (`readerLoop`) — bytes from the TNC transport. For
  `type = tcp` ports this is a network peer; a hostile/compromised KISS server
  (or a MITM between tncd and a remote TNC) gets the same primitive.

Verified with a PoC: streaming 4 MiB of unterminated frame data grew process
heap by ~10 MiB; it scales linearly with no ceiling until OOM.

**Proposed fix:**
- Add a hard cap in `Decoder` (suggest 8 KiB — the largest legitimate AX.25
  frame is well under 2 KiB even with mod-128 and max digipeaters). On
  overflow: drop the partial frame, reset `inFrame`, and count a
  `droppedOversize` stat. Log once per connection, not per byte.
- Regression test: feed `FEND` + >cap bytes, assert the decoder discards and
  resyncs on the next FEND.
- Consider the same cap concept for the AGWPE reassembly buffer
  (`internal/frontend/agwpe/server.go:101-151`), which is already bounded by
  `MaxPayload = 65536` — that's fine as-is, just note the asymmetry.

### HIGH 2 — Unauthenticated control listeners can transmit with a forged callsign

AGWPE login ('P') is accepted unconditionally (`handler.go:43-45`) — required
for AGWPE/pyham-pe compatibility, so not a bug. But it means **anyone who can
reach the AGWPE or KISS-TCP listener can transmit arbitrary frames under any
source callsign** from the operator's station. Defaults are loopback-only
(`config.go:351,379,389`), which is the saving grace, but:

- Nothing warns when `listen_host` is set to a non-loopback address.
- There is no client IP allowlist.
- The KISS-TCP frontend can also push raw KISS *command* frames (TXDELAY,
  SetHardware, …) to the TNC (`kisstcp/client.go:100-101`) — it correctly
  drops exit-KISS (line 102), but everything else passes.

**Proposed fix (defense-in-depth, keeping protocol compatibility):**
- At startup, log a prominent WARNING for every listener bound to a
  non-loopback address: "UNAUTHENTICATED listener on 0.0.0.0:8000 — anyone who
  can reach this port can transmit under your callsign."
- Add optional `allowed_subnets` (CIDR list) per listener, enforced at accept
  time. Default: allow all (preserves current behavior), but document it in
  `example.ini` next to each `listen_host`.
- Document the firewall/SSH-tunnel guidance in the README for remote-access
  setups.

### MED 3 — API reconnect endpoint is unauthenticated and CSRF-able

The API is documented as "read-only" but exposes
`POST /api/ports/{n}/reconnect` (`internal/frontend/api/server.go:112-138`),
which tears down and re-dials a port's transport. Two issues:

- **No CSRF protection.** A `fetch("http://127.0.0.1:8002/api/ports/0/reconnect",
  {method:"POST"})` from any web page the operator visits is a "simple request"
  (no preflight) — the side effect executes even though the response can't be
  read cross-origin. Drive-by disruption of the radio link from a browser.
- If the API is bound to a LAN interface, anyone on the LAN can cycle ports at
  will (rate-limited only by reconnect backoff).

**Proposed fix:**
- Reject cross-origin state-changing requests: require a custom header (e.g.
  `X-Requested-With: fetch`) — simple-request rules then force a preflight
  that fails without CORS headers. Have the UI's fetch set it.
- Optionally also validate `Origin`/`Host` when present.
- Consider gating the endpoint behind `[api] allow_reconnect = false` default.

### MED 4 — No timeouts or idle reaping on network connections

- `internal/frontend/api/server.go:59` — `http.Server{Handler: mux}` with no
  `ReadHeaderTimeout`/`ReadTimeout`/`IdleTimeout` → classic Slowloris on the
  API port (each slow client holds a goroutine + connection).
- AGWPE and KISS-TCP sockets have no read deadlines. AGWPE clients are covered
  by the 30 s idle sweep (`bridge.go:746-759`, `idle_timeout` default 300 s),
  but **KISS-TCP clients are not swept at all** — a client that connects and
  goes silent holds a slot forever (16 by default), starving legitimate
  clients.
- SSE has a 15 s heartbeat (good), which at least detects half-open streams
  on write.

**Proposed fix:**
- Set `ReadHeaderTimeout: 10s`, `IdleTimeout: 60s` on the API server (leave
  `WriteTimeout` 0 — SSE is long-lived).
- Extend the bridge idle sweep (or a per-client read deadline refreshed on
  each `Feed`) to KISS-TCP clients; add `[kisstcp] idle_timeout` mirroring
  `[server]`.

### MED 5 — Unbounded internal queues/registries

Each is individually survivable but all are reachable from unauthenticated
inputs:

- `engine.Do` (`internal/engine/engine.go:73-82`) — the central event queue
  grows without bound. A flooding AGWPE/KISS-TCP client (localhost is fast)
  enqueues a closure per frame; if the loop drains slower than the flood,
  memory grows unboundedly and AX.25 timing degrades (a "soft" off-the-air
  DoS even before OOM).
- AGWPE 'X' registrations (`handler.go:62-80`) — `registeredCalls` has no
  per-client cap; a client can register unlimited callsigns (map growth, and
  every registered call makes tncd answer SABMs addressed to it via
  `isLocalCall`).
- 'V'/'v' digipeater lists (`handler.go:106-115, 167-178`) — `data[0]` allows
  up to 255 via entries; AX.25 permits 8. Not memory-unsafe (all bounds are
  checked), but produces malformed on-air frames and oversized buffers.

**Proposed fix:**
- Cap the engine queue (e.g. 10 000) and disconnect/penalize the offending
  client on overflow; or rate-limit per client.
- Cap registered callsigns per AGWPE client (e.g. 8) — mirrors what real
  AGWPE clients do.
- Reject via lists > 8 at the AGWPE handler.

### LOW 6 — Windows GUI installer temp-file race

`gui_windows.go:145-150` writes the generated config to the predictable path
`%TEMP%\tncd-setup.ini`, then elevates `install -c "<that path>"`. A local
non-admin process can pre-create or swap the file between the write and the
elevated read, causing the SYSTEM service to be installed with an
attacker-chosen config (listen addresses, devices, init strings — not direct
code execution, but it turns the SYSTEM service into the attacker's
network/TNC proxy). The elevated `install()` does validate the config
(`install_windows.go:58-64`), which limits the blast radius to valid configs.

**Proposed fix:** write with `os.CreateTemp` (unpredictable name) and delete
after install; or better, have the unelevated GUI pass the config through a
channel that doesn't touch a shared filesystem location (e.g. write it
directly to `%ProgramData%\tncd` from the elevated child after receiving the
content via stdin).

### LOW 7 — Windows service runs as LocalSystem

`service_windows.go:101-106` creates the service with no `ServiceStartName` →
LocalSystem. tncd needs only serial/Bluetooth/network access; LocalSystem is
far more. Also verify `%ProgramData%\tncd\tncd.ini` ends up admin-write-only
(default ACL inheritance usually yields that, but the installer doesn't set it
explicitly — a world-writable config = non-admin control of a SYSTEM service).

**Proposed fix:** document the account choice; optionally support installing
under a lower-privilege account with device access; explicitly ACL the config
file/dir at install time.

### LOW 8 — Log injection via crafted on-air addresses

`decodeAddress` (`ax25/address.go:93-106`) accepts any 7-bit character,
including CR/LF, into the callsign. These flow to logs through `%s` /
`String()` (e.g. `bridge.go:127` `logAX25`, several l2 paths), letting an
on-air attacker forge log lines. Most wire-sourced strings elsewhere use `%q`
(good); the fix is cheap.

**Proposed fix:** sanitize non-printable characters in `Address.String()` or
switch frame-logging call sites to `%q`.

### LOW 9 — Process: no fuzzing, dependency hygiene

The wire parsers I read (`ax25.ParseModulo`, `ParseXID`, `agwpe.ParseHeader`,
`kiss.Decoder`, `parseRFCOMMChannel`) are carefully bounds-checked — but the
only automated assurance is golden-byte unit tests. `lxn/walk` + `lxn/win` are
pinned to 2021 commits (Windows-only GUI; limited exposure). I could not run
`govulncheck` offline.

**Proposed fix:**
- Add Go fuzz targets for `ax25.ParseModulo`, `agwpe.ParseHeader`,
  `kiss.Decoder.Feed`, `ax25.ParseXID`, `parseRFCOMMChannel`; run briefly in
  CI.
- Add `govulncheck` to CI.

---

## Things done right (no action)

- All three listeners default to `127.0.0.1`; KISS-TCP and API are
  disabled by default; `example.ini` already flags the API as UNAUTHENTICATED.
- AGWPE payload cap (`MaxPayload` 64 KiB) enforced before allocation.
- Client caps on all frontends; connection table capped at 64
  (`l2.go:12`); outbound I-frame queue capped at 512 (`l2.go:527`);
  per-client write channels are bounded with close-on-overflow.
- Web monitor escapes all on-air-controlled strings (`esc()` in
  `monitor.js`); payload preview is character-filtered — no RF→browser XSS.
- kisstcp drops exit-KISS from clients, protecting the shared TNC.
- AX.25/XID/SDP parsers are consistently bounds-checked; no panics found on
  malformed input paths.
- systemd unit runs as `nobody`; Windows installer validates configs before
  installing.

## Suggested order of work

1. KISS decoder cap (+ regression test) — small, fixes the only proven
   remote-memory bug.
2. Non-loopback startup warning (+ optional `allowed_subnets`).
3. API CSRF header check + HTTP server timeouts.
4. KISS-TCP idle reaping.
5. Engine-queue / registration / via-list caps.
6. Windows temp-file + service-account hardening.
7. Fuzz targets + govulncheck in CI.
