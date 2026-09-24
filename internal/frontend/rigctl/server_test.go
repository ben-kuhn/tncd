package rigctl

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
)

type fakeRig struct {
	hz      uint32
	setErr  error
	getErr  error
	ptt     bool
	pttErr  error
	setFreq uint32
}

func (f *fakeRig) SetFreq(hz uint32) error  { f.setFreq = hz; return f.setErr }
func (f *fakeRig) GetFreq() (uint32, error) { return f.hz, f.getErr }
func (f *fakeRig) SetPTT(on bool) error     { f.ptt = on; return f.pttErr }
func (f *fakeRig) GetPTT() (bool, error)    { return f.ptt, f.pttErr }

func TestHandleLineFrequencyCommands(t *testing.T) {
	cases := []struct {
		name string
		line string
		rig  *fakeRig
		want string
	}{
		{"get_freq returns a bare integer", `\get_freq`, &fakeRig{hz: 145030000}, "145030000"},
		{"set_freq succeeds", `\set_freq 145030000`, &fakeRig{}, "RPRT 0"},
		{"chk_vfo reports no VFO mode", `\chk_vfo`, &fakeRig{}, "CHKVFO 0"},
		{"dump_caps is a non-error ping", "dump_caps", &fakeRig{}, "RPRT 0"},
		{"unknown command", "zzz", &fakeRig{}, "RPRT -1"},
		{"set_freq with no argument", `\set_freq`, &fakeRig{}, "RPRT -1"},
		{"set_freq with junk", `\set_freq abc`, &fakeRig{}, "RPRT -1"},
		{"radio timeout", `\get_freq`, &fakeRig{getErr: rig.ErrTimeout}, "RPRT -5"},
		{"radio port closed", `\get_freq`, &fakeRig{getErr: rig.ErrClosed}, "RPRT -6"},
		{"radio malformed reply", `\get_freq`, &fakeRig{getErr: errOpaque}, "RPRT -8"},
		{"short mnemonic get_freq", "f", &fakeRig{hz: 145030000}, "145030000"},
		{"short mnemonic set_freq", "F 145030000", &fakeRig{}, "RPRT 0"},
		{"get_ptt off", `\get_ptt`, &fakeRig{ptt: false}, "0"},
		{"get_ptt on", "t", &fakeRig{ptt: true}, "1"},
		{"set_ptt is a stub", `\set_ptt 1`, &fakeRig{}, "RPRT -4"},
		{"blank line", "   ", &fakeRig{}, "RPRT -1"},
	}
	cfg := config.RigCtl{PTTTimeout: 30}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := handleLine(tc.line, tc.rig, cfg, &pttState{})
			if got != tc.want {
				t.Errorf("handleLine(%q) = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

func TestSetFreqPassesThroughValue(t *testing.T) {
	f := &fakeRig{}
	handleLine(`\set_freq 145030000`, f, config.RigCtl{PTTTimeout: 30}, &pttState{})
	if f.setFreq != 145030000 {
		t.Errorf("SetFreq got %d, want 145030000", f.setFreq)
	}
}

// An offline or relinking port must answer immediately, never hang.
func TestUnavailableRigAnswersIO(t *testing.T) {
	got := handleLine(`\get_freq`, nil, config.RigCtl{PTTTimeout: 30}, &pttState{})
	if got != "RPRT -6" {
		t.Errorf("handleLine with nil rig = %q, want RPRT -6", got)
	}
}

// chk_vfo and dump_caps must answer even while the port is offline -- PAT
// uses dump_caps purely as a liveness ping and should not see it fail just
// because the radio itself is mid-relink.
func TestNoRigCommandsAnswerWithoutARig(t *testing.T) {
	cfg := config.RigCtl{PTTTimeout: 30}
	if got := handleLine(`\chk_vfo`, nil, cfg, &pttState{}); got != "CHKVFO 0" {
		t.Errorf("chk_vfo with nil rig = %q, want CHKVFO 0", got)
	}
	if got := handleLine("dump_caps", nil, cfg, &pttState{}); got != "RPRT 0" {
		t.Errorf("dump_caps with nil rig = %q, want RPRT 0", got)
	}
}

var errOpaque = errOpaqueType("boom")

type errOpaqueType string

func (e errOpaqueType) Error() string { return string(e) }

// A typed nil (*fakeRig)(nil) satisfies Rig with a non-nil interface value --
// the classic Go footgun. A plain "r == nil" check misses this, and the first
// method call panics on a nil receiver. handleLine must catch it and answer
// RPRT -6, the same as an untyped nil, not panic.
func TestTypedNilRigAnswersIOInsteadOfPanicking(t *testing.T) {
	var f *fakeRig // nil concrete pointer, non-nil Rig interface value
	got := handleLine(`\get_freq`, f, config.RigCtl{PTTTimeout: 30}, &pttState{})
	if got != "RPRT -6" {
		t.Errorf("handleLine with typed-nil rig = %q, want RPRT -6", got)
	}
}

// newTestServer builds a Server wired to a fake Rig, with idleTimeout
// shrunk from the 30-minute default so idle tests don't take 30 real
// minutes.
func newTestServer(idleTimeout time.Duration) *Server {
	s := New(config.RigCtl{PTTTimeout: 30}, func() (Rig, error) { return &fakeRig{}, nil })
	s.idleTimeout = idleTimeout
	return s
}

// A connection that sends nothing must be reaped, not parked forever --
// that's the whole point of the idle timeout (leaked goroutines/fds in a
// process that runs for the life of the service).
func TestIdleConnectionIsClosed(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	s := newTestServer(20 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		s.handleConn(server)
		close(done)
	}()

	select {
	case <-done:
		// handleConn returned on its own -- the deadline fired.
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not return after the idle timeout")
	}

	client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected a read error on the client side once the idle server closed its end")
	}
}

// The idle timeout must not punish a client that is actually using the
// connection -- PAT holds a rigctl session open between QSYs and issues
// commands far less often than the real 30-minute default, but any client
// polling faster than the (shrunk, for this test) idle timeout must survive.
// This matters more than the reap case: a timeout that kills working
// sessions is worse than the leak it's meant to fix.
func TestActiveConnectionSurvivesIdleTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	s := newTestServer(30 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		s.handleConn(server)
		close(done)
	}()

	reader := bufio.NewReader(client)
	for i := 0; i < 5; i++ {
		client.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := client.Write([]byte("dump_caps\n")); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		client.SetReadDeadline(time.Now().Add(time.Second))
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got := strings.TrimSpace(line); got != "RPRT 0" {
			t.Fatalf("reply %d = %q, want RPRT 0", i, got)
		}
		time.Sleep(15 * time.Millisecond) // less than idleTimeout, keeps the deadline refreshed
	}

	select {
	case <-done:
		t.Fatal("handleConn returned even though the client was actively issuing commands")
	default:
	}

	client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte("q\n")); err != nil {
		t.Fatalf("write q: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleConn did not return after q")
	}
}
