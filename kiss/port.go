package kiss

import (
	"log"
	"sync"
	"sync/atomic"
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
	}
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
func (p *Port) sendParams() {
	if p.params.TXDelay != nil {
		p.tr.Write(WrapCommand(0, 0x01, uint8(*p.params.TXDelay)))
	}
	if p.params.Persistence != nil {
		p.tr.Write(WrapCommand(0, 0x02, uint8(*p.params.Persistence)))
	}
	if p.params.SlotTime != nil {
		p.tr.Write(WrapCommand(0, 0x03, uint8(*p.params.SlotTime)))
	}
	if p.params.TXTail != nil {
		p.tr.Write(WrapCommand(0, 0x04, uint8(*p.params.TXTail)))
	}
	if p.params.FullDuplex != nil {
		p.tr.Write(WrapCommand(0, 0x05, uint8(*p.params.FullDuplex)))
	}
}

// readerLoop reads from the transport, decodes KISS frames, and delivers
// data frames (cmd low nibble == 0) to onFrame. Non-data frames are dropped.
func (p *Port) readerLoop() {
	defer p.wg.Done()
	var dec Decoder
	var lastDropped uint64
	buf := make([]byte, 4096)
	for {
		n, err := p.tr.Read(buf)
		if n > 0 {
			frames := dec.Feed(buf[:n])
			if dec.DroppedOversize != lastDropped {
				lastDropped = dec.DroppedOversize
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
				// for teardown. Close stopCh to stop the writer, then close the
				// dead transport (no ExitKISS on a dead link), and fire onOffline.
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
func (p *Port) writerLoop() {
	defer p.wg.Done()
	for {
		select {
		case frame := <-p.txCh:
			if _, err := p.tr.Write(frame); err != nil {
				// Transport gone; drain any remaining sends and return.
				return
			}
		case <-p.stopCh:
			return
		}
	}
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
