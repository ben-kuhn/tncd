package kiss

const (
	FEND  = 0xC0
	FESC  = 0xDB
	TFEND = 0xDC
	TFESC = 0xDD
)

// WrapData builds a complete KISS data frame: FEND, cmd byte
// (kissPort<<4 | 0x00), escaped payload, FEND.
func WrapData(kissPort uint8, ax25Frame []byte) []byte {
	var result []byte
	result = append(result, FEND)
	// cmd byte: port in high nibble, 0x00 for data frame
	cmdByte := (kissPort << 4) | 0x00
	result = append(result, cmdByte)

	// escape payload
	for _, b := range ax25Frame {
		if b == FEND {
			result = append(result, FESC, TFEND)
		} else if b == FESC {
			result = append(result, FESC, TFESC)
		} else {
			result = append(result, b)
		}
	}

	result = append(result, FEND)
	return result
}

// WrapCommand builds a KISS TNC-parameter frame (cmd 1-6), e.g.
// WrapCommand(0, 0x01, 40) for TXDELAY=40 on port 0.
func WrapCommand(kissPort uint8, cmd uint8, value uint8) []byte {
	var result []byte
	result = append(result, FEND)
	// cmd byte: port in high nibble, cmd in low nibble
	cmdByte := (kissPort << 4) | cmd
	result = append(result, cmdByte)
	result = append(result, value)
	result = append(result, FEND)
	return result
}

// ExitFrame is the standard KISS exit sequence C0 FF C0.
func ExitFrame() []byte {
	return []byte{0xC0, 0xFF, 0xC0}
}

// Decoder incrementally decodes a KISS byte stream into frames.
// Feed returns zero or more complete frames (cmd byte + payload,
// FENDs stripped, escapes resolved). Empty frames are dropped.
type Decoder struct {
	buf     []byte
	inFrame bool
	esc     bool
}

// Feed processes a slice of bytes and returns any complete frames found.
func (d *Decoder) Feed(p []byte) [][]byte {
	var frames [][]byte

	for _, b := range p {
		if !d.inFrame {
			// Not in a frame yet
			if b == FEND {
				// Start of a frame
				d.inFrame = true
				d.buf = nil
				d.esc = false
			}
			// else: discard bytes before first FEND
			continue
		}

		// We're in a frame
		if d.esc {
			// Previous byte was FESC, this is the escaped byte
			if b == TFEND {
				d.buf = append(d.buf, FEND)
			} else if b == TFESC {
				d.buf = append(d.buf, FESC)
			} else {
				// Invalid escape sequence, just append the byte
				d.buf = append(d.buf, b)
			}
			d.esc = false
		} else if b == FESC {
			// Start of escape sequence
			d.esc = true
		} else if b == FEND {
			// End of (and simultaneously start of next) frame.
			// A FEND while in-frame closes the current frame and immediately
			// begins the next one — single-FEND delimiter semantics (kiss3).
			if len(d.buf) > 0 {
				frames = append(frames, append([]byte{}, d.buf...))
			}
			d.buf = nil
			// Stay inFrame: this FEND is both the closer and the opener.
			// (Double-FEND just produces an empty buf which we drop.)
		} else {
			// Regular data byte
			d.buf = append(d.buf, b)
		}
	}

	return frames
}
