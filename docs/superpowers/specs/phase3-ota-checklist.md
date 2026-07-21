# Phase 3 OTA Validation Checklist — AX.25 v2.2 (mod-128) vs Direwolf

**Purpose**: Gate the `feature/ax25-v22` branch for real-radio hardware
correctness before merging to `main`. Run with the local `tncd-go` binary
(from the working tree or nix-ham-packages `tncd-go`).

This checklist covers a **tncd ↔ Direwolf** mod-128 connected round-trip.
A single TNC on the tncd side is sufficient — AX.25 v2.2 is entirely
host-side; proving it on one TNC proves it for all. The full hardware matrix
(all TNCs retested) is phase 4's job.

The software-level counterpart to this gate is `e2e/test_ax25_v22.py`, which
asserts the same Direwolf `"(v2.2)"` banner over a PipeWire audio cross-link.
The OTA gate verifies the same over-the-air with a real radio and TNC.

---

## Setup

### Radio / TNC side (tncd)

Any of the following works as the TNC on the tncd side:

- **Direwolf PTY** (`/tmp/kisstnc`) — lowest-friction first choice; no RF
  hardware needed on the tncd side, but requires two Direwolf instances (one
  PTY gateway, one "peer" over audio).
- **Kantronics KPC-3+** or any KISS serial TNC (use the ini snippet below).
- **Mobilinkd TNC4** (Bluetooth SPP) — omit `device`/`serial_baudrate`;
  use the Bluetooth snippet from `phase1-ota-checklist.md`.

### Radio side (Direwolf peer)

A second radio running **Direwolf 1.8.1** (or later) in its default
configuration (v2.2 default — no `V20` entries in the config). Direwolf must
have a callsign registered as the CMS gateway, e.g. `W0NE-10` on 145.030 MHz,
or a local Winlink gateway accessible by RF.

### Required software

- `tncd-go` binary built from `feature/ax25-v22` (or installed from
  nix-ham-packages with v2.2 support).
- `pat` (Winlink client) configured for AGWPE on `127.0.0.1:8005`,
  `radio_port = 0`.
- Direwolf 1.8.1 (peer side).

---

## PAT configuration notes (from phase 1)

- `agwpe` must be a **top-level key** in `~/.config/pat/config.json` — not
  nested under `ax25.agwpe`. `initAGWPE()` reads `config.AGWPE.Addr`.
- `radio_port` is 0-indexed and maps to `[client.N]`. A fresh PAT install ships
  with `radio_port: 1`; set it to `0` for a single-TNC setup (`[client.0]`).
- Use `echo "" | pat connect agwpe:///W0NE-10` to bypass the Winlink account
  activation prompt.

---

## tncd-ota.ini (v2.2 example — serial TNC)

Adapt `device` and `serial_baudrate` for your TNC. The key line is
`ax25_version = 2.2` (this is also the compiled-in default, so you can omit
it to verify the default path, or set it explicitly to be certain).

```ini
[server]
listen_host = 127.0.0.1
listen_port = 8005
callsign = KU0HN

[client.0]
type = serial
device = /dev/ttyUSB1
serial_baudrate = 1200
init_string = INTFACE KISS\rRESET\r
init_delay = 2.0
send_kiss_exit = true
ota_baudrate = 1200
ax25_version = 2.2

[kiss.0]
tx_delay = 40
persistence = 63
slot_time = 20
```

### Direwolf PTY variant (no RF hardware on the tncd side)

```ini
[client.0]
type = serial
device = /tmp/kisstnc
ota_baudrate = 1200
ax25_version = 2.2
```

---

## Pre-flight

- [ ] `tncd-go --version` — confirm this is the `feature/ax25-v22` build
- [ ] `ls -la /dev/ttyUSB1` (or your device) — device present, user in `dialout`
- [ ] `minicom -D /dev/ttyUSB1 -b 1200` — TNC in cmd mode (`cmd:` prompt), then
  exit minicom (the serial port must be free before starting tncd-go)
- [ ] Stop `pat.service` if running — the resident daemon grabs the AGWPE
  connection and locks out the CLI client (`systemctl --user stop pat` or
  `sudo systemctl stop pat`)
- [ ] Direwolf peer is on-air, callsign registered, reachable by RF

---

## Run tncd-go

```bash
tncd-go -c tncd-ota.ini
```

Expected startup log (serial TNC example):

```
KISS serial open /dev/ttyUSB1
probe: found "cmd:" → sending init_string
INTFACE KISS sent; RESET sent
AGWPE server listening on 127.0.0.1:8005
```

---

## AX.25 v2.2 connection test

```bash
echo "" | pat connect agwpe:///W0NE-10
```

### Step 1 — SABME handshake

Watch tncd-go log for:

- [ ] `SABME → W0NE-10` (tncd sends the v2.2 connect request)
- [ ] `UA ← W0NE-10` (Direwolf accepts and moves to connected state)

### Step 2 — XID negotiation

After UA, Direwolf (as initiator) sends an **XID command P=1**. Watch for:

- [ ] `XID cmd ← W0NE-10` (Direwolf's parameter negotiation)
- [ ] `XID rsp → W0NE-10` (tncd's negotiated response: SREJ=none, window=3,
  N1=256)

### Step 3 — Direwolf confirms v2.2

On the **Direwolf peer's** console or log, look for:

```
Connected to KU0HN (v2.2)
```

- [ ] Direwolf log shows `(v2.2)` — not `(v2.0)` or blank (which would indicate
  fallback to mod-8)

### Step 4 — mod-128 I-frame flow

Initiate a Winlink session (PAT downloads or uploads at least one message):

```bash
echo "" | pat connect agwpe:///W0NE-10
```

During the B2F session, confirm in tncd-go log:

- [ ] I-frames show `N(S)` and `N(R)` values advancing past 7 (e.g. `N(S)=8`,
  `N(S)=9` … `N(S)=127`, `N(S)=0` on wrap) — confirms mod-128 sequence numbers
  are in use
- [ ] No `REJ` or retransmit storms (clean channel); if any loss occurs, a single
  `REJ` triggers a retransmit of the correct frame only (not a full go-back-N
  dump, if SREJ negotiation succeeded)
- [ ] No `SREJ` sent by tncd-go (we advertise SREJ off; Direwolf should send
  `REJ` only after XID negotiation)

### Step 5 — data round-trip

- [ ] PAT downloads at least one Winlink CMS message (confirms tncd→AGWPE→PAT
  delivery of B2F payload over mod-128 I-frames)
- [ ] Optionally: upload a small message to verify the TX direction
  (`echo "" | pat compose -to W0NE@winlink.org -s "phase3 ota" -body "mod-128 test"`)

### Step 6 — graceful disconnect

- [ ] PAT sends `FQ` (B2F close); tncd log shows `DISC → W0NE-10`
- [ ] Direwolf replies `UA`; tncd shows `UA ← W0NE-10`
- [ ] No T1 timeouts or FRMR frames during teardown

---

## KISS exit verification

```bash
# Ctrl-C tncd-go (or systemctl stop), then:
minicom -D /dev/ttyUSB1 -b 1200
```

- [ ] TNC returns to `cmd:` prompt after KISS exit frame (`\xc0\xff\xc0`) —
  confirms graceful shutdown still works with v2.2 changes in place

---

## Fallback verification (optional but recommended)

Connect to an old-peer station known to use mod-8 (e.g. a KPC-3+ as the *peer*,
or any pre-v2.2 TNC on the other radio). Expect:

- [ ] tncd-go log shows `SABME → <old-peer>` then `DM ← <old-peer>` (or
  T1 timeout × 3), followed by `SABM → <old-peer>` (fallback), then `UA ←
  <old-peer>` (connected in mod-8)
- [ ] Direwolf (or old-peer) log shows `Connected to KU0HN` with no `(v2.2)`
  — or similar mod-8 indication
- [ ] Data session completes normally under mod-8

This confirms `ax25_version = 2.2` does not break connections with legacy peers.

---

## Pass criteria summary

| # | Criterion | Check |
|---|-----------|-------|
| 1 | SABME sent; UA received (v2.2 handshake) | [ ] |
| 2 | XID command received; XID response sent (SREJ=none, window=3, N1=256) | [ ] |
| 3 | Direwolf log shows `(v2.2)` (no fallback) | [ ] |
| 4 | mod-128 I-frame flow: N(S)/N(R) advance past 7 | [ ] |
| 5 | Direwolf sends REJ only (no SREJ after XID negotiation) | [ ] |
| 6 | Clean B2F data round-trip (at least one message transferred) | [ ] |
| 7 | Graceful DISC/UA teardown | [ ] |
| 8 | Fallback: SABME→DM→SABM→UA with old peer (optional) | [ ] |

---

## Results

| Date | TNC (tncd side) | Radio / Freq | Direwolf version | Outcome | Notes |
|------|-----------------|--------------|------------------|---------|-------|
|      |                 |              |                  |         |       |

---

## Sign-off

Phase 3 (`feature/ax25-v22`) merge gate: all criteria in the Pass criteria
table must be checked before merging to `main` with `git merge --no-ff`.

After the bench run:

1. Fill in the Results table above and commit the updated checklist.
2. `git checkout main && git merge --no-ff feature/ax25-v22`
3. Proceed to phase 3.5 (full SREJ / selective-repeat) or the 2.0.0 tag
   gate (phase 4), per the umbrella plan.
