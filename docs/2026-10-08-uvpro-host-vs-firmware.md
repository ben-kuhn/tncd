# The UV-PRO frame loss is at least partly ours — hold the firmware report

**Status: the firmware report's lead claim is not currently defensible. Do not
send it.** This is the controlled test that should have been run before that
claim was written.

## What was measured

2026-10-08. Same radio, same frequency, same quiet channel, same continuously
running independent monitor, arms interleaved within the same hour. Identical
stimulus throughout: SABM every 3 s to a station that does not answer, so the
result is pure N2 retries with no connected-mode state.

Each arm addressed a **different SSID** so every decode is attributable by
callsign. That matters: an earlier version of this harness attributed by time
window and reported 13 frames decoded from 12 sent (106%), because frames one
arm flushed late landed in the next arm's window. Any result from that version
is void.

| arm | tncd L2/KISS | tncd Bluetooth code | handed | on air | radiated |
|---|---|---|---|---:|---:|---|
| raw `AF_BLUETOOTH` socket, 40 lines of Python | no | no | 48 | 48 | **100%** |
| tncd, `type = serial` on the kernel's `/dev/rfcomm0` | **yes** | no | 11 | 11 | **100%** |
| tncd, `type = bluetooth` (BlueZ/D-Bus SPP) | yes | **yes** | 22 | 16 | **73%** |

The tncd/BlueZ arm also **failed to bring the port up at all** in one of three
attempts, which is the same `br-connection-refused`-class symptom the firmware
report attributes to the radio. The raw socket connected 4 for 4.

The loss tracks tncd's **BlueZ transport**, not its AX.25 layer and not the
radio.

## Why the earlier evidence looked like firmware

The report's lead measurement -- 11 SABMs handed over, 1 radiated -- was taken
on 2026-10-03 with tncd, **on a busy channel against a live, transmitting
gateway**. Carrier-sense deferral was never controlled for. On a quiet channel
today the same software loses 2-3 of 11, not 10 of 11. So that figure has at
least two uncontrolled contributors beyond the firmware: our Bluetooth path, and
channel occupancy.

## What this does NOT overturn

Three things still point away from tncd, and a tncd bug does not explain them:

- **WoAD on Android** shows the same behaviour with the same radio. It shares no
  code with tncd. A defect in `kiss/bluetooth_linux.go` cannot cause that.
- **The 2m07s dead air** on 2026-10-03, with frames surfacing after the session
  had already died, is hard to produce by dropping writes.
- **A Mobilinkd TNC4 on the identical tncd code path passes**, including on
  Windows. If tncd's Bluetooth transport simply lost frames, it should lose the
  TNC4's too.

So the likeliest reading is **two separate problems** that have been conflated
all along: a host-side defect in our BlueZ write path, and a radio-side defect.
The report currently credits all of it to the radio.

## Unresolved contradiction

The September swap table (see the project memory on this failure) recorded the
UV-PRO over a kernel `/dev/rfcomm0` tty **stalling at 57%**. Today that same
configuration was clean, 11/11, twice. Both cannot be generally true. Either
that run had a confound, today's was lucky, or something changed in tncd between
then and `5ce0879`. Unresolved, and it bears directly on which layer to blame.

Note also that the rfcomm arm is capped at **n=2**: the radio refuses a second
RFCOMM connect while it still holds the ACL (`Port has been closed` immediately
after open), so only the first round after a fresh bind yields data. That
limitation is itself consistent with the radio-side ACL behaviour already
documented -- and it is why tncd's teardown-before-connect exists.

## What to do next, in order

1. **Hold the firmware report.** Pull the 11-of-12 section rather than hedging
   it; it is not a safe lead claim.
2. **Audit `kiss/bluetooth_linux.go`'s write path** against the raw-socket
   arm, which is the known-good reference. Three runs at 8-of-11 with a
   12-second dead window swallowing three consecutive frames is a specific,
   chaseable signature.
3. **Re-run the 2026-10-03 comparison properly**: both arms, busy channel, same
   hour, attribution by SSID. That is the measurement that separates the
   channel's contribution from the host's.
4. Only then decide what, if anything, is genuinely the firmware's.

## Harness

`scratchpad/`: `noTncdProbe.py` (raw socket, hand-built SABM, validated by
reproducing tncd's own on-air address bytes), `kissutil-probe.sh` (Dire Wolf's
`kissutil` driving the radio over `/dev/rfcomm0`), `alternate.sh` (interleaved
arms, per-SSID attribution), `isolate.sh` (tncd over the kernel tty).
