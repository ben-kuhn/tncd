//go:build linux

package kiss

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"golang.org/x/sys/unix"
)

// TestEnsureProfileRetrySeam verifies the retry behaviour of ensureProfile
// without requiring a real D-Bus / BlueZ connection.
//
// Call table:
//
//	call 1 → register() fails  → ensureProfile returns error,
//	          profileRegistered stays false
//	call 2 → register() succeeds → ensureProfile returns nil,
//	          profileRegistered becomes true
//	call 3 → register() must NOT be called again (already registered)
func TestEnsureProfileRetrySeam(t *testing.T) {
	// Reset package-level state so this test is hermetic.
	profileMu.Lock()
	profileRegistered = false
	profileMu.Unlock()
	t.Cleanup(func() {
		profileMu.Lock()
		profileRegistered = false
		profileMu.Unlock()
	})

	var callCount int
	var mu sync.Mutex

	type testCase struct {
		name      string
		returnErr error
		wantErr   bool
		wantCalls int // cumulative after this call
		wantReg   bool
	}
	cases := []testCase{
		{
			name:      "first call fails",
			returnErr: errors.New("UUID already registered"),
			wantErr:   true,
			wantCalls: 1,
			wantReg:   false,
		},
		{
			name:      "second call retries and succeeds",
			returnErr: nil,
			wantErr:   false,
			wantCalls: 2,
			wantReg:   true,
		},
		{
			name:      "third call skips register (already registered)",
			returnErr: errors.New("should never be called"),
			wantErr:   false,
			wantCalls: 2, // count must not increment
			wantReg:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capturedErr := tc.returnErr // capture for closure
			err := ensureProfile(func() error {
				mu.Lock()
				callCount++
				mu.Unlock()
				return capturedErr
			})

			if tc.wantErr && err == nil {
				t.Errorf("wanted error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("wanted nil error, got %v", err)
			}

			mu.Lock()
			got := callCount
			mu.Unlock()
			if got != tc.wantCalls {
				t.Errorf("register call count = %d, want %d", got, tc.wantCalls)
			}

			profileMu.Lock()
			reg := profileRegistered
			profileMu.Unlock()
			if reg != tc.wantReg {
				t.Errorf("profileRegistered = %v, want %v", reg, tc.wantReg)
			}
		})
	}
}

// blockingCaller models a BlueZ D-Bus object that never replies — the state
// that wedged a live port: Device1.Disconnect was issued during a reconnect
// and BlueZ never answered, so Open() blocked forever. The port stopped
// retrying and went permanently silent with nothing logged.
type blockingCaller struct{ calls int }

func (b *blockingCaller) CallWithContext(ctx context.Context, method string,
	flags dbus.Flags, args ...interface{}) *dbus.Call {
	b.calls++
	<-ctx.Done() // never replies; only the caller's timeout can end this
	return &dbus.Call{Err: ctx.Err()}
}

// TestCallBlueZTimesOut: a BlueZ call that never replies must return an error
// within the timeout rather than blocking the caller indefinitely.
func TestCallBlueZTimesOut(t *testing.T) {
	c := &blockingCaller{}
	done := make(chan error, 1)
	go func() { done <- callBlueZ(c, 100*time.Millisecond, "org.bluez.Device1.Disconnect") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from an unanswered BlueZ call")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callBlueZ blocked indefinitely on an unanswered BlueZ call")
	}
	if c.calls != 1 {
		t.Fatalf("calls = %d, want 1", c.calls)
	}
}

// okCaller replies immediately, like a healthy BlueZ.
type okCaller struct{ err error }

func (o okCaller) CallWithContext(ctx context.Context, method string,
	flags dbus.Flags, args ...interface{}) *dbus.Call {
	return &dbus.Call{Err: o.err}
}

// TestCallBlueZPassesThrough: a prompt reply is returned unchanged (success
// and failure alike) so callers keep their existing error handling.
func TestCallBlueZPassesThrough(t *testing.T) {
	if err := callBlueZ(okCaller{}, time.Second, "m"); err != nil {
		t.Fatalf("healthy call returned %v, want nil", err)
	}
	want := errors.New("boom")
	if err := callBlueZ(okCaller{err: want}, time.Second, "m"); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

// TestControlChannelBacksOntoTransport is the regression test for the
// deleted second-RFCOMM-link design: on a real UV-PRO, a separate "BS AOC"
// control service accepts a connection and answers nothing, while the Gaia
// protocol round-trips fine over the SAME socket as KISS ("SPP Dev"). So
// ControlChannel must not dial anything -- it must hand back a view of bt's
// own already-open file, reads and writes going straight through to it.
//
// It must also NOT close bt's underlying file: the port, not a rig-control
// caller, owns the transport's lifetime, and Close()ing the rig's view must
// never take the KISS data path down. Proven here by writing through bt
// again after the control channel's Close().
func TestControlChannelBacksOntoTransport(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	peer := os.NewFile(uintptr(fds[1]), "peer")
	defer peer.Close()

	bt := &bluetoothTransport{file: os.NewFile(uintptr(fds[0]), "bt")}
	defer bt.file.Close()

	cc, err := bt.ControlChannel()
	if err != nil {
		t.Fatalf("ControlChannel: %v", err)
	}

	if _, err := cc.Write([]byte("hello")); err != nil {
		t.Fatalf("control channel write: %v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("peer read = %q, want %q", buf, "hello")
	}

	if err := cc.Close(); err != nil {
		t.Fatalf("control channel Close: %v", err)
	}
	// The underlying transport must still be usable: Close() on the
	// control-channel view must not have closed bt.file.
	if _, err := bt.Write([]byte("still alive")); err != nil {
		t.Fatalf("bt.Write after control channel Close: %v", err)
	}
}

// TestControlChannelRequiresOpenTransport: asking for rig control before the
// port has opened the transport must fail cleanly, not hand back a channel
// wrapping a nil file that panics on first use.
func TestControlChannelRequiresOpenTransport(t *testing.T) {
	bt := &bluetoothTransport{}
	if _, err := bt.ControlChannel(); err == nil {
		t.Fatal("ControlChannel on an unopened transport: err = nil, want error")
	}
}

// TestTXStallDetector covers the send-queue drain logic that distinguishes a
// briefly-busy socket from one the kernel can no longer hand off to the radio.
//
// Scope note, because the naming invites the wrong assumption: this tracks the
// HOST send queue only. The on-air UV-PRO stall that prompted this code shows
// depth 0 throughout — the radio takes the bytes and buffers them — so these
// cases model RFCOMM credit starvation, not that failure. See the txStallTimeout
// comment for which layer covers what.
func TestTXStallDetector(t *testing.T) {
	t0 := time.Unix(1000, 0)
	var d txStallDetector

	// An empty queue is the healthy steady state: nothing is pending.
	if got := d.observe(0, t0); got != 0 {
		t.Fatalf("empty queue: backed up for %v, want 0", got)
	}
	// First backed-up sample only starts the clock.
	if got := d.observe(512, t0); got != 0 {
		t.Fatalf("first backlog sample: %v, want 0", got)
	}
	// Still backed up 40s later: that is the elapsed stall.
	if got := d.observe(512, t0.Add(40*time.Second)); got != 40*time.Second {
		t.Fatalf("sustained backlog: %v, want 40s", got)
	}
	// Draining clears the tracking, so a later backlog starts a fresh clock.
	if got := d.observe(0, t0.Add(41*time.Second)); got != 0 {
		t.Fatalf("drained queue: %v, want 0", got)
	}
	if got := d.observe(256, t0.Add(42*time.Second)); got != 0 {
		t.Fatalf("backlog after drain should restart the clock, got %v", got)
	}
	if got := d.observe(256, t0.Add(52*time.Second)); got != 10*time.Second {
		t.Fatalf("restarted backlog: %v, want 10s", got)
	}
}
