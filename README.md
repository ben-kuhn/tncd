# tncd — AGWPE-to-KISS Bridge

A userspace bridge that lets AGWPE-compatible applications (PAT/Winlink, Paracon,
Xastir) communicate with KISS TNCs, including full AX.25 connected-mode support.
Tested over the air with real Winlink sessions at 1200 baud. Developed because
Linux kernel 7.1 removed native AX.25 kernel support, breaking applications that
previously relied on `AF_AX25` sockets.

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
- **RR acknowledgement** — T2 delayed ACK batches acknowledgments for burst
  I-frames; immediate response to polls (P=1)
- **Piggybacked N(R)** — outgoing and retransmitted I-frames carry the current
  receive sequence number, so data transfer implicitly acknowledges received frames
- **Duplicate detection** — retransmitted I-frames from the remote are silently
  discarded; only in-sequence frames are forwarded to the AGWPE client
- **AGWPE flow control** — `Y` (outstanding frames) accurately reflects unacked
  I-frames so clients like PAT know when it is safe to send the next data block
- **TX echo suppression** — some TNCs (e.g. BTECH UV-Pro) echo transmitted frames
  back via KISS; these are detected and discarded
- **Dynamic T1/T2 timers** — retransmit and delayed-ACK timeouts calculated from
  the configured over-the-air baud rate and window size
- **DISC/DM handling** — clean disconnect in both directions

## Supported TNC Connections

### Serial TNC

Direct serial connection to a TNC via USB or RS-232.

### Network KISS (TCP)

Connects to a KISS-over-TCP server (e.g. Dire Wolf, YAAC).

### Bluetooth TNC (RFCOMM)

Use `tncd-rfcomm` to manage the Bluetooth connection. It disconnects any active
audio profile before connecting so the serial channel is available, and releases
the rfcomm binding on exit. Point the bridge at the resulting `/dev/rfcomm0` device.

## Installation

### Debian / Ubuntu

```bash
curl -fsSL https://tncd.dev/tncd.pub \
  | sudo gpg --dearmor -o /usr/share/keyrings/tncd.gpg
echo "deb [signed-by=/usr/share/keyrings/tncd.gpg] https://tncd.dev/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/tncd.list
sudo apt update && sudo apt install tncd
```

### Fedora / RHEL / openSUSE

```bash
sudo curl -fsSL https://tncd.dev/rpm/tncd.repo -o /etc/yum.repos.d/tncd.repo
sudo dnf install tncd          # Fedora / RHEL
# sudo zypper install tncd     # openSUSE
```

### Arch Linux (AUR)

```bash
yay -S tncd
```

### From source / pip

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
serial_baudrate = 9600
ota_baudrate = 1200     # over-the-air baud rate (for T1/T2 timer calculation)
# parity = N            # N=none, O=odd, E=even, M=mark, S=space (default: N)
# stopbits = 1          # 1, 1.5, or 2 (default: 1)
# rtscts = false        # RTS/CTS hardware flow control (default: false)

# KISS mode initialization (for TNCs that need a command to enter KISS mode)
# init_string = INT KISS\r   # \r = CR, \n = LF
# init_delay = 1.0

[kiss]
# KISS timing parameters (values in 10ms units)
# tx_delay = 40
# persistence = 63
# slot_time = 20
# tx_tail = 30
# full_duplex = 0
```

### Bluetooth TNC

```ini
[client]
type = serial
device = /dev/rfcomm0

[bluetooth]
enabled = true
bind_dev = /dev/rfcomm0
bdaddr = AA:BB:CC:DD:EE:FF
channel = 1
mode = watch        # auto-reconnect on drop
retry_delay = 5
```

## Usage

```bash
# Serial TNC (packaged install)
tncd -c /etc/tncd.ini

# Serial TNC (source install)
python tncd.py -c tncd.ini

# With verbose frame logging
tncd -c /etc/tncd.ini -v    # frame types
tncd -c /etc/tncd.ini -vv   # + data content
tncd -c /etc/tncd.ini -vvv  # + AGWPE internals

# Bluetooth TNC — run rfcomm manager first (in a separate terminal or as a service)
sudo tncd-rfcomm -c /etc/tncd.ini   # stays running, auto-reconnects
tncd -c /etc/tncd.ini
```

## KISS Parameters

Configured under `[kiss]` in the INI file. All values are in 10ms units.

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
serial_baudrate = 9600
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
serial_baudrate = 9600
init_string = INT KISS\r   # \r and \n are interpreted as CR/LF
init_delay = 1.0
```

`init_string` is sent to the serial port before KISS framing begins. Use `\r` for
carriage return and `\n` for line feed — most TNCs expect `\r` to terminate a command.
`init_delay` (default 1.0 s) is the wait after sending the string before the KISS
connection opens.

## systemd Service

### Packaged install (apt / dnf / zypper / AUR)

Service files are installed automatically. Copy the example config and start:

```bash
sudo cp /etc/tncd.ini.example /etc/tncd.ini
$EDITOR /etc/tncd.ini
sudo systemctl enable --now tncd
```

For Bluetooth, also enable the rfcomm manager:

```bash
sudo systemctl enable --now tncd-rfcomm
```

### Manual / source install

```bash
cp tncd.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now tncd
```

When using Bluetooth, uncomment the `After=tncd-rfcomm.service` lines in
`tncd.service` so the bridge starts after the rfcomm device is ready.

## Compatibility

### Clients

- [x] PAT (Winlink) — connected mode and UI frames, OTA-verified
- [x] Paracon
- [ ] QTTermTCP
- [ ] Xastir

### Software TNCs

- [x] Dire Wolf — KISS over TCP and PTY serial, OTA-verified at 1200 baud

### Hardware TNCs

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
