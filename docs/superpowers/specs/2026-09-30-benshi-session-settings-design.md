# Benshi session settings — design

**Status**: designed 2026-09-30. Record patchers, acquire/release and the
`tncd rig session-hold` diagnostic are IMPLEMENTED and validated on a UV-PRO
(2026-10-01): a full acquire-hold-release cycle left all five readable records
byte-identical to the pre-session baseline. Every measurement the design was
blocked on is done. The automatic trigger points are implemented
too: l2 reports per-port connection counts, rigctl reports connected clients and
pre-QSY, and a per-port gate reconciles the radio. The automatic path was validated on
hardware the same day: two `pat connect` attempts each logged a full acquire
reporting the ORIGINAL state, proving the release in between, and the radio was
byte-identical to baseline afterwards. **Remaining: an on-air session driven by
the automatic path that actually completes a transfer**, which needs a Benshi
radio that transmits, and the UV-PRO DOES -- confirmed 2026-10-02 once two rig
faults were fixed (see `docs/2026-10-02-uvpro-tx-confirmed.md`). A full Winlink
session ran over it with session settings active, reaching 50% of a message
before the gateway gave up, and every supervisory frame tncd sent was decoded
off the air by a local receiver. Session settings applied and restored correctly
throughout. The remaining limit is the RF path to the remote gateway, not tncd.

**Goal**: put a Benshi radio into a state where AX.25 packet actually works for
the duration of a connected-mode session, and put it back afterwards.

## Why

Two radio features actively break packet, and both are on by default for a
radio someone also uses for voice:

- **Dual watch** (`Settings.double_channel`, measured). The radio splits
  attention between two VFOs, so traffic on the other channel deafens it
  mid-frame.
- **APRS / position beaconing** -- measured: bit `0x10` of the BSS record's
  body byte 2, via `WRITE_BSS_SETTINGS`. See below.
- **APRS / position beaconing.** The radio transmits on its own schedule,
  colliding with a session in progress.

Operators already work around this by turning both off by hand before doing
packet. That is the status quo this replaces, and it is also why the failure
mode is mild: leaving them off is what the operator would have done anyway.

## Trigger points

**Apply on the first connected-mode session or the first QSY; restore once no
session and no rigctl client remain.** (Implemented 2026-10-01 in
`internal/bridge/riggate.go`.)

The distinction that matters in the implementation: a trigger is not the same as
a holder. An AX.25 session or a QSY TRIGGERS the acquire; an AX.25 session or a
connected rigctl client HOLDS it. A rigctl client is a holder but deliberately
not a trigger -- a monitoring client that connects and polls `get_freq` has no
business reconfiguring somebody's radio. An earlier revision made a bare client
connect acquire, which reconfigured the radio for a client that only ever read
from it.

Settings are per RADIO, but tncd supports several simultaneous AX.25
connections per port, so this is refcounted on the 0→1 and 1→0 transitions,
not per connection.

Deliberately NOT triggered by:

- **tncd starting.** People run tncd idle for hours — on a tablet in a car —
  while using the radio for voice. Grabbing settings at startup would break
  the radio for the 99% of the time no session exists.
- **Unproto/UI traffic.** There is no session to bound, and UI is
  fire-and-forget. A beacon that collides is a lost beacon, not a lost
  transfer.

## Restore

**Restore only fields tncd changed, and only if they still hold tncd's
value.** Re-read settings before restoring and diff. The operator may well
have flipped dual watch back on mid-session from the front panel — in a car,
that is a normal thing to do — and blindly writing a snapshot back would
silently undo them.

**Exit paths that must restore**: last session ends, port lost, rigctl client
disconnect, tncd shutdown (signal handler).

**When restore is impossible** (port dead, radio unreachable at shutdown):
retry while the process lives and the port may come back; at shutdown, log
loudly and give up.

**Writes are PERSISTENT -- measured 2026-10-01, and this changes the above.**
An earlier draft of this section reasoned that `WRITE_SETTINGS` (11) and
`STORE_SETTINGS` (12) being separate commands "strongly suggests writes are
volatile and only `STORE_SETTINGS` commits to NVRAM. If so a power cycle
self-heals." That inference was wrong. The radio was left displaced
(`channel_a` moved off a named memory to the VFO record) and power-cycled: it
came back up **in VFO mode on the same frequency**, panel identical to before
the power-off. `WRITE_SETTINGS` reaches NVRAM on its own.

So there is no free safety net. A failed restore is permanent until something
puts the radio back, and that something has to be tncd or the operator. tncd
must still never send `STORE_SETTINGS` -- but the reason is now "we do not know
what it does," not "it is the one that commits."

**This splits the three managed fields into two classes**, and the split is the
important consequence:

| field | left unrestored | acceptable? |
|---|---|---|
| dual watch off | radio stays in single-VFO | **yes** -- what the operator sets by hand for packet anyway |
| APRS off | no position beacons | **yes, with a caveat** -- benign for packet, but silently stops something the operator may expect to be running |
| `channel_a` moved to the VFO | radio no longer on their memory channel | **no** -- this is their radio reconfigured, permanently |

The first two keep the original "no on-disk snapshot" reasoning: the operator
was turning them off by hand before tncd existed, so leaving them off is a
state they would have chosen anyway, and a state file that can go stale or
outlive its config buys nothing.

**The third does not.** Leaving an operator's radio parked on a packet
frequency in VFO mode instead of the memory they had selected is not a state
they would have chosen, it is now permanent, and -- unlike the other two -- they
may not even notice until they next key up expecting their voice channel.
**Decision (2026-10-01): manage it anyway, restore best-effort, no state
file.** No separate opt-in and no snapshot. No other rig-control software is
this careful about an operator's radio, and a user who has enabled `[rigctl.N]`
has already asked tncd to retune it -- which already rewrites a channel record.
A second knob guarding one field of the same feature is ceremony that buys
nothing.

So the memory->VFO switch is managed on the same terms as the other two, and
restore is best-effort: retry while the process lives and the port may come
back, log loudly at shutdown if the radio is unreachable, and accept that a
crash or a dead port can leave the radio in VFO mode. The honest consequence is
recorded here rather than engineered away -- an operator who loses tncd
mid-session may find their radio off its memory channel, and the log is where
they will find out why.

The one thing this does buy is a hard requirement on the restore path: because
nothing else will fix it, restore must run on **every** exit path the process
can still act on -- last session ends, port lost, rigctl client disconnect,
signal handler -- not just the clean one.

## Read-modify-write, never rebuild

The settings record has ~50 fields, most of which tncd has no business
modelling. Decode what is needed, patch those bits in the raw bytes, write the
record back — exactly as `RFCh.WithFreq` does for channel records, and for the
same reason: re-serialising from a partial model silently zeroes every field
it failed to represent. On a settings record that means wiping someone's
squelch, mic gain, VOX and screen timeout.

## Memory mode -> VFO mode -> memory mode

The same mechanism covers the case the QSY guard currently refuses: a radio
sitting on a NAMED memory channel.

`Settings.channel_a` is the index of the record the active VFO points at. So
"switch to VFO mode" is just another managed settings field:

```
operator on a memory:   channel_a = 1    ("MN Pack")
tncd switches to VFO:   channel_a = 252  (the unnamed scratch record)
tncd restores:          channel_a = 1
```

**Ordering is load-bearing.** `SetFreq` rewrites whatever record the active
VFO points at, so the mode switch MUST precede it. Reversed, the QSY writes
the operator's voice memory -- precisely the outcome the name guard exists to
prevent.

On release, restore both: the VFO scratch record's frequency (the existing
`Teardown` path) and `channel_a` back to the memory.

The name-based refusal in `SetFreq` REMAINS for the unmanaged case. With
`manage_session_settings = false` tncd has no mandate to move the operator off
their memory, so refusing is still the right answer there.

## Scope: the tricky part

Dual watch and APRS are bounded by the AX.25 session. **The VFO/memory switch
is not** -- it has to happen at QSY time, which PRECEDES the session:

```
PAT:  rigctl connect -> set_freq -> AX.25 connect -> transfer -> disconnect -> QSX -> rigctl drop
                        ^^^^^^^^                                              ^^^^^
                        needs VFO mode here            ... and holds until at least here
```

So a single "session" boundary does not fit both. Proposed:

- **Acquire** managed state on the first QSY *or* the first connected-mode
  session, whichever comes first.
- **Release** when there is no active session AND no rigctl client connected.

That covers PAT's flow (the rigctl connection outlives the AX.25 session) and
the `tncd rig` CLI's one-shot flow (each invocation is its own session, as
`Teardown` already establishes). A QSY with no session and no client left
connected therefore still releases, rather than pinning the radio forever.

## Interaction with the QSY guard

`SetFreq` REFUSES in two remaining cases that managed state can turn into
transitions instead. A third refusal was deleted outright:

| Refusal | Managed behaviour |
|---|---|
| radio in dual watch | turn it off, do the work, put it back |
| radio on a named memory | switch `channel_a` to the VFO record, restore after |
| settings/status disagree | *refusal removed* -- `curr_ch_id` is measured unreliable, so this check only produced false refusals (below) |

Both remaining refusals REMAIN when `manage_session_settings = false`. The split is
deliberate: unmanaged, tncd has no mandate to move the operator's radio, so
refusing is correct; managed, the operator has asked for exactly that.

## Configuration: none — IMPLEMENTED 2026-10-01

An earlier draft proposed `[client.N] manage_session_settings = false`, opt-in
and default off, on the grounds that writing to a radio mid-session should be a
deliberate choice.

**No such setting was added.** `[rigctl.N] enabled = true` already is the
deliberate choice: a user who turns rig control on has asked tncd to retune
their radio, which already rewrites a channel record. A second knob guarding
other fields of the same feature is ceremony, and no other rig-control software
is this careful. Ports without rig control get no gate at all, so nothing
touches a radio nobody handed over.

## Dual watch: MEASURED 2026-10-01

`double_channel` IS the switch. Toggling "Radio Settings -> Dual Watch" on moved
exactly one named field:

```
double_channel   bit 10+2    0 -> 1
```

`channel_b` did NOT change (stayed 1), confirming what it is: **the channel the
B VFO watches.** It is not the enable -- but it IS the field dual watch exists
to use, which is why "channel_b is your dual watch" is a fair description of
it. BSS, advanced (29) and advanced2 (63) were all byte-identical.

### The important part: `curr_ch_id` is unreliable

`GET_HT_STATUS` moved at the same time:

```
off  00 80 c1 00 3c    double_channel=0   curr_ch_id = 15<<4|12 = 252
on   00 84 11 00 00    double_channel=1   curr_ch_id =  0<<4|1  =   1
```

**Front-panel ground truth, captured with dual watch on:** Main (the A side) was
the VFO on 145.670; the B side was "MN Pack". So:

| | settings | panel |
|---|---|---|
| `channel_a = 252` | the VFO scratch record | **Main / A**, 145.670 |
| `channel_b = 1` | memory 1 | **B**, "MN Pack" |

Both settings fields map exactly to the panel. But `curr_ch_id` reported **1**
-- the B side -- while Main was A.

So `curr_ch_id` does NOT track the Main/transmitting VFO once dual watch is on.
`Settings.channel_a` does. An earlier draft of this section had it backwards and
claimed `ActiveChannel()` would pick the wrong record; that was wrong, and is
corrected here rather than quietly deleted, because the mistake was reasoning
from the protocol field name instead of from the panel.

### The selection IS readable -- measured 2026-10-01

Toggling the active side on the front panel, dual watch on throughout:

```
A selected      -> double_channel = 1
B selected      -> double_channel = 2
dual watch off  -> double_channel = 0
```

Exactly benlink's `ChannelType{OFF=0, A=1, B=2}` -- so the enum was right, and
two earlier guesses recorded above were not. `curr_ch_id` read **1 in both
selections**, confirming it does not track the selection; `double_channel` plus
`channel_a`/`channel_b` are authoritative.

### But the answer is still "turn it OFF", not "pick a side"

Dual watch is harmful for packet **whichever side is selected**: there is one
receiver, time-slicing between two frequencies, so activity on the other side
eats part or all of an inbound frame. Selecting the right side does not help.

So the measured mapping is for **reading and restoring** state, not for
operating in dual watch:

- `Settings.ActiveChannel()` resolves all three states, so the QSY target is
  always unambiguous. Implemented, with the three captured records as golden
  fixtures.
- `SetFreq` no longer REFUSES in dual watch -- the target is known, so the tune
  is correct. It logs a warning instead, since the radio will then drop frames
  for a reason the operator may not connect to their radio settings.
- `activeChannel()` does not consult `curr_ch_id` **at all** -- see "the
  cross-check had to go" below. An earlier revision skipped it only in dual
  watch; measurement on a memory channel showed that was not enough.
- **Session setup turns dual watch off** (`double_channel = 0`) and restores
  the previous value on release -- including which side was selected. That is
  the actual fix; the mapping above is what makes the restore faithful.
- Restoring still means re-reading rather than predicting, since the radio
  demonstrably moves `curr_ch_id` when the setting changes.

### The restore design is validated by a round trip

Dual watch was taken off -> A -> B -> off across the captures above. On
returning to off, **all five records were byte-identical to before the
sequence** -- settings, BSS, advanced (29), advanced2 (63) and
`GET_HT_STATUS`. Nothing drifted and nothing was left behind.

That is direct evidence for the central assumption of this design: writing a
saved value back really does return the radio to its prior state, so a
restore is faithful rather than approximate. It also means the only field the
session needs to touch for dual watch is `double_channel`; the radio handles
the rest, including putting `curr_ch_id` back.

## The cross-check had to go -- MEASURED 2026-10-01

With dual watch OFF, Main was switched from the VFO to memory 5 on the front
panel:

```
settings  channel_a: 252 -> 5        (only field that moved)
status    00 80 c1 00 3c -> UNCHANGED, curr_ch_id still 252
```

Re-read 5 seconds later: still 252. It is not lag.

**Which one is right, settled by reading the records themselves:**

```
ch   1: name="MN Pack"  rx=145670000
ch   5: name="AUS 730"  rx=145730000   <- panel showed AUS 730
ch 252: name=""         rx=145670000   <- the unnamed VFO scratch record
```

The panel showed "AUS 730". `channel_a = 5` names exactly that record.
`curr_ch_id = 252` named the VFO the radio had left. **`channel_a` is the
authority; `curr_ch_id` is not.**

So `curr_ch_id` disagreed with `channel_a` in *both* states where the two could
differ -- dual watch, and memory mode -- and `channel_a` was right both times.
A field that is only correct in the one case where nothing can go wrong is not
a cross-check.

### What it cost

The live consequence, measured against the radio before the fix:

```
$ tncd rig get-freq
rig: ... settings and status disagree about the active channel (settings say 5, status says 252)
$ tncd rig set-freq 145030000
rig: ... settings and status disagree about the active channel (settings say 5, status says 252)
```

Both commands failed on a radio sitting on a memory channel -- the single most
common state an operator's radio is in. Worse, it **masked `ErrChannelMode`**,
the guard that exists precisely for this case and tells the operator what to do
about it.

After removing the cross-check, same radio, same position:

```
$ tncd rig get-freq
145730000
$ tncd rig set-freq 145030000
rig: radio is on a named memory channel, not its VFO: channel 5 is "AUS 730"
     -- switch the radio to VFO/frequency mode first
```

`GetFreq` answers correctly from the memory record, and the QSY refusal is the
actionable one.

`currChannel()` and `ErrVFOAmbiguous` are deleted rather than left unused, and
`TestActiveChannelNeverReadsHTStatus` asserts the request is never sent, so the
check cannot creep back on the strength of the field's name. The nibble-packing
decode it contained (`curr_ch_id = upper<<4|lower`, from the StatusExt trailing
word) is recorded here in case the field is ever needed for something it is
actually good for.

**Consequence for this design:** the memory -> VFO switch is now the *only* way
a managed session can QSY a radio parked on a memory, because the name guard is
reachable again and correctly refuses. That makes the `channel_a` write in
"Memory mode -> VFO mode -> memory mode" load-bearing rather than a nicety.

## APRS: MEASURED 2026-10-01

**APRS is not in `READ_SETTINGS` at all.** The settings record is byte-identical
with APRS on and off, which rules out every field guessed at earlier
(`auto_share_loc_ch`, `gpwpl_upload_en`, `positioning_system`). `SET_APRS_PATH`
is also not it -- that sets the digipeater path.

It lives in the **BSS record** (`READ_BSS_SETTINGS` 33 / `WRITE_BSS_SETTINGS`
34), which is Benshi's position/beacon subsystem -- the record also carries the
`"APRS"` destination and the operator's symbol and callsign (`/[KU0HN`).

```
APRS enable = bit 0x10 of READ_BSS_SETTINGS body byte 2
              (payload byte 1, i.e. after the reply-status byte)

on   0x1c = 0001 1100
off  0x0c = 0000 1100
```

Confirmed across two transitions in both directions. **One bit moved and
nothing else did** -- `settings`, `advanced` (29), `advanced2` (63) and
`GET_HT_STATUS` were all byte-identical throughout.

Consequences for this design:

- Disabling APRS needs `WRITE_BSS_SETTINGS` (34), **not** `WRITE_SETTINGS`.
  Two managed records, not one.
- Read-modify-write matters even more here: the BSS record holds the
  operator's callsign and symbol, so rebuilding it from a partial model would
  destroy their APRS identity, not just a preference.
- The control is labelled "Digital Mode -> Enable" on the radio, so this bit
  may gate more than position beaconing. What is measured is the bit's
  correlation with that toggle; its full meaning is inferred from a menu
  label. Worth understanding before tncd flips it for every session -- if it
  also disables something wanted during packet, the tradeoff changes.

`kiss/` note: nothing in the capture suggested a separate volatile/stored
split for BSS, so whether `WRITE_BSS_SETTINGS` persists across a power cycle
is still unverified, same as `WRITE_SETTINGS`.

## Both managed writes are validated on hardware -- 2026-10-01

The design assumes tncd can read-modify-write these two records and put them
back faithfully. All three parts of that are now measured on the UV-PRO, each
probe restoring unconditionally and comparing byte-for-byte.

**`WRITE_SETTINGS` (11) accepts a full record.** An identity write -- read the
22-byte record, write the same bytes back -- returned status 0 and left the
record unchanged. The command takes the whole record, so read-modify-write is
the available shape, exactly as `RFCh.WithFreq` is for channels.

**Dual watch really is writable, not just readable.**

```
before      5104a6...  double_channel=0
write A     5114a6...  double_channel=1   <- one byte changed, 04 -> 14
restore     5104a6...  double_channel=0   byte-identical
```

**The APRS bit is writable, and the BSS record survives intact.**

```
before   00 1c 80 1e ... 2f5b4b5530484e00   aprs on   ("/[KU0HN")
cleared  00 0c 80 1e ... 2f5b4b5530484e00   aprs off  <- one bit
restore  00 1c 80 1e ... 2f5b4b5530484e00   byte-identical
```

The callsign and symbol at the tail came back untouched, which is the specific
disaster read-modify-write exists to prevent here.

So the write path is no longer an assumption. What remains unverified needs
hands at the radio:

- **Does behaviour follow the record?** These probes confirm the radio *reports*
  what was written. That the receiver actually stops time-slicing, and the
  beacon actually stops, is still inferred from the menu the bit tracks.
- **`WRITE_BSS_SETTINGS` volatility.** `WRITE_SETTINGS` is now measured
  persistent (see "Restore"); whether the BSS record behaves the same way was
  not tested separately, though there is no reason to expect it differs.

The probes live in `internal/rig/wsprobe_test.go`, skipped unless
`TNCD_HW_BDADDR` is set, and each skips rather than guesses if the radio is not
in the state it needs (dual watch off / APRS on).

## Volatility: MEASURED 2026-10-01 -- writes persist

The test that settled it, run on the UV-PRO:

```
1. radio on memory 5 ("AUS 730", 145.730), panel showing it
2. tncd writes channel_a = 252     -> VFO mode, record 252 holds 145.670
   tncd rig set-freq 145670000     -> QSY through the production path
3. POWER CYCLE
4. panel: VFO mode, 145.67 -- identical to step 2
```

`WRITE_SETTINGS` survives a power cycle with no `STORE_SETTINGS`. The design
consequences are in "Restore" above.

Two incidental results from the same run:

**Nothing leaked from the earlier round-trip probes.** A power cycle taken
*before* this test, with everything restored, read back `settings` and `bss`
byte-identical to their pre-probe values, and `ch 5` still held "AUS 730" on
145.730. The restore paths are clean across a power cycle, not just within a
session.

**`curr_ch_id` trails one change behind, which explains every earlier
reading.** Across this sequence:

| event | `channel_a` | `curr_ch_id` |
|---|---|---|
| after power cycle, on memory 5 | 5 | 0 |
| after writing 252 | 252 | 5 |
| (earlier) on memory 5, after VFO | 5 | 252 |
| (earlier) dual watch on | 252 | 1 |

It consistently reports the *previously* selected channel. That is a mechanism,
not just a correlation, and it retires the field for this purpose: a lagging
value can never be a cross-check on the live one.

## Testing

- Golden-byte tests for the settings patcher, from real captured records,
  asserting every unmodified byte survives.
- A fuzz target on any new parser.
- Refcount tests: two overlapping sessions apply once and restore once.
- Ordering test: the VFO-mode switch precedes the QSY write, so a radio on a
  named memory never has that memory rewritten.
- Scope test: a QSY with no AX.25 session still releases once the rigctl
  client disconnects.
- A test that restore is skipped for a field the operator changed underneath.
- OTA: **partly done 2026-10-01** -- see `docs/2026-10-01-session-settings-ota.md`.
  Writes are confirmed PERSISTENT, not volatile. Acquire and restore were
  validated from the worst starting state (dual watch on, both VFOs on named
  memories, APRS on) and every record came back byte-identical. The UV-PRO does
  not transmit (its own firmware bug, isolated by a modem swap in that report), so
  nothing TX-side can be validated on that radio -- but "does packet behave better
  with dual watch off" is not among the things needing validation: one receiver
  time-slicing two frequencies drops frames by construction.
