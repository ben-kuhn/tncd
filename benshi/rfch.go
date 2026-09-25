package benshi

import (
	"encoding/binary"
	"errors"
	"strings"
)

// RFChLen is the wire size of the standard RfCh channel record:
//
//	channel_id(1) tx_word(4) rx_word(4) tx_sub(2) rx_sub(2) flags(2) name(10)
//
// Each frequency word packs modulation in the top 2 bits and the frequency in
// Hz in the low 30. A radio with DMR support answers with a longer record
// (RfChDMR appends colour codes and a slot); this package never decodes those
// extra fields but preserves them byte-for-byte, so a read-modify-write of a
// DMR channel does not silently drop them.
const RFChLen = 25

// rfChNameOff is where the 10-byte NUL-padded name field starts. The DMR
// variant appends its extra fields AFTER the name, so this offset is the same
// for both record shapes.
const rfChNameOff = 15

// rfChNameLen is the fixed width of the name field.
const rfChNameLen = 10

// ErrShortRFCh reports a channel record too short to be an RfCh.
var ErrShortRFCh = errors.New("benshi: channel record shorter than 25 bytes")

// RFCh is one stored channel record, held as the exact bytes the radio sent.
//
// Keeping the raw record rather than an exploded struct is deliberate: a QSY
// is a read-modify-write of a record with a dozen fields this package has no
// reason to model (sub-audio, bandwidth, power flags, pre-emphasis bypass, the
// DMR tail). Re-serialising from a partial model would quietly zero whatever
// it failed to represent -- on a VFO record that is merely rude, but the same
// code path can be pointed at a real memory channel, where it would be
// destructive. Patching bytes in place cannot lose a field it does not know
// about.
type RFCh struct {
	raw []byte
}

// ParseRFCh takes the channel record bytes -- the READ_RF_CH reply body with
// its leading reply-status byte already stripped.
func ParseRFCh(b []byte) (RFCh, error) {
	if len(b) < RFChLen {
		return RFCh{}, ErrShortRFCh
	}
	raw := make([]byte, len(b))
	copy(raw, b)
	return RFCh{raw: raw}, nil
}

// ID is the record's own channel id.
func (c RFCh) ID() byte { return c.raw[0] }

// TXFreqHz is the transmit frequency with the modulation bits masked off.
func (c RFCh) TXFreqHz() uint32 {
	return binary.BigEndian.Uint32(c.raw[1:5]) & freqMask
}

// RXFreqHz is the receive frequency with the modulation bits masked off.
func (c RFCh) RXFreqHz() uint32 {
	return binary.BigEndian.Uint32(c.raw[5:9]) & freqMask
}

// Name is the record's name with NUL padding removed.
//
// An empty name is what distinguishes a VFO scratch record from a memory the
// operator programmed, and is the guard internal/rig uses before writing.
func (c RFCh) Name() string {
	return strings.TrimRight(string(c.raw[rfChNameOff:rfChNameOff+rfChNameLen]), "\x00")
}

// WithFreq returns a copy tuned to hz simplex, preserving every other field
// including both modulation settings.
func (c RFCh) WithFreq(hz uint32) RFCh {
	raw := make([]byte, len(c.raw))
	copy(raw, c.raw)
	putFreq(raw[1:5], hz)
	putFreq(raw[5:9], hz)
	return RFCh{raw: raw}
}

// putFreq writes hz into a frequency word without disturbing the modulation
// bits already in the top 2.
func putFreq(w []byte, hz uint32) {
	mod := w[0] & 0xC0
	binary.BigEndian.PutUint32(w, hz&freqMask)
	w[0] = (w[0] & 0x3F) | mod
}

// Bytes is the record as it goes back on the wire. WRITE_RF_CH's body is the
// record verbatim -- the same layout READ_RF_CH replies with, minus the
// reply-status byte -- so this needs no separate encoder.
func (c RFCh) Bytes() []byte {
	out := make([]byte, len(c.raw))
	copy(out, c.raw)
	return out
}
