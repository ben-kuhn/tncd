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
