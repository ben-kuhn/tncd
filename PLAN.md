# AGWPE-to-KISS Translation Bridge

> **Historical planning doc (1.x Python line).** This describes the original
> design and milestones; the dependency/architecture details below (kiss3,
> pyham, `tncd.py`) refer to the Python implementation, now on the
> [`v1` branch](https://github.com/ben-kuhn/tncd/tree/v1). `main` is the **2.0
> Go** rewrite — see [`CLAUDE.md`](CLAUDE.md) for its architecture and
> `docs/superpowers/` for the 2.0 design/plan docs.

## Purpose
A userspace bridge allowing AGWPE-compatible client applications to communicate with KISS TNCs (serial or TCP). Essential since Linux 7.1 removed native AX.25 kernel support.

## Architecture
```
[AGWPE Client App] ----TCP----> [This Bridge] ----KISS----> [TNC Hardware]
                         (pyham_pe)     (kiss3)      (serial/TCP)
```

## Dependencies
| Package | Purpose |
|---------|---------|
| `kiss3` | KISS client - connects to TNC via serial OR TCP |
| `pyham-ax25` | AX.25 frame encode/decode |
| `pyserial` | Serial port access |

Note: `pyham-pe` is a *client* library (used by Paracon, etc.) that connects to this bridge. It is not a runtime dependency of tncd itself, but is used in tests to verify protocol compatibility.

## Configuration
Command-line args OR config file (`tncd.ini`):

```ini
[server]
listen_host = 0.0.0.0
listen_port = 8000

[client]
type = serial
device = /dev/ttyUSB0
serial_baudrate = 9600
ota_baudrate = 1200
```

## Milestones

### Milestone 1: Core Bridge (COMPLETE)
- AGWPE server accepting client connections
- KISS client connecting to TNC (serial or TCP)
- UI frame translation (UNPROTO / monitor)
- Config via CLI args OR config file
- systemd service ready

### Milestone 2: Bluetooth rfcomm Management (RETIRED)
- `tncd-rfcomm` standalone script has been removed; superseded by Milestone 4
- Bluetooth SPP is now handled in-process via BlueZ D-Bus Profile API in `tncd.py`

### Milestone 3: AX.25 Connected Mode (COMPLETE)
Full AX.25 layer 2 implementation for KISS TNCs:

- SABM/UA handshake (outgoing and incoming connections)
- I-frame sequencing with N(S)/N(R) mod-8 window
- RR acknowledgement — only sent for P=1 polls (no channel flooding)
- Duplicate I-frame detection — retransmits discarded, in-sequence only forwarded
- TX echo suppression — TNCs that echo transmitted frames (e.g. UV-Pro) are handled
- AGWPE `Y` outstanding-frames tracking — accurately reports unacked I-frame count
  so clients (PAT/Winlink) know when to send the next data block
- DISC/DM handling in both directions
- Correct AX.25 C/R (command/response) bits on all frame types

### Milestone 4: Native Bluetooth SPP (COMPLETE)
Direct Bluetooth SPP connection via BlueZ D-Bus Profile API:

- Register SPP profile via `ProfileManager1`, receive connected fd via `NewConnection`
- Auto-detect RFCOMM channel via SDP (no manual configuration needed)
- Disconnect existing BLE auto-connection before SPP connect (dual-mode devices)
- Auto-reconnect with configurable exponential backoff
- SABM retransmission via T1 timer during connection setup (AX.25 6.3.1)
- Optional `dbus-python`/`PyGObject` import (only when `type = bluetooth`)
- Deprecates `tncd-rfcomm` helper

### Milestone 5: Multi-Port / Multi-Modem Support (COMPLETE)
Multiple KISS TNC connections managed simultaneously:

- `[client.N]` numbered port sections with per-port config
- `[kiss.N]` per-port KISS parameters (defaults if no section)
- Per-port AX.25 state (connections, T1/T2 timers, window size derived from `ota_baudrate`)
- AGWPE `G` frame reports port count and human-readable names
- AGWPE `g` frame returns per-port KISS capabilities
- Ports connect in parallel at startup; offline ports return BUSY for `C` frames
- Port going offline disconnects active sessions with notification
- Backward compatible: bare `[client]`/`[kiss]` treated as port 0 with deprecation warning

## systemd Service
Type: `simple`
User: configurable
Install: Copy to `/etc/systemd/system/`
