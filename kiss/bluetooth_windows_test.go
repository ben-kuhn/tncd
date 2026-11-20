//go:build windows

package kiss

import (
	"sync"
	"testing"

	"golang.org/x/sys/windows"
)

func TestParseBTAddr(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"00:11:22:33:44:55", 0x001122334455, true},
		{"AA:BB:CC:DD:EE:FF", 0xAABBCCDDEEFF, true},
		{"aa-bb-cc-dd-ee-ff", 0xAABBCCDDEEFF, true}, // dashes + lowercase
		{"001122334455", 0x001122334455, true},      // no separators
		{"00:11:22:33:44", 0, false},                // too short
		{"zz:11:22:33:44:55", 0, false},             // non-hex
	}
	for _, c := range cases {
		got, err := parseBTAddr(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("parseBTAddr(%q) = %#x, %v; want %#x, nil", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("parseBTAddr(%q) = %#x, nil; want error", c.in, got)
		}
	}
}

// TestControlChannelRequiresOpenTransport: asking for rig control before the
// port has opened the transport must fail cleanly rather than hand back a
// channel wrapping an unopened socket. Mirrors the Linux test; a real-socket
// equivalent of TestControlChannelBacksOntoTransport (kiss/bluetooth_linux_test.go)
// cannot run here since it needs a live Winsock connection this environment
// cannot fake, but the "not open" guard is pure logic and needs none.
func TestControlChannelRequiresOpenTransport(t *testing.T) {
	bt := &bluetoothTransport{fd: windows.InvalidHandle}
	if _, err := bt.ControlChannel(); err == nil {
		t.Fatal("ControlChannel on an unopened transport: err = nil, want error")
	}
}

// wsaStartupForTest initialises Winsock for tests that call the transport's
// I/O methods directly on an injected handle. A clean Close after never ran
// Open, so started is false and the deferred WSACleanup here balances this
// Startup exactly.
func wsaStartupForTest(t *testing.T) {
	t.Helper()
	var wsad windows.WSAData
	if err := windows.WSAStartup(0x202, &wsad); err != nil {
		t.Skipf("WSAStartup: %v", err)
	}
	t.Cleanup(func() { _ = windows.WSACleanup() })
}

// dummyHandle is a value that is not a socket and never was: every Winsock
// call on it fails fast with WSAENOTSOCK, so I/O loops can spin without
// touching anything. The property under test in
// TestConcurrentCloseGuardsFD is the memory-model guard on bt.fd, which the
// race detector checks -- no real socket could make it more meaningful.
const dummyHandle = windows.Handle(0x4e454957) // "WINE" -- no socket has this value legitimately open here

// TestConcurrentCloseGuardsFD: the Windows half of followups #2. Read and
// Write must take their handle via handle() (under mu), never bare bt.fd, so
// a Close concurrent with I/O cannot let one goroutine use a handle value
// another just invalidated -- the same race the Linux transport had before
// its fix, in the same shape.
//
// Before the fix this test fails under -race: the I/O goroutines read bt.fd
// unsynchronised while the churn goroutine writes it under mu. With the fix,
// every access is behind the mutex and the detector stays silent.
//
// The churn also re-"opens" the transport between closes (under mu, as Open
// would) so the test exercises the full lifecycle: I/O racing with Close,
// I/O racing with reopen, and I/O landing on either side of both.
func TestConcurrentCloseGuardsFD(t *testing.T) {
	wsaStartupForTest(t)

	bt := &bluetoothTransport{fd: dummyHandle, open: true}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	ioLoop := func() {
		defer wg.Done()
		buf := make([]byte, 32)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = bt.Read(buf)
			_, _ = bt.Write([]byte("\xc0\x00frame\xc0"))
		}
	}
	wg.Add(2)
	go ioLoop()
	go ioLoop()

	for i := 0; i < 200; i++ {
		_ = bt.Close()
		// Simulate Open handing the transport a fresh handle, under the same
		// lock Open uses.
		bt.mu.Lock()
		bt.fd = dummyHandle
		bt.open = true
		bt.mu.Unlock()
	}
	close(stop)
	wg.Wait()
}

// TestReadWriteAfterCloseFailsCleanly: after Close, Read and Write must fail
// via the "not open" guard rather than hand WSARecv/WSASend a handle the
// transport no longer owns. The exact message matters here: a missing guard
// would also error, but as a raw WSAENOTSOCK from the syscall on the stale
// handle -- indistinguishable in a log from a radio that closed on us.
func TestReadWriteAfterCloseFailsCleanly(t *testing.T) {
	wsaStartupForTest(t)

	bt := &bluetoothTransport{fd: dummyHandle, open: true}
	if err := bt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	buf := make([]byte, 8)
	if _, err := bt.Read(buf); err == nil || err.Error() != "bluetooth: not open" {
		t.Errorf("Read after Close: err = %v, want %q", err, "bluetooth: not open")
	}
	if _, err := bt.Write([]byte("frame")); err == nil || err.Error() != "bluetooth: not open" {
		t.Errorf("Write after Close: err = %v, want %q", err, "bluetooth: not open")
	}
}
