package agwpe

import (
	"bytes"
	"encoding/binary"
	"errors"
)

const HeaderSize = 36
const MaxPayload = 65536

type Header struct {
	Port     uint8
	Kind     byte
	PID      uint8
	CallFrom string // trailing NULs stripped
	CallTo   string
	DataLen  uint32
}

// ParseHeader parses a 36-byte AGWPE header and returns the Header struct.
// The wire format (little-endian) is:
// Port(1) pad(3) DataKind(1) pad(1) PID(1) pad(1) CallFrom(10) CallTo(10) DataLen(4) User(4)
func ParseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, errors.New("header too short")
	}

	h := Header{
		Port:    b[0],
		Kind:    b[4],
		PID:     b[6],
		DataLen: binary.LittleEndian.Uint32(b[28:32]),
	}

	// CallFrom (offset 8) and CallTo (offset 18) are 10-byte NUL-terminated
	// C strings; bytes after the first NUL are client garbage, not callsign.
	h.CallFrom = cString(b[8:18])
	h.CallTo = cString(b[18:28])

	return h, nil
}

// cString returns b up to (not including) its first NUL.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// Build produces a complete AGWPE frame (header + payload) with callsigns
// NUL-padded to 10 bytes each.
func Build(port uint8, kind byte, pid uint8, from, to string, data []byte) []byte {
	frame := make([]byte, HeaderSize+len(data))

	// Port at offset 0
	frame[0] = port

	// Padding at offset 1-3 (already zero)

	// DataKind at offset 4
	frame[4] = kind

	// Padding at offset 5 (already zero)

	// PID at offset 6
	frame[6] = pid

	// Padding at offset 7 (already zero)

	// CallFrom at offset 8-17 (10 bytes, NUL-padded)
	callFromPadded := make([]byte, 10)
	copy(callFromPadded, from)
	copy(frame[8:18], callFromPadded)

	// CallTo at offset 18-27 (10 bytes, NUL-padded)
	callToPadded := make([]byte, 10)
	copy(callToPadded, to)
	copy(frame[18:28], callToPadded)

	// DataLen at offset 28-31 (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(frame[28:32], uint32(len(data)))

	// User at offset 32-35 (4 bytes, already zero)

	// Payload
	copy(frame[HeaderSize:], data)

	return frame
}
