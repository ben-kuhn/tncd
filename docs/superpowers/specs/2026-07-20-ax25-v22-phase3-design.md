# tncd 2.0 Phase 3 (AX.25 v2.2) Design

**Status:** Approved design.
**Umbrella:** `2026-07-14-tncd-2.0-go-port-design.md` (§ "AX.25 v2.2 (phase 3)", § Phasing item 3).
**Date:** 2026-07-20.

## Purpose

Add AX.25 version 2.2 support to the Go port: SABME / mod-128 (extended
sequence numbers) with SABM / mod-8 auto-fallback, and passive XID parameter
negotiation. v2.2 is entirely host-side — a KISS TNC is a byte-transparent
layer-1 framer and never parses the control field — so peer capability is
discovered over the air (SABME with SABM fallback), not from the modem.

## Framing note: greenfield, not a port

Phases 1–2 mirrored the frozen in-tree `tncd.py` 1.3.1 reference. Phase 3 has
**no Python counterpart** — v2.2 was never implemented in the Python line. The
reference implementation for this phase is **Direwolf 1.8.1** (`ax25_link.c`,
`xid.c`), which is also the interop and OTA validation target. Design
decisions below cite Direwolf source where behavior is load-bearing.

## Scope

In scope:

- SABME initiate + accept; mod-128 (2-byte I/S control fields, 7-bit N(R)/N(S),
  window up to 127).
- Auto-fallback SABME → SABM / mod-128 → mod-8 on DM, FRMR, or T1/N2 timeout.
- Passive XID: respond to an XID command with a min-negotiated XID response;
  never initiate XID. We advertise **SREJ off** (`srej_none`), which is what
  suppresses Direwolf's default-on single-SREJ (see below).
- **Tolerate received single-SREJ** (interop robustness): decode incoming SREJ
  S-frames and honor them by retransmitting the specifically requested frame.
  We never *send* SREJ and never buffer out-of-order frames — that is phase 3.5.
- Per-port `ax25_version = 2.0 | 2.2` config, default `2.2`.

Explicit non-goals — **deferred to phase 3.5** (full selective-repeat; its own
spec/plan/OTA gate):

- **Sending** SREJ + the receive-side reassembly buffer (`rxdata_by_ns`-style
  out-of-order buffering and in-order delivery). This is the part that speeds up
  the Winlink *download* direction (remote→tncd, where tncd is the receiver) and
  is the highest-risk change to `dispatchI`; it is intentionally split out.
- Multi-SREJ (info-field form) and XID up-negotiation of `srej_multi`.

Explicit non-goals — YAGNI (not planned):

- XID **initiation** (we only respond).
- TEST-frame handling.
- Per-peer version / noxid config lists (fallback covers old peers).
- mod-128 for UI / unproto (connected-mode only).

## Reference behavior (Direwolf 1.8.1)

Verified in `ax25_link.c`:

- **Default is v2.2.** Unless the peer is in a `V20` list or `maxv22 == 0`,
  Direwolf initiates with SABME (`ax25_link.c:979-994`).
- **Initiator sends XID after UA.** On receiving the UA for its SABME, the
  initiator sends an **XID command, P=1**, then expects an XID response
  (`4704-4712`). Therefore when tncd *answers* a default Direwolf connect it
  **will** receive an XID command it must handle.
- **XID responder rule:** "take the minimum of what he wants and what I can do,
  adjust my working configuration and send it back" (`5010-5022`,
  `xid_frame` → `negotiation_response`).
- **SREJ defaults ON for v2.2.** `srej_enable` *"starts out as `srej_none` for
  v2.0 or `srej_single` for v2.2"* (`ax25_link.c:273-275`); Direwolf implements
  the full receiver side (`rxdata_by_ns[128]`, `2713-2729`). XID is how it gets
  negotiated *off* (to `srej_none`) or *up* (to `srej_multi`); negotiation keeps
  the lower value. So **our XID response advertising `srej_none` is what turns
  Direwolf's SREJ off** when Direwolf connects to us. In the reverse case (tncd
  initiates, sends no XID), Direwolf's answerer keeps `srej_single` and may send
  us an SREJ on loss — which is why phase 3 decodes and honors a received
  single-SREJ rather than dropping it.
- **Old-peer handling:** a pre-2.2 station DMs the SABME or FRMRs the XID; the
  initiator then uses v2.0 (`4704-4708`). This is what our fallback consumes
  from the *initiator* side and produces from the *answerer* side.

## 1. Mode selection & fallback

`Conn` gains `modulo uint8` (8 or 128) and `triedFallback bool`. We **replicate
Direwolf's retry scheme exactly** (`ax25_link.c` `t1_expiry`, `dm_frame`,
`frmr_frame`; verified 1.8.1 behavior).

**Retry budget.** `maxV22 = N2Retry / 3` (integer division), matching Direwolf's
`maxv22 = AX25_N2_RETRY_DEFAULT / 3` (`config.c:905`). At our default
`N2Retry = 10` this is **3** — three SABMEs are transmitted before the timeout
fallback. The first SABME is transmission try 1 (Direwolf `establish_data_link`
does `SET_RC(1)` then sends). `maxV22 == 0` means "v2.0 only" and is handled at
`Connect` time (never send SABME), mirroring Direwolf's `maxv22 == 0` guard.

**Outgoing `Connect` on a 2.2 port:** send `SABME`, `state = Connecting`,
`modulo = 128`, `triedFallback = false`. Fallback (→ `modulo = 8`,
`triedFallback = true`, resend as `SABM`):

- `DM` (F=1) while Connecting-extended (`dispatchDM`) — **immediate** fallback.
  Direwolf found real TNCs (KPC-3+) answer an un-understood SABME with DM rather
  than FRMR, so this is the common old-peer path (`ax25_link.c:4559`).
- `FRMR` while Connecting-extended (`dispatchFRMR`) — **immediate** fallback
  (`ax25_link.c:4841`, `set_version_2_0`).
- T1 timeout (`t1Expired`): on each expiry the poll counter increments; when it
  reaches `maxV22`, switch to `modulo = 8` and continue the *same* retry budget
  as `SABM` (do **not** reset the counter — Direwolf keeps `rc` running). Give
  up with `FailTimeout` when the counter reaches `N2Retry` (10 total tries: 3
  SABME + 7 SABM at defaults).

`triedFallback` prevents a second downgrade loop. Exact per-expiry counts are
pinned by tests asserting the SABME→SABM transition at try 3 and give-up at try
10. On a 2.0 port `Connect` is unchanged (`SABM`, `modulo = 8`).

**Incoming:**

- `dispatchSABM` → `modulo = 8` (unchanged).
- `dispatchSABME` → if the port is 2.2 **and** `IsLocal(dst)`: `modulo = 128`,
  send `UA`, `Connected`, await the peer's XID command. If the port is 2.0-only,
  keep DM-rejecting (Direwolf then retries SABM). The existing shared-channel
  `IsLocal` gate is preserved.

## 2. Frame codec (`ax25/frame.go`)

Mod-128 changes **only I and S frames** to a **2-byte control field**. U-frames
(UI, SABM, **SABME**, UA, DM, DISC, FRMR, **XID**) stay 1 byte and are
modulo-independent. Extended layout: control byte 1 carries N(S)/S-bits + the
type bit (identical low bits to mod-8, so I/S/U classification is
modulo-independent); control byte 2 carries `N(R)<<1 | PF`.

Add the fourth supervisory type **`SREJ`** (`SS = 11`) to `ax25.FrameType` and
the S-frame parse/encode (both moduli). Today the codec maps `SS = 11` to
`UnknownType` and the engine silently drops it; phase 3 decodes it so a received
single-SREJ can be honored (§3). We only ever *decode* SREJ in phase 3; encoding
it is phase 3.5.

**Encode:** add `Frame.Modulo uint8`. `Bytes()` emits the 2-byte control for
I/S when `Modulo == 128` (else 1 byte, exactly as today). The l2 engine stamps
`f.Modulo = c.modulo` on every I/S/S-frame it builds.

**Decode — two-stage RX** (`bridge.OnKISSFrame`): a raw KISS frame's I/S control
width can't be decoded without the link modulo, but addresses, U-frames, and the
I/S/U classification always can. So:

1. `ax25.Parse(raw)` as today → correct addresses, type, PF, U-frames; I/S
   sequence fields provisional (mod-8).
2. If the frame is I or S, the bridge asks the l2 table for the link modulo via
   a new `l2.Table.ModuloFor(port, local, remote) int` (default 8). If 128,
   re-decode N(S)/N(R)/PID/Info from the raw control offset as extended.

`ax25` stays free of any `l2` dependency — the **bridge** orchestrates, since it
already holds `b.l2`. Only I/S frames on an established mod-128 link ever need
stage 2; all connection-setup frames (SABME/UA/DM/XID) are U-frames decoded
correctly in stage 1.

The internal retransmit re-parse (`l2.go:664`, `retransmitFrom`) parses stored
raw bytes with `c.modulo` (the buffer was encoded at that modulo).

## 3. L2 engine generalization (`ax25/l2/`)

- Replace every hardcoded `% 8` (send/recv seq advance, `newlyAcked`, ack loop,
  out-of-sequence gap calc, retransmit walk) with `% c.modulo`. Maps keyed by
  `uint8` already hold 0–127; `retransmitBuf` / `iframeTimestamps` unchanged.
- Window: the `MaxWindow` config clamp becomes modulo-aware — `1..7` for mod-8
  (unchanged), `1..63` for mod-128 (matching Direwolf's `AX25_K_MAXFRAME_
  EXTENDED_MAX = 63`, restricted below the theoretical 127). The **default is
  unchanged at 3** — slow links keep small windows; we do not inflate. The
  backwards-N(R) guard (`newlyAcked > MaxWindow`) is correct as-is under either
  modulo.
- Fallback bookkeeping (`modulo`, `triedFallback`) branches in `dispatchDM`,
  `dispatchFRMR`, `t1Expired`, and mode-selects in `Connect` / `dispatchSABME`.
- **Received single-SREJ** (`dispatchS`): per AX.25 2.2, `SREJ N(R)=k`
  acknowledges I-frames **up through k−1** and requests retransmission of
  **only** frame k. So: `ackFrames(c, k)` (which purges ≤ k−1 and leaves k in
  `retransmitBuf`), then retransmit exactly frame k (reusing the single-frame
  send path, **not** `retransmitFrom`'s go-back-N walk), then honor P/F like any
  S-frame. This path is only exercised in the tncd-initiates-no-XID case; when we
  answer XID we advertise `srej_none` and Direwolf sends REJ instead. Sending
  SREJ and out-of-order receive buffering are phase 3.5.

## 4. XID — passive responder

New U-frame type `XID` (base `0xAF`) in `ax25.FrameType` with parse/encode and
an `OnFrame` dispatch entry. XID carries no PID; its info field is the ISO-8885
TLV block (`FI=0x82`, `GI`, PI/PL/PV tuples) that Direwolf's `xid.c` produces.

**`ax25/xid.go`:** `parseXID(info) → xidParams` and `encodeXID(params) → info`,
ported from Direwolf `xid_parse` / `xid_encode`. Relevant fields: modulo, RX
window, RX I-field length (N1, XID carries it in bits), SREJ, full-duplex; T1/N2
optional and omitted. Golden-bytes tests use info fields **captured from
Direwolf** on the bench.

**`dispatchXID`** (mirrors `xid_frame` `mdl_state_0_ready`, command branch):

- Act only on an XID **command, P=1**, addressed to us (`IsLocal`) for an
  existing connection. An XID **response** is unexpected (we never initiate) →
  log and ignore.
- **Min-negotiate** against our capabilities, replicating Direwolf's
  `negotiation_response` ("take the minimum of what he wants and what I can do"):
  `window = min(theirs, ours)`, `N1 = min(theirs, ours)`, `modulo` = ours (128),
  **SREJ = none** (never advertise → Direwolf keeps SREJ off), full-duplex = off.
  Apply the negotiated window / N1 to the conn.
- Send an `XID` **response, F=1** carrying the negotiated params.

**Our advertised values equal Direwolf's defaults**, so with default Direwolf the
negotiation is a no-op: N1 = **256 bytes** (Direwolf `AX25_N1_PACLEN_DEFAULT =
256` — identical to our existing 256-byte info default), window = our config
default **3** (`min(3, Direwolf's extended default 32) = 3`), SREJ off. We never
initiate XID; as initiator to Direwolf, sending none leaves Direwolf on its
defaults, which interoperates as long as our N1 / window ≤ its max — which holds.

## 5. Config & CLI

`internal/config`: add `ax25_version` to `[client.N]`, values `2.0` / `2.2`,
default `2.2`. Startup validation rejects other values with a file/line-precise
error. Update `example.go` (commented default) and `tncd genconfig` output. No
`noxid` / `V20` peer lists.

## 6. Testing

**Unit (`go test`):**

- `ax25`: golden bytes for mod-128 I/S encode + decode (2-byte control); XID
  parse + encode against Direwolf-captured info fields; SABME/UA/DM/FRMR U-frame
  round-trips (SABME already present).
- `ax25/l2` behavioral, each a named case:
  - extended connect → UA → XID command → XID response → mod-128 I/O;
  - fallback on DM; fallback on FRMR; fallback on T1/N2 timeout-exhaustion;
  - incoming SABME accepted on a 2.2 port;
  - incoming SABME → DM on a 2.0-only port;
  - mod-128 sequence advance wrapping past 7 (e.g. N(S) 6 → 7 → 8 … 127 → 0);
  - received single-SREJ acks ≤ k−1 and retransmits exactly frame k (no
    go-back-N of later frames).

**E2E (`e2e/`):** new test — Direwolf in default v2.2 mode over the existing
PipeWire audio cross-link, connected-mode data round-trip to tncd, asserting
mod-128 was negotiated (window / N(S) visible in the monitor stream). Reuses the
existing Direwolf harness; the documented machine-specific `TestMultiPort`
PipeWire flake remains out of scope.

**OTA gate:** **tncd ↔ Direwolf over real radios**, mod-128 Winlink-style
connected round-trip, using **one TNC** (a single radio/interface on the tncd
side is sufficient — v2.2 is host-side, so proving it on one TNC proves it for
all; the full hardware matrix is phase 4's job, not phase 3's). Recorded in a
new `docs/superpowers/specs/phase3-ota-checklist.md`. This is the bench task and
the phase exit criterion — the implementation plan ends here, not at a tag.

## Sequencing

The umbrella placed phases on `v2-go-port`, but that branch already merged to
`main` (73868e9). Phase 3 branches from `main` as `feature/ax25-v22` and merges
back with `--no-ff` once unit + e2e + OTA all pass. No version bump or tag: v2.2
rides to the eventual v2.0.0 tag in phase 4, per the umbrella.

**Phase 3.5 (full SREJ)** follows immediately as its own spec + plan + OTA gate,
branching from `main` after phase 3 merges: adds SREJ *sending* + the
receive-side out-of-order reassembly buffer + in-order delivery (and optionally
multi-SREJ via XID up-negotiation). It is the selective-repeat throughput win
for the Winlink download direction, split out because it is the highest-risk
change to `dispatchI` and independently benchable. Phase 3 lays the groundwork it
needs: the `SREJ` frame type, mod-128 codec, and single-SREJ retransmit path.

## Exit criteria

1. All new + existing `go test` pass (`ax25`, `ax25/l2`, `internal/...`).
2. E2E connected-mode round-trip against Direwolf v2.2, mod-128 negotiated.
3. Real-radio OTA: tncd ↔ Direwolf mod-128 connected round-trip, logged in the
   phase-3 OTA checklist.
4. 2.0-only ports and existing mod-8 peers demonstrably unaffected (fallback +
   the incoming-SABME→DM path).
