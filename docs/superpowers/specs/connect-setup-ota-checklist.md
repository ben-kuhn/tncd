# Connection-Setup OTA Checklist — relink budget + setup T1

**Purpose**: Gate `fix/connect-setup-timing` for real-radio behavior, and
**re-run before every release tag** (CLAUDE.md release checklist step 3). Both
changes alter what happens when a connect attempt is *not* answered — a path
only a real radio and a real absent station exercise, which no unit or e2E test
reaches.

Origin: 2026-09-22 field report from the "carkit" Surface Go. Connecting to
W0NE-10 on 145.030 from a parking lot with no usable path made the Bluetooth
SPP link flap for the whole attempt, while SABMEs and then SABMs went out ~13s
apart.

## What changed

1. **Relink budget** (`internal/bridge/bridge.go`, `relinkBudgetSpent`). The RX
   wedge watchdog now stops cycling the transport after `relinkEscalateAfter`
   (3) consecutive relinks that restored no traffic. Any inbound frame — or the
   session ending — restores the full budget.
2. **Connection-setup T1** (`ax25/l2/l2.go`, `setupT1`). SABM/SABME/DISC
   retransmits now use a 3s base (`[ax25] frack`, Dire Wolf's
   `AX25_T1V_FRACK_DEFAULT`) scaled `2m+1` per digipeater hop, instead of the
   data-phase T1 (~13s at 1200 baud, no digi scaling).

## Pass criteria

### A. Unreachable station must NOT flap the link (the reported bug)

Bluetooth TNC, `rx_wedge_timeout` at its default of 20. Call a station with no
path — an out-of-range callsign, or a dummy load / antenna disconnected.

- [x] At most **3** `RX wedged ... relinking` lines appear for the attempt
- [x] The 3rd is the escalated `relinking has restored no traffic; last relink`
      line, and **no further relink lines** follow for the rest of the attempt
- [x] The Bluetooth link stays up after the 3rd relink — no further
      disconnect/reconnect churn in the log or on the TNC's own indicator
- [x] The connect attempt ends in a clean `connect failed` to the client
      (PAT reports failure) rather than hanging indefinitely
- [x] Attempt duration is ~30s, not ~130s (N2=10 × 3s, not × 13s)

**PASSED 2026-09-29**, BTech UV-PRO over Bluetooth SPP, calling `KU0HN-15`
(own callsign, unused SSID, so nothing can answer). Binary `b1843fa`.

At the default `rx_wedge_timeout = 20` the attempt produced only **2** relinks
and ended cleanly at **33.1s** with `connect to KU0HN-15 timed out (no
response)`. Worth knowing: the setup-T1 fix shortens the attempt so much that
the budget's hard stop usually never engages — at 3s retries, N2=10 fits in
~33s, which is barely more than one wedge timeout. To exercise the cap itself,
re-run with `rx_wedge_timeout = 5`; that produced exactly 3 relinks, 3
`RequestDisconnection` calls, the escalated last-relink line, and then **four
more SABMs over 11s with zero further Bluetooth churn**.

A controlled before/after fell out of running the wrong binary first, on the
same radio and frequency minutes apart — worth recording since it is the
comparison this checklist exists to make:

| | pre-fix build | `b1843fa` |
|---|---|---|
| Relinks | 1,2,3,4,5,6… unbounded | 2 (or exactly 3 when forced) |
| Setup retry spacing | ~13s | 3.0s |
| Attempt length | ~130s | 33.1s |
| Outcome | still cycling | clean connect-failed |

### B. A genuine UV-PRO wedge must still be recovered (the regression risk)

**This is the criterion the budget change could break.** The UV-PRO wedges
during connection setup, sometimes after a single SABME reaches the air — the
exact shape the watchdog exists for. Run against a station that IS reachable.

- [ ] When the link wedges mid-setup, the first relink restores TX and the
      SABM/SABME reaches the air
- [ ] Once any frame is received, the relink counter resets (a later wedge in
      the same session gets a fresh budget of 3 — confirm by wedging twice in
      one session if it can be provoked)
- [ ] A connect that wedges and recovers still completes the handshake

### C. Setup retry spacing on the air

Watch with an independent Dire Wolf monitor (CLAUDE.md: `tx` counters mean
"handed to the transport", not "transmitted").

- [x] Unanswered SABME/SABM retransmits are ~**3s** apart at 1200 baud, not ~13s
- [x] The SABME→SABM downgrade still happens after 3 SABMEs (`maxV22 = N2/3`)

**PASSED 2026-09-29.** Decoded by an independent TS-2000 + Dire Wolf on the
same frequency — not read off tncd's own `tx` counter:

```
      1:Fm KU0HN To KU0HN-15 <SABME P=1 >[12:17:39]
+3.0s 1:Fm KU0HN To KU0HN-15 <SABM  P=1 >[12:17:51]
+3.0s 1:Fm KU0HN To KU0HN-15 <SABM  P=1 >[12:17:54]
+3.0s 1:Fm KU0HN To KU0HN-15 <SABM  P=1 >[12:17:57]
+3.0s 1:Fm KU0HN To KU0HN-15 <SABM  P=1 >[12:18:00]
```

tncd's own log shows the same 3.0s cadence for all 11 setup frames, with the
downgrade after exactly 3 SABMEs. The receiver missed some intermediate frames
(half-duplex DCD), which is why the first gap above reads 12s — the spacing of
what it *did* decode is what matters here.
- [ ] Faster retries do **not** cause channel congestion or collisions with the
      peer's reply on a half-duplex link — if the peer's UA is being stepped on,
      raise `[ax25] frack` and note the working value below
- [ ] Via a digipeater: retries are ~9s apart for a 1-hop path (`2m+1`)

### D. No regression in the normal path

- [ ] Full Winlink CMS round-trip still passes on at least one serial TNC and
      one Bluetooth TNC (per `TESTING.md`)
- [ ] A normal DISC/UA teardown still completes (DISC also uses the setup timer)

## Results

| Date | TNC / radio | A: no flap | B: wedge recovery | C: spacing | D: round-trip | frack used | Notes |
|------|-------------|-----------|-------------------|-----------|---------------|-----------|-------|
|      |             |           |                   |           |               |           |       |

## Sign-off

Merge gate for `fix/connect-setup-timing`: A, B and D must pass. C is
informational unless retries are observed colliding with the peer's reply, in
which case tune `[ax25] frack` and record the value.
