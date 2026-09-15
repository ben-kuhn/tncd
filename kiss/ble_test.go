package kiss

import (
	"bytes"
	"testing"
)

// TestChunkForMTU covers splitting writes to fit a single ATT operation. The
// BLE KISS spec allows a KISS frame to span several transfer units, so the
// only hard requirements are that no chunk exceeds the usable payload and that
// concatenating the chunks reproduces the input byte for byte.
func TestChunkForMTU(t *testing.T) {
	cases := []struct {
		name       string
		size       int
		mtu        int
		wantChunks int
	}{
		{"empty input sends nothing", 0, 155, 0},
		{"short frame fits one write", 20, 155, 1},
		{"exactly the usable payload", 152, 155, 1},
		{"one byte over splits in two", 153, 155, 2},
		{"max AX.25 frame at default MTU", 329, bleDefaultMTU, 17},
		{"worst-case KISS-escaped buffer", 660, 155, 5},
	}
	for _, tc := range cases {
		data := make([]byte, tc.size)
		for i := range data {
			data[i] = byte(i % 251)
		}
		chunks := chunkForMTU(data, tc.mtu)
		if len(chunks) != tc.wantChunks {
			t.Errorf("%s: got %d chunks, want %d", tc.name, len(chunks), tc.wantChunks)
			continue
		}
		max := tc.mtu - bleATTOverhead
		var joined []byte
		for i, c := range chunks {
			if len(c) > max {
				t.Errorf("%s: chunk %d is %d bytes, exceeds usable payload %d",
					tc.name, i, len(c), max)
			}
			if len(c) == 0 {
				t.Errorf("%s: chunk %d is empty", tc.name, i)
			}
			joined = append(joined, c...)
		}
		if !bytes.Equal(joined, data) {
			t.Errorf("%s: rejoined chunks do not match the input", tc.name)
		}
	}
}

// TestChunkForMTUClampsBogusMTU: a stack that reports a nonsensical MTU must
// not produce zero-length or negative-length writes. Fall back to the ATT
// default, which every LE link supports.
func TestChunkForMTUClampsBogusMTU(t *testing.T) {
	data := make([]byte, 100)
	for _, mtu := range []int{0, -1, 3, 22} {
		chunks := chunkForMTU(data, mtu)
		if len(chunks) == 0 {
			t.Fatalf("mtu=%d produced no chunks for a non-empty write", mtu)
		}
		want := bleDefaultMTU - bleATTOverhead
		for i, c := range chunks {
			if len(c) > want || len(c) == 0 {
				t.Fatalf("mtu=%d chunk %d has length %d, want 1..%d", mtu, i, len(c), want)
			}
		}
	}
}
