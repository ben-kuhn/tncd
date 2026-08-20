package l2

import (
	"fmt"
	"strings"
	"time"

	"github.com/ben-kuhn/tncd/v2/ax25"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
)

const maxConnections = 64

// ax25Overhead is the overhead per frame for T1 calculation.
// tncd.py:1073: max_frame_bytes = 256 + AX25_OVERHEAD
// AX25_OVERHEAD = 20 (from tncd.py constants)
const ax25Overhead = 20

// PortParams holds the T1/T2/T3 timing and window parameters for one port.
// Derived from ota_baudrate via DeriveParams (tncd.py:1068-1087).
type PortParams struct {
	MaxWindow   int
	N2Retry     int
	T1          time.Duration // max(3s, 2*(window*frameTime + 1s))
	T2          time.Duration // max(100ms, 1.2*frameTime)
	T3          time.Duration // 0 disables
	AX25Version int           // 20 (mod-8 only) or 22 (attempt SABME/mod-128); default 20
	SREJ        bool          // v2.2 selective reject enabled for this port (default set by bridge)
}

// DeriveParams computes PortParams from the on-air baud rate and config.
// Mirrors tncd.py:1068-1087 exactly.
func DeriveParams(otaBaud, maxWindow, n2Retry, t3Seconds int) PortParams {
	if otaBaud <= 0 {
		otaBaud = 1200
	}
	maxFrameBytes := 256 + ax25Overhead
	frameTime := float64(maxFrameBytes*8) / float64(otaBaud) // seconds
	turnaround := 1.0
	t1Sec := 3.0
	if v := 2.0 * (float64(maxWindow)*frameTime + turnaround); v > t1Sec {
		t1Sec = v
	}
	t2Sec := 0.1
	const t2Multiplier = 1.2
	if v := t2Multiplier * frameTime; v > t2Sec {
		t2Sec = v
	}
	return PortParams{
		MaxWindow: maxWindow,
		N2Retry:   n2Retry,
		T1:        time.Duration(t1Sec * float64(time.Second)),
		T2:        time.Duration(t2Sec * float64(time.Second)),
		T3:        time.Duration(t3Seconds) * time.Second,
	}
}

// FailReason distinguishes why a connection attempt failed.
// Used by the bridge to choose between "BUSY" and "failed" AGWPE messages.
type FailReason int

const (
	// FailTimeout means SABM was sent N2 times with no UA response.
	// Bridge sends: *** BUSY From {remote}\r  (tncd.py:1519)
	FailTimeout FailReason = iota
	// FailDM means a DM frame was received while in Connecting state.
	// Bridge sends: *** CONNECTED With {remote} failed\r  (tncd.py:1953)
	FailDM
)

// Hooks are the callbacks l2 fires when state changes. All hooks are called
// synchronously on the engine loop (or the caller's goroutine in tests).
type Hooks struct {
	SendAX25      func(port int, f *ax25.Frame)         // → KISS TX
	Connected     func(c *Conn, incoming bool)          // → AGWPE 'C'
	ConnectFailed func(c *Conn, reason FailReason)      // SABM gave up → 'd'
	Data          func(c *Conn, pid uint8, data []byte) // → AGWPE 'D'
	Disconnected  func(c *Conn)                         // → AGWPE 'd'
	// Defer posts fn so that any frames already queued on the event loop are
	// processed before fn runs — mirrors asyncio loop.call_soon.
	// Bridge wires this to engine.Do. When nil, fn is called synchronously
	// (useful in tests that want predictable ordering without a real loop).
	Defer func(fn func())
	// IsLocal reports whether call is one of our registered callsigns on port.
	// Bridge wires this to the AGWPE clients' RegisteredCalls sets.
	// When nil, the permissive default (always true) is used so that all
	// existing tests pass unchanged without setting this hook.
	IsLocal func(port int, call string) bool
}

// connKey is the map key for the connection table.
type connKey struct {
	port          int
	local, remote string // always uppercase
}

func makeKey(port int, local, remote string) connKey {
	return connKey{
		port:   port,
		local:  strings.ToUpper(strings.TrimSpace(local)),
		remote: strings.ToUpper(strings.TrimSpace(remote)),
	}
}

// Table is the AX.25 connection table. Methods must be called on the engine
// loop (they are not concurrency-safe themselves).
type Table struct {
	clock     engine.Clock
	hooks     Hooks
	params    []PortParams
	conns     map[connKey]*Conn
	portCount int
}

// NewTable creates a connection table. params[i] is used for port i.
func NewTable(clock engine.Clock, hooks Hooks, params []PortParams) *Table {
	return &Table{
		clock:     clock,
		hooks:     hooks,
		params:    params,
		conns:     make(map[connKey]*Conn),
		portCount: len(params),
	}
}

// Hooks returns a pointer to the table's Hooks struct so tests can mutate
// individual fields (e.g. set Defer) after construction.
func (t *Table) Hooks() *Hooks {
	return &t.hooks
}

// portParams returns the PortParams for the given port (clamped to last if out of range).
func (t *Table) portParams(port int) PortParams {
	if port < 0 || len(t.params) == 0 {
		return PortParams{N2Retry: 10, T1: 3 * time.Second}
	}
	if port >= len(t.params) {
		return t.params[len(t.params)-1]
	}
	return t.params[port]
}

// Get returns the Conn for (port, local, remote), or nil if none exists.
func (t *Table) Get(port int, local, remote string) *Conn {
	return t.conns[makeKey(port, local, remote)]
}

// getOrCreate returns an existing Conn or creates one. Returns nil if the
// connection limit is reached (tncd.py:1228-1237).
func (t *Table) getOrCreate(port int, local, remote string) *Conn {
	k := makeKey(port, local, remote)
	if c, ok := t.conns[k]; ok {
		return c
	}
	if len(t.conns) >= maxConnections {
		return nil
	}
	c := newConn(k.port, k.local, k.remote)
	c.t1Value = t.portParams(port).T1
	t.conns[k] = c
	return c
}

// remove deletes a connection from the table and cancels all its timers.
// Mirrors tncd.py:_cancel_t1/_cancel_t2/_cancel_t3 called from remove_connection.
func (t *Table) remove(port int, local, remote string) {
	k := makeKey(port, local, remote)
	c, ok := t.conns[k]
	if !ok {
		return
	}
	c.t1 = cancelTimer(c.t1)
	c.t2 = cancelTimer(c.t2)
	c.t3 = cancelTimer(c.t3)
	delete(t.conns, k)
}

// removeConn removes by conn pointer (for cases where we already have the conn).
func (t *Table) removeConn(c *Conn) {
	t.remove(c.Port, c.Local, c.Remote)
}

// startT1 starts (or restarts) the T1 retransmit/poll timer for conn.
// Uses c.t1Value for the duration (Karn adaptive will update this in Task 9).
// The closure captures the returned *Timer in self so that a stale expiry
// (fired after a subsequent startT1 replaced c.t1) is a no-op (I4 guard).
func (t *Table) startT1(c *Conn) {
	c.t1 = cancelTimer(c.t1)
	d := c.t1Value
	if d <= 0 {
		d = t.portParams(c.Port).T1
	}
	conn := c
	var self *engine.Timer
	self = t.clock.After(d, func() {
		if c.t1 != self {
			return // stale — a newer startT1 replaced us
		}
		t.t1Expired(conn)
	})
	c.t1 = self
}

// cancelT1 stops the T1 timer if running.
func cancelT1(c *Conn) {
	c.t1 = cancelTimer(c.t1)
}

// scheduleT2 schedules a delayed RR (T2 timer), resetting any existing timer.
// A burst of in-sequence I-frames results in a single RR after the last frame.
// Mirrors tncd.py:1423-1436 (_schedule_t2).
// The closure captures self so that a stale expiry is a no-op (I4 guard).
func (t *Table) scheduleT2(c *Conn, src, dst string) {
	c.t2 = cancelTimer(c.t2)
	c.t2Src = src
	c.t2Dst = dst
	pp := t.portParams(c.Port)
	conn := c
	var self *engine.Timer
	self = t.clock.After(pp.T2, func() {
		if c.t2 != self {
			return // stale
		}
		t.t2Expired(conn)
	})
	c.t2 = self
}

// cancelT2 cancels the T2 delayed ACK timer.
func cancelT2(c *Conn) {
	c.t2 = cancelTimer(c.t2)
}

// t2Expired fires the delayed RR: send RR F=0 with current recvSeq.
// Mirrors tncd.py:1445-1498 (_t2_expired, _send_delayed_rr).
func (t *Table) t2Expired(c *Conn) {
	c.t2 = nil
	if c.State != Connected {
		return
	}
	// Send RR response F=0 with current V(R).
	rr := respFrame(c.t2Src, c.t2Dst, c.Via, ax25.RR, false)
	rr.NR = c.recvSeq
	t.sendFrame(c.Port, rr)
}

// startT3 starts (or restarts) the T3 inactive link timer.
// Mirrors tncd.py:1453-1463 (_start_t3).
// The closure captures self so that a stale expiry is a no-op (I4 guard).
func (t *Table) startT3(c *Conn) {
	c.t3 = cancelTimer(c.t3)
	pp := t.portParams(c.Port)
	if pp.T3 <= 0 {
		return
	}
	conn := c
	var self *engine.Timer
	self = t.clock.After(pp.T3, func() {
		if c.t3 != self {
			return // stale
		}
		t.t3Expired(conn)
	})
	c.t3 = self
}

// t3Expired fires the T3 liveness poll: sends RR P=1 command and starts T1.
// Mirrors tncd.py:1471-1487 (_t3_expired).
func (t *Table) t3Expired(c *Conn) {
	c.t3 = nil
	if c.State != Connected {
		return
	}
	// Send RR P=1 command (liveness poll).
	rr := cmdFrame(c.Remote, c.Local, c.Via, ax25.RR, true)
	rr.NR = c.recvSeq
	t.sendFrame(c.Port, rr)
	t.startT1(c)
}

// sendRRGuarded sends RR F=1 (response, PF=true) with conn.recvSeq, but
// suppresses duplicates sent within 3.0s with the same N(R).
// Mirrors tncd.py:2034-2055 (_send_rr_guarded).
func (t *Table) sendRRGuarded(c *Conn, src, dst string) {
	now := t.clock.Now()
	nr := c.recvSeq
	if nr == c.lastRRNR && !c.lastRRTime.IsZero() && now.Sub(c.lastRRTime) < 3*time.Second {
		return // suppress duplicate
	}
	c.lastRRTime = now
	c.lastRRNR = nr
	rr := respFrame(src, dst, c.Via, ax25.RR, true)
	rr.NR = nr
	t.sendFrame(c.Port, rr)
}

// defer_ posts fn via Hooks.Defer (mirrors asyncio loop.call_soon). If
// Hooks.Defer is nil, fn is called synchronously (test default).
func (t *Table) defer_(fn func()) {
	if t.hooks.Defer != nil {
		t.hooks.Defer(fn)
	} else {
		fn()
	}
}

// t1Expired handles T1 timer expiry (tncd.py:1500-1539 for CONNECTING case,
// tncd.py:1541-1579 for Connected state).
func (t *Table) t1Expired(c *Conn) {
	c.t1 = nil
	pp := t.portParams(c.Port)

	if c.State == Connecting {
		// SABM/SABME retransmission while waiting for UA (AX.25 6.3.1)
		// tncd.py:1512-1539
		c.t1Polls++
		maxV22 := pp.N2Retry / 3
		if c.modulo == 128 && !c.triedFallback && c.t1Polls >= maxV22 {
			// Direwolf MAXV22 hack: after maxV22 SABMEs, continue as SABM
			// without resetting the retry counter.
			c.modulo = 8
			c.triedFallback = true
		}
		if c.t1Polls > pp.N2Retry {
			// N2 exhausted — give up
			c.State = Disconnected
			if t.hooks.ConnectFailed != nil {
				t.hooks.ConnectFailed(c, FailTimeout)
			}
			t.removeConn(c)
			return
		}
		// Connection-setup retransmits use a FIXED T1. Karn's exponential
		// backoff is for the data (information-transfer) phase only — the AX.25
		// spec and Dire Wolf's select_t1_value keep t1v ~constant while awaiting
		// a UA. Doubling here makes SABM/SABME retries too sparse (13→26→52s),
		// so a timing-marginal radio's narrow acceptance window is missed and
		// the peer never sees a retry in time. Reset to the base so every retry
		// is evenly spaced.
		c.t1Value = pp.T1
		if c.modulo == 128 {
			t.sendSABME(c)
		} else {
			t.sendSABM(c)
		}
		t.startT1(c)
		return
	}

	// Connected state: data-phase T1 (tncd.py:1541-1579).
	if c.State != Connected || len(c.retransmitBuf) == 0 {
		return
	}
	c.t1Polls++

	// N2 retry limit: disconnect after too many unanswered polls.
	if c.t1Polls > pp.N2Retry {
		c.State = Disconnected
		if t.hooks.Disconnected != nil {
			t.hooks.Disconnected(c)
		}
		t.removeConn(c)
		return
	}

	// Poll #1: send RR P=1 only; poll #2+: also retransmit.
	rr := &ax25.Frame{
		Dst:     mustParseAddr(c.Remote),
		Src:     mustParseAddr(c.Local),
		Type:    ax25.RR,
		NR:      c.recvSeq,
		PF:      true,
		Command: true,
	}
	t.sendFrame(c.Port, rr)

	if c.t1Polls > 1 {
		t.retransmitFrom(c, c.lastAcked)
	}

	// Karn: exponential backoff on timeout (tncd.py:1575-1578).
	if c.t1Value > 0 {
		c.t1Value *= 2
		const t1Ceil = 60 * time.Second
		if c.t1Value > t1Ceil {
			c.t1Value = t1Ceil
		}
	}
	t.startT1(c)
}

// isLocal reports whether call is one of our registered callsigns on port.
// When Hooks.IsLocal is nil, returns true (permissive default: keeps all
// existing l2 tests passing unchanged without setting the hook).
func (t *Table) isLocal(port int, call string) bool {
	if t.hooks.IsLocal == nil {
		return true
	}
	return t.hooks.IsLocal(port, call)
}

// sendFrame stamps the modulo on outgoing I/S frames (if not already set)
// then calls hooks.SendAX25.
func (t *Table) sendFrame(port int, f *ax25.Frame) {
	if f.Modulo == 0 && (f.Type == ax25.I || f.Type.IsS()) {
		f.Modulo = uint8(t.ModuloFor(port, f.Src.String(), f.Dst.String()))
	}
	if t.hooks.SendAX25 != nil {
		t.hooks.SendAX25(port, f)
	}
}

// ModuloFor returns the negotiated modulo (8 or 128) for the connection
// addressed by (port, local, remote), or 8 if there is no such connection.
// The bridge uses this to decode inbound I/S control fields at the right width.
func (t *Table) ModuloFor(port int, local, remote string) int {
	if c := t.Get(port, local, remote); c != nil {
		return int(c.modulo)
	}
	return 8
}

// cmdFrame builds a command frame (dst C-bit=1, src C-bit=0).
// Mirrors tncd.py _cmd_frame().
func cmdFrame(dst, src string, via []string, typ ax25.FrameType, pf bool) *ax25.Frame {
	d, _ := ax25.ParseAddress(dst)
	s, _ := ax25.ParseAddress(src)
	f := &ax25.Frame{
		Dst:     d,
		Src:     s,
		Type:    typ,
		PF:      pf,
		Command: true, // command frame
	}
	for _, v := range via {
		a, _ := ax25.ParseAddress(v)
		f.Via = append(f.Via, a)
	}
	return f
}

// respFrame builds a response frame (dst C-bit=0, src C-bit=1).
// Mirrors tncd.py _resp_frame().
func respFrame(dst, src string, via []string, typ ax25.FrameType, pf bool) *ax25.Frame {
	d, _ := ax25.ParseAddress(dst)
	s, _ := ax25.ParseAddress(src)
	f := &ax25.Frame{
		Dst:     d,
		Src:     s,
		Type:    typ,
		PF:      pf,
		Command: false, // response frame
	}
	for _, v := range via {
		a, _ := ax25.ParseAddress(v)
		f.Via = append(f.Via, a)
	}
	return f
}

// sendSABM sends a SABM P=1 (mod-8 connect) command frame to the remote.
func (t *Table) sendSABM(c *Conn) {
	f := cmdFrame(c.Remote, c.Local, c.Via, ax25.SABM, true)
	t.sendFrame(c.Port, f)
}

// sendSABME sends a SABME P=1 command frame to the remote (AX.25 v2.2 mod-128).
func (t *Table) sendSABME(c *Conn) {
	f := cmdFrame(c.Remote, c.Local, c.Via, ax25.SABME, true)
	t.sendFrame(c.Port, f)
}

// fallbackToSABM downgrades a still-connecting mod-128 attempt to mod-8 and
// resends SABM. Mirrors Direwolf set_version_2_0 + resend. Returns true if a
// downgrade happened.
func (t *Table) fallbackToSABM(c *Conn) bool {
	if c.modulo != 128 || c.triedFallback {
		return false
	}
	c.modulo = 8
	c.triedFallback = true
	t.sendSABM(c)
	t.startT1(c)
	return true
}

// Connect initiates an outgoing AX.25 connection (tncd.py:274-304).
// Sends SABME P=1 on AX.25 v2.2 ports, SABM P=1 on v2.0 ports.
// Sets state=Connecting, starts T1. Returns an error if the connection table is full.
// getOrCreate may return a reused Conn from a prior session; all relevant state
// (including triedFallback) is reset so that a fresh v2.2 attempt starts clean.
func (t *Table) Connect(port int, local, remote string, via []string) (*Conn, error) {
	c := t.getOrCreate(port, local, remote)
	if c == nil {
		return nil, fmt.Errorf("l2: connection limit reached (%d)", maxConnections)
	}
	c.State = Connecting
	c.sendSeq = 0
	c.recvSeq = 0
	c.t1Polls = 0
	c.triedFallback = false
	c.Via = via
	c.t1Value = t.portParams(port).T1

	if t.portParams(port).AX25Version >= 22 {
		c.modulo = 128
		t.sendSABME(c)
	} else {
		c.modulo = 8
		t.sendSABM(c)
	}
	t.startT1(c)
	return c, nil
}

// Disconnect initiates a graceful disconnect (tncd.py: DISC P=1, Disconnecting).
func (t *Table) Disconnect(c *Conn) {
	cancelT1(c)
	c.State = Disconnecting
	f := cmdFrame(c.Remote, c.Local, c.Via, ax25.DISC, true)
	t.sendFrame(c.Port, f)
	t.startT1(c)
}

// maxOutboundQueue is the maximum number of frames in the outbound queue
// before drops occur (tncd.py MAX_OUTBOUND_QUEUE = 512).
const maxOutboundQueue = 512

// maxIFrameInfo is the maximum info field size for an I-frame (tncd.py: 256 bytes).
const maxIFrameInfo = 256

// SendData fragments payload into ≤256-byte chunks, queues (chunk, pid),
// drops when queue ≥ 512 with warning, then calls drainOutbound.
// Mirrors tncd.py:366-389 ('D' handler).
func (t *Table) SendData(c *Conn, pid uint8, data []byte) {
	if len(data) == 0 {
		data = []byte{}
	}
	// Fragment into 256-byte chunks.
	chunks := make([][]byte, 0, (len(data)+maxIFrameInfo-1)/maxIFrameInfo)
	if len(data) == 0 {
		chunks = append(chunks, []byte{})
	} else {
		for i := 0; i < len(data); i += maxIFrameInfo {
			end := i + maxIFrameInfo
			if end > len(data) {
				end = len(data)
			}
			chunks = append(chunks, data[i:end])
		}
	}

	for _, chunk := range chunks {
		if len(c.outQueue) >= maxOutboundQueue {
			// Queue full — drop remaining chunks.
			break
		}
		c.outQueue = append(c.outQueue, outEntry{pid: pid, data: chunk})
	}
	t.drainOutbound(c)
}

// Outstanding returns unacked + len(outQueue), the Y-frame fix.
// Mirrors tncd.py:432-449.
func (t *Table) Outstanding(c *Conn) int {
	return int(c.unacked) + len(c.outQueue)
}

// drainOutbound sends queued I-frames while the outbound window has space.
// Coalesces adjacent same-PID chunks up to 256 bytes.
// Mirrors tncd.py:1339-1385.
func (t *Table) drainOutbound(c *Conn) {
	if c.remoteBusy {
		return
	}
	pp := t.portParams(c.Port)
	sentAny := false
	for len(c.outQueue) > 0 && int(c.unacked) < pp.MaxWindow {
		// Pop first entry.
		entry := c.outQueue[0]
		c.outQueue = c.outQueue[1:]
		chunk := entry.data
		pid := entry.pid

		// Coalesce adjacent entries with the same PID up to 256 bytes.
		for len(c.outQueue) > 0 && len(chunk) < maxIFrameInfo {
			next := c.outQueue[0]
			if next.pid != pid || len(chunk)+len(next.data) > maxIFrameInfo {
				break
			}
			c.outQueue = c.outQueue[1:]
			chunk = append(chunk, next.data...)
		}

		// Build the I-frame.
		ns := c.sendSeq
		f := &ax25.Frame{
			Dst:     mustParseAddr(c.Remote),
			Src:     mustParseAddr(c.Local),
			Type:    ax25.I,
			NS:      ns,
			NR:      c.recvSeq,
			PF:      false,
			Command: true,
			PID:     pid,
			Info:    chunk,
			Modulo:  c.modulo,
		}
		for _, v := range c.Via {
			a, _ := ax25.ParseAddress(v)
			f.Via = append(f.Via, a)
		}

		// Store raw bytes in retransmitBuf, record first-TX timestamp.
		raw := f.Bytes()
		c.retransmitBuf[ns] = raw
		c.iframeTimestamps[ns] = t.clock.Now()
		c.sendSeq = (ns + 1) % c.modulo
		c.unacked++
		sentAny = true

		t.sendFrame(c.Port, f)
	}
	if sentAny {
		// Outgoing I-frames piggyback N(R): cancel T2 delayed ACK, start T1.
		c.t2 = cancelTimer(c.t2)
		t.startT1(c)
	}
}

// ackFrames processes a cumulative ACK: purge retransmit buffer, update SRTT,
// restart or cancel T1, then drain outbound queue.
// Mirrors tncd.py:1581-1616.
func (t *Table) ackFrames(c *Conn, nr uint8) {
	pp := t.portParams(c.Port)
	newlyAcked := (nr - c.lastAcked) % c.modulo
	// Guard: reject backwards N(R) (would appear as a large forward ACK).
	if int(newlyAcked) > pp.MaxWindow {
		return
	}
	if newlyAcked == 0 {
		return
	}

	c.t1Polls = 0 // remote responded — reset consecutive poll counter

	// Karn RTT sampling from first-TX timestamps (skip retransmitted frames).
	now := t.clock.Now()
	seq := c.lastAcked
	for i := uint8(0); i < newlyAcked; i++ {
		if sendTime, ok := c.iframeTimestamps[seq]; ok {
			delete(c.iframeTimestamps, seq)
			rtt := now.Sub(sendTime)
			t.updateSRTT(c, rtt)
		}
		seq = (seq + 1) % c.modulo
	}

	// Purge retransmit buffer for ACKed sequence numbers.
	seq = c.lastAcked
	for i := uint8(0); i < newlyAcked; i++ {
		delete(c.retransmitBuf, seq)
		seq = (seq + 1) % c.modulo
	}

	c.lastAcked = nr
	if int(c.unacked) > int(newlyAcked) {
		c.unacked -= newlyAcked
	} else {
		c.unacked = 0
	}

	if len(c.retransmitBuf) > 0 {
		t.startT1(c)
	} else {
		c.t1 = cancelTimer(c.t1)
	}
	t.drainOutbound(c)
}

// updateSRTT updates smoothed RTT and variance per Karn's algorithm (RFC 2988).
// Mirrors tncd.py:1404-1421.
func (t *Table) updateSRTT(c *Conn, rtt time.Duration) {
	const t1Floor = 3 * time.Second
	const t1Ceil = 60 * time.Second
	if c.srtt == 0 {
		// First measurement.
		c.srtt = rtt
		c.rttvar = rtt / 2
	} else {
		// α = 7/8, β = 3/4 (RFC 2988 / Jacobson/Karels).
		diff := c.srtt - rtt
		if diff < 0 {
			diff = -diff
		}
		c.rttvar = (3*c.rttvar + diff) / 4
		c.srtt = (7*c.srtt + rtt) / 8
	}
	t1 := c.srtt + 4*c.rttvar
	if t1 < t1Floor {
		t1 = t1Floor
	}
	if t1 > t1Ceil {
		t1 = t1Ceil
	}
	c.t1Value = t1
}

// retransmitFrom retransmits all buffered I-frames from fromSeq onward,
// rebuilding each with the current N(R) to piggyback newer ACKs, and
// dropping RTT timestamps (Karn's algorithm).
// Mirrors tncd.py:1618-1640.
func (t *Table) retransmitFrom(c *Conn, fromSeq uint8) {
	seq := fromSeq
	for {
		raw, ok := c.retransmitBuf[seq]
		if !ok {
			break
		}
		// Parse the stored frame to extract NS/PID/Info.
		orig, err := ax25.ParseModulo(raw, int(c.modulo))
		if err != nil {
			break
		}
		// Rebuild with current N(R) (piggyback-acknowledge received frames).
		f := &ax25.Frame{
			Dst:     mustParseAddr(c.Remote),
			Src:     mustParseAddr(c.Local),
			Type:    ax25.I,
			NS:      orig.NS,
			NR:      c.recvSeq,
			PF:      orig.PF,
			Command: true,
			PID:     orig.PID,
			Info:    orig.Info,
			Modulo:  c.modulo,
		}
		for _, v := range c.Via {
			a, _ := ax25.ParseAddress(v)
			f.Via = append(f.Via, a)
		}
		// Update retransmitBuf with rebuilt frame.
		c.retransmitBuf[seq] = f.Bytes()
		// Karn: discard RTT sample for retransmitted frames.
		delete(c.iframeTimestamps, seq)
		t.sendFrame(c.Port, f)
		seq = (seq + 1) % c.modulo
	}
}

// mustParseAddr parses an AX.25 address, returning the zero Address on failure.
// Used only for known-good strings where parse errors cannot occur in practice.
func mustParseAddr(s string) ax25.Address {
	a, _ := ax25.ParseAddress(s)
	return a
}

// OnFrame dispatches an inbound AX.25 frame to the appropriate handler.
// The frame's Dst is our local call, Src is the remote.
// (tncd.py:1684-1700 dispatch table)
func (t *Table) OnFrame(port int, f *ax25.Frame) {
	// src = who sent it (remote), dst = who it's addressed to (our local call)
	src := f.Src.String()
	dst := f.Dst.String()

	switch f.Type {
	case ax25.SABM:
		t.dispatchSABM(port, f, src, dst)
	case ax25.SABME:
		t.dispatchSABME(port, f, src, dst)
	case ax25.UA:
		t.dispatchUA(port, f, src, dst)
	case ax25.DM:
		t.dispatchDM(port, f, src, dst)
	case ax25.DISC:
		t.dispatchDISC(port, f, src, dst)
	case ax25.FRMR:
		t.dispatchFRMR(port, f, src, dst)
	case ax25.I:
		t.dispatchI(port, f, src, dst)
	case ax25.RR, ax25.RNR, ax25.REJ, ax25.SREJ:
		t.dispatchS(port, f, src, dst)
	case ax25.XID:
		t.dispatchXID(port, f, src, dst)
	}

	// Reset T3 inactive link timer on any received frame for a Connected conn.
	// Mirrors tncd.py:1707-1711 (on_kiss_frame level, after dispatch).
	if c := t.Get(port, dst, src); c != nil && c.State == Connected {
		t.startT3(c)
	}
}

// RemoveOwned sends DISC for all connections owned by the given owner, then
// removes them from the table. Mirrors tncd.py:1202-1220 (remove_client).
func (t *Table) RemoveOwned(owner any) {
	var toRemove []*Conn
	for _, c := range t.conns {
		if c.Owner == owner {
			toRemove = append(toRemove, c)
		}
	}
	for _, c := range toRemove {
		c.t1 = cancelTimer(c.t1)
		c.t2 = cancelTimer(c.t2)
		c.t3 = cancelTimer(c.t3)
		if c.State == Connected || c.State == Connecting {
			f := cmdFrame(c.Remote, c.Local, c.Via, ax25.DISC, true)
			t.sendFrame(c.Port, f)
		}
		t.removeConn(c)
	}
}

// PortOffline handles a port going offline: cancels all timers, fires
// Disconnected for Connected/Connecting conns on that port, and removes them.
// Mirrors tncd.py:1246-1264 (_port_went_offline).
func (t *Table) PortOffline(port int) {
	var toRemove []*Conn
	for _, c := range t.conns {
		if c.Port == port && (c.State == Connected || c.State == Connecting) {
			toRemove = append(toRemove, c)
		}
	}
	for _, c := range toRemove {
		c.t1 = cancelTimer(c.t1)
		c.t2 = cancelTimer(c.t2)
		c.t3 = cancelTimer(c.t3)
		if t.hooks.Disconnected != nil {
			t.hooks.Disconnected(c)
		}
		t.removeConn(c)
	}
}

// --- Frame dispatch handlers ---

// dispatchSABM handles an incoming SABM (tncd.py:1844-1912).
func (t *Table) dispatchSABM(port int, f *ax25.Frame, src, dst string) {
	// Overheard-frame suppression: if this pair has a connection on a
	// different port, silently drop (tncd.py:1850-1856).
	for otherPort, c := range t.connsByPair(dst, src) {
		if otherPort != port && (c.State == Connecting || c.State == Connected) {
			return
		}
	}

	// Foreign-SABM suppression: if dst is not one of our registered callsigns,
	// silently drop — no UA, no DM, no connection created. On a shared channel,
	// connect requests addressed to other stations must not be answered.
	// Deliberate divergence from tncd.py:1844-1912 (shared-channel courtesy).
	if !t.isLocal(port, dst) {
		return
	}

	// Capture digipeater path (reversed for return direction, tncd.py:1858-1859).
	incomingVia := addrSliceToStrings(f.Via)
	returnVia := reversed(incomingVia)

	// Get or create the connection (local=dst, remote=src).
	c := t.getOrCreate(port, dst, src)
	if c == nil {
		// Connection limit — reject with DM (tncd.py:1865-1873).
		dm := respFrame(src, dst, returnVia, ax25.DM, f.PF)
		t.sendFrame(port, dm)
		return
	}

	// Send UA response echoing P flag (tncd.py:1875-1883).
	ua := respFrame(src, dst, returnVia, ax25.UA, f.PF)
	t.sendFrame(port, ua)

	// Full state reset (tncd.py:1884-1896).
	c.Via = returnVia
	c.State = Connected
	c.resetSeqs()
	c.t1 = cancelTimer(c.t1)
	c.t2 = cancelTimer(c.t2)
	c.t3 = cancelTimer(c.t3)

	// Notify hook (tncd.py:1907-1912).
	if t.hooks.Connected != nil {
		c.incoming = true
		t.hooks.Connected(c, true)
	}
}

// dispatchSABME handles SABME (tncd.py:1686-1695 for the DM-reject path;
// extended here for AX.25 v2.2 accept).
// On a 2.0-only port: reject with DM P=1 so the peer (e.g. Direwolf) retries SABM.
// On a 2.2 port: accept with UA, set modulo=128, fire the Connected hook.
// Like SABM/I/DISC, the response is gated on the destination being one of
// our registered callsigns; answering foreign SABMEs would transmit DM into
// other stations' sessions on a shared channel.
func (t *Table) dispatchSABME(port int, f *ax25.Frame, src, dst string) {
	// Overheard-frame suppression: if this pair has a connection on a
	// different port, silently drop (mirrors dispatchSABM logic).
	for otherPort, c := range t.connsByPair(dst, src) {
		if otherPort != port && (c.State == Connecting || c.State == Connected) {
			return
		}
	}

	if !t.isLocal(port, dst) {
		return
	}
	if t.portParams(port).AX25Version < 22 {
		// 2.0-only port: reject; the peer (e.g. Direwolf) then retries SABM.
		dm := respFrame(src, dst, nil, ax25.DM, true)
		t.sendFrame(port, dm)
		return
	}
	// Accept mod-128. Mirror dispatchSABM but set modulo = 128.
	incomingVia := addrSliceToStrings(f.Via)
	returnVia := reversed(incomingVia)
	c := t.getOrCreate(port, dst, src)
	if c == nil {
		dm := respFrame(src, dst, returnVia, ax25.DM, f.PF)
		t.sendFrame(port, dm)
		return
	}
	ua := respFrame(src, dst, returnVia, ax25.UA, f.PF)
	t.sendFrame(port, ua)
	c.Via = returnVia
	c.State = Connected
	c.modulo = 128
	c.resetSeqs()
	c.t1 = cancelTimer(c.t1)
	c.t2 = cancelTimer(c.t2)
	c.t3 = cancelTimer(c.t3)
	if t.hooks.Connected != nil {
		c.incoming = true
		t.hooks.Connected(c, true)
	}
}

// dispatchUA handles incoming UA (tncd.py:1914-1944).
func (t *Table) dispatchUA(port int, f *ax25.Frame, src, dst string) {
	// src=remote sent UA, dst=local received it
	c := t.Get(port, dst, src)
	if c == nil {
		return
	}

	switch c.State {
	case Connecting:
		// Outgoing connect confirmed (tncd.py:1922-1934).
		c.t1 = cancelTimer(c.t1)
		c.State = Connected
		c.sendSeq = 0
		c.recvSeq = 0
		c.t1Polls = 0
		if t.hooks.Connected != nil {
			c.incoming = false
			t.hooks.Connected(c, false)
		}
		// Initiator XID: the answering peer does not send XID, so we must, to
		// negotiate SREJ for the (download) direction where tncd is the receiver.
		if t.portParams(port).SREJ && c.modulo == 128 {
			t.sendXIDCommand(c)
		}

	case Disconnecting:
		// Disconnect confirmed (tncd.py:1936-1944).
		if t.hooks.Disconnected != nil {
			t.hooks.Disconnected(c)
		}
		t.removeConn(c)
	}
}

// dispatchDM handles incoming DM (tncd.py:1946-1961).
func (t *Table) dispatchDM(port int, f *ax25.Frame, src, dst string) {
	c := t.Get(port, dst, src)
	if c == nil {
		return
	}
	if c.State == Connecting && c.modulo == 128 && !c.triedFallback {
		t.fallbackToSABM(c)
		return
	}
	// A DM while awaiting our XID response = peer rejects XID; keep the
	// connection (do not tear it down over an XID the peer didn't understand).
	if c.State == Connected && c.xidPending {
		c.xidPending = false
		c.srejEnabled = false
		return
	}
	if c.State == Connecting {
		if t.hooks.ConnectFailed != nil {
			t.hooks.ConnectFailed(c, FailDM)
		}
	} else {
		if t.hooks.Disconnected != nil {
			t.hooks.Disconnected(c)
		}
	}
	t.removeConn(c)
}

// dispatchDISC handles remote-initiated DISC (tncd.py:1963-2004).
func (t *Table) dispatchDISC(port int, f *ax25.Frame, src, dst string) {
	c := t.Get(port, dst, src)

	if c == nil {
		// No connection on this port — check other ports for overheard suppression
		// (tncd.py:1968-1974).
		for otherPort, oc := range t.connsByPair(dst, src) {
			if otherPort != port &&
				(oc.State == Connecting || oc.State == Connected || oc.State == Disconnecting) {
				return
			}
		}
		// No connection anywhere — respond with DM, but only when dst is one
		// of our own callsigns. Foreign DISC on a shared channel must not
		// trigger a DM from us.
		// Deliberate divergence from tncd.py:1987-1994 (shared-channel courtesy).
		if t.isLocal(port, dst) {
			dm := respFrame(src, dst, nil, ax25.DM, f.PF)
			t.sendFrame(port, dm)
		}
		return
	}

	// Connection exists — respond with UA (tncd.py:1979-1985).
	ua := respFrame(src, dst, c.Via, ax25.UA, f.PF)
	t.sendFrame(port, ua)

	if t.hooks.Disconnected != nil {
		t.hooks.Disconnected(c)
	}
	t.removeConn(c)
}

// dispatchFRMR handles incoming FRMR (tncd.py:2006-2032).
// Resets the connection and sends a fresh SABM.
func (t *Table) dispatchFRMR(port int, f *ax25.Frame, src, dst string) {
	c := t.Get(port, dst, src)
	if c != nil && c.State == Connecting && c.modulo == 128 && !c.triedFallback {
		t.fallbackToSABM(c)
		return
	}
	// A FRMR while we are awaiting our XID response means the peer does not
	// understand XID (partial v2.2). Disable SREJ and keep the connection on
	// REJ — do NOT reset+re-SABM (that would re-send XID and loop). Mirrors
	// Direwolf's "FRMR of XID -> use v2.0 params, stay connected".
	if c != nil && c.State == Connected && c.xidPending {
		c.xidPending = false
		c.srejEnabled = false
		return
	}
	if c == nil || c.State != Connected {
		return
	}
	// Reset sequence numbers and buffers (tncd.py:2015-2025) but NOT remoteBusy.
	// Python's _dispatch_frmr resets: send/recv seqnos, unacked, lastAcked,
	// retransmitBuf, iframeTimestamps, outQueue — NOT remote_busy.
	// dispatchSABM also calls resetSeqs() which does reset remoteBusy, but
	// FRMR should preserve the remote flow-control state.
	c.State = Connecting
	c.sendSeq = 0
	c.recvSeq = 0
	c.unacked = 0
	c.lastAcked = 0
	c.retransmitBuf = make(map[uint8][]byte)
	c.iframeTimestamps = make(map[uint8]time.Time)
	c.outQueue = c.outQueue[:0]
	// Note: remoteBusy is NOT reset here
	c.t1 = cancelTimer(c.t1)
	c.t2 = cancelTimer(c.t2)
	c.t3 = cancelTimer(c.t3)

	// Send fresh SABM (tncd.py:2026-2031).
	// Note: in Python _dispatch_frmr, the frame arg is the FRMR frame, and
	// it sends to src (remote) with dst (local) as source.
	t.sendSABM(c)
	t.startT1(c)
}

// dispatchI handles a received I-frame.
// Mirrors tncd.py:1735-1832 (_dispatch_i).
func (t *Table) dispatchI(port int, f *ax25.Frame, src, dst string) {
	// Find connection: local=dst (frame addressed to us), remote=src.
	c := t.Get(port, dst, src)

	// I-frame while CONNECTING: UA was lost OTA but remote accepted our SABM.
	// Promote to CONNECTED so the I-frame is processed normally below.
	// Mirrors tncd.py:1749-1759.
	if c != nil && c.State == Connecting {
		c.State = Connected
		c.sendSeq = 0
		c.recvSeq = 0
		if t.hooks.Connected != nil {
			c.incoming = false
			t.hooks.Connected(c, false) // outgoing — we initiated
		}
	}

	if c == nil || c.State != Connected {
		// Check if this connection exists on a different port (overheard frame).
		// If so, silently drop. Mirrors tncd.py:1764-1770.
		for otherPort, oc := range t.connsByPair(dst, src) {
			if otherPort != port && (oc.State == Connecting || oc.State == Connected) {
				return
			}
		}
		// No active connection on any port — send DM if P=1 so remote knows,
		// but only when dst is one of our own callsigns. Foreign QSOs on a
		// shared channel must not trigger a DM from us.
		// Deliberate divergence from tncd.py:1771-1781 (shared-channel courtesy).
		if f.PF && t.isLocal(port, dst) {
			dm := respFrame(src, dst, nil, ax25.DM, true)
			t.sendFrame(port, dm)
		}
		return
	}

	// I-frames carry N(R) which implicitly ACKs our sent frames (always first).
	// Mirrors tncd.py:1783-1784.
	t.ackFrames(c, f.NR)

	pp := t.portParams(port)
	expectedNS := c.recvSeq

	if c.srejEnabled {
		gap := (f.NS - expectedNS) % c.modulo
		if gap > 0 {
			if int(gap) > pp.MaxWindow {
				// Outside window (or old duplicate): discard; honor a poll.
				if f.PF {
					cancelT2(c)
					t.sendRRGuarded(c, src, dst)
				} else {
					t.scheduleT2(c, src, dst)
				}
				return
			}
			// In-window, ahead of V(R): buffer (copy — f.Info may alias) and
			// request every not-yet-requested hole in [V(R), N(S)).
			c.rxBuf[f.NS] = rxEntry{pid: f.PID, info: append([]byte(nil), f.Info...)}
			delete(c.srejSent, f.NS) // it arrived
			cancelT2(c)
			t.sendSREJHoles(c, src, dst, f.NS)
			return
		}
		// gap == 0: in sequence. Deliver, then flush contiguous buffered frames.
		delete(c.srejSent, f.NS)
		t.deliverData(c, f.PID, f.Info)
		c.recvSeq = (f.NS + 1) % c.modulo
		for {
			e, ok := c.rxBuf[c.recvSeq]
			if !ok {
				break
			}
			delete(c.rxBuf, c.recvSeq)
			delete(c.srejSent, c.recvSeq)
			t.deliverData(c, e.pid, e.info)
			c.recvSeq = (c.recvSeq + 1) % c.modulo
		}
		// No SREJ needed here: every hole below a still-buffered frame was
		// already requested when that frame arrived (recvSeq only advances, so
		// srejSent still covers it). Re-scanning to recvSeq+window would wrongly
		// SREJ trailing slots the sender has not transmitted.
		// Acknowledge the advanced V(R).
		if f.PF {
			cancelT2(c)
			t.sendRRGuarded(c, src, dst)
		} else {
			t.scheduleT2(c, src, dst)
		}
		return
	}

	// --- SREJ not enabled: existing phase-3 REJ / go-back-N path (unchanged) ---
	if f.NS != expectedNS {
		// Out-of-sequence or duplicate frame.
		gap := (f.NS - expectedNS) % c.modulo
		if gap > 0 && int(gap) <= pp.MaxWindow {
			// Gap within window: send REJ with V(R) = expected, echoing P/F.
			// Mirrors tncd.py:1791-1805.
			cancelT2(c)
			rej := respFrame(src, dst, c.Via, ax25.REJ, f.PF)
			rej.NR = expectedNS
			t.sendFrame(port, rej)
		} else {
			// True duplicate (or gap > window): discard data.
			// Mirrors tncd.py:1806-1815.
			if f.PF {
				cancelT2(c)
				t.sendRRGuarded(c, src, dst)
			} else {
				t.scheduleT2(c, src, dst)
			}
		}
		return
	}
	c.recvSeq = (f.NS + 1) % c.modulo
	if f.PF {
		cancelT2(c)
		t.sendRRGuarded(c, src, dst)
	} else {
		t.scheduleT2(c, src, dst)
	}
	if t.hooks.Data != nil {
		data := f.Info
		if data == nil {
			data = []byte{}
		}
		t.hooks.Data(c, f.PID, data)
	}
}

// deliverData hands an in-sequence payload to the connection owner.
func (t *Table) deliverData(c *Conn, pid uint8, info []byte) {
	if t.hooks.Data == nil {
		return
	}
	if info == nil {
		info = []byte{}
	}
	t.hooks.Data(c, pid, info)
}

// sendSREJHoles sends a single SREJ (response) for each missing sequence number
// in [V(R), upto) that is not buffered and not already requested, recording each
// in srejSent (dedup). F=1 only on the SREJ for V(R) itself (also acknowledges).
func (t *Table) sendSREJHoles(c *Conn, src, dst string, upto uint8) {
	for i := 0; i < int(c.modulo); i++ { // bound defends against a bad upto
		ns := (c.recvSeq + uint8(i)) % c.modulo
		if ns == upto {
			break
		}
		if _, buffered := c.rxBuf[ns]; buffered {
			continue
		}
		if c.srejSent[ns] {
			continue
		}
		srej := respFrame(src, dst, c.Via, ax25.SREJ, ns == c.recvSeq) // F=1 iff V(R)
		srej.NR = ns
		c.srejSent[ns] = true
		t.sendFrame(c.Port, srej)
	}
}

// dispatchS handles received S-frames (RR, RNR, REJ).
// Order: ackFrames first, then RNR/RR busy-flag, then REJ retransmit.
// Mirrors tncd.py:2064-2096. The poll-response side (P=1 → deferred RR F=1)
// is Task 10 — see TODO below.
func (t *Table) dispatchS(port int, f *ax25.Frame, src, dst string) {
	// local=dst, remote=src
	c := t.Get(port, dst, src)
	if c == nil || c.State != Connected {
		return
	}

	// 1. Process cumulative ACK (always first, per Python order).
	t.ackFrames(c, f.NR)

	// Single-SREJ (v2.2): ackFrames above already acked <= N(R)-1 and left
	// frame N(R) in the retransmit buffer. Retransmit ONLY that frame — not
	// go-back-N. (We never send SREJ ourselves; sending is phase 3.5.)
	if f.Type == ax25.SREJ {
		c.remoteBusy = false
		if raw, ok := c.retransmitBuf[f.NR]; ok {
			if orig, err := ax25.ParseModulo(raw, int(c.modulo)); err == nil {
				rf := &ax25.Frame{
					Dst: mustParseAddr(c.Remote), Src: mustParseAddr(c.Local),
					Type: ax25.I, Modulo: c.modulo,
					NS: orig.NS, NR: c.recvSeq, Command: true,
					PID: orig.PID, Info: orig.Info,
				}
				for _, v := range c.Via {
					a, _ := ax25.ParseAddress(v)
					rf.Via = append(rf.Via, a)
				}
				c.retransmitBuf[f.NR] = rf.Bytes()
				delete(c.iframeTimestamps, f.NR) // Karn: no RTT sample on retransmit
				t.sendFrame(c.Port, rf)
			}
		}
		return // SREJ has no REJ/RR busy semantics beyond the above
	}

	// 2. RNR/RR busy-flag update.
	switch f.Type {
	case ax25.RNR:
		c.remoteBusy = true
	case ax25.RR:
		if c.remoteBusy {
			c.remoteBusy = false
			t.drainOutbound(c)
		}
	}

	// 3. REJ: retransmit from requested sequence number.
	if f.Type == ax25.REJ {
		c.remoteBusy = false
		t.retransmitFrom(c, f.NR)
	}

	// Poll-response side: if P=1, respond with RR F=1.
	// Defer via Hooks.Defer so that any I-frames already queued on the event
	// loop (from the same KISS burst) are processed first, advancing recvSeq
	// before we build the response.
	// Exception: REJ P=1 responds immediately — a deferred call_soon would
	// invoke _retransmit_from a second time, flooding the TNC with duplicates.
	// Mirrors tncd.py:2098-2116 (_dispatch_s poll-response block).
	if f.PF {
		// Re-lookup in case state changed above (tncd.py:2108).
		conn2 := t.Get(port, dst, src)
		if conn2 != nil && conn2.State == Connected {
			if f.Type == ax25.REJ {
				// Immediate: avoid double retransmit.
				t.sendRRGuarded(conn2, src, dst)
			} else {
				// Deferred: let queued I-frames advance recvSeq first.
				c2 := conn2
				s, d := src, dst
				t.defer_(func() {
					if c2.State == Connected {
						t.sendRRGuarded(c2, s, d)
					}
				})
			}
		}
	}
}

// sendXIDCommand sends an XID command (P=1) advertising our SREJ capability, so
// that when tncd is the connection initiator the peer (which does not initiate
// XID) learns we support SREJ and negotiates it. Mirrors Direwolf's initiator
// sending XID after UA.
func (t *Table) sendXIDCommand(c *Conn) {
	pp := t.portParams(c.Port)
	params := ax25.XIDParams{
		FullDuplex:       false,
		SREJ:             ax25.SREJSingle,
		Modulo:           128,
		IFieldLenRxBytes: 256,
		WindowRx:         pp.MaxWindow,
	}
	f := cmdFrame(c.Remote, c.Local, c.Via, ax25.XID, true) // command, P=1
	f.Info = params.Encode(true)                            // command form
	t.sendFrame(c.Port, f)
	c.xidPending = true
}

// negotiateSREJ returns the agreed SREJ mode for the connection: SREJSingle iff
// the port allows SREJ, the link is mod-128, and the peer advertised
// >= SREJSingle; otherwise SREJNone. Mirrors Direwolf's "keep the lower value".
func (t *Table) negotiateSREJ(c *Conn, peerSREJ ax25.SREJMode) ax25.SREJMode {
	if t.portParams(c.Port).SREJ && c.modulo == 128 && peerSREJ >= ax25.SREJSingle {
		return ax25.SREJSingle
	}
	return ax25.SREJNone
}

// dispatchXID handles both XID commands (answerer role) and XID responses
// (initiator role, after sendXIDCommand).
//
// Command (answerer): negotiate SREJ, enable it, and reply with an XID response.
// Mirrors Direwolf xid_frame (mdl_state_0_ready, command branch).
//
// Response (initiator): adopt the negotiated SREJ from the peer's reply; no
// further reply is sent. This is the path triggered after Task 3's sendXIDCommand.
func (t *Table) dispatchXID(port int, f *ax25.Frame, src, dst string) {
	if !t.isLocal(port, dst) {
		return
	}
	c := t.Get(port, dst, src)
	if c == nil {
		return
	}
	their, err := ax25.ParseXID(f.Info)
	if err != nil {
		return
	}

	if !f.Command {
		// Response to our initiated XID command: adopt the negotiated SREJ, no reply.
		// Only act when Connected — an XID response is only valid on an established
		// connection; ignore unsolicited or duplicate responses in other states.
		if c.State != Connected {
			return
		}
		neg := t.negotiateSREJ(c, their.SREJ)
		c.xidPending = false
		c.srejEnabled = neg >= ax25.SREJSingle
		return
	}
	if !f.PF {
		return // command must have P=1
	}

	// Command (answerer role): negotiate, enable, and respond.
	pp := t.portParams(port)
	neg := t.negotiateSREJ(c, their.SREJ)
	c.srejEnabled = neg >= ax25.SREJSingle
	window := pp.MaxWindow
	if their.WindowRx > 0 && their.WindowRx < window {
		window = their.WindowRx
	}
	rsp := ax25.XIDParams{
		FullDuplex:       false,
		SREJ:             neg,
		Modulo:           128,
		IFieldLenRxBytes: 256,
		WindowRx:         window,
	}
	out := respFrame(src, dst, c.Via, ax25.XID, true) // response, F=1
	out.Info = rsp.Encode(false)
	t.sendFrame(port, out)
}

// --- helpers ---

// connsByPair returns all connections matching (local, remote) regardless of port,
// keyed by port. Used for overheard-frame checks.
func (t *Table) connsByPair(local, remote string) map[int]*Conn {
	lk := strings.ToUpper(strings.TrimSpace(local))
	rk := strings.ToUpper(strings.TrimSpace(remote))
	out := make(map[int]*Conn)
	for k, c := range t.conns {
		if k.local == lk && k.remote == rk {
			out[k.port] = c
		}
	}
	return out
}

// addrSliceToStrings converts []ax25.Address to []string.
func addrSliceToStrings(addrs []ax25.Address) []string {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		out[i] = a.String()
	}
	return out
}

// reversed returns a new slice with elements in reverse order.
func reversed(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}
