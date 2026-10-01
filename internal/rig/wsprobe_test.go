package rig

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// cmdWriteSettings is WRITE_SETTINGS (11), not yet a package constant: this
// probe exists to find out whether the radio accepts a read-modify-write of
// the settings record at all, which is the premise the session-settings design
// rests on. Defining it in benshi/ can wait until something ships that uses it.
const cmdWriteSettings benshi.Command = 11

// TestHWSettingsIdentityWrite reads the settings record and writes the exact
// same bytes back. A zero-risk probe of two unknowns: whether the radio accepts
// WRITE_SETTINGS with a full record, and whether the record survives the round
// trip byte-identical. Hardware diagnostic; skipped unless TNCD_HW_BDADDR set.
func TestHWSettingsIdentityWrite(t *testing.T) {
	addr := os.Getenv("TNCD_HW_BDADDR")
	if addr == "" {
		t.Skip("set TNCD_HW_BDADDR to probe WRITE_SETTINGS")
	}
	tr := kiss.NewBluetoothTransport(kiss.BluetoothConfig{BDAddr: addr})
	if err := tr.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tr.Close()
	cc, err := kiss.ControlChannelFor(tr)
	if err != nil {
		t.Fatalf("control channel: %v", err)
	}
	defer cc.Close()
	r := New(cc, 5*time.Second, 0)
	defer r.Close()

	before, err := r.request(benshi.CmdReadSettings, nil)
	if err != nil {
		t.Fatalf("READ_SETTINGS: %v", err)
	}
	if len(before) < 2 || before[0] != 0 {
		t.Fatalf("READ_SETTINGS rejected: % X", before)
	}
	record := append([]byte{}, before[1:]...)
	t.Logf("record (%d bytes): %s", len(record), hex.EncodeToString(record))

	wr, err := r.request(cmdWriteSettings, record)
	if err != nil {
		t.Fatalf("WRITE_SETTINGS: %v", err)
	}
	if len(wr) < 1 {
		t.Fatalf("WRITE_SETTINGS empty reply")
	}
	t.Logf("WRITE_SETTINGS reply: % X (status %d)", wr, wr[0])
	if wr[0] != 0 {
		t.Fatalf("radio REJECTED an identity write with status %d -- "+
			"read-modify-write of the settings record is not available this way", wr[0])
	}

	after, err := r.request(benshi.CmdReadSettings, nil)
	if err != nil {
		t.Fatalf("READ_SETTINGS (after): %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("record changed across an identity write:\n before %s\n after  %s",
			hex.EncodeToString(before), hex.EncodeToString(after))
	} else {
		t.Logf("round trip byte-identical -- read-modify-write is viable")
	}
}

// doubleChannelMask is double_channel's two bits within settings record byte 1.
// benlink places the field at bit offset 10 counting MSB-first from the start of
// the record, so it occupies byte 1's 0x30 bits. Every use below cross-checks
// the patch with DecodeSettings before writing, so a wrong mask aborts the probe
// instead of scribbling on the radio.
const doubleChannelMask = 0x30

// TestHWDualWatchWriteRoundTrip turns dual watch ON via WRITE_SETTINGS, confirms
// the radio reports it, then puts it back and confirms the record is identical
// to where it started. This is the exact operation a managed session performs,
// so it proves the write side of the session-settings design on real hardware.
// Hardware diagnostic; skipped unless TNCD_HW_BDADDR is set.
func TestHWDualWatchWriteRoundTrip(t *testing.T) {
	addr := os.Getenv("TNCD_HW_BDADDR")
	if addr == "" {
		t.Skip("set TNCD_HW_BDADDR to probe dual-watch writes")
	}
	tr := kiss.NewBluetoothTransport(kiss.BluetoothConfig{BDAddr: addr})
	if err := tr.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tr.Close()
	cc, err := kiss.ControlChannelFor(tr)
	if err != nil {
		t.Fatalf("control channel: %v", err)
	}
	defer cc.Close()
	r := New(cc, 5*time.Second, 0)
	defer r.Close()

	readRecord := func(what string) ([]byte, benshi.Settings) {
		b, err := r.request(benshi.CmdReadSettings, nil)
		if err != nil {
			t.Fatalf("READ_SETTINGS (%s): %v", what, err)
		}
		if len(b) < 2 || b[0] != 0 {
			t.Fatalf("READ_SETTINGS (%s) rejected: % X", what, b)
		}
		set, err := benshi.DecodeSettings(b)
		if err != nil {
			t.Fatalf("DecodeSettings (%s): %v", what, err)
		}
		return append([]byte{}, b[1:]...), set
	}

	orig, set := readRecord("before")
	t.Logf("before: %s double_channel=%d", hex.EncodeToString(orig), set.DoubleChannel)
	if set.DoubleChannel != benshi.DoubleChannelOff {
		t.Skipf("dual watch is already on (%d) -- run this probe from a known-off state", set.DoubleChannel)
	}

	// Patch to A, and verify the patch against the decoder BEFORE writing.
	patched := append([]byte{}, orig...)
	patched[1] = (patched[1] & ^byte(doubleChannelMask)) | 0x10
	check, err := benshi.DecodeSettings(append([]byte{0x00}, patched...))
	if err != nil {
		t.Fatalf("DecodeSettings(patched): %v", err)
	}
	if check.DoubleChannel != benshi.DoubleChannelA {
		t.Fatalf("patch produced double_channel=%d, want A(%d) -- mask is wrong, not writing",
			check.DoubleChannel, benshi.DoubleChannelA)
	}

	write := func(rec []byte, what string) {
		wr, err := r.request(cmdWriteSettings, rec)
		if err != nil {
			t.Fatalf("WRITE_SETTINGS (%s): %v", what, err)
		}
		if len(wr) < 1 || wr[0] != 0 {
			t.Fatalf("WRITE_SETTINGS (%s) rejected: % X", what, wr)
		}
	}

	write(patched, "dual watch on")
	// Always put the radio back, even if the assertions below fail.
	defer func() {
		write(orig, "restore")
		back, set := readRecord("after restore")
		if !bytes.Equal(back, orig) {
			t.Errorf("restore left the record changed:\n want %s\n got  %s",
				hex.EncodeToString(orig), hex.EncodeToString(back))
			return
		}
		t.Logf("restored: %s double_channel=%d (byte-identical)", hex.EncodeToString(back), set.DoubleChannel)
	}()

	on, onSet := readRecord("dual watch on")
	t.Logf("after write: %s double_channel=%d", hex.EncodeToString(on), onSet.DoubleChannel)
	if onSet.DoubleChannel != benshi.DoubleChannelA {
		t.Errorf("radio reports double_channel=%d after writing A -- write did not take",
			onSet.DoubleChannel)
	}
	if !bytes.Equal(on, patched) {
		t.Errorf("radio's record differs from what was written:\n wrote %s\n read  %s",
			hex.EncodeToString(patched), hex.EncodeToString(on))
	}
}

// BSS is Benshi's position/beacon subsystem. Its record carries the operator's
// APRS callsign and symbol as well as the enable bit, so these probes restore it
// unconditionally and compare byte-for-byte: a partial write here would destroy
// an operator's APRS identity, not merely a preference.
const (
	cmdReadBssSettings  benshi.Command = 33
	cmdWriteBssSettings benshi.Command = 34

	// aprsEnableMask is bit 0x10 of BSS record byte 1 (reply body byte 2),
	// measured 2026-10-01 against the radio's "Digital Mode -> Enable" toggle.
	aprsEnableMask = 0x10
)

// TestHWBssWriteRoundTrip clears the APRS enable bit via WRITE_BSS_SETTINGS,
// confirms the radio reports it cleared, then restores the original record and
// confirms it is byte-identical -- callsign and symbol included. This is the
// second write a managed session performs. Hardware diagnostic; skipped unless
// TNCD_HW_BDADDR is set.
func TestHWBssWriteRoundTrip(t *testing.T) {
	addr := os.Getenv("TNCD_HW_BDADDR")
	if addr == "" {
		t.Skip("set TNCD_HW_BDADDR to probe BSS writes")
	}
	tr := kiss.NewBluetoothTransport(kiss.BluetoothConfig{BDAddr: addr})
	if err := tr.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tr.Close()
	cc, err := kiss.ControlChannelFor(tr)
	if err != nil {
		t.Fatalf("control channel: %v", err)
	}
	defer cc.Close()
	r := New(cc, 5*time.Second, 0)
	defer r.Close()

	readBss := func(what string) []byte {
		b, err := r.request(cmdReadBssSettings, nil)
		if err != nil {
			t.Fatalf("READ_BSS_SETTINGS (%s): %v", what, err)
		}
		if len(b) < 3 || b[0] != 0 {
			t.Fatalf("READ_BSS_SETTINGS (%s) rejected: % X", what, b)
		}
		return append([]byte{}, b[1:]...)
	}

	orig := readBss("before")
	t.Logf("before (%d bytes): %s  aprs_enabled=%v",
		len(orig), hex.EncodeToString(orig), orig[1]&aprsEnableMask != 0)
	if orig[1]&aprsEnableMask == 0 {
		t.Skipf("APRS is already off -- run this probe with APRS on so clearing it is observable")
	}

	off := append([]byte{}, orig...)
	off[1] &= ^byte(aprsEnableMask)

	write := func(rec []byte, what string) {
		wr, err := r.request(cmdWriteBssSettings, rec)
		if err != nil {
			t.Fatalf("WRITE_BSS_SETTINGS (%s): %v", what, err)
		}
		if len(wr) < 1 || wr[0] != 0 {
			t.Fatalf("WRITE_BSS_SETTINGS (%s) rejected: % X", what, wr)
		}
	}

	write(off, "aprs off")
	defer func() {
		write(orig, "restore")
		back := readBss("after restore")
		if !bytes.Equal(back, orig) {
			t.Errorf("restore left the BSS record changed:\n want %s\n got  %s",
				hex.EncodeToString(orig), hex.EncodeToString(back))
			return
		}
		t.Logf("restored: %s aprs_enabled=%v (byte-identical)",
			hex.EncodeToString(back), back[1]&aprsEnableMask != 0)
	}()

	now := readBss("aprs off")
	t.Logf("after write: %s aprs_enabled=%v", hex.EncodeToString(now), now[1]&aprsEnableMask != 0)
	if now[1]&aprsEnableMask != 0 {
		t.Error("radio still reports APRS enabled after clearing the bit -- write did not take")
	}
	if !bytes.Equal(now, off) {
		t.Errorf("radio's BSS record differs from what was written:\n wrote %s\n read  %s",
			hex.EncodeToString(off), hex.EncodeToString(now))
	}
}

// TestHWSetChannelA points the A VFO at a given channel index and LEAVES IT
// THERE. Unlike the round-trip probes above it deliberately does not restore:
// its purpose is to displace the radio across a power cycle so the volatility
// of WRITE_SETTINGS can be observed. TNCD_SET_CHANNEL_A names the target index.
//
// channel_a is split across the record: the low nibble is bits 0-3 (byte 0's
// high nibble) and the high nibble is bits 72-75 (byte 9's high nibble), so
// writing it means patching two bytes while preserving byte 9's low nibble,
// which carries channel_b's high nibble.
func TestHWSetChannelA(t *testing.T) {
	addr := os.Getenv("TNCD_HW_BDADDR")
	target := os.Getenv("TNCD_SET_CHANNEL_A")
	if addr == "" || target == "" {
		t.Skip("set TNCD_HW_BDADDR and TNCD_SET_CHANNEL_A to move the A VFO")
	}
	var want int
	if _, err := fmt.Sscanf(target, "%d", &want); err != nil || want < 0 || want > 255 {
		t.Fatalf("TNCD_SET_CHANNEL_A=%q is not a channel index", target)
	}

	tr := kiss.NewBluetoothTransport(kiss.BluetoothConfig{BDAddr: addr})
	if err := tr.Open(); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tr.Close()
	cc, err := kiss.ControlChannelFor(tr)
	if err != nil {
		t.Fatalf("control channel: %v", err)
	}
	defer cc.Close()
	r := New(cc, 5*time.Second, 0)
	defer r.Close()

	before, err := r.request(benshi.CmdReadSettings, nil)
	if err != nil {
		t.Fatalf("READ_SETTINGS: %v", err)
	}
	set, err := benshi.DecodeSettings(before)
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	t.Logf("before: %s channel_a=%d", hex.EncodeToString(before[1:]), set.ChannelA)

	rec := append([]byte{}, before[1:]...)
	if len(rec) < 10 {
		t.Fatalf("record too short to patch channel_a: %d bytes", len(rec))
	}
	rec[0] = (rec[0] & 0x0F) | byte(want&0x0F)<<4
	rec[9] = (rec[9] & 0x0F) | byte(want>>4)<<4

	check, err := benshi.DecodeSettings(append([]byte{0x00}, rec...))
	if err != nil {
		t.Fatalf("DecodeSettings(patched): %v", err)
	}
	if int(check.ChannelA) != want {
		t.Fatalf("patch produced channel_a=%d, want %d -- not writing", check.ChannelA, want)
	}
	if check.ChannelB != set.ChannelB || check.DoubleChannel != set.DoubleChannel {
		t.Fatalf("patch disturbed another field (channel_b %d->%d, double_channel %d->%d) -- not writing",
			set.ChannelB, check.ChannelB, set.DoubleChannel, check.DoubleChannel)
	}

	wr, err := r.request(cmdWriteSettings, rec)
	if err != nil {
		t.Fatalf("WRITE_SETTINGS: %v", err)
	}
	if len(wr) < 1 || wr[0] != 0 {
		t.Fatalf("WRITE_SETTINGS rejected: % X", wr)
	}

	after, err := r.request(benshi.CmdReadSettings, nil)
	if err != nil {
		t.Fatalf("READ_SETTINGS (after): %v", err)
	}
	got, err := benshi.DecodeSettings(after)
	if err != nil {
		t.Fatalf("DecodeSettings (after): %v", err)
	}
	t.Logf("after:  %s channel_a=%d", hex.EncodeToString(after[1:]), got.ChannelA)
	if int(got.ChannelA) != want {
		t.Errorf("radio reports channel_a=%d after writing %d -- write did not take", got.ChannelA, want)
	}
	t.Logf("radio LEFT DISPLACED on purpose: channel_a=%d (was %d)", got.ChannelA, set.ChannelA)
}
