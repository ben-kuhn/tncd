package kiss

// Pure SDP/address encoding+parsing for the FreeBSD Bluetooth transport, kept
// platform-neutral (no build tag) so the byte-layout-sensitive logic — the part
// most likely to harbor a marshaling bug — is unit-testable in CI on any OS.
// The socket code that uses these lives in bluetooth_freebsd.go.

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"
)

const (
	sdpSSAReq         = 0x06   // ServiceSearchAttributeRequest PDU
	sdpSSAResp        = 0x07   // ServiceSearchAttributeResponse PDU
	uuidSPP16         = 0x1101 // Serial Port Profile service class
	uuidRFCOMM16      = 0x0003 // RFCOMM protocol UUID
	attrProtoDescList = 0x0004 // ProtocolDescriptorList attribute
)

// parseBTAddrLE parses "AA:BB:CC:DD:EE:FF" into a 6-byte BD_ADDR in FreeBSD's
// little-endian bdaddr_t order: the leading octet (AA) is most-significant and
// lands in the last byte.
func parseBTAddrLE(s string) ([6]byte, error) {
	var a [6]byte
	h := strings.ReplaceAll(strings.ReplaceAll(s, ":", ""), "-", "")
	if len(h) != 12 {
		return a, fmt.Errorf("invalid Bluetooth address %q (want AA:BB:CC:DD:EE:FF)", s)
	}
	for i := 0; i < 6; i++ {
		b, err := strconv.ParseUint(h[i*2:i*2+2], 16, 8)
		if err != nil {
			return a, fmt.Errorf("invalid Bluetooth address %q: %w", s, err)
		}
		a[5-i] = byte(b)
	}
	return a, nil
}

// buildSSAReq builds an SDP ServiceSearchAttributeRequest for the SPP service
// class (0x1101), requesting the ProtocolDescriptorList attribute (0x0004).
func buildSSAReq() []byte {
	// ServiceSearchPattern: DES(one UUID16 = SPP)  ->  0x35 len | 0x19 uuid16
	ssp := []byte{0x35, 0x03, 0x19, byte(uuidSPP16 >> 8), byte(uuidSPP16 & 0xFF)}
	// AttributeIDList: DES(one uint16 = ProtocolDescriptorList)  ->  0x35 len | 0x09 attr16
	aidl := []byte{0x35, 0x03, 0x09, byte(attrProtoDescList >> 8), byte(attrProtoDescList & 0xFF)}

	var params []byte
	params = append(params, ssp...)
	params = append(params, 0xFF, 0xFF) // MaximumAttributeByteCount
	params = append(params, aidl...)
	params = append(params, 0x00) // ContinuationState (none)

	pdu := make([]byte, 5+len(params))
	pdu[0] = sdpSSAReq
	binary.BigEndian.PutUint16(pdu[1:3], 0x0001) // TransactionID
	binary.BigEndian.PutUint16(pdu[3:5], uint16(len(params)))
	copy(pdu[5:], params)
	return pdu
}

// parseRFCOMMChannel extracts the RFCOMM server channel from an SDP
// ServiceSearchAttributeResponse by walking the data-element tree (rather than
// scanning raw bytes, which was non-deterministic on devices advertising
// multiple SPP records). It descends into every sequence and returns the uint
// that immediately follows the RFCOMM protocol UUID (0x0003) — i.e. the channel
// of the first SPP record. For devices with several SPP services where a
// different one is wanted, pin the [client] "channel" key.
func parseRFCOMMChannel(resp []byte) (int, error) {
	// PDU header: id(1) txid(2) paramLen(2) attrListsByteCount(2), then the
	// AttributeLists data element (+ trailing continuation state).
	if len(resp) < 7 || resp[0] != sdpSSAResp {
		return 0, fmt.Errorf("sdp: unexpected response (pdu 0x%02x, %d bytes)", firstByte(resp), len(resp))
	}
	if ch, ok := findRFCOMMChannel(resp[7:]); ok {
		return ch, nil
	}
	return 0, fmt.Errorf("sdp: no RFCOMM channel in SPP record (device may not advertise SPP)")
}

// sdpElement parses the data-element header at b[off:], returning the element's
// type (high 5 bits of the descriptor), where its value starts, its value
// length, and whether parsing succeeded and fits within b.
func sdpElement(b []byte, off int) (etype byte, valOff, valLen int, ok bool) {
	if off >= len(b) {
		return 0, 0, 0, false
	}
	desc := b[off]
	etype = desc >> 3
	sizeIdx := desc & 0x07
	p := off + 1
	switch sizeIdx {
	case 0:
		valLen = 1
	case 1:
		valLen = 2
	case 2:
		valLen = 4
	case 3:
		valLen = 8
	case 4:
		valLen = 16
	case 5:
		if p >= len(b) {
			return 0, 0, 0, false
		}
		valLen = int(b[p])
		p++
	case 6:
		if p+2 > len(b) {
			return 0, 0, 0, false
		}
		valLen = int(binary.BigEndian.Uint16(b[p : p+2]))
		p += 2
	case 7:
		if p+4 > len(b) {
			return 0, 0, 0, false
		}
		valLen = int(binary.BigEndian.Uint32(b[p : p+4]))
		p += 4
	}
	if etype == 0 { // nil: no value
		valLen = 0
	}
	if p+valLen > len(b) {
		return 0, 0, 0, false
	}
	return etype, p, valLen, true
}

// findRFCOMMChannel walks the elements of one sequence level (recursing into
// nested sequences) and returns the uint element immediately following the
// RFCOMM protocol UUID (0x0003) — the RFCOMM server channel.
func findRFCOMMChannel(b []byte) (int, bool) {
	off := 0
	prevWasRFCOMM := false
	for off < len(b) {
		etype, vo, vl, ok := sdpElement(b, off)
		if !ok {
			return 0, false
		}
		val := b[vo : vo+vl]
		switch etype {
		case 3: // UUID
			prevWasRFCOMM = isRFCOMMUUID(val)
		case 1: // unsigned int
			if prevWasRFCOMM && vl >= 1 {
				return int(val[vl-1]), true // channel is the low byte
			}
			prevWasRFCOMM = false
		case 6, 7: // data element sequence / alternative — recurse
			if ch, ok := findRFCOMMChannel(val); ok {
				return ch, true
			}
			prevWasRFCOMM = false
		default:
			prevWasRFCOMM = false
		}
		off = vo + vl
	}
	return 0, false
}

// isRFCOMMUUID reports whether a UUID value is the RFCOMM protocol UUID (0x0003)
// in its 16-, 32-, or 128-bit form.
func isRFCOMMUUID(v []byte) bool {
	switch len(v) {
	case 2:
		return v[0] == byte(uuidRFCOMM16>>8) && v[1] == byte(uuidRFCOMM16&0xFF)
	case 4, 16:
		return v[0] == 0 && v[1] == 0 && v[2] == byte(uuidRFCOMM16>>8) && v[3] == byte(uuidRFCOMM16&0xFF)
	}
	return false
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
