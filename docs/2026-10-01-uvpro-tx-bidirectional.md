# UV-PRO transmit, isolated bidirectionally — 2026-10-01 (post-reboot)

**Result**: the UV-PRO does not transmit. A reboot does not fix it, it is not
caused by tncd's session settings, and it is not frequency-dependent. Receive
works perfectly in the same breath.

This supersedes the weaker evidence in
`docs/2026-10-01-session-settings-ota.md`, which rested on both stacks happening
to decode one passing beacon.

## Method

Two tncd instances, same binary, same AX.25 engine, one per radio:

| instance | transport | callsign |
|---|---|---|
| A | Bluetooth, UV-PRO | KU0HN |
| B | `type = tcp` to Dire Wolf's KISS port, TS-2000 | KU0HN-7 |

Each transmits by being asked to connect to a station that does not exist, so it
sends SABM until it gives up. The other instance's RX log is then the measurement:
tncd decoding a frame is proof it was on the air, where a `tx` counter is not.

Running both directions at each of two frequencies turns "we heard nothing" into
a controlled result, because the opposite direction at the same frequency proves
both radios are live there.

## The matrix

| frequency | direction | radio state | TX | RX |
|---|---|---|---|---|
| 145.670 | TS-2000 → UV-PRO | managed (VFO 252) | 11 | **11** ✅ |
| 145.670 | UV-PRO → TS-2000 | managed (VFO 252) | 11 | **0** ❌ |
| 145.730 | TS-2000 → UV-PRO | unmanaged (memory 5, dual watch on) | 11 | **11** ✅ |
| 145.730 | UV-PRO → TS-2000 | unmanaged (memory 5, dual watch on) | 11 | **0** ❌ |

Every row changed exactly one thing from a row above it.

## What each row rules out

- **The two ✅ rows rule out the easy explanations.** Wrong frequency, deaf
  monitor, dead Dire Wolf KISS path, broken audio, a mis-wired test — all would
  have to break the receive direction too, and none of them do. They also prove
  the UV-PRO's receive chain is healthy at both frequencies.
- **The 145.730 row rules out tncd's session settings as the cause.** Every
  earlier UV-PRO transmit attempt that day had session settings applied, so
  "VFO mode broke TX" was a live hypothesis — and a serious one, since it would
  have been a bug in new code. This row has the radio in the operator's own
  state: on its named memory, dual watch on, APRS on, with no session management
  anywhere in the picture. It fails identically.
- **Both ❌ rows rule out the reboot as a fix.** The radio had just been
  rebooted, which was the reason to retest at all.

## Consequences

- The UV-PRO cannot validate anything transmit-side. That includes the one
  remaining item on the session-settings design: an on-air session driven by the
  automatic path that completes a transfer. It needs a different Benshi radio or
  fixed firmware, and no amount of work in tncd will unblock it.
- tncd's AX.25 engine and the KU0HN-10 gateway are both known-good as of the
  same day: the same binary completed a full Winlink CMS session (4 messages,
  clean I/RR sequencing, clean DISC) over Dire Wolf's KISS port.
- Session settings applied and restored correctly throughout. The radio finished
  byte-identical to the baseline captured before the first instance started.

## The recipe, kept

Two tncd instances pointed at two radios, each made to transmit by connecting to
a nonexistent station, with the other's RX log as the measurement. It needs no
extra tooling, it distinguishes "tncd did not send" from "the radio did not
transmit", and running it in both directions is what makes a silent result
trustworthy rather than ambiguous.
