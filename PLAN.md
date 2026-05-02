# AGWPE-to-KISS Translation Bridge

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

### Milestone 2: Bluetooth rfcomm Management (COMPLETE)
- `tncd-rfcomm` script to manage Bluetooth rfcomm bindings
- Disconnects audio profiles before connecting serial channel
- Auto-reconnect (`watch` mode) on connection drop
- Cleans up rfcomm binding on exit (SIGINT/SIGTERM)
- Config options for device, bdaddr, channel, retry delay

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

## systemd Service
Type: `simple`
User: configurable
Install: Copy to `/etc/systemd/system/`
