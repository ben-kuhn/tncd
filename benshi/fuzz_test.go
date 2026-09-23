package benshi

import "testing"

func FuzzDecoder(f *testing.F) {
	f.Add([]byte{0xFF, 0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x04})
	f.Add([]byte{0xFF, 0x01, 0x01, 0x02, 0x00, 0x02, 0x00, 0x23, 0xAA, 0xBB, 0x7F})
	f.Add([]byte{0xFF, 0x01, 0xFF, 0xFF})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := NewDecoder()
		// A real link feeds one long-lived Decoder many sequential chunks (BLE
		// splits on ATT MTU, RFCOMM on socket reads) -- it's never one Feed
		// call per connection. Split the input into small chunks fed to the
		// SAME decoder so accumulation across calls is exercised: a single
		// Feed(data) can never accumulate more than len(data), which made the
		// original "buffer grew unboundedly" check unreachable.
		const chunk = 3
		for i := 0; i < len(data); i += chunk {
			end := i + chunk
			if end > len(data) {
				end = len(data)
			}
			if _, err := d.Feed(data[i:end]); err != nil {
				t.Fatalf("Feed: %v", err)
			}
			// maxPending is the most a decoder can legitimately have buffered
			// while waiting for a frame to complete. After Feed returns, the
			// buffer either is empty or starts at a resynced frameStart byte
			// (the resync loop runs to exhaustion before any return). The
			// wire length byte is 8 bits, so a single frame's data field is
			// bounded by maxFrameData; add frameHeaderLen for the FF 01 flags
			// len prefix and this bounds any pending, not-yet-complete frame
			// -- regardless of how many bytes were fed per call.
			const maxPending = frameHeaderLen + maxFrameData
			if len(d.buf) > maxPending {
				t.Fatalf("decoder buffer grew to %d bytes (want <= %d) after feeding %d of %d input bytes", len(d.buf), maxPending, end, len(data))
			}
		}
	})
}
