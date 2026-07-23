package ax25

import (
	"fmt"
	"strconv"
	"strings"
)

// Address represents an AX.25 address (callsign + SSID).
type Address struct {
	Call string // base callsign, uppercase, no SSID suffix
	SSID uint8  // 0-15
	CRH  bool   // C/R bit (dst/src) or H "has-been-repeated" bit (via)
}

// ParseAddress parses a callsign string like "KU0HN-10", "CQ", or "WIDE1-1*".
// A trailing '*' sets the CRH bit. Normalises to uppercase.
func ParseAddress(s string) (Address, error) {
	if len(s) == 0 {
		return Address{}, fmt.Errorf("ax25: empty address")
	}

	crh := false
	if strings.HasSuffix(s, "*") {
		crh = true
		s = s[:len(s)-1]
	}

	s = strings.ToUpper(s)

	var call string
	var ssid uint8

	if idx := strings.LastIndex(s, "-"); idx >= 0 {
		call = s[:idx]
		suffix := s[idx+1:]
		n, err := strconv.ParseUint(suffix, 10, 8)
		if err != nil || n > 15 {
			return Address{}, fmt.Errorf("ax25: invalid SSID %q", suffix)
		}
		ssid = uint8(n)
	} else {
		call = s
		ssid = 0
	}

	if len(call) == 0 || len(call) > 6 {
		return Address{}, fmt.Errorf("ax25: callsign %q must be 1-6 characters", call)
	}

	return Address{Call: call, SSID: ssid, CRH: crh}, nil
}

// String returns the address as a human-readable string, e.g. "KU0HN-10" or "CQ".
// SSID 0 is omitted; CRH is not included.
func (a Address) String() string {
	if a.SSID == 0 {
		return a.Call
	}
	return fmt.Sprintf("%s-%d", a.Call, a.SSID)
}

// encodeAddress encodes the address into 7 bytes in AX.25 wire format.
// crh is the C/R or H bit; ext is the extension bit (1 = last address).
func (a Address) encode(crh bool, ext bool) [7]byte {
	var b [7]byte
	// Callsign: each char shifted left 1, space-padded to 6 bytes
	padded := a.Call
	for len(padded) < 6 {
		padded += " "
	}
	for i := 0; i < 6; i++ {
		b[i] = padded[i] << 1
	}
	// SSID byte: crhBit<<7 | 0x60 | ssid<<1 | extBit.
	// Mask SSID to 4 bits: a directly-constructed Address with SSID>15 must not
	// corrupt the CRH/reserved bits. (Deliberate divergence from tncd.py, which
	// does not mask; ParseAddress already rejects >15 for parsed callsigns.)
	crhBit := uint8(0)
	if crh {
		crhBit = 1
	}
	extBit := uint8(0)
	if ext {
		extBit = 1
	}
	b[6] = crhBit<<7 | 0x60 | (a.SSID&0x0F)<<1 | extBit
	return b
}

// decodeAddress reads a 7-byte AX.25 address from b.
// Returns the Address, plus the extension bit (true = last address in the list).
func decodeAddress(b []byte) (Address, bool) {
	call := make([]byte, 6)
	for i := 0; i < 6; i++ {
		call[i] = b[i] >> 1
	}
	callStr := strings.TrimRight(string(call), " ")

	ssidByte := b[6]
	crh := (ssidByte & 0x80) != 0
	ssid := (ssidByte >> 1) & 0x0F
	ext := (ssidByte & 0x01) != 0

	return Address{Call: callStr, SSID: ssid, CRH: crh}, ext
}
