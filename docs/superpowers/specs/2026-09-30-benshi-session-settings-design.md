# Benshi session settings — design

**Status**: designed 2026-09-30, NOT implemented. One question is blocked on a
bench measurement (see "Open: what APRS actually is").

**Goal**: put a Benshi radio into a state where AX.25 packet actually works for
the duration of a connected-mode session, and put it back afterwards.

## Why

Two radio features actively break packet, and both are on by default for a
radio someone also uses for voice:

- **Dual watch** (field NOT yet confirmed -- see below). The radio splits
  attention between two VFOs, so traffic on the other channel deafens it
  mid-frame.
- **APRS / position beaconing.** The radio transmits on its own schedule,
  colliding with a session in progress.

Operators already work around this by turning both off by hand before doing
packet. That is the status quo this replaces, and it is also why the failure
mode is mild: leaving them off is what the operator would have done anyway.

## Trigger points

**Apply on the first connected-mode session; restore when the last one ends.**

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
loudly and give up. **No on-disk snapshot.** The operator was turning these
off by hand before tncd existed, so a radio left in packet-ready state is the
state they would have chosen anyway — not worth a state file that can go
stale, describe a radio that has since been reconfigured, or outlive the
config that produced it.

`WRITE_SETTINGS` (11) and `STORE_SETTINGS` (12) being separate commands
strongly suggests writes are volatile and only `STORE_SETTINGS` commits to
NVRAM. If so a power cycle self-heals, and tncd must never send command 12.
**Unverified — see below.**

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

`SetFreq` currently REFUSES in three cases that managed state can turn into
transitions instead:

| Refusal | Managed behaviour |
|---|---|
| radio in dual watch | turn it off, do the work, put it back |
| radio on a named memory | switch `channel_a` to the VFO record, restore after |
| settings/status disagree | still refuse -- this is a state tncd does not model |

All three refusals REMAIN when `manage_session_settings = false`. The split is
deliberate: unmanaged, tncd has no mandate to move the operator's radio, so
refusing is correct; managed, the operator has asked for exactly that.

## Configuration

Opt-in, per port, default off. Everything else in this area refuses rather
than clobbers; writing to a radio mid-session is a step beyond that and should
be a deliberate choice. It also lets this ship before it is proven on every
Benshi variant.

Proposed: `[client.N] manage_session_settings = false`.

## Open: which field is dual watch

Two candidates, and the evidence is suggestive rather than conclusive.

Measured on a UV-PRO 2026-09-29: `channel_a = 252`, `channel_b = 1`,
`double_channel = 0`. So `channel_b` pointed at a real memory ("MN Pack")
while `double_channel` read OFF. If `channel_b` being set were what enables
dual watch, that radio would have been dual-watching at the time.

That points at `double_channel` as the switch and `channel_b` as merely what
the B VFO is tuned to, persisting whether or not dual watch is active.

**Not proven.** The front-panel state at the moment of that capture was not
recorded, and in the `Status` record `double_channel` is documented as which
channel is currently ACTIVE in dual watch -- a different thing from the
setting that enables it. Same field name, possibly different meaning in the
two records.

Settled by the same diff as APRS: toggle dual watch on the panel, read
settings before and after, diff the raw bytes.

## Open: what APRS actually is

`double_channel` is known. **APRS is not.** The plausible fields are
`auto_share_loc_ch` (+`auto_share_loc_ch_upper`), `gpwpl_upload_en` and
`positioning_system`, and which combination the radio's own APRS toggle drives
is a guess.

`SET_APRS_PATH` (71) / `GET_APRS_PATH` (72) are NOT the lever -- they set the
digipeater path (`WIDE2-1` and so on), not whether APRS is enabled.

**Do not guess.** `support_vfo` reads 0 on a radio operating in VFO mode and
`channel_count` reads 30 on a radio whose VFO is channel 252 — the field names
in this protocol mislead, repeatedly.

**The measurement**, covering every open field at once: capture
`READ_SETTINGS` after each front-panel change and diff the raw bytes.

| Toggle on the panel | Settles |
|---|---|
| APRS on -> off | which fields APRS actually drives |
| dual watch on -> off | `double_channel` vs `channel_b` |
| memory -> VFO mode | that `channel_a` really is the mode switch |

The diff also catches anything else the radio moves that nobody thought to
look for, which is the real value -- every field-name guess this week has been
wrong. Five minutes at the bench, and `tncd rig` already has the plumbing to
dump the record.

Until that exists, the dual-watch half is implementable and the APRS half is
not.

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
- OTA: confirm writes are volatile (write, power cycle, verify reverted), and
  that a session actually behaves better with dual watch off.
