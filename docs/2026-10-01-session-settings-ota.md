# Session settings OTA, 2026-10-01 — blocked by the UV-PRO, not by tncd

**Outcome**: the session-settings feature applied and restored correctly on real
hardware, but the on-air half of the test could not run on the UV-PRO: that
radio does not transmit. A controlled modem swap pinned the failure to the
radio and cleared tncd's L2 in the same run.

## What was tested

The radio was put in the worst realistic starting state by hand: dual watch ON
(side A), **both** VFOs on named memories (A = 5 "AUS 730", B = 1 "MN Pack"),
APRS beaconing on.

Because rig control and KISS share one link on these radios, a process holding a
session cannot coexist with a running tncd — hence `session-acquire`, which
applies the settings and exits. All three took:

```
before: channel_a=5   dual_watch=A  aprs=on
now:    channel_a=252 dual_watch=off aprs=off
```

## The on-air result, and how it was isolated

tncd then sent SABM to KU0HN-10 (145.670) over the UV-PRO. Six attempts, no UA.
Rather than guess, Dire Wolf's KISS port was used as an independent monitor —
a second receiver on the same frequency, decoding what actually reached the air:

```
tncd:      22 x [TX] SABM  KU0HN -> KU0HN-10
Dire Wolf:  0 SABM heard
BOTH:       1 x UI  KU0HN-1 -> MAIL      (14:03:00)
```

Both radios decoded the *same* passing UI frame, which rules out the two easy
explanations: they are on the same frequency, and the monitor is not deaf. The
UV-PRO's receive path works. Its transmit does not reach the air. `tx` counts
frames handed to the transport, not frames transmitted.

**Controlled swap — one link changed.** Same tncd binary, same AX.25 engine, same
gateway, different modem (`type = tcp`, Dire Wolf's KISS port 8001, driving the
TS-2000):

```
connected to KU0HN-10, 5+ proposals accepted,
4 messages downloaded, clean I/RR sequencing, DISC, PAT exit 0
```

So tncd's L2 is correct, the gateway is up and reachable, and the broken link is
the UV-PRO's transmit path. This is the already-documented UV-PRO firmware bug
(it buffers frames internally; inbound RF flushes them), not a regression and not
a consequence of the session settings.

## What this does and does not establish

**Established:**

- `session-acquire` correctly applies all three managed fields from the worst
  starting state, including moving off a named memory with *both* VFOs on
  memories.
- The radio operates normally in the state tncd puts it in — while held, it
  decoded the passing `KU0HN-1 -> MAIL` UI frame, so VFO mode at the target
  frequency with dual watch and APRS off does not harm reception.
- Restore is exact. `session-set 5 1 on` returned the radio to
  `channel_a=5 dual_watch=A aprs=on`, and every readable record was
  byte-identical to the pre-test capture.
- tncd's L2 and the gateway are both healthy, which is worth having on record
  independently.

**Not a question:** whether packet behaves better with dual watch off. There is
one receiver, time-slicing between two frequencies; traffic on the other side
eats part or all of an inbound frame. That follows from how the hardware works
and is not worth bench time. Dual watch off is a requirement, not a hypothesis.

**Still open:**

- The automatic trigger points are unwired, so this test drove the session from
  the CLI rather than from a connect.

## Reproducing the TX isolation

The monitor is worth keeping as a recipe, because it is the cheapest way to tell
"tncd did not send" from "the radio did not transmit":

1. A second receiver on the frequency with a KISS port (Dire Wolf on another
   radio, `KISSPORT 8001`).
2. Read frames off that port while tncd transmits.
3. A frame both stacks decode proves same-frequency and a live monitor; a TX
   count with no corresponding reception proves the radio is swallowing frames.
