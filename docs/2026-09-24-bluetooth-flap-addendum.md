# Addendum to the 2026-09-24 Bluetooth flap report — code-side analysis

Companion to `2026-09-24-bluetooth-flap-report.md`. Written on the dev box against
the report's two attached journals and the tncd source at `0333204` (the build
CarKit was running). Nothing here needed new hardware — it is the report's own
evidence read against the code that produced it.

## Verdict: the storm started in Bluetooth, not on RF

The session was healthy over RF right up to the instant it died:

```
19:28:59  'Y' query: outstanding=0     <- queue fully drained; KU0HN-10's RRs were arriving
19:28:59  outstanding climbs 1->2->3->4   normal burst
19:29:06  outstanding drops to 1        <- an RR acking three frames arrived
19:29:06+ nothing, ever again
```

A failing RF path does not look like that. It degrades: partial acks, T1 expiries,
retransmissions, *some* inbound frames. This went from a successful multi-frame ack
to total RX silence in one step, and never recovered across six fresh sockets.

The Bluetooth layer was already inconsistent an hour before any of it. At `19:27:27`
tncd logged `already connected, disconnecting first` — bluez reported `Connected=True`
on an ACL with no working SPP data path. That is the same fault that later defined
the storm, present at the very first successful connect.

The confirmation is bluez contradicting itself mid-storm: tncd's `RequestDisconnection`
at `19:30:31` was answered at `19:30:33` with `Disconnecting failed: already disconnected`
and `No matching connection for device`. The transport had dropped before tncd touched
it. The wedge detector read reality correctly; it simply cannot repair a dead link by
reconnecting it.

## Correction: the relinked sockets were NOT carrying zero data

The report records every relink as "online, zero data." That is inferred from the
absence of log lines, and it is wrong.

`(*Bridge).OnKISSFrame` updates `lastRX` and clears `relinks[port]` **only after a
successful `ax25.Parse`**. A parse failure returns before both. So a socket delivering
a steady stream of non-KISS bytes is indistinguishable from a silent one as far as the
wedge detector is concerned — and at default verbosity, inbound frames are not logged
at all, so it is invisible in the journal too.

The relink counter is the tell. On `0333204`, `relinks[port]` is reset in exactly one
place — `internal/bridge/bridge.go:377`, inside `OnKISSFrame`, after a successful
parse. Nothing else touches it: `initLastRX` runs only at startup and on port
(re)wiring, `resetPortCounters` does not clear it, and `reconnectPort` does not either.
Yet the journal shows:

```
19:31:49  still wedged after 8 relinks
19:31:56  bridge: port 0 online              (relink #9's socket)
19:32:02  agwpe: CONNECT "KU0HN" -> "KU0HN-10"   pat opens a fresh session
19:32:24  RX wedged -- 23s silence ... relinking  <- ROUTINE message, so the counter was back at 1
```

The routine message only prints when the post-increment count is below
`relinkEscalateAfter` (3). Going from 8 to 1 requires a reset, and the only reset is a
successfully parsed AX.25 frame. So **at least one valid frame arrived from the radio
between 19:31:49 and 19:32:24.** Same PID (142431) throughout, so no restart explains it.

The 5-byte `b2bd7d8fe9` at `19:31:24` — seven seconds after a fresh connect — tells the
same story from the other side. The channel is not dead. It is delivering a byte stream
that mostly does not frame as KISS.

## What that does to the hypotheses

**H1** is right about the symptom but wrong about the mechanism: this is not a silent
socket, it is a socket carrying the wrong bytes.

**H2 gains a concrete mechanism and becomes the leading candidate.** The DB-50B
advertises Handsfree (`0000111e`) and Handsfree Audio Gateway (`0000111f`) alongside
SPP — tncd drops both on every single connect, and the radio keeps re-offering them.
bluez's SDP parser terminates mid-record (`sdp_extract_attr: Unknown data descriptor :
0x5/0x30 terminating`) at the exact timestamp of each relink connect. If SPP channel
resolution then falls back to something cached or never verified, you get a live RFCOMM
socket wired to the wrong service — which produces exactly "bytes that never parse as
KISS." That is a testable claim: a `btmon` capture with the SDP response in it settles
whether the channel number is resolved or guessed.

**H3 is correct and it is the part we own.** See below.

## The churn is ours, and the fix already exists

Three full ~8-relink cycles, and `reset the Bluetooth adapter or power-cycle the TNC`
is diagnostic-only — tncd never stops. Each cycle also tears down the baseband link via
`RequestDisconnection`, so if the radio's own data path could have self-recovered, we
killed it.

`fix/connect-setup-timing` addresses this with a relink budget
(`relinkBudgetSpent`, spent at 3) plus a reset when nothing is awaiting a reply.
Replaying the report's own timeline against that branch:

| Time | main (observed) | with the fix |
|------|-----------------|--------------|
| 19:29:29–19:30:09 | relinks 1, 2, 3 | relinks 1, 2, 3 — budget spent, goes quiet |
| 19:30:29–19:31:49 | relinks 4, 5, 6, 7, 8 | nothing |
| 19:30:59 pat DISCONNECT | — | `!portAwaitingReply` -> counter resets |
| 19:32:02 pat reconnects | — | fresh budget |
| 19:32:24–19:33:04 | relinks 1, 2, 3, 4, ... forever | relinks 1, 2, 3 — quiet again |

Three relinks per client session attempt instead of unbounded, and roughly six baseband
teardowns in that window instead of fourteen and climbing. It does not stop pat from
opening new sessions, so a client that retries forever still produces bursts — bounded
ones. This report is the real-world evidence for that branch, which remains OTA-pending.

## Follow-ups filed

Recorded in `docs/followups.md` on `feature/benshi-rig-control`:

- bluez cannot parse the DB-50B's SDP record; dump it raw and check whether the RFCOMM
  channel is resolved or guessed. Likely a vendor-side malformed record.
- Unpaired ports 1 and 2 are probed every 60s for the entire uptime, ~2 error lines per
  minute burying the log that mattered. Gate on the device actually being present and
  paired, or add an explicit per-port disable.
- A relinked socket is not resynchronised to a KISS `FEND` boundary, so it parses the
  tail of a torn stream. Note that the parse failure correctly does *not* reset the
  futile-relink budget — preserve that when fixing this.

## Open question the logs cannot settle

Whether the post-relink RFCOMM channel is genuinely the wrong service or the stream is
merely torn. That needs a `btmon` capture including the SDP response, which only
reproduces on CarKit with that radio.
