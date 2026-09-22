package l2

import (
	"testing"
	"time"
)

func TestSnapshotActiveOnly(t *testing.T) {
	tbl := &Table{conns: map[connKey]*Conn{}}
	// One connected, one disconnected, one disconnecting.
	c := newConn(0, "KU0HN-10", "W0NE-10")
	c.State = Connected
	c.incoming = true
	c.modulo = 128
	c.sendSeq = 5
	c.recvSeq = 3
	c.unacked = 2
	c.outQueue = []outEntry{{pid: 0xF0, data: []byte{1}}}
	c.t1Polls = 1
	c.remoteBusy = true
	c.srtt = 1200 * time.Millisecond
	c.srejEnabled = true
	c.Via = []string{"W0NE-7"}
	tbl.conns[makeKey(0, "KU0HN-10", "W0NE-10")] = c

	d := newConn(0, "AAA", "BBB") // Disconnected (default)
	tbl.conns[makeKey(0, "AAA", "BBB")] = d

	dc := newConn(0, "CCC", "DDD")
	dc.State = Disconnecting // DISC sent, awaiting UA — must appear so the
	// RX-wedge watchdog counts it as awaiting a reply.
	tbl.conns[makeKey(0, "CCC", "DDD")] = dc

	snap := tbl.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (active incl. disconnecting)", len(snap))
	}
	// Snapshot iterates the conns map, so order is nondeterministic — find the
	// Connected conn rather than assuming snap[0].
	var got ConnInfo
	for _, s := range snap {
		if s.Local == "KU0HN-10" {
			got = s
			break
		}
	}
	if got.Local != "KU0HN-10" || got.Remote != "W0NE-10" || got.State != "connected" ||
		!got.Incoming || got.Modulo != 128 || got.SendSeq != 5 || got.RecvSeq != 3 ||
		got.Unacked != 2 || got.SendQueue != 1 || got.T1Retries != 1 || !got.RemoteBusy ||
		got.SRTTms != 1200 || !got.SREJ || len(got.Via) != 1 || got.Via[0] != "W0NE-7" {
		t.Fatalf("snapshot fields wrong: %+v", got)
	}
}
