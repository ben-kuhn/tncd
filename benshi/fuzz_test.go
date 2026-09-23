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
