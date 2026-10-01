package benshi

import "errors"

// bssMinLen is how many record bytes must be present to read the APRS enable
// bit. A UV-PRO sends 46; requiring only what is read keeps a shorter record
// from another model working.
const bssMinLen = 2

// aprsEnableBit is the APRS/position-beacon enable, measured 2026-10-01 against
// the radio's "Digital Mode -> Enable" toggle: bit 0x10 of record byte 1,
// confirmed across two transitions in both directions with every other byte of
// every readable record unchanged.
//
// The setting is NOT in READ_SETTINGS -- that record is byte-identical with APRS
// on and off, which ruled out auto_share_loc_ch, gpwpl_upload_en and
// positioning_system in turn. SET_APRS_PATH is also not it; that sets the
// digipeater path.
const aprsEnableBit = 0x10

// ErrShortBSS reports a BSS record too short to decode.
var ErrShortBSS = errors.New("benshi: BSS record too short")

// BSSRec is the radio's position/beacon settings record, held as the exact
// bytes the radio sent.
//
// Read-modify-write matters more here than anywhere else: the record carries
// the operator's APRS callsign and symbol, so rebuilding it from a partial
// model would destroy their APRS identity rather than merely a preference. A
// round trip on real hardware confirmed the tail ("/[KU0HN") survives
// byte-identical.
type BSSRec struct {
	raw []byte
}

// ParseBSSRec takes a READ_BSS_SETTINGS reply body, including its leading
// reply-status byte, and keeps the record that follows.
func ParseBSSRec(body []byte) (BSSRec, error) {
	if len(body) < 1 {
		return BSSRec{}, ErrShortBSS
	}
	if body[0] != 0 {
		return BSSRec{}, errRejected("READ_BSS_SETTINGS", body[0])
	}
	if len(body)-1 < bssMinLen {
		return BSSRec{}, ErrShortBSS
	}
	raw := make([]byte, len(body)-1)
	copy(raw, body[1:])
	return BSSRec{raw: raw}, nil
}

// APRSEnabled reports whether position beaconing is on.
func (b BSSRec) APRSEnabled() bool { return b.raw[1]&aprsEnableBit != 0 }

// WithAPRSEnabled returns a copy with the enable bit set or cleared, preserving
// every other byte including the callsign and symbol.
func (b BSSRec) WithAPRSEnabled(on bool) BSSRec {
	raw := make([]byte, len(b.raw))
	copy(raw, b.raw)
	if on {
		raw[1] |= aprsEnableBit
	} else {
		raw[1] &^= aprsEnableBit
	}
	return BSSRec{raw: raw}
}

// Bytes is the record as it goes back on the wire. WRITE_BSS_SETTINGS's body is
// the record verbatim.
func (b BSSRec) Bytes() []byte {
	out := make([]byte, len(b.raw))
	copy(out, b.raw)
	return out
}

// Equal reports whether two records are byte-identical.
func (b BSSRec) Equal(o BSSRec) bool {
	if len(b.raw) != len(o.raw) {
		return false
	}
	for i := range b.raw {
		if b.raw[i] != o.raw[i] {
			return false
		}
	}
	return true
}
