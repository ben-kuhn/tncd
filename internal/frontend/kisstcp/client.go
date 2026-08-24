package kisstcp

import (
	"log"
	"net"
	"sync"
	"time"

	"github.com/ben-kuhn/tncd/v2/kiss"
)

type client struct {
	s    *Server
	conn net.Conn

	writeCh chan []byte // buffered outbound KISS frames

	mu           sync.Mutex
	closed       bool
	lastActivity time.Time
	inflight     int // frames posted to the engine but not yet run
}

// maxInflight bounds frames a client may have queued on the engine loop
// before it is disconnected. The loop drains far faster than any real
// client sends; the cap exists to stop a flooding client from growing the
// engine queue without bound.
const maxInflight = 256

func newClient(s *Server, conn net.Conn) *client {
	return &client{s: s, conn: conn, writeCh: make(chan []byte, 256), lastActivity: time.Now()}
}

// LastActivity returns the time of the last received data.
func (c *client) LastActivity() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastActivity
}

// enqueue is called on the engine loop (from Server.OnRawRX). Non-blocking;
// a full channel closes the slow client rather than stalling the loop.
func (c *client) enqueue(frame []byte) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	select {
	case c.writeCh <- frame:
		c.mu.Unlock()
	default:
		c.mu.Unlock()
		log.Printf("kisstcp: write channel full for %s, closing", c.conn.RemoteAddr())
		c.close()
	}
}

func (c *client) run() {
	// Register on the engine loop; reject if at capacity.
	accepted := make(chan bool, 1)
	c.s.eng.Do(func() { accepted <- c.s.add(c) })
	if !<-accepted {
		log.Printf("kisstcp: client limit reached, rejecting %s", c.conn.RemoteAddr())
		c.conn.Close()
		return
	}
	log.Printf("kisstcp: client connected from %s", c.conn.RemoteAddr())

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for pkt := range c.writeCh {
			if _, err := c.conn.Write(pkt); err != nil {
				return
			}
		}
	}()

	var dec kiss.Decoder
	var lastDropped uint64
	rbuf := make([]byte, 4096)
readLoop:
	for {
		n, err := c.conn.Read(rbuf)
		if n > 0 {
			c.mu.Lock()
			c.lastActivity = time.Now()
			c.mu.Unlock()
			frames := dec.Feed(rbuf[:n])
			if dec.DroppedOversize != lastDropped {
				lastDropped = dec.DroppedOversize
				log.Printf("kisstcp: dropping oversize (> %d bytes) frame from %s (total %d)",
					kiss.MaxFrameSize, c.conn.RemoteAddr(), lastDropped)
			}
			for _, frame := range frames {
				if !c.dispatch(frame) {
					break readLoop // over inflight cap — closing
				}
			}
		}
		if err != nil {
			break
		}
	}

	c.close()
	<-writerDone
	done := make(chan struct{})
	c.s.eng.Do(func() { c.s.remove(c); close(done) })
	<-done
	log.Printf("kisstcp: client disconnected from %s", c.conn.RemoteAddr())
}

// dispatch classifies one reassembled KISS frame (cmd byte + payload) and
// marshals the action onto the engine loop. Returns false when the client
// has exceeded the inflight cap and is being closed.
func (c *client) dispatch(frame []byte) bool {
	if len(frame) < 1 {
		return true
	}
	cmdByte := frame[0]
	port := int(cmdByte >> 4)
	cmdType := cmdByte & 0x0F
	payload := append([]byte{}, frame[1:]...)

	switch {
	case cmdType == 0x00: // data
		return c.post(func() { c.s.b.SendToKISS(port, payload) })
	case cmdType >= 0x01 && cmdType <= 0x06: // timing params + SetHardware
		return c.post(func() { c.s.b.SendKISSCommand(port, cmdType, payload) })
	case cmdType == 0x0F: // exit-KISS — never forward
		log.Printf("kisstcp: dropping exit-KISS from %s (protecting shared TNC)", c.conn.RemoteAddr())
	default:
		log.Printf("kisstcp: dropping unknown KISS command %#x from %s", cmdType, c.conn.RemoteAddr())
	}
	return true
}

// post increments the inflight counter and marshals fn onto the engine loop.
// Over the cap it logs, closes the client, and returns false.
func (c *client) post(fn func()) bool {
	c.mu.Lock()
	if c.inflight >= maxInflight {
		c.mu.Unlock()
		log.Printf("kisstcp: client %s over inflight cap (%d), closing", c.conn.RemoteAddr(), maxInflight)
		c.close()
		return false
	}
	c.inflight++
	c.mu.Unlock()
	c.s.eng.Do(func() {
		fn()
		c.mu.Lock()
		c.inflight--
		c.mu.Unlock()
	})
	return true
}

func (c *client) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.conn.Close()
		close(c.writeCh)
	}
}
