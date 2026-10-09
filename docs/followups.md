# Follow-ups

Issues found while working on other things. Each one is real, none is fixed, and none was
introduced by the work that found it. Recorded here so "pre-existing, out of scope" does not
become "forgotten".

## Code

### 1. `writerLoop`'s KISS write path has no deadline — FIXED 2026-09-30
~~`kiss/port.go`'s writer loop called the transport's `Write` with no timeout.~~ On Linux
Bluetooth there is no `SetWriteDeadline`, and `checkTXDrain` samples the send queue
*before* each write, so it could not abort one already blocked inside the syscall. A wedged
radio therefore parked the writer loop indefinitely, the 64-deep TX queue filled, and every
later frame was dropped while the port still reported healthy.

Bounded by `txWriteTimeout` (10s, matching `ctrlWriteTimeout` and Windows' `btSendTimeout`,
so every write path in the tree is bounded the same way). On expiry the response is to
**fail the port**, not merely error the frame: an abandoned mid-flight write leaves the byte
stream in an unknown state, so reconnecting is the only honest recovery — the same
conclusion `internal/rig` and the control-write path reached independently.

The abandoned goroutine keeps the tx slot until `Close` releases it, which is correct:
nothing else may write while its bytes might still land. It is reclaimed when the fd
closes, since Go's runtime poller interrupts in-flight I/O on close for pollable fds.

`TestTXWriteTimeoutFailsPortCleanly` covers it, and was confirmed to hang against the old
code — which is precisely the bug.

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

### 5. Rig control enabled on a radio that does not support it — FIXED 2026-09-30
Enabling `[rigctl.N]` on a non-Benshi port bound a listener that accepted clients and
answered `RPRT -5` to everything, forever, with nothing saying why.

**Now: rig control is disabled for that port and the operator is warned.** A first pass
added the detection but never wired it in -- `Identify()` had no callers and the behaviour
was unchanged -- which is recorded here because "the mechanism exists" is not the same as
"the problem is fixed".

`Runtime.gateRigCtl` runs per enabled port: it waits for the port to come online, probes
once, and on `ErrNotBenshi` closes that rigctl listener and logs a warning naming the port
and both possible causes. It deliberately does NOT stop tncd or touch the KISS bridge,
which is the function people actually depend on. A transient failure (port offline
mid-probe, one slow round trip) is retried rather than condemning the radio.

The gate runs in the background rather than at bind time because at bind time the answer is
not knowable: ports connect asynchronously, so probing then would reject a good radio for
not having finished dialling. The listener therefore exists for a few seconds before being
withdrawn -- the honest trade against refusing to start tncd at all over a radio that might
be fine.

Verified both ways: a TCP port pointed at a listener that accepts and stays mute (exactly
what a non-Benshi TNC looks like to a Gaia request) has its listener withdrawn with the
warning above, and a real UV-PRO is identified (`vendor 6, product 260, hw 1, firmware
146`) with the listener left serving.

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

### 9. Compact (shared-FEND) KISS framing — FIXED 2026-09-30
~~`kiss/demux.go` entered a KISS frame only on its own opening FEND and left on its own
closing FEND~~, so a peer emitting `C0 <f1> C0 <f2> C0` lost `f2`: after the middle FEND the
demux was out of KISS, and the next frame's first byte hit the noise branch and was dropped
until the following FEND. Silent, with no counter, on every port.

The first attempt at this was reverted because it broke
`TestPortControlChannelKISSTXWriteNotCorruptedByConcurrentControlWrites`, which guards
mixed KISS/control interleaving — a behaviour verified on air. **That test's premise was
the problem, not the fix.** It used a raw `0xBB` byte run as a stand-in for a control
write, and control writes are never raw bytes: they are always Gaia frames from
`internal/rig`, self-identifying by their `0xFF 0x01` lead.

So the fix is now safe by construction:

- `portControlChannel.Write` **enforces** that a control write is a Gaia frame, turning the
  convention the demux relies on into a checked invariant rather than an assumption.
- `demux` carries a `sharedFEND` state across `Feed` calls; the byte after a closing FEND
  decides. `0xFF` means a control frame, anything else means the FEND also opened the next
  KISS frame — which is exactly what `framing.go`'s Decoder does ("this FEND is both the
  closer and the opener").
- The interleaving test now uses a real Gaia frame, so it tests what actually happens.

Covered by `TestDemuxCompactSharedFENDFraming` and
`TestDemuxGaiaAfterClosingFENDStillRouted`. Dire Wolf was separately measured as NOT using
the compact form (FEND runs `[1,2,2,2,1]`), so this was never affecting the most likely
peer — but it is a correctness fix for any TNC that does.

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

### 16. `go test -c` binaries need their fixtures staged alongside
Cross-platform validation runs the suite as `go test -c` binaries copied to a VM, since
neither the Windows nor the FreeBSD test box has a Go toolchain. Three packages then fail
for a reason that has nothing to do with the platform: they read fixtures by relative path
and the source tree is not there.

- `agwpe` and `ax25` want `testdata/frames.json` in the working directory
- `internal/config` wants `../../tncd.ini`

Recreate the paths and they pass. Worth knowing before reading a cross-platform run as a
portability failure -- on both Windows and FreeBSD the same three failed identically, which
is itself the clue that it is the harness and not the platform.

### 17. Remote PTT does not work on UV-PRO firmware 146
`DO_PROG_FUNC(MAIN_PTT)` is accepted by the radio and never keys the
transmitter. Measured 2026-09-30, confirmed two independent ways: the radio's
own `is_in_tx` bit stays 0, and a TS-2000 on the same frequency stayed at its
-54 noise floor while the same meter peaked at +60 for a known-good UI frame
minutes earlier.

Swept `button_id` 0-3 and 15 with LOW_TO_HIGH/HIGH_TO_LOW actions: every
combination accepted (reply status 0), every one silent.

**The firmware DOES support remote keying** -- the operator keys this radio
from the vendor's own app. An earlier version of this note concluded otherwise
and was wrong; it is recorded because "the hardware cannot do it" is exactly
the conclusion that stops anyone looking.

The body format is settled. Every ONE-byte body is rejected with status 5
whatever the effect (tested: 13, 14, 15, 18, 21, 22) and every TWO-byte body
is accepted with status 0. Note HTCommander sends one byte, so its
`doProgFunc` would not work on this firmware either -- and it never uses it
for PTT, only `toggleAbCh`.

**The mechanism is audio streaming, not a command.** HTCommander's voice PTT
does not use the Gaia protocol at all:

```dart
Future<void> _sendAudio(Uint8List data) async {
  await BluetoothClassicMacOS.instance.sendAudio(macAddress, data);
}
```

It streams PCM over a separate Bluetooth audio channel, and the radio keys
because audio is arriving. That is why HTCommander enumerates `mainPtt` and
never calls it.

tncd's own logs confirm the radio offers that path, on every single connect:

```
bluetooth: ... dropped audio profile 0000111e  (Handsfree)
bluetooth: ... dropped audio profile 0000111f  (Handsfree Audio Gateway)
```

tncd deliberately discards both. So the consistent picture is:

| path | how it keys |
|---|---|
| vendor app / HTCommander voice | stream audio -> radio keys |
| tncd packet TX (works today) | send KISS data -> radio keys |
| `DO_PROG_FUNC(MAIN_PTT)` | accepted (SUCCESS), does nothing |

**The radio keys when given something to transmit.** There is no "key with
nothing to send", which is exactly what `rigctl set_ptt 1` asks for. Supporting
it would mean tncd holding open an HFP audio channel and streaming silence --
a different kind of program from a KISS bridge, and it would fight the audio
profiles tncd currently drops on purpose.

`UNLOCK` (65) and the lock state remain untested and could still turn out to
gate `DO_PROG_FUNC`, but the audio finding makes that a less likely
explanation than "this command is enumerated and not implemented".

**Lower stakes than it looks.** tncd already keys this radio for every packet
frame -- the radio transmits when given data. Discrete PTT only matters for
tune-up or CW, not for the AX.25 path anyone actually uses tncd for. The
refusal path (`allow_ptt = false` -> `RPRT -4`) works and is validated.

Two real bugs fell out of the investigation and are fixed: the body was
malformed (one byte instead of the two-byte PF record), and `SetPTT`
discarded the reply status so the radio's rejection was invisible.

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

## Go toolchain: release and nix build paths ship an unpatched standard library

Found 2026-10-09 while fixing CI. `govulncheck` under each toolchain, same code:

| Go | source | stdlib vulns reported |
|---|---|---|
| 1.25.13 | `go.mod` directive | **8** |
| 1.26.7 | nixpkgs `go` (current channel) | affected |
| 1.27.1 | nixpkgs `go_1_27` | **6** |
| **1.27.2** | GitHub `setup-go` with `stable` | **0** |

`.github/workflows/test.yml` and `release.yml` now both use `go-version:
stable`, so CI and released binaries get the patched toolchain. Two gaps remain
and neither is fixable in this repo today:

- **`nix/default.nix` uses `buildGoModule`**, which takes nixpkgs' default `go`
  (1.26.7 on the current channel). `go_1_27` is 1.27.1, also affected, so there
  is no patched Go in this channel to pin to. It resolves when nixpkgs advances;
  until then nix-built binaries -- **including the fleet's, which builds from
  this path via the `tncd-src` flake input** -- carry the unpatched stdlib.
  Re-check with: `nix-shell -p go_1_27 --run 'go run
  golang.org/x/vuln/cmd/govulncheck@latest ./...'`
- **`packaging/PKGBUILD` declares `makedepends=('go')`**, so Arch builds with
  whatever Go that distro currently ships. Out of our control and Arch tracks
  Go closely, so probably fine, but unverified.

**Proportionality.** The affected symbols are `net/http` (mostly http2),
`net/textproto` and `crypto/tls`. The only one tncd genuinely reaches is the
monitoring API in `internal/frontend/api`, which is off by default and
loopback-bound with an allowlist when enabled. The other listeners (AGWPE,
KISS-over-TCP, rigctl) are plain TCP and do not go through net/http. So the
practical exposure is low -- but it is not zero for anyone who exposes the API,
and shipping a knowingly-unpatched stdlib in release artifacts is not a good
look for a project that runs `govulncheck` in CI.

## Stale version strings in packaging

`nix/default.nix` says `version = "1.103-Beta"` and `packaging/PKGBUILD` says
`pkgver=1.3.3`, both left over from the 1.x line while `main` is the 2.0 line.
The release checklist already says to bump both at tag time, so this is only
wrong between releases -- but `nix/default.nix`'s value is compiled into the
binary via `-X ...version.Version`, so a nix build from `main` today reports
itself as 1.103-Beta. Worth fixing independently of any release.
