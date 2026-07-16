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
[kiss]
type = serial
device = /dev/ttyUSB1
serial_baudrate = 1200
init_string = INTFACE KISS\rRESET\r
init_delay = 2.0
send_kiss_exit = true

[client.0]
listen_port = 8005
callsign = KU0HN
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
[kiss]
type = serial
device = /dev/ttyUSB1
serial_baudrate = 9600
init_string = KISS ON\rRESTART\r
init_delay = 2.0
send_kiss_exit = true
host_exit_string = KISS OFF\r
exit_delay = 1.0

[client.0]
listen_port = 8005
callsign = KU0HN
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
[kiss]
type = serial
device = /dev/ttyUSB0
serial_baudrate = 57600
rtscts = false
send_kiss_exit = true

[client.0]
listen_port = 8005
callsign = KU0HN
ota_baudrate = 1200
```

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
[kiss]
type = bluetooth
bluetooth_address = <mobilinkd-bdaddr>
bluetooth_channel = 1   # TNC4; use 6 for TNC3
send_kiss_exit = true

[client.0]
listen_port = 8005
callsign = KU0HN
ota_baudrate = 1200
```

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
