package kisstcp

import (
	"net"
	"testing"
	"time"

	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/internal/netutil"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// fakeSender implements bridge.PortSender, recording data + command sends.
type fakeSender struct {
	ch   chan []byte
	cmds chan struct{ cmd uint8; val []byte }
}
func newFakeSender() *fakeSender {
	return &fakeSender{ch: make(chan []byte, 8), cmds: make(chan struct{ cmd uint8; val []byte }, 8)}
}
func (f *fakeSender) Send(raw []byte)                        { f.ch <- append([]byte{}, raw...) }
func (f *fakeSender) SendCommand(cmd uint8, val []byte)      { f.cmds <- struct{ cmd uint8; val []byte }{cmd, append([]byte{}, val...)} }
func (f *fakeSender) Online() bool                           { return true }

func newBridge(t *testing.T, eng *engine.Engine, fs *fakeSender) *bridge.Bridge {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10},
		Ports:  []config.Port{{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200}},
	}
	b := bridge.New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 3, 10, 0)}
	bridge.InjectPorts(b, eng, params, []bridge.PortSender{fs})
	return b
}

func TestKISSTCPRoundTrip(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fs := newFakeSender()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng, fs); close(done) })
	<-done

	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, 0, netutil.Allowlist{}) // port 0 = OS-assigned
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		done := make(chan struct{})
		eng.Do(func() { srv.Close(); close(done) })
		<-done
	}()

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// TX: client sends a KISS data frame with AX.25 payload {0xAA,0xBB}.
	conn.Write(kiss.WrapData(0, []byte{0xAA, 0xBB}))
	select {
	case got := <-fs.ch:
		if len(got) != 2 || got[0] != 0xAA || got[1] != 0xBB {
			t.Fatalf("port got % x, want AA BB", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for TX to reach port")
	}

	// RX: bridge fans a raw frame out to the client.
	eng.Do(func() { srv.OnRawRX(0, []byte{0x11, 0x22}) })
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	rbuf := make([]byte, 64)
	n, err := conn.Read(rbuf)
	if err != nil {
		t.Fatal(err)
	}
	want := kiss.WrapData(0, []byte{0x11, 0x22})
	if string(rbuf[:n]) != string(want) {
		t.Fatalf("client RX = % x, want % x", rbuf[:n], want)
	}
}

// TestKISSTCPNoLoopback verifies the no-loopback invariant: when a KISS-TCP
// client sends a data frame, the raw AX.25 payload reaches the fake port and
// NEITHER the sending client NOR a second connected client receives any RX
// bytes back (a client's TX must never be re-delivered as RX).
func TestKISSTCPNoLoopback(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fs := newFakeSender()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng, fs); close(done) })
	<-done

	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, 0, netutil.Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		done := make(chan struct{})
		eng.Do(func() { srv.Close(); close(done) })
		<-done
	}()

	connA, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Close()

	connB, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Close()

	// Client A sends a KISS data frame with AX.25 payload {0xCC, 0xDD}.
	if _, err := connA.Write(kiss.WrapData(0, []byte{0xCC, 0xDD})); err != nil {
		t.Fatal(err)
	}

	// Assert the fake port received A's AX.25 payload.
	select {
	case got := <-fs.ch:
		if len(got) != 2 || got[0] != 0xCC || got[1] != 0xDD {
			t.Fatalf("port got % x, want CC DD", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for TX to reach port")
	}

	// Assert neither client A nor client B receives any RX bytes back.
	rbuf := make([]byte, 64)
	connA.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	n, err := connA.Read(rbuf)
	if n != 0 || !isTimeout(err) {
		t.Fatalf("client A got loopback: n=%d err=%v", n, err)
	}
	connB.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	n, err = connB.Read(rbuf)
	if n != 0 || !isTimeout(err) {
		t.Fatalf("client B got unexpected RX: n=%d err=%v", n, err)
	}
}

// isTimeout reports whether err is a network timeout error.
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	if ne, ok := err.(net.Error); ok {
		return ne.Timeout()
	}
	return false
}

func TestKISSTCPExitKISSDropped(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fs := newFakeSender()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng, fs); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, 0, netutil.Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		done := make(chan struct{})
		eng.Do(func() { srv.Close(); close(done) })
		<-done
	}()
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.Write([]byte{kiss.FEND, 0xFF, kiss.FEND}) // exit-KISS
	conn.Write(kiss.WrapCommandBytes(0, 0x01, []byte{40})) // TXDELAY should forward
	select {
	case c := <-fs.cmds:
		if c.cmd != 0x01 {
			t.Fatalf("got cmd %#x, want TXDELAY", c.cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout; TXDELAY not forwarded")
	}
	select {
	case <-fs.cmds:
		t.Fatal("exit-KISS must not be forwarded as a command")
	case <-fs.ch:
		t.Fatal("exit-KISS must not be forwarded as data")
	case <-time.After(200 * time.Millisecond):
		// good — nothing extra forwarded
	}
}

// TestIdleSweep: a silent client is reaped after idle_timeout while a client
// that keeps sending survives.
func TestIdleSweep(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fs := newFakeSender()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng, fs); close(done) })
	<-done

	// idle_timeout = 1s → sweep interval min(30s, 1s) = 1s.
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16, 1, netutil.Allowlist{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		done := make(chan struct{})
		eng.Do(func() { srv.Close(); close(done) })
		<-done
	}()

	idle, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	active, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()

	stop := make(chan struct{})
	defer close(stop)
	// Drain the fake port: Send blocks on a full channel and runs on the
	// engine loop, so unread keepalives would wedge the sweep itself.
	go func() {
		for {
			select {
			case <-fs.ch:
			case <-stop:
				return
			}
		}
	}()
	// Keep the active client busy until the idle one is reaped.
	go func() {
		tick := time.NewTicker(300 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if _, err := active.Write(kiss.WrapData(0, []byte{0x01})); err != nil {
					return
				}
			case <-stop:
				return
			}
		}
	}()

	// The idle client's conn is closed server-side within a few seconds.
	idle.SetReadDeadline(time.Now().Add(6 * time.Second))
	if _, err := idle.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle client was not reaped")
	}

	// The active client survives: exactly one client remains on the server.
	clients := -1
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && clients != 1 {
		done2 := make(chan struct{})
		eng.Do(func() { clients = len(srv.clients); close(done2) })
		<-done2
		if clients != 1 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if clients != 1 {
		t.Fatalf("server has %d clients after sweep, want 1 (active survives)", clients)
	}
}
