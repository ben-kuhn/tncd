package kiss

import "testing"

// FuzzDecoderFeed fuzzes the KISS stream decoder. Invariants: no panic, the
// internal buffer never exceeds MaxFrameSize (the F1 cap), and the oversize
// drop counter only increases.
func FuzzDecoderFeed(f *testing.F) {
	f.Add(WrapData(0, []byte("hello")))
	f.Add([]byte{FEND, FESC, TFEND, FESC, TFESC, FEND})
	f.Add([]byte{FEND})                   // open frame, never closed
	f.Add(make([]byte, MaxFrameSize+100)) // unterminated garbage, over cap
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		var d Decoder
		var last uint64
		// Feed in odd-sized chunks to stress split-frame paths.
		for i := 0; i < len(data); i += 7 {
			end := i + 7
			if end > len(data) {
				end = len(data)
			}
			frames := d.Feed(data[i:end])
			if len(d.buf) > MaxFrameSize {
				t.Fatalf("decoder buffer %d bytes exceeds MaxFrameSize %d", len(d.buf), MaxFrameSize)
			}
			if d.DroppedOversize < last {
				t.Fatalf("DroppedOversize went backwards: %d < %d", d.DroppedOversize, last)
			}
			last = d.DroppedOversize
			for _, fr := range frames {
				if len(fr) == 0 || len(fr) > MaxFrameSize {
					t.Fatalf("emitted frame of %d bytes", len(fr))
				}
			}
		}
	})
}

// FuzzParseRFCOMMChannel fuzzes the SDP response parser (FreeBSD Bluetooth).
// Invariant: no panic.
func FuzzParseRFCOMMChannel(f *testing.F) {
	// Minimal SSA response shell; nested sequence seeds come from the unit tests.
	f.Add([]byte{sdpSSAResp, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00})
	f.Add([]byte{0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseRFCOMMChannel(data)
	})
}
