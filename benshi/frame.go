// Package benshi implements the Benshi radio control protocol used by the
// BTech UV-Pro, RadioOddity GA-5WB and Vero VR-N76/VR-N7500.
//
// Wire details were derived from two open reimplementations:
//   - benlink (Apache-2.0) — https://github.com/khusmann/benlink
//   - HTCommander — https://github.com/Ylianst/HTCommander
package benshi

import "errors"

// Frame flags. Only CHECKSUM is known to be used.
type Flags uint8

const (
	FlagNone     Flags = 0
	FlagChecksum Flags = 1
)

const (
	frameStart   = 0xFF
	frameVersion = 0x01
	// frameHeaderLen is start + version + flags + length.
	frameHeaderLen = 4
	// msgHeaderLen is the message header (group u16 + reply/command u16) that
	// the frame's length byte does NOT count.
	msgHeaderLen = 4
	// maxFrameData bounds a frame body: the length byte is 8 bits, so the data
	// field can never exceed 255 + msgHeaderLen.
	maxFrameData = 255 + msgHeaderLen
)

// ErrShortFrame reports a frame whose declared length is impossible.
var ErrShortFrame = errors.New("benshi: frame data shorter than message header")

// Frame is one GaiaFrame: FF 01 <flags> <n> <data> [checksum].
//
// Data holds the complete message (its 4-byte header plus body). The wire
// length byte counts only what follows that header, which is why Bytes
// subtracts msgHeaderLen.
type Frame struct {
	Flags Flags
	Data  []byte
}

// Bytes serializes the frame. A frame whose Data is shorter than a message
// header cannot be represented and returns nil.
func (f Frame) Bytes() []byte {
	if len(f.Data) < msgHeaderLen || len(f.Data) > maxFrameData {
		return nil
	}
	out := make([]byte, 0, frameHeaderLen+len(f.Data))
	out = append(out, frameStart, frameVersion, byte(f.Flags), byte(len(f.Data)-msgHeaderLen))
	return append(out, f.Data...)
}

// Decoder reassembles frames from a byte stream. Both transports deliver
// partial frames -- BLE splits on ATT MTU, RFCOMM on socket reads -- so the
// decoder buffers until a whole frame is present.
type Decoder struct {
	buf []byte
}

// NewDecoder returns a decoder with an empty buffer.
func NewDecoder() *Decoder { return &Decoder{} }

// Feed appends p and returns every complete frame now available.
//
// Bytes that cannot begin a valid frame are skipped rather than returned as an
// error: a relink or a mid-frame disconnect can leave us holding a fragment,
// and resynchronizing on the next start byte recovers without dropping the
// link.
func (d *Decoder) Feed(p []byte) ([]Frame, error) {
	d.buf = append(d.buf, p...)
	var out []Frame
	for {
		// Resync: discard anything before a plausible start.
		for len(d.buf) > 0 && d.buf[0] != frameStart {
			d.buf = d.buf[1:]
		}
		if len(d.buf) < frameHeaderLen {
			return out, nil
		}
		if d.buf[1] != frameVersion {
			d.buf = d.buf[1:] // false start byte; resync past it
			continue
		}
		flags := Flags(d.buf[2])
		dataLen := int(d.buf[3]) + msgHeaderLen
		total := frameHeaderLen + dataLen
		if flags&FlagChecksum != 0 {
			total++
		}
		if len(d.buf) < total {
			return out, nil // incomplete; wait for more
		}
		data := make([]byte, dataLen)
		copy(data, d.buf[frameHeaderLen:frameHeaderLen+dataLen])
		out = append(out, Frame{Flags: flags, Data: data})
		d.buf = d.buf[total:]
	}
}
