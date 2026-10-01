package benshi

// SettingsRec is the radio's settings record, held as the exact bytes the radio
// sent.
//
// Same reasoning as RFCh: the record carries fifty-odd fields -- squelch, mic
// gain, VOX, screen timeout, every button mapping -- and this package models
// four of them. Re-serialising from that partial model would zero everything it
// failed to represent, which on a settings record means wiping an operator's
// whole configuration. Patching bits in place cannot lose a field it does not
// know about.
//
// Measured 2026-10-01: WRITE_SETTINGS accepts the full record, an identity
// write leaves it byte-identical, and the result reaches NVRAM without
// STORE_SETTINGS. There is no undo but another write.
type SettingsRec struct {
	raw []byte
}

// ParseSettingsRec takes a READ_SETTINGS reply body, including its leading
// reply-status byte, and keeps the record that follows.
func ParseSettingsRec(body []byte) (SettingsRec, error) {
	// Validate through the decoder so a rejected or truncated reply is caught
	// here rather than by a later patch silently writing past the record.
	if _, err := DecodeSettings(body); err != nil {
		return SettingsRec{}, err
	}
	raw := make([]byte, len(body)-1)
	copy(raw, body[1:])
	return SettingsRec{raw: raw}, nil
}

// Settings decodes the fields this package models.
func (s SettingsRec) Settings() Settings {
	// DecodeSettings expects the status byte, and the record is known-good
	// because ParseSettingsRec decoded it already.
	set, _ := DecodeSettings(append([]byte{0x00}, s.raw...))
	return set
}

// WithDoubleChannel returns a copy with dual watch set to dc, preserving every
// other field. dc also carries which side is selected, so restoring a saved
// value restores the selection too.
func (s SettingsRec) WithDoubleChannel(dc DoubleChannel) SettingsRec {
	out := s.clone()
	putBits(out.raw, 10, 2, uint32(dc))
	return out
}

// WithChannelA returns a copy with the A VFO pointed at channel id.
//
// The index is split across the record -- low nibble at bit 0, high nibble at
// bit 72 -- so this writes both halves. Writing only the low nibble would wrap
// any channel >= 16, which for a UV-PRO's VFO at 252 means landing on channel
// 12 instead.
func (s SettingsRec) WithChannelA(id byte) SettingsRec {
	out := s.clone()
	putBits(out.raw, 0, 4, uint32(id&0x0F))
	putBits(out.raw, 72, 4, uint32(id>>4))
	return out
}

func (s SettingsRec) clone() SettingsRec {
	raw := make([]byte, len(s.raw))
	copy(raw, s.raw)
	return SettingsRec{raw: raw}
}

// Bytes is the record as it goes back on the wire. WRITE_SETTINGS's body is the
// record verbatim, so this needs no separate encoder.
func (s SettingsRec) Bytes() []byte {
	out := make([]byte, len(s.raw))
	copy(out, s.raw)
	return out
}

// Equal reports whether two records are byte-identical. Restore compares with
// this rather than field by field, so a field this package does not model still
// counts as a change.
func (s SettingsRec) Equal(o SettingsRec) bool {
	if len(s.raw) != len(o.raw) {
		return false
	}
	for i := range s.raw {
		if s.raw[i] != o.raw[i] {
			return false
		}
	}
	return true
}

// putBits writes the low n bits of v at bit offset off, MSB-first within each
// byte -- the inverse of bits. Offsets past the end are dropped rather than
// panicking, matching bits' tolerance of short records.
func putBits(b []byte, off, n int, v uint32) {
	for i := 0; i < n; i++ {
		idx := (off + i) / 8
		if idx >= len(b) {
			return
		}
		shift := 7 - uint((off+i)%8)
		bit := (v >> uint(n-1-i)) & 1
		b[idx] = b[idx]&^(1<<shift) | byte(bit)<<shift
	}
}
