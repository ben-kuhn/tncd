# AGWPE-to-KISS Translation Bridge

A userspace bridge that allows AGWPE-compatible client applications to communicate
with KISS TNCs (Terminal Node Controllers), including full AX.25 connected-mode
support. Developed because Linux kernel 7.1 removed native AX.25 kernel support,
breaking applications that previously relied on `AF_AX25` sockets.

## Purpose

Many packet radio programs — Winlink clients (PAT, Winlink Express), APRS
applications (Xastir, APRSDroid), and BBS software (Paracon) — talk to TNCs using
the AGWPE protocol. This bridge translates AGWPE to KISS and implements the AX.25
layer 2 state machine that a KISS TNC does not provide.

## Architecture

```
[AGWPE Client App] ----TCP----> [agwkiss Bridge] ----KISS----> [TNC Hardware]
                          (port 8000)            (serial/TCP)
```

The bridge listens on port 8000 (configurable) for AGWPE client connections and
forwards frames to a KISS TNC via serial or TCP. Because KISS TNCs are dumb modems,
the bridge fully implements AX.25 connected mode: SABM/UA handshake, I-frame
sequencing, RR acknowledgement, duplicate detection, and clean DISC handling.

## Supported AGWPE Frame Types

| Kind | Direction | Description |
|------|-----------|-------------|
| `R`  | RX/TX | Version query / response |
| `G`  | RX/TX | Port info query / response |
| `g`  | RX/TX | Port capabilities query / response |
| `X`  | RX/TX | Register callsign |
| `x`  | RX    | Unregister callsign |
| `m`  | RX    | Enable frame monitoring |
| `y`  | RX/TX | Outstanding frames query (per port) |
| `Y`  | RX/TX | Outstanding frames query (per connection) — tracks unacked I-frames |
| `H`  | RX/TX | Heard stations query |
| `K`  | RX    | Raw KISS frame passthrough |
| `k`  | RX    | Raw KISS mode toggle |
| `M`  | RX    | Send UI (unproto) frame |
| `V`  | RX    | Send UI frame via digipeaters |
| `C`  | RX/TX | Connect / connected notification |
| `D`  | RX/TX | Send / receive connected data |
| `d`  | RX/TX | Disconnect / disconnected notification |
| `I`  | TX    | Monitor: received I-frame |
| `S`  | TX    | Monitor: received supervisory frame |
| `U`  | TX    | Monitor: received unnumbered frame |

## AX.25 Connected Mode

The bridge fully implements AX.25 v2.0 connected mode for KISS TNCs:

- **SABM/UA handshake** — outgoing connections (PAT/Winlink) and incoming
- **I-frame sequencing** — N(S)/N(R) send/receive sequence numbers, mod 8 window
- **RR acknowledgement** — sends RR only when remote polls (P=1); does not flood
  the channel with unsolicited RRs
- **Duplicate detection** — retransmitted I-frames from the remote are silently
  discarded; only in-sequence frames are forwarded to the AGWPE client
- **AGWPE flow control** — `Y` (outstanding frames) accurately reflects unacked
  I-frames so clients like PAT know when it is safe to send the next data block
- **TX echo suppression** — some TNCs (e.g. BTECH UV-Pro) echo transmitted frames
  back via KISS; these are detected and discarded
- **DISC/DM handling** — clean disconnect in both directions

## Supported TNC Connections

### Serial TNC

```ini
[client]
type = serial
device = /dev/ttyUSB0
baudrate = 9600
```

### Network KISS (TCP)

```ini
[client]
type = tcp
host = localhost
port = 8001
```

### Bluetooth TNC (RFCOMM)

Use `agwkiss-rfcomm` to manage the Bluetooth connection, then point the bridge at
the resulting `/dev/rfcomm0` device:

```ini
[client]
type = serial
device = /dev/rfcomm0

[bluetooth]
enabled = true
bind_dev = /dev/rfcomm0
bdaddr = 38:D2:00:01:52:8F
channel = 1
mode = watch        # auto-reconnect on drop
retry_delay = 5
```

`agwkiss-rfcomm` disconnects any active Bluetooth audio profile before connecting
so the serial channel is available, and releases the rfcomm binding on exit.

## Installation

### Dependencies

```bash
pip install kiss3 pyham-ax25 pyserial
```

Or using the provided requirements file:

```bash
pip install -r requirements.txt
```

### Via Nix (NixOS)

```nix
imports = [ /path/to/agwkiss/nix/overlay.nix ];
environment.systemPackages = [ pkgs.agwkiss ];
```

## Configuration

Copy `agwkiss.ini` and adjust for your setup:

```ini
[server]
listen_host = 0.0.0.0
listen_port = 8000
callsign = AGWPE

[client]
# type = serial or tcp
type = serial
device = /dev/ttyUSB0
baudrate = 9600

# Optional KISS timing parameters (values in 10ms units)
# tx_delay = 40
# persistence = 63
# slot_time = 20
# tx_tail = 30
# full_duplex = 0
```

## Usage

```bash
# Serial TNC
python agwkiss.py -c agwkiss.ini

# With verbose frame logging
python agwkiss.py -c agwkiss.ini -v    # frame types
python agwkiss.py -c agwkiss.ini -vv   # + data content
python agwkiss.py -c agwkiss.ini -vvv  # + AGWPE internals

# Bluetooth TNC — run rfcomm manager first (in a separate terminal or as a service)
sudo python agwkiss-rfcomm -c agwkiss.ini   # stays running, auto-reconnects
python agwkiss.py -c agwkiss.ini
```

## KISS Parameters

Configured under `[client]` in the INI file. All values are in 10ms units.

| Parameter    | Default | Description                      |
|--------------|---------|----------------------------------|
| tx_delay     | 40      | PTT key-up delay before TX data  |
| persistence  | 63      | CSMA persistence (0–255)         |
| slot_time    | 20      | CSMA slot time                   |
| tx_tail      | 30      | PTT hold after last byte         |
| full_duplex  | 0       | Full duplex mode (0 or 1)        |

## systemd Service

```bash
cp agwkiss.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable agwkiss
systemctl start agwkiss
```

See `agwkiss.service` for Bluetooth `ExecStartPre` configuration.

## Running Tests

```bash
pip install -r requirements-test.txt
pytest
```

## License

GNU General Public License v3.0 — see `COPYING`.
