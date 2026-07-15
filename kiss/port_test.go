package kiss

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

type pipeTransport struct{ c net.Conn }

func (p *pipeTransport) Open() error                 { return nil }
func (p *pipeTransport) EnterKISS() error            { return nil }
func (p *pipeTransport) ExitKISS()                   {}
func (p *pipeTransport) Close() error                { return p.c.Close() }
func (p *pipeTransport) Read(b []byte) (int, error)  { return p.c.Read(b) }
func (p *pipeTransport) Write(b []byte) (int, error) { return p.c.Write(b) }

func TestPortRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	rx := make(chan RXFrame, 4)
	p := NewPort(1, &pipeTransport{c: a}, Params{},
		func(f RXFrame) { rx <- f }, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// TNC → tncd
	go b.Write(WrapData(0, []byte("from-tnc")))
	select {
	case f := <-rx:
		if f.Port != 1 || !bytes.Equal(f.Data, []byte("from-tnc")) {
			t.Fatalf("rx = %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame received")
	}

	// tncd → TNC
	p.Send([]byte("to-tnc"))
	buf := make([]byte, 64)
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := b.Read(buf)
	if err != nil && err != io.EOF {
		t.Fatal(err)
	}
	var d Decoder
	frames := d.Feed(buf[:n])
	if len(frames) != 1 || !bytes.Equal(frames[0][1:], []byte("to-tnc")) {
		t.Fatalf("tnc got % x", buf[:n])
	}
}

func TestNonDataCommandDropped(t *testing.T) {
	a, b := net.Pipe()
	rx := make(chan RXFrame, 4)
	p := NewPort(0, &pipeTransport{c: a}, Params{},
		func(f RXFrame) { rx <- f }, func(int) {})
	p.Start()
	defer p.Close()
	go b.Write([]byte{FEND, 0x01, 40, FEND}) // TXDELAY response, not data
	select {
	case f := <-rx:
		t.Fatalf("non-data frame delivered: %+v", f)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestKISSParamsSentOnStart(t *testing.T) {
	a, b := net.Pipe()
	tx := 40
	go func() {
		p := NewPort(0, &pipeTransport{c: a},
			Params{TXDelay: &tx}, func(RXFrame) {}, func(int) {})
		p.Start()
	}()
	buf := make([]byte, 16)
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := b.Read(buf)
	if !bytes.Contains(buf[:n], []byte{FEND, 0x01, 40, FEND}) {
		t.Fatalf("TXDELAY not sent, got % x", buf[:n])
	}
}

func TestReaderEOFTriggersOffline(t *testing.T) {
	a, b := net.Pipe()
	off := make(chan int, 1)
	p := NewPort(3, &pipeTransport{c: a}, Params{},
		func(RXFrame) {}, func(n int) { off <- n })
	p.Start()
	b.Close()
	select {
	case n := <-off:
		if n != 3 || p.Online() {
			t.Fatalf("offline port = %d online=%v", n, p.Online())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("offline callback never fired")
	}
}
