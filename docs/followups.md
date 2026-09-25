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

### 2. `bluetoothTransport`'s open/closed guards are lockless
`Read`/`Write` check `bt.file == nil` (Linux) or `bt.fd == InvalidHandle` (Windows) with no
synchronisation against a concurrent `Close()`. In practice the guards make the failure
benign (a clean "not open" error rather than a use-after-close), but it is a data race by
Go's memory model and `-race` would flag it under the right interleaving.

### 3. Benshi framing constants are duplicated with no compile-time link
`kiss/demux.go` mirrors four constants from `benshi/frame.go` (`gaiaStart`, `gaiaVersion`,
`gaiaHeaderLen`, `gaiaMsgHeaderLen`, `gaiaFlagChecksum`) because `benshi` does not export the
internals the demultiplexer needs. They were verified identical when written, and the
length/resync arithmetic is line-for-line equivalent to `benshi.Decoder.Feed`.

Nothing links them, so a firmware change that moves the frame header would need both edited.
Either export the needed pieces from `benshi` or add a test that asserts the two agree.

### 4. CI's fuzz smoke step fails intermittently, and the failure looks like a real finding
`.github/workflows/test.yml` runs six fuzz targets at `-fuzztime=10s` each on every push.
On 2026-09-24 the `main` build failed with:

    --- FAIL: FuzzParseXID (11.00s)
        context deadline exceeded

That is Go's fuzz COORDINATOR timing out waiting on a worker, not a crasher. Re-running the
identical commit with no changes passed. Evidence it was never a code defect: `ax25` was
untouched by that merge; `ParseXID`'s loop is O(n) with every length bounds-checked, so
there is no pathological input to find; a local 30s run did 5.1M execs clean; and the CI log
shows throughput collapsing from 62,217/sec to 19,694/sec in the final seconds — runner
contention.

Why it matters: a deadline-exceeded result is reported exactly like a genuine fuzz failure,
and writes nothing to `testdata/fuzz/`, so there is no artifact distinguishing "flake" from
"found a crasher" without reading the log carefully. It will keep failing pushes and will
train people to re-run red builds without looking — which is how a real crasher gets missed.

Raising `-fuzztime` makes the step slower without making it more robust. Better options: tell
the two apart and only fail on an actual crasher, or move fuzzing to a scheduled run rather
than per-push, keeping the seed-corpus regression tests (which are deterministic and fast) in
the per-push job.

### 5. `kiss.Port.ControlChannel()` does not validate the radio speaks Benshi
Enabling `[rigctl.N]` on a port whose radio is not a Benshi device binds a listener that
accepts clients and answers `RPRT -5` (timeout) to every command, rather than failing at
startup with a clear message. The operator sees a working-looking listener and a rig that
never responds.

A startup probe (`GET_DEV_INFO`, which we know the radio answers) would let tncd refuse or
warn at config time. Deferred because it needs a decision about whether a non-answering
radio should be a hard config error — which would break startup for anyone who enables the
key optimistically — or a logged warning.

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

### 7. Unpaired ports are probed forever and spam the log
Same report: two configured ports were not paired on that host, and tncd called
`ConnectProfile` on them every 60s for over an hour, logging
`Method "ConnectProfile" ... doesn't exist` each time, plus bluez `Host is down` SDP noise.
tncd should gate on the bluez device object actually being present and paired, or support an
explicit per-port disable, so an unprovisioned port does not bury the log that matters.

### 8. A relinked socket is not resynchronised, so it delivers garbage
Same report: 7s after a fresh SPP connect, the socket produced
`failed to parse AX.25 frame: frame too short (5 bytes) raw=b2bd7d8fe9` — the tail of a torn
stream. A newly established transport should discard bytes until a KISS frame boundary
(FEND) rather than parsing whatever arrives first.

Note the parse failure returns BEFORE the relink counter is reset in `handleFrame`, so
corrupt bytes do not currently reset the futile-relink budget. That is the correct behaviour
and worth preserving if this is fixed.

## Radio / operational (not tncd bugs, but they cost hours)

### 9. The UV-PRO's TNC wedges after heavy connect/disconnect churn
Reproducible: after dozens of Bluetooth connect/disconnect cycles, KISS frames stop reaching
the air while everything still reports healthy — port online, writes succeed, `tx` counter
climbing. A power-cycle clears it. Independent of tncd; confirmed by reproducing the failure
with tncd's own unmodified AGWPE path after it had worked minutes earlier.

Belongs in the OTA checklist: **if KISS goes silent after repeated reconnects, power-cycle
the radio before debugging tncd.**

### 10. BLE KISS does not pass traffic on the UV-PRO
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
channel (Task 4b of the rig-control plan) cannot be validated without it: before the fix,
tncd reported a healthy online port with no LE link at all, so a BLE control channel would
have appeared to open and then silently failed every write. Whoever picks up BLE rig control
must branch from a main that contains `e8c4b31` — the rig-control feature branch was cut
from the older main and does NOT have it. Merge main in first rather than cherry-picking,
to keep the history clean.

### 11. The Mobilinkd TNC4's pairing was removed and did not re-pair
Its bond was deleted host-side during BLE investigation; re-pairing reports success but
stores no key (`Paired: yes, Bonded: no`), so it works over neither classic nor LE. The
device still holds its half of the old bond. Try a power-cycle first, then whatever reset
Mobilinkd provides.

### 12. PipeWire's ALSA plugin will not negotiate
`arecord -D pipewire` and `-D default` both fail at every rate and channel count, so Dire
Wolf cannot use PipeWire and must grab the Digirig directly via `plughw`, which prevents any
other application from sharing that audio interface. The running daemon reports libpipewire
1.6.6 while the ALSA plugins in the store are 1.4.9 and 1.6.5 — a version skew is the
leading suspect. Not a tncd issue; it just blocks sharing the radio's audio.
