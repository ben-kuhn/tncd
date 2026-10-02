package bridge

import (
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGate records what the gate asked the radio to do, without a radio.
type fakeGate struct {
	mu        sync.Mutex
	calls     []string
	applyErr  error
	releaseOK func() error
}

func (f *fakeGate) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeGate) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func newTestGate(f *fakeGate) *rigGate {
	return newRigGate("port 0",
		func() error {
			f.record("apply")
			return f.applyErr
		},
		func() error {
			f.record("release")
			if f.releaseOK != nil {
				return f.releaseOK()
			}
			return nil
		})
}

// waitCalls waits for the gate's background reconcile to settle on want.
func waitCalls(t *testing.T, f *fakeGate, want []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if eq(f.got(), want) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("calls = %v, want %v", f.got(), want)
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRigGateClientAloneDoesNotTouchTheRadio: a rigctl client that connects and
// only polls -- a monitor, a status page -- has no business reconfiguring
// somebody's radio. A client is a reason not to RELEASE, not a reason to acquire.
func TestRigGateClientAloneDoesNotTouchTheRadio(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.addClient()
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); len(got) != 0 {
		t.Errorf("a connected client alone produced %v, want no radio writes", got)
	}
	g.removeClient()
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); len(got) != 0 {
		t.Errorf("connect+disconnect produced %v, want no radio writes", got)
	}
}

// TestRigGateSessionAcquiresAndReleases is the plain AGWPE case: a client that
// never speaks rigctl at all, just connects over AX.25.
func TestRigGateSessionAcquiresAndReleases(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})
	g.setSessions(0)
	waitCalls(t, f, []string{"apply", "release"})
}

// TestRigGateRefcountsSessions: settings are per RADIO but tncd allows several
// simultaneous connections per port, so overlapping sessions must apply once and
// release once -- and crucially must NOT release when the first of two ends.
func TestRigGateRefcountsSessions(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})
	g.setSessions(2)
	g.setSessions(1)
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); !eq(got, []string{"apply"}) {
		t.Fatalf("calls = %v after a second session came and went, want just [apply]", got)
	}
	g.setSessions(0)
	waitCalls(t, f, []string{"apply", "release"})
}

// TestRigGateQSYAcquiresSynchronously: SetFreq refuses while the radio is on a
// named memory, so the acquire has to have COMPLETED before the tune is issued.
// A background reconcile would be too late.
func TestRigGateQSYAcquiresSynchronously(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.addClient() // PAT connects rigctl first
	if err := g.acquireForQSY(); err != nil {
		t.Fatalf("acquireForQSY: %v", err)
	}
	// No waiting: it must already have happened when the call returned.
	if got := f.got(); !eq(got, []string{"apply"}) {
		t.Errorf("calls = %v immediately after acquireForQSY, want [apply] -- "+
			"the acquire must complete before the tune is issued", got)
	}
}

// TestRigGateHoldsWhileAClientRemains is PAT's actual flow, and the reason a
// connected client is a holder: PAT QSYs, runs the AX.25 session, then QSXes
// back BEFORE dropping rigctl. Releasing when the session ended would undo the
// QSY underneath it, retuning the radio while PAT is still using it.
func TestRigGateHoldsWhileAClientRemains(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.addClient()
	if err := g.acquireForQSY(); err != nil {
		t.Fatalf("acquireForQSY: %v", err)
	}
	g.setSessions(1)
	g.setSessions(0) // the AX.25 session ends; PAT is still connected
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); !eq(got, []string{"apply"}) {
		t.Fatalf("calls = %v after the session ended with a client still connected, "+
			"want just [apply] -- releasing here would retune the radio under PAT", got)
	}
	g.removeClient() // PAT drops rigctl
	waitCalls(t, f, []string{"apply", "release"})
}

// TestRigGateQSYAloneReleasesWhenEverythingLetsGo: the QSY latch must not pin the
// radio forever once there is nothing left holding it.
func TestRigGateQSYAloneReleasesWhenEverythingLetsGo(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.addClient()
	if err := g.acquireForQSY(); err != nil {
		t.Fatalf("acquireForQSY: %v", err)
	}
	g.removeClient()
	waitCalls(t, f, []string{"apply", "release"})

	// And the latch is cleared: a later client alone must not re-acquire.
	g.addClient()
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); !eq(got, []string{"apply", "release"}) {
		t.Errorf("calls = %v -- a stale QSY latch re-acquired on a bare client connect", got)
	}
}

// TestRigGateFailedApplyDoesNotClaimTheRadioIsHeld: if the settings never took,
// a later release would write values that were never displaced. It must also not
// spin, and a subsequent event should retry.
func TestRigGateFailedApplyDoesNotClaimTheRadioIsHeld(t *testing.T) {
	f := &fakeGate{applyErr: errors.New("radio did not reply")}
	g := newTestGate(f)

	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); len(got) > 2 {
		t.Fatalf("calls = %v -- a failed apply is spinning", got)
	}
	// Session ends: nothing was displaced, so nothing to put back.
	g.setSessions(0)
	time.Sleep(50 * time.Millisecond)
	for _, c := range f.got() {
		if c == "release" {
			t.Error("released settings that were never applied")
		}
	}
	// A later session retries.
	f.applyErr = nil
	g.setSessions(1)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		for _, c := range f.got() {
			if c == "apply" {
				n++
			}
		}
		if n >= 2 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Errorf("calls = %v -- a later session did not retry the apply", f.got())
}

// TestRigGateFailedReleaseDoesNotSpin: the radio may simply be gone, and since
// these writes reach NVRAM there is no power-cycle safety net to wait for.
// Retrying in a tight loop would bury the one log line the operator needs.
func TestRigGateFailedReleaseDoesNotSpin(t *testing.T) {
	f := &fakeGate{releaseOK: func() error { return errors.New("port offline") }}
	g := newTestGate(f)

	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})
	g.setSessions(0)
	waitCalls(t, f, []string{"apply", "release"})
	time.Sleep(100 * time.Millisecond)
	n := 0
	for _, c := range f.got() {
		if c == "release" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("release attempted %d times after failing, want 1", n)
	}
}

// TestRigGateShutdownReleases: shutdown is the last chance. These writes reach
// NVRAM, so a radio still held here stays reconfigured until someone fixes it by
// hand.
func TestRigGateShutdownReleases(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})
	g.shutdown()
	if got := f.got(); !eq(got, []string{"apply", "release"}) {
		t.Errorf("calls = %v after shutdown, want [apply release]", got)
	}
	// Idempotent: a second shutdown must not write again.
	g.shutdown()
	if got := f.got(); !eq(got, []string{"apply", "release"}) {
		t.Errorf("calls = %v after a second shutdown, want no extra writes", got)
	}
}

// TestRigGateShutdownWithNothingHeldIsSilent: the common case is a process that
// never ran a session, and it must not poke the radio on the way out.
func TestRigGateShutdownWithNothingHeldIsSilent(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)
	g.addClient()
	g.shutdown()
	if got := f.got(); len(got) != 0 {
		t.Errorf("shutdown produced %v with nothing held, want no radio writes", got)
	}
}

// TestRigGateRapidFlapSettlesHeldOrReleasedCorrectly: the counts change on the
// engine goroutine and on rigctl goroutines while the radio I/O is slow, so the
// final state must follow the final counts rather than whichever goroutine won.
func TestRigGateRapidFlapSettlesHeldOrReleasedCorrectly(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)
	// Make the I/O slow enough that events certainly arrive mid-flight.
	g.apply = func() error { time.Sleep(10 * time.Millisecond); f.record("apply"); return nil }
	g.release = func() error { time.Sleep(10 * time.Millisecond); f.record("release"); return nil }

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				g.setSessions(1)
			} else {
				g.setSessions(0)
			}
		}(i)
	}
	wg.Wait()
	g.setSessions(0) // final word: nothing holding it
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		settled := !g.busy && !g.actual
		g.mu.Unlock()
		if settled {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	g.mu.Lock()
	actual, busy := g.actual, g.busy
	g.mu.Unlock()
	t.Errorf("after a flap ending in zero sessions: actual=%v busy=%v, want released and idle (calls=%v)",
		actual, busy, f.got())
}

// TestRigGateDisabledIsInert: when the radio turns out not to speak the control
// protocol, rig control is disabled for the port -- and the gate must go with
// it. Without this a port with a non-Benshi radio still tried to acquire on
// every session and logged "FAILED to restore the radio's settings" about a
// radio it had never touched, which is both alarming and false.
func TestRigGateDisabledIsInert(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)
	g.disabled.Store(true)

	g.setSessions(1)
	g.addClient()
	if err := g.acquireForQSY(); err != nil {
		t.Errorf("acquireForQSY on a disabled gate: %v", err)
	}
	g.setSessions(0)
	g.removeClient()
	g.shutdown()
	time.Sleep(50 * time.Millisecond)
	if got := f.got(); len(got) != 0 {
		t.Errorf("a disabled gate produced %v, want no radio writes at all", got)
	}
}

// safeBuf collects log output across goroutines. A bare strings.Builder races:
// the gate logs from its reconcile goroutine while the test reads.
type safeBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// logCapture swaps the standard logger's output for the duration of a test.
func logCapture(t *testing.T) *safeBuf {
	t.Helper()
	b := &safeBuf{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

// TestRigGateFailedApplyDoesNotClaimTheRadioIsDisplaced: a failed APPLY must not
// print the restore-failure message. It used to, because the message was chosen
// from g.actual -- which step() has already set to false by then -- so an acquire
// that never wrote anything told the operator their radio might be left in packet
// configuration and to run `session-set` to fix it. Seen in the field on a port
// whose control channel had closed.
func TestRigGateFailedApplyDoesNotClaimTheRadioIsDisplaced(t *testing.T) {
	out := logCapture(t)
	f := &fakeGate{applyErr: errors.New("control channel closed")}
	g := newTestGate(f)

	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})
	time.Sleep(50 * time.Millisecond)

	got := out.String()
	if strings.Contains(got, "FAILED to restore") {
		t.Errorf("a failed apply logged the restore-failure message:\n%s", got)
	}
	if !strings.Contains(got, "could not apply session settings") {
		t.Errorf("a failed apply did not log the apply-failure message:\n%s", got)
	}
}

// TestRigGateFailedReleaseSaysSo is the other half: a failed RELEASE must still
// tell the operator the radio is displaced and name the command that fixes it,
// because these writes reach NVRAM and nothing else will put them back if tncd
// exits before the port returns.
func TestRigGateFailedReleaseSaysSo(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)
	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})

	out := logCapture(t)
	f.releaseOK = func() error { return errors.New("port offline") }
	g.setSessions(0)
	waitCalls(t, f, []string{"apply", "release"})
	time.Sleep(50 * time.Millisecond)

	got := out.String()
	if !strings.Contains(got, "could not restore") {
		t.Errorf("a failed release did not warn that the radio is displaced:\n%s", got)
	}
	if !strings.Contains(got, "still in packet configuration") {
		t.Errorf("a failed release did not say the radio is still displaced:\n%s", got)
	}
	if !strings.Contains(got, "session-set") {
		t.Errorf("a failed release did not name the recovery command:\n%s", got)
	}
}

// TestRigGateRetriesRestoreWhenThePortComesBack is the bug this replaced, and it
// was not a rare one. Reproduced on hardware: a Bluetooth port bounced
// mid-session, the release failed because the port was offline, the port
// reconnected five seconds later -- and nothing retried, so the operator's radio
// was left on the VFO record permanently. The gate had cleared its state on the
// failure and believed the radio was restored.
func TestRigGateRetriesRestoreWhenThePortComesBack(t *testing.T) {
	f := &fakeGate{}
	g := newTestGate(f)

	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})

	// The port goes down: the session dies with it and the release fails.
	down := true
	f.releaseOK = func() error {
		if down {
			return errors.New("port 0 is offline")
		}
		return nil
	}
	g.setSessions(0)
	waitCalls(t, f, []string{"apply", "release"})

	// Still held, because the radio was never actually put back.
	g.mu.Lock()
	held := g.actual
	g.mu.Unlock()
	if !held {
		t.Fatal("gate reported the radio restored after the release failed -- " +
			"it will never retry, and the radio stays displaced")
	}

	// The port comes back.
	down = false
	g.reconcile()
	waitCalls(t, f, []string{"apply", "release", "release"})

	g.mu.Lock()
	held = g.actual
	g.mu.Unlock()
	if held {
		t.Error("gate still reports the radio held after a successful retry")
	}
}

// TestRigGateDoesNotRepeatAnIdenticalRestoreAlarm: the retry now fires on every
// later event, so a radio that is simply gone must not bury the one line the
// operator needs under identical copies.
func TestRigGateDoesNotRepeatAnIdenticalRestoreAlarm(t *testing.T) {
	f := &fakeGate{releaseOK: func() error { return errors.New("port offline") }}
	g := newTestGate(f)
	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})

	out := logCapture(t)
	g.setSessions(0)
	waitCalls(t, f, []string{"apply", "release"})
	for i := 0; i < 4; i++ {
		g.reconcile()
		time.Sleep(40 * time.Millisecond)
	}
	if n := strings.Count(out.String(), "could not restore"); n != 1 {
		t.Errorf("restore alarm logged %d times for one unchanging reason, want 1:\n%s",
			n, out.String())
	}
}

// TestRigGateRetriesRestoreAfterADelay: poking the gate the instant a port
// reports "reconnected" raced the port actually becoming usable -- the retry
// failed with the same error and the dedupe silenced it, so the radio stayed
// displaced. Observed on hardware. Restores are now re-attempted on a spaced,
// bounded schedule instead of relying on one poke.
func TestRigGateRetriesRestoreAfterADelay(t *testing.T) {
	// Shorten the schedule so the test does not wait seconds.
	prev := releaseRetryDelays
	releaseRetryDelays = []time.Duration{20 * time.Millisecond, 20 * time.Millisecond}
	t.Cleanup(func() { releaseRetryDelays = prev })

	f := &fakeGate{}
	g := newTestGate(f)
	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})

	// The port is down and stays down for the first retry, then comes back
	// WITHOUT any external event -- only the gate's own schedule can recover it.
	var mu sync.Mutex
	down := true
	f.releaseOK = func() error {
		mu.Lock()
		defer mu.Unlock()
		if down {
			return errors.New("port 0 is offline")
		}
		return nil
	}
	g.setSessions(0)
	waitCalls(t, f, []string{"apply", "release"})

	mu.Lock()
	down = false
	mu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		held := g.actual
		g.mu.Unlock()
		if !held {
			return // restored by the scheduled retry
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("radio still held after the port recovered: calls=%v -- "+
		"no scheduled retry put it back", f.got())
}

// TestRigGateGivesUpRestoringEventually: a radio that is simply gone must not be
// retried forever, and the last word must tell the operator how to fix it.
func TestRigGateGivesUpRestoringEventually(t *testing.T) {
	prev := releaseRetryDelays
	releaseRetryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { releaseRetryDelays = prev })

	f := &fakeGate{releaseOK: func() error { return errors.New("radio gone") }}
	g := newTestGate(f)
	g.setSessions(1)
	waitCalls(t, f, []string{"apply"})

	out := logCapture(t)
	g.setSessions(0)
	time.Sleep(400 * time.Millisecond)

	n := 0
	for _, c := range f.got() {
		if c == "release" {
			n++
		}
	}
	if n > len(releaseRetryDelays)+2 {
		t.Errorf("attempted %d releases for an unreachable radio -- not bounded", n)
	}
	if !strings.Contains(out.String(), "giving up") {
		t.Errorf("never reported giving up:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "session-set") {
		t.Errorf("gave up without naming the recovery command:\n%s", out.String())
	}
}
