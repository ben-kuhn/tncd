package ax25

import "fmt"

// SREJMode is the selective-reject capability advertised in XID.
type SREJMode int

const (
	SREJNone   SREJMode = iota // REJ only
	SREJSingle                 // single SREJ
	SREJMulti                  // multi-SREJ
)

// XIDParams holds the subset of AX.25 2.2 XID parameters tncd negotiates.
// Zero value of an omitted numeric field means "not present".
type XIDParams struct {
	FullDuplex       bool
	SREJ             SREJMode
	Modulo           int // 8 or 128
	IFieldLenRxBytes int // N1 in bytes (XID carries bits); 0 = omit
	WindowRx         int // 0 = omit
}

// XID format identifiers and parameter IDs (Direwolf xid.c).
const (
	xidFI = 0x82
	xidGI = 0x80

	xidPIClasses  = 2
	xidPIHDLC     = 3
	xidPIIFieldRx = 6
	xidPIWindowRx = 8
)

// Classes-of-Procedures bits.
const (
	pvBalancedABM = 0x0100
	pvHalfDuplex  = 0x2000
	pvFullDuplex  = 0x4000
)

// HDLC-Optional-Functions bits (24-bit field).
const (
	pvREJ       = 0x020000
	pvSREJ      = 0x040000
	pvMultiSREJ = 0x000020
	pvExtAddr   = 0x800000
	pvModulo8   = 0x000400
	pvModulo128 = 0x000800
	pvTEST      = 0x002000
	pv16bitFCS  = 0x008000
	pvSyncTx    = 0x000002
)

// Encode builds the XID info field. command selects the command form (a "menu"
// of acceptable SREJ choices) vs the response form (a single SREJ bit), matching
// Direwolf's xid_encode. tncd only sends responses, but both forms are supported.
func (p XIDParams) Encode(command bool) []byte {
	var body []byte

	// Classes of Procedures (always present, PL=2).
	classes := pvBalancedABM
	if p.FullDuplex {
		classes |= pvFullDuplex
	} else {
		classes |= pvHalfDuplex
	}
	body = append(body, xidPIClasses, 2, byte(classes>>8), byte(classes))

	// HDLC Optional Functions (always present, PL=3).
	hdlc := pvExtAddr | pvTEST | pv16bitFCS | pvSyncTx
	if command {
		switch p.SREJ {
		case SREJSingle:
			hdlc |= pvREJ | pvSREJ
		case SREJMulti:
			hdlc |= pvREJ | pvSREJ | pvMultiSREJ
		default:
			hdlc |= pvREJ
		}
	} else {
		switch p.SREJ {
		case SREJSingle:
			hdlc |= pvSREJ
		case SREJMulti:
			hdlc |= pvMultiSREJ
		default:
			hdlc |= pvREJ
		}
	}
	if p.Modulo == 128 {
		hdlc |= pvModulo128
	} else {
		hdlc |= pvModulo8
	}
	body = append(body, xidPIHDLC, 3, byte(hdlc>>16), byte(hdlc>>8), byte(hdlc))

	// I Field Length Rx (bits), PL=2.
	if p.IFieldLenRxBytes > 0 {
		bits := p.IFieldLenRxBytes * 8
		body = append(body, xidPIIFieldRx, 2, byte(bits>>8), byte(bits))
	}
	// Window Size Rx, PL=1.
	if p.WindowRx > 0 {
		body = append(body, xidPIWindowRx, 1, byte(p.WindowRx))
	}

	out := []byte{xidFI, xidGI, byte(len(body) >> 8), byte(len(body))}
	return append(out, body...)
}

// ParseXID decodes an XID info field into the parameters tncd cares about.
func ParseXID(info []byte) (XIDParams, error) {
	p := XIDParams{Modulo: 8, SREJ: SREJNone}
	if len(info) < 4 || info[0] != xidFI || info[1] != xidGI {
		return p, fmt.Errorf("ax25: bad XID header")
	}
	glen := int(info[2])<<8 | int(info[3])
	body := info[4:]
	if glen > len(body) {
		return p, fmt.Errorf("ax25: XID group length overflow")
	}
	body = body[:glen]
	for len(body) >= 2 {
		pi, pl := body[0], int(body[1])
		if 2+pl > len(body) {
			return p, fmt.Errorf("ax25: XID param overflow")
		}
		pv := body[2 : 2+pl]
		switch pi {
		case xidPIClasses:
			if pl == 2 {
				x := int(pv[0])<<8 | int(pv[1])
				p.FullDuplex = x&pvFullDuplex != 0
			}
		case xidPIHDLC:
			if pl == 3 {
				x := int(pv[0])<<16 | int(pv[1])<<8 | int(pv[2])
				if x&pvModulo128 != 0 {
					p.Modulo = 128
				}
				switch {
				case x&pvMultiSREJ != 0:
					p.SREJ = SREJMulti
				case x&pvSREJ != 0:
					p.SREJ = SREJSingle
				default:
					p.SREJ = SREJNone
				}
			}
		case xidPIIFieldRx:
			if pl == 2 {
				bits := int(pv[0])<<8 | int(pv[1])
				p.IFieldLenRxBytes = bits / 8
			}
		case xidPIWindowRx:
			if pl == 1 {
				p.WindowRx = int(pv[0])
			}
		}
		body = body[2+pl:]
	}
	return p, nil
}
