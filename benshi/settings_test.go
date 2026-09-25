package benshi

import (
	"errors"
	"testing"
)

// goldenSettings is a READ_SETTINGS reply body captured from a BTech UV-PRO
// on 2026-09-24. On that radio VFO A pointed at channel 252 (its unnamed VFO
// scratch record) and VFO B at channel 1 ("MN Pack"), with dual watch off --
// independently confirmed by GET_HT_STATUS reporting curr_ch_id 252.
var goldenSettings = []byte{
	0x00,
	0xc1, 0x04, 0xa6, 0x06, 0x18, 0x01, 0x3c, 0xe0, 0xa3, 0xf0,
	0x00, 0x20, 0x00, 0x00, 0x08, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
}

func TestDecodeSettingsGolden(t *testing.T) {
	s, err := DecodeSettings(goldenSettings)
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	// 252 is the whole point: the low nibble alone is 12, so a decoder that
	// skips the upper nibble 72 bits in reads a completely different channel.
	if s.ChannelA != 252 {
		t.Errorf("ChannelA = %d, want 252", s.ChannelA)
	}
	if s.ChannelB != 1 {
		t.Errorf("ChannelB = %d, want 1", s.ChannelB)
	}
	if s.DoubleChannel != DoubleChannelOff {
		t.Errorf("DoubleChannel = %d, want off", s.DoubleChannel)
	}
	if !s.KISSEnabled {
		t.Error("KISSEnabled = false, want true (the radio's TNC was on)")
	}
	id, ok := s.ActiveChannel()
	if !ok || id != 252 {
		t.Errorf("ActiveChannel = %d, %v; want 252, true", id, ok)
	}
}

func TestDecodeSettingsRejectsShortAndFailed(t *testing.T) {
	if _, err := DecodeSettings(nil); err != ErrShortSettings {
		t.Errorf("DecodeSettings(nil): err = %v, want ErrShortSettings", err)
	}
	// A radio that REFUSED the command is a different failure from a
	// truncated reply, and reporting it as "too short" sends the operator
	// looking at the wrong thing.
	if _, err := DecodeSettings([]byte{0x05, 1, 2, 3}); !errors.Is(err, ErrRadioRejected) {
		t.Errorf("DecodeSettings(failure status): err = %v, want ErrRadioRejected", err)
	}
	if _, err := DecodeSettings(goldenSettings[:settingsMinLen]); err != ErrShortSettings {
		t.Errorf("DecodeSettings(truncated): err = %v, want ErrShortSettings", err)
	}
}

// TestActiveChannelRefusesDualWatch covers the case the QSY guard depends on:
// with dual watch on, which VFO transmits is not derivable from this record,
// so ActiveChannel must decline rather than pick one.
func TestActiveChannelRefusesDualWatch(t *testing.T) {
	s := Settings{ChannelA: 252, ChannelB: 1, DoubleChannel: DoubleChannelB}
	if _, ok := s.ActiveChannel(); ok {
		t.Error("ActiveChannel accepted a dual-watch radio; want refusal")
	}
	s.DoubleChannel = DoubleChannelA
	if id, ok := s.ActiveChannel(); !ok || id != 252 {
		t.Errorf("ActiveChannel(A) = %d, %v; want 252, true", id, ok)
	}
}
