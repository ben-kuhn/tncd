# Follow-ups

Issues found while working on other things. Each one is real, none is fixed, and none was
introduced by the work that found it. Recorded here so "pre-existing, out of scope" does not
become "forgotten".

## Code

### 1. `writerLoop`'s KISS write path has no deadline
`kiss/port.go`'s writer loop calls the transport's `Write` with no timeout. On Linux
Bluetooth there is no `SetWriteDeadline` anywhere in `kiss/bluetooth_linux.go` (zero grep
hits), and `checkTXDrain` samples the queue depth *before* each write, so it cannot abort a
write already blocked inside the syscall.

A wedged radio can therefore block KISS TX indefinitely: the writer loop parks in the
syscall, the 64-deep TX queue fills, and every subsequent frame is dropped. This project has
bench-confirmed Bluetooth links that accept writes while nothing reaches the radio, so this
is a demonstrated failure mode rather than a theoretical one.

Found while bounding the *rig-control* write path (which now has a 10s deadline and fails
the port on expiry). The KISS path deliberately was not changed in that work because it is a
much larger blast radius. Windows is already bounded via `btSendTimeout` (10s SO_SNDTIMEO);
Linux is the gap.

Worth noting the fix is not simply "add a deadline": an abandoned mid-flight write leaves the
byte stream in an unknown state, so the honest response to a timeout is to fail the port and
reconnect, as both `internal/rig` and the rig-control write path concluded independently.

One useful data point for whoever takes this on: a reviewer tested whether an abandoned
blocked write actually leaks forever, using an `os.Pipe` against a real `bluetoothTransport`
(its `file` is an `os.NewFile`-wrapped socket fd, so the proxy is faithful). `Close()` DOES
unblock the blocked `Write` promptly — Go's runtime poller interrupts in-flight I/O on close
for pollable fds. Windows already bounds its writes with a 10s `SO_SNDTIMEO`, and FreeBSD
does an explicit `shutdown(SHUT_RDWR)` before close for the same reason. So no transport in
the tree leaves such a goroutine truly unkillable, and a fail-the-port response should
actually reclaim it rather than accumulating leaks.

### 2. `bluetoothTransport`'s open/closed guards are lockless — FIXED 2026-09-29
~~`Read`/`Write` check `bt.file == nil` with no synchronisation against a concurrent
`Close()`.~~ Linux now guards `file` with a mutex and hands callers a reference via
`handle()`, so a concurrent `Close` cannot nil it between the check and the use. The lock
is deliberately NOT held across the Read/Write syscalls: those block for as long as the
radio takes, and holding it there would make `Close` — whose job is to interrupt a blocked
I/O — wait on the very call it is unblocking. Verified with `-race -count=2` across kiss,
internal/rig, internal/frontend/rigctl, internal/bridge and internal/app.

**Windows still has the same shape** (`bt.fd == InvalidHandle` checked without
synchronisation) and was not touched here — no Windows box was available to test against.
Same fix, same reasoning, when someone is on that VM.

### 3. Benshi framing constants are duplicated — LINKED 2026-09-29
`kiss/demux.go` still mirrors five constants from `benshi/frame.go`, and still should: the
demultiplexer needs byte-level visibility into a partially accumulated frame that
`benshi.Decoder` deliberately does not expose, and a production `kiss -> benshi` dependency
for five integers is not worth it.

What was missing was any link between the copies. `benshi` now exports `FrameStart`,
`FrameVersion`, `FrameHeaderLen` and `MsgHeaderLen` (`FlagChecksum` already was), and
`TestGaiaConstantsMatchBenshi` asserts the demux's values against them. The import is
test-only, so the production import graph is unchanged, but a firmware change that moves the
frame header now breaks a test instead of silently desynchronising the two.

### 4. CI's fuzz smoke step fails intermittently — FIXED 2026-09-25
`.github/workflows/test.yml` ran six fuzz targets at `-fuzztime=10s` and failed twice on
commits that changed no Go code, both times as:

    context deadline exceeded

That is Go's fuzz COORDINATOR timing out, not a crasher — and it writes nothing to
`testdata/fuzz/`, so it is reported exactly like a genuine finding. It would have trained
people to re-run red builds without looking, which is how a real crasher gets missed.

**Root cause: CI had no fuzz corpus cache**, so every run started from the seeds. From cold,
`FuzzParseModulo` discovers ~47 new interesting inputs inside the 10s budget, and every new
input triggers minimisation; when the deadline lands during that work the coordinator's
context expires. Reproduced locally by running with an empty `GOCACHE` — identical counts to
the CI log (47 new, total 53). It passed locally otherwise only because a warm corpus finds
nothing new, and because this box does 180k execs/sec on 12 workers where the runner does
70k on 4, so the runner loses the boundary race far more often.

Fixed by caching `~/.cache/go-build/fuzz` across runs (per-commit key, prefix restore) plus
`-fuzzminimizetime=2s`. A warm corpus finds ~0 new inputs, so there is nothing to minimise
at the boundary, and fuzzing becomes cumulative across pushes instead of restarting from the
seeds every time — strictly better coverage for the same 10 seconds. A genuine find still
fails the step loudly, since it writes a crasher and prints the input.

All twelve `Fuzz*` targets in the tree are now listed in that step; five had never been
fuzzed in CI at all, only replaying their seed corpus under plain `go test`.

### 5. Detecting whether a radio speaks Benshi — RESOLVED 2026-09-30
Enabling `[rigctl.N]` on a non-Benshi port used to bind a listener that answered
`RPRT -5` to everything, with no way to tell why.

**The detection is `GET_DEV_INFO`** -- already sent by `Probe()`, which was throwing the
reply body away. `benshi.DecodeDevInfo` now decodes it, `(*Rig).Identify` caches it for the
life of the link, and `ErrNotBenshi` reports failure naming **both** possible causes,
because silence genuinely cannot distinguish them: this project has bench-confirmed
Bluetooth links that accept writes while delivering nothing. A timeout means "not a Benshi
radio, OR the control link is not passing data", and claiming otherwise would be a guess.

`tncd rig probe` now prints the identification (`vendor 6, product 260, hw 1, firmware
146` from a UV-PRO) instead of a bare "radio answered" -- which also fixes a README claim
that the command identifies the radio when it did not.

**Capability bits were considered and rejected -- they lie.** `GET_DEV_INFO` carries
`support_vfo` and `channel_count`, measured on a UV-PRO (firmware 146) as:

- `support_vfo = 0` on a radio that had been operating in VFO mode all day. Gating on it
  would refuse a working radio outright.
- `channel_count = 30` on a radio whose VFO record is channel 252. Whether that counts
  programmed channels or the size of one bank is unclear and nothing here distinguishes
  them -- but under every reading it is ~30 while the VFO is at 252, so it cannot derive a
  VFO floor, and a floor taken from it would sit far below the memories it is meant to
  protect. `vfo_channel_min` keeps its conservative 251 default.

Answering the command at all remains the only trustworthy signal.

### 6. bluez cannot parse the DB-50B's SDP record
From the CarKit flap report (`docs/2026-09-24-bluetooth-flap-report.md` on main):
`sdp_extract_attr: Unknown data descriptor : 0x5/0x30 terminating` appears at the exact
timestamp of every relink connect, and `Unable to get Serial Port SDP record: Host is down`
throughout the preceding hour.

If bluez's SDP parse terminates mid-record, the RFCOMM channel for SPP is resolved from
cache or not verified at all — which fits the observed symptom of a `NewConnection` handing
back a live fd that carries no traffic, and later a torn stream (a 5-byte fragment arriving
7s after a fresh connect). Worth dumping the raw SDP record and, if it is malformed,
reporting it to the vendor. Note the radio also advertises Handsfree and Handsfree Audio
Gateway, which is unusual for a TNC.

### 7. Unpaired ports are probed forever and spam the log — FIXED 2026-09-29
~~Two configured ports were not paired on that host, and tncd called `ConnectProfile` on
them every 60s for over an hour.~~ Fixed at both ends:

- `kiss/bluetooth_linux.go` checks the BlueZ `Device1.Paired` property before calling
  `ConnectProfile`, and returns `ErrNotPaired` naming the radio and the `bluetoothctl pair`
  command. A missing device object and an unpaired one report identically on purpose —
  both mean "pair this radio first", and both otherwise surfaced as the same unhelpful
  `Method "ConnectProfile" ... doesn't exist`.
- `internal/bridge` collapses an unchanging reconnect error into one line plus an hourly
  reminder (`reconnectLogRepeatEvery`). That helps every repeating cause, not just this
  one. A changed error is always reported immediately.

An explicit per-port disable was not added: a port that cannot connect now costs one log
line, which is the actual complaint, and a config key would be a second way to express
"don't use this radio" alongside deleting the section.

### 8. A relinked socket is not resynchronised — NOT A DEFECT (checked 2026-09-29)
The original claim was that a relinked socket parses the tail of a torn stream, based on
the CarKit flap report's `failed to parse AX.25 frame: frame too short (5 bytes)
raw=b2bd7d8fe9` arriving 7s after a fresh connect.

**It resynchronises already.** A reconnect builds a whole new `kiss.Port` (`bridge.go`
calls `kiss.NewPort` on the reconnect path), so it gets a zero-value `demux` holding a
fresh `Decoder`, and `framing.go`'s decoder discards every byte until it sees its first
FEND. The torn prefix is dropped, not parsed. Proven by
`TestDemuxFreshPortResyncsToFirstFEND`, which feeds exactly those five bytes followed by a
clean frame and gets one frame out.

So those five bytes must have arrived BETWEEN two FENDs — a genuinely corrupt short frame,
not a framing failure. That is consistent with the addendum's conclusion that the relinked
socket was carrying bytes that did not parse as KISS, and it points back at #6 (BlueZ
resolving the wrong RFCOMM channel) rather than at any resync logic.

Worth keeping: the parse failure returns before the relink counter is reset, so corrupt
bytes do not refresh the futile-relink budget. That is correct and load-bearing.

### 9. The demux does not handle compact (shared-FEND) KISS framing
`kiss/demux.go` enters a KISS frame only on its own opening FEND and leaves on
its own closing FEND. `framing.go`'s `Decoder` additionally treats a FEND seen
while already in-frame as closing one frame AND opening the next -- the compact
single-delimiter form -- and stays in-frame through it. So a peer emitting
`C0 <f1> C0 <f2> C0` loses `f2`: after the middle FEND the demux is out of
KISS, and `f2`'s first byte hits `scan`'s noise branch and is dropped, byte by
byte, until the next FEND. Silent, with no counter.

The demux is wired into EVERY port, so this would affect serial and TCP users
who have nothing to do with Benshi radios.

**Not fixed, deliberately.** The obvious fix -- treat a byte following a
closing FEND as the next frame's content -- breaks
`TestPortControlChannelKISSTXWriteNotCorruptedByConcurrentControlWrites`, which
guards mixed KISS/control interleaving on one stream. That interleaving is
verified on air (two KISS frames plus a Gaia reply, confirmed by an independent
Dire Wolf receiver); compact-form framing is **not** verified to be emitted by
any TNC in the supported matrix. Trading a confirmed guarantee for a
hypothetical one is the wrong way round.

**What would settle it:** capture raw bytes from each supported TNC (KPC-3+,
PK-232, TS-2000, Mobilinkd TNC4, UV-PRO) and check whether any emits
`C0 <f1> C0 <f2> C0` rather than `C0 <f1> C0 C0 <f2> C0`. If one does, the
demux needs real disambiguation rather than a blanket rule -- note that on a
Benshi port every control frame starts `0xFF`, so "anything else after a
closing FEND is KISS content" may in fact be safe there, and the test's raw
`0xBB` stand-in for a control write does not reflect the real Gaia framing.

`kiss/rawdump_test.go` (`TestRawDumpBluetooth`, skipped unless
`TNCD_HW_BDADDR` is set) is the capture tool, and `analyseKISSFraming` in it
does the counting. For a TCP/serial TNC, dump the socket instead -- the point
is only to see the delimiters, which every decoder above the transport
consumes.

**Progress 2026-09-29 — Dire Wolf is CLEAR.** Four UI frames sent from a
UV-PRO and received off air by Dire Wolf, read raw off its KISS TCP port:

```
FEND run lengths: [1, 2, 2, 2, 1]
interior single FENDs with data on both sides: 0
```

Every frame carries its own opening and closing FEND, so each interior
boundary is a doubled `C0 C0`. Dire Wolf -- the reference implementation and
by far the most likely peer -- does **not** use the compact form, and the
demux is correct for it. That removes most of the practical risk here, though
it does not close the item.

Still unknown: the **UV-PRO's own** framing, and the serial TNCs. Measuring
the UV-PRO needs it to RECEIVE, and on the bench 2026-09-29 it decoded nothing
from a TS-2000 transmitting on its frequency, while the reverse direction
worked fine all session -- see the operational note below. The serial TNCs
(KPC-3+, PK-232, TS-2000 internal) need someone to plug them in.

### 10. A stray `0xFF 0x01` can swallow a run of bytes on any port
Same file: `scan` starts a Gaia candidate on `0xFF` regardless of whether a rig
consumer is attached, and `deliverGaia` only checks for a consumer after the
frame is fully accumulated. Noise matching `0xFF 0x01` therefore consumes up to
263 following bytes, whole KISS frames included.

Lower risk than it first looks: a `0xFF` not followed by `0x01` is rejected
after one byte and re-examined, so it takes that exact pair to trigger.
Gating accumulation on an attached consumer is NOT the fix -- it was tried and
reverted. Skipping recognition feeds the Gaia frame's bytes to the KISS decoder
as frame content and fabricates a spurious KISS frame, which is worse, and it
would hit a real Benshi port whenever rig control happened to be detached.

### 15. `tncd rig` error when tncd is running — FIXED 2026-09-30
~~The one-shot CLI failed with `bluetooth: RegisterProfile: UUID already registered`~~,
which described a D-Bus detail rather than the cause. It now says the SPP profile is
already registered because another tncd is running and holding the radio, and points at
the `[rigctl.N]` listener as the way to reach a radio already in use. Refusing is still
correct -- two processes cannot share one Bluetooth link.

## Radio / operational (not tncd bugs, but they cost hours)

### The bench UV-PRO does not decode the TS-2000 (one direction only)
2026-09-29: with both radios on 145.670 and a metre apart, Dire Wolf/TS-2000
transmitted four UI frames (confirmed sent -- they appear in Dire Wolf's own
TX echo) and the UV-PRO's KISS stream stayed completely empty across two
45-second captures. The reverse direction works reliably and has all session:
UV-PRO transmissions are decoded off air by the TS-2000 every time.

So this is receive-side, UV-PRO only. Most likely front-end desense from a
strong signal at close range (the same shape as the TH-D7 dual-RX desense
noted elsewhere), or a squelch/deviation mismatch. Not investigated -- it
blocks measuring the UV-PRO's KISS framing for item #9, but nothing else, and
it is a bench-geometry problem rather than a tncd one. Worth ruling out with
an attenuator or more separation before reading anything into it.

### 11. The UV-PRO's TNC wedges after heavy connect/disconnect churn
Reproducible: after dozens of Bluetooth connect/disconnect cycles, KISS frames stop reaching
the air while everything still reports healthy — port online, writes succeed, `tx` counter
climbing. A power-cycle clears it. Independent of tncd; confirmed by reproducing the failure
with tncd's own unmodified AGWPE path after it had worked minutes earlier.

Belongs in the OTA checklist: **if KISS goes silent after repeated reconnects, power-cycle
the radio before debugging tncd.**

**2026-09-29 — a second, worse shape of this, and the adapter reset does NOT clear it.**
After a day of heavy churn (OTA criterion A runs, raw-framing captures, repeated service
stop/start), the radio stopped accepting SPP altogether:

- `bluetoothctl info` reports `Connected: yes` while `ConnectProfile` answers
  `br-connection-refused`, and bluez logs
  `Unable to get Serial Port SDP record: Host is down`
- `bluetoothctl disconnect` HANGS rather than returning
- `sudo hciconfig hci0 reset` completes but changes nothing — the radio still refuses

The adapter reset is not an equivalent recovery: the wedge is on the radio side, so
resetting the host controller changes nothing. The flap report's advice ("reset the
Bluetooth adapter **or** power-cycle the TNC") offers the two as interchangeable, and
`bridge.go`'s escalated relink message repeats that. Worth correcting.

**But a power-cycle is not required either — it self-clears.** Corrected same day: after
roughly ten minutes with nothing touching the radio, SPP accepted a connection again with no
power-cycle and no adapter reset, and the link was genuinely healthy (a UI frame through it
was decoded off air by an independent receiver, and a full connect to a BPQ gateway
followed). So the real recovery is **wait**, and the useful operator advice is to stop
hammering it: every reconnect attempt during the wedge appears to hold it open. That also
reframes the CarKit storm — tncd relinking every 20s may have been the thing preventing
recovery, which is a further argument for the relink budget.

This is also the same signature as the CarKit DB-50B flap (§6) reproducing on a different
Benshi radio, which is evidence the SDP/SPP fault is a family trait rather than one unit.

### 12. BLE KISS does not pass traffic on the UV-PRO
With a genuine LE link (MTU negotiated 155, GATT resolved, notifications subscribed), writes
to the BLE KISS characteristic either time out (write-with-response) or succeed and vanish
(write-without-response), and nothing is ever received. The service is advertised and
connectable but appears inert.

Unexplained. Note HTCommander — purpose-built for these radios — contains no BLE-KISS
references at all and moves data via the Benshi protocol's `HT_SEND_DATA`. Whether that is
the only working BLE data path on this radio is untested.

Related: the LE-transport fix (`30885b9`) is now MERGED to main (`e8c4b31`, pushed
2026-09-24). It makes tncd establish a real LE link instead of reporting a phantom one.

**That fix is a prerequisite for any BLE rig-control work.** The deferred BLE control
channel (Task 4 (BLE) / Task 11 (classic RFCOMM) of the rig-control plan) cannot be validated without it: before the fix,
tncd reported a healthy online port with no LE link at all, so a BLE control channel would
have appeared to open and then silently failed every write. Whoever picks up BLE rig control
must branch from a main that contains `e8c4b31` — the rig-control feature branch was cut
from the older main and does NOT have it. Merge main in first rather than cherry-picking,
to keep the history clean.

### 13. The Mobilinkd TNC4's pairing was removed and did not re-pair
Its bond was deleted host-side during BLE investigation; re-pairing reports success but
stores no key (`Paired: yes, Bonded: no`), so it works over neither classic nor LE. The
device still holds its half of the old bond. Try a power-cycle first, then whatever reset
Mobilinkd provides.

### 14. PipeWire's ALSA plugin will not negotiate
`arecord -D pipewire` and `-D default` both fail at every rate and channel count, so Dire
Wolf cannot use PipeWire and must grab the Digirig directly via `plughw`, which prevents any
other application from sharing that audio interface. The running daemon reports libpipewire
1.6.6 while the ALSA plugins in the store are 1.4.9 and 1.6.5 — a version skew is the
leading suspect. Not a tncd issue; it just blocks sharing the radio's audio.
