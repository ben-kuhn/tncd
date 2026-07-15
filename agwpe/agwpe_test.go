package agwpe

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

type goldenAGWPE struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Port     uint8  `json:"port"`
	CallFrom string `json:"call_from"`
	CallTo   string `json:"call_to"`
	PID      uint8  `json:"pid"`
	Hex      string `json:"hex"`
}

func TestGoldenRoundTrip(t *testing.T) {
	raw, err := os.ReadFile("testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []goldenAGWPE
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			b, _ := hex.DecodeString(c.Hex)
			h, err := ParseHeader(b)
			if err != nil {
				t.Fatal(err)
			}
			if h.Kind != c.Kind[0] || h.Port != c.Port ||
				h.CallFrom != c.CallFrom || h.CallTo != c.CallTo {
				t.Errorf("header = %+v, want %+v", h, c)
			}
			payload := b[HeaderSize : HeaderSize+int(h.DataLen)]
			rebuilt := Build(h.Port, h.Kind, h.PID, h.CallFrom, h.CallTo, payload)
			if hex.EncodeToString(rebuilt) != c.Hex {
				t.Errorf("rebuild mismatch\n got %x\nwant %s", rebuilt, c.Hex)
			}
		})
	}
}

func TestParseHeaderShort(t *testing.T) {
	if _, err := ParseHeader(make([]byte, 35)); err == nil {
		t.Fatal("expected error for short header")
	}
}
