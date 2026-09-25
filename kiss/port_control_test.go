package kiss

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
)

// TestPortControlChannelCoexistsWithKISS is the end-to-end proof this whole
// file exists for: with a rig-control consumer attached to a live Port,
// KISS frames arriving before and after an interleaved Gaia frame on the
// SAME transport both reach onFrame intact, and the Gaia frame reaches the
// control channel -- exactly the wire behaviour bench-confirmed against a
// real UV-PRO (progress notes, 2026-09-23).
func TestPortControlChannelCoexistsWithKISS(t *testing.T) {
	a, b := net.Pipe()
	rx := make(chan RXFrame, 4)
	p := NewPort(0, &pipeTransport{c: a}, Params{}, func(f RXFrame) { rx <- f }, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	cc, err := p.ControlChannel()
	if err != nil {
		t.Fatalf("ControlChannel: %v", err)
	}
	defer cc.Close()

	gaia := benshi.Frame{Flags: benshi.FlagNone, Data: []byte{0x00, 0x02, 0x00, 0x24}}.Bytes()
	raw := append(WrapData(0, []byte("before gaia")), gaia...)
	raw = append(raw, WrapData(0, []byte("after gaia"))...)

	go b.Write(raw)

	var got []RXFrame
	for len(got) < 2 {
		select {
		case f := <-rx:
			got = append(got, f)
		case <-time.After(2 * time.Second):
			t.Fatalf("only got %d of 2 expected KISS frames", len(got))
		}
	}
	if !bytes.Equal(got[0].Data, []byte("before gaia")) || !bytes.Equal(got[1].Data, []byte("after gaia")) {
		t.Fatalf("frames = %q, %q", got[0].Data, got[1].Data)
	}

	buf := make([]byte, 64)
	n, err := cc.Read(buf)
	if err != nil {
		t.Fatalf("control Read: %v", err)
	}
	if !bytes.Equal(buf[:n], gaia) {
		t.Fatalf("control channel got % X, want % X", buf[:n], gaia)
	}
}

// TestPortControlChannelWriteReachesTransport proves Write is not merely
// queued and forgotten: the bytes actually land on the wire.
func TestPortControlChannelWriteReachesTransport(t *testing.T) {
	a, b := net.Pipe()
	p := NewPort(0, &pipeTransport{c: a}, Params{}, func(RXFrame) {}, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	cc, err := p.ControlChannel()
	if err != nil {
		t.Fatalf("ControlChannel: %v", err)
	}
	defer cc.Close()

	want := []byte{0xFF, 0x01, 0x00, 0x00, 0x00, 0x02, 0x00, 0x04}
	writeErr := make(chan error, 1)
	go func() { _, err := cc.Write(want); writeErr <- err }()

	got := make([]byte, len(want))
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := readFull(b, got); err != nil {
		t.Fatalf("far end read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("far end got % X, want % X", got, want)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
}

// blockingWriteTransport's Write blocks forever until unblock is closed,
// simulating a wedged Bluetooth socket with no write deadline -- the exact
// failure mode ctrlWriteTimeout exists to bound (kiss/bluetooth_linux.go has
// no SetWriteDeadline call at all).
type blockingWriteTransport struct {
	idleTransport
	unblock    chan struct{}
	writeCount atomic.Int32
}

func newBlockingWriteTransport() *blockingWriteTransport {
	return &blockingWriteTransport{
		idleTransport: *newIdleTransport(0),
		unblock:       make(chan struct{}),
	}
}

func (w *blockingWriteTransport) Write(b []byte) (int, error) {
	w.writeCount.Add(1)
	<-w.unblock
	return len(b), nil
}

// TestPortControlChannelWriteTimeoutFailsPortCleanly is the regression test
// for the Critical finding: a control write on a transport with no write
// deadline must not be able to freeze the shared KISS TX path indefinitely.
//
// It proves the whole chain the fix relies on: Write itself returns (does
// not hang past ctrlWriteTimeout even though the underlying transport never
// returns), the port is failed as a consequence (onOffline fires, Online()
// goes false), a KISS Send afterward does not block, and -- the sharpest
// edge -- Port.Close() returns promptly even though the abandoned write
// goroutine is, and remains, permanently blocked inside Write for the rest
// of the test. That last property is the one a plain sync.Mutex could not
// have given: it is what proves writerLoop gave up on the tx slot instead
// of piling up behind it.
func TestPortControlChannelWriteTimeoutFailsPortCleanly(t *testing.T) {
	orig := ctrlWriteTimeout
	ctrlWriteTimeout = 50 * time.Millisecond
	defer func() { ctrlWriteTimeout = orig }()

	tr := newBlockingWriteTransport()
	defer close(tr.unblock) // let the permanently-blocked goroutine finish so it doesn't leak past the test

	off := make(chan int, 1)
	p := NewPort(0, tr, Params{}, func(RXFrame) {}, func(n int) { off <- n })
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cc, err := p.ControlChannel()
	if err != nil {
		t.Fatalf("ControlChannel: %v", err)
	}

	writeErr := make(chan error, 1)
	go func() {
		_, err := cc.Write([]byte{0xFF, 0x01, 0x00, 0x00})
		writeErr <- err
	}()

	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("control Write returned nil error against a permanently blocked transport")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control Write did not return within the timeout budget -- it must not hang on a wedged transport")
	}

	// A consequence of the timeout: the port must have failed, not just this
	// one call.
	select {
	case n := <-off:
		if n != 0 {
			t.Fatalf("offline port = %d, want 0", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control write timeout never triggered failTX (port did not go offline)")
	}
	if p.Online() {
		t.Fatal("port still reports Online after a control write timeout")
	}

	// A KISS Send after the port has failed must not block -- Send's own
	// select is already non-blocking, but this also exercises writerLoop
	// having already exited (via acquireTxSem observing the now-closed
	// stopCh) rather than being stuck behind the permanently-held tx slot.
	sendDone := make(chan struct{})
	go func() {
		p.Send([]byte("after timeout"))
		close(sendDone)
	}()
	select {
	case <-sendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked after the port failed")
	}

	// The sharpest check: Close() must return promptly even though the
	// abandoned write goroutine above is STILL blocked (tr.unblock has not
	// been closed yet -- that only happens in this test's own deferred
	// cleanup, after this assertion). A plain sync.Mutex held by that
	// goroutine forever would make writerLoop's next Lock() -- and thus
	// wg.Wait() inside Close() -- hang forever too.
	closeDone := make(chan struct{})
	go func() {
		p.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Port.Close() hung after a control write timeout -- txSem was not correctly abandoned")
	}
}

func readFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// TestPortControlChannelReadUnblocksOnPortTeardown proves a control-channel
// Read blocked waiting for a Gaia frame does not hang forever once the Port
// itself goes offline (e.g. the transport EOFs) -- it must return io.EOF
// rather than leak the caller's goroutine on a dead port.
func TestPortControlChannelReadUnblocksOnPortTeardown(t *testing.T) {
	a, b := net.Pipe()
	p := NewPort(0, &pipeTransport{c: a}, Params{}, func(RXFrame) {}, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cc, err := p.ControlChannel()
	if err != nil {
		t.Fatalf("ControlChannel: %v", err)
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := cc.Read(make([]byte, 64))
		readDone <- err
	}()

	// Give the Read a moment to actually block, then tear the port down the
	// way a real disconnect does (far end closes -> reader EOF).
	time.Sleep(20 * time.Millisecond)
	b.Close()

	select {
	case err := <-readDone:
		if err != io.EOF {
			t.Fatalf("Read err = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control channel Read never unblocked after port teardown")
	}
}

// TestPortControlChannelOnlyOneConsumer proves ControlChannel enforces the
// single-consumer invariant the demux relies on for safety, and that
// Close() releases the slot for a subsequent attach.
func TestPortControlChannelOnlyOneConsumer(t *testing.T) {
	a, _ := net.Pipe()
	p := NewPort(0, &pipeTransport{c: a}, Params{}, func(RXFrame) {}, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	cc1, err := p.ControlChannel()
	if err != nil {
		t.Fatalf("first ControlChannel: %v", err)
	}
	if _, err := p.ControlChannel(); err != ErrControlChannelInUse {
		t.Fatalf("second ControlChannel err = %v, want ErrControlChannelInUse", err)
	}
	cc1.Close()
	if _, err := p.ControlChannel(); err != nil {
		t.Fatalf("ControlChannel after Close: %v", err)
	}
}

// TestPortControlChannelKISSTXWriteNotCorruptedByConcurrentControlWrites
// hammers KISS TX (via Send) and control-channel writes concurrently and
// verifies the far end always sees whole, non-interleaved frames -- the
// property txMu exists to guarantee. Each side's payload is a distinct,
// recognisable repeated byte so any interleaving would show up as a byte
// sequence that belongs to neither.
func TestPortControlChannelKISSTXWriteNotCorruptedByConcurrentControlWrites(t *testing.T) {
	a, b := net.Pipe()
	p := NewPort(0, &pipeTransport{c: a}, Params{}, func(RXFrame) {}, func(int) {})
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Close()

	cc, err := p.ControlChannel()
	if err != nil {
		t.Fatalf("ControlChannel: %v", err)
	}
	defer cc.Close()

	const rounds = 50
	kissPayload := bytes.Repeat([]byte{0xAA}, 20)
	ctrlPayload := bytes.Repeat([]byte{0xBB}, 12)
	kissFrame := WrapData(0, kissPayload)

	// Drain the far end and reconstruct with a demux, not a bare Decoder:
	// a bare Decoder's inFrame flag never resets once the first FEND is
	// seen (framing.go), so the 0xBB control bytes sitting BETWEEN two
	// properly-delimited KISS frames would be misread as the payload of an
	// implicitly-opened next frame -- a reconstruction artifact of the test
	// tool, not evidence of real interleaving. demux correctly drops that
	// inter-frame noise (see TestDemuxGaiaDiscardedWithoutConsumer's sibling
	// case for plain noise), which is exactly what makes it the right tool
	// to verify the actual property under test: every reconstructed KISS
	// frame's payload must be pure 0xAA, proving no 0xBB control byte was
	// ever spliced into the middle of a KISS write.
	done := make(chan struct{})
	var kissCount, ctrlRuns int
	go func() {
		defer close(done)
		var dm demux
		buf := make([]byte, 256)
		total := 0
		want := rounds*len(kissFrame) + rounds*len(ctrlPayload)
		b.SetReadDeadline(time.Now().Add(5 * time.Second))
		for total < want {
			n, err := b.Read(buf)
			if n > 0 {
				total += n
				for _, bb := range buf[:n] {
					if bb != 0xAA && bb != 0xBB && bb != FEND && bb != 0x00 {
						t.Errorf("unexpected byte %#x on the wire (neither KISS nor control payload)", bb)
					}
				}
				for _, frame := range dm.Feed(buf[:n]) {
					kissCount++
					if !bytes.Equal(frame[1:], kissPayload) {
						t.Errorf("reconstructed KISS frame payload = % X, want pure 0xAA (control byte leaked in)", frame[1:])
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() {
		for i := 0; i < rounds; i++ {
			p.Send(kissPayload)
		}
	}()
	for i := 0; i < rounds; i++ {
		if _, err := cc.Write(ctrlPayload); err != nil {
			t.Fatalf("control Write: %v", err)
		}
		ctrlRuns++
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("far end never received all expected bytes")
	}
	if kissCount != rounds {
		t.Fatalf("far end reconstructed %d KISS frames, want %d (interleaving would corrupt framing)", kissCount, rounds)
	}
	if ctrlRuns != rounds {
		t.Fatalf("sent %d control writes, want %d", ctrlRuns, rounds)
	}
}
