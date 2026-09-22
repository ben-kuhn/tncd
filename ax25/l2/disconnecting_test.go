package l2

// disconnecting_test.go — lifecycle of the Disconnecting state (DISC sent,
// UA/DM awaited). Regression tests for the 2026-09 audit findings:
//
//   - t1Expired has no Disconnecting branch, so a conn whose DISC goes
//     unanswered is never retransmitted (Dire Wolf retries DISC on T1) and
//     NEVER reaped: the timer fires once, returns early, and is not re-armed.
//     The entry zombies in the table until the owning client quits or the
//     process restarts.
//   - PortOffline removes only Connected/Connecting conns, so Disconnecting
//     conns on a dead port also zombie — and there the UA can *never* arrive
//     (a wedged/offline port transmits nothing), so local reaping is the only
//     possible cleanup.
//
// These tests encode the REQUIRED behavior and fail against the pre-fix code.
// Fix guidance: mirror the Connecting branch (retransmit DISC on T1, give up
// after N2 polls, remove the conn, fire Disconnected) and have PortOffline
// reap Disconnecting conns locally without expecting anything to reach the air.

import (
	"fmt"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

func countSent(rec *recorder, typ ax25.FrameType) int {
	n := 0
	for _, f := range rec.sent {
		if f.Type == typ {
			n++
		}
	}
	return n
}

// TestDisconnectingRetransmitsAndReaps: a DISC that never gets its UA must be
// retransmitted on T1 (AX.25 connected-mode recovery, as Dire Wolf does) and
// the conn must be removed after N2 unanswered attempts. Pre-fix, neither
// happens: one DISC goes out and the entry sits in the table forever.
func TestDisconnectingRetransmitsAndReaps(t *testing.T) {
	tbl, rec, clk := newHarness(1200) // T1 ~13s, N2Retry=10

	c, err := tbl.Connect(0, "LOCAL-1", "REMOTE-2", nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tbl.Disconnect(c)
	if got := countSent(rec, ax25.DISC); got != 1 {
		t.Fatalf("setup: DISC count = %d, want 1", got)
	}

	// Burn through well past N2 T1 intervals.
	for i := 0; i < 15; i++ {
		clk.advance(20 * time.Second)
	}

	if got := countSent(rec, ax25.DISC); got < 2 {
		t.Errorf("DISC was never retransmitted on T1 (got %d DISC total); "+
			"Dire Wolf retries DISC like SABM so a lost DISC doesn't wedge the link", got)
	}
	if zombie := tbl.Get(0, "LOCAL-1", "REMOTE-2"); zombie != nil {
		t.Errorf("Disconnecting conn was never reaped (state=%s); it will occupy a "+
			"connection-table slot until restart", zombie.State)
	}
}

// TestPortOfflineReapsDisconnecting: when the port dies mid-disconnect the UA
// can never arrive, so PortOffline must remove Disconnecting conns too. This
// is purely local table hygiene — no DISC needs to (or could) reach the air.
func TestPortOfflineReapsDisconnecting(t *testing.T) {
	tbl, _, _ := newHarness(1200)

	c, err := tbl.Connect(0, "LOCAL-1", "REMOTE-2", nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tbl.Disconnect(c)

	tbl.PortOffline(0)

	if zombie := tbl.Get(0, "LOCAL-1", "REMOTE-2"); zombie != nil {
		t.Errorf("PortOffline left a Disconnecting conn behind (state=%s); "+
			"with the port dead its UA can never arrive, so this entry zombies", zombie.State)
	}
}

// TestDisconnectingZombiesFillConnectionTable: the table is capped at
// maxConnections; unreaped Disconnecting entries must not be able to exhaust
// it and block all future sessions. Fails pre-fix (64 zombies → the 65th
// connect is refused); passes once Disconnecting conns are reaped on T1/N2.
func TestDisconnectingZombiesFillConnectionTable(t *testing.T) {
	tbl, _, clk := newHarness(1200)

	for i := 0; i < maxConnections; i++ {
		remote := fmt.Sprintf("R%05d", i) // 6 chars, valid AX.25 call shape
		c, err := tbl.Connect(0, "LOCAL-1", remote, nil)
		if err != nil {
			t.Fatalf("Connect #%d: %v", i, err)
		}
		tbl.Disconnect(c) // Disconnecting; peer will never answer
	}

	// Every conn's T1/N2 cycle runs to completion.
	for i := 0; i < 15; i++ {
		clk.advance(20 * time.Second)
	}

	if _, err := tbl.Connect(0, "LOCAL-1", "ZZZZZ-1", nil); err != nil {
		t.Errorf("connection table exhausted by unreaped Disconnecting entries: %v", err)
	}
}
