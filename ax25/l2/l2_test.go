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

// TestStaleTimerExpiryIgnored verifies that each advance() fires exactly one
// T1 expiry per period — no double-fire from a stale cancelled timer (I4).
// In the real engine, a timer closure that raced with Cancel could still post
// to the loop; the c.t1 == self guard prevents it from doing anything. Under
// the fake clock, cancelled timers are already skipped, so this test validates
// the invariant (each T1 period fires exactly one retransmit).
func TestStaleTimerExpiryIgnored(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	_, err := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	if err != nil {
		t.Fatal(err)
	}
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")

	// Advance T1 once → one retransmit (initial SABM already sent).
	pollsBefore := c.t1Polls
	clk.advance(120 * time.Second) // well past T1 at 1200 baud (~4.7s)
	if c.t1Polls != pollsBefore+1 {
		t.Fatalf("t1Polls after one T1 expiry = %d, want %d", c.t1Polls, pollsBefore+1)
	}

	// Advance T1 again → second retransmit only.
	clk.advance(120 * time.Second)
	if c.t1Polls != pollsBefore+2 {
		t.Fatalf("t1Polls after two T1 expiries = %d, want %d", c.t1Polls, pollsBefore+2)
	}

	// Each T1 expiry in Connecting state sends exactly one SABM.
	// Initial SABM + 2 retransmits = 3 total. If a stale timer double-fired,
	// we'd see more.
	sabms := 0
	for _, f := range rec.sent {
		if f.Type == ax25.SABM {
			sabms++
		}
	}
	if sabms != 3 {
		t.Fatalf("SABMs = %d, want 3 (initial + 2 retransmits)", sabms)
	}
}

// TestRemovedConnTimerIgnored verifies that timers cancelled when a connection
// is removed do not fire any frames afterward (I4 — removed-conn invariant).
// The c.t3 == self guard in startT3 ensures this even under real-timer races.
func TestRemovedConnTimerIgnored(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	// Establish a Connected state via incoming SABM (T3 starts after OnFrame).
	tbl.OnFrame(0, mkFrame(ax25.SABM, "N0CALL-2", "KU0HN-10", pf))
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c == nil || c.State != Connected {
		t.Fatal("connection not established")
	}

	framesBefore := len(rec.sent)

	// Remove the connection while T3 is armed.
	tbl.remove(0, "KU0HN-10", "N0CALL-2")

	// Advance past T3 (180 s by default at 1200 baud).
	clk.advance(200 * time.Second)

	// No frames should have been sent from the stale T3 expiry.
	if got := len(rec.sent); got != framesBefore {
		t.Fatalf("stale T3 sent %d unexpected frame(s) after conn removed", got-framesBefore)
	}
}

func setV22(tbl *Table, port int) {
	tbl.params[port].AX25Version = 22
}

func TestConnectSendsSABMEOnV22(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	if c.modulo != 128 {
		t.Fatalf("modulo = %d, want 128", c.modulo)
	}
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.SABME {
		t.Fatalf("sent = %+v, want one SABME", rec.sent)
	}
}

func TestIncomingSABMEAcceptedOnV22(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c == nil || c.State != Connected || c.modulo != 128 {
		t.Fatalf("conn = %+v", c)
	}
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.UA {
		t.Fatalf("sent = %+v, want UA", rec.sent)
	}
}

func TestIncomingSABMERejectedOnV20(t *testing.T) {
	tbl, rec, _ := newHarness(1200) // AX25Version stays 0 (== 2.0 only)
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	if tbl.Get(0, "KU0HN-10", "N0CALL-2") != nil {
		t.Fatal("no conn should be created on a 2.0 port")
	}
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.DM {
		t.Fatalf("sent = %+v, want DM", rec.sent)
	}
}

func TestFallbackOnDM(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil) // sends SABME
	tbl.OnFrame(0, mkFrame(ax25.DM, "N0CALL-2", "KU0HN-10", pf, resp))
	if c.State != Connecting || c.modulo != 8 || !c.triedFallback {
		t.Fatalf("after DM: state=%v modulo=%d triedFallback=%v", c.State, c.modulo, c.triedFallback)
	}
	// Last sent frame is a SABM (the fallback).
	last := rec.sent[len(rec.sent)-1]
	if last.Type != ax25.SABM {
		t.Fatalf("fallback frame = %s, want SABM", last.Type)
	}
}

func TestFallbackOnTimeoutAtMaxV22(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	setV22(tbl, 0)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil) // SABME #1
	// N2Retry=10 → maxV22=3. Expiries 1,2 resend SABME; expiry 3 downgrades.
	for i := 0; i < 3; i++ {
		clk.advance(65 * time.Second) // exceed T1 (with backoff)
	}
	if c.modulo != 8 {
		t.Fatalf("modulo = %d after 3 timeouts, want 8", c.modulo)
	}
	sabmeCount, sabmCount := 0, 0
	for _, f := range rec.sent {
		switch f.Type {
		case ax25.SABME:
			sabmeCount++
		case ax25.SABM:
			sabmCount++
		}
	}
	if sabmeCount != 3 {
		t.Fatalf("SABME count = %d, want 3", sabmeCount)
	}
	if sabmCount < 1 {
		t.Fatalf("expected at least one SABM after fallback")
	}
}

func TestModuloForDefaultsTo8(t *testing.T) {
	tbl, _, _ := newHarness(1200)
	if m := tbl.ModuloFor(0, "KU0HN-10", "N0CALL-2"); m != 8 {
		t.Fatalf("ModuloFor with no conn = %d, want 8", m)
	}
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	c.modulo = 128
	if m := tbl.ModuloFor(0, "KU0HN-10", "N0CALL-2"); m != 128 {
		t.Fatalf("ModuloFor = %d, want 128", m)
	}
}

func TestReceivedSREJRetransmitsOneFrame(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	tbl.params[0].MaxWindow = 7 // ensure all 4 I-frames transmit before SREJ arrives
	// Establish outgoing mod-128 link.
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil) // SABME
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	rec.sent = nil

	// Queue 4 I-frames (N(S) 0..3).
	for i := 0; i < 4; i++ {
		tbl.SendData(c, 0xF0, []byte{byte('A' + i)})
	}
	rec.sent = nil

	// Peer SREJs frame 1: acks 0, requests only 1.
	srej := mkFrame(ax25.SREJ, "N0CALL-2", "KU0HN-10", resp, nr(1))
	tbl.OnFrame(0, srej)

	// Exactly one I-frame retransmitted, and it is N(S)=1.
	var iframes []*ax25.Frame
	for _, f := range rec.sent {
		if f.Type == ax25.I {
			iframes = append(iframes, f)
		}
	}
	if len(iframes) != 1 || iframes[0].NS != 1 {
		t.Fatalf("SREJ retransmit = %d frames, first NS=%v; want exactly frame 1", len(iframes), iframes)
	}
}

func TestXIDCommandGetsResponse(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	// Establish an incoming mod-128 link first.
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	rec.sent = nil

	// Peer's XID command: modulo 128, SREJ single, N1 256, window 7.
	cmd := ax25.XIDParams{Modulo: 128, SREJ: ax25.SREJSingle, IFieldLenRxBytes: 256, WindowRx: 7}
	xf := mkFrame(ax25.XID, "N0CALL-2", "KU0HN-10", pf)
	xf.Info = cmd.Encode(true)
	tbl.OnFrame(0, xf)

	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.XID {
		t.Fatalf("sent = %+v, want one XID response", rec.sent)
	}
	got, err := ax25.ParseXID(rec.sent[0].Info)
	if err != nil {
		t.Fatal(err)
	}
	if got.SREJ != ax25.SREJNone {
		t.Errorf("SREJ = %v, want none (must suppress Direwolf SREJ)", got.SREJ)
	}
	if got.Modulo != 128 || got.IFieldLenRxBytes != 256 {
		t.Errorf("params = %+v", got)
	}
	if got.WindowRx != 3 { // min(7, MaxWindow=3)
		t.Errorf("window = %d, want 3", got.WindowRx)
	}
}

// TestConnectResetsTriedFallback verifies that calling Connect on a reused Conn
// resets triedFallback so that a fresh v2.2 attempt can still downgrade to SABM
// if needed. getOrCreate returns the same Conn object on reconnect, so the reset
// must happen inside Connect itself.
func TestConnectResetsTriedFallback(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)

	// First connect — sends SABME; force DM fallback so triedFallback becomes true.
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	tbl.OnFrame(0, mkFrame(ax25.DM, "N0CALL-2", "KU0HN-10", pf, resp))
	if !c.triedFallback || c.modulo != 8 {
		t.Fatalf("precondition: triedFallback=%v modulo=%d", c.triedFallback, c.modulo)
	}

	// Reconnect using the same (port, local, remote) — getOrCreate returns the
	// same Conn. Connect must reset triedFallback and send SABME (not SABM).
	rec.sent = nil
	c2, err := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	if err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}
	if c2 != c {
		t.Fatal("expected getOrCreate to return the same Conn object")
	}
	if c.triedFallback {
		t.Error("triedFallback must be false after Connect (was not reset)")
	}
	if c.modulo != 128 {
		t.Errorf("modulo = %d, want 128 (v2.2 port reset)", c.modulo)
	}
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.SABME {
		t.Errorf("sent = %+v, want exactly one SABME after reconnect", rec.sent)
	}
}

func setSREJ(tbl *Table, port int, on bool) { tbl.params[port].SREJ = on }

func TestXIDAnswererEnablesSREJ(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setSREJ(tbl, 0, true)
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf)) // mod-128 conn
	rec.sent = nil

	cmd := ax25.XIDParams{Modulo: 128, SREJ: ax25.SREJSingle, IFieldLenRxBytes: 256, WindowRx: 7}
	xf := mkFrame(ax25.XID, "N0CALL-2", "KU0HN-10", pf)
	xf.Info = cmd.Encode(true)
	tbl.OnFrame(0, xf)

	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c == nil || !c.srejEnabled {
		t.Fatalf("srejEnabled = false, want true")
	}
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.XID {
		t.Fatalf("want one XID response, got %+v", rec.sent)
	}
	got, _ := ax25.ParseXID(rec.sent[0].Info)
	if got.SREJ != ax25.SREJSingle {
		t.Errorf("response SREJ = %v, want SREJSingle", got.SREJ)
	}
}

func TestXIDAnswererSREJNoneWhenPeerNone(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setSREJ(tbl, 0, true)
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	rec.sent = nil
	cmd := ax25.XIDParams{Modulo: 128, SREJ: ax25.SREJNone, IFieldLenRxBytes: 256, WindowRx: 7}
	xf := mkFrame(ax25.XID, "N0CALL-2", "KU0HN-10", pf)
	xf.Info = cmd.Encode(true)
	tbl.OnFrame(0, xf)
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c.srejEnabled {
		t.Fatalf("srejEnabled = true, want false (peer advertised none)")
	}
	got, _ := ax25.ParseXID(rec.sent[0].Info)
	if got.SREJ != ax25.SREJNone {
		t.Errorf("response SREJ = %v, want SREJNone", got.SREJ)
	}
}

func TestXIDAnswererSREJOffConfig(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setSREJ(tbl, 0, false) // config off
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	rec.sent = nil
	cmd := ax25.XIDParams{Modulo: 128, SREJ: ax25.SREJSingle, IFieldLenRxBytes: 256, WindowRx: 7}
	xf := mkFrame(ax25.XID, "N0CALL-2", "KU0HN-10", pf)
	xf.Info = cmd.Encode(true)
	tbl.OnFrame(0, xf)
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c.srejEnabled {
		t.Fatalf("srejEnabled = true, want false (srej config off)")
	}
	got, _ := ax25.ParseXID(rec.sent[0].Info)
	if got.SREJ != ax25.SREJNone {
		t.Errorf("response SREJ = %v, want SREJNone (config off)", got.SREJ)
	}
}

func TestInitiatorSendsXIDOnConnect(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setSREJ(tbl, 0, true)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil) // sends SABME
	rec.sent = nil
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp)) // connect confirmed
	// After UA, tncd should send an XID command advertising SREJSingle.
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.XID || !rec.sent[0].Command {
		t.Fatalf("want one XID command, got %+v", rec.sent)
	}
	got, _ := ax25.ParseXID(rec.sent[0].Info)
	if got.SREJ != ax25.SREJSingle {
		t.Errorf("XID cmd SREJ = %v, want SREJSingle", got.SREJ)
	}
	if c.srejEnabled {
		t.Errorf("srejEnabled true before response; want false until response arrives")
	}
}

func TestInitiatorEnablesSREJOnXIDResponse(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setSREJ(tbl, 0, true)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	rec.sent = nil
	// Peer's XID response advertising SREJSingle (F=1 response).
	rsp := ax25.XIDParams{Modulo: 128, SREJ: ax25.SREJSingle, IFieldLenRxBytes: 256, WindowRx: 7}
	xf := mkFrame(ax25.XID, "N0CALL-2", "KU0HN-10", pf, resp) // response
	xf.Info = rsp.Encode(false)
	tbl.OnFrame(0, xf)
	if !c.srejEnabled {
		t.Fatalf("srejEnabled = false after SREJSingle XID response, want true")
	}
	if len(rec.sent) != 0 {
		t.Fatalf("must not reply to an XID response, sent %+v", rec.sent)
	}
}

func TestInitiatorNoXIDWhenSREJOff(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setSREJ(tbl, 0, false)
	tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	rec.sent = nil
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	for _, fr := range rec.sent {
		if fr.Type == ax25.XID {
			t.Fatalf("XID command sent with srej=off")
		}
	}
}

// TestIncomingSABMESuppressedOnOtherPort mirrors TestOverheardSABMOnOtherPortDropped
// but for SABME: if (local, remote) already has a Connecting/Connected conn on
// port 0, an incoming SABME for the same pair on port 1 must be silently dropped
// (no UA, no DM, no conn created on port 1).
// establishIncomingSREJ brings up a mod-128 conn with srejEnabled=true and
// returns it, clearing rec.sent.
func establishIncomingSREJ(t *testing.T, tbl *Table, rec *recorder) *Conn {
	t.Helper()
	setV22(tbl, 0)
	setSREJ(tbl, 0, true)
	tbl.params[0].MaxWindow = 7
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	cmd := ax25.XIDParams{Modulo: 128, SREJ: ax25.SREJSingle, IFieldLenRxBytes: 256, WindowRx: 7}
	xf := mkFrame(ax25.XID, "N0CALL-2", "KU0HN-10", pf)
	xf.Info = cmd.Encode(true)
	tbl.OnFrame(0, xf)
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c == nil || !c.srejEnabled {
		t.Fatalf("setup: srejEnabled not true")
	}
	rec.sent = nil
	rec.data = nil
	return c
}

// iframe delivers an incoming I-frame with N(S)=ns carrying one payload byte.
func iframe(tbl *Table, ns uint8, payload byte) {
	f := mkFrame(ax25.I, "N0CALL-2", "KU0HN-10")
	f.Modulo = 128
	f.NS = ns
	f.NR = 0
	f.PID = 0xF0
	f.Info = []byte{payload}
	tbl.OnFrame(0, f)
}

func TestSREJReceiverBuffersAndDelivers(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := establishIncomingSREJ(t, tbl, rec)

	iframe(tbl, 0, 'A') // in sequence → delivered
	if len(rec.data) != 1 || rec.data[0][0] != 'A' {
		t.Fatalf("after NS=0, data=%v want [A]", rec.data)
	}
	iframe(tbl, 2, 'C') // ahead → buffered, SREJ(1)
	iframe(tbl, 3, 'D') // ahead → buffered, SREJ(1) already sent (dedup)
	if len(rec.data) != 1 {
		t.Fatalf("frames 2,3 must not be delivered yet, data=%v", rec.data)
	}
	// Exactly one SREJ for N(R)=1 emitted.
	srejCount, srejNR := 0, uint8(255)
	for _, f := range rec.sent {
		if f.Type == ax25.SREJ {
			srejCount++
			srejNR = f.NR
		}
	}
	if srejCount != 1 || srejNR != 1 {
		t.Fatalf("want exactly one SREJ for NR=1, got count=%d nr=%d", srejCount, srejNR)
	}
	// Fill the gap.
	iframe(tbl, 1, 'B')
	// Now B, C, D delivered in order; V(R) advanced to 4.
	if len(rec.data) != 4 ||
		rec.data[1][0] != 'B' || rec.data[2][0] != 'C' || rec.data[3][0] != 'D' {
		t.Fatalf("after fill, data=%v want [A B C D]", rec.data)
	}
	if c.recvSeq != 4 {
		t.Fatalf("recvSeq=%d want 4", c.recvSeq)
	}
	if len(c.rxBuf) != 0 {
		t.Fatalf("rxBuf not drained: %v", c.rxBuf)
	}
}

func TestSREJFBitOnVR(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	establishIncomingSREJ(t, tbl, rec)
	iframe(tbl, 0, 'A')
	iframe(tbl, 1, 'B') // in seq → delivered, V(R)=2
	iframe(tbl, 3, 'D') // gap at 2 → SREJ(2) with F=1 (2 == V(R))
	for _, f := range rec.sent {
		if f.Type == ax25.SREJ && f.NR == 2 {
			if !f.PF {
				t.Fatalf("SREJ for V(R)=2 must have F=1")
			}
			return
		}
	}
	t.Fatalf("no SREJ(2) emitted")
}

func TestSREJDisabledUsesREJ(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setSREJ(tbl, 0, false) // SREJ off → REJ path
	tbl.params[0].MaxWindow = 7
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	c := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c.srejEnabled {
		t.Fatal("srejEnabled must be false")
	}
	rec.sent = nil
	rec.data = nil
	iframe(tbl, 0, 'A')
	iframe(tbl, 2, 'C') // gap → REJ (phase-3 behavior), not SREJ
	sawREJ, sawSREJ := false, false
	for _, f := range rec.sent {
		switch f.Type {
		case ax25.REJ:
			sawREJ = true
		case ax25.SREJ:
			sawSREJ = true
		}
	}
	if !sawREJ || sawSREJ {
		t.Fatalf("SREJ-off path: sawREJ=%v sawSREJ=%v (want REJ, no SREJ)", sawREJ, sawSREJ)
	}
	if len(c.rxBuf) != 0 {
		t.Fatalf("REJ path must not buffer: %v", c.rxBuf)
	}
}

func TestIncomingSABMESuppressedOnOtherPort(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	setV22(tbl, 0)
	setV22(tbl, 1)

	// Establish mod-128 conn for (KU0HN-10, N0CALL-2) on port 0 via incoming SABME.
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	c0 := tbl.Get(0, "KU0HN-10", "N0CALL-2")
	if c0 == nil || c0.State != Connected {
		t.Fatalf("precondition: port-0 conn not Connected (got %+v)", c0)
	}

	// Record frame count after port-0 setup, then deliver SABME on port 1.
	n := len(rec.sent)
	tbl.OnFrame(1, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))

	// No new frames must have been sent (no UA, no DM).
	if len(rec.sent) != n {
		t.Errorf("overheard SABME sent %d unexpected frame(s) on port 1: %+v",
			len(rec.sent)-n, rec.sent[n:])
	}
	// No conn must have been created on port 1.
	if tbl.Get(1, "KU0HN-10", "N0CALL-2") != nil {
		t.Error("phantom connection created on port 1 for overheard SABME")
	}
}
