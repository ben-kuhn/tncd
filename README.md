# tncd — AGWPE-to-KISS Bridge

A userspace bridge that allows AGWPE-compatible client applications to communicate
with KISS TNCs (Terminal Node Controllers), including full AX.25 connected-mode
support. Developed because Linux kernel 7.1 removed native AX.25 kernel support,
breaking applications that previously relied on `AF_AX25` sockets.

## Purpose

Many packet radio programs — Winlink clients (PAT), APRS
applications (Xastir), and BBS software (Paracon) — talk to TNCs using
the AGWPE protocol. This bridge translates AGWPE to KISS and implements the AX.25
layer 2 state machine that a KISS TNC does not provide.

## Architecture

```
[AGWPE Client App] ----TCP----> [tncd] ----KISS----> [TNC Hardware]
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

## APRS and Unproto (UI) Frames

APRS and other unconnected-mode applications (Xastir, etc.) are fully supported.
UI frames do not require the AX.25 layer 2 state machine — the bridge passes them
directly between the AGWPE client and the KISS TNC:

- **Send** — `M` (UI frame) and `V` (UI frame via digipeaters) transmit APRS packets
- **Receive** — enable monitoring with `m`; received UI frames are delivered as `U`
  monitor frames to all registered clients

No special configuration is needed; APRS and connected-mode (Winlink/PAT) clients
can share the same bridge instance simultaneously.

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

Use `tncd-rfcomm` to manage the Bluetooth connection, then point the bridge at
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

`tncd-rfcomm` disconnects any active Bluetooth audio profile before connecting
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

See [`nix/README.md`](nix/README.md) for the full NixOS module with service options,
automatic config generation, and Bluetooth support.

## Configuration

Copy `tncd.ini` and adjust for your setup:

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
python tncd.py -c tncd.ini

# With verbose frame logging
python tncd.py -c tncd.ini -v    # frame types
python tncd.py -c tncd.ini -vv   # + data content
python tncd.py -c tncd.ini -vvv  # + AGWPE internals

# Bluetooth TNC — run rfcomm manager first (in a separate terminal or as a service)
sudo python tncd-rfcomm -c tncd.ini   # stays running, auto-reconnects
python tncd.py -c tncd.ini
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

## Serial Port Parameters

Some TNCs require non-standard serial settings. Configure under `[client]`:

| Parameter | Default | Description                                  |
|-----------|---------|----------------------------------------------|
| parity    | `N`     | Parity: `N`=none, `O`=odd, `E`=even, `M`=mark, `S`=space |
| stopbits  | `1`     | Stop bits: `1`, `1.5`, or `2`               |
| rtscts    | `false` | Hardware RTS/CTS flow control                |

Example for AEA TNCs that use odd parity:

```ini
[client]
type = serial
device = /dev/ttyUSB0
baudrate = 9600
parity = O
stopbits = 1
```

## KISS Mode Initialization

Some serial TNCs (e.g. Kantronics KPC-3) power up in terminal mode and need a command
to enter KISS mode. Configure under `[client]`:

```ini
[client]
type = serial
device = /dev/ttyUSB0
baudrate = 9600
init_string = INT KISS\r
init_delay = 1.0
```

`init_string` is sent to the serial port before KISS framing starts. `\r` and `\n`
are interpreted as carriage return / line feed. `init_delay` (default 1.0 s) is the
wait after sending the string before opening the KISS connection.

## systemd Service

### Main bridge

```bash
cp tncd.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable tncd
systemctl start tncd
```

### Bluetooth rfcomm manager (optional)

Run `tncd-rfcomm` as a service to manage the Bluetooth connection independently of
the main bridge. It will connect on startup and automatically reconnect if the link drops.

```bash
cp tncd-rfcomm.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable tncd-rfcomm
systemctl start tncd-rfcomm
```

When using Bluetooth, uncomment the `After=tncd-rfcomm.service` lines in
`tncd.service` so the bridge starts after the rfcomm device is ready.

## Compatibility

### Clients

- [x] PAT (Winlink)
- [x] Paracon
- [ ] QTTermTCP
- [ ] Xastir

### Hardware

- [x] BTECH UV-Pro (Bluetooth)
- [ ] Mobilinkd TNC4 (Bluetooth)
- [ ] Mobilinkd TNC3 (Bluetooth)
- [ ] Mobilinkd TNC2 (Bluetooth)
- [ ] Kenwood TH-D7
- [ ] Kenwood TS-2000
- [ ] AEA PK-232
- [ ] AEA DSP-2232

## Running Tests

```bash
pip install -r requirements-test.txt
pytest
```

## License

GNU General Public License v3.0 — see `COPYING`.
