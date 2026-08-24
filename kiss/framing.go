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
// The value byte is escaped so that FEND/FESC values do not corrupt the stream.
func WrapCommand(kissPort uint8, cmd uint8, value uint8) []byte {
	var result []byte
	result = append(result, FEND)
	// cmd byte: port in high nibble, cmd in low nibble
	cmdByte := (kissPort << 4) | cmd
	result = append(result, cmdByte)
	// Escape the value byte (FEND/FESC would corrupt the stream).
	switch value {
	case FEND:
		result = append(result, FESC, TFEND)
	case FESC:
		result = append(result, FESC, TFESC)
	default:
		result = append(result, value)
	}
	result = append(result, FEND)
	return result
}

// WrapCommandBytes builds a KISS command frame with a multi-byte value (e.g.
// SetHardware, cmd 6). cmd goes in the low nibble, kissPort in the high nibble;
// every value byte is escaped. WrapCommand remains the single-value convenience.
func WrapCommandBytes(kissPort uint8, cmd uint8, value []byte) []byte {
	result := []byte{FEND, (kissPort << 4) | cmd}
	for _, b := range value {
		switch b {
		case FEND:
			result = append(result, FESC, TFEND)
		case FESC:
			result = append(result, FESC, TFESC)
		default:
			result = append(result, b)
		}
	}
	return append(result, FEND)
}

// ExitFrame is the standard KISS exit sequence C0 FF C0.
func ExitFrame() []byte {
	return []byte{0xC0, 0xFF, 0xC0}
}

// MaxFrameSize is the maximum KISS frame payload the Decoder will buffer
// (cmd byte included). The largest legitimate AX.25 frame — 8 digipeaters,
// mod-128 control, 256-byte info — is ~350 bytes; 8 KiB leaves generous
// headroom for KISS extensions. A peer that never terminates a frame
// (or streams garbage) cannot grow the buffer past this cap: the partial
// frame is dropped and the decoder resyncs on the next FEND.
const MaxFrameSize = 8192

// Decoder incrementally decodes a KISS byte stream into frames.
// Feed returns zero or more complete frames (cmd byte + payload,
// FENDs stripped, escapes resolved). Empty frames are dropped.
type Decoder struct {
	buf     []byte
	inFrame bool
	esc     bool

	// DroppedOversize counts frames discarded for exceeding MaxFrameSize.
	// Callers should diff it against a last-seen value and log the delta.
	DroppedOversize uint64
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
			// Previous byte was FESC; resolve the escaped byte and fall
			// through to the append below.
			d.esc = false
			if b == TFEND {
				b = FEND
			} else if b == TFESC {
				b = FESC
			}
			// else: invalid escape sequence, append the byte as-is.
		} else if b == FESC {
			// Start of escape sequence
			d.esc = true
			continue
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
			continue
		}

		// Append one data byte, enforcing the size cap. On overflow, drop the
		// partial frame and leave in-frame state: subsequent bytes are
		// discarded as out-of-frame noise until the next FEND resyncs.
		if len(d.buf) >= MaxFrameSize {
			d.buf = nil
			d.inFrame = false
			d.esc = false
			d.DroppedOversize++
			continue
		}
		d.buf = append(d.buf, b)
	}

	return frames
}
