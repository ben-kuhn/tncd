package bridge

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// --- test helpers ---

// onLoop posts fn to the engine and blocks until it completes (or 2s timeout).
func onLoop(t *testing.T, e *engine.Engine, fn func()) {
	t.Helper()
	done := make(chan struct{})
	e.Do(func() { fn(); close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("engine stalled")
	}
}

// fakeClient implements Client for testing.
type fakeClient struct {
	mu              sync.Mutex
	sent            []agwpeSend
	monitoring      bool
	registeredCalls map[string]bool
	lastActivity    time.Time
	closed          bool
}

type agwpeSend struct {
	port uint8
	kind byte
	pid  uint8
	from string
	to   string
	data []byte
}

func newFakeClient(monitoring bool, calls ...string) *fakeClient {
	rc := make(map[string]bool)
	for _, c := range calls {
		rc[c] = true
	}
	return &fakeClient{
		monitoring:      monitoring,
		registeredCalls: rc,
		lastActivity:    time.Now(),
	}
}

func (c *fakeClient) SendAGWPE(port uint8, kind byte, pid uint8, from, to string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	c.sent = append(c.sent, agwpeSend{port, kind, pid, from, to, cp})
}

func (c *fakeClient) Monitoring() bool { return c.monitoring }
func (c *fakeClient) RegisteredCalls() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registeredCalls
}
func (c *fakeClient) LastActivity() time.Time { return c.lastActivity }
func (c *fakeClient) CloseTransport() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
}

func (c *fakeClient) getSent() []agwpeSend {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]agwpeSend, len(c.sent))
	copy(out, c.sent)
	return out
}

// fakePort implements PortSender for testing.
type fakePort struct {
	mu     sync.Mutex
	frames [][]byte
	online bool
}

func newFakePort(online bool) *fakePort { return &fakePort{online: online} }
func (p *fakePort) Send(raw []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(raw))
	copy(cp, raw)
	p.frames = append(p.frames, cp)
}
func (p *fakePort) Online() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.online
}
func (p *fakePort) getSent() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.frames))
	copy(out, p.frames)
	return out
}

// makeBridge builds a Bridge with a fake PortSender wired in, without
// starting real transports. The engine must already be running.
func makeBridge(t *testing.T, eng *engine.Engine, fp *fakePort) *Bridge {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8, IdleTimeout: 0},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 0},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}

	b := New(eng, cfg)

	// Wire L2 manually (skip real Start to avoid serial open).
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	hooks := l2pkg.Hooks{
		SendAX25: func(port int, f *ax25.Frame) {
			b.SendAX25(port, f)
		},
		Connected: func(c *l2pkg.Conn, incoming bool) {
			b.notifyConnected(c, incoming)
		},
		ConnectFailed: func(c *l2pkg.Conn, reason l2pkg.FailReason) {
			b.notifyConnectFailed(c, reason)
		},
		Data: func(c *l2pkg.Conn, pid uint8, data []byte) {
			b.notifyData(c, pid, data)
		},
		Disconnected: func(c *l2pkg.Conn) {
			b.notifyDisconnected(c)
		},
		Defer: func(fn func()) {
			eng.Do(fn)
		},
	}
	b.l2 = l2pkg.NewTable(eng, hooks, params)
	b.ports = []PortSender{fp}
	return b
}

// makeUIFrame builds a UI AX.25 frame with the given src/dst/data and returns
// the wire bytes.
func makeUIFrame(src, dst string, data []byte) []byte {
	s, _ := ax25.ParseAddress(src)
	d, _ := ax25.ParseAddress(dst)
	f := &ax25.Frame{
		Src:     s,
		Dst:     d,
		Type:    ax25.UI,
		Command: true,
		PID:     0xF0,
		Info:    data,
	}
	return f.Bytes()
}

// --- tests ---

// TestEchoSuppression: frames sent via SendToKISS are not delivered to monitors.
// A digipeated echo (same frame with H-bit set on via SSID) is also suppressed.
func TestEchoSuppression(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	var b *Bridge
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
	})

	monClient := newFakeClient(true)
	onLoop(t, eng, func() { b.AddClient(monClient) })

	// Build a UI frame.
	raw := makeUIFrame("N0CALL-2", "KU0HN-10", []byte("hello"))

	// Send via SendToKISS — this tracks it in the echo ring.
	onLoop(t, eng, func() { b.SendToKISS(0, raw) })

	// Now deliver as an RX frame — should be suppressed.
	onLoop(t, eng, func() { b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: raw}) })

	if got := monClient.getSent(); len(got) != 0 {
		t.Fatalf("echo suppression failed: got %d frames, want 0", len(got))
	}

	// Build a digipeated version: same frame but with via address and H-bit set.
	// We do this by constructing the frame with a via address that has H-bit set.
	rawWithVia := makeUIFrameWithVia("N0CALL-2", "KU0HN-10", "W1AW", true, []byte("hello"))

	// This is a digipeated echo — should also be suppressed because normalizeHBits
	// will match the original (the via H-bit gets cleared in normalization).
	// First we need the normalised original to be in the ring, and we need the
	// normalised digipeated version to match. But the original was sent with no
	// via, so the normalized forms differ. Let's instead send the via-frame and
	// receive the normalized version.
	//
	// Per tncd.py:1307-1328: H-bit on the SSID of via addresses is cleared.
	// We need to test that a frame sent WITHOUT a via matches when received
	// WITH a via + H-bit. Build the raw-with-via so normalizing it gives same
	// result as normalizing raw-without-via, which isn't possible (different
	// address counts). The correct test is: send frame WITH via H-bit clear,
	// receive WITH via H-bit set → suppressed.
	rawViaNoH := makeUIFrameWithVia("N0CALL-2", "KU0HN-10", "W1AW", false, []byte("world"))
	onLoop(t, eng, func() { b.SendToKISS(0, rawViaNoH) })

	// Digipeated version has H-bit set on the via SSID.
	rawViaWithH := makeUIFrameWithVia("N0CALL-2", "KU0HN-10", "W1AW", true, []byte("world"))
	onLoop(t, eng, func() { b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: rawViaWithH}) })

	if got := monClient.getSent(); len(got) != 0 {
		t.Fatalf("digipeated echo suppression failed: got %d frames, want 0", len(got))
	}
	_ = rawWithVia
}

// makeUIFrameWithVia builds a UI AX.25 frame with one via address.
// If hBit is true, bit 7 of the via's SSID byte is set (H-bit = has been repeated).
func makeUIFrameWithVia(src, dst, via string, hBit bool, data []byte) []byte {
	s, _ := ax25.ParseAddress(src)
	d, _ := ax25.ParseAddress(dst)
	v, _ := ax25.ParseAddress(via)
	if hBit {
		// Set H-bit: in ax25.Address, CRH field maps to bit 7 of the SSID byte.
		v.CRH = true
	}
	f := &ax25.Frame{
		Src:     s,
		Dst:     d,
		Via:     []ax25.Address{v},
		Type:    ax25.UI,
		Command: true,
		PID:     0xF0,
		Info:    data,
	}
	return f.Bytes()
}

// TestMonitorDistribution: monitoring clients get 'U' with the expected prefix;
// non-monitoring clients get nothing.
func TestMonitorDistribution(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	var b *Bridge
	onLoop(t, eng, func() { b = makeBridge(t, eng, fp) })

	monClient := newFakeClient(true)
	nonMon := newFakeClient(false)
	onLoop(t, eng, func() {
		b.AddClient(monClient)
		b.AddClient(nonMon)
	})

	raw := makeUIFrame("A", "B", []byte("hello"))

	// Don't send via SendToKISS (would echo-suppress); deliver directly.
	onLoop(t, eng, func() { b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: raw}) })

	// Check monitoring client got 'U' with the right prefix.
	got := monClient.getSent()
	if len(got) != 1 {
		t.Fatalf("monitor client got %d frames, want 1", len(got))
	}
	s := got[0]
	if s.kind != 'U' {
		t.Fatalf("kind = %c, want U", s.kind)
	}
	prefix := "Fm A To B <UI pid=F0 Len=5 >["
	if !strings.HasPrefix(string(s.data), prefix) {
		t.Fatalf("data prefix mismatch:\ngot:  %q\nwant: %q...", string(s.data), prefix)
	}

	// Non-monitoring client must receive nothing.
	if got2 := nonMon.getSent(); len(got2) != 0 {
		t.Fatalf("non-monitor client got %d frames, want 0", len(got2))
	}
}

// TestIncomingConnectOwnerByRegistration: when SABM arrives, the client whose
// RegisteredCalls contains the local callsign becomes the owner and receives
// the 'C' notification.
func TestIncomingConnectOwnerByRegistration(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	var b *Bridge
	onLoop(t, eng, func() { b = makeBridge(t, eng, fp) })

	// Client A has KU0HN-10 registered; client B has nothing.
	clientA := newFakeClient(false, "KU0HN-10")
	clientB := newFakeClient(false)
	onLoop(t, eng, func() {
		b.AddClient(clientA)
		b.AddClient(clientB)
	})

	// Deliver incoming SABM: remote=N0CALL-2, local=KU0HN-10.
	sabm := &ax25.Frame{
		Src:     mustAddr("N0CALL-2"),
		Dst:     mustAddr("KU0HN-10"),
		Type:    ax25.SABM,
		PF:      true,
		Command: true,
	}
	onLoop(t, eng, func() {
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: sabm.Bytes()})
	})

	// Client A should get the 'C' notification with "*** CONNECTED To Station N0CALL-2\r".
	gotA := clientA.getSent()
	if len(gotA) != 1 {
		t.Fatalf("clientA got %d frames, want 1", len(gotA))
	}
	if gotA[0].kind != 'C' {
		t.Fatalf("clientA frame kind = %c, want C", gotA[0].kind)
	}
	wantMsg := "*** CONNECTED To Station N0CALL-2\r"
	if string(gotA[0].data) != wantMsg {
		t.Fatalf("clientA msg:\ngot:  %q\nwant: %q", string(gotA[0].data), wantMsg)
	}

	// Client B must receive nothing.
	if got := clientB.getSent(); len(got) != 0 {
		t.Fatalf("clientB got %d frames, want 0", len(got))
	}
}

// TestClientRemovalDisconnectsOwned: when the owning client is removed,
// a DISC command frame must reach the (fake) port TX.
func TestClientRemovalDisconnectsOwned(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	var b *Bridge
	onLoop(t, eng, func() { b = makeBridge(t, eng, fp) })

	cl := newFakeClient(false, "KU0HN-10")
	onLoop(t, eng, func() { b.AddClient(cl) })

	// Bring up a connection via incoming SABM so the client owns it.
	sabm := &ax25.Frame{
		Src:     mustAddr("N0CALL-2"),
		Dst:     mustAddr("KU0HN-10"),
		Type:    ax25.SABM,
		PF:      true,
		Command: true,
	}
	onLoop(t, eng, func() {
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: sabm.Bytes()})
	})

	// Verify we have a Connected connection.
	onLoop(t, eng, func() {
		c := b.l2.Get(0, "KU0HN-10", "N0CALL-2")
		if c == nil {
			t.Errorf("no connection after SABM")
		}
	})

	// Count frames before removal (UA was sent in response to SABM).
	beforeCount := len(fp.getSent())

	// Remove the owning client — should trigger DISC.
	onLoop(t, eng, func() { b.RemoveClient(cl) })

	after := fp.getSent()
	if len(after) <= beforeCount {
		t.Fatalf("no DISC sent: got %d total frames (had %d before removal)", len(after), beforeCount)
	}

	// The last frame should be a DISC.
	lastRaw := after[len(after)-1]
	parsed, err := ax25.Parse(lastRaw)
	if err != nil {
		t.Fatalf("parse DISC: %v", err)
	}
	if parsed.Type != ax25.DISC {
		t.Fatalf("last frame type = %v, want DISC", parsed.Type)
	}
}

// TestMaxClients: AddClient returns false at max_clients.
func TestMaxClients(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	cfg := &config.Config{
		Server: config.Server{MaxClients: 2},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}
	b := New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	hooks := l2pkg.Hooks{}
	b.l2 = l2pkg.NewTable(eng, hooks, params)
	b.ports = []PortSender{fp}

	var ok1, ok2, ok3 bool
	onLoop(t, eng, func() {
		ok1 = b.AddClient(newFakeClient(false))
		ok2 = b.AddClient(newFakeClient(false))
		ok3 = b.AddClient(newFakeClient(false))
	})

	if !ok1 || !ok2 {
		t.Fatal("first two AddClient calls should succeed")
	}
	if ok3 {
		t.Fatal("third AddClient should return false (max_clients=2)")
	}
}

// mustAddr parses an AX.25 address and panics on failure.
func mustAddr(s string) ax25.Address {
	a, err := ax25.ParseAddress(s)
	if err != nil {
		panic("mustAddr: " + err.Error())
	}
	return a
}
