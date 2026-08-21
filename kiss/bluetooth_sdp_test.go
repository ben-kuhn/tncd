package kiss

import (
	"bytes"
	"testing"
)

func TestParseBTAddrLE(t *testing.T) {
	tests := []struct {
		in      string
		want    [6]byte
		wantErr bool
	}{
		// AA:BB:CC:DD:EE:FF -> little-endian bdaddr_t (AA is MSB, lands last).
		{"34:81:F4:AA:B3:D3", [6]byte{0xD3, 0xB3, 0xAA, 0xF4, 0x81, 0x34}, false},
		{"34-81-F4-AA-B3-D3", [6]byte{0xD3, 0xB3, 0xAA, 0xF4, 0x81, 0x34}, false}, // dashes
		{"3481f4aab3d3", [6]byte{0xD3, 0xB3, 0xAA, 0xF4, 0x81, 0x34}, false},      // no sep, lowercase
		{"00:00:00:00:00:01", [6]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00}, false},
		{"38:D2:00:01:52:8F", [6]byte{0x8F, 0x52, 0x01, 0x00, 0xD2, 0x38}, false},
		{"34:81:F4:AA:B3", [6]byte{}, true},        // too short
		{"34:81:F4:AA:B3:ZZ", [6]byte{}, true},     // bad hex
		{"", [6]byte{}, true},                      // empty
	}
	for _, tc := range tests {
		got, err := parseBTAddrLE(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseBTAddrLE(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("parseBTAddrLE(%q) = % x, want % x", tc.in, got, tc.want)
		}
	}
}

func TestBuildSSAReq(t *testing.T) {
	// Golden ServiceSearchAttributeRequest: SPP (0x1101), attr ProtocolDescriptorList (0x0004).
	want := []byte{
		0x06,       // PDU: ServiceSearchAttributeRequest
		0x00, 0x01, // TransactionID
		0x00, 0x0D, // ParamLength = 13
		0x35, 0x03, 0x19, 0x11, 0x01, // ServiceSearchPattern: DES{ UUID16 0x1101 }
		0xFF, 0xFF, // MaximumAttributeByteCount
		0x35, 0x03, 0x09, 0x00, 0x04, // AttributeIDList: DES{ uint16 0x0004 }
		0x00, // ContinuationState
	}
	got := buildSSAReq()
	if !bytes.Equal(got, want) {
		t.Errorf("buildSSAReq()\n got  = % x\n want = % x", got, want)
	}
}

func TestParseRFCOMMChannel(t *testing.T) {
	// Minimal SSA response carrying an RFCOMM descriptor (0x19 0x00 0x03) + uint8 channel.
	respCh6 := []byte{0x07, 0x00, 0x01, 0x00, 0x0A, 0x00, 0x07,
		0x19, 0x00, 0x03, 0x08, 0x06, 0x00, 0x00} // ...RFCOMM, channel 6
	respCh1 := []byte{0x07, 0x00, 0x01, 0x00, 0x0A, 0x00, 0x07,
		0x35, 0x03, 0x19, 0x01, 0x00, // L2CAP first (should be skipped)
		0x19, 0x00, 0x03, 0x08, 0x01} // RFCOMM, channel 1
	noRFCOMM := []byte{0x07, 0x00, 0x01, 0x00, 0x05, 0x19, 0x01, 0x00, 0x08, 0x09} // only L2CAP
	wrongPDU := []byte{0x06, 0x00, 0x01, 0x00, 0x00}
	tooShort := []byte{0x07}

	tests := []struct {
		name    string
		in      []byte
		want    int
		wantErr bool
	}{
		{"channel 6", respCh6, 6, false},
		{"channel 1 after L2CAP", respCh1, 1, false},
		{"no RFCOMM descriptor", noRFCOMM, 0, true},
		{"wrong PDU id", wrongPDU, 0, true},
		{"too short", tooShort, 0, true},
	}
	for _, tc := range tests {
		got, err := parseRFCOMMChannel(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
			continue
		}
		if err == nil && got != tc.want {
			t.Errorf("%s: channel=%d want %d", tc.name, got, tc.want)
		}
	}
}
