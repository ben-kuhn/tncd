package kiss

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
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

// trackingPipeTransport wraps pipeTransport and records whether Close was called.
type trackingPipeTransport struct {
	pipeTransport
	closed atomic.Bool
}

func (t *trackingPipeTransport) Close() error {
	t.closed.Store(true)
	return t.pipeTransport.Close()
}

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

// idleTransport returns (0, nil) for the first N reads (simulating a serial
// port's read-timeout behaviour on an idle line), then delivers a real KISS
// frame, then blocks until Close.
type idleTransport struct {
	idle    int // how many (0, nil) reads to return first
	frameC  chan []byte
	closedC chan struct{}
}

func newIdleTransport(idle int) *idleTransport {
	return &idleTransport{
		idle:    idle,
		frameC:  make(chan []byte, 1),
		closedC: make(chan struct{}),
	}
}
func (it *idleTransport) Open() error                 { return nil }
func (it *idleTransport) EnterKISS() error            { return nil }
func (it *idleTransport) ExitKISS()                   {}
func (it *idleTransport) Close() error                { close(it.closedC); return nil }
func (it *idleTransport) Write(b []byte) (int, error) { return len(b), nil }
func (it *idleTransport) Read(b []byte) (int, error) {
	if it.idle > 0 {
		it.idle--
		return 0, nil // idle timeout — no data
	}
	select {
	case frame := <-it.frameC:
		n := copy(b, frame)
		return n, nil
	case <-it.closedC:
		return 0, io.EOF
	}
}

// TestReaderLoopIdleZeroNilIsNotOffline verifies that the readerLoop correctly
// treats (0, nil) reads — which occur on a serial port with a read timeout
// when the line is idle — as "no data, keep going" rather than as an error or
// EOF.  This matches the SetReadTimeout(100ms) behaviour added in Open().
func TestReaderLoopIdleZeroNilIsNotOffline(t *testing.T) {
	const idleRounds = 5
	it := newIdleTransport(idleRounds)
	offlineCalled := make(chan struct{}, 1)
	rx := make(chan RXFrame, 4)
	p := NewPort(7, it, Params{},
		func(f RXFrame) { rx <- f },
		func(int) { offlineCalled <- struct{}{} })
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Queue a real KISS data frame after the idle reads are exhausted.
	it.frameC <- WrapData(0, []byte("hello"))

	// Expect the frame to arrive — confirms the loop kept running through (0, nil).
	select {
	case f := <-rx:
		if !bytes.Equal(f.Data, []byte("hello")) {
			t.Fatalf("unexpected frame data: %q", f.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame never received — readerLoop may have exited on (0, nil)")
	}

	// onOffline must not have been called.
	select {
	case <-offlineCalled:
		t.Fatal("onOffline was called despite no real error")
	default:
	}
}

// TestReaderEOFClosesTransportAndWriterExits verifies Bug 2 fix:
// when the transport signals EOF (device power-cycle), readerLoop must
//
//	(a) call tr.Close() on the dead transport, and
//	(b) stop the writer goroutine so a subsequent Port.Close() does not hang.
func TestReaderEOFClosesTransportAndWriterExits(t *testing.T) {
	a, b := net.Pipe()
	tr := &trackingPipeTransport{pipeTransport: pipeTransport{c: a}}

	off := make(chan int, 1)
	p := NewPort(5, tr, Params{},
		func(RXFrame) {},
		func(n int) { off <- n })

	if err := p.Start(); err != nil {
		t.Fatal(err)
	}

	// Close the far end — simulates device power-cycle / EOF.
	b.Close()

	// Wait for onOffline to fire.
	select {
	case n := <-off:
		if n != 5 {
			t.Fatalf("offline port = %d, want 5", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onOffline never fired after EOF")
	}

	// Transport must have been closed by readerLoop teardown.
	if !tr.closed.Load() {
		t.Error("transport.Close() was not called after EOF teardown")
	}

	// Port.Close() must return promptly (writer goroutine must have stopped).
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Port.Close() hung after EOF teardown — writer goroutine leaked")
	}
}

// TestReaderEOFSendDoesNotPanic verifies that Send() after EOF teardown
// does not panic (frame is silently dropped via the non-blocking select).
func TestReaderEOFSendDoesNotPanic(t *testing.T) {
	a, b := net.Pipe()
	off := make(chan int, 1)
	p := NewPort(6, &pipeTransport{c: a}, Params{},
		func(RXFrame) {},
		func(n int) { off <- n })

	if err := p.Start(); err != nil {
		t.Fatal(err)
	}

	// Trigger EOF.
	b.Close()

	select {
	case <-off:
	case <-time.After(2 * time.Second):
		t.Fatal("onOffline never fired")
	}

	// Send after teardown must not panic.
	p.Send([]byte("after-eof"))
}

// TestCloseAfterReaderEOFTeardownJoins verifies that after a reader-error
// teardown completes (onOffline observed), calling Close() returns promptly
// (join works, no deadlock).
func TestCloseAfterReaderEOFTeardownJoins(t *testing.T) {
	a, b := net.Pipe()
	off := make(chan int, 1)
	p := NewPort(8, &pipeTransport{c: a}, Params{},
		func(RXFrame) {},
		func(n int) { off <- n })

	if err := p.Start(); err != nil {
		t.Fatal(err)
	}

	// Trigger EOF to start reader-initiated teardown.
	b.Close()

	// Wait for onOffline to fire, confirming teardown is done.
	select {
	case n := <-off:
		if n != 8 {
			t.Fatalf("offline port = %d, want 8", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onOffline never fired after EOF")
	}

	// Now call Close() after the reader has already torn down.
	// This exercises the path where the CAS fails (reader won).
	// Close must still call wg.Wait() and return promptly.
	done := make(chan struct{})
	go func() {
		p.Close()
		close(done)
	}()
	select {
	case <-done:
		// Success: join completed within timeout.
	case <-time.After(2 * time.Second):
		t.Fatal("Port.Close() hung after reader-initiated teardown — join failed")
	}
}
