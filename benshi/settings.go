package benshi

import (
	"errors"
	"fmt"
)

// DoubleChannel reports the radio's dual-watch setting: which of the two VFOs
// is active, or OFF for single-VFO operation.
type DoubleChannel uint8

const (
	DoubleChannelOff DoubleChannel = 0
	DoubleChannelA   DoubleChannel = 1
	DoubleChannelB   DoubleChannel = 2
)

// settingsMinLen is how many bytes of the Settings record must be present to
// read every field below. channel_b_upper ends at bit 79, so ten bytes.
// Real radios send considerably more (a UV-PRO sends 22); requiring only what
// is read keeps a shorter record from a different model working.
const settingsMinLen = 10

// ErrShortSettings reports a settings record too short to decode.
var ErrShortSettings = errors.New("benshi: settings record too short")

// Settings is the subset of the radio's settings record this package needs.
//
// The full record has fifty-odd fields; only the ones that answer "which
// channel record is the radio actually operating on" are decoded, because
// that is the question QSY has to get right. ChannelA and ChannelB are
// INDEXES into the channel table, not frequencies: each VFO points at a
// stored record, and the radio's "frequency mode" is simply a VFO pointed at
// an unnamed scratch record near the top of that table (252 on a UV-PRO, with
// 251 as its partner).
type Settings struct {
	ChannelA      byte
	ChannelB      byte
	DoubleChannel DoubleChannel
	// KISSEnabled mirrors the radio's own KISS TNC toggle.
	KISSEnabled bool
}

// ActiveChannel returns the channel index the radio transmits on, and whether
// that is unambiguous.
//
// With dual watch off, the radio operates on VFO A. With it on, which VFO
// carries a transmission depends on radio state this record does not capture,
// so this reports false rather than guessing -- writing the wrong VFO's record
// would retune a band the operator is still listening to.
func (s Settings) ActiveChannel() (byte, bool) {
	switch s.DoubleChannel {
	case DoubleChannelOff, DoubleChannelA:
		return s.ChannelA, true
	default:
		return 0, false
	}
}

// DecodeSettings parses a READ_SETTINGS reply body.
//
// The record is one big MSB-first bitfield, so the fields wanted here are not
// byte-aligned: each VFO's channel index is split into a low nibble near the
// start of the record and a high nibble 72 bits in, and the true index is
// upper<<4|lower. Reading only the low nibble silently wraps any channel >= 16
// to a different one -- which, for a UV-PRO whose VFO lives at 252, means
// every read lands on channel 12 instead.
func DecodeSettings(body []byte) (Settings, error) {
	if len(body) < 1 {
		return Settings{}, ErrShortSettings
	}
	if body[0] != 0 {
		return Settings{}, fmt.Errorf("%w (READ_SETTINGS status %d)", ErrRadioRejected, body[0])
	}
	b := body[1:]
	if len(b) < settingsMinLen {
		return Settings{}, ErrShortSettings
	}
	return Settings{
		ChannelA:      byte(bits(b, 72, 4))<<4 | byte(bits(b, 0, 4)),
		ChannelB:      byte(bits(b, 76, 4))<<4 | byte(bits(b, 4, 4)),
		DoubleChannel: DoubleChannel(bits(b, 10, 2)),
		KISSEnabled:   bits(b, 70, 1) != 0,
	}, nil
}

// bits reads n bits starting at bit offset off, MSB-first within each byte.
// Callers must have length-checked b; bits past the end read as zero rather
// than panicking.
func bits(b []byte, off, n int) uint32 {
	var v uint32
	for i := 0; i < n; i++ {
		idx := (off + i) / 8
		var bit uint32
		if idx < len(b) {
			bit = uint32(b[idx]>>(7-uint((off+i)%8))) & 1
		}
		v = v<<1 | bit
	}
	return v
}
