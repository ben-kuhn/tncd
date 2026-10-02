# The UV-PRO does transmit — 2026-10-02

This supersedes `docs/2026-10-01-uvpro-tx-bidirectional.md` entirely. That
report's conclusion was wrong, and the cause was two faults in the test rig,
both mine.

## The two rig faults

**1. The TS-2000 was on the wrong VFO.** Probing the rig's VFOs with
`rigctl V VFOA / V VFOB` left the radio SELECTED on VFOB. Every later
`rigctl F 145670000` therefore retuned the 6 m sub VFO, while VFOA — the band
actually in use — sat on 145.730. Hours of "0 frames decoded" were measured
with the transmitter and receiver on different frequencies.

**2. The monitor script was lying.** A hand-rolled KISS client reading Dire
Wolf's port 8001 reported 0 decoded frames in runs where Dire Wolf's OWN log
shows the frames decoded perfectly. Every zero it produced is worthless.

And the positive control was available the whole time, unlooked-at: Dire Wolf's
log already contained decodes of live local stations (`KU0HN-1`, `K0LAV-6`,
audio level 198). Its receive chain was never in doubt; nobody checked.

## What is true

With the rig corrected (VFOA = 145.670, verified by a positive control in the
same session: Dire Wolf transmitted, the UV-PRO received 9/9) and Dire Wolf's
own log as the readout:

```
[0.2] KU0HN>NOCALL-1:(SABME cmd, p=1)
[0.2] KU0HN>NOCALL-1:(SABM cmd, p=1)
```

**The UV-PRO transmits, and a co-located receiver decodes it.**

### Old build vs new build: no difference

Same radio, same frequency, same audio, back to back:

| build | tncd TX | Dire Wolf decoded |
|---|---|---|
| `47656b1` (pre-2026-09-30) | 11 | 8 |
| `main`, rig control off | 11 | 7 |
| `main`, rig control ON (session settings active) | 11 | 7 |

### A real Winlink session over the UV-PRO

`main` with rig control enabled: connected to KU0HN-10, one proposal accepted,
message downloaded to 50%, then the gateway sent DISC.

The end of that session is worth recording exactly, because it says where the
limit is:

```
07:44:20  RX I[6/3]        07:44:23  RX I[6/3]        07:44:26  RX I[6/3]
07:44:21  RX I[7/3]        07:44:24  RX I[7/3]        07:44:27  RX I[7/3]
07:44:22  RX I[0/3]        07:44:25  RX I[0/3]        07:44:28  RX I[0/3]
07:44:22  TX RR[1]         07:44:25  TX RR[1]         07:44:28  TX RR[1]
                                                      07:44:28  RX DISC
```

The gateway resent frames 6, 7, 0 three times while tncd acked RR[1] each
time, then gave up.

**tncd sent what it should have, and the radio radiated it.** Counting every
supervisory frame of the session against Dire Wolf's decodes:

| N(R) | tncd logged TX | decoded off the air |
|---|---|---|
| 0 | 7 | 7 |
| 1 | 13 | 13 |
| 3 | 7 | 7 |
| 4 | 2 | 2 |
| 5 | 3 | 3 |
| 6 | 19 | 19 |

Every ack was transmitted and every one was decoded by a receiver in the same
room. The gateway nevertheless never advanced, and its own frames reached tncd
fine (116 decoded). So the limiting factor is the path from this HT to the
REMOTE gateway, in the direction away from it — not tncd's L2, not its KISS
framing, and not the radio refusing to transmit.

Note the asymmetry is expected here: the HT is a rubber duck in a concrete
basement, the gateway is a fixed station.

## Measurement rules this cost us

- **Never trust a silent result without a positive control in the same
  session, in the same direction.** "We heard nothing" is not a measurement.
- **Read the tool's own log before writing an instrument.** Dire Wolf reports
  every decode and its audio level; the custom monitor added a failure mode
  and hid the evidence.
- **`rigctl V` has a side effect.** It changes which VFO subsequent commands
  address, and it persists. Set the VFO explicitly before every `F`, and read
  back both VFOs.

## Separately: the receive audio is overdriven

Dire Wolf reports level 198-200 against the ~50 it wants, with "Audio input
level is too high" on every frame. Reducing the ALSA capture gain barely moves
it, which locates the problem upstream of the mixer:

| ALSA capture | Dire Wolf level |
|---|---|
| 35 (+23 dB) | 198 |
| 17 (+5 dB) | 132 |
| 9 (−3 dB) | 124 |

26 dB of input-gain reduction moved the reported level 198 → 124. A linear
gain stage would have given ~50, so the signal is already limited before the
sound card's gain — consistent with the radio feeding non-flat discriminator
audio (a 9600-baud-style data output) rather than filtered 1200-baud audio.
That is a front-panel menu setting on the TS-2000 and hamlib exposes no control
for it, so it cannot be checked from the host.

Capture was at 35 when this session started and is left at 9.
