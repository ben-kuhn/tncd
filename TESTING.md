# OTA Testing Status

Testing tncd with hardware KISS TNCs over the air via a BPQ gateway (KU0HN-10) on 145.670 MHz, using PAT as the AGWPE client for Winlink CMS message exchange.

## Test: 10KB Round-Trip Message Exchange

Upload a 10KB message to CMS, wait, then download pending messages. Validates the full AX.25 connected-mode data path: SABM/UA handshake, I-frame sequencing, RR acknowledgment, flow control (Y-frame), B2F protocol exchange, and DISC teardown.

## TNC Results

### Direwolf (TCP KISS) + TS-2000 — PASS

- **Connection:** TCP KISS on localhost:8001
- **Radio:** Kenwood TS-2000, PTT via hamlib rigctld
- **Result:** 10KB upload completed in ~36s, download of 2 messages succeeded. Full B2F handshake, no frame loss, some retransmissions on download (normal for OTA).
- **Note:** This validates tncd's protocol logic but not the hardware TNC use case. Direwolf handles its own KISS framing and audio — there is no serial TNC in the path.

### UV-Pro (Serial KISS, Bluetooth) — PARTIAL

- **Connection:** Serial KISS via `/dev/rfcomm0` (Bluetooth SPP)
- **Radio:** Built-in transceiver
- **Result:** Upload reached ~38% before stalling. Audio levels degraded from ~46 to ~10 over the session, causing the gateway to lose our frames. Likely a hardware audio issue on the UV-Pro side.
- **Known issue:** Audio degradation during extended transmissions. The T1 poll-then-retransmit fix (v0.7.1-Beta) correctly recovered and retransmitted, but the radio's audio quality dropped below decode threshold.

### Mobilinkd TNC3 (Serial KISS, USB) + Ailunce HD1 — FAIL

- **Connection:** Serial KISS via `/dev/ttyACM0` (USB CDC ACM)
- **Radio:** Ailunce HD1 (DMR/analog), connected via accessory jack
- **Firmware:** Mobilinkd TNC3 v2.5.9
- **Result:** Connection established, but the B2F exchange never completes. PAT hangs waiting for the CMS prompt and eventually the gateway disconnects.
- **Root cause:** The first AX.25 frame in each gateway TX burst is consistently lost. The gateway sends I-frame + RR back-to-back; the Mobilinkd only delivers the RR to tncd. A Direwolf monitor station (receive-only, same frequency) decodes both frames every time, confirming the OTA signal is good.
- **Analysis:** After tncd transmits an RR response, the gateway responds with a burst starting with an I-frame. The Mobilinkd/HD1 consistently fails to decode this first frame. The trailing RR (second frame in the burst) is always received. Increasing the gateway's TXDELAY from 350ms to 500ms and TXTAIL from 50ms to 300ms did not resolve the issue.
- **Open question:** Is the Mobilinkd dropping the frame (demodulated but lost in USB serial/kiss3 framing), or is the HD1 radio failing to decode it (accessory jack audio issue, receiver settling)? A raw KISS serial spy has been added to the test branch to distinguish these cases.

### Kenwood TH-7A (Built-in TNC, Serial KISS) — UNTESTED

- **Connection:** Serial KISS (expected `/dev/ttyUSB0`)
- **Known concern:** Buffering issues reported with larger transfers in other setups.
- **Status:** Next to test. The `test/ota-hardware-tnc` branch includes raw KISS serial spy logging to help diagnose any frame delivery issues.

## Fixes Applied During Testing

- **Implicit connect (lost UA workaround):** When an I-frame arrives during CONNECTING state, promote to CONNECTED. Handles the case where the UA was sent but lost OTA — the I-frame proves the remote accepted the SABM.
- **CONNECTING state guard:** Fixed state name check from `SABM_SENT` to `CONNECTING` to match the actual state set by the 'C' handler.
- **configparser inline comment fix:** `ota_baudrate = 1200 # comment` caused a ValueError. Moved comment to a separate line.
- **Raw KISS serial spy:** Added `_install_serial_spy()` to log raw KISS frames from serial TNCs before kiss3 unframing, for diagnosing frame drops at the USB/serial layer.

## Test Branch

Branch `test/ota-hardware-tnc` contains all fixes and diagnostic logging. The KISS RX and serial spy logging are temporary instrumentation and should be cleaned up before merging to main.
