# Multi-Port / Multi-Modem Support

## Goal

Allow tncd to manage multiple KISS TNC connections simultaneously, each mapped
to a different AGWPE port number. Users can permanently configure all their TNCs
(serial, TCP, Bluetooth) and select the appropriate one from their AGWPE client
application at usage time.

## Config Format

Ports are defined as numbered `[client.N]` sections. Port numbering starts at 0
and must be contiguous (no gaps). Each port is fully independent with its own
connection type, parameters, and AX.25 state.

```ini
[server]
listen_host = 0.0.0.0
listen_port = 8000
callsign = KU0HN

[client.0]
name = TNC3 Mobilinkd (2m)
type = bluetooth
bdaddr = 34:81:F4:3D:98:4B
ota_baudrate = 1200

[client.1]
name = TS-2000 (HF)
type = serial
device = /dev/ttyUSB0
serial_baudrate = 57600
ota_baudrate = 1200

[client.2]
name = Direwolf (testing)
type = tcp
host = 127.0.0.1
port = 8001
ota_baudrate = 1200

[kiss.1]
tx_delay = 80
persistence = 32
```

### Fields

Each `[client.N]` section supports:

| Field | Required | Description |
|-------|----------|-------------|
| `name` | No | Human-readable port name shown to AGWPE clients. Default: `Port N` |
| `type` | Yes | Connection type: `serial`, `tcp`, or `bluetooth` |
| All existing `[client]` fields | — | `device`, `serial_baudrate`, `host`, `port`, `bdaddr`, `channel`, `ota_baudrate`, `reconnect`, `reconnect_delay`, `reconnect_max_delay`, `parity`, `stopbits`, `rtscts`, `init_string`, `init_delay` |

### KISS Parameters

KISS parameters are configured per-port in `[kiss.N]` sections:

| Field | Default | Description |
|-------|---------|-------------|
| `tx_delay` | 40 | PTT key-up delay (10ms units) |
| `persistence` | 63 | CSMA persistence (0-63) |
| `slot_time` | 20 | CSMA slot time (10ms units) |
| `tx_tail` | 30 | PTT hold after last byte (10ms units) |
| `full_duplex` | 0 | Full duplex mode (0 or 1) |

If no `[kiss.N]` section exists for a port, hardcoded defaults apply.

### Backward Compatibility

- A bare `[client]` section (no `.N` suffix) is treated as `[client.0]`
- A bare `[kiss]` section (no `.N` suffix) is treated as `[kiss.0]`
- Existing single-port configs work unchanged with zero migration
- When the old format is detected, emit a deprecation warning at startup suggesting migration to the `[client.0]` / `[kiss.0]` format

## Architecture

### Bridge Changes

- `Bridge` holds a list of `KISSClient` instances indexed by port number
- Each `KISSClient` has its own:
  - Connection to the TNC (serial/TCP/Bluetooth)
  - AX.25 connection state (`connections` dict)
  - T1/T2 timers and window size (derived from per-port `ota_baudrate`)
  - KISS parameters (from `[kiss.N]` or defaults)
  - Online/offline status
- The AGWPE header's port byte routes frames to the correct `KISSClient`
- When a `KISSClient` receives a KISS frame, it dispatches to Bridge with its port number

### AGWPE Protocol

**`G` frame (port info):**

Returns the count and names of all configured ports:

```
<count>;Port 1 name;Port 2 name;...;
```

Example: `3;TNC3 Mobilinkd (2m);TS-2000 (HF);Direwolf (testing);`

Names come from the `name` config field. If omitted, defaults to `Port N`.

**`g` frame (port capabilities):**

Returns KISS parameters for the specific port indicated by the AGWPE header's
port byte. Looks up `[kiss.N]` for that port.

**`y` frame (outstanding frames per port):**

Routes to the correct port's KISSClient to count pending frames.

**`Y` frame (outstanding frames per connection):**

Already keyed by `(port, local_call, remote_call)` — works once connections
are tracked per-port.

**All other frames (`C`, `D`, `d`, `M`, `V`, `K`, etc.):**

Route to the correct KISSClient by the port byte in the AGWPE header.

### Startup

- All ports attempt connection in parallel (asyncio tasks)
- Ports that fail enter their reconnect loop independently
- Bridge starts the AGWPE server immediately — does not wait for all TNCs
- AGWPE clients can connect and query port info before all TNCs are online

### Offline Port Behavior

- **`C` (connect) on offline port** → immediate `*** BUSY From <call>` + `d` frame to client
- **`M`/`V` (UI frame) on offline port** → silently dropped
- **`K` (raw KISS) on offline port** → silently dropped
- **Invalid port number** (>= port count) → silently ignored

### Port Goes Offline Mid-Session

- Active AX.25 connections on that port receive `*** DISCONNECTED From <call>` notification
- Port enters its reconnect loop
- When TNC reconnects, port is ready for new connections (old sessions are gone)

### Multiple AGWPE Clients

- Multiple clients can register callsigns on any port
- Monitoring works across all ports
- A client can have connections on different ports simultaneously
- Same behavior as current single-port, extended to N ports

## Nix Module

The `formats.ini` generator handles dotted section names as literal INI section
headers. No special module changes needed beyond updating examples:

```nix
services.tncd.settings = {
  server.callsign = "KU0HN";
  "client.0" = {
    name = "TNC3 Mobilinkd (2m)";
    type = "bluetooth";
    bdaddr = "34:81:F4:3D:98:4B";
    ota_baudrate = 1200;
  };
  "client.1" = {
    name = "TS-2000 (HF)";
    type = "serial";
    device = "/dev/ttyUSB0";
    serial_baudrate = 57600;
    ota_baudrate = 1200;
  };
  "kiss.1".tx_delay = 80;
};
```

`bluetooth.enable` still adds deps and group membership — needed if any port
uses `type = bluetooth`.

## Validation

At startup, tncd validates:

1. At least one `[client.N]` section exists
2. Port numbers are contiguous starting from 0
3. Each port has a valid `type` field
4. Required fields per type are present (`bdaddr` for bluetooth, `device` for serial, `host`+`port` for tcp)

Invalid config → exit with clear error message.

## Testing

- Unit tests: multi-port config parsing, frame routing by port byte, `G`/`g` responses with multiple ports
- Unit tests: offline port behavior (BUSY response, reconnect)
- E2E tests: two Direwolf instances on different ports, PAT connects to each independently
