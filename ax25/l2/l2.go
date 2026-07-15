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
	MaxWindow int
	N2Retry   int
	T1        time.Duration // max(3s, 2*(window*frameTime + 1s))
	T2        time.Duration // max(100ms, 1.2*frameTime)
	T3        time.Duration // 0 disables
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

// Hooks are the callbacks l2 fires when state changes. All hooks are called
// synchronously on the engine loop (or the caller's goroutine in tests).
type Hooks struct {
	SendAX25      func(port int, f *ax25.Frame)         // → KISS TX
	Connected     func(c *Conn, incoming bool)          // → AGWPE 'C'
	ConnectFailed func(c *Conn)                         // SABM gave up → 'd'
	Data          func(c *Conn, pid uint8, data []byte) // → AGWPE 'D'
	Disconnected  func(c *Conn)                         // → AGWPE 'd'
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
func (t *Table) startT1(c *Conn) {
	c.t1 = cancelTimer(c.t1)
	d := c.t1Value
	if d <= 0 {
		d = t.portParams(c.Port).T1
	}
	conn := c // capture
	c.t1 = t.clock.After(d, func() { t.t1Expired(conn) })
}

// cancelT1 stops the T1 timer if running.
func cancelT1(c *Conn) {
	c.t1 = cancelTimer(c.t1)
}

// t1Expired handles T1 timer expiry (tncd.py:1500-1539 for CONNECTING case).
func (t *Table) t1Expired(c *Conn) {
	c.t1 = nil
	pp := t.portParams(c.Port)

	if c.State == Connecting {
		// SABM retransmission while waiting for UA (AX.25 6.3.1)
		// tncd.py:1512-1539
		c.t1Polls++
		if c.t1Polls > pp.N2Retry {
			// N2 exhausted — give up
			c.State = Disconnected
			if t.hooks.ConnectFailed != nil {
				t.hooks.ConnectFailed(c)
			}
			t.removeConn(c)
			return
		}
		// Retransmit SABM (with T1 backoff — Karn arrives in Task 9;
		// for now just use the default T1 so tests are deterministic)
		t.sendSABM(c)
		t.startT1(c)
		return
	}

	// TODO Task 9: Connected-state T1 handling (RR poll + I-frame retransmit)
}

// sendFrame is a helper that calls hooks.SendAX25.
func (t *Table) sendFrame(port int, f *ax25.Frame) {
	if t.hooks.SendAX25 != nil {
		t.hooks.SendAX25(port, f)
	}
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

// sendSABM sends a SABM P=1 command frame to the remote.
func (t *Table) sendSABM(c *Conn) {
	f := cmdFrame(c.Remote, c.Local, c.Via, ax25.SABM, true)
	t.sendFrame(c.Port, f)
}

// Connect initiates an outgoing AX.25 connection (tncd.py:274-304).
// Sends SABM P=1, sets state=Connecting, starts T1.
// Returns an error if the connection table is full.
func (t *Table) Connect(port int, local, remote string, via []string) (*Conn, error) {
	c := t.getOrCreate(port, local, remote)
	if c == nil {
		return nil, fmt.Errorf("l2: connection limit reached (%d)", maxConnections)
	}
	c.State = Connecting
	c.sendSeq = 0
	c.recvSeq = 0
	c.t1Polls = 0
	c.Via = via
	c.t1Value = t.portParams(port).T1

	t.sendSABM(c)
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

// SendData queues an I-frame for transmission. Implemented in Task 9.
func (t *Table) SendData(c *Conn, pid uint8, data []byte) {
	// TODO Task 9
}

// Outstanding returns the number of unacknowledged + queued frames. Task 9.
func (t *Table) Outstanding(c *Conn) int {
	// TODO Task 9
	return 0
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
		// TODO Task 9-10: I-frame handling
	case ax25.RR, ax25.RNR, ax25.REJ:
		// TODO Task 9-10: S-frame handling
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

// PortOffline handles a port going offline. Task 10.
func (t *Table) PortOffline(port int) {
	// TODO Task 10
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
		t.hooks.Connected(c, true)
	}
}

// dispatchSABME handles SABME — reject with DM P=1 (tncd.py:1686-1695).
func (t *Table) dispatchSABME(port int, f *ax25.Frame, src, dst string) {
	dm := respFrame(src, dst, nil, ax25.DM, true)
	t.sendFrame(port, dm)
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
			t.hooks.Connected(c, false)
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
	if c.State == Connecting {
		if t.hooks.ConnectFailed != nil {
			t.hooks.ConnectFailed(c)
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
		// No connection anywhere — respond with DM (tncd.py:1987-1994).
		dm := respFrame(src, dst, nil, ax25.DM, f.PF)
		t.sendFrame(port, dm)
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
