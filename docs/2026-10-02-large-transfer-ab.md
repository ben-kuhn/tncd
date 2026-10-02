# Large-transfer A/B over the UV-PRO — 2026-10-02

**Question**: did the Benshi rig-control / session-settings work regress large
transfers over the UV-PRO?

**Answer**: no detectable difference. The pathology is identical with that code
absent, present but idle, and fully active, and the spread WITHIN an arm is
larger than anything between arms.

## Method

Three arms, interleaved `pre -> main -> rig` twice so radio drift could not
favour one, 6 runs plus a priming session:

| arm | build | rig control |
|---|---|---|
| `pre` | `2251061`, 2026-09-22 — before any `benshi/` file existed | n/a |
| `main` | current main | off (no Gaia traffic at all) |
| `rig` | current main | on (Gaia + session settings active) |

Each run: one Bluetooth connect, one ~10 KB incompressible message queued for
upload, one Winlink session to KU0HN-10, then a 120 s cooldown because this
radio's degraded state persists for a minute or two.

Two deliberate details. A payload is composed ONLY when the outbox is empty, so
a failed run reuses its queued message instead of stacking another 10 KB and
making the arms incomparable. Downloads are counted from the inbox, not PAT's
stdout, because a transfer that dies mid-message still prints "Receiving".

**The metric is not "did it connect".** It is how late the gateway saw each
frame: tncd's `[TX]` timestamp is when it handed the frame to the radio, and the
gateway's own Dire Wolf timestamp is when it reached the air. The gap decides
whether a transfer can survive FRACK (8000 ms here) at all. Clocks were verified
in sync to 20 ms.

## Results

| run | arm | tncd TX | gateway decoded | median lag | max lag | >1 s late | outcome |
|---|---|---|---|---|---|---|---|
| prime | pre | 20 | 12 | 3.1 s | 115 s | 11/11 | connection lost |
| 1 | pre | 12 | 7 | **205.3 s** | 231 s | 6/6 | connect timeout |
| 2 | main | — | — | — | — | — | port never came online |
| 3 | rig | 40 | 18 | 3.0 s | 112 s | 17/17 | connection lost |
| 4 | pre | 40 | 9 | 1.5 s | 98 s | 7/8 | connection lost |
| 5 | main | 39 | 12 | 2.9 s | **10.2 s** | 10/11 | connection lost |
| 6 | rig | 11 | 0 | — | — | — | connect timeout |

**Zero messages moved in either direction in any run.**

The worst run is `pre` and the best is `main`. `pre` alone spans 1.5 s to 205 s.
There is no arm ordering to read here.

What every run shares, and what actually matters:

- the gateway decodes only a MINORITY of what tncd transmits — 7/12, 18/40,
  9/40, 12/39, 0/11
- what does arrive is seconds to minutes late, in bursts ~128 ms apart
- run 1's frames reached the air at 09:33:39, **88 seconds after that run had
  already timed out**

## Limits of this result

- Two runs per arm is thin. Enough to say "no large effect", not enough to
  exclude a small one.
- The frame matcher pairs on type plus sequence against the nearest preceding
  TX. The effect size swamps that heuristic, but the medians should not be read
  to three significant figures.

## An instrument failure worth recording

The harness's per-run gateway capture produced EMPTY files -- broken `date`
arithmetic in the ssh command -- so the first pass reported "gateway decoded 0"
for every run. Read naively that says nothing was transmitted, which is the same
wrong conclusion this bench produced yesterday for a different reason.

It was recoverable only because the gateway's journal is persistent: the window
was re-pulled afterwards and showed 110 of our frames decoded.

**Rule: check that a measurement captured anything before interpreting a zero.**
Three separate instruments produced false zeros on this bench in two days -- a
hand-rolled KISS monitor, a mis-tuned VFO, and now an empty log capture. Each
one, taken at face value, pointed at the radio.
