# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

tncd is an AGWPE-to-KISS bridge for amateur (ham) radio packet. It allows AGWPE-compatible client applications (PAT/Winlink, Paracon, Xastir) to communicate with KISS TNCs (Terminal Node Controllers) over serial, TCP, or Bluetooth. It implements AX.25 layer 2 connected mode (SABM/UA handshake, I-frame sequencing, RR acknowledgement, duplicate detection, DISC handling, and AX.25 v2.2 SABME/mod-128/XID) since KISS TNCs are dumb modems that don't provide this.

`main` is the **2.0 line**: a pure-Go rewrite, a single static binary (`CGO_ENABLED=0`), cross-compiled for Linux, Windows, macOS, and FreeBSD. The older **1.3.x Python** implementation lives on the [`v1` branch](https://github.com/ben-kuhn/tncd/tree/v1) and is maintenance-only.

## Before Starting Work

Always pull the latest changes before starting any work:

```bash
git pull
```

Always run `go test ./...` and ensure all tests pass before committing or deploying.

Never commit secrets (API keys, tokens, passwords, private keys) to git.

## Debugging: prove it's external before blaming (or clearing) the app

tncd sits between an AGWPE client and a long external chain: the KISS transport
(serial/TCP/**Bluetooth adapter**), the TNC/radio, its PTT/deviation/half-duplex
timing, the RF path, and the remote station. Most failures that *look* like tncd
bugs — stalls, "goes deaf", frames that never make it on the air, sessions that
die mid-transfer — originate somewhere in that chain, not in the Go code. The
reverse trap is just as costly: assuming "it's the radio" without proof.

**Before asserting a cause — app or external — isolate it with a controlled swap
that changes exactly one link in the chain.** Concrete, high-signal moves:

- **Swap the modem, keep tncd's L2.** Point tncd at Dire Wolf's KISS TCP port
  (`type = tcp`, port 8001) so a *known-good* radio drives the same L2 code. If a
  failure that happened over one radio (e.g. a Bluetooth TNC) vanishes here, the
  bug is in the radio/transport path, not tncd's AX.25 engine — and vice versa.
- **Bypass tncd entirely.** A tiny probe that opens the transport and writes one
  KISS frame (does the radio key? does the frame reach the air?) proves whether
  the transport/radio works without any tncd logic in the picture.
- **Compare against a reference stack.** Dire Wolf is a correct AX.25
  implementation and its source is available; run the same connection through it
  (`~/direwolf-vhf.conf`, AGW port 8000) and diff its on-air behavior against
  tncd's. Same inputs, different implementation → the divergence is the bug.
- **Watch the actual RF, not just counters.** An independent Dire Wolf monitor on
  the frequency shows what truly went on the air. tncd's `tx` counter means "we
  handed it to the transport," *not* "it was transmitted" — a wedged Bluetooth
  socket accepts writes and drops them.
- **Change one variable at a time.** Same binary + different radio, or same radio
  + different binary (e.g. an older tagged build via `git worktree`). Don't
  reason from a run where two things changed at once.

Only after a swap has pinned the failure to a single link should you conclude
where it lives. Guessing — in either direction — wastes hours.

## Environment

This is a NixOS system building a **pure-Go, no-cgo** binary. Use the Go toolchain from `go.mod` (Go 1.24+); `nix-shell -p go` provides it if it isn't on `PATH`. Always build/test with `CGO_ENABLED=0` (there is no C compiler in the default shell, and every release target is pure-Go).

## Commands

```bash
# Build
CGO_ENABLED=0 go build -o tncd ./cmd/tncd

# Run
./tncd -c tncd.ini

# Unit tests (fast, no hardware)
CGO_ENABLED=0 go test ./...

# A single package / test
CGO_ENABLED=0 go test ./ax25/l2/ -run TestName -v

# Cross-compile (any GOOS/GOARCH)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o tncd.exe ./cmd/tncd

# End-to-end tests (black-box against the compiled binary; needs Dire Wolf/PAT)
pip install -r e2e/requirements-test.txt
pytest -c e2e/pytest.ini e2e/
```

Useful subcommands: `tncd version`, `tncd genconfig`, `tncd check -c FILE`, `tncd ports [--json]`, and (Windows) `tncd service install|uninstall|start|stop` / `tncd install|uninstall`.

## Architecture

Go module `github.com/ben-kuhn/tncd/v2`. Exported reusable packages at the top level; policy/glue under `internal/`.

- **`ax25/`** — AX.25 frame parse/build, addresses, control fields (mod-8 + mod-128), XID. `ax25/l2/` is the connected-mode engine: per-connection state machines, T1/T2/T3 timers, the 3-second duplicate-RR guard, backwards-N(R) protection, and I-frame coalescing.
- **`kiss/`** — KISS framing/escaping plus transports behind one `Transport` interface: serial (`go.bug.st/serial`, with `usb:` resolution and a modem-status disconnect probe), TCP, and Bluetooth SPP (`bluetooth_linux.go` BlueZ/D-Bus; `bluetooth_windows.go` Winsock `AF_BTH`; `bluetooth_stub.go` elsewhere). Handles `init_string`/`exit_string` TNC lifecycle.
- **`agwpe/`** — AGWPE 36-byte header + frame encode/decode.
- **`internal/engine/`** — a single serialized event loop (one goroutine owns all L2/bridge state; everything else messages it via `Do`/`After`). This mirrors the asyncio serialization the half-duplex fixes depend on.
- **`internal/bridge/`** — coordinator: connections table, dispatch by AX.25 frame type, TX-echo suppression, transport construction (`buildTransport`), and per-port auto-reconnect with backoff.
- **`internal/frontend/{agwpe,kisstcp,api}/`** — the AGWPE TCP server, the KISS-over-TCP passthrough, and the read-only JSON/SSE monitoring API.
- **`internal/netutil/`** — client-IP allowlist (`allowed_subnets`) shared by all three listeners, enforced by a filtering `net.Listener` at accept time.
- **`internal/app/`** — `Runtime` (wires engine + bridge + frontends; `New`/`Wait`/`Shutdown`), shared by the console and Windows-service launch paths.
- **`internal/config/`** — INI load, validation, and `genconfig` example.
- **`internal/ports/`** — serial device enumeration + `usb:VID:PID` resolution; paired Bluetooth enumeration on Windows.
- **`internal/version/`** — version string (set via `-ldflags -X` at build/release).
- **`cmd/tncd/`** — flags, config load, subcommand dispatch, signal/SCM lifecycle. Windows-only files (`//go:build windows`) add the service handler, Event-Log logging, the self-installer GUI (`lxn/walk`), and `service`/`install` subcommands; `_unix.go` counterparts stub these off Windows.

Data flow: `AGWPE client → TCP → frontend/agwpe → engine/bridge → kiss.Transport → TNC` and reverse, all serialized through the engine goroutine.

## Key Dependencies

- `go.bug.st/serial` — serial port I/O + enumeration (pinned; `kiss/rtscts_linux.go` reflects into its unexported `handle` field — re-test RTSCTS before upgrading).
- `github.com/godbus/dbus/v5` — Linux Bluetooth SPP via BlueZ (Linux only).
- `golang.org/x/sys/windows` — Windows service (`svc`/`mgr`/`eventlog`), registry, and Winsock `AF_BTH` (Windows only).
- `github.com/lxn/walk` (+ `lxn/win`) — Windows installer GUI (Windows only, behind build tags).
- `gopkg.in/ini.v1` — INI config parsing.

`pyham-pe` was the AGWPE reference during the port; codec correctness is now frozen as golden-byte fixtures in the Go tests.

## Testing

- **Go unit tests** (`go test ./...`): golden-byte codec tests for `ax25/` and `agwpe/` (byte-for-byte against captured frames), and behavioral tests for `ax25/l2` with each hard-won regression fix as a named test. Fuzz targets (`Fuzz*`) cover every untrusted-byte parser (AX.25 frame/XID/address, AGWPE header, KISS decoder, SDP) — plain `go test` runs their seed/regression corpora; real fuzzing is `go test -fuzz=FuzzName -fuzztime=Ns ./pkg/`.
- **e2e** (`e2e/`, pytest): a black-box harness that drives the compiled binary against Dire Wolf/PAT (packet-browser pattern). Binary discovery: `$TNCD_BIN` → `tncd` on `PATH`/repo-root → auto-built from `./cmd/tncd`. Needs local PipeWire/Dire Wolf/PAT; not run in CI.
- **CI** (`.github/workflows/test.yml`) runs `go test`, `go vet`, the full cross-compile matrix, 10s fuzz bursts per parser target, and `govulncheck` on every PR.

## Packaging

- **nfpm** cross-compiles static Go binaries into `.deb`/`.rpm` for linux amd64/386/armhf/arm64/riscv64.
- **Windows/macOS/FreeBSD**: `tncd.exe` (with an embedded manifest + version resource, `cmd/tncd/resource_windows_*.syso`, regenerated via `go generate ./cmd/tncd`) shipped as zips; macOS/FreeBSD as tarballs. On Windows, `tncd.exe` is also its own self-installer GUI + service.
- **Nix**: `nix/default.nix` (`buildGoModule`), `nix/module.nix` (`services.tncd.*`); distributed via the `nix-ham-packages` overlay.
- **Arch**: `packaging/PKGBUILD` (Go build).
- **Release workflow**: `.github/workflows/release.yml` triggers on `v*` tags — cross-compiles all targets, packages them, and creates a GitHub Release. During the 2.0 beta the public APT/RPM/AUR repos + tncd.dev stay on the stable 1.3.x line (the `publish-repos` job is gated); they move to the Go line at v2.0.0.

## Protocol Notes

AGWPE frames use a fixed 36-byte header (little-endian) with 10-byte null-padded callsign fields. AGWPE protocol compatibility is verified by golden-byte fixtures (originally captured against `pyham-pe`), e.g. exact payload sizes for `R`/`g` frames.

## Release / Deployment Checklist

When cutting a release, follow this order:

1. **Run unit tests**: `CGO_ENABLED=0 go test ./...` — all must pass
2. **Run e2e tests**: `pytest -c e2e/pytest.ini e2e/` — validates serial/PTY/TCP regressions
3. **OTA test** (if hardware changes): connect PAT via tncd to a real TNC and complete a Winlink CMS round-trip; for a v2.0.0 tag, revalidate the full hardware matrix (serial TNCs + Bluetooth)
4. **Commit** all changes on the feature branch
5. **Merge to main**: `git checkout main && git merge --no-ff feature/branch-name`
6. **Bump version** in `packaging/PKGBUILD` and `nix/default.nix`, commit
7. **Tag**: `git tag -a vX.Y-BETA -m "description"`
8. **Push**: `git push origin main --tags` (rebase if remote is ahead, re-tag after rebase)
9. **Update nix-ham-packages** (`/home/ku0hn/dev/nix-ham-packages/tncd/`):
   - Update `version` in `default.nix`
   - Get new src hash: `nix-prefetch-url --unpack "https://github.com/ben-kuhn/tncd/archive/vX.Y-BETA.tar.gz"` then `nix hash convert --hash-algo sha256 --to sri <hash>`
   - Recompute `vendorHash` if `go.mod`/`go.sum` changed (set to a fake hash, build, read the expected hash from the error)
   - Update `module.nix` if module options changed
   - Build to verify, then commit and push
10. **Update website** (`website/`): changelog, docs, index as needed — commit and push
11. **Update docs**: README.md, nix/README.md, tncd.service as needed

The `v*` tag triggers `.github/workflows/release.yml` (packages + GitHub Release), and a `website/**` push triggers `.github/workflows/website.yml` (deploys tncd.dev via the gated `tncd_cloudflare` environment).
