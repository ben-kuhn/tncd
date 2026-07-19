package l2

import (
	"sort"
	"sync"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

// fakeClock: manual time, synchronous timers fired via advance().
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}
type fakeTimer struct {
	at        time.Time
	fn        func()
	cancelled bool
}

func newFakeClock() *fakeClock      { return &fakeClock{now: time.Unix(1000, 0)} }
func (c *fakeClock) Now() time.Time { return c.now }
func (c *fakeClock) After(d time.Duration, fn func()) *engine.Timer {
	ft := &fakeTimer{at: c.now.Add(d), fn: fn}
	c.timers = append(c.timers, ft)
	return engine.NewManualTimer(&ft.cancelled) // see Step 3 note
}
func (c *fakeClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
	// Sort by deadline so timers fire in chronological order.  Without this,
	// T3 (registered before T1) could cancel T1 before T1 fires, even when T1
	// has an earlier deadline — producing incorrect test results once T3 was
	// introduced in Task 10.
	sorted := make([]*fakeTimer, len(c.timers))
	copy(sorted, c.timers)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].at.Before(sorted[j].at)
	})
	for _, ft := range sorted {
		if !ft.cancelled && !ft.at.After(c.now) {
			ft.cancelled = true // fire once
			ft.fn()
		}
	}
}

// recorder captures hook invocations and sent frames.
type recorder struct {
	sent         []*ax25.Frame
	connected    []string // "<remote>:incoming|outgoing"
	data         [][]byte
	disconnected int
	failed       int
}

func newHarness(otaBaud int) (*Table, *recorder, *fakeClock) {
	clk := newFakeClock()
	rec := &recorder{}
	hooks := Hooks{
		SendAX25: func(port int, f *ax25.Frame) { rec.sent = append(rec.sent, f) },
		Connected: func(c *Conn, in bool) {
			dir := "outgoing"
			if in {
				dir = "incoming"
			}
			rec.connected = append(rec.connected, c.Remote+":"+dir)
		},
		ConnectFailed: func(c *Conn, _ FailReason) { rec.failed++ },
		Data:          func(c *Conn, pid uint8, d []byte) { rec.data = append(rec.data, d) },
		Disconnected:  func(c *Conn) { rec.disconnected++ },
	}
	params := DeriveParams(otaBaud, 3, 10, 180)
	return NewTable(clk, hooks, []PortParams{params, params}), rec, clk
}

// mkFrame builds an inbound frame for tests.
func mkFrame(typ ax25.FrameType, src, dst string, opts ...func(*ax25.Frame)) *ax25.Frame {
	s, _ := ax25.ParseAddress(src)
	d, _ := ax25.ParseAddress(dst)
	f := &ax25.Frame{Type: typ, Src: s, Dst: d, Command: true, PID: 0xF0}
	for _, o := range opts {
		o(f)
	}
	return f
}
func pf(f *ax25.Frame)                { f.PF = true }
func resp(f *ax25.Frame)              { f.Command = false }
func nr(n uint8) func(*ax25.Frame)    { return func(f *ax25.Frame) { f.NR = n } }
func ns(n uint8) func(*ax25.Frame)    { return func(f *ax25.Frame) { f.NS = n } }
func info(b []byte) func(*ax25.Frame) { return func(f *ax25.Frame) { f.Info = b } }
