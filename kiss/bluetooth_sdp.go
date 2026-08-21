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
// ServiceSearchAttributeResponse. Rather than fully walk the data-element tree,
// it locates the RFCOMM protocol descriptor — the UUID16 0x0003 encoded as
// 0x19 0x00 0x03 — and reads the uint8 channel element (0x08 <ch>) that follows.
func parseRFCOMMChannel(resp []byte) (int, error) {
	if len(resp) < 5 || resp[0] != sdpSSAResp {
		return 0, fmt.Errorf("sdp: unexpected response (pdu 0x%02x, %d bytes)", firstByte(resp), len(resp))
	}
	for i := 0; i+4 < len(resp); i++ {
		if resp[i] == 0x19 && resp[i+1] == byte(uuidRFCOMM16>>8) && resp[i+2] == byte(uuidRFCOMM16&0xFF) {
			if resp[i+3] == 0x08 { // uint8 element carrying the channel
				return int(resp[i+4]), nil
			}
		}
	}
	return 0, fmt.Errorf("sdp: no RFCOMM channel in SPP record (device may not advertise SPP)")
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
