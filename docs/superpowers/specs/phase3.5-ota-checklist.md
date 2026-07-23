# Phase 3.5 OTA Validation Checklist — SREJ Selective Recovery vs Direwolf

**Purpose**: Gate the `feature/srej` branch for real-radio hardware correctness
before merging to `main`. Run with the local `tncd-go` binary (from the working
tree or nix-ham-packages).

This checklist covers a **tncd ↔ Direwolf** SREJ selective-recovery test over a
**genuinely lossy path**. This is the critical difference from phase 3: SREJ only
fires on frame loss. A clean path will not exercise it at all. Run this on a weak
signal or with a deliberate attenuator — not a full-quieting path.

A single TNC on the tncd side is sufficient. The checklist requires two runs on
the same path: one with `srej = on` (selective repeat) and one with `srej = off`
(go-back-N), to confirm the throughput improvement SREJ is meant to deliver.

### Relationship to the unit/e2e tests

`e2e/test_ax25_v22.py` proves that a v2.2 link negotiates SREJ enabled (Direwolf
log confirms `srej_enable`), and that a clean round-trip still completes. It does
**not** prove selective recovery — the PipeWire cross-link has no loss. This OTA
checklist is the other half: lossy path, SREJ fires, recovery is correct, and the
throughput win is measurable.

---

## Setup

### Radio / TNC side (tncd)

Any of the following works as the TNC on the tncd side:

- **Kantronics KPC-3+** or any KISS serial TNC (see ini snippet below).
- **Direwolf PTY** (`/tmp/kisstnc`) — tncd side runs PTY; a second radio carries
  RF to Direwolf peer. This requires two radios to get genuine over-the-air loss.
- **Mobilinkd TNC4** (Bluetooth SPP) — omit `device`/`serial_baudrate`; use the
  Bluetooth snippet from `phase1-ota-checklist.md`.

### Peer side (Direwolf)

A second radio running **Direwolf 1.8.1** (or later) in its default configuration
(v2.2 default; no `V20` entries in the config file). Direwolf must have a
callsign registered as the CMS gateway (e.g. `W0NE-10` on 145.030 MHz) or a
local Winlink gateway accessible by RF.

Direwolf's default configuration enables SREJ on v2.2 links, so no special
Direwolf configuration is needed to exercise SREJ recovery.

### Lossy path requirement

**This checklist requires real frame loss.** Options:

- Run the test at the edge of the usable range (marginal signal — RST below 449,
  audible noise on the received audio).
- Insert an RF attenuator between the two radios (e.g. 20–40 dB) to bring the
  signal into the noisy regime deliberately.
- A full-quieting, strong path will not produce any SREJ frames and the two runs
  (`srej=on` vs `srej=off`) will appear identical — the checklist will be
  inconclusive.

### Required software

- `tncd-go` binary built from `feature/srej`.
- `pat` (Winlink client) configured for AGWPE on `127.0.0.1:8005`,
  `radio_port = 0`.
- Direwolf 1.8.1 (peer side).

---

## PAT configuration notes

- `agwpe` must be a **top-level key** in `~/.config/pat/config.json` — not
  nested under `ax25.agwpe`. `initAGWPE()` reads `config.AGWPE.Addr`.
- `radio_port` is 0-indexed and maps to `[client.N]`. A fresh PAT install ships
  with `radio_port: 1`; set it to `0` for a single-TNC setup (`[client.0]`).
- Use `echo "" | pat connect agwpe:///W0NE-10` to bypass the Winlink account
  activation prompt.

---

## tncd-ota.ini (SREJ example — serial TNC)

Adapt `device` and `serial_baudrate` for your TNC. The `srej = on` line is the
compiled-in default, so you can omit it to verify the default path, or set it
explicitly to be certain. `ax25_version = 2.2` is also the default.

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
srej = on

[kiss.0]
tx_delay = 40
persistence = 63
slot_time = 20
```

For the `srej=off` comparison run, change the one line:

```ini
srej = off
```

### Direwolf PTY variant (no RF hardware on the tncd side)

```ini
[client.0]
type = serial
device = /tmp/kisstnc
ota_baudrate = 1200
ax25_version = 2.2
srej = on
```

---

## Pre-flight

- [ ] `tncd-go --version` — confirm this is the `feature/srej` build
- [ ] `ls -la /dev/ttyUSB1` (or your device) — device present, user in `dialout`
- [ ] `minicom -D /dev/ttyUSB1 -b 1200` — TNC in cmd mode (`cmd:` prompt), then
  exit minicom (the serial port must be free before starting tncd-go)
- [ ] Stop `pat.service` if running — the resident daemon grabs the AGWPE
  connection and locks out the CLI client (`systemctl --user stop pat` or
  `sudo systemctl stop pat`)
- [ ] Direwolf peer is on-air, callsign registered, reachable by RF
- [ ] **Confirm the path has real loss** (RST below 449, or attenuator in line)
  before starting — a clean path makes this checklist inconclusive

---

## Run tncd-go with trace

Run with `-t` to capture the frame-level trace for both runs:

```bash
tncd-go -c tncd-ota.ini -t 2>&1 | tee tncd-srej-on.log
```

Expected startup log (serial TNC example):

```
KISS serial open /dev/ttyUSB1
probe: found "cmd:" → sending init_string
INTFACE KISS sent; RESET sent
AGWPE server listening on 127.0.0.1:8005
```

---

## Run 1: srej = on (selective repeat)

### Step 1 — SABME handshake

```bash
echo "" | pat connect agwpe:///W0NE-10
```

Watch tncd-go log for:

- [ ] `SABME → W0NE-10` (tncd sends the v2.2 connect request)
- [ ] `UA ← W0NE-10` (Direwolf accepts and moves to connected state)

### Step 2 — XID exchange (SREJ negotiation)

After UA, tncd sends an XID command to initiate SREJ negotiation. Watch for:

- [ ] `XID cmd → W0NE-10` (tncd advertises `SREJSingle`, window, N1=256, mod-128)
- [ ] `XID rsp ← W0NE-10` (Direwolf's negotiated response; both sides now know
  SREJ is enabled)

On the **Direwolf peer's** console or log, confirm:

- [ ] Direwolf log shows `(v2.2)` and indicates `srej_enable` (or equivalent) —
  not REJ-only

### Step 3 — B2F transfer over the lossy path

Initiate a Winlink session with enough data to encounter frame loss (download a
batch with several messages):

```bash
echo "" | pat connect agwpe:///W0NE-10
```

During the B2F session, watch tncd-go trace log (`-t`) for:

- [ ] I-frames arrive with N(S) values advancing — mod-128 in use (N(S) past 7
  means mod-128; N(S)/N(R) stay in 0–7 range means mod-8 fallback, stop here)
- [ ] At least one gap in N(S) detected (a missing frame): tncd logs `SREJ →
  W0NE-10 N(R)=<n>` (or equivalent trace message)
- [ ] The missing frame arrives from Direwolf after the SREJ (`I ← W0NE-10
  N(S)=<n>` retransmitted)
- [ ] V(R) advances smoothly after the gap fills (buffered frames delivered in
  order; no duplicate or out-of-order delivery to PAT)
- [ ] No `REJ` frames from tncd (confirming selective repeat is active, not
  go-back-N)

### Step 4 — data round-trip

- [ ] PAT downloads at least one Winlink CMS message successfully — confirms
  correct in-order delivery to the AGWPE client despite frame loss
- [ ] Note the elapsed time for the session and the number of SREJ / retransmit
  frames observed (for comparison in Step 6)

### Step 5 — graceful disconnect

- [ ] PAT sends `FQ` (B2F close); tncd log shows `DISC → W0NE-10`
- [ ] Direwolf replies `UA`; tncd shows `UA ← W0NE-10`
- [ ] No T1 timeouts or FRMR frames during teardown

---

## Interlude: KISS exit verification after Run 1

```bash
# Ctrl-C tncd-go (or systemctl stop), then:
minicom -D /dev/ttyUSB1 -b 1200
```

- [ ] TNC returns to `cmd:` prompt after KISS exit frame (`\xc0\xff\xc0`) —
  confirms graceful shutdown still works with SREJ changes in place

---

## Run 2: srej = off (go-back-N, same path)

Edit `tncd-ota.ini`, change `srej = on` to `srej = off`. Keep the radio path
and power settings identical to Run 1.

```bash
tncd-go -c tncd-ota.ini -t 2>&1 | tee tncd-srej-off.log
```

Repeat the same PAT session (same CMS connection, similar volume of data):

```bash
echo "" | pat connect agwpe:///W0NE-10
```

Watch tncd-go trace for:

- [ ] SABME → UA (v2.2 handshake still succeeds)
- [ ] XID exchange: tncd advertises `SREJNone`; no SREJ capability negotiated
- [ ] On frame loss: tncd sends `REJ → W0NE-10 N(R)=<n>` (go-back-N, not SREJ)
- [ ] Direwolf retransmits the whole window (multiple I-frames) after REJ —
  observe more on-air traffic than Run 1 for the same data volume
- [ ] Data still delivered correctly (go-back-N is not wrong, just slower)

---

## Comparison and pass criteria

Review the two log files (`tncd-srej-on.log` vs `tncd-srej-off.log`) and/or
time the two sessions:

- [ ] **SREJ frames observed** in Run 1 (`SREJ →` in log); `REJ` frames in Run 2
  (`REJ →` in log) — confirms each path was actually exercised
- [ ] **Data correct in both runs** — PAT received complete, uncorrupted messages
  in both cases
- [ ] **Run 1 (SREJ) visibly better than Run 2 (go-back-N)** on the same lossy
  path — fewer retransmitted frames in the log and/or shorter elapsed session
  time (even a 20–30% improvement is meaningful on a lossy path)
- [ ] **No out-of-order delivery** in Run 1 — PAT received messages in the
  correct sequence; no reassembly errors reported by PAT or by the B2F layer
- [ ] **Clean teardown in both runs** — DISC/UA exchange, no FRMR, no T1 storms

If the path produced no frame loss in either run, the checklist is
**inconclusive**: increase path loss (reduce power or increase attenuation) and
repeat from pre-flight.

---

## Pass criteria summary

| # | Criterion | Check |
|---|-----------|-------|
| 1 | SABME → UA (v2.2 handshake) in both runs | [ ] |
| 2 | XID exchange negotiates SREJ in Run 1; SREJNone in Run 2 | [ ] |
| 3 | Direwolf log shows `(v2.2)` in both runs (no mod-8 fallback) | [ ] |
| 4 | SREJ frames observed in Run 1 (at least one gap detected and requested) | [ ] |
| 5 | REJ frames observed in Run 2 (go-back-N path confirmed active) | [ ] |
| 6 | Data delivered intact in both runs (no corruption, no out-of-order to PAT) | [ ] |
| 7 | Run 1 shows fewer retransmitted I-frames / shorter session than Run 2 | [ ] |
| 8 | Graceful DISC/UA teardown in both runs | [ ] |
| 9 | KISS exit: TNC returns to cmd mode after tncd stop | [ ] |

---

## Fallback verification (optional but recommended)

Connect to an old-peer station known to use mod-8 (e.g. a KPC-3+ as the *peer*,
or any pre-v2.2 TNC on the other radio). Use `srej = on` in the ini. Expect:

- [ ] `SABME → <old-peer>` then `DM ← <old-peer>` (or T1 timeout × 3),
  followed by `SABM → <old-peer>` (fallback), then `UA ← <old-peer>`
- [ ] No XID exchange (mod-8 link; XID is not initiated)
- [ ] `srejEnabled` stays false; tncd uses REJ only (go-back-N) for the session
- [ ] Data session completes normally under mod-8 — `srej = on` does not break
  legacy peers

---

## Results

| Date | Path / loss level | TNC (tncd side) | srej=on result | srej=off result | Notes |
|------|-------------------|-----------------|----------------|-----------------|-------|
|      |                   |                 |                |                 |       |

---

## Sign-off

Phase 3.5 (`feature/srej`) merge gate: all criteria in the Pass criteria table
must be checked before merging to `main` with `git merge --no-ff`.

After the bench run:

1. Fill in the Results table above and commit the updated checklist.
2. `git checkout main && git merge --no-ff feature/srej`
3. Proceed to phase 4 (platforms & release gate / v2.0.0 tag), per the
   umbrella plan.
