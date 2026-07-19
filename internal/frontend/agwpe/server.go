// Package agwpe implements the AGWPE TCP server frontend.
// It accepts connections from AGWPE clients (PAT/Winlink, Paracon, Xastir)
// and bridges them to the AX.25 L2 state machine via Bridge.
package agwpe

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	agwpepkg "github.com/ben-kuhn/tncd/v2/agwpe"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

// Serve starts the AGWPE TCP server on host:port, posting each complete frame
// to the engine for serial dispatch. Returns a net.Listener the caller can
// Close to stop accepting new connections.
func Serve(eng *engine.Engine, b *bridge.Bridge, host string, port int) (net.Listener, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("agwpe: listen %s: %w", addr, err)
	}
	log.Printf("agwpe: listening on %s", addr)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				// Listener was closed — exit gracefully.
				return
			}
			go newClient(eng, b, conn).run()
		}
	}()

	return ln, nil
}

// client holds per-connection state for one AGWPE TCP client.
// It implements bridge.Client.
type client struct {
	eng  *engine.Engine
	b    *bridge.Bridge
	conn net.Conn

	// writeCh buffers outbound AGWPE frames (cap 256). Overflow closes client.
	writeCh chan []byte

	mu              sync.Mutex
	monitoring      bool
	registeredCalls map[string]bool
	lastActivity    time.Time
	closed          bool
}

func newClient(eng *engine.Engine, b *bridge.Bridge, conn net.Conn) *client {
	return &client{
		eng:             eng,
		b:               b,
		conn:            conn,
		writeCh:         make(chan []byte, 256),
		registeredCalls: make(map[string]bool),
		lastActivity:    time.Now(),
	}
}

// run is called in its own goroutine per connection. It registers with the
// bridge, starts the writer goroutine, then reads and dispatches frames.
func (c *client) run() {
	// Register with bridge on engine loop; if limit reached, close immediately.
	accepted := make(chan bool, 1)
	c.eng.Do(func() {
		ok := c.b.AddClient(c)
		accepted <- ok
	})
	if !<-accepted {
		log.Printf("agwpe: client limit reached, rejecting %s", c.conn.RemoteAddr())
		c.conn.Close()
		return
	}

	log.Printf("agwpe: client connected from %s", c.conn.RemoteAddr())

	// Start write goroutine.
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for pkt := range c.writeCh {
			if _, err := c.conn.Write(pkt); err != nil {
				return
			}
		}
	}()

	// Read loop: reassemble AGWPE frames.
	buf := make([]byte, 0, 512)
	rbuf := make([]byte, 4096)

	for {
		n, err := c.conn.Read(rbuf)
		if err != nil {
			if err != io.EOF {
				log.Printf("agwpe: read error from %s: %v", c.conn.RemoteAddr(), err)
			}
			break
		}
		buf = append(buf, rbuf[:n]...)

		// Update last activity (mirrors Python data_received).
		c.mu.Lock()
		c.lastActivity = time.Now()
		c.mu.Unlock()

		// Process as many complete frames as possible.
		for {
			if len(buf) < agwpepkg.HeaderSize {
				break
			}
			hdr, err := agwpepkg.ParseHeader(buf[:agwpepkg.HeaderSize])
			if err != nil {
				log.Printf("agwpe: header parse error: %v", err)
				buf = buf[:0]
				break
			}

			// Oversize payload → close (tncd.py:166-171).
			if hdr.DataLen > agwpepkg.MaxPayload {
				log.Printf("agwpe: oversized payload (%d bytes) from %s, closing",
					hdr.DataLen, c.conn.RemoteAddr())
				goto done
			}

			total := agwpepkg.HeaderSize + int(hdr.DataLen)
			if len(buf) < total {
				break // wait for more data
			}

			payload := make([]byte, hdr.DataLen)
			copy(payload, buf[agwpepkg.HeaderSize:total])
			buf = buf[total:]

			// Post to engine for serial dispatch.
			h := hdr
			p := payload
			c.eng.Do(func() { c.handleFrame(h, p) })
		}
	}

done:
	c.conn.Close()
	c.mu.Lock()
	c.closed = true
	close(c.writeCh)
	c.mu.Unlock()
	<-writerDone

	// Remove from bridge on engine loop.
	done2 := make(chan struct{})
	c.eng.Do(func() {
		c.b.RemoveClient(c)
		close(done2)
	})
	<-done2
	log.Printf("agwpe: client disconnected from %s", c.conn.RemoteAddr())
}

// --- bridge.Client interface ---

// SendAGWPE queues an AGWPE frame for the write goroutine.
// Must be safe to call from the engine loop (it is — it only writes to a channel).
func (c *client) SendAGWPE(port uint8, kind byte, pid uint8, from, to string, data []byte) {
	pkt := agwpepkg.Build(port, kind, pid, from, to, data)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	select {
	case c.writeCh <- pkt:
		c.mu.Unlock()
	default:
		c.mu.Unlock()
		log.Printf("agwpe: write channel full for %s, closing", c.conn.RemoteAddr())
		c.CloseTransport()
	}
}

// Monitoring returns whether this client has monitoring enabled.
func (c *client) Monitoring() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.monitoring
}

// RegisteredCalls returns the set of callsigns registered via 'X' frames.
// The returned map must only be accessed on the engine loop.
func (c *client) RegisteredCalls() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.registeredCalls
}

// LastActivity returns the time of the last received data.
func (c *client) LastActivity() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastActivity
}

// CloseTransport closes the underlying TCP connection, causing the read loop
// to exit. Safe to call multiple times.
func (c *client) CloseTransport() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.conn.Close()
	}
}
