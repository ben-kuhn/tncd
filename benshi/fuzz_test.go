package benshi

import (
	"bytes"
	"testing"
)

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

func FuzzDecodeMessage(f *testing.F) {
	f.Add([]byte{0x00, 0x02, 0x00, 0x24})
	f.Add([]byte{0x00, 0x02, 0x80, 0x24, 0x00, 0x09, 0xB0, 0x50, 0xF0})
	f.Add([]byte{0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := DecodeMessage(data)
		if err != nil {
			return
		}
		// A decoded message must re-serialize to the same bytes -- exact
		// equality, not just matching length, or a decoder that silently
		// swaps/drops bytes within the body would slip through.
		if got := m.Bytes(); !bytes.Equal(got, data) {
			t.Fatalf("round-trip % X != input % X", got, data)
		}
	})
}

// FuzzDecodeFreqMode exercises both untrusted-bytes decoders added for VFO-mode
// QSY. They parse radio-sourced bytes over BLE/RFCOMM, so per the project's
// fuzz-every-untrusted-parser rule they need a target even though the plan that
// added them didn't call one out.
func FuzzDecodeFreqMode(f *testing.F) {
	f.Add([]byte{0x00, 0x09, 0xB0, 0x50, 0xF0}) // valid FREQ_MODE_GET_STATUS reply
	f.Add([]byte{ // valid notification 14
		14,
		0x08, 0xA4, 0xFB, 0x70,
		0x08, 0xA4, 0xFB, 0x70,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x40,
	})
	f.Add(make([]byte, 15)) // notification type 0 -- wrong type, same length as valid
	f.Add([]byte{0x00, 0x09})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Same bytes handed to both decoders: each interprets an unrelated
		// wire shape, so garbage for one is a fine stress input for the
		// other, and this keeps a single corpus covering both.
		if freq, err := DecodeFreqModeStatus(data); err == nil && freq&^uint32(freqMask) != 0 {
			t.Fatalf("DecodeFreqModeStatus: freq %#x has bits set above the 30-bit field mask", freq)
		}
		if status, err := DecodeFreqModeNotification(data); err == nil {
			if status.RXFreqHz&^uint32(freqMask) != 0 {
				t.Fatalf("DecodeFreqModeNotification: RXFreqHz %#x has bits set above the 30-bit field mask", status.RXFreqHz)
			}
			if status.TXFreqHz&^uint32(freqMask) != 0 {
				t.Fatalf("DecodeFreqModeNotification: TXFreqHz %#x has bits set above the 30-bit field mask", status.TXFreqHz)
			}
		}
	})
}
