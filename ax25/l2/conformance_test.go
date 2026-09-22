package l2

// Spec-conformance regressions found by the pre-2.0 audit. Each test pins one
// divergence from AX.25 2.2 / Dire Wolf's ax25_link.c reference behavior.

import (
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

func countType(rec *recorder, typ ax25.FrameType) int {
	n := 0
	for _, f := range rec.sent {
		if f.Type == typ {
			n++
		}
	}
	return n
}

// Only a *command* with P=1 demands a response. An RR F=1 response (the peer
// answering our poll) must not be answered, or every poll cycle puts an extra
// frame on the half-duplex channel.
func TestPollResponseIsNotAnswered(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	connect(t, tbl, rec)
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", pf, resp))
	if len(rec.sent) != 0 {
		t.Fatalf("answered an RR F=1 response with %d frame(s): %+v", len(rec.sent), rec.sent)
	}
}

// An unanswered T3 liveness poll must escalate through T1 retries (N2) and
// then tear the link down; otherwise a dead idle link stays Connected forever.
func TestUnansweredT3PollDisconnects(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	connect(t, tbl, rec)
	for i := 0; i < 60; i++ {
		clk.advance(30 * time.Second)
	}
	if tbl.Get(0, "KU0HN-10", "N0CALL-2") != nil || rec.disconnected != 1 {
		t.Fatalf("dead idle link not torn down: disconnected=%d polls=%d",
			rec.disconnected, countType(rec, ax25.RR))
	}
	// T3 poll + N2 T1 re-polls (N2=10 in the harness).
	if got := countType(rec, ax25.RR); got != 11 {
		t.Fatalf("RR polls = %d, want 11 (T3 poll + N2 re-polls)", got)
	}
}

// An answered T3 poll ends timer recovery: no further polls, no disconnect.
func TestAnsweredT3PollKeepsLink(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	connect(t, tbl, rec)
	clk.advance(181 * time.Second) // T3 poll
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", pf, resp))
	rec.sent = nil
	clk.advance(60 * time.Second) // well past T1, short of the next T3
	if len(rec.sent) != 0 || rec.disconnected != 0 {
		t.Fatalf("answered poll still re-polled: sent=%+v disconnected=%d", rec.sent, rec.disconnected)
	}
}

// Timer recovery: when the peer answers our T1 poll with F=1 and frames are
// still outstanding, retransmit from its N(R) at once rather than waiting for
// another (doubled) T1.
func TestPollAnswerTriggersRetransmit(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.SendData(c, 0xF0, []byte("a"))
	tbl.SendData(c, 0xF0, []byte("b"))
	clk.advance(c.t1Value + time.Millisecond) // poll #1: RR P=1 only
	rec.sent = nil
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", pf, resp, nr(1)))
	ifr := iframes(rec)
	if len(ifr) != 1 || ifr[0].NS != 1 {
		t.Fatalf("poll answer N(R)=1 retransmitted %v, want exactly N(S)=1", ifr)
	}
}

// v2.0 REJ recovery: one lost frame must produce ONE REJ (the "reject
// exception"), not one per following out-of-sequence frame.
func TestSingleREJPerException(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.params[0].MaxWindow = 7
	connect(t, tbl, rec)
	for i := uint8(1); i <= 6; i++ { // N(S)=0 lost
		tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(i), nr(0), info([]byte("x"))))
	}
	if got := countType(rec, ax25.REJ); got != 1 {
		t.Fatalf("REJs for one lost frame = %d, want 1", got)
	}
	// The expected frame clears the exception; a fresh gap earns a fresh REJ.
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0), info([]byte("x"))))
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(2), nr(0), info([]byte("x"))))
	if got := countType(rec, ax25.REJ); got != 2 {
		t.Fatalf("REJs after a new gap = %d, want 2", got)
	}
}

// While in reject exception, an out-of-sequence I-frame with P=1 still gets
// its mandatory RR F=1.
func TestREJExceptionStillAnswersPoll(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.params[0].MaxWindow = 7
	connect(t, tbl, rec)
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(1), nr(0), info([]byte("x"))))
	rec.sent = nil
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(2), nr(0), pf, info([]byte("x"))))
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.RR || !rec.sent[0].PF || rec.sent[0].NR != 0 {
		t.Fatalf("poll during REJ exception = %+v, want RR F=1 N(R)=0", rec.sent)
	}
}

func mod128Link(t *testing.T, tbl *Table, rec *recorder) *Conn {
	t.Helper()
	setV22(tbl, 0)
	tbl.params[0].MaxWindow = 7
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	rec.sent = nil
	return c
}

// An SREJ acknowledges N(R)-1 only when F=1 (AX.25 2.2 4.3.2.4; Dire Wolf
// srej_frame). Treating an F=0 SREJ as an ack discards unconfirmed frames.
func TestSREJWithoutFinalDoesNotAck(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := mod128Link(t, tbl, rec)
	for i := 0; i < 4; i++ {
		tbl.SendData(c, 0xF0, []byte{byte('a' + i)})
	}
	f := mkFrame(ax25.SREJ, "N0CALL-2", "KU0HN-10", resp, nr(2))
	tbl.OnFrame(0, f)
	if c.retransmitBuf[0] == nil || c.retransmitBuf[1] == nil || c.unacked != 4 {
		t.Fatalf("SREJ F=0 acked frames: unacked=%d has0=%v has1=%v",
			c.unacked, c.retransmitBuf[0] != nil, c.retransmitBuf[1] != nil)
	}
	tbl.OnFrame(0, mkFrame(ax25.SREJ, "N0CALL-2", "KU0HN-10", resp, pf, nr(2)))
	if c.retransmitBuf[1] != nil || c.unacked != 2 {
		t.Fatalf("SREJ F=1 N(R)=2 did not ack 0..1: unacked=%d", c.unacked)
	}
}

// FRMR on an established mod-128 link re-establishes at mod-128 (SABME), not
// with a SABM that leaves the conn believing it is still mod-128.
func TestFRMRReestablishesAtLinkModulo(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := mod128Link(t, tbl, rec)
	c.srejEnabled = true
	tbl.OnFrame(0, mkFrame(ax25.FRMR, "N0CALL-2", "KU0HN-10", resp))
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.SABME {
		t.Fatalf("FRMR on mod-128 link sent %+v, want SABME", rec.sent)
	}
	if c.srejEnabled {
		t.Fatal("FRMR reset left SREJ enabled")
	}
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	if c.State != Connected || c.modulo != 128 {
		t.Fatalf("after UA: state=%v modulo=%d", c.State, c.modulo)
	}
}

// A SABM (v2.0) on a conn previously established by SABME is mod-8.
func TestSABMAfterSABMEIsMod8(t *testing.T) {
	tbl, _, _ := newHarness(1200)
	setV22(tbl, 0)
	tbl.OnFrame(0, mkFrame(ax25.SABME, "N0CALL-2", "KU0HN-10", pf))
	tbl.OnFrame(0, mkFrame(ax25.SABM, "N0CALL-2", "KU0HN-10", pf))
	if c := tbl.Get(0, "KU0HN-10", "N0CALL-2"); c.modulo != 8 {
		t.Fatalf("modulo = %d after SABM, want 8", c.modulo)
	}
}

// Every frame on a digipeated link carries the path — including the T1 poll.
func TestT1PollCarriesDigipeaterPath(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", []string{"WIDE1-1"})
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	tbl.SendData(c, 0xF0, []byte("x"))
	rec.sent = nil
	clk.advance(c.t1Value + time.Millisecond)
	if len(rec.sent) == 0 {
		t.Fatal("no T1 poll sent")
	}
	for _, f := range rec.sent {
		if len(f.Via) != 1 || f.Via[0].String() != "WIDE1-1" {
			t.Fatalf("T1 %v frame via=%v, want [WIDE1-1]", f.Type, f.Via)
		}
	}
}

// An unencodable callsign must be refused, never transmitted as a blank address.
func TestConnectRejectsInvalidCallsigns(t *testing.T) {
	cases := []struct {
		local, remote string
		via           []string
	}{
		{"KU0HN-10", "TOOLONGCALL", nil},
		{"KU0HN-99", "N0CALL-2", nil},
		{"KU0HN-10", "N0CALL-2", []string{"BAD CALL"}},
		{"KU0HN-10", "", nil},
	}
	for _, tc := range cases {
		tbl, rec, _ := newHarness(1200)
		if _, err := tbl.Connect(0, tc.local, tc.remote, tc.via); err == nil {
			t.Errorf("Connect(%q,%q,%v) accepted", tc.local, tc.remote, tc.via)
		}
		if len(rec.sent) != 0 || tbl.Get(0, tc.local, tc.remote) != nil {
			t.Errorf("Connect(%q,%q,%v) sent %d frame(s) / left a conn", tc.local, tc.remote, tc.via, len(rec.sent))
		}
	}
}

// An N(R) outside [V(A), V(S)] acknowledges frames never sent; it must be
// ignored rather than corrupt V(A) (Dire Wolf is_good_nr).
func TestNRBeyondVSIgnored(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.SendData(c, 0xF0, []byte("a")) // V(S)=1
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", resp, nr(3)))
	if c.lastAcked != 0 || c.unacked != 1 {
		t.Fatalf("N(R)=3 with V(S)=1 accepted: V(A)=%d unacked=%d", c.lastAcked, c.unacked)
	}
}

// Connect on a conn left over from an earlier session starts from clean state.
func TestConnectResetsReusedConn(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.SendData(c, 0xF0, []byte("a"))
	tbl.SendData(c, 0xF0, []byte("b"))
	tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	if c.unacked != 0 || len(c.retransmitBuf) != 0 || c.lastAcked != 0 {
		t.Fatalf("stale state after re-Connect: unacked=%d retransmitBuf=%d V(A)=%d",
			c.unacked, len(c.retransmitBuf), c.lastAcked)
	}
}

// Our XID response advertises the link's actual modulo.
func TestXIDResponseAdvertisesLinkModulo(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	connect(t, tbl, rec) // mod-8
	cmd := ax25.XIDParams{Modulo: 128, SREJ: ax25.SREJSingle, WindowRx: 7}
	xf := mkFrame(ax25.XID, "N0CALL-2", "KU0HN-10", pf)
	xf.Info = cmd.Encode(true)
	tbl.OnFrame(0, xf)
	if len(rec.sent) != 1 {
		t.Fatalf("XID reply = %+v", rec.sent)
	}
	p, err := ax25.ParseXID(rec.sent[0].Info)
	if err != nil || p.Modulo != 8 {
		t.Fatalf("XID response modulo = %d (err %v), want 8 on a mod-8 link", p.Modulo, err)
	}
}

// Queue overflow is reported to the caller instead of silently discarded.
func TestSendDataReportsDroppedChunks(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := connect(t, tbl, rec)
	c.remoteBusy = true // nothing drains
	big := make([]byte, (maxOutboundQueue+2)*maxIFrameInfo)
	if dropped := tbl.SendData(c, 0xF0, big); dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
}
