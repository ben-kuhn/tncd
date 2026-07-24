// Package kisstcp implements a Direwolf-8001-style KISS-over-TCP passthrough.
// Connected clients hear every frame received from the air and can transmit;
// TX shares the per-port queue with the L2 engine. Registered as a
// bridge.RawRXSink. Mirrors the AGWPE frontend's goroutine↔engine-loop pattern.
package kisstcp

import (
	"fmt"
	"log"
	"net"

	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
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
}

// Serve starts the listener and registers the server as a RawRXSink.
// Registration is marshalled onto the engine loop (safe whether Serve is called
// during setup before engine.Run — where it simply queues — or after).
func Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int) (*Server, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("kisstcp: listen %s: %w", addr, err)
	}
	s := &Server{eng: eng, b: b, ln: ln, maxClients: maxClients, clients: make(map[*client]struct{})}
	eng.Do(func() { b.RegisterRawRXSink(s) })
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

// Close stops accepting, unregisters the sink, and closes all clients.
// Called on the engine loop from the shutdown sequence.
func (s *Server) Close() {
	s.b.UnregisterRawRXSink(s)
	s.ln.Close()
	for c := range s.clients {
		c.close()
	}
}
