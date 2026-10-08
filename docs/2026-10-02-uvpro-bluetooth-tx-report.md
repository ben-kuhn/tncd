# UV-PRO: frames sent over Bluetooth SPP are transmitted late or not at all

Prepared for BTech / Benshi firmware support. Everything below is measured, with
the host and the receiving station's logs timestamped against clocks verified in
sync to 20 ms.

**Radios**: BTech UV-PRO -- `GET_DEV_INFO` reports vendor 6, product 260,
hardware 1, **firmware 146**. A second Benshi radio, the **DB-50B**, reproduces
the link-layer faults; see "A second radio shows the same link-layer faults".

**Hosts**: reproduced on **two operating systems with two independent Bluetooth
stacks that share no code**:

| Host | Bluetooth stack | Host implementation |
|---|---|---|
| Linux | BlueZ 5.86, SPP over RFCOMM via D-Bus | `bluetooth_linux.go` |
| Windows 11 | Winsock `AF_BTH`, RFCOMM sockets | `bluetooth_windows.go` |

Host software is `tncd` 2.0 acting as a KISS TNC bridge. The two platforms use a
different socket API, a different connection-setup path and a different
operating system. **Both produce the same failure.**

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

## Decisive measurement: 11 consecutive frames swallowed, witnessed by a third receiver

Taken 2026-10-03. The host was logging every frame it handed to the radio, with
timestamps, while an **independent receiver in the same room** (Dire Wolf on a
separate radio, receive-only, never keying) logged every frame that actually
reached the air. Same frequency, same session, two clocks.

The host attempted a connection. It sent SABME, got no answer, fell back to
SABM, and retried on its 3-second timer:

```
handed to the radio over Bluetooth        heard on the air
10:49:47.708  SABME                       10:49:48  SABME
10:49:48.874  SABM                        -- nothing, for the rest of the run --
10:49:51.876  SABM
10:49:54.877  SABM
10:49:57.879  SABM
10:50:00.881  SABM
10:50:03.881  SABM
10:50:06.883  SABM
10:50:09.883  SABM
10:50:12.885  SABM
10:50:15.885  SABM
10:50:18.886  SABM
```

**The radio transmitted the first frame and then radiated nothing at all for the
next 30 seconds, across 11 further frames.** Every one was accepted by the
RFCOMM socket without error. The connection attempt timed out with no response.

Twelve frames in 31 seconds is not a buffer-pressure scenario by any
interpretation.

## Reproduced on Windows, on an unrelated Bluetooth stack

Taken 2026-09-13/14. The Linux measurements above could in principle be a BlueZ
or a Linux problem. They are not. The same test was run on **Windows 11**, where
the host reaches the radio through **Winsock `AF_BTH`** RFCOMM sockets -- a
separate implementation sharing no code with the Linux D-Bus/BlueZ path.

- **33** SABM/SABME frames handed to the UV-PRO over `AF_BTH`
- **0** heard on the air -- the independent monitor's log did not grow by one line
- **0 of 3** sessions connected; every failure at SABM -> UA

**The positive control that makes this conclusive.** During the same run the
radio delivered one inbound frame to the host: a genuine off-air beacon
(`KU0HN-1 > MAIL`), logged by the host at 22:21:49 and independently decoded by
the monitoring receiver at 22:21:48. So in that session, on Windows, the radio's
**receiver worked, it was tuned correctly, and the Windows read path delivered
data**. Only transmit failed.

**A different modem passed the identical test on the same host.** A Mobilinkd
TNC4 -- same binary, same Windows host, same configuration, hours earlier --
completed **3 of 3** sessions.

A send timeout cannot catch this either: with `SO_SNDTIMEO` correctly applied,
**zero** send timeouts fired, because the radio *accepts* the bytes and the
socket write completes normally.

Same signature on BlueZ and on Winsock `AF_BTH`. **The failure is independent of
the operating system and of the Bluetooth stack.**

## The same failure with unrelated host software

The measurements in this report were taken with one host application. That
application is not the variable either.

**WoAD on Android.** The same radio shows the same behaviour driven by WoAD, an
unrelated Winlink application on a third operating system. WoAD speaks KISS to
the radio directly and carries its own AX.25 layer-2 implementation -- it shares
no code, no library and no language with the host software used above, and runs
on Android's Bluetooth stack rather than BlueZ or Winsock.

This is an operator observation rather than an instrumented capture, and is
flagged as such. What it adds is that three independent implementations on three
operating systems produce the same symptom:

| Host application | Operating system | Bluetooth stack | Result |
|---|---|---|---|
| `tncd` 2.0 | Linux | BlueZ / D-Bus | frames swallowed |
| `tncd` 2.0 | Windows 11 | Winsock `AF_BTH` | frames swallowed |
| WoAD | Android | Android Bluetooth | same behaviour observed |

## The same software completes transfers with every other modem

The host software, the test harness and the AX.25/KISS layers are held constant
across everything below. Only the modem changes.

**Over the same Bluetooth SPP path, same binary:**

| Modem | Result |
|---|---|
| Mobilinkd TNC4 | **3 of 3** sessions on Windows; 2 x 10 KB uploads at **100%** on Linux |
| Mobilinkd TNC3 | completes transfers |

The TNC4 is the tightest control available: same binary, same host, same
configuration, same frequency, same gateway, same hour -- only the radio
differs. It passes on **both** operating systems where the UV-PRO fails on both.
It has also never once produced the `br-connection-refused` described below,
across 900+ connection attempts.

**Over USB / serial KISS, the same AX.25 and KISS implementation:**

| Modem | Result |
|---|---|
| Kantronics KPC-3+ / KPC-9612+ | full Winlink CMS session, messages delivered |
| AEA PK-232MBX | full Winlink CMS session, 3 messages delivered |
| Kenwood TS-2000 internal TNC | full Winlink CMS round-trip, 21 messages including one of 65 KB |
| Dire Wolf (software TNC, KISS over TCP) | completes transfers reliably |
| NinoTNC | reported working on production systems by experienced operators |

The NinoTNC line is third-party field reporting rather than our own bench
measurement, and is marked as such deliberately; everything else in this report
is instrumented. We include it because it is the widest-deployment data point
available: the same KISS/AX.25 approach is in routine production use on that
hardware.

Taken together: the host's AX.25 connected-mode implementation and its KISS
framing complete real sessions against real Winlink gateways across a range of
hardware, and the Mobilinkd results establish the same for the Bluetooth SPP
transport specifically.

**No modem other than a Benshi radio has produced this failure.**

## The same failure mid-session, with the frames arriving two minutes late

A second run connected successfully and died partway through a 10KB upload. The
time alignment shows the frames were not lost -- they were held:

```
handed to the radio                       heard on the air
10:46:44.582  I[6/6]                      10:46:41  I
10:46:44.691  I[7/6]                      10:46:42  I
10:46:44.821  I[0/6]                      10:46:43  I
10:46:53.977  RR                          ------------------------
10:47:12.287  RR + I,I,I   (retransmit)     DEAD AIR, 2m 07s
10:47:48.909  RR + I,I,I   (retransmit)   ------------------------
10:48:47.009  UA                          10:48:50  I
                                          10:48:52  I
                                          10:48:54  RR, UA
```

The air was silent for **2 minutes 7 seconds** while the host handed over eleven
frames in four separate attempts. They were radiated afterwards, by which time
the remote had given up and the session was gone. This matches the ~2-minute
hold reported below.

## Feed rate is not the variable

The two runs above, and a third, were a controlled A/B of the host's new TX
pacing, which caps outstanding air time so the radio is never handed more than
one frame of air time ahead:

| run | pacing | frames handed over | reached the air | lost | outcome |
|-----|--------|-------------------:|----------------:|-----:|---------|
| 1 | on  | 37 | 25 | 32% | stalled at 40% of upload |
| 2 | off | 25 | 16 | 36% | 2m07s dead air, session lost |
| 3 | on  | 12 |  1 | 92% | never connected |

Same binary, same radio, same gateway, same monitor; only `tx_pacing` changed.
**Pacing made no difference.** Feeding the radio strictly slower than the
channel can drain does not prevent the failure, which rules out host-side
overrun of any queue as the cause. The radio is not being given more than it can
take; it stops radiating what it already holds.

## A second radio shows the same link-layer faults

A second Benshi radio -- the **DB-50B** (`38:D2:00:01:67:9D`), on a different
Linux host -- reproduces the Bluetooth link-layer problems described under "A
second symptom" below.

To be precise about what this does and does not show: the DB-50B incident is
**not** another instance of frames being swallowed, and we are not presenting it
as one. It is a different failure -- an SPP channel delivering bytes that do not
frame as KISS. What it establishes is that the link-layer faults are **not
specific to a single radio model or a single unit**, and it adds a defect of its
own.

**1. bluez cannot parse this radio's SDP record.** At the exact timestamp of
every connect attempt:

```
sdp_extract_attr: Unknown data descriptor : 0x5 terminating
sdp_extract_attr: Unknown data descriptor : 0x30 terminating
```

preceded by an hour of `Unable to get Serial Port SDP record`. An SDP parser
terminating mid-record means SPP channel resolution falls back to something
cached or never verified -- and consistent with exactly that, a freshly
connected socket delivered five bytes that were not a valid frame
(`raw=b2bd7d8fe9`) seven seconds after connecting. **This looks like a malformed
SDP record on the radio**, and is worth checking independently of everything
else in this report.

**2. The same unprompted Hands-Free dialling.** The DB-50B advertises Handsfree
(`0000111e`) and Handsfree Audio Gateway (`0000111f`) alongside SPP, the host
drops both on every single connect, and the radio keeps re-offering them.

**3. The same `Connected` with no working data path.** bluez reported
`Connected=True` on an ACL with no functioning SPP session -- the same condition
described below, now seen on a second radio.

**In fairness, part of that incident was our own bug.** The host's wedge detector
responded by relinking without an upper bound, tearing down the baseband link
each time and making the churn worse. That is fixed on our side with a relink
budget. The three firmware-side observations above are independent of it.

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

**Not the host application.** The same behaviour occurs under WoAD on Android,
an unrelated Winlink program with its own AX.25 and KISS implementation, on a
third operating system and Bluetooth stack.

**Not the host operating system or Bluetooth stack.** Reproduced on Linux
(BlueZ, SPP over D-Bus) and on Windows 11 (Winsock `AF_BTH`), which share no
host code between them -- see the Windows section above. Same signature on both,
and on both a different modem on the same host passes the same test.

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
single occurrence was a Benshi radio. A Mobilinkd TNC4 on the same host, the same
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

This needs none of our software. It reproduces with standard, freely available
tools, so there is nothing here you have to take on trust.

1. Expose the radio's SPP channel as a serial device:
   `rfcomm bind /dev/rfcomm0 <bdaddr> <spp-channel>`
2. Drive it with **Dire Wolf's own `kissutil`**, which is a KISS host for an
   external TNC: `kissutil -p /dev/rfcomm0 -v -T`. Feed it frames on stdin in
   TNC2 format (`MYCALL>TEST:probe 1`) at a modest rate -- one every 3 seconds
   is enough. `-T` timestamps what it sends.
3. Monitor the frequency with an independent receiver and compare its decode
   timestamps against `kissutil`'s send timestamps.

For a connected-mode version, Dire Wolf's `tnctest` drives two TNCs against each
other over the air: `tnctest /dev/rfcomm0=9600 <second-tnc>`. Pointing the second
at a known-good modem gives a direct side-by-side on the same channel.

Expect a minority of frames, delayed by seconds to minutes, arriving in bursts
roughly 128 ms apart. Reproduces on Linux and on Windows.

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
- Whether the DB-50B's SDP record is well-formed. A standard Linux SDP parser
  terminates mid-record on it (`Unknown data descriptor : 0x5/0x30`), which
  would make SPP channel resolution unreliable on any host.
