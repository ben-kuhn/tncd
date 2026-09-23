package benshi

import (
	"bytes"
	"testing"
)

func TestFreqModeParamsPayload(t *testing.T) {
	p := FreqModeParams{RXFreqHz: 145030000, TXFreqHz: 145030000, Step: DefaultStep}
	got := p.Payload()
	if len(got) != 16 {
		t.Fatalf("payload length = %d, want 16", len(got))
	}
	// 145030000 = 0x08A4FB70; FM modulation is 0 so the top 2 bits stay clear.
	want := []byte{
		0x08, 0xA4, 0xFB, 0x70,
		0x08, 0xA4, 0xFB, 0x70,
		0x00, 0x00,
		0x00, 0x00,
		0x00, 0x00,
		0x61, 0xA8,
	}
	if !bytes.Equal(got, want) {
		t.Errorf("Payload() = % X, want % X", got, want)
	}
}

// Modulation rides in the top 2 bits of each frequency word.
func TestFreqModeParamsModulationBits(t *testing.T) {
	p := FreqModeParams{RXFreqHz: 1, RXMod: 2, TXFreqHz: 1, TXMod: 1, Step: DefaultStep}
	got := p.Payload()
	if got[0]>>6 != 2 {
		t.Errorf("RX modulation bits = %d, want 2", got[0]>>6)
	}
	if got[4]>>6 != 1 {
		t.Errorf("TX modulation bits = %d, want 1", got[4]>>6)
	}
}

// The documented teardown is an all-zero payload, step included.
func TestTeardownPayloadIsAllZero(t *testing.T) {
	got := TeardownPayload()
	if len(got) != 16 {
		t.Fatalf("length = %d, want 16", len(got))
	}
	for i, b := range got {
		if b != 0 {
			t.Errorf("byte %d = %#x, want 0", i, b)
		}
	}
}

// Body[0] is reply status, Body[1:5] the frequency with modulation in the top
// 2 bits. HTCommander's worked example: 09 B0 50 F0 -> 162.550 MHz.
func TestDecodeFreqModeStatus(t *testing.T) {
	body := []byte{0x00, 0x09, 0xB0, 0x50, 0xF0}
	got, err := DecodeFreqModeStatus(body)
	if err != nil {
		t.Fatalf("DecodeFreqModeStatus: %v", err)
	}
	if got != 162550000 {
		t.Errorf("freq = %d, want 162550000", got)
	}
}

func TestDecodeFreqModeStatusRejectsFailureStatus(t *testing.T) {
	if _, err := DecodeFreqModeStatus([]byte{0x01, 0x09, 0xB0, 0x50, 0xF0}); err == nil {
		t.Error("non-zero reply status must be an error")
	}
}

func TestDecodeFreqModeStatusRejectsShort(t *testing.T) {
	if _, err := DecodeFreqModeStatus([]byte{0x00, 0x09}); err == nil {
		t.Error("short body must be an error")
	}
}

// Notification 14: Body[0] type, [1:5] RX, [5:9] TX, [9:13] sub-audio,
// [13:15] flags. Only the LOW flags byte is authoritative for active state.
func TestDecodeFreqModeNotificationActive(t *testing.T) {
	body := []byte{
		14,
		0x08, 0xA4, 0xFB, 0x70,
		0x08, 0xA4, 0xFB, 0x70,
		0x00, 0x00, 0x00, 0x00,
		0x00, 0x40,
	}
	got, err := DecodeFreqModeNotification(body)
	if err != nil {
		t.Fatalf("DecodeFreqModeNotification: %v", err)
	}
	if !got.Active {
		t.Error("Active = false, want true (low flags byte 0x40)")
	}
	if got.RXFreqHz != 145030000 {
		t.Errorf("RXFreqHz = %d, want 145030000", got.RXFreqHz)
	}
}

// The HIGH flags byte can stay set after leaving frequency mode, so it must not
// be consulted.
func TestDecodeFreqModeNotificationHighByteIsNotAuthoritative(t *testing.T) {
	body := []byte{
		14,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0, 0, 0, 0,
		0x80, 0x00,
	}
	got, err := DecodeFreqModeNotification(body)
	if err != nil {
		t.Fatalf("DecodeFreqModeNotification: %v", err)
	}
	if got.Active {
		t.Error("Active = true, want false (low flags byte is 0)")
	}
}

func TestDecodeFreqModeNotificationRejectsWrongType(t *testing.T) {
	body := make([]byte, 15)
	body[0] = 3 // some other notification
	if _, err := DecodeFreqModeNotification(body); err == nil {
		t.Error("non-14 notification type must be an error")
	}
}
