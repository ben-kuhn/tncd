//go:build windows

package kiss

import (
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
