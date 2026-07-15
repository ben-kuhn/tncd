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
	buf := make([]byte, 4096)
	for {
		n, err := p.tr.Read(buf)
		if n > 0 {
			frames := dec.Feed(buf[:n])
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
			if !p.closed.Load() {
				// Unexpected disconnect — fire onOffline.
				p.online.Store(false)
				p.onOffline(p.num)
			}
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

// Online returns true while the reader loop is running without error.
func (p *Port) Online() bool {
	return p.online.Load()
}

// Close stops the port: signals the writer, calls ExitKISS, closes the
// transport, and waits for goroutines. Does NOT trigger onOffline.
func (p *Port) Close() {
	if !p.closed.CompareAndSwap(false, true) {
		return // already closed
	}
	// Signal the writer loop to stop.
	close(p.stopCh)
	// Mark offline before closing the transport so the reader loop knows
	// this is an intentional close and does not call onOffline.
	p.online.Store(false)
	p.tr.ExitKISS()
	p.tr.Close()
	p.wg.Wait()
}
