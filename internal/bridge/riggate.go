package bridge

import (
	"fmt"
	"log"
	"sync"

	"github.com/ben-kuhn/tncd/v2/internal/rig"
)

// rigGate decides when a port's radio should be in packet configuration.
//
// Two independent kinds of holder keep it there, matching the two ways a radio
// gets used for packet:
//
//   - AX.25 connections on the port. Counted from l2's PortSessions hook, which
//     fires when a connection is CREATED rather than when it completes, so the
//     radio is configured before the SABM goes out rather than after the UA.
//   - Connected rigctl clients. PAT's flow is: connect rigctl, set_freq, AX.25
//     connect, transfer, disconnect, QSX, drop rigctl -- so the rigctl
//     connection outlives the AX.25 session, and releasing when the last
//     connection ends would undo the QSY while PAT is still using the radio.
//
// Settings are per RADIO but tncd supports several simultaneous connections per
// port, so this refcounts and acts only on the 0 -> 1 and 1 -> 0 transitions.
//
// Counting and radio I/O are deliberately separated. The counts change on the
// engine goroutine (l2) and on rigctl connection goroutines, while applying
// settings is a blocking round trip to the radio that must never run on the
// engine loop -- a 5-second rig timeout there would stall every port. So the
// counts are mutex-guarded and a single reconcile goroutine per port drives the
// radio toward the desired state. Reconciling rather than acting on each event
// is what makes a rapid acquire/release/acquire sequence land correctly instead
// of depending on which goroutine won.
type rigGate struct {
	mu       sync.Mutex
	sessions int
	clients  int
	// qsy latches once a QSY has happened, and clears on release. A QSY is a
	// TRIGGER but not a HOLDER: PAT QSYs, runs its session, QSXes back and only
	// then drops rigctl, so the tune alone must not keep the radio held forever
	// once everything has let go.
	qsy bool
	// actual is what the radio is believed to be in; busy means a reconcile
	// goroutine is already converging it toward the desired state.
	actual bool
	busy   bool

	// ioMu serializes the radio I/O itself, so the inline acquire a QSY needs
	// and the background reconcile can never run apply and release at once.
	ioMu sync.Mutex

	// apply and release do the radio I/O. Injected so this is testable without
	// a radio.
	apply   func() error
	release func() error
	// name identifies the port in log messages.
	name string
}

func newRigGate(name string, apply, release func() error) *rigGate {
	return &rigGate{name: name, apply: apply, release: release}
}

// setSessions records the port's AX.25 connection count.
func (g *rigGate) setSessions(n int) {
	g.mu.Lock()
	g.sessions = n
	g.mu.Unlock()
	g.reconcile()
}

// addClient and removeClient track connected rigctl clients.
func (g *rigGate) addClient() {
	g.mu.Lock()
	g.clients++
	g.mu.Unlock()
	g.reconcile()
}

func (g *rigGate) removeClient() {
	g.mu.Lock()
	if g.clients > 0 {
		g.clients--
	}
	g.mu.Unlock()
	g.reconcile()
}

// acquireForQSY holds the radio BEFORE a tune, synchronously.
//
// Ordering is load-bearing and this is why the hook is synchronous: SetFreq
// rewrites whatever record the active VFO points at, and it REFUSES while that
// is a named memory. Acquiring afterwards, or in the background, would mean the
// tune either fails or -- worse, on a radio whose guards did not catch it --
// overwrites the operator's memory channel.
//
// An error is returned rather than swallowed so the caller can decide; a QSY on
// an unmanaged radio may still succeed, and refusing to tune because the session
// settings could not be applied would be worse than tuning without them.
func (g *rigGate) acquireForQSY() error {
	g.mu.Lock()
	g.qsy = true
	g.mu.Unlock()
	return g.drain()
}

// desiredLocked is whether the radio should be in packet configuration.
//
// It needs BOTH a trigger and a holder. A trigger is an AX.25 session or a QSY:
// those are the things that mean "packet is happening". A holder is an AX.25
// session or a connected rigctl client: those are the things that mean "packet
// is still happening". A rigctl client is deliberately a holder and NOT a
// trigger -- a monitoring client that connects and polls get_freq has no
// business reconfiguring somebody's radio.
func (g *rigGate) desiredLocked() bool {
	holder := g.sessions > 0 || g.clients > 0
	trigger := g.sessions > 0 || g.qsy
	return holder && trigger
}

// step performs at most one transition toward the desired state, serialized
// against every other caller. Reports whether it did any work.
func (g *rigGate) step() (bool, error) {
	g.ioMu.Lock()
	defer g.ioMu.Unlock()

	g.mu.Lock()
	want, have := g.desiredLocked(), g.actual
	g.mu.Unlock()
	if want == have {
		return false, nil
	}

	var err error
	if want {
		err = g.apply()
	} else {
		err = g.release()
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case err == nil:
		g.actual = want
		if !want {
			g.qsy = false
		}
	case !want:
		// A failed release is NOT left pending for a retry loop: the radio may
		// simply be gone, and since these writes reach NVRAM there is no
		// power-cycle safety net to wait for. Record it as released and say so
		// loudly -- the operator needs to know their radio is still configured
		// for packet, and the exact command to undo it.
		g.actual = false
		g.qsy = false
	}
	return true, err
}

// drain converges the radio toward the desired state, re-reading that state
// after every step so events arriving mid-flight are picked up here rather than
// racing a second goroutine.
func (g *rigGate) drain() error {
	for {
		did, err := g.step()
		if err != nil {
			g.logStepError(err)
			return err
		}
		if !did {
			return nil
		}
	}
}

func (g *rigGate) logStepError(err error) {
	g.mu.Lock()
	held := g.actual
	g.mu.Unlock()
	if held {
		log.Printf("rig: %s: could not apply session settings (%v) -- "+
			"continuing with the radio as the operator left it", g.name, err)
		return
	}
	log.Printf("rig: %s: FAILED to restore the radio's settings (%v) -- "+
		"it may still be in packet configuration; "+
		"use `tncd rig session-set` to put it back", g.name, err)
}

// reconcile converges in the background, since the counts change on the engine
// goroutine and on rigctl connection goroutines, while the radio I/O is a
// blocking round trip that must not run on either.
func (g *rigGate) reconcile() {
	g.mu.Lock()
	if g.busy || g.desiredLocked() == g.actual {
		g.mu.Unlock()
		return
	}
	g.busy = true
	g.mu.Unlock()
	go func() {
		defer func() {
			g.mu.Lock()
			g.busy = false
			g.mu.Unlock()
		}()
		_ = g.drain()
	}()
}

// shutdown releases the radio if it is held, synchronously, for the process exit
// path. Nothing else will put the radio back.
func (g *rigGate) shutdown() {
	g.mu.Lock()
	g.sessions, g.clients, g.qsy = 0, 0, false
	held := g.actual
	g.mu.Unlock()
	if !held {
		return
	}
	if _, err := g.step(); err != nil {
		log.Printf("rig: %s: FAILED to restore the radio's settings at shutdown (%v) -- "+
			"it is still in packet configuration; "+
			"use `tncd rig session-set` to put it back", g.name, err)
	}
}

// --- Bridge wiring ----------------------------------------------------------

// initRigGates builds one gate per port that has rig control enabled. Ports
// without [rigctl.N] get none, so nothing touches a radio an operator has not
// opted into handing over.
func (b *Bridge) initRigGates() {
	b.rigGates = make([]*rigGate, len(b.ports))
	for port := range b.rigGates {
		if port >= len(b.cfg.RigCtl) || !b.cfg.RigCtl[port].Enabled {
			continue
		}
		p := port
		name := portName(b, p)
		b.rigGates[p] = newRigGate(name,
			func() error { return withRig(b, p, func(r *rig.Rig) error { return r.AcquireSession() }) },
			func() error { return withRig(b, p, func(r *rig.Rig) error { return r.ReleaseSession() }) },
		)
	}
}

// portName is what the operator calls this port, for log messages.
func portName(b *Bridge, port int) string {
	if port < len(b.cfg.Ports) && b.cfg.Ports[port].Name != "" {
		return fmt.Sprintf("port %d (%s)", port, b.cfg.Ports[port].Name)
	}
	return fmt.Sprintf("port %d", port)
}

// withRig resolves the port's rig on the engine loop, then runs fn OFF it.
//
// The split matters: RigFor reads engine-owned state and must run there, while
// fn is a blocking round trip to the radio that must not. Running fn on the loop
// would stall every port for as long as the radio takes to answer.
func withRig(b *Bridge, port int, fn func(*rig.Rig) error) error {
	var (
		r   *rig.Rig
		err error
	)
	done := make(chan struct{})
	b.eng.Do(func() {
		r, err = b.RigFor(port)
		close(done)
	})
	<-done
	if err != nil {
		return err
	}
	return fn(r)
}

// onPortSessions is l2's PortSessions hook: the port's connection count changed.
func (b *Bridge) onPortSessions(port int, n int) {
	if port < 0 || port >= len(b.rigGates) || b.rigGates[port] == nil {
		return
	}
	b.rigGates[port].setSessions(n)
}

// RigClientConnected and RigClientDisconnected track rigctl clients, which hold
// the radio's settings for as long as they are connected.
//
// A rigctl client outliving the AX.25 session is the normal case, not an edge
// one: PAT connects rigctl, QSYs, runs the session, then QSXes back before
// dropping the connection. Releasing when the last AX.25 connection ended would
// undo the QSY underneath it.
func (b *Bridge) RigClientConnected(port int) {
	if port < 0 || port >= len(b.rigGates) || b.rigGates[port] == nil {
		return
	}
	b.rigGates[port].addClient()
}

// RigAcquireForQSY holds the radio before a tune, synchronously. See
// rigGate.acquireForQSY for why the ordering matters.
func (b *Bridge) RigAcquireForQSY(port int) error {
	if port < 0 || port >= len(b.rigGates) || b.rigGates[port] == nil {
		return nil
	}
	return b.rigGates[port].acquireForQSY()
}

func (b *Bridge) RigClientDisconnected(port int) {
	if port < 0 || port >= len(b.rigGates) || b.rigGates[port] == nil {
		return
	}
	b.rigGates[port].removeClient()
}

// shutdownRigGates restores every held radio, synchronously. Called from
// Shutdown: these writes reach NVRAM, so if this does not put the radio back
// nothing else will.
func (b *Bridge) shutdownRigGates() {
	for _, g := range b.rigGates {
		if g != nil {
			g.shutdown()
		}
	}
}
