// Package l2 implements the AX.25 connected-mode layer-2 state machine.
// All state mutations must occur on the engine loop; callers are responsible
// for posting via engine.Do.
package l2

import (
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

// ConnState represents the AX.25 connection state.
type ConnState int

const (
	Disconnected  ConnState = iota
	Connecting              // SABM sent, awaiting UA
	Connected               // UA received / incoming SABM accepted
	Disconnecting           // DISC sent, awaiting UA
)

func (s ConnState) String() string {
	switch s {
	case Disconnected:
		return "DISCONNECTED"
	case Connecting:
		return "CONNECTING"
	case Connected:
		return "CONNECTED"
	case Disconnecting:
		return "DISCONNECTING"
	}
	return "UNKNOWN"
}

// Conn holds the state for a single AX.25 connected-mode session.
// The key is (Port, Local, Remote) — all uppercase.
// Internal fields mirror the Python Connection dataclass in tncd.py.
type Conn struct {
	Port          int
	Local, Remote string   // uppercase, e.g. "KU0HN-10", "N0CALL-2"
	Via           []string // digipeater path (return direction for outgoing)
	State         ConnState
	Owner         any // opaque; set/read by the frontend

	// Sequence numbers (mod 8)
	sendSeq   uint8
	recvSeq   uint8
	unacked   uint8
	lastAcked uint8

	// Outbound queues (Tasks 9-10)
	outQueue      []outEntry
	retransmitBuf map[uint8][]byte

	// Timers
	t1 *engine.Timer
	t2 *engine.Timer
	t3 *engine.Timer

	// T1 state
	t1Polls int
	t1Value time.Duration // current T1 (Karn adaptive arrives in Task 9)

	// RNR/flow control
	remoteBusy bool

	// T2 delayed ACK state (Task 10)
	t2Src string
	t2Dst string

	// Duplicate RR suppression (Task 9/10)
	lastRRTime time.Time
	lastRRNR   uint8

	// Karn RTT estimation (Task 9)
	srtt   time.Duration
	rttvar time.Duration

	// Per-I-frame timestamps for RTT (Task 9)
	iframeTimestamps map[uint8]time.Time
}

type outEntry struct {
	pid  uint8
	data []byte
}

func newConn(port int, local, remote string) *Conn {
	return &Conn{
		Port:             port,
		Local:            local,
		Remote:           remote,
		State:            Disconnected,
		retransmitBuf:    make(map[uint8][]byte),
		iframeTimestamps: make(map[uint8]time.Time),
	}
}

// cancelTimer cancels a timer if non-nil and returns nil.
func cancelTimer(tm *engine.Timer) *engine.Timer {
	if tm != nil {
		tm.Cancel()
	}
	return nil
}

// resetSeqs zeroes out all sequence counters and clears buffers.
func (c *Conn) resetSeqs() {
	c.sendSeq = 0
	c.recvSeq = 0
	c.unacked = 0
	c.lastAcked = 0
	c.retransmitBuf = make(map[uint8][]byte)
	c.iframeTimestamps = make(map[uint8]time.Time)
	c.outQueue = c.outQueue[:0]
	c.remoteBusy = false
}
