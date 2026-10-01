package benshi

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// Records captured from a UV-PRO on 2026-10-01. Each pair differs only in the
// field named, which is what makes them golden fixtures rather than samples:
// the radio produced both, so a patcher that turns one into the other is
// provably writing the bits the radio reads.
const (
	// A VFO pointed at memory 5 ("AUS 730"), dual watch off.
	recCh5 = "5104a60618013ce0a300002000000878000000000000"
	// The same radio with the A VFO moved to the VFO scratch record (252).
	recCh252 = "c104a60618013ce0a3f0002000000878000000000000"
	// The same radio on memory 5 with dual watch turned on, side A.
	recDualA = "5114a60618013ce0a300002000000878000000000000"
)

func mustRec(t *testing.T, h string) SettingsRec {
	t.Helper()
	b, err := hex.DecodeString(h)
	if err != nil {
		t.Fatalf("bad fixture %q: %v", h, err)
	}
	rec, err := ParseSettingsRec(append([]byte{0x00}, b...))
	if err != nil {
		t.Fatalf("ParseSettingsRec(%s): %v", h, err)
	}
	return rec
}

// TestWithChannelAMatchesCapturedRecords is the whole point of the two-nibble
// patch: 252 needs its high nibble written 72 bits away from its low one, and a
// patcher that writes only the low nibble produces channel 12 instead -- a
// different, possibly programmed, memory.
func TestWithChannelAMatchesCapturedRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from, to string
		id       byte
	}{
		{"memory 5 to the VFO record", recCh5, recCh252, 252},
		{"the VFO record back to memory 5", recCh252, recCh5, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mustRec(t, tc.from).WithChannelA(tc.id).Bytes()
			want, _ := hex.DecodeString(tc.to)
			if !bytes.Equal(got, want) {
				t.Errorf("WithChannelA(%d):\n got  %s\n want %s", tc.id,
					hex.EncodeToString(got), tc.to)
			}
			if a := mustRec(t, tc.from).WithChannelA(tc.id).Settings().ChannelA; a != tc.id {
				t.Errorf("round trip reads back channel_a = %d, want %d", a, tc.id)
			}
		})
	}
}

// TestWithChannelAPreservesChannelB guards the byte the two fields share:
// channel_a's high nibble and channel_b's high nibble are both in byte 9, so a
// patch that writes the whole byte would move the other VFO as a side effect.
func TestWithChannelAPreservesChannelB(t *testing.T) {
	rec := mustRec(t, recCh5)
	before := rec.Settings()
	after := rec.WithChannelA(252).Settings()
	if after.ChannelB != before.ChannelB {
		t.Errorf("channel_b moved %d -> %d", before.ChannelB, after.ChannelB)
	}
	if after.DoubleChannel != before.DoubleChannel || after.KISSEnabled != before.KISSEnabled {
		t.Errorf("another field moved: double_channel %d->%d kiss %v->%v",
			before.DoubleChannel, after.DoubleChannel, before.KISSEnabled, after.KISSEnabled)
	}
}

// TestWithDoubleChannelMatchesCapturedRecord: the radio produced both of these
// by having dual watch toggled on its front panel, so matching them byte for
// byte proves the field's position and width.
func TestWithDoubleChannelMatchesCapturedRecord(t *testing.T) {
	got := mustRec(t, recCh5).WithDoubleChannel(DoubleChannelA).Bytes()
	want, _ := hex.DecodeString(recDualA)
	if !bytes.Equal(got, want) {
		t.Errorf("WithDoubleChannel(A):\n got  %s\n want %s", hex.EncodeToString(got), recDualA)
	}
	back := mustRec(t, recDualA).WithDoubleChannel(DoubleChannelOff).Bytes()
	wantOff, _ := hex.DecodeString(recCh5)
	if !bytes.Equal(back, wantOff) {
		t.Errorf("WithDoubleChannel(Off):\n got  %s\n want %s", hex.EncodeToString(back), recCh5)
	}
}

// TestSettingsPatchesAreIdentityWhenUnchanged matters because restore compares
// records byte for byte: a patcher that perturbs an unrelated bit would make
// every restore look like an operator edit and be skipped.
func TestSettingsPatchesAreIdentityWhenUnchanged(t *testing.T) {
	for _, h := range []string{recCh5, recCh252, recDualA} {
		rec := mustRec(t, h)
		set := rec.Settings()
		if got := rec.WithChannelA(set.ChannelA); !got.Equal(rec) {
			t.Errorf("WithChannelA(current) changed %s -> %s", h, hex.EncodeToString(got.Bytes()))
		}
		if got := rec.WithDoubleChannel(set.DoubleChannel); !got.Equal(rec) {
			t.Errorf("WithDoubleChannel(current) changed %s -> %s", h, hex.EncodeToString(got.Bytes()))
		}
	}
}

// TestSettingsPatchDoesNotAliasTheOriginal: restore holds the pre-session record
// and compares against it later, so a patcher writing through to the saved copy
// would compare the radio against itself and never restore anything.
func TestSettingsPatchDoesNotAliasTheOriginal(t *testing.T) {
	orig := mustRec(t, recCh5)
	snapshot := orig.Bytes()
	_ = orig.WithChannelA(252)
	_ = orig.WithDoubleChannel(DoubleChannelB)
	if !bytes.Equal(orig.Bytes(), snapshot) {
		t.Errorf("patching mutated the original: %s -> %s",
			hex.EncodeToString(snapshot), hex.EncodeToString(orig.Bytes()))
	}
}

// TestPutBitsIsInverseOfBits across every offset and width the record uses.
func TestPutBitsIsInverseOfBits(t *testing.T) {
	for _, off := range []int{0, 4, 10, 70, 72, 76} {
		for _, n := range []int{1, 2, 4} {
			for v := uint32(0); v < 1<<uint(n); v++ {
				b := make([]byte, 10)
				putBits(b, off, n, v)
				if got := bits(b, off, n); got != v {
					t.Fatalf("putBits(off=%d n=%d v=%d) -> bits = %d", off, n, v, got)
				}
			}
		}
	}
}

// TestPutBitsIgnoresOffsetsPastTheEnd mirrors bits' tolerance of short records:
// a radio model with a shorter settings record must fail the length check, not
// corrupt memory.
func TestPutBitsIgnoresOffsetsPastTheEnd(t *testing.T) {
	b := []byte{0xFF, 0xFF}
	putBits(b, 72, 4, 0xF) // well past the end
	if b[0] != 0xFF || b[1] != 0xFF {
		t.Errorf("putBits past the end modified the slice: % X", b)
	}
}

// TestParseSettingsRecRejectsBadReplies: a rejected or truncated reply must not
// become a record a patcher will later write back to the radio.
func TestParseSettingsRecRejectsBadReplies(t *testing.T) {
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"empty", nil},
		{"non-zero status", append([]byte{0x05}, make([]byte, 22)...)},
		{"too short to decode", []byte{0x00, 0x51, 0x04}},
	} {
		if _, err := ParseSettingsRec(tc.body); err == nil {
			t.Errorf("ParseSettingsRec(%s) accepted a bad reply", tc.name)
		}
	}
}
