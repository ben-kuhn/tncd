package ax25

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type goldenFrame struct {
	Name    string   `json:"name"`
	Hex     string   `json:"hex"`
	Type    string   `json:"type"`
	Dst     string   `json:"dst"`
	Src     string   `json:"src"`
	Via     []string `json:"via"`
	NS      *uint8   `json:"ns"`
	NR      *uint8   `json:"nr"`
	PF      bool     `json:"pf"`
	Command bool     `json:"command"`
	PID     *uint8   `json:"pid"`
	Info    string   `json:"info"`
}

func loadGolden(t *testing.T) []goldenFrame {
	t.Helper()
	raw, err := os.ReadFile("testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenFrame
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func TestParseGolden(t *testing.T) {
	for _, c := range loadGolden(t) {
		t.Run(c.Name, func(t *testing.T) {
			raw, _ := hex.DecodeString(c.Hex)
			f, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if f.Type.String() != c.Type {
				t.Errorf("type = %s, want %s", f.Type, c.Type)
			}
			if f.Dst.String() != c.Dst || f.Src.String() != c.Src {
				t.Errorf("addr = %s>%s, want %s>%s", f.Src, f.Dst, c.Src, c.Dst)
			}
			if f.PF != c.PF {
				t.Errorf("PF = %v, want %v", f.PF, c.PF)
			}
			if c.NS != nil && f.NS != *c.NS {
				t.Errorf("NS = %d, want %d", f.NS, *c.NS)
			}
			if c.NR != nil && f.NR != *c.NR {
				t.Errorf("NR = %d, want %d", f.NR, *c.NR)
			}
			if c.Info != "" && string(f.Info) != c.Info {
				t.Errorf("info = %q, want %q", f.Info, c.Info)
			}
			if len(c.Via) != len(f.Via) {
				t.Fatalf("via count = %d, want %d", len(f.Via), len(c.Via))
			}
		})
	}
}

func TestBytesGolden(t *testing.T) {
	// Re-encoding a parsed golden frame must reproduce identical bytes.
	for _, c := range loadGolden(t) {
		t.Run(c.Name, func(t *testing.T) {
			raw, _ := hex.DecodeString(c.Hex)
			f, err := Parse(raw)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			got := f.Bytes()
			if hex.EncodeToString(got) != c.Hex {
				t.Errorf("re-encode mismatch\n got %x\nwant %s", got, c.Hex)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range [][]byte{nil, {0x01}, make([]byte, 14)} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%x): expected error", bad)
		}
	}
}

func TestExtendedIFrameRoundTrip(t *testing.T) {
	src, _ := ParseAddress("N0CALL-2")
	dst, _ := ParseAddress("KU0HN-10")
	f := &Frame{
		Dst: dst, Src: src, Type: I, Modulo: 128,
		NS: 5, NR: 3, PF: false, Command: true,
		PID: 0xF0, Info: []byte("Hi"),
	}
	raw := f.Bytes()

	// Extended I control is 2 bytes at offset 14 (dst 7 + src 7, no via).
	// byte1 = NS<<1 = 5<<1 = 0x0A ; byte2 = NR<<1|PF = 3<<1 = 0x06
	if raw[14] != 0x0A || raw[15] != 0x06 {
		t.Fatalf("extended I control = %02x %02x, want 0a 06", raw[14], raw[15])
	}
	if raw[16] != 0xF0 || string(raw[17:]) != "Hi" {
		t.Fatalf("PID/info wrong: %x", raw[16:])
	}

	g, err := ParseModulo(raw, 128)
	if err != nil {
		t.Fatal(err)
	}
	if g.Type != I || g.NS != 5 || g.NR != 3 || g.PF || string(g.Info) != "Hi" || g.PID != 0xF0 {
		t.Fatalf("round-trip mismatch: %+v", g)
	}
}

func TestExtendedSFrameAndSREJ(t *testing.T) {
	src, _ := ParseAddress("N0CALL-2")
	dst, _ := ParseAddress("KU0HN-10")
	for _, tc := range []struct {
		typ  FrameType
		bits byte // expected sBits
	}{{RR, 0}, {RNR, 1}, {REJ, 2}, {SREJ, 3}} {
		f := &Frame{Dst: dst, Src: src, Type: tc.typ, Modulo: 128, NR: 7, PF: true, Command: false}
		raw := f.Bytes()
		// byte1 = sBits<<2 | 0x01 ; byte2 = NR<<1 | PF = 7<<1|1 = 0x0F
		if raw[14] != (tc.bits<<2)|0x01 || raw[15] != 0x0F {
			t.Fatalf("%s control = %02x %02x", tc.typ, raw[14], raw[15])
		}
		g, err := ParseModulo(raw, 128)
		if err != nil {
			t.Fatal(err)
		}
		if g.Type != tc.typ || g.NR != 7 || !g.PF {
			t.Fatalf("%s round-trip: %+v", tc.typ, g)
		}
	}
}

func TestMod8SREJDecode(t *testing.T) {
	// mod-8 SREJ: control = NR<<5 | PF<<4 | (3<<2) | 0x01
	src, _ := ParseAddress("N0CALL-2")
	dst, _ := ParseAddress("KU0HN-10")
	f := &Frame{Dst: dst, Src: src, Type: SREJ, Modulo: 8, NR: 4, Command: false}
	raw := f.Bytes()
	g, _ := Parse(raw) // mod-8 wrapper
	if g.Type != SREJ || g.NR != 4 {
		t.Fatalf("mod-8 SREJ decode: %+v", g)
	}
}
