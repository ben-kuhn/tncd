package netutil

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestParseAllowlist(t *testing.T) {
	tests := []struct {
		in      string
		enabled bool
		wantErr bool
	}{
		{"", false, false},
		{"  ", false, false},
		{"192.168.1.0/24", true, false},
		{"192.168.1.0/24, 10.0.0.0/8", true, false},
		{"192.168.1.0/24,10.0.0.0/8", true, false}, // no space after comma
		{"192.168.1.7", true, false},               // bare IP → /32
		{"::1", true, false},                       // bare v6 IP
		{"2001:db8::/32", true, false},
		{"999.1.2.3/24", false, true},   // invalid
		{"192.168.1.0/33", false, true}, // bad prefix len
		{"not-an-ip", false, true},
	}
	for _, tc := range tests {
		a, err := ParseAllowlist(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseAllowlist(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if err == nil && a.Enabled() != tc.enabled {
			t.Errorf("ParseAllowlist(%q).Enabled() = %v, want %v", tc.in, a.Enabled(), tc.enabled)
		}
	}
}

func TestAllowlistAllows(t *testing.T) {
	a, err := ParseAllowlist("192.168.1.0/24, 10.1.2.3, ::1")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		addr string
		want bool
	}{
		{"192.168.1.5:1234", true},
		{"192.168.1.254:9999", true},
		{"192.168.2.1:1234", false},
		{"10.1.2.3:1234", true},  // bare IP /32
		{"10.1.2.4:1234", false}, // next host over
		{"[::1]:1234", true},
		{"[2001:db8::1]:1234", false},
		{"172.16.0.1:8000", false},
	}
	for _, tc := range cases {
		if got := a.Allows(fakeAddr(tc.addr)); got != tc.want {
			t.Errorf("Allows(%s) = %v, want %v", tc.addr, got, tc.want)
		}
	}

	// Empty allowlist allows everything.
	var empty Allowlist
	if !empty.Allows(fakeAddr("203.0.113.9:1234")) {
		t.Error("empty allowlist should allow all")
	}
}

type fakeAddr string

func (f fakeAddr) Network() string { return "tcp" }
func (f fakeAddr) String() string  { return string(f) }

// TestWrapListenerFilters verifies rejected connections are closed before
// they reach the acceptor, and allowed ones pass through.
func TestWrapListenerFilters(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Allowlist that does NOT include loopback: everything is rejected.
	deny, _ := ParseAllowlist("192.0.2.0/24")
	wrapped := WrapListener(ln, deny, "test")

	// The acceptor: one accepted conn means the filter failed open.
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := wrapped.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	select {
	case <-accepted:
		t.Fatal("rejected connection was accepted")
	case <-time.After(200 * time.Millisecond):
		// Good: still blocked. The rejected conn should be closed server-side.
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		if err == nil {
			t.Fatal("rejected connection not closed")
		}
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatal("rejected connection timed out instead of being closed")
		}
		if err != io.EOF {
			t.Logf("rejected connection read error (acceptable): %v", err)
		}
	}

	// Empty allowlist returns the listener unchanged (no filtering).
	if WrapListener(ln, Allowlist{}, "test") != ln {
		t.Error("empty allowlist should return the listener unchanged")
	}
}
