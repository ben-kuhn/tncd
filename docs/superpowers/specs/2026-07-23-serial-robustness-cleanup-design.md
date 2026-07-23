# Serial Robustness + Cleanup Design

**Status:** Approved design.
**Date:** 2026-07-23.
**Context:** Follow-ups surfaced during the phase-3.5 SREJ OTA bench session
(KPC-3+ debugging), plus the accumulated deferred Minor findings from the
phase-3/3.5 reviews. Small, focused improvements before phase 4.

## Purpose

Three independent items:
- **A. Busy-port error clarity** — a clearer message when a serial port can't be
  opened because it's in use.
- **B. KISS-entry probe hardening** — stop the probe from silently assuming KISS
  entry succeeded when it can't verify it (this misled the OTA session).
- **C. Minor-findings cleanup sweep** — fix the worthwhile deferred Minors, leave
  the intentional ones with a note.

## A. Busy-port error clarity

`go.bug.st/serial` already calls `acquireExclusiveAccess()` (`TIOCEXCL`) on open
(`serial_unix.go:280`), so tncd already holds the port exclusively: a second
tncd, or any other program opening the port after tncd, fails with `EBUSY`, and
tncd already surfaces that error. The only problem is the message is cryptic
("device or resource busy").

**Change:** in `kiss/serial.go` `serialTransport.Open()`, when `openFn` returns
an error, detect the `EBUSY` case (`errors.Is(err, syscall.EBUSY)`) and wrap it
in a clearer, **generic** message:

> `serial: cannot open /dev/ttyUSB1: port is busy (in use or exclusively locked) — free it and retry`

All other open errors keep their current wrapping. Deliberately generic: **no
process-name guesses** (the culprit varies wildly across platforms and use
cases) and **no lock files** (multiple tncd instances on *different* ports are
fine, so a per-port lock would be pointless, and UUCP lock-file machinery —
stale detection, `/run/lock` permissions, crash cleanup — is not worth it given
`TIOCEXCL` already covers the real cases).

**Test:** a unit test that injects an `openFn` returning `syscall.EBUSY` and
asserts the wrapped error mentions "busy"; and one returning a different error
asserts the message is unchanged.

## B. KISS-entry probe hardening

`EnterKISS()` (`kiss/serial.go`) currently probes once: it sends a CR, waits, and
if it reads **no response** it concludes the TNC is "already in KISS" and
**skips the init string entirely**, logging "assuming KISS mode." That inference
is wrong — silence can also mean the TNC is still rebooting after `RESET`,
mis-configured (baud/flow control), or wedged. In the OTA session the KPC-3+ was
still rebooting; tncd skipped the init and the TNC never entered KISS, but the
log claimed success.

**Reworked flow** (only when `init_string` is configured — a TNC with no
`init_string`, e.g. a Direwolf PTY, is assumed already in KISS and this whole
path is skipped, unchanged):

1. **Probe with retries** — call `tnc_in_command_mode()` up to 3 times, spaced by
   `init_delay`, before concluding. This gives a slow/rebooting TNC time to come
   up (the exact KPC-after-`RESET` case).
2. **Command-mode detected** (any probe saw printable command-mode text) → send
   the init string (as today) → re-probe once → if still in command mode, return
   an error ("TNC still in command mode after init_string"); otherwise log "TNC
   confirmed in KISS mode after init."
3. **Never any response** across the retries (ambiguous) → **send the init string
   anyway**, then log a `WARNING`: KISS entry could not be confirmed (no probe
   response); this is normal if the TNC was already in KISS, but if the TNC does
   not transmit, check baud rate / flow control / cabling. Return `nil` (proceed).

**Why send-init-on-silence is safe:** it is only reached when the operator
configured an `init_string` (so they expect a command-mode TNC). For a TNC
genuinely already in KISS, the init bytes (`INTFACE KISS\rRESET\r` etc.) are not
`C0`-framed, so a KISS decoder ignores them — no transmission, no harm. For a
wedged/slow TNC it is the rescue. So no regression for already-KISS TNCs
(Direwolf, Mobilinkd between sessions); the only new artifact is an honest
WARNING instead of a false success claim.

**Implementation notes:** `tnc_in_command_mode()` returns a bool today; the retry
loop can call it directly (any `true` → command mode). To distinguish "saw
non-command-mode bytes" from "saw nothing," the probe helper should also report
whether it read any bytes — either return `(inCmd bool, sawBytes bool)` or a
small 3-state result. Keep the existing printable-ASCII classification. The retry
count (3) and spacing (`init_delay`) are fixed, not new config (YAGNI).

**Tests:** with a fake serial (the existing test harness injects `rw`):
- TNC in command mode on first probe → init sent, confirmed KISS.
- TNC silent on the first probe(s) then command-mode text on a later retry →
  init sent, confirmed KISS (proves the retry catches a slow TNC).
- TNC never responds → init sent anyway, warning logged, returns nil (no error).
- Still in command mode after init → returns error.
- `init_string == ""` → early return, no probe (unchanged).

## C. Minor-findings cleanup sweep

Triage the deferred Minor findings from the phase-3/3.5 review ledger. **Fix**
the worthwhile ones; **leave** the intentional/unreachable ones (a short note in
the report is enough — no code change).

**Fix:**
- **KISS param escaping (real bug).** The KISS command/param builder does not
  KISS-escape value bytes, so a param value of `0xC0` (FEND) or `0xDB` (FESC) —
  e.g. `persistence = 192` in a user config — corrupts the frame. Escape the
  value bytes per the KISS spec (`0xC0`→`0xDB 0xDC`, `0xDB`→`0xDB 0xDD`). Add a
  test with a `0xC0`/`0xDB` value.
- Dead code: remove genuinely unused fields/helpers/test vars still present
  (e.g. unused `Timer.fired`, `serialPort` field, `dupFD` helper, `rawWithVia`
  test var, dead `shouldStop` read) — verify each is truly unused before removing.
- Stale comments/docs: `// mod-8` on `Frame.NR,NS` (holds 0–127 in mod-128);
  `mustParseAddr` doc says "panics" but doesn't; any leftover "Sends SABM P=1"
  docstrings; the `rtscts_other.go` "hard error" comment wording.
- Weak/missing tests: `TestFallbackOnTimeoutAtMaxV22`'s `sabmCount < 1` →
  `sabmCount == 1`; add a `ParseModulo(<short>, 128)` error-path test; add a
  `SREJMulti` XID encode/parse round-trip test.

**Leave (note only, no change):** intentional or unreachable items — `encode()`
SSID>15 not masked (unreachable via production paths), callsign truncation >10
bytes (matches the Python reference by design), `MaxPayload` enforced at the
frontend not the codec, `sweepIdleClients` iterating a live slice (safe by
convention), the XID response branch not gating `f.PF` (accepted; can't loop),
packaging `postInstall` guards and `platforms.linux` (phase-4 packaging scope),
and documented CLI limitations (`-vt` clusters, `K` raw frames vs `-t`).

Each item verified individually before change; anything that turns out to be
load-bearing is left and noted rather than removed.

## Non-goals

- UUCP lock-file handling (A) — rejected above.
- Any config surface for the probe retries (B) — fixed constants.
- Upstreaming a flow-control API to `go.bug.st/serial` to retire the RTSCTS
  reflection hack — a separate, larger effort (deferred).
- Behavior changes to the phase-3/3.5 items marked "leave" in C.

## Testing & sequencing

Go unit tests for A and B (fake-serial harness) and for the KISS-escaping fix in
C. No e2e or OTA gate needed — these are host-side robustness/cleanup with no
on-air protocol change (B's init-on-silence was validated in spirit by the OTA
session). Branch `feature/serial-robustness` off `main`; merge `--no-ff` after
review. No version bump.

## Exit criteria

1. All new + existing `go test` pass.
2. A: `EBUSY` open failures produce the clearer generic message; other errors
   unchanged.
3. B: probe retries; init sent on silence; honest WARNING instead of "assuming
   KISS"; no regression for `init_string`-less (already-KISS) TNCs.
4. C: the KISS-escaping bug fixed with a test; dead code/stale comments/weak
   tests addressed; intentional items left with a note.
