// Package rigctl serves the hamlib Net rigctl protocol over TCP, so PAT and
// other AGWPE clients can QSY a radio through tncd the same way they'd talk
// to rigctld.
//
// One listener per port: hamlib's Net rigctl protocol has no way to select
// among several rigs on one socket, so a listener-per-radio is the only
// compatible arrangement.
package rigctl

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/netutil"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
)

// defaultIdleTimeout closes a rigctl connection that has sent no command in
// this long. It exists only to reap abandoned connections -- a NAT that
// silently dropped the FIN, a monitoring probe that connects and never
// disconnects -- not to punish a legitimately idle client: a rigctl client
// (PAT in particular) is idle most of the time by nature, holding the
// connection open between QSYs for the life of a session. 30 minutes is far
// longer than any realistic gap between hamlib polls (get_freq/get_ptt happen
// on the order of seconds when they happen at all) while still bounding the
// goroutine/fd leak from a connection nobody is using.
const defaultIdleTimeout = 30 * time.Minute

// hamlib rig_errcode_e values, negated on the wire as "RPRT -n". Pinned
// against hamlib's include/hamlib/rig.h so PAT's rigctld client parses them
// as intended, not just as "some nonzero code".
const (
	rprtOK       = "RPRT 0"  // RIG_OK
	rprtEINVAL   = "RPRT -1" // RIG_EINVAL: bad or missing argument
	rprtENIMPL   = "RPRT -4" // RIG_ENIMPL: not implemented
	rprtETIMEOUT = "RPRT -5" // RIG_ETIMEOUT: radio did not reply
	rprtEIO      = "RPRT -6" // RIG_EIO: port offline or relinking
	rprtEPROTO   = "RPRT -8" // RIG_EPROTO: malformed reply from the radio
)

// Rig is the subset of radio control the server needs. Keeping it a local
// interface (rather than depending on *rig.Rig concretely) is what lets the
// protocol logic be table-tested with a fake, with no socket and no radio.
type Rig interface {
	SetFreq(hz uint32) error
	GetFreq() (uint32, error)
	SetPTT(on bool) error
	GetPTT() (bool, error)
}

// pttState tracks an active key so a later task can force-release it on
// PTTTimeout. Unused by handleSetPTT until that task lands, but the type
// lives here now so handleLine's signature doesn't change under it.
type pttState struct {
	mu    sync.Mutex
	timer *time.Timer
	keyed bool
}

// errToRPRT maps a rig error onto the hamlib code a client expects.
func errToRPRT(err error) string {
	switch {
	case err == nil:
		return rprtOK
	case errors.Is(err, rig.ErrTimeout):
		return rprtETIMEOUT
	case errors.Is(err, rig.ErrClosed):
		return rprtEIO
	default:
		return rprtEPROTO
	}
}

// handleLine executes one protocol line and returns the reply (without the
// trailing newline the caller adds on the wire).
//
// A nil rig means the port is offline or mid-relink: answer RPRT -6 at once
// rather than touching the radio, so a wedged Bluetooth port cannot turn into
// a hung rigctl client. Commands answerable without the radio (chk_vfo,
// dump_caps) are handled before that check so a liveness ping still works
// while a port is cycling.
func handleLine(line string, r Rig, cfg config.RigCtl, st *pttState) string {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) == 0 {
		return rprtEINVAL
	}
	cmd := fields[0]
	args := fields[1:]

	switch cmd {
	case `\chk_vfo`:
		// Answering "no VFO mode" is deliberate: PAT then uses an empty VFO
		// prefix and never prepends VFO arguments to get_freq/set_freq,
		// which removes a whole class of argument parsing this server would
		// otherwise need. Do not "improve" this to report VFO support.
		return "CHKVFO 0"
	case "dump_caps", `\dump_caps`:
		// PAT uses this only as a liveness ping; any non-error reply satisfies it.
		return rprtOK
	}

	if rigIsNil(r) {
		return rprtEIO
	}

	switch cmd {
	case `\get_freq`, "f":
		hz, err := r.GetFreq()
		if err != nil {
			return errToRPRT(err)
		}
		// A bare integer, Hz, nothing else on the line -- that's the whole
		// contract for get_freq's reply.
		return strconv.FormatUint(uint64(hz), 10)

	case `\set_freq`, "F":
		if len(args) < 1 {
			return rprtEINVAL
		}
		hz, err := strconv.ParseUint(args[0], 10, 32)
		if err != nil {
			return rprtEINVAL
		}
		return errToRPRT(r.SetFreq(uint32(hz)))

	case "t", `\get_ptt`:
		on, err := r.GetPTT()
		if err != nil {
			return errToRPRT(err)
		}
		if on {
			return "1"
		}
		return "0"

	case `\set_ptt`, "T":
		return handleSetPTT(args, r, cfg, st)

	default:
		return rprtEINVAL
	}
}

// rigIsNil reports whether r is nil, including the classic Go footgun where a
// typed nil pointer (e.g. (*rig.Rig)(nil)) is wrapped in a non-nil Rig
// interface value. A plain "r == nil" misses that case -- the interface
// value itself is non-nil even though the concrete pointer it holds is --
// and the first method call on it panics on a nil receiver. A future
// provider that ever returns a typed nil instead of an untyped one (an easy
// mistake: "return r, nil" when r is a *rig.Rig local variable that's still
// nil) must not turn into a panic in the one listener that can retune, and
// soon key, a transmitter.
func rigIsNil(r Rig) bool {
	if r == nil {
		return true
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// handleSetPTT is a stub: PTT keying (gated behind AllowPTT, with a hard
// maximum key time so a swallowed un-key over a wedged Bluetooth link can't
// leave the transmitter stuck on) is a later task. Answering ENIMPL here is
// truthful -- this build does not implement it yet -- and safe, since a
// client that gets ENIMPL will not believe it keyed anything.
func handleSetPTT(args []string, r Rig, cfg config.RigCtl, st *pttState) string {
	return rprtENIMPL
}

// Server owns one rigctl listener for one tncd port.
type Server struct {
	cfg config.RigCtl
	// provider returns the port's current Rig, or (nil, nil)/an error when
	// the port is offline or mid-relink. Called fresh per request (not
	// cached at Accept time) so a port that relinks mid-session is picked up
	// on the very next command instead of wedging the connection to a stale
	// handle.
	provider func() (Rig, error)
	ptt      *pttState

	// idleTimeout bounds how long a connection may go without sending a
	// command; see defaultIdleTimeout. A field (not just the constant) so
	// tests can shrink it instead of waiting 30 real minutes.
	idleTimeout time.Duration

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
}

// New creates a Server for one [rigctl.N] section. provider is called once
// per request to resolve the live Rig for that port; it must not block.
func New(cfg config.RigCtl, provider func() (Rig, error)) *Server {
	return &Server{cfg: cfg, provider: provider, ptt: &pttState{}, idleTimeout: defaultIdleTimeout}
}

// Start binds the listener, wraps it in the shared allowlist filter, and
// begins accepting connections in the background. Returns once the listener
// is bound so callers can log/inspect Addr() immediately.
func (s *Server) Start() error {
	addr := net.JoinHostPort(s.cfg.ListenHost, strconv.Itoa(s.cfg.ListenPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("rigctl: listen %s: %w", addr, err)
	}
	ln = netutil.WrapListener(ln, s.cfg.AllowedSubnets, "rigctl")

	s.mu.Lock()
	s.ln = ln
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()

	log.Printf("rigctl: listening on %s", ln.Addr())
	go s.acceptLoop(ln)
	return nil
}

// Addr returns the listener's actual address (useful when port 0 was requested).
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		if !s.trackConn(conn) {
			conn.Close() // Close raced the accept loop; shutting down.
			return
		}
		go s.handleConn(conn)
	}
}

func (s *Server) trackConn(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns == nil {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *Server) untrackConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

// rig resolves the current Rig for this port, folding a nil provider or a
// provider error into "unavailable" (nil) so handleLine's single nil check
// covers every offline case -- no separate error plumbing into the protocol
// layer.
func (s *Server) rig() Rig {
	if s.provider == nil {
		return nil
	}
	r, err := s.provider()
	if err != nil {
		return nil
	}
	return r
}

// handleConn runs the request loop for one client: one goroutine per
// connection, entirely off tncd's engine goroutine (which owns all AX.25
// state), so a slow or wedged rigctl client can never stall packet on any
// port. A rig request is resolved fresh via s.rig() and answered
// synchronously by handleLine -- no work here ever touches the engine.
//
// The read deadline is refreshed before every line, not set once: that is
// what makes it an IDLE timeout rather than a session-length cap. A client
// issuing commands more often than idleTimeout (PAT's normal usage) never
// trips it; a connection that goes silent -- abandoned, or a NAT that ate the
// FIN -- gets reaped instead of parking its goroutine and fd for the life of
// the process. This package deliberately has no dependency on
// internal/engine, so a per-connection deadline stands in for the sibling
// kisstcp frontend's engine-driven idle sweep.
func (s *Server) handleConn(conn net.Conn) {
	defer s.untrackConn(conn)
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	for {
		if err := conn.SetReadDeadline(time.Now().Add(s.idleTimeout)); err != nil {
			return
		}
		if !scanner.Scan() {
			return
		}
		line := scanner.Text()
		if strings.TrimSpace(line) == "q" {
			return // hamlib's Net rigctl "quit": close, no reply expected
		}
		reply := handleLine(line, s.rig(), s.cfg, s.ptt)
		if _, err := conn.Write([]byte(reply + "\n")); err != nil {
			return
		}
	}
}

// Close stops accepting new connections and drops every connected client.
func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.ln
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()

	var err error
	if ln != nil {
		err = ln.Close()
	}
	for c := range conns {
		c.Close()
	}
	return err
}
