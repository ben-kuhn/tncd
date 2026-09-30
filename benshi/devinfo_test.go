package benshi

import (
	"errors"
	"testing"
)

// goldenDevInfo is a GET_DEV_INFO reply captured from a BTech UV-PRO on
// 2026-09-29 (status byte plus the 10-byte DevInfo payload).
var goldenDevInfo = []byte{
	0x00, 0x06, 0x01, 0x04, 0x01, 0x00, 0x92, 0xd0, 0x68, 0x1e, 0x54,
}

func TestDecodeDevInfoGolden(t *testing.T) {
	d, err := DecodeDevInfo(goldenDevInfo)
	if err != nil {
		t.Fatalf("DecodeDevInfo: %v", err)
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"VendorID", int(d.VendorID), 6},
		{"ProductID", int(d.ProductID), 260},
		{"HWVer", int(d.HWVer), 1},
		{"SoftVer", int(d.SoftVer), 146},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if s := d.String(); s == "" {
		t.Error("String() is empty")
	}
}

func TestDecodeDevInfoRejects(t *testing.T) {
	if _, err := DecodeDevInfo(nil); !errors.Is(err, ErrShortBody) {
		t.Errorf("nil body: err = %v, want ErrShortBody", err)
	}
	if _, err := DecodeDevInfo([]byte{0x05, 1, 2, 3}); !errors.Is(err, ErrRadioRejected) {
		t.Errorf("failure status: err = %v, want ErrRadioRejected", err)
	}
	if _, err := DecodeDevInfo(goldenDevInfo[:5]); !errors.Is(err, ErrShortBody) {
		t.Errorf("truncated: err = %v, want ErrShortBody", err)
	}
}
