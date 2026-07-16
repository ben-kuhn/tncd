package kiss

import (
	"bytes"
	"testing"
)

func TestWrapDataEscapes(t *testing.T) {
	got := WrapData(0, []byte{0x01, FEND, 0x02, FESC, 0x03})
	want := []byte{FEND, 0x00, 0x01, FESC, TFEND, 0x02, FESC, TFESC, 0x03, FEND}
	if !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

func TestWrapDataPortNibble(t *testing.T) {
	got := WrapData(2, []byte{0xAA})
	if got[1] != 0x20 {
		t.Errorf("cmd byte = %02x, want 20 (port 2, data)", got[1])
	}
}

func TestWrapCommand(t *testing.T) {
	got := WrapCommand(0, 0x01, 40) // TXDELAY
	want := []byte{FEND, 0x01, 40, FEND}
	if !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

func TestExitFrame(t *testing.T) {
	if !bytes.Equal(ExitFrame(), []byte{0xC0, 0xFF, 0xC0}) {
		t.Errorf("got % x", ExitFrame())
	}
}

func TestDecoderRoundTrip(t *testing.T) {
	var d Decoder
	frames := d.Feed(WrapData(0, []byte{0x01, FEND, FESC, 0x04}))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	want := []byte{0x00, 0x01, FEND, FESC, 0x04} // cmd byte + unescaped payload
	if !bytes.Equal(frames[0], want) {
		t.Errorf("got % x, want % x", frames[0], want)
	}
}

func TestDecoderSplitAcrossFeeds(t *testing.T) {
	var d Decoder
	full := WrapData(0, []byte("hello world"))
	var frames [][]byte
	for _, b := range full { // one byte at a time
		frames = append(frames, d.Feed([]byte{b})...)
	}
	if len(frames) != 1 || !bytes.Equal(frames[0][1:], []byte("hello world")) {
		t.Fatalf("got %d frames: % x", len(frames), frames)
	}
}

func TestDecoderMultipleFramesOneFeed(t *testing.T) {
	var d Decoder
	buf := append(WrapData(0, []byte("a")), WrapData(1, []byte("b"))...)
	frames := d.Feed(buf)
	if len(frames) != 2 || frames[1][0] != 0x10 {
		t.Fatalf("got % x", frames)
	}
}

func TestDecoderDropsEmptyAndGarbage(t *testing.T) {
	var d Decoder
	// Noise before first FEND, then back-to-back FENDs (empty frame).
	frames := d.Feed([]byte{0x55, 0xAA, FEND, FEND, FEND})
	if len(frames) != 0 {
		t.Fatalf("got %d frames, want 0", len(frames))
	}
}

// TestDecoderSingleFENDDelimiter verifies that a stream using single-FEND
// frame delimiters (FEND+f1+FEND+f2+FEND) produces both frames.
// kiss3 splits on FEND and keeps every non-empty segment.
func TestDecoderSingleFENDDelimiter(t *testing.T) {
	f1 := []byte{0x00, 0xAA, 0xBB} // cmd+payload for frame 1
	f2 := []byte{0x10, 0xCC, 0xDD} // cmd+payload for frame 2

	stream := []byte{FEND}
	stream = append(stream, f1...)
	stream = append(stream, FEND)
	stream = append(stream, f2...)
	stream = append(stream, FEND)

	// Test with full feed at once.
	var d1 Decoder
	frames := d1.Feed(stream)
	if len(frames) != 2 {
		t.Fatalf("full feed: got %d frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0], f1) {
		t.Errorf("frame[0] = % x, want % x", frames[0], f1)
	}
	if !bytes.Equal(frames[1], f2) {
		t.Errorf("frame[1] = % x, want % x", frames[1], f2)
	}

	// Test byte-by-byte feed.
	var d2 Decoder
	var all [][]byte
	for _, b := range stream {
		all = append(all, d2.Feed([]byte{b})...)
	}
	if len(all) != 2 {
		t.Fatalf("byte-by-byte: got %d frames, want 2", len(all))
	}
}
