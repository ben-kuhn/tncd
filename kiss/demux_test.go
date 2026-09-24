package kiss

import (
	"bytes"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// gaiaFrame builds real Gaia wire bytes via the benshi package's own encoder,
// so these tests are checked against the same format internal/rig actually
// speaks, not a hand-rolled approximation of it.
func gaiaFrame(body ...byte) []byte {
	data := append([]byte{0x00, 0x02, 0x00, 0x24}, body...) // group=2, cmd=0x24 (FREQ_MODE_GET_STATUS)
	return benshi.Frame{Flags: benshi.FlagNone, Data: data}.Bytes()
}

// attachedDemux returns a demux with a buffered control channel already
// attached, and the channel to read deliveries from.
func attachedDemux(t *testing.T) (*demux, chan []byte) {
	t.Helper()
	d := &demux{}
	ch := make(chan []byte, 8)
	if !d.attach(ch) {
		t.Fatal("attach failed on a fresh demux")
	}
	return d, ch
}

// The common case: with no Gaia frames ever on the wire, the demux must
// extract exactly the same KISS frames, in the same order, as feeding the
// raw bytes straight to a plain Decoder. This is the test that matters most:
// it proves inserting the demux changes nothing for ordinary KISS traffic.
func TestDemuxKISSOnlyPassthroughByteIdentical(t *testing.T) {
	raw := append(WrapData(0, []byte("frame one")), WrapData(1, []byte("frame two"))...)
	raw = append(raw, WrapCommand(0, 0x01, 40)...)

	var want Decoder
	wantFrames := want.Feed(raw)

	d := &demux{}
	gotFrames := d.Feed(raw)

	if len(gotFrames) != len(wantFrames) {
		t.Fatalf("got %d frames, want %d", len(gotFrames), len(wantFrames))
	}
	for i := range wantFrames {
		if !bytes.Equal(gotFrames[i], wantFrames[i]) {
			t.Errorf("frame %d = % X, want % X", i, gotFrames[i], wantFrames[i])
		}
	}
}

// Also prove byte-identical passthrough when the raw bytes are split across
// many small Feed calls -- the shape a real transport Read delivers.
func TestDemuxKISSOnlyPassthroughSplitReads(t *testing.T) {
	raw := append(WrapData(0, []byte("split me across many reads")), WrapData(0, []byte("second frame"))...)

	var want Decoder
	var wantFrames [][]byte
	for i := 0; i < len(raw); i += 3 {
		end := i + 3
		if end > len(raw) {
			end = len(raw)
		}
		wantFrames = append(wantFrames, want.Feed(raw[i:end])...)
	}

	d := &demux{}
	var gotFrames [][]byte
	for i := 0; i < len(raw); i += 3 {
		end := i + 3
		if end > len(raw) {
			end = len(raw)
		}
		gotFrames = append(gotFrames, d.Feed(raw[i:end])...)
	}

	if len(gotFrames) != len(wantFrames) {
		t.Fatalf("got %d frames, want %d", len(gotFrames), len(wantFrames))
	}
	for i := range wantFrames {
		if !bytes.Equal(gotFrames[i], wantFrames[i]) {
			t.Errorf("frame %d = % X, want % X", i, gotFrames[i], wantFrames[i])
		}
	}
}

// Interleaved KISS / Gaia / KISS: both KISS frames must arrive intact and in
// order, and the Gaia frame must reach the attached consumer.
func TestDemuxInterleavedKISSGaiaKISS(t *testing.T) {
	d, ch := attachedDemux(t)

	k1 := WrapData(0, []byte("before the gaia frame"))
	gaia := gaiaFrame(0x00, 0x09, 0xB0, 0x50, 0xF0)
	k2 := WrapData(0, []byte("after the gaia frame"))

	raw := append(append(append([]byte{}, k1...), gaia...), k2...)
	frames := d.Feed(raw)

	if len(frames) != 2 {
		t.Fatalf("got %d KISS frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0][1:], []byte("before the gaia frame")) {
		t.Errorf("frame 0 = %q", frames[0][1:])
	}
	if !bytes.Equal(frames[1][1:], []byte("after the gaia frame")) {
		t.Errorf("frame 1 = %q", frames[1][1:])
	}

	select {
	case got := <-ch:
		if !bytes.Equal(got, gaia) {
			t.Errorf("gaia frame delivered = % X, want % X", got, gaia)
		}
	default:
		t.Fatal("gaia frame never reached the control consumer")
	}
}

// A Gaia frame split across several reads must still reassemble correctly,
// including when the split lands inside the 4-byte header (before the total
// length is even known).
func TestDemuxGaiaSplitAcrossReads(t *testing.T) {
	d, ch := attachedDemux(t)
	raw := gaiaFrame(0xAA, 0xBB, 0xCC, 0xDD, 0xEE)

	for i := 0; i < len(raw)-1; i++ {
		frames := d.Feed(raw[i : i+1])
		if len(frames) != 0 {
			t.Fatalf("unexpected KISS frame at byte %d", i)
		}
		select {
		case got := <-ch:
			t.Fatalf("gaia frame delivered early at byte %d: % X", i, got)
		default:
		}
	}
	d.Feed(raw[len(raw)-1:])

	select {
	case got := <-ch:
		if !bytes.Equal(got, raw) {
			t.Errorf("reassembled = % X, want % X", got, raw)
		}
	default:
		t.Fatal("gaia frame never completed")
	}
}

// A malformed Gaia frame (bad version byte) must be discarded, resynchronize,
// and NOT swallow the KISS frame that follows it.
func TestDemuxMalformedGaiaThenKISSFollows(t *testing.T) {
	d, ch := attachedDemux(t)

	malformed := []byte{0xFF, 0x99, 0x00, 0x00} // 0xFF then a bad version byte
	good := WrapData(0, []byte("survives the malformed gaia frame"))
	raw := append(append([]byte{}, malformed...), good...)

	frames := d.Feed(raw)
	if len(frames) != 1 {
		t.Fatalf("got %d KISS frames, want 1", len(frames))
	}
	if !bytes.Equal(frames[0][1:], []byte("survives the malformed gaia frame")) {
		t.Errorf("frame = %q", frames[0][1:])
	}
	select {
	case got := <-ch:
		t.Fatalf("malformed input must not be delivered as a gaia frame: % X", got)
	default:
	}
}

// A truncated Gaia frame (header complete, body never arrives in full)
// cannot be distinguished from "a large frame arriving slowly" -- a length
// byte alone gives no signal that the peer has abandoned the frame. So per
// its documented contract the demux keeps consuming bytes toward the
// frame's own claimed length, including bytes that would otherwise have
// been a KISS frame. What this test proves is that the cost of that is
// BOUNDED: a length byte is one uint8, so at most
// gaiaHeaderLen+255+gaiaMsgHeaderLen bytes are ever consumed this way before
// the stream resynchronizes on its own -- it cannot wedge forever as long as
// bytes keep arriving.
func TestDemuxTruncatedGaiaFrameBoundedRecovery(t *testing.T) {
	d, _ := attachedDemux(t)
	header := []byte{0xFF, 0x01, 0x00, 0xFF} // flags=0 (no checksum), length=255 -> dataLen=259
	if frames := d.Feed(header); len(frames) != 0 {
		t.Fatalf("got %d KISS frames from a bare Gaia header, want 0", len(frames))
	}

	const claimedTotal = gaiaHeaderLen + 255 + gaiaMsgHeaderLen // 263; no checksum byte
	if d.gaiaTotal != claimedTotal {
		t.Fatalf("gaiaTotal = %d, want %d", d.gaiaTotal, claimedTotal)
	}

	// Filler satisfying the claimed length -- any bytes, including ones that
	// look like KISS delimiters, are legitimately consumed as this frame's
	// payload. That consumption is exactly the documented, bounded cost.
	filler := bytes.Repeat([]byte{0xAA}, claimedTotal-len(header))
	d.Feed(filler)
	if len(d.gaiaBuf) != 0 {
		t.Fatalf("gaiaBuf still pending after the claimed length was satisfied: %d bytes", len(d.gaiaBuf))
	}

	// The stream is resynchronized: a real KISS frame now arrives normally.
	recovered := d.Feed(WrapData(0, []byte("this one must still arrive")))
	if len(recovered) != 1 || !bytes.Equal(recovered[0][1:], []byte("this one must still arrive")) {
		t.Fatalf("stream did not resynchronize: recovered=%v", recovered)
	}
}

// Gaia frames with no consumer attached must be discarded cleanly, without
// affecting the KISS frames around them.
func TestDemuxGaiaDiscardedWithoutConsumer(t *testing.T) {
	d := &demux{} // no attach call
	k1 := WrapData(0, []byte("one"))
	gaia := gaiaFrame(0x00)
	k2 := WrapData(0, []byte("two"))
	raw := append(append(append([]byte{}, k1...), gaia...), k2...)

	frames := d.Feed(raw)
	if len(frames) != 2 {
		t.Fatalf("got %d KISS frames, want 2", len(frames))
	}
	if !bytes.Equal(frames[0][1:], []byte("one")) || !bytes.Equal(frames[1][1:], []byte("two")) {
		t.Fatalf("frames = %q, %q", frames[0][1:], frames[1][1:])
	}
}

// A peer sending 0xFF forever must not grow the demux's buffer without
// bound: each pair of 0xFF bytes is accepted-then-rejected, never
// accumulating past a single byte of pending state.
func TestDemuxUnboundedFFDoesNotGrowBuffer(t *testing.T) {
	d := &demux{}
	raw := bytes.Repeat([]byte{0xFF}, 100000)
	frames := d.Feed(raw)
	if len(frames) != 0 {
		t.Fatalf("got %d spurious KISS frames from pure 0xFF input", len(frames))
	}
	if len(d.gaiaBuf) > 1 {
		t.Fatalf("gaiaBuf grew to %d bytes on a run of 0xFF, want <= 1", len(d.gaiaBuf))
	}
}

// A checksum-flagged Gaia frame carries one extra trailing byte, which the
// demux must include in what it delivers (parsing/verifying the checksum
// itself is internal/rig's job, via benshi.Decoder).
func TestDemuxGaiaChecksumFrame(t *testing.T) {
	d, ch := attachedDemux(t)
	raw := []byte{0xFF, 0x01, 0x01, 0x00, 0x00, 0x02, 0x00, 0x04, 0x7F} // benshi's own checksum fixture
	frames := d.Feed(raw)
	if len(frames) != 0 {
		t.Fatalf("got %d KISS frames from pure Gaia input, want 0", len(frames))
	}
	select {
	case got := <-ch:
		if !bytes.Equal(got, raw) {
			t.Errorf("delivered = % X, want % X", got, raw)
		}
	default:
		t.Fatal("checksum frame never delivered")
	}
}

// Only one control consumer may be attached at a time.
func TestDemuxAttachOnlyOneConsumer(t *testing.T) {
	d := &demux{}
	ch1 := make(chan []byte, 1)
	if !d.attach(ch1) {
		t.Fatal("first attach failed")
	}
	ch2 := make(chan []byte, 1)
	if d.attach(ch2) {
		t.Fatal("second attach succeeded while the first is still active")
	}
	d.detach(ch1)
	if !d.attach(ch2) {
		t.Fatal("attach after detach failed")
	}
}

// detach must not close the channel: deliverGaia is its only sender, and
// closing it from the consumer side risks a send-on-closed-channel panic on
// an in-flight delivery. Proven here by sending after detach and confirming
// no panic, rather than by inspecting unexported channel state.
func TestDemuxDetachDoesNotCloseChannel(t *testing.T) {
	d := &demux{}
	ch := make(chan []byte, 1)
	d.attach(ch)
	d.detach(ch)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("send after detach panicked: %v", r)
		}
	}()
	select {
	case ch <- []byte("still open"):
	default:
		t.Fatal("channel unexpectedly full/unusable immediately after detach")
	}
}

// A full control channel must never block Feed -- a slow or wedged rig
// consumer must not be able to stall KISS decoding.
func TestDemuxFullControlChannelDoesNotBlock(t *testing.T) {
	d := &demux{}
	ch := make(chan []byte) // unbuffered, unread: any blocking send would hang
	d.attach(ch)

	done := make(chan struct{})
	go func() {
		raw := append(gaiaFrame(0x01), WrapData(0, []byte("must still arrive"))...)
		frames := d.Feed(raw)
		if len(frames) != 1 || !bytes.Equal(frames[0][1:], []byte("must still arrive")) {
			t.Errorf("frames = %v", frames)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Feed blocked on a full/unread control channel")
	}
}
