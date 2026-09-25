package benshi

import (
	"bytes"
	"testing"
)

// Golden records captured from a BTech UV-PRO over Bluetooth on 2026-09-24
// (READ_RF_CH reply bodies with the leading reply-status byte stripped).
var (
	// Channel 252: the VFO's backing record. Unnamed, which is what makes it
	// writable -- see Name's use as internal/rig's guard.
	goldenVFOCh = []byte{
		0xfc, 0x08, 0xa4, 0xfb, 0x70, 0x08, 0xa4, 0xfb, 0x70,
		0x00, 0x00, 0x00, 0x00, 0x14, 0x00,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
	}
	// Channel 0: a real memory the operator programmed.
	goldenAPRSCh = []byte{
		0x00, 0x08, 0x9b, 0x37, 0x70, 0x08, 0x9b, 0x37, 0x70,
		0x00, 0x00, 0x00, 0x00, 0x5c, 0x00,
		'A', 'P', 'R', 'S', 0, 0, 0, 0, 0, 0,
	}
)

func TestParseRFChGolden(t *testing.T) {
	for _, tc := range []struct {
		name   string
		raw    []byte
		id     byte
		rx, tx uint32
		chName string
	}{
		{"VFO record 252", goldenVFOCh, 252, 145030000, 145030000, ""},
		{"memory 0 APRS", goldenAPRSCh, 0, 144390000, 144390000, "APRS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseRFCh(tc.raw)
			if err != nil {
				t.Fatalf("ParseRFCh: %v", err)
			}
			if got := c.ID(); got != tc.id {
				t.Errorf("ID = %d, want %d", got, tc.id)
			}
			if got := c.RXFreqHz(); got != tc.rx {
				t.Errorf("RXFreqHz = %d, want %d", got, tc.rx)
			}
			if got := c.TXFreqHz(); got != tc.tx {
				t.Errorf("TXFreqHz = %d, want %d", got, tc.tx)
			}
			if got := c.Name(); got != tc.chName {
				t.Errorf("Name = %q, want %q", got, tc.chName)
			}
		})
	}
}

func TestParseRFChTooShort(t *testing.T) {
	if _, err := ParseRFCh(goldenVFOCh[:RFChLen-1]); err != ErrShortRFCh {
		t.Errorf("ParseRFCh(short): err = %v, want ErrShortRFCh", err)
	}
}

// TestRFChBytesRoundTrips is the property the read-modify-write QSY depends
// on: a record that is parsed and re-serialised without modification must be
// byte-identical, or writing it back would corrupt fields this package does
// not model.
func TestRFChBytesRoundTrips(t *testing.T) {
	c, err := ParseRFCh(goldenAPRSCh)
	if err != nil {
		t.Fatalf("ParseRFCh: %v", err)
	}
	if got := c.Bytes(); !bytes.Equal(got, goldenAPRSCh) {
		t.Errorf("Bytes() = % x, want % x", got, goldenAPRSCh)
	}
}

// TestRFChWithFreqPreservesEverythingElse is the other half of that property:
// a QSY must move both frequencies and touch nothing else, including the
// modulation bits packed into the top of each frequency word.
func TestRFChWithFreqPreservesEverythingElse(t *testing.T) {
	c, err := ParseRFCh(goldenAPRSCh)
	if err != nil {
		t.Fatalf("ParseRFCh: %v", err)
	}
	const target = 145670000
	got := c.WithFreq(target).Bytes()
	if len(got) != len(goldenAPRSCh) {
		t.Fatalf("WithFreq changed length: %d, want %d", len(got), len(goldenAPRSCh))
	}
	tuned, err := ParseRFCh(got)
	if err != nil {
		t.Fatalf("ParseRFCh(tuned): %v", err)
	}
	if tuned.RXFreqHz() != target || tuned.TXFreqHz() != target {
		t.Errorf("tuned rx/tx = %d/%d, want %d both", tuned.RXFreqHz(), tuned.TXFreqHz(), target)
	}
	if tuned.Name() != "APRS" {
		t.Errorf("WithFreq clobbered the name: %q", tuned.Name())
	}
	if tuned.ID() != c.ID() {
		t.Errorf("WithFreq changed the channel id: %d, want %d", tuned.ID(), c.ID())
	}
	// Every byte outside the two frequency words must be untouched.
	if !bytes.Equal(got[9:], goldenAPRSCh[9:]) {
		t.Errorf("WithFreq changed bytes past the frequency words:\n got % x\nwant % x", got[9:], goldenAPRSCh[9:])
	}
	// The modulation bits live in the top 2 bits of each word.
	if got[1]&0xC0 != goldenAPRSCh[1]&0xC0 || got[5]&0xC0 != goldenAPRSCh[5]&0xC0 {
		t.Errorf("WithFreq changed the modulation bits: tx %#02x rx %#02x", got[1]&0xC0, got[5]&0xC0)
	}
}

// TestRFChPreservesDMRTail covers the longer DMR record shape: the extra
// fields sit past the name, so a read-modify-write must carry them through
// untouched even though nothing here decodes them.
func TestRFChPreservesDMRTail(t *testing.T) {
	dmr := append(append([]byte{}, goldenAPRSCh...), 0xAB, 0xCD)
	c, err := ParseRFCh(dmr)
	if err != nil {
		t.Fatalf("ParseRFCh(dmr): %v", err)
	}
	got := c.WithFreq(145670000).Bytes()
	if len(got) != len(dmr) {
		t.Fatalf("length = %d, want %d", len(got), len(dmr))
	}
	if got[25] != 0xAB || got[26] != 0xCD {
		t.Errorf("DMR tail = % x, want ab cd", got[25:])
	}
}
