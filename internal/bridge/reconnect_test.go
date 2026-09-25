package bridge

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// reconnFakeTransport is a kiss.Transport whose Open count is observable and
// whose Read blocks until Close, so a started port stays "online" until torn
// down. Reused across reconnect attempts (one shared readCh).
type reconnFakeTransport struct {
	opens  int32
	once   sync.Once
	readCh chan struct{}
}

func newReconnFake() *reconnFakeTransport {
	return &reconnFakeTransport{readCh: make(chan struct{})}
}

func (f *reconnFakeTransport) Open() error                 { atomic.AddInt32(&f.opens, 1); return nil }
func (f *reconnFakeTransport) EnterKISS() error            { return nil }
func (f *reconnFakeTransport) ExitKISS()                   {}
func (f *reconnFakeTransport) Write(p []byte) (int, error) { return len(p), nil }
func (f *reconnFakeTransport) Read(p []byte) (int, error)  { <-f.readCh; return 0, io.EOF }
func (f *reconnFakeTransport) Close() error {
	f.once.Do(func() { close(f.readCh) })
	return nil
}
func (f *reconnFakeTransport) openCount() int { return int(atomic.LoadInt32(&f.opens)) }

func waitFor(t *testing.T, cond func() bool, d time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func reconnCfg(reconnect bool) *config.Config {
	return &config.Config{
		Server: config.Server{ListenHost: "127.0.0.1", ListenPort: 0, Callsign: "TEST", MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 180},
		Ports: []config.Port{{
			Name: "Port 0", Type: "serial", Device: "/dev/fake",
			OTABaudrate: 1200, AX25Version: 22,
			Reconnect: reconnect, ReconnectDelay: 0.01, ReconnectMaxDelay: 0.05,
		}},
	}
}

// TestSerialPortReconnectsAfterOffline proves a serial port with reconnect=true
// reopens after going offline (previously only bluetooth ports reconnected).
func TestSerialPortReconnectsAfterOffline(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(true))
	ft := newReconnFake()
	b.newTransport = func(config.Port) (kiss.Transport, error) { return ft, nil }

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, func() bool { return ft.openCount() >= 1 }, 2*time.Second, "initial open")

	// Simulate the reader detecting a disconnect.
	onLoop(t, eng, func() { b.portWentOffline(0) })

	waitFor(t, func() bool { return ft.openCount() >= 2 }, 2*time.Second, "reconnect open")
}

// TestPortNoReconnectWhenDisabled proves reconnect=false opts out: no reopen
// after going offline.
func TestPortNoReconnectWhenDisabled(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(false))
	ft := newReconnFake()
	b.newTransport = func(config.Port) (kiss.Transport, error) { return ft, nil }

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, func() bool { return ft.openCount() >= 1 }, 2*time.Second, "initial open")

	onLoop(t, eng, func() { b.portWentOffline(0) })

	// Give any (erroneous) reconnect time to fire; opens must stay at 1.
	time.Sleep(200 * time.Millisecond)
	if got := ft.openCount(); got != 1 {
		t.Fatalf("reconnect happened despite reconnect=false: open count = %d", got)
	}
}

// gateFakeTransport is a kiss.Transport whose Open optionally blocks until
// released, simulating a slow dial (a Bluetooth ConnectProfile can take 30s).
// Close unblocks Read and records that it ran.
type gateFakeTransport struct {
	opened chan struct{} // closed when Open is entered
	gate   chan struct{} // non-nil: Open blocks until this is closed
	readCh chan struct{} // Close closes it, unblocking Read with EOF

	openOnce  sync.Once
	closeOnce sync.Once
	closes    int32
	writes    int32
}

func newGateFake(gated bool) *gateFakeTransport {
	f := &gateFakeTransport{
		opened: make(chan struct{}),
		readCh: make(chan struct{}),
	}
	if gated {
		f.gate = make(chan struct{})
	}
	return f
}

func (f *gateFakeTransport) Open() error {
	f.openOnce.Do(func() { close(f.opened) })
	if f.gate != nil {
		<-f.gate
	}
	return nil
}
func (f *gateFakeTransport) EnterKISS() error { return nil }
func (f *gateFakeTransport) ExitKISS()        {}
func (f *gateFakeTransport) Write(p []byte) (int, error) {
	atomic.AddInt32(&f.writes, 1)
	return len(p), nil
}
func (f *gateFakeTransport) Read(p []byte) (int, error) { <-f.readCh; return 0, io.EOF }
func (f *gateFakeTransport) Close() error {
	f.closeOnce.Do(func() { close(f.readCh) })
	atomic.AddInt32(&f.closes, 1)
	return nil
}
func (f *gateFakeTransport) closeCount() int { return int(atomic.LoadInt32(&f.closes)) }
func (f *gateFakeTransport) writeCount() int { return int(atomic.LoadInt32(&f.writes)) }
func (f *gateFakeTransport) openEntered() bool {
	select {
	case <-f.opened:
		return true
	default:
		return false
	}
}

// gateFactory hands out fakes in order, one per dial.
func gateFactory(t *testing.T, fakes ...*gateFakeTransport) func(config.Port) (kiss.Transport, error) {
	t.Helper()
	var calls int32
	return func(config.Port) (kiss.Transport, error) {
		n := atomic.AddInt32(&calls, 1)
		if int(n) > len(fakes) {
			return nil, fmt.Errorf("unexpected extra dial #%d", n)
		}
		return fakes[n-1], nil
	}
}

// portOnline polls the port's online flag from the engine loop.
func portOnline(eng *engine.Engine, b *Bridge, port int) bool {
	done := make(chan bool, 1)
	eng.Do(func() { done <- b.PortOnline(port) })
	select {
	case v := <-done:
		return v
	case <-time.After(time.Second):
		return false
	}
}

// TestStaleReconnectDoesNotOrphanLivePort — 2026-09 audit finding. When an
// auto-reconnect dial is still in flight (a Bluetooth connect can block for
// tens of seconds) and a manual relink (or a wedge relink) succeeds first,
// the stale dial eventually completes and posts b.ports[idx] = itsPort
// unconditionally: it overwrites the healthy relinked port and ORPHANS it —
// the orphan's reader/writer goroutines keep running and its onFrame callback
// keeps feeding the bridge, so two decoders split one KISS byte stream (or,
// for type=tcp against Dire Wolf, every frame is delivered twice).
//
// Required behavior: a stale connect completion notices the slot has been
// refilled by a newer attempt, tears its own fresh port down, and leaves the
// live one alone. This test fails pre-fix.
func TestStaleReconnectDoesNotOrphanLivePort(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(true))

	f1 := newGateFake(false) // initial link
	f2 := newGateFake(true)  // auto-reconnect: Open blocks until released
	f3 := newGateFake(false) // manual relink: opens immediately
	b.newTransport = gateFactory(t, f1, f2, f3)

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, f1.openEntered, 2*time.Second, "initial open")

	// Link drops: reader EOF → portWentOffline → auto-reconnect armed (10ms).
	f1.Close()

	// The auto-reconnect dials f2 and parks inside its Open.
	waitFor(t, f2.openEntered, 2*time.Second, "auto-reconnect dial")

	// Operator hits "relink" while the auto-reconnect is still dialling.
	onLoop(t, eng, func() {
		if !b.ReconnectPort(0) {
			t.Errorf("manual relink rejected")
		}
	})
	waitFor(t, f3.openEntered, 2*time.Second, "manual relink dial")
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "relink online")

	// The stale auto-reconnect dial finally completes.
	close(f2.gate)
	time.Sleep(200 * time.Millisecond) // let it post its slot assignment

	if f2.closeCount() == 0 {
		t.Errorf("stale auto-reconnect completion was not discarded: it overwrote the " +
			"live relinked port and its transport was never closed (orphaned port keeps " +
			"reading from the TNC)")
	}
	if !portOnline(eng, b, 0) {
		t.Errorf("port offline after relink; the live relink should have survived")
	}

	// A full shutdown must close every transport that was ever opened. Pre-fix
	// the orphaned relink port is untracked and leaks (goroutines + socket).
	onLoop(t, eng, func() { b.Shutdown() })
	if f3.closeCount() == 0 {
		t.Errorf("relinked port orphaned: never closed (not tracked in the slot)")
	}
}

// connEventSink records bridge ConnEvents. Engine-loop only.
type connEventSink struct{ events []ConnEvent }

func (s *connEventSink) OnConn(e ConnEvent) { s.events = append(s.events, e) }

// TestWedgeRelinkPreservesSession locks in the Bluetooth RX-wedge recovery
// contract (bridge.checkRXWedge → reconnectPort(port, keepL2=true)): cycling
// the transport must NOT tear down L2 sessions — an in-flight Winlink
// transfer resumes on the fresh link rather than dropping. Any rework of the
// reconnect paths (e.g. the stale-completion fix above) must preserve this.
func TestWedgeRelinkPreservesSession(t *testing.T) {
	eng := engine.New()
	b := New(eng, reconnCfg(true))

	f1 := newGateFake(false)
	f2 := newGateFake(false)
	b.newTransport = gateFactory(t, f1, f2)

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})

	waitFor(t, f1.openEntered, 2*time.Second, "initial open")

	// Establish an outgoing session: SABM out (via Connect), UA in.
	sink := &connEventSink{}
	onLoop(t, eng, func() {
		b.RegisterConnSink(sink)
		if _, err := b.L2().Connect(0, "LOCAL-1", "REMOTE-2", nil); err != nil {
			t.Errorf("Connect: %v", err)
		}
	})
	dst, _ := ax25.ParseAddress("LOCAL-1")
	src, _ := ax25.ParseAddress("REMOTE-2")
	ua := &ax25.Frame{Dst: dst, Src: src, Type: ax25.UA, Command: false, PF: true}
	onLoop(t, eng, func() { b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: ua.Bytes()}) })
	onLoop(t, eng, func() {
		c := b.L2().Get(0, "LOCAL-1", "REMOTE-2")
		if c == nil || c.State != l2pkg.Connected {
			t.Fatalf("setup: conn not Connected (c=%v)", c)
		}
	})

	// Wedge watchdog fires: cycle the transport, keep L2.
	onLoop(t, eng, func() {
		if !b.reconnectPort(0, true) {
			t.Fatalf("wedge relink rejected")
		}
	})
	waitFor(t, f2.openEntered, 2*time.Second, "relink dial")
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "relink online")

	// The session must have survived: still Connected, no disconnect event.
	onLoop(t, eng, func() {
		c := b.L2().Get(0, "LOCAL-1", "REMOTE-2")
		if c == nil {
			t.Errorf("wedge relink dropped the session")
			return
		}
		if c.State != l2pkg.Connected {
			t.Errorf("session state = %s after wedge relink, want Connected", c.State)
		}
		for _, e := range sink.events {
			if e.State == "disconnected" {
				t.Errorf("wedge relink emitted a spurious disconnect event: %+v", e)
			}
		}
	})

	// And the session still transmits on the fresh transport.
	onLoop(t, eng, func() {
		if c := b.L2().Get(0, "LOCAL-1", "REMOTE-2"); c != nil {
			b.L2().SendData(c, 0xF0, []byte("hello"))
		}
	})
	waitFor(t, func() bool { return f2.writeCount() > 0 }, 2*time.Second, "TX on relinked transport")
}

// TestPendingAutoReconnectYieldsToManualRelink — pre-2.0 audit finding. The
// auto-reconnect backoff timer is armed when the port drops; if the operator
// relinks manually before it fires, the timer must stand down. Otherwise it
// dials again and overwrites the working relinked port without closing it
// (duplicate RX on a multi-client TCP TNC; endless EBUSY retries on serial).
func TestPendingAutoReconnectYieldsToManualRelink(t *testing.T) {
	eng := engine.New()
	cfg := reconnCfg(true)
	cfg.Ports[0].ReconnectDelay = 0.3 // long enough to relink manually first
	b := New(eng, cfg)

	f1 := newGateFake(false) // initial link
	f2 := newGateFake(false) // manual relink
	var dials int32
	inner := gateFactory(t, f1, f2)
	b.newTransport = func(pc config.Port) (kiss.Transport, error) {
		atomic.AddInt32(&dials, 1)
		return inner(pc)
	}

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})
	waitFor(t, f1.openEntered, 2*time.Second, "initial open")

	f1.Close() // link drops: auto-reconnect armed for 300ms
	waitFor(t, func() bool { return !portOnline(eng, b, 0) }, 2*time.Second, "port offline")

	onLoop(t, eng, func() {
		if !b.ReconnectPort(0) {
			t.Errorf("manual relink rejected")
		}
	})
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "relink online")

	time.Sleep(600 * time.Millisecond) // past the armed backoff timer
	if n := atomic.LoadInt32(&dials); n != 2 {
		t.Errorf("dials = %d, want 2 (initial + manual); the stale backoff timer dialed again", n)
	}
	if f2.closeCount() != 0 || !portOnline(eng, b, 0) {
		t.Errorf("relinked port was displaced (closed=%d online=%v)", f2.closeCount(), portOnline(eng, b, 0))
	}
}

// TestWedgeRelinkStopsAfterBudget — 2026-09-22 field finding. On a Surface Go
// running over Bluetooth, a connect attempt to a station that could not be
// heard made the SPP link flap for the whole attempt: the session sat in
// Connecting, so portAwaitingReply stayed true, and with no RX ever arriving
// the watchdog relinked every rx_wedge_timeout indefinitely. Worse, frames sent
// during each relink window hit an offlineSentinel and never reached the air,
// so the flapping ate the N2 retries that were supposed to be establishing the
// link.
//
// A relink that is going to help helps on the first try. After relinkEscalateAfter
// futile cycles the transport must stop being cycled, leaving L2 to run out its
// remaining retries over a stable link.
func TestWedgeRelinkStopsAfterBudget(t *testing.T) {
	eng := engine.New()
	cfg := reconnCfg(false) // no auto-reconnect: only wedge relinks may dial
	cfg.Ports[0].RXWedgeTimeout = 20
	b := New(eng, cfg)

	var dials int32
	b.newTransport = func(config.Port) (kiss.Transport, error) {
		atomic.AddInt32(&dials, 1)
		return newGateFake(false), nil
	}

	if err := b.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	go eng.Run()
	t.Cleanup(func() {
		onLoop(t, eng, func() { b.Shutdown() })
		eng.Stop()
	})
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "initial open")

	// An outgoing connect that is never answered: the unreachable-gateway case.
	// The session stays in Connecting, so portAwaitingReply stays true forever.
	onLoop(t, eng, func() {
		if _, err := b.L2().Connect(0, "LOCAL-1", "REMOTE-2", nil); err != nil {
			t.Errorf("Connect: %v", err)
		}
	})

	// Sweep well past the timeout far more often than the budget allows. Each
	// sweep uses a fresh "now" because a relink reseeds lastRX to the sweep time.
	base := time.Now()
	for i := 1; i <= relinkEscalateAfter+4; i++ {
		// A relink leaves the slot offline until its dial lands; an offline
		// port is never judged wedged, so let it settle or the sweep is a no-op.
		waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "port online before sweep")
		onLoop(t, eng, func() { b.checkRXWedge(base.Add(time.Duration(i) * time.Minute)) })
	}
	waitFor(t, func() bool { return portOnline(eng, b, 0) }, 2*time.Second, "port online after sweeps")

	want := 1 + relinkEscalateAfter // initial dial + the capped run of relinks
	if got := int(atomic.LoadInt32(&dials)); got != want {
		t.Errorf("dials = %d, want %d (initial open + %d relinks, then the budget stops it)",
			got, want, relinkEscalateAfter)
	}

	// The session must survive: the point of stopping is to let L2 keep
	// retrying over a stable link, not to drop the connect attempt.
	onLoop(t, eng, func() {
		if c := b.L2().Get(0, "LOCAL-1", "REMOTE-2"); c == nil {
			t.Errorf("capped relink run dropped the session")
		}
	})
}
