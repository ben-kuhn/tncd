package kisstcp

import (
	"net"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
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

	srv, err := Serve(eng, b, "127.0.0.1", 0, 16) // port 0 = OS-assigned
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

func TestKISSTCPExitKISSDropped(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()
	fs := newFakeSender()
	var b *bridge.Bridge
	done := make(chan struct{})
	eng.Do(func() { b = newBridge(t, eng, fs); close(done) })
	<-done
	srv, err := Serve(eng, b, "127.0.0.1", 0, 16)
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
