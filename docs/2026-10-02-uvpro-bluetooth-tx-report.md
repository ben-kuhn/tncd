# UV-PRO: frames sent over Bluetooth SPP are transmitted late or not at all

Prepared for BTech / Benshi firmware support. Everything below is measured, with
the host and the receiving station's logs timestamped against clocks verified in
sync to 20 ms.

**Radio**: BTech UV-PRO. `GET_DEV_INFO` reports vendor 6, product 260, hardware
1, **firmware 146**.

**Host**: Linux (NixOS), BlueZ, Bluetooth SPP (RFCOMM), `tncd` 2.0 acting as a
KISS TNC bridge.

## Symptom

A KISS frame written to the radio's SPP socket is accepted by the socket and
then either never transmitted, or transmitted seconds to minutes later -- often
in a burst with other long-delayed frames.

For AX.25 this is fatal rather than merely slow: the remote station's
retransmit timer (FRACK) is 8 seconds, so an acknowledgement that arrives 90
seconds late lands on a session that has already been torn down.

## The clearest single measurement

A Winlink session, with the remote gateway's own decoder logging what reached
the air:

```
host hands 3 acks to the radio   07:44:22.642, 07:44:25.626, 07:44:28.641
gateway gives up, sends DISC      07:44:29.264
gateway RECEIVES all 13 acks      07:44:32.104 -> 07:44:33.624
                                  128 ms apart, signal level decaying 47 -> 14
```

Thirteen frames, queued over the preceding minute, were all radiated in a
1.5-second burst three seconds AFTER the session had been dropped.

## Lag distribution across six sessions

Per session, matching each frame the host transmitted against the time the
remote station decoded it:

| session | frames written | reached the air | median lag | max lag |
|---|---|---|---|---|
| A | 20 | 12 | 3.1 s | 115 s |
| B | 12 | 7 | 205 s | 231 s |
| C | 40 | 18 | 3.0 s | 112 s |
| D | 40 | 9 | 1.5 s | 98 s |
| E | 39 | 12 | 2.9 s | 10 s |
| F | 11 | 0 | — | — |

Two things are consistent: only a MINORITY of frames are ever radiated, and
those that are arrive seconds to minutes late. No session completed a transfer.

## What has been ruled out, and how

Each of these was a controlled change of exactly one thing.

**Not the host's AX.25 implementation.** The remote gateway's decoder and a
second local receiver agree frame-for-frame with what the host logged sending,
including sequence numbers, and the host answered every poll in the same
millisecond it was received. The protocol behaviour is correct; the frames
simply are not radiated when handed over.

**Not KISS framing.** The byte stream the host writes was captured and compared
across three different host builds spanning three weeks of development: it is
byte-identical, and conventional (`C0 00 <frame> C0`, one frame per pair of
FENDs).

**Not the host software version.** The same failure occurs with a build from
before the host gained any support for this radio's control protocol, with that
support present but idle, and with it fully active. The worst session of the six
was the one with that code absent.

**Not host-side audio or the receiving station.** An independent receiver in the
same room decodes the radio's transmissions at an ideal level (46-51 on Dire
Wolf's scale) when they do occur. The same host and the same gateway complete
100% of transfers when a different modem is substituted.

**Not primarily the receive direction.** Receive is good enough to rule it out
as the cause of the TX symptom: the radio decoded 11 of 11 test transmissions on
two different frequencies, and 116 frames from the gateway during one of the
failing sessions. It is not spotless, though, and we would rather state that
than overclaim -- in one post-reboot session it failed to deliver an inbound
frame that two other receivers in the same room decoded (see below). Treat
receive as working but not proven perfect.

**Not a power-cycle-clearable state.** Reproduced immediately after a reboot. In
one post-reboot session the radio transmitted exactly ONE frame of eleven, and
separately failed to deliver an inbound frame that two other receivers decoded.

**Not the radio's control channel.** `READ_SETTINGS`, `WRITE_SETTINGS`,
`READ_RF_CH` and `WRITE_BSS_SETTINGS` all succeed over the same SPP link while
KISS frames are being swallowed. The control protocol is responsive when the
data path is not.

**Not channel vs frequency mode.** Reproduced with the radio on a named memory
channel and on its VFO record, at 145.670 and 145.730.

## A second symptom: the Bluetooth link sometimes will not come up at all

This is separate from frames being swallowed, and may or may not share a cause.
It is reported here because any fix for the TX path has to survive it.

The radio intermittently refuses the SPP connection outright. BlueZ returns
`br-connection-refused` from `ConnectProfile`, and the host then retries on its
reconnect backoff:

```
bluetooth: calling ConnectProfile on /org/bluez/hci0/dev_38_D2_00_01_52_8F
bridge: port 0 reconnect error: bluetooth: ConnectProfile: br-connection-refused
bridge: port 0 reconnect in 10.0s
```

Across the bench logs for this investigation this happened 8 times, and every
single occurrence was this radio. A Mobilinkd TNC4 on the same host, the same
Bluetooth adapter and the same host code never produced it once, over 900+
`ConnectProfile` calls. The radio is paired and trusted when it refuses, and it
is reachable -- its control protocol answers over the same link once a
connection does succeed.

Two related observations:

- The radio dials **Hands-Free** at the host unprompted, treating it as a
  phone, and the host's Bluetooth daemon logs an authorization failure for it.
  The host has to actively drop the audio profiles the radio brings up, because
  an active audio profile corrupts the SPP data channel. It would be better if
  a radio in KISS/SPP use did not advertise or dial hands-free at all.
- The radio reports `Connected` as soon as the ACL is up, before any profile is
  negotiated. A host cannot tell "the radio is linked and ready for data" from
  "the radio opened a link for its own audio purposes", which makes the refused
  SPP connection above harder to diagnose than it should be.

## The host cannot detect it

`TIOCOUTQ` on the RFCOMM socket does not reflect the backlog: the socket reports
drained while frames are still held. A host therefore cannot distinguish "sent"
from "queued inside the radio", and counts a frame as transmitted the moment the
write returns.

## Reproduction

1. Connect to the radio over Bluetooth SPP and put it in KISS/TNC mode.
2. Write KISS frames at a modest rate -- an AX.25 connection attempt to an
   absent station (SABM every 3 s) is sufficient.
3. Monitor the frequency with an independent receiver and compare its decode
   timestamps against the host's write timestamps.

Expect a minority of frames, delayed by seconds to minutes, arriving in bursts
roughly 128 ms apart.

## What would help

- Whether the radio buffers host-supplied KISS frames behind its own
  carrier-sense/transmit scheduling, and what the queue depth and timeout are.
- Whether there is a way for the host to query or bound that queue.
- Whether inbound RF flushes it: bursts have repeatedly been observed to
  coincide with received traffic.
- What makes the radio return `br-connection-refused` to an SPP connection from
  a paired, trusted host, and whether the host can avoid provoking it.
- Whether hands-free can be suppressed while the radio is in KISS/SPP use, so
  the host does not have to tear down audio profiles to keep the data channel
  intact.
