package l2

import (
	"bytes"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

// connect establishes an outgoing Connected session.
func connect(t *testing.T, tbl *Table, rec *recorder) *Conn {
	t.Helper()
	c, _ := tbl.Connect(0, "KU0HN-10", "N0CALL-2", nil)
	tbl.OnFrame(0, mkFrame(ax25.UA, "N0CALL-2", "KU0HN-10", pf, resp))
	rec.sent = nil
	return c
}

func iframes(rec *recorder) []*ax25.Frame {
	var out []*ax25.Frame
	for _, f := range rec.sent {
		if f.Type == ax25.I {
			out = append(out, f)
		}
	}
	return out
}

func TestWindowLimitsInFlight(t *testing.T) {
	tbl, rec, _ := newHarness(1200) // MaxWindow=3
	c := connect(t, tbl, rec)
	for i := 0; i < 6; i++ {
		tbl.SendData(c, 0xF0, bytes.Repeat([]byte{byte('A' + i)}, 256))
	}
	if got := len(iframes(rec)); got != 3 {
		t.Fatalf("in flight = %d, want 3 (window)", got)
	}
	if tbl.Outstanding(c) != 6 { // 3 unacked + 3 queued
		t.Fatalf("Outstanding = %d, want 6", tbl.Outstanding(c))
	}
	// ACK two → two more drain.
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", resp, nr(2)))
	if got := len(iframes(rec)); got != 5 {
		t.Fatalf("after ack, sent = %d, want 5", got)
	}
}

func TestCoalescingSmallChunks(t *testing.T) {
	// PAT-style 127-byte writes coalesce into 254-byte I-frames.
	tbl, rec, _ := newHarness(1200)
	c := connect(t, tbl, rec)
	// Fill the window first so subsequent sends queue up.
	tbl.SendData(c, 0xF0, bytes.Repeat([]byte("w"), 256*3))
	rec.sent = nil
	for i := 0; i < 4; i++ {
		tbl.SendData(c, 0xF0, bytes.Repeat([]byte{byte('a' + i)}, 127))
	}
	// Ack everything in flight; queued 127s should coalesce pairwise.
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", resp, nr(3)))
	fr := iframes(rec)
	if len(fr) != 2 || len(fr[0].Info) != 254 || len(fr[1].Info) != 254 {
		sizes := []int{}
		for _, f := range fr {
			sizes = append(sizes, len(f.Info))
		}
		t.Fatalf("frames = %d sizes %v, want 2x254", len(fr), sizes)
	}
}

func TestBackwardsNRIgnored(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.SendData(c, 0xF0, bytes.Repeat([]byte("x"), 256*2))
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", resp, nr(2)))
	if tbl.Outstanding(c) != 0 {
		t.Fatalf("Outstanding = %d", tbl.Outstanding(c))
	}
	// Stale retransmitted RR with old N(R)=1: (1-2) mod 8 = 7 > window → ignore.
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", resp, nr(1)))
	if tbl.Outstanding(c) != 0 || c.State != Connected {
		t.Fatal("backwards N(R) corrupted state")
	}
	_ = rec
}

func TestT1PollThenRetransmit(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.SendData(c, 0xF0, []byte("payload"))
	rec.sent = nil
	clk.advance(120 * time.Second) // first T1: poll only
	if n := len(iframes(rec)); n != 0 {
		t.Fatalf("first expiry retransmitted %d I-frames, want 0", n)
	}
	last := rec.sent[len(rec.sent)-1]
	if last.Type != ax25.RR || !last.PF || !last.Command {
		t.Fatalf("expected RR P=1 poll, got %+v", last)
	}
	clk.advance(120 * time.Second) // second T1: poll + retransmit
	if n := len(iframes(rec)); n != 1 {
		t.Fatalf("second expiry retransmitted %d, want 1", n)
	}
}

func TestN2DisconnectsAndNotifies(t *testing.T) {
	tbl, rec, clk := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.SendData(c, 0xF0, []byte("payload"))
	for i := 0; i < 12; i++ {
		clk.advance(600 * time.Second)
	}
	if rec.disconnected != 1 || tbl.Get(0, "KU0HN-10", "N0CALL-2") != nil {
		t.Fatalf("disconnected=%d", rec.disconnected)
	}
}

func TestREJTriggersRetransmitWithUpdatedNR(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.SendData(c, 0xF0, bytes.Repeat([]byte("x"), 256*2)) // NS 0,1 in flight
	// Remote receives an I-frame from us... then REJs from 0.
	rec.sent = nil
	tbl.OnFrame(0, mkFrame(ax25.REJ, "N0CALL-2", "KU0HN-10", resp, nr(0)))
	fr := iframes(rec)
	if len(fr) != 2 || fr[0].NS != 0 || fr[1].NS != 1 {
		t.Fatalf("retransmit = %+v", fr)
	}
}

func TestRNRPausesAndRRResumes(t *testing.T) {
	tbl, rec, _ := newHarness(1200)
	c := connect(t, tbl, rec)
	tbl.OnFrame(0, mkFrame(ax25.RNR, "N0CALL-2", "KU0HN-10", resp, nr(0)))
	tbl.SendData(c, 0xF0, []byte("held"))
	if len(iframes(rec)) != 0 {
		t.Fatal("I-frame sent while remote busy")
	}
	tbl.OnFrame(0, mkFrame(ax25.RR, "N0CALL-2", "KU0HN-10", resp, nr(0)))
	if len(iframes(rec)) != 1 {
		t.Fatal("I-frame not released after RR")
	}
}
