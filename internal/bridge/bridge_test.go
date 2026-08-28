package bridge

import (
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
	mu       sync.Mutex
	frames   [][]byte
	commands []struct {
		cmd uint8
		val []byte
	}
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
func (p *fakePort) SendCommand(cmdType uint8, value []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commands = append(p.commands, struct {
		cmd uint8
		val []byte
	}{cmdType, append([]byte{}, value...)})
}
func (p *fakePort) getCommands() []struct {
	cmd uint8
	val []byte
} {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]struct {
		cmd uint8
		val []byte
	}, len(p.commands))
	copy(out, p.commands)
	return out
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
	params[0].AX25Version = 22 // enable v2.2 so mod-128 SABME is accepted in tests
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
	b.rxFrames = make([]uint64, 1)
	b.txFrames = make([]uint64, 1)
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

	// Per tncd.py:1307-1328: H-bit on the SSID of via addresses is cleared.
	// Test that a frame sent WITH via H-bit clear is suppressed when received
	// WITH via H-bit set (digipeated echo).
	rawViaNoH := makeUIFrameWithVia("N0CALL-2", "KU0HN-10", "W1AW", false, []byte("world"))
	onLoop(t, eng, func() { b.SendToKISS(0, rawViaNoH) })

	// Digipeated version has H-bit set on the via SSID.
	rawViaWithH := makeUIFrameWithVia("N0CALL-2", "KU0HN-10", "W1AW", true, []byte("world"))
	onLoop(t, eng, func() { b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: rawViaWithH}) })

	if got := monClient.getSent(); len(got) != 0 {
		t.Fatalf("digipeated echo suppression failed: got %d frames, want 0", len(got))
	}
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

type fakeMonitorSink struct {
	port int
	typ  ax25.FrameType
	src  string
	dst  string
	n    int
}
func (s *fakeMonitorSink) OnRXFrame(port int, f *ax25.Frame) {
	s.n++; s.port = port; s.typ = f.Type; s.src = f.Src.String(); s.dst = f.Dst.String()
}

// TestMonitorDistribution: bus emits decoded frame to registered MonitorSink;
// wire-format ('U' AGWPE header) is verified in the agwpe package test.
func TestMonitorDistribution(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	var b *Bridge
	sink := &fakeMonitorSink{}
	raw := makeUIFrame("A", "B", []byte("hello")) // existing helper; 5-byte payload
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
		b.RegisterMonitorSink(sink)
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: raw})
	})
	if sink.n != 1 || sink.typ != ax25.UI || sink.src != "A" || sink.dst != "B" {
		t.Fatalf("emit = {n:%d typ:%v src:%s dst:%s}, want 1 UI A→B", sink.n, sink.typ, sink.src, sink.dst)
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

// TestForeignPollNoDMWithoutRegistration verifies the shared-channel courtesy
// fix at the bridge level: IsLocal is wired to RegisteredCalls, so a foreign
// I-frame P=1 must not trigger a DM until the destination callsign has been
// registered by a client.
func TestForeignPollNoDMWithoutRegistration(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)

	// Build bridge using InjectPorts so Hooks.IsLocal is wired correctly.
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8, IdleTimeout: 0},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 0},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}
	b := New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	onLoop(t, eng, func() {
		InjectPorts(b, eng, params, []PortSender{fp})
	})

	// Add a client with no registered callsigns.
	cl := newFakeClient(false) // no calls registered
	onLoop(t, eng, func() { b.AddClient(cl) })

	// Build a foreign I-frame P=1: MNWIN→WT9M-4. Neither party is registered.
	iFrame := &ax25.Frame{
		Src:     mustAddr("MNWIN"),
		Dst:     mustAddr("WT9M-4"),
		Type:    ax25.I,
		NS:      0,
		NR:      0,
		PF:      true,
		Command: true,
		PID:     0xF0,
		Info:    []byte("foreign"),
	}
	onLoop(t, eng, func() {
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: iFrame.Bytes()})
	})

	// No DM must have been sent — WT9M-4 is not our callsign.
	if got := fp.getSent(); len(got) != 0 {
		t.Fatalf("foreign I-frame: port sent %d frame(s), want 0", len(got))
	}

	// Now register WT9M-4 on the client (simulates the 'X' handler registering
	// the callsign after the client connects).
	onLoop(t, eng, func() {
		cl.mu.Lock()
		cl.registeredCalls["WT9M-4"] = true
		cl.mu.Unlock()
	})

	// Re-deliver the same I-frame P=1 addressed to WT9M-4 (our call now).
	// Must now produce a DM.
	onLoop(t, eng, func() {
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: iFrame.Bytes()})
	})

	sent := fp.getSent()
	if len(sent) == 0 {
		t.Fatal("expected DM after callsign registered, got nothing")
	}
	last, err := ax25.Parse(sent[len(sent)-1])
	if err != nil {
		t.Fatalf("parse last TX: %v", err)
	}
	if last.Type != ax25.DM {
		t.Fatalf("last TX type = %v, want DM", last.Type)
	}
}

// TestForeignSABMNoUA verifies that an incoming SABM addressed to a foreign
// callsign (not registered by any client) produces no TX and no connection.
// Follows TestForeignPollNoDMWithoutRegistration's pattern.
func TestForeignSABMNoUA(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)

	// Build bridge using InjectPorts so Hooks.IsLocal is wired correctly.
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8, IdleTimeout: 0},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 0},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}
	b := New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	onLoop(t, eng, func() {
		InjectPorts(b, eng, params, []PortSender{fp})
	})

	// Add a client with no registered callsigns — nothing is "ours".
	onLoop(t, eng, func() { b.AddClient(newFakeClient(false)) })

	// Build a SABM P=1 addressed to a foreign callsign W0NE-10.
	sabmFrame := &ax25.Frame{
		Src:     mustAddr("N0CALL-2"),
		Dst:     mustAddr("W0NE-10"),
		Type:    ax25.SABM,
		PF:      true,
		Command: true,
	}
	onLoop(t, eng, func() {
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: sabmFrame.Bytes()})
	})

	// No UA (and no DM) must have been sent — W0NE-10 is not our callsign.
	if got := fp.getSent(); len(got) != 0 {
		t.Fatalf("foreign SABM: port sent %d frame(s), want 0", len(got))
	}
}

// TestOnKISSFrameDecodesExtendedI verifies the two-stage RX decode: when a
// mod-128 connection is established via SABME, incoming extended I-frames
// (2-byte control field) are re-parsed at modulo 128 so the second control
// byte is not misread as the PID.
func TestOnKISSFrameDecodesExtendedI(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	var b *Bridge
	onLoop(t, eng, func() { b = makeBridge(t, eng, fp) })

	clientA := newFakeClient(false, "KU0HN-10")
	onLoop(t, eng, func() { b.AddClient(clientA) })

	// Incoming SABME establishes a mod-128 connection owned by clientA.
	sabme := &ax25.Frame{Src: mustAddr("N0CALL-2"), Dst: mustAddr("KU0HN-10"), Type: ax25.SABME, PF: true, Command: true}
	onLoop(t, eng, func() { b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: sabme.Bytes()}) })

	// Extended I-frame (2-byte control), N(S)=0 in sequence, carrying "hello".
	iframe := &ax25.Frame{
		Src: mustAddr("N0CALL-2"), Dst: mustAddr("KU0HN-10"),
		Type: ax25.I, Modulo: 128, NS: 0, NR: 0, Command: true,
		PID: 0xF0, Info: []byte("hello"),
	}
	onLoop(t, eng, func() { b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: iframe.Bytes()}) })

	// clientA should receive a 'D' data frame with exactly "hello".
	// (Without the two-stage fix, the mod-8 parse misreads the 2nd control byte
	// as PID and delivers "\xf0hello".)
	var gotData string
	for _, s := range clientA.getSent() {
		if s.kind == 'D' {
			gotData = string(s.data)
		}
	}
	if gotData != "hello" {
		t.Fatalf("delivered data = %q, want %q", gotData, "hello")
	}
}

func TestSendKISSCommandRoutesToPort(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	var b *Bridge
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
		b.SendKISSCommand(0, 0x01, []byte{40})
		b.SendKISSCommand(5, 0x01, []byte{40}) // out of range → no-op
	})
	cmds := fp.getCommands()
	if len(cmds) != 1 || cmds[0].cmd != 0x01 || len(cmds[0].val) != 1 || cmds[0].val[0] != 40 {
		t.Fatalf("commands = %+v, want one TXDELAY=40", cmds)
	}
}

// TestStatusPortsCounters verifies per-port frame counters and StatusPorts snapshot.
func TestStatusPortsCounters(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	var ports []PortStatus
	onLoop(t, eng, func() {
		b := makeBridge(t, eng, fp)
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: makeUIFrame("A", "B", []byte("hi"))})
		b.SendToKISS(0, makeUIFrame("A", "B", []byte("hi")))
		ports = b.StatusPorts()
	})
	if len(ports) != 1 {
		t.Fatalf("ports len = %d, want 1", len(ports))
	}
	if ports[0].RxFrames != 1 || ports[0].TxFrames != 1 || !ports[0].Online {
		t.Fatalf("counters wrong: %+v", ports[0])
	}
}

// TestSendKISSCommandOfflinePortSkipped verifies that SendKISSCommand is a
// no-op when the target port exists but is offline (!p.Online()).
func TestSendKISSCommandOfflinePortSkipped(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(false) // offline
	var b *Bridge
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp)
		b.SendKISSCommand(0, 0x01, []byte{40})
	})
	if cmds := fp.getCommands(); len(cmds) != 0 {
		t.Fatalf("offline port: got %d command(s), want 0", len(cmds))
	}
}

// TestReconnectPortGuards: ReconnectPort returns false for an out-of-range port
// and for a slot backed by something other than a live kiss.Port (there is no
// real transport to cycle).
func TestReconnectPortGuards(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fp := newFakePort(true)
	var b *Bridge
	var neg, oob, fake bool
	onLoop(t, eng, func() {
		b = makeBridge(t, eng, fp) // ports[0] is a *fakePort, not a *kiss.Port
		neg = b.ReconnectPort(-1)
		oob = b.ReconnectPort(5)
		fake = b.ReconnectPort(0)
	})
	if neg || oob {
		t.Fatalf("out-of-range ports must return false (neg=%v oob=%v)", neg, oob)
	}
	if fake {
		t.Fatalf("non-kiss.Port slot must return false")
	}
}

// TestRXWedged exercises the read-side wedge watchdog decision logic.
func TestRXWedged(t *testing.T) {
	const to = 20 * time.Second
	cases := []struct {
		name     string
		online   bool
		timeout  time.Duration
		activeTX bool
		since    time.Duration
		want     bool
	}{
		{"wedged: online, TX pending, long silence", true, to, true, 25 * time.Second, true},
		{"wedged at exactly the timeout", true, to, true, to, true},
		{"not wedged: offline", false, to, true, 25 * time.Second, false},
		{"not wedged: watchdog disabled (timeout 0)", true, 0, true, 25 * time.Second, false},
		{"not wedged: no unacked TX (idle link)", true, to, false, 25 * time.Second, false},
		{"not wedged: RX arrived recently", true, to, true, 5 * time.Second, false},
	}
	for _, tc := range cases {
		if got := rxWedged(tc.online, tc.timeout, tc.activeTX, tc.since); got != tc.want {
			t.Errorf("%s: rxWedged(online=%v, to=%v, tx=%v, since=%v) = %v, want %v",
				tc.name, tc.online, tc.timeout, tc.activeTX, tc.since, got, tc.want)
		}
	}
}
