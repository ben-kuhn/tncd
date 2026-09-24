package benshi

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Modulation occupies the top 2 bits of each frequency word.
type Modulation uint8

const (
	ModFM Modulation = 0
	ModAM Modulation = 1
)

// DefaultStep is the constant channel step the radio expects on every
// FREQ_MODE_SET_PAR update (0x61A8 = 25000).
const DefaultStep uint16 = 0x61A8

// freqModePayloadLen is the fixed FREQ_MODE_SET_PAR payload size.
const freqModePayloadLen = 16

// freqMask is the 30-bit frequency field; the top 2 bits are modulation.
const freqMask = 0x3FFFFFFF

// notifyFreqMode is the event-notification type for a frequency-mode change.
const notifyFreqMode = 14

var (
	// ErrShortBody reports a reply body too short for its command.
	ErrShortBody = errors.New("benshi: reply body too short")
	// ErrWrongNotification reports a notification of an unexpected type.
	ErrWrongNotification = errors.New("benshi: not a frequency-mode notification")
)

// FreqModeParams is the FREQ_MODE_SET_PAR payload: it puts the radio into
// frequency (VFO) mode and tunes explicit frequencies.
//
// This is the whole reason tncd never writes a memory channel. The alternative
// -- WRITE_RF_CH -- mutates a stored channel record, because Settings.channel_a
// is an index into the channel table rather than a scratch register. HTCommander
// drives this command about once a second for satellite Doppler tracking, so it
// is by construction not touching NVRAM.
type FreqModeParams struct {
	RXFreqHz   uint32
	TXFreqHz   uint32
	RXMod      Modulation
	TXMod      Modulation
	RXSubAudio uint16 // units of 0.01 Hz; 0 = none
	TXSubAudio uint16
	Flags      uint16 // settles to 0 once in frequency mode
	Step       uint16 // send DefaultStep unchanged on every update
}

// Payload serializes the 16-byte FREQ_MODE_SET_PAR body.
func (p FreqModeParams) Payload() []byte {
	out := make([]byte, freqModePayloadLen)
	binary.BigEndian.PutUint32(out[0:4], p.RXFreqHz&freqMask)
	out[0] = (out[0] & 0x3F) | byte(p.RXMod&0x03)<<6
	binary.BigEndian.PutUint32(out[4:8], p.TXFreqHz&freqMask)
	out[4] = (out[4] & 0x3F) | byte(p.TXMod&0x03)<<6
	binary.BigEndian.PutUint16(out[8:10], p.RXSubAudio)
	binary.BigEndian.PutUint16(out[10:12], p.TXSubAudio)
	binary.BigEndian.PutUint16(out[12:14], p.Flags)
	binary.BigEndian.PutUint16(out[14:16], p.Step)
	return out
}

// TeardownPayload is the documented all-zero FREQ_MODE_SET_PAR body.
//
// WARNING: on real UV-PRO firmware this does NOT drop the radio out of
// frequency mode as documented (the claim traces to HTCommander, which sends
// exactly this and nothing more -- so the documentation is wrong, not this
// encoding). Live capture: sending this payload took a radio parked at
// 145.670 MHz and clamped it to 0x081B3200 = 136000000 Hz, the bottom of its
// VHF tuning range, while remaining in frequency mode the whole time. Do not
// send this expecting to exit frequency mode or restore channel state --
// nothing in this package does that. internal/rig's Rig.Teardown restores by
// reading the current channel's stored frequency and setting the VFO to
// match instead of sending this payload.
func TeardownPayload() []byte { return make([]byte, freqModePayloadLen) }

// DecodeFreqModeStatus parses a FREQ_MODE_GET_STATUS reply body:
// Body[0] is the reply status (0 = success), Body[1:5] the frequency with
// modulation in the top 2 bits.
func DecodeFreqModeStatus(body []byte) (uint32, error) {
	if len(body) < 5 {
		return 0, ErrShortBody
	}
	if body[0] != 0 {
		return 0, fmt.Errorf("benshi: freq mode status reply status %d", body[0])
	}
	return binary.BigEndian.Uint32(body[1:5]) & freqMask, nil
}

// FreqModeStatus is the decoded live state pushed by event notification 14.
type FreqModeStatus struct {
	Active   bool
	RXFreqHz uint32
	TXFreqHz uint32
}

// DecodeFreqModeNotification parses event notification 14.
//
// Only the LOW flags byte is authoritative for Active: the high byte can remain
// set after the radio leaves frequency mode, so consulting it reports frequency
// mode long after it has ended.
func DecodeFreqModeNotification(body []byte) (FreqModeStatus, error) {
	if len(body) < 15 {
		return FreqModeStatus{}, ErrShortBody
	}
	if body[0] != notifyFreqMode {
		return FreqModeStatus{}, ErrWrongNotification
	}
	return FreqModeStatus{
		Active:   body[14] != 0,
		RXFreqHz: binary.BigEndian.Uint32(body[1:5]) & freqMask,
		TXFreqHz: binary.BigEndian.Uint32(body[5:9]) & freqMask,
	}, nil
}
