// Package kisstcp implements a Direwolf-8001-style KISS-over-TCP passthrough.
// Connected clients hear every frame received from the air and can transmit;
// TX shares the per-port queue with the L2 engine. Registered as a
// bridge.RawRXSink. Mirrors the AGWPE frontend's goroutine↔engine-loop pattern.
package kisstcp

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/internal/netutil"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// Server owns the listener and the set of connected clients. The client set is
// mutated and read only on the engine loop.
type Server struct {
	eng        *engine.Engine
	b          *bridge.Bridge
	ln         net.Listener
	maxClients int
	clients    map[*client]struct{} // engine-loop only

	// idleTimeout is how long a client may stay silent before the sweep
	// closes it; <= 0 disables the sweep. sweepTimer is cancelled on Close.
	idleTimeout time.Duration
	sweepTimer  *engine.Timer
}

// Serve starts the listener and registers the server as a RawRXSink.
// Connections from outside allow are rejected at accept time. Clients idle
// longer than idleTimeout seconds are reaped by a periodic sweep (<=0
// disables). Registration is marshalled onto the engine loop (safe whether
// Serve is called during setup before engine.Run — where it simply queues —
// or after).
func Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients, idleTimeout int, allow netutil.Allowlist) (*Server, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("kisstcp: listen %s: %w", addr, err)
	}
	ln = netutil.WrapListener(ln, allow, "kisstcp")
	s := &Server{eng: eng, b: b, ln: ln, maxClients: maxClients, clients: make(map[*client]struct{})}
	if idleTimeout > 0 {
		s.idleTimeout = time.Duration(idleTimeout) * time.Second
	}
	eng.Do(func() { b.RegisterRawRXSink(s) })
	if s.idleTimeout > 0 {
		eng.Do(s.scheduleIdleSweep)
	}
	log.Printf("kisstcp: listening on %s", ln.Addr())

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go newClient(s, conn).run()
		}
	}()
	return s, nil
}

// Addr returns the listener's actual address (useful when port 0 was requested).
func (s *Server) Addr() string { return s.ln.Addr().String() }

// OnRawRX wraps a heard frame as a KISS data frame (port nibble = port) and
// enqueues it to every connected client. Called on the engine loop.
//
// Multiport nibble contract: the KISS command-byte high nibble carries the
// tncd port index in both directions. RX frames are wrapped with the frame's
// port in the high nibble (WrapData(port, raw)), so connected clients can
// distinguish which tncd port a frame arrived on. When a client transmits, the
// high nibble N in its KISS command byte routes the frame to SendToKISS(N, …).
// The nibble written to each physical TNC is always 0 (one TNC per tncd port).
// This asymmetry is intentional, not a bug.
func (s *Server) OnRawRX(port int, raw []byte) {
	frame := kiss.WrapData(uint8(port), raw)
	for c := range s.clients {
		c.enqueue(frame)
	}
}

// add/remove run on the engine loop (via eng.Do from client goroutines).
func (s *Server) add(c *client) bool {
	if s.maxClients > 0 && len(s.clients) >= s.maxClients {
		return false
	}
	s.clients[c] = struct{}{}
	return true
}
func (s *Server) remove(c *client) { delete(s.clients, c) }

// scheduleIdleSweep arms the next sweep. The interval is min(30s,
// idleTimeout) so the reap delay never exceeds the timeout itself by more
// than one timeout. Runs on the engine loop.
func (s *Server) scheduleIdleSweep() {
	interval := 30 * time.Second
	if s.idleTimeout < interval {
		interval = s.idleTimeout
	}
	s.sweepTimer = s.eng.After(interval, s.sweepIdleClients)
}

// sweepIdleClients closes clients silent for longer than idleTimeout and
// re-arms the sweep. Mirrors the AGWPE idle sweep in the bridge.
// Runs on the engine loop.
func (s *Server) sweepIdleClients() {
	now := time.Now()
	for c := range s.clients {
		if now.Sub(c.LastActivity()) > s.idleTimeout {
			log.Printf("kisstcp: closing idle client %s", c.conn.RemoteAddr())
			c.close()
		}
	}
	s.scheduleIdleSweep()
}

// Close stops accepting, unregisters the sink, and closes all clients. It
// accesses engine-loop-only state (the bridge sink registry and the clients
// map), so it MUST be called on the engine loop — e.g. from within eng.Do(...)
// in the shutdown sequence. Do not call it directly from another goroutine.
func (s *Server) Close() {
	if s.sweepTimer != nil {
		s.sweepTimer.Cancel()
		s.sweepTimer = nil
	}
	s.b.UnregisterRawRXSink(s)
	s.ln.Close()
	for c := range s.clients {
		c.close()
	}
}
