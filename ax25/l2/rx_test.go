package l2

import (
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

func TestInSequenceIFrameDeliversAndT2Acks(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0), info([]byte("hi"))))
	if len(rec.data) != 1 || string(rec.data[0]) != "hi" {
		t.Fatalf("data = %v", rec.data)
	}
	if len(rec.sent) != 0 {
		t.Fatalf("RR sent immediately; must wait for T2. sent=%+v", rec.sent)
	}
	clk.advance(5 * time.Second) // > T2
	last := rec.sent[len(rec.sent)-1]
	if last.Type != ax25.RR || last.NR != 1 || last.PF || last.Command {
		t.Fatalf("T2 RR = %+v, want RR resp N(R)=1 F=0", last)
	}
	_ = c
}

func TestBurstYieldsSingleT2RR(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	connect(t, tbl, rec)
	for i := 0; i < 3; i++ {
		tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10",
			ns(uint8(i)), nr(0), info([]byte{byte(i)})))
	}
	clk.advance(5 * time.Second)
	rrs := 0
	var lastNR uint8
	for _, f := range rec.sent {
		if f.Type == ax25.RR {
			rrs++
			lastNR = f.NR
		}
	}
	if rrs != 1 || lastNR != 3 {
		t.Fatalf("rrs=%d lastNR=%d, want 1 RR with N(R)=3", rrs, lastNR)
	}
}

func TestIFramePollGetsImmediateGuardedRR(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	connect(t, tbl, rec)
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0), pf,
		info([]byte("x"))))
	last := rec.sent[len(rec.sent)-1]
	if last.Type != ax25.RR || !last.PF || last.NR != 1 {
		t.Fatalf("poll response = %+v", last)
	}
}

func TestDuplicateIFrameDiscardedRRGuarded(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	connect(t, tbl, rec)
	f := mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0), pf, info([]byte("x")))
	tbl.OnFrame(0, f)
	nData, nSent := len(rec.data), len(rec.sent)
	tbl.OnFrame(0, f) // exact duplicate, same NS, within 3s
	if len(rec.data) != nData {
		t.Fatal("duplicate delivered twice")
	}
	if len(rec.sent) != nSent {
		t.Fatal("RR guard failed: duplicate RR F=1 with same N(R) within 3s")
	}
}

func TestRRGuardExpiresAfter3s(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	connect(t, tbl, rec)
	f := mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0), pf, info([]byte("x")))
	tbl.OnFrame(0, f)
	nSent := len(rec.sent)
	clk.advance(3100 * time.Millisecond)
	tbl.OnFrame(0, f)
	if len(rec.sent) != nSent+1 {
		t.Fatal("RR should be sent again after guard window")
	}
}

func TestGapSendsREJ(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	connect(t, tbl, rec)
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(2), nr(0),
		info([]byte("future"))))
	last := rec.sent[len(rec.sent)-1]
	if last.Type != ax25.REJ || last.NR != 0 {
		t.Fatalf("gap response = %+v, want REJ N(R)=0", last)
	}
	if len(rec.data) != 0 {
		t.Fatal("out-of-sequence data delivered")
	}
}

func TestIFrameWhileConnectingPromotes(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0),
		info([]byte("early"))))
	if c.State != Connected || len(rec.connected) != 1 || len(rec.data) != 1 {
		t.Fatalf("state=%v connected=%v data=%v", c.State, rec.connected, rec.data)
	}
}

func TestUnknownIFrameWithPollGetsDM(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0), pf,
		info([]byte("stale"))))
	if len(rec.sent) != 1 || rec.sent[0].Type != ax25.DM {
		t.Fatalf("response = %+v, want DM", rec.sent)
	}
}

func TestPollResponseDeferredBehindQueuedIFrames(t *testing.T) {
	// The call_soon fix: RR P=1 arriving in the same burst as I-frames
	// must report the N(R) *after* those I-frames are processed.
	tbl, rec, _ := newHarness(1200)
	connect(t, tbl, rec)
	var deferred []func()
	tbl.Hooks().Defer = func(fn func()) { deferred = append(deferred, fn) }
	// Burst: RR P=1 processed first, then two I-frames (already queued).
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", nr(0), pf))
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(0), nr(0), info([]byte("a"))))
	tbl.OnFrame(0, mkFrame(ax25.I, "N0CALL-2", "KU0HN-10", ns(1), nr(0), info([]byte("b"))))
	for _, fn := range deferred {
		fn()
	}
	var rr *ax25.Frame
	for _, f := range rec.sent {
		if f.Type == ax25.RR && f.PF {
			rr = f
		}
	}
	if rr == nil || rr.NR != 2 {
		t.Fatalf("poll response = %+v, want RR F=1 N(R)=2", rr)
	}
}

func TestT3LivenessPoll(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	connect(t, tbl, rec)
	rec.sent = nil
	clk.advance(181 * time.Second)
	if len(rec.sent) == 0 || rec.sent[0].Type != ax25.RR || !rec.sent[0].PF ||
		!rec.sent[0].Command {
		t.Fatalf("T3 poll = %+v, want RR P=1 command", rec.sent)
	}
}

func TestPortOfflineDropsConns(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	connect(t, tbl, rec)
	tbl.PortOffline(0)
	if rec.disconnected != 1 || tbl.Get(0, "KU0HN-10", "N0CALL-2") != nil {
		t.Fatalf("disconnected=%d", rec.disconnected)
	}
}
