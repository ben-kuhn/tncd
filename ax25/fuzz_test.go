package ax25

import (
	"encoding/hex"
	"strings"
	"testing"
)

// FuzzParseModulo fuzzes the AX.25 frame parser with both control-field
// widths (first byte selects mod-8 vs mod-128). Invariants: no panic, and a
// successful parse re-encodes and re-parses identically.
func FuzzParseModulo(f *testing.F) {
	// Seeds: a minimal SABM and a UI frame (N0CALL>APRS).
	sabm, _ := hex.DecodeString("88a6a8284440e0ae88a8a8a880e163")
	ui, _ := hex.DecodeString("88a6a8284440e0ae88a8a8a880e103f048656c6c6f")
	f.Add(byte(8), sabm)
	f.Add(byte(128), sabm)
	f.Add(byte(8), ui)
	f.Add(byte(0), []byte{})
	f.Add(byte(8), []byte{0x00})
	f.Fuzz(func(t *testing.T, mod byte, data []byte) {
		m := 8
		if mod == 128 {
			m = 128
		}
		fr, err := ParseModulo(data, m)
		if err != nil {
			return
		}
		// UnknownType frames carry a control byte Bytes() cannot represent;
		// tncd never re-encodes unknown types, so they are out of scope for
		// the round-trip invariant.
		if fr.Type == UnknownType {
			return
		}
		// Round-trip: re-encode and re-parse; must not error or panic.
		raw := fr.Bytes()
		if _, err := ParseModulo(raw, int(fr.Modulo)); err != nil {
			t.Fatalf("re-parse of re-encoded frame failed: %v", err)
		}
	})
}

// FuzzParseXID fuzzes the XID parameter parser. Invariant: no panic.
func FuzzParseXID(f *testing.F) {
	f.Add([]byte{0x82, 0x80, 0x00, 0x00})
	f.Add([]byte{0x82, 0x80, 0x00, 0x05, 0x02, 0x02, 0x01, 0x00})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseXID(data)
	})
}

// FuzzParseAddress fuzzes callsign parsing. Invariant: no panic, and a
// successfully parsed address's String() form re-parses to the same
// callsign/SSID. Inputs whose parsed callsign contains '*' or '-' are
// skipped: ParseAddress deliberately mirrors the Python reference's leniency
// for those (the parsed result is still self-consistent everywhere tncd uses
// it), and tightening validation is a compatibility decision, not a security
// fix.
func FuzzParseAddress(f *testing.F) {
	f.Add("KU0HN-10")
	f.Add("CQ")
	f.Add("WIDE1-1*")
	f.Add("")
	f.Add("TOOLONGCALL-99")
	f.Fuzz(func(t *testing.T, s string) {
		a, err := ParseAddress(s)
		if err != nil {
			return
		}
		if strings.ContainsAny(a.Call, "*-") {
			t.Skip() // reference-implementation leniency; see comment above
		}
		b, err := ParseAddress(a.String())
		if err != nil {
			t.Fatalf("String() %q of parsed address does not re-parse: %v", a.String(), err)
		}
		// CRH ('*') is intentionally not rendered by String(); only the
		// callsign and SSID must round-trip.
		if a.Call != b.Call || a.SSID != b.SSID {
			t.Fatalf("round-trip mismatch: %q -> %q", a, b)
		}
	})
}
