# Benshi Rig Control — Design

**Date**: 2026-09-22
**Branch**: `feature/benshi-rig-control`
**Status**: design approved, implementation not started

## Purpose

Give tncd an optional, user-facing rig-control module for Benshi-protocol
radios (BTech UV-Pro, RadioOddity GA-5WB, Vero VR-N76 / VR-N7500, BTech
GMRS-Pro), exposed as a hamlib-compatible Net rigctl server so that PAT and
other applications can QSY the radio before connecting.

The immediate motivation is remote testability: with rig control, a bench radio
can be retuned to a clear frequency from a script, which makes OTA work like
`connect-setup-ota-checklist.md` runnable without someone standing at the
radio. But the module is designed as a shipped feature, not a test harness.

## Goals

- QSY (set/get frequency) from any hamlib Net rigctl client, PAT in particular
- Never persist anything to the radio's NVRAM, and never rewrite a stored
  memory channel
- Reuse the Bluetooth connection tncd already holds for KISS, rather than
  contending with it
- Keep the KISS data path untouched, so nothing already validated on the air is
  put at risk

## Non-goals

- **Audio.** The Benshi protocol carries an audio link, and `RfCh` has a
  per-channel `pre_de_emph_bypass` (discriminator bypass) flag. Together those
  would let a Benshi radio act as a Bluetooth soundcard interface for VARA or
  Dire Wolf. That is a genuinely interesting idea and explicitly **out of scope
  for tncd**: none of it touches the radio's internal TNC, which is the only
  thing tncd bridges. It belongs in its own project. Recorded here so it is not
  re-litigated.
- **Replacing the KISS transport.** Benshi can tunnel TNC data itself
  (`HT_SEND_DATA`), but tncd's BLE-KISS path is already OTA-validated and gains
  nothing from the swap.
- **Standalone rig control**, i.e. a Benshi radio configured for rig control
  with no KISS port. Deferred by design (see "Link ownership"), not precluded.
- **Split TX/RX, memory-channel operations, and persistent writes.**

## Background

### There is no hamlib path

Hamlib has no Benshi backend and no BLE transport. A native driver would mean
writing both, upstream. The community went the other way: `benlink` (Python,
Apache-2.0) and HTCommander (Dart/C#) each reimplement the protocol.

Apache-2.0 is one-way compatible with tncd's GPL-3.0, so protocol structures
derived from benlink may be ported with attribution. HTCommander's protocol
documentation is used the same way.

### The contention worry dissolves

The UV-PRO exposes two entirely separate GATT services:

| Service | UUID | Used by |
|---|---|---|
| hessu BLE-KISS | `00000001-ba2a-46c9-ae49-01b0961f68bb` | tncd today (`kiss/ble_linux.go`) |
| Benshi control | `00001100-d102-11e1-9b23-00025b00a5a5` (write `…1101`, indicate `…1102`) | benlink, HTCommander |

GATT multiplexes over a single LE connection, so the process already holding
the link can use both services with no contention. A separate `rigctld` would
need its own connection to the same device — which is exactly where contention
would arise. "Whoever owns the Bluetooth connection owns rig control" is
therefore the correct boundary, not a compromise.

Over Bluetooth Classic, Benshi uses separate RFCOMM channels for command and
audio, with the same GaiaFrame framing as BLE. So one codec serves both
transports.

## Architecture

Four components, following tncd's existing layering:

| Component | Location | Responsibility |
|---|---|---|
| Benshi codec | `benshi/` | GaiaFrame framing + message encode/decode. Pure, no I/O. |
| Control channel | `kiss/` | Expose a byte-duplex for the Benshi command channel from the BLE and SPP transports. |
| Rig | `internal/rig/` | Request/response over the control channel; frequency get/set; caches pushed status. |
| rigctl server | `internal/frontend/rigctl/` | hamlib Net rigctl TCP listener, one per port. |

Exported reusable code lives at the top level (`benshi/`), policy and glue
under `internal/`, matching the existing split.

## Link ownership

Rig control attaches to a configured `[client.N]` Bluetooth port. The control
channel is a child of that port: when the port drops, or the RX-wedge watchdog
relinks it, the control channel closes with it and is re-established on
reconnect.

The control channel is obtained behind an interface that does not care where
the link came from, so supporting standalone rig-control-only radios later is
an additive change rather than a rework.

## Component A: the control channel

The Bluetooth transports gain one optional capability, and nothing else
changes:

```go
// kiss: implemented by transports that can carry a rig-control channel
// alongside KISS data.
type ControlCapable interface {
    ControlChannel() (io.ReadWriteCloser, error)
}
```

Byte-oriented deliberately: `kiss/` knows Bluetooth, `benshi/` knows the
protocol, neither needs the other's details.

**BLE (Linux).** `ble_linux.go` already walks GATT in `findKISSChars`. A
sibling `findBenshiChars` resolves `…1101` (write) and `…1102` (indicate) from
the same tree. No second connection: GATT multiplexes over the LE link that is
already open. Writes go to `…1101`; reads come from `…1102` notifications,
queued like the existing KISS RX path.

**Classic RFCOMM (all platforms).** A second socket to the Benshi command
channel. `buildSSAReq` in `kiss/bluetooth_sdp.go` currently hardcodes the
16-bit SPP UUID; it needs parameterizing for a 128-bit UUID (data element type
`0x1C` rather than `0x19`). v1 ships with an explicit `control_channel = N`
port key and treats SDP discovery as a later refinement — benlink has not
solved auto-discovery either, and `bluetooth_sdp.go` already documents pinning
a channel by hand for this class of case.

**Threading constraint.** A Benshi command is a round-trip to the radio with a
timeout. It must **not** execute on the engine goroutine, which owns all L2
state: blocking it would stall AX.25 on every port for the duration. The engine
is used only to hand out the current control channel safely; the request/response
itself runs in the rigctl server's own goroutine.

## Component B: the Benshi codec

New top-level package, pure and I/O-free.

**Framing.** `GaiaFrame` — flags, payload length, body. Messages carry a 4-byte
header (command group + command id) followed by a typed body.

**v1 command set** — only what is needed, not the ~50-entry enum:

| Command | Id | Use |
|---|---|---|
| `GET_DEV_INFO` | 4 | Identify the radio; verify it speaks the protocol |
| `FREQ_MODE_SET_PAR` | 35 | Enter VFO mode and tune; all-zero payload tears down |
| `FREQ_MODE_GET_STATUS` | 36 | Read current VFO frequency |
| `READ_RF_CH` | 13 | Read the active channel's frequency when not in VFO mode |
| `READ_SETTINGS` | 10 | Read the active channel index; squelch and power (structured for, not wired to hamlib in v1) |
| `GET_HT_STATUS` | 20 | Source for `t` (get_ptt) |

`WRITE_RF_CH`, `WRITE_SETTINGS` and `STORE_SETTINGS` are **not** implemented in
v1. Nothing in the v1 command set writes a channel record or persists to NVRAM.

**Testing.** Golden-byte fixtures as for `ax25/` and `agwpe/`, plus a
`FuzzGaiaFrame` target — radio-sourced bytes are untrusted input, and CLAUDE.md
requires a fuzz target for every such parser.

## Component C: the rig layer (`internal/rig/`) — QSY via VFO mode

These radios have no VFO scratch register in the conventional sense. `RfCh` is
a memory-channel record (`channel_id`, `tx_freq`/`rx_freq`, sub-audio,
`bandwidth`, power flags, `name_str`), and `Settings.channel_a_*` /
`channel_b_*` compose an 8-bit *index* selecting which channel each VFO points
at. Writing "the VFO's channel" would therefore write a real memory-channel
record.

`FREQ_MODE_SET_PAR` avoids that entirely: it puts the radio into frequency
(VFO) mode and tunes explicit frequencies without touching stored channels.
HTCommander uses it for satellite Doppler tracking, sending it about once a
second for a whole pass — a mechanism designed to be driven continuously is by
construction not writing NVRAM, which is a stronger guarantee than any
save-and-restore scheme.

**`FREQ_MODE_SET_PAR` payload, 16 bytes, big-endian:**

```
0..3    RX frequency: top 2 bits = modulation, low 30 bits = Hz
4..7    TX frequency, same encoding
8..9    RX sub-audio (CTCSS/DCS), units of 0.01 Hz, 0 = none
10..11  TX sub-audio, same units
12..13  status/mode flags (settles to 0 once in VFO mode)
14..15  channel step, constant 0x61A8 (25000)
```

An all-zero payload is the documented teardown: it drops the radio out of VFO
mode and restores its normal channel state.

**`FREQ_MODE_GET_STATUS` reply:** `data[4]` is reply status (0 = success),
`data[5..8]` is the frequency in Hz big-endian with the top 2 bits carrying
modulation (mask with `0x3FFFFFFF`).

**Notification 14** (`freqModeStatusChanged`) is pushed by the radio whenever
the tuned frequency changes in VFO mode: `data[5..8]` RX frequency,
`data[9..12]` TX, `data[13..16]` sub-audio, `data[17..18]` status flags. The
**low** flags byte (`data[18]`) is the authoritative in-VFO-mode indicator —
non-zero while in frequency mode, 0 on a preset channel. The high byte is
unreliable and can stay set on exit.

**Resulting behavior:**

- `\set_freq <hz>` → `FREQ_MODE_SET_PAR` with rx = tx = hz, FM modulation, no
  sub-audio, flags 0, step `0x61A8`. Repeat calls retune.
- `\get_freq` → served from the notification-14 cache; `FREQ_MODE_GET_STATUS`
  as fallback. When not in VFO mode, read the active channel's `rx_freq` via
  `READ_SETTINGS` + `READ_RF_CH` — read-only, still no writes.
- Release/shutdown → all-zero teardown, restoring the radio's own state.

v1 is simplex only (rx = tx), which is what Winlink RMS gateways need.

## Component D: the rigctl server

`internal/frontend/rigctl/`, a sibling of `agwpe`, `kisstcp` and `api`,
following their shape — including `internal/netutil` for `allowed_subnets`, so
all four listeners enforce the same client-IP allowlist at accept time.

One listener per port (`[rigctl.0]` → 4532, `[rigctl.1]` → 4533). hamlib's Net
rigctl protocol has no way to select among multiple rigs on one socket, so a
listener per radio is the only genuinely compatible arrangement.

**v1 command surface** — exactly what PAT's client
(`wl2k-go/rigcontrol/hamlib/rigctld.go`) sends, plus `q`:

| Command | Response |
|---|---|
| `dump_caps` | non-error; PAT uses it only as a ping |
| `\chk_vfo` | `CHKVFO 0` |
| `\get_freq` | bare integer Hz |
| `\set_freq <hz>` | `RPRT 0` |
| `t` | `0` or `1`, from `GET_HT_STATUS`; `RPRT -4` if the spike shows it does not expose TX state |
| `\set_ptt <n>` | `RPRT -4` (not implemented) |
| `q` | close the connection |

Answering `CHKVFO 0` is deliberate: PAT then uses an empty VFO prefix and never
prepends VFO arguments to any command, removing a class of parsing complexity
from v1.

`\set_ptt` is not a real gap — PAT drives PTT only for soundcard modes; with a
KISS TNC the radio keys itself. If the spike turns up a Benshi TX command it
can be added.

**Error codes**, pinned against hamlib `include/hamlib/rig.h`:

| Condition | Reply | hamlib code |
|---|---|---|
| Success | `RPRT 0` | `RIG_OK` = 0 |
| Bad or out-of-range argument | `RPRT -1` | `RIG_EINVAL` = 1 |
| Command not implemented | `RPRT -4` | `RIG_ENIMPL` = 4 |
| Radio did not reply in time | `RPRT -5` | `RIG_ETIMEOUT` = 5 |
| Port offline or relinking | `RPRT -6` | `RIG_EIO` = 6 |
| Malformed reply from radio | `RPRT -8` | `RIG_EPROTO` = 8 |
| Radio replied with a failure status | `RPRT -9` | `RIG_ERJCTED` = 9 |

**Concurrency.** One goroutine per client connection. Rig requests are
serialized by a per-port mutex and executed off the engine loop with a timeout.
A port that is offline or mid-relink answers an error immediately and never
blocks — relevant given the relink budget introduced in
`fix/connect-setup-timing`: a wedged or flapping port must not turn into a hung
rigctl client.

## Configuration

```ini
[rigctl.0]
enabled = true
listen_host = 127.0.0.1
listen_port = 4532
allowed_subnets = 127.0.0.1/32
```

Plus one new key on the port itself, for the classic-Bluetooth case:

```ini
[client.0]
type = bluetooth
bdaddr = AA:BB:CC:DD:EE:FF
control_channel = 2
```

Defaults: `enabled = false` (the module is opt-in), `listen_host = 127.0.0.1`,
and `listen_port` = `4532 + N` for `[rigctl.N]`, so a single-radio setup gets
the conventional 4532 without configuring anything. Unknown keys warn via the
existing `warnUnknownKeys` mechanism.

**Platform reach.** Linux gets both BLE and classic; Windows gets classic via
the existing `AF_BTH` code; other platforms stub out as the Bluetooth
transports already do.

## Risks and spikes

A short read-only spike runs before anything is built. In priority order:

1. **Does the internal KISS TNC still pass packet while the radio is in VFO
   mode?** If the TNC operates only on stored channels, VFO mode and packet are
   mutually exclusive on this radio and the whole approach needs revisiting.
   This is the one that can sink the design, so it is proven first.
2. **Is `FREQ_MODE_SET_PAR` supported on the firmware on the bench radio?**
   HTCommander targets this radio family, so this is expected to pass.
3. **Does the all-zero teardown restore prior state cleanly?**

Secondary risks:

- **The codec is the bulk of the work**, ported from a Python/Dart reference at
  bitfield level, and is the part most likely to be quietly wrong. Mitigated by
  scoping v1 to six commands and capturing golden bytes from a real exchange
  with the bench radio.
- **Reverse-engineered protocol.** Mitigated by the v1 command set containing
  no write-to-channel or persist command at all.

## Testing

- `benshi/` — golden-byte fixtures like `ax25/`/`agwpe/`, plus `FuzzGaiaFrame`
- `internal/rig/` — request/response, timeout and teardown behavior against a
  fake control channel
- `internal/frontend/rigctl/` — table-driven tests against an in-process fake
  rig, asserting the **exact wire responses** PAT's client parses: `CHKVFO 0`,
  bare-integer `\get_freq`, `RPRT` codes
- `tncd rig` subcommand (get/set frequency against a configured port) — makes
  the spike runnable and gives a one-liner for remote testing, which was the
  original motivation
- OTA — extend `connect-setup-ota-checklist.md` or add a companion checklist
  covering a QSY round-trip from PAT and, critically, "packet still works in
  VFO mode"

## Staging

1. `benshi/` codec (framing + the six v1 commands) — pure, fully unit-testable
2. Control channel on BLE/Linux + `internal/rig/` + `tncd rig` subcommand —
   enough to run the spike
3. `internal/frontend/rigctl/` + config wiring
4. Classic RFCOMM control channel (Windows reach) + SDP UUID parameterization

## References

- benlink — <https://github.com/khusmann/benlink> (Apache-2.0): GATT UUIDs,
  GaiaFrame framing, `Settings` and `RfCh` bitfield layouts
- HTCommander — <https://github.com/Ylianst/HTCommander>: `FREQ_MODE_SET_PAR`
  payload, `FREQ_MODE_GET_STATUS` reply, notification 14 semantics
- PAT's rigctld client — `wl2k-go/rigcontrol/hamlib/rigctld.go`: the exact
  command surface v1 must serve
- hamlib `include/hamlib/rig.h`: `rig_errcode_e` values
