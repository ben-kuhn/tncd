package main

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/ben-kuhn/tncd/v2/kiss"
)

func TestParseRigArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantCmd string
		wantHz  uint32
		wantErr bool
	}{
		{"get-freq", []string{"get-freq"}, "get-freq", 0, false},
		{"set-freq", []string{"set-freq", "145030000"}, "set-freq", 145030000, false},
		{"set-freq in MHz is rejected", []string{"set-freq", "145.03"}, "", 0, true},
		{"teardown", []string{"teardown"}, "teardown", 0, false},
		{"probe", []string{"probe"}, "probe", 0, false},
		{"set-freq needs an argument", []string{"set-freq"}, "", 0, true},
		{"unknown verb", []string{"wat"}, "", 0, true},
		{"no verb", nil, "", 0, true},
	}
	for _, tc := range cases {
		cmd, hz, err := parseRigArgs(tc.args)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: err = nil, want error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: err = %v", tc.name, err)
			continue
		}
		if cmd != tc.wantCmd || hz != tc.wantHz {
			t.Errorf("%s: got (%q, %d), want (%q, %d)", tc.name, cmd, hz, tc.wantCmd, tc.wantHz)
		}
	}
}

func TestRunRigConfigNotFound(t *testing.T) {
	err := runRig("/nonexistent/tncd.ini", 0, []string{"get-freq"})
	if err == nil {
		t.Fatal("runRig with a missing config: err = nil, want error")
	}
}

func TestRunRigPortNotConfigured(t *testing.T) {
	cfgPath := writeTestConfig(t, `
[server]
listen_host = 127.0.0.1
listen_port = 8000

[client.0]
type = tcp
host = 127.0.0.1
port = 8001
`)
	// Port 5 is out of range for a config with only [client.0].
	err := runRig(cfgPath, 5, []string{"get-freq"})
	if err == nil {
		t.Fatal("runRig with an unconfigured port: err = nil, want error")
	}
}

// TestRunRigOverNonControlCapableTransportFails is the regression test for
// the switch from handing rig.New the transport directly to routing through
// kiss.ControlChannelFor: on a Benshi radio the Gaia command protocol lives
// on the SAME RFCOMM link as KISS (confirmed on a real UV-PRO -- see the
// comment in runRig), so the only real caller of ControlChannelFor is the
// Bluetooth transport, which now hands back a view of itself. A TCP
// transport implements no such thing, so runRig against a `type = tcp` port
// must fail fast with ErrNoControlChannel rather than silently talking Gaia
// over a socket that was never a rig-control channel to begin with (the
// prior behavior, before this switch).
func TestRunRigOverNonControlCapableTransportFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())

	cfgPath := writeTestConfig(t, `
[server]
listen_host = 127.0.0.1
listen_port = 8000

[client.0]
type = tcp
host = 127.0.0.1
port = `+portStr+`
`)

	err = runRig(cfgPath, 0, []string{"probe"})
	if err == nil {
		t.Fatal("runRig probe over a TCP transport: err = nil, want ErrNoControlChannel")
	}
	if !errors.Is(err, kiss.ErrNoControlChannel) {
		t.Errorf("runRig probe over a TCP transport: err = %v, want it to wrap kiss.ErrNoControlChannel", err)
	}
}

// writeTestConfig writes an INI config to a temp file and returns its path.
func writeTestConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tncd.ini")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}
