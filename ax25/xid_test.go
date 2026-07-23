package ax25

import (
	"encoding/hex"
	"testing"
)

// Golden XID response advertising: half-duplex, SREJ none (REJ only),
// modulo 128, N1 = 256 bytes (2048 bits), window = 3.
// Structure: FI GI GLhi GLlo | PI PL PV... per Direwolf xid.c format.
//
//	82 80 00 10                    FI, GI, group length = 0x10
//	02 02 21 00                    Classes: Balanced_ABM|Half_Duplex = 0x2100
//	03 03 82 a8 02                 HDLC opt: Ext_Addr|TEST|16bitFCS|Sync|REJ|Modulo128
//	06 02 08 00                    I Field Len Rx = 2048 bits
//	08 01 03                       Window Rx = 3
//
// Grouped one TLV per span for readability; stripSpaces removes the spaces.
const goldenXIDResp = "82800010 02022100 030382a802 06020800 080103"

func TestEncodeXIDResponse(t *testing.T) {
	p := XIDParams{
		FullDuplex:       false,
		SREJ:             SREJNone,
		Modulo:           128,
		IFieldLenRxBytes: 256,
		WindowRx:         3,
	}
	got := p.Encode(false) // response
	want, _ := hex.DecodeString(stripSpaces(goldenXIDResp))
	if hex.EncodeToString(got) != hex.EncodeToString(want) {
		t.Fatalf("XID encode\n got %x\nwant %x", got, want)
	}
}

func TestParseXIDRoundTrip(t *testing.T) {
	want, _ := hex.DecodeString(stripSpaces(goldenXIDResp))
	p, err := ParseXID(want)
	if err != nil {
		t.Fatal(err)
	}
	if p.Modulo != 128 || p.SREJ != SREJNone || p.IFieldLenRxBytes != 256 || p.WindowRx != 3 || p.FullDuplex {
		t.Fatalf("parsed XID wrong: %+v", p)
	}
}

func TestXIDMultiSREJRoundTrip(t *testing.T) {
	p := XIDParams{SREJ: SREJMulti, Modulo: 128, IFieldLenRxBytes: 256, WindowRx: 7}
	got, err := ParseXID(p.Encode(true))
	if err != nil {
		t.Fatal(err)
	}
	if got.SREJ != SREJMulti {
		t.Fatalf("round-trip SREJ = %v, want SREJMulti", got.SREJ)
	}
}

func stripSpaces(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
