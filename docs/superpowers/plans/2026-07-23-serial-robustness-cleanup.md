# Serial Robustness + Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Clearer busy-port errors, an honest/robust KISS-entry probe, and a sweep of the deferred phase-3/3.5 Minor findings.

**Architecture:** All host-side, no on-air protocol change. A/B touch the serial transport (`kiss/serial.go`); C touches the AX.25 codec and assorted small spots. Reference: spec `docs/superpowers/specs/2026-07-23-serial-robustness-cleanup-design.md`.

**Tech Stack:** Go 1.x, `go.bug.st/serial`, `golang.org/x/sys/unix`. The serial tests use the existing `fakeSerial` (scripts one response per probe `\r`) and `openPort` injection in `kiss/serial_test.go`.

## Global Constraints

- Module path `github.com/ben-kuhn/tncd/v2`. `go` at `/nix/store/gb0njhqswlc5n127ikgyikvq39r40l6f-go-1.26.4/bin/go` if not on PATH. This env lacks gcc — use `CGO_ENABLED=0` for `go test ./...` / `go vet`.
- Branch `feature/serial-robustness` (already created off `main`). Commit after each task with that task's message.
- **Post-OTA policy:** bug-for-bug Python parity is no longer required; edge-case fixes that deviate from `tncd.py` are allowed and must be noted as deliberate divergences (comment + report). See spec §C.
- B's `init_string`-configured probe rework must NOT regress `init_string == ""` (already-KISS) TNCs — that path returns early, untouched.
- B: retry count (3) and spacing (`init_delay`) are fixed constants, not new config.
- Already verified during planning (do NOT redo as "bugs"): the KISS param `0xC0`/`0xDB` escaping is already implemented (`kiss/framing.go` `WrapCommand`) and tested (`TestWrapCommandEscapesValueByte`). The callsign-length and `agwpe.Build` 10-byte-field cases are protocol-correct / unreachable in production; only the `encode` SSID mask (Task 3) is done as defensive hardening.

---

### Task 1: A — clearer busy-port error

**Files:**
- Modify: `kiss/serial.go` (`Open`)
- Test: `kiss/serial_test.go`

**Interfaces:**
- Produces: on `EBUSY` from the port open, `Open` returns an error whose message contains "busy"; other open errors keep the existing `serial: open <dev>: <err>` wrapping.

- [ ] **Step 1: Write the failing test**

Add to `kiss/serial_test.go` (imports `errors`, `syscall`, `goserial` are available; add `syscall` to the import block if missing):

```go
func TestOpenBusyPortClearError(t *testing.T) {
	st := NewSerialTransport(SerialConfig{Device: "/dev/ttyUSB9"}).(*serialTransport)
	st.openPort = func(string, *goserial.Mode) (modemPort, error) {
		return nil, syscall.EBUSY
	}
	err := st.Open()
	// Assert the FRIENDLY wording ("in use"), which is NOT in the raw errno
	// ("device or resource busy") — so this genuinely fails before the fix.
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("EBUSY open error = %v, want the friendly \"in use\" wording", err)
	}
}

func TestOpenOtherErrorUnchanged(t *testing.T) {
	st := NewSerialTransport(SerialConfig{Device: "/dev/ttyUSB9"}).(*serialTransport)
	st.openPort = func(string, *goserial.Mode) (modemPort, error) {
		return nil, errors.New("boom")
	}
	err := st.Open()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("non-EBUSY error = %v, want it wrapped through", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `CGO_ENABLED=0 go test ./kiss/ -run 'OpenBusy|OpenOther' -v`
Expected: `TestOpenBusyPortClearError` FAIL (raw errno "device or resource busy" lacks "in use"); `TestOpenOtherErrorUnchanged` PASS (the current wrapping already contains "boom").

- [ ] **Step 3: Implement**

In `kiss/serial.go`, add `"errors"` and `"syscall"` to the imports. Replace the open-error handling:

```go
	port, err := openFn(s.cfg.Device, mode)
	if err != nil {
		if errors.Is(err, syscall.EBUSY) {
			return fmt.Errorf("serial: cannot open %s: port is busy (in use or exclusively locked) — free it and retry", s.cfg.Device)
		}
		return fmt.Errorf("serial: open %s: %w", s.cfg.Device, err)
	}
```

The friendly message contains "in use" (matching the Step 1 test); the fallback keeps `%w` so `TestOpenOtherErrorUnchanged` sees "boom".

- [ ] **Step 4: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./kiss/ -v` — Expected: PASS (new tests + existing).

- [ ] **Step 5: Commit**

```bash
git add kiss/serial.go kiss/serial_test.go
git commit -m "feat(kiss): clearer message when a serial port is busy"
```

---

### Task 2: B — KISS-entry probe hardening

**Files:**
- Modify: `kiss/serial.go` (`EnterKISS`, `tnc_in_command_mode`)
- Test: `kiss/serial_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `EnterKISS` retries the probe up to 3 times spaced by `init_delay`; on command-mode detection it sends the init and verifies; on **silence** (no bytes across retries) it sends the init anyway and logs a WARNING (returns nil); on **non-command bytes** (already-KISS) it does nothing (returns nil); `init_string == ""` still returns early. `tnc_in_command_mode` becomes `probeCommandMode() (inCmd, sawBytes bool)`.

- [ ] **Step 1: Write the failing tests**

Add to `kiss/serial_test.go` (keep the three existing `TestEnterKISS*` tests — they must still pass):

```go
func TestEnterKISSRetryCatchesSlowTNC(t *testing.T) {
	// Silent on the first two probes (TNC still rebooting), cmd text on the 3rd.
	fs := &fakeSerial{responses: [][]byte{{}, {}, []byte("cmd:"), {}}}
	st := newTestSerial(fs, SerialConfig{InitString: `INT KISS\rRESET\r`, InitDelay: time.Millisecond})
	if err := st.EnterKISS(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fs.written.String(), "INT KISS\r") {
		t.Fatalf("init not sent after retry caught cmd mode; written=%q", fs.written.String())
	}
}

func TestEnterKISSSendsInitOnSilence(t *testing.T) {
	// Never any response: ambiguous. Init must be sent anyway; no error.
	fs := &fakeSerial{responses: [][]byte{{}, {}, {}}}
	st := newTestSerial(fs, SerialConfig{InitString: `INT KISS\rRESET\r`, InitDelay: time.Millisecond})
	if err := st.EnterKISS(); err != nil {
		t.Fatalf("silence should not error: %v", err)
	}
	if !strings.Contains(fs.written.String(), "INT KISS\r") {
		t.Fatalf("init not sent on silence; written=%q", fs.written.String())
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `CGO_ENABLED=0 go test ./kiss/ -run 'EnterKISSRetry|EnterKISSSendsInitOnSilence' -v`
Expected: FAIL — `RetryCatchesSlowTNC` fails (current code probes once, sees silence, returns without init); `SendsInitOnSilence` fails (current code skips init on silence).

- [ ] **Step 3: Refactor the probe to report whether it saw bytes**

In `kiss/serial.go`, change `tnc_in_command_mode() bool` to `probeCommandMode() (inCmd, sawBytes bool)`. Keep the drain / CR / wait / read / classify logic; set `sawBytes = len(resp) > 0` (raw read, before filtering) and `inCmd` from the existing printable-ASCII classification. Replace the internal `return false` / `return true` with `return inCmd, sawBytes`. Concretely:

```go
// probeCommandMode probes the TNC. Returns (inCmd, sawBytes): inCmd is true if
// the response is printable command-mode text; sawBytes is true if any bytes
// were read at all (used to tell silence from a KISS-mode echo).
func (s *serialTransport) probeCommandMode() (inCmd, sawBytes bool) {
	drain := make([]byte, 256)
	for {
		n, _ := s.rw.Read(drain)
		if n == 0 {
			break
		}
	}
	_, _ = s.rw.Write([]byte("\r"))
	time.Sleep(s.probeWait)
	buf := make([]byte, 1024)
	n, _ := s.rw.Read(buf)
	resp := buf[:n]
	log.Printf("serial: TNC probe raw response: %q", resp)
	if len(resp) == 0 {
		return false, false
	}
	filtered := resp[:0]
	for _, b := range resp {
		if b != 0xC0 && b != 0x00 {
			filtered = append(filtered, b)
		}
	}
	filtered = trimSpace(filtered)
	if len(filtered) == 0 {
		return false, true // saw only KISS framing/NULs → already in KISS
	}
	for _, b := range filtered {
		if !((b >= 0x20 && b < 0x7F) || b == 0x0A || b == 0x0D) {
			return false, true // non-printable → treat as KISS
		}
	}
	log.Printf("serial: TNC probe: command-mode response %q", filtered)
	return true, true
}
```

- [ ] **Step 4: Rework `EnterKISS`**

Replace the body of `EnterKISS` (keeping the empty-`InitString` early return) with the retry + branch logic, and extract the init send into `sendInit`:

```go
func (s *serialTransport) EnterKISS() error {
	if s.cfg.InitString == "" {
		return nil
	}

	// Probe with retries: a TNC rebooting after RESET may not answer immediately.
	const probeAttempts = 3
	inCmd, sawBytes := false, false
	for i := 0; i < probeAttempts; i++ {
		c, saw := s.probeCommandMode()
		if saw {
			sawBytes = true
		}
		if c {
			inCmd = true
			break
		}
		if i < probeAttempts-1 {
			time.Sleep(s.cfg.InitDelay)
		}
	}

	if inCmd {
		if err := s.sendInit(); err != nil {
			return err
		}
		if c, _ := s.probeCommandMode(); c {
			return fmt.Errorf("serial: TNC still in command mode after init_string — " +
				"check that the init commands are correct for this TNC")
		}
		log.Printf("serial: TNC confirmed in KISS mode after init")
		return nil
	}

	if sawBytes {
		// Saw only KISS framing/non-printable bytes → already in KISS. Nothing to do.
		return nil
	}

	// Silence across all retries: ambiguous (already-KISS-but-silent, or a
	// wedged/mis-configured TNC). Send the init anyway — it is harmless if the
	// TNC is already in KISS (non-C0 bytes are ignored) and rescues one that is
	// stuck — then warn honestly rather than claiming success.
	if err := s.sendInit(); err != nil {
		return err
	}
	log.Printf("serial: WARNING: could not confirm KISS entry on %s (no probe response); "+
		"normal if the TNC was already in KISS, but if it does not transmit check "+
		"baud rate, flow control (rtscts), and cabling", s.cfg.Device)
	return nil
}

// sendInit writes the init_string command lines (split on the literal \n escape).
func (s *serialTransport) sendInit() error {
	lines := strings.Split(s.cfg.InitString, `\n`)
	for _, line := range lines {
		cmd := resolveEscapes(line)
		log.Printf("serial: TNC init: %q", line)
		if _, err := s.rw.Write([]byte(cmd)); err != nil {
			return fmt.Errorf("serial: writing init line: %w", err)
		}
		time.Sleep(s.cfg.InitDelay)
	}
	return nil
}
```

Delete the old `tnc_in_command_mode` (now `probeCommandMode`) and remove the old init-send loop from the previous `EnterKISS`.

- [ ] **Step 5: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./kiss/ -v`
Expected: PASS — the two new tests plus all three existing `TestEnterKISS*` (SendsInit: cmd on probe 1 → init + confirm; SkipsInitWhenAlreadyKISS: KISS echo → `sawBytes` true, no init; ErrorsWhenInitFails: cmd then cmd → error).

- [ ] **Step 6: Commit**

```bash
git add kiss/serial.go kiss/serial_test.go
git commit -m "fix(kiss): retry KISS-entry probe, send init on silence, honest warning"
```

---

### Task 3: C — defensive hardening + missing tests

**Files:**
- Modify: `ax25/address.go` (`encode`)
- Test: `ax25/address_test.go`, `ax25/frame_test.go`, `ax25/xid_test.go`, `ax25/l2/l2_test.go`

**Interfaces:**
- Produces: `Address.encode` masks SSID to 4 bits so an out-of-range SSID cannot corrupt the SSID byte (deliberate divergence from `tncd.py`, which does not).

- [ ] **Step 1: Write the failing/again tests**

Add to `ax25/address_test.go`:

```go
func TestEncodeMasksSSID(t *testing.T) {
	// SSID out of range (>15) must not spill into the CRH/reserved bits.
	a := Address{Call: "KU0HN", SSID: 0x1F} // 31, invalid
	b := a.encode(false, true)
	// SSID byte: 0x60 | (ssid&0x0F)<<1 | ext(1); ssid&0x0F = 0x0F.
	want := byte(0x60 | (0x0F << 1) | 0x01)
	if b[6] != want {
		t.Fatalf("encode SSID byte = %#02x, want %#02x (SSID masked to 4 bits)", b[6], want)
	}
}
```

Add to `ax25/frame_test.go` (short extended frame → error):

```go
func TestParseModuloExtendedShort(t *testing.T) {
	// 14 address bytes + 1 control byte = 15: enough for mod-8, but an extended
	// I/S frame needs a 2nd control byte. ParseModulo(...,128) must error, not panic.
	raw := make([]byte, 15)
	raw[14] = 0x00 // I-frame marker (bit0=0)
	if _, err := ParseModulo(raw, 128); err == nil {
		t.Fatal("ParseModulo(15-byte, 128) on an I-frame: expected error for missing 2nd control byte")
	}
}
```

Add to `ax25/xid_test.go` (SREJMulti round-trip):

```go
func TestXIDMultiSREJRoundTrip(t *testing.T) {
	p := XIDParams{SREJ: SREJMulti, Modulo: 128, IFieldLenRxBytes: 256, WindowRx: 7}
	got, err := ParseXID(p.Encode(true))
	if err != nil {
		t.Fatal(err)
	}
	if got.SREJ != SREJMulti {
		t.Fatalf("round-trip SREJ = %v, want SREJMulti", got.SREJ)
	}
}
```

Tighten the weak assertion in `ax25/l2/l2_test.go` `TestFallbackOnTimeoutAtMaxV22`: change `if sabmCount < 1 {` to `if sabmCount != 1 {` (exactly one SABM has been sent at the point the loop exits) and update the message accordingly.

- [ ] **Step 2: Run to verify (fail where expected)**

Run: `CGO_ENABLED=0 go test ./ax25/... -run 'EncodeMasksSSID|ParseModuloExtendedShort|XIDMultiSREJRoundTrip' -v`
Expected: `TestEncodeMasksSSID` FAIL (encode doesn't mask yet). The other two should already PASS (they lock in existing-correct behavior — that's fine, note it). Run the l2 test too: `CGO_ENABLED=0 go test ./ax25/l2/ -run FallbackOnTimeout -v` (must still PASS with the tighter assertion; if it fails, the count isn't 1 — investigate before loosening).

- [ ] **Step 3: Implement the SSID mask**

In `ax25/address.go` `encode`, change the SSID byte line and add a divergence note:

```go
	// SSID byte: crhBit<<7 | 0x60 | ssid<<1 | extBit.
	// Mask SSID to 4 bits: a directly-constructed Address with SSID>15 must not
	// corrupt the CRH/reserved bits. (Deliberate divergence from tncd.py, which
	// does not mask; ParseAddress already rejects >15 for parsed callsigns.)
	b[6] = crhBit<<7 | 0x60 | (a.SSID&0x0F)<<1 | extBit
```

- [ ] **Step 4: Run to verify pass**

Run: `CGO_ENABLED=0 go test ./ax25/... ./ax25/l2/ -v` — Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ax25/address.go ax25/address_test.go ax25/frame_test.go ax25/xid_test.go ax25/l2/l2_test.go
git commit -m "fix(ax25): mask SSID in encode (divergence); add SSID/ParseModulo/SREJMulti tests"
```

---

### Task 4: C — dead-code + stale-comment tidy

**Files:** various (verify each before touching). No behavior change.

**Interfaces:** none (mechanical cleanup).

- [ ] **Step 1: Fix stale comments/docs**

- `ax25/frame.go`: the `NR, NS uint8 // mod-8; ...` comment — change `// mod-8;` to `// 0–7 (mod-8) or 0–127 (mod-128);`.
- `ax25/l2/l2.go`: `mustParseAddr` doc says it "panics" but it doesn't — change the doc to describe actual behavior (returns the zero Address on parse failure; used only for known-good strings).
- `kiss/rtscts_other.go`: reword the "a hard error here lets the caller log a clear warning" comment (the caller treats it non-fatally) to something like "returns an error the caller logs as a non-fatal warning."
- Grep for any remaining `Sends SABM P=1` docstrings (`grep -rn "Sends SABM P=1" ax25/`) and fix to mention SABM/SABME per port version if found.

- [ ] **Step 2: Remove confirmed-dead code**

For each candidate below, verify it is unused (`grep -rn "<name>" --include=*.go .`) — if the only hits are its declaration (and it's a package-level symbol, method, field, or test var), remove it; if it's referenced elsewhere, leave it. After each removal, `CGO_ENABLED=0 go build ./...` must still pass (a used symbol removed would fail to compile).

Candidates (from the review ledger — several may already have been cleaned or may now be in use; verify each):
- `dupFD` helper (kiss/bluetooth) — "unused helper left behind."
- unused `serialPort` field; a `Timer.fired` field (internal/engine) if unused.
- a dead `shouldStop` read at the top of an engine `Run` loop; a stale comment on an unused `quit` channel.
- `rawWithVia` test var (bridge test) if unused.
- `getBool` / `buildAGWPE` — **likely now used** (getBool: rtscts/srej/send_kiss_exit; buildAGWPE: frontend). Confirm used and LEAVE.

- [ ] **Step 3: Run tests + vet**

Run: `CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go vet ./...`
Expected: all PASS, vet clean.

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "chore: remove confirmed-dead code; fix stale comments"
```

---

## Final verification

- [ ] `CGO_ENABLED=0 go test ./...` — all pass.
- [ ] `CGO_ENABLED=0 go vet ./...` — clean.
- [ ] A: EBUSY open → friendly message; other errors unchanged.
- [ ] B: probe retries; init sent on silence; honest WARNING; the three original `TestEnterKISS*` still pass (no regression for already-KISS / init-less TNCs).
- [ ] C: SSID mask + new tests; stale comments fixed; only confirmed-dead code removed.
- [ ] Merge `feature/serial-robustness` to `main` with `--no-ff` after review. No version bump.
