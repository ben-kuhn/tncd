package kisstcp

import (
	"log"
	"net"
	"sync"

	"github.com/ben-kuhn/tncd/v2/kiss"
)

type client struct {
	s    *Server
	conn net.Conn

	writeCh chan []byte // buffered outbound KISS frames

	mu     sync.Mutex
	closed bool
}

func newClient(s *Server, conn net.Conn) *client {
	return &client{s: s, conn: conn, writeCh: make(chan []byte, 256)}
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
	rbuf := make([]byte, 4096)
	for {
		n, err := c.conn.Read(rbuf)
		if n > 0 {
			for _, frame := range dec.Feed(rbuf[:n]) {
				c.dispatch(frame)
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
// marshals the action onto the engine loop.
func (c *client) dispatch(frame []byte) {
	if len(frame) < 1 {
		return
	}
	cmdByte := frame[0]
	port := int(cmdByte >> 4)
	cmdType := cmdByte & 0x0F
	payload := append([]byte{}, frame[1:]...)

	switch {
	case cmdType == 0x00: // data
		c.s.eng.Do(func() { c.s.b.SendToKISS(port, payload) })
	case cmdType >= 0x01 && cmdType <= 0x06: // timing params + SetHardware
		c.s.eng.Do(func() { c.s.b.SendKISSCommand(port, cmdType, payload) })
	case cmdType == 0x0F: // exit-KISS — never forward
		log.Printf("kisstcp: dropping exit-KISS from %s (protecting shared TNC)", c.conn.RemoteAddr())
	default:
		log.Printf("kisstcp: dropping unknown KISS command %#x from %s", cmdType, c.conn.RemoteAddr())
	}
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
