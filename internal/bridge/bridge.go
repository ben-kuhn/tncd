// Package bridge wires together the AX.25 L2 engine, KISS ports, and AGWPE
// clients. All state-touching methods (AddClient, RemoveClient, Clients,
// SendToKISS, SendAX25, OnKISSFrame, PortOnline, PortCount) MUST be called
// on the engine loop unless otherwise noted. Bridge.Start may be called from
// any goroutine; it posts online/offline events back via eng.Do.
package bridge

import (
	"bytes"
	"log"
	"time"

	"github.com/ben-kuhn/tncd/v2/agwpe"
	"github.com/ben-kuhn/tncd/v2/ax25"
	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// Client is what the AGWPE frontend implements (one per TCP client).
// All methods may be called from the engine loop.
type Client interface {
	SendAGWPE(port uint8, kind byte, pid uint8, from, to string, data []byte)
	Monitoring() bool
	RegisteredCalls() map[string]bool
	LastActivity() time.Time
	CloseTransport()
}

// PortSender is the seam between Bridge and a KISS port, allowing tests to
// inject fake senders without a real kiss.Port.
type PortSender interface {
	Send([]byte)
	Online() bool
}

// echoRingSize is the number of recently-sent frames to track for echo
// suppression. Mirrors Python's deque(maxlen=20).
const echoRingSize = 20

// Bridge wires L2, KISS ports, and AGWPE clients together.
type Bridge struct {
	eng     *engine.Engine
	cfg     *config.Config
	l2      *l2pkg.Table
	ports   []PortSender
	clients []Client

	// sentFrames is a ring buffer of normalised AX.25 bytes recently sent to
	// KISS, used to suppress echoes on RX. Mirrors Python _sent_frames deque(maxlen=20).
	sentFrames [][]byte

	// idleSweepTimer is held so we can cancel it on Shutdown.
	idleSweepTimer *engine.Timer
}

// New creates a Bridge. Call Start to connect ports and wire L2 hooks.
func New(eng *engine.Engine, cfg *config.Config) *Bridge {
	return &Bridge{
		eng: eng,
		cfg: cfg,
	}
}

// L2 returns the AX.25 connection table.
func (b *Bridge) L2() *l2pkg.Table { return b.l2 }

// PortCount returns the number of configured ports.
func (b *Bridge) PortCount() int { return len(b.ports) }

// PortOnline reports whether port i is online.
// Must be called on the engine loop.
func (b *Bridge) PortOnline(port int) bool {
	if port < 0 || port >= len(b.ports) {
		return false
	}
	return b.ports[port].Online()
}

// Clients returns a snapshot of the connected AGWPE clients.
// Must be called on the engine loop.
func (b *Bridge) Clients() []Client {
	out := make([]Client, len(b.clients))
	copy(out, b.clients)
	return out
}

// AddClient registers a new AGWPE client. Returns false when max_clients is
// reached. Must be called on the engine loop.
func (b *Bridge) AddClient(c Client) bool {
	if b.cfg.Server.MaxClients > 0 && len(b.clients) >= b.cfg.Server.MaxClients {
		return false
	}
	b.clients = append(b.clients, c)
	return true
}

// RemoveClient deregisters a client and sends DISC for any connections it owns.
// Mirrors tncd.py:1202-1220. Must be called on the engine loop.
func (b *Bridge) RemoveClient(c Client) {
	for i, cl := range b.clients {
		if cl == c {
			b.clients = append(b.clients[:i], b.clients[i+1:]...)
			break
		}
	}
	// Remove owned connections (sends DISC for Connected/Connecting ones).
	b.l2.RemoveOwned(c)
	// Clear registered callsigns.
	for k := range c.RegisteredCalls() {
		delete(c.RegisteredCalls(), k)
	}
}

// SendToKISS sends raw AX.25 bytes to the given port, tracking the frame in
// the echo-suppression ring. Mirrors tncd.py:1330-1337.
// Must be called on the engine loop.
func (b *Bridge) SendToKISS(port int, raw []byte) {
	if port < 0 || port >= len(b.ports) {
		return
	}
	p := b.ports[port]
	if !p.Online() {
		return
	}
	norm := normalizeHBits(raw)
	b.trackSent(norm)
	p.Send(raw)
}

// SendAX25 serialises a frame and sends it to the given KISS port.
// Must be called on the engine loop.
func (b *Bridge) SendAX25(port int, f *ax25.Frame) {
	b.SendToKISS(port, f.Bytes())
}

// trackSent appends a normalised frame to the echo ring, evicting oldest if
// at capacity. Mirrors Python deque(maxlen=20).
func (b *Bridge) trackSent(norm []byte) {
	if len(b.sentFrames) >= echoRingSize {
		b.sentFrames = b.sentFrames[1:]
	}
	b.sentFrames = append(b.sentFrames, norm)
}

// isEcho reports whether the given (already normalised) frame matches any
// entry in the sent-frame ring.
func (b *Bridge) isEcho(norm []byte) bool {
	for _, s := range b.sentFrames {
		if bytes.Equal(s, norm) {
			return true
		}
	}
	return false
}

// OnKISSFrame is called by a kiss.Port's reader goroutine (via engine.Do)
// when a data frame arrives. Mirrors tncd.py:1646-1711.
// Must be called on the engine loop.
func (b *Bridge) OnKISSFrame(f kiss.RXFrame) {
	raw := f.Data // cmd byte already stripped by kiss.Port

	// Echo suppression: discard frames that are our own transmissions,
	// normalising H-bits so digipeated echoes also match.
	norm := normalizeHBits(raw)
	if b.isEcho(norm) {
		log.Printf("bridge: port %d ignoring echoed TX frame", f.Port)
		return
	}

	// Parse the AX.25 frame.
	frame, err := ax25.Parse(raw)
	if err != nil {
		log.Printf("bridge: port %d failed to parse AX.25 frame: %v (len=%d raw=%x)",
			f.Port, err, len(raw), raw[:min(len(raw), 32)])
		return
	}

	// Forward to L2 state machine (handles SABM/UA/DM/DISC/FRMR/I/RR/RNR/REJ).
	b.l2.OnFrame(f.Port, frame)

	// Distribute monitor frames to monitoring clients.
	distributeMonitor(b.clients, f.Port, frame)
}

// normalizeHBits clears the H-bit (bit 7) of the SSID byte of every via
// address. This makes digipeated echoes compare equal to the original.
// Mirrors tncd.py:1307-1328 (_normalize_hbits).
func normalizeHBits(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	out := make([]byte, len(raw))
	copy(out, raw)
	// Address field: dst (7 bytes) + src (7 bytes) = 14 bytes.
	// Byte 13 is the SSID byte of src. If bit 0 is set, src is the last address
	// (no via). If not set, via addresses follow starting at byte 14.
	if len(out) > 13 && (out[13]&0x01) == 0 {
		// src lacks extension bit → via addresses follow
		i := 14
		for i+6 < len(out) {
			out[i+6] &= 0x7F // clear H-bit
			if out[i+6]&0x01 != 0 {
				break // last address
			}
			i += 7
		}
	}
	return out
}

// min returns the smaller of two ints.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Start opens all KISS ports asynchronously (one goroutine per port so a dead
// TNC doesn't stall startup). Online/offline transitions post back to the
// engine loop via eng.Do. Mirrors tncd.py:1123-1142 async port connection.
//
// Start also wires L2 hooks and schedules the idle sweep.
func (b *Bridge) Start() error {
	// Build L2 params from config.
	params := make([]l2pkg.PortParams, len(b.cfg.Ports))
	for i, p := range b.cfg.Ports {
		params[i] = l2pkg.DeriveParams(
			p.OTABaudrate,
			b.cfg.AX25.MaxWindow,
			b.cfg.AX25.N2Retry,
			b.cfg.AX25.T3Timeout,
		)
	}

	hooks := l2pkg.Hooks{
		SendAX25: func(port int, f *ax25.Frame) {
			b.SendAX25(port, f)
		},
		Connected: func(c *l2pkg.Conn, incoming bool) {
			b.notifyConnected(c, incoming)
		},
		ConnectFailed: func(c *l2pkg.Conn, reason l2pkg.FailReason) {
			b.notifyConnectFailed(c, reason)
		},
		Data: func(c *l2pkg.Conn, pid uint8, data []byte) {
			b.notifyData(c, pid, data)
		},
		Disconnected: func(c *l2pkg.Conn) {
			b.notifyDisconnected(c)
		},
		Defer: func(fn func()) {
			b.eng.Do(fn)
		},
	}
	b.l2 = l2pkg.NewTable(b.eng, hooks, params)

	// Build port senders — one per configured port. Ports are started
	// asynchronously; the bridge slot is initialised to a sentinel that
	// reports offline until the goroutine posts success.
	b.ports = make([]PortSender, len(b.cfg.Ports))
	for i := range b.cfg.Ports {
		b.ports[i] = &offlineSentinel{}
	}

	for i, portCfg := range b.cfg.Ports {
		idx := i
		pc := portCfg
		go b.connectPort(idx, pc)
	}

	// Start idle sweep if configured.
	if b.cfg.Server.IdleTimeout > 0 {
		b.scheduleIdleSweep()
	}

	return nil
}

// connectPort opens one KISS port in a background goroutine and posts the
// result back to the engine loop. Mirrors tncd.py:1123-1142.
func (b *Bridge) connectPort(idx int, pc config.Port) {
	tr, err := buildTransport(pc)
	if err != nil {
		log.Printf("bridge: port %d build transport error: %v", idx, err)
		return
	}

	params := pc.KISS
	port := kiss.NewPort(idx, tr, params,
		func(f kiss.RXFrame) {
			b.eng.Do(func() { b.OnKISSFrame(f) })
		},
		func(portNum int) {
			b.eng.Do(func() { b.portWentOffline(portNum) })
		},
	)

	if err := port.Start(); err != nil {
		log.Printf("bridge: port %d start error: %v", idx, err)
		if pc.Type == "bluetooth" && pc.Reconnect {
			delay := pc.ReconnectDelay
			b.scheduleReconnect(idx, pc, delay)
		}
		return
	}

	b.eng.Do(func() {
		b.ports[idx] = port
		log.Printf("bridge: port %d online", idx)
	})
}

// portWentOffline is called on the engine loop when a port's reader goroutine
// exits unexpectedly. Mirrors tncd.py:1246-1264.
func (b *Bridge) portWentOffline(portNum int) {
	log.Printf("bridge: port %d went offline", portNum)
	b.l2.PortOffline(portNum)

	// Schedule reconnect for bluetooth ports configured for it.
	if portNum < len(b.cfg.Ports) {
		pc := b.cfg.Ports[portNum]
		if pc.Type == "bluetooth" && pc.Reconnect {
			b.scheduleReconnect(portNum, pc, pc.ReconnectDelay)
		}
	}
}

// scheduleReconnect schedules a port reconnect attempt with exponential backoff.
func (b *Bridge) scheduleReconnect(idx int, pc config.Port, delay float64) {
	if delay <= 0 {
		delay = 5
	}
	maxDelay := pc.ReconnectMaxDelay
	if maxDelay <= 0 {
		maxDelay = 60
	}
	d := time.Duration(delay * float64(time.Second))
	log.Printf("bridge: port %d bluetooth reconnect in %.1fs", idx, delay)
	b.eng.After(d, func() {
		nextDelay := delay * 2
		if nextDelay > maxDelay {
			nextDelay = maxDelay
		}
		go b.connectPortWithBackoff(idx, pc, nextDelay)
	})
}

// connectPortWithBackoff is like connectPort but also schedules retry on failure.
func (b *Bridge) connectPortWithBackoff(idx int, pc config.Port, nextDelay float64) {
	tr, err := buildTransport(pc)
	if err != nil {
		log.Printf("bridge: port %d build transport error (reconnect): %v", idx, err)
		b.eng.Do(func() { b.scheduleReconnect(idx, pc, nextDelay) })
		return
	}

	params := pc.KISS
	port := kiss.NewPort(idx, tr, params,
		func(f kiss.RXFrame) {
			b.eng.Do(func() { b.OnKISSFrame(f) })
		},
		func(portNum int) {
			b.eng.Do(func() { b.portWentOffline(portNum) })
		},
	)

	if err := port.Start(); err != nil {
		log.Printf("bridge: port %d reconnect error: %v", idx, err)
		b.eng.Do(func() { b.scheduleReconnect(idx, pc, nextDelay) })
		return
	}

	b.eng.Do(func() {
		b.ports[idx] = port
		log.Printf("bridge: port %d reconnected", idx)
	})
}

// Shutdown gracefully closes all clients and ports.
func (b *Bridge) Shutdown() {
	if b.idleSweepTimer != nil {
		b.idleSweepTimer.Cancel()
		b.idleSweepTimer = nil
	}
	for _, c := range b.clients {
		c.CloseTransport()
	}
	for i, p := range b.ports {
		if kp, ok := p.(*kiss.Port); ok {
			kp.Close()
		}
		_ = i
	}
}

// --- L2 Hook notifications ---

// notifyConnected is called by L2 when a connection is established.
// For incoming (remote-initiated) connections, owner is found by searching
// clients for one whose RegisteredCalls contains the local callsign.
// Mirrors tncd.py:1898-1912 (incoming) and 1929-1933 (outgoing).
func (b *Bridge) notifyConnected(c *l2pkg.Conn, incoming bool) {
	if incoming {
		// Assign owner: first client with local call registered.
		for _, cl := range b.clients {
			if cl.RegisteredCalls()[c.Local] {
				c.Owner = cl
				break
			}
		}
		msg := []byte("*** CONNECTED To Station " + c.Remote + "\r")
		if c.Owner != nil {
			c.Owner.(Client).SendAGWPE(uint8(c.Port), 'C', 0, c.Remote, c.Local, msg)
		}
	} else {
		// Outgoing: owner was set at connect time by the AGWPE frontend.
		msg := []byte("*** CONNECTED With " + c.Remote + "\r")
		if c.Owner != nil {
			c.Owner.(Client).SendAGWPE(uint8(c.Port), 'C', 0, c.Remote, c.Local, msg)
		}
	}
}

// notifyConnectFailed is called by L2 when an outgoing connection attempt fails.
// FailTimeout → BUSY; FailDM → "failed". Mirrors tncd.py:1519 and 1953.
func (b *Bridge) notifyConnectFailed(c *l2pkg.Conn, reason l2pkg.FailReason) {
	if c.Owner == nil {
		return
	}
	var msg []byte
	switch reason {
	case l2pkg.FailTimeout:
		msg = []byte("*** BUSY From " + c.Remote + "\r")
	case l2pkg.FailDM:
		msg = []byte("*** CONNECTED With " + c.Remote + " failed\r")
	default:
		msg = []byte("*** CONNECTED With " + c.Remote + " failed\r")
	}
	c.Owner.(Client).SendAGWPE(uint8(c.Port), 'd', 0, c.Remote, c.Local, msg)
}

// notifyData delivers received I-frame data to the connection owner.
// Mirrors tncd.py:1834 ('D' delivery).
func (b *Bridge) notifyData(c *l2pkg.Conn, pid uint8, data []byte) {
	if c.Owner == nil {
		return
	}
	c.Owner.(Client).SendAGWPE(uint8(c.Port), 'D', pid, c.Remote, c.Local, data)
}

// notifyDisconnected notifies the connection owner of a disconnect.
// Mirrors tncd.py:1550 and 1938 (*** DISCONNECTED From {remote}\r).
func (b *Bridge) notifyDisconnected(c *l2pkg.Conn) {
	if c.Owner == nil {
		return
	}
	msg := []byte("*** DISCONNECTED From " + c.Remote + "\r")
	c.Owner.(Client).SendAGWPE(uint8(c.Port), 'd', 0, c.Remote, c.Local, msg)
}

// --- Idle sweep ---

// scheduleIdleSweep schedules the next idle sweep 30s from now.
// Mirrors tncd.py:1189-1200.
func (b *Bridge) scheduleIdleSweep() {
	b.idleSweepTimer = b.eng.After(30*time.Second, func() {
		b.sweepIdleClients()
	})
}

// sweepIdleClients closes clients that have exceeded idle_timeout.
// Mirrors tncd.py:1189-1200.
func (b *Bridge) sweepIdleClients() {
	idleTimeout := time.Duration(b.cfg.Server.IdleTimeout) * time.Second
	if idleTimeout <= 0 {
		return
	}
	now := time.Now()
	for _, c := range b.clients {
		if now.Sub(c.LastActivity()) > idleTimeout {
			log.Printf("bridge: closing idle AGWPE client")
			c.CloseTransport()
		}
	}
	b.scheduleIdleSweep()
}

// --- Sentinel for offline port slots ---

// offlineSentinel is used before a port's goroutine posts online.
type offlineSentinel struct{}

func (*offlineSentinel) Send([]byte)  {}
func (*offlineSentinel) Online() bool { return false }

// --- AGWPE frame builder helper ---

// buildAGWPE is a shorthand for agwpe.Build used by notification helpers.
func buildAGWPE(port uint8, kind byte, pid uint8, from, to string, data []byte) []byte {
	return agwpe.Build(port, kind, pid, from, to, data)
}
