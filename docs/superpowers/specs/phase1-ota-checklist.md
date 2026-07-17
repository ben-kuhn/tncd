# Phase 1 Serial OTA Validation Checklist — tncd 2.0 Go Port

**Purpose**: Gate the `v2-go-port` branch for hardware correctness before the 2.0.0 tag.
Run every test with the `tncd-go` binary from the Nix package (nix-ham-packages `tncd-go`).

**Status**: [ ] in-progress / [ ] all PASS

---

## PAT configuration notes (learned from 1.x)

- `agwpe` must be a **top-level key** in `~/.config/pat/config.json` — not nested under `ax25.agwpe`. `initAGWPE()` reads `config.AGWPE.Addr`.
- `radio_port` is 0-indexed and maps directly to the tncd `[client.N]` port number. A fresh PAT install ships with `radio_port: 1`, which will not match a single-TNC tncd setup (`[client.0]`). Set it to `0`.
- Use `echo "" | pat connect agwpe:///KU0HN-10` (pipe empty stdin to bypass the Winlink account activation prompt).

---

## TNC 1: Kantronics KPC-3+

**Serial**: `/dev/ttyUSB1`, 1200 baud, 8N1
**KISS entry**: `INTFACE KISS\r` + `RESET\r` via `init_string`, no manual minicom needed

### tncd-ota.ini excerpt

```ini
[server]
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
```

### Pre-flight

- [ ] `ls -la /dev/ttyUSB1` — device present, user in `dialout` group
- [ ] `minicom -D /dev/ttyUSB1 -b 1200` — TNC in cmd mode (`cmd:` prompt), then exit minicom

### Run tncd-go

```bash
tncd-go -c tncd-ota.ini
```

Expected startup log: `KISS serial open`, `INTFACE KISS` sent, `RESET` sent, then normal AGWPE listen.

### AX.25 connection test

```bash
echo "" | pat connect agwpe:///KU0HN-10
```

- [ ] Log shows `SABM → KU0HN-10`
- [ ] Log shows `UA ← KU0HN-10` (UA response within T1)
- [ ] Connected; PAT downloads at least one Winlink CMS message
- [ ] Disconnection shows `DISC` / `UA` exchange

### DTR / init behavior

- [ ] On `tncd-go` start: no unexpected TNC reset (DTR stays low / HUPCL off)
- [ ] On `tncd-go` stop (Ctrl-C or `systemctl stop`): KISS exit frame sent, TNC returns to cmd mode (`minicom -D /dev/ttyUSB1 -b 1200` shows `cmd:`)

### Result

```
Date: ___________  Result: PASS / FAIL
Notes:
```

---

## TNC 2: AEA PK-232MBX

**Serial**: `/dev/ttyUSB1`, 9600 baud, 8N1 (after factory re-config from 1200 7E1)
**KISS entry**: `KISS ON\r` + `RESTART\r` via `init_string`
**KISS exit**: `KISS OFF\r` via `host_exit_string` — clears NVRAM flag so TNC stays in cmd mode on power-cycle

### tncd-ota.ini excerpt

```ini
[server]
listen_port = 8005
callsign = KU0HN

[client.0]
type = serial
device = /dev/ttyUSB1
serial_baudrate = 9600
init_string = KISS ON\rRESTART\r
init_delay = 2.0
send_kiss_exit = true
host_exit_string = KISS OFF\r
exit_delay = 1.0
ota_baudrate = 1200
```

### Pre-flight

- [ ] `ls -la /dev/ttyUSB1` — device present
- [ ] `minicom -D /dev/ttyUSB1 -b 9600` — TNC in cmd mode (`cmd:` prompt), then exit minicom

### Run tncd-go

```bash
tncd-go -c tncd-ota.ini
```

Expected startup log: `KISS ON` sent, `RESTART` sent (with 2s delay), then normal AGWPE listen.

### AX.25 connection test

```bash
echo "" | pat connect agwpe:///KU0HN-10
```

- [ ] SABM → KU0HN-10, UA received
- [ ] PAT downloads at least one Winlink CMS message
- [ ] DISC / UA exchange on close

### DTR behavior

- [ ] On `tncd-go` start: no unexpected TNC reset (DTR stays low / HUPCL off)
- [ ] On `tncd-go` stop: KISS exit frame (`\xc0\xff\xc0`) sent, then `KISS OFF\r` sent after 1s delay

### KISS exit verification (critical for PK-232)

- [ ] After `tncd-go` stops: `minicom -D /dev/ttyUSB1 -b 9600` shows `cmd:` prompt (not KISS mode)
- [ ] Power-cycle the PK-232MBX, reconnect minicom: still shows `cmd:` (NVRAM flag cleared)

### Result

```
Date: ___________  Result: PASS / FAIL
Notes:
```

---

## TNC 3: Kenwood TS-2000 built-in TNC

**Serial**: `/dev/ttyUSB0`, 57600 baud, 8N1
**KISS entry**: Manual via minicom (programmatic `init_string` entry not yet reliable — known 1.x limitation)
**Hardware flow control**: MUST be disabled — Digirig uses RTS for PTT; enabling RtsCtS would key the transmitter

### tncd-ota.ini excerpt

```ini
[server]
listen_port = 8005
callsign = KU0HN

[client.0]
type = serial
device = /dev/ttyUSB0
serial_baudrate = 57600
rtscts = false
init_string = KISS ON\rRESTART\r
init_delay = 2.0
send_kiss_exit = true
ota_baudrate = 1200
```

Note: try programmatic KISS entry first — the TH-D7 (same TNC family)
entered KISS via init_string under the Go port on 2026-07-17, which 1.x
never managed on this family. Manual minicom entry remains the fallback.
Also: close the data-band SQUELCH (squelch = DCD on Kenwood internal
TNCs; open squelch silently blocks all TX).

### Pre-flight — manual KISS entry

```bash
minicom -D /dev/ttyUSB0 -b 57600
```

At the `cmd:` prompt, type:

```
kiss on
restart
```

Then exit minicom (`Ctrl-A X`). tncd-go must be started *after* minicom exits (serial port release).

### Run tncd-go

```bash
tncd-go -c tncd-ota.ini
```

### AX.25 connection test

```bash
echo "" | pat connect agwpe:///KU0HN-10
```

- [ ] SABM → KU0HN-10, UA received
- [ ] PAT downloads at least one Winlink CMS message
- [ ] DISC / UA exchange on close

### DTR behavior

- [ ] On `tncd-go` start: no unexpected TNC reset (HUPCL off confirmed)
- [ ] On `tncd-go` stop: KISS exit frame sent (TNC may return to cmd mode or require power-cycle)

### Result

```
Date: ___________  Result: PASS / FAIL
Notes:
```

---

## Bluetooth TNC — Mobilinkd (v2.0.0 tag gate, NOT phase 1)

**Scope**: This section is deferred to the 2.0.0 tag gate. Do NOT run during phase 1 serial OTA.

**Mobilinkd TNC3**: Bluetooth SPP channel 6
**Mobilinkd TNC4**: Bluetooth SPP channel 1

### Additional checks required beyond basic CMS round-trip

These checks are carried over from the Task 13 Bluetooth code review:

- [ ] **fd-count stability**: After 3 connect/disconnect cycles, `/proc/$(pgrep tncd-go)/fd | wc -l` must not grow. A leak here indicates unclosed BlueZ sockets.
- [ ] **BlueZ Disconnect latency before ConnectProfile**: Verify tncd-go calls `Disconnect` on any stale profile connection and waits for the `BlueZ.Error.NotConnected` ack before calling `ConnectProfile` — avoids racing the previous session teardown.
- [ ] **Profile registration survival**: Register the SPP profile, connect/disconnect 5 times; verify tncd-go does not need to re-register the profile on each connect cycle (profile should persist across sessions).
- [ ] **Reconnect after TNC sleep**: Put the Mobilinkd into low-power sleep (hold button), wake it, confirm tncd-go re-establishes the KISS connection without requiring a restart.

### Config excerpt (for reference)

```ini
[server]
listen_port = 8005
callsign = KU0HN

[client.0]
type = bluetooth
bdaddr = <mobilinkd-bdaddr>
channel = 1   # TNC4; use 6 for TNC3 (informational; SPP UUID drives connect)
reconnect = true
ota_baudrate = 1200
```
Note: KISS exit is serial-only; `send_kiss_exit` has no effect on
Bluetooth ports. Key names are `bdaddr`/`channel` (not
`bluetooth_address`/`bluetooth_channel`).

### Result

```
Date: ___________  Result: PASS / FAIL / DEFERRED
Notes:
```

---

## Sign-off

All phase 1 serial tests PASS:

- [ ] KPC-3+
- [ ] PK-232MBX
- [ ] TS-2000

Record final commit hash of `v2-go-port` under test: `___________`

## Direwolf 1.8.1 (software TNC via PTY) — added as first OTA subject

Setup: direwolf-vhf.conf (ADEVICE pipewire, MODEM 1200, PTT RIG via rigctld/TS-2000
RTS), `direwolf -p` PTY at /tmp/kisstnc; tncd-go [client.0] type=serial,
device=/tmp/kisstnc, ota_baudrate=1200; PAT via ax25+agwpe on 127.0.0.1:8000.

- [x] tncd-go opens PTY (SetDTR/SetRTS non-fatal ENOTTY warnings — expected)
- [x] AGWPE init from PAT ("AGWPE TNC (2.0) initialized")
- [x] SABM/UA connect to W0NE-10 on 145.030 MHz (~2 s)
- [x] Full Winlink B2F session: 2 messages received (2414 + 4787 bytes compressed)
- [x] Clean FQ disconnect, DISC/UA teardown
- [x] Graceful shutdown: SIGTERM → KISS exit (C0 FF C0) → clean stop

**Result: 2026-07-16 PASS** (KU0HN, W0NE-10 RMS, TS-2000 @ 145.030)

Notes:
- The resident `pat http` daemon grabs the AGWPE connection and registers the
  callsign as soon as tncd's port is up; a CLI `pat connect` is then refused
  ("callsign in use", correct duplicate-registration behavior). Stop pat.service
  during CLI-driven tests.
- Observed 1.x-parity behavior worth revisiting: an overheard I-frame P=1 from a
  foreign QSO (MNWIN→WT9M-4) triggered a DM transmission. Faithful port of
  tncd.py's stale-session DM, but it answers other stations' polls on shared
  channels. Candidate fix (phase 2 decision): only DM I-frames addressed to a
  registered/local callsign.

### Direwolf re-test 2026-07-16 (post shared-channel guards): PASS

Binary built from 1ca0a61 (all four IsLocal guards). Uploaded a 10,240-byte
random (incompressible) attachment to w0ne@winlink.org via W0NE-10 on
145.030 MHz: proposal FC EM 10612/10685 accepted, transmitted in one pass
(52 I-frames TX), FF/FQ clean disconnect, session ~2m20s. Foreign-TX check:
zero transmissions to non-KU0HN destinations in the tncd log during the
session (guards validated on a live shared channel). Graceful SIGTERM
shutdown with KISS exit confirmed again.

### Direwolf bidirectional test 2026-07-16: PASS

Single 35-second session, both directions: transmitted "W0NE Winlink Net
Check-In" (KCEREZ3BOHDM, 278/237 bytes, FS Y accepted) AND received the
W0NE reply "Re: tncd 2.0 OTA test - 10KB attachment" (KBFAAUXCMOFL,
413/315 bytes) with clean FF/FQ teardown. 44 frames total on the link,
zero foreign-destination transmissions. Message bodies verified intact
both ways.

### UV-PRO Bluetooth validation 2026-07-16: PASS (with findings)

Bench: KU0HN-10 gateway on 145.670, dummy load. Go binary at 1ca0a61.
- [x] BlueZ D-Bus SPP flow end-to-end: profile registration, ConnectProfile,
      NewConnection fd delivery, port online
- [x] Winlink CMS round-trip via KU0HN-10 (twice, clean FF/FQ)
- [x] Power-cycle reconnect: offline detect → 5s backoff → reconnect (cycle 1)
- [x] Profile registration survives reconnect cycles within one process
- [x] No echoed TX frames leaked past suppression into the RX path

Findings (fixes queued):
1. BUG: SPP profile registration failure is cached forever (sync.Once) — a
   UUID conflict (e.g. production 1.x tncd running) is unrecoverable without
   a process restart, despite the reconnect loop retrying.
2. BUG: reconnect leaks the previous SPP socket fd (one per successful
   reconnect; fds 9,10 observed stranded after two cycles).
3. Device note: UV-Pro auto-reconnects to the last host on power-up and can
   wedge in a half-open state after abrupt cycles (radio shows connected,
   BlueZ shows not; all inbound BR/EDR refused). Radio reboot recovers.
   tncd's retry loop behaves correctly; document in user docs.

### TNC4 Mobilinkd Bluetooth validation 2026-07-17: PASS

Bench: KU0HN-10 gateway on 145.670, dummy load. Go binary at b3459b8
(includes both bench-finding fixes). Device freshly calibrated.
- [x] Registration-retry fix demonstrated live: calibration had cleared the
      TNC4's link key (br-connection-key-missing); the running process kept
      retrying through re-pairing and connected on the next backoff attempt
      — no restart needed (impossible before 848b246)
- [x] Winlink CMS round-trip via KU0HN-10, clean FF/FQ
- [x] Power-cycle reconnect: offline → backoff → reconnected; one transient
      br-connection-already-connected (device-side auto-reconnect collision)
      absorbed by the retry loop
- [x] fd-leak fix verified on hardware: fd count flat at 9 across the cycle
      (75afa32/b3459b8; yesterday's build leaked one per reconnect)

Device note: Mobilinkd calibration/config sessions can clear the TNC's
stored link keys — if ConnectProfile fails with br-connection-key-missing,
remove and re-pair in bluetoothctl (tncd recovers automatically once paired).

### TNC4 stress test 2026-07-17: PASS

10,240-byte random (incompressible) attachment to w0ne@winlink.org via
KU0HN-10 (bench, 145.670). Proposal 10592/10670 accepted, 56 I-frames TX,
one over-the-air frame loss recovered via REJ retransmission (correct
ack-then-retransmit ordering, matching 1.x), clean FF/FQ, message in sent/.
Zero foreign-destination transmissions. Y-frame flow control observed
reporting outstanding=8 under load (window + queue accounting working).

### TNC3 Mobilinkd Bluetooth validation 2026-07-17: PASS

Bench: KU0HN-10 gateway on 145.670, dummy load. Go binary at b3459b8.
- [x] SPP connect first try (pairing survived calibration, unlike TNC4)
- [x] Winlink CMS round-trip, clean FF/FQ
- [x] Power-cycle reconnect: device-side auto-reconnect collided twice
      (br-connection-already-connected); backoff absorbed it, reconnected
      on 3rd attempt. fd count flat (9→8 during outage→9 reconnected).
- [x] 10KB stress: random attachment ZIXYHDC6BP2B (10592/10669) accepted
      and transferred, 57 I-frames, zero REJs (clean channel this run),
      in sent/. Zero foreign-destination transmissions.

Note: 10KB incompressible upload is now part of the standard per-device
validation suite.

### TNC2 Mobilinkd Bluetooth validation 2026-07-17: TESTED — APRS ONLY

Bench: KU0HN-10 gateway on 145.670. Legacy RN-42 Bluetooth: PIN pairing
(1234) required via bluetoothctl agent. Not previously tested on any
tncd version.
- [x] SPP connect via PIN-paired RN-42; tncd BlueZ flow works unchanged
- [x] AX.25 connected-mode session established (SABM/UA) after a PTT
      calibration fix
- [x] CMS session opened, message proposal accepted, all transmitted data
      link-layer acknowledged (RR to N(R)=7, outstanding drained to 0)
- [~] Session lost mid-B2F: TNC2 RX went deaf after our transmit burst
      (2 min silence, gateway DISC, correctly UA'd). Device-class
      limitation — the TNC2 is APRS-oriented; connected-mode Winlink is
      beyond its comfort zone per the operator. tncd's link handling was
      correct throughout (incl. riding through one BT drop during the
      earlier PTT-dead attempt).

Verdict "tested, APRS only" (operator's call): the partial connected-mode
session validates real AX.25 frame TX and RX through the device. Use for
APRS/UI; prefer TNC3/TNC4 for connected mode.

### TH-D7 internal TNC (serial) validation 2026-07-17: PASS

First real serial-hardware test of the Go port. /dev/ttyUSB1 @ 9600,
init_string = KISS ON\rRESTART\r, init_delay 2.0. Bench: KU0HN-10 on
145.670.
- [x] Command-mode probe → "cmd:" detected → init sent → KISS confirmed —
      programmatic KISS entry WORKED on a Kenwood internal TNC (the same
      TNC family where 1.x always required manual minicom entry)
- [x] KISS exit on shutdown verified: after SIGTERM, next start's probe
      found "cmd:" again (C0 FF C0 returned the TNC to command mode)
- [x] Winlink CMS session via KU0HN-10: queued 291-byte message delivered,
      clean FF/FQ (operator criteria: connect + data transfer = PASS for
      memory-limited onboard TNCs; no 10KB stress attempted)

Debug journey (documented for the user docs):
- No-TX mystery: frames delivered to TNC, radio never keyed. Root cause:
  data-band SQUELCH OPEN — Kenwood internal TNCs use the squelch line as
  DCD, so open squelch = channel busy = CSMA defers forever (same class as
  the 1.x DSP-2232 finding). Mobilinkd practice (open squelch, DSP DCD)
  is the opposite — easy trap when sharing a radio between both. Closing
  squelch released the queued frames instantly (confirmed on RF monitor).
- TH-D7 serial is 3-wire (TX/RX/GND) — no RTS/DTR reach the TNC.
- NOTE (corrected 2026-07-17): during this debug an RTS-low→high flip was
  committed on the mistaken theory the D7 needed RTS asserted. The real
  cause was the squelch; the D7 doesn't even wire RTS. That flip was
  reverted (bf8efb8) — serial RTS stays LOW on open (Digirig RTS→PTT safe),
  the original design.

### TS-2000 internal TNC (serial) validation 2026-07-17: PASS at 9600

Bench path: Digirig serial → TS-2000 COM connector; W0NE-10 on 145.030.
- [x] Programmatic KISS entry WORKED (probe → "cmd:" → KISS ON/RESTART →
      confirmed) — 1.x never achieved this on the TS-2000; the Go serial
      path (read timeouts) cracked it. Manual minicom no longer needed.
- [x] KISS exit on stop returns TNC to cmd mode (verified via re-probe)
- [x] Winlink CMS message delivered via W0NE-10, clean FF/FQ — at 9600.
- [!] At 57600 (the 1.x-documented rate): B2 "data stream not a correct
      format" corruption, reproduced twice, same message. Dropped to 9600
      (controlled variable): identical test passed cleanly. Verdict:
      byte-level corruption on this TNC's serial input at 57600 without
      hardware flow control (go.bug.st cannot apply CRTSCTS — the deferred
      Task 12 gap now has a concrete customer). RECOMMEND serial_baudrate
      9600 for TS-2000 under 2.0 until termios CRTSCTS support lands.
- Note: menu 56 (COM CONNECTOR PARAMETERS) baud changes require a radio
  reboot to take effect.

Bench lessons (same session, cabling):
- The TS-2000 COM port serves CAT (rigctld) AND the internal TNC — rigctld
  must be stopped for TNC use; a running rigctld silently consumes all TNC
  serial output.
- Digirig cabling trap: with the serial plug pulled but audio connected,
  the only live line is RTS→PTT — opening the port keys the transmitter
  with no data path at all. Verify both plugs before serial TNC sessions.
