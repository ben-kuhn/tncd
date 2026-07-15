package l2

import (
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

func TestConnectSendsSABM(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c, err := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	if err != nil || c.State != Connecting {
		t.Fatalf("state=%v err=%v", c.State, err)
	}
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.SABM ||
		!rec.sent[0].PF || !rec.sent[0].Command {
		t.Fatalf("sent = %+v", rec.sent)
	}
}

func TestUACompletesConnect(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	if c.State != Connected {
		t.Fatalf("state = %v", c.State)
	}
	if len(rec.connected) != 1 || rec.connected[0] != "N0CALL-2:outgoing" {
		t.Fatalf("connected = %v", rec.connected)
	}
}

func TestDMWhileConnectingFails(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	tbl.OnFrame(0, mkFrame(ax25.DM, "N0CALL-2", "KU0HN-10", pf, resp))
	if rec.failed != 1 || tbl.Get(0, "KU0HN-10", "N0CALL-2") != nil {
		t.Fatalf("failed=%d", rec.failed)
	}
}

func TestSABMRetransmitAndN2GiveUp(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	for i := 0; i < 11; i++ { // N2=10 retries then give up
		clk.advance(120 * time.Second)
	}
	if rec.failed != 1 {
		t.Fatalf("failed = %d, want 1 after N2 exhaustion", rec.failed)
	}
	// 1 initial + 10 retransmits
	sabms := 0
	for _, f := range rec.sent {
		if f.Type == ax25.SABM {
			sabms++
		}
	}
	if sabms != 11 {
		t.Fatalf("SABMs = %d, want 11", sabms)
	}
}

func TestIncomingSABMAccepted(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.OnFrame(0, mkFrame(ax25.SABM, "N0CALL-2", "KU0HN-10", pf))
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c == nil || c.State != Connected {
		t.Fatalf("conn = %+v", c)
	}
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.UA || !rec.sent[0].PF ||
		rec.sent[0].Command {
		t.Fatalf("response = %+v", rec.sent)
	}
	if rec.connected[0] != "N0CALL-2:incoming" {
		t.Fatalf("connected = %v", rec.connected)
	}
}

func TestSABMEGetsDM(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.DM || !rec.sent[0].PF {
		t.Fatalf("response = %+v", rec.sent)
	}
	if tbl.Get(0, "KU0HN-10", "N0CALL-2") != nil {
		t.Fatal("SABME must not create a connection")
	}
}

func TestRemoteDISC(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.OnFrame(0, mkFrame(ax25.SABM, "N0CALL-2", "KU0HN-10", pf))
	tbl.OnFrame(0, mkFrame(ax25.DISC, "N0CALL-2", "KU0HN-10", pf))
	last := rec.sent[len(rec.sent)-1]
	if last.Type != ax25.UA || rec.disconnected != 1 ||
		tbl.Get(0, "KU0HN-10", "N0CALL-2") != nil {
		t.Fatalf("last=%+v disc=%d", last, rec.disconnected)
	}
}

func TestDISCWithoutConnGetsDM(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.OnFrame(0, mkFrame(ax25.DISC, "N0CALL-2", "KU0HN-10", pf))
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.DM {
		t.Fatalf("response = %+v", rec.sent)
	}
}

func TestOverheardSABMOnOtherPortDropped(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.OnFrame(0, mkFrame(ax25.SABM, "N0CALL-2", "KU0HN-10", pf))
	n := len(rec.sent)
	tbl.OnFrame(1, mkFrame(ax25.SABM, "N0CALL-2", "KU0HN-10", pf)) // same pair, port 1
	if len(rec.sent) != n {
		t.Fatal("overheard SABM must be dropped silently")
	}
	if tbl.Get(1, "KU0HN-10", "N0CALL-2") != nil {
		t.Fatal("phantom connection created")
	}
}

func TestFRMRResetsConnection(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.OnFrame(0, mkFrame(ax25.SABM, "N0CALL-2", "KU0HN-10", pf))
	tbl.OnFrame(0, mkFrame(ax25.FRMR, "N0CALL-2", "KU0HN-10", resp))
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c.State != Connecting {
		t.Fatalf("state = %v, want Connecting", c.State)
	}
	last := rec.sent[len(rec.sent)-1]
	if last.Type != ax25.SABM {
		t.Fatalf("last = %+v, want fresh SABM", last)
	}
}
