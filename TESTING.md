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

### Mobilinkd TNC4 (Serial KISS, USB) + Kenwood TH-D7A — PASS

- **Connection:** Serial KISS via `/dev/ttyACM0` (USB CDC ACM)
- **Radio:** Kenwood TH-D7A, connected via accessory jack
- **Result:** Full B2F exchange completed. 4 messages uploaded, 2 messages downloaded from CMS. Some first-frame-in-burst loss on RX (I(1) missed on first attempt, recovered via retransmit), but the protocol recovered every time and the transfer completed successfully.
- **Note:** This validates tncd with a real hardware KISS TNC over the air. The TNC4 + TH-D7A combination works reliably for connected-mode packet.

### Mobilinkd TNC3 (Serial KISS, USB) + Ailunce HD1 — FAIL

- **Connection:** Serial KISS via `/dev/ttyACM0` (USB CDC ACM)
- **Radio:** Ailunce HD1 (DMR/analog), connected via accessory jack
- **Firmware:** Mobilinkd TNC3 v2.5.9
- **Result:** Connection established, but the B2F exchange never completes. Two failure modes observed:
  1. **RX loss:** The first AX.25 frame in each gateway TX burst is consistently lost. The gateway sends I-frame + RR back-to-back; the Mobilinkd only delivers the RR. Raw KISS serial spy confirmed the I-frame bytes never arrive on the USB serial interface.
  2. **TX loss:** Gateway never acknowledges our frames (N(R) stays at 0). Our RR/I-frames are transmitted but the gateway's Direwolf cannot decode them.
- **Analysis:** The serial spy rules out a kiss3 library issue — frames are lost before reaching the host. Increasing the gateway's TXDELAY from 350ms to 500ms did not help. Likely a hardware issue with the HD1's accessory jack audio path (RX settling time or TX deviation).

### Kenwood TS-2000 (Built-in TNC, Serial KISS) — PASS

- **Connection:** Serial KISS via `/dev/ttyUSB0` (CP2102N USB-UART), 57600 baud
- **Radio:** Kenwood TS-2000, built-in TNC in KISS mode (`kiss on` + `restart` via serial console)
- **Result:** Full CMS round-trip completed. 5 outbound messages sent, 16 inbound messages received (including messages up to 65KB). Multiple B2F proposal rounds handled cleanly. A few RR poll retransmissions (normal for 1200 baud OTA), all recovered successfully.
- **Note:** KISS mode must be entered manually via serial console before starting tncd. Hardware flow control must be disabled (triggers PTT on this rig). The `init_string` feature for programmatic KISS mode entry requires further work — HUPCL disable via termios prevents DTR reset on port close, but the TS-2000 still does not reliably enter KISS mode via programmatic serial writes.

### Kenwood TH-D7A (Built-in TNC, Serial KISS) — UNTESTED

- **Connection:** Serial KISS via `/dev/ttyUSB0` (CP2102N USB-UART)
- **Known concern:** Buffering issues reported with larger transfers in other setups.
- **Status:** Available for testing. The TH-D7A has a built-in TNC that can be put in KISS mode.

## Fixes Applied During Testing

- **Implicit connect (lost UA workaround):** When an I-frame arrives during CONNECTING state, promote to CONNECTED. Handles the case where the UA was sent but lost OTA — the I-frame proves the remote accepted the SABM.
- **CONNECTING state guard:** Fixed state name check from `SABM_SENT` to `CONNECTING` to match the actual state set by the 'C' handler.
- **configparser inline comment fix:** `ota_baudrate = 1200 # comment` caused a ValueError. Moved comment to a separate line.
- **AX.25 RNR handling (6.4.9):** Added `remote_busy` flag to stop I-frame sending when remote sends RNR. Cleared on RR or REJ.
- **AX.25 FRMR handling (2.4.5):** Reset connection and re-establish link via SABM on FRMR.
- **AX.25 N2 retry limit (6.3.2):** Disconnect after 10 consecutive unanswered T1 polls.

## Protocol Spec Compliance

### KISS (TNC2 spec)

- **FEND framing:** Handled by `kiss3` library. Serial and TCP transports supported.
- **KISS commands:** TX_DELAY, Persistence, SlotTime, TX_TAIL, FullDuplex sent via SetHardware from config.
- **Port byte:** Channel 0 used (port in high nibble, command in low nibble).
- **SLIP escaping:** Handled by `kiss3` (FEND/FESC/TFEND/TFESC).
- **Init string:** Optional `init_string` / `init_delay` for TNCs requiring a command to enter KISS mode.

### AGWPE

- **Header format:** 36-byte fixed header, little-endian, 10-byte null-padded callsign fields.
- **Frame types implemented:** R (version), G (ports), g (capabilities), X (register call), x (unregister), m/M (monitor toggle), C/c (connect), v (connect via), D (data), d (disconnect), K (raw), Y (outstanding frames), y (frames waiting).
- **'C' notification text:** `*** CONNECTED With Station` for outgoing, `*** CONNECTED To Station` for incoming (PAT compatibility).
- **'Y' response:** Reports `unacked + queued` (matches Direwolf behavior for PAT flow control).
- **Monitoring:** 'T'/'U'/'S'/'I' monitor frames sent to clients with monitoring enabled.

### AX.25 (v2.0, mod-8)

- **Connected mode:** SABM/UA handshake, I-frame sequencing, RR acknowledgment, DISC/UA teardown.
- **SABME rejection:** Responds with DM (tncd only supports mod-8, not mod-128 extended mode). Direwolf falls back to SABM.
- **Duplicate detection:** Received I-frames with N(S) != recv_seqno are acknowledged but data is not re-delivered.
- **Window flow control:** Configurable `max_window` (default 3, max 7). Outbound queue drains as ACKs arrive.
- **I-frame coalescing:** Adjacent small payloads merged up to 256 bytes per frame.
- **RNR handling (6.4.9):** `remote_busy` flag stops I-frame sending. Cleared on RR or REJ from remote.
- **REJ handling:** Retransmits from requested N(R) onward. Clears `remote_busy`.
- **FRMR handling (2.4.5):** Resets connection state and re-establishes link via SABM.
- **N2 retry limit (6.3.2):** Disconnects after `n2_retry` (default 10) consecutive unanswered T1 polls.
- **T1 timer:** Adaptive based on OTA baud rate. First expiry sends RR poll; second+ also retransmits.
- **T2 timer:** Delayed ACK for received I-frames, configurable multiplier.
- **RR dedup:** Suppresses duplicate RR F=1 responses within 3s to prevent half-duplex TX buffer flooding.
- **DM for unknown connections:** I-frames to unregistered connections get DM response (clears stale remote sessions).
- **TX echo suppression:** Frames sent by tncd are suppressed when received back from the TNC.

