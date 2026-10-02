# Winlink uploads corrupted by premature retransmission — 2026-10-02

**Symptom**: a Winlink upload over a 1200-baud link fails at the CMS with

```
*** Error check failed on receiving B2 message.
    [Check sum failure - failed to assemble correct binary image]
```

**Cause**: tncd retransmitted I-frames before the first window had finished
transmitting. A mod-8 receiver whose N(R) had already advanced counted those
retransmissions as NEW data, so the assembled message came out longer than what
was sent.

**Measured at the gateway** (its own `KISS Session Stats`), for an 8,175-byte
compressed message:

| build / config | large uploads passed | bytes the gateway received |
|---|---|---|
| `main`, `frack=3` (default) | 1 of 6 | 10271, 10272, 10275, 10289 |
| `main`, `frack=13` (by hand) | 2 of 2 | 8476, 8478 |
| pre-benshi (`2251061`) | 4 of 4 | 8474, 8476, 8491, 8491 |
| `main` + fix 1 | 2 of 3 | 8478, 8479, **10098** |
| `main` + both fixes | 3 of 3 | 8484, 8485, 8489 |

~1,800 extra bytes is about seven 254-byte frames counted twice.

## Why it survived three weeks of testing

**In KISS mode the TNC computes the AX.25 FCS.** Duplicated data therefore
arrives with a perfectly valid FCS and is indistinguishable from good data at
the link layer. Nothing below the application notices. Only Winlink's own B2F
checksum catches it, so it takes a multi-frame binary transfer on a slow link to
surface at all — short frames, fast links and the stronger Dire Wolf/TCP path all
pass.

## The two defects

**1. The setup timer leaked into the data phase.** `T1Setup` (FRACK, default 3s)
is sized for a handshake: a ~17-byte command and a ~17-byte UA, a quarter second
of air time. `t1Value` was never handed back to the data-phase T1 when a link
established, so the transfer began with a 3s retransmit timer while one window of
254-byte I-frames takes ~6s just to transmit at 1200 baud. Fixed by routing every
establishment path through `enterConnected`.

**2. The RTT estimator's floor ignored the link speed.** With the first defect
fixed, `updateSRTT` still drove T1 back down to a flat 3s, so the duplication
recurred mid-transfer. The samples driving it down are misleading: RTT is measured
on whatever gets acked, and the small FC/FS exchanges around a Winlink transfer
return in about a second, so the estimator sized the data phase from handshake
traffic. `DeriveParams` already computed the right quantity, so the floor now
comes from the port — never shorter than the time to put the outstanding window
on the air plus one turnaround.

```
 1200 baud: T1=13.04s  floor 3s -> 6.52s
 9600 baud: T1= 3.38s  floor 3s (unchanged)
19200 baud: T1= 3.00s  floor 3s (unchanged)
```

Only slow links change.

## How it was found, including the wrong turns

Worth recording, because three of the four isolations attempted were wrong and
each was wrong in an instructive way.

**The radio was not the problem, twice over.** The investigation began on a
UV-PRO, which has its own unrelated transmit fault, and a day was lost to
reporting that radio as broken on the strength of measurements taken through a
mis-tuned receiver and a hand-rolled monitor that produced false zeros. The
regression only became visible on a Mobilinkd TNC4 — a radio the operator
reported had *never* stalled — which is what made a build A/B meaningful.

**"It's the demux" was wrong.** Bypassing the RX demultiplexer made a failing
run pass, which looked like a clean isolation. It was a single run. Replaying
1,307 bytes captured from a *failing* session through both the demux and a bare
decoder produced 57 frames identical at every read size, which killed the
mechanism. One run is not an isolation.

**"It cannot be T1" was also wrong.** Reading the code, T1 looked irrelevant:
`updateSRTT` drives both builds to the same floor after the first ack, so the
initial value should not matter. A `frack=13` config test disproved that
immediately — the window before the first RTT measurement is exactly when the
bulk transfer starts. **The experiment was worth more than the analysis.**

**"It is deterministic" was wrong.** Two runs with byte-identical counters looked
deterministic. The next run passed. The failure rate is ~83%, not 100%, and
sample sizes of two produce confident nonsense in both directions.

**What actually worked** was the gateway's own byte counter. `Bytes Received`
versus message size is a direct, independent measure of duplication that needs no
inference and no trust in either endpoint's logs. Everything upstream of that —
frame counts, wedge counts, PAT's exit code — was ambiguous or misleading.

## Separately: PAT exit codes do not classify failures

The first harness scored any non-zero PAT exit as a transport failure, which
conflated a gateway-side B2F rejection with a lost connection. The operator
spotted it. Classify from the PAT transcript (`Check sum failure` vs
`connection lost` vs `Disconnected`), not from the exit status.
