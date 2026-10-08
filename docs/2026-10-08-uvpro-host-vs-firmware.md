# The UV-PRO frame loss is at least partly ours — hold the firmware report

**Status: two distinct failure states, and they point in opposite directions.**
In the mild state there is a measurable host-side component. In the severe state
a bare socket with no tncd at all radiates nothing either, which is the
strongest firmware evidence this project has produced. Read the whole file
before quoting either half.

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

## Later the same day: a 10-round series, and the radio went fully deaf

Run after the above, same harness, 5 rounds per arm interleaved, per-SSID
attribution, same continuously-running monitor.

| arm | rounds | handed | on air |
|---|---:|---:|---:|
| raw `AF_BLUETOOTH` socket, no tncd | 5 | 60 | **0** |
| tncd, `type = bluetooth` | 5 | 55 | **0** |

**115 frames, none radiated, over about 13 minutes.** Every socket write
returned its full byte count; tncd's port stayed online with no link failures.

The monitor was not the problem, and there is a positive control for that: in
the middle of the series it decoded a genuine off-air frame from another station
(`KU0HN-1>MAIL`), and the receiving radio was verified on VFOA 145.670
afterwards. Audio, frequency and squelch were all working, so the monitor would
have heard our transmissions had they existed.

**This is the strongest firmware evidence we have** -- stronger than the 11-of-12
measurement the report currently leads with, because there is no tncd in the
losing arm at all, and because it carries its own positive control. A raw socket
wrote 60 frames into the radio and none reached the air.

Immediately afterwards the radio stopped accepting SPP connections altogether:
`ConnectProfile` timed out at 30 s while BlueZ still reported `Connected: yes`.
The control channel was therefore unreachable, so the radio's own frequency
could not be read back -- **a self-retune cannot be formally excluded** for this
series, though it would not explain the refused connections. Clearing it needs a
power cycle.

## Reconciling the two states

Both results are real and they are not in conflict once separated:

- **Mild state** (earlier, radio freshly powered): raw socket 48/48, tncd over
  the kernel tty 11/11, tncd over BlueZ 16/22 with one link failure. A real
  host-side component, in our BlueZ transport.
- **Severe state** (after an hour of connection churn): everything dies equally,
  raw socket included. Purely the radio.

The radio appears to **degrade progressively under connection churn** -- which is
consistent with the long-standing note that only inbound RF or a power cycle
clears the wedge, and with the operator's own suspicion that the churn and
restarts between tests are themselves a trigger.

## Revised guidance

1. The firmware report's *conclusion* is better supported than it looked an hour
   ago. Replace the 11-of-12 lead (busy channel, uncontrolled carrier sense)
   with the **0-of-60 raw-socket series**, which has none of that weakness.
2. The host-side defect in our BlueZ write path is still real and still worth
   chasing, but it is a **separate, milder** problem -- not the headline.
3. Any further bench work needs a **power cycle between series**, and must
   record the radio's own frequency read-back at the start of each, since once
   it wedges the control channel is gone and that check is impossible.
