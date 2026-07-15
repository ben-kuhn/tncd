package agwpe

import (
	"encoding/binary"
	"fmt"
	"log"
	"strings"

	agwpepkg "github.com/ben-kuhn/tncd/v2/agwpe"
	"github.com/ben-kuhn/tncd/v2/ax25"
	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
)

// routedKinds are the frame kinds that require a valid port number.
// Mirrors tncd.py:201 _routed_kinds.
var routedKinds = map[byte]bool{
	'M': true, 'V': true, 'C': true, 'c': true, 'v': true,
	'D': true, 'd': true, 'K': true, 'g': true, 'H': true,
	'y': true, 'Y': true,
}

// handleFrame dispatches a complete AGWPE frame. Must be called on the engine loop.
// Mirrors tncd.py:184-457 (handle_frame).
func (c *client) handleFrame(hdr agwpepkg.Header, data []byte) {
	port := int(hdr.Port)
	kind := hdr.Kind
	pid := hdr.PID
	from := hdr.CallFrom
	to := hdr.CallTo

	log.Printf("agwpe: handle_frame: port=%d kind=%c from=%q to=%q len=%d",
		port, kind, from, to, len(data))

	// Validate port for routed kinds (tncd.py:200-205).
	if routedKinds[kind] {
		if port >= c.b.PortCount() {
			log.Printf("agwpe: ignoring frame for invalid port %d (kind=%c)", port, kind)
			return
		}
	}

	switch kind {
	case 'P':
		// Login: accepted, no response (tncd.py:207-208).
		log.Printf("agwpe: LOGIN from=%q (accepted)", from)

	case 'R':
		// Version request: reply with 8-byte <II (2,0) (tncd.py:210-212, 513-516).
		log.Printf("agwpe: VERSION request")
		c.sendVersion()

	case 'G':
		// Port info: reply with "{count};{name1};..." (tncd.py:214-216, 518-524).
		log.Printf("agwpe: PORT INFO request")
		c.sendPortInfo()

	case 'g':
		// Port capabilities: 12-byte <8BI (tncd.py:218-230).
		log.Printf("agwpe: PORT CAPABILITIES request for port %d", port)
		c.sendPortCaps(port)

	case 'X':
		// Register callsign (tncd.py:232-243).
		call := strings.ToUpper(from)
		log.Printf("agwpe: REGISTER from=%q", call)
		// Check for duplicate registration across all clients.
		for _, other := range c.b.Clients() {
			if other == c {
				continue
			}
			if other.RegisteredCalls()[call] {
				log.Printf("agwpe: rejecting duplicate registration of %q", call)
				c.sendFrame(uint8(port), 'X', from, "", []byte{0x00})
				return
			}
		}
		c.mu.Lock()
		c.registeredCalls[call] = true
		c.mu.Unlock()
		c.sendFrame(uint8(port), 'X', from, "", []byte{0x01})

	case 'x':
		// Unregister callsign: no response (tncd.py:245-249).
		call := strings.ToUpper(from)
		log.Printf("agwpe: UNREGISTER from=%q", call)
		c.mu.Lock()
		delete(c.registeredCalls, call)
		c.mu.Unlock()

	case 'm':
		// Toggle monitoring (tncd.py:251-254).
		c.mu.Lock()
		c.monitoring = !c.monitoring
		mon := c.monitoring
		c.mu.Unlock()
		log.Printf("agwpe: monitoring %v", mon)

	case 'M':
		// Send UNPROTO (UI) frame (tncd.py:256-259).
		log.Printf("agwpe: UNPROTO from %q to %q", from, to)
		c.sendUnproto(from, to, pid, data, nil, port)

	case 'V':
		// UNPROTO via digipeaters (tncd.py:261-272).
		log.Printf("agwpe: UNPROTO VIA from %q to %q", from, to)
		if len(data) > 0 {
			nVia := int(data[0])
			viaBytes := data[1 : 1+nVia*10]
			info := data[1+nVia*10:]
			vias := decodeVias(viaBytes, nVia)
			c.sendUnproto(from, to, pid, info, vias, port)
		} else {
			c.sendUnproto(from, to, pid, nil, nil, port)
		}

	case 'C':
		// Initiate AX.25 connection (tncd.py:274-303).
		log.Printf("agwpe: CONNECT %q -> %q", from, to)
		if !c.b.PortOnline(port) {
			log.Printf("agwpe: CONNECT on offline port %d: BUSY", port)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		conn, err := c.b.L2().Connect(port, from, to, nil)
		if err != nil {
			log.Printf("agwpe: L2 Connect error: %v", err)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		if conn.State != l2pkg.Connecting && conn.Owner != nil && conn.Owner != c {
			log.Printf("agwpe: rejecting non-owner 'C' for active %s->%s", from, to)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		conn.Owner = c

	case 'c':
		// Connect with non-standard PID (tncd.py:305-329).
		log.Printf("agwpe: CONNECT (PID=0x%02X) %q -> %q", pid, from, to)
		if !c.b.PortOnline(port) {
			log.Printf("agwpe: CONNECT on offline port %d: BUSY", port)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		conn, err := c.b.L2().Connect(port, from, to, nil)
		if err != nil {
			log.Printf("agwpe: L2 Connect error: %v", err)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		if conn.State != l2pkg.Connecting && conn.Owner != nil && conn.Owner != c {
			log.Printf("agwpe: rejecting non-owner 'c' for active %s->%s", from, to)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		conn.Owner = c

	case 'v':
		// Connect via digipeaters (tncd.py:331-364).
		var vias []string
		if len(data) > 0 {
			nVia := int(data[0])
			viaBytes := data[1 : 1+nVia*10]
			vias = decodeVias(viaBytes, nVia)
		}
		log.Printf("agwpe: CONNECT %q -> %q via %v", from, to, vias)
		if !c.b.PortOnline(port) {
			log.Printf("agwpe: CONNECT on offline port %d: BUSY", port)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		conn, err := c.b.L2().Connect(port, from, to, vias)
		if err != nil {
			log.Printf("agwpe: L2 Connect error: %v", err)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		if conn.State != l2pkg.Connecting && conn.Owner != nil && conn.Owner != c {
			log.Printf("agwpe: rejecting non-owner 'v' for active %s->%s", from, to)
			busy := fmt.Sprintf("*** BUSY From %s\r", from)
			c.sendFrame(uint8(port), 'd', from, to, []byte(busy))
			return
		}
		conn.Owner = c

	case 'D':
		// Send data over connected session (tncd.py:366-389).
		conn := c.b.L2().Get(port, from, to)
		if conn == nil || conn.State != l2pkg.Connected {
			state := "None"
			if conn != nil {
				state = conn.State.String()
			}
			log.Printf("agwpe: 'D' from %q to %q: no active connection (state=%s)", from, to, state)
			return
		}
		if conn.Owner != c {
			log.Printf("agwpe: rejecting non-owner 'D' for %s->%s", from, to)
			return
		}
		framePID := pid
		if framePID == 0 {
			framePID = 0xF0
		}
		if len(data) == 0 {
			data = []byte{}
		}
		c.b.L2().SendData(conn, framePID, data)

	case 'd':
		// Disconnect (tncd.py:391-406).
		conn := c.b.L2().Get(port, from, to)
		if conn == nil {
			log.Printf("agwpe: 'd' disconnect with no connection %q -> %q", from, to)
			return
		}
		if conn.Owner != c {
			log.Printf("agwpe: rejecting non-owner 'd' for %s->%s", from, to)
			return
		}
		log.Printf("agwpe: DISCONNECT %q -> %q", from, to)
		c.b.L2().Disconnect(conn)

	case 'K':
		// Raw AX.25 frame (tncd.py:408-420).
		c.mu.Lock()
		hasReg := len(c.registeredCalls) > 0
		c.mu.Unlock()
		if !hasReg {
			log.Printf("agwpe: rejecting 'K' from unregistered client")
			return
		}
		// Strip leading 0x00 byte (pe prepends it as port indicator).
		raw := data
		if len(raw) > 0 && raw[0] == 0x00 {
			raw = raw[1:]
		}
		log.Printf("agwpe: raw KISS frame, %d bytes", len(raw))
		if len(raw) > 0 {
			conn := c.b.L2().Get(port, from, to)
			if conn != nil && conn.Owner != nil && conn.Owner != c {
				log.Printf("agwpe: rejecting non-owner 'K' for %s->%s", from, to)
				return
			}
			c.b.SendToKISS(port, raw)
		}

	case 'k':
		// Toggle raw frame reception mode — not supported, log and ignore (tncd.py:422-425).
		log.Printf("agwpe: raw KISS mode toggle received (not supported)")

	case 'y':
		// Outstanding frames on port: reply 0 (tncd.py:427-430).
		log.Printf("agwpe: outstanding frames query for port %d", port)
		payload := make([]byte, 4)
		binary.LittleEndian.PutUint32(payload, 0)
		c.sendFrame(uint8(port), 'y', "", "", payload)

	case 'Y':
		// Outstanding frames for connection: unacked+queued (tncd.py:432-449).
		conn := c.b.L2().Get(port, from, to)
		var count int
		if conn != nil && conn.Owner != nil && conn.Owner != c {
			count = 0
		} else {
			if conn != nil {
				count = c.b.L2().Outstanding(conn)
			}
		}
		log.Printf("agwpe: 'Y' query %q<->%q: outstanding=%d", from, to, count)
		payload := make([]byte, 4)
		binary.LittleEndian.PutUint32(payload, uint32(count))
		c.sendFrame(uint8(port), 'Y', from, to, payload)

	case 'H':
		// Heard stations: reply empty (tncd.py:451-454).
		log.Printf("agwpe: heard stations query for port %d", port)
		c.sendFrame(uint8(port), 'H', "", "", nil)

	default:
		log.Printf("agwpe: unknown frame type %q (%d)", string([]byte{kind}), kind)
	}
}

// sendVersion sends the 8-byte version response (tncd.py:513-516).
func (c *client) sendVersion() {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data[0:4], 2) // MajorRevision
	binary.LittleEndian.PutUint32(data[4:8], 0) // MinorRevision
	c.sendFrame(0, 'R', "", "", data)
}

// sendPortInfo sends the G-frame port info string (tncd.py:518-524).
func (c *client) sendPortInfo() {
	cfg := c.b.Config()
	count := len(cfg.Ports)
	var parts []string
	parts = append(parts, fmt.Sprintf("%d", count))
	for i, p := range cfg.Ports {
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("Port %d", i)
		}
		parts = append(parts, name)
	}
	// Format: "{count};{name1};{name2};...;"
	payload := strings.Join(parts, ";") + ";"
	c.sendFrame(0, 'G', "", "", []byte(payload))
}

// sendPortCaps sends the 12-byte g-frame capabilities (tncd.py:218-230).
func (c *client) sendPortCaps(port int) {
	cfg := c.b.Config()
	// Default KISS params matching tncd.py defaults.
	txDelay := 40
	txTail := 30
	persistence := 63
	slotTime := 20

	if port < len(cfg.Ports) {
		p := cfg.Ports[port]
		if p.KISS.TXDelay != nil {
			txDelay = *p.KISS.TXDelay
		}
		if p.KISS.TXTail != nil {
			txTail = *p.KISS.TXTail
		}
		if p.KISS.Persistence != nil {
			persistence = *p.KISS.Persistence
		}
		if p.KISS.SlotTime != nil {
			slotTime = *p.KISS.SlotTime
		}
	}

	// <8BI: 8 unsigned bytes + 1 uint32 = 12 bytes total.
	// Fields: baud(0), traffic_level(255), tx_delay, tx_tail, persist, slot_time, max_frame(7), active_conns(0), bytes_rcvd(0)
	// (tncd.py:228-229)
	caps := make([]byte, 12)
	caps[0] = 0   // baud indicator (0 = unknown)
	caps[1] = 255 // traffic level
	caps[2] = byte(txDelay)
	caps[3] = byte(txTail)
	caps[4] = byte(persistence)
	caps[5] = byte(slotTime)
	caps[6] = 7                                  // max_frame
	caps[7] = 0                                  // active_conns
	binary.LittleEndian.PutUint32(caps[8:12], 0) // bytes_rcvd
	c.sendFrame(uint8(port), 'g', "", "", caps)
}

// sendFrame builds and queues an AGWPE frame. Mirrors tncd.py:478-511.
func (c *client) sendFrame(port uint8, kind byte, from, to string, data []byte) {
	pkt := agwpepkg.Build(port, kind, 0, from, to, data)
	select {
	case c.writeCh <- pkt:
	default:
		log.Printf("agwpe: write channel full, closing client")
		c.CloseTransport()
	}
}

// sendUnproto builds an AX.25 UI command frame and sends it via bridge.
// Mirrors tncd.py:459-476 (_send_unproto).
func (c *client) sendUnproto(from, to string, pid uint8, data []byte, via []string, port int) {
	if len(via) > 0 {
		log.Printf("agwpe: UNPROTO %s -> %s via %v (%d bytes)", from, to, via, len(data))
	} else {
		log.Printf("agwpe: UNPROTO %s -> %s (%d bytes)", from, to, len(data))
	}

	dstAddr, err := ax25.ParseAddress(to)
	if err != nil {
		log.Printf("agwpe: bad dst address %q: %v", to, err)
		return
	}
	dstAddr.CRH = true // command frame: dst C-bit set

	srcAddr, err := ax25.ParseAddress(from)
	if err != nil {
		log.Printf("agwpe: bad src address %q: %v", from, err)
		return
	}

	framePID := pid
	if framePID == 0 {
		framePID = 0xF0
	}

	f := &ax25.Frame{
		Dst:     dstAddr,
		Src:     srcAddr,
		Type:    ax25.UI,
		Command: true,
		PID:     framePID,
		Info:    data,
	}
	for _, v := range via {
		vAddr, err := ax25.ParseAddress(v)
		if err != nil {
			log.Printf("agwpe: bad via address %q: %v", v, err)
			continue
		}
		f.Via = append(f.Via, vAddr)
	}

	c.b.SendAX25(port, f)
}

// decodeVias decodes n via callsigns from 10-byte-per-entry wire format.
func decodeVias(viaBytes []byte, n int) []string {
	vias := make([]string, 0, n)
	for i := 0; i < n; i++ {
		start := i * 10
		end := start + 10
		if end > len(viaBytes) {
			break
		}
		call := strings.TrimRight(string(viaBytes[start:end]), "\x00")
		vias = append(vias, call)
	}
	return vias
}
