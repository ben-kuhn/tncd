package ax25

import (
	"fmt"
)

// FrameType identifies the AX.25 frame type.
type FrameType int

const (
	UnknownType FrameType = iota
	I
	RR
	RNR
	REJ
	SREJ
	UI
	SABM
	SABME
	UA
	DM
	DISC
	FRMR
)

// IsI returns true for I-frames.
func (t FrameType) IsI() bool { return t == I }

// IsS returns true for supervisory frames (RR, RNR, REJ, SREJ).
func (t FrameType) IsS() bool { return t == RR || t == RNR || t == REJ || t == SREJ }

// IsU returns true for unnumbered frames.
func (t FrameType) IsU() bool {
	switch t {
	case UI, SABM, SABME, UA, DM, DISC, FRMR:
		return true
	}
	return false
}

// String returns the frame type name.
func (t FrameType) String() string {
	switch t {
	case I:
		return "I"
	case RR:
		return "RR"
	case RNR:
		return "RNR"
	case REJ:
		return "REJ"
	case SREJ:
		return "SREJ"
	case UI:
		return "UI"
	case SABM:
		return "SABM"
	case SABME:
		return "SABME"
	case UA:
		return "UA"
	case DM:
		return "DM"
	case DISC:
		return "DISC"
	case FRMR:
		return "FRMR"
	}
	return "UNKNOWN"
}

// U-frame control byte values (with PF bit masked out, i.e. & ^0x10).
const (
	uiBase    = 0x03
	sabmBase  = 0x2F
	sabmeBase = 0x6F
	discBase  = 0x43
	dmBase    = 0x0F
	uaBase    = 0x63
	frmrBase  = 0x87
)

// Frame represents a parsed AX.25 frame.
type Frame struct {
	Dst, Src Address
	Via      []Address
	Type     FrameType
	NR, NS   uint8 // mod-8; NS only for I, NR for I and S frames
	PF       bool
	Modulo   uint8 // 8 (default/mod-8) or 128 (mod-128 extended I/S control)
	Command  bool  // true = command (dst C-bit set), false = response
	PID      uint8  // I and UI frames only
	Info     []byte // I and UI frames only
}

// Parse decodes a raw AX.25 frame assuming mod-8 (1-byte I/S control).
// For established mod-128 links, callers use ParseModulo(raw, 128).
func Parse(raw []byte) (*Frame, error) { return ParseModulo(raw, 8) }

// ParseModulo decodes a raw AX.25 frame. modulo selects the I/S control-field
// width: 8 → 1 byte (classic), 128 → 2 bytes (extended). U-frames are
// modulo-independent. Addresses and frame-type classification are always the
// same regardless of modulo.
func ParseModulo(raw []byte, modulo int) (*Frame, error) {
	if len(raw) < 15 {
		// Minimum: 7 (dst) + 7 (src) + 1 (control) = 15 bytes
		return nil, fmt.Errorf("ax25: frame too short (%d bytes)", len(raw))
	}

	pos := 0

	// Parse destination address
	dst, dstExt := decodeAddress(raw[pos : pos+7])
	pos += 7
	if dstExt {
		return nil, fmt.Errorf("ax25: ext bit set on dst address")
	}

	// Parse source address
	if pos+7 > len(raw) {
		return nil, fmt.Errorf("ax25: frame too short for src address")
	}
	src, srcExt := decodeAddress(raw[pos : pos+7])
	pos += 7

	// Parse via addresses (repeaters)
	var via []Address
	if !srcExt {
		for {
			if pos+7 > len(raw) {
				return nil, fmt.Errorf("ax25: frame too short for via address")
			}
			addr, ext := decodeAddress(raw[pos : pos+7])
			pos += 7
			via = append(via, addr)
			if ext {
				break
			}
		}
	}

	// Need at least 1 byte for control
	if pos >= len(raw) {
		return nil, fmt.Errorf("ax25: frame too short for control byte")
	}

	ctl := raw[pos]
	// Note: pos++ is NOT done here; each branch advances pos by the correct amount.

	f := &Frame{
		Dst:    dst,
		Src:    src,
		Via:    via,
		Modulo: uint8(modulo),
	}

	// Derive Command from C/R bits
	// dst.CRH && !src.CRH -> command; !dst.CRH && src.CRH -> response
	// both equal (old v1 frames) -> treat as command
	f.Command = dst.CRH || !src.CRH

	// Decode frame type from control byte
	if ctl&0x01 == 0 {
		// I-frame
		f.Type = I
		if modulo == 128 {
			if pos+1 >= len(raw) {
				return nil, fmt.Errorf("ax25: extended I-frame too short for control")
			}
			ctl2 := raw[pos+1]
			f.NS = (ctl >> 1) & 0x7F
			f.PF = ctl2&0x01 != 0
			f.NR = (ctl2 >> 1) & 0x7F
			pos += 2
		} else {
			f.NS = (ctl >> 1) & 0x07
			f.PF = ctl&0x10 != 0
			f.NR = (ctl >> 5) & 0x07
			pos++
		}
		if pos >= len(raw) {
			return nil, fmt.Errorf("ax25: I-frame too short for PID")
		}
		f.PID = raw[pos]
		pos++
		f.Info = raw[pos:]
	} else if ctl&0x03 == 0x01 {
		// S-frame
		var sBits byte
		if modulo == 128 {
			if pos+1 >= len(raw) {
				return nil, fmt.Errorf("ax25: extended S-frame too short for control")
			}
			ctl2 := raw[pos+1]
			sBits = (ctl >> 2) & 0x03
			f.PF = ctl2&0x01 != 0
			f.NR = (ctl2 >> 1) & 0x7F
			pos += 2
		} else {
			f.PF = ctl&0x10 != 0
			f.NR = (ctl >> 5) & 0x07
			sBits = (ctl >> 2) & 0x03
			pos++
		}
		switch sBits {
		case 0:
			f.Type = RR
		case 1:
			f.Type = RNR
		case 2:
			f.Type = REJ
		case 3:
			f.Type = SREJ
		}
	} else {
		// U-frame: bits 1:0 = 11
		pos++
		f.PF = (ctl & 0x10) != 0
		// Mask out PF bit to get base opcode
		base := ctl &^ uint8(0x10)
		switch base {
		case uiBase:
			f.Type = UI
		case sabmBase:
			f.Type = SABM
		case sabmeBase:
			f.Type = SABME
		case discBase:
			f.Type = DISC
		case dmBase:
			f.Type = DM
		case uaBase:
			f.Type = UA
		case frmrBase:
			f.Type = FRMR
		default:
			f.Type = UnknownType
		}
		// UI frames have PID + info
		if f.Type == UI {
			if pos >= len(raw) {
				return nil, fmt.Errorf("ax25: UI frame too short for PID")
			}
			f.PID = raw[pos]
			pos++
			f.Info = raw[pos:]
		}
	}

	return f, nil
}

// Bytes encodes the frame to AX.25 wire bytes.
func (f *Frame) Bytes() []byte {
	var buf []byte

	// Address list: dst, src, via...
	// The last address gets ext=1; all others get ext=0.
	// Command frame: dst CRH=1 (Command=true), src CRH=0 (Command=true).
	// Response frame: dst CRH=0, src CRH=1.
	// Via addresses: preserve Via[i].CRH as the H bit.

	numAddrs := 2 + len(f.Via)
	addrs := make([]Address, numAddrs)
	addrs[0] = Address{Call: f.Dst.Call, SSID: f.Dst.SSID, CRH: f.Command}
	addrs[1] = Address{Call: f.Src.Call, SSID: f.Src.SSID, CRH: !f.Command}
	for i, v := range f.Via {
		addrs[2+i] = v
	}

	for i, a := range addrs {
		ext := i == numAddrs-1
		encoded := a.encode(a.CRH, ext)
		buf = append(buf, encoded[:]...)
	}

	// Control byte(s)
	switch {
	case f.Type.IsI():
		if f.Modulo == 128 {
			buf = append(buf, f.NS<<1)                 // bit0=0, NS in 7..1
			buf = append(buf, f.NR<<1|boolBit(f.PF)) // NR in 7..1, PF bit0
		} else {
			buf = append(buf, f.NR<<5|boolBit(f.PF)<<4|f.NS<<1)
		}
	case f.Type.IsS():
		var sBits byte
		switch f.Type {
		case RR:
			sBits = 0
		case RNR:
			sBits = 1
		case REJ:
			sBits = 2
		case SREJ:
			sBits = 3
		}
		if f.Modulo == 128 {
			buf = append(buf, sBits<<2|0x01)
			buf = append(buf, f.NR<<1|boolBit(f.PF))
		} else {
			buf = append(buf, f.NR<<5|boolBit(f.PF)<<4|sBits<<2|0x01)
		}
	default:
		var base byte
		switch f.Type {
		case UI:
			base = uiBase
		case SABM:
			base = sabmBase
		case SABME:
			base = sabmeBase
		case DISC:
			base = discBase
		case DM:
			base = dmBase
		case UA:
			base = uaBase
		case FRMR:
			base = frmrBase
		}
		buf = append(buf, base|boolBit(f.PF)<<4)
	}

	// PID and Info for I and UI frames
	if f.Type == I || f.Type == UI {
		buf = append(buf, f.PID)
		buf = append(buf, f.Info...)
	}

	return buf
}

func boolBit(b bool) byte {
	if b {
		return 1
	}
	return 0
}
