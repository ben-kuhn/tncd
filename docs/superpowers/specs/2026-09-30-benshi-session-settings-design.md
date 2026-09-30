# Benshi session settings — design

**Status**: designed 2026-09-30, NOT implemented. One question is blocked on a
bench measurement (see "Open: what APRS actually is").

**Goal**: put a Benshi radio into a state where AX.25 packet actually works for
the duration of a connected-mode session, and put it back afterwards.

## Why

Two radio features actively break packet, and both are on by default for a
radio someone also uses for voice:

- **Dual watch** (`Settings.double_channel`). The radio splits attention
  between two VFOs, so traffic on the other channel deafens it mid-frame.
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

## Interaction with the QSY guard

`SetFreq` currently REFUSES when the radio is in dual watch, because which VFO
transmits is not derivable. Once tncd manages dual watch for a session, that
refusal can become "turn it off, do the work, put it back" — the guard stops
being a dead end. The refusal must remain for the unmanaged case.

## Configuration

Opt-in, per port, default off. Everything else in this area refuses rather
than clobbers; writing to a radio mid-session is a step beyond that and should
be a deliberate choice. It also lets this ship before it is proven on every
Benshi variant.

Proposed: `[client.N] manage_session_settings = false`.

## Open: what APRS actually is

`double_channel` is known. **APRS is not.** The plausible fields are
`auto_share_loc_ch` (+`auto_share_loc_ch_upper`), `gpwpl_upload_en` and
`positioning_system`, and which combination the radio's own APRS toggle drives
is a guess.

**Do not guess.** `support_vfo` reads 0 on a radio operating in VFO mode and
`channel_count` reads 30 on a radio whose VFO is channel 252 — the field names
in this protocol mislead, repeatedly.

**The measurement**: read `READ_SETTINGS`, toggle APRS on the radio's front
panel, read again, diff the raw bytes. Repeat for dual watch to confirm
`double_channel`, and the diff will also catch anything else the radio moves
that nobody thought to look for. Five minutes at the bench.

Until that exists, the dual-watch half is implementable and the APRS half is
not.

## Testing

- Golden-byte tests for the settings patcher, from real captured records,
  asserting every unmodified byte survives.
- A fuzz target on any new parser.
- Refcount tests: two overlapping sessions apply once and restore once.
- A test that restore is skipped for a field the operator changed underneath.
- OTA: confirm writes are volatile (write, power cycle, verify reverted), and
  that a session actually behaves better with dual watch off.
