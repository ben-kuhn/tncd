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

// Golden settings records captured from a UV-PRO 2026-10-01, the same radio in
// three states. channel_a = 252 (its VFO scratch record), channel_b = 1
// ("MN Pack"), verified against the front panel.
var (
	goldenDWOff = []byte{ // dual watch off
		0x00, 0xc1, 0x04, 0xa6, 0x06, 0x18, 0x01, 0x3c, 0xe0, 0xa3, 0xf0,
		0x00, 0x20, 0x00, 0x00, 0x08, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	goldenDWAActive = []byte{ // dual watch on, A selected
		0x00, 0xc1, 0x14, 0xa6, 0x06, 0x18, 0x01, 0x3c, 0xe0, 0xa3, 0xf0,
		0x00, 0x20, 0x00, 0x00, 0x08, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	goldenDWBActive = []byte{ // dual watch on, B selected
		0x00, 0xc1, 0x24, 0xa6, 0x06, 0x18, 0x01, 0x3c, 0xe0, 0xa3, 0xf0,
		0x00, 0x20, 0x00, 0x00, 0x08, 0x78, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
)

// TestActiveChannelResolvesSelectedSide pins the dual-watch mapping measured on
// hardware. double_channel carries both "is dual watch on" and which side the
// operator selected as active, and it matches benlink's ChannelType exactly.
//
// An earlier version of this test asserted that a dual-watch radio must be
// REFUSED, because the mapping was unverified and writing the wrong side's
// channel record would retune a band the operator was still listening to.
// Toggling the selection on the front panel and diffing settled it.
func TestActiveChannelResolvesSelectedSide(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record []byte
		wantDC DoubleChannel
		wantCh byte
	}{
		{"dual watch off -> A", goldenDWOff, DoubleChannelOff, 252},
		{"A selected", goldenDWAActive, DoubleChannelA, 252},
		{"B selected", goldenDWBActive, DoubleChannelB, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := DecodeSettings(tc.record)
			if err != nil {
				t.Fatalf("DecodeSettings: %v", err)
			}
			if set.DoubleChannel != tc.wantDC {
				t.Errorf("DoubleChannel = %d, want %d", set.DoubleChannel, tc.wantDC)
			}
			id, ok := set.ActiveChannel()
			if !ok {
				t.Fatalf("ActiveChannel refused a state measured on hardware")
			}
			if id != tc.wantCh {
				t.Errorf("ActiveChannel = %d, want %d", id, tc.wantCh)
			}
		})
	}
}

// An encoding never seen on hardware must still refuse rather than guess,
// because the next step rewrites a channel record.
func TestActiveChannelRefusesUnknownEncoding(t *testing.T) {
	s := Settings{ChannelA: 252, ChannelB: 1, DoubleChannel: DoubleChannel(3)}
	if id, ok := s.ActiveChannel(); ok {
		t.Errorf("ActiveChannel accepted double_channel=3, returned %d", id)
	}
}
