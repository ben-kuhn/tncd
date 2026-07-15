package agwpe_test

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	agwpepkg "github.com/ben-kuhn/tncd/v2/agwpe"
	"github.com/ben-kuhn/tncd/v2/ax25"
	l2pkg "github.com/ben-kuhn/tncd/v2/ax25/l2"
	"github.com/ben-kuhn/tncd/v2/internal/bridge"
	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/engine"
	frontend "github.com/ben-kuhn/tncd/v2/internal/frontend/agwpe"
	"github.com/ben-kuhn/tncd/v2/kiss"
)

// --- test helpers ---

// testFrame holds a parsed AGWPE header plus its payload.
type testFrame struct {
	agwpepkg.Header
	Data []byte
}

// fakePort implements bridge.PortSender and records sent frames.
type fakePort struct {
	mu     sync.Mutex
	frames [][]byte
	online bool
}

func newFakePort(online bool) *fakePort { return &fakePort{online: online} }

func (p *fakePort) Send(raw []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]byte, len(raw))
	copy(cp, raw)
	p.frames = append(p.frames, cp)
}

func (p *fakePort) Online() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.online
}

func (p *fakePort) getSent() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.frames))
	copy(out, p.frames)
	return out
}

// makeBridgeWithFakePort creates a Bridge with manually-wired L2 and fake ports.
func makeBridgeWithFakePort(t *testing.T, eng *engine.Engine, fps []*fakePort, cfgPorts []config.Port) *bridge.Bridge {
	t.Helper()
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8, IdleTimeout: 0},
		AX25:   config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 0},
		Ports:  cfgPorts,
	}
	b := bridge.New(eng, cfg)
	params := make([]l2pkg.PortParams, len(fps))
	for i := range fps {
		params[i] = l2pkg.DeriveParams(1200, 3, 10, 0)
	}
	senders := make([]bridge.PortSender, len(fps))
	for i, fp := range fps {
		senders[i] = fp
	}
	bridge.InjectPorts(b, eng, params, senders)
	return b
}

// dialServe starts a Serve on a free port and returns the listener and a dialed connection.
func dialServe(t *testing.T, eng *engine.Engine, b *bridge.Bridge) (net.Listener, net.Conn) {
	t.Helper()
	ln, err := frontend.Serve(eng, b, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	conn, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		ln.Close()
		t.Fatalf("dial: %v", err)
	}
	return ln, conn
}

// writeFrame sends one AGWPE request frame over conn.
func writeFrame(t *testing.T, conn net.Conn, port uint8, kind byte, pid uint8, from, to string, data []byte) {
	t.Helper()
	pkt := agwpepkg.Build(port, kind, pid, from, to, data)
	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(pkt); err != nil {
		t.Fatalf("writeFrame %c: %v", kind, err)
	}
}

// readFull reads exactly len(buf) bytes.
func readFull(conn net.Conn, buf []byte) error {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return err
		}
	}
	return nil
}

// readOneFrame reads exactly one AGWPE frame (header + payload) from conn.
func readOneFrame(t *testing.T, conn net.Conn) testFrame {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	hdrBuf := make([]byte, agwpepkg.HeaderSize)
	if err := readFull(conn, hdrBuf); err != nil {
		t.Fatalf("readOneFrame header: %v", err)
	}
	hdr, err := agwpepkg.ParseHeader(hdrBuf)
	if err != nil {
		t.Fatalf("readOneFrame parse: %v", err)
	}
	tf := testFrame{Header: hdr}
	if hdr.DataLen > 0 {
		tf.Data = make([]byte, hdr.DataLen)
		if err := readFull(conn, tf.Data); err != nil {
			t.Fatalf("readOneFrame payload: %v", err)
		}
	}
	return tf
}

// onLoop posts fn to the engine and blocks until it completes.
func onLoop(t *testing.T, e *engine.Engine, fn func()) {
	t.Helper()
	done := make(chan struct{})
	e.Do(func() { fn(); close(done) })
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("engine stalled")
	}
}

// decodeAX25Frames decodes raw AX.25 bytes (as stored by fakePort) into frames.
// The fakePort.Send receives raw AX.25 bytes (bridge.SendToKISS passes raw AX.25;
// the real kiss.Port.Send wraps in KISS, but fakePort stores AX.25 directly).
func decodeAX25Frames(rawFrames [][]byte) []*ax25.Frame {
	var all []*ax25.Frame
	for _, raw := range rawFrames {
		if f, err := ax25.Parse(raw); err == nil {
			all = append(all, f)
		}
	}
	return all
}

// waitForSABM polls fp until a SABM is found, or times out.
func waitForSABM(t *testing.T, fp *fakePort) *ax25.Frame {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range decodeAX25Frames(fp.getSent()) {
			if f.Type == ax25.SABM {
				return f
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("SABM not received by fake port")
	return nil
}

// --- tests ---

// TestVersionR: 'R' → 8-byte payload 02 00 00 00 00 00 00 00.
func TestVersionR(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	writeFrame(t, conn, 0, 'R', 0, "", "", nil)
	resp := readOneFrame(t, conn)

	if resp.Kind != 'R' {
		t.Fatalf("kind = %c, want R", resp.Kind)
	}
	if len(resp.Data) != 8 {
		t.Fatalf("data len = %d, want 8", len(resp.Data))
	}
	want := []byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	for i, wb := range want {
		if resp.Data[i] != wb {
			t.Fatalf("data[%d] = 0x%02x, want 0x%02x", i, resp.Data[i], wb)
		}
	}
}

// TestPortInfoG: two configured ports → "2;Port 0;Port 1;".
func TestPortInfoG(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fps := []*fakePort{newFakePort(true), newFakePort(true)}
	b := makeBridgeWithFakePort(t, eng, fps, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
		{Name: "Port 1", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	writeFrame(t, conn, 0, 'G', 0, "", "", nil)
	resp := readOneFrame(t, conn)

	if resp.Kind != 'G' {
		t.Fatalf("kind = %c, want G", resp.Kind)
	}
	got := string(resp.Data)
	want := "2;Port 0;Port 1;"
	if got != want {
		t.Fatalf("payload = %q, want %q", got, want)
	}
}

// TestCapsG: tx_delay=40 → caps bytes 0,255,40,30,63,20,7,0 + uint32 0.
func TestCapsG(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	txDelay := 40
	fp := newFakePort(true)
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{
			Name:        "Port 0",
			Type:        "serial",
			Device:      "/dev/null",
			OTABaudrate: 1200,
			KISS:        kiss.Params{TXDelay: &txDelay},
		},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	writeFrame(t, conn, 0, 'g', 0, "", "", nil)
	resp := readOneFrame(t, conn)

	if resp.Kind != 'g' {
		t.Fatalf("kind = %c, want g", resp.Kind)
	}
	if len(resp.Data) != 12 {
		t.Fatalf("caps len = %d, want 12", len(resp.Data))
	}
	// baud=0, traffic=255, tx_delay=40, tx_tail=30, persist=63, slot_time=20, max_frame=7, active=0
	wantBytes := []byte{0, 255, 40, 30, 63, 20, 7, 0}
	for i, wb := range wantBytes {
		if resp.Data[i] != wb {
			t.Fatalf("caps[%d] = %d, want %d", i, resp.Data[i], wb)
		}
	}
	wantU32 := uint32(0)
	gotU32 := binary.LittleEndian.Uint32(resp.Data[8:12])
	if gotU32 != wantU32 {
		t.Fatalf("caps bytes_rcvd = %d, want 0", gotU32)
	}
}

// TestRegisterAndDuplicate: first 'X' → \x01; same call from second client → \x00.
func TestRegisterAndDuplicate(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn1 := dialServe(t, eng, b)
	defer ln.Close()
	defer conn1.Close()
	time.Sleep(20 * time.Millisecond)

	// First client registers N0CALL.
	writeFrame(t, conn1, 0, 'X', 0, "N0CALL", "", nil)
	resp1 := readOneFrame(t, conn1)

	if resp1.Kind != 'X' {
		t.Fatalf("resp1 kind = %c, want X", resp1.Kind)
	}
	if len(resp1.Data) == 0 || resp1.Data[0] != 0x01 {
		t.Fatalf("resp1 data[0] = 0x%02x, want 0x01 (success)", resp1.Data[0])
	}

	// Second client tries to register same call.
	conn2, err := net.DialTimeout("tcp", ln.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial conn2: %v", err)
	}
	defer conn2.Close()
	time.Sleep(20 * time.Millisecond)

	writeFrame(t, conn2, 0, 'X', 0, "N0CALL", "", nil)
	resp2 := readOneFrame(t, conn2)

	if resp2.Kind != 'X' {
		t.Fatalf("resp2 kind = %c, want X", resp2.Kind)
	}
	if len(resp2.Data) == 0 || resp2.Data[0] != 0x00 {
		t.Fatalf("resp2 data[0] = 0x%02x, want 0x00 (duplicate rejection)", resp2.Data[0])
	}
}

// TestUnprotoM: 'M' → fake port receives KISS-wrapped UI frame with correct
// dst/src/PID/info.
func TestUnprotoM(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	info := []byte("HELLO WORLD")
	writeFrame(t, conn, 0, 'M', 0xF0, "N0CALL", "KU0HN-10", info)

	// Give engine time to process.
	deadline := time.Now().Add(2 * time.Second)
	var parsed *ax25.Frame
	for time.Now().Before(deadline) {
		for _, f := range decodeAX25Frames(fp.getSent()) {
			if f.Type == ax25.UI {
				parsed = f
				break
			}
		}
		if parsed != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if parsed == nil {
		t.Fatal("no UI frame received by fake port")
	}

	if parsed.Dst.String() != "KU0HN-10" {
		t.Fatalf("dst = %q, want KU0HN-10", parsed.Dst.String())
	}
	if parsed.Src.String() != "N0CALL" {
		t.Fatalf("src = %q, want N0CALL", parsed.Src.String())
	}
	if parsed.PID != 0xF0 {
		t.Fatalf("PID = 0x%02x, want 0xF0", parsed.PID)
	}
	if string(parsed.Info) != string(info) {
		t.Fatalf("info = %q, want %q", string(parsed.Info), string(info))
	}
}

// TestConnectCFlow: 'C' → SABM on fake port; inject UA via bridge.OnKISSFrame
// → client reads 'C' frame with "*** CONNECTED With N0CALL-2\r".
func TestConnectCFlow(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	// Register the local call.
	writeFrame(t, conn, 0, 'X', 0, "KU0HN-10", "", nil)
	resp := readOneFrame(t, conn)
	if resp.Kind != 'X' || (len(resp.Data) > 0 && resp.Data[0] != 0x01) {
		t.Fatalf("X register failed: kind=%c data=%v", resp.Kind, resp.Data)
	}

	// Send connect request.
	writeFrame(t, conn, 0, 'C', 0, "KU0HN-10", "N0CALL-2", nil)

	// Wait for SABM at fake port.
	sabm := waitForSABM(t, fp)

	// Build UA response (swap src/dst, response frame).
	ua := &ax25.Frame{
		Dst:     sabm.Src,
		Src:     sabm.Dst,
		Type:    ax25.UA,
		PF:      sabm.PF,
		Command: false,
	}

	// Inject UA on engine loop.
	onLoop(t, eng, func() {
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: ua.Bytes()})
	})

	// Read the 'C' connected notification.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp2 := readOneFrame(t, conn)

	if resp2.Kind != 'C' {
		t.Fatalf("expected C notification, got %c (data=%q)", resp2.Kind, string(resp2.Data))
	}
	wantMsg := "*** CONNECTED With N0CALL-2\r"
	if string(resp2.Data) != wantMsg {
		t.Fatalf("connected msg = %q, want %q", string(resp2.Data), wantMsg)
	}
}

// TestConnectOfflinePortBusy: fake port Online()=false → immediate 'd' BUSY.
func TestConnectOfflinePortBusy(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(false) // offline!
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	writeFrame(t, conn, 0, 'C', 0, "KU0HN-10", "N0CALL-2", nil)
	resp := readOneFrame(t, conn)

	if resp.Kind != 'd' {
		t.Fatalf("expected 'd' BUSY, got %c", resp.Kind)
	}
	wantMsg := "*** BUSY From KU0HN-10\r"
	if string(resp.Data) != wantMsg {
		t.Fatalf("busy msg = %q, want %q", string(resp.Data), wantMsg)
	}
}

// TestYOutstanding: after 'C'+UA and two 'D' sends beyond window →
// 'Y' response counts unacked+queued.
func TestYOutstanding(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	// Use MaxWindow=1 so 2nd D frame goes into the queue.
	cfg := &config.Config{
		Server: config.Server{MaxClients: 8},
		AX25:   config.AX25{MaxWindow: 1, N2Retry: 10, T3Timeout: 0},
		Ports: []config.Port{
			{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
		},
	}
	b := bridge.New(eng, cfg)
	params := []l2pkg.PortParams{l2pkg.DeriveParams(1200, 1, 10, 0)}
	bridge.InjectPorts(b, eng, params, []bridge.PortSender{fp})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	// Register and connect.
	writeFrame(t, conn, 0, 'X', 0, "KU0HN-10", "", nil)
	readOneFrame(t, conn) // consume X response

	writeFrame(t, conn, 0, 'C', 0, "KU0HN-10", "N0CALL-2", nil)

	// Wait for SABM.
	sabm := waitForSABM(t, fp)

	// Inject UA.
	ua := &ax25.Frame{
		Dst:     sabm.Src,
		Src:     sabm.Dst,
		Type:    ax25.UA,
		PF:      sabm.PF,
		Command: false,
	}
	onLoop(t, eng, func() {
		b.OnKISSFrame(kiss.RXFrame{Port: 0, Data: ua.Bytes()})
	})

	// Read C notification.
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	cResp := readOneFrame(t, conn)
	if cResp.Kind != 'C' {
		t.Fatalf("expected C, got %c", cResp.Kind)
	}

	// Send two D frames (MaxWindow=1, so only first goes OTA; second queues).
	writeFrame(t, conn, 0, 'D', 0xF0, "KU0HN-10", "N0CALL-2", []byte("hello"))
	writeFrame(t, conn, 0, 'D', 0xF0, "KU0HN-10", "N0CALL-2", []byte("world"))
	time.Sleep(80 * time.Millisecond) // let engine process D frames

	// Query Y — should be 2 (1 unacked + 1 queued).
	writeFrame(t, conn, 0, 'Y', 0, "KU0HN-10", "N0CALL-2", nil)
	yResp := readOneFrame(t, conn)

	if yResp.Kind != 'Y' {
		t.Fatalf("expected Y, got %c", yResp.Kind)
	}
	if len(yResp.Data) != 4 {
		t.Fatalf("Y data len = %d, want 4", len(yResp.Data))
	}
	count := binary.LittleEndian.Uint32(yResp.Data)
	if count != 2 {
		t.Fatalf("Y outstanding = %d, want 2", count)
	}
}

// TestOversizedPayloadCloses: header claiming DataLen 70000 → connection closed.
func TestOversizedPayloadCloses(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	// Build a header with DataLen = 70000 (> MaxPayload=65536).
	hdr := make([]byte, agwpepkg.HeaderSize)
	hdr[4] = 'R' // kind
	binary.LittleEndian.PutUint32(hdr[28:32], 70000)

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	conn.Write(hdr)

	// Server should close the connection.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if n != 0 || err == nil {
		t.Fatalf("expected connection closed, got n=%d err=%v", n, err)
	}
}

// TestPartialHeaderReassembly: write a 'R' request split into 3 TCP writes → still answered.
func TestPartialHeaderReassembly(t *testing.T) {
	eng := engine.New()
	go eng.Run()
	defer eng.Stop()

	fp := newFakePort(true)
	b := makeBridgeWithFakePort(t, eng, []*fakePort{fp}, []config.Port{
		{Name: "Port 0", Type: "serial", Device: "/dev/null", OTABaudrate: 1200},
	})

	ln, conn := dialServe(t, eng, b)
	defer ln.Close()
	defer conn.Close()
	time.Sleep(20 * time.Millisecond)

	// Build R frame and split into 3 writes.
	frame := agwpepkg.Build(0, 'R', 0, "", "", nil)
	part1 := frame[:5]
	part2 := frame[5:20]
	part3 := frame[20:]

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	conn.Write(part1)
	time.Sleep(5 * time.Millisecond)
	conn.Write(part2)
	time.Sleep(5 * time.Millisecond)
	conn.Write(part3)

	resp := readOneFrame(t, conn)
	if resp.Kind != 'R' {
		t.Fatalf("expected R response, got %c", resp.Kind)
	}
	if len(resp.Data) != 8 {
		t.Fatalf("version payload len = %d, want 8", len(resp.Data))
	}
}
