# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

tncd is an AGWPE-to-KISS bridge for amateur (ham) radio packet. It allows AGWPE-compatible client applications (PAT/Winlink, Paracon, Xastir) to communicate with KISS TNCs (Terminal Node Controllers) over serial or TCP. It implements AX.25 layer 2 connected mode (SABM/UA handshake, I-frame sequencing, RR acknowledgement, duplicate detection, DISC handling) since KISS TNCs are dumb modems that don't provide this.

## Before Starting Work

Always pull the latest changes before starting any work:

```bash
git pull
```

Always run `pytest` and ensure all tests pass before committing or deploying.

Never commit secrets (API keys, tokens, passwords, private keys) to git.

## Environment

This is a NixOS system. **Never use bare `pip install`** — always use a virtualenv. A Nix dev shell is available:

```bash
# Preferred: use the Nix shell (provides Python + dependencies)
nix-shell nix/shell.nix

# Or manually with a venv:
python -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
pip install -r requirements-test.txt
```

## Commands

All commands assume you are in a nix-shell or an activated venv.

```bash
# Run tests
pytest

# Run a single test
pytest tests/test_tncd.py::TestClassName::test_method_name

# Run the bridge
python tncd.py -c tncd.ini
```

## Architecture

The entire bridge is a single Python module (`tncd.py`) with three main classes:

- **`Bridge`** — Central coordinator. Manages AGWPE clients, KISS connection, and AX.25 connection state. Dispatches received KISS frames by AX.25 frame type (`_dispatch_ui`, `_dispatch_i`, `_dispatch_sabm`, `_dispatch_ua`, `_dispatch_dm`, `_dispatch_disc`, `_dispatch_s`). Tracks active connections in `self.connections` dict keyed by `(port, local_call, remote_call)`. Suppresses TX echoes via `_sent_frames` deque.

- **`AGWPEServerProtocol`** — asyncio.Protocol handling one TCP client. Parses the 36-byte AGWPE header (`AGWPE_HEADER_FORMAT`), reassembles partial frames in a buffer, and dispatches by frame kind byte in `handle_frame()`. Each AGWPE frame type (R, G, g, X, M, V, C, D, d, K, y, Y, etc.) is handled inline.

- **`KISSClient`** — Wraps the `kiss3` library. Connects via serial (`kiss.SerialKISS`) or TCP (`kiss.TCPKISS`). Runs a blocking read loop in a daemon thread (`kiss-rx`), dispatching received frames to Bridge via `loop.call_soon_threadsafe`.

Data flow: `AGWPE Client → TCP → AGWPEServerProtocol → Bridge → KISSClient → TNC` and reverse.

## Key Dependencies

- `kiss3` — KISS protocol framing (serial/TCP)
- `pyham-ax25` (imported as `ax25`) — AX.25 frame construction/parsing, Address, Control, FrameType
- `pyham-pe` — AGWPE protocol engine (test dependency only, used as reference for protocol compatibility)

## Testing

Tests use pytest with pytest-asyncio (`asyncio_mode = auto`). Tests mock the Bridge or KISSClient to test protocol handling in isolation. The `make_frame()`/`parse_frame()` helpers construct and parse raw AGWPE binary frames. `make_protocol()` returns a mock-backed protocol; `make_real_protocol()` uses a real Bridge with mocked KISS for connected-mode tests.

## Packaging

- **Nix**: `nix/overlay.nix` (package), `nix/module.nix` (NixOS service module with `services.tncd.*` options)
- **Arch**: `packaging/PKGBUILD`
- **Release workflow**: `.github/workflows/release.yml` — triggers on `v*` tags. Builds self-contained .deb and .rpm packages (with vendored Python deps via fpm) for amd64, i386, armhf, arm64, riscv64 using QEMU cross-compilation in Docker. Publishes signed APT and RPM repos plus a static website to Cloudflare Pages (tncd.dev), updates PKGBUILD for AUR, and creates a GitHub Release with all artifacts.

## Protocol Notes

AGWPE frames use a fixed 36-byte header (little-endian) with 10-byte null-padded callsign fields. The `pyham-pe` library is the reference implementation for AGWPE protocol compatibility — tests verify that responses match what pe expects (e.g., exact payload sizes for R, g frames).

## Release / Deployment Checklist

When cutting a release, follow this order:

1. **Run unit tests**: `pytest` — all must pass
2. **Run e2e tests**: `pytest tests/test_e2e.py` — validates serial/PTY/TCP regressions
3. **OTA test** (if hardware changes): connect PAT via tncd to a real TNC and complete a Winlink CMS round-trip
4. **Commit** all changes on the feature branch
5. **Merge to main**: `git checkout main && git merge --no-ff feature/branch-name`
6. **Bump version** in `packaging/PKGBUILD` and `nix/default.nix`, commit
7. **Tag**: `git tag -a vX.Y-BETA -m "description"`
8. **Push**: `git push origin main --tags` (rebase if remote is ahead, re-tag after rebase)
9. **Update nix-ham-packages** (`/home/ku0hn/dev/nix-ham-packages/tncd/`):
   - Update `version` and `rev` in `default.nix`
   - Get new hash: `nix-prefetch-url --unpack "https://github.com/ben-kuhn/tncd/archive/vX.Y-BETA.tar.gz"` then `nix hash convert --hash-algo sha256 --to sri <hash>`
   - Update `module.nix` if module options changed
   - Commit and push
10. **Update website** (`website/`): changelog, docs, index as needed — commit and push
11. **Update docs**: README.md, nix/README.md, tncd.service, PLAN.md as needed

The `v*` tag triggers `.github/workflows/release.yml` which builds .deb/.rpm packages, publishes to Cloudflare Pages (tncd.dev), updates AUR PKGBUILD, and creates the GitHub Release.
