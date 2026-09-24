# Rig Control OTA Checklist — Benshi rigctl (`[rigctl.N]`, `tncd rig`)

**Purpose**: Gate `feature/benshi-rig-control` for real-radio behavior, and
**re-run before every release tag** (CLAUDE.md release checklist step 3),
alongside `connect-setup-ota-checklist.md`. Rig control talks to the radio's
Benshi command protocol over the same link tncd already holds for KISS —
nothing here is reachable by a unit test or the e2e harness, which both run
against Dire Wolf, not a real Benshi radio.

Scope: BTech UV-PRO, RadioOddity GA-5WB, Vero VR-N76/VR-N7500 (Benshi
protocol only — see README). Verified against a real BTech UV-PRO.

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

- [ ] `tncd rig probe` succeeds and identifies the radio (`GET_DEV_INFO`)
- [ ] `tncd rig get-freq` returns a value matching the radio's own display
- [ ] `tncd rig set-freq <hz>` retunes the radio and the display follows,
      within a couple of seconds
- [ ] Repeated `set-freq` calls (simulating a Doppler-tracking client)
      continue to retune correctly, not just the first call
- [ ] With the radio on a **stored memory channel** (not in VFO mode —
      power-cycle or otherwise back it out of frequency mode first),
      `tncd rig get-freq` still returns that channel's frequency. This
      exercises the `READ_RF_CH` fallback path (rather than the VFO-mode
      `FREQ_MODE_GET_STATUS` cache) and confirms its request encoding is
      correct against real firmware, not just against a fake

### B. Teardown restores the prior state — NOT the band edge

The spec originally claimed an all-zero `FREQ_MODE_SET_PAR` "drops the radio
out of frequency mode and restores its normal channel state." **That is
false on real firmware**: an all-zero payload clamps the radio to 136.000 MHz
(the bottom of its tuning range) and leaves it stuck in frequency/VFO mode.
`Teardown()` was reimplemented to read the active channel's stored frequency
and set that back explicitly — confirm the fix actually holds on your unit:

- [ ] After `set-freq` followed by `tncd rig teardown`, the radio shows the
      **frequency it was on before `set-freq`**, not 136.000 MHz
- [ ] The radio is out of VFO/frequency-mode display state after teardown
      (back to showing a normal channel, if the radio distinguishes the two)
- [ ] After a **power cycle**, no memory channel has changed and the radio
      powers up on its original channel — confirms nothing was written to
      NVRAM anywhere in the get-freq/set-freq/teardown path

### C. Packet still works while rig control is in use (the real test)

This is the demultiplexer's actual proof, not the frequency math: KISS 0xC0
frames and Benshi 0xFF 0x01 frames share one RFCOMM/BLE link and must not
corrupt each other.

- [ ] Run a KISS round-trip (e.g. a Winlink CMS connect, or a simple AX.25
      UI-frame beacon watched on an independent monitor) with rig control
      **idle** — baseline, packet works with the rigctl listener merely
      running
- [ ] Issue a `set-freq` QSY mid-session
- [ ] Run a second KISS round-trip on the new frequency and confirm it
      completes cleanly, with both round-trips visible on the air on an
      independent Dire Wolf monitor (CLAUDE.md: tncd's own counters mean
      "handed to the transport", not "transmitted")
- [ ] No KISS frame corruption, dropped I-frames, or spurious retransmits
      attributable to interleaving during the QSY

### D. PAT / rigctld integration

- [ ] PAT (configured to use `rigctld` at the `[rigctl.N]` `listen_host:listen_port`)
      QSYs successfully before a connect attempt
- [ ] `dump_caps` / the initial PAT handshake does not hang or error

### E. PTT (only if testing `allow_ptt = true`)

- [ ] With `allow_ptt = false` (the default), `\set_ptt 1` against the
      rigctl port returns `RPRT -4` (not implemented) — confirms the refusal
      path, not just that PTT "does nothing"
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
