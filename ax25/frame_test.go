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
