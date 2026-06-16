# Bluetooth SPP D-Bus Integration Design

**Date**: 2026-05-15
**Status**: Approved
**Branch**: `feature/bluetooth-dbus`

## Problem

BlueZ 5.x removed native SPP profile support. The legacy `rfcomm` tool no longer works
(`rfcomm connect` returns "Permission denied", `rfcomm bind` creates a device node but
doesn't establish a Bluetooth link). The current `tncd-rfcomm` helper script depends on
these deprecated tools and cannot function on modern Linux systems.

## Solution

Integrate Bluetooth SPP directly into tncd's `KISSClient` using the BlueZ D-Bus Profile
API. When `type = bluetooth` is configured, tncd registers a custom SPP profile, connects
to the TNC via D-Bus, receives a connected file descriptor, and wraps it in a standard
asyncio transport. The rest of tncd (Bridge, AGWPEServerProtocol, KISSProtocol) works
unchanged.

## Connection Flow

1. Import `dbus-python` and `PyGObject` at runtime (raise clear error if missing).
2. Register SPP profile via `ProfileManager1.RegisterProfile()` with UUID
   `00001101-0000-1000-8000-00805f9b34fb`.
3. Call `Device1.ConnectProfile(uuid)` on the target `bdaddr`. BlueZ performs SDP lookup
   to find the correct RFCOMM channel automatically (optional `channel` config overrides).
4. BlueZ calls our profile's `NewConnection(path, fd, fd_properties)` method with a
   connected unix file descriptor.
5. Wrap fd: `socket.fromfd(fd, AF_UNIX, SOCK_STREAM)` then
   `loop.create_connection(KISSProtocol, sock=sock)` to get a standard asyncio transport.
6. Store transport/protocol on `self.connection` — identical interface to serial/TCP from
   this point forward.

## Reconnection

When the Bluetooth link drops (TNC powered off, out of range, link loss):

1. **Detect**: `connection_lost()` on KISSProtocol, or D-Bus `PropertiesChanged` signal
   on `Device1.Connected`.
2. **Notify Bridge**: tear down active AX.25 connections, send DM to pending sessions.
3. **Retry with backoff**: `ConnectProfile()` again with exponential backoff starting at
   `reconnect_delay` (default 5s), doubling up to `reconnect_max_delay` (default 60s),
   reset on successful connect.
4. **On reconnect**: re-wrap new fd, create new transport/protocol, re-register with
   Bridge. AGWPE clients stay connected (they see the TNC go away and come back).

Setting `reconnect = false` makes tncd exit on disconnect (systemd can restart it).

## Configuration

```ini
[client]
type = bluetooth
bdaddr = AA:BB:CC:DD:EE:FF
# channel = 6              # optional, auto-detected via SDP
ota_baudrate = 1200
reconnect = true           # default: true
reconnect_delay = 5        # initial delay in seconds, default: 5
reconnect_max_delay = 60   # cap in seconds, default: 60
```

## Dependencies

`dbus-python` and `PyGObject` are imported only when `type = bluetooth` is configured.
If missing, tncd raises:

```
RuntimeError: Bluetooth support requires dbus-python and PyGObject:
              pip install dbus-python PyGObject
```

### requirements.txt

```
kiss3>=8.0.0
pyham-ax25>=1.0.0
pyserial>=3.5

# Bluetooth support (Linux only)
dbus-python>=1.2.0; sys_platform == 'linux'
PyGObject>=3.40.0; sys_platform == 'linux'
```

### Packaging

- **Nix module** (`nix/module.nix`): conditionally add `python3Packages.dbus-python` and
  `pygobject3` when `services.tncd.bluetooth` options are configured.
- **Nix package** (`tncd/default.nix` in nix-ham-packages): add optional
  `bluetoothSupport` flag (default false) that pulls in `dbus-python` + `pygobject3`.
- **PKGBUILD**: add `python-dbus` and `python-gobject` to depends.
- **fpm (.deb/.rpm)**: add `python3-dbus` and `python3-gi` to package dependencies.

## Platform Notes

The D-Bus/BlueZ integration is Linux-only. Other platforms handle SPP differently:

- **macOS**: Bluetooth stack exposes SPP devices as `/dev/tty.*` serial ports after
  pairing in System Settings. Use `type = serial` with the device path.
- **Windows**: After pairing in Bluetooth Settings, Windows creates a COM port. Use
  `type = serial` with the COM port (e.g. `device = COM5`).

These platform differences will be documented in README.md.

## Documentation

**README.md** additions:

1. Bluetooth prerequisites: pair and trust TNC via `bluetoothctl` before configuring tncd.
2. Configuration example for `type = bluetooth`.
3. Platform notes (Linux native D-Bus, macOS/Windows use serial).
4. Troubleshooting (device not paired, BlueZ not running, channel override).

**tncd-rfcomm**: deprecation notice added. Will be removed in a future release.

## Code Changes

All primary changes in `tncd.py` within `KISSClient`:

1. **`connect()`** — new `elif conn_type == 'bluetooth':` branch handling D-Bus profile
   registration, `ConnectProfile()`, fd wrapping, and transport creation.
2. **Reconnection logic** — `connection_lost()` callback triggers retry loop with backoff.
3. **Cleanup** — unregister D-Bus profile and close socket on `stop()`.

### Files Touched

| File | Change |
|------|--------|
| `tncd.py` | Bluetooth branch in KISSClient.connect(), reconnect logic, cleanup |
| `tncd.ini` | Example bluetooth config (commented out) |
| `README.md` | Bluetooth docs, platform notes, prerequisites |
| `requirements.txt` | Conditional Linux-only deps |
| `tncd-rfcomm` | Deprecation notice |
| `nix/module.nix` | Conditional bluetooth deps |
| `tncd/default.nix` (nix-ham-packages) | Optional bluetoothSupport flag |
| `packaging/PKGBUILD` | Add python-dbus, python-gobject deps |

### Not Touched

- kiss3 library (no modifications)
- Bridge class
- AGWPEServerProtocol
- KISSProtocol

New tests will be added for the bluetooth connection path (mocking D-Bus interactions).
