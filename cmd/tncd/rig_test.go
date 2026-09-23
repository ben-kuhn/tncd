package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
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

func TestRunRigNoControlChannel(t *testing.T) {
	// A real listener so Open() succeeds and the failure under test is
	// specifically "no control channel", not "connection refused".
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

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

	// A TCP transport never implements kiss.ControlCapable, so this must fail
	// with ErrNoControlChannel once Open() succeeds.
	err = runRig(cfgPath, 0, []string{"get-freq"})
	if err == nil {
		t.Fatal("runRig on a TCP port: err = nil, want error (no control channel)")
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
