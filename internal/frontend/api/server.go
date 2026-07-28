// Package api implements a read-only HTTP monitoring API (JSON + SSE).
// It registers as a bridge sink for RX/TX frames and connection events, and
// snapshots bridge/l2 state on the engine loop for the GET endpoints.
package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/internal/version"
)

type Server struct {
	eng        *engine.Engine
	b          *bridge.Bridge
	ln         net.Listener
	httpSrv    *http.Server
	maxClients int
	clients    map[*sseClient]struct{} // engine-loop only
}

type sseClient struct {
	ch chan []byte // buffered SSE frames
}

// Serve starts the API server and registers its sinks. Registration is
// marshalled onto the engine loop (safe during setup or while running).
func Serve(eng *engine.Engine, b *bridge.Bridge, host string, port, maxClients int, serveUI bool) (*Server, error) {
	addr := fmt.Sprintf("%s:%d", host, port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("api: listen %s: %w", addr, err)
	}
	s := &Server{eng: eng, b: b, ln: ln, maxClients: maxClients, clients: make(map[*sseClient]struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/connections", s.handleConnections)
	mux.HandleFunc("/api/events", s.handleEvents)
	if serveUI {
		h, err := uiHandler()
		if err != nil {
			ln.Close()
			return nil, fmt.Errorf("api: ui handler: %w", err)
		}
		mux.Handle("/", h)
	}
	s.httpSrv = &http.Server{Handler: mux}

	eng.Do(func() {
		b.RegisterMonitorSink(s)
		b.RegisterTxFrameSink(s)
		b.RegisterConnSink(s)
	})
	log.Printf("api: listening on %s", ln.Addr())
	go s.httpSrv.Serve(ln)
	return s, nil
}

func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close unregisters sinks, stops the HTTP server, and drops SSE clients.
// MUST be called on the engine loop (touches sink registry + clients map).
func (s *Server) Close() {
	s.b.UnregisterMonitorSink(s)
	s.b.UnregisterTxFrameSink(s)
	s.b.UnregisterConnSink(s)
	// http.Server.Close force-closes listeners+connections and returns without
	// joining handler goroutines (unlike Shutdown), so calling it on the engine
	// loop cannot deadlock against a handler parked in eng.Do.
	s.httpSrv.Close()
	for c := range s.clients {
		close(c.ch)
	}
	s.clients = map[*sseClient]struct{}{}
}

// --- GET handlers (snapshot on the loop, serialize off it) ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	var ports []bridge.PortStatus
	done := make(chan struct{})
	s.eng.Do(func() { ports = s.b.StatusPorts(); close(done) })
	<-done
	writeJSON(w, map[string]any{"version": version.Version, "ports": ports})
}

func (s *Server) handleConnections(w http.ResponseWriter, r *http.Request) {
	var conns any
	done := make(chan struct{})
	s.eng.Do(func() { conns = s.b.ConnectionSnapshot(); close(done) })
	<-done
	writeJSON(w, map[string]any{"connections": conns})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// --- SSE ---

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	c := &sseClient{ch: make(chan []byte, 256)}
	admitted := make(chan bool, 1)
	s.eng.Do(func() {
		if s.maxClients > 0 && len(s.clients) >= s.maxClients {
			admitted <- false
			return
		}
		s.clients[c] = struct{}{}
		admitted <- true
	})
	if !<-admitted {
		http.Error(w, "too many clients", http.StatusServiceUnavailable)
		return
	}
	defer s.eng.Do(func() { delete(s.clients, c) })

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	for {
		select {
		case msg, open := <-c.ch:
			if !open {
				return
			}
			if _, err := w.Write(msg); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// push sends a pre-framed SSE message to all clients, non-blocking. Called on
// the loop. A full client channel drops the message (slow client), never blocks.
func (s *Server) push(eventType string, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, data))
	for c := range s.clients {
		select {
		case c.ch <- msg:
		default:
		}
	}
}

// --- bridge sink implementations (all called on the engine loop) ---

func (s *Server) OnRXFrame(port int, f *ax25.Frame) { s.push("rx", encodeFrame(port, f)) }
func (s *Server) OnTXFrame(port int, f *ax25.Frame) { s.push("tx", encodeFrame(port, f)) }

func (s *Server) OnConn(e bridge.ConnEvent) {
	if e.State == "connected" {
		s.push("connect", connectEvent{Port: e.Port, Local: e.Local, Remote: e.Remote, Incoming: e.Incoming})
	} else {
		s.push("disconnect", disconnectEvent{Port: e.Port, Local: e.Local, Remote: e.Remote})
	}
}
