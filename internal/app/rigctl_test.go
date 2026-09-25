package app

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/benshi"
	"github.com/ben-kuhn/tncd/v2/internal/config"
)

// minimalConfig builds a one-port config whose port transport is "tcp"
// pointed at a privileged port nothing is listening on: the bridge dials it
// asynchronously (so New never blocks), the dial fails immediately
// (ECONNREFUSED, not a timeout), and the port sits offline forever unless a
// test overrides Host/TCPPort to point at a real listener. That "offline by
// default" behaviour is deliberately reused by TestRigCtlOfflinePortAnswersRPRT6.
func minimalConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Server: config.Server{
			ListenHost: "127.0.0.1",
			ListenPort: 0,
			Callsign:   "TEST",
			MaxClients: 8,
		},
		AX25: config.AX25{MaxWindow: 3, N2Retry: 10, T3Timeout: 180},
		Ports: []config.Port{{
			Name:        "Port 0",
			Type:        "tcp",
			Host:        "127.0.0.1",
			TCPPort:     1, // nothing listening
			OTABaudrate: 1200,
			AX25Version: 22,
			Reconnect:   false,
		}},
		RigCtl: []config.RigCtl{{
			Enabled:    false,
			ListenHost: "127.0.0.1",
			ListenPort: 0,
			PTTTimeout: 30,
		}},
	}
}

// A disabled [rigctl.0] must bind nothing.
func TestRigCtlDisabledStartsNoListener(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.RigCtl[0].Enabled = false

	rt, err := New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	go func() { rt.Wait(); close(done) }()
	defer func() {
		rt.Shutdown()
		<-done
	}()

	if n := rt.rigCtlListenerCount(); n != 0 {
		t.Errorf("started %d rigctl listeners, want 0", n)
	}
}

// An enabled [rigctl.0] binds exactly one listener for its port.
func TestRigCtlEnabledStartsOneListenerPerPort(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.RigCtl[0].Enabled = true
	cfg.RigCtl[0].ListenPort = 0 // ephemeral

	rt, err := New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	done := make(chan struct{})
	go func() { rt.Wait(); close(done) }()
	defer func() {
		rt.Shutdown()
		<-done
	}()

	if n := rt.rigCtlListenerCount(); n != 1 {
		t.Errorf("started %d rigctl listeners, want 1", n)
	}
	if rt.rigSrvs[0].Addr() == "" {
		t.Error("rigctl listener has no bound address")
	}
}

// Shutdown must close the rigctl listener and must not hang, mirroring
// TestRuntimeServesThenShutsDown's proof for the AGWPE listener.
func TestRigCtlShutdownClosesListeners(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.RigCtl[0].Enabled = true
	cfg.RigCtl[0].ListenPort = 0

	rt, err := New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr := rt.rigSrvs[0].Addr()

	done := make(chan struct{})
	go func() { rt.Wait(); close(done) }()

	rt.Shutdown()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Shutdown")
	}

	if c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("rigctl listener still accepting after Shutdown")
	}
}

// An offline port (see minimalConfig: nothing listens on the bridge's
// configured TCPPort, so it never comes online) must make the rigctl server
// answer RPRT -6 immediately -- never block waiting on a radio that isn't
// there. This is the behavior handleLine documents for a nil Rig, exercised
// here end to end through the real provider wired up in New.
func TestRigCtlOfflinePortAnswersRPRT6(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.RigCtl[0].Enabled = true
	cfg.RigCtl[0].ListenPort = 0

	rt, err := New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// The rigctl provider (rigForPort) does a round trip through the engine
	// loop for every request -- see its doc comment. Without the loop
	// running, that round trip's <-done would block forever, wedging the
	// connection this test is about to make.
	done := make(chan struct{})
	go func() { rt.Wait(); close(done) }()
	defer func() {
		rt.Shutdown()
		<-done
	}()

	conn, err := net.DialTimeout("tcp", rt.rigSrvs[0].Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial rigctl: %v", err)
	}
	defer conn.Close()

	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := conn.Write([]byte("f\n")); err != nil {
		t.Fatalf("write get_freq: %v", err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if got := strings.TrimSpace(reply); got != "RPRT -6" {
		t.Errorf("reply = %q, want RPRT -6", got)
	}
}

// waitPortOnline polls bridge.PortOnline (via the engine loop, as its doc
// comment requires) until port comes online or timeout elapses.
func waitPortOnline(t *testing.T, rt *Runtime, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		online := make(chan bool, 1)
		rt.eng.Do(func() { online <- rt.bridge.PortOnline(port) })
		if <-online {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("port %d never came online within %s", port, timeout)
}

// doProgFuncReply encodes a Benshi DO_PROG_FUNC reply -- the wire shape
// (*rig.Rig).SetPTT's request() call is waiting for after either a key or an
// unkey (DO_PROG_FUNC carries no press/release parameter, so both send and
// expect the identical bytes; see (*rig.Rig).SetPTT's doc comment). The body
// content past the status byte doesn't matter to SetPTT, which only checks
// for an error.
func doProgFuncReply() []byte {
	m := benshi.Message{Group: benshi.GroupBasic, IsReply: true, Command: benshi.CmdDoProgFunc, Body: []byte{0x00}}
	return benshi.Frame{Flags: benshi.FlagNone, Data: m.Bytes()}.Bytes()
}

// TestRigCtlShutdownReleasesKeyedPTTBeforePortsTeardown proves the ordering
// constraint the brief calls out explicitly: "PTT is released on server
// close -- if the port is already gone, a keyed transmitter cannot be
// released." It keys PTT through a real rigctl client against a live port
// backed by a fake radio, keeps that client connected (so the only release
// path exercised is Server.Close's explicit force-release, not a
// client-disconnect one), calls Shutdown, and asserts the fake radio
// actually received a second DO_PROG_FUNC write -- the un-key -- proving the
// control channel was still alive when the release was attempted.
//
// This is deliberately NOT a test of "is the rigctl listener closed by time
// X": that signal doesn't distinguish correct ordering from backwards
// ordering here, because Server.Close() closes its listener unconditionally
// regardless of whether the PTT release it attempts first actually reaches
// the radio (see releaseLocked: a nil rig from an already-offline port is a
// logged failure, not a panic or an abort). The write actually reaching a
// still-open transport is the one signal that only a correct order produces.
func TestRigCtlShutdownReleasesKeyedPTTBeforePortsTeardown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (fake radio): %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{})
	var writes atomic.Int32
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		defer conn.Close()
		buf := make([]byte, 256)
		for {
			if _, err := conn.Read(buf); err != nil {
				return
			}
			writes.Add(1)
			if _, err := conn.Write(doProgFuncReply()); err != nil {
				return
			}
		}
	}()

	host, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	tcpPort, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi: %v", err)
	}

	cfg := minimalConfig(t)
	cfg.Ports[0].Host = host
	cfg.Ports[0].TCPPort = tcpPort
	cfg.RigCtl[0].Enabled = true
	cfg.RigCtl[0].ListenPort = 0
	cfg.RigCtl[0].AllowPTT = true
	cfg.RigCtl[0].PTTTimeout = 30 // long enough that the timer isn't what releases it

	rt, err := New(cfg, 0, 0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	waitDone := make(chan struct{})
	go func() { rt.Wait(); close(waitDone) }()

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("fake radio never saw the bridge connect")
	}
	waitPortOnline(t, rt, 0, 3*time.Second)

	rigConn, err := net.DialTimeout("tcp", rt.rigSrvs[0].Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial rigctl: %v", err)
	}
	defer rigConn.Close()
	if err := rigConn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	if _, err := rigConn.Write([]byte("T 1\n")); err != nil {
		t.Fatalf("write set_ptt: %v", err)
	}
	reply, err := bufio.NewReader(rigConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read set_ptt reply: %v", err)
	}
	if got := strings.TrimSpace(reply); got != "RPRT 0" {
		t.Fatalf("set_ptt reply = %q, want RPRT 0 (key must succeed for this test to mean anything)", got)
	}
	if n := writes.Load(); n != 1 {
		t.Fatalf("fake radio saw %d writes after keying, want 1", n)
	}

	// rigConn stays open on purpose: closing it would trigger handleConn's
	// own deferred force-release, exercising a different code path than the
	// one this test targets (Server.Close's explicit release).
	rt.Shutdown()

	select {
	case <-waitDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Wait did not return after Shutdown")
	}

	if n := writes.Load(); n != 2 {
		t.Errorf("fake radio saw %d writes after Shutdown, want 2 (key + release) -- "+
			"the PTT release did not reach the radio, meaning the port's control channel was "+
			"already gone when Shutdown tried to release it: rigctl servers must close, and "+
			"force-release any keyed PTT, before ports are torn down", n)
	}
}

// TestRigCtlStartFailureDoesNotDeadlockNew is a regression test for a startup
// hang: when a LATER rigctl listener failed to bind after an earlier one had
// already succeeded, New's cleanup path never returned.
//
// The cycle was Runtime.New -> closeRigServers -> (*rigctl.Server).Close ->
// forceReleasePTT(s.rig(), ...) -> Runtime.rigForPort -> eng.Do + block on the
// reply. Nothing drains the engine queue until eng.Run, which only happens in
// Runtime.Wait -- i.e. after New has returned. tncd hung at startup with no
// output and no error and had to be killed. It fired regardless of allow_ptt
// and regardless of whether anything had ever been keyed, because the rig was
// resolved before the "is anything keyed?" check rather than after it.
func TestRigCtlStartFailureDoesNotDeadlockNew(t *testing.T) {
	// Occupy a port so the second listener's bind is guaranteed to fail.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer occupied.Close()
	_, portStr, err := net.SplitHostPort(occupied.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	taken, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("Atoi: %v", err)
	}

	cfg := minimalConfig(t)
	cfg.Ports = append(cfg.Ports, cfg.Ports[0])
	cfg.RigCtl = []config.RigCtl{
		{Enabled: true, ListenHost: "127.0.0.1", ListenPort: 0, PTTTimeout: 30},     // binds
		{Enabled: true, ListenHost: "127.0.0.1", ListenPort: taken, PTTTimeout: 30}, // fails
	}

	type result struct {
		rt  *Runtime
		err error
	}
	done := make(chan result, 1)
	go func() {
		rt, err := New(cfg, 0, 0)
		done <- result{rt, err}
	}()

	select {
	case got := <-done:
		if got.err == nil {
			t.Fatal("New succeeded despite a rigctl listener that could not bind")
		}
		if got.rt != nil {
			t.Error("New returned a Runtime alongside an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: New did not return within 5s on the rigctl start-failure cleanup path")
	}
}
