package rig

import (
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// TestHWReadChannels reads the named RF_CH records so the settings record's
// channel_a can be checked against what the front panel shows. Hardware
// diagnostic; skipped unless TNCD_HW_BDADDR is set. TNCD_CHANS is a
// comma-separated list of channel ids (default "5,252").
func TestHWReadChannels(t *testing.T) {
	addr := os.Getenv("TNCD_HW_BDADDR")
	if addr == "" {
		t.Skip("set TNCD_HW_BDADDR to read channels")
	}
	list := os.Getenv("TNCD_CHANS")
	if list == "" {
		list = "5,252"
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

	for _, s := range strings.Split(list, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatalf("bad channel %q: %v", s, err)
		}
		body, err := r.request(benshi.CmdReadRFCh, []byte{byte(n)})
		if err != nil {
			t.Logf("  ch %3d: no answer (%v)", n, err)
			continue
		}
		if len(body) < 1 || body[0] != 0 {
			t.Logf("  ch %3d: rejected (status %d)", n, body[0])
			continue
		}
		ch, err := benshi.ParseRFCh(body[1:])
		if err != nil {
			t.Logf("  ch %3d: parse: %v", n, err)
			continue
		}
		t.Logf("  ch %3d: id=%d name=%q rx=%d tx=%d", n, ch.ID(), ch.Name(), ch.RXFreqHz(), ch.TXFreqHz())
	}
}
