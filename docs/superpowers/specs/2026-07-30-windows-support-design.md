# tncd on Windows — Service, Self-Installer, and Direct Bluetooth

**Date:** 2026-07-30
**Branch:** feature branch off `main` (Go 2.0 line)
**Status:** Approved design; phase-4 deliverable for the 2.0 release.

## Context

`tncd.exe` already cross-compiles clean from the Linux CI (`GOOS=windows go
build ./...` succeeds today) and runs correctly in console/foreground mode:
serial works through `go.bug.st/serial` on `COM*` ports, and the BlueZ D-Bus
Bluetooth code is correctly excluded by `//go:build linux` (`kiss/bluetooth_stub.go`
covers `!linux`). `golang.org/x/sys v0.43.0` is already a direct dependency, so
`x/sys/windows/svc`, `.../svc/mgr`, `.../svc/eventlog`, and the raw Winsock
surface are all available with no new module requirements.

What is missing for a first-class Windows experience:

1. **Service integration** — running under the Service Control Manager (SCM),
   not just a console.
2. **A friction-free install** — Windows users expect to double-click, answer
   a couple of questions, and be done; not copy a binary, hand-write an INI,
   and register a service by hand.
3. **First-class Bluetooth** — parity with the Linux in-process SPP path, so a
   config just names the TNC and tncd connects to it.
4. **Surviving COM-port renumbering** — Windows reassigns `COMx` when a USB
   adapter moves to a different physical port; a hard-coded `COM3` is not a
   stable identity.

## Goals

- Single self-contained artifact: **`tncd.exe`** is the app, the CLI, the
  Windows service, and its own GUI installer. No external installer toolchain
  (no NSIS/WiX/Inno/Wine) on the release runners.
- Run as a proper Windows service via `x/sys/windows/svc`.
- GRC-style **self-installer**: double-click an un-installed `tncd.exe` and a
  small GUI wizard copies it into place, collects the essentials, writes a
  working config, and registers + starts the service.
- **Direct Bluetooth** via Winsock RFCOMM (`AF_BTH`), matching the Linux UX:
  `device = bt:Mobilinkd-TNC4`, in-app connect with auto-reconnect, no virtual
  COM port.
- **Stable device identity** so a working config survives COM renumbering.

## Non-goals

- MSI / Group-Policy / SCCM enterprise deployment. Ham use, not enterprise.
- macOS/BSD service integration (launchd/rc). Those stay tarball + manual for
  2.0; the `Runtime` refactor here makes adding them later cheap.
- Bluetooth *pairing* inside tncd on Windows. As on Linux, the device is paired
  once through the OS (Windows Settings); tncd only connects to a paired device.
- Config-format changes. The Windows config is the same INI as every other
  platform, with the same keys.

## Decisions (settled during brainstorming)

| Topic | Decision |
|---|---|
| Artifact | One binary, `tncd.exe`. No separate installer file. |
| Service API | `x/sys/windows/svc` directly (per the 2.0 go-port design), not `kardianos/service`. |
| Install UX | GUI wizard that collects the essentials, writes a working config, registers + **starts** the service. |
| GUI toolkit | `github.com/lxn/walk` (pure-syscall Win32 wrapper, no cgo), Windows-only behind build tags. |
| Elevation | Collect input un-elevated; self-elevate via `ShellExecute("runas")` for the privileged install step (one UAC prompt). |
| Serial identity | USB `VID:PID(:serial)`; resolved to the live `COMx` at startup. Raw `COMx` still accepted. |
| Bluetooth | Direct Winsock `AF_BTH` / `BTHPROTO_RFCOMM`; `device = bt:<name>` or `bt:<MAC>`. |
| Enumeration | `tncd ports [--json]` lists serial ports (with VID:PID) and paired BT SPP devices (name+MAC). The wizard reads it. |
| Config path | `%ProgramData%\tncd\tncd.ini`; binary at `%ProgramFiles%\tncd\tncd.exe`. |
| Service start type | Automatic (Delayed Start), so the network/Bluetooth stacks are up first. |

## Architecture — one binary, mode dispatch

`tncd.exe` chooses its mode at startup:

| Condition (checked in order) | Mode |
|---|---|
| `svc.IsWindowsService()` is true | **Service**: run the SCM handler → `Runtime`. Logs go to the Windows Event Log. |
| A subcommand/flags present (`-c`, `service …`, `ports`, `check`, `version`, `genconfig`, `--uninstall`) | **CLI/foreground**, as today. |
| Interactive (double-click), **not installed** | **GUI installer** wizard. |
| Interactive, **already installed** | **GUI manage** window (Start/Stop, Uninstall, Open Config, Open Web Monitor). |

"Installed" = the running image path is under `%ProgramFiles%\tncd\` **or** the
service is registered. This detection is cheap and needs no persisted state.

All four modes share one platform-neutral core, so there is a single source of
truth for "run the bridge" and "shut it down cleanly."

### Component 1 — `Runtime` refactor (platform-neutral)

Extract the middle of `cmd/tncd/main.go` (build engine/bridge, start AGWPE +
optional kisstcp/api servers, and the existing 4-step shutdown block at
`main.go:282-309`) into a small reusable type in a new package
`internal/app`:

```go
type Runtime struct { /* eng, bridge, listeners */ }
func New(cfg *config.Config, v, t int) (*Runtime, error) // start everything
func (r *Runtime) Wait()      // eng.Run(); blocks until Shutdown
func (r *Runtime) Shutdown()  // the existing eng.Do() 4-step teardown
```

The console path (`main_unix.go` / interactive Windows) wires SIGINT/`os.Interrupt`
to `Shutdown()`. The service handler calls the same three methods. This is the
only change to existing runtime code; it is fully testable on Linux before any
Windows-specific file exists, and it is the foundation everything else builds on.

### Component 2 — Windows service (`x/sys/windows/svc`)

New Windows-only files (`//go:build windows`) in `internal/winsvc`:

- **`svc.Handler.Execute`** — report `StartPending` → `app.New` → `Running`
  (accepting Stop+Shutdown) → loop on the request channel; on `Stop`/`Shutdown`
  → `StopPending` → `Runtime.Shutdown()` → return. Handle `Interrogate`.
- **Event-log logging** — a small `slog.Handler` that writes to
  `eventlog.Log` (Info/Warning/Error) when running under the SCM, since there is
  no stderr. Console/interactive modes keep the existing stderr text handler.
- **Service-control subcommands** — `tncd service install|uninstall|start|stop`
  via `svc/mgr`. `install` records an **absolute** `-c` path (a service's working
  directory is `C:\Windows\System32`, so a relative path silently breaks — the
  primary gotcha), sets Automatic-Delayed start, a description, and registers the
  event-log source. These subcommands are what the installer calls internally, so
  SCM logic lives in exactly one place.

`main_unix.go` retains today's SIGINT/SIGTERM handling unchanged.

### Component 3 — port enumeration + stable device identity

New package `internal/ports` with a platform-neutral interface and per-OS
backends:

```go
type Port struct {
    Ref   string // stable reference to write into config
    Label string // human label for the wizard
    Kind  string // "serial" | "bluetooth"
    // serial: VID, PID, Serial, COM; bluetooth: Name, Addr
}
func List() ([]Port, error)              // enumerate
func Resolve(ref string) (target string, err error) // ref -> live COMx / bt target
```

- **Serial** (all platforms): `go.bug.st/serial/enumerator.GetDetailedPortsList()`
  gives Name, IsUSB, VID, PID, SerialNumber. A USB port's stable `Ref` is
  `usb:VID:PID[:serial]`; `Resolve` enumerates and returns the current `COMx`
  whose identity matches. Serial number disambiguates two identical dongles.
  If a bare `COMx` (or `/dev/tty…`) is given, it is passed through unchanged.
- **Bluetooth** (Windows): paired SPP devices enumerated via `WSALookupService`
  in the `NS_BTH` namespace (or `BluetoothFindFirstDevice`), yielding name +
  MAC. `Ref` is `bt:<name>` (or `bt:<MAC>`); `Resolve` returns the MAC to hand to
  the Bluetooth transport.

`tncd ports [--json]` is a thin subcommand over `List()`. The wizard shells out
to it; the runtime uses `Resolve()` in the serial transport at startup and logs
`resolved usb:0403:6001:A50285BI -> COM7`. On a resolve miss, the serial
transport uses the existing reconnect/backoff (the TNC may simply be unplugged).

### Component 4 — Windows Bluetooth (Winsock RFCOMM)

New `kiss/bluetooth_windows.go` (`//go:build windows`) implementing the same
KISS transport interface as `bluetooth_linux.go`, so it slots behind the bridge
with no bridge changes. Pure Go via `x/sys/windows` + a few hand-rolled `ws2_32`
syscalls; no cgo.

- **Connect**: `WSAStartup`; `socket(AF_BTH, SOCK_STREAM, BTHPROTO_RFCOMM)`;
  `connect()` with a `SOCKADDR_BTH{ addressFamily, btAddr, serviceClassId, port }`
  where `serviceClassId` = the Serial Port Profile UUID
  `{00001101-0000-1000-8000-00805F9B34FB}` and `port = 0`. Passing the service
  GUID makes Windows perform the SDP channel lookup — the free equivalent of the
  Linux "auto-detect RFCOMM channel via SDP." `x/sys/windows` has no
  `SOCKADDR_BTH` helper, so we define the struct and drive
  `connect`/`recv`/`send`/`closesocket` via raw syscalls (~120–150 lines).
- **I/O**: a blocking-socket `io.ReadWriteCloser` driven by the existing
  one-reader / one-writer goroutine model — blocking `recv` in the reader
  goroutine is exactly the pattern the other transports already use.
- **Name → MAC**: via the same `internal/ports` Bluetooth backend.
- **No pairing**; assume the device is paired (parity with Linux). Reconnect
  with backoff reuses the shared transport reconnect logic.

Config accepts `type = bluetooth` with `device = bt:<name|MAC>` on Windows, the
same shape as Linux, so a documented config example reads identically across
platforms.

### Component 5 — self-installer + manage GUI (`walk`)

New Windows-only package `internal/wingui` (`//go:build windows`), using
`github.com/lxn/walk`. The dependency is Windows-only (behind build tags) and
never enters the Linux/macOS builds.

**Installer wizard** (interactive, not installed):

1. Welcome / what-this-does.
2. **Essentials** page — callsign; a **port dropdown** populated from
   `tncd ports` (each entry labeled `USB: FT232R (COM3)` or
   `Bluetooth: Mobilinkd-TNC4`); baud + TNC type with sane defaults.
3. **Install** action → **self-elevate** via `ShellExecute(verb="runas")`; the
   elevated instance:
   - copies the running image to `%ProgramFiles%\tncd\tncd.exe`,
   - writes `%ProgramData%\tncd\tncd.ini` (fully-qualified stable `device`
     ref, with a human comment noting which physical device it was),
   - calls the internal `service install` + `service start`,
   - writes an Add/Remove Programs uninstall registry entry
     (`…\Uninstall\tncd` → `tncd.exe --uninstall`).
4. **Finish** page — service state, and **the config file path** in plain text
   for future edits, plus a note: *"Bluetooth TNC? Pair it once in Windows
   Settings, then re-run and pick it from the list,"* and the web-monitor URL if
   the API is enabled.

Input is collected **before** elevation so the UAC prompt only appears when the
user commits.

**Manage window** (interactive, already installed): service status + buttons —
Start/Stop (self-elevate as needed), Open Config (launches the INI in the
default editor), Open Web Monitor (opens the API URL), Uninstall
(`--uninstall`). Minimal, GRC-flavored.

**Uninstall** (`tncd --uninstall`, elevated): `service stop` + `service
uninstall`, remove the event-log source and the Add/Remove entry, delete the
Program Files copy; **prompt whether to keep `tncd.ini`** (default keep).

## Config & paths on Windows

- Default config path when none is given: `%ProgramData%\tncd\tncd.ini`
  (`config.Load("")` gains this Windows default; other platforms keep their
  current behavior).
- Install locations: binary `%ProgramFiles%\tncd\tncd.exe`, config
  `%ProgramData%\tncd\tncd.ini`.
- `genconfig` output is what the wizard starts from; the wizard fills in
  callsign/device rather than shipping a purely commented template.

## Packaging & release

- Add `GOOS=windows GOARCH=amd64` and `GOARCH=arm64` targets to the existing
  cross-compile matrix in `.github/workflows/release.yml`, producing
  `tncd.exe`. No nfpm step for Windows (nfpm is deb/rpm only) and **no installer
  toolchain** — the exe is the installer.
- Distribute `tncd.exe` directly on the GitHub Release (optionally inside a
  small `.zip` alongside `README`/`LICENSE`). SHA256 sums as for other artifacts.
- Embed a Windows application manifest and version resource (via
  `go:embed`/`x/sys` or a `.syso`) requesting `asInvoker` (self-elevation is
  on-demand, so the base process stays un-elevated) and declaring the app name
  for UAC prompts.

## Testing & OTA gate

- **Linux-testable now** (no Windows needed): the `internal/app` `Runtime`
  refactor (start/shutdown parity, existing e2e still green against the Go
  binary), and the platform-neutral half of `internal/ports` (serial
  identity parsing + `usb:` ref round-tripping against a faked enumerator).
- **Requires a real Windows box/VM** (the expensive, un-fakeable part):
  - SCM install/start/stop/uninstall and Event-Log entries.
  - Console-mode Ctrl+C shutdown.
  - The self-installer wizard end-to-end, UAC elevation, Add/Remove entry.
  - A serial TNC over a real `COMx`, including moving it to a different USB
    port and confirming `usb:VID:PID` still resolves.
  - **Bluetooth over Winsock RFCOMM to a real Mobilinkd TNC3/TNC4** — the
    highest-risk code (raw `SOCKADDR_BTH` layout) and the one path that cannot
    be exercised on Linux at all.
- This Windows hardware pass joins the existing v2.0.0 pre-tag hardware matrix
  (serial TNCs + Linux Bluetooth). Windows Bluetooth is a hard gate for tagging.

## Implementation order (for the plan)

1. `internal/app` `Runtime` refactor — land first, verify on Linux, e2e green.
2. `internal/ports` (serial backend + `tncd ports`) — Linux-testable.
3. Windows service (`internal/winsvc`) + Event-Log logging + `service …`
   subcommands.
4. Windows Bluetooth transport (`kiss/bluetooth_windows.go`) + BT enumeration
   backend.
5. Self-installer + manage GUI (`internal/wingui`, `walk`) + elevation +
   uninstall + Add/Remove entry.
6. Release-matrix + manifest/version resource; docs/website.
7. Windows hardware OTA validation (serial + Bluetooth).

Each step is independently reviewable; 1–2 are safe to merge before any
Windows-specific code exists.

## Risks

- **Raw `SOCKADDR_BTH`/Winsock layout** is the classic place for silent,
  hard-to-debug bugs; budget real Windows+hardware time, not just a code review.
- **`lxn/walk` maintenance** — it is stable but not very active. Mitigated by
  keeping the GUI thin (three wizard pages + one manage window) and Windows-only,
  so it never affects the core or other platforms.
- **UAC/elevation UX** across Windows versions (10/11, Home vs Pro) needs
  spot-checking on real installs.
