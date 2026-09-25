package kiss

import (
	"fmt"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const txQueueSize = 64

// Port runs a Transport: reader goroutine decoding KISS frames and
// delivering data frames via onFrame; writer goroutine draining a TX queue.
type Port struct {
	num       int
	tr        Transport
	params    Params
	onFrame   func(RXFrame)
	onOffline func(port int)

	txCh   chan []byte
	online atomic.Bool
	closed atomic.Bool
	stopCh chan struct{}
	wg     sync.WaitGroup

	// demux is the reader loop's sole byte-stream decoder. It replaces a
	// bare kiss.Decoder so that an attached rig-control consumer (see
	// ControlChannel) can share the transport's RX stream safely -- see
	// demux.go.
	demux demux

	// txSem serializes every write to tr: ordinary KISS TX (writerLoop, via
	// txCh) and rig-control TX (ControlChannel's Write). Both share one
	// physical link; without this, two goroutines could interleave bytes
	// from two concurrent writes on the wire, corrupting whichever frame
	// lost the race.
	//
	// A buffered channel, not a sync.Mutex: acquiring it must be abortable
	// via stopCh (see acquireTxSem). A control write has no way to bound the
	// underlying transport's blocking Write call on every platform (Linux
	// Bluetooth has no write deadline -- see ctrlWriteTimeout), so a wedged
	// control write can end up holding this slot forever. With a plain
	// Mutex, writerLoop's next Lock() would then hang forever too, and
	// Port.Close's wg.Wait() would hang waiting for writerLoop -- a total
	// port brick reachable just by querying rig status, with no packet in
	// flight. acquireTxSem lets writerLoop (and any later control write)
	// give up cleanly once the port is torn down instead.
	txSem chan struct{}
}

// NewPort creates a Port that is not yet started.
func NewPort(num int, tr Transport, params Params,
	onFrame func(RXFrame), onOffline func(port int)) *Port {
	return &Port{
		num:       num,
		tr:        tr,
		params:    params,
		onFrame:   onFrame,
		onOffline: onOffline,
		txCh:      make(chan []byte, txQueueSize),
		stopCh:    make(chan struct{}),
		txSem:     make(chan struct{}, 1),
	}
}

// acquireTxSem blocks until the transport write slot is free, or the port
// tears down (stopCh closes) -- whichever comes first. Returns false in the
// latter case, meaning "give up, do not touch the transport": the slot may
// be held forever by an abandoned, wedged control write (see
// portControlChannel.Write), and a caller that kept blocking on it would
// itself hang forever, defeating the whole point of tearing the port down.
func (p *Port) acquireTxSem() bool {
	select {
	case p.txSem <- struct{}{}:
		return true
	case <-p.stopCh:
		return false
	}
}

// releaseTxSem frees the transport write slot.
func (p *Port) releaseTxSem() {
	<-p.txSem
}

// Start opens the transport, enters KISS mode, sends params, and spawns
// the reader and writer goroutines.
func (p *Port) Start() error {
	if err := p.tr.Open(); err != nil {
		return err
	}
	if err := p.tr.EnterKISS(); err != nil {
		p.tr.Close()
		return err
	}
	// Send KISS parameters.
	p.sendParams()

	p.online.Store(true)

	p.wg.Add(2)
	go p.readerLoop()
	go p.writerLoop()
	return nil
}

// sendParams writes any non-nil KISS parameter frames to the transport.
//
// A failure here is logged rather than fatal: the TNC keeps its previous (or
// default) timing, which is degraded but workable, and if the transport is
// genuinely dead the first real frame will trip writerLoop's failTX and take
// the port offline with a clearer error. What it must not do is fail silently
// — an unset TXDelay produces truncated transmissions that look like RF
// trouble rather than a configuration write that never landed.
func (p *Port) sendParams() {
	params := []struct {
		name string
		code uint8
		val  *int
	}{
		{"txdelay", 0x01, p.params.TXDelay},
		{"persistence", 0x02, p.params.Persistence},
		{"slottime", 0x03, p.params.SlotTime},
		{"txtail", 0x04, p.params.TXTail},
		{"fullduplex", 0x05, p.params.FullDuplex},
	}
	for _, prm := range params {
		if prm.val == nil {
			continue
		}
		if err := writeAll(p.tr, WrapCommand(0, prm.code, uint8(*prm.val))); err != nil {
			log.Printf("kiss: port %d could not set %s=%d (%v)", p.num, prm.name, *prm.val, err)
		}
	}
}

// readerLoop reads from the transport, decodes KISS frames, and delivers
// data frames (cmd low nibble == 0) to onFrame. Non-data frames are dropped.
func (p *Port) readerLoop() {
	defer p.wg.Done()
	var lastDropped uint64
	buf := make([]byte, 4096)
	for {
		n, err := p.tr.Read(buf)
		if n > 0 {
			// p.demux.Feed replaces a bare Decoder.Feed here so that bytes
			// recognised as a Gaia/Benshi frame (leading 0xFF 0x01) are
			// routed to any attached rig-control consumer instead of being
			// handed to the KISS decoder. When no consumer is attached this
			// is behaviourally identical to the old dec.Feed call -- see
			// TestDemuxKISSOnlyPassthroughByteIdentical in demux_test.go.
			frames := p.demux.Feed(buf[:n])
			if p.demux.kissDec.DroppedOversize != lastDropped {
				lastDropped = p.demux.kissDec.DroppedOversize
				log.Printf("kiss: port %d dropped oversize (> %d bytes) frame from transport (total %d)",
					p.num, MaxFrameSize, lastDropped)
			}
			for _, frame := range frames {
				if len(frame) < 1 {
					continue
				}
				cmdByte := frame[0]
				// Low nibble: 0x00 = data frame; anything else is a param/command frame.
				if cmdByte&0x0F != 0x00 {
					// Non-data command — drop per tncd.py:1653.
					continue
				}
				p.onFrame(RXFrame{
					Port: p.num,
					Data: append([]byte{}, frame[1:]...),
				})
			}
		}
		if err != nil {
			// EOF or transport error.
			if p.closed.CompareAndSwap(false, true) {
				// Unexpected disconnect: we won the CAS, so we are responsible
				// for teardown. Report the cause first — "port N went offline"
				// on its own gives an operator nothing to act on, and the
				// distinction between a clean EOF, a removed device and a
				// transport error decides what they should do about it.
				log.Printf("kiss: port %d read failed (%v) -- taking port offline", p.num, err)
				p.online.Store(false)
				close(p.stopCh)
				p.tr.Close()
				p.onOffline(p.num)
			}
			// If the CAS lost, Close() is already tearing down; just return.
			return
		}
	}
}

// writerLoop drains the TX channel and writes to the transport.
//
// A write failure takes the port offline rather than just ending this
// goroutine. Returning silently would leave the reader running and the port
// still reporting Online while nothing can ever be transmitted again — frames
// would be accepted and dropped into a dead TX path with no indication to the
// operator (the "tncd says it's transmitting, but it isn't" silent failure).
// Taking the port offline lets the bridge tear down and reconnect it.
func (p *Port) writerLoop() {
	defer p.wg.Done()
	for {
		select {
		case frame := <-p.txCh:
			if !p.acquireTxSem() {
				// Port is tearing down -- e.g. a wedged control write already
				// poisoned it via failTX (see portControlChannel.Write). Give
				// up without blocking on a slot that may never free.
				return
			}
			err := writeAll(p.tr, frame)
			p.releaseTxSem()
			if err != nil {
				log.Printf("kiss: port %d TX write failed (%v) -- taking port offline", p.num, err)
				p.failTX()
				return
			}
		case <-p.stopCh:
			return
		}
	}
}

// writeAll writes all of b, looping over short writes. Several transports are
// a single write(2) (go.bug.st/serial on Unix, the FreeBSD RFCOMM socket) and
// may accept part of a frame; sending the rest later would otherwise never
// happen, and the TNC would key up the truncated frame with a valid FCS. A
// write that makes no progress is an error, so a dead link cannot spin here.
func writeAll(w interface{ Write([]byte) (int, error) }, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 {
			return fmt.Errorf("transport accepted 0 of %d bytes", len(b))
		}
		b = b[n:]
	}
	return nil
}

// failTX tears the port down after an unrecoverable transport write error.
// Mirrors the readerLoop's disconnect path: whoever wins the closed CAS owns
// teardown (stop the peer loop, close the dead transport, fire onOffline).
func (p *Port) failTX() {
	if !p.closed.CompareAndSwap(false, true) {
		return // Close() or the reader already owns teardown
	}
	p.online.Store(false)
	close(p.stopCh)
	// Closing the transport unblocks the reader goroutine's pending Read.
	p.tr.Close()
	p.onOffline(p.num)
}

// Send wraps ax25Frame in a KISS data frame (port nibble 0) and queues it
// for transmission. If the queue is full the frame is dropped with a log.
func (p *Port) Send(ax25Frame []byte) {
	frame := WrapData(0, ax25Frame)
	select {
	case p.txCh <- frame:
	default:
		log.Printf("kiss: port %d TX queue full, dropping frame", p.num)
	}
}

// SendCommand queues a KISS command frame (cmdType in 1..6) for transmission on
// this port's TNC. The wire port nibble is 0 (one physical TNC per Port).
// Dropped with a log if the TX queue is full.
func (p *Port) SendCommand(cmdType uint8, value []byte) {
	frame := WrapCommandBytes(0, cmdType, value)
	select {
	case p.txCh <- frame:
	default:
		log.Printf("kiss: port %d TX queue full, dropping command frame", p.num)
	}
}

// Online returns true while the reader loop is running without error.
func (p *Port) Online() bool {
	return p.online.Load()
}

// Close stops the port and joins all goroutines before returning.
// It signals the writer, calls ExitKISS, closes the transport, and waits for
// all goroutines. If an unexpected reader error already initiated teardown,
// Close skips the teardown steps (the reader won the race) but still waits
// for all goroutines to finish. Does NOT trigger onOffline.
func (p *Port) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return // already closed
	}
	// If this CAS succeeded, we own teardown. The reader may have already
	// closed stopCh and set online to false in a concurrent teardown, but
	// only one of us will win the CAS.
	// If the reader won (CAS failed above), it already closed stopCh, set
	// online=false, and closed tr. We skip these steps but still join below.
	if p.online.Load() {
		// Normal close path: we shut down first.
		close(p.stopCh)
		p.online.Store(false)
		p.tr.ExitKISS()
		p.tr.Close()
	} else {
		// Reader already won and cleaned up; stopCh is closed, tr is closed.
		// Just join the goroutines.
	}
	p.wg.Wait()
}

// ctrlRXQueue bounds how many Gaia frames may sit unread before the demux
// starts dropping them (demux.deliverGaia's non-blocking send). Rig traffic
// is request/response, one frame in flight at a time in normal use; this
// only absorbs a consumer that briefly falls behind, not sustained load.
const ctrlRXQueue = 16

// ControlChannel returns a byte-duplex for rig control that shares this
// Port's transport with the live KISS data path, safely: Read is fed by the
// demultiplexer running inside the Port's own reader goroutine (demux.go),
// so there is never a second reader racing the KISS decoder for bytes; Write
// is serialized against KISS TX writes by txSem, so the two can never
// interleave bytes on the wire, and is bounded by ctrlWriteTimeout so a
// wedged transport cannot freeze KISS TX indefinitely -- see Write's doc
// comment for why a timeout alone is not enough and what happens instead.
//
// Only one control consumer may be attached at a time -- ErrControlChannelInUse
// otherwise. This is deliberate, not a limitation to be lifted later: a
// second concurrent consumer would have to share demux's single ctrl slot
// somehow, and doing that safely is exactly the problem this type solves for
// the KISS side, so it is enforced here rather than trusted to callers.
func (p *Port) ControlChannel() (io.ReadWriteCloser, error) {
	rx := make(chan []byte, ctrlRXQueue)
	if !p.demux.attach(rx) {
		return nil, ErrControlChannelInUse
	}
	return &portControlChannel{p: p, rx: rx, closed: make(chan struct{})}, nil
}

// portControlChannel is the io.ReadWriteCloser Port.ControlChannel hands
// out. Read never touches the transport directly -- it only drains frames
// the demux has already routed to rx. Write does touch the transport, under
// txMu (see ControlChannel's doc comment).
type portControlChannel struct {
	p *Port

	rx       chan []byte
	leftover []byte // tail of a frame that did not fit the caller's buffer

	closed    chan struct{}
	closeOnce sync.Once
}

func (c *portControlChannel) Read(b []byte) (int, error) {
	if len(c.leftover) > 0 {
		n := copy(b, c.leftover)
		c.leftover = c.leftover[n:]
		return n, nil
	}
	select {
	case frame := <-c.rx:
		n := copy(b, frame)
		if n < len(frame) {
			c.leftover = append([]byte(nil), frame[n:]...)
		}
		return n, nil
	case <-c.closed:
		return 0, io.EOF
	case <-c.p.stopCh:
		// The Port itself is tearing down (reader error, or Port.Close):
		// nothing will ever feed rx again, so a Read blocked here must not
		// hang forever waiting for a consumer-driven Close that may never
		// come. This is what "the control channel closes with the port"
		// (the design doc's link-ownership rule) means at this layer --
		// without it, a caller like internal/rig's readLoop would leak a
		// goroutine blocked on a dead port every time it goes offline.
		return 0, io.EOF
	}
}

// ctrlWriteTimeout bounds how long a control write may occupy the transport
// before the port gives up on it and fails. Not every transport this
// package supports has a write deadline: kiss/bluetooth_linux.go writes to a
// raw *os.File with no SetWriteDeadline anywhere in that file, and
// checkTXDrain's queue-depth stall detector only samples BEFORE a write, so
// it cannot abort one already blocked inside the syscall. Without a bound
// here, a wedged radio turns "query rig status" into an indefinite freeze of
// the shared KISS TX path -- worse, one with no packet in flight to make it
// obviously symptomatic. 10s matches btSendTimeout, the equivalent bound
// bluetooth_windows.go already places on a single SPP send via SO_SNDTIMEO.
//
// A var, not a const, solely so tests can shorten it rather than waiting out
// the real 10s -- see TestPortControlChannelWriteTimeoutFailsPortCleanly.
var ctrlWriteTimeout = 10 * time.Second

// Write sends b (a caller-encoded Gaia frame) straight to the transport,
// holding the tx slot for the duration so it cannot interleave with a
// concurrent KISS TX write. It is synchronous -- it blocks until the
// underlying write actually returns or ctrlWriteTimeout elapses -- which
// internal/rig's request layer depends on for its own write-phase timeout to
// mean anything.
//
// The write itself runs in a separate goroutine so this call can give up at
// the deadline without waiting for a transport that may never return. That
// goroutine is deliberately NOT abandoned to run and vanish, the way a
// simple "release the lock either way" scheme would: if Write released the
// tx slot on timeout, a subsequent KISS write could start immediately, and
// the abandoned write could then land on the wire at any later, unbounded
// time -- interleaving its bytes with that KISS frame, exactly the
// corruption the tx slot exists to prevent. So on timeout the slot is left
// held (releaseTxSem is only ever called by the goroutine that did the
// write, whenever -- if ever -- it returns) and the port is poisoned via
// failTX instead of retried: the byte stream's state is now unknown, so
// discarding it and reconnecting is the only safe recovery, mirroring the
// same conclusion internal/rig's own write-phase timeout already reached one
// layer up (see internal/rig's request doc comment). failTX unblocks
// writerLoop (acquireTxSem observes stopCh and gives up rather than piling
// up behind the held slot) and hands off to the bridge's existing reconnect
// path -- recovery this change reuses rather than invents.
func (c *portControlChannel) Write(b []byte) (int, error) {
	select {
	case <-c.closed:
		return 0, fmt.Errorf("kiss: control channel closed")
	case <-c.p.stopCh:
		return 0, fmt.Errorf("kiss: port closed")
	default:
	}
	if !c.p.acquireTxSem() {
		return 0, fmt.Errorf("kiss: port closed")
	}

	result := make(chan error, 1) // buffered: the goroutine must never block trying to report to an abandoned caller
	go func() {
		result <- writeAll(c.p.tr, b)
		c.p.releaseTxSem()
	}()

	timer := time.NewTimer(ctrlWriteTimeout)
	defer timer.Stop()
	select {
	case err := <-result:
		if err != nil {
			return 0, err
		}
		return len(b), nil
	case <-timer.C:
		log.Printf("kiss: port %d control write did not complete within %s -- failing port (transport state unknown)",
			c.p.num, ctrlWriteTimeout)
		c.p.failTX()
		return 0, fmt.Errorf("kiss: control write timed out after %s -- port failed", ctrlWriteTimeout)
	case <-c.p.stopCh:
		return 0, fmt.Errorf("kiss: port closed")
	}
}

// Close detaches this channel from the demux and unblocks any pending Read.
// It deliberately does not close rx or touch the transport: the Port, not
// whoever asked for the control channel, owns the transport's lifetime.
func (c *portControlChannel) Close() error {
	c.closeOnce.Do(func() {
		c.p.demux.detach(c.rx)
		close(c.closed)
	})
	return nil
}
