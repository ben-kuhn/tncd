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

// pttState tracks an active key -- one per Server, shared across every
// connection to that port's listener -- so it can be force-released on
// PTTTimeout, on client disconnect, or on server Close. See handleSetPTT,
// releaseLocked, and forceReleasePTT.
type pttState struct {
	mu    sync.Mutex
	timer *time.Timer
	keyed bool
	// gen is bumped every time the safety timer is (re)armed. A timer
	// callback captures the gen it was armed with and checks it against the
	// current value before acting -- see armTimerLocked and timeoutFire.
	// This is what makes "refresh the timer without resending the key"
	// safe: Stop-then-Reset (or Stop-then-new-AfterFunc without a
	// generation check) cannot distinguish "I successfully cancelled the
	// old timer" from "the old timer already fired and its goroutine is
	// merely blocked behind me on st.mu" -- in the second case a naive
	// refresh would still let the stale callback run afterward and release
	// a key that a newer refresh just extended. The generation check closes
	// that window without needing to drain a channel or otherwise reason
	// about time.Timer's notoriously fiddly Stop/Reset semantics.
	gen uint64
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

// handleSetPTT keys or unkeys the transmitter, subject to allow_ptt and a
// hard maximum key time (cfg.PTTTimeout).
//
// The guard is not optional. A remote key whose un-key write can be
// silently swallowed by a wedged Bluetooth link (see CLAUDE.md) is a stuck
// transmitter, and that failure mode is known to occur on these radios.
// PTTTimeout's timer, wired here, is the backstop for that -- it does not
// depend on the client ever sending another command.
//
// RESIDUAL DRIFT RISK, for an operator deciding whether to set allow_ptt:
// this code does NOT read GetPTT() back after a key or unkey to confirm the
// physical bit actually followed. That was considered and rejected, not
// overlooked. A verification read is (*rig.Rig).GetPTT, which is just
// another round trip over request() on the SAME transport as the SetPTT it
// would be checking -- so the one failure mode this whole design exists to
// guard against (a wedged Bluetooth link that silently eats a write) is
// exactly as capable of silently eating, delaying, or stale-answering the
// verification read. A mismatch wouldn't be actionable either: since
// DO_PROG_FUNC toggles rather than sets (see (*rig.Rig).SetPTT), the only
// automatic response to "GetPTT disagrees with what I expected" would be to
// send another toggle -- which is just as likely to compound the drift as
// fix it, given no bench evidence pins down the timing or reliability of
// GET_HT_STATUS's is_in_tx bit relative to a DO_PROG_FUNC that was just
// sent. And PTTTimeout already bounds the worst case: whatever the physical
// state actually is, it is force-released within cfg.PTTTimeout regardless
// of what st.keyed believes, so an added round trip would buy confidence at
// the cost of latency on every key, without changing the outer bound on how
// long a stuck key can last. Net: st.keyed is tncd's best BELIEF about
// physical state, kept internally consistent by the keyLocked/releaseLocked
// guards, but it is never a bench-verified GUARANTEE, and nothing in this
// package closes that gap synchronously. The timer is the actual guarantee.
func handleSetPTT(args []string, r Rig, cfg config.RigCtl, st *pttState) string {
	if !cfg.AllowPTT {
		return rprtENIMPL
	}
	if len(args) < 1 {
		return rprtEINVAL
	}
	var on bool
	switch args[0] {
	case "0":
		on = false
	case "1", "3": // hamlib RIG_PTT_ON, RIG_PTT_ON_DATA -- both mean "key"
		on = true
	default:
		return rprtEINVAL
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if !on {
		return errToRPRT(releaseLocked(r, st))
	}
	return errToRPRT(keyLocked(r, st, time.Duration(cfg.PTTTimeout)*time.Second))
}

// keyLocked sends the key command -- but only if not already keyed -- and
// (re)arms the safety timer. Must be called with st.mu held.
//
// The already-keyed guard mirrors releaseLocked's guard in the opposite
// direction, and for the identical underlying reason: st.keyed only means
// anything if it actually tracks the radio's physical state, and that
// invariant has to be maintained on BOTH the key and release paths, not
// just the release side -- an invariant enforced on only one of two paths
// that mutate it is not an invariant at all. DO_PROG_FUNC carries no
// press/release parameter (see (*rig.Rig).SetPTT), so PFEffectMainPTT
// toggles a single physical bit on every call regardless of the argument:
// resending it while already keyed flips the transmitter back OFF on the
// wire while st.keyed keeps reading true. That divergence alone would just
// be a spurious unkey -- except every force-release path (the PTTTimeout
// timer, a client disconnect, Server.Close) trusts st.keyed and skips the
// radio once it reads false, so a diverged st.keyed doesn't just misreport
// state, it disarms the safety net that exists specifically to catch a
// stuck key. Confirmed by reproduction: key, key again (toggles OFF on the
// wire; st.keyed stays true because the old code never checked it), release
// (st.keyed still true, so the old releaseLocked sent ANOTHER toggle, which
// flipped the radio back ON) -- left the radio transmitting with every
// piece of tncd's own bookkeeping insisting it was not. This guard is what
// keeps a second "key" from ever creating that divergence in the first
// place: it is a no-op on the radio, and only refreshes the timer.
func keyLocked(r Rig, st *pttState, timeout time.Duration) error {
	if !st.keyed {
		if err := r.SetPTT(true); err != nil {
			return err
		}
		st.keyed = true
	}
	armTimerLocked(r, st, timeout)
	return nil
}

// armTimerLocked stops any existing timer and starts a fresh one, bumping
// st.gen so a callback from the timer it just replaced can recognize itself
// as stale (see pttState.gen) instead of firing a release that belongs to a
// generation that no longer exists. Must be called with st.mu held.
func armTimerLocked(r Rig, st *pttState, timeout time.Duration) {
	if st.timer != nil {
		st.timer.Stop()
	}
	st.gen++
	gen := st.gen
	st.timer = time.AfterFunc(timeout, func() { timeoutFire(r, st, gen) })
}

// timeoutFire is the PTTTimeout timer's callback. It first confirms -- under
// st.mu, atomically with the release itself, so there is no gap for a
// concurrent re-arm to slip through -- that no newer key/refresh has
// superseded it (see pttState.gen) before releasing. A stale generation
// means a later keyLocked call already re-armed a new timer that owns the
// next release; this one has nothing left to do.
func timeoutFire(r Rig, st *pttState, gen uint64) {
	st.mu.Lock()
	if st.gen != gen {
		st.mu.Unlock()
		return
	}
	err := releaseLocked(r, st)
	st.mu.Unlock()
	if err != nil {
		log.Printf("rigctl: PTT timeout force-release failed -- transmitter may still be keyed: %v", err)
	}
}

// releaseLocked sends the un-key command and clears the keyed/timer state.
// Must be called with st.mu held.
//
// It is a deliberate no-op on the radio when nothing is currently keyed,
// rather than resending the un-key command "just to be sure": DO_PROG_FUNC
// carries no press/release parameter (see (*rig.Rig).SetPTT's doc comment),
// so if PFEffectMainPTT turns out to be a toggle rather than a level,
// resending the identical wire bytes for a redundant release risks flipping
// the transmitter back ON -- exactly backwards from what a second "release"
// must do. A second/idempotent unkey -- whether it's the client calling
// set_ptt 0 twice, or a force-release landing after the client already
// released -- therefore short-circuits before touching the radio at all.
func releaseLocked(r Rig, st *pttState) error {
	if !st.keyed {
		return nil
	}
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
	}
	st.keyed = false
	if rigIsNil(r) {
		return rig.ErrClosed
	}
	return r.SetPTT(false)
}

// forceReleasePTT is releaseLocked for the two callers that don't already
// hold st.mu, aren't tied to a specific timer generation, and have no RPRT
// reply to give anyone: a client disconnecting while keyed, and the server
// closing while keyed. (The third force-release path, the PTTTimeout timer
// itself, goes through timeoutFire instead, which additionally checks
// pttState.gen before calling releaseLocked -- see its doc comment for
// why.) Wiring all three paths is the point of this task -- a keyed
// transmitter must not survive any of them.
//
// This is best-effort, not a guarantee. CLAUDE.md documents a bench-confirmed
// failure mode where a wedged Bluetooth link accepts a write and never
// delivers it to the TNC, so nothing here can prove the transmitter actually
// went quiet just because this function returned. There is deliberately no
// retry loop: retrying into a wedged transport is not meaningfully more
// likely to land than the first attempt, and (*rig.Rig).request's own
// timeout already bounds how long a single attempt can take -- looping would
// only multiply that latency without multiplying the odds of success. What
// this function DOES guarantee is that tncd's own bookkeeping (st.keyed, the
// timer) is cleared regardless of whether the radio ever heard about it, so
// tncd itself never believes a key is still down and never leaves a second
// timer running to fire later and confuse things further. A rig that is nil
// at release time (port offline, mid-relink) is the worst case -- there is
// no channel to send the un-key on at all -- and is logged loudly rather
// than silently swallowed, since that is the one situation where only a
// human (power off, or wait for the physical key timeout on the radio side)
// can guarantee the transmitter actually goes quiet.
func forceReleasePTT(r Rig, st *pttState) {
	st.mu.Lock()
	err := releaseLocked(r, st)
	st.mu.Unlock()
	if err != nil {
		log.Printf("rigctl: PTT force-release failed -- transmitter may still be keyed: %v", err)
	}
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
// per request to resolve the live Rig for that port, from a rigctl
// connection goroutine, never the engine goroutine. It may block briefly --
// the real wiring (internal/bridge's rigForPort) deliberately does a bounded
// round trip through the engine loop to look up which *rig.Rig, if any, is
// currently attached to the port -- but it must not perform radio I/O itself,
// and must never be called from the engine goroutine, which would deadlock
// against the very round trip it's waiting on.
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
	// A dropped TCP connection, a killed client, or a network partition must
	// not leave the transmitter keyed just because nobody is left to send
	// set_ptt 0. This is a no-op (see releaseLocked) unless this connection
	// -- or another one sharing the same server-wide pttState -- actually
	// left it keyed.
	defer forceReleasePTT(s.rig(), s.ptt)

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

	// tncd shutting down must not leave a transmitter keyed behind it. Do
	// this before tearing down the listener/conns below: those don't touch
	// the radio, so ordering relative to them doesn't matter, but doing it
	// first means a Close() that's interrupted or panics partway through
	// still attempted the release before anything else.
	forceReleasePTT(s.rig(), s.ptt)

	var err error
	if ln != nil {
		err = ln.Close()
	}
	for c := range conns {
		c.Close()
	}
	return err
}
