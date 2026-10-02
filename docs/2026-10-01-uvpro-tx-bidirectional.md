# UV-PRO transmit, isolated bidirectionally — 2026-10-01 (post-reboot)

> **SUPERSEDED — see `docs/2026-10-02-uvpro-tx-confirmed.md`.** The UV-PRO does
> transmit; this report's zeros were measured with the TS-2000 selected on the
> wrong VFO (so transmitter and receiver were on different frequencies) and read
> out through a monitor script that reported false zeros. Nothing in the matrix
> below is usable.
>
> **RETRACTED 2026-10-01, same day.** The conclusion below -- "the UV-PRO does
> not transmit" -- is not supported by the evidence in it, and the operator was
> right to reject it. See "What was wrong with this" at the end. The measurements
> are left intact; the conclusions drawn from them are not reliable.

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

## What was wrong with this — RETRACTION

The operator's objection: this radio has had no successful transmission since
the session-settings work, and it worked before. Checking the reasoning rather
than arguing, three of the four claims above do not hold.

**1. "Does not transmit" is false. The radio keys.** The operator watched the
TX light come on during these very runs. So frames do reach the radio and it
does key the transmitter. The honest statement is much narrower: *nothing the
UV-PRO sent was decoded by the one receiver under test.* Those are different
failures with different causes, and the stronger wording pointed diagnosis in
the wrong direction.

**2. Every "0 received" rests on a receiver that was never validated.** The ✅
rows validate the UV-PRO *as a receiver*. Nothing anywhere in this report
validates TS-2000 → digirig → Dire Wolf *as a receiver*. A closed squelch, a
dead audio path or a wrong mixer level would produce exactly these zeros with a
perfectly healthy transmitter. The claim that the receive rows "rule out a deaf
monitor" is simply wrong: they exercise the opposite direction, through
different hardware.

Confirming that: the TS-2000's S-meter was polled every 3s through a full
transmit cycle and read a constant **-54** with no deviation whatsoever,
including while the TX light was on. A reading that never moves is not a
measurement.

**3. The Dire Wolf comparison changed two variables, not one.** Dire Wolf is
reached over **TCP**, not Bluetooth. So "same binary, same L2, different modem"
was never true -- it was a different modem *and* a different transport. It
therefore cannot separate "the UV-PRO is broken" from "tncd's Bluetooth TX path
is broken", which is precisely the hypothesis the operator raised. CLAUDE.md
says change one link at a time; this changed two and then reasoned as if it had
changed one.

**4. The session settings are NOT exonerated.** The 145.730 row put the radio
back into the operator's own settings *state*, but `session-set` had been run
over the same Bluetooth link minutes earlier, so the radio had still received
`WRITE_SETTINGS` since its reboot. That row rules out the three field *values*.
It does not rule out the Gaia control traffic, nor a wedge those writes may
leave behind.

**Also not usable:** a binary bisect against 47656b1 (pre-2026-09-30) also
decoded 0 frames, which would contradict "it worked prior" -- but it rests on
the same unvalidated receiver, so it establishes nothing either way.

### What would actually settle it

- **Same Bluetooth code, different radio.** A Mobilinkd TNC over Bluetooth,
  transmitting while Dire Wolf listens. If it decodes, the Bluetooth TX path and
  the receiver are both good and the UV-PRO is the outlier; if it does not, the
  receiver or the Bluetooth TX path is at fault and the UV-PRO was never the
  problem. **Blocked**: TNC4's pairing is gone (`Paired: no`) and TNC3 did not
  answer a 30s connect, so it is powered off.
- **Validate the receiver independently of any decode.** Capture the digirig
  input directly and look for signal energy while the radio keys. **Blocked**:
  Dire Wolf holds that capture device exclusively, and it is the operator's
  running iGate.
- **Compare the bytes tncd hands the radio** against a known-good build, at the
  transport boundary. This needs no radio at all and is the one avenue not
  blocked on hardware.
