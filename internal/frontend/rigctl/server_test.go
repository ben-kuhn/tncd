package rigctl

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ben-kuhn/tncd/v2/internal/config"
	"github.com/ben-kuhn/tncd/v2/internal/rig"
)

// fakeRig's ptt field is touched by a background goroutine in the
// force-release tests (the PTTTimeout timer, or handleConn's disconnect
// defer, both fire on their own goroutine) while the test goroutine polls
// it, so it needs its own lock -- unlike the other fields here, which are
// only ever written and read synchronously within a single test goroutine.
//
// SetPTT models TOGGLE semantics, not level semantics: it flips f.ptt on
// every call and ignores the on argument entirely, exactly like the real
// DO_PROG_FUNC(MAIN_PTT) command, which carries no press/release parameter
// of its own (see (*rig.Rig).SetPTT's doc comment). An earlier version of
// this fake stored the argument it was given ("f.ptt = on"), which let a
// real bug -- handleSetPTT resending the same wire command while already
// keyed, flipping the physical bit back off while st.keyed kept reading
// true -- pass the entire suite: the fake reported whatever the code
// claimed to have set, never what a toggle-based radio would actually do.
// Assertions here must read the physical bit via keyed()/GetPTT, never
// infer state from a request's argument or return value.
type fakeRig struct {
	hz      uint32
	setErr  error
	getErr  error
	pttErr  error
	setFreq uint32

	mu       sync.Mutex
	ptt      bool
	pttCalls int // counts actual SetPTT invocations, to prove a redundant key/release didn't re-send
}

func (f *fakeRig) SetFreq(hz uint32) error  { f.setFreq = hz; return f.setErr }
func (f *fakeRig) GetFreq() (uint32, error) { return f.hz, f.getErr }

func (f *fakeRig) SetPTT(on bool) error {
	f.mu.Lock()
	f.ptt = !f.ptt // toggle, ignoring on -- see the type doc comment
	f.pttCalls++
	f.mu.Unlock()
	return f.pttErr
}

func (f *fakeRig) GetPTT() (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ptt, f.pttErr
}

// keyed is the thread-safe read of f.ptt, for tests that poll it from a
// different goroutine than the one keying/releasing it.
func (f *fakeRig) keyed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ptt
}

func (f *fakeRig) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pttCalls
}

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
		{"set_ptt refused when allow_ptt defaults false", `\set_ptt 1`, &fakeRig{}, "RPRT -4"},
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

// --- PTT ---

func TestSetPTTRefusedWhenDisabled(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: false, PTTTimeout: 30}
	got := handleLine(`\set_ptt 1`, &fakeRig{}, cfg, &pttState{})
	if got != "RPRT -4" {
		t.Errorf("handleLine = %q, want RPRT -4 when allow_ptt is false", got)
	}
}

func TestSetPTTAllowedWhenEnabled(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	f := &fakeRig{}
	st := &pttState{}
	if got := handleLine(`\set_ptt 1`, f, cfg, st); got != "RPRT 0" {
		t.Errorf("handleLine = %q, want RPRT 0", got)
	}
	if !f.keyed() {
		t.Error("rig was not keyed")
	}
	// Release so the test does not leave a timer running.
	if got := handleLine(`\set_ptt 0`, f, cfg, st); got != "RPRT 0" {
		t.Errorf("release = %q, want RPRT 0", got)
	}
	if f.keyed() {
		t.Error("rig still keyed after release")
	}
}

// hamlib sends 0, 1 or 3 (PTT_ON_DATA); anything else, or a missing
// argument, is a bad argument.
func TestSetPTTRejectsBadArgument(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	for _, arg := range []string{"", "9", "x"} {
		line := `\set_ptt`
		if arg != "" {
			line += " " + arg
		}
		if got := handleLine(line, &fakeRig{}, cfg, &pttState{}); got != "RPRT -1" {
			t.Errorf("set_ptt %q = %q, want RPRT -1", arg, got)
		}
	}
}

func TestSetPTTAcceptsValidArguments(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	for _, arg := range []string{"0", "1", "3"} {
		f := &fakeRig{}
		st := &pttState{}
		if got := handleLine(`\set_ptt `+arg, f, cfg, st); got != "RPRT 0" {
			t.Errorf("set_ptt %s = %q, want RPRT 0", arg, got)
		}
		handleLine(`\set_ptt 0`, f, cfg, st) // release, don't leave a timer running
	}
}

// The stuck-transmitter guard: a key nobody releases must be force-released
// once PTTTimeout elapses, with no further action from any client.
func TestPTTForceReleasesOnTimeout(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 1} // 1s: the shortest positive value config allows
	f := &fakeRig{}
	st := &pttState{}
	if got := handleLine(`\set_ptt 1`, f, cfg, st); got != "RPRT 0" {
		t.Fatalf("set_ptt 1 = %q, want RPRT 0", got)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !f.keyed() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("transmitter still keyed after the ptt_timeout elapsed")
}

// A second unkey -- whether the client sends set_ptt 0 twice, or the
// PTTTimeout timer fires after the client already released -- must not
// error, and per releaseLocked's doc comment must not re-send the wire
// command either (see that comment for why: it's the same bytes as "key",
// with no press/release parameter to distinguish them).
func TestReleasePTTIsIdempotent(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	f := &fakeRig{}
	st := &pttState{}
	handleLine(`\set_ptt 1`, f, cfg, st)
	handleLine(`\set_ptt 0`, f, cfg, st)
	if got := handleLine(`\set_ptt 0`, f, cfg, st); got != "RPRT 0" {
		t.Errorf("second release = %q, want RPRT 0", got)
	}
	if f.keyed() {
		t.Error("rig keyed after a redundant release")
	}
	if got := f.calls(); got != 2 {
		t.Errorf("SetPTT called %d times, want 2 (one key, one release) -- the redundant release must not re-send", got)
	}
}

// Releasing an already-unkeyed rig (nothing was ever keyed) must also be a
// harmless no-op, not just a repeat of an actual release.
func TestReleasePTTWithNothingKeyedIsHarmless(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	f := &fakeRig{}
	if got := handleLine(`\set_ptt 0`, f, cfg, &pttState{}); got != "RPRT 0" {
		t.Errorf("release with nothing keyed = %q, want RPRT 0", got)
	}
	if got := f.calls(); got != 0 {
		t.Errorf("SetPTT called %d times, want 0 -- releasing nothing must not touch the radio", got)
	}
}

// forceReleasePTT must tolerate a nil Rig (port offline/mid-relink at the
// moment of release) rather than panicking -- the one situation where the
// radio genuinely cannot be reached.
//
// A NON-final release keeps believing the key is down and leaves a retry
// timer armed. That is the point: a nil rig means the port is mid-relink,
// so the very next attempt may well find a live one, and clearing the state
// here would disarm every remaining force-release path for a transmitter
// that is probably still on the air.
func TestForceReleasePTTWithNilRigKeepsRetrying(t *testing.T) {
	st := &pttState{keyed: true, timer: time.AfterFunc(time.Hour, func() {})}
	forceReleasePTT(nil, st, false)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.keyed {
		t.Error("gave up on a keyed transmitter when the rig was merely mid-relink")
	}
	if st.timer == nil {
		t.Error("no retry timer armed after a failed release -- the backstop is gone")
	}
}

// A FINAL release (server shutting down) must clear the state instead,
// because nothing will be left running to fire a retry timer.
func TestForceReleasePTTFinalWithNilRigClearsState(t *testing.T) {
	st := &pttState{keyed: true, timer: time.AfterFunc(time.Hour, func() {})}
	forceReleasePTT(nil, st, true)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.keyed {
		t.Error("pttState still reports keyed after a final force-release")
	}
	if st.timer != nil {
		t.Error("timer not cleared after a final force-release -- it would only leak")
	}
}

// A failed key must be treated as keyed. (*rig.Rig).request only reaches its
// reply phase after the write has landed, so a reply timeout means the radio
// very probably IS transmitting; recording "not keyed" would arm no timer and
// make every force-release path short-circuit, leaving it keyed until a
// power cycle.
func TestFailedKeyStillArmsTheSafetyTimer(t *testing.T) {
	st := &pttState{}
	f := &fakeRig{pttErr: errors.New("reply timed out")}
	if err := keyLocked(f, st, time.Hour); err == nil {
		t.Fatal("keyLocked hid the failure from its caller")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.keyed {
		t.Error("a failed key was recorded as not keyed -- every force-release path is now disarmed")
	}
	if st.timer == nil {
		t.Error("no safety timer armed after a failed key")
	}
}

// A failed un-key must not cancel the safety timer. The old code stopped and
// nil'd it BEFORE calling the radio, so a failure there removed the only
// thing that would have caught a key still being down.
func TestFailedReleaseKeepsTheSafetyTimer(t *testing.T) {
	st := &pttState{keyed: true}
	f := &fakeRig{pttErr: errors.New("reply timed out")}
	st.mu.Lock()
	err := releaseLocked(f, st)
	st.mu.Unlock()
	if err == nil {
		t.Fatal("releaseLocked hid the failure from its caller")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.keyed {
		t.Error("a failed release cleared st.keyed -- tncd now believes a possibly-keyed radio is idle")
	}
	if st.timer == nil {
		t.Error("a failed release left no retry timer armed")
	}
}

// A client that disconnects while keyed (dropped TCP, killed app, network
// partition) must not leave the transmitter on just because nobody is left
// to send set_ptt 0.
func TestPTTForceReleasesOnClientDisconnect(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	f := &fakeRig{}
	s := New(config.RigCtl{AllowPTT: true, PTTTimeout: 30}, func() (Rig, error) { return f, nil })

	done := make(chan struct{})
	go func() {
		s.handleConn(server)
		close(done)
	}()

	reader := bufio.NewReader(client)
	client.SetWriteDeadline(time.Now().Add(time.Second))
	if _, err := client.Write([]byte("\\set_ptt 1\n")); err != nil {
		t.Fatalf("write set_ptt 1: %v", err)
	}
	client.SetReadDeadline(time.Now().Add(time.Second))
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read set_ptt reply: %v", err)
	}
	if got := strings.TrimSpace(line); got != "RPRT 0" {
		t.Fatalf("set_ptt 1 reply = %q, want RPRT 0", got)
	}
	if !f.keyed() {
		t.Fatal("rig was not keyed")
	}

	client.Close() // simulate a dropped connection: no set_ptt 0, no "q"

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not return after the client disconnected")
	}

	if f.keyed() {
		t.Error("transmitter still keyed after the client disconnected")
	}
}

// tncd shutting down must not leave a transmitter keyed behind it.
func TestPTTForceReleasesOnServerClose(t *testing.T) {
	f := &fakeRig{}
	s := New(config.RigCtl{AllowPTT: true, PTTTimeout: 30}, func() (Rig, error) { return f, nil })

	if got := handleLine(`\set_ptt 1`, f, s.cfg, s.ptt); got != "RPRT 0" {
		t.Fatalf("set_ptt 1 = %q, want RPRT 0", got)
	}
	if !f.keyed() {
		t.Fatal("rig was not keyed")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.keyed() {
		t.Error("transmitter still keyed after Close")
	}
}

// Close on a server that was never keyed (the overwhelmingly common case)
// must not touch the rig at all -- confirms the release path's guard, not
// just its effect.
func TestServerCloseWithNothingKeyedDoesNotTouchRig(t *testing.T) {
	f := &fakeRig{}
	s := New(config.RigCtl{AllowPTT: true, PTTTimeout: 30}, func() (Rig, error) { return f, nil })
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.keyed() {
		t.Error("rig reports keyed after Close with nothing ever keyed")
	}
	if got := f.calls(); got != 0 {
		t.Errorf("SetPTT called %d times, want 0 -- Close with nothing keyed must not touch the radio", got)
	}
}

// --- Fix-round-1 regressions: double-key must never desync st.keyed from
// the physical toggle bit, on any of the four ways a key can end. ---
//
// Root cause (reviewer-reproduced): handleSetPTT's "on" branch used to call
// r.SetPTT(true) unconditionally, with no "already keyed" guard --
// asymmetric with releaseLocked, which has always had the mirror-image
// guard on the release side. Against a REAL toggle-semantics radio (see
// fakeRig's doc comment) that meant: key (physical ON, st.keyed=true), key
// again (physical toggles back OFF, st.keyed STAYS true because nothing
// checked it), release (st.keyed true, so releaseLocked sends a THIRD
// toggle -- physical back ON). Worse: every force-release backstop
// (timeout, disconnect, Close) also trusts st.keyed and would have skipped
// the radio entirely once it read false, so the safety net built to catch
// a stuck key was itself disarmed by the same bug. keyLocked's
// already-keyed guard (added in this fix round) closes this by never
// re-sending the toggle for a second "on", so st.keyed and the physical bit
// can no longer diverge in the first place.

// (a) The exact reported sequence: key, key again, release. Ends unkeyed.
func TestDoubleKeyThenReleaseEndsUnkeyed(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 30}
	f := &fakeRig{}
	st := &pttState{}

	if got := handleLine(`\set_ptt 1`, f, cfg, st); got != "RPRT 0" {
		t.Fatalf("first key = %q, want RPRT 0", got)
	}
	if !f.keyed() {
		t.Fatal("radio not keyed after the first set_ptt 1")
	}
	if got := handleLine(`\set_ptt 1`, f, cfg, st); got != "RPRT 0" {
		t.Fatalf("second key = %q, want RPRT 0", got)
	}
	if !f.keyed() {
		t.Fatal("radio physically unkeyed after a redundant set_ptt 1 -- the already-keyed guard did not suppress the resend")
	}
	if got := handleLine(`\set_ptt 0`, f, cfg, st); got != "RPRT 0" {
		t.Fatalf("release = %q, want RPRT 0", got)
	}
	if f.keyed() {
		t.Error("radio still physically keyed after release following a double-key")
	}
}

// (b) Double-key, then let the PTTTimeout timer fire instead of an explicit
// release. This is a backstop path: it must end unkeyed even though nobody
// sent set_ptt 0.
func TestDoubleKeyThenTimeoutEndsUnkeyed(t *testing.T) {
	cfg := config.RigCtl{AllowPTT: true, PTTTimeout: 1}
	f := &fakeRig{}
	st := &pttState{}

	handleLine(`\set_ptt 1`, f, cfg, st)
	handleLine(`\set_ptt 1`, f, cfg, st)
	if !f.keyed() {
		t.Fatal("radio not keyed after double-key")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !f.keyed() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("transmitter still keyed after ptt_timeout elapsed following a double-key")
}

// (c) Double-key, then the client disconnects instead of releasing. Another
// backstop path: must end unkeyed.
func TestDoubleKeyThenDisconnectEndsUnkeyed(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	f := &fakeRig{}
	s := New(config.RigCtl{AllowPTT: true, PTTTimeout: 30}, func() (Rig, error) { return f, nil })

	done := make(chan struct{})
	go func() {
		s.handleConn(server)
		close(done)
	}()

	reader := bufio.NewReader(client)
	for i := 0; i < 2; i++ {
		client.SetWriteDeadline(time.Now().Add(time.Second))
		if _, err := client.Write([]byte("\\set_ptt 1\n")); err != nil {
			t.Fatalf("write set_ptt 1 (%d): %v", i, err)
		}
		client.SetReadDeadline(time.Now().Add(time.Second))
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read set_ptt reply (%d): %v", i, err)
		}
		if got := strings.TrimSpace(line); got != "RPRT 0" {
			t.Fatalf("set_ptt 1 reply (%d) = %q, want RPRT 0", i, got)
		}
	}
	if !f.keyed() {
		t.Fatal("radio not keyed after double-key")
	}

	client.Close() // simulate a dropped connection: no set_ptt 0, no "q"

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleConn did not return after the client disconnected")
	}

	if f.keyed() {
		t.Error("transmitter still keyed after a disconnect following a double-key")
	}
}

// (d) Double-key, then the server is Closed (tncd shutting down) instead of
// the client releasing. The last backstop path: must end unkeyed.
func TestDoubleKeyThenServerCloseEndsUnkeyed(t *testing.T) {
	f := &fakeRig{}
	s := New(config.RigCtl{AllowPTT: true, PTTTimeout: 30}, func() (Rig, error) { return f, nil })

	handleLine(`\set_ptt 1`, f, s.cfg, s.ptt)
	handleLine(`\set_ptt 1`, f, s.cfg, s.ptt)
	if !f.keyed() {
		t.Fatal("radio not keyed after double-key")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.keyed() {
		t.Error("transmitter still keyed after Close following a double-key")
	}
}
