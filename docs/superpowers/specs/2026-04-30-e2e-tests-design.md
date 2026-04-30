# E2E Integration Tests with Direwolf and PAT

## Overview

End-to-end integration tests that validate tncd's AGWPE-to-KISS bridge functionality using real Direwolf TNC instances connected via PipeWire virtual audio. Three tests cover UI frame forwarding (APRS) and connected-mode data transfer over both KISS TCP and KISS serial (pseudo-TTY) interfaces.

## Test Architecture

### Audio Plumbing

Two Direwolf instances are connected bidirectionally via PipeWire virtual audio using `pw-loopback`. Direwolf-A's audio output feeds Direwolf-B's input and vice versa, simulating an RF link without hardware.

### Test 1: APRS/UI Frames (KISS TCP)

```
AGWPEClientEmulator → tncd (AGWPE:8000) → Direwolf-A (KISS TCP:8001)
                                              ↕ PipeWire audio
                                          Direwolf-B → stdout log (parsed)
```

- Uses the existing `AGWPEClientEmulator` to send APRS packets through tncd
- Sends three packet types: position, message, telemetry
- Validates receipt by parsing Direwolf-B's stdout/log output
- Log parsing avoids dependency on the AGWPE emulator for validation

### Test 2: Connected Mode (KISS TCP)

```
PAT-A → tncd (AGWPE:8000) → Direwolf-A (KISS TCP:8001)
                                  ↕ PipeWire audio
PAT-B → Direwolf-B (native AGWPE:8010)
```

- Two PAT instances with test callsigns (`N0CALL-1`, `N0CALL-2`)
- tncd connects to Direwolf-A via KISS TCP
- PAT-B connects directly to Direwolf-B's native AGWPE server
- A ~10KB random attachment is sent as a P2P message in each direction (A→B, then B→A)
- Validates message receipt and attachment integrity on both sides
- Exercises full AX.25 connected-mode: SABM/UA handshake, I-frame sequencing, RR acks, DISC

### Test 3: Connected Mode (KISS Serial/PTY)

Identical to Test 2, except:
- Direwolf-A creates a KISS pseudo-TTY instead of a KISS TCP port
- tncd connects via `type = serial` using the PTY device path
- PTY path is parsed from Direwolf-A's startup output (e.g., `/dev/pts/NN`)

## Fixtures

### `pipewire_audio_link` (function-scoped)

- Creates two `pw-loopback` instances for bidirectional audio
- Returns identifiers for Direwolf config (device names or node IDs)
- Tears down loopback nodes on cleanup

### `direwolf_pair` (function-scoped, parameterized)

- Generates two Direwolf config files in a temp directory
- Direwolf-A: KISS TCP or KISS PTY depending on test
- Direwolf-B: Native AGWPE server enabled (port 8010)
- Both configured with PipeWire audio devices, 1200 baud
- Minimized TX timing (TXDELAY, TXTAIL, SLOTTIME) — no PTT latency on virtual audio
- Starts both processes, captures stdout for log parsing and PTY path extraction
- Kills both on teardown

### `tncd_instance` (function-scoped)

- Generates `tncd.ini` in temp directory with appropriate KISS connection settings
- For KISS TCP: `type = tcp`, `host = localhost`, `port = 8001`
- For KISS PTY: `type = serial`, `device = <parsed PTY path>`
- Starts `python tncd.py -c <config>`
- Waits for AGWPE port to accept connections before yielding
- Kills on teardown

### `pat_pair` (function-scoped, tests 2 & 3 only)

- Generates two PAT configs in separate temp directories
- Callsigns: `N0CALL-1` (via tncd) and `N0CALL-2` (via Direwolf-B native AGWPE)
- Throwaway Maidenhead locators
- Mailbox directories isolated in temp dirs
- PAT config method TBD — will try `--config` flag first, fall back to `HOME` env var override

## Direwolf Configuration

### Direwolf-A (KISS TCP variant)

```
ADEVICE pipewire:<source-A> pipewire:<sink-A>
CHANNEL 0
MYCALL N0CALL-1
MODEM 1200
KISSPORT 8001
TXDELAY 10
TXTAIL 5
SLOTTIME 10
PERSIST 255
```

### Direwolf-A (PTY variant)

Same as above, replacing `KISSPORT 8001` with:
```
SERIALKISS /tmp/kisstnc
```

Direwolf creates the PTY and prints the path to stdout.

### Direwolf-B

```
ADEVICE pipewire:<source-B> pipewire:<sink-B>
CHANNEL 0
MYCALL N0CALL-2
MODEM 1200
AGWPORT 8010
TXDELAY 10
TXTAIL 5
SLOTTIME 10
PERSIST 255
```

## Test Details

### Test 1: APRS Packets

**Packets sent via AGWPEClientEmulator (AGWPE `M` frames):**

1. **Position**: APRS position report (e.g., `!4903.50N/07201.75W-`)
2. **Message**: APRS message (e.g., `:BLN1     :Test message`)
3. **Telemetry**: APRS telemetry (e.g., `T#001,100,200,300,400,500,10000000`)

**Validation:**
- Parse Direwolf-B's stdout line by line
- Match decoded packet content against what was sent
- Timeout: ~30 seconds

### Tests 2 & 3: Connected Mode P2P Messages

**Procedure:**

1. Generate a ~10KB file of random bytes, base64-encoded for attachment
2. Use `pat compose` to create a P2P message from N0CALL-1 to N0CALL-2 with the attachment
3. Use `pat connect` to initiate the transfer (A→B)
4. Wait for transfer to complete
5. Verify PAT-B's mailbox contains the message with matching attachment
6. Repeat in reverse direction (B→A)
7. Verify PAT-A's mailbox contains the message with matching attachment

**Timeout:** ~180 seconds per direction (10KB at 1200 baud with AX.25 overhead)

## Skip Conditions

```python
pytest.mark.skipif(not shutil.which("direwolf"), reason="direwolf not installed")
pytest.mark.skipif(not shutil.which("pat"), reason="pat not installed")
pytest.mark.skipif(not shutil.which("pw-loopback"), reason="pipewire not available")
```

Tests are skipped gracefully if required binaries are not present.

## Cleanup

All fixtures use `try/finally` blocks:
- Subprocesses terminated with `SIGTERM`
- `SIGKILL` after a short grace period if `SIGTERM` doesn't work
- Temp directories cleaned up automatically (pytest `tmp_path` or `tempfile`)
- PipeWire loopback nodes destroyed

## File Location

`tests/test_e2e.py` — single file containing all three tests and shared fixtures.

## Dependencies

No new Python dependencies required. System binaries needed:
- `direwolf` (>= 1.7)
- `pat` (>= 0.15)
- `pw-loopback` (PipeWire)

These should be added to `nix/shell.nix` for the dev environment.
