package rig

import (
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// TestHWDumpSettings captures one READ_SETTINGS record to a file so successive
// captures can be diffed bit by bit. Hardware diagnostic; skipped unless
// TNCD_HW_BDADDR is set. TNCD_DUMP_LABEL names the output file.
func TestHWDumpSettings(t *testing.T) {
	addr := os.Getenv("TNCD_HW_BDADDR")
	if addr == "" {
		t.Skip("set TNCD_HW_BDADDR to capture settings")
	}
	label := os.Getenv("TNCD_DUMP_LABEL")
	if label == "" {
		label = "capture"
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

	body, err := r.request(benshi.CmdReadSettings, nil)
	if err != nil {
		t.Fatalf("READ_SETTINGS: %v", err)
	}
	st, err := r.request(benshi.CmdGetHTStatus, nil)
	if err != nil {
		t.Fatalf("GET_HT_STATUS: %v", err)
	}

	// Also grab the other readable records, since APRS turned out not to live
	// in READ_SETTINGS. BSS is Benshi's position/beacon subsystem and
	// ADVANCED_SETTINGS is the other plausible home for a "digital mode"
	// toggle. Both read-only; a command the radio refuses just records as
	// empty rather than aborting the capture.
	extra := map[string]benshi.Command{
		"advanced":  29, // READ_ADVANCED_SETTINGS
		"bss":       33, // READ_BSS_SETTINGS
		"advanced2": 63, // READ_ADVANCED_SETTINGS2
	}
	out := fmt.Sprintf("settings %s\nstatus %s\n", hex.EncodeToString(body), hex.EncodeToString(st))
	for _, name := range []string{"advanced", "bss", "advanced2"} {
		b, err := r.request(extra[name], nil)
		if err != nil {
			t.Logf("  %-10s: no answer (%v)", name, err)
			continue
		}
		out += fmt.Sprintf("%s %s\n", name, hex.EncodeToString(b))
		t.Logf("  %-10s: %s", name, hex.EncodeToString(b))
	}
	path := os.Getenv("TNCD_DUMP_DIR") + "/" + label + ".txt"
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("captured %q -> %s", label, path)
	t.Logf("  settings: %s", hex.EncodeToString(body))
	t.Logf("  status  : %s", hex.EncodeToString(st))
	if s, err := benshi.DecodeSettings(body); err == nil {
		t.Logf("  decoded : channel_a=%d channel_b=%d double_channel=%d kiss_en=%v",
			s.ChannelA, s.ChannelB, s.DoubleChannel, s.KISSEnabled)
	}
}
