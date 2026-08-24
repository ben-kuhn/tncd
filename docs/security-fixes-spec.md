# Security Hardening Spec — tncd v2.0 line

Companion to `docs/security-review.md` (2026-08-24). This spec defines the
concrete behavior changes, new configuration surface, and tests for each
finding. Compatibility constraints:

- **AGWPE wire behavior is frozen** by golden-byte tests and pyham-pe
  compatibility. No fix may change the bytes of any valid AGWPE exchange.
  Rejections use existing AGWPE idioms (e.g. 'X' failure payload `0x00`).
- **KISS-TCP** must stay Dire Wolf-8001-compatible for well-behaved clients.
- **AX.25 on-air behavior** is unchanged for spec-compliant peers; caps only
  reject inputs that were already protocol violations.
- All new config keys must be optional, documented in `example.ini`, and
  default to current behavior unless stated otherwise.

---

## F1 — KISS decoder frame-size cap

**Finding:** `kiss.Decoder` buffers an unterminated frame without bound
(reachable from kisstcp clients and from a `type = tcp` KISS peer).

**Design:**

- New exported constant `kiss.MaxFrameSize = 8192` (bytes). The largest
  legitimate AX.25 frame — 8 digipeaters, mod-128 control, 256-byte info — is
  ~350 bytes; 8 KiB is generous headroom for KISS extensions.
- When the in-frame buffer reaches the cap: discard the partial frame, exit
  the in-frame state (the next FEND resyncs), and increment an exported
  counter `Decoder.DroppedOversize`. Exactly one increment per oversize frame.
- Callers (`kiss.Port.readerLoop`, `kisstcp` client read loop) compare the
  counter against a last-seen value after each `Feed` and log one line per
  drop event, naming the peer. The connection stays open; valid frames after
  the oversize one decode normally.

**Tests** (`kiss/framing_test.go`): frame at exactly the cap decodes; frame
over the cap is dropped, counter increments once, and a subsequent valid frame
resyncs and decodes; escape sequence straddling the cap boundary.

## F2 — Non-loopback listener warning + optional subnet allowlist

**Finding:** unauthenticated listeners (AGWPE protocol has no auth, by design)
can be rebound to non-loopback interfaces with no warning; anyone reaching the
port can transmit under an arbitrary callsign.

**Design:**

- At startup (`internal/app/app.go`), for each enabled listener whose
  `listen_host` is not loopback (`127.0.0.0/8`, `::1`, `localhost`; empty
  string means "all interfaces" and warns), emit one `slog.Warn` per listener:
  `"UNAUTHENTICATED <name> listener on <host:port> — anyone who can reach it
  can transmit under your callsign; restrict with allowed_subnets or a
  firewall"`.
- New optional key per listener section (`[server]`, `[kisstcp]`, `[api]`):
  `allowed_subnets = 192.168.1.0/24, 10.0.0.0/8`. Comma-separated CIDRs;
  bare IPs are treated as /32 (/128 for v6). Empty (default) = allow all,
  preserving current behavior. An unparseable entry fails config load.
- Enforcement: a filtering `net.Listener` wrapper (new internal package
  `internal/netutil`): `Allowlist.Allows(net.Addr) bool`; the wrapper's
  `Accept` closes and logs rejected connections. Applied uniformly to all
  three servers at `Serve` time, so HTTP and TCP paths share one
  implementation.

**Tests:** config parsing (valid list, bare IP, invalid CIDR fails load);
`Allowlist.Allows` table test (in/out of range, v4/v6, empty list allows all);
listener wrapper accepts allowed and closes rejected (net.Pipe or loopback
listener).

## F3 — API CSRF guard + HTTP server timeouts

**Finding:** `POST /api/ports/{n}/reconnect` is a cross-origin "simple
request" target (drive-by relink from any web page), and the `http.Server`
has no timeouts (Slowloris).

**Design:**

- The reconnect handler requires header `X-Requested-With: tncd`; anything
  else gets 403. Because the header is not CORS-safelisted, a cross-origin
  page must preflight, and tncd sends no CORS headers, so the browser blocks
  the request. The embedded UI's `fetch` sets the header (`monitor.js`).
- If an `Origin` header is present on the POST and its host does not match the
  request `Host`, respond 403 (defense in depth for non-browser clients).
- Timeouts on the API `http.Server`: `ReadHeaderTimeout: 10s`,
  `IdleTimeout: 120s`. `ReadTimeout`/`WriteTimeout` stay 0 — SSE streams are
  long-lived (the 15 s heartbeat keeps them from ever being idle).

**Tests** (`internal/frontend/api/server_test.go`): POST without the header →
403; with header → 200/409 as before; cross-origin Origin → 403; same-host
Origin → allowed. UI change verified by cross-compile (no JS test harness).

## F4 — KISS-TCP idle reaping

**Finding:** kisstcp clients are outside the AGWPE idle sweep; a silent client
holds one of `max_clients` slots forever.

**Design:**

- New `[kisstcp] idle_timeout` key, seconds, default 300, ≤ 0 disables —
  mirroring `[server] idle_timeout` semantics.
- kisstcp client tracks `lastActivity` (mutex-guarded, updated on every
  successful `Read`), same pattern as the AGWPE client.
- The kisstcp `Server` schedules a 30 s sweep on the engine loop (started in
  `Serve` when timeout > 0; cancelled in `Close`), closing clients idle past
  the timeout.

**Tests:** with a fake clock or short timeout: active client survives, idle
client is closed, sweep reschedules, Close cancels.

## F5 — Inflight, registration, and digipeater caps

**Finding:** engine-queue growth under client flood; unbounded 'X'
registrations per client; 'V'/'v' digipeater counts up to 255 (AX.25 allows 8).

**Design** (all follow the codebase's existing "over-limit → log + close
client" flow-control idiom):

- **Per-client inflight cap.** Each AGWPE and kisstcp client counts frames it
  has posted to the engine but which have not yet run (`inflight`, mutex
  guarded; incremented in the read goroutine before `eng.Do`, decremented
  inside the posted closure). Cap: 256 per client. On overflow: log and close
  the client — identical policy to the existing write-channel-full path. 256
  is far above any real burst (the engine drains thousands of cheap handlers
  per second) and bounds worst-case queued-payload memory to
  256 × 64 KiB × clients, several orders below the unbounded status quo.
- **Registration cap:** `maxRegisteredCalls = 8` per AGWPE client. An 'X'
  beyond the cap gets the same failure response as a duplicate (payload
  `0x00`), and a log line. Re-registering an already-held call is idempotent
  and does not count twice.
- **Digipeater cap:** 'V' and 'v' frames whose via count exceeds 8 are
  rejected (logged, frame dropped) rather than clamped — a client emitting
  >8 digis is broken and the drop surfaces it.

**Tests:** inflight cap closes a flooding client (drive the read loop with a
slow engine); 9th distinct 'X' → `0x00`, re-register of held call still
succeeds; 'V' with 9 digis is dropped, 8 works (golden-byte check on the
emitted UI frame unchanged for ≤8).

## F6 — Windows installer temp-file race

**Finding:** the GUI writes the predictable path `%TEMP%\tncd-setup.ini`,
then an elevated process reads it as the SYSTEM-service config.

**Design:** `gui_windows.go` uses `os.CreateTemp("", "tncd-setup-*.ini")`
(unpredictable, user-private), and deletes the file after the elevated
`install` exits. The elevated `install()` already validates the config before
copying — unchanged. Verified by `GOOS=windows` cross-compile (no Windows test
host here).

## F7 — Fuzz targets + CI vuln check

**Design:**

- Go fuzz targets over every untrusted-byte parser: `ax25.FuzzParseModulo`
  (first seed byte selects modulo 8/128), `ax25.FuzzParseXID`,
  `ax25.FuzzParseAddress`, `agwpe.FuzzParseHeader`, `kiss.FuzzDecoderFeed`
  (also asserts the F1 cap), `kiss.FuzzParseRFCOMMChannel`. Seed corpora from
  the existing golden fixtures. Plain `go test` runs the seeds (CI-friendly);
  actual fuzzing is `go test -fuzz` locally.
- CI: add a `govulncheck` step to the test workflow (`.github/workflows/`),
  non-blocking initially if the toolchain pin fights it.

---

## Explicitly out of scope

- Authentication/encryption on AGWPE or KISS-TCP protocols (incompatible with
  the reference implementations; mitigated by F2).
- On-air privacy (prohibited by regulation).
- Changing the Windows service account from LocalSystem (documented in the
  review as an operator consideration; no code change).

## Testing & release gate

Per `CLAUDE.md`, before merge:

1. `CGO_ENABLED=0 go test ./...` — all pass, including the new tests above.
2. `go vet ./...`.
3. Cross-compile matrix spot-check: linux/windows/darwin/freebsd amd64
   (`CGO_ENABLED=0 GOOS=... go build ./cmd/tncd`), required because F6 touches
   Windows-only code and F2/F4 touch all frontends.
4. `pytest -c e2e/pytest.ini e2e/` against the compiled binary (Dire Wolf/PAT
   harness) — validates no AGWPE/KISS wire regressions from F1/F5.
5. OTA test only if a maintainer judges the KISS decoder change to affect
   on-air timing (it should not: the cap is 20× larger than any legal frame).
