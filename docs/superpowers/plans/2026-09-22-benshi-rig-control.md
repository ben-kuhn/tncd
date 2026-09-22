# Benshi Rig Control Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give tncd an optional hamlib-compatible Net rigctl server that QSYs Benshi-protocol radios (BTech UV-Pro and relatives) over the Bluetooth connection tncd already holds for KISS.

**Architecture:** A pure `benshi/` codec (GaiaFrame framing + messages) sits under an `internal/rig/` request/response layer, which talks over a byte-duplex control channel exposed by the existing Bluetooth transports — a second GATT service on BLE, a second RFCOMM channel on classic. `internal/frontend/rigctl/` serves hamlib Net rigctl on TCP, one listener per port. QSY uses the radio's frequency (VFO) mode, so no memory channel is ever written and nothing is persisted to NVRAM.

**Tech Stack:** Go 1.24+, pure Go (`CGO_ENABLED=0`), BlueZ D-Bus (`github.com/godbus/dbus/v5`) on Linux, Winsock `AF_BTH` on Windows, `gopkg.in/ini.v1` for config.

**Spec:** `docs/superpowers/specs/2026-09-22-benshi-rig-control-design.md`

## Global Constraints

- Module path is `github.com/ben-kuhn/tncd/v2`. Exported reusable packages live at the top level; policy and glue under `internal/`.
- **Always** build and test with `CGO_ENABLED=0`. There is no C compiler in the default shell and every release target is pure Go.
- `go` is **not on `PATH`**. Run every Go command as: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./...'`
- `CGO_ENABLED=0 go test ./...` must pass before every commit.
- Every parser of untrusted bytes needs a `Fuzz*` target. Radio-sourced bytes are untrusted.
- Nothing may block the engine goroutine (`internal/engine`). It owns all L2 state; blocking it stalls AX.25 on every port.
- **No v1 code path may write a memory channel record or persist to NVRAM.** `WRITE_RF_CH`, `WRITE_SETTINGS` and `STORE_SETTINGS` must not appear in the implementation.
- tncd is GPL-3.0. Protocol structures are derived from benlink (Apache-2.0) and HTCommander. Each new file that encodes protocol knowledge carries an attribution comment naming the source.
- Commit messages end with the two attribution lines used by the rest of this branch.

---

### Task 0: Bench spike — does packet survive VFO mode? (NO CODE)

This is a **gate**, not an implementation task. The entire design rests on the
radio being usable in frequency (VFO) mode while its internal TNC passes
packet. If it is not, stop and return to the spec.

It needs no code because the radio can be put into frequency mode from its own
front panel.

**Files:**
- Modify: `docs/superpowers/specs/2026-09-22-benshi-rig-control-design.md` (record the result)

- [ ] **Step 1: Baseline packet on a stored channel**

With the UV-PRO on a normal stored memory channel, run tncd against it as usual
and confirm a KISS frame reaches the air (watch with an independent Dire Wolf
monitor — tncd's `tx` counter means "handed to the transport", not
"transmitted").

- [ ] **Step 2: Switch the radio to frequency (VFO) mode by hand**

Use the radio's own UI to leave channel mode and enter frequency/VFO mode. Tune
it to the same frequency used in Step 1.

- [ ] **Step 3: Repeat the packet test, unchanged**

Run exactly the same tncd configuration. Confirm on the monitor that frames
still reach the air, and that received frames still arrive.

- [ ] **Step 4: Record the result in the spec**

Add a short "Spike results" section to the spec under "Risks and spikes" with
the date and the outcome.

**GATE:** If packet does **not** work in VFO mode, stop. The "never write a
channel" property is unachievable via VFO mode and the spec needs revisiting
(the fallback is the scratch-channel scheme discussed during design). Do not
start Task 1.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/2026-09-22-benshi-rig-control-design.md
git commit -m "docs(spec): record VFO-mode packet spike result"
```

---

### Task 1: GaiaFrame framing

The outermost wire framing, shared by BLE and classic RFCOMM.

Wire format: `FF 01 <flags:u8> <n:u8> <data: n+4 bytes> [checksum:u8 if flags&1]`.
`n` deliberately excludes the 4-byte message header, so the data field is `n+4` long.

**Files:**
- Create: `benshi/frame.go`
- Create: `benshi/frame_test.go`
- Create: `benshi/fuzz_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `benshi.Frame{Flags Flags, Data []byte}`, `Frame.Bytes() []byte`, `benshi.NewDecoder() *Decoder`, `(*Decoder).Feed(p []byte) ([]Frame, error)`, constants `FlagNone`, `FlagChecksum`

- [ ] **Step 1: Write the failing test**

```go
package benshi

import (
	"bytes"
	"testing"
)

// Golden bytes: a minimal message body of 4 header bytes and no payload.
func TestFrameBytesGolden(t *testing.T) {
	f := Frame{Flags: FlagNone, Data: []byte{0x00, 0x02, 0x00, 0x04}}
	got := f.Bytes()
	want := []byte{0xFF, 0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x04}
	if !bytes.Equal(got, want) {
		t.Errorf("Bytes() = % X, want % X", got, want)
	}
}

// n_bytes_payload counts only what follows the 4-byte message header.
func TestFrameBytesCountsPayloadExcludingHeader(t *testing.T) {
	f := Frame{Flags: FlagNone, Data: []byte{0x00, 0x02, 0x00, 0x23, 0xAA, 0xBB}}
	got := f.Bytes()
	if got[3] != 2 {
		t.Errorf("n_bytes_payload = %d, want 2 (6 data bytes - 4 header)", got[3])
	}
}

func TestDecoderRoundTrip(t *testing.T) {
	f := Frame{Flags: FlagNone, Data: []byte{0x00, 0x02, 0x00, 0x24, 0x01, 0x02, 0x03}}
	d := NewDecoder()
	frames, err := d.Feed(f.Bytes())
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !bytes.Equal(frames[0].Data, f.Data) {
		t.Errorf("Data = % X, want % X", frames[0].Data, f.Data)
	}
}

// BLE delivers KISS-style partial writes; the decoder must reassemble.
func TestDecoderReassemblesSplitFrame(t *testing.T) {
	f := Frame{Flags: FlagNone, Data: []byte{0x00, 0x02, 0x00, 0x24, 0x09, 0x08}}
	raw := f.Bytes()
	d := NewDecoder()
	for i := 0; i < len(raw)-1; i++ {
		frames, err := d.Feed(raw[i : i+1])
		if err != nil {
			t.Fatalf("Feed byte %d: %v", i, err)
		}
		if len(frames) != 0 {
			t.Fatalf("frame emitted early at byte %d", i)
		}
	}
	frames, err := d.Feed(raw[len(raw)-1:])
	if err != nil || len(frames) != 1 {
		t.Fatalf("final Feed: %d frames, err %v", len(frames), err)
	}
}

// A checksum-flagged frame carries one extra trailing byte.
func TestDecoderChecksumFrame(t *testing.T) {
	raw := []byte{0xFF, 0x01, 0x01, 0x00, 0x00, 0x02, 0x00, 0x04, 0x7F}
	d := NewDecoder()
	frames, err := d.Feed(raw)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
}

// Garbage before a valid frame must be skipped, not fatal: a relinked radio can
// hand us the tail of a partial frame.
func TestDecoderResyncsAfterGarbage(t *testing.T) {
	f := Frame{Flags: FlagNone, Data: []byte{0x00, 0x02, 0x00, 0x04}}
	d := NewDecoder()
	frames, err := d.Feed(append([]byte{0x12, 0x34, 0xFF, 0x99}, f.Bytes()...))
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./benshi/ -run TestFrame -v'`
Expected: FAIL — `undefined: Frame`

- [ ] **Step 3: Write the implementation**

```go
// Package benshi implements the Benshi radio control protocol used by the
// BTech UV-Pro, RadioOddity GA-5WB and Vero VR-N76/VR-N7500.
//
// Wire details were derived from two open reimplementations:
//   - benlink (Apache-2.0) — https://github.com/khusmann/benlink
//   - HTCommander — https://github.com/Ylianst/HTCommander
package benshi

import "errors"

// Frame flags. Only CHECKSUM is known to be used.
type Flags uint8

const (
	FlagNone     Flags = 0
	FlagChecksum Flags = 1
)

const (
	frameStart   = 0xFF
	frameVersion = 0x01
	// frameHeaderLen is start + version + flags + length.
	frameHeaderLen = 4
	// msgHeaderLen is the message header (group u16 + reply/command u16) that
	// the frame's length byte does NOT count.
	msgHeaderLen = 4
	// maxFrameData bounds a frame body: the length byte is 8 bits, so the data
	// field can never exceed 255 + msgHeaderLen.
	maxFrameData = 255 + msgHeaderLen
)

// ErrShortFrame reports a frame whose declared length is impossible.
var ErrShortFrame = errors.New("benshi: frame data shorter than message header")

// Frame is one GaiaFrame: FF 01 <flags> <n> <data> [checksum].
//
// Data holds the complete message (its 4-byte header plus body). The wire
// length byte counts only what follows that header, which is why Bytes
// subtracts msgHeaderLen.
type Frame struct {
	Flags Flags
	Data  []byte
}

// Bytes serializes the frame. A frame whose Data is shorter than a message
// header cannot be represented and returns nil.
func (f Frame) Bytes() []byte {
	if len(f.Data) < msgHeaderLen || len(f.Data) > maxFrameData {
		return nil
	}
	out := make([]byte, 0, frameHeaderLen+len(f.Data))
	out = append(out, frameStart, frameVersion, byte(f.Flags), byte(len(f.Data)-msgHeaderLen))
	return append(out, f.Data...)
}

// Decoder reassembles frames from a byte stream. Both transports deliver
// partial frames -- BLE splits on ATT MTU, RFCOMM on socket reads -- so the
// decoder buffers until a whole frame is present.
type Decoder struct {
	buf []byte
}

// NewDecoder returns a decoder with an empty buffer.
func NewDecoder() *Decoder { return &Decoder{} }

// Feed appends p and returns every complete frame now available.
//
// Bytes that cannot begin a valid frame are skipped rather than returned as an
// error: a relink or a mid-frame disconnect can leave us holding a fragment,
// and resynchronizing on the next start byte recovers without dropping the
// link.
func (d *Decoder) Feed(p []byte) ([]Frame, error) {
	d.buf = append(d.buf, p...)
	var out []Frame
	for {
		// Resync: discard anything before a plausible start.
		for len(d.buf) > 0 && d.buf[0] != frameStart {
			d.buf = d.buf[1:]
		}
		if len(d.buf) < frameHeaderLen {
			return out, nil
		}
		if d.buf[1] != frameVersion {
			d.buf = d.buf[1:] // false start byte; resync past it
			continue
		}
		flags := Flags(d.buf[2])
		dataLen := int(d.buf[3]) + msgHeaderLen
		total := frameHeaderLen + dataLen
		if flags&FlagChecksum != 0 {
			total++
		}
		if len(d.buf) < total {
			return out, nil // incomplete; wait for more
		}
		data := make([]byte, dataLen)
		copy(data, d.buf[frameHeaderLen:frameHeaderLen+dataLen])
		out = append(out, Frame{Flags: flags, Data: data})
		d.buf = d.buf[total:]
	}
}
```

- [ ] **Step 4: Write the fuzz target**

```go
package benshi

import "testing"

func FuzzDecoder(f *testing.F) {
	f.Add([]byte{0xFF, 0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x04})
	f.Add([]byte{0xFF, 0x01, 0x01, 0x02, 0x00, 0x02, 0x00, 0x23, 0xAA, 0xBB, 0x7F})
	f.Add([]byte{0xFF, 0x01, 0xFF, 0xFF})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := NewDecoder()
		// Must never panic and must never grow without bound.
		if _, err := d.Feed(data); err != nil {
			return
		}
		if len(d.buf) > len(data)+maxFrameData {
			t.Fatalf("decoder buffer grew to %d for %d input bytes", len(d.buf), len(data))
		}
	})
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./benshi/ -v'`
Expected: PASS

- [ ] **Step 6: Run a short fuzz burst**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./benshi/ -fuzz=FuzzDecoder -fuzztime=30s'`
Expected: no crashers

- [ ] **Step 7: Commit**

```bash
git add benshi/
git commit -m "feat(benshi): GaiaFrame framing with streaming decoder"
```

---

### Task 2: Message header and command identifiers

The `Data` field of a frame is a message: a 16-bit command group, a 1-bit reply
flag packed with a 15-bit command id, then the body.

**Files:**
- Create: `benshi/message.go`
- Create: `benshi/message_test.go`
- Modify: `benshi/fuzz_test.go`

**Interfaces:**
- Consumes: `Frame` from Task 1
- Produces: `benshi.Message{Group, IsReply, Command, Body}`, `Message.Bytes() []byte`, `benshi.DecodeMessage([]byte) (Message, error)`, constants `GroupBasic`, `GroupExtended`, `CmdGetDevInfo`, `CmdReadSettings`, `CmdReadRFCh`, `CmdGetHTStatus`, `CmdEventNotification`, `CmdFreqModeSetPar`, `CmdFreqModeGetStatus`, `CmdDoProgFunc`

- [ ] **Step 1: Write the failing test**

```go
package benshi

import (
	"bytes"
	"testing"
)

func TestMessageBytesGolden(t *testing.T) {
	m := Message{Group: GroupBasic, IsReply: false, Command: CmdFreqModeGetStatus}
	got := m.Bytes()
	// group 2 big-endian, then reply bit clear | command 36.
	want := []byte{0x00, 0x02, 0x00, 0x24}
	if !bytes.Equal(got, want) {
		t.Errorf("Bytes() = % X, want % X", got, want)
	}
}

func TestMessageReplyBitIsTopBit(t *testing.T) {
	m := Message{Group: GroupBasic, IsReply: true, Command: CmdFreqModeGetStatus}
	got := m.Bytes()
	want := []byte{0x00, 0x02, 0x80, 0x24}
	if !bytes.Equal(got, want) {
		t.Errorf("Bytes() = % X, want % X", got, want)
	}
}

func TestDecodeMessageRoundTrip(t *testing.T) {
	in := Message{Group: GroupBasic, IsReply: true, Command: CmdGetHTStatus, Body: []byte{0x00, 0x11}}
	got, err := DecodeMessage(in.Bytes())
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if got.Group != in.Group || got.IsReply != in.IsReply || got.Command != in.Command {
		t.Errorf("header = %+v, want %+v", got, in)
	}
	if !bytes.Equal(got.Body, in.Body) {
		t.Errorf("Body = % X, want % X", got.Body, in.Body)
	}
}

func TestDecodeMessageRejectsShort(t *testing.T) {
	for _, b := range [][]byte{nil, {0x00}, {0x00, 0x02, 0x00}} {
		if _, err := DecodeMessage(b); err == nil {
			t.Errorf("DecodeMessage(% X) = nil error, want error", b)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./benshi/ -run TestMessage -v'`
Expected: FAIL — `undefined: Message`

- [ ] **Step 3: Write the implementation**

```go
package benshi

import (
	"encoding/binary"
	"errors"
)

// CommandGroup selects which command namespace an id belongs to.
type CommandGroup uint16

const (
	GroupBasic    CommandGroup = 2
	GroupExtended CommandGroup = 10
)

// Command is a 15-bit command id within a group.
type Command uint16

// The v1 subset. The full protocol has roughly fifty basic commands; only
// those needed for read-only status and VFO-mode QSY are defined here, so that
// no channel-writing or NVRAM-persisting command is reachable from this code.
const (
	CmdGetDevInfo        Command = 4
	CmdEventNotification Command = 9
	CmdReadSettings      Command = 10
	CmdReadRFCh          Command = 13
	CmdGetHTStatus       Command = 20
	CmdFreqModeSetPar    Command = 35
	CmdFreqModeGetStatus Command = 36
	CmdDoProgFunc        Command = 66
)

// ErrShortMessage reports a message too short to hold a header.
var ErrShortMessage = errors.New("benshi: message shorter than 4-byte header")

// replyBit is the top bit of the third/fourth header byte pair.
const replyBit = 0x8000

// Message is one protocol message: a 4-byte header and an opaque body.
//
// Bodies are left as bytes here; each command's body layout is decoded by the
// command-specific helpers, which keeps this type free of per-command
// knowledge.
type Message struct {
	Group   CommandGroup
	IsReply bool
	Command Command
	Body    []byte
}

// Bytes serializes the message: group big-endian, then the reply bit packed
// into the top bit of the 16-bit command word.
func (m Message) Bytes() []byte {
	out := make([]byte, msgHeaderLen, msgHeaderLen+len(m.Body))
	binary.BigEndian.PutUint16(out[0:2], uint16(m.Group))
	cmd := uint16(m.Command) &^ replyBit
	if m.IsReply {
		cmd |= replyBit
	}
	binary.BigEndian.PutUint16(out[2:4], cmd)
	return append(out, m.Body...)
}

// DecodeMessage parses a message from the Data field of a Frame.
func DecodeMessage(b []byte) (Message, error) {
	if len(b) < msgHeaderLen {
		return Message{}, ErrShortMessage
	}
	cmd := binary.BigEndian.Uint16(b[2:4])
	m := Message{
		Group:   CommandGroup(binary.BigEndian.Uint16(b[0:2])),
		IsReply: cmd&replyBit != 0,
		Command: Command(cmd &^ replyBit),
	}
	if len(b) > msgHeaderLen {
		m.Body = make([]byte, len(b)-msgHeaderLen)
		copy(m.Body, b[msgHeaderLen:])
	}
	return m, nil
}
```

- [ ] **Step 4: Extend the fuzz target**

Append to `benshi/fuzz_test.go`:

```go
func FuzzDecodeMessage(f *testing.F) {
	f.Add([]byte{0x00, 0x02, 0x00, 0x24})
	f.Add([]byte{0x00, 0x02, 0x80, 0x24, 0x00, 0x09, 0xB0, 0x50, 0xF0})
	f.Add([]byte{0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := DecodeMessage(data)
		if err != nil {
			return
		}
		// A decoded message must re-serialize to the same bytes.
		if got := m.Bytes(); len(got) != len(data) {
			t.Fatalf("round-trip length %d != input %d", len(got), len(data))
		}
	})
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./benshi/ -v'`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add benshi/
git commit -m "feat(benshi): message header codec and v1 command ids"
```

---

### Task 3: Frequency (VFO) mode payloads

The QSY mechanism. `FREQ_MODE_SET_PAR` enters frequency mode and tunes;
an all-zero payload leaves it. `FREQ_MODE_GET_STATUS` and event notification 14
read it back.

**Files:**
- Create: `benshi/freqmode.go`
- Create: `benshi/freqmode_test.go`

**Interfaces:**
- Consumes: `Message` from Task 2
- Produces: `benshi.FreqModeParams`, `FreqModeParams.Payload() []byte`, `benshi.TeardownPayload() []byte`, `benshi.DecodeFreqModeStatus(body []byte) (uint32, error)`, `benshi.FreqModeStatus{Active bool, RXFreqHz, TXFreqHz uint32}`, `benshi.DecodeFreqModeNotification(body []byte) (FreqModeStatus, error)`, constants `ModFM`, `DefaultStep`

- [ ] **Step 1: Write the failing test**

```go
package benshi

import (
	"bytes"
	"testing"
)

func TestFreqModeParamsPayload(t *testing.T) {
	p := FreqModeParams{RXFreqHz: 145030000, TXFreqHz: 145030000, Step: DefaultStep}
	got := p.Payload()
	if len(got) != 16 {
		t.Fatalf("payload length = %d, want 16", len(got))
	}
	// 145030000 = 0x08A4A6B0; FM modulation is 0 so the top 2 bits stay clear.
	want := []byte{
		0x08, 0xA4, 0xA6, 0xB0,
		0x08, 0xA4, 0xA6, 0xB0,
		0x00, 0x00,
		0x00, 0x00,
		0x00, 0x00,
		0x61, 0xA8,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Payload() = % X, want % X", got, want)
	}
}

// Modulation rides in the top 2 bits of each frequency word.
func TestFreqModeParamsModulationBits(t *testing.T) {
	p := FreqModeParams{RXFreqHz: 1, RXMod: 2, TXFreqHz: 1, TXMod: 1, Step: DefaultStep}
	got := p.Payload()
	if got[0]>>6 != 2 {
		t.Errorf("RX modulation bits = %d, want 2", got[0]>>6)
	}
	if got[4]>>6 != 1 {
		t.Errorf("TX modulation bits = %d, want 1", got[4]>>6)
	}
}

// The documented teardown is an all-zero payload, step included.
func TestTeardownPayloadIsAllZero(t *testing.T) {
	got := TeardownPayload()
	if len(got) != 16 {
		t.Fatalf("length = %d, want 16", len(got))
	}
	for i, b := range got {
		if b != 0 {
			t.Errorf("byte %d = %#x, want 0", i, b)
		}
	}
}

// Body[0] is reply status, Body[1:5] the frequency with modulation in the top
// 2 bits. HTCommander's worked example: 09 B0 50 F0 -> 162.550 MHz.
func TestDecodeFreqModeStatus(t *testing.T) {
	body := []byte{0x00, 0x09, 0xB0, 0x50, 0xF0}
	got, err := DecodeFreqModeStatus(body)
	if err != nil {
		t.Fatalf("DecodeFreqModeStatus: %v", err)
	}
	if got != 162550000 {
		t.Errorf("freq = %d, want 162550000", got)
	}
}

func TestDecodeFreqModeStatusRejectsFailureStatus(t *testing.T) {
	if _, err := DecodeFreqModeStatus([]byte{0x01, 0x09, 0xB0, 0x50, 0xF0}); err == nil {
		t.Error("non-zero reply status must be an error")
	}
}

func TestDecodeFreqModeStatusRejectsShort(t *testing.T) {
	if _, err := DecodeFreqModeStatus([]byte{0x00, 0x09}); err == nil {
		t.Error("short body must be an error")
	}
}

// Notification 14: Body[0] type, [1:5] RX, [5:9] TX, [9:13] sub-audio,
// [13:15] flags. Only the LOW flags byte is authoritative for active state.
func TestDecodeFreqModeNotificationActive(t *testing.T) {
	body := []byte{
		14,
		0x08, 0xA4, 0xA6, 0xB0,
		0x08, 0xA4, 0xA6, 0xB0,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x40,
	}
	got, err := DecodeFreqModeNotification(body)
	if err != nil {
		t.Fatalf("DecodeFreqModeNotification: %v", err)
	}
	if !got.Active {
		t.Error("Active = false, want true (low flags byte 0x40)")
	}
	if got.RXFreqHz != 145030000 {
		t.Errorf("RXFreqHz = %d, want 145030000", got.RXFreqHz)
	}
}

// The HIGH flags byte can stay set after leaving frequency mode, so it must not
// be consulted.
func TestDecodeFreqModeNotificationHighByteIsNotAuthoritative(t *testing.T) {
	body := []byte{
		14,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0x80, 0x00,
	}
	got, err := DecodeFreqModeNotification(body)
	if err != nil {
		t.Fatalf("DecodeFreqModeNotification: %v", err)
	}
	if got.Active {
		t.Error("Active = true, want false (low flags byte is 0)")
	}
}

func TestDecodeFreqModeNotificationRejectsWrongType(t *testing.T) {
	body := make([]byte, 15)
	body[0] = 3 // some other notification
	if _, err := DecodeFreqModeNotification(body); err == nil {
		t.Error("non-14 notification type must be an error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./benshi/ -run TestFreqMode -v'`
Expected: FAIL — `undefined: FreqModeParams`

- [ ] **Step 3: Write the implementation**

```go
package benshi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Modulation occupies the top 2 bits of each frequency word.
type Modulation uint8

const (
	ModFM Modulation = 0
	ModAM Modulation = 1
)

// DefaultStep is the constant channel step the radio expects on every
// FREQ_MODE_SET_PAR update (0x61A8 = 25000).
const DefaultStep uint16 = 0x61A8

// freqModePayloadLen is the fixed FREQ_MODE_SET_PAR payload size.
const freqModePayloadLen = 16

// freqMask is the 30-bit frequency field; the top 2 bits are modulation.
const freqMask = 0x3FFFFFFF

// notifyFreqMode is the event-notification type for a frequency-mode change.
const notifyFreqMode = 14

var (
	// ErrShortBody reports a reply body too short for its command.
	ErrShortBody = errors.New("benshi: reply body too short")
	// ErrWrongNotification reports a notification of an unexpected type.
	ErrWrongNotification = errors.New("benshi: not a frequency-mode notification")
)

// FreqModeParams is the FREQ_MODE_SET_PAR payload: it puts the radio into
// frequency (VFO) mode and tunes explicit frequencies.
//
// This is the whole reason tncd never writes a memory channel. The alternative
// -- WRITE_RF_CH -- mutates a stored channel record, because Settings.channel_a
// is an index into the channel table rather than a scratch register. HTCommander
// drives this command about once a second for satellite Doppler tracking, so it
// is by construction not touching NVRAM.
type FreqModeParams struct {
	RXFreqHz   uint32
	TXFreqHz   uint32
	RXMod      Modulation
	TXMod      Modulation
	RXSubAudio uint16 // units of 0.01 Hz; 0 = none
	TXSubAudio uint16
	Flags      uint16 // settles to 0 once in frequency mode
	Step       uint16 // send DefaultStep unchanged on every update
}

// Payload serializes the 16-byte FREQ_MODE_SET_PAR body.
func (p FreqModeParams) Payload() []byte {
	out := make([]byte, freqModePayloadLen)
	binary.BigEndian.PutUint32(out[0:4], p.RXFreqHz&freqMask)
	out[0] = (out[0] & 0x3F) | byte(p.RXMod&0x03)<<6
	binary.BigEndian.PutUint32(out[4:8], p.TXFreqHz&freqMask)
	out[4] = (out[4] & 0x3F) | byte(p.TXMod&0x03)<<6
	binary.BigEndian.PutUint16(out[8:10], p.RXSubAudio)
	binary.BigEndian.PutUint16(out[10:12], p.TXSubAudio)
	binary.BigEndian.PutUint16(out[12:14], p.Flags)
	binary.BigEndian.PutUint16(out[14:16], p.Step)
	return out
}

// TeardownPayload is the documented all-zero FREQ_MODE_SET_PAR body: it drops
// the radio out of frequency mode and restores its normal channel state.
func TeardownPayload() []byte { return make([]byte, freqModePayloadLen) }

// DecodeFreqModeStatus parses a FREQ_MODE_GET_STATUS reply body:
// Body[0] is the reply status (0 = success), Body[1:5] the frequency with
// modulation in the top 2 bits.
func DecodeFreqModeStatus(body []byte) (uint32, error) {
	if len(body) < 5 {
		return 0, ErrShortBody
	}
	if body[0] != 0 {
		return 0, fmt.Errorf("benshi: freq mode status reply status %d", body[0])
	}
	return binary.BigEndian.Uint32(body[1:5]) & freqMask, nil
}

// FreqModeStatus is the decoded live state pushed by event notification 14.
type FreqModeStatus struct {
	Active   bool
	RXFreqHz uint32
	TXFreqHz uint32
}

// DecodeFreqModeNotification parses event notification 14.
//
// Only the LOW flags byte is authoritative for Active: the high byte can remain
// set after the radio leaves frequency mode, so consulting it reports frequency
// mode long after it has ended.
func DecodeFreqModeNotification(body []byte) (FreqModeStatus, error) {
	if len(body) < 15 {
		return FreqModeStatus{}, ErrShortBody
	}
	if body[0] != notifyFreqMode {
		return FreqModeStatus{}, ErrWrongNotification
	}
	return FreqModeStatus{
		Active:   body[14] != 0,
		RXFreqHz: binary.BigEndian.Uint32(body[1:5]) & freqMask,
		TXFreqHz: binary.BigEndian.Uint32(body[5:9]) & freqMask,
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./benshi/ -v'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add benshi/
git commit -m "feat(benshi): frequency (VFO) mode payloads and status decoding"
```

---

### Task 4: Control channel on the BLE transport

Expose the Benshi GATT service as a byte-duplex, reusing the LE connection the
KISS transport already holds.

**Files:**
- Create: `kiss/control.go`
- Modify: `kiss/ble_linux.go` (add Benshi UUIDs beside the KISS ones at line 17-26; add `findBenshiChars`; add `ControlChannel`)
- Modify: `kiss/ble_stub.go` (no-op `ControlChannel` for non-Linux)
- Create: `kiss/control_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks
- Produces: `kiss.ControlCapable` interface with `ControlChannel() (io.ReadWriteCloser, error)`, `kiss.ErrNoControlChannel`

- [ ] **Step 1: Write the failing test**

```go
package kiss

import (
	"errors"
	"testing"
)

// A transport with no control channel must be detectable without a type switch
// at every call site.
func TestControlChannelForReportsUnsupported(t *testing.T) {
	var tr Transport = &tcpTransport{}
	if _, err := ControlChannelFor(tr); !errors.Is(err, ErrNoControlChannel) {
		t.Errorf("err = %v, want ErrNoControlChannel", err)
	}
}

type fakeControlTransport struct {
	tcpTransport
	ch *fakeRWC
}

func (f *fakeControlTransport) ControlChannel() (ioReadWriteCloser, error) { return f.ch, nil }

func TestControlChannelForReturnsChannel(t *testing.T) {
	f := &fakeControlTransport{ch: &fakeRWC{}}
	got, err := ControlChannelFor(f)
	if err != nil {
		t.Fatalf("ControlChannelFor: %v", err)
	}
	if got != f.ch {
		t.Error("returned channel is not the transport's channel")
	}
}
```

Note: `ioReadWriteCloser` above is shorthand for `io.ReadWriteCloser`; import
`io` and use the real name. `fakeRWC` is a struct with no-op `Read`, `Write`
and `Close` methods returning `(0, io.EOF)`, `(len(p), nil)` and `nil`.

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./kiss/ -run TestControlChannel -v'`
Expected: FAIL — `undefined: ControlChannelFor`

- [ ] **Step 3: Write the capability shim**

```go
package kiss

import (
	"errors"
	"io"
)

// ErrNoControlChannel reports a transport that cannot carry rig control.
var ErrNoControlChannel = errors.New("kiss: transport has no rig control channel")

// ControlCapable is implemented by transports that can carry a rig-control
// channel alongside KISS data.
//
// The channel is byte-oriented on purpose: this package knows Bluetooth, the
// benshi package knows the protocol, and neither needs the other's details.
type ControlCapable interface {
	ControlChannel() (io.ReadWriteCloser, error)
}

// ControlChannelFor returns tr's rig-control channel, or ErrNoControlChannel if
// the transport does not support one.
func ControlChannelFor(tr Transport) (io.ReadWriteCloser, error) {
	cc, ok := tr.(ControlCapable)
	if !ok {
		return nil, ErrNoControlChannel
	}
	return cc.ControlChannel()
}
```

- [ ] **Step 4: Add the Benshi UUIDs and characteristic lookup to the BLE transport**

In `kiss/ble_linux.go`, beside the existing KISS UUID block:

```go
// Benshi rig-control service and characteristic UUIDs, lowercase to match
// BlueZ. Distinct from the KISS service above: the radio advertises both, and
// GATT multiplexes them over the single LE connection this transport already
// holds, so rig control costs no extra connection.
const (
	benshiService    = "00001100-d102-11e1-9b23-00025b00a5a5"
	benshiWriteChar  = "00001101-d102-11e1-9b23-00025b00a5a5"
	benshiNotifyChar = "00001102-d102-11e1-9b23-00025b00a5a5"
)
```

```go
// findBenshiChars locates the Benshi control characteristics on an
// already-connected device. Same walk as findKISSChars, different UUIDs: both
// services live on the one LE connection.
func findBenshiChars(conn *dbus.Conn, devPath dbus.ObjectPath) (write, notify dbus.ObjectPath) {
	var objects map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	obj := conn.Object("org.bluez", "/")
	ctx, cancel := context.WithTimeout(context.Background(), btCallTimeout)
	defer cancel()
	if err := obj.CallWithContext(ctx,
		"org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects); err != nil {
		return "", ""
	}
	prefix := string(devPath) + "/"
	for path, ifaces := range objects {
		if !strings.HasPrefix(string(path), prefix) {
			continue
		}
		props, ok := ifaces["org.bluez.GattCharacteristic1"]
		if !ok {
			continue
		}
		uuid, _ := props["UUID"].Value().(string)
		switch strings.ToLower(uuid) {
		case benshiWriteChar:
			write = path
		case benshiNotifyChar:
			notify = path
		}
	}
	return write, notify
}

// bleControlChannel is the byte-duplex over the Benshi GATT characteristics.
//
// It deliberately does NOT own the LE connection: the KISS transport does. So
// Close tears down only this channel's notification watch, leaving packet data
// flowing.
type bleControlChannel struct {
	conn      *dbus.Conn
	writeChar dbus.BusObject
	mtu       int
	rx        chan []byte
	leftover  []byte
	closeOnce sync.Once
	closedCh  chan struct{}
}

func (c *bleControlChannel) Write(p []byte) (int, error) {
	for _, chunk := range chunkForMTU(p, c.mtu) {
		ctx, cancel := context.WithTimeout(context.Background(), btCallTimeout)
		err := c.writeChar.CallWithContext(ctx,
			"org.bluez.GattCharacteristic1.WriteValue", 0,
			chunk, map[string]dbus.Variant{}).Store()
		cancel()
		if err != nil {
			return 0, fmt.Errorf("kiss: benshi control write: %w", err)
		}
	}
	return len(p), nil
}

func (c *bleControlChannel) Read(p []byte) (int, error) {
	if len(c.leftover) > 0 {
		n := copy(p, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}
	select {
	case b, ok := <-c.rx:
		if !ok {
			return 0, io.EOF
		}
		n := copy(p, b)
		if n < len(b) {
			c.leftover = b[n:]
		}
		return n, nil
	case <-c.closedCh:
		return 0, io.EOF
	}
}

func (c *bleControlChannel) Close() error {
	c.closeOnce.Do(func() { close(c.closedCh) })
	return nil
}

// ControlChannel returns the Benshi control channel for this radio, reusing the
// LE connection Open already established.
func (bt *bleTransport) ControlChannel() (io.ReadWriteCloser, error) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	if !bt.open {
		return nil, fmt.Errorf("kiss: BLE transport is not open")
	}
	writePath, notifyPath := findBenshiChars(bt.conn, bt.devPath)
	if writePath == "" || notifyPath == "" {
		return nil, ErrNoControlChannel
	}
	ch := &bleControlChannel{
		conn:      bt.conn,
		writeChar: bt.conn.Object("org.bluez", writePath),
		mtu:       readCharMTU(bt.conn, writePath),
		rx:        make(chan []byte, bleRXQueue),
		closedCh:  make(chan struct{}),
	}
	if err := watchNotifications(bt.conn, notifyPath, ch.rx, ch.closedCh); err != nil {
		return nil, fmt.Errorf("kiss: benshi notify: %w", err)
	}
	return ch, nil
}
```

Note `watchNotifications` already calls `StartNotify` and forwards `Value`
updates; it is reused as-is rather than duplicated.

- [ ] **Step 5: Stub it off non-Linux**

In `kiss/ble_stub.go`, the stub transport's `ControlChannel` returns
`(nil, ErrNoControlChannel)`.

- [ ] **Step 6: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./kiss/ -v'`
Expected: PASS

- [ ] **Step 7: Verify it still cross-compiles**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./... && CGO_ENABLED=0 GOOS=freebsd GOARCH=amd64 go build ./...'`
Expected: no output

- [ ] **Step 8: Commit**

```bash
git add kiss/
git commit -m "feat(kiss): expose the Benshi control channel from the BLE transport"
```

---

### Task 5: The rig layer

Request/response over the control channel, with timeouts, a notification cache,
and teardown.

**Files:**
- Create: `internal/rig/rig.go`
- Create: `internal/rig/rig_test.go`

**Interfaces:**
- Consumes: `benshi.Frame`, `benshi.NewDecoder`, `benshi.Message`, `benshi.DecodeMessage`, `benshi.FreqModeParams`, `benshi.TeardownPayload`, `benshi.DecodeFreqModeStatus`, `benshi.DecodeFreqModeNotification` (Tasks 1-3); `io.ReadWriteCloser` from Task 4
- Produces: `rig.New(ch io.ReadWriteCloser, timeout time.Duration) *Rig`, `(*Rig).SetFreq(hz uint32) error`, `(*Rig).GetFreq() (uint32, error)`, `(*Rig).Teardown() error`, `(*Rig).Close() error`, `(*Rig).CachedFreq() (uint32, bool)`, `(*Rig).Probe() error`, `rig.ErrTimeout`, `rig.ErrClosed`

- [ ] **Step 1: Write the failing test**

```go
package rig

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// fakeChannel is a control channel whose replies are scripted by the test.
type fakeChannel struct {
	mu      sync.Mutex
	written [][]byte
	replies chan []byte
	closed  bool
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{replies: make(chan []byte, 8)}
}

func (f *fakeChannel) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]byte, len(p))
	copy(cp, p)
	f.written = append(f.written, cp)
	return len(p), nil
}

func (f *fakeChannel) Read(p []byte) (int, error) {
	b, ok := <-f.replies
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.replies)
	}
	return nil
}

func (f *fakeChannel) writes() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.written))
	copy(out, f.written)
	return out
}

// reply enqueues a reply message for the given command.
func (f *fakeChannel) reply(cmd benshi.Command, body []byte) {
	m := benshi.Message{Group: benshi.GroupBasic, IsReply: true, Command: cmd, Body: body}
	f.replies <- benshi.Frame{Flags: benshi.FlagNone, Data: m.Bytes()}.Bytes()
}

func TestSetFreqSendsFreqModeSetPar(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeSetPar, []byte{0x00})
	}()

	if err := r.SetFreq(145030000); err != nil {
		t.Fatalf("SetFreq: %v", err)
	}
	w := ch.writes()
	if len(w) != 1 {
		t.Fatalf("wrote %d frames, want 1", len(w))
	}
	m, err := benshi.DecodeMessage(w[0][4:])
	if err != nil {
		t.Fatalf("DecodeMessage: %v", err)
	}
	if m.Command != benshi.CmdFreqModeSetPar {
		t.Errorf("command = %d, want CmdFreqModeSetPar", m.Command)
	}
	if len(m.Body) != 16 {
		t.Errorf("body length = %d, want 16", len(m.Body))
	}
}

func TestGetFreqUsesStatusReply(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeGetStatus, []byte{0x00, 0x09, 0xB0, 0x50, 0xF0})
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("GetFreq: %v", err)
	}
	if hz != 162550000 {
		t.Errorf("GetFreq = %d, want 162550000", hz)
	}
}

// A radio that never answers must not wedge the caller.
func TestRequestTimesOut(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, 50*time.Millisecond)
	defer r.Close()

	start := time.Now()
	err := r.SetFreq(145030000)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want roughly the 50ms timeout", elapsed)
	}
}

// Teardown must send the documented all-zero payload.
func TestTeardownSendsAllZeroPayload(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdFreqModeSetPar, []byte{0x00})
	}()

	if err := r.Teardown(); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	w := ch.writes()
	m, _ := benshi.DecodeMessage(w[0][4:])
	for i, b := range m.Body {
		if b != 0 {
			t.Fatalf("teardown body byte %d = %#x, want 0", i, b)
		}
	}
}

// A pushed notification updates the cache, so GetFreq need not hit the radio.
func TestNotificationPopulatesCache(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	body := []byte{14, 0x08, 0xA4, 0xA6, 0xB0, 0x08, 0xA4, 0xA6, 0xB0,
		0, 0, 0, 0, 0x00, 0x40}
	ch.reply(benshi.CmdEventNotification, body)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if hz, ok := r.CachedFreq(); ok && hz == 145030000 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("notification never reached the cache")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/rig/ -v'`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: Write the implementation**

```go
// Package rig drives a Benshi-protocol radio over a control channel.
//
// Everything here runs OFF the engine goroutine. A Benshi command is a
// round-trip to the radio with a timeout, and the engine owns all L2 state, so
// blocking it would stall AX.25 on every port for the duration.
package rig

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// ErrTimeout reports a radio that did not answer within the configured timeout.
var ErrTimeout = errors.New("rig: radio did not reply in time")

// ErrClosed reports use of a rig whose channel has been closed.
var ErrClosed = errors.New("rig: control channel closed")

// Rig is a request/response session with one radio.
//
// One request is in flight at a time: the radio is a single serial endpoint and
// replies carry no correlation id, so concurrent requests could not be matched
// to their commands.
type Rig struct {
	ch      io.ReadWriteCloser
	timeout time.Duration

	reqMu sync.Mutex // serializes whole request/response exchanges

	mu        sync.Mutex
	waiting   chan benshi.Message // non-nil while a request awaits its reply
	waitCmd   benshi.Command
	cachedHz  uint32
	cachedOK  bool
	closed    bool
	closeOnce sync.Once
}

// New starts a rig on ch. The reader goroutine runs until Close.
func New(ch io.ReadWriteCloser, timeout time.Duration) *Rig {
	r := &Rig{ch: ch, timeout: timeout}
	go r.readLoop()
	return r
}

// Close shuts the rig down and closes the underlying channel.
func (r *Rig) Close() error {
	var err error
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		err = r.ch.Close()
	})
	return err
}

// CachedFreq returns the last frequency pushed by the radio, if any.
func (r *Rig) CachedFreq() (uint32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cachedHz, r.cachedOK
}

// SetFreq enters frequency (VFO) mode and tunes rx = tx = hz.
//
// v1 is simplex only, which is what Winlink RMS gateways need.
func (r *Rig) SetFreq(hz uint32) error {
	p := benshi.FreqModeParams{
		RXFreqHz: hz,
		TXFreqHz: hz,
		RXMod:    benshi.ModFM,
		TXMod:    benshi.ModFM,
		Step:     benshi.DefaultStep,
	}
	_, err := r.request(benshi.CmdFreqModeSetPar, p.Payload())
	return err
}

// GetFreq reads the current frequency from the radio.
func (r *Rig) GetFreq() (uint32, error) {
	body, err := r.request(benshi.CmdFreqModeGetStatus, nil)
	if err != nil {
		return 0, err
	}
	return benshi.DecodeFreqModeStatus(body)
}

// Teardown drops the radio out of frequency mode, restoring its channel state.
func (r *Rig) Teardown() error {
	_, err := r.request(benshi.CmdFreqModeSetPar, benshi.TeardownPayload())
	return err
}

// request sends one command and waits for its reply.
func (r *Rig) request(cmd benshi.Command, body []byte) ([]byte, error) {
	r.reqMu.Lock()
	defer r.reqMu.Unlock()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrClosed
	}
	replyCh := make(chan benshi.Message, 1)
	r.waiting = replyCh
	r.waitCmd = cmd
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.waiting = nil
		r.mu.Unlock()
	}()

	msg := benshi.Message{Group: benshi.GroupBasic, Command: cmd, Body: body}
	frame := benshi.Frame{Flags: benshi.FlagNone, Data: msg.Bytes()}
	raw := frame.Bytes()
	if raw == nil {
		return nil, fmt.Errorf("rig: command %d produced an unencodable frame", cmd)
	}
	if _, err := r.ch.Write(raw); err != nil {
		return nil, fmt.Errorf("rig: write: %w", err)
	}

	select {
	case m := <-replyCh:
		return m.Body, nil
	case <-time.After(r.timeout):
		return nil, ErrTimeout
	}
}

// readLoop decodes frames from the radio, routing replies to a waiting request
// and folding notifications into the cache.
func (r *Rig) readLoop() {
	dec := benshi.NewDecoder()
	buf := make([]byte, 512)
	for {
		n, err := r.ch.Read(buf)
		if n > 0 {
			frames, derr := dec.Feed(buf[:n])
			if derr == nil {
				for _, f := range frames {
					r.dispatch(f)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (r *Rig) dispatch(f benshi.Frame) {
	m, err := benshi.DecodeMessage(f.Data)
	if err != nil {
		return
	}
	if m.Command == benshi.CmdEventNotification {
		if st, err := benshi.DecodeFreqModeNotification(m.Body); err == nil {
			r.mu.Lock()
			if st.Active {
				r.cachedHz, r.cachedOK = st.RXFreqHz, true
			} else {
				r.cachedOK = false
			}
			r.mu.Unlock()
		}
		return
	}
	r.mu.Lock()
	w, cmd := r.waiting, r.waitCmd
	r.mu.Unlock()
	if w != nil && m.Command == cmd {
		select {
		case w <- m:
		default:
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/rig/ -race -v'`
Expected: PASS, no race warnings

- [ ] **Step 5: Commit**

```bash
git add internal/rig/
git commit -m "feat(rig): Benshi request/response layer with VFO-mode QSY"
```

- [ ] **Step 6: Write the failing test for channel-mode readback and probe**

`\get_freq` must still answer when the radio is on a stored channel rather than
in frequency mode. The spec reaches the active channel via `READ_SETTINGS`;
`GET_HT_STATUS` carries the same channel id in one field instead of
`Settings`' split nibbles, so use it and read the channel with `READ_RF_CH`.

```go
func TestGetFreqFallsBackToChannelRead(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// FREQ_MODE_GET_STATUS reports failure: not in frequency mode.
		ch.reply(benshi.CmdFreqModeGetStatus, []byte{0x01})
		time.Sleep(10 * time.Millisecond)
		// GET_HT_STATUS: reply_status 0, then Status. curr_ch_id_lower is the
		// high nibble of the second status byte; channel 3 here.
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0x80, 0x30})
		time.Sleep(10 * time.Millisecond)
		// READ_RF_CH: status, channel_id, tx word, rx word (145.030 MHz).
		ch.reply(benshi.CmdReadRFCh, []byte{
			0x00, 0x03,
			0x08, 0xA4, 0xA6, 0xB0,
			0x08, 0xA4, 0xA6, 0xB0,
		})
	}()

	hz, err := r.GetFreq()
	if err != nil {
		t.Fatalf("GetFreq: %v", err)
	}
	if hz != 145030000 {
		t.Errorf("GetFreq = %d, want 145030000 via the channel-mode fallback", hz)
	}
}

func TestGetPTTReadsTXBit(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		// is_in_tx is bit 1 from the MSB of the first status byte.
		ch.reply(benshi.CmdGetHTStatus, []byte{0x00, 0xC0, 0x00})
	}()

	on, err := r.GetPTT()
	if err != nil {
		t.Fatalf("GetPTT: %v", err)
	}
	if !on {
		t.Error("GetPTT = false, want true when is_in_tx is set")
	}
}

func TestProbeRejectsFailureStatus(t *testing.T) {
	ch := newFakeChannel()
	r := New(ch, time.Second)
	defer r.Close()

	go func() {
		time.Sleep(10 * time.Millisecond)
		ch.reply(benshi.CmdGetDevInfo, []byte{0x01})
	}()

	if err := r.Probe(); err == nil {
		t.Error("Probe must fail when the radio reports a failure status")
	}
}
```

- [ ] **Step 7: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/rig/ -run "TestGetFreqFallsBack|TestGetPTT|TestProbe" -v'`
Expected: FAIL — `r.Probe undefined`

- [ ] **Step 8: Implement the fallback, probe and TX-bit read**

```go
// htStatusTXBit is is_in_tx within the first Status byte. Status packs, MSB
// first: is_power_on, is_in_tx, is_sq, is_in_rx, double_channel(2), is_scan,
// is_radio -- so is_in_tx is bit 6.
const htStatusTXBit = 0x40

// currChannel returns the radio's currently selected channel id from
// GET_HT_STATUS. curr_ch_id_lower is the high nibble of the second Status byte.
func (r *Rig) currChannel() (byte, error) {
	body, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		return 0, err
	}
	if len(body) < 3 || body[0] != 0 {
		return 0, benshi.ErrShortBody
	}
	return body[2] >> 4, nil
}

// channelFreq reads a stored channel's RX frequency. READ_RF_CH is read-only;
// its writing counterpart is deliberately absent from this package.
func (r *Rig) channelFreq(id byte) (uint32, error) {
	body, err := r.request(benshi.CmdReadRFCh, []byte{id})
	if err != nil {
		return 0, err
	}
	// body: reply status, channel_id, tx word (mod<<30|freq), rx word.
	if len(body) < 10 || body[0] != 0 {
		return 0, benshi.ErrShortBody
	}
	return binary.BigEndian.Uint32(body[6:10]) & 0x3FFFFFFF, nil
}

// Probe confirms the radio answers the protocol at all, so callers can fail
// with a clear message rather than a timeout on every later command.
func (r *Rig) Probe() error {
	body, err := r.request(benshi.CmdGetDevInfo, nil)
	if err != nil {
		return err
	}
	if len(body) < 1 || body[0] != 0 {
		return fmt.Errorf("rig: radio rejected GET_DEV_INFO")
	}
	return nil
}
```

Then extend `GetFreq` so that when `DecodeFreqModeStatus` reports a failure
status — which is what a radio on a stored channel returns — it falls back to
`currChannel` plus `channelFreq` instead of surfacing the error.

- [ ] **Step 9: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/rig/ -race -v'`
Expected: PASS

- [ ] **Step 10: Commit**

```bash
git add internal/rig/
git commit -m "feat(rig): channel-mode frequency fallback, TX-bit read and probe"
```

---

### Task 6: `tncd rig` subcommand

The smallest thing that turns the stack into working software, and the tool that
makes spikes 2-4 runnable.

**Files:**
- Modify: `cmd/tncd/main.go` (add `rig` to the subcommand dispatch at line 82 and to the usage text at line 116)
- Create: `cmd/tncd/rig.go`
- Create: `cmd/tncd/rig_test.go`

**Interfaces:**
- Consumes: `rig.New`, `(*Rig).SetFreq`, `(*Rig).GetFreq`, `(*Rig).Teardown` (Task 5); `kiss.ControlChannelFor` (Task 4); `config.Load`
- Produces: `tncd rig -c FILE --port N get-freq|set-freq <hz>|teardown`

- [ ] **Step 1: Write the failing test**

```go
package main

import "testing"

func TestParseRigArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantCmd string
		wantHz  uint32
		wantErr bool
	}{
		{"get-freq", []string{"get-freq"}, "get-freq", 0, false},
		{"set-freq", []string{"set-freq", "145030000"}, "set-freq", 145030000, false},
		{"set-freq in MHz is rejected", []string{"set-freq", "145.03"}, "", 0, true},
		{"teardown", []string{"teardown"}, "teardown", 0, false},
		{"set-freq needs an argument", []string{"set-freq"}, "", 0, true},
		{"unknown verb", []string{"wat"}, "", 0, true},
		{"no verb", nil, "", 0, true},
	}
	for _, tc := range cases {
		cmd, hz, err := parseRigArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: err = nil, want error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: err = %v", tc.name, err)
			continue
		}
		if cmd != tc.wantCmd || hz != tc.wantHz {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", tc.name, cmd, hz, tc.wantCmd, tc.wantHz)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./cmd/tncd/ -run TestParseRigArgs -v'`
Expected: FAIL — `undefined: parseRigArgs`

- [ ] **Step 3: Write the argument parser and the subcommand**

```go
package main

import (
	"fmt"
	"strconv"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// parseRigArgs validates the `tncd rig` verb and its argument.
//
// Frequencies are Hz only. Accepting MHz would make "145.03" and "145030000"
// both plausible, and a silent factor-of-a-million error keys a transmitter on
// the wrong band.
func parseRigArgs(args []string) (string, uint32, error) {
	if len(args) == 0 {
		return "", 0, fmt.Errorf("rig: need one of get-freq, set-freq <hz>, teardown")
	}
	switch args[0] {
	case "get-freq", "teardown":
		return args[0], 0, nil
	case "set-freq":
		if len(args) < 2 {
			return "", 0, fmt.Errorf("rig set-freq: need a frequency in Hz")
		}
		hz, err := strconv.ParseUint(args[1], 10, 32)
		if err != nil {
			return "", 0, fmt.Errorf("rig set-freq: %q is not a frequency in Hz", args[1])
		}
		return "set-freq", uint32(hz), nil
	default:
		return "", 0, fmt.Errorf("rig: unknown command %q", args[0])
	}
}

// runRig opens port n's transport, borrows its control channel, and runs one
// command. It is a one-shot tool: it does not start the engine or any frontend.
func runRig(cfgPath string, port int, args []string) error {
	cmd, hz, err := parseRigArgs(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if port < 0 || port >= len(cfg.Ports) {
		return fmt.Errorf("rig: port %d is not configured", port)
	}
	tr, err := buildRigTransport(cfg.Ports[port])
	if err != nil {
		return err
	}
	if err := tr.Open(); err != nil {
		return fmt.Errorf("rig: open port %d: %w", port, err)
	}
	defer tr.Close()

	ch, err := kiss.ControlChannelFor(tr)
	if err != nil {
		return fmt.Errorf("rig: port %d: %w", port, err)
	}
	r := rig.New(ch, 5*time.Second)
	defer r.Close()

	switch cmd {
	case "get-freq":
		got, err := r.GetFreq()
		if err != nil {
			return err
		}
		fmt.Println(got)
	case "set-freq":
		if err := r.SetFreq(hz); err != nil {
			return err
		}
	case "teardown":
		if err := r.Teardown(); err != nil {
			return err
		}
	}
	return nil
}
```

`buildRigTransport` constructs a transport from a `config.Port` for this
one-shot path. Reuse the bridge's existing construction logic rather than
duplicating it: export a small constructor from `internal/bridge`'s
`buildTransport` (or move it to a shared helper) and call it here.

- [ ] **Step 4: Wire the subcommand into dispatch**

In `cmd/tncd/main.go`, add a `case "rig":` beside the existing `case "genconfig":`
that reads the `-c` config path and an optional `--port` (default 0), calls
`runRig`, and exits non-zero on error. Add a usage line beside the others:

```go
fmt.Fprintf(os.Stderr, "  rig             Query or set radio frequency (Benshi radios)\n")
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./... 2>&1 | grep -v "no test files"'`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add cmd/tncd/ internal/bridge/
git commit -m "feat(cmd): tncd rig subcommand for one-shot QSY"
```

- [ ] **Step 7: Run bench spikes 2-4**

With the binary built (`nix-shell -p go --run 'CGO_ENABLED=0 go build -o tncd ./cmd/tncd'`):

1. `./tncd rig -c tncd.ini --port 0 get-freq` — confirms the control channel resolves and the radio answers.
2. `./tncd rig -c tncd.ini --port 0 set-freq 145030000` then `get-freq` — confirms `FREQ_MODE_SET_PAR` is supported on this firmware.
3. `./tncd rig -c tncd.ini --port 0 teardown`, then power-cycle the radio — confirms teardown restores prior state and nothing persisted.

Record the results in the spec's "Spike results" section and commit.

---

### Task 7: Configuration

**Files:**
- Modify: `internal/config/config.go` (add `RigCtl` struct; add `control_channel` to `knownPortKeys` at line 138-144; add a `knownRigCtlKeys` list beside `knownAX25Keys` at line 125; parse `[rigctl.N]` sections)
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks
- Produces: `config.RigCtl{Enabled bool, ListenHost string, ListenPort int, AllowedSubnets []*net.IPNet, AllowPTT bool, PTTTimeout int}`, `cfg.RigCtl []RigCtl` indexed by port, `config.Port.ControlChannel int`

- [ ] **Step 1: Write the failing test**

```go
func TestRigCtlDefaults(t *testing.T) {
	cfg, err := Load(write(t, "[client.0]\ntype=bluetooth\nbdaddr=00:11:22:33:44:55\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.RigCtl) != 1 {
		t.Fatalf("len(RigCtl) = %d, want 1 (one per port)", len(cfg.RigCtl))
	}
	rc := cfg.RigCtl[0]
	if rc.Enabled {
		t.Error("RigCtl must be opt-in, got Enabled = true")
	}
	if rc.ListenPort != 4532 {
		t.Errorf("ListenPort = %d, want 4532 for port 0", rc.ListenPort)
	}
	if rc.AllowPTT {
		t.Error("AllowPTT must default to false")
	}
	if rc.PTTTimeout != 30 {
		t.Errorf("PTTTimeout = %d, want 30", rc.PTTTimeout)
	}
}

func TestRigCtlPortDefaultsIncrementWithIndex(t *testing.T) {
	ini := "[client.0]\ntype=bluetooth\nbdaddr=00:11:22:33:44:55\n" +
		"[client.1]\ntype=bluetooth\nbdaddr=00:11:22:33:44:66\n"
	cfg, err := Load(write(t, ini))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RigCtl[1].ListenPort != 4533 {
		t.Errorf("port 1 ListenPort = %d, want 4533", cfg.RigCtl[1].ListenPort)
	}
}

func TestRigCtlExplicitValues(t *testing.T) {
	ini := "[client.0]\ntype=bluetooth\nbdaddr=00:11:22:33:44:55\ncontrol_channel=2\n" +
		"[rigctl.0]\nenabled=true\nlisten_port=4600\nallow_ptt=true\nptt_timeout=10\n"
	cfg, err := Load(write(t, ini))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.RigCtl[0].Enabled || cfg.RigCtl[0].ListenPort != 4600 {
		t.Errorf("RigCtl[0] = %+v", cfg.RigCtl[0])
	}
	if !cfg.RigCtl[0].AllowPTT || cfg.RigCtl[0].PTTTimeout != 10 {
		t.Errorf("PTT config = %+v", cfg.RigCtl[0])
	}
	if cfg.Ports[0].ControlChannel != 2 {
		t.Errorf("ControlChannel = %d, want 2", cfg.Ports[0].ControlChannel)
	}
}

// A zero or negative key timeout would defeat the stuck-transmitter guard.
func TestRigCtlRejectsNonPositivePTTTimeout(t *testing.T) {
	ini := "[client.0]\ntype=bluetooth\nbdaddr=00:11:22:33:44:55\n" +
		"[rigctl.0]\nenabled=true\nallow_ptt=true\nptt_timeout=0\n"
	cfg, err := Load(write(t, ini))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RigCtl[0].PTTTimeout != 30 {
		t.Errorf("PTTTimeout = %d, want the 30s default to replace an invalid 0", cfg.RigCtl[0].PTTTimeout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/config/ -run TestRigCtl -v'`
Expected: FAIL — `cfg.RigCtl undefined`

- [ ] **Step 3: Write the implementation**

Add the struct beside the other section types:

```go
// RigCtl holds one [rigctl.N] section: a hamlib Net rigctl listener for port N.
//
// One listener per port because hamlib's Net rigctl protocol has no way to
// select among several rigs on one socket.
type RigCtl struct {
	Enabled        bool   // default false; the module is opt-in
	ListenHost     string // default "127.0.0.1"
	ListenPort     int    // default 4532 + N
	AllowedSubnets []*net.IPNet
	AllowPTT       bool // default false; see PTTTimeout
	// PTTTimeout is the maximum time tncd will leave the transmitter keyed
	// before force-releasing it, in seconds. Default 30. A remote key whose
	// un-key path can be swallowed by a wedged Bluetooth link is a stuck
	// transmitter, so this is not optional when AllowPTT is set.
	PTTTimeout int
}
```

Add `"control_channel"` to `knownPortKeys`, add `ControlChannel int` to
`config.Port`, add:

```go
// knownRigCtlKeys are the recognized keys in [rigctl.N].
var knownRigCtlKeys = []string{
	"enabled", "listen_host", "listen_port", "allowed_subnets",
	"allow_ptt", "ptt_timeout",
}
```

and parse one `RigCtl` per configured port, defaulting `ListenPort` to
`4532 + N`, clamping `PTTTimeout` to 30 when it is not positive (with a
`log.Printf` warning matching the style of the `n2_retry` clamp).

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/config/ -v'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): [rigctl.N] sections and port control_channel key"
```

---

### Task 8: The rigctl server — frequency commands

**Files:**
- Create: `internal/frontend/rigctl/server.go`
- Create: `internal/frontend/rigctl/server_test.go`

**Interfaces:**
- Consumes: `config.RigCtl` (Task 7); `internal/netutil` allowlist listener
- Produces: `rigctl.Rig` interface (`SetFreq(uint32) error`, `GetFreq() (uint32, error)`, `SetPTT(bool) error`, `GetPTT() (bool, error)`), `rigctl.New(cfg config.RigCtl, provider func() (Rig, error)) *Server`, `(*Server).Start() error`, `(*Server).Close() error`, `rigctl.handleLine(line string, rig Rig, cfg config.RigCtl, state *pttState) string`

- [ ] **Step 1: Write the failing test**

```go
package rigctl

import (
	"errors"
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/config"
)

type fakeRig struct {
	hz      uint32
	setErr  error
	getErr  error
	ptt     bool
	pttErr  error
	setFreq uint32
}

func (f *fakeRig) SetFreq(hz uint32) error   { f.setFreq = hz; return f.setErr }
func (f *fakeRig) GetFreq() (uint32, error)  { return f.hz, f.getErr }
func (f *fakeRig) SetPTT(on bool) error      { f.ptt = on; return f.pttErr }
func (f *fakeRig) GetPTT() (bool, error)     { return f.ptt, f.pttErr }

func TestHandleLineFrequencyCommands(t *testing.T) {
	cases := []struct {
		name string
		line string
		rig  *fakeRig
		want string
	}{
		{"get_freq returns a bare integer", `\get_freq`, &fakeRig{hz: 145030000}, "145030000"},
		{"set_freq succeeds", `\set_freq 145030000`, &fakeRig{}, "RPRT 0"},
		{"chk_vfo reports no VFO mode", `\chk_vfo`, &fakeRig{}, "CHKVFO 0"},
		{"dump_caps is a non-error ping", "dump_caps", &fakeRig{}, "RPRT 0"},
		{"unknown command", "zzz", &fakeRig{}, "RPRT -1"},
		{"set_freq with no argument", `\set_freq`, &fakeRig{}, "RPRT -1"},
		{"set_freq with junk", `\set_freq abc`, &fakeRig{}, "RPRT -1"},
		{"radio timeout", `\get_freq`, &fakeRig{getErr: errTestTimeout}, "RPRT -5"},
	}
	cfg := config.RigCtl{PTTTimeout: 30}
	for _, tc := range cases {
		got := handleLine(tc.line, tc.rig, cfg, &pttState{})
		if got != tc.want {
			t.Errorf("%s: handleLine(%q) = %q, want %q", tc.name, tc.line, got, tc.want)
		}
	}
}

func TestSetFreqPassesThroughValue(t *testing.T) {
	f := &fakeRig{}
	handleLine(`\set_freq 145030000`, f, config.RigCtl{PTTTimeout: 30}, &pttState{})
	if f.setFreq != 145030000 {
		t.Errorf("SetFreq got %d, want 145030000", f.setFreq)
	}
}

// An offline or relinking port must answer immediately, never hang.
func TestUnavailableRigAnswersIO(t *testing.T) {
	got := handleLine(`\get_freq`, nil, config.RigCtl{PTTTimeout: 30}, &pttState{})
	if got != "RPRT -6" {
		t.Errorf("handleLine with nil rig = %q, want RPRT -6", got)
	}
}

var errTestTimeout = errors.New("rig: radio did not reply in time")
```

Note: map `rig.ErrTimeout` to `RPRT -5` by `errors.Is`; the test's
`errTestTimeout` should be replaced with the real `rig.ErrTimeout` once the
import is in place.

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/frontend/rigctl/ -v'`
Expected: FAIL — `undefined: handleLine`

- [ ] **Step 3: Write the implementation**

```go
// Package rigctl serves the hamlib Net rigctl protocol over TCP.
//
// One listener per port: hamlib's protocol has no way to select among several
// rigs on one socket.
package rigctl

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
)

// hamlib rig_errcode_e values, negated on the wire as "RPRT -n".
const (
	rprtOK       = "RPRT 0"  // RIG_OK
	rprtEINVAL   = "RPRT -1" // RIG_EINVAL: bad or missing argument
	rprtENIMPL   = "RPRT -4" // RIG_ENIMPL: not implemented
	rprtETIMEOUT = "RPRT -5" // RIG_ETIMEOUT: radio did not reply
	rprtEIO      = "RPRT -6" // RIG_EIO: port offline or relinking
	rprtEPROTO   = "RPRT -8" // RIG_EPROTO: malformed reply from the radio
)

// Rig is the subset of radio control the server needs. Keeping it an interface
// lets the protocol be tested without a radio or a Bluetooth stack.
type Rig interface {
	SetFreq(hz uint32) error
	GetFreq() (uint32, error)
	SetPTT(on bool) error
	GetPTT() (bool, error)
}

// pttState tracks an active key so it can be force-released.
type pttState struct {
	mu    sync.Mutex
	timer *time.Timer
	keyed bool
}

// errToRPRT maps a rig error onto the hamlib code a client expects.
func errToRPRT(err error) string {
	switch {
	case err == nil:
		return rprtOK
	case errors.Is(err, rig.ErrTimeout):
		return rprtETIMEOUT
	case errors.Is(err, rig.ErrClosed):
		return rprtEIO
	default:
		return rprtEPROTO
	}
}

// handleLine executes one protocol line and returns the reply.
//
// A nil rig means the port is offline or mid-relink: answer RPRT -6 at once
// rather than blocking, so a wedged port cannot turn into a hung client.
func handleLine(line string, r Rig, cfg config.RigCtl, st *pttState) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return rprtEINVAL
	}
	cmd := fields[0]
	args := fields[1:]

	// Commands answerable without the radio.
	switch cmd {
	case `\chk_vfo`:
		// 0 makes PAT use an empty VFO prefix, so it never prepends VFO
		// arguments to any command.
		return "CHKVFO 0"
	case "dump_caps", `\dump_caps`:
		return rprtOK
	}

	if r == nil {
		return rprtEIO
	}

	switch cmd {
	case `\get_freq`, "f":
		hz, err := r.GetFreq()
		if err != nil {
			return errToRPRT(err)
		}
		return strconv.FormatUint(uint64(hz), 10)

	case `\set_freq`, "F":
		if len(args) < 1 {
			return rprtEINVAL
		}
		hz, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil {
			return rprtEINVAL
		}
		return errToRPRT(r.SetFreq(uint32(hz)))

	case "t", `\get_ptt`:
		on, err := r.GetPTT()
		if err != nil {
			return errToRPRT(err)
		}
		if on {
			return "1"
		}
		return "0"

	case `\set_ptt`, "T":
		return handleSetPTT(args, r, cfg, st)

	default:
		return rprtEINVAL
	}
}

// handleSetPTT is defined in Task 9.
func handleSetPTT(args []string, r Rig, cfg config.RigCtl, st *pttState) string {
	return rprtENIMPL
}

var _ = fmt.Sprintf // retained for Task 9
```

Also write `New`, `Start` and `Close`: `Start` binds
`net.Listen("tcp", net.JoinHostPort(cfg.ListenHost, strconv.Itoa(cfg.ListenPort)))`,
wraps it in the `internal/netutil` filtering listener with `cfg.AllowedSubnets`,
and serves one goroutine per connection reading newline-delimited lines through
`handleLine`, writing each reply followed by `\n`. A `q` line closes the
connection.

- [ ] **Step 4: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/frontend/rigctl/ -v'`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/frontend/rigctl/
git commit -m "feat(rigctl): hamlib Net rigctl server with frequency commands"
```

---

### Task 9: PTT, gated and time-limited

**Files:**
- Modify: `internal/frontend/rigctl/server.go` (replace the `handleSetPTT` stub)
- Modify: `internal/frontend/rigctl/server_test.go`
- Modify: `internal/rig/rig.go` (add `SetPTT`/`GetPTT`)
- Modify: `internal/rig/rig_test.go`

**Interfaces:**
- Consumes: `benshi.CmdDoProgFunc`, `benshi.CmdGetHTStatus` (Task 2); `pttState` (Task 8)
- Produces: `(*rig.Rig).SetPTT(on bool) error`, `(*rig.Rig).GetPTT() (bool, error)`, `benshi.PFEffectMainPTT`

- [ ] **Step 1: Write the failing test**

```go
func TestSetPTTRefusedWhenDisabled(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: false, PTTTimeout: 30}
	got := handleLine(`\set_ptt 1`, &fakeRig{}, cfg, &pttState{})
	if got != "RPRT -4" {
		t.Errorf("handleLine = %q, want RPRT -4 when allow_ptt is false", got)
	}
}

func TestSetPTTAllowedWhenEnabled(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	f := &fakeRig{}
	st := &pttState{}
	if got := handleLine(`\set_ptt 1`, f, cfg, st); got != "RPRT 0" {
		t.Errorf("handleLine = %q, want RPRT 0", got)
	}
	if !f.ptt {
		t.Error("rig was not keyed")
	}
	// Release so the test does not leave a timer running.
	handleLine(`\set_ptt 0`, f, cfg, st)
}

// The stuck-transmitter guard: a key nobody releases must be force-released.
func TestPTTForceReleasesOnTimeout(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 1}
	f := &fakeRig{}
	st := &pttState{}
	handleLine(`\set_ptt 1`, f, cfg, st)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !f.ptt {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("transmitter still keyed after the ptt_timeout elapsed")
}

func TestReleasePTTIsIdempotent(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	f := &fakeRig{}
	st := &pttState{}
	handleLine(`\set_ptt 1`, f, cfg, st)
	handleLine(`\set_ptt 0`, f, cfg, st)
	if got := handleLine(`\set_ptt 0`, f, cfg, st); got != "RPRT 0" {
		t.Errorf("second release = %q, want RPRT 0", got)
	}
}

// hamlib sends 0, 1 or 3; anything else is a bad argument.
func TestSetPTTRejectsBadArgument(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	for _, arg := range []string{"", "9", "x"} {
		line := `\set_ptt`
		if arg != "" {
			line += " " + arg
		}
		if got := handleLine(line, &fakeRig{}, cfg, &pttState{}); got != "RPRT -1" {
			t.Errorf("set_ptt %q = %q, want RPRT -1", arg, got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/frontend/rigctl/ -run TestPTT -v'`
Expected: FAIL — PTT is still stubbed to `RPRT -4`

- [ ] **Step 3: Implement PTT on the rig**

In `benshi/freqmode.go` (or a new `benshi/progfunc.go`):

```go
// PFEffect is a programmable-function effect id, triggerable remotely through
// DO_PROG_FUNC.
type PFEffect uint8

// PFEffectMainPTT keys the main-VFO transmitter.
const PFEffectMainPTT PFEffect = 13
```

In `internal/rig/rig.go`:

```go
// SetPTT keys or unkeys the transmitter via DO_PROG_FUNC(MAIN_PTT).
//
// The effect carries no press/release parameter, so whether a remote key can be
// HELD is unproven -- HTCommander speculates that the LOW_TO_HIGH/HIGH_TO_LOW
// edge actions are the live press/release, but does not establish it. Callers
// must treat a successful return as "the command was accepted", not as proof
// the transmitter is in the requested state, and must enforce their own maximum
// key time.
func (r *Rig) SetPTT(on bool) error {
	_, err := r.request(benshi.CmdDoProgFunc, []byte{byte(benshi.PFEffectMainPTT)})
	return err
}

// GetPTT reports whether the radio is transmitting, from GET_HT_STATUS.
// (Implemented in Task 5 Step 8 alongside htStatusTXBit; shown here only so
// this task's SetPTT has its counterpart in view.)
func (r *Rig) GetPTT() (bool, error) {
	body, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		return false, err
	}
	if len(body) < 2 || body[0] != 0 {
		return false, benshi.ErrShortBody
	}
	return body[1]&htStatusTXBit != 0, nil
}
```

`GetPTT` and `htStatusTXBit` were already implemented in Task 5 Step 8; this
task only adds `SetPTT`. Do not redefine them.

- [ ] **Step 4: Implement the gated, time-limited handler**

```go
// handleSetPTT keys or unkeys the transmitter, subject to allow_ptt and a hard
// maximum key time.
//
// The guard is not optional. A remote key whose un-key write can be silently
// swallowed by a wedged Bluetooth link is a stuck transmitter, and that failure
// mode is known to occur on these radios.
func handleSetPTT(args []string, r Rig, cfg config.RigCtl, st *pttState) string {
	if !cfg.AllowPTT {
		return rprtENIMPL
	}
	if len(args) < 1 {
		return rprtEINVAL
	}
	var on bool
	switch args[0] {
	case "0":
		on = false
	case "1", "3": // hamlib PTT_ON and PTT_ON_DATA
		on = true
	default:
		return rprtEINVAL
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if !on {
		if st.timer != nil {
			st.timer.Stop()
			st.timer = nil
		}
		st.keyed = false
		return errToRPRT(r.SetPTT(false))
	}

	if err := r.SetPTT(true); err != nil {
		return errToRPRT(err)
	}
	st.keyed = true
	if st.timer != nil {
		st.timer.Stop()
	}
	st.timer = time.AfterFunc(time.Duration(cfg.PTTTimeout)*time.Second, func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		if !st.keyed {
			return
		}
		st.keyed = false
		st.timer = nil
		_ = r.SetPTT(false)
	})
	return rprtOK
}
```

The connection handler must call the same release path when a client
disconnects, and the server's `Close` must release any keyed PTT, so a dropped
client or a shutdown cannot leave the transmitter keyed.

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/frontend/rigctl/ ./internal/rig/ -race -v'`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/frontend/rigctl/ internal/rig/ benshi/
git commit -m "feat(rigctl): optional PTT with a hard maximum key time"
```

---

### Task 10: Wire the server into the runtime

**Files:**
- Modify: `internal/bridge/bridge.go` (add a `RigFor(port int) (rigctl.Rig, error)` accessor)
- Modify: `internal/app/app.go` (start and stop a rigctl server per enabled port)
- Create: `internal/app/rigctl_test.go`

**Interfaces:**
- Consumes: `rigctl.New`, `(*Server).Start`, `(*Server).Close` (Task 8); `config.RigCtl` (Task 7); `kiss.ControlChannelFor` (Task 4); `rig.New` (Task 5)
- Produces: a running listener per enabled port

- [ ] **Step 1: Write the failing test**

```go
package app

import (
	"testing"

	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// A disabled [rigctl.N] must bind nothing.
func TestRigCtlDisabledStartsNoListener(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.RigCtl[0].Enabled = false
	rt, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Shutdown()
	if n := rt.rigCtlListenerCount(); n != 0 {
		t.Errorf("started %d rigctl listeners, want 0", n)
	}
}

func TestRigCtlEnabledStartsOneListenerPerPort(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.RigCtl[0].Enabled = true
	cfg.RigCtl[0].ListenPort = 0 // ephemeral
	rt, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer rt.Shutdown()
	if n := rt.rigCtlListenerCount(); n != 1 {
		t.Errorf("started %d rigctl listeners, want 1", n)
	}
}
```

`minimalConfig` builds a one-port config with a transport that does not touch
hardware, following whatever pattern `internal/app`'s existing tests use.

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./internal/app/ -run TestRigCtl -v'`
Expected: FAIL — `rt.rigCtlListenerCount undefined`

- [ ] **Step 3: Implement the bridge accessor**

```go
// RigFor returns a rig bound to port n's control channel, or an error if the
// port is offline, is not a control-capable transport, or has no live link.
//
// Must be called on the engine loop: it reads b.ports. The returned rig
// performs its I/O off-loop, which is the point -- a Benshi round-trip must
// never run on the engine goroutine.
func (b *Bridge) RigFor(port int) (*rig.Rig, error) {
	if port < 0 || port >= len(b.ports) {
		return nil, fmt.Errorf("bridge: port %d out of range", port)
	}
	kp, ok := b.ports[port].(*kiss.Port)
	if !ok || !b.ports[port].Online() {
		return nil, fmt.Errorf("bridge: port %d is offline", port)
	}
	ch, err := kiss.ControlChannelFor(kp.Transport())
	if err != nil {
		return nil, err
	}
	return rig.New(ch, rigRequestTimeout), nil
}
```

Add `const rigRequestTimeout = 5 * time.Second` and, if `kiss.Port` does not
already expose its transport, a `Transport()` accessor.

- [ ] **Step 4: Start the listeners in the runtime**

In `internal/app`, after the bridge is started, for each port whose
`cfg.RigCtl[i].Enabled` is true, construct a `rigctl.New` with a provider
closure that calls `bridge.RigFor(i)` **via the engine** (`eng.Do`) and returns
`(nil, err)` when the port is unavailable — the server passes that nil straight
to `handleLine`, which answers `RPRT -6` without blocking. Track the
servers in the `Runtime` and close them in `Shutdown` before the bridge, so a
keyed PTT is released while the transport is still alive.

- [ ] **Step 5: Run tests to verify they pass**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./... 2>&1 | grep -v "no test files"'`
Expected: all PASS

- [ ] **Step 6: Commit**

```bash
git add internal/app/ internal/bridge/
git commit -m "feat(app): start a rigctl listener per enabled port"
```

---

### Task 11: Control channel over classic RFCOMM

Gives Windows reach, where pure-Go BLE is not available.

**Files:**
- Modify: `kiss/bluetooth_sdp.go` (parameterize `buildSSAReq` by UUID)
- Modify: `kiss/bluetooth_linux.go` and `kiss/bluetooth_windows.go` (add `ControlChannel`)
- Modify: `kiss/bluetooth_sdp_test.go`

**Interfaces:**
- Consumes: `config.Port.ControlChannel` (Task 7); `kiss.ControlCapable` (Task 4)
- Produces: `kiss.buildSSAReq(uuid []byte) []byte` accepting 16-bit or 128-bit UUIDs

- [ ] **Step 1: Write the failing test**

```go
func TestBuildSSAReqUUID16(t *testing.T) {
	got := buildSSAReq(uuid16Bytes(uuidSPP16))
	// ServiceSearchPattern: DES len 3, then 0x19 (UUID16) and the two bytes.
	if got[5] != 0x35 || got[6] != 0x03 || got[7] != 0x19 {
		t.Errorf("search pattern = % X, want a UUID16 element", got[5:8])
	}
}

func TestBuildSSAReqUUID128(t *testing.T) {
	u := make([]byte, 16)
	got := buildSSAReq(u)
	// A 128-bit UUID uses element type 0x1C and a 17-byte sequence.
	if got[5] != 0x35 || got[6] != 0x11 || got[7] != 0x1C {
		t.Errorf("search pattern = % X, want a UUID128 element", got[5:8])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./kiss/ -run TestBuildSSAReq -v'`
Expected: FAIL — `too many arguments in call to buildSSAReq`

- [ ] **Step 3: Implement**

```go
// buildSSAReq builds an SDP ServiceSearchAttribute request for one service
// UUID. A 2-byte uuid uses data element type 0x19 (UUID16); a 16-byte uuid uses
// 0x1C (UUID128), which is what the Benshi control service needs.
func buildSSAReq(uuid []byte) []byte {
	var elemType byte
	switch len(uuid) {
	case 2:
		elemType = 0x19
	case 16:
		elemType = 0x1C
	default:
		return nil
	}
	ssp := append([]byte{0x35, byte(len(uuid) + 1), elemType}, uuid...)
	aidl := []byte{0x35, 0x03, 0x09, byte(attrProtoDescList >> 8), byte(attrProtoDescList & 0xFF)}

	var params []byte
	params = append(params, ssp...)
	params = append(params, 0xFF, 0xFF)
	params = append(params, aidl...)
	params = append(params, 0x00)

	pdu := make([]byte, 5+len(params))
	pdu[0] = sdpSSAReq
	binary.BigEndian.PutUint16(pdu[1:3], 0x0001)
	binary.BigEndian.PutUint16(pdu[3:5], uint16(len(params)))
	copy(pdu[5:], params)
	return pdu
}
```

Update the existing caller to pass the SPP UUID bytes. Then add `ControlChannel`
to the classic transports: open a second RFCOMM socket to
`port.ControlChannel`, or, when it is 0, discover the channel via
`buildSSAReq(benshiUUID128)`. Return `ErrNoControlChannel` when neither yields a
channel.

- [ ] **Step 4: Run tests and cross-compile**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./kiss/ -v && CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...'`
Expected: PASS, no build output

- [ ] **Step 5: Commit**

```bash
git add kiss/
git commit -m "feat(kiss): Benshi control channel over classic RFCOMM"
```

---

### Task 12: Documentation and the OTA checklist

**Files:**
- Create: `docs/superpowers/specs/rig-control-ota-checklist.md`
- Modify: `README.md` (document `[rigctl.N]`, `control_channel`, and `tncd rig`)
- Modify: `internal/config/example.go` (add a commented `[rigctl.0]` block)
- Modify: `CLAUDE.md` (add the new checklist to release step 3)

- [ ] **Step 1: Write the OTA checklist**

Create `docs/superpowers/specs/rig-control-ota-checklist.md` with these
criteria, in the format of `connect-setup-ota-checklist.md`:

- [ ] `tncd rig get-freq` returns the radio's actual displayed frequency
- [ ] `tncd rig set-freq` retunes the radio and the display follows
- [ ] `tncd rig teardown` returns the radio to its prior channel
- [ ] After a power-cycle, no memory channel has changed and the radio is on its
      original channel
- [ ] **Packet still works while in VFO mode** — a full KISS round-trip after a
      `set-freq`
- [ ] With the radio on a stored channel (not in VFO mode), `tncd rig get-freq`
      still returns that channel's frequency — exercises the READ_RF_CH fallback
      and confirms its request encoding
- [ ] PAT QSYs successfully via `rigctld` at the configured port
- [ ] With `allow_ptt = false`, `\set_ptt 1` returns `RPRT -4`
- [ ] With `allow_ptt = true` into a dummy load, `\set_ptt 1` keys and
      `\set_ptt 0` unkeys
- [ ] With `allow_ptt = true` into a dummy load, killing the client while keyed
      force-releases within `ptt_timeout`
- [ ] Pulling the Bluetooth link while keyed does not leave the radio keyed
      (the radio's own `tx_time_limit` is the backstop)

- [ ] **Step 2: Update README and the config example**

Document the `[rigctl.N]` keys with their defaults, the port `control_channel`
key, and `tncd rig`. State plainly that rig control is supported only on Benshi
radios, that it never writes a memory channel or persists to NVRAM, and that
PTT is off by default and experimental.

- [ ] **Step 3: Add the checklist to the release checklist**

In `CLAUDE.md` step 3, name `rig-control-ota-checklist.md` alongside the
connect-setup checklist.

- [ ] **Step 4: Verify the whole suite and the cross-compile matrix**

Run: `nix-shell -p go --run 'CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go vet ./... && for os in linux windows darwin freebsd; do CGO_ENABLED=0 GOOS=$os GOARCH=amd64 go build ./... || exit 1; done'`
Expected: all PASS, no build output

- [ ] **Step 5: Commit**

```bash
git add docs/ README.md internal/config/example.go CLAUDE.md
git commit -m "docs: rig control OTA checklist, README and config example"
```
