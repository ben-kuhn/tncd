package ax25

import "testing"

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in   string
		call string
		ssid uint8
		err  bool
	}{
		{"KU0HN-10", "KU0HN", 10, false},
		{"ku0hn-1", "KU0HN", 1, false}, // normalized uppercase
		{"CQ", "CQ", 0, false},
		{"WIDE1-1", "WIDE1", 1, false},
		{"TOOLONGCALL", "", 0, true}, // > 6 chars
		{"KU0HN-16", "", 0, true},    // SSID > 15
		{"", "", 0, true},
	}
	for _, c := range cases {
		a, err := ParseAddress(c.in)
		if c.err != (err != nil) {
			t.Errorf("ParseAddress(%q) err = %v, want err=%v", c.in, err, c.err)
			continue
		}
		if err == nil && (a.Call != c.call || a.SSID != c.ssid) {
			t.Errorf("ParseAddress(%q) = %s-%d, want %s-%d",
				c.in, a.Call, a.SSID, c.call, c.ssid)
		}
	}
}

func TestAddressString(t *testing.T) {
	if s := (Address{Call: "KU0HN", SSID: 10}).String(); s != "KU0HN-10" {
		t.Errorf("got %q", s)
	}
	if s := (Address{Call: "CQ"}).String(); s != "CQ" {
		t.Errorf("got %q, want CQ (no -0 suffix)", s)
	}
}

func TestDecodeAddressSanitizesControlBytes(t *testing.T) {
	// F-new-3: on-air bytes outside printable ASCII must be mapped to '?' at
	// decode time so a crafted callsign (e.g. one containing CR/LF) cannot
	// forge log lines or AGWPE monitor output in the operator's display.
	raw := []byte{
		0x0D << 1, // '\r'
		0x0A << 1, // '\n'
		'A' << 1, 'B' << 1, 'C' << 1, 'D' << 1,
		0x61, // ssid 0, crh 0, ext 1 (last address)
	}
	a, ext := decodeAddress(raw)
	if !ext {
		t.Errorf("ext = false, want true")
	}
	if a.Call != "??ABCD" {
		t.Errorf("Call = %q, want %q (control bytes sanitised to '?')", a.Call, "??ABCD")
	}
	if a.String() != "??ABCD" {
		t.Errorf("String() = %q, want %q", a.String(), "??ABCD")
	}
}

func TestEncodeMasksSSID(t *testing.T) {
	// SSID out of range (>15) must not spill into the CRH/reserved bits.
	// Use SSID=0x1F (31) as in the brief; ssid&0x0F=0xF, want 0x60|(0xF<<1)|1 = 0x7f.
	// Note: the brief's chosen value (0x1F) happens to produce the same b[6] with or
	// without the mask (0x60 ORed over the high bits obscures the difference), so
	// this test is GREEN before and after the fix for 0x1F. We include it as-specified
	// and supplement with SSID=0x40 below, which actually exposes the bug.
	a := Address{Call: "KU0HN", SSID: 0x1F} // 31, invalid
	b := a.encode(false, true)
	// SSID byte: 0x60 | (ssid&0x0F)<<1 | ext(1); ssid&0x0F = 0x0F.
	want := byte(0x60 | (0x0F << 1) | 0x01)
	if b[6] != want {
		t.Fatalf("encode SSID byte = %#02x, want %#02x (SSID masked to 4 bits)", b[6], want)
	}

	// SSID=0x40 (64): without mask, ssid<<1 = 0x80, which corrupts the CRH bit.
	// crh=false so crhBit=0; the result must not have bit7 set.
	a2 := Address{Call: "KU0HN", SSID: 0x40}
	b2 := a2.encode(false, true)
	// ssid&0x0F = 0; want 0x60|(0<<1)|1 = 0x61
	want2 := byte(0x60 | (0x00 << 1) | 0x01)
	if b2[6] != want2 {
		t.Fatalf("encode SSID=0x40: byte = %#02x, want %#02x (ssid<<1 must not corrupt CRH bit)", b2[6], want2)
	}
}
