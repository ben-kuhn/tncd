package benshi

import "fmt"

// devInfoLen is the DevInfo payload size: the fields below pack to exactly
// 80 bits.
const devInfoLen = 10

// DevInfo is the subset of GET_DEV_INFO's reply that is worth trusting.
//
// The reply carries a pile of capability bits as well. They are deliberately
// NOT exposed, because two of them are demonstrably wrong on real firmware:
// a BTech UV-PRO running soft_ver 146 reports support_vfo = 0 while happily
// operating in VFO mode, and channel_count = 30 while channel record 252
// exists and is the VFO. Anything gated on those would refuse a radio that
// works, so this type keeps only the identification fields, which decode
// sensibly and are useful for telling an operator what answered.
type DevInfo struct {
	VendorID  uint8
	ProductID uint16
	HWVer     uint8
	SoftVer   uint16
}

// String renders the identification for operator-facing output.
func (d DevInfo) String() string {
	return fmt.Sprintf("vendor %d, product %d, hw %d, firmware %d",
		d.VendorID, d.ProductID, d.HWVer, d.SoftVer)
}

// DecodeDevInfo parses a GET_DEV_INFO reply body (status byte included).
//
// That a radio answers this at all is the only reliable "speaks Benshi"
// signal available: a non-Benshi TNC sharing the same SPP link simply never
// replies. See internal/rig's probe-on-first-use.
func DecodeDevInfo(body []byte) (DevInfo, error) {
	if len(body) < 1 {
		return DevInfo{}, ErrShortBody
	}
	if body[0] != 0 {
		return DevInfo{}, fmt.Errorf("%w (GET_DEV_INFO status %d)", ErrRadioRejected, body[0])
	}
	b := body[1:]
	if len(b) < devInfoLen {
		return DevInfo{}, ErrShortBody
	}
	return DevInfo{
		VendorID:  b[0],
		ProductID: uint16(b[1])<<8 | uint16(b[2]),
		HWVer:     b[3],
		SoftVer:   uint16(b[4])<<8 | uint16(b[5]),
	}, nil
}
