package kiss

import "testing"

// FuzzDemuxFeed fuzzes the KISS/Gaia demultiplexer -- the first thing bytes
// arriving from the radio hit once a rig-control consumer is attached.
// Invariants, all checked on every call:
//   - no panic
//   - emitted KISS frames are never empty and never exceed MaxFrameSize
//     (the same bound Decoder.Feed itself guarantees)
//   - the pending Gaia candidate buffer never exceeds the documented bound
//     (a uint8 length byte caps a frame at gaiaHeaderLen+255+gaiaMsgHeaderLen+1
//     bytes, regardless of how much or how little input has been fed)
//   - the underlying KISS decoder's own buffer never exceeds MaxFrameSize
//     either, proving Gaia-looking bytes are never leaked into it
//
// The control channel is a buffered channel, drained after every Feed call,
// so a real Gaia frame recognised during fuzzing does not itself make the
// non-blocking delivery path (already covered directly in demux_test.go)
// the thing under test here.
func FuzzDemuxFeed(f *testing.F) {
	f.Add(WrapData(0, []byte("hello")))
	f.Add(append(WrapData(0, []byte("before")), append([]byte{0xFF, 0x01, 0x00, 0x04, 1, 2, 3, 4},
		WrapData(0, []byte("after"))...)...))
	f.Add([]byte{0xFF, 0x01, 0x00, 0x00})       // minimal valid Gaia header, empty-ish body
	f.Add([]byte{0xFF, 0x99, 0xC0, 0x00, 0xC0}) // bad version byte immediately followed by KISS
	f.Add([]byte{0xFF})
	f.Add([]byte{FEND, FESC, TFEND, FESC, TFESC, FEND})
	f.Add(make([]byte, 300)) // long run of zero bytes
	f.Add([]byte{})

	const maxGaiaBuf = gaiaHeaderLen + 255 + gaiaMsgHeaderLen + 1

	f.Fuzz(func(t *testing.T, data []byte) {
		d := &demux{}
		ch := make(chan []byte, 64)
		if !d.attach(ch) {
			t.Fatal("attach failed on a fresh demux")
		}

		// Feed in small, odd-sized chunks to stress split-frame paths on
		// both sides (KISS and Gaia) the way arbitrary transport Read
		// boundaries would.
		for i := 0; i < len(data); i += 5 {
			end := i + 5
			if end > len(data) {
				end = len(data)
			}
			frames := d.Feed(data[i:end])
			for _, fr := range frames {
				if len(fr) == 0 || len(fr) > MaxFrameSize {
					t.Fatalf("emitted KISS frame of %d bytes", len(fr))
				}
			}
			if len(d.gaiaBuf) > maxGaiaBuf {
				t.Fatalf("gaiaBuf grew to %d bytes, exceeds bound %d", len(d.gaiaBuf), maxGaiaBuf)
			}
			if len(d.kissDec.buf) > MaxFrameSize {
				t.Fatalf("kissDec buffer grew to %d bytes, exceeds MaxFrameSize %d (Gaia bytes leaked into it)",
					len(d.kissDec.buf), MaxFrameSize)
			}
		drain:
			for {
				select {
				case <-ch:
				default:
					break drain
				}
			}
		}
	})
}
