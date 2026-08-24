package agwpe

import "testing"

// FuzzParseHeader fuzzes the AGWPE 36-byte header parser. Invariants: no
// panic; on success the callsign fields never exceed the 10-byte wire field
// after NUL-stripping.
func FuzzParseHeader(f *testing.F) {
	seed := make([]byte, HeaderSize)
	seed[4] = 'D'
	copy(seed[8:18], "N0CALL")
	copy(seed[18:28], "CQ")
	f.Add(seed)
	f.Add([]byte{})
	f.Add(make([]byte, HeaderSize-1))
	f.Add(make([]byte, HeaderSize+64))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ParseHeader(data)
		if err != nil {
			return
		}
		if len(h.CallFrom) > 10 || len(h.CallTo) > 10 {
			t.Fatalf("callsign over 10 bytes: from=%q to=%q", h.CallFrom, h.CallTo)
		}
	})
}
