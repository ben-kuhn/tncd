# Rig Control OTA Checklist — Benshi rigctl (`[rigctl.N]`, `tncd rig`)

**Purpose**: Gate `feature/benshi-rig-control` for real-radio behavior, and
**re-run before every release tag** (CLAUDE.md release checklist step 3),
alongside `connect-setup-ota-checklist.md`. Rig control talks to the radio's
Benshi command protocol over the same link tncd already holds for KISS —
nothing here is reachable by a unit test or the e2e harness, which both run
against Dire Wolf, not a real Benshi radio.

Scope: BTech UV-PRO, RadioOddity GA-5WB/DB-50B, Vero VR-N76/VR-N7500 (Benshi
protocol only — see README, whose radio list this must match).

**Only the UV-PRO has actually been verified.** The others are listed because
they share the Benshi protocol, which is an inference, not a test result — and
one worth treating carefully here specifically: the VFO channel-id floor
(`vfo_channel_min`, default 251) was derived from a UV-PRO's channel map, and
nothing confirms other variants number their VFOs the same way. On a new
model, run section A first and read what a refusal names.

## Before you start

- Radio powered on, paired/trusted, and configured as a normal `[client.N]`
  KISS port (Bluetooth Classic SPP or BLE — rig control rides the same link,
  there is no separate control-channel config any more; see "Corrections" in
  the design spec if you're wondering why not).
- `[rigctl.N]` enabled for that port's index, `allow_ptt = false` to start.
- Note the frequency and **active memory channel** shown on the radio's own
  display before touching anything, so "restored cleanly" has a baseline.
- Run everything at max verbosity (`tncd -v`) per the user's standing
  preference, and log to a file you can go back and read.

## Standing caution: a fake that models intent, not the device, proves nothing

This feature's test suite went green three separate times while being wrong,
because a test or fake encoded what we *meant* to happen instead of what the
radio actually does on the wire:

- a frequency literal that was wrong by 21.7 kHz and passed every unit test;
- an error matcher that looked for `"not supported"` with a space, when the
  radio's actual error string is `br-connection-not-supported` (hyphens);
- a PTT fake that stored whatever argument `SetPTT` was called with, so
  `SetPTT(true)` then `SetPTT(false)` read back as correct regardless of what
  went out over the air — when the real radio **toggles** on every call, with
  no separate on/off parameter.

None of these are things more unit tests would have caught: the fakes were
internally consistent, just consistent with the wrong model. Only a real
radio, watched, tells you whether the wire behavior matches the code's belief
about it. Treat every item below as needing an eyes-on-the-radio confirmation,
not just an `RPRT 0`.

## Pass criteria

### A. Basic frequency control

- [x] `tncd rig probe` succeeds (`GET_DEV_INFO`) — 2026-09-29. Note it does
      **not** identify the radio: it checks the reply status and prints the
      literal `radio answered`. Nothing decodes the device info. The wording
      here and in README overstated it.
- [x] `tncd rig get-freq` returns a value matching the radio's own display —
      2026-09-29, confirmed against the front panel by the operator
- [x] `tncd rig set-freq <hz>` retunes the radio and the display follows —
      2026-09-29, confirmed against the front panel in both directions
      (145.670 -> 145.030 -> 145.670)
- [x] Repeated `set-freq` calls (simulating a Doppler-tracking client)
      continue to retune correctly — 2026-09-29, four consecutive retunes
      (145.030, 145.050, 145.070, 145.670) each read back correctly
- [ ] With the radio on a **named stored memory channel**, `tncd rig get-freq`
      still returns that channel's frequency — `GetFreq` always reads the
      active VFO's channel record (`READ_SETTINGS` → `GET_HT_STATUS` →
      `READ_RF_CH`; there is no second path and no notification cache), so
      this confirms that request encoding against real firmware rather than
      against a fake
- [ ] On that same named memory channel, `tncd rig set-freq <hz>` **refuses**
      with a message naming the channel, and the memory is unchanged
      afterwards. This is the guard that stops rig control overwriting a
      memory you programmed; verify it on the radio, not just in tests
- [ ] With the radio in **dual watch**, `set-freq` refuses rather than
      guessing which VFO transmits

### B. Teardown restores the prior frequency

`Teardown` restores a saved copy of the record this session's **first**
`set-freq` displaced. Note that each `tncd rig` invocation is its own session,
so a standalone `tncd rig teardown` has nothing to restore and will say so —
exercise this through the **rigctld server**, which holds one session for the
life of the connection, or accept that the CLI cannot test it.

Do NOT expect an "exit VFO mode" command to exist. The spec originally claimed
an all-zero `FREQ_MODE_SET_PAR` payload does that; on real firmware it clamps
the radio to 136.000 MHz, the bottom of its tuning range. tncd never sends it.

- [x] Through a rigctl client: `set_freq`, then drop the connection —
      **this item was written wrong and is corrected here.** tncd does NOT
      restore on disconnect, deliberately: `internal/rig`'s package doc says
      so ("rigctld leaves a radio wherever it was last tuned on disconnect,
      so restore-on-exit was never a requirement here, and this package does
      not fake one"), and nothing in `internal/frontend/rigctl` calls
      `Teardown`. Verified 2026-09-29: after `F 145670000` and dropping the
      connection, the radio stayed on 145.670.
      **The client owns the restore, and PAT does it** — see section D, where
      PAT emitted `QSX ax25+agwpe: 145670.000` after its session and the radio
      followed. So the design is right and matches real rigctld; what needed
      fixing was this checklist.
- [ ] `tncd rig teardown` on its own prints that there is nothing to restore,
      rather than silently doing nothing or claiming success
- [ ] After a **power cycle**, the VFO holds whatever it was last set to and
      **no named memory channel has changed** — QSY writes the VFO's own
      channel record, so this is the check that the guards kept it away from
      everything else

### C. Packet still works while rig control is in use (the real test)

This is the demultiplexer's actual proof, not the frequency math: KISS 0xC0
frames and Benshi 0xFF 0x01 frames share one RFCOMM/BLE link and must not
corrupt each other.

- [x] Baseline round-trip with rig control idle — 2026-09-29
- [x] `set-freq` QSY mid-session — via the rigctl port, `RPRT 0`, read back
- [x] Second round-trip on the new frequency, both seen on the air by an
      independent Dire Wolf/TS-2000 (retuned to follow)
- [x] No corruption, dropped frames or spurious retransmits

**PASSED 2026-09-29.** Both beacons decoded off air by the independent
receiver, with a rigctl QSY between them:

```
145.670   "SECC-BASE on 145.670"   <- decoded off air
F 145030000 -> RPRT 0, get_freq reads 145030000
145.030   "SECC-QSY on 145.030"    <- decoded off air
```

tncd's log over the whole run: zero parse failures, zero oversize drops, zero
retransmits, zero wedges. This is the demultiplexer's real proof — KISS
`0xC0` frames and Benshi `0xFF 0x01` frames shared one RFCOMM link across a
live QSY without corrupting each other.

### D. PAT / rigctld integration

- [x] PAT QSYs successfully before a connect attempt — 2026-09-29
- [x] The initial PAT handshake does not hang or error

**PASSED 2026-09-29.** PAT pointed at `127.0.0.1:4534` via `hamlib_rigs`,
with `ax25.rig` set to that entry (note: it is the **transport's** `rig` key
that matters, not `agwpe.rig` — setting only the latter gives
`Unable to QSY: hamlib rig '<other>' not loaded`, which is a confusing way to
learn that):

```
UV-PRO ready. Dial frequency is 145.670.00 MHz.
AGWPE TNC (2.0) initialized
QSY ax25+agwpe: 145030
Connecting to KU0HN-15 (ax25+agwpe)...
Unable to establish connection to remote: *** connect to KU0HN-15 timed out
QSX ax25+agwpe: 145670.000
```

The connect failure is expected (`KU0HN-15` does not exist). What matters is
the QSY, the clean handshake, and the `QSX` restore afterwards — the radio was
confirmed back on 145.670 via `get_freq`.

### E. PTT (only if testing `allow_ptt = true`)

**PTT DOES NOT WORK on BTech UV-PRO firmware 146 (measured 2026-09-30).**
The radio accepts a well-formed `DO_PROG_FUNC(MAIN_PTT)` and never transmits.
Confirmed two independent ways: its own `is_in_tx` bit stays 0, and a TS-2000
on the same frequency stayed at its -54 noise floor — the same meter having
peaked at +60 for a known-good UI frame minutes earlier, so the instrument was
working. `button_id` 0-3 and 15 were swept with LOW_TO_HIGH/HIGH_TO_LOW; all
accepted, all silent.

**The firmware is not the limitation** -- the operator keys this radio from the
vendor's own app. The unexplored leads are the radio's lock state (`UNLOCK` is
command 65) and the possibility that the vendor app keys by opening an audio
stream rather than sending a command. See `docs/followups.md` #17.

The probing did fix two real bugs — see the commit: tncd was sending a
malformed one-byte body that the radio REJECTED with status 5 every time, and
`SetPTT` discarded the reply so the rejection was invisible.

Items below are kept for a radio or firmware that does support it.

- [x] With `allow_ptt = false` (the default), `\set_ptt 1` against the
      rigctl port returns `RPRT -4` (not implemented) — confirms the refusal
      path, not just that PTT "does nothing". **PASSED 2026-09-30**, both
      `\set_ptt 1` and the short form `T 1`.
- [ ] With `allow_ptt = true`, **radio connected to a dummy load**:
      `\set_ptt 1` keys the transmitter (confirm on a separate receiver or
      power meter, not just by trusting the reply)
- [ ] `\set_ptt 0` unkeys it
- [ ] With `allow_ptt = true` into a dummy load: key the transmitter, then
      kill the rigctl client mid-key (e.g. `kill -9` the PAT/nc process) —
      confirm `ptt_timeout` force-releases it and the transmitter goes quiet
      within the configured window, with no further client interaction
- [ ] Pulling the Bluetooth link while keyed does not leave the radio keyed
      longer than the radio's own `tx_time_limit` backstop

## Operational warning — read before you start debugging tncd

**This radio's internal TNC wedges after heavy Bluetooth connect/disconnect
churn.** KISS goes completely silent — no frames in, no frames out — while
the Bluetooth link itself, tncd's port state, and every health indicator
tncd can see all continue to report healthy. There is no tncd-visible signal
that distinguishes "the radio's TNC is wedged" from "nothing is happening on
the air." This cost hours to diagnose during development.

**Power-cycle the radio before debugging tncd** if KISS traffic stops making
sense partway through a rig-control test session, especially after several
back-to-back `probe`/`get-freq`/`set-freq`/`teardown` runs or reconnects. Do
this *first*, before reading logs or suspecting the demultiplexer.

## Results

| Date | Radio / firmware | A: freq control | B: teardown | C: packet+QSY | D: PAT | E: PTT | Notes |
|------|-------------------|-----------------|-------------|----------------|--------|--------|-------|
|      |                   |                 |             |                |        |        |       |

## Sign-off

Merge/release gate: A, B, C and D must pass. E is required only when shipping
with `allow_ptt = true` enabled by default for a given deployment (it stays
`false` in the shipped example config); if E is skipped, record that PTT was
not exercised this run.
