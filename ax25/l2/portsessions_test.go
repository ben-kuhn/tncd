package l2

import (
	"testing"

	"github.com/ben-kuhn/tncd/v2/ax25"
)

// sessionRec records PortSessions along with how many frames had been sent when
// each report arrived, which is how the ordering claim below is checked.
type sessionRec struct {
	counts    []int
	sentWhen  []int
	sentSoFar *int
}

func newSessionHarness() (*Table, *recorder, *sessionRec, *fakeClock) {
	clk := newFakeClock()
	rec := &recorder{}
	sent := 0
	sr := &sessionRec{sentSoFar: &sent}
	hooks := Hooks{
		SendAX25: func(port int, f *ax25.Frame) {
			rec.sent = append(rec.sent, f)
			sent++
		},
		Connected:    func(c *Conn, in bool) {},
		Data:         func(c *Conn, pid uint8, d []byte) {},
		Disconnected: func(c *Conn) { rec.disconnected++ },
		PortSessions: func(port int, n int) {
			sr.counts = append(sr.counts, n)
			sr.sentWhen = append(sr.sentWhen, sent)
		},
	}
	params := DeriveParams(1200, 3, 10, 180)
	return NewTable(clk, hooks, []PortParams{params, params}), rec, sr, clk
}

// TestPortSessionsFiresBeforeTheSABM is the property the whole session-settings
// trigger depends on: the radio has to be out of dual watch and on the right
// frequency BEFORE the handshake goes out, not after the UA comes back. Hooking
// Connected instead would configure the radio only once the connection already
// succeeded -- by which time the handshake it was supposed to help is over.
func TestPortSessionsFiresBeforeTheSABM(t *testing.T) {
	tbl, _, sr, _ := newSessionHarness()

	if _, err := tbl.Connect(0, "KU0HN", "KU0HN-10", nil); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(sr.counts) == 0 {
		t.Fatal("PortSessions never fired for an outgoing connect")
	}
	if sr.counts[0] != 1 {
		t.Errorf("first report = %d, want 1", sr.counts[0])
	}
	if sr.sentWhen[0] != 0 {
		t.Errorf("PortSessions fired after %d frames had already been sent, want 0 -- "+
			"the session must be reported before the SABM leaves", sr.sentWhen[0])
	}
}

// TestPortSessionsFiresForAnIncomingSABM: a station calling us is a session too,
// and the radio needs the same treatment.
func TestPortSessionsFiresForAnIncomingSABM(t *testing.T) {
	tbl, _, sr, _ := newSessionHarness()

	tbl.OnFrame(0, mkFrame(ax25.SABM, "KU0HN-10", "KU0HN", pf))
	if len(sr.counts) == 0 || sr.counts[0] != 1 {
		t.Fatalf("reports = %v, want a first report of 1 for an incoming SABM", sr.counts)
	}
}

// TestPortSessionsReportsZeroWhenTheConnectionGoesAway: without this the radio is
// never put back. Checked through a real DISC exchange rather than by calling
// remove directly, so it covers the path a session actually takes.
func TestPortSessionsReportsZeroWhenTheConnectionGoesAway(t *testing.T) {
	tbl, _, sr, _ := newSessionHarness()

	tbl.OnFrame(0, mkFrame(ax25.SABM, "KU0HN-10", "KU0HN", pf))
	tbl.OnFrame(0, mkFrame(ax25.DISC, "KU0HN-10", "KU0HN", pf))

	if len(sr.counts) < 2 {
		t.Fatalf("reports = %v, want at least a 1 and a 0", sr.counts)
	}
	if last := sr.counts[len(sr.counts)-1]; last != 0 {
		t.Errorf("last report = %d, want 0 after the connection was torn down", last)
	}
}

// TestPortSessionsCountsPerPort: settings are per radio, and ports have
// different radios. A session on port 1 must not report against port 0.
func TestPortSessionsCountsPerPort(t *testing.T) {
	clk := newFakeClock()
	byPort := map[int][]int{}
	hooks := Hooks{
		SendAX25:     func(port int, f *ax25.Frame) {},
		Connected:    func(c *Conn, in bool) {},
		Data:         func(c *Conn, pid uint8, d []byte) {},
		Disconnected: func(c *Conn) {},
		PortSessions: func(port int, n int) { byPort[port] = append(byPort[port], n) },
	}
	params := DeriveParams(1200, 3, 10, 180)
	tbl := NewTable(clk, hooks, []PortParams{params, params})

	if _, err := tbl.Connect(0, "KU0HN", "KU0HN-10", nil); err != nil {
		t.Fatalf("Connect port 0: %v", err)
	}
	if _, err := tbl.Connect(1, "KU0HN", "W0NE-10", nil); err != nil {
		t.Fatalf("Connect port 1: %v", err)
	}
	if got := byPort[0]; len(got) != 1 || got[0] != 1 {
		t.Errorf("port 0 reports = %v, want [1]", got)
	}
	if got := byPort[1]; len(got) != 1 || got[0] != 1 {
		t.Errorf("port 1 reports = %v, want [1] -- counts must not pool across ports", got)
	}
}

// TestPortSessionsCountsOverlappingConnections: several connections on one port
// share one radio, so the count has to be a count, not a flag.
func TestPortSessionsCountsOverlappingConnections(t *testing.T) {
	tbl, _, sr, _ := newSessionHarness()

	tbl.OnFrame(0, mkFrame(ax25.SABM, "KU0HN-10", "KU0HN", pf))
	tbl.OnFrame(0, mkFrame(ax25.SABM, "W0NE-10", "KU0HN", pf))
	if last := sr.counts[len(sr.counts)-1]; last != 2 {
		t.Errorf("last report = %d, want 2 for two connections on one port", last)
	}
	tbl.OnFrame(0, mkFrame(ax25.DISC, "KU0HN-10", "KU0HN", pf))
	if last := sr.counts[len(sr.counts)-1]; last != 1 {
		t.Errorf("last report = %d, want 1 -- one connection remains", last)
	}
}
