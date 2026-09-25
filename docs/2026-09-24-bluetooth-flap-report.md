# tncd Bluetooth connection flap — Dev report (2026-09-24)

Host: `CarKit` (NixOS 26.05.20260922.1bc55b9, kernel 7.2.4)
tncd: `2.0-main-0333204` (`/nix/store/b2ywg4gyb39yfrmlpxvhlap12j9h66ca-tncd-2.0-main-0333204/bin/tncd`)
bluez: `bluetoothd 5.86` (mgmt 1.23)
tncd unit: `tncd.service` (AGWPE-to-KISS bridge, `User=tncd`, Wants=bluetooth.service)
Bluetooth controller: `AC:67:5D:B6:50:CF "CarKit"` (btusb)
AGWPE client driving the session: `pat` (Winlink, pid 159659, 127.0.0.1:8005)

## Port map (tncd.ini)

| Port | Device (bdaddr) | Name         | Paired on this host |
|------|-----------------|--------------|---------------------|
| 0    | 38:D2:00:01:67:9D | Truck Mobile (a.k.a. "DB50-B") | YES |
| 1    | 34:81:F4:AA:B3:D3 | Mobilinkd TNC4 | NO |
| 2    | 38:D2:00:01:52:8F | UV-Pro       | NO |

Port 0 is the TNC that flapped. Ports 1 and 2 are not paired with the host and are a separate, sustained noise source (see §6).

> **Superseded in part — see [`2026-09-24-bluetooth-flap-addendum.md`](2026-09-24-bluetooth-flap-addendum.md).**
> The relinked sockets were not carrying *zero* data; they were carrying bytes that did
> not parse as KISS. tncd only logs a parse failure and only advances `lastRX` on a
> successful parse, so at default verbosity a mis-framed stream is indistinguishable
> from silence. That changes the fix from "reconnect harder" to "resync to a FEND
> boundary and check SDP channel resolution". The addendum also corrects H3's relink
> threshold (below) and settles the RF-vs-Bluetooth question.

## Summary

- After ~55 min of failed connect attempts, port 0 finally connected at `19:27:33` and an AX.25 (Winlink P2P) session `KU0HN -> KU0HN-10` ran normally.
- At `19:29:06` an outstanding (un-ACKed) TX frame became stuck. tncd's wedge detector fired at `19:29:29` ("23s silence with unacked TX") and began a relink storm: **6 disconnect/reconnect cycles in ~98s** (`19:29:31` → `19:31:09`).
- Every reconnect delivered a `NewConnection` + "SPP socket ready" + `bridge: port 0 online`, yet **no *parseable* frame arrived from the radio on any of them** — the port remained wedged the whole time. (Originally written as "no bytes"; see the addendum — bytes did arrive, they just did not frame as KISS.)
- bluetoothd shows the underlying transport was already dead: `Disconnecting failed: already disconnected` / `No matching connection for device` (`19:30:33`) immediately after tncd's own `RequestDisconnection` (`19:30:31`), and SDP parse failures (`sdp_extract_attr: Unknown data descriptor : 0x5/0x30 terminating`) at the exact timestamps of the later reconnects (`19:29:37`, `19:30:38`).
- tncd's own guidance is borne out: "reconnecting is not clearing it; reset the Bluetooth adapter or power-cycle the TNC." Reconnecting SPP alone cannot recover this link.
- A 5-byte, unparseable AX.25 frame (`raw=b2bd7d8fe9`) arrived on the freshly "reconnected" socket at `19:31:24` — evidence that the "new" channel is actually a torn/stale data stream, not a clean SPP session.

## Narrative / sequence of events (all times CDT)

### 1. Startup and an hour of failed connects (18:27 → 19:27)
```
18:27:11 tncd.service started
18:27:12 SPP profile registered at /org/tncd/spp
18:27:12 ConnectProfile on all 3 devices
18:27:12  port 1 start error: ConnectProfile ... "doesn't exist"  (not paired)
18:27:12  port 2 start error: ConnectProfile ... "doesn't exist"  (not paired)
18:27:42  port 0 start error: connection to 38:D2:00:01:67:9D timed out (30s)
...port 0 retries ~every 30-90s, backoff 5s→60s, all "timed out (30s)"...
...ports 1/2 retry every 60s, every attempt errors "ConnectProfile doesn't exist"...
```
During this whole hour bluetoothd logs `Unable to get Serial Port SDP record: Host is down` (~every 90 s). The radio was off/out of range; bluez could not resolve the SPP channel from SDP.

### 2. First successful connect + healthy session (19:27 → 19:29:05)
```
19:27:27  port 0 "already connected, disconnecting first"   ← bluez ACL Connected=True, but no SPP data path
19:27:27  RequestDisconnection ...
19:27:33  NewConnection (fd=8) / SPP socket ready / dropped audio profile 0000111e & 0000111f
19:27:33  bridge: port 0 reconnected
19:28:24  pat: CONNECT "KU0HN" -> "KU0HN-10"
19:28:24-19:28:59  real data frames (kind=0x44), ACKs flow, outstanding drains to 0
19:28:59-19:29:05  more data queued, outstanding climbs 1→2→3→4 (normal burst)
19:29:06  outstanding drops to 1 and stays pinned  ← transport silent from here on
```
This proves the baseband + SPP path works when the radio is in a good state.

**Control-session log excerpt (the healthy 19:27:33 → 19:29:05 window):**
```
19:27:27 tncd: bluetooth: 38:D2:00:01:67:9D already connected, disconnecting first
19:27:29 tncd: bluetooth: calling ConnectProfile on /org/bluez/hci0/dev_38_D2_00_01_67_9D
19:27:33 tncd: bluetooth: NewConnection: path=/org/bluez/hci0/dev_38_D2_00_01_67_9D fd=8
19:27:33 tncd: bluetooth: SPP socket ready (fd=8) for 38:D2:00:01:67:9D
19:27:33 tncd: bluetooth: 38:D2:00:01:67:9D dropped audio profile 0000111e-0000-1000-8000-00805f9b34fb
19:27:33 tncd: bluetooth: 38:D2:00:01:67:9D dropped audio profile 0000111f-0000-1000-8000-00805f9b34fb
19:27:33 tncd: bridge: port 0 reconnected
19:28:24 tncd: agwpe: VERSION request
19:28:24 tncd: agwpe: CONNECT "KU0HN" -> "KU0HN-10"
19:28:36 tncd: agwpe: kind=0x44 KU0HN->KU0HN-10 len=74 / 29 / 6  (real traffic)
19:28:59 tncd: agwpe: 'Y' query: outstanding=0   ← ACKs flowing; queue fully drains
19:28:59 tncd: agwpe: kind=0x44 KU0HN->KU0HN-10 len=30 / 127 / 114 / 2
19:28:59 tncd: agwpe: 'Y' query: outstanding climbs 1→2→3→4
19:29:05 tncd: agwpe: 'Y' query: outstanding=4     ← pinned
19:29:06 tncd: agwpe: 'Y' query: outstanding=1     ← after partial ACK, one frame stuck forever
19:29:29 tncd: bridge: port 0 RX wedged -- 23s silence with unacked TX; relinking (keeping session)
```

### 3. Link dies mid-session (19:29:06)
```
19:29:06  outstanding drops to 1 and stays pinned at 1  (an un-ACKed data frame is stuck)
19:29:06-19:29:29  pat sends 'Y' (0x59) queries ~4/s; outstanding never clears → transport is silent
19:29:29  bridge: port 0 RX wedged -- 23s silence with unacked TX; relinking (keeping session)
```

### 4. Relink storm (19:29:31 → 19:31:09) — the visible "flap"
| # | Disconnect (tncd) | NewConnection | bluetoothd at same time | Result |
|---|-------------------|---------------|--------------------------|--------|
| 1 | 19:29:31 | 19:29:37 fd=12 | `sdp_extract_attr: Unknown data descriptor : 0x5 terminating` | online, no parseable frames |
| 2 | 19:29:51 | 19:29:56 fd=12 | — | online, no parseable frames |
| — | — | 19:30:09 | `still wedged after 3 relinks` | — |
| 3 | 19:30:11 | 19:30:18 fd=12 | — | online, no parseable frames |
| — | — | 19:30:29 | `still wedged after 4 relinks` | — |
| 4 | 19:30:31 | 19:30:38 fd=12 | `Disconnecting failed: already disconnected` (19:30:33), `No matching connection for device` (19:30:33), `sdp_extract_attr: Unknown data descriptor : 0x30 terminating` (19:30:38) | online, no parseable frames |
| — | — | 19:30:49 | `still wedged after 5 relinks` | — |
| 5 | 19:30:51 | 19:30:56 fd=12 | — | online, no parseable frames |
| — | — | 19:31:09 | `still wedged after 6 relinks -- reconnecting is not clearing it; reset the Bluetooth adapter or power-cycle the TNC (consider serial/tcp for this port)` | — |
| 6 | 19:31:11 | 19:31:17 fd=12 | — | online, no parseable frames |

### 5. End of session
```
19:30:59  pat gives up: DISCONNECT "KU0HN" -> "KU0HN-10"
19:31:24  bridge: port 0 failed to parse AX.25 frame: frame too short (5 bytes) len=5 raw=b2bd7d8fe9
```
That last line is important: 7 s after a *fresh* SPP connect, the socket delivered 5 corrupt bytes. bluez handed back a torn stream, not a clean channel.

## Root-cause analysis (ranked)

### H1 — The SPP channel silently dies while bluez believes the ACL link is alive (primary)
- tncd saw "already connected" at 19:27:27 even though no SPP data path existed; bluez's `Connected` (ACL/baseband) state did not reflect the broken profile session.
- During the storm, tncd's `RequestDisconnection` at 19:30:31 was answered by bluez with "already disconnected / No matching connection" at 19:30:33 — the transport had dropped *before* tncd acted. The wedge detector was correct; it simply cannot fix a dead link by reconnecting.
- Every relink's `NewConnection` presented a live `fd`, but zero bytes flowed on all six. The "connected" SPP socket is therefore credential-free trust in bluez state that does not match reality.

### H2 — SDP record resolution is failing on this device (likely root cause of H1)
- `Unable to get Serial Port SDP record: Host is down` during the entire first hour (failed initial connects).
- `sdp_extract_attr: Unknown data descriptor : 0x5 / 0x30 terminating` precisely at the two later relink connects.
- The radio advertises Handsfree (`0000111e`) and Handsfree Audio Gateway (`0000111f`) profiles (dropped by tncd on every connect) — an unusual SDP record for a TNC. bluez's SDP parser terminating mid-record means SPP channel lookup falls back to cached/stale channel info. The "connected" socket that then carries garbage (5-byte partial frame, §5) is consistent with bluez wiring up a channel number it never actually verified.
- Needs a `btmon` trace to confirm, and possibly a DB50-B firmware report upstream.

### H3 — tncd relink policy makes the flap worse
- Backoff between relinks is ~20 s and relinks proceed even when the preceding `RequestDisconnection` itself errored ("already disconnected").
- Each relink tears down the baseband link (`RequestDisconnection`), so even if the radio firmware might have recovered its own data path after a hiccup, tncd kills it.
- From the 3rd relink onward (`relinkEscalateAfter = 3`) tncd only prints a warning; it keeps cycling forever against a dead radio, keeping the AGWPE client's session half-alive (outstanding frames pinned, 'Y' queries answered with no progress). The 6 above is the count observed before the log was drafted, not a threshold.
- `19:31:24` garbage frame shows the socket isn't even discarded/framed properly on an exhausted reconnect — consider tearing down to a *hard* reset (adapter power cycle == what the warning suggests) rather than more SPP reconnects.

## Secondary issues worth fixing while you're in here

- **Unpaired devices are probed forever (ports 1, 2).** `Mobilinkd TNC4` and `UV-Pro` are not paired/cached on the host, yet tncd calls `ConnectProfile` on them every 60 s and logs `Method "ConnectProfile" with signature "s" on interface "org.bluez.Device1" doesn't exist` each time — ~2 error lines/min sustained for the entire 1h+ log (plus bluetoothd `Host is down` SDP noise). tncd should gate on the bluez device object actually exposing `org.bluez.Device1` (paired/trusted, valid adapter), or support an explicit `disabled` flag so unprovisioned ports don't spam the log.
- **"already connected, disconnecting first" on every connect** — bluez `Connected` propagation is unreliable on this hardware combo; tncd should not assume that a `NewConnection` fd implies a functional channel.
- **Torn-frame handling** — the `frame too short` message on a fresh socket implies tncd keeps reading bytes from a stale/half-dead stream; add frame-level flow resync (KISS FEND) instead of one-shot parse-and-error.

## Recommended developer actions

1. Reproduce with `btmon`/`bluetoothctl monitor` while running a Winlink P2P session to capture: when bytes stop, do L2CAP RFCOMM flow-control or the baseband link drop first? Is `Disconnect Complete` ever received, or is bluez totally blind to the loss?
2. Dump the DB50-B SDP record on connect; check why sdp_extract_attr terminates (descriptor 0x5/0x30) and whether the SPP RFCOMM channel number is being resolved or guessed.
3. In tncd: treat "already disconnected" on teardown as proof the link is unrecoverable and escalate to adapter reset / stop-and-report rather than reconnecting blindly; add a config knob for max relinks; surface wedge state to the monitoring API (`127.0.0.1:8002`).
4. Skip unpaired devices or make ports configurable enabled/disabled.

## Follow-up: the storm is ongoing and endless (live check ~19:33)

Re-checked the live journals after drafting; the cycle had **not stopped** and is repeating indefinitely:

- Relink count climbed past the original window: `still wedged after 7 relinks` (19:31:29), `after 8` (19:31:49), then the counter **resets** and a fresh wedge is declared:
  - `RX wedged -- 23s silence with unacked TX; relinking` (19:32:24)
  - `RX wedged -- 20s silence with unacked TX; relinking` (19:32:44)
  - `still wedged after 3 relinks` (19:33:04) → `after 4` (19:33:24) → …
- **Every** reconnect now coincides with a bluetoothd `sdp_extract_attr: Unknown data descriptor : 0x30 terminating` (19:31:37, 19:32:51, 19:33:12) — strengthens H2 (SDP channel resolution is broken on this radio).
- pat is still mid-session and still stuck: `'Y' query ... outstanding=2` (19:33:42) → `outstanding=3` → continues sending data frames (kind 0x44) that go nowhere.
- The `still wedged ... reset the Bluetooth adapter` message is diagnostic-only: tncd does **not** stop or escalate after N relinks — it resets the counter and repeats forever.

Net: three full ~8-relink cycles observed (`19:29:29`→`19:31:09`, `19:32:24`→`19:33:24`, and continuing). This is not a transient; a resync-free, endless reconnect loop is the steady state until the radio is physically recovered.

## Log artifacts (attached)

- `journalctl -u tncd` (full day) — 2231 lines
- `journalctl -u bluetooth` (full day) — 129 lines
- Relevant excerpt window: `19:27:00`–`19:31:30`
- tncd config used: `/nix/store/ls1mx4gcsbxb7vbwfdq1gayp1s5ww93y-tncd.ini`